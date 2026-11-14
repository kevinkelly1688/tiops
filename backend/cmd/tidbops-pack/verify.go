// @author: kevinkelly
// @license: Apache License 2.0
// @file: verify.go
// @time: 2026/10/28

package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"tidbops/internal/release"
)

const (
	maxEntrySize = 1 << 30 // 单个文件上限 1 GiB
	maxTotalSize = 2 << 30 // 解包总量上限 2 GiB
	maxEntries   = 5000
)

var (
	reRootDir   = regexp.MustCompile(`^tidbops-([0-9A-Za-z.+-]+)-linux-(amd64|arm64)$`)
	reDriveName = regexp.MustCompile(`^[A-Za-z]:`)
)

// safeName 校验 tar 条目名：不得是绝对路径、含 ..、反斜杠、盘符、Windows 非法字符。返回按 / 切分的各段。
func safeName(name string) ([]string, error) {
	if name == "" || strings.ContainsAny(name, "\x00\\") || strings.HasPrefix(name, "/") || reDriveName.MatchString(name) {
		return nil, fmt.Errorf("条目路径不安全（绝对路径 / 反斜杠 / 盘符）：%q", name)
	}
	trimmed := strings.TrimSuffix(name, "/")
	segs := strings.Split(trimmed, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("条目路径不安全（含 . / .. / 空段）：%q", name)
		}
		if strings.ContainsAny(s, `:*?"<>|`) {
			return nil, fmt.Errorf("条目路径含非法字符：%q", name)
		}
	}
	if path.Clean(trimmed) != trimmed {
		return nil, fmt.Errorf("条目路径不规范：%q", name)
	}
	return segs, nil
}

// extractSafe 把发布包解到 dest，返回包根目录名与每个文件在 tar 里的模式。
// 只允许普通文件与目录；拒绝符号链接 / 硬链接 / 设备、路径穿越、多个根目录、重复条目、超大条目。
func extractSafe(tgz, dest string) (root string, modes map[string]uint32, err error) {
	f, err := os.Open(tgz)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", nil, fmt.Errorf("不是合法的 gzip 文件：%w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	modes = map[string]uint32{}
	seen := map[string]bool{}
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("读取 tar 失败：%w", err)
		}
		if n >= maxEntries {
			return "", nil, fmt.Errorf("条目过多（超过 %d）", maxEntries)
		}
		segs, err := safeName(h.Name)
		if err != nil {
			return "", nil, err
		}
		if root == "" {
			root = segs[0]
			if !reRootDir.MatchString(root) {
				return "", nil, fmt.Errorf("根目录名 %q 不符合 tidbops-<版本>-linux-<架构>", root)
			}
		} else if segs[0] != root {
			return "", nil, fmt.Errorf("发布包有多个根目录：%q 与 %q", root, segs[0])
		}
		clean := strings.Join(segs, "/")
		if seen[clean] {
			return "", nil, fmt.Errorf("重复的条目：%s", clean)
		}
		seen[clean] = true
		target := filepath.Join(dest, filepath.FromSlash(clean))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if len(segs) < 2 {
				return "", nil, fmt.Errorf("根位置不能是文件：%s", clean)
			}
			if h.Size < 0 || h.Size > maxEntrySize {
				return "", nil, fmt.Errorf("条目过大：%s（%d 字节）", clean, h.Size)
			}
			if total += h.Size; total > maxTotalSize {
				return "", nil, fmt.Errorf("解包总量超过 %d 字节", int64(maxTotalSize))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", nil, err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return "", nil, err
			}
			_, cerr := io.CopyN(out, tr, h.Size)
			if e := out.Close(); cerr == nil {
				cerr = e
			}
			if cerr != nil {
				return "", nil, fmt.Errorf("解包 %s 失败：%w", clean, cerr)
			}
			modes[strings.Join(segs[1:], "/")] = uint32(h.Mode) & 0o777
		default:
			return "", nil, fmt.Errorf("不允许的条目类型（符号链接 / 硬链接 / 设备等）：%s（类型 %q）", clean, string(h.Typeflag))
		}
	}
	if root == "" {
		return "", nil, fmt.Errorf("发布包是空的")
	}
	return root, modes, nil
}

