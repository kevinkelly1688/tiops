// @author: kevinkelly
// @license: Apache License 2.0
// @file: misc.go
// @time: 2026/10/28

package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"tidbops/internal/model"
	"tidbops/internal/source"
)

// ---------------------------------------------------------------- 告警 ----

// Alerts 解析 Alertmanager 的活跃告警，按严重程度排序。
func (c *Collector) Alerts(ctx context.Context) ([]model.Alert, error) {
	b, err := c.get(ctx, "alert/alerts")
	if err != nil {
		return nil, fmt.Errorf("alert/alerts: %w", err)
	}
	var raw []struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("解析告警失败: %w", err)
	}
	var out []model.Alert
	for _, a := range raw {
		lv := strings.ToLower(a.Labels["severity"])
		if lv != "critical" && lv != "warning" {
			lv = "info"
		}
		ts := ""
		if t, err := time.Parse(time.RFC3339, a.StartsAt); err == nil {
			ts = t.Local().Format("15:04")
		}
		desc := a.Annotations["description"]
		if desc == "" {
			desc = a.Annotations["summary"]
		}
		out = append(out, model.Alert{Lv: lv, Name: a.Labels["alertname"], Inst: a.Labels["instance"], Time: ts, Desc: desc})
	}
	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Lv] < rank[out[j].Lv] })
	return out, nil
}

// ---------------------------------------------------------------- 热点 ----

