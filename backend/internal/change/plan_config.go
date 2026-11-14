// @author: kevinkelly
// @license: Apache License 2.0
// @file: plan_config.go
// @time: 2026/10/28

package change

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"tidbops/internal/exec"
	"tidbops/internal/model"
)

type cfgChange struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	From  string `json:"from,omitempty"` // 仅用于展示 / 历史
}

type configParams struct {
	Node    string      `json:"node"`
	Scope   string      `json:"scope"` // instance | role
	Changes []cfgChange `json:"changes"`
}

// statusAddr 是实例的状态端口地址（SET CONFIG 按它定位实例）。
func statusAddr(n model.Instance) string {
	if n.StatusPort > 0 {
		return fmt.Sprintf("%s:%d", n.Host, n.StatusPort)
	}
	switch n.Type {
	case "tikv":
		return fmt.Sprintf("%s:%d", n.Host, n.Port+20)
	case "tidb":
		return fmt.Sprintf("%s:10080", n.Host)
	}
	return addr(n)
}

func shortKey(k string) string {
	if i := strings.LastIndex(k, "."); i >= 0 {
		return k[i+1:]
	}
	return k
}

func (c Ctx) planConfig(raw json.RawMessage) (Plan, error) {
	var in configParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	n := c.resolve(in.Node)
	if n == nil {
		return Plan{}, fmt.Errorf("集群拓扑中没有实例 %q", in.Node)
	}
	if in.Scope != "instance" && in.Scope != "role" {
		in.Scope = "instance"
	}
	if len(in.Changes) == 0 {
		return Plan{}, fmt.Errorf("没有要变更的配置项")
	}
	role := n.Type
	var group []model.Instance
	if in.Scope == "role" {
		group = c.ofType(role)
	} else {
		group = []model.Instance{*n}
	}
	p := Plan{Risk: "mid", Targets: ids(group), Hosts: uniq(hostsOf(group)), EstMins: 5, Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }

	type item struct {
		it  exec.ConfigItem
		val string
	}
	var items []item
	for _, ch := range in.Changes {
		it, ok := exec.Lookup(role, ch.Key)
		if !ok {
			return Plan{}, fmt.Errorf("配置项 %s 不在 %s 的白名单目录内", ch.Key, typeLabel[role])
		}
		v, err := exec.Normalize(it, ch.Value)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", ch.Key, err)
		}
		items = append(items, item{it, v})
		c.valueChecks(&p, role, it, v)
		if it.Danger {
			p.Risk = "high"
			add("bad", fmt.Sprintf("%s 是高危参数：修改会触发全量副本变更，请确认已评估容量并安排低峰", it.Key))
		}
	}
	for _, t := range group {
		if t.Status == "joining" || t.Status == "offline" {
			add("bad", fmt.Sprintf("%s 处于「%s」状态，请等待完成后再变更", t.ID, t.Status))
		}
		if m := c.maintOn(t.Host); m != nil {
			add("bad", fmt.Sprintf("%s 所在主机 %s 正在维护（%s），不能变更配置", t.ID, t.Host, m.ID))
		}
	}

	var online, persist []exec.Cmd
	needReload := false
	yamlEdits := map[string]any{}
	for _, x := range items {
		switch {
		case x.it.Sys:
			online = append(online, exec.Cmd{Kind: exec.KindSQLSetGlobal, Cluster: c.Cluster, Key: x.it.Key, Value: x.val})
		case role == "pd" && x.it.Scope == "cluster":
			online = append(online, exec.Cmd{Kind: exec.KindPDConfigSet, Cluster: c.Cluster, Key: x.it.Key, Value: x.val})
		default:
			if x.it.Online {
				cmd := exec.Cmd{Kind: exec.KindSQLSetConfig, Cluster: c.Cluster, Key: x.it.Key, Value: x.val}
				if in.Scope == "instance" {
					cmd.Instance = statusAddr(*n)
				} else {
					cmd.Role = role
				}
				online = append(online, cmd)
			} else {
				needReload = true
			}
			yamlEdits[x.it.Key] = x.val
		}
	}
	if len(yamlEdits) > 0 {
		patched, err := PatchTopology(c.TopologyYAML, tiupRole(role), in.Scope, *n, yamlEdits)
		if err != nil {
			return Plan{}, err
		}
		if patched == c.TopologyYAML {
			add("warn", "配置文件中已是目标值，无需写入拓扑")
		} else {
			persist = append(persist, exec.Cmd{Kind: exec.KindClusterEditCfg, Cluster: c.Cluster, Content: patched})
			p.Rollback = []exec.Cmd{{Kind: exec.KindClusterEditCfg, Cluster: c.Cluster, Content: c.TopologyYAML}}
		}
	}

	if needReload {
		p.Risk = maxRisk(p.Risk, "mid")
		p.EstMins = 3 * len(group)
		add("warn", fmt.Sprintf("需滚动 reload %d 个 %s，逐个重启（TiKV 先驱逐 Leader），每个约 2–3 分钟", len(group), typeLabel[role]))
		healthyN := 0
		for _, t := range c.ofType(role) {
			if healthy(t) {
				healthyN++
			}
		}
		if len(group) >= healthyN && healthyN <= 1 {
			add("bad", fmt.Sprintf("%s 只有 %d 个健康实例，reload 期间服务不可用", typeLabel[role], healthyN))
		}
		if role == "tikv" {
			for _, m := range c.Maints {
				if MaintLive(m.State) && m.HasTiKV {
					add("bad", "当前有 TiKV 主机处于维护中，此时再滚动重启 TiKV 可能导致部分 Region 失去多数派")
					break
				}
			}
		}
		if role == "pd" {
			for _, t := range group {
				if t.Leader {
					add("warn", "PD Leader 在 reload 范围内，会触发选举，TSO 有秒级抖动")
					break
				}
			}
		}
	} else {
		add("ok", "全部为在线生效项，无需重启实例")
	}
	add("ok", "变更前的拓扑已随计划保存，失败时可恢复")

	var keys []string
	for _, x := range items {
		keys = append(keys, shortKey(x.it.Key))
	}
	scope := n.ID
	if in.Scope == "role" {
		scope = "全部 " + typeLabel[role]
	}
	p.Title = fmt.Sprintf("配置变更 · %s（%s）", strings.Join(keys, "、"), scope)

	if len(online) > 0 {
		p.Steps = append(p.Steps, Step{Name: fmt.Sprintf("在线下发配置（%d 项）", len(online)), Cmds: online})
	}
	if len(persist) > 0 {
		p.Steps = append(p.Steps, Step{Name: "写入 tiup 拓扑（edit-config），避免之后 reload 被覆盖", Cmds: persist})
	}
	if needReload {
		cmd := exec.Cmd{Kind: exec.KindClusterReload, Cluster: c.Cluster}
		if in.Scope == "role" {
			cmd.Role = tiupRole(role)
		} else {
			cmd.Nodes = []string{addr(*n)}
		}
		p.Steps = append(p.Steps, Step{Name: "滚动 reload", Cmds: []exec.Cmd{cmd}, Irreversible: true})
		for _, t := range group {
			p.Steps = append(p.Steps, Step{Name: "等待 " + t.ID + " 就绪", Wait: &Wait{Kind: "node-up", Node: addr(t), Timeout: 15 * time.Minute, Interval: 10 * time.Second, Desc: "等待实例 Up"}})
		}
	}
	p.Steps = append(p.Steps, Step{Name: "查看集群状态", Cmds: []exec.Cmd{{Kind: exec.KindClusterDisplay, Cluster: c.Cluster}}})
	if len(p.Steps) == 1 {
		add("bad", "没有可执行的变更")
	}
	return p, nil
}

