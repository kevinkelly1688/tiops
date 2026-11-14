// @author: kevinkelly
// @license: Apache License 2.0
// @file: fake.go
// @time: 2026/10/28

// Package fakecluster 在进程内模拟一个 TiDB 集群的只读接口：
// PD API、TiDB 状态端口、Prometheus、Alertmanager，以及中控机命令 / 只读 SQL 的输出。
//
// 用途：
//  1. 单元 / 集成测试：不依赖真实集群验证导出与回放；
//  2. `tidbops-fixtures demo`：在没有生产数据时生成一份演示用样例数据包，供 B1 之后的采集层开发使用。
//
// 默认数据（DefaultProfile）与原型页面里的示例集群（prod-tidb-01，3 TiDB / 3 PD / 6 TiKV / 2 TiFlash）保持一致；
// 多集群演示用 StagingProfile / DevProfile（见 profile.go）生成规模与负载明显不同的数据。
package fakecluster

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
)

// Cluster 是一组 httptest 服务。
type Cluster struct {
	Name                  string
	Profile               Profile
	PD, TiDB, Prom, Alert *httptest.Server
}

// Start 以默认预设（集群名为 name）启动全部模拟服务。
func Start(name string) *Cluster {
	p := DefaultProfile()
	p.Name = name
	return StartProfile(p)
}

// StartProfile 按预设启动全部模拟服务。
func StartProfile(p Profile) *Cluster {
	c := &Cluster{Name: p.Name, Profile: p}
	c.PD = httptest.NewServer(http.HandlerFunc(p.pdHandler))
	c.TiDB = httptest.NewServer(http.HandlerFunc(p.tidbHandler))
	c.Prom = httptest.NewServer(http.HandlerFunc(p.promHandler))
	c.Alert = httptest.NewServer(http.HandlerFunc(p.alertHandler))
	return c
}

// Close 关闭全部服务。
func (c *Cluster) Close() {
	for _, s := range []*httptest.Server{c.PD, c.TiDB, c.Prom, c.Alert} {
		s.Close()
	}
}

// Endpoints 返回逻辑端点 → URL。
func (c *Cluster) Endpoints() map[string]string {
	return map[string]string{"pd": c.PD.URL, "tidb": c.TiDB.URL, "prom": c.Prom.URL, "alert": c.Alert.URL}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.MarshalIndent(v, "", "  ")
	w.Write(b)
}

type m = map[string]any

func (p Profile) stores() []m {
	var out []m
	for i := 0; i < p.TiKV; i++ {
		out = append(out, m{
			"store": m{"id": i + 1, "address": fmt.Sprintf("%s:20160", p.host(2, 11+i)), "version": p.short(), "status_address": fmt.Sprintf("%s:20180", p.host(2, 11+i)),
				"labels": []m{{"key": "zone", "value": p.Zones[i%len(p.Zones)]}, {"key": "rack", "value": fmt.Sprintf("r%02d", 1+i)}, {"key": "host", "value": fmt.Sprintf("%s-2-%d", p.dashed(), 11+i)}}, "state_name": "Up"},
			"status": m{"capacity": fmt.Sprintf("%gTiB", p.CapTiB), "available": fmt.Sprintf("%.2fTiB", p.CapTiB-float64(p.UsedGiB[i])/1024), "used_size": fmt.Sprintf("%dGiB", p.UsedGiB[i]),
				"leader_count": p.Leaders[i], "region_count": p.Regions[i], "region_weight": 1, "start_ts": "2026-09-20T03:00:00Z"},
		})
	}
	return out
}

func (p Profile) pdHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/pd/api/v1/members":
		var members []m
		for i, h := range p.pdHosts() {
			members = append(members, m{"name": fmt.Sprintf("pd-%d", i+1), "member_id": 1001 + i, "client_urls": []string{fmt.Sprintf("http://%s:2379", h)}})
		}
		writeJSON(w, m{"members": members, "leader": m{"name": "pd-1", "member_id": 1001}})
	case r.URL.Path == "/pd/api/v1/stores":
		all := p.stores()
		zones := []string{"az1", "az2"}
		for i := 0; i < p.TiFlash; i++ {
			all = append(all, m{
				"store": m{"id": 21 + i, "address": fmt.Sprintf("%s:3930", p.host(3, 11+i)), "version": p.Version, "state_name": "Up",
					"labels": []m{{"key": "engine", "value": "tiflash"}, {"key": "zone", "value": zones[i%len(zones)]}}},
				"status": m{"capacity": "7TiB", "available": "3.6TiB", "used_size": "3.4TiB", "leader_count": 0, "region_count": 24100},
			})
		}
		writeJSON(w, m{"count": len(all), "stores": all})
	case r.URL.Path == "/pd/api/v1/config":
		writeJSON(w, m{"schedule": m{"max-store-down-time": "30m0s", "region-schedule-limit": 2048, "leader-schedule-limit": 4},
			"replication": m{"max-replicas": p.Replicas, "location-labels": "zone,host"}, "security": m{"cacert-path": "", "key-path": ""}})
	case r.URL.Path == "/pd/api/v1/health":
		var hs []m
		for i := range p.pdHosts() {
			hs = append(hs, m{"name": fmt.Sprintf("pd-%d", i+1), "member_id": 1001 + i, "health": true})
		}
		writeJSON(w, hs)
	case r.URL.Path == "/pd/api/v1/cluster":
		writeJSON(w, m{"id": 7001234567890, "max_peer_count": p.Replicas})
	case r.URL.Path == "/pd/api/v1/version":
		writeJSON(w, m{"version": p.Version})
	case r.URL.Path == "/pd/api/v1/stats/region":
		writeJSON(w, m{"count": p.TotalRegion, "empty_count": 12, "storage_size": 6200000, "storage_keys": 1200000000})
	case r.URL.Path == "/pd/api/v1/regions":
		// 抽样的 5 个 Region：都属于 table_id=112（orders），按 RowID 切成每 100 万行一个 Region
		var rs []m
		for i := 0; i < 5; i++ {
			rs = append(rs, region(20871+i, 2, TableKey(112, 12345+int64(i)*1000000), TableKey(112, 12345+int64(i+1)*1000000)))
		}
		writeJSON(w, m{"count": 5, "regions": rs})
	case r.URL.Path == "/pd/api/v1/regions/writeflow" || r.URL.Path == "/pd/api/v1/regions/readflow":
		// 流量 Top Region（含起止 Key）：20871 是 orders 的热点；30211 是 event_log（table_id=118）的尾部 Region
		rs := []m{region(20871, 2, TableKey(112, 12345), TableKey(112, 1012345))}
		if strings.HasSuffix(r.URL.Path, "writeflow") {
			rs = append(rs, region(30211, 2, TableKey(118, 880000000), TableKey(118, 881000000)))
		}
		writeJSON(w, m{"count": len(rs), "regions": rs})
	case strings.HasPrefix(r.URL.Path, "/pd/api/v1/hotspot/regions/"):
		writeJSON(w, m{"as_leader": m{"2": m{"total_flow_bytes": 224395264, "regions_count": 3, "statistics": []m{{"region_id": 20871, "hot_degree": 5, "flow_bytes": 90000000, "store_id": 2}}}}})
	default:
		http.NotFound(w, r)
	}
}

