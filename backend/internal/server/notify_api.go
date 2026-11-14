// @author: kevinkelly
// @license: Apache License 2.0
// @file: notify_api.go
// @time: 2026/10/28

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tidbops/internal/authz"
	"tidbops/internal/change"
	"tidbops/internal/model"
	"tidbops/internal/notify"
)

// notifyWatchEvery 是告警 / 诊断监视器的轮询间隔。
const notifyWatchEvery = 60 * time.Second

// maxWatchMsgs 限制每轮最多逐条发送的新告警 / 诊断条数，超出的合并成一条汇总，避免刷屏。
const maxWatchMsgs = 8

// notifyWatch 保存告警 / 诊断监视器的状态。
type notifyWatch struct {
	mu        sync.Mutex
	baselined bool
	alerts    map[string]bool
	diags     map[string]bool
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
}

// initNotify 创建通知器。渠道文件放在 <Dir>/notify.json（Dir 为空则只放内存）。
func (s *Server) initNotify(o *ops) {
	opts := notify.Options{
		MasterKey:            o.cfg.MasterKey,
		ExtraHosts:           o.cfg.NotifyHosts,
		Env:                  o.cfg.Env,
		Cluster:              s.cfg.Col.Params.Cluster,
		AllowInsecureForTest: o.cfg.NotifyInsecureTest,
		Now:                  o.cfg.Now,
		Logf:                 s.cfg.Logf,
	}
	if o.cfg.Dir != "" {
		opts.File = filepath.Join(o.cfg.Dir, "notify.json")
	}
	o.notifier = notify.New(opts)
	if err := o.notifier.Err(); err != nil {
		s.cfg.Logf("通知渠道文件无法读取，通知渠道已进入只读状态：%v", err)
	}
}

// startNotifyWatch 启动告警 / 诊断监视器（NotifyWatchEvery<0 时不启动，测试里手动调用 watchOnce）。
func (s *Server) startNotifyWatch(o *ops) {
	o.nw.stop, o.nw.done = make(chan struct{}), make(chan struct{})
	if o.cfg.NotifyWatchEvery < 0 {
		close(o.nw.done)
		return
	}
	every := o.cfg.NotifyWatchEvery
	if every == 0 {
		every = notifyWatchEvery
	}
	go func() {
		defer close(o.nw.done)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-o.nw.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		defer func() {
			if r := recover(); r != nil {
				s.cfg.Logf("通知监视器异常: %v", r)
			}
		}()
		s.watchOnce(o, s.Snapshot(ctx, false)) // 第一次只记录基线
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-o.nw.stop:
				return
			case <-t.C:
				s.watchOnce(o, s.Snapshot(ctx, false))
			}
		}
	}()
}

// stopNotifyWatch 停止监视器并关闭通知器（等待队列排空）。必须在引擎关闭之后调用，才能送出平台停止时的失败通知。
func (s *Server) stopNotifyWatch(o *ops) {
	o.nw.stopOnce.Do(func() {
		if o.nw.stop != nil {
			close(o.nw.stop)
			<-o.nw.done
		}
	})
}

