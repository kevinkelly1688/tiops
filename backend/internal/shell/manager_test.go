// @author: kevinkelly
// @license: Apache License 2.0
// @file: manager_test.go
// @time: 2026/10/28

package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent 模拟子进程通道：记录每次调用；run 模式按 lines 回传输出，block 非空时阻塞到被取消（模拟长命令）。
type fakeAgent struct {
	mu    sync.Mutex
	calls []ChildReq
	err   error
	res   ChildRes
	lines []string
	block chan struct{}
	sudo  bool
}

func (f *fakeAgent) Call(ctx context.Context, req ChildReq, emit func(string)) (ChildRes, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	err, res, lines, block, sudo := f.err, f.res, f.lines, f.block, f.sudo
	f.mu.Unlock()
	if err != nil {
		return ChildRes{}, err
	}
	switch req.Mode {
	case ModeProbe:
		return ChildRes{Sudo: sudo, Home: "/home/tidb", User: "tidb"}, nil
	case ModeRun:
		for _, l := range lines {
			if emit != nil {
				emit(l)
			}
		}
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				if emit != nil {
					emit("[平台] 已取消")
				}
				return ChildRes{Exit: 143}, nil
			}
		}
		return res, nil
	}
	if res.Out == "" && res.Err == "" {
		res.Out = "Running as unit: " + req.Unit
	}
	return res, nil
}

func (f *fakeAgent) last() ChildReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ChildReq{}
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeAgent) count(mode string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Mode == mode {
			n++
		}
	}
	return n
}

