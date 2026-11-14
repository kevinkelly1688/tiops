// @author: kevinkelly
// @license: Apache License 2.0
// @file: slow.go
// @time: 2026/10/28

package collect

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"tidbops/internal/model"
)

type slowRow struct {
	digest, db, user, query, index string
	qt, total, process, wait       float64
	keys, pkeys, rows, mem         float64
}

// Slow 汇总慢查询（information_schema.cluster_slow_query）与语句摘要（cluster_statements_summary），
// 按 Digest 聚合，并附带规则化的优化建议。
func (c *Collector) Slow(ctx context.Context) ([]model.SlowGroup, error) {
	out, _, err := c.slowWithExt(ctx)
	return out, err
}

// slowWithExt 同 Slow，并返回慢日志扩展聚合（sql/slow-ext，取不到为 nil），供诊断规则使用。
func (c *Collector) slowWithExt(ctx context.Context) ([]model.SlowGroup, map[string]*slowAgg, error) {
	b, err := c.get(ctx, "sql/slow-query")
	if err != nil {
		return nil, nil, fmt.Errorf("sql/slow-query: %w", err)
	}
	_, rows := parseTSV(b)
	groups := map[string][]slowRow{}
	var order []string
	for _, r := range rows {
		sr := slowRow{digest: r["digest"], db: r["db"], user: r["user"], query: r["query"], index: r["index_names"],
			qt: f64(r["query_time"]), total: f64(r["total_keys"]), process: f64(r["process_time"]), wait: f64(r["wait_time"]),
			keys: f64(r["total_keys"]), pkeys: f64(r["process_keys"]), rows: f64(r["result_rows"]), mem: f64(r["mem_max"])}
		if sr.digest == "" {
			continue
		}
		if _, ok := groups[sr.digest]; !ok {
			order = append(order, sr.digest)
		}
		groups[sr.digest] = append(groups[sr.digest], sr)
	}
	// 语句摘要：累计执行次数 / 平均耗时（纳秒）
	type sum struct{ exec, avg, max, total float64 }
	summ := map[string]sum{}
	if sb, err := c.get(ctx, "sql/statements-summary"); err == nil {
		_, srows := parseTSV(sb)
		for _, r := range srows {
			summ[r["digest"]] = sum{f64(r["exec_count"]), f64(r["avg_latency"]) / 1e9, f64(r["max_latency"]) / 1e9, f64(r["sum_latency"]) / 1e9}
		}
	}
	plans := c.planMap(ctx)
	ext := c.SlowExt(ctx)
	var out []model.SlowGroup
	for _, d := range order {
		rs := groups[d]
		g := model.SlowGroup{Digest: d, DB: rs[0].db, User: rs[0].user, SQL: rs[0].query, Index: rs[0].index, Plan: [][]any{}}
		var qts []float64
		for _, r := range rs {
			g.Avg += r.qt
			g.Keys += r.keys
			g.PKeys += r.pkeys
			g.Rows += r.rows
			g.Wait += r.wait
			if r.mem > 0 && human(r.mem) != "" && r.mem > memVal(g.Mem) {
				g.Mem = human(r.mem)
			}
			qts = append(qts, r.qt)
		}
		n := float64(len(rs))
		g.Avg, g.Keys, g.PKeys, g.Rows, g.Wait = g.Avg/n, g.Keys/n, g.PKeys/n, g.Rows/n, g.Wait/n
		waitRatio := 0.0 // 等待占慢日志样本平均耗时的比例（与摘要里的平均耗时口径不同，需在覆盖前计算）
		if g.Avg > 0 {
			waitRatio = g.Wait / g.Avg
		}
		g.Exec, g.P99 = len(rs), percentile(qts, 0.99)
		g.Total = g.Avg * n
		if s, ok := summ[d]; ok && s.exec > 0 {
			g.Exec, g.Avg, g.Total = int(s.exec), s.avg, s.total
			if s.max > g.P99 {
				g.P99 = s.max
			}
		}
		if g.Index == "" {
			g.Index = "—（TableFullScan）"
		}
		g.Tag, g.Tips = advise(g, waitRatio)
		if p := plans[d]; len(p) > 0 {
			g.Plan = p
			if tips, tag := planTips(p, g.Tips); len(tips) > 0 {
				g.Tips = append(tips, g.Tips...)
				if g.Tag == "耗时较长" {
					g.Tag = tag
				}
			}
		}
		applyExt(&g, ext[d])
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	for i := range out {
		out[i].ID = i + 1
		out[i].Avg, out[i].P99, out[i].Total = round2(out[i].Avg), round2(out[i].P99), round2(out[i].Total)
	}
	return out, ext, nil
}

func memVal(s string) float64 {
	var v float64
	var u string
	fmt.Sscanf(s, "%f %s", &v, &u)
	switch u {
	case "GiB":
		return v * (1 << 30)
	case "MiB":
		return v * (1 << 20)
	case "KiB":
		return v * (1 << 10)
	}
	return v
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(float64(len(s)-1)*p + 0.5)
	return s[i]
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

var (
	reFuncCol  = regexp.MustCompile(`(?i)\b(date|year|month|day|lower|upper|substr|substring|ifnull|cast)\s*\(\s*[A-Za-z_` + "`" + `][\w.` + "`" + `]*\s*\)\s*(=|>|<|>=|<=|in)`)
	reLeadLike = regexp.MustCompile(`(?i)\blike\s+'%`)
	reFrom     = regexp.MustCompile("(?i)\\b(?:from|update|into)\\s+`?([A-Za-z_][A-Za-z0-9_]*)`?")
	reSelStar  = regexp.MustCompile(`(?i)^\s*select\s+\*`)
)

func tableOf(sql string) string {
	if m := reFrom.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	return "<表名>"
}

// TagKVQueue 是「慢日志里 Wait_time 占比高」的标签（原来叫「热点行锁等待」，但 Wait_time 是排队而不是锁等待，见 advise）。
const TagKVQueue = "TiKV 排队等待"

// advise 根据慢查询的统计特征与 SQL 文本给出主要问题标签和建议。规则来自官方 SQL 调优与故障排查文档。
func advise(g model.SlowGroup, waitRatio float64) (string, []model.Tip) {
	var tips []model.Tip
	tag := ""
	sql, tbl := g.SQL, tableOf(g.SQL)
	isSelect := strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "select")
	set := func(t string) {
		if tag == "" {
			tag = t
		}
	}
	if waitRatio >= 0.7 {
		// 慢日志的 Wait_time 是 TiKV 侧 Coprocessor 线程的排队等待时间，不是锁等待（官方「慢查询日志」说明）：
		// 官方的说法是「wait_time 很大而 process_time 很小的语句通常不是问题语句，多半是被其他问题语句阻塞」。
		// 锁冲突的证据是 Backoff_types 里的 txnLock / txnLockFast、Resolve_lock_time 偏大、或 DATA_LOCK_WAITS / DEADLOCKS 里有记录。
		set(TagKVQueue)
		tips = append(tips,
			model.Tip{T: fmt.Sprintf("慢日志样本中约 %.0f%% 的耗时是 TiKV 端 Coprocessor 排队等待（Wait_time），语句本身处理量不大：它通常不是根因，而是被同时运行的重 SQL 或繁忙的 TiKV 拖慢。先找同时段处理量大（Process_time、Total_keys 高）的语句，并看 TiKV 的线程 CPU 与热点", waitRatio*100)},
			model.Tip{T: "Wait_time 不代表锁等待。若怀疑是并发更新同一行造成的锁冲突，用下面的语句确认（慢日志里 Backoff_types 含 txnLock、或 Resolve_lock_time 偏大才是锁冲突的证据）", Cmd: "SELECT * FROM information_schema.DATA_LOCK_WAITS;\nSELECT * FROM information_schema.DEADLOCKS;"})
	}
	if reFuncCol.MatchString(sql) {
		set("索引列上使用函数")
		tips = append(tips, model.Tip{T: "对条件列使用函数会让索引失效，请改写为范围条件（例如 col >= '起始' AND col < '结束'）"})
	}
	if reLeadLike.MatchString(sql) {
		set("前导通配符 LIKE")
		tips = append(tips, model.Tip{T: "LIKE '%xxx' 无法走 B-Tree 索引：可改用全文检索，或评估 TiFlash 扫描",
			Cmd: fmt.Sprintf("ALTER TABLE %s SET TIFLASH REPLICA 2;", tbl)})
	}
	if isSelect && g.Keys >= 1e6 && g.PKeys > 0 && g.Keys/maxF(g.Rows, 1) >= 1000 && strings.HasPrefix(g.Index, "—") {
		set("缺索引 / 统计信息过期")
		tips = append(tips,
			model.Tip{T: fmt.Sprintf("扫描 %.0f 个 Key 只返回 %.0f 行，且没有使用索引：先更新统计信息，再为过滤条件列建（联合）索引", g.Keys, g.Rows), Cmd: fmt.Sprintf("ANALYZE TABLE %s;", tbl)},
			model.Tip{T: "建索引前可用 SQL Binding 固定计划验证效果，上线后再放开", Cmd: "CREATE GLOBAL BINDING FOR <原 SQL> USING <带 USE_INDEX 提示的 SQL>;"})
	}
	if memVal(g.Mem) >= 1<<30 {
		set("大查询占用内存高")
		tips = append(tips,
			model.Tip{T: fmt.Sprintf("单条查询最大内存 %s：限制单 SQL 内存，并把报表类查询放到独立资源组或下推到 TiFlash", g.Mem), Cmd: "SET SESSION tidb_mem_quota_query = 2147483648;"},
			model.Tip{T: "分析型查询可使用 TiFlash（列存 + MPP）", Cmd: fmt.Sprintf("ALTER TABLE %s SET TIFLASH REPLICA 2;", tbl)})
	}
	if reSelStar.MatchString(sql) {
		tips = append(tips, model.Tip{T: "避免 SELECT *：只取需要的列，减少回表与网络开销，利于覆盖索引"})
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "insert") && g.Exec >= 100000 {
		set("高频写入（注意写热点）")
		tips = append(tips, model.Tip{T: "高频顺序写入如使用 AUTO_INCREMENT 主键会产生写热点：改用 AUTO_RANDOM，或 SHARD_ROW_ID_BITS 打散",
			Cmd: "CREATE TABLE t (id BIGINT PRIMARY KEY AUTO_RANDOM, ...);"})
	}
	if tag == "" {
		tag = "耗时较长"
		tips = append(tips, model.Tip{T: "未命中常见模式：请用 EXPLAIN ANALYZE 查看执行计划，对比 estRows 与 actRows，确认统计信息是否准确", Cmd: "EXPLAIN ANALYZE <该 SQL>;"})
	}
	return tag, tips
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
