// @author: kevinkelly
// @license: Apache License 2.0
// @file: shell_api.go
// @time: 2026/10/28

package server

// 「中控机命令」接口（只在主实例上，/api/shell*，全局端点）：运维人员在页面里向中控机发命令、看输出。
// 命令先按风险分级（只读 / 变更 / 禁止，见 internal/shell）；只读直接执行，变更需要另一名管理员批准（批准只对这一批命令、
// 这个目录有效，且只能用一次），禁止的一律拒绝。一律以 tidb 身份执行、不需要 root 密码：经 sudoers 放行的固定入口
// `sudo -n -u tidb <bin> shell-exec --stdin`——只读命令在该子进程里执行（仍在服务沙箱内），变更命令由它用 tidb 的 sudo 免密起
// systemd-run 临时单元（沙箱之外）；需要 root 权限的命令在整行开头写 sudo。
//
//	GET  /api/shell/status                   是否可用、中控机信息（主机名 / 系统 / tidb 家目录 = 默认工作目录 / 执行通道与 sudo 免密探测）、当前设置
//	POST /api/shell/check                    只判定不执行：返回整批与逐条的风险级别、原因、将走的执行路径（页面输入时实时调用）
//	POST /api/shell/run                      执行 {cwd, commands[], timeout, stopOnError, request?}
//	GET  /api/shell/requests                 变更申请
//	POST /api/shell/requests                 提交变更申请 {…同 run…, reason}
//	POST /api/shell/requests/{id}/approve    批准（另一名管理员）
//	GET  /api/shell/jobs                     执行历史（命令已打码）
//	GET  /api/shell/jobs/{id}?offset=N       任务状态 + 从偏移 N 起的输出（已遮蔽）；看别人的输出需要 audit.view，并写 shell.output.view 审计
//	GET  /api/shell/jobs/{id}/download       完整输出（文本附件，已遮蔽；记审计）
//	POST /api/shell/jobs/{id}/cancel         取消
//	GET  /api/shell/settings                 设置（策略档位 / 输出保留 / 追加的可读目录 / 只读命令是否也走临时单元）
//	POST /api/shell/settings                 修改设置（settings.edit，写审计 high）
//
// 权限（在「角色权限」页授予给角色，再把角色分配给用户）：执行 / 提交申请 / 取消 = shell.run；批准他人的变更申请 = shell.approve；
// 改策略设置 = settings.edit（仅内置管理员）；查看历史与输出 = shell.run、shell.approve 或 audit.view（看别人的输出另需 audit.view）。
// 内置管理员角色默认同时拥有 shell.run 与 shell.approve（与之前由 settings.edit 控制时的人员范围一致）。
// 错误码：denied 403（被护栏拒绝，带逐条原因）| need_approval 409 | busy 409 | not_ready 409 | self_approve 403 | invalid 400 | not_found 404 |
// unavailable 503（没有执行通道）| agent_failed 502（sudo 入口 / 子进程 / systemd-run 失败）。

import (
	"errors"
	"net/http"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"tidbops/internal/authz"
	"tidbops/internal/shell"
)

type shellState struct {
	once sync.Once
	m    *shell.Manager
	err  error
}

// shellMgr 返回命令管理器；不可用时返回原因。
func (s *Server) shellMgr() (*shell.Manager, string) {
	if m := s.ops.cfg.Shell; m != nil {
		return m, ""
	}
	if s.hub == nil || !s.hub.cfg.LocalTiup {
		return nil, "中控机命令只在中控机模式下可用（安装平台时加 --standalone --local-tiup）"
	}
	if runtime.GOOS != "linux" {
		return nil, "中控机命令需要 Linux 中控机（经 sudo 以 tidb 身份执行、systemd-run 起任务）"
	}
	if s.ops.cfg.Dir == "" {
		return nil, "需要数据目录（--data-dir）"
	}
	if s.ops.cfg.ShellAgent == nil {
		return nil, "中控机命令需要执行器为 --exec sudo（sudoers 放行的 shell-exec 入口）"
	}
	s.sh.once.Do(func() {
		run := "tidbops"
		if u, err := user.Current(); err == nil && u.Username != "" {
			run = u.Username
		}
		s.sh.m, s.sh.err = shell.NewManager(shell.Config{Dir: filepath.Join(s.ops.cfg.Dir, "shell"), Agent: s.ops.cfg.ShellAgent, RunUser: run, Now: s.ops.now})
	})
	if s.sh.err != nil {
		return nil, "无法初始化命令目录：" + s.sh.err.Error()
	}
	return s.sh.m, ""
}

