// @author: kevinkelly
// @license: Apache License 2.0
// @file: lb_test.go
// @time: 2026/10/28

package lb

import (
	"reflect"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

func TestNormalizeAddr(t *testing.T) {
	ok := map[string]string{
		"10.0.0.5:3306":           "10.0.0.5:3306",
		" 10.0.0.5:3306 ":         "10.0.0.5:3306",
		"LB.Example.com:3306":     "lb.example.com:3306",
		"slb-1.cn-north.aliyun:1": "slb-1.cn-north.aliyun:1",
		"[::1]:3306":              "[::1]:3306",
		"db:65535":                "db:65535",
	}
	for in, want := range ok {
		got, err := NormalizeAddr(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAddr(%q) = %q, %v；want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "   ", "10.0.0.5", "10.0.0.5:", ":3306", "10.0.0.5:0", "10.0.0.5:65536", "10.0.0.5:-1", "10.0.0.5:+80", "10.0.0.5:80a",
		"http://10.0.0.5:3306", "tcp://lb:3306", "root:pw@10.0.0.5:3306", "user@lb:3306", "lb:3306/path", "lb:3306?x=1",
		"10.0.0.5 :3306", "lb :3306", "lb:33 06", "10.0.0:3306", "-bad.example.com:80", "bad_.example.com:80", "a..b:80", "lb\t:3306",
		"::1:3306", strings.Repeat("a", 64) + ".com:80",
	}
	for _, in := range bad {
		if got, err := NormalizeAddr(in); err == nil {
			t.Errorf("NormalizeAddr(%q) 应失败，得到 %q", in, got)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"ha", "haproxy-prod", "a1", "x" + strings.Repeat("y", 31)} {
		if !ValidID(id) {
			t.Errorf("%q 应合法", id)
		}
	}
	for _, id := range []string{"", "a", "1ab", "-ab", "Ab", "a_b", "a b", "x" + strings.Repeat("y", 32), "中文"} {
		if ValidID(id) {
			t.Errorf("%q 应不合法", id)
		}
	}
}

func TestApplyAddValidatesAndNormalizes(t *testing.T) {
	f, changed, err := Apply(Fields{}, Input{Type: sp("haproxy"), Name: sp(" 生产 HAProxy "), Addr: sp("VIP.example.com:3306"), Backends: &[]string{"10.0.1.11:4000", "10.0.1.11:4000", "10.0.1.12:4000"}, Note: sp(" 备注 ")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "生产 HAProxy" || f.Addr != "vip.example.com:3306" || f.Note != "备注" || !reflect.DeepEqual(f.Backends, []string{"10.0.1.11:4000", "10.0.1.12:4000"}) {
		t.Errorf("规整结果不对: %+v", f)
	}
	if len(changed) != 5 {
		t.Errorf("新增时全部字段都算变更: %v", changed)
	}
	// 新增缺必填
	for name, in := range map[string]Input{
		"无 type": {Name: sp("n"), Addr: sp("h:1")}, "无 name": {Type: sp("lvs"), Addr: sp("h:1")}, "无 addr": {Type: sp("lvs"), Name: sp("n")},
		"非法 type": {Type: sp("envoy"), Name: sp("n"), Addr: sp("h:1")}, "空 name": {Type: sp("lvs"), Name: sp("  "), Addr: sp("h:1")},
		"长 name":  {Type: sp("lvs"), Name: sp(strings.Repeat("长", 41)), Addr: sp("h:1")},
		"长 note":  {Type: sp("lvs"), Name: sp("n"), Addr: sp("h:1"), Note: sp(strings.Repeat("长", 201))},
		"坏 addr":  {Type: sp("lvs"), Name: sp("n"), Addr: sp("http://h:1")},
		"坏后端":     {Type: sp("lvs"), Name: sp("n"), Addr: sp("h:1"), Backends: &[]string{"h"}},
		"换行 name": {Type: sp("lvs"), Name: sp("a\nb"), Addr: sp("h:1")},
	} {
		if _, _, err := Apply(Fields{}, in, true); err == nil {
			t.Errorf("%s 应失败", name)
		}
	}
	// 后端上限：64 可以，65 不行
	var l65 []string
	for i := 1; i <= 65; i++ {
		l65 = append(l65, "h"+itoa(i)+".example.com:4000")
	}
	l64 := l65[:64]
	if _, _, err := Apply(Fields{}, Input{Type: sp("lvs"), Name: sp("n"), Addr: sp("h:1"), Backends: &l64}, true); err != nil {
		t.Errorf("64 个后端应通过: %v", err)
	}
	if _, _, err := Apply(Fields{}, Input{Type: sp("lvs"), Name: sp("n"), Addr: sp("h:1"), Backends: &l65}, true); err == nil {
		t.Error("65 个后端应失败")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

func TestApplyUpdatePartial(t *testing.T) {
	base := Fields{Type: "haproxy", Name: "n", Addr: "h:1", Backends: []string{"a:1"}, Note: "x"}
	f, changed, err := Apply(base, Input{Name: sp("m"), Backends: &[]string{"a:1"}}, false)
	if err != nil || f.Name != "m" || f.Addr != "h:1" || !reflect.DeepEqual(changed, []string{"name"}) {
		t.Errorf("部分更新: %+v %v %v", f, changed, err)
	}
	empty := []string{}
	f, changed, _ = Apply(base, Input{Backends: &empty}, false)
	if len(f.Backends) != 0 || f.Backends == nil || !reflect.DeepEqual(changed, []string{"backends"}) {
		t.Errorf("清空后端: %+v %v", f, changed)
	}
	if _, changed, _ := Apply(base, Input{}, false); len(changed) != 0 {
		t.Errorf("无字段 = 无变化: %v", changed)
	}
}

func TestCompare(t *testing.T) {
	es := []Entry{
		{ID: "a", Backends: []string{"10.0.1.11:4000", "10.0.1.99:4000"}},
		{ID: "b"}, // 没有后端：不检查
		{ID: "c", Backends: []string{"10.0.1.11:4000", "10.0.1.12:4000", "10.0.1.13:4000"}},
		{ID: "d", Backends: []string{"HOST.Example:4000"}},
	}
	got := Compare(es, []string{"10.0.1.11:4000", "10.0.1.12:4000", "10.0.1.13:4000"})
	if len(got) != 3 || got[0].ID != "a" || got[1].ID != "c" || got[2].ID != "d" {
		t.Fatalf("应只检查有后端的登记: %+v", got)
	}
	if !reflect.DeepEqual(got[0].Missing, []string{"10.0.1.12:4000", "10.0.1.13:4000"}) || !reflect.DeepEqual(got[0].Extra, []string{"10.0.1.99:4000"}) {
		t.Errorf("a: %+v", got[0])
	}
	if len(got[1].Missing) != 0 || len(got[1].Extra) != 0 || got[1].Missing == nil || got[1].Extra == nil {
		t.Errorf("c 应一致且数组非 nil: %+v", got[1])
	}
	if len(got[2].Missing) != 3 || !reflect.DeepEqual(got[2].Extra, []string{"HOST.Example:4000"}) {
		t.Errorf("d: %+v", got[2])
	}
	// 主机大小写不敏感
	if c := Compare([]Entry{{ID: "x", Backends: []string{"Db1:4000"}}}, []string{"db1:4000"}); len(c[0].Missing) != 0 || len(c[0].Extra) != 0 {
		t.Errorf("大小写应归一: %+v", c)
	}
}

func TestDescribe(t *testing.T) {
	e := Entry{Name: "haproxy-prod", Type: "haproxy", Addr: "10.0.0.5:3306"}
	if got := Describe(e); got != "haproxy-prod（HAProxy，10.0.0.5:3306）" {
		t.Errorf("Describe: %q", got)
	}
	if got := DescribeAll([]Entry{e, {Name: "s", Type: "slb", Addr: "x:1"}}); len(got) != 2 || got[1] != "s（云 SLB，x:1）" {
		t.Errorf("DescribeAll: %v", got)
	}
}
