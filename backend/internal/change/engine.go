// @author: kevinkelly
// @license: Apache License 2.0
// @file: engine.go
// @time: 2026/10/28

package change

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tidbops/internal/exec"
)

// Freeze 是变更冻结期。
type Freeze struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Label string    `json:"label"`
}

// Config 是引擎配置。
type Config struct {
	Cluster string
	Env     string // test | staging | prod
	Dir     string // 保存 changes.json 的目录；为空则只在内存中
	Runner  exec.Runner
	Key     []byte // 审批令牌的 HMAC 密钥
	Prober  Prober
	// Policy 返回某风险×环境所需的审批级数以及申请人是否需要 MFA。
	Policy func(risk, env string, emergency bool) (levels int, applicantMFA bool)
	// Gate 在创建与执行前调用：写操作被关闭（开关 / READONLY 熔断 / 版本不受支持）时返回错误。
	Gate func(env string) error
	// Base 返回规划上下文（拓扑、PD 阈值、拓扑 YAML 等）；引擎会补上 Live 与 Maints。
	Base func(ctx context.Context) (Ctx, error)
	// Events 接收引擎事件（审计 / 通知），可为 nil。
	Events  func(Event)
	Freezes func() []Freeze
	// Windows 返回生效的变更窗口规则（见 window.go）；为 nil 表示不做窗口检查。
	Windows func() []Window
	Now     func() time.Time
	// ApprovalTTL 是审批有效期，默认 24 小时。
	ApprovalTTL time.Duration
	// WaitScale 缩放等待的超时与间隔（测试用，默认 1）。
	WaitScale float64
	// TickEvery 是调度循环的间隔，默认 2 秒。
	TickEvery time.Duration
	// MaxLog 是单个变更保留的日志行数上限。
	MaxLog int
}

// BlockedError 表示计划存在阻断项。
type BlockedError struct{ Plan Plan }

func (e *BlockedError) Error() string {
	var bad []string
	for _, c := range e.Plan.Checks {
		if c.Lv == "bad" {
			bad = append(bad, c.T)
		}
	}
	return "前置检查未通过：" + strings.Join(bad, "；")
}

// 引擎错误。
var (
	ErrNeedMFA  = errors.New("高风险变更需要申请人二次验证（MFA）")
	ErrNotFound = errors.New("找不到该变更")
)

// CreateInput 是创建变更的输入。
type CreateInput struct {
	Kind        string
	Params      json.RawMessage
	Note        string
	Ticket      string
	Applicant   string
	ScheduledAt *time.Time
	Emergency   bool
	MFAVerified bool
}

// Engine 是变更引擎。
type Engine struct {
	cfg Config

	mu      sync.Mutex
	changes []*Change
	maints  []*Maint
	seq     int
	mseq    int
	dirty   int

	sem     chan struct{} // 全局串行：同一时刻只执行一个变更
	cancels map[string]context.CancelFunc
	confirm map[string]chan struct{}
	noAbort map[string]bool
	aborted map[string]string
	root    context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

type persisted struct {
	Seq     int       `json:"seq"`
	MSeq    int       `json:"mseq"`
	Changes []*Change `json:"changes"`
	Maints  []*Maint  `json:"maints"`
}

// New 创建引擎并恢复已保存的状态；上次进程退出时仍在执行中的变更会被标记为失败（需要人工核对集群状态）。
func New(cfg Config) (*Engine, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ApprovalTTL <= 0 {
		cfg.ApprovalTTL = 24 * time.Hour
	}
	if cfg.WaitScale <= 0 {
		cfg.WaitScale = 1
	}
	if cfg.TickEvery <= 0 {
		cfg.TickEvery = 2 * time.Second
	}
	if cfg.MaxLog <= 0 {
		cfg.MaxLog = 3000
	}
	if cfg.Policy == nil {
		return nil, errors.New("缺少审批策略")
	}
	e := &Engine{cfg: cfg, sem: make(chan struct{}, 1), cancels: map[string]context.CancelFunc{}, confirm: map[string]chan struct{}{}, noAbort: map[string]bool{}, aborted: map[string]string{}}
	e.root, e.stop = context.WithCancel(context.Background())
	if cfg.Dir != "" {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, err
		}
		if b, err := os.ReadFile(filepath.Join(cfg.Dir, "changes.json")); err == nil {
			var p persisted
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, fmt.Errorf("读取变更记录失败: %w", err)
			}
			e.changes, e.maints, e.seq, e.mseq = p.Changes, p.Maints, p.Seq, p.MSeq
		}
	}
	now := cfg.Now()
	for _, c := range e.changes {
		if c.State == StateRunning {
			c.State, c.Reason, c.Finished = StateFailed, "平台在执行过程中重启，变更被中断：请核对集群状态后重新规划", &now
			e.logLine(c, "✖ "+c.Reason)
		}
	}
	for _, m := range e.maints {
		if m.State == "draining" {
			m.State = "cancelled"
			m.Log = append(m.Log, "进入维护的变更被中断（平台重启）")
		}
	}
	e.saveLocked()
	return e, nil
}

