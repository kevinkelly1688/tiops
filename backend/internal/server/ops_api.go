// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_api.go
// @time: 2026/10/28

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/change"
	"tidbops/internal/collect"
	"tidbops/internal/model"
)

// csrfFor 由会话令牌派生 CSRF 令牌（无状态，随会话轮换而变化）。
func (o *ops) csrfFor(session string) string {
	m := hmac.New(sha256.New, append([]byte("csrf|"), o.cfg.Key...))
	m.Write([]byte(session))
	return hex.EncodeToString(m.Sum(nil))
}

func (o *ops) checkCSRF(session, got string) bool {
	return hmac.Equal([]byte(o.csrfFor(session)), []byte(got))
}

type meResp struct {
	Authenticated bool              `json:"authenticated"`
	User          *authz.User       `json:"user,omitempty"`
	Roles         []string          `json:"roles,omitempty"`
	RoleInfo      []roleInfo        `json:"roleInfo,omitempty"` // roles 里每个角色的中文名（含自定义角色）
	Caps          map[string]string `json:"caps,omitempty"`
	CSRF          string            `json:"csrf,omitempty"`
	Env           string            `json:"env"`
	Write         writeState        `json:"write"`
	NeedMFAHigh   bool              `json:"mfaHigh"`
	// 多集群：当前用户可见的集群列表，与当前选中的集群（env / caps / write 都是该集群的）。
	Clusters []clusterBrief `json:"clusters,omitempty"`
	Cluster  *clusterBrief  `json:"cluster,omitempty"`
}

type writeState struct {
	Enabled        bool            `json:"enabled"`
	ReadOnlyForced bool            `json:"readOnlyForced"`
	Pending        *SwitchProposal `json:"pending,omitempty"`
	Reason         string          `json:"reason,omitempty"` // 当前不能写的原因
	Exec           string          `json:"exec"`
	Sim            bool            `json:"sim"`
}

func (s *Server) writeState() writeState {
	o := s.ops
	o.mu.Lock()
	ws := writeState{Enabled: o.settings.WriteEnabled, Pending: o.settings.Pending, Exec: o.cfg.ExecMode, Sim: o.cfg.Sim, ReadOnlyForced: o.readOnlyForced()}
	o.mu.Unlock()
	if err := s.gate(o); err != nil {
		ws.Reason = err.Error()
	}
	return ws
}

func (s *Server) me(ru reqUser) meResp {
	o := s.ops
	r := meResp{Authenticated: true, User: &ru.U, CSRF: o.csrfFor(ru.Session), Env: o.cfg.Env, Write: s.writeState(), Caps: map[string]string{}}
	for _, x := range ru.U.RolesFor(o.cfg.Env) {
		r.Roles = append(r.Roles, string(x))
		name, builtin, _ := authz.RoleDisplayName(x)
		r.RoleInfo = append(r.RoleInfo, roleInfo{ID: string(x), Name: name, Builtin: builtin})
	}
	for _, c := range authz.AllCaps() {
		if g := ru.U.Grant(o.cfg.Env, c); g.Can() {
			r.Caps[string(c)] = g.String()
		}
	}
	_, mfa := o.approvalPolicy("high", o.cfg.Env, false) // 读当前生效的策略表（已套用 NoMFA）
	r.NeedMFAHigh = mfa
	if s.hub != nil {
		r.Clusters, r.Cluster = s.hub.meClusters(ru.U, s.clusterID)
	}
	return r
}

