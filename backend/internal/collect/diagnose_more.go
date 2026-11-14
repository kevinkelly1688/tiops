// @author: kevinkelly
// @license: Apache License 2.0
// @file: diagnose_more.go
// @time: 2026/10/28

package collect

// 诊断规则第一批扩充：DB Time 自动判读、容量预测入诊断、「持续一段时间才按原级别提示」的判定。
// 规则依据官方「性能调优方法」「日常巡检」「热点问题处理」「锁冲突排查」等文档；阈值是经验值（官方只给定性结论），
// 文档里写明的数字（如 pending-peer 持续 30 分钟）才按原文取。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"tidbops/internal/model"
)

func rank(sev string) int {
	switch sev {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

// sortFindings 按严重程度排序（稳定，同级别保持原来的顺序）。
func sortFindings(fs []model.Finding) {
	sort.SliceStable(fs, func(i, j int) bool { return rank(fs[i].Sev) < rank(fs[j].Sev) })
}

// ---------------------------------------------------------------- DB Time ----

const (
	dbtMinQPS      = 10.0 // QPS 低于它时，DB Time 的占比没有意义（空闲集群里任何阶段都可能占大头），不判读
	dbtCompilePct  = 20.0 // Compile 占 DB Time 的比例
	dbtTokenPct    = 5.0  // Get Token 占 DB Time 的比例
	dbtTSOPct      = 30.0 // TSO 等待占 Execute 细分的比例
	dbtPessLockPct = 30.0 // 悲观锁请求占 Execute 细分的比例
)

func itemPct(items []model.Item, match func(name string) bool) (float64, bool) {
	for _, it := range items {
		if match(it.Name) {
			return it.Pct, true
		}
	}
	return 0, false
}

func eqFold(want string) func(string) bool {
	return func(n string) bool { return strings.EqualFold(n, want) }
}

func pctList(items []model.Item) string {
	var s []string
	for _, it := range items {
		s = append(s, fmt.Sprintf("%s %.1f%%", it.Name, it.Pct))
	}
	return strings.Join(s, " · ")
}

// DiagnoseDBTime 把 DB Time 的三个分解维度自动判读成诊断项（官方「性能调优方法」里的「典型异常模式」）。
// 占比是相对值（各阶段 rate(xxx_sum) 的相对比例），不是绝对耗时，所以要求有一定负载（qps ≥ 10）才判读；
// 「Execute 细分」各项有交叠，只用于相对比较。
func DiagnoseDBTime(dbt model.DBTime, qps float64) []model.Finding {
	if qps < dbtMinQPS {
		return nil
	}
	var out []model.Finding
	stage := func(name string) (float64, bool) { return itemPct(dbt.ByStage, eqFold(name)) }
	ev := func(extra ...string) []string {
		e := []string{fmt.Sprintf("按处理阶段：%s（QPS %.0f）", pctList(dbt.ByStage), qps)}
		return append(e, extra...)
	}

	// 1. Compile 占比高：没用上执行计划缓存
	if p, ok := stage("Compile"); ok && p >= dbtCompilePct {
		pp, _ := stage("Parse")
		out = append(out, model.Finding{ID: "dbt-compile", Sev: "warning", Scope: "tidb",
			Title:    fmt.Sprintf("SQL 编译（Compile）占数据库时间 %.0f%%：执行计划缓存可能没有生效", p),
			Evidence: ev(fmt.Sprintf("Compile %.1f%% ≥ %.0f%%，Parse %.1f%%", p, dbtCompilePct, pp)),
			Cause:    "官方性能调优方法：Compile 占比高，通常是应用用 query 命令而不是 Prepared Statement（每次都重新编译），或 Prepare 后立刻 Close 清掉了缓存的计划（StmtClose 与 StmtExecute 同频），或语句不满足缓存条件。",
			Actions: []model.Tip{
				{T: "确认计划缓存命中情况：看 Grafana「Queries Using Plan Cache OPS」里 avg-hit 是否长期为 0；或查摘要表里 PLAN_IN_CACHE / PLAN_CACHE_HITS",
					Cmd: "SELECT digest_text, exec_count, plan_in_cache, plan_cache_hits, plan_cache_unqualified_last_reason\nFROM information_schema.statements_summary ORDER BY sum_latency DESC LIMIT 10;"},
				{T: "应用改用 Prepared Statement 并复用（连接池 + 服务端预编译），避免每次拼接 SQL 文本"},
				{T: "v6.0.0 起，应用每次 Execute 后都 Close 时，可让 TiDB 忽略 Close 以保留缓存的计划", Cmd: "SHOW VARIABLES LIKE 'tidb_enable_prepared_plan_cache';\nSET GLOBAL tidb_ignore_prepared_cache_close_stmt = ON;"}},
			Link: model.Link{Page: "sql", Label: "查看 Top SQL"}, Ref: "performance-tuning-methods"})
	}
	// 2. Get Token 占比高：并发超过 token-limit
	if p, ok := stage("Get Token"); ok && p >= dbtTokenPct {
		out = append(out, model.Finding{ID: "dbt-token", Sev: "warning", Scope: "tidb",
			Title:    fmt.Sprintf("获取令牌（Get Token）占数据库时间 %.0f%%：并发请求在 TiDB 内排队", p),
			Evidence: ev(fmt.Sprintf("Get Token %.1f%% ≥ %.0f%%（正常应接近 0）", p, dbtTokenPct)),
			Cause:    "同时执行的请求数超过 TiDB 的并发令牌数（performance.token-limit，默认 1000），多出来的请求要先排队取令牌；常见于连接数暴涨、慢查询占满并发。",
			Actions: []model.Tip{
				{T: "先看是不是慢 SQL 占住了并发：处理慢查询通常比调大 token-limit 更有效"},
				{T: "确认各 TiDB 的当前配置", Cmd: "SELECT instance, value FROM information_schema.cluster_config\nWHERE type = 'tidb' AND `key` = 'performance.token-limit';"},
				{T: "连接数持续过高时，用连接池限制应用并发，或扩容 TiDB"}},
			Link: model.Link{Page: "sql", Label: "查看 Top SQL"}, Ref: "performance-tuning-methods"})
	}
	// 3. TSO 等待占比高：PD / 网络
	if p, ok := itemPct(dbt.ByExec, eqFold("TSO 等待")); ok && p >= dbtTSOPct {
		out = append(out, model.Finding{ID: "dbt-tso", Sev: "warning", Scope: "tidb",
			Title:    fmt.Sprintf("TSO 等待占 Execute 阶段 %.0f%%：获取时间戳偏慢", p),
			Evidence: ev(fmt.Sprintf("Execute 细分（各项有交叠，仅作相对比较）：%s", pctList(dbt.ByExec)), fmt.Sprintf("TSO 等待 %.1f%% ≥ %.0f%%", p, dbtTSOPct)),
			Cause:    "官方性能调优方法与日常巡检：TSO 等待占比高，需要排查 TiDB 到 PD 的网络延迟、TiDB 自身负载和 PD 负载。",
			Actions: []model.Tip{
				{T: "看监控「PD TSO Wait Duration」与「PD TSO RPC Duration」，确认是等待 PD 还是网络", Cmd: "ping -c 10 <PD 主机>   # 同机房通常远小于 1ms，跨机房明显偏高就是网络"},
				{T: "排查 PD leader 所在机器的 CPU / 磁盘 / 网络，以及是否刚发生过 PD leader 切换"},
				{T: "TiDB 侧 CPU 打满也会拉长等待，结合总览里各 TiDB 的 CPU 判断"}},
			Link: model.Link{Page: "arch", Label: "查看拓扑"}, Ref: "performance-tuning-methods"})
	}
	// 4. 悲观锁请求占比高：锁冲突
	if p, ok := itemPct(dbt.ByExec, func(n string) bool { return strings.Contains(strings.ToLower(n), "pessimisticlock") }); ok && p >= dbtPessLockPct {
		out = append(out, model.Finding{ID: "dbt-lock", Sev: "warning", Scope: "tidb",
			Title:    fmt.Sprintf("悲观锁请求占 Execute 阶段 %.0f%%：疑似存在锁冲突 / 热点行", p),
			Evidence: ev(fmt.Sprintf("Execute 细分（各项有交叠，仅作相对比较）：%s", pctList(dbt.ByExec)), fmt.Sprintf("PessimisticLock %.1f%% ≥ %.0f%%", p, dbtPessLockPct)),
			Cause:    "官方性能调优方法：悲观锁（PessimisticLock）占比高，说明加锁等待与重试耗掉了大量时间，通常是少数热点行被并发更新，或事务过长 / 过大。占比只是线索，要用下面的系统表确认。",
			Actions: []model.Tip{
				{T: "确认谁在等锁、谁持锁（仅覆盖悲观锁；这些表只反映当前时刻）", Cmd: "SELECT * FROM information_schema.DATA_LOCK_WAITS;\nSELECT * FROM information_schema.DEADLOCKS;"},
				{T: "查看等待锁的事务与已等待时长", Cmd: "SELECT * FROM information_schema.CLUSTER_TIDB_TRX WHERE state = 'LockWaiting';"},
				{T: "从业务侧降低同一行的并发更新（分桶 / 合并 / 缩短事务）；同一行高并发加锁可评估 tidb_pessimistic_txn_fair_locking（新集群默认启用）"}},
			Link: model.Link{Page: "sql", Label: "查看 Top SQL"}, Ref: "troubleshoot-lock-conflicts"})
	}
	return out
}

// ---------------------------------------------------------------- 容量预测 ----

// DiagnoseForecast 把容量预测变成诊断项：现在还没到 80% 水位，但按近 7 天增长趋势很快会到，提前提示扩容 / 清理。
// 已经 ≥ 80% 的由「TiKV 磁盘水位」规则与告警负责，这里不重复。样本不足（Enough=false）时不给结论。
func DiagnoseForecast(f model.Forecast) []model.Finding {
	if !f.Enough || f.CapGB <= 0 || f.DaysTo80 == nil {
		return nil
	}
	used := f.UsedGB / f.CapGB * 100
	if used >= 80 {
		return nil
	}
	d80 := *f.DaysTo80
	sev := ""
	switch {
	case d80 <= 7:
		sev = "critical"
	case d80 <= 30:
		sev = "warning"
	default:
		return nil
	}
	ev := []string{
		fmt.Sprintf("全部 TiKV 已用 %.1f TiB / 容量 %.1f TiB（%.0f%%）", f.UsedGB/1024, f.CapGB/1024, used),
		fmt.Sprintf("近期增长约 %.0f GB/天；按此速度约 %s后到 80%% 水位", f.GrowthGBPerDay, daysText(d80)),
	}
	if f.DaysTo95 != nil {
		ev = append(ev, fmt.Sprintf("约 %s后到 95%%", daysText(*f.DaysTo95)))
	}
	if f.Basis != "" {
		ev = append(ev, "预测依据："+f.Basis)
	}
	return []model.Finding{{ID: "forecast", Sev: sev, Scope: "tikv",
		Title:    fmt.Sprintf("按近期增长，约 %s后 TiKV 容量到 80%% 水位", daysText(d80)),
		Evidence: ev,
		Cause:    "容量使用率 ≥ 80% 时（PD 默认 low-space-ratio）副本补齐与 Region 均衡受限，更高会有写入失败风险（官方告警规则 NODE_disk_used_more_than_80% / TiKV_low_space）。预测是对近 7 天用量做线性回归，大批量导入或清理会让它失准，仅供规划。",
		Actions: []model.Tip{
			{T: "现在就规划扩容 TiKV（扩容后 PD 会自动均衡 Region，需要一段时间，所以要留出提前量）", Cmd: "tiup cluster scale-out <集群名> scale-out-tikv.yml"},
			{T: "同时核对是否有可以清理的数据：历史表、日志表（可用 TTL 自动清理），以及 GC 是否正常推进", Cmd: "SELECT * FROM mysql.tidb WHERE variable_name LIKE 'tikv_gc%';"}},
		Link: model.Link{Page: "nodes", Label: "去扩容 TiKV", Args: map[string]string{"tab": "scaleout", "role": "tikv"}}, Ref: "scale-tidb-using-tiup"}}
}

func daysText(d float64) string {
	if d < 10 {
		return fmt.Sprintf("%.1f 天", d)
	}
	return fmt.Sprintf("%.0f 天", d)
}

// forecastTTL：容量预测要查 7 天的区间数据，不必每次快照都查。
const forecastTTL = 10 * time.Minute

// forecastCached 返回缓存的容量预测（最多 forecastTTL 内不重复查询）。
func (c *Collector) forecastCached(ctx context.Context) model.Forecast {
	c.fcMu.Lock()
	defer c.fcMu.Unlock()
	if !c.fcAt.IsZero() && c.now().Sub(c.fcAt) < forecastTTL {
		return c.fcVal
	}
	c.fcVal, c.fcAt = c.Forecast(ctx), c.now()
	return c.fcVal
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------- 持续时间判定 ----

// sustainRule：规则 ID 前缀 → 需要持续的分钟数。官方多处强调「较小的非零值通常正常」「持续一段时间才有问题」
// （如 pending-peer 持续超过 30 分钟），瞬时抖动不应当直接按严重级别报。
// 实例停止 / 磁盘水位 / 容量预测 / 慢 SQL 及其对比 / 告警汇总 / 文件系统只读 / inode 不在此列：它们要么本身就是持续状态，要么应当立刻提示；
// pending-peer（pdp-）自带「最近 30 分钟最低值」的判定，不再叠加。
var sustainRules = []struct {
	prefix  string
	minutes int
}{{"hotw-", 3}, {"tidbres-", 5}, {"balance", 30}, {"dbt-", 5}, {"tikvw-", 5}, {"pd-", 5}, {"pds-", 5}, {"hostl-", 5}}

// sustainGap：两次快照间隔超过它，就认为中间断了（没人看页面、平台重启……），重新计时。
const sustainGap = 3 * time.Minute

type tracker struct {
	mu    sync.Mutex
	first map[string]time.Time
	last  map[string]time.Time
}

func sustainNeed(id string) int {
	for _, r := range sustainRules {
		if strings.HasPrefix(id, r.prefix) {
			return r.minutes
		}
	}
	return 0
}

func durText(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// apply 记录各诊断项首次出现的时间；需要持续判定的项，在持续时间不够时降为提示级别并标注「观察中」，够了再按原级别显示。
// 不在本次结果里的项会被清掉（下次出现重新计时）。返回的切片已按严重程度重新排序。
func (t *tracker) apply(now time.Time, fs []model.Finding) []model.Finding {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.first == nil {
		t.first, t.last = map[string]time.Time{}, map[string]time.Time{}
	}
	seen := map[string]bool{}
	for i := range fs {
		f := &fs[i]
		need := sustainNeed(f.ID)
		if need == 0 {
			continue
		}
		seen[f.ID] = true
		if l, ok := t.last[f.ID]; !ok || now.Sub(l) > sustainGap {
			t.first[f.ID] = now
		}
		t.last[f.ID] = now
		elapsed := now.Sub(t.first[f.ID])
		if elapsed < time.Duration(need)*time.Minute {
			f.Sev = "info"
			f.Title += fmt.Sprintf("（观察中：已持续 %s / 需 %d 分钟）", durText(elapsed), need)
			f.Evidence = append(f.Evidence, fmt.Sprintf("为避免瞬时抖动误报，同一现象需持续 %d 分钟才按原级别提示；当前已持续 %s。平台重启或长时间没有人查看页面后会重新计时。", need, durText(elapsed)))
		} else {
			f.Evidence = append(f.Evidence, fmt.Sprintf("已持续 %s（首次发现于 %s）", durText(elapsed), t.first[f.ID].Format("15:04:05")))
		}
	}
	for id := range t.first {
		if !seen[id] {
			delete(t.first, id)
			delete(t.last, id)
		}
	}
	sortFindings(fs)
	return fs
}

// sustain 对一次快照的诊断结果做持续时间判定。回放模式（静态数据，时间不会推进）与 NoSustain 时原样返回。
func (c *Collector) sustain(mode string, fs []model.Finding) []model.Finding {
	if mode == "replay" || c.NoSustain {
		sortFindings(fs)
		return fs
	}
	return c.tr.apply(c.now(), fs)
}
