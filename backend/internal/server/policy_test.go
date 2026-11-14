// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy_test.go
// @time: 2026/10/28

package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// ---------------------------------------------------------------- 测试脚手架（可在同一数据目录上重建服务）----

type polEnv struct {
	*opsEnv
	stop func()
}

func buildFixtures(t *testing.T) (fx string, params source.CatalogParams) {
	t.Helper()
	fc := fakecluster.Start("prod-tidb-01")
	t.Cleanup(fc.Close)
	live := source.NewLive(source.LiveConfig{Endpoints: fc.Endpoints(), MySQL: source.MySQLConfig{Host: "h", User: "u"}, Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile})
	params = source.CatalogParams{Cluster: "prod-tidb-01", Home: "/home/tidb", Now: time.Now()}
	items := source.DefaultCatalog(params)
	fx = filepath.Join(t.TempDir(), "fx")
	rec := source.NewRecorder(live, fx, "prod-tidb-01", "t", fixtures.Options{MaskSQLLiterals: true, RedactSecrets: true})
	rec.RecordAll(context.Background(), items)
	if err := rec.Finish(items); err != nil {
		t.Fatal(err)
	}
	return fx, params
}

// startPolicy 在 dir 上启动一个运维模式服务（环境 staging）。同一个 dir 可以先 stop 再重新 start，用来验证重启后状态仍在。
func startPolicy(t *testing.T, fx string, params source.CatalogParams, dir string) *polEnv {
	t.Helper()
	rp, err := source.NewReplay(fx)
	if err != nil {
		t.Fatal(err)
	}
	web := t.TempDir()
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>home</html>"), 0o644)
	s := server.New(server.Config{Col: collect.New(rp, params), Mode: "replay", Web: web, SnapTTL: time.Minute, Logf: func(string, ...any) {}})
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
	pol := exec.Policy{Clusters: []string{"prod-tidb-01"}, TempDir: t.TempDir(), AlertURL: "http://127.0.0.1:1", MySQLUser: "tidbops_ops", BackupRoots: []string{"local:///backup/"}}
	sim := &exec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: opsKey}
	ro := filepath.Join(dir, "READONLY")
	if err := s.EnableOps(server.OpsConfig{Dir: dir, Env: "staging", Users: users, Audit: al, Runner: sim, Prober: &change.SimProber{Polls: 1}, Key: opsKey, ReadOnlyFile: ro, Sim: true, ExecMode: "sim", WaitScale: 0.001, TickEvery: time.Hour, NotifyWatchEvery: -1}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	var once sync.Once
	stop := func() {
		once.Do(func() {
			ts.Close()
			s.Close()
			al.Close()
		})
	}
	t.Cleanup(stop)
	return &polEnv{opsEnv: &opsEnv{ts: ts, dir: dir, readonly: ro, srv: s, sim: sim, audit: al}, stop: stop}
}

func setupPolicy(t *testing.T) *polEnv {
	fx, params := buildFixtures(t)
	return startPolicy(t, fx, params, t.TempDir())
}

// dig 按路径取嵌套 JSON 字段。
func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func propID(t *testing.T, out map[string]any) string {
	t.Helper()
	id, _ := dig(out, "proposal", "id").(string)
	if id == "" {
		t.Fatalf("响应里没有提案: %v", out)
	}
	return id
}

func auditActions(t *testing.T, c *client) map[string][]map[string]any {
	t.Helper()
	code, out := c.do("GET", "/api/audit?limit=1000", nil)
	if code != 200 {
		t.Fatalf("读取审计: %d %v", code, out)
	}
	acts := map[string][]map[string]any{}
	for _, en := range out["entries"].([]any) {
		m := en.(map[string]any)
		a := m["action"].(string)
		acts[a] = append(acts[a], m)
	}
	return acts
}

