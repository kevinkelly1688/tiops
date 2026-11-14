// @author: kevinkelly
// @license: Apache License 2.0
// @file: parse.go
// @time: 2026/10/28

package collect

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------- Prometheus ----

// Point 是时间序列上的一个点。
type Point struct {
	T int64
	V float64
}

// Series 是 query_range 返回的一条序列。
type Series struct {
	Metric map[string]string
	Points []Point
}

// ParseMatrix 解析 Prometheus /api/v1/query_range 的响应。
func ParseMatrix(b []byte) ([]Series, error) {
	var r struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析 Prometheus 响应失败: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("Prometheus 返回状态 %q", r.Status)
	}
	var out []Series
	for _, s := range r.Data.Result {
		ser := Series{Metric: s.Metric}
		for _, v := range s.Values {
			if len(v) != 2 {
				continue
			}
			t, _ := v[0].(float64)
			str, _ := v[1].(string)
			ser.Points = append(ser.Points, Point{T: int64(t), V: f64(str)})
		}
		out = append(out, ser)
	}
	return out, nil
}

// Last 返回最近 n 个点的平均值（n≥1），用于抹平单点抖动。
func (s Series) Last(n int) float64 {
	if len(s.Points) == 0 {
		return 0
	}
	if n > len(s.Points) {
		n = len(s.Points)
	}
	sum := 0.0
	for _, p := range s.Points[len(s.Points)-n:] {
		sum += p.V
	}
	return sum / float64(n)
}

// First 返回最早 n 个点的平均值。
func (s Series) First(n int) float64 {
	if len(s.Points) == 0 {
		return 0
	}
	if n > len(s.Points) {
		n = len(s.Points)
	}
	sum := 0.0
	for _, p := range s.Points[:n] {
		sum += p.V
	}
	return sum / float64(n)
}

// ByHost 把带 instance 标签的序列按主机（去掉端口）索引，取最近 3 个点的平均值。
func ByHost(series []Series) map[string]float64 {
	m := map[string]float64{}
	for _, s := range series {
		if inst := s.Metric["instance"]; inst != "" {
			m[hostOf(inst)] = s.Last(3)
		}
	}
	return m
}

// ---------------------------------------------------------------- PD ----

// Store 是 PD 里的一个 Store（TiKV 或 TiFlash）。
type Store struct {
	ID         uint64
	Address    string
	Host       string
	Version    string
	State      string
	Labels     map[string]string
	CapacityGB float64
	AvailGB    float64
	UsedGB     float64
	Leaders    int
	Regions    int
}

// IsFlash 判断是否为 TiFlash Store。
func (s Store) IsFlash() bool { return s.Labels["engine"] == "tiflash" }

// DiskPct 返回磁盘使用率（容量 - 可用）/ 容量。
func (s Store) DiskPct() float64 {
	if s.CapacityGB <= 0 {
		return 0
	}
	return (s.CapacityGB - s.AvailGB) / s.CapacityGB * 100
}

// ParseStores 解析 /pd/api/v1/stores。
func ParseStores(b []byte) ([]Store, error) {
	var r struct {
		Stores []struct {
			Store struct {
				ID        uint64 `json:"id"`
				Address   string `json:"address"`
				Version   string `json:"version"`
				StateName string `json:"state_name"`
				Labels    []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"labels"`
			} `json:"store"`
			Status struct {
				Capacity    string `json:"capacity"`
				Available   string `json:"available"`
				UsedSize    string `json:"used_size"`
				LeaderCount int    `json:"leader_count"`
				RegionCount int    `json:"region_count"`
			} `json:"status"`
		} `json:"stores"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析 PD stores 失败: %w", err)
	}
	var out []Store
	for _, s := range r.Stores {
		st := Store{ID: s.Store.ID, Address: s.Store.Address, Host: hostOf(s.Store.Address), Version: strings.TrimPrefix(s.Store.Version, "v"), State: s.Store.StateName, Labels: map[string]string{},
			CapacityGB: sizeGiB(s.Status.Capacity), AvailGB: sizeGiB(s.Status.Available), UsedGB: sizeGiB(s.Status.UsedSize), Leaders: s.Status.LeaderCount, Regions: s.Status.RegionCount}
		for _, l := range s.Store.Labels {
			st.Labels[l.Key] = l.Value
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ParseMembers 解析 /pd/api/v1/members，返回 Leader 所在主机与全部成员主机。
func ParseMembers(b []byte) (leaderHost string, hosts []string, err error) {
	var r struct {
		Members []struct {
			Name       string   `json:"name"`
			ClientURLs []string `json:"client_urls"`
		} `json:"members"`
		Leader struct {
			Name       string   `json:"name"`
			ClientURLs []string `json:"client_urls"`
		} `json:"leader"`
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return "", nil, fmt.Errorf("解析 PD members 失败: %w", err)
	}
	for _, m := range r.Members {
		if len(m.ClientURLs) > 0 {
			hosts = append(hosts, hostOf(m.ClientURLs[0]))
		}
		if m.Name == r.Leader.Name && len(m.ClientURLs) > 0 {
			leaderHost = hostOf(m.ClientURLs[0])
		}
	}
	if leaderHost == "" && len(r.Leader.ClientURLs) > 0 {
		leaderHost = hostOf(r.Leader.ClientURLs[0])
	}
	return
}

// StoreHot 是一个 Store 上的热点汇总与热点 Region。
type StoreHot struct {
	StoreID uint64
	FlowMBs float64
	Regions []HotStat
}

// HotStat 是一个热点 Region 的统计。StartKey / EndKey 是十六进制 Key；PD 的热点统计通常不带，需要从 Region 列表里补。
type HotStat struct {
	ID       uint64
	Flow     float64
	Deg      int
	StartKey string
	EndKey   string
}

// ParseHot 解析 /pd/api/v1/hotspot/regions/{write,read}，按 Leader 视角汇总。
func ParseHot(b []byte) (map[uint64]*StoreHot, error) {
	var r struct {
		AsLeader map[string]struct {
			Total int64 `json:"total_flow_bytes"`
			Stats []struct {
				RegionID uint64  `json:"region_id"`
				Flow     float64 `json:"flow_bytes"`
				Degree   int     `json:"hot_degree"`
				StartKey string  `json:"start_key"`
				EndKey   string  `json:"end_key"`
			} `json:"statistics"`
		} `json:"as_leader"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析热点 Region 失败: %w", err)
	}
	out := map[uint64]*StoreHot{}
	for k, v := range r.AsLeader {
		var id uint64
		fmt.Sscan(k, &id)
		sh := &StoreHot{StoreID: id, FlowMBs: float64(v.Total) / 1e6}
		for _, s := range v.Stats {
			sh.Regions = append(sh.Regions, HotStat{ID: s.RegionID, Flow: s.Flow / 1e6, Deg: s.Degree, StartKey: s.StartKey, EndKey: s.EndKey})
		}
		out[id] = sh
	}
	return out, nil
}
