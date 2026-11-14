// @author: kevinkelly
// @license: Apache License 2.0
// @file: mask.go
// @time: 2026/10/28

package shell

import (
	"regexp"
	"strings"
)

// 输出遮蔽：口令 / token / 密钥、私钥块、连接串里的口令、手机号、身份证号、银行卡号。
// 同一套规则有两份实现：Go 版（读取输出时再遮蔽一遍，并用于命令文本）与 SedScript（任务脚本里在写盘之前就遮蔽，
// 所以原文不会落盘）。两边的规则要一起改；测试检查它们对同一批样例给出一致的结果。
// 遮蔽是尽力而为：手机号 / 身份证号 / 银行卡号靠位数识别，有漏报也有误报（例如 16–19 位的纳秒时间戳会被当成银行卡号）。

const (
	maskPwd   = "******"
	maskKey   = "[私钥已遮蔽]"
	maskPhone = "1**********"
	maskID    = "******************"
	maskBank  = "****************"
)

var (
	reMaskKV    = regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|access_key|api_key)(\s*[=:]\s*)\S+`)
	reMaskKey   = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	reMaskURL   = regexp.MustCompile(`(://[^:/\s]+:)[^@\s]+@`)
	reMaskPhone = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
	reMaskID    = regexp.MustCompile(`\b\d{17}[\dXx]\b`)
	reMaskBank  = regexp.MustCompile(`\b\d{16,19}\b`)
	reStars     = regexp.MustCompile(`\*{6,}`) // 一段连续的 * 算一处（口令 6 个、手机号 10 个、银行卡 16 个、身份证 18 个）
	reCmdPwd    = regexp.MustCompile(`(--password=|--passwd=|--pwd=|\s-p)\S{3,}`)
)

// MaskText 遮蔽文本里的敏感内容，返回遮蔽后的文本与命中次数。
func MaskText(s string) (string, int) {
	n := 0
	rep := func(re *regexp.Regexp, to string) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			if strings.Contains(to, "$") {
				return re.ReplaceAllString(m, to)
			}
			return to
		})
	}
	rep(reMaskKey, maskKey)
	rep(reMaskKV, "${1}${2}"+maskPwd)
	rep(reMaskURL, "${1}"+maskPwd+"@")
	rep(reMaskPhone, maskPhone)
	rep(reMaskID, maskID)
	rep(reMaskBank, maskBank)
	return s, n
}

// LineMasker 对逐行到达的输出做遮蔽（平台进程内执行时用），结果与对整段文本调用 MaskText 一致：
// 私钥块（BEGIN … END PRIVATE KEY）跨行，所以块内的行先攒着，到 END 行再整体替换；其余规则逐行即可。
type LineMasker struct {
	buf   []string
	inKey bool
}

const maxKeyLines = 200 // 攒私钥块的上限：超过就按普通行放出（与整段遮蔽在「没有 END」时的行为一致）

var (
	reKeyBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	reKeyEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// Line 处理一行（不含换行符）：返回要输出的文本与是否有输出（私钥块攒着的时候没有）。
func (lm *LineMasker) Line(s string) (string, bool) {
	if lm.inKey {
		lm.buf = append(lm.buf, s)
		if reKeyEnd.MatchString(s) || len(lm.buf) >= maxKeyLines {
			return lm.flush(), true
		}
		return "", false
	}
	if reKeyBegin.MatchString(s) && !reKeyEnd.MatchString(s) {
		lm.inKey = true
		lm.buf = append(lm.buf[:0], s)
		return "", false
	}
	out, _ := MaskText(s)
	return out, true
}

// Flush 在输出结束时把没等到 END 的私钥块放出（经常规遮蔽）。
func (lm *LineMasker) Flush() (string, bool) {
	if !lm.inKey {
		return "", false
	}
	return lm.flush(), true
}

func (lm *LineMasker) flush() string {
	text, _ := MaskText(strings.Join(lm.buf, "\n"))
	lm.buf, lm.inKey = lm.buf[:0], false
	return text
}

// CountMasks 统计文本里已有的遮蔽标记个数（输出在写盘前已被 sed 遮蔽，读取时用它估算「已遮蔽 N 处」）。
func CountMasks(s string) int {
	return strings.Count(s, maskKey) + len(reStars.FindAllString(s, -1))
}

// MaskCommand 给命令文本里的口令打码（用于历史、审计与界面显示）：--password=xxx、-pxxx（至少 3 个字符，避免误伤 -p 1）。
func MaskCommand(s string) string {
	s = reCmdPwd.ReplaceAllString(s, "${1}"+maskPwd)
	out, _ := MaskText(s)
	return out
}

// SedScript 返回与 MaskText 等价的 GNU sed -E 脚本（在任务里用 sed -u -E -f 执行）。
func SedScript() string {
	return strings.Join([]string{
		`/-----BEGIN [A-Z ]*PRIVATE KEY-----/,/-----END [A-Z ]*PRIVATE KEY-----/c\`,
		maskKey,
		`s/(password|passwd|pwd|token|secret|access_key|api_key)([[:space:]]*[=:][[:space:]]*)[^[:space:]]+/\1\2` + maskPwd + `/Ig`,
		`s#(://[^:/[:space:]]+:)[^@[:space:]]+@#\1` + maskPwd + `@#g`,
		`s/\b1[3-9][0-9]{9}\b/` + maskPhone + `/g`,
		`s/\b[0-9]{17}[0-9Xx]\b/` + maskID + `/g`,
		`s/\b[0-9]{16,19}\b/` + maskBank + `/g`,
		"",
	}, "\n")
}
