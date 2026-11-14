// @author: kevinkelly
// @license: Apache License 2.0
// @file: roles_api_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tidbops/internal/authz"
)

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asList(v any) []any         { l, _ := v.([]any); return l }

func roleByID(t *testing.T, c *client, id string) map[string]any {
	t.Helper()
	code, out := c.do("GET", "/api/roles", nil)
	if code != 200 {
		t.Fatalf("GET /api/roles: %d %v", code, out)
	}
	for _, r := range asList(out["roles"]) {
		if m := asMap(r); m["id"] == id {
			return m
		}
	}
	return nil
}

func mkRole(t *testing.T, c *client, id string, caps map[string]string) map[string]any {
	t.Helper()
	code, out := c.do("POST", "/api/roles", map[string]any{"id": id, "name": "角色" + id, "desc": "测试", "caps": caps})
	if code != 200 {
		t.Fatalf("创建角色 %s: %d %v", id, code, out)
	}
	return asMap(out["role"])
}

func mkUser(t *testing.T, admin *client, name string, roles ...string) {
	t.Helper()
	code, out := admin.do("POST", "/api/users", map[string]any{"name": name, "display": name, "password": authz.DemoPassword, "roles": map[string][]string{"*": roles}})
	if code != 200 {
		t.Fatalf("创建用户 %s: %d %v", name, code, out)
	}
}

