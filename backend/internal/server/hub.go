// @author: kevinkelly
// @license: Apache License 2.0
// @file: hub.go
// @time: 2026/10/28

package server

// 多集群：Hub + 每个集群一个 Server 实例。
//
// 一个进程可以纳管多个 TiDB 集群。每个集群有自己的 Server 实例（采集器 + 运维状态：变更引擎与变更库、写开关、窗口 / 冻结期、
// 通知渠道、主机池……），Hub 按请求头 X-Cluster（或查询参数 ?cluster=）把请求路由到对应实例，缺省 = 主集群。
// 用户库、角色库、审计日志（同一条哈希链）、会话表（含 CSRF 派生用的 Key）、限速计数、审批策略是全局的，
// 所有实例共享同一份对象，所以一次登录在全部集群通用，退出 / 吊销立即对全部集群生效。
//
// 端点分类（详见 docs/运维部署手册.md 6.3 与「集群接入」帮助）：
//
//	全局端点（始终由主集群实例处理，与 X-Cluster 无关）：
//	  /api/health（除外：已登录且带 X-Cluster 时返回该集群的值）、/api/auth/*（me 同理）、/api/users*、/api/roles*、
//	  /api/audit*（支持 cluster 筛选）、/api/version、/api/ops/version、/api/ops/proposals*、/api/ops/policy、/api/clusters*、静态与帮助文件
//	集群端点（按 X-Cluster 路由）：除上面以外的全部 /api/ 路径
//
// 授权：集群端点先由 Hub 做可见性判定（登录用户在该集群 env 下有 view.overview），不可见 / 不存在 / 大小写变形 / 重复头
// 一律返回同一个 404，不泄露集群是否存在；通过后由实例自己的 secured() 再按该集群 env 判定具体能力。
//
// 并发：实例表由 RWMutex 保护；添加 / 更新 / 删除由 opMu 串行；热生效 = 先构造实例，成功后才持久化并换入，失败回滚且不影响其他集群。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/clusters"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
)

// Backend 是某个集群实例的后端：采集器，以及运维模式下的执行器等。由 BackendFactory 按集群定义构造。
type Backend struct {
	Col      *collect.Collector
	Mode     string // replay | live
	Fixtures string // 回放数据包目录（仅展示）
	SnapTTL  time.Duration
	Timeout  time.Duration
	// Ops 在实例创建后、EnableOps 之前调用：按该集群设置 Runner / Prober / ExecMode / Sim / BackupRoots 等。nil = 不配执行器。
	Ops func(s *Server, cfg *OpsConfig) error
	// Preflight 在实例启动后调用，返回可读的警告（例如 sudo 模式下特权端的策略文件拒绝该集群名）。可为 nil。
	Preflight func(ctx context.Context, s *Server) []string
}

// BackendFactory 按集群定义构造后端。失败返回错误（实例标记为 error，不影响其他集群）。
type BackendFactory func(spec clusters.Spec) (Backend, error)

// HubConfig 是 Hub 的配置。
type HubConfig struct {
	Dir           string        // 数据目录；空 = 仅内存（注册表与各集群数据都不落盘）
	Primary       clusters.Spec // 主集群虚拟条目（id / name / env / mode / fixtures / 连接信息来自启动参数）
	FixturesRoots []string      // 回放数据包白名单根目录
	Factory       BackendFactory
	Prober        clusters.Prober // 连通性测试（零值可用）
	Warm          bool            // 构造 / 启动后在后台做一次首次采集（main 开启；测试关闭）
	LocalTiup     bool            // 本机是 tiup 中控机：允许一键导入本机 tiup 管理的集群（独立部署 + --local-tiup）
}

type instance struct {
	spec  clusters.Spec
	srv   *Server
	h     http.Handler
	state string // ok | error | loading
	err   string
	warns []string
}

// Hub 是多集群路由器。
type Hub struct {
	cfg       HubConfig
	primary   *Server
	primaryID string
	reg       *clusters.Registry

	opMu sync.Mutex // 串行化 添加 / 更新 / 删除

	mu     sync.RWMutex
	inst   map[string]*instance
	router http.Handler
}

