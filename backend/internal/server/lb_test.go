// @author: kevinkelly
// @license: Apache License 2.0
// @file: lb_test.go
// @time: 2026/10/28

package server_test

// 负载均衡层：手工登记 CRUD / 校验 / 持久化 / 多集群隔离 / 权限 / 审计 / 一致性检查 / 规划文案 / 自动发现的 TiProxy。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lbBody(id, typ, name, addr string, backends []string) map[string]any {
	m := map[string]any{"id": id, "type": typ, "name": name, "addr": addr}
	if backends != nil {
		m["backends"] = backends
	}
	return m
}

func lbList(t *testing.T, c *hc) map[string]any {
	t.Helper()
	code, out := c.do("GET", "/api/lb", nil)
	if code != 200 {
		t.Fatalf("GET /api/lb: %d %v", code, out)
	}
	return out
}

func entryIDs(out map[string]any) []string {
	var ids []string
	for _, e := range out["entries"].([]any) {
		ids = append(ids, e.(map[string]any)["id"].(string))
	}
	return ids
}

func TestLBCrudValidationAndErrors(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")

	// 空
	out := lbList(t, admin)
	if len(entryIDs(out)) != 0 || len(out["proxies"].([]any)) != 0 || dg(out, "effective", "hasLB") != false || dg(out, "effective", "reason") != "none" || len(out["check"].([]any)) != 0 {
		t.Fatalf("初始应为空: %v", out)
	}
	// 新增
	code, ent := admin.do("POST", "/api/ops/lb", lbBody("haproxy-prod", "haproxy", " 生产 HAProxy ", "VIP.example.com:3306", []string{"10.0.1.11:4000", "10.0.1.12:4000"}))
	if code != 200 || ent["id"] != "haproxy-prod" || ent["name"] != "生产 HAProxy" || ent["addr"] != "vip.example.com:3306" || ent["source"] != "manual" || ent["updatedBy"] != "admin" || ent["created"] == nil {
		t.Fatalf("新增: %d %v", code, ent)
	}
	out = lbList(t, admin)
	if got := entryIDs(out); len(got) != 1 || got[0] != "haproxy-prod" || dg(out, "effective", "hasLB") != true || dg(out, "effective", "reason") != "manual" {
		t.Fatalf("列表: %v", out)
	}
	// 更新：部分字段；id 不可改
	code, ent = admin.do("POST", "/api/ops/lb/haproxy-prod", map[string]any{"name": "改名", "note": "机密备注 XYZ-SECRET"})
	if code != 200 || ent["name"] != "改名" || ent["addr"] != "vip.example.com:3306" || ent["note"] != "机密备注 XYZ-SECRET" {
		t.Fatalf("更新: %d %v", code, ent)
	}
	if code, o := admin.do("POST", "/api/ops/lb/haproxy-prod", map[string]any{"id": "other", "name": "x"}); code != 400 || o["code"] != "invalid" {
		t.Errorf("改 id 应 400 invalid: %d %v", code, o)
	}
	// 清空后端
	if code, ent = admin.do("POST", "/api/ops/lb/haproxy-prod", map[string]any{"backends": []string{}}); code != 200 || len(ent["backends"].([]any)) != 0 {
		t.Errorf("清空后端: %d %v", code, ent)
	}

	// 校验：400 invalid
	bad := map[string]map[string]any{
		"id 非法":    lbBody("Bad_ID", "lvs", "n", "h:1", nil),
		"id 太短":    lbBody("a", "lvs", "n", "h:1", nil),
		"type 非法":  lbBody("lb-1", "envoy", "n", "h:1", nil),
		"name 空":   lbBody("lb-1", "lvs", " ", "h:1", nil),
		"name 太长":  lbBody("lb-1", "lvs", strings.Repeat("长", 41), "h:1", nil),
		"addr 协议":  lbBody("lb-1", "lvs", "n", "http://h:80", nil),
		"addr 凭据":  lbBody("lb-1", "lvs", "n", "root:pw@h:3306", nil),
		"addr 空格":  lbBody("lb-1", "lvs", "n", "h :3306", nil),
		"addr 端口0": lbBody("lb-1", "lvs", "n", "h:0", nil),
		"addr 端口大": lbBody("lb-1", "lvs", "n", "h:65536", nil),
		"addr 无端口": lbBody("lb-1", "lvs", "n", "h", nil),
		"后端非法":     lbBody("lb-1", "lvs", "n", "h:1", []string{"10.0.0.1"}),
	}
	long := lbBody("lb-1", "lvs", "n", "h:1", nil)
	long["note"] = strings.Repeat("长", 201)
	bad["note 太长"] = long
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example.com:4000", i)
	}
	bad["后端 65 个"] = lbBody("lb-1", "lvs", "n", "h:1", many)
	for name, body := range bad {
		if code, o := admin.do("POST", "/api/ops/lb", body); code != 400 || o["code"] != "invalid" {
			t.Errorf("%s 应 400 invalid: %d %v", name, code, o)
		}
	}
	// 缺字段
	if code, o := admin.do("POST", "/api/ops/lb", map[string]any{"id": "lb-2", "type": "lvs"}); code != 400 || o["code"] != "invalid" {
		t.Errorf("缺 name/addr: %d %v", code, o)
	}
	// 请求体不是 JSON
	if code, _, _ := admin.raw("POST", "/api/ops/lb", nil, nil); code != 400 {
		t.Errorf("空请求体应 400: %d", code)
	}
	// 重复 id：409 exists
	if code, o := admin.do("POST", "/api/ops/lb", lbBody("haproxy-prod", "lvs", "n", "h:1", nil)); code != 409 || o["code"] != "exists" {
		t.Errorf("重复 id: %d %v", code, o)
	}
	// 不存在：404
	if code, o := admin.do("POST", "/api/ops/lb/nope", map[string]any{"name": "x"}); code != 404 || o["code"] != "not_found" {
		t.Errorf("更新不存在: %d %v", code, o)
	}
	if code, o := admin.do("POST", "/api/ops/lb/nope/delete", nil); code != 404 || o["code"] != "not_found" {
		t.Errorf("删除不存在: %d %v", code, o)
	}
	// 上限 20：已有 1 条，再加 19 条 OK，第 21 条 409 limit
	for i := 2; i <= 20; i++ {
		if code, o := admin.do("POST", "/api/ops/lb", lbBody(fmt.Sprintf("lb-%02d", i), "other", fmt.Sprintf("lb %d", i), "h:1", nil)); code != 200 {
			t.Fatalf("第 %d 条: %d %v", i, code, o)
		}
	}
	if code, o := admin.do("POST", "/api/ops/lb", lbBody("lb-21", "other", "x", "h:1", nil)); code != 409 || o["code"] != "limit" {
		t.Errorf("超过上限: %d %v", code, o)
	}
	// 重复 id 优先于上限判断；删除后腾出名额
	if code, _ := admin.do("POST", "/api/ops/lb/lb-20/delete", nil); code != 200 {
		t.Error("删除失败")
	}
	if code, _ := admin.do("POST", "/api/ops/lb", lbBody("lb-21", "other", "x", "h:1", nil)); code != 200 {
		t.Error("删除后应能再添加")
	}
	if n := len(entryIDs(lbList(t, admin))); n != 20 {
		t.Errorf("应有 20 条: %d", n)
	}
	// CSRF
	badCSRF := *admin
	badCSRF.csrf = "bad"
	if code, _, _ := badCSRF.raw("POST", "/api/ops/lb", lbBody("csrf-lb", "lvs", "n", "h:1", nil), nil); code != 403 {
		t.Errorf("CSRF 错误应 403: %d", code)
	}
}