type env struct {
	m   *Manager
	ag  *fakeAgent
	now time.Time
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	osr := filepath.Join(t.TempDir(), "os-release")
	_ = os.WriteFile(osr, []byte("NAME=\"Rocky Linux\"\nPRETTY_NAME=\"Rocky Linux 9.3 (Blue Onyx)\"\n"), 0o644)
	e := &env{ag: &fakeAgent{sudo: true}, now: time.Date(2026, 10, 9, 21, 0, 0, 0, time.UTC), dir: filepath.Join(t.TempDir(), "shell")}
	m, err := NewManager(Config{Dir: e.dir, Agent: e.ag, NoPathCheck: true, OSRelease: osr, Now: func() time.Time { return e.now },
		LookupUser: func(n string) (string, bool) {
			if n == "tidb" {
				return "/home/tidb", true
			}
			return "", false
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return e
}

func (e *env) finish(id string, rc string) {
	p := filepath.Join(e.m.jobDir(id), "exit")
	_ = os.WriteFile(p, []byte(rc), 0o600)
	_ = os.Chtimes(p, e.now, e.now) // 结束时间取假时钟，与 Sweep 的判断一致
}

func (e *env) wait(t *testing.T, id string) {
	t.Helper()
	if !e.m.Wait(id, 5*time.Second) {
		t.Fatalf("任务 %s 没有在 5 秒内结束", id)
	}
}

func ro(cmds ...string) RunReq { return RunReq{Commands: cmds} }

func TestInfoAndNormalize(t *testing.T) {
	e := newEnv(t)
	in := e.m.Info()
	if in.OS != "Rocky Linux 9.3 (Blue Onyx)" || in.Identity != "tidb" || in.Home != "/home/tidb" || !in.Exists || !in.Agent || in.Sudo == nil || !*in.Sudo {
		t.Errorf("Info: %+v", in)
	}
	if e.ag.count(ModeProbe) != 1 {
		t.Errorf("应探测一次: %+v", e.ag.calls)
	}
	e.m.Info()
	if e.ag.count(ModeProbe) != 1 {
		t.Error("探测结果应缓存")
	}
	v, r, err := e.m.Check(RunReq{Commands: []string{" df -h ", "", "uptime"}})
	if err != nil || r.Identity != "tidb" || r.Cwd != "/home/tidb" || r.Timeout != 30 || len(r.Commands) != 2 || v.Action != ActRun || v.Route != RouteInline {
		t.Fatalf("默认工作目录应取 tidb 的家目录、只读走平台进程内: %+v %+v %v", r, v, err)
	}
	if v, _, _ := e.m.Check(RunReq{Commands: []string{"systemctl restart chronyd"}}); v.Action != ActApprove || v.Route != RouteUnit {
		t.Errorf("变更应走临时单元: %+v", v)
	}
	if v, _, _ := e.m.Check(RunReq{Commands: []string{"sudo journalctl -n 5"}}); v.Action != ActApprove || v.Route != RouteUnit || !v.Sudo {
		t.Errorf("带 sudo 的命令一律按变更：要审批、走临时单元，不在平台进程内免审批执行: %+v", v)
	}
	for name, bad := range map[string]RunReq{
		"root 身份": {Identity: "root", Commands: []string{"df"}},
		"其他身份":    {Identity: "oracle", Commands: []string{"df"}},
		"目录相对":    {Cwd: "tmp", Commands: []string{"df"}},
		"目录含 ..":  {Cwd: "/a/../b", Commands: []string{"df"}},
		"目录含空格":   {Cwd: "/a b", Commands: []string{"df"}},
		"目录含引号":   {Cwd: "/a'; id; '", Commands: []string{"df"}},
		"超时":      {Timeout: 99999, Commands: []string{"df"}},
		"没有命令":    {Commands: []string{"  ", ""}},
	} {
		var ie *InvalidError
		if _, _, err := e.m.Check(bad); !errors.As(err, &ie) {
			t.Errorf("%s：应返回 InvalidError: %v", name, err)
		}
	}
	if _, _, err := e.m.Check(RunReq{Identity: "root", Commands: []string{"df"}}); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("root 身份的提示应指向 sudo 前缀: %v", err)
	}
	// 没有 tidb 用户时用探测到的家目录
	e2 := newEnv(t)
	e2.m.cfg.LookupUser = func(string) (string, bool) { return "", false }
	if _, r, err := e2.m.Check(ro("df")); err != nil || r.Cwd != "/home/tidb" {
		t.Errorf("应用探测到的家目录: %+v %v", r, err)
	}
	// 执行通道不可用
	e3 := newEnv(t)
	e3.ag.err = &AgentError{"sudo: a password is required"}
	e3.m.probed = nil
	if in := e3.m.Info(); in.Agent || !strings.Contains(in.AgentErr, "password") {
		t.Errorf("通道不可用: %+v", in)
	}
}

func TestRunReadOnlyInline(t *testing.T) {
	e := newEnv(t)
	e.ag.lines = []string{"$ df -h", "Filesystem", "[平台] 任务结束，退出码 0"}
	j, err := e.m.Run(context.Background(), "admin", RunReq{Cwd: "/home/tidb", Timeout: 120, StopOnError: true, Commands: []string{"tiup cluster display prod-tidb-01", "df -h"}})
	if err != nil || j.Level != LevelRO || j.Route != RouteInline || len(j.Lines) != 2 || j.Unit != "" {
		t.Fatalf("只读在平台进程内执行: %+v %v", j, err)
	}
	e.wait(t, j.ID)
	c := e.ag.last()
	if c.Mode != ModeRun || c.Cwd != "/home/tidb" || c.Timeout != 120 || !c.Stop || len(c.Lines) != 2 || c.Lines[1] != "df -h" || c.JobID != j.ID {
		t.Errorf("交给子进程的请求: %+v", c)
	}
	dir := e.m.jobDir(j.ID)
	for _, f := range []string{"cmds.txt", "run.sh", "mask.sed"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("平台进程内执行不应落盘 %s", f)
		}
	}
	g, _ := e.m.Job(j.ID)
	if g.Status != StatusOK || g.Exit == nil || *g.Exit != 0 {
		t.Errorf("完成: %+v", g)
	}
	if text, _, _, _ := e.m.Log(j.ID, 0); !strings.Contains(text, "Filesystem") || !strings.Contains(text, "退出码 0") {
		t.Errorf("输出: %q", text)
	}
	// 子进程返回非 0 → 失败
	e.ag.res = ChildRes{Exit: 2}
	j2, _ := e.m.Run(context.Background(), "admin", ro("uptime"))
	e.wait(t, j2.ID)
	if g, _ := e.m.Job(j2.ID); g.Status != StatusFailed || *g.Exit != 2 {
		t.Errorf("失败: %+v", g)
	}
	// 子进程报错（例如没有 sudo 免密）→ 失败并写进输出
	e.ag.res = ChildRes{Exit: -1, Err: "tidb 在中控机上没有 sudo 免密"}
	j3, _ := e.m.Run(context.Background(), "admin", ro("uptime"))
	e.wait(t, j3.ID)
	if g, _ := e.m.Job(j3.ID); g.Status != StatusFailed {
		t.Errorf("子进程报错: %+v", g)
	}
	if text, _, _, _ := e.m.Log(j3.ID, 0); !strings.Contains(text, "sudo 免密") {
		t.Errorf("错误应写进输出: %q", text)
	}
	// 通道错误
	e.ag.res = ChildRes{}
	e.ag.err = &AgentError{"sudoers 没有放行"}
	j4, err := e.m.Run(context.Background(), "admin", ro("uptime"))
	if err != nil {
		t.Fatalf("通道错误发生在后台: %v", err)
	}
	e.wait(t, j4.ID)
	if g, _ := e.m.Job(j4.ID); g.Status != StatusFailed {
		t.Errorf("通道错误应算失败: %+v", g)
	}
}

func TestRunBusyRules(t *testing.T) {
	e := newEnv(t)
	e.ag.block = make(chan struct{})
	j, err := e.m.Run(context.Background(), "admin", ro("uptime"))
	if err != nil || j.Status != StatusRunning {
		t.Fatalf("运行中: %+v %v", j, err)
	}
	if _, err := e.m.Run(context.Background(), "admin", ro("uptime")); !errors.Is(err, ErrBusy) {
		t.Errorf("同一账号并发: %v", err)
	}
	j2, err := e.m.Run(context.Background(), "admin2", ro("uptime"))
	if err != nil {
		t.Fatalf("另一个账号: %v", err)
	}
	if _, err := e.m.Run(context.Background(), "admin3", ro("uptime")); err != nil {
		t.Fatalf("第三个: %v", err)
	}
	if _, err := e.m.Run(context.Background(), "admin4", ro("uptime")); !errors.Is(err, ErrBusy) {
		t.Errorf("平台同时最多 3 批: %v", err)
	}
	close(e.ag.block)
	e.wait(t, j.ID)
	e.wait(t, j2.ID)
	if _, err := e.m.Run(context.Background(), "admin", ro("uptime")); err != nil {
		t.Errorf("结束后可以再运行: %v", err)
	}
}

func TestRunDeniedNeverCallsAgent(t *testing.T) {
	e := newEnv(t)
	for _, c := range []string{"cat ~/.ssh/id_rsa", "reboot", "mysql -e 'select 1'", "scp a b:c", "cat $(id)", "sudo -u root ls", "sudo su -"} {
		_, err := e.m.Run(context.Background(), "admin", ro(c))
		var de *DeniedError
		if !errors.As(err, &de) || de.Verdict.Action != ActDeny {
			t.Errorf("%q 应被拒绝: %v", c, err)
		}
	}
	if len(e.ag.calls) != 0 {
		t.Errorf("被拒绝的命令不能调用执行通道: %v", e.ag.calls)
	}
	if len(e.m.Jobs(10)) != 0 {
		t.Error("被拒绝的命令不应留下任务")
	}
	e.m.cfg.Agent = nil
	if _, err := e.m.Run(context.Background(), "admin", ro("df")); !errors.Is(err, ErrNoAgent) {
		t.Errorf("没有通道: %v", err)
	}
}

func TestMutatingNeedsApprovalOnceAndExactMatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mut := RunReq{Commands: []string{"sudo -n systemctl restart chronyd", "tiup cluster reload c -R pd"}}
	if _, err := e.m.Run(ctx, "admin", mut); !errors.Is(err, ErrNeedApproval) {
		t.Fatalf("变更需要先申请: %v", err)
	}
	if _, err := e.m.RequestApproval("admin", mut); err == nil || !strings.Contains(err.Error(), "原因") {
		t.Errorf("申请必须有原因: %v", err)
	}
	if _, err := e.m.RequestApproval("admin", ro("df")); !errors.Is(err, ErrNotReady) {
		t.Errorf("只读命令不需要申请: %v", err)
	}
	var de *DeniedError
	if _, err := e.m.RequestApproval("admin", RunReq{Commands: []string{"reboot"}, Reason: "x"}); !errors.As(err, &de) {
		t.Errorf("禁止的命令不能申请: %v", err)
	}
	mut.Reason = "时间同步偏差大，重启 chronyd"
	q, err := e.m.RequestApproval("admin", mut)
	if err != nil || q.State != "pending" || len(q.Lines) != 2 || q.Lines[0].Level != LevelMut || !q.Lines[0].Sudo {
		t.Fatalf("申请: %+v %v", q, err)
	}
	mut.Request = q.ID
	if _, err := e.m.Run(ctx, "admin", mut); !errors.Is(err, ErrNotReady) {
		t.Errorf("未批准不能执行: %v", err)
	}
	if _, err := e.m.Approve("admin", q.ID); !errors.Is(err, ErrSelfApprove) {
		t.Errorf("自批: %v", err)
	}
	if a, err := e.m.Approve("admin2", q.ID); err != nil || a.State != "approved" || a.ApprovedBy != "admin2" {
		t.Fatalf("批准: %+v %v", a, err)
	}
	if _, err := e.m.Approve("admin3", q.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("重复批准: %v", err)
	}
	// 改一个字（命令 / 目录 / 超时 / 遇错停止）都不行
	for name, mod := range map[string]func(*RunReq){
		"命令":   func(r *RunReq) { r.Commands[0] = "sudo systemctl restart chronyd2" },
		"加命令":  func(r *RunReq) { r.Commands = append(r.Commands, "uptime") },
		"目录":   func(r *RunReq) { r.Cwd = "/tmp" },
		"超时":   func(r *RunReq) { r.Timeout = 600 },
		"遇错停止": func(r *RunReq) { r.StopOnError = true },
	} {
		c := mut
		c.Commands = append([]string{}, mut.Commands...)
		mod(&c)
		if _, err := e.m.Run(ctx, "admin", c); err == nil {
			t.Errorf("改了%s还能用旧批准", name)
		}
	}
	// 只有申请人能执行
	if _, err := e.m.Run(ctx, "admin2", mut); !errors.Is(err, ErrNotReady) {
		t.Errorf("别人不能用你的申请: %v", err)
	}
	// 启动失败：留任务记录、不消耗申请
	e.ag.res = ChildRes{Exit: 1, Err: "tidb 在中控机上没有 sudo 免密（sudo -n 失败）：sudo: a password is required"}
	j, err := e.m.Run(ctx, "admin", mut)
	if err == nil || j.Status != StatusFailed || !strings.Contains(j.LaunchErr, "sudo 免密") {
		t.Fatalf("启动失败: %+v %v", j, err)
	}
	if rs := e.m.Requests(); rs[0].State != "approved" {
		t.Errorf("启动失败不应消耗申请: %+v", rs)
	}
	e.ag.res = ChildRes{}
	j, err = e.m.Run(ctx, "admin", mut)
	if err != nil || j.Level != LevelMut || j.Route != RouteUnit || !j.Sudo || j.Approver != "admin2" || j.Reason == "" || j.Request != q.ID || j.Unit != "tidbops-shell-"+j.ID {
		t.Fatalf("批准后执行: %+v %v", j, err)
	}
	c := e.ag.last()
	dir := e.m.jobDir(j.ID)
	if c.Mode != ModeLaunch || c.Unit != j.Unit || c.JobID != j.ID || c.Script != filepath.ToSlash(filepath.Join(dir, "run.sh")) {
		t.Errorf("应经子进程起临时单元: %+v", c)
	}
	for _, f := range []string{"run.sh", "cmds.txt", "mask.sed", "out.log", "exit", "job.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("任务目录缺 %s", f)
		}
	}
	cmds, _ := os.ReadFile(filepath.Join(dir, "cmds.txt"))
	if string(cmds) != "sudo systemctl restart chronyd\ntiup cluster reload c -R pd\n" {
		t.Errorf("cmds.txt 里 sudo 行应是规范形式（去掉 -n）: %q", cmds)
	}
	rs, _ := os.ReadFile(filepath.Join(dir, "run.sh"))
	script := string(rs)
	// 命令文本从不拼进脚本（只在 cmds.txt 里）；脚本只含校验过的身份 / 目录 / 超时
	if strings.Contains(script, "chronyd") || strings.Contains(script, "reload") {
		t.Errorf("命令文本不应出现在 run.sh 里:\n%s", script)
	}
	for _, want := range []string{"IDN='tidb'", "CWD='/home/tidb'", "TMO=30", "STOP=0", "runuser -u \"$IDN\"", `sudo\ *)`, `rest="${line#sudo }"`, "HOME=/root", "sed -u -E -f \"$JOB/mask.sed\"", "head -c 1048576", "rm -f \"$JOB/cmds.txt\"", "env -i"} {
		if !strings.Contains(script, want) {
			t.Errorf("run.sh 缺少 %q:\n%s", want, script)
		}
	}
	if text, _, _, _ := e.m.Log(j.ID, 0); !strings.Contains(text, "Running as unit") {
		t.Errorf("systemd-run 的输出应记进日志: %q", text)
	}
	// 批准只对一次执行有效
	e.finish(j.ID, "0")
	if _, err := e.m.Run(ctx, "admin", mut); !errors.Is(err, ErrNotReady) {
		t.Errorf("批准是一次性的: %v", err)
	}
	if rs := e.m.Requests(); len(rs) != 1 || rs[0].State != "used" || rs[0].Job != j.ID {
		t.Errorf("申请状态: %+v", rs)
	}
}

