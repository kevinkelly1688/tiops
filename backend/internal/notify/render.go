// @author: kevinkelly
// @license: Apache License 2.0
// @file: render.go
// @time: 2026/10/28

package notify

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	wecomMaxBytes = 4096  // 企业微信 markdown 内容上限
	dingMaxBytes  = 18000 // 钉钉 markdown 文本（官方上限约 20000 字节）
	larkMaxBytes  = 18000 // Lark 文本（官方上限约 30KB）
	slackMaxBytes = 18000 // Slack 文本（官方上限 40000 字符，超过 4000 会被折叠显示）
	maxLineRunes  = 300
	maxTitleRunes = 100
	prefix        = "[TiDB 运维]"
)

var reSecretKV = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|access_token|api[_-]?key|authorization|sign)\b\s*[=:：]\s*[^\s,;&，；]+`)

// cleanText 把多行文本压成单行、掩码形如 password=xxx 的片段并截断。兜底措施，调用方本就不应传入敏感内容。
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = reSecretKV.ReplaceAllString(s, "$1=****")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		rs := []rune(s)
		s = string(rs[:max]) + "…"
	}
	return s
}

// sanitizeMessage 清洗消息各字段。
func sanitizeMessage(m Message) Message {
	m.Env = cleanText(m.Env, 20)
	m.Cluster = cleanText(m.Cluster, 80)
	m.Title = cleanText(m.Title, maxTitleRunes)
	var ls []string
	for _, l := range m.Lines {
		if l = cleanText(l, maxLineRunes); l != "" {
			ls = append(ls, l)
		}
	}
	m.Lines = ls
	m.Link = safeLink(m.Link)
	switch m.Level {
	case LvInfo, LvSuccess, LvWarning, LvCritical:
	default:
		m.Level = LvInfo
	}
	return m
}

// safeLink 只接受 http(s) 链接，且去掉会破坏 markdown 语法的字符。
func safeLink(l string) string {
	l = strings.TrimSpace(l)
	if l == "" {
		return ""
	}
	u, err := url.Parse(l)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	if strings.ContainsAny(l, " \t\r\n<>\"'`") {
		return ""
	}
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(l)
}

func levelTag(lv string) string {
	switch lv {
	case LvCritical:
		return "严重"
	case LvWarning:
		return "警告"
	case LvSuccess:
		return "成功"
	}
	return "通知"
}

func wecomColor(lv string) string {
	switch lv {
	case LvCritical, LvWarning:
		return "warning"
	case LvSuccess:
		return "info"
	}
	return "comment"
}

// mdEscape 防止用户输入（变更标题、原因等）注入 markdown / HTML 标签或链接。
func mdEscape(s string) string {
	return strings.NewReplacer("<", "＜", ">", "＞", "](", "] (").Replace(s)
}

func headLine(m Message) string {
	var parts []string
	if m.Env != "" {
		parts = append(parts, "环境："+m.Env)
	}
	if m.Cluster != "" {
		parts = append(parts, "集群："+m.Cluster)
	}
	return strings.Join(parts, "　")
}

func timeLine(m Message) string {
	if m.Time.IsZero() {
		return ""
	}
	return "时间：" + m.Time.Format("2006-01-02 15:04:05")
}

// truncateBytes 按字节截断（不切断 UTF-8 字符），超限时追加省略号。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	cut := max - len(ell)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}

// renderWecom 渲染企业微信 markdown（仅用其支持的子集：加粗、<font color>、引用、链接）。
// 内容超过 4096 字节时按行丢弃尾部明细，保证标签完整，链接始终保留。
func renderWecom(m Message) string {
	head := fmt.Sprintf("<font color=\"%s\">**%s %s**</font>", wecomColor(m.Level), prefix+"【"+levelTag(m.Level)+"】", mdEscape(m.Title))
	var tail []string
	if t := timeLine(m); t != "" {
		tail = append(tail, "> <font color=\"comment\">"+t+"</font>")
	}
	if m.Link != "" {
		tail = append(tail, "[查看详情]("+m.Link+")")
	}
	var body []string
	if h := headLine(m); h != "" {
		body = append(body, "> "+mdEscape(h))
	}
	for _, l := range m.Lines {
		body = append(body, "> "+mdEscape(l))
	}
	build := func(n int) string {
		parts := append([]string{head}, body[:n]...)
		if n < len(body) {
			parts = append(parts, "> …（已省略 "+fmt.Sprint(len(body)-n)+" 行）")
		}
		return strings.Join(append(parts, tail...), "\n")
	}
	n := len(body)
	out := build(n)
	for len(out) > wecomMaxBytes && n > 0 {
		n--
		out = build(n)
	}
	return truncateBytes(out, wecomMaxBytes)
}