// Start 启动调度循环（到点执行、审批过期、维护超时提醒）。
func (e *Engine) Start() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(e.cfg.TickEvery)
		defer t.Stop()
		for {
			select {
			case <-e.root.Done():
				return
			case <-t.C:
				e.Tick()
			}
		}
	}()
}

// Close 停止调度并取消正在执行的变更。
func (e *Engine) Close() {
	e.stop()
	e.wg.Wait()
}

// Wait 等待所有执行中的变更结束（测试用）。
func (e *Engine) Wait() { e.wg.Wait() }

func (e *Engine) emit(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = e.cfg.Now()
	}
	if e.cfg.Events != nil {
		e.cfg.Events(ev)
	}
}

// ---------------------------------------------------------------- 查询 ----

func cloneChange(c *Change) Change {
	cp := *c
	cp.Approvals = append([]Approval(nil), c.Approvals...)
	cp.Steps = append([]StepState(nil), c.Steps...)
	cp.Log = append([]string(nil), c.Log...)
	return cp
}

// Get 返回变更的副本。
func (e *Engine) Get(id string) (Change, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.changes {
		if c.ID == id {
			return cloneChange(c), true
		}
	}
	return Change{}, false
}

// List 按创建时间倒序返回变更（不含日志，减小体积）。
func (e *Engine) List() []Change {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Change, 0, len(e.changes))
	for i := len(e.changes) - 1; i >= 0; i-- {
		cp := cloneChange(e.changes[i])
		if len(cp.Log) > 20 {
			cp.Log = cp.Log[len(cp.Log)-20:]
		}
		out = append(out, cp)
	}
	return out
}

// Maints 返回维护记录（新的在前）。
func (e *Engine) Maints() []Maint {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Maint, 0, len(e.maints))
	for i := len(e.maints) - 1; i >= 0; i-- {
		m := *e.maints[i]
		m.Todo = append([]Todo(nil), m.Todo...)
		m.Log = append([]string(nil), m.Log...)
		out = append(out, m)
	}
	return out
}

// SetTodo 勾选 / 取消维护检查单的一项。
func (e *Engine) SetTodo(id string, idx int, done bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.maints {
		if m.ID == id {
			if idx < 0 || idx >= len(m.Todo) {
				return fmt.Errorf("检查单序号不合法")
			}
			m.Todo[idx].Done = done
			e.saveLocked()
			return nil
		}
	}
	return ErrNotFound
}

func (e *Engine) liveLocked() []*Change {
	var out []*Change
	for _, c := range e.changes {
		if LiveStates[c.State] {
			out = append(out, c)
		}
	}
	return out
}

func (e *Engine) planCtx(ctx context.Context) (Ctx, error) {
	c, err := e.cfg.Base(ctx)
	if err != nil {
		return Ctx{}, err
	}
	c.Cluster, c.Env = e.cfg.Cluster, e.cfg.Env
	if c.Now.IsZero() {
		c.Now = e.cfg.Now()
	}
	e.mu.Lock()
	c.Live = nil
	for _, l := range e.liveLocked() {
		cp := cloneChange(l)
		c.Live = append(c.Live, &cp)
	}
	c.Maints = nil
	for _, m := range e.maints {
		cp := *m
		c.Maints = append(c.Maints, &cp)
	}
	e.mu.Unlock()
	return c, nil
}

// Preview 规划但不落库。Blocked 的计划也会返回（带检查结果）。
func (e *Engine) Preview(ctx context.Context, kind string, params json.RawMessage) (Plan, error) {
	pc, err := e.planCtx(ctx)
	if err != nil {
		return Plan{}, err
	}
	return pc.Plan(kind, params)
}

// ---------------------------------------------------------------- 创建与审批 ----

func (e *Engine) frozen(at time.Time, mins int) *Freeze {
	if e.cfg.Freezes == nil {
		return nil
	}
	end := at.Add(time.Duration(mins) * time.Minute)
	for _, f := range e.cfg.Freezes() {
		if at.Before(f.To) && end.After(f.From) {
			ff := f
			return &ff
		}
	}
	return nil
}

