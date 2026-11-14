// @author: kevinkelly
// @license: Apache License 2.0
// @file: audit.go
// @time: 2026/10/28

// Package audit 实现 A3 §5 的审计日志：JSONL 只追加文件 + SHA-256 哈希链。
//
// 每条记录的 Hash = SHA256(Prev + "\n" + 规范化字段 JSON)，Prev 为上一条的 Hash（首条为空串）。
// 任何对中间记录的修改、删除都会使链断裂，Verify 可以发现。
//
// 已知边界（需要由外送 Sink 与文件权限共同弥补）：
//   - 攻击者若重算其后全部记录的哈希，离线无法发现，因此建议通过 Sink 实时外送；
//   - 本进程运行期间删除尾部记录可被发现（对比内存中的最大序号），重启后删除尾部无法发现；
//   - 同一文件只支持单进程写入。
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Result 的合法取值。
const (
	ResultAllowed = "allowed"
	ResultDenied  = "denied"
	ResultSuccess = "success"
	ResultFailed  = "failed"
)

const (
	maxQueryLimit     = 1000
	defaultQueryLimit = 100
	maxSummaryBytes   = 4096
	maxFieldBytes     = 1024
)

// Entry 是一条审计记录。
type Entry struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	User    string    `json:"user"`
	Role    string    `json:"role"`
	IP      string    `json:"ip"`
	Env     string    `json:"env"`
	Cluster string    `json:"cluster"`
	Action  string    `json:"action"`
	Target  string    `json:"target"`
	Risk    string    `json:"risk"`
	Summary string    `json:"summary"`
	Result  string    `json:"result"`
	Ref     string    `json:"ref"`
	Prev    string    `json:"prev"`
	Hash    string    `json:"hash"`
}

// canonical 是参与哈希的规范化字段（顺序固定，不含 Prev/Hash）。
type canonical struct {
	Seq     uint64 `json:"seq"`
	Time    string `json:"time"`
	User    string `json:"user"`
	Role    string `json:"role"`
	IP      string `json:"ip"`
	Env     string `json:"env"`
	Cluster string `json:"cluster"`
	Action  string `json:"action"`
	Target  string `json:"target"`
	Risk    string `json:"risk"`
	Summary string `json:"summary"`
	Result  string `json:"result"`
	Ref     string `json:"ref"`
}

