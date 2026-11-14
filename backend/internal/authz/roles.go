// @author: kevinkelly
// @license: Apache License 2.0
// @file: roles.go
// @time: 2026/10/28

package authz

// 自定义角色（角色 = 能力集合）与能力目录。
//
// 设计要点：
//   - 内置 7 个角色（rbac.go 的 matrix）永远不可修改 / 删除；自定义角色存放在 RoleStore（roles.json），
//     并通过一个包级的、不可变快照注册表（atomic.Pointer）对 Role.Valid / User.Grant / HasRole 等判定函数可见，
//     所以这些函数的签名保持不变，且读路径无锁。
//   - 注册表是进程级单例：同一进程里最后一次 OpenRoleStore / 变更 的结果生效（服务进程只会打开一个 RoleStore）。
//     测试里用 ClearCustomRoles 清理。
//   - 安全硬下限（API 与存储加载都校验，见 ValidateCaps / sanitizeLoaded）：
//     自定义角色不得授予「仅管理员」能力 users.manage / writeswitch / settings.edit；
//     每项能力的授予方式受 AllowedGrants 限制；必须含 view.overview；
//     操作者只能授予自己已拥有且授予方式不低于所授予方式的能力（admin 除外，但「仅管理员」下限对所有人生效）。
//   - 被删除（或存储里丢失）的自定义角色：持有它的用户该角色按「无权限」处理（Role.Valid 为 false，Grant 为 none）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// 自定义角色的限制。
const (
	MaxCustomRoles   = 50
	MaxRoleNameRunes = 40
	MaxRoleDescRunes = 200
)

var roleIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// ---------------------------------------------------------------- 内置角色文案 ----

// builtinMeta 是内置角色的中文名与说明，文案与前端 security.js 的 SEC_ROLES、A3 §2.1 一致。
var builtinMeta = map[Role]struct{ name, desc string }{
	RoleViewer:    {"访客", "只读：看总览、拓扑"},
	RoleDeveloper: {"开发", "业务开发：只看自己被授权的库的慢 SQL 与执行计划"},
	RoleSRE:       {"值班 SRE", "值班：看全部、跑诊断巡检、申请实例级操作"},
	RoleDBA:       {"DBA", "变更执行：发起并执行扩缩容、配置、维护、备份等变更（经审批）"},
	RoleApprover:  {"审批人", "把关：审批中 / 高风险变更；不能执行变更"},
	RoleAdmin:     {"管理员", "平台管理：用户与角色、平台设置；不能直接做集群变更"},
	RoleAuditor:   {"审计员", "审计：只读审计日志并导出"},
}

// IsBuiltin 报告 r 是否是内置角色。
func IsBuiltin(r Role) bool { _, ok := matrix[r]; return ok }

// BuiltinRoleInfo 返回内置角色的中文名与说明；非内置角色 ok=false。
func BuiltinRoleInfo(r Role) (name, desc string, ok bool) {
	m, ok := builtinMeta[r]
	return m.name, m.desc, ok
}

// ---------------------------------------------------------------- 能力目录 ----

// 风险等级（目录里给前端展示用，与审批风险 low/mid/high 不是同一个概念：none 表示只读 / 无集群影响）。
const (
	CapRiskNone = "none"
	CapRiskMid  = "mid"
	CapRiskHigh = "high"
)

// CapInfo 是能力目录里的一项。
type CapInfo struct {
	Cap           Cap      `json:"cap"`
	Name          string   `json:"name"`
	Desc          string   `json:"desc"`
	Risk          string   `json:"risk"`          // none / mid / high
	AllowedGrants []string `json:"allowedGrants"` // 自定义角色可选的授予方式，含 "none"
	Locked        bool     `json:"locked"`        // true = 仅内置管理员角色，自定义角色不可授予
}

// CapGroup 是能力目录的一个分组。
type CapGroup struct {
	Group string    `json:"group"`
	Caps  []CapInfo `json:"caps"`
}

type capMeta struct {
	cap    Cap
	name   string
	desc   string
	risk   string
	grants []Grant // 自定义角色可选授予方式（不含 none）；空 + locked=true 表示不可授予
	locked bool
}

var gAllow = []Grant{GrantAllow}
var gAllowApproval = []Grant{GrantAllow, GrantApproval}

