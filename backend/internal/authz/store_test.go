// @author: kevinkelly
// @license: Apache License 2.0
// @file: store_test.go
// @time: 2026/10/28

package authz

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

const pw = "Str0ng-Passw0rd"

func newStore(t *testing.T, key []byte) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "users.json")
	s, err := OpenStore(p, key)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func addAdmin(t *testing.T, s *Store, name string) {
	t.Helper()
	if err := s.Add(name, name, pw, map[string][]Role{"*": {RoleAdmin}}, nil); err != nil {
		t.Fatal(err)
	}
}

func addUser(t *testing.T, s *Store, name string, role Role) {
	t.Helper()
	if err := s.Add(name, name, pw, map[string][]Role{"*": {role}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCRUDAndPersistence(t *testing.T) {
	s, p := newStore(t, nil)
	if err := s.Add("Alice", "爱丽丝", pw, map[string][]Role{"*": {RoleSRE}, "test": {RoleDBA, RoleDBA}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("dev1", "", pw, map[string][]Role{"*": {RoleDeveloper}}, []string{"shop"}); err != nil {
		t.Fatal(err)
	}
	u, ok := s.Get("ALICE") // 大小写不敏感
	if !ok || u.Name != "alice" || u.Display != "爱丽丝" || u.Created.IsZero() {
		t.Fatalf("%+v", u)
	}
	if rs := u.RolesFor("test"); len(rs) != 1 || rs[0] != RoleDBA {
		t.Fatalf("角色应去重：%v", rs)
	}
	if d, _ := s.Get("dev1"); d.Display != "dev1" || len(d.DBs) != 1 || d.DBs[0] != "shop" {
		t.Fatalf("%+v", d)
	}
	// 错误路径
	if err := s.Add("alice", "x", pw, nil, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("重复用户：%v", err)
	}
	if err := s.Add("a b", "x", pw, nil, nil); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("非法用户名：%v", err)
	}
	if err := s.Add("bob", "x", "short", nil, nil); err == nil {
		t.Fatal("弱口令应被拒绝")
	}
	if err := s.Add("bob", "x", "Bob-Passw0rd-1", nil, nil); err == nil {
		t.Fatal("口令含用户名应被拒绝")
	}
	if err := s.Add("bob", "x", pw, map[string][]Role{"*": {"root"}}, nil); err == nil {
		t.Fatal("未知角色应被拒绝")
	}
	if _, ok := s.Get("bob"); ok {
		t.Fatal("失败的 Add 不应留下用户")
	}
	// Get 返回副本
	u.Roles["*"][0] = RoleAdmin
	if u2, _ := s.Get("alice"); u2.Roles["*"][0] != RoleSRE {
		t.Fatal("Get 应返回深拷贝")
	}

	if err := s.SetRoles("alice", map[string][]Role{"*": {RoleViewer}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDBs("dev1", []string{"shop", "pay"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled("dev1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("alice", "New-Passw0rd-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword("alice", "weak"); err == nil {
		t.Fatal("SetPassword 应校验策略")
	}
	if err := s.SetRoles("nobody", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}

	// 重开：全部持久化
	s2, err := OpenStore(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := s2.List()
	if len(l) != 2 || l[0].Name != "alice" || l[1].Name != "dev1" {
		t.Fatalf("List=%+v", l)
	}
	if !l[1].Disabled || len(l[1].DBs) != 2 || l[0].RolesFor("test")[0] != RoleViewer {
		t.Fatalf("状态未持久化：%+v", l)
	}
	if _, err := s2.Authenticate("alice", "New-Passw0rd-2", "", "1.1.1.1", t0); err != nil {
		t.Fatalf("新口令应可登录：%v", err)
	}
	if _, err := s2.Authenticate("alice", pw, "", "1.1.1.1", t0); err == nil {
		t.Fatal("旧口令不应可登录")
	}
	if err := s2.Delete("dev1"); err != nil {
		t.Fatal(err)
	}
	s3, _ := OpenStore(p, nil)
	if len(s3.List()) != 1 {
		t.Fatal("删除未持久化")
	}
	// 文件不含明文口令；无残留临时文件
	b, _ := os.ReadFile(p)
	if bytes.Contains(b, []byte("New-Passw0rd-2")) || !bytes.Contains(b, []byte("pbkdf2-sha256$")) {
		t.Fatal("口令应以哈希存储")
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("目录里应只有用户库文件：%v", ents)
	}
	if os.PathSeparator == '/' {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("权限=%v", st.Mode())
		}
	}
}

func TestOpenStoreRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "u.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := OpenStore(p, nil); err == nil {
		t.Fatal("损坏的用户库应报错")
	}
}

func TestStoreSaveFailureKeepsMemoryConsistent(t *testing.T) {
	s, _ := newStore(t, nil)
	addAdmin(t, s, "root1")
	s.path = filepath.Join(t.TempDir(), "no-such-dir", "u.json") // 让落盘失败
	if err := s.Add("carol", "c", pw, nil, nil); err == nil {
		t.Fatal("落盘失败应返回错误")
	}
	if _, ok := s.Get("carol"); ok {
		t.Fatal("落盘失败不应改变内存状态")
	}
}

func TestAuthenticateBasic(t *testing.T) {
	s, _ := newStore(t, nil)
	addUser(t, s, "alice", RoleSRE)
	u, err := s.Authenticate(" Alice ", pw, "", "10.0.0.1", t0)
	if err != nil || u.Name != "alice" {
		t.Fatalf("%+v %v", u, err)
	}
	// 各类失败信息完全一致
	s.SetDisabled("alice", false)
	addUser(t, s, "off", RoleViewer)
	s.SetDisabled("off", true)
	msgs := map[string]error{}
	_, msgs["no-user"] = s.Authenticate("ghost", pw, "", "10.0.0.2", t0)
	_, msgs["bad-pw"] = s.Authenticate("alice", "wrong-Passw0rd", "", "10.0.0.3", t0)
	_, msgs["disabled"] = s.Authenticate("off", pw, "", "10.0.0.4", t0)
	_, msgs["empty"] = s.Authenticate("", "", "", "10.0.0.5", t0)
	for k, e := range msgs {
		if !errors.Is(e, ErrInvalidCredentials) || e.Error() != "用户名或口令错误" {
			t.Errorf("%s：错误信息不统一：%v", k, e)
		}
	}
}

func TestLockoutByUser(t *testing.T) {
	s, _ := newStore(t, nil)
	addUser(t, s, "alice", RoleSRE)
	for i := 0; i < 5; i++ {
		// 换 IP，只触发“用户”维度
		_, err := s.Authenticate("alice", "wrong-Passw0rd", "", "10.0.0."+string(rune('1'+i)), t0.Add(time.Duration(i)*time.Second))
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次：%v", i+1, err)
		}
	}
	// 锁定期内，即使口令正确也被拒绝
	now := t0.Add(5 * time.Minute)
	_, err := s.Authenticate("alice", pw, "", "10.9.9.9", now)
	var le *LockedError
	if !errors.Is(err, ErrLocked) || !errors.As(err, &le) {
		t.Fatalf("应被锁定：%v", err)
	}
	if want := t0.Add(4 * time.Second).Add(10 * time.Minute); !le.Until.Equal(want) {
		t.Fatalf("解锁时间=%v want %v", le.Until, want)
	}
	// 锁定到期后恢复
	if _, err := s.Authenticate("alice", pw, "", "10.9.9.9", t0.Add(4*time.Second+10*time.Minute)); err != nil {
		t.Fatalf("到期后应可登录：%v", err)
	}
}

func TestLockoutByIPAndNonexistentUser(t *testing.T) {
	s, _ := newStore(t, nil)
	addUser(t, s, "alice", RoleSRE)
	// 同一 IP 对不同（含不存在的）用户猜 5 次
	for i := 0; i < 5; i++ {
		s.Authenticate("ghost"+string(rune('a'+i)), "x", "", "6.6.6.6", t0)
	}
	if _, err := s.Authenticate("alice", pw, "", "6.6.6.6", t0.Add(time.Minute)); !errors.Is(err, ErrLocked) {
		t.Fatalf("该 IP 应被锁定：%v", err)
	}
	// 其他 IP 不受影响
	if _, err := s.Authenticate("alice", pw, "", "7.7.7.7", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 不存在的用户名同样会被锁定（行为与存在的用户一致，不泄露存在性）
	for i := 0; i < 5; i++ {
		s.Authenticate("phantom", "x", "", "8.8.8.8", t0)
	}
	_, err := s.Authenticate("phantom", "x", "", "9.9.9.9", t0.Add(time.Second))
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("不存在用户也应锁定：%v", err)
	}
}

func TestFailureWindowAndSuccessReset(t *testing.T) {
	s, _ := newStore(t, nil)
	addUser(t, s, "alice", RoleSRE)
	for i := 0; i < 4; i++ {
		s.Authenticate("alice", "wrong-Passw0rd", "", "", t0)
	}
	// 成功登录清零用户计数
	if _, err := s.Authenticate("alice", pw, "", "", t0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		s.Authenticate("alice", "wrong-Passw0rd", "", "", t0.Add(time.Second))
	}
	if _, err := s.Authenticate("alice", pw, "", "", t0.Add(2*time.Second)); err != nil {
		t.Fatalf("成功后计数应清零：%v", err)
	}
	// 窗口外的失败不累计
	for i := 0; i < 4; i++ {
		s.Authenticate("alice", "wrong-Passw0rd", "", "", t0)
	}
	later := t0.Add(11 * time.Minute)
	s.Authenticate("alice", "wrong-Passw0rd", "", "", later)
	if _, err := s.Authenticate("alice", pw, "", "", later.Add(time.Second)); err != nil {
		t.Fatalf("窗口过期后不应锁定：%v", err)
	}
}

func enrollTOTP(t *testing.T, s *Store, name string, now time.Time) string {
	t.Helper()
	secret, uri, err := s.BeginTOTP(name)
	if err != nil || !strings.HasPrefix(uri, "otpauth://totp/") || !strings.Contains(uri, secret) {
		t.Fatalf("%q %q %v", secret, uri, err)
	}
	if err := s.ConfirmTOTP(name, TOTPCode(secret, now), now); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestTOTPEnrollmentAndLogin(t *testing.T) {
	s, _ := newStore(t, []byte("master-key"))
	addUser(t, s, "alice", RoleDBA)
	if u, _ := s.Get("alice"); u.HasTOTP {
		t.Fatal("未绑定")
	}
	if err := s.ConfirmTOTP("alice", "123456", t0); !errors.Is(err, ErrTOTPNotPending) {
		t.Fatalf("%v", err)
	}
	secret, _, err := s.BeginTOTP("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmTOTP("alice", "000000", t0); !errors.Is(err, ErrBadOTP) {
		t.Fatalf("错误验证码：%v", err)
	}
	if u, _ := s.Get("alice"); u.HasTOTP {
		t.Fatal("确认失败不应启用")
	}
	// 待确认期间仍可只用口令登录
	if _, err := s.Authenticate("alice", pw, "", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmTOTP("alice", TOTPCode(secret, t0), t0); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get("alice"); !u.HasTOTP {
		t.Fatal("应已绑定")
	}
	if _, _, err := s.BeginTOTP("alice"); !errors.Is(err, ErrTOTPBound) {
		t.Fatalf("已绑定不能重复开始：%v", err)
	}
	// 登录只认口令：已绑定 TOTP 的用户不带动态码也能登录，错误口令仍然失败
	if _, err := s.Authenticate("alice", pw, "", "", t0.Add(time.Minute)); err != nil {
		t.Fatalf("已绑定 TOTP 的用户只用口令应能登录：%v", err)
	}
	if _, err := s.Authenticate("alice", "wrong-Passw0rd", "", "", t0.Add(time.Minute)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错口令应失败：%v", err)
	}
}

func TestVerifyOTPReplayAndLock(t *testing.T) {
	s, _ := newStore(t, nil)
	addUser(t, s, "alice", RoleDBA)
	addUser(t, s, "bob", RoleDBA) // 未绑定
	secret := enrollTOTP(t, s, "alice", t0)

	t1 := t0.Add(90 * time.Second)
	code := TOTPCode(secret, t1)
	if !s.VerifyOTP("alice", code, t1) {
		t.Fatal("有效验证码应通过")
	}
	if s.VerifyOTP("alice", code, t1) {
		t.Fatal("同一验证码不可重复使用")
	}
	// 窗口 ±1：上一个步长的码（比 lastStep 新）仍可用
	t2 := t1.Add(60 * time.Second)
	if !s.VerifyOTP("alice", TOTPCode(secret, t2.Add(-30*time.Second)), t2) {
		t.Fatal("±1 窗口内的码应可用")
	}
	if s.VerifyOTP("bob", "123456", t2) || s.VerifyOTP("ghost", "123456", t2) {
		t.Fatal("未绑定 / 不存在的用户应返回 false")
	}
	// 连续失败 5 次后锁定二次确认
	t3 := t2.Add(5 * time.Minute)
	for i := 0; i < 5; i++ {
		if s.VerifyOTP("alice", "000000", t3) {
			t.Fatal("错误验证码")
		}
	}
	if s.VerifyOTP("alice", TOTPCode(secret, t3), t3.Add(time.Second)) {
		t.Fatal("应被锁定")
	}
	if !s.VerifyOTP("alice", TOTPCode(secret, t3.Add(11*time.Minute)), t3.Add(11*time.Minute)) {
		t.Fatal("锁定到期后应恢复")
	}
}

func TestResetTOTP(t *testing.T) {
	s, _ := newStore(t, nil)
	addUser(t, s, "alice", RoleDBA)
	secret := enrollTOTP(t, s, "alice", t0)
	if err := s.ResetTOTP("alice"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get("alice"); u.HasTOTP {
		t.Fatal("应已解绑")
	}
	if _, err := s.Authenticate("alice", pw, "", "", t0); err != nil {
		t.Fatalf("解绑后只需口令：%v", err)
	}
	if s.VerifyOTP("alice", TOTPCode(secret, t0.Add(time.Hour)), t0.Add(time.Hour)) {
		t.Fatal("旧密钥应失效")
	}
	if _, _, err := s.BeginTOTP("alice"); err != nil {
		t.Fatalf("可重新绑定：%v", err)
	}
	if err := s.ResetTOTP("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestTOTPEncryptedAtRest(t *testing.T) {
	key := []byte("a-master-key")
	s, p := newStore(t, key)
	addUser(t, s, "alice", RoleDBA)
	secret, _, _ := s.BeginTOTP("alice")
	b, _ := os.ReadFile(p)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("待确认的 TOTP 密钥不应明文落盘")
	}
	if err := s.ConfirmTOTP("alice", TOTPCode(secret, t0), t0); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatalf("TOTP 密钥不应明文落盘：\n%s", b)
	}
	if !bytes.Contains(b, []byte(`"totpEnc": true`)) {
		t.Fatalf("应标记已加密：\n%s", b)
	}

	// 正确密钥重开：可继续使用
	s2, err := OpenStore(p, key)
	if err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(2 * time.Minute)
	if _, err := s2.Authenticate("alice", pw, TOTPCode(secret, t1), "", t1); err != nil {
		t.Fatalf("重开后 TOTP 应可用：%v", err)
	}
	// 缺少 / 错误的主密钥
	if _, err := OpenStore(p, nil); err == nil {
		t.Fatal("缺少 masterKey 应报错")
	}
	if _, err := OpenStore(p, []byte("wrong")); err == nil {
		t.Fatal("错误的 masterKey 应报错")
	}
	// 密文绑定用户名：把 alice 的密文挪到别的用户名下应解密失败
	b, _ = os.ReadFile(p)
	moved := bytes.Replace(b, []byte(`"name": "alice"`), []byte(`"name": "mallory"`), 1)
	p2 := filepath.Join(t.TempDir(), "moved.json")
	os.WriteFile(p2, moved, 0o600)
	if _, err := OpenStore(p2, key); err == nil {
		t.Fatal("密文被挪用应解密失败")
	}
}

func TestTOTPPlaintextModeAndMigration(t *testing.T) {
	s, p := newStore(t, nil) // masterKey 为空：明文并标记
	addUser(t, s, "alice", RoleDBA)
	secret := enrollTOTP(t, s, "alice", t0)
	b, _ := os.ReadFile(p)
	if !bytes.Contains(b, []byte(secret)) || !bytes.Contains(b, []byte(`"totpEnc": false`)) {
		t.Fatalf("明文模式应标记 totpEnc=false 且保存明文：\n%s", b)
	}
	// 之后提供 masterKey：能读取明文，下一次写入自动转为加密
	s2, err := OpenStore(p, []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := s2.Get("alice"); !u.HasTOTP {
		t.Fatal("明文记录应可读取")
	}
	if err := s2.SetDBs("alice", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("迁移后不应再有明文")
	}
}

func TestLastAdminProtection(t *testing.T) {
	s, _ := newStore(t, nil)
	addAdmin(t, s, "root1")
	addUser(t, s, "dba1", RoleDBA)
	if err := s.Delete("root1"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("删除最后一个管理员：%v", err)
	}
	if err := s.SetDisabled("root1", true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("禁用最后一个管理员：%v", err)
	}
	if err := s.SetRoles("root1", map[string][]Role{"*": {RoleViewer}}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("降级最后一个管理员：%v", err)
	}
	if err := s.SetRoles("root1", map[string][]Role{}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("清空角色：%v", err)
	}
	if u, _ := s.Get("root1"); u.Disabled || !u.HasRole("*", RoleAdmin) {
		t.Fatal("失败的操作不应改变状态")
	}
	// 仍可对非管理员操作；保留 admin 的同时改其他角色可以
	if err := s.SetRoles("root1", map[string][]Role{"*": {RoleAdmin, RoleAuditor}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("dba1"); err != nil {
		t.Fatal(err)
	}
	// 有第二个管理员后可以降级 / 禁用 / 删除其一，但永远剩一个
	addAdmin(t, s, "root2")
	if err := s.SetDisabled("root1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("root2"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("被禁用的管理员不计数：%v", err)
	}
	if err := s.SetDisabled("root1", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoles("root2", map[string][]Role{"*": {RoleViewer}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("root1"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("%v", err)
	}
	// 被禁用的管理员再次启用不受限
	if err := s.SetDisabled("root2", true); err != nil {
		t.Fatal(err)
	}
}

func TestStoreConcurrency(t *testing.T) {
	s, p := newStore(t, nil)
	addUser(t, s, "alice", RoleSRE)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "user" + string(rune('a'+i))
			if err := s.Add(name, name, pw, map[string][]Role{"*": {RoleViewer}}, nil); err != nil {
				t.Error(err)
			}
			s.Authenticate("alice", pw, "", "1.2.3.4", t0)
			s.List()
		}(i)
	}
	wg.Wait()
	s2, err := OpenStore(p, nil)
	if err != nil || len(s2.List()) != 9 {
		t.Fatalf("%v %d", err, len(s2.List()))
	}
}

func TestSeedDemoUsers(t *testing.T) {
	s, p := newStore(t, nil)
	if err := SeedDemoUsers(s); err != nil {
		t.Fatal(err)
	}
	want := map[string]Role{"admin": RoleAdmin, "admin2": RoleAdmin, "dba": RoleDBA, "sre": RoleSRE,
		"approver": RoleApprover, "approver2": RoleApprover, "viewer": RoleViewer, "auditor": RoleAuditor, "dev": RoleDeveloper}
	l := s.List()
	if len(l) != len(want) {
		t.Fatalf("演示账号数=%d", len(l))
	}
	for name, role := range want {
		u, ok := s.Get(name)
		if !ok || !u.HasRole("prod", role) {
			t.Errorf("%s 缺失或角色不对：%+v", name, u)
		}
		if _, err := s.Authenticate(name, "Tidbops-Demo-1", "", "", t0); err != nil {
			t.Errorf("%s 应可用演示口令登录：%v", name, err)
		}
	}
	if d, _ := s.Get("dev"); len(d.DBs) != 1 || d.DBs[0] != "shop" {
		t.Fatalf("dev.DBs=%v", d.DBs)
	}
	// 非空库不再播种
	if err := s.Delete("viewer"); err != nil {
		t.Fatal(err)
	}
	if err := SeedDemoUsers(s); err != nil || len(s.List()) != len(want)-1 {
		t.Fatalf("非空库不应播种：%v %d", err, len(s.List()))
	}
	s2, _ := OpenStore(p, nil)
	if err := SeedDemoUsers(s2); err != nil || len(s2.List()) != len(want)-1 {
		t.Fatal("重开后同样不播种")
	}
}
