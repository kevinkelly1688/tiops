// @author: kevinkelly
// @license: Apache License 2.0
// @file: upgrade.go
// @time: 2026/10/28

package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UpgradeOpts 是 admin upgrade 的参数。
type UpgradeOpts struct {
	Package        string
	DryRun         bool
	Force          bool          // 忽略「有执行中变更」；允许重新应用同版本
	AllowDowngrade bool          // 允许版本降级
	NoBackup       bool          // 跳过数据备份（危险）
	Keep           int           // 保留最近几个 release，默认 3
	Timeout        time.Duration // 健康检查总超时，0 表示用引擎默认（60s）
}

var errSkipped = errors.New("跳过")

// preflight 逐项执行并打印预检；非 dry-run 遇到第一个失败即停止，dry-run 会跑完所有检查。
type preflight struct {
	e      *Engine
	dry    bool
	failed []string
}

func (p *preflight) do(name string, f func() (string, error)) bool {
	if len(p.failed) > 0 && !p.dry {
		return false
	}
	detail, err := f()
	switch {
	case errors.Is(err, errSkipped):
		p.e.printf("  - %s：跳过（依赖的检查未通过）\n", name)
		return false
	case err != nil:
		p.e.printf("  ✗ %s：%v\n", name, err)
		p.failed = append(p.failed, fmt.Sprintf("%s：%v", name, err))
		return false
	}
	if detail != "" {
		p.e.printf("  ✓ %s：%s\n", name, detail)
	} else {
		p.e.printf("  ✓ %s\n", name)
	}
	return true
}

func (p *preflight) err() error {
	if len(p.failed) == 0 {
		return nil
	}
	return fmt.Errorf("预检未通过（%d 项）：%s", len(p.failed), strings.Join(p.failed, "；"))
}

