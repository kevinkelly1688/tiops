// @author: kevinkelly
// @license: Apache License 2.0
// @file: hub_test.go
// @time: 2026/10/28

package server_test

// 多集群（Hub）测试：路由与隔离、越权 404、会话共享、审计、集群管理接口、并发。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	"tidbops/internal/clusters"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

var hubMaster = []byte("hub-master-key-for-test")

// fixturesRoot 返回仓库里的回放数据包根目录（D:\tidb\fixtures：demo / stg / dev）；缺失则跳过。
func fixturesRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"demo", "stg", "dev"} {
		if _, err := os.Stat(filepath.Join(root, d, "manifest.json")); err != nil {
			t.Skipf("缺少回放数据包 %s：%v", d, err)
		}
	}
	return root
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Printf(format string, args ...any) {
	l.mu.Lock()
	fmt.Fprintf(&l.b, format+"\n", args...)
	l.mu.Unlock()
}
func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type hubEnv struct {
	t       *testing.T
	ts      *httptest.Server
	hub     *server.Hub
	primary *server.Server
	dir     string
	fx      string
	audit   *audit.Log
	users   *authz.Store
	logs    *logBuf

	mu      sync.Mutex
	runners map[string]*exec.SimRunner
	failPD  map[string]bool // 测试用：PD 地址在其中的集群，实例构造会失败（模拟 live 后端启动失败）
	warnIDs map[string]string
	closed  bool
}

