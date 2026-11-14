// @author: kevinkelly
// @license: Apache License 2.0
// @file: proxy_test.go
// @time: 2026/10/28

package collect_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tidbops/internal/collect"
	"tidbops/internal/source"
)

func replayCol(t *testing.T, name string) *collect.Collector {
	t.Helper()
	dir, _ := filepath.Abs(filepath.Join("..", "..", "..", "fixtures", name))
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Skipf("缺少回放数据包 %s: %v", name, err)
	}
	rp, err := source.NewReplay(dir)
	if err != nil {
		t.Fatal(err)
	}
	return collect.New(rp, source.CatalogParams{Cluster: rp.Manifest().Cluster, Home: "/home/tidb", Now: time.Now()})
}

// stg 数据包含 2 个 TiProxy：进入 Proxies，不进入 Instances（实例数 / 类型分布与引入前一致），诊断不受影响。
func TestReplayStgDiscoversProxies(t *testing.T) {
	col := replayCol(t, "stg")
	topo, errs := col.Topology(context.Background())
	if len(topo.Proxies) != 2 {
		t.Fatalf("stg 应有 2 个 TiProxy: %+v（errs=%v）", topo.Proxies, errs)
	}
	p1, p2 := topo.Proxies[0], topo.Proxies[1]
	if p1.ID != "tiproxy-1" || p1.Host != "10.20.1.41" || p1.SQLPort != 6000 || p1.APIPort != 3080 || p1.Status != "up" || p1.Type != "tiproxy" {
		t.Errorf("tiproxy-1: %+v", p1)
	}
	if p2.ID != "tiproxy-2" || p2.Host != "10.20.1.42" {
		t.Errorf("tiproxy-2: %+v", p2)
	}
	count := map[string]int{}
	for _, in := range topo.Instances {
		count[in.Type]++
	}
	if len(topo.Instances) != 9 || count["tidb"] != 2 || count["pd"] != 3 || count["tikv"] != 3 || count["tiflash"] != 1 || count["tiproxy"] != 0 {
		t.Errorf("TiProxy 不应进入 Instances: %v", count)
	}
	snap := col.Snapshot(context.Background(), "replay")
	if len(snap.Topology.Proxies) != 2 || len(snap.Topology.Instances) != 9 {
		t.Errorf("快照应带出 proxies 且 Instances 不变: %d / %d", len(snap.Topology.Proxies), len(snap.Topology.Instances))
	}
	for _, f := range snap.Findings {
		if f.Ref == "tiproxy-1" || f.Ref == "tiproxy-2" {
			t.Errorf("诊断不应针对 TiProxy: %+v", f)
		}
	}
}

// demo / dev 没有 TiProxy：Proxies 为空，Instances 数量不变。
func TestReplayDemoDevHaveNoProxies(t *testing.T) {
	for name, want := range map[string]int{"demo": 16, "dev": 5} {
		topo, _ := replayCol(t, name).Topology(context.Background())
		if len(topo.Proxies) != 0 || len(topo.Instances) != want {
			t.Errorf("%s: proxies=%d instances=%d（应为 0 / %d）", name, len(topo.Proxies), len(topo.Instances), want)
		}
	}
}
