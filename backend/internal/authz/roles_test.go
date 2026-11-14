// @author: kevinkelly
// @license: Apache License 2.0
// @file: roles_test.go
// @time: 2026/10/28

package authz

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var adminUser = User{Name: "root", Roles: map[string][]Role{"*": {RoleAdmin}}}

func openRS(t *testing.T) (*RoleStore, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "roles.json")
	rs, err := OpenRoleStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ClearCustomRoles)
	return rs, p
}

func caps(kv ...any) map[Cap]Grant {
	m := map[Cap]Grant{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(Cap)] = kv[i+1].(Grant)
	}
	return m
}

func mustCreate(t *testing.T, rs *RoleStore, actor User, id string, c map[Cap]Grant) CustomRole {
	t.Helper()
	r, err := rs.Create(actor, "prod", RoleInput{ID: id, Name: "角色-" + id, Caps: c}, t0)
	if err != nil {
		t.Fatalf("创建 %s: %v", id, err)
	}
	return r
}

var viewOnly = func() map[Cap]Grant { return caps(CapViewOverview, GrantAllow) }

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var re *RoleError
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("期望错误码 %s，得到 %v", code, err)
	}
}

// a. 内置角色永远不可修改 / 删除。
func TestBuiltinRolesImmutable(t *testing.T) {
	rs, _ := openRS(t)
	before := Matrix()
	n := "x"
	for _, r := range AllRoles() {
		_, _, err := rs.Update(adminUser, "prod", string(r), RoleUpdate{Name: &n}, t0)
		wantCode(t, err, RoleErrBuiltin)
		if !errors.Is(err, ErrRoleBuiltin) {
			t.Fatal("errors.Is 应匹配 ErrRoleBuiltin")
		}
		_, err = rs.Delete(string(r), nil)
		wantCode(t, err, RoleErrBuiltin)
		_, err = rs.Create(adminUser, "prod", RoleInput{ID: string(r), Name: "新名字", Caps: viewOnly()}, t0)
		if err == nil {
			t.Fatalf("不应允许用内置 id %s 创建自定义角色", r)
		}
	}
	after := Matrix()
	for r, row := range before {
		for c, g := range row {
			if after[r][c] != g {
				t.Fatalf("内置矩阵被改动：%s/%s", r, c)
			}
		}
	}
	if len(rs.List()) != 0 {
		t.Fatal("不应产生任何自定义角色")
	}
}

// b. 仅管理员能力：创建 / 更新都拒绝（对 admin 也一样）；不含这些能力的角色正常。
func TestLockedCapsRejected(t *testing.T) {
	rs, _ := openRS(t)
	for _, c := range []Cap{CapUsers, CapWriteSwitch, CapSettings} {
		if !IsLockedCap(c) || len(AllowedGrants(c)) != 0 {
			t.Fatalf("%s 应为 locked", c)
		}
		for _, g := range []Grant{GrantAllow, GrantApproval, GrantScoped} {
			m := viewOnly()
			m[c] = g
			_, err := rs.Create(adminUser, "prod", RoleInput{ID: "evil", Name: "evil", Caps: m}, t0)
			wantCode(t, err, RoleErrLockedCap)
			if !errors.Is(err, ErrRoleLockedCap) {
				t.Fatal("errors.Is 不匹配")
			}
		}
	}
	if len(rs.List()) != 0 {
		t.Fatal("被拒绝的创建不应落库")
	}
	mustCreate(t, rs, adminUser, "ok", caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow))
	m := caps(CapViewOverview, GrantAllow, CapUsers, GrantAllow)
	_, _, err := rs.Update(adminUser, "prod", "ok", RoleUpdate{Caps: m}, t0)
	wantCode(t, err, RoleErrLockedCap)
	// 值为 none 的 locked 项视为未授予，不报错
	if _, err := rs.Create(adminUser, "prod", RoleInput{ID: "fine", Name: "fine", Caps: caps(CapViewOverview, GrantAllow, CapUsers, GrantNone)}, t0); err != nil {
		t.Fatal(err)
	}
	// 提权不可达：自定义角色成员没有 users.manage
	u := User{Name: "u", Roles: map[string][]Role{"*": {"ok"}}}
	for _, c := range []Cap{CapUsers, CapWriteSwitch, CapSettings} {
		if u.Can("prod", c) {
			t.Fatalf("自定义角色成员不应拥有 %s", c)
		}
	}
}

