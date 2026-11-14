// @author: kevinkelly
// @license: Apache License 2.0
// @file: child_linux.go
// @time: 2026/10/28

//go:build linux

package shell

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcGroup 让命令自成进程组，超时 / 取消时能连同它派生的子进程一起终止。
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup 终止整个进程组（force = SIGKILL，否则 SIGTERM）。
func killGroup(p *os.Process, force bool) error {
	if p == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-p.Pid, sig)
}

// termProcess 给进程发 SIGTERM（平台取消子进程时用：sudo 会把它转发给子进程）。
func termProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGTERM)
}
