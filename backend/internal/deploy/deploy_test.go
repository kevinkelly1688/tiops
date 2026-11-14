// @author: kevinkelly
// @license: Apache License 2.0
// @file: deploy_test.go
// @time: 2026/10/28

package deploy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func goodSpec() Spec {
	return Spec{Cluster: "prod-tidb-01", Mode: "cluster", Version: "auto", User: "tidb", SSHPort: "22",
		PD: "172.16.0.201 172.16.0.202 172.16.0.203", TiDB: "172.16.0.201,172.16.0.202", TiKV: "172.16.0.203 172.16.0.204 172.16.0.205", Monitor: "172.16.0.204",
		DeployDir: "/tidb-deploy", DataDir: "/tidb-data", Profile: "prod", Firewall: "open", RepoFix: "skip", RepoMirror: "aliyun", WithMonitor: true, Install: "online"}
}

func TestSpecRenderMatchesFrontendFormat(t *testing.T) {
	s := goodSpec().Normalize()
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatalf("合法配置不应有错误: %v", errs)
	}
	conf := s.Render()
	for _, want := range []string{"CLUSTER_NAME=prod-tidb-01\n", "TIDB_VERSION=auto\n", "DEPLOY_MODE=cluster\n",
		`PD_HOSTS="172.16.0.201 172.16.0.202 172.16.0.203"`, `TIDB_HOSTS="172.16.0.201 172.16.0.202"`, `TIFLASH_HOSTS=""`, `MONITOR_HOST="172.16.0.204"`,
		"WITH_MONITOR=true\n", "DEPLOY_USER=tidb\n", "SSH_PORT=22\n", `SSH_KEY=""`, "SPEC=prod\n", "FIREWALL=open\n", "INSTALL_MODE=online\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("deploy.conf 缺少 %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "OFFLINE_PKG_DIR") || strings.Contains(conf, "ZONE_MAP") {
		t.Errorf("在线安装 / 无 zone 时不应出现离线与 ZONE_MAP 项:\n%s", conf)
	}
	off := goodSpec()
	off.Install, off.OfflineDir, off.Zone = "offline", "/opt/tidb-pkg/", "172.16.0.203=az1,172.16.0.204=az2,172.16.0.205=az3"
	off = off.Normalize()
	if errs := off.Validate(); len(errs) != 0 {
		t.Fatalf("离线配置: %v", errs)
	}
	c2 := off.Render()
	if !strings.Contains(c2, `OFFLINE_PKG_DIR="/opt/tidb-pkg"`) || !strings.Contains(c2, `ZONE_MAP="172.16.0.203=az1,172.16.0.204=az2,172.16.0.205=az3"`) {
		t.Errorf("离线 / zone 渲染不对:\n%s", c2)
	}
}

// ALLOW_CHECK_FAIL：不勾选时渲染结果不含该行（旧版本保存的配置哈希不变）；勾选且 dev 规格才输出；prod 规格被拒绝。
func TestSpecAllowCheckFail(t *testing.T) {
	prod := goodSpec().Normalize()
	if strings.Contains(prod.Render(), "ALLOW_CHECK_FAIL") {
		t.Error("默认（不勾选）不应输出 ALLOW_CHECK_FAIL")
	}
	// 旧版本保存的配置 JSON 没有这个字段：按 false 处理，渲染不变
	var old Spec
	if err := json.Unmarshal([]byte(`{"cluster":"c1","mode":"single","spec":"dev"}`), &old); err != nil || old.AllowCheckFail {
		t.Fatalf("旧配置缺字段应为 false: %+v %v", old, err)
	}

	dev := goodSpec()
	dev.Profile, dev.AllowCheckFail = "dev", true
	dev = dev.Normalize()
	if errs := dev.Validate(); len(errs) != 0 {
		t.Fatalf("dev 规格应允许: %v", errs)
	}
	if conf := dev.Render(); !strings.Contains(conf, "\nALLOW_CHECK_FAIL=true\n") {
		t.Errorf("勾选后应输出 ALLOW_CHECK_FAIL=true:\n%s", conf)
	}
	devOff := dev
	devOff.AllowCheckFail = false
	if devOff.Render() == dev.Render() {
		t.Error("勾选与不勾选的渲染结果应不同（版本哈希随之变化）")
	}

	bad := goodSpec() // 生产规格
	bad.AllowCheckFail = true
	bad = bad.Normalize()
	if errs := bad.Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), "dev 规格") {
		t.Errorf("prod 规格勾选必须被拒绝: %v", errs)
	}
	if _, _, err := NewStore(t.TempDir()).Save(bad, "admin", time.Now()); err == nil {
		t.Error("保存 prod + 忽略检查失败 应被校验拒绝")
	}
}

