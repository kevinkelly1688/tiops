// @author: kevinkelly
// @license: Apache License 2.0
// @file: rbac.go
// @time: 2026/10/28

package authz

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Role 是平台角色（A3 §2.1）。
type Role string

const (
	RoleViewer    Role = "viewer"
	RoleDeveloper Role = "developer"
	RoleSRE       Role = "sre"
	RoleDBA       Role = "dba"
	RoleApprover  Role = "approver"
	RoleAdmin     Role = "admin"
	RoleAuditor   Role = "auditor"
)

// AllRoles 按矩阵列顺序返回全部「内置」角色（固定 7 个，用于矩阵构建）；含自定义角色的列表见 KnownRoles。
func AllRoles() []Role {
	return []Role{RoleViewer, RoleDeveloper, RoleSRE, RoleDBA, RoleApprover, RoleAdmin, RoleAuditor}
}

// Valid 报告 r 是否是已知角色：内置角色，或当前已登记的自定义角色（roles.go）。
func (r Role) Valid() bool {
	if IsBuiltin(r) {
		return true
	}
	_, ok := customRole(r)
	return ok
}

// Cap 是一项能力（A3 §2.2 矩阵的行）。
type Cap string

const (
	CapViewOverview Cap = "view.overview"
	CapViewSQL      Cap = "view.sql"
	CapDiag         Cap = "diag.run"
	CapReport       Cap = "report.export"
	CapInstOp       Cap = "inst.op"
	CapScale        Cap = "scale"
	CapConfig       Cap = "config"
	CapMaint        Cap = "maint"
	CapBackup       Cap = "backup"
	CapSchedCreate  Cap = "sched.create"
	CapApproveMid   Cap = "approve.mid"
	CapApproveHigh  Cap = "approve.high"
	CapRelease      Cap = "release"
	CapSettings     Cap = "settings.edit"
	CapWriteSwitch  Cap = "writeswitch"
	CapUsers        Cap = "users.manage"
	CapAuditView    Cap = "audit.view"
	CapAuditExport  Cap = "audit.export"
	CapShellRun     Cap = "shell.run"     // 中控机命令：执行 / 提交申请 / 取消
	CapShellApprove Cap = "shell.approve" // 中控机命令：批准他人提交的变更命令
)

// AllCaps 按矩阵行顺序返回全部能力。
func AllCaps() []Cap {
	return []Cap{CapViewOverview, CapViewSQL, CapDiag, CapReport, CapInstOp, CapScale, CapConfig, CapMaint,
		CapBackup, CapSchedCreate, CapApproveMid, CapApproveHigh, CapRelease, CapSettings, CapWriteSwitch,
		CapUsers, CapAuditView, CapAuditExport, CapShellRun, CapShellApprove}
}

// Grant 是角色对某能力的授予方式。
type Grant int

const (
	GrantNone     Grant = iota // —
	GrantAllow                 // ✔
	GrantApproval              // 审：可发起，但要按风险×环境走审批
	GrantScoped                // 范：仅限被授权范围（如 developer 的库）
)

// Can 非 None 即 true（注意：Approval 仅表示可发起，Scoped 仅表示范围内可用）。
func (g Grant) Can() bool { return g != GrantNone }

func (g Grant) String() string {
	switch g {
	case GrantAllow:
		return "allow"
	case GrantApproval:
		return "approval"
	case GrantScoped:
		return "scoped"
	}
	return "none"
}

// rank 用于多角色取最强：Allow > Approval > Scoped > None。
func (g Grant) rank() int {
	switch g {
	case GrantAllow:
		return 3
	case GrantApproval:
		return 2
	case GrantScoped:
		return 1
	}
	return 0
}

// 权限矩阵：每行依次对应 viewer, developer, sre, dba, approver, admin, auditor；
// Y=✔  A=审  S=范  .=—。严格对照 A3 §2.2。
var matrixRows = []struct {
	cap   Cap
	cells string
}{
	{CapViewOverview, "YYYYYYY"},
	{CapViewSQL, ".SYYYY."},
	{CapDiag, "..YYYY."},
	{CapReport, "..YYYY."},
	{CapInstOp, "..AA..."},
	{CapScale, "...A..."},
	{CapConfig, "...A..."},
	{CapMaint, "...A..."},
	{CapBackup, "...A..."},
	{CapSchedCreate, "..YY..."},
	{CapApproveMid, "....Y.."},
	{CapApproveHigh, "....Y.."},
	{CapRelease, ".....A."},
	{CapSettings, ".....Y."},
	{CapWriteSwitch, ".....A."},
	{CapUsers, ".....Y."},
	{CapAuditView, "....YYY"},
	{CapAuditExport, "......Y"},
	{CapShellRun, ".....Y."},
	{CapShellApprove, ".....Y."},
}

var matrix = buildMatrix()

func buildMatrix() map[Role]map[Cap]Grant {
	roles := AllRoles()
	m := make(map[Role]map[Cap]Grant, len(roles))
	for _, r := range roles {
		m[r] = make(map[Cap]Grant)
	}
	for _, row := range matrixRows {
		for i, ch := range row.cells {
			g := GrantNone
			switch ch {
			case 'Y':
				g = GrantAllow
			case 'A':
				g = GrantApproval
			case 'S':
				g = GrantScoped
			}
			m[roles[i]][row.cap] = g
		}
	}
	return m
}

