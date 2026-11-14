// @author: kevinkelly
// @license: Apache License 2.0
// @file: summary_test.go
// @time: 2026/10/28

package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testPW  = "Tp-testpw0123456789abcdef"
	testPW2 = "Tp-second0123456789abcdefg"
)

// 安装结束打印：访问地址与端口、账号、初始口令、各目录的绝对路径；口令不出现在任何命令行参数里。
func TestInstallPrintsAccessSummary(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Listen: "127.0.0.1:9090", PublicURL: "https://ops.example.com", Start: true}); err != nil {
		t.Fatalf("install: %v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "访问信息", out, "访问信息", "http://127.0.0.1:9090", "监听端口：9090", "账号：admin\n", "密码："+testPW, "账号：admin2", "密码："+testPW2, "两名管理员", "只显示这一次", "https://ops.example.com", "SSH 隧道")
	mustContain(t, "目录", out, "安装根目录", v.l.Opt(), v.l.ReleaseDir("1.0.0"), v.l.Data(), v.l.Etc(), v.l.Backups(), v.l.Unit(), "前端文件", "数据目录", "配置目录", "备份目录", "systemd 单元")
	mustNotContain(t, "访问信息", out, "尚未启动")

	// 创建管理员经 runuser，以 tidbops 身份；口令不在命令行里
	nCreate := 0
	for _, c := range v.sys.calls {
		if strings.HasPrefix(c, "runuser") {
			nCreate++
			mustContain(t, "runuser 命令", c, "-s /bin/sh tidbops -c", "user add --data-dir", "--role admin", "--gen-password")
		}
		if strings.Contains(c, testPW) || strings.Contains(c, testPW2) {
			t.Errorf("口令不应出现在命令里: %s", c)
		}
	}
	if nCreate != 2 {
		t.Errorf("应创建两名管理员（admin、admin2），实际调用 %d 次: %v", nCreate, v.sys.calls)
	}
	// 管理员必须在服务启动之前创建（否则服务首次启动会自己建一个，口令只在日志里）
	if iu, is := v.sys.indexOf("runuser"), v.sys.indexOf("systemctl enable --now tidbops"); iu < 0 || is < 0 || iu > is {
		t.Errorf("应先创建管理员再启动服务: runuser=%d start=%d", iu, is)
	}
}

func TestInstallSummaryDefaultsAndNotStarted(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	mustContain(t, "未启动", v.dumpOut(), "http://127.0.0.1:8080", "监听端口：8080", "尚未启动", "systemctl enable --now tidbops", "密码："+testPW)
}

// 监听全部网卡：列出本机地址（至少有回环兜底），端口不变。
func TestAccessURLs(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		us := accessURLs(listen)
		if len(us) == 0 || us[len(us)-1] != "http://127.0.0.1:8080" {
			t.Errorf("%s → %v", listen, us)
		}
		for _, u := range us {
			if !strings.HasSuffix(u, ":8080") {
				t.Errorf("%s 端口不对: %s", listen, u)
			}
		}
	}
	if got := accessURLs("10.0.0.5:9000"); len(got) != 1 || got[0] != "http://10.0.0.5:9000" {
		t.Errorf("指定地址: %v", got)
	}
	if got := accessURLs("[::1]:9000"); len(got) != 1 || got[0] != "http://[::1]:9000" {
		t.Errorf("IPv6: %v", got)
	}
}

// 数据目录里已有用户（重装 / 保留数据）：不再创建管理员，也不会覆盖口令。
func TestInstallKeepsExistingUsers(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := os.MkdirAll(v.l.Data(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.l.Data(), "users.json"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Start: true}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	for _, c := range v.sys.calls {
		if strings.HasPrefix(c, "runuser") {
			t.Errorf("已有用户时不应创建管理员: %s", c)
		}
	}
	out := v.dumpOut()
	mustContain(t, "已有用户", out, "沿用原来的账号与口令", "user passwd")
	mustNotContain(t, "已有用户", out, testPW, "只显示这一次")
	if v.readData("users.json") != "existing" {
		t.Error("改动了已有用户数据")
	}
}

// 创建管理员失败不应让安装失败：说明原因，并告诉用户首次启动后口令在日志里。
func TestInstallAdminCreationFailureIsNotFatal(t *testing.T) {
	v := newEnv(t)
	v.adminErr = errors.New("runuser: command not found")
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Start: true}); err != nil {
		t.Fatalf("创建管理员失败不应让安装失败: %v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "失败说明", out, "安装完成", "自动创建管理员失败", "runuser: command not found", "journalctl -u tidbops", "初始口令", "访问信息")
	mustNotContain(t, "失败说明", out, testPW)
	if v.cur() != "1.0.0" {
		t.Errorf("current = %q", v.cur())
	}
}

func TestInstallDryRunSummaryHasNoPassword(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, DryRun: true, Listen: "0.0.0.0:8443"}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "dry-run", out, "访问信息", ":8443", "dry-run：不创建账号", v.l.Opt())
	mustNotContain(t, "dry-run", out, testPW)
	for _, c := range v.sys.calls {
		if strings.HasPrefix(c, "runuser") {
			t.Errorf("dry-run 不应创建管理员: %s", c)
		}
	}
}

// 非独立部署同样打印访问信息与口令。
func TestInstallClusterModePrintsSummary(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "c1", Start: true}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	mustContain(t, "同机部署", v.dumpOut(), "访问信息", "密码："+testPW, "http://127.0.0.1:8080", v.l.Data())
}

// 默认创建两名管理员；第二名失败时第一名的账号口令仍要打印，并提示补建第二名。
func TestInstallSecondAdminFailureStillPrintsFirst(t *testing.T) {
	v := newEnv(t)
	v.admin2Err = errors.New("exit status 1")
	pkg := v.makePkg("1.0.0", pkgOpt{})
	if err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Standalone: true, Start: true}); err != nil {
		t.Fatalf("第二名管理员创建失败不应让安装失败: %v\n%s", err, v.dumpOut())
	}
	out := v.dumpOut()
	mustContain(t, "第二名失败", out, "账号：admin\n", "密码："+testPW, "管理员 admin2 创建失败", "手工创建第二名管理员")
	mustNotContain(t, "第二名失败", out, testPW2, "账号：admin2")
}
