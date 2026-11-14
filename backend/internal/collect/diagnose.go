// @author: kevinkelly
// @license: Apache License 2.0
// @file: diagnose.go
// @time: 2026/10/28

package collect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tidbops/internal/model"
)

// Diagnose 是基础诊断规则引擎（路线图 C1 的第一版）：规则基于官方「故障排查地图」与「性能调优方法」，
// 输入是已采集的拓扑 / 热点 / 慢 SQL / 告警，输出带证据、根因、建议命令的诊断结果。
func Diagnose(topo model.Topology, storeWrite map[uint64]float64, hot []model.HotRegion, slow []model.SlowGroup, alerts []model.Alert) []model.Finding {
	var out []model.Finding
	add := func(f model.Finding) { out = append(out, f) }
	var tikv []model.Instance
	for _, in := range topo.Instances {
		if in.Type == "tikv" && in.Status != "down" && in.Status != "offline" {
			tikv = append(tikv, in)
		}
	}

	// R1 实例状态异常
	for _, in := range topo.Instances {
		if in.Status == "down" || in.Status == "offline" {
			add(model.Finding{ID: "inst-" + in.ID, Sev: "critical", Title: fmt.Sprintf("%s 状态异常（%s）", in.ID, map[string]string{"down": "已停止 / 失联", "offline": "下线中"}[in.Status]),
				Scope: in.ID, Evidence: []string{fmt.Sprintf("%s:%d 状态 %s", in.Host, in.Port, in.Status)},
				Cause:   "实例进程退出、主机故障、网络隔离，或正处于计划内下线 / 维护。",
				Actions: []model.Tip{{T: "查看实例日志与进程状态", Cmd: fmt.Sprintf("tiup cluster display %s -N %s:%d", topo.Cluster, in.Host, in.Port)}, {T: "确认是否在计划内维护；TiKV 失联超过 max-store-down-time（默认 30 分钟）PD 会开始补副本"}},
				Link:    model.Link{Page: "nodes", Label: "查看节点", Args: map[string]string{"tab": "inst", "node": in.ID}}, Ref: "tidb-troubleshooting-map"})
		}
	}
	// R2 TiKV 磁盘水位
	var avgDisk float64
	for _, in := range tikv {
		avgDisk += in.Disk / float64(len(tikv))
	}
	for _, in := range tikv {
		if in.Disk >= 80 {
			add(model.Finding{ID: "disk-" + in.ID, Sev: "critical", Title: fmt.Sprintf("%s 磁盘使用率 %.0f%%，超过 80%% 水位", in.ID, in.Disk), Scope: in.ID,
				Evidence: []string{fmt.Sprintf("/data 使用率 %.1f%%（阈值 80%%）", in.Disk), fmt.Sprintf("PD 对该 Store 的调度打分下降，副本补齐与均衡变慢；集群 TiKV 平均 %.0f%%", avgDisk)},
				Cause:    "数据倾斜或未及时扩容。使用率 ≥ 80% 时副本补齐、Region 均衡受限，≥ 90% 有写入失败风险。",
				Actions: []model.Tip{{T: "扩容 TiKV，让 PD 自动均衡", Cmd: fmt.Sprintf("tiup cluster scale-out %s scale-out-tikv.yml", topo.Cluster)},
					{T: "确认 GC 正常推进，清理历史版本", Cmd: "SELECT * FROM mysql.tidb WHERE variable_name LIKE 'tikv_gc%';"}},
				Link: model.Link{Page: "nodes", Label: "去扩容 TiKV", Args: map[string]string{"tab": "scaleout", "role": "tikv"}}, Ref: "tidb-troubleshooting-map"})
		}
	}
	// R3 写热点：某 TiKV 写流量远高于其他节点
	if len(tikv) >= 3 {
		for _, in := range tikv {
			wr := in.WR
			if w, ok := storeWrite[in.SID]; ok && w > wr {
				wr = w
			}
			var others, n float64
			for _, o := range tikv {
				if o.ID != in.ID {
					ow := o.WR
					if w, ok := storeWrite[o.SID]; ok && w > ow {
						ow = w
					}
					others += ow
					n++
				}
			}
			avg := others / n
			if wr >= 20 && wr >= 3*avg {
				var tbls []string
				seen := map[string]bool{}
				for _, h := range hot {
					if h.Kind == "写热点" && h.Store == in.SID && h.Table != "" && !seen[h.Table] {
						seen[h.Table] = true
						tbls = append(tbls, h.Table)
					}
				}
				ev := []string{fmt.Sprintf("%s 写流量 %.0f MB/s，其余 TiKV 平均 %.0f MB/s（%.1f×）", in.ID, wr, avg, wr/maxF(avg, 0.1))}
				if in.RaftCPU > 0 {
					ev = append(ev, fmt.Sprintf("Raftstore 线程 CPU %.0f%%（按单线程折算）", in.RaftCPU))
				}
				if len(tbls) > 0 {
					ev = append(ev, "热点 Region 所属表："+strings.Join(tbls, "、"))
				}
				acts := []model.Tip{{T: "无整数主键的表用 SHARD_ROW_ID_BITS 打散 RowID", Cmd: "ALTER TABLE <表名> SHARD_ROW_ID_BITS = 4;  -- 16 个分片"},
					{T: "有整数主键、写入顺序递增的表改用 AUTO_RANDOM（新表 / 重建表）", Cmd: "CREATE TABLE <表名> (id BIGINT PRIMARY KEY AUTO_RANDOM, ...);"},
					{T: "临时止血：手动拆分热点 Region", Cmd: "tiup ctl:<版本> pd -u <PD地址> operator add split-region <region_id> --policy=approximate"}}
				add(model.Finding{ID: "hotw-" + in.ID, Sev: "critical", Title: fmt.Sprintf("%s 疑似写入热点", in.ID), Scope: in.ID, Evidence: ev,
					Cause:   "顺序递增主键 / 自增 RowID 使新写入集中到同一个 Region，单个 TiKV 的 Raftstore 线程成为瓶颈。扩容不能缓解单 Region 热点。",
					Actions: acts, Link: model.Link{Page: "hotspot", Label: "查看热点分析"}, Ref: "troubleshoot-hot-spot-issues"})
			}
		}
	}
	// R4 TiDB 内存 / CPU
	for _, in := range topo.Instances {
		if in.Type != "tidb" {
			continue
		}
		if in.Mem >= 80 || in.CPU >= 85 {
			add(model.Finding{ID: "tidbres-" + in.ID, Sev: "warning", Title: fmt.Sprintf("%s 资源使用偏高（内存 %.0f%% / CPU %.0f%%）", in.ID, in.Mem, in.CPU), Scope: in.ID,
				Evidence: []string{fmt.Sprintf("内存 %.1f%%，CPU %.1f%%，QPS %.0f，连接 %d", in.Mem, in.CPU, in.QPS, in.Conn)},
				Cause:    "个别大查询（Join 算法 / 排序）占用内存，或该节点分担的流量偏高。v3.0+ 可在日志里搜索 expensive_query 定位。",
				Actions: []model.Tip{{T: "限制单条 SQL 内存并超限取消", Cmd: "SET GLOBAL tidb_mem_quota_query = 2147483648;\nSET GLOBAL tidb_mem_oom_action = 'CANCEL';"},
					{T: "在日志中定位大查询", Cmd: "grep expensive_query /tidb-deploy/tidb-4000/log/tidb.log | tail"}},
				Link: model.Link{Page: "sql", Label: "查看 Top SQL"}, Ref: "tidb-troubleshooting-map"})
		}
	}
	// R5 Region 分布不均
	if len(tikv) >= 3 {
		minR, maxR := tikv[0].Regions, tikv[0].Regions
		for _, in := range tikv {
			if in.Regions < minR {
				minR = in.Regions
			}
			if in.Regions > maxR {
				maxR = in.Regions
			}
		}
		if minR > 0 && float64(maxR)/float64(minR) > 1.25 {
			add(model.Finding{ID: "balance", Sev: "warning", Title: "TiKV 之间 Region 分布不均", Scope: "tikv",
				Evidence: []string{fmt.Sprintf("Region 数最多 %d，最少 %d（%.2f×）", maxR, minR, float64(maxR)/float64(minR))},
				Cause:    "刚扩缩容、调度限速过严，或存在磁盘水位过高的 Store 影响了均衡。",
				Actions:  []model.Tip{{T: "查看调度配置与 Store 打分", Cmd: "tiup ctl:<版本> pd -u <PD地址> config show\ntiup ctl:<版本> pd -u <PD地址> store"}},
				Link:     model.Link{Page: "arch", Label: "查看拓扑"}, Ref: "tidb-scheduling"})
		}
	}
	// R6 慢 SQL
	n := 0
	for _, g := range slow {
		if g.Tag == "缺索引 / 统计信息过期" || g.Tag == TagKVQueue || g.Tag == "大查询占用内存高" || g.Tag == "索引列上使用函数" {
			if n++; n > 4 {
				break
			}
			sev := "warning"
			if g.Tag == "大查询占用内存高" || g.Tag == TagKVQueue { // 排队等待只是症状（多半被别的语句拖慢），不当作需要立刻处理的警告
				sev = "info"
			}
			sql := g.SQL
			if len(sql) > 90 {
				sql = sql[:90] + "…"
			}
			add(model.Finding{ID: "slow-" + g.Digest, Sev: sev, Title: fmt.Sprintf("慢 SQL（%s）：%s", g.Tag, sql), Scope: g.DB + "." + g.User,
				Evidence: []string{fmt.Sprintf("平均 %.2fs / P99 %.2fs，累计 %.0fs，执行 %d 次", g.Avg, g.P99, g.Total, g.Exec), fmt.Sprintf("扫描 Key %.0f，处理 Key %.0f，返回 %.0f 行", g.Keys, g.PKeys, g.Rows)},
				Cause:    "详见建议；根因由慢日志中的 Total_keys / Process_keys / Wait_time / Mem_max 与 SQL 文本特征推断。",
				Actions:  g.Tips, Link: model.Link{Page: "sql", Label: "查看该 SQL"}, Ref: "sql-tuning-overview"})
		}
	}
	// R7 严重告警汇总
	crit := 0
	for _, a := range alerts {
		if a.Lv == "critical" {
			crit++
		}
	}
	if crit > 0 {
		var names []string
		for _, a := range alerts {
			if a.Lv == "critical" {
				names = append(names, fmt.Sprintf("%s（%s）", a.Name, a.Inst))
			}
		}
		add(model.Finding{ID: "alerts", Sev: "info", Title: fmt.Sprintf("%d 条严重告警正在触发", crit), Scope: "Alertmanager", Evidence: names,
			Cause: "告警来自 Prometheus 规则；上面的诊断项已覆盖其中的大部分原因。", Actions: []model.Tip{{T: "在 Alertmanager / Grafana 中确认告警详情并跟进"}},
			Link: model.Link{Page: "overview", Label: "查看总览"}, Ref: "dashboard-overview"})
	}
	sortFindings(out)
	return out
}

