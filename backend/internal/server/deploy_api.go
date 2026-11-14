// @author: kevinkelly
// @license: Apache License 2.0
// @file: deploy_api.go
// @time: 2026/10/28

package server

// 一键部署接口（只在主实例上，/api/deploy*，全局端点）。流程：保存配置 → 预检 → 提交申请 → 另一名管理员批准 → 开始部署。
// 每个接口都要登录 + CSRF（写类）+ settings.edit（平台管理员）；批准的人不能是申请人，所以至少要两名管理员。
//
//	GET  /api/deploy/status                  是否可用（不可用时给原因）、当前运行中的任务
//	GET  /api/deploy/configs[?cluster=]      已保存的配置版本（不带 cluster = 有配置的集群名）
//	POST /api/deploy/configs                 保存配置（body = 表单字段，服务端校验并重新渲染 deploy.conf；内容相同不新建版本）
//	POST /api/deploy/precheck                对已保存的版本做只读预检 {cluster, ver, rootPassword}
//	GET  /api/deploy/jobs[?cluster=]         最近的任务
//	GET  /api/deploy/jobs/{id}?offset=N      任务状态 + 从偏移 N 起的输出（轮询用）
//	GET  /api/deploy/jobs/{id}/download      完整输出（文本附件）
//	POST /api/deploy/jobs/{id}/cancel        取消 {rootPassword}
//	POST /api/deploy/jobs/{id}/secret        取一次新集群的 TiDB root 随机密码（取后平台这一份即删）
//	GET  /api/deploy/requests                部署申请
//	POST /api/deploy/requests                提交申请 {precheck}（必须有一次成功的预检）
//	POST /api/deploy/requests/{id}/approve   批准（另一名管理员）
//	POST /api/deploy/requests/{id}/start     开始 / 从断点继续 {confirm, rootPassword}
//
// root 密码只出现在 precheck / start / cancel 的请求体里，只传给执行器一次；审计、日志、响应里都不含它。
// 错误码：bad_password 403（还剩次数在 left）| locked 429 | busy 409 | not_ready 409 | self_approve 403 | invalid 400 | not_found 404 | gone 410。

import (
	"errors"
	"net/http"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"

	"tidbops/internal/authz"
	"tidbops/internal/deploy"
)

type deployState struct {
	once sync.Once
	m    *deploy.Manager
	err  error
}

// deployMgr 返回部署管理器；不可用时返回原因。
func (s *Server) deployMgr() (*deploy.Manager, string) {
	if m := s.ops.cfg.Deploy; m != nil {
		return m, ""
	}
	if s.hub == nil || !s.hub.cfg.LocalTiup {
		return nil, "一键部署只在中控机模式下可用（安装平台时加 --standalone --local-tiup）"
	}
	if runtime.GOOS != "linux" {
		return nil, "一键部署需要 Linux 中控机（用 su 取得 root、systemd-run 起任务）"
	}
	if s.ops.cfg.Dir == "" {
		return nil, "需要数据目录（--data-dir）"
	}
	s.dep.once.Do(func() {
		run := "tidbops"
		if u, err := user.Current(); err == nil && u.Username != "" {
			run = u.Username
		}
		s.dep.m, s.dep.err = deploy.NewManager(deploy.Config{Dir: filepath.Join(s.ops.cfg.Dir, "deploy"), WebDir: s.cfg.Web, Runner: deploy.DefaultRootRunner(), RunUser: run, Now: s.ops.now})
	})
	if s.dep.err != nil {
		return nil, "无法初始化部署目录：" + s.dep.err.Error()
	}
	return s.dep.m, ""
}

type deployReq struct {
	Cluster      string `json:"cluster"`
	Ver          int    `json:"ver"`
	RootPassword string `json:"rootPassword"`
	Precheck     string `json:"precheck"`
	Confirm      string `json:"confirm"`
}