// Matrix 返回权限矩阵的副本（修改返回值不影响内部判定）。每个角色都包含全部 20 项能力。
func Matrix() map[Role]map[Cap]Grant {
	out := make(map[Role]map[Cap]Grant, len(matrix))
	for r, row := range matrix {
		c := make(map[Cap]Grant, len(row))
		for k, v := range row {
			c[k] = v
		}
		out[r] = c
	}
	return out
}

// RoleGrant 返回单个角色（内置或自定义）对某能力的授予方式；未知 / 已删除的角色为 GrantNone。
func RoleGrant(r Role, c Cap) Grant { return roleGrant(r, c) }

// 环境常量（Roles 的键；"*" 表示默认）。
const (
	EnvDefault = "*"
	EnvTest    = "test"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// 风险等级。
const (
	RiskLow  = "low"
	RiskMid  = "mid"
	RiskHigh = "high"
)

// User 是平台用户（不含口令哈希与 TOTP 密钥）。
type User struct {
	Name     string            `json:"name"`
	Display  string            `json:"display"`
	Roles    map[string][]Role `json:"roles"` // env → 角色；"*" 为默认，具体环境存在则覆盖 "*"
	DBs      []string          `json:"dbs"`   // developer 被授权的库
	Disabled bool              `json:"disabled"`
	HasTOTP  bool              `json:"hasTotp"`
	Created  time.Time         `json:"created"`

	// 资料（users.manage 可改）。旧 users.json 没有这些字段时均为零值。
	Dept      string     `json:"dept"`
	Email     string     `json:"email"`
	Phone     string     `json:"phone"`
	Note      string     `json:"note"`
	LastLogin *time.Time `json:"lastLogin,omitempty"` // 最近一次登录成功的时间；从未登录为 nil
}

// RolesFor 返回用户在 env 下的角色：env 键存在（即使为空）则用之，否则用 "*"。
func (u User) RolesFor(env string) []Role {
	if rs, ok := u.Roles[env]; ok {
		return append([]Role(nil), rs...)
	}
	return append([]Role(nil), u.Roles[EnvDefault]...)
}

// Grant 返回用户在 env 下对能力 c 的授予方式（多角色取最强）。被禁用的用户一律 GrantNone。
func (u User) Grant(env string, c Cap) Grant {
	if u.Disabled {
		return GrantNone
	}
	best := GrantNone
	for _, r := range u.RolesFor(env) {
		if g := roleGrant(r, c); g.rank() > best.rank() {
			best = g
		}
	}
	return best
}

// Can 等价于 u.Grant(env, c).Can()。
func (u User) Can(env string, c Cap) bool { return u.Grant(env, c).Can() }

// HasRole 报告用户在 env 下是否拥有角色 r。被禁用的用户恒为 false。
func (u User) HasRole(env string, r Role) bool {
	if u.Disabled {
		return false
	}
	for _, x := range u.RolesFor(env) {
		if x == r {
			return true
		}
	}
	return false
}

// CanViewSQLOf 报告用户能否在 env 下查看数据库 db 的 SQL 文本：
// 完整授权角色恒可；developer（范）仅限 DBs 列表内的库。
func (u User) CanViewSQLOf(env, db string) bool {
	switch u.Grant(env, CapViewSQL) {
	case GrantAllow:
		return true
	case GrantScoped:
		for _, d := range u.DBs {
			if strings.EqualFold(d, db) {
				return true
			}
		}
	}
	return false
}

// CanApprove 报告 u 是否具备审批该风险等级变更的能力：mid 需 approve.mid，high 需 approve.high；
// low 无需审批，恒为 false。被禁用的用户恒为 false。
func CanApprove(u User, env, risk string) bool {
	switch risk {
	case RiskMid:
		return u.Grant(env, CapApproveMid).Can()
	case RiskHigh:
		return u.Grant(env, CapApproveHigh).Can()
	}
	return false
}

// 审批校验错误。
var (
	ErrSelfApproval      = errors.New("申请人不能审批自己的变更")
	ErrDuplicateApproval = errors.New("同一人不能重复审批同一变更")
	ErrNotApprover       = errors.New("无权审批该风险等级的变更")
)

// ValidateApproval 校验 approver 对申请人 applicant 的变更（已有审批人 prior）追加审批是否合规：
// 申请人不能自批、同一人不能重复审批、审批人需具备对应能力（且未被禁用）。名字比较不区分大小写。
func ValidateApproval(applicant string, prior []string, approver User, env, risk string) error {
	if strings.EqualFold(strings.TrimSpace(approver.Name), strings.TrimSpace(applicant)) {
		return ErrSelfApproval
	}
	for _, p := range prior {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(approver.Name)) {
			return ErrDuplicateApproval
		}
	}
	if !CanApprove(approver, env, risk) {
		return fmt.Errorf("%w（用户 %s，环境 %s，风险 %s）", ErrNotApprover, approver.Name, env, risk)
	}
	return nil
}
