// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_extra.go
// @time: 2026/10/28

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"tidbops/internal/authz"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
	"tidbops/internal/model"
)

// runRead 通过执行器运行一条只读命令（巡检、状态查询）。读命令不需要令牌，但仍然要通过执行器的白名单校验。
func (s *Server) runRead(ctx context.Context, cmd exec.Cmd) (string, error) {
	var sb strings.Builder
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := s.ops.cfg.Runner.Run(cctx, exec.Request{Cmd: cmd}, func(line string) { sb.WriteString(line + "\n") })
	if err != nil {
		return sb.String(), err
	}
	if res.Exit != 0 {
		msg := res.Err
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.Exit)
		}
		return sb.String(), fmt.Errorf("%s", msg)
	}
	if sb.Len() == 0 {
		sb.WriteString(res.Output)
	}
	return sb.String(), nil
}

func (s *Server) registerOpsExtra(mux *http.ServeMux) {
	o := s.ops
	cluster := s.cfg.Col.Params.Cluster
	s.registerHosts(mux)   // 主机池与预检（host_api.go）
	s.registerVersion(mux) // 版本与升级历史（version_api.go）

	// ---- 巡检
	mux.HandleFunc("POST /api/inspect", s.secured(authz.CapDiag, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			OS bool `json:"os"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
		snap := s.Snapshot(r.Context(), true)
		var extra []model.CheckItem
		if in.OS {
			out, err := s.runRead(r.Context(), exec.Cmd{Kind: exec.KindClusterCheck, Cluster: cluster})
			if err != nil {
				extra = append(extra, model.CheckItem{G: "主机", N: "tiup cluster check", R: "na", D: "执行失败：" + err.Error()})
			} else if items := collect.ParseCheckOutput(out); len(items) > 0 {
				extra = append(extra, items...)
			} else {
				extra = append(extra, model.CheckItem{G: "主机", N: "tiup cluster check", R: "na", D: "没有解析到检查结果"})
			}
		}
		rep := collect.Inspect(snap, s.cfg.Col.Settings(r.Context()), extra)
		o.mu.Lock()
		o.lastInsp = &rep
		o.mu.Unlock()
		s.saveLastInspect(o, &rep)
		s.auditReq(r, ru.U, "inspect.run", cluster, "low", fmt.Sprintf("巡检：通过 %d / 警告 %d / 异常 %d / 未采集 %d（含主机检查=%v）", rep.Ok, rep.Warn, rep.Bad, rep.Na, in.OS), "success")
		writeJSON(w, rep)
	}))
	mux.HandleFunc("GET /api/inspect/last", s.secured(authz.CapDiag, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		o.mu.Lock()
		rep := o.lastInsp
		o.mu.Unlock()
		writeJSON(w, map[string]any{"report": rep})
	}))
	mux.HandleFunc("GET /api/inspect/report", s.secured(authz.CapReport, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		o.mu.Lock()
		rep := o.lastInsp
		o.mu.Unlock()
		if rep == nil {
			writeErr(w, http.StatusNotFound, "还没有巡检结果，请先执行巡检")
			return
		}
		s.auditReq(r, ru.U, "report.export", cluster, "low", "导出巡检报告", "success")
		if r.URL.Query().Get("format") == "html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="inspection.html"`)
			fmt.Fprint(w, reportHTML(*rep))
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="inspection.md"`)
		fmt.Fprint(w, reportMarkdown(*rep))
	}))

	// ---- 只读命令（display / check / PITR 状态 / CDC capture）
	mux.HandleFunc("POST /api/ops/read-cmd", s.secured(authz.CapDiag, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ Cmd string }
		if !decodeBody(w, r, &in) {
			return
		}
		kinds := map[string]string{"display": exec.KindClusterDisplay, "check": exec.KindClusterCheck, "br-status": exec.KindBRLogStatus, "captures": exec.KindCDCCaptures}
		k, ok := kinds[in.Cmd]
		if !ok {
			writeErr(w, http.StatusBadRequest, "不支持的只读命令")
			return
		}
		out, err := s.runRead(r.Context(), exec.Cmd{Kind: k, Cluster: cluster})
		s.auditReq(r, ru.U, "cmd.read", k, "low", "运行只读命令", map[bool]string{true: "success", false: "failed"}[err == nil])
		res := map[string]any{"output": out}
		if err != nil {
			res["error"] = err.Error()
		}
		writeJSON(w, res)
	}))

	// ---- 告警静默（低风险写操作：经执行器与令牌，不走审批）
	mux.HandleFunc("POST /api/alerts/silence", s.secured(authz.CapDiag, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Alertname string `json:"alertname"`
			Instance  string `json:"instance"`
			Minutes   int    `json:"minutes"`
			Comment   string `json:"comment"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if err := s.gate(o); err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		if in.Minutes <= 0 || in.Minutes > 1440 {
			writeErr(w, http.StatusBadRequest, "静默时长需在 1–1440 分钟之间")
			return
		}
		cmd := exec.Cmd{Kind: exec.KindAlertSilence, Cluster: cluster, Key: in.Alertname, Instance: in.Instance, Rate: in.Minutes, Value: strings.TrimSpace(in.Comment + " (by " + ru.U.Name + ")")}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err := o.cfg.Runner.Run(ctx, exec.Request{Cmd: cmd, Actor: ru.U.Name, ChangeID: "silence", Token: exec.Sign(o.cfg.Key, cmd, "silence", o.now().Add(5*time.Minute))}, nil)
		ok := err == nil && res.Exit == 0
		s.auditReq(r, ru.U, "alert.silence", in.Alertname+" "+in.Instance, "low", fmt.Sprintf("静默 %d 分钟", in.Minutes), map[bool]string{true: "success", false: "failed"}[ok])
		if !ok {
			msg := res.Err
			if err != nil {
				msg = err.Error()
			}
			writeErr(w, http.StatusBadGateway, "静默失败："+msg)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

	// ---- 事件时间线：告警 + 变更 + 维护 + 诊断
	mux.HandleFunc("GET /api/timeline", s.secured(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, map[string]any{"events": s.timeline(r.Context())})
	}))
}

