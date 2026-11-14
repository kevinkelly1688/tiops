// @author: kevinkelly
// @license: Apache License 2.0
// @file: history.go
// @time: 2026/10/28

package release

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// HistoryEntry 是升级历史（<数据目录>/upgrades.jsonl）里的一行。
type HistoryEntry struct {
	Time       time.Time `json:"time"`
	Action     string    `json:"action"` // install | upgrade | rollback
	From       string    `json:"from"`
	To         string    `json:"to"`
	OK         bool      `json:"ok"`
	RolledBack bool      `json:"rolledBack"`
	Backup     string    `json:"backup,omitempty"`
	SchemaFrom int       `json:"schemaFrom"`
	SchemaTo   int       `json:"schemaTo"`
	Error      string    `json:"error,omitempty"`
	Operator   string    `json:"operator,omitempty"`
}

// readHistory 读取全部历史（文件不存在返回空；坏行跳过）。
func readHistory(path string) ([]HistoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var h HistoryEntry
		if json.Unmarshal(sc.Bytes(), &h) == nil && h.Action != "" {
			out = append(out, h)
		}
	}
	return out, sc.Err()
}

// appendHistory 追加一行。
func appendHistory(path string, h HistoryEntry) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeHistory 整体重写（restore 之后用来保住历史）。
func writeHistory(path string, hs []HistoryEntry) error {
	var buf []byte
	for _, h := range hs {
		b, err := json.Marshal(h)
		if err != nil {
			return err
		}
		buf = append(append(buf, b...), '\n')
	}
	return writeFileAtomic(path, buf, 0o600)
}

// record 把一条历史写进数据目录；失败只告警（不影响主流程）。
func (e *Engine) record(h HistoryEntry) {
	if h.Time.IsZero() {
		h.Time = e.Sys.Now()
	}
	h.Operator = e.Operator
	if _, err := os.Stat(e.L.Data()); err != nil {
		return // 数据目录还不存在（安装中途失败）：没有地方可写
	}
	if err := appendHistory(e.L.History(), h); err != nil {
		e.printf("警告：写升级历史失败：%v\n", err)
	}
}
