// @author: kevinkelly
// @license: Apache License 2.0
// @file: window.go
// @time: 2026/10/28

package change

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 窗口模式。
const (
	WindowBlock  = "block"  // 禁止在此窗口执行（紧急变更除外）
	WindowAvoid  = "avoid"  // 不建议：给出警告，推荐时降分
	WindowPrefer = "prefer" // 推荐窗口：推荐时加分
)

// MaxWindows 是窗口规则条数上限。
const MaxWindows = 50

// Window 是一条变更窗口规则。时间一律按服务器本地时区解释。
//
// Days 是星期（0=周日 … 6=周六），为空表示每天；跨午夜的规则（From > To，如 22:00–06:00）
// 按「起点所在日」判断星期，即周一 22:00–周二 06:00 属于周一的规则。
// Risks / Kinds 为空表示适用于全部风险 / 全部变更种类。
type Window struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Days    []int    `json:"days"`
	From    string   `json:"from"` // HH:MM
	To      string   `json:"to"`   // HH:MM，可小于 From（跨午夜）；允许 24:00
	Mode    string   `json:"mode"` // block | avoid | prefer
	Risks   []string `json:"risks"`
	Kinds   []string `json:"kinds"`
	Enabled bool     `json:"enabled"`
	Note    string   `json:"note,omitempty"`
}

// DefaultWindows 返回默认规则（settings.json 里没有 windows 字段时使用）。
func DefaultWindows() []Window {
	return []Window{
		{ID: "backup", Name: "备份窗口", Days: []int{}, From: "01:30", To: "04:00", Mode: WindowAvoid, Risks: []string{"mid", "high"}, Kinds: []string{}, Enabled: true, Note: "每日全量备份占用磁盘与网络 IO，避免与变更叠加"},
		{ID: "peak-am", Name: "业务高峰", Days: []int{1, 2, 3, 4, 5}, From: "09:00", To: "12:00", Mode: WindowAvoid, Risks: []string{"mid", "high"}, Kinds: []string{}, Enabled: true, Note: "工作日上午业务高峰"},
		{ID: "peak-pm", Name: "业务高峰", Days: []int{1, 2, 3, 4, 5}, From: "14:00", To: "18:00", Mode: WindowAvoid, Risks: []string{"mid", "high"}, Kinds: []string{}, Enabled: true, Note: "工作日下午业务高峰"},
		{ID: "night", Name: "夜间低峰", Days: []int{}, From: "22:00", To: "06:00", Mode: WindowPrefer, Risks: []string{}, Kinds: []string{}, Enabled: true, Note: "流量最低的时段，优先安排变更"},
	}
}

