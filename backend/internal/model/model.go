// @author: kevinkelly
// @license: Apache License 2.0
// @file: model.go
// @time: 2026/10/28

// Package model 定义平台对外提供的数据结构。字段名与前端原型里的数据模型一致（NODES / SQLS / ALERTS / FINDINGS），
// 因此前端在真实数据模式下几乎不需要改动页面代码。
package model

import "time"

// Instance 是集群中的一个实例（TiDB / PD / TiKV / TiFlash / TiCDC）。
// 百分比字段取 0–100；取不到的指标为 0（并在 Note 中说明）。
type Instance struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"` // tidb | pd | tikv | tiflash | ticdc
	Host       string            `json:"host"`
	Port       int               `json:"port"`
	StatusPort int               `json:"statusPort,omitempty"`
	AZ         string            `json:"az"`
	Status     string            `json:"status"` // up | warn | down | offline | joining
	Version    string            `json:"version,omitempty"`
	CPU        float64           `json:"cpu"`
	Mem        float64           `json:"mem"`
	Disk       float64           `json:"disk"`
	Regions    int               `json:"regions,omitempty"`
	Leaders    int               `json:"leaders,omitempty"`
	WR         float64           `json:"wr"`      // 写流量 MB/s
	RD         float64           `json:"rd"`      // 读流量 MB/s
	RaftCPU    float64           `json:"raftCpu"` // Raftstore 线程 CPU（核）折算的百分比
	QPS        float64           `json:"qps,omitempty"`
	Conn       int               `json:"conn,omitempty"`
	Leader     bool              `json:"leader,omitempty"`
	SID        uint64            `json:"sid,omitempty"`
	CapacityGB float64           `json:"capacityGB,omitempty"`
	UsedGB     float64           `json:"usedGB,omitempty"`
	Lag        string            `json:"lag,omitempty"`
	Note       string            `json:"note,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`

	// 实例目录（实例目录页）。DataDir / DeployDir 取自 `tiup cluster display` 的 Data Dir / Deploy Dir 列，
	// 取不到（列缺失，或该组件没有数据目录，如 TiDB）时为空。
	DataDir   string `json:"dataDir,omitempty"`
	DeployDir string `json:"deployDir,omitempty"`
	// LogDir 优先取 meta.yaml 里该实例的 log_dir（相对路径按 tiup 约定拼在 deploy_dir 下）；
	// meta.yaml 没有给出且没有全局 log_dir 时，按 tiup 的默认约定「推断」为 <deploy_dir>/log，此时 LogDirInferred=true。
	// 既没有 log_dir 也没有 deploy_dir（或设置了全局 log_dir 导致无法确定）时为空。
	LogDir         string `json:"logDir,omitempty"`
	LogDirInferred bool   `json:"logDirInferred,omitempty"` // LogDir 是推断值（tiup 默认约定），不是 meta.yaml 里明确写的
}

// HostInfo 是一台主机的规格与归属（主机规格页）。规格来自 node_exporter（经 Prometheus）；取不到的项为 0 / 空，不做猜测。
type HostInfo struct {
	IP        string   `json:"ip"`
	Cores     int      `json:"cores"`              // node_cpu_seconds_total{mode="idle"} 的 CPU 个数（逻辑核）
	MemGB     float64  `json:"memGB"`              // node_memory_MemTotal_bytes / 2^30，保留 1 位小数（内核保留后的可用总量，通常略小于标称容量）
	OS        string   `json:"os"`                 // node_os_info 的 PRETTY_NAME；exporter 较旧没有该指标时为空
	Kernel    string   `json:"kernel"`             // node_uname_info 的 release
	Hostname  string   `json:"hostname,omitempty"` // node_uname_info 的 nodename
	DiskGB    float64  `json:"diskGB"`             // 最大的非内存文件系统容量（近似数据盘），GiB；取不到为 0
	Instances []string `json:"instances"`          // 该主机上的实例 ID
	AZ        string   `json:"az"`                 // 取自实例的 zone 标签；未知为空
	Rack      string   `json:"rack"`               // 取自实例的 rack 标签；未知为空
	Known     bool     `json:"known"`              // 是否取到了任何规格数据（核数 / 内存 / 系统 / 内核 / 磁盘）
}

