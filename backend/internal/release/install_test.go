// @author: kevinkelly
// @license: Apache License 2.0
// @file: install_test.go
// @time: 2026/10/28

package release

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tidbops/internal/dataver"
)

func TestInstallFullFlow(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "prod-tidb-01", Env: "staging", TidbUser: "dbuser", Listen: "127.0.0.1:9090", PublicURL: "https://ops.example.com", Start: true})
	if err == nil {
		t.Fatal("tidb 用户 dbuser 不存在时应拒绝")
	}
	v.sys.users["dbuser"] = true
	v.out.Reset()
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "demo-cluster", Env: "staging", TidbUser: "dbuser", Listen: "127.0.0.1:9090", PublicURL: "https://ops.example.com", Start: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "输出", out, "安装完成", "下一步", "user add", "READONLY")

	// 目录与符号链接
	if got := v.cur(); got != "1.0.0" {
		t.Fatalf("current = %q", got)
	}
	for _, p := range []string{"bin/tidbops-server", "web/index.html", "deploy/ops/tidbops.service", ManifestName} {
		if !exists(filepath.Join(v.l.ReleaseDir("1.0.0"), filepath.FromSlash(p))) {
			t.Errorf("release 缺少 %s", p)
		}
	}
	// 复制后的 release 与清单一致
	m, _ := ReadManifest(v.l.ReleaseDir("1.0.0"))
	if err := VerifyDir(v.l.ReleaseDir("1.0.0"), m); err != nil {
		t.Errorf("release 校验: %v", err)
	}

	// unit：路径替换与参数
	unit, _ := os.ReadFile(v.l.Unit())
	u := string(unit)
	mustContain(t, "unit", u, "ExecStart=/opt/tidbops/current/bin/tidbops-server", "--web /opt/tidbops/current/web",
		"--exec-bin /opt/tidbops/current/bin/tidbops-server", "--env staging", "--cluster demo-cluster", "--exec-sudo-user dbuser",
		"--listen 127.0.0.1:9090", "--home /home/dbuser", "--public-url https://ops.example.com", "--data-dir /var/lib/tidbops")
	mustNotContain(t, "unit", u, "/opt/tidbops/bin/", "/opt/tidbops/web", "--env prod", "--exec-sudo-user tidb ")
	if p, err := ParseUnit(u); err != nil || p.Cluster != "demo-cluster" || p.Env != "staging" || p.TidbUser != "dbuser" || p.Listen != "127.0.0.1:9090" || p.PublicURL != "https://ops.example.com" {
		t.Errorf("ParseUnit 往返失败: %+v %v", p, err)
	}
	// sudoers
	sud, _ := os.ReadFile(v.l.Sudoers())
	mustContain(t, "sudoers", string(sud), "tidbops ALL=(dbuser) NOPASSWD: /opt/tidbops/current/bin/tidbops-server exec --stdin")
	mustNotContain(t, "sudoers", string(sud), "/opt/tidbops/bin/")
	if exists(filepath.Join(v.l.SudoersDir(), ".tidbops.sudoers.new")) {
		t.Error("临时 sudoers 文件未清理")
	}
	// 密钥、策略、env
	key, _ := os.ReadFile(v.l.ExecKey())
	if len(strings.TrimSpace(string(key))) != 64 {
		t.Errorf("exec.key 应为 32 字节十六进制: %q", key)
	}
	ej, _ := os.ReadFile(v.l.ExecJSON())
	mustContain(t, "exec.json", string(ej), `"demo-cluster"`)
	mustNotContain(t, "exec.json", string(ej), "prod-tidb-01")
	if !exists(v.l.ExecEnv()) || !exists(v.l.Env()) {
		t.Error("exec.env / env 未创建")
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{v.l.ExecKey(): 0o640, v.l.ExecEnv(): 0o640, v.l.Env(): 0o640, v.l.ExecJSON(): 0o644, v.l.Sudoers(): 0o440, v.l.Data(): 0o700, v.l.Backups(): 0o700} {
			if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s 权限 = %v，期望 %v", p, st.Mode().Perm(), want)
			}
		}
	}
	// 属主（经 Sys.Chown）
	chown := strings.Join(v.sys.chowns, "\n")
	mustContain(t, "chown 记录", chown, "exec.key tidbops:dbuser", "exec.env root:dbuser", "env root:tidbops", "tidbops.sudoers.new root:root")
	// 外部命令
	if !v.sys.called("useradd --system --home-dir /var/lib/tidbops --shell /sbin/nologin tidbops") {
		t.Errorf("没有按预期创建账号: %v", v.sys.calls)
	}
	for _, c := range []string{"visudo -cf", "systemctl daemon-reload", "systemctl enable --now tidbops"} {
		if !v.sys.called(c) {
			t.Errorf("缺少调用 %s", c)
		}
	}
	// 数据版本与历史
	in, err := dataver.Read(v.l.Data())
	if err != nil || in.Schema != 1 || in.Version != "1.0.0" || in.InstalledAt == nil {
		t.Errorf("VERSION = %+v %v", in, err)
	}
	h := v.history()
	if len(h) != 1 || h[0].Action != "install" || !h[0].OK || h[0].To != "1.0.0" || h[0].Operator != "tester" {
		t.Errorf("历史 = %+v", h)
	}
	if exists(v.l.Lock()) {
		t.Error("锁文件应在结束后删除")
	}
}

