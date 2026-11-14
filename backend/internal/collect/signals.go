// @author: kevinkelly
// @license: Apache License 2.0
// @file: signals.go
// @time: 2026/10/28

package collect

// 诊断第二批：PD / 副本健康、TiKV 写入链路、主机层。数据来自 Prometheus（采集项 prom/pd-*、prom/tikv-*、prom/node-*），
// 规则与阈值取自官方「日常巡检」「告警规则」「TiDB 磁盘 I/O 过高的处理办法」；官方没有给数字的（Apply / Raftstore 线程 CPU 占线程池的比例）是经验值。
// 任何一项取不到（没有对应指标、exporter 版本不同）都当作「没有数据」，不产生诊断项。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"tidbops/internal/model"
)

const (
	pdMissPeerMax      = 100.0 // 缺副本 Region 数（官方告警：> 100）
	pdPendingPeerMax   = 100.0 // pending-peer Region 数（官方：> 100 且持续超过 30 分钟才算异常）
	pdPendingMinutes   = 30
	pdTSOP99Sec        = 0.1 // TSO 请求 P99（官方：> 0.1s）
	pdEtcdFsyncSec     = 1.0 // etcd WAL fsync P99（官方：> 1s）
	pdEtcdRTTSec       = 1.0 // etcd 节点间往返 P99（官方：> 1s）
	pdSlowScore        = 80.0
	tikvLogP99Sec      = 1.0  // append / apply log、scheduler latch 等待 P99（官方：> 1s；正常 append < 50ms、apply < 100ms）
	tikvPoolBusyRatio  = 0.8  // Raftstore / Apply 线程 CPU 占线程池（线程数）的比例；官方以 80% 作为 store-pool-size 默认值下的告警线
	tikvDefaultPool    = 2    // raftstore.store-pool-size / apply-pool-size 的默认值
	hostInodeWarn      = 80.0 // inode 使用率
	hostInodeCrit      = 90.0
	hostReadLatSec     = 0.032 // 磁盘读延迟（官方：> 32ms）
	hostWriteLatSec    = 0.016 // 磁盘写延迟（官方：> 16ms）
	hostLoadPerCore    = 1.0   // 5 分钟负载 / 核数
	hostTCPEstablished = 50000.0
	sigListMax         = 8 // 主机类诊断项证据里最多列出几台
)

// Signals 是第二批规则用到的指标（取最近几个点的平均，pending-peer 取最近 30 分钟的最小值）。
// 按主机归并的 map 以主机 IP 为键。
type Signals struct {
	MissPeer, DownPeer *float64
	PendingPeerMin     *float64 // 最近 30 分钟内的最小值：全程都 > 阈值才算「持续」
	TSOP99             *float64

	EtcdFsync, EtcdRTT, StoreSlow                    map[string]float64
	ApplyCPU, WriteStall, AppendP99, ApplyP99, Latch map[string]float64
	StorePool, ApplyPool                             map[string]int // 来自 cluster_config，缺省按默认值

	Inode, ReadLat, WriteLat, Load, TCP map[string]float64
	ReadOnly                            map[string][]string // 主机 → 只读的挂载点
}

func validVals(s Series) []float64 {
	var out []float64
	for _, p := range s.Points {
		if !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
			out = append(out, p.V)
		}
	}
	return out
}

// lastValid 返回最近 n 个有效点的平均值（跳过 NaN / Inf：histogram_quantile 在没有样本时会返回 NaN）。
func lastValid(s Series, n int) (float64, bool) {
	v := validVals(s)
	if len(v) == 0 {
		return 0, false
	}
	if n > len(v) {
		n = len(v)
	}
	sum := 0.0
	for _, x := range v[len(v)-n:] {
		sum += x
	}
	return sum / float64(n), true
}

// minLast 返回最近 n 个点中的最小值；有效点不足 n*2/3（采集中断 / 刚启动）时认为不足以判断。
func minLast(s Series, n int) (float64, bool) {
	v := validVals(s)
	if len(v) < n*2/3 || len(v) == 0 {
		return 0, false
	}
	if len(v) > n {
		v = v[len(v)-n:]
	}
	m := v[0]
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m, true
}

