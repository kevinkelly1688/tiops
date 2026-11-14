// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops.go
// @time: 2026/10/28

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/change"
	"tidbops/internal/collect"
	"tidbops/internal/deploy"
	"tidbops/internal/exec"
	"tidbops/internal/lb"
	"tidbops/internal/model"
	"tidbops/internal/notify"
	"tidbops/internal/shell"
)

var _ = model.Snapshot{}

// OpsConfig 是「运维模式」的配置：登录、审批、审计、变更执行。不提供时服务保持只读（无登录、仅 GET）。
type OpsConfig struct {
	Dir          string // 数据目录：users.json / audit.log / changes.json / settings.json
	Env          string // test | staging | prod
	Users        *authz.Store
	Roles        *authz.RoleStore // 自定义角色库；nil 则在 EnableOps 里按 Dir/roles.json 打开（Dir 为空则仅内存）
	Audit        *audit.Log
	Runner       exec.Runner // 变更执行与只读命令（巡检、PITR 状态）；nil 表示不能执行任何命令
	ReadOnlyWhy  string      // 没有执行器时给用户看的原因（如「手工接入的远程集群」）；空 = 通用提示
	Prober       change.Prober
	Key          []byte // 审批令牌密钥
	ReadOnlyFile string // 存在即强制只读，默认 /etc/tidb-ops/READONLY
	SecureCookie bool
	NoMFA        bool          // 关闭「高风险申请人需 MFA」（仅限测试 / POC）
	Sim          bool          // 执行器是模拟的
	Demo         bool          // 演示模式（--dev-seed）：登录页显示演示账号快捷填充；正式环境恒为 false
	SessionIdle  time.Duration // 默认 30 分钟
	SessionAbs   time.Duration // 默认 8 小时
	WaitScale    float64       // 测试用：缩放等待时间
	TickEvery    time.Duration
	Now          func() time.Time
	ExecMode     string   // sim | direct | sudo | none（仅展示）
	MasterKey    []byte   // 加密落盘用的主密钥（通知渠道的 Webhook 等）
	PublicURL    string   // 平台对外访问地址，用于通知里的链接（可为空）
	NotifyHosts  []string // 额外允许的 Webhook 主机（私有化部署的 Lark / 企业微信网关；默认只允许官方域名）
	BackupRoots  []string // 备份存储前缀白名单（与执行器策略一致，规划时提前校验）

	// NotifyInsecureTest 仅供测试：允许通知渠道使用 http 地址（打到 httptest 假 Webhook）。
	// 生产路径（cmd 启动参数）不要暴露、不要设置；风险：开启后 Webhook 可以明文传输。
	NotifyInsecureTest bool
	// NotifyWatchEvery 是告警 / 诊断监视器的轮询间隔：0 = 60 秒；<0 = 不启动后台监视（测试里手动调用 watchOnce）。
	NotifyWatchEvery time.Duration

	// Deploy 是「一键部署」管理器（见 deploy_api.go）。nil = 中控机模式下按需创建（用 su 取得 root）；测试里注入带假执行器的实例。
	Deploy *deploy.Manager
	// Shell 是「中控机命令」管理器（见 shell_api.go）。nil = 中控机模式下按需创建（需要 ShellAgent）；测试里注入带假执行器的实例。
	Shell *shell.Manager
	// ShellAgent 是「中控机命令」的子进程通道（sudo -n -u tidb <bin> shell-exec --stdin）；执行器为 --exec sudo 时由启动程序设置。
	ShellAgent shell.Agent

	// ---- 多集群（Hub 构造从属实例时注入；单集群部署全部留空）----

	// Sessions 非空时使用这张会话表（多个集群实例共享：一次登录处处可用，退出 / 吊销对全部实例即时生效）；
	// nil 则新建一张。CSRF 令牌由 Key 与会话令牌派生，各实例用同一个 Key，所以天然通用。
	Sessions *authz.Sessions
	// Limits 非空时共享写接口的限速计数（每用户每分钟 60 次，跨实例合计）；nil 则新建。
	Limits *RateLimits

	// policyOwner 非空表示这是从属集群实例：审批策略（风险 × 环境）是全局的，读主集群实例里生效的那一张；
	// 同时跳过只应由主实例做一次的启动动作（自定义角色库告警）。
	policyOwner *ops
}

