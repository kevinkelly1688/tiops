// @author: kevinkelly
// @license: Apache License 2.0
// @file: profile.go
// @time: 2026/10/28

package fakecluster

import "fmt"

// HostSpec 是一类主机的规格（node_exporter 指标的来源）。
type HostSpec struct{ Cores, MemGiB, DiskGiB int }

// AlertSpec 描述一条活跃告警，实例由角色与序号指向模拟集群里的节点。
type AlertSpec struct {
	Name string
	Role string // tikv | tidb
	Idx  int
	Sev  string
	Desc string
}

// OSInfo 是主机的操作系统信息（node_os_info / node_uname_info / /etc/os-release）。
type OSInfo struct {
	ID, Name, Pretty, VersionID, Kernel string
}

// Profile 决定模拟集群的外观：集群名、版本、网段、各角色节点数、规模与负载。
// DefaultProfile 复现原型页面里的示例集群（prod-tidb-01，3 TiDB / 3 PD / 6 TiKV / 2 TiFlash / 2 TiCDC）；
// 其余预设用于多集群演示（预发 / 开发），主机、集群名、版本、负载都与默认明显不同。
type Profile struct {
	Name    string
	Version string // tiup 风格，带 v，如 v8.5.2
	Net     string // 网段前两段，如 10.0；节点主机为 Net.1.x（TiDB / PD / 监控）、Net.2.x（TiKV）、Net.3.x（TiFlash）、Net.4.x（TiCDC）

	TiDB, PD, TiKV, TiFlash, CDC int

	// TiProxy 是 tiup 部署的 TiProxy 个数（默认 0：display 输出与未引入该字段前逐字节一致）。
	// 主机为 Net.1.41 起；只出现在 `tiup cluster display` 里（SQL 端口 6000 / API 端口 3080）。
	TiProxy int

	Load float64 // 流量 / 连接数 / DB Time 的倍率

	// TiKV 逐节点数据（长度应与 TiKV 一致）。
	Zones       []string
	Regions     []int
	Leaders     []int
	UsedGiB     []int
	FlowMB      []float64
	CapTiB      float64
	TotalRegion int // 集群 Region 总数（stats/region）

	StoreUsedBase float64 // 各 TiKV「已用空间」序列的基准值（字节）
	RegionBase    float64 // 各 TiKV Region 数序列的基准值

	TiDBQPS   []float64
	TiDBConns []float64
	QPSTotal  float64
	ConnTotal float64

	HotTiKV   int // CPU / Raftstore 偏高的 TiKV 序号，-1 = 没有
	HotTiDB   int // 内存偏高的 TiDB 序号，-1 = 没有
	DriftTiKV int // 配置漂移（store-pool-size 与其余不同）的 TiKV 序号，-1 = 没有
	Replicas  int

	GrowGiBPerDay float64 // 容量预测：已用每天增长
	GrowRegionDay float64

	SpecTiDB, SpecPD, SpecMon, SpecTiKV, SpecFlash, SpecCDC HostSpec

	OS     OSInfo
	Alerts []AlertSpec

	SlowLimit int // >0：只返回前 N 条慢查询 / 语句摘要
}

// DefaultProfile 是原有示例集群。
func DefaultProfile() Profile {
	return Profile{
		Name: "prod-tidb-01", Version: "v8.5.2", Net: "10.0",
		TiDB: 3, PD: 3, TiKV: 6, TiFlash: 2, CDC: 2,
		Load:    1,
		Zones:   []string{"az1", "az1", "az2", "az2", "az3", "az3"},
		Regions: []int{16120, 16090, 16130, 16240, 16100, 16110},
		Leaders: []int{5380, 5410, 5350, 5390, 5330, 5360},
		UsedGiB: []int{2230, 2240, 2150, 3120, 2080, 2190},
		FlowMB:  []float64{38, 214, 41, 35, 37, 36},
		CapTiB:  3.5, TotalRegion: 48210, StoreUsedBase: 2.3e12, RegionBase: 16100,
		TiDBQPS: []float64{3210, 3340, 3980}, TiDBConns: []float64{212, 231, 288},
		QPSTotal: 10530, ConnTotal: 731,
		HotTiKV: 1, HotTiDB: 2, DriftTiKV: 2, Replicas: 3,
		GrowGiBPerDay: 120, GrowRegionDay: 150,
		SpecTiDB: HostSpec{16, 32, 500}, SpecPD: HostSpec{8, 16, 200}, SpecMon: HostSpec{8, 16, 500},
		SpecTiKV: HostSpec{16, 64, 3584}, SpecFlash: HostSpec{32, 128, 7168}, SpecCDC: HostSpec{8, 16, 500},
		OS: OSInfo{ID: "rocky", Name: "Rocky Linux", Pretty: "Rocky Linux 9.3 (Blue Onyx)", VersionID: "9.3", Kernel: "5.14.0-362.8.1.el9_3.x86_64"},
		Alerts: []AlertSpec{
			{"TiKV_disk_used_more_than_80%", "tikv", 3, "critical", "磁盘使用率 86%"},
			{"TiKV_raftstore_thread_cpu_seconds_total", "tikv", 1, "critical", "Raftstore 线程 CPU 超过阈值"},
			{"TiDB_memory_high", "tidb", 2, "warning", "内存使用率 83%"},
		},
	}
}