func hostsOf(ns []model.Instance) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Host)
	}
	return out
}

func maxRisk(a, b string) string {
	rank := map[string]int{"low": 0, "mid": 1, "high": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// valueChecks 是与取值有关的合理性提示（来自官方调优建议；与主机内存相关的检查需要主机规格，暂不做）。
func (c Ctx) valueChecks(p *Plan, role string, it exec.ConfigItem, v string) {
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	var iv int
	fmt.Sscan(v, &iv)
	switch it.Key {
	case "replication.max-replicas":
		nt := len(c.ofType("tikv"))
		if iv > nt {
			add("bad", fmt.Sprintf("副本数 %d 大于 TiKV 数量 %d，副本无法补齐", iv, nt))
		} else if iv%2 == 0 {
			add("warn", "偶数副本不提升容错，建议使用奇数")
		} else if iv != c.replicas() {
			add("warn", "修改副本数将触发全量 Region 补齐 / 删除，I/O 与网络压力极大")
		}
	case "schedule.max-store-down-time":
		var m int
		switch {
		case strings.HasSuffix(v, "h"):
			fmt.Sscan(strings.TrimSuffix(v, "h"), &m)
			m *= 60
		case strings.HasSuffix(v, "ms"):
			m = 0
		case strings.HasSuffix(v, "m"):
			fmt.Sscan(strings.TrimSuffix(v, "m"), &m)
		case strings.HasSuffix(v, "s"):
			fmt.Sscan(strings.TrimSuffix(v, "s"), &m)
			m /= 60
		}
		if m < 10 {
			add("warn", "过短：TiKV 短暂重启也会被判定 Down，引发不必要的补副本")
		} else if m > 180 {
			add("warn", "过长：真实故障时副本缺失窗口会很长，风险升高")
		}
		for _, mt := range c.Maints {
			if mt.State == "active" {
				add("warn", "当前有进行中的主机维护，修改该阈值会影响其「是否触发补副本」的判断")
				break
			}
		}
	case "raftstore.store-pool-size":
		if iv > 5 {
			add("warn", "常用范围 1–5，过大会抢占其他线程的 CPU")
		}
	case "schedule.region-schedule-limit":
		if iv > 4096 {
			add("warn", "调度并发过高会影响线上延迟")
		}
	case "tidb_distsql_scan_concurrency":
		if iv > 64 {
			add("warn", "并发过高会加重 TiKV 负载")
		}
	case "log.level", "logger.level", "log-level":
		if v == "debug" {
			add("warn", "debug 日志量巨大，会明显影响性能，仅限短时排障")
		}
	}
}

// PatchTopology 在 tiup 拓扑 YAML 上打补丁：role 级写入 server_configs.<role>，实例级写入对应 *_servers 条目的 config。
// 使用 yaml.Node 以保留原有顺序与注释。edits 的值一律按字符串写入（数字与布尔字面量保持为标量）。
func PatchTopology(src, role, scope string, n model.Instance, edits map[string]any) (string, error) {
	if strings.TrimSpace(src) == "" {
		return "", fmt.Errorf("没有可用的集群拓扑，无法生成持久化配置")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return "", fmt.Errorf("集群拓扑不是合法的 YAML: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("集群拓扑格式不正确")
	}
	root := doc.Content[0]
	keys := make([]string, 0, len(edits))
	for k := range edits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var target *yaml.Node
	if scope == "role" {
		sc := mapGet(root, "server_configs", true)
		target = mapGet(sc, role, true)
	} else {
		section := map[string]string{"tidb": "tidb_servers", "tikv": "tikv_servers", "pd": "pd_servers", "tiflash": "tiflash_servers", "cdc": "cdc_servers"}[role]
		seq := mapGet(root, section, false)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			return "", fmt.Errorf("拓扑中没有 %s", section)
		}
		portKey := map[string]string{"pd": "client_port", "tiflash": "flash_service_port"}[role]
		if portKey == "" {
			portKey = "port"
		}
		for _, e := range seq.Content {
			h := mapGet(e, "host", false)
			if h == nil || h.Value != n.Host {
				continue
			}
			pv := mapGet(e, portKey, false)
			typ := role
			if role == "cdc" {
				typ = "ticdc"
			}
			port := defaultPort[typ]
			if pv != nil {
				fmt.Sscan(pv.Value, &port)
			}
			if port == n.Port {
				target = mapGet(e, "config", true)
				break
			}
		}
		if target == nil {
			return "", fmt.Errorf("拓扑中找不到实例 %s:%d", n.Host, n.Port)
		}
	}
	for _, k := range keys {
		v := fmt.Sprint(edits[k])
		setScalar(target, k, v)
	}
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	_ = enc.Close()
	return out.String(), nil
}

// mapGet 在映射节点里按键取值节点；create 为 true 时不存在则创建空映射。
func mapGet(m *yaml.Node, key string, create bool) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if create && v.Kind != yaml.MappingNode && v.Tag == "!!null" {
				*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return v
		}
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func setScalar(m *yaml.Node, key, val string) {
	tag := "!!str"
	var iv int
	if _, err := fmt.Sscan(val, &iv); err == nil && fmt.Sprint(iv) == val {
		tag = "!!int"
	}
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: val}
	if tag == "!!str" {
		node.Style = yaml.DoubleQuotedStyle
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = node
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
}

// SynthTopologyYAML 从拓扑推导一份最小的 tiup 拓扑 YAML，仅供回放 / 演示使用（真实环境必须读取 meta.yaml）。
func SynthTopologyYAML(t model.Topology) string {
	user := t.User
	if user == "" {
		user = "tidb"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "global:\n  user: %s\n  ssh_port: 22\n  deploy_dir: /tidb-deploy\n  data_dir: /tidb-data\nserver_configs:\n  tidb: {}\n  tikv: {}\n  pd: {}\n", user)
	sections := []struct{ typ, key, portKey string }{{"tidb", "tidb_servers", "port"}, {"pd", "pd_servers", "client_port"}, {"tikv", "tikv_servers", "port"}, {"tiflash", "tiflash_servers", "flash_service_port"}, {"ticdc", "cdc_servers", "port"}}
	for _, s := range sections {
		var list []model.Instance
		for _, in := range t.Instances {
			if in.Type == s.typ {
				list = append(list, in)
			}
		}
		if len(list) == 0 {
			continue
		}
		b.WriteString(s.key + ":\n")
		for _, in := range list {
			fmt.Fprintf(&b, "  - host: %s\n    %s: %d\n", in.Host, s.portKey, in.Port)
		}
	}
	return b.String()
}