// deploy.conf 会被 root source：任何能改变 shell 语义的字符都必须在校验时被拒绝。
func TestSpecRejectsShellInjection(t *testing.T) {
	bad := map[string]func(*Spec){
		"集群名带分号":   func(s *Spec) { s.Cluster = "a;rm -rf /" },
		"集群名带路径":   func(s *Spec) { s.Cluster = "../x" },
		"用户带引号":    func(s *Spec) { s.User = `tidb"` },
		"用户 root":  func(s *Spec) { s.User = "root" },
		"主机带命令替换":  func(s *Spec) { s.PD = "172.16.0.201 $(id)" },
		"主机带引号":    func(s *Spec) { s.TiKV = `172.16.0.203" ; id ; "` },
		"私钥路径带空格":  func(s *Spec) { s.SSHKey = "/home/tidb/.ssh/id rsa" },
		"私钥路径带反引号": func(s *Spec) { s.SSHKey = "/home/`id`/k" },
		"目录相对路径":   func(s *Spec) { s.DataDir = "tidb-data" },
		"目录含 ..":   func(s *Spec) { s.DeployDir = "/a/../etc" },
		"目录是根":     func(s *Spec) { s.DeployDir = "/" },
		"版本带分号":    func(s *Spec) { s.Version = "v8.5;id" },
		"规格非法":     func(s *Spec) { s.Profile = "x" },
		"端口非法":     func(s *Spec) { s.SSHPort = "22;id" },
		"换行注入":     func(s *Spec) { s.Cluster = "a\nDEPLOY_USER=root" },
	}
	for name, mut := range bad {
		s := goodSpec()
		mut(&s)
		if errs := s.Normalize().Validate(); len(errs) == 0 {
			t.Errorf("%s：应被拒绝", name)
		}
	}
	// 离线目录、zone 也要校验
	s := goodSpec()
	s.Install, s.OfflineDir = "offline", "/opt/pkg;id"
	if len(s.Normalize().Validate()) == 0 {
		t.Error("离线目录带分号应被拒绝")
	}
	s = goodSpec()
	s.Zone = "172.16.0.203=az1;id"
	if len(s.Normalize().Validate()) == 0 {
		t.Error("zone 带分号应被拒绝")
	}
}

func TestSpecBusinessRules(t *testing.T) {
	s := goodSpec()
	s.PD = "172.16.0.201"
	if errs := s.Normalize().Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), "生产规格") {
		t.Errorf("生产规格 PD<3 应报错: %v", errs)
	}
	s.Profile = "dev"
	if errs := s.Normalize().Validate(); len(errs) != 0 {
		t.Errorf("dev 规格允许低配: %v", errs)
	}
	s = goodSpec()
	s.TiKV = "172.16.0.203 172.16.0.203 172.16.0.205"
	if errs := s.Normalize().Validate(); len(errs) == 0 {
		t.Error("重复主机应报错")
	}
	s = goodSpec()
	s.Zone = "172.16.0.203=az1"
	if errs := s.Normalize().Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), "缺少 TiKV 主机") {
		t.Errorf("zone 缺 TiKV 主机应报错: %v", errs)
	}
	single := Spec{Cluster: "t1", Mode: "single", User: "tidb", SingleHost: "10.0.0.5"}.Normalize()
	if errs := single.Validate(); len(errs) != 0 {
		t.Errorf("单机配置: %v", errs)
	}
	if !strings.Contains(single.Render(), `SINGLE_HOST="10.0.0.5"`) {
		t.Errorf("单机渲染: %s", single.Render())
	}
	if got := goodSpec().Normalize().AllHosts(); len(got) != 5 {
		t.Errorf("AllHosts 应去重得到 5 台: %v", got)
	}
}

