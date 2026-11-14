// @author: kevinkelly
// @license: Apache License 2.0
// @file: audit_test.go
// @time: 2026/10/28

package audit

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Log, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, p
}

func ent(user, action, result string) Entry {
	return Entry{User: user, Role: "dba", IP: "10.0.0.1", Env: "prod", Cluster: "c1", Action: action,
		Target: "tikv-1", Risk: "mid", Summary: "测试 " + action, Result: result}
}

func fill(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(ent("alice", "inst.op", ResultSuccess)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppendChain(t *testing.T) {
	l, p := openTemp(t)
	e1, err := l.Append(ent("alice", "login", ResultSuccess))
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := l.Append(ent("bob", "inst.op", ResultDenied))
	if e1.Seq != 1 || e2.Seq != 2 {
		t.Fatalf("序号不对：%d %d", e1.Seq, e2.Seq)
	}
	if e1.Prev != "" || e2.Prev != e1.Hash || e1.Hash == "" || e2.Hash == e1.Hash {
		t.Fatalf("链不对：%+v %+v", e1, e2)
	}
	if e1.Time.IsZero() {
		t.Fatal("Time 应默认为当前时间")
	}
	if ComputeHash(e2) != e2.Hash {
		t.Fatal("ComputeHash 不一致")
	}
	n, err := l.Verify()
	if err != nil || n != 2 {
		t.Fatalf("Verify = %d, %v", n, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("权限应为 0600：%v", st.Mode())
	}
	if _, err := l.Append(Entry{Result: ResultAllowed}); err == nil {
		t.Fatal("缺 Action 应报错")
	}
	if _, err := l.Append(Entry{Action: "x", Result: "ok"}); err == nil {
		t.Fatal("非法 Result 应报错")
	}
}

func TestReopenContinuesChain(t *testing.T) {
	l, p := openTemp(t)
	fill(t, l, 3)
	last, _ := l.Append(ent("alice", "x", ResultAllowed))
	l.Close()
	if _, err := l.Append(ent("a", "x", ResultAllowed)); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后应返回 ErrClosed：%v", err)
	}
	l2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.LastSeq() != 4 {
		t.Fatalf("LastSeq=%d", l2.LastSeq())
	}
	e, _ := l2.Append(ent("alice", "y", ResultAllowed))
	if e.Seq != 5 || e.Prev != last.Hash {
		t.Fatalf("重开后链未延续：%+v", e)
	}
	if n, err := l2.Verify(); err != nil || n != 5 {
		t.Fatalf("Verify=%d %v", n, err)
	}
}

func TestReopenRepairsTornTail(t *testing.T) {
	l, p := openTemp(t)
	fill(t, l, 3)
	l.Close()
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"seq":4,"time":"2026-01-01T00:00:0`) // 半行
	f.Close()
	l2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	e, err := l2.Append(ent("a", "x", ResultAllowed))
	if err != nil || e.Seq != 4 {
		t.Fatalf("%+v %v", e, err)
	}
	if n, err := l2.Verify(); err != nil || n != 4 {
		t.Fatalf("Verify=%d %v", n, err)
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func writeLines(t *testing.T, p string, lines []string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTamperDetected(t *testing.T) {
	l, p := openTemp(t)
	fill(t, l, 5)
	lines := readLines(t, p)
	lines[2] = strings.Replace(lines[2], "tikv-1", "tikv-9", 1) // 改第 3 条
	writeLines(t, p, lines)
	n, err := l.Verify()
	var ce *ChainError
	if !errors.As(err, &ce) || ce.Seq != 3 || n != 3 {
		t.Fatalf("应发现第 3 条被篡改：n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "第 3 条被篡改或缺失") {
		t.Fatalf("错误信息：%v", err)
	}
}

func TestTamperRehashedDetectedAtNext(t *testing.T) {
	l, p := openTemp(t)
	fill(t, l, 4)
	lines := readLines(t, p)
	var e Entry
	json.Unmarshal([]byte(lines[1]), &e)
	e.Summary = "改过"
	e.Hash = ComputeHash(e) // 攻击者重算本条哈希
	b, _ := json.Marshal(e)
	lines[1] = string(b)
	writeLines(t, p, lines)
	n, err := l.Verify()
	if err == nil || n != 3 {
		t.Fatalf("应在第 3 条处发现断裂：n=%d err=%v", n, err)
	}
}

func TestDeleteLineDetected(t *testing.T) {
	l, p := openTemp(t)
	fill(t, l, 5)
	lines := readLines(t, p)
	lines = append(lines[:2], lines[3:]...) // 删第 3 条
	writeLines(t, p, lines)
	n, err := l.Verify()
	if err == nil || n != 3 || !strings.Contains(err.Error(), "第 3 条被篡改或缺失") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestDeleteTailDetectedWhileRunning(t *testing.T) {
	l, p := openTemp(t)
	fill(t, l, 5)
	lines := readLines(t, p)
	writeLines(t, p, lines[:4])
	// 注意：文件变短后 size 快照大于实际，读到 EOF 即结束
	n, err := l.Verify()
	if err == nil || n != 5 {
		t.Fatalf("应发现第 5 条缺失：n=%d err=%v", n, err)
	}
}

func TestConcurrentAppend(t *testing.T) {
	l, _ := openTemp(t)
	var sinkMu sync.Mutex
	var sunk []uint64
	l.SetSink(func(e Entry) {
		sinkMu.Lock()
		sunk = append(sunk, e.Seq)
		sinkMu.Unlock()
	})
	const G, N = 20, 25
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				if _, err := l.Append(ent("u", "a", ResultSuccess)); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	all, err := l.Query(Filter{}, 1000)
	if err != nil || len(all) != G*N {
		t.Fatalf("len=%d err=%v", len(all), err)
	}
	for i, e := range all { // 倒序，序号应连续
		if e.Seq != uint64(G*N-i) {
			t.Fatalf("序号不连续：位置 %d 为 %d", i, e.Seq)
		}
	}
	if n, err := l.Verify(); err != nil || n != G*N {
		t.Fatalf("Verify=%d %v", n, err)
	}
	for i, s := range sunk {
		if s != uint64(i+1) {
			t.Fatalf("Sink 顺序错误")
		}
	}
	if len(sunk) != G*N {
		t.Fatalf("Sink 次数=%d", len(sunk))
	}
}

func TestSinkPanicDoesNotBreak(t *testing.T) {
	l, _ := openTemp(t)
	l.Sink = func(Entry) { panic("boom") }
	if _, err := l.Append(ent("u", "a", ResultSuccess)); err != nil {
		t.Fatal(err)
	}
	if n, err := l.Verify(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"login password=hunter2 ok":             "login password=*** ok",
		"Token: abcdef123456":                   "Token: ***",
		"secret='a b c' next":                   "secret='***' next",
		`{"password":"p@ss w0rd","user":"x"}`:   `{"password":"***","user":"x"}`,
		"api_key=AKIA123&x=1":                   "api_key=***&x=1",
		"Authorization: Bearer abcdefghijkl123": "Authorization: ***",
		"curl -H 'Bearer abcdefghijkl123'":      "curl -H 'Bearer ***'",
		"mysql://root:pa55@10.0.0.1:4000/test":  "mysql://root:***@10.0.0.1:4000/test",
		"没有敏感信息，只是 passwords 复数":                "没有敏感信息，只是 passwords 复数",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	l, p := openTemp(t)
	e := ent("a", "settings", ResultSuccess)
	e.Summary = "改 webhook token=abc123 secret=xyz"
	got, _ := l.Append(e)
	if strings.Contains(got.Summary, "abc123") || strings.Contains(got.Summary, "xyz") {
		t.Fatalf("Summary 未脱敏：%s", got.Summary)
	}
	b, _ := os.ReadFile(p)
	if bytes.Contains(b, []byte("abc123")) {
		t.Fatal("文件里不应出现明文令牌")
	}
	if _, err := l.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateSummary(t *testing.T) {
	l, _ := openTemp(t)
	e := ent("a", "x", ResultSuccess)
	e.Summary = strings.Repeat("中", 5000)
	got, err := l.Append(e)
	if err != nil || len(got.Summary) > maxSummaryBytes+4 {
		t.Fatalf("len=%d err=%v", len(got.Summary), err)
	}
	if n, err := l.Verify(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestQueryFilters(t *testing.T) {
	l, _ := openTemp(t)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	add := func(i int, user, action, result, env, summary string) {
		e := ent(user, action, result)
		e.Env = env
		e.Summary = summary
		e.Time = base.Add(time.Duration(i) * time.Hour)
		if _, err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	add(0, "alice", "login", ResultSuccess, "prod", "登录成功")
	add(1, "bob", "inst.op", ResultDenied, "prod", "重启 TiKV 被拒")
	add(2, "alice", "inst.op", ResultSuccess, "test", "重启 TiDB")
	add(3, "carol", "scale", ResultFailed, "staging", "扩容失败 REASON")
	add(4, "alice", "config", ResultSuccess, "prod", "改配置")

	q := func(f Filter, limit int) []uint64 {
		t.Helper()
		es, err := l.Query(f, limit)
		if err != nil {
			t.Fatal(err)
		}
		var seqs []uint64
		for _, e := range es {
			seqs = append(seqs, e.Seq)
		}
		return seqs
	}
	eq := func(got []uint64, want ...uint64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
	eq(q(Filter{}, 0), 5, 4, 3, 2, 1) // 倒序
	eq(q(Filter{}, 2), 5, 4)
	eq(q(Filter{User: "alice"}, 10), 5, 3, 1)
	eq(q(Filter{Action: "inst.op"}, 10), 3, 2)
	eq(q(Filter{Result: ResultDenied}, 10), 2)
	eq(q(Filter{Env: "prod"}, 10), 5, 2, 1)
	eq(q(Filter{Since: base.Add(2 * time.Hour)}, 10), 5, 4, 3)
	eq(q(Filter{Until: base.Add(1 * time.Hour)}, 10), 2, 1)
	eq(q(Filter{Since: base.Add(time.Hour), Until: base.Add(3 * time.Hour)}, 10), 4, 3, 2)
	eq(q(Filter{Contains: "reason"}, 10), 4) // 不区分大小写
	eq(q(Filter{Contains: "重启"}, 10), 3, 2)
	eq(q(Filter{User: "alice", Env: "prod", Contains: "配置"}, 10), 5)
	eq(q(Filter{User: "nobody"}, 10))

	// limit 上限 1000
	l2, _ := openTemp(t)
	fill(t, l2, 1010)
	es, _ := l2.Query(Filter{}, 5000)
	if len(es) != 1000 || es[0].Seq != 1010 {
		t.Fatalf("limit 上限不对：%d", len(es))
	}
}

func TestExport(t *testing.T) {
	l, _ := openTemp(t)
	e := ent("alice", "login", ResultSuccess)
	e.Summary = "=cmd|' /C calc'!A0"
	l.Append(e)
	l.Append(ent("bob", "inst.op", ResultDenied))

	var jb bytes.Buffer
	if err := l.Export(&jb, "jsonl"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(jb.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl 行数=%d", len(lines))
	}
	var got Entry
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil || got.User != "alice" || ComputeHash(got) != got.Hash {
		t.Fatalf("%+v %v", got, err)
	}

	var cb bytes.Buffer
	if err := l.Export(&cb, "CSV"); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&cb).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("csv rows=%d err=%v", len(rows), err)
	}
	if rows[0][0] != "seq" || rows[1][2] != "alice" || rows[2][2] != "bob" {
		t.Fatalf("%v", rows)
	}
	if !strings.HasPrefix(rows[1][10], "'=") {
		t.Fatalf("应防公式注入：%q", rows[1][10])
	}

	var fb bytes.Buffer
	if err := l.ExportFiltered(&fb, Filter{User: "bob"}, "jsonl"); err != nil || strings.Count(fb.String(), "\n") != 1 {
		t.Fatalf("%q %v", fb.String(), err)
	}
	if err := l.Export(&jb, "xml"); err == nil {
		t.Fatal("未知格式应报错")
	}
}