// NewHub 为已启用运维模式的主集群实例创建 Hub：读取 clusters.json，逐个构造其余集群的实例。
// 单个集群构造失败不会让 NewHub 失败：该集群标记为 error（见 /api/clusters），并写日志与审计。
func NewHub(primary *Server, cfg HubConfig) (*Hub, error) {
	if primary == nil || primary.ops == nil {
		return nil, errors.New("多集群需要运维模式（--data-dir）")
	}
	if cfg.Factory == nil {
		return nil, errors.New("缺少集群后端工厂")
	}
	regPath := ""
	if cfg.Dir != "" {
		regPath = filepath.Join(cfg.Dir, "clusters.json")
	}
	reg, err := clusters.Open(regPath, primary.ops.cfg.MasterKey, cfg.Primary, clusters.Options{FixturesRoots: cfg.FixturesRoots, Now: primary.ops.cfg.Now, RequirePD: cfg.Primary.Platform, LocalTiup: cfg.LocalTiup})
	if err != nil {
		return nil, fmt.Errorf("读取 clusters.json 失败: %w", err)
	}
	h := &Hub{cfg: cfg, primary: primary, reg: reg, primaryID: reg.Primary().ID, inst: map[string]*instance{}}
	primary.hub, primary.clusterID = h, h.primaryID
	h.inst[h.primaryID] = &instance{spec: reg.Primary(), srv: primary, h: primary.Handler(), state: "ok"}
	for _, w := range reg.Warnings() {
		primary.cfg.Logf("clusters.json：%s", w)
	}
	for _, sp := range reg.List() {
		if sp.Primary {
			continue
		}
		h.inst[sp.ID] = h.startInstance(sp)
	}
	h.router = h.buildRouter()
	if cfg.Warm {
		h.Warm()
	}
	return h, nil
}

// startInstance 构造一个实例；失败返回 error 状态的占位（并写日志 / 审计）。
func (h *Hub) startInstance(sp clusters.Spec) *instance {
	srv, warns, err := h.build(sp)
	if err != nil {
		h.primary.cfg.Logf("集群 %s 启动失败：%v", sp.ID, err)
		_, _ = h.primary.ops.cfg.Audit.Append(auditEntry("system", sp, "cluster.load", "high", "集群实例启动失败："+err.Error(), "failed"))
		return &instance{spec: sp, state: "error", err: err.Error()}
	}
	return &instance{spec: sp, srv: srv, h: srv.Handler(), state: "ok", warns: warns}
}

// Warm 在后台为每个实例做一次首次采集（填充状态摘要）。
func (h *Hub) Warm() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, in := range h.inst {
		if in.srv == nil || id == h.primaryID {
			continue
		}
		go func(s *Server) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s.Snapshot(ctx, false)
		}(in.srv)
	}
}

func auditEntry(user string, sp clusters.Spec, action, risk, summary, result string) audit.Entry {
	return audit.Entry{User: user, Env: sp.Env, Cluster: sp.Name, Action: action, Target: sp.ID, Risk: risk, Summary: summary, Result: result}
}

// build 构造一个从属实例（不登记、不持久化）。
func (h *Hub) build(sp clusters.Spec) (*Server, []string, error) {
	if sp.LoadError != "" {
		return nil, nil, errors.New(sp.LoadError)
	}
	be, err := h.cfg.Factory(sp)
	if err != nil {
		return nil, nil, err
	}
	dir := ""
	if h.cfg.Dir != "" {
		dir = filepath.Join(h.cfg.Dir, "clusters", sp.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, err
		}
	}
	p := h.primary
	s := New(Config{Col: be.Col, Mode: be.Mode, Web: p.cfg.Web, SnapTTL: firstDur(be.SnapTTL, p.cfg.SnapTTL), Timeout: firstDur(be.Timeout, p.cfg.Timeout), Logf: p.cfg.Logf, Fixtures: be.Fixtures, ICP: p.cfg.ICP, Unavailable: p.cfg.Unavailable})
	s.hub, s.clusterID = h, sp.ID
	pc := p.ops.cfg
	cfg := OpsConfig{
		Dir: dir, Env: sp.Env, Users: pc.Users, Roles: pc.Roles, Audit: pc.Audit, Key: pc.Key, ReadOnlyFile: pc.ReadOnlyFile,
		SecureCookie: pc.SecureCookie, NoMFA: pc.NoMFA, Demo: pc.Demo, SessionIdle: pc.SessionIdle, SessionAbs: pc.SessionAbs,
		WaitScale: pc.WaitScale, TickEvery: pc.TickEvery, Now: pc.Now, MasterKey: pc.MasterKey, PublicURL: pc.PublicURL, NotifyHosts: pc.NotifyHosts,
		NotifyInsecureTest: pc.NotifyInsecureTest, NotifyWatchEvery: pc.NotifyWatchEvery, ExecMode: "none",
		Sessions: pc.Sessions, Limits: pc.Limits, policyOwner: p.ops,
	}
	if be.Ops != nil {
		if err := be.Ops(s, &cfg); err != nil {
			return nil, nil, err
		}
	}
	if err := s.EnableOps(cfg); err != nil {
		return nil, nil, err
	}
	var warns []string
	if be.Preflight != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		warns = be.Preflight(ctx, s)
		cancel()
	}
	return s, warns, nil
}