// RateLimits 是按用户的写接口限速计数（滑动 1 分钟窗口）。并发安全；可在多个实例间共享。
type RateLimits struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

// NewRateLimits 创建限速器。
func NewRateLimits() *RateLimits { return &RateLimits{m: map[string][]time.Time{}} }

func (l *RateLimits) allow(user string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-time.Minute)
	var keep []time.Time
	for _, t := range l.m[user] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 60 {
		l.m[user] = keep
		return false
	}
	l.m[user] = append(keep, now)
	return true
}

// Settings 是持久化的平台设置。
type Settings struct {
	WriteEnabled bool              `json:"writeEnabled"`
	Pending      *SwitchProposal   `json:"pending,omitempty"`
	Freezes      []change.Freeze   `json:"freezes"`
	Windows      []change.Window   `json:"windows"` // 变更窗口规则：nil（settings.json 里没有该字段）= 默认规则，空数组 = 没有任何规则
	LB           bool              `json:"lb"`      // 集群前有负载均衡（缩容 / 维护 TiDB 时要求人工摘除）
	Overrides    map[string]string `json:"overrides"`
	// LBs 是手工登记的负载均衡（见 lb_api.go）。旧 settings.json 没有该字段时为 nil；
	// 有效的「有负载均衡」= LB || len(LBs) > 0（effectiveLBLocked）。GET /api/ops/settings 不返回该字段（走 /api/lb）。
	LBs []lb.Entry `json:"lbs,omitempty"`
	// ApprovalPolicy 是生效的审批策略（风险 × 环境）。nil（settings.json 里没有该字段）= 默认策略（A3 §2.4）。
	// 只能经「approval-policy」提案由两名管理员确认后修改，见 policy_api.go。
	ApprovalPolicy *authz.PolicyTable `json:"approvalPolicy,omitempty"`
}

// SwitchProposal 是待第二位管理员确认的写操作开关。
type SwitchProposal struct {
	By      string    `json:"by"`
	Enabled bool      `json:"enabled"`
	At      time.Time `json:"at"`
}

type ops struct {
	cfg      OpsConfig
	sessions *authz.Sessions
	engine   *change.Engine

	mu       sync.Mutex
	settings Settings
	rl       *RateLimits
	lastInsp *model.Report

	notifier *notify.Notifier // 通知渠道（企业微信 / 钉钉 / Lark），见 notify_api.go
	nw       notifyWatch      // 告警 / 诊断监视器状态
	props    *proposalStore   // 双人复核提案（授予管理员角色 / 修改审批策略），见 policy_api.go
}

func (o *ops) now() time.Time {
	if o.cfg.Now != nil {
		return o.cfg.Now()
	}
	return time.Now()
}

