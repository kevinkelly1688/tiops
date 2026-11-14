// @author: kevinkelly
// @license: Apache License 2.0
// @file: host_test.go
// @time: 2026/10/28

package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

// hostTestClock 是可调的测试时钟（同时给平台与变更引擎使用）。
type hostTestClock struct{ ns atomic.Int64 }

func newHostTestClock() *hostTestClock {
	c := &hostTestClock{}
	c.ns.Store(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *hostTestClock) Now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *hostTestClock) Add(d time.Duration) { c.ns.Add(int64(d)) }

// setupHostOps 与 setupOps 相同，但数据目录可指定（用来模拟重启后从磁盘恢复）、可不配置执行器、可注入时钟。
func setupHostOps(t *testing.T, dir string, withRunner bool, clk *hostTestClock) *opsEnv {
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

	users, err := authz.OpenStore(filepath.Join(dir, "users.json"), []byte("master-key-for-test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := authz.SeedDemoUsers(users); err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	cfg := server.OpsConfig{Dir: dir, Env: "staging", Users: users, Audit: al, Prober: &change.SimProber{Polls: 1}, Key: opsKey, ReadOnlyFile: filepath.Join(dir, "READONLY"), Sim: true, ExecMode: "sim", WaitScale: 0.001, TickEvery: time.Hour}
	if clk != nil {
		cfg.Now = clk.Now
	}
	var sim *exec.SimRunner
	if withRunner {
		pol := exec.Policy{Clusters: []string{"prod-tidb-01"}, TempDir: t.TempDir(), AlertURL: "http://127.0.0.1:1", MySQLUser: "tidbops_ops", BackupRoots: []string{"local:///backup/"}}
		sim = &exec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: opsKey}
		cfg.Runner = sim
	}
	if err := s.EnableOps(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &opsEnv{ts: ts, dir: dir, srv: s, sim: sim, audit: al}
}

func hostList(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	m := map[string]map[string]any{}
	hs, _ := out["hosts"].([]any)
	for _, h := range hs {
		hm := h.(map[string]any)
		m[hm["ip"].(string)] = hm
	}
	return m
}

func hostInCluster(out map[string]any) map[string]bool {
	m := map[string]bool{}
	ips, _ := out["inCluster"].([]any)
	for _, ip := range ips {
		m[ip.(string)] = true
	}
	return m
}

func hostAuditActions(t *testing.T, e *opsEnv, action string) []audit.Entry {
	t.Helper()
	es, err := e.audit.Query(audit.Filter{Action: action}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// ---------------------------------------------------------------- 主机规格 ----

func TestHostsAPIReadOnlyAndOps(t *testing.T) {
	check := func(t *testing.T, out map[string]any) {
		t.Helper()
		hosts, _ := out["hosts"].([]any)
		if len(hosts) != 16 {
			t.Fatalf("应有 16 台主机: %d", len(hosts))
		}
		var kv map[string]any
		for _, h := range hosts {
			if hm := h.(map[string]any); hm["ip"] == "10.0.2.11" {
				kv = hm
			}
		}
		if kv == nil || kv["cores"].(float64) != 16 || kv["memGB"].(float64) != 64 || kv["os"] != "Rocky Linux 9.3 (Blue Onyx)" ||
			kv["kernel"] != "5.14.0-362.8.1.el9_3.x86_64" || kv["diskGB"].(float64) != 3584 || kv["known"] != true || kv["az"] != "az1" || kv["rack"] != "r01" {
			t.Errorf("TiKV 主机: %v", kv)
		}
		if insts, _ := kv["instances"].([]any); len(insts) != 1 || insts[0] != "tikv-1" {
			t.Errorf("实例: %v", kv["instances"])
		}
		if note, ok := out["note"].(string); !ok || note != "" {
			t.Errorf("数据完整时 note 应为空串: %v", out["note"])
		}
	}
	// 只读模式：无需登录
	ts, _ := setup(t)
	code, body, h := get(t, ts.URL+"/api/hosts")
	if code != 200 || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("/api/hosts: %d %.200s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	check(t, out)
	// 运维模式：未登录 401，访客可读（与总览同一能力）
	e := setupOps(t)
	if code, _ := e.client(t).do("GET", "/api/hosts", nil); code != 401 {
		t.Errorf("未登录应 401: %d", code)
	}
	code, o2 := e.login(t, "viewer").do("GET", "/api/hosts", nil)
	if code != 200 {
		t.Fatalf("访客读主机规格: %d %v", code, o2)
	}
	check(t, o2)
}

func TestTopologyJSONCarriesDirs(t *testing.T) {
	ts, _ := setup(t)
	_, body, _ := get(t, ts.URL+"/api/topology")
	var topo struct {
		Instances []map[string]any `json:"instances"`
	}
	if err := json.Unmarshal([]byte(body), &topo); err != nil {
		t.Fatal(err)
	}
	for _, in := range topo.Instances {
		if in["id"] == "tikv-1" && (in["dataDir"] != "/tidb-data/tikv-20160" || in["deployDir"] != "/tidb-deploy/tikv-20160" || in["logDir"] != "/tidb-log/tikv-20160") {
			t.Errorf("tikv-1 目录: %v", in)
		}
		if in["id"] == "tikv-2" && (in["logDir"] != "/tidb-deploy/tikv-20160/log" || in["logDirInferred"] != true) {
			t.Errorf("tikv-2 日志目录应为推断值: %v", in)
		}
	}
}

// ---------------------------------------------------------------- 主机池 ----

func TestHostPoolCRUDAndValidation(t *testing.T) {
	e := setupOps(t)
	dba := e.login(t, "dba")

	code, out := dba.do("GET", "/api/hostpool", nil)
	if code != 200 || len(hostList(t, out)) != 0 {
		t.Fatalf("初始为空: %d %v", code, out)
	}
	if ic := hostInCluster(out); !ic["10.0.2.11"] || !ic["10.0.1.21"] || ic["10.0.2.17"] || len(ic) != 16 {
		t.Errorf("inCluster 应是拓扑里的 16 台主机: %v", out["inCluster"])
	}

	// 新增
	code, out = dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.17", "az": "az1", "rack": "r07", "note": "已采购，待加入"})
	if code != 200 || out["ok"] != true {
		t.Fatalf("新增: %d %v", code, out)
	}
	h := out["host"].(map[string]any)
	if h["ip"] != "10.0.2.17" || h["az"] != "az1" || h["rack"] != "r07" || h["note"] != "已采购，待加入" || h["createdBy"] != "dba" || h["lastCheck"] != nil {
		t.Errorf("新增结果: %v", h)
	}
	if ts, _ := h["createdAt"].(string); ts == "" {
		t.Errorf("缺少 createdAt: %v", h)
	}
	_, out = dba.do("GET", "/api/hostpool", nil)
	if l := hostList(t, out); len(l) != 1 || l["10.0.2.17"] == nil {
		t.Errorf("列表: %v", out)
	}

	// 校验：非法 IP、集群内主机、重复、az / rack / 备注
	bad := []struct {
		name string
		body map[string]any
		want int
	}{
		{"非 IP", map[string]any{"ip": "not-an-ip"}, 400},
		{"缺段", map[string]any{"ip": "10.0.2"}, 400},
		{"前导零", map[string]any{"ip": "010.000.002.018"}, 400},
		{"IPv6", map[string]any{"ip": "::1"}, 400},
		{"回环", map[string]any{"ip": "127.0.0.1"}, 400},
		{"未指定", map[string]any{"ip": "0.0.0.0"}, 400},
		{"组播", map[string]any{"ip": "224.0.0.1"}, 400},
		{"带端口", map[string]any{"ip": "10.0.2.18:20160"}, 400},
		{"空", map[string]any{"ip": ""}, 400},
		{"已在拓扑里", map[string]any{"ip": "10.0.2.11"}, 409},
		{"重复", map[string]any{"ip": "10.0.2.17"}, 409},
		{"az 含空格", map[string]any{"ip": "10.0.2.18", "az": "a z"}, 400},
		{"az 含斜杠", map[string]any{"ip": "10.0.2.18", "az": "az/1"}, 400},
		{"az 过长", map[string]any{"ip": "10.0.2.18", "az": strings.Repeat("a", 33)}, 400},
		{"rack 含引号", map[string]any{"ip": "10.0.2.18", "rack": `r"1`}, 400},
		{"rack 中文", map[string]any{"ip": "10.0.2.18", "rack": "机架1"}, 400},
		{"备注含换行", map[string]any{"ip": "10.0.2.18", "note": "a\nb"}, 400},
		{"备注过长", map[string]any{"ip": "10.0.2.18", "note": strings.Repeat("好", 201)}, 400},
	}
	for _, b := range bad {
		if code, out := dba.do("POST", "/api/hostpool", b.body); code != b.want {
			t.Errorf("%s: %d %v，want %d", b.name, code, out, b.want)
		}
	}
	if code, _ := dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.18", "az": strings.Repeat("a", 32), "rack": "R-1_x.y"}); code != 200 {
		t.Errorf("32 字符 az 与合法 rack 应通过: %d", code)
	}
	if _, out = dba.do("GET", "/api/hostpool", nil); len(hostList(t, out)) != 2 {
		t.Errorf("失败的新增不应写入: %v", out)
	}

	// 更新：只改给出的字段
	code, out = dba.do("POST", "/api/hostpool/10.0.2.17", map[string]any{"note": "改过的备注"})
	if h := out["host"].(map[string]any); code != 200 || h["note"] != "改过的备注" || h["az"] != "az1" || h["rack"] != "r07" {
		t.Errorf("部分更新: %d %v", code, out)
	}
	code, out = dba.do("POST", "/api/hostpool/10.0.2.17", map[string]any{"az": "az2", "rack": "r08"})
	if h := out["host"].(map[string]any); code != 200 || h["az"] != "az2" || h["rack"] != "r08" || h["note"] != "改过的备注" {
		t.Errorf("更新 az / rack: %d %v", code, out)
	}
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.17", map[string]any{"az": "bad az"}); code != 400 {
		t.Errorf("更新时同样校验: %d", code)
	}
	if _, out = dba.do("GET", "/api/hostpool", nil); hostList(t, out)["10.0.2.17"]["az"] != "az2" {
		t.Errorf("非法更新不应生效: %v", out)
	}
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.99", map[string]any{"note": "x"}); code != 404 {
		t.Errorf("不存在应 404: %d", code)
	}
	if code, _ := dba.do("POST", "/api/hostpool/not-an-ip", map[string]any{"note": "x"}); code != 400 {
		t.Errorf("路径里的 IP 非法应 400: %d", code)
	}

	// 删除
	if code, out := dba.do("POST", "/api/hostpool/10.0.2.18/delete", nil); code != 200 || out["ok"] != true {
		t.Errorf("删除: %d %v", code, out)
	}
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.18/delete", nil); code != 404 {
		t.Errorf("重复删除应 404: %d", code)
	}
	if _, out = dba.do("GET", "/api/hostpool", nil); len(hostList(t, out)) != 1 {
		t.Errorf("删除后: %v", out)
	}

	// 审计
	for _, a := range []string{"hostpool.add", "hostpool.update", "hostpool.delete"} {
		es := hostAuditActions(t, e, a)
		if len(es) == 0 || es[0].User != "dba" || es[0].Result != "success" {
			t.Errorf("缺少审计 %s: %+v", a, es)
		}
	}
	if es := hostAuditActions(t, e, "hostpool.add"); len(es) != 2 || es[0].Target != "10.0.2.18" {
		t.Errorf("hostpool.add 应有 2 条（两次成功新增）: %+v", es)
	}
}

func TestHostPoolPermissions(t *testing.T) {
	e := setupOps(t)
	anon := e.client(t)
	if code, _ := anon.do("GET", "/api/hostpool", nil); code != 401 {
		t.Errorf("未登录读主机池应 401: %d", code)
	}
	if code, _ := anon.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.17"}); code != 401 {
		t.Errorf("未登录新增应 401: %d", code)
	}
	if code, _ := e.login(t, "dba").do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.17"}); code != 200 {
		t.Fatalf("dba 新增: %d", code)
	}
	// 只要登录就能看（访客、SRE）；没有扩缩容权限的角色不能写
	for _, u := range []string{"viewer", "sre", "auditor", "dev", "approver"} {
		c := e.login(t, u)
		if code, out := c.do("GET", "/api/hostpool", nil); code != 200 || len(hostList(t, out)) != 1 {
			t.Errorf("%s 应能读主机池: %d %v", u, code, out)
		}
		for name, call := range map[string]func() int{
			"新增": func() int { code, _ := c.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.18"}); return code },
			"更新": func() int {
				code, _ := c.do("POST", "/api/hostpool/10.0.2.17", map[string]any{"note": "x"})
				return code
			},
			"删除": func() int { code, _ := c.do("POST", "/api/hostpool/10.0.2.17/delete", nil); return code },
			"预检": func() int {
				code, _ := c.do("POST", "/api/hostpool/10.0.2.17/check", map[string]any{"role": "tikv"})
				return code
			},
		} {
			if got := call(); got != 403 {
				t.Errorf("%s 不应能%s主机池: %d", u, name, got)
			}
		}
	}
	// 没有 CSRF 令牌的写请求被拒
	dba := e.login(t, "dba")
	dba.csrf = ""
	if code, _ := dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.18"}); code != 403 {
		t.Errorf("缺 CSRF 应 403: %d", code)
	}
	if es := hostAuditActions(t, e, "authz.deny"); len(es) < 20 {
		t.Errorf("越权尝试应写审计: %d 条", len(es))
	}
	// 越权没有改动数据
	_, out := e.login(t, "dba").do("GET", "/api/hostpool", nil)
	if l := hostList(t, out); len(l) != 1 || l["10.0.2.17"]["note"] != "" {
		t.Errorf("越权调用不应改动主机池: %v", out)
	}
}

