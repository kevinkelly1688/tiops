// @author: kevinkelly
// @license: Apache License 2.0
// @file: roles_api.go
// @time: 2026/10/28

package server

// 自定义角色（角色 = 能力集合）与用户资料扩展。
//
//	GET  /api/roles               角色列表（内置 + 自定义）、能力目录、限制（users.manage 或 audit.view）
//	POST /api/roles               新建自定义角色，可 copyFrom 复制已有角色（users.manage）
//	POST /api/roles/{id}          修改自定义角色的名称 / 说明 / 能力；内置角色 403（users.manage）
//	POST /api/roles/{id}/delete   删除自定义角色；仍有成员持有则 409 并给出成员名单（users.manage）
//
// 约束（均在 internal/authz/roles.go 实现，这里只做 HTTP 适配与审计）：内置角色不可改 / 删；自定义角色不能授予
// users.manage / writeswitch / settings.edit；每项能力的授予方式受限；必须含 view.overview；防提权；id / 名称唯一。
// 生效即时：会话里不缓存用户或能力，每个请求都按当前注册表判定（见 authenticate），所以改完下一次请求就生效。
// 审计动作：role.create / role.update / role.delete（摘要含「+cap(grant) −cap」差异）；加载时的降级告警为 role.load。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
)

// roleView 是 GET /api/roles 与写接口返回的角色。
type roleView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Desc        string            `json:"desc"`
	Builtin     bool              `json:"builtin"`
	Caps        map[string]string `json:"caps"`
	Members     int               `json:"members"`
	MemberNames []string          `json:"memberNames"`
	Created     *time.Time        `json:"created,omitempty"`
	Updated     *time.Time        `json:"updated,omitempty"`
	UpdatedBy   string            `json:"updatedBy,omitempty"`
}

func customView(c authz.CustomRole, users []authz.User) roleView {
	m := authz.RoleMembers(users, authz.Role(c.ID))
	v := roleView{ID: c.ID, Name: c.Name, Desc: c.Desc, Caps: authz.CapStrings(c.Caps), Members: len(m), MemberNames: m, UpdatedBy: c.UpdatedBy}
	if !c.Created.IsZero() {
		t := c.Created
		v.Created = &t
	}
	if !c.Updated.IsZero() {
		t := c.Updated
		v.Updated = &t
	}
	return v
}

func builtinView(r authz.Role, users []authz.User) roleView {
	name, desc, _ := authz.BuiltinRoleInfo(r)
	caps, _ := authz.RoleCaps(r)
	m := authz.RoleMembers(users, r)
	return roleView{ID: string(r), Name: name, Desc: desc, Builtin: true, Caps: authz.CapStrings(caps), Members: len(m), MemberNames: m}
}

// roleViews 返回全部角色：内置在前（矩阵列顺序），自定义在后（创建先后）。
func (s *Server) roleViews() []roleView {
	users := s.ops.cfg.Users.List()
	out := make([]roleView, 0, 16)
	for _, r := range authz.AllRoles() {
		out = append(out, builtinView(r, users))
	}
	for _, c := range s.ops.cfg.Roles.List() {
		out = append(out, customView(c, users))
	}
	return out
}

// initRoles 在 EnableOps 里调用：把角色库加载时的降级告警写到日志与审计（不会提权，只会丢弃）。
func (s *Server) initRoles(o *ops) {
	ws := o.cfg.Roles.Warnings()
	if len(ws) == 0 {
		return
	}
	for _, w := range ws {
		s.cfg.Logf("roles.json：%s", w)
	}
	_, _ = o.cfg.Audit.Append(audit.Entry{User: "system", Action: "role.load", Env: o.cfg.Env, Cluster: s.cfg.Col.Params.Cluster,
		Target: "roles.json", Risk: "high", Summary: "自定义角色库加载时已降级处理：" + strings.Join(ws, "；"), Result: "failed"})
}

