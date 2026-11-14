// @author: kevinkelly
// @license: Apache License 2.0
// @file: runner_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// directRunner 构造一个把 tiup / mysql 都指向测试二进制（helper 模式）的 DirectRunner。
func directRunner(t *testing.T, logs *[]string) (DirectRunner, Policy) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pol := testPolicy(t.TempDir())
	pol.TiupBin, pol.MySQLBin = exe, exe
	var mu sync.Mutex
	r := DirectRunner{Pol: pol, Topo: StaticTopo(testTopo()), Key: testKey}
	if logs != nil {
		r.Log = func(s string) {
			mu.Lock()
			*logs = append(*logs, s)
			mu.Unlock()
		}
	}
	return r, pol
}

func collect() (func(string), func() []string) {
	var mu sync.Mutex
	var lines []string
	return func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() },
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), lines...) }
}

func TestDirectRunnerReadCommand(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "must-not-leak")
	t.Setenv("MYSQL_PWD", "must-not-leak-either")
	var logs []string
	r, _ := directRunner(t, &logs)
	emit, got := collect()
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}, Actor: "alice"}, emit)
	if err != nil || res.Exit != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "ARGS:cluster|display|"+testCluster+"\n") {
		t.Errorf("argv 不符: %q", res.Output)
	}
	if len(got()) < 2 || got()[0] != "ARGS:cluster|display|"+testCluster {
		t.Errorf("emit 应逐行回调: %q", got())
	}
	// 环境变量最小化。
	var envLine string
	for _, l := range strings.Split(res.Output, "\n") {
		if strings.HasPrefix(l, "ENV:") {
			envLine = l[4:]
		}
	}
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "TIUP_HOME": true, "SYSTEMROOT": true, "USERPROFILE": true, "TEMP": true, "TMP": true, "": true}
	for _, n := range strings.Split(envLine, ",") {
		if !allowed[n] && !strings.HasPrefix(n, "=") {
			t.Errorf("子进程环境里出现了不该有的变量 %q（全部: %s）", n, envLine)
		}
	}
	if strings.Contains(envLine, "SECRET_TOKEN") || strings.Contains(envLine, "MYSQL_PWD") {
		t.Errorf("不得继承父进程环境: %s", envLine)
	}
	if !strings.Contains(envLine, "LANG") || !strings.Contains(envLine, "PATH") {
		t.Errorf("应带 PATH 与 LANG: %s", envLine)
	}
	// 审计日志。
	j := strings.Join(logs, "\n")
	if !strings.Contains(j, "exec start kind=cluster.display") || !strings.Contains(j, "exec end") || !strings.Contains(j, "actor=alice") {
		t.Errorf("日志: %s", j)
	}
}

func TestDirectRunnerWriteRequiresToken(t *testing.T) {
	var logs []string
	r, pol := directRunner(t, &logs)
	r.Pol.TiupBin = "/nonexistent/tiup-must-not-run"
	cmd := Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}
	ctx := context.Background()

	for name, req := range map[string]Request{
		"无令牌":        {Cmd: cmd, ChangeID: "CHG-1"},
		"空 changeID": {Cmd: cmd, Token: Sign(testKey, cmd, "CHG-1", time.Now().Add(time.Minute))},
		"伪造令牌":       {Cmd: cmd, ChangeID: "CHG-1", Token: "v1.9999999999.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		"别的命令的令牌":    {Cmd: cmd, ChangeID: "CHG-1", Token: Sign(testKey, Cmd{Kind: KindClusterStop, Cluster: testCluster, Nodes: cmd.Nodes}, "CHG-1", time.Now().Add(time.Minute))},
		"过期令牌":       {Cmd: cmd, ChangeID: "CHG-1", Token: Sign(testKey, cmd, "CHG-1", time.Now().Add(-time.Minute))},
		"别的变更单":      {Cmd: cmd, ChangeID: "CHG-2", Token: Sign(testKey, cmd, "CHG-1", time.Now().Add(time.Minute))},
		"别的密钥":       {Cmd: cmd, ChangeID: "CHG-1", Token: Sign([]byte("another-key-another-key-another!"), cmd, "CHG-1", time.Now().Add(time.Minute))},
	} {
		res, err := r.Run(ctx, req, nil)
		if err == nil || res.Exit != -1 || res.Err == "" {
			t.Errorf("%s: 应被拒绝: %+v %v", name, res, err)
		}
		if strings.Contains(res.Err, "无法启动") {
			t.Errorf("%s: 在令牌验证之前就尝试启动了进程", name)
		}
	}
	// 篡改命令（令牌是为另一份 Nodes 签的）。
	tok := Sign(testKey, cmd, "CHG-1", time.Now().Add(time.Minute))
	tampered := cmd
	tampered.Nodes = []string{"10.0.2.12:20160"}
	if _, err := r.Run(ctx, Request{Cmd: tampered, ChangeID: "CHG-1", Token: tok}, nil); err == nil {
		t.Error("篡改命令后应拒绝")
	}
	// 没有配置密钥：写命令一律拒绝。
	r2 := r
	r2.Key = nil
	if _, err := r2.Run(ctx, Request{Cmd: cmd, ChangeID: "CHG-1", Token: tok}, nil); err == nil {
		t.Error("未配置密钥应拒绝写命令")
	}
	// 日志里不能出现令牌，且被拒绝的请求没有 start 记录；伪造的换行不能拆成多行日志。
	_, _ = r.Run(ctx, Request{Cmd: cmd, ChangeID: "x\nexec start FORGED", Actor: "a\nb", Token: tok}, nil)
	for _, l := range logs {
		if strings.Contains(l, tok) || strings.Contains(l, "\n") {
			t.Errorf("日志不安全: %q", l)
		}
		if strings.HasPrefix(l, "exec start") {
			t.Errorf("被拒绝的请求不应有 start 日志: %q", l)
		}
	}
	_ = pol
}