// newHub 创建一个主集群（prod-tidb-01，env=prod，回放 demo）的多集群环境。dir 为空则新建临时目录；
// 非空则复用（用于模拟重启：同一数据目录再起一遍）。
func newHub(t *testing.T, dir string) *hubEnv {
	t.Helper()
	fx := fixturesRoot(t)
	if dir == "" {
		dir = t.TempDir()
	}
	e := &hubEnv{t: t, dir: dir, fx: fx, logs: &logBuf{}, runners: map[string]*exec.SimRunner{}, failPD: map[string]bool{}, warnIDs: map[string]string{}}
	demo := filepath.Join(fx, "demo")
	rp, err := source.NewReplay(demo)
	if err != nil {
		t.Fatal(err)
	}
	params := source.CatalogParams{Cluster: rp.Manifest().Cluster, Home: "/home/tidb", Now: time.Now()}
	web := t.TempDir()
	_ = os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>home</html>"), 0o644)
	s := server.New(server.Config{Col: collect.New(rp, params), Mode: "replay", Web: web, SnapTTL: time.Minute, Logf: e.logs.Printf, Fixtures: demo})
	users, err := authz.OpenStore(filepath.Join(dir, "users.json"), hubMaster)
	if err != nil {
		t.Fatal(err)
	}
	if err := authz.SeedDemoUsers(users); err != nil {
		t.Fatal(err)
	}
	for name, roles := range map[string]map[string][]authz.Role{
		"prodonly": {"prod": {authz.RoleDBA}},
		"stgonly":  {"staging": {authz.RoleDBA}},
		"prodaud":  {"prod": {authz.RoleAuditor}},
	} {
		if _, ok := users.Get(name); !ok {
			if err := users.Add(name, name, authz.DemoPassword, roles, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	al, err := audit.Open(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	e.audit, e.users = al, users
	pol := exec.Policy{Clusters: []string{params.Cluster}, TempDir: t.TempDir(), AlertURL: "http://127.0.0.1:1", MySQLUser: "tidbops_ops", BackupRoots: []string{"local:///backup/"}}
	sim := &exec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: opsKey}
	if err := s.EnableOps(server.OpsConfig{Dir: dir, Env: "prod", Users: users, Audit: al, Runner: sim, Prober: &change.SimProber{Polls: 1}, Key: opsKey, MasterKey: hubMaster,
		ReadOnlyFile: filepath.Join(dir, "READONLY"), Sim: true, ExecMode: "sim", WaitScale: 0.001, TickEvery: time.Hour, NotifyWatchEvery: -1,
		NotifyInsecureTest: true, NotifyHosts: []string{"127.0.0.1"}, PublicURL: "https://ops.example.com/"}); err != nil {
		t.Fatal(err)
	}
	e.primary = s
	primary := clusters.Spec{ID: "prod-tidb-01", Name: params.Cluster, Display: params.Cluster, Env: "prod", Mode: "replay", Fixtures: demo}
	hub, err := server.NewHub(s, server.HubConfig{Dir: dir, Primary: primary, FixturesRoots: []string{fx}, Factory: e.factory})
	if err != nil {
		t.Fatal(err)
	}
	e.hub = hub
	e.ts = httptest.NewServer(hub.Handler())
	t.Cleanup(e.shutdown)
	return e
}

func (e *hubEnv) shutdown() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	e.ts.Close()
	e.hub.Close()
	e.primary.Close()
	_ = e.audit.Close()
}

// factory 是测试用的后端工厂：回放集群用 fixtures + 模拟执行器；live 集群的 PD 在 failPD 里则构造失败，否则也用 demo 数据冒充。
func (e *hubEnv) factory(spec clusters.Spec) (server.Backend, error) {
	e.mu.Lock()
	fail, warn := e.failPD[spec.PD], e.warnIDs[spec.ID]
	e.mu.Unlock()
	if fail {
		return server.Backend{}, fmt.Errorf("模拟的后端启动失败（连接 %s 超时）", spec.PD)
	}
	dir := spec.Fixtures
	if spec.Mode != clusters.ModeReplay {
		dir = filepath.Join(e.fx, "dev")
	}
	rp, err := source.NewReplay(dir)
	if err != nil {
		return server.Backend{}, err
	}
	params := source.CatalogParams{Cluster: spec.Name, Home: "/home/tidb", Now: time.Now()}
	be := server.Backend{Col: collect.New(rp, params), Mode: "replay", Fixtures: dir}
	be.Ops = func(s *server.Server, cfg *server.OpsConfig) error {
		pol := exec.Policy{Clusters: []string{spec.Name}, TempDir: e.t.TempDir(), AlertURL: "http://127.0.0.1:1", MySQLUser: "tidbops_ops", BackupRoots: []string{"local:///backup/"}}
		sim := &exec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: opsKey}
		e.mu.Lock()
		e.runners[spec.ID] = sim
		e.mu.Unlock()
		cfg.Runner, cfg.Prober, cfg.Sim, cfg.ExecMode, cfg.WaitScale = sim, &change.SimProber{Polls: 1}, true, "sim", 0.001
		return nil
	}
	if warn != "" {
		be.Preflight = func(_ context.Context, _ *server.Server) []string { return []string{warn} }
	}
	return be, nil
}

// ---------------------------------------------------------------- HTTP 客户端 ----

type hc struct {
	t       *testing.T
	base    string
	c       *http.Client
	csrf    string
	cluster string // 默认带的 X-Cluster
}

func (e *hubEnv) client() *hc {
	jar, _ := cookiejar.New(nil)
	return &hc{t: e.t, base: e.ts.URL, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *hubEnv) login(user string) *hc {
	e.t.Helper()
	c := e.client()
	code, out := c.do("POST", "/api/auth/login", map[string]string{"user": user, "password": authz.DemoPassword})
	if code != 200 {
		e.t.Fatalf("登录 %s 失败: %d %v", user, code, out)
	}
	c.csrf, _ = out["csrf"].(string)
	return c
}

// on 返回一个带 X-Cluster 的同会话客户端副本。
func (c *hc) on(id string) *hc {
	cp := *c
	cp.cluster = id
	return &cp
}

func (c *hc) raw(method, path string, body any, hdr map[string]string) (int, []byte, http.Header) {
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
	if c.cluster != "" {
		req.Header.Set("X-Cluster", c.cluster)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func (c *hc) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	code, b, _ := c.raw(method, path, body, nil)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{"_raw": string(b)}
	}
	return code, out
}

const notFoundBody = `{"error":"集群不存在或无权访问","code":"cluster_not_found"}`

func dg(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// addReplay 以管理员身份接入一个回放集群，返回响应。
func (e *hubEnv) addReplay(admin *hc, id, name, env, fixture string) (int, map[string]any) {
	return admin.do("POST", "/api/clusters", map[string]any{"id": id, "name": name, "display": id + " 集群", "env": env, "mode": "replay", "fixtures": filepath.Join(e.fx, fixture)})
}

func (e *hubEnv) addStgDev(admin *hc) {
	e.t.Helper()
	if code, out := e.addReplay(admin, "stg", "stg-tidb-01", "staging", "stg"); code != 200 {
		e.t.Fatalf("添加 stg: %d %v", code, out)
	}
	if code, out := e.addReplay(admin, "dev", "dev-tidb-01", "test", "dev"); code != 200 {
		e.t.Fatalf("添加 dev: %d %v", code, out)
	}
}

func (e *hubEnv) auditEntries(q string) []map[string]any {
	e.t.Helper()
	c := e.login("auditor")
	code, out := c.do("GET", "/api/audit?limit=1000"+q, nil)
	if code != 200 {
		e.t.Fatalf("审计查询: %d %v", code, out)
	}
	var res []map[string]any
	for _, x := range out["entries"].([]any) {
		res = append(res, x.(map[string]any))
	}
	return res
}

// enableWrite 经两名管理员在指定集群上打开写开关。
func (e *hubEnv) enableWrite(admin, admin2 *hc, id string) {
	e.t.Helper()
	admin.on(id).do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	if code, out := admin2.on(id).do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 200 || out["enabled"] != true {
		e.t.Fatalf("开启 %s 写开关: %d %v", id, code, out)
	}
}

// ---------------------------------------------------------------- 测试 ----

func TestHubSingleClusterDefaultsAndMe(t *testing.T) {
	e := newHub(t, "")
	dba := e.login("dba")
	// 没有任何附加集群：不带 X-Cluster 与带主集群 id 等价
	_, a := dba.do("GET", "/api/topology", nil)
	_, b := dba.on("prod-tidb-01").do("GET", "/api/topology", nil)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) || dg(a, "version") != "v8.5.2" {
		t.Errorf("缺省应等于主集群: %v / %v", a["version"], b["version"])
	}
	code, me := dba.do("GET", "/api/auth/me", nil)
	if code != 200 || dg(me, "env") != "prod" || dg(me, "cluster", "id") != "prod-tidb-01" || dg(me, "cluster", "primary") != true {
		t.Fatalf("me: %v", me)
	}
	cl := me["clusters"].([]any)
	if len(cl) != 1 || dg(cl[0].(map[string]any), "write", "enabled") != false {
		t.Errorf("clusters: %v", cl)
	}
	_, h := e.client().do("GET", "/api/health", nil)
	if dg(h, "ops", "env") != "prod" || dg(h, "cluster", "id") != "prod-tidb-01" {
		t.Errorf("health: %v", h)
	}
	// 静态页面不受影响
	code, body, _ := dba.raw("GET", "/", nil, nil)
	if code != 200 || !strings.Contains(string(body), "home") {
		t.Errorf("静态页: %d %s", code, body)
	}
}

func TestHubRoutingIsolationAndEnvValues(t *testing.T) {
	e := newHub(t, "")
	admin, admin2, dba := e.login("admin"), e.login("admin2"), e.login("dba")
	e.addStgDev(admin)

	// 按 X-Cluster 路由：版本不同
	ver := func(c *hc, id string) any {
		_, o := c.on(id).do("GET", "/api/topology", nil)
		return o["version"]
	}
	if ver(dba, "") != "v8.5.2" || ver(dba, "stg") != "v8.1.5" || ver(dba, "dev") != "v8.1.2" {
		t.Errorf("路由版本不对: %v %v %v", ver(dba, ""), ver(dba, "stg"), ver(dba, "dev"))
	}
	// 查询参数同样可选集群（用于下载 / 导出链接）
	if code, o := dba.do("GET", "/api/topology?cluster=stg", nil); code != 200 || o["version"] != "v8.1.5" {
		t.Errorf("?cluster=: %d %v", code, o["version"])
	}
	// env / cluster 值是请求所属集群的
	for id, env := range map[string]string{"": "prod", "stg": "staging", "dev": "test"} {
		_, me := dba.on(id).do("GET", "/api/auth/me", nil)
		if dg(me, "env") != env {
			t.Errorf("me(%q).env=%v 期望 %s", id, dg(me, "env"), env)
		}
		_, h := dba.on(id).do("GET", "/api/health", nil)
		if dg(h, "ops", "env") != env {
			t.Errorf("health(%q).ops.env=%v 期望 %s", id, dg(h, "ops", "env"), env)
		}
	}
	_, me := dba.on("stg").do("GET", "/api/auth/me", nil)
	if dg(me, "cluster", "id") != "stg" || len(me["clusters"].([]any)) != 3 {
		t.Errorf("me.clusters: %v", me)
	}

	// 写开关隔离：在 stg 打开，prod / dev 不受影响
	e.enableWrite(admin, admin2, "stg")
	for id, want := range map[string]bool{"stg": true, "": false, "dev": false} {
		_, m := dba.on(id).do("GET", "/api/auth/me", nil)
		if dg(m, "write", "enabled") != want {
			t.Errorf("集群 %q 写开关 %v 期望 %v", id, dg(m, "write", "enabled"), want)
		}
	}
	// 设置（LB / 冻结期 / 窗口）隔离
	stgAdmin := admin.on("stg")
	if code, out := stgAdmin.do("POST", "/api/ops/settings", map[string]any{"lb": true, "freezes": []any{map[string]any{"from": "2030-01-01T00:00:00Z", "to": "2030-01-02T00:00:00Z", "label": "stg-freeze"}}, "windows": []any{}}); code != 200 {
		t.Fatalf("改 stg 设置: %d %v", code, out)
	}
	for id, want := range map[string]bool{"stg": true, "": false, "dev": false} {
		_, st := admin.on(id).do("GET", "/api/ops/settings", nil)
		if dg(st, "settings", "lb") != want {
			t.Errorf("集群 %q lb=%v 期望 %v", id, dg(st, "settings", "lb"), want)
		}
	}
	_, st := admin.on("stg").do("GET", "/api/ops/settings", nil)
	if w, _ := dg(st, "settings", "windows").([]any); len(w) != 0 {
		t.Errorf("stg 窗口应已清空: %v", w)
	}
	_, st = admin.do("GET", "/api/ops/settings", nil)
	if w, _ := dg(st, "settings", "windows").([]any); len(w) == 0 {
		t.Errorf("prod 仍应是默认窗口规则: %v", st)
	}
	for id, want := range map[string]int{"stg": 1, "": 0, "dev": 0} {
		_, st := admin.on(id).do("GET", "/api/ops/settings", nil)
		if fz, _ := dg(st, "settings", "freezes").([]any); len(fz) != want {
			t.Errorf("集群 %q 冻结期 %d 条，期望 %d", id, len(fz), want)
		}
	}
	// 变更隔离：stg 上创建的变更，prod / dev 的列表与详情里都没有
	body := map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "start"}}
	code, ch := dba.on("stg").do("POST", "/api/changes", body)
	if code != 200 {
		t.Fatalf("stg 创建变更: %d %v", code, ch)
	}
	id := ch["id"].(string)
	if ch["cluster"] != "stg-tidb-01" || ch["env"] != "staging" {
		t.Errorf("变更单的 cluster/env 应是 stg 的: %v %v", ch["cluster"], ch["env"])
	}
	for cid, want := range map[string]int{"stg": 1, "": 0, "dev": 0} {
		_, l := dba.on(cid).do("GET", "/api/changes", nil)
		if got := len(l["changes"].([]any)); got != want {
			t.Errorf("集群 %q 变更数 %d 期望 %d", cid, got, want)
		}
	}
	for _, cid := range []string{"", "dev"} {
		if code, _ := dba.on(cid).do("GET", "/api/changes/"+id, nil); code != 404 {
			t.Errorf("集群 %q 不应能读到 stg 的变更 %s: %d", cid, id, code)
		}
		if code, _ := admin.on(cid).do("POST", "/api/changes/"+id+"/reject", map[string]any{"reason": "x"}); code != 404 {
			t.Errorf("集群 %q 不应能驳回 stg 的变更: %d", cid, code)
		}
	}
	if code, _ := dba.do("GET", "/api/maints", nil); code != 200 {
		t.Errorf("maints: %d", code)
	}
	// 通知渠道隔离：在 stg 配一个渠道，prod / dev 看不到
	if code, out := admin.on("stg").do("POST", "/api/ops/notify/channels", map[string]any{"type": "wecom", "name": "stg-群", "url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc"}); code != 200 {
		t.Fatalf("加通知渠道: %d %v", code, out)
	}
	for id, n := range map[string]int{"stg": 1, "": 0, "dev": 0} {
		_, o := admin.on(id).do("GET", "/api/ops/notify", nil)
		if got := len(o["channels"].([]any)); got != n {
			t.Errorf("集群 %q 通知渠道 %d 个，期望 %d", id, got, n)
		}
	}
}