// watchOnce 用一份快照比较上一轮：新出现的 critical 告警（key=name+instance）发 alert.critical，
// 新出现的 sev 为 bad / critical 的诊断发现（key=Finding.ID）发 diag.critical。
// 第一次调用只记录基线，不发送；消失（恢复）暂不通知。
func (s *Server) watchOnce(o *ops, snap model.Snapshot) {
	if o.notifier == nil {
		return
	}
	if len(snap.Errors) > 0 && len(snap.Alerts) == 0 && len(snap.Findings) == 0 {
		return // 采集失败时的空结果不当作「全部恢复」，也不更新基线
	}
	alerts, diags := map[string]bool{}, map[string]bool{}
	var newAlerts []model.Alert
	var newDiags []model.Finding
	o.nw.mu.Lock()
	for _, a := range snap.Alerts {
		if a.Lv != "critical" {
			continue
		}
		k := a.Name + "|" + a.Inst
		if alerts[k] {
			continue
		}
		alerts[k] = true
		if o.nw.baselined && !o.nw.alerts[k] {
			newAlerts = append(newAlerts, a)
		}
	}
	for _, f := range snap.Findings {
		if f.Sev != "bad" && f.Sev != "critical" || diags[f.ID] {
			continue
		}
		diags[f.ID] = true
		if o.nw.baselined && !o.nw.diags[f.ID] {
			newDiags = append(newDiags, f)
		}
	}
	o.nw.baselined, o.nw.alerts, o.nw.diags = true, alerts, diags
	o.nw.mu.Unlock()

	for i, a := range newAlerts {
		if i >= maxWatchMsgs {
			s.notifySummary(o, notify.EvAlertCritical, "新出现严重告警", len(newAlerts)-maxWatchMsgs, newAlerts[maxWatchMsgs].Name+"|"+newAlerts[maxWatchMsgs].Inst, "#overview")
			break
		}
		lines := []string{"告警：" + a.Name}
		if a.Inst != "" {
			lines = append(lines, "实例："+a.Inst)
		}
		if a.Desc != "" {
			lines = append(lines, "说明："+a.Desc)
		}
		if a.Time != "" {
			lines = append(lines, "触发："+a.Time)
		}
		s.notifyMsg(o, notify.Message{Event: notify.EvAlertCritical, Title: "新出现严重告警：" + a.Name, Lines: lines, Level: notify.LvCritical, Key: a.Name + "|" + a.Inst, Link: s.notifyLink(o, "#overview")})
	}
	for i, f := range newDiags {
		if i >= maxWatchMsgs {
			s.notifySummary(o, notify.EvDiagCritical, "新出现严重诊断发现", len(newDiags)-maxWatchMsgs, newDiags[maxWatchMsgs].ID, "#diag")
			break
		}
		lines := []string{"发现：" + f.Title}
		if f.Scope != "" {
			lines = append(lines, "范围："+f.Scope)
		}
		if f.Cause != "" {
			lines = append(lines, "可能原因："+f.Cause) // 不附带证据（可能含 SQL / 业务数据）
		}
		s.notifyMsg(o, notify.Message{Event: notify.EvDiagCritical, Title: "新出现严重诊断发现：" + f.Title, Lines: lines, Level: notify.LvCritical, Key: f.ID, Link: s.notifyLink(o, "#diag")})
	}
}

func (s *Server) notifySummary(o *ops, ev, title string, more int, key, anchor string) {
	s.notifyMsg(o, notify.Message{Event: ev, Title: title, Lines: []string{fmt.Sprintf("本轮还有 %d 条同类新增，请登录平台查看", more)}, Level: notify.LvCritical, Key: "summary|" + key, Link: s.notifyLink(o, anchor)})
}

// notifyMsg 补全环境 / 集群后交给通知器（异步）。
func (s *Server) notifyMsg(o *ops, m notify.Message) {
	if o.notifier == nil {
		return
	}
	m.Env, m.Cluster = o.cfg.Env, s.cfg.Col.Params.Cluster
	if m.Time.IsZero() {
		m.Time = o.now()
	}
	o.notifier.Notify(m)
}

// notifyLink 拼出平台页面链接；PublicURL 为空则不加链接。
func (s *Server) notifyLink(o *ops, anchor string) string {
	base := strings.TrimSpace(o.cfg.PublicURL)
	if base == "" {
		return ""
	}
	if s.hub != nil && s.clusterID != "" && s.clusterID != s.hub.primaryID {
		return strings.TrimRight(base, "/") + "/?cluster=" + s.clusterID + anchor // 多集群：非主集群的链接带上集群 id（前端据此选择集群）
	}
	return strings.TrimRight(base, "/") + "/" + anchor
}

func riskLabel(r string) string {
	switch r {
	case "low":
		return "低"
	case "mid":
		return "中"
	case "high":
		return "高"
	}
	return r
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
}

