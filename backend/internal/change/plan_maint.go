// @author: kevinkelly
// @license: Apache License 2.0
// @file: plan_maint.go
// @time: 2026/10/28

package change

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tidbops/internal/exec"
	"tidbops/internal/model"
)

// MaintType 是维护类型。
type MaintType struct {
	Label string
	Def   int
	Todo  []string
}

// MaintTypes 与前端目录一致。
var MaintTypes = map[string]MaintType{
	"kernel":   {"内核 / OS 升级", 25, []string{"升级内核 / 系统包并重启主机", "确认 THP、Swap、sysctl 等参数未被还原", "确认时间同步（chrony / ntp）正常"}},
	"disk":     {"磁盘更换 / 扩容", 90, []string{"更换或扩容磁盘并重新挂载（nodelalloc,noatime）", "确认数据目录权限、空间与文件系统", "抽样校验数据目录文件完整"}},
	"hardware": {"硬件检修（内存 / 电源 / 网卡）", 120, []string{"完成硬件检修并上电", "确认 BIOS 电源策略为高性能", "确认网卡速率与双工模式"}},
	"net":      {"网络变更", 30, []string{"完成网络变更", "验证到 PD / TiKV / TiDB 各节点互通", "确认跨机房时延在阈值内（< 5ms）"}},
	"reboot":   {"仅重启主机", 10, []string{"重启主机", "确认系统服务与 tiup 目录挂载正常"}},
}

type maintEnterParams struct {
	IP       string `json:"ip"`
	Type     string `json:"type"`
	Mins     int    `json:"mins"`
	KeepData *bool  `json:"keepData"`
	Reason   string `json:"reason"`
	Strategy string `json:"strategy"` // auto | standard | extend
	LB       string `json:"lb"`
}

func (c Ctx) hostInsts(ip string) []model.Instance {
	var out []model.Instance
	for _, n := range c.Topo.Instances {
		if n.Host == ip {
			out = append(out, n)
		}
	}
	return out
}

