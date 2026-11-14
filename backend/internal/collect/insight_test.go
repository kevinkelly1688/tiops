// @author: kevinkelly
// @license: Apache License 2.0
// @file: insight_test.go
// @time: 2026/10/28

package collect_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"tidbops/internal/collect"
	"tidbops/internal/exec"
	"tidbops/internal/fakecluster"
	"tidbops/internal/model"
	"tidbops/internal/source"
)

// stubSource 按条目名返回固定内容，用于构造边界数据。未登记的条目返回错误。
type stubSource map[string]string

func (s stubSource) Mode() string { return "stub" }
func (s stubSource) Fetch(_ context.Context, it source.Item) ([]byte, error) {
	if v, ok := s[it.Name]; ok {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("stub: 没有条目 %s", it.Name)
}

func stubCollector(s stubSource) *collect.Collector { return collect.New(s, params()) }

func specsFromCatalog() []collect.ConfigSpec {
	var out []collect.ConfigSpec
	for _, items := range exec.Catalog() {
		for _, it := range items {
			out = append(out, collect.ConfigSpec{Role: it.Role, Key: it.Key, Type: string(it.Type), Opts: it.Opts, Sys: it.Sys, Scope: it.Scope})
		}
	}
	return out
}

// ---------------------------------------------------------------- 配置当前值 ----

func TestConfigValuesFromFakeCluster(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	cv := c.ConfigValues(context.Background(), specsFromCatalog())

	// 目录里每个配置项都应该有值（模拟集群没有 TiCDC 的 cluster_config 行，所以 ticdc 为空）
	for role, items := range exec.Catalog() {
		for _, it := range items {
			_, ok := cv.Values[role][it.Key]
			if role == "ticdc" {
				if ok {
					t.Errorf("TiCDC 不在 cluster_config 中，不应有值: %s", it.Key)
				}
				continue
			}
			if !ok {
				t.Errorf("%s / %s 应该有当前值", role, it.Key)
			}
		}
	}

	// 一致的实例级配置：16GiB → norm 16GB，无漂移
	bc := cv.Values["tikv"]["storage.block-cache.capacity"]
	if len(bc.Instances) != 6 || bc.Instances["10.0.2.11:20160"] != "16GiB" || bc.Cluster != "" || bc.Drift || bc.Majority != "16GiB" || bc.Norm != "16GB" {
		t.Errorf("block-cache: %+v", bc)
	}
	if len(bc.InstNorm) != 6 || bc.InstNorm["10.0.2.16:20160"] != "16GB" {
		t.Errorf("各实例的归一化值: %+v", bc.InstNorm)
	}
	// tikv-3 单独调过：漂移，多数派 2
	pool := cv.Values["tikv"]["raftstore.store-pool-size"]
	if !pool.Drift || pool.Majority != "2" || pool.Instances["10.0.2.13:20160"] != "3" || pool.Instances["10.0.2.11:20160"] != "2" || pool.Norm != "2" {
		t.Errorf("store-pool-size 应漂移（tikv-3=3，多数派 2）: %+v", pool)
	}
	if v := cv.Values["tikv"]["log.level"]; v.Drift || v.Norm != "info" || len(v.Instances) != 6 {
		t.Errorf("tikv log.level: %+v", v)
	}
	// 同名键 log.level 按 TYPE 分开：tidb 3 个实例、pd 3 个实例
	if len(cv.Values["tidb"]["log.level"].Instances) != 3 || len(cv.Values["pd"]["log.level"].Instances) != 3 {
		t.Errorf("log.level 应按组件分开: tidb=%+v pd=%+v", cv.Values["tidb"]["log.level"], cv.Values["pd"]["log.level"])
	}
	if v := cv.Values["tiflash"]["profiles.default.max_memory_usage"]; v.Norm != "0GB" || len(v.Instances) != 2 {
		t.Errorf("tiflash 内存上限 0 应归一为 0GB: %+v", v)
	}
	// 系统变量：集群级，字节数 → 1GB
	mq := cv.Values["tidb"]["tidb_mem_quota_query"]
	if mq.Cluster != "1073741824" || mq.Norm != "1GB" || len(mq.Instances) != 0 || mq.Drift {
		t.Errorf("tidb_mem_quota_query: %+v", mq)
	}
	if v := cv.Values["tidb"]["tidb_enable_prepared_plan_cache"]; v.Cluster != "ON" || v.Norm != "ON" {
		t.Errorf("prepared plan cache: %+v", v)
	}
	if v := cv.Values["tidb"]["tidb_replica_read"]; v.Cluster != "leader" || v.Norm != "leader" {
		t.Errorf("replica read: %+v", v)
	}
	// PD 集群级：保持原值 30m0s，同时给出归一化值 30m
	dt := cv.Values["pd"]["schedule.max-store-down-time"]
	if dt.Cluster != "30m0s" || dt.Norm != "30m" {
		t.Errorf("max-store-down-time 应保留原值并给出 norm: %+v", dt)
	}
	if v := cv.Values["pd"]["schedule.region-schedule-limit"]; v.Cluster != "2048" || v.Norm != "2048" {
		t.Errorf("region-schedule-limit: %+v", v)
	}
	if v := cv.Values["pd"]["replication.max-replicas"]; v.Cluster != "3" {
		t.Errorf("max-replicas: %+v", v)
	}

	// JSON 形状：与前端约定一致
	b, _ := json.Marshal(cv)
	var raw map[string]map[string]map[string]map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	e := raw["values"]["tikv"]["raftstore.store-pool-size"]
	if e["cluster"] != "" || e["drift"] != true || e["majority"] != "2" || e["instances"] == nil {
		t.Errorf("JSON 形状不对: %v", e)
	}
}

func TestConfigValuesOmitMissingAndNormalizeEdgeCases(t *testing.T) {
	cc := "TYPE\tINSTANCE\tKEY\tVALUE\n" +
		// 同一容量的不同写法：不算漂移（按归一化值比较）；多数派取个数最多的写法
		"tikv\tn1:20160\tstorage.block-cache.capacity\t16GiB\n" +
		"tikv\tn2:20160\tstorage.block-cache.capacity\t17179869184\n" +
		"tikv\tn3:20160\tstorage.block-cache.capacity\t16GiB\n" +
		// 两种取值各 2 个：并列时取字典序最小的取值，结果稳定
		"tikv\tn1:20160\traftstore.apply-pool-size\t4\n" +
		"tikv\tn2:20160\traftstore.apply-pool-size\t4\n" +
		"tikv\tn3:20160\traftstore.apply-pool-size\t2\n" +
		"tikv\tn4:20160\traftstore.apply-pool-size\t2\n" +
		// TiKV 自动计算出的带小数容量：能给出以 B 为单位的归一化值，不报错
		"tikv\tn1:20160\tstorage.scheduler-worker-pool-size\t4\n" +
		// 不关心的 TYPE 被忽略
		"tiflash-learner\tl1:20170\tlog.level\twarn\n"
	pd := `{"schedule":{"max-store-down-time":"1h0m0s","region-schedule-limit":2048,"leader-schedule-limit":4},"replication":{"max-replicas":5,"location-labels":["zone","rack","host"]}}`
	c := stubCollector(stubSource{"sql/cluster-config": cc, "pd/config": pd, "sql/variables": "VARIABLE_NAME\tVARIABLE_VALUE\ntidb_slow_log_threshold\t300\n"})
	cv := c.ConfigValues(context.Background(), specsFromCatalog())

	bc := cv.Values["tikv"]["storage.block-cache.capacity"]
	if bc.Drift || bc.Majority != "16GiB" || bc.Norm != "16GB" || bc.Instances["n2:20160"] != "17179869184" || bc.InstNorm["n2:20160"] != "16GB" {
		t.Errorf("16GiB 与 17179869184 字节应视为一致: %+v", bc)
	}
	if ap := cv.Values["tikv"]["raftstore.apply-pool-size"]; !ap.Drift || ap.Majority != "2" {
		t.Errorf("并列时多数派取字典序最小者，且应判定漂移: %+v", ap)
	}
	if d := cv.Values["pd"]["schedule.max-store-down-time"]; d.Cluster != "1h0m0s" || d.Norm != "1h" {
		t.Errorf("1h0m0s → 1h: %+v", d)
	}
	if v := cv.Values["pd"]["replication.max-replicas"]; v.Cluster != "5" {
		t.Errorf("max-replicas: %+v", v)
	}
	if v := cv.Values["tidb"]["tidb_slow_log_threshold"]; v.Cluster != "300" || v.Norm != "300" {
		t.Errorf("slow_log_threshold: %+v", v)
	}
	// 取不到的项不出现（不猜测默认值）
	for _, k := range []string{"rocksdb.max-background-jobs", "server.grpc-concurrency"} {
		if _, ok := cv.Values["tikv"][k]; ok {
			t.Errorf("没有采集到的 %s 应被省略", k)
		}
	}
	if _, ok := cv.Values["tidb"]["tidb_mem_quota_query"]; ok {
		t.Error("没有采集到的系统变量应被省略")
	}
	if len(cv.Values["tikv"]["log.level"].Instances) != 0 {
		t.Error("tiflash-learner 的行不应归入任何角色")
	}
	if cv.Values["ticdc"] == nil || cv.Values["tiflash"] == nil {
		t.Error("五个角色键都应存在（可能为空对象）")
	}

	// 什么都取不到：不报错，各角色为空对象
	empty := stubCollector(stubSource{}).ConfigValues(context.Background(), specsFromCatalog())
	for role, m := range empty.Values {
		if len(m) != 0 {
			t.Errorf("%s 应为空: %v", role, m)
		}
	}
}

func TestNormalizeConfigMatchesExecNormalize(t *testing.T) {
	// 归一化形式必须与 exec.Normalize 一致，前端才能直接比较「用户输入归一化后」与「当前值归一化后」
	cat := exec.Catalog()
	find := func(role, key string) (exec.ConfigItem, collect.ConfigSpec) {
		for _, it := range cat[role] {
			if it.Key == key {
				return it, collect.ConfigSpec{Role: role, Key: key, Type: string(it.Type), Opts: it.Opts, Sys: it.Sys, Scope: it.Scope}
			}
		}
		t.Fatalf("目录里没有 %s/%s", role, key)
		return exec.ConfigItem{}, collect.ConfigSpec{}
	}
	cases := []struct{ role, key, cur string }{
		{"tikv", "storage.block-cache.capacity", "16GiB"},
		{"tikv", "storage.block-cache.capacity", "512MiB"},
		{"tikv", "raftstore.store-pool-size", "3"},
		{"tikv", "log.level", "INFO"},
		{"pd", "schedule.max-store-down-time", "30m0s"},
		{"pd", "schedule.max-store-down-time", "2h0m0s"},
		{"pd", "schedule.max-store-down-time", "1h30m0s"},
		{"tidb", "tidb_enable_prepared_plan_cache", "1"},
		{"tidb", "tidb_enable_prepared_plan_cache", "off"},
		{"tidb", "tidb_mem_quota_query", "2GiB"},
		{"tidb", "tidb_slow_log_threshold", "300"},
		{"tidb", "tidb_replica_read", "Closest-Replicas"},
		{"tiflash", "logger.level", "warning"},
	}
	for _, tc := range cases {
		it, sp := find(tc.role, tc.key)
		want, err := exec.Normalize(it, tc.cur)
		if err != nil {
			t.Fatalf("%s=%s 不是合法值，修正测试数据: %v", tc.key, tc.cur, err)
		}
		if got := collect.NormalizeConfig(sp, tc.cur); got != want {
			t.Errorf("NormalizeConfig(%s, %q) = %q，exec.Normalize 给出 %q", tc.key, tc.cur, got, want)
		}
	}
	// 当前值超出目录范围（目录的 Min/Max 只约束修改，不约束读到的值）时仍然给出归一化值
	_, sp := find("tidb", "tidb_slow_log_threshold")
	if got := collect.NormalizeConfig(sp, "10"); got != "10" {
		t.Errorf("超出目录范围的当前值也应归一化: %q", got)
	}
	// 无法归一化 → 空串，而不是猜
	_, size := find("tikv", "storage.block-cache.capacity")
	for _, bad := range []string{"", "abc", "16 apples", "-1GiB"} {
		if got := collect.NormalizeConfig(size, bad); got != "" {
			t.Errorf("%q 不应被归一化: %q", bad, got)
		}
	}
	if got := collect.NormalizeConfig(size, "13.5GiB"); got != "13824MB" {
		t.Errorf("带小数的容量应折算成整数单位: %q", got)
	}
	if got := collect.NormalizeConfig(size, "0"); got != "0GB" {
		t.Errorf("0 → 0GB（与目录默认值写法一致）: %q", got)
	}
}

// ---------------------------------------------------------------- DB Time ----

func sumPct(items []model.Item) float64 {
	s := 0.0
	for _, it := range items {
		s += it.Pct
	}
	return math.Round(s*10) / 10
}

func TestDBTimeFromFakeCluster(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	snap := c.Snapshot(context.Background(), "live")
	if len(snap.Errors) != 0 {
		t.Fatalf("不应有采集错误: %v", snap.Errors)
	}
	d := snap.DBTime
	for name, items := range map[string][]model.Item{"byType": d.ByType, "byStage": d.ByStage, "byExec": d.ByExec} {
		if len(items) == 0 {
			t.Fatalf("%s 不应为空", name)
		}
		if s := sumPct(items); s != 100 {
			t.Errorf("%s 占比之和 = %v，应为 100: %+v", name, s, items)
		}
	}
	// 按类型：前 6 名 + 其他，Select 占比最大
	if len(d.ByType) != 7 || d.ByType[0].Name != "Select" || d.ByType[len(d.ByType)-1].Name != "其他" {
		t.Errorf("byType: %+v", d.ByType)
	}
	if d.ByType[0].Pct < 43 || d.ByType[0].Pct > 46 {
		t.Errorf("Select 占比应约 44.7%%: %+v", d.ByType[0])
	}
	// 按阶段：固定顺序；Get Token 的指标单位是微秒，必须换算后才是 ~1%（不换算会变成 ~100%）
	var names []string
	by := map[string]float64{}
	for _, it := range d.ByStage {
		names = append(names, it.Name)
		by[it.Name] = it.Pct
	}
	if strings.Join(names, ",") != "Get Token,Parse,Compile,Execute" {
		t.Errorf("byStage 顺序: %v", names)
	}
	if by["Get Token"] < 0.5 || by["Get Token"] > 1.6 || by["Parse"] < 2.5 || by["Parse"] > 3.5 || by["Compile"] < 16 || by["Compile"] > 18 || by["Execute"] < 77 || by["Execute"] > 81 {
		t.Errorf("byStage 占比不对（Get Token 需要从微秒换算）: %+v", d.ByStage)
	}
	// 执行阶段细分：TSO / DistSQL / 前 4 种 KV 请求 + 其他 KV
	have := map[string]bool{}
	for _, it := range d.ByExec {
		have[it.Name] = true
	}
	for _, n := range []string{"TSO 等待", "DistSQL 请求", "KV Cop", "KV Commit", "KV Prewrite", "KV Get", "KV 其他"} {
		if !have[n] {
			t.Errorf("byExec 缺少 %q: %+v", n, d.ByExec)
		}
	}
	if have["KV Rollback"] {
		t.Errorf("排在前 4 之外的请求类型应并入「KV 其他」: %+v", d.ByExec)
	}
	// JSON：数组而不是 null
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"byType":[{"name":"Select","pct":`) {
		t.Errorf("JSON 形状: %s", b)
	}
}

func TestDBTimeMissingDimensionsAreEmptyArrays(t *testing.T) {
	// 没有任何 Prometheus 数据：三个维度都是空数组（JSON []，不是 null），且不报错
	d := stubCollector(stubSource{}).DBTime(context.Background())
	b, _ := json.Marshal(d)
	if string(b) != `{"byType":[],"byStage":[],"byExec":[]}` {
		t.Errorf("无数据时应为空数组: %s", b)
	}
	// 只有按类型的数据：其他维度为空；缺少 Execute 时阶段占比没有意义，保持为空
	mat := func(metric string, v float64) string {
		return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":%s,"values":[[1,"%g"],[2,"%g"]]}]}}`, metric, v, v)
	}
	s := stubSource{
		"prom/dbtime-type":    `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"sql_type":"Select"},"values":[[1,"3"],[2,"3"]]},{"metric":{"sql_type":"Insert"},"values":[[1,"1"],[2,"1"]]}]}}`,
		"prom/dbtime-compile": mat("{}", 2),
	}
	d = stubCollector(s).DBTime(context.Background())
	if len(d.ByType) != 2 || d.ByType[0].Pct != 75 || d.ByType[1].Pct != 25 {
		t.Errorf("byType: %+v", d.ByType)
	}
	if len(d.ByStage) != 0 || len(d.ByExec) != 0 {
		t.Errorf("缺少 Execute 时不给阶段占比: %+v", d)
	}
	// 占比之和恒为 100：三项各 1/3
	s = stubSource{"prom/dbtime-type": `{"status":"success","data":{"resultType":"matrix","result":[` +
		`{"metric":{"sql_type":"A"},"values":[[1,"1"]]},{"metric":{"sql_type":"B"},"values":[[1,"1"]]},{"metric":{"sql_type":"C"},"values":[[1,"1"]]}]}}`}
	if d := stubCollector(s).DBTime(context.Background()); sumPct(d.ByType) != 100 || len(d.ByType) != 3 {
		t.Errorf("三等分的占比之和应为 100: %+v", d.ByType)
	}
}

// ---------------------------------------------------------------- 集群事实 ----

func TestTopologyFacts(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	topo, _ := c.Topology(context.Background())
	if topo.Replicas != 3 || strings.Join(topo.LocationLabels, ",") != "zone,host" || topo.AZCount != 3 {
		t.Errorf("集群事实: replicas=%d labels=%v az=%d", topo.Replicas, topo.LocationLabels, topo.AZCount)
	}
	b, _ := json.Marshal(topo)
	for _, want := range []string{`"replicas":3`, `"locationLabels":["zone","host"]`, `"azCount":3`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("拓扑 JSON 缺少 %s", want)
		}
	}
	// 没有 PD 配置时为零值，不猜
	empty, _ := stubCollector(stubSource{}).Topology(context.Background())
	if empty.Replicas != 0 || len(empty.LocationLabels) != 0 || empty.AZCount != 0 {
		t.Errorf("取不到时应为零值: %+v", empty)
	}
	// 实例没有 zone 标签：AZ 数为 0（"—" 不计）
	st := `{"stores":[{"store":{"id":1,"address":"h1:20160","state_name":"Up"},"status":{}},{"store":{"id":2,"address":"h2:20160","state_name":"Up","labels":[{"key":"zone","value":"z1"}]},"status":{}}]}`
	one, _ := stubCollector(stubSource{"pd/stores": st}).Topology(context.Background())
	if one.AZCount != 1 {
		t.Errorf("只有一个已知 zone: %d", one.AZCount)
	}
}

