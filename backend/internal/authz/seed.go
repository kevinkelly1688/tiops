// @author: kevinkelly
// @license: Apache License 2.0
// @file: seed.go
// @time: 2026/10/28

package authz

// DemoPassword 是演示账号的统一口令，仅供回放 / 演示模式使用。
const DemoPassword = "Tidbops-Demo-1"

// SeedDemoUsers 在用户库为空时创建演示账号：admin、admin2、dba、sre、approver、approver2、viewer、auditor、
// dev（共 9 个，dev 的 DBs=["shop"]），口令统一为 DemoPassword。非空库不做任何事。
// 调用方负责只在回放 / 演示模式调用，生产模式不得调用。
func SeedDemoUsers(s *Store) error {
	if len(s.List()) > 0 {
		return nil
	}
	all := func(r Role) map[string][]Role { return map[string][]Role{EnvDefault: {r}} }
	demo := []struct {
		name, display string
		role          Role
		dbs           []string
	}{
		{"admin", "平台管理员", RoleAdmin, nil},
		{"admin2", "平台管理员（复核）", RoleAdmin, nil},
		{"dba", "DBA", RoleDBA, nil},
		{"sre", "值班 SRE", RoleSRE, nil},
		{"approver", "审批人", RoleApprover, nil},
		{"approver2", "审批人（二级）", RoleApprover, nil},
		{"viewer", "访客", RoleViewer, nil},
		{"auditor", "审计员", RoleAuditor, nil},
		{"dev", "业务开发", RoleDeveloper, []string{"shop"}},
	}
	for _, d := range demo {
		if err := s.Add(d.name, d.display, DemoPassword, all(d.role), d.dbs); err != nil {
			return err
		}
	}
	return nil
}
