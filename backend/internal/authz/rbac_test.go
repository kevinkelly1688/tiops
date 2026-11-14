// @author: kevinkelly
// @license: Apache License 2.0
// @file: rbac_test.go
// @time: 2026/10/28

package authz

import (
	"errors"
	"testing"
)

func TestMain(m *testing.M) {
	passwordIterations = 1000 // 加速测试；默认值在 TestPasswordHashFormat 里单独验证
	m.Run()
}

// A3 §2.2 权限矩阵，逐行逐列手工抄录：列依次为
// viewer, developer, sre, dba, approver, admin, auditor；Y=✔ A=审 S=范 .=—。
var wantMatrix = []struct {
	cap   Cap
	cells string
}{
	{"view.overview", "YYYYYYY"}, // 查看总览 / 拓扑
	{"view.sql", ".SYYYY."},      // 查看慢 SQL / 执行计划
	{"diag.run", "..YYYY."},      // 运行诊断 / 巡检
	{"report.export", "..YYYY."}, // 导出诊断 / 巡检报告
	{"inst.op", "..AA..."},       // 单实例操作
	{"scale", "...A..."},         // 扩缩容 / 新增主机
	{"config", "...A..."},        // 配置变更
	{"maint", "...A..."},         // 主机维护
	{"backup", "...A..."},        // 备份恢复操作
	{"sched.create", "..YY..."},  // 创建变更排期
	{"approve.mid", "....Y.."},   // 审批中风险变更
	{"approve.high", "....Y.."},  // 审批高风险变更
	{"release", ".....A."},       // 平台发布
	{"settings.edit", ".....Y."}, // 修改平台设置
	{"writeswitch", ".....A."},   // 开关生产写操作
	{"users.manage", ".....Y."},  // 用户与角色管理
	{"audit.view", "....YYY"},    // 查看审计日志
	{"audit.export", "......Y"},  // 导出审计日志
	{"shell.run", ".....Y."},     // 中控机命令：执行
	{"shell.approve", ".....Y."}, // 中控机命令：批准变更
}

func TestMatrixMatchesA3(t *testing.T) {
	roles := []Role{"viewer", "developer", "sre", "dba", "approver", "admin", "auditor"}
	m := Matrix()
	if len(m) != 7 {
		t.Fatalf("角色数=%d", len(m))
	}
	if len(wantMatrix) != 20 || len(AllCaps()) != 20 {
		t.Fatalf("能力数不对")
	}
	for _, row := range wantMatrix {
		for i, ch := range row.cells {
			want := map[rune]Grant{'Y': GrantAllow, 'A': GrantApproval, 'S': GrantScoped, '.': GrantNone}[ch]
			if got := m[roles[i]][row.cap]; got != want {
				t.Errorf("%s × %s：got %v want %v", roles[i], row.cap, got, want)
			}
			if got := RoleGrant(roles[i], row.cap); got != want {
				t.Errorf("RoleGrant %s × %s：got %v want %v", roles[i], row.cap, got, want)
			}
		}
	}
	// 能力常量字符串与规格一致
	if CapViewOverview != "view.overview" || CapApproveHigh != "approve.high" || CapAuditExport != "audit.export" {
		t.Fatal("能力常量值不对")
	}
	// 职责分离
	if m[RoleAdmin][CapScale] != GrantNone || m[RoleAdmin][CapConfig] != GrantNone || m[RoleAdmin][CapInstOp] != GrantNone {
		t.Fatal("管理员不应能直接执行集群变更")
	}
	for _, c := range []Cap{CapInstOp, CapScale, CapConfig, CapMaint, CapBackup} {
		if m[RoleApprover][c] != GrantNone {
			t.Fatalf("审批人不应能执行 %s", c)
		}
	}
}

func TestMatrixIsCopy(t *testing.T) {
	m := Matrix()
	m[RoleViewer][CapUsers] = GrantAllow
	if Matrix()[RoleViewer][CapUsers] != GrantNone {
		t.Fatal("Matrix 应返回副本")
	}
	if (User{Roles: map[string][]Role{"*": {RoleViewer}}}).Grant("prod", CapUsers) != GrantNone {
		t.Fatal("修改副本影响了判定")
	}
}

func TestGrantCan(t *testing.T) {
	if GrantNone.Can() || !GrantAllow.Can() || !GrantApproval.Can() || !GrantScoped.Can() {
		t.Fatal("Grant.Can 不对")
	}
}

