// @author: kevinkelly
// @license: Apache License 2.0
// @file: topo.go
// @time: 2026/10/28

package exec

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"tidbops/internal/source"
)

// RunFunc 由调用方注入，负责真正执行（只读的）`tiup cluster display` 并返回输出。
// argv 形如 ["tiup","cluster","display","<name>"]；注入方应使用只读的 DirectRunner / source.Live 等实现。
type RunFunc func(ctx context.Context, argv []string) ([]byte, error)

// TopoOption 调整 DisplayTopo 的行为（主要用于测试）。
type TopoOption func(*displayTopo)

// WithTopoTTL 设置缓存时长（默认 10 秒）。
func WithTopoTTL(d time.Duration) TopoOption { return func(t *displayTopo) { t.ttl = d } }

// WithTopoClock 注入时钟。
func WithTopoClock(now func() time.Time) TopoOption { return func(t *displayTopo) { t.now = now } }

type topoEntry struct {
	t  Topo
	at time.Time
}

type displayTopo struct {
	run   RunFunc
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	cache map[string]topoEntry
}

// DisplayTopo 通过 `tiup cluster display <c>` 解析集群拓扑，带 10 秒缓存：
//
//	Version  ← Cluster version
//	User     ← Deploy user
//	PD       ← 第一个 Up 的 PD：http://host:client_port
//	Nodes    ← 实例 ID(host:port) → 角色（cdc 归一为 ticdc；监控组件保留原名）
//	Status   ← 状态地址 → 实例 ID：TiKV 取第 2 个端口（20180）、TiDB 第 2 个端口（10080）、
//	           PD 取客户端端口、TiFlash 取第 5 个端口（proxy status，20292）
//	SQLHost/SQLPort ← 第一个 Up 的 TiDB
//
// 返回值是深拷贝，调用方修改不会污染缓存。解析不出版本 / 实例时返回错误。
func DisplayTopo(run RunFunc, opts ...TopoOption) TopoFunc {
	d := &displayTopo{run: run, ttl: 10 * time.Second, now: time.Now, cache: map[string]topoEntry{}}
	for _, o := range opts {
		o(d)
	}
	return d.get
}

func (d *displayTopo) get(ctx context.Context, cluster string) (Topo, error) {
	if !clusterNameRe.MatchString(cluster) {
		return Topo{}, fmt.Errorf("集群名不合法")
	}
	d.mu.Lock()
	if e, ok := d.cache[cluster]; ok && d.now().Sub(e.at) < d.ttl {
		d.mu.Unlock()
		return cloneTopo(e.t), nil
	}
	d.mu.Unlock()

	out, err := d.run(ctx, []string{"tiup", "cluster", "display", cluster})
	if err != nil {
		return Topo{}, fmt.Errorf("tiup cluster display 失败: %w", err)
	}
	t, err := ParseTopo(string(out))
	if err != nil {
		return Topo{}, err
	}
	d.mu.Lock()
	d.cache[cluster] = topoEntry{cloneTopo(t), d.now()}
	d.mu.Unlock()
	return t, nil
}

// ParseTopo 解析 `tiup cluster display` 的输出为 Topo。
func ParseTopo(display string) (Topo, error) {
	ci := source.ParseDisplay(display)
	if !versionRe.MatchString(ci.Version) {
		return Topo{}, fmt.Errorf("无法从 display 输出中解析集群版本")
	}
	if len(ci.Instances) == 0 {
		return Topo{}, fmt.Errorf("display 输出中没有实例")
	}
	t := Topo{Version: ci.Version, User: ci.User, Nodes: map[string]string{}, Status: map[string]string{}}
	for _, in := range ci.Instances {
		ports := strings.Split(in.Ports, "/")
		id := in.ID
		if !nodeRe.MatchString(id) {
			id = net.JoinHostPort(in.Host, ports[0])
		}
		role := canonicalRole(in.Role)
		t.Nodes[id] = role
		up := strings.HasPrefix(in.Status, "Up")
		statusPort := ""
		switch role {
		case "tikv", "tidb":
			if len(ports) > 1 {
				statusPort = ports[1]
			}
		case "pd":
			statusPort = ports[0]
		case "tiflash":
			if len(ports) > 4 {
				statusPort = ports[4]
			}
		}
		if p, err := strconv.Atoi(statusPort); err == nil && p > 0 && p < 65536 {
			t.Status[net.JoinHostPort(in.Host, statusPort)] = id
		}
		if up && role == "pd" && t.PD == "" {
			t.PD = "http://" + net.JoinHostPort(in.Host, ports[0])
		}
		if up && role == "tidb" && t.SQLHost == "" {
			if p, err := strconv.Atoi(ports[0]); err == nil {
				t.SQLHost, t.SQLPort = in.Host, p
			}
		}
	}
	return t, nil
}

func cloneTopo(t Topo) Topo {
	c := t
	c.Nodes = make(map[string]string, len(t.Nodes))
	for k, v := range t.Nodes {
		c.Nodes[k] = v
	}
	c.Status = make(map[string]string, len(t.Status))
	for k, v := range t.Status {
		c.Status[k] = v
	}
	return c
}

// StaticTopo 返回固定拓扑（对任何集群名都返回同一份拷贝），供测试 / 演示使用。
func StaticTopo(t Topo) TopoFunc {
	return func(context.Context, string) (Topo, error) { return cloneTopo(t), nil }
}

// StaticTopos 按集群名返回不同的固定拓扑；未登记的集群返回错误。
func StaticTopos(m map[string]Topo) TopoFunc {
	return func(_ context.Context, cluster string) (Topo, error) {
		t, ok := m[cluster]
		if !ok {
			return Topo{}, fmt.Errorf("未知集群 %q", cluster)
		}
		return cloneTopo(t), nil
	}
}