// hostMax 把序列按主机归并（同一主机多条序列取最大值，如多块盘 / 多个 TiKV 实例）。
func (c *Collector) hostMax(ctx context.Context, name, label string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range c.matrix(ctx, name) {
		key := s.Metric[label]
		if key == "" {
			continue
		}
		v, ok := lastValid(s, 3)
		if !ok {
			continue
		}
		h := hostOf(key)
		if cur, seen := out[h]; !seen || v > cur {
			out[h] = v
		}
	}
	return out
}

func (c *Collector) single(ctx context.Context, name string, last func(Series) (float64, bool)) *float64 {
	var best *float64
	for _, s := range c.matrix(ctx, name) {
		if v, ok := last(s); ok && (best == nil || v > *best) {
			vv := v
			best = &vv
		}
	}
	return best
}

// poolSizes 从 sql/cluster-config 取各 TiKV 的线程池大小（按主机）；取不到的主机按默认值处理。
func (c *Collector) poolSizes(ctx context.Context, key string) map[string]int {
	b, err := c.get(ctx, "sql/cluster-config")
	if err != nil {
		return nil
	}
	_, rows := parseTSV(b)
	out := map[string]int{}
	for _, r := range rows {
		if strings.EqualFold(r["type"], "tikv") && r["key"] == key {
			if n := atoi(r["value"]); n > 0 {
				out[hostOf(r["instance"])] = n
			}
		}
	}
	return out
}

// Signals 采集第二批规则用的指标。
func (c *Collector) Signals(ctx context.Context) Signals {
	sg := Signals{
		MissPeer:       c.single(ctx, "prom/pd-miss-peer", func(s Series) (float64, bool) { return lastValid(s, 3) }),
		DownPeer:       c.single(ctx, "prom/pd-down-peer", func(s Series) (float64, bool) { return lastValid(s, 3) }),
		PendingPeerMin: c.single(ctx, "prom/pd-pending-peer", func(s Series) (float64, bool) { return minLast(s, pdPendingMinutes) }),
		TSOP99:         c.single(ctx, "prom/pd-tso-p99", func(s Series) (float64, bool) { return lastValid(s, 3) }),
		EtcdFsync:      c.hostMax(ctx, "prom/pd-etcd-fsync-p99", "instance"),
		EtcdRTT:        c.hostMax(ctx, "prom/pd-etcd-rtt-p99", "instance"),
		StoreSlow:      c.hostMax(ctx, "prom/pd-store-slow-score", "address"),
		ApplyCPU:       c.hostMax(ctx, "prom/tikv-apply-cpu", "instance"),
		WriteStall:     c.hostMax(ctx, "prom/tikv-write-stall", "instance"),
		AppendP99:      c.hostMax(ctx, "prom/tikv-append-log-p99", "instance"),
		ApplyP99:       c.hostMax(ctx, "prom/tikv-apply-log-p99", "instance"),
		Latch:          c.hostMax(ctx, "prom/tikv-latch-wait-p99", "instance"),
		StorePool:      c.poolSizes(ctx, "raftstore.store-pool-size"),
		ApplyPool:      c.poolSizes(ctx, "raftstore.apply-pool-size"),
		Inode:          c.hostMax(ctx, "prom/node-inode-used", "instance"),
		ReadLat:        c.hostMax(ctx, "prom/node-disk-read-lat", "instance"),
		WriteLat:       c.hostMax(ctx, "prom/node-disk-write-lat", "instance"),
		Load:           c.hostMax(ctx, "prom/node-load-per-core", "instance"),
		TCP:            c.hostMax(ctx, "prom/node-tcp-estab", "instance"),
		ReadOnly:       map[string][]string{},
	}
	for _, s := range c.matrix(ctx, "prom/node-fs-readonly") {
		if v, ok := lastValid(s, 3); ok && v >= 1 && s.Metric["instance"] != "" {
			h := hostOf(s.Metric["instance"])
			sg.ReadOnly[h] = append(sg.ReadOnly[h], s.Metric["mountpoint"])
		}
	}
	return sg
}

func fmtSec(v float64) string {
	if v < 1 {
		return fmt.Sprintf("%.0f ms", v*1000)
	}
	return fmt.Sprintf("%.2f s", v)
}

func poolOf(m map[string]int, host string) int {
	if n := m[host]; n > 0 {
		return n
	}
	return tikvDefaultPool
}

