// @author: kevinkelly
// @license: Apache License 2.0
// @file: profile_test.go
// @time: 2026/10/28

package fakecluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tidbops/internal/source"
)

// 默认预设与 dev 预设没有 TiProxy：display 输出解析后必须与已提交的 fixtures/demo、fixtures/dev 一致
// （给 Profile 增加 TiProxy 字段不能改变它们）。比较的是解析结果而不是原始字节：已提交的 demo 数据包是更早版本导出的，
// TiFlash 行的列间空白略有不同，但解析结果相同。
func TestDisplayTextUnchangedWithoutTiProxy(t *testing.T) {
	for name, p := range map[string]Profile{"demo": DefaultProfile(), "dev": DevProfile()} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", name, "data", "tiup", "cluster-display.txt"))
		if err != nil {
			t.Skipf("缺少 fixtures/%s: %v", name, err)
		}
		if p.TiProxy != 0 {
			t.Fatalf("%s 预设不应有 TiProxy", name)
		}
		got := p.displayText()
		if strings.Contains(got, "tiproxy") {
			t.Errorf("%s 的 display 里不应出现 tiproxy", name)
		}
		if a, b := source.ParseDisplay(got), source.ParseDisplay(string(b)); !reflect.DeepEqual(a, b) {
			t.Errorf("%s 的 display 解析结果与 fixtures 不一致:\n%+v\n%+v", name, a, b)
		}
	}
}

func TestStagingDisplayHasTwoTiProxy(t *testing.T) {
	p := StagingProfile()
	d := p.displayText()
	if n := strings.Count(d, " tiproxy "); n != 2 {
		t.Fatalf("stg 应有 2 个 tiproxy 行，得到 %d:\n%s", n, d)
	}
	if !strings.Contains(d, "10.20.1.41:6000") || !strings.Contains(d, "6000/3080") || !strings.Contains(d, "Total nodes: 14") {
		t.Errorf("display 内容不对:\n%s", d)
	}
	// 已提交的 fixtures/stg 必须与 StagingProfile 一致（逐字节）：改了预设要用 tidbops-fixtures demo --profile stg 重新生成
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "stg", "data", "tiup", "cluster-display.txt"))
	if err == nil && string(b) != d {
		t.Error("fixtures/stg 与 StagingProfile 的 display 输出不一致，请用 tidbops-fixtures demo --profile stg 重新生成")
	}
}