var requiredFiles = []string{"bin/tidbops-server", "VERSION", "CHANGELOG.md", "install.sh", "upgrade.sh", "rollback.sh", "web/index.html", "deploy/ops/tidbops.service"}

// verifyPackage 校验发布包：sidecar 哈希（若有）、安全解包、清单与内容逐文件一致、模式一致，并打印摘要。
func verifyPackage(tgz string, out io.Writer) error {
	// 旁边的 .sha256
	if b, err := os.ReadFile(tgz + ".sha256"); err == nil {
		f := strings.Fields(string(b))
		got, _, herr := release.HashFile(tgz)
		if herr != nil {
			return herr
		}
		if len(f) == 0 || !strings.EqualFold(f[0], got) {
			return fmt.Errorf("整包 SHA-256 与 %s.sha256 不一致（文件被改动或下载不完整）", filepath.Base(tgz))
		}
		fmt.Fprintf(out, "整包 sha256 校验通过：%s\n", got)
	} else {
		fmt.Fprintln(out, "提示：没有找到同名 .sha256 文件，跳过整包哈希校验")
	}

	tmp, err := os.MkdirTemp("", "tidbops-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	root, modes, err := extractSafe(tgz, tmp)
	if err != nil {
		return err
	}
	dir := filepath.Join(tmp, root)
	m, err := release.ReadManifest(dir)
	if err != nil {
		return err
	}
	rm := reRootDir.FindStringSubmatch(root)
	if rm[1] != m.Version || rm[2] != m.Arch || m.OS != "linux" {
		return fmt.Errorf("目录名 %s 与清单（版本 %s、%s/%s）不一致", root, m.Version, m.OS, m.Arch)
	}
	if err := release.VerifyDir(dir, m); err != nil {
		return err
	}
	have := map[string]release.FileEntry{}
	var total int64
	for _, f := range m.Files {
		have[f.Path] = f
		total += f.Size
		if got, ok := modes[f.Path]; ok && got != f.Mode {
			return fmt.Errorf("文件模式与清单不一致：%s（包内 %o，清单 %o）", f.Path, got, f.Mode)
		}
	}
	for _, r := range requiredFiles {
		if _, ok := have[r]; !ok {
			return fmt.Errorf("清单缺少必需文件：%s", r)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "VERSION")); err != nil || strings.TrimSpace(string(b)) != m.Version {
		return fmt.Errorf("VERSION 文件与清单版本 %s 不一致", m.Version)
	}

	fmt.Fprintf(out, "校验通过：%s\n", filepath.Base(tgz))
	fmt.Fprintf(out, "  产品：%s    版本：%s    平台：%s/%s\n", m.Product, m.Version, m.OS, m.Arch)
	fmt.Fprintf(out, "  构建时间：%s    Go：%s\n", m.BuildTime.Format("2006-01-02 15:04:05Z07:00"), m.GoVersion)
	fmt.Fprintf(out, "  数据 schema：%d    最低可直接升级版本：%s\n", m.SchemaVersion, orDash(m.MinFrom))
	fmt.Fprintf(out, "  文件：%d 个，合计 %s\n", len(m.Files), humanSize(total))
	var top []string
	counts := map[string]int{}
	for _, f := range m.Files {
		d := f.Path
		if i := strings.Index(d, "/"); i >= 0 {
			d = d[:i+1]
		}
		counts[d]++
	}
	for d, c := range counts {
		top = append(top, fmt.Sprintf("%s(%d)", d, c))
	}
	sort.Strings(top)
	fmt.Fprintf(out, "  内容：%s\n", strings.Join(top, "  "))
	if m.Notes != "" {
		lines := strings.Split(strings.TrimSpace(m.Notes), "\n")
		if len(lines) > 6 {
			lines = append(lines[:6], "…")
		}
		fmt.Fprintf(out, "  说明：%s\n", strings.Join(lines, "\n        "))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "不限"
	}
	return s
}
