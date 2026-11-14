// @author: kevinkelly
// @license: Apache License 2.0
// @file: configvals.go
// @time: 2026/10/28

package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 本文件把「配置当前值」的三个来源统一起来，供配置变更页（/api/config-values）与参数建议页（ParamRows）共用：
//
//	TiKV / TiDB / PD / TiFlash 的实例级配置 ← sql/cluster-config（information_schema.cluster_config）
//	TiDB 系统变量                             ← sql/variables（information_schema.session_variables）
//	PD 集群级参数                             ← pd/config
//
// 取不到的项一律省略，不猜测默认值。

// ConfigSpec 描述目录里的一个配置项。它是 exec.ConfigItem 的子集：collect 不依赖 exec 包，由调用方转换。
type ConfigSpec struct {
	Role  string   // tidb | tikv | pd | tiflash | ticdc
	Key   string   // 配置键或系统变量名
	Type  string   // int | size | duration | bool | enum
	Opts  []string // enum 可选值
	Sys   bool     // TiDB 系统变量
	Scope string   // instance | cluster
}

// ConfigValue 是一个配置项的当前值。
type ConfigValue struct {
	// Cluster 是集群级取值（系统变量、PD 集群级参数）；实例级配置项为空串。
	Cluster string `json:"cluster"`
	// Instances 是实例级配置项在各实例上的取值（地址 → 原始值）。
	Instances map[string]string `json:"instances,omitempty"`
	// Drift 表示各实例取值不一致（按归一化值比较，16GiB 与 16GB 视为一致）。
	Drift bool `json:"drift"`
	// Majority 是多数派取值（原始值）；取值个数相同时取字典序最小者。
	Majority string `json:"majority,omitempty"`
	// Norm 是 Cluster（或 Majority）归一化后的形式，与 exec.Normalize 的输出一致（16GiB → 16GB，30m0s → 30m）。
	// 无法归一化（如 TiKV 自动计算出的带小数容量）时为空。
	Norm string `json:"norm,omitempty"`
	// InstNorm 是各实例取值的归一化形式，仅当至少一个实例的归一化值与原始值不同时给出。
	InstNorm map[string]string `json:"instNorm,omitempty"`
}

// ConfigValues 是 /api/config-values 的响应体：角色 → 配置键 → 当前值。
type ConfigValues struct {
	Values map[string]map[string]ConfigValue `json:"values"`
}

// configSources 是一次采集得到的三类原始配置。
type configSources struct {
	cc   map[string]map[string]map[string]string // 角色 → 键 → 实例地址 → 值
	vars map[string]string                       // 系统变量名（小写）→ 值
	pd   map[string]string                       // PD 配置键（section.name）→ 值
}

// ccRole 把 cluster_config 的 TYPE 列映射为目录里的角色；不关心的类型（如 tiflash-learner）返回空串。
func ccRole(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "tidb", "pd", "tikv", "tiflash":
		return strings.ToLower(strings.TrimSpace(t))
	case "ticdc", "cdc":
		return "ticdc"
	}
	return ""
}

func (c *Collector) loadConfigSources(ctx context.Context) configSources {
	src := configSources{cc: map[string]map[string]map[string]string{}, vars: map[string]string{}, pd: c.pdConfigFlat(ctx)}
	if b, err := c.get(ctx, "sql/variables"); err == nil {
		_, rows := parseTSV(b)
		for _, r := range rows {
			if n := strings.ToLower(r["variable_name"]); n != "" {
				src.vars[n] = r["variable_value"]
			}
		}
	}
	if b, err := c.get(ctx, "sql/cluster-config"); err == nil {
		_, rows := parseTSV(b)
		for _, r := range rows {
			role, key, inst := ccRole(r["type"]), r["key"], r["instance"]
			if role == "" || key == "" || inst == "" {
				continue
			}
			if src.cc[role] == nil {
				src.cc[role] = map[string]map[string]string{}
			}
			if src.cc[role][key] == nil {
				src.cc[role][key] = map[string]string{}
			}
			src.cc[role][key][inst] = r["value"]
		}
	}
	return src
}