// Create 规划并创建变更。需要审批时处于 pending；无需审批且不定时则立即开始执行。
func (e *Engine) Create(ctx context.Context, in CreateInput) (Change, error) {
	if e.cfg.Gate != nil {
		if err := e.cfg.Gate(e.cfg.Env); err != nil {
			return Change{}, err
		}
	}
	pc, err := e.planCtx(ctx)
	if err != nil {
		return Change{}, err
	}
	plan, err := pc.Plan(in.Kind, in.Params)
	if err != nil {
		return Change{}, err
	}
	if plan.Blocked {
		return Change{}, &BlockedError{Plan: plan}
	}
	now := e.cfg.Now()
	startAt := now
	if in.ScheduledAt != nil {
		startAt = *in.ScheduledAt
		if startAt.Before(now.Add(-time.Minute)) {
			return Change{}, errors.New("计划执行时间已过")
		}
	}
	if f := e.frozen(startAt, plan.EstMins); f != nil && !in.Emergency {
		return Change{}, fmt.Errorf("计划时间落在变更冻结期「%s」内；确需执行请走紧急变更", f.Label)
	}
	// 窗口规则（备份窗口 / 业务高峰 / 推荐低峰）：定时变更按计划时间，立即执行按当前时间；冻结期是更强的绝对阻断，已在上面处理。
	wchecks, wblocked := e.WindowChecks(plan, startAt, in.Emergency)
	if wblocked {
		for _, c := range wchecks {
			if c.Lv == "bad" {
				return Change{}, errors.New("窗口规则拦截：" + c.T)
			}
		}
	}
	levels, mfa := e.cfg.Policy(plan.Risk, e.cfg.Env, in.Emergency)
	if mfa && !in.MFAVerified {
		return Change{}, ErrNeedMFA
	}
	e.mu.Lock()
	if in.ScheduledAt != nil {
		if o := e.timeConflictLocked(plan, startAt); o != "" {
			e.mu.Unlock()
			return Change{}, fmt.Errorf("与已排期的变更 %s 在时间与资源上冲突", o)
		}
	}
	e.seq++
	ch := &Change{ID: fmt.Sprintf("CHG-%04d", e.seq), Kind: in.Kind, Title: plan.Title, Cluster: e.cfg.Cluster, Env: e.cfg.Env,
		Params: in.Params, Note: in.Note, Ticket: in.Ticket, Risk: plan.Risk, Emergency: in.Emergency, Applicant: in.Applicant, Created: now,
		Need: levels, NeedMFA: mfa, State: StatePending, ScheduledAt: in.ScheduledAt, Expires: now.Add(e.cfg.ApprovalTTL), Plan: plan, Approvals: []Approval{}}
	if in.ScheduledAt != nil && startAt.After(ch.Expires) {
		ch.Expires = startAt
	}
	for _, s := range plan.Steps {
		ch.Steps = append(ch.Steps, StepState{Name: s.Name, State: "wait"})
	}
	e.changes = append(e.changes, ch)
	for _, c := range wchecks {
		if c.Lv == "warn" {
			e.logLine(ch, "⚠ 窗口检查："+c.T)
		}
	}
	e.logLine(ch, fmt.Sprintf("变更已创建：%s（风险 %s，需 %d 级审批）", plan.Title, plan.Risk, levels))
	immediate := e.advanceLocked(ch)
	e.saveLocked()
	cp := cloneChange(ch)
	e.mu.Unlock()
	e.emit(Event{ChangeID: cp.ID, Type: "created", User: in.Applicant, Env: cp.Env, Risk: cp.Risk, Detail: cp.Title, Result: "success"})
	if immediate {
		e.start(cp.ID)
	}
	return cp, nil
}

// timeConflictLocked 返回与给定计划在时间与资源上冲突的已排期 / 执行中变更的 ID。
func (e *Engine) timeConflictLocked(p Plan, at time.Time) string {
	res := map[string]bool{}
	for _, t := range p.Targets {
		res["i:"+t] = true
	}
	for _, h := range p.Hosts {
		res["h:"+h] = true
	}
	end := at.Add(time.Duration(p.EstMins) * time.Minute)
	for _, o := range e.liveLocked() {
		if o.State == StatePending && o.ScheduledAt == nil {
			continue
		}
		os, oe := o.Created, o.Created.Add(time.Duration(o.Plan.EstMins)*time.Minute)
		if o.ScheduledAt != nil {
			os = *o.ScheduledAt
			oe = os.Add(time.Duration(o.Plan.EstMins) * time.Minute)
		}
		if !(at.Before(oe) && end.After(os)) {
			continue
		}
		for _, t := range o.Plan.Targets {
			if res["i:"+t] {
				return o.ID
			}
		}
		for _, h := range o.Plan.Hosts {
			if res["h:"+h] {
				return o.ID
			}
		}
	}
	return ""
}

