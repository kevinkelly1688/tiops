// @author: kevinkelly
// @license: Apache License 2.0
// @file: expiry_test.go
// @time: 2026/10/28

package expiry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeadlineIs20300101Beijing(t *testing.T) {
	want := time.Date(2029, 12, 31, 16, 0, 0, 0, time.UTC) // 北京时间 2030-01-01 00:00
	if !Deadline.Equal(want) {
		t.Fatalf("Deadline = %v，期望 %v", Deadline, want)
	}
}

type logs struct {
	mu sync.Mutex
	ls []string
}

func (l *logs) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ls = append(l.ls, fmt.Sprintf(f, a...))
}

func (l *logs) n() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.ls) }

func newGuard(now *time.Time, l *logs) *Guard {
	return &Guard{Deadline: Deadline, Now: func() time.Time { return *now }, Logf: l.logf}
}

func TestBeforeDeadlineServesAndStaysQuiet(t *testing.T) {
	now := Deadline.Add(-time.Second)
	l := &logs{}
	g := newGuard(&now, l)
	called := false
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !called || rec.Code != 204 {
		t.Fatalf("到期前应正常处理：called=%v code=%d", called, rec.Code)
	}
	if l.n() != 0 {
		t.Fatalf("到期前不应打印日志：%v", l.ls)
	}
}

func TestAtAndAfterDeadlineIs503WithoutExplanation(t *testing.T) {
	for _, d := range []time.Duration{0, time.Hour, 24 * 365 * time.Hour} {
		now := Deadline.Add(d)
		l := &logs{}
		g := newGuard(&now, l)
		called := false
		h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
		for _, p := range []string{"/", "/index.html", "/api/health", "/api/auth/login", "/js/app.js"} {
			for _, m := range []string{"GET", "POST", "HEAD"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(m, p, nil))
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s %s（到期+%v）= %d，期望 503", m, p, d, rec.Code)
				}
				if rec.Body.Len() != 0 {
					t.Fatalf("到期响应不应带任何说明：%q", rec.Body.String())
				}
			}
		}
		if called {
			t.Fatal("到期后不应再调用后续处理器")
		}
		if l.n() != 1 {
			t.Fatalf("到期后日志应限频为 1 条，实际 %d：%v", l.n(), l.ls)
		}
		if !strings.Contains(l.ls[0], "使用期限已过") {
			t.Fatalf("日志内容不对：%q", l.ls[0])
		}
	}
}

func TestLogIsRateLimited(t *testing.T) {
	now := Deadline.Add(time.Second)
	l := &logs{}
	g := newGuard(&now, l)
	for i := 0; i < 5; i++ {
		g.Expired()
	}
	if l.n() != 1 {
		t.Fatalf("同一分钟内只应打印 1 条，实际 %d", l.n())
	}
	now = now.Add(logEvery)
	g.Expired()
	if l.n() != 2 {
		t.Fatalf("过了间隔应再打印 1 条，实际 %d", l.n())
	}
}

func TestStartup(t *testing.T) {
	now := Deadline.Add(-24 * time.Hour)
	l := &logs{}
	newGuard(&now, l).Startup()
	if l.n() != 1 || !strings.Contains(l.ls[0], "使用期限至") {
		t.Fatalf("到期前启动应记录期限：%v", l.ls)
	}
	now = Deadline.Add(time.Hour)
	l = &logs{}
	newGuard(&now, l).Startup()
	if l.n() != 1 || !strings.Contains(l.ls[0], "使用期限已过") {
		t.Fatalf("到期后启动应记录已过期：%v", l.ls)
	}
}

func TestNilLogfDoesNotPanic(t *testing.T) {
	now := Deadline.Add(time.Hour)
	g := &Guard{Deadline: Deadline, Now: func() time.Time { return now }}
	if !g.Expired() {
		t.Fatal("应判定为已到期")
	}
	g.Startup()
}
