// @author: kevinkelly
// @license: Apache License 2.0
// @file: hub_routes_test.go
// @time: 2026/10/28

package server_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"tidbops/internal/server"
)

// 端点分类表（与 hub.go 顶部的端点分类说明一致）。新增路由时必须在这里登记，并决定它是全局端点还是集群端点；
// 本测试扫描 internal/server 下全部 mux 注册，发现未登记的路由就失败，避免新端点悄悄落入错误的分类。
var routeClass = map[string]string{
	// ---- 全局端点：始终由主集群实例处理，与 X-Cluster 无关（health / auth/me 例外：已登录且指定集群时返回该集群的值）
	"/api/health": "global", "/api/auth/login": "global", "/api/auth/logout": "global", "/api/auth/me": "global", "/api/auth/password": "global",
	"/api/auth/totp/begin": "global", "/api/auth/totp/confirm": "global",
	"/api/audit": "global", "/api/audit/verify": "global", "/api/audit/export": "global",
	"/api/users": "global", "/api/users/x/roles": "global", "/api/users/x/disable": "global", "/api/users/x/password": "global", "/api/users/x/delete": "global",
	"/api/users/x/totp-reset": "global", "/api/users/x/profile": "global", "/api/users/x/unlock": "global",
	"/api/roles": "global", "/api/roles/x": "global", "/api/roles/x/delete": "global",
	"/api/ops/version": "global", "/api/ops/proposals": "global", "/api/ops/proposals/x/confirm": "global", "/api/ops/proposals/x/reject": "global", "/api/ops/policy": "global",
	"/api/clusters": "global", "/api/clusters/x": "global", "/api/clusters/x/delete": "global", "/api/clusters/x/test": "global",
	"/api/clusters/discover": "global", "/api/clusters/import": "global",
	"/api/watermark":     "global",
	"/api/deploy/status": "global", "/api/deploy/configs": "global", "/api/deploy/precheck": "global", "/api/deploy/jobs": "global", "/api/deploy/jobs/x": "global",
	"/api/deploy/jobs/x/cancel": "global", "/api/deploy/jobs/x/download": "global", "/api/deploy/jobs/x/secret": "global",
	"/api/deploy/requests": "global", "/api/deploy/requests/x/approve": "global", "/api/deploy/requests/x/start": "global",
	"/api/shell/status": "global", "/api/shell/check": "global", "/api/shell/run": "global", "/api/shell/requests": "global", "/api/shell/requests/x/approve": "global",
	"/api/shell/jobs": "global", "/api/shell/jobs/x": "global", "/api/shell/jobs/x/download": "global", "/api/shell/jobs/x/cancel": "global", "/api/shell/settings": "global",

	// ---- 集群端点：按 X-Cluster 路由到该集群的实例
	"/api/snapshot": "cluster", "/api/topology": "cluster", "/api/overview": "cluster", "/api/slow": "cluster", "/api/hot": "cluster", "/api/alerts": "cluster",
	"/api/findings": "cluster", "/api/env": "cluster", "/api/params": "cluster", "/api/config-values": "cluster", "/api/forecast": "cluster", "/api/hosts": "cluster",
	"/api/inspect": "cluster", "/api/inspect/last": "cluster", "/api/inspect/report": "cluster", "/api/wizard/evidence": "cluster",
	"/api/changes": "cluster", "/api/changes/preview": "cluster", "/api/changes/recommend": "cluster", "/api/changes/x": "cluster",
	"/api/changes/x/approve": "cluster", "/api/changes/x/reject": "cluster", "/api/changes/x/cancel": "cluster", "/api/changes/x/abort": "cluster",
	"/api/changes/x/confirm": "cluster", "/api/changes/x/review": "cluster",
	"/api/maints": "cluster", "/api/maints/x/todo": "cluster",
	"/api/hostpool": "cluster", "/api/hostpool/x": "cluster", "/api/hostpool/x/delete": "cluster", "/api/hostpool/x/check": "cluster",
	"/api/ops/settings": "cluster", "/api/ops/write-switch": "cluster", "/api/ops/write-switch/cancel": "cluster", "/api/ops/selfcheck": "cluster",
	"/api/ops/read-cmd": "cluster", "/api/alerts/silence": "cluster", "/api/timeline": "cluster",
	"/api/ops/notify": "cluster", "/api/ops/notify/channels": "cluster", "/api/ops/notify/channels/x": "cluster", "/api/ops/notify/channels/x/enabled": "cluster",
	"/api/ops/notify/channels/x/delete": "cluster", "/api/ops/notify/channels/x/test": "cluster",
	"/api/lb": "cluster", "/api/ops/lb": "cluster", "/api/ops/lb/x": "cluster", "/api/ops/lb/x/delete": "cluster",
}

var (
	reRoute = regexp.MustCompile(`HandleFunc\("(?:(?:GET|POST) )?(/api/[^"]*)"`)
	rePart  = regexp.MustCompile(`part\("([a-z-]+)"`)
	reParam = regexp.MustCompile(`\{[^}]*\}`)
)

func TestHubRouteClassificationCoversEveryRoute(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reRoute.FindAllStringSubmatch(string(b), -1) {
			p := reParam.ReplaceAllString(m[1], "x")
			if p == "/api/" { // 兜底 404
				continue
			}
			found[p] = true
		}
		for _, m := range rePart.FindAllStringSubmatch(string(b), -1) {
			found["/api/"+m[1]] = true
		}
	}
	var unknown []string
	for p := range found {
		cls, ok := routeClass[p]
		if !ok {
			unknown = append(unknown, p)
			continue
		}
		if got := server.IsGlobalAPIForTest(p); got != (cls == "global") {
			t.Errorf("%s 在分类表里是 %s，但 isGlobalAPI=%v", p, cls, got)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("以下路由没有在 routeClass 与 docs 第 12 节登记分类（全局 / 集群）：%v", unknown)
	}
	for p := range routeClass {
		if !found[p] {
			t.Errorf("分类表里的 %s 在源码里找不到对应的路由（已删除？）", p)
		}
	}
	if len(found) < 60 {
		t.Errorf("只扫描到 %d 条路由，扫描可能失效", len(found))
	}
}
