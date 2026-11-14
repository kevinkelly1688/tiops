// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy.go
// @time: 2026/10/28

// Package shell 实现「中控机命令」：运维人员在平台页面里向中控机发送命令、实时看到输出。
//
// 它不是交互终端，而是一个带护栏的「一次一批命令」执行器：
//   - 每条命令先按风险分级（只读 / 变更 / 禁止）；只读直接执行，变更要另一名管理员批准，禁止的一律拒绝；
//   - 读密钥、读业务数据、把数据送出去、命令替换 / 嵌套 shell（会绕过检查）都在禁止之列；
//   - 一律以 tidb 身份执行，不需要 root 密码：只读命令经 sudoers 放行的固定入口（tidbops-server shell-exec）在平台进程内执行；
//     变更命令由同一个入口用 tidb 的 sudo 免密起 systemd-run 临时单元，在平台服务沙箱之外执行；
//     需要 root 权限的命令在整行开头写 sudo：带 sudo 的行一律按变更处理（需要另一名管理员批准），在临时单元里直接以 root 执行；
//   - 输出在写盘前就遮蔽口令 / 密钥 / 手机号 / 身份证号等（平台内执行由 Go 流式遮蔽，临时单元里由 sed），平台读取时再遮蔽一遍。
//
// 重要的限制：这里的命令分级是「基于文本的护栏」，防的是误操作和随手滥用，不是对抗恶意管理员的安全边界；
// 真正的边界是 sudoers 放行了谁、两人审批、全程审计与输出遮蔽。
package shell

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Level 是命令的风险级别。
type Level string

const (
	LevelRO   Level = "ro"   // 只读
	LevelMut  Level = "mut"  // 变更
	LevelDeny Level = "deny" // 禁止
)

// Item 是一条命令的判定。
type Item struct {
	Line  string `json:"line"` // 命令文本（已打码）
	Level Level  `json:"level"`
	Why   string `json:"why,omitempty"`
	Sudo  bool   `json:"sudo,omitempty"` // 整行以 sudo 开头（以 root 权限执行）
}

// 策略档位。
const (
	PolicyStrict = "strict" // 只允许只读
	PolicyStd    = "std"    // 只读直接执行，变更需审批（默认）
	PolicyLoose  = "loose"  // 变更免审批、只记录（不推荐）
)

// 判定动作。
const (
	ActRun     = "run"
	ActApprove = "approve"
	ActDeny    = "deny"
)

// Verdict 是对一批命令的整体判定。
type Verdict struct {
	Level  Level  `json:"level"`
	Action string `json:"action"`
	Why    string `json:"why,omitempty"`
	Items  []Item `json:"items"`
	Sudo   bool   `json:"sudo,omitempty"`  // 有以 sudo 开头的行
	Route  string `json:"route,omitempty"` // 执行路径（由 Manager 填）：inline = 平台进程内；unit = systemd-run 临时单元
}

// 限制。
const (
	MaxLines   = 10
	MaxLineLen = 2000
)

const (
	sedAddr = `(?:[0-9]+|\$)(?:,(?:[0-9]+|\$))?`
	sedPat  = `[A-Za-z0-9 _.:@%=,*^$\[\]+?|()-]{1,80}`
	sedRepl = `[A-Za-z0-9 _.:@%=,&-]{0,80}`
)

const (
	denyData  = "读取业务数据请用平台的只读 SQL 通道，这里不开放"
	denySend  = "会把数据传到别处或登录其他主机"
	denyPower = "关机 / 重启主机（请走「节点管理」的维护流程）"
	denyDisk  = "磁盘分区 / 格式化 / 块设备操作"
	denyAcct  = "修改账号 / 权限，或切换身份（需要 root 权限的命令请在整行开头加 sudo）"
	denySudo  = "sudo 只支持「sudo 命令」或「sudo -n 命令」放在整行开头（不支持 -u / -i / -s / -E 等选项，也不能放在管道或 ; 之后）"
	denyTTY   = "需要交互或持续输出，平台不是终端（看日志用 tail -n 200）"
	denyNet   = "改防火墙 / SELinux / 计划任务 / 内核模块（请用对应的运维流程）"
)