var capGroups = []struct {
	name string
	caps []capMeta
}{
	{"查看与诊断", []capMeta{
		{CapViewOverview, "查看总览 / 拓扑", "查看集群总览、拓扑与健康状态；所有角色的最低要求，缺少它用户登录后什么都看不到", CapRiskNone, gAllow, false},
		{CapViewSQL, "查看慢 SQL / 执行计划", "查看含 SQL 文本的慢查询与执行计划（可能含业务数据）；「范围」仅限用户被授权的库", CapRiskMid, []Grant{GrantAllow, GrantScoped}, false},
		{CapDiag, "运行诊断 / 巡检", "运行只读诊断与巡检，会对集群发起只读查询", CapRiskNone, gAllow, false},
		{CapReport, "导出诊断 / 巡检报告", "导出诊断与巡检报告", CapRiskNone, gAllow, false},
	}},
	{"运维变更", []capMeta{
		{CapInstOp, "单实例操作", "重启 / 停止 / 启动实例、驱逐 Leader；「需审批」表示可发起，须按风险×环境走审批", CapRiskHigh, gAllowApproval, false},
		{CapScale, "扩缩容 / 升级", "扩容、缩容、新增主机、集群升级；「需审批」表示可发起，须按风险×环境走审批", CapRiskHigh, gAllowApproval, false},
		{CapConfig, "配置变更", "修改集群配置（在线 / 需 reload）；「需审批」表示可发起，须按风险×环境走审批", CapRiskHigh, gAllowApproval, false},
		{CapMaint, "主机维护", "进入 / 退出 / 延长主机维护；「需审批」表示可发起，须按风险×环境走审批", CapRiskHigh, gAllowApproval, false},
		{CapBackup, "备份恢复操作", "发起备份与恢复；「需审批」表示可发起，须按风险×环境走审批", CapRiskHigh, gAllowApproval, false},
	}},
	{"排期与审批", []capMeta{
		{CapSchedCreate, "创建变更排期", "创建变更排期（执行时仍要走对应能力的鉴权）", CapRiskMid, gAllow, false},
		{CapApproveMid, "审批中风险变更", "审批中风险变更；申请人不能审批自己的变更", CapRiskMid, gAllow, false},
		{CapApproveHigh, "审批高风险变更", "审批高风险变更（多级审批）；申请人不能审批自己的变更", CapRiskHigh, gAllow, false},
	}},
	{"平台管理", []capMeta{
		{CapRelease, "平台发布", "平台自身的发布流程；「需审批」表示须经审批", CapRiskHigh, gAllowApproval, false},
		{CapSettings, "修改平台设置", "修改冻结期、变更窗口、通知渠道、审批策略等；仅内置管理员角色", CapRiskMid, nil, true},
		{CapWriteSwitch, "开启 / 关闭写操作开关", "环境写操作总开关（开启需两名管理员复核）；仅内置管理员角色", CapRiskHigh, nil, true},
		{CapUsers, "用户与角色管理", "管理用户、角色与权限；仅内置管理员角色", CapRiskHigh, nil, true},
		{CapShellRun, "中控机命令：执行", "在「中控机命令」页以 tidb 身份向中控机执行命令、提交变更申请、取消自己的任务；只读命令直接执行，变更命令要另一名持有「批准变更」的用户批准；等同于在中控机上的 tidb（有 sudo 免密）权限，只给确实需要的人", CapRiskHigh, gAllow, false},
		{CapShellApprove, "中控机命令：批准变更", "批准他人在「中控机命令」页提交的变更命令（申请人不能批准自己的申请）；批准人应看清命令与目录", CapRiskHigh, gAllow, false},
	}},
	{"审计", []capMeta{
		{CapAuditView, "查看审计日志", "查看并校验审计日志", CapRiskNone, gAllow, false},
		{CapAuditExport, "导出审计日志", "导出审计日志（含全部操作痕迹）", CapRiskMid, gAllow, false},
	}},
}

func findCapMeta(c Cap) (capMeta, bool) {
	for _, g := range capGroups {
		for _, m := range g.caps {
			if m.cap == c {
				return m, true
			}
		}
	}
	return capMeta{}, false
}

