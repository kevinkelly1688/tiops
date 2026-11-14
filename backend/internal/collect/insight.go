// @author: kevinkelly
// @license: Apache License 2.0
// @file: insight.go
// @time: 2026/10/28

package collect

import (
	"context"
	"fmt"
	"math"
	"sort"

	"tidbops/internal/model"
)

// ---------------------------------------------------------------- 序列工具 ----

// Mean 返回序列所有有效点（非 NaN / Inf、非负）的平均值；没有有效点时 ok=false。
func (s Series) Mean() (v float64, ok bool) {
	sum, n := 0.0, 0
	for _, p := range s.Points {
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) || p.V < 0 {
			continue
		}
		sum += p.V
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// matrix 取一个 Prometheus 条目；取不到返回 nil。
func (c *Collector) matrix(ctx context.Context, name string) []Series {
	b, err := c.get(ctx, name)
	if err != nil {
		return nil
	}
	s, err := ParseMatrix(b)
	if err != nil {
		return nil
	}
	return s
}

// ---------------------------------------------------------------- DB Time ----

// DBTime 计算数据库时间的三个分解维度。它们都是窗口内 rate(xxx_sum) 的平均值（即「每秒累计耗时」）的占比，
// 因此是相对占比，而不是绝对耗时。某个维度取不到数据时返回空数组。
func (c *Collector) DBTime(ctx context.Context) model.DBTime {
	out := model.DBTime{ByType: []model.Item{}, ByStage: []model.Item{}, ByExec: []model.Item{}}

	// 按 SQL 类型：sql_type 标签，前 6 名 + 其他
	types := map[string]float64{}
	for _, s := range c.matrix(ctx, "prom/dbtime-type") {
		if v, ok := s.Mean(); ok && s.Metric["sql_type"] != "" {
			types[s.Metric["sql_type"]] += v
		}
	}
	out.ByType = topItems(types, 6, "其他")

	// 按处理阶段：必须有 Execute 才有意义；其余阶段缺失时按 0 处理。
	// tidb_server_get_token_duration_seconds 的单位实际是微秒，换算成秒再与其他阶段相加。
	single := func(name string, scale float64) (float64, bool) {
		ss := c.matrix(ctx, name)
		if len(ss) == 0 {
			return 0, false
		}
		sum, got := 0.0, false
		for _, s := range ss {
			if v, ok := s.Mean(); ok {
				sum, got = sum+v, true
			}
		}
		return sum * scale, got
	}
	var names []string
	var vals []float64
	execOK := false
	for _, st := range []struct {
		name, item string
		scale      float64
	}{{"Get Token", "prom/dbtime-token", 1e-6}, {"Parse", "prom/dbtime-parse", 1}, {"Compile", "prom/dbtime-compile", 1}, {"Execute", "prom/dbtime-execute", 1}} {
		v, ok := single(st.item, st.scale)
		if !ok {
			continue
		}
		if st.name == "Execute" {
			execOK = true
		}
		names, vals = append(names, st.name), append(vals, v)
	}
	if execOK {
		out.ByStage = pctItems(names, vals)
	}

	// 执行阶段细分：TSO 等待 + DistSQL 请求 + TiKV 客户端各类请求（前 4 种 + 其他）。
	// 注意：DistSQL 的耗时包含其中的 Cop 请求，各项之间有交叠，这里只用于相对比较。
	exec := map[string]float64{}
	if v, ok := single("prom/dbtime-exec-tso", 1); ok {
		exec["TSO 等待"] = v
	}
	if v, ok := single("prom/dbtime-exec-distsql", 1); ok {
		exec["DistSQL 请求"] = v
	}
	kv := map[string]float64{}
	for _, s := range c.matrix(ctx, "prom/dbtime-exec-kv") {
		if v, ok := s.Mean(); ok && s.Metric["type"] != "" {
			kv["KV "+s.Metric["type"]] += v
		}
	}
	for _, it := range topItemsRaw(kv, 4, "KV 其他") {
		exec[it.name] = it.v
	}
	out.ByExec = topItems(exec, len(exec), "其他")
	return out
}

type namedVal struct {
	name string
	v    float64
}

// topItemsRaw 取数值最大的 n 项，其余合并为 other（other 排在最后）。只保留有限的正数。
func topItemsRaw(m map[string]float64, n int, other string) []namedVal {
	var list []namedVal
	for k, v := range m {
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			list = append(list, namedVal{k, v})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].name < list[j].name
	})
	if len(list) <= n {
		return list
	}
	rest := 0.0
	for _, x := range list[n:] {
		rest += x.v
	}
	return append(list[:n:n], namedVal{other, rest})
}

// topItems 是 topItemsRaw 加上占比归一（总和恰为 100）。没有有效数据时返回空数组。
func topItems(m map[string]float64, n int, other string) []model.Item {
	list := topItemsRaw(m, n, other)
	names, vals := make([]string, len(list)), make([]float64, len(list))
	for i, x := range list {
		names[i], vals[i] = x.name, x.v
	}
	return pctItems(names, vals)
}

