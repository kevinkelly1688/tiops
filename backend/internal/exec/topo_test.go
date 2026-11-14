// @author: kevinkelly
// @license: Apache License 2.0
// @file: topo_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTopo(t *testing.T) {
	tp, err := ParseTopo(displayText)
	if err != nil {
		t.Fatal(err)
	}
	if tp.Version != "v8.5.2" || tp.User != "tidb" {
		t.Errorf("version/user: %+v", tp)
	}
	if tp.PD != "http://10.0.1.21:2379" {
		t.Errorf("PD = %q", tp.PD)
	}
	if tp.SQLHost != "10.0.1.11" || tp.SQLPort != 4000 {
		t.Errorf("SQL = %s:%d", tp.SQLHost, tp.SQLPort)
	}
	if len(tp.Nodes) != 19 {
		t.Errorf("节点数 %d", len(tp.Nodes))
	}
	roles := map[string]int{}
	for _, r := range tp.Nodes {
		roles[r]++
	}
	want := map[string]int{"tikv": 6, "tidb": 3, "pd": 3, "tiflash": 2, "ticdc": 2, "alertmanager": 1, "grafana": 1, "prometheus": 1}
	for r, n := range want {
		if roles[r] != n {
			t.Errorf("角色 %s: %d, 期望 %d", r, roles[r], n)
		}
	}
	if roles["cdc"] != 0 {
		t.Error("cdc 应归一为 ticdc")
	}
	for st, id := range map[string]string{
		"10.0.2.11:20180": "10.0.2.11:20160", // TiKV 状态端口
		"10.0.1.11:10080": "10.0.1.11:4000",  // TiDB 状态端口
		"10.0.1.21:2379":  "10.0.1.21:2379",  // PD
		"10.0.3.11:20292": "10.0.3.11:3930",  // TiFlash proxy status
	} {
		if tp.Status[st] != id {
			t.Errorf("Status[%s] = %q, want %q", st, tp.Status[st], id)
		}
	}
	if _, ok := tp.Status["10.0.2.11:20160"]; ok {
		t.Error("业务端口不是状态地址")
	}
	for id := range tp.Nodes {
		if !nodeRe.MatchString(id) {
			t.Errorf("节点 ID %q 格式", id)
		}
	}
}

