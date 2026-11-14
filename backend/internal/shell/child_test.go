// @author: kevinkelly
// @license: Apache License 2.0
// @file: child_test.go
// @time: 2026/10/28

package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// callChild 像平台那样调用子进程入口：stdin 保持打开直到拿到结果（close 可提前关掉 stdin 模拟取消）。
func callChild(t *testing.T, req ChildReq, after func(w io.WriteCloser)) ([]string, ChildRes) {
	t.Helper()
	pr, pw := io.Pipe()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeChild(context.Background(), pr, &out)
	}()
	b, _ := json.Marshal(req)
	_, _ = pw.Write(append(b, '\n'))
	if after != nil {
		after(pw)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("子进程没有在 30 秒内返回")
	}
	_ = pw.Close()
	var lines []string
	var res ChildRes
	for _, ln := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var m struct {
			T    string `json:"t"`
			Line string `json:"line"`
			ChildRes
		}
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("协议行不合法: %q", ln)
		}
		if m.T == "out" {
			lines = append(lines, m.Line)
		} else {
			res = m.ChildRes
		}
	}
	return lines, res
}

func TestChildRejectsBadRequests(t *testing.T) {
	// 格式错误 / 未知模式也以 result 回传
	pr, pw := io.Pipe()
	var out bytes.Buffer
	go func() { _, _ = pw.Write([]byte("{not json")); _ = pw.Close() }()
	_ = ServeChild(context.Background(), pr, &out)
	if !strings.Contains(out.String(), `"t":"result"`) || !strings.Contains(out.String(), "请求格式错误") {
		t.Errorf("格式错误: %s", out.String())
	}
	if _, res := callChild(t, ChildReq{Mode: "weird"}, nil); res.Exit != -1 || !strings.Contains(res.Err, "未知模式") {
		t.Errorf("未知模式: %+v", res)
	}
	good := ChildReq{Mode: ModeRun, JobID: "s-20260101-000000-abcdef", Cwd: "/tmp", Timeout: 30, Lines: []string{"uptime"}}
	for name, mod := range map[string]func(*ChildReq){
		"目录":      func(r *ChildReq) { r.Cwd = "relative" },
		"目录 ..":   func(r *ChildReq) { r.Cwd = "/a/../b" },
		"超时":      func(r *ChildReq) { r.Timeout = 7 },
		"任务号":     func(r *ChildReq) { r.JobID = "x" },
		"变更命令":    func(r *ChildReq) { r.Lines = []string{"systemctl restart x"} },
		"禁止命令":    func(r *ChildReq) { r.Lines = []string{"cat /etc/shadow"} },
		"sudo 变更": func(r *ChildReq) { r.Lines = []string{"sudo yum install -y x"} },
	} {
		r := good
		r.Lines = append([]string{}, good.Lines...)
		mod(&r)
		if _, res := callChild(t, r, nil); res.Exit != -1 || res.Err == "" {
			t.Errorf("%s：子进程应拒绝: %+v", name, res)
		}
	}
	// launch / stop 只接受平台命名规则的单元名与任务目录里的 run.sh
	for name, r := range map[string]ChildReq{
		"单元名":   {Mode: ModeLaunch, JobID: "s-20260101-000000-abcdef", Unit: "sshd", Script: "/var/lib/tidbops/shell/jobs/s-20260101-000000-abcdef/run.sh"},
		"单元名不符": {Mode: ModeLaunch, JobID: "s-20260101-000000-abcdef", Unit: "tidbops-shell-s-20260101-000000-abcde0", Script: "/var/lib/tidbops/shell/jobs/s-20260101-000000-abcdef/run.sh"},
		"脚本路径":  {Mode: ModeLaunch, JobID: "s-20260101-000000-abcdef", Unit: "tidbops-shell-s-20260101-000000-abcdef", Script: "/etc/x/run.sh"},
		"脚本 ..": {Mode: ModeLaunch, JobID: "s-20260101-000000-abcdef", Unit: "tidbops-shell-s-20260101-000000-abcdef", Script: "/var/lib/../s-20260101-000000-abcdef/run.sh"},
		"停止单元名": {Mode: ModeStop, Unit: "sshd"},
	} {
		if _, res := callChild(t, r, nil); res.Exit != -1 || !strings.Contains(res.Err, "不合法") {
			t.Errorf("%s：应拒绝: %+v", name, res)
		}
	}
}

func TestChildRunOnBash(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("run 模式只在 Linux 上验证（进程组、bash）")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("缺少 bash")
	}
	dir := t.TempDir()
	lines, res := callChild(t, ChildReq{Mode: ModeRun, JobID: "s-20260101-000000-abcdef", Cwd: dir, Timeout: 30, Stop: true,
		Lines: []string{"echo hello phone=13800138000 password=Abcd1234", "sleep 0.1; echo second", "pwd", "false", "echo NOT-REACHED"}}, nil)
	text := strings.Join(lines, "\n")
	for _, want := range []string{"$ echo hello phone=1********** password=******", "hello phone=1********** password=******", "$ sleep 0.1; echo second", "second", dir, "退出码 1，按「遇错停止」", "任务结束，退出码 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("输出缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "NOT-REACHED") || strings.Contains(text, "13800138000") || strings.Contains(text, "Abcd1234") {
		t.Errorf("遇错停止 / 遮蔽未生效:\n%s", text)
	}
	if res.Exit != 1 || res.Err != "" {
		t.Errorf("结果: %+v", res)
	}
	// 取消：关掉 stdin → 终止当前命令，退出码 143
	start := time.Now()
	lines, res = callChild(t, ChildReq{Mode: ModeRun, JobID: "s-20260101-000000-abcdef", Cwd: dir, Timeout: 30, Lines: []string{"sleep 20", "echo NOT-REACHED"}},
		func(w io.WriteCloser) { time.Sleep(500 * time.Millisecond); _ = w.Close() })
	if res.Exit != 143 || time.Since(start) > 10*time.Second || strings.Contains(strings.Join(lines, "\n"), "NOT-REACHED") || !strings.Contains(strings.Join(lines, "\n"), "已取消") {
		t.Errorf("取消: %+v %v %v", res, time.Since(start), lines)
	}
}
