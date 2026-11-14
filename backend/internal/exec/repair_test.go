// @author: kevinkelly
// @license: Apache License 2.0
// @file: repair_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetRepair() {
	repairLast.mu.Lock()
	repairLast.at = map[string]time.Time{}
	repairLast.mu.Unlock()
}

// fakeHome 建一个带标记文件的 TIUP_HOME：测试二进制（假 tiup）据此模拟「ctl 文件缺失」并记录调用。
func fakeHome(t *testing.T, flags ...string) string {
	t.Helper()
	resetRepair()
	t.Cleanup(resetRepair)
	d := t.TempDir()
	for _, f := range append([]string{"fake-tiup"}, flags...) {
		if err := os.WriteFile(filepath.Join(d, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TIUP_HOME", d)
	return d
}

func calls(t *testing.T, home string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "calls.log"))
	return strings.Fields(string(b))
}

func TestRepairTarget(t *testing.T) {
	sig := "Starting component `ctl`: /home/tidb/.tiup/components/ctl/v8.5.8/ctl pd\nError: Failed to start component `ctl`: fork/exec /home/tidb/.tiup/components/ctl/v8.5.8/ctl: no such file or directory\n"
	cases := []struct {
		name   string
		argv   []string
		out    string
		comp   string
		ver    string
		wantOK bool
	}{
		{"ctl 文件缺失", []string{"tiup", "ctl:v8.5.8", "pd", "-u", "http://10.0.1.21:2379", "store"}, sig, "ctl", "v8.5.8", true},
		{"br 文件缺失", []string{"tiup", "br:v8.5.8", "log", "status"}, strings.ReplaceAll(sig, "`ctl`", "`br`"), "br", "v8.5.8", true},
		{"报错组件与 argv 不一致", []string{"tiup", "ctl:v8.5.8", "pd"}, strings.ReplaceAll(sig, "`ctl`", "`br`"), "", "", false},
		{"别的错误", []string{"tiup", "ctl:v8.5.8", "pd"}, "Error: connection refused\n", "", "", false},
		{"cluster 子命令不修", []string{"tiup", "cluster", "display", "c1"}, "Error: Failed to start component `cluster`: fork/exec x: no such file or directory\n", "", "", false},
		{"版本格式不对", []string{"tiup", "ctl:latest", "pd"}, sig, "", "", false},
		{"版本带注入字符", []string{"tiup", "ctl:v8.5.8;rm", "pd"}, sig, "", "", false},
		{"argv 太短", []string{"tiup"}, sig, "", "", false},
	}
	for _, c := range cases {
		comp, ver, ok := repairTarget(c.argv, c.out)
		if ok != c.wantOK || comp != c.comp || ver != c.ver {
			t.Errorf("%s: got (%q,%q,%v)", c.name, comp, ver, ok)
		}
	}
}

func TestRepairComponentThenRetry(t *testing.T) {
	home := fakeHome(t, "broken")
	var logs []string
	r, _ := directRunner(t, &logs)
	emit, got := collect()
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindPDQuery, Cluster: testCluster, Key: "store"}, Actor: "alice"}, emit)
	if err != nil || res.Exit != 0 {
		t.Fatalf("修复后应重试成功: res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "[自动修复]") || !strings.Contains(res.Output, "已重新安装，重试原命令") {
		t.Errorf("输出应说明自动修复: %q", res.Output)
	}
	if !strings.Contains(strings.Join(got(), "\n"), "[自动修复]") {
		t.Errorf("修复说明应同时逐行回调 emit: %q", got())
	}
	cs := calls(t, home)
	var kinds []string
	for _, c := range cs {
		kinds = append(kinds, strings.SplitN(c, "|", 2)[0])
	}
	// 原命令（失败）→ uninstall → install → 原命令（成功）
	if len(kinds) != 4 || !strings.HasPrefix(kinds[0], "ctl:") || kinds[1] != "uninstall" || kinds[2] != "install" || !strings.HasPrefix(kinds[3], "ctl:") {
		t.Fatalf("调用序列不对: %q", cs)
	}
	if !strings.HasPrefix(cs[1], "uninstall|ctl:") || !strings.HasPrefix(cs[2], "install|ctl:") {
		t.Errorf("卸载 / 安装的对象应是同一个 ctl 版本: %q", cs)
	}
	if j := strings.Join(logs, "\n"); !strings.Contains(j, "exec repair start") || !strings.Contains(j, "exec repair ok") {
		t.Errorf("审计日志应记录自动修复: %s", j)
	}
}

func TestRepairCooldownAndFailure(t *testing.T) {
	home := fakeHome(t, "broken", "install-fails")
	r, _ := directRunner(t, nil)
	req := Request{Cmd: Cmd{Kind: KindPDQuery, Cluster: testCluster, Key: "store"}}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil || res.Exit == 0 {
		t.Fatalf("安装失败时应返回原命令的失败: res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "重新安装失败") || !strings.Contains(res.Output, "no such file or directory") {
		t.Errorf("应说明自动修复失败并保留原报错: %q", res.Output)
	}
	n := len(calls(t, home))
	// 冷却期内再次失败：不再重复卸载 / 安装
	res, _ = r.Run(context.Background(), req, nil)
	if res.Exit == 0 || !strings.Contains(res.Output, "10 分钟内已尝试过") {
		t.Errorf("冷却期内应跳过自动修复: %+v", res)
	}
	if got := len(calls(t, home)); got != n+1 {
		t.Errorf("冷却期内只应多一次原命令调用（%d → %d）", n, got)
	}
}

func TestRepairNotTriggeredByOtherFailures(t *testing.T) {
	home := fakeHome(t)
	r, _ := directRunner(t, nil)
	res, err := r.Run(context.Background(), Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: "failc"}}, nil)
	if err != nil || res.Exit != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, c := range calls(t, home) {
		if strings.HasPrefix(c, "install|") || strings.HasPrefix(c, "uninstall|") {
			t.Errorf("普通失败不应触发自动修复: %q", c)
		}
	}
	if strings.Contains(res.Output, "[自动修复]") {
		t.Errorf("输出不应带自动修复说明: %q", res.Output)
	}
}