func TestHubTokenBoundToCluster(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	e.mu.Lock()
	stg, dev := e.runners["stg"], e.runners["dev"]
	e.mu.Unlock()
	if stg == nil || dev == nil {
		t.Fatal("缺少模拟执行器")
	}
	cmdStg := exec.Cmd{Kind: exec.KindClusterRestart, Cluster: "stg-tidb-01", Role: "tidb"}
	cmdDev := exec.Cmd{Kind: exec.KindClusterRestart, Cluster: "dev-tidb-01", Role: "tidb"}
	tok := exec.Sign(opsKey, cmdStg, "CHG-0001", time.Now().Add(time.Minute))
	run := func(r *exec.SimRunner, c exec.Cmd, token string) error {
		_, err := r.Run(context.Background(), exec.Request{Cmd: c, Actor: "t", ChangeID: "CHG-0001", Token: token}, nil)
		return err
	}
	if err := run(stg, cmdStg, tok); err != nil {
		t.Fatalf("stg 的令牌应能在 stg 执行: %v", err)
	}
	if err := run(dev, cmdStg, tok); err == nil {
		t.Error("stg 的令牌 + stg 的命令不能在 dev 的执行器上执行（集群不在 dev 的白名单）")
	}
	if err := run(dev, cmdDev, tok); err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Errorf("stg 的令牌不能用于 dev 的命令: %v", err)
	}
	if err := run(stg, cmdDev, tok); err == nil {
		t.Error("dev 的命令不能在 stg 的执行器上执行")
	}
}

func TestHubVisibilityUnified404(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	prodonly, stgonly := e.login("prodonly"), e.login("stgonly")

	// prodonly 在 staging / test 下没有任何角色：看不到 stg、dev
	check404 := func(who string, c *hc, method, path string, hdr map[string]string) {
		t.Helper()
		code, b, _ := c.raw(method, path, nil, hdr)
		if code != 404 || strings.TrimSpace(string(b)) != notFoundBody {
			t.Errorf("%s %s %s %v → %d %q，应为统一的 404", who, method, path, hdr, code, b)
		}
	}
	variants := []string{"stg", "STG", "Stg", "stg,stg", "stg;", "ｓｔｇ", "stg%00", "../stg", "nonexistent", "dev", "*", "stg\\x", "stg\\", "prod-tidb-01,stg"}
	for _, v := range variants {
		check404("prodonly", prodonly, "GET", "/api/snapshot", map[string]string{"X-Cluster": v})
		check404("prodonly", prodonly, "GET", "/api/changes", map[string]string{"X-Cluster": v})
		check404("prodonly", prodonly, "POST", "/api/changes/preview", map[string]string{"X-Cluster": v})
	}
	check404("admin", admin, "GET", "/api/snapshot", map[string]string{"X-Cluster": "PROD-TIDB-01"}) // 大小写变形连主集群也不匹配
	check404("prodonly", prodonly, "GET", "/api/topology?cluster=stg", nil)
	check404("prodonly", prodonly, "GET", "/api/topology?cluster=STG", nil)
	check404("prodonly", prodonly, "GET", "/api/auth/me", map[string]string{"X-Cluster": "stg"})
	check404("prodonly", prodonly, "GET", "/api/health", map[string]string{"X-Cluster": "stg"})
	// 重复的头 / 参数 / 头与参数冲突：一律不认
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/snapshot", nil)
	req.Header.Add("X-Cluster", "prod-tidb-01")
	req.Header.Add("X-Cluster", "stg")
	prodonly.c.Jar.SetCookies(req.URL, prodonly.c.Jar.Cookies(req.URL))
	resp, err := prodonly.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || strings.TrimSpace(string(b)) != notFoundBody {
		t.Errorf("重复头: %d %s", resp.StatusCode, b)
	}
	check404("prodonly", prodonly, "GET", "/api/snapshot?cluster=prod-tidb-01&cluster=stg", nil)

	// 管理员的 stg 访问正常；prodonly 访问自己的主集群正常
	if code, _ := admin.on("stg").do("GET", "/api/snapshot", nil); code != 200 {
		t.Errorf("admin 访问 stg: %d", code)
	}
	if code, _ := prodonly.on("prod-tidb-01").do("GET", "/api/snapshot", nil); code != 200 {
		t.Errorf("prodonly 访问 prod: %d", code)
	}
	// 与「不存在」的集群响应字节级一致（不泄露是否存在）
	_, b1, h1 := prodonly.raw("GET", "/api/snapshot", nil, map[string]string{"X-Cluster": "stg"})
	_, b2, h2 := prodonly.raw("GET", "/api/snapshot", nil, map[string]string{"X-Cluster": "no-such"})
	if !bytes.Equal(b1, b2) || h1.Get("Content-Type") != h2.Get("Content-Type") {
		t.Errorf("存在但不可见与不存在的响应应完全一致: %q vs %q", b1, b2)
	}

	// 列表只给可见的；没有 settings.edit 看不到 config
	_, l := prodonly.do("GET", "/api/clusters", nil)
	cl := l["clusters"].([]any)
	if len(cl) != 1 || dg(cl[0].(map[string]any), "id") != "prod-tidb-01" || cl[0].(map[string]any)["config"] != nil || l["canManage"] != false {
		t.Errorf("prodonly 的集群列表: %v", l)
	}
	_, me := prodonly.do("GET", "/api/auth/me", nil)
	if len(me["clusters"].([]any)) != 1 {
		t.Errorf("me.clusters: %v", me["clusters"])
	}

	// stgonly 看不到主集群（prod）：缺省路径同样 404（多集群模式下缺省也要可见）；me 缺省落到第一个可见集群
	check404("stgonly", stgonly, "GET", "/api/snapshot", nil)
	check404("stgonly", stgonly, "GET", "/api/changes", nil)
	_, me = stgonly.do("GET", "/api/auth/me", nil)
	if dg(me, "cluster", "id") != "stg" || dg(me, "env") != "staging" || len(me["clusters"].([]any)) != 1 {
		t.Errorf("stgonly 的 me: %v", me)
	}
	if code, _ := stgonly.on("stg").do("GET", "/api/snapshot", nil); code != 200 {
		t.Errorf("stgonly 访问 stg: %d", code)
	}
	check404("stgonly", stgonly, "GET", "/api/snapshot", map[string]string{"X-Cluster": "prod-tidb-01"})
	// 登录响应里的 me 也按可见集群给（stgonly 登录后缺省集群是 stg）
	c := e.client()
	_, lo := c.do("POST", "/api/auth/login", map[string]string{"user": "stgonly", "password": authz.DemoPassword})
	if dg(lo, "cluster", "id") != "stg" || dg(lo, "env") != "staging" {
		t.Errorf("登录响应: %v", lo)
	}

	// 未登录：统一走 401，不区分集群是否存在
	anon := e.client()
	for _, v := range []string{"stg", "no-such", ""} {
		if code, _, _ := anon.raw("GET", "/api/snapshot", nil, map[string]string{"X-Cluster": v}); code != 401 {
			t.Errorf("未登录 X-Cluster=%q 应 401: %d", v, code)
		}
	}
	if code, _, _ := anon.raw("POST", "/api/changes", map[string]any{}, map[string]string{"X-Cluster": "stg"}); code != 401 {
		t.Errorf("未登录写接口应 401: %d", code)
	}
	// 匿名 health 忽略 X-Cluster，回主集群，不泄露
	_, hb, _ := anon.raw("GET", "/api/health", nil, map[string]string{"X-Cluster": "stg"})
	if !strings.Contains(string(hb), `"env":"prod"`) || strings.Contains(string(hb), "staging") {
		t.Errorf("匿名 health: %s", hb)
	}
	// 全局端点与 X-Cluster 无关：带不存在的集群也照常
	if code, _ := admin.on("no-such").do("GET", "/api/users", nil); code != 200 {
		t.Errorf("/api/users 是全局端点: %d", code)
	}
	if code, _ := admin.on("no-such").do("GET", "/api/clusters", nil); code != 200 {
		t.Errorf("/api/clusters 是全局端点: %d", code)
	}
}