func (p Profile) tidbHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/status":
		writeJSON(w, m{"connections": int(p.TiDBConns[0]), "version": p.tidbVersion(), "git_hash": "0123456789abcdef"})
	case "/info/all":
		writeJSON(w, m{"servers_num": p.TiDB, "all_servers_info": m{"tidb-1": m{"ip": p.host(1, 11), "port": 4000, "status_port": 10080}}})
	default:
		http.NotFound(w, r)
	}
}

func (p Profile) alertHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v2/alerts" {
		http.NotFound(w, r)
		return
	}
	out := []m{}
	for _, a := range p.Alerts {
		inst := p.alertInstance(a)
		if inst == "" {
			continue
		}
		out = append(out, m{"labels": m{"alertname": a.Name, "instance": inst, "severity": a.Sev}, "annotations": m{"description": a.Desc}})
	}
	writeJSON(w, out)
}

func (p Profile) forecast() map[string]struct{ end, perDay float64 } {
	used := 0
	for _, u := range p.UsedGiB {
		used += u
	}
	return map[string]struct{ end, perDay float64 }{
		`sum(tikv_store_size_bytes{type="used"})`:         {float64(used) * (1 << 30), p.GrowGiBPerDay * (1 << 30)},
		`sum(tikv_store_size_bytes{type="capacity"})`:     {float64(p.TiKV) * p.CapTiB * (1 << 40), 0},
		`sum(tikv_raftstore_region_count{type="leader"})`: {float64(p.TotalRegion), p.GrowRegionDay},
	}
}

