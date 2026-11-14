// @author: kevinkelly
// @license: Apache License 2.0
// @file: rollback.go
// @time: 2026/10/28

package release

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// rbPlan 是回滚的内部计划，升级失败的自动回滚与 admin rollback 共用。
type rbPlan struct {
	target  string     // 回到的版本
	archive string     // 非空：先恢复该备份
	params  UnitParams // 单元参数（保留现有）
	label   string     // 恢复前安全备份的文件名标签（被回滚掉的版本）
	preBak  string     // 输出：恢复前对当前数据做的安全备份
}

// rollbackTo 执行：停服务 →（可选）恢复数据 → 用目标版本的模板同步单元 / sudoers → 切换链接 → 启动 → 健康检查（版本等于目标）。
// 任何一步失败即返回错误，不做二次自动操作。
func (e *Engine) rollbackTo(ctx context.Context, p *rbPlan) error {
	e.printf("== 停止服务 ==\n")
	if err := e.stopService(ctx); err != nil {
		return err
	}
	if p.archive != "" {
		e.printf("== 恢复数据 ==\n")
		label := p.label
		if label == "" {
			label = "unknown"
		}
		bk, err := e.makeBackup(label, "-pre-restore")
		if err != nil {
			return fmt.Errorf("恢复前备份当前数据失败（未恢复）: %w", err)
		}
		p.preBak = bk
		e.printf("  恢复前已备份当前数据：%s\n", bk)
		if err := e.restoreData(p.archive); err != nil {
			return err
		}
		e.printf("  已从 %s 恢复数据目录\n", p.archive)
	}
	e.printf("== 切换到 %s ==\n", p.target)
	rel := e.L.ReleaseDir(p.target)
	if _, _, err := e.syncConfigs(ctx, rel, p.params); err != nil {
		return fmt.Errorf("按 %s 的模板还原 systemd 单元 / sudoers 失败: %w", p.target, err)
	}
	if err := e.switchCurrent(p.target); err != nil {
		return err
	}
	e.printf("== 启动服务并健康检查 ==\n")
	if err := e.startService(ctx); err != nil {
		return err
	}
	return e.waitHealthy(ctx, p.params.Listen, p.target)
}

// RollbackOpts 是 admin rollback 的参数。
type RollbackOpts struct {
	To            string // 目标版本，默认上一个版本
	RestoreData   bool   // 强制恢复升级前备份
	NoRestoreData bool   // 强制只切换二进制
	BackupFile    string // 指定要恢复的备份文件
	DryRun        bool
}

