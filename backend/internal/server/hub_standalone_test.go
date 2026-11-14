// @author: kevinkelly
// @license: Apache License 2.0
// @file: hub_standalone_test.go
// @time: 2026/10/28

package server_test

// 独立部署（--standalone）：主实例是「平台」本身，不代表任何集群；集群全部由管理员在「集群接入」里添加。

import (
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
	"tidbops/internal/server"
	"tidbops/internal/source"
)

type standaloneEnv struct {
	t   *testing.T
	ts  *httptest.Server
	hub *server.Hub
	fx  string
	dir string
}

func newStandalone(t *testing.T) *standaloneEnv {
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
	if err := s.EnableOps(server.OpsConfig{Dir: dir, Env: "prod", Users: users, Audit: al, Key: opsKey, MasterKey: hubMaster, ReadOnlyFile: filepath.Join(dir, "READONLY"),
		ExecMode: "none", TickEvery: time.Hour, NotifyWatchEvery: -1}); err != nil {
		t.Fatal(err)
	}
	primary := clusters.Spec{ID: clusters.PlatformID, Name: clusters.PlatformName, Display: "平台", Env: "prod", Mode: clusters.ModeLive, Platform: true}
	e := &standaloneEnv{t: t, fx: fx, dir: dir}
	// 工厂：用回放数据冒充远程集群（真实工厂会按 live 配置连 PD / Prometheus）；不配执行器 = 只读接入
	factory := func(spec clusters.Spec) (server.Backend, error) {
		d := spec.Fixtures
		if spec.Mode != clusters.ModeReplay {
			d = filepath.Join(fx, "dev")
		}
		rp, err := source.NewReplay(d)
		if err != nil {
			return server.Backend{}, err
		}
		return server.Backend{Col: collect.New(rp, source.CatalogParams{Cluster: spec.Name, Home: "/home/tidb", Now: time.Now(), Remote: true}), Mode: "live", Fixtures: d}, nil
	}
	hub, err := server.NewHub(s, server.HubConfig{Dir: dir, Primary: primary, FixturesRoots: []string{fx}, Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	e.hub = hub
	e.ts = httptest.NewServer(hub.Handler())
	t.Cleanup(func() { e.ts.Close(); hub.Close(); s.Close(); _ = al.Close() })
	return e
}

func (e *standaloneEnv) login(user string) *hc {
	e.t.Helper()
	jar := (&hubEnv{t: e.t, ts: e.ts}).client()
	jar.base = e.ts.URL
	code, out := jar.do("POST", "/api/auth/login", map[string]string{"user": user, "password": authz.DemoPassword})
	if code != 200 {
		e.t.Fatalf("登录 %s: %d %v", user, code, out)
	}
	jar.csrf, _ = out["csrf"].(string)
	return jar
}

func TestStandaloneEmptyPlatform(t *testing.T) {
	e := newStandalone(t)
	if !e.hub.Platform() {
		t.Fatal("Hub.Platform() 应为 true")
	}
	// 匿名健康检查：带 standalone 标记
	anon := (&hubEnv{t: t, ts: e.ts}).client()
	if _, h := anon.do("GET", "/api/health", nil); h["standalone"] != true || dg(h, "ops", "enabled") != true {
		t.Errorf("health 应标记 standalone: %v", h)
	}
	admin := e.login("admin")
	code, me := admin.do("GET", "/api/auth/me", nil)
	if code != 200 || me["authenticated"] != true {
		t.Fatalf("me: %d %v", code, me)
	}
	if cl, _ := me["clusters"].([]any); len(cl) != 0 {
		t.Errorf("还没有接入集群：me.clusters 应为空: %v", me["clusters"])
	}
	// 集群接入页：列表为空，不含平台实例，也不把它当主集群
	code, lst := admin.do("GET", "/api/clusters", nil)
	if code != 200 || lst["standalone"] != true || lst["primary"] != "" || lst["canManage"] != true {
		t.Fatalf("GET /api/clusters: %d %v", code, lst)
	}
	if cl, _ := lst["clusters"].([]any); len(cl) != 0 {
		t.Errorf("集群列表应为空: %v", lst["clusters"])
	}
	// 平台实例不提供集群数据：不带头与显式指定都是统一的 404
	for _, hdr := range []string{"", clusters.PlatformID, "nope"} {
		code, b, _ := admin.on(hdr).raw("GET", "/api/snapshot", nil, nil)
		if body := strings.TrimSpace(string(b)); code != 404 || body != notFoundBody {
			t.Errorf("X-Cluster=%q 的 /api/snapshot 应是统一 404，得到 %d %s", hdr, code, b)
		}
	}
	// 平台实例不能被当成集群删除 / 更新
	if code, _ := admin.do("POST", "/api/clusters/"+clusters.PlatformID+"/delete", nil); code != 403 && code != 404 {
		t.Errorf("删除平台实例应被拒绝，得到 %d", code)
	}
	// 静态页面不受影响
	if code, _, _ := admin.raw("GET", "/", nil, nil); code != 200 {
		t.Errorf("静态页: %d", code)
	}
}

func TestStandaloneAddClusters(t *testing.T) {
	e := newStandalone(t)
	admin := e.login("admin")
	add := func(id, env string) (int, map[string]any) {
		return admin.do("POST", "/api/clusters", map[string]any{"id": id, "name": id + "-tidb-01", "display": id + " 集群", "env": env, "mode": "replay", "fixtures": filepath.Join(e.fx, id)})
	}
	if code, out := add("stg", "staging"); code != 200 {
		t.Fatalf("接入 stg: %d %v", code, out)
	}
	if code, out := add("dev", "test"); code != 200 {
		t.Fatalf("接入 dev: %d %v", code, out)
	}
	_, me := admin.do("GET", "/api/auth/me", nil)
	cl, _ := me["clusters"].([]any)
	if len(cl) != 2 {
		t.Fatalf("me.clusters 应有 2 个（不含平台）: %v", me["clusters"])
	}
	for _, c := range cl {
		m := c.(map[string]any)
		if m["id"] == clusters.PlatformID || m["primary"] == true {
			t.Errorf("平台实例不应出现在可见集群里: %v", m)
		}
	}
	// 显式选择集群：返回该集群的数据
	if code, v := admin.on("stg").do("GET", "/api/topology", nil); code != 200 || dg(v, "cluster") != "stg-tidb-01" {
		t.Errorf("stg 拓扑: %d %v", code, v["cluster"])
	}
	// 不带头：落到该用户可见的第一个集群，而不是平台实例
	if code, v := admin.do("GET", "/api/topology", nil); code != 200 || dg(v, "cluster") == clusters.PlatformName || dg(v, "cluster") == nil {
		t.Errorf("缺省应落到第一个可见集群: %d %v", code, v["cluster"])
	}
	_, lst := admin.do("GET", "/api/clusters", nil)
	if cs, _ := lst["clusters"].([]any); len(cs) != 2 || lst["primary"] != "" {
		t.Errorf("集群接入列表: %v", lst)
	}

}

// 独立部署的平台没有本机 tiup 可做自动探测：live 集群必须填 PD 地址。
func TestStandaloneLiveRequiresPD(t *testing.T) {
	e := newStandalone(t)
	admin := e.login("admin")
	code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "prod2", "env": "prod", "mode": "live", "mysqlHost": "10.0.0.1", "mysqlUser": "ro"})
	if code != 400 || out["code"] != "invalid" {
		t.Fatalf("live 没填 PD 应 400: %d %v", code, out)
	}
	if code, out := admin.do("POST", "/api/clusters", map[string]any{"id": "prod2", "env": "prod", "mode": "live", "pd": "http://10.0.0.21:2379"}); code != 200 {
		t.Fatalf("填了 PD 应可接入: %d %v", code, out)
	}
}

// 平台实例不计入 20 个集群的上限。
func TestStandalonePlatformNotCountedInLimit(t *testing.T) {
	r, err := clusters.Open("", hubMaster, clusters.Spec{ID: clusters.PlatformID, Name: clusters.PlatformName, Env: "prod", Mode: clusters.ModeLive, Platform: true}, clusters.Options{Max: 2, RequirePD: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aa", "bb"} {
		spec, err := r.PrepareAdd(clusters.Input{ID: id, Env: ps("prod"), Mode: ps("live"), PD: ps("http://10.0.0.1:2379")}, "admin")
		if err != nil {
			t.Fatalf("添加 %s: %v", id, err)
		}
		if err := r.Put(spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.PrepareAdd(clusters.Input{ID: "cc", Env: ps("prod"), Mode: ps("live"), PD: ps("http://10.0.0.1:2379")}, "admin"); err == nil {
		t.Error("超过上限应拒绝")
	}
}

func ps(s string) *string { return &s }
