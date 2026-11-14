// @author: kevinkelly
// @license: Apache License 2.0
// @file: wizard_api.go
// @time: 2026/10/28

package server

// 排障向导的实时证据：用最近一次快照为向导（前端 data.js 的 WIZARD 决策树）里「能被快照回答」的问题给出建议答案。
//
//	GET /api/wizard/evidence
//	  → {"evidence":{"<症状>/<节点id>":{"answer":"yes|no|unknown","why":"依据（含具体数据）","ref":"页面跳转 id（可选）"}},
//	     "facts":[{"id":..,"level":"bad|warn|ok","text":..,"ref":..}], "collectedAt":.., "mode":..}
//
// 只读接口：运维模式要求登录（view.overview），只读模式直接放行。
// 规则表驱动：key 与前端 WIZARD 的「症状/节点 id」一一对应（节点 id 以 data.js 为准）；
// 不能用快照判断的问题（日志、Grafana 面板里的延迟直方图等）不返回，前端仍由用户自己回答。
// 建议答案只是证据，不替用户跳步：最终选择仍由用户点击。

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"tidbops/internal/authz"
	"tidbops/internal/model"
)

// wizEvidence 是一个向导问题的建议答案。
type wizEvidence struct {
	Answer string `json:"answer"` // yes | no | unknown
	Why    string `json:"why"`
	Ref    string `json:"ref,omitempty"`
}

// wizFact 是与具体问题无关的实时线索，向导页可在问题上方展示。
type wizFact struct {
	ID    string `json:"id"`
	Level string `json:"level"` // bad | warn | ok
	Text  string `json:"text"`
	Ref   string `json:"ref,omitempty"`
}

// wizRule 把一个向导问题映射到快照判断。Eval 返回 ok=false 表示「快照里没有足够信息」，此时不下发该问题。
type wizRule struct {
	Key  string // "症状/节点id"
	Q    string // 对应的问题（仅供维护者对照 data.js）
	Eval func(snap model.Snapshot) (wizEvidence, bool)
}

func yes(why, ref string) (wizEvidence, bool)     { return wizEvidence{"yes", why, ref}, true }
func no(why, ref string) (wizEvidence, bool)      { return wizEvidence{"no", why, ref}, true }
func unknown(why, ref string) (wizEvidence, bool) { return wizEvidence{"unknown", why, ref}, true }

// 阈值（与诊断引擎 collect/diagnose.go 的口径一致）。
const (
	wizRaftSkew     = 2.0  // Raftstore CPU：最高节点 ≥ 其余节点平均的 2 倍，且最高 ≥ wizRaftMin%，视为「只有个别节点很高」
	wizRaftMin      = 30.0 // 低于该值时，即使有倍数差也不算「很高」
	wizRaftBalanced = 1.5  // 最高 ≤ 其余平均的 1.5 倍，视为均衡
	wizRaftNear100  = 90.0 // Raftstore 线程 CPU 接近 100%
	wizWriteSkew    = 3.0  // 写流量：最高节点 ≥ 其余平均的 3 倍
	wizWriteMin     = 20.0 // 且 ≥ 20 MB/s
	wizCompileHigh  = 30.0 // Compile 占 DB Time ≥ 30% 视为很高
	wizCompileLow   = 15.0 // ≤ 15% 视为不高
)

func liveTiKV(snap model.Snapshot) []model.Instance {
	var out []model.Instance
	for _, in := range snap.Topology.Instances {
		if in.Type == "tikv" && in.Status != "down" && in.Status != "offline" {
			out = append(out, in)
		}
	}
	return out
}

// topVsOthers 返回 vals 中最大值所在下标、该最大值，以及其余各项的平均值。
func topVsOthers(vals []float64) (idx int, top, othersAvg float64) {
	for i, v := range vals {
		if v > top || i == 0 {
			idx, top = i, v
		}
	}
	if len(vals) > 1 {
		for i, v := range vals {
			if i != idx {
				othersAvg += v / float64(len(vals)-1)
			}
		}
	}
	return
}

func ratioStr(top, avg float64) string {
	if avg <= 0 {
		return "远高于"
	}
	return fmt.Sprintf("%.1f×", top/avg)
}