func firstDur(a, b time.Duration) time.Duration {
	if a > 0 {
		return a
	}
	return b
}

// Close 关闭全部从属实例（主实例由调用方关闭）。
func (h *Hub) Close() {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	h.mu.Lock()
	var list []*Server
	for id, in := range h.inst {
		if id != h.primaryID && in.srv != nil {
			list = append(list, in.srv)
		}
	}
	h.mu.Unlock()
	for _, s := range list {
		s.Close()
	}
}

// Registry 返回注册表（只读使用）。
func (h *Hub) Registry() *clusters.Registry { return h.reg }

// Platform 报告是否独立部署：主实例是平台本身（不代表任何集群），对用户不可见，所有集群都在「集群接入」里添加。
func (h *Hub) Platform() bool { return h.reg.Primary().Platform }

// Instance 返回某个集群的实例（测试 / 内部用）；不存在或启动失败返回 nil。
func (h *Hub) Instance(id string) *Server {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if in := h.inst[id]; in != nil {
		return in.srv
	}
	return nil
}

func (h *Hub) get(id string) *instance {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.inst[id]
}

func (h *Hub) put(id string, in *instance) {
	h.mu.Lock()
	h.inst[id] = in
	h.mu.Unlock()
}

// ---------------------------------------------------------------- 路由 ----

const clusterHeader = "X-Cluster"

// Handler 返回多集群的 HTTP 处理器（应替代主实例的 Handler 交给 http.Server）。
func (h *Hub) Handler() http.Handler { return h.router }

func pathIs(p, base string) bool { return p == base || strings.HasPrefix(p, base+"/") }

// isGlobalAPI 判断 /api/ 路径是否是全局端点（始终由主集群实例处理）。其余 /api/ 路径都是集群端点。
func isGlobalAPI(p string) bool {
	switch {
	case p == "/api/health", p == "/api/version", p == "/api/ops/version":
		return true
	}
	for _, b := range []string{"/api/auth", "/api/users", "/api/roles", "/api/audit", "/api/ops/proposals", "/api/ops/policy", "/api/clusters", "/api/watermark", "/api/deploy", "/api/shell"} {
		if pathIs(p, b) {
			return true
		}
	}
	return false
}

// selector 取请求选择的集群 id：请求头优先，其次查询参数 cluster。空值视为未指定；重复的头 / 参数视为非法。
func selector(r *http.Request) (id string, explicit, bad bool) {
	if vs := r.Header.Values(clusterHeader); len(vs) > 0 {
		if len(vs) > 1 {
			return "", true, true
		}
		if vs[0] != "" {
			return vs[0], true, false
		}
	}
	if qs, ok := r.URL.Query()["cluster"]; ok {
		if len(qs) > 1 {
			return "", true, true
		}
		if qs[0] != "" {
			return qs[0], true, false
		}
	}
	return "", false, false
}

func visibleTo(u authz.User, env string) bool { return u.Can(env, authz.CapViewOverview) }

func notFoundCluster(w http.ResponseWriter) {
	writeJSONCode(w, http.StatusNotFound, apiError{Error: "集群不存在或无权访问", Code: "cluster_not_found"})
}

func (h *Hub) buildRouter() http.Handler {
	return h.primary.guard(http.HandlerFunc(h.route))
}