// Proxy 是 tiup 部署的负载均衡实例（目前只有 TiProxy），从 `tiup cluster display` 的 tiproxy 行解析而来。
// 它刻意不放进 Instances：Instances 驱动扩缩容、配置目录、诊断、实例计数等逻辑，而 TiProxy 不参与这些。
// 状态只来自 display 的 Status 列（沿用 tiupStatus 映射：up | down | offline | joining）；没有资源指标。
type Proxy struct {
	ID      string `json:"id"`   // tiproxy-1 …（按 host + 端口排序编号）
	Type    string `json:"type"` // tiproxy
	Host    string `json:"host"`
	SQLPort int    `json:"sqlPort"`           // 客户端连接用的 SQL 端口（display Ports 的第 1 个，通常 6000）
	APIPort int    `json:"apiPort,omitempty"` // HTTP API / 状态端口（display Ports 的第 2 个，通常 3080）
	Status  string `json:"status"`
	Version string `json:"version,omitempty"` // display 不给出单实例版本，取不到时为空（不用集群版本冒充）
}

// Topology 是集群拓扑。
type Topology struct {
	Cluster     string     `json:"cluster"`
	Version     string     `json:"version"`
	User        string     `json:"user,omitempty"`
	PDLeader    string     `json:"pdLeader,omitempty"`
	Instances   []Instance `json:"instances"`
	Proxies     []Proxy    `json:"proxies,omitempty"` // 自动发现的负载均衡（TiProxy）；没有时字段缺省
	Grafana     string     `json:"grafana,omitempty"`
	Dashboard   string     `json:"dashboard,omitempty"`
	Prometheus  string     `json:"prometheus,omitempty"`
	Alertmanage string     `json:"alertmanager,omitempty"`
	// 集群事实（来自 PD 配置与拓扑），总览页据此展示副本数与 AZ 数，而不是写死。取不到时为零值。
	Replicas       int      `json:"replicas,omitempty"`       // replication.max-replicas
	LocationLabels []string `json:"locationLabels,omitempty"` // replication.location-labels，如 [zone host]
	AZCount        int      `json:"azCount,omitempty"`        // TiKV 实例所在的不同 zone（AZ）个数
}

// Overview 是总览页的关键指标。
type Overview struct {
	QPS           float64 `json:"qps"`
	QPSChangePct  float64 `json:"qpsChangePct"` // 相对窗口起点
	P99ms         float64 `json:"p99ms"`
	P99ChangeMs   float64 `json:"p99ChangeMs"`
	Connections   float64 `json:"connections"`
	StorageUsedGB float64 `json:"storageUsedGB"`
	StorageCapGB  float64 `json:"storageCapGB"`
	Regions       int     `json:"regions"`
	StoresUp      int     `json:"storesUp"`
	StoresTotal   int     `json:"storesTotal"`
	AlertsCrit    int     `json:"alertsCritical"`
	AlertsWarn    int     `json:"alertsWarning"`
}

// Trends 是趋势图数据（等间隔采样）。
type Trends struct {
	Start int64     `json:"start"`
	Step  int       `json:"step"`
	QPS   []float64 `json:"qps"`
	P99ms []float64 `json:"p99ms"`
	Conns []float64 `json:"conns"`
	// StoreWrite 是各 TiKV 的写入流量时间序列（MB/s，与 QPS 同一时间轴），用于「Store × 时间」热力图。
	StoreWrite []StoreSeries `json:"storeWrite,omitempty"`
}

