// @author: kevinkelly
// @license: Apache License 2.0
// @file: window_test.go
// @time: 2026/10/28

package collect

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tidbops/internal/source"
)

// recSource 记录每次取数的条目（只记路径与名称），返回固定内容。
type recSource struct {
	mu   sync.Mutex
	seen []source.Item
}

func (r *recSource) Mode() string { return "live" }
func (r *recSource) Fetch(ctx context.Context, it source.Item) ([]byte, error) {
	r.mu.Lock()
	r.seen = append(r.seen, it)
	r.mu.Unlock()
	return []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil
}
func (r *recSource) last() source.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[len(r.seen)-1]
}

// rangeOf 取出 query_range 路径里的 start / end / step / query。
func rangeOf(t *testing.T, path string) (start, end, step int64, query string) {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatalf("路径不合法 %q: %v", path, err)
	}
	q := u.Query()
	start, _ = strconv.ParseInt(q.Get("start"), 10, 64)
	end, _ = strconv.ParseInt(q.Get("end"), 10, 64)
	step, _ = strconv.ParseInt(q.Get("step"), 10, 64)
	return start, end, step, q.Get("query")
}

// 真实连接模式的回归：目录只在创建时生成一次（start / end 是那一刻的绝对时间），
// 之后每次取数都要把窗口平移到「现在」，否则 QPS / P99 等趋势永远停在创建时刻。
func TestPrometheusWindowFollowsClock(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 14, 18, 9, 0, time.UTC)
	rec := &recSource{}
	c := New(rec, source.CatalogParams{Cluster: "c1", Home: "/home/tidb", Now: t0})
	clock := t0
	c.Now = func() time.Time { return clock }

	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		width int64 // 窗口长度（秒）
		step  int64
	}{
		{"prom/qps", 3600, 60},               // 趋势：最近 1 小时
		{"prom/p99", 3600, 60},               // 趋势
		{"prom/tidb-qps-instance", 3600, 60}, // 各 TiDB 的 QPS
		{"prom/connections", 3600, 60},       // 连接数
	} {
		if _, err := c.get(ctx, tc.name); err != nil {
			t.Fatal(err)
		}
		s0, e0, st0, q0 := rangeOf(t, rec.last().Path)
		if e0 != t0.Unix() || e0-s0 != tc.width || st0 != tc.step {
			t.Fatalf("%s 创建时刻的窗口不对: start=%d end=%d step=%d", tc.name, s0, e0, st0)
		}

		clock = t0.Add(65 * time.Minute) // 过去一个多小时（对应「15:19 还在用 14:18 的窗口」）
		if _, err := c.get(ctx, tc.name); err != nil {
			t.Fatal(err)
		}
		s1, e1, st1, q1 := rangeOf(t, rec.last().Path)
		if e1 != clock.Unix() {
			t.Errorf("%s: end 应跟随当前时间 %d，实际 %d（窗口被冻住了）", tc.name, clock.Unix(), e1)
		}
		if e1-s1 != tc.width || st1 != tc.step {
			t.Errorf("%s: 窗口长度 / 步长应保持 %d / %d，实际 %d / %d", tc.name, tc.width, tc.step, e1-s1, st1)
		}
		if q1 != q0 || q1 == "" {
			t.Errorf("%s: 查询表达式不应变化: %q → %q", tc.name, q0, q1)
		}
		clock = t0 // 复位，下一个条目重新从创建时刻开始
	}
}

// 容量预测的窗口（7 天、每小时一个点）同样要跟着走，并保持长度与步长。
func TestForecastWindowFollowsClock(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 14, 18, 9, 0, time.UTC)
	rec := &recSource{}
	c := New(rec, source.CatalogParams{Cluster: "c1", Home: "/home/tidb", Now: t0})
	var name string
	for n, it := range c.Items {
		if strings.HasPrefix(n, "prom/") && strings.Contains(it.Path, "step=3600") {
			name = n
			break
		}
	}
	if name == "" {
		t.Skip("目录里没有容量预测条目")
	}
	c.Now = func() time.Time { return t0.Add(3 * time.Hour) }
	if _, err := c.get(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	s, e, step, _ := rangeOf(t, rec.last().Path)
	if e != t0.Add(3*time.Hour).Unix() || e-s != 7*24*3600 || step != 3600 {
		t.Errorf("预测窗口应平移并保持 7 天 / 3600：start=%d end=%d step=%d", s, e, step)
	}
}

func TestSlideWindowLeavesOtherItemsAlone(t *testing.T) {
	now := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)
	for name, it := range map[string]source.Item{
		"PD 接口":          {Name: "pd/stats", Kind: source.KindHTTP, Endpoint: "pd", Path: "/pd/api/v1/stats/region"},
		"PD 里碰巧有 start":  {Name: "pd/x", Kind: source.KindHTTP, Endpoint: "pd", Path: "/pd/api/v1/x?start=1&end=2"},
		"Prom 即时查询":      {Name: "prom/i", Kind: source.KindHTTP, Endpoint: "prom", Path: "/api/v1/query?query=up"},
		"Prom 范围查询缺 end": {Name: "prom/r", Kind: source.KindHTTP, Endpoint: "prom", Path: "/api/v1/query_range?query=up&start=100&step=60"},
		"Prom 范围查询窗口为空":  {Name: "prom/r2", Kind: source.KindHTTP, Endpoint: "prom", Path: "/api/v1/query_range?query=up&start=200&end=100&step=60"},
		"SQL":            {Name: "sql/v", Kind: source.KindSQL, Query: "SELECT 1"},
		"命令":             {Name: "cmd/h", Kind: source.KindCmd, Argv: []string{"hostname"}},
	} {
		if got := slideWindow(it, now); got.Path != it.Path || got.Query != it.Query || got.Name != it.Name {
			t.Errorf("%s 不应被改动: %+v → %+v", name, it, got)
		}
	}
	// 表达式里带 & 与 start= 字样（已被 URL 编码）时，只改真正的参数
	enc := "/api/v1/query_range?query=" + url.QueryEscape(`sum(rate(x{a="&start=1&end=2"}[1m]))`) + "&start=1000&end=4600&step=60"
	got := slideWindow(source.Item{Name: "prom/e", Kind: source.KindHTTP, Endpoint: "prom", Path: enc}, now)
	s, e, step, q := rangeOf(t, got.Path)
	if e != now.Unix() || e-s != 3600 || step != 60 || q != `sum(rate(x{a="&start=1&end=2"}[1m]))` {
		t.Errorf("表达式里的 &start= 不能被误改: start=%d end=%d step=%d query=%q", s, e, step, q)
	}
}

// 没有时钟（Collector{} 字面量，诊断单测里会这样用）时退回系统时间，不 panic。
func TestGetWithoutClockDoesNotPanic(t *testing.T) {
	rec := &recSource{}
	c := &Collector{Src: rec, Items: map[string]source.Item{
		"prom/qps": {Name: "prom/qps", Kind: source.KindHTTP, Endpoint: "prom", Path: "/api/v1/query_range?query=up&start=1&end=3601&step=60"},
	}}
	if _, err := c.get(context.Background(), "prom/qps"); err != nil {
		t.Fatal(err)
	}
	_, e, _, _ := rangeOf(t, rec.last().Path)
	if time.Since(time.Unix(e, 0)) > time.Minute {
		t.Errorf("没有时钟时应取系统当前时间，实际 end=%d", e)
	}
}