// promHandler 为 query_range 生成确定性的时间序列（正弦 + 偏移），便于测试断言。
func (p Profile) promHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/query_range" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query().Get("query")
	var start, end int64
	fmt.Sscan(r.URL.Query().Get("start"), &start)
	fmt.Sscan(r.URL.Query().Get("end"), &end)
	// 容量预测：7 天、每小时一个点的单序列，带上升趋势（默认：已用每天 +120 GiB，Region 每天 +150）
	if v, ok := p.forecast()[q]; ok {
		step := int64(3600)
		fmt.Sscan(r.URL.Query().Get("step"), &step)
		var vals [][]any
		for t := start; t <= end; t += step {
			x := v.end - v.perDay*float64(end-t)/86400
			if v.perDay > 0 {
				x *= 1 + 0.001*math.Sin(float64(t/step)/5) // 微小波动，回归斜率基本不受影响
			}
			vals = append(vals, []any{t, fmt.Sprintf("%.0f", x)})
		}
		writeJSON(w, m{"status": "success", "data": m{"resultType": "matrix", "result": []m{{"metric": m{}, "values": vals}}}})
		return
	}
	// 每个序列：instance 标签 + 基准值 + 微小波动
	type ser struct {
		inst   string
		base   float64
		labels map[string]string
		flat   bool // 常量序列（规格类指标：核数、内存、信息型指标），不加波动
	}
	L := p.Load
	var list []ser
	labeled := func(label string, vals map[string]float64) {
		names := make([]string, 0, len(vals))
		for k := range vals {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			list = append(list, ser{"", vals[k] * L, map[string]string{label: k}, false})
		}
	}
	tikvHosts := p.tikvHosts()
	tidbHosts := p.tidbHosts()
	allHosts := append(append(append([]string{}, tidbHosts...), p.pdHosts()...), tikvHosts...)
	hotTiKV, hotTiDB := "", ""
	if p.HotTiKV >= 0 && p.HotTiKV < len(tikvHosts) {
		hotTiKV = tikvHosts[p.HotTiKV]
	}
	if p.HotTiDB >= 0 && p.HotTiDB < len(tidbHosts) {
		hotTiDB = tidbHosts[p.HotTiDB]
	}
	// 热点 TiKV（HotTiKV）所在主机：写入链路与磁盘指标偏高，与它「写热点 + CPU 高」的故事一致；其余节点都在正常范围
	perTiKV := func(normal, hot float64) {
		for i, h := range tikvHosts {
			b := normal
			if i == p.HotTiKV {
				b = hot
			}
			list = append(list, ser{h + ":20180", b, nil, false})
		}
	}
	perHost := func(normal, hot float64) {
		for _, h := range allHosts {
			b := normal
			if h == hotTiKV {
				b = hot
			}
			list = append(list, ser{h + ":9100", b, nil, false})
		}
	}
	switch {
	// 第二批：PD / TiKV 写入链路 / 主机层（node_exporter）。这些分支必须放在其它分支之前（表达式里含 node_cpu_seconds_total、by (instance) 等）
	case strings.Contains(q, "pd_regions_status"):
		list = append(list, ser{"", 0, nil, true})
	case strings.Contains(q, "etcd_disk_wal_fsync"):
		for _, h := range p.pdHosts() {
			list = append(list, ser{h + ":2379", 0.004, nil, false})
		}
	case strings.Contains(q, "etcd_network_peer_round_trip"):
		for _, h := range p.pdHosts() {
			list = append(list, ser{h + ":2379", 0.002, nil, false})
		}
	case strings.Contains(q, "pd_scheduler_store_status"):
		for _, h := range tikvHosts {
			list = append(list, ser{"", 2, map[string]string{"address": h + ":20160"}, true})
		}
	case strings.Contains(q, "tikv_thread_cpu_seconds_total") && strings.Contains(q, "apply_"):
		perTiKV(0.5, 1.9)
	case strings.Contains(q, "tikv_engine_write_stall"):
		perTiKV(0, 3)
	case strings.Contains(q, "tikv_raftstore_append_log_duration"):
		perTiKV(0.02, 1.3)
	case strings.Contains(q, "tikv_raftstore_apply_log_duration"):
		perTiKV(0.04, 1.2)
	case strings.Contains(q, "tikv_scheduler_latch_wait_duration"):
		perTiKV(0.001, 0.002)
	case strings.Contains(q, "node_filesystem_files"):
		perHost(12, 12)
	case strings.Contains(q, "node_filesystem_readonly"):
		for _, h := range allHosts {
			list = append(list, ser{h + ":9100", 0, map[string]string{"mountpoint": "/"}, true})
		}
	case strings.Contains(q, "node_disk_read_time_seconds_total"):
		perHost(0.002, 0.004)
	case strings.Contains(q, "node_disk_write_time_seconds_total"):
		perHost(0.001, 0.02)
	case strings.HasPrefix(q, "node_load5"):
		perHost(0.4, 1.3)
	case q == "node_netstat_Tcp_CurrEstab":
		perHost(600, 900)
	// DB Time 分解：值是「每秒累计耗时（秒）」，各维度的比例参考原型页面的示例
	case strings.Contains(q, "tidb_server_handle_query_duration_seconds_sum"):
		labeled("sql_type", map[string]float64{"Select": 3.8, "Insert": 1.3, "Update": 1.7, "Delete": 0.3, "Commit": 1.2, "Begin": 0.1, "Rollback": 0.05, "Use": 0.02, "Set": 0.03})
	case strings.Contains(q, "tidb_server_get_token_duration_seconds_sum"):
		list = append(list, ser{"", 100000 * L, nil, false}) // 微秒/秒：即 0.1 秒/秒
	case strings.Contains(q, "tidb_session_parse_duration_seconds_sum"):
		list = append(list, ser{"", 0.3 * L, nil, false})
	case strings.Contains(q, "tidb_session_compile_duration_seconds_sum"):
		list = append(list, ser{"", 1.7 * L, nil, false})
	case strings.Contains(q, "tidb_session_execute_duration_seconds_sum"):
		list = append(list, ser{"", 7.9 * L, nil, false})
	case strings.Contains(q, "pd_client_cmd_handle_cmds_duration_seconds_sum"):
		list = append(list, ser{"", 0.9 * L, nil, false})
	case strings.Contains(q, "tidb_distsql_handle_query_duration_seconds_sum"):
		list = append(list, ser{"", 3.5 * L, nil, false})
	case strings.Contains(q, "tidb_tikvclient_request_seconds_sum"):
		labeled("type", map[string]float64{"Cop": 3.4, "Commit": 1.2, "Prewrite": 1.0, "Get": 0.9, "BatchGet": 0.3, "Scan": 0.2, "PessimisticLock": 0.15, "Rollback": 0.01})
	// 主机规格（node_exporter）。注意这些分支必须放在 node_cpu_seconds_total 的 CPU 使用率分支之前
	case strings.HasPrefix(q, "count(node_cpu_seconds_total"):
		for _, h := range p.specHosts() {
			list = append(list, ser{h.ip + ":9100", float64(h.cores), nil, true})
		}
	case q == "node_memory_MemTotal_bytes":
		for _, h := range p.specHosts() {
			list = append(list, ser{h.ip + ":9100", float64(h.memGiB) * (1 << 30), nil, true})
		}
	case q == "node_uname_info":
		for _, h := range p.specHosts() {
			list = append(list, ser{h.ip + ":9100", 1, map[string]string{"nodename": h.name, "release": p.OS.Kernel, "sysname": "Linux", "machine": "x86_64"}, true})
		}
	case q == "node_os_info":
		for _, h := range p.specHosts() {
			list = append(list, ser{h.ip + ":9100", 1, map[string]string{"id": p.OS.ID, "name": p.OS.Name, "pretty_name": p.OS.Pretty, "version_id": p.OS.VersionID}, true})
		}
	case strings.HasPrefix(q, "max(node_filesystem_size_bytes"):
		for _, h := range p.specHosts() {
			list = append(list, ser{h.ip + ":9100", float64(h.diskGiB) * (1 << 30), nil, true})
		}
	case strings.Contains(q, "node_cpu_seconds_total"):
		for _, h := range allHosts {
			b := 38.0
			if h == hotTiKV {
				b = 92
			}
			list = append(list, ser{h + ":9100", b, nil, false})
		}
	case strings.Contains(q, "node_memory_MemAvailable_bytes"):
		for _, h := range allHosts {
			b := 55.0
			if h == hotTiDB {
				b = 86
			}
			list = append(list, ser{h + ":9100", b, nil, false})
		}
	case strings.Contains(q, "tidb_server_query_total") && strings.Contains(q, "by (instance)"):
		for i, h := range tidbHosts {
			list = append(list, ser{h + ":10080", p.TiDBQPS[i], nil, false})
		}
	case strings.Contains(q, "tidb_server_connections") && strings.Contains(q, "by (instance)"):
		for i, h := range tidbHosts {
			list = append(list, ser{h + ":10080", p.TiDBConns[i], nil, false})
		}
	case strings.Contains(q, "tikv_engine_flow_bytes"):
		for i, h := range tikvHosts {
			list = append(list, ser{h + ":20180", p.FlowMB[i] * 1e6, nil, false})
		}
	case strings.Contains(q, "by (instance)"):
		base := 100.0
		switch {
		case strings.Contains(q, "raftstore"):
			base = 0.4
		case strings.Contains(q, "cpu"):
			base = 4
		case strings.Contains(q, "store_size") && strings.Contains(q, "capacity"):
			base = p.CapTiB * 1e12
		case strings.Contains(q, "store_size"):
			base = p.StoreUsedBase
		case strings.Contains(q, "region_count"):
			base = p.RegionBase
		}
		for i, h := range tikvHosts {
			b := base
			if i == p.HotTiKV && strings.Contains(q, "raftstore") {
				b = 0.92
			}
			list = append(list, ser{h + ":20180", b, nil, false})
		}
	default:
		base := 100.0
		switch {
		case strings.Contains(q, "tidb_server_query_total"):
			base = p.QPSTotal
		case strings.Contains(q, "histogram_quantile"):
			base = 0.03
		case strings.Contains(q, "connections"):
			base = p.ConnTotal
		}
		list = append(list, ser{"", base, nil, false})
	}
	var result []m
	for s, sr := range list {
		var vals [][]any
		for t, i := start, 0; t <= end && i < 120; t, i = t+60, i+1 {
			v := sr.base * (1 + 0.05*math.Sin(float64(i)/6+float64(s)))
			if sr.flat {
				v = sr.base
			}
			vals = append(vals, []any{t, fmt.Sprintf("%.4f", v)})
		}
		metric := m{}
		if sr.inst != "" {
			metric["instance"] = sr.inst
		}
		for k, v := range sr.labels {
			metric[k] = v
		}
		result = append(result, m{"metric": metric, "values": vals})
	}
	writeJSON(w, m{"status": "success", "data": m{"resultType": "matrix", "result": result}})
}