// ---------------------------------------------------------------- 预检 ----

func TestHostPoolCheckWithSimRunner(t *testing.T) {
	e := setupOps(t)
	dba := e.login(t, "dba")
	for _, ip := range []string{"10.0.2.17", "10.0.2.250", "10.0.2.251"} {
		if code, out := dba.do("POST", "/api/hostpool", map[string]any{"ip": ip, "az": "az1", "rack": "r01"}); code != 200 {
			t.Fatalf("新增 %s: %d %v", ip, code, out)
		}
	}
	items := func(out map[string]any) map[string]map[string]any {
		m := map[string]map[string]any{}
		lc, _ := out["lastCheck"].(map[string]any)
		for _, it := range lc["items"].([]any) {
			im := it.(map[string]any)
			m[im["n"].(string)] = im
		}
		return m
	}

	// 通过的主机
	code, out := dba.do("POST", "/api/hostpool/10.0.2.17/check", map[string]any{"role": "tikv"})
	if code != 200 || out["ok"] != true {
		t.Fatalf("预检 10.0.2.17: %d %v", code, out)
	}
	lc := out["lastCheck"].(map[string]any)
	if lc["role"] != "tikv" || lc["ok"] != true || lc["at"] == "" || len(items(out)) < 5 {
		t.Errorf("lastCheck: %v", lc)
	}
	for n, it := range items(out) {
		if it["r"] != "ok" || it["fix"] != "" {
			t.Errorf("通过的主机 %s: %v", n, it)
		}
	}
	// 请求里只含这一台主机
	calls := e.sim.Calls()
	last := calls[len(calls)-1].Cmd
	if last.Kind != exec.KindClusterCheck || !strings.Contains(last.Content, "host: 10.0.2.17") || strings.Count(last.Content, "- host:") != 1 ||
		!strings.Contains(last.Content, "tikv_servers:") || !strings.Contains(last.Content, `zone: "az1"`) {
		t.Errorf("预检命令: %+v", last)
	}

	// swap Fail + THP Warn：ok=false，带 fix
	code, out = dba.do("POST", "/api/hostpool/10.0.2.250/check", map[string]any{"role": "tidb"})
	if code != 200 || out["ok"] != false {
		t.Fatalf("预检 10.0.2.250: %d %v", code, out)
	}
	it := items(out)
	if it["swap"]["r"] != "bad" || !strings.Contains(it["swap"]["fix"].(string), "swapoff -a") || !strings.Contains(it["swap"]["d"].(string), "swap is enabled") {
		t.Errorf("swap: %v", it["swap"])
	}
	if it["thp"]["r"] != "warn" || it["thp"]["fix"] != "echo never > /sys/kernel/mm/transparent_hugepage/enabled" {
		t.Errorf("thp: %v", it["thp"])
	}
	if it["os-version"]["r"] != "ok" {
		t.Errorf("os-version: %v", it["os-version"])
	}
	if last := e.sim.Calls(); !strings.Contains(last[len(last)-1].Cmd.Content, "tidb_servers:") {
		t.Errorf("role=tidb 应生成 tidb_servers")
	}

	// 只有警告：ok 仍为 true（不阻止扩容）；多行同一检查项被归并
	code, out = dba.do("POST", "/api/hostpool/10.0.2.251/check", map[string]any{"role": "tikv"})
	it = items(out)
	if code != 200 || out["ok"] != false || it["limits"]["r"] != "bad" || it["disk"]["r"] != "warn" ||
		!strings.Contains(it["limits"]["fix"].(string), "tidb hard nofile 1000000") || it["disk"]["fix"] != "mount -o remount,nodelalloc /tidb-data" ||
		it["sysctl"]["fix"] != "sysctl -w net.core.somaxconn=32768" {
		t.Errorf("251: %d %v", code, it)
	}

	// 结果已保存在主机池里
	_, pool := dba.do("GET", "/api/hostpool", nil)
	l := hostList(t, pool)
	if lc, _ := l["10.0.2.250"]["lastCheck"].(map[string]any); lc == nil || lc["ok"] != false || lc["role"] != "tidb" {
		t.Errorf("lastCheck 应保存: %v", l["10.0.2.250"])
	}
	if l["10.0.2.17"]["lastCheck"] == nil {
		t.Errorf("10.0.2.17 的 lastCheck: %v", l["10.0.2.17"])
	}

	// 错误请求
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.17/check", map[string]any{"role": "mysql"}); code != 400 {
		t.Errorf("非法角色应 400: %d", code)
	}
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.17/check", map[string]any{}); code != 400 {
		t.Errorf("缺少角色应 400: %d", code)
	}
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.77/check", map[string]any{"role": "tikv"}); code != 404 {
		t.Errorf("不在主机池里应 404: %d", code)
	}
	// 没有可用区的主机池条目做 TiKV 预检：用占位 zone，仍可运行
	dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.30"})
	if code, out := dba.do("POST", "/api/hostpool/10.0.2.30/check", map[string]any{"role": "tikv"}); code != 200 || out["ok"] != true {
		t.Errorf("没有 az 的 TiKV 预检: %d %v", code, out)
	}

	// 审计：读命令，风险 low
	es := hostAuditActions(t, e, "hostpool.check")
	if len(es) != 4 || es[0].Risk != "low" || es[0].Result != "success" || es[0].User != "dba" {
		t.Errorf("hostpool.check 审计: %+v", es)
	}
}