// ---------------------------------------------------------------- 热点 Region 范围 ----

func TestHotRegionRangeFromFakeCluster(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	snap := c.Snapshot(context.Background(), "live")
	if len(snap.Hot) == 0 || snap.Hot[0].ID != 20871 {
		t.Fatalf("热点: %+v", snap.Hot)
	}
	if got := snap.Hot[0].Range; got != "table_id=112 row 12345 … row 1012345" {
		t.Errorf("热点 Region 范围 = %q", got)
	}
	b, _ := json.Marshal(snap.Hot[0])
	if !strings.Contains(string(b), `"range":"table_id=112 row 12345`) {
		t.Errorf("JSON: %s", b)
	}
	// 热点统计自带 Key 时优先使用它
	stats := `{"as_leader":{"2":{"total_flow_bytes":9000000,"statistics":[{"region_id":7,"flow_bytes":9000000,"hot_degree":3,"start_key":"` +
		fakecluster.IndexKey(55, 3) + `","end_key":"` + fakecluster.IndexKey(55, 4) + `"}]}}}`
	hs := stubCollector(stubSource{"pd/hot-write": stats})
	list, _, err := hs.Hot(context.Background())
	if err == nil || len(list) != 1 || list[0].Range != "table_id=55 index 3 … index 4" {
		t.Errorf("统计自带的 Key: %+v err=%v", list, err)
	}
}

