// @author: kevinkelly
// @license: Apache License 2.0
// @file: clusters_api.go
// @time: 2026/10/28

package server

// 集群接入（多集群管理）接口。全部是全局端点，由主集群实例处理；管理接口需要 settings.edit（主集群 env 下的内置管理员）。
//
//	GET  /api/clusters                列出集群（登录即可）：普通用户 = 其可见的集群（在该集群 env 下有 view.overview）；
//	                                  settings.edit 管理员 = 全部集群，并额外带 config（完整连接配置，密码只回 hasPassword）
//	POST /api/clusters                添加并热生效（settings.edit）；失败（含实例启动失败）整体回滚，不影响其他集群
//	POST /api/clusters/{id}           更新（settings.edit）：id 不可改；mysqlPassword 留空 = 不改；clearPassword=true 清除；
//	                                  主集群只能改 display / desc
//	POST /api/clusters/{id}/delete    删除并热移除（settings.edit）：仅非主集群；有待审批 / 排队 / 执行中的变更或维护时拒绝；
//	                                  数据目录改名为 clusters/.removed-<id>-<时间戳>/ 保留，不物理删除
//	POST /api/clusters/{id}/test      只读连通性测试（settings.edit）：[{item,status:ok|warn|bad,detail,ms}]，每项 10 秒超时
//
// 错误码（风格同 roles_api.go）：invalid 400 | exists 409 | not_found 404 | limit 409 | primary 403 | in_use 409 | store 500 | start 422。
// 审计动作：cluster.add / cluster.update / cluster.remove / cluster.test（remove 风险 high，其余 mid）；摘要不含任何凭据。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/clusters"
)

// clusterStatus 是集群的状态摘要（来自最近一次采集的缓存，不会触发采集）。
type clusterStatus struct {
	State      string     `json:"state"` // ok | error | loading
	Error      string     `json:"error,omitempty"`
	SnapshotAt *time.Time `json:"snapshotAt,omitempty"`
	Reachable  *bool      `json:"reachable"` // null = 尚未采集
	Errors     int        `json:"errors"`    // 上次采集缺失的数据项数
	Version    string     `json:"version,omitempty"`
	Nodes      int        `json:"nodes"`
}

// clusterView 是 /api/clusters 的一项。
type clusterView struct {
	ID       string         `json:"id"`
	Display  string         `json:"display"`
	Desc     string         `json:"desc"`
	Env      string         `json:"env"`
	Mode     string         `json:"mode"`
	Primary  bool           `json:"primary"`
	Local    bool           `json:"local"` // 本机 tiup 导入（可运维）；false = 手工添加的远程集群（只读）
	State    string         `json:"state"`
	Status   clusterStatus  `json:"status"`
	Write    briefWrite     `json:"write"`
	Warnings []string       `json:"warnings,omitempty"`
	Config   *clusters.View `json:"config,omitempty"` // 仅 settings.edit 管理员：完整连接配置（密码只有 hasPassword）
}

func (h *Hub) viewOf(in *instance, admin bool) clusterView {
	b := in.brief()
	v := clusterView{ID: in.spec.ID, Display: in.spec.Display, Desc: in.spec.Desc, Env: in.spec.Env, Mode: b.Mode, Primary: in.spec.Primary, Local: in.spec.Local,
		State: in.state, Status: in.statusOf(), Write: b.Write, Warnings: in.warns}
	if admin {
		cv := clusters.ViewOf(in.spec)
		v.Config = &cv
	} else {
		v.Status.Error, v.Warnings = "", nil // 启动错误 / 预检警告里可能含地址与路径，只给管理员
	}
	return v
}

