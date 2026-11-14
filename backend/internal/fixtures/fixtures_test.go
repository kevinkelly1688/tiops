// @author: kevinkelly
// @license: Apache License 2.0
// @file: fixtures_test.go
// @time: 2026/10/28

package fixtures

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaskSQL(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM orders WHERE user_id = 10086 AND phone = '13800138000' AND status IN (1, 2, 3)": "SELECT * FROM orders WHERE user_id = ? AND phone = ? AND status IN (...)",
		`select a from t where b = "x y" and c > 3.14`:                                                 "select a from t where b = ? and c > ?",
		"select * from orders_2024 where t1 = 1":                                                       "select * from orders_2024 where t1 = ?", // 标识符中的数字不应被替换
		"insert into t values ('it''s', 1)":                                                            "insert into t values (...)",             // 多值折叠，与 TiDB digest 风格一致
	}
	for in, want := range cases {
		if got := MaskSQL(in); got != want {
			t.Errorf("MaskSQL(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestSanitizeSecrets(t *testing.T) {
	s := NewSanitizer(Options{RedactSecrets: true})
	in := `password: "abc123"
token=xyz
{"secret_key": "k1", "name": "ok"}
api_key: 'q'`
	out := string(s.Apply([]byte(in), nil))
	for _, leak := range []string{"abc123", "xyz", "k1", `'q'`} {
		if strings.Contains(out, leak) {
			t.Errorf("脱敏后仍包含 %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, `"name": "ok"`) {
		t.Errorf("不应误伤普通字段:\n%s", out)
	}
}

func TestSanitizeIPConsistent(t *testing.T) {
	s := NewSanitizer(Options{MaskIP: true})
	out := string(s.Apply([]byte("A=10.0.1.21 B=10.0.1.22 A2=10.0.1.21 local=127.0.0.1 ver=8.5.2"), nil))
	if strings.Contains(out, "10.0.1.2") {
		t.Fatalf("真实 IP 未被替换: %s", out)
	}
	if !strings.Contains(out, "local=127.0.0.1") || !strings.Contains(out, "ver=8.5.2") {
		t.Fatalf("回环地址与版本号不应被替换: %s", out)
	}
	kv := map[string]string{}
	for _, f := range strings.Fields(out) {
		p := strings.SplitN(f, "=", 2)
		kv[p[0]] = p[1]
	}
	if kv["A"] != kv["A2"] {
		t.Errorf("同一个 IP 应映射为同一个假 IP: %v", kv)
	}
	if kv["A"] == kv["B"] {
		t.Errorf("不同 IP 不应映射为同一个假 IP: %v", kv)
	}
}

func TestSanitizeTSVColumns(t *testing.T) {
	s := NewSanitizer(Options{MaskSQLLiterals: true})
	tsv := "Time\tQuery_time\tQuery\n2026-10-05 09:58:01.123\t2.84\tSELECT * FROM t WHERE phone = '13800138000' AND id = 7\n"
	out := string(s.Apply([]byte(tsv), []string{"Query"}))
	if strings.Contains(out, "13800138000") || strings.Contains(out, "id = 7") {
		t.Errorf("Query 列未脱敏: %s", out)
	}
	if !strings.Contains(out, "2026-10-05 09:58:01.123\t2.84\t") {
		t.Errorf("其他列（时间、耗时）不应被改动: %s", out)
	}
}

func TestStoreRoundTripAndTamper(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	rel, sum, err := d.Write("pd/stores", ".json", []byte(`{"count":6}`))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "data/pd/stores.json" {
		t.Errorf("相对路径 = %q", rel)
	}
	m := &Manifest{Schema: SchemaVersion, Cluster: "c"}
	m.Add(ItemRecord{Name: "pd/stores", Kind: "http", File: rel, OK: true, SHA256: sum, Bytes: 11})
	m.Add(ItemRecord{Name: "alert/alerts", Kind: "http", Error: "boom", Optional: true})
	if err := d.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.Read(m2, "pd/stores"); err != nil || string(got) != `{"count":6}` {
		t.Fatalf("读取失败: %v %q", err, got)
	}
	if _, err := d.Read(m2, "alert/alerts"); err == nil {
		t.Error("采集失败的条目不应能读取")
	}
	if len(d.Verify(m2)) != 0 {
		t.Error("未改动时应校验通过")
	}
	// 篡改文件
	if err := os.WriteFile(filepath.Join(root, "data", "pd", "stores.json"), []byte(`{"count":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(d.Verify(m2)) == 0 {
		t.Error("篡改后应校验失败")
	}
	if ok, f, of := m2.Summary(); ok != 1 || f != 0 || of != 1 {
		t.Errorf("Summary = %d %d %d", ok, f, of)
	}
}

func TestValidName(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/../b", "A/b", "/abs", "a//b", "a b"} {
		if ValidName(bad) == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	for _, ok := range []string{"pd/stores", "prom/tikv-cpu", "ctl/os-release", "a.b/c_d"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q 应被接受: %v", ok, err)
		}
	}
}

func TestSanitizeStringOnlyColumn(t *testing.T) {
	s := NewSanitizer(Options{MaskSQLLiterals: true})
	tsv := "Digest\tPlan\nabc\t" + `\tTableFullScan_14\t2400000.00\teq(orders.phone, '13800138000')` + "\n"
	out := string(s.Apply([]byte(tsv), []string{"str:Plan"}))
	if strings.Contains(out, "13800138000") {
		t.Errorf("计划里的字符串字面量应被遮蔽: %s", out)
	}
	if !strings.Contains(out, "2400000.00") {
		t.Errorf("str: 列不应遮蔽数字（estRows）: %s", out)
	}
}
