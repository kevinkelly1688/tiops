// @author: kevinkelly
// @license: Apache License 2.0
// @file: catalog.go
// @time: 2026/10/28

package source

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// CatalogParams 是生成默认采集目录所需的参数。
type CatalogParams struct {
	Cluster string        // tiup 集群名
	Home    string        // 中控机 tiup 用户的主目录，用于定位 meta.yaml
	Since   time.Duration // Prometheus 趋势窗口，默认 1 小时
	Now     time.Time
	Regions int // Region 抽样数量，默认 200
	// Remote 表示本机不是该集群的中控机（独立部署）：不采集 tiup 与中控机（ctl/*）条目，
	// 否则显示的会是平台所在机器的信息，而不是集群的。拓扑退化为从 PD / cluster_info 推导。
	Remote bool
}

// ConfigInstanceKeys 是 information_schema.cluster_config 里要采集的「实例级配置项」键：
// 与 exec.Catalog() 中 Sys=false 且 Scope=instance 的键一一对应（source 不能 import exec，
// 由 source_test 里的 TestConfigKeysMatchExecCatalog 断言两者一致，防止目录与采集漂移）。
// 不在此列表里的键永远不会被采集（也就不会采到密码、证书路径等敏感配置）。
var ConfigInstanceKeys = []string{
	"storage.block-cache.capacity", "raftstore.store-pool-size", "raftstore.apply-pool-size",
	"storage.scheduler-worker-pool-size", "readpool.unified.max-thread-count", "rocksdb.max-background-jobs",
	"raftstore.raft-max-inflight-msgs", "server.grpc-concurrency", "log.level",
	"performance.max-procs", "performance.token-limit",
	"profiles.default.max_memory_usage", "profiles.default.max_threads", "logger.level",
	"gc-ttl", "per-table-memory-quota", "log-level",
}

// SystemVariables 是 information_schema.session_variables 里要采集的 TiDB 系统变量：
// exec.Catalog() 中全部 Sys=true 的变量，外加参数建议页使用的 tidb_gc_life_time。
var SystemVariables = []string{
	"tidb_mem_quota_query", "tidb_enable_prepared_plan_cache", "tidb_gc_life_time",
	"tidb_replica_read", "tidb_slow_log_threshold", "tidb_distsql_scan_concurrency",
}

// inList 把名称列表渲染成 SQL 的 IN 列表。名称只含 [a-z0-9._-]，不需要转义。
func inList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "'" + n + "'"
	}
	return "(" + strings.Join(q, ",") + ")"
}

type promQuery struct{ id, purpose, expr string }