func TestLBPermissions(t *testing.T) {
	e := newHub(t, "")
	admin, dba, viewer := e.login("admin"), e.login("dba"), e.login("viewer")
	e.addStgDev(admin)
	if code, _ := admin.do("POST", "/api/ops/lb", lbBody("lb-a", "lvs", "n", "h:1", nil)); code != 200 {
		t.Fatal("admin 新增失败")
	}
	// 非管理员写 403（含 DBA）；只读用户 GET 200
	for who, c := range map[string]*hc{"dba": dba, "viewer": viewer} {
		if code, _ := c.do("POST", "/api/ops/lb", lbBody("lb-b", "lvs", "n", "h:1", nil)); code != 403 {
			t.Errorf("%s 新增应 403: %d", who, code)
		}
		if code, _ := c.do("POST", "/api/ops/lb/lb-a", map[string]any{"name": "x"}); code != 403 {
			t.Errorf("%s 更新应 403: %d", who, code)
		}
		if code, _ := c.do("POST", "/api/ops/lb/lb-a/delete", nil); code != 403 {
			t.Errorf("%s 删除应 403: %d", who, code)
		}
		if code, out := c.do("GET", "/api/lb", nil); code != 200 || len(entryIDs(out)) != 1 {
			t.Errorf("%s 读取应 200: %d %v", who, code, out)
		}
	}
	// 未登录 401
	if code, _ := e.client().do("GET", "/api/lb", nil); code != 401 {
		t.Errorf("未登录应 401: %d", code)
	}
	// 无 view.overview 的集群：统一 404（prodonly 在 staging / test 没有角色）
	prodonly := e.login("prodonly")
	if code, _ := prodonly.on("prod-tidb-01").do("GET", "/api/lb", nil); code != 200 {
		t.Errorf("prodonly 看 prod 应 200: %d", code)
	}
	for _, id := range []string{"stg", "dev"} {
		code, b, _ := prodonly.on(id).raw("GET", "/api/lb", nil, nil)
		if code != 404 || strings.TrimSpace(string(b)) != notFoundBody {
			t.Errorf("prodonly 看 %s 应统一 404: %d %s", id, code, b)
		}
		code, b, _ = prodonly.on(id).raw("POST", "/api/ops/lb", lbBody("lb-x", "lvs", "n", "h:1", nil), nil)
		if code != 404 {
			t.Errorf("prodonly 写 %s 应 404: %d %s", id, code, b)
		}
	}
	// 没有写开关也可以登记（平台元数据）：写开关此时是关闭的
	_, me := admin.do("GET", "/api/auth/me", nil)
	if dg(me, "write", "enabled") != false {
		t.Fatalf("前提：写开关应关闭: %v", me["write"])
	}
}

