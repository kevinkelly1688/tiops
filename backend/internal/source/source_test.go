// @author: kevinkelly
// @license: Apache License 2.0
// @file: source_test.go
// @time: 2026/10/28

package source_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tidbops/internal/fakecluster"
	"tidbops/internal/fixtures"
	"tidbops/internal/source"
)

func TestValidateCmd(t *testing.T) {
	ok := [][]string{
		{"tiup", "cluster", "list"}, {"tiup", "--version"}, {"tiup", "cluster", "display", "prod-tidb-01"},
		{"hostname", "-I"}, {"uname", "-r"}, {"free", "-m"}, {"cat", "/etc/os-release"},
	}
	for _, a := range ok {
		if err := source.ValidateCmd(a); err != nil {
			t.Errorf("%v 应放行: %v", a, err)
		}
	}
	bad := [][]string{
		{"tiup", "cluster", "destroy", "prod"}, {"tiup", "cluster", "display", "a;rm"}, {"tiup", "cluster", "display", "a b"},
		{"rm", "-rf", "/"}, {"cat", "/etc/shadow"}, {"bash", "-c", "id"}, {"tiup", "cluster", "restart", "prod", "-N", "x"},
		{"tiup", "cluster", "display", "$(id)"}, {}, {"hostname", "-f"},
	}
	for _, a := range bad {
		if err := source.ValidateCmd(a); err == nil {
			t.Errorf("%v 应被拒绝", a)
		}
	}
}

func TestValidateSQL(t *testing.T) {
	ok := []string{
		"SELECT tidb_version()",
		"SELECT * FROM information_schema.cluster_info LIMIT 500",
		"SELECT a FROM information_schema.cluster_slow_query WHERE Time > NOW() - INTERVAL 1 HOUR ORDER BY Query_time DESC LIMIT 200",
		"select x from performance_schema.events_statements_summary_by_digest limit 10",
	}
	for _, q := range ok {
		if err := source.ValidateSQL(q); err != nil {
			t.Errorf("应放行 %q: %v", q, err)
		}
	}
	bad := []string{
		"", "DROP TABLE t", "DELETE FROM information_schema.x LIMIT 1", "UPDATE t SET a=1",
		"SELECT * FROM mysql.user LIMIT 10", "SELECT * FROM shop.orders LIMIT 10",
		"SELECT * FROM information_schema.cluster_info", "SELECT * FROM information_schema.cluster_info LIMIT 100000",
		"SELECT * FROM information_schema.a LIMIT 1; DROP TABLE t", "SELECT * FROM information_schema.a LIMIT 1 -- x",
		"SELECT * FROM information_schema.a /* x */ LIMIT 1", "SELECT 1", "SELECT SLEEP(10) FROM information_schema.a LIMIT 1",
		"SELECT * FROM information_schema.a JOIN shop.t ON 1=1 LIMIT 1", "SELECT * FROM information_schema.a LIMIT 1 FOR UPDATE",
		"SELECT * INTO OUTFILE '/tmp/x' FROM information_schema.a LIMIT 1",
	}
	for _, q := range bad {
		if err := source.ValidateSQL(q); err == nil {
			t.Errorf("应拒绝 %q", q)
		}
	}
}

func TestValidateItemHTTPAndFile(t *testing.T) {
	good := source.Item{Name: "pd/x", Kind: source.KindHTTP, Endpoint: "pd", Path: "/pd/api/v1/stores"}
	if err := source.ValidateItem(good); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"pd/api", "//evil.com/x", "http://evil.com/x", "/a b", "/a\nb"} {
		it := good
		it.Path = p
		if source.ValidateItem(it) == nil {
			t.Errorf("路径 %q 应被拒绝", p)
		}
	}
	it := good
	it.Endpoint = "internal"
	if source.ValidateItem(it) == nil {
		t.Error("未知端点应被拒绝")
	}
	for _, f := range []string{"/home/tidb/.tiup/storage/cluster/clusters/prod/meta.yaml", "/etc/os-release"} {
		if err := source.ValidateFile(f); err != nil {
			t.Errorf("%s 应放行: %v", f, err)
		}
	}
	for _, f := range []string{"/etc/passwd", "/home/tidb/.ssh/id_rsa", "/home/tidb/.tiup/storage/cluster/clusters/../x/meta.yaml", "/home/tidb/.tiup/storage/cluster/clusters/prod/ssh/id_rsa"} {
		if source.ValidateFile(f) == nil {
			t.Errorf("%s 应被拒绝", f)
		}
	}
}

