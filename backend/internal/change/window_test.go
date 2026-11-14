// @author: kevinkelly
// @license: Apache License 2.0
// @file: window_test.go
// @time: 2026/10/28

package change

import (
	"context"
	"strings"
	"testing"
	"time"
)

// lt 构造本地时区时间（窗口规则按服务器本地时区解释，测试也用本地时区，避免依赖机器时区）。
func lt(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.Local)
}

// 2026-10-05 是周一。
func TestFixtureDateIsMonday(t *testing.T) {
	if wd := lt(2026, 10, 5, 0, 0).Weekday(); wd != time.Monday {
		t.Fatalf("测试日期假设有误: %v", wd)
	}
}

func win(name, mode, from, to string, days ...int) Window {
	return Window{ID: name, Name: name, Days: days, From: from, To: to, Mode: mode, Enabled: true}
}

func TestWindowOccurrences(t *testing.T) {
	night := win("night", WindowPrefer, "22:00", "06:00") // 每天，跨午夜
	cases := []struct {
		name       string
		w          Window
		s, e       time.Time
		hit, whole bool
	}{
		{"夜间-晚上落在窗口内", night, lt(2026, 10, 5, 22, 30), lt(2026, 10, 5, 23, 0), true, true},
		{"夜间-次日凌晨落在前一天的窗口内", night, lt(2026, 10, 6, 5, 0), lt(2026, 10, 6, 5, 30), true, true},
		{"夜间-结束边界不算相交", night, lt(2026, 10, 6, 6, 0), lt(2026, 10, 6, 6, 30), false, false},
		{"夜间-开始前一刻不相交", night, lt(2026, 10, 5, 21, 0), lt(2026, 10, 5, 22, 0), false, false},
		{"夜间-部分相交", night, lt(2026, 10, 5, 21, 40), lt(2026, 10, 5, 22, 10), true, false},
		{"夜间-区间跨午夜整体落在窗口内", night, lt(2026, 10, 5, 23, 30), lt(2026, 10, 6, 0, 30), true, true},
		{"夜间-白天不相交", night, lt(2026, 10, 5, 12, 0), lt(2026, 10, 5, 13, 0), false, false},
		{"周五夜间-周六凌晨属于周五的窗口", win("fri-night", WindowAvoid, "22:00", "06:00", 5), lt(2026, 10, 10, 2, 0), lt(2026, 10, 10, 2, 30), true, true},
		{"周五夜间-周五凌晨属于周四，不命中", win("fri-night", WindowAvoid, "22:00", "06:00", 5), lt(2026, 10, 9, 2, 0), lt(2026, 10, 9, 2, 30), false, false},
		{"周五夜间-周六晚上不命中", win("fri-night", WindowAvoid, "22:00", "06:00", 5), lt(2026, 10, 10, 23, 0), lt(2026, 10, 10, 23, 30), false, false},
		{"工作日高峰-周一命中", win("peak", WindowAvoid, "09:00", "12:00", 1, 2, 3, 4, 5), lt(2026, 10, 5, 10, 0), lt(2026, 10, 5, 10, 30), true, true},
		{"工作日高峰-周六不命中", win("peak", WindowAvoid, "09:00", "12:00", 1, 2, 3, 4, 5), lt(2026, 10, 10, 10, 0), lt(2026, 10, 10, 10, 30), false, false},
		{"工作日高峰-周日不命中", win("peak", WindowAvoid, "09:00", "12:00", 1, 2, 3, 4, 5), lt(2026, 10, 11, 10, 0), lt(2026, 10, 11, 10, 30), false, false},
		{"长变更跨越多天仍能命中窗口", win("peak", WindowAvoid, "09:00", "12:00", 3), lt(2026, 10, 5, 8, 0), lt(2026, 10, 8, 8, 0), true, false},
		{"24:00 表示当天结束", win("late", WindowBlock, "20:00", "24:00"), lt(2026, 10, 5, 23, 50), lt(2026, 10, 5, 23, 59), true, true},
		{"24:00 之后不相交", win("late", WindowBlock, "20:00", "24:00"), lt(2026, 10, 6, 0, 0), lt(2026, 10, 6, 0, 10), false, false},
	}
	for _, c := range cases {
		occ := c.w.occurrences(c.s, c.e)
		if (len(occ) > 0) != c.hit {
			t.Errorf("%s: hit=%v 期望 %v", c.name, len(occ) > 0, c.hit)
			continue
		}
		hs := evalWindows([]Window{c.w}, "mid", KindInstOp, c.s, c.e)
		if c.hit && (len(hs) != 1 || hs[0].Full != c.whole) {
			t.Errorf("%s: full=%v 期望 %v", c.name, hs, c.whole)
		}
	}
}

