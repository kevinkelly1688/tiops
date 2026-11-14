// @author: kevinkelly
// @license: Apache License 2.0
// @file: host_api.go
// @time: 2026/10/28

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"tidbops/internal/authz"
	"tidbops/internal/change"
	"tidbops/internal/collect"
	"tidbops/internal/exec"
	"tidbops/internal/model"
)

// 主机池：候选扩容主机的清单（尚未加入集群的机器），持久化到 <Dir>/hostpool.json（0600，原子写）。
// 每台主机可以做「预检」：生成只含该主机的最小扩容拓扑，经执行器的读类命令 tiup cluster check <c> <file> --cluster 运行，
// 解析该主机的检查结果并保存为 lastCheck。预检不改变任何东西，不受写操作开关限制，但需要执行器。
//
//	GET  /api/hostpool                 登录即可：{hosts, inCluster}
//	POST /api/hostpool                 scale：{ip, az, rack, note}
//	POST /api/hostpool/{ip}            scale：更新 {az, rack, note}（缺省的字段保持不变）
//	POST /api/hostpool/{ip}/delete     scale：移出主机池
//	POST /api/hostpool/{ip}/check      scale：{role} 预检，最长 3 分钟

// hostCheckFresh 是「最近一次预检」的有效期：扩容规划只在它之内参考预检结果。
const hostCheckFresh = 5 * time.Minute