func rolesOf(t *testing.T, admin *client, name string) []string {
	t.Helper()
	_, out := admin.do("GET", "/api/users", nil)
	for _, u := range out["users"].([]any) {
		m := u.(map[string]any)
		if m["name"] == name {
			var rs []string
			if rm, ok := m["roles"].(map[string]any); ok {
				for _, v := range rm {
					for _, r := range v.([]any) {
						rs = append(rs, r.(string))
					}
				}
			}
			return rs
		}
	}
	t.Fatalf("找不到用户 %s", name)
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func previewLevels(t *testing.T, c *client) float64 {
	t.Helper()
	code, out := c.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}})
	if code != 200 {
		t.Fatalf("preview: %d %v", code, out)
	}
	return out["levels"].(float64)
}

func policyBody(mut func(levels map[string]map[string]int, mfa map[string]map[string]bool)) map[string]any {
	d := authz.DefaultPolicy()
	mut(d.Levels, d.MFA)
	return map[string]any{"policy": d}
}

// ---------------------------------------------------------------- 授予管理员角色：双人复核 ----

func TestGrantAdminRoleNeedsSecondAdmin(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2, dba, viewer, auditor := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "dba"), e.login(t, "viewer"), e.login(t, "auditor")

	// 发起：返回 202 与提案，角色暂不变化
	code, out := admin.do("POST", "/api/users/dba/roles", map[string]any{"roles": map[string][]string{"*": {"dba", "admin"}}})
	if code != 202 || dig(out, "proposal", "state") != "pending" || !strings.Contains(out["message"].(string), "需要另一名管理员确认") {
		t.Fatalf("授予 admin 应创建提案: %d %v", code, out)
	}
	if dig(out, "proposal", "kind") != "grant-admin" || dig(out, "proposal", "by") != "admin" {
		t.Errorf("提案字段: %v", out["proposal"])
	}
	id := propID(t, out)
	if rs := rolesOf(t, admin, "dba"); contains(rs, "admin") {
		t.Fatalf("确认前不应有 admin 角色: %v", rs)
	}
	// 目标用户的会话不受影响
	if code, _ := dba.do("GET", "/api/snapshot", nil); code != 200 {
		t.Errorf("提案未生效前 dba 的会话应保持: %d", code)
	}
	// 发起人不能确认自己的提案
	if code, out := admin.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 409 || !strings.Contains(out["error"].(string), "另一名管理员") {
		t.Errorf("发起人自己确认应 409: %d %v", code, out)
	}
	// 非管理员不能确认（也看不到别人的提案）
	if code, _ := dba.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 403 {
		t.Errorf("非管理员确认应 403: %d", code)
	}
	if code, _ := auditor.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 403 {
		t.Errorf("审计员确认应 403: %d", code)
	}
	if code, _ := dba.do("POST", "/api/ops/proposals/"+id+"/reject", nil); code != 403 {
		t.Errorf("非管理员拒绝应 403: %d", code)
	}
	if code, out := viewer.do("GET", "/api/ops/proposals", nil); code != 200 || len(out["pending"].([]any)) != 0 {
		t.Errorf("非管理员只能看到自己发起的提案: %d %v", code, out)
	}
	if code, out := admin2.do("GET", "/api/ops/proposals", nil); code != 200 || len(out["pending"].([]any)) != 1 || dig(out["pending"].([]any)[0].(map[string]any), "canConfirm") != true {
		t.Errorf("另一名管理员应看到可确认的 pending 提案: %d %v", code, out)
	}
	// 另一名管理员确认 → 生效
	code, out = admin2.do("POST", "/api/ops/proposals/"+id+"/confirm", nil)
	if code != 200 || dig(out, "proposal", "state") != "applied" || dig(out, "proposal", "confirmedBy") != "admin2" {
		t.Fatalf("确认: %d %v", code, out)
	}
	if rs := rolesOf(t, admin, "dba"); !contains(rs, "admin") || !contains(rs, "dba") {
		t.Errorf("确认后应同时有 dba 与 admin: %v", rs)
	}
	// 角色变更后，目标用户旧会话被吊销
	if code, _ := dba.do("GET", "/api/snapshot", nil); code != 401 {
		t.Errorf("授权后应吊销其旧会话: %d", code)
	}
	// 终态不可再变
	if code, out := admin2.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 409 || out["code"] != "state" {
		t.Errorf("重复确认应 409: %d %v", code, out)
	}
	if code, _ := admin2.do("POST", "/api/ops/proposals/"+id+"/reject", nil); code != 409 {
		t.Errorf("已生效的提案不能再拒绝: %d", code)
	}
	if code, _ := admin2.do("POST", "/api/ops/proposals/PRP-9999/confirm", nil); code != 404 {
		t.Errorf("不存在的提案应 404: %d", code)
	}
	_, hist := admin2.do("GET", "/api/ops/proposals", nil)
	if h := hist["history"].([]any); len(h) != 1 || h[0].(map[string]any)["state"] != "applied" {
		t.Errorf("历史: %v", hist["history"])
	}
	// 审计
	acts := auditActions(t, auditor)
	for _, want := range []string{"proposal.create", "proposal.confirm", "user.roles"} {
		if len(acts[want]) == 0 {
			t.Errorf("审计缺少 %s", want)
		}
	}
	var sawDenied bool
	for _, en := range acts["proposal.confirm"] {
		if en["result"] == "denied" {
			sawDenied = true
		}
	}
	if !sawDenied {
		t.Error("发起人自批应记一条 denied 的 proposal.confirm 审计")
	}
}

