// @author: kevinkelly
// @license: Apache License 2.0
// @file: percluster_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"strings"
	"testing"
)

// 多集群共用一份策略文件时，SQL 地址与 Alertmanager 必须能按集群区分：没有 PerCluster 的集群沿用全局值（旧行为不变），
// 有的集群用自己的值（空字段 = 没有，不会回退到别的集群的全局值）。
func TestPolicyPerClusterOverridesConnections(t *testing.T) {
	pol := testPolicy(t.TempDir())
	pol.MySQLHost, pol.MySQLPort = "10.0.1.11", 4000 // 全局值属于主集群
	pol.PerCluster = map[string]ClusterConn{
		"slowc": {MySQLHost: "10.9.9.9", MySQLPort: 4001, AlertURL: "http://10.9.0.1:9093"},
		"failc": {}, // 显式声明「没有」：既不用全局 SQL 地址（回退拓扑），也没有 Alertmanager
	}
	ctx := context.Background()
	sql := func(cluster string) Plan {
		t.Helper()
		p, err := Argv(ctx, pol, mustTopo(t), Cmd{Kind: KindSQLSetGlobal, Cluster: cluster, Key: "tidb_slow_log_threshold", Value: "500"})
		if err != nil {
			t.Fatalf("%s: %v", cluster, err)
		}
		return p
	}
	if a := strings.Join(sql(testCluster).Argv, " "); !strings.Contains(a, "-h 10.0.1.11 -P 4000") {
		t.Errorf("没有覆盖项的集群沿用全局值: %s", a)
	}
	if a := strings.Join(sql("slowc").Argv, " "); !strings.Contains(a, "-h 10.9.9.9 -P 4001") || strings.Contains(a, "10.0.1.11") {
		t.Errorf("slowc 应使用自己的 SQL 地址: %s", a)
	}
	topoHost := mustTopo(t)
	tp, _ := topoHost(ctx, "failc")
	if a := strings.Join(sql("failc").Argv, " "); !strings.Contains(a, "-h "+tp.SQLHost) {
		t.Errorf("空覆盖项应回退拓扑里的 TiDB，而不是全局值: %s", a)
	}

	silence := func(cluster string) (Plan, error) {
		return Argv(ctx, pol, mustTopo(t), Cmd{Kind: KindAlertSilence, Cluster: cluster, Key: "x", Rate: 5})
	}
	if p, err := silence(testCluster); err != nil || p.HTTP.URL != "http://10.0.1.31:9093/api/v2/silences" {
		t.Errorf("全局 Alertmanager: %v %+v", err, p.HTTP)
	}
	if p, err := silence("slowc"); err != nil || p.HTTP.URL != "http://10.9.0.1:9093/api/v2/silences" {
		t.Errorf("slowc 应使用自己的 Alertmanager: %v %+v", err, p.HTTP)
	}
	if _, err := silence("failc"); err == nil {
		t.Error("没有 Alertmanager 的集群不能静默（不得回退到别的集群的全局地址）")
	}
	if err := Validate(ctx, pol, mustTopo(t), Cmd{Kind: KindAlertSilence, Cluster: "failc", Key: "x", Rate: 5}); err == nil {
		t.Error("Validate 同样应拒绝")
	}
}
