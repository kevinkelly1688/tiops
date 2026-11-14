// @author: kevinkelly
// @license: Apache License 2.0
// @file: insight_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"tidbops/internal/model"
)

type cfgVal struct {
	Cluster   string            `json:"cluster"`
	Instances map[string]string `json:"instances"`
	Drift     bool              `json:"drift"`
	Majority  string            `json:"majority"`
	Norm      string            `json:"norm"`
}

func checkConfigValues(t *testing.T, out map[string]any) {
	t.Helper()
	b, _ := json.Marshal(out)
	var cv struct {
		Values map[string]map[string]cfgVal `json:"values"`
	}
	if err := json.Unmarshal(b, &cv); err != nil {
		t.Fatal(err)
	}
	pool := cv.Values["tikv"]["raftstore.store-pool-size"]
	if !pool.Drift || pool.Majority != "2" || pool.Instances["10.0.2.13:20160"] != "3" || pool.Cluster != "" {
		t.Errorf("tikv-3 的 store-pool-size 应漂移: %+v", pool)
	}
	if v := cv.Values["tikv"]["storage.block-cache.capacity"]; v.Norm != "16GB" || v.Drift || len(v.Instances) != 6 {
		t.Errorf("block-cache: %+v", v)
	}
	if v := cv.Values["pd"]["schedule.max-store-down-time"]; v.Cluster != "30m0s" || v.Norm != "30m" {
		t.Errorf("PD 参数应保留原值并给出 norm: %+v", v)
	}
	if v := cv.Values["tidb"]["tidb_mem_quota_query"]; v.Cluster != "1073741824" || v.Norm != "1GB" {
		t.Errorf("系统变量: %+v", v)
	}
	if len(cv.Values["ticdc"]) != 0 {
		t.Errorf("TiCDC 没有采集来源，应为空: %v", cv.Values["ticdc"])
	}
}

func checkForecast(t *testing.T, out map[string]any) {
	t.Helper()
	g, _ := out["growthGBPerDay"].(float64)
	d80, ok := out["daysTo80"].(float64)
	if out["enough"] != true || g < 100 || g > 140 || !ok || d80 < 20 || d80 > 35 || out["usedGB"].(float64) < 13000 || out["capGB"].(float64) < 21000 || out["regions"].(float64) < 40000 || out["basis"] == "" {
		t.Errorf("容量预测: %v", out)
	}
}

func checkReadOnlyInspect(t *testing.T, out map[string]any) {
	t.Helper()
	items, _ := out["items"].([]any)
	if len(items) < 10 || out["cluster"] != "prod-tidb-01" {
		t.Fatalf("巡检报告: %v", out)
	}
	var disk, host bool
	for _, it := range items {
		m := it.(map[string]any)
		if strings.Contains(m["n"].(string), "磁盘") && m["r"] == "bad" {
			disk = true
		}
		if m["g"] == "主机" {
			host = true
		}
	}
	if !disk {
		t.Error("巡检应发现 tikv-4 磁盘异常")
	}
	if host {
		t.Error("GET 巡检不含主机检查")
	}
	if out["bad"].(float64) < 1 {
		t.Errorf("汇总: %v", out)
	}
}

func TestReadOnlyInsights(t *testing.T) {
	ts, _ := setup(t)
	for _, path := range []string{"/api/config-values", "/api/forecast", "/api/inspect"} {
		code, body, h := get(t, ts.URL+path)
		if code != 200 || !strings.Contains(h.Get("Content-Type"), "application/json") || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %.200s", path, code, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		switch path {
		case "/api/config-values":
			checkConfigValues(t, out)
		case "/api/forecast":
			checkForecast(t, out)
		case "/api/inspect":
			checkReadOnlyInspect(t, out)
		}
	}
	// 只读后端仍然只接受 GET / HEAD
	resp, err := http.Post(ts.URL+"/api/inspect", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("只读后端的 POST /api/inspect 应 405: %d", resp.StatusCode)
	}
	if resp, err := http.Post(ts.URL+"/api/config-values", "application/json", strings.NewReader("{}")); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/config-values 应 405: %v", err)
	}
	// 快照：DB Time、集群事实、热点范围
	code, body, _ := get(t, ts.URL+"/api/snapshot")
	var snap model.Snapshot
	if code != 200 || json.Unmarshal([]byte(body), &snap) != nil {
		t.Fatalf("snapshot: %d", code)
	}
	if len(snap.DBTime.ByType) == 0 || len(snap.DBTime.ByStage) != 4 || len(snap.DBTime.ByExec) == 0 {
		t.Errorf("快照应带 DB Time: %+v", snap.DBTime)
	}
	if snap.Topology.Replicas != 3 || snap.Topology.AZCount != 3 || strings.Join(snap.Topology.LocationLabels, ",") != "zone,host" {
		t.Errorf("快照应带集群事实: %+v", snap.Topology)
	}
	if len(snap.Hot) == 0 || !strings.HasPrefix(snap.Hot[0].Range, "table_id=112 row 12345") {
		t.Errorf("热点 Region 范围: %+v", snap.Hot)
	}
	for _, want := range []string{`"dbTime":{"byType":[{"name":`, `"replicas":3`, `"azCount":3`, `"range":"table_id=112`} {
		if !strings.Contains(body, want) {
			t.Errorf("快照 JSON 缺少 %s", want)
		}
	}
	// ?refresh=1 不应出错
	if code, _, _ := get(t, ts.URL+"/api/config-values?refresh=1"); code != 200 {
		t.Errorf("refresh: %d", code)
	}
}

func TestOpsInsights(t *testing.T) {
	e := setupOps(t)
	// 运维模式下这些读接口要求登录
	anon := e.client(t)
	for _, p := range []string{"/api/config-values", "/api/forecast", "/api/inspect"} {
		if code, _ := anon.do("GET", p, nil); code != 401 {
			t.Errorf("%s 未登录应 401: %d", p, code)
		}
	}
	// 访客（最低权限）也能读：与总览页同一能力
	viewer := e.login(t, "viewer")
	code, out := viewer.do("GET", "/api/config-values", nil)
	if code != 200 {
		t.Fatalf("config-values: %d %v", code, out)
	}
	checkConfigValues(t, out)
	code, out = viewer.do("GET", "/api/forecast", nil)
	if code != 200 {
		t.Fatalf("forecast: %d %v", code, out)
	}
	checkForecast(t, out)
	code, out = viewer.do("GET", "/api/inspect", nil)
	if code != 200 {
		t.Fatalf("GET /api/inspect: %d %v", code, out)
	}
	checkReadOnlyInspect(t, out)

	// POST /api/inspect（含主机检查、需要 CapDiag）依然只在运维模式、且权限不变
	if code, _ := viewer.do("POST", "/api/inspect", map[string]any{}); code != 403 {
		t.Errorf("访客不能 POST 巡检: %d", code)
	}
	sre := e.login(t, "sre")
	code, out = sre.do("POST", "/api/inspect", map[string]any{"os": true})
	if code != 200 {
		t.Fatalf("POST /api/inspect: %d %v", code, out)
	}
	hasHost := false
	for _, it := range out["items"].([]any) {
		if it.(map[string]any)["g"] == "主机" {
			hasHost = true
		}
	}
	if !hasHost {
		t.Error("POST 巡检应包含主机检查")
	}
	// 同一个用户的 GET 巡检仍然不含主机检查
	code, out = sre.do("GET", "/api/inspect", nil)
	if code != 200 {
		t.Fatalf("sre GET /api/inspect: %d", code)
	}
	checkReadOnlyInspect(t, out)
}