func (c Ctx) planMaintEnter(raw json.RawMessage) (Plan, error) {
	var in maintEnterParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	mt, ok := MaintTypes[in.Type]
	if !ok {
		return Plan{}, fmt.Errorf("未知的维护类型 %q", in.Type)
	}
	if in.Mins <= 0 {
		in.Mins = mt.Def
	}
	if in.Mins > 24*60 {
		return Plan{}, fmt.Errorf("计划时长不能超过 24 小时")
	}
	keep := in.KeepData == nil || *in.KeepData
	ns := c.hostInsts(in.IP)
	if len(ns) == 0 {
		return Plan{}, fmt.Errorf("主机 %s 上没有实例", in.IP)
	}
	thr := c.pdDown()
	var tikvs, tidbs, pdl, cdcs, flashes []model.Instance
	for _, n := range ns {
		switch n.Type {
		case "tikv":
			tikvs = append(tikvs, n)
		case "tidb":
			tidbs = append(tidbs, n)
		case "pd":
			if n.Leader {
				pdl = append(pdl, n)
			}
		case "ticdc":
			cdcs = append(cdcs, n)
		case "tiflash":
			flashes = append(flashes, n)
		}
	}
	hasTikv := len(tikvs) > 0
	p := Plan{Risk: "high", Targets: ids(ns), Hosts: []string{in.IP}, EstMins: in.Mins + 10, Abortable: true}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	for _, m := range c.Maints {
		if !MaintLive(m.State) || m.IP == in.IP {
			if MaintLive(m.State) && m.IP == in.IP {
				add("bad", fmt.Sprintf("主机 %s 已有进行中的维护 %s", in.IP, m.ID))
			}
			continue
		}
		if hasTikv && m.HasTiKV {
			add("bad", fmt.Sprintf("主机 %s 的 TiKV 正在维护：同一时刻只允许一台 TiKV 主机维护，否则部分 Region 会同时失去 2 个副本", m.IP))
		} else {
			add("warn", fmt.Sprintf("主机 %s 也在维护中，请确认两台同时下线不影响多数派与服务容量", m.IP))
		}
	}
	for _, n := range ns {
		if n.Status == "joining" || n.Status == "offline" {
			add("bad", fmt.Sprintf("%s 处于「%s」，请等待完成后再维护", n.ID, n.Status))
		}
		var left []model.Instance
		for _, x := range c.Topo.Instances {
			if x.Host != in.IP && x.Type == n.Type && healthy(x) {
				left = append(left, x)
			}
		}
		switch n.Type {
		case "tidb":
			if len(left) == 0 {
				add("bad", fmt.Sprintf("%s 是唯一健康的 TiDB，维护期间 SQL 服务将中断", n.ID))
			} else {
				add("info", fmt.Sprintf("%s：约 %d 个连接会被排空并重连；其余 %d 个 TiDB 承接流量", n.ID, n.Conn, len(left)))
			}
		case "pd":
			if len(left) < 2 {
				add("bad", fmt.Sprintf("停掉 %s 后 PD 仅剩 %d 个，失去多数派，集群将不可用", n.ID, len(left)))
			} else {
				add("ok", fmt.Sprintf("停掉 %s 后 PD 仍有 %d 个健康成员，保持多数派", n.ID, len(left)))
			}
			if n.Leader {
				add("warn", fmt.Sprintf("%s 是 PD Leader，将先 transfer leader，TSO 有秒级抖动", n.ID))
			}
		case "tikv":
			if len(left) < c.replicas() {
				add("bad", fmt.Sprintf("停掉 %s 后健康 TiKV 仅 %d 个，小于 %d，副本无法满足多数派", n.ID, len(left), c.replicas()))
			} else {
				add("ok", fmt.Sprintf("停掉 %s 后仍有 %d 个健康 TiKV，Raft 多数派不受影响", n.ID, len(left)))
			}
			add("info", fmt.Sprintf("%s：将驱逐 %d 个 Leader；约 %d 个 Region 在维护期间只有 %d 个副本（仍可读写，但容错下降）", n.ID, n.Leaders, n.Regions, c.replicas()-1))
			if n.SID == 0 {
				add("bad", fmt.Sprintf("%s 缺少 Store ID，无法驱逐 Leader", n.ID))
			}
			for _, x := range left {
				if x.Disk >= 80 {
					add("warn", fmt.Sprintf("%s 磁盘 ≥ 80%%，若维护超时触发补副本，其承接数据的能力受限", x.ID))
				}
			}
		case "tiflash":
			if len(left) == 0 {
				add("warn", fmt.Sprintf("%s 是唯一的 TiFlash，维护期间列存 / MPP 查询将回退到 TiKV 或失败", n.ID))
			} else {
				add("ok", fmt.Sprintf("其余 %d 个 TiFlash 可继续提供列存副本", len(left)))
			}
		case "ticdc":
			if len(left) == 0 {
				add("warn", fmt.Sprintf("%s 是唯一的 TiCDC，维护期间 changefeed 会中断，同步延迟将增长约 %d 分钟", n.ID, in.Mins))
			} else {
				add("info", fmt.Sprintf("%s 的 changefeed 将迁移到其余 %d 个 capture", n.ID, len(left)))
			}
		}
	}
	strategy := in.Strategy
	if strategy == "" || strategy == "auto" {
		strategy = "standard"
		if hasTikv && in.Mins > thr-5 {
			strategy = "extend"
		}
	}
	if !hasTikv {
		strategy = "standard"
	}
	limit := thr
	if strategy == "extend" {
		limit = in.Mins + 30
		if limit < thr {
			limit = thr
		}
	}
	if hasTikv {
		if strategy == "standard" && in.Mins > thr-5 {
			add("bad", fmt.Sprintf("计划 %d 分钟已接近 / 超过 PD 的 max-store-down-time（%d 分钟），超时后 PD 会补副本：请选择「临时调大阈值」策略", in.Mins, thr))
		} else if strategy == "extend" {
			add("warn", fmt.Sprintf("计划 %d 分钟接近 / 超过 max-store-down-time（%d 分钟）：将临时调大到 %d 分钟，结束后恢复", in.Mins, thr, limit))
		} else {
			add("ok", fmt.Sprintf("计划 %d 分钟小于 max-store-down-time（%d 分钟），不会触发补副本", in.Mins, thr))
		}
	}
	if !keep {
		add("bad", fmt.Sprintf("「%s」会丢失数据盘数据：这已不是「停机 → 恢复」，请改走「缩容下线 + 扩容新增」替换节点的流程", mt.Label))
	}
	if h := c.Now.Hour(); h >= 1 && h <= 3 {
		add("warn", "当前时间接近每日 02:00 的全量备份窗口，建议错开")
	}

	spec := &MaintSpec{IP: in.IP, Type: in.Type, Mins: in.Mins, KeepData: keep, Reason: in.Reason, Strategy: strategy, OrigThr: thr, Limit: limit, Nodes: ids(ns), HasTiKV: hasTikv}
	for _, n := range tikvs {
		spec.TiKVStore = append(spec.TiKVStore, int64(n.SID))
	}
	p.Maint = spec
	p.Title = fmt.Sprintf("进入维护 %s（%s）", in.IP, mt.Label)

	if strategy == "extend" {
		p.Steps = append(p.Steps, Step{Name: fmt.Sprintf("临时调大 max-store-down-time：%dm → %dm", thr, limit),
			Cmds: []exec.Cmd{{Kind: exec.KindPDConfigSet, Cluster: c.Cluster, Key: "schedule.max-store-down-time", Value: fmt.Sprintf("%dm", limit)}}})
		p.Rollback = append(p.Rollback, exec.Cmd{Kind: exec.KindPDConfigSet, Cluster: c.Cluster, Key: "schedule.max-store-down-time", Value: fmt.Sprintf("%dm", thr)})
	}
	if len(tidbs) > 0 && (c.LB || in.LB == "haproxy" || in.LB == "tiproxy") {
		p.Steps = append(p.Steps, Step{Name: "从负载均衡摘除并排空连接", Manual: c.lbRemoveText(addrs(tidbs))})
	}
	for _, n := range pdl {
		for _, o := range c.ofType("pd") {
			if o.Host != in.IP && healthy(o) {
				p.Steps = append(p.Steps, Step{Name: "迁移 PD Leader", Cmds: []exec.Cmd{{Kind: exec.KindPDLeaderTransfer, Cluster: c.Cluster, Name: pdMember(o)}}})
				break
			}
		}
		_ = n
	}
	if len(cdcs) > 0 {
		p.Steps = append(p.Steps, Step{Name: "确认 changefeed 已迁移到其他 capture", Cmds: []exec.Cmd{{Kind: exec.KindCDCCaptures, Cluster: c.Cluster}}, IgnoreErr: true})
	}
	for _, n := range tikvs {
		p.Steps = append(p.Steps, Step{Name: "驱逐 Leader · " + n.ID, Cmds: []exec.Cmd{{Kind: exec.KindPDEvictAdd, Cluster: c.Cluster, Store: int64(n.SID)}},
			Wait: &Wait{Kind: "leader-zero", Store: int64(n.SID), Timeout: 15 * time.Minute, Interval: 5 * time.Second, Desc: "等待 Leader 迁出"}})
		p.Rollback = append(p.Rollback, exec.Cmd{Kind: exec.KindPDEvictRemove, Cluster: c.Cluster, Store: int64(n.SID)})
	}
	_ = flashes
	order := sortByOrder(ns, orderStop)
	p.Steps = append(p.Steps, Step{Name: fmt.Sprintf("停止实例（%s）", strings.Join(ids(order), " → ")),
		Cmds: []exec.Cmd{{Kind: exec.KindClusterStop, Cluster: c.Cluster, Nodes: addrs(order)}}, Irreversible: true})
	return p, nil
}

