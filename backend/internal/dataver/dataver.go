// @author: kevinkelly
// @license: Apache License 2.0
// @file: dataver.go
// @time: 2026/10/28

// Package dataver 管理数据目录（用户、审计、变更、设置…）的 schema 版本。
// 新版本二进制可以迁移旧数据；旧版本二进制拒绝读取更新版本创建的数据，避免静默损坏。
//
// 数据目录根下的 VERSION 文件（JSON）记录 {schema, version, installedAt, updatedAt}。
// 没有 VERSION 文件的数据目录（旧版本创建的）视为 schema 0。
package dataver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Schema 是当前二进制支持的数据 schema 版本。数据文件格式发生不兼容变化时加一，并在迁移表里加一步迁移。
const Schema = 1

// FileName 是数据目录里的版本文件名。
const FileName = "VERSION"

// Info 是 VERSION 文件的内容。
type Info struct {
	Schema      int        `json:"schema"`
	Version     string     `json:"version"` // 最近一次写入 / 迁移该数据的程序版本
	InstalledAt *time.Time `json:"installedAt,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// TooNewError 表示数据目录由更新版本的程序创建。
type TooNewError struct{ Data, Supported int }

func (e *TooNewError) Error() string {
	return fmt.Sprintf("数据目录由更新版本创建（schema %d，本程序支持到 %d）：请使用对应版本，或用 admin rollback / restore 从备份恢复", e.Data, e.Supported)
}

// Read 读取 VERSION；文件不存在时返回的错误满足 errors.Is(err, fs.ErrNotExist)。
func Read(dir string) (Info, error) {
	var in Info
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("%s 不是合法的 JSON: %w", FileName, err)
	}
	if in.Schema < 0 {
		return in, fmt.Errorf("%s 里的 schema 不合法: %d", FileName, in.Schema)
	}
	return in, nil
}

// Write 原子地写入 VERSION（0600）。UpdatedAt 为零值时取当前时间。
func Write(dir string, in Info) error {
	if in.UpdatedAt.IsZero() {
		in.UpdatedAt = time.Now()
	}
	in.UpdatedAt = in.UpdatedAt.UTC().Truncate(time.Second)
	if in.InstalledAt != nil {
		t := in.InstalledAt.UTC().Truncate(time.Second)
		in.InstalledAt = &t
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Check 返回数据目录的 schema：没有 VERSION 视为 0；比本程序支持的更新则返回 *TooNewError。只读，不改任何文件。
func Check(dir string) (schema int, err error) {
	in, err := Read(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if in.Schema > Schema {
		return in.Schema, &TooNewError{Data: in.Schema, Supported: Schema}
	}
	return in.Schema, nil
}

// Migration 是一步数据迁移：把 From 版本的数据升级为 To（= From+1）。
type Migration struct {
	From, To    int
	Desc        string
	Destructive bool // 会改写 / 删除已有业务文件；服务启动时不自动执行，必须经 admin upgrade / migrate（有备份）
	Fn          func(dir string) error
}

// Migrations 是迁移表。以后数据格式不兼容变化时在这里追加一步，并把 Schema 加一。
// 0 → 1：只创建 VERSION 文件（由框架在每步成功后写入），不改任何业务文件。
var Migrations = []Migration{
	{From: 0, To: 1, Desc: "创建 VERSION 文件", Fn: func(string) error { return nil }},
}

// Migrate 把数据目录迁移到 Schema。每步成功后立即写 VERSION；某步失败则停止并报告，已执行的步骤不回滚
// （所以升级流程会先备份）。幂等：已是最新则什么都不做。返回迁移前后的 schema。
// 数据 schema 大于本程序支持的返回 *TooNewError。
func Migrate(dir, version string, log func(string)) (from, to int, err error) {
	return migrate(dir, version, Migrations, Schema, log)
}

func migrate(dir, version string, table []Migration, target int, log func(string)) (from, to int, err error) {
	if log == nil {
		log = func(string) {}
	}
	cur, err := Read(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, 0, err
		}
		cur = Info{} // 没有 VERSION：旧数据，schema 0
	}
	from = cur.Schema
	if cur.Schema > target {
		return from, from, &TooNewError{Data: cur.Schema, Supported: target}
	}
	if cur.Schema == target {
		log(fmt.Sprintf("数据 schema 已是 %d，无需迁移", target))
		return from, from, nil
	}
	steps := append([]Migration(nil), table...)
	sort.Slice(steps, func(i, j int) bool { return steps[i].From < steps[j].From })
	s := cur.Schema
	for s < target {
		var m *Migration
		for i := range steps {
			if steps[i].From == s {
				m = &steps[i]
				break
			}
		}
		if m == nil || m.To <= s {
			return from, s, fmt.Errorf("迁移表缺少 schema %d 之后的步骤", s)
		}
		log(fmt.Sprintf("迁移 schema %d → %d：%s", m.From, m.To, m.Desc))
		if err := m.Fn(dir); err != nil {
			return from, s, fmt.Errorf("迁移 schema %d → %d 失败（已执行的步骤不会回滚，请从备份恢复）: %w", m.From, m.To, err)
		}
		cur.Schema, cur.Version, cur.UpdatedAt = m.To, version, time.Now()
		if err := Write(dir, cur); err != nil {
			return from, s, fmt.Errorf("写 %s 失败: %w", FileName, err)
		}
		s = m.To
	}
	return from, s, nil
}

// EnsureCurrent 供服务启动时调用（加载用户库之前）：
//   - 数据目录比本程序支持的新：返回 *TooNewError，拒绝启动；
//   - 空目录（没有任何文件）：直接写入当前 schema 的 VERSION；
//   - 没有 VERSION 的旧数据（schema 0）或更低 schema：自动执行非破坏性迁移；遇到破坏性迁移则拒绝并提示用 admin migrate；
//   - 已是最新：不动。
//
// 返回迁移前的 schema（未迁移时等于当前 schema）。
func EnsureCurrent(dir, version string) (from int, err error) {
	return ensure(dir, version, Migrations, Schema)
}

func ensure(dir, version string, table []Migration, target int) (int, error) {
	in, err := Read(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err != nil { // 没有 VERSION
		ents, rerr := os.ReadDir(dir)
		if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return 0, rerr
		}
		if len(ents) == 0 {
			now := time.Now()
			return target, Write(dir, Info{Schema: target, Version: version, InstalledAt: &now})
		}
		in = Info{} // 旧数据：schema 0
	}
	if in.Schema > target {
		return in.Schema, &TooNewError{Data: in.Schema, Supported: target}
	}
	if in.Schema == target {
		return target, nil
	}
	for _, m := range table {
		if m.From >= in.Schema && m.From < target && m.Destructive {
			return in.Schema, fmt.Errorf("数据 schema %d 需要破坏性迁移（%s），不能在启动时自动执行：请用 admin upgrade（带备份）或 admin migrate --data-dir %s", in.Schema, m.Desc, dir)
		}
	}
	f, _, err := migrate(dir, version, table, target, nil)
	return f, err
}