func TestUserRolesForOverride(t *testing.T) {
	u := User{Name: "x", Roles: map[string][]Role{
		"*":    {RoleSRE},
		"test": {RoleDBA},
		"prod": {}, // 显式清空：生产无任何角色
	}}
	if rs := u.RolesFor("staging"); len(rs) != 1 || rs[0] != RoleSRE {
		t.Fatalf("staging 应回落到 *：%v", rs)
	}
	if rs := u.RolesFor("test"); len(rs) != 1 || rs[0] != RoleDBA {
		t.Fatalf("test：%v", rs)
	}
	if rs := u.RolesFor("prod"); len(rs) != 0 {
		t.Fatalf("prod 应被显式覆盖为空：%v", rs)
	}
	if u.Grant("test", CapScale) != GrantApproval || u.Grant("staging", CapScale) != GrantNone {
		t.Fatal("按环境授权不对")
	}
	if u.Grant("prod", CapViewOverview) != GrantNone {
		t.Fatal("prod 无角色不应有任何权限")
	}
	if !u.HasRole("test", RoleDBA) || u.HasRole("test", RoleSRE) || !u.HasRole("staging", RoleSRE) {
		t.Fatal("HasRole 不对")
	}
	// RolesFor 返回副本
	rs := u.RolesFor("test")
	rs[0] = RoleAdmin
	if !u.HasRole("test", RoleDBA) {
		t.Fatal("RolesFor 应返回副本")
	}
}

func TestUserGrantStrongestOfMultipleRoles(t *testing.T) {
	u := User{Roles: map[string][]Role{"*": {RoleDeveloper, RoleSRE}}}
	if g := u.Grant("prod", CapViewSQL); g != GrantAllow { // 范 + ✔ → ✔
		t.Fatalf("got %v", g)
	}
	u = User{Roles: map[string][]Role{"*": {RoleSRE, RoleDBA}}}
	if g := u.Grant("prod", CapInstOp); g != GrantApproval {
		t.Fatalf("got %v", g)
	}
	if g := u.Grant("prod", CapScale); g != GrantApproval {
		t.Fatalf("got %v", g)
	}
	if g := u.Grant("prod", CapUsers); g != GrantNone {
		t.Fatalf("got %v", g)
	}
	// 合并角色不会突破职责分离：dba+approver 同时具备执行与审批能力（靠 ValidateApproval 保证同一变更不能自批）
	u = User{Roles: map[string][]Role{"*": {RoleDBA, RoleApprover}}}
	if !u.Can("prod", CapScale) || !u.Can("prod", CapApproveHigh) {
		t.Fatal("多角色叠加不对")
	}
}

func TestDisabledUserHasNoGrant(t *testing.T) {
	u := User{Disabled: true, Roles: map[string][]Role{"*": {RoleAdmin, RoleApprover}}}
	if u.Grant("prod", CapUsers) != GrantNone || u.HasRole("prod", RoleAdmin) || CanApprove(u, "prod", "high") {
		t.Fatal("被禁用用户不应有权限")
	}
}

func TestCanViewSQLOf(t *testing.T) {
	dev := User{Roles: map[string][]Role{"*": {RoleDeveloper}}, DBs: []string{"shop"}}
	if !dev.CanViewSQLOf("prod", "shop") || !dev.CanViewSQLOf("prod", "SHOP") || dev.CanViewSQLOf("prod", "pay") {
		t.Fatal("developer 应仅限被授权的库")
	}
	if !(User{Roles: map[string][]Role{"*": {RoleSRE}}}).CanViewSQLOf("prod", "anything") {
		t.Fatal("SRE 应可看所有库")
	}
	if (User{Roles: map[string][]Role{"*": {RoleViewer}}}).CanViewSQLOf("prod", "shop") {
		t.Fatal("viewer 不能看 SQL")
	}
}

