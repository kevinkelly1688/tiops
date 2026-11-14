// @author: kevinkelly
// @license: Apache License 2.0
// @file: collect_test.go
// @time: 2026/10/28

package collect_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tidbops/internal/collect"
	"tidbops/internal/fakecluster"
	"tidbops/internal/fixtures"
	"tidbops/internal/model"
	"tidbops/internal/source"
)

func params() source.CatalogParams {
	return source.CatalogParams{Cluster: "prod-tidb-01", Home: "/home/tidb", Now: time.Now()}
}

func liveCollector(t *testing.T) (*collect.Collector, func()) {
	c := fakecluster.Start("prod-tidb-01")
	live := source.NewLive(source.LiveConfig{Endpoints: c.Endpoints(), MySQL: source.MySQLConfig{Host: "10.0.1.11", User: "ro"}, Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile})
	return collect.New(live, params()), c.Close
}

func inst(t *testing.T, topo model.Topology, id string) model.Instance {
	for _, in := range topo.Instances {
		if in.ID == id {
			return in
		}
	}
	t.Fatalf("拓扑中没有实例 %s", id)
	return model.Instance{}
}

func TestTopology(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	topo, errs := col.Topology(context.Background())
	if len(errs) != 0 {
		t.Fatalf("不应有采集错误: %v", errs)
	}
	count := map[string]int{}
	for _, in := range topo.Instances {
		count[in.Type]++
	}
	if count["tidb"] != 3 || count["pd"] != 3 || count["tikv"] != 6 || count["tiflash"] != 2 || count["ticdc"] != 2 || len(topo.Instances) != 16 {
		t.Fatalf("实例数量不对: %v", count)
	}
	if topo.Cluster != "prod-tidb-01" || topo.Version != "v8.5.2" || topo.User != "tidb" {
		t.Errorf("集群信息不对: %+v", topo)
	}
	if !inst(t, topo, "pd-1").Leader || inst(t, topo, "pd-2").Leader {
		t.Error("PD Leader 应只有 pd-1")
	}
	k2 := inst(t, topo, "tikv-2")
	if k2.Host != "10.0.2.12" || k2.Port != 20160 || k2.StatusPort != 20180 || k2.SID != 2 || k2.AZ != "az1" {
		t.Errorf("tikv-2 基本信息不对: %+v", k2)
	}
	if k2.Status != "warn" || k2.CPU < 85 || k2.WR < 200 || k2.RaftCPU < 85 {
		t.Errorf("tikv-2 应为 CPU 高 / 写流量高的告警状态: %+v", k2)
	}
	k4 := inst(t, topo, "tikv-4")
	if k4.Disk < 85 || k4.Status != "warn" || !strings.Contains(k4.Note, "磁盘") {
		t.Errorf("tikv-4 磁盘应 ≥ 85%% 并告警: %+v", k4)
	}
	if k4.Regions < 16000 || k4.CapacityGB < 3500 || k4.Leaders == 0 {
		t.Errorf("Store 统计未带入: %+v", k4)
	}
	d3 := inst(t, topo, "tidb-3")
	if d3.Status != "warn" || d3.Mem < 80 || d3.QPS < 3700 || d3.Conn < 270 || d3.Conn > 300 {
		t.Errorf("tidb-3 应为内存高的告警状态且带 QPS / 连接数: %+v", d3)
	}
	if f := inst(t, topo, "tiflash-1"); f.SID != 21 || f.AZ != "az1" || f.Disk < 40 {
		t.Errorf("TiFlash 应匹配到 Store 21: %+v", f)
	}
	if inst(t, topo, "tikv-1").Status != "up" || inst(t, topo, "tidb-1").Status != "up" {
		t.Error("健康实例应为 up")
	}
}

