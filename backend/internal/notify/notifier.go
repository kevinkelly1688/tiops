// @author: kevinkelly
// @license: Apache License 2.0
// @file: notifier.go
// @time: 2026/10/28

package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxLog      = 200 // 保留的投递记录条数
	maxChannels = 20
	maxName     = 40
	maxSecret   = 128
)

// Options 是 Notifier 的构造参数。
type Options struct {
	File       string   // 渠道文件路径（0600，原子写）；空表示只放内存
	LogFile    string   // 投递记录文件；空则取渠道文件同目录的 notify-log.json
	MasterKey  []byte   // 加密主密钥；空则渠道文件为明文（文件内 plain=true）
	ExtraHosts []string // 额外允许的 Webhook 主机（私有化网关）；这些主机放行内网地址，且不限渠道类型
	Env        string   // 测试消息里展示的环境
	Cluster    string   // 测试消息里展示的集群
	ProxyURL   string   // 显式配置的出站代理；空表示不使用任何代理（不读环境变量）

	// HTTPClient 仅供测试注入（打到 httptest 服务）。注入后仍会强制「不跟随重定向」。
	HTTPClient *http.Client
	// AllowInsecureForTest 仅供测试：允许 http 地址。生产路径不要设置。
	AllowInsecureForTest bool

	Workers           int             // 默认 2
	QueueSize         int             // 默认 256
	Backoff           []time.Duration // 失败重试的退避序列，默认 1s/3s/9s（即最多重试 3 次）；测试可缩小
	RateLimit         int             // 每渠道每分钟最多条数，默认 20
	DedupeWindow      time.Duration   // 默认 10 分钟
	AlertDedupeWindow time.Duration   // 告警类事件，默认 30 分钟
	Timeout           time.Duration   // 单次请求超时，默认 10 秒
	CloseTimeout      time.Duration   // Close 等待队列排空的最长时间，默认 15 秒
	Now               func() time.Time
	Logf              func(string, ...any)
}

type job struct {
	chID     string
	msg      Message
	attempts int
}

// Notifier 管理渠道并异步投递通知。
type Notifier struct {
	opts   Options
	key    []byte
	client *http.Client
	lookup func(ctx context.Context, host string) ([]net.IP, error)

	mu       sync.Mutex
	channels []*channel
	loadErr  error
	closed   bool
	inflight int
	dedupe   map[string]time.Time
	rate     map[string][]time.Time

	logMu   sync.Mutex
	log     []Delivery // 新的在前
	logPath string

	q      chan job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 创建并启动 Notifier。渠道文件读取 / 解密失败时不会 panic：Err() 返回原因，
// 此时不允许修改渠道（避免覆盖掉无法解密的文件）。
func New(opts Options) *Notifier {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}
	if opts.Backoff == nil {
		opts.Backoff = []time.Duration{time.Second, 3 * time.Second, 9 * time.Second}
	}
	if opts.RateLimit <= 0 {
		opts.RateLimit = 20
	}
	if opts.DedupeWindow <= 0 {
		opts.DedupeWindow = 10 * time.Minute
	}
	if opts.AlertDedupeWindow <= 0 {
		opts.AlertDedupeWindow = 30 * time.Minute
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 15 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	n := &Notifier{opts: opts, key: deriveKey(opts.MasterKey), dedupe: map[string]time.Time{}, rate: map[string][]time.Time{}, lookup: defaultLookup}
	n.q = make(chan job, opts.QueueSize)
	n.ctx, n.cancel = context.WithCancel(context.Background())

	n.logPath = opts.LogFile
	if n.logPath == "" && opts.File != "" {
		n.logPath = filepath.Join(filepath.Dir(opts.File), "notify-log.json")
	}
	n.channels, n.loadErr = loadChannels(opts.File, n.key)
	n.log = loadLog(n.logPath)

	if opts.HTTPClient != nil {
		c := *opts.HTTPClient
		n.client = &c
	} else {
		tr := &http.Transport{
			Proxy:                 nil, // 不使用环境代理
			DialContext:           n.dial,
			TLSHandshakeTimeout:   opts.Timeout,
			ResponseHeaderTimeout: opts.Timeout,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		}
		if opts.ProxyURL != "" {
			if pu, err := url.Parse(opts.ProxyURL); err == nil && pu.Host != "" {
				tr.Proxy = http.ProxyURL(pu)
				tr.DialContext = (&net.Dialer{Timeout: opts.Timeout}).DialContext // 经代理时由代理解析目标，主机白名单仍然生效
			}
		}
		n.client = &http.Client{Transport: tr}
	}
	n.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if n.client.Timeout == 0 {
		n.client.Timeout = opts.Timeout
	}

	for i := 0; i < opts.Workers; i++ {
		n.wg.Add(1)
		go n.worker()
	}
	return n
}

// Err 返回加载渠道文件时的错误（nil 表示正常）。
func (n *Notifier) Err() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.loadErr
}

