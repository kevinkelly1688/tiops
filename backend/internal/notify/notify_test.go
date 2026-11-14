// @author: kevinkelly
// @license: Apache License 2.0
// @file: notify_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const okBody = `{"errcode":0,"errmsg":"ok","code":0,"msg":"success","StatusCode":0}`

type hit struct {
	path, query string
	body        []byte
	at          time.Time
}

type fake struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []hit
}

// newFake 启动假 Webhook 服务；h 收到第 n 次（从 1 开始）请求，返回 nil 处理器则回成功。
func newFake(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 0, 1024)
		buf := make([]byte, 4096)
		for {
			k, err := r.Body.Read(buf)
			b = append(b, buf[:k]...)
			if err != nil {
				break
			}
		}
		f.mu.Lock()
		f.hits = append(f.hits, hit{path: r.URL.Path, query: r.URL.RawQuery, body: b, at: time.Now()})
		n := len(f.hits)
		f.mu.Unlock()
		if h != nil {
			h(n, w, r)
			return
		}
		w.Write([]byte(okBody))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *fake) get(i int) hit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[i]
}

func testNotifier(t *testing.T, f *fake, mod func(*Options)) *Notifier {
	t.Helper()
	o := Options{AllowInsecureForTest: true, ExtraHosts: []string{"127.0.0.1"}, Backoff: []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}, Env: "prod", Cluster: "c1"}
	if f != nil {
		o.HTTPClient = f.srv.Client()
	}
	if mod != nil {
		mod(&o)
	}
	n := New(o)
	t.Cleanup(n.Close)
	return n
}

const (
	wecomKey = "KEY1234567890abcd"
	dingTok  = "TOKabcdefghijklmnwxyz"
	larkTok  = "LARKhookid00000ef12"
)

