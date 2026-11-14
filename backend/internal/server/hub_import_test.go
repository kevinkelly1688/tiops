// @author: kevinkelly
// @license: Apache License 2.0
// @file: hub_import_test.go
// @time: 2026/10/28

package server_test

// 中控机模式（--standalone --local-tiup）：一键导入本机 tiup 管理的集群（可运维），手工添加的远程集群只读。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/clusters"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

const fakeList = `Name    User  Version  Path                                      PrivateKey
----    ----  -------  ----                                      ----------
prod-a  tidb  v8.5.2   /home/tidb/.tiup/storage/cluster/prod-a   /home/tidb/.tiup/storage/cluster/prod-a/ssh/id_rsa
prod-b  tidb  v8.1.0   /home/tidb/.tiup/storage/cluster/prod-b   /home/tidb/.tiup/storage/cluster/prod-b/ssh/id_rsa
`

func fakeDisplay(name string) string {
	return `Cluster type:       tidb
Cluster name:       ` + name + `
Cluster version:    v8.5.2
Deploy user:        tidb
ID                  Role        Host       Ports        OS/Arch       Status   Data Dir   Deploy Dir
--                  ----        ----       -----        -------       ------   --------   ----------
10.0.1.21:2379      pd          10.0.1.21  2379/2380    linux/x86_64  Up|L|UI  /data/pd   /deploy/pd
10.0.1.11:4000      tidb        10.0.1.11  4000/10080   linux/x86_64  Up       -          /deploy/tidb
10.0.1.31:9090      prometheus  10.0.1.31  9090/12020   linux/x86_64  Up       /data/prom /deploy/prom
Total nodes: 3
`
}

// fakeTiup 模拟中控机上经 sudo 的执行器：只认 cluster.list / cluster.display，并记录收到的命令。
type fakeTiup struct{ got []string }

func (f *fakeTiup) Run(_ context.Context, req exec.Request, _ func(string)) (exec.Result, error) {
	f.got = append(f.got, req.Cmd.Kind+" "+req.Cmd.Cluster)
	switch req.Cmd.Kind {
	case exec.KindClusterList:
		return exec.Result{Output: fakeList}, nil
	case exec.KindClusterDisplay:
		if req.Cmd.Cluster == "prod-a" || req.Cmd.Cluster == "prod-b" {
			return exec.Result{Output: fakeDisplay(req.Cmd.Cluster)}, nil
		}
		return exec.Result{Exit: 1, Err: "cluster not found"}, nil
	}
	return exec.Result{Exit: 1, Err: "unexpected " + req.Cmd.Kind}, nil
}

type localEnv struct {
	*standaloneEnv
	run *fakeTiup
}