func TestRegionRangeDecodeAndFallback(t *testing.T) {
	k := fakecluster.TableKey
	cases := []struct{ name, start, end, want string }{
		{"行键，同表", k(112, 12345), k(112, 98765), "table_id=112 row 12345 … row 98765"},
		{"行键，负 RowID", k(5, -10), k(5, 10), "table_id=5 row -10 … row 10"},
		{"跨表", k(112, 1), k(113, 0), "table_id=112 row 1 … table_id=113 row 0"},
		{"同一索引", fakecluster.IndexKey(77, 2), fakecluster.IndexKey(77, 2), "table_id=77 index 2"},
		{"不同索引", fakecluster.IndexKey(77, 2), fakecluster.IndexKey(77, 3), "table_id=77 index 2 … index 3"},
		{"起点为空（最左 Region）", "", k(10, 0), "-∞ … table_id=10 row 0"},
		{"终点为空（最右 Region）", k(10, 0), "", "table_id=10 row 0 … +∞"},
		{"被截断的表前缀（残缺分组）", "7480000000000000FF70", "7480000000000000FF71", "table_id=112 … table_id=113"},
		{"未编码的原始键（t + 表 ID）", "74800000000000007B", "74800000000000007C", "table_id=123 … table_id=124"},
		{"元数据键", "6D44423A3100000000FF0000", k(1, 0), "TiDB 元数据 … table_id=1 row 0"},
		// 解码失败：回退为截断的十六进制
		{"非 TiDB 键", "0102030405", "0102030406", "0102030405 → 0102030406"},
		{"非法十六进制", "ZZ", "01", "ZZ → 01"},
		{"过长的十六进制被截断", "AABBCCDDEEFF00112233445566778899", "AABBCCDDEEFF00112233445566778899FF", "AABBCCDD…66778899 → AABBCCDD…778899FF"},
		{"一端可解码一端不可", k(1, 1), "0102", strings.ToUpper(k(1, 1))[:8] + "…" + k(1, 1)[len(k(1, 1))-8:] + " → 0102"},
	}
	for _, tc := range cases {
		if got := collect.RegionRange(tc.start, tc.end); got != tc.want {
			t.Errorf("%s: RegionRange(%q, %q)\n got  %q\n want %q", tc.name, tc.start, tc.end, got, tc.want)
		}
	}
	if got := collect.RegionRange("", ""); got != "" {
		t.Errorf("两端都空表示整个 Key 空间，不展示: %q", got)
	}
	// 聚簇索引（非整数主键）的行键：handle 不是 8 字节整数，不能当作 RowID 解码
	raw := append([]byte{'t'}, 0x80, 0, 0, 0, 0, 0, 0, 0x2A, '_', 'r', 0x01, 0x61, 0x62, 0x63, 0x00, 0xFF, 0x01, 0x02, 0x03)
	if got := collect.RegionRange(fakeEncode(raw), fakeEncode(raw)); !strings.Contains(got, "table_id=42 row（聚簇索引键）") {
		t.Errorf("聚簇索引行键: %q", got)
	}
}