func (n *Notifier) now() time.Time { return n.opts.Now() }

// ---------------------------------------------------------------- 渠道管理 ----

func (n *Notifier) view(c *channel) ChannelView {
	ev := append([]string{}, c.Events...)
	host := ""
	if u, err := url.Parse(c.URL); err == nil {
		host = u.Host
	}
	return ChannelView{ID: c.ID, Type: c.Type, Name: c.Name, Enabled: c.Enabled, URL: maskURL(c.Type, c.URL), Host: host, HasSecret: c.Secret != "", Events: ev, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// Channels 返回脱敏后的渠道列表（按创建时间排序）。
func (n *Notifier) Channels() []ChannelView {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]ChannelView, 0, len(n.channels))
	for _, c := range n.channels {
		out = append(out, n.view(c))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// View 返回单个渠道的脱敏视图。
func (n *Notifier) View(id string) (ChannelView, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.channels {
		if c.ID == id {
			return n.view(c), true
		}
	}
	return ChannelView{}, false
}

func cloneChannels(in []*channel) []*channel {
	out := make([]*channel, len(in))
	for i, c := range in {
		cp := *c
		cp.Events = append([]string(nil), c.Events...)
		out[i] = &cp
	}
	return out
}

// commitLocked 把新的渠道列表落盘，成功后才替换内存中的列表。
func (n *Notifier) commitLocked(list []*channel) error {
	if n.opts.File != "" {
		b, err := encodeChannels(list, n.key)
		if err != nil {
			return fmt.Errorf("保存渠道失败: %w", err)
		}
		if err := writeFileAtomic(n.opts.File, b); err != nil {
			return fmt.Errorf("保存渠道失败: %w", err)
		}
	}
	n.channels = list
	return nil
}

func (n *Notifier) writableLocked() error {
	if n.loadErr != nil {
		return fmt.Errorf("渠道文件无法读取，已禁止修改：%w", n.loadErr)
	}
	return nil
}

func cleanEvents(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" || e == EvTest || seen[e] {
			continue
		}
		if !knownEvent(e) {
			return nil, invalid("未知的事件类型：" + cleanText(e, 40))
		}
		seen[e] = true
		out = append(out, e)
	}
	if len(out) == len(Events()) {
		out = nil // 全选等价于订阅全部
	}
	return out, nil
}

// Upsert 创建（ID 为空）或更新渠道。更新时 URL / Secret 留空表示不修改。
func (n *Notifier) Upsert(in ChannelInput) (ChannelView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.writableLocked(); err != nil {
		return ChannelView{}, err
	}
	name := strings.TrimSpace(in.Name)
	if nr := utf8.RuneCountInString(name); nr == 0 || nr > maxName {
		return ChannelView{}, invalid(fmt.Sprintf("渠道名称长度须在 1–%d 个字符之间", maxName))
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return ChannelView{}, invalid("渠道名称不能包含换行")
	}
	secret := strings.TrimSpace(in.Secret)
	if len(secret) > maxSecret {
		return ChannelView{}, invalid(fmt.Sprintf("签名密钥过长（最多 %d 个字符）", maxSecret))
	}
	if strings.ContainsAny(secret, "\r\n") {
		return ChannelView{}, invalid("签名密钥不能包含换行")
	}
	events, err := cleanEvents(in.Events)
	if err != nil {
		return ChannelView{}, err
	}
	list := cloneChannels(n.channels)
	now := n.now()
	var cur *channel
	if in.ID == "" {
		if !knownType(in.Type) {
			return ChannelView{}, invalid("渠道类型必须是 wecom、dingtalk、feishu、lark、slack 或 other")
		}
		if len(list) >= maxChannels {
			return ChannelView{}, invalid(fmt.Sprintf("渠道数量已达上限（%d）", maxChannels))
		}
		if _, err := n.validateURL(in.Type, in.URL); err != nil {
			return ChannelView{}, err
		}
		cur = &channel{ID: newID(), Type: in.Type, Enabled: true, CreatedAt: now}
		list = append(list, cur)
	} else {
		for _, c := range list {
			if c.ID == in.ID {
				cur = c
			}
		}
		if cur == nil {
			return ChannelView{}, ErrNotFound
		}
		if in.Type != "" && in.Type != cur.Type {
			return ChannelView{}, invalid("渠道类型创建后不能修改，请新建渠道")
		}
		if strings.TrimSpace(in.URL) != "" {
			if _, err := n.validateURL(cur.Type, in.URL); err != nil {
				return ChannelView{}, err
			}
		}
	}
	if secret != "" && cur.Type == TypeWecom {
		return ChannelView{}, invalid("企业微信群机器人没有签名密钥")
	}
	if secret != "" && cur.Type == TypeSlack {
		return ChannelView{}, invalid("Slack Incoming Webhook 没有签名密钥")
	}
	cur.Name, cur.Events, cur.UpdatedAt = name, events, now
	if u := strings.TrimSpace(in.URL); u != "" {
		cur.URL = u
	}
	if secret != "" {
		cur.Secret = secret
	} else if in.ClearSecret {
		cur.Secret = ""
	}
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	if err := n.commitLocked(list); err != nil {
		return ChannelView{}, err
	}
	return n.view(cur), nil
}