// timeline 汇总近期事件。
func (s *Server) timeline(ctx context.Context) []model.TimelineEvent {
	o := s.ops
	var ev []model.TimelineEvent
	snap := s.Snapshot(ctx, false)
	today := o.now().Local()
	for _, a := range snap.Alerts {
		t := today
		if len(a.Time) == 5 {
			var h, m int
			fmt.Sscanf(a.Time, "%d:%d", &h, &m)
			t = time.Date(today.Year(), today.Month(), today.Day(), h, m, 0, 0, today.Location())
			if t.After(today) {
				t = t.AddDate(0, 0, -1)
			}
		}
		lv := "warn"
		if a.Lv == "critical" {
			lv = "bad"
		}
		ev = append(ev, model.TimelineEvent{Time: t, Kind: "alert", Level: lv, Title: a.Name, Detail: a.Inst + " " + a.Desc})
	}
	for _, c := range o.engine.List() {
		lv := "info"
		switch c.State {
		case "failed":
			lv = "bad"
		case "succeeded":
			lv = "ok"
		}
		ev = append(ev, model.TimelineEvent{Time: c.Created, Kind: "change", Level: "info", Title: "变更创建：" + c.Title, Detail: c.Applicant + " · " + c.Risk, Ref: c.ID})
		if c.Finished != nil {
			ev = append(ev, model.TimelineEvent{Time: *c.Finished, Kind: "change", Level: lv, Title: "变更" + stateCN(c.State) + "：" + c.Title, Detail: c.Reason, Ref: c.ID})
		}
	}
	for _, m := range o.engine.Maints() {
		ev = append(ev, model.TimelineEvent{Time: m.Started, Kind: "maint", Level: "info", Title: "主机维护 " + m.IP + "（" + m.TypeLabel + "）", Detail: m.State, Ref: m.ID})
	}
	for _, f := range snap.Findings {
		if f.Sev == "bad" {
			ev = append(ev, model.TimelineEvent{Time: snap.CollectedAt, Kind: "diag", Level: "bad", Title: f.Title, Detail: f.Scope, Ref: f.ID})
		}
	}
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].Time.After(ev[j].Time) })
	if len(ev) > 100 {
		ev = ev[:100]
	}
	return ev
}