func roleStatus(code string) int {
	switch code {
	case authz.RoleErrInvalid:
		return http.StatusBadRequest
	case authz.RoleErrBuiltin, authz.RoleErrLockedCap, authz.RoleErrEscalation:
		return http.StatusForbidden
	case authz.RoleErrNotFound:
		return http.StatusNotFound
	case authz.RoleErrExists, authz.RoleErrLimit, authz.RoleErrInUse:
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// roleFail 回复错误并写审计：规则拒绝（RoleError）记 denied，存储失败记 failed。
func (s *Server) roleFail(w http.ResponseWriter, r *http.Request, ru reqUser, action, target string, err error) {
	var re *authz.RoleError
	if errors.As(err, &re) {
		s.auditReq(r, ru.U, action, target, "high", re.Error(), "denied")
		body := map[string]any{"error": re.Error(), "code": re.Code}
		if len(re.Members) > 0 {
			body["members"] = re.Members
		}
		writeJSONCode(w, roleStatus(re.Code), body)
		return
	}
	s.auditReq(r, ru.U, action, target, "high", "角色库保存失败："+err.Error(), "failed")
	writeJSONCode(w, http.StatusInternalServerError, map[string]any{"error": "角色库保存失败：" + err.Error(), "code": "store"})
}

func (s *Server) registerRoles(mux *http.ServeMux) {
	o := s.ops

	mux.HandleFunc("GET /api/roles", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		if !ru.U.Can(o.cfg.Env, authz.CapUsers) && !ru.U.Can(o.cfg.Env, authz.CapAuditView) {
			s.auditReq(r, ru.U, "authz.deny", r.URL.Path, "", "缺少能力 users.manage 或 audit.view", "denied")
			writeErr(w, http.StatusForbidden, "没有权限：users.manage 或 audit.view")
			return
		}
		writeJSON(w, map[string]any{
			"roles":   s.roleViews(),
			"catalog": authz.Catalog(),
			"limits": map[string]any{"maxCustom": authz.MaxCustomRoles, "maxName": authz.MaxRoleNameRunes, "maxDesc": authz.MaxRoleDescRunes,
				"idPattern": "^[a-z][a-z0-9_-]{1,31}$"},
			"warnings": o.cfg.Roles.Warnings(),
		})
	}))

	mux.HandleFunc("POST /api/roles", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			ID       string            `json:"id"`
			Name     string            `json:"name"`
			Desc     string            `json:"desc"`
			Caps     map[string]string `json:"caps"`
			CopyFrom string            `json:"copyFrom"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		in.ID = strings.TrimSpace(in.ID)
		parsed, err := authz.ParseCapMap(in.Caps)
		if err != nil {
			s.roleFail(w, r, ru, "role.create", in.ID, err)
			return
		}
		caps := map[authz.Cap]authz.Grant{}
		if cf := strings.TrimSpace(in.CopyFrom); cf != "" {
			if caps, err = authz.CopyBase(authz.Role(cf)); err != nil {
				s.roleFail(w, r, ru, "role.create", in.ID, err)
				return
			}
		}
		for c, g := range parsed { // body.caps 叠加在底之上；none 表示去掉继承来的能力
			if g == authz.GrantNone {
				delete(caps, c)
			} else {
				caps[c] = g
			}
		}
		role, err := o.cfg.Roles.Create(ru.U, o.cfg.Env, authz.RoleInput{ID: in.ID, Name: in.Name, Desc: in.Desc, Caps: caps}, o.now())
		if err != nil {
			s.roleFail(w, r, ru, "role.create", in.ID, err)
			return
		}
		sum := fmt.Sprintf("创建自定义角色「%s」：%s", role.Name, strings.Join(authz.DiffCaps(nil, role.Caps), " "))
		if cf := strings.TrimSpace(in.CopyFrom); cf != "" {
			sum += "（复制自 " + cf + "）"
		}
		s.auditReq(r, ru.U, "role.create", role.ID, "high", sum, "success")
		writeJSON(w, map[string]any{"ok": true, "role": customView(role, o.cfg.Users.List())})
	}))

	mux.HandleFunc("POST /api/roles/{id}", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		var in struct {
			Name *string           `json:"name"`
			Desc *string           `json:"desc"`
			Caps map[string]string `json:"caps"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		up := authz.RoleUpdate{Name: in.Name, Desc: in.Desc}
		if in.Caps != nil {
			parsed, err := authz.ParseCapMap(in.Caps)
			if err != nil {
				s.roleFail(w, r, ru, "role.update", id, err)
				return
			}
			up.Caps = parsed
		}
		before, after, err := o.cfg.Roles.Update(ru.U, o.cfg.Env, id, up, o.now())
		if err != nil {
			s.roleFail(w, r, ru, "role.update", id, err)
			return
		}
		users := o.cfg.Users.List()
		view := customView(after, users)
		var parts []string
		if before.Name != after.Name {
			parts = append(parts, fmt.Sprintf("名称「%s」→「%s」", before.Name, after.Name))
		}
		if before.Desc != after.Desc {
			parts = append(parts, "说明已修改")
		}
		if d := authz.DiffCaps(before.Caps, after.Caps); len(d) > 0 {
			parts = append(parts, "权限 "+strings.Join(d, " "))
		}
		changed := len(parts) > 0
		if !changed {
			parts = append(parts, "无变化")
		}
		s.auditReq(r, ru.U, "role.update", id, "high", fmt.Sprintf("修改自定义角色「%s」：%s；即时影响 %d 名成员", after.Name, strings.Join(parts, "；"), view.Members), "success")
		writeJSON(w, map[string]any{"ok": true, "changed": changed, "role": view, "affected": view.Members, "memberNames": view.MemberNames})
	}))

	mux.HandleFunc("POST /api/roles/{id}/delete", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		old, err := o.cfg.Roles.Delete(id, o.cfg.Users)
		if err != nil {
			s.roleFail(w, r, ru, "role.delete", id, err)
			return
		}
		s.auditReq(r, ru.U, "role.delete", id, "high", fmt.Sprintf("删除自定义角色「%s」（原含 %d 项能力）", old.Name, len(old.Caps)), "success")
		writeJSON(w, map[string]any{"ok": true, "id": id})
	}))
}