func TestDirectRunnerWriteWithToken(t *testing.T) {
	r, _ := directRunner(t, nil)
	cmd := Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160", "10.0.2.12:20160"}}
	res, err := r.Run(context.Background(), signedReq(cmd), nil)
	if err != nil || res.Exit != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "ARGS:cluster|restart|"+testCluster+"|-N|10.0.2.11:20160,10.0.2.12:20160|-y\n") {
		t.Errorf("argv 不符: %q", res.Output)
	}
}

func TestDirectRunnerClockAndConsume(t *testing.T) {
	r, _ := directRunner(t, nil)
	cmd := Cmd{Kind: KindClusterPrune, Cluster: testCluster}
	req := signedReq(cmd)
	r.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := r.Run(context.Background(), req, nil); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Errorf("时钟超过有效期应拒绝: %v", err)
	}
	r.Now = nil
	calls := 0
	r.Consume = func(changeID, token string) error {
		calls++
		if calls > 1 {
			return errors.New("令牌已使用")
		}
		return nil
	}
	if _, err := r.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), req, nil); err == nil {
		t.Error("一次性令牌第二次应被拒绝")
	}
	// 读命令不会消耗令牌。
	calls = 0
	if _, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, nil); err != nil || calls != 0 {
		t.Errorf("读命令不应触发 Consume: %v %d", err, calls)
	}
}

func TestDirectRunnerRejectsBeforeExec(t *testing.T) {
	r, _ := directRunner(t, nil)
	r.Pol.TiupBin = "/nonexistent/tiup-must-not-run"
	for _, c := range []Cmd{
		{Kind: "cluster.destroy", Cluster: testCluster},
		{Kind: "bash", Cluster: testCluster},
		{Kind: KindClusterDisplay, Cluster: "../x"},
		{Kind: KindClusterDisplay, Cluster: testCluster, Nodes: []string{"a:1"}},
	} {
		res, err := r.Run(context.Background(), signedReq(c), nil)
		if err == nil || res.Exit != -1 || strings.Contains(res.Err, "无法启动") {
			t.Errorf("%+v 应在启动进程之前被拒绝: %+v %v", c, res, err)
		}
	}
}

func TestDirectRunnerNonZeroExit(t *testing.T) {
	r, _ := directRunner(t, nil)
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: "failc"}}, nil)
	if err != nil {
		t.Fatalf("命令已执行、以非 0 退出，不应返回 error: %v", err)
	}
	if res.Exit != 3 || res.Err != "退出码 3" {
		t.Errorf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "boom on stderr") {
		t.Errorf("stderr 应与 stdout 合并: %q", res.Output)
	}
}

func TestDirectRunnerTimeout(t *testing.T) {
	r, _ := directRunner(t, nil)
	r.Pol.Timeouts = map[string]time.Duration{KindClusterDisplay: 400 * time.Millisecond}
	start := time.Now()
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: "slowc"}}, nil)
	if err == nil || res.Exit != -1 || !strings.Contains(res.Err, "超过") {
		t.Fatalf("应超时: %+v %v", res, err)
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("超时后进程没有被及时终止: %v", time.Since(start))
	}
}

func TestDirectRunnerCancel(t *testing.T) {
	r, _ := directRunner(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := r.Run(ctx, Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: "slowc"}}, nil)
	if err == nil || res.Exit != -1 {
		t.Fatalf("取消应返回错误: %+v %v", res, err)
	}
}