// c. 每个能力允许的授予方式（用独立的期望表对照）。
func TestGrantCombinations(t *testing.T) {
	ok := map[Cap][]Grant{
		CapInstOp: {GrantAllow, GrantApproval}, CapScale: {GrantAllow, GrantApproval}, CapConfig: {GrantAllow, GrantApproval},
		CapMaint: {GrantAllow, GrantApproval}, CapBackup: {GrantAllow, GrantApproval}, CapRelease: {GrantAllow, GrantApproval},
		CapViewSQL: {GrantAllow, GrantScoped},
	}
	locked := map[Cap]bool{CapUsers: true, CapWriteSwitch: true, CapSettings: true}
	for _, c := range AllCaps() {
		for _, g := range []Grant{GrantAllow, GrantApproval, GrantScoped} {
			m := viewOnly()
			m[c] = g
			err := ValidateCaps(m)
			want := false
			switch {
			case locked[c]:
			case c == CapViewOverview:
				want = g == GrantAllow
			case ok[c] != nil:
				for _, x := range ok[c] {
					want = want || x == g
				}
			default:
				want = g == GrantAllow
			}
			if want && err != nil {
				t.Errorf("%s=%s 应合法：%v", c, g, err)
			}
			if !want && err == nil {
				t.Errorf("%s=%s 应被拒绝", c, g)
			}
		}
	}
	// 未知能力 / 未知授予方式
	if _, err := ParseCapMap(map[string]string{"nope": "allow"}); err == nil {
		t.Error("未知能力应被拒绝")
	}
	if _, err := ParseCapMap(map[string]string{"diag.run": "sudo"}); err == nil {
		t.Error("未知授予方式应被拒绝")
	}
	if err := ValidateCaps(map[Cap]Grant{CapViewOverview: GrantAllow, "nope": GrantAllow}); err == nil {
		t.Error("未知能力应被拒绝")
	}
}

// d. 必须至少含 view.overview。
func TestMustHaveOverview(t *testing.T) {
	rs, _ := openRS(t)
	for _, m := range []map[Cap]Grant{nil, {}, caps(CapDiag, GrantAllow), caps(CapViewOverview, GrantNone, CapDiag, GrantAllow)} {
		_, err := rs.Create(adminUser, "prod", RoleInput{ID: "noov", Name: "noov", Caps: m}, t0)
		wantCode(t, err, RoleErrInvalid)
		if err != nil && !strings.Contains(err.Error(), "view.overview") {
			t.Fatalf("文案应提到 view.overview：%v", err)
		}
	}
	mustCreate(t, rs, adminUser, "r1", viewOnly())
	_, _, err := rs.Update(adminUser, "prod", "r1", RoleUpdate{Caps: caps(CapDiag, GrantAllow)}, t0)
	wantCode(t, err, RoleErrInvalid)
	_, _, err = rs.Update(adminUser, "prod", "r1", RoleUpdate{Caps: map[Cap]Grant{}}, t0)
	wantCode(t, err, RoleErrInvalid)
}

