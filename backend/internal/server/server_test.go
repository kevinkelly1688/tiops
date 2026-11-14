// @author: kevinkelly
// @license: Apache License 2.0
// @file: server_test.go
// @time: 2026/10/28

package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tidbops/internal/collect"
	"tidbops/internal/fakecluster"
	"tidbops/internal/fixtures"
	"tidbops/internal/model"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

func setup(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	fc := fakecluster.Start("prod-tidb-01")
	t.Cleanup(fc.Close)
	live := source.NewLive(source.LiveConfig{Endpoints: fc.Endpoints(), MySQL: source.MySQLConfig{Host: "h", User: "u"}, Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile})
	params := source.CatalogParams{Cluster: "prod-tidb-01", Home: "/home/tidb", Now: time.Now()}
	items := source.DefaultCatalog(params)
	fx := filepath.Join(t.TempDir(), "fx")
	rec := source.NewRecorder(live, fx, "prod-tidb-01", "t", fixtures.Options{MaskSQLLiterals: true, RedactSecrets: true})
	rec.RecordAll(context.Background(), items)
	if err := rec.Finish(items); err != nil {
		t.Fatal(err)
	}
	rp, err := source.NewReplay(fx)
	if err != nil {
		t.Fatal(err)
	}
	web := t.TempDir()
	for name, body := range map[string]string{"index.html": "<html>home</html>", "app.js": "//js", "style.css": "/*css*/", "secret.txt": "TOPSECRET", "config.json": "{}"} {
		os.WriteFile(filepath.Join(web, name), []byte(body), 0o644)
	}
	for name, body := range map[string]string{"js/main.js": "//js", "css/main.css": "/*css*/", "js/notes.txt": "x", "js/sub/a.js": "//sub", "other/x.js": "//other"} {
		os.MkdirAll(filepath.Dir(filepath.Join(web, name)), 0o755)
		os.WriteFile(filepath.Join(web, name), []byte(body), 0o644)
	}
	os.MkdirAll(filepath.Join(web, "fixtures", "prod"), 0o755)
	os.WriteFile(filepath.Join(web, "fixtures", "prod", "manifest.json"), []byte("{}"), 0o644)
	os.MkdirAll(filepath.Join(web, "backend"), 0o755)
	os.WriteFile(filepath.Join(web, "backend", "go.mod"), []byte("module x"), 0o644)
	os.MkdirAll(filepath.Join(web, "help", "sub"), 0o755)
	os.WriteFile(filepath.Join(web, "help", "quickstart.md"), []byte("# 快速入门"), 0o644)
	os.WriteFile(filepath.Join(web, "help", "shell.md"), []byte("# 中控机命令"), 0o644)
	os.WriteFile(filepath.Join(web, "help", "deploy.md"), []byte("# 安装部署"), 0o644)
	os.WriteFile(filepath.Join(web, "help", "README.md"), []byte("# 规范"), 0o644)
	os.WriteFile(filepath.Join(web, "help", "notes.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(web, "help", "sub", "a.md"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(web, "deploy", "dist"), 0o755)
	os.WriteFile(filepath.Join(web, "deploy", "dist", "tidb-deploy-0.2.0.tar.gz"), []byte("PKG"), 0o644)
	os.WriteFile(filepath.Join(web, "deploy", "tidb-deploy.sh"), []byte("#!/bin/bash"), 0o644)
	s := server.New(server.Config{Col: collect.New(rp, params), Mode: "replay", Web: web, SnapTTL: time.Minute, Logf: func(string, ...any) {}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, web
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b), r.Header
}

func TestAPI(t *testing.T) {
	ts, _ := setup(t)
	code, body, h := get(t, ts.URL+"/api/health")
	if code != 200 || !strings.Contains(body, `"readOnly":true`) || !strings.Contains(body, `"mode":"replay"`) {
		t.Fatalf("health: %d %s", code, body)
	}
	if !strings.Contains(h.Get("Content-Type"), "application/json") || h.Get("Cache-Control") != "no-store" {
		t.Errorf("API 应返回 JSON 且不缓存: %v", h)
	}
	code, body, _ = get(t, ts.URL+"/api/snapshot")
	var snap model.Snapshot
	if code != 200 || json.Unmarshal([]byte(body), &snap) != nil {
		t.Fatalf("snapshot: %d %.200s", code, body)
	}
	if len(snap.Topology.Instances) != 16 || snap.Mode != "replay" || len(snap.Findings) < 4 || len(snap.Slow) != 6 {
		t.Errorf("快照内容不对: inst=%d findings=%d slow=%d errs=%v", len(snap.Topology.Instances), len(snap.Findings), len(snap.Slow), snap.Errors)
	}
	for _, p := range []string{"topology", "overview", "slow", "hot", "alerts", "findings", "env"} {
		if c, b, _ := get(t, ts.URL+"/api/"+p); c != 200 || len(b) < 2 {
			t.Errorf("/api/%s: %d", p, c)
		}
	}
	if c, _, _ := get(t, ts.URL+"/api/unknown"); c != 404 {
		t.Errorf("未知接口应 404，实际 %d", c)
	}
	// 回放数据已脱敏
	_, slow, _ := get(t, ts.URL+"/api/slow")
	if strings.Contains(slow, "13800138000") || strings.Contains(slow, "SKU-1001") {
		t.Error("API 返回的 SQL 应已脱敏")
	}
}

func TestReadOnlyAndHeaders(t *testing.T) {
	ts, _ := setup(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(m, ts.URL+"/api/snapshot", strings.NewReader("{}"))
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s 应返回 405（只读服务），实际 %d", m, r.StatusCode)
		}
	}
	_, _, h := get(t, ts.URL+"/")
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if h.Get(k) != want {
			t.Errorf("缺少安全头 %s: %q", k, h.Get(k))
		}
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("缺少 CSP")
	}
}

func TestStaticAllowlist(t *testing.T) {
	ts, _ := setup(t)
	for path, want := range map[string]int{
		"/": 200, "/index.html": 200, "/app.js": 200, "/style.css": 200,
		"/js/main.js": 200, "/css/main.css": 200, "/js/notes.txt": 404, "/js/sub/a.js": 404, "/other/x.js": 404, "/js/": 404, "/css/": 404,
		"/secret.txt": 404, "/config.json": 404, "/fixtures/prod/manifest.json": 404, "/fixtures/": 404, "/backend/go.mod": 404, "/backend/": 404,
		"/help/quickstart.md": 200, "/help/shell.md": 200, "/help/deploy.md": 200, "/help/README.md": 404, "/help/notes.txt": 404, "/help/sub/a.md": 404, "/help/": 404, "/help/..%2fapp.js": 404,
		"/deploy/dist/tidb-deploy-0.2.0.tar.gz": 200, "/deploy/tidb-deploy.sh": 404, "/deploy/": 404, "/deploy/dist/": 404,
		"/%2e%2e/etc/passwd": 404, "/..%2fsecret.txt": 404, "/app.js/../secret.txt": 404,
	} {
		if c, _, _ := get(t, ts.URL+path); c != want {
			t.Errorf("GET %s = %d，期望 %d", path, c, want)
		}
	}
	_, body, h := get(t, ts.URL+"/deploy/dist/tidb-deploy-0.2.0.tar.gz")
	if body != "PKG" || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Errorf("部署包应作为附件下载: %q %v", body, h)
	}
	_, body, _ = get(t, ts.URL+"/")
	if !strings.Contains(body, "home") {
		t.Errorf("首页内容不对: %s", body)
	}
}

func TestSnapshotCache(t *testing.T) {
	ts, _ := setup(t)
	_, a, _ := get(t, ts.URL+"/api/snapshot")
	_, b, _ := get(t, ts.URL+"/api/snapshot")
	var sa, sb model.Snapshot
	json.Unmarshal([]byte(a), &sa)
	json.Unmarshal([]byte(b), &sb)
	if !sa.CollectedAt.Equal(sb.CollectedAt) {
		t.Error("TTL 内的两次请求应返回同一份缓存快照")
	}
	_, c, _ := get(t, ts.URL+"/api/snapshot?refresh=1")
	var sc model.Snapshot
	json.Unmarshal([]byte(c), &sc)
	if !sc.CollectedAt.Equal(sa.CollectedAt) {
		t.Error("2 秒内的强制刷新应被限速（返回缓存）")
	}
	time.Sleep(2100 * time.Millisecond)
	_, d, _ := get(t, ts.URL+"/api/snapshot?refresh=1")
	var sd model.Snapshot
	json.Unmarshal([]byte(d), &sd)
	if !sd.CollectedAt.After(sa.CollectedAt) {
		t.Error("超过 2 秒后强制刷新应重新采集")
	}
}