func TestDirectRunnerStartFailure(t *testing.T) {
	r, _ := directRunner(t, nil)
	r.Pol.TiupBin = "/nonexistent/tiup"
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, nil)
	if err == nil || res.Exit != -1 || !strings.Contains(res.Err, "无法启动") {
		t.Errorf("%+v %v", res, err)
	}
}

func TestDirectRunnerOutputCap(t *testing.T) {
	r, _ := directRunner(t, nil)
	emit, got := collect()
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: "bigc"}}, emit)
	if err != nil || res.Exit != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(res.Output) > MaxOutput+200 {
		t.Errorf("输出未截断: %d", len(res.Output))
	}
	if !strings.Contains(res.Output, "截断") {
		t.Error("应带截断提示")
	}
	total := 0
	for _, l := range got() {
		total += len(l)
	}
	if total > MaxOutput+500 {
		t.Errorf("emit 也应受上限约束: %d", total)
	}
	if !strings.Contains(strings.Join(got(), "\n"), "截断") {
		t.Error("emit 应收到截断提示")
	}
}

func TestDirectRunnerPasswordNeverLeaks(t *testing.T) {
	t.Setenv("TIDBOPS_DB_PWD", "topsecretpw")
	var logs []string
	r, _ := directRunner(t, &logs)
	emit, got := collect()
	cmd := Cmd{Kind: KindSQLSetGlobal, Cluster: testCluster, Key: "tidb_slow_log_threshold", Value: "500"}
	res, err := r.Run(context.Background(), signedReq(cmd), emit)
	if err != nil || res.Exit != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Contains(res.Output, "topsecretpw") || strings.Contains(strings.Join(got(), "\n"), "topsecretpw") || strings.Contains(strings.Join(logs, "\n"), "topsecretpw") {
		t.Fatal("密码泄露到了输出 / emit / 日志")
	}
	if !strings.Contains(res.Output, "PWD-IS:***") {
		t.Errorf("子进程应收到 MYSQL_PWD，且输出里的密码被替换: %q", res.Output)
	}
	if !strings.Contains(res.Output, "SET GLOBAL tidb_slow_log_threshold = 500") || strings.Contains(res.Output, "ARGS:--batch|-h|10.0.1.11|-P|4000|-u|tidbops_ops|-e|SET GLOBAL tidb_slow_log_threshold = 500\nENV:") == false {
		t.Errorf("mysql argv: %q", res.Output)
	}
	if !strings.Contains(res.Output, "MYSQL_PWD") {
		t.Errorf("ENV 里应有 MYSQL_PWD: %q", res.Output)
	}
}

func TestDirectRunnerTempFileLifecycle(t *testing.T) {
	r, pol := directRunner(t, nil)
	edit := "global:\n  user: tidb\n  ssh_port: 22\nserver_configs:\n  tikv:\n    storage.block-cache.capacity: 16GB\n"
	res, err := r.Run(context.Background(), signedReq(Cmd{Kind: KindClusterEditCfg, Cluster: testCluster, Content: edit}), nil)
	if err != nil || res.Exit != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(res.Output, "--topology-file|") || !strings.Contains(res.Output, "|-y\n") {
		t.Errorf("argv: %q", res.Output)
	}
	var fileLine string
	for _, l := range strings.Split(res.Output, "\n") {
		if strings.HasPrefix(l, "FILE:") {
			fileLine = l
		}
	}
	if fileLine == "" {
		t.Fatalf("子进程应能读到临时文件: %q", res.Output)
	}
	if runtime.GOOS != "windows" && !strings.HasPrefix(fileLine, "FILE:-rw-------|") {
		t.Errorf("执行时临时文件权限应为 0600: %s", fileLine)
	}
	if !strings.HasSuffix(fileLine, "|global:") {
		t.Errorf("临时文件内容: %s", fileLine)
	}
	if n := countFiles(t, pol.TempDir); n != 0 {
		t.Errorf("执行后临时文件应被删除，剩 %d 个", n)
	}
	// 失败的执行同样要清理。
	r.Pol.TiupBin = "/nonexistent/tiup"
	_, _ = r.Run(context.Background(), signedReq(Cmd{Kind: KindClusterEditCfg, Cluster: testCluster, Content: edit}), nil)
	if n := countFiles(t, pol.TempDir); n != 0 {
		t.Errorf("启动失败后临时文件应被删除，剩 %d 个", n)
	}
}