func addCh(t *testing.T, n *Notifier, f *fake, typ, secret string, events ...string) ChannelView {
	t.Helper()
	u := f.srv.URL
	switch typ {
	case TypeWecom:
		u += "/cgi-bin/webhook/send?key=" + wecomKey
	case TypeDingtalk:
		u += "/robot/send?access_token=" + dingTok
	case TypeLark:
		u += "/open-apis/bot/v2/hook/" + larkTok
	}
	v, err := n.Upsert(ChannelInput{Type: typ, Name: "ch-" + typ, URL: u, Secret: secret, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func fixedNow(t time.Time) (func() time.Time, func(d time.Duration)) {
	var mu sync.Mutex
	cur := t
	return func() time.Time { mu.Lock(); defer mu.Unlock(); return cur },
		func(d time.Duration) { mu.Lock(); cur = cur.Add(d); mu.Unlock() }
}

func sample() Message {
	return Message{Env: "prod", Cluster: "prod-tidb-01", Event: EvFailed, Title: "变更执行失败", Level: LvCritical,
		Lines: []string{"变更：重启 tikv-2（CHG-20260101-001）", "风险：中", "申请人：dba", "原因：等待 store 恢复超时"},
		Link:  "https://ops.example.com/#schedule",
		Time:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Key:   "CHG-1",
	}
}

// ---------------------------------------------------------------- 签名 ----

func TestSignatureValues(t *testing.T) {
	// 期望值由 Python 的 hmac / hashlib 另行算出
	if got := dingSign("SECabc123", 1700000000123); got != "etIBQNvdgq9e1O4P4gRnRS05m9vXVgr6%2F5xt3Azj2s8%3D" {
		t.Errorf("钉钉签名 = %s", got)
	}
	if got := larkSign("SECabc123", 1700000000); got != "UqhI0v4zAkSwI4hNYBuHQvnrAqshA0UaeBGHCUMPX70=" {
		t.Errorf("Lark 签名 = %s", got)
	}
}

func TestDingtalkRequestWithSign(t *testing.T) {
	f := newFake(t, nil)
	now, _ := fixedNow(time.UnixMilli(1700000000123))
	n := testNotifier(t, f, func(o *Options) { o.Now = now })
	v := addCh(t, n, f, TypeDingtalk, "SECabc123")
	if err := n.Test(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	h := f.get(0)
	if h.path != "/robot/send" {
		t.Errorf("路径 = %s", h.path)
	}
	q, _ := url.ParseQuery(h.query)
	if q.Get("access_token") != dingTok || q.Get("timestamp") != "1700000000123" || q.Get("sign") != "etIBQNvdgq9e1O4P4gRnRS05m9vXVgr6/5xt3Azj2s8=" {
		t.Errorf("查询串 = %s", h.query)
	}
	if !strings.Contains(h.query, "sign=etIBQNvdgq9e1O4P4gRnRS05m9vXVgr6%2F5xt3Azj2s8%3D") {
		t.Errorf("sign 必须 URL 编码: %s", h.query)
	}
	var body struct {
		Msgtype  string
		Markdown struct{ Title, Text string }
	}
	_ = json.Unmarshal(h.body, &body)
	if body.Msgtype != "markdown" || !strings.Contains(body.Markdown.Title, "通知渠道测试") || !strings.Contains(body.Markdown.Text, "环境：prod") {
		t.Errorf("钉钉请求体 = %s", h.body)
	}
	// 没有 secret 时不加签
	v2, _ := n.Upsert(ChannelInput{ID: v.ID, Name: "x", ClearSecret: true})
	if v2.HasSecret {
		t.Error("ClearSecret 应清除 secret")
	}
	_ = n.Test(context.Background(), v.ID)
	if q, _ := url.ParseQuery(f.get(1).query); q.Get("sign") != "" || q.Get("timestamp") != "" {
		t.Errorf("无 secret 不应加签: %s", f.get(1).query)
	}
}

func TestLarkRequestWithSign(t *testing.T) {
	f := newFake(t, nil)
	now, _ := fixedNow(time.Unix(1700000000, 0))
	n := testNotifier(t, f, func(o *Options) { o.Now = now })
	v := addCh(t, n, f, TypeLark, "SECabc123")
	if err := n.Test(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	h := f.get(0)
	if h.path != "/open-apis/bot/v2/hook/"+larkTok {
		t.Errorf("路径 = %s", h.path)
	}
	var body struct {
		MsgType   string `json:"msg_type"`
		Timestamp string
		Sign      string
		Content   struct{ Text string }
	}
	_ = json.Unmarshal(h.body, &body)
	if body.MsgType != "text" || body.Timestamp != "1700000000" || body.Sign != "UqhI0v4zAkSwI4hNYBuHQvnrAqshA0UaeBGHCUMPX70=" || !strings.Contains(body.Content.Text, "通知渠道测试") {
		t.Errorf("Lark 请求体 = %s", h.body)
	}
}

func TestWecomRequest(t *testing.T) {
	f := newFake(t, nil)
	n := testNotifier(t, f, nil)
	addCh(t, n, f, TypeWecom, "", EvFailed)
	n.Notify(sample())
	if !n.Drain(3 * time.Second) {
		t.Fatal("未发完")
	}
	h := f.get(0)
	if h.path != "/cgi-bin/webhook/send" || h.query != "key="+wecomKey {
		t.Errorf("URL = %s?%s", h.path, h.query)
	}
	var body struct {
		Msgtype  string
		Markdown struct{ Content string }
	}
	_ = json.Unmarshal(h.body, &body)
	c := body.Markdown.Content
	if body.Msgtype != "markdown" {
		t.Errorf("msgtype = %s", body.Msgtype)
	}
	for _, want := range []string{`<font color="warning">`, "变更执行失败", "环境：prod", "集群：prod-tidb-01", "CHG-20260101-001", "申请人：dba", "时间：2026-01-02 03:04:05", "[查看详情](https://ops.example.com/#schedule)"} {
		if !strings.Contains(c, want) {
			t.Errorf("企业微信内容缺少 %q:\n%s", want, c)
		}
	}
}

// ---------------------------------------------------------------- 成功判定 ----

func TestCheckResp(t *testing.T) {
	cases := []struct {
		typ    string
		status int
		body   string
		ok     bool
	}{
		{TypeWecom, 200, `{"errcode":0,"errmsg":"ok"}`, true},
		{TypeWecom, 200, `{"errcode":93000,"errmsg":"invalid webhook url"}`, false},
		{TypeWecom, 200, `{"errmsg":"ok"}`, false},
		{TypeWecom, 500, `{"errcode":0}`, false},
		{TypeWecom, 200, `not json`, false},
		{TypeDingtalk, 200, `{"errcode":0,"errmsg":"ok"}`, true},
		{TypeDingtalk, 200, `{"errcode":310000,"errmsg":"sign not match"}`, false},
		{TypeDingtalk, 403, `{"errcode":0}`, false},
		{TypeLark, 200, `{"code":0,"msg":"success"}`, true},
		{TypeLark, 200, `{"StatusCode":0,"StatusMessage":"success"}`, true},
		{TypeLark, 200, `{"code":19021,"msg":"sign match fail"}`, false},
		{TypeLark, 200, `{"StatusCode":1}`, false},
		{TypeLark, 200, `{}`, false},
		{TypeLark, 400, `{"code":0}`, false},
		{TypeLark, 302, ``, false},
	}
	for i, c := range cases {
		err := checkResp(c.typ, c.status, []byte(c.body))
		if (err == nil) != c.ok {
			t.Errorf("#%d %s %d %s: err=%v 期望 ok=%v", i, c.typ, c.status, c.body, err, c.ok)
		}
	}
}

func TestFailureResultIsRedacted(t *testing.T) {
	// 远端把令牌回显在 errmsg 里：返回给调用方与投递记录的错误里不能出现令牌
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":40001,"errmsg":"invalid key ` + wecomKey + ` in ` + r.URL.String() + `"}`))
	})
	n := testNotifier(t, f, nil)
	v := addCh(t, n, f, TypeWecom, "")
	err := n.Test(context.Background(), v.ID)
	if err == nil || !strings.Contains(err.Error(), "errcode=40001") {
		t.Fatalf("应返回渠道的真实失败: %v", err)
	}
	if strings.Contains(err.Error(), wecomKey) {
		t.Errorf("错误里不应有令牌: %v", err)
	}
	l := n.Log(5)
	if len(l) != 1 || l[0].OK || l[0].Event != EvTest || l[0].Attempts != 1 {
		t.Errorf("投递记录 = %+v", l)
	}
}

// ---------------------------------------------------------------- 安全 ----

func TestUpsertValidation(t *testing.T) {
	n := New(Options{ExtraHosts: []string{"hook.corp.example"}})
	defer n.Close()
	good := map[string]string{
		TypeWecom:    "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef12-3456",
		TypeDingtalk: "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456",
		TypeLark:     "https://open.larksuite.com/open-apis/bot/v2/hook/abcdef-1234",
	}
	for typ, u := range good {
		if _, err := n.Upsert(ChannelInput{Type: typ, Name: "ok", URL: u}); err != nil {
			t.Errorf("%s 合法地址被拒绝: %v", typ, err)
		}
	}
	if _, err := n.Upsert(ChannelInput{Type: TypeLark, Name: "feishu", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef-1234"}); err != nil {
		t.Errorf("open.feishu.cn 应允许: %v", err)
	}
	if _, err := n.Upsert(ChannelInput{Type: TypeWecom, Name: "gw", URL: "https://hook.corp.example/any/path?x=1"}); err != nil {
		t.Errorf("额外允许主机应放行: %v", err)
	}
	bad := []struct {
		in   ChannelInput
		want string
	}{
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "http://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123"}, "https"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://oapi.dingtalk.com/robot/send?access_token=abcdef123"}, "不匹配"},
		{ChannelInput{Type: TypeDingtalk, Name: "a", URL: "https://open.larksuite.com/open-apis/bot/v2/hook/abcdef123"}, "不匹配"},
		{ChannelInput{Type: TypeLark, Name: "a", URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123"}, "不匹配"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://evil.example.com/cgi-bin/webhook/send?key=abcdef123"}, "不在允许列表"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://127.0.0.1/cgi-bin/webhook/send?key=abcdef123"}, "不在允许列表"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://qyapi.weixin.qq.com.evil.com/cgi-bin/webhook/send?key=abcdef123"}, "不在允许列表"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://u:p@qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123"}, "用户名"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://qyapi.weixin.qq.com:8443/cgi-bin/webhook/send?key=abcdef123"}, "443"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send"}, "形如"},
		{ChannelInput{Type: TypeDingtalk, Name: "a", URL: "https://oapi.dingtalk.com/robot/send"}, "形如"},
		{ChannelInput{Type: TypeLark, Name: "a", URL: "https://open.larksuite.com/other"}, "形如"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: ""}, "不能为空"},
		{ChannelInput{Type: "slack", Name: "a", URL: good[TypeWecom]}, "渠道类型"},
		{ChannelInput{Type: TypeWecom, Name: "", URL: good[TypeWecom]}, "名称"},
		{ChannelInput{Type: TypeWecom, Name: strings.Repeat("名", 41), URL: good[TypeWecom]}, "名称"},
		{ChannelInput{Type: TypeDingtalk, Name: "a", URL: good[TypeDingtalk], Secret: strings.Repeat("s", 129)}, "过长"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: good[TypeWecom], Secret: "x"}, "没有签名密钥"},
		{ChannelInput{Type: TypeWecom, Name: "a", URL: good[TypeWecom], Events: []string{"nope"}}, "未知的事件"},
	}
	for i, c := range bad {
		_, err := n.Upsert(c.in)
		var ie *InvalidError
		if err == nil || !strings.Contains(err.Error(), c.want) || !asInvalid(err, &ie) {
			t.Errorf("#%d 应拒绝并包含 %q: %v", i, c.want, err)
		}
	}
	if _, err := n.Upsert(ChannelInput{ID: "ch-nope", Name: "a"}); err != ErrNotFound {
		t.Errorf("更新不存在的渠道: %v", err)
	}
	// 更新时不能改类型，也不能把 URL 改到别家域名
	var v ChannelView
	for _, c := range n.Channels() {
		if c.Type == TypeWecom {
			v = c
		}
	}
	if _, err := n.Upsert(ChannelInput{ID: v.ID, Type: TypeDingtalk, Name: "a"}); err == nil {
		t.Error("不允许修改类型")
	}
	if _, err := n.Upsert(ChannelInput{ID: v.ID, Name: "a", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef123"}); err == nil {
		t.Error("更新 URL 也要校验主机与类型匹配")
	}
}

func asInvalid(err error, target **InvalidError) bool {
	ie, ok := err.(*InvalidError)
	*target = ie
	return ok
}

func TestIsBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "::1", "fe80::1", "fd00::1", "100.64.0.1", "224.0.0.1", "::ffff:10.0.0.1"} {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拒绝", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "203.0.113.5", "2606:4700::1111"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s 不应被拒绝", s)
		}
	}
}

