// @author: kevinkelly
// @license: Apache License 2.0
// @file: topology.go
// @time: 2026/10/28

package collect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"tidbops/internal/model"
	"tidbops/internal/source"
)

var roleType = map[string]string{"tidb": "tidb", "pd": "pd", "tikv": "tikv", "tiflash": "tiflash", "cdc": "ticdc"}

func tiupStatus(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.HasPrefix(l, "up"):
		return "up"
	case strings.Contains(l, "offline"):
		return "offline"
	case strings.Contains(l, "joining"):
		return "joining"
	}
	return "down"
}

func storeStatus(state, cur string) string {
	switch strings.ToLower(state) {
	case "offline":
		return "offline"
	case "down", "disconnected", "tombstone":
		return "down"
	}
	return cur
}

func parsePorts(role, ports string) (port, status int) {
	p := strings.Split(ports, "/")
	atoi := func(i int) int {
		if i < len(p) {
			v, _ := strconv.Atoi(p[i])
			return v
		}
		return 0
	}
	if role == "tiflash" && len(p) >= 3 {
		return atoi(2), atoi(3) // flash service port 3930 / proxy 20170
	}
	return atoi(0), atoi(1)
}

// Topology 汇集 tiup display、PD stores / members、cluster_info 与 Prometheus 主机指标，得到集群拓扑。
// 任何一类数据缺失都会降级而不是失败，缺失原因通过第二个返回值给出。
func (c *Collector) Topology(ctx context.Context) (model.Topology, []string) {
	var errs []string
	topo := model.Topology{}
	var ci source.ClusterInfo
	if b, err := c.get(ctx, "tiup/cluster-display"); err == nil {
		ci = source.ParseDisplay(string(b))
	} else if !c.Params.Remote {
		errs = append(errs, "tiup cluster display: "+err.Error())
	}
	var stores []Store
	if b, err := c.get(ctx, "pd/stores"); err == nil {
		stores, err = ParseStores(b)
		if err != nil {
			errs = append(errs, err.Error())
		}
	} else {
		errs = append(errs, "pd/stores: "+err.Error())
	}
	leaderHost := ""
	var pdHosts []string
	if b, err := c.get(ctx, "pd/members"); err == nil {
		leaderHost, pdHosts, _ = ParseMembers(b)
	} else {
		errs = append(errs, "pd/members: "+err.Error())
	}
	ver := map[string]string{} // 实例地址 → 版本（来自 information_schema.cluster_info）
	var tidbAddrs []string
	if b, err := c.get(ctx, "sql/cluster-info"); err == nil {
		_, rows := parseTSV(b)
		for _, r := range rows {
			ver[r["instance"]] = strings.TrimPrefix(r["version"], "v")
			if strings.EqualFold(r["type"], "tidb") {
				tidbAddrs = append(tidbAddrs, r["instance"])
			}
		}
	}
	metric := func(name string) map[string]float64 {
		b, err := c.get(ctx, name)
		if err != nil {
			return nil
		}
		s, err := ParseMatrix(b)
		if err != nil {
			return nil
		}
		return ByHost(s)
	}
	cpuBy, memBy := metric("prom/node-cpu"), metric("prom/node-mem")
	qpsBy, connBy := metric("prom/tidb-qps-instance"), metric("prom/tidb-conn-instance")
	wrBy, raftBy := metric("prom/tikv-write-bytes"), metric("prom/tikv-raftstore-cpu")
	if cpuBy == nil {
		errs = append(errs, "缺少 Prometheus 节点指标（node-cpu / node-mem）：CPU / 内存将显示为 0，请确认已部署 node_exporter")
	}

	// 1. 以 tiup display 为主；没有则退化到 PD / cluster_info
	type base struct {
		role, host, ports, status string
		leader                    bool
		id, dataDir, deployDir    string // display 的 ID（host:port）与目录列
	}
	var bases []base
	for _, in := range ci.Instances {
		if _, ok := roleType[in.Role]; !ok {
			continue
		}
		bases = append(bases, base{in.Role, in.Host, in.Ports, in.Status, strings.Contains(in.Status, "|L"), in.ID, in.DataDir, in.DeployDir})
	}
	meta := c.readMetaDirs(ctx) // 仅用于 LogDir；读不到不影响其余字段
	if len(bases) == 0 {
		for _, a := range tidbAddrs {
			bases = append(bases, base{role: "tidb", host: hostOf(a), ports: portOf(a) + "/10080", status: "Up"})
		}
		for _, h := range pdHosts {
			bases = append(bases, base{role: "pd", host: h, ports: "2379/2380", status: "Up", leader: h == leaderHost})
		}
		for _, s := range stores {
			role, ports := "tikv", portOf(s.Address)+"/"+strconv.Itoa(atoi(portOf(s.Address))+20)
			if s.IsFlash() {
				role, ports = "tiflash", "9000/8123/"+portOf(s.Address)
			}
			bases = append(bases, base{role: role, host: s.Host, ports: ports, status: "Up"})
		}
		if len(bases) > 0 {
			if c.Params.Remote {
				errs = append(errs, "远程接入（平台所在机器没有 tiup）：拓扑由 PD / cluster_info 推导，不含 TiCDC 等组件，也没有实例目录")
			} else {
				errs = append(errs, "tiup 拓扑不可用，已退化为从 PD / cluster_info 推导（缺少 TiCDC 等组件）")
			}
		}
	}

	storeByHost := map[string]Store{}
	for _, s := range stores {
		key := s.Host
		if !s.IsFlash() {
			key = s.Address
		}
		storeByHost[key] = s
	}

	// 2. 稳定编号：按角色分组，主机 + 端口排序
	sort.SliceStable(bases, func(i, j int) bool {
		if bases[i].role != bases[j].role {
			return roleOrder(bases[i].role) < roleOrder(bases[j].role)
		}
		return bases[i].host+"|"+bases[i].ports < bases[j].host+"|"+bases[j].ports
	})
	counter := map[string]int{}
	for _, b := range bases {
		t := roleType[b.role]
		counter[t]++
		port, sp := parsePorts(b.role, b.ports)
		in := model.Instance{ID: fmt.Sprintf("%s-%d", t, counter[t]), Type: t, Host: b.host, Port: port, StatusPort: sp, AZ: "—", Status: tiupStatus(b.status), Leader: b.leader || (t == "pd" && b.host == leaderHost)}
		addr := fmt.Sprintf("%s:%d", b.host, port)
		in.Version = strings.TrimPrefix(ver[addr], "v")
		if v := ci.Version; in.Version == "" {
			in.Version = strings.TrimPrefix(v, "v")
		}
		in.DataDir, in.DeployDir = b.dataDir, b.deployDir
		in.LogDir, in.LogDirInferred = meta.resolveLogDir(b.deployDir, b.id, addr)
		in.CPU, in.Mem = round1(cpuBy[b.host]), round1(memBy[b.host])
		switch t {
		case "tikv", "tiflash":
			key := b.host
			if t == "tikv" {
				key = addr
			}
			if s, ok := storeByHost[key]; ok {
				in.SID, in.Regions, in.Leaders = s.ID, s.Regions, s.Leaders
				in.CapacityGB, in.UsedGB, in.Disk = round1(s.CapacityGB), round1(s.UsedGB), round1(s.DiskPct())
				if z := s.Labels["zone"]; z != "" {
					in.AZ = z
				}
				in.Labels = s.Labels
				in.Status = storeStatus(s.State, in.Status)
				if s.Version != "" {
					in.Version = s.Version
				}
			} else if in.Status == "up" {
				in.Note = "PD 中未找到对应 Store"
			}
			if t == "tikv" {
				in.WR = round1(wrBy[b.host] / 1e6)
				in.RaftCPU = round1(raftBy[b.host] * 100)
			}
		case "tidb":
			in.QPS, in.Conn = round1(qpsBy[b.host]), int(connBy[b.host])
		}
		// 简单健康判定：资源接近上限视为告警
		if in.Status == "up" {
			var notes []string
			if in.Disk >= 85 {
				notes = append(notes, fmt.Sprintf("磁盘使用率 %.0f%% ≥ 85%%", in.Disk))
			}
			if in.CPU >= 85 {
				notes = append(notes, fmt.Sprintf("CPU %.0f%% ≥ 85%%", in.CPU))
			}
			if in.Mem >= 80 {
				notes = append(notes, fmt.Sprintf("内存 %.0f%% ≥ 80%%", in.Mem))
			}
			if len(notes) > 0 {
				in.Status, in.Note = "warn", strings.Join(notes, "；")
			}
		}
		topo.Instances = append(topo.Instances, in)
	}
	// 3. 没有 zone 标签的非存储实例：沿用同主机上已知的 zone
	zoneOfHost := map[string]string{}
	for _, in := range topo.Instances {
		if in.AZ != "—" {
			zoneOfHost[in.Host] = in.AZ
		}
	}
	for i := range topo.Instances {
		if z := zoneOfHost[topo.Instances[i].Host]; z != "" && topo.Instances[i].AZ == "—" {
			topo.Instances[i].AZ = z
		}
	}
	topo.Proxies = discoverProxies(ci, ver)
	topo.Cluster, topo.Version, topo.User = ci.Name, ci.Version, ci.User
	topo.Dashboard, topo.Grafana = ci.DashboardURL, ci.GrafanaURL
	ep, _, _ := ci.Endpoints()
	topo.Prometheus, topo.Alertmanage = ep["prom"], ep["alert"]
	topo.PDLeader = leaderHost
	// 集群事实：副本数与 location-labels 来自 pd/config，AZ 数来自 TiKV 实例所在的 zone
	if pd := c.pdConfigFlat(ctx); pd != nil {
		topo.Replicas = atoi(pd["replication.max-replicas"])
		for _, l := range strings.Split(pd["replication.location-labels"], ",") {
			if l = strings.TrimSpace(l); l != "" {
				topo.LocationLabels = append(topo.LocationLabels, l)
			}
		}
	}
	topo.AZCount = countAZ(topo.Instances)
	if topo.Cluster == "" {
		topo.Cluster = c.clusterGuess()
	}
	return topo, errs
}

