// @author: kevinkelly
// @license: Apache License 2.0
// @file: configcat.go
// @time: 2026/10/28

package exec

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 配置项目录（与前端 catalog.js 的 CONFIG_CATALOG 一一对应）。
//
// 它同时是「允许改什么」的白名单与「值是否注入安全」的唯一把关点：
// 所有会被拼进 SQL / pd-ctl 参数 / 拓扑 YAML 的配置值，都必须先经过 Normalize。

// ConfigType 是配置项的值类型。
type ConfigType string

const (
	TypeInt      ConfigType = "int"
	TypeSize     ConfigType = "size"
	TypeDuration ConfigType = "duration"
	TypeBool     ConfigType = "bool"
	TypeEnum     ConfigType = "enum"
)

// ConfigItem 是一个可变更的配置项。
type ConfigItem struct {
	Role    string     // tidb | tikv | pd | tiflash | ticdc
	Key     string     // 完整配置键，如 storage.block-cache.capacity；系统变量为变量名
	Label   string     // 展示名
	Type    ConfigType // int | size | duration | bool | enum
	Min     int64      // int：下限；size：字节下限（0=不限）；duration：秒下限
	Max     int64      // int：上限（0=按 int32 上限）；size：字节上限（0=不限）；duration：秒上限（0=不限）
	Opts    []string   // enum 可选值
	Default string     // 默认值（仅展示）
	Online  bool       // 可在线生效（SET CONFIG / SET GLOBAL / pd-ctl）
	Scope   string     // instance | cluster
	Sys     bool       // 是 TiDB 系统变量（SET GLOBAL），而不是配置文件项
	Danger  bool       // 高危参数
	Desc    string
}

