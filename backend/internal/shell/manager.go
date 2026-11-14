// @author: kevinkelly
// @license: Apache License 2.0
// @file: manager.go
// @time: 2026/10/28

package shell

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 任务状态。
const (
	StatusRunning = "running"
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusUnknown = "unknown"
)

// 执行路径。
const (
	RouteInline = "inline" // 平台进程内（经 sudo 入口以 tidb 身份、仍在服务沙箱里）：只读命令
	RouteUnit   = "unit"   // systemd-run 临时单元（由 tidb 的 sudo 免密起，在沙箱之外）：变更命令
)

const (
	requestValid = time.Hour        // 申请多久内要有人批准
	approvalLive = time.Hour        // 批准后多久内要执行
	staleAfter   = 6 * time.Hour    // 没有退出码也不再认为在运行
	runTimeout   = 60 * time.Second // 起 / 停临时单元、探测的总超时
	probeTTL     = 5 * time.Minute  // sudo 探测结果的缓存时间
	maxChunk     = 256 << 10
	maxOutput    = 1 << 20 // 单个任务的输出上限
	maxRunning   = 3       // 同时运行的任务总数
)

var (
	reID   = regexp.MustCompile(`^[a-z0-9-]{8,48}$`)
	rePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]*$`)
)

// 错误。
var (
	ErrNotFound     = errors.New("没有找到")
	ErrBusy         = errors.New("你已有命令在运行，或平台上同时运行的任务太多")
	ErrNotReady     = errors.New("条件不满足")
	ErrSelfApprove  = errors.New("申请人不能批准自己的申请，需要另一名管理员")
	ErrNeedApproval = errors.New("这是变更命令，需要先提交申请并由另一名管理员批准")
)

// DeniedError 表示命令被平台拒绝（没有执行）。
type DeniedError struct{ Verdict Verdict }

func (e *DeniedError) Error() string { return "命令被拒绝：" + e.Verdict.Why }

// InvalidError 表示请求本身不合法。
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// Settings 是持久化的平台设置（只能由有 settings.edit 的管理员改，改动写审计）。
type Settings struct {
	Policy      string   `json:"policy"`      // strict | std | loose
	RetainDays  int      `json:"retainDays"`  // 输出保留天数（1–365）
	SummaryOnly bool     `json:"summaryOnly"` // 任务结束一段时间后，只保留退出码、行数和前 3 行
	ReadRoots   []string `json:"readRoots"`   // 在内置之外追加的「可读内容」目录（例如自定义的 deploy_dir）
	ROViaUnit   bool     `json:"roViaUnit"`   // 只读命令也用临时单元执行（默认在平台进程内执行；沙箱挡住了某些命令时可打开）
}

func defaultSettings() Settings { return Settings{Policy: PolicyStd, RetainDays: 30} }

// Validate 校验并规整设置。
func (s Settings) Validate() (Settings, error) {
	if s.Policy != PolicyStrict && s.Policy != PolicyStd && s.Policy != PolicyLoose {
		return s, &InvalidError{"策略档位只能是 strict、std 或 loose"}
	}
	if s.RetainDays < 1 || s.RetainDays > 365 {
		return s, &InvalidError{"输出保留天数必须在 1–365 之间"}
	}
	var roots []string
	for _, r := range s.ReadRoots {
		orig := strings.TrimSpace(r)
		r = strings.TrimRight(orig, "/")
		if r == "" {
			if orig != "" {
				return s, &InvalidError{"可读目录不能是根目录 /"}
			}
			continue
		}
		if !rePath.MatchString(r) || strings.Contains(r, "..") || strings.Count(r, "/") < 2 {
			return s, &InvalidError{fmt.Sprintf("可读目录 %q 不合法：必须是至少两级的绝对路径（如 /data/tidb-deploy），只含字母数字和 _ . / -", r)}
		}
		for _, d := range deny {
			if d.re.MatchString(r + "/x") {
				return s, &InvalidError{fmt.Sprintf("可读目录 %q 含敏感路径：%s", r, d.why)}
			}
		}
		for _, bad := range []string{"/etc", "/root", "/var/lib", "/proc", "/sys", "/dev", "/boot", "/tidb-data"} {
			if r == bad || strings.HasPrefix(r, bad+"/") {
				return s, &InvalidError{fmt.Sprintf("可读目录 %q 不允许：系统目录与数据目录里的内容不能通过这里读取", r)}
			}
		}
		for _, bad := range []string{"/home", "/usr", "/opt", "/tmp", "/var", "/data"} {
			if r == bad {
				return s, &InvalidError{fmt.Sprintf("可读目录 %q 太宽：请写到具体的部署 / 日志目录（如 /data/tidb-deploy）", r)}
			}
		}
		roots = append(roots, r)
	}
	s.ReadRoots = roots
	return s, nil
}

// Info 是页面需要的中控机信息。执行身份固定为 tidb；Sudo 是 tidb 的 sudo -n 是否可用（nil = 还没探测到）。
type Info struct {
	Host     string         `json:"host"`
	OS       string         `json:"os"`
	Identity string         `json:"identity"`
	Home     string         `json:"home"`   // tidb 的家目录（默认工作目录）
	Exists   bool           `json:"exists"` // 中控机上有 tidb 用户
	Agent    bool           `json:"agent"`  // 执行通道（sudo 入口）可用
	AgentErr string         `json:"agentErr,omitempty"`
	Sudo     *bool          `json:"sudo"`
	SudoErr  string         `json:"sudoErr,omitempty"`
	Settings Settings       `json:"settings"`
	Limits   map[string]int `json:"limits"`
}

// RunReq 是一次执行请求。
type RunReq struct {
	Identity    string   `json:"identity"` // 只接受 tidb（可留空）；root 身份已取消，需要 root 权限的行在开头写 sudo
	Cwd         string   `json:"cwd"`
	Commands    []string `json:"commands"`
	Timeout     int      `json:"timeout"`     // 每条命令的超时（秒）：30 | 120 | 600
	StopOnError bool     `json:"stopOnError"` // 某条命令失败后不再执行后面的
	Reason      string   `json:"reason"`      // 变更申请的原因
	Request     string   `json:"request"`     // 已批准的申请号
}

// Request 是一次变更命令的申请。
type Request struct {
	ID         string     `json:"id"`
	By         string     `json:"by"`
	Identity   string     `json:"identity"`
	Cwd        string     `json:"cwd"`
	Lines      []Item     `json:"lines"` // 已打码的命令与判定
	Timeout    int        `json:"timeout"`
	Stop       bool       `json:"stop"`
	Reason     string     `json:"reason"`
	Hash       string     `json:"-"`
	At         time.Time  `json:"at"`
	ApprovedBy string     `json:"approvedBy,omitempty"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	Job        string     `json:"job,omitempty"` // 已用于哪个任务（一次性）
	State      string     `json:"state"`         // pending | approved | used | expired
}

