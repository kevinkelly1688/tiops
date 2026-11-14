// @author: kevinkelly
// @license: Apache License 2.0
// @file: notify_test.go
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
	"tidbops/internal/model"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

// setupNotifyOps 与 setupOps 相同，但允许调整 OpsConfig（通知测试需要放行假 Webhook 主机等）。
func setupNotifyOps(t *testing.T, mod func(*server.OpsConfig)) *opsEnv {
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
	s := server.New(server.Config{Col: collect.New(rp, params), Mode: "replay", Web: web, SnapTTL: time.Minute, Logf: func(string, ...any) {}})

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
	ro := filepath.Join(dir, "READONLY")
	cfg := server.OpsConfig{Dir: dir, Env: "staging", Users: users, Audit: al, Runner: sim, Prober: &change.SimProber{Polls: 1}, Key: opsKey, ReadOnlyFile: ro, Sim: true, ExecMode: "sim", WaitScale: 0.001, TickEvery: time.Hour}
	if mod != nil {
		mod(&cfg)
	}
	if err := s.EnableOps(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &opsEnv{ts: ts, dir: dir, readonly: ro, srv: s, sim: sim, audit: al}
}

// hook 是假的 Webhook 服务：记录收到的消息正文，可切换为返回失败。
type hook struct {
	srv  *httptest.Server
	mu   sync.Mutex
	msgs []string
	fail bool
}

const hookKey = "WEBHOOKKEY-0123456789-wxyz"

func newHook(t *testing.T) *hook {
	h := &hook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Markdown struct{ Content string }
		}
		_ = json.Unmarshal(b, &body)
		h.mu.Lock()
		h.msgs = append(h.msgs, body.Markdown.Content)
		fail := h.fail
		h.mu.Unlock()
		if fail {
			w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook url"}`))
			return
		}
		w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) url() string { return h.srv.URL + "/cgi-bin/webhook/send?key=" + hookKey }

func (h *hook) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}