var configCatalog = map[string][]ConfigItem{
	"tikv": {
		{Key: "storage.block-cache.capacity", Label: "Block Cache 容量", Type: TypeSize, Default: "16GB", Online: true, Scope: "instance", Desc: "RocksDB Block Cache（各 CF 共享）。过大是 TiKV OOM 的常见原因，建议 ≤ 内存 45%。"},
		{Key: "raftstore.store-pool-size", Label: "Raftstore 线程数", Type: TypeInt, Default: "2", Min: 1, Max: 16, Online: true, Scope: "instance", Desc: "处理 Raft 消息与 propose 的线程数。propose wait duration 偏高时调大，常用范围 1–5。"},
		{Key: "raftstore.apply-pool-size", Label: "Apply 线程数", Type: TypeInt, Default: "2", Min: 1, Max: 16, Online: true, Scope: "instance", Desc: "把已提交日志应用到 RocksDB 的线程数。apply log duration 偏高时调大。"},
		{Key: "storage.scheduler-worker-pool-size", Label: "Scheduler 线程数", Type: TypeInt, Default: "4", Min: 1, Max: 32, Online: true, Scope: "instance", Desc: "事务调度线程，Scheduler CPU 繁忙时调大；不应超过 CPU 核数。"},
		{Key: "readpool.unified.max-thread-count", Label: "统一读线程池上限", Type: TypeInt, Default: "16", Min: 4, Max: 64, Online: true, Scope: "instance", Desc: "点查与 Coprocessor 共用的读线程池上限，Coprocessor 排队时可调大。"},
		{Key: "rocksdb.max-background-jobs", Label: "RocksDB 后台任务数", Type: TypeInt, Default: "8", Min: 2, Max: 32, Online: true, Scope: "instance", Desc: "Flush / Compaction 并发数；write stall 频繁时可适度调大，注意占用 CPU 与 I/O。"},
		{Key: "raftstore.raft-max-inflight-msgs", Label: "Raft 在途消息上限", Type: TypeInt, Default: "256", Min: 64, Max: 4096, Online: false, Scope: "instance", Desc: "Raft commit log duration 偏高时可调到 1024；需重启生效。"},
		{Key: "server.grpc-concurrency", Label: "gRPC 线程数", Type: TypeInt, Default: "5", Min: 1, Max: 32, Online: false, Scope: "instance", Desc: "gRPC 服务线程数；gRPC CPU 打满时调大。需重启生效。"},
		{Key: "log.level", Label: "日志级别", Type: TypeEnum, Opts: []string{"debug", "info", "warn", "error"}, Default: "info", Online: true, Scope: "instance", Desc: "debug 日志量很大，只建议短时排障使用。"},
	},
	"tidb": {
		{Key: "tidb_mem_quota_query", Label: "单条 SQL 内存配额", Type: TypeSize, Default: "1GB", Online: true, Scope: "cluster", Sys: true, Desc: "超过后按 tidb_mem_oom_action 处理（日志 / 取消）。避免个别 SQL 拖垮节点。"},
		{Key: "tidb_enable_prepared_plan_cache", Label: "Prepared 执行计划缓存", Type: TypeBool, Default: "ON", Online: true, Scope: "cluster", Sys: true, Desc: "开启可明显降低 Compile 耗时。"},
		{Key: "tidb_slow_log_threshold", Label: "慢日志阈值 (ms)", Type: TypeInt, Default: "300", Min: 50, Max: 10000, Online: true, Scope: "cluster", Sys: true, Desc: "超过该耗时的 SQL 记入慢日志；OLTP 常用 300ms。"},
		{Key: "tidb_distsql_scan_concurrency", Label: "扫描并发度", Type: TypeInt, Default: "15", Min: 1, Max: 256, Online: true, Scope: "cluster", Sys: true, Desc: "单条 SQL 向 TiKV 发起扫描的并发，调大会加重 TiKV 负载。"},
		{Key: "tidb_replica_read", Label: "读副本策略", Type: TypeEnum, Opts: []string{"leader", "follower", "closest-replicas", "leader-and-follower"}, Default: "leader", Online: true, Scope: "cluster", Sys: true, Desc: "跨 AZ 部署可用 closest-replicas 降低读延迟。"},
		{Key: "performance.max-procs", Label: "max-procs（0=全部 CPU）", Type: TypeInt, Default: "0", Min: 0, Max: 128, Online: false, Scope: "instance", Desc: "TiDB 使用的 CPU 核数上限，混部时可限制。需重启生效。"},
		{Key: "performance.token-limit", Label: "并发 token 上限", Type: TypeInt, Default: "1000", Min: 100, Max: 20000, Online: false, Scope: "instance", Desc: "同时执行的会话数上限，Get Token 耗时高时调大。需重启生效。"},
		{Key: "log.level", Label: "日志级别", Type: TypeEnum, Opts: []string{"debug", "info", "warn", "error"}, Default: "info", Online: false, Scope: "instance", Desc: "需重启生效。"},
	},
	"pd": {
		{Key: "schedule.max-store-down-time", Label: "Store 判定 Down 的时间", Type: TypeDuration, Default: "30m", Online: true, Scope: "cluster", Desc: "TiKV 失联超过该时间，PD 判定其 Down 并开始补副本。计划内维护务必关注该值。"},
		{Key: "schedule.region-schedule-limit", Label: "Region 调度并发上限", Type: TypeInt, Default: "2048", Min: 1, Max: 8192, Online: true, Scope: "cluster", Desc: "同时进行的 Region 调度数。调大迁移更快、对线上影响更大。"},
		{Key: "schedule.leader-schedule-limit", Label: "Leader 调度并发上限", Type: TypeInt, Default: "4", Min: 1, Max: 512, Online: true, Scope: "cluster", Desc: "同时进行的 Leader 调度数。"},
		{Key: "replication.max-replicas", Label: "副本数 max-replicas", Type: TypeInt, Default: "3", Min: 1, Max: 7, Online: true, Scope: "cluster", Danger: true, Desc: "每个 Region 的副本数。修改会触发全量副本补齐/删除，影响极大。"},
		{Key: "log.level", Label: "日志级别", Type: TypeEnum, Opts: []string{"debug", "info", "warn", "error"}, Default: "info", Online: false, Scope: "instance", Desc: "需重启生效。"},
	},
	"tiflash": {
		{Key: "profiles.default.max_memory_usage", Label: "单查询内存上限", Type: TypeSize, Default: "0GB", Online: false, Scope: "instance", Desc: "0 表示不限制。分析型查询占用过大时设置上限。需重启生效。"},
		{Key: "profiles.default.max_threads", Label: "查询线程数", Type: TypeInt, Default: "0", Min: 0, Max: 128, Online: false, Scope: "instance", Desc: "0 表示按 CPU 核数自动决定。"},
		{Key: "logger.level", Label: "日志级别", Type: TypeEnum, Opts: []string{"debug", "info", "warning", "error"}, Default: "info", Online: false, Scope: "instance", Desc: "需重启生效。"},
	},
	"ticdc": {
		{Key: "gc-ttl", Label: "上游 GC 保留时长 (秒)", Type: TypeInt, Default: "86400", Min: 600, Max: 604800, Online: false, Scope: "instance", Desc: "changefeed 暂停期间上游保留数据的时长；过大占用 TiKV 空间。"},
		{Key: "per-table-memory-quota", Label: "单表内存配额 (字节)", Type: TypeInt, Default: "10485760", Min: 1048576, Max: 1073741824, Online: false, Scope: "instance", Desc: "同步大表时可适当调大。"},
		{Key: "log-level", Label: "日志级别", Type: TypeEnum, Opts: []string{"debug", "info", "warn", "error"}, Default: "info", Online: false, Scope: "instance", Desc: "需重启生效。"},
	},
}

