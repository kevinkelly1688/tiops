// @author: kevinkelly
// @license: Apache License 2.0
// @file: display.go
// @time: 2026/10/28

package source

import (
	"fmt"
	"strings"
)

// Instance 是 `tiup cluster display` 输出里的一行实例。
// DataDir / DeployDir 来自表格的 Data Dir / Deploy Dir 两列：列数不足（旧版 tiup 或被截断的输出）时为空；
// 没有数据目录的组件（TiDB、Grafana 等）display 里写 "-"，这里同样归一为空。
type Instance struct {
	ID, Role, Host, Ports, Status string
	DataDir, DeployDir            string
}

// ClusterInfo 是解析后的 display 结果。
type ClusterInfo struct {
	Name, Version, User, DashboardURL, GrafanaURL string
	Instances                                     []Instance
}

// ParseDisplay 解析 `tiup cluster display <name>` 的文本输出。
// 解析失败不会 panic：缺失的字段保持为空，调用方按「未探测到」处理。
func ParseDisplay(out string) ClusterInfo {
	var ci ClusterInfo
	lines := strings.Split(out, "\n")
	table := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if !table {
			switch {
			case strings.HasPrefix(t, "Cluster name:"):
				ci.Name = field(t)
			case strings.HasPrefix(t, "Cluster version:"):
				ci.Version = field(t)
			case strings.HasPrefix(t, "Deploy user:"):
				ci.User = field(t)
			case strings.HasPrefix(t, "Dashboard URL:"):
				ci.DashboardURL = field(t)
			case strings.HasPrefix(t, "Grafana URL:"):
				ci.GrafanaURL = field(t)
			case strings.HasPrefix(t, "ID") && strings.Contains(t, "Role"):
				table = true
			}
			continue
		}
		if strings.HasPrefix(t, "--") || strings.HasPrefix(t, "Total nodes") {
			continue
		}
		f := strings.Fields(t)
		if len(f) < 6 {
			continue
		}
		in := Instance{ID: f[0], Role: f[1], Host: f[2], Ports: f[3], Status: f[5]}
		// 目录列：ID Role Host Ports OS/Arch Status DataDir DeployDir。tiup 的路径不含空白，按空白切分即可；
		// 恰好 7 列时无法判断缺的是哪一列，保守地都留空。
		if len(f) >= 8 {
			in.DataDir, in.DeployDir = dirCell(f[6]), dirCell(f[7])
		}
		ci.Instances = append(ci.Instances, in)
	}
	return ci
}

// dirCell 把 display 里的目录单元格归一：占位符 "-" 视为没有。
func dirCell(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

func field(line string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+1:])
}

// firstPort 取 "2379/2380" 的第一个端口，secondPort 取第二个。
func port(ports string, idx int) string {
	p := strings.Split(ports, "/")
	if idx < len(p) {
		return p[idx]
	}
	return ""
}

func (ci ClusterInfo) first(role string) *Instance {
	for i := range ci.Instances {
		in := &ci.Instances[i]
		if in.Role == role && strings.HasPrefix(in.Status, "Up") {
			return in
		}
	}
	return nil
}

// Endpoints 从拓扑推导各端点地址与 MySQL 连接（主机、端口）。找不到的键不出现在结果里。
func (ci ClusterInfo) Endpoints() (ep map[string]string, mysqlHost string, mysqlPort int) {
	ep = map[string]string{}
	if in := ci.first("pd"); in != nil {
		ep["pd"] = fmt.Sprintf("http://%s:%s", in.Host, port(in.Ports, 0))
	}
	if in := ci.first("tidb"); in != nil {
		ep["tidb"] = fmt.Sprintf("http://%s:%s", in.Host, port(in.Ports, 1))
		mysqlHost = in.Host
		fmt.Sscan(port(in.Ports, 0), &mysqlPort)
	}
	if in := ci.first("prometheus"); in != nil {
		ep["prom"] = fmt.Sprintf("http://%s:%s", in.Host, port(in.Ports, 0))
	}
	if in := ci.first("alertmanager"); in != nil {
		ep["alert"] = fmt.Sprintf("http://%s:%s", in.Host, port(in.Ports, 0))
	}
	return
}

// ParseClusterList 解析 `tiup cluster list`，返回集群名列表。
func ParseClusterList(out string) []string {
	var names []string
	for _, r := range ParseClusterListRows(out) {
		names = append(names, r.Name)
	}
	return names
}

// ClusterListRow 是 `tiup cluster list` 的一行（Name User Version Path PrivateKey）。PrivateKey 不取：平台不需要也不展示。
type ClusterListRow struct {
	Name, User, Version, Path string
}

// ParseClusterListRows 解析 `tiup cluster list` 的表格。
func ParseClusterListRows(out string) []ClusterListRow {
	var rows []ClusterListRow
	table := false
	for _, ln := range strings.Split(out, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if !table {
			if strings.HasPrefix(t, "Name") && strings.Contains(t, "Version") {
				table = true
			}
			continue
		}
		if strings.HasPrefix(t, "--") {
			continue
		}
		if f := strings.Fields(t); len(f) >= 3 {
			r := ClusterListRow{Name: f[0], User: f[1], Version: f[2]}
			if len(f) >= 4 {
				r.Path = f[3]
			}
			rows = append(rows, r)
		}
	}
	return rows
}