func TestStoreVersions(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Now()
	v1, created, err := st.Save(goodSpec(), "admin", now)
	if err != nil || !created || v1.Ver != 1 || len(v1.SHA) != 64 {
		t.Fatalf("首次保存: %+v %v %v", v1, created, err)
	}
	b, err := os.ReadFile(st.ConfPath("prod-tidb-01", 1))
	if err != nil || !strings.Contains(string(b), "CLUSTER_NAME=prod-tidb-01") {
		t.Fatalf("conf 文件: %v %q", err, b)
	}
	if runtimeUnix() {
		if fi, _ := os.Stat(st.ConfPath("prod-tidb-01", 1)); fi.Mode().Perm() != 0o600 {
			t.Errorf("conf 权限应为 600: %v", fi.Mode())
		}
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != v1.SHA {
		t.Error("SHA 应是 conf 内容的 sha256")
	}
	same, created, err := st.Save(goodSpec(), "admin2", now)
	if err != nil || created || same.Ver != 1 {
		t.Errorf("内容相同不应新建版本: %+v %v %v", same, created, err)
	}
	s2 := goodSpec()
	s2.Version = "v7.5"
	v2, created, err := st.Save(s2, "admin", now.Add(time.Minute))
	if err != nil || !created || v2.Ver != 2 {
		t.Fatalf("改了内容应是 v2: %+v %v %v", v2, created, err)
	}
	if vs, _ := st.List("prod-tidb-01"); len(vs) != 2 {
		t.Errorf("List: %+v", vs)
	}
	if g, err := st.Get("prod-tidb-01", 0); err != nil || g.Ver != 2 {
		t.Errorf("Get 最新: %+v %v", g, err)
	}
	if g, err := st.Get("prod-tidb-01", 1); err != nil || g.Spec.Version != "auto" {
		t.Errorf("Get v1: %+v %v", g, err)
	}
	if _, err := st.Get("prod-tidb-01", 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("越界版本: %v", err)
	}
	if _, err := st.Get("../etc", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("目录穿越应当拒绝: %v", err)
	}
	bad := goodSpec()
	bad.Cluster = "x;y"
	var ve *ValidationError
	if _, _, err := st.Save(bad, "admin", now); !errors.As(err, &ve) {
		t.Errorf("非法配置应返回 ValidationError: %v", err)
	}
	if cs := st.Clusters(); len(cs) != 1 || cs[0] != "prod-tidb-01" {
		t.Errorf("Clusters: %v", cs)
	}
}

func runtimeUnix() bool { return filepath.Separator == '/' }

// ---------------------------------------------------------------- 管理器 ----

type fakeRoot struct {
	calls     []string
	passwords []string
	err       error
	onRun     func(cmdline string)
}

func (f *fakeRoot) RunAsRoot(_ context.Context, cmdline string, pw []byte) (string, error) {
	f.calls = append(f.calls, cmdline)
	f.passwords = append(f.passwords, string(pw))
	if f.err != nil {
		return "", f.err
	}
	if f.onRun != nil {
		f.onRun(cmdline)
	}
	return "Running as unit: x.service", nil
}

func makeToolPkg(t *testing.T, web, ver string) {
	t.Helper()
	dir := filepath.Join(web, "deploy", "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "tidb-deploy-"+ver+".tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	top := "tidb-deploy-" + ver
	_ = tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := "#!/bin/bash\necho hi\n"
	_ = tw.WriteHeader(&tar.Header{Name: top + "/tidb-deploy.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	b, _ := os.ReadFile(p)
	sum := sha256.Sum256(b)
	_ = os.WriteFile(p+".sha256", []byte(hex.EncodeToString(sum[:])+"  "+filepath.Base(p)+"\n"), 0o644)
}

type env struct {
	m   *Manager
	rt  *fakeRoot
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	web := t.TempDir()
	makeToolPkg(t, web, "0.3.0")
	makeToolPkg(t, web, "0.3.1")
	e := &env{rt: &fakeRoot{}, now: time.Date(2026, 10, 9, 21, 0, 0, 0, time.UTC)}
	m, err := NewManager(Config{Dir: filepath.Join(t.TempDir(), "deploy"), WebDir: web, Runner: e.rt, WorkDir: "/var/lib/tidb-deploy", NoPathCheck: true, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return e
}

func (e *env) finish(t *testing.T, id string, rc int, secret string) {
	t.Helper()
	dir := e.m.jobDir(id)
	if secret != "" {
		_ = os.WriteFile(filepath.Join(dir, tidbRootSecret), []byte(secret+"\n"), 0o600)
	}
	_ = os.WriteFile(filepath.Join(dir, "exit"), []byte(itoa(rc)), 0o600)
}

func itoa(n int) string { return strconv.Itoa(n) }

func pw(s string) []byte { return []byte(s) }

func TestToolPackagePicksHighestAndVerifies(t *testing.T) {
	web := t.TempDir()
	makeToolPkg(t, web, "0.3.1")
	makeToolPkg(t, web, "0.10.0")
	makeToolPkg(t, web, "0.9.9")
	p, ver, err := FindToolPackage(web)
	if err != nil || ver != "0.10.0" || !strings.HasSuffix(p, "tidb-deploy-0.10.0.tar.gz") {
		t.Fatalf("应取最高版本 0.10.0（按数字比较）: %q %q %v", p, ver, err)
	}
	dest := filepath.Join(t.TempDir(), "tool")
	script, err := ExtractTool(p, dest)
	if err != nil || filepath.Base(script) != "tidb-deploy.sh" {
		t.Fatalf("解包: %q %v", script, err)
	}
	// 篡改包 → .sha256 校验失败
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	_ = os.WriteFile(p, b, 0o644)
	if _, err := ExtractTool(p, filepath.Join(t.TempDir(), "x")); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Errorf("被篡改的包应校验失败: %v", err)
	}
	if _, _, err := FindToolPackage(t.TempDir()); err == nil {
		t.Error("没有工具包目录应报错")
	}
}

func TestToolExtractRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tidb-deploy-9.9.9.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "x/../../evil.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	if _, err := ExtractTool(p, filepath.Join(dir, "out")); err == nil {
		t.Error("含 .. 的路径应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.sh")); err == nil {
		t.Error("不应写出目标目录")
	}
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _, err := e.m.Store.Save(goodSpec(), "admin", e.now)
	if err != nil {
		t.Fatal(err)
	}
	// 预检
	pj, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("s3cret"))
	if err != nil || pj.Kind != KindPrecheck || pj.Status != StatusRunning {
		t.Fatalf("预检: %+v %v", pj, err)
	}
	if len(e.rt.calls) != 1 || !strings.Contains(e.rt.calls[0], "systemd-run --unit=tidbops-deploy-"+pj.ID) || !strings.Contains(e.rt.calls[0], "/run.sh") {
		t.Errorf("应通过 systemd-run 启动: %v", e.rt.calls)
	}
	if e.rt.passwords[0] != "s3cret" {
		t.Errorf("root 密码应原样交给执行器: %q", e.rt.passwords)
	}
	dir := e.m.jobDir(pj.ID)
	for _, f := range []string{"deploy.conf", "run.sh", "out.log", "exit", "tool/tidb-deploy.sh", "job.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("任务目录缺 %s: %v", f, err)
		}
	}
	rs, _ := os.ReadFile(filepath.Join(dir, "run.sh"))
	if !strings.Contains(string(rs), `./tidb-deploy.sh check --config "$JOB/deploy.conf"`) || strings.Contains(string(rs), "--yes") {
		t.Errorf("预检脚本: %s", rs)
	}
	// 口令不能出现在任何落盘文件里
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "s3cret") {
				t.Errorf("口令泄漏到文件 %s", p)
			}
		}
		return nil
	})
	// 预检还在运行时：不能再起别的任务
	if _, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("s3cret")); !errors.Is(err, ErrBusy) {
		t.Errorf("同时只能有一个任务: %v", err)
	}
	// 预检没通过 → 不能申请
	e.finish(t, pj.ID, 1, "")
	if _, err := e.m.RequestDeploy("admin", pj.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("预检失败不能提交申请: %v", err)
	}
	// 重新预检，通过
	pj2, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, pj2.ID, 0, "")
	if j, _ := e.m.Job(pj2.ID); j.Status != StatusOK || j.Exit == nil || *j.Exit != 0 {
		t.Fatalf("预检应成功: %+v", j)
	}
	req, err := e.m.RequestDeploy("admin", pj2.ID)
	if err != nil || req.State != "pending" {
		t.Fatalf("申请: %+v %v", req, err)
	}
	// 未批准不能开始；申请人不能自己批准
	if _, err := e.m.StartDeploy(ctx, "admin", req.ID, "prod-tidb-01", pw("s3cret")); !errors.Is(err, ErrNotReady) {
		t.Errorf("未批准不能开始: %v", err)
	}
	if _, err := e.m.Approve("admin", req.ID); !errors.Is(err, ErrSelfApprove) {
		t.Errorf("不能自己批准: %v", err)
	}
	if r, err := e.m.Approve("admin2", req.ID); err != nil || r.State != "approved" || r.ApprovedBy != "admin2" {
		t.Fatalf("批准: %+v %v", r, err)
	}
	if _, err := e.m.Approve("admin3", req.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("重复批准应拒绝: %v", err)
	}
	// 集群名不一致 / 没密码
	if _, err := e.m.StartDeploy(ctx, "admin", req.ID, "other", pw("s3cret")); !errors.Is(err, ErrNameMismatch) {
		t.Errorf("集群名不一致: %v", err)
	}
	if _, err := e.m.StartDeploy(ctx, "admin", req.ID, "prod-tidb-01", nil); !errors.Is(err, ErrNoPassword) {
		t.Errorf("没密码: %v", err)
	}
	calls := len(e.rt.calls)
	dj, err := e.m.StartDeploy(ctx, "admin", req.ID, "prod-tidb-01", pw("s3cret"))
	if err != nil || dj.Kind != KindDeploy || dj.Request != req.ID {
		t.Fatalf("部署: %+v %v", dj, err)
	}
	if len(e.rt.calls) != calls+1 {
		t.Errorf("部署应只调用一次执行器")
	}
	rs, _ = os.ReadFile(filepath.Join(e.m.jobDir(dj.ID), "run.sh"))
	if !strings.Contains(string(rs), `./tidb-deploy.sh deploy --config "$JOB/deploy.conf" --yes`) || !strings.Contains(string(rs), "/var/lib/tidb-deploy/prod-tidb-01/root-password.txt") {
		t.Errorf("部署脚本: %s", rs)
	}
	// 部署失败 → 申请仍有效，可以从断点继续
	e.finish(t, dj.ID, 1, "")
	if j, _ := e.m.Job(dj.ID); j.Status != StatusFailed {
		t.Fatalf("应失败: %+v", j)
	}
	if r, _ := e.m.GetRequest(req.ID); r.State != "approved" || r.LastJob != dj.ID {
		t.Errorf("失败后申请仍是 approved: %+v", r)
	}
	dj2, err := e.m.StartDeploy(ctx, "admin", req.ID, "prod-tidb-01", pw("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, dj2.ID, 0, "Xk9mPq2vL7eR")
	j, _ := e.m.Job(dj2.ID)
	if j.Status != StatusOK || !j.Secret {
		t.Fatalf("成功且有密码可取: %+v", j)
	}
	sec, err := e.m.RevealSecret(dj2.ID)
	if err != nil || sec != "Xk9mPq2vL7eR" {
		t.Fatalf("取密码: %q %v", sec, err)
	}
	if _, err := e.m.RevealSecret(dj2.ID); !errors.Is(err, ErrSecretGone) {
		t.Errorf("只能取一次: %v", err)
	}
	if j, _ := e.m.Job(dj2.ID); j.Secret {
		t.Error("取过之后不应再标记有密码")
	}
	if r, _ := e.m.GetRequest(req.ID); r.State != "done" || !r.Done {
		t.Errorf("部署成功后申请应是 done: %+v", r)
	}
	if _, err := e.m.StartDeploy(ctx, "admin", req.ID, "prod-tidb-01", pw("s3cret")); !errors.Is(err, ErrNotReady) {
		t.Errorf("已完成的申请不能再部署: %v", err)
	}
}

func TestBadPasswordLocksAndLeavesNoJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _, _ := e.m.Store.Save(goodSpec(), "admin", e.now)
	e.rt.err = ErrBadPassword
	for i, left := range []int{2, 1, 0} {
		_, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("wrong"))
		var be *BadPasswordError
		if !errors.As(err, &be) || be.Left != left || !errors.Is(err, ErrBadPassword) {
			t.Fatalf("第 %d 次应报密码错误且剩 %d 次: %v", i+1, left, err)
		}
	}
	if jobs := e.m.Jobs("", 10); len(jobs) != 0 {
		t.Errorf("密码错误不应留下任务记录: %+v", jobs)
	}
	calls := len(e.rt.calls)
	e.rt.err = nil
	if _, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("right")); !errors.Is(err, ErrLocked) {
		t.Errorf("连续输错 3 次应锁定: %v", err)
	}
	if len(e.rt.calls) != calls {
		t.Error("锁定期间不应再调用 su")
	}
	// 另一个账号不受影响；锁定 10 分钟后解除
	if _, err := e.m.StartPrecheck(ctx, "admin2", "prod-tidb-01", v.Ver, pw("right")); err != nil {
		t.Errorf("其他账号不受影响: %v", err)
	}
	e.now = e.now.Add(11 * time.Minute)
	for _, j := range e.m.Jobs("", 5) {
		e.finish(t, j.ID, 0, "")
	}
	if _, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("right")); err != nil {
		t.Errorf("10 分钟后应解除锁定: %v", err)
	}
}

