// @author: kevinkelly
// @license: Apache License 2.0
// @file: send.go
// @time: 2026/10/28

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxRespBytes = 64 << 10

// 官方域名白名单（主机必须与渠道类型匹配）。
var defaultHosts = map[string][]string{
	TypeWecom:    {"qyapi.weixin.qq.com"},
	TypeDingtalk: {"oapi.dingtalk.com"},
	TypeLark:     {"open.larksuite.com", "open.feishu.cn"},
	TypeFeishu:   {"open.feishu.cn"},
	TypeSlack:    {"hooks.slack.com"},
	// TypeOther 没有官方域名：主机只能来自管理员配置的额外允许主机。
}

var (
	errBlockedIP = errors.New("目标地址属于回环 / 私网 / 链路本地地址，已拒绝（防 SSRF）")
	errRedirect  = errors.New("收到重定向响应，已拒绝跟随")
)

// permanentError 表示重试也不会成功的错误（配置 / 安全校验失败）。
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) || errors.Is(err, errBlockedIP)
}

// ---------------------------------------------------------------- 主机与 URL 校验 ----

func normHost(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// extraHostMatch 判断 URL 的主机是否在「额外允许主机」里。条目可以是 host 或 host:port。
func extraHostMatch(extra []string, u *url.URL) bool {
	h, hp := normHost(u.Hostname()), normHost(u.Host)
	for _, e := range extra {
		e = normHost(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, ":") && !strings.HasPrefix(e, "[") {
			if e == hp {
				return true
			}
		} else if e == h {
			return true
		}
	}
	return false
}

// hostAllowedPrivate 判断某个主机名（拨号时只有主机名）是否在额外允许主机里，此时放行内网地址。
func (n *Notifier) hostAllowedPrivate(host string) bool {
	host = normHost(host)
	for _, e := range n.opts.ExtraHosts {
		e = normHost(e)
		if e == host {
			return true
		}
		if i := strings.LastIndex(e, ":"); i > 0 && !strings.HasSuffix(e, "]") && e[:i] == host {
			return true
		}
	}
	return false
}

// AllowedHosts 返回当前允许的 Webhook 主机（默认官方域名 + 额外允许主机）。
func (n *Notifier) AllowedHosts() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range []string{TypeWecom, TypeDingtalk, TypeLark, TypeFeishu, TypeSlack} {
		for _, h := range defaultHosts[t] {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	for _, e := range n.opts.ExtraHosts {
		if e = normHost(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// validateURL 校验渠道 URL：仅 https、不带用户信息、主机在白名单且与渠道类型匹配、官方域名下路径形态正确。
func (n *Notifier) validateURL(typ, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, invalid("Webhook 地址不能为空")
	}
	if len(raw) > 1024 {
		return nil, invalid("Webhook 地址过长")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, invalid("Webhook 地址格式不正确")
	}
	if u.User != nil {
		return nil, invalid("Webhook 地址不能包含用户名口令")
	}
	if u.Fragment != "" {
		return nil, invalid("Webhook 地址不能包含 # 片段")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && n.opts.AllowInsecureForTest:
	default:
		return nil, invalid("Webhook 地址必须是 https")
	}
	host := normHost(u.Hostname())
	if extraHostMatch(n.opts.ExtraHosts, u) {
		return u, nil // 私有化网关：形态由管理员负责
	}
	if typ == TypeOther {
		return nil, invalid(fmt.Sprintf("「其他」类型的 Webhook 主机 %s 不在允许列表：需由管理员在启动参数 --notify-allow-hosts 里加入该主机", host))
	}
	official := false
	for _, h := range defaultHosts[typ] {
		if h == host {
			official = true
		}
	}
	if !official {
		for t, hs := range defaultHosts {
			for _, h := range hs {
				if h == host && t != typ {
					return nil, invalid(fmt.Sprintf("主机 %s 属于%s，与所选渠道类型（%s）不匹配", host, typeLabel(t), typeLabel(typ)))
				}
			}
		}
		return nil, invalid(fmt.Sprintf("主机 %s 不在允许列表（官方域名：%s；其他需由管理员在启动参数里加入允许主机）", host, strings.Join(defaultHosts[typ], "、")))
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, invalid("官方域名的 Webhook 不能指定非 443 端口")
	}
	q := u.Query()
	switch typ {
	case TypeWecom:
		if u.Path != "/cgi-bin/webhook/send" || q.Get("key") == "" {
			return nil, invalid("企业微信 Webhook 形如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…")
		}
	case TypeDingtalk:
		if u.Path != "/robot/send" || q.Get("access_token") == "" {
			return nil, invalid("钉钉 Webhook 形如 https://oapi.dingtalk.com/robot/send?access_token=…")
		}
	case TypeLark, TypeFeishu:
		if !strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/") || len(strings.TrimPrefix(u.Path, "/open-apis/bot/v2/hook/")) < 4 {
			if typ == TypeFeishu {
				return nil, invalid("飞书 Webhook 形如 https://open.feishu.cn/open-apis/bot/v2/hook/…")
			}
			return nil, invalid("Lark Webhook 形如 https://open.larksuite.com/open-apis/bot/v2/hook/…")
		}
	case TypeSlack:
		if parts := strings.Split(strings.Trim(u.Path, "/"), "/"); len(parts) != 4 || parts[0] != "services" || parts[1] == "" || parts[2] == "" || len(parts[3]) < 4 {
			return nil, invalid("Slack Webhook 形如 https://hooks.slack.com/services/T…/B…/…")
		}
	}
	return u, nil
}

func typeLabel(t string) string {
	for _, x := range Types() {
		if x.Type == t {
			return x.Label
		}
	}
	return t
}

// token 取出 URL 中的机器人令牌（用于脱敏与错误信息打码）。
func token(typ string, u *url.URL) string {
	switch typ {
	case TypeWecom:
		return u.Query().Get("key")
	case TypeDingtalk:
		return u.Query().Get("access_token")
	case TypeLark, TypeFeishu, TypeSlack:
		if i := strings.LastIndex(u.Path, "/"); i >= 0 {
			return u.Path[i+1:]
		}
	}
	return ""
}

// maskURL 返回脱敏后的 URL，只保留令牌末 4 位：https://host/…key=****abcd。
func maskURL(typ, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "****"
	}
	tk := token(typ, u)
	if tk == "" { // 私有化网关等未知形态：取整个 URL 的末尾
		tk = raw
	}
	last := "****"
	if len(tk) >= 8 {
		last = "****" + tk[len(tk)-4:]
	}
	label := ""
	switch typ {
	case TypeWecom:
		label = "key="
	case TypeDingtalk:
		label = "access_token="
	}
	return u.Scheme + "://" + u.Host + "/…" + label + last
}

var reQuerySecret = regexp.MustCompile(`(?i)\b(key|access_token|sign|token|secret)=[^&\s"']+`)

// redact 把错误文本里的 URL、令牌、secret 全部打码，并截断。
func redact(msg string, c *channel) string {
	if c != nil {
		if c.URL != "" {
			msg = strings.ReplaceAll(msg, c.URL, "****")
			if u, err := url.Parse(c.URL); err == nil {
				if tk := token(c.Type, u); len(tk) >= 6 {
					msg = strings.ReplaceAll(msg, tk, "****")
				}
			}
		}
		if len(c.Secret) >= 4 {
			msg = strings.ReplaceAll(msg, c.Secret, "****")
		}
	}
	msg = reQuerySecret.ReplaceAllString(msg, "$1=****")
	msg = strings.Join(strings.Fields(msg), " ")
	if utf8.RuneCountInString(msg) > 200 {
		msg = string([]rune(msg)[:200]) + "…"
	}
	return msg
}

// ---------------------------------------------------------------- 签名 ----

// dingSign 钉钉加签：urlencode(base64(HMAC-SHA256(key=secret, msg=timestamp+"\n"+secret)))，timestamp 为毫秒。
func dingSign(secret string, tsMillis int64) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(fmt.Sprintf("%d\n%s", tsMillis, secret)))
	return url.QueryEscape(base64.StdEncoding.EncodeToString(m.Sum(nil)))
}

