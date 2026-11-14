// @author: kevinkelly
// @license: Apache License 2.0
// @file: sim.go
// @time: 2026/10/28

package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SimRunner 用于演示 / 测试：校验与令牌验证与 DirectRunner 完全一致，但不会执行任何进程，
// 而是按 Kind 生成逼真的 tiup 输出并回调 emit。
type SimRunner struct {
	Pol  Policy
	Topo TopoFunc
	Key  []byte
	Log  func(string)
	Now  func() time.Time // 测试用时钟；nil 表示 time.Now

	// Delay 是一次执行的总耗时，均匀分摊到每一行输出之间（响应 ctx 取消）。
	Delay time.Duration
	// FailKind 非空时，该 Kind 的命令会在输出 FailAfter 行之后以退出码 1 失败。
	FailKind  string
	FailAfter int

	mu    sync.Mutex
	calls []Request
}

// Calls 返回通过校验、被「执行」过的请求（按时间顺序，已拷贝）。被拒绝的请求不会出现在里面。
func (s *SimRunner) Calls() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.calls))
	for i, r := range s.calls {
		r.Cmd.Nodes = append([]string(nil), r.Cmd.Nodes...)
		out[i] = r
	}
	return out
}

func (s *SimRunner) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(logSafe(fmt.Sprintf(format, a...)))
	}
}