// fakeEncode 用模拟集群的 memcomparable 编码把原始键编成十六进制。
func fakeEncode(raw []byte) string {
	return strings.ToUpper(fmt.Sprintf("%x", fakecluster.MemcmpEncode(raw)))
}

// ---------------------------------------------------------------- 容量预测 ----

func hourly(n int, end time.Time, f func(i int) float64) *collect.Series {
	s := &collect.Series{Metric: map[string]string{}}
	for i := 0; i < n; i++ {
		s.Points = append(s.Points, collect.Point{T: end.Add(-time.Duration(n-1-i) * time.Hour).Unix(), V: f(i)})
	}
	return s
}

const gb = 1 << 30

func TestForecastRegression(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// 7 天每小时一个点：从 1000 GiB 起每天 +10 GiB；容量 2000 GiB；Region 每天 +24
	n := 7*24 + 1
	used := hourly(n, now, func(i int) float64 { return (1000 + 10*float64(i)/24) * gb })
	capc := hourly(n, now, func(int) float64 { return 2000 * gb })
	reg := hourly(n, now, func(i int) float64 { return 5000 + 24*float64(i)/24 })
	f := collect.ForecastFrom(used, capc, reg)
	curUsed := 1000 + 10*float64(n-1)/24 // 1070 GiB
	if !f.Enough || f.GrowthGBPerDay != 10 || f.CapGB != 2000 || math.Abs(f.UsedGB-curUsed) > 0.1 {
		t.Fatalf("回归结果: %+v", f)
	}
	// 到 80%（1600 GiB）还差 530 GiB → 53 天；到 95%（1900 GiB）还差 830 GiB → 83 天
	if f.DaysTo80 == nil || math.Abs(*f.DaysTo80-53) > 0.2 || f.DaysTo95 == nil || math.Abs(*f.DaysTo95-83) > 0.2 {
		t.Errorf("到达天数: %+v / %+v", f.DaysTo80, f.DaysTo95)
	}
	if f.Regions != 5168 || f.RegionGrowthPerDay != 24 {
		t.Errorf("Region: %d %+v", f.Regions, f.RegionGrowthPerDay)
	}
	if !strings.Contains(f.Basis, "线性回归") || !strings.Contains(f.Basis, "169") {
		t.Errorf("依据说明: %q", f.Basis)
	}

	// 已超过 80% 水位：daysTo80 = 0，daysTo95 仍按增长速度计算
	over := hourly(n, now, func(i int) float64 { return (1700 + 10*float64(i)/24) * gb })
	f = collect.ForecastFrom(over, capc, nil)
	if f.DaysTo80 == nil || *f.DaysTo80 != 0 || f.DaysTo95 == nil || *f.DaysTo95 <= 0 || f.Regions != 0 {
		t.Errorf("已超过 80%%: %+v", f)
	}
	// 容量在缩小：不会到达 → null
	shrink := hourly(n, now, func(i int) float64 { return (1500 - 5*float64(i)/24) * gb })
	f = collect.ForecastFrom(shrink, capc, nil)
	b, _ := json.Marshal(f)
	if !f.Enough || f.GrowthGBPerDay != -5 || f.DaysTo80 != nil || !strings.Contains(string(b), `"daysTo80":null`) || !strings.Contains(string(b), `"daysTo95":null`) {
		t.Errorf("不增长时到达天数应为 null: %s", b)
	}
	// 只取最近 7 天：更早的数据（斜率不同）不参与回归
	long := hourly(14*24+1, now, func(i int) float64 {
		if i < 7*24 {
			return 100 * gb // 前 7 天持平
		}
		return (100 + 20*float64(i-7*24)/24) * gb // 后 7 天每天 +20
	})
	if f := collect.ForecastFrom(long, capc, nil); f.GrowthGBPerDay != 20 {
		t.Errorf("应只用最近 7 天: %+v", f)
	}
}