var (
	reHostLabel = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,32}$`)
	poolRoles   = map[string]bool{"tikv": true, "tidb": true, "pd": true, "tiflash": true, "ticdc": true}
)

const maxPoolHosts = 500

// poolCheckItem 是预检的一项。r：ok | warn | bad（与 tiup 的 Pass / Warn / Fail 对应）；fix 是失败 / 警告项的修复建议命令，没有为空串。
type poolCheckItem struct {
	N   string `json:"n"`           // tiup cluster check 的检查项名，如 swap、thp、disk
	L   string `json:"l,omitempty"` // 中文名
	R   string `json:"r"`
	D   string `json:"d"`
	Fix string `json:"fix"`
}

// poolCheck 是主机池里一台主机最近一次预检的结果。OK=没有 bad（失败）项；警告项不阻止扩容。
type poolCheck struct {
	At    time.Time       `json:"at"`
	Role  string          `json:"role"`
	OK    bool            `json:"ok"`
	Items []poolCheckItem `json:"items"`
}

// poolHost 是主机池里的一台候选主机。
type poolHost struct {
	IP        string     `json:"ip"`
	AZ        string     `json:"az"`
	Rack      string     `json:"rack"`
	Note      string     `json:"note"`
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	LastCheck *poolCheck `json:"lastCheck"`
}

// hostPool 是主机池的内存状态与持久化。
type hostPool struct {
	mu       sync.Mutex
	path     string // 为空表示不持久化（运维模式没有数据目录时）
	hosts    []*poolHost
	checking map[string]bool // 正在预检的主机
}

// hostPools 按 ops 实例保存主机池（ops 结构由别处定义，这里不给它加字段）。
var hostPools sync.Map // *ops → *hostPool

// pool 取得（首次使用时从磁盘加载）这个 ops 的主机池。文件损坏时备份为 hostpool.json.bad 并从空池开始，不静默覆盖。
func (s *Server) pool(o *ops) *hostPool {
	if p, ok := hostPools.Load(o); ok {
		return p.(*hostPool)
	}
	p := &hostPool{checking: map[string]bool{}}
	if o.cfg.Dir != "" {
		p.path = filepath.Join(o.cfg.Dir, "hostpool.json")
		if b, err := os.ReadFile(p.path); err == nil {
			var f struct {
				Hosts []*poolHost `json:"hosts"`
			}
			if err := json.Unmarshal(b, &f); err != nil {
				s.cfg.Logf("hostpool.json 解析失败，已备份为 hostpool.json.bad 并从空池开始: %v", err)
				_ = os.Rename(p.path, p.path+".bad")
			} else {
				for _, h := range f.Hosts {
					if h != nil && h.IP != "" {
						p.hosts = append(p.hosts, h)
					}
				}
			}
		}
	}
	act, _ := hostPools.LoadOrStore(o, p)
	return act.(*hostPool)
}

// saveLocked 原子写盘（临时文件 + rename，0600）。调用方持有 p.mu。
func (p *hostPool) saveLocked() error {
	if p.path == "" {
		return nil
	}
	hosts := p.hosts
	if hosts == nil {
		hosts = []*poolHost{}
	}
	b, err := json.MarshalIndent(map[string]any{"hosts": hosts}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, p.path)
}

func (p *hostPool) findLocked(ip string) *poolHost {
	for _, h := range p.hosts {
		if h.IP == ip {
			return h
		}
	}
	return nil
}

// list 返回按 IP 排序的副本。
func (p *hostPool) list() []poolHost {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]poolHost, 0, len(p.hosts))
	for _, h := range p.hosts {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return ipKeyLess(out[i].IP, out[j].IP) })
	return out
}

func ipKeyLess(a, b string) bool {
	pa, pb := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if pa == nil || pb == nil {
		return a < b
	}
	for i := 0; i < 4; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// recentHostChecks 返回最近 5 分钟内有预检记录的主机：ip → 是否通过。供扩容规划（change.Ctx.HostChecks）使用。
func (s *Server) recentHostChecks(o *ops) map[string]bool {
	p := s.pool(o)
	now := o.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out map[string]bool
	for _, h := range p.hosts {
		if c := h.LastCheck; c != nil && now.Sub(c.At) <= hostCheckFresh && now.Sub(c.At) > -time.Minute {
			if out == nil {
				out = map[string]bool{}
			}
			out[h.IP] = c.OK
		}
	}
	return out
}

// validIPv4 校验规范写法的 IPv4 单播地址（拒绝前导零、IPv6、未指定 / 回环 / 组播 / 广播）。
func validIPv4(s string) error {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil || strings.Contains(s, ":") || ip.To4().String() != s {
		return fmt.Errorf("%q 不是合法的 IPv4 地址", truncRunes(s, 40))
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.To4().Equal(net.IPv4bcast) {
		return fmt.Errorf("%s 不是可用的主机地址", s)
	}
	return nil
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// validNote 校验备注：不超过 200 个字符，不含控制字符（换行、制表符也不允许，保证审计与表格里一行显示）。
func validNote(s string) error {
	if len([]rune(s)) > 200 {
		return fmt.Errorf("备注不能超过 200 个字符")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("备注不能包含控制字符或换行")
		}
	}
	return nil
}

func validPlacement(az, rack string) error {
	if !reHostLabel.MatchString(az) {
		return fmt.Errorf("可用区（az）只能包含字母、数字、_ . -，且不超过 32 个字符")
	}
	if !reHostLabel.MatchString(rack) {
		return fmt.Errorf("机架（rack）只能包含字母、数字、_ . -，且不超过 32 个字符")
	}
	return nil
}

// clusterHostSet 返回拓扑里已有的主机 IP 集合。
func (s *Server) clusterHostSet(ctx context.Context) map[string]bool {
	set := map[string]bool{}
	for _, in := range s.Snapshot(ctx, false).Topology.Instances {
		set[in.Host] = true
	}
	return set
}

func (s *Server) registerHosts(mux *http.ServeMux) {
	o := s.ops
	cluster := s.cfg.Col.Params.Cluster
	p := s.pool(o)
	hostPath := func(r *http.Request) (string, bool) {
		ip := r.PathValue("ip")
		return ip, validIPv4(ip) == nil
	}

	mux.HandleFunc("GET /api/hostpool", s.secured("", func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		in := []string{}
		for ip := range s.clusterHostSet(r.Context()) {
			in = append(in, ip)
		}
		sort.Slice(in, func(i, j int) bool { return ipKeyLess(in[i], in[j]) })
		writeJSON(w, map[string]any{"hosts": p.list(), "inCluster": in})
	}))

	mux.HandleFunc("POST /api/hostpool", s.secured(authz.CapScale, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		var in struct{ IP, AZ, Rack, Note string }
		if !decodeBody(w, r, &in) {
			return
		}
		in.IP, in.AZ, in.Rack, in.Note = strings.TrimSpace(in.IP), strings.TrimSpace(in.AZ), strings.TrimSpace(in.Rack), strings.TrimSpace(in.Note)
		for _, err := range []error{validIPv4(in.IP), validPlacement(in.AZ, in.Rack), validNote(in.Note)} {
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if s.clusterHostSet(r.Context())[in.IP] {
			writeErr(w, http.StatusConflict, in.IP+" 已在集群拓扑里，不能加入主机池")
			return
		}
		p.mu.Lock()
		if p.findLocked(in.IP) != nil {
			p.mu.Unlock()
			writeErr(w, http.StatusConflict, in.IP+" 已在主机池里")
			return
		}
		if len(p.hosts) >= maxPoolHosts {
			p.mu.Unlock()
			writeErr(w, http.StatusConflict, fmt.Sprintf("主机池已满（上限 %d 台）", maxPoolHosts))
			return
		}
		h := &poolHost{IP: in.IP, AZ: in.AZ, Rack: in.Rack, Note: in.Note, CreatedBy: ru.U.Name, CreatedAt: o.now().UTC()}
		p.hosts = append(p.hosts, h)
		if err := p.saveLocked(); err != nil {
			p.hosts = p.hosts[:len(p.hosts)-1]
			p.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "保存主机池失败："+err.Error())
			return
		}
		out := *h
		p.mu.Unlock()
		s.auditReq(r, ru.U, "hostpool.add", in.IP, "low", fmt.Sprintf("加入主机池 az=%s rack=%s", in.AZ, in.Rack), "success")
		writeJSON(w, map[string]any{"ok": true, "host": out})
	}))

	mux.HandleFunc("POST /api/hostpool/{ip}", s.secured(authz.CapScale, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ip, ok := hostPath(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "主机地址不合法")
			return
		}
		var in struct{ AZ, Rack, Note *string }
		if !decodeBody(w, r, &in) {
			return
		}
		p.mu.Lock()
		h := p.findLocked(ip)
		if h == nil {
			p.mu.Unlock()
			writeErr(w, http.StatusNotFound, ip+" 不在主机池里")
			return
		}
		n := *h
		set := func(dst *string, src *string) {
			if src != nil {
				*dst = strings.TrimSpace(*src)
			}
		}
		set(&n.AZ, in.AZ)
		set(&n.Rack, in.Rack)
		set(&n.Note, in.Note)
		for _, err := range []error{validPlacement(n.AZ, n.Rack), validNote(n.Note)} {
			if err != nil {
				p.mu.Unlock()
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		old := *h
		h.AZ, h.Rack, h.Note = n.AZ, n.Rack, n.Note
		if err := p.saveLocked(); err != nil {
			h.AZ, h.Rack, h.Note = old.AZ, old.Rack, old.Note
			p.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "保存主机池失败："+err.Error())
			return
		}
		out := *h
		p.mu.Unlock()
		s.auditReq(r, ru.U, "hostpool.update", ip, "low", fmt.Sprintf("更新主机池条目 az=%s rack=%s", out.AZ, out.Rack), "success")
		writeJSON(w, map[string]any{"ok": true, "host": out})
	}))

	mux.HandleFunc("POST /api/hostpool/{ip}/delete", s.secured(authz.CapScale, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ip, ok := hostPath(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "主机地址不合法")
			return
		}
		p.mu.Lock()
		idx := -1
		for i, h := range p.hosts {
			if h.IP == ip {
				idx = i
			}
		}
		if idx < 0 {
			p.mu.Unlock()
			writeErr(w, http.StatusNotFound, ip+" 不在主机池里")
			return
		}
		removed := p.hosts[idx]
		p.hosts = append(append([]*poolHost(nil), p.hosts[:idx]...), p.hosts[idx+1:]...)
		if err := p.saveLocked(); err != nil {
			p.hosts = append(p.hosts[:idx], append([]*poolHost{removed}, p.hosts[idx:]...)...)
			p.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "保存主机池失败："+err.Error())
			return
		}
		p.mu.Unlock()
		s.auditReq(r, ru.U, "hostpool.delete", ip, "low", "移出主机池", "success")
		writeJSON(w, map[string]any{"ok": true})
	}))

	mux.HandleFunc("POST /api/hostpool/{ip}/check", s.secured(authz.CapScale, func(w http.ResponseWriter, r *http.Request, ru reqUser) {
		ip, ok := hostPath(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "主机地址不合法")
			return
		}
		var in struct{ Role string }
		if !decodeBody(w, r, &in) {
			return
		}
		if !poolRoles[in.Role] {
			writeErr(w, http.StatusBadRequest, "role 需为 tikv | tidb | pd | tiflash | ticdc")
			return
		}
		if _, isNo := o.cfg.Runner.(noRunner); isNo {
			writeErr(w, http.StatusServiceUnavailable, "未配置执行器（启动参数 --exec），无法运行预检")
			return
		}
		if s.clusterHostSet(r.Context())[ip] {
			writeErr(w, http.StatusConflict, ip+" 已在集群拓扑里，主机池预检仅针对尚未加入集群的主机")
			return
		}
		p.mu.Lock()
		h := p.findLocked(ip)
		if h == nil {
			p.mu.Unlock()
			writeErr(w, http.StatusNotFound, ip+" 不在主机池里")
			return
		}
		if p.checking[ip] {
			p.mu.Unlock()
			writeErr(w, http.StatusConflict, ip+" 正在预检，请稍候")
			return
		}
		p.checking[ip] = true
		az, rack := h.AZ, h.Rack
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			delete(p.checking, ip)
			p.mu.Unlock()
		}()

		// 只含该主机的最小扩容拓扑；TiKV 要求 zone 标签，主机池没填可用区时用占位值（预检不依赖它）。
		if in.Role == "tikv" && az == "" {
			az = "az-precheck"
		}
		content, err := change.BuildScaleOutYAML(in.Role, []change.HostSpec{{IP: ip, AZ: az, Rack: rack}}, 0, 0)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		out, runErr := s.runRead(ctx, exec.Cmd{Kind: exec.KindClusterCheck, Cluster: cluster, Content: content})
		items := collect.ParseCheckByHost(out, ip)
		if len(items) == 0 {
			// 没有该主机的任何检查行：命令根本没跑起来（执行器拒绝 / 超时）→ 报错且不保存；
			// tiup 跑了但失败（如 SSH 连不上，输出里没有检查表）→ 记为一项失败的预检，让用户在页面上看到原因。
			tail := tailLines(out, 3)
			if runErr == nil || strings.TrimSpace(out) == "" {
				msg := "预检没有返回该主机的检查结果"
				if runErr != nil {
					msg = "预检执行失败：" + runErr.Error()
				}
				s.auditReq(r, ru.U, "hostpool.check", ip, "low", fmt.Sprintf("主机池预检（%s）：%s", in.Role, msg), "failed")
				writeErr(w, http.StatusBadGateway, msg)
				return
			}
			d := runErr.Error()
			if tail != "" {
				d += "：" + tail
			}
			items = []model.CheckItem{{G: "主机", N: "tiup cluster check", L: "预检执行", R: "bad", D: truncRunes(d, 400)}}
		}
		chk := &poolCheck{At: o.now().UTC(), Role: in.Role, OK: true}
		nbad, nwarn := 0, 0
		for _, it := range items {
			chk.Items = append(chk.Items, poolCheckItem{N: it.N, L: it.L, R: it.R, D: it.D, Fix: it.Fix})
			switch it.R {
			case "bad":
				nbad++
				chk.OK = false
			case "warn":
				nwarn++
			}
		}
		p.mu.Lock()
		h = p.findLocked(ip)
		if h == nil { // 预检期间被移出了主机池
			p.mu.Unlock()
			writeErr(w, http.StatusNotFound, ip+" 已被移出主机池")
			return
		}
		h.LastCheck = chk
		serr := p.saveLocked()
		res := *h
		p.mu.Unlock()
		if serr != nil {
			s.cfg.Logf("保存主机池失败: %v", serr)
		}
		s.auditReq(r, ru.U, "hostpool.check", ip, "low", fmt.Sprintf("主机池预检（%s）：失败 %d / 警告 %d / 共 %d 项", in.Role, nbad, nwarn, len(items)), "success")
		writeJSON(w, map[string]any{"ok": chk.OK, "host": res, "lastCheck": chk})
	}))
}

// tailLines 取输出的最后 n 个非空行，用于在预检失败时提示原因。
func tailLines(s string, n int) string {
	var ls []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			ls = append(ls, t)
		}
	}
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " | ")
}

// hostsNote 说明主机规格里哪些数据缺失（node_exporter 指标不全时给出排查提示），数据完整时为空串。
func hostsNote(hosts []model.HostInfo) string {
	if len(hosts) == 0 {
		return "拓扑里没有主机"
	}
	var unknown []string
	noOS := 0
	for _, h := range hosts {
		if !h.Known {
			unknown = append(unknown, h.IP)
		} else if h.OS == "" {
			noOS++
		}
	}
	var notes []string
	switch {
	case len(unknown) == len(hosts):
		return "没有采集到任何主机规格：请确认各主机已部署 node_exporter 且 Prometheus 已抓取（node_cpu_seconds_total、node_memory_MemTotal_bytes、node_uname_info）；规格显示为空，不做猜测"
	case len(unknown) > 0:
		notes = append(notes, fmt.Sprintf("%d 台主机没有规格数据（%s）：检查这些主机的 node_exporter", len(unknown), strings.Join(unknown, "、")))
	}
	if noOS > 0 {
		notes = append(notes, "部分主机没有 node_os_info 指标（node_exporter 版本较旧），系统版本未显示")
	}
	return strings.Join(notes, "；")
}