var promQueries = []promQuery{
	{"qps", "每秒查询数", `sum(rate(tidb_server_query_total[1m]))`},
	{"p99", "查询 P99 延迟（秒）", `histogram_quantile(0.99, sum(rate(tidb_server_handle_query_duration_seconds_bucket[1m])) by (le))`},
	{"connections", "TiDB 连接数", `sum(tidb_server_connections)`},
	{"tikv-cpu", "各 TiKV 的 CPU 使用", `sum(rate(tikv_thread_cpu_seconds_total[1m])) by (instance)`},
	{"tikv-raftstore-cpu", "各 TiKV 的 Raftstore 线程 CPU", `sum(rate(tikv_thread_cpu_seconds_total{name=~"raftstore_.*"}[1m])) by (instance)`},
	{"tikv-store-used", "各 TiKV 磁盘已用字节", `sum(tikv_store_size_bytes{type="used"}) by (instance)`},
	{"tikv-store-capacity", "各 TiKV 磁盘容量字节", `sum(tikv_store_size_bytes{type="capacity"}) by (instance)`},
	{"tikv-region-count", "各 TiKV 的 Region 数", `sum(tikv_raftstore_region_count{type="region"}) by (instance)`},
	{"node-cpu", "各主机 CPU 使用率（%）", `100 - avg(irate(node_cpu_seconds_total{mode="idle"}[1m])) by (instance) * 100`},
	{"node-mem", "各主机内存使用率（%）", `(1 - sum(node_memory_MemAvailable_bytes) by (instance) / sum(node_memory_MemTotal_bytes) by (instance)) * 100`},
	{"tidb-qps-instance", "各 TiDB 的 QPS", `sum(rate(tidb_server_query_total[1m])) by (instance)`},
	{"tidb-conn-instance", "各 TiDB 的连接数", `sum(tidb_server_connections) by (instance)`},
	// 主机规格（node_exporter，instance 标签为 host:9100；collect.Hosts 按 host 归并）。
	// uname / os-info 的值恒为 1，有用的信息在标签里（release、nodename、PRETTY_NAME）；os-info 较新的 exporter 才有。
	{"node-cores", "各主机 CPU 核数（主机规格）", `count(node_cpu_seconds_total{mode="idle"}) by (instance)`},
	{"node-mem-total", "各主机内存总量字节（主机规格）", `node_memory_MemTotal_bytes`},
	{"node-uname", "各主机内核版本与主机名（主机规格，值在标签里）", `node_uname_info`},
	{"node-os-info", "各主机操作系统名称（主机规格，较新的 node_exporter 才有，值在标签里）", `node_os_info`},
	{"node-disk-size", "各主机最大文件系统容量字节（主机规格，近似数据盘容量）", `max(node_filesystem_size_bytes{fstype!~"tmpfs|overlay|squashfs"}) by (instance)`},
	{"tikv-write-bytes", "各 TiKV 的写入字节/秒", `sum(rate(tikv_engine_flow_bytes{db="kv",type="wal_file_bytes"}[1m])) by (instance)`},
	{"pd-tso-p99", "PD TSO 请求 P99（秒）", `histogram_quantile(0.99, sum(rate(pd_client_request_handle_requests_duration_seconds_bucket{type="tso"}[1m])) by (le))`},

	// DB Time 分解：rate(xxx_sum) 即「每秒累计耗时（秒）」，各维度内部按占比归一。
	{"dbtime-type", "DB Time：按 SQL 类型（不含内部 SQL）", `sum(rate(tidb_server_handle_query_duration_seconds_sum{sql_type!="internal"}[1m])) by (sql_type)`},
	// 注意：tidb_server_get_token_duration_seconds 的单位实际是微秒（指标名里的 seconds 有误导），在 collect 里换算。
	{"dbtime-token", "DB Time：取令牌 Get Token（该指标单位为微秒）", `sum(rate(tidb_server_get_token_duration_seconds_sum[1m]))`},
	{"dbtime-parse", "DB Time：解析 Parse", `sum(rate(tidb_session_parse_duration_seconds_sum{sql_type!="internal"}[1m]))`},
	{"dbtime-compile", "DB Time：编译 Compile", `sum(rate(tidb_session_compile_duration_seconds_sum{sql_type!="internal"}[1m]))`},
	{"dbtime-execute", "DB Time：执行 Execute", `sum(rate(tidb_session_execute_duration_seconds_sum{sql_type!="internal"}[1m]))`},
	{"dbtime-exec-tso", "执行阶段细分：TSO 等待", `sum(rate(pd_client_cmd_handle_cmds_duration_seconds_sum{type="wait"}[1m]))`},
	{"dbtime-exec-distsql", "执行阶段细分：分布式 SQL 请求", `sum(rate(tidb_distsql_handle_query_duration_seconds_sum[1m]))`},
	{"dbtime-exec-kv", "执行阶段细分：TiKV 客户端请求（按请求类型）", `sum(rate(tidb_tikvclient_request_seconds_sum[1m])) by (type)`},

	// PD 与副本健康（官方「日常巡检」「告警规则」）。pd_regions_status 只有 PD leader 上有值，取 max 即可。
	{"pd-miss-peer", "缺副本的 Region 数（PD）", `max(pd_regions_status{type="miss-peer-region-count"})`},
	{"pd-down-peer", "有副本失联（down-peer）的 Region 数（PD）", `max(pd_regions_status{type="down-peer-region-count"})`},
	{"pd-pending-peer", "有副本落后（pending-peer）的 Region 数（PD）", `max(pd_regions_status{type="pending-peer-region-count"})`},
	{"pd-etcd-fsync-p99", "各 PD 的 etcd WAL fsync P99（秒）", `histogram_quantile(0.99, sum(rate(etcd_disk_wal_fsync_duration_seconds_bucket[1m])) by (instance, le))`},
	{"pd-etcd-rtt-p99", "各 PD 的 etcd 节点间往返延迟 P99（秒）", `histogram_quantile(0.99, sum(rate(etcd_network_peer_round_trip_time_seconds_bucket[1m])) by (instance, le))`},
	{"pd-store-slow-score", "PD 给各 Store 打的慢节点分数（按 Store 地址）", `max(pd_scheduler_store_status{type="store_slow_score"}) by (address)`},

	// TiKV 写入链路（官方「TiDB 磁盘 I/O 过高的处理办法」「告警规则」）
	{"tikv-apply-cpu", "各 TiKV 的 Apply 线程 CPU（核）", `sum(rate(tikv_thread_cpu_seconds_total{name=~"apply_.*"}[1m])) by (instance)`},
	{"tikv-write-stall", "各 TiKV 近 10 分钟 write stall 的增量", `max(delta(tikv_engine_write_stall[10m])) by (instance)`},
	{"tikv-append-log-p99", "各 TiKV 的 Raft 日志追加（append log）P99（秒）", `histogram_quantile(0.99, sum(rate(tikv_raftstore_append_log_duration_seconds_bucket[1m])) by (instance, le))`},
	{"tikv-apply-log-p99", "各 TiKV 的 Raft 日志应用（apply log）P99（秒）", `histogram_quantile(0.99, sum(rate(tikv_raftstore_apply_log_duration_seconds_bucket[1m])) by (instance, le))`},
	{"tikv-latch-wait-p99", "各 TiKV 的 scheduler latch 等待 P99（秒）", `histogram_quantile(0.99, sum(rate(tikv_scheduler_latch_wait_duration_seconds_bucket[1m])) by (instance, le))`},

	// 主机层（node_exporter 告警规则）。文件系统类排除虚拟文件系统；磁盘类排除 loop / ram / 光驱，且只在有一定 IO 量时才算延迟。
	{"node-inode-used", "各主机最高的 inode 使用率（%）", `max(100 * (1 - node_filesystem_files_free{fstype!~"tmpfs|overlay|squashfs|rootfs|ramfs|devtmpfs|iso9660"} / (node_filesystem_files{fstype!~"tmpfs|overlay|squashfs|rootfs|ramfs|devtmpfs|iso9660"} > 0))) by (instance)`},
	{"node-fs-readonly", "各挂载点是否只读（1 = 只读）", `max(node_filesystem_readonly{fstype!~"rootfs|selinuxfs|autofs|rpc_pipefs|tmpfs|udev|squashfs|iso9660|overlay|ramfs|devtmpfs"}) by (instance, mountpoint)`},
	{"node-disk-read-lat", "各主机最高的磁盘读延迟（秒，读 IOPS > 5 时）", `max(rate(node_disk_read_time_seconds_total{device!~"loop.*|ram.*|sr.*|fd.*"}[5m]) / (rate(node_disk_reads_completed_total{device!~"loop.*|ram.*|sr.*|fd.*"}[5m]) > 5)) by (instance)`},
	{"node-disk-write-lat", "各主机最高的磁盘写延迟（秒，写 IOPS > 5 时）", `max(rate(node_disk_write_time_seconds_total{device!~"loop.*|ram.*|sr.*|fd.*"}[5m]) / (rate(node_disk_writes_completed_total{device!~"loop.*|ram.*|sr.*|fd.*"}[5m]) > 5)) by (instance)`},
	{"node-load-per-core", "各主机 5 分钟负载 / CPU 核数", `node_load5 / on(instance) count(node_cpu_seconds_total{mode="idle"}) by (instance)`},
	{"node-tcp-estab", "各主机 ESTABLISHED 状态的 TCP 连接数", `node_netstat_Tcp_CurrEstab`},
}

