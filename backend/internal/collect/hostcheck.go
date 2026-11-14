// @author: kevinkelly
// @license: Apache License 2.0
// @file: hostcheck.go
// @time: 2026/10/28

package collect

import (
	"fmt"
	"regexp"
	"strings"

	"tidbops/internal/model"
)

// checkLabels 是 `tiup cluster check` 各检查项的中文名（与前端主机池的预检项语义一致）。
var checkLabels = map[string]string{
	"os-version": "操作系统版本", "cpu-cores": "CPU 核数", "memory": "内存容量", "swap": "Swap 已关闭",
	"thp": "透明大页 (THP) 已关闭", "ntp": "时间同步 (chrony / ntp)", "timezone": "时区一致", "selinux": "SELinux 已关闭",
	"limits": "文件句柄 / 栈 ulimit", "disk": "数据盘 nodelalloc / noatime", "sysctl": "内核参数", "network": "网络",
	"command": "必需命令", "service": "系统服务（irqbalance 等）", "cpu-governor": "CPU 调频 performance",
	"epoll-exclusive": "epoll 独占唤醒", "fio": "磁盘 IO 性能", "permission": "目录权限", "listening-port": "服务端口未占用",
}

var (
	reLimit   = regexp.MustCompile(`(soft|hard) limit of '([a-z]+)' for user '([A-Za-z0-9_-]+)'`)
	reSysctl  = regexp.MustCompile(`([a-z][a-z0-9_]*(?:\.[a-z0-9_-]+)+)[^a-z]*?(?:should (?:be|set to)|expect(?:ed)?)\s+(?:(?:greater|more|less|smaller|lower|higher) than\s+|at least\s+|no more than\s+)?(-?\d+)`)
	reMount   = regexp.MustCompile(`mount point (/\S*)`)
	reMntOpt  = regexp.MustCompile(`'(noatime|nodelalloc)'`)
	reService = regexp.MustCompile(`service ([a-z][a-z0-9_.@-]*)`)
	reUser    = regexp.MustCompile(`user '([A-Za-z0-9_-]+)'`)
)

// limitValue 是 limits 检查建议的取值（TiDB 官方部署要求）。
var limitValue = map[string]string{"nofile": "1000000", "stack": "10240"}

// checkFix 按检查项与失败信息给出修复建议命令（仅展示，需 root 在目标主机上执行）；映射不到返回空串。
// 参考前端主机池预检（data.js CHECK_ITEMS / nodes.js applyFix）与 tiup cluster check --apply 能修复的项。
func checkFix(name, msg string) string {
	switch name {
	case "thp":
		return "echo never > /sys/kernel/mm/transparent_hugepage/enabled"
	case "swap":
		return `swapoff -a && sed -ri '/\sswap\s/ s/^/#/' /etc/fstab`
	case "selinux":
		return "setenforce 0 && sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config"
	case "limits":
		var lines []string
		seen := map[string]bool{}
		for _, m := range reLimit.FindAllStringSubmatch(msg, -1) {
			v := limitValue[m[2]]
			line := fmt.Sprintf("%s %s %s %s", m[3], m[1], m[2], v)
			if v == "" || seen[line] {
				continue
			}
			seen[line] = true
			lines = append(lines, fmt.Sprintf("echo %q >> /etc/security/limits.conf", line))
		}
		if len(lines) == 0 { // 信息里没有可解析的项：按部署用户给出标准两行
			user := "tidb"
			if m := reUser.FindStringSubmatch(msg); m != nil {
				user = m[1]
			}
			lines = []string{
				fmt.Sprintf("echo %q >> /etc/security/limits.conf", user+" soft nofile 1000000"),
				fmt.Sprintf("echo %q >> /etc/security/limits.conf", user+" hard nofile 1000000"),
			}
		}
		return strings.Join(lines, " && ")
	case "sysctl":
		var cmds []string
		for _, m := range reSysctl.FindAllStringSubmatch(msg, -1) {
			cmds = append(cmds, fmt.Sprintf("sysctl -w %s=%s", m[1], m[2]))
		}
		return strings.Join(cmds, " && ")
	case "ntp":
		if strings.Contains(msg, "offset") {
			return "chronyc makestep"
		}
		return "systemctl enable --now chronyd"
	case "disk":
		m := reMount.FindStringSubmatch(msg)
		if m == nil {
			return ""
		}
		var opts []string
		for _, o := range reMntOpt.FindAllStringSubmatch(msg, -1) {
			opts = append(opts, o[1])
		}
		if len(opts) == 0 {
			return ""
		}
		return fmt.Sprintf("mount -o remount,%s %s", strings.Join(opts, ","), m[1])
	case "cpu-governor":
		return "cpupower frequency-set --governor performance"
	case "service":
		if m := reService.FindStringSubmatch(msg); m != nil {
			return "systemctl enable --now " + m[1]
		}
	}
	return ""
}

// ParseCheckByHost 解析 `tiup cluster check` 的结果表（Node / Check / Result / Message），只取 host 这台主机的行，
// 每个检查项一条（同一项在该主机上有多行时取最严重的结果并合并说明）。R：Pass→ok，Warn→warn，Fail→bad；
// 失败 / 警告项附修复建议 Fix（映射不到为空）。结果顺序与输出里第一次出现的顺序一致；没有该主机的行返回 nil。
func ParseCheckByHost(out string, host string) []model.CheckItem {
	type agg struct {
		rank int // 0 pass 1 warn 2 fail
		msgs []string
		pass string // Pass 行的说明（所有行都通过时展示）
	}
	m := map[string]*agg{}
	var order []string
	rank := map[string]int{"Pass": 0, "Warn": 1, "Fail": 2}
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 || f[0] != host {
			continue
		}
		rk, ok := rank[f[2]]
		if !ok {
			continue
		}
		a := m[f[1]]
		if a == nil {
			a = &agg{}
			m[f[1]] = a
			order = append(order, f[1])
		}
		msg := strings.Join(f[3:], " ")
		if rk > a.rank {
			a.rank = rk
		}
		switch {
		case rk == 0 && a.pass == "":
			a.pass = msg
		case rk > 0 && msg != "":
			a.msgs = append(a.msgs, msg)
		}
	}
	var items []model.CheckItem
	for _, name := range order {
		a := m[name]
		it := model.CheckItem{G: "主机", N: name, L: checkLabels[name], R: [...]string{"ok", "warn", "bad"}[a.rank]}
		if a.rank == 0 {
			it.D = a.pass
			if it.D == "" {
				it.D = "正常"
			}
		} else {
			it.D = strings.Join(a.msgs, "；")
			if it.D == "" {
				it.D = map[int]string{1: "警告", 2: "失败"}[a.rank]
			}
			it.Fix = checkFix(name, it.D)
		}
		items = append(items, it)
	}
	return items
}