// Hot 汇总 PD 的读写热点 Region，并用 information_schema.tidb_hot_regions 补充表名。
func (c *Collector) Hot(ctx context.Context) (list []model.HotRegion, storeWrite map[uint64]float64, err error) {
	storeWrite = map[uint64]float64{}
	tables := map[uint64]string{}
	if b, e := c.get(ctx, "sql/hot-regions"); e == nil {
		_, rows := parseTSV(b)
		for _, r := range rows {
			var id uint64
			fmt.Sscan(r["region_id"], &id)
			tables[id] = r["table_name"]
		}
	}
	// Region 的起止 Key：热点统计本身通常不带，依次从写 / 读流量 Top 与 Region 抽样里补
	keys := map[uint64][2]string{}
	for _, name := range []string{"pd/regions-sample", "pd/regions-readflow", "pd/regions-writeflow"} { // 后者覆盖前者
		if b, e := c.get(ctx, name); e == nil {
			if m, e := ParseRegionKeys(b); e == nil {
				for id, k := range m {
					keys[id] = k
				}
			}
		}
	}
	for _, kind := range []struct{ name, label string }{{"pd/hot-write", "写热点"}, {"pd/hot-read", "读热点"}} {
		b, e := c.get(ctx, kind.name)
		if e != nil {
			err = fmt.Errorf("%s: %w", kind.name, e)
			continue
		}
		m, e := ParseHot(b)
		if e != nil {
			err = e
			continue
		}
		for sid, sh := range m {
			if kind.label == "写热点" {
				storeWrite[sid] = sh.FlowMBs
			}
			for _, r := range sh.Regions {
				k := keys[r.ID]
				if r.StartKey != "" || r.EndKey != "" {
					k = [2]string{r.StartKey, r.EndKey}
				}
				list = append(list, model.HotRegion{ID: r.ID, Kind: kind.label, Store: sid, Table: tables[r.ID], Flow: round1(r.Flow), Range: RegionRange(k[0], k[1])})
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Flow > list[j].Flow })
	return
}

// ---------------------------------------------------------------- 总览 ----

// Overview 计算总览 KPI 与趋势数据。
func (c *Collector) Overview(ctx context.Context, topo model.Topology, alerts []model.Alert) (model.Overview, model.Trends, []string) {
	var ov model.Overview
	var tr model.Trends
	var errs []string
	series := func(name string) *Series {
		b, err := c.get(ctx, name)
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			return nil
		}
		s, err := ParseMatrix(b)
		if err != nil || len(s) == 0 {
			if err != nil {
				errs = append(errs, name+": "+err.Error())
			}
			return nil
		}
		return &s[0]
	}
	if s := series("prom/qps"); s != nil {
		ov.QPS = round1(s.Last(3))
		if f := s.First(3); f > 0 {
			ov.QPSChangePct = round1((s.Last(3) - f) / f * 100)
		}
		tr.QPS = values(*s)
		if len(s.Points) > 1 {
			tr.Start, tr.Step = s.Points[0].T, int(s.Points[1].T-s.Points[0].T)
		}
	}
	if s := series("prom/p99"); s != nil {
		ov.P99ms = round1(s.Last(3) * 1000)
		ov.P99ChangeMs = round1((s.Last(3) - s.First(3)) * 1000)
		for _, p := range s.Points {
			tr.P99ms = append(tr.P99ms, round1(p.V*1000))
		}
	}
	if b, err := c.get(ctx, "prom/tikv-write-bytes"); err == nil {
		if ss, err := ParseMatrix(b); err == nil {
			for _, x := range ss {
				host := x.Metric["instance"]
				if i := strings.LastIndex(host, ":"); i >= 0 {
					host = host[:i]
				}
				vals := make([]float64, len(x.Points))
				for i, p := range x.Points {
					vals[i] = round1(p.V / 1048576)
				}
				tr.StoreWrite = append(tr.StoreWrite, model.StoreSeries{Inst: host, Vals: vals})
			}
			sort.Slice(tr.StoreWrite, func(i, j int) bool { return tr.StoreWrite[i].Inst < tr.StoreWrite[j].Inst })
		}
	}
	if s := series("prom/connections"); s != nil {
		ov.Connections = round1(s.Last(3))
		tr.Conns = values(*s)
	}
	if b, err := c.get(ctx, "pd/stores"); err == nil {
		stores, _ := ParseStores(b)
		for _, s := range stores {
			ov.StoresTotal++
			if s.State == "Up" {
				ov.StoresUp++
			}
			if !s.IsFlash() {
				ov.StorageUsedGB += s.CapacityGB - s.AvailGB
				ov.StorageCapGB += s.CapacityGB
			}
		}
		ov.StorageUsedGB, ov.StorageCapGB = round1(ov.StorageUsedGB), round1(ov.StorageCapGB)
	}
	if b, err := c.get(ctx, "pd/stats-region"); err == nil {
		var r struct {
			Count int `json:"count"`
		}
		if json.Unmarshal(b, &r) == nil {
			ov.Regions = r.Count
		}
	}
	for _, a := range alerts {
		switch a.Lv {
		case "critical":
			ov.AlertsCrit++
		case "warning":
			ov.AlertsWarn++
		}
	}
	return ov, tr, errs
}

func values(s Series) []float64 {
	out := make([]float64, len(s.Points))
	for i, p := range s.Points {
		out[i] = round1(p.V)
	}
	return out
}

// ---------------------------------------------------------------- 环境探测（B8）----

var (
	reTZ      = regexp.MustCompile(`Time zone:\s*(.+)`)
	rePretty  = regexp.MustCompile(`(?m)^PRETTY_NAME="?([^"\n]+)"?`)
	reMemLine = regexp.MustCompile(`(?m)^Mem:\s+(\d+)`)
	reTiupVer = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)
)