// EnableOps 为服务打开运维模式。必须在 Handler() 之前调用。
func (s *Server) EnableOps(cfg OpsConfig) error {
	if cfg.Users == nil || cfg.Audit == nil {
		return errors.New("运维模式需要用户库与审计日志")
	}
	if cfg.Env == "" {
		cfg.Env = "prod"
	}
	if cfg.ReadOnlyFile == "" {
		cfg.ReadOnlyFile = "/etc/tidb-ops/READONLY"
	}
	if cfg.SessionIdle <= 0 {
		cfg.SessionIdle = 30 * time.Minute
	}
	if cfg.SessionAbs <= 0 {
		cfg.SessionAbs = 8 * time.Hour
	}
	if cfg.Roles == nil {
		// 注意：自定义角色注册表是进程级的，这里（再次）打开会用该库的内容整体替换注册表。
		rp := ""
		if cfg.Dir != "" {
			rp = filepath.Join(cfg.Dir, "roles.json")
		}
		rs, err := authz.OpenRoleStore(rp)
		if err != nil {
			return fmt.Errorf("读取 roles.json 失败: %w", err)
		}
		cfg.Roles = rs
	}
	if cfg.Sessions == nil {
		cfg.Sessions = authz.NewSessions(cfg.SessionIdle, cfg.SessionAbs)
	}
	if cfg.Limits == nil {
		cfg.Limits = NewRateLimits()
	}
	o := &ops{cfg: cfg, sessions: cfg.Sessions, rl: cfg.Limits}
	o.settings = Settings{Freezes: []change.Freeze{}, Overrides: map[string]string{}}
	if cfg.Dir != "" {
		if b, err := os.ReadFile(filepath.Join(cfg.Dir, "settings.json")); err == nil {
			if err := json.Unmarshal(b, &o.settings); err != nil {
				return fmt.Errorf("读取 settings.json 失败: %w", err)
			}
			if o.settings.Overrides == nil {
				o.settings.Overrides = map[string]string{}
			}
		}
	}
	o.lastInsp = loadLastInspect(cfg.Dir) // 最近一次巡检报告：重启 / 升级后仍保留
	if cfg.Runner == nil {
		cfg.Runner = noRunner{}
		o.cfg.Runner = cfg.Runner
	}
	if cfg.Prober == nil {
		cfg.Prober = &change.SimProber{Polls: 1}
		o.cfg.Prober = cfg.Prober
	}
	s.initNotify(o)
	s.initPolicy(o) // 校验已落盘的审批策略、加载双人复核提案
	if cfg.policyOwner == nil {
		s.initRoles(o) // 自定义角色库的加载告警写日志与审计（从属实例共用主实例的角色库，不重复告警）
	}
	eng, err := change.New(change.Config{
		Cluster: s.cfg.Col.Params.Cluster, Env: cfg.Env, Dir: cfg.Dir, Runner: o.cfg.Runner, Key: cfg.Key, Prober: o.cfg.Prober,
		Policy: o.approvalPolicy, // 每次审批判定都读当前生效的策略表（可被提案修改）
		Gate:   func(env string) error { return s.gate(o) },
		Base:   func(ctx context.Context) (change.Ctx, error) { return s.planBase(ctx, o) },
		Events: func(ev change.Event) { s.engineEvent(o, ev) },
		Freezes: func() []change.Freeze {
			o.mu.Lock()
			defer o.mu.Unlock()
			return append([]change.Freeze(nil), o.settings.Freezes...)
		},
		Windows: o.effectiveWindows,
		Now:     cfg.Now, WaitScale: cfg.WaitScale, TickEvery: cfg.TickEvery,
	})
	if err != nil {
		o.notifier.Close()
		return err
	}
	o.engine = eng
	s.ops = o
	eng.Start()
	s.startNotifyWatch(o)
	cfg.Audit.Append(audit.Entry{User: "system", Action: "platform.start", Env: cfg.Env, Cluster: s.cfg.Col.Params.Cluster, Summary: fmt.Sprintf("平台启动，版本 %s，执行器 %s，写操作开关 %v", Version, cfg.ExecMode, o.settings.WriteEnabled), Result: "success"})
	return nil
}

// Close 停止后台任务。
func (s *Server) Close() {
	if s.ops != nil {
		s.stopNotifyWatch(s.ops)
		s.ops.engine.Close()
		s.ops.notifier.Close()  // 在引擎之后关闭，才能送出「平台停止」导致的失败通知
		hostPools.Delete(s.ops) // 主机池按实例缓存：实例关闭（含多集群热删除）后释放
	}
}

type noRunner struct{}

func (noRunner) Run(ctx context.Context, req exec.Request, emit func(string)) (exec.Result, error) {
	return exec.Result{}, errors.New("未配置执行器（启动参数 --exec）")
}

func (s *Server) saveSettings(o *ops) { _ = s.saveSettingsErr(o) }

