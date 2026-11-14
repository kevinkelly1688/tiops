// @author: kevinkelly
// @license: Apache License 2.0
// @file: upgrade_test.go
// @time: 2026/10/28

package release

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tidbops/internal/dataver"
)

// 读取备份归档里的文件内容。
func readBackupFile(t *testing.T, archive, name string) string {
	t.Helper()
	dst := t.TempDir()
	if err := extractTarGz(archive, dst); err != nil {
		t.Fatalf("解压备份 %s: %v", archive, err)
	}
	b, err := os.ReadFile(filepath.Join(dst, name))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func backupFiles(t *testing.T, v *tenv) []string {
	t.Helper()
	bs, err := v.e.listBackups()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range bs {
		out = append(out, b.Path)
	}
	return out
}

func TestUpgradeSuccess(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	v.sys.calls = nil

	_, err := v.upgrade("1.1.0", pkgOpt{schema: 2}, UpgradeOpts{})
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "输出", out, "预检", "升级完成：1.0.0 → 1.1.0", "备份")

	if v.cur() != "1.1.0" {
		t.Fatalf("current = %q", v.cur())
	}
	// 备份存在、内容正确（迁移前的数据 + VERSION）、0600
	bks := backupFiles(t, v)
	if len(bks) != 1 || !strings.HasSuffix(bks[0], "-1.0.0.tar.gz") {
		t.Fatalf("备份 = %v", bks)
	}
	if got := readBackupFile(t, bks[0], "users.json"); got != "users-v1" {
		t.Errorf("备份里的 users.json = %q，应为迁移前的数据", got)
	}
	if got := readBackupFile(t, bks[0], "VERSION"); !strings.Contains(got, `"schema": 1`) {
		t.Errorf("备份应含升级前的 VERSION: %s", got)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(bks[0]); st.Mode().Perm() != 0o600 {
			t.Errorf("备份权限 = %v", st.Mode().Perm())
		}
	}
	// 数据已迁移
	if got := v.readData("users.json"); got != "users-v1|migrated-to-2" {
		t.Errorf("迁移后 users.json = %q", got)
	}
	if s, _ := v.e.currentSchema(); s != 2 {
		t.Errorf("schema = %d", s)
	}
	// 旧 release 保留
	if !exists(v.l.ReleaseDir("1.0.0")) || !exists(v.l.ReleaseDir("1.1.0")) {
		t.Error("两个 release 都应存在")
	}
	// 历史
	h := v.history()
	last := h[len(h)-1]
	if last.Action != "upgrade" || !last.OK || last.RolledBack || last.From != "1.0.0" || last.To != "1.1.0" || last.SchemaFrom != 1 || last.SchemaTo != 2 || last.Backup != bks[0] || last.Operator != "tester" {
		t.Errorf("历史 = %+v", last)
	}
	// 命令顺序：stop → migrate → start
	iStop := v.sys.indexOf("systemctl stop tidbops")
	iMig := v.sys.indexOf(filepath.Join(v.l.ReleaseDir("1.1.0"), "bin", "tidbops-server") + " admin migrate --data-dir " + v.l.Data())
	iStart := v.sys.indexOf("systemctl start tidbops")
	if !(iStop >= 0 && iStop < iMig && iMig < iStart) {
		t.Errorf("命令顺序不对 stop=%d migrate=%d start=%d: %v", iStop, iMig, iStart, v.sys.calls)
	}
	if !v.sys.isActive() {
		t.Error("升级后服务应在运行")
	}
	// 参数保留
	unit, _ := os.ReadFile(v.l.Unit())
	mustContain(t, "unit", string(unit), "--cluster prod-tidb-01", "--env staging", "--listen 127.0.0.1:9090")
	if exists(v.l.Lock()) {
		t.Error("锁文件应已删除")
	}
}

