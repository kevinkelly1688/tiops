// @author: kevinkelly
// @license: Apache License 2.0
// @file: planner.go
// @time: 2026/10/28

package change

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"tidbops/internal/exec"
	"tidbops/internal/model"
)

// Ctx 是规划所需的集群上下文。规划只读，不改任何状态。
type Ctx struct {
	Cluster   string
	Env       string
	Topo      model.Topology
	Now       time.Time
	PDDownMin int       // PD 当前 max-store-down-time（分钟），默认 30
	Replicas  int       // max-replicas，默认 3
	Live      []*Change // 占用资源的其他变更（pending/approved/scheduled/running）
	Maints    []*Maint  // 全部维护记录
	// TopologyYAML 是 tiup 保存的集群拓扑（meta.yaml 的 topology 部分），edit-config 在它的基础上打补丁。
	TopologyYAML string
	// TopologySynthetic 表示 TopologyYAML 是从拓扑推导的（无法读取 meta.yaml），只能用于演示 / 回放。
	TopologySynthetic bool
	Sim               bool     // 执行器是模拟的（回放 / 演示）
	LB                bool     // 集群前有负载均衡（需要人工摘除 TiDB）；有效值 = 设置里的开关 || 有手工登记
	LBs               []string // 手工登记的负载均衡描述（名称（类型，入口）），用于在人工步骤里点名；为空则文案与登记功能引入前一致
	BackupRoots       []string // 允许的备份存储前缀；为空表示不在规划时校验（执行器仍会校验）
	// HostChecks 是主机池里「最近一次预检」的结果：ip → 是否通过。只包含最近 5 分钟内有预检记录的主机
	// （调用方负责按时间过滤；键不存在 = 没有近期预检）。扩容规划据此对「最近预检未通过」的主机给出 warn。可为 nil。
	HostChecks map[string]bool
}

func (c Ctx) pdDown() int {
	if c.PDDownMin <= 0 {
		return 30
	}
	return c.PDDownMin
}
func (c Ctx) replicas() int {
	if c.Replicas <= 0 {
		return 3
	}
	return c.Replicas
}

func (c Ctx) byID(id string) *model.Instance {
	for i := range c.Topo.Instances {
		if c.Topo.Instances[i].ID == id {
			return &c.Topo.Instances[i]
		}
	}
	return nil
}

// resolve 按实例 ID 或 host:port 找实例。
func (c Ctx) resolve(s string) *model.Instance {
	if n := c.byID(s); n != nil {
		return n
	}
	for i := range c.Topo.Instances {
		if addr(c.Topo.Instances[i]) == s {
			return &c.Topo.Instances[i]
		}
	}
	return nil
}

func addr(n model.Instance) string { return fmt.Sprintf("%s:%d", n.Host, n.Port) }

func healthy(n model.Instance) bool { return n.Status == "up" || n.Status == "warn" }

func (c Ctx) others(n model.Instance) []model.Instance {
	var out []model.Instance
	for _, x := range c.Topo.Instances {
		if x.Type == n.Type && x.ID != n.ID && healthy(x) {
			out = append(out, x)
		}
	}
	return out
}

func (c Ctx) ofType(t string) []model.Instance {
	var out []model.Instance
	for _, x := range c.Topo.Instances {
		if x.Type == t {
			out = append(out, x)
		}
	}
	return out
}

var typeLabel = map[string]string{"tidb": "TiDB", "pd": "PD", "tikv": "TiKV", "tiflash": "TiFlash", "ticdc": "TiCDC"}

// tiup 的角色名：ticdc → cdc
func tiupRole(t string) string {
	if t == "ticdc" {
		return "cdc"
	}
	return t
}

// 与 tiup 一致的停止 / 启动顺序。
var orderStop = []string{"tidb", "ticdc", "tiflash", "tikv", "pd"}
var orderStart = []string{"pd", "tikv", "tiflash", "ticdc", "tidb"}

func sortByOrder(ns []model.Instance, order []string) []model.Instance {
	out := append([]model.Instance(nil), ns...)
	idx := func(t string) int {
		for i, o := range order {
			if o == t {
				return i
			}
		}
		return len(order)
	}
	sort.SliceStable(out, func(i, j int) bool { return idx(out[i].Type) < idx(out[j].Type) })
	return out
}

func addrs(ns []model.Instance) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, addr(n))
	}
	return out
}