// renderDingtalk 渲染钉钉 markdown，返回 (title, text)。
func renderDingtalk(m Message) (string, string) {
	title := prefix + " " + levelTag(m.Level) + "：" + m.Title
	var b strings.Builder
	b.WriteString("### " + mdEscape(m.Title) + "\n\n")
	if h := headLine(m); h != "" {
		b.WriteString("- " + mdEscape(h) + "\n")
	}
	for _, l := range m.Lines {
		b.WriteString("- " + mdEscape(l) + "\n")
	}
	if t := timeLine(m); t != "" {
		b.WriteString("- " + t + "\n")
	}
	if m.Link != "" {
		b.WriteString("\n[查看详情](" + m.Link + ")\n")
	}
	return truncateBytes(title, 200), truncateBytes(b.String(), dingMaxBytes)
}

// renderLark 渲染 Lark 纯文本。
func renderLark(m Message) string {
	var b strings.Builder
	b.WriteString(prefix + "【" + levelTag(m.Level) + "】" + m.Title + "\n")
	if h := headLine(m); h != "" {
		b.WriteString(h + "\n")
	}
	for _, l := range m.Lines {
		b.WriteString(l + "\n")
	}
	if t := timeLine(m); t != "" {
		b.WriteString(t + "\n")
	}
	if m.Link != "" {
		b.WriteString("详情：" + m.Link + "\n")
	}
	return truncateBytes(strings.TrimRight(b.String(), "\n"), larkMaxBytes)
}

// slackEscape 按 Slack 的要求转义 & < >（它们在 mrkdwn 里有特殊含义），并把 * _ ~ ` 全角化，避免用户输入改变格式。
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "*", "＊", "_", "＿", "~", "～", "`", "｀").Replace(s)
}

// renderSlack 渲染 Slack mrkdwn 文本：加粗标题、列表、链接 <url|文字>。
func renderSlack(m Message) string {
	var b strings.Builder
	b.WriteString("*" + slackEscape(prefix+"【"+levelTag(m.Level)+"】"+m.Title) + "*\n")
	if h := headLine(m); h != "" {
		b.WriteString(slackEscape(h) + "\n")
	}
	for _, l := range m.Lines {
		b.WriteString("• " + slackEscape(l) + "\n")
	}
	if t := timeLine(m); t != "" {
		b.WriteString(t + "\n")
	}
	if m.Link != "" {
		b.WriteString("<" + strings.ReplaceAll(m.Link, "|", "%7C") + "|查看详情>\n")
	}
	return truncateBytes(strings.TrimRight(b.String(), "\n"), slackMaxBytes)
}

// otherPayload 是「其他」渠道（通用 Webhook）的 JSON 请求体，字段稳定，接收方可直接解析。
type otherPayload struct {
	Source  string   `json:"source"`  // 固定 tidb-ops
	Version int      `json:"version"` // 载荷版本，目前 1
	Event   string   `json:"event"`   // 事件键，如 change.failed、test
	Level   string   `json:"level"`   // info | success | warning | critical
	Title   string   `json:"title"`
	Env     string   `json:"env"`
	Cluster string   `json:"cluster"`
	Lines   []string `json:"lines"`
	Link    string   `json:"link,omitempty"`
	Time    string   `json:"time"` // RFC 3339
	Text    string   `json:"text"` // 与 Lark 相同的纯文本渲染，便于直接转发到聊天工具
}

// renderOther 渲染通用 Webhook 的 JSON 载荷。
func renderOther(m Message, now time.Time) otherPayload {
	t := m.Time
	if t.IsZero() {
		t = now
	}
	lines := m.Lines
	if lines == nil {
		lines = []string{}
	}
	return otherPayload{Source: "tidb-ops", Version: 1, Event: m.Event, Level: m.Level, Title: m.Title, Env: m.Env, Cluster: m.Cluster,
		Lines: lines, Link: m.Link, Time: t.Format(time.RFC3339), Text: renderLark(m)}
}
