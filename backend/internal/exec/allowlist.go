// @author: kevinkelly
// @license: Apache License 2.0
// @file: allowlist.go
// @time: 2026/10/28

package exec

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 命令白名单与参数校验（A3 §3.1）。
//
// 校验分四层，任何一层失败都拒绝：
//  1. Kind 必须已登记；该 Kind 不使用的字段必须为空（防止「夹带」参数）；
//  2. 所有字符串字段不得含空白 / 控制字符 / shell 元字符 / 引号 / 反斜杠 / 非 ASCII；
//  3. 集群名 ∈ Policy.Clusters 且格式合法；节点、实例、角色等必须来自拓扑或枚举；
//  4. 值按配置目录 Normalize、备份路径按 Policy.BackupRoots 前缀、拓扑 YAML 按 yamlcheck 校验。

// KindInfo 描述一个命令种类的静态属性。
type KindInfo struct {
	Known   bool          // 是否已登记；未登记的 Kind 按「写命令」处理且不可执行
	Write   bool          // 写命令必须带审批令牌
	Timeout time.Duration // 默认超时（Policy.Timeouts 可覆盖）
	Risk    string        // 读 | 低 | 中 | 高
}

type field uint16

const (
	fCluster field = 1 << iota
	fNodes
	fRole
	fStore
	fName
	fKey
	fValue
	fInstance
	fRate
	fContent
)

type kindSpec struct {
	write   bool
	timeout time.Duration
	risk    string
	fields  field // 该 Kind 允许使用的字段，其余必须为空
}

const (
	riskRead = "读"
	riskLow  = "低"
	riskMid  = "中"
	riskHigh = "高"
)

var specs = map[string]kindSpec{
	KindClusterList:    {false, 30 * time.Second, riskRead, 0},
	KindTiupVersion:    {false, 15 * time.Second, riskRead, 0},
	KindTiupClusterVer: {false, 15 * time.Second, riskRead, 0},
	KindClusterDisplay: {false, 30 * time.Second, riskRead, fCluster},
	KindClusterCheck:   {false, 120 * time.Second, riskRead, fCluster | fContent}, // Content：对 scale-out 拓扑做预检
	KindPDQuery:        {false, 30 * time.Second, riskRead, fCluster | fKey},
	KindCDCCaptures:    {false, 30 * time.Second, riskRead, fCluster},

	KindClusterRestart:   {true, 600 * time.Second, riskMid, fCluster | fNodes | fRole},
	KindClusterStop:      {true, 600 * time.Second, riskHigh, fCluster | fNodes},
	KindClusterStart:     {true, 600 * time.Second, riskLow, fCluster | fNodes},
	KindClusterScaleOut:  {true, 3600 * time.Second, riskMid, fCluster | fContent},
	KindClusterScaleIn:   {true, 3600 * time.Second, riskHigh, fCluster | fNodes},
	KindClusterPrune:     {true, 300 * time.Second, riskMid, fCluster},
	KindClusterEditCfg:   {true, 120 * time.Second, riskMid, fCluster | fContent},
	KindClusterReload:    {true, 1800 * time.Second, riskMid, fCluster | fNodes | fRole},
	KindPDConfigSet:      {true, 30 * time.Second, riskMid, fCluster | fKey | fValue},
	KindPDEvictAdd:       {true, 30 * time.Second, riskMid, fCluster | fStore},
	KindPDEvictRemove:    {true, 30 * time.Second, riskMid, fCluster | fStore},
	KindPDStoreLimit:     {true, 30 * time.Second, riskMid, fCluster | fRate},
	KindPDLeaderTransfer: {true, 30 * time.Second, riskMid, fCluster | fName},
	KindSQLSetConfig:     {true, 10 * time.Second, riskMid, fCluster | fRole | fInstance | fKey | fValue},
	KindSQLSetGlobal:     {true, 10 * time.Second, riskMid, fCluster | fKey | fValue},
	KindBRBackupFull:     {true, 12 * time.Hour, riskMid, fCluster | fValue},
	KindBRRestoreFull:    {true, 12 * time.Hour, riskHigh, fCluster | fValue},
	KindBRLogStart:       {true, 60 * time.Second, riskMid, fCluster | fName | fValue},
	KindBRLogStop:        {true, 60 * time.Second, riskMid, fCluster | fName},
	KindBRLogStatus:      {false, 60 * time.Second, riskRead, fCluster},
	KindClusterUpgrade:   {true, 3 * time.Hour, riskHigh, fCluster | fValue},
	KindAlertSilence:     {true, 30 * time.Second, riskLow, fCluster | fKey | fInstance | fRate | fValue},
}

