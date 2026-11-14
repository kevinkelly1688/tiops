// @author: kevinkelly
// @license: Apache License 2.0
// @file: manifest.go
// @time: 2026/10/28

// Package fixtures 定义「样例数据包」的格式：manifest.json + data/ 目录。
//
// 一个样例数据包记录了某个集群在某一时刻、通过只读方式采集到的全部外部响应
// （PD API、Prometheus、TiDB 状态端口、只读 SQL、中控机命令输出），
// 用于在不能直连生产的开发阶段，让采集层以「回放」方式工作。
package fixtures

import (
	"fmt"
	"sort"
	"time"
)

const (
	// SchemaVersion 是 manifest 的格式版本。
	SchemaVersion = 1
	// ManifestName 是清单文件名。
	ManifestName = "manifest.json"
	// DataDir 是数据文件所在子目录。
	DataDir = "data"
)

// Options 记录导出时启用的脱敏选项，便于接收方知道数据被如何处理过。
type Options struct {
	MaskIP          bool `json:"mask_ip"`
	MaskSQLLiterals bool `json:"mask_sql_literals"`
	RedactSecrets   bool `json:"redact_secrets"`
}

// ItemRecord 是清单里的一条记录，对应一次采集请求。
type ItemRecord struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Purpose  string `json:"purpose,omitempty"`
	Request  string `json:"request"` // 请求的文字描述，只含逻辑名，不含真实地址
	File     string `json:"file,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Bytes    int    `json:"bytes"`
	SHA256   string `json:"sha256,omitempty"`
}

// Attestation 是「只读声明」：列出本次导出实际允许 / 执行过的全部操作种类，
// 供使用者在发送数据包之前审阅。
type Attestation struct {
	HTTPMethods []string `json:"http_methods"`
	Commands    []string `json:"commands"`
	SQL         []string `json:"sql"`
	Files       []string `json:"files"`
}

// Manifest 是样例数据包的清单。
type Manifest struct {
	Schema         int          `json:"schema"`
	Tool           string       `json:"tool"`
	ToolVersion    string       `json:"tool_version"`
	Cluster        string       `json:"cluster"`
	ClusterVersion string       `json:"cluster_version,omitempty"`
	CollectedAt    time.Time    `json:"collected_at"`
	Options        Options      `json:"options"`
	Items          []ItemRecord `json:"items"`
	ReadOnly       Attestation  `json:"read_only"`
}

// Find 按名称查找记录。
func (m *Manifest) Find(name string) *ItemRecord {
	for i := range m.Items {
		if m.Items[i].Name == name {
			return &m.Items[i]
		}
	}
	return nil
}

// Add 追加（或覆盖同名）记录，并保持按名称排序，使 manifest 内容稳定。
func (m *Manifest) Add(r ItemRecord) {
	if old := m.Find(r.Name); old != nil {
		*old = r
	} else {
		m.Items = append(m.Items, r)
	}
	sort.Slice(m.Items, func(i, j int) bool { return m.Items[i].Name < m.Items[j].Name })
}

// Summary 返回「成功 / 失败（必需）/ 失败（可选）」数量。
func (m *Manifest) Summary() (ok, failed, optionalFailed int) {
	for _, it := range m.Items {
		switch {
		case it.OK:
			ok++
		case it.Optional:
			optionalFailed++
		default:
			failed++
		}
	}
	return
}

func (m *Manifest) String() string {
	ok, f, of := m.Summary()
	return fmt.Sprintf("cluster=%s items=%d ok=%d failed=%d optional_failed=%d", m.Cluster, len(m.Items), ok, f, of)
}
