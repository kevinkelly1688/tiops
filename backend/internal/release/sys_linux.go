// @author: kevinkelly
// @license: Apache License 2.0
// @file: sys_linux.go
// @time: 2026/10/28

//go:build linux

package release

import "syscall"

func (RealSys) FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