// notifyEngineEvent 把引擎事件转成通知。step / command / cancelled 不发。
// 消息只含变更标题、ID、风险、申请人 / 审批人、原因与时间；不含命令行、命令输出、凭据与 SQL。
func (s *Server) notifyEngineEvent(o *ops, ev change.Event) {
	if o.notifier == nil {
		return
	}
	m := notify.Message{Time: ev.Time, Link: s.notifyLink(o, "#schedule")}
	var ch change.Change
	haveCh := false
	if ev.ChangeID != "" && o.engine != nil {
		ch, haveCh = o.engine.Get(ev.ChangeID)
	}
	lines := func(extra ...string) []string {
		var ls []string
		if haveCh {
			ls = append(ls, fmt.Sprintf("变更：%s（%s）", ch.Title, ch.ID), "风险："+riskLabel(ch.Risk))
			if ch.Emergency {
				ls = append(ls, "类型：紧急变更")
			}
		} else if ev.ChangeID != "" {
			ls = append(ls, "变更："+ev.ChangeID, "风险："+riskLabel(ev.Risk))
		}
		return append(ls, extra...)
	}
	key := ev.ChangeID
	switch ev.Type {
	case "created":
		if !haveCh || ch.Need <= 0 {
			return // 免审批的变更不发 pending
		}
		m.Event, m.Title, m.Level = notify.EvPending, "有变更等待审批", notify.LvWarning
		m.Lines = lines("申请人："+ch.Applicant, fmt.Sprintf("需要审批：%d 级", ch.Need))
	case "approved":
		m.Event, m.Level = notify.EvApproved, notify.LvInfo
		m.Title = "变更审批通过"
		if haveCh && len(ch.Approvals) < ch.Need {
			m.Title = "变更已通过一级审批，等待下一级"
		}
		m.Lines = lines("审批人："+ev.User, "审批进度："+ev.Detail)
		if haveCh {
			m.Lines = append(m.Lines, "申请人："+ch.Applicant)
		}
		key += "|" + ev.Detail
	case "rejected":
		m.Event, m.Title, m.Level = notify.EvRejected, "变更被驳回", notify.LvWarning
		m.Lines = lines("驳回人：" + ev.User)
		if ev.Detail != "" {
			m.Lines = append(m.Lines, "原因："+ev.Detail)
		}
	case "started":
		m.Event, m.Title, m.Level = notify.EvStarted, "变更开始执行", notify.LvInfo
		m.Lines = lines()
		if haveCh {
			m.Lines = append(m.Lines, "申请人："+ch.Applicant)
		}
	case "succeeded":
		m.Event, m.Title, m.Level = notify.EvSucceeded, "变更执行成功", notify.LvSuccess
		var ex []string
		if haveCh && ch.Started != nil && ch.Finished != nil {
			ex = append(ex, "耗时："+shortDur(ch.Finished.Sub(*ch.Started)))
		}
		m.Lines = lines(ex...)
	case "failed":
		m.Event, m.Title, m.Level = notify.EvFailed, "变更执行失败", notify.LvCritical
		var ex []string
		if haveCh {
			for _, st := range ch.Steps {
				if st.State == "failed" {
					ex = append(ex, "失败步骤："+st.Name)
					break
				}
			}
		}
		if ev.Detail != "" {
			ex = append(ex, "原因："+ev.Detail) // 引擎的失败摘要；Notifier 会再截断并掩码
		}
		m.Lines = lines(ex...)
	case "aborted":
		m.Event, m.Title, m.Level = notify.EvAborted, "变更被中止", notify.LvWarning
		m.Lines = lines("中止人：" + ev.User)
	case "expired":
		m.Event, m.Title, m.Level = notify.EvExpired, "变更已过期（超过有效期仍未批准 / 执行）", notify.LvWarning
		m.Lines = lines()
	case "manual-wait":
		m.Event, m.Title, m.Level = notify.EvManual, "变更等待人工确认", notify.LvWarning
		m.Lines = lines()
		if ev.Detail != "" {
			m.Lines = append(m.Lines, "需要人工操作："+ev.Detail)
		}
		key += "|" + ev.Detail
	case "maint-overdue":
		m.Event, m.Title, m.Level = notify.EvMaintOverdue, "主机维护超过 PD 阈值", notify.LvCritical
		m.Lines = []string{ev.Detail}
		m.Link = s.notifyLink(o, "#nodes")
		key = ev.Detail
	default:
		return
	}
	m.Key = key
	s.notifyMsg(o, m)
}