// curCond 是 SQL 里「属于当前窗口（最近 1 小时）」的条件；扩展采集取最近 2 小时，其中更早的那 1 小时是「上一窗口」。
const curCond = "Time > NOW() - INTERVAL 1 HOUR"

// slowExtQuery 按 (Digest, Plan_digest) 聚合最近 2 小时的慢日志：用于执行计划突变、耗时跳变、统计信息缺失、
// 锁冲突 / TiKV 繁忙 / Region 调度（Backoff 类型）、MVCC 旧版本 / tombstone 等判断。
// 不取 Query 文本（敏感信息不进入这个条目）；不同 TiDB 版本都有的列才放进来，否则整个条目会失败。
func slowExtQuery() string {
	cnt := func(cond string) string { return "SUM(IF(" + curCond + " AND " + cond + ", 1, 0))" }
	mx := func(col string) string { return "MAX(IF(" + curCond + ", " + col + ", 0))" }
	return "SELECT Digest, Plan_digest, COUNT(*) AS cnt, SUM(IF(" + curCond + ", 1, 0)) AS cur_cnt, " +
		"SUM(IF(" + curCond + ", Query_time, 0)) AS cur_sum, SUM(IF(" + curCond + ", 0, Query_time)) AS prev_sum, " +
		cnt("Stats LIKE '%pseudo%'") + " AS pseudo_cnt, " +
		cnt("Backoff_types LIKE '%txnLock%'") + " AS lock_cnt, " +
		cnt("Backoff_types LIKE '%regionMiss%'") + " AS region_miss_cnt, " +
		cnt("Backoff_types LIKE '%tikvServerBusy%'") + " AS busy_cnt, " +
		cnt("Backoff_types LIKE '%pdRPC%'") + " AS pd_cnt, " +
		mx("Resolve_lock_time") + " AS max_resolve_lock, " + mx("Rocksdb_delete_skipped_count") + " AS max_del_skipped, " +
		mx("Exec_retry_count") + " AS max_retry " +
		"FROM information_schema.cluster_slow_query WHERE Time > NOW() - INTERVAL 2 HOUR AND Digest != '' " +
		"GROUP BY Digest, Plan_digest ORDER BY SUM(Query_time) DESC LIMIT 1000"
}

