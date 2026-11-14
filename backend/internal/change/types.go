// @author: kevinkelly
// @license: Apache License 2.0
// @file: types.go
// @time: 2026/10/28

// Package change 实现变更的规划、审批状态机与执行引擎（D1–D6）。
//
// 流程：Preview（规划+前置检查，不落库）→ Create（落库，按风险×环境确定审批级数）→ Approve（逐级）→
// 立即执行或到点执行 → 每个步骤的命令都经 exec.Runner（白名单 + 令牌）执行 → 终态（成功/失败/中止）。
// 本包不做认证与授权：调用方（server）在调用前完成 RBAC 判定，引擎只做状态机、职责分离的兜底校验与执行。
package change

import (
	"encoding/json"
	"time"

	"tidbops/internal/exec"
)

// 变更种类（与前端 intent.kind 一致）。
const (
	KindInstOp      = "inst.op"      // 单实例：restart | stop | start | evict | unevict
	KindScaleOut    = "scale.out"    // 扩容
	KindScaleIn     = "scale.in"     // 缩容
	KindConfig      = "config.set"   // 配置变更
	KindMaintEnter  = "maint.enter"  // 进入主机维护
	KindMaintExit   = "maint.exit"   // 结束主机维护
	KindMaintExtend = "maint.extend" // 延长维护窗口
	KindBackup      = "backup.run"   // 全量备份 / 日志备份启停
	KindRestore     = "backup.restore"
)

// 变更状态。
const (
	StatePending   = "pending"   // 待审批
	StateApproved  = "approved"  // 已批准，等待执行（或排队中）
	StateScheduled = "scheduled" // 已批准且定时，等待到点
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateAborted   = "aborted"
	StateRejected  = "rejected"
	StateCancelled = "cancelled"
	StateExpired   = "expired"
)

// LiveStates 是占用资源的状态（用于冲突检测）。
var LiveStates = map[string]bool{StatePending: true, StateApproved: true, StateScheduled: true, StateRunning: true}

// Check 是一条前置检查结果。
type Check struct {
	Lv string `json:"lv"` // ok | warn | bad | info
	T  string `json:"t"`
}

// Wait 是步骤执行后的等待条件（由 Prober 探测）。
type Wait struct {
	Kind     string        `json:"kind"` // store-state | leader-zero | node-up | no-down-peers | sleep
	Store    int64         `json:"store,omitempty"`
	Node     string        `json:"node,omitempty"` // host:port
	Want     string        `json:"want,omitempty"` // store-state 的期望值：Up | Offline | Tombstone
	Timeout  time.Duration `json:"timeout"`
	Interval time.Duration `json:"interval"`
	Desc     string        `json:"desc,omitempty"`
}

// Step 是计划中的一个步骤。
type Step struct {
	Name         string     `json:"name"`
	Cmds         []exec.Cmd `json:"cmds,omitempty"`
	Wait         *Wait      `json:"wait,omitempty"`
	Manual       string     `json:"manual,omitempty"` // 非空表示需要人工确认后继续（如负载均衡摘除），内容是操作指引
	IgnoreErr    bool       `json:"ignoreErr,omitempty"`
	Irreversible bool       `json:"irreversible,omitempty"` // 开始执行后不可中止
}

// Plan 是一次变更的执行计划。
type Plan struct {
	Title     string     `json:"title"`
	Kind      string     `json:"kind"`
	Risk      string     `json:"risk"` // low | mid | high
	Checks    []Check    `json:"checks"`
	Steps     []Step     `json:"steps"`
	Rollback  []exec.Cmd `json:"rollback,omitempty"` // 失败 / 中止时尽力执行的恢复命令
	Targets   []string   `json:"targets"`            // 实例 ID
	Hosts     []string   `json:"hosts,omitempty"`    // 涉及的主机 IP
	EstMins   int        `json:"estMins"`
	Display   []string   `json:"display"` // 给人看的命令清单
	Abortable bool       `json:"abortable"`
	Blocked   bool       `json:"blocked"`
	Maint     *MaintSpec `json:"maint,omitempty"`
}

// StepState 是步骤的运行状态。
type StepState struct {
	Name    string     `json:"name"`
	State   string     `json:"state"` // wait | run | done | skipped | failed | manual
	Started *time.Time `json:"started,omitempty"`
	Ended   *time.Time `json:"ended,omitempty"`
	Detail  string     `json:"detail,omitempty"`
}

