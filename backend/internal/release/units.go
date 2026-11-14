// @author: kevinkelly
// @license: Apache License 2.0
// @file: units.go
// @time: 2026/10/28

package release

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// 参数校验。凡是要进入 systemd 单元 / sudoers / 外部命令的值都必须先通过这里。
var (
	clusterRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	userRe     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	listenRe   = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]*):[0-9]{1,5}$`)
	safeValRe  = regexp.MustCompile(`^[A-Za-z0-9_./:@\[\]=+,-]+$`)
	envChoices = map[string]bool{"test": true, "staging": true, "prod": true}
)

// UnitParams 是写进 systemd 单元 / sudoers 的安装参数。
type UnitParams struct {
	Env        string // test | staging | prod
	Cluster    string
	TidbUser   string
	Listen     string
	PublicURL  string // 可空
	Standalone bool   // 独立部署（--standalone）：不预设集群；默认不依赖本机 tiup / tidb 用户，没有 sudo 执行器、exec.json、sudoers
	LocalTiup  bool   // 独立部署 + 本机是 tiup 中控机（--local-tiup）：保留 sudo 执行器 / exec.json / sudoers，可一键导入本机集群并做全部运维
}

// UsesExec 报告该部署是否需要「本机 tiup + 受限 sudo 执行器」（tidb 用户、sudoers、exec.json、exec.key）。
func (p UnitParams) UsesExec() bool { return !p.Standalone || p.LocalTiup }

// Validate 校验全部参数。
func (p UnitParams) Validate() error {
	if !envChoices[p.Env] {
		return fmt.Errorf("--env 只能是 test | staging | prod（收到 %q）", p.Env)
	}
	if p.Cluster != "" && !clusterRe.MatchString(p.Cluster) { // 空表示沿用自动探测；install 另行要求必填
		return fmt.Errorf("集群名 %q 不合法：须匹配 ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$", p.Cluster)
	}
	if !userRe.MatchString(p.TidbUser) {
		return fmt.Errorf("tidb 用户名 %q 不合法：须匹配 ^[a-z_][a-z0-9_-]{0,31}$", p.TidbUser)
	}
	if !listenRe.MatchString(p.Listen) {
		return fmt.Errorf("监听地址 %q 不合法（应为 host:port，如 127.0.0.1:8080）", p.Listen)
	}
	if p.PublicURL != "" {
		u, err := url.Parse(p.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !safeValRe.MatchString(p.PublicURL) {
			return fmt.Errorf("--public-url %q 不合法（应为 http(s)://host[:port]，不含空白与引号）", p.PublicURL)
		}
	}
	return nil
}

// flagGroup 是 ExecStart 里的一个选项及其值。
type flagGroup struct {
	name string
	vals []string
}

// execStart 解析出的 ExecStart：二进制路径 + 选项组。
type execStart struct {
	bin    string
	groups []flagGroup
}

// findExecStart 在单元文本里定位 ExecStart 的逻辑行（含 \ 续行）：返回行起止下标（行号，左闭右开）与拼接后的文本。
func findExecStart(lines []string) (start, end int, text string, ok bool) {
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "ExecStart=") {
			continue
		}
		var parts []string
		j := i
		for ; j < len(lines); j++ {
			t := strings.TrimRight(lines[j], " \t\r")
			cont := strings.HasSuffix(t, `\`)
			parts = append(parts, strings.TrimSuffix(t, `\`))
			if !cont {
				j++
				break
			}
		}
		return i, j, strings.Join(parts, " "), true
	}
	return 0, 0, "", false
}

func parseExecStart(text string) (execStart, error) {
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "ExecStart="))
	toks := strings.Fields(text)
	if len(toks) == 0 {
		return execStart{}, fmt.Errorf("ExecStart 为空")
	}
	es := execStart{bin: toks[0]}
	for _, t := range toks[1:] {
		if strings.HasPrefix(t, "--") {
			es.groups = append(es.groups, flagGroup{name: t})
		} else if len(es.groups) > 0 {
			g := &es.groups[len(es.groups)-1]
			g.vals = append(g.vals, t)
		} else {
			return es, fmt.Errorf("ExecStart 里有无法识别的参数：%s", t)
		}
	}
	return es, nil
}

func (es *execStart) has(name string) bool {
	for _, g := range es.groups {
		if g.name == name {
			return true
		}
	}
	return false
}

// remove 删除这些选项（连同它们的值）。
func (es *execStart) remove(names ...string) {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	kept := es.groups[:0:0]
	for _, g := range es.groups {
		if !drop[g.name] {
			kept = append(kept, g)
		}
	}
	es.groups = kept
}

// setFlag 设置一个没有值的开关；已存在则不动，不存在则插到 after 之后（找不到则追加到末尾）。
func (es *execStart) setFlag(name, after string) {
	if es.has(name) {
		return
	}
	g := flagGroup{name: name}
	for i := range es.groups {
		if es.groups[i].name == after {
			es.groups = append(es.groups[:i+1], append([]flagGroup{g}, es.groups[i+1:]...)...)
			return
		}
	}
	es.groups = append(es.groups, g)
}

func (es *execStart) get(name string) (string, bool) {
	for _, g := range es.groups {
		if g.name == name && len(g.vals) > 0 {
			return g.vals[0], true
		}
	}
	return "", false
}

// set 设置某选项的值；不存在则在 after 之后插入（after 为空或找不到则追加到末尾）。
func (es *execStart) set(name, val, after string) {
	for i := range es.groups {
		if es.groups[i].name == name {
			es.groups[i].vals = []string{val}
			return
		}
	}
	g := flagGroup{name: name, vals: []string{val}}
	for i := range es.groups {
		if es.groups[i].name == after {
			es.groups = append(es.groups[:i+1], append([]flagGroup{g}, es.groups[i+1:]...)...)
			return
		}
	}
	es.groups = append(es.groups, g)
}

func (es execStart) render() []string {
	out := []string{"ExecStart=" + es.bin + ` \`}
	for i, g := range es.groups {
		ln := "  " + strings.Join(append([]string{g.name}, g.vals...), " ")
		if i < len(es.groups)-1 {
			ln += ` \`
		}
		out = append(out, ln)
	}
	return out
}

// ParseUnit 从已安装的单元文本里还原安装参数（--env / --cluster / --listen / --exec-sudo-user / --public-url）。
func ParseUnit(text string) (UnitParams, error) {
	lines := strings.Split(text, "\n")
	_, _, et, ok := findExecStart(lines)
	if !ok {
		return UnitParams{}, fmt.Errorf("systemd 单元里没有 ExecStart")
	}
	es, err := parseExecStart(et)
	if err != nil {
		return UnitParams{}, err
	}
	var p UnitParams
	p.Env, _ = es.get("--env")
	p.Cluster, _ = es.get("--cluster")
	p.TidbUser, _ = es.get("--exec-sudo-user")
	p.Listen, _ = es.get("--listen")
	p.PublicURL, _ = es.get("--public-url")
	p.Standalone = es.has("--standalone")
	p.LocalTiup = es.has("--local-tiup")
	if p.TidbUser == "" {
		p.TidbUser = "tidb"
	}
	if p.Env == "" {
		p.Env = "prod"
	}
	if p.Listen == "" {
		p.Listen = "127.0.0.1:8080"
	}
	return p, nil
}

// RenderUnit 以包内模板生成 systemd 单元：
//   - 把 /opt/tidbops/bin/tidbops-server、/opt/tidbops/web 换成稳定的 current 路径；
//   - 按参数设置 --env / --cluster / --listen / --exec-sudo-user / --home / --public-url。
func RenderUnit(tmpl string, p UnitParams) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	s := strings.ReplaceAll(tmpl, "/opt/tidbops/bin/tidbops-server", CurrentBin)
	s = strings.ReplaceAll(s, "/opt/tidbops/web", CurrentWeb)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	st, en, et, ok := findExecStart(lines)
	if !ok {
		return "", fmt.Errorf("包内 systemd 单元模板没有 ExecStart")
	}
	es, err := parseExecStart(et)
	if err != nil {
		return "", err
	}
	es.set("--listen", p.Listen, "--web")
	es.set("--env", p.Env, "--data-dir")
	switch {
	case p.Standalone && p.LocalTiup:
		// 中控机：平台不代表任何集群，但要经 sudo 执行器操作本机 tiup；没有「主集群」，也就没有 --cluster / 只读采集账号（SQL 账号在各集群接入里配）
		es.remove("--cluster", "--mysql-user")
		es.set("--exec-sudo-user", p.TidbUser, "--exec")
		es.set("--home", "/home/"+p.TidbUser, "")
		es.setFlag("--standalone", "--env")
		es.setFlag("--local-tiup", "--standalone")
	case p.Standalone:
		// 独立部署：平台不代表任何集群，没有 sudo 执行器与只读采集账号；这些选项都不要
		es.remove("--cluster", "--exec", "--exec-sudo-user", "--exec-bin", "--exec-policy", "--key-file", "--home", "--mysql-user", "--local-tiup")
		es.setFlag("--standalone", "--env")
	default:
		if p.Cluster != "" {
			es.set("--cluster", p.Cluster, "--listen")
		}
		es.set("--exec-sudo-user", p.TidbUser, "--exec")
		es.set("--home", "/home/"+p.TidbUser, "")
	}
	if p.PublicURL != "" {
		es.set("--public-url", p.PublicURL, "--env")
	}
	if es.bin != CurrentBin {
		return "", fmt.Errorf("包内 systemd 单元模板的 ExecStart 不是预期的 /opt/tidbops/bin/tidbops-server")
	}
	out := append([]string{}, lines[:st]...)
	out = append(out, es.render()...)
	out = append(out, lines[en:]...)
	if p.Standalone && !p.LocalTiup {
		out = standaloneUnitLines(out)
	}
	return strings.Join(out, "\n"), nil
}

// standaloneUnitLines 把单元里与 sudo 执行器有关的内容去掉：提到 sudo / --exec 的注释行已不适用；
// 没有 sudo 提权，就可以开 NoNewPrivileges=yes（更严格）。
func standaloneUnitLines(lines []string) []string {
	out := make([]string, 0, len(lines)+1)
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "#") && (strings.Contains(t, "sudo") || strings.Contains(t, "--exec")) {
			continue
		}
		if t == "NoNewPrivileges=no" {
			ln = "NoNewPrivileges=yes"
		}
		out = append(out, ln)
	}
	// 在文件开头补一行说明（对已渲染过的单元再渲染时不重复添加）
	const note = "# TiDB Ops 平台（Web 进程）· 独立部署（--standalone）：不代表任何集群，不依赖本机 tiup，也不需要提权；运行账号 tidbops：非 root、无登录 shell。"
	if len(out) > 0 && strings.TrimSpace(out[0]) == note {
		return out
	}
	return append([]string{note}, out...)
}

var sudoersLineRe = regexp.MustCompile(`(?m)^(tidbops\s+ALL=\()[^)]*(\))`)

// RenderSudoers 以包内模板生成 sudoers：命令路径换成 current 稳定路径，被代理用户换成 tidb 用户。
func RenderSudoers(tmpl, tidbUser string) (string, error) {
	if !userRe.MatchString(tidbUser) {
		return "", fmt.Errorf("tidb 用户名 %q 不合法", tidbUser)
	}
	s := strings.ReplaceAll(tmpl, "/opt/tidbops/bin/tidbops-server", CurrentBin)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !sudoersLineRe.MatchString(s) {
		return "", fmt.Errorf("包内 sudoers 模板里没有 tidbops ALL=(…) 行")
	}
	s = sudoersLineRe.ReplaceAllString(s, "${1}"+tidbUser+"${2}")
	if !strings.Contains(s, "NOPASSWD: "+CurrentBin+" exec --stdin") {
		return "", fmt.Errorf("包内 sudoers 模板的命令行不是预期的 exec --stdin")
	}
	if !strings.Contains(s, "NOPASSWD: "+CurrentBin+" shell-exec --stdin") {
		return "", fmt.Errorf("包内 sudoers 模板缺少「中控机命令」入口 shell-exec --stdin")
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s, nil
}

// RenderDropIn 生成放开沙箱可写路径的 systemd drop-in。服务单元带 ProtectSystem=strict / ProtectHome=read-only，
// 经 sudo 起来的执行器子进程继承这个沙箱：不放开的话 tiup 写不了 ~tidb/.tiup（日志、集群元数据），执行器也写不了临时目录。
// 路径前的 "-" 表示不存在时忽略（比如 tiup 还没装），否则服务会因为路径不存在而起不来；之后 tiup 装好再重启服务即可生效。
func RenderDropIn(tidbUser string) string {
	return "# 由 tidbops 安装器生成：升级不会覆盖本文件，可自行追加路径。\n" +
		"# 执行器（经 sudo 以 " + tidbUser + " 运行）继承服务的沙箱（ProtectSystem=strict / ProtectHome=read-only），\n" +
		"# 这里放开 tiup 要写的 ~" + tidbUser + "/.tiup 与执行器临时目录。路径前的 - 表示不存在时忽略。\n" +
		"[Service]\n" +
		"ReadWritePaths=-/home/" + tidbUser + "/.tiup -" + ExecTempDir + "\n"
}

// RenderExecJSONLocal 生成中控机模式的 exec.json：集群名白名单是通配 "*"（集群由管理员在页面一键导入，不必每次改策略文件）；
// 全局的 SQL 地址 / Alertmanager 地址必须去掉——多个集群共用一份策略时，它们会让其他集群的 SET CONFIG / 告警静默指向样例里那个集群
// （SQL 地址缺省回退到各集群拓扑里的 TiDB）。
func RenderExecJSONLocal(example string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(example), &m); err != nil {
		return nil, fmt.Errorf("包内 exec.json 样例不是合法 JSON: %w", err)
	}
	m["clusters"] = []string{"*"}
	for _, k := range []string{"mysqlHost", "mysqlPort", "alertURL"} {
		delete(m, k)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// RenderExecJSON 把包内样例里的集群名 prod-tidb-01 换成指定集群，并确认结果仍是合法 JSON。
func RenderExecJSON(example, cluster string) ([]byte, error) {
	if !clusterRe.MatchString(cluster) {
		return nil, fmt.Errorf("集群名 %q 不合法", cluster)
	}
	s := strings.ReplaceAll(example, `"prod-tidb-01"`, `"`+cluster+`"`)
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("包内 exec.json 样例不是合法 JSON")
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s), nil
}

// healthURL 由 --listen 得到健康检查地址（0.0.0.0 / [::] / 空主机统一用 127.0.0.1）。
func healthURL(listen string) string {
	i := strings.LastIndex(listen, ":")
	host, port := "127.0.0.1", listen
	if i >= 0 {
		host, port = listen[:i], listen[i+1:]
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port + "/api/health"
}