// Delete 删除渠道。
func (n *Notifier) Delete(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.writableLocked(); err != nil {
		return err
	}
	list := cloneChannels(n.channels)
	for i, c := range list {
		if c.ID == id {
			return n.commitLocked(append(list[:i], list[i+1:]...))
		}
	}
	return ErrNotFound
}

// SetEnabled 启用 / 停用渠道。
func (n *Notifier) SetEnabled(id string, on bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.writableLocked(); err != nil {
		return err
	}
	list := cloneChannels(n.channels)
	for _, c := range list {
		if c.ID == id {
			c.Enabled, c.UpdatedAt = on, n.now()
			return n.commitLocked(list)
		}
	}
	return ErrNotFound
}

func (n *Notifier) lookupChannel(id string) (channel, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range n.channels {
		if c.ID == id {
			cp := *c
			return cp, true
		}
	}
	return channel{}, false
}

// ---------------------------------------------------------------- 发送 ----

func isAlertEvent(ev string) bool {
	return ev == EvAlertCritical || ev == EvDiagCritical || ev == EvMaintOverdue
}

// Test 同步向渠道发一条 test 消息（单次，不重试，不受限速与去重影响），返回渠道的真实结果。
func (n *Notifier) Test(ctx context.Context, id string) error {
	c, ok := n.lookupChannel(id)
	if !ok {
		return ErrNotFound
	}
	m := sanitizeMessage(Message{Env: n.opts.Env, Cluster: n.opts.Cluster, Event: EvTest, Level: LvInfo, Time: n.now(),
		Title: "通知渠道测试", Lines: []string{"这是一条测试消息，收到说明渠道「" + c.Name + "」配置正确。"}})
	err := n.deliver(ctx, &c, m)
	d := Delivery{Time: n.now(), ChannelID: c.ID, ChannelName: c.Name, Event: EvTest, Title: m.Title, OK: err == nil, Attempts: 1}
	if err != nil {
		d.Error = err.Error()
	}
	n.appendLog(d)
	return err
}

