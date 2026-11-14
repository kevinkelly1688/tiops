// @author: kevinkelly
// @license: Apache License 2.0
// @file: notify_types_test.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 飞书、Slack、「其他」三种渠道类型。

func addByURL(t *testing.T, n *Notifier, f *fake, typ, path, secret string) ChannelView {
	t.Helper()
	v, err := n.Upsert(ChannelInput{Type: typ, Name: "ch-" + typ, URL: f.srv.URL + path, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFeishuRequestWithSign(t *testing.T) {
	f := newFake(t, nil)
	now, _ := fixedNow(time.Unix(1700000000, 0))
	n := testNotifier(t, f, func(o *Options) { o.Now = now })
	v := addByURL(t, n, f, TypeFeishu, "/open-apis/bot/v2/hook/"+larkTok, "SECabc123")
	if err := n.Test(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	var body struct {
		MsgType   string `json:"msg_type"`
		Timestamp string
		Sign      string
		Content   struct{ Text string }
	}
	_ = json.Unmarshal(f.get(0).body, &body)
	// 飞书与 Lark 的签名与载荷完全一致
	if body.MsgType != "text" || body.Timestamp != "1700000000" || body.Sign != "UqhI0v4zAkSwI4hNYBuHQvnrAqshA0UaeBGHCUMPX70=" || !strings.Contains(body.Content.Text, "通知渠道测试") {
		t.Errorf("飞书请求体 = %s", f.get(0).body)
	}
}

func TestSlackRequestAndResponse(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			w.Write([]byte("ok"))
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid_payload"))
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	})
	n := testNotifier(t, f, nil)
	v := addByURL(t, n, f, TypeSlack, "/services/T0000/B0000/abcdEFGH1234", "")
	if err := n.Test(context.Background(), v.ID); err != nil {
		t.Fatalf("响应体为 ok 应成功：%v", err)
	}
	var body map[string]string
	_ = json.Unmarshal(f.get(0).body, &body)
	if !strings.HasPrefix(body["text"], "*[TiDB 运维]") || !strings.Contains(body["text"], "通知渠道测试") || !strings.Contains(body["text"], "环境：prod") {
		t.Errorf("Slack 请求体 = %s", f.get(0).body)
	}
	if err := n.Test(context.Background(), v.ID); err == nil || !strings.Contains(err.Error(), "invalid_payload") {
		t.Errorf("400 应失败并带上错误码：%v", err)
	}
	if err := n.Test(context.Background(), v.ID); err == nil {
		t.Error("200 但响应体不是 ok 应失败")
	}
	if _, err := n.Upsert(ChannelInput{ID: v.ID, Name: "x", Secret: "SEC123456"}); err == nil || !strings.Contains(err.Error(), "没有签名密钥") {
		t.Errorf("Slack 不接受签名密钥：%v", err)
	}
}

func TestSlackEscape(t *testing.T) {
	m := sanitizeMessage(Message{Title: "a <!channel> & *b*", Lines: []string{"x <@U1> _y_"}, Link: "https://ops.example.com/#a|b", Level: LvWarning, Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	got := renderSlack(m)
	for _, bad := range []string{"<!channel>", "<@U1>", "*b*"} {
		if strings.Contains(got, bad) {
			t.Errorf("未转义 %q：\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "&lt;!channel&gt; &amp;") || !strings.Contains(got, "<https://ops.example.com/#a%7Cb|查看详情>") {
		t.Errorf("Slack 渲染:\n%s", got)
	}
}

func TestOtherRequestAndSignature(t *testing.T) {
	var mu sync.Mutex
	var hdr http.Header
	status := http.StatusNoContent
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hdr = r.Header.Clone()
		st := status
		mu.Unlock()
		w.WriteHeader(st)
	})
	now, _ := fixedNow(time.Unix(1700000000, 0))
	n := testNotifier(t, f, func(o *Options) { o.Now = now })
	v := addByURL(t, n, f, TypeOther, "/hook", "mysecret")
	if err := n.Test(context.Background(), v.ID); err != nil {
		t.Fatalf("2xx 应成功：%v", err)
	}
	h := f.get(0)
	var p otherPayload
	if err := json.Unmarshal(h.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Source != "tidb-ops" || p.Version != 1 || p.Event != EvTest || p.Env != "prod" || p.Cluster != "c1" || !strings.Contains(p.Title, "通知渠道测试") || p.Lines == nil || p.Text == "" {
		t.Errorf("通用载荷 = %s", h.body)
	}
	mu.Lock()
	ts, sig := hdr.Get("X-TiDBOps-Timestamp"), hdr.Get("X-TiDBOps-Signature")
	mu.Unlock()
	if ts != "1700000000" || sig != otherSign("mysecret", 1700000000, h.body) || !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("签名头 ts=%q sig=%q", ts, sig)
	}
	// 没有 secret 时不带签名头
	if _, err := n.Upsert(ChannelInput{ID: v.ID, Name: "x", ClearSecret: true}); err != nil {
		t.Fatal(err)
	}
	_ = n.Test(context.Background(), v.ID)
	mu.Lock()
	if hdr.Get("X-TiDBOps-Signature") != "" {
		t.Error("无 secret 不应带签名头")
	}
	status = http.StatusInternalServerError
	mu.Unlock()
	if err := n.Test(context.Background(), v.ID); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("5xx 应失败：%v", err)
	}
}

func TestNewTypesValidation(t *testing.T) {
	n := New(Options{ExtraHosts: []string{"hook.corp.example"}})
	defer n.Close()
	good := map[string]string{
		TypeFeishu: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef-1234",
		TypeSlack:  "https://hooks.slack.com/services/T0000/B0000/abcdEFGH1234",
		TypeOther:  "https://hook.corp.example/tidbops?x=1",
	}
	for typ, u := range good {
		if _, err := n.Upsert(ChannelInput{Type: typ, Name: "ok", URL: u}); err != nil {
			t.Errorf("%s 合法地址被拒绝: %v", typ, err)
		}
	}
	bad := []struct {
		in   ChannelInput
		want string
	}{
		{ChannelInput{Type: TypeFeishu, Name: "a", URL: "https://open.larksuite.com/open-apis/bot/v2/hook/abcdef-1234"}, "不匹配"},
		{ChannelInput{Type: TypeFeishu, Name: "a", URL: "https://open.feishu.cn/other"}, "飞书 Webhook 形如"},
		{ChannelInput{Type: TypeSlack, Name: "a", URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abcdef123"}, "不匹配"},
		{ChannelInput{Type: TypeSlack, Name: "a", URL: "https://hooks.slack.com/services/T1/B2"}, "Slack Webhook 形如"},
		{ChannelInput{Type: TypeSlack, Name: "a", URL: "http://hooks.slack.com/services/T0000/B0000/abcdEFGH1234"}, "https"},
		{ChannelInput{Type: TypeOther, Name: "a", URL: "https://hooks.example.com/x"}, "--notify-allow-hosts"},
		{ChannelInput{Type: TypeOther, Name: "a", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef-1234"}, "--notify-allow-hosts"},
		{ChannelInput{Type: "teams", Name: "a", URL: "https://example.com"}, "渠道类型必须是"},
	}
	for _, c := range bad {
		_, err := n.Upsert(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v 期望错误含 %q，得到 %v", c.in, c.want, err)
		}
	}
	// 旧配置兼容：Lark 类型仍然接受 open.feishu.cn
	if _, err := n.Upsert(ChannelInput{Type: TypeLark, Name: "old", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdef-1234"}); err != nil {
		t.Errorf("Lark 类型应继续接受 open.feishu.cn: %v", err)
	}
}

func TestCheckRespNewTypes(t *testing.T) {
	cases := []struct {
		typ    string
		status int
		body   string
		ok     bool
	}{
		{TypeFeishu, 200, `{"code":0,"msg":"success"}`, true},
		{TypeFeishu, 200, `{"code":19021,"msg":"sign match fail"}`, false},
		{TypeSlack, 200, "ok", true},
		{TypeSlack, 200, "ok\n", true},
		{TypeSlack, 200, "no_text", false},
		{TypeSlack, 404, "no_service", false},
		{TypeSlack, 302, "", false},
		{TypeOther, 200, "", true},
		{TypeOther, 202, "anything", true},
		{TypeOther, 204, "", true},
		{TypeOther, 400, "", false},
		{TypeOther, 302, "", false},
	}
	for _, c := range cases {
		err := checkResp(c.typ, c.status, []byte(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s %d %q: err=%v 期望 ok=%v", c.typ, c.status, c.body, err, c.ok)
		}
	}
	if err := checkResp(TypeFeishu, 200, []byte(`{"code":19021,"msg":"x"}`)); err == nil || !strings.HasPrefix(err.Error(), "飞书 返回") {
		t.Errorf("飞书错误文案：%v", err)
	}
}

func TestMaskAndTokenForNewTypes(t *testing.T) {
	if got := maskURL(TypeSlack, "https://hooks.slack.com/services/T0000/B0000/abcdEFGH1234"); !strings.HasSuffix(got, "****1234") || strings.Contains(got, "abcdEFGH") {
		t.Errorf("Slack 脱敏 = %s", got)
	}
	if got := maskURL(TypeFeishu, "https://open.feishu.cn/open-apis/bot/v2/hook/11111111-2222-3333-4444-555566667777"); !strings.HasSuffix(got, "****7777") {
		t.Errorf("飞书脱敏 = %s", got)
	}
}
