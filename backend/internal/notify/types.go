// @author: kevinkelly
// @license: Apache License 2.0
// @file: types.go
// @time: 2026/10/28

package notify

import (
	"errors"
	"time"
)

// 渠道类型。
const (
	TypeWecom    = "wecom"    // 企业微信群机器人
	TypeDingtalk = "dingtalk" // 钉钉群机器人
	TypeLark     = "lark"     // Lark（国际版）自定义机器人；为兼容旧配置，也接受 open.feishu.cn
	TypeFeishu   = "feishu"   // 飞书（国内版）自定义机器人
	TypeSlack    = "slack"    // Slack Incoming Webhook
	TypeOther    = "other"    // 其他：通用 JSON Webhook（主机必须在管理员配置的允许主机里）
)

// 事件键（渠道订阅与消息分发使用同一套）。
const (
	EvPending       = "change.pending"
	EvApproved      = "change.approved"
	EvRejected      = "change.rejected"
	EvStarted       = "change.started"
	EvSucceeded     = "change.succeeded"
	EvFailed        = "change.failed"
	EvAborted       = "change.aborted"
	EvExpired       = "change.expired"
	EvManual        = "change.manual"
	EvMaintOverdue  = "maint.overdue"
	EvAlertCritical = "alert.critical"
	EvDiagCritical  = "diag.critical"
	EvTest          = "test"
)

// 消息级别。
const (
	LvInfo     = "info"
	LvSuccess  = "success"
	LvWarning  = "warning"
	LvCritical = "critical"
)

// TypeInfo 描述一种渠道类型（给前端渲染表单用）。
type TypeInfo struct {
	Type         string   `json:"type"`
	Label        string   `json:"label"`
	HostExamples []string `json:"hostExamples"`
	SecretLabel  string   `json:"secretLabel"` // 签名密钥在各平台的叫法
}

// EventInfo 描述一种可订阅的事件。
type EventInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Types 返回支持的渠道类型。
func Types() []TypeInfo {
	return []TypeInfo{
		{TypeWecom, "企业微信群机器人", []string{"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…"}, "（企业微信无签名校验）"},
		{TypeDingtalk, "钉钉群机器人", []string{"https://oapi.dingtalk.com/robot/send?access_token=…"}, "加签密钥（SEC 开头）"},
		{TypeFeishu, "飞书群机器人", []string{"https://open.feishu.cn/open-apis/bot/v2/hook/…"}, "签名校验密钥"},
		{TypeLark, "Lark（国际版）自定义机器人", []string{"https://open.larksuite.com/open-apis/bot/v2/hook/…"}, "签名校验密钥"},
		{TypeSlack, "Slack", []string{"https://hooks.slack.com/services/T…/B…/…"}, "（Slack Incoming Webhook 无签名校验）"},
		{TypeOther, "其他（通用 Webhook）", []string{"https://hooks.example.com/tidbops（主机须在管理员配置的允许主机里）"}, "签名密钥（可选，HMAC-SHA256）"},
	}
}

// Events 返回可订阅的事件（不含 test）。
func Events() []EventInfo {
	return []EventInfo{
		{EvPending, "有变更等待审批"},
		{EvApproved, "变更审批通过"},
		{EvRejected, "变更被驳回"},
		{EvStarted, "变更开始执行"},
		{EvSucceeded, "变更执行成功"},
		{EvFailed, "变更执行失败"},
		{EvAborted, "变更被中止"},
		{EvExpired, "变更已过期"},
		{EvManual, "变更等待人工确认"},
		{EvMaintOverdue, "维护超过 PD 阈值"},
		{EvAlertCritical, "新出现严重告警"},
		{EvDiagCritical, "新出现严重诊断发现"},
	}
}

func knownEvent(k string) bool {
	for _, e := range Events() {
		if e.Key == k {
			return true
		}
	}
	return false
}

func knownType(t string) bool {
	switch t {
	case TypeWecom, TypeDingtalk, TypeLark, TypeFeishu, TypeSlack, TypeOther:
		return true
	}
	return false
}

// Message 是与渠道无关的通知内容，由各渠道自行渲染。
// 调用方负责保证 Lines 里没有命令输出、命令行、凭据与完整 SQL；Notifier 另外做兜底的脱敏与截断。
type Message struct {
	Env     string    // test | staging | prod
	Cluster string    // 集群名
	Event   string    // 事件键（Ev*）
	Title   string    // 事件标题
	Lines   []string  // 明细行（变更标题与 ID、风险、申请人、原因……）
	Link    string    // 跳转链接，空则不显示
	Level   string    // info | success | warning | critical，空视为 info
	Key     string    // 去重键：同一 (Event, Key) 在去重窗口内只发一次；空表示不去重
	Time    time.Time // 事件时间，零值取当前时间
}

// ChannelInput 是创建 / 更新渠道的入参。ID 为空表示创建。
// 更新时 URL / Secret 留空表示不修改；ClearSecret 为 true 时清除已保存的 Secret。
type ChannelInput struct {
	ID          string   `json:"id,omitempty"`
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Enabled     *bool    `json:"enabled,omitempty"` // 创建时缺省为 true，更新时缺省为不修改
	URL         string   `json:"url,omitempty"`
	Secret      string   `json:"secret,omitempty"`
	ClearSecret bool     `json:"clearSecret,omitempty"`
	Events      []string `json:"events"` // 空表示订阅全部
}

// ChannelView 是返回给前端的渠道：URL 脱敏，Secret 只给布尔值。
type ChannelView struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	URL       string    `json:"url"` // 脱敏：https://host/…key=****abcd
	Host      string    `json:"host"`
	HasSecret bool      `json:"hasSecret"`
	Events    []string  `json:"events"` // 空数组表示订阅全部
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Delivery 是一条投递记录，不含 URL、secret 与响应原文。
type Delivery struct {
	Time        time.Time `json:"time"`
	ChannelID   string    `json:"channelId"`
	ChannelName string    `json:"channelName"`
	Event       string    `json:"event"`
	Title       string    `json:"title"`
	OK          bool      `json:"ok"`
	Attempts    int       `json:"attempts"`
	Error       string    `json:"error,omitempty"`
	Reason      string    `json:"reason,omitempty"` // rate-limited | queue-full | channel-deleted | shutdown
}

// ErrNotFound 表示渠道不存在。
var ErrNotFound = errors.New("渠道不存在")

// InvalidError 表示入参不合法（给用户看的中文说明）。
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(msg string) error { return &InvalidError{Msg: msg} }