func init() {
	// 回填 Role，保证 ConfigItem 自描述。
	for role, items := range configCatalog {
		for i := range items {
			items[i].Role = role
		}
	}
}

// canonicalRole 把 cdc 归一为 ticdc（目录与拓扑里用 ticdc，tiup -R 用 cdc）。
func canonicalRole(role string) string {
	if role == "cdc" {
		return "ticdc"
	}
	return role
}

// Lookup 按角色与键查找配置项。role 接受 cdc 作为 ticdc 的别名。返回的是副本，修改不影响目录。
func Lookup(role, key string) (ConfigItem, bool) {
	for _, it := range configCatalog[canonicalRole(role)] {
		if it.Key == key {
			it.Opts = append([]string(nil), it.Opts...)
			return it, true
		}
	}
	return ConfigItem{}, false
}

// Catalog 返回整个配置项目录的副本（角色 → 配置项）。
func Catalog() map[string][]ConfigItem {
	out := make(map[string][]ConfigItem, len(configCatalog))
	for role, items := range configCatalog {
		cp := make([]ConfigItem, len(items))
		for i, it := range items {
			it.Opts = append([]string(nil), it.Opts...)
			cp[i] = it
		}
		out[role] = cp
	}
	return out
}

// PDShortKey 返回 pd-ctl `config set` 使用的短键：最后一个 '.' 之后的部分。
// 如 schedule.max-store-down-time → max-store-down-time。
func PDShortKey(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

var (
	intRe      = regexp.MustCompile(`^[0-9]{1,10}$`)
	sizeRe     = regexp.MustCompile(`(?i)^([0-9]{1,7})(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$`)
	durationRe = regexp.MustCompile(`^(?:[0-9]{1,6}[hms]){1,3}$`)
	durPartRe  = regexp.MustCompile(`([0-9]{1,6})([hms])`)
	// 归一化后的值必须满足该字符集：数字、字母与 . _ -，不含任何引号 / 空白 / 反斜杠 / 分号。
	safeValueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

const (
	maxSizeBytes    = int64(16) << 40 // 16 TiB 的合理性上限
	maxDurationSecs = int64(366 * 24 * 3600)
)

var sizeUnits = map[string]int64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}

// Normalize 校验并归一化配置值。返回值保证「注入安全」：仅含 [A-Za-z0-9._-]，
// 因而可以被放进 SQL 字符串字面量、pd-ctl 参数或 YAML 标量而不会逃逸。
// 归一化是幂等的：Normalize(it, Normalize(it, x)) == Normalize(it, x)。
//
//	int      纯数字（无符号、无空格），须落在 [Min,Max]
//	size     数字+单位 B/KB/MB/GB/TB（KiB 等价 KB…，大小写不敏感），输出大写单位，如 16GB
//	duration 由 h/m/s 组成（可组合，如 1h30m），输出最简整单位：2h / 90m / 45s
//	bool     on/off/true/false/1/0/yes/no → ON / OFF
//	enum     必须等于 Opts 之一（大小写不敏感，输出目录里的规范写法）
func Normalize(item ConfigItem, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s: 值不能为空", item.Key)
	}
	if len(raw) > 64 {
		return "", fmt.Errorf("%s: 值过长", item.Key)
	}
	var out string
	switch item.Type {
	case TypeInt:
		if !intRe.MatchString(raw) {
			return "", fmt.Errorf("%s: 必须是纯数字（不含符号、空格、小数点）", item.Key)
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		hi := item.Max
		if hi == 0 {
			hi = math.MaxInt32
		}
		if n < item.Min || n > hi {
			return "", fmt.Errorf("%s: 取值 %d 超出允许范围 %d–%d", item.Key, n, item.Min, hi)
		}
		out = strconv.FormatInt(n, 10)
	case TypeSize:
		m := sizeRe.FindStringSubmatch(raw)
		if m == nil {
			return "", fmt.Errorf("%s: 容量格式应形如 16GB / 512MB（单位 B/KB/MB/GB/TB）", item.Key)
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		unit := canonicalSizeUnit(m[2])
		bytes := n * sizeUnits[unit]
		if bytes > maxSizeBytes {
			return "", fmt.Errorf("%s: 容量过大", item.Key)
		}
		if bytes < item.Min || (item.Max > 0 && bytes > item.Max) {
			return "", fmt.Errorf("%s: 容量超出允许范围", item.Key)
		}
		out = strconv.FormatInt(n, 10) + unit
	case TypeDuration:
		if !durationRe.MatchString(raw) {
			return "", fmt.Errorf("%s: 时长格式应形如 30m / 2h / 90s", item.Key)
		}
		secs, err := parseDuration(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", item.Key, err)
		}
		if secs > maxDurationSecs {
			return "", fmt.Errorf("%s: 时长过长", item.Key)
		}
		if secs < item.Min || (item.Max > 0 && secs > item.Max) {
			return "", fmt.Errorf("%s: 时长超出允许范围", item.Key)
		}
		out = formatDuration(secs)
	case TypeBool:
		switch strings.ToLower(raw) {
		case "on", "true", "1", "yes":
			out = "ON"
		case "off", "false", "0", "no":
			out = "OFF"
		default:
			return "", fmt.Errorf("%s: 必须是 ON 或 OFF", item.Key)
		}
	case TypeEnum:
		for _, o := range item.Opts {
			if strings.EqualFold(raw, o) {
				out = o
				break
			}
		}
		if out == "" {
			return "", fmt.Errorf("%s: 取值必须是 %s 之一", item.Key, strings.Join(item.Opts, " / "))
		}
	default:
		return "", fmt.Errorf("%s: 未知的值类型 %q", item.Key, item.Type)
	}
	if !safeValueRe.MatchString(out) {
		return "", fmt.Errorf("%s: 归一化后的值不安全", item.Key)
	}
	return out, nil
}

func canonicalSizeUnit(u string) string {
	switch strings.ToUpper(u) {
	case "KB", "KIB":
		return "KB"
	case "MB", "MIB":
		return "MB"
	case "GB", "GIB":
		return "GB"
	case "TB", "TIB":
		return "TB"
	}
	return "B"
}

// SizeBytes 把已归一化的 size（如 16GB）换算成字节数。
func SizeBytes(norm string) (int64, error) {
	m := sizeRe.FindStringSubmatch(norm)
	if m == nil {
		return 0, fmt.Errorf("不是合法的容量: %q", norm)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n * sizeUnits[canonicalSizeUnit(m[2])], nil
}

// parseDuration 解析 1h30m 之类；单位必须按 h > m > s 降序且不重复。
func parseDuration(raw string) (int64, error) {
	rank := map[string]int{"h": 3, "m": 2, "s": 1}
	last := 4
	var total int64
	for _, m := range durPartRe.FindAllStringSubmatch(raw, -1) {
		if rank[m[2]] >= last {
			return 0, fmt.Errorf("时长单位顺序应为 h、m、s 且不可重复")
		}
		last = rank[m[2]]
		n, _ := strconv.ParseInt(m[1], 10, 64)
		switch m[2] {
		case "h":
			total += n * 3600
		case "m":
			total += n * 60
		default:
			total += n
		}
	}
	return total, nil
}

func formatDuration(secs int64) string {
	switch {
	case secs != 0 && secs%3600 == 0:
		return strconv.FormatInt(secs/3600, 10) + "h"
	case secs != 0 && secs%60 == 0:
		return strconv.FormatInt(secs/60, 10) + "m"
	}
	return strconv.FormatInt(secs, 10) + "s"
}

// DurationSeconds 把已归一化的 duration 换算成秒。
func DurationSeconds(norm string) (int64, error) {
	if !durationRe.MatchString(norm) {
		return 0, fmt.Errorf("不是合法的时长: %q", norm)
	}
	return parseDuration(norm)
}
