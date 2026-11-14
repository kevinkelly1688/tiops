// @author: kevinkelly
// @license: Apache License 2.0
// @file: yamlcheck.go
// @time: 2026/10/28

package exec

import (
	"fmt"
	"io"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// 拓扑 YAML 校验（scale-out 与 edit-config）。
//
// 总原则：拓扑文件最终由 tiup 读取并经 SSH 下发到各节点，所以内容也是「命令」的一部分，
// 必须按白名单校验，而不是只当作数据。统一的底线（两种拓扑都适用）：
//   - 只接受单个 YAML 文档，根必须是映射；拒绝锚点 / 别名 / 合并键 / 自定义标签 / 重复键；
//   - 所有标量与键仅允许可打印 ASCII（拒绝控制字符、换行、非 ASCII），长度有限；
//   - 嵌套深度、节点总数、文件大小都有上限（防止 YAML 炸弹）；
//   - 路径只允许 /tidb-deploy 或 /tidb-data 之下的规范化绝对路径，不含 ..；
//   - host 只允许单播 IPv4，端口必须是 1–65535 的十进制整数；
//   - 会让 tiup 读取平台本机任意目录 / 文件的键（rule_dir、dashboard_dir、config_file、
//     additional_scrape_conf 等）一律不允许。

const (
	maxScaleOutLen   = 64 << 10
	maxEditConfigLen = 256 << 10
	maxYAMLDepth     = 16
	maxYAMLNodes     = 20000
	maxScalarLen     = 4096
)

var (
	ipv4Re       = regexp.MustCompile(`^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])$`)
	portTextRe   = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	yamlKeyRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	labelKeyRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	labelValRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	dirCharsRe   = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,200}$`)
	instNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	simpleWordRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	numaRe       = regexp.MustCompile(`^[0-9][0-9,-]{0,15}$`)
	retentionRe  = regexp.MustCompile(`^[0-9]{1,5}[smhdwy]$`)
	resCtlRe     = regexp.MustCompile(`^[A-Za-z0-9_.%-]{0,64}$`)
)

var okTags = map[string]bool{"!!str": true, "!!int": true, "!!bool": true, "!!float": true, "!!null": true, "!!map": true, "!!seq": true}

// parseYAMLRoot 解析并做通用安全检查，返回根映射节点。
func parseYAMLRoot(content string, maxLen int) (*yaml.Node, error) {
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("拓扑内容为空")
	}
	if len(content) > maxLen {
		return nil, fmt.Errorf("拓扑内容过大（上限 %d 字节）", maxLen)
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return nil, fmt.Errorf("拓扑含非法字符")
	}
	dec := yaml.NewDecoder(strings.NewReader(content))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("拓扑内容为空")
		}
		return nil, fmt.Errorf("YAML 解析失败: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("拓扑只允许单个 YAML 文档")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("拓扑结构非法")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("拓扑根必须是映射")
	}
	count := 0
	if err := checkNodeSafe(root, 0, &count); err != nil {
		return nil, err
	}
	return root, nil
}

func asciiPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func checkNodeSafe(n *yaml.Node, depth int, count *int) error {
	*count++
	if *count > maxYAMLNodes {
		return fmt.Errorf("拓扑节点过多")
	}
	if depth > maxYAMLDepth {
		return fmt.Errorf("拓扑嵌套过深")
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return fmt.Errorf("拓扑不允许使用 YAML 锚点 / 别名")
	}
	if !okTags[n.ShortTag()] {
		return fmt.Errorf("拓扑含不允许的 YAML 标签 %q", n.ShortTag())
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if len(n.Value) > maxScalarLen {
			return fmt.Errorf("拓扑标量过长")
		}
		if !asciiPrintable(n.Value) {
			return fmt.Errorf("拓扑含控制字符、换行或非 ASCII 字符")
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := checkNodeSafe(c, depth+1, count); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				return fmt.Errorf("拓扑的键必须是普通字符串")
			}
			if !yamlKeyRe.MatchString(k.Value) {
				return fmt.Errorf("拓扑的键 %q 不合法", k.Value)
			}
			if seen[k.Value] {
				return fmt.Errorf("拓扑含重复键 %q", k.Value)
			}
			seen[k.Value] = true
			if err := checkNodeSafe(v, depth+1, count); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("拓扑含不支持的 YAML 节点")
	}
	return nil
}

type pair struct {
	key string
	val *yaml.Node
}

func mapPairs(n *yaml.Node) []pair {
	var out []pair
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, pair{n.Content[i].Value, n.Content[i+1]})
	}
	return out
}

func isScalar(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode }

// portValue 要求 n 是 1–65535 的十进制整数（不接受字符串、十六进制、八进制、前导 0）。
func portValue(n *yaml.Node, what string) (int, error) {
	if !isScalar(n) || n.ShortTag() != "!!int" || !portTextRe.MatchString(n.Value) {
		return 0, fmt.Errorf("%s 必须是 1–65535 的整数", what)
	}
	p, _ := strconv.Atoi(n.Value)
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("%s 必须是 1–65535 的整数", what)
	}
	return p, nil
}

func strValue(n *yaml.Node, what string, re *regexp.Regexp) (string, error) {
	if !isScalar(n) || (n.ShortTag() != "!!str" && n.ShortTag() != "!!int") || !re.MatchString(n.Value) {
		return "", fmt.Errorf("%s 取值不合法", what)
	}
	return n.Value, nil
}

func boolValue(n *yaml.Node, what string) error {
	if !isScalar(n) || n.ShortTag() != "!!bool" {
		return fmt.Errorf("%s 必须是布尔值", what)
	}
	return nil
}

func hostValue(n *yaml.Node) (string, error) {
	if !isScalar(n) || n.ShortTag() != "!!str" || !ipv4Re.MatchString(n.Value) {
		return "", fmt.Errorf("host 必须是 IPv4 地址")
	}
	ip := net.ParseIP(n.Value)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || n.Value == "255.255.255.255" {
		return "", fmt.Errorf("host %s 不是可用的单播地址", n.Value)
	}
	return n.Value, nil
}

// dirValue 只允许 /tidb-deploy 或 /tidb-data 之下的规范化绝对路径。
func dirValue(n *yaml.Node, what string) error {
	if !isScalar(n) || n.ShortTag() != "!!str" {
		return fmt.Errorf("%s 必须是路径字符串", what)
	}
	return checkDir(n.Value, what)
}

var allowedDirRoots = []string{"/tidb-deploy", "/tidb-data"}

func checkDir(p, what string) error {
	if !dirCharsRe.MatchString(p) || !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || path.Clean(p) != p {
		return fmt.Errorf("%s %q 必须是不含 .. 的规范化绝对路径", what, p)
	}
	for _, r := range allowedDirRoots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return nil
		}
	}
	return fmt.Errorf("%s %q 必须位于 /tidb-deploy 或 /tidb-data 之下", what, p)
}

type cfgLeaf struct {
	key string
	val *yaml.Node
}

// flattenConfig 把嵌套 / 点号混合的配置映射展开成「点号键 → 标量」。拒绝序列。
func flattenConfig(n *yaml.Node, prefix string, out *[]cfgLeaf) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("config 必须是映射")
	}
	for _, p := range mapPairs(n) {
		k := p.key
		if prefix != "" {
			k = prefix + "." + p.key
		}
		switch p.val.Kind {
		case yaml.MappingNode:
			if err := flattenConfig(p.val, k, out); err != nil {
				return err
			}
		case yaml.ScalarNode:
			*out = append(*out, cfgLeaf{k, p.val})
		default:
			return fmt.Errorf("配置项 %s 不允许是列表", k)
		}
	}
	return nil
}

// scaleOutRoles：scale-out 允许的顶层 *_servers → 目录角色。
var scaleOutRoles = map[string]string{
	"tidb_servers": "tidb", "tikv_servers": "tikv", "pd_servers": "pd",
	"tiflash_servers": "tiflash", "cdc_servers": "ticdc",
}

type portDef struct {
	name string
	def  int
}

var rolePorts = map[string][]portDef{
	"tidb":    {{"port", 4000}, {"status_port", 10080}},
	"tikv":    {{"port", 20160}, {"status_port", 20180}},
	"pd":      {{"client_port", 2379}, {"peer_port", 2380}},
	"tiflash": {{"tcp_port", 9000}, {"http_port", 8123}, {"flash_service_port", 3930}, {"flash_proxy_port", 20170}, {"flash_proxy_status_port", 20292}, {"metrics_port", 8234}},
	"ticdc":   {{"port", 8300}},
}

const maxScaleOutEntries = 20

// ValidateScaleOut 校验 scale-out 拓扑（保守设计）：
//   - 顶层键只能是 global / tidb_servers / tikv_servers / pd_servers / tiflash_servers / cdc_servers；
//   - global 只允许 user / ssh_port / deploy_dir / data_dir：user 必须等于现有集群的部署用户（t.User，未知则不允许出现），
//     ssh_port 若已知必须相同，目录同样受 /tidb-deploy、/tidb-data 约束；
//   - 每个实例条目只允许 host、ssh_port、name(PD)、该角色的端口键、deploy_dir/data_dir/log_dir、config、learner_config(TiFlash)；
//   - config 展开为点号键后，只允许 server.labels.*（TiKV；TiFlash 在 learner_config）以及 configcat 里登记的非系统变量，
//     且值必须通过 Normalize；
//   - 新实例的任一端口（含默认端口）不得与 t.Nodes / t.Status 里现有的 host:port 冲突，新实例之间也不得冲突。
func ValidateScaleOut(content string, t Topo) error {
	root, err := parseYAMLRoot(content, maxScaleOutLen)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for k := range t.Nodes {
		taken[k] = true
	}
	for k := range t.Status {
		taken[k] = true
	}
	total := 0
	for _, p := range mapPairs(root) {
		if p.key == "global" {
			if err := checkScaleOutGlobal(p.val, t); err != nil {
				return err
			}
			continue
		}
		role, ok := scaleOutRoles[p.key]
		if !ok {
			return fmt.Errorf("scale-out 拓扑不允许顶层键 %q", p.key)
		}
		if p.val.Kind != yaml.SequenceNode || len(p.val.Content) == 0 {
			return fmt.Errorf("%s 必须是非空列表", p.key)
		}
		if len(p.val.Content) > maxScaleOutEntries {
			return fmt.Errorf("%s 条目过多（上限 %d）", p.key, maxScaleOutEntries)
		}
		for i, e := range p.val.Content {
			if err := checkScaleOutEntry(p.key, role, e, taken); err != nil {
				return fmt.Errorf("%s[%d]: %w", p.key, i, err)
			}
			total++
		}
	}
	if total == 0 {
		return fmt.Errorf("scale-out 拓扑没有任何实例")
	}
	return nil
}

func checkScaleOutGlobal(n *yaml.Node, t Topo) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("global 必须是映射")
	}
	for _, p := range mapPairs(n) {
		switch p.key {
		case "user":
			if t.User == "" {
				return fmt.Errorf("无法确认集群部署用户，global.user 不允许出现")
			}
			if !isScalar(p.val) || p.val.Value != t.User {
				return fmt.Errorf("global.user 必须等于集群部署用户")
			}
		case "ssh_port":
			port, err := portValue(p.val, "global.ssh_port")
			if err != nil {
				return err
			}
			if t.SSHPort > 0 && port != t.SSHPort {
				return fmt.Errorf("global.ssh_port 必须与集群一致（%d）", t.SSHPort)
			}
		case "deploy_dir", "data_dir":
			if err := dirValue(p.val, "global."+p.key); err != nil {
				return err
			}
		default:
			return fmt.Errorf("scale-out 的 global 只允许 user / ssh_port / deploy_dir / data_dir，发现 %q", p.key)
		}
	}
	return nil
}

func checkScaleOutEntry(section, role string, e *yaml.Node, taken map[string]bool) error {
	if e.Kind != yaml.MappingNode {
		return fmt.Errorf("条目必须是映射")
	}
	allowedPorts := map[string]int{}
	for _, pd := range rolePorts[role] {
		allowedPorts[pd.name] = pd.def
	}
	host := ""
	used := map[string]int{}
	for k, v := range allowedPorts {
		used[k] = v
	}
	hasHost := false
	for _, p := range mapPairs(e) {
		switch p.key {
		case "host":
			h, err := hostValue(p.val)
			if err != nil {
				return err
			}
			host, hasHost = h, true
		case "ssh_port":
			if _, err := portValue(p.val, "ssh_port"); err != nil {
				return err
			}
		case "name":
			if role != "pd" {
				return fmt.Errorf("只有 PD 条目允许 name")
			}
			if _, err := strValue(p.val, "name", instNameRe); err != nil {
				return err
			}
		case "deploy_dir", "data_dir", "log_dir":
			if err := dirValue(p.val, p.key); err != nil {
				return err
			}
		case "config":
			if err := checkScaleOutConfig(role, p.val, false); err != nil {
				return err
			}
		case "learner_config":
			if role != "tiflash" {
				return fmt.Errorf("只有 TiFlash 条目允许 learner_config")
			}
			if err := checkScaleOutConfig(role, p.val, true); err != nil {
				return err
			}
		default:
			if _, ok := allowedPorts[p.key]; !ok {
				return fmt.Errorf("不允许的键 %q", p.key)
			}
			port, err := portValue(p.val, p.key)
			if err != nil {
				return err
			}
			used[p.key] = port
		}
	}
	if !hasHost {
		return fmt.Errorf("缺少 host")
	}
	// 同一实例内的端口也不能互相重复。
	own := map[int]string{}
	names := make([]string, 0, len(used))
	for k := range used {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		port := used[k]
		if other, dup := own[port]; dup {
			return fmt.Errorf("端口 %d 同时用于 %s 与 %s", port, other, k)
		}
		own[port] = k
		id := net.JoinHostPort(host, strconv.Itoa(port))
		if taken[id] {
			return fmt.Errorf("%s（%s）与现有实例或本次新增实例冲突", id, k)
		}
	}
	for port := range own {
		taken[net.JoinHostPort(host, strconv.Itoa(port))] = true
	}
	return nil
}

func checkScaleOutConfig(role string, n *yaml.Node, learner bool) error {
	var leaves []cfgLeaf
	if err := flattenConfig(n, "", &leaves); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, l := range leaves {
		if seen[l.key] {
			return fmt.Errorf("配置项 %s 重复", l.key)
		}
		seen[l.key] = true
		if err := checkConfigLeaf(role, learner, l.key, l.val.Value, l.val.ShortTag()); err != nil {
			return err
		}
	}
	return nil
}

// checkConfigLeaf 校验单个展开后的配置项：labels 或目录内的非系统变量，且值合法。
func checkConfigLeaf(role string, learner bool, key, value, tag string) error {
	if rest, ok := strings.CutPrefix(key, "server.labels."); ok && (role == "tikv" || learner) {
		if !labelKeyRe.MatchString(rest) {
			return fmt.Errorf("标签名 %q 不合法", rest)
		}
		if (tag != "!!str" && tag != "!!int") || !labelValRe.MatchString(value) {
			return fmt.Errorf("标签 %s 的值不合法", rest)
		}
		return nil
	}
	if learner {
		return fmt.Errorf("learner_config 只允许 server.labels.*，发现 %q", key)
	}
	it, ok := Lookup(role, key)
	if !ok || it.Sys {
		return fmt.Errorf("配置项 %q 不在 %s 的配置目录内", key, role)
	}
	if tag == "!!null" {
		return fmt.Errorf("配置项 %s 的值不能为空", key)
	}
	if _, err := Normalize(it, value); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- edit-config ----

var editConfigTop = map[string]bool{
	"global": true, "monitored": true, "server_configs": true,
	"pd_servers": true, "tidb_servers": true, "tikv_servers": true, "tiflash_servers": true, "cdc_servers": true,
	"monitoring_servers": true, "grafana_servers": true, "alertmanager_servers": true, "tiproxy_servers": true,
}

// editRoleOf：edit-config 里 *_servers → 目录角色（用于 config 键校验与差异检查）。
var editRoleOf = map[string]string{
	"tidb_servers": "tidb", "tikv_servers": "tikv", "pd_servers": "pd", "tiflash_servers": "tiflash", "cdc_servers": "ticdc",
}

// serverConfigsRoles：server_configs 的二级键 → 目录角色（空表示没有目录，只允许放行不做改动）。
var serverConfigsRoles = map[string]string{
	"tidb": "tidb", "tikv": "tikv", "pd": "pd", "tiflash": "tiflash", "tiflash-learner": "tiflash", "cdc": "ticdc", "tiproxy": "",
}

// ValidateEditConfig 校验 edit-config 的完整拓扑 YAML：合法 YAML、顶层键在白名单、global.user 等于 wantUser（必须出现）。
// 结构规则见 checkEditConfig；本函数不检查 ssh_port 的期望值（见 ValidateEditConfigFull）。
func ValidateEditConfig(content string, wantUser string) error {
	return ValidateEditConfigFull(content, wantUser, 0, "")
}

// ValidateEditConfigFull 在 ValidateEditConfig 之上：
//   - wantSSHPort > 0 时要求 global.ssh_port（若出现）等于它；
//   - base 非空时（当前拓扑 YAML），要求「相对 base 发生变化的配置项」全部落在配置目录内
//     （server_configs.<role>.<key> 或 <role>_servers[i].config.<key>，键须已登记且值通过 Normalize），
//     host / 端口 / 目录 / 新增删除实例等结构性变更一律拒绝。
func ValidateEditConfigFull(content, wantUser string, wantSSHPort int, base string) error {
	if wantUser == "" {
		return fmt.Errorf("无法确认集群部署用户，拒绝 edit-config")
	}
	root, err := parseYAMLRoot(content, maxEditConfigLen)
	if err != nil {
		return err
	}
	var global *yaml.Node
	for _, p := range mapPairs(root) {
		if !editConfigTop[p.key] {
			return fmt.Errorf("edit-config 拓扑不允许顶层键 %q", p.key)
		}
		if p.key == "global" {
			global = p.val
		}
	}
	if global == nil || global.Kind != yaml.MappingNode {
		return fmt.Errorf("edit-config 拓扑必须包含 global 并带有 user")
	}
	for _, p := range mapPairs(root) {
		if err := checkEditSection(p.key, p.val, wantUser, wantSSHPort); err != nil {
			return fmt.Errorf("%s: %w", p.key, err)
		}
	}
	if base != "" {
		return checkEditDiff(base, content)
	}
	return nil
}

var globalKeys = map[string]bool{"user": true, "group": true, "ssh_port": true, "ssh_type": true, "deploy_dir": true, "data_dir": true,
	"log_dir": true, "os": true, "arch": true, "listen_host": true, "systemd_mode": true, "resource_control": true}

func checkEditSection(key string, n *yaml.Node, wantUser string, wantPort int) error {
	switch key {
	case "global":
		hasUser := false
		for _, p := range mapPairs(n) {
			if !globalKeys[p.key] {
				return fmt.Errorf("不允许的键 %q", p.key)
			}
			switch p.key {
			case "user":
				if !isScalar(p.val) || p.val.Value != wantUser {
					return fmt.Errorf("global.user 必须等于集群部署用户，不得修改")
				}
				hasUser = true
			case "group", "os", "arch", "systemd_mode":
				if _, err := strValue(p.val, p.key, simpleWordRe); err != nil {
					return err
				}
			case "ssh_port":
				port, err := portValue(p.val, "global.ssh_port")
				if err != nil {
					return err
				}
				if wantPort > 0 && port != wantPort {
					return fmt.Errorf("global.ssh_port 不得修改")
				}
			case "ssh_type":
				if !isScalar(p.val) || (p.val.Value != "builtin" && p.val.Value != "system") {
					return fmt.Errorf("global.ssh_type 只允许 builtin / system")
				}
			case "deploy_dir", "data_dir", "log_dir":
				if err := dirValue(p.val, "global."+p.key); err != nil {
					return err
				}
			case "listen_host":
				if !isScalar(p.val) || !ipv4Re.MatchString(p.val.Value) {
					return fmt.Errorf("listen_host 必须是 IPv4")
				}
			case "resource_control":
				if err := checkResourceControl(p.val); err != nil {
					return err
				}
			}
		}
		if !hasUser {
			return fmt.Errorf("global.user 必须存在且等于集群部署用户")
		}
	case "monitored":
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("必须是映射")
		}
		for _, p := range mapPairs(n) {
			switch p.key {
			case "node_exporter_port", "blackbox_exporter_port":
				if _, err := portValue(p.val, p.key); err != nil {
					return err
				}
			case "deploy_dir", "data_dir", "log_dir":
				if err := dirValue(p.val, p.key); err != nil {
					return err
				}
			default:
				return fmt.Errorf("不允许的键 %q", p.key)
			}
		}
	case "server_configs":
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("必须是映射")
		}
		for _, p := range mapPairs(n) {
			if _, ok := serverConfigsRoles[p.key]; !ok {
				return fmt.Errorf("不允许的组件 %q", p.key)
			}
			if p.val.Kind == yaml.ScalarNode && p.val.ShortTag() == "!!null" {
				continue
			}
			if err := checkFreeConfig(p.val); err != nil {
				return fmt.Errorf("%s: %w", p.key, err)
			}
		}
	default: // *_servers
		if n.Kind != yaml.SequenceNode {
			return fmt.Errorf("必须是列表")
		}
		for i, e := range n.Content {
			if err := checkEditEntry(e); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// checkFreeConfig 校验 server_configs / config 这类「任意配置键」的子树：键名受 yamlKeyRe 约束，
// 值是标量或标量列表。具体哪些键允许被「改动」由 ValidateEditConfigFull 的 base 差异检查把关。
func checkFreeConfig(n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		for _, p := range mapPairs(n) {
			if err := checkFreeConfig(p.val); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("配置列表只允许标量元素")
			}
		}
	}
	return nil
}

func checkResourceControl(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("resource_control 必须是映射")
	}
	for _, p := range mapPairs(n) {
		if !isScalar(p.val) || !resCtlRe.MatchString(p.val.Value) {
			return fmt.Errorf("resource_control.%s 取值不合法", p.key)
		}
	}
	return nil
}

var entryPortKeys = map[string]bool{"port": true, "status_port": true, "client_port": true, "peer_port": true, "tcp_port": true, "http_port": true,
	"flash_service_port": true, "flash_proxy_port": true, "flash_proxy_status_port": true, "metrics_port": true,
	"web_port": true, "cluster_port": true, "ng_port": true}

func checkEditEntry(e *yaml.Node) error {
	if e.Kind != yaml.MappingNode {
		return fmt.Errorf("条目必须是映射")
	}
	for _, p := range mapPairs(e) {
		k := p.key
		switch {
		case k == "host":
			if _, err := hostValue(p.val); err != nil {
				return err
			}
		case k == "ssh_port" || entryPortKeys[k]:
			if _, err := portValue(p.val, k); err != nil {
				return err
			}
		case k == "deploy_dir" || k == "data_dir" || k == "log_dir":
			if err := dirValue(p.val, k); err != nil {
				return err
			}
		case k == "name" || k == "default_theme" || k == "org_name" || k == "org_role" || k == "os" || k == "arch":
			if _, err := strValue(p.val, k, simpleWordRe); err != nil {
				return err
			}
		case k == "numa_node":
			if _, err := strValue(p.val, k, numaRe); err != nil {
				return err
			}
		case k == "retention":
			if _, err := strValue(p.val, k, retentionRe); err != nil {
				return err
			}
		case k == "offline" || k == "anonymous_enable":
			if err := boolValue(p.val, k); err != nil {
				return err
			}
		case k == "resource_control":
			if err := checkResourceControl(p.val); err != nil {
				return err
			}
		case k == "config" || k == "learner_config":
			if err := checkFreeConfig(p.val); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		default:
			// 包含 rule_dir / dashboard_dir / config_file / additional_scrape_conf 等会让 tiup 读取本机路径的键。
			return fmt.Errorf("不允许的键 %q", k)
		}
	}
	return nil
}

// ---- 与当前拓扑的差异检查 ----

// leafMap 把文档展开为「路径(\x00 分隔) → tag:value」。
func leafMap(n *yaml.Node, path []string, out map[string]string) {
	key := strings.Join(path, "\x00")
	switch n.Kind {
	case yaml.MappingNode:
		if len(n.Content) == 0 {
			out[key] = "{}"
		}
		for _, p := range mapPairs(n) {
			leafMap(p.val, append(path[:len(path):len(path)], p.key), out)
		}
	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			out[key] = "[]"
		}
		for i, c := range n.Content {
			leafMap(c, append(path[:len(path):len(path)], "["+strconv.Itoa(i)+"]"), out)
		}
	default:
		out[key] = n.ShortTag() + ":" + n.Value
	}
}

func checkEditDiff(base, content string) error {
	broot, err := parseYAMLRoot(base, maxEditConfigLen)
	if err != nil {
		return fmt.Errorf("当前拓扑无法解析: %w", err)
	}
	nroot, err := parseYAMLRoot(content, maxEditConfigLen)
	if err != nil {
		return err
	}
	bm, nm := map[string]string{}, map[string]string{}
	leafMap(broot, nil, bm)
	leafMap(nroot, nil, nm)
	changed := map[string]bool{}
	for k, v := range bm {
		if nv, ok := nm[k]; !ok || nv != v {
			changed[k] = true
		}
	}
	for k := range nm {
		if _, ok := bm[k]; !ok {
			changed[k] = true
		}
	}
	keys := make([]string, 0, len(changed))
	for k := range changed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		segs := strings.Split(k, "\x00")
		if err := changedPathAllowed(segs, nm); err != nil {
			return fmt.Errorf("不允许的变更 %s: %w", strings.ReplaceAll(k, "\x00", "."), err)
		}
	}
	return nil
}

// changedPathAllowed：变更只允许落在 server_configs.<组件>.<键> 或 <role>_servers[i].config.<键>。
func changedPathAllowed(segs []string, newDoc map[string]string) error {
	var role string
	var learner bool
	var cfgKeys []string
	if len(segs) == 3 && editRoleOf[segs[0]] != "" && strings.HasPrefix(segs[1], "[") && (segs[2] == "config" || segs[2] == "learner_config") && newDoc[strings.Join(segs, "\x00")] == "{}" {
		return nil // 配置被清空为空映射
	}
	switch {
	case len(segs) >= 3 && segs[0] == "server_configs":
		r, ok := serverConfigsRoles[segs[1]]
		if !ok || r == "" {
			return fmt.Errorf("组件 %s 没有可变更的配置项", segs[1])
		}
		role, learner, cfgKeys = r, segs[1] == "tiflash-learner", segs[2:]
	case len(segs) >= 4 && editRoleOf[segs[0]] != "" && strings.HasPrefix(segs[1], "[") && (segs[2] == "config" || segs[2] == "learner_config"):
		role, learner, cfgKeys = editRoleOf[segs[0]], segs[2] == "learner_config", segs[3:]
	default:
		return fmt.Errorf("只允许变更 server_configs 与实例 config 下的配置项")
	}
	key := strings.Join(cfgKeys, ".")
	val, present := newDoc[strings.Join(segs, "\x00")]
	if !present { // 删除配置项（恢复默认）：只要键本身在目录内即可
		val = ""
	}
	tag, v, _ := strings.Cut(val, ":")
	if !present {
		tag, v = "!!str", ""
		if strings.HasPrefix(key, "server.labels.") {
			return nil
		}
		if learner {
			return fmt.Errorf("learner_config 只允许 server.labels.*")
		}
		if it, ok := Lookup(role, key); !ok || it.Sys {
			return fmt.Errorf("配置项 %q 不在配置目录内", key)
		}
		return nil
	}
	return checkConfigLeaf(role, learner, key, v, tag)
}