func TestLaunchFailureKeepsJobWithReason(t *testing.T) {
	e := newEnv(t)
	v, _, _ := e.m.Store.Save(goodSpec(), "admin", e.now)
	e.rt.err = &SuError{Output: "su: Permission denied", Err: errors.New("exit status 1")}
	j, err := e.m.StartPrecheck(context.Background(), "admin", "prod-tidb-01", v.Ver, pw("x"))
	if err == nil || j.Status != StatusFailed || !strings.Contains(j.LaunchErr, "Permission denied") {
		t.Fatalf("启动失败应保留任务并说明原因: %+v %v", j, err)
	}
	if log, _, _ := e.m.Log(j.ID, 0); !strings.Contains(log, "启动失败") {
		t.Errorf("日志里应写明启动失败: %q", log)
	}
	if _, busy := e.m.Running(); busy {
		t.Error("启动失败的任务不应占着「运行中」")
	}
	// 密码错误以外的失败不计入锁定
	if err := e.m.noteFailCount("admin"); err != 0 {
		t.Errorf("非密码错误不应计数: %d", err)
	}
}

func (m *Manager) noteFailCount(user string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.fails[user])
}

func TestCancelNeedsPasswordAndStopsUnit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _, _ := e.m.Store.Save(goodSpec(), "admin", e.now)
	j, err := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Cancel(ctx, "admin", j.ID, nil); !errors.Is(err, ErrNoPassword) {
		t.Errorf("取消要密码: %v", err)
	}
	e.rt.err = ErrBadPassword
	if _, err := e.m.Cancel(ctx, "admin", j.ID, pw("bad")); !errors.Is(err, ErrBadPassword) {
		t.Errorf("密码错误: %v", err)
	}
	e.rt.err = nil
	cj, err := e.m.Cancel(ctx, "admin", j.ID, pw("pw"))
	if err != nil || !cj.Cancelled || cj.Status != StatusFailed || cj.Exit == nil || *cj.Exit != 143 {
		t.Fatalf("取消: %+v %v", cj, err)
	}
	if last := e.rt.calls[len(e.rt.calls)-1]; last != "systemctl stop "+j.Unit {
		t.Errorf("应 systemctl stop 对应单元: %q", last)
	}
	if log, _, _ := e.m.Log(j.ID, 0); !strings.Contains(log, "已由 admin 取消") {
		t.Errorf("日志: %q", log)
	}
	if _, err := e.m.Cancel(ctx, "admin", j.ID, pw("pw")); !errors.Is(err, ErrNotReady) {
		t.Errorf("已结束的任务不能再取消: %v", err)
	}
}

