// @author: kevinkelly
// @license: Apache License 2.0
// @file: child.go
// @time: 2026/10/28

package shell

// 子进程入口（tidbops-server shell-exec --stdin）。平台进程经 sudoers 放行的这一条固定命令以 tidb 身份调用它：
// stdin 一个 JSON 请求（ChildReq），stdout 按行回传 {"t":"out","line":…}，最后一行 {"t":"result",…}（ChildRes）。
//
// 模式：
//   probe   探测：tidb 的 sudo -n 是否可用、家目录、用户名；
//   run     在本进程里（仍在平台服务的沙箱内：文件系统只读、私有 /tmp）逐条执行只读命令。这里会**重新判定**，不是只读就拒绝，
//           所以即使平台进程被控制，这条通道也只能跑护栏认定的只读命令；sudo 行以 tidb 的 sudo -n 执行；
//   launch  用 sudo -n systemd-run 起临时单元执行平台生成的 run.sh（变更命令，在沙箱之外）；
//   stop    sudo -n systemctl stop 临时单元。
//
// 取消：平台关掉子进程的 stdin（或平台自己退出）即视为取消——子进程一直读 stdin 到 EOF；也响应 SIGTERM。
// 遮蔽：run 模式的每一行在发回之前经 LineMasker 遮蔽，原文不出子进程。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// 子进程模式。
const (
	ModeProbe  = "probe"
	ModeRun    = "run"
	ModeLaunch = "launch"
	ModeStop   = "stop"
)

// ChildReq 是平台发给子进程的请求。
type ChildReq struct {
	Mode    string   `json:"mode"`
	JobID   string   `json:"job,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
	Stop    bool     `json:"stop,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Roots   []string `json:"roots,omitempty"` // 追加的可读目录（与平台设置一致）
	Unit    string   `json:"unit,omitempty"`
	Script  string   `json:"script,omitempty"`
}

// ChildRes 是子进程的最终结果。
type ChildRes struct {
	Exit int    `json:"exit"`
	Err  string `json:"err,omitempty"`
	Sudo bool   `json:"sudo,omitempty"` // probe：tidb 的 sudo -n 可用
	Home string `json:"home,omitempty"` // probe
	User string `json:"user,omitempty"` // probe
	Out  string `json:"out,omitempty"`  // launch / stop：命令输出（已遮蔽）
}

const (
	childMaxReq  = 1 << 20
	childLineMax = 64 << 10
	sudoTimeout  = 45 * time.Second
)

var (
	reUnit  = regexp.MustCompile(`^tidbops-shell-s-[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)
	reJobID = regexp.MustCompile(`^s-[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)
)

// ServeChild 处理一个请求（子进程的主体）。请求格式错误也以 result（exit=-1）回传，返回 nil。
func ServeChild(ctx context.Context, in io.Reader, out io.Writer) error {
	w := bufio.NewWriter(out)
	defer w.Flush()
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = w.Write(append(b, '\n'))
		_ = w.Flush()
	}
	result := func(r ChildRes) {
		write(struct {
			T string `json:"t"`
			ChildRes
		}{"result", r})
	}
	dec := json.NewDecoder(io.LimitReader(in, childMaxReq))
	dec.DisallowUnknownFields()
	var req ChildReq
	if err := dec.Decode(&req); err != nil {
		result(ChildRes{Exit: -1, Err: "请求格式错误：" + err.Error()})
		return nil
	}
	// stdin 到 EOF = 平台取消或平台退出；SIGTERM 同样
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, io.MultiReader(dec.Buffered(), in))
		cancel()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-ctx.Done():
		}
	}()

	emit := func(line string) {
		write(struct {
			T    string `json:"t"`
			Line string `json:"line"`
		}{"out", line})
	}
	switch req.Mode {
	case ModeProbe:
		result(probe(ctx))
	case ModeRun:
		result(runInline(ctx, req, emit))
	case ModeLaunch:
		result(launchUnit(ctx, req))
	case ModeStop:
		result(stopUnit(ctx, req))
	default:
		result(ChildRes{Exit: -1, Err: "未知模式"})
	}
	return nil
}