// advanceLocked 在审批数达标时推进状态；返回是否应立即开始执行。
func (e *Engine) advanceLocked(ch *Change) bool {
	if ch.State != StatePending || len(ch.Approvals) < ch.Need {
		return false
	}
	if ch.ScheduledAt != nil && ch.ScheduledAt.After(e.cfg.Now()) {
		ch.State = StateScheduled
		e.logLine(ch, fmt.Sprintf("审批完成，已排期到 %s 执行", ch.ScheduledAt.Local().Format("2006-01-02 15:04")))
		return false
	}
	ch.State = StateApproved
	e.logLine(ch, "审批完成，准备执行")
	return true
}

// Approve 登记一次审批。调用方须已完成角色与 MFA 校验；引擎兜底校验职责分离。
func (e *Engine) Approve(id, user, comment string) (Change, error) {
	e.mu.Lock()
	ch := e.findLocked(id)
	if ch == nil {
		e.mu.Unlock()
		return Change{}, ErrNotFound
	}
	if ch.State != StatePending {
		e.mu.Unlock()
		return Change{}, fmt.Errorf("变更当前状态为 %s，不能审批", ch.State)
	}
	if e.cfg.Now().After(ch.Expires) {
		e.mu.Unlock()
		return Change{}, errors.New("审批已过期")
	}
	if user == ch.Applicant {
		e.mu.Unlock()
		return Change{}, errors.New("申请人不能审批自己的变更")
	}
	for _, a := range ch.Approvals {
		if a.User == user {
			e.mu.Unlock()
			return Change{}, errors.New("同一审批人不能重复审批同一变更")
		}
	}
	ch.Approvals = append(ch.Approvals, Approval{User: user, Level: len(ch.Approvals) + 1, Time: e.cfg.Now(), Comment: comment})
	e.logLine(ch, fmt.Sprintf("%s 完成第 %d/%d 级审批", user, len(ch.Approvals), ch.Need))
	immediate := e.advanceLocked(ch)
	e.saveLocked()
	cp := cloneChange(ch)
	e.mu.Unlock()
	e.emit(Event{ChangeID: id, Type: "approved", User: user, Env: cp.Env, Risk: cp.Risk, Detail: fmt.Sprintf("第 %d/%d 级", len(cp.Approvals), cp.Need), Result: "success"})
	if immediate {
		e.start(id)
	}
	return cp, nil
}

// Reject 驳回待审批的变更。
func (e *Engine) Reject(id, user, reason string) (Change, error) {
	return e.finishEarly(id, user, StateRejected, reason, "rejected", func(ch *Change) error {
		if ch.State != StatePending {
			return fmt.Errorf("变更当前状态为 %s，不能驳回", ch.State)
		}
		if user == ch.Applicant {
			return errors.New("申请人不能驳回自己的变更，请使用取消")
		}
		return nil
	})
}

// Cancel 取消尚未开始执行的变更。
func (e *Engine) Cancel(id, user, reason string) (Change, error) {
	return e.finishEarly(id, user, StateCancelled, reason, "cancelled", func(ch *Change) error {
		if ch.State != StatePending && ch.State != StateApproved && ch.State != StateScheduled {
			return fmt.Errorf("变更当前状态为 %s，不能取消（执行中的请使用中止）", ch.State)
		}
		return nil
	})
}

func (e *Engine) finishEarly(id, user, state, reason, evType string, check func(*Change) error) (Change, error) {
	e.mu.Lock()
	ch := e.findLocked(id)
	if ch == nil {
		e.mu.Unlock()
		return Change{}, ErrNotFound
	}
	if err := check(ch); err != nil {
		e.mu.Unlock()
		return Change{}, err
	}
	now := e.cfg.Now()
	ch.State, ch.Reason, ch.Finished = state, reason, &now
	e.logLine(ch, fmt.Sprintf("%s：%s %s", map[string]string{StateRejected: "已驳回", StateCancelled: "已取消"}[state], user, reason))
	e.saveLocked()
	cp := cloneChange(ch)
	e.mu.Unlock()
	e.emit(Event{ChangeID: id, Type: evType, User: user, Env: cp.Env, Risk: cp.Risk, Detail: reason, Result: "success"})
	return cp, nil
}

// SetPostReview 记录紧急变更的事后复盘。
func (e *Engine) SetPostReview(id, text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.findLocked(id)
	if ch == nil {
		return ErrNotFound
	}
	ch.PostReview = text
	e.saveLocked()
	return nil
}

