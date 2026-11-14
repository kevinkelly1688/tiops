// @author: kevinkelly
// @license: Apache License 2.0
// @file: su_other.go
// @time: 2026/10/28

//go:build !linux

package deploy

import "context"

// SuRunner 在非 Linux 系统上不可用（开发机 Windows / macOS 只用于跑单元测试，真正的部署只在 Linux 中控机上进行）。
type SuRunner struct{}

// DefaultRootRunner 返回生产用的 root 执行器。
func DefaultRootRunner() RootRunner { return SuRunner{} }

// RunAsRoot 总是返回 ErrUnsupported。
func (SuRunner) RunAsRoot(_ context.Context, _ string, password []byte) (string, error) {
	zero(password)
	return "", ErrUnsupported
}
