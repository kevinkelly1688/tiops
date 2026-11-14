// @author: kevinkelly
// @license: Apache License 2.0
// @file: watermark_test.go
// @time: 2026/10/28

package server

import (
	"strings"
	"testing"
)

func TestWatermarkValidate(t *testing.T) {
	ok := defaultWatermark()
	ok.Enabled, ok.Text = true, "  内部资料 {display} {date}  "
	got, bad := wmValidate(ok)
	if bad != "" || got.Text != "内部资料 {display} {date}" {
		t.Fatalf("合法设置应通过并去掉首尾空白：%q %q", got.Text, bad)
	}

	cases := map[string]func(*Watermark){
		"启用但文字为空": func(w *Watermark) { w.Text = " " },
		"文字过长":    func(w *Watermark) { w.Text = strings.Repeat("字", wmMaxTextRunes+1) },
		"含换行":     func(w *Watermark) { w.Text = "a\nb" },
		"未知变量":    func(w *Watermark) { w.Text = "{ip}" },
		"透明度过低":   func(w *Watermark) { w.Opacity = 0.01 },
		"透明度过高":   func(w *Watermark) { w.Opacity = 0.9 },
		"字号过小":    func(w *Watermark) { w.FontSize = 9 },
		"角度越界":    func(w *Watermark) { w.Angle = 91 },
		"颜色格式":    func(w *Watermark) { w.Color = "red" },
		"间距过小":    func(w *Watermark) { w.GapX = 10 },
		"间距过大":    func(w *Watermark) { w.GapY = 401 },
	}
	for name, mut := range cases {
		w := defaultWatermark()
		w.Enabled, w.Text = true, "{user}"
		mut(&w)
		if _, bad := wmValidate(w); bad == "" {
			t.Errorf("%s：应被拒绝", name)
		}
	}

	// 关闭时允许文字为空
	off := defaultWatermark()
	off.Text = ""
	if _, bad := wmValidate(off); bad != "" {
		t.Errorf("关闭时文字可以为空：%s", bad)
	}
}

func TestWatermarkStorePersist(t *testing.T) {
	dir := t.TempDir()
	st := openWMStore(dir)
	if st.get().Enabled {
		t.Fatal("默认应关闭")
	}
	w := defaultWatermark()
	w.Enabled, w.Text, w.Color = true, "{user} {time}", "#FF0000"
	w, bad := wmValidate(w)
	if bad != "" {
		t.Fatal(bad)
	}
	if err := st.set(w); err != nil {
		t.Fatal(err)
	}
	got := openWMStore(dir).get()
	if !got.Enabled || got.Text != "{user} {time}" || got.Color != "#ff0000" {
		t.Fatalf("重新打开应读到保存的值：%+v", got)
	}
}