// ComputeHash 按链规则计算一条记录的哈希（忽略 e.Hash，使用 e.Prev）。
func ComputeHash(e Entry) string {
	c := canonical{e.Seq, e.Time.UTC().Format(time.RFC3339Nano), e.User, e.Role, e.IP, e.Env,
		e.Cluster, e.Action, e.Target, e.Risk, e.Summary, e.Result, e.Ref}
	b, _ := json.Marshal(c)
	h := sha256.New()
	h.Write([]byte(e.Prev))
	h.Write([]byte("\n"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// ChainError 表示哈希链在 Seq 处断裂。
type ChainError struct {
	Seq    uint64
	Reason string
}

func (e *ChainError) Error() string {
	return fmt.Sprintf("审计链校验失败：第 %d 条被篡改或缺失（%s）", e.Seq, e.Reason)
}

// ErrClosed 表示日志已关闭。
var ErrClosed = errors.New("审计日志已关闭")

// Filter 是查询条件，零值字段表示不限制。Since/Until 为闭区间；Contains 不区分大小写，匹配所有文本字段。
type Filter struct {
	User     string
	Action   string
	Result   string
	Env      string
	Since    time.Time
	Until    time.Time
	Contains string
	// Clusters 非空时，只匹配 Cluster 字段等于其中任一值的记录（多集群：同一集群可能以 id 或 tiup 名出现）。
	Clusters []string
	// Allow 非空时，对其余条件都满足的记录再做一次判定（返回 false 的丢弃）。多集群里用来按调用者可见的环境裁剪。
	Allow func(e *Entry) bool
}

func (f Filter) match(e *Entry, lc string) bool {
	if f.User != "" && e.User != f.User {
		return false
	}
	if len(f.Clusters) > 0 {
		ok := false
		for _, c := range f.Clusters {
			if e.Cluster == c {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.Allow != nil && !f.Allow(e) {
		return false
	}
	if f.Action != "" && e.Action != f.Action {
		return false
	}
	if f.Result != "" && e.Result != f.Result {
		return false
	}
	if f.Env != "" && e.Env != f.Env {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.Time.After(f.Until) {
		return false
	}
	if lc != "" {
		hay := strings.ToLower(strings.Join([]string{e.User, e.Role, e.IP, e.Env, e.Cluster, e.Action,
			e.Target, e.Risk, e.Summary, e.Result, e.Ref}, "\x00"))
		if !strings.Contains(hay, lc) {
			return false
		}
	}
	return true
}

// Log 是只追加的审计日志。并发安全。
type Log struct {
	// Sink 在每条记录成功落盘后被调用（持有写锁，保证与链顺序一致），用于外送到 syslog / webhook。
	// 实现应当快速返回（例如投递到 channel）；panic 会被吞掉，不影响主流程。
	// 并发使用期间请用 SetSink 修改。
	Sink func(Entry)

	mu     sync.Mutex
	path   string
	f      *os.File
	seq    uint64
	prev   string
	size   int64
	closed bool
	broken error
}

// Open 打开（必要时创建）审计文件，权限 0600，并从尾部恢复 Seq 与 Prev。
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	l := &Log{path: path, f: f}
	if err := l.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return l, nil
}

// recover 读取尾部恢复状态。若末尾存在未写完的半行（崩溃所致）则丢弃。
func (l *Log) recover() error {
	st, err := l.f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size > 0 {
		last, newSize, err := readTail(l.f, size)
		if err != nil {
			return err
		}
		size = newSize
		if len(last) > 0 {
			var e Entry
			if err := json.Unmarshal(last, &e); err != nil {
				return fmt.Errorf("审计文件尾部记录无法解析：%w", err)
			}
			if ComputeHash(e) != e.Hash {
				return &ChainError{Seq: e.Seq, Reason: "尾部记录哈希不一致"}
			}
			l.seq, l.prev = e.Seq, e.Hash
		}
	}
	if _, err := l.f.Seek(size, io.SeekStart); err != nil {
		return err
	}
	l.size = size
	return nil
}

// readTail 返回最后一条完整记录行，以及（可能被修复后的）文件大小。
func readTail(f *os.File, size int64) (last []byte, newSize int64, err error) {
	var buf []byte
	var start int64
	for n := int64(64 << 10); ; n *= 2 {
		if n > size {
			n = size
		}
		start = size - n
		buf = make([]byte, n)
		if _, err = f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, 0, err
		}
		if n == size || bytes.Count(buf, []byte{'\n'}) >= 3 {
			break
		}
	}
	newSize = size
	if buf[len(buf)-1] != '\n' { // 末尾半行
		idx := bytes.LastIndexByte(buf, '\n') + 1
		frag := buf[idx:]
		var e Entry
		if json.Unmarshal(frag, &e) == nil && ComputeHash(e) == e.Hash {
			// 只是缺少换行：补上
			if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
				return nil, 0, err
			}
			return frag, size + 1, nil
		}
		if err := f.Truncate(start + int64(idx)); err != nil {
			return nil, 0, fmt.Errorf("修复审计文件尾部失败：%w", err)
		}
		newSize = start + int64(idx)
		buf = buf[:idx]
	}
	buf = bytes.TrimRight(buf, "\r\n")
	if len(buf) == 0 {
		return nil, newSize, nil
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	return buf, newSize, nil
}

// SetSink 并发安全地设置外送回调。
func (l *Log) SetSink(fn func(Entry)) {
	l.mu.Lock()
	l.Sink = fn
	l.mu.Unlock()
}

// Close 关闭文件。
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.f.Close()
}

// Path 返回审计文件路径。
func (l *Log) Path() string { return l.path }

// LastSeq 返回当前最大序号。
func (l *Log) LastSeq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Append 追加一条记录并 fsync。Seq/Prev/Hash 由本函数填写；Time 为零则取当前时间。
// Summary/Target 会做防御性脱敏。Action 与 Result（取值见常量）必填。
func (l *Log) Append(e Entry) (Entry, error) {
	if e.Action == "" {
		return Entry{}, errors.New("审计记录缺少 Action")
	}
	switch e.Result {
	case ResultAllowed, ResultDenied, ResultSuccess, ResultFailed:
	default:
		return Entry{}, fmt.Errorf("审计记录的 Result 非法：%q", e.Result)
	}
	e.Summary = truncate(Redact(e.Summary), maxSummaryBytes)
	e.Target = truncate(Redact(e.Target), maxFieldBytes)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Entry{}, ErrClosed
	}
	if l.broken != nil {
		return Entry{}, fmt.Errorf("审计日志不可写：%w", l.broken)
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Time = e.Time.UTC()
	e.Seq = l.seq + 1
	e.Prev = l.prev
	e.Hash = ComputeHash(e)
	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	line = append(line, '\n')
	_, err = l.f.Write(line)
	if err == nil {
		err = l.f.Sync()
	}
	if err != nil {
		// 回滚到写入前，避免留下半行破坏后续记录
		if terr := l.f.Truncate(l.size); terr != nil {
			l.broken = terr
		} else if _, serr := l.f.Seek(l.size, io.SeekStart); serr != nil {
			l.broken = serr
		}
		return Entry{}, fmt.Errorf("写入审计日志失败：%w", err)
	}
	l.size += int64(len(line))
	l.seq, l.prev = e.Seq, e.Hash
	if l.Sink != nil {
		func() {
			defer func() { _ = recover() }()
			l.Sink(e)
		}()
	}
	return e, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) { // 避免截断半个 UTF-8 字符
		cut--
	}
	return s[:cut] + "…"
}

// ---- 脱敏 ----

var (
	reKV     = regexp.MustCompile(`(?i)(["']?(?:password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|authorization)["']?\s*[=:]\s*)("[^"]*"|'[^']*'|(?:(?:bearer|basic)\s+)?[^\s,;&"'}]+)`)
	reBearer = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	reURL    = regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`)
)

// Redact 把疑似口令 / 令牌（password=、token=、secret=、Bearer xxx、URL 里的口令等）之后的值替换为 ***。
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = reKV.ReplaceAllStringFunc(s, func(m string) string {
		sm := reKV.FindStringSubmatch(m)
		v := sm[2]
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			return sm[1] + string(v[0]) + "***" + string(v[0])
		}
		return sm[1] + "***"
	})
	s = reBearer.ReplaceAllString(s, "$1 ***")
	s = reURL.ReplaceAllString(s, "${1}***@")
	return s
}

// ---- 读取 ----

// snapshot 取得当前写入位置与序号，读取只读到该位置，不阻塞追加。
func (l *Log) snapshot() (size int64, seq uint64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, 0, ErrClosed
	}
	return l.size, l.seq, nil
}

// iterate 逐行（跳过空行）回调。
func (l *Log) iterate(size int64, fn func(line []byte) error) error {
	f, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, size), 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			if ferr := fn(t); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Verify 从头校验哈希链。成功返回 (记录条数, nil)；失败返回 (第一处断裂的 Seq, *ChainError)，
// 错误信息形如“第 N 条被篡改或缺失”。
func (l *Log) Verify() (n int, err error) {
	size, last, err := l.snapshot()
	if err != nil {
		return 0, err
	}
	var exp uint64 = 1
	prev := ""
	err = l.iterate(size, func(line []byte) error {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return &ChainError{exp, "记录无法解析"}
		}
		switch {
		case e.Seq != exp:
			return &ChainError{exp, "序号不连续，记录缺失或被替换"}
		case e.Prev != prev:
			return &ChainError{e.Seq, "与前一条记录的哈希不衔接"}
		case ComputeHash(e) != e.Hash:
			return &ChainError{e.Seq, "内容与哈希不一致"}
		}
		prev = e.Hash
		exp++
		return nil
	})
	if err != nil {
		var ce *ChainError
		if errors.As(err, &ce) {
			return int(ce.Seq), err
		}
		return 0, err
	}
	if exp-1 != last {
		return int(exp), &ChainError{exp, "尾部记录缺失"}
	}
	return int(last), nil
}

func readEntries(l *Log, size int64, f Filter, fn func(Entry)) error {
	lc := strings.ToLower(f.Contains)
	return l.iterate(size, func(line []byte) error {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("审计记录无法解析：%w", err)
		}
		if f.match(&e, lc) {
			fn(e)
		}
		return nil
	})
}

// Query 返回符合条件的记录，按 Seq（即时间）倒序；limit<=0 取 100，上限 1000。
func (l *Log) Query(f Filter, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	size, _, err := l.snapshot()
	if err != nil {
		return nil, err
	}
	var all []Entry
	err = readEntries(l, size, f, func(e Entry) {
		all = append(all, e)
		if len(all) >= 2*limit { // 只保留最近的 limit 条，限制内存
			all = append(all[:0], all[len(all)-limit:]...)
		}
	})
	if err != nil {
		return nil, err
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if all == nil {
		all = []Entry{}
	}
	return all, nil
}

// Export 把全部记录按序号升序导出，format 为 "csv" 或 "jsonl"。
func (l *Log) Export(w io.Writer, format string) error {
	return l.ExportFiltered(w, Filter{}, format)
}

// ExportFiltered 导出符合条件的记录（升序）。csv 中以 = + - @ 开头的文本单元格会加前缀 ' 以防公式注入；
// 需要无损数据请使用 jsonl。
func (l *Log) ExportFiltered(w io.Writer, f Filter, format string) error {
	format = strings.ToLower(format)
	if format != "csv" && format != "jsonl" {
		return fmt.Errorf("不支持的导出格式 %q（可选 csv / jsonl）", format)
	}
	size, _, err := l.snapshot()
	if err != nil {
		return err
	}
	if format == "jsonl" {
		bw := bufio.NewWriter(w)
		enc := json.NewEncoder(bw)
		var werr error
		err = readEntries(l, size, f, func(e Entry) {
			if werr == nil {
				werr = enc.Encode(e)
			}
		})
		if err == nil {
			err = werr
		}
		if err == nil {
			err = bw.Flush()
		}
		return err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"seq", "time", "user", "role", "ip", "env", "cluster", "action", "target",
		"risk", "summary", "result", "ref", "prev", "hash"})
	err = readEntries(l, size, f, func(e Entry) {
		_ = cw.Write([]string{strconv.FormatUint(e.Seq, 10), e.Time.UTC().Format(time.RFC3339Nano),
			csvSafe(e.User), csvSafe(e.Role), csvSafe(e.IP), csvSafe(e.Env), csvSafe(e.Cluster),
			csvSafe(e.Action), csvSafe(e.Target), csvSafe(e.Risk), csvSafe(e.Summary), csvSafe(e.Result),
			csvSafe(e.Ref), e.Prev, e.Hash})
	})
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
