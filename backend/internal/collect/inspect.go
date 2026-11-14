// @author: kevinkelly
// @license: Apache License 2.0
// @file: inspect.go
// @time: 2026/10/28

package collect

import (
	"fmt"
	"sort"
	"strings"

	"tidbops/internal/model"
)

// SupportedVersions 是平台支持写操作的 TiDB 版本系列（A2 决策）。
var SupportedVersions = []string{"v7.5.", "v8.1.", "v8.5."}

// VersionSupported 判断版本是否在支持矩阵内。
func VersionSupported(v string) bool {
	for _, p := range SupportedVersions {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// Inspect 基于一次快照生成巡检报告。只用已采集的数据，取不到的项标记为 na（而不是猜测）。
// extra 是调用方追加的检查项（如 tiup cluster check 的主机检查、BR 状态）。
func Inspect(snap model.Snapshot, set ClusterSettings, extra []model.CheckItem) model.Report {
	rep := model.Report{Time: snap.CollectedAt, Cluster: snap.Cluster, Version: snap.Topology.Version, Mode: snap.Mode}
	add := func(g, n, r, d string) { rep.Items = append(rep.Items, model.CheckItem{G: g, N: n, R: r, D: d}) }
	ins := snap.Topology.Instances
	by := map[string][]model.Instance{}
	for _, i := range ins {
		by[i.Type] = append(by[i.Type], i)
	}
	// ---- 集群状态
	if len(ins) == 0 {
		add("集群状态", "拓扑可用", "bad", "没有采集到任何实例")
	} else {
		var down []string
		up := 0
		for _, i := range ins {
			if i.Status == "up" || i.Status == "warn" {
				up++
			} else {
				down = append(down, i.ID+"（"+i.Status+"）")
			}
		}
		if len(down) == 0 {
			add("集群状态", "所有组件 Up", "ok", fmt.Sprintf("%d/%d 个实例 Up", up, len(ins)))
		} else {
			add("集群状态", "所有组件 Up", "bad", fmt.Sprintf("%d/%d 个实例 Up；异常：%s", up, len(ins), strings.Join(down, "、")))
		}
	}
	if pd := by["pd"]; len(pd) > 0 {
		up := 0
		leader := ""
		for _, i := range pd {
			if i.Status == "up" || i.Status == "warn" {
				up++
			}
			if i.Leader {
				leader = i.ID
			}
		}
		switch {
		case up*2 <= len(pd):
			add("集群状态", "PD 健康 / 多数派", "bad", fmt.Sprintf("%d/%d 个 PD 健康，已失去多数派", up, len(pd)))
		case up < len(pd):
			add("集群状态", "PD 健康 / 多数派", "warn", fmt.Sprintf("%d/%d 个 PD 健康，仍有多数派但容错下降", up, len(pd)))
		default:
			add("集群状态", "PD 健康 / 多数派", "ok", fmt.Sprintf("%d/%d 成员健康，Leader %s", up, len(pd), orDash(leader)))
		}
		if len(pd) < 3 || len(pd)%2 == 0 {
			add("集群状态", "PD 数量（奇数且 ≥ 3）", "warn", fmt.Sprintf("当前 %d 个", len(pd)))
		} else {
			add("集群状态", "PD 数量（奇数且 ≥ 3）", "ok", fmt.Sprintf("%d 个", len(pd)))
		}
	}
	// ---- 存储
	tikv := by["tikv"]
	if len(tikv) > 0 {
		var hi, mid []string
		var sumR, sumL, sumW float64
		for _, i := range tikv {
			switch {
			case i.Disk >= 80:
				hi = append(hi, fmt.Sprintf("%s：%.0f%%", i.ID, i.Disk))
			case i.Disk >= 70:
				mid = append(mid, fmt.Sprintf("%s：%.0f%%", i.ID, i.Disk))
			}
			sumR += float64(i.Regions)
			sumL += float64(i.Leaders)
			sumW += i.WR
		}
		switch {
		case len(hi) > 0:
			add("存储", "TiKV 磁盘使用率 < 80%", "bad", strings.Join(hi, "、"))
		case len(mid) > 0:
			add("存储", "TiKV 磁盘使用率 < 80%", "warn", "接近水位："+strings.Join(mid, "、"))
		default:
			add("存储", "TiKV 磁盘使用率 < 80%", "ok", fmt.Sprintf("最高 %.0f%%", maxOf(tikv, func(i model.Instance) float64 { return i.Disk })))
		}
		n := float64(len(tikv))
		skew := func(f func(model.Instance) float64, avg float64) (string, float64) {
			id, mx := "", 0.0
			for _, i := range tikv {
				if v := f(i); v > mx {
					mx, id = v, i.ID
				}
			}
			if avg <= 0 {
				return id, 0
			}
			return id, mx / avg
		}
		if idr, r := skew(func(i model.Instance) float64 { return float64(i.Regions) }, sumR/n); sumR > 0 && r >= 1.3 {
			add("存储", "Region 分布均衡度", "warn", fmt.Sprintf("%s 的 Region 数是均值的 %.1f×", idr, r))
		} else if sumR > 0 {
			add("存储", "Region 分布均衡度", "ok", fmt.Sprintf("最大偏差 %.2f×", r))
		}
		if idw, w := skew(func(i model.Instance) float64 { return i.WR }, sumW/n); sumW > 0 && w >= 3 {
			add("存储", "写流量均衡度", "warn", fmt.Sprintf("%s 写流量是均值的 %.1f×，疑似写热点", idw, w))
		} else if sumW > 0 {
			add("存储", "写流量均衡度", "ok", fmt.Sprintf("最大偏差 %.1f×", w))
		}
		if set.Replicas > 0 && len(tikv) < set.Replicas {
			add("存储", "TiKV 数量 ≥ 副本数", "bad", fmt.Sprintf("TiKV %d 个 < max-replicas %d", len(tikv), set.Replicas))
		} else if set.Replicas > 0 {
			add("存储", "TiKV 数量 ≥ 副本数", "ok", fmt.Sprintf("TiKV %d 个，max-replicas=%d", len(tikv), set.Replicas))
		}
	}
	if snap.Overview.StoresTotal > 0 {
		if snap.Overview.StoresUp < snap.Overview.StoresTotal {
			add("存储", "PD Store 状态", "bad", fmt.Sprintf("%d/%d Store Up", snap.Overview.StoresUp, snap.Overview.StoresTotal))
		} else {
			add("存储", "PD Store 状态", "ok", fmt.Sprintf("%d/%d Store Up，Region 共 %s", snap.Overview.StoresUp, snap.Overview.StoresTotal, commas(snap.Overview.Regions)))
		}
	}
	// ---- 性能
	if snap.Overview.P99ms > 0 {
		r := "ok"
		switch {
		case snap.Overview.P99ms >= 500:
			r = "bad"
		case snap.Overview.P99ms >= 100:
			r = "warn"
		}
		add("性能", "P99 延迟", r, fmt.Sprintf("%.1f ms（较 1 小时前 %+.1f ms）", snap.Overview.P99ms, snap.Overview.P99ChangeMs))
	}
	if len(snap.Slow) > 0 {
		total := 0
		for _, g := range snap.Slow {
			total += g.Exec
		}
		r := "warn"
		if len(snap.Slow) <= 2 && total < 100 {
			r = "ok"
		}
		add("性能", "慢查询（近 1 小时）", r, fmt.Sprintf("%d 类 SQL；最慢 %s", len(snap.Slow), slowTop(snap.Slow)))
	} else {
		add("性能", "慢查询（近 1 小时）", "ok", "没有慢查询")
	}
	// ---- 资源
	if tidb := by["tidb"]; len(tidb) > 0 {
		var hot []string
		for _, i := range tidb {
			if i.Mem >= 80 {
				hot = append(hot, fmt.Sprintf("%s：%.0f%%", i.ID, i.Mem))
			}
		}
		if len(hot) > 0 {
			add("资源", "TiDB 内存使用率 < 80%", "warn", strings.Join(hot, "、"))
		} else {
			add("资源", "TiDB 内存使用率 < 80%", "ok", fmt.Sprintf("最高 %.0f%%", maxOf(tidb, func(i model.Instance) float64 { return i.Mem })))
		}
	}
	if len(tikv) > 0 {
		var hot []string
		for _, i := range tikv {
			if i.CPU >= 85 {
				hot = append(hot, fmt.Sprintf("%s：%.0f%%", i.ID, i.CPU))
			}
		}
		if len(hot) > 0 {
			add("资源", "TiKV CPU 使用率 < 85%", "warn", strings.Join(hot, "、"))
		} else {
			add("资源", "TiKV CPU 使用率 < 85%", "ok", fmt.Sprintf("最高 %.0f%%", maxOf(tikv, func(i model.Instance) float64 { return i.CPU })))
		}
	}
	// ---- 告警
	crit, warn := snap.Overview.AlertsCrit, snap.Overview.AlertsWarn
	switch {
	case crit > 0:
		add("告警", "活跃告警", "bad", fmt.Sprintf("严重 %d 条，警告 %d 条", crit, warn))
	case warn > 0:
		add("告警", "活跃告警", "warn", fmt.Sprintf("警告 %d 条", warn))
	default:
		add("告警", "活跃告警", "ok", "无")
	}
	// ---- 配置
	if set.Replicas > 0 {
		r := "ok"
		if set.Replicas != 3 && set.Replicas != 5 {
			r = "warn"
		}
		add("配置", "副本数 max-replicas", r, fmt.Sprint(set.Replicas))
	}
	if set.PDDownMin > 0 {
		r := "ok"
		if set.PDDownMin < 10 || set.PDDownMin > 180 {
			r = "warn"
		}
		add("配置", "max-store-down-time", r, fmt.Sprintf("%d 分钟", set.PDDownMin))
	}
	// ---- 合规
	if v := snap.Topology.Version; v != "" {
		if VersionSupported(v) {
			add("合规", "版本在平台支持范围内", "ok", v)
		} else {
			add("合规", "版本在平台支持范围内", "warn", v+"：平台写操作将降级为只读（支持 v7.5 / v8.1 / v8.5 系列）")
		}
	}
	// ---- 诊断发现汇总
	nbad := 0
	for _, f := range snap.Findings {
		if f.Sev == "bad" || f.Sev == "critical" {
			nbad++
		}
	}
	if len(snap.Findings) > 0 {
		r := "warn"
		if nbad > 0 {
			r = "bad"
		}
		add("诊断", "规则诊断发现", r, fmt.Sprintf("%d 条（其中严重 %d 条）", len(snap.Findings), nbad))
	}
	// ---- 数据保护（平台目前没有只读通道获取备份历史）
	add("数据保护", "最近一次全量备份", "na", "平台未接入备份历史（通过「备份与恢复」页的 PITR 状态与平台发起的备份记录查看）")

	rep.Items = append(rep.Items, extra...)
	order := map[string]int{"集群状态": 0, "存储": 1, "性能": 2, "资源": 3, "告警": 4, "配置": 5, "诊断": 6, "数据保护": 7, "主机": 8, "合规": 9}
	sort.SliceStable(rep.Items, func(i, j int) bool { return order[rep.Items[i].G] < order[rep.Items[j].G] })
	for _, it := range rep.Items {
		switch it.R {
		case "ok":
			rep.Ok++
		case "warn":
			rep.Warn++
		case "bad":
			rep.Bad++
		default:
			rep.Na++
		}
	}
	return rep
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func maxOf(ns []model.Instance, f func(model.Instance) float64) float64 {
	m := 0.0
	for _, n := range ns {
		if v := f(n); v > m {
			m = v
		}
	}
	return m
}

func slowTop(g []model.SlowGroup) string {
	top := g[0]
	for _, x := range g {
		if x.Avg > top.Avg {
			top = x
		}
	}
	return fmt.Sprintf("%.1fs（%s）", top.Avg, top.Digest)
}

// ParseCheckOutput 解析 `tiup cluster check` 的结果表（Node / Check / Result / Message），转成巡检项。
func ParseCheckOutput(out string) []model.CheckItem {
	var items []model.CheckItem
	type agg struct {
		pass, warn, fail int
		msgs             []string
	}
	m := map[string]*agg{}
	var order []string
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		res := f[2]
		if res != "Pass" && res != "Warn" && res != "Fail" {
			continue
		}
		chk := f[1]
		a := m[chk]
		if a == nil {
			a = &agg{}
			m[chk] = a
			order = append(order, chk)
		}
		switch res {
		case "Pass":
			a.pass++
		case "Warn":
			a.warn++
			a.msgs = append(a.msgs, f[0]+" "+strings.Join(f[3:], " "))
		case "Fail":
			a.fail++
			a.msgs = append(a.msgs, f[0]+" "+strings.Join(f[3:], " "))
		}
	}
	for _, chk := range order {
		a := m[chk]
		r := "ok"
		d := fmt.Sprintf("%d 台通过", a.pass)
		if a.fail > 0 {
			r = "bad"
		} else if a.warn > 0 {
			r = "warn"
		}
		if len(a.msgs) > 0 {
			if len(a.msgs) > 3 {
				a.msgs = append(a.msgs[:3], fmt.Sprintf("…等 %d 项", len(a.msgs)))
			}
			d = fmt.Sprintf("通过 %d / 警告 %d / 失败 %d：%s", a.pass, a.warn, a.fail, strings.Join(a.msgs, "；"))
		}
		items = append(items, model.CheckItem{G: "主机", N: chk, R: r, D: d})
	}
	return items
}