func TestWindowAppliesFilterAndDisabled(t *testing.T) {
	plan := Plan{Kind: KindInstOp, Risk: "low", EstMins: 10}
	start := lt(2026, 10, 5, 10, 0)
	w := win("peak", WindowAvoid, "09:00", "12:00")
	w.Risks = []string{"mid", "high"}
	if hs := evalWindows([]Window{w}, plan.Risk, plan.Kind, start, planEnd(plan, start)); len(hs) != 0 {
		t.Error("低风险不应命中仅适用于 mid/high 的规则")
	}
	w.Risks = nil
	w.Kinds = []string{KindConfig}
	if hs := evalWindows([]Window{w}, plan.Risk, plan.Kind, start, planEnd(plan, start)); len(hs) != 0 {
		t.Error("种类不匹配不应命中")
	}
	w.Kinds = []string{KindInstOp}
	if hs := evalWindows([]Window{w}, plan.Risk, plan.Kind, start, planEnd(plan, start)); len(hs) != 1 {
		t.Error("种类匹配应命中")
	}
	w.Enabled = false
	if hs := evalWindows([]Window{w}, plan.Risk, plan.Kind, start, planEnd(plan, start)); len(hs) != 0 {
		t.Error("停用的规则不应命中")
	}
}

func TestWindowChecksModes(t *testing.T) {
	v := newEnv(t, "")
	block := win("停机窗口", WindowBlock, "09:00", "12:00")
	avoid := win("备份窗口", WindowAvoid, "13:00", "15:00")
	prefer := win("夜间低峰", WindowPrefer, "22:00", "06:00")
	v.e.cfg.Windows = func() []Window { return []Window{block, avoid, prefer} }
	plan := Plan{Kind: KindInstOp, Risk: "mid", EstMins: 20}

	cks, blocked := v.e.WindowChecks(plan, lt(2026, 10, 5, 10, 0), false)
	if !blocked || len(cks) != 1 || cks[0].Lv != "bad" || !strings.Contains(cks[0].T, "停机窗口") {
		t.Errorf("block 命中且非紧急应阻断: %v %v", cks, blocked)
	}
	cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 10, 0), true)
	if blocked || len(cks) != 1 || cks[0].Lv != "warn" || !strings.Contains(cks[0].T, "紧急") {
		t.Errorf("紧急变更命中 block 应只警告: %v %v", cks, blocked)
	}
	cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 14, 0), false)
	if blocked || len(cks) != 1 || cks[0].Lv != "warn" || !strings.Contains(cks[0].T, "备份窗口") {
		t.Errorf("avoid 命中应警告: %v %v", cks, blocked)
	}
	cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 23, 0), false)
	if blocked || len(cks) != 1 || cks[0].Lv != "ok" || !strings.Contains(cks[0].T, "落在推荐窗口") {
		t.Errorf("prefer 命中应提示: %v %v", cks, blocked)
	}
	cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 21, 50), false)
	if len(cks) != 1 || cks[0].Lv != "ok" || !strings.Contains(cks[0].T, "部分落在推荐窗口") {
		t.Errorf("部分重叠应说明是部分落在: %v", cks)
	}
	if cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 16, 0), false); len(cks) != 0 || blocked {
		t.Errorf("未命中任何规则不应有检查项: %v", cks)
	}
	// 同时命中 block 与 avoid：block 优先体现为 blocked，avoid 仍给出警告
	v.e.cfg.Windows = func() []Window { return []Window{block, win("全天提示", WindowAvoid, "00:00", "24:00")} }
	cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 10, 0), false)
	if !blocked || len(cks) != 2 {
		t.Errorf("block + avoid: %v %v", cks, blocked)
	}
	// 没有配置规则：不检查
	v.e.cfg.Windows = nil
	if cks, blocked = v.e.WindowChecks(plan, lt(2026, 10, 5, 10, 0), false); len(cks) != 0 || blocked {
		t.Errorf("Windows 为 nil 不应检查: %v", cks)
	}
}

