// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy_hardening_test.go
// @time: 2026/10/28

package shell

import (
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// 安全审计里证实的「只读」判定绕过：每一条在修复前都被判成只读并免审批执行。
func TestHardeningNotReadOnly(t *testing.T) {
	for c, want := range map[string]string{
		// sed 的 e / w 命令会执行程序、写文件
		"sed '1e id > /tmp/pwn' /var/log/messages":            "sed 的脚本",
		"sed 's/.*/touch \\/tmp\\/pwned/e' /var/log/messages": "sed 的脚本",
		"sed -n '1w /tmp/out' /var/log/messages":              "sed 的脚本",
		"sed -n '1,3p;5e id' /var/log/messages":               "sed 的脚本",
		"sed -n -e '1p' -e '2e id' /var/log/messages":         "sed 的脚本",
		"sed -f /tmp/script.sed /var/log/messages":            "sed 的脚本",
		"sed -s -n 1p /var/log/messages":                      "sed 的脚本",
		"sed --expression=1e\\ id /var/log/messages":          "sed 的脚本",
		"sed 's/a/b/w /tmp/x' /var/log/messages":              "sed 的脚本",
		"sed 's/a/b/;s/c/d/' /var/log/messages":               "sed 的脚本",
		// 带目录的命令名可以被同名的自备程序冒充
		"/tmp/x/cat /var/log/messages":           "以路径调用",
		"./cat /var/log/messages":                "以路径调用",
		"/home/tidb/.tiup/bin/tiup cluster list": "以路径调用",
		// 命令前的环境变量赋值会改变实际执行的程序
		"PATH=/tmp/x cat /var/log/messages":             "环境变量赋值",
		"LD_PRELOAD=/tmp/evil.so cat /var/log/messages": "环境变量赋值",
		"BASH_ENV=/tmp/x.sh ls":                         "环境变量赋值",
		// 本身只读的命令加 sudo 也按变更
		"sudo sed -n 1p /var/log/messages":  "sudo",
		"sudo cat /var/log/messages":        "sudo",
		"sudo /tmp/x/cat /var/log/messages": "以路径调用",
		// 其他会读取任意文件 / 执行程序的选项
		"wc --files0-from=/home/tidb/list":                       "files0-from",
		"sort --compress-program=/tmp/x /var/log/messages":       "compress-program",
		"sort --files0-from=/home/tidb/list":                     "files0-from",
		"date -f /home/tidb/some.conf":                           "date -f",
		"date --file=/home/tidb/some.conf":                       "date -f",
		"ip -batch /home/tidb/cmds":                              "ip -batch",
		"journalctl --file=/home/tidb/x.journal":                 "journalctl",
		"journalctl -D /home/tidb/j":                             "journalctl",
		"grep -R foo /var/log":                                   "grep -R",
		"grep -rR foo /var/log":                                  "grep -R",
		"grep --dereference-recursive foo /var/log":              "grep -R",
		"grep --exclude-from=/home/tidb/x foo /var/log/messages": "exclude-from",
		"find /var/log -name x -fprint0 /tmp/o":                  "find",
	} {
		l, why := lvl(c)
		if l != LevelMut || !strings.Contains(why, want) {
			t.Errorf("%q 不应是只读：应是变更（含 %q），实际 %s（%s）", c, want, l, why)
		}
	}
}

func TestHardeningFind(t *testing.T) {
	for c, want := range map[string]string{
		"find / -name '*.log'":          "起始目录",
		"find /etc -name '*.conf'":      "起始目录",
		"find /tidb-data -name '*.sst'": "起始目录",
		"find -name x":                  "起始目录",
		"find -L -name x":               "起始目录",
		"find /var/log/../etc -name x":  "..",
	} {
		l, why := lvl(c)
		if l != LevelDeny || !strings.Contains(why, want) {
			t.Errorf("%q 应被拒绝（含 %q），实际 %s（%s）", c, want, l, why)
		}
	}
	for _, c := range []string{"find /var/log -name '*.log' -mtime -1", "find /var/log /tidb-deploy -name x", "find -L /var/log -name x", "find /var/log ( -name a -o -name b )"} {
		if l, why := lvl(c); l != LevelRO {
			t.Errorf("%q 应仍是只读，实际 %s（%s）", c, l, why)
		}
	}
}

// 合法的只读用法不能被误伤。
func TestHardeningStillReadOnly(t *testing.T) {
	for _, c := range []string{
		"sed -n '100,200p' /var/log/messages", "sed -n 1,20p /var/log/messages", "sed -n '$p' /var/log/messages", "sed -n '/error/p' /var/log/messages",
		"sed 's/foo/bar/g' /var/log/messages", "sed -n -e '5p' /var/log/messages", "sed '1d' /var/log/messages", "sed -E 's/a+/b/' /var/log/messages",
		"cat /var/log/messages | sed -n 1p", "tail -n 100 /var/log/messages | sed 's/ERROR/E/g'",
		"LC_ALL=C sort /var/log/messages", "LANG=C grep -c error /var/log/messages", "TZ=UTC date",
		"grep -r foo /var/log", "grep -rn foo /var/log/messages",
		"date", "date +%s", "date -u", "ip addr show", "journalctl -u tidbops -n 100 --no-pager",
	} {
		if l, why := lvl(c); l != LevelRO {
			t.Errorf("%q 应仍是只读，实际 %s（%s）", c, l, why)
		}
	}
}

// sed 脚本白名单：整段完全匹配，不能夹带第二条命令。
func TestSedScriptWhitelist(t *testing.T) {
	ok := []string{"p", "d", "1p", "10,20p", "$p", "1,$d", "/error/p", "/error/d", "s/a/b/", "s/a/b/g", "1,5s/foo/bar/g", "s/a b/c d/", "s/^foo.*$/x/", "s/a/&&/", "s/a+/b/", "s/(a|b)/c/", "s/a?b/c/"}
	bad := []string{"", "1e id", "e id", "w /tmp/x", "1w /tmp/x", "r /etc/passwd", "s/a/b/e", "s/a/b/w x", "s/a/b/;p", "p;p", "p\np", "1p;2e id", "s/a/b/ge", "{p}", "/a/,/b/p", "y/a/b/", "s/a/$(id)/", "s/a/\\n/", "1~2p", "0~3p", "l", "=", "q", "Q", "x", "n;p"}
	for _, s := range ok {
		if !reSedScript.MatchString(s) {
			t.Errorf("sed 脚本 %q 应放行", s)
		}
	}
	for _, s := range bad {
		if reSedScript.MatchString(s) {
			t.Errorf("sed 脚本 %q 不应放行", s)
		}
	}
}

// 用假的文件系统视图验证符号链接解析（不依赖真实符号链接，Windows 上也能跑）。
func fakeFS(links map[string]string, strict bool) *fsView {
	res := func(p string) (string, error) {
		p = path.Clean(p)
		for from, to := range links {
			if p == from || strings.HasPrefix(p, from+"/") {
				return to + strings.TrimPrefix(p, from), nil
			}
		}
		if strings.HasPrefix(p, "/denied") {
			return "", fs.ErrPermission
		}
		return p, nil
	}
	return &fsView{resolve: res, glob: func(p string) ([]string, error) {
		switch p {
		case "/var/log/*.log":
			return []string{"/var/log/a.log", "/var/log/b.log"}, nil
		case "/var/log/m*":
			return []string{"/var/log/messages", "/var/log/escape"}, nil
		}
		return nil, nil
	}, strict: strict}
}

func TestSymlinkEscapeDenied(t *testing.T) {
	roots := ReadRoots(nil)
	links := map[string]string{
		"/tidb-deploy/link": "/tidb-data/tikv-1/db", // 允许目录里的链接指向数据目录
		"/var/log/escape":   "/etc/passwd",
		"/var/log/inner":    "/var/log/real", // 指向允许目录内部：没问题
		"/etc/os-release":   "/usr/lib/os-release",
		"/var/log/dirlink":  "/tidb-data",
	}
	f := fakeFS(links, true)
	for _, tc := range []struct {
		line string
		want Level
	}{
		{"cat /tidb-deploy/link/000001.sst", LevelDeny},
		{"cat /var/log/escape", LevelDeny},
		{"cat /var/log/dirlink/x", LevelDeny},
		{"grep foo /var/log/m*", LevelDeny}, // 通配符展开后有一个指向目录之外
		{"cat /var/log/inner", LevelRO},
		{"cat /var/log/messages", LevelRO},
		{"cat /etc/os-release", LevelRO}, // 链接指向 /usr/lib/os-release，但允许目录本身解析后也在名单里
		{"grep foo /var/log/*.log", LevelRO},
		{"find /var/log/dirlink -name x", LevelDeny},
		{"find /var/log -name x", LevelRO},
	} {
		it := classifyLine(tc.line, roots, f)
		if it.Level != tc.want {
			t.Errorf("%q 应是 %s，实际 %s（%s）", tc.line, tc.want, it.Level, it.Why)
		}
	}
	// 不带文件系统视图时只按文本判断（平台进程里的旧行为），说明检查确实来自解析
	if it := classifyLine("cat /var/log/escape", roots, nil); it.Level != LevelRO {
		t.Errorf("无视图时按文本判断应是只读: %s", it.Level)
	}
}

func TestSymlinkResolveErrors(t *testing.T) {
	roots := append(ReadRoots(nil), "/denied/logs")
	line := "cat /denied/logs/x.log"
	if it := classifyLine(line, roots, fakeFS(nil, true)); it.Level != LevelDeny {
		t.Errorf("子进程里（strict）解析出错应拒绝: %s %s", it.Level, it.Why)
	}
	if it := classifyLine(line, roots, fakeFS(nil, false)); it.Level != LevelRO {
		t.Errorf("平台进程预判定（non-strict）解析不了应退回文本判断，由子进程兜底: %s %s", it.Level, it.Why)
	}
	// 不存在的文件不算错误：文本在允许目录里即可
	_, err := resolveExisting("/definitely/not/existing/path/xyz")
	if err != nil {
		t.Errorf("不存在的路径不应报错: %v", err)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		t.Error("不存在的路径不应返回 PathError")
	}
}

// 子进程执行时的 PATH：tidb 可写的 ~/.tiup/bin 必须排在系统目录之后，否则同名文件能冒充 cat / ls。
func TestPathOrderSystemFirst(t *testing.T) {
	t.Setenv("HOME", "/home/tidb")
	env, _, _ := childEnv()
	var p string
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			p = strings.TrimPrefix(e, "PATH=")
		}
	}
	parts := strings.Split(p, ":")
	if len(parts) < 2 || parts[len(parts)-1] != "/home/tidb/.tiup/bin" || parts[0] == "/home/tidb/.tiup/bin" {
		t.Errorf("~/.tiup/bin 应排在最后: %s", p)
	}
}
