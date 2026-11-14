// @author: kevinkelly
// @license: Apache License 2.0
// @file: tool.go
// @time: 2026/10/28

package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var reToolPkg = regexp.MustCompile(`^tidb-deploy-(\d+)\.(\d+)\.(\d+)\.tar\.gz$`)

// FindToolPackage 在前端目录的 deploy/dist 下找版本最高的 tidb-deploy-X.Y.Z.tar.gz（发布包里带着它，页面的「下载部署工具包」用的就是它）。
func FindToolPackage(webDir string) (pkgPath, version string, err error) {
	dir := filepath.Join(webDir, "deploy", "dist")
	ents, e := os.ReadDir(dir)
	if e != nil {
		return "", "", fmt.Errorf("找不到部署工具包目录 %s：%w", dir, e)
	}
	best := [3]int{-1}
	for _, en := range ents {
		m := reToolPkg.FindStringSubmatch(en.Name())
		if m == nil || en.IsDir() {
			continue
		}
		v := [3]int{}
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		if less(best, v) {
			best, pkgPath, version = v, filepath.Join(dir, en.Name()), fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
		}
	}
	if pkgPath == "" {
		return "", "", fmt.Errorf("%s 里没有 tidb-deploy-X.Y.Z.tar.gz", dir)
	}
	return pkgPath, version, nil
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

const maxToolBytes = 32 << 20

// ExtractTool 把工具包解到 dest（去掉最外层目录），返回 tidb-deploy.sh 的路径。
// 如果旁边有 .sha256 文件，先校验整包；解包时拒绝绝对路径、.. 与非常规文件。
func ExtractTool(pkgPath, dest string) (script string, err error) {
	f, err := os.Open(pkgPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if b, e := os.ReadFile(pkgPath + ".sha256"); e == nil {
		want := strings.ToLower(strings.TrimSpace(strings.Fields(string(b) + " ")[0]))
		h := sha256.New()
		if _, e := io.Copy(h, f); e != nil {
			return "", e
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			return "", fmt.Errorf("部署工具包校验失败：%s 的 SHA-256 与 .sha256 文件不一致", filepath.Base(pkgPath))
		}
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return "", e
		}
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxToolBytes))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", e
		}
		name := path.Clean(h.Name)
		parts := strings.Split(name, "/")
		if path.IsAbs(name) || len(parts) < 2 || strings.Contains(name, "..") {
			if len(parts) == 1 && h.Typeflag == tar.TypeDir {
				continue // 最外层目录本身
			}
			return "", fmt.Errorf("工具包里有不合法的路径 %q", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(strings.Join(parts[1:], "/")))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return "", err
			}
			mode := os.FileMode(0o600)
			if h.Mode&0o111 != 0 || strings.HasSuffix(target, ".sh") {
				mode = 0o700
			}
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return "", err
			}
			if err := w.Close(); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("工具包里有不支持的文件类型：%q", h.Name)
		}
	}
	script = filepath.Join(dest, "tidb-deploy.sh")
	if st, e := os.Stat(script); e != nil || st.IsDir() {
		return "", errors.New("工具包里没有 tidb-deploy.sh")
	}
	return script, nil
}