func TestCreateBlockedByWindow(t *testing.T) {
	v := newEnv(t, "")
	v.clk.t = lt(2026, 10, 5, 10, 0)
	v.e.cfg.Windows = func() []Window {
		return []Window{win("核心交易时段", WindowBlock, "09:00", "12:00", 1, 2, 3, 4, 5), win("备份窗口", WindowAvoid, "13:00", "15:00")}
	}
	p := js(instOpParams{Node: "tikv-2", Action: "restart"})
	// 立即执行：按当前时间（周一 10:00）判断
	_, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "alice"})
	if err == nil || !strings.Contains(err.Error(), "核心交易时段") || !strings.Contains(err.Error(), "10-05 10:00") {
		t.Fatalf("立即执行落在 block 窗口应被拒绝，且文案含规则名与时间: %v", err)
	}
	// 定时：按 ScheduledAt 判断
	in := lt(2026, 10, 5, 11, 0)
	if _, err = v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "alice", ScheduledAt: &in}); err == nil || !strings.Contains(err.Error(), "核心交易时段") {
		t.Fatalf("定时落在 block 窗口应被拒绝: %v", err)
	}
	if len(v.e.List()) != 0 {
		t.Error("被拒绝的变更不应落库")
	}
	// 周六同一时间不在规则的星期内
	sat := lt(2026, 10, 10, 10, 0)
	ch, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "alice", ScheduledAt: &sat})
	if err != nil {
		t.Fatalf("周六不在规则内: %v", err)
	}
	if strings.Contains(strings.Join(ch.Log, "\n"), "窗口检查") {
		t.Errorf("未命中窗口不应有窗口检查日志: %v", ch.Log)
	}
	_, _ = v.e.Cancel(ch.ID, "alice", "释放资源")
	// 紧急变更可以进入 block 窗口，并在日志第一行留痕
	em, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "alice", Emergency: true})
	if err != nil {
		t.Fatalf("紧急变更应放行: %v", err)
	}
	if len(em.Log) < 2 || !strings.Contains(em.Log[0], "窗口检查") || !strings.Contains(em.Log[0], "核心交易时段") || !strings.Contains(em.Log[0], "紧急") {
		t.Errorf("紧急变更应在日志第一行写入窗口警告: %v", em.Log)
	}
	_, _ = v.e.Cancel(em.ID, "alice", "释放资源")
	// avoid：放行，警告写入日志第一行
	at := lt(2026, 10, 5, 14, 0)
	av, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "alice", ScheduledAt: &at})
	if err != nil {
		t.Fatalf("avoid 只警告: %v", err)
	}
	if !strings.Contains(av.Log[0], "备份窗口") || !strings.Contains(av.Log[0], "⚠") {
		t.Errorf("avoid 警告应在日志第一行: %v", av.Log)
	}
}

func TestFreezeStrongerThanWindow(t *testing.T) {
	v := newEnv(t, "")
	v.clk.t = lt(2026, 10, 5, 10, 0)
	v.e.cfg.Windows = func() []Window { return []Window{win("核心交易时段", WindowBlock, "09:00", "12:00")} }
	v.e.cfg.Freezes = func() []Freeze {
		return []Freeze{{From: lt(2026, 10, 5, 0, 0), To: lt(2026, 10, 7, 0, 0), Label: "国庆保障"}}
	}
	p := js(instOpParams{Node: "tikv-2", Action: "restart"})
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "a"}); err == nil || !strings.Contains(err.Error(), "国庆保障") {
		t.Errorf("冻结期应先于窗口规则阻断: %v", err)
	}
	// 紧急变更：冻结期与 block 窗口都放行
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: p, Applicant: "a", Emergency: true}); err != nil {
		t.Errorf("紧急变更应同时越过冻结期与窗口: %v", err)
	}
}