// Approval 是一次审批。
type Approval struct {
	User    string    `json:"user"`
	Level   int       `json:"level"`
	Time    time.Time `json:"time"`
	Comment string    `json:"comment,omitempty"`
}

// Change 是一条变更记录。
type Change struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Cluster     string          `json:"cluster"`
	Env         string          `json:"env"`
	Params      json.RawMessage `json:"params"`
	Note        string          `json:"note,omitempty"`
	Ticket      string          `json:"ticket,omitempty"`
	Risk        string          `json:"risk"`
	Emergency   bool            `json:"emergency,omitempty"`
	Applicant   string          `json:"applicant"`
	Created     time.Time       `json:"created"`
	Need        int             `json:"need"` // 需要的审批级数
	NeedMFA     bool            `json:"needMfa,omitempty"`
	Approvals   []Approval      `json:"approvals"`
	State       string          `json:"state"`
	Reason      string          `json:"reason,omitempty"` // 驳回 / 失败 / 过期原因
	ScheduledAt *time.Time      `json:"scheduledAt,omitempty"`
	Expires     time.Time       `json:"expires"`
	Started     *time.Time      `json:"started,omitempty"`
	Finished    *time.Time      `json:"finished,omitempty"`
	Plan        Plan            `json:"plan"`
	Steps       []StepState     `json:"steps"`
	Log         []string        `json:"log"`
	Cur         int             `json:"cur"`
	MaintID     string          `json:"maintId,omitempty"`
	PostReview  string          `json:"postReview,omitempty"` // 紧急变更的事后复盘
}

// MaintSpec 是维护计划的参数（进入维护时落成 Maint 记录）。
type MaintSpec struct {
	IP        string   `json:"ip"`
	Type      string   `json:"type"`
	Mins      int      `json:"mins"`
	KeepData  bool     `json:"keepData"`
	Reason    string   `json:"reason,omitempty"`
	Strategy  string   `json:"strategy"` // standard | extend
	OrigThr   int      `json:"origThresh"`
	Limit     int      `json:"limit"`
	Nodes     []string `json:"nodes"`
	HasTiKV   bool     `json:"hasTikv"`
	TiKVStore []int64  `json:"tikvStores,omitempty"`
}

// Maint 是一次主机维护的生命周期记录。
type Maint struct {
	ID         string     `json:"id"`
	IP         string     `json:"ip"`
	Type       string     `json:"type"`
	TypeLabel  string     `json:"typeLabel"`
	Mins       int        `json:"mins"`
	ExtendedBy int        `json:"extendedBy"`
	Reason     string     `json:"reason,omitempty"`
	Ticket     string     `json:"ticket,omitempty"`
	Strategy   string     `json:"strategy"`
	OrigThr    int        `json:"origThresh"`
	Limit      int        `json:"limit"`
	HasTiKV    bool       `json:"hasTikv"`
	Nodes      []string   `json:"nodes"`
	Stores     []int64    `json:"stores,omitempty"`
	State      string     `json:"state"` // draining | active | recovering | done | cancelled
	Started    time.Time  `json:"started"`
	ActiveAt   *time.Time `json:"activeAt,omitempty"` // 实例全部停止、开始计时的时间
	Ended      *time.Time `json:"ended,omitempty"`
	Todo       []Todo     `json:"todo"`
	Log        []string   `json:"log"`
	EnterID    string     `json:"enterId"`
	ExitID     string     `json:"exitId,omitempty"`
	Alerted    bool       `json:"alerted,omitempty"`
}

// Todo 是维护检查单的一项。
type Todo struct {
	T    string `json:"t"`
	Done bool   `json:"done"`
}

// MaintLive 判断维护是否仍占用资源。
func MaintLive(s string) bool { return s == "draining" || s == "active" || s == "recovering" }

// Event 是引擎向外（审计、通知）发出的事件。
type Event struct {
	Time     time.Time `json:"time"`
	ChangeID string    `json:"changeId"`
	Type     string    `json:"type"` // created | approved | rejected | cancelled | started | step | succeeded | failed | aborted | expired | command | manual
	User     string    `json:"user,omitempty"`
	Env      string    `json:"env,omitempty"`
	Risk     string    `json:"risk,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Result   string    `json:"result,omitempty"`
}