func childEnv() (env []string, home, name string) {
	home = os.Getenv("HOME")
	if u, err := user.Current(); err == nil {
		name = u.Username
		if home == "" {
			home = u.HomeDir
		}
	}
	if home == "" {
		home = "/"
	}
	env = []string{"HOME=" + home, "USER=" + name, "LANG=C.UTF-8", "TERM=dumb",
		// tidb 可写的 ~/.tiup/bin 放最后：放在前面时，那里的同名文件会冒充 cat / ls 等系统命令
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:" + home + "/.tiup/bin"}
	return
}

// sudoRun 以 sudo -n 执行一条固定命令（probe / launch / stop 用），返回合并输出（已遮蔽）与退出码。
func sudoRun(ctx context.Context, args ...string) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, sudoTimeout)
	defer cancel()
	env, _, _ := childEnv()
	cmd := exec.CommandContext(cctx, "sudo", append([]string{"-n"}, args...)...)
	cmd.Env = env
	cmd.Stdin = nil
	b, err := cmd.CombinedOutput()
	text, _ := MaskText(strings.TrimSpace(string(b)))
	code := 0
	if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if strings.Contains(text, "a password is required") || strings.Contains(text, "not allowed") || strings.Contains(text, "may not run sudo") {
			return text, code, fmt.Errorf("tidb 在中控机上没有 sudo 免密（sudo -n 失败）：%s", text)
		}
		if cctx.Err() != nil {
			return text, code, fmt.Errorf("sudo 超过 %s 没有返回", sudoTimeout)
		}
		return text, code, fmt.Errorf("sudo 执行失败：%s", firstNonEmpty(text, err.Error()))
	}
	return text, 0, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func probe(ctx context.Context) ChildRes {
	_, home, name := childEnv()
	r := ChildRes{Home: home, User: name}
	if _, err := exec.LookPath("sudo"); err != nil {
		r.Err = "中控机上没有 sudo"
		return r
	}
	if _, _, err := sudoRun(ctx, "true"); err != nil {
		r.Err = err.Error()
		return r
	}
	r.Sudo = true
	return r
}

