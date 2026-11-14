// @author: kevinkelly
// @license: Apache License 2.0
// @file: clusters_test.go
// @time: 2026/10/28

package clusters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var master = []byte("master-key-for-tests")

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

func primarySpec() Spec {
	return Spec{ID: "prod-tidb-01", Name: "prod-tidb-01", Display: "prod-tidb-01", Env: "prod", Mode: ModeReplay}
}

// fixturesRoot 造一个白名单根目录，下面有 stg 数据包目录。
func fixturesRoot(t *testing.T) (root, stg string) {
	t.Helper()
	root = t.TempDir()
	stg = filepath.Join(root, "stg")
	if err := os.MkdirAll(stg, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, stg
}

func openReg(t *testing.T, path string, roots []string) *Registry {
	t.Helper()
	r, err := Open(path, master, primarySpec(), Options{FixturesRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"prod-tidb-01": "prod-tidb-01", "Prod_TiDB.01": "prod-tidb-01", "9lives": "c-9lives", "": "cluster", "a": "a0",
		strings.Repeat("x", 80): strings.Repeat("x", 32),
	} {
		got := Slug(in)
		if got != want || !ValidID(got) {
			t.Errorf("Slug(%q)=%q 期望 %q（且合法）", in, got, want)
		}
	}
}

func TestValidateIDName(t *testing.T) {
	good := Spec{ID: "stg", Env: "staging", Mode: ModeLive}
	if err := Validate(&good, nil); err != nil || good.Name != "stg" || good.Display != "stg" {
		t.Fatalf("默认值: %v %+v", err, good)
	}
	for _, id := range []string{"", "a", "A1", "1ab", "a_b", "ab cd", "ab/cd", "ab..", strings.Repeat("a", 33), "STG", "stg\n"} {
		s := Spec{ID: id, Env: "test", Mode: ModeLive}
		if err := Validate(&s, nil); code(err) != ErrInvalid {
			t.Errorf("id %q 应不合法: %v", id, err)
		}
	}
	for _, n := range []string{"has space", "a/b", "a;b", "名字", strings.Repeat("a", 65), "a\nb", "$(x)"} {
		s := Spec{ID: "stg", Name: n, Env: "test", Mode: ModeLive}
		if err := Validate(&s, nil); code(err) != ErrInvalid {
			t.Errorf("name %q 应不合法: %v", n, err)
		}
	}
	for _, c := range []Spec{
		{ID: "stg", Env: "dev", Mode: ModeLive},
		{ID: "stg", Env: "prod", Mode: "weird"},
		{ID: "stg", Env: "prod", Mode: ModeLive, Display: "a\nb"},
		{ID: "stg", Env: "prod", Mode: ModeLive, Desc: "x\x00y"},
		{ID: "stg", Env: "prod", Mode: ModeLive, Display: strings.Repeat("长", 41)},
	} {
		c := c
		if err := Validate(&c, nil); code(err) != ErrInvalid {
			t.Errorf("%+v 应不合法: %v", c, err)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"http://10.0.1.21:2379":  "http://10.0.1.21:2379",
		"https://prom.example/":  "https://prom.example",
		"http://pd.local:2379/x": "http://pd.local:2379/x",
		"http://[::1]:9090":      "http://[::1]:9090",
		"":                       "",
	}
	for in, want := range ok {
		got, err := NormalizeURL("pd", in)
		if err != nil || got != want {
			t.Errorf("%q → %q,%v 期望 %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"ftp://10.0.0.1", "file:///etc/passwd", "10.0.0.1:2379", "http://", "http://:2379", "http://user:pw@10.0.0.1:2379", "http://user@10.0.0.1",
		"http://10.0.0.1:99999", "http://10.0.0.1:0", "http://10.0.0.1:abc", "http://10.0.0.1/\x00", "http://10.0.0.1/a b", "http://10.0.0.1/?token=x",
		"http://10.0.0.1/#x", "http://10.0.\n0.1", "http://exa mple.com", "javascript:alert(1)", "http://-bad-.com", "http://a\\b",
	} {
		if _, err := NormalizeURL("pd", bad); code(err) != ErrInvalid {
			t.Errorf("%q 应不合法，得到 %v", bad, err)
		}
	}
}

func TestResolveFixturesWhitelist(t *testing.T) {
	root, stg := fixturesRoot(t)
	if got, err := ResolveFixtures(stg, []string{root}); err != nil || got == "" {
		t.Fatalf("白名单内应通过: %v", err)
	}
	if _, err := ResolveFixtures(root, []string{root}); err != nil {
		t.Fatalf("根目录自身也允许: %v", err)
	}
	outside := t.TempDir()
	for _, p := range []string{outside, filepath.Join(stg, "..", "..", filepath.Base(outside)), filepath.Join(root, "nope"), "", "/etc", "\x00"} {
		if _, err := ResolveFixtures(p, []string{root}); code(err) != ErrInvalid {
			t.Errorf("%q 应被拒绝: %v", p, err)
		}
	}
	if _, err := ResolveFixtures(stg, nil); code(err) != ErrInvalid {
		t.Errorf("没有白名单根目录应拒绝: %v", err)
	}
	// 同名前缀的兄弟目录不能蒙混过关：root=/a/fx，/a/fx-evil 不在其下
	evil := root + "-evil"
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(evil)
	if _, err := ResolveFixtures(evil, []string{root}); code(err) != ErrInvalid {
		t.Errorf("前缀相同的兄弟目录应被拒绝: %v", err)
	}
	// 符号链接指向白名单之外：解析后比较，必须拒绝
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := ResolveFixtures(link, []string{root}); code(err) != ErrInvalid {
			t.Errorf("指向白名单外的符号链接应被拒绝: %v", err)
		}
	}
}

func TestRegistryAddValidationAndLimits(t *testing.T) {
	root, stg := fixturesRoot(t)
	r := openReg(t, "", []string{root})
	add := func(in Input) (Spec, error) { return r.PrepareAdd(in, "admin") }

	if _, err := add(Input{ID: "stg", Env: sp("staging"), Mode: sp(ModeReplay)}); code(err) != ErrInvalid {
		t.Errorf("replay 缺 fixtures: %v", err)
	}
	s, err := add(Input{ID: "stg", Name: sp("stg-tidb-01"), Env: sp("staging"), Mode: sp(ModeReplay), Fixtures: &stg, MySQLPassword: sp("should-be-dropped")})
	if err != nil {
		t.Fatal(err)
	}
	if s.MySQLPassword != "" || s.CreatedBy != "admin" || s.Created.IsZero() {
		t.Errorf("replay 不应保存连接凭据: %+v", s)
	}
	if err := r.Put(s); err != nil {
		t.Fatal(err)
	}
	// 重复 id / name / 与主集群冲突
	for name, in := range map[string]Input{
		"同 id":           {ID: "stg", Env: sp("test"), Mode: sp(ModeLive)},
		"name 撞已有 name":  {ID: "other", Name: sp("stg-tidb-01"), Env: sp("test"), Mode: sp(ModeLive)},
		"name 撞已有 id":    {ID: "other", Name: sp("stg"), Env: sp("test"), Mode: sp(ModeLive)},
		"id 撞已有 name":    {ID: "stg-tidb-01", Env: sp("test"), Mode: sp(ModeLive)},
		"id 撞主集群":        {ID: "prod-tidb-01", Env: sp("test"), Mode: sp(ModeLive)},
		"name 撞主集群 name": {ID: "other", Name: sp("prod-tidb-01"), Env: sp("test"), Mode: sp(ModeLive)},
	} {
		if _, err := add(in); code(err) != ErrExists {
			t.Errorf("%s 应 exists: %v", name, err)
		}
	}
	// 非法 URL / 凭据在 URL 里
	for _, in := range []Input{
		{ID: "bad1", Env: sp("test"), Mode: sp(ModeLive), PD: sp("http://u:p@10.0.0.1:2379")},
		{ID: "bad2", Env: sp("test"), Mode: sp(ModeLive), Prom: sp("ftp://x")},
		{ID: "bad3", Env: sp("test"), Mode: sp(ModeLive), Alert: sp("http://")},
		{ID: "bad4", Env: sp("test"), Mode: sp(ModeLive), MySQLHost: sp("h"), MySQLUser: sp("")},
		{ID: "bad5", Env: sp("test"), Mode: sp(ModeLive), MySQLPort: ip(70000)},
		{ID: "bad6", Env: sp("test"), Mode: sp(ModeLive), Tiup: sp("/usr/bin/rm")},
		{ID: "bad7", Env: sp("test"), Mode: sp(ModeLive), Tiup: sp("tiup")},
		{ID: "bad8", Env: sp("test"), Mode: sp(ModeLive), MySQLBin: sp("/bin/sh")},
		{ID: "bad9", Env: sp("test"), Mode: sp(ModeLive), Home: sp("/home/a b")},
		{ID: "bad10", Env: sp("test"), Mode: sp(ModeLive), MySQLPassword: sp("a\nb")},
	} {
		if _, err := add(in); code(err) != ErrInvalid {
			t.Errorf("%+v 应 invalid: %v", in, err)
		}
	}
	// 上限：含主集群共 20 个
	for i := 0; ; i++ {
		id := "c" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		sp2, err := add(Input{ID: id, Env: sp("test"), Mode: sp(ModeLive)})
		if err != nil {
			if code(err) != ErrLimit {
				t.Fatalf("应因上限失败: %v", err)
			}
			break
		}
		if err := r.Put(sp2); err != nil {
			t.Fatal(err)
		}
		if i > 40 {
			t.Fatal("上限没有生效")
		}
	}
	if n := len(r.List()); n != MaxClusters {
		t.Errorf("集群数 %d 应为上限 %d（含主集群）", n, MaxClusters)
	}
}

func TestRegistryPrimaryRules(t *testing.T) {
	r := openReg(t, filepath.Join(t.TempDir(), "clusters.json"), nil)
	if l := r.List(); len(l) != 1 || !l[0].Primary || l[0].ID != "prod-tidb-01" {
		t.Fatalf("主集群应是虚拟条目: %+v", l)
	}
	if _, err := r.Remove("prod-tidb-01"); code(err) != ErrPrimary {
		t.Errorf("主集群不可删: %v", err)
	}
	for _, in := range []Input{{Mode: sp(ModeLive)}, {Env: sp("test")}, {Name: sp("other")}, {PD: sp("http://10.0.0.1:2379")}, {ClearPassword: true}, {MySQLPassword: sp("x")}} {
		if _, _, err := r.PrepareUpdate("prod-tidb-01", in, "admin"); code(err) != ErrPrimary {
			t.Errorf("%+v 对主集群应 primary 错误: %v", in, err)
		}
	}
	// 重复提交与当前相同的值是允许的（前端会整体提交）
	if _, _, err := r.PrepareUpdate("prod-tidb-01", Input{Mode: sp(ModeReplay), Env: sp("prod"), Name: sp("prod-tidb-01")}, "admin"); err != nil {
		t.Errorf("相同值应允许: %v", err)
	}
	old, next, err := r.PrepareUpdate("prod-tidb-01", Input{Display: sp("生产主集群"), Desc: sp("说明")}, "admin")
	if err != nil || next.Display != "生产主集群" || old.Display == next.Display {
		t.Fatalf("改 display / desc: %v %+v", err, next)
	}
	if err := r.Put(next); err != nil {
		t.Fatal(err)
	}
	// 重启后 display / desc 仍在，且主集群的连接信息不入文件
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(r.path), "clusters.json"))
	if !strings.Contains(string(b), "生产主集群") {
		t.Errorf("clusters.json 应保存主集群的 display: %s", b)
	}
	r2 := openReg(t, r.path, nil)
	if p := r2.Primary(); p.Display != "生产主集群" || p.Desc != "说明" || p.Env != "prod" {
		t.Errorf("重启后主集群: %+v", p)
	}
	if _, _, err := r.PrepareUpdate("nope", Input{}, "a"); code(err) != ErrNotFound {
		t.Errorf("不存在: %v", err)
	}
	if _, _, err := r.PrepareUpdate("prod-tidb-01", Input{ID: "other"}, "a"); code(err) != ErrInvalid {
		t.Errorf("id 不可改: %v", err)
	}
}

func TestPasswordEncryptedAtRestAndNeverEchoed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.json")
	r := openReg(t, path, nil)
	const pw = "S3cret-Passw0rd!xyz"
	s, err := r.PrepareAdd(Input{ID: "stg", Env: sp("staging"), Mode: sp(ModeLive), MySQLHost: sp("10.0.0.1"), MySQLUser: sp("ro"), MySQLPassword: sp(pw), PD: sp("http://10.0.0.1:2379")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Put(s); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), pw) {
		t.Fatalf("clusters.json 不能含明文密码:\n%s", raw)
	}
	if !strings.Contains(string(raw), "mysqlPasswordEnc") || !strings.Contains(string(raw), "enc1:") {
		t.Errorf("应保存密文: %s", raw)
	}
	// View / JSON 永不含密码，只有 hasPassword
	v := ViewOf(s)
	jb, _ := json.Marshal(v)
	if strings.Contains(string(jb), pw) || !v.HasPassword || !strings.Contains(string(jb), `"hasPassword":true`) {
		t.Errorf("view: %s", jb)
	}
	if strings.Contains(string(jb), "mysqlPassword") {
		t.Errorf("view 不应含 mysqlPassword 字段: %s", jb)
	}
	// 重启恢复，密码可用
	r2 := openReg(t, path, nil)
	got, ok := r2.Get("stg")
	if !ok || got.MySQLPassword != pw || got.LoadError != "" {
		t.Fatalf("重启后应恢复: %+v", got)
	}
	// 更新留空 = 不改；清除；换新
	_, n1, err := r2.PrepareUpdate("stg", Input{Display: sp("预发")}, "admin")
	if err != nil || n1.MySQLPassword != pw {
		t.Fatalf("留空应不改: %v %q", err, n1.MySQLPassword)
	}
	_, n2, _ := r2.PrepareUpdate("stg", Input{MySQLPassword: sp("")}, "admin")
	if n2.MySQLPassword != pw {
		t.Error("空串也是不改")
	}
	_, n3, _ := r2.PrepareUpdate("stg", Input{ClearPassword: true}, "admin")
	if n3.HasPassword() {
		t.Error("clearPassword 应清除")
	}
	_, n4, _ := r2.PrepareUpdate("stg", Input{MySQLPassword: sp("new-pw-123")}, "admin")
	if n4.MySQLPassword != "new-pw-123" {
		t.Error("应换新密码")
	}
	// 换了主密钥：条目标 LoadError，不丢密文；重新填密码后恢复
	r3, err := Open(path, []byte("another-master-key"), primarySpec(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := r3.Get("stg")
	if bad.LoadError == "" || bad.MySQLPassword != "" || !bad.HasPassword() {
		t.Fatalf("主密钥不对应标记 LoadError: %+v", bad)
	}
	_, fixed, err := r3.PrepareUpdate("stg", Input{MySQLPassword: sp("brand-new-pw")}, "admin")
	if err != nil || fixed.LoadError != "" {
		t.Fatalf("重填密码应清除错误: %v %+v", err, fixed)
	}
	// 没有主密钥时拒绝以明文保存
	r4, _ := Open(filepath.Join(t.TempDir(), "c.json"), nil, primarySpec(), Options{})
	s4, _ := r4.PrepareAdd(Input{ID: "xx", Env: sp("test"), Mode: sp(ModeLive), MySQLHost: sp("h1"), MySQLUser: sp("u"), MySQLPassword: sp("pw")}, "a")
	if err := r4.Put(s4); code(err) != ErrStore {
		t.Errorf("无主密钥不应保存密码: %v", err)
	}
	if len(r4.List()) != 1 {
		t.Error("保存失败应回滚内存状态")
	}
}

func TestRegistryPersistRemoveAndKeepUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.json")
	r := openReg(t, path, nil)
	for _, id := range []string{"aa", "bb"} {
		s, err := r.PrepareAdd(Input{ID: id, Env: sp("test"), Mode: sp(ModeLive)}, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("clusters.json 应为 0600: %v %v", st, err)
	}
	r2 := openReg(t, path, nil)
	if ids := idsOf(r2); strings.Join(ids, ",") != "prod-tidb-01,aa,bb" {
		t.Fatalf("重启后顺序: %v", ids)
	}
	if _, err := r2.Remove("aa"); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Remove("aa"); code(err) != ErrNotFound {
		t.Errorf("重复删除: %v", err)
	}
	if ids := idsOf(openReg(t, path, nil)); strings.Join(ids, ",") != "prod-tidb-01,bb" {
		t.Errorf("删除应持久化: %v", ids)
	}
	// 手工编辑出的坏条目：不加载、告警，但保存时原样保留（不静默丢失）
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	doc["clusters"] = append(doc["clusters"].([]any), map[string]any{"id": "Bad ID", "env": "prod", "note": "手写"})
	b, _ := json.Marshal(doc)
	_ = os.WriteFile(path, b, 0o600)
	r3 := openReg(t, path, nil)
	if len(r3.Warnings()) == 0 || len(r3.List()) != 2 {
		t.Fatalf("应告警且不加载: %v %v", r3.Warnings(), idsOf(r3))
	}
	s, _ := r3.PrepareAdd(Input{ID: "cc", Env: sp("test"), Mode: sp(ModeLive)}, "a")
	if err := r3.Put(s); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "Bad ID") {
		t.Errorf("保存时应保留无法识别的条目: %s", after)
	}
	// 损坏的 JSON：Open 报错
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := Open(path, master, primarySpec(), Options{}); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

func idsOf(r *Registry) []string {
	var out []string
	for _, s := range r.List() {
		out = append(out, s.ID)
	}
	return out
}

func TestResolve(t *testing.T) {
	r := openReg(t, "", nil)
	s, _ := r.PrepareAdd(Input{ID: "stg", Name: sp("stg-tidb-01"), Env: sp("staging"), Mode: sp(ModeLive)}, "a")
	_ = r.Put(s)
	if got := r.Resolve("stg"); strings.Join(got, ",") != "stg,stg-tidb-01" {
		t.Errorf("%v", got)
	}
	if got := r.Resolve("stg-tidb-01"); strings.Join(got, ",") != "stg,stg-tidb-01" {
		t.Errorf("%v", got)
	}
	if got := r.Resolve("whatever"); len(got) != 1 || got[0] != "whatever" {
		t.Errorf("%v", got)
	}
}

// ---------------------------------------------------------------- 连通性测试 ----

func TestProberLiveNoCredentialLeak(t *testing.T) {
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pd/api/v1/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"health":true},{"health":false}]`))
	}))
	defer pd.Close()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" && r.URL.Query().Get("query") == "up" {
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			return
		}
		http.Error(w, "x", 500)
	}))
	defer prom.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
	}))
	defer redirect.Close()
	const pw = "p@ss-word-9999"
	var gotEnv []string
	var gotArgv [][]string
	pr := Prober{DefaultTiup: "/usr/bin/tiup", Exec: func(ctx context.Context, argv []string, env []string) ([]byte, error) {
		gotEnv = append(gotEnv, env...)
		gotArgv = append(gotArgv, argv)
		if argv[0] == "/usr/bin/mysql" {
			return nil, errors.New("access denied for password " + pw)
		}
		return []byte("Cluster type: tidb\nCluster version: v8.1.5\n"), nil
	}}
	items := pr.Run(context.Background(), Spec{ID: "stg", Name: "stg-tidb-01", Mode: ModeLive, PD: pd.URL, Prom: prom.URL, Alert: redirect.URL,
		MySQLHost: "10.0.0.1", MySQLUser: "ro", MySQLPassword: pw, MySQLBin: "/usr/bin/mysql"})
	by := map[string]TestItem{}
	for _, it := range items {
		by[strings.SplitN(it.Item, "（", 2)[0]] = it
		if strings.Contains(it.Detail, pw) || strings.Contains(it.Item, pw) {
			t.Errorf("探测结果泄露了密码: %+v", it)
		}
	}
	if it := by["PD 健康"]; it.Status != "warn" || !strings.Contains(it.Detail, "1 / 2") {
		t.Errorf("PD: %+v", it)
	}
	if it := by["Prometheus 查询"]; it.Status != "ok" {
		t.Errorf("Prom: %+v", it)
	}
	if it := by["Alertmanager"]; it.Status != "bad" {
		t.Errorf("重定向不应被跟随，应判失败: %+v", it)
	}
	if it := by["TiDB 状态端口"]; it.Status != "warn" {
		t.Errorf("未配置应 warn: %+v", it)
	}
	if it := by["TiDB SQL"]; it.Status != "bad" || !strings.Contains(it.Detail, "***") {
		t.Errorf("SQL 失败信息应被脱敏: %+v", it)
	}
	tiup := items[len(items)-1]
	if tiup.Status != "ok" || !strings.Contains(tiup.Detail, "v8.1.5") {
		t.Errorf("tiup: %+v", tiup)
	}
	// 密码只经环境变量，不进 argv
	for _, a := range gotArgv {
		if strings.Contains(strings.Join(a, " "), pw) {
			t.Errorf("密码不应出现在 argv: %v", a)
		}
	}
	if len(gotEnv) != 1 || gotEnv[0] != "MYSQL_PWD="+pw {
		t.Errorf("env: %v", gotEnv)
	}
	// 只跑了固定的只读命令
	for _, a := range gotArgv {
		switch {
		case a[0] == "/usr/bin/mysql" && a[len(a)-1] == "SELECT 1":
		case a[0] == "/usr/bin/tiup" && strings.Join(a[1:], " ") == "cluster display stg-tidb-01":
		default:
			t.Errorf("出现了非预期的命令: %v", a)
		}
	}
}

func TestProberTimeoutAndReplay(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	pr := Prober{Timeout: 150 * time.Millisecond, Exec: func(ctx context.Context, argv []string, env []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	t0 := time.Now()
	items := pr.Run(context.Background(), Spec{ID: "stg", Name: "stg", Mode: ModeLive, PD: slow.URL})
	if time.Since(t0) > 3*time.Second {
		t.Errorf("每项应在超时内返回，耗时 %v", time.Since(t0))
	}
	if items[0].Status != "bad" {
		t.Errorf("超时应 bad: %+v", items[0])
	}
	// replay：manifest 不可读 → bad
	dir := t.TempDir()
	if it := pr.Run(context.Background(), Spec{Mode: ModeReplay, Fixtures: dir})[0]; it.Status != "bad" {
		t.Errorf("无 manifest 应 bad: %+v", it)
	}
}

// 随仓库提供的示例文件必须能被注册表加载，且其中的 live 条目通过校验。
func TestExampleFileLoads(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "ops", "clusters.json.example"))
	if err != nil {
		t.Skip("没有示例文件")
	}
	path := filepath.Join(t.TempDir(), "clusters.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, master, primarySpec(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings()) != 0 {
		t.Errorf("示例文件加载告警: %v", r.Warnings())
	}
	stg, ok := r.Get("stg")
	if !ok || stg.LoadError != "" || stg.Mode != ModeLive || stg.PD != "http://10.20.1.21:2379" || stg.Env != "staging" {
		t.Errorf("stg: %+v", stg)
	}
	if p := r.Primary(); p.Display != "生产主集群" {
		t.Errorf("主集群 display: %q", p.Display)
	}
	if dev, ok := r.Get("dev"); !ok || dev.LoadError == "" {
		t.Errorf("dev 的示例路径不在白名单下，应带 LoadError: %+v", dev)
	}
}
