// @author: kevinkelly
// @license: Apache License 2.0
// @file: store.go
// @time: 2026/10/28

package fixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// ErrNotRecorded 表示数据包里没有该条目（或该条目当时采集失败）。
var ErrNotRecorded = errors.New("fixture: 数据包中没有该条目")

// ValidName 校验条目名：小写字母数字、点、下划线、连字符，用 / 分层，不允许 ..。
func ValidName(name string) error {
	if !nameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("fixture: 非法条目名 %q", name)
	}
	return nil
}

// Dir 表示磁盘上的一个样例数据包目录。
type Dir struct{ Root string }

func (d Dir) dataPath(name, ext string) string {
	return filepath.Join(d.Root, DataDir, filepath.FromSlash(name)+ext)
}

// Hash 返回数据的 SHA-256 十六进制串。
func Hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Write 写入一个数据文件，返回相对路径（使用 / 分隔）与哈希。
func (d Dir) Write(name, ext string, data []byte) (rel, sum string, err error) {
	if err = ValidName(name); err != nil {
		return
	}
	p := d.dataPath(name, ext)
	if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	if err = os.WriteFile(p, data, 0o644); err != nil {
		return
	}
	rel = filepath.ToSlash(filepath.Join(DataDir, filepath.FromSlash(name)+ext))
	return rel, Hash(data), nil
}

// SaveManifest 写入 manifest.json。
func (d Dir) SaveManifest(m *Manifest) error {
	if err := os.MkdirAll(d.Root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d.Root, ManifestName), append(b, '\n'), 0o644)
}

// LoadManifest 读取并校验 manifest.json 的格式版本。
func LoadManifest(root string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("fixture: 读取 %s 失败: %w", ManifestName, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("fixture: 解析 %s 失败: %w", ManifestName, err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("fixture: 不支持的 manifest 版本 %d（期望 %d）", m.Schema, SchemaVersion)
	}
	return &m, nil
}

// Read 读取条目内容，并用清单中的哈希校验，防止数据包被改动或损坏。
func (d Dir) Read(m *Manifest, name string) ([]byte, error) {
	rec := m.Find(name)
	if rec == nil || !rec.OK {
		return nil, fmt.Errorf("%w: %s", ErrNotRecorded, name)
	}
	b, err := os.ReadFile(filepath.Join(d.Root, filepath.FromSlash(rec.File)))
	if err != nil {
		return nil, fmt.Errorf("fixture: 读取 %s 失败: %w", rec.File, err)
	}
	if got := Hash(b); got != rec.SHA256 {
		return nil, fmt.Errorf("fixture: %s 哈希不一致（清单 %s…，实际 %s…），数据包可能被改动", rec.File, rec.SHA256[:8], got[:8])
	}
	return b, nil
}

// Verify 校验整个数据包，返回全部问题（空表示通过）。
func (d Dir) Verify(m *Manifest) []error {
	var errs []error
	for _, it := range m.Items {
		if !it.OK {
			continue
		}
		if _, err := d.Read(m, it.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