func (e *Engine) findLocked(id string) *Change {
	for _, c := range e.changes {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------- 调度 ----

// Tick 执行一轮调度：到点执行、审批过期、维护超时。
func (e *Engine) Tick() {
	now := e.cfg.Now()
	var startIDs []string
	var events []Event
	e.mu.Lock()
	for _, ch := range e.changes {
		switch ch.State {
		case StatePending:
			if now.After(ch.Expires) {
				ch.State, ch.Reason, ch.Finished = StateExpired, "审批超时未完成，已作废", &now
				e.logLine(ch, "审批超时，变更已作废")
				events = append(events, Event{ChangeID: ch.ID, Type: "expired", Env: ch.Env, Risk: ch.Risk, Result: "success"})
			}
		case StateScheduled:
			if !ch.ScheduledAt.After(now) {
				ch.State = StateApproved
				e.logLine(ch, "到达计划时间，开始执行")
				startIDs = append(startIDs, ch.ID)
			}
		}
	}
	for _, m := range e.maints {
		if m.State == "active" && m.ActiveAt != nil && !m.Alerted {
			el := int(now.Sub(*m.ActiveAt).Minutes())
			if m.HasTiKV && el >= m.Limit {
				m.Alerted = true
				m.Log = append(m.Log, fmt.Sprintf("[%s] ⚠ 维护已 %d 分钟，超过 PD 阈值 %d 分钟：PD 已开始为该主机上的 Store 补副本", now.Local().Format("15:04:05"), el, m.Limit))
				events = append(events, Event{Type: "maint-overdue", Detail: fmt.Sprintf("主机 %s 维护 %d 分钟，已超过 PD max-store-down-time", m.IP, el), Result: "failed"})
			}
		}
	}
	if len(startIDs) > 0 || len(events) > 0 {
		e.saveLocked()
	}
	e.mu.Unlock()
	for _, ev := range events {
		e.emit(ev)
	}
	for _, id := range startIDs {
		e.start(id)
	}
}

func (e *Engine) start(id string) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.execute(id)
	}()
}