// Notify 异步分发消息：按渠道订阅筛选，(Event, Key) 去重，每渠道限速，入队后由 worker 发送。
func (n *Notifier) Notify(msg Message) {
	if msg.Event == "" || msg.Event == EvTest {
		return
	}
	if msg.Time.IsZero() {
		msg.Time = n.now()
	}
	msg = sanitizeMessage(msg)
	now := n.now()

	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	var targets []*channel
	for _, c := range n.channels {
		if c.Enabled && c.subscribes(msg.Event) {
			cp := *c
			targets = append(targets, &cp)
		}
	}
	if len(targets) == 0 {
		n.mu.Unlock()
		return
	}
	if msg.Key != "" {
		win := n.opts.DedupeWindow
		if isAlertEvent(msg.Event) {
			win = n.opts.AlertDedupeWindow
		}
		for k, t := range n.dedupe { // 顺手清理过期项
			if now.Sub(t) > n.opts.AlertDedupeWindow && now.Sub(t) > n.opts.DedupeWindow {
				delete(n.dedupe, k)
			}
		}
		dk := msg.Event + "\x00" + msg.Key
		if t, ok := n.dedupe[dk]; ok && now.Sub(t) < win {
			n.mu.Unlock()
			return
		}
		n.dedupe[dk] = now
	}
	var dropped []Delivery
	for _, c := range targets {
		cut := now.Add(-time.Minute)
		var keep []time.Time
		for _, t := range n.rate[c.ID] {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		if len(keep) >= n.opts.RateLimit {
			n.rate[c.ID] = keep
			dropped = append(dropped, Delivery{Time: now, ChannelID: c.ID, ChannelName: c.Name, Event: msg.Event, Title: msg.Title, Error: fmt.Sprintf("rate-limited：该渠道每分钟最多 %d 条，已丢弃", n.opts.RateLimit), Reason: "rate-limited"})
			continue
		}
		n.rate[c.ID] = append(keep, now)
		select {
		case n.q <- job{chID: c.ID, msg: msg}:
			n.inflight++
		default:
			dropped = append(dropped, Delivery{Time: now, ChannelID: c.ID, ChannelName: c.Name, Event: msg.Event, Title: msg.Title, Error: "queue-full：发送队列已满，已丢弃", Reason: "queue-full"})
		}
	}
	n.mu.Unlock()
	for _, d := range dropped {
		n.appendLog(d)
	}
}

func (n *Notifier) jobDone() {
	n.mu.Lock()
	n.inflight--
	n.mu.Unlock()
}

func (n *Notifier) worker() {
	defer n.wg.Done()
	for {
		select {
		case <-n.ctx.Done():
			return
		case j := <-n.q:
			n.handle(j)
		}
	}
}

func (n *Notifier) handle(j job) {
	c, ok := n.lookupChannel(j.chID)
	if !ok {
		n.appendLog(Delivery{Time: n.now(), ChannelID: j.chID, ChannelName: "(已删除)", Event: j.msg.Event, Title: j.msg.Title, Attempts: j.attempts, Error: "渠道已删除", Reason: "channel-deleted"})
		n.jobDone()
		return
	}
	err := n.deliver(n.ctx, &c, j.msg)
	j.attempts++
	d := Delivery{Time: n.now(), ChannelID: c.ID, ChannelName: c.Name, Event: j.msg.Event, Title: j.msg.Title, Attempts: j.attempts}
	if err == nil {
		d.OK = true
		n.appendLog(d)
		n.jobDone()
		return
	}
	if isPermanent(err) || j.attempts > len(n.opts.Backoff) || n.ctx.Err() != nil {
		d.Error = err.Error()
		n.appendLog(d)
		n.jobDone()
		n.opts.Logf("通知投递失败 渠道=%s 事件=%s 次数=%d: %s", c.Name, j.msg.Event, j.attempts, err.Error())
		return
	}
	wait := n.opts.Backoff[j.attempts-1]
	go func() { // 退避期间不占用 worker
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-n.ctx.Done():
			d.Error, d.Reason = "平台停止，重试已取消："+err.Error(), "shutdown"
			n.appendLog(d)
			n.jobDone()
			return
		}
		select {
		case n.q <- j:
		case <-n.ctx.Done():
			d.Error, d.Reason = "平台停止，重试已取消："+err.Error(), "shutdown"
			n.appendLog(d)
			n.jobDone()
		}
	}()
}

// ---------------------------------------------------------------- 投递记录与关闭 ----

func (n *Notifier) appendLog(d Delivery) {
	n.logMu.Lock()
	defer n.logMu.Unlock()
	n.log = append([]Delivery{d}, n.log...)
	if len(n.log) > maxLog {
		n.log = n.log[:maxLog]
	}
	if n.logPath != "" {
		if b, err := json.MarshalIndent(n.log, "", "  "); err == nil {
			if err := writeFileAtomic(n.logPath, b); err != nil {
				n.opts.Logf("写入通知投递记录失败: %v", err)
			}
		}
	}
}

// Log 返回最近的投递记录（新的在前）；limit<=0 表示全部（最多 200 条）。
func (n *Notifier) Log(limit int) []Delivery {
	n.logMu.Lock()
	defer n.logMu.Unlock()
	if limit <= 0 || limit > len(n.log) {
		limit = len(n.log)
	}
	return append(make([]Delivery, 0, limit), n.log[:limit]...)
}

// Drain 等待队列（含等待重试的消息）发送完毕；超时返回 false。
func (n *Notifier) Drain(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		n.mu.Lock()
		idle := n.inflight <= 0
		n.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Close 停止接收新消息，等待队列排空（最长 CloseTimeout），然后停止 worker。可重复调用。
func (n *Notifier) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	n.mu.Unlock()
	n.Drain(n.opts.CloseTimeout)
	n.cancel()
	n.wg.Wait()
	for { // 超时未发出的消息记为未送达
		select {
		case j := <-n.q:
			if c, ok := n.lookupChannel(j.chID); ok {
				n.appendLog(Delivery{Time: n.now(), ChannelID: c.ID, ChannelName: c.Name, Event: j.msg.Event, Title: j.msg.Title, Attempts: j.attempts, Error: "平台停止，消息未送达", Reason: "shutdown"})
			}
		default:
			if tr, ok := n.client.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
			return
		}
	}
}