func TestPrivateResolutionRejectedUnlessExtraHost(t *testing.T) {
	f := newFake(t, nil)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}

	// 官方域名被解析到内网地址：拒绝，且不会连到服务器
	n := New(Options{Timeout: 2 * time.Second})
	defer n.Close()
	n.lookup = lookup
	v, err := n.Upsert(ChannelInput{Type: TypeWecom, Name: "w", URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	err = n.Test(context.Background(), v.ID)
	if err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("解析到回环地址应被拒绝: %v", err)
	}
	if f.count() != 0 {
		t.Error("不应连到内网服务")
	}
	// 内网地址不重试（永久错误）
	n.Notify(Message{Event: EvFailed, Title: "x", Key: "k"})
	_ = n.Drain(time.Second)

	// 额外允许的主机放行内网地址
	n2 := New(Options{Timeout: 2 * time.Second, ExtraHosts: []string{"hook.corp.example"}, AllowInsecureForTest: true})
	defer n2.Close()
	n2.lookup = lookup
	v2, err := n2.Upsert(ChannelInput{Type: TypeWecom, Name: "gw", URL: "http://hook.corp.example:" + port + "/cgi-bin/webhook/send?key=abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	if err := n2.Test(context.Background(), v2.ID); err != nil {
		t.Fatalf("额外允许主机应放行: %v", err)
	}
	if f.count() != 1 {
		t.Errorf("服务器应收到 1 次请求: %d", f.count())
	}
}

func TestNoRedirectFollow(t *testing.T) {
	target := newFake(t, nil)
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+"/stolen", http.StatusFound)
	})
	n := testNotifier(t, f, nil)
	v := addCh(t, n, f, TypeWecom, "")
	err := n.Test(context.Background(), v.ID)
	if err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("应拒绝重定向: %v", err)
	}
	if target.count() != 0 {
		t.Error("不应跟随重定向")
	}
	// 重定向属于永久错误，不重试
	n.Notify(sample())
	n.Drain(time.Second)
	if f.count() != 2 {
		t.Errorf("重定向不应重试，请求数 = %d", f.count())
	}
}

