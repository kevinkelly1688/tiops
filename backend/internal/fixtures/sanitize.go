// @author: kevinkelly
// @license: Apache License 2.0
// @file: sanitize.go
// @time: 2026/10/28

package fixtures

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var (
	reSecret = regexp.MustCompile(`(?i)("?(?:password|passwd|secret|token|access[_-]?key|secret[_-]?key|private[_-]?key|api[_-]?key)"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,}\]]+)`)
	reIPv4   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reStr    = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.)*"`)
	reNum    = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	reInList = regexp.MustCompile(`\((?:\s*\?\s*,)+\s*\?\s*\)`)
)

// MaskSQL 把 SQL 中的字符串与数字字面量替换为 ?，并折叠 IN 列表，
// 与 TiDB 的 digest 文本风格一致。用于防止手机号、身份证、订单号等进入样例数据包。
func MaskSQL(q string) string {
	q = reStr.ReplaceAllString(q, "?")
	q = reNum.ReplaceAllString(q, "?")
	return reInList.ReplaceAllString(q, "(...)")
}

// Sanitizer 对采集到的数据做脱敏。同一个 Sanitizer 内 IP 映射保持一致，
// 因此脱敏后各文件之间的 IP 关系仍然成立。
type Sanitizer struct {
	Opt   Options
	ipMap map[string]string
}

// NewSanitizer 创建脱敏器。
func NewSanitizer(opt Options) *Sanitizer { return &Sanitizer{Opt: opt, ipMap: map[string]string{}} }

func (s *Sanitizer) maskIP(ip string) string {
	if ip == "127.0.0.1" || ip == "0.0.0.0" {
		return ip
	}
	if v, ok := s.ipMap[ip]; ok {
		return v
	}
	n := len(s.ipMap)
	v := fmt.Sprintf("10.250.%d.%d", n/250, n%250+1)
	s.ipMap[ip] = v
	return v
}

// Apply 对一个条目的内容脱敏。sqlCols 非空时，按 TSV 的列名对这些列做 SQL 字面量脱敏。
func (s *Sanitizer) Apply(data []byte, sqlCols []string) []byte {
	out := data
	if s.Opt.MaskSQLLiterals && len(sqlCols) > 0 {
		out = maskTSVColumns(out, sqlCols)
	}
	if s.Opt.RedactSecrets {
		out = reSecret.ReplaceAll(out, []byte(`${1}"***"`))
	}
	if s.Opt.MaskIP {
		out = reIPv4.ReplaceAllFunc(out, func(b []byte) []byte { return []byte(s.maskIP(string(b))) })
	}
	return out
}

// maskTSVColumns 处理 mysql --batch 输出的 TSV：第一行为表头，换行与制表符已被转义。
func maskTSVColumns(data []byte, cols []string) []byte {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) == 0 {
		return data
	}
	header := strings.Split(string(lines[0]), "\t")
	// 列名以 "str:" 开头表示只遮蔽字符串字面量（执行计划文本里的数字是行数估算，不能遮蔽）
	var idx []int
	strOnly := map[int]bool{}
	for i, h := range header {
		for _, c := range cols {
			if strings.EqualFold(h, strings.TrimPrefix(c, "str:")) {
				idx = append(idx, i)
				if strings.HasPrefix(c, "str:") {
					strOnly[i] = true
				}
			}
		}
	}
	if len(idx) == 0 {
		return data
	}
	for li := 1; li < len(lines); li++ {
		if len(lines[li]) == 0 {
			continue
		}
		f := strings.Split(string(lines[li]), "\t")
		for _, i := range idx {
			if i < len(f) {
				if strOnly[i] {
					f[i] = reStr.ReplaceAllString(f[i], "?")
				} else {
					f[i] = MaskSQL(f[i])
				}
			}
		}
		lines[li] = []byte(strings.Join(f, "\t"))
	}
	return bytes.Join(lines, []byte("\n"))
}