// runningChanges 返回数据目录 changes.json 里 state=="running" 的变更 ID。
func runningChanges(dataDir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "changes.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var p struct {
		Changes []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("changes.json 不是合法的 JSON: %w", err)
	}
	var ids []string
	for _, c := range p.Changes {
		if c.State == "running" {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// Upgrade 一键升级：预检 → 停服务 → 备份 → 装新 release → 更新单元 / sudoers → 迁移数据 → 切换 → 启动 → 健康检查；
// 失败自动回滚；最后写历史并清理旧版本与旧备份。
func (e *Engine) Upgrade(ctx context.Context, o UpgradeOpts) error {
	e.defaults()
	if o.Keep <= 0 {
		o.Keep = 3
	}
	if o.Timeout > 0 {
		e.HealthTimeout = o.Timeout
	}
	if o.Package == "" {
		return errors.New("未指定发布包目录（--package）")
	}
	if o.NoBackup {
		e.printf("警告：已指定 --no-backup，本次升级不做数据备份；若迁移改变了数据 schema，失败后将无法恢复数据！\n")
	}

	var (
		m         Manifest
		haveM     bool
		cur       string
		curSchema int
		haveCur   bool
		params    UnitParams
	)
	pf := &preflight{e: e, dry: o.DryRun}
	e.printf("预检：\n")
	pf.do("root 权限", func() (string, error) {
		if err := e.requireRoot(); err != nil {
			if o.DryRun {
				return "当前不是 root（dry-run 不受影响，真正升级需要 sudo）", nil
			}
			return "", err
		}
		return "", nil
	})
	pf.do("已安装", func() (string, error) {
		v, err := e.currentVersion()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", errors.New("尚未安装：首次部署请用 admin install")
			}
			return "", err
		}
		p, err := e.installedParams()
		if err != nil {
			return "", err
		}
		cur, params, haveCur = v, p, true
		return fmt.Sprintf("当前版本 %s（环境 %s，集群 %s，监听 %s）", v, p.Env, orDash(p.Cluster), p.Listen), nil
	})
	pf.do("发布包", func() (string, error) {
		mm, err := e.loadPackage(o.Package)
		if err != nil {
			return "", err
		}
		m, haveM = mm, true
		return fmt.Sprintf("tidbops %s（%s/%s，数据 schema %d），哈希校验通过", mm.Version, mm.OS, mm.Arch, mm.SchemaVersion), nil
	})
	pf.do("版本比较", func() (string, error) {
		if !haveCur || !haveM {
			return "", errSkipped
		}
		cv, _ := ParseVersion(cur)
		nv, _ := ParseVersion(m.Version)
		switch c := nv.Compare(cv); {
		case c < 0 && !o.AllowDowngrade:
			return "", fmt.Errorf("不允许降级（%s → %s）：回到旧版本请用 admin rollback；确需降级加 --allow-downgrade", cur, m.Version)
		case c < 0:
			return fmt.Sprintf("%s → %s（降级，已加 --allow-downgrade）", cur, m.Version), nil
		case c == 0 && !o.Force:
			return "", fmt.Errorf("已经是 %s，无需升级（要重新应用加 --force）", cur)
		case c == 0:
			return fmt.Sprintf("%s（--force 重新应用）", cur), nil
		}
		return fmt.Sprintf("%s → %s", cur, m.Version), nil
	})
	pf.do("minFrom", func() (string, error) {
		if !haveCur || !haveM {
			return "", errSkipped
		}
		if m.MinFrom == "" {
			return "不限", nil
		}
		mf, _ := ParseVersion(m.MinFrom)
		cv, _ := ParseVersion(cur)
		if cv.Compare(mf) < 0 {
			return "", fmt.Errorf("新版本要求从 %s 及以上直接升级，当前是 %s：请先升级到中间版本", m.MinFrom, cur)
		}
		return "当前版本满足 " + m.MinFrom, nil
	})
	pf.do("数据 schema", func() (string, error) {
		if !haveM {
			return "", errSkipped
		}
		s, err := e.currentSchema()
		if err != nil {
			return "", err
		}
		curSchema = s
		if m.SchemaVersion < s {
			return "", fmt.Errorf("发布包 schema %d 低于数据目录 schema %d：旧版本无法读取新数据，拒绝升级", m.SchemaVersion, s)
		}
		if m.SchemaVersion > s {
			return fmt.Sprintf("数据 schema %d → %d（将执行数据迁移%s）", s, m.SchemaVersion, ifStr(!o.NoBackup, "，已备份可恢复", "，且未备份！")), nil
		}
		return fmt.Sprintf("数据 schema %d，无需迁移", s), nil
	})
	pf.do("磁盘空间", func() (string, error) {
		if !haveM {
			return "", errSkipped
		}
		need := dirSize(e.L.Data()) + 16<<20
		pkg := dirSize(o.Package) + 16<<20
		var notes []string
		if !o.NoBackup {
			if free, err := e.Sys.FreeBytes(nearestDir(e.L.Backups())); err != nil {
				notes = append(notes, "无法查询备份目录空间，已跳过")
			} else if free < uint64(need) {
				return "", fmt.Errorf("备份目录可用空间 %s，备份约需 %s", humanBytes(int64(free)), humanBytes(need))
			}
		}
		if free, err := e.Sys.FreeBytes(nearestDir(e.L.Releases())); err != nil {
			notes = append(notes, "无法查询 releases 目录空间，已跳过")
		} else if free < uint64(pkg) {
			return "", fmt.Errorf("release 目录可用空间 %s，新版本约需 %s", humanBytes(int64(free)), humanBytes(pkg))
		}
		if len(notes) > 0 {
			return strings.Join(notes, "；"), nil
		}
		return "足够", nil
	})
	pf.do("执行中的变更", func() (string, error) {
		ids, err := runningChanges(e.L.Data())
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "没有", nil
		}
		if o.Force {
			return fmt.Sprintf("有执行中的变更 %s，已加 --force（该变更会被标记为中断）", strings.Join(ids, ", ")), nil
		}
		return "", fmt.Errorf("有正在执行的变更（%s）：请等待执行中的变更结束，或加 --force（强行升级会让该变更被标记为中断）", strings.Join(ids, ", "))
	})
	if err := pf.err(); err != nil {
		return err
	}

	e.printf("\n计划：升级 %s → %s\n", cur, m.Version)
	plan := []string{
		"停止服务 " + ServiceName + " 并等待退出",
		ifStr(o.NoBackup, "（--no-backup）不备份数据目录", "备份数据目录到 "+e.L.Backups()+"（tar.gz，0600）"),
		"安装新 release 到 " + e.L.ReleaseDir(m.Version),
		"若 systemd 单元 / sudoers 与新版本模板不同则更新（保留 --env / 集群名 / 监听地址等参数；sudoers 经 visudo -cf 校验）",
		"数据迁移：" + filepath.Join(e.L.ReleaseDir(m.Version), pkgBin) + " admin migrate --data-dir " + e.L.Data(),
		"原子切换 " + e.L.Current() + " → releases/" + m.Version,
		"systemctl daemon-reload && systemctl start " + ServiceName,
		fmt.Sprintf("健康检查：轮询 %s 直到版本为 %s（超时 %s）；失败则自动回滚到 %s", healthURL(params.Listen), m.Version, e.HealthTimeout, cur),
		"写升级历史；保留最近 " + fmt.Sprint(o.Keep) + " 个 release 与最近 10 个备份",
	}
	if params.UsesExec() { // 插在「更新单元 / sudoers」之后（第 4 项之后）
		plan = append(plan[:4], append([]string{"补齐执行器运行条件：执行器临时目录与 systemd drop-in（旧版安装器没建的才补，已有的保留）"}, plan[4:]...)...)
	}
	for i, s := range plan {
		e.printf("  %d. %s\n", i+1, s)
	}
	if o.DryRun {
		e.printf("\n[dry-run] 预检通过；以上步骤均未执行，系统未做任何改动\n")
		return nil
	}

	unlock, err := e.lock()
	if err != nil {
		return err
	}
	defer unlock()

	entry := HistoryEntry{Action: "upgrade", From: cur, To: m.Version, SchemaFrom: curSchema, SchemaTo: curSchema}
	fail := func(err error) error {
		entry.OK, entry.Error = false, err.Error()
		e.record(entry)
		return err
	}
	newRel := e.L.ReleaseDir(m.Version)

	e.printf("\n== 停止服务 ==\n")
	if err := e.stopService(ctx); err != nil {
		return fail(fmt.Errorf("停止服务失败，升级已中止（未做任何改动）：%w", err))
	}

	if !o.NoBackup {
		e.printf("== 备份数据目录 ==\n")
		bk, err := e.makeBackup(cur, "")
		if err != nil {
			// 服务已停：不能留着停机，原样拉起旧版本
			rb := e.rollbackTo(context.WithoutCancel(ctx), &rbPlan{target: cur, params: params})
			if rb != nil {
				return fail(fmt.Errorf("备份失败：%w；重新启动旧版本也失败：%v", err, rb))
			}
			return fail(fmt.Errorf("备份失败，升级已中止，旧版本 %s 已重新启动：%w", cur, err))
		}
		entry.Backup = bk
		e.printf("  备份：%s\n", bk)
	}

	mayMigrate := false
	stage := func() error {
		e.printf("== 安装新 release ==\n")
		reused, err := e.installRelease(o.Package, m, false)
		if err != nil {
			return err
		}
		if reused {
			e.printf("  %s 已存在且校验一致，复用\n", newRel)
		}
		e.printf("== 更新 systemd 单元 / sudoers ==\n")
		uc, sc, err := e.syncConfigs(ctx, newRel, params)
		if err != nil {
			return err
		}
		e.printf("  单元%s，sudoers%s\n", ifStr(uc, "已更新", "无变化"), ifStr(sc, "已更新", "无变化"))
		if params.UsesExec() { // 补齐旧版安装器没建的执行器临时目录 / 沙箱 drop-in；失败只警告，不因此中止升级
			e.printf("== 补齐执行器运行条件 ==\n")
			if err := e.ensureExecRuntime(params); err != nil {
				e.printf("  ⚠️ 补齐失败（不影响升级，可按 docs/运维部署手册.md 5.1 步骤 4 手工补）：%v\n", err)
			}
		}
		e.printf("== 数据迁移 ==\n")
		mayMigrate = m.SchemaVersion > curSchema
		out, err := e.Sys.Run(ctx, filepath.Join(newRel, filepath.FromSlash(pkgBin)), "admin", "migrate", "--data-dir", e.L.Data())
		if len(out) > 0 {
			e.printf("%s", indent(string(out)))
		}
		if err != nil {
			return fmt.Errorf("数据迁移失败: %w", err)
		}
		if err := e.Sys.Chown(e.L.Data(), RunUser, RunUser, true); err != nil {
			return err
		}
		after, err := e.currentSchema()
		if err != nil {
			return err
		}
		entry.SchemaTo = after
		if after != m.SchemaVersion {
			return fmt.Errorf("迁移后数据 schema 为 %d，与新版本期望的 %d 不一致", after, m.SchemaVersion)
		}
		e.printf("== 切换 current 链接 ==\n")
		if err := e.switchCurrent(m.Version); err != nil {
			return err
		}
		e.printf("== 启动服务并健康检查 ==\n")
		if err := e.startService(ctx); err != nil {
			return err
		}
		return e.waitHealthy(ctx, params.Listen, m.Version)
	}
	if err := stage(); err != nil {
		return e.autoRollback(ctx, &entry, params, err, mayMigrate)
	}

	entry.OK = true
	e.record(entry)
	e.printf("\n升级完成：%s → %s\n", cur, m.Version)
	if entry.Backup != "" {
		e.printf("升级前数据备份：%s（回滚 admin rollback 在 schema 变化时会用它恢复数据）\n", entry.Backup)
	}
	e.cleanup(o.Keep, m.Version, cur)
	return nil
}