// registerNotify 注册通知渠道（企业微信 / 钉钉 / Lark Webhook）相关接口。管理与查看都需要 settings.edit。
func (s *Server) registerNotify(mux *http.ServeMux) {
	o := s.ops
	n := o.notifier
	mux.HandleFunc("GET /api/ops/notify", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, map[string]any{
			"channels":     n.Channels(),
			"log":          n.Log(50),
			"types":        notify.Types(),
			"events":       notify.Events(),
			"allowedHosts": n.AllowedHosts(),
			"readOnly":     n.Err() != nil,
		})
	}))
	mux.HandleFunc("POST /api/ops/notify/channels", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in notify.ChannelInput
		if !decodeBody(w, r, &in) {
			return
		}
		in.ID = ""
		v, err := n.Upsert(in)
		if err != nil {
			s.auditReq(r, ru.U, "notify.channel.add", "", "", fmt.Sprintf("新增通知渠道「%s」（%s）失败", clip(in.Name), in.Type), "failed")
			s.notifyErr(w, err)
			return
		}
		s.auditReq(r, ru.U, "notify.channel.add", v.ID, "", fmt.Sprintf("新增通知渠道「%s」（%s）", v.Name, v.Type), "success")
		writeJSON(w, v)
	}))
	mux.HandleFunc("POST /api/ops/notify/channels/{id}", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in notify.ChannelInput
		if !decodeBody(w, r, &in) {
			return
		}
		in.ID = r.PathValue("id")
		v, err := n.Upsert(in)
		if err != nil {
			s.auditReq(r, ru.U, "notify.channel.update", in.ID, "", fmt.Sprintf("修改通知渠道「%s」失败", clip(in.Name)), "failed")
			s.notifyErr(w, err)
			return
		}
		sum := fmt.Sprintf("修改通知渠道「%s」（%s）", v.Name, v.Type)
		if strings.TrimSpace(in.URL) != "" {
			sum += "，更换了 Webhook 地址"
		}
		if in.Secret != "" || in.ClearSecret {
			sum += "，更换了签名密钥"
		}
		s.auditReq(r, ru.U, "notify.channel.update", v.ID, "", sum, "success")
		writeJSON(w, v)
	}))
	mux.HandleFunc("POST /api/ops/notify/channels/{id}/enabled", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		id := r.PathValue("id")
		old, _ := n.View(id)
		if err := n.SetEnabled(id, in.Enabled); err != nil {
			s.auditReq(r, ru.U, "notify.channel.update", id, "", "启停通知渠道失败", "failed")
			s.notifyErr(w, err)
			return
		}
		state := "停用"
		if in.Enabled {
			state = "启用"
		}
		s.auditReq(r, ru.U, "notify.channel.update", id, "", fmt.Sprintf("%s通知渠道「%s」（%s）", state, old.Name, old.Type), "success")
		v, _ := n.View(id)
		writeJSON(w, v)
	}))
	mux.HandleFunc("POST /api/ops/notify/channels/{id}/delete", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		old, _ := n.View(id)
		if err := n.Delete(id); err != nil {
			s.auditReq(r, ru.U, "notify.channel.delete", id, "", "删除通知渠道失败", "failed")
			s.notifyErr(w, err)
			return
		}
		s.auditReq(r, ru.U, "notify.channel.delete", id, "", fmt.Sprintf("删除通知渠道「%s」（%s）", old.Name, old.Type), "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/ops/notify/channels/{id}/test", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		v, ok := n.View(id)
		if !ok {
			s.notifyErr(w, notify.ErrNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		err := n.Test(ctx, id)
		res, msg := "success", ""
		if err != nil {
			res, msg = "failed", err.Error()
		}
		s.auditReq(r, ru.U, "notify.channel.test", id, "", fmt.Sprintf("测试通知渠道「%s」（%s）", v.Name, v.Type), res)
		writeJSON(w, map[string]any{"ok": err == nil, "error": msg})
	}))
}

func clip(s string) string {
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

func (s *Server) notifyErr(w http.ResponseWriter, err error) {
	var ie *notify.InvalidError
	switch {
	case errors.Is(err, notify.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &ie):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}