func TestUpgradeUpdatesChangedUnitKeepingParams(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	newUnit := strings.Replace(tmplUnit, "--mysql-user tidbops_ro", "--mysql-user tidbops_ro \\\n  --newflag yes", 1)
	if _, err := v.upgrade("1.1.0", pkgOpt{unit: newUnit}, UpgradeOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	u, _ := os.ReadFile(v.l.Unit())
	mustContain(t, "unit", string(u), "--newflag yes", "--cluster prod-tidb-01", "--env staging", "--listen 127.0.0.1:9090", "--exec-sudo-user tidb")
	mustContain(t, "输出", v.dumpOut(), "单元已更新")
	// 回滚后用旧模板还原单元（旧二进制不认识新参数）
	if err := v.e.Rollback(ctxBG(), RollbackOpts{}); err != nil {
		t.Fatalf("rollback: %v\n%s", err, v.dumpOut())
	}
	u, _ = os.ReadFile(v.l.Unit())
	mustNotContain(t, "回滚后的 unit", string(u), "--newflag")
	mustContain(t, "回滚后的 unit", string(u), "--cluster prod-tidb-01")
}

func TestUpgradeSudoersVisudoFailureRollsBack(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	// 新版本 sudoers 模板不同（多一行）→ 需要更新并校验；让 visudo 失败
	pkg := v.makePkg("1.1.0", pkgOpt{extra: map[string]string{"deploy/ops/sudoers.tidbops": tmplSudoers + "# changed\n"}})
	v.sys.failOn("visudo -cf", -1)
	err := v.e.Upgrade(ctxBG(), UpgradeOpts{Package: pkg})
	if err == nil || !strings.Contains(err.Error(), "已自动回滚到 1.0.0") {
		t.Fatalf("应失败并自动回滚: %v", err)
	}
	if v.cur() != "1.0.0" || !v.sys.isActive() {
		t.Errorf("回滚后应回到 1.0.0 且服务运行 cur=%s", v.cur())
	}
}

func TestUpgradeCleanupPolicy(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	// 先放 12 个旧备份，其中 1 个手动备份不应被清理
	_ = os.MkdirAll(v.l.Backups(), 0o700)
	for i := 1; i <= 12; i++ {
		_ = os.WriteFile(filepath.Join(v.l.Backups(), fmt.Sprintf("202001%02d-000000-0.9.0.tar.gz", i)), []byte("x"), 0o600)
	}
	_ = os.WriteFile(filepath.Join(v.l.Backups(), "20190101-000000-0.8.0-manual.tar.gz"), []byte("x"), 0o600)

	for _, ver := range []string{"1.1.0", "1.2.0", "1.3.0", "1.4.0"} {
		if _, err := v.upgrade(ver, pkgOpt{}, UpgradeOpts{Keep: 2}); err != nil {
			t.Fatalf("upgrade %s: %v\n%s", ver, err, v.dumpOut())
		}
	}
	rels, _ := v.e.listReleases()
	if strings.Join(rels, ",") != "1.4.0,1.3.0" {
		t.Errorf("releases = %v，应只保留最近 2 个", rels)
	}
	if v.cur() != "1.4.0" {
		t.Errorf("current = %s", v.cur())
	}
	bks := backupFiles(t, v)
	auto, manual := 0, 0
	for _, b := range bks {
		if strings.Contains(b, "-manual") {
			manual++
		} else {
			auto++
		}
	}
	if auto != 10 || manual != 1 {
		t.Errorf("自动备份应保留最近 10 个、手动备份保留: auto=%d manual=%d (%v)", auto, manual, bks)
	}
}

func TestUpgradeKeepNeverRemovesCurrentOrPrevious(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{Keep: 1}); err != nil {
		t.Fatal(err)
	}
	// keep=1 但 previous（1.0.0）必须保留
	if !exists(v.l.ReleaseDir("1.0.0")) || !exists(v.l.ReleaseDir("1.1.0")) {
		t.Error("current 与 previous 永不删除")
	}
}

