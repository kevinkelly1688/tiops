// @author: kevinkelly
// @license: Apache License 2.0
// @file: runner.go
// @time: 2026/10/28

package deploy

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// RootRunner 以 root 身份执行一条命令行（实现里是 `su - root -c <cmdline>`，口令经伪终端交给 su）。
// 实现必须在用完后把 password 清零；password 不得出现在任何日志、错误信息或返回的输出里。
type RootRunner interface {
	RunAsRoot(ctx context.Context, cmdline string, password []byte) (output string, err error)
}

var (
	// ErrBadPassword：su 拒绝了口令。此时没有任何命令被执行。
	ErrBadPassword = errors.New("root 密码不正确，没有任何命令被执行")
	// ErrUnsupported：当前系统不支持（非 Linux，或没有 su）。
	ErrUnsupported = errors.New("当前系统不支持以 root 口令启动部署（需要 Linux 与 su）")
)

var (
	rePrompt  = regexp.MustCompile(`(?i)(password|密码|口令)[^\n]{0,20}[:：]\s*$`)
	reAuthBad = regexp.MustCompile(`(?i)authentication failure|incorrect password|sorry|认证失败|鉴定故障|密码错误|su: failure`)
)

// IsPasswordPrompt 判断 su 输出的末尾是不是口令提示。
func IsPasswordPrompt(out string) bool { return rePrompt.MatchString(strings.TrimRight(out, " \t")) }

// ClassifySu 把 su 的退出结果归类：exitErr 为 nil（退出码 0）→ nil；输出里有认证失败的字样 → ErrBadPassword；
// 其余原样返回（附上输出，便于排查 su 被 PAM 限制等情况；输出里不会有口令，因为终端在输入口令时关闭了回显）。
func ClassifySu(out string, exitErr error) error {
	if exitErr == nil {
		return nil
	}
	if reAuthBad.MatchString(out) {
		return ErrBadPassword
	}
	return &SuError{Output: CleanOutput(out), Err: exitErr}
}

// SuError 是 su 因口令之外的原因失败（例如 pam_wheel 不允许、被沙箱限制）。
type SuError struct {
	Output string
	Err    error
}

func (e *SuError) Error() string {
	if e.Output == "" {
		return "su 执行失败：" + e.Err.Error()
	}
	return "su 执行失败：" + e.Err.Error() + "：" + e.Output
}
func (e *SuError) Unwrap() error { return e.Err }

// CleanOutput 去掉伪终端带来的回车与首尾空白，并截断到 2000 字符。
func CleanOutput(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	if len(s) > 2000 {
		s = s[:2000] + "…"
	}
	return s
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
