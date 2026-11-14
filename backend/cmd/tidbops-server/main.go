// @author: kevinkelly
// @license: Apache License 2.0
// @file: main.go
// @time: 2026/10/28

// tidbops-server：TiDB 运维平台后端，同时托管前端页面。
//
//	# 开发 / 演示：回放样例数据包（不连任何集群），并启用运维模式（登录、审批、模拟执行器）
//	tidbops-server --fixtures ../fixtures/demo --web ../frontend --data-dir /tmp/tidbops --dev-seed --env staging
//
//	# 在 tiup 中控机上直连真实集群（只读；地址与集群名默认自动探测）
//	TIDBOPS_MYSQL_PWD=xxx tidbops-server --web /opt/tidbops/web --mysql-user readonly
//
//	# 同机运维模式：Web 进程（tidbops 用户）通过 sudo 调用受限的 exec 入口（tidb 用户）
//	tidbops-server --web ... --data-dir /var/lib/tidbops --exec sudo --exec-sudo-user tidb --exec-policy /etc/tidb-ops/exec.json
//
//	# 多集群：启动参数构建的是主集群；其余集群在 <data-dir>/clusters.json（平台「集群接入」页或 /api/clusters 管理，热生效）。
//	# 回放集群的数据包目录必须在 --fixtures-root 之下（默认：与 --fixtures 同级的目录）。详见 docs/运维部署手册.md 6.3，以及页面「集群接入」帮助
//	tidbops-server --fixtures ../fixtures/demo --fixtures-root ../fixtures --web ../frontend --data-dir /tmp/tidbops --dev-seed --env staging
//
//	# exec 入口（由 sudo 调用，读取 stdin 里的一个 JSON 请求）
//	tidbops-server exec --stdin --policy /etc/tidb-ops/exec.json --key-file /etc/tidb-ops/exec.key
//
//	# 用户管理（首次部署创建管理员等）
//	TIDBOPS_USER_PASSWORD=xxx tidbops-server user add --data-dir /var/lib/tidbops --name alice --role admin
//
//	# 平台自身的部署 / 升级 / 回滚（在解压后的发布包内运行；详见 tidbops-server admin -h）
//	sudo ./bin/tidbops-server admin install --cluster prod-tidb-01 --env prod
//	sudo ./bin/tidbops-server admin upgrade
//	sudo /opt/tidbops/current/bin/tidbops-server admin rollback
//
// 不带 --data-dir 时服务保持只读（无登录、仅 GET）。默认只监听 127.0.0.1:8080；非本机监听必须显式加 --allow-remote，
// 并且应放在带 TLS 与访问控制的反向代理之后。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/authz"
	"tidbops/internal/buildinfo"
	"tidbops/internal/change"
	"tidbops/internal/clusters"
	"tidbops/internal/collect"
	"tidbops/internal/dataver"
	xexec "tidbops/internal/exec"
	"tidbops/internal/expiry"
	"tidbops/internal/release"
	"tidbops/internal/server"
	"tidbops/internal/shell"
	"tidbops/internal/source"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "exec":
			os.Exit(runExec(os.Args[2:]))
		case "shell-exec":
			os.Exit(runShellExec(os.Args[2:]))
		case "user":
			os.Exit(runUser(os.Args[2:]))
		case "admin":
			os.Exit(release.Main(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	runServer()
}

// ---------------------------------------------------------------- 公共 ----

func readOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) < 16 {
			return nil, fmt.Errorf("%s 不是有效的密钥文件（应为 ≥16 字节的十六进制）", path)
		}
		return k, nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func osRun(ctx context.Context, argv []string) ([]byte, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
}

