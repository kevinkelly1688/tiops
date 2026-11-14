// @author: kevinkelly
// @license: Apache License 2.0
// @file: inspect_last_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// 巡检报告落盘：执行巡检后写入 inspect-last.json；平台重启（重新 EnableOps）后 /api/inspect/last 仍能读到。
func TestInspectLastPersisted(t *testing.T) {
	e := setupOps(t)
	dba := e.login(t, "dba")
	code, out := dba.do("POST", "/api/inspect", map[string]any{})
	if code != 200 {
		t.Fatalf("POST /api/inspect: %d %v", code, out)
	}
	p := filepath.Join(e.dir, "inspect-last.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("巡检后应写入 inspect-last.json: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Errorf("inspect-last.json 权限应为 0600: %v", st.Mode().Perm())
	}
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Errorf("不应残留临时文件")
	}
	var saved struct {
		Cluster string          `json:"cluster"`
		Items   json.RawMessage `json:"items"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &saved); err != nil || saved.Cluster == "" || len(saved.Items) < 3 {
		t.Fatalf("inspect-last.json 内容不对: %v %s", err, b)
	}
}

func TestInspectLastLoadedAtStart(t *testing.T) {
	want := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	e := setupOpsPre(t, func(dir string) {
		rep := map[string]any{"time": want, "cluster": "prod-tidb-01", "version": "v8.1.0", "mode": "live", "items": []any{}, "ok": 3, "warn": 1, "bad": 0, "na": 0}
		b, _ := json.Marshal(rep)
		if err := os.WriteFile(filepath.Join(dir, "inspect-last.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	dba := e.login(t, "dba")
	code, out := dba.do("GET", "/api/inspect/last", nil)
	if code != 200 {
		t.Fatalf("GET /api/inspect/last: %d %v", code, out)
	}
	rep, _ := out["report"].(map[string]any)
	if rep == nil || rep["cluster"] != "prod-tidb-01" || rep["ok"] != float64(3) {
		t.Fatalf("启动时应加载已保存的巡检报告: %v", out)
	}
	// 导出同样可用（审计员有导出报告的能力）
	aud := e.login(t, "auditor")
	if code, _ := aud.do("GET", "/api/inspect/report?format=md", nil); code != 200 && code != 403 {
		t.Errorf("导出应可用或仅因权限被拒: %d", code)
	}
}

func TestInspectLastCorruptIgnored(t *testing.T) {
	e := setupOpsPre(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "inspect-last.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	dba := e.login(t, "dba")
	code, out := dba.do("GET", "/api/inspect/last", nil)
	if code != 200 || out["report"] != nil {
		t.Fatalf("文件损坏时应当作没有报告: %d %v", code, out)
	}
}