// ---------------------------------------------------------------- 用户资料与解锁 ----

// userView 是 GET /api/users 返回的用户：User 的全部字段，外加登录锁定截止时间（只读）。
type userView struct {
	authz.User
	LockedUntil *time.Time `json:"lockedUntil,omitempty"`
}

type roleInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
}

func knownRoleInfo() (ids []string, info []roleInfo) {
	ids, info = []string{}, []roleInfo{}
	for _, r := range authz.KnownRoles() {
		name, builtin, _ := authz.RoleDisplayName(r)
		ids = append(ids, string(r))
		info = append(info, roleInfo{ID: string(r), Name: name, Builtin: builtin})
	}
	return
}

func (s *Server) usersView() []userView {
	now := s.ops.now()
	us := s.ops.cfg.Users.List()
	out := make([]userView, 0, len(us))
	for _, u := range us {
		v := userView{User: u}
		if t, ok := s.ops.cfg.Users.LockedUntil(u.Name, now); ok {
			v.LockedUntil = &t
		}
		out = append(out, v)
	}
	return out
}

var profileFieldLabel = map[string]string{"display": "显示名", "dept": "部门", "email": "邮箱", "phone": "手机", "note": "备注"}

func (s *Server) registerUserProfile(mux *http.ServeMux) {
	o := s.ops

	// 摘要里只写变更了哪些字段，不写邮箱 / 手机 / 备注的内容；口令、TOTP 与此接口无关。
	mux.HandleFunc("POST /api/users/{name}/profile", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Display *string `json:"display"`
			Dept    *string `json:"dept"`
			Email   *string `json:"email"`
			Phone   *string `json:"phone"`
			Note    *string `json:"note"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		name := r.PathValue("name")
		changed, u, err := o.cfg.Users.UpdateProfile(name, authz.ProfileUpdate{Display: in.Display, Dept: in.Dept, Email: in.Email, Phone: in.Phone, Note: in.Note})
		if err != nil {
			s.auditReq(r, ru.U, "user.profile", name, "mid", err.Error(), "denied")
			code := http.StatusBadRequest
			if errors.Is(err, authz.ErrNotFound) {
				code = http.StatusNotFound
			}
			writeErr(w, code, err.Error())
			return
		}
		sum := "资料无变化"
		if len(changed) > 0 {
			var ls []string
			for _, k := range changed {
				ls = append(ls, profileFieldLabel[k])
			}
			sum = "更新资料：" + strings.Join(ls, "、")
		}
		s.auditReq(r, ru.U, "user.profile", u.Name, "mid", sum, "success")
		writeJSON(w, map[string]any{"ok": true, "changed": changed, "user": userView{User: u}})
	}))

	mux.HandleFunc("POST /api/users/{name}/unlock", s.secured(authz.CapUsers, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		name := r.PathValue("name")
		was, until, err := o.cfg.Users.Unlock(name, o.now())
		if err != nil {
			s.auditReq(r, ru.U, "user.unlock", name, "mid", err.Error(), "denied")
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		sum := "清除登录失败计数（当时未被锁定）"
		if was {
			sum = fmt.Sprintf("解除登录锁定（原锁定至 %s）", until.Local().Format("15:04:05"))
		}
		s.auditReq(r, ru.U, "user.unlock", name, "mid", sum, "success")
		writeJSON(w, map[string]any{"ok": true, "wasLocked": was})
	}))
}
