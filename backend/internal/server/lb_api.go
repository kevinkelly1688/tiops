// @author: kevinkelly
// @license: Apache License 2.0
// @file: lb_api.go
// @time: 2026/10/28

package server

// 负载均衡层（每集群各自一份，按 X-Cluster 路由）：
//
//	GET  /api/lb                  任何有 view.overview 的登录用户：手工登记 + 自动发现的 TiProxy + 生效判定 + 一致性提示（不触发采集）
//	POST /api/ops/lb              新增登记（settings.edit，即内置管理员）
//	POST /api/ops/lb/{id}         更新登记（settings.edit）；id 不可改，未给出的字段不修改
//	POST /api/ops/lb/{id}/delete  删除登记（settings.edit）
//
// 登记簿属于平台元数据，不是集群变更：不要求写操作开关打开，也不走审批。写操作需要 CSRF，并写审计 lb.add / lb.update / lb.delete
// （风险 mid；摘要只写变更的字段名与入口地址，不写备注原文）。持久化在每集群的 settings.json（Settings.LBs）。

import (
	"fmt"
	"net/http"
	"strings"

	"tidbops/internal/authz"
	"tidbops/internal/lb"
	"tidbops/internal/model"
)

// lbEntryView 是 GET /api/lb 里手工登记的展示形式：在 lb.Entry 之上加 source。
type lbEntryView struct {
	lb.Entry
	Source string `json:"source"` // manual
}

// lbProxyView 是自动发现的 TiProxy 的展示形式：在 model.Proxy 之上加 source。
type lbProxyView struct {
	model.Proxy
	Source string `json:"source"` // auto
}

type lbEffective struct {
	HasLB  bool   `json:"hasLB"`
	Reason string `json:"reason"` // legacy-flag（设置页的复选框）| manual（有手工登记）| none
}

// effectiveLBLocked 是「集群前有负载均衡」的有效值（调用方须持有 o.mu）：设置里的旧开关 || 有手工登记。
// 自动发现的 TiProxy 不参与：其后端发现与优雅迁移行为尚未在真实环境验证，保守起见仍由开关 / 手工登记决定。
func (o *ops) effectiveLBLocked() bool { return o.settings.LB || len(o.settings.LBs) > 0 }

func (o *ops) lbEffectiveLocked() lbEffective {
	switch {
	case o.settings.LB:
		return lbEffective{true, "legacy-flag"}
	case len(o.settings.LBs) > 0:
		return lbEffective{true, "manual"}
	}
	return lbEffective{false, "none"}
}

func writeLBErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONCode(w, status, apiError{Error: msg, Code: code})
}

// tidbSQLAddrs 取拓扑里 TiDB 实例的 host:port（SQL 端口）。
func tidbSQLAddrs(t model.Topology) []string {
	var out []string
	for _, in := range t.Instances {
		if in.Type == "tidb" {
			out = append(out, fmt.Sprintf("%s:%d", in.Host, in.Port))
		}
	}
	return out
}