func TestUpgradeRefusals(t *testing.T) {
	type tc struct {
		name   string
		setup  func(v *tenv)
		ver    string
		opt    pkgOpt
		uo     UpgradeOpts
		tamper bool
		want   string
	}
	cases := []tc{
		{name: "降级", ver: "0.9.0", want: "不允许降级"},
		{name: "同版本", ver: "1.0.0", want: "无需升级"},
		{name: "minFrom", ver: "3.0.0", opt: pkgOpt{minFrom: "2.0.0"}, want: "2.0.0"},
		{name: "schema 更低", setup: func(v *tenv) {
			_ = dataver.Write(v.l.Data(), dataver.Info{Schema: 2, Version: "1.0.0"})
		}, ver: "1.1.0", opt: pkgOpt{schema: 1}, want: "低于数据目录 schema"},
		{name: "有执行中的变更", setup: func(v *tenv) {
			_ = os.WriteFile(filepath.Join(v.l.Data(), "changes.json"), []byte(`{"changes":[{"id":"CHG-7","state":"running"},{"id":"CHG-6","state":"done"}]}`), 0o600)
		}, ver: "1.1.0", want: "请等待执行中的变更结束，或加 --force"},
		{name: "包被篡改", ver: "1.1.0", tamper: true, want: "校验失败"},
		{name: "锁已存在", setup: func(v *tenv) {
			_ = os.MkdirAll(filepath.Dir(v.l.Lock()), 0o755)
			_ = os.WriteFile(v.l.Lock(), []byte("pid=99"), 0o600)
		}, ver: "1.1.0", want: "锁文件"},
		{name: "非 root", setup: func(v *tenv) { v.sys.euid = 1000 }, ver: "1.1.0", want: "root"},
		{name: "架构不符", ver: "1.1.0", opt: pkgOpt{arch: "arm64"}, want: "arm64"},
		{name: "磁盘空间不足", setup: func(v *tenv) { v.sys.free = 1024 }, ver: "1.1.0", want: "可用空间"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newEnv(t)
			v.install("1.0.0", pkgOpt{})
			v.seedData("users-v1")
			if c.setup != nil {
				c.setup(v)
			}
			pkg := v.makePkg(c.ver, c.opt)
			if c.tamper {
				_ = os.WriteFile(filepath.Join(pkg, "web", "app.js"), []byte("tampered"), 0o644)
			}
			v.sys.calls = nil
			c.uo.Package = pkg
			v.out.Reset()
			err := v.e.Upgrade(ctxBG(), c.uo)
			if err == nil || !strings.Contains(err.Error()+v.dumpOut(), c.want) {
				t.Fatalf("应拒绝且提示 %q，实际 err=%v\n%s", c.want, err, v.dumpOut())
			}
			// 预检阶段拒绝：什么都没动
			if v.cur() != "1.0.0" {
				t.Errorf("current 不应改变: %s", v.cur())
			}
			if v.sys.called("systemctl stop") || v.sys.called("systemctl start") {
				t.Errorf("预检失败不应碰服务: %v", v.sys.calls)
			}
			if len(backupFiles(t, v)) != 0 {
				t.Error("预检失败不应产生备份")
			}
			if c.ver != "1.0.0" && exists(v.l.ReleaseDir(c.ver)) {
				t.Error("预检失败不应安装 release")
			}
			if v.readData("users.json") != "users-v1" {
				t.Error("数据不应改变")
			}
		})
	}
}

func TestUpgradeAllowDowngradeAndForceRunningChange(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	_ = os.WriteFile(filepath.Join(v.l.Data(), "changes.json"), []byte(`{"changes":[{"id":"CHG-7","state":"running"}]}`), 0o600)
	if _, err := v.upgrade("0.9.0", pkgOpt{}, UpgradeOpts{AllowDowngrade: true, Force: true}); err != nil {
		t.Fatalf("加 --allow-downgrade --force 应放行: %v\n%s", err, v.dumpOut())
	}
	if v.cur() != "0.9.0" {
		t.Errorf("current = %s", v.cur())
	}
	mustContain(t, "输出", v.dumpOut(), "标记为中断")
}

func TestUpgradeDryRun(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	v.sys.euid = 1000
	v.sys.calls = nil
	pkg := v.makePkg("1.1.0", pkgOpt{schema: 2})
	v.out.Reset()
	if err := v.e.Upgrade(ctxBG(), UpgradeOpts{Package: pkg, DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v\n%s", err, v.dumpOut())
	}
	mustContain(t, "输出", v.dumpOut(), "预检", "计划：升级 1.0.0 → 1.1.0", "[dry-run]", "未做任何改动", "数据 schema 1 → 2")
	if v.cur() != "1.0.0" || exists(v.l.ReleaseDir("1.1.0")) || len(backupFiles(t, v)) != 0 {
		t.Error("dry-run 不应改动任何东西")
	}
	for _, c := range v.sys.calls {
		if strings.HasPrefix(c, "systemctl") || strings.Contains(c, "migrate") {
			t.Errorf("dry-run 不应执行 %s", c)
		}
	}
	// dry-run 会列出全部失败项
	bad := v.makePkg("0.5.0", pkgOpt{minFrom: "9.0.0", schema: 1})
	_ = os.WriteFile(filepath.Join(v.l.Data(), "changes.json"), []byte(`{"changes":[{"id":"CHG-1","state":"running"}]}`), 0o600)
	v.out.Reset()
	err := v.e.Upgrade(ctxBG(), UpgradeOpts{Package: bad, DryRun: true})
	if err == nil {
		t.Fatal("预检有失败项时 dry-run 应返回错误")
	}
	mustContain(t, "输出", v.dumpOut(), "不允许降级", "执行中的变更")
}

func TestUpgradeNoBackupWarns(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{NoBackup: true}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "输出", v.dumpOut(), "--no-backup")
	if len(backupFiles(t, v)) != 0 {
		t.Error("--no-backup 不应产生备份")
	}
	h := v.history()
	if h[len(h)-1].Backup != "" {
		t.Error("历史里不应有备份路径")
	}
}