// Env 从中控机相关条目探测环境信息。键与前端「环境与设置」页的项目一致；取不到的项 Detected=false。
func (c *Collector) Env(ctx context.Context, topo model.Topology) []model.EnvItem {
	text := func(name string) (string, bool) {
		b, err := c.get(ctx, name)
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(b)), true
	}
	var out []model.EnvItem
	add := func(key, val string, ok bool, cmd string) {
		out = append(out, model.EnvItem{Key: key, Value: val, Detected: ok && val != "", Cmd: cmd})
	}
	if v, ok := text("ctl/hostname"); ok {
		add("ctl.host", strings.Fields(v + " ")[0], true, "hostname -I")
	} else {
		add("ctl.host", "", false, "hostname -I")
	}
	if v, ok := text("ctl/os-release"); ok {
		if m := rePretty.FindStringSubmatch(v); m != nil {
			v = m[1]
		}
		add("ctl.os", v, true, "cat /etc/os-release")
	} else {
		add("ctl.os", "", false, "cat /etc/os-release")
	}
	v, ok := text("ctl/kernel")
	add("ctl.kernel", v, ok, "uname -r")
	nproc, ok1 := text("ctl/nproc")
	free, ok2 := text("ctl/free")
	res := ""
	if ok1 && ok2 {
		mem := ""
		if m := reMemLine.FindStringSubmatch(free); m != nil {
			mem = fmt.Sprintf("%.0f GiB", f64(m[1])/1024)
		}
		res = nproc + " 核 / " + mem
	}
	add("ctl.res", res, ok1 && ok2, "nproc; free -m")
	tz, ok := text("ctl/timedatectl")
	if m := reTZ.FindStringSubmatch(tz); m != nil {
		tz = strings.TrimSpace(m[1])
	} else {
		ok = false
	}
	add("ctl.tz", tz, ok, "timedatectl")
	add("ctl.user", topo.User, topo.User != "", "tiup cluster display 的 Deploy user")
	if v, ok := text("tiup/version"); ok {
		if m := reTiupVer.FindStringSubmatch(v); m != nil {
			v = "v" + m[1]
		}
		add("tiup.ver", v, true, "tiup --version")
	} else {
		add("tiup.ver", "", false, "tiup --version")
	}
	if v, ok := text("tiup/cluster-version"); ok {
		if m := reTiupVer.FindStringSubmatch(v); m != nil {
			v = "v" + m[1]
		}
		add("tiup.cluster", v, true, "tiup cluster --version")
	} else {
		add("tiup.cluster", "", false, "tiup cluster --version")
	}
	if v, ok := text("tiup/cluster-list"); ok {
		add("cl.list", strings.Join(source.ParseClusterList(v), "、"), true, "tiup cluster list")
	} else {
		add("cl.list", "", false, "tiup cluster list")
	}
	add("cl.name", topo.Cluster, topo.Cluster != "", "tiup cluster list")
	add("cl.ver", topo.Version, topo.Version != "", "tiup cluster display <集群名>（Cluster version）")
	counts := map[string]int{}
	for _, in := range topo.Instances {
		counts[in.Type]++
	}
	inst := fmt.Sprintf("TiDB %d · PD %d · TiKV %d · TiFlash %d · TiCDC %d", counts["tidb"], counts["pd"], counts["tikv"], counts["tiflash"], counts["ticdc"])
	add("cl.inst", inst, len(topo.Instances) > 0, "tiup cluster display <集群名>")
	region := ""
	if b, err := c.get(ctx, "pd/stats-region"); err == nil {
		var r struct {
			Count int `json:"count"`
		}
		if json.Unmarshal(b, &r) == nil && r.Count > 0 {
			region = "约 " + commas(r.Count)
		}
	}
	add("cl.region", region, region != "", "GET /pd/api/v1/stats/region")
	if b, err := c.get(ctx, "tiup/cluster-display"); err == nil {
		ci := source.ParseDisplay(string(b))
		ep, _, _ := ci.Endpoints()
		add("addr.pd", ep["pd"], ep["pd"] != "", "tiup cluster display → pd")
		add("addr.prom", ep["prom"], ep["prom"] != "", "tiup cluster display → prometheus")
		add("addr.grafana", ci.GrafanaURL, ci.GrafanaURL != "", "tiup cluster display → Grafana URL")
		add("addr.alert", ep["alert"], ep["alert"] != "", "tiup cluster display → alertmanager")
	}
	return out
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