func TestCreateAdminUserSplitsIntoUserAndProposal(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2 := e.login(t, "admin"), e.login(t, "admin2")
	const pw = "Fresh-Pass-4567"
	code, out := admin.do("POST", "/api/users", map[string]any{"name": "boss", "display": "新管理员", "password": pw, "roles": map[string][]string{"*": {"admin"}}})
	if code != 202 || out["created"] != true || dig(out, "proposal", "state") != "pending" {
		t.Fatalf("创建带 admin 的用户: %d %v", code, out)
	}
	id := propID(t, out)
	// 提案与落盘文件里都不能有口令
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), pw) {
		t.Error("响应里不应出现口令")
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, "proposals.json"))
	if err != nil || strings.Contains(string(raw), pw) {
		t.Errorf("proposals.json 不应包含口令: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(e.dir, "proposals.json")); err != nil || fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("proposals.json 权限应为 0600: %v %v", fi, err)
	}
	// 用户已创建并可登录，但还不是管理员
	boss := e.client(t)
	code, lo := boss.do("POST", "/api/auth/login", map[string]string{"user": "boss", "password": pw})
	if code != 200 {
		t.Fatalf("新用户应可登录: %d %v", code, lo)
	}
	boss.csrf, _ = lo["csrf"].(string)
	if code, _ := boss.do("GET", "/api/users", nil); code != 403 {
		t.Errorf("确认前不应有管理员权限: %d", code)
	}
	if code, out := admin2.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 200 {
		t.Fatalf("确认: %d %v", code, out)
	}
	boss = e.login2(t, "boss", pw)
	if code, _ := boss.do("GET", "/api/users", nil); code != 200 {
		t.Errorf("确认后应有管理员权限: %d", code)
	}
	// 不含 admin 的创建照旧立即生效（200）
	if code, out := admin.do("POST", "/api/users", map[string]any{"name": "plain", "password": pw, "roles": map[string][]string{"*": {"sre"}}}); code != 200 || out["ok"] != true {
		t.Errorf("普通创建应 200: %d %v", code, out)
	}
	// 非法角色在发起前就被拒绝，不产生提案
	if code, _ := admin.do("POST", "/api/users", map[string]any{"name": "bad1", "password": pw, "roles": map[string][]string{"*": {"admin", "root"}}}); code != 400 {
		t.Errorf("未知角色应 400: %d", code)
	}
	_, lst := admin.do("GET", "/api/ops/proposals", nil)
	if n := len(lst["pending"].([]any)); n != 0 {
		t.Errorf("不应残留 pending 提案: %d", n)
	}
}

