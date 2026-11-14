// @author: kevinkelly
// @license: Apache License 2.0
// @file: sys.go
// @time: 2026/10/28

package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Sys 抽象所有与操作系统交互的副作用（外部命令、账号、属主、磁盘空间、时间），
// 便于在 Windows 开发机与单元测试里用假实现替换。
//
// 约束：Run 的 name / args 只能来自固定字面量或经过校验的值（用户名、集群名、路径），绝不经过 shell。
type Sys interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	LookPath(name string) (string, error)
	Geteuid() int
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
	// Chown 修改属主与属组；recursive 为真时递归。非 Linux 的真实实现为空操作。
	Chown(path, user, group string, recursive bool) error
	// FreeBytes 返回 path 所在文件系统的可用字节数；不支持时返回错误（调用方跳过检查）。
	FreeBytes(path string) (uint64, error)
}

// HealthFunc 请求 url（形如 http://127.0.0.1:8080/api/health），返回服务报告的版本号。
type HealthFunc func(ctx context.Context, url string) (version string, err error)

// RealSys 是真实实现。
type RealSys struct{}

func (RealSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (RealSys) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (RealSys) Geteuid() int                         { return os.Geteuid() }
func (RealSys) Now() time.Time                       { return time.Now() }

func (RealSys) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s RealSys) Chown(path, user, group string, recursive bool) error {
	if runtime.GOOS != "linux" {
		return nil // 开发机（Windows / macOS）不处理属主
	}
	args := []string{"-h"}
	if recursive {
		args = append(args, "-R")
	}
	args = append(args, user+":"+group, path)
	_, err := s.Run(context.Background(), "chown", args...)
	return err
}

// HTTPHealth 是真实的健康检查：GET url，读取 JSON 的 version 字段。
func HTTPHealth(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("健康检查返回 HTTP %d", resp.StatusCode)
	}
	var h struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h); err != nil {
		return "", fmt.Errorf("健康检查响应不是合法 JSON: %w", err)
	}
	if h.Version == "" {
		return "", fmt.Errorf("健康检查响应缺少 version 字段")
	}
	return h.Version, nil
}
