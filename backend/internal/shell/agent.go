// @author: kevinkelly
// @license: Apache License 2.0
// @file: agent.go
// @time: 2026/10/28

package shell

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Agent 是平台进程调用子进程入口（ServeChild）的通道。emit 在 run 模式下按行收到已遮蔽的输出（可为 nil）。
type Agent interface {
	Call(ctx context.Context, req ChildReq, emit func(line string)) (ChildRes, error)
}

// AgentError 表示通道本身出了问题（sudoers 没放行、子进程没起来、协议异常），而不是命令失败。
type AgentError struct{ Msg string }

func (e *AgentError) Error() string { return e.Msg }

// SudoAgent 经 sudo -n -u <User> <Bin> shell-exec --stdin 调用子进程（sudoers 只放行这一条固定命令）。
// 请求 JSON 写入子进程 stdin 并保持打开：ctx 取消时关掉 stdin 并发 SIGTERM，子进程据此终止命令并返回退出码 143。
type SudoAgent struct {
	Sudo string // 默认 sudo
	User string // 目标用户（tidb）
	Bin  string // 平台二进制的绝对路径
}

var agentUserRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,31}$`)

func (a SudoAgent) Call(ctx context.Context, req ChildReq, emit func(string)) (ChildRes, error) {
	if !agentUserRe.MatchString(a.User) {
		return ChildRes{}, &AgentError{"sudo 目标用户未配置或不合法"}
	}
	if a.Bin == "" || strings.HasPrefix(a.Bin, "-") || strings.ContainsAny(a.Bin, " \t\r\n\x00") {
		return ChildRes{}, &AgentError{"平台二进制路径未配置或不合法"}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return ChildRes{}, &AgentError{"序列化请求失败"}
	}
	sudo := a.Sudo
	if sudo == "" {
		sudo = "sudo"
	}
	cmd := exec.CommandContext(ctx, sudo, "-n", "-u", a.User, a.Bin, "shell-exec", "--stdin")
	cmd.Env = []string{"LANG=C.UTF-8", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if p := os.Getenv("PATH"); p != "" {
		cmd.Env[1] = "PATH=" + p
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ChildRes{}, &AgentError{err.Error()}
	}
	cmd.Cancel = func() error {
		_ = stdin.Close()
		return termProcess(cmd.Process)
	}
	cmd.WaitDelay = 10 * time.Second
	var stderr limitedBuf
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ChildRes{}, &AgentError{err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return ChildRes{}, &AgentError{"无法启动 sudo：" + err.Error()}
	}
	if _, err := stdin.Write(payload); err == nil {
		_, _ = stdin.Write([]byte("\n"))
	}
	var (
		final    *ChildRes
		protoErr string
	)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 || protoErr != "" {
			continue
		}
		var m struct {
			T    string `json:"t"`
			Line string `json:"line"`
			ChildRes
		}
		if err := json.Unmarshal(line, &m); err != nil {
			protoErr = "协议异常：无法解析子进程输出"
			continue
		}
		switch {
		case final != nil:
			protoErr = "协议异常：result 之后仍有输出"
		case m.T == "out":
			if emit != nil {
				emit(m.Line)
			}
		case m.T == "result":
			r := m.ChildRes
			final = &r
		}
	}
	_ = stdin.Close()
	werr := cmd.Wait()
	if final != nil && protoErr == "" {
		return *final, nil
	}
	if protoErr != "" {
		return ChildRes{}, &AgentError{protoErr}
	}
	msg := strings.TrimSpace(stderr.String())
	switch {
	case ctx.Err() != nil:
		return ChildRes{}, &AgentError{"子进程在返回结果前被终止：" + ctx.Err().Error()}
	case strings.Contains(msg, "a password is required") || strings.Contains(msg, "not allowed") || strings.Contains(msg, "may not run sudo"):
		return ChildRes{}, &AgentError{"sudoers 没有放行 shell-exec 入口（/etc/sudoers.d/tidbops 需要随本版本重新生成，见部署手册）：" + msg}
	case msg != "":
		return ChildRes{}, &AgentError{"子进程异常退出：" + msg}
	case werr != nil:
		return ChildRes{}, &AgentError{"子进程异常退出：" + werr.Error()}
	}
	return ChildRes{}, &AgentError{"子进程没有返回结果"}
}

// limitedBuf 只保留 stderr 的前 1 KB。
type limitedBuf struct{ b bytes.Buffer }

func (l *limitedBuf) Write(p []byte) (int, error) {
	if room := 1024 - l.b.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		l.b.Write(p)
	}
	return len(p), nil
}

func (l *limitedBuf) String() string { return l.b.String() }

// ErrNoAgent：没有执行通道（平台不是 --exec sudo 的中控机模式）。
var ErrNoAgent = errors.New("没有可用的执行通道（需要中控机模式且执行器为 --exec sudo）")

var _ io.Writer = (*limitedBuf)(nil)

func describeRes(r ChildRes) string {
	if r.Err != "" {
		return r.Err
	}
	return fmt.Sprintf("退出码 %d：%s", r.Exit, r.Out)
}
