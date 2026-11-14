// @author: kevinkelly
// @license: Apache License 2.0
// @file: store.go
// @time: 2026/10/28

package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Version 是一份已保存的部署配置（不可变：改了就是新版本）。
type Version struct {
	Cluster string    `json:"cluster"`
	Ver     int       `json:"ver"`
	SHA     string    `json:"sha256"` // 渲染出的 deploy.conf 的 SHA-256（十六进制）
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Spec    Spec      `json:"spec"`
}

// ErrNotFound 表示没有这个集群 / 版本。
var ErrNotFound = errors.New("没有找到这份部署配置")

// Store 把部署配置按集群名分目录保存：<Dir>/configs/<集群>/index.json 与 v<N>.conf（0600）。
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore 在 dir 下保存；dir 由调用方创建权限（0700）。
func NewStore(dir string) *Store { return &Store{dir: filepath.Join(dir, "configs")} }

func (s *Store) clusterDir(cluster string) string { return filepath.Join(s.dir, cluster) }

// ConfPath 返回某版本 deploy.conf 的磁盘路径（不检查是否存在）。cluster 必须已通过 ValidCluster。
func (s *Store) ConfPath(cluster string, ver int) string {
	return filepath.Join(s.clusterDir(cluster), fmt.Sprintf("v%d.conf", ver))
}

func (s *Store) readIndex(cluster string) ([]Version, error) {
	b, err := os.ReadFile(filepath.Join(s.clusterDir(cluster), "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vs []Version
	if err := json.Unmarshal(b, &vs); err != nil {
		return nil, fmt.Errorf("读取 %s 的配置索引失败：%w", cluster, err)
	}
	return vs, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Save 校验并保存。与最新版本渲染结果完全相同时不新建版本（created=false，返回最新版本）。
func (s *Store) Save(spec Spec, by string, at time.Time) (v Version, created bool, err error) {
	spec = spec.Normalize()
	if errs := spec.Validate(); len(errs) > 0 {
		return Version{}, false, &ValidationError{Errors: errs}
	}
	conf := spec.Render()
	sum := sha256.Sum256([]byte(conf))
	sha := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, err := s.readIndex(spec.Cluster)
	if err != nil {
		return Version{}, false, err
	}
	if n := len(vs); n > 0 && vs[n-1].SHA == sha {
		return vs[n-1], false, nil
	}
	dir := s.clusterDir(spec.Cluster)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Version{}, false, err
	}
	v = Version{Cluster: spec.Cluster, Ver: len(vs) + 1, SHA: sha, By: by, At: at, Spec: spec}
	if err := writeFileAtomic(s.ConfPath(spec.Cluster, v.Ver), []byte(conf), 0o600); err != nil {
		return Version{}, false, err
	}
	idx, _ := json.MarshalIndent(append(vs, v), "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, "index.json"), idx, 0o600); err != nil {
		return Version{}, false, err
	}
	return v, true, nil
}

// List 返回某集群的全部版本（旧 → 新）。
func (s *Store) List(cluster string) ([]Version, error) {
	if !ValidCluster(cluster) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, err := s.readIndex(cluster)
	if vs == nil {
		vs = []Version{}
	}
	return vs, err
}

// Get 返回指定版本；ver <= 0 取最新。
func (s *Store) Get(cluster string, ver int) (Version, error) {
	vs, err := s.List(cluster)
	if err != nil {
		return Version{}, err
	}
	if len(vs) == 0 {
		return Version{}, ErrNotFound
	}
	if ver <= 0 {
		return vs[len(vs)-1], nil
	}
	if ver > len(vs) {
		return Version{}, ErrNotFound
	}
	return vs[ver-1], nil
}

// Clusters 列出有已保存配置的集群名（排序）。
func (s *Store) Clusters() []string {
	ents, err := os.ReadDir(s.dir)
	out := []string{}
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() && ValidCluster(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ValidationError 携带全部校验错误。
type ValidationError struct{ Errors []string }

func (e *ValidationError) Error() string {
	if len(e.Errors) == 1 {
		return e.Errors[0]
	}
	return fmt.Sprintf("配置有 %d 个错误：%s", len(e.Errors), e.Errors[0])
}