// pctItems 把数值换算成占比，保留 1 位小数，并用最大余数法保证总和恰好为 100.0。
// 保持传入顺序。总和为 0 时返回空数组。
func pctItems(names []string, vals []float64) []model.Item {
	total := 0.0
	for _, v := range vals {
		if v > 0 {
			total += v
		}
	}
	items := []model.Item{}
	if total <= 0 {
		return items
	}
	type part struct {
		tenths int
		frac   float64
	}
	parts := make([]part, len(vals))
	used := 0
	for i, v := range vals {
		if v < 0 {
			v = 0
		}
		x := v / total * 1000
		parts[i] = part{int(math.Floor(x)), x - math.Floor(x)}
		used += parts[i].tenths
	}
	order := make([]int, len(vals))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return parts[order[a]].frac > parts[order[b]].frac })
	for k := 0; used < 1000 && k < len(order); k, used = k+1, used+1 {
		parts[order[k]].tenths++
	}
	for i, n := range names {
		items = append(items, model.Item{Name: n, Pct: float64(parts[i].tenths) / 10})
	}
	return items
}

// ---------------------------------------------------------------- 容量预测 ----

const (
	gib           = 1 << 30
	forecastMinPt = 24 // 至少 24 个小时点（约 24 小时）才做预测
)

// Forecast 基于最近 7 天（每小时一个点）的已用容量与 Region 数做线性回归，预测容量何时到达 80% / 95%。
func (c *Collector) Forecast(ctx context.Context) model.Forecast {
	first := func(name string) *Series {
		if ss := c.matrix(ctx, name); len(ss) > 0 {
			return &ss[0]
		}
		return nil
	}
	return ForecastFrom(first("prom/forecast-used"), first("prom/forecast-capacity"), first("prom/forecast-regions"))
}

// ForecastFrom 是 Forecast 的纯计算部分。used / capacity 的单位为字节；regions 可为 nil。
// 样本数不足 24 个、或时间跨度不足 23 小时时 Enough=false：给出当前值，但不给出增长速度与到达天数。
func ForecastFrom(used, capacity, regions *Series) model.Forecast {
	f := model.Forecast{}
	if used == nil || len(used.Points) == 0 || capacity == nil || len(capacity.Points) == 0 {
		f.Basis = "未采集到容量指标（Prometheus 的 tikv_store_size_bytes），无法预测"
		return f
	}
	f.UsedGB = round1(lastOf(*used) / gib)
	f.CapGB = round1(lastOf(*capacity) / gib)
	if regions != nil && len(regions.Points) > 0 {
		f.Regions = int(math.Round(lastOf(*regions)))
	}
	pts := recent(used.Points, 7*24*3600)
	span := pts[len(pts)-1].T - pts[0].T
	if len(pts) < forecastMinPt || span < 23*3600 {
		f.Basis = fmt.Sprintf("样本不足：仅有 %d 个小时点（约 %.0f 小时），至少需要 24 小时的数据才能预测", len(pts), float64(span)/3600)
		return f
	}
	slopeGBDay, _ := slopePerDay(pts, 1.0/gib)
	f.Enough = true
	f.GrowthGBPerDay = rnd2(slopeGBDay)
	f.DaysTo80 = daysTo(f.UsedGB, f.CapGB*0.80, slopeGBDay)
	f.DaysTo95 = daysTo(f.UsedGB, f.CapGB*0.95, slopeGBDay)
	if regions != nil {
		if rp := recent(regions.Points, 7*24*3600); len(rp) >= forecastMinPt {
			g, _ := slopePerDay(rp, 1)
			f.RegionGrowthPerDay = rnd1(g)
		}
	}
	f.Basis = fmt.Sprintf("最近 %.1f 天 %d 个小时点的线性回归（已用容量每天约 %+.1f GiB）", float64(span)/86400, len(pts), slopeGBDay)
	return f
}

func lastOf(s Series) float64 { return s.Points[len(s.Points)-1].V }

// recent 返回最后一个点之前 window 秒内的点（过滤 NaN / Inf）。至少返回 1 个点（调用方保证 pts 非空）。
func recent(pts []Point, window int64) []Point {
	if len(pts) == 0 {
		return nil
	}
	end := pts[len(pts)-1].T
	var out []Point
	for _, p := range pts {
		if p.T >= end-window && !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = pts[len(pts)-1:]
	}
	return out
}

// slopePerDay 对 (时间, 值*scale) 做最小二乘线性回归，返回每天的增长量与截距。
func slopePerDay(pts []Point, scale float64) (slope, intercept float64) {
	n := float64(len(pts))
	if n < 2 {
		return 0, 0
	}
	t0 := pts[0].T
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		x := float64(p.T-t0) / 86400
		y := p.V * scale
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, sy / n
	}
	slope = (n*sxy - sx*sy) / den
	return slope, (sy - slope*sx) / n
}

// daysTo 返回已用容量从 usedGB 增长到 targetGB 需要的天数。已达到返回 0；不增长返回 nil（JSON null）。
func daysTo(usedGB, targetGB, growthPerDay float64) *float64 {
	if usedGB >= targetGB {
		z := 0.0
		return &z
	}
	if growthPerDay <= 0 {
		return nil
	}
	d := round1((targetGB - usedGB) / growthPerDay)
	return &d
}

func rnd2(v float64) float64 { return math.Round(v*100) / 100 }
func rnd1(v float64) float64 { return math.Round(v*10) / 10 }