// e. 防提权。
func TestAntiEscalation(t *testing.T) {
	rs, _ := openRS(t)
	// “角色管理员”场景在 authz 层构造：持有 ops 角色（view.overview / diag / inst.op 需审批）的人去授予别人。
	mustCreate(t, rs, adminUser, "ops", caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow, CapInstOp, GrantApproval, CapViewSQL, GrantScoped))
	actor := User{Name: "opsman", Roles: map[string][]Role{"*": {"ops"}}}
	try := func(env string, m map[Cap]Grant) error {
		_, err := rs.Create(actor, env, RoleInput{ID: fmt.Sprintf("t%d", len(rs.List())), Name: fmt.Sprintf("n%d", len(rs.List())), Caps: m}, t0)
		return err
	}
	// 反例：授予自己没有的能力 / 更强的授予方式
	for name, m := range map[string]map[Cap]Grant{
		"没有的能力":          caps(CapViewOverview, GrantAllow, CapScale, GrantApproval),
		"授予方式更强":         caps(CapViewOverview, GrantAllow, CapInstOp, GrantAllow),
		"范围升级为完整":        caps(CapViewOverview, GrantAllow, CapViewSQL, GrantAllow),
		"没有的审计能力":        caps(CapViewOverview, GrantAllow, CapAuditView, GrantAllow),
		"没有的审批能力":        caps(CapViewOverview, GrantAllow, CapApproveHigh, GrantAllow),
		"没有的排期能力（即使低风险）": caps(CapViewOverview, GrantAllow, CapSchedCreate, GrantAllow),
	} {
		wantCode(t, try("prod", m), RoleErrEscalation)
		_ = name
	}
	// 正例：不超过自己
	if err := try("prod", caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow, CapInstOp, GrantApproval, CapViewSQL, GrantScoped)); err != nil {
		t.Fatalf("授予自己拥有的能力应成功：%v", err)
	}
	if err := try("prod", caps(CapViewOverview, GrantAllow)); err != nil {
		t.Fatal(err)
	}
	// 按环境：只在 test 环境有 dba 的人，在 prod 下不能授予 backup
	envActor := User{Name: "e", Roles: map[string][]Role{"test": {RoleDBA}, "prod": {RoleViewer}}}
	if _, err := rs.Create(envActor, "test", RoleInput{ID: "envok", Name: "envok", Caps: caps(CapViewOverview, GrantAllow, CapBackup, GrantApproval)}, t0); err != nil {
		t.Fatalf("test 环境下 dba 可授予 backup(approval)：%v", err)
	}
	_, err := rs.Create(envActor, "prod", RoleInput{ID: "envbad", Name: "envbad", Caps: caps(CapViewOverview, GrantAllow, CapBackup, GrantApproval)}, t0)
	wantCode(t, err, RoleErrEscalation)
	// 被禁用的人没有任何能力
	dis := User{Name: "d", Disabled: true, Roles: map[string][]Role{"*": {RoleDBA}}}
	_, err = rs.Create(dis, "prod", RoleInput{ID: "disbad", Name: "disbad", Caps: caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow)}, t0)
	wantCode(t, err, RoleErrEscalation)
	// admin 不受限：admin 自己没有 inst.op，也可授予；但 locked 对 admin 同样生效
	if err := func() error {
		_, e := rs.Create(adminUser, "prod", RoleInput{ID: "bigops", Name: "bigops", Caps: caps(CapViewOverview, GrantAllow, CapInstOp, GrantAllow, CapBackup, GrantAllow)}, t0)
		return e
	}(); err != nil {
		t.Fatalf("admin 应不受防提权限制：%v", err)
	}
	_, err = rs.Create(adminUser, "prod", RoleInput{ID: "bigbad", Name: "bigbad", Caps: caps(CapViewOverview, GrantAllow, CapSettings, GrantAllow)}, t0)
	wantCode(t, err, RoleErrLockedCap)
}

// 更新里保持不变的能力不触发防提权；降级放行；提升拒绝。
func TestAntiEscalationOnUpdateUnchangedAllowed(t *testing.T) {
	rs, _ := openRS(t)
	mustCreate(t, rs, adminUser, "ops", caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow, CapInstOp, GrantApproval))
	mustCreate(t, rs, adminUser, "big", caps(CapViewOverview, GrantAllow, CapInstOp, GrantAllow, CapBackup, GrantAllow))
	actor := User{Name: "opsman", Roles: map[string][]Role{"*": {"ops"}}}
	// 降级 inst.op、保持 backup 不变（actor 没有 backup，但未提升）、去掉 view.overview 之外的……
	if _, _, err := rs.Update(actor, "prod", "big", RoleUpdate{Caps: caps(CapViewOverview, GrantAllow, CapInstOp, GrantApproval, CapBackup, GrantAllow)}, t0); err != nil {
		t.Fatalf("保持不变 + 降级应放行：%v", err)
	}
	// 提升 inst.op 回 allow：拒绝
	_, _, err := rs.Update(actor, "prod", "big", RoleUpdate{Caps: caps(CapViewOverview, GrantAllow, CapInstOp, GrantAllow, CapBackup, GrantAllow)}, t0)
	wantCode(t, err, RoleErrEscalation)
	// 新增 scale：拒绝
	_, _, err = rs.Update(actor, "prod", "big", RoleUpdate{Caps: caps(CapViewOverview, GrantAllow, CapInstOp, GrantApproval, CapBackup, GrantAllow, CapScale, GrantApproval)}, t0)
	wantCode(t, err, RoleErrEscalation)
	// 只改名称不触发
	n := "只改名"
	if _, _, err := rs.Update(actor, "prod", "big", RoleUpdate{Name: &n}, t0); err != nil {
		t.Fatal(err)
	}
}