func (s *Server) registerOps(mux *http.ServeMux) {
	o := s.ops
	// ---- 认证
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ User, Password string }
		if !decodeBody(w, r, &in) {
			return
		}
		u, err := o.cfg.Users.Authenticate(in.User, in.Password, "", clientIP(r), o.now())
		if err != nil {
			var le *authz.LockedError
			code := http.StatusUnauthorized
			msg := authz.ErrInvalidCredentials.Error()
			if errors.As(err, &le) {
				code, msg = http.StatusTooManyRequests, fmt.Sprintf("登录失败次数过多，请在 %s 后重试", le.Until.Local().Format("15:04:05"))
			}
			_, _ = o.cfg.Audit.Append(audit.Entry{User: strings.ToLower(strings.TrimSpace(in.User)), IP: clientIP(r), Env: o.cfg.Env, Action: "auth.login", Summary: "登录失败", Result: "denied"})
			writeErr(w, code, msg)
			return
		}
		// 只在登录成功时记录最近登录时间（带节流，见 authz.Store.RecordLogin）；失败不影响登录。
		if err := o.cfg.Users.RecordLogin(u.Name, o.now()); err != nil {
			s.cfg.Logf("记录最近登录时间失败：%v", err)
		} else if nu, ok := o.cfg.Users.Get(u.Name); ok {
			u = nu
		}
		tok, _ := o.sessions.Create(u.Name)
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secureCookie(r), MaxAge: int(o.cfg.SessionAbs.Seconds())})
		s.auditReq(r, u, "auth.login", u.Name, "", "登录成功", "success")
		writeJSON(w, s.loginMe(reqUser{U: u, Session: tok}))
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if ru, ok := s.authenticate(r); ok {
			if !o.checkCSRF(ru.Session, r.Header.Get("X-CSRF-Token")) {
				writeErr(w, http.StatusForbidden, "CSRF 校验失败")
				return
			}
			o.sessions.Destroy(ru.Session)
			s.auditReq(r, ru.U, "auth.logout", ru.U.Name, "", "退出登录", "success")
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		ru, ok := s.authenticate(r)
		if !ok {
			writeJSON(w, meResp{Authenticated: false, Env: o.cfg.Env})
			return
		}
		writeJSON(w, s.me(ru))
	})
	mux.HandleFunc("POST /api/auth/password", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Old, New string }
		if !decodeBody(w, r, &in) {
			return
		}
		// 任何错误都拒绝：账号被锁定时 Authenticate 返回的不是「口令错误」，旧写法（!Contains(…) == false）会因此跳过校验，
		// 持有会话的人先把自己锁定（连错 5 次）就能免旧口令改密。
		if _, err := o.cfg.Users.Authenticate(ru.U.Name, in.Old, "", "", o.now()); err != nil {
			msg := "当前口令不正确"
			if errors.Is(err, authz.ErrLocked) {
				msg = err.Error()
			}
			writeErr(w, http.StatusForbidden, msg)
			return
		}
		if err := authz.CheckPassword(ru.U.Name, in.New); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := o.cfg.Users.SetPassword(ru.U.Name, in.New); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.auditReq(r, ru.U, "auth.password", ru.U.Name, "mid", "修改口令", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/auth/totp/begin", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		sec, uri, err := o.cfg.Users.BeginTOTP(ru.U.Name)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"secret": sec, "uri": uri})
	}))
	mux.HandleFunc("POST /api/auth/totp/confirm", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Code string }
		if !decodeBody(w, r, &in) {
			return
		}
		if err := o.cfg.Users.ConfirmTOTP(ru.U.Name, in.Code, o.now()); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.auditReq(r, ru.U, "auth.totp", ru.U.Name, "mid", "绑定 TOTP", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))

	// ---- 变更
	mux.HandleFunc("POST /api/changes/preview", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Kind        string          `json:"kind"`
			Params      json.RawMessage `json:"params"`
			ScheduledAt string          `json:"scheduledAt"` // 可选：计划执行时间（RFC3339），用于窗口检查；缺省按当前时间
			Emergency   bool            `json:"emergency"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		c := kindCap(in.Kind)
		if c == "" || !ru.U.Can(o.cfg.Env, c) {
			writeErr(w, http.StatusForbidden, "没有权限："+string(c))
			return
		}
		startAt := o.now()
		if in.ScheduledAt != "" {
			t, err := time.Parse(time.RFC3339, in.ScheduledAt)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "scheduledAt 需为 RFC3339 时间")
				return
			}
			startAt = t
		}
		plan, err := o.engine.Preview(r.Context(), in.Kind, in.Params)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		wchecks, wblocked := o.engine.WindowChecks(plan, startAt, in.Emergency)
		if wchecks == nil {
			wchecks = []change.Check{}
		}
		levels, mfa := o.approvalPolicy(plan.Risk, o.cfg.Env, in.Emergency) // 与引擎 Create 走同一张生效的策略表
		gateErr := ""
		if err := s.gate(o); err != nil {
			gateErr = err.Error()
		}
		writeJSON(w, map[string]any{"plan": plan, "levels": levels, "mfa": mfa, "gate": gateErr, "env": o.cfg.Env, "windowChecks": wchecks, "windowBlocked": wblocked})
	}))
	mux.HandleFunc("POST /api/changes", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Kind        string          `json:"kind"`
			Params      json.RawMessage `json:"params"`
			Note        string          `json:"note"`
			Ticket      string          `json:"ticket"`
			ScheduledAt string          `json:"scheduledAt"`
			Emergency   bool            `json:"emergency"`
			OTP         string          `json:"otp"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		c := kindCap(in.Kind)
		if c == "" || !ru.U.Can(o.cfg.Env, c) {
			s.auditReq(r, ru.U, "authz.deny", "change."+in.Kind, "", "缺少能力 "+string(c), "denied")
			writeErr(w, http.StatusForbidden, "没有权限："+string(c))
			return
		}
		ci := change.CreateInput{Kind: in.Kind, Params: in.Params, Note: in.Note, Ticket: in.Ticket, Applicant: ru.U.Name, Emergency: in.Emergency}
		if in.ScheduledAt != "" {
			t, err := time.Parse(time.RFC3339, in.ScheduledAt)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "scheduledAt 需为 RFC3339 时间")
				return
			}
			ci.ScheduledAt = &t
		}
		if in.OTP != "" {
			if !o.cfg.Users.VerifyOTP(ru.U.Name, in.OTP, o.now()) {
				writeErr(w, http.StatusForbidden, "动态验证码错误或已被使用")
				return
			}
			ci.MFAVerified = true
		}
		ch, err := o.engine.Create(r.Context(), ci)
		if err != nil {
			var be *change.BlockedError
			switch {
			case errors.As(err, &be):
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "code": "blocked", "plan": be.Plan})
			case errors.Is(err, change.ErrNeedMFA):
				code := "need_mfa"
				if !ru.U.HasTOTP {
					code = "need_mfa_enroll"
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(apiError{Error: err.Error(), Code: code})
			default:
				s.auditReq(r, ru.U, "change.create", in.Kind, "", err.Error(), "denied")
				writeErr(w, http.StatusConflict, err.Error())
			}
			return
		}
		writeJSON(w, ch)
	}))
	mux.HandleFunc("GET /api/changes", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, map[string]any{"changes": o.engine.List(), "summary": o.engine.Summary(), "serverTime": o.now()})
	}))
	mux.HandleFunc("GET /api/changes/{id}", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "找不到该变更")
			return
		}
		writeJSON(w, ch)
	}))
	mux.HandleFunc("POST /api/changes/{id}/approve", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Comment string }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "找不到该变更")
			return
		}
		var prior []string
		for _, a := range ch.Approvals {
			prior = append(prior, a.User)
		}
		if err := authz.ValidateApproval(ch.Applicant, prior, ru.U, o.cfg.Env, ch.Risk); err != nil {
			s.auditReq(r, ru.U, "change.approve", ch.ID, ch.Risk, err.Error(), "denied")
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		out, err := o.engine.Approve(ch.ID, ru.U.Name, in.Comment)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("POST /api/changes/{id}/reject", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Reason string }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "找不到该变更")
			return
		}
		if !authz.CanApprove(ru.U, o.cfg.Env, ch.Risk) {
			s.auditReq(r, ru.U, "change.reject", ch.ID, ch.Risk, "无审批权限", "denied")
			writeErr(w, http.StatusForbidden, authz.ErrNotApprover.Error())
			return
		}
		out, err := o.engine.Reject(ch.ID, ru.U.Name, in.Reason)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, out)
	}))
	owner := func(ch change.Change, u authz.User) bool {
		return ch.Applicant == u.Name || u.HasRole(o.cfg.Env, authz.RoleDBA)
	}
	mux.HandleFunc("POST /api/changes/{id}/cancel", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Reason string }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "找不到该变更")
			return
		}
		if !owner(ch, ru.U) {
			writeErr(w, http.StatusForbidden, "只有申请人或 DBA 可以取消")
			return
		}
		out, err := o.engine.Cancel(ch.ID, ru.U.Name, in.Reason)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("POST /api/changes/{id}/abort", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "找不到该变更")
			return
		}
		if !owner(ch, ru.U) {
			writeErr(w, http.StatusForbidden, "只有申请人或 DBA 可以中止")
			return
		}
		if err := o.engine.Abort(ch.ID, ru.U.Name); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/changes/{id}/confirm", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "找不到该变更")
			return
		}
		if !owner(ch, ru.U) {
			writeErr(w, http.StatusForbidden, "只有申请人或 DBA 可以确认人工步骤")
			return
		}
		if err := o.engine.ConfirmStep(ch.ID, ru.U.Name); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/changes/{id}/review", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Text string }
		if !decodeBody(w, r, &in) {
			return
		}
		ch, ok := o.engine.Get(r.PathValue("id"))
		if !ok || ch.Applicant != ru.U.Name {
			writeErr(w, http.StatusForbidden, "只有申请人可以填写复盘")
			return
		}
		if err := o.engine.SetPostReview(ch.ID, in.Text); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		s.auditReq(r, ru.U, "change.review", ch.ID, ch.Risk, "填写事后复盘", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("GET /api/maints", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, map[string]any{"maints": o.engine.Maints(), "types": maintTypes(), "serverTime": o.now()})
	}))
	mux.HandleFunc("POST /api/maints/{id}/todo", s.secured(authz.CapMaint, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Idx  int  `json:"idx"`
			Done bool `json:"done"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if err := o.engine.SetTodo(r.PathValue("id"), in.Idx, in.Done); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

	// ---- 审计
	mux.HandleFunc("GET /api/audit", s.secured(authz.CapAuditView, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		q := r.URL.Query()
		f := s.auditFilter(r, ru.U)
		if t, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
			f.Since = t
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 200
		}
		es, err := o.cfg.Audit.Query(f, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"entries": es})
	}))
	mux.HandleFunc("GET /api/audit/verify", s.secured(authz.CapAuditView, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		n, err := o.cfg.Audit.Verify()
		res := map[string]any{"ok": err == nil, "entries": n}
		if err != nil {
			res["error"] = err.Error()
			s.auditReq(r, ru.U, "audit.verify", "", "high", "审计链校验失败："+err.Error(), "failed")
		}
		writeJSON(w, res)
	}))
	mux.HandleFunc("GET /api/audit/export", s.secured(authz.CapAuditExport, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		format := r.URL.Query().Get("format")
		if format != "jsonl" {
			format = "csv"
		}
		s.auditReq(r, ru.U, "audit.export", format, "mid", "导出审计日志", "success")
		w.Header().Set("Content-Disposition", `attachment; filename="audit.`+format+`"`)
		if format == "csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "application/x-ndjson")
		}
		_ = o.cfg.Audit.ExportFiltered(w, s.auditFilter(r, ru.U), format)
	}))

	// ---- 用户管理
	mux.HandleFunc("GET /api/users", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		// 审计员（audit.view）可只读查看用户清单与角色分配（合规核对），写操作仍需 users.manage
		if !ru.U.Can(o.cfg.Env, authz.CapUsers) && !ru.U.Can(o.cfg.Env, authz.CapAuditView) {
			s.auditReq(r, ru.U, "authz.deny", r.URL.Path, "", "缺少能力 users.manage 或 audit.view", "denied")
			writeErr(w, http.StatusForbidden, "没有权限：users.manage 或 audit.view")
			return
		}
		ids, info := knownRoleInfo() // 内置 7 个 + 自定义角色；roleInfo 带中文名与 builtin 标记
		writeJSON(w, map[string]any{"users": s.usersView(), "roles": ids, "roleInfo": info})
	}))
	mux.HandleFunc("POST /api/users", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Name, Display, Password  string
			Roles                    map[string][]authz.Role
			DBs                      []string
			Dept, Email, Phone, Note string // 可选资料
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if err := authz.CheckPassword(in.Name, in.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		prof, err := authz.Profile{Display: in.Display, Dept: in.Dept, Email: in.Email, Phone: in.Phone, Note: in.Note}.Normalize()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validRoleMap(in.Roles); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// 授予 admin 角色需要双人复核：先以不含 admin 的角色创建用户（立即生效），再对「授予 admin」发起提案。
		// 提案里不含口令。
		wantAdmin := len(newlyAdmin(nil, in.Roles)) > 0
		roles := in.Roles
		if wantAdmin {
			roles = stripAdmin(in.Roles)
		}
		if err := o.cfg.Users.AddWithProfile(in.Name, in.Password, roles, in.DBs, prof); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if !wantAdmin {
			s.auditReq(r, ru.U, "user.add", in.Name, "high", fmt.Sprintf("创建用户，角色 %v", in.Roles), "success")
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		s.auditReq(r, ru.U, "user.add", in.Name, "high", fmt.Sprintf("创建用户，角色 %v（admin 角色待另一名管理员确认）", roles), "success")
		created, _ := o.cfg.Users.Get(in.Name)
		s.proposeGrantAdmin(w, r, ru, created.Name, created.Roles, in.Roles, nil, true)
	}))
	mux.HandleFunc("POST /api/users/{name}/roles", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Roles map[string][]authz.Role
			DBs   []string
		}
		if !decodeBody(w, r, &in) {
			return
		}
		name := r.PathValue("name")
		if err := validRoleMap(in.Roles); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// 新角色集合比旧集合多出 admin：不直接生效，改为「授予管理员角色」提案，由另一名管理员确认后才修改。
		// 降级（去掉 admin）与其他角色改动照旧立即生效（最后一个管理员保护仍由用户库保证）。
		if cur, ok := o.cfg.Users.Get(name); ok && len(newlyAdmin(cur.Roles, in.Roles)) > 0 {
			s.proposeGrantAdmin(w, r, ru, cur.Name, cur.Roles, in.Roles, in.DBs, false)
			return
		}
		if err := o.cfg.Users.SetRoles(name, in.Roles); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.DBs != nil {
			_ = o.cfg.Users.SetDBs(name, in.DBs)
		}
		o.sessions.RevokeUser(name)
		s.auditReq(r, ru.U, "user.roles", name, "high", fmt.Sprintf("角色改为 %v，已吊销其会话", in.Roles), "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/users/{name}/disable", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Disabled bool }
		if !decodeBody(w, r, &in) {
			return
		}
		name := r.PathValue("name")
		if err := o.cfg.Users.SetDisabled(name, in.Disabled); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.Disabled {
			o.sessions.RevokeUser(name)
		}
		s.auditReq(r, ru.U, "user.disable", name, "high", fmt.Sprintf("disabled=%v", in.Disabled), "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/users/{name}/password", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Password string }
		if !decodeBody(w, r, &in) {
			return
		}
		name := r.PathValue("name")
		if err := authz.CheckPassword(name, in.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := o.cfg.Users.SetPassword(name, in.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		o.sessions.RevokeUser(name)
		s.auditReq(r, ru.U, "user.password", name, "high", "重置口令，已吊销其会话", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/users/{name}/delete", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		name := r.PathValue("name")
		if err := o.cfg.Users.Delete(name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		o.sessions.RevokeUser(name)
		s.auditReq(r, ru.U, "user.delete", name, "high", "删除用户", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/users/{name}/totp-reset", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		name := r.PathValue("name")
		if err := o.cfg.Users.ResetTOTP(name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.auditReq(r, ru.U, "user.totp-reset", name, "high", "重置 TOTP", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))

	// ---- 平台设置与写操作开关
	mux.HandleFunc("GET /api/ops/settings", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		o.mu.Lock()
		st := o.settings
		st.Windows = o.windowsLocked() // 返回生效的规则（未自定义时是默认规则）
		st.LBs = nil                   // 手工登记走 GET /api/lb；本接口的返回结构保持不变
		custom := o.settings.Windows != nil
		o.mu.Unlock()
		writeJSON(w, map[string]any{"settings": st, "write": s.writeState(), "env": o.cfg.Env, "windowsCustom": custom, "windowDefaults": change.DefaultWindows()})
	}))
	mux.HandleFunc("POST /api/ops/settings", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Freezes   *[]change.Freeze   `json:"freezes"`
			LB        *bool              `json:"lb"`
			Overrides *map[string]string `json:"overrides"`
			Windows   *[]change.Window   `json:"windows"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		var wins []change.Window
		if in.Windows != nil {
			v, err := change.ValidateWindows(*in.Windows)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "窗口规则不合法："+err.Error())
				return
			}
			wins = v
		}
		o.mu.Lock()
		var changed []string
		if in.Windows != nil {
			o.settings.Windows = wins
			changed = append(changed, "windows")
		}
		if in.Freezes != nil {
			o.settings.Freezes = *in.Freezes
			changed = append(changed, "freezes")
		}
		if in.LB != nil {
			o.settings.LB = *in.LB
			changed = append(changed, "lb")
		}
		if in.Overrides != nil {
			o.settings.Overrides = *in.Overrides
			changed = append(changed, "overrides")
		}
		s.saveSettings(o)
		o.mu.Unlock()
		s.auditReq(r, ru.U, "settings.update", strings.Join(changed, ","), "mid", "修改平台设置", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/ops/write-switch", s.secured(authz.CapWriteSwitch, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Enabled bool }
		if !decodeBody(w, r, &in) {
			return
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		cur := o.settings.WriteEnabled
		switch {
		case !in.Enabled:
			// 熔断：关闭写操作只需一名管理员，立即生效
			o.settings.WriteEnabled, o.settings.Pending = false, nil
			s.saveSettings(o)
			s.auditReq(r, ru.U, "writeswitch.off", o.cfg.Env, "high", "关闭写操作", "success")
			writeJSON(w, map[string]any{"ok": true, "enabled": false})
		case cur:
			writeJSON(w, map[string]any{"ok": true, "enabled": true})
		case o.settings.Pending == nil || !o.settings.Pending.Enabled:
			o.settings.Pending = &SwitchProposal{By: ru.U.Name, Enabled: true, At: o.now()}
			s.saveSettings(o)
			s.auditReq(r, ru.U, "writeswitch.propose", o.cfg.Env, "high", "提议开启写操作，等待另一名管理员确认", "success")
			writeJSON(w, map[string]any{"ok": true, "enabled": false, "pending": o.settings.Pending})
		case o.settings.Pending.By == ru.U.Name:
			writeErr(w, http.StatusConflict, "需要另一名管理员确认")
		case o.now().Sub(o.settings.Pending.At) > time.Hour:
			o.settings.Pending = &SwitchProposal{By: ru.U.Name, Enabled: true, At: o.now()}
			s.saveSettings(o)
			s.auditReq(r, ru.U, "writeswitch.propose", o.cfg.Env, "high", "上一个提议已超时，重新提议", "success")
			writeJSON(w, map[string]any{"ok": true, "enabled": false, "pending": o.settings.Pending})
		default:
			o.settings.WriteEnabled, o.settings.Pending = true, nil
			s.saveSettings(o)
			s.auditReq(r, ru.U, "writeswitch.on", o.cfg.Env, "high", "两名管理员确认，开启写操作", "success")
			writeJSON(w, map[string]any{"ok": true, "enabled": true})
		}
	}))
	mux.HandleFunc("POST /api/ops/write-switch/cancel", s.secured(authz.CapWriteSwitch, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		o.mu.Lock()
		o.settings.Pending = nil
		s.saveSettings(o)
		o.mu.Unlock()
		s.auditReq(r, ru.U, "writeswitch.cancel", o.cfg.Env, "mid", "撤销开启提议", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("GET /api/ops/selfcheck", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, map[string]any{"items": s.selfCheck()})
	}))

	// ---- 双人复核提案与可配置审批策略（policy_api.go）
	s.registerPolicy(mux)

	// ---- 自定义角色、用户资料与解锁（roles_api.go）
	s.registerRoles(mux)
	s.registerUserProfile(mux)
}

func maintTypes() map[string]any {
	out := map[string]any{}
	for k, v := range change.MaintTypes {
		out[k] = map[string]any{"label": v.Label, "def": v.Def, "todo": v.Todo}
	}
	return out
}

// filterSnapshot 按用户权限裁剪快照：没有 SQL 查看权限的看不到慢 SQL；开发角色只能看被授权库的 SQL。
func (s *Server) filterSnapshot(snap model.Snapshot, u authz.User) model.Snapshot {
	env := s.ops.cfg.Env
	if !u.Can(env, authz.CapViewSQL) {
		snap.Slow = []model.SlowGroup{}
		return snap
	}
	if u.Grant(env, authz.CapViewSQL) == authz.GrantAllow {
		return snap
	}
	var keep []model.SlowGroup
	for _, g := range snap.Slow {
		if u.CanViewSQLOf(env, g.DB) {
			keep = append(keep, g)
		}
	}
	snap.Slow = keep
	if snap.Slow == nil {
		snap.Slow = []model.SlowGroup{}
	}
	return snap
}

var _ = collect.VersionSupported