// waitFor 等到收到包含 sub 的消息，返回该消息。
func (h *hook) waitFor(t *testing.T, sub string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range h.all() {
			if strings.Contains(m, sub) {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("没有收到包含 %q 的消息，已收到: %q", sub, h.all())
	return ""
}

func notifyOps(t *testing.T) (*opsEnv, *hook) {
	t.Helper()
	e := setupNotifyOps(t, func(c *server.OpsConfig) {
		c.MasterKey = []byte("notify-master-key")
		c.PublicURL = "https://ops.example.com/"
		c.NotifyHosts = []string{"127.0.0.1"}
		c.NotifyInsecureTest = true
		c.NotifyWatchEvery = -1
	})
	return e, newHook(t)
}

func addWecom(t *testing.T, c *client, h *hook, events ...string) map[string]any {
	t.Helper()
	code, out := c.do("POST", "/api/ops/notify/channels", map[string]any{"type": "wecom", "name": "运维群", "url": h.url(), "events": events})
	if code != 200 {
		t.Fatalf("创建渠道: %d %v", code, out)
	}
	return out
}

func TestNotifyRequiresSettingsCap(t *testing.T) {
	e, h := notifyOps(t)
	anon := e.client(t)
	if code, _ := anon.do("GET", "/api/ops/notify", nil); code != 401 {
		t.Errorf("未登录应 401: %d", code)
	}
	admin := e.login(t, "admin")
	ch := addWecom(t, admin, h)
	id := ch["id"].(string)
	for _, u := range []string{"dba", "sre", "approver", "auditor"} {
		c := e.login(t, u)
		if code, _ := c.do("GET", "/api/ops/notify", nil); code != 403 {
			t.Errorf("%s 查看通知渠道应 403: %d", u, code)
		}
		for _, p := range []string{"/api/ops/notify/channels", "/api/ops/notify/channels/" + id, "/api/ops/notify/channels/" + id + "/delete", "/api/ops/notify/channels/" + id + "/test", "/api/ops/notify/channels/" + id + "/enabled"} {
			if code, _ := c.do("POST", p, map[string]any{"type": "wecom", "name": "x", "url": h.url()}); code != 403 {
				t.Errorf("%s POST %s 应 403: %d", u, p, code)
			}
		}
	}
	if n := len(h.all()); n != 0 {
		t.Errorf("无权限的请求不应触发发送: %d", n)
	}
}

func TestNotifyChannelLifecycle(t *testing.T) {
	e, h := notifyOps(t)
	admin, auditor := e.login(t, "admin"), e.login(t, "auditor")

	// 创建：返回脱敏的 URL，不含令牌
	ch := addWecom(t, admin, h, "change.failed", "change.succeeded")
	id := ch["id"].(string)
	if u, _ := ch["url"].(string); !strings.HasSuffix(u, "…key=****wxyz") || strings.Contains(u, "WEBHOOKKEY") {
		t.Errorf("创建返回的 url 应脱敏: %v", ch["url"])
	}
	if ch["hasSecret"] != false || ch["enabled"] != true {
		t.Errorf("创建结果: %v", ch)
	}
	// 钉钉 + 加签：只返回 hasSecret
	code, out := admin.do("POST", "/api/ops/notify/channels", map[string]any{"type": "dingtalk", "name": "钉钉群", "url": h.srv.URL + "/robot/send?access_token=DINGTOKEN-abcdefgh-1234", "secret": "SEC-super-secret-xyz"})
	if code != 200 || out["hasSecret"] != true || out["secret"] != nil {
		t.Fatalf("钉钉渠道: %d %v", code, out)
	}
	dingID := out["id"].(string)

	// 列表：脱敏、元数据齐全
	code, list := admin.do("GET", "/api/ops/notify", nil)
	if code != 200 {
		t.Fatalf("列表: %d", code)
	}
	raw, _ := json.Marshal(list)
	for _, leak := range []string{"WEBHOOKKEY-0123", "DINGTOKEN-abcdefgh", "SEC-super-secret-xyz"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("列表泄露 %q: %s", leak, raw)
		}
	}
	if chs, _ := list["channels"].([]any); len(chs) != 2 {
		t.Errorf("channels = %v", list["channels"])
	}
	if ty, _ := list["types"].([]any); len(ty) != 6 {
		t.Errorf("types = %v", list["types"])
	}
	if ev, _ := list["events"].([]any); len(ev) != 12 {
		t.Errorf("events = %v", list["events"])
	}
	if hs, _ := list["allowedHosts"].([]any); len(hs) < 5 {
		t.Errorf("allowedHosts = %v", list["allowedHosts"])
	}
	if lg, ok := list["log"].([]any); !ok || len(lg) != 0 {
		t.Errorf("log 应为空数组: %v", list["log"])
	}

	// 落盘：加密，文件里没有明文
	disk, err := os.ReadFile(filepath.Join(e.dir, "notify.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"WEBHOOKKEY-0123", "DINGTOKEN-abcdefgh", "SEC-super-secret-xyz", "127.0.0.1"} {
		if strings.Contains(string(disk), leak) {
			t.Errorf("notify.json 含明文 %q", leak)
		}
	}

	// 测试按钮：真的打到假服务器，并返回渠道的结果
	code, out = admin.do("POST", "/api/ops/notify/channels/"+id+"/test", nil)
	if code != 200 || out["ok"] != true {
		t.Fatalf("test: %d %v", code, out)
	}
	m := h.waitFor(t, "通知渠道测试")
	if !strings.Contains(m, "环境：staging") || !strings.Contains(m, "prod-tidb-01") {
		t.Errorf("测试消息应带环境与集群: %s", m)
	}
	h.mu.Lock()
	h.fail = true
	h.mu.Unlock()
	code, out = admin.do("POST", "/api/ops/notify/channels/"+id+"/test", nil)
	if code != 200 || out["ok"] != false || !strings.Contains(out["error"].(string), "93000") || strings.Contains(out["error"].(string), "WEBHOOKKEY") {
		t.Errorf("渠道失败应如实返回: %d %v", code, out)
	}
	h.mu.Lock()
	h.fail = false
	h.mu.Unlock()
	if code, _ := admin.do("POST", "/api/ops/notify/channels/ch-nope/test", nil); code != 404 {
		t.Errorf("不存在的渠道: %d", code)
	}

	// 更新：URL / secret 留空表示不修改；停用后不再收到消息
	code, out = admin.do("POST", "/api/ops/notify/channels/"+id, map[string]any{"name": "运维群-改", "events": []string{"change.failed"}})
	if code != 200 || out["name"] != "运维群-改" || !strings.HasSuffix(out["url"].(string), "key=****wxyz") {
		t.Fatalf("更新: %d %v", code, out)
	}
	if code, _ := admin.do("POST", "/api/ops/notify/channels/"+id, map[string]any{"name": "运维群-改", "url": h.url()}); code != 200 {
		t.Errorf("重新填写 URL: %d", code)
	}
	if code, out = admin.do("POST", "/api/ops/notify/channels/"+id+"/test", nil); code != 200 || out["ok"] != true {
		t.Errorf("更新后仍可发送: %v", out)
	}
	if code, out = admin.do("POST", "/api/ops/notify/channels/"+id+"/enabled", map[string]any{"enabled": false}); code != 200 || out["enabled"] != false {
		t.Errorf("停用: %d %v", code, out)
	}

	// 校验：非官方域名 / 类型与主机不匹配 / 非法类型
	for name, body := range map[string]map[string]any{
		"类型不匹配": {"type": "dingtalk", "name": "x", "url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123456"},
		"未知主机":  {"type": "wecom", "name": "x", "url": "https://evil.example.com/cgi-bin/webhook/send?key=abcdef123456"},
		"类型非法":  {"type": "teams", "name": "x", "url": h.url()},
		"名称为空":  {"type": "wecom", "name": " ", "url": h.url()},
	} {
		if code, out := admin.do("POST", "/api/ops/notify/channels", body); code != 400 {
			t.Errorf("%s 应 400: %d %v", name, code, out)
		}
	}

	// 删除
	if code, out = admin.do("POST", "/api/ops/notify/channels/"+dingID+"/delete", nil); code != 200 || out["ok"] != true {
		t.Errorf("删除: %d %v", code, out)
	}
	if code, _ = admin.do("POST", "/api/ops/notify/channels/"+dingID+"/delete", nil); code != 404 {
		t.Errorf("重复删除: %d", code)
	}

	// 审计：有记录，且摘要里没有 URL / secret
	_, au := auditor.do("GET", "/api/audit?limit=500", nil)
	acts := map[string]bool{}
	for _, en := range au["entries"].([]any) {
		acts[en.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"notify.channel.add", "notify.channel.update", "notify.channel.test", "notify.channel.delete"} {
		if !acts[want] {
			t.Errorf("审计缺少 %s: %v", want, acts)
		}
	}
	al, _ := os.ReadFile(filepath.Join(e.dir, "audit.log"))
	for _, leak := range []string{"WEBHOOKKEY", "DINGTOKEN", "SEC-super-secret", "/cgi-bin/webhook", h.srv.URL} {
		if strings.Contains(string(al), leak) {
			t.Errorf("审计日志含敏感内容 %q", leak)
		}
	}
	if !strings.Contains(string(al), "运维群") {
		t.Error("审计摘要应包含渠道名")
	}

	// 投递记录（测试发送）可见，且不含 URL
	_, list = admin.do("GET", "/api/ops/notify", nil)
	lg, _ := list["log"].([]any)
	if len(lg) < 3 {
		t.Errorf("应有测试投递记录: %v", lg)
	}
	raw, _ = json.Marshal(lg)
	if strings.Contains(string(raw), "WEBHOOKKEY") || strings.Contains(string(raw), h.srv.URL) {
		t.Errorf("投递记录泄露: %s", raw)
	}
}

func TestNotifyChangeEvents(t *testing.T) {
	e, h := notifyOps(t)
	dba, approver, admin, admin2 := e.login(t, "dba"), e.login(t, "approver"), e.login(t, "admin"), e.login(t, "admin2")
	addWecom(t, admin, h) // 订阅全部
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})

	code, out := dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}, "note": "测试重启"})
	if code != 200 || out["state"] != "pending" {
		t.Fatalf("创建: %d %v", code, out)
	}
	id := out["id"].(string)

	pending := h.waitFor(t, "有变更等待审批")
	for _, want := range []string{id, "申请人：dba", "风险：中", "环境：staging", "集群：prod-tidb-01", "[查看详情](https://ops.example.com/#schedule)", "需要审批：1 级"} {
		if !strings.Contains(pending, want) {
			t.Errorf("change.pending 缺少 %q:\n%s", want, pending)
		}
	}
	if code, out := approver.do("POST", "/api/changes/"+id+"/approve", map[string]any{"comment": "同意"}); code != 200 {
		t.Fatalf("审批: %d %v", code, out)
	}
	appr := h.waitFor(t, "变更审批通过")
	if !strings.Contains(appr, "审批人：approver") || !strings.Contains(appr, "第 1/1 级") {
		t.Errorf("change.approved:\n%s", appr)
	}
	h.waitFor(t, "变更开始执行")
	done := h.waitFor(t, "变更执行成功")
	if !strings.Contains(done, id) || !strings.Contains(done, `<font color="info">`) {
		t.Errorf("change.succeeded:\n%s", done)
	}
	// 不含命令行 / 命令输出 / 凭据
	for _, m := range h.all() {
		for _, bad := range []string{"tiup", "cluster restart", "pd-ctl", "--token", WebhookKeyPart(), "password"} {
			if strings.Contains(m, bad) {
				t.Errorf("消息不应含 %q:\n%s", bad, m)
			}
		}
	}
	// step / command 事件不发：整个流程只应有 pending / approved / started / succeeded 四条
	if n := len(h.all()); n != 4 {
		t.Errorf("消息数 = %d，期望 4（pending/approved/started/succeeded）: %q", n, h.all())
	}
	// 取消不通知
	code, out = dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-3", "action": "restart"}})
	if code != 200 {
		t.Fatalf("创建2: %d %v", code, out)
	}
	id2 := out["id"].(string)
	h.waitFor(t, id2)
	if code, out := dba.do("POST", "/api/changes/"+id2+"/cancel", nil); code != 200 {
		t.Fatalf("取消: %d %v", code, out)
	}
	e.srv.NotifyDrainForTest()
	time.Sleep(50 * time.Millisecond)
	if n := len(h.all()); n != 5 {
		t.Errorf("取消不应通知，消息数 = %d: %q", n, h.all())
	}
}