func TestLogOffsetsAndStale(t *testing.T) {
	e := newEnv(t)
	v, _, _ := e.m.Store.Save(goodSpec(), "admin", e.now)
	j, _ := e.m.StartPrecheck(context.Background(), "admin", "prod-tidb-01", v.Ver, pw("pw"))
	f, _ := os.OpenFile(filepath.Join(e.m.jobDir(j.ID), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("第一行\n第二行\n")
	_ = f.Close()
	a, off, err := e.m.Log(j.ID, 0)
	if err != nil || !strings.Contains(a, "第一行\n第二行\n") || off == 0 {
		t.Fatalf("读日志: %q %d %v", a, off, err)
	}
	f, _ = os.OpenFile(filepath.Join(e.m.jobDir(j.ID), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("第三行\n")
	_ = f.Close()
	b, off2, _ := e.m.Log(j.ID, off)
	if b != "第三行\n" || off2 <= off {
		t.Errorf("增量读取: %q %d", b, off2)
	}
	if c, o3, _ := e.m.Log(j.ID, off2+1000); !strings.Contains(c, "第一行") || o3 != off2 {
		t.Errorf("偏移越界应从头读: %q", c)
	}
	if _, _, err := e.m.Log("../../etc", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("任务 ID 不合法: %v", err)
	}
	e.now = e.now.Add(7 * time.Hour)
	if got, _ := e.m.Job(j.ID); got.Status != StatusUnknown {
		t.Errorf("长时间没有退出码应是 unknown: %+v", got)
	}
}

func TestRequestExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	v, _, _ := e.m.Store.Save(goodSpec(), "admin", e.now)
	pj, _ := e.m.StartPrecheck(ctx, "admin", "prod-tidb-01", v.Ver, pw("pw"))
	e.finish(t, pj.ID, 0, "")
	req, _ := e.m.RequestDeploy("admin", pj.ID)
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.m.Approve("admin2", req.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("超过 1 小时没人批准应过期: %v", err)
	}
	if r, _ := e.m.GetRequest(req.ID); r.State != "expired" {
		t.Errorf("状态: %+v", r)
	}
	// 预检超过 24 小时不能提交申请
	e.now = e.now.Add(25 * time.Hour)
	if _, err := e.m.RequestDeploy("admin", pj.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("预检过期: %v", err)
	}
}

func TestSuClassification(t *testing.T) {
	for _, s := range []string{"Password: ", "Password:", "密码：", "Password for root: "} {
		if !IsPasswordPrompt(s) {
			t.Errorf("应识别为口令提示: %q", s)
		}
	}
	for _, s := range []string{"", "Running as unit: x.service", "su: Authentication failure\r\n"} {
		if IsPasswordPrompt(s) {
			t.Errorf("不应是口令提示: %q", s)
		}
	}
	if err := ClassifySu("", nil); err != nil {
		t.Error("退出码 0 → nil")
	}
	if err := ClassifySu("su: Authentication failure\r\n", errors.New("exit 1")); !errors.Is(err, ErrBadPassword) {
		t.Errorf("认证失败 → ErrBadPassword: %v", err)
	}
	err := ClassifySu("su: user root does not exist", errors.New("exit 1"))
	var se *SuError
	if errors.Is(err, ErrBadPassword) || !errors.As(err, &se) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("其它失败应保留输出: %v", err)
	}
	b := []byte("abc")
	zero(b)
	if string(b) != "\x00\x00\x00" {
		t.Error("zero 应清零")
	}
}

func TestSuRunnerUnsupportedOrZeroes(t *testing.T) {
	if runtimeUnix() {
		t.Skip("Linux 上的真实 su 只在目标机上验证")
	}
	b := []byte("pw")
	if _, err := (SuRunner{}).RunAsRoot(context.Background(), "id", b); !errors.Is(err, ErrUnsupported) || b[0] != 0 {
		t.Errorf("非 Linux 应返回 ErrUnsupported 并清零口令: %v %v", err, b)
	}
}