func TestForecastNotEnoughSamples(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	used := hourly(12, now, func(i int) float64 { return (1000 + float64(i)) * gb })
	capc := hourly(12, now, func(int) float64 { return 2000 * gb })
	f := collect.ForecastFrom(used, capc, hourly(12, now, func(i int) float64 { return 100 }))
	if f.Enough || f.DaysTo80 != nil || f.DaysTo95 != nil || f.GrowthGBPerDay != 0 {
		t.Errorf("不足 24 小时不应给出预测: %+v", f)
	}
	if f.UsedGB != 1011 || f.CapGB != 2000 || f.Regions != 100 || !strings.Contains(f.Basis, "样本不足") || !strings.Contains(f.Basis, "12") {
		t.Errorf("样本不足时仍给出当前值与原因: %+v", f)
	}
	// 23 个点（22 小时）也不够；24 个点（23 小时）刚好够
	if f := collect.ForecastFrom(hourly(23, now, func(i int) float64 { return float64(i) * gb }), capc, nil); f.Enough {
		t.Error("23 个点不应够")
	}
	if f := collect.ForecastFrom(hourly(24, now, func(i int) float64 { return float64(i) * gb }), capc, nil); !f.Enough {
		t.Error("24 个小时点应够")
	}
	// 没有容量指标
	if f := collect.ForecastFrom(nil, nil, nil); f.Enough || !strings.Contains(f.Basis, "未采集") {
		t.Errorf("没有指标: %+v", f)
	}
	if f := stubCollector(stubSource{}).Forecast(context.Background()); f.Enough || !strings.Contains(f.Basis, "未采集") {
		t.Errorf("取不到数据: %+v", f)
	}
}

