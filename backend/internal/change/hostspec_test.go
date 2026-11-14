// @author: kevinkelly
// @license: Apache License 2.0
// @file: hostspec_test.go
// @time: 2026/10/28

package change

import (
	"strings"
	"testing"

	"tidbops/internal/exec"
)

// BuildScaleOutYAML 是 scaleOutYAML 的导出封装：同样的输入必须得到完全相同的文件，且各角色的最小拓扑都能通过执行器校验。
func TestBuildScaleOutYAML(t *testing.T) {
	v := newEnv(t, "")
	hosts := []HostSpec{{IP: "10.0.2.17", AZ: "az1", Rack: "r07"}}
	for _, role := range []string{"tikv", "tidb", "pd", "tiflash", "ticdc"} {
		got, err := BuildScaleOutYAML(role, hosts, 0, 0)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		want, err := scaleOutYAML(scaleOutParams{Role: role, Hosts: hosts, Port: defaultPort[role]}, v.topo)
		if err != nil || got != want {
			t.Errorf("%s: 封装应与 scaleOutYAML 一致: %v\n%s\n---\n%s", role, err, got, want)
		}
		if !strings.Contains(got, "host: 10.0.2.17") || strings.Count(got, "- host:") != 1 {
			t.Errorf("%s: 应只含一台主机:\n%s", role, got)
		}
		if err := exec.ValidateScaleOut(got, execTopo(v.topo)); err != nil {
			t.Errorf("%s: 执行器应接受预检用的最小拓扑: %v\n%s", role, err, got)
		}
	}
	// 指定端口与 block-cache
	y, err := BuildScaleOutYAML("tikv", hosts, 20161, 16)
	if err != nil || !strings.Contains(y, "port: 20161") || !strings.Contains(y, `storage.block-cache.capacity: "16GB"`) || !strings.Contains(y, `zone: "az1", rack: "r07", host: "10-0-2-17"`) {
		t.Errorf("端口 / cache / 标签: %v\n%s", err, y)
	}
	// 错误：未知角色、没有主机、TiKV 缺 zone
	if _, err := BuildScaleOutYAML("redis", hosts, 0, 0); err == nil {
		t.Error("未知角色应报错")
	}
	if _, err := BuildScaleOutYAML("tikv", nil, 0, 0); err == nil {
		t.Error("没有主机应报错")
	}
	if _, err := BuildScaleOutYAML("tikv", []HostSpec{{IP: "10.0.2.17"}}, 0, 0); err == nil {
		t.Error("TiKV 缺可用区应报错（与扩容变更一致）")
	}
	// 既有行为不变：TiDB 不需要 AZ
	if _, err := BuildScaleOutYAML("tidb", []HostSpec{{IP: "10.0.1.14"}}, 0, 0); err != nil {
		t.Errorf("TiDB 不需要 AZ: %v", err)
	}
}

// 主机池里最近一次预检未通过的主机：扩容规划给出 warn（不阻断）；通过 / 没有记录的主机不产生该提示。
func TestPlanScaleOutWarnsOnFailedHostCheck(t *testing.T) {
	v := newEnv(t, "")
	plan := func(hc map[string]bool, ip string) Plan {
		c := Ctx{Cluster: "prod", Topo: v.topo, HostChecks: hc}
		p, err := c.Plan(KindScaleOut, js(scaleOutParams{Role: "tikv", Hosts: []HostSpec{{IP: ip, AZ: "az1", Rack: "r1"}}}))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	has := func(p Plan, lv, sub string) bool {
		for _, c := range p.Checks {
			if c.Lv == lv && strings.Contains(c.T, sub) {
				return true
			}
		}
		return false
	}
	p := plan(map[string]bool{"10.0.2.17": false}, "10.0.2.17")
	if !has(p, "warn", "最近一次预检未通过") || p.Blocked {
		t.Errorf("应给出 warn 且不阻断: blocked=%v %+v", p.Blocked, p.Checks)
	}
	if p := plan(map[string]bool{"10.0.2.17": true}, "10.0.2.17"); has(p, "warn", "预检未通过") {
		t.Errorf("预检通过不应告警: %+v", p.Checks)
	}
	if p := plan(map[string]bool{"10.0.2.99": false}, "10.0.2.17"); has(p, "warn", "预检未通过") {
		t.Errorf("别的主机的预检失败不应影响这台: %+v", p.Checks)
	}
	if p := plan(nil, "10.0.2.17"); has(p, "warn", "预检未通过") {
		t.Errorf("没有预检记录不应告警: %+v", p.Checks)
	}
}
