// @author: kevinkelly
// @license: Apache License 2.0
// @file: proxy_internal_test.go
// @time: 2026/10/28

package collect

import (
	"testing"

	"tidbops/internal/source"
)

const displayWithProxy = `Cluster type:       tidb
Cluster name:       c1
Cluster version:    v8.1.5
Deploy user:        tidb
ID                 Role          Host       Ports        OS/Arch       Status   Data Dir                      Deploy Dir
--                 ----          ----       -----        -------       ------   --------                      ----------
10.0.1.21:2379     pd            10.0.1.21  2379/2380    linux/x86_64  Up|L|UI  /tidb-data/pd-2379            /tidb-deploy/pd-2379
10.0.1.11:4000     tidb          10.0.1.11  4000/10080   linux/x86_64  Up       -                             /tidb-deploy/tidb-4000
10.0.1.52:6000     tiproxy       10.0.1.52  6000/3080    linux/x86_64  Down     /tidb-data/tiproxy-6000       /tidb-deploy/tiproxy-6000
10.0.1.51:6000     tiproxy       10.0.1.51  6000/3080    linux/x86_64  Up       /tidb-data/tiproxy-6000       /tidb-deploy/tiproxy-6000
10.0.1.53:6001     tiproxy       10.0.1.53  6001         linux/x86_64  Up       -                             -
Total nodes: 5
`

func TestDiscoverProxies(t *testing.T) {
	ps := discoverProxies(source.ParseDisplay(displayWithProxy), map[string]string{"10.0.1.51:6000": "v1.3.0"})
	if len(ps) != 3 {
		t.Fatalf("应发现 3 个 TiProxy: %+v", ps)
	}
	p := ps[0]
	if p.ID != "tiproxy-1" || p.Type != "tiproxy" || p.Host != "10.0.1.51" || p.SQLPort != 6000 || p.APIPort != 3080 || p.Status != "up" || p.Version != "1.3.0" {
		t.Errorf("tiproxy-1: %+v", p)
	}
	if ps[1].ID != "tiproxy-2" || ps[1].Host != "10.0.1.52" || ps[1].Status != "down" || ps[1].Version != "" {
		t.Errorf("down 状态应映射为 down，且不用集群版本冒充: %+v", ps[1])
	}
	if ps[2].SQLPort != 6001 || ps[2].APIPort != 0 {
		t.Errorf("只有一个端口时 APIPort 为 0: %+v", ps[2])
	}
	// 没有 tiproxy 行：nil
	if got := discoverProxies(source.ParseDisplay("ID Role Host Ports OS/Arch Status Data Dir Deploy Dir\n--\n10.0.1.11:4000 tidb 10.0.1.11 4000/10080 linux/x86_64 Up - /d\n"), nil); got != nil {
		t.Errorf("没有 tiproxy 时应为 nil: %+v", got)
	}
}

// tiproxy 行不会被当成 Instances（roleType 不识别它），其余角色的解析不受影响。
func TestTiproxyNotInRoleType(t *testing.T) {
	if _, ok := roleType["tiproxy"]; ok {
		t.Fatal("tiproxy 不能进入 roleType（否则会进入 Instances，影响扩缩容 / 诊断 / 计数）")
	}
	if roleOrder("tiproxy") != 99 {
		t.Error("tiproxy 不应有排序位")
	}
}
