// @author: kevinkelly
// @license: Apache License 2.0
// @file: diagnose_more_test.go
// @time: 2026/10/28

package collect

import (
	"context"
	"strings"
	"testing"
	"time"

	"tidbops/internal/model"
)

func items(kv ...any) []model.Item {
	var out []model.Item
	for i := 0; i < len(kv); i += 2 {
		out = append(out, model.Item{Name: kv[i].(string), Pct: kv[i+1].(float64)})
	}
	return out
}

func byID(fs []model.Finding) map[string]model.Finding {
	m := map[string]model.Finding{}
	for _, f := range fs {
		m[f.ID] = f
	}
	return m
}

func TestDiagnoseDBTimeRules(t *testing.T) {
	dbt := model.DBTime{
		ByStage: items("Get Token", 6.0, "Parse", 4.0, "Compile", 25.0, "Execute", 65.0),
		ByExec:  items("TSO 等待", 35.0, "DistSQL 请求", 20.0, "KV PessimisticLock", 33.0, "KV Prewrite", 12.0),
	}
	got := byID(DiagnoseDBTime(dbt, 500))
	for _, id := range []string{"dbt-compile", "dbt-token", "dbt-tso", "dbt-lock"} {
		f, ok := got[id]
		if !ok {
			t.Errorf("缺少 %s（实际 %v）", id, got)
			continue
		}
		if f.Sev != "warning" || f.Scope != "tidb" || len(f.Evidence) == 0 || len(f.Actions) == 0 || f.Cause == "" || f.Ref == "" || f.Link.Page == "" {
			t.Errorf("%s 内容不完整: %+v", id, f)
		}
	}
	// 证据里要带上实际占比，便于核对
	if !strings.Contains(strings.Join(got["dbt-compile"].Evidence, " "), "Compile 25.0%") {
		t.Errorf("compile 证据: %v", got["dbt-compile"].Evidence)
	}
	// 锁冲突要给出可执行的系统表查询
	if c := got["dbt-lock"].Actions[0].Cmd; !strings.Contains(c, "DATA_LOCK_WAITS") {
		t.Errorf("锁冲突建议应含 DATA_LOCK_WAITS: %q", c)
	}
}

func TestDiagnoseDBTimeQuietCases(t *testing.T) {
	healthy := model.DBTime{
		ByStage: items("Get Token", 0.1, "Parse", 2.0, "Compile", 6.0, "Execute", 91.9),
		ByExec:  items("TSO 等待", 8.0, "DistSQL 请求", 50.0, "KV Get", 30.0, "KV PessimisticLock", 2.0),
	}
	if fs := DiagnoseDBTime(healthy, 500); len(fs) != 0 {
		t.Errorf("健康的 DB Time 不应有诊断项: %+v", fs)
	}
	bad := model.DBTime{ByStage: items("Compile", 60.0, "Execute", 40.0)}
	if fs := DiagnoseDBTime(bad, 3); len(fs) != 0 {
		t.Errorf("QPS 太低时占比没有意义，不应判读: %+v", fs)
	}
	if fs := DiagnoseDBTime(model.DBTime{}, 500); len(fs) != 0 {
		t.Errorf("取不到 DB Time 时不应有诊断项: %+v", fs)
	}
}

func fptr(v float64) *float64 { return &v }

func withDays(f model.Forecast, d80, d95 *float64) model.Forecast {
	f.DaysTo80, f.DaysTo95 = d80, d95
	return f
}

func TestDiagnoseForecast(t *testing.T) {
	base := model.Forecast{UsedGB: 6000, CapGB: 10000, GrowthGBPerDay: 120, Enough: true, Basis: "近 7 天 168 个点"}
	cases := []struct {
		name string
		f    model.Forecast
		sev  string // 空表示不应有诊断项
	}{
		{"5 天后到 80%", withDays(base, fptr(5), fptr(30)), "critical"},
		{"20 天后到 80%", withDays(base, fptr(20), fptr(50)), "warning"},
		{"90 天后到 80%", withDays(base, fptr(90), fptr(120)), ""},
		{"不增长（null）", withDays(base, nil, nil), ""},
	}
	for _, c := range cases {
		fs := DiagnoseForecast(c.f)
		if c.sev == "" {
			if len(fs) != 0 {
				t.Errorf("%s：不应有诊断项: %+v", c.name, fs)
			}
			continue
		}
		if len(fs) != 1 || fs[0].ID != "forecast" || fs[0].Sev != c.sev || fs[0].Link.Args["role"] != "tikv" {
			t.Errorf("%s：期望 %s: %+v", c.name, c.sev, fs)
		}
	}
	// 样本不足 / 已经超过 80%（交给磁盘水位规则）/ 容量未知：不给结论
	short := withDays(base, fptr(5), nil)
	short.Enough = false
	over := withDays(base, fptr(0), nil)
	over.UsedGB = 8500
	nocap := withDays(base, fptr(5), nil)
	nocap.CapGB = 0
	for name, f := range map[string]model.Forecast{"样本不足": short, "已超 80%": over, "容量未知": nocap} {
		if fs := DiagnoseForecast(f); len(fs) != 0 {
			t.Errorf("%s：不应有诊断项: %+v", name, fs)
		}
	}
}

