// @author: kevinkelly
// @license: Apache License 2.0
// @file: live.go
// @time: 2026/10/28

package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ExecFunc 执行一个已通过白名单校验的命令（不经过 shell）。可在测试中替换。
type ExecFunc func(ctx context.Context, argv []string, env []string) ([]byte, error)

// ReadFileFunc 读取一个已通过白名单校验的文件。可在测试中替换。
type ReadFileFunc func(path string) ([]byte, error)

// MySQLConfig 是只读 SQL 的连接参数。密码不放在命令行，而是通过环境变量传给 mysql 客户端。
type MySQLConfig struct {
	Bin      string // mysql 客户端路径，默认 mysql
	Host     string
	Port     int
	User     string
	Password string
}

// LiveConfig 是 Live 的配置。
type LiveConfig struct {
	Endpoints map[string]string // pd / tidb / prom / alert → 基础 URL
	MySQL     MySQLConfig
	TiupBin   string        // tiup 可执行文件路径，默认 tiup
	Timeout   time.Duration // 单个请求超时，默认 20s
	MaxBody   int64         // 单个响应最大字节数，默认 32 MiB
	Exec      ExecFunc
	ReadFile  ReadFileFunc
	Client    *http.Client
}

// Live 是真实访问的 Source。它只会发出通过 ValidateItem 的请求：
// HTTP 只用 GET 且不跟随重定向；命令不经过 shell；SQL 只能是受限的 SELECT。
type Live struct {
	cfg LiveConfig
	// Executed 记录实际发出过的请求（逻辑描述），用于生成只读声明。
	Executed []string
}

// NewLive 创建 Live，并补全默认值。
func NewLive(cfg LiveConfig) *Live {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 32 << 20
	}
	if cfg.TiupBin == "" {
		cfg.TiupBin = "tiup"
	}
	if cfg.MySQL.Bin == "" {
		cfg.MySQL.Bin = "mysql"
	}
	if cfg.Exec == nil {
		cfg.Exec = defaultExec(cfg.MaxBody)
	}
	if cfg.ReadFile == nil {
		cfg.ReadFile = os.ReadFile
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Live{cfg: cfg}
}

// Mode 实现 Source。
func (l *Live) Mode() string { return "live" }

// Fetch 实现 Source。
func (l *Live) Fetch(ctx context.Context, it Item) ([]byte, error) {
	if err := ValidateItem(it); err != nil {
		return nil, fmt.Errorf("拒绝执行: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, l.cfg.Timeout)
	defer cancel()
	l.Executed = append(l.Executed, it.Request())
	switch it.Kind {
	case KindHTTP:
		return l.fetchHTTP(ctx, it)
	case KindCmd:
		argv := append([]string(nil), it.Argv...)
		if argv[0] == "tiup" {
			argv[0] = l.cfg.TiupBin
		}
		return l.cfg.Exec(ctx, argv, nil)
	case KindSQL:
		return l.fetchSQL(ctx, it)
	case KindFile:
		return l.cfg.ReadFile(it.File)
	}
	return nil, fmt.Errorf("未知种类 %q", it.Kind)
}

func (l *Live) fetchHTTP(ctx context.Context, it Item) ([]byte, error) {
	base := strings.TrimRight(l.cfg.Endpoints[it.Endpoint], "/")
	if base == "" {
		return nil, fmt.Errorf("端点 %q 未配置", it.Endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+it.Path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", it.Endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, l.cfg.MaxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > l.cfg.MaxBody {
		return nil, fmt.Errorf("响应超过上限 %d 字节", l.cfg.MaxBody)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func (l *Live) fetchSQL(ctx context.Context, it Item) ([]byte, error) {
	m := l.cfg.MySQL
	if m.Host == "" || m.User == "" {
		return nil, fmt.Errorf("未配置只读 SQL 连接（--mysql-host / --mysql-user）")
	}
	port := m.Port
	if port == 0 {
		port = 4000
	}
	// --batch：输出 TSV，并转义换行与制表符，保证一行一条记录。
	argv := []string{m.Bin, "-h", m.Host, "-P", fmt.Sprint(port), "-u", m.User, "--batch", "--connect-timeout=5", "-e", it.Query}
	var env []string
	if m.Password != "" {
		env = []string{"MYSQL_PWD=" + m.Password}
	}
	return l.cfg.Exec(ctx, argv, env)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DefaultExec 返回直接执行命令的 ExecFunc（供需要在其外面再包一层的调用方使用，如中控机模式把 tiup 命令改走执行器）。
func DefaultExec(max int64) ExecFunc { return defaultExec(max) }

// defaultExec 直接执行命令（exec.Command，不经过 shell），并限制输出大小。
func defaultExec(max int64) ExecFunc {
	return func(ctx context.Context, argv []string, env []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), env...)
		var out, errb bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, max: max}
		cmd.Stderr = &limitWriter{w: &errb, max: 8 << 10}
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%s 执行失败: %w: %s", argv[0], err, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), nil
	}
}

type limitWriter struct {
	w   *bytes.Buffer
	max int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(l.w.Len()+len(p)) > l.max {
		return 0, fmt.Errorf("输出超过上限 %d 字节", l.max)
	}
	return l.w.Write(p)
}