var (
	hmRe   = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
	kindRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,39}$`)
	idRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
)

// parseHM 把 "HH:MM" 解析成当天的分钟数；"24:00" 表示当天结束（仅用于 To）。
func parseHM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "24:00" {
		return 1440, true
	}
	m := hmRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	var h, mi int
	fmt.Sscanf(m[1], "%d", &h)
	fmt.Sscanf(m[2], "%d", &mi)
	return h*60 + mi, true
}

func fmtHM(s string) string {
	n, ok := parseHM(s)
	if !ok {
		return s
	}
	if n == 1440 {
		return "24:00"
	}
	return fmt.Sprintf("%02d:%02d", n/60, n%60)
}

// ValidateWindows 校验并规范化规则列表（补全 ID、排序星期、去重、统一时间格式）。返回副本。
func ValidateWindows(in []Window) ([]Window, error) {
	if len(in) > MaxWindows {
		return nil, fmt.Errorf("窗口规则最多 %d 条", MaxWindows)
	}
	out := make([]Window, 0, len(in))
	ids := map[string]bool{}
	for i, w := range in {
		pos := fmt.Sprintf("第 %d 条规则", i+1)
		w.Name = strings.TrimSpace(w.Name)
		if w.Name == "" {
			return nil, fmt.Errorf("%s：名称不能为空", pos)
		}
		if utf8.RuneCountInString(w.Name) > 40 {
			return nil, fmt.Errorf("%s：名称不能超过 40 个字符", pos)
		}
		w.Note = strings.TrimSpace(w.Note)
		if utf8.RuneCountInString(w.Note) > 200 {
			return nil, fmt.Errorf("%s：备注不能超过 200 个字符", pos)
		}
		switch w.Mode {
		case WindowBlock, WindowAvoid, WindowPrefer:
		default:
			return nil, fmt.Errorf("%s：mode 只能是 block / avoid / prefer", pos)
		}
		f, ok1 := parseHM(w.From)
		t, ok2 := parseHM(w.To)
		if !ok1 || f == 1440 {
			return nil, fmt.Errorf("%s：开始时间需为 HH:MM（00:00–23:59）", pos)
		}
		if !ok2 {
			return nil, fmt.Errorf("%s：结束时间需为 HH:MM（00:00–24:00）", pos)
		}
		if f == t {
			return nil, fmt.Errorf("%s：开始与结束时间不能相同（全天请用 00:00–24:00）", pos)
		}
		w.From, w.To = fmtHM(w.From), fmtHM(w.To)
		seenDay := map[int]bool{}
		days := []int{}
		for _, d := range w.Days {
			if d < 0 || d > 6 {
				return nil, fmt.Errorf("%s：星期需为 0（周日）到 6（周六）", pos)
			}
			if !seenDay[d] {
				seenDay[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		if len(days) == 7 {
			days = []int{} // 七天全选等价于每天
		}
		w.Days = days
		risks := []string{}
		seen := map[string]bool{}
		for _, r := range w.Risks {
			if r != "low" && r != "mid" && r != "high" {
				return nil, fmt.Errorf("%s：风险只能是 low / mid / high", pos)
			}
			if !seen[r] {
				seen[r] = true
				risks = append(risks, r)
			}
		}
		w.Risks = risks
		kinds := []string{}
		seen = map[string]bool{}
		for _, k := range w.Kinds {
			if !kindRe.MatchString(k) {
				return nil, fmt.Errorf("%s：变更种类 %q 不合法", pos, k)
			}
			if !seen[k] {
				seen[k] = true
				kinds = append(kinds, k)
			}
		}
		w.Kinds = kinds
		w.ID = strings.TrimSpace(w.ID)
		if w.ID == "" {
			for n := i + 1; ; n++ {
				if id := fmt.Sprintf("w%d", n); !ids[id] {
					w.ID = id
					break
				}
			}
		}
		if !idRe.MatchString(w.ID) {
			return nil, fmt.Errorf("%s：id 只能包含字母、数字、. _ -", pos)
		}
		if ids[w.ID] {
			return nil, fmt.Errorf("%s：id %q 重复", pos, w.ID)
		}
		ids[w.ID] = true
		out = append(out, w)
	}
	return out, nil
}

func (w Window) applies(risk, kind string) bool {
	if len(w.Risks) > 0 && !contains(w.Risks, risk) {
		return false
	}
	if len(w.Kinds) > 0 && !contains(w.Kinds, kind) {
		return false
	}
	return true
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

var weekNames = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// daysText 把星期集合写成「每天」「周一至周五」「周日、周六」。
func daysText(days []int) string {
	if len(days) == 0 || len(days) == 7 {
		return "每天"
	}
	// 以周一为一周的开始来识别连续区间
	order := func(d int) int { return (d + 6) % 7 }
	ds := append([]int(nil), days...)
	sort.Slice(ds, func(i, j int) bool { return order(ds[i]) < order(ds[j]) })
	var parts []string
	for i := 0; i < len(ds); {
		j := i
		for j+1 < len(ds) && order(ds[j+1]) == order(ds[j])+1 {
			j++
		}
		if j-i >= 2 {
			parts = append(parts, weekNames[ds[i]]+"至"+weekNames[ds[j]])
		} else {
			for k := i; k <= j; k++ {
				parts = append(parts, weekNames[ds[k]])
			}
		}
		i = j + 1
	}
	return strings.Join(parts, "、")
}

func (w Window) spec() string {
	return fmt.Sprintf("%s %s–%s", daysText(w.Days), w.From, w.To)
}

type span struct{ Start, End time.Time }

// occurrences 返回与 [s,e) 相交的规则时段（按本地时区展开；跨午夜的时段归属起点所在日的星期）。
func (w Window) occurrences(s, e time.Time) []span {
	f, ok1 := parseHM(w.From)
	t, ok2 := parseHM(w.To)
	if !ok1 || !ok2 || f == t || f == 1440 {
		return nil
	}
	ls, le := s.Local(), e.Local()
	y, m, d := ls.Date()
	loc := ls.Location()
	extra := 0
	if t < f {
		extra = 1
	}
	var out []span
	for i := -1; i < 400; i++ {
		day := time.Date(y, m, d+i, 0, 0, 0, 0, loc)
		if day.After(le) {
			break
		}
		if len(w.Days) > 0 && !contains(w.Days, int(day.Weekday())) {
			continue
		}
		ws := time.Date(y, m, d+i, f/60, f%60, 0, 0, loc)
		we := time.Date(y, m, d+i+extra, t/60, t%60, 0, 0, loc)
		if ws.Before(e) && we.After(s) {
			out = append(out, span{ws, we})
		}
	}
	return out
}

// windowHit 是一条规则对某个时间区间的命中。
type windowHit struct {
	W    Window
	Span span // 命中的（首个，或完整包含该区间的）规则时段
	Full bool // 整个区间都落在规则时段内
}

func evalWindows(ws []Window, risk, kind string, s, e time.Time) []windowHit {
	var hits []windowHit
	for _, w := range ws {
		if !w.Enabled || !w.applies(risk, kind) {
			continue
		}
		occ := w.occurrences(s, e)
		if len(occ) == 0 {
			continue
		}
		h := windowHit{W: w, Span: occ[0]}
		for _, o := range occ {
			if !o.Start.After(s) && !o.End.Before(e) {
				h.Full, h.Span = true, o
				break
			}
		}
		hits = append(hits, h)
	}
	return hits
}

func planEnd(plan Plan, start time.Time) time.Time {
	m := plan.EstMins
	if m < 1 {
		m = 1
	}
	return start.Add(time.Duration(m) * time.Minute)
}

func (e *Engine) windows() []Window {
	if e.cfg.Windows == nil {
		return nil
	}
	return e.cfg.Windows()
}

// windowChecksWith 是 WindowChecks 的实现，规则由调用方提供（避免在持有引擎锁时回调外部函数）。
func windowChecksWith(ws []Window, plan Plan, start time.Time, emergency bool) ([]Check, bool) {
	var checks []Check
	blocked := false
	end := planEnd(plan, start)
	when := fmt.Sprintf("计划 %s 起约 %d 分钟", start.Local().Format("01-02 15:04"), plan.EstMins)
	for _, h := range evalWindows(ws, plan.Risk, plan.Kind, start, end) {
		note := ""
		if h.W.Note != "" {
			note = "（" + h.W.Note + "）"
		}
		switch h.W.Mode {
		case WindowBlock:
			if emergency {
				checks = append(checks, Check{"warn", fmt.Sprintf("落在禁止变更窗口「%s」（%s）：%s，按紧急变更处理，需留痕并事后复盘%s", h.W.Name, h.W.spec(), when, note)})
			} else {
				blocked = true
				checks = append(checks, Check{"bad", fmt.Sprintf("落在禁止变更窗口「%s」（%s）：%s，非紧急变更不允许；请改期，确需执行请走紧急变更%s", h.W.Name, h.W.spec(), when, note)})
			}
		case WindowAvoid:
			checks = append(checks, Check{"warn", fmt.Sprintf("落在不建议变更的窗口「%s」（%s）：%s，建议改到其他时段%s", h.W.Name, h.W.spec(), when, note)})
		case WindowPrefer:
			if h.Full {
				checks = append(checks, Check{"ok", fmt.Sprintf("落在推荐窗口「%s」（%s）%s", h.W.Name, h.W.spec(), note)})
			} else {
				checks = append(checks, Check{"ok", fmt.Sprintf("部分落在推荐窗口「%s」（%s）%s", h.W.Name, h.W.spec(), note)})
			}
		}
	}
	return checks, blocked
}

// WindowChecks 对 [start, start+EstMins] 与所有启用的窗口规则做相交判断。
// block 命中且非紧急 → bad 并 blocked=true；紧急 → warn；avoid 命中 → warn；prefer 命中 → ok。
func (e *Engine) WindowChecks(plan Plan, start time.Time, emergency bool) (checks []Check, blocked bool) {
	return windowChecksWith(e.windows(), plan, start, emergency)
}

// ---------------------------------------------------------------- 推荐窗口 ----

// Slot 是一个推荐的执行时段。
type Slot struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Score   int       `json:"score"`
	Reasons []string  `json:"reasons"`
}

// ceil30 把时间向上对齐到本地时钟的整 30 分钟。
func ceil30(t time.Time) time.Time {
	l := t.Local()
	b := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, l.Location())
	for b.Before(t) {
		b = b.Add(30 * time.Minute)
	}
	return b
}

// Recommend 返回按分数降序的推荐执行时段，见 RecommendPlan。
func (e *Engine) Recommend(ctx context.Context, kind string, params json.RawMessage, from time.Time, days, count int) ([]Slot, error) {
	_, slots, err := e.RecommendPlan(ctx, kind, params, from, days, count)
	return slots, err
}

// RecommendPlan 规划变更并在 [from, from+days] 内以 30 分钟步长扫描候选起点：
//   - 起点不早于 now+10 分钟，并对齐到整 30 分钟；
//   - 排除落在冻结期、命中 block 规则、与已排期 / 执行中变更在资源与时间上冲突的起点；
//   - 打分：prefer 命中 +50（整个窗口都在 prefer 内再 +20）、每个 avoid 命中 −30、每晚一天 −2；
//   - 同一天内相隔不足 2 小时的只保留分高的一个。
//
// 规划时不把「其他排期变更占用同一实例」当作阻断（那只对当前时刻有意义），时间冲突由扫描过程按时段判断。
func (e *Engine) RecommendPlan(ctx context.Context, kind string, params json.RawMessage, from time.Time, days, count int) (Plan, []Slot, error) {
	if days <= 0 {
		days = 7
	}
	if days > 14 {
		days = 14
	}
	if count <= 0 {
		count = 5
	}
	if count > 10 {
		count = 10
	}
	pc, err := e.planCtx(ctx)
	if err != nil {
		return Plan{}, nil, err
	}
	pc.Live = nil
	plan, err := pc.Plan(kind, params)
	if err != nil {
		return Plan{}, nil, err
	}
	if plan.Blocked {
		return plan, nil, &BlockedError{Plan: plan}
	}
	now := e.cfg.Now()
	first := ceil30(now.Add(10 * time.Minute))
	if from.IsZero() || from.Before(first) {
		from = first
	} else {
		from = ceil30(from)
	}
	until := from.Add(time.Duration(days) * 24 * time.Hour)
	ws := e.windows()

	type cand struct {
		start time.Time
		score int
		why   []string
	}
	var cands []cand
	for st := from; !st.After(until); st = st.Add(30 * time.Minute) {
		if e.frozen(st, plan.EstMins) != nil {
			continue
		}
		end := planEnd(plan, st)
		score, why, ok := 0, []string{}, true
		preferred, full := "", false
		for _, h := range evalWindows(ws, plan.Risk, plan.Kind, st, end) {
			switch h.W.Mode {
			case WindowBlock:
				ok = false
			case WindowAvoid:
				score -= 30
				why = append(why, fmt.Sprintf("与「%s」（%s）重叠，降分", h.W.Name, h.W.spec()))
			case WindowPrefer:
				if preferred == "" {
					preferred = fmt.Sprintf("「%s」（%s）", h.W.Name, h.W.spec())
				}
				full = full || h.Full
			}
		}
		if !ok {
			continue
		}
		if preferred != "" {
			score += 50
			if full {
				score += 20
				why = append([]string{"整个变更落在推荐窗口" + preferred + "内"}, why...)
			} else {
				why = append([]string{"部分落在推荐窗口" + preferred}, why...)
			}
		}
		if d := int(st.Sub(from) / (24 * time.Hour)); d > 0 {
			score -= 2 * d
		}
		if len(why) == 0 {
			why = append(why, "未命中窗口规则，可以执行")
		}
		cands = append(cands, cand{st, score, why})
	}
	// 与已排期 / 执行中的变更在时间与资源上冲突的起点
	e.mu.Lock()
	kept := cands[:0]
	for _, c := range cands {
		if e.timeConflictLocked(plan, c.start) == "" {
			kept = append(kept, c)
		}
	}
	e.mu.Unlock()
	cands = kept

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].start.Before(cands[j].start)
	})
	slots := []Slot{}
	for _, c := range cands {
		if len(slots) >= count {
			break
		}
		near := false
		cy, cm, cd := c.start.Local().Date()
		for _, s := range slots {
			sy, sm, sd := s.Start.Local().Date()
			diff := c.start.Sub(s.Start)
			if diff < 0 {
				diff = -diff
			}
			if sy == cy && sm == cm && sd == cd && diff < 2*time.Hour {
				near = true
				break
			}
		}
		if near {
			continue
		}
		slots = append(slots, Slot{Start: c.start, End: planEnd(plan, c.start), Score: c.score, Reasons: c.why})
	}
	return plan, slots, nil
}
