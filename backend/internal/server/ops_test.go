// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_test.go
// @time: 2026/10/28

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/change"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
	"tidbops/internal/fakecluster"
	"tidbops/internal/fixtures"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

var opsKey = []byte("0123456789abcdef0123456789abcdef")

type opsEnv struct {
	ts       *httptest.Server
	dir      string
	readonly string
	srv      *server.Server
	sim      *exec.SimRunner
	audit    *audit.Log
}

func setupOps(t *testing.T) *opsEnv { return setupOpsPre(t, nil) }

// setupOpsPre 与 setupOps 相同，但在 EnableOps 之前以数据目录调用 pre（可预置 roles.json 等文件）。
func setupOpsPre(t *testing.T, pre func(dir string)) *opsEnv { return setupOpsCfg(t, pre, nil) }

// setupOpsCfg 在 setupOpsPre 的基础上允许调整 server.Config（如 ICP / Unavailable）。
func setupOpsCfg(t *testing.T, pre func(dir string), tweak func(*server.Config)) *opsEnv {
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
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>home</html>"), 0o644)
	scfg := server.Config{Col: collect.New(rp, params), Mode: "replay", Web: web, SnapTTL: time.Minute, Logf: func(string, ...any) {}}
	if tweak != nil {
		tweak(&scfg)
	}
	s := server.New(scfg)

	dir := t.TempDir()
	users, err := authz.OpenStore(filepath.Join(dir, "users.json"), []byte("master-key-for-test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := authz.SeedDemoUsers(users); err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	pol := exec.Policy{Clusters: []string{"prod-tidb-01"}, TempDir: t.TempDir(), AlertURL: "http://127.0.0.1:1", MySQLUser: "tidbops_ops", BackupRoots: []string{"local:///backup/"}}
	sim := &exec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: opsKey}
	if pre != nil {
		pre(dir)
	}
	ro := filepath.Join(dir, "READONLY")
	if err := s.EnableOps(server.OpsConfig{Dir: dir, Env: "staging", Users: users, Audit: al, Runner: sim, Prober: &change.SimProber{Polls: 1}, Key: opsKey, ReadOnlyFile: ro, Sim: true, ExecMode: "sim", WaitScale: 0.001, TickEvery: time.Hour}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &opsEnv{ts: ts, dir: dir, readonly: ro, srv: s, sim: sim, audit: al}
}

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf string
}

func (e *opsEnv) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: e.ts.URL, hc: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
		if c.csrf != "" {
			req.Header.Set("X-CSRF-Token", c.csrf)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out
}

func (e *opsEnv) login(t *testing.T, user string) *client {
	t.Helper()
	c := e.client(t)
	code, out := c.do("POST", "/api/auth/login", map[string]string{"user": user, "password": authz.DemoPassword})
	if code != 200 {
		t.Fatalf("登录 %s 失败: %d %v", user, code, out)
	}
	c.csrf, _ = out["csrf"].(string)
	return c
}

func TestOpsRequiresLoginAndCSRF(t *testing.T) {
	e := setupOps(t)
	anon := e.client(t)
	for _, p := range []string{"/api/snapshot", "/api/topology", "/api/changes", "/api/audit", "/api/users"} {
		if code, _ := anon.do("GET", p, nil); code != 401 {
			t.Errorf("%s 未登录应 401，得到 %d", p, code)
		}
	}
	if code, out := anon.do("GET", "/api/health", nil); code != 200 || out["ops"] == nil {
		t.Errorf("health 应公开并带 ops 信息: %v", out)
	}
	if code, _ := anon.do("POST", "/api/changes", map[string]any{"kind": "inst.op"}); code != 401 {
		t.Errorf("未登录写接口应 401: %d", code)
	}
	if code, out := anon.do("POST", "/api/auth/login", map[string]string{"user": "dba", "password": "wrong-password-1"}); code != 401 || !strings.Contains(out["error"].(string), "用户名或口令错误") {
		t.Errorf("错误口令: %d %v", code, out)
	}
	c := e.login(t, "dba")
	if code, out := c.do("GET", "/api/auth/me", nil); code != 200 || out["authenticated"] != true {
		t.Errorf("me: %v", out)
	}
	saved := c.csrf
	c.csrf = ""
	if code, _ := c.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}}); code != 403 {
		t.Errorf("缺少 CSRF 应 403: %d", code)
	}
	c.csrf = saved
	if code, _ := c.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}}); code != 200 {
		t.Errorf("带 CSRF 应通过: %d", code)
	}
	// 跨站 Origin
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/changes/preview", strings.NewReader(`{}`))
	req.Header.Set("X-CSRF-Token", saved)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("跨站 Origin 应 403: %d", resp.StatusCode)
	}
	c.do("POST", "/api/auth/logout", nil)
	if code, _ := c.do("GET", "/api/snapshot", nil); code != 401 {
		t.Errorf("退出后应 401: %d", code)
	}
}