func TestUpgradeStopFailureAborts(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.sys.failOn("systemctl stop", -1)
	_, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "停止服务失败") {
		t.Fatalf("应因停服务失败中止: %v", err)
	}
	if v.cur() != "1.0.0" || exists(v.l.ReleaseDir("1.1.0")) || len(backupFiles(t, v)) != 0 {
		t.Error("停服务失败后不应有任何改动")
	}
	if h := v.history(); h[len(h)-1].OK || h[len(h)-1].RolledBack {
		t.Errorf("历史应记录失败（未回滚）: %+v", h[len(h)-1])
	}
}

func TestUpgradeStopTimeout(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	// stop 命令成功，但服务一直显示 active（退出超时）
	v.sys.onRun = func(name string, args []string) (bool, []byte, error) {
		if name == "systemctl" && args[0] == "is-active" {
			return true, []byte("active\n"), nil
		}
		if name == "systemctl" && args[0] == "stop" {
			return true, nil, nil
		}
		return v.onRun(name, args)
	}
	v.e.StopTimeout = 3e9
	_, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应因等待退出超时中止: %v", err)
	}
	if v.cur() != "1.0.0" {
		t.Error("不应切换")
	}
}

func TestUpgradeHealthFailureRollsBackBinaryOnly(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	v.healthBad["1.1.0"] = true // 新版本起不来；schema 不变，所以不恢复数据
	_, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "已自动回滚到 1.0.0") {
		t.Fatalf("应报告已自动回滚: %v", err)
	}
	mustContain(t, "输出", v.dumpOut(), "已自动回滚到 1.0.0")
	if v.cur() != "1.0.0" {
		t.Errorf("符号链接应切回旧版本: %s", v.cur())
	}
	if !v.sys.isActive() {
		t.Error("回滚后旧版本应在运行")
	}
	if v.readData("users.json") != "users-v1" {
		t.Error("数据不应改变")
	}
	if v.sys.called("systemctl stop tidbops") && v.countRestoreBackups() != 0 {
		t.Error("schema 未变化时不应恢复数据 / 做 pre-restore 备份")
	}
	h := v.history()
	last := h[len(h)-1]
	if last.Action != "upgrade" || last.OK || !last.RolledBack || !strings.Contains(last.Error, "503") {
		t.Errorf("历史应记录 rolledBack=true: %+v", last)
	}
	if exists(v.l.Lock()) {
		t.Error("锁文件应已删除")
	}
}

func (v *tenv) countRestoreBackups() int {
	n := 0
	bs, _ := v.e.listBackups()
	for _, b := range bs {
		if strings.Contains(b.Name, "-pre-restore") {
			n++
		}
	}
	return n
}

func TestUpgradeHealthFailureRestoresMigratedData(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	_ = os.WriteFile(filepath.Join(v.l.Data(), "audit.log"), []byte("audit-1"), 0o600)
	v.healthBad["1.1.0"] = true
	_, err := v.upgrade("1.1.0", pkgOpt{schema: 2}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "已自动回滚到 1.0.0") {
		t.Fatalf("应自动回滚: %v\n%s", err, v.dumpOut())
	}
	if v.cur() != "1.0.0" || !v.sys.isActive() {
		t.Error("应回到旧版本并运行")
	}
	// 迁移改过的数据被恢复
	if got := v.readData("users.json"); got != "users-v1" {
		t.Errorf("users.json = %q，应从备份恢复为迁移前", got)
	}
	if got := v.readData("audit.log"); got != "audit-1" {
		t.Errorf("audit.log = %q", got)
	}
	if s, _ := v.e.currentSchema(); s != 1 {
		t.Errorf("schema 应回到 1: %d", s)
	}
	if v.countRestoreBackups() != 1 {
		t.Error("恢复前应对当前（迁移后）数据再备份一次")
	}
	// 恢复数据目录之后，升级历史仍然保留（含这次失败的升级）
	h := v.history()
	if len(h) < 2 {
		t.Fatalf("历史丢失: %+v", h)
	}
	last := h[len(h)-1]
	if last.Action != "upgrade" || !last.RolledBack || last.OK || last.SchemaFrom != 1 || last.SchemaTo != 1 {
		t.Errorf("历史 = %+v", last)
	}
	if h[0].Action != "install" {
		t.Errorf("安装记录应保留: %+v", h[0])
	}
}

