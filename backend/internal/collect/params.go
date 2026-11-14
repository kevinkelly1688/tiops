// @author: kevinkelly
// @license: Apache License 2.0
// @file: params.go
// @time: 2026/10/28

package collect

import (
	"context"
	"fmt"
	"strings"

	"tidbops/internal/model"
)

type paramRule struct {
	scope, name, rec, why, set string
	// bad 判断当前值是否偏离推荐；nil 表示只展示
	bad func(cur string) bool
}

var paramRules = []paramRule{
	{"tidb", "tidb_mem_quota_query", "2 GiB 并配合 tidb_mem_oom_action=CANCEL", "限制单条 SQL 内存，防止 OOM 影响整个节点", "SET GLOBAL tidb_mem_quota_query = 2147483648;", func(c string) bool { return c == "1073741824" }},
	{"tidb", "tidb_enable_prepared_plan_cache", "ON", "启用后可明显降低 Compile 开销", "SET GLOBAL tidb_enable_prepared_plan_cache = ON;", func(c string) bool { return strings.EqualFold(c, "OFF") || c == "0" }},
	{"tidb", "tidb_gc_life_time", "≥ 最长事务时长；备份期间需 ≥ 备份时长", "过短会导致 GC life time is shorter 报错，过长占用存储", "SET GLOBAL tidb_gc_life_time = '30m';", nil},
	{"tidb", "tidb_replica_read", "closest-replicas（跨 AZ 部署）", "读请求就近读取，降低跨机房延迟", "SET GLOBAL tidb_replica_read = 'closest-replicas';", nil},
	{"tidb", "tidb_slow_log_threshold", "300（OLTP，单位 ms）", "慢日志阈值，过低会产生大量日志", "SET GLOBAL tidb_slow_log_threshold = 300;", func(c string) bool { var v int; fmt.Sscan(c, &v); return v > 0 && v < 100 }},
	{"tikv", "storage.block-cache.capacity", "≤ 40%–45% 内存（与其他服务混部需更低）", "过大可能引发 TiKV OOM", "tiup cluster edit-config <集群>   # storage.block-cache.capacity\ntiup cluster reload <集群> -R tikv", nil},
	{"tikv", "raftstore.store-pool-size", "2–5（视 Raftstore CPU 而定）", "propose wait duration 高时可调大", "tiup cluster edit-config <集群>   # raftstore.store-pool-size: 3\ntiup cluster reload <集群> -R tikv", func(c string) bool { var v int; fmt.Sscan(c, &v); return v > 5 }},
	{"tikv", "storage.scheduler-worker-pool-size", "按 CPU 核数，≥ 8 核可适当调大", "Scheduler 线程繁忙时增大", "tiup cluster edit-config <集群>   # storage.scheduler-worker-pool-size: 6\ntiup cluster reload <集群> -R tikv", nil},
	{"pd", "schedule.region-schedule-limit", "按迁移速度需求，业务高峰调小", "控制 Region 调度速度，保护在线业务", "pd-ctl config set region-schedule-limit 1024", func(c string) bool { var v int; fmt.Sscan(c, &v); return v > 4096 }},
	{"pd", "replication.location-labels", "zone,rack,host（按真实机房拓扑）", "保证副本分散在不同故障域", "pd-ctl config set location-labels zone,rack,host", func(c string) bool { return c != "" && !strings.Contains(c, "zone") }},
	{"pd", "schedule.max-store-down-time", "30m（计划内长维护需临时调大）", "TiKV 失联超过该时间 PD 判定 Down 并补副本", "pd-ctl config set max-store-down-time 30m", nil},
}

// ParamRows 汇总关键参数的当前值与推荐。取不到的值显示 "—"，不猜测。
func (c *Collector) ParamRows(ctx context.Context) []model.ParamRow {
	src := c.loadConfigSources(ctx) // 与 /api/config-values 共用同一来源
	var out []model.ParamRow
	for _, r := range paramRules {
		row := model.ParamRow{Scope: r.scope, Name: r.name, Rec: r.rec, Why: r.why, Set: r.set, Cur: "—", State: "na"}
		if list := src.values(r.scope, r.name); len(list) > 0 {
			row.Cur = strings.Join(list, "、")
			row.Drift = len(list) > 1
			row.State = "ok"
			if r.bad != nil {
				for _, v := range list {
					if r.bad(v) {
						row.State = "warn"
					}
				}
			}
			if row.Drift && row.State == "ok" {
				row.State = "warn"
			}
		}
		out = append(out, row)
	}
	return out
}