func (h *Hub) route(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(r.URL.Path)
	if !strings.HasPrefix(p, "/api/") {
		h.get(h.primaryID).h.ServeHTTP(w, r) // 静态页面与帮助文件
		return
	}
	if p == "/api/health" || p == "/api/auth/me" {
		h.serveSoft(w, r, p == "/api/auth/me")
		return
	}
	if isGlobalAPI(p) {
		h.get(h.primaryID).h.ServeHTTP(w, r)
		return
	}
	ru, authed := h.primary.authenticate(r)
	if !authed {
		h.get(h.primaryID).h.ServeHTTP(w, r) // 未登录：走现有 401 流程，不泄露集群是否存在
		return
	}
	in, ok := h.resolve(r, ru.U)
	if !ok {
		notFoundCluster(w)
		return
	}
	h.dispatch(w, r, in)
}

// resolve 按请求选择集群并做可见性判定。未指定时 = 主集群；多集群时同样要求可见（避免无关用户经缺省路径读到主集群）。
func (h *Hub) resolve(r *http.Request, u authz.User) (*instance, bool) {
	id, explicit, bad := selector(r)
	if bad {
		return nil, false
	}
	if !explicit {
		if h.Platform() { // 独立部署：缺省 = 该用户可见的第一个集群；平台实例本身不是集群，不提供集群数据
			if in := h.defaultInstance(u); in != nil && !in.spec.Platform {
				return in, true
			}
			return nil, false
		}
		id = h.primaryID
	}
	in := h.get(id)
	if in != nil && in.spec.Platform { // 显式指定平台实例：与「不存在」一致
		return nil, false
	}
	if in == nil || !visibleTo(u, in.spec.Env) {
		if !explicit && h.single() { // 单集群部署的缺省路径保持原有行为：交给端点自己的权限判定
			return h.get(h.primaryID), true
		}
		return nil, false
	}
	return in, true
}

func (h *Hub) single() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.inst) <= 1
}

func (h *Hub) dispatch(w http.ResponseWriter, r *http.Request, in *instance) {
	if in.srv == nil {
		msg := "集群实例不可用"
		if in.err != "" {
			msg = "集群实例未能启动，请联系管理员（原因见集群接入页）" // 具体原因可能含地址 / 路径，不在这里返回
		}
		if in.state == "loading" {
			msg = "集群正在重载，请稍后重试"
		}
		writeJSONCode(w, http.StatusServiceUnavailable, apiError{Error: msg, Code: "cluster_unavailable"})
		return
	}
	in.h.ServeHTTP(w, r)
}

// serveSoft 处理 /api/health 与 /api/auth/me：未登录一律由主实例回答（忽略 X-Cluster，不泄露集群）；
// 已登录且显式指定集群则按集群端点规则（可见才回答，否则 404）；已登录未指定：health = 主集群，me = 缺省选择
// （主集群可见则主集群，否则第一个可见集群）。
func (h *Hub) serveSoft(w http.ResponseWriter, r *http.Request, isMe bool) {
	ru, authed := h.primary.authenticate(r)
	if !authed {
		h.get(h.primaryID).h.ServeHTTP(w, r)
		return
	}
	id, explicit, bad := selector(r)
	if bad {
		notFoundCluster(w)
		return
	}
	if explicit {
		in := h.get(id)
		if in == nil || in.spec.Platform || !visibleTo(ru.U, in.spec.Env) {
			notFoundCluster(w)
			return
		}
		h.dispatch(w, r, in)
		return
	}
	if isMe {
		if in := h.defaultInstance(ru.U); in != nil {
			h.dispatch(w, r, in)
			return
		}
	}
	h.get(h.primaryID).h.ServeHTTP(w, r)
}

// defaultInstance 是未显式选择集群时的缺省：主集群可见则主集群，否则第一个可见的集群，都不可见则主集群。
func (h *Hub) defaultInstance(u authz.User) *instance {
	p := h.get(h.primaryID)
	if p != nil && !p.spec.Platform && visibleTo(u, p.spec.Env) {
		return p
	}
	for _, sp := range h.reg.List() {
		if sp.Platform {
			continue
		}
		if in := h.get(sp.ID); in != nil && visibleTo(u, sp.Env) {
			return in
		}
	}
	return p // 独立部署且没有可见集群时 = 平台实例：/api/auth/me 由它回答（clusters 为空）
}