func hotFinding() model.Finding {
	return model.Finding{ID: "hotw-tikv-2", Sev: "critical", Title: "tikv-2 疑似写入热点", Evidence: []string{"写流量 90 MB/s"}}
}

func TestSustainTracker(t *testing.T) {
	var tr tracker
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	run := func(now time.Time, fs ...model.Finding) []model.Finding { return tr.apply(now, fs) }

	// 第一次出现：热点规则要持续 3 分钟，先降为提示并标注观察中
	fs := run(t0, hotFinding())
	if fs[0].Sev != "info" || !strings.Contains(fs[0].Title, "观察中") || fs[0].ID != "hotw-tikv-2" {
		t.Fatalf("首次出现应降为观察中: %+v", fs[0])
	}
	// 2 分钟后仍在观察
	if fs = run(t0.Add(2*time.Minute), hotFinding()); fs[0].Sev != "info" {
		t.Fatalf("2 分钟仍应观察中: %+v", fs[0])
	}
	// 满 3 分钟：恢复原级别，证据写明持续时间与首次发现时间
	fs = run(t0.Add(3*time.Minute), hotFinding())
	if fs[0].Sev != "critical" || strings.Contains(fs[0].Title, "观察中") {
		t.Fatalf("持续满 3 分钟应恢复 critical: %+v", fs[0])
	}
	if ev := strings.Join(fs[0].Evidence, " "); !strings.Contains(ev, "已持续 3 分钟") || !strings.Contains(ev, "12:00:00") {
		t.Errorf("证据应写明持续时间: %v", fs[0].Evidence)
	}
	// 消失一次再出现：重新计时
	run(t0.Add(3*time.Minute + 30*time.Second))
	if fs = run(t0.Add(4*time.Minute), hotFinding()); fs[0].Sev != "info" {
		t.Fatalf("中间消失后应重新计时: %+v", fs[0])
	}
	// 快照断档超过 sustainGap（没人看页面 / 平台重启）也要重新计时
	run(t0.Add(10*time.Minute), hotFinding())
	if fs = run(t0.Add(10*time.Minute+sustainGap+time.Second), hotFinding()); fs[0].Sev != "info" {
		t.Fatalf("断档后应重新计时: %+v", fs[0])
	}
}

func TestSustainOnlyForListedRulesAndSorted(t *testing.T) {
	var tr tracker
	t0 := time.Now()
	in := []model.Finding{
		{ID: "slow-abc", Sev: "warning", Title: "慢 SQL"},
		{ID: "inst-tikv-1", Sev: "critical", Title: "tikv-1 状态异常"},
		{ID: "disk-tikv-4", Sev: "critical", Title: "磁盘"},
		{ID: "balance", Sev: "warning", Title: "Region 分布不均"},
		{ID: "dbt-compile", Sev: "warning", Title: "Compile 高"},
		{ID: "forecast", Sev: "critical", Title: "容量预测"},
	}
	out := tr.apply(t0, in)
	got := byID(out)
	want := map[string]string{"slow-abc": "warning", "inst-tikv-1": "critical", "disk-tikv-4": "critical", "forecast": "critical"}
	for id, sev := range want {
		if f := got[id]; strings.Contains(f.Title, "观察中") || f.Sev != sev {
			t.Errorf("%s 不应做持续判定: %+v", id, f)
		}
	}
	for _, id := range []string{"balance", "dbt-compile"} {
		if f := got[id]; f.Sev != "info" || !strings.Contains(f.Title, "观察中") {
			t.Errorf("%s 应处于观察中: %+v", id, f)
		}
	}
	// 结果按严重程度重新排序
	r := 0
	for _, f := range out {
		if rank(f.Sev) < r {
			t.Fatalf("未按严重程度排序: %+v", out)
		}
		r = rank(f.Sev)
	}
}

func TestSustainSkippedInReplayAndWhenDisabled(t *testing.T) {
	c := &Collector{Now: time.Now}
	fs := c.sustain("replay", []model.Finding{hotFinding()})
	if fs[0].Sev != "critical" || strings.Contains(fs[0].Title, "观察中") {
		t.Errorf("回放模式不做持续判定: %+v", fs[0])
	}
	c2 := &Collector{Now: time.Now, NoSustain: true}
	if fs = c2.sustain("live", []model.Finding{hotFinding()}); fs[0].Sev != "critical" {
		t.Errorf("NoSustain 时不做持续判定: %+v", fs[0])
	}
	c3 := &Collector{Now: time.Now}
	if fs = c3.sustain("live", []model.Finding{hotFinding()}); fs[0].Sev != "info" {
		t.Errorf("live 模式首次出现应观察中: %+v", fs[0])
	}
}

func TestForecastCachedUsesTTL(t *testing.T) {
	now := time.Now()
	c := &Collector{Now: func() time.Time { return now }, fcAt: now, fcVal: model.Forecast{UsedGB: 1, Enough: true}}
	if f := c.forecastCached(context.Background()); f.UsedGB != 1 {
		t.Errorf("TTL 内应直接返回缓存: %+v", f)
	}
}