func TestResponseBodyLimit(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 1<<20)))
	})
	n := testNotifier(t, f, nil)
	v := addCh(t, n, f, TypeWecom, "")
	if err := n.Test(context.Background(), v.ID); err == nil {
		t.Error("超大非 JSON 响应应判为失败")
	}
}

func TestEnvProxyNotUsed(t *testing.T) {
	n := New(Options{})
	defer n.Close()
	tr := n.client.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("默认不应使用环境代理")
	}
	if n.client.Timeout != 10*time.Second {
		t.Errorf("默认超时应为 10 秒: %v", n.client.Timeout)
	}
	if n.client.CheckRedirect == nil {
		t.Error("必须禁止跟随重定向")
	}
	n2 := New(Options{ProxyURL: "http://proxy.corp:3128"})
	defer n2.Close()
	if n2.client.Transport.(*http.Transport).Proxy == nil {
		t.Error("显式配置代理应生效")
	}
}

// ---------------------------------------------------------------- 重试 / 限速 / 去重 / 订阅 ----

func TestRetryWithBackoff(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(okBody))
	})
	n := testNotifier(t, f, func(o *Options) {
		o.Backoff = []time.Duration{30 * time.Millisecond, 60 * time.Millisecond, 120 * time.Millisecond}
	})
	addCh(t, n, f, TypeWecom, "")
	n.Notify(sample())
	if !n.Drain(5 * time.Second) {
		t.Fatal("未发完")
	}
	if f.count() != 3 {
		t.Fatalf("请求数 = %d，期望 3", f.count())
	}
	l := n.Log(10)
	if len(l) != 1 || !l[0].OK || l[0].Attempts != 3 {
		t.Errorf("投递记录 = %+v", l)
	}
	if d := f.get(1).at.Sub(f.get(0).at); d < 25*time.Millisecond {
		t.Errorf("第一次退避过短: %v", d)
	}
	if d := f.get(2).at.Sub(f.get(1).at); d < 55*time.Millisecond {
		t.Errorf("第二次退避过短: %v", d)
	}
}

func TestRetryGivesUpAfterThreeRetries(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":45009,"errmsg":"reach max api rate limit"}`))
	})
	n := testNotifier(t, f, nil)
	addCh(t, n, f, TypeDingtalk, "")
	n.Notify(sample())
	n.Drain(5 * time.Second)
	if f.count() != 4 {
		t.Errorf("首发 + 3 次重试 = 4 次，实际 %d", f.count())
	}
	l := n.Log(1)
	if len(l) != 1 || l[0].OK || l[0].Attempts != 4 || !strings.Contains(l[0].Error, "45009") {
		t.Errorf("投递记录 = %+v", l)
	}
}