// Catalog 返回按组列出的能力目录（每次返回新副本）。
func Catalog() []CapGroup {
	out := make([]CapGroup, 0, len(capGroups))
	for _, g := range capGroups {
		cg := CapGroup{Group: g.name, Caps: make([]CapInfo, 0, len(g.caps))}
		for _, m := range g.caps {
			ag := []string{GrantNone.String()}
			for _, x := range m.grants {
				ag = append(ag, x.String())
			}
			cg.Caps = append(cg.Caps, CapInfo{Cap: m.cap, Name: m.name, Desc: m.desc, Risk: m.risk, AllowedGrants: ag, Locked: m.locked})
		}
		out = append(out, cg)
	}
	return out
}

// CapKnown 报告 c 是否是已知能力。
func CapKnown(c Cap) bool { _, ok := findCapMeta(c); return ok }

// IsLockedCap 报告 c 是否仅限内置管理员角色（自定义角色不可授予）：users.manage / writeswitch / settings.edit。
func IsLockedCap(c Cap) bool { m, ok := findCapMeta(c); return ok && m.locked }

// AllowedGrants 返回自定义角色对能力 c 可选的授予方式（不含 GrantNone）；locked 或未知能力返回 nil。
func AllowedGrants(c Cap) []Grant {
	m, ok := findCapMeta(c)
	if !ok || m.locked {
		return nil
	}
	return append([]Grant(nil), m.grants...)
}

// ParseGrant 解析 "none" / "allow" / "approval" / "scoped"。
func ParseGrant(s string) (Grant, bool) {
	switch strings.TrimSpace(s) {
	case "none", "":
		return GrantNone, true
	case "allow":
		return GrantAllow, true
	case "approval":
		return GrantApproval, true
	case "scoped":
		return GrantScoped, true
	}
	return GrantNone, false
}

// ---------------------------------------------------------------- 错误 ----

// RoleError 是自定义角色操作的错误。Code 见下面的常量；Members 仅在 in_use 时给出持有该角色的用户名。
type RoleError struct {
	Code    string
	Msg     string
	Members []string
}

func (e *RoleError) Error() string { return e.Msg }

// Is 让 errors.Is(err, ErrRoleXxx) 按 Code 匹配。
func (e *RoleError) Is(target error) bool {
	t, ok := target.(*RoleError)
	return ok && t.Code == e.Code
}

// RoleError 的 Code。
const (
	RoleErrInvalid    = "invalid"    // 参数不合法（id / 名称 / 能力 / 授予方式 / 缺少 view.overview）
	RoleErrBuiltin    = "builtin"    // 内置角色不可修改 / 删除
	RoleErrLockedCap  = "locked_cap" // 试图授予仅管理员能力
	RoleErrEscalation = "escalation" // 防提权：授予了自己没有的能力
	RoleErrExists     = "exists"     // id 或名称重复
	RoleErrLimit      = "limit"      // 自定义角色数量达到上限
	RoleErrInUse      = "in_use"     // 仍有用户持有该角色
	RoleErrNotFound   = "not_found"
)

// 哨兵错误，仅用于 errors.Is。
var (
	ErrRoleInvalid    = &RoleError{Code: RoleErrInvalid}
	ErrRoleBuiltin    = &RoleError{Code: RoleErrBuiltin}
	ErrRoleLockedCap  = &RoleError{Code: RoleErrLockedCap}
	ErrRoleEscalation = &RoleError{Code: RoleErrEscalation}
	ErrRoleExists     = &RoleError{Code: RoleErrExists}
	ErrRoleLimit      = &RoleError{Code: RoleErrLimit}
	ErrRoleInUse      = &RoleError{Code: RoleErrInUse}
	ErrRoleNotFound   = &RoleError{Code: RoleErrNotFound}
)