// specHost 是模拟主机的规格（node_exporter 指标的来源）。
type specHost struct {
	ip, name               string
	cores, memGiB, diskGiB int
}

// specHosts 返回模拟集群全部主机的规格。默认预设（17 台）：TiKV 16 核 / 64GB / 3.5T，TiDB 16 核 / 32GB / 500G，
// PD、监控、TiCDC 8 核 / 16GB，TiFlash 32 核 / 128GB / 7T；系统 Rocky Linux 9.3。
func (p Profile) specHosts() []specHost {
	var hs []specHost
	add := func(sub, n int, name string, s HostSpec) {
		for i := 0; i < n; i++ {
			hs = append(hs, specHost{p.host(sub, 11+i), fmt.Sprintf("%s-%02d", name, i+1), s.Cores, s.MemGiB, s.DiskGiB})
		}
	}
	add(1, p.TiDB, "tidb", p.SpecTiDB)
	for i, h := range p.pdHosts() {
		hs = append(hs, specHost{h, fmt.Sprintf("pd-%02d", i+1), p.SpecPD.Cores, p.SpecPD.MemGiB, p.SpecPD.DiskGiB})
	}
	hs = append(hs, specHost{p.monitor(), "monitor-01", p.SpecMon.Cores, p.SpecMon.MemGiB, p.SpecMon.DiskGiB})
	add(2, p.TiKV, "tikv", p.SpecTiKV)
	add(3, p.TiFlash, "tiflash", p.SpecFlash)
	add(4, p.CDC, "cdc", p.SpecCDC)
	return hs
}