// 容量预测的采集范围：固定 7 天、每小时一个点（与 CatalogParams.Since 无关）。
const (
	forecastWindow = 7 * 24 * time.Hour
	forecastStep   = 3600
)

var forecastQueries = []promQuery{
	{"forecast-used", "容量预测：全部 TiKV 已用字节（7 天，每小时）", `sum(tikv_store_size_bytes{type="used"})`},
	{"forecast-capacity", "容量预测：全部 TiKV 磁盘容量字节（7 天，每小时）", `sum(tikv_store_size_bytes{type="capacity"})`},
	{"forecast-regions", "容量预测：Region 总数（各 TiKV 的 Leader 数之和，7 天，每小时）", `sum(tikv_raftstore_region_count{type="leader"})`},
}

// DefaultCatalog 生成默认采集目录。每一项都能通过 ValidateItem；这里就是「会读哪些数据」的完整清单。
func DefaultCatalog(p CatalogParams) []Item {
	if p.Since <= 0 {
		p.Since = time.Hour
	}
	if p.Regions <= 0 {
		p.Regions = 200
	}
	if p.Now.IsZero() {
		p.Now = time.Now()
	}
	var items []Item
	http := func(name, ep, pth, purpose string, optional bool) {
		items = append(items, Item{Name: name, Kind: KindHTTP, Endpoint: ep, Path: pth, Purpose: purpose, Ext: ".json", Optional: optional})
	}
	cmd := func(name, purpose string, optional bool, argv ...string) {
		items = append(items, Item{Name: name, Kind: KindCmd, Argv: argv, Purpose: purpose, Ext: ".txt", Optional: optional})
	}
	sql := func(name, purpose, q string, optional bool, cols ...string) {
		items = append(items, Item{Name: name, Kind: KindSQL, Query: q, Purpose: purpose, Ext: ".tsv", Optional: optional, SQLCols: cols})
	}

	// 中控机与 tiup（Remote：本机不是中控机，整段跳过）
	if !p.Remote {
		cmd("ctl/hostname", "中控机主机名与地址", true, "hostname", "-I")
		items = append(items, Item{Name: "ctl/os-release", Kind: KindFile, File: "/etc/os-release", Purpose: "中控机操作系统", Ext: ".txt"})
		cmd("ctl/kernel", "内核版本", false, "uname", "-r")
		cmd("ctl/nproc", "CPU 核数", false, "nproc")
		cmd("ctl/free", "内存", false, "free", "-m")
		cmd("ctl/timedatectl", "时区与时间同步", true, "timedatectl")
		cmd("tiup/version", "tiup 版本", false, "tiup", "--version")
		cmd("tiup/cluster-version", "tiup cluster 组件版本", false, "tiup", "cluster", "--version")
		cmd("tiup/cluster-list", "中控机管理的集群列表", false, "tiup", "cluster", "list")
	}
	if p.Cluster != "" && !p.Remote {
		cmd("tiup/cluster-display", "集群拓扑与状态", false, "tiup", "cluster", "display", p.Cluster)
		if p.Home != "" {
			items = append(items, Item{Name: "tiup/meta", Kind: KindFile, Purpose: "tiup 拓扑元数据（用于地址探测）", Ext: ".yaml", Optional: true,
				File: path.Join(p.Home, ".tiup/storage/cluster/clusters", p.Cluster, "meta.yaml")})
		}
	}

	// PD
	http("pd/members", "pd", "/pd/api/v1/members", "PD 成员与 Leader", false)
	http("pd/stores", "pd", "/pd/api/v1/stores", "TiKV / TiFlash Store 列表与容量", false)
	http("pd/config", "pd", "/pd/api/v1/config", "PD 配置（调度、副本）", false)
	http("pd/health", "pd", "/pd/api/v1/health", "PD 健康", false)
	http("pd/cluster", "pd", "/pd/api/v1/cluster", "集群基本信息", false)
	http("pd/version", "pd", "/pd/api/v1/version", "PD 版本", true)
	http("pd/stats-region", "pd", "/pd/api/v1/stats/region", "Region 统计", true)
	http("pd/regions-sample", "pd", fmt.Sprintf("/pd/api/v1/regions?limit=%d", p.Regions), "Region 抽样", true)
	http("pd/hot-write", "pd", "/pd/api/v1/hotspot/regions/write", "写热点 Region", true)
	http("pd/hot-read", "pd", "/pd/api/v1/hotspot/regions/read", "读热点 Region", true)
	http("pd/regions-writeflow", "pd", "/pd/api/v1/regions/writeflow?limit=50", "写流量 Top 50 Region（含起止 Key，用于解码热点范围）", true)
	http("pd/regions-readflow", "pd", "/pd/api/v1/regions/readflow?limit=50", "读流量 Top 50 Region（含起止 Key，用于解码热点范围）", true)

	// TiDB 状态端口
	http("tidb/status", "tidb", "/status", "TiDB 实例状态与版本", false)
	http("tidb/info-all", "tidb", "/info/all", "TiDB 实例注册信息", true)

	// 告警
	http("alert/alerts", "alert", "/api/v2/alerts", "Alertmanager 活跃告警", true)

	// Prometheus 趋势
	start, end := p.Now.Add(-p.Since).Unix(), p.Now.Unix()
	for _, q := range promQueries {
		http("prom/"+q.id, "prom", fmt.Sprintf("/api/v1/query_range?query=%s&start=%d&end=%d&step=60", url.QueryEscape(q.expr), start, end), q.purpose, true)
	}
	fStart, fEnd := p.Now.Add(-forecastWindow).Unix(), p.Now.Unix()
	for _, q := range forecastQueries {
		http("prom/"+q.id, "prom", fmt.Sprintf("/api/v1/query_range?query=%s&start=%d&end=%d&step=%d", url.QueryEscape(q.expr), fStart, fEnd, forecastStep), q.purpose, true)
	}

	// 只读 SQL
	sql("sql/version", "TiDB 版本（交叉验证 tiup 的版本）", "SELECT tidb_version()", true)
	sql("sql/cluster-info", "集群各组件实例信息", "SELECT * FROM information_schema.cluster_info LIMIT 500", true)
	sql("sql/slow-query", "最近 1 小时最慢的 200 条查询",
		"SELECT Time, Digest, Plan_digest, DB, User, Query_time, Parse_time, Compile_time, Process_time, Wait_time, Total_keys, Process_keys, Result_rows, Mem_max, Index_names, SUBSTRING(Query, 1, 2000) AS Query "+
			"FROM information_schema.cluster_slow_query WHERE Time > NOW() - INTERVAL 1 HOUR ORDER BY Query_time DESC LIMIT 200", true, "Query")
	sql("sql/slow-ext", "最近 2 小时慢日志按 SQL 与执行计划聚合（执行计划突变、耗时跳变、统计信息缺失、锁冲突等判断；不含 SQL 文本）", slowExtQuery(), true)
	sql("sql/slow-plan", "慢查询的执行计划（tidb_decode_plan 解码，仅字符串字面量脱敏）",
		"SELECT Digest, tidb_decode_plan(Plan) AS Plan FROM information_schema.cluster_slow_query "+
			"WHERE Time > NOW() - INTERVAL 1 HOUR AND Plan != '' ORDER BY Query_time DESC LIMIT 100", true, "str:Plan")
	sql("sql/statements-summary", "累计耗时 Top 100 的 SQL 摘要",
		"SELECT DIGEST, SCHEMA_NAME, EXEC_COUNT, AVG_LATENCY, MAX_LATENCY, SUM_LATENCY, AVG_MEM, SUBSTRING(DIGEST_TEXT, 1, 2000) AS DIGEST_TEXT "+
			"FROM information_schema.cluster_statements_summary ORDER BY SUM_LATENCY DESC LIMIT 100", true, "DIGEST_TEXT")
	sql("sql/variables", "关键系统变量的当前值（参数建议页）",
		"SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.session_variables WHERE VARIABLE_NAME IN "+
			inList(SystemVariables)+" LIMIT 100", true)
	sql("sql/cluster-config", "配置目录中全部实例级配置项在各实例上的当前值（配置变更页、参数建议与配置漂移检测）",
		"SELECT TYPE, INSTANCE, `KEY`, VALUE FROM information_schema.cluster_config WHERE `KEY` IN "+
			inList(ConfigInstanceKeys)+" LIMIT 1000", true)
	sql("sql/hot-regions", "热点 Region", "SELECT * FROM information_schema.tidb_hot_regions LIMIT 200", true)
	return items
}

// FilterItems 按名称前缀过滤：only 非空时只保留命中的；skip 中命中的剔除。
func FilterItems(items []Item, only, skip []string) []Item {
	match := func(name string, prefixes []string) bool {
		for _, p := range prefixes {
			if p != "" && strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []Item
	for _, it := range items {
		if len(only) > 0 && !match(it.Name, only) {
			continue
		}
		if match(it.Name, skip) {
			continue
		}
		out = append(out, it)
	}
	return out
}