// f. 删除时仍有成员持有 → 拒绝并返回成员名单。
func TestDeleteRejectedWhileInUse(t *testing.T) {
	rs, _ := openRS(t)
	us, _ := newStore(t, nil)
	mustCreate(t, rs, adminUser, "analyst", viewOnly())
	if err := us.Add("amy", "Amy", pw, map[string][]Role{"prod": {"analyst"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := us.Add("bob", "Bob", pw, map[string][]Role{"*": {RoleViewer}, "test": {"analyst", RoleViewer}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := us.Add("cat", "Cat", pw, map[string][]Role{"*": {RoleViewer}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := us.SetDisabled("bob", true); err != nil { // 被禁用的成员同样算
		t.Fatal(err)
	}
	_, err := rs.Delete("analyst", us)
	wantCode(t, err, RoleErrInUse)
	var re *RoleError
	errors.As(err, &re)
	if strings.Join(re.Members, ",") != "amy,bob" {
		t.Fatalf("成员名单不对：%v", re.Members)
	}
	if _, ok := rs.Get("analyst"); !ok || !Role("analyst").Valid() {
		t.Fatal("被拒绝的删除不应影响角色")
	}
	if err := us.SetRoles("amy", map[string][]Role{"*": {RoleViewer}}); err != nil {
		t.Fatal(err)
	}
	if err := us.SetRoles("bob", map[string][]Role{"*": {RoleViewer}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Delete("analyst", us); err != nil {
		t.Fatal(err)
	}
	if Role("analyst").Valid() {
		t.Fatal("删除后注册表应不再识别该角色")
	}
	_, err = rs.Delete("analyst", us)
	wantCode(t, err, RoleErrNotFound)
	// 用户库不再接受已删除的角色
	if err := us.SetRoles("cat", map[string][]Role{"*": {"analyst"}}); err == nil {
		t.Fatal("已删除的角色不应能再分配")
	}
}

// g. id / 名称唯一；名称不区分大小写，且不得与内置显示名相同；数量上限；格式。
func TestUniquenessAndFormat(t *testing.T) {
	rs, _ := openRS(t)
	mustCreate(t, rs, adminUser, "dup", viewOnly())
	_, err := rs.Create(adminUser, "prod", RoleInput{ID: "dup", Name: "另一个名字", Caps: viewOnly()}, t0)
	wantCode(t, err, RoleErrExists)
	_, err = rs.Create(adminUser, "prod", RoleInput{ID: "dup2", Name: "角色-DUP", Caps: viewOnly()}, t0) // 与「角色-dup」仅大小写不同
	wantCode(t, err, RoleErrExists)
	mustCreate(t, rs, adminUser, "other", viewOnly())
	nm := " 角色-DUP "
	_, _, err = rs.Update(adminUser, "prod", "other", RoleUpdate{Name: &nm}, t0)
	wantCode(t, err, RoleErrExists)
	same := "角色-dup" // 改成自己当前的名字不算冲突
	if _, _, err := rs.Update(adminUser, "prod", "dup", RoleUpdate{Name: &same}, t0); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"DBA", "dba", "访客", "开发", "值班 SRE", "值班 sre", "审批人", "管理员", "审计员", "viewer", "Admin"} {
		_, err := rs.Create(adminUser, "prod", RoleInput{ID: "rsv", Name: n, Caps: viewOnly()}, t0)
		wantCode(t, err, RoleErrExists)
	}
	for _, id := range []string{"", "a", "A1", "1ab", "ab cd", "ab.cd", "_ab", strings.Repeat("a", 33), "角色"} {
		_, err := rs.Create(adminUser, "prod", RoleInput{ID: id, Name: "x" + id, Caps: viewOnly()}, t0)
		wantCode(t, err, RoleErrInvalid)
	}
	for _, id := range []string{"ab", "a-b_c9", strings.Repeat("a", 32)} {
		if _, err := rs.Create(adminUser, "prod", RoleInput{ID: id, Name: "名-" + id, Caps: viewOnly()}, t0); err != nil {
			t.Errorf("id %q 应合法：%v", id, err)
		}
	}
	// 内置 id 不能被占用
	for _, r := range AllRoles() {
		if err := ValidRoleID(string(r)); err == nil {
			t.Errorf("内置 id %s 应不可用", r)
		}
	}
}

func TestNameDescLimits(t *testing.T) {
	rs, _ := openRS(t)
	ok40 := strings.Repeat("字", 40)
	if _, err := rs.Create(adminUser, "prod", RoleInput{ID: "n40", Name: ok40, Desc: strings.Repeat("说", 200), Caps: viewOnly()}, t0); err != nil {
		t.Fatalf("40 字名称 / 200 字说明应合法：%v", err)
	}
	for name, in := range map[string]RoleInput{
		"空名":      {ID: "e1", Name: "   ", Caps: viewOnly()},
		"41 字名称":  {ID: "e2", Name: ok40 + "x", Caps: viewOnly()},
		"201 字说明": {ID: "e3", Name: "e3", Desc: strings.Repeat("说", 201), Caps: viewOnly()},
		"控制字符":    {ID: "e4", Name: "a\x00b", Caps: viewOnly()},
	} {
		if _, err := rs.Create(adminUser, "prod", in, t0); err == nil {
			t.Errorf("%s 应被拒绝", name)
		} else {
			wantCode(t, err, RoleErrInvalid)
		}
	}
	for i := 0; i < MaxCustomRoles; i++ { // 已有 1 个，再建到上限
		if len(rs.List()) >= MaxCustomRoles {
			break
		}
		mustCreate(t, rs, adminUser, fmt.Sprintf("bulk%02d", i), viewOnly())
	}
	_, err := rs.Create(adminUser, "prod", RoleInput{ID: "over", Name: "over", Caps: viewOnly()}, t0)
	wantCode(t, err, RoleErrLimit)
	if len(rs.List()) != MaxCustomRoles {
		t.Fatalf("应恰好 %d 个，实际 %d", MaxCustomRoles, len(rs.List()))
	}
}

func TestRolePersistence(t *testing.T) {
	rs, p := openRS(t)
	r := mustCreate(t, rs, adminUser, "keep", caps(CapViewOverview, GrantAllow, CapViewSQL, GrantScoped, CapInstOp, GrantApproval))
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("roles.json 权限应为 0600：%v %v", st, err)
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
	raw, _ := os.ReadFile(p)
	var f struct {
		Version int
		Roles   []struct {
			ID   string
			Caps map[string]string
		}
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Roles) != 1 || f.Roles[0].Caps["view.sql"] != "scoped" || f.Roles[0].Caps["inst.op"] != "approval" {
		t.Fatalf("落盘格式不对：%s", raw)
	}
	ClearCustomRoles()
	rs2, err := OpenRoleStore(p)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := rs2.Get("keep")
	if !ok || g.Name != r.Name || g.Caps[CapViewSQL] != GrantScoped || g.UpdatedBy != "root" || !g.Created.Equal(r.Created) {
		t.Fatalf("重新打开后内容不一致：%+v", g)
	}
	if !Role("keep").Valid() || RoleGrant("keep", CapInstOp) != GrantApproval {
		t.Fatal("重新打开后注册表应生效")
	}
	if len(rs2.Warnings()) != 0 {
		t.Fatalf("正常文件不应有告警：%v", rs2.Warnings())
	}
	// 写盘失败时内存不变：把目录替换成文件使 CreateTemp 失败
	bad := &RoleStore{path: filepath.Join(p, "sub", "roles.json"), roles: map[string]*CustomRole{}}
	if _, err := bad.Create(adminUser, "prod", RoleInput{ID: "zz", Name: "zz", Caps: viewOnly()}, t0); err == nil {
		t.Fatal("写盘失败应返回错误")
	}
	if len(bad.List()) != 0 {
		t.Fatal("写盘失败不应改变内存")
	}
	// 损坏的 JSON 应返回错误（不静默清空）
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := OpenRoleStore(p); err == nil {
		t.Fatal("无法解析的 roles.json 应返回错误")
	}
}

// 存储被手改后的加载降级：丢弃高危能力、非法组合、非法角色，不得提权。
func TestLoadDowngradesHandEditedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "roles.json")
	t.Cleanup(ClearCustomRoles)
	body := `{"version":1,"roles":[
	 {"id":"evil","name":"evil","desc":"","caps":{"view.overview":"allow","users.manage":"allow","writeswitch":"approval","settings.edit":"allow","inst.op":"scoped","diag.run":"allow","bogus.cap":"allow","report.export":"superuser","scale":"approval"}},
	 {"id":"admin","name":"我是内置 id","caps":{"view.overview":"allow","users.manage":"allow"}},
	 {"id":"Bad ID","name":"x","caps":{"view.overview":"allow"}},
	 {"id":"noov","name":"noov","caps":{"diag.run":"allow"}},
	 {"id":"rsv","name":"DBA","caps":{"view.overview":"allow"}},
	 {"id":"evil","name":"重复","caps":{"view.overview":"allow","users.manage":"allow"}}
	]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := OpenRoleStore(p)
	if err != nil {
		t.Fatal(err)
	}
	u := User{Name: "u", Roles: map[string][]Role{"*": {"evil"}}}
	for _, c := range []Cap{CapUsers, CapWriteSwitch, CapSettings} {
		if u.Can("prod", c) {
			t.Fatalf("手改的存储不得提权：%s", c)
		}
	}
	if u.Grant("prod", CapInstOp) != GrantNone {
		t.Fatal("非法授予方式 inst.op=scoped 应被丢弃")
	}
	if u.Grant("prod", CapReport) != GrantNone {
		t.Fatal("未知授予方式应被丢弃")
	}
	if u.Grant("prod", CapDiag) != GrantAllow || u.Grant("prod", CapScale) != GrantApproval || u.Grant("prod", CapViewOverview) != GrantAllow {
		t.Fatal("合法项应保留")
	}
	if Role("Bad ID").Valid() || RoleGrant(RoleAdmin, CapUsers) != GrantAllow {
		t.Fatal("非法 id / 内置 id 的条目应被丢弃，且不影响内置矩阵")
	}
	if !Role("noov").Valid() || !Role("rsv").Valid() {
		t.Fatal("缺 view.overview / 名称冲突的角色应保留（只是告警 / 改名）")
	}
	if g, _ := rs.Get("rsv"); g.Name != "rsv" {
		t.Fatalf("与内置同名应改用 id：%q", g.Name)
	}
	ws := strings.Join(rs.Warnings(), "\n")
	for _, want := range []string{"users.manage", "writeswitch", "settings.edit", "inst.op", "bogus.cap", "report.export", "Bad ID", "admin", "noov", "evil"} {
		if !strings.Contains(ws, want) {
			t.Errorf("告警里应提到 %q：\n%s", want, ws)
		}
	}
	// 内置角色的 admin 仍是 admin
	if (User{Roles: map[string][]Role{"*": {RoleAdmin}}}).Grant("prod", CapUsers) != GrantAllow {
		t.Fatal("内置 admin 不受影响")
	}
}

// 多角色取最强，含自定义角色；被删除的角色按无权限；被禁用的用户恒无权限。
func TestMultiRoleStrongestWithCustom(t *testing.T) {
	rs, _ := openRS(t)
	mustCreate(t, rs, adminUser, "sqlscope", caps(CapViewOverview, GrantAllow, CapViewSQL, GrantScoped, CapInstOp, GrantApproval))
	mustCreate(t, rs, adminUser, "sqlfull", caps(CapViewOverview, GrantAllow, CapViewSQL, GrantAllow))
	mustCreate(t, rs, adminUser, "opsfull", caps(CapViewOverview, GrantAllow, CapInstOp, GrantAllow))
	env := "prod"
	u := User{Name: "m", Roles: map[string][]Role{"*": {"sqlscope"}}, DBs: []string{"Shop"}}
	if u.Grant(env, CapViewSQL) != GrantScoped || !u.CanViewSQLOf(env, "shop") || u.CanViewSQLOf(env, "other") {
		t.Fatal("自定义 scoped 应只限 DBs")
	}
	u.Roles = map[string][]Role{"*": {"sqlscope", "sqlfull"}}
	if u.Grant(env, CapViewSQL) != GrantAllow || !u.CanViewSQLOf(env, "other") {
		t.Fatal("scoped + allow 取 allow")
	}
	u.Roles = map[string][]Role{"*": {"sqlscope", "opsfull"}}
	if u.Grant(env, CapInstOp) != GrantAllow {
		t.Fatal("approval + allow 取 allow")
	}
	u.Roles = map[string][]Role{"*": {"sqlscope", RoleDBA}} // 自定义 approval + 内置 dba approval
	if u.Grant(env, CapInstOp) != GrantApproval || u.Grant(env, CapScale) != GrantApproval {
		t.Fatal("自定义与内置合并")
	}
	u.Roles = map[string][]Role{"*": {RoleViewer, "sqlfull"}} // 内置 viewer 无 view.sql
	if u.Grant(env, CapViewSQL) != GrantAllow {
		t.Fatal("内置 + 自定义合并")
	}
	u.Roles = map[string][]Role{"*": {RoleSRE, "sqlscope"}} // 内置 allow 胜过自定义 scoped
	if u.Grant(env, CapViewSQL) != GrantAllow {
		t.Fatal("内置 allow 应胜过自定义 scoped")
	}
	// 按环境覆盖："prod" 键存在则覆盖 "*"
	u.Roles = map[string][]Role{"*": {"opsfull"}, "prod": {"sqlscope"}}
	if u.Grant("prod", CapInstOp) != GrantApproval || u.Grant("test", CapInstOp) != GrantAllow {
		t.Fatal("环境覆盖规则对自定义角色同样成立")
	}
	// 审批能力
	mustCreate(t, rs, adminUser, "apv", caps(CapViewOverview, GrantAllow, CapApproveMid, GrantAllow))
	a := User{Name: "a", Roles: map[string][]Role{"*": {"apv"}}}
	if !CanApprove(a, env, RiskMid) || CanApprove(a, env, RiskHigh) {
		t.Fatal("自定义 approve.mid")
	}
	if err := ValidateApproval("someone", nil, a, env, RiskMid); err != nil {
		t.Fatal(err)
	}
	if err := ValidateApproval("a", nil, a, env, RiskMid); !errors.Is(err, ErrSelfApproval) {
		t.Fatal("自定义角色同样不能自批")
	}
	// 禁用
	a.Disabled = true
	if a.Can(env, CapViewOverview) || CanApprove(a, env, RiskMid) {
		t.Fatal("被禁用恒无权限")
	}
	// 删除后按无权限
	if _, err := rs.Delete("apv", nil); err != nil {
		t.Fatal(err)
	}
	a.Disabled = false
	if a.Can(env, CapViewOverview) || CanApprove(a, env, RiskMid) || Role("apv").Valid() {
		t.Fatal("已删除的自定义角色应按无权限处理")
	}
	if !a.HasRole(env, "apv") { // 身份记录仍在，但没有任何权限
		t.Log("HasRole 仅反映持有关系")
	}
}

func TestRoleInfoAndKnownRoles(t *testing.T) {
	rs, _ := openRS(t)
	mustCreate(t, rs, adminUser, "zeta", viewOnly())
	mustCreate(t, rs, adminUser, "alpha", viewOnly())
	k := KnownRoles()
	if len(k) != 9 || k[0] != RoleViewer || k[6] != RoleAuditor || k[7] != "alpha" || k[8] != "zeta" {
		t.Fatalf("KnownRoles 顺序不对：%v", k)
	}
	if len(AllRoles()) != 7 {
		t.Fatal("AllRoles 固定为内置 7 个")
	}
	if n, b, ok := RoleDisplayName(RoleDBA); n != "DBA" || !b || !ok {
		t.Fatal("内置名称")
	}
	if n, b, ok := RoleDisplayName("alpha"); n != "角色-alpha" || b || !ok {
		t.Fatal("自定义名称")
	}
	if _, _, ok := RoleDisplayName("ghost"); ok {
		t.Fatal("未知角色")
	}
	for _, r := range AllRoles() {
		if n, d, ok := BuiltinRoleInfo(r); !ok || n == "" || d == "" {
			t.Fatalf("内置角色 %s 缺少文案", r)
		}
	}
}

func TestCatalogIsComplete(t *testing.T) {
	seen := map[Cap]bool{}
	locked := 0
	groups := []string{}
	for _, g := range Catalog() {
		groups = append(groups, g.Group)
		for _, c := range g.Caps {
			if seen[c.Cap] {
				t.Fatalf("重复能力 %s", c.Cap)
			}
			seen[c.Cap] = true
			if c.Name == "" || c.Desc == "" {
				t.Errorf("%s 缺文案", c.Cap)
			}
			if c.Risk != "none" && c.Risk != "mid" && c.Risk != "high" {
				t.Errorf("%s 风险值 %q", c.Cap, c.Risk)
			}
			if len(c.AllowedGrants) == 0 || c.AllowedGrants[0] != "none" {
				t.Errorf("%s allowedGrants 应以 none 开头：%v", c.Cap, c.AllowedGrants)
			}
			if c.Locked {
				locked++
				if len(c.AllowedGrants) != 1 {
					t.Errorf("locked 能力只能是 none：%s %v", c.Cap, c.AllowedGrants)
				}
			}
			if len(c.AllowedGrants)-1 != len(AllowedGrants(c.Cap)) {
				t.Errorf("%s 目录与 AllowedGrants 不一致", c.Cap)
			}
		}
	}
	for _, c := range AllCaps() {
		if !seen[c] {
			t.Errorf("目录缺少 %s", c)
		}
	}
	if len(seen) != len(AllCaps()) || locked != 3 {
		t.Fatalf("目录项 %d / locked %d", len(seen), locked)
	}
	if strings.Join(groups, ",") != "查看与诊断,运维变更,排期与审批,平台管理,审计" {
		t.Fatalf("分组：%v", groups)
	}
	for _, c := range []Cap{CapInstOp, CapScale, CapConfig, CapMaint, CapBackup, CapRelease, CapApproveHigh, CapWriteSwitch, CapUsers} {
		m, _ := findCapMeta(c)
		if m.risk != CapRiskHigh {
			t.Errorf("%s 应为 high", c)
		}
	}
}

func TestDiffCaps(t *testing.T) {
	d := DiffCaps(caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow, CapInstOp, GrantApproval),
		caps(CapViewOverview, GrantAllow, CapInstOp, GrantAllow, CapBackup, GrantApproval))
	if strings.Join(d, " ") != "−diag.run ~inst.op(approval→allow) +backup(approval)" {
		t.Fatalf("diff = %v", d)
	}
	if len(DiffCaps(viewOnly(), viewOnly())) != 0 {
		t.Fatal("相同应无差异")
	}
}

func TestCopyBaseDropsLocked(t *testing.T) {
	rs, _ := openRS(t)
	b, err := CopyBase(RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Cap{CapUsers, CapWriteSwitch, CapSettings} {
		if _, ok := b[c]; ok {
			t.Fatalf("复制底不应含 %s", c)
		}
	}
	if b[CapRelease] != GrantApproval || b[CapDiag] != GrantAllow || b[CapAuditView] != GrantAllow {
		t.Fatal("其余能力应保留")
	}
	if _, err := CopyBase("ghost"); err == nil {
		t.Fatal("未知角色应报错")
	}
	mustCreate(t, rs, adminUser, "src", caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow))
	c, err := CopyBase("src")
	if err != nil || c[CapDiag] != GrantAllow {
		t.Fatal("复制自定义角色")
	}
	c[CapDiag] = GrantNone // 副本修改不影响注册表
	if RoleGrant("src", CapDiag) != GrantAllow {
		t.Fatal("RoleCaps 应返回副本")
	}
}

func TestUpdateNoChangeDoesNotWrite(t *testing.T) {
	rs, p := openRS(t)
	mustCreate(t, rs, adminUser, "same", viewOnly())
	st1, _ := os.Stat(p)
	time.Sleep(20 * time.Millisecond)
	b, a, err := rs.Update(adminUser, "prod", "same", RoleUpdate{Caps: viewOnly()}, t0.Add(time.Hour))
	if err != nil || !b.Updated.Equal(a.Updated) {
		t.Fatalf("无变化应原样返回：%v", err)
	}
	st2, _ := os.Stat(p)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("无变化不应写盘")
	}
	d := "新说明"
	b, a, err = rs.Update(adminUser, "prod", "same", RoleUpdate{Desc: &d}, t0.Add(time.Hour))
	if err != nil || a.Desc != d || !a.Updated.Equal(t0.Add(time.Hour)) || a.UpdatedBy != "root" || b.Desc != "" {
		t.Fatalf("更新：%v %+v", err, a)
	}
}

// 并发：读路径（User.Grant / Valid）与写路径（创建 / 更新 / 删除）同时进行，不 panic、不死锁、终态一致。
func TestConcurrentRegistry(t *testing.T) {
	rs, _ := openRS(t)
	us, _ := newStore(t, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u := User{Name: "r", Roles: map[string][]Role{"*": {"hot", RoleViewer}}}
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = u.Grant("prod", CapDiag)
				_ = Role("hot").Valid()
				_ = u.CanViewSQLOf("prod", "x")
				_ = KnownRoles()
				_, _ = RoleCaps("hot")
				_ = rs.List()
			}
		}(i)
	}
	var ww sync.WaitGroup
	for w := 0; w < 4; w++ {
		ww.Add(1)
		go func(w int) {
			defer ww.Done()
			for i := 0; i < 15; i++ {
				id := fmt.Sprintf("w%d-%d", w, i)
				if _, err := rs.Create(adminUser, "prod", RoleInput{ID: id, Name: id, Caps: caps(CapViewOverview, GrantAllow, CapDiag, GrantAllow)}, t0); err != nil {
					var re *RoleError
					if errors.As(err, &re) && re.Code == RoleErrLimit {
						continue
					}
					t.Errorf("创建 %s：%v", id, err)
					return
				}
				d := "x"
				if _, _, err := rs.Update(adminUser, "prod", id, RoleUpdate{Desc: &d, Caps: caps(CapViewOverview, GrantAllow)}, t0); err != nil {
					t.Errorf("更新 %s：%v", id, err)
				}
				if i%2 == 0 {
					if _, err := rs.Delete(id, us); err != nil {
						t.Errorf("删除 %s：%v", id, err)
					}
				}
			}
		}(w)
	}
	ww.Wait()
	close(stop)
	wg.Wait()
	list := rs.List()
	for _, r := range list {
		if !Role(r.ID).Valid() || RoleGrant(Role(r.ID), CapDiag) != GrantNone {
			t.Fatalf("终态与注册表不一致：%s", r.ID)
		}
	}
	if len(list) > MaxCustomRoles {
		t.Fatalf("超过上限：%d", len(list))
	}
}