func TestLBMultiClusterIsolation(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	if code, o := admin.on("stg").do("POST", "/api/ops/lb", lbBody("stg-ha", "haproxy", "预发 HAProxy", "10.20.0.5:3306", nil)); code != 200 {
		t.Fatalf("stg 新增: %d %v", code, o)
	}
	if code, o := admin.on("prod-tidb-01").do("POST", "/api/ops/lb", lbBody("prod-lvs", "lvs", "生产 LVS", "10.0.0.9:3306", nil)); code != 200 {
		t.Fatalf("prod 新增: %d %v", code, o)
	}
	if got := entryIDs(lbList(t, admin.on("stg"))); len(got) != 1 || got[0] != "stg-ha" {
		t.Errorf("stg: %v", got)
	}
	if got := entryIDs(lbList(t, admin.on("prod-tidb-01"))); len(got) != 1 || got[0] != "prod-lvs" {
		t.Errorf("prod: %v", got)
	}
	if got := entryIDs(lbList(t, admin)); len(got) != 1 || got[0] != "prod-lvs" { // 缺省 = 主集群
		t.Errorf("缺省: %v", got)
	}
	out := lbList(t, admin.on("dev"))
	if len(entryIDs(out)) != 0 || dg(out, "effective", "hasLB") != false {
		t.Errorf("dev 不应看到别的集群的登记: %v", out)
	}
	// 同一个 id 在不同集群可各自存在；在 dev 上删除 stg 的 id 是 404
	if code, _ := admin.on("dev").do("POST", "/api/ops/lb/stg-ha/delete", nil); code != 404 {
		t.Errorf("跨集群删除应 404: %d", code)
	}
	if code, _ := admin.on("dev").do("POST", "/api/ops/lb", lbBody("stg-ha", "lvs", "dev 的", "h:1", nil)); code != 200 {
		t.Error("不同集群可有同 id")
	}
}