// Abort 中止执行中的变更（仅限可中止的计划，且尚未越过不可逆步骤）。
func (e *Engine) Abort(id, user string) error {
	e.mu.Lock()
	ch := e.findLocked(id)
	if ch == nil {
		e.mu.Unlock()
		return ErrNotFound
	}
	if ch.State != StateRunning {
		e.mu.Unlock()
		return fmt.Errorf("变更当前状态为 %s，只有执行中的变更可以中止", ch.State)
	}
	if !ch.Plan.Abortable {
		e.mu.Unlock()
		return errors.New("该变更不支持中止")
	}
	if e.noAbort[id] {
		e.mu.Unlock()
		return errors.New("已越过不可逆步骤，不能中止；请等待完成或按失败处理")
	}
	cancel := e.cancels[id]
	e.aborted[id] = user
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// ConfirmStep 确认一个需要人工完成的步骤。
func (e *Engine) ConfirmStep(id, user string) error {
	e.mu.Lock()
	ch := e.confirm[id]
	c := e.findLocked(id)
	e.mu.Unlock()
	if ch == nil || c == nil {
		return errors.New("该变更当前没有等待人工确认的步骤")
	}
	select {
	case ch <- struct{}{}:
		e.emit(Event{ChangeID: id, Type: "manual", User: user, Env: c.Env, Risk: c.Risk, Detail: "人工步骤已确认", Result: "success"})
		return nil
	default:
		return errors.New("已确认，请稍候")
	}
}

// ---------------------------------------------------------------- 执行 ----

func (e *Engine) logLine(ch *Change, s string) {
	ch.Log = append(ch.Log, fmt.Sprintf("[%s] %s", e.cfg.Now().Local().Format("15:04:05"), s))
	if len(ch.Log) > e.cfg.MaxLog {
		ch.Log = ch.Log[len(ch.Log)-e.cfg.MaxLog:]
	}
}

func (e *Engine) mlog(m *Maint, s string) {
	m.Log = append(m.Log, fmt.Sprintf("[%s] %s", e.cfg.Now().Local().Format("15:04:05"), s))
}

func (e *Engine) logTo(id, s string) {
	e.mu.Lock()
	if ch := e.findLocked(id); ch != nil {
		e.logLine(ch, s)
		e.dirty++
		if e.dirty >= 25 {
			e.saveLocked()
		}
	}
	e.mu.Unlock()
}

func (e *Engine) setStep(id string, i int, state, detail string) {
	e.mu.Lock()
	if ch := e.findLocked(id); ch != nil && i < len(ch.Steps) {
		now := e.cfg.Now()
		st := &ch.Steps[i]
		st.State = state
		if detail != "" {
			st.Detail = detail
		}
		if state == "run" || state == "manual" {
			if st.Started == nil {
				st.Started = &now
			}
		} else {
			st.Ended = &now
		}
		ch.Cur = i
		e.saveLocked()
	}
	e.mu.Unlock()
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	d = time.Duration(float64(d) * e.cfg.WaitScale)
	if d <= 0 {
		d = time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (e *Engine) execute(id string) {
	// 全局串行
	select {
	case e.sem <- struct{}{}:
	case <-e.root.Done():
		return
	}
	defer func() { <-e.sem }()

	ctx, cancel := context.WithCancel(e.root)
	defer cancel()
	e.mu.Lock()
	ch := e.findLocked(id)
	if ch == nil || ch.State != StateApproved {
		e.mu.Unlock()
		return
	}
	plan := ch.Plan
	env, risk, applicant := ch.Env, ch.Risk, ch.Applicant
	e.cancels[id] = cancel
	delete(e.noAbort, id)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, id)
		delete(e.confirm, id)
		delete(e.noAbort, id)
		delete(e.aborted, id)
		e.mu.Unlock()
	}()

	if e.cfg.Gate != nil {
		if err := e.cfg.Gate(env); err != nil {
			e.fail(id, plan, "执行前检查未通过："+err.Error(), false)
			return
		}
	}
	e.mu.Lock()
	ch = e.findLocked(id)
	now := e.cfg.Now()
	ch.State, ch.Started = StateRunning, &now
	e.logLine(ch, "▶ 开始执行："+plan.Title)
	if plan.Kind == KindMaintEnter && plan.Maint != nil {
		e.mseq++
		mt := MaintTypes[plan.Maint.Type]
		m := &Maint{ID: fmt.Sprintf("M%04d", e.mseq), IP: plan.Maint.IP, Type: plan.Maint.Type, TypeLabel: mt.Label, Mins: plan.Maint.Mins, Reason: plan.Maint.Reason, Ticket: ch.Ticket,
			Strategy: plan.Maint.Strategy, OrigThr: plan.Maint.OrigThr, Limit: plan.Maint.Limit, HasTiKV: plan.Maint.HasTiKV, Nodes: plan.Maint.Nodes, Stores: plan.Maint.TiKVStore,
			State: "draining", Started: now, EnterID: id}
		for _, t := range mt.Todo {
			m.Todo = append(m.Todo, Todo{T: t})
		}
		e.mlog(m, fmt.Sprintf("创建维护计划：%s，计划 %d 分钟，策略 %s", mt.Label, m.Mins, m.Strategy))
		e.maints = append(e.maints, m)
		ch.MaintID = m.ID
	}
	if plan.Kind == KindMaintExit || plan.Kind == KindMaintExtend {
		var p struct {
			MaintID string `json:"maintId"`
		}
		_ = json.Unmarshal(ch.Params, &p)
		ch.MaintID = p.MaintID
		if m := e.maintLocked(p.MaintID); m != nil && plan.Kind == KindMaintExit {
			m.State, m.ExitID = "recovering", id
			e.mlog(m, "开始恢复")
		}
	}
	e.saveLocked()
	e.mu.Unlock()
	e.emit(Event{ChangeID: id, Type: "started", User: applicant, Env: env, Risk: risk, Detail: plan.Title, Result: "success"})

	err := e.runSteps(ctx, id, plan)
	if err == nil {
		e.succeed(id, plan)
		return
	}
	if e.root.Err() != nil {
		// 平台正在停止：不再发起任何命令，只记录中断，重启后需要人工核对集群状态
		e.logTo(id, "✖ 平台停止，执行被中断：请核对集群状态（可能残留驱逐调度等中间状态）")
		if cp, ok := e.finish(id, StateFailed, "平台停止，执行被中断：请核对集群状态后重新规划"); ok {
			e.mu.Lock()
			e.saveLocked()
			e.mu.Unlock()
			e.emit(Event{ChangeID: id, Type: "failed", Env: cp.Env, Risk: cp.Risk, Detail: cp.Reason, Result: "failed"})
		}
		return
	}
	e.mu.Lock()
	by, wasAborted := e.aborted[id]
	e.mu.Unlock()
	if wasAborted && errors.Is(err, context.Canceled) {
		e.abortDone(id, plan, by)
		return
	}
	e.fail(id, plan, err.Error(), true)
}

func (e *Engine) maintLocked(id string) *Maint {
	for _, m := range e.maints {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (e *Engine) runSteps(ctx context.Context, id string, plan Plan) error {
	for i, st := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if st.Irreversible {
			e.mu.Lock()
			e.noAbort[id] = true
			e.mu.Unlock()
		}
		e.setStep(id, i, "run", "")
		e.logTo(id, "▶ "+st.Name)
		e.emit(Event{ChangeID: id, Type: "step", Detail: st.Name})
		if st.Manual != "" {
			if err := e.manual(ctx, id, i, st); err != nil {
				e.setStep(id, i, "failed", err.Error())
				return fmt.Errorf("%s：%w", st.Name, err)
			}
		}
		for _, cmd := range st.Cmds {
			if err := e.runCmd(ctx, id, cmd); err != nil {
				if st.IgnoreErr && ctx.Err() == nil {
					e.logTo(id, "  ⚠ 忽略失败："+err.Error())
					continue
				}
				e.setStep(id, i, "failed", err.Error())
				return fmt.Errorf("%s：%w", st.Name, err)
			}
		}
		if st.Wait != nil {
			if err := e.wait(ctx, id, *st.Wait); err != nil {
				if st.IgnoreErr && ctx.Err() == nil {
					e.logTo(id, "  ⚠ 忽略失败："+err.Error())
				} else {
					e.setStep(id, i, "failed", err.Error())
					return fmt.Errorf("%s：%w", st.Name, err)
				}
			}
		}
		if len(st.Cmds) == 0 && st.Wait == nil && st.Manual == "" {
			e.logTo(id, "  （无需执行集群命令）")
		}
		e.setStep(id, i, "done", "")
	}
	return nil
}

func (e *Engine) manual(ctx context.Context, id string, i int, st Step) error {
	ch := make(chan struct{}, 1)
	e.mu.Lock()
	e.confirm[id] = ch
	e.mu.Unlock()
	e.setStep(id, i, "manual", st.Manual)
	e.logTo(id, "⏸ 需要人工操作："+st.Manual)
	e.emit(Event{ChangeID: id, Type: "manual-wait", Detail: st.Manual})
	defer func() {
		e.mu.Lock()
		delete(e.confirm, id)
		e.mu.Unlock()
	}()
	select {
	case <-ch:
		e.logTo(id, "  人工步骤已确认")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(float64(12*time.Hour) * e.cfg.WaitScale)):
		return errors.New("等待人工确认超时（12 小时）")
	}
}

func (e *Engine) runCmd(ctx context.Context, id string, cmd exec.Cmd) error {
	e.logTo(id, "$ "+Render(cmd))
	req := exec.Request{Cmd: cmd, ChangeID: id}
	e.mu.Lock()
	if ch := e.findLocked(id); ch != nil {
		req.Actor = ch.Applicant
	}
	e.mu.Unlock()
	req.Token = exec.Sign(e.cfg.Key, cmd, id, e.cfg.Now().Add(15*time.Minute))
	e.emit(Event{ChangeID: id, Type: "command", User: req.Actor, Detail: Render(cmd)})
	res, err := e.cfg.Runner.Run(ctx, req, func(line string) { e.logTo(id, "  "+line) })
	if err != nil {
		e.emit(Event{ChangeID: id, Type: "command", Detail: Render(cmd), Result: "failed"})
		return err
	}
	if res.Exit != 0 {
		e.emit(Event{ChangeID: id, Type: "command", Detail: Render(cmd), Result: "failed"})
		msg := res.Err
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.Exit)
		}
		return errors.New(msg)
	}
	return nil
}