// 默认采集目录里的每一项都必须通过白名单校验——这是「只读保证」的回归测试。
func TestDefaultCatalogAllValid(t *testing.T) {
	items := source.DefaultCatalog(source.CatalogParams{Cluster: "prod-tidb-01", Home: "/home/tidb", Now: time.Now()})
	if len(items) < 30 {
		t.Fatalf("目录项过少: %d", len(items))
	}
	seen := map[string]bool{}
	for _, it := range items {
		if err := source.ValidateItem(it); err != nil {
			t.Errorf("目录项未通过校验: %v", err)
		}
		if err := fixtures.ValidName(it.Name); err != nil {
			t.Error(err)
		}
		if seen[it.Name] {
			t.Errorf("条目名重复: %s", it.Name)
		}
		seen[it.Name] = true
	}
	at := source.Attest(items)
	if len(at.HTTPMethods) != 1 || at.HTTPMethods[0] != "GET" {
		t.Errorf("只读声明的 HTTP 方法应只有 GET: %v", at.HTTPMethods)
	}
}

func TestLiveRejectsUnsafeItems(t *testing.T) {
	var exec int32
	l := source.NewLive(source.LiveConfig{Exec: func(context.Context, []string, []string) ([]byte, error) {
		atomic.AddInt32(&exec, 1)
		return nil, nil
	}})
	for _, it := range []source.Item{
		{Name: "x/destroy", Kind: source.KindCmd, Argv: []string{"tiup", "cluster", "destroy", "prod"}},
		{Name: "x/drop", Kind: source.KindSQL, Query: "DROP TABLE t"},
		{Name: "x/shadow", Kind: source.KindFile, File: "/etc/shadow"},
	} {
		if _, err := l.Fetch(context.Background(), it); err == nil {
			t.Errorf("%s 应被拒绝", it.Name)
		}
	}
	if atomic.LoadInt32(&exec) != 0 {
		t.Error("被拒绝的请求不应触达执行函数")
	}
}

func TestLiveHTTPOnlyGETAndNoRedirect(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	l := source.NewLive(source.LiveConfig{Endpoints: map[string]string{"pd": srv.URL}})
	if b, err := l.Fetch(context.Background(), source.Item{Name: "pd/a", Kind: source.KindHTTP, Endpoint: "pd", Path: "/a"}); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("GET 失败: %v %s", err, b)
	}
	if _, err := l.Fetch(context.Background(), source.Item{Name: "pd/r", Kind: source.KindHTTP, Endpoint: "pd", Path: "/redir"}); err == nil {
		t.Error("重定向应被视为失败，而不是跟随")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("出现了非 GET 请求: %s", m)
		}
	}
	if _, err := l.Fetch(context.Background(), source.Item{Name: "tidb/s", Kind: source.KindHTTP, Endpoint: "tidb", Path: "/status"}); err == nil {
		t.Error("未配置的端点应报错")
	}
}

