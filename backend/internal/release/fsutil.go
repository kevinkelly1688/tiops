// @author: kevinkelly
// @license: Apache License 2.0
// @file: fsutil.go
// @time: 2026/10/28

package release

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// writeFileAtomic 先写同目录临时文件再 rename，权限在写入前就是最终权限。
func writeFileAtomic(p string, data []byte, perm os.FileMode) error {
	tmp := p + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil { // 不受 umask 影响
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeOwned 写文件并设置属主 / 权限。敏感文件先以 0600 写入、改属主后再放宽到目标权限，避免短暂过宽。
func (e *Engine) writeOwned(p string, data []byte, perm os.FileMode, user, group string) error {
	tmp := p + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	fail := func(err error) error { _ = os.Remove(tmp); return err }
	if user != "" {
		if err := e.Sys.Chown(tmp, user, group, false); err != nil {
			return fail(err)
		}
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fail(err)
	}
	return nil
}

// copyTree 把发布包目录复制到 dst：只允许普通文件与目录（遇到符号链接 / 设备文件即拒绝）。
// 目录 0755；文件有任何可执行位则 0755，否则 0644（不继承属组 / 其他人的写权限）。
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("不安全的路径：%s", rel)
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			perm := os.FileMode(0o644)
			if info.Mode().Perm()&0o111 != 0 {
				perm = 0o755
			}
			return copyFile(p, target, perm)
		default:
			return fmt.Errorf("发布包里不允许符号链接或特殊文件：%s", rel)
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

// dirSize 统计目录下普通文件的总大小（目录不存在返回 0）。
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// ---- current 符号链接 ----

// setLink 原子切换符号链接：先建临时链接再 rename 覆盖。
// 开发机 Windows 上 rename 不能覆盖目录链接，退化为先删再换（非原子，仅开发用）；
// 若连符号链接都建不了（无权限），退化为写一个只含目标的文本文件，便于在 Windows 上跑测试。
func setLink(link, target string) error {
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		return writeFileAtomic(link, []byte(target+"\n"), 0o644)
	}
	if err := os.Rename(tmp, link); err != nil {
		if runtime.GOOS != "windows" {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, link)
	}
	return nil
}

// readLink 返回符号链接的目标（及 Windows 开发机的文本文件退化形式）。
func readLink(link string) (string, error) {
	t, err := os.Readlink(link)
	if err == nil {
		return t, nil
	}
	if runtime.GOOS == "windows" {
		if b, rerr := os.ReadFile(link); rerr == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", err
}

// ---- tar ----

// archiveSkipTop 是备份时排除的数据目录顶层名（执行器临时目录）。
var archiveSkipTop = map[string]bool{"tmp": true}

// writeTarGz 把 dir 的内容打成 tar.gz（相对路径、不含顶层目录名；0600；排除 tmp 目录与 *.tmp 残留；跳过符号链接等特殊文件）。
// 返回被跳过的特殊文件列表。
func writeTarGz(dir, dst string, perm os.FileMode) (skipped []string, err error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if archiveSkipTop[strings.SplitN(slash, "/", 2)[0]] && d.IsDir() && !strings.Contains(slash, "/") {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsDir() && !info.Mode().IsRegular() {
			skipped = append(skipped, slash)
			return nil
		}
		if info.Mode().IsRegular() && strings.HasSuffix(slash, ".tmp") && !strings.Contains(slash, "/") {
			return nil
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = slash
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, io.LimitReader(in, h.Size))
			in.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return skipped, err
	}
	if err := tw.Close(); err != nil {
		return skipped, err
	}
	if err := gz.Close(); err != nil {
		return skipped, err
	}
	if err := f.Sync(); err != nil {
		return skipped, err
	}
	ok = true
	return skipped, nil
}

// safeTarName 校验归档里的条目名：不能是绝对路径、不能含 .. 或反斜杠 / 盘符，返回规范化的相对路径。
func safeTarName(name string) (string, error) {
	if name == "" {
		return "", errors.New("归档里有空路径")
	}
	if strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || (len(name) >= 2 && name[1] == ':') {
		return "", fmt.Errorf("归档里有不安全的路径（绝对路径或含非法字符）：%q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("归档里有路径穿越：%q", name)
		}
	}
	c := path.Clean(name)
	if c == "." {
		return ".", nil
	}
	if !filepath.IsLocal(filepath.FromSlash(c)) {
		return "", fmt.Errorf("归档里有不安全的路径：%q", name)
	}
	return c, nil
}

// extractTarGz 把 tar.gz 解到已存在的空目录 dst：只允许普通文件与目录；符号链接 / 硬链接 / 设备一律拒绝；
// 路径穿越与绝对路径一律拒绝。任何问题都返回错误（调用方应丢弃 dst）。
func extractTarGz(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s 不是有效的 tar.gz: %w", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取归档失败: %w", err)
		}
		rel, err := safeTarName(h.Name)
		if err != nil {
			return err
		}
		if rel == "." {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			perm := os.FileMode(h.Mode&0o777) | 0o600
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("归档里有不允许的条目类型（符号链接 / 硬链接 / 设备文件）：%q", h.Name)
		}
	}
	return nil
}

// removeContents 删除目录下的所有内容但保留目录本身（保留其属主与权限）。
func removeContents(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