// login2 用自定义口令登录。
func (e *polEnv) login2(t *testing.T, user, pw string) *client {
	t.Helper()
	c := e.client(t)
	code, out := c.do("POST", "/api/auth/login", map[string]string{"user": user, "password": pw})
	if code != 200 {
		t.Fatalf("登录 %s: %d %v", user, code, out)
	}
	c.csrf, _ = out["csrf"].(string)
	return c
}

func TestDemoteAndOtherRoleChangesStayImmediate(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2 := e.login(t, "admin"), e.login(t, "admin2")
	// 其他角色改动照旧立即生效
	if code, out := admin.do("POST", "/api/users/viewer/roles", map[string]any{"roles": map[string][]string{"*": {"viewer", "sre"}}}); code != 200 || out["ok"] != true {
		t.Fatalf("普通角色改动: %d %v", code, out)
	}
	if rs := rolesOf(t, admin, "viewer"); !contains(rs, "sre") {
		t.Errorf("应立即生效: %v", rs)
	}
	// 已有 admin 的用户保持 admin 不算「多出」，立即生效
	if code, _ := admin.do("POST", "/api/users/admin2/roles", map[string]any{"roles": map[string][]string{"*": {"admin", "auditor"}}}); code != 200 {
		t.Errorf("保留 admin 不应走提案: %d", code)
	}
	// 降级立即生效
	if code, _ := admin.do("POST", "/api/users/admin2/roles", map[string]any{"roles": map[string][]string{"*": {"dba"}}}); code != 200 {
		t.Fatalf("降级应立即生效: %d", code)
	}
	_ = admin2
	// 最后一个管理员保护保持
	if code, out := admin.do("POST", "/api/users/admin/roles", map[string]any{"roles": map[string][]string{"*": {"viewer"}}}); code != 400 || !strings.Contains(out["error"].(string), "最后一个管理员") {
		t.Errorf("最后一个管理员不能被降级: %d %v", code, out)
	}
	// 授予 admin 的目标环境键也要走提案：为已有「*」角色的用户新增 prod 环境的 admin
	code, out := admin.do("POST", "/api/users/dba/roles", map[string]any{"roles": map[string][]string{"*": {"dba"}, "prod": {"admin"}}})
	if code != 202 {
		t.Errorf("任一环境授予 admin 都应走提案: %d %v", code, out)
	}
}

func TestProposalRejectCancelAndSupersede(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2, auditor := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "auditor")
	grant := func(c *client, user string) string {
		code, out := c.do("POST", "/api/users/"+user+"/roles", map[string]any{"roles": map[string][]string{"*": {"sre", "admin"}}})
		if code != 202 {
			t.Fatalf("发起: %d %v", code, out)
		}
		return propID(t, out)
	}
	state := func(c *client, id string) string {
		_, out := c.do("GET", "/api/ops/proposals", nil)
		for _, k := range []string{"pending", "history"} {
			for _, p := range out[k].([]any) {
				if m := p.(map[string]any); m["id"] == id {
					return m["state"].(string)
				}
			}
		}
		return "?"
	}
	// 另一名管理员拒绝
	a := grant(admin, "sre")
	if code, out := admin2.do("POST", "/api/ops/proposals/"+a+"/reject", map[string]any{"note": "不需要"}); code != 200 || dig(out, "proposal", "state") != "rejected" {
		t.Fatalf("拒绝: %d %v", code, out)
	}
	if rs := rolesOf(t, admin, "sre"); contains(rs, "admin") {
		t.Error("拒绝后不应授予")
	}
	// 发起人撤回 = cancelled
	b := grant(admin, "sre")
	if code, out := admin.do("POST", "/api/ops/proposals/"+b+"/reject", nil); code != 200 || dig(out, "proposal", "state") != "cancelled" {
		t.Fatalf("撤回: %d %v", code, out)
	}
	// 同一对象的新提案取代旧提案
	c1 := grant(admin, "sre")
	c2 := grant(admin2, "sre")
	if state(admin, c1) != "cancelled" || state(admin, c2) != "pending" {
		t.Errorf("新提案应取代旧提案: %s / %s", state(admin, c1), state(admin, c2))
	}
	// 被取代的不能再确认
	if code, _ := admin2.do("POST", "/api/ops/proposals/"+c1+"/confirm", nil); code != 409 {
		t.Errorf("已取消的提案不能确认: %d", code)
	}
	acts := auditActions(t, auditor)
	if len(acts["proposal.reject"]) != 1 || len(acts["proposal.cancel"]) != 2 {
		t.Errorf("审计 reject=%d cancel=%d（期望 1 / 2）", len(acts["proposal.reject"]), len(acts["proposal.cancel"]))
	}
}