func recEnv(t *testing.T) *env {
	v := newEnv(t, "")
	v.clk.t = time.Date(2026, 10, 5, 10, 7, 13, 0, time.Local) // 周一 10:07:13
	v.e.cfg.Windows = DefaultWindows
	return v
}

func recommend(t *testing.T, v *env, node string, days, count int) (Plan, []Slot) {
	t.Helper()
	plan, slots, err := v.e.RecommendPlan(context.Background(), KindInstOp, js(instOpParams{Node: node, Action: "restart"}), time.Time{}, days, count)
	if err != nil {
		t.Fatal(err)
	}
	return plan, slots
}

func TestRecommendDefaultRules(t *testing.T) {
	v := recEnv(t)
	plan, slots := recommend(t, v, "tikv-2", 7, 5)
	if len(slots) != 5 {
		t.Fatalf("应返回 5 个推荐: %v", slots)
	}
	earliest := v.clk.Now().Add(10 * time.Minute)
	for i, s := range slots {
		if s.Start.Before(earliest) {
			t.Errorf("起点早于 now+10 分钟: %v", s.Start)
		}
		if l := s.Start.Local(); l.Minute()%30 != 0 || l.Second() != 0 || l.Nanosecond() != 0 {
			t.Errorf("起点未对齐 30 分钟: %v", s.Start)
		}
		if got := s.End.Sub(s.Start); got != time.Duration(plan.EstMins)*time.Minute {
			t.Errorf("End 应为 Start+EstMins: %v", s)
		}
		for _, h := range evalWindows(DefaultWindows(), plan.Risk, plan.Kind, s.Start, s.End) {
			if h.W.Mode == WindowAvoid {
				t.Errorf("推荐不应落在 avoid 窗口（%s）: %v", h.W.Name, s.Start)
			}
		}
		if s.Score < 60 || len(s.Reasons) == 0 {
			t.Errorf("前 5 名应是整段落在夜间低峰的时段: %+v", s)
		}
		if i > 0 && slots[i-1].Score < s.Score {
			t.Errorf("应按分数降序: %v", slots)
		}
	}
	if !slots[0].Start.Equal(lt(2026, 10, 5, 22, 0)) || slots[0].Score != 70 {
		t.Errorf("第一名应是周一 22:00（70 分）: %+v", slots[0])
	}
	// 同一天内相隔不足 2 小时的只保留一个
	for i := range slots {
		for j := i + 1; j < len(slots); j++ {
			a, b := slots[i].Start.Local(), slots[j].Start.Local()
			d := a.Sub(b)
			if d < 0 {
				d = -d
			}
			if a.YearDay() == b.YearDay() && d < 2*time.Hour {
				t.Errorf("同日相隔不足 2 小时: %v %v", a, b)
			}
		}
	}
	// 越晚越降分：最后一个的分数低于第一个
	if slots[len(slots)-1].Score >= slots[0].Score {
		t.Errorf("较晚的日期应略微降分: %v", slots)
	}
}

func TestRecommendAvoidsPeakAndPrefersLowTraffic(t *testing.T) {
	v := recEnv(t)
	v.e.cfg.Windows = func() []Window {
		return []Window{win("白天", WindowAvoid, "08:00", "23:00"), win("凌晨", WindowPrefer, "02:00", "05:00")}
	}
	_, slots := recommend(t, v, "tikv-2", 3, 10)
	if len(slots) == 0 {
		t.Fatal("应有推荐")
	}
	if l := slots[0].Start.Local(); l.Hour() < 2 || l.Hour() >= 5 {
		t.Errorf("第一名应在凌晨推荐窗口内: %v", slots[0])
	}
	for _, s := range slots {
		if s.Score < 0 {
			continue
		}
		if h := s.Start.Local().Hour(); h >= 8 && h < 23 {
			t.Errorf("白天（avoid）不应排在非负分里: %+v", s)
		}
	}
}

