// @author: kevinkelly
// @license: Apache License 2.0
// @file: sim_check_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"strings"
	"testing"
)

// SimRunner 对带拓扑的 cluster.check 产出逼真的检查表：普通主机全部通过，末位 250 / 251 的主机带 Warn / Fail，
// 供主机池预检的测试使用。
func TestSimClusterCheckProblemHosts(t *testing.T) {
	sim := &SimRunner{Pol: testPolicy(""), Topo: StaticTopo(testTopo()), Key: testKey}
	run := func(hosts ...string) string {
		// 多台主机各自一个列表项：合并成一个 tikv_servers 列表
		content := "tikv_servers:\n"
		for _, h := range hosts {
			content += "  - host: " + h + "\n    port: 20160\n    status_port: 20180\n    deploy_dir: /tidb-deploy/tikv-20160\n    data_dir: /tidb-data/tikv-20160\n"
		}
		res, err := sim.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterCheck, Cluster: "prod-tidb-01", Content: content}}, nil)
		if err != nil || res.Exit != 0 {
			t.Fatalf("预检应通过执行器校验: %v %+v", err, res)
		}
		return res.Output
	}
	out := run("10.0.2.250", "10.0.2.251", "10.0.2.17")
	has := func(sub string) bool { return strings.Contains(out, sub) }
	if !has("10.0.2.250  swap") || !has("swap is enabled") || !has("THP is enabled") {
		t.Errorf("末位 250 的主机应有 swap Fail / THP Warn:\n%s", out)
	}
	if !has("soft limit of 'nofile'") || !has("nodelalloc") || !has("net.core.somaxconn") {
		t.Errorf("末位 251 的主机应有 limits / disk / sysctl 问题:\n%s", out)
	}
	if has("All checks passed") {
		t.Errorf("有 Warn / Fail 时不应输出「全部通过」:\n%s", out)
	}
	clean := run("10.0.2.17")
	if strings.Contains(clean, "Fail") || strings.Contains(clean, "Warn") || !strings.Contains(clean, "All checks passed") {
		t.Errorf("普通主机应全部通过:\n%s", clean)
	}
}