// Rollback 一键回滚到上一个版本（或 --to 指定的版本）。
func (e *Engine) Rollback(ctx context.Context, o RollbackOpts) error {
	e.defaults()
	if o.RestoreData && o.NoRestoreData {
		return errors.New("--restore-data 与 --no-restore-data 不能同时使用")
	}
	if !o.DryRun {
		if err := e.requireRoot(); err != nil {
			return err
		}
	}
	cur, err := e.currentVersion()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("尚未安装，无可回滚的版本")
		}
		return err
	}
	params, err := e.installedParams()
	if err != nil {
		return err
	}
	hist, _ := readHistory(e.L.History())
	var up *HistoryEntry // 把当前版本升上来的那次成功升级
	for i := len(hist) - 1; i >= 0; i-- {
		if h := hist[i]; h.Action == "upgrade" && h.OK && !h.RolledBack && sameVersion(h.To, cur) {
			up = &hist[i]
			break
		}
	}

	target := o.To
	switch {
	case target != "":
		v, err := ParseVersion(target)
		if err != nil {
			return err
		}
		target = v.String()
	case up != nil:
		target = up.From
	default:
		rels, err := e.listReleases()
		if err != nil {
			return err
		}
		cv, _ := ParseVersion(cur)
		for _, r := range rels { // 从大到小：第一个比当前小的就是「次大」
			if rv, _ := ParseVersion(r); rv.Compare(cv) < 0 {
				target = r
				break
			}
		}
		if target == "" {
			return fmt.Errorf("没有可回滚的上一个版本（releases 里没有比 %s 更低的版本）；可用 --to 指定", cur)
		}
	}
	if sameVersion(target, cur) {
		return fmt.Errorf("目标版本 %s 就是当前版本", target)
	}
	tm, err := e.releaseManifest(target)
	if err != nil {
		return fmt.Errorf("目标版本 %s 不在 %s 下或清单损坏: %w", target, e.L.Releases(), err)
	}
	if err := VerifyDir(e.L.ReleaseDir(target), tm); err != nil {
		return fmt.Errorf("目标 release %s 校验失败: %w", target, err)
	}

	dataSchema, err := e.currentSchema()
	if err != nil {
		return err
	}
	upMatches := up != nil && sameVersion(up.From, target)
	restore, reason := false, ""
	switch {
	case o.NoRestoreData:
	case o.RestoreData:
		restore, reason = true, "指定了 --restore-data"
	case upMatches && up.SchemaTo > up.SchemaFrom:
		restore, reason = true, fmt.Sprintf("上次升级把数据 schema 从 %d 迁移到 %d", up.SchemaFrom, up.SchemaTo)
	}
	if !restore && dataSchema > tm.SchemaVersion {
		return fmt.Errorf("目标版本 %s 只支持数据 schema ≤ %d，而当前数据是 schema %d：不恢复备份的话它无法启动；请加 --restore-data（用 --backup 指定备份）", target, tm.SchemaVersion, dataSchema)
	}
	archive := ""
	if restore {
		switch {
		case o.BackupFile != "":
			archive = o.BackupFile
		case upMatches && up.Backup != "":
			archive = up.Backup
		default:
			bs, _ := e.listBackups()
			for _, b := range bs {
				if strings.Contains(b.Name, "-"+target+".tar.gz") || strings.Contains(b.Name, "-"+target+"-") {
					if !strings.Contains(b.Name, "-pre-restore") {
						archive = b.Path
						break
					}
				}
			}
		}
		if archive == "" {
			return fmt.Errorf("找不到版本 %s 的升级前备份：请用 --backup 指定备份文件", target)
		}
		if _, err := os.Stat(archive); err != nil {
			return fmt.Errorf("备份文件不可用（可能已被清理）：%v；可用 --backup 指定其他备份", err)
		}
	}

	e.printf("回滚计划：%s → %s\n", cur, target)
	if restore {
		e.printf("  数据：恢复备份 %s（%s）\n", archive, reason)
		e.printf("  ⚠ 警告：恢复会丢失升级之后产生的审计 / 变更 / 用户等数据变更；恢复前会先把当前数据另存一份（-pre-restore）\n")
	} else {
		e.printf("  数据：不恢复，只切换二进制（数据 schema %d，目标版本支持到 %d）\n", dataSchema, tm.SchemaVersion)
	}
	if o.DryRun {
		e.printf("[dry-run] 未执行任何改动\n")
		return nil
	}

	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()

	entry := HistoryEntry{Action: "rollback", From: cur, To: target, SchemaFrom: dataSchema, SchemaTo: dataSchema}
	p := &rbPlan{target: target, archive: archive, params: params, label: cur}
	if err := e.rollbackTo(ctx, p); err != nil {
		entry.Error, entry.Backup = err.Error(), p.preBak
		e.record(entry)
		return fmt.Errorf("回滚失败，现场已保留（未做二次操作）：%w", err)
	}
	if after, err := e.currentSchema(); err == nil {
		entry.SchemaTo = after
	}
	entry.OK, entry.Backup = true, p.preBak
	e.record(entry)
	e.printf("\n回滚完成：%s → %s\n", cur, target)
	if p.preBak != "" {
		e.printf("恢复前的数据快照：%s\n", p.preBak)
	}
	return nil
}