func TestTopologyDegradesWithoutTiup(t *testing.T) {
	// 不采集 tiup 相关条目 → 从 PD / cluster_info 推导
	c := fakecluster.Start("c")
	defer c.Close()
	live := source.NewLive(source.LiveConfig{Endpoints: c.Endpoints(), MySQL: source.MySQLConfig{Host: "h", User: "u"}, Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile})
	root := filepath.Join(t.TempDir(), "pkg")
	items := source.FilterItems(source.DefaultCatalog(params()), nil, []string{"tiup/"})
	rec := source.NewRecorder(live, root, "c", "t", fixtures.Options{})
	rec.RecordAll(context.Background(), items)
	if err := rec.Finish(items); err != nil {
		t.Fatal(err)
	}
	rp, err := source.NewReplay(root)
	if err != nil {
		t.Fatal(err)
	}
	col := collect.New(rp, params())
	topo, errs := col.Topology(context.Background())
	if len(topo.Instances) != 14 {
		t.Fatalf("降级后应推导出 14 个实例（3 TiDB + 3 PD + 6 TiKV + 2 TiFlash），实际 %d", len(topo.Instances))
	}
	joined := strings.Join(errs, "；")
	if !strings.Contains(joined, "tiup") {
		t.Errorf("应说明 tiup 拓扑不可用: %v", errs)
	}
	if !inst(t, topo, "pd-1").Leader {
		t.Error("降级模式下仍应识别 PD Leader")
	}
}

func TestSnapshotOverviewSlowHotAlertsEnv(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	snap := col.Snapshot(context.Background(), "live")
	if len(snap.Errors) != 0 {
		t.Fatalf("不应有采集错误: %v", snap.Errors)
	}
	ov := snap.Overview
	if ov.QPS < 9500 || ov.QPS > 11500 || ov.P99ms < 25 || ov.P99ms > 35 || ov.Connections < 650 || ov.Connections > 800 {
		t.Errorf("总览指标不对: %+v", ov)
	}
	if ov.Regions != 48210 || ov.StoresTotal != 8 || ov.StoresUp != 8 || ov.StorageCapGB < 21000 || ov.AlertsCrit != 2 || ov.AlertsWarn != 1 {
		t.Errorf("总览汇总不对: %+v", ov)
	}
	if len(snap.Trends.QPS) < 60 || snap.Trends.Step != 60 || len(snap.Trends.P99ms) != len(snap.Trends.QPS) {
		t.Errorf("趋势数据不对: %d 点, step %d", len(snap.Trends.QPS), snap.Trends.Step)
	}
	if m := snap.Trends.P99ms[len(snap.Trends.P99ms)-1]; m < 5 {
		t.Errorf("P99 趋势应为毫秒且非 0（秒值不能被提前取整）: %v", m)
	}
	// 慢 SQL：按累计耗时排序，标签与建议
	if len(snap.Slow) != 6 {
		t.Fatalf("应有 6 组慢 SQL，实际 %d", len(snap.Slow))
	}
	tags := map[string]string{}
	for _, g := range snap.Slow {
		tags[g.Digest] = g.Tag
	}
	want := map[string]string{"e810aa": collect.TagKVQueue, "a3f9c1": "缺索引 / 统计信息过期", "7bd024": "大查询占用内存高", "5c7e12": "索引列上使用函数", "2dd7f0": "前导通配符 LIKE", "91ab33": "高频写入（注意写热点）"}
	for d, tag := range want {
		if tags[d] != tag {
			t.Errorf("digest %s 标签 = %q，期望 %q", d, tags[d], tag)
		}
	}
	if snap.Slow[0].Digest != "e810aa" || snap.Slow[0].ID != 1 || snap.Slow[1].Digest != "a3f9c1" {
		t.Errorf("应按累计耗时降序: %s %s", snap.Slow[0].Digest, snap.Slow[1].Digest)
	}
	a3 := snap.Slow[1]
	if a3.Exec != 18240 || a3.Avg < 2.8 || a3.Avg > 2.9 || !strings.Contains(a3.SQL, "orders") || strings.Contains(a3.SQL, "13800138000") == false {
		// 注意：Live 直连模式下的原始 SQL 未脱敏；脱敏发生在 Recorder 导出时
		t.Errorf("a3f9c1 聚合不对: %+v", a3)
	}
	hasAnalyze := false
	for _, tip := range a3.Tips {
		if strings.Contains(tip.Cmd, "ANALYZE TABLE orders") {
			hasAnalyze = true
		}
	}
	if !hasAnalyze {
		t.Errorf("缺索引建议里应包含 ANALYZE TABLE orders: %+v", a3.Tips)
	}
	// 热点
	if len(snap.Hot) == 0 || snap.Hot[0].Store != 2 || snap.Hot[0].Table != "orders" || snap.Hot[0].Leader != "tikv-2" {
		t.Errorf("热点 Region 不对: %+v", snap.Hot)
	}
	// 告警
	if len(snap.Alerts) != 3 || snap.Alerts[0].Lv != "critical" || snap.Alerts[2].Lv != "warning" {
		t.Errorf("告警排序 / 数量不对: %+v", snap.Alerts)
	}
	// 环境探测
	env := map[string]model.EnvItem{}
	for _, e := range snap.Env {
		env[e.Key] = e
	}
	for k, want := range map[string]string{"ctl.os": "Rocky Linux 9.3 (Blue Onyx)", "ctl.kernel": "5.14.0-362.el9.x86_64", "tiup.ver": "v1.16.1", "cl.name": "prod-tidb-01", "cl.ver": "v8.5.2", "ctl.user": "tidb", "addr.pd": "http://10.0.1.21:2379", "addr.prom": "http://10.0.1.31:9090", "addr.grafana": "http://10.0.1.31:3000", "addr.alert": "http://10.0.1.31:9093", "cl.region": "约 48,210"} {
		if e := env[k]; !e.Detected || e.Value != want {
			t.Errorf("环境项 %s = %+v，期望 %q", k, e, want)
		}
	}
	if e := env["ctl.res"]; !strings.HasPrefix(e.Value, "8 核") || env["cl.inst"].Value != "TiDB 3 · PD 3 · TiKV 6 · TiFlash 2 · TiCDC 2" {
		t.Errorf("资源 / 规模探测不对: %+v %+v", e, env["cl.inst"])
	}
}