func TestRolesListContract(t *testing.T) {
	e := setupOps(t)
	admin := e.login(t, "admin")
	code, out := admin.do("GET", "/api/roles", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	roles := asList(out["roles"])
	if len(roles) != 7 {
		t.Fatalf("无自定义角色时应只有 7 个内置角色：%d", len(roles))
	}
	wantOrder := []string{"viewer", "developer", "sre", "dba", "approver", "admin", "auditor"}
	for i, r := range roles {
		m := asMap(r)
		if m["id"] != wantOrder[i] || m["builtin"] != true || m["name"] == "" || m["desc"] == "" {
			t.Fatalf("内置角色 %d：%v", i, m)
		}
		if m["created"] != nil || m["updatedBy"] != nil {
			t.Fatalf("内置角色不应有 created / updatedBy：%v", m)
		}
	}
	dba := asMap(roles[3])
	if dba["name"] != "DBA" || asMap(dba["caps"])["inst.op"] != "approval" || asMap(dba["caps"])["view.overview"] != "allow" {
		t.Fatalf("dba caps：%v", dba)
	}
	if _, has := asMap(dba["caps"])["users.manage"]; has {
		t.Fatal("caps 不应包含 none 项")
	}
	adm := asMap(roles[5])
	if asMap(adm["caps"])["users.manage"] != "allow" || adm["members"] != float64(2) {
		t.Fatalf("admin：%v", adm) // 演示数据里有 admin / admin2
	}
	if names := asList(adm["memberNames"]); len(names) != 2 || names[0] != "admin" || names[1] != "admin2" {
		t.Fatalf("memberNames：%v", names)
	}
	cat := asList(out["catalog"])
	if len(cat) != 5 {
		t.Fatalf("catalog 分组：%v", cat)
	}
	total, locked := 0, 0
	for _, g := range cat {
		gm := asMap(g)
		for _, c := range asList(gm["caps"]) {
			cm := asMap(c)
			total++
			if cm["locked"] == true {
				locked++
			}
			for _, k := range []string{"cap", "name", "desc", "risk", "allowedGrants", "locked"} {
				if _, ok := cm[k]; !ok {
					t.Fatalf("catalog 项缺少 %s：%v", k, cm)
				}
			}
		}
	}
	if total != 20 || locked != 3 {
		t.Fatalf("catalog 能力 %d / locked %d", total, locked)
	}
	if asMap(out["limits"])["maxCustom"] != float64(50) {
		t.Fatalf("limits：%v", out["limits"])
	}
}

func TestRolesPermissions(t *testing.T) {
	e := setupOps(t)
	// 未登录
	if code, _ := e.client(t).do("GET", "/api/roles", nil); code != 401 {
		t.Fatalf("未登录应 401：%d", code)
	}
	// 读：users.manage 或 audit.view（admin / approver / auditor 可读；viewer / dev / sre / dba 不可）
	for user, want := range map[string]int{"admin": 200, "auditor": 200, "approver": 200, "viewer": 403, "dev": 403, "sre": 403, "dba": 403} {
		if code, out := e.login(t, user).do("GET", "/api/roles", nil); code != want {
			t.Errorf("%s GET /api/roles = %d，期望 %d：%v", user, code, want, out)
		}
	}
	// 写：只有 users.manage；auditor / approver 可读不可写
	adm := e.login(t, "admin")
	mkRole(t, adm, "keep", map[string]string{"view.overview": "allow"})
	body := map[string]any{"id": "nope", "name": "不该成功", "caps": map[string]string{"view.overview": "allow"}}
	for _, user := range []string{"auditor", "approver", "dba", "sre", "viewer", "dev"} {
		c := e.login(t, user)
		if code, _ := c.do("POST", "/api/roles", body); code != 403 {
			t.Errorf("%s POST /api/roles 应 403：%d", user, code)
		}
		if code, _ := c.do("POST", "/api/roles/keep", map[string]any{"name": "改名"}); code != 403 {
			t.Errorf("%s POST /api/roles/keep 应 403：%d", user, code)
		}
		if code, _ := c.do("POST", "/api/roles/keep/delete", nil); code != 403 {
			t.Errorf("%s delete 应 403：%d", user, code)
		}
	}
	if r := roleByID(t, adm, "nope"); r != nil {
		t.Fatal("被拒绝的创建不应生效")
	}
	if r := roleByID(t, adm, "keep"); r == nil || r["name"] != "角色keep" {
		t.Fatalf("被拒绝的修改 / 删除不应生效：%v", r)
	}
	// CSRF 缺失
	nocsrf := e.login(t, "admin")
	nocsrf.csrf = ""
	if code, _ := nocsrf.do("POST", "/api/roles", body); code != 403 {
		t.Fatalf("缺少 CSRF 应 403：%d", code)
	}
	acts := auditActions(t, adm)
	if len(acts["authz.deny"]) < 6 {
		t.Fatalf("越权访问应写 authz.deny 审计：%d", len(acts["authz.deny"]))
	}
	if len(acts["security.csrf"]) == 0 {
		t.Fatal("CSRF 失败应入审计")
	}
}

func TestRoleCreateUpdateDeleteFlow(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	r := mkRole(t, adm, "diagonly", map[string]string{"view.overview": "allow", "diag.run": "allow", "view.sql": "scoped"})
	if r["id"] != "diagonly" || r["builtin"] != false || r["updatedBy"] != "admin" || r["members"] != float64(0) || r["created"] == nil {
		t.Fatalf("创建返回：%v", r)
	}
	if c := asMap(r["caps"]); len(c) != 3 || c["view.sql"] != "scoped" {
		t.Fatalf("caps：%v", c)
	}
	// 更新
	code, out := adm.do("POST", "/api/roles/diagonly", map[string]any{"name": "只诊断", "desc": "新说明", "caps": map[string]string{"view.overview": "allow", "diag.run": "allow", "report.export": "allow"}})
	if code != 200 || out["affected"] != float64(0) || out["changed"] != true {
		t.Fatalf("更新：%d %v", code, out)
	}
	if got := roleByID(t, adm, "diagonly"); got["name"] != "只诊断" || got["desc"] != "新说明" || asMap(got["caps"])["report.export"] != "allow" || asMap(got["caps"])["view.sql"] != nil {
		t.Fatalf("更新后：%v", got)
	}
	// 无变化
	code, out = adm.do("POST", "/api/roles/diagonly", map[string]any{"name": "只诊断"})
	if code != 200 || out["changed"] != false {
		t.Fatalf("无变化：%d %v", code, out)
	}
	// 删除
	if code, out = adm.do("POST", "/api/roles/diagonly/delete", nil); code != 200 || out["ok"] != true {
		t.Fatalf("删除：%d %v", code, out)
	}
	if roleByID(t, adm, "diagonly") != nil {
		t.Fatal("删除后应不在列表中")
	}
	if code, out = adm.do("POST", "/api/roles/diagonly/delete", nil); code != 404 || out["code"] != "not_found" {
		t.Fatalf("重复删除：%d %v", code, out)
	}
	if code, out = adm.do("POST", "/api/roles/ghost", map[string]any{"name": "x"}); code != 404 {
		t.Fatalf("不存在：%d %v", code, out)
	}
}

func TestRoleConstraintsOverHTTP(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	post := func(body map[string]any) (int, map[string]any) { return adm.do("POST", "/api/roles", body) }
	base := func(caps map[string]string) map[string]any {
		return map[string]any{"id": "bad1", "name": "bad1", "caps": caps}
	}
	ov := map[string]string{"view.overview": "allow"}
	with := func(k, v string) map[string]string {
		m := map[string]string{"view.overview": "allow"}
		m[k] = v
		return m
	}

	cases := []struct {
		name string
		body map[string]any
		code int
		want string
	}{
		{"b 仅管理员 users.manage", base(with("users.manage", "allow")), 403, "locked_cap"},
		{"b 仅管理员 writeswitch", base(with("writeswitch", "approval")), 403, "locked_cap"},
		{"b 仅管理员 settings.edit", base(with("settings.edit", "allow")), 403, "locked_cap"},
		{"c inst.op scoped", base(with("inst.op", "scoped")), 400, "invalid"},
		{"c view.sql approval", base(with("view.sql", "approval")), 400, "invalid"},
		{"c diag.run approval", base(with("diag.run", "approval")), 400, "invalid"},
		{"c 未知授予方式", base(with("diag.run", "sudo")), 400, "invalid"},
		{"c 未知能力", base(with("no.such", "allow")), 400, "invalid"},
		{"d 缺 view.overview", base(map[string]string{"diag.run": "allow"}), 400, "invalid"},
		{"d 空 caps", base(map[string]string{}), 400, "invalid"},
		{"id 非法", map[string]any{"id": "Bad Id", "name": "x", "caps": ov}, 400, "invalid"},
		{"id 与内置相同", map[string]any{"id": "dba", "name": "x", "caps": ov}, 409, "exists"},
		{"g 名称与内置显示名相同", map[string]any{"id": "bad2", "name": "dba", "caps": ov}, 409, "exists"},
		{"g 名称与内置中文名相同", map[string]any{"id": "bad3", "name": "访客", "caps": ov}, 409, "exists"},
		{"名称过长", map[string]any{"id": "bad4", "name": strings.Repeat("长", 41), "caps": ov}, 400, "invalid"},
		{"说明过长", map[string]any{"id": "bad5", "name": "x5", "desc": strings.Repeat("长", 201), "caps": ov}, 400, "invalid"},
		{"copyFrom 不存在", map[string]any{"id": "bad6", "name": "x6", "copyFrom": "ghost", "caps": ov}, 400, "invalid"},
	}
	for _, c := range cases {
		code, out := post(c.body)
		if code != c.code || out["code"] != c.want || out["error"] == nil || out["error"] == "" {
			t.Errorf("%s：期望 %d/%s，得到 %d %v", c.name, c.code, c.want, code, out)
		}
	}
	if n := len(asList(func() any { _, o := adm.do("GET", "/api/roles", nil); return o["roles"] }())); n != 7 {
		t.Fatalf("所有被拒绝的创建都不应落库：%d 个角色", n)
	}
	// g：id / 名称唯一（名称不区分大小写）
	mkRole(t, adm, "uniq", ov)
	if code, out := post(map[string]any{"id": "uniq", "name": "另一个", "caps": ov}); code != 409 || out["code"] != "exists" {
		t.Fatalf("id 重复：%d %v", code, out)
	}
	if code, out := post(map[string]any{"id": "uniq2", "name": "角色UNIQ", "caps": ov}); code != 409 || out["code"] != "exists" {
		t.Fatalf("名称大小写重复：%d %v", code, out)
	}
	// a：内置角色不可改 / 删
	for _, id := range []string{"viewer", "developer", "sre", "dba", "approver", "admin", "auditor"} {
		if code, out := adm.do("POST", "/api/roles/"+id, map[string]any{"name": "改名"}); code != 403 || out["code"] != "builtin" {
			t.Errorf("改内置 %s：%d %v", id, code, out)
		}
		if code, out := adm.do("POST", "/api/roles/"+id+"/delete", nil); code != 403 || out["code"] != "builtin" {
			t.Errorf("删内置 %s：%d %v", id, code, out)
		}
	}
	// 更新时同样校验
	if code, out := adm.do("POST", "/api/roles/uniq", map[string]any{"caps": with("users.manage", "allow")}); code != 403 || out["code"] != "locked_cap" {
		t.Fatalf("更新加入 locked 能力：%d %v", code, out)
	}
	if code, out := adm.do("POST", "/api/roles/uniq", map[string]any{"caps": map[string]string{"diag.run": "allow"}}); code != 400 {
		t.Fatalf("更新去掉 view.overview：%d %v", code, out)
	}
	if code, out := adm.do("POST", "/api/roles/uniq", map[string]any{"name": "访客"}); code != 409 {
		t.Fatalf("更新成内置名：%d %v", code, out)
	}
	if got := roleByID(t, adm, "uniq"); got["name"] != "角色uniq" || len(asMap(got["caps"])) != 1 {
		t.Fatalf("被拒绝的更新不应生效：%v", got)
	}
}

func TestRoleCopyFrom(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	// 复制内置 admin：locked 项被去掉；body.caps 叠加，none 删除继承来的项
	code, out := adm.do("POST", "/api/roles", map[string]any{"id": "adminlite", "name": "管理员精简", "copyFrom": "admin",
		"caps": map[string]string{"audit.view": "none", "diag.run": "allow"}})
	if code != 200 {
		t.Fatalf("copyFrom admin：%d %v", code, out)
	}
	caps := asMap(asMap(out["role"])["caps"])
	for _, k := range []string{"users.manage", "writeswitch", "settings.edit", "audit.view"} {
		if _, has := caps[k]; has {
			t.Errorf("复制结果不应含 %s", k)
		}
	}
	if caps["release"] != "approval" || caps["diag.run"] != "allow" || caps["view.overview"] != "allow" {
		t.Fatalf("复制结果：%v", caps)
	}
	// 复制自定义角色并降级
	code, out = adm.do("POST", "/api/roles", map[string]any{"id": "dbalite", "name": "DBA 精简", "copyFrom": "dba", "caps": map[string]string{"scale": "none", "view.sql": "scoped"}})
	if code != 200 {
		t.Fatalf("copyFrom dba：%d %v", code, out)
	}
	caps = asMap(asMap(out["role"])["caps"])
	if _, has := caps["scale"]; has || caps["view.sql"] != "scoped" || caps["backup"] != "approval" {
		t.Fatalf("dba 复制结果：%v", caps)
	}
	code, out = adm.do("POST", "/api/roles", map[string]any{"id": "clone2", "name": "再复制", "copyFrom": "dbalite"})
	if code != 200 || asMap(asMap(out["role"])["caps"])["backup"] != "approval" {
		t.Fatalf("复制自定义角色：%d %v", code, out)
	}
	// body 叠加了非法组合依旧被拒
	if code, _ = adm.do("POST", "/api/roles", map[string]any{"id": "clone3", "name": "c3", "copyFrom": "dba", "caps": map[string]string{"diag.run": "approval"}}); code != 400 {
		t.Fatalf("叠加非法组合应 400：%d", code)
	}
}

// 生效即时：成员的会话不重新登录，下一次请求就按新权限判定；删除被成员拒绝。
func TestRoleChangeTakesEffectImmediately(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	mkRole(t, adm, "cust", map[string]string{"view.overview": "allow"})
	mkUser(t, adm, "carol", "cust")
	carol := e.login(t, "carol")

	code, me := carol.do("GET", "/api/auth/me", nil)
	if code != 200 {
		t.Fatal(code, me)
	}
	if roles := asList(me["roles"]); len(roles) != 1 || roles[0] != "cust" {
		t.Fatalf("me.roles：%v", me["roles"])
	}
	if ri := asList(me["roleInfo"]); len(ri) != 1 || asMap(ri[0])["name"] != "角色cust" || asMap(ri[0])["builtin"] != false {
		t.Fatalf("me.roleInfo：%v", me["roleInfo"])
	}
	if c := asMap(me["caps"]); len(c) != 1 || c["view.overview"] != "allow" {
		t.Fatalf("me.caps：%v", c)
	}
	if code, _ = carol.do("GET", "/api/audit", nil); code != 403 {
		t.Fatalf("尚无 audit.view：%d", code)
	}
	if code, _ = carol.do("GET", "/api/roles", nil); code != 403 {
		t.Fatalf("尚无 audit.view / users.manage：%d", code)
	}
	if code, out := carol.do("POST", "/api/roles", map[string]any{"id": "x1", "name": "x", "caps": map[string]string{"view.overview": "allow"}}); code != 403 {
		t.Fatalf("自定义角色成员不能管理角色：%d %v", code, out)
	}

	// 管理员给角色加上 audit.view：carol 的会话不变，下一次请求立即生效
	code, out := adm.do("POST", "/api/roles/cust", map[string]any{"caps": map[string]string{"view.overview": "allow", "audit.view": "allow"}})
	if code != 200 || out["affected"] != float64(1) || asList(out["memberNames"])[0] != "carol" {
		t.Fatalf("更新：%d %v", code, out)
	}
	if code, _ = carol.do("GET", "/api/audit", nil); code != 200 {
		t.Fatalf("角色加权后应立即可读审计：%d", code)
	}
	if code, _ = carol.do("GET", "/api/roles", nil); code != 200 {
		t.Fatalf("audit.view 可读角色列表：%d", code)
	}
	_, me = carol.do("GET", "/api/auth/me", nil)
	if asMap(me["caps"])["audit.view"] != "allow" {
		t.Fatalf("me.caps 应反映新权限：%v", me["caps"])
	}
	// 再收回
	if code, out = adm.do("POST", "/api/roles/cust", map[string]any{"caps": map[string]string{"view.overview": "allow"}}); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ = carol.do("GET", "/api/audit", nil); code != 403 {
		t.Fatalf("收回后应立即 403：%d", code)
	}

	// f：仍有成员 → 拒绝删除，返回成员名单
	code, out = adm.do("POST", "/api/roles/cust/delete", nil)
	if code != 409 || out["code"] != "in_use" || len(asList(out["members"])) != 1 || asList(out["members"])[0] != "carol" {
		t.Fatalf("在用角色删除：%d %v", code, out)
	}
	if roleByID(t, adm, "cust") == nil {
		t.Fatal("被拒绝的删除不应生效")
	}
	if got := roleByID(t, adm, "cust"); got["members"] != float64(1) {
		t.Fatalf("members：%v", got)
	}
	// 移除成员后可删
	if code, out = adm.do("POST", "/api/users/carol/roles", map[string]any{"roles": map[string][]string{"*": {"viewer"}}}); code != 200 {
		t.Fatalf("改角色：%d %v", code, out)
	}
	if code, out = adm.do("POST", "/api/roles/cust/delete", nil); code != 200 {
		t.Fatalf("删除：%d %v", code, out)
	}
	// 任何环境持有都算：按 prod 键持有也拒绝
	mkRole(t, adm, "envrole", map[string]string{"view.overview": "allow"})
	mkUser(t, adm, "dave", "viewer")
	if code, out = adm.do("POST", "/api/users/dave/roles", map[string]any{"roles": map[string][]string{"*": {"viewer"}, "prod": {"envrole"}}}); code != 200 {
		t.Fatal(code, out)
	}
	if code, out = adm.do("POST", "/api/roles/envrole/delete", nil); code != 409 {
		t.Fatalf("prod 键持有也应拒绝：%d %v", code, out)
	}
}

// 自定义角色驱动实际能力：变更接口的 opCan 靠 caps 判定。
func TestCustomRoleDrivesChangeCapabilities(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	mkRole(t, adm, "restarter", map[string]string{"view.overview": "allow", "inst.op": "approval", "view.sql": "scoped"})
	mkUser(t, adm, "rick", "restarter")
	rick := e.login(t, "rick")
	_, me := rick.do("GET", "/api/auth/me", nil)
	if c := asMap(me["caps"]); c["inst.op"] != "approval" || c["view.sql"] != "scoped" || c["scale"] != nil {
		t.Fatalf("caps：%v", c)
	}
	code, out := rick.do("POST", "/api/changes/preview", map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "restart"}})
	if code != 200 {
		t.Fatalf("inst.op 预览应放行：%d %v", code, out)
	}
	code, out = rick.do("POST", "/api/changes/preview", map[string]any{"kind": "scale.in", "params": map[string]any{}})
	if code != 403 {
		t.Fatalf("没有 scale 能力应 403：%d %v", code, out)
	}
	// 自定义角色不能审批（没有 approve.*）
	if code, _ = rick.do("GET", "/api/ops/settings", nil); code != 200 {
		t.Fatal("登录即可读设置")
	}
	if code, _ = rick.do("POST", "/api/ops/settings", map[string]any{"lb": true}); code != 403 {
		t.Fatalf("没有 settings.edit：%d", code)
	}
	if code, _ = rick.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true}); code != 403 {
		t.Fatalf("没有 writeswitch：%d", code)
	}
	if code, _ = rick.do("GET", "/api/users", nil); code != 403 {
		t.Fatalf("没有 users.manage：%d", code)
	}
}

