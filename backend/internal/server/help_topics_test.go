// @author: kevinkelly
// @license: Apache License 2.0
// @file: help_topics_test.go
// @time: 2026/10/28

package server_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 帮助主题与文件一致：frontend/js/help.js 里 TOPICS 列出的每个 id 都要有 frontend/help/<id>.md，
// 反过来 help/ 目录里的每个 .md（README 除外）也都要在 TOPICS 里登记，否则页面上找不到入口。
// 找不到仓库目录（例如只拷贝了 backend 单独跑测试）时跳过。
func TestHelpTopicsMatchFiles(t *testing.T) {
	root := filepath.Join("..", "..", "..", "frontend")
	js, err := os.ReadFile(filepath.Join(root, "js", "help.js"))
	if err != nil {
		t.Skipf("找不到 frontend/js/help.js：%v", err)
	}
	re := regexp.MustCompile(`\{g: '[^']*', id: '([a-z]+)', t: '[^']*'\}`)
	ids := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(js), -1) {
		ids[m[1]] = true
	}
	if len(ids) < 10 {
		t.Fatalf("只解析到 %d 个主题，正则可能失效", len(ids))
	}
	files := map[string]bool{}
	ents, err := os.ReadDir(filepath.Join(root, "help"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".md") && n != "README.md" {
			files[strings.TrimSuffix(n, ".md")] = true
		}
	}
	var missing, orphan []string
	for id := range ids {
		if !files[id] {
			missing = append(missing, id)
		}
	}
	for f := range files {
		if !ids[f] {
			orphan = append(orphan, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(orphan)
	if len(missing) > 0 {
		t.Errorf("help.js 登记了主题但没有对应文件：%v", missing)
	}
	if len(orphan) > 0 {
		t.Errorf("frontend/help 里有文件没有在 help.js 登记：%v", orphan)
	}
	// 每个页面文档都应有统一结构的几个一级小节（见 help/README.md）
	for id := range ids {
		b, err := os.ReadFile(filepath.Join(root, "help", id+".md"))
		if err != nil {
			continue
		}
		if id == "quickstart" || id == "faq" { // 入门与问答不是页面文档，结构不同
			continue
		}
		for _, h := range []string{"## 这个页面做什么", "## 怎么用"} {
			if !strings.Contains(string(b), h) {
				t.Errorf("help/%s.md 缺少小节「%s」", id, h)
			}
		}
	}
}
