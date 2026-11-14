// @author: kevinkelly
// @license: Apache License 2.0
// @file: signals_test.go
// @time: 2026/10/28

package collect

import (
	"math"
	"strings"
	"testing"

	"tidbops/internal/model"
)

func sigTopo() model.Topology {
	return model.Topology{Cluster: "c", Instances: []model.Instance{
		{ID: "pd-1", Type: "pd", Host: "10.0.1.21", Status: "up"},
		{ID: "tikv-1", Type: "tikv", Host: "10.0.2.11", Status: "up", RaftCPU: 40},
		{ID: "tikv-2", Type: "tikv", Host: "10.0.2.12", Status: "up", RaftCPU: 175},
		{ID: "tidb-1", Type: "tidb", Host: "10.0.1.11", Status: "up"},
	}}
}

func p(v float64) *float64 { return &v }

func TestDiagnosePDSignals(t *testing.T) {
	sg := Signals{MissPeer: p(250), DownPeer: p(2), PendingPeerMin: p(180), TSOP99: p(0.35),
		EtcdFsync: map[string]float64{"10.0.1.21": 1.8}, EtcdRTT: map[string]float64{"10.0.1.21": 0.002},
		StoreSlow: map[string]float64{"10.0.2.12": 95, "10.0.2.11": 3}}
	got := findingsByID(DiagnoseSignals(sigTopo(), sg))
	for _, id := range []string{"pd-miss-peer", "pd-down-peer", "pdp-pending-peer", "pd-tso", "pd-etcd-fsync-pd-1", "pds-slow-tikv-2"} {
		f, ok := got[id]
		if !ok {
			t.Errorf("缺少 %s（实际 %v）", id, got)
			continue
		}
		if f.Sev != "warning" || len(f.Evidence) == 0 || f.Cause == "" || len(f.Actions) == 0 || f.Link.Page == "" || f.Ref == "" {
			t.Errorf("%s 内容不完整: %+v", id, f)
		}
	}
	if _, ok := got["pd-etcd-rtt-pd-1"]; ok {
		t.Error("etcd 往返延迟正常，不应报")
	}
	if _, ok := got["pds-slow-tikv-1"]; ok {
		t.Error("慢分数低的 Store 不应报")
	}
	if !strings.Contains(strings.Join(got["pdp-pending-peer"].Evidence, " "), "30 分钟") {
		t.Errorf("pending-peer 证据应写明持续 30 分钟的口径: %v", got["pdp-pending-peer"].Evidence)
	}
}

func TestDiagnosePDSignalsQuietWhenHealthyOrMissing(t *testing.T) {
	healthy := Signals{MissPeer: p(0), DownPeer: p(0), PendingPeerMin: p(40), TSOP99: p(0.03),
		EtcdFsync: map[string]float64{"10.0.1.21": 0.004}, StoreSlow: map[string]float64{"10.0.2.12": 2}}
	quiet := sigTopo()
	quiet.Instances[2].RaftCPU = 40 // Raftstore 线程 CPU 来自拓扑，正常值
	if fs := DiagnoseSignals(quiet, healthy); len(fs) != 0 {
		t.Errorf("健康的指标不应有诊断项: %+v", fs)
	}
	if fs := DiagnoseSignals(quiet, Signals{}); len(fs) != 0 {
		t.Errorf("没有任何指标时不应有诊断项: %+v", fs)
	}
}

func TestDiagnoseTiKVWriteChain(t *testing.T) {
	sg := Signals{
		AppendP99:  map[string]float64{"10.0.2.12": 1.4, "10.0.2.11": 0.02},
		ApplyP99:   map[string]float64{"10.0.2.12": 0.04},
		Latch:      map[string]float64{"10.0.2.12": 0.001},
		ApplyCPU:   map[string]float64{"10.0.2.12": 1.9, "10.0.2.11": 0.4},
		WriteStall: map[string]float64{"10.0.2.12": 3, "10.0.2.11": 0},
	}
	got := findingsByID(DiagnoseSignals(sigTopo(), sg))
	f, ok := got["tikvw-tikv-2"]
	if !ok {
		t.Fatalf("tikv-2 应有写入链路诊断项: %v", got)
	}
	// tikv-2 的 Raftstore CPU 1.75 核 / 默认线程池 2（87%）≥ 80%；append log、Apply 线程（1.9 / 2）、write stall 也超阈值
	for _, want := range []string{"日志追加", "Raftstore 线程", "Apply 线程", "write stall"} {
		if !strings.Contains(f.Title, want) {
			t.Errorf("标题应包含 %q: %s", want, f.Title)
		}
	}
	if strings.Contains(f.Title, "scheduler latch") || strings.Contains(f.Title, "日志应用") {
		t.Errorf("正常的段落不应出现在标题里: %s", f.Title)
	}
	if len(f.Evidence) != 4 || f.Sev != "warning" || f.Link.Args["node"] != "tikv-2" {
		t.Errorf("证据 / 级别 / 链接不对: %+v", f)
	}
	if _, ok := got["tikvw-tikv-1"]; ok {
		t.Error("tikv-1 一切正常，不应报")
	}
	// 配置里的线程池比默认大时，按实际线程数折算：Raftstore 1.75 核 / 4 个线程 = 44%，不再算繁忙
	sg2 := Signals{StorePool: map[string]int{"10.0.2.12": 4}}
	if fs := DiagnoseSignals(sigTopo(), sg2); len(fs) != 0 {
		t.Errorf("线程池为 4 时 1.75 核不算繁忙: %+v", fs)
	}
}

