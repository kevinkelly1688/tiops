// @author: kevinkelly
// @license: Apache License 2.0
// @file: sys_other.go
// @time: 2026/10/28

//go:build !linux

package release

import "errors"

// FreeBytes 在非 Linux 上不可用，调用方会跳过磁盘空间检查。
func (RealSys) FreeBytes(path string) (uint64, error) {
	return 0, errors.New("当前平台不支持查询磁盘空间")
}