func TestParseTopoEdgeCases(t *testing.T) {
	// 第一个 PD 是 Down，则 PD 取第一个 Up 的。
	down := strings.Replace(displayText, "10.0.1.21:2379     pd            10.0.1.21  2379/2380    linux/x86_64  Up|L|UI", "10.0.1.21:2379     pd            10.0.1.21  2379/2380    linux/x86_64  Down    ", 1)
	tp, err := ParseTopo(down)
	if err != nil || tp.PD != "http://10.0.1.22:2379" {
		t.Errorf("PD = %q %v", tp.PD, err)
	}
	// Down 的节点仍在 Nodes 里（允许 start / restart 它）。
	if tp.Nodes["10.0.1.21:2379"] != "pd" {
		t.Error("Down 节点应保留在 Nodes 中")
	}
	// 没有 Up 的 PD：PD 为空（pd-ctl 类命令会因此被拒绝）。
	allDown := strings.NewReplacer("Up|L|UI", "Down", "pd            10.0.1.22  2379/2380    linux/x86_64  Up", "pd            10.0.1.22  2379/2380    linux/x86_64  Down").Replace(displayText)
	allDown = strings.ReplaceAll(allDown, "pd            10.0.1.23  2379/2380    linux/x86_64  Up", "pd            10.0.1.23  2379/2380    linux/x86_64  Down")
	if tp, err := ParseTopo(allDown); err != nil || tp.PD != "" {
		t.Errorf("无 Up PD: %q %v", tp.PD, err)
	}
	for name, in := range map[string]string{
		"空":     "",
		"垃圾":    "error: no such cluster\n",
		"没有版本":  strings.Replace(displayText, "Cluster version:    v8.5.2", "", 1),
		"版本非法":  strings.Replace(displayText, "v8.5.2", "nightly", 1),
		"版本被注入": strings.Replace(displayText, "v8.5.2", "v8.5.2;id", 1),
		"没有实例":  "Cluster version:    v8.5.2\nID  Role  Host  Ports  OS/Arch  Status\n",
	} {
		if _, err := ParseTopo(in); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestDisplayTopoCache(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var argvs [][]string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		argvs = append(argvs, argv)
		return []byte(displayText), nil
	}
	now := time.Unix(1_700_000_000, 0)
	topo := DisplayTopo(run, WithTopoClock(func() time.Time { return now }))
	ctx := context.Background()
	tp, err := topo(ctx, testCluster)
	if err != nil || tp.Version != "v8.5.2" {
		t.Fatal(err)
	}
	if len(argvs[0]) != 4 || strings.Join(argvs[0], " ") != "tiup cluster display "+testCluster {
		t.Errorf("argv = %q", argvs[0])
	}
	// 10 秒内走缓存。
	now = now.Add(9 * time.Second)
	if _, err := topo(ctx, testCluster); err != nil || calls != 1 {
		t.Errorf("缓存期内不应再次调用: %d %v", calls, err)
	}
	// 缓存返回的是拷贝：调用方改动不污染缓存。
	tp.Nodes["10.9.9.9:1"] = "tikv"
	tp.Status["x:1"] = "y"
	tp2, _ := topo(ctx, testCluster)
	if _, ok := tp2.Nodes["10.9.9.9:1"]; ok {
		t.Error("缓存被调用方污染")
	}
	if _, ok := tp2.Status["x:1"]; ok {
		t.Error("缓存被调用方污染(Status)")
	}
	// 过期后重新获取。
	now = now.Add(2 * time.Second)
	if _, err := topo(ctx, testCluster); err != nil || calls != 2 {
		t.Errorf("过期后应重新获取: %d %v", calls, err)
	}
	// 不同集群各自缓存。
	if _, err := topo(ctx, "slowc"); err != nil || calls != 3 {
		t.Errorf("%d %v", calls, err)
	}
	// 自定义 TTL。
	topo2 := DisplayTopo(run, WithTopoTTL(0))
	calls = 0
	_, _ = topo2(ctx, testCluster)
	_, _ = topo2(ctx, testCluster)
	if calls != 2 {
		t.Errorf("TTL=0 不应缓存: %d", calls)
	}
}

func TestDisplayTopoErrors(t *testing.T) {
	calls := 0
	boom := func(context.Context, []string) ([]byte, error) {
		calls++
		return nil, errors.New("ssh: connection refused")
	}
	topo := DisplayTopo(boom)
	if _, err := topo(context.Background(), testCluster); err == nil {
		t.Error("run 失败应返回错误")
	}
	// 失败不缓存。
	_, _ = topo(context.Background(), testCluster)
	if calls != 2 {
		t.Errorf("失败不应被缓存: %d", calls)
	}
	// 集群名不合法时根本不调用 run。
	calls = 0
	for _, n := range []string{"", "../x", "a b", "-x", "a;b", "a\n"} {
		if _, err := topo(context.Background(), n); err == nil {
			t.Errorf("%q 应拒绝", n)
		}
	}
	if calls != 0 {
		t.Errorf("非法集群名不应触发 run: %d", calls)
	}
	// 输出无法解析。
	bad := DisplayTopo(func(context.Context, []string) ([]byte, error) { return []byte("garbage"), nil })
	if _, err := bad(context.Background(), testCluster); err == nil {
		t.Error("解析失败应报错")
	}
}

// DisplayTopo + DirectRunner：display 命令不需要拓扑，不会递归。
func TestDisplayTopoWithDirectRunner(t *testing.T) {
	r, pol := directRunner(t, nil)
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		// 注入的 run 只允许走只读白名单：把 argv 翻译成结构化 Cmd。
		if len(argv) != 4 || argv[1] != "cluster" || argv[2] != "display" {
			return nil, errors.New("不允许")
		}
		sim := &SimRunner{Pol: pol, Topo: func(context.Context, string) (Topo, error) { return Topo{}, errors.New("display 不应需要拓扑") }, Key: testKey}
		res, err := sim.Run(ctx, Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: argv[3]}}, nil)
		if err != nil {
			return nil, err
		}
		// 模拟输出里没有真实的 display 头部，补上 fake 的完整输出以便解析。
		_ = res
		return []byte(displayText), nil
	}
	r.Topo = DisplayTopo(run)
	res, err := r.Run(context.Background(), signedReq(Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}), nil)
	if err != nil || res.Exit != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// 拓扑里没有的节点被拒绝。
	if _, err := r.Run(context.Background(), signedReq(Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.9.9:20160"}}), nil); err == nil {
		t.Error("拓扑外节点应拒绝")
	}
}

func TestStaticTopo(t *testing.T) {
	topo := StaticTopo(testTopo())
	a, _ := topo(context.Background(), "anything")
	a.Nodes["x:1"] = "tikv"
	b, _ := topo(context.Background(), "other")
	if _, ok := b.Nodes["x:1"]; ok {
		t.Error("StaticTopo 应返回独立拷贝")
	}
	m := StaticTopos(map[string]Topo{"a": testTopo()})
	if _, err := m(context.Background(), "a"); err != nil {
		t.Error(err)
	}
	if _, err := m(context.Background(), "b"); err == nil {
		t.Error("未知集群应报错")
	}
}