func TestHubSessionsSharedAcrossClusters(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)

	dba := e.login("dba")
	for _, id := range []string{"", "stg", "dev"} {
		if code, _ := dba.on(id).do("GET", "/api/snapshot", nil); code != 200 {
			t.Errorf("登录一次后集群 %q 应可用: %d", id, code)
		}
	}
	// CSRF 令牌跨集群通用
	for _, id := range []string{"", "stg", "dev"} {
		if code, _ := dba.on(id).do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "start"}}); code != 200 {
			t.Errorf("集群 %q 预览应通过 CSRF: %d", id, code)
		}
	}
	bad := dba.on("stg")
	bad.csrf = "wrong"
	if code, _ := bad.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op"}); code != 403 {
		t.Errorf("错误的 CSRF 应 403: %d", code)
	}
	// 在 stg 上退出登录：prod / dev 同样失效
	if code, _ := dba.on("stg").do("POST", "/api/auth/logout", nil); code != 200 {
		t.Fatal("logout")
	}
	for _, id := range []string{"", "stg", "dev"} {
		if code, _ := dba.on(id).do("GET", "/api/snapshot", nil); code != 401 {
			t.Errorf("退出后集群 %q 应 401: %d", id, code)
		}
	}
	// 改角色（吊销会话）对所有集群立即生效
	sre := e.login("sre")
	if code, out := admin.do("POST", "/api/users/sre/roles", map[string]any{"roles": map[string][]string{"*": {"viewer"}}}); code != 200 {
		t.Fatalf("改角色: %d %v", code, out)
	}
	for _, id := range []string{"", "stg", "dev"} {
		if code, _ := sre.on(id).do("GET", "/api/snapshot", nil); code != 401 {
			t.Errorf("角色变更后集群 %q 应 401: %d", id, code)
		}
	}
	// 禁用同理
	viewer := e.login("viewer")
	if code, _ := admin.do("POST", "/api/users/viewer/disable", map[string]any{"disabled": true}); code != 200 {
		t.Fatal("disable")
	}
	for _, id := range []string{"", "stg", "dev"} {
		if code, _ := viewer.on(id).do("GET", "/api/snapshot", nil); code != 401 {
			t.Errorf("禁用后集群 %q 应 401: %d", id, code)
		}
	}
}

func TestHubRateLimitSharedAcrossClusters(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	n429 := 0
	ids := []string{"", "stg", "dev"}
	for i := 0; i < 90; i++ {
		code, _ := admin.on(ids[i%3]).do("POST", "/api/ops/write-switch/cancel", nil)
		if code == 429 {
			n429++
		}
	}
	if n429 == 0 {
		t.Error("每用户每分钟 60 次的限速应跨集群合计生效")
	}
}

func TestHubGlobalApprovalPolicyAppliesToEveryCluster(t *testing.T) {
	e := newHub(t, "")
	admin, admin2, dba := e.login("admin"), e.login("admin2"), e.login("dba")
	e.addStgDev(admin)
	preview := func(id string) float64 {
		code, out := dba.on(id).do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "restart"}})
		if code != 200 {
			t.Fatalf("preview %q: %d %v", id, code, out)
		}
		return out["levels"].(float64)
	}
	_, pol := admin.do("GET", "/api/ops/policy", nil)
	cur := pol["policy"].(map[string]any)
	levels := cur["levels"].(map[string]any)
	riskOf := func(id string) string {
		_, out := dba.on(id).do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "restart"}})
		return dg(out, "plan", "risk").(string)
	}
	risk := riskOf("stg")
	before := preview("stg")
	// 提议把 staging 在该风险下的级数 +1（高风险下限 2，所以只在不超过 3 时 +1）
	envLv := levels[risk].(map[string]any)
	next := int(envLv["staging"].(float64)) + 1
	if next > 3 {
		t.Skipf("风险 %s 在 staging 已是 3 级", risk)
	}
	envLv["staging"] = float64(next)
	code, out := admin.on("stg").do("POST", "/api/ops/policy", map[string]any{"policy": cur})
	if code != 202 {
		t.Fatalf("提议策略（即使带着 X-Cluster 也由主集群处理）: %d %v", code, out)
	}
	pid := dg(out, "proposal", "id").(string)
	if code, o := admin2.on("dev").do("POST", "/api/ops/proposals/"+pid+"/confirm", nil); code != 200 {
		t.Fatalf("确认: %d %v", code, o)
	}
	if got := preview("stg"); got != before+1 {
		t.Errorf("stg 的审批级数应随全局策略变化：%v → %v", before, got)
	}
	// 其他环境的格子没变：prod 同风险仍是原来的级数
	if got, want := preview(""), levels[risk].(map[string]any)["prod"].(float64); got != want {
		t.Errorf("prod 级数 %v 期望 %v", got, want)
	}
}

func TestHubAuditClusterEnvAndFilter(t *testing.T) {
	e := newHub(t, "")
	admin, admin2, dba := e.login("admin"), e.login("admin2"), e.login("dba")
	e.addStgDev(admin)
	e.enableWrite(admin, admin2, "stg")
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}) // prod 只提议
	dba.on("stg").do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "start"}})
	admin.on("dev").do("POST", "/api/ops/settings", map[string]any{"lb": true})

	all := e.auditEntries("")
	find := func(es []map[string]any, action, cluster string) map[string]any {
		for _, x := range es {
			if x["action"] == action && x["cluster"] == cluster {
				return x
			}
		}
		return nil
	}
	for _, c := range []struct{ action, cluster, env string }{
		{"writeswitch.on", "stg-tidb-01", "staging"},
		{"writeswitch.propose", "prod-tidb-01", "prod"},
		{"settings.update", "dev-tidb-01", "test"},
		{"cluster.add", "stg-tidb-01", "staging"},
		{"cluster.add", "dev-tidb-01", "test"},
		{"platform.start", "stg-tidb-01", "staging"},
	} {
		x := find(all, c.action, c.cluster)
		if x == nil || x["env"] != c.env {
			t.Errorf("审计 %s 应带 cluster=%s env=%s: %v", c.action, c.cluster, c.env, x)
		}
	}
	// 引擎事件（变更创建）
	var gotChange bool
	for _, x := range all {
		if strings.HasPrefix(x["action"].(string), "change.") && x["cluster"] == "stg-tidb-01" && x["env"] == "staging" {
			gotChange = true
		}
		if strings.HasPrefix(x["action"].(string), "change.") && x["cluster"] != "stg-tidb-01" {
			t.Errorf("变更事件的 cluster 错误: %v", x)
		}
	}
	if !gotChange {
		t.Error("应有 stg 的变更审计")
	}
	// 筛选：id 与 tiup 名都可以
	for _, q := range []string{"&cluster=stg", "&cluster=stg-tidb-01"} {
		es := e.auditEntries(q)
		if len(es) == 0 {
			t.Fatalf("%s 没有结果", q)
		}
		for _, x := range es {
			if x["cluster"] != "stg-tidb-01" {
				t.Errorf("%s 混入了其他集群的记录: %v", q, x)
			}
		}
	}
	for _, x := range e.auditEntries("&cluster=prod-tidb-01") {
		if x["cluster"] != "prod-tidb-01" {
			t.Errorf("筛 prod 混入: %v", x)
		}
	}
	if es := e.auditEntries("&cluster=no-such"); len(es) != 0 {
		t.Errorf("不存在的集群筛选应为空: %d", len(es))
	}
	// 导出同样支持筛选
	auditor := e.login("auditor")
	code, b, _ := auditor.raw("GET", "/api/audit/export?format=jsonl&cluster=dev", nil, nil)
	if code != 200 || len(bytes.TrimSpace(b)) == 0 {
		t.Fatalf("导出: %d", code)
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var x map[string]any
		_ = json.Unmarshal([]byte(ln), &x)
		if x["cluster"] != "dev-tidb-01" {
			t.Errorf("导出混入了其他集群: %s", ln)
		}
	}
	// 哈希链仍然完整（同一条链）
	if _, o := auditor.do("GET", "/api/audit/verify", nil); o["ok"] != true {
		t.Errorf("审计链: %v", o)
	}

	// 只有 prod 的审计权限：看不到 staging / test 的记录
	prodaud := e.login("prodaud")
	_, out := prodaud.do("GET", "/api/audit?limit=1000", nil)
	n := 0
	for _, x := range out["entries"].([]any) {
		n++
		if env := x.(map[string]any)["env"]; env != "prod" {
			t.Errorf("prodaud 不应看到 env=%v 的审计: %v", env, x)
		}
	}
	if n == 0 {
		t.Error("prodaud 应能看到 prod 的审计")
	}
	if _, o := prodaud.do("GET", "/api/audit?cluster=stg", nil); len(o["entries"].([]any)) != 0 {
		t.Error("prodaud 筛 stg 应为空")
	}
}

