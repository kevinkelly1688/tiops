// @author: kevinkelly
// @license: Apache License 2.0
// @file: rollback_test.go
// @time: 2026/10/28

package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollbackBinaryOnly(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{}); err != nil { // schema 不变
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	// 升级之后产生的新数据：只切二进制的回滚必须保留
	v.seedData("users-v1+new-after-upgrade")

	v.out.Reset()
	if err := v.e.Rollback(ctxBG(), RollbackOpts{}); err != nil {
		t.Fatalf("rollback: %v\n%s", err, v.dumpOut())
	}
	if v.cur() != "1.0.0" || !v.sys.isActive() {
		t.Errorf("current = %s", v.cur())
	}
	if got := v.readData("users.json"); got != "users-v1+new-after-upgrade" {
		t.Errorf("只切二进制时数据不应变: %q", got)
	}
	mustContain(t, "输出", v.dumpOut(), "不恢复，只切换二进制", "回滚完成：1.1.0 → 1.0.0")
	if v.countRestoreBackups() != 0 {
		t.Error("不恢复数据时不应做 pre-restore 备份")
	}
	h := v.history()
	last := h[len(h)-1]
	if last.Action != "rollback" || !last.OK || last.From != "1.1.0" || last.To != "1.0.0" {
		t.Errorf("历史 = %+v", last)
	}
	// 两个 release 都还在
	if !exists(v.l.ReleaseDir("1.1.0")) {
		t.Error("回滚不删除 release")
	}
	// 当前版本由回滚到达，再回滚没有「更低」的版本
	if err := v.e.Rollback(ctxBG(), RollbackOpts{}); err == nil || !strings.Contains(err.Error(), "没有可回滚") {
		t.Errorf("没有更低版本时应报错: %v", err)
	}
}

