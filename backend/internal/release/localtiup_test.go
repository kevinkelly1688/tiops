// @author: kevinkelly
// @license: Apache License 2.0
// @file: localtiup_test.go
// @time: 2026/10/28

package release

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

// 中控机模式（--local-tiup，隐含 --standalone）：不预设集群，但保留 tidb 用户 / sudoers / exec.json / exec.key。
func TestInstallLocalTiupFlow(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true, Env: "prod", Listen: "127.0.0.1:8080", Start: true}); err != nil {
		t.Fatalf("install --local-tiup: %v\n%s", err, v.dumpOut())
	}
	mustContain(t, "输出", v.dumpOut(), "安装完成", "中控机模式", "一键导入", "cluster.list", "tiup cluster list")
	mustNotContain(t, "输出", v.dumpOut(), "--cluster")

	u, _ := os.ReadFile(v.l.Unit())
	unit := string(u)
	mustContain(t, "unit", unit, "--standalone", "--local-tiup", "--exec sudo", "--exec-sudo-user tidb", "--exec-policy", "--key-file", "--home /home/tidb", "--data-dir /var/lib/tidbops")
	mustNotContain(t, "unit", unit, "--cluster", "--mysql-user")

	for name, p := range map[string]string{"sudoers": v.l.Sudoers(), "exec.json": v.l.ExecJSON(), "exec.key": v.l.ExecKey(), "exec.env": v.l.ExecEnv(), "env": v.l.Env()} {
		if !exists(p) {
			t.Errorf("中控机模式应创建 %s（%s）", name, p)
		}
	}
	if !v.sys.called("visudo") {
		t.Errorf("sudoers 应经 visudo 校验: %v", v.sys.calls)
	}

	// exec.json：集群名通配；不能留下样例里的全局 SQL / Alertmanager 地址（会让别的集群的写命令指向样例集群）
	b, _ := os.ReadFile(v.l.ExecJSON())
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("exec.json 不是合法 JSON: %v\n%s", err, b)
	}
	if cl, _ := m["clusters"].([]any); len(cl) != 1 || cl[0] != "*" {
		t.Errorf("clusters 应为 [\"*\"]: %v", m["clusters"])
	}
	for _, k := range []string{"mysqlHost", "mysqlPort", "alertURL"} {
		if _, has := m[k]; has {
			t.Errorf("中控机模式的 exec.json 不应带全局 %s: %s", k, b)
		}
	}
	if m["tiupBin"] == nil || m["mysqlUser"] == nil {
		t.Errorf("其余策略项应保留: %s", b)
	}

	p, err := ParseUnit(unit)
	if err != nil || !p.Standalone || !p.LocalTiup || p.Cluster != "" || !p.UsesExec() {
		t.Errorf("ParseUnit 往返失败: %+v %v", p, err)
	}
	if u2, err := RenderUnit(unit, p); err != nil || u2 != unit {
		t.Errorf("渲染不幂等 (%v):\n%s\n---\n%s", err, unit, u2)
	}
}

func TestInstallLocalTiupValidation(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true, Cluster: "c1"}); err == nil || !strings.Contains(err.Error(), "--standalone") {
		t.Errorf("--local-tiup 与 --cluster 同用应拒绝: %v", err)
	}
	// 本机没有 tidb 用户：中控机模式需要它
	delete(v.sys.users, "tidb")
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true}); err == nil || !strings.Contains(err.Error(), "tidb") {
		t.Errorf("没有 tidb 用户应拒绝: %v", err)
	}
}

// 升级沿用 --local-tiup：单元里保留执行器选项，sudoers 照常随包更新。
func TestUpgradeLocalTiupKeepsModeAndSyncsSudoers(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true, Start: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	if _, err := v.upgrade("1.1.0", pkgOpt{extra: map[string]string{"deploy/ops/sudoers.tidbops": tmplSudoers + "# new\n"}}, UpgradeOpts{}); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, v.dumpOut())
	}
	u, _ := os.ReadFile(v.l.Unit())
	mustContain(t, "unit", string(u), "--standalone", "--local-tiup", "--exec sudo")
	mustNotContain(t, "unit", string(u), "--cluster")
	if s, _ := os.ReadFile(v.l.Sudoers()); !strings.Contains(string(s), "# new") {
		t.Errorf("中控机模式升级应更新 sudoers: %s", s)
	}
}

