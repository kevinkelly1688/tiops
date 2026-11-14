// @author: kevinkelly
// @license: Apache License 2.0
// @file: semver.go
// @time: 2026/10/28

package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version 是语义化版本（只支持 主.次.修订[-预发布]，不支持 +构建元数据；版本号会用作目录名与文件名，所以限制很严）。
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // 预发布标识，如 rc.1 → ["rc","1"]
	raw                 string
}

var verRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// ParseVersion 解析版本号；允许前缀 v，但 String() 返回的是不带 v 的规范形式。
func ParseVersion(s string) (Version, error) {
	t := strings.TrimPrefix(strings.TrimSpace(s), "v")
	m := verRe.FindStringSubmatch(t)
	if m == nil {
		return Version{}, fmt.Errorf("版本号 %q 不合法（应为 主.次.修订[-预发布]，如 1.4.0 或 1.5.0-rc.1）", s)
	}
	v := Version{raw: t}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
	}
	return v, nil
}

func (v Version) String() string { return v.raw }

// Compare 返回 -1 / 0 / 1。
func (v Version) Compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1 // 正式版大于预发布
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		a, b := v.Pre[i], o.Pre[i]
		an, aerr := strconv.Atoi(a)
		bn, berr := strconv.Atoi(b)
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aerr == nil:
			return -1 // 数字标识小于字母标识
		case berr == nil:
			return 1
		default:
			if a != b {
				if a < b {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(v.Pre) < len(o.Pre):
		return -1
	case len(v.Pre) > len(o.Pre):
		return 1
	}
	return 0
}

// sameVersion 比较两个版本字符串是否相同（容忍 v 前缀）；解析失败时按字面比较。
func sameVersion(a, b string) bool {
	x, e1 := ParseVersion(a)
	y, e2 := ParseVersion(b)
	if e1 != nil || e2 != nil {
		return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
	}
	return x.Compare(y) == 0
}