func (s *Server) shellFail(w http.ResponseWriter, err error) {
	var de *shell.DeniedError
	var ie *shell.InvalidError
	var ae *shell.AgentError
	switch {
	case errors.As(err, &de):
		writeJSONCode(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "denied", "verdict": de.Verdict})
	case errors.As(err, &ie):
		writeJSONCode(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "invalid"})
	case errors.Is(err, shell.ErrNeedApproval):
		writeJSONCode(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "need_approval"})
	case errors.Is(err, shell.ErrBusy):
		writeJSONCode(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "busy"})
	case errors.Is(err, shell.ErrNotReady):
		writeJSONCode(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "not_ready"})
	case errors.Is(err, shell.ErrSelfApprove):
		writeJSONCode(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "self_approve"})
	case errors.Is(err, shell.ErrNotFound):
		writeJSONCode(w, http.StatusNotFound, map[string]any{"error": err.Error(), "code": "not_found"})
	case errors.Is(err, shell.ErrNoAgent):
		writeJSONCode(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error(), "code": "unavailable"})
	case errors.As(err, &ae):
		writeJSONCode(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "code": "agent_failed"})
	default:
		writeJSONCode(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

func joinLines(items []shell.Item) string {
	var l []string
	for _, it := range items {
		l = append(l, it.Line)
	}
	return strings.Join(l, " ⏎ ")
}

func (s *Server) registerShell(mux *http.ServeMux) {
	env := s.ops.cfg.Env
	canUse := func(u authz.User) bool { return u.Can(env, authz.CapShellRun) }
	canApprove := func(u authz.User) bool { return u.Can(env, authz.CapShellApprove) }
	canSettings := func(u authz.User) bool { return u.Can(env, authz.CapSettings) }
	canView := func(u authz.User) bool {
		return canUse(u) || canApprove(u) || u.Can(env, authz.CapAuditView)
	}
	// 接口所需能力（kind）：run 执行 / 申请 / 取消；approve 批准；settings 改策略设置；view 只看
	const (
		kRun, kApprove, kSettings, kView = "run", "approve", "settings", "view"
	)
	need := map[string]struct {
		ok   func(authz.User) bool
		desc string
	}{
		kRun:      {canUse, "缺少能力 shell.run（中控机命令：执行）"},
		kApprove:  {canApprove, "缺少能力 shell.approve（中控机命令：批准变更）"},
		kSettings: {canSettings, "缺少能力 settings.edit（修改平台设置）"},
		kView:     {canView, "缺少能力 shell.run、shell.approve 或 audit.view"},
	}
	// guard：登录 + CSRF（写类）+ 能力；取管理器，不可用时 503 并说明原因
	guard := func(kind string, h func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager)) http.HandlerFunc {
		return s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
			if !need[kind].ok(ru.U) {
				s.auditReq(r, ru.U, "authz.deny", r.URL.Path, "", need[kind].desc, "denied")
				writeErr(w, http.StatusForbidden, "没有权限："+need[kind].desc)
				return
			}
			m, why := s.shellMgr()
			if m == nil {
				writeJSONCode(w, http.StatusServiceUnavailable, map[string]any{"error": why, "code": "unavailable"})
				return
			}
			h(w, r, ru, m)
		})
	}
	mux.HandleFunc("GET /api/shell/status", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		if !canView(ru.U) {
			writeJSON(w, map[string]any{"available": false, "reason": ""})
			return
		}
		m, why := s.shellMgr()
		if m == nil {
			writeJSON(w, map[string]any{"available": false, "reason": why})
			return
		}
		writeJSON(w, map[string]any{"available": true, "canRun": canUse(ru.U), "canApprove": canApprove(ru.U), "canSettings": canSettings(ru.U), "info": m.Info(), "me": ru.U.Name})
	}))

	mux.HandleFunc("POST /api/shell/check", guard(kRun, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		var b shell.RunReq
		if !decodeBody(w, r, &b) {
			return
		}
		v, req, err := m.Check(b)
		if err != nil {
			s.shellFail(w, err)
			return
		}
		writeJSON(w, map[string]any{"verdict": v, "request": req})
	}))
	mux.HandleFunc("POST /api/shell/run", guard(kRun, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		var b shell.RunReq
		if !decodeBody(w, r, &b) {
			return
		}
		j, err := m.Run(r.Context(), ru.U.Name, b)
		if err != nil {
			var de *shell.DeniedError
			sum := "未执行：" + firstLine(err.Error())
			if errors.As(err, &de) {
				sum = "被拒绝：" + de.Verdict.Why + "；命令：" + joinLines(de.Verdict.Items)
			}
			if j.ID != "" { // 任务已建、启动失败：审计里带上任务号
				sum = "任务 " + j.ID + " 启动失败：" + firstLine(err.Error())
			}
			s.auditReq(r, ru.U, "shell.run", j.ID, "high", sum, shellResult(err))
			s.shellFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "shell.run", j.ID, shellRisk(j.Level), "路径 "+shellRoute(j)+"，目录 "+j.Cwd+"，"+strconv.Itoa(len(j.Lines))+" 条：「"+joinLines(j.Lines)+"」"+shellApprover(j), "success")
		writeJSON(w, map[string]any{"job": j})
	}))

	mux.HandleFunc("GET /api/shell/requests", guard(kView, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		writeJSON(w, map[string]any{"requests": m.Requests()})
	}))
	mux.HandleFunc("POST /api/shell/requests", guard(kRun, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		var b shell.RunReq
		if !decodeBody(w, r, &b) {
			return
		}
		q, err := m.RequestApproval(ru.U.Name, b)
		if err != nil {
			s.auditReq(r, ru.U, "shell.request", "", "high", "变更申请未成功："+firstLine(err.Error()), shellResult(err))
			s.shellFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "shell.request", q.ID, "high", "目录 "+q.Cwd+"：「"+joinLines(q.Lines)+"」；原因："+q.Reason, "success")
		writeJSON(w, map[string]any{"request": q})
	}))
	mux.HandleFunc("POST /api/shell/requests/{id}/approve", guard(kApprove, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		q, err := m.Approve(ru.U.Name, r.PathValue("id"))
		if err != nil {
			s.auditReq(r, ru.U, "shell.approve", r.PathValue("id"), "high", "批准未成功："+firstLine(err.Error()), shellResult(err))
			s.shellFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "shell.approve", q.ID, "high", "批准变更命令：申请人 "+q.By+"，「"+joinLines(q.Lines)+"」", "success")
		writeJSON(w, map[string]any{"request": q})
	}))

	mux.HandleFunc("GET /api/shell/jobs", guard(kView, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		writeJSON(w, map[string]any{"jobs": m.Jobs(30)})
	}))
	// 取任务并检查「看别人的输出」的权限
	viewJob := func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager, audit bool) (shell.Job, bool) {
		j, err := m.Job(r.PathValue("id"))
		if err != nil {
			s.shellFail(w, err)
			return j, false
		}
		if j.By != ru.U.Name {
			if !ru.U.Can(s.ops.cfg.Env, authz.CapAuditView) {
				s.auditReq(r, ru.U, "shell.output.view", j.ID, "mid", "无权查看 "+j.By+" 的输出", "denied")
				writeErr(w, http.StatusForbidden, "查看别人的命令输出需要 audit.view")
				return j, false
			}
			if audit {
				s.auditReq(r, ru.U, "shell.output.view", j.ID, "mid", "查看了 "+j.By+" 的命令输出", "success")
			}
		}
		return j, true
	}
	mux.HandleFunc("GET /api/shell/jobs/{id}", guard(kView, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		j, ok := viewJob(w, r, ru, m, off == 0)
		if !ok {
			return
		}
		text, next, masks, err := m.Log(j.ID, off)
		if err != nil {
			s.shellFail(w, err)
			return
		}
		writeJSON(w, map[string]any{"job": j, "log": text, "offset": next, "masks": masks})
	}))
	mux.HandleFunc("GET /api/shell/jobs/{id}/download", guard(kView, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		j, ok := viewJob(w, r, ru, m, false)
		if !ok {
			return
		}
		var all strings.Builder
		var off int64
		for i := 0; i < 8; i++ { // 输出上限 1 MiB，最多 4 块
			chunk, next, _, err := m.Log(j.ID, off)
			if err != nil {
				s.shellFail(w, err)
				return
			}
			all.WriteString(chunk)
			if next == off {
				break
			}
			off = next
		}
		s.auditReq(r, ru.U, "shell.output.download", j.ID, "mid", "下载了 "+j.By+" 的命令输出（已遮蔽）", "success")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="shell-`+j.ID+`.log"`)
		_, _ = w.Write([]byte(all.String()))
	}))
	mux.HandleFunc("POST /api/shell/jobs/{id}/cancel", guard(kRun, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		j, err := m.Cancel(r.Context(), ru.U.Name, r.PathValue("id"))
		if err != nil {
			s.auditReq(r, ru.U, "shell.cancel", r.PathValue("id"), "high", "取消未成功："+firstLine(err.Error()), shellResult(err))
			s.shellFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "shell.cancel", j.ID, "high", "取消任务（"+j.By+" 的命令）", "success")
		writeJSON(w, map[string]any{"job": j})
	}))

	mux.HandleFunc("GET /api/shell/settings", guard(kView, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		writeJSON(w, map[string]any{"settings": m.Settings()})
	}))
	mux.HandleFunc("POST /api/shell/settings", guard(kSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *shell.Manager) {
		var st shell.Settings
		if !decodeBody(w, r, &st) {
			return
		}
		old := m.Settings()
		v, err := m.SetSettings(st)
		if err != nil {
			s.auditReq(r, ru.U, "shell.settings", "", "high", "设置未保存："+firstLine(err.Error()), shellResult(err))
			s.shellFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "shell.settings", "", "high", "策略 "+old.Policy+" → "+v.Policy+"；保留 "+strconv.Itoa(old.RetainDays)+" → "+strconv.Itoa(v.RetainDays)+" 天；只存摘要 "+strconv.FormatBool(v.SummaryOnly)+
			"；只读命令走临时单元 "+strconv.FormatBool(v.ROViaUnit)+"；追加可读目录 "+strings.Join(v.ReadRoots, "、"), "success")
		writeJSON(w, map[string]any{"settings": v})
	}))
}

func shellRisk(l shell.Level) string {
	if l == shell.LevelRO {
		return "low"
	}
	return "high"
}

func shellRoute(j shell.Job) string {
	if j.Route == shell.RouteUnit {
		return "临时单元"
	}
	return "平台进程内"
}

func shellApprover(j shell.Job) string {
	if j.Approver == "" {
		return ""
	}
	return "；申请 " + j.Request + "，原因：" + j.Reason + "，批准人 " + j.Approver
}

func shellResult(err error) string {
	var de *shell.DeniedError
	var ie *shell.InvalidError
	if errors.As(err, &de) || errors.As(err, &ie) || errors.Is(err, shell.ErrSelfApprove) ||
		errors.Is(err, shell.ErrNotReady) || errors.Is(err, shell.ErrBusy) || errors.Is(err, shell.ErrNeedApproval) {
		return "denied"
	}
	return "failed"
}
