// @author: kevinkelly
// @license: Apache License 2.0
// @file: change_test.go
// @time: 2026/10/28

package change

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"tidbops/internal/exec"
	"tidbops/internal/model"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func testTopo() model.Topology {
	t := model.Topology{Cluster: "prod", Version: "v8.5.2", User: "tidb"}
	for i := 1; i <= 3; i++ {
		t.Instances = append(t.Instances, model.Instance{ID: fmt.Sprintf("tidb-%d", i), Type: "tidb", Host: fmt.Sprintf("10.0.1.%d", 10+i), Port: 4000, StatusPort: 10080, AZ: fmt.Sprintf("az%d", i), Status: "up", Conn: 100})
		t.Instances = append(t.Instances, model.Instance{ID: fmt.Sprintf("pd-%d", i), Type: "pd", Host: fmt.Sprintf("10.0.1.%d", 20+i), Port: 2379, AZ: fmt.Sprintf("az%d", i), Status: "up", Leader: i == 1})
	}
	for i := 1; i <= 6; i++ {
		t.Instances = append(t.Instances, model.Instance{ID: fmt.Sprintf("tikv-%d", i), Type: "tikv", Host: fmt.Sprintf("10.0.2.%d", 10+i), Port: 20160, StatusPort: 20180, AZ: fmt.Sprintf("az%d", (i-1)%3+1),
			Status: "up", SID: uint64(i), Disk: 40, UsedGB: 1000, CapacityGB: 3000, Leaders: 5000, Regions: 15000})
	}
	return t
}

func execTopo(t model.Topology) exec.Topo {
	et := exec.Topo{Version: t.Version, PD: "http://10.0.1.21:2379", Nodes: map[string]string{}, Status: map[string]string{}}
	for _, in := range t.Instances {
		a := fmt.Sprintf("%s:%d", in.Host, in.Port)
		et.Nodes[a] = in.Type
		et.Status[statusAddr(in)] = a
	}
	return et
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	e      *Engine
	sim    *exec.SimRunner
	clk    *clock
	events []Event
	mu     sync.Mutex
	levels int
	mfa    bool
	gate   error
	topo   model.Topology
}

func newEnv(t *testing.T, dir string) *env {
	t.Helper()
	v := &env{clk: &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}, levels: 1, topo: testTopo()}
	pol := exec.Policy{Clusters: []string{"prod"}, TempDir: t.TempDir(), BackupRoots: []string{"local:///backup/"}}
	v.sim = &exec.SimRunner{Pol: pol, Topo: exec.StaticTopo(execTopo(v.topo)), Key: testKey, Now: v.clk.Now}
	e, err := New(Config{
		Cluster: "prod", Env: "staging", Dir: dir, Runner: v.sim, Key: testKey, Prober: &SimProber{Polls: 1},
		Policy: func(risk, env string, emergency bool) (int, bool) {
			if risk == "low" {
				return 0, false
			}
			return v.levels, v.mfa
		},
		Gate: func(string) error { return v.gate },
		Base: func(ctx context.Context) (Ctx, error) {
			return Ctx{Topo: v.topo, Sim: true, TopologyYAML: SynthTopologyYAML(v.topo), TopologySynthetic: true}, nil
		},
		Events: func(ev Event) { v.mu.Lock(); v.events = append(v.events, ev); v.mu.Unlock() },
		Now:    v.clk.Now, WaitScale: 0.0005, TickEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	v.e = e
	t.Cleanup(e.Close)
	return v
}

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (v *env) create(t *testing.T, kind string, params any, who string) Change {
	t.Helper()
	ch, err := v.e.Create(context.Background(), CreateInput{Kind: kind, Params: js(params), Applicant: who})
	if err != nil {
		t.Fatalf("创建 %s 失败: %v", kind, err)
	}
	return ch
}

func (v *env) waitState(t *testing.T, id string, want ...string) Change {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ch, _ := v.e.Get(id)
		for _, w := range want {
			if ch.State == w {
				return ch
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	ch, _ := v.e.Get(id)
	t.Fatalf("变更 %s 没有进入 %v，当前 %s（%s）\n%s", id, want, ch.State, ch.Reason, strings.Join(ch.Log, "\n"))
	return ch
}

func kinds(rs []exec.Request) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Cmd.Kind)
	}
	return out
}