func alertMatch(snap model.Snapshot, subs ...string) (model.Alert, bool) {
	for _, a := range snap.Alerts {
		low := strings.ToLower(a.Name + " " + a.Desc)
		for _, s := range subs {
			if strings.Contains(low, strings.ToLower(s)) {
				return a, true
			}
		}
	}
	return model.Alert{}, false
}

func alertText(a model.Alert) string {
	t := fmt.Sprintf("告警 %s（%s）", a.Name, a.Inst)
	if a.Time != "" {
		t = fmt.Sprintf("告警 %s（%s，%s）", a.Name, a.Inst, a.Time)
	}
	if a.Desc != "" {
		t += "：" + a.Desc
	}
	return t
}

// 写入变慢 / w1：Raftstore CPU 是否只有个别节点很高。同时参考诊断引擎的「写热点」结论与各 TiKV 写流量。
func evalRaftSkew(snap model.Snapshot) (wizEvidence, bool) {
	for _, f := range snap.Findings {
		if strings.HasPrefix(f.ID, "hotw-") {
			why := "诊断引擎判定：" + f.Title
			if len(f.Evidence) > 0 {
				why += "（" + strings.Join(f.Evidence[:min(len(f.Evidence), 2)], "；") + "）"
			}
			return yes(why, "hotspot")
		}
	}
	tikv := liveTiKV(snap)
	if len(tikv) < 3 {
		return wizEvidence{}, false
	}
	cpu, wr := make([]float64, len(tikv)), make([]float64, len(tikv))
	haveCPU := false
	for i, in := range tikv {
		cpu[i], wr[i] = in.RaftCPU, in.WR
		if in.RaftCPU > 0 {
			haveCPU = true
		}
	}
	if i, top, avg := topVsOthers(wr); top >= wizWriteMin && top >= wizWriteSkew*avg {
		return yes(fmt.Sprintf("%s 写流量 %.0f MB/s，其余 TiKV 平均 %.0f MB/s（%s）", tikv[i].ID, top, avg, ratioStr(top, avg)), "hotspot")
	}
	if !haveCPU {
		return wizEvidence{}, false
	}
	i, top, avg := topVsOthers(cpu)
	switch {
	case top >= wizRaftMin && top >= wizRaftSkew*avg:
		return yes(fmt.Sprintf("%s 的 Raftstore 线程 CPU %.0f%%，其余 TiKV 平均 %.0f%%（%s）", tikv[i].ID, top, avg, ratioStr(top, avg)), "hotspot")
	case top < wizRaftMin:
		return no(fmt.Sprintf("各 TiKV 的 Raftstore 线程 CPU 都不高（最高 %s %.0f%%）", tikv[i].ID, top), "hotspot")
	case top <= wizRaftBalanced*avg:
		return no(fmt.Sprintf("各 TiKV 的 Raftstore 线程 CPU 较均衡（最高 %s %.0f%%，其余平均 %.0f%%）", tikv[i].ID, top, avg), "hotspot")
	}
	return unknown(fmt.Sprintf("%s 的 Raftstore 线程 CPU %.0f%%，高于其余 TiKV 平均 %.0f%%，但差距未到明显程度，请对照 Grafana 判断", tikv[i].ID, top, avg), "hotspot")
}

// 写入变慢 / w2、TiKV server is busy / b1：是否出现 Stalling。快照里没有日志，只能依据告警。
func evalStall(snap model.Snapshot) (wizEvidence, bool) {
	if a, ok := alertMatch(snap, "stall"); ok {
		return yes("快照中有 write stall 相关的"+alertText(a), "diag")
	}
	return wizEvidence{}, false
}