func roleErr(code, format string, a ...any) *RoleError {
	return &RoleError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// ---------------------------------------------------------------- 模型与校验 ----

// CustomRole 是自定义角色。Caps 只存非 none 的能力。
type CustomRole struct {
	ID        string
	Name      string
	Desc      string
	Caps      map[Cap]Grant
	Created   time.Time
	Updated   time.Time
	UpdatedBy string
}

func (c CustomRole) clone() CustomRole {
	c.Caps = cloneCaps(c.Caps)
	return c
}

func cloneCaps(m map[Cap]Grant) map[Cap]Grant {
	out := make(map[Cap]Grant, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// CapStrings 把能力集合转成 {cap: "allow"|"approval"|"scoped"}（按 JSON 输出用）。
func CapStrings(m map[Cap]Grant) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != GrantNone {
			out[string(k)] = v.String()
		}
	}
	return out
}

// ParseCapMap 把 {cap: grant 字符串} 解析成能力集合。保留值为 none 的项（便于「覆盖」继承来的能力），
// 未知能力或未知授予方式返回 invalid 错误。
func ParseCapMap(in map[string]string) (map[Cap]Grant, error) {
	out := make(map[Cap]Grant, len(in))
	for k, v := range in {
		c := Cap(strings.TrimSpace(k))
		if !CapKnown(c) {
			return nil, roleErr(RoleErrInvalid, "未知能力 %q", k)
		}
		g, ok := ParseGrant(v)
		if !ok {
			return nil, roleErr(RoleErrInvalid, "能力 %s 的授予方式 %q 不合法（可选 none / allow / approval / scoped）", c, v)
		}
		out[c] = g
	}
	return out, nil
}

// NormalizeCaps 返回去掉 none 项的副本。
func NormalizeCaps(m map[Cap]Grant) map[Cap]Grant {
	out := make(map[Cap]Grant, len(m))
	for k, v := range m {
		if v != GrantNone {
			out[k] = v
		}
	}
	return out
}

func grantAllowedFor(c Cap, g Grant) bool {
	for _, x := range AllowedGrants(c) {
		if x == g {
			return true
		}
	}
	return false
}

// ValidateCaps 校验自定义角色的能力集合（none 项忽略）：未知能力；仅管理员能力（users.manage / writeswitch /
// settings.edit，硬下限）；每项能力允许的授予方式；必须含 view.overview。返回 *RoleError。
func ValidateCaps(caps map[Cap]Grant) error {
	names := make([]string, 0, len(caps))
	for c := range caps {
		names = append(names, string(c))
	}
	sort.Strings(names) // 错误信息稳定
	for _, n := range names {
		c, g := Cap(n), caps[Cap(n)]
		if !CapKnown(c) {
			return roleErr(RoleErrInvalid, "未知能力 %q", n)
		}
		if g == GrantNone {
			continue
		}
		if IsLockedCap(c) {
			return roleErr(RoleErrLockedCap, "能力 %s 仅限内置管理员角色，自定义角色不可授予", c)
		}
		if !grantAllowedFor(c, g) {
			return roleErr(RoleErrInvalid, "能力 %s 不支持授予方式 %s（可选：%s）", c, g, grantList(c))
		}
	}
	if caps[CapViewOverview] != GrantAllow {
		return roleErr(RoleErrInvalid, "角色必须包含 view.overview（否则用户登录后什么都看不到）")
	}
	return nil
}

func grantList(c Cap) string {
	var s []string
	for _, g := range AllowedGrants(c) {
		s = append(s, g.String())
	}
	return strings.Join(s, " / ")
}

// CheckGrantable 是防提权校验：对 next 中相对 prev「新增或提升」的每项能力，操作者 actor 在 env 下必须已拥有该能力，
// 且其授予方式不低于所授予方式（allow > approval > scoped）。内置 admin 角色的持有者不受此限（但 ValidateCaps 的
// 「仅管理员能力」下限对所有人生效，需另行调用）。prev 为 nil 表示新建（next 的全部能力都要检查）。
func CheckGrantable(actor User, env string, prev, next map[Cap]Grant) error {
	if actor.HasRole(env, RoleAdmin) {
		return nil
	}
	for _, c := range AllCaps() {
		g := next[c]
		if g == GrantNone || g.rank() <= prev[c].rank() {
			continue
		}
		if actor.Grant(env, c).rank() < g.rank() {
			return roleErr(RoleErrEscalation, "不能授予自己没有的权限：%s（%s）", c, g)
		}
	}
	return nil
}

// DiffCaps 返回两个能力集合的差异，供审计摘要：新增「+cap(grant)」、移除「−cap」、授予方式变化「~cap(旧→新)」。
func DiffCaps(before, after map[Cap]Grant) []string {
	var out []string
	for _, c := range AllCaps() {
		b, a := before[c], after[c]
		switch {
		case b == a:
		case b == GrantNone:
			out = append(out, fmt.Sprintf("+%s(%s)", c, a))
		case a == GrantNone:
			out = append(out, fmt.Sprintf("−%s", c))
		default:
			out = append(out, fmt.Sprintf("~%s(%s→%s)", c, b, a))
		}
	}
	return out
}

func hasControl(s string, allowNewline bool) bool {
	for _, r := range s {
		if unicode.IsControl(r) && !(allowNewline && (r == '\n' || r == '\r' || r == '\t')) {
			return true
		}
	}
	return false
}

func normRoleName(name string) (string, error) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n < 1 || n > MaxRoleNameRunes {
		return "", roleErr(RoleErrInvalid, "角色名称需 1–%d 个字", MaxRoleNameRunes)
	}
	if hasControl(name, false) {
		return "", roleErr(RoleErrInvalid, "角色名称不能包含控制字符")
	}
	return name, nil
}