var (
	// defaultReadRoots 是「读文件内容」类命令（cat / head / tail / grep …）允许的目录：日志与系统信息。数据目录不在其中。
	defaultReadRoots = []string{"/var/log", "/tidb-deploy", "/home/tidb/.tiup/logs", "/etc/os-release", "/etc/hosts", "/etc/chrony.conf", "/proc/meminfo", "/proc/cpuinfo", "/proc/loadavg", "/proc/uptime"}

	reSyntax = regexp.MustCompile("\\$\\(|`|\\$\\{|\\$'|<\\(|>\\(|<<")
	reCtl    = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f]")
	reAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	reSysRO  = regexp.MustCompile(`^(status|is-active|is-enabled|is-failed|list-units|list-unit-files|show)$`)
	reChrony = regexp.MustCompile(`^(tracking|sources|sourcestats|activity|ntpdata|serverstats)$`)

	// 命令前的 VAR=x 赋值只放行这些无害的变量；PATH、LD_PRELOAD、BASH_ENV 等能让「只读命令」去执行别的程序，一律按变更处理。
	reSafeEnv = regexp.MustCompile(`^(LC_[A-Z_]+|LANG|TZ|COLUMNS|LINES)$`)

	// sed 只读脚本：整段必须完全匹配下面的写法之一（可选行号 / 行范围 + p 或 d；/文本/p|d；s/文本/替换/ 可带 g）。
	// 字符集里没有 ; { } 换行 反斜杠，所以不能串联第二条命令，也不可能出现 e / w / r 等会执行或写文件的命令与标志。
	reSedScript = regexp.MustCompile(`^(?:` + sedAddr + `[pd]|p|d|/` + sedPat + `/[pd]|(?:` + sedAddr + `)?s/` + sedPat + `/` + sedRepl + `/g?)$`)

	// 只读命令里这些选项会读取任意文件或执行程序，带上就按变更处理。
	roDangerPrefix = []string{"--files0-from", "--compress-program", "--exclude-from", "--include-from"}

	// 禁止规则：每条是 (匹配, 原因)。匹配在「去掉引号与反斜杠」之后的文本上做，防止用 ".ssh"、c\at 之类拆词绕过。
	deny = []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`\.ssh\b|id_rsa|id_ed25519|id_ecdsa|\.pem\b|\.key\b|\.p12\b|\.pfx\b|/etc/(shadow|gshadow|sudoers|tidb-ops)|/var/lib/(tidbops|tidb-deploy)|root-password|/proc/[^ ]*/environ|master\.key|exec\.key|\.pgpass|\.my\.cnf|\.mylogin|credentials`), "读取密钥、口令或平台数据文件"},
		{regexp.MustCompile(`\btiup\s+ctl\b|\btiup\s+(dumpling|tidb-lightning)\b`), "读取业务数据请用平台的只读 SQL 通道，这里不开放"},
		{regexp.MustCompile(`\b(curl|wget)\b[^|;&]*(\s-d\b|\s--data\S*|\s-T\b|\s-F\b|\s--upload-file|--post)`), "会把数据传到别处或登录其他主机"},
		{regexp.MustCompile(`\btiup\s+cluster\s+(exec|push|pull|edit-config|meta|scale-in|scale-out|patch|import|template|replay|rename)\b`), "会在所有集群主机上执行 / 传文件、改拓扑，或展示含口令的配置（请用「节点管理」等有审批的页面）"},
		{regexp.MustCompile(`\brm\s+-[a-z]*(rf|fr)[a-z]*\s+(/|/\*|~|\$HOME|/tidb-data|/tidb-deploy)(\s|$)`), "递归强制删除根目录、家目录或集群数据"},
		{regexp.MustCompile(`\bdd\b[^|]*\bof=/dev/`), "磁盘分区 / 格式化 / 直接写块设备"},
		{regexp.MustCompile(`\binit\s+[06]\b|\bsystemctl\s+(reboot|poweroff|halt|kexec|isolate|rescue|emergency)\b`), "关机 / 重启主机（请走「节点管理」的维护流程）"},
		{regexp.MustCompile(`\btiup\s+cluster\s+(destroy|clean)\b`), "销毁集群（只能用部署工具的 destroy 并确认集群名）"},
		{regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*\S*(ba)?sh\b`), "下载后直接执行"},
		{regexp.MustCompile(`\btail\b[^|;&]*\s(-[a-zA-Z]*f\b|--follow)|\bjournalctl\b[^|;&]*\s(-[a-zA-Z]*f\b|--follow)`), "需要交互或持续输出，平台不是终端（看日志用 tail -n 200）"},
	}

	// 命令名黑名单：只在「命令位置」（每个简单命令的第一个词）检查，所以 grep su /var/log/messages 这类把它们当参数的写法不会误伤。
	denyCmd = map[string]string{
		"env": "会暴露环境变量里的口令", "printenv": "会暴露环境变量里的口令", "history": "会暴露历史里的口令", "export": "会暴露环境变量里的口令",
		"mysql": denyData, "mariadb": denyData, "mycli": denyData, "dumpling": denyData, "mydumper": denyData, "myloader": denyData, "tidb-lightning": denyData,
		"sqlite3": denyData, "psql": denyData, "redis-cli": denyData, "mongo": denyData,
		"scp": denySend, "sftp": denySend, "rsync": denySend, "nc": denySend, "ncat": denySend, "netcat": denySend, "socat": denySend, "ftp": denySend, "tftp": denySend,
		"telnet": denySend, "ssh": denySend, "sshpass": denySend,
		"shutdown": denyPower, "reboot": denyPower, "halt": denyPower, "poweroff": denyPower,
		"fdisk": denyDisk, "parted": denyDisk, "wipefs": denyDisk, "mkswap": denyDisk, "losetup": denyDisk,
		"passwd": denyAcct, "userdel": denyAcct, "usermod": denyAcct, "useradd": denyAcct, "groupadd": denyAcct, "groupdel": denyAcct, "chpasswd": denyAcct,
		"visudo": denyAcct, "su": denyAcct, "pkexec": denyAcct, "doas": denyAcct,
		"top": denyTTY, "htop": denyTTY, "iotop": denyTTY, "vi": denyTTY, "vim": denyTTY, "nano": denyTTY, "less": denyTTY, "more": denyTTY, "watch": denyTTY,
		"screen": denyTTY, "tmux": denyTTY, "nmtui": denyTTY,
		"iptables": denyNet, "ip6tables": denyNet, "nft": denyNet, "firewall-cmd": denyNet, "setenforce": denyNet, "crontab": denyNet, "at": denyNet,
		"insmod": denyNet, "rmmod": denyNet, "modprobe": denyNet,
	}

	roBase = map[string]bool{"ls": true, "cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "wc": true, "df": true, "du": true, "free": true,
		"uptime": true, "uname": true, "whoami": true, "id": true, "ps": true, "ss": true, "netstat": true, "ping": true, "stat": true, "lsblk": true,
		"echo": true, "pwd": true, "cut": true, "sort": true, "date": true, "hostname": true, "find": true, "dmesg": true, "ip": true, "journalctl": true,
		"timedatectl": true, "chronyc": true, "sed": true, "tiup": true, "systemctl": true}
	contentCmds = map[string]bool{"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "wc": true, "cut": true, "sort": true, "sed": true}
	paramFlags  = map[string]map[string]bool{
		"head": {"-n": true, "-c": true}, "tail": {"-n": true, "-c": true}, "cut": {"-d": true, "-f": true, "-c": true, "-b": true},
		"sort": {"-k": true, "-t": true, "-S": true}, "grep": {"-m": true, "-A": true, "-B": true, "-C": true, "-e": true}, "egrep": {"-m": true, "-A": true, "-B": true, "-C": true, "-e": true},
		"sed": {"-e": true}, "wc": {}, "cat": {},
	}
	mutBase = map[string]bool{"kill": true, "pkill": true, "killall": true, "cp": true, "mv": true, "rm": true, "mkdir": true, "rmdir": true, "touch": true, "chmod": true, "chown": true,
		"ln": true, "yum": true, "dnf": true, "rpm": true, "tee": true, "sysctl": true, "mount": true, "umount": true, "curl": true, "wget": true, "tar": true, "unzip": true,
		"gzip": true, "gunzip": true, "truncate": true, "sync": true, "swapoff": true, "swapon": true, "awk": true, "ulimit": true, "nohup": true}
)

// stripQ 去掉引号与反斜杠（用于检测被拆开的敏感词）。
func stripQ(s string) string {
	return strings.NewReplacer("'", "", "\"", "", "\\", "").Replace(s)
}

// splitArgs 按 shell 的引号规则把一段简单命令拆成参数（不展开任何变量）。
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	var q rune
	for _, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			q, in = r, true
		case r == ' ' || r == '\t':
			if in || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func under(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// pathOK 判断 p 是不是允许读内容的绝对路径（不含 ..；通配符前的部分必须在允许的目录下）。
func pathOK(p string, roots []string) bool {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return false
	}
	base := p
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		base = p[:i]
		if !strings.HasSuffix(base, "/") { // /var/l* 这种在目录名中间通配：不允许
			return false
		}
		base = strings.TrimSuffix(base, "/")
	}
	return under(path.Clean(base), roots)
}

// contentFiles 取 cat / head / grep … 这类命令的文件参数（不含模式、数值和选项值）。
func contentFiles(cmd string, args []string) (files []string, recursive bool) {
	needPat := cmd == "grep" || cmd == "egrep" || cmd == "sed"
	pf := paramFlags[cmd]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			continue
		case (cmd == "grep" || cmd == "egrep") && a == "-f":
			if i+1 < len(args) {
				files = append(files, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if (cmd == "grep" || cmd == "egrep") && (strings.Contains(a, "r") || strings.Contains(a, "R")) && !strings.HasPrefix(a, "--") {
				recursive = true
			}
			if a == "--recursive" || a == "--dereference-recursive" {
				recursive = true
			}
			if a == "-e" && (cmd == "grep" || cmd == "egrep" || cmd == "sed") {
				needPat = false
			}
			if pf[a] && i+1 < len(args) {
				i++
			}
		default:
			if needPat {
				needPat = false
				continue
			}
			files = append(files, a)
		}
	}
	return
}

func mask1(line string) string {
	return MaskCommand(line)
}

type scanFlags struct{ write, read, bg bool }

// scan 按 shell 的引号规则把一行拆成简单命令（以 ; | || && 分隔），同时识别输出重定向、输入重定向和后台运行。
// 引号里的 | ; < > & 不算；2>&1、>/dev/null、2>/dev/null、&>/dev/null 不算「写文件」；重定向的目标词不进入命令参数。
func scan(line string) (segs []string, f scanFlags) {
	rs := []rune(line)
	var cur strings.Builder
	var q rune
	flush := func() {
		segs = append(segs, cur.String())
		cur.Reset()
	}
	skipSpaces := func(i int) int {
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
			i++
		}
		return i
	}
	// target 读取重定向目标词，返回结束位置与目标文本
	target := func(i int) (int, string) {
		i = skipSpaces(i)
		st := i
		for i < len(rs) && rs[i] != ' ' && rs[i] != '\t' && rs[i] != ';' && rs[i] != '|' && rs[i] != '&' {
			i++
		}
		return i, string(rs[st:i])
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if q != 0 {
			cur.WriteRune(r)
			if r == q {
				q = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			q = r
			cur.WriteRune(r)
		case ';':
			flush()
		case '|':
			if i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			flush()
		case '&':
			switch {
			case i+1 < len(rs) && rs[i+1] == '&':
				i++
				flush()
			case i+1 < len(rs) && rs[i+1] == '>': // &>file
				j := i + 2
				if j < len(rs) && rs[j] == '>' {
					j++
				}
				e, t := target(j)
				if t != "/dev/null" {
					f.write = true
				}
				i = e - 1
			default:
				f.bg = true
			}
		case '>':
			// 命令里紧挨着 > 的数字（2>）是文件描述符，不是参数
			if t := strings.TrimRight(cur.String(), "0123456789"); len(t) < cur.Len() && (len(t) == 0 || t[len(t)-1] == ' ' || t[len(t)-1] == '	') {
				cur.Reset()
				cur.WriteString(t)
			}
			j := i + 1
			if j < len(rs) && rs[j] == '>' {
				j++
			}
			if j < len(rs) && rs[j] == '&' { // 2>&1
				e := j + 1
				for e < len(rs) && rs[e] >= '0' && rs[e] <= '9' {
					e++
				}
				i = e - 1
				continue
			}
			e, t := target(j)
			if t != "/dev/null" {
				f.write = true
			}
			i = e - 1
		case '<':
			f.read = true
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return
}

// ClassifyLine 判定一条命令（不含空行）。roots 是「读文件内容」允许的目录。只按文本判断路径（不解析符号链接）。
func ClassifyLine(line string, roots []string) Item { return classifyLine(line, roots, nil) }

// classifyLine 同 ClassifyLine；fsv 非空时，读内容的路径还要解析符号链接后再比对允许目录。
func classifyLine(line string, roots []string, fsv *fsView) Item {
	it := Item{Line: mask1(line), Level: LevelRO}
	fail := func(l Level, why string) Item { it.Level, it.Why = l, why; return it }
	if len(line) > MaxLineLen {
		return fail(LevelDeny, fmt.Sprintf("单条命令不能超过 %d 个字符", MaxLineLen))
	}
	if reCtl.MatchString(line) {
		return fail(LevelDeny, "含控制字符")
	}
	plain := stripQ(line)
	for _, d := range deny {
		if d.re.MatchString(plain) || d.re.MatchString(line) {
			return fail(LevelDeny, d.why)
		}
	}
	if reSyntax.MatchString(line) {
		return fail(LevelDeny, "不支持命令替换、进程替换、here-doc（它们会绕过命令检查）；请拆成单条简单命令")
	}
	segments, sc := scan(line)
	if sc.bg {
		return fail(LevelDeny, "不支持后台运行（&）")
	}
	if sc.read {
		return fail(LevelDeny, "不支持输入重定向（<）：请把文件路径作为命令参数")
	}
	mutWhy := ""
	if sc.write {
		mutWhy = "有输出重定向到文件"
	}
	set := func(w string) {
		if mutWhy == "" {
			mutWhy = w
		}
	}
	for i, seg := range segments {
		w, sudo, d := classifySeg(strings.TrimSpace(seg), i == 0, roots, fsv)
		if d != nil {
			d.Line = it.Line
			return *d
		}
		if sudo {
			it.Sudo = true
			if w == "" { // 文本护栏分不清「只读」的 sudo 命令是否真的只读（读 root 才能读的任何文件、被替换的程序…），一律走审批
				w = "以 sudo（root 权限）执行的命令一律按变更处理，需要另一名管理员批准"
			}
		}
		set(w)
	}
	if mutWhy != "" {
		return fail(LevelMut, mutWhy)
	}
	return it
}

// SplitSudo 识别整行开头的 sudo 前缀：返回去掉「sudo」/「sudo -n」后的命令与是否有前缀。
// 只认这两种写法（与护栏一致）；其他 sudo 选项由护栏拒绝，这里不处理。
func SplitSudo(line string) (rest string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "sudo" || !strings.HasPrefix(t, "sudo ") && !strings.HasPrefix(t, "sudo\t") {
		return line, false
	}
	t = strings.TrimSpace(t[len("sudo"):])
	if t == "-n" {
		return "", true
	}
	if strings.HasPrefix(t, "-n ") || strings.HasPrefix(t, "-n\t") {
		t = strings.TrimSpace(t[2:])
	}
	return t, true
}

// classifySeg 判定一个简单命令：返回需要「变更」的原因（空 = 只读）、是否带 sudo 前缀，或一个「禁止」的判定。
// first 表示这是整行的第一个简单命令：只有这里允许 sudo 前缀。
func classifySeg(seg string, first bool, roots []string, fsv *fsView) (mutWhy string, sudo bool, deny *Item) {
	set := func(w string) {
		if mutWhy == "" {
			mutWhy = w
		}
	}
	fail := func(why string) (string, bool, *Item) { return "", false, &Item{Level: LevelDeny, Why: why} }
	if seg == "" {
		return
	}
	args := splitArgs(seg)
	if len(args) > 0 && args[0] == "sudo" {
		if !first {
			return fail(denySudo)
		}
		sudo = true
		args = args[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			if args[0] != "-n" {
				return fail(denySudo)
			}
			args = args[1:]
		}
		if len(args) == 0 {
			return fail(denySudo)
		}
	}
	for len(args) > 0 && reAssign.MatchString(args[0]) {
		if name, _, _ := strings.Cut(args[0], "="); !reSafeEnv.MatchString(name) {
			set("命令前带环境变量赋值「" + name + "=…」（如 PATH、LD_PRELOAD 会改变命令实际执行的程序），按变更处理")
		}
		args = args[1:] // VAR=x cmd
	}
	if len(args) == 0 {
		return "只有变量赋值", sudo, nil
	}
	f, a := path.Base(args[0]), args[1:]
	if strings.Contains(args[0], "/") {
		// 只按文件名判定会被同名的自备程序冒充（/tmp/x/cat 看起来像 cat）：带目录的命令一律不当只读
		set("以路径调用的命令（" + args[0] + "）无法确认是系统自带的程序，按变更处理")
	}
	if f == "sudo" {
		return fail(denySudo)
	}
	if why, bad := denyCmd[f]; bad {
		return fail(why)
	}
	if strings.HasPrefix(f, "mkfs") {
		return fail(denyDisk)
	}
	has := func(flags ...string) bool {
		for _, x := range a {
			for _, fl := range flags {
				if x == fl {
					return true
				}
			}
		}
		return false
	}
	switch {
	case mutBase[f]:
		return "「" + f + "」会改动系统或文件", sudo, nil
	case !roBase[f]:
		return "「" + f + "」不在平台认识的命令里，按变更处理", sudo, nil
	}
	okPath := func(p string) bool {
		if !pathOK(p, roots) {
			return false
		}
		return fsv == nil || fsv.within(p, roots)
	}
	// 只读命令里带「会改东西」的用法
	switch f {
	case "tiup":
		switch {
		case len(a) >= 1 && (a[0] == "--version" || a[0] == "-v"):
		case len(a) >= 2 && a[0] == "cluster" && (a[1] == "list" || a[1] == "display" || a[1] == "audit" || a[1] == "--version"):
		case len(a) >= 2 && a[0] == "cluster" && a[1] == "check" && !has("--apply"):
		default:
			set("「tiup " + strings.Join(firstN(a, 2), " ") + "」会改动集群或不在只读白名单里")
		}
	case "systemctl":
		if len(a) == 0 || !reSysRO.MatchString(a[0]) {
			set("「systemctl " + strings.Join(firstN(a, 1), " ") + "」会改动服务")
		}
	case "ip":
		if has("add", "del", "delete", "set", "flush", "change", "replace", "append") {
			set("「ip」带有改动网络配置的子命令")
		}
		if has("-b", "-batch", "--batch") {
			set("ip -batch 会按文件内容执行任意 ip 子命令")
		}
	case "date":
		if has("-s", "--set") {
			set("设置系统时间")
		}
		for _, x := range a {
			if x == "-f" || strings.HasPrefix(x, "--file") || strings.HasPrefix(x, "-f") && !strings.HasPrefix(x, "--") {
				set("date -f 会把任意文件的内容当作日期读出来")
			}
		}
	case "journalctl":
		for _, x := range a {
			if x == "-D" || strings.HasPrefix(x, "-D") && !strings.HasPrefix(x, "--") || strings.HasPrefix(x, "--directory") || strings.HasPrefix(x, "--file") || strings.HasPrefix(x, "--root") || strings.HasPrefix(x, "--image") {
				set("journalctl 读取指定目录 / 文件里的日志，不在只读白名单里")
			}
		}
	case "grep", "egrep":
		for _, x := range a {
			if x == "--dereference-recursive" || strings.HasPrefix(x, "-") && !strings.HasPrefix(x, "--") && strings.Contains(x, "R") {
				set("grep -R 会跟随符号链接，可能读到允许目录之外的文件")
			}
		}
	case "hostname":
		for _, x := range a {
			if !strings.HasPrefix(x, "-") {
				set("设置主机名")
			}
		}
	case "dmesg":
		if has("-c", "-C", "--clear", "--read-clear") {
			set("清空内核日志")
		}
	case "timedatectl":
		if len(a) > 0 && strings.HasPrefix(a[0], "set-") {
			set("改时间 / 时区设置")
		}
	case "chronyc":
		if len(a) == 0 || !reChrony.MatchString(a[0]) {
			set("「chronyc」只读子命令之外")
		}
	case "find":
		if has("-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls") {
			set("find 带有删除 / 执行 / 写文件的参数")
		}
		if mutWhy == "" { // 起始目录也要在允许的目录里：不能用 find 满盘枚举文件名
			i := 0
			for i < len(a) && (a[i] == "-H" || a[i] == "-L" || a[i] == "-P") {
				i++
			}
			n := 0
			for ; i < len(a) && !strings.HasPrefix(a[i], "-") && a[i] != "(" && a[i] != ")" && a[i] != "!"; i++ {
				n++
				if !okPath(a[i]) {
					return fail(fmt.Sprintf("「find」的起始目录只允许日志与系统信息目录（%s 等），%s 不在范围内；请用绝对路径", strings.Join(firstN(roots, 3), "、"), a[i]))
				}
			}
			if n == 0 {
				return fail("find 要写明起始目录（绝对路径，且在日志 / 系统信息目录里）")
			}
		}
	case "sed":
		for _, x := range a {
			if x == "-i" || strings.HasPrefix(x, "-i") && !strings.HasPrefix(x, "--") || x == "--in-place" {
				set("sed -i 会改文件")
			}
		}
		if mutWhy == "" && !sedSafe(a) {
			set("sed 的脚本不在只读白名单里（只放行行号 + p / d、/文本/p、s/文本/替换/g；e / w / r 等命令会执行程序或写文件）")
		}
	case "sort":
		if has("-o") {
			set("sort -o 会写文件")
		}
	}
	for _, x := range a {
		if strings.Contains(x, "..") && strings.Contains(x, "/") {
			return fail("路径里不能有 ..")
		}
		for _, d := range roDangerPrefix {
			if strings.HasPrefix(x, d) {
				set("「" + f + "」的 " + d + " 会读取任意文件或执行程序，按变更处理")
			}
		}
	}
	if contentCmds[f] && mutWhy == "" { // 已经是变更的命令要人工审批，这里不再卡路径范围
		files, rec := contentFiles(f, a)
		if rec && len(files) == 0 {
			return fail("grep -r 要写明目录（绝对路径，且在日志 / 系统信息目录里）")
		}
		for _, p := range files {
			if !okPath(p) {
				return fail(fmt.Sprintf("「%s」读文件内容只允许日志与系统信息目录（%s 等），%s 不在范围内（含指向目录之外的符号链接）；请用绝对路径", f, strings.Join(firstN(roots, 3), "、"), p))
			}
		}
	}
	return mutWhy, sudo, nil
}

// sedSafe 判断 sed 的参数是否只是「只读脚本」：只允许 -n / -E / -r 和一段（-e 给出的，或第一个非选项参数）符合 reSedScript 的脚本。
func sedSafe(a []string) bool {
	scripts := 0
	for i := 0; i < len(a); i++ {
		x := a[i]
		switch {
		case x == "-n" || x == "-E" || x == "-r" || x == "-nE" || x == "-En" || x == "-nr" || x == "-rn":
		case x == "-e":
			if i+1 >= len(a) || !reSedScript.MatchString(a[i+1]) {
				return false
			}
			i++
			scripts++
		case x == "--":
		case strings.HasPrefix(x, "-") && len(x) > 1:
			return false
		default:
			if scripts == 0 {
				if !reSedScript.MatchString(x) {
					return false
				}
				scripts++
			}
		}
	}
	return scripts == 1
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ReadRoots 返回读内容命令允许的目录：内置的加上设置里追加的。
func ReadRoots(extra []string) []string {
	return append(append([]string{}, defaultReadRoots...), extra...)
}

// Decide 按策略档位对一批命令作整体判定（执行身份固定为 tidb；带 sudo 的行一律按变更处理）。只按文本判断路径。
func Decide(policy string, lines []string, extraRoots []string) Verdict {
	return decide(policy, lines, extraRoots, nil)
}

// DecideOnHost 同 Decide，但读内容的路径会在本机文件系统上解析符号链接后再比对允许目录。
// strict = true 用在 tidb 身份的 shell-exec 子进程里（真正执行前的最后一道判定）：解析出错一律拒绝；
// strict = false 用在平台进程的预判定：平台账号可能没有权限穿过 tidb 的目录，解析不了就退回文本判断，由子进程兜底。
func DecideOnHost(policy string, lines []string, extraRoots []string, strict bool) Verdict {
	return decide(policy, lines, extraRoots, hostFS(strict))
}

func decide(policy string, lines []string, extraRoots []string, fsv *fsView) Verdict {
	v := Verdict{Level: LevelRO, Action: ActRun}
	roots := ReadRoots(extraRoots)
	if len(lines) == 0 {
		return Verdict{Level: LevelRO, Action: ActDeny, Why: "没有命令"}
	}
	if len(lines) > MaxLines {
		return Verdict{Level: LevelDeny, Action: ActDeny, Why: fmt.Sprintf("一次最多 %d 条命令", MaxLines)}
	}
	rank := map[Level]int{LevelRO: 0, LevelMut: 1, LevelDeny: 2}
	for _, l := range lines {
		it := classifyLine(l, roots, fsv)
		v.Items = append(v.Items, it)
		if it.Sudo {
			v.Sudo = true
		}
		if rank[it.Level] > rank[v.Level] {
			v.Level, v.Why = it.Level, it.Why
		}
	}
	if v.Level == LevelDeny {
		v.Action = ActDeny
		return v
	}
	switch {
	case v.Level == LevelRO:
		v.Action = ActRun
	case policy == PolicyStrict:
		v.Action, v.Why = ActDeny, "严格档位只允许只读命令；变更请走「节点管理」「变更排期」等有审批流程的页面"
	case policy == PolicyLoose:
		v.Action = ActRun
	default:
		v.Action = ActApprove
	}
	return v
}