// proxyRole 是 `tiup cluster display` 里 TiProxy 的 Role 列取值。
const proxyRole = "tiproxy"

// discoverProxies 从 display 里取出 TiProxy 实例（不放进 Instances，见 model.Proxy）。
// Ports 列形如 "6000/3080"：SQL 端口 / HTTP API 端口；只有一个端口时 APIPort 为 0。
// 编号按 host + 端口排序，与其他角色一致。
func discoverProxies(ci source.ClusterInfo, ver map[string]string) []model.Proxy {
	type px struct {
		host        string
		sql, api    int
		status, key string
	}
	var ps []px
	for _, in := range ci.Instances {
		if in.Role != proxyRole {
			continue
		}
		sql, api := parsePorts(in.Role, in.Ports)
		ps = append(ps, px{in.Host, sql, api, tiupStatus(in.Status), fmt.Sprintf("%s|%s", in.Host, in.Ports)})
	}
	if len(ps) == 0 {
		return nil
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].key < ps[j].key })
	out := make([]model.Proxy, 0, len(ps))
	for i, p := range ps {
		out = append(out, model.Proxy{ID: fmt.Sprintf("%s-%d", proxyRole, i+1), Type: proxyRole, Host: p.host, SQLPort: p.sql, APIPort: p.api, Status: p.status,
			Version: strings.TrimPrefix(ver[fmt.Sprintf("%s:%d", p.host, p.sql)], "v")})
	}
	return out
}

// countAZ 统计 TiKV 实例所在的不同 zone 个数（副本落在这些故障域上）；没有 TiKV 时统计全部实例。AZ 未知（"—"）不计。
func countAZ(ins []model.Instance) int {
	count := func(only string) int {
		set := map[string]bool{}
		for _, in := range ins {
			if (only == "" || in.Type == only) && in.AZ != "" && in.AZ != "—" {
				set[in.AZ] = true
			}
		}
		return len(set)
	}
	if n := count("tikv"); n > 0 {
		return n
	}
	return count("")
}

func (c *Collector) clusterGuess() string {
	for _, it := range c.Items {
		if it.Name == "tiup/cluster-display" && len(it.Argv) == 4 {
			return it.Argv[3]
		}
	}
	return ""
}

func roleOrder(r string) int {
	for i, x := range []string{"tidb", "pd", "tikv", "tiflash", "cdc"} {
		if x == r {
			return i
		}
	}
	return 99
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
