// @author: kevinkelly
// @license: Apache License 2.0
// @file: clusters_import.go
// @time: 2026/10/28

package server

// 中控机模式：一键导入本机 tiup 管理的集群。
//
//	GET  /api/clusters/discover  列出本机 tiup 管理的集群（settings.edit）：经执行器跑只读的 tiup cluster list；
//	                             每项带状态 new | imported | conflict（同名但是手工添加的远程集群，只读）
//	POST /api/clusters/import    导入所选集群（settings.edit）：对每个集群经执行器跑只读的 tiup cluster display，
//	                             由输出推导 PD / TiDB / Prometheus / Alertmanager 地址，登记为 Local 集群（可做全部运维操作）
//
// 只有 --standalone --local-tiup 的进程提供；其他进程返回 available=false。手工添加（POST /api/clusters）的集群
// 永远不是 Local：平台碰不到它的 tiup / SSH，只读。导入不需要、也拿不到任何 SSH 私钥——tiup 在 tidb 用户下用自己的密钥工作。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tidbops/internal/authz"
	"tidbops/internal/clusters"
	"tidbops/internal/exec"
	"tidbops/internal/source"
)

const maxImportBatch = 20

// discoverItem 是 discover 返回的一项。
type discoverItem struct {
	Name    string `json:"name"`
	User    string `json:"user,omitempty"`
	Version string `json:"version,omitempty"`
	State   string `json:"state"` // new | imported | conflict
	ID      string `json:"id"`    // imported：已登记的 id；new：建议的 id
	Display string `json:"display,omitempty"`
	Env     string `json:"env,omitempty"`
}

// localRunner 返回本机执行器；没有（未配置 / noRunner）时返回错误说明。
func (h *Hub) localRunner() (exec.Runner, error) {
	if !h.cfg.LocalTiup {
		return nil, errors.New("本机不是 tiup 中控机：安装平台时需要加 --standalone --local-tiup")
	}
	r := h.primary.ops.cfg.Runner
	if _, isNo := r.(noRunner); isNo || r == nil {
		return nil, errors.New("未配置执行器（需要 --exec sudo 或 direct）")
	}
	return r, nil
}

// listLocal 经执行器读 tiup cluster list。
func (h *Hub) listLocal(ctx context.Context) ([]source.ClusterListRow, error) {
	run, err := h.localRunner()
	if err != nil {
		return nil, err
	}
	res, err := run.Run(ctx, exec.Request{Cmd: exec.Cmd{Kind: exec.KindClusterList}}, nil)
	if err != nil || res.Exit != 0 {
		msg := res.Err
		if err != nil {
			msg = err.Error()
		}
		return nil, fmt.Errorf("读取本机 tiup 集群列表失败：%s（请确认在平台所在机器上 sudo -n -u tidb tiup cluster list 能无交互运行）", truncRunes(msg, 200))
	}
	return source.ParseClusterListRows(res.Output), nil
}

// freeID 给出一个没被占用的 id（以 tiup 集群名为基础）。
func (h *Hub) freeID(name string) string {
	base := clusters.Slug(name)
	taken := func(x string) bool {
		for _, sp := range h.reg.List() {
			if sp.ID == x || sp.Name == x {
				return true
			}
		}
		return clusters.ValidID(x) && (x == "import" || x == "discover")
	}
	if !taken(base) {
		return base
	}
	for i := 2; i < 100; i++ {
		suffix := fmt.Sprintf("-%d", i)
		cand := base
		if len(cand)+len(suffix) > 32 {
			cand = strings.TrimRight(cand[:32-len(suffix)], "-")
		}
		if cand += suffix; !taken(cand) {
			return cand
		}
	}
	return base
}

type importItem struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Display string `json:"display"`
	Env     string `json:"env"`
	Desc    string `json:"desc"`
}

type importReq struct {
	Items         []importItem `json:"items"`
	MySQLUser     string       `json:"mysqlUser"`     // 只读 SQL 账号（可选；不填则不配置 SQL，相关页面无数据，之后可在编辑里补）
	MySQLPassword string       `json:"mysqlPassword"` // 可选；永不回显、不进审计
}

