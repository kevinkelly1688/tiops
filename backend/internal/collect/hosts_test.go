// @author: kevinkelly
// @license: Apache License 2.0
// @file: hosts_test.go
// @time: 2026/10/28

package collect_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tidbops/internal/model"
	"tidbops/internal/source"
)

func hostOf(t *testing.T, hosts []model.HostInfo, ip string) model.HostInfo {
	t.Helper()
	for _, h := range hosts {
		if h.IP == ip {
			return h
		}
	}
	t.Fatalf("没有主机 %s", ip)
	return model.HostInfo{}
}

func TestHostsMergedFromNodeExporter(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	topo, _ := col.Topology(context.Background())
	hosts := col.Hosts(context.Background(), topo)
	if len(hosts) != 16 {
		t.Fatalf("主机数应为 16（3 TiDB + 3 PD + 6 TiKV + 2 TiFlash + 2 TiCDC）: %d", len(hosts))
	}
	for i := 1; i < len(hosts); i++ { // 按 IP 数值排序：10.0.1.x 在 10.0.2.x 之前
		if hosts[i-1].IP >= hosts[i].IP && len(hosts[i-1].IP) == len(hosts[i].IP) {
			t.Errorf("主机应按 IP 升序: %s, %s", hosts[i-1].IP, hosts[i].IP)
		}
	}
	kv := hostOf(t, hosts, "10.0.2.11")
	if kv.Cores != 16 || kv.MemGB != 64 || kv.DiskGB != 3584 || !kv.Known {
		t.Errorf("TiKV 主机规格不对: %+v", kv)
	}
	if kv.OS != "Rocky Linux 9.3 (Blue Onyx)" || kv.Kernel != "5.14.0-362.8.1.el9_3.x86_64" || kv.Hostname != "tikv-01" {
		t.Errorf("系统 / 内核 / 主机名不对: %+v", kv)
	}
	if len(kv.Instances) != 1 || kv.Instances[0] != "tikv-1" || kv.AZ != "az1" || kv.Rack != "r01" {
		t.Errorf("归属不对: %+v", kv)
	}
	db := hostOf(t, hosts, "10.0.1.11")
	if db.Cores != 16 || db.MemGB != 32 || db.AZ != "" || db.Rack != "" {
		t.Errorf("TiDB 主机规格 / 归属不对（没有 zone / rack 标签应为空）: %+v", db)
	}
	if fl := hostOf(t, hosts, "10.0.3.11"); fl.Cores != 32 || fl.MemGB != 128 || fl.AZ != "az1" {
		t.Errorf("TiFlash 主机: %+v", fl)
	}
}

// 同一主机上有多个实例：实例 ID 归并到同一条，规格不重复统计。
func TestHostsGroupsInstancesOnSameHost(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	topo := model.Topology{Instances: []model.Instance{
		{ID: "tikv-1", Type: "tikv", Host: "10.0.2.11", AZ: "az1", Labels: map[string]string{"rack": "r01"}},
		{ID: "tidb-1", Type: "tidb", Host: "10.0.2.11", AZ: "—"},
		{ID: "tikv-9", Type: "tikv", Host: "10.0.9.9", AZ: "—"}, // 不在 Prometheus 里的主机
	}}
	hosts := col.Hosts(context.Background(), topo)
	if len(hosts) != 2 {
		t.Fatalf("应归并为 2 台主机: %+v", hosts)
	}
	h := hostOf(t, hosts, "10.0.2.11")
	if strings.Join(h.Instances, ",") != "tikv-1,tidb-1" || h.Cores != 16 || h.Rack != "r01" {
		t.Errorf("归并结果不对: %+v", h)
	}
	u := hostOf(t, hosts, "10.0.9.9")
	if u.Known || u.Cores != 0 || u.MemGB != 0 || u.OS != "" || u.Kernel != "" || u.DiskGB != 0 {
		t.Errorf("取不到规格的主机应全为零值且 known=false（不猜测）: %+v", u)
	}
}

// failingSource 让指定前缀的采集项失败，其余转发给真实（模拟）数据源。
type failingSource struct {
	source.Source
	failPrefix []string
}

func (s failingSource) Fetch(ctx context.Context, it source.Item) ([]byte, error) {
	for _, p := range s.failPrefix {
		if strings.HasPrefix(it.Name, p) {
			return nil, fmt.Errorf("simulated: %s 不可用", it.Name)
		}
	}
	return s.Source.Fetch(ctx, it)
}

// node_os_info 与磁盘指标缺失（较旧的 node_exporter）：其余规格照常，缺失项为空 / 0。
func TestHostsOptionalMetricsMissing(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	col.Src = failingSource{Source: col.Src, failPrefix: []string{"prom/node-os-info", "prom/node-disk-size"}}
	topo, _ := col.Topology(context.Background())
	hosts := col.Hosts(context.Background(), topo)
	kv := hostOf(t, hosts, "10.0.2.12")
	if kv.Cores != 16 || kv.MemGB != 64 || kv.Kernel == "" {
		t.Errorf("其余规格应照常: %+v", kv)
	}
	if kv.OS != "" || kv.DiskGB != 0 {
		t.Errorf("没有 node_os_info / 磁盘指标时 os 应为空、diskGB 为 0: %+v", kv)
	}
	// 全部 node_exporter 指标都没有
	col.Src = failingSource{Source: col.Src, failPrefix: []string{"prom/node-"}}
	for _, h := range col.Hosts(context.Background(), topo) {
		if h.Known || h.Cores != 0 {
			t.Fatalf("没有任何规格指标时 known 应为 false: %+v", h)
		}
	}
}

func TestTopologyCarriesInstanceDirs(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	topo, _ := col.Topology(context.Background())
	k2 := inst(t, topo, "tikv-2")
	if k2.DataDir != "/tidb-data/tikv-20160" || k2.DeployDir != "/tidb-deploy/tikv-20160" {
		t.Errorf("目录取自 display 的 Data Dir / Deploy Dir 列: %+v", k2)
	}
	// meta.yaml 里没写 log_dir：按 tiup 约定推断为 <deploy_dir>/log，并标记为推断
	if k2.LogDir != "/tidb-deploy/tikv-20160/log" || !k2.LogDirInferred {
		t.Errorf("日志目录应推断为 deploy_dir/log: %+v", k2)
	}
	// meta.yaml 里明确写了 log_dir（10.0.2.11）：优先使用，且不是推断值
	k1 := inst(t, topo, "tikv-1")
	if k1.LogDir != "/tidb-log/tikv-20160" || k1.LogDirInferred {
		t.Errorf("meta.yaml 的 log_dir 应优先: %+v", k1)
	}
	// TiDB 没有数据目录（display 里是 "-"）
	d := inst(t, topo, "tidb-1")
	if d.DataDir != "" || d.DeployDir != "/tidb-deploy/tidb-4000" {
		t.Errorf("TiDB 无数据目录: %+v", d)
	}
	if f := inst(t, topo, "tiflash-1"); f.DataDir != "/tidb-data/tiflash-9000" || f.DeployDir != "/tidb-deploy/tiflash-9000" {
		t.Errorf("TiFlash 目录: %+v", f)
	}
	// meta.yaml 不可用：不推断 LogDir（无法确认没有自定义 log_dir）
	col.Src = failingSource{Source: col.Src, failPrefix: []string{"tiup/meta"}}
	topo, _ = col.Topology(context.Background())
	if k := inst(t, topo, "tikv-2"); k.LogDir != "" || k.LogDirInferred || k.DeployDir == "" {
		t.Errorf("没有 meta.yaml 时 LogDir 应为空: %+v", k)
	}
}