func TestDiagnose(t *testing.T) {
	col, closeFn := liveCollector(t)
	defer closeFn()
	col.NoSustain = true // 持续时间判定另有专门的测试（diagnose_more_test.go）；这里只看规则本身
	snap := col.Snapshot(context.Background(), "live")
	ids := map[string]model.Finding{}
	for _, f := range snap.Findings {
		ids[f.ID] = f
	}
	for _, id := range []string{"disk-tikv-4", "hotw-tikv-2", "tidbres-tidb-3", "slow-a3f9c1", "slow-e810aa", "alerts"} {
		if _, ok := ids[id]; !ok {
			t.Errorf("缺少诊断项 %s（实际: %v）", id, keys(ids))
		}
	}
	if f := ids["disk-tikv-4"]; f.Sev != "critical" || f.Link.Page != "nodes" || f.Link.Args["role"] != "tikv" || len(f.Actions) == 0 {
		t.Errorf("磁盘诊断项不完整: %+v", f)
	}
	if f := ids["hotw-tikv-2"]; f.Sev != "critical" || !strings.Contains(strings.Join(f.Evidence, " "), "orders") || len(f.Actions) < 3 {
		t.Errorf("写热点诊断项不完整（应带表名与 ≥3 条建议）: %+v", f)
	}
	// 严重程度排序：critical 在前
	last := 0
	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	for _, f := range snap.Findings {
		if rank[f.Sev] < last {
			t.Fatalf("诊断结果未按严重程度排序: %v", keys(ids))
		}
		last = rank[f.Sev]
	}
	// 健康集群不应有磁盘 / 热点诊断
	healthy := model.Topology{Cluster: "c", Instances: []model.Instance{
		{ID: "tikv-1", Type: "tikv", Status: "up", Disk: 50, WR: 30, Regions: 100, SID: 1}, {ID: "tikv-2", Type: "tikv", Status: "up", Disk: 52, WR: 33, Regions: 102, SID: 2}, {ID: "tikv-3", Type: "tikv", Status: "up", Disk: 51, WR: 31, Regions: 99, SID: 3}}}
	if fs := collect.Diagnose(healthy, nil, nil, nil, nil); len(fs) != 0 {
		t.Errorf("健康集群不应有诊断项: %+v", fs)
	}
	down := healthy
	down.Instances = append([]model.Instance{}, healthy.Instances...)
	down.Instances[0].Status = "down"
	if fs := collect.Diagnose(down, nil, nil, nil, nil); len(fs) != 1 || fs[0].Sev != "critical" || !strings.HasPrefix(fs[0].ID, "inst-") {
		t.Errorf("实例停止应触发 critical 诊断: %+v", fs)
	}
}