func (s *Server) deployFail(w http.ResponseWriter, err error) {
	var ve *deploy.ValidationError
	var bp *deploy.BadPasswordError
	var se *deploy.SuError
	switch {
	case errors.As(err, &ve):
		writeJSONCode(w, http.StatusBadRequest, map[string]any{"error": ve.Error(), "code": "invalid", "errors": ve.Errors})
	case errors.As(err, &bp):
		writeJSONCode(w, http.StatusForbidden, map[string]any{"error": bp.Error(), "code": "bad_password", "left": bp.Left})
	case errors.Is(err, deploy.ErrBadPassword):
		writeJSONCode(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "bad_password"})
	case errors.Is(err, deploy.ErrLocked):
		writeJSONCode(w, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "code": "locked"})
	case errors.Is(err, deploy.ErrBusy):
		writeJSONCode(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "busy"})
	case errors.Is(err, deploy.ErrNotReady):
		writeJSONCode(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "not_ready"})
	case errors.Is(err, deploy.ErrSelfApprove):
		writeJSONCode(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "self_approve"})
	case errors.Is(err, deploy.ErrNameMismatch), errors.Is(err, deploy.ErrNoPassword):
		writeJSONCode(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "invalid"})
	case errors.Is(err, deploy.ErrNotFound):
		writeJSONCode(w, http.StatusNotFound, map[string]any{"error": err.Error(), "code": "not_found"})
	case errors.Is(err, deploy.ErrSecretGone):
		writeJSONCode(w, http.StatusGone, map[string]any{"error": err.Error(), "code": "gone"})
	case errors.As(err, &se), errors.Is(err, deploy.ErrUnsupported):
		writeJSONCode(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "code": "su_failed"})
	default:
		writeJSONCode(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

func (s *Server) registerDeploy(mux *http.ServeMux) {
	// need 包装：取管理器，不可用时 503 并说明原因
	need := func(c authz.Cap, h func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager)) http.HandlerFunc {
		return s.secured(c, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
			m, why := s.deployMgr()
			if m == nil {
				writeJSONCode(w, http.StatusServiceUnavailable, map[string]any{"error": why, "code": "unavailable"})
				return
			}
			h(w, r, ru, m)
		})
	}
	mux.HandleFunc("GET /api/deploy/status", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		m, why := s.deployMgr()
		out := map[string]any{"available": m != nil}
		if m == nil {
			out["reason"] = why
			writeJSON(w, out)
			return
		}
		if ok, why := m.Available(); !ok {
			out["available"], out["reason"] = false, why
		}
		if j, busy := m.Running(); busy {
			out["running"] = j
		}
		out["canApprove"] = ru.U.Can(s.ops.cfg.Env, authz.CapSettings)
		writeJSON(w, out)
	}))

	mux.HandleFunc("GET /api/deploy/configs", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		c := r.URL.Query().Get("cluster")
		if c == "" {
			writeJSON(w, map[string]any{"clusters": m.Store.Clusters()})
			return
		}
		vs, err := m.Store.List(c)
		if err != nil {
			s.deployFail(w, err)
			return
		}
		type verOut struct {
			deploy.Version
			Conf string `json:"conf"` // 该版本渲染出的 deploy.conf（不含任何密码）
		}
		out := make([]verOut, 0, len(vs))
		for _, v := range vs {
			out = append(out, verOut{Version: v, Conf: v.Spec.Render()})
		}
		writeJSON(w, map[string]any{"versions": out})
	}))
	mux.HandleFunc("POST /api/deploy/configs", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		var sp deploy.Spec
		if !decodeBody(w, r, &sp) {
			return
		}
		v, created, err := m.Store.Save(sp, ru.U.Name, s.ops.now())
		if err != nil {
			var ve *deploy.ValidationError
			if errors.As(err, &ve) {
				s.auditReq(r, ru.U, "deploy.config.save", sp.Cluster, "low", "配置校验未通过："+ve.Error(), "denied")
			}
			s.deployFail(w, err)
			return
		}
		if created {
			s.auditReq(r, ru.U, "deploy.config.save", v.Cluster, "low", "保存部署配置 v"+strconv.Itoa(v.Ver)+"（sha256 "+v.SHA[:12]+"…）", "success")
		}
		writeJSON(w, map[string]any{"version": v, "created": created, "conf": v.Spec.Render()})
	}))

	mux.HandleFunc("POST /api/deploy/precheck", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		var b deployReq
		if !decodeBody(w, r, &b) {
			return
		}
		pw := []byte(b.RootPassword)
		b.RootPassword = ""
		j, err := m.StartPrecheck(r.Context(), ru.U.Name, b.Cluster, b.Ver, pw)
		if err != nil {
			s.auditReq(r, ru.U, "deploy.precheck", b.Cluster, "mid", "预检未启动："+firstLine(err.Error()), resultOf(err))
			s.deployFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "deploy.precheck", b.Cluster, "mid", "开始预检，任务 "+j.ID+"，配置 v"+strconv.Itoa(j.ConfVer), "success")
		writeJSON(w, map[string]any{"job": j})
	}))

	mux.HandleFunc("GET /api/deploy/jobs", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		writeJSON(w, map[string]any{"jobs": m.Jobs(r.URL.Query().Get("cluster"), 30)})
	}))
	mux.HandleFunc("GET /api/deploy/jobs/{id}", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		j, err := m.Job(r.PathValue("id"))
		if err != nil {
			s.deployFail(w, err)
			return
		}
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		log, next, err := m.Log(j.ID, off)
		if err != nil {
			s.deployFail(w, err)
			return
		}
		writeJSON(w, map[string]any{"job": j, "log": log, "offset": next})
	}))
	mux.HandleFunc("GET /api/deploy/jobs/{id}/download", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		j, err := m.Job(r.PathValue("id"))
		if err != nil {
			s.deployFail(w, err)
			return
		}
		var all []byte
		var off int64
		for i := 0; i < 400; i++ { // 最多 100MB
			chunk, next, err := m.Log(j.ID, off)
			if err != nil {
				s.deployFail(w, err)
				return
			}
			all = append(all, chunk...)
			if next == off {
				break
			}
			off = next
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="deploy-`+j.Cluster+`-`+j.ID+`.log"`)
		_, _ = w.Write(all)
	}))
	mux.HandleFunc("POST /api/deploy/jobs/{id}/cancel", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		var b deployReq
		if !decodeBody(w, r, &b) {
			return
		}
		pw := []byte(b.RootPassword)
		b.RootPassword = ""
		j, err := m.Cancel(r.Context(), ru.U.Name, r.PathValue("id"), pw)
		if err != nil {
			s.auditReq(r, ru.U, "deploy.cancel", r.PathValue("id"), "high", "取消未成功："+firstLine(err.Error()), resultOf(err))
			s.deployFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "deploy.cancel", j.ID, "high", "取消任务（集群 "+j.Cluster+"）", "success")
		writeJSON(w, map[string]any{"job": j})
	}))
	mux.HandleFunc("POST /api/deploy/jobs/{id}/secret", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		pw, err := m.RevealSecret(r.PathValue("id"))
		if err != nil {
			s.deployFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "deploy.secret.reveal", r.PathValue("id"), "high", "显示了新集群的 TiDB root 随机密码（只能显示一次）", "success")
		writeJSON(w, map[string]any{"password": pw})
	}))

	mux.HandleFunc("GET /api/deploy/requests", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		writeJSON(w, map[string]any{"requests": m.Requests()})
	}))
	mux.HandleFunc("POST /api/deploy/requests", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		var b deployReq
		if !decodeBody(w, r, &b) {
			return
		}
		req, err := m.RequestDeploy(ru.U.Name, b.Precheck)
		if err != nil {
			s.auditReq(r, ru.U, "deploy.request", b.Precheck, "high", "提交部署申请未成功："+firstLine(err.Error()), resultOf(err))
			s.deployFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "deploy.request", req.ID, "high", "提交部署申请：集群 "+req.Cluster+"，配置 v"+strconv.Itoa(req.ConfVer), "success")
		writeJSON(w, map[string]any{"request": req})
	}))
	mux.HandleFunc("POST /api/deploy/requests/{id}/approve", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		req, err := m.Approve(ru.U.Name, r.PathValue("id"))
		if err != nil {
			s.auditReq(r, ru.U, "deploy.approve", r.PathValue("id"), "high", "批准未成功："+firstLine(err.Error()), resultOf(err))
			s.deployFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "deploy.approve", req.ID, "high", "批准部署申请：集群 "+req.Cluster+"，申请人 "+req.By, "success")
		writeJSON(w, map[string]any{"request": req})
	}))
	mux.HandleFunc("POST /api/deploy/requests/{id}/start", need(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser, m *deploy.Manager) {
		var b deployReq
		if !decodeBody(w, r, &b) {
			return
		}
		pw := []byte(b.RootPassword)
		b.RootPassword = ""
		j, err := m.StartDeploy(r.Context(), ru.U.Name, r.PathValue("id"), b.Confirm, pw)
		if err != nil {
			s.auditReq(r, ru.U, "deploy.start", r.PathValue("id"), "high", "部署未启动："+firstLine(err.Error()), resultOf(err))
			s.deployFail(w, err)
			return
		}
		s.auditReq(r, ru.U, "deploy.start", j.ID, "high", "开始部署集群 "+j.Cluster+"（配置 v"+strconv.Itoa(j.ConfVer)+"，申请 "+j.Request+"），经 su 以 root 运行", "success")
		writeJSON(w, map[string]any{"job": j})
	}))
}

func resultOf(err error) string {
	if errors.Is(err, deploy.ErrBadPassword) || errors.Is(err, deploy.ErrLocked) || errors.Is(err, deploy.ErrSelfApprove) || errors.Is(err, deploy.ErrNotReady) || errors.Is(err, deploy.ErrBusy) || errors.Is(err, deploy.ErrNameMismatch) {
		return "denied"
	}
	return "failed"
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