func TestOpsRoleScopedSQL(t *testing.T) {
	e := setupOps(t)
	count := func(user string) (int, map[string]bool) {
		c := e.login(t, user)
		code, out := c.do("GET", "/api/snapshot", nil)
		if code != 200 {
			t.Fatalf("%s: %d", user, code)
		}
		slow, _ := out["slow"].([]any)
		dbs := map[string]bool{}
		for _, g := range slow {
			dbs[g.(map[string]any)["db"].(string)] = true
		}
		return len(slow), dbs
	}
	if n, _ := count("viewer"); n != 0 {
		t.Errorf("访客不应看到慢 SQL: %d", n)
	}
	if n, _ := count("auditor"); n != 0 {
		t.Errorf("审计员不应看到慢 SQL: %d", n)
	}
	nd, _ := count("dba")
	if nd < 6 {
		t.Errorf("DBA 应看到全部慢 SQL: %d", nd)
	}
	n, dbs := count("dev")
	if n == 0 || n >= nd || len(dbs) != 1 || !dbs["shop"] {
		t.Errorf("开发只能看到被授权库 shop 的 SQL: n=%d dbs=%v", n, dbs)
	}
}

func TestOpsWriteSwitchDualControlAndChangeFlow(t *testing.T) {
	e := setupOps(t)
	dba, sre, approver, admin, admin2, auditor := e.login(t, "dba"), e.login(t, "sre"), e.login(t, "approver"), e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "auditor")
	params := map[string]any{"node": "tikv-2", "action": "restart"}
	// 开关未开：拒绝，并说明原因
	code, out := dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": params})
	if code != 409 && code != 403 || !strings.Contains(out["error"].(string), "开关") {
		t.Fatalf("开关关闭应拒绝: %d %v", code, out)
	}
	// 非管理员不能动开关
	if code, _ := dba.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 403 {
		t.Errorf("DBA 不能开写开关: %d", code)
	}
	// 管理员提议 → 同一人不能自己确认 → 另一名管理员确认
	if code, out := admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 200 || out["enabled"] != false || out["pending"] == nil {
		t.Fatalf("提议: %d %v", code, out)
	}
	if code, _ := admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 409 {
		t.Errorf("同一管理员不能自己确认: %d", code)
	}
	if code, out := admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 200 || out["enabled"] != true {
		t.Fatalf("确认: %d %v", code, out)
	}
	// SRE 没有重启以外的能力：重启可以发起，扩缩容不行
	if code, _ := sre.do("POST", "/api/changes", map[string]any{"kind": "scale.in", "params": map[string]any{"nodes": []string{"tikv-6"}}}); code != 403 {
		t.Errorf("SRE 不能缩容: %d", code)
	}
	// 阻断的计划返回 422 与检查结果
	code, out = dba.do("POST", "/api/changes", map[string]any{"kind": "scale.in", "params": map[string]any{"nodes": []string{"tikv-1", "tikv-2", "tikv-3", "tikv-4", "tikv-5"}}})
	if code != 422 || out["code"] != "blocked" || out["plan"] == nil {
		t.Errorf("不满足副本数的缩容应被阻断: %d %v", code, out)
	}
	// 预览
	code, out = dba.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": params})
	plan, _ := out["plan"].(map[string]any)
	if code != 200 || plan["risk"] != "mid" || out["levels"].(float64) != 1 || out["gate"] != "" {
		t.Errorf("预览: %d %v", code, out)
	}
	// 创建 → 待审批；申请人不能自批；审批人批准后自动执行
	code, out = dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": params, "note": "测试重启"})
	if code != 200 || out["state"] != "pending" {
		t.Fatalf("创建: %d %v", code, out)
	}
	id := out["id"].(string)
	if code, _ := dba.do("POST", "/api/changes/"+id+"/approve", nil); code != 403 {
		t.Errorf("申请人不能审批自己: %d", code)
	}
	if code, _ := sre.do("POST", "/api/changes/"+id+"/approve", nil); code != 403 {
		t.Errorf("SRE 没有审批权限: %d", code)
	}
	if code, out := approver.do("POST", "/api/changes/"+id+"/approve", map[string]any{"comment": "同意"}); code != 200 {
		t.Fatalf("审批: %d %v", code, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		_, o := dba.do("GET", "/api/changes/"+id, nil)
		state, _ = o["state"].(string)
		if state == "succeeded" || state == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state != "succeeded" {
		_, o := dba.do("GET", "/api/changes/"+id, nil)
		t.Fatalf("变更应成功: %v", o)
	}
	var ks []string
	for _, r := range e.sim.Calls() {
		ks = append(ks, r.Cmd.Kind)
	}
	if strings.Join(ks, ",") != "pd.evict-add,cluster.restart,pd.evict-remove" {
		t.Errorf("执行的命令 = %v", ks)
	}
	// 审计：链完整，包含登录、提议、审批、命令、终态；审计员能导出，DBA 不能
	if code, out := auditor.do("GET", "/api/audit/verify", nil); code != 200 || out["ok"] != true {
		t.Errorf("审计链: %v", out)
	}
	code, out = auditor.do("GET", "/api/audit?limit=500", nil)
	acts := map[string]bool{}
	for _, en := range out["entries"].([]any) {
		acts[en.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"auth.login", "authz.deny", "writeswitch.propose", "writeswitch.on", "change.created", "change.approved", "change.command", "change.succeeded"} {
		if !acts[want] {
			t.Errorf("审计缺少 %s: %v", want, acts)
		}
	}
	if code, _ := dba.do("GET", "/api/audit", nil); code != 403 {
		t.Errorf("DBA 不能看审计: %d", code)
	}
	if code, _ := dba.do("GET", "/api/audit/export", nil); code != 403 {
		t.Errorf("DBA 不能导出审计: %d", code)
	}
	if code, out := auditor.do("GET", "/api/audit/export?format=jsonl", nil); code != 200 || out["_raw"] == nil {
		t.Errorf("审计员应可导出: %d", code)
	}
	// 篡改审计文件 → 校验发现
	b, _ := os.ReadFile(filepath.Join(e.dir, "audit.log"))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	lines[2] = strings.Replace(lines[2], "\"result\":\"success\"", "\"result\":\"failed\"", 1)
	if lines[2] == strings.Split(strings.TrimRight(string(b), "\n"), "\n")[2] {
		lines[2] = lines[2] + " "
		lines[2] = strings.Replace(lines[2], "user", "usr", 1)
	}
	_ = os.WriteFile(filepath.Join(e.dir, "audit.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	if _, out := auditor.do("GET", "/api/audit/verify", nil); out["ok"] != false {
		t.Errorf("篡改后校验应失败: %v", out)
	}
}

func TestOpsKillSwitchFileAndOffSwitch(t *testing.T) {
	e := setupOps(t)
	admin, admin2, dba := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "dba")
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	body := map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "start"}}
	if code, out := dba.do("POST", "/api/changes", body); code != 200 {
		t.Fatalf("开关打开后低风险变更应可创建: %d %v", code, out)
	}
	if err := os.WriteFile(e.readonly, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := dba.do("POST", "/api/changes", body)
	if code == 200 || !strings.Contains(out["error"].(string), "只读") {
		t.Errorf("READONLY 文件存在时必须拒绝: %d %v", code, out)
	}
	_, me := dba.do("GET", "/api/auth/me", nil)
	if w := me["write"].(map[string]any); w["readOnlyForced"] != true {
		t.Errorf("me 应反映强制只读: %v", w)
	}
	os.Remove(e.readonly)
	// 单个管理员即可熔断关闭
	if code, o := admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": false}); code != 200 || o["enabled"] != false {
		t.Errorf("关闭: %d %v", code, o)
	}
	if code, _ := dba.do("POST", "/api/changes", body); code == 200 {
		t.Error("关闭后应拒绝")
	}
}