func normRoleDesc(desc string) (string, error) {
	desc = strings.TrimSpace(desc)
	if utf8.RuneCountInString(desc) > MaxRoleDescRunes {
		return "", roleErr(RoleErrInvalid, "角色说明不能超过 %d 个字", MaxRoleDescRunes)
	}
	if hasControl(desc, true) {
		return "", roleErr(RoleErrInvalid, "角色说明不能包含控制字符")
	}
	return desc, nil
}

func nameKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// reservedName 报告 name（不区分大小写）是否与内置角色的显示名或 id 相同。
func reservedName(name string) bool {
	k := nameKey(name)
	for r, m := range builtinMeta {
		if k == nameKey(m.name) || k == string(r) {
			return true
		}
	}
	return false
}

// ValidRoleID 报告 id 是否合法且不与内置角色冲突。
func ValidRoleID(id string) error {
	if !roleIDRe.MatchString(id) {
		return roleErr(RoleErrInvalid, "角色 ID 需 2–32 位，以小写字母开头，只能包含小写字母、数字、_ 和 -")
	}
	if IsBuiltin(Role(id)) {
		return roleErr(RoleErrExists, "角色 ID %q 与内置角色重名", id)
	}
	return nil
}

// ---------------------------------------------------------------- 全局注册表 ----

// registry 是不可变快照：发布后永远不会被原地修改，读路径无锁。
type registry struct{ m map[Role]*CustomRole }

var customReg atomic.Pointer[registry]

func publish(roles map[string]*CustomRole) {
	m := make(map[Role]*CustomRole, len(roles))
	for id, r := range roles {
		c := r.clone()
		m[Role(id)] = &c
	}
	customReg.Store(&registry{m: m})
}

// ClearCustomRoles 清空注册表（测试 / 关闭用）。
func ClearCustomRoles() { customReg.Store(&registry{m: map[Role]*CustomRole{}}) }

func customRole(r Role) (*CustomRole, bool) {
	reg := customReg.Load()
	if reg == nil {
		return nil, false
	}
	c, ok := reg.m[r]
	return c, ok
}

// roleGrant 是 User.Grant 等使用的单角色判定：内置角色查 matrix，自定义角色查注册表，其余（含已删除的自定义角色）为 none。
func roleGrant(r Role, c Cap) Grant {
	if row, ok := matrix[r]; ok {
		return row[c]
	}
	if cr, ok := customRole(r); ok {
		return cr.Caps[c]
	}
	return GrantNone
}

// RoleCaps 返回任意已知角色（内置或自定义）的非 none 能力集合副本；未知角色 ok=false。
func RoleCaps(r Role) (map[Cap]Grant, bool) {
	if row, ok := matrix[r]; ok {
		return NormalizeCaps(row), true
	}
	if cr, ok := customRole(r); ok {
		return cloneCaps(cr.Caps), true
	}
	return nil, false
}

// CopyBase 返回 from 角色的能力集合作为新自定义角色的底：去掉仅管理员（locked）能力。
func CopyBase(from Role) (map[Cap]Grant, error) {
	caps, ok := RoleCaps(from)
	if !ok {
		return nil, roleErr(RoleErrInvalid, "copyFrom 指定的角色 %q 不存在", string(from))
	}
	for c := range caps {
		if IsLockedCap(c) {
			delete(caps, c)
		}
	}
	return caps, nil
}

