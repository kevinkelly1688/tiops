// @author: kevinkelly
// @license: Apache License 2.0
// @file: siteconf_test.go
// @time: 2026/10/28

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConf(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "platform.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSiteConf(t *testing.T) {
	// 文件不存在：忽略，不是错误
	if c, err := loadSiteConf(filepath.Join(t.TempDir(), "nope.json")); err != nil || c.ICP != "" {
		t.Fatalf("不存在应返回零值: %+v %v", c, err)
	}
	cases := []struct {
		name, body, want string
	}{
		{"正常", `{"icp":"京ICP备12345678号-1"}`, "京ICP备12345678号-1"},
		{"首尾空白被去掉", `{"icp":"  粤ICP备2024000001号  "}`, "粤ICP备2024000001号"},
		{"空串=不显示", `{"icp":""}`, ""},
		{"没有 icp 字段", `{}`, ""},
		{"未知字段被忽略（回滚到旧版本也能起）", `{"icp":"沪ICP备1号","future":{"a":1}}`, "沪ICP备1号"},
		{"UTF-8 BOM", "\xef\xbb\xbf" + `{"icp":"京ICP备1号"}`, "京ICP备1号"},
	}
	for _, c := range cases {
		got, err := loadSiteConf(writeConf(t, c.body))
		if err != nil || got.ICP != c.want {
			t.Errorf("%s: icp=%q err=%v，期望 %q", c.name, got.ICP, err, c.want)
		}
	}
}

func TestLoadSiteConfRejectsBadContent(t *testing.T) {
	bad := map[string]string{
		"不是 JSON":  `icp: 京ICP备1号`,
		"类型不对":     `{"icp": 123}`,
		"含换行":      `{"icp":"京ICP备1号\n第二行"}`,
		"含控制字符":    `{"icp":"京ICP备1号\u0007"}`,
		"太长":       `{"icp":"` + strings.Repeat("备", maxICPLen+1) + `"}`,
		"整个文件是空文本": ``,
	}
	for name, body := range bad {
		if c, err := loadSiteConf(writeConf(t, body)); err == nil || c.ICP != "" {
			t.Errorf("%s 应报错且不返回备案号: %+v %v", name, c, err)
		}
	}
	// 恰好 maxICPLen 个字符（按字符数而非字节数）可以
	if _, err := loadSiteConf(writeConf(t, `{"icp":"`+strings.Repeat("备", maxICPLen)+`"}`)); err != nil {
		t.Errorf("恰好 %d 个字符应通过: %v", maxICPLen, err)
	}
}

func TestLoadSiteConfUnreadablePath(t *testing.T) {
	// 路径是目录：读取失败应返回错误（而不是当作「不存在」静默忽略）
	if _, err := loadSiteConf(t.TempDir()); err == nil {
		t.Error("路径是目录时应报错")
	}
}