func TestInstallIdempotentKeepsSecretsAndConfig(t *testing.T) {
	v := newEnv(t)
	pkg := v.install("1.0.0", pkgOpt{})
	key1, _ := os.ReadFile(v.l.ExecKey())
	_ = os.WriteFile(v.l.ExecJSON(), []byte(`{"custom":true}`), 0o644)
	_ = os.WriteFile(v.l.ExecEnv(), []byte("TIDBOPS_OPS_PWD=secret\n"), 0o640)
	v.seedData("users-v1")

	v.out.Reset()
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "prod-tidb-01", Force: true}); err != nil {
		t.Fatalf("--force 重装: %v", err)
	}
	if key2, _ := os.ReadFile(v.l.ExecKey()); string(key1) != string(key2) {
		t.Error("重装覆盖了 exec.key")
	}
	if b, _ := os.ReadFile(v.l.ExecJSON()); string(b) != `{"custom":true}` {
		t.Errorf("重装覆盖了 exec.json: %s", b)
	}
	if b, _ := os.ReadFile(v.l.ExecEnv()); !strings.Contains(string(b), "secret") {
		t.Error("重装覆盖了 exec.env")
	}
	if v.readData("users.json") != "users-v1" {
		t.Error("重装改动了数据")
	}
	mustContain(t, "输出", v.dumpOut(), "保留已有")
}

func TestInstallRefusesWhenInstalled(t *testing.T) {
	v := newEnv(t)
	pkg := v.install("1.0.0", pkgOpt{})
	err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "prod-tidb-01"})
	if err == nil || !strings.Contains(err.Error(), "admin upgrade") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("应拒绝并提示 upgrade / --force: %v", err)
	}
	pkg2 := v.makePkg("1.1.0", pkgOpt{})
	err = v.e.Install(context.Background(), InstallOpts{Package: pkg2, Cluster: "prod-tidb-01", Force: true})
	if err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("--force 不应用来换版本: %v", err)
	}
	if v.cur() != "1.0.0" {
		t.Error("current 不应改变")
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	v := newEnv(t)
	v.sys.euid = 1000 // dry-run 不需要 root
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1", DryRun: true, Start: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	out := v.dumpOut()
	mustContain(t, "输出", out, "[dry-run]", "创建运行账号", "sudoers", "systemd", "启动服务", "未做任何改动")
	ents, _ := os.ReadDir(v.root)
	if len(ents) != 0 {
		t.Errorf("dry-run 不应在根目录下创建任何东西: %v", ents)
	}
	for _, c := range v.sys.calls {
		if strings.HasPrefix(c, "useradd") || strings.HasPrefix(c, "systemctl") || strings.HasPrefix(c, "visudo") {
			t.Errorf("dry-run 不应执行 %s", c)
		}
	}
}

func TestInstallVisudoFailureLeavesNoSudoers(t *testing.T) {
	v := newEnv(t)
	v.sys.failOn("visudo -cf", -1)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1"})
	if err == nil || !strings.Contains(err.Error(), "sudoers 校验未通过") {
		t.Fatalf("应因 visudo 失败中止: %v", err)
	}
	if exists(v.l.Sudoers()) {
		t.Error("校验失败后不应留下 sudoers")
	}
	ents, _ := os.ReadDir(v.l.SudoersDir())
	if len(ents) != 0 {
		t.Errorf("sudoers.d 里不应留任何文件: %v", ents)
	}
	if exists(v.l.Unit()) {
		t.Error("sudoers 失败后不应继续写 unit")
	}
	if exists(v.l.Lock()) {
		t.Error("失败后也要删除锁文件")
	}
}