func (s *SimRunner) Run(ctx context.Context, req Request, emit func(string)) (Result, error) {
	start := time.Now()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	c := req.Cmd
	if err := authorize(ctx, s.Pol, s.Topo, s.Key, now, req, nil); err != nil {
		s.logf("sim reject kind=%s cluster=%s actor=%s change=%s reason=%v", truncate(c.Kind, 40), truncate(c.Cluster, 64), truncate(req.Actor, 64), truncate(req.ChangeID, 128), err)
		return failResult(start, err)
	}
	plan, t, err := dryArgv(ctx, s.Pol, s.Topo, c)
	if err != nil {
		return failResult(start, err)
	}
	if len(t.Nodes) == 0 && s.Topo != nil { // display 等命令校验时不取拓扑，这里尽力补上以生成输出
		if tp, err := s.Topo(ctx, c.Cluster); err == nil {
			t = tp
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	s.logf("sim exec kind=%s cluster=%s actor=%s change=%s cmd=%s", c.Kind, c.Cluster, truncate(req.Actor, 64), req.ChangeID, plan)

	lines := simLines(c, t, plan)
	fail := s.FailKind != "" && s.FailKind == c.Kind
	if fail {
		if s.FailAfter < len(lines) {
			lines = lines[:s.FailAfter]
		}
		lines = append(lines, "Error: simulated failure (injected by SimRunner)")
	}
	step := time.Duration(0)
	if s.Delay > 0 && len(lines) > 0 {
		step = s.Delay / time.Duration(len(lines))
	}
	ob := newOutBuf(emit, nil)
	for _, ln := range lines {
		if step > 0 {
			select {
			case <-time.After(step):
			case <-ctx.Done():
				ob.flush()
				return Result{Exit: -1, Output: ob.String(), Err: "执行已取消", Millis: time.Since(start).Milliseconds()}, ctx.Err()
			}
		}
		_, _ = ob.Write([]byte(ln + "\n"))
	}
	ob.flush()
	res := Result{Output: ob.String(), Millis: time.Since(start).Milliseconds()}
	if fail {
		res.Exit, res.Err = 1, "退出码 1"
	}
	return res, nil
}

// ---------------------------------------------------------------- 输出生成 ----

var hostLineRe = regexp.MustCompile(`(?m)^\s*-?\s*host:\s*([0-9.]+)\s*$`)

// simCheckRows 生成一台主机的 `tiup cluster check` 结果行（Node Check Result Message）。
// 模拟约定：IP 末位为 250 的主机有「Swap 开启（Fail）+ THP 为 always（Warn）」，末位为 251 的主机有
// 「文件句柄不足（Fail）+ 数据盘缺 nodelalloc（Warn）+ 内核参数（Fail）」，其余主机全部通过。
// 第二个返回值表示有没有 Warn / Fail。
func simCheckRows(ip string) ([]string, bool) {
	last := ip[strings.LastIndex(ip, ".")+1:]
	type row struct{ name, res, msg string }
	rows := []row{
		{"os-version", "Pass", "OS is Rocky Linux 9.3 9.3"},
		{"cpu-cores", "Pass", "number of CPU cores / threads: 16"},
		{"memory", "Pass", "memory size is 64GB"},
		{"selinux", "Pass", "SELinux is disabled"},
		{"thp", "Pass", "THP is disabled"},
		{"swap", "Pass", "swap is disabled"},
		{"ntp", "Pass", "service chronyd is running"},
		{"cpu-governor", "Pass", "CPU frequency governor is performance"},
	}
	set := func(name, res, msg string) {
		for i := range rows {
			if rows[i].name == name {
				rows[i].res, rows[i].msg = res, msg
				return
			}
		}
		rows = append(rows, row{name, res, msg})
	}
	switch last {
	case "250":
		set("swap", "Fail", "swap is enabled, please disable it for best performance")
		set("thp", "Warn", "THP is enabled, please disable it for best performance")
	case "251":
		set("limits", "Fail", "soft limit of 'nofile' for user 'tidb' is not set or too low")
		rows = append(rows, row{"limits", "Fail", "hard limit of 'nofile' for user 'tidb' is not set or too low"}) // 同一项多行，由调用方归并
		set("disk", "Warn", "mount point /tidb-data does not have 'nodelalloc' option set")
		set("sysctl", "Fail", "net.core.somaxconn = 128, should be greater than 32768")
	}
	var out []string
	bad := false
	for _, r := range rows {
		if r.res != "Pass" {
			bad = true
		}
		out = append(out, fmt.Sprintf("%s  %-14s %-7s %s", ip, r.name, r.res, r.msg))
	}
	return out, bad
}

func sortedNodes(t Topo) []string {
	ids := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func simDisplay(cluster string, t Topo) []string {
	user := orDefault(t.User, "tidb")
	out := []string{
		"Cluster type:       tidb",
		"Cluster name:       " + cluster,
		"Cluster version:    " + t.Version,
		"Deploy user:        " + user,
		"SSH type:           builtin",
		"Dashboard URL:      " + t.PD + "/dashboard",
		"ID                 Role          Host       Ports        OS/Arch       Status   Data Dir                      Deploy Dir",
		"--                 ----          ----       -----        -------       ------   --------                      ----------",
	}
	statusOf := map[string]string{}
	for st, id := range t.Status {
		statusOf[id] = st
	}
	leaderMarked := false
	for _, id := range sortedNodes(t) {
		role := tiupRoleName(t.Nodes[id])
		host, port, _ := strings.Cut(id, ":")
		ports := port
		if st, ok := statusOf[id]; ok {
			if _, sp, _ := strings.Cut(st, ":"); sp != port {
				ports = port + "/" + sp
			}
		}
		status := "Up"
		if role == "pd" && !leaderMarked {
			status, leaderMarked = "Up|L|UI", true
		}
		data := "/tidb-data/" + role + "-" + port
		if role == "tidb" {
			data = "-"
		}
		out = append(out, fmt.Sprintf("%-18s %-13s %-10s %-12s %-13s %-8s %-29s %s",
			id, role, host, ports, "linux/x86_64", status, data, "/tidb-deploy/"+role+"-"+port))
	}
	out = append(out, fmt.Sprintf("Total nodes: %d", len(t.Nodes)))
	return out
}

func perNode(verbs []string, nodes []string, t Topo) []string {
	var out []string
	for _, n := range nodes {
		role := tiupRoleName(t.Nodes[n])
		for _, v := range verbs {
			out = append(out, fmt.Sprintf("\t%s instance %s", v, n))
		}
		out = append(out, fmt.Sprintf("\tSuccess: %s %s", role, n))
	}
	return out
}

func simLines(c Cmd, t Topo, plan Plan) []string {
	header := func() string {
		if len(plan.Argv) < 2 {
			return ""
		}
		comp, _, _ := strings.Cut(plan.Argv[1], ":")
		if comp == "cluster" && len(plan.Argv) > 2 {
			return "Starting component `cluster`: /home/tidb/.tiup/components/cluster/v1.16.1/tiup-cluster " + strings.Join(plan.Argv[2:], " ")
		}
		return "Starting component `" + comp + "`: " + strings.Join(plan.Argv[1:], " ")
	}
	nodes := c.Nodes
	switch c.Kind {
	case KindClusterList:
		return []string{header(), "Name  User  Version  Path  PrivateKey", "----  ----  -------  ----  ----------"}
	case KindTiupVersion:
		return []string{"1.16.1 tiup", "Go Version: go1.21.13", "Git Ref: v1.16.1"}
	case KindTiupClusterVer:
		return []string{"tiup version v1.16.1 tiup"}
	case KindClusterDisplay:
		return append([]string{header()}, simDisplay(c.Cluster, t)...)
	case KindClusterCheck:
		out := []string{header(), "Node         Check         Result  Message", "----         -----         ------  -------"}
		problems := false
		for _, h := range hostLineRe.FindAllStringSubmatch(c.Content, -1) {
			rows, bad := simCheckRows(h[1])
			problems = problems || bad
			out = append(out, rows...)
		}
		if c.Content == "" {
			for _, n := range sortedNodes(t) {
				host, _, _ := strings.Cut(n, ":")
				out = append(out, host+"  os-version    Pass    OS is Rocky Linux 9.3 9.3")
			}
		}
		if problems {
			return append(out, "Some checks failed or warned (simulated)；可用 tiup cluster check --apply 修复部分项")
		}
		return append(out, "All checks passed (simulated)")
	case KindPDQuery:
		return simPDQuery(c.Key, t)
	case KindCDCCaptures:
		out := []string{"["}
		var ids []string
		for _, id := range sortedNodes(t) {
			if canonicalRole(t.Nodes[id]) == "ticdc" {
				ids = append(ids, id)
			}
		}
		for i, id := range ids {
			comma := ","
			if i == len(ids)-1 {
				comma = ""
			}
			out = append(out, fmt.Sprintf(`  {"id": "cdc-%d", "is-owner": %v, "address": "%s", "cluster-id": "default"}%s`, i+1, i == 0, id, comma))
		}
		return append(out, "]")

	case KindClusterRestart:
		out := []string{header(), fmt.Sprintf("Will restart the cluster %s with nodes: %s", c.Cluster, strings.Join(nodes, ","))}
		out = append(out, "+ [ Serial ] - RestartCluster")
		out = append(out, perNode([]string{"Stopping", "Starting"}, nodes, t)...)
		return append(out, fmt.Sprintf("Restarted cluster `%s` successfully", c.Cluster))
	case KindClusterStop:
		out := []string{header(), fmt.Sprintf("Will stop the cluster %s with nodes: %s", c.Cluster, strings.Join(nodes, ","))}
		out = append(out, perNode([]string{"Stopping"}, nodes, t)...)
		return append(out, fmt.Sprintf("Stopped cluster `%s` successfully", c.Cluster))
	case KindClusterStart:
		out := []string{header(), fmt.Sprintf("Starting cluster %s with nodes: %s", c.Cluster, strings.Join(nodes, ","))}
		out = append(out, perNode([]string{"Starting"}, nodes, t)...)
		return append(out, fmt.Sprintf("Started cluster `%s` successfully", c.Cluster))
	case KindClusterScaleIn:
		out := []string{header(), fmt.Sprintf("Will scale-in the cluster %s with nodes: %s", c.Cluster, strings.Join(nodes, ","))}
		out = append(out, perNode([]string{"Scaling in"}, nodes, t)...)
		return append(out, "Scaled cluster `"+c.Cluster+"` in successfully")
	case KindClusterScaleOut:
		out := []string{header(), "+ Detect CPU Arch Name", "  - Detecting node(s) arch info ... Done", "+ [ Serial ] - ScaleOut"}
		for _, h := range hostLineRe.FindAllStringSubmatch(c.Content, -1) {
			out = append(out, "\tDeploy instance on "+h[1], "\tStart instance on "+h[1], "\tSuccess: "+h[1])
		}
		return append(out, "Scaled cluster `"+c.Cluster+"` out successfully")
	case KindClusterPrune:
		return []string{header(), "+ [ Serial ] - FindTomestoneNodes", "Nothing to prune (simulated)", "Destroy success"}
	case KindClusterEditCfg:
		return []string{header(), "Applied topology file to cluster `" + c.Cluster + "` (simulated)", "Cluster `" + c.Cluster + "` edit-config succeeded"}
	case KindClusterReload:
		target := strings.Join(nodes, ",")
		if c.Role != "" {
			target = "role " + c.Role
			nodes = nil
			for _, id := range sortedNodes(t) {
				if canonicalRole(t.Nodes[id]) == canonicalRole(c.Role) {
					nodes = append(nodes, id)
				}
			}
		}
		out := []string{header(), "Will reload the cluster " + c.Cluster + " with " + target, "+ [ Serial ] - UpgradeCluster"}
		out = append(out, perNode([]string{"Restarting"}, nodes, t)...)
		return append(out, "Reloaded cluster `"+c.Cluster+"` successfully")

	case KindPDConfigSet, KindPDEvictAdd, KindPDEvictRemove, KindPDStoreLimit, KindPDLeaderTransfer:
		return []string{header(), "Success!"}
	case KindSQLSetConfig, KindSQLSetGlobal:
		return nil // mysql --batch 对 SET 语句没有输出

	case KindBRBackupFull:
		return []string{header(), "Detail BR log in /tmp/br.log.simulated",
			"Full Backup <---------/.................................................> 18.00%",
			"Full Backup <------------------------------------------------------/> 100.00%",
			"[INFO] [collector.go:77] [\"Full Backup success summary\"] [total-ranges=64] [ranges-succeed=64]",
			"[INFO] [collector.go:77] [\"Full Backup success summary\"] [backup-total-regions=64]"}
	case KindBRRestoreFull:
		return []string{header(), "Detail BR log in /tmp/br.log.simulated",
			"Full Restore <------------------------------------------------------/> 100.00%",
			"[INFO] [collector.go:77] [\"Full Restore success summary\"] [total-ranges=64] [ranges-succeed=64]"}
	case KindBRLogStart:
		return []string{header(), "[INFO] [log.go:60] [\"log backup task started\"] [task=" + c.Name + "]", "Log backup task " + c.Name + " started (simulated)"}
	case KindBRLogStop:
		return []string{header(), "[INFO] [log.go:80] [\"log backup task stopped\"] [task=" + c.Name + "]", "Log backup task " + c.Name + " stopped (simulated)"}
	case KindClusterUpgrade:
		return []string{header(), "Will upgrade the cluster " + c.Cluster + " from " + t.Version + " to " + c.Value, "+ [ Serial ] - Download components", "+ [ Serial ] - UpgradeCluster", "	Upgrading component pd", "	Upgrading component tikv", "	Upgrading component tidb", "Upgraded cluster `" + c.Cluster + "` successfully"}
	case KindAlertSilence:
		sum := sha256.Sum256([]byte(c.Key + "|" + c.Instance + "|" + strconv.Itoa(c.Rate)))
		return []string{"静默已创建 silenceID=" + hex.EncodeToString(sum[:8])}
	case KindBRLogStatus:
		return []string{header(), "● Total 1 Tasks.", "> #1 <", "              name: simulated-task", "            status: ● NORMAL", "             start: 2025-10-05 09:00:00.000 +0800", "               end: 2090-11-18 22:07:45.624 +0800"}
	}
	return []string{header(), "(simulated)"}
}

func simPDQuery(key string, t Topo) []string {
	switch key {
	case "store":
		var tikv []string
		for _, id := range sortedNodes(t) {
			if t.Nodes[id] == "tikv" {
				tikv = append(tikv, id)
			}
		}
		out := []string{"{", fmt.Sprintf(`  "count": %d,`, len(tikv)), `  "stores": [`}
		for i, id := range tikv {
			comma := ","
			if i == len(tikv)-1 {
				comma = ""
			}
			out = append(out, fmt.Sprintf(`    {"store": {"id": %d, "address": "%s", "state_name": "Up"}}%s`, i+1, id, comma))
		}
		return append(out, "  ]", "}")
	case "member":
		out := []string{"{", `  "members": [`}
		var pds []string
		for _, id := range sortedNodes(t) {
			if t.Nodes[id] == "pd" {
				pds = append(pds, id)
			}
		}
		for i, id := range pds {
			comma := ","
			if i == len(pds)-1 {
				comma = ""
			}
			out = append(out, fmt.Sprintf(`    {"name": "pd-%s", "member_id": %d, "client_urls": ["http://%s"]}%s`, strings.ReplaceAll(id, ":", "-"), 1000+i, id, comma))
		}
		return append(out, "  ]", "}")
	case "scheduler-show":
		return []string{"[", `  "balance-leader-scheduler",`, `  "balance-region-scheduler",`, `  "balance-hot-region-scheduler"`, "]"}
	case "region-down-peer", "region-pending-peer":
		return []string{"{", `  "count": 0,`, `  "regions": []`, "}"}
	case "config-show":
		return []string{"{", `  "replication": {"max-replicas": ` + strconv.Itoa(3) + `},`, `  "schedule": {"max-store-down-time": "30m0s"}`, "}"}
	}
	return nil
}
