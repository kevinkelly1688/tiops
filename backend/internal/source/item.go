// @author: kevinkelly
// @license: Apache License 2.0
// @file: item.go
// @time: 2026/10/28

// Package source 定义采集层的数据来源接口 Source，以及三种实现：
//
//   - Live   ：真实访问（HTTP GET、白名单命令、只读 SQL、白名单文件），全部只读；
//   - Replay ：从样例数据包（fixtures）读取，开发 / CI / 演示使用；
//   - Recorder：包一层 Live，把响应脱敏后落盘成样例数据包。
//
// 平台的采集代码只依赖 Source 接口，因此「不能直连生产」的开发阶段可以完全走 Replay。
package source

import (
	"context"
	"fmt"
	"strings"
)

// Kind 是请求种类。
type Kind string

const (
	KindHTTP Kind = "http" // 仅 GET
	KindCmd  Kind = "cmd"  // 白名单命令，不经过 shell
	KindSQL  Kind = "sql"  // 只读 SELECT，经 mysql 客户端
	KindFile Kind = "file" // 白名单文件（只读）
)

// Item 描述一次采集请求。Name 是全局唯一的逻辑名，同时决定样例数据包中的文件位置。
type Item struct {
	Name     string
	Kind     Kind
	Purpose  string
	Optional bool // 失败时不影响整体结果（例如集群未部署 TiFlash）

	// HTTP：Endpoint 是逻辑端点（pd / tidb / prom / alert），Path 含查询串，以 / 开头。
	Endpoint string
	Path     string
	// Cmd：argv[0] 为逻辑命令名（如 tiup），真实路径由 Live 配置决定。
	Argv []string
	// SQL：单条 SELECT。
	Query string
	// File：绝对路径。
	File string

	Ext     string   // 文件扩展名，如 .json / .txt / .tsv
	SQLCols []string // 需要做 SQL 字面量脱敏的 TSV 列名
}

// Request 返回请求的文字描述。只含逻辑名，不含真实地址，可以安全写入清单。
func (it Item) Request() string {
	switch it.Kind {
	case KindHTTP:
		return fmt.Sprintf("GET %s:%s", it.Endpoint, it.Path)
	case KindCmd:
		return "EXEC " + strings.Join(it.Argv, " ")
	case KindSQL:
		return "SQL " + oneLine(it.Query)
	case KindFile:
		return "READ " + it.File
	}
	return string(it.Kind)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Source 是采集层的数据来源。
type Source interface {
	// Fetch 取回一个条目的原始内容。
	Fetch(ctx context.Context, it Item) ([]byte, error)
	// Mode 返回 live / replay 等模式名。
	Mode() string
}