func TestParseDisplayAndEndpoints(t *testing.T) {
	b, err := fakecluster.Exec()(context.Background(), []string{"tiup", "cluster", "display", "prod-tidb-01"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ci := source.ParseDisplay(string(b))
	if ci.Name != "prod-tidb-01" || ci.Version != "v8.5.2" || ci.User != "tidb" || len(ci.Instances) != 19 {
		t.Fatalf("解析结果不对: %+v", ci)
	}
	ep, host, port := ci.Endpoints()
	if ep["pd"] != "http://10.0.1.21:2379" || ep["tidb"] != "http://10.0.1.11:10080" || ep["prom"] != "http://10.0.1.31:9090" || ep["alert"] != "http://10.0.1.31:9093" {
		t.Errorf("端点推导不对: %v", ep)
	}
	if host != "10.0.1.11" || port != 4000 {
		t.Errorf("MySQL 地址不对: %s:%d", host, port)
	}
	lb, _ := fakecluster.Exec()(context.Background(), []string{"tiup", "cluster", "list"}, nil)
	if names := source.ParseClusterList(string(lb)); len(names) != 1 || names[0] != "prod-tidb-01" {
		t.Errorf("集群列表解析不对: %v", names)
	}
	if ci := source.ParseDisplay("garbage"); ci.Name != "" || len(ci.Instances) != 0 {
		t.Error("乱码输入不应产生结果")
	}
}

// 端到端：模拟集群 → Live → Recorder（脱敏落盘）→ 校验 → Replay 读回，并检查脱敏与篡改检测。
func TestRecordVerifyReplay(t *testing.T) {
	c := fakecluster.Start("prod-tidb-01")
	defer c.Close()
	live := source.NewLive(source.LiveConfig{
		Endpoints: c.Endpoints(), MySQL: source.MySQLConfig{Host: "10.0.1.11", User: "ro"},
		Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile,
	})
	root := filepath.Join(t.TempDir(), "pkg")
	items := source.DefaultCatalog(source.CatalogParams{Cluster: "prod-tidb-01", Home: "/home/tidb", Now: time.Now()})
	opt := fixtures.Options{MaskSQLLiterals: true, RedactSecrets: true}
	rec := source.NewRecorder(live, root, "prod-tidb-01", "test", opt)
	rec.RecordAll(context.Background(), items)
	if err := rec.Finish(items); err != nil {
		t.Fatal(err)
	}
	ok, failed, _ := rec.Man.Summary()
	if failed != 0 || ok < 30 {
		t.Fatalf("采集结果异常: %s", rec.Man)
	}

	// 实际发出的请求全部是白名单内的 GET / 命令
	for _, e := range live.Executed {
		if strings.HasPrefix(e, "GET ") || strings.HasPrefix(e, "EXEC ") || strings.HasPrefix(e, "SQL ") || strings.HasPrefix(e, "READ ") {
			continue
		}
		t.Errorf("出现未知请求种类: %s", e)
	}

	// 回放
	rp, err := source.NewReplay(root)
	if err != nil {
		t.Fatal(err)
	}
	var stores struct {
		Count  int `json:"count"`
		Stores []struct {
			Store struct {
				ID int `json:"id"`
			} `json:"store"`
		} `json:"stores"`
	}
	if err := rp.GetJSON(context.Background(), "pd/stores", &stores); err != nil || stores.Count != 8 || len(stores.Stores) != 8 {
		t.Fatalf("回放 pd/stores 失败: %v %+v", err, stores)
	}
	if rp.Mode() != "replay" || len(rp.Manifest().Items) != len(items) {
		t.Error("清单条目数应与目录一致")
	}

	// 脱敏：慢 SQL 的手机号与字面量、meta.yaml 的密码都不应出现
	slow, _ := rp.Fetch(context.Background(), source.Item{Name: "sql/slow-query"})
	if strings.Contains(string(slow), "13800138000") || strings.Contains(string(slow), "10086") {
		t.Errorf("慢 SQL 未脱敏:\n%s", slow)
	}
	if !strings.Contains(string(slow), "2.91") || !strings.Contains(string(slow), "a3f9c1") {
		t.Errorf("耗时与 digest 不应被改动:\n%s", slow)
	}
	meta, _ := rp.Fetch(context.Background(), source.Item{Name: "tiup/meta"})
	if strings.Contains(string(meta), "not-a-real-password") {
		t.Errorf("meta.yaml 密码未脱敏:\n%s", meta)
	}

	// 清单只读声明
	m := rp.Manifest()
	if len(m.ReadOnly.HTTPMethods) != 1 || m.ReadOnly.HTTPMethods[0] != "GET" || len(m.ReadOnly.Commands) == 0 || len(m.ReadOnly.SQL) == 0 {
		t.Errorf("只读声明不完整: %+v", m.ReadOnly)
	}
	// 清单里不应出现真实地址（只含逻辑名）
	raw, _ := os.ReadFile(filepath.Join(root, "manifest.json"))
	if strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), c.PD.URL) {
		t.Error("manifest 不应包含实际访问的地址")
	}

	// 校验通过 → 篡改后失败
	d := fixtures.Dir{Root: root}
	if errs := d.Verify(m); len(errs) != 0 {
		t.Fatalf("未篡改时应通过: %v", errs)
	}
	p := filepath.Join(root, "data", "pd", "stores.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"count": 8`, `"count": 9`, 1)), 0o644)
	if _, err := rp.Fetch(context.Background(), source.Item{Name: "pd/stores"}); err == nil {
		t.Error("篡改后的回放读取应失败（哈希不一致）")
	}
	// 未录制的条目
	if _, err := rp.Fetch(context.Background(), source.Item{Name: "pd/nope"}); err == nil || !strings.Contains(err.Error(), "没有该条目") {
		t.Errorf("未录制的条目应返回明确错误: %v", err)
	}
}

func TestRecordKeepsGoingOnFailure(t *testing.T) {
	// alert 端点不配置：可选项失败，不影响其他项
	c := fakecluster.Start("c")
	defer c.Close()
	ep := c.Endpoints()
	delete(ep, "alert")
	live := source.NewLive(source.LiveConfig{Endpoints: ep, Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile})
	items := source.FilterItems(source.DefaultCatalog(source.CatalogParams{Cluster: "c", Home: "/home/tidb"}), []string{"pd/", "alert/"}, nil)
	rec := source.NewRecorder(live, filepath.Join(t.TempDir(), "p"), "c", "test", fixtures.Options{})
	rec.RecordAll(context.Background(), items)
	ok, failed, optFailed := rec.Man.Summary()
	if ok == 0 || failed != 0 || optFailed != 1 {
		t.Errorf("Summary = ok %d failed %d optFailed %d", ok, failed, optFailed)
	}
	r := rec.Man.Find("alert/alerts")
	if r == nil || r.OK || r.Error == "" {
		t.Errorf("失败原因应记录在清单中: %+v", r)
	}
}

func TestJSONManifestStable(t *testing.T) {
	m := &fixtures.Manifest{Schema: 1}
	m.Add(fixtures.ItemRecord{Name: "b"})
	m.Add(fixtures.ItemRecord{Name: "a"})
	b, _ := json.Marshal(m.Items)
	if !strings.HasPrefix(string(b), `[{"name":"a"`) {
		t.Errorf("清单条目应按名称排序，使输出稳定: %s", b)
	}
}