func newLocalTiup(t *testing.T, localTiup bool) *localEnv {
	t.Helper()
	fx := fixturesRoot(t)
	dir := t.TempDir()
	web := t.TempDir()
	_ = os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>home</html>"), 0o644)
	params := source.CatalogParams{Cluster: clusters.PlatformName, Home: "/home/tidb", Now: time.Now()}
	s := server.New(server.Config{Col: collect.New(source.Unconfigured{}, params), Mode: "live", Web: web, SnapTTL: time.Minute, Logf: (&logBuf{}).Printf})
	users, err := authz.OpenStore(filepath.Join(dir, "users.json"), hubMaster)
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
	run := &fakeTiup{}
	cfg := server.OpsConfig{Dir: dir, Env: "prod", Users: users, Audit: al, Key: opsKey, MasterKey: hubMaster, ReadOnlyFile: filepath.Join(dir, "READONLY"),
		ExecMode: "none", TickEvery: time.Hour, NotifyWatchEvery: -1}
	if localTiup {
		cfg.ExecMode, cfg.Runner = "sudo", run
	}
	if err := s.EnableOps(cfg); err != nil {
		t.Fatal(err)
	}
	primary := clusters.Spec{ID: clusters.PlatformID, Name: clusters.PlatformName, Display: "平台", Env: "prod", Mode: clusters.ModeLive, Platform: true}
	factory := func(spec clusters.Spec) (server.Backend, error) {
		d := spec.Fixtures
		if spec.Mode != clusters.ModeReplay {
			d = filepath.Join(fx, "dev")
		}
		rp, err := source.NewReplay(d)
		if err != nil {
			return server.Backend{}, err
		}
		local := localTiup && spec.Local
		be := server.Backend{Col: collect.New(rp, source.CatalogParams{Cluster: spec.Name, Home: "/home/tidb", Now: time.Now(), Remote: !local}), Mode: "live", Fixtures: d}
		be.Ops = func(_ *server.Server, c *server.OpsConfig) error { // 与 cmd/tidbops-server/clusters.go configureOps 的分流一致
			if local {
				c.ExecMode, c.Runner = "sudo", run
			} else {
				c.ReadOnlyWhy = "该集群是手工接入的远程集群：平台只读，不能执行变更（要运维请在中控机上「一键导入」）"
			}
			return nil
		}
		return be, nil
	}
	hub, err := server.NewHub(s, server.HubConfig{Dir: dir, Primary: primary, FixturesRoots: []string{fx}, Factory: factory, LocalTiup: localTiup})
	if err != nil {
		t.Fatal(err)
	}
	e := &standaloneEnv{t: t, fx: fx, dir: dir, hub: hub}
	e.ts = httptest.NewServer(hub.Handler())
	t.Cleanup(func() { e.ts.Close(); hub.Close(); s.Close(); _ = al.Close() })
	return &localEnv{standaloneEnv: e, run: run}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func items(t *testing.T, v map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := v["items"].([]any)
	for _, x := range list {
		m := x.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func TestImportDiscoverAndImport(t *testing.T) {
	e := newLocalTiup(t, true)
	admin := e.login("admin")

	code, d := admin.do("GET", "/api/clusters/discover", nil)
	if code != 200 || d["available"] != true {
		t.Fatalf("discover: %d %v", code, d)
	}
	it := items(t, d)
	if len(it) != 2 || it["prod-a"]["state"] != "new" || it["prod-a"]["id"] != "prod-a" || it["prod-b"]["version"] != "v8.1.0" {
		t.Fatalf("discover 内容不符: %v", d["items"])
	}
	if strings.Contains(strings.ToLower(toJSON(d)), "id_rsa") || strings.Contains(toJSON(d), "PrivateKey") {
		t.Errorf("discover 不应暴露私钥路径: %s", toJSON(d))
	}

	// 导入 prod-a：地址由 tiup cluster display 推导
	code, out := admin.do("POST", "/api/clusters/import", map[string]any{"items": []map[string]any{{"name": "prod-a", "display": "生产 A", "env": "prod"}}})
	if code != 200 || out["ok"] != true {
		t.Fatalf("import: %d %v", code, out)
	}
	_, lst := admin.do("GET", "/api/clusters", nil)
	var cfg map[string]any
	for _, c := range lst["clusters"].([]any) {
		if m := c.(map[string]any); m["id"] == "prod-a" {
			if m["local"] != true {
				t.Errorf("导入的集群 local 应为 true: %v", m)
			}
			cfg, _ = m["config"].(map[string]any)
		}
	}
	if cfg == nil || cfg["pd"] != "http://10.0.1.21:2379" || cfg["tidbStatus"] != "http://10.0.1.11:10080" || cfg["prom"] != "http://10.0.1.31:9090" || cfg["local"] != true {
		t.Fatalf("导入后的配置不符: %v", cfg)
	}
	if lst["localTiup"] != true {
		t.Errorf("/api/clusters 应带 localTiup=true: %v", lst["localTiup"])
	}
	if _, has := cfg["mysqlHost"]; has {
		t.Errorf("没给只读账号就不应配置 SQL: %v", cfg)
	}
	if out["results"].([]any)[0].(map[string]any)["warnings"] == nil {
		t.Errorf("没配 SQL 账号应有警告: %v", out["results"])
	}

	// 再 discover：prod-a 已导入；重复导入被拒绝
	_, d = admin.do("GET", "/api/clusters/discover", nil)
	if items(t, d)["prod-a"]["state"] != "imported" {
		t.Errorf("prod-a 应为 imported: %v", d["items"])
	}
	_, out = admin.do("POST", "/api/clusters/import", map[string]any{"items": []map[string]any{{"name": "prod-a", "env": "prod"}}})
	if r := out["results"].([]any)[0].(map[string]any); r["ok"] == true || out["ok"] == true {
		t.Errorf("重复导入应失败: %v", out)
	}

	// 不在 tiup cluster list 里的名字不能导入（不信任客户端）
	_, out = admin.do("POST", "/api/clusters/import", map[string]any{"items": []map[string]any{{"name": "ghost", "env": "prod"}}})
	if r := out["results"].([]any)[0].(map[string]any); r["ok"] == true || !strings.Contains(r["error"].(string), "没有这个集群") {
		t.Errorf("未知集群应拒绝: %v", out)
	}
	// 只有只读命令被发给执行器
	for _, g := range e.run.got {
		if !strings.HasPrefix(g, exec.KindClusterList) && !strings.HasPrefix(g, exec.KindClusterDisplay) {
			t.Errorf("导入只应使用只读命令，却执行了 %q", g)
		}
	}
	// 审计里有 cluster.import，且没有任何口令
	code, au := admin.do("GET", "/api/audit?action=cluster.import", nil)
	if code != 200 || !strings.Contains(toJSON(au), "cluster.import") {
		t.Errorf("审计应有 cluster.import: %d %v", code, au)
	}
}

func TestImportWithSQLAccountNeverEchoesPassword(t *testing.T) {
	e := newLocalTiup(t, true)
	admin := e.login("admin")
	code, out := admin.do("POST", "/api/clusters/import", map[string]any{"mysqlUser": "tidbops_ro", "mysqlPassword": "Sup3r-Secret",
		"items": []map[string]any{{"name": "prod-b", "id": "pb", "env": "staging"}}})
	if code != 200 || out["ok"] != true {
		t.Fatalf("import: %d %v", code, out)
	}
	_, lst := admin.do("GET", "/api/clusters", nil)
	if s := toJSON(lst); strings.Contains(s, "Sup3r-Secret") || !strings.Contains(s, `"mysqlHost":"10.0.1.11"`) || !strings.Contains(s, `"hasPassword":true`) {
		t.Errorf("SQL 账号应配置且密码不回显: %s", s)
	}
	_, au := admin.do("GET", "/api/audit?action=cluster.import", nil)
	if strings.Contains(toJSON(au), "Sup3r-Secret") {
		t.Error("审计里不能有口令")
	}
}

// 手工添加永远是远程接入：不能自称 local，写操作恒被拒绝且原因明确；同名会在 discover 里标成 conflict。
func TestManualClusterIsReadOnly(t *testing.T) {
	e := newLocalTiup(t, true)
	admin := e.login("admin")
	code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "remote1", "name": "prod-a", "env": "prod", "mode": "live", "pd": "http://10.9.9.9:2379", "local": true})
	if code != 200 {
		t.Fatalf("手工添加: %d %v", code, out)
	}
	if sp, _ := e.hub.Registry().Get("remote1"); sp.Local {
		t.Fatal("手工添加的集群不能是 local（请求里的 local 字段必须被忽略）")
	}
	_, d := admin.do("GET", "/api/clusters/discover", nil)
	if items(t, d)["prod-a"]["state"] != "conflict" {
		t.Errorf("同名的手工集群应标 conflict: %v", d["items"])
	}
	_, out = admin.do("POST", "/api/clusters/import", map[string]any{"items": []map[string]any{{"name": "prod-a", "env": "prod"}}})
	if out["ok"] == true {
		t.Errorf("与手工集群同名时导入应失败: %v", out)
	}
	// 即使两名管理员把写开关打开，变更预览的闸门仍然说明「手工接入的远程集群」只读
	admin2 := e.login("admin2")
	admin.on("remote1").do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.on("remote1").do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	code, pv := e.login("dba").on("remote1").do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "start"}})
	if gate, _ := pv["gate"].(string); code != 200 || !strings.Contains(gate, "手工接入的远程集群") {
		t.Errorf("远程集群的写闸门应恒为只读: %d %v", code, pv)
	}
	_, me := admin.do("GET", "/api/auth/me", nil)
	for _, c := range me["clusters"].([]any) {
		m := c.(map[string]any)
		if m["id"] == "remote1" {
			reason, _ := dg(m, "write", "reason").(string)
			if m["local"] == true || !strings.Contains(reason, "手工接入的远程集群") {
				t.Errorf("远程集群的写状态应说明只读原因: %v", m)
			}
		}
	}
}

