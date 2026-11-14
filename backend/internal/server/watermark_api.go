// @author: kevinkelly
// @license: Apache License 2.0
// @file: watermark_api.go
// @time: 2026/10/28

package server

// 页面水印（平台全局，由 Hub 路由到主实例）：
//
//	GET  /api/watermark  任何已登录用户：读取当前水印设置（前端据此在页面上叠加水印）
//	POST /api/watermark  settings.edit（即内置管理员）：整体保存；写审计 watermark.update（风险 mid）
//
// 水印是平台自己的显示设置，不是对集群的变更：不要求写操作开关打开，也不走审批。默认关闭。
// 持久化在 <数据目录>/watermark.json（数据目录为空则只在内存）。文字模板里可用的变量由前端替换：
// {user} 登录名、{display} 显示名、{env} 环境、{date} 日期、{time} 时间。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"tidbops/internal/authz"
)

// Watermark 是水印设置。
type Watermark struct {
	Enabled   bool      `json:"enabled"`
	Text      string    `json:"text"`     // 文字模板，可含 {user} {display} {env} {date} {time}
	Opacity   float64   `json:"opacity"`  // 0.02–0.5
	FontSize  int       `json:"fontSize"` // 10–48（px）
	Angle     int       `json:"angle"`    // -90–90（度，负数为逆时针）
	Color     string    `json:"color"`    // #rrggbb
	GapX      int       `json:"gapX"`     // 水平间距 40–400（px）
	GapY      int       `json:"gapY"`     // 垂直间距 40–400（px）
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

const (
	wmMaxTextRunes = 100
	wmDefaultText  = "{display} {user} {date}"
)

var (
	wmColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	wmVarRe   = regexp.MustCompile(`\{[^{}]*\}`)
	wmVars    = map[string]bool{"{user}": true, "{display}": true, "{env}": true, "{date}": true, "{time}": true}
)

func defaultWatermark() Watermark {
	return Watermark{Enabled: false, Text: wmDefaultText, Opacity: 0.12, FontSize: 16, Angle: -20, Color: "#808080", GapX: 120, GapY: 100}
}

// wmValidate 校验并规整（text 去首尾空白、color 转小写）；返回面向管理员的错误说明。
func wmValidate(w Watermark) (Watermark, string) {
	w.Text = strings.TrimSpace(w.Text)
	if n := len([]rune(w.Text)); n > wmMaxTextRunes {
		return w, fmt.Sprintf("水印文字最多 %d 个字", wmMaxTextRunes)
	}
	for _, r := range w.Text {
		if unicode.IsControl(r) {
			return w, "水印文字不能含换行或控制字符"
		}
	}
	for _, v := range wmVarRe.FindAllString(w.Text, -1) {
		if !wmVars[v] {
			return w, "不支持的变量 " + v + "，可用：{user} {display} {env} {date} {time}"
		}
	}
	if w.Enabled && w.Text == "" {
		return w, "启用水印时文字不能为空"
	}
	if w.Opacity < 0.02 || w.Opacity > 0.5 {
		return w, "透明度须在 0.02–0.5 之间"
	}
	if w.FontSize < 10 || w.FontSize > 48 {
		return w, "字号须在 10–48 之间"
	}
	if w.Angle < -90 || w.Angle > 90 {
		return w, "旋转角度须在 -90–90 之间"
	}
	if !wmColorRe.MatchString(w.Color) {
		return w, "颜色须为 #rrggbb 格式"
	}
	w.Color = strings.ToLower(w.Color)
	if w.GapX < 40 || w.GapX > 400 || w.GapY < 40 || w.GapY > 400 {
		return w, "水平 / 垂直间距须在 40–400 之间"
	}
	return w, ""
}

// wmStore 是水印设置的内存副本 + 落盘。
type wmStore struct {
	mu   sync.RWMutex
	path string // 空 = 仅内存
	cur  Watermark
}

func openWMStore(dir string) *wmStore {
	st := &wmStore{cur: defaultWatermark()}
	if dir == "" {
		return st
	}
	st.path = filepath.Join(dir, "watermark.json")
	b, err := os.ReadFile(st.path)
	if err != nil {
		return st // 不存在 = 默认（关闭）
	}
	var w Watermark
	if json.Unmarshal(b, &w) != nil {
		return st // 文件损坏时保持默认，下次保存会覆盖
	}
	if v, bad := wmValidate(w); bad == "" {
		st.cur = v
	}
	return st
}

func (s *wmStore) get() Watermark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

func (s *wmStore) set(w Watermark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		b, err := json.MarshalIndent(w, "", "  ")
		if err != nil {
			return err
		}
		tmp := s.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, s.path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	s.cur = w
	return nil
}

// wmDiff 列出变更了哪些字段（审计摘要里不写文字原文以外的敏感信息；水印文字本身不敏感，写入便于追溯）。
func wmDiff(a, b Watermark) []string {
	var out []string
	if a.Enabled != b.Enabled {
		out = append(out, fmt.Sprintf("启用 %v→%v", a.Enabled, b.Enabled))
	}
	if a.Text != b.Text {
		out = append(out, fmt.Sprintf("文字「%s」→「%s」", a.Text, b.Text))
	}
	if a.Opacity != b.Opacity {
		out = append(out, fmt.Sprintf("透明度 %g→%g", a.Opacity, b.Opacity))
	}
	if a.FontSize != b.FontSize {
		out = append(out, fmt.Sprintf("字号 %d→%d", a.FontSize, b.FontSize))
	}
	if a.Angle != b.Angle {
		out = append(out, fmt.Sprintf("角度 %d→%d", a.Angle, b.Angle))
	}
	if a.Color != b.Color {
		out = append(out, fmt.Sprintf("颜色 %s→%s", a.Color, b.Color))
	}
	if a.GapX != b.GapX || a.GapY != b.GapY {
		out = append(out, fmt.Sprintf("间距 %dx%d→%dx%d", a.GapX, a.GapY, b.GapX, b.GapY))
	}
	return out
}

func (s *Server) registerWatermark(mux *http.ServeMux) {
	o := s.ops
	st := openWMStore(o.cfg.Dir)

	mux.HandleFunc("GET /api/watermark", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, map[string]any{"watermark": st.get(), "canManage": ru.U.Can(o.cfg.Env, authz.CapSettings),
			"limits":   map[string]any{"maxText": wmMaxTextRunes, "vars": []string{"{user}", "{display}", "{env}", "{date}", "{time}"}},
			"defaults": defaultWatermark()})
	}))

	mux.HandleFunc("POST /api/watermark", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in Watermark
		if !decodeBody(w, r, &in) {
			return
		}
		next, bad := wmValidate(in)
		if bad != "" {
			writeJSONCode(w, http.StatusBadRequest, apiError{Error: bad, Code: "invalid"})
			return
		}
		next.UpdatedBy, next.UpdatedAt = ru.U.Name, o.now()
		old := st.get()
		if err := st.set(next); err != nil {
			s.auditReq(r, ru.U, "watermark.update", "watermark", "mid", "保存水印设置失败", "failed")
			writeJSONCode(w, http.StatusInternalServerError, apiError{Error: "保存失败：请检查数据目录的权限与磁盘空间", Code: "store"})
			return
		}
		sum := "无变化"
		if d := wmDiff(old, next); len(d) > 0 {
			sum = strings.Join(d, "；")
		}
		s.auditReq(r, ru.U, "watermark.update", "watermark", "mid", "修改水印设置："+sum, "success")
		writeJSON(w, map[string]any{"ok": true, "watermark": next})
	}))
}