// pdConfigFlat 读取 pd/config 并展开成 section.name → 值。数值保持原样（不经过 float64），数组用逗号连接。
func (c *Collector) pdConfigFlat(ctx context.Context) map[string]string {
	b, err := c.get(ctx, "pd/config")
	if err != nil {
		return nil
	}
	return flattenPDConfig(b)
}

func flattenPDConfig(b []byte) map[string]string {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw map[string]any
	if dec.Decode(&raw) != nil {
		return nil
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				if prefix == "" {
					walk(k, sub)
				} else {
					walk(prefix+"."+k, sub)
				}
			}
		case []any:
			parts := make([]string, 0, len(x))
			for _, e := range x {
				parts = append(parts, fmt.Sprint(e))
			}
			out[prefix] = strings.Join(parts, ",")
		case nil:
			out[prefix] = ""
		case json.Number:
			out[prefix] = x.String()
		default:
			out[prefix] = strings.TrimSpace(fmt.Sprint(x))
		}
	}
	walk("", raw)
	return out
}

// ConfigValues 返回目录里每个配置项的当前真实值。取不到的项不出现在结果里。
func (c *Collector) ConfigValues(ctx context.Context, specs []ConfigSpec) ConfigValues {
	return buildConfigValues(c.loadConfigSources(ctx), specs)
}

func buildConfigValues(src configSources, specs []ConfigSpec) ConfigValues {
	out := ConfigValues{Values: map[string]map[string]ConfigValue{}}
	for _, role := range []string{"tidb", "pd", "tikv", "tiflash", "ticdc"} {
		out.Values[role] = map[string]ConfigValue{}
	}
	for _, sp := range specs {
		if out.Values[sp.Role] == nil {
			out.Values[sp.Role] = map[string]ConfigValue{}
		}
		switch {
		case sp.Sys:
			if v, ok := src.vars[strings.ToLower(sp.Key)]; ok {
				out.Values[sp.Role][sp.Key] = ConfigValue{Cluster: v, Norm: NormalizeConfig(sp, v)}
			}
		case sp.Role == "pd" && sp.Scope == "cluster":
			if v, ok := src.pd[sp.Key]; ok {
				out.Values[sp.Role][sp.Key] = ConfigValue{Cluster: v, Norm: NormalizeConfig(sp, v)}
			}
		default:
			if insts := src.cc[sp.Role][sp.Key]; len(insts) > 0 {
				out.Values[sp.Role][sp.Key] = instanceValue(sp, insts)
			}
		}
	}
	return out
}

// instanceValue 汇总一个实例级配置项：漂移判定按归一化值，多数派取个数最多者。
func instanceValue(sp ConfigSpec, insts map[string]string) ConfigValue {
	cv := ConfigValue{Instances: map[string]string{}}
	norms := map[string]string{}
	count := map[string]int{}    // 比较键（归一化值，无则原始值）→ 实例数
	first := map[string]string{} // 比较键 → 原始值（字典序最小的实例取值，保证确定性）
	addrs := make([]string, 0, len(insts))
	for a := range insts {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	diffNorm := false
	for _, a := range addrs {
		raw := insts[a]
		cv.Instances[a] = raw
		n := NormalizeConfig(sp, raw)
		norms[a] = n
		if n != "" && n != raw {
			diffNorm = true
		}
		key := n
		if key == "" {
			key = raw
		}
		count[key]++
		if _, ok := first[key]; !ok {
			first[key] = raw
		}
	}
	best, bestN := "", -1
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if count[k] > bestN {
			best, bestN = k, count[k]
		}
	}
	cv.Drift = len(count) > 1
	cv.Majority = first[best]
	cv.Norm = NormalizeConfig(sp, cv.Majority)
	if diffNorm {
		cv.InstNorm = map[string]string{}
		for a, n := range norms {
			if n != "" {
				cv.InstNorm[a] = n
			}
		}
	}
	return cv
}

