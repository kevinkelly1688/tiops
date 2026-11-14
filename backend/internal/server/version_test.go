// @author: kevinkelly
// @license: Apache License 2.0
// @file: version_test.go
// @time: 2026/10/28

package server_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tidbops/internal/dataver"
	"tidbops/internal/server"
)

func TestVersionAPI(t *testing.T) {
	e := setupOps(t)

	// 未登录：401
	if code, _ := e.client(t).do("GET", "/api/ops/version", nil); code != 401 {
		t.Fatalf("未登录应返回 401，实际 %d", code)
	}

	// 数据目录里放 VERSION 与 upgrades.jsonl（含坏行、空行，共 12 条有效记录）
	if err := os.WriteFile(filepath.Join(e.dir, "VERSION"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&sb, `{"time":"2026-10-%02dT10:00:00Z","action":"upgrade","from":"1.0.%d","to":"1.0.%d","ok":true}`+"\n", i, i-1, i)
		if i == 5 {
			sb.WriteString("{这不是 JSON\n\n")
		}
	}
	sb.WriteString("[1,2,3]\n") // 合法 JSON 但不是对象：同样跳过
	if err := os.WriteFile(filepath.Join(e.dir, "upgrades.jsonl"), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	c := e.login(t, "dba")
	code, out := c.do("GET", "/api/ops/version", nil)
	if code != 200 {
		t.Fatalf("登录后应返回 200，实际 %d：%v", code, out)
	}
	if out["version"] != server.Version {
		t.Errorf("version = %v，期望 %s", out["version"], server.Version)
	}
	if out["goVersion"] != runtime.Version() || out["os"] != runtime.GOOS || out["arch"] != runtime.GOARCH {
		t.Errorf("运行环境字段不对：%v", out)
	}
	if int(out["schema"].(float64)) != dataver.Schema {
		t.Errorf("schema = %v", out["schema"])
	}
	if int(out["dataSchema"].(float64)) != 1 {
		t.Errorf("dataSchema = %v，期望 1", out["dataSchema"])
	}
	if out["startedAt"] == nil || out["uptimeSec"] == nil {
		t.Errorf("缺少 startedAt / uptimeSec：%v", out)
	}
	for _, k := range []string{"commit", "buildTime"} {
		if _, ok := out[k]; !ok {
			t.Errorf("缺少字段 %s", k)
		}
	}
	lay, ok := out["installLayout"].(map[string]any)
	if !ok {
		t.Fatalf("installLayout 缺失：%v", out)
	}
	// 测试二进制不在 releases/<版本>/bin 下：未安装
	if lay["installed"] != false || lay["current"] != "" {
		t.Errorf("测试进程不应被识别为已安装：%v", lay)
	}
	if rel, ok := lay["releases"].([]any); !ok || len(rel) != 0 {
		t.Errorf("releases 应为空数组：%v", lay["releases"])
	}
	hist, ok := lay["history"].([]any)
	if !ok || len(hist) != 10 {
		t.Fatalf("history 应有 10 条（坏行跳过、取最近 10 条），实际 %v", lay["history"])
	}
	first := hist[0].(map[string]any)
	if first["to"] != "1.0.12" {
		t.Errorf("history 应新的在前，第一条 to=%v", first["to"])
	}
	if last := hist[9].(map[string]any); last["to"] != "1.0.3" {
		t.Errorf("history 最后一条 to=%v，期望 1.0.3", last["to"])
	}
}

// 数据目录里没有 VERSION / upgrades.jsonl：全部容错为空。
func TestVersionAPIEmptyData(t *testing.T) {
	e := setupOps(t)
	c := e.login(t, "dba")
	code, out := c.do("GET", "/api/ops/version", nil)
	if code != 200 {
		t.Fatalf("code=%d %v", code, out)
	}
	if int(out["dataSchema"].(float64)) != 0 {
		t.Errorf("dataSchema 应为 0：%v", out["dataSchema"])
	}
	lay := out["installLayout"].(map[string]any)
	if h, ok := lay["history"].([]any); !ok || len(h) != 0 {
		t.Errorf("history 应为空数组：%v", lay["history"])
	}
}

func TestHealthHasBuildInfo(t *testing.T) {
	e := setupOps(t)
	code, out := e.client(t).do("GET", "/api/health", nil)
	if code != 200 || out["version"] != server.Version {
		t.Fatalf("health: %d %v", code, out)
	}
	for _, k := range []string{"commit", "buildTime"} {
		if _, ok := out[k]; !ok {
			t.Errorf("health 缺少 %s", k)
		}
	}
}