func stateCN(s string) string {
	return map[string]string{"succeeded": "成功", "failed": "失败", "aborted": "中止", "rejected": "被驳回", "cancelled": "取消", "expired": "过期"}[s]
}

// ---------------------------------------------------------------- 部署自检 ----

// SelfCheckItem 是启动 / 部署自检的一项。
type SelfCheckItem struct {
	N string `json:"n"`
	R string `json:"r"` // ok | warn | bad
	D string `json:"d"`
}

// selfCheck 检查平台自身的部署安全基线（A3 §3.3、§6）。
func (s *Server) selfCheck() []SelfCheckItem {
	o := s.ops
	var out []SelfCheckItem
	add := func(n, r, d string) { out = append(out, SelfCheckItem{n, r, d}) }
	if runtime.GOOS == "windows" {
		add("运行账号与文件权限", "warn", "Windows 上不检查 POSIX 权限与 root 运行；生产请部署在 Linux 中控机")
	} else {
		if os.Geteuid() == 0 {
			add("不以 root 运行", "bad", "当前以 root 运行：平台被攻破即获得整机权限")
		} else {
			add("不以 root 运行", "ok", fmt.Sprintf("uid=%d", os.Geteuid()))
		}
		for _, f := range []string{"users.json", "audit.log", "changes.json", "settings.json", "roles.json", "master.key", "clusters.json"} {
			p := filepath.Join(o.cfg.Dir, f)
			if st, err := os.Stat(p); err == nil {
				if st.Mode().Perm()&0o077 != 0 {
					add("数据文件权限 "+f, "bad", fmt.Sprintf("权限 %o，应为 0600", st.Mode().Perm()))
				} else {
					add("数据文件权限 "+f, "ok", "0600")
				}
			}
		}
	}
	if n, err := o.cfg.Audit.Verify(); err != nil {
		add("审计哈希链", "bad", err.Error())
	} else {
		add("审计哈希链", "ok", fmt.Sprintf("%d 条记录，链完整", n))
	}
	switch o.cfg.ExecMode {
	case "sudo":
		add("特权分离（Web 进程 + sudo 受限 exec 入口）", "ok", "执行器通过 sudo -u 受限入口调用")
	case "direct":
		add("特权分离（Web 进程 + sudo 受限 exec 入口）", "warn", "Web 进程直接持有执行权限；生产建议使用 --exec sudo")
	case "sim":
		add("特权分离（Web 进程 + sudo 受限 exec 入口）", "warn", "模拟执行器：不会对任何集群做变更")
	default:
		if s.hub != nil && s.hub.Platform() {
			add("特权分离（Web 进程 + sudo 受限 exec 入口）", "ok", "独立部署：平台不在本机执行集群命令，集群为只读接入（没有特权入口可被攻击）")
		} else {
			add("特权分离（Web 进程 + sudo 受限 exec 入口）", "warn", "未配置执行器：平台只读")
		}
	}
	admins := 0
	for _, u := range o.cfg.Users.List() {
		if !u.Disabled && u.HasRole(o.cfg.Env, authz.RoleAdmin) {
			admins++
		}
	}
	if admins >= 2 {
		add("至少两名管理员（写开关双人复核）", "ok", fmt.Sprintf("%d 名", admins))
	} else {
		add("至少两名管理员（写开关双人复核）", "bad", fmt.Sprintf("只有 %d 名管理员，无法完成双人复核开启写操作", admins))
	}
	if o.cfg.NoMFA {
		add("高风险申请人 MFA", "warn", "已通过 --no-mfa 关闭（仅限测试 / POC）")
	} else {
		add("高风险申请人 MFA", "ok", "生产高风险变更需要申请人动态码")
	}
	if o.readOnlyForced() {
		add("READONLY 熔断文件", "warn", "存在 "+o.cfg.ReadOnlyFile+"，平台强制只读")
	} else {
		add("READONLY 熔断文件", "ok", "不存在（可 touch "+o.cfg.ReadOnlyFile+" 紧急只读）")
	}
	add("会话策略", "ok", fmt.Sprintf("空闲 %s / 绝对 %s，Cookie HttpOnly + SameSite=Strict，写接口校验 CSRF 与 Origin", o.cfg.SessionIdle, o.cfg.SessionAbs))
	if v := s.cachedSnapshotVersion(); v != "" {
		if collect.VersionSupported(v) {
			add("集群版本在支持矩阵内", "ok", v)
		} else {
			add("集群版本在支持矩阵内", "warn", v+"：写操作已降级为只读")
		}
	}
	prod := o.cfg.Env == "prod"
	if prod && o.cfg.Sim {
		add("生产环境使用真实执行器", "bad", "生产环境不应使用模拟执行器")
	}
	return out
}