// WebhookKeyPart 是 Webhook 令牌的一段（消息里不应出现）。
func WebhookKeyPart() string { return "WEBHOOKKEY" }

func TestNotifyRejectedAndSubscription(t *testing.T) {
	e, h := notifyOps(t)
	dba, approver, admin, admin2 := e.login(t, "dba"), e.login(t, "approver"), e.login(t, "admin"), e.login(t, "admin2")
	addWecom(t, admin, h, "change.rejected") // 只订阅驳回
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	_, out := dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}})
	id := out["id"].(string)
	if code, o := approver.do("POST", "/api/changes/"+id+"/reject", map[string]any{"reason": "窗口不合适"}); code != 200 {
		t.Fatalf("驳回: %d %v", code, o)
	}
	m := h.waitFor(t, "变更被驳回")
	if !strings.Contains(m, "原因：窗口不合适") || !strings.Contains(m, "驳回人：approver") {
		t.Errorf("change.rejected:\n%s", m)
	}
	e.srv.NotifyDrainForTest()
	if n := len(h.all()); n != 1 {
		t.Errorf("未订阅的事件不应发送，消息数 = %d: %q", n, h.all())
	}
}

func TestNotifyWatchAlertsAndDiagnostics(t *testing.T) {
	e, h := notifyOps(t)
	admin := e.login(t, "admin")
	addWecom(t, admin, h, "alert.critical", "diag.critical")

	base := e.srv.Snapshot(context.Background(), false)
	base.Alerts = append(append([]model.Alert(nil), base.Alerts...), model.Alert{Lv: "critical", Name: "PreExisting", Inst: "tidb-1:4000", Desc: "启动前就存在"})
	base.Findings = append(append([]model.Finding(nil), base.Findings...), model.Finding{ID: "pre-existing", Sev: "bad", Title: "启动前的发现"})

	// 第一次只记录基线，不发送（包含启动前就存在的严重告警）
	e.srv.WatchOnceForTest(base)
	e.srv.NotifyDrainForTest()
	if n := len(h.all()); n != 0 {
		t.Fatalf("基线轮不应发送: %q", h.all())
	}
	// 第二轮：新出现的 critical 告警、bad 诊断发现才发；warning 与已存在的不发
	next := base
	next.Alerts = append(append([]model.Alert(nil), base.Alerts...),
		model.Alert{Lv: "critical", Name: "TiKV_down", Inst: "tikv-9:20160", Time: "10:20", Desc: "实例不可达"},
		model.Alert{Lv: "warning", Name: "SlowQuery", Inst: "tidb-1:4000"})
	next.Findings = append(append([]model.Finding(nil), base.Findings...),
		model.Finding{ID: "hot-region", Sev: "critical", Title: "热点 Region", Scope: "tikv-3", Cause: "写热点", Evidence: []string{"select * from t where secret='x'"}},
		model.Finding{ID: "minor", Sev: "warn", Title: "轻微问题"})
	e.srv.WatchOnceForTest(next)
	a := h.waitFor(t, "新出现严重告警：TiKV_down")
	for _, want := range []string{"实例：tikv-9:20160", "说明：实例不可达", "环境：staging", "[查看详情](https://ops.example.com/#overview)"} {
		if !strings.Contains(a, want) {
			t.Errorf("alert.critical 缺少 %q:\n%s", want, a)
		}
	}
	d := h.waitFor(t, "新出现严重诊断发现：热点 Region")
	if !strings.Contains(d, "范围：tikv-3") || strings.Contains(d, "select * from") {
		t.Errorf("diag.critical（不得含证据 / SQL）:\n%s", d)
	}
	e.srv.NotifyDrainForTest()
	if n := len(h.all()); n != 2 {
		t.Fatalf("第二轮应只发 2 条: %q", h.all())
	}
	// 第三轮：没有新增（同一份快照）→ 不再发；恢复（消失）也不发
	e.srv.WatchOnceForTest(next)
	e.srv.WatchOnceForTest(base)
	e.srv.NotifyDrainForTest()
	if n := len(h.all()); n != 2 {
		t.Errorf("无新增不应发送: %q", h.all())
	}
	// 采集失败（有错误且无任何告警 / 发现）不当作全部恢复，也不更新基线
	failed := model.Snapshot{Errors: []string{"采集失败"}}
	e.srv.WatchOnceForTest(failed)
	e.srv.WatchOnceForTest(base)
	e.srv.NotifyDrainForTest()
	if n := len(h.all()); n != 2 {
		t.Errorf("采集失败后恢复不应重复发送: %q", h.all())
	}
}
