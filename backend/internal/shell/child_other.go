// @author: kevinkelly
// @license: Apache License 2.0
// @file: child_other.go
// @time: 2026/10/28

//go:build !linux

package shell

import (
	"os"
	"os/exec"
)

func setProcGroup(*exec.Cmd) {}

func killGroup(p *os.Process, _ bool) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}

func termProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