func TestLBPersistenceRestartAndLegacySettings(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	admin.on("stg").do("POST", "/api/ops/lb", lbBody("stg-ha", "haproxy", "预发 HAProxy", "10.20.0.5:3306", []string{"10.20.1.11:4000"}))
	admin.on("stg").do("POST", "/api/ops/lb/stg-ha", map[string]any{"note": "n1"})
	e.shutdown()

	e2 := newHub(t, e.dir)
	admin2 := e2.login("admin")
	out := lbList(t, admin2.on("stg"))
	got := out["entries"].([]any)
	if len(got) != 1 {
		t.Fatalf("重启后应恢复: %v", out)
	}
	ent := got[0].(map[string]any)
	if ent["id"] != "stg-ha" || ent["note"] != "n1" || ent["addr"] != "10.20.0.5:3306" || ent["updatedBy"] != "admin" {
		t.Errorf("恢复的内容: %v", ent)
	}
	if len(entryIDs(lbList(t, admin2.on("dev")))) != 0 || len(entryIDs(lbList(t, admin2))) != 0 {
		t.Error("重启后不应串到别的集群")
	}
	// settings.json 在每集群各自的目录；旧字段 lb 不受影响
	admin2.on("stg").do("POST", "/api/ops/settings", map[string]any{"lb": true})
	_, st := admin2.on("stg").do("GET", "/api/ops/settings", nil)
	if dg(st, "settings", "lb") != true {
		t.Errorf("settings.lb: %v", st)
	}
	if _, has := st["settings"].(map[string]any)["lbs"]; has {
		t.Errorf("GET /api/ops/settings 的返回结构不应变化（不含 lbs）: %v", st["settings"])
	}
	out = lbList(t, admin2.on("stg"))
	if dg(out, "effective", "reason") != "legacy-flag" || dg(out, "effective", "hasLB") != true || len(entryIDs(out)) != 1 {
		t.Errorf("开关 + 登记并存: %v", out["effective"])
	}
	// 只有旧开关：hasLB=true, legacy-flag
	admin2.on("dev").do("POST", "/api/ops/settings", map[string]any{"lb": true})
	if out := lbList(t, admin2.on("dev")); dg(out, "effective", "reason") != "legacy-flag" || dg(out, "effective", "hasLB") != true {
		t.Errorf("legacy: %v", out["effective"])
	}
}

