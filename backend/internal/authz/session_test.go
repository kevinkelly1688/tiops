// @author: kevinkelly
// @license: Apache License 2.0
// @file: session_test.go
// @time: 2026/10/28

package authz

import (
	"encoding/base64"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newSess(now time.Time) (*Sessions, *time.Time) {
	cur := now
	s := NewSessions(30*time.Minute, 8*time.Hour)
	s.Now = func() time.Time { return cur }
	return s, &cur
}

func TestSessionTokens(t *testing.T) {
	s, _ := newSess(t0)
	tok, csrf := s.Create("alice")
	tok2, csrf2 := s.Create("alice")
	if tok == tok2 || csrf == csrf2 || tok == csrf {
		t.Fatal("令牌应随机且互不相同")
	}
	for _, v := range []string{tok, csrf} {
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(b) != 32 {
			t.Fatalf("令牌应为 32 字节 base64url：%q", v)
		}
	}
	sn, ok := s.Get(tok, t0)
	if !ok || sn.User != "alice" {
		t.Fatalf("%+v %v", sn, ok)
	}
	if _, ok := s.Get("", t0); ok {
		t.Fatal("空令牌")
	}
	if _, ok := s.Get("garbage", t0); ok {
		t.Fatal("未知令牌")
	}
}

func TestSessionStoresOnlyHashes(t *testing.T) {
	s, _ := newSess(t0)
	tok, csrf := s.Create("alice")
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if k == tok || strings.Contains(k, tok) {
			t.Fatal("服务端不应保存会话令牌明文")
		}
		if len(k) != 64 {
			t.Fatalf("键应为 SHA-256 十六进制：%q", k)
		}
		if string(e.csrfHash[:]) == csrf {
			t.Fatal("不应保存 CSRF 明文")
		}
	}
	// 结构里不应有任何 string 字段能装下明文令牌
	rt := reflect.TypeOf(sess{})
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Name != "user" && rt.Field(i).Type.Kind() == reflect.String {
			t.Fatalf("意外的字符串字段 %s", rt.Field(i).Name)
		}
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	s, cur := newSess(t0)
	tok, _ := s.Create("alice")
	// 每 20 分钟访问一次：空闲时间被刷新，不会过期
	for i := 1; i <= 5; i++ {
		*cur = t0.Add(time.Duration(i) * 20 * time.Minute)
		if _, ok := s.Get(tok, *cur); !ok {
			t.Fatalf("第 %d 次访问不应过期", i)
		}
	}
	// 空闲超过 30 分钟：过期并被删除
	if _, ok := s.Get(tok, cur.Add(31*time.Minute)); ok {
		t.Fatal("空闲 31 分钟应过期")
	}
	if s.Len() != 0 {
		t.Fatal("过期会话应被删除")
	}
	if _, ok := s.Get(tok, *cur); ok {
		t.Fatal("已删除的会话不能复活")
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	s, _ := newSess(t0)
	tok, _ := s.Create("alice")
	// 一直活跃，但超过 8 小时绝对期限
	for m := 20; m < 8*60; m += 20 {
		if _, ok := s.Get(tok, t0.Add(time.Duration(m)*time.Minute)); !ok {
			t.Fatalf("%d 分钟时不应过期", m)
		}
	}
	if _, ok := s.Get(tok, t0.Add(8*time.Hour+time.Minute)); ok {
		t.Fatal("超过绝对期限应过期")
	}
}

func TestSessionDefaults(t *testing.T) {
	s := NewSessions(0, 0)
	if s.idle != 30*time.Minute || s.abs != 8*time.Hour {
		t.Fatalf("默认值不对：%v %v", s.idle, s.abs)
	}
}

func TestSessionCSRF(t *testing.T) {
	s, cur := newSess(t0)
	tok, csrf := s.Create("alice")
	if !s.CheckCSRF(tok, csrf) {
		t.Fatal("正确的 CSRF 应通过")
	}
	_, otherCSRF := s.Create("bob")
	for _, c := range [][2]string{{tok, ""}, {tok, "wrong"}, {tok, otherCSRF}, {"", csrf}, {"nope", csrf}, {tok, csrf + "x"}} {
		if s.CheckCSRF(c[0], c[1]) {
			t.Errorf("不应通过：%q %q", c[0], c[1])
		}
	}
	*cur = t0.Add(time.Hour)
	if s.CheckCSRF(tok, csrf) {
		t.Fatal("过期会话的 CSRF 不应通过")
	}
}

func TestSessionDestroyRevokeRotate(t *testing.T) {
	s, _ := newSess(t0)
	a1, c1 := s.Create("alice")
	a2, _ := s.Create("Alice")
	b1, _ := s.Create("bob")
	s.Destroy(a1)
	if _, ok := s.Get(a1, t0); ok || s.CheckCSRF(a1, c1) {
		t.Fatal("Destroy 后应失效")
	}
	if n := s.RevokeUser("alice"); n != 1 {
		t.Fatalf("吊销数=%d", n)
	}
	if _, ok := s.Get(a2, t0); ok {
		t.Fatal("RevokeUser 后应失效")
	}
	if _, ok := s.Get(b1, t0); !ok {
		t.Fatal("不应影响其他用户")
	}
	nt, nc, ok := s.Rotate(b1, t0)
	if !ok || nt == b1 || nc == "" {
		t.Fatal("Rotate 应签发新令牌")
	}
	if _, ok := s.Get(b1, t0); ok {
		t.Fatal("旧令牌应失效")
	}
	if sn, ok := s.Get(nt, t0); !ok || sn.User != "bob" {
		t.Fatal("新令牌应有效")
	}
	if _, _, ok := s.Rotate("bogus", t0); ok {
		t.Fatal("无效令牌不能轮换")
	}
}

func TestSessionSweepAndConcurrency(t *testing.T) {
	s, _ := newSess(t0)
	var wg sync.WaitGroup
	toks := make(chan string, 200)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				tok, csrf := s.Create("u")
				s.Get(tok, t0)
				s.CheckCSRF(tok, csrf)
				toks <- tok
			}
		}()
	}
	wg.Wait()
	close(toks)
	if s.Len() != 200 {
		t.Fatalf("会话数=%d", s.Len())
	}
	if n := s.Sweep(t0.Add(time.Hour)); n != 200 || s.Len() != 0 {
		t.Fatalf("Sweep=%d Len=%d", n, s.Len())
	}
}