type importResult struct {
	Name     string   `json:"name"`
	OK       bool     `json:"ok"`
	ID       string   `json:"id,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func (s *Server) registerClusterImport(mux *http.ServeMux) {
	h := s.hub
	mux.HandleFunc("GET /api/clusters/discover", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		if _, err := h.localRunner(); err != nil {
			writeJSON(w, map[string]any{"available": false, "reason": err.Error(), "items": []discoverItem{}})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		rows, err := h.listLocal(ctx)
		if err != nil {
			writeJSON(w, map[string]any{"available": true, "error": err.Error(), "items": []discoverItem{}})
			return
		}
		items := []discoverItem{}
		for _, row := range rows {
			it := discoverItem{Name: row.Name, User: row.User, Version: row.Version, State: "new", ID: h.freeID(row.Name)}
			for _, sp := range h.reg.List() {
				if sp.Platform || sp.Name != row.Name {
					continue
				}
				it.ID, it.Display, it.Env = sp.ID, sp.Display, sp.Env
				it.State = "conflict"
				if sp.Local {
					it.State = "imported"
				}
			}
			items = append(items, it)
		}
		writeJSON(w, map[string]any{"available": true, "items": items, "defaultEnv": s.ops.cfg.Env})
	}))

	mux.HandleFunc("POST /api/clusters/import", s.secured(authz.CapSettings, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in importReq
		if !decodeBody(w, r, &in) {
			return
		}
		if len(in.Items) == 0 || len(in.Items) > maxImportBatch {
			writeJSONCode(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("一次导入 1–%d 个集群", maxImportBatch), "code": clusters.ErrInvalid})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		rows, err := h.listLocal(ctx)
		if err != nil {
			s.auditCluster(r, ru.U, "cluster.import", "", "", "", "mid", err.Error(), "failed")
			writeJSONCode(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": clusters.ErrStart})
			return
		}
		known := map[string]bool{}
		for _, row := range rows {
			known[row.Name] = true
		}
		run, _ := h.localRunner()
		results := make([]importResult, 0, len(in.Items))
		okN := 0
		for _, it := range in.Items {
			res := importResult{Name: it.Name}
			sp, warns, err := s.importOne(ctx, run, known, it, in, ru.U.Name)
			if err != nil {
				var ce *clusters.Error
				if errors.As(err, &ce) {
					res.Error = ce.Msg
				} else {
					res.Error = err.Error()
				}
				s.auditCluster(r, ru.U, "cluster.import", it.Env, it.Name, it.ID, "mid", "导入集群「"+it.Name+"」失败："+res.Error, "failed")
			} else {
				res.OK, res.ID, res.Warnings = true, sp.ID, warns
				okN++
				s.auditCluster(r, ru.U, "cluster.import", sp.Env, sp.Name, sp.ID, "mid", "从本机 tiup 导入集群「"+sp.Display+"」（可做全部运维操作）："+describe(sp), "success")
			}
			results = append(results, res)
		}
		writeJSON(w, map[string]any{"ok": okN == len(in.Items), "imported": okN, "results": results})
	}))
}

// importOne 导入一个集群：名字必须在 tiup cluster list 里；地址由 tiup cluster display 的输出推导（只读命令）。
func (s *Server) importOne(ctx context.Context, run exec.Runner, known map[string]bool, it importItem, req importReq, by string) (clusters.Spec, []string, error) {
	h := s.hub
	name := strings.TrimSpace(it.Name)
	if !clusters.ValidName(name) || !known[name] {
		return clusters.Spec{}, nil, &clusters.Error{Code: clusters.ErrNotFound, Msg: "本机 tiup 里没有这个集群"}
	}
	if it.ID == "" {
		it.ID = h.freeID(name)
	}
	res, err := run.Run(ctx, exec.Request{Cmd: exec.Cmd{Kind: exec.KindClusterDisplay, Cluster: name}}, nil)
	if err != nil || res.Exit != 0 {
		msg := res.Err
		if err != nil {
			msg = err.Error()
		}
		return clusters.Spec{}, nil, fmt.Errorf("tiup cluster display 失败：%s（执行器策略 exec.json 的 clusters 是否允许该集群？）", truncRunes(msg, 200))
	}
	ep, mh, mp := source.ParseDisplay(res.Output).Endpoints()
	if ep["pd"] == "" {
		return clusters.Spec{}, nil, errors.New("无法从 tiup cluster display 的输出里解析出 PD 地址（集群是否已启动 / 输出格式是否被改动？）")
	}
	display := strings.TrimSpace(it.Display)
	if display == "" {
		display = name
	}
	mode := clusters.ModeLive
	in := clusters.Input{ID: it.ID, Name: &name, Display: &display, Desc: &it.Desc, Env: &it.Env, Mode: &mode,
		PD: strp(ep["pd"]), TiDBStatus: strp(ep["tidb"]), Prom: strp(ep["prom"]), Alert: strp(ep["alert"])}
	var warns []string
	if u := strings.TrimSpace(req.MySQLUser); u != "" && mh != "" {
		port := mp
		in.MySQLHost, in.MySQLPort, in.MySQLUser = &mh, &port, &u
		if req.MySQLPassword != "" {
			in.MySQLPassword = &req.MySQLPassword
		}
	} else {
		warns = append(warns, "未配置只读 SQL 账号：慢 SQL / 连接数等依赖 SQL 的页面暂无数据，可在「集群接入」里编辑补上")
	}
	if ep["prom"] == "" {
		warns = append(warns, "拓扑里没有 Prometheus：监控曲线类页面暂无数据")
	}
	sp, w2, err := h.Import(in, by)
	if err != nil {
		return clusters.Spec{}, nil, err
	}
	return sp, append(warns, w2...), nil
}

func strp(s string) *string { return &s }