func keys(m map[string]model.Finding) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

// 导出 → 回放：回放得到的快照与直连一致，并且 SQL 文本已脱敏。
func TestReplaySnapshotMatchesLiveAndIsSanitized(t *testing.T) {
	c := fakecluster.Start("prod-tidb-01")
	defer c.Close()
	live := source.NewLive(source.LiveConfig{Endpoints: c.Endpoints(), MySQL: source.MySQLConfig{Host: "10.0.1.11", User: "ro"}, Exec: fakecluster.Exec(), ReadFile: fakecluster.ReadFile})
	root := filepath.Join(t.TempDir(), "pkg")
	items := source.DefaultCatalog(params())
	rec := source.NewRecorder(live, root, "prod-tidb-01", "t", fixtures.Options{MaskSQLLiterals: true, RedactSecrets: true})
	rec.RecordAll(context.Background(), items)
	if err := rec.Finish(items); err != nil {
		t.Fatal(err)
	}
	rp, err := source.NewReplay(root)
	if err != nil {
		t.Fatal(err)
	}
	a := collect.New(live, params()).Snapshot(context.Background(), "live")
	b := collect.New(rp, params()).Snapshot(context.Background(), "replay")
	if len(b.Errors) != 0 {
		t.Fatalf("回放不应有错误: %v", b.Errors)
	}
	if len(a.Topology.Instances) != len(b.Topology.Instances) || a.Overview.Regions != b.Overview.Regions || len(a.Findings) != len(b.Findings) || len(a.Slow) != len(b.Slow) {
		t.Errorf("回放与直连的快照应一致: live=%d/%d/%d replay=%d/%d/%d", len(a.Topology.Instances), a.Overview.Regions, len(a.Findings), len(b.Topology.Instances), b.Overview.Regions, len(b.Findings))
	}
	for _, g := range b.Slow {
		if strings.Contains(g.SQL, "13800138000") || strings.Contains(g.SQL, "SKU-1001") || strings.Contains(g.SQL, "10086") {
			t.Errorf("回放的慢 SQL 应已脱敏: %s", g.SQL)
		}
	}
	if b.Mode != "replay" {
		t.Errorf("mode = %s", b.Mode)
	}
	// 新增采集项（DB Time、集群事实、热点范围、配置当前值、容量预测）在回放中与直连一致
	if len(b.DBTime.ByType) == 0 || len(b.DBTime.ByType) != len(a.DBTime.ByType) || len(b.DBTime.ByStage) != 4 || len(b.DBTime.ByExec) != len(a.DBTime.ByExec) {
		t.Errorf("回放的 DB Time 应与直连一致: live=%+v replay=%+v", a.DBTime, b.DBTime)
	}
	if b.Topology.Replicas != 3 || b.Topology.AZCount != 3 || len(b.Topology.LocationLabels) != 2 {
		t.Errorf("回放的集群事实: %+v", b.Topology)
	}
	if len(b.Hot) == 0 || b.Hot[0].Range != a.Hot[0].Range || b.Hot[0].Range == "" {
		t.Errorf("回放的热点范围: %+v vs %+v", b.Hot, a.Hot)
	}
	rc := collect.New(rp, params())
	if cv := rc.ConfigValues(context.Background(), specsFromCatalog()); !cv.Values["tikv"]["raftstore.store-pool-size"].Drift || cv.Values["pd"]["schedule.max-store-down-time"].Norm != "30m" {
		t.Errorf("回放的配置当前值: %+v", cv.Values["tikv"]["raftstore.store-pool-size"])
	}
	if f := rc.Forecast(context.Background()); !f.Enough || f.GrowthGBPerDay < 100 {
		t.Errorf("回放的容量预测: %+v", f)
	}
}

func TestMissingDataIsReportedNotFatal(t *testing.T) {
	// 只有 PD 可用（没有 Prometheus / Alertmanager / SQL / tiup）
	c := fakecluster.Start("c")
	defer c.Close()
	ep := map[string]string{"pd": c.PD.URL}
	live := source.NewLive(source.LiveConfig{Endpoints: ep, Exec: func(context.Context, []string, []string) ([]byte, error) { return nil, context.Canceled }, ReadFile: fakecluster.ReadFile})
	snap := collect.New(live, params()).Snapshot(context.Background(), "live")
	if len(snap.Errors) == 0 {
		t.Error("缺失的数据应记入 Errors")
	}
	if len(snap.Topology.Instances) == 0 {
		t.Error("只有 PD 时也应从 Store 推导出 TiKV / TiFlash 实例")
	}
	if snap.Alerts == nil || snap.Slow == nil || snap.Hot == nil {
		t.Error("列表字段应为空数组而不是 nil（便于前端处理）")
	}
}

