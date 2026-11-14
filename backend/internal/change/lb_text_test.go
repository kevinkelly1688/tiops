// @author: kevinkelly
// @license: Apache License 2.0
// @file: lb_text_test.go
// @time: 2026/10/28

package change

import (
	"strings"
	"testing"
)

// 负载均衡人工步骤的文案：没有手工登记时与登记功能引入前逐字一致；有登记时点名全部入口。

func manualOf(t *testing.T, p Plan, name string) string {
	t.Helper()
	for _, s := range p.Steps {
		if s.Name == name {
			return s.Manual
		}
	}
	t.Fatalf("计划里没有步骤 %q: %+v", name, p.Steps)
	return ""
}

func TestLBRemoveTextScaleIn(t *testing.T) {
	v := newEnv(t, "")
	params := js(scaleInParams{Nodes: []string{"tidb-3"}})

	// 开关打开、没有登记：旧文案
	p, err := (Ctx{Cluster: "prod", Topo: v.topo, LB: true}).Plan(KindScaleIn, params)
	if err != nil {
		t.Fatal(err)
	}
	want := "把 10.0.1.13:4000 从负载均衡后端摘除，并等待连接排空，确认后继续"
	if got := manualOf(t, p, "从负载均衡摘除并等待连接排空"); got != want {
		t.Errorf("无登记时文案应与旧版一致:\n got %q\nwant %q", got, want)
	}

	// 没有 LB 也没有登记：不加摘除步骤
	p, _ = (Ctx{Cluster: "prod", Topo: v.topo}).Plan(KindScaleIn, params)
	for _, s := range p.Steps {
		if strings.Contains(s.Name, "负载均衡") {
			t.Errorf("没有负载均衡时不应出现摘除步骤: %+v", s)
		}
	}

	// 一条登记
	c := Ctx{Cluster: "prod", Topo: v.topo, LB: true, LBs: []string{"haproxy-prod（HAProxy，10.0.0.5:3306）"}}
	p, _ = c.Plan(KindScaleIn, params)
	want = "先从负载均衡 haproxy-prod（HAProxy，10.0.0.5:3306） 摘除 10.0.1.13:4000，并等待连接排空，确认后继续"
	if got := manualOf(t, p, "从负载均衡摘除并等待连接排空"); got != want {
		t.Errorf("有登记时应点名入口:\n got %q\nwant %q", got, want)
	}

	// 多条登记：全部列出
	c.LBs = []string{"haproxy-prod（HAProxy，10.0.0.5:3306）", "slb-a（云 SLB，slb.example.com:3306）"}
	p, _ = c.Plan(KindScaleIn, params)
	got := manualOf(t, p, "从负载均衡摘除并等待连接排空")
	if !strings.Contains(got, "haproxy-prod") || !strings.Contains(got, "slb-a") {
		t.Errorf("多条登记应全部列出: %q", got)
	}
}

func TestLBTextMaintEnterAndExit(t *testing.T) {
	v := newEnv(t, "")
	enter := js(maintEnterParams{IP: "10.0.1.12", Type: "reboot", Mins: 10})

	p, err := (Ctx{Cluster: "prod", Topo: v.topo, LB: true}).Plan(KindMaintEnter, enter)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := manualOf(t, p, "从负载均衡摘除并排空连接"), "把 10.0.1.12:4000 从负载均衡后端摘除，并等待连接排空，确认后继续"; got != want {
		t.Errorf("维护摘除（无登记）:\n got %q\nwant %q", got, want)
	}
	c := Ctx{Cluster: "prod", Topo: v.topo, LB: true, LBs: []string{"lvs-1（LVS，10.0.0.9:3306）"}}
	p, _ = c.Plan(KindMaintEnter, enter)
	if got, want := manualOf(t, p, "从负载均衡摘除并排空连接"), "先从负载均衡 lvs-1（LVS，10.0.0.9:3306） 摘除 10.0.1.12:4000，并等待连接排空，确认后继续"; got != want {
		t.Errorf("维护摘除（有登记）:\n got %q\nwant %q", got, want)
	}
}

func TestLBTextHelpers(t *testing.T) {
	if got := (Ctx{}).lbAddBackText([]string{"a:1", "b:2"}); got != "把 a:1、b:2 加回负载均衡后端并确认流量恢复" {
		t.Errorf("无登记的加回文案应与旧版一致: %q", got)
	}
	if got := (Ctx{LBs: []string{"x（Nginx，h:1）"}}).lbAddBackText([]string{"a:1"}); got != "把 a:1 加回负载均衡 x（Nginx，h:1） 的后端并确认流量恢复" {
		t.Errorf("有登记的加回文案: %q", got)
	}
}
