// @author: kevinkelly
// @license: Apache License 2.0
// @file: wizard_test.go
// @time: 2026/10/28

package server_test

import (
	"encoding/json"
	"strings"
	"testing"
)

type wizResp struct {
	Evidence map[string]struct {
		Answer string `json:"answer"`
		Why    string `json:"why"`
		Ref    string `json:"ref"`
	} `json:"evidence"`
	Facts []struct {
		ID, Level, Text string
	} `json:"facts"`
	Mode string `json:"mode"`
}

func checkWizardReplay(t *testing.T, body string, wantSlow bool) {
	t.Helper()
	var r wizResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("响应不是 JSON: %v %s", err, body)
	}
	// 回放快照里 tikv-2 写流量是其余节点的 6 倍、Raftstore CPU 87%，并有 Raftstore CPU 严重告警
	if e, ok := r.Evidence["写入变慢/w1"]; !ok || e.Answer != "yes" || !strings.Contains(e.Why, "tikv-2") || e.Ref != "hotspot" {
		t.Errorf("w1 应为 yes 且点名 tikv-2: %+v", e)
	}
	if e, ok := r.Evidence["TiKV server is busy/b3"]; !ok || e.Answer != "yes" || !strings.Contains(e.Why, "Raftstore") {
		t.Errorf("b3 应为 yes（Raftstore CPU 告警）: %+v", e)
	}
	// 快照里没有切换历史：只能给 unknown
	if e, ok := r.Evidence["延迟升高/l2"]; !ok || e.Answer != "unknown" || e.Why == "" {
		t.Errorf("l2 应为 unknown: %+v", e)
	}
	// Compile 占比 17%：既不算「很高」也不算「很低」
	if e, ok := r.Evidence["延迟升高/l6"]; !ok || e.Answer != "unknown" || !strings.Contains(e.Why, "Compile") {
		t.Errorf("l6: %+v", e)
	}
	// 没有数据可判断的问题不返回
	for _, k := range []string{"写入变慢/w3", "写入变慢/w4", "TiKV server is busy/b2", "延迟升高/l1", "延迟升高/l5", "写入变慢/w2", "TiKV server is busy/b1"} {
		if _, ok := r.Evidence[k]; ok {
			t.Errorf("%s 不能由快照回答，不应返回", k)
		}
	}
	for k, e := range r.Evidence {
		if e.Answer != "yes" && e.Answer != "no" && e.Answer != "unknown" || e.Why == "" {
			t.Errorf("%s 的答案 / 依据不合法: %+v", k, e)
		}
		if !strings.Contains(k, "/") {
			t.Errorf("key 应为「症状/节点id」: %s", k)
		}
	}
	// 实时线索带具体数据
	facts := map[string]string{}
	for _, f := range r.Facts {
		facts[f.ID] = f.Text
	}
	if !strings.Contains(facts["disk-high"], "tikv-4") || !strings.Contains(facts["disk-high"], "87%") {
		t.Errorf("磁盘线索应点名 tikv-4 87%%: %q", facts["disk-high"])
	}
	if !strings.Contains(facts["tidb-mem"], "tidb-3") {
		t.Errorf("内存线索: %q", facts["tidb-mem"])
	}
	if !strings.Contains(facts["inst-down"], "16") || (wantSlow && facts["slow-fullscan"] == "") || (!wantSlow && facts["slow-fullscan"] != "") {
		t.Errorf("线索: %v", facts)
	}
}

func TestWizardEvidenceReadOnlyMode(t *testing.T) {
	ts, _ := setup(t) // 只读模式：无登录
	code, body, hdr := get(t, ts.URL+"/api/wizard/evidence")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("只读模式应可访问: %d %s", code, body)
	}
	checkWizardReplay(t, body, true)
}

func TestWizardEvidenceOpsMode(t *testing.T) {
	e := setupPolicy(t)
	if code, _ := e.client(t).do("GET", "/api/wizard/evidence", nil); code != 401 {
		t.Errorf("运维模式未登录应 401: %d", code)
	}
	for _, u := range []string{"viewer", "dev", "auditor", "dba"} { // 所有角色都有 view.overview
		c := e.login(t, u)
		code, out := c.do("GET", "/api/wizard/evidence", nil)
		if code != 200 {
			t.Fatalf("%s: %d %v", u, code, out)
		}
		b, _ := json.Marshal(out)
		checkWizardReplay(t, string(b), u == "dba" || u == "dev") // viewer / auditor 没有 SQL 查看权限，看不到慢 SQL 线索；dev 只能看到被授权库的
	}
	// 只读方法：POST 不存在
	if code, _ := e.login(t, "viewer").do("POST", "/api/wizard/evidence", map[string]any{}); code == 200 {
		t.Errorf("POST 不应成功: %d", code)
	}
}