// StagingProfile 是预发演示集群：stg-tidb-01，v8.1.5，2 TiDB / 3 PD / 3 TiKV / 1 TiFlash / 2 TiProxy，网段 10.20，负载约为默认的四分之一。
func StagingProfile() Profile {
	return Profile{
		Name: "stg-tidb-01", Version: "v8.1.5", Net: "10.20",
		TiDB: 2, PD: 3, TiKV: 3, TiFlash: 1, CDC: 0, TiProxy: 2,
		Load:    0.25,
		Zones:   []string{"az1", "az2", "az3"},
		Regions: []int{4210, 4180, 4235},
		Leaders: []int{1410, 1395, 1405},
		UsedGiB: []int{520, 505, 540},
		FlowMB:  []float64{9, 11, 8},
		CapTiB:  1.8, TotalRegion: 12625, StoreUsedBase: 5.4e11, RegionBase: 4200,
		TiDBQPS: []float64{820, 790}, TiDBConns: []float64{58, 61},
		QPSTotal: 1610, ConnTotal: 119,
		HotTiKV: -1, HotTiDB: -1, DriftTiKV: -1, Replicas: 3,
		GrowGiBPerDay: 18, GrowRegionDay: 40,
		SpecTiDB: HostSpec{8, 16, 200}, SpecPD: HostSpec{4, 8, 100}, SpecMon: HostSpec{4, 8, 200},
		SpecTiKV: HostSpec{8, 32, 1800}, SpecFlash: HostSpec{16, 64, 3584}, SpecCDC: HostSpec{4, 8, 200},
		OS: OSInfo{ID: "ubuntu", Name: "Ubuntu", Pretty: "Ubuntu 22.04.4 LTS", VersionID: "22.04", Kernel: "5.15.0-105-generic"},
		Alerts: []AlertSpec{
			{"TiDB_server_is_down", "tidb", 1, "warning", "状态端口探测失败（预发环境滚动重启中）"},
		},
		SlowLimit: 4,
	}
}

// DevProfile 是开发 / 测试演示集群：dev-tidb-01，v8.1.2，1 TiDB / 1 PD / 3 TiKV，网段 10.30，几乎没有负载、没有告警。
func DevProfile() Profile {
	return Profile{
		Name: "dev-tidb-01", Version: "v8.1.2", Net: "10.30",
		TiDB: 1, PD: 1, TiKV: 3, TiFlash: 0, CDC: 0,
		Load:    0.05,
		Zones:   []string{"az1", "az1", "az1"},
		Regions: []int{820, 815, 830},
		Leaders: []int{275, 270, 280},
		UsedGiB: []int{96, 92, 101},
		FlowMB:  []float64{2, 2, 3},
		CapTiB:  0.5, TotalRegion: 2465, StoreUsedBase: 9.5e10, RegionBase: 820,
		TiDBQPS: []float64{96}, TiDBConns: []float64{14},
		QPSTotal: 96, ConnTotal: 14,
		HotTiKV: -1, HotTiDB: -1, DriftTiKV: -1, Replicas: 3,
		GrowGiBPerDay: 3, GrowRegionDay: 6,
		SpecTiDB: HostSpec{4, 8, 100}, SpecPD: HostSpec{2, 4, 50}, SpecMon: HostSpec{2, 4, 100},
		SpecTiKV: HostSpec{4, 16, 500}, SpecFlash: HostSpec{8, 32, 1000}, SpecCDC: HostSpec{2, 4, 100},
		OS:        OSInfo{ID: "rocky", Name: "Rocky Linux", Pretty: "Rocky Linux 8.9 (Green Obsidian)", VersionID: "8.9", Kernel: "4.18.0-513.5.1.el8_9.x86_64"},
		SlowLimit: 2,
	}
}

// ProfileByName 按名字取预设：demo（默认）| stg | dev。
func ProfileByName(n string) (Profile, error) {
	switch n {
	case "", "demo", "prod":
		return DefaultProfile(), nil
	case "stg", "staging":
		return StagingProfile(), nil
	case "dev", "test":
		return DevProfile(), nil
	}
	return Profile{}, fmt.Errorf("未知的模拟集群预设 %q（可选 demo | stg | dev）", n)
}

// ---- 派生：主机与地址 ----

func (p Profile) host(sub, i int) string { return fmt.Sprintf("%s.%d.%d", p.Net, sub, i) }

func (p Profile) tidbHosts() []string    { return p.hosts(1, 11, p.TiDB) }
func (p Profile) pdHosts() []string      { return p.hosts(1, 21, p.PD) }
func (p Profile) tikvHosts() []string    { return p.hosts(2, 11, p.TiKV) }
func (p Profile) tiflashHosts() []string { return p.hosts(3, 11, p.TiFlash) }
func (p Profile) cdcHosts() []string     { return p.hosts(4, 11, p.CDC) }
func (p Profile) tiproxyHosts() []string { return p.hosts(1, 41, p.TiProxy) }
func (p Profile) monitor() string        { return p.host(1, 31) }

func (p Profile) hosts(sub, start, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, p.host(sub, start+i))
	}
	return out
}

// short 是不带 v 的版本号，如 8.5.2。
func (p Profile) short() string {
	if len(p.Version) > 0 && p.Version[0] == 'v' {
		return p.Version[1:]
	}
	return p.Version
}

func (p Profile) tidbVersion() string { return "8.0.11-TiDB-" + p.Version }

func (p Profile) dashed() string {
	b := []byte(p.Net)
	for i, c := range b {
		if c == '.' {
			b[i] = '-'
		}
	}
	return string(b)
}

func (p Profile) alertInstance(a AlertSpec) string {
	switch a.Role {
	case "tidb":
		if h := p.tidbHosts(); a.Idx >= 0 && a.Idx < len(h) {
			return h[a.Idx] + ":4000"
		}
	default:
		if h := p.tikvHosts(); a.Idx >= 0 && a.Idx < len(h) {
			return h[a.Idx] + ":20160"
		}
	}
	return ""
}

// SQLHost 是第一个 TiDB 节点的地址（演示导出时作为只读 SQL 的连接地址）。
func (p Profile) SQLHost() string { return p.host(1, 11) }