func TestDirectRunnerScaleOutAndCheckFiles(t *testing.T) {
	r, pol := directRunner(t, nil)
	res, err := r.Run(context.Background(), signedReq(Cmd{Kind: KindClusterScaleOut, Cluster: testCluster, Content: goodTiKV}), nil)
	if err != nil || res.Exit != 0 || !strings.Contains(res.Output, "FILE:") {
		t.Fatalf("%+v %v", res, err)
	}
	// 带 Content 的 check 是读类：无需令牌。
	res, err = r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterCheck, Cluster: testCluster, Content: goodTiKV}}, nil)
	if err != nil || res.Exit != 0 || !strings.Contains(res.Output, "|--cluster\n") {
		t.Fatalf("%+v %v", res, err)
	}
	if n := countFiles(t, pol.TempDir); n != 0 {
		t.Errorf("剩 %d 个临时文件", n)
	}
}

func TestDirectRunnerAlertSilence(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	var gotPath, gotMethod, gotCT string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotMethod, gotCT = req.URL.Path, req.Method, req.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(req.Body)
		if status == http.StatusFound {
			w.Header().Set("Location", "http://127.0.0.1:1/evil")
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"silenceID":"abc-123"}`))
		} else {
			_, _ = w.Write([]byte("bad things"))
		}
	}))
	defer srv.Close()
	r, _ := directRunner(t, nil)
	r.Pol.AlertURL = srv.URL
	r.Pol.TiupBin = "/nonexistent/tiup-must-not-run" // HTTP 不经进程
	cmd := Cmd{Kind: KindAlertSilence, Cluster: testCluster, Key: "tikv_down", Instance: "10.0.2.11:20160", Rate: 30, Value: `维护窗口 "quoted" \n`}

	if _, err := r.Run(context.Background(), Request{Cmd: cmd}, nil); err == nil {
		t.Fatal("alert.silence 是写命令，必须有令牌")
	}
	emit, got := collect()
	res, err := r.Run(context.Background(), signedReq(cmd), emit)
	if err != nil || res.Exit != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(res.Output, "silenceID=abc-123") || len(got()) != 1 {
		t.Errorf("输出: %q %q", res.Output, got())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/v2/silences" || gotMethod != "POST" || gotCT != "application/json" {
		t.Errorf("请求: %s %s %s", gotMethod, gotPath, gotCT)
	}
	var body struct {
		Matchers []struct {
			Name    string `json:"name"`
			Value   string `json:"value"`
			IsRegex bool   `json:"isRegex"`
			IsEqual bool   `json:"isEqual"`
		} `json:"matchers"`
		StartsAt, EndsAt, CreatedBy, Comment string
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v: %s", err, gotBody)
	}
	if len(body.Matchers) != 2 || body.Matchers[0].Name != "alertname" || body.Matchers[0].Value != "tikv_down" || body.Matchers[0].IsRegex || !body.Matchers[0].IsEqual ||
		body.Matchers[1].Name != "instance" || body.Matchers[1].Value != "10.0.2.11:20160" || body.Matchers[1].IsRegex {
		t.Errorf("matchers: %+v", body.Matchers)
	}
	s, _ := time.Parse(time.RFC3339, body.StartsAt)
	e, _ := time.Parse(time.RFC3339, body.EndsAt)
	if e.Sub(s) != 30*time.Minute {
		t.Errorf("静默时长 %v", e.Sub(s))
	}
	if body.CreatedBy != "tidbops" || !strings.Contains(body.Comment, `维护窗口 "quoted" \n`) || !strings.Contains(body.Comment, "alice") {
		t.Errorf("createdBy/comment: %+v", body)
	}
	// 不带实例时只有 alertname 一个 matcher。
	mu.Unlock()
	cmd2 := Cmd{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 5}
	if _, err := r.Run(context.Background(), signedReq(cmd2), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	var b2 struct{ Matchers []any }
	_ = json.Unmarshal(gotBody, &b2)
	if len(b2.Matchers) != 1 {
		t.Errorf("matchers: %v", b2.Matchers)
	}
	// 非 2xx → Exit 1；重定向不跟随。
	status = http.StatusBadRequest
	mu.Unlock()
	res, err = r.Run(context.Background(), signedReq(cmd2), nil)
	if err != nil || res.Exit != 1 || !strings.Contains(res.Output, "400") {
		t.Errorf("400: %+v %v", res, err)
	}
	mu.Lock()
	status = http.StatusFound
	mu.Unlock()
	res, err = r.Run(context.Background(), signedReq(cmd2), nil)
	if err != nil || res.Exit != 1 || !strings.Contains(res.Err, "302") {
		t.Errorf("302: %+v %v", res, err)
	}
	mu.Lock()
}

func TestDirectRunnerAlertSilenceUnreachable(t *testing.T) {
	r, _ := directRunner(t, nil)
	r.Pol.AlertURL = "http://127.0.0.1:1"
	res, err := r.Run(context.Background(), signedReq(Cmd{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 5}), nil)
	if err == nil || res.Exit != -1 {
		t.Errorf("连接失败应报错: %+v %v", res, err)
	}
}

// ---------------------------------------------------------------- SimRunner ----

func newSim(t *testing.T) *SimRunner {
	return &SimRunner{Pol: testPolicy(""), Topo: StaticTopo(testTopo()), Key: testKey}
}

// 每个 Kind 的一个合法样例。
func sampleCmd(kind string) Cmd {
	c := testCluster
	n := []string{"10.0.2.11:20160"}
	switch kind {
	case KindClusterList, KindTiupVersion, KindTiupClusterVer:
		return Cmd{Kind: kind}
	case KindClusterDisplay, KindClusterCheck, KindCDCCaptures, KindClusterPrune, KindBRLogStatus:
		return Cmd{Kind: kind, Cluster: c}
	case KindPDQuery:
		return Cmd{Kind: kind, Cluster: c, Key: "store"}
	case KindClusterRestart, KindClusterStop, KindClusterStart:
		return Cmd{Kind: kind, Cluster: c, Nodes: n}
	case KindClusterScaleOut:
		return Cmd{Kind: kind, Cluster: c, Content: goodTiKV}
	case KindClusterScaleIn:
		return Cmd{Kind: kind, Cluster: c, Nodes: []string{"10.0.2.16:20160"}}
	case KindClusterEditCfg:
		return Cmd{Kind: kind, Cluster: c, Content: goodEdit}
	case KindClusterReload:
		return Cmd{Kind: kind, Cluster: c, Role: "tikv"}
	case KindPDConfigSet:
		return Cmd{Kind: kind, Cluster: c, Key: "replication.max-replicas", Value: "3"}
	case KindPDEvictAdd, KindPDEvictRemove:
		return Cmd{Kind: kind, Cluster: c, Store: 1}
	case KindPDStoreLimit:
		return Cmd{Kind: kind, Cluster: c, Rate: 15}
	case KindPDLeaderTransfer:
		return Cmd{Kind: kind, Cluster: c, Name: "pd-10.0.1.22-2379"}
	case KindSQLSetConfig:
		return Cmd{Kind: kind, Cluster: c, Role: "tikv", Key: "log.level", Value: "info"}
	case KindSQLSetGlobal:
		return Cmd{Kind: kind, Cluster: c, Key: "tidb_slow_log_threshold", Value: "500"}
	case KindBRBackupFull, KindBRRestoreFull:
		return Cmd{Kind: kind, Cluster: c, Value: "local:///data/backup/x"}
	case KindBRLogStart:
		return Cmd{Kind: kind, Cluster: c, Name: "pitr", Value: "local:///data/backup/log"}
	case KindBRLogStop:
		return Cmd{Kind: kind, Cluster: c, Name: "pitr"}
	case KindClusterUpgrade:
		return Cmd{Kind: kind, Cluster: c, Value: "v8.5.3"}
	case KindAlertSilence:
		return Cmd{Kind: kind, Cluster: c, Key: "x", Rate: 5}
	}
	panic("没有样例: " + kind)
}

func TestSimRunnerAllKinds(t *testing.T) {
	sim := newSim(t)
	for _, k := range Kinds() {
		cmd := sampleCmd(k)
		emit, got := collect()
		res, err := sim.Run(context.Background(), signedReq(cmd), emit)
		if err != nil || res.Exit != 0 {
			t.Errorf("%s: %+v %v", k, res, err)
			continue
		}
		if k != KindSQLSetConfig && k != KindSQLSetGlobal && (res.Output == "" || len(got()) == 0) {
			t.Errorf("%s 应生成输出", k)
		}
		if res.Output != strings.Join(got(), "\n")+func() string {
			if len(got()) > 0 {
				return "\n"
			}
			return ""
		}() {
			t.Errorf("%s: Result.Output 与 emit 不一致", k)
		}
	}
	if len(sim.Calls()) != len(Kinds()) {
		t.Errorf("调用记录 %d != %d", len(sim.Calls()), len(Kinds()))
	}
}

func TestSimRunnerOutputIsRealistic(t *testing.T) {
	sim := newSim(t)
	res, _ := sim.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, nil)
	tp, err := ParseTopo(res.Output)
	if err != nil {
		t.Fatalf("模拟的 display 输出应能被 ParseTopo 解析: %v\n%s", err, res.Output)
	}
	want := testTopo()
	if len(tp.Nodes) != len(want.Nodes) || tp.Version != "v8.5.2" || tp.PD != want.PD {
		t.Errorf("round trip 不一致: %+v", tp)
	}
	for id, role := range want.Nodes {
		if tp.Nodes[id] != role {
			t.Errorf("节点 %s: %q vs %q", id, tp.Nodes[id], role)
		}
	}
	res, _ = sim.Run(context.Background(), signedReq(Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}), nil)
	for _, w := range []string{"tiup-cluster restart " + testCluster + " -N 10.0.2.11:20160 -y", "Restarted cluster `" + testCluster + "` successfully"} {
		if !strings.Contains(res.Output, w) {
			t.Errorf("缺少 %q:\n%s", w, res.Output)
		}
	}
	res, _ = sim.Run(context.Background(), signedReq(Cmd{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 5}), nil)
	if !strings.Contains(res.Output, "silenceID=") {
		t.Errorf("silence: %q", res.Output)
	}
}

func TestSimRunnerValidatesLikeDirect(t *testing.T) {
	sim := newSim(t)
	cmd := Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}
	ctx := context.Background()
	for name, req := range map[string]Request{
		"无令牌":      {Cmd: cmd, ChangeID: "CHG-1"},
		"篡改":       {Cmd: Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.12:20160"}}, ChangeID: "CHG-1", Token: Sign(testKey, cmd, "CHG-1", time.Now().Add(time.Hour))},
		"拓扑外节点":    signedReq(Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.9.9:20160"}}),
		"注入":       signedReq(Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160;id"}}),
		"未登记 Kind": signedReq(Cmd{Kind: "cluster.destroy", Cluster: testCluster}),
	} {
		if res, err := sim.Run(ctx, req, nil); err == nil || res.Exit != -1 {
			t.Errorf("%s 应被拒绝: %+v %v", name, res, err)
		}
	}
	if len(sim.Calls()) != 0 {
		t.Errorf("被拒绝的请求不应被记录: %+v", sim.Calls())
	}
	// 过期。
	sim.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := sim.Run(ctx, signedReq(cmd), nil); err == nil {
		t.Error("过期令牌应拒绝")
	}
	sim.Now = nil
	if _, err := sim.Run(ctx, signedReq(cmd), nil); err != nil {
		t.Error(err)
	}
}

func TestSimRunnerCallsAreCopies(t *testing.T) {
	sim := newSim(t)
	cmd := Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}
	_, _ = sim.Run(context.Background(), signedReq(cmd), nil)
	calls := sim.Calls()
	if len(calls) != 1 || calls[0].Cmd.Kind != KindClusterRestart || calls[0].Actor != "alice" || calls[0].ChangeID != "CHG-1" {
		t.Fatalf("%+v", calls)
	}
	calls[0].Cmd.Nodes[0] = "tampered"
	if sim.Calls()[0].Cmd.Nodes[0] != "10.0.2.11:20160" {
		t.Error("Calls 应返回副本")
	}
}

func TestSimRunnerFailureInjection(t *testing.T) {
	sim := newSim(t)
	sim.FailKind, sim.FailAfter = KindClusterRestart, 2
	emit, got := collect()
	res, err := sim.Run(context.Background(), signedReq(sampleCmd(KindClusterRestart)), emit)
	if err != nil || res.Exit != 1 || res.Err == "" {
		t.Fatalf("注入失败: %+v %v", res, err)
	}
	if n := len(got()); n != 3 || !strings.HasPrefix(got()[2], "Error:") {
		t.Errorf("应先输出 FailAfter=2 行，再输出错误行: %q", got())
	}
	// 其它 Kind 不受影响。
	if res, err := sim.Run(context.Background(), signedReq(sampleCmd(KindClusterStop)), nil); err != nil || res.Exit != 0 {
		t.Errorf("%+v %v", res, err)
	}
	// FailAfter=0：立刻失败。
	sim.FailAfter = 0
	emit, got = collect()
	res, _ = sim.Run(context.Background(), signedReq(sampleCmd(KindClusterRestart)), emit)
	if res.Exit != 1 || len(got()) != 1 {
		t.Errorf("%+v %q", res, got())
	}
	// 失败的调用同样被记录。
	if len(sim.Calls()) != 3 {
		t.Errorf("calls = %d", len(sim.Calls()))
	}
}

func TestSimRunnerDelayAndCancel(t *testing.T) {
	sim := newSim(t)
	sim.Delay = 300 * time.Millisecond
	start := time.Now()
	if res, err := sim.Run(context.Background(), Request{Cmd: sampleCmd(KindClusterDisplay)}, nil); err != nil || res.Exit != 0 {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("Delay 未生效: %v", d)
	}
	sim.Delay = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	res, err := sim.Run(ctx, Request{Cmd: sampleCmd(KindClusterDisplay)}, nil)
	if err == nil || res.Exit != -1 || time.Since(start) > 5*time.Second {
		t.Errorf("取消应及时返回: %+v %v %v", res, err, time.Since(start))
	}
}

// ---------------------------------------------------------------- Serve / SudoRunner ----

func parseLines(t *testing.T, out string) (outs []string, result Result, nresult int) {
	t.Helper()
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		var m struct {
			T    string `json:"t"`
			Line string `json:"line"`
			Result
		}
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("协议行不是 JSON: %q", ln)
		}
		switch m.T {
		case "out":
			outs = append(outs, m.Line)
		case "result":
			result = m.Result
			nresult++
		default:
			t.Fatalf("未知类型 %q", m.T)
		}
	}
	return
}

func serveOnce(t *testing.T, runner Runner, in string) string {
	t.Helper()
	var out strings.Builder
	if err := Serve(context.Background(), strings.NewReader(in), &out, runner); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return out.String()
}

func TestServeRoundTrip(t *testing.T) {
	sim := newSim(t)
	req := signedReq(Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}})
	in, _ := json.Marshal(req)
	out := serveOnce(t, sim, string(in))
	lines, res, n := parseLines(t, out)
	if n != 1 || res.Exit != 0 || res.Err != "" || len(lines) < 3 {
		t.Fatalf("res=%+v n=%d lines=%q", res, n, lines)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "}") || !strings.Contains(strings.Split(strings.TrimSpace(out), "\n")[len(strings.Split(strings.TrimSpace(out), "\n"))-1], `"t":"result"`) {
		t.Error("result 必须是最后一行")
	}
	if strings.Join(lines, "\n")+"\n" != res.Output {
		t.Error("out 行应与 result.output 一致")
	}
	if strings.Contains(out, req.Token) || strings.Contains(out, "CHG-1") {
		t.Error("输出不得回显令牌或变更单号")
	}
	if len(sim.Calls()) != 1 {
		t.Error("应调用了一次 runner")
	}
}

func TestServeErrorsAsResult(t *testing.T) {
	sim := newSim(t)
	cmd := Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}
	good := signedReq(cmd)
	good.Token += "x" // 令牌损坏
	in, _ := json.Marshal(good)
	cases := map[string]string{
		"令牌无效":     string(in),
		"不是 JSON":  "not json",
		"空输入":      "",
		"未知字段":     `{"cmd":{"kind":"cluster.display","cluster":"prod-tidb-01"},"extra":1}`,
		"cmd 未知字段": `{"cmd":{"kind":"cluster.display","cluster":"prod-tidb-01","shell":"id"}}`,
		"多余内容":     `{"cmd":{"kind":"cluster.display","cluster":"prod-tidb-01"}}{"cmd":{"kind":"cluster.display","cluster":"prod-tidb-01"}}`,
		"未登记命令":    `{"cmd":{"kind":"cluster.destroy","cluster":"prod-tidb-01"}}`,
		"类型错误":     `{"cmd":{"kind":"pd.evict-add","cluster":"prod-tidb-01","store":"x"}}`,
	}
	for name, input := range cases {
		out := serveOnce(t, sim, input)
		lines, res, n := parseLines(t, out)
		if n != 1 || len(lines) != 0 || res.Exit != -1 || res.Err == "" {
			t.Errorf("%s: %q → %+v", name, out, res)
		}
		if strings.Contains(out, good.Token) {
			t.Errorf("%s: 输出回显了令牌", name)
		}
	}
	if len(sim.Calls()) != 0 {
		t.Errorf("出错的请求不得被执行: %+v", sim.Calls())
	}
}

type panicRunner struct{}

func (panicRunner) Run(context.Context, Request, func(string)) (Result, error) { panic("boom") }

type errRunner struct{}

func (errRunner) Run(_ context.Context, _ Request, emit func(string)) (Result, error) {
	emit("partial")
	return Result{Exit: 2, Output: "partial\n"}, errors.New("磁盘已满")
}

func TestServeRunnerFailures(t *testing.T) {
	in := `{"cmd":{"kind":"cluster.display","cluster":"prod-tidb-01"}}`
	_, res, n := parseLines(t, serveOnce(t, panicRunner{}, in))
	if n != 1 || res.Exit != -1 || res.Err == "" || strings.Contains(res.Err, "boom") {
		t.Errorf("panic: %+v", res)
	}
	lines, res, _ := parseLines(t, serveOnce(t, errRunner{}, in))
	if res.Exit != -1 || res.Err != "磁盘已满" || len(lines) != 1 {
		t.Errorf("err: %+v %q", res, lines)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestServeWriteFailure(t *testing.T) {
	sim := newSim(t)
	in := `{"cmd":{"kind":"cluster.display","cluster":"prod-tidb-01"}}`
	if err := Serve(context.Background(), strings.NewReader(in), failWriter{}, sim); err == nil {
		t.Error("写出失败应返回错误")
	}
}

func sudoRunner(bin string) SudoRunner {
	exe, _ := os.Executable()
	return SudoRunner{Sudo: exe, User: "tidb", Bin: bin}
}

func TestSudoRunnerEndToEnd(t *testing.T) {
	r := sudoRunner("good")
	emit, got := collect()
	cmd := Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}
	res, err := r.Run(context.Background(), signedReq(cmd), emit)
	if err != nil || res.Exit != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(got()) < 3 || !strings.Contains(res.Output, "Restarted cluster") {
		t.Errorf("out 行应被转发: %q / %q", got(), res.Output)
	}
	// 子进程里的 SimRunner 独立验证令牌：无令牌的写命令 → exit=-1 + 错误。
	res, err = r.Run(context.Background(), Request{Cmd: cmd, ChangeID: "CHG-1"}, nil)
	if err == nil || res.Exit != -1 || !strings.Contains(res.Err, "令牌") {
		t.Errorf("无令牌: %+v %v", res, err)
	}
	// 读命令。
	res, err = r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, nil)
	if err != nil || res.Exit != 0 || !strings.Contains(res.Output, "Cluster version:    v8.5.2") {
		t.Errorf("读命令: %+v %v", res, err)
	}
}

func TestSudoRunnerArgv(t *testing.T) {
	r := sudoRunner("echoargs")
	emit, got := collect()
	if _, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, emit); err != nil {
		t.Fatal(err)
	}
	if len(got()) != 1 || got()[0] != "-n|-u|tidb|echoargs|exec|--stdin" {
		t.Errorf("sudo argv: %q", got())
	}
	r.Args = []string{"exec", "--stdin", "--extra"}
	emit, got = collect()
	_, _ = r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, emit)
	if got()[0] != "-n|-u|tidb|echoargs|exec|--stdin|--extra" {
		t.Errorf("自定义 Args: %q", got())
	}
}

func TestSudoRunnerProtocolErrors(t *testing.T) {
	req := Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}
	for mode, wantSub := range map[string]string{
		"junk":     "协议异常",
		"exit3":    "异常退出",
		"denied":   "a password is required",
		"noresult": "未收到 result",
		"extra":    "协议异常",
		"badtype":  "协议异常",
	} {
		emit, _ := collect()
		res, err := sudoRunner(mode).Run(context.Background(), req, emit)
		if err == nil || res.Exit != -1 || !strings.Contains(err.Error(), wantSub) {
			t.Errorf("%s: err=%v res=%+v，期望包含 %q", mode, err, res, wantSub)
		}
	}
	// 子进程回报 exit=-1：转为 error。
	res, err := sudoRunner("failresult").Run(context.Background(), req, nil)
	if err == nil || res.Exit != -1 || res.Err != "令牌已过期" {
		t.Errorf("failresult: %+v %v", res, err)
	}
	// 命令已执行但非 0：result 原样返回，不是 error。
	res, err = sudoRunner("nonzero").Run(context.Background(), req, nil)
	if err != nil || res.Exit != 2 || res.Output != "x\n" {
		t.Errorf("nonzero: %+v %v", res, err)
	}
}

func TestSudoRunnerConfigValidation(t *testing.T) {
	req := Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}
	exe, _ := os.Executable()
	for name, r := range map[string]SudoRunner{
		"无用户":        {Sudo: exe, Bin: "good"},
		"用户含空格":      {Sudo: exe, User: "ti db", Bin: "good"},
		"用户以 - 开头":   {Sudo: exe, User: "-u", Bin: "good"},
		"无 Bin":      {Sudo: exe, User: "tidb"},
		"Bin 含空格":    {Sudo: exe, User: "tidb", Bin: "a b"},
		"Bin 以 - 开头": {Sudo: exe, User: "tidb", Bin: "-x"},
		"sudo 不存在":   {Sudo: "/nonexistent/sudo", User: "tidb", Bin: "good"},
	} {
		if res, err := r.Run(context.Background(), req, nil); err == nil || res.Exit != -1 {
			t.Errorf("%s 应报错: %+v %v", name, res, err)
		}
	}
}

func TestSudoRunnerTimeoutAndCancel(t *testing.T) {
	r := sudoRunner("hang")
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, nil)
	if err == nil || res.Exit != -1 || time.Since(start) > 8*time.Second {
		t.Errorf("超时: %+v %v %v", res, err, time.Since(start))
	}
	r.Timeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err = r.Run(ctx, Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}, nil)
	if err == nil || res.Exit != -1 {
		t.Errorf("取消: %+v %v", res, err)
	}
}

// 所有 Runner 满足接口。
var (
	_ Runner = DirectRunner{}
	_ Runner = &DirectRunner{}
	_ Runner = &SimRunner{}
	_ Runner = SudoRunner{}
	_ Runner = &SudoRunner{}
)