// Info 返回 Kind 的静态属性。未登记的 Kind 返回 Known=false、Write=true。
func Info(kind string) KindInfo {
	sp, ok := specs[kind]
	if !ok {
		return KindInfo{Known: false, Write: true, Risk: "未知"}
	}
	return KindInfo{Known: true, Write: sp.write, Timeout: sp.timeout, Risk: sp.risk}
}

// Kinds 返回全部已登记的 Kind（排序后），用于文档与自检。
func Kinds() []string {
	out := make([]string, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// timeoutFor 返回生效的超时：Policy.Timeouts 优先，其次 Info 默认值。
func timeoutFor(pol Policy, kind string) time.Duration {
	if d, ok := pol.Timeouts[kind]; ok && d > 0 {
		return d
	}
	return Info(kind).Timeout
}

var (
	clusterNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	nodeRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}:[0-9]{1,5}$`)
	hostRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
	taskNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	memberNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	versionRe     = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.]{1,32})?$`)
	pdURLRe       = regexp.MustCompile(`^https?://[A-Za-z0-9][A-Za-z0-9.-]{0,252}:[0-9]{1,5}$`)
	dbUserRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)
	envNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	backupCharsRe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]{1,512}$`)
	changeIDRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// 允许的角色枚举。cdc 与 ticdc 等价（tiup -R 用 cdc，目录与拓扑用 ticdc）。
var roleSet = map[string]bool{"tidb": true, "tikv": true, "pd": true, "tiflash": true, "cdc": true, "ticdc": true}

// SET CONFIG 支持的组件。
var setConfigRoles = map[string]bool{"tidb": true, "tikv": true, "pd": true, "tiflash": true}

// pd.query 的 Key → 固定的 pd-ctl 只读子命令。
var pdQueryCmds = map[string][]string{
	"store":               {"store"},
	"member":              {"member"},
	"scheduler-show":      {"scheduler", "show"},
	"region-down-peer":    {"region", "check", "down-peer"},
	"region-pending-peer": {"region", "check", "pending-peer"},
	"config-show":         {"config", "show"},
}

// tiupRoleName 返回 tiup -R 使用的角色名。
func tiupRoleName(role string) string {
	if role == "ticdc" {
		return "cdc"
	}
	return role
}

const shellMeta = ";&|<>`$(){}[]\\\"'!*?#~^"

// badToken 拒绝空白、控制字符、非 ASCII 以及 shell 元字符（含引号、反斜杠、反引号、$()）。
func badToken(name, s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c > 0x7e || strings.IndexByte(shellMeta, c) >= 0 {
			return fmt.Errorf("%s 含空白、控制字符或 shell 元字符", name)
		}
	}
	return nil
}

// needsTopo 判断校验该命令是否需要集群拓扑。display / prune 不需要（DisplayTopo 自己要调 display，必须避免递归）。
func needsTopo(c Cmd) bool {
	switch c.Kind {
	case KindClusterDisplay, KindClusterPrune:
		return false
	case KindClusterCheck:
		return c.Content != ""
	case KindAlertSilence:
		return c.Instance != ""
	}
	return true
}

// Validate 校验一条命令是否允许执行（不校验令牌）。未通过返回错误，绝不执行任何东西。
func Validate(ctx context.Context, pol Policy, topo TopoFunc, c Cmd) error {
	_, err := check(ctx, pol, topo, c)
	return err
}

