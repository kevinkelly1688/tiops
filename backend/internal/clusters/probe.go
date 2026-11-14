// @author: kevinkelly
// @license: Apache License 2.0
// @file: probe.go
// @time: 2026/10/28

package clusters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"

	"tidbops/internal/source"
)

// TestItem 是连通性测试的一项。status：ok | warn | bad。
type TestItem struct {
	Item   string `json:"item"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	MS     int64  `json:"ms"`
}

// ProbeTimeout 是单项探测的超时。
const ProbeTimeout = 10 * time.Second

// ExecFunc 执行一个命令（不经过 shell）。env 形如 "KEY=VALUE"。
type ExecFunc func(ctx context.Context, argv []string, env []string) ([]byte, error)

// Prober 做只读连通性探测。零值可用（真实 HTTP 客户端与 os/exec）。
//
// 探测全部是只读的、固定的：PD 的 /pd/api/v1/health、Prometheus 的即时查询 up、Alertmanager 的 /api/v2/status、
// TiDB 状态端口 /status、mysql -e "SELECT 1"、tiup cluster display <name>。不跟随重定向；响应体最多读 1 MiB；
// 返回的 detail 里不会出现数据库密码。
type Prober struct {
	HTTP *http.Client
	Exec ExecFunc
	// 继承的默认值（集群自己没配时使用，对应启动参数 --tiup / --mysql-bin）。
	DefaultTiup, DefaultMySQLBin string
	Timeout                      time.Duration
	// NoTiup 独立部署：本机不是集群的中控机，不探测 tiup。
	NoTiup bool
}

func (p Prober) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return ProbeTimeout
}

func (p Prober) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (p Prober) exec() ExecFunc {
	if p.Exec != nil {
		return p.Exec
	}
	return func(ctx context.Context, argv []string, env []string) ([]byte, error) {
		cmd := osexec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), env...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &capWriter{w: &out, max: 1 << 20}, &capWriter{w: &errb, max: 8 << 10}
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg != "" {
				return out.Bytes(), fmt.Errorf("%w: %s", err, msg)
			}
			return out.Bytes(), err
		}
		return out.Bytes(), nil
	}
}

type capWriter struct {
	w   *bytes.Buffer
	max int
}

func (c *capWriter) Write(b []byte) (int, error) {
	if room := c.max - c.w.Len(); room > 0 {
		if len(b) > room {
			c.w.Write(b[:room])
		} else {
			c.w.Write(b)
		}
	}
	return len(b), nil // 超出部分丢弃，不让子进程因管道写失败
}

func scrub(s Spec, text string) string {
	if s.MySQLPassword != "" {
		text = strings.ReplaceAll(text, s.MySQLPassword, "***")
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return strings.Join(strings.Fields(text), " ")
}

// Run 对一个集群做全部探测。
func (p Prober) Run(ctx context.Context, s Spec) []TestItem {
	if s.Mode == ModeReplay {
		return []TestItem{p.timed("回放数据包", func() (string, string) {
			rp, err := source.NewReplay(s.Fixtures)
			if err != nil {
				return "bad", "无法读取 manifest：" + scrub(s, err.Error())
			}
			m := rp.Manifest()
			return "ok", fmt.Sprintf("manifest 可读：集群 %s %s，采集于 %s，%d 个条目", m.Cluster, m.ClusterVersion, m.CollectedAt.Format("2006-01-02 15:04"), len(m.Items))
		})}
	}
	var out []TestItem
	http1 := func(label, base, path string, check func(status int, body []byte) (string, string)) {
		if base == "" {
			out = append(out, TestItem{Item: label, Status: "warn", Detail: "未配置地址（启动时会尝试自动探测）"})
			return
		}
		out = append(out, p.timed(label, func() (string, string) {
			cctx, cancel := context.WithTimeout(ctx, p.timeout())
			defer cancel()
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+path, nil)
			if err != nil {
				return "bad", "请求构造失败"
			}
			req.Header.Set("Accept", "application/json")
			resp, err := p.client().Do(req)
			if err != nil {
				return "bad", scrub(s, err.Error())
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return check(resp.StatusCode, body)
		}))
	}
	http1("PD 健康（/pd/api/v1/health）", s.PD, "/pd/api/v1/health", func(code int, body []byte) (string, string) {
		if code != http.StatusOK {
			return "bad", "HTTP " + strconv.Itoa(code)
		}
		var hs []struct {
			Health bool `json:"health"`
		}
		if json.Unmarshal(body, &hs) != nil {
			return "warn", "HTTP 200，但响应不是预期的健康列表"
		}
		bad := 0
		for _, h := range hs {
			if !h.Health {
				bad++
			}
		}
		switch {
		case len(hs) == 0:
			return "warn", "没有返回任何 PD 成员"
		case bad > 0:
			return "warn", fmt.Sprintf("%d / %d 个 PD 成员不健康", bad, len(hs))
		}
		return "ok", fmt.Sprintf("%d 个 PD 成员健康", len(hs))
	})
	http1("TiDB 状态端口（/status）", s.TiDBStatus, "/status", func(code int, body []byte) (string, string) {
		if code != http.StatusOK {
			return "bad", "HTTP " + strconv.Itoa(code)
		}
		return "ok", "HTTP 200"
	})
	http1("Prometheus 查询", s.Prom, "/api/v1/query?"+url.Values{"query": {"up"}}.Encode(), func(code int, body []byte) (string, string) {
		if code != http.StatusOK {
			return "bad", "HTTP " + strconv.Itoa(code)
		}
		var r struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &r) != nil || r.Status != "success" {
			return "bad", "查询未返回 success"
		}
		return "ok", "查询 up 成功"
	})
	http1("Alertmanager（/api/v2/status）", s.Alert, "/api/v2/status", func(code int, body []byte) (string, string) {
		if code != http.StatusOK {
			return "bad", "HTTP " + strconv.Itoa(code)
		}
		return "ok", "HTTP 200"
	})
	// TiDB SQL
	if s.MySQLHost == "" || s.MySQLUser == "" {
		out = append(out, TestItem{Item: "TiDB SQL（SELECT 1）", Status: "warn", Detail: "未配置 mysqlHost / mysqlUser"})
	} else {
		out = append(out, p.timed("TiDB SQL（SELECT 1）", func() (string, string) {
			bin := s.MySQLBin
			if bin == "" {
				bin = p.DefaultMySQLBin
			}
			if bin == "" {
				bin = "mysql"
			}
			port := s.MySQLPort
			if port == 0 {
				port = 4000
			}
			cctx, cancel := context.WithTimeout(ctx, p.timeout())
			defer cancel()
			argv := []string{bin, "-h", s.MySQLHost, "-P", strconv.Itoa(port), "-u", s.MySQLUser, "--batch", "--connect-timeout=5", "-e", "SELECT 1"}
			var env []string
			if s.MySQLPassword != "" {
				env = []string{"MYSQL_PWD=" + s.MySQLPassword}
			}
			if _, err := p.exec()(cctx, argv, env); err != nil {
				return "bad", scrub(s, err.Error())
			}
			return "ok", "SELECT 1 成功"
		}))
	}
	// tiup
	if p.NoTiup {
		return out
	}
	out = append(out, p.timed("tiup cluster display "+s.Name, func() (string, string) {
		bin := s.Tiup
		if bin == "" {
			bin = p.DefaultTiup
		}
		if bin == "" {
			bin = "tiup"
		}
		cctx, cancel := context.WithTimeout(ctx, p.timeout())
		defer cancel()
		b, err := p.exec()(cctx, []string{bin, "cluster", "display", s.Name}, nil)
		if err != nil {
			return "bad", scrub(s, err.Error())
		}
		for _, ln := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "Cluster version:"); ok {
				return "ok", "可执行，集群版本 " + strings.TrimSpace(v)
			}
		}
		return "ok", "可执行"
	}))
	return out
}

func (p Prober) timed(label string, f func() (status, detail string)) TestItem {
	t0 := time.Now()
	st, d := f()
	return TestItem{Item: label, Status: st, Detail: d, MS: time.Since(t0).Milliseconds()}
}