func TestSlowGroupsCarryDecodedPlans(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	snap := c.Snapshot(context.Background(), "live")
	var a3, e8 *model.SlowGroup
	for i := range snap.Slow {
		switch snap.Slow[i].Digest {
		case "a3f9c1":
			a3 = &snap.Slow[i]
		case "e810aa":
			e8 = &snap.Slow[i]
		}
	}
	if a3 == nil || e8 == nil {
		t.Fatal("缺少慢 SQL 分组")
	}
	if len(a3.Plan) != 4 || a3.Plan[3][0] != "    └─TableFullScan_14" || a3.Plan[3][1] != 2400000.0 || a3.Plan[3][2] != 4200000.0 || a3.Plan[3][3] != "cop[tikv]" || a3.Plan[3][4] != "table:orders" {
		t.Errorf("a3f9c1 的执行计划解析不对: %+v", a3.Plan)
	}
	if len(e8.Plan) != 0 {
		t.Errorf("没有计划的 SQL 应保持空计划: %+v", e8.Plan)
	}
}

func TestParsePlanVariants(t *testing.T) {
	// 无 actRows、无 access object 列；index 访问对象带括号内的逗号
	txt := "\tid\ttask\testRows\toperator info\n" +
		"\tIndexLookUp_10\troot\t100.00\t\n" +
		"\t└─IndexRangeScan_8\tcop[tikv]\t100.00\ttable:t, index:idx_ab(a, b), range:[1,1], keep order:false\n"
	p := collect.ParsePlan(txt)
	if len(p) != 2 {
		t.Fatalf("应解析出 2 个算子: %+v", p)
	}
	if p[1][2] != 100.0 {
		t.Errorf("缺少 actRows 时应取 estRows: %+v", p[1])
	}
	if p[1][4] != "table:t, index:idx_ab(a, b)" || p[1][5] != "range:[1,1], keep order:false" {
		t.Errorf("access object 拆分不对: %q | %q", p[1][4], p[1][5])
	}
	if collect.ParsePlan("garbage") != nil || collect.ParsePlan("") != nil {
		t.Error("无表头 id 列应返回 nil")
	}
}

func TestParamRows(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	rows := c.ParamRows(context.Background())
	by := map[string]model.ParamRow{}
	for _, r := range rows {
		by[r.Name] = r
	}
	if r := by["tidb_mem_quota_query"]; r.Cur != "1073741824" || r.State != "warn" {
		t.Errorf("tidb_mem_quota_query: %+v", r)
	}
	if r := by["raftstore.store-pool-size"]; !r.Drift || !strings.Contains(r.Cur, "2") || !strings.Contains(r.Cur, "3") || r.State != "warn" {
		t.Errorf("tikv-3 单独调过，应识别为配置漂移: %+v", r)
	}
	if r := by["schedule.region-schedule-limit"]; r.Cur != "2048" || r.State != "ok" {
		t.Errorf("PD 参数应取自 pd/config: %+v", r)
	}
	if r := by["tidb_gc_life_time"]; r.Cur != "10m0s" {
		t.Errorf("gc: %+v", r)
	}
}

func TestTrendsCarryPerStoreWriteFlow(t *testing.T) {
	c, done := liveCollector(t)
	defer done()
	snap := c.Snapshot(context.Background(), "live")
	if len(snap.Trends.StoreWrite) < 6 {
		t.Fatalf("应有各 TiKV 的写流量序列: %d", len(snap.Trends.StoreWrite))
	}
	for _, s := range snap.Trends.StoreWrite {
		if len(s.Vals) != len(snap.Trends.QPS) {
			t.Errorf("%s 序列长度应与时间轴一致: %d vs %d", s.Inst, len(s.Vals), len(snap.Trends.QPS))
		}
	}
}
