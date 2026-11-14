// @author: kevinkelly
// @license: Apache License 2.0
// @file: buildinfo.go
// @time: 2026/10/28

// Package buildinfo 保存构建时注入的版本信息：
//
//	go build -ldflags "-X tidbops/internal/buildinfo.Version=1.0.0 -X tidbops/internal/buildinfo.Commit=abc123 -X tidbops/internal/buildinfo.BuildTime=2026-10-05T12:00:00Z"
package buildinfo

// 默认值表示「开发构建」。发布包用 ldflags 覆盖。
var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
)
