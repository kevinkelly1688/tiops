// @author: kevinkelly
// @license: Apache License 2.0
// @file: expiry.go
// @time: 2026/10/28

// Package expiry 是平台的使用期限。
//
// 到期（Deadline，北京时间 2030-01-01 00:00）之后服务不可用：
//   - HTTP 层：所有请求（页面、静态资源、API）一律返回不带任何说明的 503；
//   - 写操作闸门：定时变更到点也不会再执行（见 server.Config.Unavailable）。
//
// 不向用户展示任何提示（页面、响应体、使用帮助、变更说明里都不出现），只在服务端日志里记录；
// 日志按间隔限频，避免请求量大时刷屏。
//
// 只作用于 tidbops-server 的服务进程；user / admin（安装、升级、回滚）等命令行入口不受影响，
// 以便到期后仍能换装新版本。
package expiry

import (
	"net/http"
	"sync"
	"time"
)

// Deadline 是使用期限：该时刻（含）之后服务不可用。
var Deadline = time.Date(2030, 1, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))

// logEvery 是到期后日志的最小间隔。
const logEvery = time.Minute

// Guard 判断是否到期，并在到期时（限频地）打印日志。
type Guard struct {
	Deadline time.Time
	Now      func() time.Time // nil 表示 time.Now
	Logf     func(format string, args ...any)

	mu   sync.Mutex
	last time.Time
}

// New 返回使用 Deadline 与系统时钟的 Guard。
func New(logf func(format string, args ...any)) *Guard {
	return &Guard{Deadline: Deadline, Logf: logf}
}

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Expired 报告是否已到期；已到期时按间隔打印日志。
func (g *Guard) Expired() bool {
	now := g.now()
	if now.Before(g.Deadline) {
		return false
	}
	g.mu.Lock()
	due := g.last.IsZero() || now.Sub(g.last) >= logEvery
	if due {
		g.last = now
	}
	g.mu.Unlock()
	if due && g.Logf != nil {
		g.Logf("使用期限已过（%s），服务不可用，已拒绝请求", g.Deadline.Format("2006-01-02 15:04:05 -0700"))
	}
	return true
}

// Startup 在服务启动时记录一行使用期限（到期前）或到期状态（到期后）。
func (g *Guard) Startup() {
	if g.Logf == nil {
		return
	}
	if g.Expired() {
		return // Expired 已经打印
	}
	g.Logf("使用期限至 %s", g.Deadline.Format("2006-01-02 15:04:05 -0700"))
}

// Wrap 包装最外层 HTTP 处理器：到期后所有请求返回 503，不带任何说明。
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.Expired() {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
