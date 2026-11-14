// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy_api.go
// @time: 2026/10/28

package server

// 双人复核框架与可配置审批策略（A3 §2.4）。
//
//	GET  /api/ops/proposals              待确认提案 + 最近 20 条历史（登录即可；管理员看全部，其他人只看自己发起的）
//	POST /api/ops/proposals/{id}/confirm 确认提案（grant-admin 需 users.manage；approval-policy 需 settings.edit）
//	POST /api/ops/proposals/{id}/reject  拒绝提案；发起人本人调用 = 撤回（cancelled）
//	GET  /api/ops/policy                 生效的审批策略 / 默认值 / 硬性下限 / 待确认的策略提案（登录即可）
//	POST /api/ops/policy                 发起「修改审批策略」提案（settings.edit），HTTP 202，需另一名管理员确认
//
// 提案状态机：pending → applied | rejected | expired | cancelled，终态不可再变。
//
//	applied   另一名管理员确认并已生效
//	rejected  另一名管理员拒绝；或确认时发现提案已过时 / 无法应用（Note 说明原因）
//	cancelled 发起人撤回，或被同一对象的新提案取代
//	expired   超过 1 小时无人确认（惰性判定：每次访问提案接口、确认、拒绝、发起时检查）
//
// 开启 / 关闭生产写操作开关的双人确认仍是 ops.go / ops_api.go 里原有的实现，不在此框架内。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
)

// 提案种类与状态。
const (
	PropGrantAdmin     = "grant-admin"
	PropApprovalPolicy = "approval-policy"

	PropPending   = "pending"
	PropApplied   = "applied"
	PropRejected  = "rejected"
	PropExpired   = "expired"
	PropCancelled = "cancelled"
)

const (
	proposalTTL     = time.Hour // 提案有效期
	proposalKeep    = 200       // 落盘保留的条数（pending 的永不裁剪）
	proposalHistory = 20        // 列表里返回的历史条数
)

// Proposal 是一项等待另一名管理员确认的高风险操作。Payload 是该种类的参数（JSON），绝不含口令。
type Proposal struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Summary     string          `json:"summary"`
	Payload     json.RawMessage `json:"payload"`
	Target      string          `json:"target"` // 提案作用的对象：用户名 / "approval-policy"；同一对象的新提案会取代旧的
	By          string          `json:"by"`
	At          time.Time       `json:"at"`
	Expires     time.Time       `json:"expires"`
	State       string          `json:"state"`
	ConfirmedBy string          `json:"confirmedBy,omitempty"` // 确认（applied）或拒绝（rejected）的管理员；撤回时为发起人
	Note        string          `json:"note,omitempty"`
}

// proposalView 是返回给前端的提案：带上「当前用户能否确认」。
type proposalView struct {
	Proposal
	CanConfirm bool `json:"canConfirm"`
	CanCancel  bool `json:"canCancel"`
}

type proposalStore struct {
	mu    sync.Mutex
	path  string // 空 = 仅内存
	seq   int
	items []*Proposal // 按创建顺序
}

type proposalFile struct {
	Version int         `json:"version"`
	Items   []*Proposal `json:"items"`
}