// StoreSeries 是某个实例的一条时间序列。
type StoreSeries struct {
	Inst string    `json:"inst"` // host（不含端口）
	Vals []float64 `json:"vals"`
}

// SlowGroup 是按 Digest 聚合的慢 SQL。
type SlowGroup struct {
	ID     int     `json:"id"`
	Digest string  `json:"digest"`
	DB     string  `json:"db"`
	User   string  `json:"user"`
	SQL    string  `json:"sql"`
	Exec   int     `json:"exec"`
	Avg    float64 `json:"avg"`
	P99    float64 `json:"p99"`
	Total  float64 `json:"total"`
	Keys   float64 `json:"keys"`
	PKeys  float64 `json:"pkeys"`
	Rows   float64 `json:"rows"`
	Mem    string  `json:"mem"`
	Index  string  `json:"index"`
	Tag    string  `json:"tag"`
	Wait   float64 `json:"wait"`
	Tips   []Tip   `json:"tips"`
	Plan   [][]any `json:"plan"`
}

// Tip 是一条优化建议，Cmd 可为空。
type Tip struct {
	T   string `json:"t"`
	Cmd string `json:"cmd,omitempty"`
}

// HotRegion 是热点 Region。
type HotRegion struct {
	ID     uint64  `json:"id"`
	Kind   string  `json:"hot"` // 写热点 | 读热点
	Store  uint64  `json:"store"`
	Table  string  `json:"table,omitempty"`
	Flow   float64 `json:"flowMBps"`
	Leader string  `json:"leader,omitempty"`
	// Range 是 Region 的 Key 范围，人类可读：能解码 TiDB 表键 / 索引键时形如 "table_id=112 row 12345 … row 1012345"，
	// 解码失败则是截断后的十六进制 "start → end"；PD 没有返回该 Region 的 Key 时为空。
	Range string `json:"range,omitempty"`
}

// Alert 是活跃告警。
type Alert struct {
	Lv   string `json:"lv"` // critical | warning | info
	Name string `json:"name"`
	Inst string `json:"inst"`
	Time string `json:"time"`
	Desc string `json:"desc"`
}

// Finding 是一条诊断结果。
type Finding struct {
	ID       string   `json:"id"`
	Sev      string   `json:"sev"`
	Title    string   `json:"title"`
	Scope    string   `json:"scope"`
	Evidence []string `json:"evidence"`
	Cause    string   `json:"cause"`
	Actions  []Tip    `json:"actions"`
	Link     Link     `json:"link"`
	Ref      string   `json:"ref"`
}

// Link 指向前端页面。
type Link struct {
	Page  string            `json:"page"`
	Label string            `json:"label"`
	Args  map[string]string `json:"args,omitempty"`
}

// EnvItem 是环境探测的一项结果。
type EnvItem struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Detected bool   `json:"detected"`
	Cmd      string `json:"cmd"`
}

// Item 是堆叠条里的一段：名称与占比（0–100，同一维度内的占比之和为 100）。
type Item struct {
	Name string  `json:"name"`
	Pct  float64 `json:"pct"`
}

// DBTime 是数据库时间（DB Time）的三个分解维度。取不到的维度为空数组（前端提示「未采集」）。
type DBTime struct {
	ByType  []Item `json:"byType"`  // 按 SQL 类型：Select / Insert / Update …
	ByStage []Item `json:"byStage"` // 按处理阶段：Get Token / Parse / Compile / Execute
	ByExec  []Item `json:"byExec"`  // 执行阶段细分：TSO 等待 / DistSQL / TiKV 各类请求（各项可能有交叠，仅作相对比较）
}

