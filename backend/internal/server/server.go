// @author: kevinkelly
// @license: Apache License 2.0
// @file: server.go
// @time: 2026/10/28

// Package server 提供平台的只读 HTTP 接口，并托管前端静态页面。
//
// 安全基线（对应 A3）：只接受 GET / HEAD；默认只监听本机；静态文件走白名单（不会暴露 fixtures、源码、配置）；
// 响应带安全头；接口响应不缓存；没有任何写操作入口。
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tidbops/internal/authz"
	"tidbops/internal/buildinfo"
	"tidbops/internal/collect"
	"tidbops/internal/model"
)

// Version 是服务版本：发布构建由 -ldflags 注入 buildinfo.Version；开发构建（未注入）显示 0.3.0-dev。
var Version = func() string {
	if buildinfo.Version != "" && buildinfo.Version != "dev" {
		return buildinfo.Version
	}
	return "0.3.0-dev"
}()

// Config 是服务配置。
type Config struct {
	Col      *collect.Collector
	Mode     string        // replay | live
	Web      string        // 前端目录
	SnapTTL  time.Duration // 快照缓存时间
	Timeout  time.Duration // 单次采集总超时
	Logf     func(format string, args ...any)
	Fixtures string // replay 模式下的数据包目录（仅用于展示）
	ICP      string // 备案号（来自配置文件，仅用于页面展示；空表示不显示）
	// Unavailable 非 nil 且返回 true 时，写操作闸门（gate）一律拒绝，定时变更也不会再开始执行。
	// 由进程级的使用期限判断注入（见 internal/expiry）；nil 表示不限制。
	Unavailable func() bool
}

// Server 是 HTTP 服务。
type Server struct {
	cfg Config
	web http.Handler

	mu   sync.Mutex
	snap *model.Snapshot
	at   time.Time
	pub  atomic.Pointer[model.Snapshot] // 与 snap 同步发布的只读副本：状态查询读它，不必等正在进行的采集

	ops *ops // 运维模式（登录 / 审批 / 变更执行）；nil 表示纯只读

	hub       *Hub   // 多集群：所属的 Hub（单集群部署为 nil）
	clusterID string // 多集群：本实例的集群 id（单集群部署为空）

	insights insightCache // 洞察接口（配置当前值、容量预测）的小缓存

	dep deployState // 一键部署管理器（按需创建，见 deploy_api.go）
	sh  shellState  // 中控机命令管理器（按需创建，见 shell_api.go）
}

