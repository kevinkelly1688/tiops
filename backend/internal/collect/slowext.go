// @author: kevinkelly
// @license: Apache License 2.0
// @file: slowext.go
// @time: 2026/10/28

package collect

// 慢 SQL 深化（诊断第二批）：在原有慢日志采集之外，再取一份「最近 2 小时按 (Digest, Plan_digest) 聚合」的数据（sql/slow-ext），
// 用来判断官方「慢查询日志」「SQL 突然变慢」文档里的几类情形：执行计划突变、同一计划变慢、统计信息缺失（Stats=pseudo）、
// 锁冲突 / TiKV 繁忙 / Region 调度（Backoff_types）、MVCC 旧版本与 tombstone（Total_keys ≫ Process_keys、Rocksdb_delete_skipped_count）。
// 对比的是「慢日志里的样本」：只有超过慢日志阈值（默认 300ms）的执行才会进入，所以「上一小时没有记录」不等于「上一小时没执行过」。

import (
	"context"
	"fmt"
	"sort"

	"tidbops/internal/model"
)

const (
	extMinSamples   = 5   // 平均耗时对比至少需要的样本数（两个窗口各自）
	extSlowerRatio  = 2.0 // 同一计划耗时变为上一窗口的多少倍算「明显变慢」
	extPlanSlower   = 1.5 // 计划变化后耗时变为多少倍，把计划突变从提示升为警告
	extMinAvgSec    = 0.5 // 耗时跳变至少要达到的平均秒数（避免 0.05s → 0.12s 这类无意义的倍数）
	extPseudoShare  = 0.5 // 当前窗口里统计信息为 pseudo 的样本占比
	extLockShare    = 0.3 // 当前窗口里出现 txnLock 退避的样本占比
	extBusyShare    = 0.3 // 当前窗口里出现 tikvServerBusy 退避的样本占比
	extMissShare    = 0.5 // 当前窗口里出现 regionMiss 退避的样本占比
	extMVCCRatio    = 10.0
	extMVCCMinKeys  = 1e5
	extTombstoneMin = 10000.0 // 单条慢查询跳过的 tombstone（Rocksdb_delete_skipped_count）个数
	extMaxFindings  = 3       // 每一类最多给几条诊断项
)

type planAgg struct{ cur, prev int }

// slowAgg 是一个 Digest 的聚合（各计划汇总）。cur 指最近 1 小时，prev 指再早的那 1 小时。
type slowAgg struct {
	plans                                   map[string]*planAgg
	cur, prev                               int
	curSum, prevSum                         float64
	pseudo, lock, regionMiss, busy, pd      int
	maxResolveLock, maxDelSkipped, maxRetry float64
}

func (a *slowAgg) share(n int) float64 {
	if a.cur == 0 {
		return 0
	}
	return float64(n) / float64(a.cur)
}

func (a *slowAgg) avgCur() float64 {
	if a.cur == 0 {
		return 0
	}
	return a.curSum / float64(a.cur)
}

func (a *slowAgg) avgPrev() float64 {
	if a.prev == 0 {
		return 0
	}
	return a.prevSum / float64(a.prev)
}