func TestForecastFromFakeCluster(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	f := c.Forecast(context.Background())
	if !f.Enough {
		t.Fatalf("模拟集群有 7 天数据: %+v", f)
	}
	// 模拟数据：已用 14010 GiB、每天 +120 GiB，容量 21504 GiB → 80% 水位（17203 GiB）约 27 天，95% 约 54 天
	if math.Abs(f.UsedGB-14010) > 30 || f.CapGB != 21504 || math.Abs(f.GrowthGBPerDay-120) > 8 {
		t.Errorf("容量预测: %+v", f)
	}
	if f.DaysTo80 == nil || *f.DaysTo80 < 22 || *f.DaysTo80 > 32 || f.DaysTo95 == nil || *f.DaysTo95 < 45 || *f.DaysTo95 > 64 || *f.DaysTo95 <= *f.DaysTo80 {
		t.Errorf("到达天数: %v %v", f.DaysTo80, f.DaysTo95)
	}
	if math.Abs(float64(f.Regions)-48210) > 100 || math.Abs(f.RegionGrowthPerDay-150) > 25 {
		t.Errorf("Region: %d %.1f", f.Regions, f.RegionGrowthPerDay)
	}
}

func TestParamRowsShareConfigSource(t *testing.T) {
	// ParamRows 复用配置来源：只用 3 次读取（变量 / cluster_config / pd 配置），且取值与 ConfigValues 一致
	c, done := liveCollector(t)
	defer done()
	rows := c.ParamRows(context.Background())
	by := map[string]model.ParamRow{}
	for _, r := range rows {
		by[r.Name] = r
	}
	if r := by["storage.block-cache.capacity"]; r.Cur != "16GiB" || r.Drift {
		t.Errorf("block-cache: %+v", r)
	}
	if r := by["replication.location-labels"]; r.Cur != "zone,host" || r.State != "ok" {
		t.Errorf("location-labels: %+v", r)
	}
	// PD 返回数组形式的 location-labels 时同样可用
	pd := `{"replication":{"max-replicas":3,"location-labels":["zone","rack","host"]}}`
	rows = stubCollector(stubSource{"pd/config": pd}).ParamRows(context.Background())
	for _, r := range rows {
		if r.Name == "replication.location-labels" && r.Cur != "zone,rack,host" {
			t.Errorf("数组形式的 location-labels: %+v", r)
		}
	}
}

func TestNormalizeConfigSizeInBytes(t *testing.T) {
	sp := collect.ConfigSpec{Role: "tidb", Key: "tidb_mem_quota_query", Type: "size", Sys: true, Scope: "cluster"}
	for in, want := range map[string]string{"2147483648": "2GB", "1073741824": "1GB", "536870912": "512MB", "1099511627776": "1TB", "1000": "1000B", "17179869184": "16GB"} {
		if got := collect.NormalizeConfig(sp, in); got != want {
			t.Errorf("NormalizeConfig(%q) = %q，期望 %q", in, got, want)
		}
	}
}
