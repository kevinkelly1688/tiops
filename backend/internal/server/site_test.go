// @author: kevinkelly
// @license: Apache License 2.0
// @file: site_test.go
// @time: 2026/10/28

package server_test

import (
	"strings"
	"sync/atomic"
	"testing"

	"tidbops/internal/server"
)

// 备案号随 /api/health 下发（未登录也要能取到：登录页要显示）；没配置时不出现这个字段。
func TestHealthCarriesICP(t *testing.T) {
	e := setupOpsCfg(t, nil, func(c *server.Config) { c.ICP = "京ICP备12345678号-1" })
	code, out := e.client(t).do("GET", "/api/health", nil)
	if code != 200 || out["icp"] != "京ICP备12345678号-1" {
		t.Fatalf("health 应带 icp: %d %v", code, out)
	}

	e = setupOps(t)
	code, out = e.client(t).do("GET", "/api/health", nil)
	if code != 200 {
		t.Fatalf("health: %d %v", code, out)
	}
	if _, has := out["icp"]; has {
		t.Errorf("没配置备案号时 health 不应有 icp 字段: %v", out)
	}
}

// Unavailable 为 true 时写操作闸门一律拒绝（定时变更到点也不会开始执行），且报错不透露原因。
func TestUnavailableClosesWriteGateSilently(t *testing.T) {
	var down atomic.Bool
	e := setupOpsCfg(t, nil, func(c *server.Config) { c.Unavailable = down.Load })
	admin, admin2, dba := e.login(t, "admin"), e.login(t, "admin2"), e.login(t, "dba")
	admin.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	admin2.do("POST", "/api/ops/write-switch", map[string]any{"enabled": true})
	body := map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tidb-1", "action": "start"}}
	if code, out := dba.do("POST", "/api/changes", body); code != 200 {
		t.Fatalf("可用时低风险变更应可创建: %d %v", code, out)
	}

	down.Store(true)
	other := map[string]any{"kind": "inst.op", "params": map[string]any{"node": "tikv-2", "action": "start"}} // 换一个节点：tidb-1 已被上面那条变更占用
	code, out := dba.do("POST", "/api/changes", other)
	if code == 200 {
		t.Fatalf("不可用时必须拒绝创建变更: %v", out)
	}
	msg, _ := out["error"].(string)
	if msg != "服务不可用" {
		t.Errorf("应被写操作闸门拒绝（而不是别的检查）: %d %v", code, out)
	}
	for _, leak := range []string{"期限", "到期", "过期", "授权", "2030", "license", "expire"} {
		if strings.Contains(strings.ToLower(msg), leak) {
			t.Errorf("报错不应透露原因（含 %q）: %q", leak, msg)
		}
	}

	down.Store(false)
	if code, out := dba.do("POST", "/api/changes", other); code != 200 {
		t.Errorf("恢复后应可创建: %d %v", code, out)
	}
}