// planChange 判断执行计划是否发生变化：上一窗口有样本，而当前窗口出现了上一窗口没有的计划。
// 返回「旧计划」（上一窗口样本最多的计划）与「新计划」（当前窗口样本最多的新增计划）。
func (a *slowAgg) planChange() (oldP, newP string, changed bool) {
	if a.prev == 0 {
		return
	}
	best := func(pick func(*planAgg) int, filter func(*planAgg) bool) string {
		var ids []string
		for id, p := range a.plans {
			if id != "" && filter(p) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		bestID, bestN := "", -1
		for _, id := range ids {
			if n := pick(a.plans[id]); n > bestN {
				bestID, bestN = id, n
			}
		}
		return bestID
	}
	newP = best(func(p *planAgg) int { return p.cur }, func(p *planAgg) bool { return p.cur > 0 && p.prev == 0 })
	if newP == "" {
		return
	}
	oldP = best(func(p *planAgg) int { return p.prev }, func(p *planAgg) bool { return p.prev > 0 })
	return oldP, newP, true
}

// SlowExt 取并解析 sql/slow-ext；取不到（版本缺列、无权限……）返回 nil，不影响其余慢查询分析。
func (c *Collector) SlowExt(ctx context.Context) map[string]*slowAgg {
	b, err := c.get(ctx, "sql/slow-ext")
	if err != nil {
		return nil
	}
	return parseSlowExt(b)
}

func parseSlowExt(b []byte) map[string]*slowAgg {
	_, rows := parseTSV(b)
	out := map[string]*slowAgg{}
	for _, r := range rows {
		d, p := r["digest"], r["plan_digest"]
		if d == "" {
			continue
		}
		a := out[d]
		if a == nil {
			a = &slowAgg{plans: map[string]*planAgg{}}
			out[d] = a
		}
		cnt, cur := atoi(r["cnt"]), atoi(r["cur_cnt"])
		pa := a.plans[p]
		if pa == nil {
			pa = &planAgg{}
			a.plans[p] = pa
		}
		pa.cur += cur
		pa.prev += cnt - cur
		a.cur += cur
		a.prev += cnt - cur
		a.curSum += f64(r["cur_sum"])
		a.prevSum += f64(r["prev_sum"])
		a.pseudo += atoi(r["pseudo_cnt"])
		a.lock += atoi(r["lock_cnt"])
		a.regionMiss += atoi(r["region_miss_cnt"])
		a.busy += atoi(r["busy_cnt"])
		a.pd += atoi(r["pd_cnt"])
		a.maxResolveLock = maxF(a.maxResolveLock, f64(r["max_resolve_lock"]))
		a.maxDelSkipped = maxF(a.maxDelSkipped, f64(r["max_del_skipped"]))
		a.maxRetry = maxF(a.maxRetry, f64(r["max_retry"]))
	}
	return out
}

// applyExt 用扩展数据补充 SlowGroup 的标签与建议（SQL 优化页显示）。只在原来没有更具体标签（「耗时较长」）时替换标签；
// 锁冲突证据比「TiKV 排队等待」更具体，所以也可以替换后者。
func applyExt(g *model.SlowGroup, a *slowAgg) {
	if a == nil || a.cur == 0 {
		return
	}
	var tips []model.Tip
	retag := func(tag string, overrideQueue bool) {
		if g.Tag == "耗时较长" || (overrideQueue && g.Tag == TagKVQueue) {
			g.Tag = tag
		}
	}
	if a.share(a.lock) >= extLockShare {
		retag("锁冲突", true)
		tips = append(tips, lockTip(a))
	}
	if oldP, newP, ok := a.planChange(); ok {
		retag("执行计划突变", false)
		tips = append(tips, planTip(g, a, oldP, newP))
	}
	if a.share(a.pseudo) >= extPseudoShare {
		retag("统计信息缺失", false)
		tips = append(tips, pseudoTip(g))
	}
	if a.share(a.busy) >= extBusyShare {
		retag("TiKV 繁忙", false)
		tips = append(tips, busyTip(a))
	}
	if ratio := mvccRatio(g); ratio >= extMVCCRatio && g.Keys >= extMVCCMinKeys && g.Tag != "缺索引 / 统计信息过期" {
		retag("MVCC 旧版本多", false)
		tips = append(tips, mvccTip(g, a, ratio))
	}
	g.Tips = append(tips, g.Tips...)
}

func mvccRatio(g *model.SlowGroup) float64 {
	if g.PKeys <= 0 {
		return 0
	}
	return g.Keys / g.PKeys
}

func lockTip(a *slowAgg) model.Tip {
	return model.Tip{T: fmt.Sprintf("最近 1 小时 %d 条慢日志里有 %.0f%% 出现 txnLock 退避（Resolve_lock_time 最长 %.2fs）：这是锁冲突的直接证据，比 Wait_time 可靠。用下面的语句看谁在等锁、谁持锁（仅覆盖悲观锁，只反映当前时刻）",
		a.cur, a.share(a.lock)*100, a.maxResolveLock), Cmd: "SELECT * FROM information_schema.DATA_LOCK_WAITS;\nSELECT * FROM information_schema.DEADLOCKS;"}
}

func planTip(g *model.SlowGroup, a *slowAgg, oldP, newP string) model.Tip {
	t := fmt.Sprintf("执行计划相比上一小时发生了变化（新计划 %s，上一小时是 %s）", shortID(newP), shortID(oldP))
	if a.avgPrev() > 0 && a.avgCur() > 0 {
		t += fmt.Sprintf("；慢日志样本平均耗时 %.2fs → %.2fs", a.avgPrev(), a.avgCur())
	}
	t += "。先对比两个计划，常见原因是统计信息变化（自动 ANALYZE 之后）或数据分布变化；确认旧计划更好时，可从历史语句绑定旧计划来止血"
	cmd := "-- 查看该 SQL 近期用过的执行计划\nSELECT PLAN_DIGEST, EXEC_COUNT, AVG_LATENCY, LAST_SEEN FROM information_schema.cluster_statements_summary WHERE DIGEST = '" + g.Digest + "' LIMIT 10;"
	if oldP != "" {
		cmd += "\n-- 绑定旧计划（v6.6.0 起支持；绑定前先用 EXPLAIN ANALYZE 验证旧计划确实更快）\nCREATE GLOBAL BINDING FROM HISTORY USING PLAN DIGEST '" + oldP + "';"
	}
	return model.Tip{T: t, Cmd: cmd}
}

func pseudoTip(g *model.SlowGroup) model.Tip {
	tbl := tableOf(g.SQL)
	return model.Tip{T: "慢日志里这条语句的统计信息是 pseudo（表没有统计信息或严重过期），优化器只能按经验估算行数，容易选错计划。先收集统计信息，再观察",
		Cmd: fmt.Sprintf("SHOW STATS_HEALTHY WHERE Table_name = '%s';\nANALYZE TABLE %s;", tbl, tbl)}
}

func busyTip(a *slowAgg) model.Tip {
	return model.Tip{T: fmt.Sprintf("最近 1 小时 %.0f%% 的慢日志出现 tikvServerBusy 退避：TiKV 处理不过来（线程池满、write stall、磁盘 I/O 高等）。看 TiKV 的 CPU / 线程池 / 磁盘延迟，再决定是限流还是扩容", a.share(a.busy)*100)}
}

func mvccTip(g *model.SlowGroup, a *slowAgg, ratio float64) model.Tip {
	t := fmt.Sprintf("平均扫描 %.0f 个 Key 只处理 %.0f 个（%.0f 倍）：多出来的是 MVCC 旧版本或已删除的数据（官方：Total_keys 远大于 Process_keys 说明旧版本多）", g.Keys, g.PKeys, ratio)
	if a.maxDelSkipped >= extTombstoneMin {
		t += fmt.Sprintf("；单次最多跳过 %.0f 个 tombstone，删除后留下的标记没有被 compaction 清掉", a.maxDelSkipped)
	}
	t += "。确认 GC 在正常推进、没有长事务卡住 safe point"
	return model.Tip{T: t, Cmd: "SELECT * FROM mysql.tidb WHERE variable_name IN ('tikv_gc_safe_point', 'tikv_gc_last_run_time', 'tikv_gc_life_time');\nSELECT * FROM information_schema.CLUSTER_TIDB_TRX ORDER BY start_time LIMIT 10;"}
}

func shortID(s string) string {
	if s == "" {
		return "（无记录）"
	}
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// DiagnoseSlowExt 把慢日志的扩展聚合变成诊断项。slow 提供 SQL 文本与库名（扩展数据里没有 SQL 文本），
// 所以只覆盖 slow 里有的 Digest；每类最多 extMaxFindings 条，按 slow 的顺序（累计耗时从大到小）取。
func DiagnoseSlowExt(slow []model.SlowGroup, ext map[string]*slowAgg) []model.Finding {
	if len(ext) == 0 {
		return nil
	}
	var out []model.Finding
	count := map[string]int{}
	add := func(kind string, f model.Finding) {
		if count[kind] >= extMaxFindings {
			return
		}
		count[kind]++
		out = append(out, f)
	}
	for i := range slow {
		g := &slow[i]
		a := ext[g.Digest]
		if a == nil || a.cur == 0 {
			continue
		}
		sql := g.SQL
		if len(sql) > 90 {
			sql = sql[:90] + "…"
		}
		scope := g.DB + "." + g.User
		link := model.Link{Page: "sql", Label: "查看该 SQL"}
		base := func(id, sev, title, cause, ref string, ev []string, acts ...model.Tip) model.Finding {
			return model.Finding{ID: id, Sev: sev, Title: title, Scope: scope, Evidence: ev, Cause: cause, Actions: acts, Link: link, Ref: ref}
		}
		stat := fmt.Sprintf("最近 1 小时慢日志 %d 条，平均 %.2fs", a.cur, a.avgCur())

		if oldP, newP, ok := a.planChange(); ok {
			sev := "info"
			ev := []string{stat, fmt.Sprintf("上一小时 %d 条，平均 %.2fs；新计划 %s，旧计划 %s", a.prev, a.avgPrev(), shortID(newP), shortID(oldP))}
			if a.avgPrev() > 0 && a.avgCur() >= extPlanSlower*a.avgPrev() {
				sev = "warning"
				ev = append(ev, fmt.Sprintf("平均耗时变为 %.1f 倍", a.avgCur()/a.avgPrev()))
			}
			add("plan", base("slowx-plan-"+g.Digest, sev, fmt.Sprintf("执行计划发生变化：%s", sql),
				"官方「SQL 突然变慢」的首要排查项：同一条 SQL 换了执行计划。多见于统计信息更新（自动 ANALYZE）、数据分布变化或参数不同导致估算偏差。注意这里比较的是慢日志样本：上一小时没有记录，只说明那时没有超过慢日志阈值的执行。",
				"sql-plan-management", ev, planTip(g, a, oldP, newP)))
		} else if a.prev >= extMinSamples && a.cur >= extMinSamples && a.avgPrev() > 0 && a.avgCur() >= extMinAvgSec && a.avgCur() >= extSlowerRatio*a.avgPrev() {
			add("slower", base("slowx-slower-"+g.Digest, "warning", fmt.Sprintf("同一计划下耗时明显上升：%s", sql),
				"执行计划没有变，但平均耗时上升：常见原因是数据量增长、统计信息过期、TiKV 负载或热点、并发增加或锁冲突，要结合其它诊断项与监控判断。",
				"identify-slow-queries",
				[]string{stat, fmt.Sprintf("上一小时 %d 条，平均 %.2fs（变为 %.1f 倍）", a.prev, a.avgPrev(), a.avgCur()/a.avgPrev())},
				model.Tip{T: "对比执行计划各算子的 actRows 与 estRows、耗时，确认慢在哪一步", Cmd: "EXPLAIN ANALYZE <该 SQL>;"}))
		}
		if a.share(a.pseudo) >= extPseudoShare {
			add("pseudo", base("slowx-stats-"+g.Digest, "warning", fmt.Sprintf("统计信息缺失（pseudo）：%s", sql),
				"官方「慢查询日志」：Stats 显示 pseudo 表示该表没有统计信息或已严重过期，优化器按经验值估算，容易选错索引 / Join 方式。",
				"statistics", []string{stat, fmt.Sprintf("%d 条（%.0f%%）使用 pseudo 统计信息", a.pseudo, a.share(a.pseudo)*100)}, pseudoTip(g)))
		}
		if a.share(a.lock) >= extLockShare {
			add("lock", base("slowx-lock-"+g.Digest, "warning", fmt.Sprintf("慢日志显示锁冲突：%s", sql),
				"慢日志的 Backoff_types 含 txnLock，说明这条语句反复遇到别的事务持有的锁并重试；通常是少数热点行被并发更新或事务过长。",
				"troubleshoot-lock-conflicts",
				[]string{stat, fmt.Sprintf("%d 条（%.0f%%）出现 txnLock 退避，Resolve_lock_time 最长 %.2fs，最多重试 %.0f 次", a.lock, a.share(a.lock)*100, a.maxResolveLock, a.maxRetry)}, lockTip(a)))
		}
		if a.share(a.busy) >= extBusyShare {
			add("busy", base("slowx-busy-"+g.Digest, "info", fmt.Sprintf("TiKV 繁忙导致退避：%s", sql),
				"慢日志的 Backoff_types 含 tikvServerBusy：请求到达 TiKV 时它处理不过来，需要退避重试。这是症状，根因在 TiKV（线程池 / 磁盘 / write stall），见 TiKV 写入链路相关诊断项。",
				"troubleshoot-high-disk-io", []string{stat, fmt.Sprintf("%d 条（%.0f%%）出现 tikvServerBusy 退避", a.busy, a.share(a.busy)*100)}, busyTip(a)))
		}
		if a.share(a.regionMiss) >= extMissShare && a.pd == 0 {
			add("miss", base("slowx-miss-"+g.Digest, "info", fmt.Sprintf("Region 信息过期导致重试：%s", sql),
				"Backoff_types 含 regionMiss：TiDB 缓存的 Region 位置过期（刚发生 Region 分裂 / 合并 / Leader 迁移，或在做调度）。持续出现说明 Region 调度很频繁。",
				"tidb-scheduling", []string{stat, fmt.Sprintf("%d 条（%.0f%%）出现 regionMiss 退避", a.regionMiss, a.share(a.regionMiss)*100)},
				model.Tip{T: "对照 PD 的调度监控（operator 数量）和是否正在扩缩容 / 迁移", Cmd: "tiup ctl:<版本> pd -u <PD地址> operator show"}))
		}
		if ratio := mvccRatio(g); ratio >= extMVCCRatio && g.Keys >= extMVCCMinKeys && g.Tag != "缺索引 / 统计信息过期" {
			add("mvcc", base("slowx-mvcc-"+g.Digest, "info", fmt.Sprintf("扫描的 Key 远多于处理的 Key：%s", sql),
				"官方「慢查询日志」：Total_keys 远大于 Process_keys，说明扫描中遇到了很多 MVCC 旧版本或已删除标记（tombstone）。常见于大批量删除 / 更新之后 GC 或 compaction 还没跟上，也可能有长事务拖住 GC。",
				"identify-slow-queries", []string{stat, fmt.Sprintf("平均扫描 %.0f Key，处理 %.0f Key（%.0f 倍）；单次最多跳过 %.0f 个 tombstone", g.Keys, g.PKeys, ratio, a.maxDelSkipped)}, mvccTip(g, a, ratio)))
		}
	}
	return out
}
