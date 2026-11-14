// @author: kevinkelly
// @license: Apache License 2.0
// @file: registry.go
// @time: 2026/10/28

package clusters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tidbops/internal/notify"
)

// Options 是注册表的外部约束。
type Options struct {
	// FixturesRoots 是回放数据包的白名单根目录。为空则不允许添加回放集群。
	FixturesRoots []string
	// Max 是集群数量上限（含主集群；独立部署的平台实例不计入），0 取 MaxClusters。
	Max int
	Now func() time.Time
	// RequirePD 要求 live 集群必须填写 PD 地址：独立部署时本机没有 tiup，无法自动探测地址。
	RequirePD bool
	// LocalTiup：本机是 tiup 中控机，允许一键导入本机 tiup 管理的集群（Spec.Local）。
	LocalTiup bool
}

// Registry 是集群注册表。并发安全。
//
// 添加 / 更新是两阶段：PrepareAdd / PrepareUpdate 只校验（不改状态），调用方构造并验证实例成功后再 Put 持久化；
// 失败则什么都没改，实现「热生效、失败回滚」。调用方（Hub）负责把 Prepare→Put 串行化。
type Registry struct {
	mu      sync.RWMutex
	path    string // 空 = 仅内存
	master  []byte
	opts    Options
	primary Spec
	items   []Spec
	keep    []json.RawMessage // 加载时无法识别（id 非法 / 重复）的条目：原样写回，避免保存时静默丢失
	warns   []string
}

type fileDoc struct {
	Version  int               `json:"version"`
	Clusters []json.RawMessage `json:"clusters"`
}

type fileSpec struct {
	Spec
	PasswordEnc string `json:"mysqlPasswordEnc,omitempty"`
}

func aad(id string) string { return "cluster|" + id + "|mysqlPassword" }

// Open 打开（不存在则视为空）注册表。primary 是启动参数构建的主集群虚拟条目（ID / Name / Env / Mode 由调用方给出）；
// 文件里 primary:true 的条目只会把 display / desc 叠加到它上面。
// 加载问题（解密失败、条目不合法）不会让 Open 失败：对应条目带 LoadError，或进入 Warnings。
func Open(path string, master []byte, primary Spec, opts Options) (*Registry, error) {
	if opts.Max <= 0 {
		opts.Max = MaxClusters
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	primary.Primary = true
	if err := Validate(&primary, nil); err != nil {
		return nil, fmt.Errorf("主集群定义不合法: %w", err)
	}
	r := &Registry{path: path, master: master, opts: opts, primary: primary}
	if path == "" {
		return r, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var doc fileDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("clusters.json 不是合法 JSON: %w", err)
	}
	seen := map[string]bool{primary.ID: true, primary.Name: true}
	for i, raw := range doc.Clusters {
		var fs fileSpec
		if err := json.Unmarshal(raw, &fs); err != nil {
			r.warns = append(r.warns, fmt.Sprintf("第 %d 个条目无法解析，已保留原文不加载：%v", i+1, err))
			r.keep = append(r.keep, raw)
			continue
		}
		s := fs.Spec
		if s.Primary {
			if s.ID == primary.ID {
				if s.Display != "" {
					r.primary.Display = s.Display
				}
				r.primary.Desc = s.Desc
				r.primary.Updated, r.primary.UpdatedBy = s.Updated, s.UpdatedBy
			} else {
				r.warns = append(r.warns, fmt.Sprintf("条目 %q 标记为主集群，但当前主集群是 %q（--cluster 是否变更？），已忽略", s.ID, primary.ID))
			}
			continue
		}
		if !reID.MatchString(s.ID) || seen[s.ID] || (s.Name != "" && seen[s.Name]) {
			r.warns = append(r.warns, fmt.Sprintf("条目 id=%q 不合法或与已有集群的 id / name 冲突，已保留原文不加载", s.ID))
			r.keep = append(r.keep, raw)
			continue
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		if fs.PasswordEnc != "" {
			pw, err := notify.OpenSecret(master, fs.PasswordEnc, aad(s.ID))
			if err != nil {
				s.passwordEnc = fs.PasswordEnc
				s.LoadError = "数据库密码无法解密（master.key 是否变更？）：请在集群接入页重新填写密码"
			} else {
				s.MySQLPassword = pw
			}
		}
		s.Primary = false
		// 加载时也做一次校验：不合法（例如 fixtures 不再在白名单下）不丢弃，标记 LoadError，由实例构造阶段呈现为 error 状态。
		chk := s
		if err := Validate(&chk, opts.FixturesRoots); err != nil {
			if s.LoadError == "" {
				s.LoadError = err.Error()
			}
		} else {
			keepErr := s.LoadError
			chk.passwordEnc, chk.LoadError = s.passwordEnc, keepErr
			s = chk
		}
		seen[s.ID], seen[s.Name] = true, true
		r.items = append(r.items, s)
	}
	return r, nil
}

// Warnings 返回加载时的告警。
func (r *Registry) Warnings() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.warns...)
}