func launchUnit(ctx context.Context, req ChildReq) ChildRes {
	if !reUnit.MatchString(req.Unit) || !reJobID.MatchString(req.JobID) || !strings.HasSuffix(req.Unit, req.JobID) {
		return ChildRes{Exit: -1, Err: "单元名不合法"}
	}
	if !rePath.MatchString(req.Script) || strings.Contains(req.Script, "..") || !strings.HasSuffix(req.Script, "/"+req.JobID+"/run.sh") {
		return ChildRes{Exit: -1, Err: "任务脚本路径不合法"}
	}
	out, code, err := sudoRun(ctx, "systemd-run", "--unit="+req.Unit, "--description=tidbops shell "+req.JobID, "/bin/bash", req.Script)
	r := ChildRes{Exit: code, Out: out}
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

func stopUnit(ctx context.Context, req ChildReq) ChildRes {
	if !reUnit.MatchString(req.Unit) {
		return ChildRes{Exit: -1, Err: "单元名不合法"}
	}
	out, code, err := sudoRun(ctx, "systemctl", "stop", req.Unit)
	r := ChildRes{Exit: code, Out: out}
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

// runInline 在本进程里逐条执行只读命令（run 模式）。
func runInline(ctx context.Context, req ChildReq, emitRaw func(string)) ChildRes {
	if !rePath.MatchString(req.Cwd) || strings.Contains(req.Cwd, "..") {
		return ChildRes{Exit: -1, Err: "工作目录不合法"}
	}
	switch req.Timeout {
	case 30, 120, 600:
	default:
		return ChildRes{Exit: -1, Err: "超时只能是 30、120 或 600 秒"}
	}
	if !reJobID.MatchString(req.JobID) {
		return ChildRes{Exit: -1, Err: "任务号不合法"}
	}
	var roots []string
	for _, r := range req.Roots {
		if rePath.MatchString(r) && !strings.Contains(r, "..") {
			roots = append(roots, r)
		}
	}
	v := DecideOnHost(PolicyStd, req.Lines, roots, true) // 最后一道判定：解析符号链接，出错即拒绝
	if v.Action != ActRun || v.Level != LevelRO {
		return ChildRes{Exit: -1, Err: "平台进程内只执行只读命令：" + v.Why}
	}
	if _, err := exec.LookPath("bash"); err != nil {
		return ChildRes{Exit: -1, Err: "中控机上没有 bash"}
	}
	env, _, name := childEnv()
	// 所有输出先遮蔽再发回；总量限 1 MiB
	lm := &LineMasker{}
	sent, truncated := 0, false
	emit := func(s string) {
		if truncated {
			return
		}
		sent += len(s) + 1
		if sent > maxOutput {
			truncated = true
			emitRaw("[平台] 输出超过 1 MiB，已截断")
			return
		}
		emitRaw(s)
	}
	out := func(s string) {
		if t, ok := lm.Line(s); ok {
			for _, l := range strings.Split(t, "\n") {
				emit(l)
			}
		}
	}
	out(fmt.Sprintf("[平台] 任务 %s 开始（身份 %s，目录 %s，%s）", req.JobID, name, req.Cwd, time.Now().Format("2006-01-02 15:04:05")))
	rc := 0
	for _, line := range req.Lines {
		if ctx.Err() != nil {
			rc = 143
			break
		}
		out("$ " + line)
		cmdline := line
		if rest, ok := SplitSudo(line); ok {
			cmdline = "sudo -n " + rest
		}
		rc = runOne(ctx, cmdline, req.Cwd, env, req.Timeout, out)
		switch {
		case ctx.Err() != nil:
			out("[平台] 已取消")
			rc = 143
		case rc == 124:
			out(fmt.Sprintf("[平台] 命令超过 %d 秒仍未结束，已终止", req.Timeout))
		}
		if ctx.Err() != nil {
			break
		}
		if rc != 0 && req.Stop {
			out(fmt.Sprintf("[平台] 命令退出码 %d，按「遇错停止」不再执行后面的命令", rc))
			break
		}
	}
	if t, ok := lm.Flush(); ok {
		for _, l := range strings.Split(t, "\n") {
			emit(l)
		}
	}
	emit(fmt.Sprintf("[平台] 任务结束，退出码 %d（%s）", rc, time.Now().Format("2006-01-02 15:04:05")))
	return ChildRes{Exit: rc}
}

// runOne 执行一条命令：bash -c 'cd "$0" && exec bash -c "$1"' <cwd> <命令>，合并 stdout / stderr 逐行回调；
// 超时先 SIGTERM 整个进程组、5 秒后 SIGKILL；返回退出码（超时 124，取消 143）。
func runOne(ctx context.Context, cmdline, cwd string, env []string, timeout int, out func(string)) int {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.Command("bash", "-c", `cd "$0" && exec bash -c "$1"`, cwd, cmdline)
	cmd.Env = env
	cmd.Stdin = nil
	setProcGroup(cmd)
	r, w, err := os.Pipe()
	if err != nil {
		out("[平台] 无法创建管道：" + err.Error())
		return 1
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		out("[平台] 无法启动：" + err.Error())
		return 127
	}
	_ = w.Close()
	killed := make(chan struct{})
	go func() {
		select {
		case <-cctx.Done():
			_ = killGroup(cmd.Process, false)
			select {
			case <-killed:
			case <-time.After(5 * time.Second):
				_ = killGroup(cmd.Process, true)
			}
		case <-killed:
		}
	}()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if line != "" || err == nil {
			out(strings.TrimRight(line, "\r"))
		}
		if err != nil {
			break
		}
	}
	_ = r.Close()
	werr := cmd.Wait()
	close(killed)
	switch {
	case ctx.Err() != nil:
		return 143
	case cctx.Err() != nil:
		return 124
	case werr == nil:
		return 0
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
		return 137
	}
	return 1
}

// readLine 读一行（不含换行）；超长的行按 childLineMax 切开，每段算一行。
func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		part, isPrefix, err := br.ReadLine()
		sb.Write(part)
		if err != nil {
			return sb.String(), err
		}
		if !isPrefix || sb.Len() >= childLineMax {
			return sb.String(), nil
		}
	}
}