func TestPlanRestartTiKV(t *testing.T) {
	v := newEnv(t, "")
	p, err := Ctx{Cluster: "prod", Topo: v.topo}.Plan(KindInstOp, js(instOpParams{Node: "tikv-2", Action: "restart"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked || p.Risk != "mid" || len(p.Steps) != 3 {
		t.Fatalf("计划不对: %+v", p)
	}
	if p.Steps[0].Cmds[0].Kind != exec.KindPDEvictAdd || p.Steps[0].Cmds[0].Store != 2 || p.Steps[1].Cmds[0].Nodes[0] != "10.0.2.12:20160" || p.Steps[2].Cmds[0].Kind != exec.KindPDEvictRemove {
		t.Errorf("TiKV 重启应为 驱逐 → 重启 → 恢复: %+v", p.Steps)
	}
	if len(p.Rollback) != 1 || len(p.Display) < 3 || !strings.Contains(p.Display[1], "tiup cluster restart prod -N 10.0.2.12:20160") {
		t.Errorf("回滚 / 展示命令不对: %+v %v", p.Rollback, p.Display)
	}
	if _, err := (Ctx{Cluster: "prod", Topo: v.topo}).Plan(KindInstOp, js(instOpParams{Node: "tikv-99", Action: "restart"})); err == nil {
		t.Error("不存在的实例应报错")
	}
	if _, err := (Ctx{Cluster: "prod", Topo: v.topo}).Plan(KindInstOp, js(instOpParams{Node: "tidb-1", Action: "evict"})); err == nil {
		t.Error("非 TiKV 不能驱逐 Leader")
	}
}

func TestScaleInBlockedAndOK(t *testing.T) {
	v := newEnv(t, "")
	c := Ctx{Cluster: "prod", Topo: v.topo}
	p, _ := c.Plan(KindScaleIn, js(scaleInParams{Nodes: []string{"tikv-1", "tikv-2", "tikv-3", "tikv-4"}}))
	if !p.Blocked {
		t.Errorf("剩余 2 个 TiKV 应被阻断: %+v", p.Checks)
	}
	p, _ = c.Plan(KindScaleIn, js(scaleInParams{Nodes: []string{"tikv-6"}}))
	if p.Blocked {
		t.Fatalf("缩 1 个 TiKV 应可行: %+v", p.Checks)
	}
	var ks []string
	for _, s := range p.Steps {
		for _, cm := range s.Cmds {
			ks = append(ks, cm.Kind)
		}
	}
	want := []string{exec.KindPDEvictAdd, exec.KindClusterScaleIn, exec.KindClusterPrune, exec.KindClusterDisplay}
	if strings.Join(ks, ",") != strings.Join(want, ",") {
		t.Errorf("缩容命令序列 = %v，期望 %v", ks, want)
	}
	p, _ = c.Plan(KindScaleIn, js(scaleInParams{Nodes: []string{"pd-1", "pd-2"}}))
	if !p.Blocked {
		t.Error("PD 缩到 1 个应被阻断")
	}
}

func TestScaleOutYAMLAndValidation(t *testing.T) {
	v := newEnv(t, "")
	c := Ctx{Cluster: "prod", Topo: v.topo}
	p, err := c.Plan(KindScaleOut, js(scaleOutParams{Role: "tikv", Hosts: []HostSpec{{IP: "10.0.2.17", AZ: "az1", Rack: "r1"}}, Cache: 16, Rate: 30}))
	if err != nil || p.Blocked {
		t.Fatalf("%v %+v", err, p.Checks)
	}
	var content string
	for _, s := range p.Steps {
		for _, cm := range s.Cmds {
			if cm.Kind == exec.KindClusterScaleOut {
				content = cm.Content
			}
		}
	}
	var doc map[string][]map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil || len(doc["tikv_servers"]) != 1 {
		t.Fatalf("生成的 YAML 不对: %v\n%s", err, content)
	}
	if doc["tikv_servers"][0]["host"] != "10.0.2.17" {
		t.Errorf("host 不对: %v", doc)
	}
	// 端口冲突
	p, _ = c.Plan(KindScaleOut, js(scaleOutParams{Role: "tikv", Hosts: []HostSpec{{IP: "10.0.2.11", AZ: "az1"}}}))
	if !p.Blocked {
		t.Error("10.0.2.11:20160 已被占用应阻断")
	}
	// 非法 IP
	p, _ = c.Plan(KindScaleOut, js(scaleOutParams{Role: "tidb", Hosts: []HostSpec{{IP: "not-an-ip"}}}))
	if !p.Blocked {
		t.Error("非法 IP 应阻断")
	}
	// 模拟执行器按白名单校验生成的拓扑
	req := exec.Request{Cmd: exec.Cmd{Kind: exec.KindClusterScaleOut, Cluster: "prod", Content: content}, ChangeID: "x"}
	req.Token = exec.Sign(testKey, req.Cmd, "x", v.clk.Now().Add(time.Minute))
	if res, err := v.sim.Run(context.Background(), req, nil); err != nil || res.Exit != 0 {
		t.Errorf("exec 层应接受平台生成的扩容拓扑: %v %+v", err, res)
	}
}

func TestPatchTopology(t *testing.T) {
	src := SynthTopologyYAML(testTopo())
	n := testTopo().Instances[len(testTopo().Instances)-1] // tikv-6
	out, err := PatchTopology(src, "tikv", "instance", n, map[string]any{"raftstore.raft-max-inflight-msgs": "1024"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tikv []struct {
			Host   string         `yaml:"host"`
			Config map[string]any `yaml:"config"`
		} `yaml:"tikv_servers"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, s := range doc.Tikv {
		if s.Config != nil {
			found++
			if s.Host != "10.0.2.16" || fmt.Sprint(s.Config["raftstore.raft-max-inflight-msgs"]) != "1024" {
				t.Errorf("补丁写错了实例: %+v", s)
			}
		}
	}
	if found != 1 {
		t.Errorf("只应有 1 个实例被修改，实际 %d\n%s", found, out)
	}
	out, err = PatchTopology(src, "tikv", "role", n, map[string]any{"log.level": "warn"})
	if err != nil || !strings.Contains(out, "log.level: \"warn\"") {
		t.Errorf("role 级补丁失败: %v\n%s", err, out)
	}
	if _, err := PatchTopology("", "tikv", "role", n, map[string]any{"a": "b"}); err == nil {
		t.Error("没有拓扑应报错")
	}
}

func TestConfigPlan(t *testing.T) {
	v := newEnv(t, "")
	c := Ctx{Cluster: "prod", Topo: v.topo, TopologyYAML: SynthTopologyYAML(v.topo), TopologySynthetic: true, Sim: true}
	p, err := c.Plan(KindConfig, js(configParams{Node: "tikv-3", Scope: "instance", Changes: []cfgChange{{Key: "storage.block-cache.capacity", Value: "12gb"}, {Key: "raftstore.raft-max-inflight-msgs", Value: "1024"}}}))
	if err != nil || p.Blocked {
		t.Fatalf("%v %+v", err, p.Checks)
	}
	var ks []string
	for _, s := range p.Steps {
		for _, cm := range s.Cmds {
			ks = append(ks, cm.Kind)
			if cm.Kind == exec.KindSQLSetConfig && (cm.Instance != "10.0.2.13:20180" || cm.Value != "12GB") {
				t.Errorf("SET CONFIG 参数不对: %+v", cm)
			}
		}
	}
	want := []string{exec.KindSQLSetConfig, exec.KindClusterEditCfg, exec.KindClusterReload, exec.KindClusterDisplay}
	if strings.Join(ks, ",") != strings.Join(want, ",") {
		t.Errorf("配置变更命令序列 = %v，期望 %v", ks, want)
	}
	if len(p.Rollback) != 1 {
		t.Errorf("应保存变更前拓扑用于回滚")
	}
	// 非白名单配置项
	if _, err := c.Plan(KindConfig, js(configParams{Node: "tikv-3", Changes: []cfgChange{{Key: "server.addr", Value: "1.1.1.1:1"}}})); err == nil {
		t.Error("目录之外的配置项必须拒绝")
	}
	// 值注入
	if _, err := c.Plan(KindConfig, js(configParams{Node: "tikv-3", Changes: []cfgChange{{Key: "log.level", Value: "info'; DROP"}}})); err == nil {
		t.Error("非法取值必须拒绝")
	}
	// 副本数大于 TiKV 数
	p, _ = c.Plan(KindConfig, js(configParams{Node: "pd-1", Scope: "role", Changes: []cfgChange{{Key: "replication.max-replicas", Value: "7"}}}))
	if !p.Blocked {
		t.Error("副本数 7 > TiKV 6 应阻断")
	}
	// 真实环境读不到 meta.yaml 时必须拒绝
	c.Sim = false
	p, _ = c.Plan(KindConfig, js(configParams{Node: "tikv-3", Changes: []cfgChange{{Key: "raftstore.raft-max-inflight-msgs", Value: "1024"}}}))
	if !p.Blocked {
		t.Error("没有真实拓扑时不能生成持久化配置")
	}
}

func TestLowRiskRunsImmediately(t *testing.T) {
	v := newEnv(t, "")
	ch := v.create(t, KindInstOp, instOpParams{Node: "tidb-1", Action: "start"}, "dba")
	if ch.Need != 0 {
		t.Fatalf("低风险应免审批: %+v", ch)
	}
	ch = v.waitState(t, ch.ID, StateSucceeded, StateFailed)
	if ch.State != StateSucceeded {
		t.Fatalf("应成功: %s\n%s", ch.Reason, strings.Join(ch.Log, "\n"))
	}
	if got := kinds(v.sim.Calls()); len(got) != 1 || got[0] != exec.KindClusterStart {
		t.Errorf("执行的命令 = %v", got)
	}
	for _, s := range ch.Steps {
		if s.State != "done" {
			t.Errorf("步骤状态: %+v", ch.Steps)
		}
	}
}

func TestApprovalFlowAndSegregation(t *testing.T) {
	v := newEnv(t, "")
	ch := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "restart"}, "alice")
	if ch.State != StatePending || ch.Need != 1 {
		t.Fatalf("应待审批: %+v", ch)
	}
	if _, err := v.e.Approve(ch.ID, "alice", ""); err == nil {
		t.Error("申请人不能审批自己")
	}
	if _, err := v.e.Approve(ch.ID, "bob", "ok"); err != nil {
		t.Fatal(err)
	}
	ch = v.waitState(t, ch.ID, StateSucceeded, StateFailed)
	if ch.State != StateSucceeded {
		t.Fatalf("%s\n%s", ch.Reason, strings.Join(ch.Log, "\n"))
	}
	got := strings.Join(kinds(v.sim.Calls()), ",")
	if got != "pd.evict-add,cluster.restart,pd.evict-remove" {
		t.Errorf("命令顺序 = %s", got)
	}
	// 已完成不能再审批
	if _, err := v.e.Approve(ch.ID, "carol", ""); err == nil {
		t.Error("已完成的变更不能审批")
	}
}

func TestTwoLevelApprovalNeedsDistinctPeople(t *testing.T) {
	v := newEnv(t, "")
	v.levels = 2
	ch := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "stop"}, "alice")
	if _, err := v.e.Approve(ch.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Approve(ch.ID, "bob", ""); err == nil {
		t.Error("同一人不能重复审批")
	}
	got, _ := v.e.Get(ch.ID)
	if got.State != StatePending {
		t.Fatalf("一级审批后仍应待审批: %s", got.State)
	}
	if _, err := v.e.Approve(ch.ID, "carol", ""); err != nil {
		t.Fatal(err)
	}
	v.waitState(t, ch.ID, StateSucceeded)
}

func TestRejectCancelExpire(t *testing.T) {
	v := newEnv(t, "")
	a := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "restart"}, "alice")
	if _, err := v.e.Reject(a.ID, "alice", "x"); err == nil {
		t.Error("申请人不能驳回自己的变更")
	}
	if got, err := v.e.Reject(a.ID, "bob", "风险太高"); err != nil || got.State != StateRejected {
		t.Fatalf("驳回失败: %v %+v", err, got)
	}
	b := v.create(t, KindInstOp, instOpParams{Node: "tikv-3", Action: "restart"}, "alice")
	if got, err := v.e.Cancel(b.ID, "alice", "不做了"); err != nil || got.State != StateCancelled {
		t.Fatalf("取消失败: %v", err)
	}
	c := v.create(t, KindInstOp, instOpParams{Node: "tikv-4", Action: "restart"}, "alice")
	v.clk.Add(25 * time.Hour)
	v.e.Tick()
	if got, _ := v.e.Get(c.ID); got.State != StateExpired {
		t.Errorf("审批超时应作废: %s", got.State)
	}
	if _, err := v.e.Approve(c.ID, "bob", ""); err == nil {
		t.Error("过期后不能审批")
	}
}

func TestFailureRunsRollback(t *testing.T) {
	v := newEnv(t, "")
	v.levels = 0
	v.sim.FailKind = exec.KindClusterRestart
	ch := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "restart"}, "alice")
	ch = v.waitState(t, ch.ID, StateFailed, StateSucceeded)
	if ch.State != StateFailed {
		t.Fatalf("应失败: %s", ch.State)
	}
	got := kinds(v.sim.Calls())
	if len(got) < 3 || got[len(got)-1] != exec.KindPDEvictRemove {
		t.Errorf("失败后应尝试恢复 Leader 调度: %v", got)
	}
}

func TestIgnoreErrStep(t *testing.T) {
	v := newEnv(t, "")
	v.levels = 0
	v.sim.FailKind = exec.KindPDEvictRemove
	ch := v.create(t, KindInstOp, instOpParams{Node: "tikv-1", Action: "start"}, "alice")
	ch = v.waitState(t, ch.ID, StateSucceeded, StateFailed)
	if ch.State != StateSucceeded {
		t.Errorf("可忽略的失败不应让变更失败: %s %s", ch.State, ch.Reason)
	}
}

type stuckProber struct{ n int }

func (s *stuckProber) Probe(ctx context.Context, w Wait) (bool, string, error) {
	return false, "还没好", nil
}

func TestAbortRunning(t *testing.T) {
	v := newEnv(t, "")
	v.levels = 0
	v.e.cfg.Prober = &stuckProber{}
	v.e.cfg.WaitScale = 1
	ch := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "restart"}, "alice")
	v.waitState(t, ch.ID, StateRunning)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := v.e.Abort(ch.ID, "alice"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("中止失败")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ch = v.waitState(t, ch.ID, StateAborted)
	got := kinds(v.sim.Calls())
	if got[len(got)-1] != exec.KindPDEvictRemove {
		t.Errorf("中止后应恢复 Leader 调度: %v", got)
	}
}

func TestManualStepNeedsConfirm(t *testing.T) {
	v := newEnv(t, "")
	v.levels = 0
	v.e.cfg.WaitScale = 1
	ch := v.create(t, KindScaleIn, scaleInParams{Nodes: []string{"tidb-3"}, LB: "haproxy"}, "alice")
	cur := v.waitState(t, ch.ID, StateRunning)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _ = v.e.Get(ch.ID)
		if cur.Steps[0].State == "manual" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没有进入人工步骤: %+v", cur.Steps)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(v.sim.Calls()) != 0 {
		t.Error("人工确认前不应执行命令")
	}
	if err := v.e.ConfirmStep(ch.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	cur = v.waitState(t, ch.ID, StateSucceeded, StateFailed)
	if cur.State != StateSucceeded {
		t.Fatalf("%s\n%s", cur.Reason, strings.Join(cur.Log, "\n"))
	}
}

func TestConflictBlocksSecondChange(t *testing.T) {
	v := newEnv(t, "")
	a := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "restart"}, "alice")
	_ = a
	_, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "stop"}), Applicant: "bob"})
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("同一实例的第二个变更应被阻断: %v", err)
	}
}

func TestGateMFAFreeze(t *testing.T) {
	v := newEnv(t, "")
	v.gate = errors.New("写操作开关未开启")
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "restart"}), Applicant: "a"}); err == nil || !strings.Contains(err.Error(), "开关") {
		t.Errorf("写开关关闭时应拒绝: %v", err)
	}
	v.gate = nil
	v.mfa = true
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "restart"}), Applicant: "a"}); !errors.Is(err, ErrNeedMFA) {
		t.Errorf("需要 MFA: %v", err)
	}
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "restart"}), Applicant: "a", MFAVerified: true}); err != nil {
		t.Errorf("MFA 通过后应可创建: %v", err)
	}
	v.mfa = false
	v.e.cfg.Freezes = func() []Freeze {
		return []Freeze{{From: v.clk.Now().Add(-time.Hour), To: v.clk.Now().Add(48 * time.Hour), Label: "国庆保障"}}
	}
	_, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-3", Action: "restart"}), Applicant: "a"})
	if err == nil || !strings.Contains(err.Error(), "国庆保障") {
		t.Errorf("冻结期内应拒绝: %v", err)
	}
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-3", Action: "restart"}), Applicant: "a", Emergency: true}); err != nil {
		t.Errorf("紧急变更可进入冻结期: %v", err)
	}
}

func TestScheduledRunsAtTime(t *testing.T) {
	v := newEnv(t, "")
	at := v.clk.Now().Add(2 * time.Hour)
	ch, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "restart"}), Applicant: "alice", ScheduledAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Approve(ch.ID, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.e.Get(ch.ID); got.State != StateScheduled {
		t.Fatalf("批准后应为已排期: %s", got.State)
	}
	v.e.Tick()
	if got, _ := v.e.Get(ch.ID); got.State != StateScheduled {
		t.Fatal("未到时间不应执行")
	}
	// 同一时间窗口的另一个同资源变更冲突
	at2 := at.Add(10 * time.Minute)
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindInstOp, Params: js(instOpParams{Node: "tikv-2", Action: "stop"}), Applicant: "x", ScheduledAt: &at2}); err == nil {
		t.Error("同资源同时间窗口应冲突")
	}
	v.clk.Add(2*time.Hour + time.Minute)
	v.e.Tick()
	v.waitState(t, ch.ID, StateSucceeded)
}

func TestMaintLifecycle(t *testing.T) {
	v := newEnv(t, "")
	v.levels = 0
	ch := v.create(t, KindMaintEnter, maintEnterParams{IP: "10.0.2.11", Type: "kernel", Mins: 20}, "dba")
	ch = v.waitState(t, ch.ID, StateSucceeded, StateFailed)
	if ch.State != StateSucceeded {
		t.Fatalf("%s\n%s", ch.Reason, strings.Join(ch.Log, "\n"))
	}
	ms := v.e.Maints()
	if len(ms) != 1 || ms[0].State != "active" || ms[0].Strategy != "standard" || !ms[0].HasTiKV {
		t.Fatalf("维护记录不对: %+v", ms)
	}
	// 同主机不能重复维护；另一台 TiKV 主机也不行
	p, _ := (Ctx{Cluster: "prod", Topo: v.topo, Maints: []*Maint{&ms[0]}}).Plan(KindMaintEnter, js(maintEnterParams{IP: "10.0.2.12", Type: "reboot", Mins: 10}))
	if !p.Blocked {
		t.Error("同一时刻只允许一台 TiKV 主机维护")
	}
	if err := v.e.SetTodo(ms[0].ID, 0, true); err != nil {
		t.Fatal(err)
	}
	// 检查单没勾完，正常结束被阻断，紧急可以
	if _, err := v.e.Create(context.Background(), CreateInput{Kind: KindMaintExit, Params: js(maintExitParams{MaintID: ms[0].ID}), Applicant: "dba"}); err == nil {
		t.Error("检查单未完成时应阻断正常结束")
	}
	// 延长窗口
	ext := v.create(t, KindMaintExtend, maintExtendParams{MaintID: ms[0].ID, Add: 30}, "dba")
	v.waitState(t, ext.ID, StateSucceeded)
	if m := v.e.Maints()[0]; m.ExtendedBy != 30 || m.Limit < 50 {
		t.Errorf("延长后记录不对: %+v", m)
	}
	ex := v.create(t, KindMaintExit, maintExitParams{MaintID: ms[0].ID, Urgent: true}, "dba")
	ex = v.waitState(t, ex.ID, StateSucceeded, StateFailed)
	if ex.State != StateSucceeded {
		t.Fatalf("%s\n%s", ex.Reason, strings.Join(ex.Log, "\n"))
	}
	if m := v.e.Maints()[0]; m.State != "done" || m.Ended == nil {
		t.Errorf("结束后维护应为 done: %+v", m)
	}
}

func TestMaintStrategyExtendsWhenLong(t *testing.T) {
	v := newEnv(t, "")
	p, err := (Ctx{Cluster: "prod", Topo: v.topo, PDDownMin: 30}).Plan(KindMaintEnter, js(maintEnterParams{IP: "10.0.2.11", Type: "hardware", Mins: 120}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Maint.Strategy != "extend" || p.Maint.Limit != 150 {
		t.Errorf("长维护应临时调大阈值: %+v", p.Maint)
	}
	if p.Steps[0].Cmds[0].Kind != exec.KindPDConfigSet || p.Steps[0].Cmds[0].Value != "150m" {
		t.Errorf("第一步应是调大阈值: %+v", p.Steps[0])
	}
	p, _ = (Ctx{Cluster: "prod", Topo: v.topo, PDDownMin: 30}).Plan(KindMaintEnter, js(maintEnterParams{IP: "10.0.2.11", Type: "hardware", Mins: 120, Strategy: "standard"}))
	if !p.Blocked {
		t.Error("强制标准策略但时长超过阈值应阻断")
	}
	keep := false
	p, _ = (Ctx{Cluster: "prod", Topo: v.topo}).Plan(KindMaintEnter, js(maintEnterParams{IP: "10.0.2.11", Type: "disk", Mins: 20, KeepData: &keep}))
	if !p.Blocked {
		t.Error("会丢数据的维护应阻断")
	}
}

func TestPersistenceAndRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	v := newEnv(t, dir)
	v.levels = 0
	v.e.cfg.Prober = &stuckProber{}
	v.e.cfg.WaitScale = 1
	ch := v.create(t, KindInstOp, instOpParams{Node: "tikv-2", Action: "restart"}, "alice")
	v.waitState(t, ch.ID, StateRunning)
	pending := v.create(t, KindInstOp, instOpParams{Node: "tikv-5", Action: "stop"}, "alice")
	_ = pending
	v.e.Close()
	v2 := newEnv(t, dir)
	got, ok := v2.e.Get(ch.ID)
	if !ok || got.State != StateFailed || !strings.Contains(got.Reason, "中断") {
		t.Errorf("重启后执行中的变更应标记为失败: %+v", got)
	}
	if len(v2.e.List()) != 2 {
		t.Errorf("应恢复 2 条变更: %d", len(v2.e.List()))
	}
	nxt := v2.create(t, KindInstOp, instOpParams{Node: "tikv-6", Action: "start"}, "alice")
	if nxt.ID == ch.ID || nxt.ID == pending.ID {
		t.Errorf("序号应延续: %s", nxt.ID)
	}
}

func TestUpgradeAndRestartRolePlans(t *testing.T) {
	v := newEnv(t, "")
	c := Ctx{Cluster: "prod", Topo: v.topo}
	p, err := c.Plan(KindUpgrade, js(upgradeParams{Version: "v8.5.3"}))
	if err != nil || p.Blocked || p.Risk != "high" {
		t.Fatalf("%v %+v", err, p.Checks)
	}
	if p, _ = c.Plan(KindUpgrade, js(upgradeParams{Version: "v8.1.0"})); !p.Blocked {
		t.Error("降级应阻断")
	}
	if _, err := c.Plan(KindUpgrade, js(upgradeParams{Version: "8.5.3; rm"})); err == nil {
		t.Error("版本格式非法应拒绝")
	}
	p, err = c.Plan(KindRestartRole, js(restartRoleParams{Role: "tikv"}))
	if err != nil || p.Blocked || p.Steps[0].Cmds[0].Role != "tikv" {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestBackupStoragePrefixCheckedAtPlanTime(t *testing.T) {
	v := newEnv(t, "")
	c := Ctx{Cluster: "prod", Topo: v.topo, BackupRoots: []string{"local:///backup/"}}
	if p, err := c.Plan(KindBackup, js(backupParams{Mode: "full", Storage: "s3://evil/x"})); err != nil || !p.Blocked {
		t.Errorf("白名单之外的存储应阻断: %v %+v", err, p.Checks)
	}
	if p, err := c.Plan(KindBackup, js(backupParams{Mode: "full", Storage: "local:///backup/snap-1"})); err != nil || p.Blocked {
		t.Errorf("白名单内应放行: %v %+v", err, p.Checks)
	}
	if p, _ := c.Plan(KindRestore, js(backupParams{Storage: "local:///backup/../etc"})); !p.Blocked {
		t.Error("路径穿越应阻断")
	}
}
