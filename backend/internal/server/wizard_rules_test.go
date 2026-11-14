// @author: kevinkelly
// @license: Apache License 2.0
// @file: wizard_rules_test.go
// @time: 2026/10/28

package server

import (
	"strings"
	"testing"

	"tidbops/internal/model"
)

// wizSnap 构造一个 3 个 TiKV + 1 个 TiDB 的快照，各 TiKV 的 Raftstore CPU / 写流量由参数给定。
func wizSnap(raft, wr [3]float64) model.Snapshot {
	s := model.Snapshot{Topology: model.Topology{Instances: []model.Instance{{ID: "tidb-1", Type: "tidb", Status: "up", Mem: 50, Conn: 100}}}}
	for i, id := range []string{"tikv-1", "tikv-2", "tikv-3"} {
		s.Topology.Instances = append(s.Topology.Instances, model.Instance{ID: id, Type: "tikv", Status: "up", RaftCPU: raft[i], WR: wr[i], Disk: 50})
	}
	return s
}

func TestWizardRulesRaftSkew(t *testing.T) {
	cases := []struct {
		name string
		snap model.Snapshot
		want string // "" = 不返回
		in   string // why 里应包含
	}{
		{"个别节点很高", wizSnap([3]float64{20, 75, 22}, [3]float64{10, 12, 11}), "yes", "tikv-2"},
		{"写流量 ≥3× 均值", wizSnap([3]float64{0, 0, 0}, [3]float64{10, 10, 90}), "yes", "tikv-3"},
		{"很均衡", wizSnap([3]float64{40, 44, 41}, [3]float64{10, 12, 11}), "no", "均衡"},
		{"都不高", wizSnap([3]float64{3, 10, 5}, [3]float64{10, 12, 11}), "no", "都不高"},
		{"介于两者之间", wizSnap([3]float64{40, 65, 40}, [3]float64{10, 12, 11}), "unknown", "tikv-2"},
		{"没有 CPU 数据也没有写热点", wizSnap([3]float64{0, 0, 0}, [3]float64{10, 12, 11}), "", ""},
	}
	for _, c := range cases {
		ev := wizardEvidence(c.snap)["写入变慢/w1"]
		if ev.Answer != c.want {
			t.Errorf("%s: answer=%q，期望 %q（%+v）", c.name, ev.Answer, c.want, ev)
			continue
		}
		if c.want != "" && (!strings.Contains(ev.Why, c.in) || ev.Ref != "hotspot") {
			t.Errorf("%s: why=%q ref=%q", c.name, ev.Why, ev.Ref)
		}
	}
	// 少于 3 个 TiKV 无法比较
	two := wizSnap([3]float64{20, 90, 20}, [3]float64{})
	two.Topology.Instances = two.Topology.Instances[:3]
	if _, ok := wizardEvidence(two)["写入变慢/w1"]; ok {
		t.Error("TiKV 少于 3 个不应给出结论")
	}
	// 诊断引擎已经判定的写热点优先
	hot := wizSnap([3]float64{40, 40, 40}, [3]float64{10, 10, 10})
	hot.Findings = []model.Finding{{ID: "hotw-tikv-1", Title: "tikv-1 疑似写入热点", Evidence: []string{"tikv-1 写流量 99 MB/s"}}}
	if ev := wizardEvidence(hot)["写入变慢/w1"]; ev.Answer != "yes" || !strings.Contains(ev.Why, "99 MB/s") {
		t.Errorf("应采用诊断结论: %+v", ev)
	}
}