func TestHostPoolCheckWithoutRunner(t *testing.T) {
	e := setupHostOps(t, t.TempDir(), false, nil)
	dba := e.login(t, "dba")
	dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.17", "az": "az1"})
	code, out := dba.do("POST", "/api/hostpool/10.0.2.17/check", map[string]any{"role": "tikv"})
	if code != 503 || !strings.Contains(out["error"].(string), "执行器") {
		t.Errorf("没有执行器应返回 503 与清晰的原因: %d %v", code, out)
	}
	_, pool := dba.do("GET", "/api/hostpool", nil)
	if hostList(t, pool)["10.0.2.17"]["lastCheck"] != nil {
		t.Errorf("失败的预检不应保存结果: %v", pool)
	}
	// 主机池本身的增删改不依赖执行器
	if code, _ := dba.do("POST", "/api/hostpool/10.0.2.17", map[string]any{"note": "x"}); code != 200 {
		t.Errorf("更新不依赖执行器: %d", code)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func TestHostPoolPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	e1 := setupHostOps(t, dir, true, nil)
	dba := e1.login(t, "dba")
	dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.17", "az": "az1", "rack": "r07", "note": "n1"})
	dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.250", "az": "az2", "rack": "r12"})
	dba.do("POST", "/api/hostpool/10.0.2.250/check", map[string]any{"role": "tikv"})
	dba.do("POST", "/api/hostpool", map[string]any{"ip": "10.0.2.99"})
	dba.do("POST", "/api/hostpool/10.0.2.99/delete", nil)

	// 文件：0600，没有残留临时文件
	p := filepath.Join(dir, "hostpool.json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("应写出 hostpool.json: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("hostpool.json 权限 %v，应为 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Error("不应残留临时文件")
	}
	var onDisk struct {
		Hosts []map[string]any `json:"hosts"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &onDisk); err != nil || len(onDisk.Hosts) != 2 {
		t.Fatalf("文件内容: %v %s", err, b)
	}

	// 「重启」：新服务实例读同一数据目录
	e2 := setupHostOps(t, dir, true, nil)
	_, out := e2.login(t, "dba").do("GET", "/api/hostpool", nil)
	l := hostList(t, out)
	if len(l) != 2 || l["10.0.2.99"] != nil {
		t.Fatalf("重启后应恢复 2 台主机: %v", out)
	}
	h := l["10.0.2.17"]
	if h["az"] != "az1" || h["rack"] != "r07" || h["note"] != "n1" || h["createdBy"] != "dba" {
		t.Errorf("条目: %v", h)
	}
	lc, _ := l["10.0.2.250"]["lastCheck"].(map[string]any)
	if lc == nil || lc["ok"] != false || lc["role"] != "tikv" || len(lc["items"].([]any)) < 5 {
		t.Errorf("lastCheck 应随重启恢复: %v", l["10.0.2.250"])
	}
	// 恢复后可继续修改并再次落盘
	if code, _ := e2.login(t, "dba").do("POST", "/api/hostpool/10.0.2.17", map[string]any{"note": "n2"}); code != 200 {
		t.Fatalf("重启后更新: %d", code)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), `"n2"`) {
		t.Errorf("更新应写盘: %s", b)
	}
}

func TestHostPoolCorruptFileIsBackedUp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hostpool.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := setupHostOps(t, dir, true, nil)
	dba := e.login(t, "dba")
	if code, out := dba.do("GET", "/api/hostpool", nil); code != 200 || len(hostList(t, out)) != 0 {
		t.Fatalf("损坏的文件应从空池开始: %d %v", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "hostpool.json.bad")); err != nil || string(b) != "{not json" {
		t.Errorf("损坏的文件应备份而不是被覆盖: %v %q", err, b)
	}
}

// ---------------------------------------------------------------- 扩容规划联动 ----

func TestScaleOutPlanWarnsOnRecentFailedHostCheck(t *testing.T) {
	clk := newHostTestClock()
	e := setupHostOps(t, t.TempDir(), true, clk)
	dba := e.login(t, "dba")
	for _, ip := range []string{"10.0.2.17", "10.0.2.250"} {
		dba.do("POST", "/api/hostpool", map[string]any{"ip": ip, "az": "az1", "rack": "r01"})
	}
	preview := func(ip string) []string {
		code, out := dba.do("POST", "/api/changes/preview", map[string]any{"kind": "scale.out", "params": map[string]any{
			"role": "tikv", "hosts": []map[string]any{{"ip": ip, "az": "az1", "rack": "r01"}}, "cache": 16}})
		if code != 200 {
			t.Fatalf("预览 %s: %d %v", ip, code, out)
		}
		var warns []string
		for _, c := range out["plan"].(map[string]any)["checks"].([]any) {
			cm := c.(map[string]any)
			if cm["lv"] == "warn" && strings.Contains(cm["t"].(string), "最近一次预检未通过") {
				warns = append(warns, cm["t"].(string))
			}
			if cm["lv"] == "bad" {
				t.Errorf("预检未通过只告警不阻断: %v", cm)
			}
		}
		return warns
	}
	if w := preview("10.0.2.250"); len(w) != 0 {
		t.Fatalf("没有预检记录不应告警: %v", w)
	}
	dba.do("POST", "/api/hostpool/10.0.2.250/check", map[string]any{"role": "tikv"}) // 失败
	dba.do("POST", "/api/hostpool/10.0.2.17/check", map[string]any{"role": "tikv"})  // 通过
	if w := preview("10.0.2.250"); len(w) != 1 || !strings.Contains(w[0], "10.0.2.250") {
		t.Errorf("预检失败的主机应给出 warn: %v", w)
	}
	if w := preview("10.0.2.17"); len(w) != 0 {
		t.Errorf("预检通过的主机不应告警: %v", w)
	}
	// 4 分 59 秒内仍有效，超过 5 分钟后不再提示
	clk.Add(4*time.Minute + 59*time.Second)
	if w := preview("10.0.2.250"); len(w) != 1 {
		t.Errorf("5 分钟内仍应告警: %v", w)
	}
	clk.Add(2 * time.Second)
	if w := preview("10.0.2.250"); len(w) != 0 {
		t.Errorf("超过 5 分钟的预检结果不再参考: %v", w)
	}
	// 重新预检后又会提示
	dba.do("POST", "/api/hostpool/10.0.2.250/check", map[string]any{"role": "tikv"})
	if w := preview("10.0.2.250"); len(w) != 1 {
		t.Errorf("重新预检后应再次告警: %v", w)
	}
}