func TestHubClusterManagementLifecycle(t *testing.T) {
	e := newHub(t, "")
	admin, admin2, dba := e.login("admin"), e.login("admin2"), e.login("dba")

	// 权限：未登录 401；dba 没有 settings.edit 403；CSRF
	if code, _, _ := e.client().raw("POST", "/api/clusters", map[string]any{}, nil); code != 401 {
		t.Errorf("未登录: %d", code)
	}
	if code, _ := dba.do("POST", "/api/clusters", map[string]any{"id": "stg"}); code != 403 {
		t.Errorf("dba 不能添加: %d", code)
	}
	noCSRF := *admin
	noCSRF.csrf = ""
	if code, _ := noCSRF.do("POST", "/api/clusters", map[string]any{"id": "stg"}); code != 403 {
		t.Errorf("缺 CSRF: %d", code)
	}
	// GET 列表：管理员带 config，主集群在前
	_, l := admin.do("GET", "/api/clusters", nil)
	cl := l["clusters"].([]any)
	p0 := cl[0].(map[string]any)
	if len(cl) != 1 || p0["primary"] != true || dg(p0, "config", "name") != "prod-tidb-01" || l["canManage"] != true || l["primary"] != "prod-tidb-01" {
		t.Fatalf("初始列表: %v", l)
	}

	// 添加：非法输入 → 400 invalid；路径越界；重复
	bad := []map[string]any{
		{"id": "Bad_ID", "env": "staging", "mode": "replay", "fixtures": filepath.Join(e.fx, "stg")},
		{"id": "x1", "env": "prod2", "mode": "replay", "fixtures": filepath.Join(e.fx, "stg")},
		{"id": "x1", "env": "staging", "mode": "replay", "fixtures": filepath.Join(e.fx, "..", "backend")},
		{"id": "x1", "env": "staging", "mode": "replay", "fixtures": t.TempDir()},
		{"id": "x1", "env": "staging", "mode": "replay"},
		{"id": "x1", "env": "staging", "mode": "live", "pd": "http://user:pass@10.0.0.1:2379"},
		{"id": "x1", "env": "staging", "mode": "live", "prom": "gopher://x"},
		{"id": "x1", "env": "staging", "mode": "live", "name": "a b"},
	}
	for _, b := range bad {
		if code, out := admin.do("POST", "/api/clusters", b); code != 400 || out["code"] != "invalid" {
			t.Errorf("%v 应 400 invalid: %d %v", b, code, out)
		}
	}
	code, out := e.addReplay(admin, "stg", "stg-tidb-01", "staging", "stg")
	if code != 200 || out["ok"] != true || dg(out, "cluster", "state") != "ok" || dg(out, "cluster", "config", "hasPassword") != false {
		t.Fatalf("添加: %d %v", code, out)
	}
	// 热生效：立即可用，且数据目录布局为 clusters/<id>/
	if code, o := dba.on("stg").do("GET", "/api/topology", nil); code != 200 || o["version"] != "v8.1.5" {
		t.Errorf("热添加后应立即可用: %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "clusters", "stg")); err != nil {
		t.Errorf("集群数据目录: %v", err)
	}
	if code, o := e.addReplay(admin, "stg", "other", "staging", "stg"); code != 409 || o["code"] != "exists" {
		t.Errorf("重复 id: %d %v", code, o)
	}
	if code, o := e.addReplay(admin, "other", "stg-tidb-01", "staging", "stg"); code != 409 || o["code"] != "exists" {
		t.Errorf("重复 name: %d %v", code, o)
	}
	if code, o := e.addReplay(admin, "prod-tidb-01", "x", "staging", "stg"); code != 409 || o["code"] != "exists" {
		t.Errorf("与主集群同 id: %d %v", code, o)
	}

	// 更新：id 不可改；display / desc 热改；env 改变需要无在途变更
	if code, o := admin.do("POST", "/api/clusters/stg", map[string]any{"id": "other"}); code != 400 {
		t.Errorf("id 不可改: %d %v", code, o)
	}
	if code, o := admin.do("POST", "/api/clusters/nope", map[string]any{"display": "x"}); code != 404 || o["code"] != "not_found" {
		t.Errorf("不存在: %d %v", code, o)
	}
	code, out = admin.do("POST", "/api/clusters/stg", map[string]any{"display": "预发（新）", "desc": "新说明"})
	if code != 200 || dg(out, "cluster", "display") != "预发（新）" {
		t.Fatalf("更新 display: %d %v", code, out)
	}
	// 主集群：只能改 display / desc
	if code, o := admin.do("POST", "/api/clusters/prod-tidb-01", map[string]any{"mode": "live"}); code != 403 || o["code"] != "primary" {
		t.Errorf("主集群不可改 mode: %d %v", code, o)
	}
	if code, _ := admin.do("POST", "/api/clusters/prod-tidb-01", map[string]any{"display": "生产主集群"}); code != 200 {
		t.Errorf("主集群可改 display: %d", code)
	}
	if code, o := admin.do("POST", "/api/clusters/prod-tidb-01/delete", nil); code != 403 || o["code"] != "primary" {
		t.Errorf("主集群不可删: %d %v", code, o)
	}

	// 在途变更：删除 / 改 env 被拒（in_use）
	e.enableWrite(admin, admin2, "stg")
	code, ch := dba.on("stg").do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-1", "action": "restart"}})
	if code != 200 {
		t.Fatalf("创建变更: %d %v", code, ch)
	}
	if ch["state"] != "pending" {
		t.Skipf("变更没有停在待审批（%v），无法测试在用保护", ch["state"])
	}
	if code, o := admin.do("POST", "/api/clusters/stg/delete", nil); code != 409 || o["code"] != "in_use" {
		t.Errorf("有待审批变更时删除应被拒: %d %v", code, o)
	}
	if code, o := admin.do("POST", "/api/clusters/stg", map[string]any{"env": "test"}); code != 409 || o["code"] != "in_use" {
		t.Errorf("有待审批变更时改 env 应被拒: %d %v", code, o)
	}
	// 被拒之后实例仍然正常
	if code, _ := dba.on("stg").do("GET", "/api/changes/"+ch["id"].(string), nil); code != 200 {
		t.Errorf("拒绝删除后实例应恢复: %d", code)
	}
	// 处理完（取消）后可以删除；数据目录改名保留
	if code, o := dba.on("stg").do("POST", "/api/changes/"+ch["id"].(string)+"/cancel", map[string]any{"reason": "t"}); code != 200 {
		t.Fatalf("取消: %d %v", code, o)
	}
	code, out = admin.do("POST", "/api/clusters/stg/delete", nil)
	if code != 200 {
		t.Fatalf("删除: %d %v", code, out)
	}
	if code, b, _ := dba.on("stg").raw("GET", "/api/topology", nil, nil); code != 404 || strings.TrimSpace(string(b)) != notFoundBody {
		t.Errorf("热删除后端点应 404: %d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "clusters", "stg")); !os.IsNotExist(err) {
		t.Errorf("原数据目录应已改名: %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(e.dir, "clusters"))
	var kept string
	for _, d := range ents {
		if strings.HasPrefix(d.Name(), ".removed-stg-") {
			kept = d.Name()
		}
	}
	if kept == "" {
		t.Fatalf("应保留 .removed-stg-<时间戳> 目录: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "clusters", kept, "changes.json")); err != nil {
		t.Errorf("保留的目录应含运行态数据: %v", err)
	}
	if kp, _ := out["keptDir"].(string); !strings.Contains(kp, ".removed-stg-") {
		t.Errorf("keptDir: %v", out["keptDir"])
	}
	// 之后 id 可以重新使用，数据从空开始
	if code, o := e.addReplay(admin, "stg", "stg-tidb-01", "staging", "stg"); code != 200 {
		t.Fatalf("重新添加: %d %v", code, o)
	}
	_, l2 := dba.on("stg").do("GET", "/api/changes", nil)
	if len(l2["changes"].([]any)) != 0 {
		t.Errorf("重新添加的集群应是空数据: %v", l2)
	}
	// 审计里有 add / update / remove，且 remove 为 high
	var sawRemove bool
	for _, x := range e.auditEntries("") {
		if x["action"] == "cluster.remove" && x["result"] == "success" {
			sawRemove = true
			if x["risk"] != "high" || x["target"] != "stg" || x["env"] != "staging" {
				t.Errorf("remove 审计: %v", x)
			}
		}
	}
	if !sawRemove {
		t.Error("缺少 cluster.remove 审计")
	}
}

func TestHubAddFailureRollsBack(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.mu.Lock()
	e.failPD["http://10.9.9.9:2379"] = true
	e.mu.Unlock()
	code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "liv", "name": "liv-tidb", "env": "staging", "mode": "live", "pd": "http://10.9.9.9:2379"})
	if code != 422 || out["code"] != "start" {
		t.Fatalf("启动失败应 422 start: %d %v", code, out)
	}
	_, l := admin.do("GET", "/api/clusters", nil)
	if len(l["clusters"].([]any)) != 1 {
		t.Errorf("失败的添加不能留下痕迹: %v", l)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "clusters", "liv")); !os.IsNotExist(err) {
		t.Errorf("失败的添加不应遗留数据目录: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "clusters.json")); strings.Contains(string(b), "liv") {
		t.Errorf("失败的添加不应写入 clusters.json: %s", b)
	}
	// 之后同 id 再试（修好后）可以成功
	e.mu.Lock()
	delete(e.failPD, "http://10.9.9.9:2379")
	e.mu.Unlock()
	if code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "liv", "name": "liv-tidb", "env": "staging", "mode": "live", "pd": "http://10.9.9.9:2379"}); code != 200 {
		t.Fatalf("修好后应成功: %d %v", code, out)
	}
	// 更新时新配置启动失败：回滚到旧配置，旧实例继续服务
	e.mu.Lock()
	e.failPD["http://10.9.9.8:2379"] = true
	e.mu.Unlock()
	code, out = admin.do("POST", "/api/clusters/liv", map[string]any{"pd": "http://10.9.9.8:2379"})
	if code != 422 || out["code"] != "start" {
		t.Fatalf("更新失败应 422: %d %v", code, out)
	}
	e.mu.Lock()
	delete(e.failPD, "http://10.9.9.8:2379")
	e.mu.Unlock()
	_, l = admin.do("GET", "/api/clusters", nil)
	var got map[string]any
	for _, c := range l["clusters"].([]any) {
		if c.(map[string]any)["id"] == "liv" {
			got = c.(map[string]any)
		}
	}
	if got == nil || dg(got, "config", "pd") != "http://10.9.9.9:2379" || got["state"] != "ok" {
		t.Errorf("回滚后应保留旧配置并可用: %v", got)
	}
	if code, _ := admin.on("liv").do("GET", "/api/topology", nil); code != 200 {
		t.Errorf("回滚后的实例应可用: %d", code)
	}
	// 单个集群的失败不影响其他集群
	if code, _ := admin.do("GET", "/api/snapshot", nil); code != 200 {
		t.Errorf("主集群应不受影响: %d", code)
	}
}

func TestHubPreflightWarning(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.mu.Lock()
	e.warnIDs["warn1"] = "执行器预检未通过：集群名 warn1 不在特权端策略文件里"
	e.mu.Unlock()
	code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "warn1", "env": "test", "mode": "live", "pd": "http://10.9.9.9:2379"})
	ws, _ := out["warnings"].([]any)
	if code != 200 || len(ws) != 1 || !strings.Contains(ws[0].(string), "预检") {
		t.Errorf("预检警告应在响应里返回，而不是静默: %d %v", code, out)
	}
	_, l := admin.do("GET", "/api/clusters", nil)
	for _, c := range l["clusters"].([]any) {
		if c.(map[string]any)["id"] == "warn1" && len(c.(map[string]any)["warnings"].([]any)) != 1 {
			t.Errorf("列表里也应带警告: %v", c)
		}
	}
}

