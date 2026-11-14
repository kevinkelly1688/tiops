// @author: kevinkelly
// @license: Apache License 2.0
// @file: sudo.go
// @time: 2026/10/28

package exec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	osexec "os/exec"
	"regexp"
	"strings"
	"time"
)

// SudoRunner 供面向用户的 Web 进程使用：通过
//
//	sudo -n -u <User> <Bin> exec --stdin
//
// 调用特权 exec 子进程。Request 的 JSON 写入子进程 stdin（令牌与拓扑内容不会出现在 argv 里），
// 子进程按 Serve 的行协议返回输出与结果。sudoers 只应放行这一条固定命令。
type SudoRunner struct {
	Sudo string   // sudo 可执行文件，默认 "sudo"
	User string   // 目标用户（拥有 tiup 权限的账号）
	Bin  string   // 平台二进制的绝对路径
	Args []string // Bin 之后的参数，默认 ["exec", "--stdin"]
	// Timeout 是等待子进程的总上限；0 表示该 Kind 的默认超时 + 1 分钟。
	Timeout time.Duration
}

var sudoUserRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,31}$`)

const maxProtoLine = 4 << 20

func (s SudoRunner) Run(ctx context.Context, req Request, emit func(string)) (Result, error) {
	start := time.Now()
	if !sudoUserRe.MatchString(s.User) {
		return failResult(start, fmt.Errorf("sudo 目标用户未配置或不合法"))
	}
	if s.Bin == "" || strings.HasPrefix(s.Bin, "-") || strings.ContainsAny(s.Bin, " \t\r\n\x00") {
		return failResult(start, fmt.Errorf("exec 二进制路径未配置或不合法"))
	}
	args := s.Args
	if len(args) == 0 {
		args = []string{"exec", "--stdin"}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return failResult(start, fmt.Errorf("序列化请求失败"))
	}
	total := s.Timeout
	if total <= 0 {
		total = Info(req.Cmd.Kind).Timeout + time.Minute
		if !Info(req.Cmd.Kind).Known {
			total = time.Minute
		}
	}
	cctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	argv := append([]string{"-n", "-u", s.User, s.Bin}, args...)
	cmd := osexec.CommandContext(cctx, orDefault(s.Sudo, "sudo"), argv...)
	cmd.Env = minimalEnv()
	cmd.Stdin = bytes.NewReader(payload)
	cmd.WaitDelay = 3 * time.Second
	var stderr limitedBuf
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failResult(start, err)
	}
	if err := cmd.Start(); err != nil {
		return failResult(start, fmt.Errorf("无法启动 sudo: %v", err))
	}

	var (
		final    *Result
		protoErr error
	)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), maxProtoLine)
	for sc.Scan() {
		if protoErr != nil {
			continue // 继续排空，避免子进程阻塞
		}
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m struct {
			T    string `json:"t"`
			Line string `json:"line"`
			Result
		}
		if err := json.Unmarshal(line, &m); err != nil {
			protoErr = fmt.Errorf("协议异常: 无法解析输出行")
			cancel()
			continue
		}
		switch {
		case final != nil:
			protoErr = fmt.Errorf("协议异常: result 之后仍有输出")
			cancel()
		case m.T == "out":
			if emit != nil {
				emit(m.Line)
			}
		case m.T == "result":
			r := m.Result
			final = &r
		default:
			protoErr = fmt.Errorf("协议异常: 未知消息类型")
			cancel()
		}
	}
	if err := sc.Err(); err != nil && protoErr == nil {
		protoErr = fmt.Errorf("协议异常: 读取输出失败: %v", err)
		cancel()
	}
	waitErr := cmd.Wait()

	res := Result{Exit: -1, Millis: time.Since(start).Milliseconds()}
	if final != nil {
		res = *final
	}
	switch {
	case protoErr != nil:
		res.Exit, res.Err = -1, protoErr.Error()
		return res, protoErr
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		e := fmt.Errorf("等待 exec 子进程超过 %s", total)
		res.Exit, res.Err = -1, e.Error()
		return res, e
	case ctx.Err() != nil:
		res.Exit, res.Err = -1, "执行已取消"
		return res, ctx.Err()
	case waitErr != nil:
		e := fmt.Errorf("exec 子进程异常退出: %v%s", waitErr, stderr.suffix())
		res.Exit, res.Err = -1, e.Error()
		return res, e
	case final == nil:
		e := fmt.Errorf("协议异常: 未收到 result%s", stderr.suffix())
		res.Err = e.Error()
		return res, e
	case res.Exit == -1:
		if res.Err == "" {
			res.Err = "执行失败"
		}
		return res, errors.New(res.Err)
	}
	return res, nil
}

// limitedBuf 只保留 stderr 的前 512 字节，用于错误提示（如 sudo 的拒绝信息）。
type limitedBuf struct{ b []byte }

func (l *limitedBuf) Write(p []byte) (int, error) {
	if room := 512 - len(l.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.b = append(l.b, p[:room]...)
	}
	return len(p), nil
}

func (l *limitedBuf) suffix() string {
	s := strings.TrimSpace(logSafe(strings.ToValidUTF8(string(l.b), "?")))
	if s == "" {
		return ""
	}
	return "（stderr: " + s + "）"
}