// Job 是一次执行任务。
type Job struct {
	ID        string     `json:"id"`
	By        string     `json:"by"`
	Identity  string     `json:"identity"`
	Cwd       string     `json:"cwd"`
	Lines     []Item     `json:"lines"`
	Level     Level      `json:"level"`
	Route     string     `json:"route"` // inline | unit
	Sudo      bool       `json:"sudo,omitempty"`
	Timeout   int        `json:"timeout"`
	Stop      bool       `json:"stop"`
	Reason    string     `json:"reason,omitempty"`
	Request   string     `json:"request,omitempty"`
	Approver  string     `json:"approver,omitempty"`
	Started   time.Time  `json:"started"`
	Unit      string     `json:"unit,omitempty"`
	Cancelled bool       `json:"cancelled,omitempty"`
	LaunchErr string     `json:"launchErr,omitempty"`
	Compacted bool       `json:"compacted,omitempty"`
	Status    string     `json:"status"`
	Exit      *int       `json:"exit,omitempty"`
	Ended     *time.Time `json:"ended,omitempty"`
}

// Config 是 Manager 的配置。
type Config struct {
	Dir         string // <数据目录>/shell
	Agent       Agent  // 子进程通道（生产用 SudoAgent；测试注入假的）
	Now         func() time.Time
	OSRelease   string                                   // 默认 /etc/os-release
	LookupUser  func(name string) (home string, ok bool) // 默认 os/user
	NoPathCheck bool                                     // 仅供测试
	RunUser     string                                   // 运行平台的系统用户（任务文件的属主）
}

// Manager 管理设置、申请与任务。
type Manager struct {
	cfg Config

	mu        sync.Mutex
	settings  Settings
	lastSweep time.Time
	inline    map[string]*inlineJob // 正在平台进程内执行的任务
	probed    *Info                 // 探测结果缓存（Agent / Sudo 字段）
	probedAt  time.Time
}

type inlineJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager 创建 Manager。
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Dir == "" {
		return nil, errors.New("shell: 需要数据目录")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OSRelease == "" {
		cfg.OSRelease = "/etc/os-release"
	}
	if cfg.LookupUser == nil {
		cfg.LookupUser = func(n string) (string, bool) {
			u, err := user.Lookup(n)
			if err != nil {
				return "", false
			}
			return u.HomeDir, true
		}
	}
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, "jobs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	m := &Manager{cfg: cfg, settings: defaultSettings(), inline: map[string]*inlineJob{}}
	if b, err := os.ReadFile(filepath.Join(cfg.Dir, "settings.json")); err == nil {
		var s Settings
		if json.Unmarshal(b, &s) == nil {
			if v, err := s.Validate(); err == nil {
				m.settings = v
			}
		}
	}
	m.markInterrupted()
	m.Sweep()
	return m, nil
}

// markInterrupted 在启动时把上次运行留下的「平台进程内执行」任务标记为中断（它们随平台进程一起结束了）。
func (m *Manager) markInterrupted() {
	for _, j := range m.Jobs(0) {
		if j.Route == RouteInline && j.Status == StatusRunning {
			m.appendLog(j.ID, "[平台] 平台服务重启，任务中断")
			_ = os.WriteFile(filepath.Join(m.jobDir(j.ID), "exit"), []byte("143"), 0o600)
		}
	}
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

// Settings 返回当前设置。
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings
	s.ReadRoots = append([]string{}, s.ReadRoots...)
	return s
}

// SetSettings 校验并保存设置。
func (m *Manager) SetSettings(s Settings) (Settings, error) {
	v, err := s.Validate()
	if err != nil {
		return Settings{}, err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := writeFileAtomic(filepath.Join(m.cfg.Dir, "settings.json"), b, 0o600); err != nil {
		return Settings{}, err
	}
	m.mu.Lock()
	m.settings = v
	m.mu.Unlock()
	return v, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Info 返回中控机信息（主机名、系统、tidb 的家目录、执行通道与 sudo 免密的探测结果）。
func (m *Manager) Info() Info {
	host, _ := os.Hostname()
	osName := ""
	if b, err := os.ReadFile(m.cfg.OSRelease); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(ln, "PRETTY_NAME=") {
				osName = strings.Trim(strings.TrimPrefix(ln, "PRETTY_NAME="), `"`)
			}
		}
	}
	home, ok := m.cfg.LookupUser("tidb")
	in := Info{Host: host, OS: osName, Identity: "tidb", Home: home, Exists: ok, Settings: m.Settings(),
		Limits: map[string]int{"maxLines": MaxLines, "maxLineLen": MaxLineLen, "maxOutputBytes": maxOutput}}
	p := m.probe()
	in.Agent, in.AgentErr, in.Sudo, in.SudoErr = p.Agent, p.AgentErr, p.Sudo, p.SudoErr
	if in.Home == "" && p.Home != "" {
		in.Home, in.Exists = p.Home, true
	}
	return in
}

// probe 经执行通道探测 tidb 的 sudo 免密是否可用（结果缓存 probeTTL）。
func (m *Manager) probe() Info {
	m.mu.Lock()
	if m.probed != nil && m.now().Sub(m.probedAt) < probeTTL {
		p := *m.probed
		m.mu.Unlock()
		return p
	}
	m.mu.Unlock()
	p := Info{}
	if m.cfg.Agent == nil {
		p.AgentErr = ErrNoAgent.Error()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		res, err := m.cfg.Agent.Call(ctx, ChildReq{Mode: ModeProbe}, nil)
		cancel()
		switch {
		case err != nil:
			p.AgentErr = err.Error()
		default:
			p.Agent = true
			ok := res.Sudo
			p.Sudo, p.SudoErr, p.Home = &ok, res.Err, res.Home
		}
	}
	m.mu.Lock()
	m.probed, m.probedAt = &p, m.now()
	m.mu.Unlock()
	return p
}

