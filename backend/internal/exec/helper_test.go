// @author: kevinkelly
// @license: Apache License 2.0
// @file: helper_test.go
// @time: 2026/10/28

package exec

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 与 fakecluster 相同风格的 display 输出，用来构造测试拓扑（同时覆盖 ParseTopo）。
const displayText = `Cluster type:       tidb
Cluster name:       prod-tidb-01
Cluster version:    v8.5.2
Deploy user:        tidb
SSH type:           builtin
Dashboard URL:      http://10.0.1.21:2379/dashboard
Grafana URL:        http://10.0.1.31:3000
ID                 Role          Host       Ports        OS/Arch       Status   Data Dir                      Deploy Dir
--                 ----          ----       -----        -------       ------   --------                      ----------
10.0.1.31:9093     alertmanager  10.0.1.31  9093/9094    linux/x86_64  Up       /tidb-data/alertmanager-9093  /tidb-deploy/alertmanager-9093
10.0.1.31:3000     grafana       10.0.1.31  3000         linux/x86_64  Up       -                             /tidb-deploy/grafana-3000
10.0.1.21:2379     pd            10.0.1.21  2379/2380    linux/x86_64  Up|L|UI  /tidb-data/pd-2379            /tidb-deploy/pd-2379
10.0.1.22:2379     pd            10.0.1.22  2379/2380    linux/x86_64  Up       /tidb-data/pd-2379            /tidb-deploy/pd-2379
10.0.1.23:2379     pd            10.0.1.23  2379/2380    linux/x86_64  Up       /tidb-data/pd-2379            /tidb-deploy/pd-2379
10.0.1.31:9090     prometheus    10.0.1.31  9090/12020   linux/x86_64  Up       /tidb-data/prometheus-9090    /tidb-deploy/prometheus-9090
10.0.1.11:4000     tidb          10.0.1.11  4000/10080   linux/x86_64  Up       -                             /tidb-deploy/tidb-4000
10.0.1.12:4000     tidb          10.0.1.12  4000/10080   linux/x86_64  Up       -                             /tidb-deploy/tidb-4000
10.0.1.13:4000     tidb          10.0.1.13  4000/10080   linux/x86_64  Up       -                             /tidb-deploy/tidb-4000
10.0.2.11:20160    tikv          10.0.2.11  20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160         /tidb-deploy/tikv-20160
10.0.2.12:20160    tikv          10.0.2.12  20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160         /tidb-deploy/tikv-20160
10.0.2.13:20160    tikv          10.0.2.13  20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160         /tidb-deploy/tikv-20160
10.0.2.14:20160    tikv          10.0.2.14  20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160         /tidb-deploy/tikv-20160
10.0.2.15:20160    tikv          10.0.2.15  20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160         /tidb-deploy/tikv-20160
10.0.2.16:20160    tikv          10.0.2.16  20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160         /tidb-deploy/tikv-20160
10.0.3.11:3930     tiflash       10.0.3.11  9000/8123/3930/20170/20292/8234  linux/x86_64  Up  /tidb-data/tiflash-9000  /tidb-deploy/tiflash-9000
10.0.3.12:3930     tiflash       10.0.3.12  9000/8123/3930/20170/20292/8234  linux/x86_64  Up  /tidb-data/tiflash-9000  /tidb-deploy/tiflash-9000
10.0.4.11:8300     cdc           10.0.4.11  8300         linux/x86_64  Up       /tidb-data/cdc-8300           /tidb-deploy/cdc-8300
10.0.4.12:8300     cdc           10.0.4.12  8300         linux/x86_64  Up       /tidb-data/cdc-8300           /tidb-deploy/cdc-8300
Total nodes: 19
`

const testCluster = "prod-tidb-01"

var testKey = []byte("0123456789abcdef0123456789abcdef")

func testTopo() Topo {
	t, err := ParseTopo(displayText)
	if err != nil {
		panic(err)
	}
	t.SSHPort = 22
	return t
}

func testPolicy(tempDir string) Policy {
	return Policy{
		Clusters:    []string{testCluster, "slowc", "failc", "bigc"},
		TiupBin:     "tiup",
		MySQLBin:    "mysql",
		MySQLUser:   "tidbops_ops",
		MySQLPwdEnv: "TIDBOPS_DB_PWD",
		BackupRoots: []string{"local:///data/backup/", "s3://bkt/tidb/"},
		TempDir:     tempDir,
		AlertURL:    "http://10.0.1.31:9093",
	}
}

// signedReq 为命令签发 5 分钟有效的令牌。
func signedReq(c Cmd) Request {
	return Request{Cmd: c, ChangeID: "CHG-1", Actor: "alice", Token: Sign(testKey, c, "CHG-1", time.Now().Add(5*time.Minute))}
}

func mustTopo(t *testing.T) TopoFunc {
	t.Helper()
	return StaticTopo(testTopo())
}

