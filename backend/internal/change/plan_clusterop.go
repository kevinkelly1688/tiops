// @author: kevinkelly
// @license: Apache License 2.0
// @file: plan_clusterop.go
// @time: 2026/10/28

package change

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tidbops/internal/exec"
)

// 集群级操作种类。
const (
	KindRestartRole = "cluster.restart-role" // 按角色滚动重启
	KindUpgrade     = "cluster.upgrade"      // 滚动升级
)

type restartRoleParams struct {
	Role string `json:"role"` // tidb | tikv | pd | tiflash | ticdc
}

func (c Ctx) planRestartRole(raw json.RawMessage) (Plan, error) {
	var in restartRoleParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	if _, ok := typeLabel[in.Role]; !ok {
		return Plan{}, fmt.Errorf("未知的角色 %q", in.Role)
	}
	group := c.ofType(in.Role)
	if len(group) == 0 {
		return Plan{}, fmt.Errorf("集群中没有 %s", typeLabel[in.Role])
	}
	p := Plan{Title: "滚动重启 " + typeLabel[in.Role], Risk: "mid", Targets: ids(group), Hosts: uniq(hostsOf(group)), EstMins: 3 * len(group), Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	healthyN := 0
	for _, n := range group {
		if healthy(n) {
			healthyN++
		}
		if m := c.maintOn(n.Host); m != nil {
			add("bad", fmt.Sprintf("%s 所在主机 %s 正在维护（%s）", n.ID, n.Host, m.ID))
		}
	}
	if healthyN <= 1 {
		add("bad", fmt.Sprintf("%s 只有 %d 个健康实例，滚动重启期间服务不可用", typeLabel[in.Role], healthyN))
	}
	switch in.Role {
	case "tikv":
		add("warn", "TiKV 会驱逐 Leader 后逐个重启，业务有短暂抖动")
		for _, m := range c.Maints {
			if MaintLive(m.State) && m.HasTiKV {
				add("bad", "当前有 TiKV 主机处于维护中，此时再滚动重启 TiKV 可能导致部分 Region 失去多数派")
				break
			}
		}
	case "pd":
		add("warn", "PD Leader 重启会触发选举，TSO 有秒级抖动")
	case "tidb":
		add("warn", "逐个重启 TiDB，连接会断开；请确认负载均衡健康检查已配置")
	}
	add("ok", "避开业务高峰与备份窗口")
	p.Steps = []Step{{Name: "滚动重启（tiup cluster restart -R）", Cmds: []exec.Cmd{{Kind: exec.KindClusterRestart, Cluster: c.Cluster, Role: tiupRole(in.Role)}}, Irreversible: true}}
	for _, n := range group {
		p.Steps = append(p.Steps, Step{Name: "等待 " + n.ID + " 就绪", Wait: &Wait{Kind: "node-up", Node: addr(n), Timeout: 15 * time.Minute, Interval: 10 * time.Second, Desc: "等待实例 Up"}})
	}
	return p, nil
}

type upgradeParams struct {
	Version string `json:"version"`
}

var reVer = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

func parseVer(s string) (a [3]int, ok bool) {
	m := reVer.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return a, false
	}
	for i := 0; i < 3; i++ {
		a[i], _ = strconv.Atoi(m[i+1])
	}
	return a, true
}

func (c Ctx) planUpgrade(raw json.RawMessage) (Plan, error) {
	var in upgradeParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	to, ok := parseVer(in.Version)
	if !ok {
		return Plan{}, fmt.Errorf("版本格式应为 v8.5.3")
	}
	p := Plan{Title: "滚动升级到 " + in.Version, Risk: "high", Targets: ids(c.Topo.Instances), Hosts: uniq(hostsOf(c.Topo.Instances)), EstMins: 20 + 4*len(c.Topo.Instances), Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	cur, okc := parseVer(c.Topo.Version)
	switch {
	case !okc:
		add("warn", "无法识别当前版本，请自行确认升级路径")
	case to == cur:
		add("bad", "目标版本与当前版本相同")
	case to[0] < cur[0] || (to[0] == cur[0] && to[1] < cur[1]) || (to[0] == cur[0] && to[1] == cur[1] && to[2] < cur[2]):
		add("bad", fmt.Sprintf("不支持降级（当前 %s → 目标 %s）", c.Topo.Version, in.Version))
	case to[0] != cur[0] || to[1]-cur[1] > 1:
		add("warn", "跨大版本 / 跨多个小版本升级，请先阅读官方升级指南并在预发环境演练")
	default:
		add("ok", "升级路径合理")
	}
	for _, n := range c.Topo.Instances {
		if !healthy(n) {
			add("bad", fmt.Sprintf("%s 状态为 %s：升级前必须所有实例健康", n.ID, n.Status))
		}
	}
	for _, m := range c.Maints {
		if MaintLive(m.State) {
			add("bad", fmt.Sprintf("主机 %s 正在维护（%s），请先结束维护", m.IP, m.ID))
		}
	}
	add("warn", "升级前必须完成一次全量备份，并阅读目标版本 Release Notes 与兼容性变更")
	add("ok", "滚动升级顺序：PD → TiKV → TiDB，业务无需停机")
	p.Steps = []Step{
		{Name: "升级前预检（tiup cluster check）", Cmds: []exec.Cmd{{Kind: exec.KindClusterCheck, Cluster: c.Cluster}}, IgnoreErr: true},
		{Name: "滚动升级（tiup cluster upgrade）", Cmds: []exec.Cmd{{Kind: exec.KindClusterUpgrade, Cluster: c.Cluster, Value: in.Version}}, Irreversible: true},
	}
	for _, n := range c.Topo.Instances {
		p.Steps = append(p.Steps, Step{Name: "等待 " + n.ID + " 就绪", Wait: &Wait{Kind: "node-up", Node: addr(n), Timeout: 30 * time.Minute, Interval: 15 * time.Second, Desc: "等待实例 Up"}})
	}
	p.Steps = append(p.Steps, Step{Name: "查看集群状态", Cmds: []exec.Cmd{{Kind: exec.KindClusterDisplay, Cluster: c.Cluster}}})
	return p, nil
}