func TestImportRequiresAdminAndLocalTiup(t *testing.T) {
	e := newLocalTiup(t, true)
	dba := e.login("dba")
	if code, _ := dba.do("GET", "/api/clusters/discover", nil); code != 403 {
		t.Errorf("非管理员 discover 应 403，得到 %d", code)
	}
	if code, _ := dba.do("POST", "/api/clusters/import", map[string]any{"items": []map[string]any{{"name": "prod-a", "env": "prod"}}}); code != 403 {
		t.Errorf("非管理员 import 应 403，得到 %d", code)
	}

	// 不是中控机模式：discover 说明不可用；import 拒绝
	n := newLocalTiup(t, false)
	admin := n.login("admin")
	code, d := admin.do("GET", "/api/clusters/discover", nil)
	if code != 200 || d["available"] != false || !strings.Contains(toJSON(d), "--local-tiup") {
		t.Errorf("非中控机模式 discover 应 available=false: %d %v", code, d)
	}
	if code, out := admin.do("POST", "/api/clusters/import", map[string]any{"items": []map[string]any{{"name": "prod-a", "env": "prod"}}}); code == 200 {
		t.Errorf("非中控机模式不能导入: %d %v", code, out)
	}
}

func TestImportReservedIDs(t *testing.T) {
	e := newLocalTiup(t, true)
	admin := e.login("admin")
	for _, id := range []string{"import", "discover"} {
		if code, _ := admin.do("POST", "/api/clusters", map[string]any{"id": id, "env": "prod", "mode": "live", "pd": "http://10.9.9.9:2379"}); code != 400 {
			t.Errorf("id %q 是保留字，应 400，得到 %d", id, code)
		}
	}
}
