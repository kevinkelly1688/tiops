// @author: kevinkelly
// @license: Apache License 2.0
// @file: deploy_api_test.go
// @time: 2026/10/28

package server_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/collect"
	"tidbops/internal/deploy"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

type apiRoot struct {
	pwOK  string
	calls []string
}

func (a *apiRoot) RunAsRoot(_ context.Context, cmdline string, pw []byte) (string, error) {
	a.calls = append(a.calls, cmdline)
	if string(pw) != a.pwOK {
		return "", deploy.ErrBadPassword
	}
	return "Running as unit", nil
}

type depEnv struct {
	ts    *httptest.Server
	dir   string
	depd  string
	audit string
	srv   *server.Server
	rt    *apiRoot
	env   *opsEnv
}

func setupDeploy(t *testing.T, withManager bool) *depEnv {
	t.Helper()
	web := t.TempDir()
	dist := filepath.Join(web, "deploy", "dist")
	_ = os.MkdirAll(dist, 0o755)
	f, _ := os.Create(filepath.Join(dist, "tidb-deploy-0.3.1.tar.gz"))
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := "#!/bin/bash\n"
	_ = tw.WriteHeader(&tar.Header{Name: "tidb-deploy-0.3.1/tidb-deploy.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
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
	e := &depEnv{dir: dir, depd: filepath.Join(dir, "deploy"), audit: filepath.Join(dir, "audit.log"), rt: &apiRoot{pwOK: "Root-Pw-123"}, srv: s}
	cfg := server.OpsConfig{Dir: dir, Env: "staging", Users: users, Audit: al, Key: opsKey, ReadOnlyFile: filepath.Join(dir, "READONLY"), ExecMode: "none", TickEvery: time.Hour, NotifyWatchEvery: -1}
	if withManager {
		m, err := deploy.NewManager(deploy.Config{Dir: e.depd, WebDir: web, Runner: e.rt, NoPathCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Deploy = m
	}
	if err := s.EnableOps(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	e.ts = httptest.NewServer(s.Handler())
	t.Cleanup(e.ts.Close)
	e.env = &opsEnv{ts: e.ts, dir: dir, srv: s}
	return e
}

func goodBody(cluster string) map[string]any {
	return map[string]any{"cluster": cluster, "mode": "cluster", "version": "auto", "user": "tidb", "sshPort": "22",
		"pd": "172.16.0.201 172.16.0.202 172.16.0.203", "tidb": "172.16.0.201 172.16.0.202", "tikv": "172.16.0.203 172.16.0.204 172.16.0.205", "monitor": "172.16.0.204",
		"deployDir": "/tidb-deploy", "dataDir": "/tidb-data", "spec": "prod", "firewall": "open", "repoFix": "skip", "repoMirror": "aliyun", "withMonitor": true, "install": "online"}
}

func (e *depEnv) finish(t *testing.T, jobID string, rc string, secret string) {
	t.Helper()
	d := filepath.Join(e.depd, "jobs", jobID)
	if secret != "" {
		_ = os.WriteFile(filepath.Join(d, "tidb-root-password"), []byte(secret), 0o600)
	}
	if err := os.WriteFile(filepath.Join(d, "exit"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeployRoutesAbsentWithoutManager(t *testing.T) {
	e := setupDeploy(t, false)
	c := e.env.login(t, "admin")
	if code, _ := c.do("GET", "/api/deploy/status", nil); code != 404 {
		t.Errorf("没有管理器（非中控机模式）时不应注册部署接口: %d", code)
	}
}

func TestDeployPermissions(t *testing.T) {
	e := setupDeploy(t, true)
	if code, _ := e.env.client(t).do("GET", "/api/deploy/status", nil); code != 401 {
		t.Errorf("未登录: %d", code)
	}
	for _, u := range []string{"viewer", "dba", "sre"} {
		c := e.env.login(t, u)
		if code, _ := c.do("GET", "/api/deploy/status", nil); code != 403 {
			t.Errorf("%s 没有 settings.edit，应 403: %d", u, code)
		}
		if code, _ := c.do("POST", "/api/deploy/configs", goodBody("c1")); code != 403 {
			t.Errorf("%s 不能保存部署配置: %d", u, code)
		}
	}
	c := e.env.login(t, "admin")
	c.csrf = ""
	if code, _ := c.do("POST", "/api/deploy/configs", goodBody("c1")); code != 403 {
		t.Errorf("缺 CSRF 应 403: %d", code)
	}
	c = e.env.login(t, "admin")
	if code, out := c.do("GET", "/api/deploy/status", nil); code != 200 || out["available"] != true || out["canApprove"] != true {
		t.Errorf("admin 状态: %d %v", code, out)
	}
}

func TestDeployConfigSaveValidatesAndVersions(t *testing.T) {
	e := setupDeploy(t, true)
	c := e.env.login(t, "admin")
	bad := goodBody("c;1")
	code, out := c.do("POST", "/api/deploy/configs", bad)
	if code != 400 || out["code"] != "invalid" || len(out["errors"].([]any)) == 0 {
		t.Fatalf("非法配置应 400 并带错误列表: %d %v", code, out)
	}
	inj := goodBody("c1")
	inj["pd"] = "172.16.0.201 $(id)"
	if code, _ := c.do("POST", "/api/deploy/configs", inj); code != 400 {
		t.Errorf("命令替换应被拒绝: %d", code)
	}
	code, out = c.do("POST", "/api/deploy/configs", goodBody("prod-tidb-01"))
	if code != 200 || out["created"] != true || !strings.Contains(out["conf"].(string), "CLUSTER_NAME=prod-tidb-01") {
		t.Fatalf("保存: %d %v", code, out)
	}
	if v := out["version"].(map[string]any); v["ver"].(float64) != 1 || v["by"] != "admin" {
		t.Errorf("版本: %v", v)
	}
	if _, out = c.do("POST", "/api/deploy/configs", goodBody("prod-tidb-01")); out["created"] != false {
		t.Errorf("相同内容不应新建版本: %v", out)
	}
	b2 := goodBody("prod-tidb-01")
	b2["version"] = "v7.5"
	_, out = c.do("POST", "/api/deploy/configs", b2)
	if out["created"] != true || out["version"].(map[string]any)["ver"].(float64) != 2 {
		t.Errorf("改动应是 v2: %v", out)
	}
	if _, out = c.do("GET", "/api/deploy/configs?cluster=prod-tidb-01", nil); len(out["versions"].([]any)) != 2 {
		t.Errorf("列表: %v", out)
	}
	if _, out = c.do("GET", "/api/deploy/configs", nil); len(out["clusters"].([]any)) != 1 {
		t.Errorf("集群列表: %v", out)
	}
	if code, _ := c.do("GET", "/api/deploy/configs?cluster=..%2Fetc", nil); code != 404 {
		t.Errorf("目录穿越: %d", code)
	}
}

// 「忽略 tiup check 的失败项」：dev 规格可保存并写进 deploy.conf；prod 规格被拒绝，且不产生版本。
func TestDeployConfigAllowCheckFail(t *testing.T) {
	e := setupDeploy(t, true)
	c := e.env.login(t, "admin")
	prod := goodBody("acf-prod")
	prod["allowCheckFail"] = true
	code, out := c.do("POST", "/api/deploy/configs", prod)
	if code != 400 || !strings.Contains(strings.Join(anyStrings(out["errors"]), ";"), "dev 规格") {
		t.Fatalf("prod 勾选应 400: %d %v", code, out)
	}
	if _, out = c.do("GET", "/api/deploy/configs?cluster=acf-prod", nil); len(out["versions"].([]any)) != 0 {
		t.Errorf("被拒绝的配置不应产生版本: %v", out)
	}

	dev := goodBody("acf-dev")
	dev["spec"], dev["allowCheckFail"] = "dev", true
	code, out = c.do("POST", "/api/deploy/configs", dev)
	if code != 200 || !strings.Contains(out["conf"].(string), "\nALLOW_CHECK_FAIL=true\n") {
		t.Fatalf("dev 勾选应保存并输出 ALLOW_CHECK_FAIL=true: %d %v", code, out)
	}
	if v := out["version"].(map[string]any)["spec"].(map[string]any); v["allowCheckFail"] != true {
		t.Errorf("版本里应记录 allowCheckFail: %v", v)
	}
	dev["allowCheckFail"] = false
	if _, out = c.do("POST", "/api/deploy/configs", dev); out["created"] != true || strings.Contains(out["conf"].(string), "ALLOW_CHECK_FAIL") {
		t.Errorf("取消勾选应是新版本且不含该行: %v", out)
	}
}

func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func TestDeployEndToEndAndPasswordNeverPersisted(t *testing.T) {
	e := setupDeploy(t, true)
	a := e.env.login(t, "admin")
	a2 := e.env.login(t, "admin2")
	if code, _ := a.do("POST", "/api/deploy/configs", goodBody("prod-tidb-01")); code != 200 {
		t.Fatal("保存失败")
	}
	// 预检：密码错误 → 403，剩余次数，且没有任务
	code, out := a.do("POST", "/api/deploy/precheck", map[string]any{"cluster": "prod-tidb-01", "ver": 1, "rootPassword": "WrongPw-XYZ"})
	if code != 403 || out["code"] != "bad_password" || out["left"].(float64) != 2 {
		t.Fatalf("密码错误: %d %v", code, out)
	}
	if _, o := a.do("GET", "/api/deploy/jobs", nil); len(o["jobs"].([]any)) != 0 {
		t.Errorf("密码错误不应有任务: %v", o)
	}
	// 正确密码
	code, out = a.do("POST", "/api/deploy/precheck", map[string]any{"cluster": "prod-tidb-01", "ver": 1, "rootPassword": "Root-Pw-123"})
	if code != 200 {
		t.Fatalf("预检: %d %v", code, out)
	}
	pj := out["job"].(map[string]any)
	pid := pj["id"].(string)
	if pj["status"] != "running" || pj["kind"] != "precheck" {
		t.Errorf("预检任务: %v", pj)
	}
	// 轮询输出：平台写的启动行 + 模拟脚本输出，增量读取
	code, out = a.do("GET", "/api/deploy/jobs/"+pid, nil)
	if code != 200 || out["offset"] == nil {
		t.Fatalf("轮询: %d %v", code, out)
	}
	off := int64(out["offset"].(float64))
	lf, _ := os.OpenFile(filepath.Join(e.depd, "jobs", pid, "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = lf.WriteString("  ✔ 172.16.0.201 部署用户 tidb：sudo 免密、SSH 免密登录正常\n")
	_ = lf.Close()
	_, out = a.do("GET", "/api/deploy/jobs/"+pid+"?offset="+itoa64(off), nil)
	if !strings.Contains(out["log"].(string), "部署用户 tidb") {
		t.Errorf("增量输出: %v", out)
	}
	// 同一时间只有一个任务
	if code, o := a.do("POST", "/api/deploy/precheck", map[string]any{"cluster": "prod-tidb-01", "ver": 1, "rootPassword": "Root-Pw-123"}); code != 409 || o["code"] != "busy" {
		t.Errorf("busy: %d %v", code, o)
	}
	// 预检未结束：申请被拒
	if code, o := a.do("POST", "/api/deploy/requests", map[string]any{"precheck": pid}); code != 409 || o["code"] != "not_ready" {
		t.Errorf("预检未通过不能申请: %d %v", code, o)
	}
	e.finish(t, pid, "0", "")
	code, out = a.do("POST", "/api/deploy/requests", map[string]any{"precheck": pid})
	if code != 200 {
		t.Fatalf("申请: %d %v", code, out)
	}
	rid := out["request"].(map[string]any)["id"].(string)
	// 自己批准 → 403 self_approve；未批准就开始 → 409
	if code, o := a.do("POST", "/api/deploy/requests/"+rid+"/approve", nil); code != 403 || o["code"] != "self_approve" {
		t.Errorf("自批: %d %v", code, o)
	}
	if code, o := a.do("POST", "/api/deploy/requests/"+rid+"/start", map[string]any{"confirm": "prod-tidb-01", "rootPassword": "Root-Pw-123"}); code != 409 || o["code"] != "not_ready" {
		t.Errorf("未批准: %d %v", code, o)
	}
	// 没有 approve.high 的 sre 不能批准；admin2 可以
	if code, _ := e.env.login(t, "sre").do("POST", "/api/deploy/requests/"+rid+"/approve", nil); code != 403 {
		t.Errorf("sre 不能批准: %d", code)
	}
	if code, o := a2.do("POST", "/api/deploy/requests/"+rid+"/approve", nil); code != 200 || o["request"].(map[string]any)["state"] != "approved" {
		t.Fatalf("admin2 批准: %d %v", code, o)
	}
	// 集群名不一致 / 缺密码 / 密码错
	if code, _ := a.do("POST", "/api/deploy/requests/"+rid+"/start", map[string]any{"confirm": "other", "rootPassword": "Root-Pw-123"}); code != 400 {
		t.Errorf("集群名不一致: %d", code)
	}
	if code, _ := a.do("POST", "/api/deploy/requests/"+rid+"/start", map[string]any{"confirm": "prod-tidb-01"}); code != 400 {
		t.Errorf("缺密码: %d", code)
	}
	code, out = a.do("POST", "/api/deploy/requests/"+rid+"/start", map[string]any{"confirm": "prod-tidb-01", "rootPassword": "WrongPw-XYZ"})
	if code != 403 || out["code"] != "bad_password" || out["left"].(float64) != 2 {
		t.Errorf("start 密码错: %d %v", code, out)
	}
	code, out = a.do("POST", "/api/deploy/requests/"+rid+"/start", map[string]any{"confirm": "prod-tidb-01", "rootPassword": "Root-Pw-123"})
	if code != 200 {
		t.Fatalf("start: %d %v", code, out)
	}
	did := out["job"].(map[string]any)["id"].(string)
	// 部署成功 + TiDB root 密码只能取一次
	e.finish(t, did, "0", "Xk9mPq2vL7eR\n")
	_, out = a.do("GET", "/api/deploy/jobs/"+did, nil)
	if j := out["job"].(map[string]any); j["status"] != "ok" || j["secret"] != true {
		t.Errorf("部署成功: %v", j)
	}
	code, out = a.do("POST", "/api/deploy/jobs/"+did+"/secret", nil)
	if code != 200 || out["password"] != "Xk9mPq2vL7eR" {
		t.Fatalf("取密码: %d %v", code, out)
	}
	if code, o := a.do("POST", "/api/deploy/jobs/"+did+"/secret", nil); code != 410 || o["code"] != "gone" {
		t.Errorf("只能取一次: %d %v", code, o)
	}
	// 下载完整输出
	code, out = a.do("GET", "/api/deploy/jobs/"+did+"/download", nil)
	if code != 200 || !strings.Contains(out["_raw"].(string), "[平台]") {
		t.Errorf("下载: %d %v", code, out)
	}
	// 申请变成 done
	_, out = a.do("GET", "/api/deploy/requests", nil)
	if r := out["requests"].([]any)[0].(map[string]any); r["state"] != "done" {
		t.Errorf("申请状态: %v", r)
	}

	// 审计：有动作记录，但任何位置都不能出现 root 密码或 TiDB root 密码
	_ = e.srv
	raw, _ := os.ReadFile(e.audit)
	for _, want := range []string{"deploy.config.save", "deploy.precheck", "deploy.request", "deploy.approve", "deploy.start", "deploy.secret.reveal"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("审计缺少 %s", want)
		}
	}
	_ = filepath.Walk(e.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, secret := range []string{"Root-Pw-123", "WrongPw-XYZ", "Xk9mPq2vL7eR"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("口令 %q 泄漏到 %s", secret, p)
			}
		}
		return nil
	})
}

func itoa64(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
