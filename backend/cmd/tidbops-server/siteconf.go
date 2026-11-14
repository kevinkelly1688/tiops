// @author: kevinkelly
// @license: Apache License 2.0
// @file: siteconf.go
// @time: 2026/10/28

package main

// 平台配置文件（--config，默认 /etc/tidb-ops/platform.json，可选）。目前只有页面展示用的字段：
//
//	{"icp": "京ICP备12345678号-1"}
//
// 只在进程启动时读取，改完需要重启服务。这些都是展示用的非关键配置：文件不存在直接忽略，
// 内容有问题只打警告、不阻止启动（否则一个手滑的备案号会让升级后的健康检查失败、触发自动回滚）。
// 未知字段一律忽略，这样回滚到旧版本时新版本写下的配置不会让旧版本起不来。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	defaultConfigPath = "/etc/tidb-ops/platform.json"
	maxICPLen         = 64 // 字符数；现行备案号（如「京ICP备12345678号-1」）远短于此
)

type siteConf struct {
	ICP string `json:"icp"` // 网站备案号，登录页与页脚展示
}

// loadSiteConf 读取配置文件。文件不存在返回零值与 nil；其余问题返回零值与错误（调用方打警告后继续）。
func loadSiteConf(path string) (siteConf, error) {
	var c siteConf
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return siteConf{}, fmt.Errorf("读取失败: %w", err)
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // Windows 编辑器保存的 UTF-8 BOM
	if err := json.Unmarshal(b, &c); err != nil {
		return siteConf{}, fmt.Errorf("不是合法的 JSON: %w", err)
	}
	c.ICP = strings.TrimSpace(c.ICP)
	if err := checkICP(c.ICP); err != nil {
		return siteConf{}, err
	}
	return c, nil
}

// checkICP 校验备案号：可以为空（不显示）；不含控制字符，长度不超过 maxICPLen。
// 页面按纯文本输出（前端不拼 HTML），这里的限制只是防手滑（比如把整段说明或多行文本贴进来）。
func checkICP(s string) error {
	if utf8.RuneCountInString(s) > maxICPLen {
		return fmt.Errorf("icp 太长（超过 %d 个字符）", maxICPLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("icp 含有换行或控制字符")
		}
	}
	return nil
}