func (s *Server) registerLB(mux *http.ServeMux) {
	o := s.ops
	mux.HandleFunc("GET /api/lb", s.secured(authz.CapViewOverview, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		o.mu.Lock()
		entries := append([]lb.Entry(nil), o.settings.LBs...)
		eff := o.lbEffectiveLocked()
		o.mu.Unlock()
		resp := struct {
			Entries   []lbEntryView `json:"entries"`
			Proxies   []lbProxyView `json:"proxies"`
			Effective lbEffective   `json:"effective"`
			Check     []lb.Check    `json:"check"`
		}{Entries: []lbEntryView{}, Proxies: []lbProxyView{}, Effective: eff, Check: []lb.Check{}}
		for _, e := range entries {
			if e.Backends == nil {
				e.Backends = []string{}
			}
			resp.Entries = append(resp.Entries, lbEntryView{e, "manual"})
		}
		if snap, ok := s.cachedSnapshot(); ok { // 只读缓存快照，不触发采集；还没有快照时 proxies / check 为空
			for _, p := range snap.Topology.Proxies {
				resp.Proxies = append(resp.Proxies, lbProxyView{p, "auto"})
			}
			resp.Check = lb.Compare(entries, tidbSQLAddrs(snap.Topology))
		}
		writeJSON(w, resp)
	}))

	mux.HandleFunc("POST /api/ops/lb", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in lb.Input
		if !decodeBody(w, r, &in) {
			return
		}
		fail := func(status int, code, msg string) {
			s.auditReq(r, ru.U, "lb.add", clip(in.ID), "mid", "新增负载均衡登记失败："+code, "failed")
			writeLBErr(w, status, code, msg)
		}
		if !lb.ValidID(in.ID) {
			fail(http.StatusBadRequest, "invalid", "id 需匹配 ^[a-z][a-z0-9-]{1,31}$（小写字母开头，2–32 位小写字母 / 数字 / 连字符）")
			return
		}
		f, _, err := lb.Apply(lb.Fields{}, in, true)
		if err != nil {
			fail(http.StatusBadRequest, "invalid", err.Error())
			return
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		for _, e := range o.settings.LBs {
			if e.ID == in.ID {
				fail(http.StatusConflict, "exists", "该 id 已登记："+in.ID)
				return
			}
		}
		if len(o.settings.LBs) >= lb.MaxEntries {
			fail(http.StatusConflict, "limit", fmt.Sprintf("每个集群最多登记 %d 条负载均衡", lb.MaxEntries))
			return
		}
		now := o.now()
		e := lb.Entry{ID: in.ID, Type: f.Type, Name: f.Name, Addr: f.Addr, Backends: f.Backends, Note: f.Note, Created: now, Updated: now, UpdatedBy: ru.U.Name}
		prev := o.settings.LBs
		o.settings.LBs = append(append([]lb.Entry(nil), prev...), e)
		if err := s.saveSettingsErr(o); err != nil {
			o.settings.LBs = prev
			s.cfg.Logf("保存负载均衡登记失败：%v", err)
			fail(http.StatusInternalServerError, "store", "保存失败："+err.Error())
			return
		}
		s.auditReq(r, ru.U, "lb.add", e.ID, "mid", fmt.Sprintf("新增负载均衡登记 %s（%s），入口 %s", e.Name, lb.TypeLabel(e.Type), e.Addr), "success")
		writeJSON(w, lbEntryView{e, "manual"})
	}))

	mux.HandleFunc("POST /api/ops/lb/{id}", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		var in lb.Input
		if !decodeBody(w, r, &in) {
			return
		}
		fail := func(status int, code, msg string) {
			s.auditReq(r, ru.U, "lb.update", clip(id), "mid", "修改负载均衡登记失败："+code, "failed")
			writeLBErr(w, status, code, msg)
		}
		if in.ID != "" && in.ID != id {
			fail(http.StatusBadRequest, "invalid", "id 不可修改")
			return
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		idx := -1
		for i, e := range o.settings.LBs {
			if e.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			fail(http.StatusNotFound, "not_found", "找不到该负载均衡登记："+id)
			return
		}
		old := o.settings.LBs[idx]
		f, changed, err := lb.Apply(lb.Fields{Type: old.Type, Name: old.Name, Addr: old.Addr, Note: old.Note, Backends: old.Backends}, in, false)
		if err != nil {
			fail(http.StatusBadRequest, "invalid", err.Error())
			return
		}
		e := old
		e.Type, e.Name, e.Addr, e.Backends, e.Note = f.Type, f.Name, f.Addr, f.Backends, f.Note
		e.Updated, e.UpdatedBy = o.now(), ru.U.Name
		prev := o.settings.LBs
		next := append([]lb.Entry(nil), prev...)
		next[idx] = e
		o.settings.LBs = next
		if err := s.saveSettingsErr(o); err != nil {
			o.settings.LBs = prev
			s.cfg.Logf("保存负载均衡登记失败：%v", err)
			fail(http.StatusInternalServerError, "store", "保存失败："+err.Error())
			return
		}
		what := "无字段变化"
		if len(changed) > 0 {
			what = "修改字段 " + strings.Join(changed, ",")
		}
		s.auditReq(r, ru.U, "lb.update", id, "mid", fmt.Sprintf("修改负载均衡登记 %s：%s，入口 %s", e.Name, what, e.Addr), "success")
		writeJSON(w, lbEntryView{e, "manual"})
	}))

	mux.HandleFunc("POST /api/ops/lb/{id}/delete", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		id := r.PathValue("id")
		fail := func(status int, code, msg string) {
			s.auditReq(r, ru.U, "lb.delete", clip(id), "mid", "删除负载均衡登记失败："+code, "failed")
			writeLBErr(w, status, code, msg)
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		idx := -1
		for i, e := range o.settings.LBs {
			if e.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			fail(http.StatusNotFound, "not_found", "找不到该负载均衡登记："+id)
			return
		}
		old := o.settings.LBs[idx]
		prev := o.settings.LBs
		next := append(append([]lb.Entry(nil), prev[:idx]...), prev[idx+1:]...)
		o.settings.LBs = next
		if err := s.saveSettingsErr(o); err != nil {
			o.settings.LBs = prev
			s.cfg.Logf("保存负载均衡登记失败：%v", err)
			fail(http.StatusInternalServerError, "store", "保存失败："+err.Error())
			return
		}
		s.auditReq(r, ru.U, "lb.delete", id, "mid", fmt.Sprintf("删除负载均衡登记 %s，入口 %s", old.Name, old.Addr), "success")
		writeJSON(w, map[string]any{"ok": true})
	}))
}
