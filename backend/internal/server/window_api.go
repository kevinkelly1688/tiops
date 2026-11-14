// @author: kevinkelly
// @license: Apache License 2.0
// @file: window_api.go
// @time: 2026/10/28

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"tidbops/internal/change"
)

// windowsLocked 返回生效的窗口规则（调用方须持有 o.mu）：settings.json 里没有 windows 字段时用默认规则。
func (o *ops) windowsLocked() []change.Window {
	if o.settings.Windows == nil {
		return change.DefaultWindows()
	}
	out := make([]change.Window, len(o.settings.Windows)) // 非 nil：空数组要序列化成 []
	copy(out, o.settings.Windows)
	return out
}

// effectiveWindows 供引擎读取生效的窗口规则。
func (o *ops) effectiveWindows() []change.Window {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.windowsLocked()
}

// registerWindows 注册变更窗口规则与推荐窗口接口。
// 规则的读写走 GET/POST /api/ops/settings（见 ops_api.go）；预检里的窗口检查走 POST /api/changes/preview。
func (s *Server) registerWindows(mux *http.ServeMux) {
	o := s.ops
	// 推荐窗口：按变更计划的预计时长，在未来若干天里找出规则全通过且得分最高的起点。
	mux.HandleFunc("POST /api/changes/recommend", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct {
			Kind   string          `json:"kind"`
			Params json.RawMessage `json:"params"`
			From   string          `json:"from"`
			Days   int             `json:"days"`
			Count  int             `json:"count"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		c := kindCap(in.Kind)
		if c == "" {
			writeErr(w, http.StatusBadRequest, "未知的变更类型："+in.Kind)
			return
		}
		if !ru.U.Can(o.cfg.Env, c) {
			writeErr(w, http.StatusForbidden, "没有权限："+string(c))
			return
		}
		var from time.Time
		if in.From != "" {
			t, err := time.Parse(time.RFC3339, in.From)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "from 需为 RFC3339 时间")
				return
			}
			from = t
		}
		if in.Days <= 0 {
			in.Days = 7
		}
		if in.Days > 14 {
			in.Days = 14
		}
		if in.Count <= 0 {
			in.Count = 5
		}
		if in.Count > 10 {
			in.Count = 10
		}
		plan, slots, err := o.engine.RecommendPlan(r.Context(), in.Kind, in.Params, from, in.Days, in.Count)
		if err != nil {
			var be *change.BlockedError
			if errors.As(err, &be) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "code": "blocked", "plan": be.Plan})
				return
			}
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if slots == nil {
			slots = []change.Slot{}
		}
		writeJSON(w, map[string]any{
			"plan":  map[string]any{"title": plan.Title, "risk": plan.Risk, "estMins": plan.EstMins},
			"slots": slots,
		})
	}))
}