// larkSign Lark 签名校验：base64(HMAC-SHA256(key=timestamp+"\n"+secret, msg 为空))，timestamp 为秒。
func larkSign(secret string, tsSeconds int64) string {
	m := hmac.New(sha256.New, []byte(fmt.Sprintf("%d\n%s", tsSeconds, secret)))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// otherSign 通用 Webhook 签名：hex(HMAC-SHA256(key=secret, msg=timestamp+"."+body))，timestamp 为秒。
// 请求头 X-TiDBOps-Timestamp 与 X-TiDBOps-Signature: sha256=<hex>；接收方应校验时间戳与当前时间相差不大（如 5 分钟内）。
func otherSign(secret string, tsSeconds int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.", tsSeconds)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ---------------------------------------------------------------- 请求构造与响应判定 ----

// build 构造目标 URL 与请求体。
func (n *Notifier) build(c *channel, m Message) (string, []byte, error) {
	target := c.URL
	var body any
	switch c.Type {
	case TypeWecom:
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": renderWecom(m)}}
	case TypeDingtalk:
		title, text := renderDingtalk(m)
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": title, "text": text}}
		if c.Secret != "" {
			ts := n.now().UnixMilli()
			sep := "&"
			if !strings.Contains(target, "?") {
				sep = "?"
			}
			target += fmt.Sprintf("%stimestamp=%d&sign=%s", sep, ts, dingSign(c.Secret, ts))
		}
	case TypeSlack:
		body = map[string]string{"text": renderSlack(m)}
	case TypeOther:
		body = renderOther(m, n.now())
	case TypeLark, TypeFeishu:
		b := map[string]any{"msg_type": "text", "content": map[string]string{"text": renderLark(m)}}
		if c.Secret != "" {
			ts := n.now().Unix()
			b["timestamp"] = fmt.Sprint(ts)
			b["sign"] = larkSign(c.Secret, ts)
		}
		body = b
	default:
		return "", nil, permanentError{fmt.Errorf("未知渠道类型 %q", c.Type)}
	}
	raw, err := json.Marshal(body)
	return target, raw, err
}

// checkResp 按渠道约定判定成功：企业微信 / 钉钉 HTTP 200 且 errcode==0；Lark HTTP 200 且 code==0（或 StatusCode==0）。
func checkResp(typ string, status int, body []byte) error {
	if status >= 300 && status < 400 {
		return permanentError{fmt.Errorf("HTTP %d：%w", status, errRedirect)}
	}
	if typ == TypeOther { // 通用 Webhook：任何 2xx 都算成功，不解析响应体
		if status < 200 || status > 299 {
			return fmt.Errorf("HTTP %d", status)
		}
		return nil
	}
	if typ == TypeSlack { // Slack：HTTP 200 且响应体为 ok；失败时响应体是错误码文本（如 invalid_payload、no_service）
		txt := strings.TrimSpace(string(body))
		if status == http.StatusOK && txt == "ok" {
			return nil
		}
		if utf8.RuneCountInString(txt) > 80 {
			txt = string([]rune(txt)[:80]) + "…"
		}
		if status != http.StatusOK {
			return fmt.Errorf("Slack 返回 HTTP %d：%s", status, txt)
		}
		return fmt.Errorf("Slack 返回非预期内容：%s", txt)
	}
	if status != http.StatusOK {
		return fmt.Errorf("HTTP %d", status)
	}
	var r struct {
		Errcode       *int   `json:"errcode"`
		Errmsg        string `json:"errmsg"`
		Code          *int   `json:"code"`
		Msg           string `json:"msg"`
		StatusCode    *int   `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return errors.New("响应不是合法 JSON")
	}
	if typ == TypeLark || typ == TypeFeishu {
		if r.Code == nil && r.StatusCode == nil {
			return errors.New("响应缺少 code / StatusCode")
		}
		if (r.Code == nil || *r.Code == 0) && (r.StatusCode == nil || *r.StatusCode == 0) {
			return nil
		}
		code, msg := 0, r.Msg
		if r.Code != nil && *r.Code != 0 {
			code = *r.Code
		} else if r.StatusCode != nil {
			code, msg = *r.StatusCode, r.StatusMessage
		}
		return fmt.Errorf("%s 返回 code=%d：%s", map[bool]string{true: "飞书", false: "Lark"}[typ == TypeFeishu], code, msg)
	}
	if r.Errcode == nil {
		return errors.New("响应缺少 errcode")
	}
	if *r.Errcode != 0 {
		return fmt.Errorf("errcode=%d：%s", *r.Errcode, r.Errmsg)
	}
	return nil
}

// deliver 向渠道发送一次（不重试），返回已打码的错误。
func (n *Notifier) deliver(ctx context.Context, c *channel, m Message) error {
	if _, err := n.validateURL(c.Type, c.URL); err != nil {
		return permanentError{errors.New(redact(err.Error(), c))}
	}
	target, body, err := n.build(c, m)
	if err != nil {
		return errors.New(redact(err.Error(), c))
	}
	ctx, cancel := context.WithTimeout(ctx, n.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return permanentError{errors.New("构造请求失败")}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "tidbops-notify/1")
	if c.Type == TypeOther && c.Secret != "" {
		ts := n.now().Unix()
		req.Header.Set("X-TiDBOps-Timestamp", fmt.Sprint(ts))
		req.Header.Set("X-TiDBOps-Signature", otherSign(c.Secret, ts, body))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = fmt.Errorf("%s：%w", ue.Op, ue.Err) // 去掉带 URL 的外层
		}
		if errors.Is(err, errBlockedIP) {
			return permanentError{errors.New(redact(err.Error(), c))}
		}
		return errors.New(redact(err.Error(), c))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err := checkResp(c.Type, resp.StatusCode, raw); err != nil {
		perm := isPermanent(err)
		e := errors.New(redact(err.Error(), c))
		if perm {
			return permanentError{e}
		}
		return e
	}
	return nil
}

// ---------------------------------------------------------------- 拨号防护 ----

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// isBlockedIP 判断是否为回环 / 私网 / 链路本地 / 未指定 / 组播等不应作为 Webhook 目标的地址。
func isBlockedIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// dial 先自行解析主机，再检查每个解析结果（防 DNS 重绑定 / 内网域名），最后直接拨号到通过检查的 IP。
// 在「额外允许主机」里的主机放行内网地址。
func (n *Notifier) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ips, err = n.lookup(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	allow := n.hostAllowedPrivate(host)
	if !allow {
		for _, ip := range ips {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("%s 解析到 %s：%w", host, ip, errBlockedIP)
			}
		}
	}
	d := net.Dialer{Timeout: n.opts.Timeout}
	var last error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("没有可用的解析结果")
	}
	return nil, last
}

func defaultLookup(ctx context.Context, host string) ([]net.IP, error) {
	as, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(as))
	for _, a := range as {
		out = append(out, a.IP)
	}
	return out, nil
}