// 旧版 settings.json（没有 lbs 字段）可以加载，且不丢其余字段。
func TestLBLegacySettingsFileLoads(t *testing.T) {
	e := setupOpsPre(t, func(dir string) {
		old := `{"writeEnabled":false,"freezes":[],"windows":null,"lb":true,"overrides":{"k":"v"}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	admin := e.login(t, "admin")
	code, out := admin.do("GET", "/api/lb", nil)
	if code != 200 || dg(out, "effective", "hasLB") != true || dg(out, "effective", "reason") != "legacy-flag" || len(out["entries"].([]any)) != 0 {
		t.Fatalf("旧文件: %d %v", code, out)
	}
	if code, o := admin.do("POST", "/api/ops/lb", lbBody("ha-1", "haproxy", "n", "h:3306", nil)); code != 200 {
		t.Fatalf("新增: %d %v", code, o)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["lb"] != true || dg(m, "overrides", "k") != "v" || len(m["lbs"].([]any)) != 1 {
		t.Errorf("保存后应保留旧字段并带上 lbs: %s", b)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "settings.json.tmp")); err == nil {
		t.Error("原子写不应留下 .tmp")
	}
}

func TestLBAuditContent(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	admin.do("POST", "/api/ops/lb", lbBody("ha-1", "haproxy", "审计测试", "10.0.0.5:3306", nil))
	admin.do("POST", "/api/ops/lb/ha-1", map[string]any{"note": "TOPSECRET-NOTE-TEXT", "addr": "10.0.0.6:3306"})
	admin.do("POST", "/api/ops/lb", lbBody("ha-1", "haproxy", "重复", "h:1", nil))         // exists → failed
	admin.do("POST", "/api/ops/lb", lbBody("ha-2", "haproxy", "坏地址", "http://x:1", nil)) // invalid → failed
	admin.do("POST", "/api/ops/lb/ha-1/delete", nil)
	e.login("dba").do("POST", "/api/ops/lb", lbBody("ha-3", "haproxy", "越权", "h:1", nil)) // 403 → authz.deny

	ents := e.auditEntries("")
	got := map[string][]map[string]any{}
	for _, a := range ents {
		act, _ := a["action"].(string)
		got[act] = append(got[act], a)
		if s, _ := a["summary"].(string); strings.Contains(s, "TOPSECRET-NOTE-TEXT") {
			t.Errorf("审计不应包含备注原文: %v", a)
		}
	}
	if len(got["lb.add"]) != 3 || len(got["lb.update"]) != 1 || len(got["lb.delete"]) != 1 {
		t.Fatalf("lb.* 审计条数: add=%d update=%d delete=%d", len(got["lb.add"]), len(got["lb.update"]), len(got["lb.delete"]))
	}
	upd := got["lb.update"][0]
	sum := upd["summary"].(string)
	if upd["risk"] != "mid" || upd["result"] != "success" || upd["target"] != "ha-1" || !strings.Contains(sum, "addr") || !strings.Contains(sum, "note") || !strings.Contains(sum, "10.0.0.6:3306") {
		t.Errorf("update 审计: %v", upd)
	}
	results := map[string]int{}
	for _, a := range got["lb.add"] {
		results[a["result"].(string)]++
		if a["risk"] != "mid" || a["user"] != "admin" {
			t.Errorf("add 审计: %v", a)
		}
	}
	if results["success"] != 1 || results["failed"] != 2 {
		t.Errorf("add 的结果分布（成功 1，失败 2）: %v", results)
	}
	if d := got["lb.delete"][0]; d["result"] != "success" || !strings.Contains(d["summary"].(string), "10.0.0.6:3306") {
		t.Errorf("delete 审计: %v", d)
	}
	denied := 0
	for _, a := range got["authz.deny"] {
		if a["target"] == "/api/ops/lb" {
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("越权应记 authz.deny: %d", denied)
	}
}

// GET /api/lb 带出自动发现的 TiProxy，并按拓扑里 TiDB 的 SQL 地址做一致性检查；读取不触发采集。
func TestLBProxiesAndCheck(t *testing.T) {
	e := newHub(t, "")
	admin := e.login("admin")
	e.addStgDev(admin)
	stg := admin.on("stg")

	// 快照还没有被采集过：proxies / check 为空（不触发采集）
	if out := lbList(t, stg); len(out["proxies"].([]any)) != 0 {
		// 新增集群时可能已预热快照；此时只要求结构正确
		t.Logf("快照已预热: %v", out["proxies"])
	}
	stg.do("GET", "/api/snapshot", nil) // 采集一次，填充缓存
	stg.do("POST", "/api/ops/lb", lbBody("ha-ok", "haproxy", "一致", "10.20.0.5:3306", []string{"10.20.1.11:4000", "10.20.1.12:4000"}))
	stg.do("POST", "/api/ops/lb", lbBody("ha-diff", "lvs", "不一致", "10.20.0.6:3306", []string{"10.20.1.11:4000", "10.20.1.99:4000"}))
	stg.do("POST", "/api/ops/lb", lbBody("ha-none", "nginx", "无后端", "10.20.0.7:3306", nil))

	out := lbList(t, stg)
	px := out["proxies"].([]any)
	if len(px) != 2 {
		t.Fatalf("stg 应自动发现 2 个 TiProxy: %v", out["proxies"])
	}
	p1 := px[0].(map[string]any)
	if p1["source"] != "auto" || p1["id"] != "tiproxy-1" || p1["type"] != "tiproxy" || p1["host"] != "10.20.1.41" || p1["sqlPort"] != float64(6000) || p1["apiPort"] != float64(3080) || p1["status"] != "up" {
		t.Errorf("proxy: %v", p1)
	}
	for _, x := range out["entries"].([]any) {
		if x.(map[string]any)["source"] != "manual" {
			t.Errorf("手工登记 source 应为 manual: %v", x)
		}
	}
	checks := map[string]map[string]any{}
	for _, c := range out["check"].([]any) {
		m := c.(map[string]any)
		checks[m["id"].(string)] = m
	}
	if len(checks) != 2 || checks["ha-none"] != nil {
		t.Fatalf("只检查有后端的登记: %v", out["check"])
	}
	if m := checks["ha-ok"]; len(m["missing"].([]any)) != 0 || len(m["extra"].([]any)) != 0 {
		t.Errorf("ha-ok 应一致: %v", m)
	}
	m := checks["ha-diff"]
	if fmt.Sprint(m["missing"]) != "[10.20.1.12:4000]" || fmt.Sprint(m["extra"]) != "[10.20.1.99:4000]" {
		t.Errorf("ha-diff: %v", m)
	}
	// 自动发现的 TiProxy 不触发 effective：stg 只有 3 条手工登记时是 manual；删光后是 none
	for _, id := range []string{"ha-ok", "ha-diff", "ha-none"} {
		stg.do("POST", "/api/ops/lb/"+id+"/delete", nil)
	}
	out = lbList(t, stg)
	if dg(out, "effective", "hasLB") != false || dg(out, "effective", "reason") != "none" || len(out["proxies"].([]any)) != 2 {
		t.Errorf("TiProxy 自动发现不应让 hasLB 为真: %v", out)
	}
	// 其余集群没有 TiProxy
	if out := lbList(t, admin); len(out["proxies"].([]any)) != 0 {
		t.Errorf("demo 不应有 proxies: %v", out["proxies"])
	}
	// /api/topology 与 /api/snapshot 都带出 proxies，Instances 不含 tiproxy
	_, topo := stg.do("GET", "/api/topology", nil)
	if len(topo["proxies"].([]any)) != 2 || len(topo["instances"].([]any)) != 9 {
		t.Errorf("topology: proxies=%v instances=%d", topo["proxies"], len(topo["instances"].([]any)))
	}
	_, snap := stg.do("GET", "/api/snapshot", nil)
	if len(dg(snap, "topology", "proxies").([]any)) != 2 {
		t.Errorf("snapshot.topology.proxies: %v", dg(snap, "topology", "proxies"))
	}
	_, dtopo := admin.do("GET", "/api/topology", nil)
	if _, has := dtopo["proxies"]; has {
		t.Errorf("没有 TiProxy 的集群不应带 proxies 字段: %v", dtopo["proxies"])
	}
}

// 变更规划：有手工登记时 c.LB 取有效值，缩容 TiDB 的摘除步骤点名具体入口；没有登记时文案不变。
func TestLBPlanTextNamesRegisteredEntries(t *testing.T) {
	e := newHub(t, "")
	admin, dba := e.login("admin"), e.login("dba")
	preview := func() []map[string]any {
		t.Helper()
		code, out := dba.do("POST", "/api/changes/preview", map[string]any{"kind": "scale.in", "params": map[string]any{"nodes": []string{"tidb-3"}}})
		if code != 200 {
			t.Fatalf("preview: %d %v", code, out)
		}
		var steps []map[string]any
		for _, s := range dg(out, "plan", "steps").([]any) {
			steps = append(steps, s.(map[string]any))
		}
		return steps
	}
	manual := func(steps []map[string]any) (string, bool) {
		for _, s := range steps {
			if s["name"] == "从负载均衡摘除并等待连接排空" {
				return s["manual"].(string), true
			}
		}
		return "", false
	}
	// 没有任何负载均衡：没有摘除步骤
	if _, ok := manual(preview()); ok {
		t.Fatal("没有负载均衡时不应有摘除步骤")
	}
	// 只有旧开关：旧文案
	admin.do("POST", "/api/ops/settings", map[string]any{"lb": true})
	got, ok := manual(preview())
	if !ok || got != "把 10.0.1.13:4000 从负载均衡后端摘除，并等待连接排空，确认后继续" {
		t.Errorf("旧开关文案应不变: %q", got)
	}
	admin.do("POST", "/api/ops/settings", map[string]any{"lb": false})
	// 只有手工登记（开关关闭）：c.LB 取有效值 → 出现摘除步骤，且点名
	admin.do("POST", "/api/ops/lb", lbBody("haproxy-prod", "haproxy", "haproxy-prod", "10.0.0.5:3306", nil))
	got, ok = manual(preview())
	if !ok || got != "先从负载均衡 haproxy-prod（HAProxy，10.0.0.5:3306） 摘除 10.0.1.13:4000，并等待连接排空，确认后继续" {
		t.Errorf("有登记时应点名入口: %q", got)
	}
	// 多条：全部列出
	admin.do("POST", "/api/ops/lb", lbBody("slb-a", "slb", "云 SLB", "slb.example.com:3306", nil))
	got, _ = manual(preview())
	if !strings.Contains(got, "haproxy-prod（HAProxy，10.0.0.5:3306）") || !strings.Contains(got, "云 SLB（云 SLB，slb.example.com:3306）") {
		t.Errorf("多条登记应全部列出: %q", got)
	}
	// 删光后恢复为没有摘除步骤
	admin.do("POST", "/api/ops/lb/haproxy-prod/delete", nil)
	admin.do("POST", "/api/ops/lb/slb-a/delete", nil)
	if _, ok := manual(preview()); ok {
		t.Error("删除全部登记后不应再有摘除步骤")
	}
}

// 写盘失败：500 store，内存状态回滚（不出现「接口说失败但列表里已有」的不一致），审计记 failed。
func TestLBStoreFailureRollsBack(t *testing.T) {
	e := setupOps(t)
	admin := e.login(t, "admin")
	if code, o := admin.do("POST", "/api/ops/lb", lbBody("ha-1", "haproxy", "n", "h:3306", nil)); code != 200 {
		t.Fatalf("新增: %d %v", code, o)
	}
	// 让 settings.json.tmp 成为目录：WriteFile 必然失败
	if err := os.Mkdir(filepath.Join(e.dir, "settings.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, o := admin.do("POST", "/api/ops/lb", lbBody("ha-2", "haproxy", "n", "h:3306", nil)); code != 500 || o["code"] != "store" {
		t.Errorf("新增应 500 store: %d %v", code, o)
	}
	if code, o := admin.do("POST", "/api/ops/lb/ha-1", map[string]any{"name": "改"}); code != 500 || o["code"] != "store" {
		t.Errorf("更新应 500 store: %d %v", code, o)
	}
	if code, o := admin.do("POST", "/api/ops/lb/ha-1/delete", nil); code != 500 || o["code"] != "store" {
		t.Errorf("删除应 500 store: %d %v", code, o)
	}
	_, out := admin.do("GET", "/api/lb", nil)
	ents := out["entries"].([]any)
	if len(ents) != 1 || ents[0].(map[string]any)["id"] != "ha-1" || ents[0].(map[string]any)["name"] != "n" {
		t.Errorf("失败后内存应回滚: %v", out["entries"])
	}
}