func TestUpgradeMigrationFailureRestoresAndRollsBack(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	v.migFail = fmt.Errorf("migration blew up")
	_, err := v.upgrade("1.1.0", pkgOpt{schema: 2}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "数据迁移失败") || !strings.Contains(err.Error(), "已自动回滚到 1.0.0") {
		t.Fatalf("err = %v", err)
	}
	if got := v.readData("users.json"); got != "users-v1" {
		t.Errorf("半途失败的迁移应被备份恢复，users.json = %q", got)
	}
	if v.cur() != "1.0.0" || !v.sys.isActive() {
		t.Error("应回到旧版本并运行")
	}
	if s, _ := v.e.currentSchema(); s != 1 {
		t.Errorf("schema = %d", s)
	}
}

func TestUpgradeStartFailureOnceRollsBack(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.sys.failOn("systemctl start tidbops", 1) // 新版本启动失败一次，回滚时的启动成功
	_, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "systemctl start 失败") || !strings.Contains(err.Error(), "已自动回滚到 1.0.0") {
		t.Fatalf("err = %v", err)
	}
	if v.cur() != "1.0.0" || !v.sys.isActive() {
		t.Error("应回到旧版本并运行")
	}
	h := v.history()
	if !h[len(h)-1].RolledBack {
		t.Error("历史应标记 rolledBack")
	}
}

func TestUpgradeStartFailureAlwaysReportsRollbackFailure(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.sys.failOn("systemctl start tidbops", -1)
	_, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "自动回滚到 1.0.0 也失败") {
		t.Fatalf("err = %v", err)
	}
	h := v.history()
	if last := h[len(h)-1]; last.RolledBack || last.OK || !strings.Contains(last.Error, "自动回滚失败") {
		t.Errorf("历史 = %+v", last)
	}
	if exists(v.l.Lock()) {
		t.Error("锁文件应已删除")
	}
}

func TestUpgradeBackupFailureRestartsOld(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	// 让备份目录的位置被一个普通文件占住 → 备份失败
	_ = os.RemoveAll(v.l.Backups())
	_ = os.WriteFile(v.l.Backups(), []byte("not a dir"), 0o600)
	_, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{})
	if err == nil || !strings.Contains(err.Error(), "备份失败") || !strings.Contains(err.Error(), "已重新启动") {
		t.Fatalf("err = %v", err)
	}
	if v.cur() != "1.0.0" || !v.sys.isActive() || exists(v.l.ReleaseDir("1.1.0")) {
		t.Error("备份失败应中止并拉起旧版本，且不安装新 release")
	}
}

func TestUpgradeReusesExistingReleaseAfterVerify(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	pkg := v.makePkg("1.1.0", pkgOpt{})
	if _, err := v.e.installRelease(pkg, mustManifest(t, pkg), false); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Upgrade(ctxBG(), UpgradeOpts{Package: pkg}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	mustContain(t, "输出", v.dumpOut(), "复用")
	// 已存在但被改动 → 拒绝（并回滚）
	v2 := newEnv(t)
	v2.install("1.0.0", pkgOpt{})
	pkg2 := v2.makePkg("1.1.0", pkgOpt{})
	if _, err := v2.e.installRelease(pkg2, mustManifest(t, pkg2), false); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(v2.l.ReleaseDir("1.1.0"), "web", "app.js"), []byte("tampered"), 0o644)
	err := v2.e.Upgrade(ctxBG(), UpgradeOpts{Package: pkg2})
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("已存在但被改动的 release 应拒绝: %v", err)
	}
	if v2.cur() != "1.0.0" || !v2.sys.isActive() {
		t.Error("应回到旧版本并运行")
	}
}

func mustManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
