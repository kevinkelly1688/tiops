// @author: kevinkelly
// @license: Apache License 2.0
// @file: password_change_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"testing"

	"tidbops/internal/authz"
)

const newPW = "Xk9#mP2vL7qR-new"

// 自助改口令必须校验旧口令；账号被锁定时也不能因此跳过校验（旧写法的布尔优先级错误）。
func TestChangePasswordRequiresOldPassword(t *testing.T) {
	e := setupOps(t)
	dba := e.login(t, "dba")

	// 旧口令错误：403，口令不变
	if code, out := dba.do("POST", "/api/auth/password", map[string]string{"old": "wrong-old-password", "new": newPW}); code != 403 {
		t.Fatalf("旧口令错误应 403: %d %v", code, out)
	}

	// 别处连续输错口令把账号锁定（5 次）；持有会话的人不能借此免旧口令改密
	other := e.client(t)
	for i := 0; i < 5; i++ {
		other.do("POST", "/api/auth/login", map[string]string{"user": "dba", "password": "wrong-" + string(rune('a'+i))})
	}
	code, out := dba.do("POST", "/api/auth/password", map[string]string{"old": "still-wrong", "new": newPW})
	if code != 403 {
		t.Fatalf("账号被锁定时也必须拒绝（修复前这里会改密成功）: %d %v", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "登录失败次数过多") {
		t.Errorf("锁定时应提示稍后重试，而不是「口令不正确」: %v", out)
	}
	// 锁定期间连正确的旧口令也不放行：锁定就是锁定
	if code, _ := dba.do("POST", "/api/auth/password", map[string]string{"old": authz.DemoPassword, "new": newPW}); code != 403 {
		t.Errorf("锁定期间应一律拒绝: %d", code)
	}
}

func TestChangePasswordWithCorrectOld(t *testing.T) {
	e := setupOps(t)
	sre := e.login(t, "sre")
	if code, out := sre.do("POST", "/api/auth/password", map[string]string{"old": authz.DemoPassword, "new": newPW}); code != 200 {
		t.Fatalf("旧口令正确应能改: %d %v", code, out)
	}
	// 新口令能登录，旧口令不能
	c := e.client(t)
	if code, _ := c.do("POST", "/api/auth/login", map[string]string{"user": "sre", "password": newPW}); code != 200 {
		t.Errorf("新口令应能登录: %d", code)
	}
	if code, _ := e.client(t).do("POST", "/api/auth/login", map[string]string{"user": "sre", "password": authz.DemoPassword}); code == 200 {
		t.Error("旧口令不应再能登录")
	}
}