func TestRecommendExcludesBlockFreezeAndConflicts(t *testing.T) {
	v := recEnv(t)
	v.e.cfg.Windows = func() []Window {
		return append(DefaultWindows(), win("夜间封网", WindowBlock, "22:00", "24:00"))
	}
	// 冻结周二 00:00–05:00
	v.e.cfg.Freezes = func() []Freeze {
		return []Freeze{{From: lt(2026, 10, 6, 0, 0), To: lt(2026, 10, 6, 5, 0), Label: "活动保障"}}
	}
	// 周二 05:00（冻结期刚结束）已有排期（tikv-2）
	at := lt(2026, 10, 6, 5, 0)
	busy, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "restart"}), Applicant: "alice", ScheduledAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	plan, slots := recommend(t, v, "tikv-2", 2, 10)
	if len(slots) == 0 {
		t.Fatal("应有推荐")
	}
	bs, be := at, at.Add(time.Duration(busy.Plan.EstMins)*time.Minute)
	for _, s := range slots {
		l := s.Start.Local()
		if h := l.Hour(); h >= 22 {
			t.Errorf("命中 block 窗口的起点必须排除: %v", s.Start)
		}
		if s.Start.Before(lt(2026, 10, 6, 5, 0)) && s.End.After(lt(2026, 10, 6, 0, 0)) {
			t.Errorf("落在冻结期内: %v", s.Start)
		}
		if s.Start.Before(be) && s.End.After(bs) {
			t.Errorf("与已排期变更在同一实例上时间重叠: %v（排期 %v）", s.Start, at)
		}
	}
	// 同一实例：排期时段被排除，紧随其后的 05:30 仍可推荐；另一台主机不冲突，05:00 仍是推荐
	has := func(ss []Slot, t time.Time) bool {
		for _, s := range ss {
			if s.Start.Equal(t) {
				return true
			}
		}
		return false
	}
	if has(slots, at) || !has(slots, lt(2026, 10, 6, 5, 30)) {
		t.Errorf("tikv-2 应排除 05:00 并推荐 05:30: %v (plan %d 分钟)", slots, plan.EstMins)
	}
	_, other := recommend(t, v, "tikv-3", 2, 10)
	if !has(other, at) {
		t.Errorf("不同实例不应冲突，周二 05:00 应在推荐里: %v", other)
	}
	// 排期不阻断规划本身（即使该实例已被其他变更占用，也能给出之后的时段）
	if _, _, err := v.e.RecommendPlan(context.Background(), KindInstOp, js(instOpParams{Node: "tikv-2", Action: "restart"}), time.Time{}, 7, 5); err != nil {
		t.Errorf("被排期占用的实例也应能推荐其他时段: %v", err)
	}
}

func TestRecommendAllBlockedReturnsEmpty(t *testing.T) {
	v := recEnv(t)
	v.e.cfg.Windows = func() []Window { return []Window{win("全封", WindowBlock, "00:00", "24:00")} }
	_, slots := recommend(t, v, "tikv-2", 7, 5)
	if slots == nil || len(slots) != 0 {
		t.Errorf("全部被封应返回空列表（非 nil）: %#v", slots)
	}
}