// TiKV server is busy / b3：raftstore 线程 CPU 接近 100%（channel full 只出现在日志里，快照无法判断）。
func evalRaftBusy(snap model.Snapshot) (wizEvidence, bool) {
	tikv := liveTiKV(snap)
	var worst model.Instance
	haveCPU := false
	for _, in := range tikv {
		if in.RaftCPU > 0 {
			haveCPU = true
		}
		if in.RaftCPU > worst.RaftCPU {
			worst = in
		}
	}
	if haveCPU && worst.RaftCPU >= wizRaftNear100 {
		return yes(fmt.Sprintf("%s 的 Raftstore 线程 CPU %.0f%%，接近 100%%", worst.ID, worst.RaftCPU), "hotspot")
	}
	if a, ok := alertMatch(snap, "raftstore_thread_cpu"); ok {
		return yes("Raftstore 线程 CPU 已触发"+alertText(a), "diag")
	}
	if haveCPU && worst.RaftCPU < 70 {
		return no(fmt.Sprintf("各 TiKV 的 Raftstore 线程 CPU 最高 %s %.0f%%，没有接近 100%%，也没有相关告警（channel full 需查看日志确认）", worst.ID, worst.RaftCPU), "hotspot")
	}
	if haveCPU {
		return unknown(fmt.Sprintf("%s 的 Raftstore 线程 CPU %.0f%%，偏高但未到 90%%，也没有相关告警；channel full 需查看日志确认", worst.ID, worst.RaftCPU), "hotspot")
	}
	return wizEvidence{}, false
}

// 延迟升高 / l2：尖峰时刻是否同时发生了 PD Leader 切换 / TiKV 掉 Leader。
// 快照没有历史切换记录，能确认「有」（告警、实例已停止），不能确认「没有」。
func evalLeaderChange(snap model.Snapshot) (wizEvidence, bool) {
	if a, ok := alertMatch(snap, "leader_change", "leader change"); ok {
		return yes("存在 Leader 切换的"+alertText(a), "diag")
	}
	var downs []string
	for _, in := range snap.Topology.Instances {
		if (in.Type == "tikv" || in.Type == "pd") && (in.Status == "down" || in.Status == "offline") {
			downs = append(downs, in.ID)
		}
	}
	if len(downs) > 0 {
		return yes(strings.Join(downs, "、")+" 已停止 / 下线，其上的 Leader 会被迫迁移", "nodes")
	}
	if len(snap.Topology.Instances) == 0 {
		return wizEvidence{}, false
	}
	return unknown("快照里没有 PD Leader 切换告警，也没有 TiKV / PD 实例停止；但快照不含切换历史，无法排除，请对照 PD 与 TiKV 日志", "diag")
}

// 延迟升高 / l6：DB Time 中 Compile 占比是否很高。
func evalCompile(snap model.Snapshot) (wizEvidence, bool) {
	for _, it := range snap.DBTime.ByStage {
		if it.Name != "Compile" {
			continue
		}
		switch {
		case it.Pct >= wizCompileHigh:
			return yes(fmt.Sprintf("Compile 占 DB Time %.0f%%（≥ %.0f%% 视为很高）", it.Pct, wizCompileHigh), "overview")
		case it.Pct <= wizCompileLow:
			return no(fmt.Sprintf("Compile 仅占 DB Time %.0f%%", it.Pct), "overview")
		}
		return unknown(fmt.Sprintf("Compile 占 DB Time %.0f%%，介于 %.0f%%–%.0f%% 之间，请结合 Plan Cache 命中率判断", it.Pct, wizCompileLow, wizCompileHigh), "overview")
	}
	return wizEvidence{}, false
}

// wizRules 是规则表。新增向导节点时，在这里加一行，key 用 data.js 里的「症状/节点 id」。
var wizRules = []wizRule{
	{"写入变慢/w1", "Raftstore CPU 是否只有个别节点很高（与其他节点差距明显）？", evalRaftSkew},
	{"写入变慢/w2", "RocksDB 日志是否出现 Stalling，或 Write Stall 面板有数据？", evalStall},
	{"TiKV server is busy/b1", "日志中是否有 \"Stalling\" 字样？", evalStall},
	{"TiKV server is busy/b3", "日志出现 channel full 或 raftstore 线程 CPU 接近 100%？", evalRaftBusy},
	{"延迟升高/l2", "尖峰时刻是否同时发生了 PD Leader 切换 / TiKV 掉 Leader？", evalLeaderChange},
	{"延迟升高/l6", "DB Time 中 Compile 占比是否很高（橙色）？", evalCompile},
	// 不能由快照回答（没有对应数据）的节点：写入变慢/w3（WAL Sync Duration）、写入变慢/w4（Apply / Propose 耗时）、
	// TiKV server is busy/b2（latch wait）、延迟升高/l1（尖峰还是持续，需要看时间序列）、延迟升高/l5（TiDB–TiKV 延迟差）。
}