func (ps *proposalStore) find(id string) *Proposal {
	for _, p := range ps.items {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// saveLocked 原子写入 proposals.json（0600）；先裁剪到最近 proposalKeep 条（pending 不裁剪）。
func (ps *proposalStore) saveLocked() error {
	if n := len(ps.items) - proposalKeep; n > 0 {
		kept := make([]*Proposal, 0, len(ps.items))
		for _, p := range ps.items {
			if n > 0 && p.State != PropPending {
				n--
				continue
			}
			kept = append(kept, p)
		}
		ps.items = kept
	}
	if ps.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(proposalFile{Version: 1, Items: ps.items}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(ps.path), ".proposals-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, ps.path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// initPolicy 在 EnableOps 里调用：校验落盘的审批策略（不合法则退回默认策略并记审计）、加载提案文件。
func (s *Server) initPolicy(o *ops) {
	if t := o.settings.ApprovalPolicy; t != nil {
		if err := t.Validate(); err != nil {
			s.cfg.Logf("settings.json 里的审批策略不合法，已退回默认策略：%v", err)
			o.settings.ApprovalPolicy = nil
			_, _ = o.cfg.Audit.Append(audit.Entry{User: "system", Action: "policy.load", Env: o.cfg.Env, Cluster: s.cfg.Col.Params.Cluster,
				Target: "approval-policy", Risk: "high", Summary: "落盘的审批策略不合法，已退回默认策略：" + err.Error(), Result: "failed"})
		}
	}
	ps := &proposalStore{}
	if o.cfg.Dir != "" {
		ps.path = filepath.Join(o.cfg.Dir, "proposals.json")
		if b, err := os.ReadFile(ps.path); err == nil {
			var f proposalFile
			if err := json.Unmarshal(b, &f); err != nil {
				s.cfg.Logf("proposals.json 无法解析，双人复核提案已清空：%v", err)
			} else {
				ps.items = f.Items
			}
		}
	}
	for _, p := range ps.items {
		if n, err := strconv.Atoi(strings.TrimPrefix(p.ID, "PRP-")); err == nil && n > ps.seq {
			ps.seq = n
		}
	}
	o.props = ps
}

// approvalPolicy 是引擎与预览共用的审批判定：读当前生效的策略表（线程安全），再套用 NoMFA（仅测试 / POC）。
func (o *ops) approvalPolicy(risk, env string, emergency bool) (int, bool) {
	src := o
	if o.cfg.policyOwner != nil {
		src = o.cfg.policyOwner // 多集群：审批策略是全局的，从属实例读主实例生效的那一张
	}
	src.mu.Lock()
	t := src.settings.ApprovalPolicy // 指针只会被整体替换，从不原地修改，所以拿到后可在锁外使用
	src.mu.Unlock()
	var levels int
	var mfa bool
	if t == nil {
		levels, mfa = authz.ApprovalPolicy(risk, env, emergency)
	} else {
		levels, mfa = t.Lookup(risk, env, emergency)
	}
	if o.cfg.NoMFA {
		mfa = false
	}
	return levels, mfa
}

// policyTable 返回当前生效的策略表（副本）与是否在用默认值。
func (o *ops) policyTable() (authz.PolicyTable, bool) {
	o.mu.Lock()
	t := o.settings.ApprovalPolicy
	o.mu.Unlock()
	if t == nil {
		return authz.DefaultPolicy(), true
	}
	return t.Clone(), false
}

func writeJSONCode(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ---------------------------------------------------------------- 提案生命周期 ----

// propAudit 写一条提案审计。r 为 nil 表示系统动作（如过期）。Summary 只含类型与摘要，不含口令。
func (s *Server) propAudit(r *http.Request, u *authz.User, action string, p Proposal, result, extra string) {
	o := s.ops
	sum := fmt.Sprintf("[%s] %s", p.Kind, p.Summary)
	if extra != "" {
		sum += "；" + extra
	}
	if r != nil && u != nil {
		s.auditReq(r, *u, action, p.ID, "high", sum, result)
		return
	}
	_, _ = o.cfg.Audit.Append(audit.Entry{User: "system", Env: o.cfg.Env, Cluster: s.cfg.Col.Params.Cluster, Action: action, Target: p.ID, Risk: "high", Summary: sum, Result: result})
}

// expireLocked 把已超时的 pending 提案置为 expired 并写审计。调用方持有 ps.mu。
func (s *Server) expireLocked(o *ops) {
	now := o.now()
	changed := false
	for _, p := range o.props.items {
		if p.State == PropPending && !now.Before(p.Expires) {
			p.State = PropExpired
			p.Note = "超过 1 小时无人确认，已作废"
			changed = true
			s.propAudit(nil, nil, "proposal.expire", *p, "success", "发起人 "+p.By)
		}
	}
	if changed {
		_ = o.props.saveLocked()
	}
}

// capForKind 是确认 / 拒绝某类提案所需的能力。
func capForKind(kind string) authz.Cap {
	if kind == PropApprovalPolicy {
		return authz.CapSettings
	}
	return authz.CapUsers
}

// newProposal 创建提案：先取消同一对象上仍 pending 的旧提案（标记 cancelled），再追加新提案并落盘。
func (s *Server) newProposal(r *http.Request, ru reqUser, kind, target, summary string, payload any) (Proposal, error) {
	o := s.ops
	raw, err := json.Marshal(payload)
	if err != nil {
		return Proposal{}, err
	}
	ps := o.props
	ps.mu.Lock()
	defer ps.mu.Unlock()
	s.expireLocked(o)
	now := o.now()
	for _, old := range ps.items {
		if old.State == PropPending && old.Kind == kind && old.Target == target {
			old.State = PropCancelled
			old.Note = "被 " + ru.U.Name + " 发起的新提案取代"
			s.propAudit(r, &ru.U, "proposal.cancel", *old, "success", old.Note)
		}
	}
	ps.seq++
	p := &Proposal{ID: fmt.Sprintf("PRP-%04d", ps.seq), Kind: kind, Summary: summary, Payload: raw, Target: target,
		By: ru.U.Name, At: now, Expires: now.Add(proposalTTL), State: PropPending}
	ps.items = append(ps.items, p)
	if err := ps.saveLocked(); err != nil {
		// 落盘失败：撤销这条提案，避免内存与磁盘不一致
		ps.items = ps.items[:len(ps.items)-1]
		ps.seq--
		return Proposal{}, fmt.Errorf("提案保存失败：%w", err)
	}
	s.propAudit(r, &ru.U, "proposal.create", *p, "success", "等待另一名管理员确认")
	return *p, nil
}

func isAdminFor(o *ops, u authz.User) bool {
	return u.Can(o.cfg.Env, authz.CapUsers) || u.Can(o.cfg.Env, authz.CapSettings)
}

func (s *Server) viewOf(o *ops, u authz.User, p Proposal) proposalView {
	self := strings.EqualFold(p.By, u.Name)
	return proposalView{Proposal: p,
		CanConfirm: p.State == PropPending && !self && u.Can(o.cfg.Env, capForKind(p.Kind)),
		CanCancel:  p.State == PropPending && (self || u.Can(o.cfg.Env, capForKind(p.Kind)))}
}

// ---------------------------------------------------------------- 应用提案 ----

type grantAdminPayload struct {
	User  string                  `json:"user"`
	Roles map[string][]authz.Role `json:"roles"` // 确认后的完整角色集合（含 admin）
	DBs   []string                `json:"dbs,omitempty"`
	Base  map[string][]authz.Role `json:"base"` // 发起时用户的角色；确认时若已变化则提案作废
	Envs  []string                `json:"envs"` // 被授予 admin 的环境键（展示用）
}

func roleSet(m map[string][]authz.Role) map[string]map[authz.Role]bool {
	out := map[string]map[authz.Role]bool{}
	for k, rs := range m {
		set := map[authz.Role]bool{}
		for _, r := range rs {
			set[r] = true
		}
		out[strings.TrimSpace(k)] = set
	}
	return out
}

func rolesEqual(a, b map[string][]authz.Role) bool { return reflect.DeepEqual(roleSet(a), roleSet(b)) }

// newlyAdmin 返回 next 相对 prev「多出 admin 角色」的环境键（排序）：next[env] 含 admin 而 prev[env] 不含。
func newlyAdmin(prev, next map[string][]authz.Role) []string {
	ps := roleSet(prev)
	var out []string
	for env, set := range roleSet(next) {
		if set[authz.RoleAdmin] && !ps[env][authz.RoleAdmin] {
			out = append(out, env)
		}
	}
	sort.Strings(out)
	return out
}

// stripAdmin 返回去掉所有 admin 的角色集合（环境键保留，列表可能为空）。
func stripAdmin(m map[string][]authz.Role) map[string][]authz.Role {
	out := make(map[string][]authz.Role, len(m))
	for k, rs := range m {
		list := []authz.Role{}
		for _, r := range rs {
			if r != authz.RoleAdmin {
				list = append(list, r)
			}
		}
		out[k] = list
	}
	return out
}

func validRoleMap(m map[string][]authz.Role) error {
	for env, rs := range m {
		if strings.TrimSpace(env) == "" {
			return errors.New("环境名不能为空")
		}
		for _, r := range rs {
			if !r.Valid() {
				return fmt.Errorf("未知角色 %q", r)
			}
		}
	}
	return nil
}

// applyProposal 执行提案（调用方持有 ps.mu，且已校验确认人身份）。返回错误则提案作废，并带上原因。
func (s *Server) applyProposal(o *ops, r *http.Request, ru reqUser, p *Proposal) error {
	switch p.Kind {
	case PropGrantAdmin:
		var pl grantAdminPayload
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			return errors.New("提案内容损坏")
		}
		cur, ok := o.cfg.Users.Get(pl.User)
		if !ok {
			return fmt.Errorf("用户 %s 已不存在", pl.User)
		}
		if !rolesEqual(cur.Roles, pl.Base) {
			return fmt.Errorf("用户 %s 的角色在提案发起后已被修改，请基于最新角色重新发起", pl.User)
		}
		if err := o.cfg.Users.SetRoles(pl.User, pl.Roles); err != nil {
			return err
		}
		if pl.DBs != nil {
			_ = o.cfg.Users.SetDBs(pl.User, pl.DBs)
		}
		o.sessions.RevokeUser(pl.User)
		s.auditReq(r, ru.U, "user.roles", pl.User, "high", fmt.Sprintf("两名管理员确认（发起人 %s），角色改为 %v，已吊销其会话", p.By, pl.Roles), "success")
		return nil
	case PropApprovalPolicy:
		var t authz.PolicyTable
		if err := json.Unmarshal(p.Payload, &t); err != nil {
			return errors.New("提案内容损坏")
		}
		if err := t.Validate(); err != nil { // 再校验一次：硬性下限在确认时同样生效
			return err
		}
		t = t.Clone()
		o.mu.Lock()
		o.settings.ApprovalPolicy = &t
		s.saveSettings(o)
		o.mu.Unlock()
		return nil
	}
	return fmt.Errorf("未知的提案种类 %q", p.Kind)
}

// ---------------------------------------------------------------- 发起提案的业务入口 ----

// proposeGrantAdmin 为「授予 admin 角色」创建提案并回复 202。base 是发起时用户已生效的角色，roles 是确认后的完整角色。
func (s *Server) proposeGrantAdmin(w http.ResponseWriter, r *http.Request, ru reqUser, user string, base, roles map[string][]authz.Role, dbs []string, created bool) {
	envs := newlyAdmin(base, roles)
	summary := fmt.Sprintf("授予用户 %s 管理员角色（环境：%s）", user, strings.Join(envs, "、"))
	p, err := s.newProposal(r, ru, PropGrantAdmin, user, summary, grantAdminPayload{User: user, Roles: roles, DBs: dbs, Base: base, Envs: envs})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := "需要另一名管理员确认"
	if created {
		msg = fmt.Sprintf("用户 %s 已创建（暂不含管理员角色）；授予管理员角色需要另一名管理员确认", user)
	}
	writeJSONCode(w, http.StatusAccepted, map[string]any{"ok": true, "created": created, "proposal": s.viewOf(s.ops, ru.U, p), "message": msg})
}

// ---------------------------------------------------------------- HTTP 接口 ----

func (s *Server) registerPolicy(mux *http.ServeMux) {
	o := s.ops

	mux.HandleFunc("GET /api/ops/proposals", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ps := o.props
		ps.mu.Lock()
		s.expireLocked(o)
		admin := isAdminFor(o, ru.U)
		pending, history := []proposalView{}, []proposalView{}
		for i := len(ps.items) - 1; i >= 0; i-- { // 新的在前
			p := ps.items[i]
			if !admin && !strings.EqualFold(p.By, ru.U.Name) {
				continue
			}
			if p.State == PropPending {
				pending = append(pending, s.viewOf(o, ru.U, *p))
			} else if len(history) < proposalHistory {
				history = append(history, s.viewOf(o, ru.U, *p))
			}
		}
		ps.mu.Unlock()
		writeJSON(w, map[string]any{"pending": pending, "history": history, "admin": admin, "ttlMinutes": int(proposalTTL / time.Minute), "serverTime": o.now()})
	}))

	mux.HandleFunc("POST /api/ops/proposals/{id}/confirm", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ps := o.props
		ps.mu.Lock()
		defer ps.mu.Unlock()
		s.expireLocked(o)
		p := ps.find(r.PathValue("id"))
		if p == nil {
			writeErr(w, http.StatusNotFound, "找不到该提案")
			return
		}
		if p.State != PropPending {
			msg := "提案已是「" + p.State + "」状态，不能再确认"
			if p.State == PropExpired {
				msg = "提案已过期，请重新发起"
			}
			writeJSONCode(w, http.StatusConflict, map[string]any{"error": msg, "code": "state", "proposal": s.viewOf(o, ru.U, *p)})
			return
		}
		if !ru.U.Can(o.cfg.Env, capForKind(p.Kind)) {
			s.auditReq(r, ru.U, "authz.deny", r.URL.Path, "", "缺少能力 "+string(capForKind(p.Kind)), "denied")
			writeErr(w, http.StatusForbidden, "没有权限："+string(capForKind(p.Kind)))
			return
		}
		if strings.EqualFold(p.By, ru.U.Name) {
			s.propAudit(r, &ru.U, "proposal.confirm", *p, "denied", "发起人不能确认自己的提案")
			writeErr(w, http.StatusConflict, "需要另一名管理员确认：发起人不能确认自己的提案")
			return
		}
		if err := s.applyProposal(o, r, ru, p); err != nil {
			p.State, p.ConfirmedBy, p.Note = PropRejected, ru.U.Name, "确认时无法应用："+err.Error()
			_ = ps.saveLocked()
			s.propAudit(r, &ru.U, "proposal.confirm", *p, "failed", err.Error())
			writeJSONCode(w, http.StatusConflict, map[string]any{"error": p.Note, "code": "apply", "proposal": s.viewOf(o, ru.U, *p)})
			return
		}
		p.State, p.ConfirmedBy = PropApplied, ru.U.Name
		if err := ps.saveLocked(); err != nil {
			s.cfg.Logf("proposals.json 保存失败：%v", err)
		}
		s.propAudit(r, &ru.U, "proposal.confirm", *p, "success", "发起人 "+p.By+"，确认人 "+ru.U.Name+"，已生效")
		writeJSON(w, map[string]any{"ok": true, "proposal": s.viewOf(o, ru.U, *p)})
	}))

	mux.HandleFunc("POST /api/ops/proposals/{id}/reject", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Note string }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
		if rs := []rune(strings.TrimSpace(in.Note)); len(rs) > 200 {
			in.Note = string(rs[:200])
		}
		ps := o.props
		ps.mu.Lock()
		defer ps.mu.Unlock()
		s.expireLocked(o)
		p := ps.find(r.PathValue("id"))
		if p == nil {
			writeErr(w, http.StatusNotFound, "找不到该提案")
			return
		}
		if p.State != PropPending {
			writeJSONCode(w, http.StatusConflict, map[string]any{"error": "提案已是「" + p.State + "」状态，不能再拒绝 / 撤回", "code": "state", "proposal": s.viewOf(o, ru.U, *p)})
			return
		}
		switch {
		case strings.EqualFold(p.By, ru.U.Name): // 发起人撤回
			p.State, p.ConfirmedBy, p.Note = PropCancelled, ru.U.Name, strings.TrimSpace(in.Note)
			_ = ps.saveLocked()
			s.propAudit(r, &ru.U, "proposal.cancel", *p, "success", "发起人撤回")
		case ru.U.Can(o.cfg.Env, capForKind(p.Kind)):
			p.State, p.ConfirmedBy, p.Note = PropRejected, ru.U.Name, strings.TrimSpace(in.Note)
			_ = ps.saveLocked()
			s.propAudit(r, &ru.U, "proposal.reject", *p, "success", "发起人 "+p.By+"，拒绝人 "+ru.U.Name)
		default:
			s.auditReq(r, ru.U, "authz.deny", r.URL.Path, "", "缺少能力 "+string(capForKind(p.Kind)), "denied")
			writeErr(w, http.StatusForbidden, "没有权限："+string(capForKind(p.Kind)))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "proposal": s.viewOf(o, ru.U, *p)})
	}))

	mux.HandleFunc("GET /api/ops/policy", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		cur, isDefault := o.policyTable()
		ps := o.props
		ps.mu.Lock()
		s.expireLocked(o)
		var pend *proposalView
		for i := len(ps.items) - 1; i >= 0; i-- {
			if p := ps.items[i]; p.State == PropPending && p.Kind == PropApprovalPolicy {
				v := s.viewOf(o, ru.U, *p)
				pend = &v
				break
			}
		}
		ps.mu.Unlock()
		out := map[string]any{
			"policy": cur, "defaults": authz.DefaultPolicy(), "usingDefault": isDefault,
			"risks": authz.Risks(), "envs": authz.Envs(), "maxLevels": authz.MaxLevels,
			"floors": authz.PolicyFloors(), "limits": policyLimits(), "env": o.cfg.Env, "mfaDisabled": o.cfg.NoMFA,
			"canEdit": ru.U.Can(o.cfg.Env, authz.CapSettings),
		}
		if pend != nil {
			out["pendingProposal"] = pend
		}
		writeJSON(w, out)
	}))

	mux.HandleFunc("POST /api/ops/policy", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Policy *authz.PolicyTable `json:"policy"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if in.Policy == nil {
			writeErr(w, http.StatusBadRequest, "缺少 policy")
			return
		}
		if err := in.Policy.Validate(); err != nil {
			s.auditReq(r, ru.U, "policy.propose", "approval-policy", "high", err.Error(), "denied")
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		next := in.Policy.Clone()
		cur, _ := o.policyTable()
		diff := cur.Diff(next)
		if len(diff) == 0 {
			writeErr(w, http.StatusBadRequest, "与当前生效的审批策略相同，无需修改")
			return
		}
		p, err := s.newProposal(r, ru, PropApprovalPolicy, "approval-policy", "修改审批策略："+strings.Join(diff, "；"), next)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONCode(w, http.StatusAccepted, map[string]any{"ok": true, "proposal": s.viewOf(o, ru.U, p), "message": "需要另一名管理员确认"})
	}))
}

// policyLimits 是展示给管理员的规则说明。
func policyLimits() []string {
	return []string{
		"审批级数范围 0–3；低风险在任何环境都允许 0 级（免审批、留痕）。",
		"硬性下限：生产环境的高风险变更至少 2 级审批，且申请人必须通过 MFA。",
		"硬性下限：生产环境的中风险变更至少 1 级审批。",
		"硬性下限：预发环境的高风险变更至少 2 级审批。",
		"紧急变更（冻结期）规则固定、不可配置：预发 / 生产至少 2 级审批；测试环境无紧急通道，按常规。",
		"修改策略需由两名管理员先后确认（发起人不能自己确认），提案 1 小时内未确认自动作废。",
	}
}