// DiagnoseSignals 把第二批指标变成诊断项。topo 用来把主机 / 地址对应到实例 ID。
func DiagnoseSignals(topo model.Topology, sg Signals) []model.Finding {
	var out []model.Finding
	out = append(out, diagnosePD(topo, sg)...)
	out = append(out, diagnoseTiKVWrite(topo, sg)...)
	out = append(out, diagnoseHosts(topo, sg)...)
	return out
}

// ---------------------------------------------------------------- PD ----

func diagnosePD(topo model.Topology, sg Signals) []model.Finding {
	var out []model.Finding
	link := model.Link{Page: "arch", Label: "查看拓扑"}
	pdCmd := "tiup ctl:<版本> pd -u <PD地址> region check miss-peer\ntiup ctl:<版本> pd -u <PD地址> region check down-peer\ntiup ctl:<版本> pd -u <PD地址> region check pending-peer"

	if v := sg.MissPeer; v != nil && *v > pdMissPeerMax {
		out = append(out, model.Finding{ID: "pd-miss-peer", Sev: "warning", Scope: "pd",
			Title:    fmt.Sprintf("%.0f 个 Region 缺少副本", *v),
			Evidence: []string{fmt.Sprintf("缺副本的 Region 数 %.0f（官方告警阈值 > %.0f）", *v, pdMissPeerMax)},
			Cause:    "副本数少于配置（默认 3）。常见于 TiKV 刚下线 / 宕机后还在补副本、调度被限速、剩余 Store 空间不足（水位 ≥ 80% 时 PD 不再往上补）。",
			Actions: []model.Tip{{T: "列出缺副本的 Region，并看补副本的调度有没有被限速", Cmd: pdCmd},
				{T: "确认没有 Store 处于 Down / Offline，磁盘水位有余量", Cmd: "tiup ctl:<版本> pd -u <PD地址> store"}},
			Link: link, Ref: "alert-rules"})
	}
	if v := sg.DownPeer; v != nil && *v > 0 {
		out = append(out, model.Finding{ID: "pd-down-peer", Sev: "warning", Scope: "pd",
			Title:    fmt.Sprintf("%.0f 个 Region 有副本失联（down-peer）", *v),
			Evidence: []string{fmt.Sprintf("down-peer Region 数 %.0f（官方告警阈值 > 0）", *v)},
			Cause:    "Region 的某个副本长时间没有心跳（所在 TiKV 宕机、网络隔离，或副本数据损坏）。PD 会在超过 max-store-down-time（默认 30 分钟）后补副本。",
			Actions:  []model.Tip{{T: "找出失联副本所在的 Store", Cmd: "tiup ctl:<版本> pd -u <PD地址> region check down-peer"}},
			Link:     link, Ref: "alert-rules"})
	}
	if v := sg.PendingPeerMin; v != nil && *v > pdPendingPeerMax {
		out = append(out, model.Finding{ID: "pdp-pending-peer", Sev: "warning", Scope: "pd",
			Title:    fmt.Sprintf("至少 %.0f 个 Region 的副本落后，已持续 %d 分钟以上", *v, pdPendingMinutes),
			Evidence: []string{fmt.Sprintf("最近 %d 分钟内 pending-peer Region 数最低也有 %.0f（官方：> %.0f 且持续超过 %d 分钟才算异常，短时间出现是正常的）", pdPendingMinutes, *v, pdPendingPeerMax, pdPendingMinutes)},
			Cause:    "副本数据长时间追不上 leader：多见于新加入的 TiKV 正在同步、磁盘 I/O 慢或网络带宽被占满，快照应用排队。",
			Actions:  []model.Tip{{T: "查看落后副本分布在哪些 Store，对照这些 TiKV 的磁盘延迟和网络", Cmd: "tiup ctl:<版本> pd -u <PD地址> region check pending-peer"}},
			Link:     link, Ref: "alert-rules"})
	}
	if v := sg.TSOP99; v != nil && *v > pdTSOP99Sec {
		out = append(out, model.Finding{ID: "pd-tso", Sev: "warning", Scope: "pd",
			Title:    fmt.Sprintf("PD TSO 请求 P99 %s，偏高", fmtSec(*v)),
			Evidence: []string{fmt.Sprintf("TSO 请求 P99 %s（官方告警阈值 > %.1f s）", fmtSec(*v), pdTSOP99Sec)},
			Cause:    "PD leader 负载高、leader 刚切换，或 TiDB 到 PD 的网络延迟大。TSO 慢会直接拉长每条事务的开始时间。",
			Actions:  []model.Tip{{T: "看 PD leader 所在机器的 CPU / 磁盘，并对照下面的 etcd fsync 延迟"}, {T: "测 TiDB 到 PD 的网络延迟", Cmd: "ping -c 10 <PD 主机>"}},
			Link:     link, Ref: "alert-rules"})
	}
	pdByHost := map[string]string{}
	for _, in := range topo.Instances {
		if in.Type == "pd" {
			pdByHost[in.Host] = in.ID
		}
	}
	hosts := sortedKeys(pdByHost)
	for _, h := range hosts {
		if v, ok := sg.EtcdFsync[h]; ok && v > pdEtcdFsyncSec {
			out = append(out, model.Finding{ID: "pd-etcd-fsync-" + pdByHost[h], Sev: "warning", Scope: pdByHost[h],
				Title:    fmt.Sprintf("%s 的 etcd WAL fsync P99 %s，磁盘写入慢", pdByHost[h], fmtSec(v)),
				Evidence: []string{fmt.Sprintf("etcd WAL fsync P99 %s（官方告警阈值 > %.0f s）", fmtSec(v), pdEtcdFsyncSec)},
				Cause:    "PD 内嵌的 etcd 每次提交都要 fsync 到磁盘，磁盘慢（和别的服务共用盘、HDD、被 TiKV 的 I/O 挤占）会让 PD 响应变慢甚至 leader 切换。PD 应使用独立的 SSD。",
				Actions:  []model.Tip{{T: "看该机器的磁盘延迟与是否和 TiKV / 其它服务混部", Cmd: "iostat -x 1 5"}},
				Link:     model.Link{Page: "nodes", Label: "查看节点", Args: map[string]string{"tab": "inst", "node": pdByHost[h]}}, Ref: "alert-rules"})
		}
		if v, ok := sg.EtcdRTT[h]; ok && v > pdEtcdRTTSec {
			out = append(out, model.Finding{ID: "pd-etcd-rtt-" + pdByHost[h], Sev: "warning", Scope: pdByHost[h],
				Title:    fmt.Sprintf("%s 与其它 PD 节点的往返延迟 P99 %s", pdByHost[h], fmtSec(v)),
				Evidence: []string{fmt.Sprintf("etcd 节点间往返延迟 P99 %s（官方告警阈值 > %.0f s）", fmtSec(v), pdEtcdRTTSec)},
				Cause:    "PD 节点之间网络延迟大或丢包，会引起选举超时、leader 频繁切换。",
				Actions:  []model.Tip{{T: "检查 PD 节点之间的网络（跨机房部署时尤其注意）", Cmd: "ping -c 20 <其它 PD 主机>"}},
				Link:     model.Link{Page: "nodes", Label: "查看节点", Args: map[string]string{"tab": "inst", "node": pdByHost[h]}}, Ref: "alert-rules"})
		}
	}
	// 慢节点：PD 给 Store 打的慢分数
	for _, in := range topo.Instances {
		if in.Type != "tikv" {
			continue
		}
		if v, ok := sg.StoreSlow[in.Host]; ok && v > pdSlowScore {
			out = append(out, model.Finding{ID: "pds-slow-" + in.ID, Sev: "warning", Scope: in.ID,
				Title:    fmt.Sprintf("%s 被 PD 判定为慢节点（慢分数 %.0f）", in.ID, v),
				Evidence: []string{fmt.Sprintf("Store 慢分数 %.0f（> %.0f 表示该 Store 的磁盘 I/O 或网络明显慢于其它节点）", v, pdSlowScore)},
				Cause:    "PD 根据 TiKV 上报的磁盘 I/O 延迟等给 Store 打慢分数；分数高说明这台机器的磁盘 / 网络出了问题。配置了 evict-slow-store 调度时，PD 会把 Leader 从它身上赶走。",
				Actions:  []model.Tip{{T: "检查该机器的磁盘健康与延迟", Cmd: "iostat -x 1 5\ndmesg | tail -50"}, {T: "查看是否已触发慢节点驱逐调度", Cmd: "tiup ctl:<版本> pd -u <PD地址> scheduler show"}},
				Link:     model.Link{Page: "nodes", Label: "查看节点", Args: map[string]string{"tab": "inst", "node": in.ID}}, Ref: "alert-rules"})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// ---------------------------------------------------------------- TiKV 写入链路 ----

// diagnoseTiKVWrite 每个 TiKV 一条：把官方「写入慢」的分段（scheduler latch → append log → raftstore → apply → write stall）
// 里超阈值的段落一起列出，便于逐段定位。
func diagnoseTiKVWrite(topo model.Topology, sg Signals) []model.Finding {
	var out []model.Finding
	for _, in := range topo.Instances {
		if in.Type != "tikv" || in.Status == "down" || in.Status == "offline" {
			continue
		}
		type seg struct {
			name, ev, cause string
			acts            []model.Tip
		}
		var segs []seg
		if v, ok := sg.Latch[in.Host]; ok && v > tikvLogP99Sec {
			segs = append(segs, seg{"scheduler latch 等待", fmt.Sprintf("scheduler latch 等待 P99 %s（> %.0f s）", fmtSec(v), tikvLogP99Sec),
				"同一批 key 的写请求在 scheduler 里排队等 latch：多个事务在写相同的 key，或 scheduler 线程池不够。",
				[]model.Tip{{T: "先排查是不是热点行 / 热点 Region 的并发写，再考虑调大 storage.scheduler-worker-pool-size", Cmd: "SELECT instance, `key`, value FROM information_schema.cluster_config WHERE type = 'tikv' AND `key` = 'storage.scheduler-worker-pool-size';"}}})
		}
		if v, ok := sg.AppendP99[in.Host]; ok && v > tikvLogP99Sec {
			segs = append(segs, seg{"日志追加", fmt.Sprintf("append log P99 %s（> %.0f s；正常在 50 ms 内）", fmtSec(v), tikvLogP99Sec),
				"Raft 日志要先写盘（fsync）才能提交，慢说明磁盘写延迟高：盘性能不足、被其它进程占用，或磁盘快满。",
				[]model.Tip{{T: "看该机器磁盘的写延迟和利用率；TiKV 数据盘应使用独立的 NVMe / SSD", Cmd: "iostat -x 1 5"}}})
		}
		if v, ok := sg.ApplyP99[in.Host]; ok && v > tikvLogP99Sec {
			segs = append(segs, seg{"日志应用", fmt.Sprintf("apply log P99 %s（> %.0f s；正常在 100 ms 内）", fmtSec(v), tikvLogP99Sec),
				"已提交的日志要在 Apply 线程里写入 RocksDB：Apply 线程忙、单个 Region 写入过大，或 RocksDB 写入被限流（见 write stall）。",
				[]model.Tip{{T: "看 Apply 线程 CPU 与 RocksDB 的 compaction 压力"}}})
		}
		if n := poolOf(sg.StorePool, in.Host); in.RaftCPU/100 >= tikvPoolBusyRatio*float64(n) && in.RaftCPU > 0 {
			segs = append(segs, seg{"Raftstore 线程", fmt.Sprintf("Raftstore 线程 CPU %.2f 核，线程池 %d 个线程（已用 %.0f%%）", in.RaftCPU/100, n, in.RaftCPU/float64(n)),
				"Raftstore 线程池几乎跑满，Raft 消息的处理被拖慢。先看是不是写热点（单个 Region 打满一个线程）；确认不是热点后，可适度调大 raftstore.store-pool-size（官方建议 1–5）。",
				[]model.Tip{{T: "确认是否有写热点", Cmd: "SELECT * FROM information_schema.tidb_hot_regions WHERE type = 'write' LIMIT 20;"}}})
		}
		if v, ok := sg.ApplyCPU[in.Host]; ok {
			if n := poolOf(sg.ApplyPool, in.Host); v >= tikvPoolBusyRatio*float64(n) {
				segs = append(segs, seg{"Apply 线程", fmt.Sprintf("Apply 线程 CPU %.2f 核，线程池 %d 个线程（已用 %.0f%%）", v, n, v/float64(n)*100),
					"Apply 线程池几乎跑满，已提交的日志应用不过来。确认不是热点后，可适度调大 raftstore.apply-pool-size（官方建议 1–5）。",
					[]model.Tip{{T: "查看当前线程池配置", Cmd: "SELECT instance, `key`, value FROM information_schema.cluster_config WHERE type = 'tikv' AND `key` IN ('raftstore.store-pool-size', 'raftstore.apply-pool-size');"}}})
			}
		}
		if v, ok := sg.WriteStall[in.Host]; ok && v > 0 {
			segs = append(segs, seg{"RocksDB write stall", fmt.Sprintf("近 10 分钟 write stall 增加 %.0f", v),
				"RocksDB 的 compaction 跟不上写入（L0 文件太多或待 compaction 的数据太大）时会限流甚至停写，这是官方告警项（TiKV_write_stall）。",
				[]model.Tip{{T: "看 Grafana「TiKV-Details → RocksDB-kv → Write Stall Duration」与 compaction pending bytes；磁盘带宽不够时增加 rocksdb.max-background-jobs 也没有用，要先解决磁盘"}}})
		}
		if len(segs) == 0 {
			continue
		}
		var names, ev []string
		var acts []model.Tip
		cause := "官方把「写入慢」拆成几段逐段定位：scheduler latch → 日志追加（磁盘）→ Raftstore → Apply → RocksDB。下面列出的是超过阈值的段落，往往最靠前的一段就是根因。\n"
		for _, s := range segs {
			names = append(names, s.name)
			ev = append(ev, s.ev)
			cause += "· " + s.name + "：" + s.cause + "\n"
			acts = append(acts, s.acts...)
		}
		out = append(out, model.Finding{ID: "tikvw-" + in.ID, Sev: "warning", Scope: in.ID,
			Title:    fmt.Sprintf("%s 写入链路偏慢（%s）", in.ID, strings.Join(names, "、")),
			Evidence: ev, Cause: strings.TrimSpace(cause), Actions: acts,
			Link: model.Link{Page: "nodes", Label: "查看节点", Args: map[string]string{"tab": "inst", "node": in.ID}}, Ref: "troubleshoot-high-disk-io"})
	}
	return out
}

// ---------------------------------------------------------------- 主机层 ----

type hostHit struct{ host, text string }

// hostList 把命中的主机列成证据行（带该主机上的实例 ID），最多 sigListMax 台。
func hostList(topo model.Topology, hits []hostHit) []string {
	ids := map[string][]string{}
	for _, in := range topo.Instances {
		ids[in.Host] = append(ids[in.Host], in.ID)
	}
	sort.Slice(hits, func(i, j int) bool { return ipLess(hits[i].host, hits[j].host) })
	var out []string
	for i, h := range hits {
		if i >= sigListMax {
			out = append(out, fmt.Sprintf("……另有 %d 台", len(hits)-sigListMax))
			break
		}
		line := h.host
		if len(ids[h.host]) > 0 {
			line += "（" + strings.Join(ids[h.host], "、") + "）"
		}
		out = append(out, line+"："+h.text)
	}
	return out
}

func diagnoseHosts(topo model.Topology, sg Signals) []model.Finding {
	var out []model.Finding
	link := model.Link{Page: "nodes", Label: "查看节点"}
	if len(sg.ReadOnly) > 0 {
		var hits []hostHit
		for h, mp := range sg.ReadOnly {
			sort.Strings(mp)
			hits = append(hits, hostHit{h, "只读挂载点 " + strings.Join(mp, "、")})
		}
		out = append(out, model.Finding{ID: "hostr-readonly", Sev: "critical", Scope: "hosts",
			Title:    fmt.Sprintf("%d 台主机的文件系统变成只读", len(hits)),
			Evidence: hostList(topo, hits),
			Cause:    "文件系统出错时内核会把它重新挂载为只读（磁盘故障、文件系统损坏、云盘掉线），进程随后写盘失败。如果该挂载点是数据盘，上面的实例已经或即将异常。",
			Actions:  []model.Tip{{T: "查看内核日志里的磁盘 / 文件系统错误，并确认挂载状态", Cmd: "dmesg -T | grep -iE 'error|remount|i/o' | tail -50\nmount | grep -w ro"}},
			Link:     link, Ref: "alert-rules"})
	}
	var inodeHits []hostHit
	crit := false
	for h, v := range sg.Inode {
		if v >= hostInodeWarn {
			inodeHits = append(inodeHits, hostHit{h, fmt.Sprintf("最高的文件系统 inode 使用率 %.0f%%", v)})
			crit = crit || v >= hostInodeCrit
		}
	}
	if len(inodeHits) > 0 {
		sev := "warning"
		if crit {
			sev = "critical"
		}
		out = append(out, model.Finding{ID: "hosti-inode", Sev: sev, Scope: "hosts",
			Title:    fmt.Sprintf("%d 台主机的 inode 使用率 ≥ %.0f%%", len(inodeHits), hostInodeWarn),
			Evidence: hostList(topo, inodeHits),
			Cause:    "inode 用尽后即使磁盘还有空间也无法创建新文件（日志、临时文件、备份小文件过多）。≥ 90% 视为严重。",
			Actions:  []model.Tip{{T: "找出文件数最多的目录并清理（日志轮转、过期备份）", Cmd: "df -i\nfind <挂载点> -xdev -type f | cut -d/ -f1-4 | sort | uniq -c | sort -rn | head"}},
			Link:     link, Ref: "alert-rules"})
	}
	var diskHits []hostHit
	for _, h := range sortedKeys(mergeKeys(sg.ReadLat, sg.WriteLat)) {
		var parts []string
		if v, ok := sg.ReadLat[h]; ok && v > hostReadLatSec {
			parts = append(parts, fmt.Sprintf("读延迟 %s（> %.0f ms）", fmtSec(v), hostReadLatSec*1000))
		}
		if v, ok := sg.WriteLat[h]; ok && v > hostWriteLatSec {
			parts = append(parts, fmt.Sprintf("写延迟 %s（> %.0f ms）", fmtSec(v), hostWriteLatSec*1000))
		}
		if len(parts) > 0 {
			diskHits = append(diskHits, hostHit{h, strings.Join(parts, "，")})
		}
	}
	if len(diskHits) > 0 {
		out = append(out, model.Finding{ID: "hostl-disk", Sev: "warning", Scope: "hosts",
			Title:    fmt.Sprintf("%d 台主机的磁盘延迟偏高", len(diskHits)),
			Evidence: hostList(topo, diskHits),
			Cause:    "磁盘读写延迟超过官方告警阈值（读 32 ms / 写 16 ms，取该主机上最慢的一块盘，且 IOPS > 5）。TiKV / PD 对磁盘延迟很敏感，延迟高会直接表现为写入变慢、PD 响应慢。",
			Actions:  []model.Tip{{T: "确认是哪块盘、哪个进程在占用 I/O", Cmd: "iostat -x 1 5\niotop -oPa"}},
			Link:     link, Ref: "alert-rules"})
	}
	var loadHits []hostHit
	for h, v := range sg.Load {
		if v > hostLoadPerCore {
			loadHits = append(loadHits, hostHit{h, fmt.Sprintf("5 分钟负载是核数的 %.2f 倍", v)})
		}
	}
	if len(loadHits) > 0 {
		out = append(out, model.Finding{ID: "hostl-load", Sev: "warning", Scope: "hosts",
			Title:    fmt.Sprintf("%d 台主机的系统负载超过 CPU 核数", len(loadHits)),
			Evidence: hostList(topo, loadHits),
			Cause:    "5 分钟平均负载 / CPU 核数 > 1 表示可运行的任务多于核数，进程在排队等 CPU（也可能是大量进程在等磁盘 I/O，要和上一项对照）。",
			Actions:  []model.Tip{{T: "看是哪个进程占用 CPU，混部时尤其注意", Cmd: "top -c -b -n 1 | head -20"}},
			Link:     link, Ref: "alert-rules"})
	}
	var tcpHits []hostHit
	for h, v := range sg.TCP {
		if v > hostTCPEstablished {
			tcpHits = append(tcpHits, hostHit{h, fmt.Sprintf("ESTABLISHED 连接 %.0f", v)})
		}
	}
	if len(tcpHits) > 0 {
		out = append(out, model.Finding{ID: "hostl-tcp", Sev: "warning", Scope: "hosts",
			Title:    fmt.Sprintf("%d 台主机的 TCP 连接数 > %.0f", len(tcpHits), hostTCPEstablished),
			Evidence: hostList(topo, tcpHits),
			Cause:    "ESTABLISHED 连接数超过官方告警阈值。TiDB 所在机器多为应用连接过多（先看连接池配置）；其它角色多为组件间连接，注意是否有连接泄漏。",
			Actions:  []model.Tip{{T: "按状态与对端统计连接", Cmd: "ss -tan state established | awk '{print $4}' | cut -d: -f1 | sort | uniq -c | sort -rn | head"}},
			Link:     link, Ref: "alert-rules"})
	}
	return out
}

func mergeKeys(a, b map[string]float64) map[string]bool {
	m := map[string]bool{}
	for k := range a {
		m[k] = true
	}
	for k := range b {
		m[k] = true
	}
	return m
}
