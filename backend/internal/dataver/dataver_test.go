// @author: kevinkelly
// @license: Apache License 2.0
// @file: dataver_test.go
// @time: 2026/10/28

package dataver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadWrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("没有 VERSION 应返回 ErrNotExist: %v", err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := Write(dir, Info{Schema: 3, Version: "1.2.3", InstalledAt: &now}); err != nil {
		t.Fatal(err)
	}
	in, err := Read(dir)
	if err != nil || in.Schema != 3 || in.Version != "1.2.3" || in.InstalledAt == nil || !in.InstalledAt.Equal(now) || in.UpdatedAt.IsZero() {
		t.Fatalf("Read = %+v %v", in, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, FileName))
	for _, k := range []string{`"schema"`, `"version"`, `"installedAt"`, `"updatedAt"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("VERSION 缺少字段 %s: %s", k, b)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+".tmp")); err == nil {
		t.Error("不应残留临时文件")
	}
	write(t, dir, FileName, "not json")
	if _, err := Read(dir); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("损坏的 VERSION 应报错: %v", err)
	}
	write(t, dir, FileName, `{"schema": -1}`)
	if _, err := Read(dir); err == nil {
		t.Error("负的 schema 应报错")
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	// 没有 VERSION → schema 0，无错误
	if s, err := Check(dir); s != 0 || err != nil {
		t.Errorf("无 VERSION: %d %v", s, err)
	}
	// 目录本身不存在也视为 0
	if s, err := Check(filepath.Join(dir, "nope")); s != 0 || err != nil {
		t.Errorf("目录不存在: %d %v", s, err)
	}
	// 当前 schema
	_ = Write(dir, Info{Schema: Schema, Version: "x"})
	if s, err := Check(dir); s != Schema || err != nil {
		t.Errorf("当前: %d %v", s, err)
	}
	// 更低 schema：不是错误（可迁移）
	_ = Write(dir, Info{Schema: 0, Version: "x"})
	if s, err := Check(dir); s != 0 || err != nil {
		t.Errorf("更低: %d %v", s, err)
	}
	// 更高 schema：拒绝，文案符合要求
	_ = Write(dir, Info{Schema: Schema + 1, Version: "9"})
	s, err := Check(dir)
	var tn *TooNewError
	if s != Schema+1 || !errors.As(err, &tn) {
		t.Fatalf("更高: %d %v", s, err)
	}
	want := fmt.Sprintf("数据目录由更新版本创建（schema %d，本程序支持到 %d）：请使用对应版本，或用 admin rollback / restore 从备份恢复", Schema+1, Schema)
	if err.Error() != want {
		t.Errorf("文案 = %q", err.Error())
	}
	// 损坏
	write(t, dir, FileName, "{")
	if _, err := Check(dir); err == nil {
		t.Error("损坏的 VERSION 应报错")
	}
}

func TestMigrateLegacyAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "users.json", "legacy-users")
	write(t, dir, "audit.log", "audit")
	var logs []string
	from, to, err := Migrate(dir, "1.0.0", func(s string) { logs = append(logs, s) })
	if err != nil || from != 0 || to != Schema {
		t.Fatalf("Migrate = %d→%d %v", from, to, err)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "0 → 1") {
		t.Errorf("日志 = %v", logs)
	}
	in, _ := Read(dir)
	if in.Schema != Schema || in.Version != "1.0.0" {
		t.Errorf("VERSION = %+v", in)
	}
	// 0→1 不改业务文件
	for f, c := range map[string]string{"users.json": "legacy-users", "audit.log": "audit"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); string(b) != c {
			t.Errorf("%s 被改动: %q", f, b)
		}
	}
	// 幂等
	before, _ := os.ReadFile(filepath.Join(dir, FileName))
	from, to, err = Migrate(dir, "9.9.9", nil)
	if err != nil || from != Schema || to != Schema {
		t.Fatalf("幂等: %d→%d %v", from, to, err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, FileName)); string(before) != string(after) {
		t.Error("已是最新时不应改写 VERSION")
	}
	// 数据 schema 更高
	_ = Write(dir, Info{Schema: Schema + 5})
	if _, _, err := Migrate(dir, "1.0.0", nil); err == nil || !strings.Contains(err.Error(), "数据目录由更新版本创建") {
		t.Errorf("schema 过高应报错: %v", err)
	}
}

func TestMigrateChainStopsOnFailure(t *testing.T) {
	var ran []string
	table := []Migration{
		{From: 0, To: 1, Desc: "a", Fn: func(d string) error { ran = append(ran, "0-1"); return nil }},
		{From: 1, To: 2, Desc: "b", Fn: func(d string) error { ran = append(ran, "1-2"); return nil }},
		{From: 2, To: 3, Desc: "c", Fn: func(d string) error { ran = append(ran, "2-3"); return errors.New("boom") }},
		{From: 3, To: 4, Desc: "d", Fn: func(d string) error { ran = append(ran, "3-4"); return nil }},
	}
	dir := t.TempDir()
	from, to, err := migrate(dir, "v", table, 4, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "不会回滚") {
		t.Fatalf("应在失败步骤停止并报告: %v", err)
	}
	if from != 0 || to != 2 {
		t.Errorf("from/to = %d/%d，已执行成功的步骤应已落盘", from, to)
	}
	if strings.Join(ran, ",") != "0-1,1-2,2-3" {
		t.Errorf("执行序列 = %v（失败后不应继续）", ran)
	}
	if in, _ := Read(dir); in.Schema != 2 {
		t.Errorf("VERSION 应停在最后成功的步骤: %+v", in)
	}
	// 修好后重跑：从 2 继续（已执行的步骤不重复）
	ran = nil
	table[2].Fn = func(d string) error { ran = append(ran, "2-3"); return nil }
	if _, to, err := migrate(dir, "v", table, 4, nil); err != nil || to != 4 {
		t.Fatalf("重跑: %d %v", to, err)
	}
	if strings.Join(ran, ",") != "2-3,3-4" {
		t.Errorf("重跑序列 = %v", ran)
	}
	// 迁移表缺一步
	if _, _, err := migrate(t.TempDir(), "v", table[:1], 3, nil); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Errorf("缺步骤应报错: %v", err)
	}
}

func TestEnsureCurrent(t *testing.T) {
	// 空目录：直接写当前 schema
	dir := t.TempDir()
	from, err := EnsureCurrent(dir, "1.0.0")
	if err != nil || from != Schema {
		t.Fatalf("空目录: %d %v", from, err)
	}
	in, _ := Read(dir)
	if in.Schema != Schema || in.Version != "1.0.0" || in.InstalledAt == nil {
		t.Errorf("VERSION = %+v", in)
	}
	// 已是最新：不动
	before, _ := os.ReadFile(filepath.Join(dir, FileName))
	if f, err := EnsureCurrent(dir, "2.0.0"); err != nil || f != Schema {
		t.Fatalf("最新: %d %v", f, err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, FileName)); string(before) != string(after) {
		t.Error("已是最新不应改写")
	}
	// 旧数据（有文件无 VERSION）：自动非破坏性迁移
	legacy := t.TempDir()
	write(t, legacy, "users.json", "legacy")
	from, err = EnsureCurrent(legacy, "1.0.0")
	if err != nil || from != 0 {
		t.Fatalf("旧数据: %d %v", from, err)
	}
	if in, _ := Read(legacy); in.Schema != Schema {
		t.Errorf("旧数据应已迁移: %+v", in)
	}
	if b, _ := os.ReadFile(filepath.Join(legacy, "users.json")); string(b) != "legacy" {
		t.Error("不应改业务文件")
	}
	// 更新版本创建的数据：拒绝，且不改动
	newer := t.TempDir()
	write(t, newer, "users.json", "x")
	_ = Write(newer, Info{Schema: Schema + 2, Version: "9.0.0"})
	b0, _ := os.ReadFile(filepath.Join(newer, FileName))
	_, err = EnsureCurrent(newer, "1.0.0")
	var tn *TooNewError
	if !errors.As(err, &tn) || !strings.Contains(err.Error(), "请使用对应版本，或用 admin rollback / restore 从备份恢复") {
		t.Fatalf("应拒绝启动: %v", err)
	}
	if b1, _ := os.ReadFile(filepath.Join(newer, FileName)); string(b0) != string(b1) {
		t.Error("拒绝时不应改动 VERSION")
	}
	// 损坏的 VERSION：报错而不是当成 0 覆盖
	bad := t.TempDir()
	write(t, bad, FileName, "garbage")
	if _, err := EnsureCurrent(bad, "1.0.0"); err == nil {
		t.Error("损坏的 VERSION 应拒绝")
	}
}

func TestEnsureRefusesDestructiveMigration(t *testing.T) {
	table := []Migration{
		{From: 0, To: 1, Desc: "safe", Fn: func(string) error { return nil }},
		{From: 1, To: 2, Desc: "rewrite users.json", Destructive: true, Fn: func(string) error { t.Error("不应被执行"); return nil }},
	}
	dir := t.TempDir()
	write(t, dir, "users.json", "x")
	_ = Write(dir, Info{Schema: 1, Version: "1"})
	_, err := ensure(dir, "2.0.0", table, 2)
	if err == nil || !strings.Contains(err.Error(), "破坏性") || !strings.Contains(err.Error(), "admin migrate") {
		t.Fatalf("破坏性迁移不能在启动时自动执行: %v", err)
	}
	// 同一张表里，从 0 出发也要拦住（链条上有破坏性步骤）
	dir2 := t.TempDir()
	write(t, dir2, "users.json", "x")
	if _, err := ensure(dir2, "2.0.0", table, 2); err == nil {
		t.Fatal("链条上有破坏性步骤应拒绝")
	}
	if _, e := Read(dir2); !errors.Is(e, fs.ErrNotExist) {
		t.Error("拒绝时不应先执行前面的安全步骤")
	}
}
