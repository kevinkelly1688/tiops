// @author: kevinkelly
// @license: Apache License 2.0
// @file: shell_api_test.go
// @time: 2026/10/28

package server_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/collect"
	"tidbops/internal/server"
	"tidbops/internal/shell"
	"tidbops/internal/source"
)

// shAgent 模拟子进程通道（见 internal/shell 的 fakeAgent）。
type shAgent struct {
	mu    sync.Mutex
	calls []shell.ChildReq
	lines []string
	block chan struct{}
}

func (a *shAgent) Call(ctx context.Context, req shell.ChildReq, emit func(string)) (shell.ChildRes, error) {
	a.mu.Lock()
	a.calls = append(a.calls, req)
	lines, block := a.lines, a.block
	a.mu.Unlock()
	switch req.Mode {
	case shell.ModeProbe:
		return shell.ChildRes{Sudo: true, Home: "/home/tidb", User: "tidb"}, nil
	case shell.ModeRun:
		for _, l := range lines {
			emit(l)
		}
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				return shell.ChildRes{Exit: 143}, nil
			}
		}
		return shell.ChildRes{}, nil
	}
	return shell.ChildRes{Out: "Running as unit"}, nil
}

func (a *shAgent) count(mode string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if c.Mode == mode {
			n++
		}
	}
	return n
}

type shEnv struct {
	dir  string
	shd  string
	ag   *shAgent
	m    *shell.Manager
	env  *opsEnv
	logf string
}