func TestWizardRulesStallBusyLeaderCompile(t *testing.T) {
	// Stalling 只能依据告警
	s := wizSnap([3]float64{40, 40, 40}, [3]float64{10, 10, 10})
	ev := wizardEvidence(s)
	for _, k := range []string{"写入变慢/w2", "TiKV server is busy/b1"} {
		if _, ok := ev[k]; ok {
			t.Errorf("没有 stall 告警时 %s 不应返回", k)
		}
	}
	s.Alerts = []model.Alert{{Lv: "warning", Name: "TiKV_write_stall", Inst: "10.0.2.12:20160", Time: "09:00", Desc: "RocksDB write stall"}}
	for _, k := range []string{"写入变慢/w2", "TiKV server is busy/b1"} {
		if e := wizardEvidence(s)[k]; e.Answer != "yes" || !strings.Contains(e.Why, "TiKV_write_stall") {
			t.Errorf("%s: %+v", k, e)
		}
	}

	// b3：接近 100% / 告警 / 不高 / 偏高
	if e := wizardEvidence(wizSnap([3]float64{40, 95, 40}, [3]float64{}))["TiKV server is busy/b3"]; e.Answer != "yes" || !strings.Contains(e.Why, "tikv-2") {
		t.Errorf("b3 接近 100%%: %+v", e)
	}
	if e := wizardEvidence(wizSnap([3]float64{40, 50, 40}, [3]float64{}))["TiKV server is busy/b3"]; e.Answer != "no" {
		t.Errorf("b3 不高: %+v", e)
	}
	if e := wizardEvidence(wizSnap([3]float64{40, 80, 40}, [3]float64{}))["TiKV server is busy/b3"]; e.Answer != "unknown" {
		t.Errorf("b3 偏高: %+v", e)
	}
	al := wizSnap([3]float64{40, 80, 40}, [3]float64{})
	al.Alerts = []model.Alert{{Lv: "critical", Name: "TiKV_raftstore_thread_cpu_seconds_total", Inst: "x"}}
	if e := wizardEvidence(al)["TiKV server is busy/b3"]; e.Answer != "yes" {
		t.Errorf("b3 告警: %+v", e)
	}
	if _, ok := wizardEvidence(wizSnap([3]float64{}, [3]float64{}))["TiKV server is busy/b3"]; ok {
		t.Error("没有 Raftstore CPU 数据时 b3 不应返回")
	}

	// l2：告警 / 实例停止 → yes；否则 unknown；没有拓扑 → 不返回
	if e := wizardEvidence(wizSnap([3]float64{}, [3]float64{}))["延迟升高/l2"]; e.Answer != "unknown" {
		t.Errorf("l2 默认应 unknown: %+v", e)
	}
	lc := wizSnap([3]float64{}, [3]float64{})
	lc.Alerts = []model.Alert{{Lv: "warning", Name: "PD_leader_change", Inst: "10.0.1.21:2379", Time: "03:12", Desc: "PD Leader 24h 内切换 ≥ 2 次"}}
	if e := wizardEvidence(lc)["延迟升高/l2"]; e.Answer != "yes" || !strings.Contains(e.Why, "PD_leader_change") {
		t.Errorf("l2 告警: %+v", e)
	}
	dn := wizSnap([3]float64{}, [3]float64{})
	dn.Topology.Instances[2].Status = "down"
	if e := wizardEvidence(dn)["延迟升高/l2"]; e.Answer != "yes" || !strings.Contains(e.Why, "tikv-2") {
		t.Errorf("l2 实例停止: %+v", e)
	}
	if _, ok := wizardEvidence(model.Snapshot{})["延迟升高/l2"]; ok {
		t.Error("空快照不应给结论")
	}

	// l6：Compile 占比
	comp := func(p float64) model.Snapshot {
		s := wizSnap([3]float64{}, [3]float64{})
		s.DBTime.ByStage = []model.Item{{Name: "Parse", Pct: 5}, {Name: "Compile", Pct: p}, {Name: "Execute", Pct: 95 - p}}
		return s
	}
	for p, want := range map[float64]string{40: "yes", 30: "yes", 20: "unknown", 8: "no"} {
		if e := wizardEvidence(comp(p))["延迟升高/l6"]; e.Answer != want {
			t.Errorf("Compile %.0f%%: %+v，期望 %s", p, e, want)
		}
	}
	if _, ok := wizardEvidence(wizSnap([3]float64{}, [3]float64{}))["延迟升高/l6"]; ok {
		t.Error("没有 DBTime 数据时 l6 不应返回")
	}
}

func TestWizardFacts(t *testing.T) {
	s := wizSnap([3]float64{}, [3]float64{})
	s.Topology.Instances[1].Disk = 86
	s.Topology.Instances[0].Mem = 85
	s.Topology.Instances[3].Status = "down"
	s.Alerts = []model.Alert{{Lv: "critical", Name: "A_crit"}, {Lv: "warning", Name: "B_warn"}}
	s.Slow = []model.SlowGroup{{Index: "—（TableFullScan）"}, {Index: "idx_a"}}
	facts := map[string]model.Alert{}
	for _, f := range wizardFacts(s) {
		facts[f.ID] = model.Alert{Lv: f.Level, Desc: f.Text}
	}
	for id, want := range map[string]string{"inst-down": "tikv-3", "disk-high": "tikv-1 磁盘 86%", "tidb-mem": "tidb-1 内存 85%", "alerts-critical": "A_crit", "slow-fullscan": "1 类"} {
		if f, ok := facts[id]; !ok || !strings.Contains(f.Desc, want) {
			t.Errorf("线索 %s: %+v 应含 %q", id, f, want)
		}
	}
	if strings.Contains(facts["alerts-critical"].Desc, "B_warn") {
		t.Error("只列严重告警")
	}
	if got := wizardFacts(model.Snapshot{}); got == nil || len(got) != 0 {
		t.Errorf("空快照应返回空数组而非 nil: %#v", got)
	}
}