func TestRateLimitPerChannel(t *testing.T) {
	f := newFake(t, nil)
	n := testNotifier(t, f, nil)
	addCh(t, n, f, TypeLark, "")
	for i := 0; i < 25; i++ {
		m := sample()
		m.Key = "k" + string(rune('a'+i))
		n.Notify(m)
	}
	n.Drain(5 * time.Second)
	ok, limited := 0, 0
	for _, d := range n.Log(0) {
		switch {
		case d.OK:
			ok++
		case d.Reason == "rate-limited":
			limited++
		}
	}
	if ok != 20 || limited != 5 || f.count() != 20 {
		t.Errorf("成功 %d、限速 %d、请求 %d，期望 20 / 5 / 20", ok, limited, f.count())
	}
}

func TestRateLimitWindowSlides(t *testing.T) {
	f := newFake(t, nil)
	now, adv := fixedNow(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	n := testNotifier(t, f, func(o *Options) { o.Now = now; o.RateLimit = 2 })
	addCh(t, n, f, TypeWecom, "")
	send := func(k string) { m := sample(); m.Key = k; n.Notify(m); n.Drain(time.Second) }
	send("1")
	send("2")
	send("3") // 被限速
	if f.count() != 2 {
		t.Fatalf("限速后请求数 = %d", f.count())
	}
	adv(61 * time.Second)
	send("4")
	if f.count() != 3 {
		t.Errorf("一分钟后应恢复: %d", f.count())
	}
}

func TestDedupe(t *testing.T) {
	f := newFake(t, nil)
	now, adv := fixedNow(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	n := testNotifier(t, f, func(o *Options) { o.Now = now; o.RateLimit = 1000 })
	addCh(t, n, f, TypeWecom, "")
	send := func(ev, key string) { m := sample(); m.Event, m.Key = ev, key; n.Notify(m); n.Drain(time.Second) }

	send(EvFailed, "A")
	send(EvFailed, "A") // 重复
	send(EvFailed, "B") // 不同 key
	send(EvSucceeded, "A")
	if f.count() != 3 {
		t.Fatalf("去重后请求数 = %d，期望 3", f.count())
	}
	adv(11 * time.Minute)
	send(EvFailed, "A") // 超过 10 分钟，再发
	if f.count() != 4 {
		t.Errorf("普通事件 10 分钟后应再发: %d", f.count())
	}
	send(EvAlertCritical, "x@y")
	send(EvAlertCritical, "x@y")
	adv(11 * time.Minute)
	send(EvAlertCritical, "x@y") // 告警类窗口 30 分钟，仍在窗口内
	if f.count() != 5 {
		t.Errorf("告警类 30 分钟内不应重发: %d", f.count())
	}
	adv(20 * time.Minute)
	send(EvAlertCritical, "x@y")
	if f.count() != 6 {
		t.Errorf("告警类超过 30 分钟应再发: %d", f.count())
	}
	m := sample()
	m.Key = ""
	n.Notify(m)
	n.Notify(m)
	n.Drain(time.Second)
	if f.count() != 8 {
		t.Errorf("无 Key 不去重: %d", f.count())
	}
}

func TestSubscriptionAndEnabled(t *testing.T) {
	f := newFake(t, nil)
	n := testNotifier(t, f, nil)
	only := addCh(t, n, f, TypeWecom, "", EvFailed, EvAborted)
	all := addCh(t, n, f, TypeLark, "")
	off := addCh(t, n, f, TypeDingtalk, "")
	if err := n.SetEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}
	send := func(ev string) { m := sample(); m.Event, m.Key = ev, ev; n.Notify(m); n.Drain(time.Second) }
	send(EvFailed)
	send(EvSucceeded)
	send(EvTest) // test 事件只能通过 Test 发送
	got := map[string]map[string]bool{}
	for _, d := range n.Log(0) {
		if got[d.ChannelID] == nil {
			got[d.ChannelID] = map[string]bool{}
		}
		got[d.ChannelID][d.Event] = true
	}
	if !got[only.ID][EvFailed] || got[only.ID][EvSucceeded] {
		t.Errorf("限定订阅的渠道 = %v", got[only.ID])
	}
	if !got[all.ID][EvFailed] || !got[all.ID][EvSucceeded] || got[all.ID][EvTest] {
		t.Errorf("订阅全部的渠道 = %v", got[all.ID])
	}
	if len(got[off.ID]) != 0 {
		t.Errorf("停用的渠道不应收到消息: %v", got[off.ID])
	}
	if f.count() != 3 {
		t.Errorf("请求数 = %d，期望 3", f.count())
	}
	// 全选事件等价于订阅全部
	var all12 []string
	for _, e := range Events() {
		all12 = append(all12, e.Key)
	}
	v, _ := n.Upsert(ChannelInput{ID: only.ID, Name: "n", Events: all12})
	if len(v.Events) != 0 {
		t.Errorf("全选应归一为空: %v", v.Events)
	}
}

func TestCloseDrainsQueue(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.Write([]byte(okBody))
	})
	n := testNotifier(t, f, func(o *Options) { o.Workers = 1 })
	addCh(t, n, f, TypeWecom, "")
	for i := 0; i < 3; i++ {
		m := sample()
		m.Key = string(rune('a' + i))
		n.Notify(m)
	}
	n.Close()
	if f.count() != 3 {
		t.Errorf("Close 应等待队列排空: %d", f.count())
	}
	if l := n.Log(0); len(l) != 3 {
		t.Errorf("投递记录 = %d", len(l))
	}
	n.Notify(sample()) // 关闭后无效，不 panic
	n.Close()          // 可重复调用
	if f.count() != 3 {
		t.Error("关闭后不应再发送")
	}
}