// KnownRoles 返回全部已知角色：内置 7 个（矩阵列顺序）在前，自定义角色按 id 排序在后。
func KnownRoles() []Role {
	out := AllRoles()
	if reg := customReg.Load(); reg != nil {
		var ids []string
		for r := range reg.m {
			ids = append(ids, string(r))
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, Role(id))
		}
	}
	return out
}

// RoleDisplayName 返回角色的中文名（内置取内置文案，自定义取其名称）；未知角色 ok=false，name 为 id 本身。
func RoleDisplayName(r Role) (name string, builtin, ok bool) {
	if m, ok := builtinMeta[r]; ok {
		return m.name, true, true
	}
	if cr, ok := customRole(r); ok {
		return cr.Name, false, true
	}
	return string(r), false, false
}

// RoleMembers 返回在任一环境持有角色 id 的用户名（含被禁用的用户；排序）。
func RoleMembers(users []User, id Role) []string {
	out := []string{}
	for _, u := range users {
		found := false
		for _, rs := range u.Roles {
			for _, r := range rs {
				if r == id {
					found = true
				}
			}
		}
		if found {
			out = append(out, u.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- 持久化 ----

type diskRole struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Desc      string            `json:"desc"`
	Caps      map[string]string `json:"caps"`
	Created   time.Time         `json:"created"`
	Updated   time.Time         `json:"updated"`
	UpdatedBy string            `json:"updatedBy"`
}

type diskRoleFile struct {
	Version int        `json:"version"`
	Roles   []diskRole `json:"roles"`
}

// RoleStore 是自定义角色库：JSON 文件（0600，原子替换写入，并发安全）。每次成功修改后发布到全局注册表。
type RoleStore struct {
	mu       sync.Mutex
	path     string // 空 = 仅内存
	roles    map[string]*CustomRole
	warnings []string
}

// OpenRoleStore 打开自定义角色库并把内容发布到全局注册表。path 为空表示仅内存；文件不存在视为空库
// （首次写入时才创建）。文件无法解析时返回错误；内容不合法的部分按「降级」处理而不是报错——
// 仅管理员能力、非法授予方式、未知能力被丢弃，非法 / 重复的角色被丢弃，原因见 Warnings。
func OpenRoleStore(path string) (*RoleStore, error) {
	s := &RoleStore{path: path, roles: map[string]*CustomRole{}}
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, err
		default:
			var f diskRoleFile
			if err := json.Unmarshal(b, &f); err != nil {
				return nil, fmt.Errorf("角色库 %s 无法解析：%w", path, err)
			}
			s.load(f)
			_ = os.Chmod(path, 0o600)
		}
	}
	publish(s.roles)
	return s, nil
}

// load 把磁盘内容规整到内存，所有降级都记入 warnings。任何降级都只会减少权限，不会增加。
func (s *RoleStore) load(f diskRoleFile) {
	warn := func(format string, a ...any) { s.warnings = append(s.warnings, fmt.Sprintf(format, a...)) }
	seenName := map[string]string{}
	for _, d := range f.Roles {
		if !roleIDRe.MatchString(d.ID) || IsBuiltin(Role(d.ID)) {
			warn("角色 %q：ID 不合法或与内置角色重名，已丢弃", d.ID)
			continue
		}
		if _, dup := s.roles[d.ID]; dup {
			warn("角色 %q：ID 重复，已丢弃后一条", d.ID)
			continue
		}
		if len(s.roles) >= MaxCustomRoles {
			warn("角色 %q：超过上限 %d 个，已丢弃", d.ID, MaxCustomRoles)
			continue
		}
		caps := map[Cap]Grant{}
		keys := make([]string, 0, len(d.Caps))
		for k := range d.Caps {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := Cap(k)
			g, ok := ParseGrant(d.Caps[k])
			switch {
			case !CapKnown(c):
				warn("角色 %s：未知能力 %q，已丢弃", d.ID, k)
			case !ok:
				warn("角色 %s：能力 %s 的授予方式 %q 不合法，已丢弃该能力", d.ID, k, d.Caps[k])
			case g == GrantNone:
			case IsLockedCap(c):
				warn("角色 %s：存储里含仅管理员能力 %s，已丢弃（防提权）", d.ID, k)
			case !grantAllowedFor(c, g):
				warn("角色 %s：能力 %s 的授予方式 %s 不合法，已丢弃该能力", d.ID, k, g)
			default:
				caps[c] = g
			}
		}
		if caps[CapViewOverview] != GrantAllow {
			warn("角色 %s：缺少 view.overview，该角色的成员登录后将看不到任何内容，请编辑补全", d.ID)
		}
		name := strings.TrimSpace(d.Name)
		if name == "" || utf8.RuneCountInString(name) > MaxRoleNameRunes || hasControl(name, false) {
			warn("角色 %s：名称不合法，已改用 ID 作为名称", d.ID)
			name = d.ID
		}
		if reservedName(name) {
			warn("角色 %s：名称 %q 与内置角色重名，已改用 ID 作为名称", d.ID, name)
			name = d.ID
		}
		if other, dup := seenName[nameKey(name)]; dup {
			warn("角色 %s：名称 %q 与角色 %s 重复，已改用 ID 作为名称", d.ID, name, other)
			name = d.ID
		}
		seenName[nameKey(name)] = d.ID
		desc := strings.TrimSpace(d.Desc)
		if utf8.RuneCountInString(desc) > MaxRoleDescRunes {
			desc = string([]rune(desc)[:MaxRoleDescRunes])
			warn("角色 %s：说明过长，已截断", d.ID)
		}
		s.roles[d.ID] = &CustomRole{ID: d.ID, Name: name, Desc: desc, Caps: caps, Created: d.Created, Updated: d.Updated, UpdatedBy: d.UpdatedBy}
	}
}

// Warnings 返回加载时的降级告警（可能为空）。
func (s *RoleStore) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

func writeFileAtomic(path, pattern string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
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
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (s *RoleStore) save(roles map[string]*CustomRole) error {
	if s.path == "" {
		return nil
	}
	ids := make([]string, 0, len(roles))
	for id := range roles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	f := diskRoleFile{Version: 1, Roles: make([]diskRole, 0, len(ids))}
	for _, id := range ids {
		r := roles[id]
		f.Roles = append(f.Roles, diskRole{ID: r.ID, Name: r.Name, Desc: r.Desc, Caps: CapStrings(r.Caps), Created: r.Created, Updated: r.Updated, UpdatedBy: r.UpdatedBy})
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, ".roles-*.tmp", b)
}

func (s *RoleStore) cloneRoles() map[string]*CustomRole {
	m := make(map[string]*CustomRole, len(s.roles))
	for k, v := range s.roles {
		c := v.clone()
		m[k] = &c
	}
	return m
}

// commit 落盘成功后才替换内存并发布到注册表。
func (s *RoleStore) commit(roles map[string]*CustomRole) error {
	if err := s.save(roles); err != nil {
		return err
	}
	s.roles = roles
	publish(roles)
	return nil
}

// List 返回全部自定义角色（按创建时间、id 排序）。
func (s *RoleStore) List() []CustomRole {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CustomRole, 0, len(s.roles))
	for _, r := range s.roles {
		out = append(out, r.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get 返回一个自定义角色（副本）。
func (s *RoleStore) Get(id string) (CustomRole, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.roles[id]
	if !ok {
		return CustomRole{}, false
	}
	return r.clone(), true
}

func (s *RoleStore) nameTaken(name, exceptID string) bool {
	if reservedName(name) {
		return true
	}
	k := nameKey(name)
	for id, r := range s.roles {
		if id != exceptID && nameKey(r.Name) == k {
			return true
		}
	}
	return false
}

// RoleInput 是新建自定义角色的参数。Caps 里值为 none 的项会被忽略。
type RoleInput struct {
	ID, Name, Desc string
	Caps           map[Cap]Grant
}

// Create 新建自定义角色。校验顺序：id → 名称 / 说明 → 能力（未知 / 仅管理员 / 授予方式 / view.overview）→
// 防提权（actor 在 env 下）→ 数量上限与唯一性。返回 *RoleError。
func (s *RoleStore) Create(actor User, env string, in RoleInput, now time.Time) (CustomRole, error) {
	if err := ValidRoleID(in.ID); err != nil {
		return CustomRole{}, err
	}
	name, err := normRoleName(in.Name)
	if err != nil {
		return CustomRole{}, err
	}
	desc, err := normRoleDesc(in.Desc)
	if err != nil {
		return CustomRole{}, err
	}
	caps := NormalizeCaps(in.Caps)
	if err := ValidateCaps(caps); err != nil {
		return CustomRole{}, err
	}
	if err := CheckGrantable(actor, env, nil, caps); err != nil {
		return CustomRole{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.roles[in.ID]; dup {
		return CustomRole{}, roleErr(RoleErrExists, "角色 ID %q 已存在", in.ID)
	}
	if s.nameTaken(name, "") {
		return CustomRole{}, roleErr(RoleErrExists, "角色名称 %q 已被使用（名称不区分大小写，且不能与内置角色同名）", name)
	}
	if len(s.roles) >= MaxCustomRoles {
		return CustomRole{}, roleErr(RoleErrLimit, "自定义角色最多 %d 个", MaxCustomRoles)
	}
	now = now.UTC()
	r := &CustomRole{ID: in.ID, Name: name, Desc: desc, Caps: caps, Created: now, Updated: now, UpdatedBy: actor.Name}
	roles := s.cloneRoles()
	roles[r.ID] = r
	if err := s.commit(roles); err != nil {
		return CustomRole{}, err
	}
	return r.clone(), nil
}

// RoleUpdate 是更新自定义角色的参数：nil 字段表示不修改；Caps 非 nil 表示整体替换能力集合。
type RoleUpdate struct {
	Name, Desc *string
	Caps       map[Cap]Grant
}

// Update 修改自定义角色，返回修改前后的副本。内置角色返回 builtin 错误。
// 防提权只针对相对旧值「新增或提升」的能力。能力集合不变时不写盘。
func (s *RoleStore) Update(actor User, env, id string, up RoleUpdate, now time.Time) (before, after CustomRole, err error) {
	if IsBuiltin(Role(id)) {
		return before, after, roleErr(RoleErrBuiltin, "内置角色不可修改")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.roles[id]
	if !ok {
		return before, after, roleErr(RoleErrNotFound, "角色 %q 不存在", id)
	}
	next := cur.clone()
	if up.Name != nil {
		if next.Name, err = normRoleName(*up.Name); err != nil {
			return before, after, err
		}
		if s.nameTaken(next.Name, id) {
			return before, after, roleErr(RoleErrExists, "角色名称 %q 已被使用（名称不区分大小写，且不能与内置角色同名）", next.Name)
		}
	}
	if up.Desc != nil {
		if next.Desc, err = normRoleDesc(*up.Desc); err != nil {
			return before, after, err
		}
	}
	if up.Caps != nil {
		next.Caps = NormalizeCaps(up.Caps)
		if err := ValidateCaps(next.Caps); err != nil {
			return before, after, err
		}
		if err := CheckGrantable(actor, env, cur.Caps, next.Caps); err != nil {
			return before, after, err
		}
	}
	before = cur.clone()
	if next.Name == cur.Name && next.Desc == cur.Desc && len(DiffCaps(cur.Caps, next.Caps)) == 0 {
		return before, before, nil
	}
	next.Updated, next.UpdatedBy = now.UTC(), actor.Name
	roles := s.cloneRoles()
	roles[id] = &next
	if err := s.commit(roles); err != nil {
		return before, after, err
	}
	return before, next.clone(), nil
}

// Delete 删除自定义角色。内置角色返回 builtin；仍有用户（users 里任一环境）持有则返回 in_use 并带成员名单。
// users 可为 nil（视为没有成员）。
func (s *RoleStore) Delete(id string, users *Store) (CustomRole, error) {
	if IsBuiltin(Role(id)) {
		return CustomRole{}, roleErr(RoleErrBuiltin, "内置角色不可删除")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.roles[id]
	if !ok {
		return CustomRole{}, roleErr(RoleErrNotFound, "角色 %q 不存在", id)
	}
	if users != nil {
		if m := RoleMembers(users.List(), Role(id)); len(m) > 0 {
			return CustomRole{}, &RoleError{Code: RoleErrInUse, Members: m,
				Msg: fmt.Sprintf("仍有 %d 个用户持有该角色（%s），请先移除", len(m), strings.Join(m, "、"))}
		}
	}
	roles := s.cloneRoles()
	delete(roles, id)
	if err := s.commit(roles); err != nil {
		return CustomRole{}, err
	}
	return cur.clone(), nil
}
