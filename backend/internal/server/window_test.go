// @author: kevinkelly
// @license: Apache License 2.0
// @file: window_test.go
// @time: 2026/10/28

package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tidbops/internal/audit"
)

var restartTiKV = map[string]any{"node": "tikv-2", "action": "restart"}

// nextWeekday10 返回明天起第一个工作日的本地 10:30（默认规则里落在「业务高峰」）。
func nextWeekday10() time.Time {
	d := time.Now().Local().AddDate(0, 0, 1)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 10, 30, 0, 0, time.Local)
}

func list(v any) []map[string]any {
	var out []map[string]any
	arr, _ := v.([]any)
	for _, x := range arr {
		m, _ := x.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestWindowSettingsReadWriteValidate(t *testing.T) {
	e := setupOps(t)
	admin, dba := e.login(t, "admin"), e.login(t, "dba")

	// 默认规则：settings.json 里没有 windows 字段时使用
	code, out := dba.do("GET", "/api/ops/settings", nil)
	if code != 200 {
		t.Fatalf("GET settings: %d %v", code, out)
	}
	ws := list(out["settings"].(map[string]any)["windows"])
	if len(ws) != 4 || out["windowsCustom"] != false {
		t.Fatalf("应返回 4 条默认规则: %v", out)
	}
	names := map[string]string{}
	for _, w := range ws {
		names[w["id"].(string)] = w["mode"].(string)
	}
	if names["backup"] != "avoid" || names["peak-am"] != "avoid" || names["peak-pm"] != "avoid" || names["night"] != "prefer" {
		t.Errorf("默认规则: %v", names)
	}

	// 权限：只有 CapSettings 才能改
	good := []map[string]any{{"name": "夜间封网", "days": []int{}, "from": "23:00", "to": "05:00", "mode": "block", "risks": []string{"high"}, "kinds": []string{}, "enabled": true, "note": "测试"}}
	if code, _ := dba.do("POST", "/api/ops/settings", map[string]any{"windows": good}); code != 403 {
		t.Errorf("DBA 不能改窗口规则: %d", code)
	}
	// 校验
	long := make([]map[string]any, 51)
	for i := range long {
		long[i] = map[string]any{"name": "x", "from": "01:00", "to": "02:00", "mode": "avoid", "enabled": true}
	}
	bads := map[string][]map[string]any{
		"时间格式":  {{"name": "x", "from": "25:00", "to": "02:00", "mode": "avoid"}},
		"mode":  {{"name": "x", "from": "01:00", "to": "02:00", "mode": "ban"}},
		"名称为空":  {{"name": "", "from": "01:00", "to": "02:00", "mode": "avoid"}},
		"名称过长":  {{"name": strings.Repeat("长", 41), "from": "01:00", "to": "02:00", "mode": "avoid"}},
		"星期":    {{"name": "x", "days": []int{9}, "from": "01:00", "to": "02:00", "mode": "avoid"}},
		"风险":    {{"name": "x", "from": "01:00", "to": "02:00", "mode": "avoid", "risks": []string{"urgent"}}},
		"超过 50": long,
	}
	for name, body := range bads {
		if code, out := admin.do("POST", "/api/ops/settings", map[string]any{"windows": body}); code != 400 || !strings.Contains(out["error"].(string), "窗口规则不合法") {
			t.Errorf("%s 应 400: %d %v", name, code, out)
		}
	}
	if _, out := admin.do("GET", "/api/ops/settings", nil); out["windowsCustom"] != false {
		t.Error("校验失败不应改变设置")
	}
	// 写入：自定义规则生效、持久化、写审计
	if code, out := admin.do("POST", "/api/ops/settings", map[string]any{"windows": good}); code != 200 {
		t.Fatalf("POST windows: %d %v", code, out)
	}
	_, out = dba.do("GET", "/api/ops/settings", nil)
	ws = list(out["settings"].(map[string]any)["windows"])
	if len(ws) != 1 || ws[0]["name"] != "夜间封网" || ws[0]["id"] != "w1" || ws[0]["from"] != "23:00" || out["windowsCustom"] != true {
		t.Errorf("自定义规则: %v", out)
	}
	if defs := list(out["windowDefaults"]); len(defs) != 4 {
		t.Errorf("应附带默认规则供界面恢复: %v", out["windowDefaults"])
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, "settings.json"))
	if err != nil || !strings.Contains(string(raw), "夜间封网") {
		t.Errorf("应持久化到 settings.json: %v %s", err, raw)
	}
	es, _ := e.audit.Query(audit.Filter{Action: "settings.update"}, 10)
	found := false
	for _, a := range es {
		if a.User == "admin" && strings.Contains(a.Target, "windows") {
			found = true
		}
	}
	if !found {
		t.Errorf("应写审计 settings.update(windows): %+v", es)
	}
	// 不带 windows 的其他设置更新不影响规则
	admin.do("POST", "/api/ops/settings", map[string]any{"lb": true})
	if _, out = dba.do("GET", "/api/ops/settings", nil); len(list(out["settings"].(map[string]any)["windows"])) != 1 {
		t.Error("更新其他设置不应重置窗口规则")
	}
	// 空数组 = 没有任何规则（不回退到默认）
	if code, _ := admin.do("POST", "/api/ops/settings", map[string]any{"windows": []any{}}); code != 200 {
		t.Fatal("清空规则应成功")
	}
	_, out = dba.do("GET", "/api/ops/settings", nil)
	if w := out["settings"].(map[string]any)["windows"]; w == nil || len(list(w)) != 0 || out["windowsCustom"] != true {
		t.Errorf("空数组应表示没有规则: %v", out)
	}
}

func TestWindowPreviewChecks(t *testing.T) {
	e := setupOps(t)
	dba, admin := e.login(t, "dba"), e.login(t, "admin")
	at := nextWeekday10()
	body := map[string]any{"kind": "inst.op", "params": restartTiKV, "scheduledAt": at.Format(time.RFC3339)}

	// 默认规则：工作日上午高峰 → avoid 警告，不阻断
	code, out := dba.do("POST", "/api/changes/preview", body)
	if code != 200 {
		t.Fatalf("preview: %d %v", code, out)
	}
	checks := list(out["windowChecks"])
	if out["windowBlocked"] != false || len(checks) == 0 {
		t.Fatalf("应有窗口检查且不阻断: %v", out)
	}
	hasWarn := false
	for _, c := range checks {
		if c["lv"] == "warn" && strings.Contains(c["t"].(string), "业务高峰") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Errorf("应提示业务高峰: %v", checks)
	}
	// 不带 scheduledAt：按当前时间，依然返回数组（可能为空）
	if _, out = dba.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": restartTiKV}); out["windowChecks"] == nil || out["windowBlocked"] == nil {
		t.Errorf("windowChecks / windowBlocked 必须总是存在: %v", out)
	}
	if code, _ := dba.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": restartTiKV, "scheduledAt": "明天"}); code != 400 {
		t.Errorf("scheduledAt 格式错误应 400: %d", code)
	}

	// 自定义 block 规则：非紧急阻断，紧急只警告
	block := []map[string]any{{"name": "核心交易封网", "from": "09:00", "to": "12:00", "mode": "block", "enabled": true}}
	if code, out := admin.do("POST", "/api/ops/settings", map[string]any{"windows": block}); code != 200 {
		t.Fatalf("设置规则: %d %v", code, out)
	}
	_, out = dba.do("POST", "/api/changes/preview", body)
	if out["windowBlocked"] != true || list(out["windowChecks"])[0]["lv"] != "bad" || !strings.Contains(list(out["windowChecks"])[0]["t"].(string), "核心交易封网") {
		t.Errorf("block 应阻断: %v", out)
	}
	body["emergency"] = true
	_, out = dba.do("POST", "/api/changes/preview", body)
	if out["windowBlocked"] != false || list(out["windowChecks"])[0]["lv"] != "warn" {
		t.Errorf("紧急变更应只警告: %v", out)
	}

	// 创建：打开写开关后，落在 block 窗口的定时变更被拒绝，并带规则名
	admin2 := e.login(t, "admin2")
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	if code, out := admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 200 || out["enabled"] != true {
		t.Fatalf("打开写开关: %d %v", code, out)
	}
	code, out = dba.do("POST", "/api/changes", map[string]any{"kind": "inst.op", "params": restartTiKV, "scheduledAt": at.Format(time.RFC3339)})
	if code != 409 || !strings.Contains(out["error"].(string), "核心交易封网") {
		t.Errorf("创建应被窗口规则拒绝: %d %v", code, out)
	}
}