func loadPolicy(path string) (xexec.Policy, error) {
	var p xexec.Policy
	if path == "" {
		return p, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// ---------------------------------------------------------------- exec 入口 ----

func runExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	stdin := fs.Bool("stdin", false, "从 stdin 读取一个 JSON 请求（必填）")
	policyFile := fs.String("policy", "/etc/tidb-ops/exec.json", "执行器策略文件")
	keyFile := fs.String("key-file", "/etc/tidb-ops/exec.key", "审批令牌密钥文件")
	envFile := fs.String("env-file", "/etc/tidb-ops/exec.env", "额外环境变量文件（KEY=VALUE 每行一条，如运维账号口令；sudo 会清空环境，所以放在这里）；不存在则忽略")
	_ = fs.Parse(args)
	if b, err := os.ReadFile(*envFile); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			if k, v, ok := strings.Cut(ln, "="); ok && strings.TrimSpace(k) != "" {
				_ = os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
			}
		}
	}
	if !*stdin {
		fmt.Fprintln(os.Stderr, "exec 入口只接受 --stdin")
		return 2
	}
	pol, err := loadPolicy(*policyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取策略失败:", err)
		return 2
	}
	kb, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取密钥失败:", err)
		return 2
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(kb)))
	if err != nil || len(key) < 16 {
		fmt.Fprintln(os.Stderr, "密钥格式不正确")
		return 2
	}
	tiup := pol.TiupBin
	if tiup == "" {
		tiup = "tiup"
	}
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		if len(argv) > 0 && argv[0] == "tiup" {
			argv = append([]string{tiup}, argv[1:]...)
		}
		return osRun(ctx, argv)
	}
	r := xexec.DirectRunner{Pol: pol, Topo: xexec.DisplayTopo(run), Key: key}
	if err := xexec.Serve(context.Background(), os.Stdin, os.Stdout, r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- shell-exec 入口 ----

// runShellExec 是「中控机命令」的子进程入口：平台进程经 sudoers 放行的 `sudo -n -u tidb <bin> shell-exec --stdin` 调用，
// 以 tidb 身份执行只读命令、或用 tidb 的 sudo 免密起 / 停 systemd-run 临时单元（见 internal/shell/child.go）。
func runShellExec(args []string) int {
	fs := flag.NewFlagSet("shell-exec", flag.ExitOnError)
	stdin := fs.Bool("stdin", false, "从 stdin 读取一个 JSON 请求（必填）")
	_ = fs.Parse(args)
	if !*stdin {
		fmt.Fprintln(os.Stderr, "shell-exec 入口只接受 --stdin")
		return 2
	}
	if err := shell.ServeChild(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- 用户管理 ----

func runUser(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: user add|list|passwd|disable|enable|totp-reset ... --data-dir DIR（口令从环境变量 TIDBOPS_USER_PASSWORD 读取）")
		return 2
	}
	fs := flag.NewFlagSet("user", flag.ExitOnError)
	dir := fs.String("data-dir", "", "数据目录")
	name := fs.String("name", "", "用户名")
	display := fs.String("display", "", "显示名")
	role := fs.String("role", "viewer", "角色：viewer|developer|sre|dba|approver|admin|auditor 或自定义角色 id（逗号分隔可多个）")
	env := fs.String("env", "*", "角色适用的环境：* | test | staging | prod")
	dbs := fs.String("db", "", "developer 可见的库，逗号分隔")
	insecure := fs.Bool("insecure-password", false, "passwd 时跳过口令强度策略（仅限本机演示 / 开发，请勿用于生产）")
	genPw := fs.Bool("gen-password", false, "add 时生成随机口令并打印一行 "+release.AdminPasswordPrefix+"<口令>（安装器用它创建初始管理员；口令不经过命令行参数）")
	_ = fs.Parse(args[1:])
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "需要 --data-dir")
		return 2
	}
	mk, err := readOrCreateKey(filepath.Join(*dir, "master.key"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := authz.OpenStore(filepath.Join(*dir, "users.json"), mk)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// 打开自定义角色库，使 --role 能识别自定义角色 id（只读使用，不会写 roles.json）
	if _, err := authz.OpenRoleStore(filepath.Join(*dir, "roles.json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pw := os.Getenv("TIDBOPS_USER_PASSWORD")
	fail := func(err error) int { fmt.Fprintln(os.Stderr, "失败:", err); return 1 }
	switch args[0] {
	case "add":
		var rs []authz.Role
		for _, r := range strings.Split(*role, ",") {
			rs = append(rs, authz.Role(strings.TrimSpace(r)))
		}
		if *genPw { // 与服务首次启动创建 admin 时同一套随机口令（Tp- + 24 位十六进制），不经口令强度检查
			pw = randomPassword()
		} else if err := authz.CheckPassword(*name, pw); err != nil {
			return fail(err)
		}
		var d []string
		if *dbs != "" {
			d = strings.Split(*dbs, ",")
		}
		if err := st.Add(*name, *display, pw, map[string][]authz.Role{*env: rs}, d); err != nil {
			return fail(err)
		}
		if *genPw {
			fmt.Println(release.AdminPasswordPrefix + pw)
		}
	case "passwd":
		if *insecure {
			fmt.Fprintln(os.Stderr, "警告：已跳过口令强度策略，仅限本机演示 / 开发使用")
			if err := st.SetPasswordInsecure(*name, pw); err != nil {
				return fail(err)
			}
			break
		}
		if err := authz.CheckPassword(*name, pw); err != nil {
			return fail(err)
		}
		if err := st.SetPassword(*name, pw); err != nil {
			return fail(err)
		}
	case "disable", "enable":
		if err := st.SetDisabled(*name, args[0] == "disable"); err != nil {
			return fail(err)
		}
	case "totp-reset":
		if err := st.ResetTOTP(*name); err != nil {
			return fail(err)
		}
	case "list":
		for _, u := range st.List() {
			last, dept := "-", u.Dept
			if u.LastLogin != nil {
				last = u.LastLogin.Local().Format("2006-01-02 15:04")
			}
			if dept == "" {
				dept = "-"
			}
			fmt.Printf("%-16s %-10v dept=%s roles=%v totp=%v disabled=%v lastLogin=%s\n", u.Name, u.Display, dept, u.Roles, u.HasTOTP, u.Disabled, last)
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "未知子命令", args[0])
		return 2
	}
	fmt.Println("完成")
	return 0
}

// ---------------------------------------------------------------- 服务 ----

func runServer() {
	listen := flag.String("listen", "127.0.0.1:8080", "监听地址")
	allowRemote := flag.Bool("allow-remote", false, "允许监听非本机地址（务必放在带 TLS 与访问控制的反向代理之后）")
	web := flag.String("web", ".", "前端目录（含 index.html、js/、css/、help/；源码里是 frontend/）")
	fixtures := flag.String("fixtures", "", "样例数据包目录；设置后进入回放模式，不连接任何集群")
	cluster := flag.String("cluster", "", "tiup 集群名；留空则自动探测")
	pd := flag.String("pd", "", "PD 地址")
	tidb := flag.String("tidb-status", "", "TiDB 状态端口地址")
	prom := flag.String("prom", "", "Prometheus 地址")
	alert := flag.String("alert", "", "Alertmanager 地址")
	myHost := flag.String("mysql-host", "", "只读 SQL 的 TiDB 地址")
	myPort := flag.Int("mysql-port", 0, "TiDB SQL 端口，默认 4000")
	myUser := flag.String("mysql-user", "", "只读账号（密码用环境变量 TIDBOPS_MYSQL_PWD）")
	myBin := flag.String("mysql-bin", "mysql", "mysql 客户端路径")
	tiup := flag.String("tiup", "tiup", "tiup 路径")
	home := flag.String("home", "", "tiup 用户主目录，默认当前用户主目录")
	timeout := flag.Duration("timeout", 20*time.Second, "单个数据源请求超时")
	cacheTTL := flag.Duration("cache-ttl", 15*time.Second, "数据源响应缓存时间（保护生产环境）")
	snapTTL := flag.Duration("snapshot-ttl", 10*time.Second, "快照缓存时间")
	noAuto := flag.Bool("no-auto", false, "关闭自动探测")
	showVersion := flag.Bool("version", false, "显示版本")
	// 运维模式
	dataDir := flag.String("data-dir", "", "数据目录；设置后启用运维模式（登录 / 审批 / 审计 / 变更执行），否则服务只读")
	envName := flag.String("env", "prod", "本集群所属环境：test | staging | prod")
	execMode := flag.String("exec", "", "执行器：sim（模拟，仅回放模式默认）| direct（本进程直接执行）| sudo（经 sudo 调用受限 exec 入口）| none")
	execPolicy := flag.String("exec-policy", "", "执行器策略文件（JSON）；缺省按启动参数生成")
	execSudoUser := flag.String("exec-sudo-user", "tidb", "sudo 模式下执行 tiup 的账号")
	execBin := flag.String("exec-bin", "", "sudo 模式下的平台二进制路径，默认当前可执行文件")
	keyFile := flag.String("key-file", "", "审批令牌密钥文件，默认 <data-dir>/exec.key（sudo 模式下须让 exec 账号可读）")
	noMFA := flag.Bool("no-mfa", false, "关闭生产高风险变更的申请人 MFA（仅限测试 / POC）")
	devSeed := flag.Bool("dev-seed", false, "回放模式下创建演示账号（口令 "+authz.DemoPassword+"），live 模式拒绝")
	secureCookie := flag.Bool("secure-cookie", false, "会话 Cookie 加 Secure（放在 HTTPS 反向代理后时开启）")
	readonlyFile := flag.String("readonly-file", "/etc/tidb-ops/READONLY", "存在该文件时平台强制只读")
	publicURL := flag.String("public-url", "", "平台对外访问地址（通知消息里的链接用），如 https://ops.example.com")
	notifyHosts := flag.String("notify-allow-hosts", "", "额外允许的 Webhook 主机，逗号分隔（私有化部署的网关用；默认只允许官方域名）")
	simDelay := flag.Duration("sim-delay", 1500*time.Millisecond, "模拟执行器每条命令的耗时")
	fixturesRoot := flag.String("fixtures-root", "", "多集群：允许添加回放集群的数据包白名单根目录，逗号分隔；留空 = 只允许与 --fixtures 同级的目录（没有 --fixtures 则不允许添加回放集群）")
	standalone := flag.Bool("standalone", false, "独立部署：平台本身不代表任何集群、不依赖本机 tiup / tidb 用户，集群全部由管理员在「集群接入」页添加（只读接入）；需要 --data-dir")
	localTiup := flag.Bool("local-tiup", false, "与 --standalone 同用：本机是 tiup 中控机。管理员可在「集群接入」页一键导入本机 tiup 管理的集群并做全部运维操作（经 --exec sudo|direct 的执行器）；手工添加的远程集群仍为只读")
	configFile := flag.String("config", defaultConfigPath, "平台配置文件（JSON，可选，不存在则忽略；改动需重启生效）。目前支持 icp：备案号，显示在登录页与页脚")
	flag.Parse()
	if *showVersion {
		fmt.Printf("tidbops-server %s (commit %s, built %s)\n", selfVersion(), orUnknown(buildinfo.Commit), orUnknown(buildinfo.BuildTime))
		return
	}

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatalf("监听地址不合法: %v", err)
	}
	if ip := net.ParseIP(host); (host != "localhost" && (ip == nil || !ip.IsLoopback())) && !*allowRemote {
		log.Fatalf("拒绝监听非本机地址 %s：请加 --allow-remote，并放在带 TLS 与访问控制的反向代理之后", host)
	}

	if *standalone {
		switch {
		case *dataDir == "":
			log.Fatal("--standalone 需要 --data-dir（运维模式：登录、审计、集群接入都依赖它）")
		case *fixtures != "" || *cluster != "" || *pd != "" || *tidb != "" || *prom != "" || *alert != "" || *myHost != "" || *myUser != "":
			log.Fatal("--standalone 不能与 --fixtures / --cluster / --pd / --tidb-status / --prom / --alert / --mysql-host / --mysql-user 同用：独立部署的平台不代表任何集群，请在页面「集群接入」里添加")
		case *devSeed:
			log.Fatal("--standalone 不能与 --dev-seed 同用")
		case !*localTiup && *execMode != "" && *execMode != "none":
			log.Fatal("--standalone 不支持 --exec " + *execMode + "：独立部署的平台默认不在本机执行集群命令（只读接入）；本机是 tiup 中控机时请加 --local-tiup")
		case *localTiup && *execMode != "sudo" && *execMode != "direct":
			log.Fatal("--local-tiup 需要 --exec sudo（推荐，特权分离）或 --exec direct")
		}
	} else if *localTiup {
		log.Fatal("--local-tiup 只能与 --standalone 同用")
	}

	h := *home
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	params := source.CatalogParams{Cluster: *cluster, Home: h, Now: time.Now()}
	var src source.Source
	mode := "live"
	var alertAddr string
	if *standalone {
		src, params.Cluster = source.Unconfigured{}, clusters.PlatformName
		if *localTiup {
			log.Printf("独立部署模式（中控机）：管理员可在「集群接入」页一键导入本机 tiup 管理的集群（可做全部运维操作）；手工添加的远程集群为只读")
		} else {
			log.Printf("独立部署模式：平台不连接任何集群；请用管理员登录后在「集群接入」页添加集群（只读接入）")
		}
	} else if *fixtures != "" {
		rp, err := source.NewReplay(*fixtures)
		if err != nil {
			log.Fatalf("打开样例数据包失败: %v", err)
		}
		src, mode = rp, "replay"
		params.Cluster = rp.Manifest().Cluster
		log.Printf("回放模式：%s（集群 %s，采集于 %s）", *fixtures, rp.Manifest().Cluster, rp.Manifest().CollectedAt.Format(time.RFC3339))
	} else {
		cfg := source.LiveConfig{Endpoints: map[string]string{}, TiupBin: *tiup, Timeout: *timeout,
			MySQL: source.MySQLConfig{Bin: *myBin, Host: *myHost, Port: *myPort, User: *myUser, Password: os.Getenv("TIDBOPS_MYSQL_PWD")}}
		for k, v := range map[string]string{"pd": *pd, "tidb": *tidb, "prom": *prom, "alert": *alert} {
			if v != "" {
				cfg.Endpoints[k] = v
			}
		}
		if !*noAuto {
			name, notes, err := source.Detect(context.Background(), &cfg, params.Cluster)
			if err != nil {
				log.Fatalf("%v", err)
			}
			params.Cluster = name
			for _, n := range notes {
				log.Print(n)
			}
		}
		if params.Cluster == "" {
			log.Fatal("需要集群名：请用 --cluster 指定，或去掉 --no-auto")
		}
		alertAddr = cfg.Endpoints["alert"]
		src = source.NewLive(cfg)
		log.Printf("直连模式（只读）：集群 %s，端点 %v", params.Cluster, keysOf(cfg.Endpoints))
	}
	src = source.NewCached(src, *cacheTTL)

	site, err := loadSiteConf(*configFile)
	if err != nil {
		log.Printf("警告：平台配置文件 %s 无效，已忽略：%v", *configFile, err)
	} else if site.ICP != "" {
		log.Printf("备案号：%s（来自 %s）", site.ICP, *configFile)
	}
	guard := expiry.New(log.Printf)
	guard.Startup()

	s := server.New(server.Config{Col: collect.New(src, params), Mode: mode, Web: *web, SnapTTL: *snapTTL, Fixtures: *fixtures, ICP: site.ICP, Unavailable: guard.Expired})
	var handler http.Handler = s.Handler()
	if *dataDir != "" {
		sh := &opsShared{}
		if err := enableOps(s, opsArgs{dir: *dataDir, env: *envName, mode: mode, execMode: *execMode, policyFile: *execPolicy, sudoUser: *execSudoUser, execBin: *execBin,
			keyFile: *keyFile, noMFA: *noMFA, devSeed: *devSeed, secure: *secureCookie, readonly: *readonlyFile, simDelay: *simDelay, tiup: *tiup,
			cluster: params.Cluster, alert: alertAddr, publicURL: *publicURL, notifyHosts: *notifyHosts, myHost: *myHost, myPort: *myPort}, sh); err != nil {
			log.Fatalf("启用运维模式失败: %v", err)
		}
		defer s.Close()
		hub := startHub(s, sh, hubArgs{dir: *dataDir, env: *envName, mode: mode, fixtures: *fixtures, fixturesRoot: *fixturesRoot, devSeed: *devSeed, cluster: params.Cluster, sudoUser: *execSudoUser, execBin: *execBin,
			pd: *pd, tidb: *tidb, prom: *prom, alert: alertAddr, myHost: *myHost, myPort: *myPort, myUser: *myUser, myBin: *myBin, tiup: *tiup, home: h, timeout: *timeout, cacheTTL: *cacheTTL, snapTTL: *snapTTL, noAuto: *noAuto, simDelay: *simDelay, standalone: *standalone, localTiup: *localTiup})
		defer hub.Close()
		handler = hub.Handler()
	} else {
		log.Printf("未指定 --data-dir：只读模式（无登录，仅 GET）")
	}

	if !*standalone { // 独立部署的平台实例没有数据源，各集群接入后在后台各自首次采集
		snap := s.Snapshot(context.Background(), true)
		log.Printf("首次采集完成：%d 个实例，%d 条诊断，%d 组慢 SQL；%d 项数据缺失", len(snap.Topology.Instances), len(snap.Findings), len(snap.Slow), len(snap.Errors))
		for _, e := range snap.Errors {
			log.Printf("  - %s", e)
		}
	}

	srv := &http.Server{Addr: *listen, Handler: guard.Wrap(handler), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("监听 http://%s", *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

type opsArgs struct {
	dir, env, mode, execMode, policyFile, sudoUser, execBin, keyFile string
	noMFA, devSeed, secure                                           bool
	readonly                                                         string
	simDelay                                                         time.Duration
	tiup, cluster, alert, myHost, publicURL, notifyHosts             string
	myPort                                                           int
}

// opsShared 是 enableOps 产出的、构造其他集群实例时要复用的东西（执行器策略、令牌密钥、审计日志）。
type opsShared struct {
	pol         xexec.Policy // 最终生效的主集群执行器策略（含默认值）
	polClusters []string     // 策略文件里显式列出的集群名（空 = 没有显式白名单）
	key         []byte
	em          string // 进程级执行器模式：sim | direct | sudo | none
	audit       *audit.Log
}

func enableOps(s *server.Server, a opsArgs, sh *opsShared) error {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	switch a.env {
	case "test", "staging", "prod":
	default:
		return fmt.Errorf("--env 只能是 test | staging | prod")
	}
	// 数据版本检查（加载用户库之前）：旧数据（无 VERSION）自动做非破坏性迁移；更新版本创建的数据拒绝启动
	from, err := dataver.EnsureCurrent(a.dir, selfVersion())
	if err != nil {
		return err
	}
	if from != dataver.Schema {
		log.Printf("数据目录已从 schema %d 自动迁移到 %d", from, dataver.Schema)
	}
	mk, err := readOrCreateKey(filepath.Join(a.dir, "master.key"))
	if err != nil {
		return err
	}
	users, err := authz.OpenStore(filepath.Join(a.dir, "users.json"), mk)
	if err != nil {
		return err
	}
	roles, err := authz.OpenRoleStore(filepath.Join(a.dir, "roles.json")) // 自定义角色库（必须先于任何角色判定打开）
	if err != nil {
		return err
	}
	al, err := audit.Open(filepath.Join(a.dir, "audit.log"))
	if err != nil {
		return err
	}
	if a.keyFile == "" {
		a.keyFile = filepath.Join(a.dir, "exec.key")
	}
	key, err := readOrCreateKey(a.keyFile)
	if err != nil {
		return err
	}
	if len(users.List()) == 0 {
		switch {
		case a.devSeed && a.mode == "replay":
			if err := authz.SeedDemoUsers(users); err != nil {
				return err
			}
			log.Printf("已创建演示账号（口令 %s）：admin admin2 dba sre approver approver2 viewer auditor dev", authz.DemoPassword)
		case a.devSeed:
			return fmt.Errorf("--dev-seed 只允许在回放模式使用")
		default:
			pw := randomPassword()
			if err := users.Add("admin", "平台管理员", pw, map[string][]authz.Role{"*": {authz.RoleAdmin}}, nil); err != nil {
				return err
			}
			log.Printf("首次启动：已创建管理员 admin，初始口令 %s（只显示这一次，请登录后立即修改，并用 tidbops-server user add 增加第二名管理员）", pw)
		}
	} else if a.devSeed && a.mode != "replay" {
		return fmt.Errorf("--dev-seed 只允许在回放模式使用")
	}

	pol, err := loadPolicy(a.policyFile)
	if err != nil {
		return err
	}
	sh.polClusters = append([]string(nil), pol.Clusters...)
	for _, c := range pol.Clusters {
		if c == xexec.AnyCluster { // 通配：没有显式的集群白名单，每个集群用自己的名字
			sh.polClusters = nil
		}
	}
	if len(pol.Clusters) == 0 {
		pol.Clusters = []string{a.cluster}
	}
	if pol.TiupBin == "" {
		pol.TiupBin = a.tiup
	}
	if pol.AlertURL == "" {
		pol.AlertURL = a.alert
	}
	if pol.MySQLHost == "" {
		pol.MySQLHost, pol.MySQLPort = a.myHost, a.myPort
	}
	if pol.TempDir == "" {
		pol.TempDir = filepath.Join(a.dir, "tmp")
	}
	if err := os.MkdirAll(pol.TempDir, 0o700); err != nil {
		return err
	}

	em := a.execMode
	if em == "" {
		em = "none"
		if a.mode == "replay" {
			em = "sim"
		}
	}
	cfg := server.OpsConfig{MasterKey: mk, PublicURL: a.publicURL, NotifyHosts: splitCSV(a.notifyHosts), BackupRoots: pol.BackupRoots, Dir: a.dir, Env: a.env, Users: users, Roles: roles, Audit: al, Key: key, ReadOnlyFile: a.readonly, SecureCookie: a.secure, NoMFA: a.noMFA, ExecMode: em, Demo: a.devSeed && a.mode == "replay"}
	switch em {
	case "sim":
		if pol.MySQLUser == "" {
			pol.MySQLUser = "tidbops_ops"
		}
		if len(pol.BackupRoots) == 0 {
			pol.BackupRoots = []string{"local:///data/backup/"}
		}
		if pol.AlertURL == "" {
			pol.AlertURL = "http://127.0.0.1:9093"
		}
		cfg.Sim = true
		cfg.WaitScale = 0.02
		cfg.Runner = &xexec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: key, Delay: a.simDelay}
		cfg.Prober = &change.SimProber{Polls: 2}
	case "direct":
		tf := xexec.DisplayTopo(osRun)
		cfg.Runner = xexec.DirectRunner{Pol: pol, Topo: tf, Key: key}
	case "sudo":
		bin := a.execBin
		if bin == "" {
			bin, _ = os.Executable()
		}
		cfg.Runner = xexec.SudoRunner{User: a.sudoUser, Bin: bin}
		cfg.ShellAgent = shell.SudoAgent{User: a.sudoUser, Bin: bin}
	case "none":
	default:
		return fmt.Errorf("--exec 只能是 sim | direct | sudo | none")
	}
	if em == "direct" || em == "sudo" {
		cfg.Prober = s.NewPDProber(cfg.Runner)
	}
	sh.pol, sh.key, sh.em, sh.audit = pol, key, em, al
	return s.EnableOps(cfg)
}

// selfVersion 返回本程序版本：发布构建用 ldflags 注入的 buildinfo.Version，开发构建（dev）回退到 server.Version。
func selfVersion() string {
	v := buildinfo.Version
	if v == "dev" {
		v = server.Version
	}
	return v
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func randomPassword() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "Tp-" + hex.EncodeToString(b)
}

func keysOf(m map[string]string) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return strings.Join(k, ",")
}

func splitCSV(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