func TestProposalExpires(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2, auditor := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "auditor")
	code, out := admin.do("POST", "/api/users/sre/roles", map[string]any{"roles": map[string][]string{"*": {"sre", "admin"}}})
	if code != 202 {
		t.Fatal(code, out)
	}
	id := propID(t, out)
	exp, _ := time.Parse(time.RFC3339, dig(out, "proposal", "expires").(string))
	at, _ := time.Parse(time.RFC3339, dig(out, "proposal", "at").(string))
	if d := exp.Sub(at); d != time.Hour {
		t.Errorf("有效期应为 1 小时: %v", d)
	}
	e.srv.AgeProposalsForTest(61 * time.Minute)
	code, out = admin2.do("POST", "/api/ops/proposals/"+id+"/confirm", nil)
	if code != 409 || !strings.Contains(out["error"].(string), "过期") {
		t.Fatalf("过期的提案不能确认: %d %v", code, out)
	}
	if rs := rolesOf(t, admin, "sre"); contains(rs, "admin") {
		t.Error("过期提案不应生效")
	}
	_, lst := admin.do("GET", "/api/ops/proposals", nil)
	if len(lst["pending"].([]any)) != 0 || lst["history"].([]any)[0].(map[string]any)["state"] != "expired" {
		t.Errorf("过期后应在历史里: %v", lst)
	}
	if acts := auditActions(t, auditor); len(acts["proposal.expire"]) != 1 {
		t.Errorf("审计应有 1 条 proposal.expire: %d", len(acts["proposal.expire"]))
	}
	// 过期后可以重新发起并确认
	code, out = admin.do("POST", "/api/users/sre/roles", map[string]any{"roles": map[string][]string{"*": {"sre", "admin"}}})
	if code != 202 {
		t.Fatal(code, out)
	}
	if code, _ := admin2.do("POST", "/api/ops/proposals/"+propID(t, out)+"/confirm", nil); code != 200 {
		t.Errorf("重新发起后应可确认: %d", code)
	}
}

func TestGrantAdminProposalStaleWhenRolesChanged(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2 := e.login(t, "admin"), e.login(t, "admin2")
	_, out := admin.do("POST", "/api/users/sre/roles", map[string]any{"roles": map[string][]string{"*": {"sre", "admin"}}})
	id := propID(t, out)
	// 发起后目标用户的角色被另一次修改改变 → 提案过时，确认被拒且提案作废
	if code, _ := admin2.do("POST", "/api/users/sre/roles", map[string]any{"roles": map[string][]string{"*": {"sre", "dba"}}}); code != 200 {
		t.Fatal("改角色失败")
	}
	code, out := admin2.do("POST", "/api/ops/proposals/"+id+"/confirm", nil)
	if code != 409 || out["code"] != "apply" || dig(out, "proposal", "state") != "rejected" {
		t.Fatalf("过时提案应作废: %d %v", code, out)
	}
	if rs := rolesOf(t, admin, "sre"); contains(rs, "admin") {
		t.Error("过时提案不应授予 admin")
	}
}

// ---------------------------------------------------------------- 可配置审批策略 ----