func reportMarkdown(rep model.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# TiDB 集群巡检报告\n\n- 集群：%s（%s）\n- 时间：%s\n- 数据来源：%s\n- 结果：通过 %d · 警告 %d · 异常 %d · 未采集 %d\n\n", rep.Cluster, rep.Version, rep.Time.Local().Format("2006-01-02 15:04:05"), rep.Mode, rep.Ok, rep.Warn, rep.Bad, rep.Na)
	g := ""
	for _, it := range rep.Items {
		if it.G != g {
			g = it.G
			fmt.Fprintf(&b, "\n## %s\n\n| 结果 | 检查项 | 说明 |\n|---|---|---|\n", g)
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", map[string]string{"ok": "通过", "warn": "警告", "bad": "异常", "na": "未采集"}[it.R], it.N, strings.ReplaceAll(it.D, "|", "\\|"))
	}
	return b.String()
}

func reportHTML(rep model.Report) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>TiDB 集群巡检报告</title><style>body{font:14px/1.6 system-ui,sans-serif;margin:32px;color:#222}table{border-collapse:collapse;width:100%;margin:8px 0 20px}td,th{border:1px solid #ddd;padding:6px 10px;text-align:left}th{background:#f5f5f5}.ok{color:#1a7f37}.warn{color:#9a6700}.bad{color:#cf222e}.na{color:#888}</style></head><body>`)
	fmt.Fprintf(&b, "<h1>TiDB 集群巡检报告</h1><p>集群：%s（%s）<br>时间：%s<br>数据来源：%s<br>结果：通过 %d · 警告 %d · 异常 %d · 未采集 %d</p>", html.EscapeString(rep.Cluster), html.EscapeString(rep.Version), rep.Time.Local().Format("2006-01-02 15:04:05"), html.EscapeString(rep.Mode), rep.Ok, rep.Warn, rep.Bad, rep.Na)
	g := ""
	for _, it := range rep.Items {
		if it.G != g {
			if g != "" {
				b.WriteString("</table>")
			}
			g = it.G
			fmt.Fprintf(&b, "<h2>%s</h2><table><tr><th>结果</th><th>检查项</th><th>说明</th></tr>", html.EscapeString(g))
		}
		fmt.Fprintf(&b, `<tr><td class="%s">%s</td><td>%s</td><td>%s</td></tr>`, it.R, map[string]string{"ok": "通过", "warn": "警告", "bad": "异常", "na": "未采集"}[it.R], html.EscapeString(it.N), html.EscapeString(it.D))
	}
	b.WriteString("</table></body></html>")
	return b.String()
}