// normalize 校验并规整请求（不判定风险）。
func (m *Manager) normalize(r RunReq) (RunReq, error) {
	switch r.Identity {
	case "", "tidb":
		r.Identity = "tidb"
	case "root":
		return r, &InvalidError{"执行身份固定为 tidb：需要 root 权限的命令请在整行开头加 sudo"}
	default:
		return r, &InvalidError{"执行身份只能是 tidb"}
	}
	home, ok := m.cfg.LookupUser("tidb")
	if !ok {
		if p := m.probe(); p.Home != "" {
			home, ok = p.Home, true
		}
	}
	if !ok {
		return r, &InvalidError{"中控机上没有 tidb 用户"}
	}
	r.Cwd = strings.TrimSpace(r.Cwd)
	if r.Cwd == "" {
		r.Cwd = home
	}
	if !rePath.MatchString(r.Cwd) || strings.Contains(r.Cwd, "..") || strings.Contains(r.Cwd, "//") {
		return r, &InvalidError{"工作目录必须是绝对路径，只含字母数字和 _ . / -"}
	}
	switch r.Timeout {
	case 0:
		r.Timeout = 30
	case 30, 120, 600:
	default:
		return r, &InvalidError{"超时只能是 30、120 或 600 秒"}
	}
	var lines []string
	for _, l := range r.Commands {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return r, &InvalidError{"请输入命令"}
	}
	r.Commands = lines
	r.Reason = strings.TrimSpace(r.Reason)
	return r, nil
}

func hashOf(r RunReq) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%d\n%v\n%s", r.Identity, r.Cwd, r.Timeout, r.StopOnError, strings.Join(r.Commands, "\n"))
	return hex.EncodeToString(h.Sum(nil))
}

// Check 只判定、不执行（页面输入时实时调用）。Verdict.Route 给出将走哪条执行路径。
func (m *Manager) Check(r RunReq) (Verdict, RunReq, error) {
	r, err := m.normalize(r)
	if err != nil {
		return Verdict{}, r, err
	}
	s := m.Settings()
	v := DecideOnHost(s.Policy, r.Commands, s.ReadRoots, false)
	switch {
	case v.Action == ActDeny:
	case v.Level == LevelRO && !s.ROViaUnit:
		v.Route = RouteInline
	default:
		v.Route = RouteUnit
	}
	return v, r, nil
}