func TestApprovalPolicyProposalFlow(t *testing.T) {
	e := setupPolicy(t)
	admin, admin2, dba, viewer, auditor := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "dba"), e.login(t, "viewer"), e.login(t, "auditor")

	// 读取：登录即可；默认值与 A3 §2.4 一致
	code, out := viewer.do("GET", "/api/ops/policy", nil)
	if code != 200 || dig(out, "policy", "levels", "mid", "staging") != float64(1) || dig(out, "defaults", "levels", "high", "prod") != float64(2) ||
		dig(out, "policy", "mfa", "high", "prod") != true || out["usingDefault"] != true || len(out["limits"].([]any)) == 0 || out["pendingProposal"] != nil {
		t.Fatalf("GET policy: %d %v", code, out)
	}
	if code, _ := e.client(t).do("GET", "/api/ops/policy", nil); code != 401 {
		t.Errorf("未登录应 401: %d", code)
	}
	if levels := previewLevels(t, dba); levels != 1 {
		t.Fatalf("staging 中风险默认 1 级: %v", levels)
	}

	// 修改需要 settings.edit
	if code, _ := dba.do("POST", "/api/ops/policy", policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) { l["mid"]["staging"] = 2 })); code != 403 {
		t.Errorf("DBA 不能修改审批策略: %d", code)
	}
	// 硬性下限：全部 400
	floors := map[string]func(l map[string]map[string]int, m map[string]map[string]bool){
		"prod 高风险 1 级":    func(l map[string]map[string]int, m map[string]map[string]bool) { l["high"]["prod"] = 1 },
		"prod 高风险关闭 MFA":  func(l map[string]map[string]int, m map[string]map[string]bool) { m["high"]["prod"] = false },
		"prod 中风险 0 级":    func(l map[string]map[string]int, m map[string]map[string]bool) { l["mid"]["prod"] = 0 },
		"staging 高风险 1 级": func(l map[string]map[string]int, m map[string]map[string]bool) { l["high"]["staging"] = 1 },
		"级数 4":            func(l map[string]map[string]int, m map[string]map[string]bool) { l["low"]["test"] = 4 },
	}
	for name, mut := range floors {
		if code, out := admin.do("POST", "/api/ops/policy", policyBody(mut)); code != 400 || out["error"] == nil {
			t.Errorf("%s 应被拒绝: %d %v", name, code, out)
		}
	}
	if code, out := admin.do("POST", "/api/ops/policy", map[string]any{"policy": map[string]any{"levels": map[string]any{"low": map[string]any{"test": 0}}}}); code != 400 {
		t.Errorf("缺格应 400: %d %v", code, out)
	}
	if code, _ := admin.do("POST", "/api/ops/policy", map[string]any{}); code != 400 {
		t.Errorf("缺少 policy 应 400: %d", code)
	}
	if code, out := admin.do("POST", "/api/ops/policy", policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) {})); code != 400 {
		t.Errorf("与当前相同应 400: %d %v", code, out)
	}
	if _, lst := admin.do("GET", "/api/ops/proposals", nil); len(lst["pending"].([]any)) != 0 {
		t.Error("被拒绝的请求不应产生提案")
	}

	// 合法修改：202，确认前不生效
	code, out = admin.do("POST", "/api/ops/policy", policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) {
		l["mid"]["staging"] = 2
		l["low"]["test"] = 1
	}))
	if code != 202 || dig(out, "proposal", "kind") != "approval-policy" || dig(out, "proposal", "state") != "pending" || !strings.Contains(out["message"].(string), "另一名管理员") {
		t.Fatalf("发起策略提案: %d %v", code, out)
	}
	if s := dig(out, "proposal", "summary").(string); !strings.Contains(s, "预发·中风险 级数 1→2") || !strings.Contains(s, "测试·低风险 级数 0→1") {
		t.Errorf("摘要应列出差异: %s", s)
	}
	id := propID(t, out)
	if levels := previewLevels(t, dba); levels != 1 {
		t.Errorf("确认前仍应是 1 级: %v", levels)
	}
	_, g := viewer.do("GET", "/api/ops/policy", nil)
	if dig(g, "pendingProposal", "id") != id {
		t.Errorf("GET policy 应带 pendingProposal: %v", g["pendingProposal"])
	}
	// 发起人不能自己确认；非管理员不能确认
	if code, _ := admin.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 409 {
		t.Errorf("发起人自己确认应 409: %d", code)
	}
	if code, _ := dba.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 403 {
		t.Errorf("非管理员确认应 403: %d", code)
	}
	// 通过「平台设置」接口不能绕过双人复核
	admin.do("POST", "/api/ops/settings", map[string]any{"approvalPolicy": authz.DefaultPolicy(), "lb": true})
	if levels := previewLevels(t, dba); levels != 1 {
		t.Errorf("settings 接口不应能改策略: %v", levels)
	}
	// 确认后生效：预览与创建（引擎）都用新表
	if code, out := admin2.do("POST", "/api/ops/proposals/"+id+"/confirm", nil); code != 200 || dig(out, "proposal", "state") != "applied" {
		t.Fatalf("确认: %d %v", code, out)
	}
	if levels := previewLevels(t, dba); levels != 2 {
		t.Errorf("确认后预览应是 2 级: %v", levels)
	}
	_, g = viewer.do("GET", "/api/ops/policy", nil)
	if g["usingDefault"] != false || dig(g, "policy", "levels", "mid", "staging") != float64(2) || g["pendingProposal"] != nil {
		t.Errorf("GET policy 应反映生效策略: %v", g)
	}
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	code, ch := dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}, "note": "策略生效验证"})
	if code != 200 || ch["need"] != float64(2) {
		t.Errorf("引擎应按新策略要求 2 级审批: %d %v", code, ch)
	}

	// 再改回默认：由 admin2 发起、admin 确认；新提案取代 pending 的旧提案
	back := policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) {})
	_, lst := admin2.do("POST", "/api/ops/policy", policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) { l["mid"]["staging"] = 3 }))
	stale := propID(t, lst)
	code, out = admin2.do("POST", "/api/ops/policy", back)
	if code != 202 {
		t.Fatalf("改回默认: %d %v", code, out)
	}
	id2 := propID(t, out)
	_, ps := admin.do("GET", "/api/ops/proposals", nil)
	pending := ps["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["id"] != id2 {
		t.Errorf("只应剩下最新的策略提案: %v", pending)
	}
	if code, _ := admin.do("POST", "/api/ops/proposals/"+stale+"/confirm", nil); code != 409 {
		t.Errorf("被取代的提案不能确认: %d", code)
	}
	if code, _ := admin.do("POST", "/api/ops/proposals/"+id2+"/confirm", nil); code != 200 {
		t.Fatal("改回默认的确认失败")
	}
	if levels := previewLevels(t, dba); levels != 1 {
		t.Errorf("改回后预览应是 1 级: %v", levels)
	}
	_, g = viewer.do("GET", "/api/ops/policy", nil)
	if dig(g, "policy", "levels", "low", "test") != float64(0) {
		t.Errorf("改回后 low/test 应是 0: %v", g["policy"])
	}

	// 审计：proposal.create / confirm / cancel，且有被硬性下限拒绝的记录；摘要里没有口令类内容
	acts := auditActions(t, auditor)
	for _, want := range []string{"proposal.create", "proposal.confirm", "proposal.cancel", "policy.propose"} {
		if len(acts[want]) == 0 {
			t.Errorf("审计缺少 %s", want)
		}
	}
	for _, en := range acts["policy.propose"] {
		if en["result"] != "denied" {
			t.Errorf("policy.propose 只应记录被拒绝的尝试: %v", en)
		}
	}
	for _, en := range acts["proposal.create"] {
		if s := en["summary"].(string); !strings.HasPrefix(s, "[approval-policy]") && !strings.HasPrefix(s, "[grant-admin]") {
			t.Errorf("审计摘要应以提案类型开头: %s", s)
		}
	}
}

