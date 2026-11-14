// @author: kevinkelly
// @license: Apache License 2.0
// @file: settings.go
// @time: 2026/10/28

package collect

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ClusterSettings 是变更规划需要的集群级设置。取不到的项保持零值（调用方使用默认值）。
type ClusterSettings struct {
	PDDownMin    int    // schedule.max-store-down-time（分钟）
	Replicas     int    // replication.max-replicas
	TopologyYAML string // tiup 保存的拓扑（meta.yaml 的 topology 部分）
	TopologyReal bool   // TopologyYAML 来自 meta.yaml（而非推导）
}

var reDur = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$`)

// parseDurMin 把 PD 返回的时长（如 "30m0s"、"1h0m0s"）换算为分钟。
func parseDurMin(s string) int {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
		return int(d.Minutes())
	}
	return 0
}

// Settings 读取 PD 配置与 tiup 拓扑。
func (c *Collector) Settings(ctx context.Context) ClusterSettings {
	var out ClusterSettings
	if b, err := c.get(ctx, "pd/config"); err == nil {
		var cfg struct {
			Schedule struct {
				MaxStoreDown string `json:"max-store-down-time"`
			} `json:"schedule"`
			Replication struct {
				MaxReplicas int `json:"max-replicas"`
			} `json:"replication"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			out.PDDownMin = parseDurMin(cfg.Schedule.MaxStoreDown)
			out.Replicas = cfg.Replication.MaxReplicas
		}
	}
	if b, err := c.get(ctx, "tiup/meta"); err == nil {
		var meta struct {
			Topology yaml.Node `yaml:"topology"`
		}
		if yaml.Unmarshal(b, &meta) == nil && meta.Topology.Kind == yaml.MappingNode {
			var sb strings.Builder
			enc := yaml.NewEncoder(&sb)
			enc.SetIndent(2)
			if enc.Encode(&meta.Topology) == nil {
				_ = enc.Close()
				out.TopologyYAML, out.TopologyReal = sb.String(), true
			}
		}
	}
	return out
}
