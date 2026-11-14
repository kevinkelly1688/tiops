// @author: kevinkelly
// @license: Apache License 2.0
// @file: none.go
// @time: 2026/10/28

package source

import (
	"context"
	"errors"
)

// ErrNoSource 是 Unconfigured 对任何请求返回的错误。
var ErrNoSource = errors.New("平台尚未接入集群")

// Unconfigured 是独立部署（--standalone）时平台实例自己的数据源：它不代表任何集群，所有请求都返回 ErrNoSource。
// 集群数据来自「集群接入」里各集群自己的 Source。
type Unconfigured struct{}

// Fetch 实现 Source。
func (Unconfigured) Fetch(context.Context, Item) ([]byte, error) { return nil, ErrNoSource }

// Mode 实现 Source。
func (Unconfigured) Mode() string { return "live" }