func clusterStatusCode(code string) int {
	switch code {
	case clusters.ErrInvalid:
		return http.StatusBadRequest
	case clusters.ErrPrimary:
		return http.StatusForbidden
	case clusters.ErrNotFound:
		return http.StatusNotFound
	case clusters.ErrExists, clusters.ErrLimit, clusters.ErrInUse:
		return http.StatusConflict
	case clusters.ErrStart:
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// auditCluster 写一条集群管理审计：Env / Cluster 记被操作集群的值（未知时用主集群的）。
func (s *Server) auditCluster(r *http.Request, u authz.User, action, env, name, target, risk, summary, result string) {
	if env == "" {
		env = s.ops.cfg.Env
	}
	if name == "" {
		name = s.cfg.Col.Params.Cluster
	}
	role := ""
	if rs := u.RolesFor(s.ops.cfg.Env); len(rs) > 0 {
		var ns []string
		for _, x := range rs {
			ns = append(ns, string(x))
		}
		role = strings.Join(ns, ",")
	}
	_, _ = s.ops.cfg.Audit.Append(audit.Entry{User: u.Name, Role: role, IP: clientIP(r), Env: env, Cluster: name, Action: action, Target: target, Risk: risk, Summary: summary, Result: result})
}

func (s *Server) clusterFail(w http.ResponseWriter, r *http.Request, ru reqUser, action, id, risk string, err error) {
	var ce *clusters.Error
	if !errors.As(err, &ce) {
		ce = &clusters.Error{Code: clusters.ErrStore, Msg: err.Error()}
	}
	res := "denied"
	if ce.Code == clusters.ErrStore || ce.Code == clusters.ErrStart {
		res = "failed"
	}
	env, name := "", ""
	if sp, ok := s.hub.reg.Get(id); ok {
		env, name = sp.Env, sp.Name
	}
	s.auditCluster(r, ru.U, action, env, name, id, risk, ce.Msg, res)
	writeJSONCode(w, clusterStatusCode(ce.Code), map[string]any{"error": ce.Msg, "code": ce.Code})
}

// describe 是写进审计的接入摘要：只含地址与模式，不含任何凭据（密码只记「已保存」）。
func describe(sp clusters.Spec) string {
	parts := []string{fmt.Sprintf("env=%s mode=%s tiup 名=%s", sp.Env, sp.Mode, sp.Name)}
	if sp.Mode == clusters.ModeReplay {
		parts = append(parts, "fixtures="+sp.Fixtures)
	} else {
		for _, kv := range [][2]string{{"pd", sp.PD}, {"tidbStatus", sp.TiDBStatus}, {"prom", sp.Prom}, {"alert", sp.Alert}} {
			if kv[1] != "" {
				parts = append(parts, kv[0]+"="+kv[1])
			}
		}
		if sp.MySQLHost != "" {
			parts = append(parts, fmt.Sprintf("sql=%s@%s:%d", sp.MySQLUser, sp.MySQLHost, sp.MySQLPort))
		}
		if sp.HasPassword() {
			parts = append(parts, "已保存数据库密码")
		}
	}
	return strings.Join(parts, " ")
}

// diffSpec 返回更新改了哪些字段（只写字段名；密码只写「已更换」）。
func diffSpec(a, b clusters.Spec) []string {
	var out []string
	add := func(name string, x, y any) {
		if x != y {
			out = append(out, name)
		}
	}
	add("name", a.Name, b.Name)
	add("display", a.Display, b.Display)
	add("desc", a.Desc, b.Desc)
	add("env", a.Env, b.Env)
	add("mode", a.Mode, b.Mode)
	add("fixtures", a.Fixtures, b.Fixtures)
	add("pd", a.PD, b.PD)
	add("tidbStatus", a.TiDBStatus, b.TiDBStatus)
	add("prom", a.Prom, b.Prom)
	add("alert", a.Alert, b.Alert)
	add("mysqlHost", a.MySQLHost, b.MySQLHost)
	add("mysqlPort", a.MySQLPort, b.MySQLPort)
	add("mysqlUser", a.MySQLUser, b.MySQLUser)
	add("mysqlBin", a.MySQLBin, b.MySQLBin)
	add("tiup", a.Tiup, b.Tiup)
	add("home", a.Home, b.Home)
	if a.MySQLPassword != b.MySQLPassword || a.HasPassword() != b.HasPassword() {
		out = append(out, "mysqlPassword（已更换 / 清除）")
	}
	sort.Strings(out)
	return out
}

func (s *Server) registerClusters(mux *http.ServeMux) {
	o := s.ops
	h := s.hub
	if s.clusterID != h.primaryID {
		return // 集群管理只由主实例提供（Hub 也只会把 /api/clusters* 路由到主实例）
	}
	isAdmin := func(u authz.User) bool { return u.Can(o.cfg.Env, authz.CapSettings) }
	s.registerClusterImport(mux)

	mux.HandleFunc("GET /api/clusters", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		admin := isAdmin(ru.U)
		views := []clusterView{}
		for _, sp := range h.reg.List() {
			in := h.get(sp.ID)
			if in == nil || sp.Platform || (!admin && !visibleTo(ru.U, sp.Env)) { // 独立部署的平台实例不是集群，不列出
				continue
			}
			views = append(views, h.viewOf(in, admin))
		}
		primary := h.primaryID
		if h.Platform() {
			primary = ""
		}
		out := map[string]any{"clusters": views, "primary": primary, "standalone": h.Platform(), "localTiup": h.cfg.LocalTiup, "canManage": admin,
			"limits": map[string]any{"max": clusters.MaxClusters, "idPattern": clusters.IDPattern, "namePattern": clusters.NamePattern, "maxDisplay": clusters.MaxDisplayRunes, "maxDesc": clusters.MaxDescRunes}}
		if admin {
			out["fixturesRoots"] = h.reg.FixturesRoots()
			if ws := h.reg.Warnings(); len(ws) > 0 {
				out["warnings"] = ws
			}
		}
		writeJSON(w, out)
	}))

	mux.HandleFunc("POST /api/clusters", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in clusters.Input
		if !decodeBody(w, r, &in) {
			return
		}
		sp, warns, err := h.Add(in, ru.U.Name)
		if err != nil {
			s.clusterFail(w, r, ru, "cluster.add", in.ID, "mid", err)
			return
		}
		s.auditCluster(r, ru.U, "cluster.add", sp.Env, sp.Name, sp.ID, "mid", "接入集群「"+sp.Display+"」："+describe(sp), "success")
		writeJSON(w, map[string]any{"ok": true, "cluster": h.viewOf(h.get(sp.ID), true), "warnings": nonNil(warns)})
	}))

	mux.HandleFunc("POST /api/clusters/{id}", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		var in clusters.Input
		if !decodeBody(w, r, &in) {
			return
		}
		old, next, warns, err := h.Update(id, in, ru.U.Name)
		if err != nil {
			s.clusterFail(w, r, ru, "cluster.update", id, "mid", err)
			return
		}
		d := diffSpec(old, next)
		sum := "无变化"
		if len(d) > 0 {
			sum = "修改集群「" + next.Display + "」：" + strings.Join(d, "、")
		}
		s.auditCluster(r, ru.U, "cluster.update", next.Env, next.Name, id, "mid", sum, "success")
		writeJSON(w, map[string]any{"ok": true, "changed": d, "cluster": h.viewOf(h.get(id), true), "warnings": nonNil(warns)})
	}))

	mux.HandleFunc("POST /api/clusters/{id}/delete", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		sp, kept, err := h.Remove(id)
		if err != nil {
			s.clusterFail(w, r, ru, "cluster.remove", id, "high", err)
			return
		}
		sum := "移除集群「" + sp.Display + "」：" + describe(sp)
		if kept != "" {
			sum += "；运行态数据已改名保留为 " + kept
		}
		s.auditCluster(r, ru.U, "cluster.remove", sp.Env, sp.Name, id, "high", sum, "success")
		writeJSON(w, map[string]any{"ok": true, "id": id, "keptDir": kept})
	}))

	mux.HandleFunc("POST /api/clusters/{id}/test", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		sp, ok := h.reg.Get(id)
		if !ok {
			s.clusterFail(w, r, ru, "cluster.test", id, "mid", &clusters.Error{Code: clusters.ErrNotFound, Msg: "没有这个集群"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		items := h.cfg.Prober.Run(ctx, sp)
		var ok1, warn, bad int
		for _, it := range items {
			switch it.Status {
			case "ok":
				ok1++
			case "warn":
				warn++
			default:
				bad++
			}
		}
		res := "success"
		if bad > 0 {
			res = "failed"
		}
		s.auditCluster(r, ru.U, "cluster.test", sp.Env, sp.Name, id, "mid", fmt.Sprintf("连通性测试：通过 %d / 警告 %d / 异常 %d（共 %d 项，只读探测）", ok1, warn, bad, len(items)), res)
		writeJSON(w, map[string]any{"ok": bad == 0, "items": items})
	}))
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// loginMe 是登录响应里的 me：多集群时按缺省选择（主集群可见则主集群，否则第一个可见集群）计算 env / caps / write。
func (s *Server) loginMe(ru reqUser) meResp {
	if s.hub != nil {
		if in := s.hub.defaultInstance(ru.U); in != nil && in.srv != nil && in.srv != s {
			return in.srv.me(ru)
		}
	}
	return s.me(ru)
}

// auditFilter 由查询参数构造审计筛选：user / action / result / env / q / since / cluster（id 或 tiup 名）。
// 并且只返回调用者在该记录的 env 下有 audit.view 的记录（env 为空视为主集群 env）：持有 audit.view 的人看不到自己
// 没有该环境权限的集群的审计。单集群部署里所有记录都是同一个 env，行为与之前一致。
func (s *Server) auditFilter(r *http.Request, u authz.User) audit.Filter {
	q := r.URL.Query()
	f := audit.Filter{User: q.Get("user"), Action: q.Get("action"), Result: q.Get("result"), Env: q.Get("env"), Contains: q.Get("q")}
	if t, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
		f.Since = t
	}
	if c := q.Get("cluster"); c != "" {
		if s.hub != nil {
			f.Clusters = s.hub.reg.Resolve(c)
		} else {
			f.Clusters = []string{c}
		}
	}
	primaryEnv := s.ops.cfg.Env
	f.Allow = func(e *audit.Entry) bool {
		env := e.Env
		if env == "" {
			env = primaryEnv
		}
		return u.Can(env, authz.CapAuditView)
	}
	return f
}