// wizardEvidence 对快照逐条求值规则表。
func wizardEvidence(snap model.Snapshot) map[string]wizEvidence {
	out := map[string]wizEvidence{}
	for _, r := range wizRules {
		if ev, ok := r.Eval(snap); ok {
			out[r.Key] = ev
		}
	}
	return out
}

// wizardFacts 汇总与具体问题无关的实时线索：实例停止、磁盘水位、TiDB 内存、严重告警、全表扫描慢 SQL、连接数、写热点。
func wizardFacts(snap model.Snapshot) []wizFact {
	var out []wizFact
	var downs []string
	var diskHigh, memHigh []string
	conn := 0
	for _, in := range snap.Topology.Instances {
		if in.Status == "down" || in.Status == "offline" {
			downs = append(downs, fmt.Sprintf("%s（%s）", in.ID, in.Status))
		}
		if in.Type == "tikv" && in.Status != "down" && in.Status != "offline" && in.Disk >= 80 {
			diskHigh = append(diskHigh, fmt.Sprintf("%s 磁盘 %.0f%%", in.ID, in.Disk))
		}
		if in.Type == "tidb" {
			conn += in.Conn
			if in.Mem >= 80 {
				memHigh = append(memHigh, fmt.Sprintf("%s 内存 %.0f%%", in.ID, in.Mem))
			}
		}
	}
	if len(downs) > 0 {
		out = append(out, wizFact{"inst-down", "bad", "有实例停止 / 下线：" + strings.Join(downs, "、"), "nodes"})
	} else if len(snap.Topology.Instances) > 0 {
		out = append(out, wizFact{"inst-down", "ok", fmt.Sprintf("全部 %d 个实例状态正常", len(snap.Topology.Instances)), ""})
	}
	if len(diskHigh) > 0 {
		out = append(out, wizFact{"disk-high", "bad", "TiKV 磁盘 ≥ 80%：" + strings.Join(diskHigh, "、"), "nodes"})
	}
	if len(memHigh) > 0 {
		out = append(out, wizFact{"tidb-mem", "warn", "TiDB 内存 ≥ 80%：" + strings.Join(memHigh, "、"), "sql"})
	}
	if snap.Overview.AlertsCrit > 0 || len(snap.Alerts) > 0 {
		var names []string
		for _, a := range snap.Alerts {
			if a.Lv == "critical" {
				names = append(names, a.Name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			out = append(out, wizFact{"alerts-critical", "bad", fmt.Sprintf("%d 条严重告警正在触发：%s", len(names), strings.Join(names, "、")), "diag"})
		}
	}
	scan := 0
	for _, g := range snap.Slow {
		if strings.HasPrefix(g.Index, "—") || strings.Contains(g.Index, "TableFullScan") {
			scan++
		}
	}
	if scan > 0 {
		out = append(out, wizFact{"slow-fullscan", "warn", fmt.Sprintf("慢 SQL 中有 %d 类走全表扫描（共 %d 类）", scan, len(snap.Slow)), "sql"})
	}
	if conn > 0 {
		out = append(out, wizFact{"tidb-conn", "ok", fmt.Sprintf("TiDB 当前连接数合计 %d", conn), ""})
	}
	for _, f := range snap.Findings {
		if strings.HasPrefix(f.ID, "hotw-") {
			out = append(out, wizFact{"hot-write", "bad", f.Title, "hotspot"})
		}
	}
	if out == nil {
		out = []wizFact{}
	}
	return out
}

// registerWizard 注册排障向导的实时证据接口。运维模式与只读模式都会调用；
// read 在运维模式下要求登录并检查能力，只读模式直接放行（u 为 nil）。
func (s *Server) registerWizard(mux *http.ServeMux, read func(authz.Cap, func(w http.ResponseWriter, r *http.Request, u *authz.User)) http.HandlerFunc) {
	mux.HandleFunc("GET /api/wizard/evidence", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		snap := s.Snapshot(r.Context(), false)
		if u != nil {
			snap = s.filterSnapshot(snap, *u) // 与其他读接口一致：按角色裁剪（慢 SQL）
		}
		writeJSON(w, map[string]any{"evidence": wizardEvidence(snap), "facts": wizardFacts(snap), "collectedAt": snap.CollectedAt, "mode": snap.Mode})
	}))
}