// saveSettingsErr 与 saveSettings 相同（临时文件 + 改名的原子写，调用方持有 o.mu），但返回写盘错误，
// 供需要在失败时回滚内存状态并报 500 的写接口（负载均衡登记）使用。数据目录为空（纯内存）时恒为 nil。
func (s *Server) saveSettingsErr(o *ops) error {
	if o.cfg.Dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(o.settings, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(o.cfg.Dir, "settings.json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// readOnlyForced 判断 READONLY 熔断文件是否存在。
func (o *ops) readOnlyForced() bool {
	if o.cfg.ReadOnlyFile == "" {
		return false
	}
	_, err := os.Stat(o.cfg.ReadOnlyFile)
	return err == nil
}

// gate 是写操作的总闸（A3 §2.3 第 3、4 步）：熔断文件、环境写开关、版本支持矩阵、执行器。
func (s *Server) gate(o *ops) error {
	if s.cfg.Unavailable != nil && s.cfg.Unavailable() {
		return errors.New("服务不可用") // 不说明原因：对用户保持静默，原因只在服务端日志里
	}
	if o.readOnlyForced() {
		return fmt.Errorf("平台处于强制只读（存在 %s）", o.cfg.ReadOnlyFile)
	}
	if _, isNo := o.cfg.Runner.(noRunner); isNo && o.cfg.ReadOnlyWhy != "" { // 结构性只读（远程接入的集群）：开关再怎么开也没用，先说这个
		return errors.New(o.cfg.ReadOnlyWhy)
	}
	o.mu.Lock()
	on := o.settings.WriteEnabled
	o.mu.Unlock()
	if !on {
		return errors.New("写操作开关未开启：由两名管理员确认后才能对该环境执行变更")
	}
	if _, isNo := o.cfg.Runner.(noRunner); isNo {
		return errors.New("未配置执行器，无法执行变更")
	}
	if v := s.cachedSnapshotVersion(); v != "" && !collect.VersionSupported(v) {
		return fmt.Errorf("集群版本 %s 不在平台支持范围内（%s），写操作已降级为只读", v, strings.Join(collect.SupportedVersions, " "))
	}
	return nil
}

func (s *Server) cachedSnapshotVersion() string {
	if snap, ok := s.cachedSnapshot(); ok {
		return snap.Topology.Version
	}
	return ""
}

// planBase 提供规划上下文。
func (s *Server) planBase(ctx context.Context, o *ops) (change.Ctx, error) {
	snap := s.Snapshot(ctx, true)
	if len(snap.Topology.Instances) == 0 {
		return change.Ctx{}, errors.New("没有拓扑数据，无法规划变更：" + strings.Join(snap.Errors, "；"))
	}
	set := s.cfg.Col.Settings(ctx)
	c := change.Ctx{BackupRoots: o.cfg.BackupRoots, Topo: snap.Topology, PDDownMin: set.PDDownMin, Replicas: set.Replicas, Sim: o.cfg.Sim, TopologyYAML: set.TopologyYAML, TopologySynthetic: !set.TopologyReal}
	if o.cfg.Sim || !set.TopologyReal {
		c.TopologyYAML, c.TopologySynthetic = change.SynthTopologyYAML(snap.Topology), true
	}
	o.mu.Lock()
	c.LB = o.effectiveLBLocked()
	c.LBs = lb.DescribeAll(o.settings.LBs)
	o.mu.Unlock()
	c.HostChecks = s.recentHostChecks(o) // 主机池里最近 5 分钟内的预检结果（host_api.go）
	return c, nil
}

// engineEvent 把引擎事件写入审计。
func (s *Server) engineEvent(o *ops, ev change.Event) {
	s.notifyEngineEvent(o, ev) // 先转成通知（异步入队）；step / command 等事件在里面被忽略
	res := ev.Result
	if res == "" {
		res = "success"
	}
	action := "change." + ev.Type
	if ev.Type == "step" || ev.Type == "manual-wait" {
		return // 步骤进度只在变更记录里保留，审计只记录命令与终态
	}
	user := ev.User
	if user == "" {
		user = "system"
	}
	_, _ = o.cfg.Audit.Append(audit.Entry{User: user, Action: action, Env: o.cfg.Env, Cluster: s.cfg.Col.Params.Cluster, Target: ev.ChangeID, Risk: ev.Risk, Summary: ev.Detail, Result: res, Ref: ev.ChangeID})
}

// ---------------------------------------------------------------- 认证与请求辅助 ----

const cookieName = "tidbops_session"

type ctxKey int

const userKey ctxKey = 1

type reqUser struct {
	U       authz.User
	Session string
	CSRF    string
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) secureCookie(r *http.Request) bool {
	return s.ops.cfg.SecureCookie || r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// authenticate 从 Cookie 取得会话用户。
func (s *Server) authenticate(r *http.Request) (reqUser, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return reqUser{}, false
	}
	sess, ok := s.ops.sessions.Get(c.Value, s.ops.now())
	if !ok {
		return reqUser{}, false
	}
	u, ok := s.ops.cfg.Users.Get(sess.User)
	if !ok || u.Disabled {
		s.ops.sessions.Destroy(c.Value)
		return reqUser{}, false
	}
	return reqUser{U: u, Session: c.Value}, true
}

func (s *Server) auditReq(r *http.Request, u authz.User, action, target, risk, summary, result string) {
	role := ""
	if rs := u.RolesFor(s.ops.cfg.Env); len(rs) > 0 {
		var names []string
		for _, x := range rs {
			names = append(names, string(x))
		}
		role = strings.Join(names, ",")
	}
	_, _ = s.ops.cfg.Audit.Append(audit.Entry{User: u.Name, Role: role, IP: clientIP(r), Env: s.ops.cfg.Env, Cluster: s.cfg.Col.Params.Cluster, Action: action, Target: target, Risk: risk, Summary: summary, Result: result})
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(apiError{Error: msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法的 JSON："+err.Error())
		return false
	}
	return true
}

// rateOK 对写类接口做每用户限速（每分钟 60 次；多集群时各实例共享同一个计数）。
func (o *ops) rateOK(user string) bool { return o.rl.allow(user, o.now()) }

// handlerFunc 是已通过认证的处理器。
type handlerFunc func(w http.ResponseWriter, r *http.Request, ru reqUser)

// secured 包装需要登录的接口：认证 → CSRF（非 GET）→ 能力（cap 为空则只需登录）→ 限速 → 审计拒绝。
func (s *Server) secured(c authz.Cap, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ru, ok := s.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.ops.checkCSRF(ru.Session, r.Header.Get("X-CSRF-Token")) {
				s.auditReq(r, ru.U, "security.csrf", r.URL.Path, "", "CSRF 令牌缺失或不匹配", "denied")
				writeErr(w, http.StatusForbidden, "CSRF 校验失败")
				return
			}
			if org := r.Header.Get("Origin"); org != "" && !sameOrigin(org, r.Host) {
				s.auditReq(r, ru.U, "security.origin", r.URL.Path, "", "跨站请求："+org, "denied")
				writeErr(w, http.StatusForbidden, "拒绝跨站请求")
				return
			}
			if !s.ops.rateOK(ru.U.Name) {
				writeErr(w, http.StatusTooManyRequests, "请求过于频繁")
				return
			}
		}
		if c != "" && !ru.U.Can(s.ops.cfg.Env, c) {
			s.auditReq(r, ru.U, "authz.deny", r.URL.Path, "", "缺少能力 "+string(c), "denied")
			writeErr(w, http.StatusForbidden, "没有权限："+string(c))
			return
		}
		h(w, r, ru)
	}
}

func sameOrigin(origin, host string) bool {
	origin = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return strings.EqualFold(origin, host)
}

// kindCap 是变更种类所需的能力。
func kindCap(kind string) authz.Cap {
	switch kind {
	case change.KindInstOp, change.KindRestartRole:
		return authz.CapInstOp
	case change.KindUpgrade:
		return authz.CapScale
	case change.KindScaleOut, change.KindScaleIn:
		return authz.CapScale
	case change.KindConfig:
		return authz.CapConfig
	case change.KindMaintEnter, change.KindMaintExit, change.KindMaintExtend:
		return authz.CapMaint
	case change.KindBackup, change.KindRestore:
		return authz.CapBackup
	}
	return ""
}