// ---------------------------------------------------------------- 归一化 ----

var (
	reIntVal  = regexp.MustCompile(`^[+-]?[0-9]+$`)
	reSizeVal = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(B|KB|MB|GB|TB|PB|KiB|MiB|GiB|TiB|PiB)?$`)
)

var sizeMul = map[string]float64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40, "PB": 1 << 50}

// NormalizeConfig 把集群里读到的值归一化成与 exec.Normalize 输出一致的形式，便于与用户输入的新值比较。
// 与 exec.Normalize 的区别：只做格式归一，不做范围与注入校验（当前值是什么就是什么）；无法归一化时返回空串。
//
//	size     16GiB / 17179869184 → 16GB（取能整除的最大单位 TB/GB/MB/KB/B；0 → 0GB）
//	duration 30m0s → 30m，1h0m0s → 1h，90m0s → 90m；不是整秒的不归一化
//	int      去掉前导 0 / 正号
//	bool     on/true/1 → ON，off/false/0 → OFF
//	enum     按目录里的规范写法（大小写不敏感）
func NormalizeConfig(sp ConfigSpec, raw string) string {
	v := strings.Trim(strings.TrimSpace(raw), `"`)
	if v == "" {
		return ""
	}
	switch sp.Type {
	case "int":
		if !reIntVal.MatchString(v) {
			return ""
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return ""
		}
		return strconv.FormatInt(n, 10)
	case "size":
		m := reSizeVal.FindStringSubmatch(v)
		if m == nil {
			return ""
		}
		unit := strings.ToUpper(m[2])
		unit = strings.Replace(unit, "IB", "B", 1)
		if unit == "" {
			unit = "B"
		}
		nb := math.Round(f64(m[1]) * sizeMul[unit])
		return formatSizeNorm(nb)
	case "duration":
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d%time.Second != 0 {
			return ""
		}
		secs := int64(d / time.Second)
		switch {
		case secs != 0 && secs%3600 == 0:
			return strconv.FormatInt(secs/3600, 10) + "h"
		case secs != 0 && secs%60 == 0:
			return strconv.FormatInt(secs/60, 10) + "m"
		}
		return strconv.FormatInt(secs, 10) + "s"
	case "bool":
		switch strings.ToLower(v) {
		case "on", "true", "1", "yes":
			return "ON"
		case "off", "false", "0", "no":
			return "OFF"
		}
		return ""
	case "enum":
		for _, o := range sp.Opts {
			if strings.EqualFold(v, o) {
				return o
			}
		}
		return ""
	}
	return ""
}

func formatSizeNorm(nb float64) string {
	if nb <= 0 {
		return "0GB"
	}
	if nb >= 1<<62 {
		return ""
	}
	n := int64(nb)
	for _, u := range []struct {
		name string
		mul  int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n%u.mul == 0 {
			return strconv.FormatInt(n/u.mul, 10) + u.name
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}

// ---------------------------------------------------------------- 供 ParamRows 复用 ----

// values 返回某个作用域下某个键的全部不同取值（已排序）。scope 为 tidb 时优先系统变量，再看 TiDB 配置文件项。
func (s configSources) values(scope, name string) []string {
	set := map[string]bool{}
	switch scope {
	case "tidb":
		if v, ok := s.vars[strings.ToLower(name)]; ok {
			set[v] = true
		}
	case "pd":
		if v, ok := s.pd[name]; ok {
			set[v] = true
		}
	}
	for _, v := range s.cc[scope][name] {
		set[v] = true
	}
	list := make([]string, 0, len(set))
	for v := range set {
		list = append(list, v)
	}
	sort.Strings(list)
	return list
}