// ---------------------------------------------------------------- 展示 ----

type briefWrite struct {
	Enabled        bool   `json:"enabled"`
	Pending        bool   `json:"pending"`
	ReadOnlyForced bool   `json:"readOnlyForced"`
	Reason         string `json:"reason,omitempty"`
}

// clusterBrief 是 /api/auth/me 里的集群简要信息。
type clusterBrief struct {
	ID      string     `json:"id"`
	Display string     `json:"display"`
	Env     string     `json:"env"`
	Mode    string     `json:"mode"`
	Primary bool       `json:"primary"`
	Local   bool       `json:"local"`
	State   string     `json:"state"`
	Write   briefWrite `json:"write"`
}

func (in *instance) brief() clusterBrief {
	b := clusterBrief{ID: in.spec.ID, Display: in.spec.Display, Env: in.spec.Env, Mode: in.spec.Mode, Primary: in.spec.Primary, Local: in.spec.Local, State: in.state}
	if in.srv != nil && in.srv.ops != nil {
		ws := in.srv.writeState()
		b.Write = briefWrite{Enabled: ws.Enabled, Pending: ws.Pending != nil, ReadOnlyForced: ws.ReadOnlyForced, Reason: ws.Reason}
		if b.Mode == "" {
			b.Mode = in.srv.cfg.Mode
		}
	} else {
		b.Write = briefWrite{Reason: "集群实例不可用"} // 具体原因（可能含地址 / 路径）只给管理员，见 /api/clusters
	}
	return b
}

func (h *Hub) brief(id string) (clusterBrief, bool) {
	in := h.get(id)
	if in == nil {
		return clusterBrief{}, false
	}
	return in.brief(), true
}

// meClusters 返回 /api/auth/me 的 clusters（用户可见的全部集群）与 cluster（curID 对应的集群）。
func (h *Hub) meClusters(u authz.User, curID string) ([]clusterBrief, *clusterBrief) {
	list := []clusterBrief{}
	var cur *clusterBrief
	for _, sp := range h.reg.List() {
		in := h.get(sp.ID)
		if in == nil || sp.Platform {
			continue
		}
		b := in.brief()
		if sp.ID == curID {
			bb := b
			cur = &bb
		}
		if visibleTo(u, sp.Env) {
			list = append(list, b)
		}
	}
	return list, cur
}

// ---------------------------------------------------------------- 添加 / 更新 / 删除 ----

// Add 接入一个新集群并热生效。返回规整后的定义与可读警告。
func (h *Hub) Add(in clusters.Input, by string) (clusters.Spec, []string, error) {
	return h.add(in, by, false)
}

// Import 接入一个本机 tiup 管理的集群（Spec.Local）并热生效：该集群可经本机执行器做全部运维操作。
func (h *Hub) Import(in clusters.Input, by string) (clusters.Spec, []string, error) {
	return h.add(in, by, true)
}

func (h *Hub) add(in clusters.Input, by string, local bool) (clusters.Spec, []string, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	prepare := h.reg.PrepareAdd
	if local {
		prepare = h.reg.PrepareImport
	}
	sp, err := prepare(in, by)
	if err != nil {
		return clusters.Spec{}, nil, err
	}
	dir := h.clusterDir(sp.ID)
	_, statErr := os.Stat(dir)
	fresh := dir != "" && os.IsNotExist(statErr)
	srv, warns, err := h.build(sp)
	if err != nil {
		if fresh {
			_ = os.Remove(dir) // 只删刚创建的空目录
		}
		return clusters.Spec{}, nil, &clusters.Error{Code: clusters.ErrStart, Msg: "集群实例启动失败：" + err.Error()}
	}
	if err := h.reg.Put(sp); err != nil {
		srv.Close()
		if fresh {
			_ = os.RemoveAll(dir)
		}
		return clusters.Spec{}, nil, err
	}
	h.put(sp.ID, &instance{spec: sp, srv: srv, h: srv.Handler(), state: "ok", warns: warns})
	if h.cfg.Warm {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			srv.Snapshot(ctx, false)
		}()
	}
	return sp, warns, nil
}