func (p Profile) displayText() string {
	var b strings.Builder
	total := 0
	line := func(a, b2, c, d, e, f, g, h string) {
		fmt.Fprintf(&b, "%-18s %-13s %-10s %-12s %-13s %-8s %-29s %s\n", a, b2, c, d, e, f, g, h)
	}
	row := func(id, role, host, ports, status, data, deploy string) {
		total++
		line(id, role, host, ports, "linux/x86_64", status, data, deploy)
	}
	fmt.Fprintf(&b, "Cluster type:       tidb\nCluster name:       %s\nCluster version:    %s\nDeploy user:        tidb\nSSH type:           builtin\nDashboard URL:      http://%s:2379/dashboard\nGrafana URL:        http://%s:3000\n",
		p.Name, p.Version, p.pdHosts()[0], p.monitor())
	line("ID", "Role", "Host", "Ports", "OS/Arch", "Status", "Data Dir", "Deploy Dir")
	line("--", "----", "----", "-----", "-------", "------", "--------", "----------")
	mon := p.monitor()
	row(mon+":9093", "alertmanager", mon, "9093/9094", "Up", "/tidb-data/alertmanager-9093", "/tidb-deploy/alertmanager-9093")
	row(mon+":3000", "grafana", mon, "3000", "Up", "-", "/tidb-deploy/grafana-3000")
	for i, h := range p.pdHosts() {
		st := "Up"
		if i == 0 {
			st = "Up|L|UI"
		}
		row(h+":2379", "pd", h, "2379/2380", st, "/tidb-data/pd-2379", "/tidb-deploy/pd-2379")
	}
	row(mon+":9090", "prometheus", mon, "9090/12020", "Up", "/tidb-data/prometheus-9090", "/tidb-deploy/prometheus-9090")
	for _, h := range p.tidbHosts() {
		row(h+":4000", "tidb", h, "4000/10080", "Up", "-", "/tidb-deploy/tidb-4000")
	}
	for _, h := range p.tikvHosts() {
		row(h+":20160", "tikv", h, "20160/20180", "Up", "/tidb-data/tikv-20160", "/tidb-deploy/tikv-20160")
	}
	for _, h := range p.tiflashHosts() {
		row(h+":3930", "tiflash", h, "9000/8123/3930/20170/20292/8234", "Up", "/tidb-data/tiflash-9000", "/tidb-deploy/tiflash-9000")
	}
	for _, h := range p.cdcHosts() {
		row(h+":8300", "cdc", h, "8300", "Up", "/tidb-data/cdc-8300", "/tidb-deploy/cdc-8300")
	}
	for _, h := range p.tiproxyHosts() {
		row(h+":6000", "tiproxy", h, "6000/3080", "Up", "/tidb-data/tiproxy-6000", "/tidb-deploy/tiproxy-6000")
	}
	fmt.Fprintf(&b, "Total nodes: %d\n", total)
	return b.String()
}

func (p Profile) listText() string {
	path := "/home/tidb/.tiup/storage/cluster/clusters/" + p.Name
	return fmt.Sprintf("Name          User  Version  Path                                               PrivateKey\n----          ----  -------  ----                                               ----------\n%s  tidb  %s   %s  %s/ssh/id_rsa\n", p.Name, p.Version, path, path)
}

func (p Profile) metaText() string {
	return fmt.Sprintf(`user: tidb
tidb_version: %s
topology:
  global:
    user: tidb
    ssh_port: 22
  monitored:
    node_exporter_port: 9100
  server_configs:
    tidb:
      security.cluster-ssl-key: ""
      password: "not-a-real-password"
  pd_servers:
    - host: %s
  tikv_servers:
    - host: %s
      port: 20160
      status_port: 20180
      deploy_dir: /tidb-deploy/tikv-20160
      data_dir: /tidb-data/tikv-20160
      log_dir: /tidb-log/tikv-20160
`, p.Version, p.pdHosts()[0], p.tikvHosts()[0])
}

func (p Profile) osRelease() string {
	return fmt.Sprintf("NAME=\"%s\"\nVERSION=\"%s\"\nID=\"%s\"\nVERSION_ID=\"%s\"\nPRETTY_NAME=\"%s\"\n", p.OS.Name, strings.TrimSpace(strings.TrimPrefix(p.OS.Pretty, p.OS.Name)), p.OS.ID, p.OS.VersionID, p.OS.Pretty)
}

// Exec 返回一个模拟命令与只读 SQL 输出的执行函数（签名与 source.ExecFunc 相同），数据来自默认预设。
func Exec() func(ctx context.Context, argv []string, env []string) ([]byte, error) {
	return DefaultProfile().Exec()
}

// Exec 返回该预设的模拟命令与只读 SQL 执行函数。
func (p Profile) Exec() func(ctx context.Context, argv []string, env []string) ([]byte, error) {
	return func(_ context.Context, argv []string, _ []string) ([]byte, error) {
		cmd := strings.Join(argv, " ")
		switch {
		case cmd == "hostname -I":
			return []byte(p.Net + ".0.5 \n"), nil
		case cmd == "uname -r":
			return []byte("5.14.0-362.el9.x86_64\n"), nil
		case cmd == "nproc":
			return []byte("8\n"), nil
		case cmd == "free -m":
			return []byte("               total        used        free\nMem:           32010        8120       12044\nSwap:              0           0           0\n"), nil
		case cmd == "timedatectl":
			return []byte("               Local time: Mon 2026-10-05 10:00:00 CST\n                Time zone: Asia/Shanghai (CST, +0800)\nSystem clock synchronized: yes\n"), nil
		case strings.HasSuffix(cmd, " --version") && !strings.Contains(cmd, "cluster"):
			return []byte("1.16.1 tiup\nGo Version: go1.22.5\nGit Ref: v1.16.1\n"), nil
		case strings.HasSuffix(cmd, "cluster --version"):
			return []byte("tiup version v1.16.1 tiup\n"), nil
		case strings.HasSuffix(cmd, "cluster list"):
			return []byte(p.listText()), nil
		case strings.Contains(cmd, "cluster display"):
			return []byte(p.displayText()), nil
		case strings.Contains(cmd, "-e"):
			return p.sqlOutput(argv[len(argv)-1])
		}
		return nil, fmt.Errorf("fakecluster: 未模拟的命令 %q", cmd)
	}
}