func TestRecommendClampsAndFrom(t *testing.T) {
	v := recEnv(t)
	_, slots := recommend(t, v, "tikv-2", 99, 99)
	if len(slots) > 10 {
		t.Errorf("count 最大 10: %d", len(slots))
	}
	for _, s := range slots {
		if s.Start.After(v.clk.Now().Add(15*24*time.Hour)) || s.Start.Before(v.clk.Now()) {
			t.Errorf("超出扫描范围: %v", s.Start)
		}
	}
	// from 在很远的将来：起点从 from 之后开始
	from := lt(2026, 10, 12, 0, 0)
	_, s2, err := v.e.RecommendPlan(context.Background(), KindInstOp, js(instOpParams{Node: "tikv-2", Action: "restart"}), from, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range s2 {
		if s.Start.Before(from) || s.Start.After(from.Add(24*time.Hour)) {
			t.Errorf("应在 from 之后 1 天内: %v", s.Start)
		}
	}
	if _, err := v.e.Recommend(context.Background(), "no.such", nil, time.Time{}, 7, 5); err == nil {
		t.Error("未知种类应报错")
	}
	if sl, err := v.e.Recommend(context.Background(), KindInstOp, js(instOpParams{Node: "tikv-2", Action: "restart"}), time.Time{}, 0, 0); err != nil || len(sl) != 5 {
		t.Errorf("默认 count=5: %v %v", len(sl), err)
	}
}

func TestValidateWindows(t *testing.T) {
	ok, err := ValidateWindows(DefaultWindows())
	if err != nil || len(ok) != len(DefaultWindows()) {
		t.Fatalf("默认规则应通过校验: %v", err)
	}
	// 规范化
	got, err := ValidateWindows([]Window{{Name: " 夜间 ", Days: []int{5, 1, 1, 3}, From: "9:00", To: "6:30", Mode: "avoid", Risks: []string{"mid", "mid"}, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	w := got[0]
	if w.ID != "w1" || w.Name != "夜间" || w.From != "09:00" || w.To != "06:30" || len(w.Days) != 3 || w.Days[0] != 1 || w.Days[2] != 5 || len(w.Risks) != 1 || w.Kinds == nil {
		t.Errorf("规范化结果: %+v", w)
	}
	all7, _ := ValidateWindows([]Window{{Name: "x", Days: []int{0, 1, 2, 3, 4, 5, 6}, From: "01:00", To: "02:00", Mode: "prefer"}})
	if len(all7[0].Days) != 0 {
		t.Errorf("七天全选等价于每天: %v", all7[0].Days)
	}
	if empty, err := ValidateWindows(nil); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("空列表合法且非 nil: %#v %v", empty, err)
	}
	base := Window{Name: "x", From: "01:00", To: "02:00", Mode: "avoid", Enabled: true}
	bad := map[string]func(w *Window){
		"名称为空":     func(w *Window) { w.Name = "  " },
		"名称过长":     func(w *Window) { w.Name = strings.Repeat("长", 41) },
		"备注过长":     func(w *Window) { w.Note = strings.Repeat("a", 201) },
		"mode 非法":  func(w *Window) { w.Mode = "forbid" },
		"开始格式":     func(w *Window) { w.From = "25:00" },
		"开始格式2":    func(w *Window) { w.From = "1:5" },
		"结束格式":     func(w *Window) { w.To = "ab" },
		"开始 24:00": func(w *Window) { w.From = "24:00" },
		"起止相同":     func(w *Window) { w.To = "01:00" },
		"星期越界":     func(w *Window) { w.Days = []int{7} },
		"星期负数":     func(w *Window) { w.Days = []int{-1} },
		"风险非法":     func(w *Window) { w.Risks = []string{"critical"} },
		"种类非法":     func(w *Window) { w.Kinds = []string{"Bad Kind!"} },
		"id 非法":    func(w *Window) { w.ID = "a b" },
	}
	for name, mut := range bad {
		w := base
		mut(&w)
		if _, err := ValidateWindows([]Window{w}); err == nil {
			t.Errorf("%s 应校验失败", name)
		}
	}
	if _, err := ValidateWindows([]Window{{ID: "a", Name: "x", From: "01:00", To: "02:00", Mode: "avoid"}, {ID: "a", Name: "y", From: "01:00", To: "02:00", Mode: "avoid"}}); err == nil {
		t.Error("id 重复应失败")
	}
	many := make([]Window, MaxWindows+1)
	for i := range many {
		many[i] = base
	}
	if _, err := ValidateWindows(many); err == nil {
		t.Error("超过上限应失败")
	}
	if _, err := ValidateWindows(many[:MaxWindows]); err != nil {
		t.Errorf("恰好上限应通过: %v", err)
	}
}

func TestDaysText(t *testing.T) {
	cases := map[string][]int{
		"每天":       nil,
		"周一至周五":    {1, 2, 3, 4, 5},
		"周六、周日":    {0, 6},
		"周一、周三":    {3, 1},
		"周五至周日":    {5, 6, 0},
		"周一至周三、周五": {1, 2, 3, 5},
	}
	for want, days := range cases {
		if got := daysText(days); got != want {
			t.Errorf("daysText(%v)=%q 期望 %q", days, got, want)
		}
	}
}
