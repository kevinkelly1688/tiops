// @author: kevinkelly
// @license: Apache License 2.0
// @file: autodetect.go
// @time: 2026/10/28

package source

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Detect 用只读命令 tiup cluster list / display 自动补全集群名、各组件地址与 SQL 连接。
// cfg 会被就地补全（已显式给出的值不会被覆盖）；返回最终的集群名与探测说明。
func Detect(ctx context.Context, cfg *LiveConfig, cluster string) (string, []string, error) {
	if cfg.Endpoints == nil {
		cfg.Endpoints = map[string]string{}
	}
	live := NewLive(*cfg)
	var notes []string
	if cluster == "" {
		b, err := live.Fetch(ctx, Item{Name: "tiup/cluster-list", Kind: KindCmd, Argv: []string{"tiup", "cluster", "list"}})
		if err != nil {
			return "", notes, fmt.Errorf("自动探测集群失败（可用 --cluster 指定）: %w", err)
		}
		names := ParseClusterList(string(b))
		switch len(names) {
		case 0:
			return "", notes, fmt.Errorf("tiup cluster list 没有返回任何集群，请检查是否在中控机上以 tiup 用户运行")
		case 1:
			cluster = names[0]
		default:
			return "", notes, fmt.Errorf("检测到多个集群 %v，请用 --cluster 指定其一", names)
		}
		notes = append(notes, fmt.Sprintf("自动探测：集群 %s", cluster))
	}
	needs := cfg.Endpoints["pd"] == "" || cfg.Endpoints["tidb"] == "" || cfg.Endpoints["prom"] == "" || cfg.Endpoints["alert"] == "" || cfg.MySQL.Host == ""
	if needs {
		b, err := live.Fetch(ctx, Item{Name: "tiup/cluster-display", Kind: KindCmd, Argv: []string{"tiup", "cluster", "display", cluster}})
		if err != nil {
			notes = append(notes, fmt.Sprintf("提示：自动探测拓扑失败（%v），请手动给出各地址", err))
			return cluster, notes, nil
		}
		ep, mh, mp := ParseDisplay(string(b)).Endpoints()
		var filled []string
		for k, v := range ep {
			if cfg.Endpoints[k] == "" {
				cfg.Endpoints[k] = v
				filled = append(filled, k)
			}
		}
		if cfg.MySQL.Host == "" {
			cfg.MySQL.Host = mh
		}
		if cfg.MySQL.Port == 0 {
			cfg.MySQL.Port = mp
		}
		notes = append(notes, fmt.Sprintf("自动探测：端点 %v", filled))
	}
	return cluster, notes, nil
}

// Cached 给任意 Source 加上按条目名的 TTL 缓存，避免页面频繁刷新时反复访问生产环境。
type Cached struct {
	Src Source
	TTL int64 // 秒

	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	at   time.Time
	data []byte
	err  error
}

// NewCached 创建缓存包装。ttl 为 0 表示不缓存。
func NewCached(src Source, ttl time.Duration) *Cached {
	return &Cached{Src: src, TTL: int64(ttl / time.Second), m: map[string]cacheEntry{}}
}

// Mode 实现 Source。
func (c *Cached) Mode() string { return c.Src.Mode() }

// Fetch 实现 Source。失败结果也会被短暂缓存（5 秒），避免故障时被刷新请求放大。
func (c *Cached) Fetch(ctx context.Context, it Item) ([]byte, error) {
	if c.TTL <= 0 {
		return c.Src.Fetch(ctx, it)
	}
	c.mu.Lock()
	e, ok := c.m[it.Name]
	c.mu.Unlock()
	ttl := time.Duration(c.TTL) * time.Second
	if e.err != nil && ttl > 5*time.Second {
		ttl = 5 * time.Second
	}
	if ok && time.Since(e.at) < ttl {
		return e.data, e.err
	}
	data, err := c.Src.Fetch(ctx, it)
	c.mu.Lock()
	c.m[it.Name] = cacheEntry{at: time.Now(), data: data, err: err}
	c.mu.Unlock()
	return data, err
}
