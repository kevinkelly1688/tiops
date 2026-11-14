// @author: kevinkelly
// @license: Apache License 2.0
// @file: doc.go
// @time: 2026/10/28

// Package notify 通过 Webhook 把平台事件推送到企业微信群机器人、钉钉群机器人、飞书群机器人、Lark（国际版）自定义机器人、Slack，
// 以及「其他」（通用 JSON Webhook）。
//
// # API 摘要
//
//	n := notify.New(notify.Options{File: dir+"/notify.json", MasterKey: key, ExtraHosts: hosts, Env: "prod", Cluster: "c1"})
//	defer n.Close()                              // 停止接收、等待队列排空（最长 CloseTimeout），再停 worker
//	n.Err() error                                // 渠道文件读取 / 解密失败的原因；非空时禁止修改渠道（不覆盖原文件）
//	n.Channels() []ChannelView                   // 脱敏列表：url 只留末 4 位，secret 只给 hasSecret
//	n.View(id) (ChannelView, bool)
//	n.Upsert(ChannelInput) (ChannelView, error)  // ID 为空=创建；更新时 url / secret 留空=不改；ClearSecret 清除 secret
//	n.Delete(id) / n.SetEnabled(id, on) error
//	n.Test(ctx, id) error                        // 同步发一条 test 消息（单次、不重试、不限速），返回渠道真实结果（已打码）
//	n.Notify(Message)                            // 异步：按订阅筛选 → (Event,Key) 去重 → 每渠道限速 → 入队 → worker 发送 → 失败重试
//	n.Log(limit) []Delivery                      // 最近投递记录（新的在前，最多 200 条）
//	n.Drain(timeout) bool                        // 等队列（含等待重试的消息）发完
//	n.AllowedHosts() []string                    // 官方域名 + 额外允许主机
//	notify.Types() / notify.Events()             // 前端渲染表单用的元数据
//
// 错误类型：*InvalidError（入参不合法，HTTP 400）、ErrNotFound（404）。
//
// # 渠道与协议
//
//   - wecom：POST https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…，
//     {"msgtype":"markdown","markdown":{"content":…}}，内容 ≤4096 字节（超限按行丢弃尾部明细，保留时间与链接）。
//     成功 = HTTP 200 且 errcode==0。无签名。
//   - dingtalk：POST https://oapi.dingtalk.com/robot/send?access_token=…，
//     {"msgtype":"markdown","markdown":{"title":…,"text":…}}；配置了加签 secret 时追加
//     &timestamp=<毫秒>&sign=urlencode(base64(HMAC-SHA256(key=secret, msg=timestamp+"\n"+secret)))。成功 = HTTP 200 且 errcode==0。
//     若机器人启用了「自定义关键词」安全设置，关键词请设为 "TiDB"（每条消息的标题都含「[TiDB 运维]」）。
//   - lark：POST https://open.larksuite.com/open-apis/bot/v2/hook/… 或 open.feishu.cn，
//     {"msg_type":"text","content":{"text":…}}；配置了签名校验 secret 时 body 追加 timestamp（秒，字符串）与
//     sign=base64(HMAC-SHA256(key=timestamp+"\n"+secret, msg 为空))。成功 = HTTP 200 且 code==0（或 StatusCode==0）。
//     lark 类型为兼容旧配置，同时接受 open.feishu.cn。
//   - feishu：POST https://open.feishu.cn/open-apis/bot/v2/hook/…，载荷、签名、成功判定与 lark 完全一致，只是域名限定为 open.feishu.cn。
//   - slack：POST https://hooks.slack.com/services/T…/B…/…（Incoming Webhook），{"text":mrkdwn 文本}（& < > 转义，* _ ~ ` 全角化防注入）；
//     无签名。成功 = HTTP 200 且响应体为 ok；失败时响应体是错误码（如 invalid_payload、no_service），会带进错误信息。
//   - other：通用 JSON Webhook，POST 到管理员指定的 https 地址（主机必须在 Options.ExtraHosts / --notify-allow-hosts 里，没有官方域名）。
//     载荷 {"source":"tidb-ops","version":1,"event","level","title","env","cluster","lines":[…],"link","time":RFC3339,"text":纯文本}；
//     配置了 secret 时追加请求头 X-TiDBOps-Timestamp（秒）与 X-TiDBOps-Signature: sha256=hex(HMAC-SHA256(key=secret, msg=timestamp+"."+body))。
//     成功 = 任意 2xx（不解析响应体）。
//
// # 安全
//
//   - 只允许 https；主机必须在白名单且与渠道类型匹配（官方域名 + Options.ExtraHosts；额外主机不限类型且放行内网地址）。
//   - 拨号时自行解析主机并检查每个 IP：回环 / 私网 / 链路本地 / CGNAT / 组播 / 未指定地址一律拒绝（防 SSRF / DNS 重绑定）。
//   - 不跟随重定向；不读环境代理（需要时显式配置 Options.ProxyURL，此时拨号层的 IP 检查由代理承担）；单次超时 10 秒；响应体最多读 64KB。
//   - 渠道 url / secret 以 AES-256-GCM 加密落盘（密钥 = SHA-256(MasterKey)，GCM 的附加数据绑定渠道 ID 与字段名）；
//     MasterKey 为空则明文并在文件里标记 "plain": true。文件 0600、原子写（临时文件 + rename）。
//   - 投递记录（notify-log.json，与渠道文件同目录）不含 URL / secret；错误文本里的 URL、令牌、secret 一律打码。
//   - 消息正文兜底清洗：折叠换行、掩码 password=/token=/secret= 之类片段、单行 ≤300 字、转义 markdown / HTML、链接只接受 http(s)。
//     调用方仍须保证不传入命令输出、命令行、凭据与完整 SQL。
//
// # 投递语义
//
//   - 队列 256、2 个 worker；失败重试 3 次（退避 1s/3s/9s，Options.Backoff 可缩放）；配置 / 安全类错误与重定向不重试。
//   - 每渠道每分钟最多 20 条，超出丢弃并记录 reason=rate-limited；队列满记录 queue-full。
//   - 去重：相同 (Event, Key) 在窗口内只发一次（默认 10 分钟；alert.critical / diag.critical / maint.overdue 为 30 分钟）。Key 为空不去重。
//   - 事件键：change.pending / approved / rejected / started / succeeded / failed / aborted / expired / manual，
//     maint.overdue，alert.critical，diag.critical，test（只能经 Test 发送，不可订阅）。订阅集合为空表示全部。
//
// # 测试专用开关
//
// Options.HTTPClient 与 Options.AllowInsecureForTest 仅供测试（打到 httptest 服务）；生产路径（cmd 启动参数）不要暴露。
package notify