// FixturesRoots 返回白名单根目录（副本）。
func (r *Registry) FixturesRoots() []string { return append([]string(nil), r.opts.FixturesRoots...) }

// Primary 返回主集群条目。
func (r *Registry) Primary() Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.primary
}

// List 返回全部集群：主集群在前，其余按添加顺序。
func (r *Registry) List() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listLocked()
}

func (r *Registry) listLocked() []Spec {
	out := make([]Spec, 0, 1+len(r.items))
	out = append(out, r.primary)
	return append(out, r.items...)
}

// Get 按 id 精确查找（区分大小写）。
func (r *Registry) Get(id string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.listLocked() {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}

// Resolve 把审计筛选里的 cluster 参数（id 或 tiup 名）解析成该集群的 {id, name}；找不到则原样返回单元素。
func (r *Registry) Resolve(idOrName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.listLocked() {
		if s.ID == idOrName || s.Name == idOrName {
			if s.ID == s.Name {
				return []string{s.ID}
			}
			return []string{s.ID, s.Name}
		}
	}
	return []string{idOrName}
}

// platformCount 是列表里平台实例的个数（0 或 1），不计入集群数量上限。
func (r *Registry) platformCount() int {
	if r.primary.Platform {
		return 1
	}
	return 0
}

// requirePD 在独立部署时要求 live 集群填写 PD 地址（本机没有 tiup，无法自动探测）。
func (r *Registry) requirePD(s Spec) error {
	if r.opts.RequirePD && !s.Primary && s.Mode == ModeLive && s.PD == "" {
		return invalid("独立部署的平台没有本机 tiup 可用于自动探测：live 集群必须填写 PD 地址（建议同时填 Prometheus / Alertmanager / TiDB status 与只读 SQL 账号，否则对应页面没有数据）")
	}
	return nil
}

func (r *Registry) conflictLocked(s Spec) error {
	for _, o := range r.listLocked() {
		if o.ID == s.ID {
			continue
		}
		if o.ID == s.ID || o.Name == s.Name || o.ID == s.Name || o.Name == s.ID {
			return &Error{Code: ErrExists, Msg: fmt.Sprintf("id / tiup 集群名与已有集群「%s」冲突", o.ID)}
		}
	}
	return nil
}

// PrepareAdd 校验一个手工添加的集群并返回规整后的 Spec（不改变注册表）。手工添加的集群恒为远程接入（只读）。
func (r *Registry) PrepareAdd(in Input, by string) (Spec, error) { return r.prepareAdd(in, by, false) }

// PrepareImport 校验一个从本机 tiup 导入的集群（Spec.Local = true）。本机不是 tiup 中控机时拒绝。
func (r *Registry) PrepareImport(in Input, by string) (Spec, error) {
	if !r.opts.LocalTiup {
		return Spec{}, invalid("本机不是 tiup 中控机（安装时需要 --local-tiup），不能一键导入")
	}
	if in.Mode != nil && *in.Mode != ModeLive {
		return Spec{}, invalid("导入的集群只能是 live 模式")
	}
	return r.prepareAdd(in, by, true)
}

func (r *Registry) prepareAdd(in Input, by string, local bool) (Spec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n := len(r.listLocked()) - r.platformCount(); n >= r.opts.Max {
		return Spec{}, &Error{Code: ErrLimit, Msg: fmt.Sprintf("最多接入 %d 个集群", r.opts.Max)}
	}
	s := Spec{ID: strings.TrimSpace(in.ID)}
	if s.ID != in.ID {
		return Spec{}, invalid("id 不能包含首尾空白")
	}
	in.apply(&s)
	if local {
		s.Local, s.Mode = true, ModeLive
	}
	if err := Validate(&s, r.opts.FixturesRoots); err != nil {
		return Spec{}, err
	}
	if err := r.requirePD(s); err != nil {
		return Spec{}, err
	}
	for _, o := range r.listLocked() {
		if o.ID == s.ID || o.Name == s.Name || o.ID == s.Name || o.Name == s.ID {
			return Spec{}, &Error{Code: ErrExists, Msg: fmt.Sprintf("id / tiup 集群名与已有集群「%s」冲突", o.ID)}
		}
	}
	now := r.opts.Now().UTC()
	s.Created, s.Updated, s.CreatedBy, s.UpdatedBy = now, now, by, by
	return s, nil
}

// PrepareUpdate 校验对 id 的更新，返回（旧条目，新条目）。id 不可改；主集群只能改 display / desc。
func (r *Registry) PrepareUpdate(id string, in Input, by string) (old, next Spec, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.listLocked() {
		if s.ID == id {
			old = s
		}
	}
	if old.ID == "" {
		return Spec{}, Spec{}, &Error{Code: ErrNotFound, Msg: "没有这个集群"}
	}
	if in.ID != "" && in.ID != id {
		return Spec{}, Spec{}, invalid("id 创建后不可修改")
	}
	next = old
	if old.Primary {
		probe := old
		in.apply(&probe)
		probe.Display, probe.Desc = old.Display, old.Desc
		probe.Updated, probe.UpdatedBy = old.Updated, old.UpdatedBy
		if probe != old || in.ClearPassword {
			return Spec{}, Spec{}, &Error{Code: ErrPrimary, Msg: "主集群由启动参数定义：只能修改 display 和 desc"}
		}
		if in.Display != nil {
			next.Display = *in.Display
		}
		if in.Desc != nil {
			next.Desc = *in.Desc
		}
	} else {
		in.apply(&next)
	}
	if err := Validate(&next, r.opts.FixturesRoots); err != nil {
		return Spec{}, Spec{}, err
	}
	if err := r.requirePD(next); err != nil {
		return Spec{}, Spec{}, err
	}
	if err := r.conflictLocked(next); err != nil {
		return Spec{}, Spec{}, err
	}
	if next.passwordEnc == "" {
		next.LoadError = ""
	}
	next.Updated, next.UpdatedBy = r.opts.Now().UTC(), by
	return old, next, nil
}

// Put 持久化一个已通过 Prepare 且实例构造成功的条目（新增或替换）。保存失败则内存状态回滚，返回 store 错误。
func (r *Registry) Put(s Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prevPrimary, prevItems := r.primary, append([]Spec(nil), r.items...)
	switch {
	case s.Primary:
		r.primary.Display, r.primary.Desc, r.primary.Updated, r.primary.UpdatedBy = s.Display, s.Desc, s.Updated, s.UpdatedBy
	default:
		replaced := false
		for i := range r.items {
			if r.items[i].ID == s.ID {
				r.items[i], replaced = s, true
			}
		}
		if !replaced {
			r.items = append(r.items, s)
		}
	}
	if err := r.saveLocked(); err != nil {
		r.primary, r.items = prevPrimary, prevItems
		return &Error{Code: ErrStore, Msg: "保存集群注册表失败：" + err.Error()}
	}
	return nil
}

// Remove 从注册表删除一个非主集群并持久化。
func (r *Registry) Remove(id string) (Spec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == r.primary.ID {
		return Spec{}, &Error{Code: ErrPrimary, Msg: "主集群不能删除"}
	}
	idx := -1
	for i, s := range r.items {
		if s.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return Spec{}, &Error{Code: ErrNotFound, Msg: "没有这个集群"}
	}
	removed := r.items[idx]
	prev := append([]Spec(nil), r.items...)
	r.items = append(append([]Spec(nil), r.items[:idx]...), r.items[idx+1:]...)
	if err := r.saveLocked(); err != nil {
		r.items = prev
		return Spec{}, &Error{Code: ErrStore, Msg: "保存集群注册表失败：" + err.Error()}
	}
	return removed, nil
}

// saveLocked 原子写 clusters.json（0600）。主集群只保存 display / desc（连接信息来自启动参数）。
func (r *Registry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	doc := fileDoc{Version: 1, Clusters: []json.RawMessage{}}
	p := Spec{ID: r.primary.ID, Name: r.primary.Name, Display: r.primary.Display, Desc: r.primary.Desc, Primary: true, Platform: r.primary.Platform, Updated: r.primary.Updated, UpdatedBy: r.primary.UpdatedBy}
	raw, err := json.Marshal(fileSpec{Spec: p})
	if err != nil {
		return err
	}
	doc.Clusters = append(doc.Clusters, raw)
	for _, s := range r.items {
		fs := fileSpec{Spec: s}
		switch {
		case s.MySQLPassword != "":
			enc, err := notify.SealSecret(r.master, s.MySQLPassword, aad(s.ID))
			if err != nil {
				return err
			}
			fs.PasswordEnc = enc
		case s.passwordEnc != "":
			fs.PasswordEnc = s.passwordEnc
		}
		raw, err := json.Marshal(fs)
		if err != nil {
			return err
		}
		doc.Clusters = append(doc.Clusters, raw)
	}
	doc.Clusters = append(doc.Clusters, r.keep...)
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, b)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