func TestApprovalExpires(t *testing.T) {
	e := newEnv(t)
	mut := RunReq{Commands: []string{"systemctl restart chronyd"}, Reason: "r"}
	q, _ := e.m.RequestApproval("admin", mut)
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.m.Approve("admin2", q.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("申请超过 1 小时没人批准应过期: %v", err)
	}
	e.now = e.now.Add(-2 * time.Hour)
	q2, _ := e.m.RequestApproval("admin", mut)
	_, _ = e.m.Approve("admin2", q2.ID)
	e.now = e.now.Add(90 * time.Minute)
	mut.Request = q2.ID
	if _, err := e.m.Run(context.Background(), "admin", mut); !errors.Is(err, ErrNotReady) {
		t.Errorf("批准后 1 小时内要执行: %v", err)
	}
}

func TestROViaUnitSetting(t *testing.T) {
	e := newEnv(t)
	if _, err := e.m.SetSettings(Settings{Policy: PolicyStd, RetainDays: 30, ROViaUnit: true}); err != nil {
		t.Fatal(err)
	}
	v, _, _ := e.m.Check(ro("df -h"))
	if v.Action != ActRun || v.Route != RouteUnit {
		t.Fatalf("打开开关后只读也走临时单元: %+v", v)
	}
	j, err := e.m.Run(context.Background(), "admin", ro("df -h"))
	if err != nil || j.Route != RouteUnit || e.ag.last().Mode != ModeLaunch {
		t.Errorf("只读经临时单元: %+v %v %+v", j, err, e.ag.last())
	}
	if _, err := os.Stat(filepath.Join(e.m.jobDir(j.ID), "cmds.txt")); err != nil {
		t.Error("临时单元需要 cmds.txt")
	}
}

