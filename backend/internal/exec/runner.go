// @author: kevinkelly
// @license: Apache License 2.0
// @file: runner.go
// @time: 2026/10/28

package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// MaxOutput 是单次执行累计保留的输出上限（超过则截断）。
const MaxOutput = 256 << 10

// maxLine 单行上限：超长的行会被强制切开，避免无换行的巨量输出撑爆内存。
const maxLine = 64 << 10

// DirectRunner 在本进程内直接执行：Validate → 验证令牌 → Argv → os/exec。
// 它应当运行在拥有 tiup 权限的 exec 子进程里（见 Serve / SudoRunner），而不是面向用户的 Web 进程。
//
// 语义约定：err != nil 表示「没有执行成功地跑完」（校验 / 令牌失败、无法启动、超时、被取消），此时 Result.Exit == -1；
// 命令已执行但以非 0 退出，则 err == nil、Result.Exit 为退出码（>0）。
type DirectRunner struct {
	Pol  Policy
	Topo TopoFunc
	Key  []byte           // 令牌 HMAC 密钥
	Log  func(string)     // 审计日志（不含任何凭据）；可为 nil
	Now  func() time.Time // 测试用时钟；nil 表示 time.Now
	// Consume 可选：令牌验证通过、执行之前调用，用于实现一次性令牌（返回错误则拒绝执行）。
	Consume func(changeID, token string) error
}

func (r DirectRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r DirectRunner) logf(format string, a ...any) {
	if r.Log != nil {
		r.Log(logSafe(fmt.Sprintf(format, a...)))
	}
}

// authorize 做校验与令牌验证。
func authorize(ctx context.Context, pol Policy, topo TopoFunc, key []byte, now time.Time, req Request, consume func(string, string) error) error {
	if err := Validate(ctx, pol, topo, req.Cmd); err != nil {
		return err
	}
	if Info(req.Cmd.Kind).Write {
		if err := Verify(key, req.Cmd, req.ChangeID, req.Token, now); err != nil {
			return err
		}
		if consume != nil {
			if err := consume(req.ChangeID, req.Token); err != nil {
				return err
			}
		}
	}
	return nil
}

func failResult(start time.Time, err error) (Result, error) {
	return Result{Exit: -1, Err: err.Error(), Millis: time.Since(start).Milliseconds()}, err
}

func (r DirectRunner) Run(ctx context.Context, req Request, emit func(line string)) (Result, error) {
	start := time.Now()
	c := req.Cmd
	if err := authorize(ctx, r.Pol, r.Topo, r.Key, r.now(), req, r.Consume); err != nil {
		r.logf("exec reject kind=%s cluster=%s actor=%s change=%s reason=%v", truncate(c.Kind, 40), truncate(c.Cluster, 64), truncate(req.Actor, 64), truncate(req.ChangeID, 128), err)
		return failResult(start, err)
	}
	plan, err := Argv(ctx, r.Pol, r.Topo, c)
	if err != nil {
		r.logf("exec reject kind=%s cluster=%s actor=%s change=%s reason=%v", c.Kind, c.Cluster, truncate(req.Actor, 64), req.ChangeID, err)
		return failResult(start, err)
	}
	defer plan.Cleanup()
	if plan.HTTP != nil {
		return r.runHTTP(ctx, req, plan, start, emit)
	}

	timeout := timeoutFor(r.Pol, c.Kind)

	var secrets []string
	for _, e := range plan.Env {
		if _, v, ok := strings.Cut(e, "="); ok && len(v) >= 3 {
			secrets = append(secrets, v)
		}
	}

	r.logf("exec start kind=%s cluster=%s actor=%s change=%s cmd=%s", c.Kind, c.Cluster, truncate(req.Actor, 64), req.ChangeID, plan)
	var res Result
	var notes strings.Builder // 自动修复的说明行，并入最终输出
	for attempt := 0; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		ob := newOutBuf(emit, secrets)
		cmd := osexec.CommandContext(cctx, plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = append(minimalEnv(), plan.Env...)
		cmd.Stdout, cmd.Stderr = ob, ob // 同一个 writer：stdout/stderr 合并且按序
		cmd.WaitDelay = 3 * time.Second

		runErr := cmd.Run()
		ob.flush()
		res = Result{Output: ob.String(), Millis: time.Since(start).Milliseconds()}
		err = nil

		var ee *osexec.ExitError
		switch {
		case runErr == nil:
			res.Exit = 0
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			res.Exit, res.Err = -1, fmt.Sprintf("命令执行超过 %s，已被终止", timeout)
			err = errors.New(res.Err)
		case ctx.Err() != nil:
			res.Exit, res.Err = -1, "执行已取消"
			err = ctx.Err()
		case errors.As(runErr, &ee):
			code := ee.ExitCode()
			if code <= 0 { // 被信号杀死等
				code = 255
			}
			res.Exit, res.Err = code, fmt.Sprintf("退出码 %d", code)
		default:
			res.Exit, res.Err = -1, "无法启动命令: "+runErr.Error()
			err = runErr
		}
		cancel()

		// tiup 组件文件缺失（命令没有跑起来）：自动重新安装该组件后重试一次。
		if attempt == 0 && err == nil && res.Exit > 0 {
			if comp, ver, ok := repairTarget(plan.Argv, res.Output); ok {
				say := func(l string) {
					notes.WriteString(l)
					notes.WriteByte('\n')
					if emit != nil {
						emit(l)
					}
				}
				if r.repairComponent(ctx, plan.Argv[0], comp, ver, say) {
					continue
				}
			}
		}
		break
	}
	if notes.Len() > 0 {
		res.Output = notes.String() + res.Output
		res.Millis = time.Since(start).Milliseconds()
	}
	r.logf("exec end kind=%s cluster=%s change=%s exit=%d millis=%d", c.Kind, c.Cluster, req.ChangeID, res.Exit, res.Millis)
	if err != nil {
		return res, err
	}
	return res, nil
}