func TestApprovalPolicy(t *testing.T) {
	cases := []struct {
		risk, env string
		emergency bool
		levels    int
		mfa       bool
	}{
		// 低
		{"low", "test", false, 0, false},
		{"low", "staging", false, 0, false},
		{"low", "prod", false, 0, false},
		// 中
		{"mid", "test", false, 0, false},
		{"mid", "staging", false, 1, false},
		{"mid", "prod", false, 1, false},
		// 高
		{"high", "test", false, 1, false},
		{"high", "staging", false, 2, false},
		{"high", "prod", false, 2, true},
		// 紧急变更（冻结期）：staging / prod 2 级；test 无紧急通道，按常规
		{"mid", "test", true, 0, false},
		{"mid", "staging", true, 2, false},
		{"mid", "prod", true, 2, false},
		{"high", "test", true, 1, false},
		{"high", "staging", true, 2, false},
		{"high", "prod", true, 2, true},
		{"low", "test", true, 0, false},
		{"low", "staging", true, 2, false},
		{"low", "prod", true, 2, false},
		// 未知输入 fail closed
		{"critical", "prod", false, 2, true},
		{"low", "dev", false, 2, true},
		{"", "", false, 2, true},
	}
	for _, c := range cases {
		l, m := ApprovalPolicy(c.risk, c.env, c.emergency)
		if l != c.levels || m != c.mfa {
			t.Errorf("ApprovalPolicy(%q,%q,%v) = (%d,%v)，期望 (%d,%v)", c.risk, c.env, c.emergency, l, m, c.levels, c.mfa)
		}
	}
}

func mk(name string, roles ...Role) User {
	return User{Name: name, Roles: map[string][]Role{"*": roles}}
}

func TestCanApprove(t *testing.T) {
	ap := mk("a", RoleApprover)
	for _, r := range []string{"mid", "high"} {
		if !CanApprove(ap, "prod", r) {
			t.Errorf("approver 应可审批 %s", r)
		}
	}
	if CanApprove(ap, "prod", "low") {
		t.Error("低风险无需审批")
	}
	if CanApprove(ap, "prod", "bogus") {
		t.Error("未知风险不能审批")
	}
	for _, r := range []Role{RoleViewer, RoleDeveloper, RoleSRE, RoleDBA, RoleAdmin, RoleAuditor} {
		if CanApprove(mk("x", r), "prod", "mid") || CanApprove(mk("x", r), "prod", "high") {
			t.Errorf("%s 不应能审批", r)
		}
	}
	// 按环境授予：只在 staging 是审批人
	u := User{Name: "e", Roles: map[string][]Role{"*": {RoleSRE}, "staging": {RoleApprover}}}
	if !CanApprove(u, "staging", "high") || CanApprove(u, "prod", "high") {
		t.Error("审批能力应随环境")
	}
}

func TestValidateApproval(t *testing.T) {
	ap1, ap2 := mk("approver", RoleApprover), mk("approver2", RoleApprover)
	// 合规
	if err := ValidateApproval("dba", nil, ap1, "prod", "high"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateApproval("dba", []string{"approver"}, ap2, "prod", "high"); err != nil {
		t.Fatal(err)
	}
	// 申请人自批（含大小写差异）
	both := mk("dba", RoleDBA, RoleApprover)
	if err := ValidateApproval("dba", nil, both, "prod", "mid"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("应拒绝自批：%v", err)
	}
	if err := ValidateApproval("DBA", nil, both, "prod", "mid"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("应拒绝自批（大小写）：%v", err)
	}
	// 重复审批
	if err := ValidateApproval("dba", []string{"approver"}, ap1, "prod", "high"); !errors.Is(err, ErrDuplicateApproval) {
		t.Fatalf("应拒绝重复审批：%v", err)
	}
	if err := ValidateApproval("dba", []string{"Approver"}, ap1, "prod", "high"); !errors.Is(err, ErrDuplicateApproval) {
		t.Fatalf("应拒绝重复审批（大小写）：%v", err)
	}
	// 无能力
	if err := ValidateApproval("dba", nil, mk("sre1", RoleSRE), "prod", "mid"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("应拒绝无审批能力者：%v", err)
	}
	if err := ValidateApproval("dba", nil, mk("adm", RoleAdmin), "prod", "high"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("管理员不能审批：%v", err)
	}
	// 低风险无需审批，也就不存在审批资格
	if err := ValidateApproval("dba", nil, ap1, "prod", "low"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("低风险：%v", err)
	}
	// 已禁用
	dis := ap2
	dis.Disabled = true
	if err := ValidateApproval("dba", nil, dis, "prod", "mid"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("已禁用：%v", err)
	}
	// 环境内无审批角色
	scoped := User{Name: "s", Roles: map[string][]Role{"test": {RoleApprover}}}
	if err := ValidateApproval("dba", nil, scoped, "prod", "high"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("环境不匹配：%v", err)
	}
}
