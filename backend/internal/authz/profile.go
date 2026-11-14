// @author: kevinkelly
// @license: Apache License 2.0
// @file: profile.go
// @time: 2026/10/28

package authz

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 用户资料：显示名 / 部门 / 邮箱 / 手机 / 备注。口令与 TOTP 不在此列。

// 资料字段的长度限制（按字符数）。
const (
	MaxDisplayRunes = 40
	MaxDeptRunes    = 40
	MaxEmailLen     = 120
	MaxNoteRunes    = 200
)

// ErrInvalidProfile 是资料校验失败的错误基类（errors.Is 可判断），文案为中文可直接展示。
var ErrInvalidProfile = errors.New("用户资料不合法")

var (
	emailRe = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]+\.[^@\s]+$`)
	phoneRe = regexp.MustCompile(`^[0-9+\-() ]{3,30}$`)
)

// Profile 是用户资料。
type Profile struct {
	Display, Dept, Email, Phone, Note string
}

func profileErr(format string, a ...any) error {
	return fmt.Errorf("%w：%s", ErrInvalidProfile, fmt.Sprintf(format, a...))
}

// Normalize 去掉首尾空白并校验：显示名 / 部门 ≤40 字、邮箱 ≤120 且格式为 a@b.c（可空）、手机 3–30 位数字与 + - ( ) 空格（可空）、
// 备注 ≤200 字；除备注外不允许控制字符。Display 留空保持为空（调用方决定是否回退为用户名）。
func (p Profile) Normalize() (Profile, error) {
	p.Display, p.Dept, p.Email, p.Phone, p.Note = strings.TrimSpace(p.Display), strings.TrimSpace(p.Dept),
		strings.TrimSpace(p.Email), strings.TrimSpace(p.Phone), strings.TrimSpace(p.Note)
	if utf8.RuneCountInString(p.Display) > MaxDisplayRunes {
		return p, profileErr("显示名不能超过 %d 个字", MaxDisplayRunes)
	}
	if utf8.RuneCountInString(p.Dept) > MaxDeptRunes {
		return p, profileErr("部门不能超过 %d 个字", MaxDeptRunes)
	}
	if len(p.Email) > MaxEmailLen || (p.Email != "" && !emailRe.MatchString(p.Email)) {
		return p, profileErr("邮箱格式不正确（形如 name@example.com，不超过 %d 位）", MaxEmailLen)
	}
	if p.Phone != "" && !phoneRe.MatchString(p.Phone) {
		return p, profileErr("手机号格式不正确（3–30 位，只能含数字与 + - ( ) 空格）")
	}
	if utf8.RuneCountInString(p.Note) > MaxNoteRunes {
		return p, profileErr("备注不能超过 %d 个字", MaxNoteRunes)
	}
	if hasControl(p.Display, false) || hasControl(p.Dept, false) || hasControl(p.Email, false) || hasControl(p.Phone, false) || hasControl(p.Note, true) {
		return p, profileErr("不能包含控制字符")
	}
	return p, nil
}

// ProfileUpdate 是资料更新：nil 字段表示不修改。
type ProfileUpdate struct {
	Display, Dept, Email, Phone, Note *string
}

// AddWithProfile 创建用户并写入资料（p.Display 为空则取用户名）。其余同 Add。
func (s *Store) AddWithProfile(name, password string, roles map[string][]Role, dbs []string, p Profile) error {
	name = normName(name)
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	p, err := p.Normalize()
	if err != nil {
		return err
	}
	if err := CheckPassword(name, password); err != nil {
		return err
	}
	rl, err := validateRoles(roles)
	if err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[name]; ok {
		return ErrExists
	}
	if p.Display == "" {
		p.Display = name
	}
	recs := s.cloneRecs()
	recs[name] = &rec{Name: name, Display: p.Display, Dept: p.Dept, Email: p.Email, Phone: p.Phone, Note: p.Note,
		PassHash: hash, Roles: rl, DBs: append([]string{}, dbs...), Created: time.Now().UTC()}
	return s.commit(recs)
}

// UpdateProfile 更新资料，返回实际变化的字段名（display / dept / email / phone / note）与更新后的用户。
// Display 被改成空时回退为用户名。没有变化时不写盘。
func (s *Store) UpdateProfile(name string, up ProfileUpdate) (changed []string, u User, err error) {
	name = normName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.recs[name]
	if !ok {
		return nil, User{}, ErrNotFound
	}
	p := Profile{cur.Display, cur.Dept, cur.Email, cur.Phone, cur.Note}
	if up.Display != nil {
		p.Display = *up.Display
	}
	if up.Dept != nil {
		p.Dept = *up.Dept
	}
	if up.Email != nil {
		p.Email = *up.Email
	}
	if up.Phone != nil {
		p.Phone = *up.Phone
	}
	if up.Note != nil {
		p.Note = *up.Note
	}
	if p, err = p.Normalize(); err != nil {
		return nil, User{}, err
	}
	if p.Display == "" {
		p.Display = name
	}
	for _, f := range []struct {
		key      string
		old, new string
	}{{"display", cur.Display, p.Display}, {"dept", cur.Dept, p.Dept}, {"email", cur.Email, p.Email}, {"phone", cur.Phone, p.Phone}, {"note", cur.Note, p.Note}} {
		if f.old != f.new {
			changed = append(changed, f.key)
		}
	}
	if len(changed) == 0 {
		return nil, cur.user(), nil
	}
	recs := s.cloneRecs()
	r := recs[name]
	r.Display, r.Dept, r.Email, r.Phone, r.Note = p.Display, p.Dept, p.Email, p.Phone, p.Note
	if err := s.commit(recs); err != nil {
		return nil, User{}, err
	}
	return changed, r.user(), nil
}

// loginPersistEvery 是最近登录时间的写盘节流：距上次记录不足该间隔的登录只更新内存（下一次任何保存时一并落盘）。
const loginPersistEvery = 30 * time.Second

// RecordLogin 记录一次登录成功的时间。只应在登录成功时调用（不要在每次请求 / 校验口令时调用）。
// 写盘失败只返回错误，不影响内存状态。
func (s *Store) RecordLogin(name string, now time.Time) error {
	name = normName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[name]
	if !ok {
		return ErrNotFound
	}
	now = now.UTC()
	prev := r.LastLogin
	r.LastLogin = &now
	if prev != nil && now.Sub(*prev) >= 0 && now.Sub(*prev) < loginPersistEvery {
		return nil
	}
	return s.save(s.recs)
}

// LockedUntil 返回用户当前的登录锁定截止时间（仅用户名维度的登录失败锁定）；未锁定返回 false。
func (s *Store) LockedUntil(name string, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockedUntil(now, "u:"+normName(name))
}

// Unlock 清除用户的失败锁定与失败计数：登录失败（用户名维度）与动态码二次确认失败；不触碰 IP 维度的计数。
// 返回清除前该用户是否处于锁定中及其截止时间。用户不存在返回 ErrNotFound。
func (s *Store) Unlock(name string, now time.Time) (wasLocked bool, until time.Time, err error) {
	name = normName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[name]; !ok {
		return false, time.Time{}, ErrNotFound
	}
	until, wasLocked = s.lockedUntil(now, "u:"+name, "o:"+name)
	s.clearFailure("u:" + name)
	s.clearFailure("o:" + name)
	return wasLocked, until, nil
}