func newID(prefix string, now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + "-" + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------- 申请 ----

func (m *Manager) reqFile() string { return filepath.Join(m.cfg.Dir, "requests.json") }

type storedReq struct {
	Request
	StoredHash string `json:"hash"`
}

func (m *Manager) loadReqs() []storedReq {
	b, err := os.ReadFile(m.reqFile())
	if err != nil {
		return nil
	}
	var rs []storedReq
	_ = json.Unmarshal(b, &rs)
	return rs
}

func (m *Manager) saveReqs(rs []storedReq) error {
	if len(rs) > 200 {
		rs = rs[len(rs)-200:]
	}
	b, _ := json.MarshalIndent(rs, "", "  ")
	return writeFileAtomic(m.reqFile(), b, 0o600)
}

func (m *Manager) stateOf(r Request) string {
	switch {
	case r.Job != "":
		return "used"
	case r.ApprovedAt != nil && m.now().Sub(*r.ApprovedAt) > approvalLive:
		return "expired"
	case r.ApprovedAt == nil && m.now().Sub(r.At) > requestValid:
		return "expired"
	case r.ApprovedAt != nil:
		return "approved"
	}
	return "pending"
}

// Requests 返回最近的申请（新 → 旧）。
func (m *Manager) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Request{}
	for _, s := range m.loadReqs() {
		s.Request.State = m.stateOf(s.Request)
		out = append(out, s.Request)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

// RequestApproval 为一批变更命令提交申请：命令内容、工作目录一经申请就锁定（hash），改一个字都要重新申请。
func (m *Manager) RequestApproval(user string, r RunReq) (Request, error) {
	v, r, err := m.Check(r)
	if err != nil {
		return Request{}, err
	}
	if v.Action == ActDeny {
		return Request{}, &DeniedError{v}
	}
	if v.Action != ActApprove {
		return Request{}, fmt.Errorf("%w：这批命令不需要申请，可以直接执行", ErrNotReady)
	}
	if r.Reason == "" {
		return Request{}, &InvalidError{"请填写变更原因"}
	}
	if len([]rune(r.Reason)) > 500 {
		return Request{}, &InvalidError{"变更原因不能超过 500 字"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := Request{ID: newID("q", m.now()), By: user, Identity: r.Identity, Cwd: r.Cwd, Lines: v.Items, Timeout: r.Timeout, Stop: r.StopOnError, Reason: r.Reason, At: m.now(), State: "pending"}
	rs := append(m.loadReqs(), storedReq{Request: q, StoredHash: hashOf(r)})
	if err := m.saveReqs(rs); err != nil {
		return Request{}, err
	}
	return q, nil
}

// Approve 由另一名管理员批准。
func (m *Manager) Approve(user, id string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.loadReqs()
	for i := range rs {
		if rs[i].ID != id {
			continue
		}
		switch st := m.stateOf(rs[i].Request); st {
		case "expired":
			return Request{}, fmt.Errorf("%w：申请已过期，请重新提交", ErrNotReady)
		case "approved", "used":
			return Request{}, fmt.Errorf("%w：申请已经被批准或使用过了", ErrNotReady)
		}
		if rs[i].By == user {
			return Request{}, ErrSelfApprove
		}
		now := m.now()
		rs[i].ApprovedBy, rs[i].ApprovedAt = user, &now
		if err := m.saveReqs(rs); err != nil {
			return Request{}, err
		}
		rs[i].Request.State = "approved"
		return rs[i].Request, nil
	}
	return Request{}, ErrNotFound
}

// ---------------------------------------------------------------- 任务 ----

func (m *Manager) jobDir(id string) string { return filepath.Join(m.cfg.Dir, "jobs", id) }

func (m *Manager) writeJob(j Job) error {
	b, _ := json.MarshalIndent(j, "", "  ")
	return writeFileAtomic(filepath.Join(m.jobDir(j.ID), "job.json"), b, 0o600)
}

// Job 读取任务并推导状态。
func (m *Manager) Job(id string) (Job, error) {
	if !reID.MatchString(id) {
		return Job{}, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(m.jobDir(id), "job.json"))
	if err != nil {
		return Job{}, ErrNotFound
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return Job{}, err
	}
	m.derive(&j)
	return j, nil
}

func (m *Manager) derive(j *Job) {
	dir := m.jobDir(j.ID)
	j.Status, j.Exit, j.Ended = StatusRunning, nil, nil
	if j.LaunchErr != "" {
		j.Status = StatusFailed
		e := j.Started
		j.Ended = &e
		return
	}
	if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil && strings.TrimSpace(string(b)) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			j.Exit = &n
			if st, e := os.Stat(filepath.Join(dir, "exit")); e == nil {
				t := st.ModTime()
				j.Ended = &t
			}
			j.Status = StatusFailed
			if n == 0 {
				j.Status = StatusOK
			}
			return
		}
	}
	if m.now().Sub(j.Started) > staleAfter {
		j.Status = StatusUnknown
	}
}

// Jobs 返回最近的任务（新 → 旧，最多 n 条；0 = 全部）。
func (m *Manager) Jobs(n int) []Job {
	ents, _ := os.ReadDir(filepath.Join(m.cfg.Dir, "jobs"))
	out := []Job{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if j, err := m.Job(e.Name()); err == nil {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Log 读取输出（再遮蔽一遍）：从字节偏移 offset 起，返回内容、新偏移与这一段里的遮蔽处数。
func (m *Manager) Log(id string, offset int64) (string, int64, int, error) {
	if !reID.MatchString(id) {
		return "", 0, 0, ErrNotFound
	}
	f, err := os.Open(filepath.Join(m.jobDir(id), "out.log"))
	if err != nil {
		return "", 0, 0, ErrNotFound
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, 0, err
	}
	if offset < 0 || offset > st.Size() {
		offset = 0
	}
	n := st.Size() - offset
	if n > maxChunk {
		n = maxChunk
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil && n > 0 {
		return "", 0, 0, err
	}
	if offset+n < st.Size() { // 不把一行或一个多字节字符截成两半
		if i := strings.LastIndexByte(string(buf), '\n'); i >= 0 {
			buf = buf[:i+1]
		}
	}
	text, hit := MaskText(string(buf))
	_ = hit
	return text, offset + int64(len(buf)), CountMasks(text), nil
}

func (m *Manager) appendLog(id, line string) {
	f, err := os.OpenFile(filepath.Join(m.jobDir(id), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func (m *Manager) running() (all int, byUser map[string]int) {
	byUser = map[string]int{}
	for _, j := range m.Jobs(50) {
		if j.Status == StatusRunning {
			all++
			byUser[j.By]++
		}
	}
	return
}

// Run 执行一批命令：判定 → （变更命令）核对申请 → 只读在平台进程内执行，变更起 systemd-run 临时单元。不需要 root 密码。
func (m *Manager) Run(ctx context.Context, user string, r RunReq) (Job, error) {
	v, r, err := m.Check(r)
	if err != nil {
		return Job{}, err
	}
	if v.Action == ActDeny {
		return Job{}, &DeniedError{v}
	}
	var approver, reason, reqID string
	if v.Action == ActApprove {
		q, err := m.checkApproval(user, r)
		if err != nil {
			return Job{}, err
		}
		approver, reason, reqID = q.ApprovedBy, q.Reason, q.ID
	}
	if m.cfg.Agent == nil {
		return Job{}, ErrNoAgent
	}
	m.Sweep()

	m.mu.Lock()
	all, by := m.running()
	if by[user] > 0 || all >= maxRunning {
		m.mu.Unlock()
		return Job{}, ErrBusy
	}
	now := m.now()
	j := Job{ID: newID("s", now), By: user, Identity: r.Identity, Cwd: r.Cwd, Lines: v.Items, Level: v.Level, Route: v.Route, Sudo: v.Sudo, Timeout: r.Timeout, Stop: r.StopOnError,
		Reason: reason, Request: reqID, Approver: approver, Started: now}
	if j.Route == RouteUnit {
		j.Unit = "tidbops-shell-" + j.ID
	}
	dir := m.jobDir(j.ID)
	err = m.prepare(j, dir, r)
	if err == nil {
		err = m.writeJob(j)
	}
	m.mu.Unlock()
	if err != nil {
		_ = os.RemoveAll(dir)
		return Job{}, err
	}
	if j.Route == RouteInline {
		m.startInline(j, r)
		if reqID != "" {
			m.markUsed(reqID, j.ID)
		}
		m.derive(&j)
		return j, nil
	}

	cctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	res, rerr := m.cfg.Agent.Call(cctx, ChildReq{Mode: ModeLaunch, JobID: j.ID, Unit: j.Unit, Script: filepath.ToSlash(filepath.Join(dir, "run.sh"))}, nil)
	if rerr == nil && (res.Err != "" || res.Exit != 0) {
		rerr = &AgentError{describeRes(res)}
	}
	if rerr != nil {
		j.LaunchErr = rerr.Error()
		_ = m.writeJob(j)
		m.appendLog(j.ID, "[平台] 启动失败："+rerr.Error())
		_ = os.Remove(filepath.Join(dir, "cmds.txt"))
		m.derive(&j)
		return j, rerr
	}
	if reqID != "" {
		m.markUsed(reqID, j.ID)
	}
	if res.Out != "" {
		m.appendLog(j.ID, "[平台] "+res.Out)
	}
	m.derive(&j)
	return j, nil
}

// startInline 在后台经执行通道以 run 模式执行只读命令，输出按行追加到 out.log（子进程已遮蔽），结束后写退出码。
func (m *Manager) startInline(j Job, r RunReq) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(r.Commands)*r.Timeout+60)*time.Second)
	ij := &inlineJob{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.inline[j.ID] = ij
	roots := append([]string{}, m.settings.ReadRoots...)
	m.mu.Unlock()
	go func() {
		defer close(ij.done)
		defer cancel()
		f, ferr := os.OpenFile(filepath.Join(m.jobDir(j.ID), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
		emit := func(line string) {
			if ferr == nil {
				_, _ = f.WriteString(line + "\n")
			}
		}
		res, err := m.cfg.Agent.Call(ctx, ChildReq{Mode: ModeRun, JobID: j.ID, Cwd: r.Cwd, Timeout: r.Timeout, Stop: r.StopOnError, Lines: r.Commands, Roots: roots}, emit)
		code := res.Exit
		switch {
		case err != nil && ctx.Err() != nil:
			emit("[平台] 任务已终止（取消或超过总时限）")
			code = 143
		case err != nil:
			emit("[平台] 执行通道错误：" + err.Error())
			code = 1
		case res.Err != "":
			emit("[平台] " + res.Err)
			if code == 0 {
				code = 1
			}
		}
		if ferr == nil {
			_ = f.Close()
		}
		_ = os.WriteFile(filepath.Join(m.jobDir(j.ID), "exit"), []byte(strconv.Itoa(code)), 0o600)
		m.mu.Lock()
		delete(m.inline, j.ID)
		m.mu.Unlock()
	}()
}

// Wait 等待一个平台进程内执行的任务结束（最多 d；测试与取消用）。不是这类任务或已结束时立即返回 true。
func (m *Manager) Wait(id string, d time.Duration) bool {
	m.mu.Lock()
	ij := m.inline[id]
	m.mu.Unlock()
	if ij == nil {
		return true
	}
	select {
	case <-ij.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (m *Manager) checkApproval(user string, r RunReq) (Request, error) {
	if r.Request == "" {
		return Request{}, ErrNeedApproval
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.loadReqs() {
		if s.ID != r.Request {
			continue
		}
		switch m.stateOf(s.Request) {
		case "pending":
			return Request{}, fmt.Errorf("%w：还没有另一名管理员批准", ErrNotReady)
		case "expired":
			return Request{}, fmt.Errorf("%w：申请已过期，请重新提交", ErrNotReady)
		case "used":
			return Request{}, fmt.Errorf("%w：这个申请已经用过了（批准只对一次执行有效）", ErrNotReady)
		}
		if s.By != user {
			return Request{}, fmt.Errorf("%w：这个申请是 %s 提交的，只有申请人可以执行", ErrNotReady, s.By)
		}
		if s.StoredHash != hashOf(r) {
			return Request{}, fmt.Errorf("%w：命令、工作目录或超时与申请时不一致；改了内容要重新申请", ErrNotReady)
		}
		return s.Request, nil
	}
	return Request{}, ErrNotFound
}

func (m *Manager) markUsed(reqID, jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.loadReqs()
	for i := range rs {
		if rs[i].ID == reqID {
			rs[i].Job = jobID
		}
	}
	_ = m.saveReqs(rs)
}

// prepare 建任务目录。平台进程内执行的任务只有 out.log / exit / job.json（命令经 stdin 交给子进程，不落盘）；
// 临时单元的任务另有 cmds.txt（命令，数据）、mask.sed、run.sh。
func (m *Manager) prepare(j Job, dir string, r RunReq) error {
	if !m.cfg.NoPathCheck && !rePath.MatchString(filepath.ToSlash(m.cfg.Dir)) {
		return fmt.Errorf("数据目录 %q 含不支持的字符（只允许字母数字和 _ . / -），无法交给临时单元执行", m.cfg.Dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []string{"out.log", "exit"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			return err
		}
	}
	if j.Route != RouteUnit {
		return nil
	}
	// sudo 行写成规范形式「sudo <命令>」（去掉 -n、多余空白），脚本只按这个前缀识别
	lines := make([]string, 0, len(r.Commands))
	for _, l := range r.Commands {
		if rest, ok := SplitSudo(l); ok {
			l = "sudo " + rest
		}
		lines = append(lines, l)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmds.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "mask.sed"), []byte(SedScript()), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "run.sh"), []byte(m.runScript(j, filepath.ToSlash(dir))), 0o700)
}

// runScript 生成任务脚本（临时单元以 root 运行）。命令文本只放在 cmds.txt（数据），从不拼进脚本；目录 / 超时都是校验过的值。
// 普通行经 runuser 切到 tidb 执行；整行以 sudo 开头的行去掉前缀后直接以 root 执行（护栏保证前缀只有「sudo」或「sudo -n」）。
func (m *Manager) runScript(j Job, dir string) string {
	stop := 0
	if j.Stop {
		stop = 1
	}
	return fmt.Sprintf(`#!/bin/bash
# 由平台生成：在 transient systemd 单元里（root，不受平台服务的沙箱限制）逐条执行 cmds.txt 里的命令
JOB='%[1]s'
IDN='%[2]s'
CWD='%[3]s'
TMO=%[4]d
STOP=%[5]d
HOMEDIR="$(getent passwd "$IDN" | cut -d: -f6)"; [ -n "$HOMEDIR" ] || HOMEDIR=/home/tidb
PATHV="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$HOMEDIR/.tiup/bin"
umask 077
run_all() {
  local rc=0 line rest
  echo "[平台] 任务 %[6]s 开始（身份 $IDN，目录 $CWD，$(date '+%%F %%T')）"
  while IFS= read -r line || [ -n "$line" ]; do
    [ -z "$line" ] && continue
    printf '$ %%s\n' "$line"
    case "$line" in
      sudo\ *)
        rest="${line#sudo }"
        env -i HOME=/root USER=root PATH="$PATHV" LANG=C.UTF-8 TERM=dumb timeout -k 5 "$TMO" bash -c 'cd "$0" && exec bash -c "$1"' "$CWD" "$rest" </dev/null 2>&1
        ;;
      *)
        timeout -k 5 "$TMO" runuser -u "$IDN" -- env -i HOME="$HOMEDIR" USER="$IDN" PATH="$PATHV" LANG=C.UTF-8 TERM=dumb bash -c 'cd "$0" && exec bash -c "$1"' "$CWD" "$line" </dev/null 2>&1
        ;;
    esac
    rc=$?
    [ $rc -eq 124 ] && echo "[平台] 命令超过 ${TMO} 秒仍未结束，已终止"
    if [ $rc -ne 0 ] && [ "$STOP" = 1 ]; then echo "[平台] 命令退出码 $rc，按「遇错停止」不再执行后面的命令"; return $rc; fi
  done < "$JOB/cmds.txt"
  return $rc
}
run_all 2>&1 | sed -u -E -f "$JOB/mask.sed" | head -c %[7]d >> "$JOB/out.log"
rc=${PIPESTATUS[0]}
[ "$(stat -c %%s "$JOB/out.log" 2>/dev/null || echo 0)" -ge %[7]d ] && echo "[平台] 输出超过 1 MiB，已截断" >> "$JOB/out.log"
echo "[平台] 任务结束，退出码 $rc（$(date '+%%F %%T')）" >> "$JOB/out.log"
rm -f "$JOB/cmds.txt"
printf '%%s' "$rc" > "$JOB/exit"
exit $rc
`, dir, j.Identity, j.Cwd, j.Timeout, stop, j.ID, maxOutput)
}

// Cancel 取消运行中的任务：平台进程内的任务直接终止子进程；临时单元经 sudo 入口 systemctl stop。不需要密码。
func (m *Manager) Cancel(ctx context.Context, user, id string) (Job, error) {
	j, err := m.Job(id)
	if err != nil {
		return Job{}, err
	}
	if j.Status != StatusRunning {
		return j, fmt.Errorf("%w：任务不在运行", ErrNotReady)
	}
	if j.Route == RouteInline {
		m.mu.Lock()
		ij := m.inline[id]
		m.mu.Unlock()
		if ij != nil {
			ij.cancel()
			select {
			case <-ij.done:
			case <-time.After(15 * time.Second):
			}
		} else { // 平台重启过，子进程早已不在
			_ = os.WriteFile(filepath.Join(m.jobDir(j.ID), "exit"), []byte("143"), 0o600)
		}
		m.appendLog(j.ID, "[平台] 已由 "+user+" 取消")
	} else {
		if m.cfg.Agent == nil {
			return Job{}, ErrNoAgent
		}
		cctx, cancel := context.WithTimeout(ctx, runTimeout)
		defer cancel()
		res, rerr := m.cfg.Agent.Call(cctx, ChildReq{Mode: ModeStop, Unit: j.Unit}, nil)
		if rerr == nil && res.Err != "" {
			rerr = &AgentError{res.Err}
		}
		if rerr != nil {
			return Job{}, rerr
		}
		m.appendLog(j.ID, "[平台] 已由 "+user+" 取消（systemctl stop "+j.Unit+"）")
		_ = os.WriteFile(filepath.Join(m.jobDir(j.ID), "exit"), []byte("143"), 0o600)
		// 命令文件里可能有口令类内容，被杀掉的脚本来不及自己删：平台属主的目录里，这个文件也是平台创建的，直接删
		_ = os.Remove(filepath.Join(m.jobDir(j.ID), "cmds.txt"))
	}
	j.Cancelled = true
	_ = m.writeJob(j)
	nj, _ := m.Job(j.ID)
	return nj, nil
}

// Sweep 清理过期任务（超过保留天数的整个任务目录）；「只存摘要」时把已结束一段时间的输出压缩成摘要。每小时最多做一次。
func (m *Manager) Sweep() {
	m.mu.Lock()
	if !m.lastSweep.IsZero() && m.now().Sub(m.lastSweep) < time.Hour {
		m.mu.Unlock()
		return
	}
	m.lastSweep = m.now()
	set := m.settings
	m.mu.Unlock()
	ents, _ := os.ReadDir(filepath.Join(m.cfg.Dir, "jobs"))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		j, err := m.Job(e.Name())
		if err != nil {
			continue
		}
		if m.now().Sub(j.Started) > time.Duration(set.RetainDays)*24*time.Hour && j.Status != StatusRunning {
			_ = os.RemoveAll(m.jobDir(j.ID))
			continue
		}
		if set.SummaryOnly && !j.Compacted && j.Ended != nil && m.now().Sub(*j.Ended) > 10*time.Minute {
			m.compact(j)
		}
	}
}

func (m *Manager) compact(j Job) {
	p := filepath.Join(m.jobDir(j.ID), "out.log")
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	keep := lines
	if len(keep) > 3 {
		keep = lines[:3]
	}
	keep = append(keep, fmt.Sprintf("[平台] 按「只存摘要」策略，历史只保留前 3 行；原输出共 %d 行，已删除", len(lines)))
	_ = os.WriteFile(p, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
	j.Compacted = true
	_ = m.writeJob(j)
}