func ids(ns []model.Instance) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Plan 按种类规划一次变更。返回的 Plan 即使 Blocked 也包含检查结果，供界面展示。
func (c Ctx) Plan(kind string, params json.RawMessage) (Plan, error) {
	var p Plan
	var err error
	switch kind {
	case KindInstOp:
		p, err = c.planInstOp(params)
	case KindScaleOut:
		p, err = c.planScaleOut(params)
	case KindScaleIn:
		p, err = c.planScaleIn(params)
	case KindConfig:
		p, err = c.planConfig(params)
	case KindMaintEnter:
		p, err = c.planMaintEnter(params)
	case KindMaintExit:
		p, err = c.planMaintExit(params)
	case KindMaintExtend:
		p, err = c.planMaintExtend(params)
	case KindBackup:
		p, err = c.planBackup(params)
	case KindRestore:
		p, err = c.planRestore(params)
	case KindRestartRole:
		p, err = c.planRestartRole(params)
	case KindUpgrade:
		p, err = c.planUpgrade(params)
	default:
		return Plan{}, fmt.Errorf("未知的变更类型 %q", kind)
	}
	if err != nil {
		return Plan{}, err
	}
	p.Kind = kind
	c.commonChecks(&p)
	for _, ck := range p.Checks {
		if ck.Lv == "bad" {
			p.Blocked = true
		}
	}
	for _, s := range p.Steps {
		for _, cmd := range s.Cmds {
			p.Display = append(p.Display, Render(cmd))
		}
		if s.Manual != "" {
			p.Display = append(p.Display, "# 人工操作："+s.Manual)
		}
	}
	return p, nil
}

// commonChecks 是所有写变更共有的检查：资源冲突、维护中的主机、执行器状态。
func (c Ctx) commonChecks(p *Plan) {
	tset := map[string]bool{}
	for _, t := range p.Targets {
		tset[t] = true
	}
	hset := map[string]bool{}
	for _, h := range p.Hosts {
		hset[h] = true
	}
	for _, o := range c.Live {
		if o.Kind == p.Kind && o.Kind == KindMaintExit && p.Maint == nil {
			// 同一维护的结束不与自己冲突，下面按目标判断
		}
		for _, t := range o.Plan.Targets {
			if tset[t] {
				p.Checks = append(p.Checks, Check{"bad", fmt.Sprintf("%s 已被变更 %s（%s）占用，请等待其完成或取消", t, o.ID, o.Title)})
			}
		}
	}
	if c.TopologySynthetic && !c.Sim && p.Kind == KindConfig {
		p.Checks = append(p.Checks, Check{"bad", "未能读取 tiup 的 meta.yaml，无法安全地生成持久化配置；请确认平台运行用户可读 ~/.tiup/storage/cluster/clusters/<集群>/meta.yaml"})
	}
}

// ---------------------------------------------------------------- 单实例 ----

type instOpParams struct {
	Node   string `json:"node"`
	Action string `json:"action"`
}