func TestOpsUserAdmin(t *testing.T) {
	e := setupOps(t)
	admin, dba := e.login(t, "admin"), e.login(t, "dba")
	if code, _ := dba.do("GET", "/api/users", nil); code != 403 {
		t.Errorf("DBA 不能管理用户: %d", code)
	}
	if code, out := admin.do("POST", "/api/users", map[string]any{"name": "newbie", "display": "新人", "password": "short"}); code != 400 {
		t.Errorf("弱口令应拒绝: %d %v", code, out)
	}
	if code, out := admin.do("POST", "/api/users", map[string]any{"name": "newbie", "display": "新人", "password": "Fresh-Pass-4567", "roles": map[string][]string{"*": {"sre"}}}); code != 200 {
		t.Fatalf("创建用户: %d %v", code, out)
	}
	c := e.client(t)
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"user": "newbie", "password": "Fresh-Pass-4567"}); code != 200 {
		t.Errorf("新用户应可登录: %d", code)
	}
	// 禁用后会话立刻失效
	if code, _ := admin.do("POST", "/api/users/newbie/disable", map[string]any{"disabled": true}); code != 200 {
		t.Fatal("禁用失败")
	}
	if code, _ := c.do("GET", "/api/snapshot", nil); code != 401 {
		t.Errorf("禁用后会话应失效: %d", code)
	}
	// 登录失败 5 次锁定
	for i := 0; i < 5; i++ {
		e.client(t).do("POST", "/api/auth/login", map[string]string{"user": "viewer", "password": "bad-password-1"})
	}
	if code, out := e.client(t).do("POST", "/api/auth/login", map[string]string{"user": "viewer", "password": authz.DemoPassword}); code != 429 {
		t.Errorf("连续失败后应锁定: %d %v", code, out)
	}
}

