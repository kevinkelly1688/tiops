// @author: kevinkelly
// @license: Apache License 2.0
// @file: insight_api.go
// @time: 2026/10/28

package server

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"tidbops/internal/authz"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
)

// insightCache 是洞察接口的小缓存：这些接口要额外查询 Prometheus / 只读 SQL，不应随页面刷新被放大。
// 零值可用。key 是接口名；带 ?refresh=1 时跳过缓存（但仍保留 2 秒的最短间隔）。
type insightCache struct {
	mu sync.Mutex
	m  map[string]insightEntry
}

type insightEntry struct {
	at time.Time
	v  any
}

func (c *insightCache) get(key string, ttl time.Duration, force bool, build func() any) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		if age := time.Since(e.at); age < ttl && !force || age < 2*time.Second {
			return e.v
		}
	}
	v := build()
	if c.m == nil {
		c.m = map[string]insightEntry{}
	}
	c.m[key] = insightEntry{time.Now(), v}
	return v
}

// configSpecs 把配置项目录（exec.Catalog）转成 collect 需要的形式。顺序稳定。
func configSpecs() []collect.ConfigSpec {
	cat := exec.Catalog()
	roles := make([]string, 0, len(cat))
	for r := range cat {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	var out []collect.ConfigSpec
	for _, r := range roles {
		for _, it := range cat[r] {
			out = append(out, collect.ConfigSpec{Role: it.Role, Key: it.Key, Type: string(it.Type), Opts: it.Opts, Sys: it.Sys, Scope: it.Scope})
		}
	}
	return out
}

// registerInsights 注册只读洞察接口：配置当前值、容量预测、巡检。运维模式与只读模式都会调用；
// read 在运维模式下要求登录并检查能力，只读模式直接放行（u 为 nil）。
//
//	GET /api/config-values   目录里每个配置项的当前真实值（取不到的项省略）
//	GET /api/forecast        基于最近 7 天的容量与 Region 增长预测
//	GET /api/inspect         巡检（不含主机检查；含主机检查的 POST /api/inspect 仅运维模式提供）
func (s *Server) registerInsights(mux *http.ServeMux, read func(authz.Cap, func(w http.ResponseWriter, r *http.Request, u *authz.User)) http.HandlerFunc) {
	refresh := func(r *http.Request) bool { return r.URL.Query().Get("refresh") == "1" }

	mux.HandleFunc("GET /api/config-values", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		v := s.insights.get("config-values", 10*time.Second, refresh(r), func() any {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
			defer cancel()
			return s.cfg.Col.ConfigValues(ctx, configSpecs())
		})
		writeJSON(w, v)
	}))

	mux.HandleFunc("GET /api/forecast", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		v := s.insights.get("forecast", 5*time.Minute, refresh(r), func() any {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
			defer cancel()
			return s.cfg.Col.Forecast(ctx)
		})
		writeJSON(w, v)
	}))

	// 主机规格：每台主机的核数、内存、系统、内核、数据盘容量（node_exporter）与归属实例、AZ、机架；取不到的项为 0 / 空。
	mux.HandleFunc("GET /api/hosts", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		v := s.insights.get("hosts", 30*time.Second, refresh(r), func() any {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
			defer cancel()
			hosts := s.cfg.Col.Hosts(ctx, s.Snapshot(ctx, false).Topology)
			return map[string]any{"hosts": hosts, "note": hostsNote(hosts)}
		})
		writeJSON(w, v)
	}))

	// 只读巡检：与 POST /api/inspect 同一套检查，但没有主机检查（tiup cluster check 需要运维模式的执行器）。
	mux.HandleFunc("GET /api/inspect", read(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, u *authz.User) {
		snap := s.Snapshot(r.Context(), true)
		if u != nil {
			snap = s.filterSnapshot(snap, *u)
		}
		writeJSON(w, collect.Inspect(snap, s.cfg.Col.Settings(r.Context()), nil))
	}))
}
