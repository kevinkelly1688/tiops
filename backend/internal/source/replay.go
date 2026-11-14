// @author: kevinkelly
// @license: Apache License 2.0
// @file: replay.go
// @time: 2026/10/28

package source

import (
	"context"
	"encoding/json"
	"fmt"

	"tidbops/internal/fixtures"
)

// Replay 从样例数据包读取，不发出任何外部请求。
// 它是开发 / CI / 演示阶段的默认 Source。
type Replay struct {
	dir fixtures.Dir
	m   *fixtures.Manifest
}

// NewReplay 打开一个样例数据包。
func NewReplay(root string) (*Replay, error) {
	m, err := fixtures.LoadManifest(root)
	if err != nil {
		return nil, err
	}
	return &Replay{dir: fixtures.Dir{Root: root}, m: m}, nil
}

// Mode 实现 Source。
func (r *Replay) Mode() string { return "replay" }

// Manifest 返回数据包清单。
func (r *Replay) Manifest() *fixtures.Manifest { return r.m }

// Fetch 实现 Source：按条目名读取，并校验哈希。
func (r *Replay) Fetch(_ context.Context, it Item) ([]byte, error) {
	return r.dir.Read(r.m, it.Name)
}

// GetJSON 是便捷方法：读取条目并解析为 JSON。
func (r *Replay) GetJSON(ctx context.Context, name string, v any) error {
	b, err := r.Fetch(ctx, Item{Name: name})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", name, err)
	}
	return nil
}
