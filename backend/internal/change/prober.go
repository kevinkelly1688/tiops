// @author: kevinkelly
// @license: Apache License 2.0
// @file: prober.go
// @time: 2026/10/28

package change

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"tidbops/internal/source"
)

// Prober 探测步骤的等待条件。done 为 false 且 err 为 nil 表示「还没到」；err 为暂时性错误，引擎会继续重试直到超时。
type Prober interface {
	Probe(ctx context.Context, w Wait) (done bool, detail string, err error)
}

// PDProber 通过 PD 的 HTTP API 与 `tiup cluster display` 探测集群状态（只读）。
type PDProber struct {
	PD      func() string                                              // PD 地址，如 http://10.0.1.21:2379
	Get     func(ctx context.Context, url string) ([]byte, int, error) // 返回 body 与状态码
	Display func(ctx context.Context) (string, error)                  // tiup cluster display 的输出
}

// Probe 实现 Prober。
func (p PDProber) Probe(ctx context.Context, w Wait) (bool, string, error) {
	switch w.Kind {
	case "leader-zero", "store-state":
		body, code, err := p.Get(ctx, fmt.Sprintf("%s/pd/api/v1/store/%d", strings.TrimRight(p.PD(), "/"), w.Store))
		if err != nil {
			return false, "", err
		}
		if code == 404 {
			return w.Kind == "store-state" && w.Want == "Tombstone", "Store 已不存在", nil
		}
		var s struct {
			Store struct {
				State string `json:"state_name"`
			} `json:"store"`
			Status struct {
				Leader int `json:"leader_count"`
				Region int `json:"region_count"`
			} `json:"status"`
		}
		if err := json.Unmarshal(body, &s); err != nil {
			return false, "", fmt.Errorf("解析 PD 响应失败: %w", err)
		}
		if w.Kind == "leader-zero" {
			return s.Status.Leader == 0, fmt.Sprintf("leader_count=%d", s.Status.Leader), nil
		}
		return strings.EqualFold(s.Store.State, w.Want), fmt.Sprintf("状态 %s，剩余 Region %d", s.Store.State, s.Status.Region), nil
	case "node-up":
		out, err := p.Display(ctx)
		if err != nil {
			return false, "", err
		}
		for _, in := range source.ParseDisplay(out).Instances {
			if in.ID == w.Node {
				return strings.HasPrefix(in.Status, "Up"), "状态 " + in.Status, nil
			}
		}
		return false, "拓扑里还没有该实例", nil
	case "no-down-peers":
		for _, k := range []string{"down-peer", "pending-peer"} {
			body, code, err := p.Get(ctx, fmt.Sprintf("%s/pd/api/v1/regions/check/%s", strings.TrimRight(p.PD(), "/"), k))
			if err != nil {
				return false, "", err
			}
			if code != 200 {
				return false, "", fmt.Errorf("PD 返回 %d", code)
			}
			var r struct {
				Count int `json:"count"`
			}
			if err := json.Unmarshal(body, &r); err != nil {
				return false, "", err
			}
			if r.Count > 0 {
				return false, fmt.Sprintf("仍有 %d 个 Region 处于 %s", r.Count, k), nil
			}
		}
		return true, "无 down-peer / pending-peer", nil
	}
	return false, "", fmt.Errorf("未知的等待条件 %q", w.Kind)
}

// SimProber 用于回放 / 演示：每个等待在探测 Polls 次后完成。
type SimProber struct {
	Polls int
	mu    sync.Mutex
	n     map[string]int
}

// Probe 实现 Prober。
func (s *SimProber) Probe(ctx context.Context, w Wait) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	k := fmt.Sprintf("%s/%d/%s", w.Kind, w.Store, w.Node)
	s.n[k]++
	if s.n[k] > s.Polls {
		delete(s.n, k)
		return true, "（模拟）条件已满足", nil
	}
	return false, fmt.Sprintf("（模拟）第 %d 次探测", s.n[k]), nil
}