func TestCloseTimeoutAbortsRetries(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	n := testNotifier(t, f, func(o *Options) {
		o.Backoff = []time.Duration{time.Hour, time.Hour, time.Hour}
		o.CloseTimeout = 100 * time.Millisecond
	})
	addCh(t, n, f, TypeWecom, "")
	n.Notify(sample())
	start := time.Now()
	n.Close()
	if time.Since(start) > 3*time.Second {
		t.Error("Close 应在超时后返回")
	}
	l := n.Log(1)
	if len(l) != 1 || l[0].OK || l[0].Reason != "shutdown" {
		t.Errorf("未送达的消息应记为 shutdown: %+v", l)
	}
}

// ---------------------------------------------------------------- 存储 ----

func TestEncryptedStorage(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notify.json")
	key := []byte("master-key-for-test")
	url1 := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=SECRETKEY-1234567890-abcd"
	n := New(Options{File: file, MasterKey: key})
	v1, err := n.Upsert(ChannelInput{Type: TypeWecom, Name: "企微", URL: url1, Events: []string{EvFailed}})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := n.Upsert(ChannelInput{Type: TypeDingtalk, Name: "钉钉", URL: "https://oapi.dingtalk.com/robot/send?access_token=DINGTOKEN-0987654321", Secret: "SEC-very-secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	n.Close()

	raw, _ := os.ReadFile(file)
	for _, leak := range []string{"SECRETKEY-1234567890-abcd", "DINGTOKEN-0987654321", "SEC-very-secret-value", "qyapi.weixin.qq.com", "access_token"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("落盘文件不应含明文 %q", leak)
		}
	}
	if !strings.Contains(string(raw), `"plain": false`) || !strings.Contains(string(raw), "enc1:") {
		t.Errorf("应标记 plain=false 且为密文:\n%s", raw)
	}
	if st, _ := os.Stat(file); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("文件权限应为 0600: %v", st.Mode())
	}

	// 重开后可解密
	n2 := New(Options{File: file, MasterKey: key})
	defer n2.Close()
	if n2.Err() != nil {
		t.Fatal(n2.Err())
	}
	chs := n2.Channels()
	if len(chs) != 2 || chs[0].ID != v1.ID || chs[1].ID != v2.ID || !chs[1].HasSecret || chs[0].Events[0] != EvFailed {
		t.Fatalf("重开后渠道 = %+v", chs)
	}
	c, _ := n2.lookupChannel(v1.ID)
	if c.URL != url1 {
		t.Errorf("解密后 URL = %s", c.URL)
	}
	c2, _ := n2.lookupChannel(v2.ID)
	if c2.Secret != "SEC-very-secret-value" {
		t.Errorf("解密后 secret = %s", c2.Secret)
	}

	// 主密钥错误：Err 非空，且禁止修改（不会覆盖掉原文件）
	n3 := New(Options{File: file, MasterKey: []byte("another-key")})
	defer n3.Close()
	if n3.Err() == nil {
		t.Fatal("错误的主密钥应报错")
	}
	if _, err := n3.Upsert(ChannelInput{Type: TypeWecom, Name: "x", URL: url1}); err == nil {
		t.Error("加载失败时不应允许修改")
	}
	raw2, _ := os.ReadFile(file)
	if string(raw2) != string(raw) {
		t.Error("原文件不应被改动")
	}
	// 缺少主密钥同样报错
	if n4 := New(Options{File: file}); n4.Err() == nil {
		t.Error("加密文件没有主密钥应报错")
	} else {
		n4.Close()
	}
}

func TestPlainStorageWhenNoMasterKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notify.json")
	u := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=plainkey-1234"
	n := New(Options{File: file})
	if _, err := n.Upsert(ChannelInput{Type: TypeWecom, Name: "p", URL: u}); err != nil {
		t.Fatal(err)
	}
	n.Close()
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), `"plain": true`) || !strings.Contains(string(raw), "plainkey-1234") {
		t.Errorf("无主密钥应明文并标记 plain=true:\n%s", raw)
	}
	// 之后提供了主密钥：读得出来，下次保存时转为密文
	n2 := New(Options{File: file, MasterKey: []byte("k")})
	defer n2.Close()
	if n2.Err() != nil || len(n2.Channels()) != 1 {
		t.Fatalf("plain 文件应可读取: %v", n2.Err())
	}
	if _, err := n2.Upsert(ChannelInput{ID: n2.Channels()[0].ID, Name: "p2"}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(file)
	if strings.Contains(string(raw), "plainkey-1234") || !strings.Contains(string(raw), `"plain": false`) {
		t.Errorf("提供主密钥后保存应加密:\n%s", raw)
	}
}

func TestMaskedViewAndPartialUpdate(t *testing.T) {
	n := New(Options{})
	defer n.Close()
	v, err := n.Upsert(ChannelInput{Type: TypeWecom, Name: "w", URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=aaaa-bbbb-cccc-abcd"})
	if err != nil {
		t.Fatal(err)
	}
	if v.URL != "https://qyapi.weixin.qq.com/…key=****abcd" {
		t.Errorf("企业微信脱敏 = %s", v.URL)
	}
	d, _ := n.Upsert(ChannelInput{Type: TypeDingtalk, Name: "d", URL: "https://oapi.dingtalk.com/robot/send?access_token=tok-1234-5678-wxyz", Secret: "SECsecretvalue"})
	if d.URL != "https://oapi.dingtalk.com/…access_token=****wxyz" || !d.HasSecret {
		t.Errorf("钉钉脱敏 = %s hasSecret=%v", d.URL, d.HasSecret)
	}
	l, _ := n.Upsert(ChannelInput{Type: TypeLark, Name: "l", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/11111111-2222-3333-4444-555566667777"})
	if l.URL != "https://open.feishu.cn/…****7777" || l.HasSecret {
		t.Errorf("Lark 脱敏 = %s", l.URL)
	}
	js, _ := json.Marshal(n.Channels())
	for _, leak := range []string{"aaaa-bbbb", "tok-1234", "SECsecretvalue", "11111111-2222"} {
		if strings.Contains(string(js), leak) {
			t.Errorf("视图 JSON 不应泄露 %q: %s", leak, js)
		}
	}
	if strings.Contains(string(js), `"secret"`) {
		t.Errorf("视图不应有 secret 字段: %s", js)
	}
	// 更新：URL / secret 留空表示不修改
	d2, err := n.Upsert(ChannelInput{ID: d.ID, Name: "d2", Events: []string{EvFailed}})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := n.lookupChannel(d.ID)
	if d2.Name != "d2" || !d2.HasSecret || cur.Secret != "SECsecretvalue" || !strings.Contains(cur.URL, "tok-1234-5678-wxyz") {
		t.Errorf("留空应保持不变: %+v", cur)
	}
	// 更换 URL 与 secret
	if _, err := n.Upsert(ChannelInput{ID: d.ID, Name: "d2", URL: "https://oapi.dingtalk.com/robot/send?access_token=new-token-0000-9999", Secret: "NEWSEC"}); err != nil {
		t.Fatal(err)
	}
	cur, _ = n.lookupChannel(d.ID)
	if cur.Secret != "NEWSEC" || !strings.Contains(cur.URL, "new-token") {
		t.Errorf("更换失败: %+v", cur)
	}
	if err := n.Delete(v.ID); err != nil || len(n.Channels()) != 2 {
		t.Errorf("删除: %v", err)
	}
	if err := n.Delete(v.ID); err != ErrNotFound {
		t.Errorf("重复删除: %v", err)
	}
	if err := n.SetEnabled("nope", true); err != ErrNotFound {
		t.Errorf("启停不存在的渠道: %v", err)
	}
}

func TestDeliveryLogHasNoSecrets(t *testing.T) {
	dir := t.TempDir()
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		// 失败响应里回显了令牌与签名
		w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match, token ` + dingTok + ` secret SECabc123 url ` + r.URL.String() + `"}`))
	})
	n := testNotifier(t, f, func(o *Options) {
		o.File = filepath.Join(dir, "notify.json")
		o.MasterKey = []byte("k")
		o.Backoff = []time.Duration{}
	})
	addCh(t, n, f, TypeDingtalk, "SECabc123")
	m := sample()
	m.Lines = append(m.Lines, "password=hunter2 已尝试")
	n.Notify(m)
	n.Drain(2 * time.Second)
	n.Close()

	js, _ := json.Marshal(n.Log(0))
	file, _ := os.ReadFile(filepath.Join(dir, "notify-log.json"))
	for _, s := range []string{string(js), string(file)} {
		for _, leak := range []string{dingTok, "SECabc123", "access_token=T", f.srv.URL} {
			if strings.Contains(s, leak) {
				t.Errorf("投递记录泄露 %q: %s", leak, s)
			}
		}
	}
	if !strings.Contains(string(file), "310000") {
		t.Errorf("记录应保留错误码: %s", file)
	}
	// 请求体里的 password=… 也被掩码
	if strings.Contains(string(f.get(0).body), "hunter2") {
		t.Errorf("消息正文应掩码口令: %s", f.get(0).body)
	}
	// 重开后投递记录保留
	n2 := New(Options{File: filepath.Join(dir, "notify.json"), MasterKey: []byte("k")})
	defer n2.Close()
	if l := n2.Log(0); len(l) != 1 || l[0].Event != EvFailed || l[0].OK {
		t.Errorf("重开后记录 = %+v", l)
	}
}

func TestLogKeepsLast200(t *testing.T) {
	f := newFake(t, nil)
	n := testNotifier(t, f, func(o *Options) { o.RateLimit = 100000 })
	addCh(t, n, f, TypeWecom, "")
	for i := 0; i < 230; i++ {
		m := sample()
		m.Key = ""
		n.Notify(m)
		if i%50 == 0 {
			n.Drain(5 * time.Second)
		}
	}
	n.Drain(10 * time.Second)
	if l := n.Log(0); len(l) != 200 {
		t.Errorf("应只保留 200 条: %d", len(l))
	}
	if l := n.Log(5); len(l) != 5 {
		t.Errorf("limit: %d", len(l))
	}
}

// ---------------------------------------------------------------- 渲染 ----

func TestWecomTruncatesTo4096Bytes(t *testing.T) {
	m := sample()
	m.Lines = nil
	for i := 0; i < 80; i++ {
		m.Lines = append(m.Lines, strings.Repeat("长行内容", 40))
	}
	out := renderWecom(sanitizeMessage(m))
	if len(out) > 4096 || !utf8.ValidString(out) {
		t.Fatalf("长度 %d，合法 UTF-8=%v", len(out), utf8.ValidString(out))
	}
	if !strings.Contains(out, "已省略") || !strings.Contains(out, "[查看详情](https://ops.example.com/#schedule)") || !strings.Contains(out, "时间：2026-01-02 03:04:05") {
		t.Errorf("截断后应保留省略提示、时间与链接:\n%s", out)
	}
	if strings.Count(out, "<font") != strings.Count(out, "</font>") {
		t.Error("截断不应破坏 font 标签")
	}
	if got := truncateBytes(strings.Repeat("汉", 3000), 4096); len(got) > 4096 || !utf8.ValidString(got) {
		t.Errorf("truncateBytes: %d", len(got))
	}
}

func TestRenderEscapesAndLinkSafety(t *testing.T) {
	m := sample()
	m.Title = `<font color="info">伪造</font>`
	m.Lines = []string{"原因：[点我](http://evil.example)", "token=abc123 与 Authorization: Bearer xyz"}
	m.Link = "javascript:alert(1)"
	m = sanitizeMessage(m)
	if m.Link != "" {
		t.Error("非 http(s) 链接应丢弃")
	}
	w := renderWecom(m)
	if strings.Contains(w, `<font color="info">伪造`) || strings.Contains(w, "](http://evil") || strings.Contains(w, "abc123") || strings.Contains(w, "Bearer xyz") {
		t.Errorf("企业微信内容未转义 / 未掩码:\n%s", w)
	}
	_, d := renderDingtalk(m)
	if strings.Contains(d, "<font") || strings.Contains(d, "](http://evil") {
		t.Errorf("钉钉内容未转义:\n%s", d)
	}
	if got := safeLink("https://ops.example.com/a(b)#nodes"); got != "https://ops.example.com/a%28b%29#nodes" {
		t.Errorf("链接括号应转义: %s", got)
	}
	l := renderLark(sanitizeMessage(sample()))
	if !strings.HasPrefix(l, "[TiDB 运维]【严重】变更执行失败\n环境：prod") || !strings.Contains(l, "详情：https://ops.example.com/#schedule") {
		t.Errorf("Lark 文本:\n%s", l)
	}
	if len(sanitizeMessage(Message{Title: strings.Repeat("长", 500)}).Title) > 400 {
		t.Error("标题应截断")
	}
}

func TestMetaLists(t *testing.T) {
	if len(Types()) != 6 || len(Events()) != 12 {
		t.Errorf("types=%d events=%d", len(Types()), len(Events()))
	}
	n := New(Options{ExtraHosts: []string{"Hook.Corp.Example"}})
	defer n.Close()
	hs := strings.Join(n.AllowedHosts(), ",")
	for _, w := range []string{"qyapi.weixin.qq.com", "oapi.dingtalk.com", "open.larksuite.com", "open.feishu.cn", "hook.corp.example"} {
		if !strings.Contains(hs, w) {
			t.Errorf("允许主机缺少 %s: %s", w, hs)
		}
	}
}