// ReadFile 返回一个模拟文件读取函数（默认预设）。
func ReadFile(path string) ([]byte, error) { return DefaultProfile().ReadFile(path) }

// ReadFile 返回该预设的模拟文件读取。
func (p Profile) ReadFile(path string) ([]byte, error) {
	switch {
	case path == "/etc/os-release":
		return []byte(p.osRelease()), nil
	case strings.HasSuffix(path, "/meta.yaml"):
		return []byte(p.metaText()), nil
	}
	return nil, fmt.Errorf("fakecluster: 未模拟的文件 %q", path)
}

func (p Profile) sqlOutput(q string) ([]byte, error) {
	l := strings.ToLower(q)
	switch {
	case strings.Contains(l, "tidb_version()"):
		return []byte("tidb_version()\nRelease Version: " + p.Version + "\\nEdition: Community\\nGit Commit Hash: 0123abcd\n"), nil
	case strings.Contains(l, "cluster_info"):
		var b strings.Builder
		b.WriteString("TYPE\tINSTANCE\tSTATUS_ADDRESS\tVERSION\n")
		for _, h := range p.tidbHosts() {
			fmt.Fprintf(&b, "tidb\t%s:4000\t%s:10080\t%s\n", h, h, p.tidbVersion())
		}
		for _, h := range p.pdHosts() {
			fmt.Fprintf(&b, "pd\t%s:2379\t%s:2379\t%s\n", h, h, p.Version)
		}
		for _, h := range p.tikvHosts() {
			fmt.Fprintf(&b, "tikv\t%s:20160\t%s:20180\t%s\n", h, h, p.short())
		}
		return []byte(b.String()), nil
	case strings.Contains(l, "session_variables"):
		return []byte("VARIABLE_NAME\tVARIABLE_VALUE\ntidb_mem_quota_query\t1073741824\ntidb_enable_prepared_plan_cache\tON\ntidb_gc_life_time\t10m0s\ntidb_replica_read\tleader\ntidb_slow_log_threshold\t300\ntidb_distsql_scan_concurrency\t15\n"), nil
	case strings.Contains(l, "cluster_config"):
		var b strings.Builder
		b.WriteString("TYPE\tINSTANCE\tKEY\tVALUE\n")
		row := func(typ, inst, key, val string) { fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", typ, inst, key, val) }
		for i, h := range p.tikvHosts() {
			pool := "2"
			if i == p.DriftTiKV { // 单独调过 Raftstore 线程数 → 配置漂移
				pool = "3"
			}
			inst := h + ":20160"
			row("tikv", inst, "storage.block-cache.capacity", "16GiB")
			row("tikv", inst, "raftstore.store-pool-size", pool)
			row("tikv", inst, "raftstore.apply-pool-size", "2")
			row("tikv", inst, "storage.scheduler-worker-pool-size", "4")
			row("tikv", inst, "readpool.unified.max-thread-count", "16")
			row("tikv", inst, "rocksdb.max-background-jobs", "8")
			row("tikv", inst, "raftstore.raft-max-inflight-msgs", "256")
			row("tikv", inst, "server.grpc-concurrency", "5")
			row("tikv", inst, "log.level", "info")
		}
		pds, tidbs := p.pdHosts(), p.tidbHosts()
		for i := 0; i < len(tidbs) || i < len(pds); i++ {
			if i < len(tidbs) {
				inst := tidbs[i] + ":4000"
				row("tidb", inst, "performance.max-procs", "0")
				row("tidb", inst, "performance.token-limit", "1000")
				row("tidb", inst, "log.level", "info")
			}
			if i < len(pds) {
				row("pd", pds[i]+":2379", "log.level", "info")
			}
		}
		for _, h := range p.tiflashHosts() {
			inst := h + ":3930"
			row("tiflash", inst, "profiles.default.max_memory_usage", "0")
			row("tiflash", inst, "profiles.default.max_threads", "0")
			row("tiflash", inst, "logger.level", "info")
		}
		return []byte(b.String()), nil
	case strings.Contains(l, "tidb_decode_plan"):
		// mysql --batch 会把计划文本里的换行与制表符转义成两个字符的 \n、\t
		hdr := "\\tid\\ttask\\testRows\\toperator info\\tactRows\\texecution info\\tmemory\\tdisk"
		row := func(id, task, est, info, act, exec string) string {
			return "\\t" + id + "\\t" + task + "\\t" + est + "\\t" + info + "\\t" + act + "\\t" + exec + "\\t1.2 MB\\tN/A"
		}
		p1 := strings.Join([]string{hdr,
			row("TopN_8", "root", "12.00", "orders.created_at:desc, offset:0, count:20", "20", "time:2.8s, loops:2"),
			row("└─TableReader_16", "root", "12.00", "data:Selection_15", "380000", "time:2.7s, loops:380"),
			row("  └─Selection_15", "cop[tikv]", "12.00", "eq(shop.orders.user_id, ?), eq(shop.orders.status, ?)", "380000", "tikv_task:{time:2.6s}"),
			row("    └─TableFullScan_14", "cop[tikv]", "2400000.00", "table:orders, keep order:false, stats:pseudo", "4200000", "tikv_task:{time:2.4s}"),
		}, "\\n")
		p2 := strings.Join([]string{hdr,
			row("HashAgg_9", "root", "36.00", "group by:shop.orders.region, funcs:count(1)->Column#9", "36", "time:9.6s, loops:2"),
			row("└─HashJoin_21", "root", "61000000.00", "inner join, equal:[eq(shop.order_item.order_id, shop.orders.id)]", "61000000", "time:9.4s, loops:60000"),
			row("  ├─IndexLookUp_32(Build)", "root", "61000000.00", "", "61000000", "time:3.1s, loops:59600"),
			row("  │ ├─IndexRangeScan_30(Build)", "cop[tikv]", "61000000.00", "table:oi, index:idx_created(created_at), range:(?,+inf], keep order:false", "61000000", "tikv_task:{time:1.9s}"),
			row("  │ └─TableRowIDScan_31(Probe)", "cop[tikv]", "61000000.00", "table:oi, keep order:false", "61000000", "tikv_task:{time:2.2s}"),
			row("  └─TableReader_35(Probe)", "root", "2400000.00", "data:TableFullScan_34", "2400000", "time:0.4s"),
			row("    └─TableFullScan_34", "cop[tikv]", "2400000.00", "table:o, keep order:false", "2400000", "tikv_task:{time:0.3s}"),
		}, "\\n")
		return []byte("Digest\tPlan\n" + "a3f9c1\t" + p1 + "\n7bd024\t" + p2 + "\n"), nil
	case strings.Contains(l, "cluster_slow_query") && strings.Contains(l, "group by digest"):
		// 慢日志扩展聚合（sql/slow-ext）：a3f9c1 统计信息是 pseudo；7bd024 换了执行计划（上一小时是 p2old，平均 2s，现在 9.7s）；其余无异常
		hdr := "Digest\tPlan_digest\tcnt\tcur_cnt\tcur_sum\tprev_sum\tpseudo_cnt\tlock_cnt\tregion_miss_cnt\tbusy_cnt\tpd_cnt\tmax_resolve_lock\tmax_del_skipped\tmax_retry\n"
		rows := []string{
			"a3f9c1\tp1\t2\t2\t5.68\t0\t2\t0\t0\t0\t0\t0\t0\t0",
			"7bd024\tp2\t1\t1\t9.7\t0\t0\t0\t0\t0\t0\t0\t0\t0",
			"7bd024\tp2old\t3\t0\t0\t6.0\t0\t0\t0\t0\t0\t0\t0\t0",
			"e810aa\tp3\t2\t2\t0.84\t0\t0\t0\t0\t0\t0\t0\t0\t0",
			"5c7e12\tp4\t1\t1\t1.62\t0\t0\t0\t0\t0\t0\t0\t0\t0",
			"2dd7f0\tp6\t1\t1\t0.91\t0\t0\t0\t0\t0\t0\t0\t0\t0",
			"91ab33\tp5\t1\t1\t0.18\t0\t0\t0\t0\t0\t0\t0\t0\t0",
		}
		return []byte(hdr + strings.Join(rows, "\n") + "\n"), nil
	case strings.Contains(l, "cluster_slow_query"):
		hdr := "Time\tDigest\tPlan_digest\tDB\tUser\tQuery_time\tParse_time\tCompile_time\tProcess_time\tWait_time\tTotal_keys\tProcess_keys\tResult_rows\tMem_max\tIndex_names\tQuery\n"
		rows := []string{
			"2026-10-05 09:58:01.123\ta3f9c1\tp1\tshop\tapp_order\t2.91\t0.001\t0.01\t2.7\t0.1\t4200000\t120000\t20\t100663296\t\tSELECT * FROM orders WHERE user_id = 10086 AND phone = '13800138000' AND status IN (1, 2, 3) ORDER BY created_at DESC LIMIT 20",
			"2026-10-05 09:58:42.001\ta3f9c1\tp1\tshop\tapp_order\t2.77\t0.001\t0.01\t2.6\t0.1\t4150000\t118000\t20\t100663296\t\tSELECT * FROM orders WHERE user_id = 20001 AND phone = '13900139000' AND status IN (4, 5) ORDER BY created_at DESC LIMIT 20",
			"2026-10-05 09:57:10.456\t7bd024\tp2\tshop\tapp_report\t9.7\t0.002\t0.2\t9.0\t0.3\t98000000\t61000000\t36\t2254857830\tidx_created\tSELECT o.region, COUNT(*) FROM order_item oi JOIN orders o ON oi.order_id = o.id WHERE oi.created_at > '2026-09-01' GROUP BY o.region",
			"2026-10-05 09:56:30.789\te810aa\tp3\tshop\tapp_stock\t0.46\t0.001\t0.001\t0.01\t0.41\t3\t3\t0\t4096\tuniq_sku\tUPDATE inventory SET stock = stock - 1 WHERE sku = 'SKU-1001' AND stock >= 1",
			"2026-10-05 09:56:31.101\te810aa\tp3\tshop\tapp_stock\t0.38\t0.001\t0.001\t0.01\t0.35\t3\t3\t0\t4096\tuniq_sku\tUPDATE inventory SET stock = stock - 1 WHERE sku = 'SKU-1001' AND stock >= 1",
			"2026-10-05 09:58:07.456\t5c7e12\tp4\tshop\tapp_web\t1.62\t0.001\t0.004\t1.5\t0.05\t1800000\t1800000\t420\t50331648\t\tSELECT id, name FROM users WHERE DATE(created_at) = '2026-09-30'",
			"2026-10-05 09:55:12.222\t2dd7f0\tp6\tshop\tapp_web\t0.91\t0.001\t0.003\t0.85\t0.03\t760000\t760000\t50\t23068672\t\tSELECT id, title FROM product WHERE title LIKE '%手机%' LIMIT 50",
			"2026-10-05 09:54:00.010\t91ab33\tp5\tlog\tapp_log\t0.18\t0.0005\t0.001\t0.02\t0.12\t2\t2\t0\t2048\t\tINSERT INTO event_log (payload, created_at) VALUES ('{\"a\":1}', NOW())",
		}
		if p.SlowLimit > 0 && p.SlowLimit < len(rows) {
			rows = rows[:p.SlowLimit]
		}
		return []byte(hdr + strings.Join(rows, "\n") + "\n"), nil
	case strings.Contains(l, "cluster_statements_summary"):
		rows := []string{
			"a3f9c1\tshop\t18240\t2840000000\t6100000000\t51801600000000\t100663296\tselect * from `orders` where `user_id` = ? and `phone` = ? and `status` in ( ... ) order by `created_at` desc limit ?\n",
			"7bd024\tshop\t620\t9700000000\t21300000000\t6014000000000\t2254857830\tselect `o` . `region` , count ( ? ) from `order_item` `oi` join `orders` `o` on `oi` . `order_id` = `o` . `id` where `oi` . `created_at` > ? group by `o` . `region`\n",
			"e810aa\tshop\t210400\t410000000\t3200000000\t86264000000000\t4096\tupdate `inventory` set `stock` = `stock` - ? where `sku` = ? and `stock` >= ?\n",
			"5c7e12\tshop\t3900\t1620000000\t2900000000\t6318000000000\t50331648\tselect `id` , `name` from `users` where date ( `created_at` ) = ?\n",
			"91ab33\tlog\t884000\t12000000\t180000000\t10608000000000\t2048\tinsert into `event_log` ( `payload` , `created_at` ) values ( ... )\n",
			"2dd7f0\tshop\t15800\t880000000\t1700000000\t13904000000000\t23068672\tselect `id` , `title` from `product` where `title` like ? limit ?\n",
		}
		if p.SlowLimit > 0 && p.SlowLimit < len(rows) {
			rows = rows[:p.SlowLimit]
		}
		return []byte("DIGEST\tSCHEMA_NAME\tEXEC_COUNT\tAVG_LATENCY\tMAX_LATENCY\tSUM_LATENCY\tAVG_MEM\tDIGEST_TEXT\n" + strings.Join(rows, "")), nil
	case strings.Contains(l, "tidb_hot_regions"):
		return []byte("REGION_ID\tTABLE_NAME\tTYPE\tFLOW_BYTES\n20871\torders\twrite\t90000000\n30211\tevent_log\twrite\t60000000\n"), nil
	}
	return nil, fmt.Errorf("fakecluster: 未模拟的 SQL %q", q)
}

// ---------------------------------------------------------------- Region Key 编码 ----

func cmpInt(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v)^(1<<63))
	return b
}