func TestLaunchFailureKeepsJob(t *testing.T) {
	e := newEnv(t)
	e.ag.err = &AgentError{"子进程异常退出：sudo: a password is required"}
	j, err := e.m.Run(context.Background(), "admin", RunReq{Commands: []string{"systemctl restart x"}, Reason: "r"})
	if !errors.Is(err, ErrNeedApproval) {
		t.Fatalf("变更先要申请: %v", err)
	}
	_, _ = e.m.SetSettings(Settings{Policy: PolicyLoose, RetainDays: 30})
	j, err = e.m.Run(context.Background(), "admin", RunReq{Commands: []string{"systemctl restart x"}})
	var ae *AgentError
	if !errors.As(err, &ae) || j.Status != StatusFailed || !strings.Contains(j.LaunchErr, "password is required") {
		t.Fatalf("启动失败: %+v %v", j, err)
	}
	if text, _, _, _ := e.m.Log(j.ID, 0); !strings.Contains(text, "启动失败") {
		t.Errorf("日志: %q", text)
	}
	if _, err := os.Stat(filepath.Join(e.m.jobDir(j.ID), "cmds.txt")); err == nil {
		t.Error("启动失败后应删除 cmds.txt")
	}
	e.ag.err = nil
	if _, err := e.m.Run(context.Background(), "admin", RunReq{Commands: []string{"systemctl restart x"}}); err != nil {
		t.Errorf("失败的任务不占用并发名额；宽松档位变更免审批: %v", err)
	}
}