func TestInstallValidation(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	ctx := context.Background()
	for name, o := range map[string]InstallOpts{
		"缺集群":       {Package: pkg},
		"集群名含空格":    {Package: pkg, Cluster: "a b"},
		"集群名含引号":    {Package: pkg, Cluster: `a"b`},
		"集群名过长":     {Package: pkg, Cluster: strings.Repeat("a", 65)},
		"集群名以-开头":   {Package: pkg, Cluster: "-abc"},
		"非法 env":    {Package: pkg, Cluster: "c1", Env: "dev"},
		"非法 user":   {Package: pkg, Cluster: "c1", TidbUser: "Root;x"},
		"非法 listen": {Package: pkg, Cluster: "c1", Listen: "bad listen"},
		"非法 url":    {Package: pkg, Cluster: "c1", PublicURL: "ftp://x y"},
	} {
		if err := v.e.Install(ctx, o); err == nil {
			t.Errorf("%s：应被拒绝", name)
		}
	}
	if exists(v.l.Opt()) {
		t.Error("校验失败不应改动系统")
	}

	// 非 root
	v.sys.euid = 1000
	if err := v.e.Install(ctx, InstallOpts{Package: pkg, Cluster: "c1"}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("非 root 应给出清晰提示: %v", err)
	}
	v.sys.euid = 0

	// OS / 架构不符
	arm := v.makePkg("1.0.1", pkgOpt{arch: "arm64"})
	if err := v.e.Install(ctx, InstallOpts{Package: arm, Cluster: "c1"}); err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("架构不符应拒绝: %v", err)
	}
	// 注入本机架构后可通过
	v.e.Arch = "arm64"
	if err := v.e.Install(ctx, InstallOpts{Package: arm, Cluster: "c1"}); err != nil {
		t.Errorf("本机为 arm64 时应通过: %v", err)
	}
	v.e.Arch = "amd64"
}

func TestInstallRejectsTamperedPackage(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	_ = os.WriteFile(filepath.Join(pkg, "web", "app.js"), []byte("evil"), 0o644)
	err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1"})
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("被篡改的包应拒绝: %v", err)
	}
	// 多出文件
	pkg2 := v.makePkg("1.0.1", pkgOpt{})
	_ = os.WriteFile(filepath.Join(pkg2, "extra.sh"), []byte("x"), 0o755)
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg2, Cluster: "c1"}); err == nil {
		t.Fatal("多出清单之外的文件应拒绝")
	}
	if exists(v.l.Opt()) {
		t.Error("不应改动系统")
	}
}

func TestInstallMissingPrerequisites(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	delete(v.sys.users, "tidb")
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1"}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("tidb 用户不存在应拒绝: %v", err)
	}
	v.sys.users["tidb"] = true
	v.sys.noLook["visudo"] = true
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1"}); err == nil || !strings.Contains(err.Error(), "visudo") {
		t.Errorf("缺 visudo 应拒绝: %v", err)
	}
}

func TestInstallLegacyDataWithoutVersion(t *testing.T) {
	v := newEnv(t)
	v.seedData("old-users")
	v.install("1.0.0", pkgOpt{})
	if exists(filepath.Join(v.l.Data(), dataver.FileName)) {
		t.Error("已有无 VERSION 的旧数据时 install 不应伪造 VERSION（交给启动时迁移）")
	}
	mustContain(t, "输出", v.dumpOut(), "schema 0")
	if v.readData("users.json") != "old-users" {
		t.Error("不应改动旧数据")
	}
}

func TestLockPreventsConcurrentOperations(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	_ = os.MkdirAll(filepath.Dir(v.l.Lock()), 0o755)
	_ = os.WriteFile(v.l.Lock(), []byte("pid=1 operator=other"), 0o600)
	err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1"})
	if err == nil || !strings.Contains(err.Error(), "锁文件") || !strings.Contains(err.Error(), "另一个") {
		t.Fatalf("锁已存在应拒绝: %v", err)
	}
	if !exists(v.l.Lock()) {
		t.Error("不能删除别人的锁")
	}
	if exists(v.l.Opt()) {
		t.Error("拿不到锁就不能动系统")
	}
}