func (h *Hub) clusterDir(id string) string {
	if h.cfg.Dir == "" {
		return ""
	}
	return filepath.Join(h.cfg.Dir, "clusters", id)
}

// rebuildNeeded 判断更新是否影响实例（只改 display / desc 不需要重建）。
func rebuildNeeded(a, b clusters.Spec) bool {
	a.Display, a.Desc, a.Updated, a.UpdatedBy = b.Display, b.Desc, b.Updated, b.UpdatedBy
	a.LoadError, b.LoadError = "", ""
	return a != b
}

// Update 更新一个集群并热生效：连接 / env / name 等变化会重建实例（先关旧实例再构造新实例，失败则用旧定义恢复）。
func (h *Hub) Update(id string, in clusters.Input, by string) (clusters.Spec, clusters.Spec, []string, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	old, next, err := h.reg.PrepareUpdate(id, in, by)
	if err != nil {
		return clusters.Spec{}, clusters.Spec{}, nil, err
	}
	cur := h.get(id)
	if cur == nil {
		return clusters.Spec{}, clusters.Spec{}, nil, &clusters.Error{Code: clusters.ErrNotFound, Msg: "没有这个集群"}
	}
	if old.Primary || !rebuildNeeded(old, next) && cur.state == "ok" {
		if err := h.reg.Put(next); err != nil {
			return clusters.Spec{}, clusters.Spec{}, nil, err
		}
		ni := *cur
		ni.spec = next
		h.put(id, &ni)
		return old, next, cur.warns, nil
	}
	if cur.srv != nil && cur.srv.ops != nil {
		sum := cur.srv.ops.engine.Summary()
		if old.Env != next.Env || old.Name != next.Name {
			if n := cur.srv.ops.engine.Busy(); n > 0 {
				return clusters.Spec{}, clusters.Spec{}, nil, &clusters.Error{Code: clusters.ErrInUse, Msg: fmt.Sprintf("该集群还有 %d 项进行中的变更 / 维护，修改 env 或 tiup 集群名前请先处理完", n)}
			}
		} else if sum["running"] > 0 {
			return clusters.Spec{}, clusters.Spec{}, nil, &clusters.Error{Code: clusters.ErrInUse, Msg: "该集群有正在执行的变更，重载连接信息会中断它；请等执行结束后再改"}
		}
	}
	h.put(id, &instance{spec: old, state: "loading", err: "正在重载"})
	if cur.srv != nil {
		cur.srv.Close()
	}
	restore := func(reason error) {
		ni := h.startInstance(old)
		h.put(id, ni)
		if ni.state != "ok" {
			h.primary.cfg.Logf("集群 %s 回滚旧配置失败：%v（原因：%v）", id, ni.err, reason)
		}
	}
	srv, warns, err := h.build(next)
	if err != nil {
		restore(err)
		return clusters.Spec{}, clusters.Spec{}, nil, &clusters.Error{Code: clusters.ErrStart, Msg: "新配置下实例启动失败，已回滚到原配置：" + err.Error()}
	}
	if err := h.reg.Put(next); err != nil {
		srv.Close()
		restore(err)
		return clusters.Spec{}, clusters.Spec{}, nil, err
	}
	h.put(id, &instance{spec: next, srv: srv, h: srv.Handler(), state: "ok", warns: warns})
	return old, next, warns, nil
}