// Snapshot 采集全部数据并汇总。单类数据失败只记录说明，不影响其他部分。
func (c *Collector) Snapshot(ctx context.Context, mode string) model.Snapshot {
	snap := model.Snapshot{Mode: mode, CollectedAt: time.Now()}
	topo, errs := c.Topology(ctx)
	snap.Errors = append(snap.Errors, errs...)
	snap.Alerts, _ = c.Alerts(ctx)
	if _, err := c.get(ctx, "alert/alerts"); err != nil {
		snap.Errors = append(snap.Errors, "告警不可用（未配置 Alertmanager 或无法访问）: "+err.Error())
	}
	hot, storeWrite, err := c.Hot(ctx)
	if err != nil {
		snap.Errors = append(snap.Errors, "热点数据: "+err.Error())
	}
	// 热点所在 Store → 实例，补全写流量与 Leader 名
	idBySID := map[uint64]string{}
	for i := range topo.Instances {
		in := &topo.Instances[i]
		if in.SID != 0 {
			idBySID[in.SID] = in.ID
		}
		if w, ok := storeWrite[in.SID]; ok && in.Type == "tikv" && in.WR == 0 {
			in.WR = round1(w)
		}
	}
	for i := range hot {
		hot[i].Leader = idBySID[hot[i].Store]
	}
	slow, slowExt, err := c.slowWithExt(ctx)
	if err != nil {
		snap.Errors = append(snap.Errors, "慢查询: "+err.Error())
	}
	ov, tr, e2 := c.Overview(ctx, topo, snap.Alerts)
	snap.Errors = append(snap.Errors, e2...)
	snap.Topology, snap.Overview, snap.Trends, snap.Slow, snap.Hot = topo, ov, tr, slow, hot
	snap.Cluster = topo.Cluster
	snap.DBTime = c.DBTime(ctx)
	findings := Diagnose(topo, storeWrite, hot, slow, snap.Alerts)
	findings = append(findings, DiagnoseDBTime(snap.DBTime, ov.QPS)...)
	findings = append(findings, DiagnoseForecast(c.forecastCached(ctx))...)
	findings = append(findings, DiagnoseSlowExt(slow, slowExt)...)
	findings = append(findings, DiagnoseSignals(topo, c.Signals(ctx))...)
	snap.Findings = c.sustain(mode, findings)
	snap.Env = c.Env(ctx, topo)
	if snap.Alerts == nil {
		snap.Alerts = []model.Alert{}
	}
	if snap.Slow == nil {
		snap.Slow = []model.SlowGroup{}
	}
	if snap.Hot == nil {
		snap.Hot = []model.HotRegion{}
	}
	return snap
}