func TestOpsInspectTimelineSelfcheckSilence(t *testing.T) {
	e := setupOps(t)
	sre, viewer, admin, admin2 := e.login(t, "sre"), e.login(t, "viewer"), e.login(t, "admin"), e.login(t, "admin2")
	if code, _ := viewer.do("POST", "/api/inspect", map[string]any{}); code != 403 {
		t.Errorf("访客不能巡检: %d", code)
	}
	code, out := sre.do("POST", "/api/inspect", map[string]any{"os": true})
	if code != 200 || out["items"] == nil {
		t.Fatalf("巡检: %d %v", code, out)
	}
	items := out["items"].([]any)
	var hasDisk, hasHost bool
	for _, it := range items {
		m := it.(map[string]any)
		if strings.Contains(m["n"].(string), "磁盘") && m["r"] == "bad" {
			hasDisk = true
		}
		if m["g"] == "主机" {
			hasHost = true
		}
	}
	if !hasDisk || !hasHost {
		t.Errorf("巡检应发现 tikv-4 磁盘异常并包含主机检查: disk=%v host=%v", hasDisk, hasHost)
	}
	if code, out := sre.do("GET", "/api/inspect/report?format=md", nil); code != 200 || !strings.Contains(out["_raw"].(string), "巡检报告") {
		t.Errorf("报告导出: %d", code)
	}
	if code, out := viewer.do("GET", "/api/timeline", nil); code != 200 || out["events"] == nil {
		t.Errorf("时间线: %d", code)
	}
	if code, out := viewer.do("GET", "/api/ops/selfcheck", nil); code != 200 || len(out["items"].([]any)) < 5 {
		t.Errorf("自检: %d %v", code, out)
	}
	// 静默：写开关关闭时拒绝，打开后成功
	body := map[string]any{"alertname": "TiKV_disk_used_more_than_80%", "instance": "10.0.2.14:20160", "minutes": 60, "comment": "维护中"}
	if code, _ := sre.do("POST", "/api/alerts/silence", body); code != 403 {
		t.Errorf("开关关闭时不能静默: %d", code)
	}
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	if code, out := sre.do("POST", "/api/alerts/silence", body); code != 200 {
		t.Errorf("静默: %d %v", code, out)
	}
}

