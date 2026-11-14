// @author: kevinkelly
// @license: Apache License 2.0
// @file: plan_maint_exit_test.go
// @time: 2026/10/28

package change

import "testing"

// 结束维护的「验证：无 down-peer」只在主机上有 TiKV / TiFlash 时才加：只有 TiDB / PD 的主机没有 Region 副本要验证，
// 不能因为中控机缺 ctl 组件之类的无关问题让已经恢复的维护单失败。
func TestMaintExitVerifyOnlyWithStorage(t *testing.T) {
	topo := testTopo()
	hasVerify := func(p Plan) bool {
		for _, s := range p.Steps {
			if s.Name == "验证：无 down-peer" {
				return true
			}
		}
		return false
	}
	cases := []struct {
		name  string
		ip    string
		nodes []string
		want  bool
	}{
		{"仅 TiDB", "10.0.1.11", []string{"tidb-1"}, false},
		{"仅 PD", "10.0.1.21", []string{"pd-1"}, false},
		{"含 TiKV", "10.0.2.11", []string{"tikv-1"}, true},
	}
	for _, c := range cases {
		m := &Maint{ID: "MNT-1", IP: c.ip, State: "active", Nodes: c.nodes, Strategy: "standard", Mins: 20, Limit: 30}
		p, err := (Ctx{Cluster: "prod", Topo: topo, Maints: []*Maint{m}}).Plan(KindMaintExit, js(maintExitParams{MaintID: "MNT-1", Urgent: true}))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := hasVerify(p); got != c.want {
			t.Errorf("%s: 是否含「验证：无 down-peer」= %v，期望 %v（步骤 %+v）", c.name, got, c.want, p.Steps)
		}
		// 启动与等待就绪始终保留
		if len(p.Steps) < 2 || p.Steps[0].Cmds[0].Kind == "" {
			t.Errorf("%s: 启动步骤缺失: %+v", c.name, p.Steps)
		}
	}
}
