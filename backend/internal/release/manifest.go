// @author: kevinkelly
// @license: Apache License 2.0
// @file: manifest.go
// @time: 2026/10/28

// Package release 实现平台自身的打包、部署、升级与回滚。
//
// 发布包是一个 tar.gz，解压后的根目录为 tidbops-<版本>-linux-<架构>/，内容：
//
//	MANIFEST.json            清单：版本、构建信息、数据 schema 版本、每个文件的 SHA-256
//	VERSION                  纯文本版本号
//	CHANGELOG.md             变更说明
//	bin/tidbops-server       平台二进制（Linux）
//	web/                     前端静态文件（index.html、*.js、*.css）
//	deploy/ops/              systemd 单元、sudoers、exec.json 样例
//	deploy/tidb-deploy/      TiDB 集群一键部署工具包（可选，供「安装部署」页下载）
//	docs/                    部署与联调文档
//
// 目标机目录布局（root 拥有，运行账号只读）：
//
//	/opt/tidbops/releases/<版本>/{bin,web,...}   每个版本一份，升级不覆盖旧版本
//	/opt/tidbops/current -> releases/<版本>      当前版本（原子切换符号链接）
//	/var/lib/tidbops                              数据目录（用户、审计、变更、设置…）
//	/var/backups/tidbops/<时间>-<旧版本>.tar.gz   升级前的数据备份
//	/etc/tidb-ops/                                执行器策略 / 密钥 / 环境变量
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ManifestName 是清单文件名。
const ManifestName = "MANIFEST.json"

// Product 是产品标识，写进清单，防止拿别的包来升级。
const Product = "tidbops"

// FileEntry 是清单里的一个文件。
type FileEntry struct {
	Path   string `json:"path"` // 相对发布包根目录，正斜杠
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
}

// Manifest 描述一个发布包。
type Manifest struct {
	Product       string      `json:"product"`
	Version       string      `json:"version"` // 如 1.4.0（不带 v 前缀）
	BuildTime     time.Time   `json:"buildTime"`
	GoVersion     string      `json:"goVersion"`
	OS            string      `json:"os"`
	Arch          string      `json:"arch"`              // amd64 | arm64
	SchemaVersion int         `json:"schemaVersion"`     // 数据目录 schema 版本；新版本可以迁移旧数据，旧版本不能读新数据
	MinFrom       string      `json:"minFrom,omitempty"` // 允许直接升级的最低版本；空表示不限
	Notes         string      `json:"notes,omitempty"`
	Files         []FileEntry `json:"files"`
}

// HashFile 返回文件的 SHA-256 与大小。
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// BuildFileList 遍历目录生成文件清单（不含 MANIFEST.json 自身），按路径排序。
func BuildFileList(root string) ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("发布包里不允许非普通文件：%s", rel)
		}
		sum, size, err := HashFile(p)
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, SHA256: sum, Mode: uint32(info.Mode().Perm()), Size: size})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// WriteManifest 把清单写到目录下的 MANIFEST.json。
func WriteManifest(root string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, ManifestName), append(b, '\n'), 0o644)
}

// ReadManifest 读取并基本校验目录下的清单。
func ReadManifest(root string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return m, fmt.Errorf("读取 %s 失败: %w", ManifestName, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s 不是合法的 JSON: %w", ManifestName, err)
	}
	if m.Product != Product {
		return m, fmt.Errorf("不是 tidbops 发布包（product=%q）", m.Product)
	}
	if m.Version == "" {
		return m, fmt.Errorf("清单缺少版本号")
	}
	return m, nil
}

// VerifyDir 校验目录内容与清单完全一致：每个文件存在且哈希相同、没有清单之外的文件。
func VerifyDir(root string, m Manifest) error {
	want := map[string]FileEntry{}
	for _, f := range m.Files {
		want[f.Path] = f
	}
	got, err := BuildFileList(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, g := range got {
		w, ok := want[g.Path]
		if !ok {
			return fmt.Errorf("发布包里有清单之外的文件：%s", g.Path)
		}
		if w.SHA256 != g.SHA256 {
			return fmt.Errorf("文件被改动或损坏（哈希不一致）：%s", g.Path)
		}
		seen[g.Path] = true
	}
	for p := range want {
		if !seen[p] {
			return fmt.Errorf("发布包缺少文件：%s", p)
		}
	}
	return nil
}