func TestRollbackRestoresDataWhenSchemaMigrated(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	if _, err := v.upgrade("1.1.0", pkgOpt{schema: 2}, UpgradeOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	if v.readData("users.json") != "users-v1|migrated-to-2" {
		t.Fatal("前置：应已迁移")
	}
	_ = os.WriteFile(filepath.Join(v.l.Data(), "audit.log"), []byte("audit-after-upgrade"), 0o600)

	v.out.Reset()
	if err := v.e.Rollback(ctxBG(), RollbackOpts{}); err != nil {
		t.Fatalf("rollback: %v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "输出", out, "恢复备份", "警告", "丢失升级之后产生的", "-pre-restore")
	if v.cur() != "1.0.0" || !v.sys.isActive() {
		t.Errorf("current = %s", v.cur())
	}
	if got := v.readData("users.json"); got != "users-v1" {
		t.Errorf("应恢复升级前的数据: %q", got)
	}
	if got := v.readData("audit.log"); got != "<missing>" {
		t.Errorf("升级后新增的文件应随恢复消失: %q", got)
	}
	if s, _ := v.e.currentSchema(); s != 1 {
		t.Errorf("schema = %d", s)
	}
	// 恢复前对当前数据另存了一份，且包含升级之后的数据
	var pre string
	bs, _ := v.e.listBackups()
	for _, b := range bs {
		if strings.Contains(b.Name, "-pre-restore") {
			pre = b.Path
		}
	}
	if pre == "" {
		t.Fatal("缺少 pre-restore 备份")
	}
	if got := readBackupFile(t, pre, "audit.log"); got != "audit-after-upgrade" {
		t.Errorf("pre-restore 备份应含升级后的数据: %q", got)
	}
	// 历史完整：install、upgrade、rollback
	var acts []string
	for _, h := range v.history() {
		acts = append(acts, h.Action)
	}
	if strings.Join(acts, ",") != "install,upgrade,rollback" {
		t.Errorf("历史动作 = %v", acts)
	}
	h := v.history()
	if last := h[len(h)-1]; !last.OK || last.SchemaFrom != 2 || last.SchemaTo != 1 || last.Backup != pre {
		t.Errorf("rollback 历史 = %+v", last)
	}
}

func TestRollbackFlags(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	if _, err := v.upgrade("1.1.0", pkgOpt{schema: 2}, UpgradeOpts{}); err != nil {
		t.Fatal(err)
	}
	// --no-restore-data：目标版本（schema 1）读不了 schema 2 的数据 → 拒绝
	err := v.e.Rollback(ctxBG(), RollbackOpts{NoRestoreData: true})
	if err == nil || !strings.Contains(err.Error(), "--restore-data") {
		t.Fatalf("应拒绝并提示 --restore-data: %v", err)
	}
	if v.cur() != "1.1.0" {
		t.Error("拒绝时不应改动")
	}
	if err := v.e.Rollback(ctxBG(), RollbackOpts{RestoreData: true, NoRestoreData: true}); err == nil {
		t.Error("两个互斥开关同时给应报错")
	}
	// dry-run
	v.sys.euid = 1000
	v.out.Reset()
	if err := v.e.Rollback(ctxBG(), RollbackOpts{DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	mustContain(t, "输出", v.dumpOut(), "回滚计划：1.1.0 → 1.0.0", "[dry-run]")
	if v.cur() != "1.1.0" {
		t.Error("dry-run 不应改动")
	}
	// 非 root
	if err := v.e.Rollback(ctxBG(), RollbackOpts{}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("非 root 应拒绝: %v", err)
	}
	v.sys.euid = 0
	// 指定不存在的版本
	if err := v.e.Rollback(ctxBG(), RollbackOpts{To: "0.1.0"}); err == nil {
		t.Error("不存在的目标版本应报错")
	}
	if err := v.e.Rollback(ctxBG(), RollbackOpts{To: "1.1.0"}); err == nil || !strings.Contains(err.Error(), "就是当前版本") {
		t.Errorf("目标=当前应报错: %v", err)
	}
	if err := v.e.Rollback(ctxBG(), RollbackOpts{To: "not-a-version"}); err == nil {
		t.Error("非法版本号应报错")
	}
}

func TestRollbackExplicitRestoreWithBackupFile(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{}); err != nil { // schema 不变，不会自动恢复
		t.Fatal(err)
	}
	v.seedData("users-after")
	if err := v.e.Rollback(ctxBG(), RollbackOpts{RestoreData: true, BackupFile: filepath.Join(v.root, "nope.tar.gz")}); err == nil {
		t.Fatal("备份文件不存在应报错")
	}
	if v.cur() != "1.1.0" {
		t.Fatal("报错时不应切换")
	}
	// 不指定 --backup：用历史里升级前备份
	if err := v.e.Rollback(ctxBG(), RollbackOpts{RestoreData: true}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	if v.readData("users.json") != "users-v1" || v.cur() != "1.0.0" {
		t.Errorf("--restore-data 应恢复升级前数据: %q %s", v.readData("users.json"), v.cur())
	}
}

func TestRollbackHealthFailureKeepsScene(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	if _, err := v.upgrade("1.1.0", pkgOpt{}, UpgradeOpts{}); err != nil {
		t.Fatal(err)
	}
	v.healthBad["1.0.0"] = true
	err := v.e.Rollback(ctxBG(), RollbackOpts{})
	if err == nil || !strings.Contains(err.Error(), "保留") {
		t.Fatalf("应报错并保留现场: %v", err)
	}
	// 不做二次自动操作：链接停在 1.0.0，不会再切回 1.1.0
	if v.cur() != "1.0.0" {
		t.Errorf("现场应保留（current 已是目标版本）: %s", v.cur())
	}
	h := v.history()
	if last := h[len(h)-1]; last.Action != "rollback" || last.OK {
		t.Errorf("历史 = %+v", last)
	}
	if exists(v.l.Lock()) {
		t.Error("锁文件应已删除")
	}
}

func TestRollbackPicksNextLowestWhenNoHistory(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	// 手工放入多个 release，不经 upgrade（历史里没有升级记录）
	for _, ver := range []string{"1.2.0", "1.5.0"} {
		pkg := v.makePkg(ver, pkgOpt{})
		if _, err := v.e.installRelease(pkg, mustManifest(t, pkg), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.e.switchCurrent("1.5.0"); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Rollback(ctxBG(), RollbackOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	if v.cur() != "1.2.0" {
		t.Errorf("应回到比当前低的最大版本 1.2.0，实际 %s", v.cur())
	}
}