// 安装器替用户准备执行器的运行条件：临时目录（不在 /tmp 下，属主 tidb、0700）与放开沙箱可写路径的 drop-in；卸载时删 drop-in。
func TestInstallPreparesExecTempDirAndDropIn(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	st, err := os.Stat(v.l.ExecTemp())
	if err != nil || !st.IsDir() {
		t.Fatalf("执行器临时目录应已创建: %v", err)
	}
	if st.Mode().Perm() != 0o700 && runtime.GOOS != "windows" {
		t.Errorf("临时目录权限应为 0700，实际 %v", st.Mode().Perm())
	}
	d, err := os.ReadFile(v.l.DropIn())
	if err != nil {
		t.Fatalf("drop-in 应已写入: %v", err)
	}
	mustContain(t, "drop-in", string(d), "[Service]", "ReadWritePaths=-/home/tidb/.tiup -"+ExecTempDir)
	// 已有（可能被用户改过的）drop-in 不覆盖
	if err := os.WriteFile(v.l.DropIn(), []byte("[Service]\nReadWritePaths=/data/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true, Force: true}); err != nil {
		t.Fatalf("重装: %v\n%s", err, v.dumpOut())
	}
	if d2, _ := os.ReadFile(v.l.DropIn()); !strings.Contains(string(d2), "/data/x") {
		t.Errorf("重装不应覆盖已有 drop-in: %s", d2)
	}
	if err := v.e.Uninstall(context.Background(), UninstallOpts{}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(v.l.DropIn()); err == nil {
		t.Errorf("卸载应删除 drop-in")
	}
}

// 已有 exec.json 的 tempDir 在 /tmp 下（旧安装器生成）：保留文件，但提示用户改。
func TestInstallWarnsLegacyTempDir(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := os.MkdirAll(v.l.Etc(), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"clusters":["tidb-test"],"tiupBin":"/home/tidb/.tiup/bin/tiup","tempDir":"/tmp/tidbops-exec"}` + "\n"
	if err := os.WriteFile(v.l.ExecJSON(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	if b, _ := os.ReadFile(v.l.ExecJSON()); string(b) != old {
		t.Errorf("已有 exec.json 不应被改动: %s", b)
	}
	mustContain(t, "安装输出", v.dumpOut(), "tempDir", "/tmp/tidbops-exec", ExecTempDir, "PrivateTmp")
}

func TestRealTemplateLocalTiupRender(t *testing.T) {
	ub, err := os.ReadFile("../../../deploy/ops/tidbops.service")
	if err != nil {
		t.Skip("找不到 deploy/ops 模板，跳过")
	}
	u, err := RenderUnit(string(ub), UnitParams{Env: "prod", TidbUser: "tidb", Listen: "127.0.0.1:8080", Standalone: true, LocalTiup: true})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "真实 unit（中控机）", u, "--standalone", "--local-tiup", "--exec sudo", "--exec-sudo-user tidb", "\nNoNewPrivileges=no\n", "ProtectSystem=strict")
	mustNotContain(t, "真实 unit（中控机）", u, "--cluster", "--mysql-user", "\nNoNewPrivileges=yes\n") // sudo 需要提权，不能开 NoNewPrivileges

	ex, err := os.ReadFile("../../../deploy/ops/exec.json.example")
	if err != nil {
		t.Skip("找不到 exec.json.example，跳过")
	}
	b, err := RenderExecJSONLocal(string(ex))
	if err != nil {
		t.Fatalf("真实样例应能生成中控机策略: %v", err)
	}
	mustContain(t, "真实 exec.json（中控机）", string(b), `"*"`, "tiupBin", "mysqlUser", "backupRoots", `"tempDir": "`+ExecTempDir+`"`)
	mustNotContain(t, "真实 exec.json（中控机）", string(b), "/tmp/")
	mustNotContain(t, "真实 exec.json（中控机）", string(b), "mysqlHost", "mysqlPort", "alertURL", "prod-tidb-01")
}

// 旧版安装器装的中控机（没有执行器临时目录与沙箱 drop-in）：升级时补上；已有的 drop-in（可能被用户改过）保留。
func TestUpgradeBackfillsExecRuntimeForOldInstall(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, LocalTiup: true, Start: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	// 模拟旧版安装器：没有临时目录、没有 drop-in
	if err := os.RemoveAll(v.l.ExecTemp()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(v.l.DropIn()); err != nil {
		t.Fatal(err)
	}
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{}); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, v.dumpOut())
	}
	mustContain(t, "升级输出", v.dumpOut(), "补齐执行器运行条件", "已建执行器临时目录", "已写入 systemd drop-in")
	st, err := os.Stat(v.l.ExecTemp())
	if err != nil || !st.IsDir() {
		t.Fatalf("升级应补建临时目录: %v", err)
	}
	if st.Mode().Perm() != 0o700 && runtime.GOOS != "windows" {
		t.Errorf("临时目录权限应为 0700，实际 %v", st.Mode().Perm())
	}
	d, err := os.ReadFile(v.l.DropIn())
	if err != nil {
		t.Fatalf("升级应补写 drop-in: %v", err)
	}
	mustContain(t, "drop-in", string(d), "[Service]", "ReadWritePaths=-/home/tidb/.tiup -"+ExecTempDir)

	// 再升级一次：drop-in 已被用户改过，必须原样保留，也不再提示「已写入」
	custom := "[Service]\nReadWritePaths=/data/x\n"
	if err := os.WriteFile(v.l.DropIn(), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v.upgrade("1.2.0", pkgOpt{}, UpgradeOpts{}); err != nil {
		t.Fatalf("upgrade 2: %v\n%s", err, v.dumpOut())
	}
	if d2, _ := os.ReadFile(v.l.DropIn()); string(d2) != custom {
		t.Errorf("已有 drop-in 不应被覆盖: %q", d2)
	}
}

// 纯独立部署（没有 sudo 执行器）升级时不建执行器临时目录与 drop-in；预演（--dry-run）不改动系统，但计划里要写明这一步。
func TestUpgradeExecRuntimeOnlyForExecModesAndDryRunUntouched(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Start: true}); err != nil {
		t.Fatalf("install standalone: %v\n%s", err, v.dumpOut())
	}
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{}); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, v.dumpOut())
	}
	for name, p := range map[string]string{"临时目录": v.l.ExecTemp(), "drop-in": v.l.DropIn()} {
		if exists(p) {
			t.Errorf("纯独立部署升级不应创建%s（%s）", name, p)
		}
	}
	mustNotContain(t, "独立部署升级输出", v.dumpOut(), "补齐执行器运行条件")

	// 中控机：dry-run 计划里有这一步，但什么都不建
	v2 := newEnv(t)
	pkg2 := v2.makePkg("1.0.0", pkgOpt{})
	if err := v2.e.Install(context.Background(), InstallOpts{Package: pkg2, LocalTiup: true, Start: true}); err != nil {
		t.Fatalf("install local-tiup: %v\n%s", err, v2.dumpOut())
	}
	os.RemoveAll(v2.l.ExecTemp())
	os.RemoveAll(v2.l.DropIn())
	if _, err := v2.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v\n%s", err, v2.dumpOut())
	}
	mustContain(t, "dry-run 输出", v2.dumpOut(), "补齐执行器运行条件")
	if exists(v2.l.ExecTemp()) || exists(v2.l.DropIn()) {
		t.Error("dry-run 不应创建任何东西")
	}
}