func (e *Engine) wait(ctx context.Context, id string, w Wait) error {
	if w.Kind == "sleep" {
		if !e.sleep(ctx, w.Timeout) {
			return ctx.Err()
		}
		return nil
	}
	deadline := time.Now().Add(time.Duration(float64(w.Timeout) * e.cfg.WaitScale))
	iv := w.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	last := ""
	for {
		done, detail, err := e.cfg.Prober.Probe(ctx, w)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			detail = "探测失败：" + err.Error()
		}
		if detail != last {
			e.logTo(id, "  … "+w.Desc+"："+detail)
			last = detail
		}
		if err == nil && done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s：等待超时（%s）", w.Desc, detail)
		}
		if !e.sleep(ctx, iv) {
			return ctx.Err()
		}
	}
}

// rollback 尽力执行恢复命令（忽略单条失败）。
func (e *Engine) rollback(id string, plan Plan, why string) {
	if len(plan.Rollback) == 0 {
		return
	}
	e.logTo(id, "↩ "+why+"，尝试恢复")
	ctx, cancel := context.WithTimeout(e.root, 10*time.Minute)
	defer cancel()
	for i := len(plan.Rollback) - 1; i >= 0; i-- {
		if err := e.runCmd(ctx, id, plan.Rollback[i]); err != nil {
			e.logTo(id, "  ⚠ 恢复命令失败："+err.Error())
		}
	}
}

func (e *Engine) finish(id, state, reason string) (Change, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := e.findLocked(id)
	if ch == nil {
		return Change{}, false
	}
	now := e.cfg.Now()
	ch.State, ch.Reason, ch.Finished = state, reason, &now
	return cloneChange(ch), true
}