// autoRollback 升级失败后回到旧版本：停服务 → （迁移过 schema 则恢复备份）→ 切回链接 → 启动 → 健康检查。
func (e *Engine) autoRollback(ctx context.Context, entry *HistoryEntry, params UnitParams, cause error, mayMigrate bool) error {
	ctx = context.WithoutCancel(ctx) // 即便用户按了 Ctrl-C 也要把旧版本拉起来
	e.printf("\n✖ 升级失败：%v\n正在自动回滚到 %s …\n", cause, entry.From)
	p := &rbPlan{target: entry.From, params: params, label: entry.To}
	if mayMigrate {
		if entry.Backup != "" {
			p.archive = entry.Backup
			e.printf("  数据迁移已开始，将恢复升级前备份 %s\n", entry.Backup)
		} else {
			e.printf("  警告：数据迁移已开始但没有备份（--no-backup），无法恢复数据；旧版本可能无法读取已迁移的数据\n")
		}
	}
	rbErr := e.rollbackTo(ctx, p)
	entry.OK, entry.Error = false, cause.Error()
	if rbErr != nil {
		entry.Error += "；自动回滚失败：" + rbErr.Error()
		e.record(*entry)
		return fmt.Errorf("升级到 %s 失败：%v；自动回滚到 %s 也失败：%v；现场已保留（升级前备份：%s），请人工处理", entry.To, cause, entry.From, rbErr, orDash(entry.Backup))
	}
	entry.RolledBack = true
	if p.archive != "" {
		entry.SchemaTo = entry.SchemaFrom
	}
	e.record(*entry)
	e.printf("已自动回滚到 %s，服务健康检查通过\n", entry.From)
	return fmt.Errorf("升级到 %s 失败：%v；已自动回滚到 %s", entry.To, cause, entry.From)
}

// cleanup 升级成功后清理：release 保留最近 keep 个（永不删除 current 与 previous），备份保留最近 10 个。
func (e *Engine) cleanup(keep int, current, previous string) {
	rels, err := e.listReleases()
	if err == nil {
		for i, v := range rels {
			if i < keep || sameVersion(v, current) || sameVersion(v, previous) {
				continue
			}
			if err := os.RemoveAll(e.L.ReleaseDir(v)); err == nil {
				e.printf("  清理旧版本 %s\n", v)
			}
		}
	}
	e.pruneBackups(10)
}

// ---- 小工具 ----

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ifStr(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ") + "\n"
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}

// nearestDir 返回 p 或其最近的已存在祖先目录（目录尚未创建时用来查磁盘空间）。
func nearestDir(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}
