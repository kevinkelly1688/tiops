// @author: kevinkelly
// @license: Apache License 2.0
// @file: session.go
// @time: 2026/10/28

package authz

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// 会话默认超时（A3 §6）。
const (
	DefaultIdle = 30 * time.Minute
	DefaultAbs  = 8 * time.Hour
)

// Session 是对外可见的会话信息。
type Session struct {
	User     string
	Created  time.Time
	LastSeen time.Time
}

type sess struct {
	user     string
	csrfHash [sha256.Size]byte
	created  time.Time
	last     time.Time
}

// Sessions 是服务端会话表。服务端只保存会话令牌与 CSRF 令牌的 SHA-256，不保存明文。并发安全。
type Sessions struct {
	// Now 返回当前时间，可在测试里替换；并发使用前设置好。
	Now func() time.Time

	mu   sync.Mutex
	idle time.Duration
	abs  time.Duration
	m    map[string]*sess
}

// NewSessions 创建会话表；idle / abs 非正则取默认 30 分钟 / 8 小时。
func NewSessions(idle, abs time.Duration) *Sessions {
	if idle <= 0 {
		idle = DefaultIdle
	}
	if abs <= 0 {
		abs = DefaultAbs
	}
	return &Sessions{Now: time.Now, idle: idle, abs: abs, m: map[string]*sess{}}
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("authz: 无法读取系统随机数：" + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *Sessions) expired(e *sess, now time.Time) bool {
	return now.Sub(e.last) > s.idle || now.Sub(e.created) > s.abs
}

// Create 为用户创建会话，返回会话令牌与 CSRF 令牌（各 32 随机字节，base64url）。
func (s *Sessions) Create(user string) (token, csrf string) {
	token, csrf = randToken(), randToken()
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) > 1024 { // 顺手清理过期会话
		s.sweepLocked(now)
	}
	s.m[tokenKey(token)] = &sess{user: user, csrfHash: sha256.Sum256([]byte(csrf)), created: now, last: now}
	return token, csrf
}

// Get 取得会话。过期（空闲或绝对）则删除并返回 false；成功访问会刷新空闲时间。
func (s *Sessions) Get(token string, now time.Time) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	k := tokenKey(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[k]
	if !ok {
		return Session{}, false
	}
	if s.expired(e, now) {
		delete(s.m, k)
		return Session{}, false
	}
	e.last = now
	return Session{User: e.user, Created: e.created, LastSeen: e.last}, true
}

// Destroy 销毁会话（登出）。
func (s *Sessions) Destroy(token string) {
	s.mu.Lock()
	delete(s.m, tokenKey(token))
	s.mu.Unlock()
}

// RevokeUser 吊销某用户的全部会话（禁用 / 删除 / 改口令后调用），返回吊销数量。
func (s *Sessions) RevokeUser(name string) int {
	n := 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if strings.EqualFold(e.user, name) {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// CheckCSRF 以恒定时间校验 CSRF 令牌；会话不存在或已过期返回 false（不刷新空闲时间）。
func (s *Sessions) CheckCSRF(token, csrf string) bool {
	if token == "" || csrf == "" {
		return false
	}
	now := s.Now()
	s.mu.Lock()
	e, ok := s.m[tokenKey(token)]
	var want [sha256.Size]byte
	if ok && !s.expired(e, now) {
		want = e.csrfHash
	} else {
		ok = false
	}
	s.mu.Unlock()
	got := sha256.Sum256([]byte(csrf))
	// 无论会话是否有效都做一次比较，减少时间差异
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	return ok && match
}

// Rotate 轮换会话 ID（登录后、提权后调用）：销毁旧令牌并为同一用户签发新令牌。旧令牌无效则 ok=false。
func (s *Sessions) Rotate(old string, now time.Time) (token, csrf string, ok bool) {
	sn, ok := s.Get(old, now)
	if !ok {
		return "", "", false
	}
	s.Destroy(old)
	token, csrf = s.Create(sn.User)
	return token, csrf, true
}

// Sweep 清除所有已过期会话，返回清除数量。
func (s *Sessions) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepLocked(now)
}

func (s *Sessions) sweepLocked(now time.Time) int {
	n := 0
	for k, e := range s.m {
		if s.expired(e, now) {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// Len 返回当前会话数（含尚未清理的过期会话）。
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
