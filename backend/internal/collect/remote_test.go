// @author: kevinkelly
// @license: Apache License 2.0
// @file: remote_test.go
// @time: 2026/10/28

package collect

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"tidbops/internal/source"
)

// denyLocal 模拟「平台所在机器不是该集群的中控机」：tiup 与中控机（ctl/*）条目全部不可用，其余照常读回放数据。
type denyLocal struct{ source.Source }

func (d denyLocal) Fetch(ctx context.Context, it source.Item) ([]byte, error) {
	if strings.HasPrefix(it.Name, "tiup/") || strings.HasPrefix(it.Name, "ctl/") {
		return nil, errors.New("exec: \"tiup\": executable file not found in $PATH")
	}
	return d.Source.Fetch(ctx, it)
}

func remoteCollector(t *testing.T, remote bool) *Collector {
	t.Helper()
	rp, err := source.NewReplay(filepath.Join("..", "..", "..", "fixtures", "demo"))
	if err != nil {
		t.Skipf("缺少 fixtures/demo：%v", err)
	}
	return New(denyLocal{rp}, source.CatalogParams{Cluster: rp.Manifest().Cluster, Home: "/home/tidb", Remote: remote})
}

// 远程集群（独立部署，本机没有 tiup）：目录里没有 tiup / 中控机条目；拓扑退化为从 PD / cluster_info 推导，
// 并且不再把「tiup cluster display 失败」当成数据缺失。
func TestRemoteTopologyWithoutTiup(t *testing.T) {
	c := remoteCollector(t, true)
	for name := range c.Items {
		if strings.HasPrefix(name, "tiup/") || strings.HasPrefix(name, "ctl/") {
			t.Errorf("Remote 目录不应包含 %s", name)
		}
	}
	topo, errs := c.Topology(context.Background())
	if len(topo.Instances) == 0 {
		t.Fatalf("没有 tiup 时应能从 PD / cluster_info 推导出实例：%v", errs)
	}
	have := map[string]int{}
	for _, in := range topo.Instances {
		have[in.Type]++
	}
	if have["pd"] == 0 || have["tikv"] == 0 || have["tidb"] == 0 {
		t.Errorf("至少应推导出 tidb / pd / tikv：%v", have)
	}
	for _, e := range errs {
		if strings.Contains(e, "tiup cluster display") {
			t.Errorf("远程接入不应报 tiup 缺失：%q", e)
		}
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "远程接入") {
		t.Errorf("应说明拓扑是推导的（缺 TiCDC / 实例目录）：%v", errs)
	}
}

// 对照：非远程（同机部署）时 tiup 失败仍要报出来。
func TestLocalTopologyStillReportsTiupFailure(t *testing.T) {
	c := remoteCollector(t, false)
	_, errs := c.Topology(context.Background())
	if !strings.Contains(strings.Join(errs, "\n"), "tiup cluster display") {
		t.Errorf("同机部署时 tiup 不可用应报缺失：%v", errs)
	}
}

// 环境页：远程集群没有中控机信息，应是「未探测到」，而不是平台所在机器的信息。
func TestRemoteEnvHasNoControlNodeInfo(t *testing.T) {
	c := remoteCollector(t, true)
	topo, _ := c.Topology(context.Background())
	for _, it := range c.Env(context.Background(), topo) {
		switch it.Key {
		case "ctl.host", "ctl.os", "ctl.res", "tiup.ver", "tiup.cluster":
			if it.Detected {
				t.Errorf("远程集群不应探测到 %s = %q", it.Key, it.Value)
			}
		}
	}
}
