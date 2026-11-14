// @author: kevinkelly
// @license: Apache License 2.0
// @file: lb.go
// @time: 2026/10/28

// Package lb 是「负载均衡层」的登记簿：手工登记的负载均衡入口（HAProxy / LVS / Nginx / 云 SLB / F5 / 自建 TiProxy / 其他）
// 的数据模型、校验，以及登记的后端与拓扑里 TiDB 实例的一致性比较。
//
// 它只做纯计算，不做 IO：持久化放在 server 的 settings.json（每集群一份），自动发现的 TiProxy 来自 collect（model.Proxy）。
package lb

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// 上限与长度限制。
const (
	MaxEntries  = 20  // 每个集群最多登记条数
	MaxBackends = 64  // 每条登记最多后端数
	MaxName     = 40  // 名称最大字数
	MaxNote     = 200 // 备注最大字数
)

// Types 是允许的类型。tiproxy 手工登记用于「tiup 之外自己部署的 TiProxy」；tiup 部署的由拓扑自动发现。
var Types = []string{"haproxy", "lvs", "nginx", "slb", "f5", "tiproxy", "other"}

var typeLabels = map[string]string{"haproxy": "HAProxy", "lvs": "LVS", "nginx": "Nginx", "slb": "云 SLB", "f5": "F5", "tiproxy": "TiProxy", "other": "其他"}

// TypeLabel 是类型的展示名；未知类型原样返回。
func TypeLabel(t string) string {
	if l, ok := typeLabels[t]; ok {
		return l
	}
	return t
}

// Entry 是一条手工登记的负载均衡。id 创建后不可改。
type Entry struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Addr      string    `json:"addr"`     // 对外入口 host:port（VIP / 域名），不含协议与凭据
	Backends  []string  `json:"backends"` // 后端 TiDB 地址 host:port；可为空（空则不做一致性检查）
	Note      string    `json:"note"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updatedBy"`
}

// Input 是新增 / 更新的请求体。更新时为 nil 的字段表示不修改；新增时 id / type / name / addr 必填。
// 更新时 id 若给出必须与路径一致（id 不可改）。
type Input struct {
	ID       string    `json:"id"`
	Type     *string   `json:"type"`
	Name     *string   `json:"name"`
	Addr     *string   `json:"addr"`
	Backends *[]string `json:"backends"`
	Note     *string   `json:"note"`
}

var reID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// ValidID 判断登记 id 是否合法：小写字母开头，小写字母 / 数字 / 连字符，2–32 位。
func ValidID(id string) bool { return reID.MatchString(id) }

// ValidType 判断类型是否合法。
func ValidType(t string) bool {
	for _, x := range Types {
		if x == t {
			return true
		}
	}
	return false
}

var reLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_-]{0,61}[A-Za-z0-9])?$`)

// ValidHost 判断主机是 IP 或合法主机名。不接受仅由数字和点组成的非 IP（如 10.0.0）。
func ValidHost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	if len(h) > 253 {
		return false
	}
	allNumeric := true
	for _, c := range h {
		if !(c >= '0' && c <= '9') && c != '.' {
			allNumeric = false
		}
	}
	if allNumeric {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !reLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// NormalizeAddr 校验并规整 host:port：去首尾空白；不得含协议、凭据、路径、空白；端口 1–65535（纯数字）。
// 返回规整后的地址（主机小写，IPv6 带方括号）。
func NormalizeAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("地址不能为空")
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("地址 %q 不能包含协议（如 http://），只填 主机:端口", s)
	}
	if strings.ContainsAny(s, "@/?#\\") {
		return "", fmt.Errorf("地址 %q 不能包含凭据或路径，只填 主机:端口", s)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("地址 %q 不能包含空白或控制字符", s)
		}
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("地址 %q 格式应为 主机:端口（IPv6 请写成 [::1]:3306）", s)
	}
	if !ValidHost(host) {
		return "", fmt.Errorf("地址 %q 的主机部分不是合法的 IP 或主机名", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
		return "", fmt.Errorf("地址 %q 的端口需为 1–65535 的整数", s)
	}
	return joinHostPort(host, n), nil
}

func joinHostPort(host string, port int) string {
	host = strings.ToLower(host)
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func runeLen(s string) int { return len([]rune(s)) }

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' {
			return true
		}
	}
	return false
}

// Fields 是经过校验、规整后的可编辑字段。
type Fields struct {
	Type, Name, Addr, Note string
	Backends               []string
}

// Apply 把 in 应用到 base（新增时是零值 Fields，更新时是现有值）并校验，返回新值与被修改的字段名（按固定顺序）。
// add=true 时 type / name / addr 必须给出。错误信息可直接展示给用户。
func Apply(base Fields, in Input, add bool) (Fields, []string, error) {
	f := base
	var changed []string
	if add {
		if in.Type == nil || in.Name == nil || in.Addr == nil {
			return f, nil, fmt.Errorf("type、name、addr 必填")
		}
	}
	if in.Type != nil {
		t := strings.TrimSpace(*in.Type)
		if !ValidType(t) {
			return f, nil, fmt.Errorf("type 需为 %s 之一", strings.Join(Types, " | "))
		}
		if t != f.Type {
			changed = append(changed, "type")
		}
		f.Type = t
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || runeLen(n) > MaxName {
			return f, nil, fmt.Errorf("name 需为 1–%d 个字", MaxName)
		}
		if hasControl(n) || strings.Contains(n, "\n") {
			return f, nil, fmt.Errorf("name 不能包含换行或控制字符")
		}
		if n != f.Name {
			changed = append(changed, "name")
		}
		f.Name = n
	}
	if in.Addr != nil {
		a, err := NormalizeAddr(*in.Addr)
		if err != nil {
			return f, nil, fmt.Errorf("addr：%w", err)
		}
		if a != f.Addr {
			changed = append(changed, "addr")
		}
		f.Addr = a
	}
	if in.Backends != nil {
		if len(*in.Backends) > MaxBackends {
			return f, nil, fmt.Errorf("backends 最多 %d 个", MaxBackends)
		}
		seen := map[string]bool{}
		out := []string{}
		for _, b := range *in.Backends {
			a, err := NormalizeAddr(b)
			if err != nil {
				return f, nil, fmt.Errorf("backends：%w", err)
			}
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
		if !sameList(out, f.Backends) {
			changed = append(changed, "backends")
		}
		f.Backends = out
	}
	if f.Backends == nil {
		f.Backends = []string{}
	}
	if in.Note != nil {
		n := strings.TrimSpace(*in.Note)
		if runeLen(n) > MaxNote {
			return f, nil, fmt.Errorf("note 最多 %d 个字", MaxNote)
		}
		if hasControl(n) {
			return f, nil, fmt.Errorf("note 不能包含控制字符")
		}
		if n != f.Note {
			changed = append(changed, "note")
		}
		f.Note = n
	}
	return f, changed, nil
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Check 是一条登记与拓扑的一致性提示。
type Check struct {
	ID      string   `json:"id"`
	Missing []string `json:"missing"` // 拓扑里有、登记的后端里没有的 TiDB
	Extra   []string `json:"extra"`   // 登记了、拓扑里没有的后端
}

// Compare 把每条登记（backends 非空的）与拓扑里的 TiDB SQL 地址比较。tidb 是拓扑里 TiDB 的 host:port（SQL 端口）。
// 比较前对双方规整（主机小写）；结果按字典序，Missing / Extra 恒为非 nil 数组。
func Compare(entries []Entry, tidb []string) []Check {
	topo := map[string]bool{}
	for _, a := range tidb {
		topo[canon(a)] = true
	}
	out := []Check{}
	for _, e := range entries {
		if len(e.Backends) == 0 {
			continue
		}
		have := map[string]bool{}
		c := Check{ID: e.ID, Missing: []string{}, Extra: []string{}}
		for _, b := range e.Backends {
			k := canon(b)
			have[k] = true
			if !topo[k] {
				c.Extra = append(c.Extra, b)
			}
		}
		for _, a := range tidb {
			if !have[canon(a)] {
				c.Missing = append(c.Missing, a)
			}
		}
		sort.Strings(c.Missing)
		sort.Strings(c.Extra)
		out = append(out, c)
	}
	return out
}

func canon(a string) string { return strings.ToLower(strings.TrimSpace(a)) }

// Describe 是一条登记的简短描述，用于变更计划里的人工步骤文案：名称（类型，入口地址）。
func Describe(e Entry) string {
	return fmt.Sprintf("%s（%s，%s）", e.Name, TypeLabel(e.Type), e.Addr)
}

// DescribeAll 描述全部登记（保持登记顺序）。
func DescribeAll(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, Describe(e))
	}
	return out
}
