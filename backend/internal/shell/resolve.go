// @author: kevinkelly
// @license: Apache License 2.0
// @file: resolve.go
// @time: 2026/10/28

package shell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 「读内容」类命令的路径检查原本只看文本前缀：/tidb-deploy/link 是个指向 /tidb-data/… 的符号链接时，文本上在允许目录里，
// 实际读到的却是数据目录。这里在判定时把路径解析成真实路径（跟随符号链接）再比对允许目录。
//
// 两种用法：
//   - strict = true（tidb 身份的 shell-exec 子进程里，真正执行前的最后一道判定）：解析出错（除「不存在」外）一律拒绝；
//   - strict = false（平台进程的预判定）：平台账号可能没有权限穿过 tidb 的目录，解析不了就退回文本判断，由子进程兜底。

const maxGlobMatches = 2000

// fsView 抽象文件系统视图，测试里可以换成假的。
type fsView struct {
	resolve func(string) (string, error)
	glob    func(string) ([]string, error)
	strict  bool
}

func hostFS(strict bool) *fsView {
	return &fsView{resolve: resolveExisting, glob: filepath.Glob, strict: strict}
}

// resolveExisting 解析 p 的真实路径；p 不存在时，对最长的已存在前缀解析再拼回剩余部分（不存在的东西谈不上符号链接）。
func resolveExisting(p string) (string, error) {
	cur, rest := filepath.Clean(p), ""
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			rp, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			if rest != "" {
				rp = filepath.Join(rp, rest)
			}
			return filepath.ToSlash(rp), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.ToSlash(p), nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// roots 加上它们各自解析后的真实路径（/var/log 在有的系统上本身就是符号链接）。
func (v *fsView) rootsWithResolved(roots []string) []string {
	out := append([]string{}, roots...)
	for _, r := range roots {
		if rp, err := v.resolve(r); err == nil && rp != r {
			out = append(out, rp)
		}
	}
	return out
}

// within 判断 p（可含通配符）实际指向的每个文件是否都在允许目录之内。
func (v *fsView) within(p string, roots []string) bool {
	targets := []string{p}
	if strings.ContainsAny(p, "*?[") {
		ms, err := v.glob(p)
		if err != nil {
			return !v.strict
		}
		if len(ms) > maxGlobMatches {
			return false
		}
		targets = ms // 没有匹配就没有东西可读
	}
	rr := v.rootsWithResolved(roots)
	for _, t := range targets {
		rp, err := v.resolve(t)
		if err != nil {
			if v.strict {
				return false
			}
			continue
		}
		if !under(rp, rr) {
			return false
		}
	}
	return true
}
