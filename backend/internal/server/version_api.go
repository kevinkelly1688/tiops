// @author: kevinkelly
// @license: Apache License 2.0
// @file: version_api.go
// @time: 2026/10/28

package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"tidbops/internal/buildinfo"
	"tidbops/internal/dataver"
)

// processStart 是进程启动时间，用于计算已运行时长。
var processStart = time.Now()

// versionHistoryMax 是返回的升级历史条数上限。
const versionHistoryMax = 10

// registerVersion 注册 GET /api/ops/version：版本、构建信息、安装布局与最近升级历史。
// 全部只读、容错：任何读取失败都返回空值，而不是报错。
func (s *Server) registerVersion(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ops/version", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		writeJSON(w, s.versionInfo())
	}))
}

func (s *Server) versionInfo() map[string]any {
	dir := ""
	if s.ops != nil {
		dir = s.ops.cfg.Dir
	}
	now := time.Now()
	return map[string]any{
		"version":       Version,
		"commit":        buildinfo.Commit,
		"buildTime":     buildinfo.BuildTime,
		"goVersion":     runtime.Version(),
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"schema":        dataver.Schema,
		"dataSchema":    readDataSchema(dir),
		"startedAt":     processStart,
		"uptimeSec":     int64(now.Sub(processStart).Seconds()),
		"installLayout": installLayout(dir),
	}
}

// readDataSchema 读取 <数据目录>/VERSION 里的数据 schema 版本；没有或读不出返回 0。
func readDataSchema(dir string) int {
	if dir == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// installLayout 根据可执行文件路径推断安装布局：<opt>/releases/<版本>/bin/tidbops-server。
func installLayout(dataDir string) map[string]any {
	out := map[string]any{"installed": false, "current": "", "releases": []string{}, "history": readUpgradeHistory(dataDir)}
	exe, err := os.Executable()
	if err != nil {
		return out
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	binDir := filepath.Dir(exe)      // .../releases/<ver>/bin
	relDir := filepath.Dir(binDir)   // .../releases/<ver>
	releases := filepath.Dir(relDir) // .../releases
	if filepath.Base(binDir) != "bin" || filepath.Base(releases) != "releases" {
		return out
	}
	out["installed"] = true
	out["current"] = filepath.Base(relDir)
	out["optRoot"] = filepath.Dir(releases)
	out["releases"] = listReleases(releases)
	return out
}

// listReleases 列出 releases 目录下的版本目录（按版本号从新到旧）。
func listReleases(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	vs := []string{}
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			vs = append(vs, e.Name())
		}
	}
	sort.Slice(vs, func(i, j int) bool { return semverLess(vs[j], vs[i]) })
	return vs
}

// semverLess 按 x.y.z 数字比较；无法解析的退回字符串比较。
func semverLess(a, b string) bool {
	pa, pb := semverParts(a), semverParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return a < b
}

func semverParts(v string) [3]int {
	var out [3]int
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	for i, p := range strings.Split(core, ".") {
		if i >= 3 {
			break
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// readUpgradeHistory 读取 <数据目录>/upgrades.jsonl 最近 10 条（新的在前）。每行一个 JSON 对象，坏行跳过。
func readUpgradeHistory(dir string) []map[string]any {
	out := []map[string]any{}
	if dir == "" {
		return out
	}
	f, err := os.Open(filepath.Join(dir, "upgrades.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	var all []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m == nil {
			continue
		}
		all = append(all, m)
		if len(all) > 200 { // 只保留末尾一段，避免超大文件占内存
			all = all[len(all)-versionHistoryMax:]
		}
	}
	if len(all) > versionHistoryMax {
		all = all[len(all)-versionHistoryMax:]
	}
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
	}
	return out
}