// minimalEnv 只带最小环境变量：PATH、HOME、LANG（以及 tiup 的 TIUP_HOME；Windows 上运行进程所需的系统变量）。
// 不继承任何其它变量，因此 Web 进程环境里的密钥不会泄露给 tiup / mysql 子进程。
func minimalEnv() []string {
	env := []string{"LANG=" + orDefault(os.Getenv("LANG"), "C.UTF-8")}
	keys := []string{"PATH", "HOME", "TIUP_HOME"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "SYSTEMROOT", "USERPROFILE", "TEMP", "TMP")
	}
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// outBuf 把合并的输出按行切开：逐行回调 emit，同时累计至多 MaxOutput 字节。
// 已知的敏感值（如数据库密码）会被替换为 ***，且不会因超长行而无限增长。
type outBuf struct {
	mu      sync.Mutex
	emit    func(string)
	secrets []string
	buf     bytes.Buffer
	part    []byte
	trunc   bool
}

func newOutBuf(emit func(string), secrets []string) *outBuf {
	return &outBuf{emit: emit, secrets: secrets}
}

func (o *outBuf) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.part = append(o.part, p...)
	for {
		i := bytes.IndexByte(o.part, '\n')
		if i < 0 {
			break
		}
		o.line(string(o.part[:i]))
		o.part = o.part[i+1:]
	}
	for len(o.part) > maxLine {
		o.line(string(o.part[:maxLine]))
		o.part = o.part[maxLine:]
	}
	if len(o.part) == 0 {
		o.part = nil
	}
	return len(p), nil
}

func (o *outBuf) line(s string) {
	s = strings.TrimSuffix(s, "\r")
	s = strings.ToValidUTF8(s, "?")
	for _, sec := range o.secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	if o.trunc {
		return
	}
	if o.buf.Len()+len(s)+1 > MaxOutput {
		o.trunc = true
		if room := MaxOutput - o.buf.Len(); room > 0 && room < len(s) {
			s = strings.ToValidUTF8(s[:room], "")
		} else if room <= 0 {
			s = ""
		}
		if s != "" {
			o.buf.WriteString(s)
			o.buf.WriteByte('\n')
			o.send(s)
		}
		o.send("…[输出超过 256KB，后续内容已截断]")
		return
	}
	o.buf.WriteString(s)
	o.buf.WriteByte('\n')
	o.send(s)
}

func (o *outBuf) send(s string) {
	if o.emit != nil {
		o.emit(s)
	}
}

func (o *outBuf) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.part) > 0 {
		o.line(string(o.part))
		o.part = nil
	}
}

func (o *outBuf) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.buf.String()
	if o.trunc {
		s += "…[输出超过 256KB，后续内容已截断]\n"
	}
	return s
}

// logSafe 去掉控制字符（换行等），防止请求里的字段伪造多行日志。
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