// Forecast 是容量预测。Enough=false 表示样本不足 24 小时，此时不给出增长与到达天数（为 null）。
type Forecast struct {
	UsedGB             float64  `json:"usedGB"`
	CapGB              float64  `json:"capGB"`
	GrowthGBPerDay     float64  `json:"growthGBPerDay"`
	DaysTo80           *float64 `json:"daysTo80"` // 已用达到 80% 容量还需多少天；已超过为 0；不增长则为 null
	DaysTo95           *float64 `json:"daysTo95"`
	Regions            int      `json:"regions"`
	RegionGrowthPerDay float64  `json:"regionGrowthPerDay"`
	Enough             bool     `json:"enough"`
	Basis              string   `json:"basis"` // 预测依据说明（样本数、窗口）或不可用原因
}

// Snapshot 汇总前端一次刷新所需的全部数据。
type Snapshot struct {
	Mode        string      `json:"mode"` // replay | live
	Cluster     string      `json:"cluster"`
	CollectedAt time.Time   `json:"collectedAt"`
	Topology    Topology    `json:"topology"`
	Overview    Overview    `json:"overview"`
	Trends      Trends      `json:"trends"`
	DBTime      DBTime      `json:"dbTime"`
	Slow        []SlowGroup `json:"slow"`
	Hot         []HotRegion `json:"hot"`
	Alerts      []Alert     `json:"alerts"`
	Findings    []Finding   `json:"findings"`
	Env         []EnvItem   `json:"env"`
	Errors      []string    `json:"errors,omitempty"` // 某一类数据采集失败时的说明（不影响其他部分）
}

// CheckItem 是巡检报告中的一项。R 取值 ok | warn | bad | na（未采集 / 不适用）。
type CheckItem struct {
	G string `json:"g"`
	N string `json:"n"`
	R string `json:"r"`
	D string `json:"d"`
	// 以下两项只由主机预检（collect.ParseCheckByHost）填写：L 是检查项的中文名；Fix 是失败 / 警告项的修复建议命令（仅展示，
	// 需要 root 权限在目标主机上执行；映射不到时为空）。
	L   string `json:"l,omitempty"`
	Fix string `json:"fix,omitempty"`
}

// Report 是一次巡检的结果。
type Report struct {
	Time    time.Time   `json:"time"`
	Cluster string      `json:"cluster"`
	Version string      `json:"version"`
	Mode    string      `json:"mode"`
	Items   []CheckItem `json:"items"`
	Ok      int         `json:"ok"`
	Warn    int         `json:"warn"`
	Bad     int         `json:"bad"`
	Na      int         `json:"na"`
}

// KeyViz 是近似的流量热力图：行是按 Key 顺序划分的区间，列是时间采样点。
// 数据来自对 PD Region 的写 / 读流量定期采样，只反映「相对热度」，不是 TiDB Dashboard 的 Key Visualizer。
type KeyViz struct {
	Start   int64       `json:"start"` // 第一列的 Unix 秒
	Step    int         `json:"step"`  // 采样间隔（秒）
	Rows    int         `json:"rows"`  // Key 区间数
	Write   [][]float64 `json:"write"` // [row][col] MB/s
	Read    [][]float64 `json:"read"`
	Samples int         `json:"samples"` // 已采样列数
	Note    string      `json:"note,omitempty"`
}

// TimelineEvent 是事件时间线上的一项。
type TimelineEvent struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // alert | change | maint | audit | diag
	Level  string    `json:"level"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Ref    string    `json:"ref,omitempty"`
}

// ParamRow 是「参数建议」页的一行：当前值取自集群，推荐值来自官方调优建议（规则化）。
type ParamRow struct {
	Scope string `json:"scope"` // tidb | tikv | pd
	Name  string `json:"name"`
	Cur   string `json:"cur"` // 当前值；各实例不一致时以逗号分隔并标注；取不到为 "—"
	Rec   string `json:"rec"`
	Why   string `json:"why"`
	Set   string `json:"set,omitempty"`   // 修改命令（仅展示；实际变更请走「节点管理 → 配置变更」）
	State string `json:"state"`           // ok | warn | na：当前值与推荐是否相符
	Drift bool   `json:"drift,omitempty"` // 各实例取值不一致（配置漂移）
}