type maintExitParams struct {
	MaintID string `json:"maintId"`
	Urgent  bool   `json:"urgent"`
	LB      string `json:"lb"`
}

func (c Ctx) findMaint(id string) *Maint {
	for _, m := range c.Maints {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (c Ctx) planMaintExit(raw json.RawMessage) (Plan, error) {
	var in maintExitParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	m := c.findMaint(in.MaintID)
	if m == nil {
		return Plan{}, fmt.Errorf("找不到维护记录 %q", in.MaintID)
	}
	if m.State != "active" {
		return Plan{}, fmt.Errorf("维护 %s 当前状态为 %s，只有「维护中」可以结束", m.ID, m.State)
	}
	var ns []model.Instance
	for _, id := range m.Nodes {
		if n := c.byID(id); n != nil {
			ns = append(ns, *n)
		}
	}
	if len(ns) == 0 {
		return Plan{}, fmt.Errorf("维护记录里的实例已不在拓扑中")
	}
	p := Plan{Risk: "mid", Targets: ids(ns), Hosts: []string{m.IP}, EstMins: 15, Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	var undone []string
	for _, t := range m.Todo {
		if !t.Done {
			undone = append(undone, t.T)
		}
	}
	if len(undone) > 0 && !in.Urgent {
		add("bad", fmt.Sprintf("检查单还有 %d 项未勾选：%s（紧急恢复可跳过）", len(undone), strings.Join(undone, "；")))
	}
	if in.Urgent {
		add("warn", "紧急恢复：跳过检查单，直接启动实例；请在恢复后补做主机预检")
	}
	if m.Alerted {
		add("warn", fmt.Sprintf("维护已超过 PD 阈值（%d 分钟），PD 已为该 Store 补副本；恢复后会有额外的 Region 均衡", m.Limit))
	}
	// 「无 down-peer」是 Region 副本的验证：只有主机上有 TiKV / TiFlash 才有意义。只有 tidb / pd / ticdc 的主机不加这一步，
	// 否则中控机缺 ctl 组件之类与本次维护无关的问题会让已经恢复的维护单失败。
	verifyRegions := len(filterType(ns, "tikv")) > 0 || len(filterType(ns, "tiflash")) > 0
	if verifyRegions {
		add("ok", "先预检主机、再按 pd → tikv → tiflash → ticdc → tidb 顺序启动，最后做 Region 健康验证")
	} else {
		add("ok", "先预检主机、再按 pd → tikv → tiflash → ticdc → tidb 顺序启动（主机上没有 TiKV / TiFlash，不做 Region 健康验证）")
	}
	p.Title = "结束维护 " + m.IP
	p.Maint = &MaintSpec{IP: m.IP}
	if !in.Urgent {
		p.Steps = append(p.Steps, Step{Name: "主机预检（tiup cluster check：重启后参数是否还原）", Cmds: []exec.Cmd{{Kind: exec.KindClusterCheck, Cluster: c.Cluster}}, IgnoreErr: true})
	}
	order := sortByOrder(ns, orderStart)
	p.Steps = append(p.Steps, Step{Name: fmt.Sprintf("按依赖顺序启动实例（%s）", strings.Join(ids(order), " → ")), Cmds: []exec.Cmd{{Kind: exec.KindClusterStart, Cluster: c.Cluster, Nodes: addrs(order)}}})
	for _, n := range order {
		p.Steps = append(p.Steps, Step{Name: "等待 " + n.ID + " 就绪", Wait: &Wait{Kind: "node-up", Node: addr(n), Timeout: 15 * time.Minute, Interval: 10 * time.Second, Desc: "等待实例 Up"}})
	}
	for _, n := range ns {
		if n.Type == "tikv" && n.SID > 0 {
			p.Steps = append(p.Steps, Step{Name: "移除 Leader 驱逐调度 · " + n.ID, Cmds: []exec.Cmd{{Kind: exec.KindPDEvictRemove, Cluster: c.Cluster, Store: int64(n.SID)}}, IgnoreErr: true})
		}
	}
	if m.Strategy == "extend" {
		p.Steps = append(p.Steps, Step{Name: fmt.Sprintf("恢复 max-store-down-time = %dm", m.OrigThr),
			Cmds: []exec.Cmd{{Kind: exec.KindPDConfigSet, Cluster: c.Cluster, Key: "schedule.max-store-down-time", Value: fmt.Sprintf("%dm", m.OrigThr)}}})
	}
	for _, n := range ns {
		if n.Type == "tidb" && (c.LB || in.LB == "haproxy" || in.LB == "tiproxy") {
			p.Steps = append(p.Steps, Step{Name: "重新加入负载均衡并恢复流量", Manual: c.lbAddBackText(addrs(filterType(ns, "tidb")))})
			break
		}
	}
	if verifyRegions {
		p.Steps = append(p.Steps, Step{Name: "验证：无 down-peer", Cmds: []exec.Cmd{{Kind: exec.KindPDQuery, Cluster: c.Cluster, Key: "region-down-peer"}}, Wait: &Wait{Kind: "no-down-peers", Timeout: 20 * time.Minute, Interval: 15 * time.Second, Desc: "等待无 down-peer / pending-peer"}})
	}
	return p, nil
}

func filterType(ns []model.Instance, t string) []model.Instance {
	var out []model.Instance
	for _, n := range ns {
		if n.Type == t {
			out = append(out, n)
		}
	}
	return out
}

type maintExtendParams struct {
	MaintID string `json:"maintId"`
	Add     int    `json:"add"`
}

func (c Ctx) planMaintExtend(raw json.RawMessage) (Plan, error) {
	var in maintExtendParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	m := c.findMaint(in.MaintID)
	if m == nil {
		return Plan{}, fmt.Errorf("找不到维护记录 %q", in.MaintID)
	}
	if m.State != "active" {
		return Plan{}, fmt.Errorf("只有「维护中」的记录可以延长窗口")
	}
	if in.Add <= 0 || in.Add > 24*60 {
		return Plan{}, fmt.Errorf("延长时长不合法")
	}
	newEnd := m.Mins + m.ExtendedBy + in.Add
	need := m.HasTiKV && m.Limit < newEnd
	p := Plan{Risk: "low", Targets: m.Nodes, Hosts: []string{m.IP}, EstMins: 1, Title: fmt.Sprintf("延长维护窗口 +%d 分钟（%s）", in.Add, m.IP)}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	if m.Alerted {
		add("warn", "PD 已判定 Store Down 并开始补副本，延长窗口无法撤销，只能延后恢复时间")
	}
	if need {
		add("warn", fmt.Sprintf("新窗口 %d 分钟超过当前 PD 阈值 %d 分钟，需同步调大 max-store-down-time 至 %dm", newEnd, m.Limit, newEnd+15))
		p.Steps = []Step{{Name: "调大 PD 阈值", Cmds: []exec.Cmd{{Kind: exec.KindPDConfigSet, Cluster: c.Cluster, Key: "schedule.max-store-down-time", Value: fmt.Sprintf("%dm", newEnd+15)}}}}
	} else {
		add("ok", "新窗口仍在 PD 阈值之内，无需调整 PD 配置")
		p.Steps = []Step{{Name: "更新维护窗口"}}
	}
	return p, nil
}

// ---------------------------------------------------------------- 备份恢复 ----

type backupParams struct {
	Mode    string `json:"mode"` // full | log-start | log-stop
	Storage string `json:"storage"`
	Task    string `json:"task"`
}

// storageOK 判断备份存储路径是否在白名单前缀内。
func (c Ctx) storageOK(p string) bool {
	if len(c.BackupRoots) == 0 {
		return true
	}
	if strings.Contains(p, "..") {
		return false
	}
	for _, r := range c.BackupRoots {
		if strings.HasPrefix(p, r) {
			return true
		}
	}
	return false
}

func (c Ctx) planBackup(raw json.RawMessage) (Plan, error) {
	var in backupParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	p := Plan{Risk: "mid", EstMins: 60, Abortable: false}
	add := func(lv, t string) { p.Checks = append(p.Checks, Check{lv, t}) }
	if in.Storage != "" && !c.storageOK(in.Storage) {
		add("bad", fmt.Sprintf("备份存储路径不在允许的前缀内（%s）", strings.Join(c.BackupRoots, "、")))
	}
	switch in.Mode {
	case "full":
		if in.Storage == "" {
			return Plan{}, fmt.Errorf("请指定备份存储路径")
		}
		p.Title = "全量备份 → " + in.Storage
		add("info", "BR 全量备份会读取全部数据，建议在业务低峰执行并限速（--ratelimit）")
		add("warn", "备份期间 TiKV 读 I/O 与网络会升高；避开每日 02:00 的计划备份窗口")
		p.Steps = []Step{{Name: "BR 全量备份", Cmds: []exec.Cmd{{Kind: exec.KindBRBackupFull, Cluster: c.Cluster, Value: in.Storage}}}}
	case "log-start":
		if in.Storage == "" || in.Task == "" {
			return Plan{}, fmt.Errorf("请指定日志备份任务名与存储路径")
		}
		p.Title, p.EstMins = "启动日志备份（PITR）"+in.Task, 5
		add("info", "日志备份会持续把变更日志写入存储，需保证存储容量与网络")
		p.Steps = []Step{{Name: "启动日志备份任务", Cmds: []exec.Cmd{{Kind: exec.KindBRLogStart, Cluster: c.Cluster, Name: in.Task, Value: in.Storage}}}, {Name: "查看日志备份状态", Cmds: []exec.Cmd{{Kind: exec.KindBRLogStatus, Cluster: c.Cluster}}}}
	case "log-stop":
		if in.Task == "" {
			return Plan{}, fmt.Errorf("请指定日志备份任务名")
		}
		p.Title, p.EstMins, p.Risk = "停止日志备份（PITR）"+in.Task, 5, "high"
		add("warn", "停止后将无法再恢复到停止之后的任意时间点")
		p.Steps = []Step{{Name: "停止日志备份任务", Cmds: []exec.Cmd{{Kind: exec.KindBRLogStop, Cluster: c.Cluster, Name: in.Task}}}}
	default:
		return Plan{}, fmt.Errorf("未知的备份模式 %q", in.Mode)
	}
	return p, nil
}

func (c Ctx) planRestore(raw json.RawMessage) (Plan, error) {
	var in backupParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return Plan{}, fmt.Errorf("参数不合法: %w", err)
	}
	if in.Storage == "" {
		return Plan{}, fmt.Errorf("请指定备份存储路径")
	}
	p := Plan{Risk: "high", EstMins: 120, Title: "从备份恢复 ← " + in.Storage}
	if !c.storageOK(in.Storage) {
		p.Checks = append(p.Checks, Check{"bad", fmt.Sprintf("备份存储路径不在允许的前缀内（%s）", strings.Join(c.BackupRoots, "、"))})
	}
	p.Checks = append(p.Checks, []Check{
		{"warn", "BR 恢复要求目标集群为空（或目标库表不存在）；恢复会占用大量 I/O 与网络，期间集群性能显著下降"},
		{"warn", "建议先在隔离的演练集群上做恢复演练，确认备份可用后再对生产执行"},
	}...)
	p.Steps = []Step{{Name: "BR 全量恢复", Cmds: []exec.Cmd{{Kind: exec.KindBRRestoreFull, Cluster: c.Cluster, Value: in.Storage}}, Irreversible: true}}
	return p, nil
}