func TestOpsConfigScaleOutAndMaintThroughSimRunner(t *testing.T) {
	e := setupOps(t)
	admin, admin2, dba, approver, approver2 := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "dba"), e.login(t, "approver"), e.login(t, "approver2")
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	waitDone := func(id string) map[string]any {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			_, o := dba.do("GET", "/api/changes/"+id, nil)
			if st, _ := o["state"].(string); st == "succeeded" || st == "failed" {
				return o
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("变更 %s 超时", id)
		return nil
	}
	run := func(kind string, params any, approvers ...*client) map[string]any {
		code, out := dba.do("POST", "/api/changes", map[string]any{"kind": kind, "params": params})
		if code != 200 {
			t.Fatalf("创建 %s: %d %v", kind, code, out)
		}
		id := out["id"].(string)
		for _, a := range approvers {
			if code, o := a.do("POST", "/api/changes/"+id+"/approve", nil); code != 200 {
				t.Fatalf("审批 %s: %d %v", id, code, o)
			}
		}
		res := waitDone(id)
		if res["state"] != "succeeded" {
			t.Fatalf("%s 应成功: %v %v", kind, res["reason"], res["log"])
		}
		return res
	}
	// 配置变更：在线项 + 需 reload 项 + 持久化（edit-config 经 exec 层 yaml 校验）
	run("config.set", map[string]any{"node": "tikv-3", "scope": "instance", "changes": []map[string]string{{"key": "storage.block-cache.capacity", "value": "12GB"}, {"key": "raftstore.raft-max-inflight-msgs", "value": "1024"}}}, approver)
	// 扩容 TiKV
	run("scale.out", map[string]any{"role": "tikv", "hosts": []map[string]string{{"ip": "10.0.2.99", "az": "az1", "rack": "r1"}}, "cache": 16, "rate": 30}, approver)
	// 主机维护：进入 → 延长 → 紧急结束
	enter := run("maint.enter", map[string]any{"ip": "10.0.2.11", "type": "kernel", "mins": 20}, approver, approver2)
	mid, _ := enter["maintId"].(string)
	if mid == "" {
		t.Fatalf("进入维护应产生维护记录: %v", enter)
	}
	_, ml := dba.do("GET", "/api/maints", nil)
	if ms := ml["maints"].([]any); len(ms) != 1 || ms[0].(map[string]any)["state"] != "active" {
		t.Fatalf("维护应为 active: %v", ml)
	}
	run("maint.extend", map[string]any{"maintId": mid, "add": 30})
	run("maint.exit", map[string]any{"maintId": mid, "urgent": true}, approver)
	_, ml = dba.do("GET", "/api/maints", nil)
	if ms := ml["maints"].([]any); ms[0].(map[string]any)["state"] != "done" {
		t.Errorf("维护应已结束: %v", ml)
	}
	var kinds []string
	for _, r := range e.sim.Calls() {
		kinds = append(kinds, r.Cmd.Kind)
	}
	for _, want := range []string{"sql.set-config", "cluster.edit-config", "cluster.reload", "cluster.scale-out", "cluster.stop", "cluster.start"} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("执行器应收到 %s: %v", want, kinds)
		}
	}
}