func setupShell(t *testing.T, withManager bool) *shEnv {
	t.Helper()
	web := t.TempDir()
	_ = os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>home</html>"), 0o644)
	params := source.CatalogParams{Cluster: "platform", Home: "/home/tidb", Now: time.Now()}
	s := server.New(server.Config{Col: collect.New(nil, params), Mode: "replay", Web: web, SnapTTL: time.Minute, Logf: func(string, ...any) {}})
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
	e := &shEnv{dir: dir, shd: filepath.Join(dir, "shell"), ag: &shAgent{}, logf: filepath.Join(dir, "audit.log")}
	cfg := server.OpsConfig{Dir: dir, Env: "staging", Users: users, Audit: al, Key: opsKey, ReadOnlyFile: filepath.Join(dir, "READONLY"), ExecMode: "none", TickEvery: time.Hour, NotifyWatchEvery: -1}
	if withManager {
		m, err := shell.NewManager(shell.Config{Dir: e.shd, Agent: e.ag, NoPathCheck: true, LookupUser: func(n string) (string, bool) {
			if n == "tidb" {
				return "/home/tidb", true
			}
			return "", false
		}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Shell = m
		e.m = m
	}
	if err := s.EnableOps(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	e.env = &opsEnv{ts: ts, dir: dir, srv: s}
	return e
}

func (e *shEnv) finish(t *testing.T, id, rc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.shd, "jobs", id, "exit"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *shEnv) wait(t *testing.T, id string) {
	t.Helper()
	if !e.m.Wait(id, 5*time.Second) {
		t.Fatalf("任务 %s 没有结束", id)
	}
}

func TestShellRoutesAbsentWithoutManager(t *testing.T) {
	e := setupShell(t, false)
	if code, _ := e.env.login(t, "admin").do("GET", "/api/shell/status", nil); code != 404 {
		t.Errorf("非中控机模式不应注册: %d", code)
	}
}

func TestShellPermissions(t *testing.T) {
	e := setupShell(t, true)
	if code, _ := e.env.client(t).do("GET", "/api/shell/status", nil); code != 401 {
		t.Errorf("未登录: %d", code)
	}
	for _, u := range []string{"viewer", "dba", "sre", "dev"} {
		c := e.env.login(t, u)
		if code, out := c.do("GET", "/api/shell/status", nil); code != 200 || out["available"] != false {
			t.Errorf("%s 看不到入口: %d %v", u, code, out)
		}
		if code, _ := c.do("POST", "/api/shell/run", map[string]any{"commands": []string{"df"}}); code != 403 {
			t.Errorf("%s 不能执行: %d", u, code)
		}
		if code, _ := c.do("GET", "/api/shell/jobs", nil); code != 403 {
			t.Errorf("%s 不能看历史: %d", u, code)
		}
	}
	// 审计员：能看历史与输出，不能执行 / 申请 / 批准 / 改设置
	au := e.env.login(t, "auditor")
	if code, out := au.do("GET", "/api/shell/status", nil); code != 200 || out["available"] != true || out["canRun"] != false {
		t.Errorf("auditor 状态: %d %v", code, out)
	}
	if code, _ := au.do("GET", "/api/shell/jobs", nil); code != 200 {
		t.Errorf("auditor 看历史: %d", code)
	}
	for _, p := range []string{"/api/shell/run", "/api/shell/check", "/api/shell/requests", "/api/shell/settings"} {
		if code, _ := au.do("POST", p, map[string]any{"commands": []string{"df"}}); code != 403 {
			t.Errorf("auditor 不能 POST %s: %d", p, code)
		}
	}
	c := e.env.login(t, "admin")
	c.csrf = ""
	if code, _ := c.do("POST", "/api/shell/check", map[string]any{"commands": []string{"df"}}); code != 403 {
		t.Errorf("缺 CSRF: %d", code)
	}
}

func TestShellCheckAndDefaultCwd(t *testing.T) {
	e := setupShell(t, true)
	c := e.env.login(t, "admin")
	code, out := c.do("GET", "/api/shell/status", nil)
	if code != 200 || out["available"] != true || out["canRun"] != true || out["me"] != "admin" {
		t.Fatalf("状态: %d %v", code, out)
	}
	info := out["info"].(map[string]any)
	if info["identity"] != "tidb" || info["home"] != "/home/tidb" || info["agent"] != true || info["sudo"] != true {
		t.Errorf("中控机信息应含 tidb 家目录、通道与 sudo 探测: %v", info)
	}
	code, out = c.do("POST", "/api/shell/check", map[string]any{"commands": []string{"df -h", "systemctl restart chronyd"}})
	v := out["verdict"].(map[string]any)
	if code != 200 || v["action"] != "approve" || v["level"] != "mut" || v["route"] != "unit" || len(v["items"].([]any)) != 2 || out["request"].(map[string]any)["cwd"] != "/home/tidb" {
		t.Errorf("check: %d %v", code, out)
	}
	code, out = c.do("POST", "/api/shell/check", map[string]any{"commands": []string{"sudo journalctl -n 5"}})
	if v := out["verdict"].(map[string]any); code != 200 || v["action"] != "approve" || v["route"] != "unit" || v["sudo"] != true {
		t.Errorf("带 sudo 的命令一律按变更（要审批、走临时单元）: %d %v", code, out)
	}
	if code, out = c.do("POST", "/api/shell/check", map[string]any{"cwd": "/a b", "commands": []string{"df"}}); code != 400 || out["code"] != "invalid" {
		t.Errorf("目录非法: %d %v", code, out)
	}
	if code, out = c.do("POST", "/api/shell/check", map[string]any{"identity": "root", "commands": []string{"df"}}); code != 400 || !strings.Contains(out["error"].(string), "sudo") {
		t.Errorf("root 身份已取消: %d %v", code, out)
	}
	if e.ag.count(shell.ModeRun)+e.ag.count(shell.ModeLaunch) != 0 {
		t.Error("check 不能执行")
	}
}

func TestShellRunDeniedApprovalAndNoPassword(t *testing.T) {
	e := setupShell(t, true)
	a, a2 := e.env.login(t, "admin"), e.env.login(t, "admin2")
	body := func(cmds ...string) map[string]any { return map[string]any{"commands": cmds} }
	// 禁止：403 + 逐条原因，不调用通道
	code, out := a.do("POST", "/api/shell/run", body("cat ~/.ssh/id_rsa"))
	if code != 403 || out["code"] != "denied" || !strings.Contains(out["error"].(string), "密钥") {
		t.Fatalf("被拒绝: %d %v", code, out)
	}
	if e.ag.count(shell.ModeRun)+e.ag.count(shell.ModeLaunch) != 0 {
		t.Error("被拒绝的命令不能调用通道")
	}
	// 变更未申请
	if code, out = a.do("POST", "/api/shell/run", body("systemctl restart chronyd")); code != 409 || out["code"] != "need_approval" {
		t.Errorf("变更需申请: %d %v", code, out)
	}
	// 只读：不需要密码，直接在平台进程内执行
	e.ag.lines = []string{"$ df -h", "login phone=13800138000 password=Abcd1234"}
	code, out = a.do("POST", "/api/shell/run", body("df -h", "uptime"))
	if code != 200 {
		t.Fatalf("只读执行: %d %v", code, out)
	}
	j := out["job"].(map[string]any)
	id := j["id"].(string)
	if j["level"] != "ro" || j["route"] != "inline" || j["identity"] != "tidb" || j["cwd"] != "/home/tidb" {
		t.Errorf("任务: %v", j)
	}
	e.wait(t, id)
	_, out = a.do("GET", "/api/shell/jobs/"+id, nil)
	logText := out["log"].(string)
	if strings.Contains(logText, "13800138000") || strings.Contains(logText, "Abcd1234") || out["masks"].(float64) < 2 || out["job"].(map[string]any)["status"] != "ok" {
		t.Errorf("输出应已遮蔽、任务应已结束: %v", out)
	}
	// 变更：申请 → 自批被拒 → admin2 批准 → 执行（经临时单元，不需要密码）→ 一次性
	mut := body("sudo systemctl restart chronyd")
	mut["reason"] = "时间同步偏差大"
	code, out = a.do("POST", "/api/shell/requests", mut)
	if code != 200 {
		t.Fatalf("申请: %d %v", code, out)
	}
	qid := out["request"].(map[string]any)["id"].(string)
	if code, out = a.do("POST", "/api/shell/requests/"+qid+"/approve", nil); code != 403 || out["code"] != "self_approve" {
		t.Errorf("自批: %d %v", code, out)
	}
	if code, _ = e.env.login(t, "sre").do("POST", "/api/shell/requests/"+qid+"/approve", nil); code != 403 {
		t.Errorf("sre 不能批准: %d", code)
	}
	if code, out = a2.do("POST", "/api/shell/requests/"+qid+"/approve", nil); code != 200 {
		t.Fatalf("批准: %d %v", code, out)
	}
	mut["request"] = qid
	code, out = a.do("POST", "/api/shell/run", mut)
	if code != 200 || out["job"].(map[string]any)["approver"] != "admin2" || out["job"].(map[string]any)["route"] != "unit" {
		t.Fatalf("批准后执行: %d %v", code, out)
	}
	mid := out["job"].(map[string]any)["id"].(string)
	if e.ag.count(shell.ModeLaunch) != 1 {
		t.Errorf("变更应起临时单元: %+v", e.ag.calls)
	}
	e.finish(t, mid, "0")
	if code, out = a.do("POST", "/api/shell/run", mut); code != 409 || out["code"] != "not_ready" {
		t.Errorf("批准是一次性的: %d %v", code, out)
	}
	// 取消运行中的进程内任务：不需要密码
	e.ag.block = make(chan struct{})
	code, out = a.do("POST", "/api/shell/run", body("uptime"))
	if code != 200 {
		t.Fatalf("长命令: %d %v", code, out)
	}
	rid := out["job"].(map[string]any)["id"].(string)
	if code, out = a.do("POST", "/api/shell/jobs/"+rid+"/cancel", nil); code != 200 || out["job"].(map[string]any)["cancelled"] != true {
		t.Errorf("取消: %d %v", code, out)
	}
	// 看别人的输出：admin2 有 audit.view（管理员角色），sre 没有任何权限；admin2 查看要记 output.view 审计
	if code, _ = a2.do("GET", "/api/shell/jobs/"+id, nil); code != 200 {
		t.Errorf("admin2 看 admin 的输出: %d", code)
	}
	if code, _ = a2.do("GET", "/api/shell/jobs/"+id+"/download", nil); code != 200 {
		t.Errorf("下载: %d", code)
	}
	// 审计 + 泄漏检查（变更命令的原文只能在 cmds.txt 里，不能在审计里）
	raw, _ := os.ReadFile(e.logf)
	for _, want := range []string{"shell.run", "shell.request", "shell.approve", "shell.cancel", "shell.output.view", "shell.output.download", "平台进程内", "临时单元"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("审计缺少 %s", want)
		}
	}
	_ = filepath.Walk(e.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(p) == "out.log" { // 本测试让假通道往 out.log 里写了未遮蔽的样例；读取时的遮蔽已在上面断言
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, secret := range []string{"Abcd1234", "13800138000"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("敏感内容 %q 泄漏到 %s", secret, p)
			}
		}
		return nil
	})
}

func TestShellSettingsAndHistory(t *testing.T) {
	e := setupShell(t, true)
	a := e.env.login(t, "admin")
	_, out := a.do("GET", "/api/shell/settings", nil)
	if s := out["settings"].(map[string]any); s["policy"] != "std" || s["retainDays"].(float64) != 30 || s["roViaUnit"] != false {
		t.Errorf("默认设置: %v", s)
	}
	if code, out := a.do("POST", "/api/shell/settings", map[string]any{"policy": "strict", "retainDays": 7, "summaryOnly": true, "readRoots": []string{"/data/tidb-deploy"}, "roViaUnit": true}); code != 200 {
		t.Fatalf("保存: %d %v", code, out)
	}
	if code, out := a.do("POST", "/api/shell/settings", map[string]any{"policy": "std", "retainDays": 7, "readRoots": []string{"/etc"}}); code != 400 || out["code"] != "invalid" {
		t.Errorf("敏感目录: %d %v", code, out)
	}
	if code, out := a.do("POST", "/api/shell/run", map[string]any{"commands": []string{"systemctl restart x"}}); code != 403 || out["code"] != "denied" {
		t.Errorf("严格档位拒绝变更: %d %v", code, out)
	}
	code, out := a.do("POST", "/api/shell/run", map[string]any{"commands": []string{"tail -n 5 /data/tidb-deploy/tidb-4000/log/tidb.log"}})
	if code != 200 || out["job"].(map[string]any)["route"] != "unit" {
		t.Errorf("追加的可读目录应生效、只读走临时单元的开关应生效: %d %v", code, out)
	}
	if _, out := a.do("GET", "/api/shell/jobs", nil); len(out["jobs"].([]any)) != 1 {
		t.Errorf("历史: %v", out)
	}
	raw, _ := os.ReadFile(e.logf)
	if !strings.Contains(string(raw), "shell.settings") || !strings.Contains(string(raw), "只读命令走临时单元 true") {
		t.Error("改设置应记审计")
	}
}

// 「中控机命令」的权限是独立的两项能力（shell.run 执行 / 申请 / 取消，shell.approve 批准），可通过「角色权限」授予自定义角色、再分配给用户；
// 内置管理员角色默认两项都有；改策略设置仍要 settings.edit（仅管理员）。
func TestShellCapabilitiesGrantedViaRoles(t *testing.T) {
	e := setupShell(t, true)
	adm := e.env.login(t, "admin")
	mkRole(t, adm, "shellrunner", map[string]string{"view.overview": "allow", "shell.run": "allow"})
	mkRole(t, adm, "shellapprover", map[string]string{"view.overview": "allow", "shell.approve": "allow"})
	mkUser(t, adm, "runner1", "shellrunner")
	mkUser(t, adm, "judge1", "shellapprover")
	run, judge := e.env.login(t, "runner1"), e.env.login(t, "judge1")

	// 状态：能力决定页面上出现什么
	code, out := run.do("GET", "/api/shell/status", nil)
	if code != 200 || out["available"] != true || out["canRun"] != true || out["canApprove"] != false || out["canSettings"] != false {
		t.Fatalf("runner 状态: %d %v", code, out)
	}
	code, out = judge.do("GET", "/api/shell/status", nil)
	if code != 200 || out["available"] != true || out["canRun"] != false || out["canApprove"] != true || out["canSettings"] != false {
		t.Fatalf("approver 状态: %d %v", code, out)
	}
	code, out = adm.do("GET", "/api/shell/status", nil)
	if code != 200 || out["canRun"] != true || out["canApprove"] != true || out["canSettings"] != true {
		t.Fatalf("内置管理员应同时拥有执行、批准、改设置: %d %v", code, out)
	}

	// 执行：只读直接跑（不需要密码），变更需要申请
	if code, out = run.do("POST", "/api/shell/check", map[string]any{"commands": []string{"df -h"}}); code != 200 {
		t.Errorf("runner check: %d %v", code, out)
	}
	code, out = run.do("POST", "/api/shell/run", map[string]any{"commands": []string{"df -h"}})
	if code != 200 {
		t.Fatalf("runner 只读执行: %d %v", code, out)
	}
	e.wait(t, out["job"].(map[string]any)["id"].(string))
	mut := map[string]any{"commands": []string{"systemctl restart chronyd"}, "reason": "授权测试"}
	code, out = run.do("POST", "/api/shell/requests", mut)
	if code != 200 {
		t.Fatalf("runner 提交申请: %d %v", code, out)
	}
	qid := out["request"].(map[string]any)["id"].(string)

	// 不持有 shell.approve 不能批准（自己的也不行）；不持有 settings.edit 不能改策略
	if code, _ = run.do("POST", "/api/shell/requests/"+qid+"/approve", nil); code != 403 {
		t.Errorf("runner 没有 shell.approve，不能批准: %d", code)
	}
	if code, _ = run.do("POST", "/api/shell/settings", map[string]any{"policy": "loose", "retainDays": 30}); code != 403 {
		t.Errorf("runner 不能改策略设置: %d", code)
	}
	if code, _ = judge.do("POST", "/api/shell/settings", map[string]any{"policy": "loose", "retainDays": 30}); code != 403 {
		t.Errorf("approver 不能改策略设置: %d", code)
	}

	// approver：不能执行 / 申请 / 取消，可以看申请与历史并批准
	for _, p := range []string{"/api/shell/run", "/api/shell/check", "/api/shell/requests"} {
		if code, _ = judge.do("POST", p, map[string]any{"commands": []string{"df"}, "reason": "x"}); code != 403 {
			t.Errorf("approver 不能 POST %s: %d", p, code)
		}
	}
	if code, out = judge.do("GET", "/api/shell/requests", nil); code != 200 || len(out["requests"].([]any)) != 1 {
		t.Errorf("approver 看申请: %d %v", code, out)
	}
	if code, _ = judge.do("GET", "/api/shell/jobs", nil); code != 200 {
		t.Errorf("approver 看历史: %d", code)
	}
	if code, out = judge.do("POST", "/api/shell/requests/"+qid+"/approve", nil); code != 200 {
		t.Fatalf("approver 批准: %d %v", code, out)
	}
	mut["request"] = qid
	if code, out = run.do("POST", "/api/shell/run", mut); code != 200 || out["job"].(map[string]any)["approver"] != "judge1" {
		t.Fatalf("批准后 runner 执行: %d %v", code, out)
	}

	// 没有这两项能力的内置角色（viewer / sre / dba）仍然什么都做不了
	for _, u := range []string{"viewer", "sre", "dba"} {
		c := e.env.login(t, u)
		if code, out = c.do("GET", "/api/shell/status", nil); code != 200 || out["available"] != false {
			t.Errorf("%s 不应看到入口: %d %v", u, code, out)
		}
	}
	// 非管理员不能创建 / 修改角色（users.manage 仍是仅管理员能力）
	if code, _ = run.do("POST", "/api/roles", map[string]any{"id": "x1", "name": "x", "desc": "x", "caps": map[string]string{"view.overview": "allow", "shell.run": "allow"}}); code != 403 {
		t.Errorf("runner 不能创建角色: %d", code)
	}
	// 审计里记了谁批准谁执行
	raw, _ := os.ReadFile(e.logf)
	for _, want := range []string{"runner1", "judge1", "shell.approve"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("审计缺少 %s", want)
		}
	}
}