// TestMain：测试二进制自身兼任「假的 tiup / mysql / sudo 包装 / exec 子进程」，
// 这样 DirectRunner 与 SudoRunner 可以在 Windows 上跑真实的 os/exec 流程而不依赖外部程序。
// 约定：第一个参数不是 -test. 开头时进入 helper 模式。
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(helperMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helperMain(args []string) int {
	if args[0] == "-n" { // sudo -n -u <user> <bin> exec --stdin
		return fakeSudo(args)
	}
	return fakeTool(args)
}

func fakeSudo(args []string) int {
	mode := "good"
	if len(args) >= 4 {
		mode = args[3]
	}
	switch mode {
	case "good":
		r := &SimRunner{Pol: testPolicy(""), Topo: StaticTopo(testTopo()), Key: testKey}
		if err := Serve(context.Background(), os.Stdin, os.Stdout, r); err != nil {
			return 2
		}
		return 0
	case "echoargs":
		b, _ := json.Marshal(map[string]string{"t": "out", "line": strings.Join(args, "|")})
		fmt.Println(string(b))
		fmt.Println(`{"t":"result","exit":0,"millis":1}`)
		return 0
	case "hang":
		time.Sleep(10 * time.Second)
		return 0
	case "junk":
		fmt.Println("this is not json")
		return 0
	case "exit3":
		return 3
	case "denied":
		fmt.Fprintln(os.Stderr, "sudo: a password is required")
		return 1
	case "noresult":
		fmt.Println(`{"t":"out","line":"hello"}`)
		return 0
	case "extra":
		fmt.Println(`{"t":"result","exit":0,"millis":1}`)
		fmt.Println(`{"t":"out","line":"late"}`)
		return 0
	case "badtype":
		fmt.Println(`{"t":"weird"}`)
		return 0
	case "failresult":
		fmt.Println(`{"t":"result","exit":-1,"err":"令牌已过期","millis":1}`)
		return 0
	case "nonzero":
		fmt.Println(`{"t":"result","exit":2,"output":"x\n","err":"退出码 2","millis":1}`)
		return 0
	}
	return 9
}

// fakeTool 模拟 tiup / mysql：把收到的 argv 与环境变量名打印出来，供测试断言。
func fakeTool(args []string) int {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	// 自动修复测试：TIUP_HOME 下有 fake-tiup 标记文件时，记录调用并模拟 ctl 文件缺失（broken）/ 安装失败（install-fails）
	if home := os.Getenv("TIUP_HOME"); home != "" && len(args) > 0 {
		if _, err := os.Stat(filepath.Join(home, "fake-tiup")); err == nil {
			if f, err := os.OpenFile(filepath.Join(home, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				fmt.Fprintln(f, strings.Join(args, "|"))
				f.Close()
			}
			_, broken := os.Stat(filepath.Join(home, "broken"))
			switch {
			case args[0] == "uninstall":
				return 0
			case args[0] == "install":
				if _, err := os.Stat(filepath.Join(home, "install-fails")); err == nil {
					fmt.Fprintln(os.Stderr, "Error: fetch manifest failed: network unreachable")
					return 1
				}
				os.Remove(filepath.Join(home, "broken"))
				return 0
			case strings.HasPrefix(args[0], "ctl:") && broken == nil:
				comp, ver, _ := strings.Cut(args[0], ":")
				fmt.Fprintf(os.Stderr, "Starting component `%s`: /home/tidb/.tiup/components/%s/%s/%s pd\n", comp, comp, ver, comp)
				fmt.Fprintf(os.Stderr, "Error: Failed to start component `%s`: fork/exec /home/tidb/.tiup/components/%s/%s/%s: no such file or directory\n", comp, comp, ver, comp)
				return 1
			}
		}
	}
	fmt.Fprintln(w, "ARGS:"+strings.Join(args, "|"))
	var names []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		names = append(names, strings.ToUpper(k))
	}
	sort.Strings(names)
	fmt.Fprintln(w, "ENV:"+strings.Join(names, ","))
	if pw := os.Getenv("MYSQL_PWD"); pw != "" {
		fmt.Fprintln(w, "PWD-IS:"+pw) // 用于验证输出里的密码被替换为 ***
	}
	for _, a := range args {
		if strings.HasSuffix(a, ".yaml") {
			if fi, err := os.Stat(a); err == nil {
				b, _ := os.ReadFile(a)
				fmt.Fprintf(w, "FILE:%s|%d|%s\n", fi.Mode().Perm(), len(b), strings.SplitN(string(b), "\n", 2)[0])
			}
		}
		switch a {
		case "slowc":
			w.Flush()
			time.Sleep(10 * time.Second)
		case "failc":
			fmt.Fprintln(os.Stderr, "boom on stderr")
			w.Flush()
			return 3
		case "bigc":
			line := strings.Repeat("x", 1023)
			for i := 0; i < 600; i++ {
				fmt.Fprintln(w, line)
			}
		}
	}
	return 0
}