func TestUsersAPIIncludesCustomRoles(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	mkRole(t, adm, "analyst", map[string]string{"view.overview": "allow"})
	code, out := adm.do("GET", "/api/users", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	ids := asList(out["roles"])
	if len(ids) != 8 || ids[0] != "viewer" || ids[6] != "auditor" || ids[7] != "analyst" {
		t.Fatalf("roles：%v", ids)
	}
	info := asList(out["roleInfo"])
	if len(info) != 8 {
		t.Fatalf("roleInfo：%v", info)
	}
	if a := asMap(info[7]); a["id"] != "analyst" || a["name"] != "角色analyst" || a["builtin"] != false {
		t.Fatalf("roleInfo[7]：%v", a)
	}
	if a := asMap(info[0]); a["id"] != "viewer" || a["name"] != "访客" || a["builtin"] != true {
		t.Fatalf("roleInfo[0]：%v", a)
	}
	// 现有字段仍在
	u := asMap(asList(out["users"])[0])
	for _, k := range []string{"name", "display", "roles", "dbs", "disabled", "hasTotp", "created", "dept", "email", "phone", "note"} {
		if _, ok := u[k]; !ok {
			t.Errorf("users[0] 缺少字段 %s：%v", k, u)
		}
	}
	// 创建 / 改角色认自定义角色，未知角色拒绝
	mkUser(t, adm, "erin", "analyst")
	if code, out = adm.do("POST", "/api/users", map[string]any{"name": "frank", "password": authz.DemoPassword, "roles": map[string][]string{"*": {"ghostrole"}}}); code != 400 {
		t.Fatalf("未知角色应 400：%d %v", code, out)
	}
	if code, out = adm.do("POST", "/api/users/erin/roles", map[string]any{"roles": map[string][]string{"*": {"viewer", "analyst"}}}); code != 200 {
		t.Fatalf("多角色（内置 + 自定义）：%d %v", code, out)
	}
	if rs := rolesOf(t, adm, "erin"); !contains(rs, "analyst") || !contains(rs, "viewer") {
		t.Fatalf("erin 角色：%v", rs)
	}
	if code, out = adm.do("POST", "/api/users/erin/roles", map[string]any{"roles": map[string][]string{"*": {"deleted-role"}}}); code != 400 {
		t.Fatalf("改成未知角色应 400：%d %v", code, out)
	}
	// 授予 admin 仍走双人复核，自定义角色不影响
	code, out = adm.do("POST", "/api/users/erin/roles", map[string]any{"roles": map[string][]string{"*": {"admin", "analyst"}}})
	if code != 202 {
		t.Fatalf("授予 admin 应走提案：%d %v", code, out)
	}
}

func TestUserProfileAndLastLogin(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	// 创建时带资料
	code, out := adm.do("POST", "/api/users", map[string]any{"name": "gina", "display": "吉娜", "password": authz.DemoPassword,
		"roles": map[string][]string{"*": {"viewer"}}, "dept": "数据库组", "email": "gina@example.com", "phone": "+86 13800000000", "note": "实习生"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	find := func(name string) map[string]any {
		_, o := adm.do("GET", "/api/users", nil)
		for _, u := range asList(o["users"]) {
			if m := asMap(u); m["name"] == name {
				return m
			}
		}
		t.Fatalf("没有用户 %s", name)
		return nil
	}
	g := find("gina")
	if g["dept"] != "数据库组" || g["email"] != "gina@example.com" || g["phone"] != "+86 13800000000" || g["note"] != "实习生" || g["display"] != "吉娜" {
		t.Fatalf("资料：%v", g)
	}
	if g["lastLogin"] != nil || g["lockedUntil"] != nil {
		t.Fatalf("未登录用户不应有 lastLogin / lockedUntil：%v", g)
	}
	if find("admin")["lastLogin"] == nil {
		t.Fatal("admin 刚登录过，应有 lastLogin")
	}
	// 非法资料
	for name, body := range map[string]map[string]any{
		"邮箱":  {"email": "not-an-email"},
		"手机":  {"phone": "abc"},
		"备注":  {"note": strings.Repeat("长", 201)},
		"部门":  {"dept": strings.Repeat("长", 41)},
		"显示名": {"display": strings.Repeat("长", 41)},
	} {
		if code, out := adm.do("POST", "/api/users/gina/profile", body); code != 400 {
			t.Errorf("非法%s应 400：%d %v", name, code, out)
		}
	}
	if code, _ = adm.do("POST", "/api/users", map[string]any{"name": "hank", "password": authz.DemoPassword, "email": "bad"}); code != 400 {
		t.Fatalf("创建时邮箱非法应 400：%d", code)
	}
	if code, _ = adm.do("POST", "/api/users/ghost/profile", map[string]any{"dept": "x"}); code != 404 {
		t.Fatalf("不存在的用户应 404：%d", code)
	}
	// 更新
	code, out = adm.do("POST", "/api/users/gina/profile", map[string]any{"dept": "平台组", "email": "gina.new@example.com", "display": "吉娜（新）"})
	if code != 200 || len(asList(out["changed"])) != 3 {
		t.Fatalf("更新资料：%d %v", code, out)
	}
	g = find("gina")
	if g["dept"] != "平台组" || g["email"] != "gina.new@example.com" || g["phone"] != "+86 13800000000" || g["display"] != "吉娜（新）" {
		t.Fatalf("未传的字段保持不变：%v", g)
	}
	// 权限
	for _, user := range []string{"auditor", "dba", "viewer"} {
		if code, _ := e.login(t, user).do("POST", "/api/users/gina/profile", map[string]any{"dept": "x"}); code != 403 {
			t.Errorf("%s 改资料应 403：%d", user, code)
		}
	}
	// 登录成功后 lastLogin 出现
	e.login(t, "gina")
	if find("gina")["lastLogin"] == nil {
		t.Fatal("登录后应有 lastLogin")
	}
	// 审计：只记字段名，不含邮箱 / 手机 / 备注内容
	acts := auditActions(t, adm)
	profs := acts["user.profile"]
	if len(profs) == 0 {
		t.Fatal("缺少 user.profile 审计")
	}
	var successSum string
	for _, p := range profs {
		if p["result"] == "success" {
			successSum = p["summary"].(string)
		}
		if p["result"] == "success" && p["target"] != "gina" {
			t.Fatalf("target：%v", p)
		}
	}
	if successSum != "更新资料：显示名、部门、邮箱" && successSum != "更新资料：部门、邮箱、显示名" {
		// changed 的顺序是 display, dept, email
		if !strings.HasPrefix(successSum, "更新资料：") || !strings.Contains(successSum, "部门") || !strings.Contains(successSum, "邮箱") {
			t.Fatalf("摘要：%q", successSum)
		}
	}
	assertNoSecretsInAudit(t, acts, "gina.new@example.com", "gina@example.com", "13800000000", "实习生", authz.DemoPassword)
}

func assertNoSecretsInAudit(t *testing.T, acts map[string][]map[string]any, secrets ...string) {
	t.Helper()
	for action, es := range acts {
		for _, en := range es {
			b, _ := json.Marshal(en)
			for _, s := range secrets {
				if strings.Contains(string(b), s) {
					t.Errorf("审计 %s 里不应出现敏感内容 %q：%s", action, s, b)
				}
			}
		}
	}
}

func TestUnlockUser(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	others := map[string]*client{"auditor": e.login(t, "auditor"), "dba": e.login(t, "dba")} // 同一 IP 被锁定后就无法再登录，先登录好
	// 连续 5 次错误口令把 dev 锁定
	anon := e.client(t)
	for i := 0; i < 5; i++ {
		anon.do("POST", "/api/auth/login", map[string]string{"user": "dev", "password": "wrong-Password-1"})
	}
	code, _ := anon.do("POST", "/api/auth/login", map[string]string{"user": "dev", "password": authz.DemoPassword})
	if code != 429 {
		t.Fatalf("应被锁定：%d", code)
	}
	find := func(name string) map[string]any {
		_, o := adm.do("GET", "/api/users", nil)
		for _, u := range asList(o["users"]) {
			if m := asMap(u); m["name"] == name {
				return m
			}
		}
		return nil
	}
	if find("dev")["lockedUntil"] == nil {
		t.Fatal("GET /api/users 应给出 lockedUntil")
	}
	if find("viewer")["lockedUntil"] != nil {
		t.Fatal("其他用户不应有 lockedUntil")
	}
	// 权限
	for user, c := range others {
		if code, _ := c.do("POST", "/api/users/dev/unlock", nil); code != 403 {
			t.Errorf("%s unlock 应 403：%d", user, code)
		}
	}
	if code, _ := adm.do("POST", "/api/users/ghost/unlock", nil); code != 404 {
		t.Fatalf("不存在的用户：%d", code)
	}
	code, out := adm.do("POST", "/api/users/dev/unlock", nil)
	if code != 200 || out["wasLocked"] != true {
		t.Fatalf("解锁：%d %v", code, out)
	}
	if find("dev")["lockedUntil"] != nil {
		t.Fatal("解锁后不应再有 lockedUntil")
	}
	// 再解锁：当时未锁定
	if code, out = adm.do("POST", "/api/users/dev/unlock", nil); code != 200 || out["wasLocked"] != false {
		t.Fatalf("重复解锁：%d %v", code, out)
	}
	// 只清用户维度：同一 IP 的失败也计满了 5 次，IP 维度锁定不被清除（见 docs 已知限制）
	if code, _ = anon.do("POST", "/api/auth/login", map[string]string{"user": "dev", "password": authz.DemoPassword}); code != 429 {
		t.Fatalf("IP 维度仍被锁定：%d", code)
	}
	acts := auditActions(t, adm)
	us := acts["user.unlock"]
	if len(us) < 3 {
		t.Fatalf("user.unlock 审计：%d", len(us))
	}
	var denied, ok bool
	for _, u := range us {
		denied = denied || u["result"] == "denied"
		ok = ok || (u["result"] == "success" && strings.Contains(u["summary"].(string), "解除登录锁定"))
	}
	if !ok || !denied {
		t.Fatalf("审计应含成功与失败：%v", us)
	}
	assertNoSecretsInAudit(t, acts, authz.DemoPassword, "wrong-Password-1")
}

func TestRoleAuditTrail(t *testing.T) {
	e := setupOps(t)
	adm := e.login(t, "admin")
	mkRole(t, adm, "audited", map[string]string{"view.overview": "allow", "diag.run": "allow"})
	adm.do("POST", "/api/roles/audited", map[string]any{"caps": map[string]string{"view.overview": "allow", "inst.op": "approval", "scale": "allow"}})
	adm.do("POST", "/api/roles", map[string]any{"id": "evil", "name": "evil", "caps": map[string]string{"view.overview": "allow", "users.manage": "allow"}}) // 拒绝
	adm.do("POST", "/api/roles/admin", map[string]any{"name": "x"})                                                                                          // 拒绝
	mkUser(t, adm, "zed", "audited")
	adm.do("POST", "/api/roles/audited/delete", nil) // 在用，拒绝
	adm.do("POST", "/api/users/zed/roles", map[string]any{"roles": map[string][]string{"*": {"viewer"}}})
	adm.do("POST", "/api/roles/audited/delete", nil)

	acts := auditActions(t, adm)
	find := func(action, result, contains string) bool {
		for _, en := range acts[action] {
			if en["result"] == result && strings.Contains(en["summary"].(string), contains) && en["user"] == "admin" && en["risk"] == "high" {
				return true
			}
		}
		return false
	}
	if !find("role.create", "success", "+view.overview(allow) +diag.run(allow)") {
		t.Errorf("role.create 摘要应含差异：%v", acts["role.create"])
	}
	if !find("role.update", "success", "−diag.run") || !find("role.update", "success", "+inst.op(approval)") || !find("role.update", "success", "+scale(allow)") {
		t.Errorf("role.update 摘要应含 +/− 差异：%v", acts["role.update"])
	}
	if !find("role.update", "success", "即时影响 0 名成员") {
		t.Errorf("role.update 摘要应含影响成员数")
	}
	if !find("role.create", "denied", "users.manage") {
		t.Errorf("被拒绝的创建应记 denied：%v", acts["role.create"])
	}
	if !find("role.update", "denied", "内置角色") {
		t.Errorf("改内置角色应记 denied：%v", acts["role.update"])
	}
	if !find("role.delete", "denied", "zed") {
		t.Errorf("在用角色删除应记 denied 并含成员：%v", acts["role.delete"])
	}
	if !find("role.delete", "success", "删除自定义角色") {
		t.Errorf("删除成功应入审计：%v", acts["role.delete"])
	}
	for _, en := range acts["role.create"] {
		if en["target"] == "" {
			t.Fatal("target 应是角色 id")
		}
	}
	assertNoSecretsInAudit(t, acts, authz.DemoPassword)
	// 审计链仍然完整
	if code, out := adm.do("GET", "/api/audit/verify", nil); code != 200 || out["ok"] != true {
		t.Fatalf("审计链：%d %v", code, out)
	}
	// 按动作筛选可用
	code, out := adm.do("GET", "/api/audit?action=role.create&limit=50", nil)
	if code != 200 || len(asList(out["entries"])) < 2 {
		t.Fatalf("按 role.create 筛选：%d %v", code, out)
	}
}

func TestRolesPersistAndLoadDowngradeAtStartup(t *testing.T) {
	// 预置一个被手改的 roles.json：含 users.manage 的自定义角色，启动后必须被丢弃，且写入审计 role.load
	bad := `{"version":1,"roles":[{"id":"sneaky","name":"潜入","desc":"","caps":{"view.overview":"allow","users.manage":"allow","settings.edit":"allow","audit.view":"allow"},"created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z","updatedBy":"hand"}]}`
	e := setupOpsPre(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "roles.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	adm := e.login(t, "admin")
	r := roleByID(t, adm, "sneaky")
	if r == nil {
		t.Fatal("合法部分应保留")
	}
	caps := asMap(r["caps"])
	if _, has := caps["users.manage"]; has {
		t.Fatalf("users.manage 必须被丢弃：%v", caps)
	}
	if _, has := caps["settings.edit"]; has || caps["audit.view"] != "allow" {
		t.Fatalf("caps：%v", caps)
	}
	_, out := adm.do("GET", "/api/roles", nil)
	if ws := asList(out["warnings"]); len(ws) < 2 {
		t.Fatalf("应返回告警：%v", out["warnings"])
	}
	acts := auditActions(t, adm)
	if len(acts["role.load"]) != 1 || acts["role.load"][0]["result"] != "failed" || !strings.Contains(acts["role.load"][0]["summary"].(string), "users.manage") {
		t.Fatalf("应写 role.load 审计：%v", acts["role.load"])
	}
	// 该角色的成员实际没有 users.manage
	mkUser(t, adm, "sly", "sneaky")
	sly := e.login(t, "sly")
	if code, _ := sly.do("POST", "/api/users/sly/unlock", map[string]any{}); code != 403 {
		t.Fatalf("手改的存储不得提权：%d", code)
	}
	if code, _ := sly.do("GET", "/api/audit", nil); code != 200 {
		t.Fatalf("合法的 audit.view 仍然有效：%d", code)
	}
	// 修改后落盘：roles.json 权限 0600 且不再含 users.manage
	if code, out := adm.do("POST", "/api/roles/sneaky", map[string]any{"desc": "已修复"}); code != 200 {
		t.Fatal(code, out)
	}
	p := filepath.Join(e.dir, "roles.json")
	b, err := os.ReadFile(p)
	if err != nil || strings.Contains(string(b), "users.manage") || !strings.Contains(string(b), "已修复") {
		t.Fatalf("落盘内容：%s %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("权限：%v", st.Mode())
		}
	}
	// 自检接口包含 roles.json 权限检查项不应报错
	if code, _ := adm.do("GET", "/api/ops/selfcheck", nil); code != 200 {
		t.Fatal("selfcheck")
	}
}

// 审计员（audit.view）可只读查看用户清单（合规核对），但不能做任何用户写操作；DBA 连读都不行。
func TestAuditorCanReadUsersButNotWrite(t *testing.T) {
	e := setupOps(t)
	aud, dba := e.login(t, "auditor"), e.login(t, "dba")
	code, out := aud.do("GET", "/api/users", nil)
	if code != 200 || len(asList(out["users"])) == 0 {
		t.Fatalf("审计员应能只读用户清单：%d %v", code, out)
	}
	for _, u := range asList(out["users"]) {
		for _, k := range []string{"password", "hash", "totp", "secret"} {
			if _, has := asMap(u)[k]; has {
				t.Fatalf("用户清单不得含敏感字段 %s", k)
			}
		}
	}
	if code, _ := aud.do("POST", "/api/users/dba/unlock", map[string]any{}); code != 403 {
		t.Fatalf("审计员不能解锁用户：%d", code)
	}
	if code, _ := aud.do("POST", "/api/users/dba/disable", map[string]any{"disabled": true}); code != 403 {
		t.Fatalf("审计员不能禁用用户：%d", code)
	}
	if code, _ := dba.do("GET", "/api/users", nil); code != 403 {
		t.Fatalf("DBA 不能读用户清单：%d", code)
	}
}
