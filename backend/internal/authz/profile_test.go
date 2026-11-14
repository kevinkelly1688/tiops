// @author: kevinkelly
// @license: Apache License 2.0
// @file: profile_test.go
// @time: 2026/10/28

package authz

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sp(s string) *string { return &s }

// 旧版 users.json（没有 dept / email / phone / note / lastLogin 字段）可以正常加载，且之后保存不丢数据。
func TestOldUsersFileLoadsWithoutProfileFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.json")
	h, _ := HashPassword(pw)
	old := `{"version":1,"users":[{"name":"olduser","display":"老用户","passHash":"` + h + `","roles":{"*":["viewer"]},"dbs":[],"disabled":false,"created":"2026-01-02T03:04:05Z","totpEnc":false}]}`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := s.Get("olduser")
	if !ok || u.Display != "老用户" || u.Dept != "" || u.Email != "" || u.Phone != "" || u.Note != "" || u.LastLogin != nil {
		t.Fatalf("旧文件加载结果不对：%+v", u)
	}
	if _, err := s.Authenticate("olduser", pw, "", "", t0); err != nil {
		t.Fatalf("旧用户应能登录：%v", err)
	}
	if _, _, err := s.UpdateProfile("olduser", ProfileUpdate{Dept: sp("平台部")}); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := s2.Get("olduser"); u.Dept != "平台部" || u.Display != "老用户" {
		t.Fatalf("保存后重新加载：%+v", u)
	}
}

func TestAddWithProfileAndValidation(t *testing.T) {
	s, p := newStore(t, nil)
	prof := Profile{Display: " 张三 ", Dept: "数据库组", Email: "zs@example.com", Phone: "+86 138-0000-0000", Note: "值班\n备注"}
	if err := s.AddWithProfile("zs", pw, map[string][]Role{"*": {RoleViewer}}, nil, prof); err != nil {
		t.Fatal(err)
	}
	u, _ := s.Get("zs")
	if u.Display != "张三" || u.Dept != "数据库组" || u.Email != "zs@example.com" || u.Phone != "+86 138-0000-0000" || u.Note != "值班\n备注" {
		t.Fatalf("%+v", u)
	}
	s2, _ := OpenStore(p, nil)
	if u2, _ := s2.Get("zs"); u2.Dept != "数据库组" || u2.Email != "zs@example.com" {
		t.Fatal("资料应落盘")
	}
	if err := s.AddWithProfile("ls", pw, nil, nil, Profile{}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get("ls"); u.Display != "ls" {
		t.Fatalf("显示名留空应回退为用户名：%q", u.Display)
	}
	for name, bad := range map[string]Profile{
		"邮箱缺@":   {Email: "abc"},
		"邮箱缺域名点": {Email: "a@b"},
		"邮箱含空格":  {Email: "a b@c.com"},
		"邮箱过长":   {Email: strings.Repeat("a", 118) + "@b.cc"},
		"手机含字母":  {Phone: "13800x"},
		"手机过短":   {Phone: "12"},
		"部门过长":   {Dept: strings.Repeat("部", 41)},
		"备注过长":   {Note: strings.Repeat("注", 201)},
		"显示名过长":  {Display: strings.Repeat("名", 41)},
		"部门含控制符": {Dept: "a\x01b"},
	} {
		err := s.AddWithProfile("bad"+strings.Repeat("x", 1), pw, nil, nil, bad)
		if !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("%s 应返回 ErrInvalidProfile：%v", name, err)
		}
	}
	if _, ok := s.Get("badx"); ok {
		t.Fatal("非法资料不应创建用户")
	}
	if err := s.AddWithProfile("ok2", pw, nil, nil, Profile{Note: strings.Repeat("注", 200), Dept: strings.Repeat("部", 40)}); err != nil {
		t.Fatalf("边界值应合法：%v", err)
	}
}