func TestHubCredentialsNeverLeak(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	const pw = "Sup3r-Secret-DB-Passw0rd"
	body := map[string]any{"id": "liv", "name": "liv-tidb", "display": "实时集群", "env": "staging", "mode": "live", "pd": "http://10.9.9.9:2379",
		"mysqlHost": "10.9.9.9", "mysqlUser": "ro", "mysqlPassword": pw}
	code, raw, _ := admin.raw("POST", "/api/clusters", body, nil)
	if code != 200 || bytes.Contains(raw, []byte(pw)) || !bytes.Contains(raw, []byte(`"hasPassword":true`)) {
		t.Fatalf("添加响应不得回显密码: %d %s", code, raw)
	}
	_, raw, _ = admin.raw("GET", "/api/clusters", nil, nil)
	if bytes.Contains(raw, []byte(pw)) {
		t.Errorf("列表不得回显密码: %s", raw)
	}
	_, raw, _ = admin.raw("POST", "/api/clusters/liv", map[string]any{"display": "改名", "mysqlPassword": ""}, nil)
	if bytes.Contains(raw, []byte(pw)) {
		t.Errorf("更新响应不得回显密码: %s", raw)
	}
	_, raw, _ = admin.raw("POST", "/api/clusters/liv", map[string]any{"mysqlPassword": pw + "-2"}, nil)
	if bytes.Contains(raw, []byte(pw)) {
		t.Errorf("换密码响应不得回显: %s", raw)
	}
	// 落盘：clusters.json 里没有明文，有密文
	b, err := os.ReadFile(filepath.Join(e.dir, "clusters.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(pw)) || !bytes.Contains(b, []byte("mysqlPasswordEnc")) {
		t.Errorf("clusters.json: %s", b)
	}
	// 审计日志与服务日志里都不能出现
	ab, _ := os.ReadFile(filepath.Join(e.dir, "audit.log"))
	if bytes.Contains(ab, []byte(pw)) {
		t.Error("审计日志含密码")
	}
	if strings.Contains(e.logs.String(), pw) {
		t.Error("服务日志含密码")
	}
	var sum string
	for _, x := range e.auditEntries("") {
		if x["action"] == "cluster.add" {
			sum, _ = x["summary"].(string)
		}
	}
	if !strings.Contains(sum, "已保存数据库密码") {
		t.Errorf("审计摘要应说明保存了密码（不含密码本身）: %q", sum)
	}
	// 失败路径（启动失败）的错误与审计也不含密码
	e.mu.Lock()
	e.failPD["http://10.9.9.7:2379"] = true
	e.mu.Unlock()
	code, raw, _ = admin.raw("POST", "/api/clusters", map[string]any{"id": "liv2", "env": "test", "mode": "live", "pd": "http://10.9.9.7:2379", "mysqlHost": "h1", "mysqlUser": "u", "mysqlPassword": pw}, nil)
	ab, _ = os.ReadFile(filepath.Join(e.dir, "audit.log"))
	if code != 422 || bytes.Contains(raw, []byte(pw)) || bytes.Contains(ab, []byte(pw)) {
		t.Errorf("失败路径泄露: %d %s", code, raw)
	}
	// 重启后密码仍可解密（实例拿得到），说明加密落盘可逆
	e.shutdown()
	e2 := newHub(t, e.dir)
	_ = e2
	sp, ok := e2.hub.Registry().Get("liv")
	if !ok || sp.MySQLPassword != pw+"-2" {
		t.Errorf("重启后应能解密出密码: %v", ok)
	}
}

func TestHubClusterTestEndpoint(t *testing.T) {
	e := newHub(t, "")
	admin, dba := e.login("admin"), e.login("dba")
	e.addStgDev(admin)
	code, out := admin.do("POST", "/api/clusters/stg/test", nil)
	items, _ := out["items"].([]any)
	if code != 200 || out["ok"] != true || len(items) != 1 {
		t.Fatalf("回放测试: %d %v", code, out)
	}
	it := items[0].(map[string]any)
	if it["status"] != "ok" || !strings.Contains(it["detail"].(string), "stg-tidb-01") || it["ms"] == nil {
		t.Errorf("item: %v", it)
	}
	if code, o := admin.do("POST", "/api/clusters/nope/test", nil); code != 404 || o["code"] != "not_found" {
		t.Errorf("不存在: %d %v", code, o)
	}
	if code, _ := dba.do("POST", "/api/clusters/stg/test", nil); code != 403 {
		t.Errorf("无权限: %d", code)
	}
	var sawTest bool
	for _, x := range e.auditEntries("&cluster=stg") {
		if x["action"] == "cluster.test" {
			sawTest = true
		}
	}
	if !sawTest {
		t.Error("缺少 cluster.test 审计")
	}
}

func TestHubRestartRestoresClusters(t *testing.T) {
	e := newHub(t, "")
	admin, admin2, dba := e.login("admin"), e.login("admin2"), e.login("dba")
	e.addStgDev(admin)
	e.enableWrite(admin, admin2, "stg")
	code, ch := dba.on("stg").do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-1", "action": "restart"}})
	if code != 200 {
		t.Fatalf("创建变更: %d %v", code, ch)
	}
	admin.on("dev").do("POST", "/api/ops/settings", map[string]any{"lb": true})
	if code, _ := admin.do("POST", "/api/clusters/stg", map[string]any{"display": "预发（重启测试）"}); code != 200 {
		t.Fatal("update")
	}
	e.shutdown()

	e2 := newHub(t, e.dir)
	dba2, admin3 := e2.login("dba"), e2.login("admin")
	_, l := admin3.do("GET", "/api/clusters", nil)
	got := map[string]string{}
	for _, c := range l["clusters"].([]any) {
		m := c.(map[string]any)
		got[m["id"].(string)] = fmt.Sprint(m["display"], "|", m["state"], "|", m["env"])
	}
	if got["stg"] != "预发（重启测试）|ok|staging" || got["dev"] != "dev 集群|ok|test" || len(got) != 3 {
		t.Errorf("重启后集群列表: %v", got)
	}
	if _, o := dba2.on("stg").do("GET", "/api/changes", nil); len(o["changes"].([]any)) != 1 {
		t.Errorf("stg 的变更应随重启恢复: %v", o)
	}
	if _, o := dba2.on("dev").do("GET", "/api/changes", nil); len(o["changes"].([]any)) != 0 {
		t.Errorf("dev 不应有变更: %v", o)
	}
	_, st := admin3.on("dev").do("GET", "/api/ops/settings", nil)
	if dg(st, "settings", "lb") != true {
		t.Errorf("dev 的设置应随重启恢复: %v", st)
	}
	_, me := dba2.on("stg").do("GET", "/api/auth/me", nil)
	if dg(me, "write", "enabled") != true {
		t.Errorf("stg 的写开关应随重启恢复: %v", me["write"])
	}
	_, me = dba2.on("dev").do("GET", "/api/auth/me", nil)
	if dg(me, "write", "enabled") != false {
		t.Errorf("dev 的写开关应保持关闭: %v", me["write"])
	}
	// 重启后 clusters.json 里某个集群的 fixtures 失效 → 该集群 error，其他不受影响
	e2.shutdown()
	b, _ := os.ReadFile(filepath.Join(e.dir, "clusters.json"))
	bad := strings.ReplaceAll(string(b), "fixtures"+string(filepath.Separator)+"dev", "fixtures"+string(filepath.Separator)+"gone")
	bad = strings.ReplaceAll(bad, strings.ReplaceAll(filepath.Join(e.fx, "dev"), `\`, `\\`), strings.ReplaceAll(filepath.Join(e.fx, "gone"), `\`, `\\`))
	if bad == string(b) {
		t.Fatalf("没有替换成功: %s", b)
	}
	_ = os.WriteFile(filepath.Join(e.dir, "clusters.json"), []byte(bad), 0o600)
	e3 := newHub(t, e.dir)
	admin4 := e3.login("admin")
	_, l = admin4.do("GET", "/api/clusters", nil)
	states := map[string]string{}
	for _, c := range l["clusters"].([]any) {
		m := c.(map[string]any)
		states[m["id"].(string)] = m["state"].(string)
	}
	if states["dev"] != "error" || states["stg"] != "ok" || states["prod-tidb-01"] != "ok" {
		t.Errorf("单个集群失败不应影响其他: %v", states)
	}
	if code, o := admin4.on("dev").do("GET", "/api/snapshot", nil); code != 503 || o["code"] != "cluster_unavailable" {
		t.Errorf("error 状态的集群应 503: %d %v", code, o)
	}
	// 启动错误的细节（可能含地址 / 路径）只给管理员：普通用户在 503 响应、集群列表、me 里都看不到
	if _, o := admin4.on("dev").do("GET", "/api/snapshot", nil); strings.Contains(fmt.Sprint(o), "gone") {
		t.Errorf("503 响应不应泄露错误细节: %v", o)
	}
	dba3 := e3.login("dba")
	_, dl := dba3.do("GET", "/api/clusters", nil)
	if strings.Contains(fmt.Sprint(dl), "gone") || strings.Contains(fmt.Sprint(dl), "loadError") {
		t.Errorf("普通用户的集群列表不应含启动错误细节: %v", dl)
	}
	_, ml := dba3.do("GET", "/api/auth/me", nil)
	if strings.Contains(fmt.Sprint(ml["clusters"]), "gone") {
		t.Errorf("me.clusters 不应含启动错误细节: %v", ml["clusters"])
	}
	_, al := admin4.do("GET", "/api/clusters", nil)
	if !strings.Contains(fmt.Sprint(al), "loadError") {
		t.Errorf("管理员应能看到加载错误: %v", al)
	}
	var sawLoad bool
	for _, x := range e3.auditEntries("") {
		if x["action"] == "cluster.load" && x["result"] == "failed" {
			sawLoad = true
		}
	}
	if !sawLoad {
		t.Error("启动失败应写 cluster.load 审计")
	}
}

func TestHubConcurrentAddRemoveAndRequests(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	dba := e.login("dba")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad sync.Map
	// 读者：反复访问固定的集群与列表
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids := []string{"", "stg", "dev", "tmp-a", "tmp-b", "nope"}
			c := dba.on("")
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				id := ids[(n+i)%len(ids)]
				c.cluster = id
				code, _, _ := c.raw("GET", "/api/topology", nil, nil)
				switch code {
				case 200, 404, 503:
				default:
					bad.Store(fmt.Sprintf("topology %q → %d", id, code), true)
				}
				code, _, _ = c.raw("GET", "/api/clusters", nil, nil)
				if code != 200 {
					bad.Store(fmt.Sprintf("/api/clusters → %d", code), true)
				}
			}
		}(i)
	}
	// 写者：不同 id 并发添加 / 删除（Hub 内部串行）
	var wmu sync.WaitGroup
	for w := 0; w < 3; w++ {
		wmu.Add(1)
		go func(w int) {
			defer wmu.Done()
			id := []string{"tmp-a", "tmp-b", "tmp-c"}[w]
			for n := 0; n < 4; n++ {
				if _, _, err := e.hub.Add(clusters.Input{ID: id, Env: ptr("staging"), Mode: ptr("replay"), Fixtures: ptr(filepath.Join(e.fx, "dev"))}, "t"); err != nil {
					bad.Store("add "+id+": "+err.Error(), true)
					return
				}
				if _, _, err := e.hub.Remove(id); err != nil {
					bad.Store("remove "+id+": "+err.Error(), true)
					return
				}
			}
		}(w)
	}
	wmu.Wait()
	close(stop)
	wg.Wait()
	bad.Range(func(k, _ any) bool { t.Errorf("并发异常：%v", k); return true })
	_, l := admin.do("GET", "/api/clusters", nil)
	if n := len(l["clusters"].([]any)); n != 3 {
		t.Errorf("并发添加 / 删除后应只剩 3 个集群，得到 %d", n)
	}
	// 注册表与实例表一致
	if got := len(e.hub.Registry().List()); got != 3 {
		t.Errorf("注册表 %d 个", got)
	}
	for _, id := range []string{"tmp-a", "tmp-b", "tmp-c"} {
		if e.hub.Instance(id) != nil {
			t.Errorf("%s 应已移除", id)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// rejectRunner 模拟特权端拒绝：集群名不在策略文件里。
type rejectRunner struct{}

func (rejectRunner) Run(_ context.Context, req exec.Request, _ func(string)) (exec.Result, error) {
	err := fmt.Errorf("集群 %q 不在执行器策略的 clusters 白名单里", req.Cmd.Cluster)
	return exec.Result{Exit: -1, Err: err.Error()}, err
}

func TestHubRunnerPreflightWarnsInsteadOfSilence(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	// sudo 模式下特权端策略文件不含新集群名：预检被拒，API 返回可读警告
	if code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "ghost", "name": "ghost-tidb", "env": "staging", "mode": "replay", "fixtures": filepath.Join(e.fx, "stg")}); code != 200 {
		t.Fatalf("add: %d %v", code, out)
	}
	s := e.hub.Instance("ghost")
	if s == nil {
		t.Fatal("实例应已启动")
	}
	ws := server.RunnerPreflight(context.Background(), s, "ghost-tidb")
	if len(ws) != 0 {
		t.Errorf("模拟执行器不应有预检警告: %v", ws)
	}
	// 直接用一个会拒绝的执行器验证文案
	srv2 := e.newServerWithRunner(rejectRunner{}, "sudo")
	ws = server.RunnerPreflight(context.Background(), srv2, "ghost-tidb")
	if len(ws) != 1 || !strings.Contains(ws[0], "--exec-policy") || !strings.Contains(ws[0], "ghost-tidb") || !strings.Contains(ws[0], "预检") {
		t.Errorf("sudo 预检警告应指向策略文件并包含集群名: %v", ws)
	}
	// 没有执行器 / 模拟执行器：不预检
	if ws := server.RunnerPreflight(context.Background(), e.newServerWithRunner(rejectRunner{}, "sim"), "x"); len(ws) != 0 {
		t.Errorf("sim 不预检: %v", ws)
	}
}

// newServerWithRunner 造一个独立的、启用运维模式的实例（只用于验证预检文案）。
func (e *hubEnv) newServerWithRunner(r exec.Runner, mode string) *server.Server {
	e.t.Helper()
	rp, err := source.NewReplay(filepath.Join(e.fx, "dev"))
	if err != nil {
		e.t.Fatal(err)
	}
	s := server.New(server.Config{Col: collect.New(rp, source.CatalogParams{Cluster: "x", Now: time.Now()}), Mode: "replay", Web: e.t.TempDir(), Logf: func(string, ...any) {}})
	if err := s.EnableOps(server.OpsConfig{Dir: e.t.TempDir(), Env: "test", Users: e.users, Audit: e.audit, Runner: r, Key: opsKey, MasterKey: hubMaster, ExecMode: mode,
		ReadOnlyFile: filepath.Join(e.t.TempDir(), "RO"), TickEvery: time.Hour, NotifyWatchEvery: -1}); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(s.Close)
	return s
}

func TestHubUpdateRebuildsInstanceAndKeepsData(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	if code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "liv", "name": "liv-tidb", "env": "staging", "mode": "live", "pd": "http://10.9.9.9:2379"}); code != 200 {
		t.Fatalf("add: %d %v", code, out)
	}
	e.mu.Lock()
	r1 := e.runners["liv"]
	e.mu.Unlock()
	// 状态摘要：没访问过之前未采集；访问一次后可连
	status := func() map[string]any {
		_, l := admin.do("GET", "/api/clusters", nil)
		for _, c := range l["clusters"].([]any) {
			if c.(map[string]any)["id"] == "liv" {
				return c.(map[string]any)["status"].(map[string]any)
			}
		}
		return nil
	}
	if st := status(); st["reachable"] != nil || st["state"] != "ok" {
		t.Errorf("尚未采集: %v", st)
	}
	if code, _ := admin.on("liv").do("GET", "/api/snapshot", nil); code != 200 {
		t.Fatal("snapshot")
	}
	if st := status(); st["reachable"] != true || st["snapshotAt"] == nil || st["nodes"].(float64) == 0 {
		t.Errorf("采集后应可连: %v", st)
	}
	admin.on("liv").do("POST", "/api/ops/settings", map[string]any{"lb": true})
	code, out := admin.do("POST", "/api/clusters/liv", map[string]any{"pd": "http://10.9.9.8:2379", "desc": "换了 PD"})
	if code != 200 || out["ok"] != true {
		t.Fatalf("update: %d %v", code, out)
	}
	if ch, _ := out["changed"].([]any); len(ch) != 2 {
		t.Errorf("changed: %v", out["changed"])
	}
	e.mu.Lock()
	r2 := e.runners["liv"]
	e.mu.Unlock()
	if r1 == r2 {
		t.Error("连接信息变化后应重建实例（新的执行器）")
	}
	_, st := admin.on("liv").do("GET", "/api/ops/settings", nil)
	if dg(st, "settings", "lb") != true {
		t.Errorf("重建后运行态数据应保留: %v", st)
	}
	// 只改 display：不重建
	code, _ = admin.do("POST", "/api/clusters/liv", map[string]any{"display": "新名字"})
	e.mu.Lock()
	r3 := e.runners["liv"]
	e.mu.Unlock()
	if code != 200 || r3 != r2 {
		t.Errorf("只改 display 不应重建: %d", code)
	}
	// 改 name（tiup 集群名）会让执行器策略随之改变：重建后新名字生效
	if code, out := admin.do("POST", "/api/clusters/liv", map[string]any{"name": "liv-tidb-2"}); code != 200 {
		t.Fatalf("改 name: %d %v", code, out)
	}
	if code, out := admin.do("POST", "/api/clusters/liv", map[string]any{"name": "prod-tidb-01"}); code != 409 || out["code"] != "exists" {
		t.Errorf("name 撞主集群: %d %v", code, out)
	}
}

// 写开关的双人确认是每个集群各自的：在 A 上发起的提议，不能靠在 B 上点「确认」生效。
func TestHubWriteSwitchProposalsAreIsolated(t *testing.T) {
	e := newHub(t, "")
	admin, admin2 := e.login("admin"), e.login("admin2")
	e.addStgDev(admin)
	if code, out := admin.on("dev").do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 200 || out["pending"] == nil {
		t.Fatalf("dev 发起提议: %d %v", code, out)
	}
	pending := func(id string) (enabled, pend any) {
		_, m := admin.on(id).do("GET", "/api/auth/me", nil)
		return dg(m, "write", "enabled"), dg(m, "write", "pending") != nil
	}
	if en, p := pending("dev"); en != false || p != true {
		t.Errorf("dev 应有待确认的提议: %v %v", en, p)
	}
	// admin2 在主集群 / stg 上「确认」：只会各自发起新的提议，dev 不受影响
	for _, id := range []string{"", "stg"} {
		code, out := admin2.on(id).do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
		if code != 200 || out["enabled"] != false || out["pending"] == nil {
			t.Errorf("集群 %q 上应是新的提议而不是生效: %d %v", id, code, out)
		}
	}
	if en, p := pending("dev"); en != false || p != true {
		t.Errorf("dev 不应被其他集群上的操作确认: %v %v", en, p)
	}
	for _, id := range []string{"", "stg"} {
		if en, p := pending(id); en != false || p != true {
			t.Errorf("集群 %q: enabled=%v pending=%v", id, en, p)
		}
	}
	// 在 dev 上由另一名管理员确认才生效，且只影响 dev
	if code, out := admin2.on("dev").do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 200 || out["enabled"] != true {
		t.Fatalf("dev 确认: %d %v", code, out)
	}
	if en, _ := pending("dev"); en != true {
		t.Error("dev 应已开启")
	}
	for _, id := range []string{"", "stg"} {
		if en, _ := pending(id); en != false {
			t.Errorf("集群 %q 不应被开启", id)
		}
	}
	// 关闭（熔断）同样只影响自己
	admin.on("dev").do("POST", "/api/ops/write-switch", map[string]any{"enabled": false})
	if en, _ := pending("dev"); en != false {
		t.Error("dev 应已关闭")
	}
}

// 通知文案里的环境 / 集群是该通知所属集群的值，且各集群的渠道互不串。
func TestHubNotificationsCarryOwnClusterAndEnv(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	hs, hd := newHook(t), newHook(t)
	add := func(id string, h *hook) string {
		code, out := admin.on(id).do("POST", "/api/ops/notify/channels", map[string]any{"type": "wecom", "name": id + "-群", "url": h.url()})
		if code != 200 {
			t.Fatalf("加渠道 %s: %d %v", id, code, out)
		}
		return out["id"].(string)
	}
	sid, did := add("stg", hs), add("dev", hd)
	if code, out := admin.on("stg").do("POST", "/api/ops/notify/channels/"+sid+"/test", nil); code != 200 || out["ok"] != true {
		t.Fatalf("测试 stg 渠道: %d %v", code, out)
	}
	if code, out := admin.on("dev").do("POST", "/api/ops/notify/channels/"+did+"/test", nil); code != 200 || out["ok"] != true {
		t.Fatalf("测试 dev 渠道: %d %v", code, out)
	}
	m := hs.waitFor(t, "stg-tidb-01")
	if !strings.Contains(m, "环境：staging") || strings.Contains(m, "dev-tidb-01") {
		t.Errorf("stg 的通知: %s", m)
	}
	m = hd.waitFor(t, "dev-tidb-01")
	if !strings.Contains(m, "环境：test") || strings.Contains(m, "stg-tidb-01") {
		t.Errorf("dev 的通知: %s", m)
	}
	// 在 stg 上用 dev 的渠道 id：找不到（渠道按集群隔离）
	if code, _ := admin.on("stg").do("POST", "/api/ops/notify/channels/"+did+"/test", nil); code != 404 {
		t.Errorf("跨集群使用渠道应 404: %d", code)
	}
	// 非主集群的变更事件通知：文案是该集群的 env / cluster
	admin2, dba := e.login("admin2"), e.login("dba")
	e.enableWrite(admin, admin2, "stg")
	if code, out := admin.on("stg").do("POST", "/api/ops/notify/channels/"+sid, map[string]any{"type": "wecom", "name": "stg-群", "events": []string{"change.pending"}}); code != 200 {
		t.Fatalf("订阅事件: %d %v", code, out)
	}
	code, ch := dba.on("stg").do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-1", "action": "restart"}})
	if code != 200 || ch["state"] != "pending" {
		t.Skipf("变更没有停在待审批：%d %v", code, ch["state"])
	}
	m = hs.waitFor(t, "有变更等待审批")
	if !strings.Contains(m, "stg-tidb-01") || !strings.Contains(m, "环境：staging") || !strings.Contains(m, "https://ops.example.com/?cluster=stg") {
		t.Errorf("变更通知: %s", m)
	}
}