// Remove 热移除一个非主集群：有进行中的变更 / 维护则拒绝；成功后实例关闭，数据目录改名为
// clusters/.removed-<id>-<时间戳>/ 保留（不物理删除）。
func (h *Hub) Remove(id string) (clusters.Spec, string, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	sp, ok := h.reg.Get(id)
	if !ok {
		return clusters.Spec{}, "", &clusters.Error{Code: clusters.ErrNotFound, Msg: "没有这个集群"}
	}
	if sp.Primary {
		return clusters.Spec{}, "", &clusters.Error{Code: clusters.ErrPrimary, Msg: "主集群不能删除"}
	}
	cur := h.get(id)
	if cur == nil {
		return clusters.Spec{}, "", &clusters.Error{Code: clusters.ErrNotFound, Msg: "没有这个集群"}
	}
	// 先把实例摘出路由（之后的请求得到 503），再检查是否在用；在用则恢复。
	h.put(id, &instance{spec: sp, state: "loading", err: "正在删除"})
	if cur.srv != nil && cur.srv.ops != nil {
		if n := cur.srv.ops.engine.Busy(); n > 0 {
			h.put(id, cur)
			return clusters.Spec{}, "", &clusters.Error{Code: clusters.ErrInUse, Msg: fmt.Sprintf("该集群还有 %d 项待审批 / 排队 / 执行中的变更或进行中的维护，请先处理完（取消 / 驳回 / 等待结束）再删除", n)}
		}
	}
	if cur.srv != nil {
		cur.srv.Close()
	}
	rollback := func(reason error) {
		h.put(id, h.startInstance(sp))
		h.primary.cfg.Logf("集群 %s 删除失败，已恢复：%v", id, reason)
	}
	kept := ""
	if dir := h.clusterDir(id); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			kept = h.uniqueRemovedDir(id)
			if err := renameRetry(dir, kept); err != nil {
				rollback(err)
				return clusters.Spec{}, "", &clusters.Error{Code: clusters.ErrStore, Msg: "移走集群数据目录失败：" + err.Error()}
			}
		}
	}
	if _, err := h.reg.Remove(id); err != nil {
		if kept != "" {
			_ = os.Rename(kept, h.clusterDir(id))
		}
		rollback(err)
		return clusters.Spec{}, "", err
	}
	h.mu.Lock()
	delete(h.inst, id)
	h.mu.Unlock()
	return sp, kept, nil
}

// statusOf 返回实例的状态摘要（只读缓存，不触发采集）。
func (in *instance) statusOf() clusterStatus {
	st := clusterStatus{State: in.state, Error: in.err}
	if in.srv == nil {
		return st
	}
	if snap, ok := in.srv.cachedSnapshot(); ok {
		t := snap.CollectedAt
		st.SnapshotAt = &t
		reach := len(snap.Topology.Instances) > 0
		st.Reachable = &reach
		st.Errors = len(snap.Errors)
		st.Version = snap.Topology.Version
		st.Nodes = len(snap.Topology.Instances)
	}
	return st
}

// ---------------------------------------------------------------- 测试辅助：执行器预检 ----

// RunnerPreflight 是 Backend.Preflight 的通用实现：经执行器跑一条只读的 tiup cluster display，被拒绝 / 失败就返回可读警告。
func RunnerPreflight(ctx context.Context, s *Server, name string) []string {
	if s.ops == nil {
		return nil
	}
	switch s.ops.cfg.ExecMode {
	case "direct", "sudo":
	default:
		return nil
	}
	if _, isNo := s.ops.cfg.Runner.(noRunner); isNo || s.ops.cfg.Runner == nil {
		return nil
	}
	res, err := s.ops.cfg.Runner.Run(ctx, exec.Request{Cmd: exec.Cmd{Kind: exec.KindClusterDisplay, Cluster: name}}, nil)
	if err == nil && res.Exit == 0 {
		return nil
	}
	msg := res.Err
	if err != nil {
		msg = err.Error()
	}
	w := fmt.Sprintf("执行器预检未通过（%s）：该集群的变更 / 巡检命令可能无法执行", truncRunes(msg, 200))
	if s.ops.cfg.ExecMode == "sudo" {
		w += fmt.Sprintf("。sudo 模式下特权端的策略文件（--exec-policy）必须允许集群名 %q：请把它加入策略文件的 clusters 列表，并确认 tiup 路径与账号", name)
	} else {
		w += fmt.Sprintf("。请确认执行器策略允许集群名 %q，且 tiup 可执行", name)
	}
	return []string{w}
}

// renameRetry 改名目录；Windows 上刚关闭的实例的文件句柄可能被杀毒 / 索引短暂占用，失败时重试几次。
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 8; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
	return err
}

// uniqueRemovedDir 返回 clusters/.removed-<id>-<时间戳>（同一秒内重复删除同 id 时追加序号，避免撞名）。
func (h *Hub) uniqueRemovedDir(id string) string {
	base := filepath.Join(h.cfg.Dir, "clusters", fmt.Sprintf(".removed-%s-%s", id, time.Now().UTC().Format("20060102T150405Z")))
	p := base
	for n := 2; ; n++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = fmt.Sprintf("%s-%d", base, n)
	}
}