// New 创建服务。
func New(cfg Config) *Server {
	if cfg.SnapTTL <= 0 {
		cfg.SnapTTL = 10 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Server{cfg: cfg, web: http.FileServer(http.Dir(cfg.Web))}
}

// Snapshot 返回（带缓存的）快照。force 为 true 时跳过缓存，但最短间隔 2 秒，防止被刷新请求放大。
func (s *Server) Snapshot(ctx context.Context, force bool) model.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	age := time.Since(s.at)
	if s.snap != nil && (age < s.cfg.SnapTTL && !force || age < 2*time.Second) {
		return *s.snap
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	snap := s.cfg.Col.Snapshot(ctx, s.cfg.Mode)
	s.snap, s.at = &snap, time.Now()
	s.pub.Store(s.snap)
	return snap
}

// cachedSnapshot 返回最近一次采集完成的快照（不触发采集，也不会因为另一个请求正在采集而阻塞）。
func (s *Server) cachedSnapshot() (model.Snapshot, bool) {
	if p := s.pub.Load(); p != nil {
		return *p, true
	}
	return model.Snapshot{}, false
}

var (
	reRootAsset = regexp.MustCompile(`^/((js|css)/)?[A-Za-z0-9_-]+\.(js|css)$`) // 前端目录布局：js/*.js、css/*.css（根下的 *.js / *.css 也放行）
	reDownload  = regexp.MustCompile(`^/deploy/dist/tidb-deploy-[0-9.]+\.tar\.gz(\.sha256)?$`)
	reHelp      = regexp.MustCompile(`^/help/[a-z]+\.md$`) // 使用帮助（help/<主题>.md，纯文档，不含任何配置或数据）
)

// staticAllowed 是静态文件白名单：首页、js/ 与 css/ 下（以及根目录下）的 js / css、使用帮助文档，以及部署工具包。
func staticAllowed(p string) bool {
	return p == "/" || p == "/index.html" || reRootAsset.MatchString(p) || reHelp.MatchString(p) || reDownload.MatchString(p)
}

// Handler 返回完整的 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		h := map[string]any{"ok": true, "mode": s.cfg.Mode, "version": Version, "commit": buildinfo.Commit, "buildTime": buildinfo.BuildTime, "readOnly": s.ops == nil, "fixtures": s.cfg.Fixtures, "now": time.Now()}
		if s.cfg.ICP != "" {
			h["icp"] = s.cfg.ICP // 备案号：登录页与页脚展示（未登录也要能取到，所以放在 health 里）
		}
		if s.ops != nil {
			h["ops"] = map[string]any{"enabled": true, "env": s.ops.cfg.Env, "exec": s.ops.cfg.ExecMode, "sim": s.ops.cfg.Sim, "demo": s.ops.cfg.Demo}
		}
		if s.hub != nil && s.hub.Platform() {
			h["standalone"] = true               // 独立部署：平台本身不代表任何集群（前端据此显示「尚未接入集群」引导）
			h["localTiup"] = s.hub.cfg.LocalTiup // 中控机模式：引导里提示「从本机 tiup 导入」
		}
		if s.hub != nil {
			if b, ok := s.hub.brief(s.clusterID); ok {
				h["cluster"] = b
			}
		}
		writeJSON(w, h)
	})
	if s.ops != nil {
		s.registerOps(mux)
		s.registerOpsExtra(mux)
		s.registerNotify(mux)
		s.registerWindows(mux)
		s.registerLB(mux)
		s.registerWatermark(mux)
		if s.hub != nil {
			s.registerClusters(mux) // 集群接入（clusters_api.go）：只在主实例上有意义，由 Hub 路由
		}
		if s.hub != nil || s.ops.cfg.Deploy != nil {
			s.registerDeploy(mux) // 一键部署（deploy_api.go）：全局端点，由主实例处理
		}
		if s.hub != nil || s.ops.cfg.Shell != nil {
			s.registerShell(mux) // 中控机命令（shell_api.go）：全局端点，由主实例处理
		}
	}
	// read 在运维模式下要求登录（并按角色裁剪），否则直接放行
	read := func(c authz.Cap, f func(w http.ResponseWriter, r *http.Request, u *authz.User)) http.HandlerFunc {
		if s.ops == nil {
			return func(w http.ResponseWriter, r *http.Request) { f(w, r, nil) }
		}
		return s.secured(c, func(w http.ResponseWriter, r *http.Request, ru reqUser) { f(w, r, &ru.U) })
	}
	snapFor := func(r *http.Request, u *authz.User, force bool) model.Snapshot {
		snap := s.Snapshot(r.Context(), force)
		if u != nil {
			snap = s.filterSnapshot(snap, *u)
		}
		return snap
	}
	mux.HandleFunc("/api/snapshot", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		writeJSON(w, snapFor(r, u, r.URL.Query().Get("refresh") == "1"))
	}))
	part := func(name string, pick func(model.Snapshot) any) {
		mux.HandleFunc("/api/"+name, read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) { writeJSON(w, pick(snapFor(r, u, false))) }))
	}
	part("topology", func(m model.Snapshot) any { return m.Topology })
	part("overview", func(m model.Snapshot) any { return map[string]any{"overview": m.Overview, "trends": m.Trends} })
	part("slow", func(m model.Snapshot) any { return m.Slow })
	part("hot", func(m model.Snapshot) any { return m.Hot })
	part("alerts", func(m model.Snapshot) any { return m.Alerts })
	part("findings", func(m model.Snapshot) any { return m.Findings })
	part("env", func(m model.Snapshot) any { return m.Env })
	s.registerInsights(mux, read)
	s.registerWizard(mux, read)
	mux.HandleFunc("/api/params", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		writeJSON(w, map[string]any{"params": s.cfg.Col.ParamRows(r.Context())})
	}))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !staticAllowed(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/help/") {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		}
		if strings.HasPrefix(r.URL.Path, "/deploy/") {
			w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(r.URL.Path)+`"`)
		}
		s.web.ServeHTTP(w, r)
	})
	return s.guard(mux)
}

// guard 统一加：只读方法限制、安全头、恢复 panic、简单访问日志。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		} else {
			// 静态页面与脚本：每次向服务端校验（Last-Modified），避免升级后浏览器继续用旧脚本
			h.Set("Cache-Control", "no-cache")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !(s.ops != nil && r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/")) {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "只读服务：仅支持 GET / HEAD", http.StatusMethodNotAllowed)
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				s.cfg.Logf("panic: %v", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