func (c Ctx) planInstOp(raw json.RawMessage) (Plan, error) {
	var in instOpParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	n := c.resolve(in.Node)
	if n == nil {
		return Plan{}, fmt.Errorf("集群拓扑中没有实例 %q", in.Node)
	}
	label := typeLabel[n.Type]
	same := c.others(*n)
	tikv := n.Type == "tikv"
	if tikv && n.SID == 0 {
		return Plan{}, fmt.Errorf("%s 缺少 Store ID，无法驱逐 Leader（请先刷新数据）", n.ID)
	}
	p := Plan{Targets: []string{n.ID}, Hosts: []string{n.Host}, EstMins: 5, Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	if n.Status == "joining" || n.Status == "offline" {
		add("bad", fmt.Sprintf("%s 正处于「%s」状态，请等待当前变更完成", n.ID, n.Status))
	}
	if m := c.maintOn(n.Host); m != nil && in.Action != "start" {
		add("bad", fmt.Sprintf("%s 所在主机 %s 正在维护（%s），请在维护流程中操作", n.ID, n.Host, m.ID))
	}
	a := addr(*n)
	evictAdd := exec.Cmd{Kind: exec.KindPDEvictAdd, Cluster: c.Cluster, Store: int64(n.SID)}
	evictRm := exec.Cmd{Kind: exec.KindPDEvictRemove, Cluster: c.Cluster, Store: int64(n.SID)}
	evictWait := &Wait{Kind: "leader-zero", Store: int64(n.SID), Timeout: 10 * time.Minute, Interval: 5 * time.Second, Desc: "等待 Leader 迁出"}
	upWait := &Wait{Kind: "node-up", Node: a, Timeout: 10 * time.Minute, Interval: 5 * time.Second, Desc: "等待实例 Up"}
	switch in.Action {
	case "restart":
		p.Title, p.Risk = "重启 "+n.ID, "mid"
		if len(same) == 0 {
			add("bad", fmt.Sprintf("%s 仅此一个健康实例，重启期间该组件不可用", label))
		}
		if tikv {
			add("ok", "重启前先驱逐 Leader，业务抖动通常为秒级")
			if len(same) < 2 {
				add("warn", "其他健康 TiKV 少于 2 个，重启期间多数派风险升高")
			} else {
				add("ok", fmt.Sprintf("其余 %d 个 TiKV 健康，Raft 多数派不受影响", len(same)))
			}
			p.Steps = []Step{{Name: "驱逐 Leader", Cmds: []exec.Cmd{evictAdd}, Wait: evictWait},
				{Name: "重启实例", Cmds: []exec.Cmd{{Kind: exec.KindClusterRestart, Cluster: c.Cluster, Nodes: []string{a}}}, Wait: upWait},
				{Name: "移除驱逐调度，Leader 回迁", Cmds: []exec.Cmd{evictRm}, IgnoreErr: true}}
			p.Rollback = []exec.Cmd{evictRm}
		} else {
			if n.Type == "pd" && n.Leader {
				add("warn", "PD Leader 重启会触发重新选举，TSO 短暂不可用")
			}
			if n.Type == "tidb" {
				add("warn", fmt.Sprintf("该节点约 %d 个连接会断开，需应用具备重连机制；建议先从负载均衡摘除", n.Conn))
			}
			p.Steps = []Step{{Name: "重启实例", Cmds: []exec.Cmd{{Kind: exec.KindClusterRestart, Cluster: c.Cluster, Nodes: []string{a}}}, Wait: upWait}}
		}
	case "stop":
		p.Title, p.Risk = "停止 "+n.ID, "high"
		if len(same) == 0 {
			add("bad", fmt.Sprintf("%s 仅此一个健康实例，停止后该组件整体不可用", label))
		}
		if tikv {
			add("warn", fmt.Sprintf("PD 在 max-store-down-time（%d 分钟）后会把该 Store 判为 Down 并开始补副本，计划内停机请在此之前恢复", c.pdDown()))
			if len(same) < c.replicas() {
				add("bad", fmt.Sprintf("停止后健康 TiKV 少于 %d 个，部分 Region 将失去多数派", c.replicas()))
			}
		}
		if n.Type == "pd" && len(same) < 2 {
			add("bad", "停止后 PD 失去多数派，集群将不可用")
		}
		if n.Type == "tidb" && n.Conn > 0 {
			add("warn", fmt.Sprintf("约 %d 个连接会立即中断", n.Conn))
		}
		if tikv {
			p.Steps = []Step{{Name: "驱逐 Leader", Cmds: []exec.Cmd{evictAdd}, Wait: evictWait},
				{Name: "停止实例", Cmds: []exec.Cmd{{Kind: exec.KindClusterStop, Cluster: c.Cluster, Nodes: []string{a}}}}}
			p.Rollback = []exec.Cmd{evictRm}
		} else {
			p.Steps = []Step{{Name: "停止实例", Cmds: []exec.Cmd{{Kind: exec.KindClusterStop, Cluster: c.Cluster, Nodes: []string{a}}}}}
		}
	case "start":
		p.Title, p.Risk = "启动 "+n.ID, "low"
		add("ok", "启动后会自动重新加入集群；TiKV 将逐步恢复 Leader / Region 均衡")
		p.Steps = []Step{{Name: "启动实例", Cmds: []exec.Cmd{{Kind: exec.KindClusterStart, Cluster: c.Cluster, Nodes: []string{a}}}, Wait: upWait}}
		if tikv {
			p.Steps = append(p.Steps, Step{Name: "移除遗留的驱逐调度（若有）", Cmds: []exec.Cmd{evictRm}, IgnoreErr: true})
		}
	case "evict":
		if !tikv {
			return Plan{}, fmt.Errorf("只有 TiKV 支持驱逐 Leader")
		}
		p.Title, p.Risk = "驱逐 "+n.ID+" 的 Leader", "mid"
		add("ok", "Leader 会通过 TransferLeader 迁移到其他 Store，不涉及数据拷贝")
		add("warn", fmt.Sprintf("%d 个 Leader 将迁到其余 TiKV，其读写压力随之转移，请确认剩余节点余量", n.Leaders))
		p.Steps = []Step{{Name: "添加 evict-leader-scheduler", Cmds: []exec.Cmd{evictAdd}, Wait: evictWait}}
		p.Rollback = []exec.Cmd{evictRm}
	case "unevict":
		if !tikv {
			return Plan{}, fmt.Errorf("只有 TiKV 支持恢复 Leader 调度")
		}
		p.Title, p.Risk = "恢复 "+n.ID+" 的 Leader 调度", "low"
		add("ok", "移除驱逐调度后，PD 会逐步把 Leader 均衡回该 Store")
		p.Steps = []Step{{Name: "移除 evict-leader-scheduler", Cmds: []exec.Cmd{evictRm}}}
	default:
		return Plan{}, fmt.Errorf("未知的操作 %q", in.Action)
	}
	if in.Action == "stop" || in.Action == "restart" {
		p.Abortable = true
	}
	return p, nil
}

func (c Ctx) maintOn(host string) *Maint {
	for _, m := range c.Maints {
		if m.IP == host && MaintLive(m.State) {
			return m
		}
	}
	return nil
}

// ---------------------------------------------------------------- 扩容 ----

// HostSpec 是扩容的目标主机。
type HostSpec struct {
	IP   string `json:"ip"`
	AZ   string `json:"az"`
	Rack string `json:"rack"`
}

type scaleOutParams struct {
	Role  string     `json:"role"`
	Hosts []HostSpec `json:"hosts"`
	Port  int        `json:"port"`
	Cache int        `json:"cache"` // TiKV block-cache GB
	Rate  int        `json:"rate"`  // store limit，个/分钟
	LB    string     `json:"lb"`
}

var defaultPort = map[string]int{"tidb": 4000, "tikv": 20160, "pd": 2379, "tiflash": 3930, "ticdc": 8300}

func (c Ctx) planScaleOut(raw json.RawMessage) (Plan, error) {
	var in scaleOutParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	if _, ok := typeLabel[in.Role]; !ok {
		return Plan{}, fmt.Errorf("未知的角色 %q", in.Role)
	}
	if len(in.Hosts) == 0 {
		return Plan{}, fmt.Errorf("请选择至少一台目标主机")
	}
	if in.Port == 0 {
		in.Port = defaultPort[in.Role]
	}
	label := typeLabel[in.Role]
	p := Plan{Risk: "mid", EstMins: 10 + 5*len(in.Hosts), Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	seen := map[string]bool{}
	for _, h := range in.Hosts {
		if net.ParseIP(h.IP) == nil || net.ParseIP(h.IP).To4() == nil {
			add("bad", fmt.Sprintf("主机 %q 不是合法的 IPv4 地址", h.IP))
			continue
		}
		if seen[h.IP] {
			add("bad", fmt.Sprintf("主机 %s 重复", h.IP))
		}
		seen[h.IP] = true
		for _, x := range c.Topo.Instances {
			if x.Host == h.IP && x.Port == in.Port {
				add("bad", fmt.Sprintf("%s:%d 已被 %s 使用", h.IP, in.Port, x.ID))
			}
		}
		p.Hosts = append(p.Hosts, h.IP)
		if m := c.maintOn(h.IP); m != nil {
			add("bad", fmt.Sprintf("主机 %s 正在维护（%s）", h.IP, m.ID))
		}
		if ok, has := c.HostChecks[h.IP]; has && !ok {
			add("warn", fmt.Sprintf("主机 %s 最近一次预检未通过（主机池），请先在「主机池」处理预检失败项或重新预检", h.IP))
		}
	}
	if in.Role == "pd" && (len(c.ofType("pd"))+len(in.Hosts))%2 == 0 {
		add("warn", fmt.Sprintf("扩容后 PD 为 %d 个（偶数），容错能力不提升，建议保持奇数", len(c.ofType("pd"))+len(in.Hosts)))
	}
	if in.Role == "tikv" {
		if in.Cache < 0 {
			add("bad", "Block Cache 容量不合法")
		}
		azs := map[string]int{}
		for _, x := range c.ofType("tikv") {
			azs[x.AZ]++
		}
		for _, h := range in.Hosts {
			azs[h.AZ]++
		}
		if in.Rate != 0 && (in.Rate < 1 || in.Rate > 200) {
			add("bad", "store limit 取值范围 1–200")
		}
		add("info", "Region 将由 PD 逐步均衡到新 Store，期间 I/O 与网络会上升；速度由 store limit 控制")
	}
	content, err := scaleOutYAML(in, c.Topo)
	if err != nil {
		return Plan{}, err
	}
	p.Title = fmt.Sprintf("扩容 %s ×%d（%s）", label, len(in.Hosts), strings.Join(p.Hosts, "、"))
	p.Steps = []Step{{Name: "预检目标主机（tiup cluster check）", Cmds: []exec.Cmd{{Kind: exec.KindClusterCheck, Cluster: c.Cluster, Content: content}}, IgnoreErr: c.Sim}}
	if in.Role == "tikv" && in.Rate != 0 && in.Rate != 15 {
		p.Steps = append(p.Steps, Step{Name: fmt.Sprintf("调整 Region 迁移速度（store limit %d）", in.Rate), Cmds: []exec.Cmd{{Kind: exec.KindPDStoreLimit, Cluster: c.Cluster, Rate: in.Rate}}})
	}
	var waits []string
	for _, h := range in.Hosts {
		waits = append(waits, fmt.Sprintf("%s:%d", h.IP, in.Port))
	}
	scale := Step{Name: "tiup scale-out：分发组件并启动新实例", Cmds: []exec.Cmd{{Kind: exec.KindClusterScaleOut, Cluster: c.Cluster, Content: content}}, Irreversible: true}
	p.Steps = append(p.Steps, scale)
	for _, w := range waits {
		p.Steps = append(p.Steps, Step{Name: "等待 " + w + " 就绪", Wait: &Wait{Kind: "node-up", Node: w, Timeout: 15 * time.Minute, Interval: 10 * time.Second, Desc: "等待新实例 Up"}})
	}
	if in.Role == "tikv" && in.Rate != 0 && in.Rate != 15 {
		p.Steps = append(p.Steps, Step{Name: "恢复默认迁移速度（15）", Cmds: []exec.Cmd{{Kind: exec.KindPDStoreLimit, Cluster: c.Cluster, Rate: 15}}, IgnoreErr: true})
	}
	if in.Role == "tidb" {
		lb := "HAProxy"
		if in.LB == "tiproxy" {
			lb = "TiProxy"
		}
		if in.LB != "none" && in.LB != "" {
			p.Steps = append(p.Steps, Step{Name: "把新 TiDB 加入负载均衡", Manual: fmt.Sprintf("把 %s 加入 %s 后端并 reload，确认后继续", strings.Join(waits, "、"), lb)})
		}
	}
	p.Steps = append(p.Steps, Step{Name: "查看集群状态", Cmds: []exec.Cmd{{Kind: exec.KindClusterDisplay, Cluster: c.Cluster}}})
	if in.Role == "tikv" && in.Rate != 0 && in.Rate != 15 {
		p.Rollback = []exec.Cmd{{Kind: exec.KindPDStoreLimit, Cluster: c.Cluster, Rate: 15}}
	}
	return p, nil
}

// BuildScaleOutYAML 是 scaleOutYAML 的导出封装：按角色与主机生成扩容拓扑文件（与扩容变更完全相同的格式）。
// 主机池预检用它生成「只含候选主机」的最小拓扑，交给 `tiup cluster check <c> <file> --cluster`。
// role 取 tidb | tikv | pd | tiflash | ticdc；port 为 0 时用该角色的默认端口；cache 仅 TiKV 用（block-cache GB，0 = 不写）。
func BuildScaleOutYAML(role string, hosts []HostSpec, port, cache int) (string, error) {
	if _, ok := typeLabel[role]; !ok {
		return "", fmt.Errorf("未知的角色 %q", role)
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("至少需要一台主机")
	}
	if port == 0 {
		port = defaultPort[role]
	}
	return scaleOutYAML(scaleOutParams{Role: role, Hosts: hosts, Port: port, Cache: cache}, model.Topology{})
}

// scaleOutYAML 生成 scale-out 拓扑文件。格式固定、字段固定，exec 层会再次校验。
func scaleOutYAML(in scaleOutParams, topo model.Topology) (string, error) {
	key := map[string]string{"tidb": "tidb_servers", "tikv": "tikv_servers", "pd": "pd_servers", "tiflash": "tiflash_servers", "ticdc": "cdc_servers"}[in.Role]
	var b strings.Builder
	b.WriteString(key + ":\n")
	for _, h := range in.Hosts {
		fmt.Fprintf(&b, "  - host: %s\n", h.IP)
		switch in.Role {
		case "pd":
			fmt.Fprintf(&b, "    client_port: %d\n    peer_port: 2380\n", in.Port)
		case "tiflash":
			fmt.Fprintf(&b, "    flash_service_port: %d\n    tcp_port: 9000\n    http_port: 8123\n", in.Port)
		default:
			fmt.Fprintf(&b, "    port: %d\n", in.Port)
		}
		if in.Role == "tikv" {
			b.WriteString("    status_port: 20180\n")
		}
		if in.Role == "tidb" {
			b.WriteString("    status_port: 10080\n")
		}
		fmt.Fprintf(&b, "    deploy_dir: /tidb-deploy/%s-%d\n", in.Role, in.Port)
		if in.Role != "tidb" {
			fmt.Fprintf(&b, "    data_dir: /tidb-data/%s-%d\n", in.Role, in.Port)
		}
		if in.Role == "tikv" {
			rack := h.Rack
			if rack == "" || rack == "—" {
				rack = "r00"
			}
			az := h.AZ
			if az == "" || az == "—" {
				return "", fmt.Errorf("TiKV 扩容需要指定主机 %s 的可用区（zone 标签）", h.IP)
			}
			b.WriteString("    config:\n")
			fmt.Fprintf(&b, "      server.labels: { zone: \"%s\", rack: \"%s\", host: \"%s\" }\n", az, rack, strings.ReplaceAll(h.IP, ".", "-"))
			if in.Cache > 0 {
				fmt.Fprintf(&b, "      storage.block-cache.capacity: \"%dGB\"\n", in.Cache)
			}
		}
	}
	_ = topo
	return b.String(), nil
}

// ---------------------------------------------------------------- 缩容 ----

type scaleInParams struct {
	Nodes []string `json:"nodes"`
	LB    string   `json:"lb"`
}

func (c Ctx) planScaleIn(raw json.RawMessage) (Plan, error) {
	var in scaleInParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	var picks []model.Instance
	pickSet := map[string]bool{}
	for _, s := range in.Nodes {
		n := c.resolve(s)
		if n == nil {
			return Plan{}, fmt.Errorf("集群拓扑中没有实例 %q", s)
		}
		if !pickSet[n.ID] {
			pickSet[n.ID] = true
			picks = append(picks, *n)
		}
	}
	if len(picks) == 0 {
		return Plan{}, fmt.Errorf("请选择要下线的实例")
	}
	var rem []model.Instance
	for _, x := range c.Topo.Instances {
		if !pickSet[x.ID] {
			rem = append(rem, x)
		}
	}
	p := Plan{Risk: "high", Targets: ids(picks), EstMins: 30, Abortable: true}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	hosts := []string{}
	for _, n := range picks {
		hosts = append(hosts, n.Host)
		if n.Status == "joining" || n.Status == "offline" {
			add("bad", fmt.Sprintf("%s 正处于「%s」，请等待当前变更完成", n.ID, n.Status))
		}
		if m := c.maintOn(n.Host); m != nil {
			add("bad", fmt.Sprintf("%s 所在主机 %s 正在维护（%s）", n.ID, n.Host, m.ID))
		}
	}
	p.Hosts = uniq(hosts)
	roles := map[string]bool{}
	for _, n := range picks {
		roles[n.Type] = true
	}
	for _, role := range orderStop {
		if !roles[role] {
			continue
		}
		var ps, left []model.Instance
		for _, n := range picks {
			if n.Type == role {
				ps = append(ps, n)
			}
		}
		for _, n := range rem {
			if n.Type == role {
				left = append(left, n)
			}
		}
		switch role {
		case "tikv":
			if len(left) < c.replicas() {
				add("bad", fmt.Sprintf("剩余 TiKV 仅 %d 个，小于 max-replicas=%d，副本无法满足，Region 将长期缺副本", len(left), c.replicas()))
			} else {
				add("ok", fmt.Sprintf("剩余 %d 个 TiKV，仍满足 %d 副本", len(left), c.replicas()))
			}
			// zone 打散只在 TiKV 真的带了 zone 标签时才有意义：没有标签（"" / "—"）的节点不参与判断。
			// 只有「下线让可用区数从够用降到不够用」才阻断；本来就不足的只提示，不拦。
			zonesOf := func(ns []model.Instance) map[string]bool {
				z := map[string]bool{}
				for _, n := range ns {
					if n.AZ != "" && n.AZ != "—" {
						z[n.AZ] = true
					}
				}
				return z
			}
			before, after := zonesOf(c.ofType("tikv")), zonesOf(left)
			var lost []string
			for az := range before {
				if !after[az] {
					lost = append(lost, az)
				}
			}
			sort.Strings(lost)
			if len(lost) > 0 {
				switch {
				case len(after) >= c.replicas():
					add("ok", fmt.Sprintf("%s 将没有 TiKV，但剩余 %d 个可用区仍满足 %d 副本按 zone 打散", strings.Join(lost, "、"), len(after), c.replicas()))
				case len(before) >= c.replicas():
					add("bad", fmt.Sprintf("%s 将没有 TiKV，可用区数由 %d 降到 %d，小于 max-replicas=%d，副本无法再按 zone 打散", strings.Join(lost, "、"), len(before), len(after), c.replicas()))
				default:
					add("warn", fmt.Sprintf("%s 将没有 TiKV：集群本来只有 %d 个带标签的可用区（小于 max-replicas=%d），副本本就不能完全按 zone 打散，这次下线不改变这一点", strings.Join(lost, "、"), len(before), c.replicas()))
				}
			}
			if len(ps) > 1 {
				add("warn", "一次下线多个 TiKV：Region 迁移量叠加，建议逐个下线，待 Tombstone 后再继续")
			}
			var moveGB float64
			for _, n := range ps {
				moveGB += n.UsedGB
				if n.SID == 0 {
					add("bad", fmt.Sprintf("%s 缺少 Store ID，无法驱逐 Leader", n.ID))
				}
			}
			var leftCap, leftUsed float64
			for _, n := range left {
				leftCap += n.CapacityGB
				leftUsed += n.UsedGB
			}
			if leftCap > 0 {
				pct := (leftUsed + moveGB) / leftCap * 100
				switch {
				case pct >= 80:
					add("bad", fmt.Sprintf("迁入数据后剩余 TiKV 磁盘约 %.0f%%，超过 80%% 水位，PD 将限制向其调度，存在写满风险；请先扩容", pct))
				case pct >= 70:
					add("warn", fmt.Sprintf("迁入数据后剩余 TiKV 磁盘约 %.0f%%，余量偏小", pct))
				default:
					add("ok", fmt.Sprintf("迁入数据后剩余 TiKV 磁盘约 %.0f%%，容量充足", pct))
				}
				p.EstMins += int(moveGB/200) + 10
			}
		case "pd":
			switch {
			case len(left) < 3:
				add("bad", fmt.Sprintf("缩容后 PD 仅剩 %d 个，无法容忍任何节点故障（推荐保持 3 或 5）", len(left)))
			case len(left)%2 == 0:
				add("warn", fmt.Sprintf("缩容后 PD 为 %d 个（偶数），容错能力与 %d 个相同，建议保持奇数", len(left), len(left)-1))
			default:
				add("ok", fmt.Sprintf("缩容后 PD 为 %d 个（奇数），仍可容忍 %d 个节点故障", len(left), (len(left)-1)/2))
			}
			for _, n := range ps {
				if n.Leader {
					add("warn", "PD Leader 在下线范围内，将先执行 member leader transfer，TSO 会有秒级抖动")
				}
			}
		case "tidb":
			if len(left) == 0 {
				add("bad", "这是最后一个 TiDB，下线后集群将无法提供 SQL 服务")
			} else {
				if len(left) == 1 {
					add("warn", "缩容后只剩 1 个 TiDB，成为单点")
				}
				conns := 0
				for _, n := range ps {
					conns += n.Conn
				}
				add("info", fmt.Sprintf("约 %d 个连接会在摘除后断开并重连，请先从负载均衡摘除并等待排空", conns))
			}
		case "tiflash":
			if len(left) == 0 {
				add("bad", "这是最后一个 TiFlash，下线后依赖列存 / MPP 的查询将失败或回退到 TiKV")
			} else if len(left) < 2 {
				add("warn", "表的 TiFlash 副本数为 2 时，剩余节点不足；需先 ALTER TABLE ... SET TIFLASH REPLICA 1")
			} else {
				add("ok", fmt.Sprintf("剩余 %d 个 TiFlash，可满足 2 副本", len(left)))
			}
		case "ticdc":
			if len(left) == 0 {
				add("bad", "这是最后一个 TiCDC，下线后所有 changefeed 将中断")
			} else {
				add("info", fmt.Sprintf("changefeed 会迁移到剩余 %d 个 capture，期间同步延迟会短暂上升", len(left)))
			}
		}
		// 步骤
		switch role {
		case "tidb":
			if c.LB || in.LB == "haproxy" || in.LB == "tiproxy" {
				p.Steps = append(p.Steps, Step{Name: "从负载均衡摘除并等待连接排空", Manual: c.lbRemoveText(addrs(ps))})
			}
			p.Steps = append(p.Steps, Step{Name: "下线 TiDB", Cmds: []exec.Cmd{{Kind: exec.KindClusterScaleIn, Cluster: c.Cluster, Nodes: addrs(ps)}}, Irreversible: true})
		case "tikv":
			for _, n := range ps {
				p.Steps = append(p.Steps, Step{Name: "驱逐 Leader · " + n.ID, Cmds: []exec.Cmd{{Kind: exec.KindPDEvictAdd, Cluster: c.Cluster, Store: int64(n.SID)}},
					Wait: &Wait{Kind: "leader-zero", Store: int64(n.SID), Timeout: 15 * time.Minute, Interval: 5 * time.Second, Desc: "等待 Leader 迁出"}})
				p.Rollback = append(p.Rollback, exec.Cmd{Kind: exec.KindPDEvictRemove, Cluster: c.Cluster, Store: int64(n.SID)})
			}
			p.Steps = append(p.Steps, Step{Name: "tiup scale-in：Store 进入 Offline", Cmds: []exec.Cmd{{Kind: exec.KindClusterScaleIn, Cluster: c.Cluster, Nodes: addrs(ps)}}, Irreversible: true})
			for _, n := range ps {
				p.Steps = append(p.Steps, Step{Name: "等待 Region 迁出 · " + n.ID, Wait: &Wait{Kind: "store-state", Store: int64(n.SID), Want: "Tombstone", Timeout: 48 * time.Hour, Interval: 30 * time.Second, Desc: "等待 Store 变为 Tombstone"}})
			}
			p.Steps = append(p.Steps, Step{Name: "清理 Tombstone 残留（tiup cluster prune）", Cmds: []exec.Cmd{{Kind: exec.KindClusterPrune, Cluster: c.Cluster}}})
		case "pd":
			for _, n := range ps {
				if n.Leader {
					var other *model.Instance
					for i := range rem {
						if rem[i].Type == "pd" {
							other = &rem[i]
							break
						}
					}
					if other != nil {
						p.Steps = append(p.Steps, Step{Name: "迁移 PD Leader", Cmds: []exec.Cmd{{Kind: exec.KindPDLeaderTransfer, Cluster: c.Cluster, Name: pdMember(*other)}}})
					}
				}
			}
			p.Steps = append(p.Steps, Step{Name: "下线 PD", Cmds: []exec.Cmd{{Kind: exec.KindClusterScaleIn, Cluster: c.Cluster, Nodes: addrs(ps)}}, Irreversible: true})
		default:
			p.Steps = append(p.Steps, Step{Name: "下线 " + typeLabel[role], Cmds: []exec.Cmd{{Kind: exec.KindClusterScaleIn, Cluster: c.Cluster, Nodes: addrs(ps)}}, Irreversible: true})
		}
	}
	p.Steps = append(p.Steps, Step{Name: "查看集群状态", Cmds: []exec.Cmd{{Kind: exec.KindClusterDisplay, Cluster: c.Cluster}}})
	p.Title = "缩容 " + strings.Join(ids(picks), "、")
	return p, nil
}

// pdMember 返回 tiup 部署的 PD 成员名。
func pdMember(n model.Instance) string { return fmt.Sprintf("pd-%s-%d", n.Host, n.Port) }

// Render 把结构化命令渲染成人类可读的命令行（仅用于展示与审计，不会被执行）。
func Render(c exec.Cmd) string {
	switch c.Kind {
	case exec.KindClusterUpgrade:
		return fmt.Sprintf("tiup cluster upgrade %s %s", c.Cluster, c.Value)
	case exec.KindClusterRestart, exec.KindClusterStop, exec.KindClusterStart, exec.KindClusterScaleIn:
		if len(c.Nodes) == 0 && c.Role != "" {
			return fmt.Sprintf("tiup cluster restart %s -R %s", c.Cluster, c.Role)
		}
		verb := strings.TrimPrefix(c.Kind, "cluster.")
		return fmt.Sprintf("tiup cluster %s %s -N %s", verb, c.Cluster, strings.Join(c.Nodes, ","))
	case exec.KindClusterScaleOut:
		return fmt.Sprintf("tiup cluster scale-out %s scale-out.yml -y", c.Cluster)
	case exec.KindClusterCheck:
		if c.Content != "" {
			return fmt.Sprintf("tiup cluster check %s scale-out.yml --cluster", c.Cluster)
		}
		return fmt.Sprintf("tiup cluster check %s --cluster", c.Cluster)
	case exec.KindClusterDisplay:
		return "tiup cluster display " + c.Cluster
	case exec.KindClusterPrune:
		return "tiup cluster prune " + c.Cluster
	case exec.KindClusterEditCfg:
		return fmt.Sprintf("tiup cluster edit-config %s --topology-file <平台生成并 diff 过的拓扑>", c.Cluster)
	case exec.KindClusterReload:
		if len(c.Nodes) > 0 {
			return fmt.Sprintf("tiup cluster reload %s -N %s", c.Cluster, strings.Join(c.Nodes, ","))
		}
		return fmt.Sprintf("tiup cluster reload %s -R %s", c.Cluster, c.Role)
	case exec.KindPDConfigSet:
		return fmt.Sprintf("pd-ctl config set %s %s", c.Key, c.Value)
	case exec.KindPDEvictAdd:
		return fmt.Sprintf("pd-ctl scheduler add evict-leader-scheduler %d", c.Store)
	case exec.KindPDEvictRemove:
		return fmt.Sprintf("pd-ctl scheduler remove evict-leader-scheduler-%d", c.Store)
	case exec.KindPDStoreLimit:
		return fmt.Sprintf("pd-ctl store limit all %d", c.Rate)
	case exec.KindPDLeaderTransfer:
		return "pd-ctl member leader transfer " + c.Name
	case exec.KindPDQuery:
		return "pd-ctl " + c.Key
	case exec.KindCDCCaptures:
		return "tiup cdc cli capture list"
	case exec.KindSQLSetConfig:
		target := c.Role
		if c.Instance != "" {
			target = `"` + c.Instance + `"`
		}
		return fmt.Sprintf("SET CONFIG %s `%s` = '%s';", target, c.Key, c.Value)
	case exec.KindSQLSetGlobal:
		return fmt.Sprintf("SET GLOBAL %s = '%s';", c.Key, c.Value)
	case exec.KindBRBackupFull:
		return "tiup br backup full --storage " + c.Value
	case exec.KindBRRestoreFull:
		return "tiup br restore full --storage " + c.Value
	case exec.KindBRLogStart:
		return fmt.Sprintf("tiup br log start --task-name %s --storage %s", c.Name, c.Value)
	case exec.KindBRLogStop:
		return "tiup br log stop --task-name " + c.Name
	case exec.KindBRLogStatus:
		return "tiup br log status"
	}
	return c.Kind
}

// lbRemoveText 是「从负载均衡摘除」人工步骤的说明。有手工登记时点名具体入口（名称、类型、地址），多条时全部列出；
// 没有登记时与登记功能引入前的文案完全一致。
func (c Ctx) lbRemoveText(targets []string) string {
	t := strings.Join(targets, "、")
	if len(c.LBs) == 0 {
		return fmt.Sprintf("把 %s 从负载均衡后端摘除，并等待连接排空，确认后继续", t)
	}
	return fmt.Sprintf("先从负载均衡 %s 摘除 %s，并等待连接排空，确认后继续", strings.Join(c.LBs, "、"), t)
}

// lbAddBackText 是维护结束后「重新加入负载均衡」人工步骤的说明（点名规则同 lbRemoveText）。
func (c Ctx) lbAddBackText(targets []string) string {
	t := strings.Join(targets, "、")
	if len(c.LBs) == 0 {
		return "把 " + t + " 加回负载均衡后端并确认流量恢复"
	}
	return "把 " + t + " 加回负载均衡 " + strings.Join(c.LBs, "、") + " 的后端并确认流量恢复"
}