// MemcmpEncode 实现 TiKV 的 memcomparable 字节编码：每 8 字节一组，后跟 0xFF - 填充字节数。
func MemcmpEncode(raw []byte) []byte {
	var out []byte
	for i := 0; ; i += 8 {
		rest := raw[min(i, len(raw)):]
		if len(rest) >= 8 {
			out = append(out, rest[:8]...)
			if len(rest) == 8 {
				// 恰好整组结束时还要补一个全填充的空组
				out = append(out, 0xFF)
				out = append(out, make([]byte, 8)...)
				out = append(out, 0xF7)
				return out
			}
			out = append(out, 0xFF)
			continue
		}
		pad := 8 - len(rest)
		out = append(out, rest...)
		out = append(out, make([]byte, pad)...)
		out = append(out, byte(0xFF-pad))
		return out
	}
}

// TableKey 返回 TiDB 行键 t{tableID}_r{rowID} 经 memcomparable 编码后的十六进制（与 PD 返回的 start_key / end_key 同形式）。
func TableKey(tableID, rowID int64) string {
	raw := append([]byte{'t'}, cmpInt(tableID)...)
	raw = append(raw, '_', 'r')
	raw = append(raw, cmpInt(rowID)...)
	return strings.ToUpper(hex.EncodeToString(MemcmpEncode(raw)))
}

// IndexKey 返回 TiDB 索引键 t{tableID}_i{indexID} 经 memcomparable 编码后的十六进制。
func IndexKey(tableID, indexID int64) string {
	raw := append([]byte{'t'}, cmpInt(tableID)...)
	raw = append(raw, '_', 'i')
	raw = append(raw, cmpInt(indexID)...)
	return strings.ToUpper(hex.EncodeToString(MemcmpEncode(raw)))
}

func region(id, leaderStore int, start, end string) m {
	return m{"id": id, "start_key": start, "end_key": end, "leader": m{"store_id": leaderStore}, "approximate_size": 96, "approximate_keys": 960000}
}
