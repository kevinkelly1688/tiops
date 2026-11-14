// @author: kevinkelly
// @license: Apache License 2.0
// @file: standalone_test.go
// @time: 2026/10/28

package release

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 独立部署：不依赖 tidb 用户、visudo、集群名；不产生 sudoers / exec.json / exec.key / exec.env。
func TestInstallStandaloneFlow(t *testing.T) {
	v := newEnv(t)
	delete(v.sys.users, "tidb") // 本机没有 tiup 部署用户
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Env: "prod", Listen: "127.0.0.1:8080", PublicURL: "https://ops.example.com", Start: true}); err != nil {
		t.Fatalf("install --standalone: %v\n%s", err, v.dumpOut())
	}
	mustContain(t, "输出", v.dumpOut(), "安装完成", "独立部署", "集群接入", "user add")
	mustNotContain(t, "输出", v.dumpOut(), "visudo", "sudo -n -u")

	u, _ := os.ReadFile(v.l.Unit())
	unit := string(u)
	mustContain(t, "unit", unit, "--standalone", "--data-dir /var/lib/tidbops", "--env prod", "--public-url https://ops.example.com", "--listen 127.0.0.1:8080", "ExecStart=/opt/tidbops/current/bin/tidbops-server")
	mustNotContain(t, "unit", unit, "--cluster", "--exec", "--home", "--mysql-user", "--key-file", "--exec-policy")

	for name, p := range map[string]string{"sudoers": v.l.Sudoers(), "exec.json": v.l.ExecJSON(), "exec.key": v.l.ExecKey(), "exec.env": v.l.ExecEnv(), "env": v.l.Env()} {
		if exists(p) {
			t.Errorf("独立部署不应创建 %s（%s）", name, p)
		}
	}
	for _, c := range v.sys.calls {
		if c == "id tidb" || strings.Contains(c, "visudo") {
			t.Errorf("独立部署不应检查 tidb 用户 / visudo: %v", v.sys.calls)
		}
	}
	if !v.sys.called("systemctl enable --now tidbops") {
		t.Errorf("应启动服务: %v", v.sys.calls)
	}

	p, err := ParseUnit(unit)
	if err != nil || !p.Standalone || p.Cluster != "" || p.Env != "prod" || p.PublicURL != "https://ops.example.com" {
		t.Errorf("ParseUnit 往返失败: %+v %v", p, err)
	}
	// 渲染幂等
	u2, err := RenderUnit(unit, p)
	if err != nil || u2 != unit {
		t.Errorf("渲染不幂等 (%v):\n%s\n---\n%s", err, unit, u2)
	}
}

func TestInstallStandaloneValidation(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	ctx := context.Background()
	if err := v.e.Install(ctx, InstallOpts{Package: pkg, Standalone: true, Cluster: "c1"}); err == nil || !strings.Contains(err.Error(), "--standalone") {
		t.Errorf("--standalone 与 --cluster 同用应拒绝: %v", err)
	}
	if err := v.e.Install(ctx, InstallOpts{Package: pkg}); err == nil || !strings.Contains(err.Error(), "--standalone") {
		t.Errorf("缺 --cluster 的错误应提示 --standalone: %v", err)
	}
	if exists(v.l.Opt()) {
		t.Error("校验失败不应改动系统")
	}
}

func TestInstallStandaloneDryRunChangesNothing(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, DryRun: true}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	if exists(v.l.Opt()) || exists(v.l.Unit()) {
		t.Error("dry-run 不应写任何文件")
	}
	mustContain(t, "输出", v.dumpOut(), "dry-run", "独立部署")
}

// 升级沿用 --standalone，且不创建 / 校验 sudoers。
func TestUpgradeStandaloneKeepsModeAndSkipsSudoers(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Start: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	v.sys.calls = nil
	// 新版本的 sudoers 模板不同：独立部署也不应去装它
	if _, err := v.upgrade("1.1.0", pkgOpt{extra: map[string]string{"deploy/ops/sudoers.tidbops": tmplSudoers + "# new\n"}}, UpgradeOpts{}); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, v.dumpOut())
	}
	if v.cur() != "1.1.0" {
		t.Fatalf("current = %q", v.cur())
	}
	u, _ := os.ReadFile(v.l.Unit())
	mustContain(t, "unit", string(u), "--standalone")
	mustNotContain(t, "unit", string(u), "--cluster", "--exec")
	if exists(v.l.Sudoers()) || v.sys.called("visudo") {
		t.Errorf("独立部署升级不应处理 sudoers: %v", v.sys.calls)
	}
}

func TestRealTemplateStandaloneRender(t *testing.T) {
	ub, err := os.ReadFile("../../../deploy/ops/tidbops.service")
	if err != nil {
		t.Skip("找不到 deploy/ops 模板，跳过")
	}
	u, err := RenderUnit(string(ub), UnitParams{Env: "test", TidbUser: "tidb", Listen: "0.0.0.0:8080", Standalone: true})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "真实 unit（独立部署）", u, "--standalone", "--data-dir /var/lib/tidbops", "--env test", "ProtectSystem=strict", "MemoryMax=1G", "NoNewPrivileges=yes")
	mustNotContain(t, "真实 unit（独立部署）", u, "NoNewPrivileges=no", "sudo")
	mustNotContain(t, "真实 unit（独立部署）", u, "--exec", "--home", "--mysql-user", "--key-file", "--cluster")
}
