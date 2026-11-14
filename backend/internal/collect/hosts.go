// @author: kevinkelly
// @license: Apache License 2.0
// @file: hosts.go
// @time: 2026/10/28

package collect

import (
	"context"
	"math"
	"net"
	"sort"
	"strings"

	"tidbops/internal/model"
)

// hostLabels 把带 instance 标签的 Prometheus 响应按主机归并，返回「主机 → 指定标签的值」。
// 同一主机有多条序列（例如多个网卡、多个挂载点）时取第一条有值的，保证结果稳定（按 instance 排序）。
func hostLabels(b []byte, labels ...string) map[string]map[string]string {
	series, err := ParseMatrix(b)
	if err != nil {
		return nil
	}
	sort.SliceStable(series, func(i, j int) bool { return series[i].Metric["instance"] < series[j].Metric["instance"] })
	out := map[string]map[string]string{}
	for _, s := range series {
		inst := s.Metric["instance"]
		if inst == "" {
			continue
		}
		h := hostOf(inst)
		for _, l := range labels {
			if v := s.Metric[l]; v != "" {
				if out[h] == nil {
					out[h] = map[string]string{}
				}
				if out[h][l] == "" {
					out[h][l] = v
				}
			}
		}
	}
	return out
}

// hostValues 把一个指标按主机取值（同一主机多条序列取最大值：核数、内存、磁盘容量都是「越大越接近真实」的规格量）。
func (c *Collector) hostValues(ctx context.Context, item string) map[string]float64 {
	b, err := c.get(ctx, item)
	if err != nil {
		return nil
	}
	series, err := ParseMatrix(b)
	if err != nil {
		return nil
	}
	m := map[string]float64{}
	for _, s := range series {
		inst := s.Metric["instance"]
		if inst == "" {
			continue
		}
		h := hostOf(inst)
		if v := s.Last(3); v > m[h] {
			m[h] = v
		}
	}
	return m
}

// Hosts 汇总拓扑里每台主机的规格与归属。规格来自 node_exporter 指标（采集项 prom/node-*），
// 任何一项取不到都保持 0 / 空（不猜测）；整台主机一项都没取到时 Known=false。
// 主机按 IP 排序；实例 ID 按拓扑里的顺序。
func (c *Collector) Hosts(ctx context.Context, topo model.Topology) []model.HostInfo {
	cores := c.hostValues(ctx, "prom/node-cores")
	mem := c.hostValues(ctx, "prom/node-mem-total")
	disk := c.hostValues(ctx, "prom/node-disk-size")
	var uname, osinfo map[string]map[string]string
	if b, err := c.get(ctx, "prom/node-uname"); err == nil {
		uname = hostLabels(b, "release", "nodename")
	}
	if b, err := c.get(ctx, "prom/node-os-info"); err == nil {
		osinfo = hostLabels(b, "pretty_name", "name", "version_id")
	}

	byIP := map[string]*model.HostInfo{}
	var ips []string
	for _, in := range topo.Instances {
		if in.Host == "" {
			continue
		}
		h := byIP[in.Host]
		if h == nil {
			h = &model.HostInfo{IP: in.Host, Instances: []string{}}
			byIP[in.Host] = h
			ips = append(ips, in.Host)
		}
		h.Instances = append(h.Instances, in.ID)
		if h.AZ == "" && in.AZ != "" && in.AZ != "—" {
			h.AZ = in.AZ
		}
		if h.Rack == "" {
			if r := in.Labels["rack"]; r != "" && r != "—" {
				h.Rack = r
			}
		}
	}
	sort.Slice(ips, func(i, j int) bool { return ipLess(ips[i], ips[j]) })
	out := make([]model.HostInfo, 0, len(ips))
	for _, ip := range ips {
		h := byIP[ip]
		h.Cores = int(math.Round(cores[ip]))
		h.MemGB = round1(mem[ip] / float64(gib))
		h.DiskGB = round1(disk[ip] / float64(gib))
		if u := uname[ip]; u != nil {
			h.Kernel, h.Hostname = u["release"], u["nodename"]
		}
		if o := osinfo[ip]; o != nil {
			switch {
			case o["pretty_name"] != "":
				h.OS = o["pretty_name"]
			case o["name"] != "":
				h.OS = strings.TrimSpace(o["name"] + " " + o["version_id"])
			}
		}
		h.Known = h.Cores > 0 || h.MemGB > 0 || h.DiskGB > 0 || h.OS != "" || h.Kernel != ""
		out = append(out, *h)
	}
	return out
}

// ipLess 按数值比较 IPv4；无法解析的按字符串比较。
func ipLess(a, b string) bool {
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