func TestUpdateProfile(t *testing.T) {
	s, _ := newStore(t, nil)
	if err := s.AddWithProfile("amy", pw, nil, nil, Profile{Display: "Amy", Dept: "A", Email: "amy@x.com"}); err != nil {
		t.Fatal(err)
	}
	changed, u, err := s.UpdateProfile("AMY", ProfileUpdate{Dept: sp("B"), Phone: sp("123456"), Display: sp("Amy")})
	if err != nil || strings.Join(changed, ",") != "dept,phone" || u.Dept != "B" || u.Email != "amy@x.com" {
		t.Fatalf("%v %v %+v", err, changed, u)
	}
	changed, _, err = s.UpdateProfile("amy", ProfileUpdate{})
	if err != nil || len(changed) != 0 {
		t.Fatal("空更新应无变化")
	}
	if _, _, err := s.UpdateProfile("amy", ProfileUpdate{Email: sp("bad")}); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("%v", err)
	}
	if u, _ := s.Get("amy"); u.Email != "amy@x.com" {
		t.Fatal("校验失败不应修改")
	}
	if _, u, err := s.UpdateProfile("amy", ProfileUpdate{Email: sp(""), Display: sp("")}); err != nil || u.Email != "" || u.Display != "amy" {
		t.Fatalf("清空邮箱 / 显示名回退：%v %+v", err, u)
	}
	if _, _, err := s.UpdateProfile("ghost", ProfileUpdate{Dept: sp("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatal("不存在的用户")
	}
}

func TestRecordLoginThrottle(t *testing.T) {
	s, p := newStore(t, nil)
	addAdmin(t, s, "alice")
	if u, _ := s.Get("alice"); u.LastLogin != nil {
		t.Fatal("从未登录应为 nil")
	}
	if err := s.RecordLogin("alice", t0); err != nil {
		t.Fatal(err)
	}
	disk := func() *time.Time {
		s2, err := OpenStore(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := s2.Get("alice")
		return u.LastLogin
	}
	if d := disk(); d == nil || !d.Equal(t0) {
		t.Fatalf("首次登录应落盘：%v", d)
	}
	st1, _ := os.Stat(p)
	time.Sleep(15 * time.Millisecond)
	// 30 秒内再次登录：只更新内存，不写盘
	if err := s.RecordLogin("alice", t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("节流窗口内不应写盘")
	}
	if u, _ := s.Get("alice"); !u.LastLogin.Equal(t0.Add(10 * time.Second)) {
		t.Fatal("内存里应是最新时间")
	}
	if d := disk(); !d.Equal(t0) {
		t.Fatal("磁盘仍是上次落盘的值")
	}
	// 超过窗口：落盘
	if err := s.RecordLogin("alice", t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d := disk(); !d.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("应落盘：%v", d)
	}
	// 读取 / 校验不会写盘、不会改最近登录时间
	st3, _ := os.Stat(p)
	for i := 0; i < 3; i++ {
		_, _ = s.Get("alice")
		_ = s.List()
	}
	st4, _ := os.Stat(p)
	if !st3.ModTime().Equal(st4.ModTime()) {
		t.Fatal("读操作不应写盘")
	}
	if err := s.RecordLogin("ghost", t0); !errors.Is(err, ErrNotFound) {
		t.Fatal("不存在的用户")
	}
}

func TestUnlockClearsUserDimensionOnly(t *testing.T) {
	s, _ := newStore(t, nil)
	addAdmin(t, s, "alice")
	addAdmin(t, s, "bob")
	ip := "10.0.0.9"
	now := t0
	for i := 0; i < maxFails; i++ {
		_, err := s.Authenticate("alice", "wrong-password-1", "", ip, now)
		if !errors.Is(err, ErrInvalidCredentials) && !errors.Is(err, ErrLocked) {
			t.Fatal(err)
		}
	}
	until, locked := s.LockedUntil("alice", now)
	if !locked || !until.After(now) {
		t.Fatal("alice 应已被锁定")
	}
	if _, locked := s.LockedUntil("bob", now); locked {
		t.Fatal("bob 不应被锁定")
	}
	if _, err := s.Authenticate("alice", pw, "", "", now); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定期间正确口令也应被拒绝：%v", err)
	}
	was, u2, err := s.Unlock("alice", now)
	if err != nil || !was || !u2.Equal(until) {
		t.Fatalf("Unlock：%v %v %v", was, u2, err)
	}
	if _, locked := s.LockedUntil("alice", now); locked {
		t.Fatal("解锁后不应再锁定")
	}
	// IP 维度没有被清除：同一 IP 仍处于锁定
	s.mu.Lock()
	ipst := s.fails["i:"+ip]
	ipLocked := ipst != nil && now.Before(ipst.lockedUntil)
	s.mu.Unlock()
	if !ipLocked {
		t.Fatal("Unlock 不应清除 IP 维度的计数")
	}
	// 不带 IP 的登录成功
	if _, err := s.Authenticate("alice", pw, "", "", now); err != nil {
		t.Fatalf("解锁后应能登录：%v", err)
	}
	// 再次解锁：当时未锁定
	was, _, err = s.Unlock("alice", now)
	if err != nil || was {
		t.Fatal("未锁定时 wasLocked 应为 false")
	}
	if _, _, err := s.Unlock("ghost", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("不存在的用户")
	}
}