func TestDiagnoseHostSignals(t *testing.T) {
	sg := Signals{
		ReadOnly: map[string][]string{"10.0.2.12": {"/data", "/data/log"}},
		Inode:    map[string]float64{"10.0.2.11": 93, "10.0.2.12": 81, "10.0.1.21": 20},
		ReadLat:  map[string]float64{"10.0.2.11": 0.05, "10.0.2.12": 0.004},
		WriteLat: map[string]float64{"10.0.2.12": 0.03},
		Load:     map[string]float64{"10.0.1.11": 1.4, "10.0.2.11": 0.5},
		TCP:      map[string]float64{"10.0.1.11": 61000},
	}
	got := findingsByID(DiagnoseSignals(sigTopo(), sg))
	if f := got["hostr-readonly"]; f.Sev != "critical" || !strings.Contains(strings.Join(f.Evidence, " "), "/data、/data/log") || !strings.Contains(f.Evidence[0], "tikv-2") {
		t.Errorf("只读文件系统: %+v", f)
	}
	if f := got["hosti-inode"]; f.Sev != "critical" || len(f.Evidence) != 2 || !strings.Contains(f.Title, "2 台") {
		t.Errorf("inode ≥ 90%% 应为严重，且两台主机都列出: %+v", f)
	}
	if f := got["hostl-disk"]; f.Sev != "warning" || len(f.Evidence) != 2 {
		t.Errorf("磁盘延迟应列出读 / 写各一台: %+v", f)
	}
	if f := got["hostl-load"]; f.Sev != "warning" || !strings.Contains(f.Evidence[0], "tidb-1") || !strings.Contains(f.Evidence[0], "1.40") {
		t.Errorf("负载: %+v", f)
	}
	if f := got["hostl-tcp"]; f.Sev != "warning" || !strings.Contains(f.Evidence[0], "61000") {
		t.Errorf("TCP 连接数: %+v", f)
	}
	// inode 只有 81% → 警告
	warn := findingsByID(DiagnoseSignals(sigTopo(), Signals{Inode: map[string]float64{"10.0.2.12": 81}}))
	if warn["hosti-inode"].Sev != "warning" {
		t.Errorf("inode 81%% 应为警告: %+v", warn)
	}
}

func TestHostEvidenceListIsCapped(t *testing.T) {
	hits := map[string]float64{}
	for i := 1; i <= 12; i++ {
		hits["10.0.9."+string(rune('0'+i%10))+string(rune('a'+i))] = 1.5
	}
	f := findingsByID(DiagnoseSignals(model.Topology{}, Signals{Load: hits}))["hostl-load"]
	if len(f.Evidence) != sigListMax+1 || !strings.Contains(f.Evidence[len(f.Evidence)-1], "另有 4 台") {
		t.Errorf("证据最多列 %d 台并说明其余数量: %v", sigListMax, f.Evidence)
	}
}

func TestSeriesHelpersSkipNaN(t *testing.T) {
	nan := math.NaN()
	s := Series{Points: []Point{{1, 0.2}, {2, nan}, {3, 0.4}, {4, math.Inf(1)}}}
	if v, ok := lastValid(s, 3); !ok || math.Abs(v-0.3) > 1e-9 {
		t.Errorf("应跳过 NaN / Inf 后取平均: %v %v", v, ok)
	}
	if _, ok := lastValid(Series{Points: []Point{{1, nan}}}, 3); ok {
		t.Error("全是 NaN 时应视为没有数据")
	}
	var pts []Point
	for i := 0; i < 30; i++ {
		pts = append(pts, Point{int64(i), 150})
	}
	pts[10].V = 120
	if v, ok := minLast(Series{Points: pts}, 30); !ok || v != 120 {
		t.Errorf("minLast 应取窗口内最小值: %v %v", v, ok)
	}
	if _, ok := minLast(Series{Points: pts[:10]}, 30); ok {
		t.Error("样本不足 2/3 时不应判断")
	}
}

func TestSustainRulesCoverSecondBatch(t *testing.T) {
	for id, want := range map[string]int{"tikvw-tikv-2": 5, "pd-miss-peer": 5, "pds-slow-tikv-1": 5, "hostl-load": 5, "pdp-pending-peer": 0, "hostr-readonly": 0, "hosti-inode": 0, "slowx-plan-abc": 0} {
		if got := sustainNeed(id); got != want {
			t.Errorf("sustainNeed(%q) = %d，期望 %d", id, got, want)
		}
	}
}