func TestPolicyAndProposalsSurviveRestart(t *testing.T) {
	fx, params := buildFixtures(t)
	dir := t.TempDir()
	e := startPolicy(t, fx, params, dir)
	admin, admin2 := e.login(t, "admin"), e.login(t, "admin2")
	// 1) 一个已生效的策略修改  2) 一个 pending 的策略提案  3) 一个 pending 的授予 admin
	_, out := admin.do("POST", "/api/ops/policy", policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) { l["mid"]["staging"] = 2 }))
	if code, _ := admin2.do("POST", "/api/ops/proposals/"+propID(t, out)+"/confirm", nil); code != 200 {
		t.Fatal("确认失败")
	}
	_, out = admin.do("POST", "/api/ops/policy", policyBody(func(l map[string]map[string]int, m map[string]map[string]bool) {
		l["mid"]["staging"] = 3
		l["low"]["prod"] = 1
	}))
	pendPolicy := propID(t, out)
	_, out = admin.do("POST", "/api/users/dba/roles", map[string]any{"roles": map[string][]string{"*": {"dba", "admin"}}})
	pendGrant := propID(t, out)
	e.stop()

	// 重建 Server，读取同一个目录
	e2 := startPolicy(t, fx, params, dir)
	dba := e2.login(t, "dba")
	if levels := previewLevels(t, dba); levels != 2 {
		t.Errorf("重启后生效的策略应保留（2 级）: %v", levels)
	}
	admin, admin2 = e2.login(t, "admin"), e2.login(t, "admin2")
	_, g := admin2.do("GET", "/api/ops/policy", nil)
	if dig(g, "policy", "levels", "mid", "staging") != float64(2) || dig(g, "pendingProposal", "id") != pendPolicy {
		t.Errorf("重启后 pending 的策略提案应保留: %v", g)
	}
	_, ps := admin2.do("GET", "/api/ops/proposals", nil)
	if n := len(ps["pending"].([]any)); n != 2 {
		t.Errorf("重启后应有 2 条 pending 提案: %v", ps["pending"])
	}
	// 编号接着往下排，不重复
	_, out = admin.do("POST", "/api/users/sre/roles", map[string]any{"roles": map[string][]string{"*": {"sre", "admin"}}})
	if id := propID(t, out); id == pendPolicy || id == pendGrant || id <= pendGrant {
		t.Errorf("重启后提案编号应继续递增: %s（已有 %s / %s）", id, pendPolicy, pendGrant)
	}
	// 重启前发起的提案可以确认并生效
	if code, _ := admin2.do("POST", "/api/ops/proposals/"+pendPolicy+"/confirm", nil); code != 200 {
		t.Fatal("重启后确认失败")
	}
	if levels := previewLevels(t, e2.login(t, "dba")); levels != 3 {
		t.Errorf("确认后应是 3 级: %v", levels)
	}
	if code, _ := admin2.do("POST", "/api/ops/proposals/"+pendGrant+"/confirm", nil); code != 200 {
		t.Fatal("授予 admin 的提案确认失败")
	}
	if rs := rolesOf(t, admin, "dba"); !contains(rs, "admin") {
		t.Errorf("授予应生效: %v", rs)
	}
	// proposals.json 权限
	if fi, err := os.Stat(filepath.Join(dir, "proposals.json")); err != nil {
		t.Error(err)
	} else if os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Errorf("proposals.json 权限 %v", fi.Mode().Perm())
	}
}

func TestInvalidPersistedPolicyFallsBackToDefault(t *testing.T) {
	fx, params := buildFixtures(t)
	dir := t.TempDir()
	// 被手工改坏的 settings.json：prod 高风险 0 级（违反硬性下限）
	bad := authz.DefaultPolicy()
	bad.Levels["high"]["prod"] = 0
	b, _ := json.Marshal(map[string]any{"writeEnabled": false, "freezes": []any{}, "overrides": map[string]string{}, "approvalPolicy": bad})
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e := startPolicy(t, fx, params, dir)
	_, g := e.login(t, "viewer").do("GET", "/api/ops/policy", nil)
	if g["usingDefault"] != true || dig(g, "policy", "levels", "high", "prod") != float64(2) {
		t.Errorf("不合法的落盘策略应退回默认: %v", g["policy"])
	}
	if acts := auditActions(t, e.login(t, "auditor")); len(acts["policy.load"]) != 1 {
		t.Errorf("应记一条 policy.load 审计: %v", acts["policy.load"])
	}
}
