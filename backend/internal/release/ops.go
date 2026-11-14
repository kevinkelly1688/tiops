// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops.go
// @time: 2026/10/28

package release

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Backup 手动备份数据目录（0600）。out 为空则写到备份目录下 <时间>-<版本>-manual.tar.gz。
// 服务运行中也可备份（变更 / 用户文件都是原子写入）；要得到严格一致的快照请先停服务。
func (e *Engine) Backup(ctx context.Context, out string) error {
	e.defaults()
	if err := e.requireRoot(); err != nil {
		return err
	}
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ver, _ := e.currentVersion()
	if ver == "" {
		ver = "unknown"
	}
	var dst string
	if out == "" {
		dst, err = e.makeBackup(ver, "-manual")
	} else {
		abs, aerr := filepath.Abs(out)
		if aerr != nil {
			return aerr
		}
		if within(e.L.Data(), abs) {
			return fmt.Errorf("备份文件不能放在数据目录 %s 里面", e.L.Data())
		}
		dst, err = e.backupTo(abs)
	}
	if err != nil {
		return err
	}
	e.printf("备份完成：%s\n", dst)
	return nil
}

// within 判断 p 是否在 dir 之内。
func within(dir, p string) bool {
	d, err1 := filepath.Abs(dir)
	q, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(d, q)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Restore 从备份恢复数据目录：服务运行则先停止；恢复前备份当前数据；归档里有路径穿越 / 绝对路径 / 符号链接则整体拒绝。
func (e *Engine) Restore(ctx context.Context, file string) error {
	e.defaults()
	if file == "" {
		return errors.New("需要 --file 指定备份文件")
	}
	if err := e.requireRoot(); err != nil {
		return err
	}
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("备份文件不可用: %w", err)
	}
	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()

	// 先在暂存目录里完整校验归档（不碰数据目录与服务）
	if err := os.MkdirAll(filepath.Dir(e.L.Data()), 0o755); err != nil {
		return err
	}
	chk, err := os.MkdirTemp(filepath.Dir(e.L.Data()), ".tidbops-restore-check-")
	if err != nil {
		return err
	}
	cerr := extractTarGz(file, chk)
	_ = os.RemoveAll(chk)
	if cerr != nil {
		return fmt.Errorf("备份文件校验失败，未做任何改动: %w", cerr)
	}

	ver, _ := e.currentVersion()
	if ver == "" {
		ver = "unknown"
	}
	wasActive, _ := e.serviceActive(ctx)
	if wasActive {
		e.printf("== 停止服务 ==\n")
		if err := e.stopService(ctx); err != nil {
			return err
		}
	}
	e.printf("== 备份当前数据 ==\n")
	pre, err := e.makeBackup(ver, "-pre-restore")
	if err != nil {
		return fmt.Errorf("恢复前备份当前数据失败（未恢复）: %w", err)
	}
	e.printf("  %s\n", pre)
	e.printf("== 恢复数据 ==\n")
	if err := e.restoreData(file); err != nil {
		return err
	}
	e.printf("  已从 %s 恢复；⚠ 备份之后产生的审计 / 变更 / 用户等数据已被覆盖（恢复前快照：%s）\n", file, pre)

	schema, _ := e.currentSchema()
	if ver != "unknown" {
		if m, merr := e.releaseManifest(ver); merr == nil && schema > m.SchemaVersion {
			e.printf("警告：恢复的数据 schema 为 %d，高于当前版本 %s 支持的 %d，服务将拒绝启动；请先 admin rollback 到对应版本。服务保持停止。\n", schema, ver, m.SchemaVersion)
			return nil
		}
	}
	if wasActive {
		e.printf("== 启动服务 ==\n")
		p, perr := e.installedParams()
		if perr != nil {
			return perr
		}
		if err := e.startService(ctx); err != nil {
			return err
		}
		if err := e.waitHealthy(ctx, p.Listen, ver); err != nil {
			return err
		}
	}
	e.printf("恢复完成\n")
	return nil
}

// UninstallOpts 是 admin uninstall 的参数。
type UninstallOpts struct {
	PurgeData bool
	Yes       bool
	DryRun    bool
}

// Uninstall 停止并禁用服务，删除 unit / sudoers / current 链接；默认保留数据目录、备份与 releases。
func (e *Engine) Uninstall(ctx context.Context, o UninstallOpts) error {
	e.defaults()
	if o.PurgeData && !o.Yes && !o.DryRun {
		return errors.New("--purge-data 会永久删除数据目录与全部备份，需要再加 --yes 确认")
	}
	if !o.DryRun {
		if err := e.requireRoot(); err != nil {
			return err
		}
		unlock, err := e.lock()
		if err != nil {
			return err
		}
		defer unlock()
	}
	steps := []step{
		{"停止并禁用服务", func() error {
			if _, err := e.Sys.Run(ctx, "systemctl", "disable", "--now", ServiceName); err != nil {
				e.printf("  警告：%v（服务可能未安装 / 未运行，继续）\n", err)
			}
			return nil
		}},
		{"删除 systemd 单元 " + e.L.Unit(), func() error { return removeIfExists(e.L.Unit()) }},
		{"删除 sudoers " + e.L.Sudoers(), func() error { return removeIfExists(e.L.Sudoers()) }},
		{"删除 systemd drop-in " + e.L.DropIn(), func() error { return removeIfExists(e.L.DropIn()) }},
		{"删除 current 链接 " + e.L.Current(), func() error { return removeIfExists(e.L.Current()) }},
		{"systemctl daemon-reload", func() error {
			if _, err := e.Sys.Run(ctx, "systemctl", "daemon-reload"); err != nil {
				e.printf("  警告：%v\n", err)
			}
			return nil
		}},
	}
	if o.PurgeData {
		steps = append(steps, step{"永久删除数据目录与备份（--purge-data --yes）", func() error {
			if err := os.RemoveAll(e.L.Data()); err != nil {
				return err
			}
			return os.RemoveAll(e.L.Backups())
		}})
	}
	if err := e.runSteps(o.DryRun, steps); err != nil {
		return err
	}
	if o.DryRun {
		e.printf("[dry-run] 未执行任何改动\n")
		return nil
	}
	e.printf("\n已卸载。保留：releases（%s）", e.L.Releases())
	if !o.PurgeData {
		e.printf("、数据目录（%s）、备份（%s）", e.L.Data(), e.L.Backups())
	}
	e.printf("、配置与密钥（%s）、账号 %s。\n", e.L.Etc(), RunUser)
	return nil
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