func (e *Engine) succeed(id string, plan Plan) {
	e.mu.Lock()
	ch := e.findLocked(id)
	now := e.cfg.Now()
	ch.State, ch.Finished = StateSucceeded, &now
	e.logLine(ch, "✔ 变更完成")
	if m := e.maintLocked(ch.MaintID); m != nil {
		switch plan.Kind {
		case KindMaintEnter:
			m.State, m.ActiveAt = "active", &now
			e.mlog(m, "实例已全部停止，进入维护窗口，开始计时")
		case KindMaintExit:
			m.State, m.Ended = "done", &now
			el := 0
			if m.ActiveAt != nil {
				el = int(now.Sub(*m.ActiveAt).Minutes())
			}
			e.mlog(m, fmt.Sprintf("维护完成：计划 %d 分钟，实际 %d 分钟", m.Mins+m.ExtendedBy, el))
		case KindMaintExtend:
			var p struct {
				Add int `json:"add"`
			}
			_ = json.Unmarshal(ch.Params, &p)
			m.ExtendedBy += p.Add
			if m.HasTiKV && m.Limit < m.Mins+m.ExtendedBy {
				m.Limit = m.Mins + m.ExtendedBy + 15
				m.Strategy = "extend"
			}
			e.mlog(m, fmt.Sprintf("窗口延长 %d 分钟（现计划 %d 分钟）", p.Add, m.Mins+m.ExtendedBy))
		}
	}
	e.saveLocked()
	cp := cloneChange(ch)
	e.mu.Unlock()
	e.emit(Event{ChangeID: id, Type: "succeeded", Env: cp.Env, Risk: cp.Risk, Detail: cp.Title, Result: "success"})
}

func (e *Engine) fail(id string, plan Plan, reason string, doRollback bool) {
	e.logTo(id, "✖ 失败："+reason)
	if doRollback {
		e.rollback(id, plan, "变更失败")
	}
	cp, ok := e.finish(id, StateFailed, reason)
	if !ok {
		return
	}
	e.mu.Lock()
	if m := e.maintLocked(cp.MaintID); m != nil {
		switch plan.Kind {
		case KindMaintEnter:
			m.State = "cancelled"
			e.mlog(m, "进入维护失败，已尝试恢复")
		case KindMaintExit:
			m.State = "active"
			e.mlog(m, "结束维护失败，仍处于维护中，请处理后重试")
		}
	}
	e.saveLocked()
	e.mu.Unlock()
	e.emit(Event{ChangeID: id, Type: "failed", Env: cp.Env, Risk: cp.Risk, Detail: reason, Result: "failed"})
}

func (e *Engine) abortDone(id string, plan Plan, by string) {
	e.logTo(id, "■ 已中止（"+by+"）")
	e.rollback(id, plan, "变更被中止")
	cp, ok := e.finish(id, StateAborted, "被 "+by+" 中止")
	if !ok {
		return
	}
	e.mu.Lock()
	if m := e.maintLocked(cp.MaintID); m != nil && plan.Kind == KindMaintEnter {
		m.State = "cancelled"
		e.mlog(m, "进入维护被中止，已恢复调度与阈值")
	}
	e.saveLocked()
	e.mu.Unlock()
	e.emit(Event{ChangeID: id, Type: "aborted", User: by, Env: cp.Env, Risk: cp.Risk, Result: "success"})
}

// ---------------------------------------------------------------- 持久化 ----

func (e *Engine) saveLocked() {
	e.dirty = 0
	if e.cfg.Dir == "" {
		return
	}
	b, err := json.Marshal(persisted{Seq: e.seq, MSeq: e.mseq, Changes: e.changes, Maints: e.maints})
	if err != nil {
		return
	}
	path := filepath.Join(e.cfg.Dir, "changes.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// Busy 返回仍占用资源的工作数量：待审批 / 已批准排队 / 定时 / 执行中的变更，加上进行中的主机维护（draining / active / recovering）。
// 多集群里删除 / 改名集群前用它判断是否「在用」。
func (e *Engine) Busy() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.changes {
		if LiveStates[c.State] {
			n++
		}
	}
	for _, m := range e.maints {
		switch m.State {
		case "draining", "active", "recovering":
			n++
		}
	}
	return n
}

// Summary 返回状态计数（给角标用）。
func (e *Engine) Summary() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := map[string]int{}
	for _, c := range e.changes {
		m[c.State]++
	}
	return m
}

// Sorted 按创建时间返回所有变更 ID（测试辅助）。
func (e *Engine) sortedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	for _, c := range e.changes {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return ids
}