func TestLogMasksAndPaginates(t *testing.T) {
	e := newEnv(t)
	j, _ := e.m.Run(context.Background(), "admin", ro("df"))
	e.wait(t, j.ID)
	f, _ := os.OpenFile(filepath.Join(e.m.jobDir(j.ID), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("$ echo\nlogin phone=13800138000 password=Abcd1234\n第二行\n")
	_ = f.Close()
	text, off, n, err := e.m.Log(j.ID, 0)
	if err != nil || strings.Contains(text, "13800138000") || strings.Contains(text, "Abcd1234") || n < 2 {
		t.Fatalf("读取时应再遮蔽一遍: %q %d %v", text, n, err)
	}
	f, _ = os.OpenFile(filepath.Join(e.m.jobDir(j.ID), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("第三行\n")
	_ = f.Close()
	if t2, _, _, _ := e.m.Log(j.ID, off); t2 != "第三行\n" {
		t.Errorf("增量: %q", t2)
	}
	if _, _, _, err := e.m.Log("../../x", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("任务号不合法: %v", err)
	}
}

func TestCancelInlineAndUnit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 平台进程内的任务：取消 = 终止子进程，不需要密码
	e.ag.block = make(chan struct{})
	j, _ := e.m.Run(ctx, "admin", ro("uptime"))
	c, err := e.m.Cancel(ctx, "admin", j.ID)
	if err != nil || !c.Cancelled || c.Exit == nil || *c.Exit != 143 || c.Status != StatusFailed {
		t.Fatalf("取消: %+v %v", c, err)
	}
	if text, _, _, _ := e.m.Log(j.ID, 0); !strings.Contains(text, "已由 admin 取消") || !strings.Contains(text, "已取消") {
		t.Errorf("日志: %q", text)
	}
	if _, err := e.m.Cancel(ctx, "admin", j.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("已结束不能再取消: %v", err)
	}
	// 临时单元：经子进程 systemctl stop
	e.ag.block = nil
	_, _ = e.m.SetSettings(Settings{Policy: PolicyLoose, RetainDays: 30})
	u, err := e.m.Run(ctx, "admin", RunReq{Commands: []string{"systemctl restart chronyd"}})
	if err != nil || u.Route != RouteUnit {
		t.Fatalf("临时单元: %+v %v", u, err)
	}
	e.ag.res = ChildRes{Err: "sudo 执行失败：Failed to stop"}
	if _, err := e.m.Cancel(ctx, "admin", u.ID); err == nil {
		t.Error("stop 失败应报错")
	}
	e.ag.res = ChildRes{}
	cu, err := e.m.Cancel(ctx, "admin", u.ID)
	if err != nil || !cu.Cancelled || *cu.Exit != 143 {
		t.Fatalf("取消临时单元: %+v %v", cu, err)
	}
	if last := e.ag.last(); last.Mode != ModeStop || last.Unit != u.Unit {
		t.Errorf("应 systemctl stop: %+v", last)
	}
	if _, err := os.Stat(filepath.Join(e.m.jobDir(u.ID), "cmds.txt")); err == nil {
		t.Error("取消后命令文件应删除")
	}
}

func TestInterruptedOnRestart(t *testing.T) {
	e := newEnv(t)
	e.ag.block = make(chan struct{})
	j, _ := e.m.Run(context.Background(), "admin", ro("uptime"))
	// 模拟平台重启：同一目录再建一个 Manager；上次的进程内任务应标记为中断
	m2, err := NewManager(Config{Dir: e.dir, Agent: e.ag, NoPathCheck: true, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := m2.Job(j.ID)
	if g.Status != StatusFailed || g.Exit == nil || *g.Exit != 143 {
		t.Errorf("重启后应标记为中断: %+v", g)
	}
	if text, _, _, _ := m2.Log(j.ID, 0); !strings.Contains(text, "重启") {
		t.Errorf("日志: %q", text)
	}
	close(e.ag.block)
	e.wait(t, j.ID)
}

func TestSettingsPersistAndSweep(t *testing.T) {
	e := newEnv(t)
	if s := e.m.Settings(); s.Policy != PolicyStd || s.RetainDays != 30 || s.ROViaUnit {
		t.Fatalf("默认设置: %+v", s)
	}
	if _, err := e.m.SetSettings(Settings{Policy: "weird", RetainDays: 5}); err == nil {
		t.Error("非法设置应拒绝")
	}
	if _, err := e.m.SetSettings(Settings{Policy: PolicyStrict, RetainDays: 7, SummaryOnly: true, ReadRoots: []string{"/data/tidb-deploy"}, ROViaUnit: true}); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(Config{Dir: e.dir, Agent: e.ag, NoPathCheck: true, Now: func() time.Time { return e.now }})
	if err != nil || m2.Settings().Policy != PolicyStrict || m2.Settings().RetainDays != 7 || len(m2.Settings().ReadRoots) != 1 || !m2.Settings().ROViaUnit {
		t.Fatalf("设置应持久化: %+v %v", m2.Settings(), err)
	}
	// 严格档位：变更直接拒绝
	var de *DeniedError
	if _, err := e.m.Run(context.Background(), "admin", RunReq{Commands: []string{"systemctl restart x"}}); !errors.As(err, &de) {
		t.Errorf("严格档位应拒绝变更: %v", err)
	}
	// 清理：超过保留天数的任务被删；「只存摘要」时结束一段时间后压缩
	old, _ := e.m.Run(context.Background(), "admin", ro("df"))
	e.finish(old.ID, "0")
	f, _ := os.OpenFile(filepath.Join(e.m.jobDir(old.ID), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("a\nb\nc\nd\ne\n")
	_ = f.Close()
	e.now = e.now.Add(30 * time.Minute)
	e.m.lastSweep = time.Time{}
	e.m.Sweep()
	text, _, _, _ := e.m.Log(old.ID, 0)
	if !strings.Contains(text, "只存摘要") || strings.Contains(text, "d\n") {
		t.Errorf("应压缩成摘要: %q", text)
	}
	e.now = e.now.Add(10 * 24 * time.Hour)
	e.m.lastSweep = time.Time{}
	e.m.Sweep()
	if _, err := e.m.Job(old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("超过保留天数应删除: %v", err)
	}
}