func TestWindowRecommend(t *testing.T) {
	e := setupOps(t)
	dba, admin, viewer := e.login(t, "dba"), e.login(t, "admin"), e.login(t, "viewer")
	anon := e.client(t)
	req := map[string]any{"kind": "inst.op", "params": restartTiKV}

	if code, _ := anon.do("POST", "/api/changes/recommend", req); code != 401 {
		t.Errorf("未登录应 401: %d", code)
	}
	if code, _ := viewer.do("POST", "/api/changes/recommend", req); code != 403 {
		t.Errorf("没有该种类能力应 403: %d", code)
	}
	if code, _ := dba.do("POST", "/api/changes/recommend", map[string]any{"kind": "nope"}); code != 400 {
		t.Errorf("未知种类应 400: %d", code)
	}
	if code, _ := dba.do("POST", "/api/changes/recommend", map[string]any{"kind": "inst.op", "params": restartTiKV, "from": "x"}); code != 400 {
		t.Errorf("from 格式错误应 400: %d", code)
	}
	if code, out := dba.do("POST", "/api/changes/recommend", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "nope", "action": "restart"}}); code != 422 {
		t.Errorf("规划失败应 422: %d %v", code, out)
	}

	// 自定义一条跨午夜的 block 规则 + 默认之外的 prefer，检查推荐都合法且避开 block
	rules := []map[string]any{
		{"name": "夜间封网", "from": "23:00", "to": "02:00", "mode": "block", "enabled": true},
		{"name": "凌晨低峰", "from": "02:00", "to": "06:00", "mode": "prefer", "enabled": true},
		{"name": "白天高峰", "from": "08:00", "to": "20:00", "mode": "avoid", "enabled": true},
	}
	if code, out := admin.do("POST", "/api/ops/settings", map[string]any{"windows": rules}); code != 200 {
		t.Fatalf("设置规则: %d %v", code, out)
	}
	code, out := dba.do("POST", "/api/changes/recommend", map[string]any{"kind": "inst.op", "params": restartTiKV, "days": 99, "count": 99})
	if code != 200 {
		t.Fatalf("recommend: %d %v", code, out)
	}
	plan := out["plan"].(map[string]any)
	est := int(plan["estMins"].(float64))
	if plan["title"] == "" || plan["risk"] == "" || est <= 0 {
		t.Errorf("plan: %v", plan)
	}
	slots := list(out["slots"])
	if len(slots) == 0 || len(slots) > 10 {
		t.Fatalf("count 最大 10 且应有推荐: %d", len(slots))
	}
	now := time.Now()
	var prevScore float64 = 1e9
	for _, s := range slots {
		st, err1 := time.Parse(time.RFC3339, s["start"].(string))
		en, err2 := time.Parse(time.RFC3339, s["end"].(string))
		if err1 != nil || err2 != nil {
			t.Fatalf("时间格式: %v", s)
		}
		if en.Sub(st) != time.Duration(est)*time.Minute {
			t.Errorf("end 应为 start+estMins: %v", s)
		}
		if st.Before(now.Add(10*time.Minute)) || st.After(now.Add(15*24*time.Hour)) {
			t.Errorf("起点范围不合法: %v", st)
		}
		l := st.Local()
		if l.Minute()%30 != 0 || l.Second() != 0 {
			t.Errorf("未对齐 30 分钟: %v", l)
		}
		// 不落在 block 窗口 23:00–02:00（整个区间都不相交）
		for _, p := range []time.Time{l, en.Local().Add(-time.Second)} {
			if h := p.Hour(); h >= 23 || h < 2 {
				t.Errorf("落在 block 窗口: %v", s)
			}
		}
		sc := s["score"].(float64)
		if sc > prevScore {
			t.Errorf("应按分数降序: %v", slots)
		}
		prevScore = sc
		if rs, _ := s["reasons"].([]any); len(rs) == 0 {
			t.Errorf("应说明原因: %v", s)
		}
	}
	// 推荐的第一名应在凌晨推荐窗口内
	if st, _ := time.Parse(time.RFC3339, slots[0]["start"].(string)); st.Local().Hour() < 2 || st.Local().Hour() >= 6 {
		t.Errorf("第一名应落在凌晨低峰: %v", slots[0])
	}
	// 规则全部清空后也能推荐（只受 30 分钟对齐与最早时间约束）
	admin.do("POST", "/api/ops/settings", map[string]any{"windows": []any{}})
	if code, out := dba.do("POST", "/api/changes/recommend", map[string]any{"kind": "inst.op", "params": restartTiKV, "count": 3}); code != 200 || len(list(out["slots"])) != 3 {
		t.Errorf("无规则也应推荐: %d %v", code, out)
	}
}