// check 执行完整校验，并返回校验时取得的拓扑（不需要拓扑的命令返回零值）。
func check(ctx context.Context, pol Policy, topo TopoFunc, c Cmd) (Topo, error) {
	pol = pol.forCluster(c.Cluster)
	sp, ok := specs[c.Kind]
	if !ok {
		return Topo{}, fmt.Errorf("命令 %q 不在白名单内", truncate(c.Kind, 40))
	}
	if err := checkFields(sp.fields, c); err != nil {
		return Topo{}, err
	}
	if err := checkStrings(c); err != nil {
		return Topo{}, err
	}
	if c.Kind == KindClusterList || c.Kind == KindTiupVersion || c.Kind == KindTiupClusterVer { // 不针对某个集群：列出集群 / 读 tiup 版本（只读）
		return Topo{}, nil
	}
	if !clusterNameRe.MatchString(c.Cluster) {
		return Topo{}, fmt.Errorf("集群名不合法")
	}
	inPolicy := false
	for _, n := range pol.Clusters {
		if n == c.Cluster || n == AnyCluster {
			inPolicy = true
			break
		}
	}
	if !inPolicy {
		return Topo{}, fmt.Errorf("集群 %q 未接入", c.Cluster)
	}
	var t Topo
	if needsTopo(c) {
		if topo == nil {
			return Topo{}, fmt.Errorf("未配置拓扑来源，无法校验")
		}
		var err error
		if t, err = topo(ctx, c.Cluster); err != nil {
			return Topo{}, fmt.Errorf("获取集群拓扑失败: %w", err)
		}
	}
	if err := checkKind(pol, t, c); err != nil {
		return Topo{}, err
	}
	return t, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// checkFields：该 Kind 不使用的字段必须为空。
func checkFields(allowed field, c Cmd) error {
	type f struct {
		bit   field
		name  string
		empty bool
	}
	for _, x := range []f{
		{fCluster, "cluster", c.Cluster == ""},
		{fNodes, "nodes", len(c.Nodes) == 0},
		{fRole, "role", c.Role == ""},
		{fStore, "store", c.Store == 0},
		{fName, "name", c.Name == ""},
		{fKey, "key", c.Key == ""},
		{fValue, "value", c.Value == ""},
		{fInstance, "instance", c.Instance == ""},
		{fRate, "rate", c.Rate == 0},
		{fContent, "content", c.Content == ""},
	} {
		if allowed&x.bit == 0 && !x.empty {
			return fmt.Errorf("命令 %s 不使用字段 %s，必须为空", c.Kind, x.name)
		}
	}
	return nil
}

func checkStrings(c Cmd) error {
	for _, x := range []struct{ n, v string }{
		{"cluster", c.Cluster}, {"role", c.Role}, {"name", c.Name}, {"key", c.Key},
		{"instance", c.Instance},
	} {
		if len(x.v) > 600 {
			return fmt.Errorf("%s 过长", x.n)
		}
		if err := badToken(x.n, x.v); err != nil {
			return err
		}
	}
	if c.Kind != KindAlertSilence { // 静默备注允许空格与中文，由 checkSilence 单独校验
		if len(c.Value) > 600 {
			return fmt.Errorf("value 过长")
		}
		if err := badToken("value", c.Value); err != nil {
			return err
		}
	}
	if len(c.Nodes) > 100 {
		return fmt.Errorf("nodes 过多")
	}
	for _, n := range c.Nodes {
		if len(n) > 300 {
			return fmt.Errorf("nodes 含过长项")
		}
		if err := badToken("nodes", n); err != nil {
			return err
		}
	}
	return nil
}

func checkNodes(t Topo, nodes []string) error {
	if len(nodes) == 0 {
		return fmt.Errorf("nodes 不能为空")
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if !nodeRe.MatchString(n) {
			return fmt.Errorf("节点 %q 格式应为 host:port", n)
		}
		if p := portOf(n); p < 1 || p > 65535 {
			return fmt.Errorf("节点 %q 端口不合法", n)
		}
		if _, ok := t.Nodes[n]; !ok {
			return fmt.Errorf("节点 %s 不在集群拓扑内", n)
		}
		if seen[n] {
			return fmt.Errorf("节点 %s 重复", n)
		}
		seen[n] = true
	}
	return nil
}

func portOf(hostPort string) int {
	i := strings.LastIndexByte(hostPort, ':')
	p, err := strconv.Atoi(hostPort[i+1:])
	if err != nil {
		return 0
	}
	return p
}

func checkRole(role string) error {
	if !roleSet[role] {
		return fmt.Errorf("角色 %q 不在允许范围 tidb|tikv|pd|tiflash|cdc", truncate(role, 20))
	}
	return nil
}

func checkCtlTarget(t Topo) error {
	if !versionRe.MatchString(t.Version) {
		return fmt.Errorf("拓扑中的集群版本不合法，无法确定 pd-ctl 版本")
	}
	if !pdURLRe.MatchString(t.PD) {
		return fmt.Errorf("拓扑中没有可用的 PD 地址")
	}
	return nil
}

// checkScaleIn：缩容后必须保留足够的实例（A3：不得使剩余 TiKV < 3；PD / TiDB 至少保留 1 个）。
func checkScaleIn(pol Policy, t Topo, nodes []string) error {
	rm := map[string]bool{}
	removing := map[string]int{}
	for _, n := range nodes {
		rm[n] = true
		removing[t.Nodes[n]]++
	}
	remain := map[string]int{}
	for id, role := range t.Nodes {
		if !rm[id] {
			remain[role]++
		}
	}
	minTiKV := pol.MinTiKV
	if minTiKV <= 0 {
		minTiKV = 3
	}
	if removing["tikv"] > 0 && remain["tikv"] < minTiKV {
		return fmt.Errorf("缩容后 TiKV 仅剩 %d 个，低于下限 %d", remain["tikv"], minTiKV)
	}
	for _, r := range []string{"pd", "tidb"} {
		if removing[r] > 0 && remain[r] < 1 {
			return fmt.Errorf("缩容将移除全部 %s 实例", r)
		}
	}
	return nil
}

func checkBackupPath(pol Policy, v string) error {
	if v == "" {
		return fmt.Errorf("备份存储路径不能为空")
	}
	if !backupCharsRe.MatchString(v) || strings.Contains(v, "..") {
		return fmt.Errorf("备份存储路径含非法字符或 ..")
	}
	for _, root := range pol.BackupRoots {
		if root != "" && !strings.HasPrefix(root, "-") && strings.HasPrefix(v, root) {
			return nil
		}
	}
	return fmt.Errorf("备份存储路径不在允许的前缀内")
}

// cfgTarget 是 pd.config-set / sql.set-* 解析出的配置项与归一化值。
type cfgTarget struct {
	item  ConfigItem
	value string // 已归一化
	role  string // SET CONFIG 的目标角色（Instance 时由拓扑推导）
}

func resolveConfig(t Topo, c Cmd) (cfgTarget, error) {
	var role string
	switch c.Kind {
	case KindPDConfigSet:
		role = "pd"
	case KindSQLSetGlobal:
		role = "tidb"
	case KindSQLSetConfig:
		switch {
		case c.Role != "" && c.Instance != "":
			return cfgTarget{}, fmt.Errorf("role 与 instance 只能二选一")
		case c.Role != "":
			if !setConfigRoles[c.Role] {
				return cfgTarget{}, fmt.Errorf("SET CONFIG 只支持 tidb|tikv|pd|tiflash")
			}
			role = c.Role
		case c.Instance != "":
			if !nodeRe.MatchString(c.Instance) {
				return cfgTarget{}, fmt.Errorf("instance 格式应为 host:statusPort")
			}
			id, ok := t.Status[c.Instance]
			if !ok {
				return cfgTarget{}, fmt.Errorf("实例状态地址 %s 不在集群拓扑内", c.Instance)
			}
			role = t.Nodes[id]
			if !setConfigRoles[role] {
				return cfgTarget{}, fmt.Errorf("实例 %s 的角色 %q 不支持 SET CONFIG", c.Instance, role)
			}
		default:
			return cfgTarget{}, fmt.Errorf("必须指定 role 或 instance")
		}
	}
	if c.Key == "" {
		return cfgTarget{}, fmt.Errorf("key 不能为空")
	}
	it, ok := Lookup(role, c.Key)
	if !ok {
		return cfgTarget{}, fmt.Errorf("配置项 %s 不在 %s 的配置目录内", c.Key, role)
	}
	if !it.Online {
		return cfgTarget{}, fmt.Errorf("配置项 %s 不支持在线修改，需 edit-config + reload", it.Key)
	}
	switch c.Kind {
	case KindPDConfigSet:
		if it.Scope != "cluster" || it.Sys {
			return cfgTarget{}, fmt.Errorf("配置项 %s 不是 PD 集群级参数", it.Key)
		}
	case KindSQLSetGlobal:
		if !it.Sys {
			return cfgTarget{}, fmt.Errorf("%s 不是 TiDB 系统变量", it.Key)
		}
	case KindSQLSetConfig:
		if it.Sys {
			return cfgTarget{}, fmt.Errorf("%s 是系统变量，应使用 sql.set-global", it.Key)
		}
	}
	v, err := Normalize(it, c.Value)
	if err != nil {
		return cfgTarget{}, err
	}
	return cfgTarget{item: it, value: v, role: role}, nil
}

func checkKind(pol Policy, t Topo, c Cmd) error {
	switch c.Kind {
	case KindClusterDisplay, KindClusterPrune:
		return nil

	case KindClusterCheck:
		if c.Content != "" {
			return ValidateScaleOut(c.Content, t)
		}

	case KindClusterRestart:
		return checkNodesOrRole(t, c)

	case KindClusterStop, KindClusterStart:
		return checkNodes(t, c.Nodes)

	case KindClusterScaleIn:
		if err := checkNodes(t, c.Nodes); err != nil {
			return err
		}
		return checkScaleIn(pol, t, c.Nodes)

	case KindClusterScaleOut:
		if c.Content == "" {
			return fmt.Errorf("缺少拓扑内容")
		}
		return ValidateScaleOut(c.Content, t)

	case KindClusterEditCfg:
		if c.Content == "" {
			return fmt.Errorf("缺少拓扑内容")
		}
		return ValidateEditConfigFull(c.Content, t.User, t.SSHPort, t.EditBase)

	case KindClusterReload:
		return checkNodesOrRole(t, c)

	case KindPDQuery:
		if _, ok := pdQueryCmds[c.Key]; !ok {
			return fmt.Errorf("pd.query 不支持查询 %q", truncate(c.Key, 30))
		}
		return checkCtlTarget(t)

	case KindCDCCaptures:
		if !versionRe.MatchString(t.Version) {
			return fmt.Errorf("拓扑中的集群版本不合法")
		}
		if _, err := cdcServer(t); err != nil {
			return err
		}

	case KindPDConfigSet:
		if err := checkCtlTarget(t); err != nil {
			return err
		}
		_, err := resolveConfig(t, c)
		return err

	case KindPDEvictAdd, KindPDEvictRemove:
		if c.Store <= 0 {
			return fmt.Errorf("store 必须是正整数")
		}
		return checkCtlTarget(t)

	case KindPDStoreLimit:
		if c.Rate < 1 || c.Rate > 200 {
			return fmt.Errorf("store limit 取值范围 1–200")
		}
		return checkCtlTarget(t)

	case KindPDLeaderTransfer:
		if !memberNameRe.MatchString(c.Name) {
			return fmt.Errorf("PD 成员名不合法")
		}
		return checkCtlTarget(t)

	case KindSQLSetConfig, KindSQLSetGlobal:
		if _, err := resolveConfig(t, c); err != nil {
			return err
		}
		_, _, _, err := sqlTarget(pol, t)
		return err

	case KindBRBackupFull, KindBRRestoreFull:
		if err := checkBackupPath(pol, c.Value); err != nil {
			return err
		}
		return checkCtlTarget(t)

	case KindBRLogStart:
		if !taskNameRe.MatchString(c.Name) {
			return fmt.Errorf("任务名不合法")
		}
		if err := checkBackupPath(pol, c.Value); err != nil {
			return err
		}
		return checkCtlTarget(t)

	case KindBRLogStop:
		if !taskNameRe.MatchString(c.Name) {
			return fmt.Errorf("任务名不合法")
		}
		return checkCtlTarget(t)

	case KindBRLogStatus:
		return checkCtlTarget(t)

	case KindClusterUpgrade:
		return checkUpgrade(t, c)

	case KindAlertSilence:
		return checkSilence(pol, t, c)

	default:
		return fmt.Errorf("命令 %q 不在白名单内", truncate(c.Kind, 40))
	}
	return nil
}

// sqlTarget 返回运维账号连接参数：Policy 优先，其次拓扑里的 TiDB 地址；并校验策略里的账号与密码变量名。
func sqlTarget(pol Policy, t Topo) (host string, port int, user string, err error) {
	host, port = pol.MySQLHost, pol.MySQLPort
	if host == "" {
		host, port = t.SQLHost, t.SQLPort
	}
	if !hostRe.MatchString(host) || port < 1 || port > 65535 {
		return "", 0, "", fmt.Errorf("未配置有效的 TiDB 连接地址")
	}
	if !dbUserRe.MatchString(pol.MySQLUser) {
		return "", 0, "", fmt.Errorf("未配置有效的运维账号")
	}
	if pol.MySQLPwdEnv != "" && !envNameRe.MatchString(pol.MySQLPwdEnv) {
		return "", 0, "", fmt.Errorf("策略里的密码环境变量名不合法")
	}
	return host, port, pol.MySQLUser, nil
}

// cdcServer 选取一个 TiCDC 实例（按地址排序后的第一个）作为 cli 的 --server。
func cdcServer(t Topo) (string, error) {
	var ids []string
	for id, r := range t.Nodes {
		if canonicalRole(r) == "ticdc" && nodeRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("集群中没有 TiCDC 实例")
	}
	sort.Strings(ids)
	return "http://" + ids[0], nil
}

// checkNodesOrRole：restart / reload 的目标必须在 Nodes 与 Role 中二选一。
func checkNodesOrRole(t Topo, c Cmd) error {
	if (len(c.Nodes) == 0) == (c.Role == "") {
		return fmt.Errorf("%s 必须在 nodes 与 role 中二选一", c.Kind)
	}
	if c.Role == "" {
		return checkNodes(t, c.Nodes)
	}
	if err := checkRole(c.Role); err != nil {
		return err
	}
	for _, r := range t.Nodes {
		if canonicalRole(r) == canonicalRole(c.Role) {
			return nil
		}
	}
	return fmt.Errorf("集群中没有角色为 %s 的实例", c.Role)
}

var (
	upgradeVerRe = regexp.MustCompile(`^v([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$`)
	alertNameRe  = regexp.MustCompile(`^[A-Za-z0-9_:.%-]{1,128}$`)
	alertURLRe   = regexp.MustCompile(`^https?://[A-Za-z0-9][A-Za-z0-9.-]{0,252}(?::[0-9]{1,5})?$`)
	verPartsRe   = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)`)
)

func versionParts(re *regexp.Regexp, v string) (p [3]int, ok bool) {
	m := re.FindStringSubmatch(v)
	if m == nil {
		return p, false
	}
	for i := 0; i < 3; i++ {
		p[i], _ = strconv.Atoi(m[i+1])
	}
	return p, true
}

// checkUpgrade：目标版本格式合法，且不低于当前版本（降级不允许）。
func checkUpgrade(t Topo, c Cmd) error {
	target, ok := versionParts(upgradeVerRe, c.Value)
	if !ok {
		return fmt.Errorf("目标版本格式应为 vX.Y.Z")
	}
	if cur, ok := versionParts(verPartsRe, t.Version); ok {
		for i := 0; i < 3; i++ {
			if target[i] != cur[i] {
				if target[i] < cur[i] {
					return fmt.Errorf("不允许降级（当前 %s，目标 %s）", t.Version, c.Value)
				}
				break
			}
		}
	}
	return nil
}

// checkSilence 校验 alert.silence 的参数。Value（备注）允许空格与中文，但不得含控制字符，≤200 字。
func checkSilence(pol Policy, t Topo, c Cmd) error {
	if !alertNameRe.MatchString(c.Key) {
		return fmt.Errorf("告警名不合法")
	}
	if c.Rate < 1 || c.Rate > 1440 {
		return fmt.Errorf("静默时长取值范围 1–1440 分钟")
	}
	if c.Instance != "" {
		if !nodeRe.MatchString(c.Instance) {
			return fmt.Errorf("instance 格式应为 host:port")
		}
		_, inNodes := t.Nodes[c.Instance]
		_, inStatus := t.Status[c.Instance]
		if !inNodes && !inStatus {
			return fmt.Errorf("实例 %s 不在集群拓扑内", c.Instance)
		}
	}
	if utf8.RuneCountInString(c.Value) > 200 || !utf8.ValidString(c.Value) {
		return fmt.Errorf("备注过长（≤200 字）或编码非法")
	}
	for _, r := range c.Value {
		if unicode.IsControl(r) {
			return fmt.Errorf("备注含控制字符")
		}
	}
	if !alertURLRe.MatchString(pol.AlertURL) {
		return fmt.Errorf("未配置有效的 Alertmanager 地址")
	}
	return nil
}
