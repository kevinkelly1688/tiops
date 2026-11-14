// @author: kevinkelly
// @license: Apache License 2.0
// @file: catalog_insight_test.go
// @time: 2026/10/28

package source_test

import (
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"tidbops/internal/exec"
	"tidbops/internal/source"
)

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

// 采集目录里的配置键必须与配置项目录（exec.Catalog）保持一致：目录新增 / 删除配置项时，这里会失败，提醒同步 catalog.go。
func TestConfigKeysMatchExecCatalog(t *testing.T) {
	inst := map[string]bool{}
	sys := map[string]bool{"tidb_gc_life_time": true} // 参数建议页额外使用
	for _, items := range exec.Catalog() {
		for _, it := range items {
			switch {
			case it.Sys:
				sys[it.Key] = true
			case it.Scope == "instance":
				inst[it.Key] = true
			}
		}
	}
	var wantInst, wantSys []string
	for k := range inst {
		wantInst = append(wantInst, k)
	}
	for k := range sys {
		wantSys = append(wantSys, k)
	}
	if got, want := sortedCopy(source.ConfigInstanceKeys), sortedCopy(wantInst); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ConfigInstanceKeys 与 exec.Catalog() 的实例级配置键不一致:\n got  %v\n want %v", got, want)
	}
	if got, want := sortedCopy(source.SystemVariables), sortedCopy(wantSys); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SystemVariables 与 exec.Catalog() 的系统变量不一致:\n got  %v\n want %v", got, want)
	}

	// 生成的 SQL 恰好包含这些键、通过只读校验，且没有目录之外的键
	byName := map[string]source.Item{}
	for _, it := range source.DefaultCatalog(source.CatalogParams{Cluster: "c", Home: "/home/tidb", Now: time.Now()}) {
		byName[it.Name] = it
	}
	cc := byName["sql/cluster-config"]
	if err := source.ValidateItem(cc); err != nil {
		t.Fatal(err)
	}
	for _, k := range source.ConfigInstanceKeys {
		if !strings.Contains(cc.Query, "'"+k+"'") {
			t.Errorf("cluster_config 查询缺少键 %s", k)
		}
	}
	if n := strings.Count(cc.Query, "','") + 1; n != len(source.ConfigInstanceKeys) {
		t.Errorf("cluster_config 查询里的键个数 = %d，期望 %d", n, len(source.ConfigInstanceKeys))
	}
	for _, bad := range []string{"password", "secret", "cert", "key-path", "ssl"} {
		if strings.Contains(strings.ToLower(cc.Query), bad) {
			t.Errorf("cluster_config 查询不应包含敏感键 %q", bad)
		}
	}
	vars := byName["sql/variables"]
	if err := source.ValidateItem(vars); err != nil {
		t.Fatal(err)
	}
	for _, v := range source.SystemVariables {
		if !strings.Contains(vars.Query, "'"+v+"'") {
			t.Errorf("系统变量查询缺少 %s", v)
		}
	}
}

// 新增的 DB Time / 容量预测 / 热点范围采集项都通过只读校验，且容量预测固定为 7 天、3600 秒步长。
func TestInsightItemsInCatalog(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	items := map[string]source.Item{}
	for _, it := range source.DefaultCatalog(source.CatalogParams{Cluster: "c", Home: "/home/tidb", Now: now, Since: 2 * time.Hour}) {
		if err := source.ValidateItem(it); err != nil {
			t.Errorf("未通过校验: %v", err)
		}
		items[it.Name] = it
	}
	for _, n := range []string{"prom/dbtime-type", "prom/dbtime-token", "prom/dbtime-parse", "prom/dbtime-compile", "prom/dbtime-execute",
		"prom/dbtime-exec-tso", "prom/dbtime-exec-distsql", "prom/dbtime-exec-kv", "pd/regions-writeflow", "pd/regions-readflow"} {
		it, ok := items[n]
		if !ok {
			t.Errorf("缺少采集项 %s", n)
			continue
		}
		if !it.Optional {
			t.Errorf("%s 应为可选项（缺失不影响其他数据）", n)
		}
		if strings.HasPrefix(n, "prom/") && !strings.Contains(it.Path, "step=60") {
			t.Errorf("%s 应使用 60 秒步长: %s", n, it.Path)
		}
	}
	if q := items["prom/dbtime-type"].Path; !strings.Contains(q, url.QueryEscape(`sql_type!="internal"`)) {
		t.Errorf("DB Time 按类型应排除内部 SQL: %s", q)
	}
	for _, n := range []string{"prom/forecast-used", "prom/forecast-capacity", "prom/forecast-regions"} {
		it, ok := items[n]
		if !ok {
			t.Errorf("缺少采集项 %s", n)
			continue
		}
		u, err := url.Parse(it.Path)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("step") != "3600" || q.Get("end") != "1800000000" || q.Get("start") != "1799395200" {
			t.Errorf("%s 应固定为 7 天 / 3600 秒（与 Since 无关）: %s", n, it.Path)
		}
	}
	// 目录里每个条目名都能作为数据包文件名（由 TestDefaultCatalogAllValid 覆盖），这里确认总数没有意外膨胀
	if len(items) < 50 || len(items) > 100 {
		t.Errorf("采集项数量异常: %d", len(items))
	}
}

// 诊断第二批新增的采集项：全部通过只读校验、都是可选项（缺失不影响其它数据）；慢日志扩展聚合不取 SQL 文本。
func TestSecondBatchItemsInCatalog(t *testing.T) {
	items := map[string]source.Item{}
	for _, it := range source.DefaultCatalog(source.CatalogParams{Cluster: "c", Home: "/home/tidb", Now: time.Unix(1_800_000_000, 0)}) {
		items[it.Name] = it
	}
	for _, n := range []string{"prom/pd-miss-peer", "prom/pd-down-peer", "prom/pd-pending-peer", "prom/pd-etcd-fsync-p99", "prom/pd-etcd-rtt-p99", "prom/pd-store-slow-score",
		"prom/tikv-apply-cpu", "prom/tikv-write-stall", "prom/tikv-append-log-p99", "prom/tikv-apply-log-p99", "prom/tikv-latch-wait-p99",
		"prom/node-inode-used", "prom/node-fs-readonly", "prom/node-disk-read-lat", "prom/node-disk-write-lat", "prom/node-load-per-core", "prom/node-tcp-estab", "sql/slow-ext"} {
		it, ok := items[n]
		if !ok {
			t.Errorf("缺少采集项 %s", n)
			continue
		}
		if err := source.ValidateItem(it); err != nil {
			t.Errorf("%s 未通过校验: %v", n, err)
		}
		if !it.Optional {
			t.Errorf("%s 应为可选项", n)
		}
	}
	ext := items["sql/slow-ext"]
	for _, want := range []string{"cluster_slow_query", "GROUP BY Digest, Plan_digest", "INTERVAL 2 HOUR", "Backoff_types", "Stats LIKE '%pseudo%'", "LIMIT 1000"} {
		if !strings.Contains(ext.Query, want) {
			t.Errorf("slow-ext 查询缺少 %q: %s", want, ext.Query)
		}
	}
	if strings.Contains(strings.ToLower(ext.Query), "query,") || strings.Contains(strings.ToLower(ext.Query), " query ") || len(ext.SQLCols) != 0 {
		t.Errorf("slow-ext 不应取 SQL 文本（没有需要脱敏的列）: %s", ext.Query)
	}
}
