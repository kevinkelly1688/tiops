// @author: kevinkelly
// @license: Apache License 2.0
// @file: collector.go
// @time: 2026/10/28

// Package collect 把 Source 返回的原始响应（PD API、Prometheus、tiup 输出、只读 SQL 的 TSV 等）
// 解析成平台的数据模型（model 包），并在其上做基础诊断。
//
// 它只依赖 source.Source 接口：开发 / 测试用 Replay，真实环境用 Live，业务代码完全相同。
package collect

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"tidbops/internal/model"
	"tidbops/internal/source"
)

// Collector 从 Source 采集并解析数据。
type Collector struct {
	Src   source.Source
	Items map[string]source.Item
	Now   func() time.Time
	// Params 是创建时的目录参数（集群名、tiup 主目录等）。
	Params source.CatalogParams
	// NoSustain 为 true 时诊断项不做「持续一段时间才按原级别提示」的判定（测试 / 一次性离线分析用）。
	NoSustain bool

	tr    tracker    // 诊断项首次出现时间（持续时间判定）
	fcMu  sync.Mutex // 容量预测缓存
	fcAt  time.Time
	fcVal model.Forecast
}

// New 创建 Collector。params 决定采集目录（集群名、目录等），Replay 模式下只用条目名。
func New(src source.Source, p source.CatalogParams) *Collector {
	items := map[string]source.Item{}
	for _, it := range source.DefaultCatalog(p) {
		items[it.Name] = it
	}
	return &Collector{Src: src, Items: items, Now: time.Now, Params: p}
}

func (c *Collector) get(ctx context.Context, name string) ([]byte, error) {
	it, ok := c.Items[name]
	if !ok {
		it = source.Item{Name: name}
	} else {
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		it = slideWindow(it, now())
	}
	return c.Src.Fetch(ctx, it)
}

var (
	reWinStart = regexp.MustCompile(`([?&])start=(\d+)`)
	reWinEnd   = regexp.MustCompile(`([?&])end=(\d+)`)
)

// slideWindow 把 Prometheus 范围查询（query_range）的 start / end 平移到 now，窗口长度与步长不变。
// 目录（source.DefaultCatalog）在创建 Collector 时只生成一次，里面的 start / end 是「那一刻」的绝对时间；
// 不平移的话，真实连接模式下趋势窗口永远停在创建时刻，QPS / P99 等随时间变旧（回放模式按条目名取数，不受影响）。
// 其他条目（PD / TiDB / SQL / 命令，以及 Prometheus 的即时查询）原样返回。
func slideWindow(it source.Item, now time.Time) source.Item {
	if it.Kind != source.KindHTTP || it.Endpoint != "prom" || !strings.HasPrefix(it.Path, "/api/v1/query_range?") {
		return it
	}
	ms, me := reWinStart.FindStringSubmatch(it.Path), reWinEnd.FindStringSubmatch(it.Path)
	if ms == nil || me == nil {
		return it
	}
	s, err1 := strconv.ParseInt(ms[2], 10, 64)
	e, err2 := strconv.ParseInt(me[2], 10, 64)
	if err1 != nil || err2 != nil || e <= s {
		return it
	}
	end := now.Unix()
	start := end - (e - s)
	p := reWinStart.ReplaceAllString(it.Path, "${1}start="+strconv.FormatInt(start, 10))
	it.Path = reWinEnd.ReplaceAllString(p, "${1}end="+strconv.FormatInt(end, 10))
	return it
}

// ---------------------------------------------------------------- 通用解析工具 ----

func f64(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

// parseTSV 解析 mysql --batch 的输出：第一行为表头，其后每行一条记录。
func parseTSV(b []byte) (header []string, rows []map[string]string) {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, nil
	}
	header = strings.Split(lines[0], "\t")
	for _, ln := range lines[1:] {
		if ln == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		row := make(map[string]string, len(header))
		for i, h := range header {
			if i < len(f) {
				row[strings.ToLower(h)] = unescapeTSV(f[i])
			}
		}
		rows = append(rows, row)
	}
	return
}

func unescapeTSV(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\\`, `\`)
	return r.Replace(s)
}

// sizeGiB 把 PD 返回的容量字符串（如 "3.5TiB"、"2230GiB"、"0B"）换算为 GiB。
func sizeGiB(s string) float64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suf string
		mul float64
	}{{"PiB", 1024 * 1024}, {"TiB", 1024}, {"GiB", 1}, {"MiB", 1.0 / 1024}, {"KiB", 1.0 / 1048576}, {"B", 1.0 / 1073741824}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			return f64(strings.TrimSuffix(s, u.suf)) * u.mul
		}
	}
	return f64(s)
}

func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		addr = addr[:i]
	}
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	return addr
}

func human(bytes float64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GiB", bytes/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0f MiB", bytes/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0f KiB", bytes/(1<<10))
	}
	return fmt.Sprintf("%.0f B", bytes)
}
