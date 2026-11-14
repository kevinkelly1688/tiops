// @author: kevinkelly
// @license: Apache License 2.0
// @file: script_test.go
// @time: 2026/10/28

package shell

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var maskSamples = []string{
	"login ok user=zhang phone=13800138000 password=Abcd1234 id=110101199003078888 card=6222021234567890123",
	"url=mysql://root:S3cr3t@10.0.0.1:4000/db token: abc.def PASSWORD=Up pid 12345 ts 1700000000 port 4000",
	"-----BEGIN RSA PRIVATE KEY-----", "MIIabc", "-----END RSA PRIVATE KEY-----", "after key",
	"access_key = AKIAXXXXXXXX secret:   s3 Api_Key=k1 pwd=p1 passwd: p2",
	"普通中文行 手机 13912345678 身份证 44030119900101001X",
}

var maskLeaks = []string{"Abcd1234", "13800138000", "S3cr3t", "MIIabc", "AKIAXXXXXXXX", "13912345678", "44030119900101001X"}

// 遮蔽规则有三份实现（Go 的 MaskText、平台进程内执行用的流式 LineMasker、临时单元脚本里的 sed）：对同一批样例必须给出一致的结果。
func TestLineMaskerMatchesMaskText(t *testing.T) {
	in := strings.Join(maskSamples, "\n")
	want, _ := MaskText(in)
	lm := &LineMasker{}
	var got []string
	for _, l := range maskSamples {
		if o, ok := lm.Line(l); ok {
			got = append(got, o)
		}
	}
	if o, ok := lm.Flush(); ok {
		got = append(got, o)
	}
	if strings.Join(got, "\n") != want {
		t.Errorf("流式遮蔽与整段遮蔽不一致：\n--- 流式:\n%s\n--- 整段:\n%s", strings.Join(got, "\n"), want)
	}
	for _, leak := range maskLeaks {
		if strings.Contains(strings.Join(got, "\n"), leak) {
			t.Errorf("流式版本漏掉了 %q", leak)
		}
	}
	// 没等到 END 的私钥块：结束时放出（常规遮蔽照常）
	lm2 := &LineMasker{}
	if _, ok := lm2.Line("-----BEGIN RSA PRIVATE KEY-----"); ok {
		t.Error("BEGIN 行应先攒着")
	}
	if _, ok := lm2.Line("password=zzz"); ok {
		t.Error("块内的行应先攒着")
	}
	if o, ok := lm2.Flush(); !ok || strings.Contains(o, "zzz") || !strings.Contains(o, "BEGIN") {
		t.Errorf("Flush: %q %v", o, ok)
	}
	// 单行里 BEGIN 与 END 同时出现：直接按整段规则处理
	if o, ok := (&LineMasker{}).Line("k=-----BEGIN RSA PRIVATE KEY-----x-----END RSA PRIVATE KEY----- tail"); !ok || o != "k="+maskKey+" tail" {
		t.Errorf("单行私钥: %q %v", o, ok)
	}
}

// sed 版与 Go 版一致。没有 sed 的机器跳过（Windows 开发机通常经 Git 自带的 sed 也能跑）。
func TestSedScriptMatchesGo(t *testing.T) {
	sed, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("没有 sed")
	}
	in := strings.Join(append(append([]string{}, maskSamples...), ""), "\n")
	script := filepath.Join(t.TempDir(), "mask.sed")
	if err := os.WriteFile(script, []byte(SedScript()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sed, "-E", "-f", script)
	cmd.Stdin = strings.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Skipf("这台机器的 sed 跑不了该脚本（%v）：%s", err, errb.String())
	}
	want, _ := MaskText(in)
	got := strings.ReplaceAll(out.String(), "\r\n", "\n")
	if got != want {
		t.Errorf("sed 与 Go 的遮蔽结果不一致：\n--- sed:\n%s\n--- go:\n%s", got, want)
	}
	for _, leak := range maskLeaks {
		if strings.Contains(got, leak) {
			t.Errorf("sed 版本漏掉了 %q", leak)
		}
	}
}

// 临时单元脚本的集成测试（Linux + bash）：以当前用户模拟「sudo 行直接执行」的分支（不经 runuser），
// 验证：命令逐条执行并回显、sudo 前缀被去掉、遮蔽在写盘前生效、遇错停止、超时、退出码与命令文件删除。
func TestRunScriptOnBash(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("任务脚本只在 Linux 上验证（需要 bash、timeout、getent）")
	}
	for _, bin := range []string{"bash", "timeout", "getent", "sed", "stat"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("缺少 %s", bin)
		}
	}
	dir := t.TempDir()
	m := &Manager{}
	j := Job{ID: "s-20260101-000000-abcdef", Identity: "tidb", Cwd: dir, Timeout: 30, Stop: true, Route: RouteUnit}
	for name, body := range map[string]string{
		"run.sh":   m.runScript(j, dir),
		"mask.sed": SedScript(),
		"cmds.txt": "sudo echo hello phone=13800138000 password=Abcd1234\nsudo sleep 0.1; echo second\nsudo false\nsudo echo NOT-REACHED\n",
		"out.log":  "", "exit": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", filepath.Join(dir, "run.sh"))
	_ = cmd.Run() // 退出码由 exit 文件体现
	out, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	exitB, _ := os.ReadFile(filepath.Join(dir, "exit"))
	text := string(out)
	for _, want := range []string{"$ sudo echo hello phone=1********** password=******", "hello phone=1********** password=******", "$ sudo sleep 0.1; echo second", "second", "退出码 1，按「遇错停止」", "任务结束，退出码 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("out.log 缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "NOT-REACHED") || strings.Contains(text, "13800138000") || strings.Contains(text, "Abcd1234") {
		t.Errorf("遇错停止 / 遮蔽未生效:\n%s", text)
	}
	if strings.TrimSpace(string(exitB)) != "1" {
		t.Errorf("exit 文件应为 1：%q", exitB)
	}
	if _, err := os.Stat(filepath.Join(dir, "cmds.txt")); err == nil {
		t.Error("任务结束后应删除 cmds.txt")
	}
	// 超时：1 秒的超时跑 sleep 5 → 124 并提示
	j2 := Job{ID: "s-20260101-000001-abcdef", Identity: "tidb", Cwd: dir, Timeout: 1, Stop: false, Route: RouteUnit}
	d2 := t.TempDir()
	for name, body := range map[string]string{"run.sh": m.runScript(j2, d2), "mask.sed": SedScript(), "cmds.txt": "sudo sleep 5\nsudo echo after\n", "out.log": "", "exit": ""} {
		_ = os.WriteFile(filepath.Join(d2, name), []byte(body), 0o700)
	}
	_ = exec.Command("bash", filepath.Join(d2, "run.sh")).Run()
	out2, _ := os.ReadFile(filepath.Join(d2, "out.log"))
	if !strings.Contains(string(out2), "秒仍未结束，已终止") || !strings.Contains(string(out2), "after") {
		t.Errorf("超时提示 / 不停止继续执行:\n%s", out2)
	}
}
