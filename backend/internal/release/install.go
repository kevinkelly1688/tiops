// @author: kevinkelly
// @license: Apache License 2.0
// @file: install.go
// @time: 2026/10/28

package release

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tidbops/internal/dataver"
)

// InstallOpts 是 admin install 的参数。
type InstallOpts struct {
	Package   string // 发布包根目录
	Env       string // test | staging | prod，默认 prod
	Cluster   string // 必填（独立部署时不填）
	TidbUser  string // 默认 tidb（独立部署时不用）
	Listen    string // 默认 127.0.0.1:8080
	PublicURL string
	DryRun    bool
	Start     bool
	Force     bool // 已安装时重装当前版本（不覆盖密钥与配置文件）
	// Standalone 独立部署：平台装在任意机器上，不依赖本机 tiup、tidb 用户或任何集群；
	// 不建 sudoers / exec.json / exec.key，集群由管理员登录后在「集群接入」页添加（首期只读接入）。
	Standalone bool
	// LocalTiup 中控机模式（隐含 Standalone）：平台装在 tiup 中控机上，仍不预设集群；
	// 保留 sudoers / exec.json（集群名通配 "*"）/ exec.key，管理员可在「集群接入」页一键导入本机 tiup 管理的集群并做全部运维操作，
	// 手工添加的远程集群仍然只读。
	LocalTiup bool
}

func (o *InstallOpts) fillDefaults() {
	if o.Env == "" {
		o.Env = "prod"
	}
	if o.TidbUser == "" {
		o.TidbUser = "tidb"
	}
	if o.Listen == "" {
		o.Listen = "127.0.0.1:8080"
	}
}

// Install 首次部署。幂等：重复运行不会覆盖已有的密钥与配置文件。
func (e *Engine) Install(ctx context.Context, o InstallOpts) error {
	e.defaults()
	o.fillDefaults()
	if o.LocalTiup {
		o.Standalone = true
	}
	switch {
	case o.Standalone && o.Cluster != "":
		return errors.New("--standalone 与 --cluster 不能同时使用：独立部署的平台不代表任何集群，集群在页面「集群接入」里添加")
	case !o.Standalone && o.Cluster == "":
		return errors.New("需要 --cluster（tiup 集群名）；平台独立部署（不在 tiup 中控机上、不预设集群）请改用 --standalone")
	}
	params := UnitParams{Env: o.Env, Cluster: o.Cluster, TidbUser: o.TidbUser, Listen: o.Listen, PublicURL: o.PublicURL, Standalone: o.Standalone, LocalTiup: o.LocalTiup}
	if err := params.Validate(); err != nil {
		return err
	}
	if o.Package == "" {
		return errors.New("未指定发布包目录（--package）")
	}
	if !o.DryRun {
		if err := e.requireRoot(); err != nil {
			return err
		}
	}
	m, err := e.loadPackage(o.Package)
	if err != nil {
		return err
	}
	e.printf("发布包：tidbops %s（%s/%s，数据 schema %d），校验通过\n", m.Version, m.OS, m.Arch, m.SchemaVersion)

	if cur, cerr := e.currentVersion(); cerr == nil {
		if !o.Force {
			return fmt.Errorf("已安装版本 %s：升级请用 admin upgrade；要重装当前版本请加 --force（不会覆盖密钥与配置文件）", cur)
		}
		if !sameVersion(cur, m.Version) {
			return fmt.Errorf("已安装版本 %s，与发布包版本 %s 不同：--force 只用于重装当前版本，升级请用 admin upgrade", cur, m.Version)
		}
	} else if !errors.Is(cerr, fs.ErrNotExist) {
		return cerr
	}

	// 先决条件（dry-run 只告警）
	pre := func(err error) error {
		if err == nil {
			return nil
		}
		if o.DryRun {
			e.printf("[dry-run] 警告：%v\n", err)
			return nil
		}
		return err
	}
	if params.UsesExec() {
		if _, err := e.Sys.Run(ctx, "id", o.TidbUser); err != nil {
			if perr := pre(fmt.Errorf("用户 %s 不存在（tiup 部署用户）", o.TidbUser)); perr != nil {
				return perr
			}
		}
		if _, err := e.Sys.LookPath("visudo"); err != nil {
			if perr := pre(errors.New("缺少 visudo（请安装 sudo）")); perr != nil {
				return perr
			}
		}
	}

	if !o.DryRun {
		unlock, err := e.lock()
		if err != nil {
			return err
		}
		defer unlock()
	}

	pkgRead := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(o.Package, filepath.FromSlash(rel)))
	}
	var jsonCreated bool
	var adminCreds []adminCred // 安装器创建的初始管理员账号与口令；adminNote 非空 = 没有取到口令的原因 / 说明
	var adminNote string
	steps := []step{
		{"创建运行账号 " + RunUser, func() error {
			if _, err := e.Sys.Run(ctx, "id", RunUser); err == nil {
				e.printf("  账号 %s 已存在，跳过\n", RunUser)
				return nil
			}
			_, err := e.Sys.Run(ctx, "useradd", "--system", "--home-dir", DataDir, "--shell", "/sbin/nologin", RunUser)
			return err
		}},
		{"创建目录并设置权限", func() error {
			for _, d := range []struct {
				p    string
				perm os.FileMode
			}{{e.L.Opt(), 0o755}, {e.L.Releases(), 0o755}, {e.L.Etc(), 0o755}, {e.L.Backups(), 0o700}, {e.L.Data(), 0o700}} {
				if err := os.MkdirAll(d.p, d.perm); err != nil {
					return err
				}
				if err := os.Chmod(d.p, d.perm); err != nil {
					return err
				}
			}
			return e.Sys.Chown(e.L.Data(), RunUser, RunUser, false)
		}},
		{"安装 release " + m.Version, func() error {
			reused, err := e.installRelease(o.Package, m, o.Force)
			if err != nil {
				return err
			}
			if reused {
				e.printf("  %s 已存在且校验一致，复用\n", e.L.ReleaseDir(m.Version))
			}
			return nil
		}},
		{"切换 current 链接", func() error { return e.switchCurrent(m.Version) }},
	}
	// 以下是与「本机 tiup + 受限 sudo 执行器」相关的步骤：独立部署不需要（没有 tidb 用户、sudoers、exec.json、exec.key）
	if params.UsesExec() {
		steps = append(steps, e.execSteps(ctx, o, pkgRead, &jsonCreated)...)
	}
	steps = append(steps, []step{
		{"systemd 单元 tidbops.service", func() error {
			ub, err := pkgRead(pkgUnit)
			if err != nil {
				return err
			}
			u, err := RenderUnit(string(ub), params)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(e.L.Unit()), 0o755); err != nil {
				return err
			}
			return writeFileAtomic(e.L.Unit(), []byte(u), 0o644)
		}},
		{"数据目录 VERSION 文件", func() error {
			if _, err := dataver.Read(e.L.Data()); err == nil {
				e.printf("  保留已有 VERSION\n")
			} else if e.dataHasFiles() {
				e.printf("  数据目录已有文件但没有 VERSION（旧版本创建的数据，视为 schema 0）：服务首次启动时会自动迁移（非破坏性）\n")
			} else {
				now := e.Sys.Now()
				if err := dataver.Write(e.L.Data(), dataver.Info{Schema: m.SchemaVersion, Version: m.Version, InstalledAt: &now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return e.Sys.Chown(e.L.Data(), RunUser, RunUser, true)
		}},
		{"创建两名管理员 admin / admin2 并生成初始口令（数据目录里已有用户则跳过）", func() error {
			adminCreds, adminNote = e.createAdmins(ctx)
			return nil // 失败不中止安装：服务首次启动时仍会创建 admin，口令写在日志里（adminNote 会说明）
		}},
		{"systemctl daemon-reload", func() error {
			_, err := e.Sys.Run(ctx, "systemctl", "daemon-reload")
			return err
		}},
	}...)
	if o.Start {
		steps = append(steps, step{"启动服务并做健康检查", func() error {
			if _, err := e.Sys.Run(ctx, "systemctl", "enable", "--now", ServiceName); err != nil {
				return err
			}
			return e.waitHealthy(ctx, o.Listen, m.Version)
		}})
	}

	switch {
	case o.LocalTiup:
		e.printf("将部署 tidbops %s（中控机模式：不预设集群，可一键导入本机 tiup 管理的集群），环境 %s，tidb 用户 %s，监听 %s\n", m.Version, o.Env, o.TidbUser, o.Listen)
	case o.Standalone:
		e.printf("将部署 tidbops %s（独立部署，不预设集群），环境 %s，监听 %s\n", m.Version, o.Env, o.Listen)
	default:
		e.printf("将部署 tidbops %s：集群 %s，环境 %s，tidb 用户 %s，监听 %s\n", m.Version, o.Cluster, o.Env, o.TidbUser, o.Listen)
	}
	if err := e.runSteps(o.DryRun, steps); err != nil {
		e.record(HistoryEntry{Action: "install", To: m.Version, SchemaTo: m.SchemaVersion, Error: err.Error()})
		return err
	}
	if o.DryRun {
		e.printf("[dry-run] 以上步骤均未执行，系统未做任何改动\n")
		e.printAccessSummary(o, m.Version, false, nil, "dry-run：不创建账号，实际安装时在这里打印两名管理员（admin、admin2）的账号和初始口令")
		e.printNextSteps(o, false)
		return nil
	}
	if jsonCreated {
		e.printf("已生成 %s，请按实际环境修改 tiupBin / mysql 运维账号 / backupRoots / alertURL\n", e.L.ExecJSON())
	}
	sch, _ := e.currentSchema()
	e.record(HistoryEntry{Time: time.Time{}, Action: "install", To: m.Version, OK: true, SchemaFrom: sch, SchemaTo: sch})
	e.printf("\n安装完成：tidbops %s\n", m.Version)
	e.printAccessSummary(o, m.Version, o.Start, adminCreds, adminNote)
	e.printNextSteps(o, o.Start)
	return nil
}

// adminCred 是安装器创建的一个初始管理员账号。
type adminCred struct{ Name, PW string }

// initialAdmins 是安装时默认创建的管理员：授予管理员、开启写操作开关都需要两名不同的管理员，所以默认建两个；
// 页面里「授予管理员」要另一名管理员确认，只有一名管理员时靠页面建不出第二名。
var initialAdmins = []struct{ Name, Display string }{{"admin", "平台管理员"}, {"admin2", "平台管理员2"}}

// createAdmins 以运行账号 tidbops 调用 `user add --gen-password` 依次创建 admin、admin2 两名管理员，返回账号与生成的口令。
// 数据目录里已有 users.json 则不动（重装 / 升级后的数据保留）；取不到口令时 note 说明原因。口令不经过命令行参数。
// 第一个（admin）失败就不再建第二个：用户库仍是空的，服务首次启动会自己创建 admin（口令只在日志里）；
// admin 成功而 admin2 失败时，只提示 admin2 要手工补建。
func (e *Engine) createAdmins(ctx context.Context) (creds []adminCred, note string) {
	if _, err := os.Lstat(filepath.Join(e.L.Data(), "users.json")); err == nil {
		return nil, "数据目录里已有用户，沿用原来的账号与口令（忘记口令：先停服务，再 sudo -u " + RunUser + " env TIDBOPS_USER_PASSWORD='新口令' " + CurrentBin + " user passwd --data-dir " + DataDir + " --name admin）"
	}
	for i, ad := range initialAdmins {
		// 命令串全部由固定常量组成（没有任何外部输入）；账号 tidbops 的登录 shell 是 nologin，所以用 -s 指定 /bin/sh。
		cmd := e.L.P(CurrentBin) + " user add --data-dir " + e.L.Data() + " --name " + ad.Name + " --display " + ad.Display + " --role admin --gen-password"
		out, err := e.Sys.Run(ctx, "runuser", "-s", "/bin/sh", RunUser, "-c", cmd)
		pw := ""
		for _, ln := range strings.Split(string(out), "\n") {
			if p, ok := strings.CutPrefix(strings.TrimSpace(ln), AdminPasswordPrefix); ok && p != "" {
				pw = p
				break
			}
		}
		if pw != "" {
			creds = append(creds, adminCred{ad.Name, pw})
			continue
		}
		reason := "没有取到口令"
		if err != nil {
			reason = err.Error()
		}
		if i == 0 {
			return nil, "自动创建管理员失败（" + reason + "）：服务首次启动时仍会创建 admin，初始口令只在日志里显示一次：journalctl -u " + ServiceName + " | grep 初始口令；第二名管理员请按「下一步」里的命令停服务后创建"
		}
		return creds, "管理员 " + ad.Name + " 创建失败（" + reason + "）：请按「下一步」里的命令停服务后手工创建第二名管理员"
	}
	return creds, ""
}

// accessURLs 由 --listen 得到可访问的地址列表：监听全部网卡时列出本机的非回环 IPv4。
func accessURLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{"http://" + listen}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	var out []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				out = append(out, "http://"+net.JoinHostPort(ipn.IP.String(), port))
			}
		}
	}
	return append(out, "http://"+net.JoinHostPort("127.0.0.1", port))
}

// printAccessSummary 在安装结束时打印：访问地址与端口、账号、初始口令、各目录的绝对路径。
func (e *Engine) printAccessSummary(o InstallOpts, version string, started bool, creds []adminCred, note string) {
	listen := o.Listen
	_, port, _ := net.SplitHostPort(listen)
	urls := accessURLs(listen)
	e.printf("\n==================== 访问信息 ====================\n")
	e.printf("访问地址：%s\n", strings.Join(urls, "\n          "))
	e.printf("监听端口：%s（监听 %s）\n", port, listen)
	if host, _, err := net.SplitHostPort(listen); err == nil && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		e.printf("          只监听本机：远程访问请放在反向代理（TLS + 访问控制）之后，或用 SSH 隧道：ssh -L %s:127.0.0.1:%s <本机>\n", port, port)
	}
	if o.PublicURL != "" {
		e.printf("对外地址：%s（--public-url，通知消息里的链接用）\n", o.PublicURL)
	}
	if !started {
		e.printf("服务状态：尚未启动（systemctl enable --now %s）\n", ServiceName)
	}
	if len(creds) == 0 {
		e.printf("账号：admin\n")
		e.printf("密码：（未显示）%s\n", note)
	} else {
		for _, c := range creds {
			e.printf("账号：%s\n密码：%s\n", c.Name, c.PW)
		}
		e.printf("      （默认建了两名管理员：写操作开关、授予管理员都需要两名不同的管理员确认。以上是初始口令，只显示这一次；请登录后各自立即修改口令）\n")
		if note != "" {
			e.printf("      注意：%s\n", note)
		}
	}
	e.printf("部署目录（绝对路径）：\n")
	for _, kv := range [][2]string{
		{"安装根目录", e.L.Opt()},
		{"当前版本", e.L.ReleaseDir(version) + "（" + e.L.Current() + " 指向它）"},
		{"程序", e.L.P(CurrentBin)},
		{"前端文件", e.L.P(CurrentWeb)},
		{"数据目录", e.L.Data() + "（用户 / 审计 / 集群接入等数据，含 master.key；升级前会自动备份）"},
		{"配置目录", e.L.Etc()},
		{"备份目录", e.L.Backups()},
		{"systemd 单元", e.L.Unit()},
	} {
		e.printf("  %s：%s\n", kv[0], kv[1])
	}
	e.printf("==================================================\n")
}

// execSteps 是安装「本机 tiup + 受限 sudo 执行器」所需的步骤（令牌密钥、exec.json、口令文件、sudoers）。
func (e *Engine) execSteps(ctx context.Context, o InstallOpts, pkgRead func(string) ([]byte, error), jsonCreated *bool) []step {
	return []step{
		{"令牌密钥 exec.key（属主 " + RunUser + "、组 " + o.TidbUser + "、0640；已存在则保留）", func() error {
			if _, err := os.Lstat(e.L.ExecKey()); err == nil {
				e.printf("  保留已有 %s\n", e.L.ExecKey())
				return nil
			}
			k := make([]byte, 32)
			if _, err := io.ReadFull(e.Rand, k); err != nil {
				return err
			}
			return e.writeOwned(e.L.ExecKey(), []byte(hex.EncodeToString(k)+"\n"), 0o640, RunUser, o.TidbUser)
		}},
		{"执行器策略 exec.json（由包内样例生成；已存在则保留）", func() error {
			if _, err := os.Lstat(e.L.ExecJSON()); err == nil {
				e.printf("  保留已有 %s\n", e.L.ExecJSON())
				e.warnTempDir()
				return nil
			}
			ex, err := pkgRead(pkgExecJSON)
			if err != nil {
				return err
			}
			var b []byte
			if o.LocalTiup {
				b, err = RenderExecJSONLocal(string(ex))
			} else {
				b, err = RenderExecJSON(string(ex), o.Cluster)
			}
			if err != nil {
				return err
			}
			*jsonCreated = true
			return e.writeOwned(e.L.ExecJSON(), b, 0o644, "root", "root")
		}},
		{"执行器临时目录 " + ExecTempDir + "（属主 " + o.TidbUser + "、0700；exec.json 的 tempDir 默认指向它）", func() error {
			d := e.L.ExecTemp()
			if err := os.MkdirAll(d, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(d, 0o700); err != nil {
				return err
			}
			return e.Sys.Chown(d, o.TidbUser, o.TidbUser, false)
		}},
		{"systemd drop-in " + DropInFile + "（放开沙箱里 tiup / 执行器要写的路径；已存在则保留）", func() error {
			if err := os.MkdirAll(filepath.Dir(e.L.DropIn()), 0o755); err != nil {
				return err
			}
			return e.createIfAbsent(e.L.DropIn(), RenderDropIn(o.TidbUser), 0o644, "root", "root")
		}},
		{"运维账号口令文件 exec.env（root:" + o.TidbUser + " 0640；已存在则保留）", func() error {
			return e.createIfAbsent(e.L.ExecEnv(), "# 运维账号口令（仅 exec 入口读取；sudo 会清空环境）\n# TIDBOPS_OPS_PWD=...\n", 0o640, "root", o.TidbUser)
		}},
		{"只读采集账号口令文件 env（root:" + RunUser + " 0640；已存在则保留）", func() error {
			return e.createIfAbsent(e.L.Env(), "# 只读采集账号口令（Web 进程）\n# TIDBOPS_MYSQL_PWD=...\n", 0o640, "root", RunUser)
		}},
		{"sudoers（只放行受限 exec 入口；visudo 校验后以 0440 安装）", func() error {
			sb, err := pkgRead(pkgSudoers)
			if err != nil {
				return err
			}
			s, err := RenderSudoers(string(sb), o.TidbUser)
			if err != nil {
				return err
			}
			return e.installSudoers(ctx, s)
		}},
	}
}

// ensureExecRuntime 补齐「本机 tiup + 受限 sudo 执行器」在 systemd 沙箱里运行所需的两样东西：执行器临时目录、放开沙箱可写路径的 drop-in。
// 新装时由 execSteps 完成；旧版安装器装的机器没有它们，升级时在这里补上——否则 tiup 写不了 ~tidb/.tiup（沙箱里 /home 只读），
// 导入集群时 tiup cluster list 会以退出码 1 失败。已存在的一律保留（可能被用户改过）；exec.json 不动，tempDir 不合适只提示。
// 必须在启动服务（daemon-reload）之前调用。
func (e *Engine) ensureExecRuntime(p UnitParams) error {
	d := e.L.ExecTemp()
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
		if err := e.Sys.Chown(d, p.TidbUser, p.TidbUser, false); err != nil {
			return err
		}
		e.printf("  已建执行器临时目录 %s（属主 %s、0700）\n", d, p.TidbUser)
	}
	if _, err := os.Lstat(e.L.DropIn()); err != nil {
		if err := os.MkdirAll(filepath.Dir(e.L.DropIn()), 0o755); err != nil {
			return err
		}
		if err := e.writeOwned(e.L.DropIn(), []byte(RenderDropIn(p.TidbUser)), 0o644, "root", "root"); err != nil {
			return err
		}
		e.printf("  已写入 systemd drop-in %s（放开沙箱里 ~%s/.tiup 与执行器临时目录的可写权限）\n", e.L.DropIn(), p.TidbUser)
	}
	if _, err := os.Lstat(e.L.ExecJSON()); err == nil {
		e.warnTempDir()
	}
	return nil
}

// warnTempDir 在保留已有 exec.json 时提示：tempDir 为空或落在 /tmp 下，执行器看不到服务的私有 /tmp（PrivateTmp=yes），扩容 / 改配置等会失败。
func (e *Engine) warnTempDir() {
	b, err := os.ReadFile(e.L.ExecJSON())
	if err != nil {
		return
	}
	var m struct {
		TempDir string `json:"tempDir"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	if m.TempDir == "" || m.TempDir == "/tmp" || strings.HasPrefix(m.TempDir, "/tmp/") || strings.HasPrefix(m.TempDir, "/var/tmp/") {
		e.printf("  ⚠️ exec.json 的 tempDir 是 %q：服务带 PrivateTmp=yes，执行器看不到它。请改为 %s（已为你建好该目录和 systemd drop-in），然后 systemctl restart %s\n", m.TempDir, ExecTempDir, ServiceName)
	}
}

func (e *Engine) createIfAbsent(p, content string, perm os.FileMode, user, group string) error {
	if _, err := os.Lstat(p); err == nil {
		e.printf("  保留已有 %s\n", p)
		return nil
	}
	return e.writeOwned(p, []byte(content), perm, user, group)
}

func (e *Engine) printNextSteps(o InstallOpts, started bool) {
	listenHost := o.Listen
	if o.LocalTiup {
		e.printf(`
下一步（中控机模式：平台不预设集群；本机 tiup 管理的集群由管理员在页面一键导入，之后可做全部运维操作）：
 1. 启动服务：systemctl enable --now %[1]s（安装时加了 --start 则已启动）。管理员 admin 与初始口令见上面的「访问信息」。
 2. 先在本机验证受限 exec 入口与 tiup（两条都应能无交互运行，不应出现密码提示）：
      echo '{"cmd":{"kind":"cluster.list"}}' | sudo -u %[1]s sudo -n -u %[6]s %[2]s exec --stdin
      sudo -u %[6]s tiup cluster list
    密钥文件不用交给平台：tiup 用 %[6]s 用户自己的 ~/.tiup/storage/cluster/clusters/<集群>/ssh/id_rsa 连各节点（属主 %[6]s、权限 0600、不带口令短语）。
 3. 用管理员登录，到「平台配置 → 集群接入」点「从本机 tiup 导入」，勾选集群、选环境、导入。导入的集群可做全部运维；手工添加的远程集群只读。
 4. 在集群里建两个数据库账号：只读账号（慢 SQL / 配置采集；导入时填，口令加密保存）与运维账号（仅 SET GLOBAL / SET CONFIG，口令写进 %[4]s/exec.env）。
 5. 编辑 %[4]s/exec.json（tiupBin / mysql 运维账号 / backupRoots）。clusters 为 ["*"]：本机 tiup 管理的任何集群都可被导入；要收紧请改成明确的集群名列表。
 6. 两名管理员 admin / admin2 已由安装器创建（口令见上面的「访问信息」，登录后各自立即修改）。只有数据目录里已有用户、或上面提示创建失败时，才需要手工补建第二名管理员（高风险操作需要两人）。
    页面里「授予管理员」本身也要另一名管理员确认，所以只有一名管理员时必须用命令行创建；
    服务运行时不要改用户库（会被内存里的版本覆盖），先停服务、建完再启动：
      systemctl stop %[1]s
      sudo -u %[1]s env TIDBOPS_USER_PASSWORD='强口令' %[2]s user add --data-dir %[3]s --name bob --role admin
      systemctl start %[1]s
    之后新增其他用户在「平台配置 → 用户管理」里创建；开启某个集群的写操作要两名管理员在「权限与审计 → 写操作开关」依次确认。
 7. 访问 %[5]s：默认只监听本机，可用 SSH 隧道或放在带 TLS 与访问控制的反向代理之后。
`, RunUser, CurrentBin, DataDir, EtcDir, listenHost, o.TidbUser)
		e.printf("紧急只读：touch %s/READONLY\n以后升级：解压新发布包，在包内执行 sudo ./upgrade.sh；回滚：sudo %s admin rollback\n", EtcDir, CurrentBin)
		return
	}
	if o.Standalone {
		e.printf(`
下一步（独立部署：平台不代表任何集群，也不需要 tiup / tidb 用户）：
 1. 启动服务：systemctl enable --now %[1]s（安装时加了 --start 则已启动）。管理员 admin 与初始口令见上面的「访问信息」。
 2. 两名管理员 admin / admin2 已由安装器创建（口令见上面的「访问信息」）。只有数据目录里已有用户、或上面提示创建失败时，才需要手工补建第二名管理员
    （高风险操作需要两人；页面里授予管理员也要另一名管理员确认，所以第一名额外的管理员必须用命令行）。
    服务运行时不要改用户库（会被内存里的版本覆盖），先 systemctl stop %[1]s，建完再 systemctl start %[1]s；口令从环境变量读取：
      sudo -u %[2]s env TIDBOPS_USER_PASSWORD='强口令' %[3]s user add --data-dir %[4]s --name bob --role admin
 3. 访问 %[5]s：默认只监听本机，可用 SSH 隧道（ssh -L 8080:127.0.0.1:8080 <本机>）或放在带 TLS 与访问控制的反向代理之后。
 4. 用管理员登录，到「平台配置 → 集群接入」添加已有集群：PD 地址必填，建议同时填 Prometheus / Alertmanager / TiDB status 与只读 SQL 账号（口令加密保存）。
    首期为只读接入：总览、拓扑、慢 SQL、热点、告警、诊断可用；扩缩容 / 配置变更 / 重启等写操作需要在集群的中控机上执行。
 5. 本机需要装 mysql 客户端（采集慢 SQL 与 cluster_info 用）；平台所在机器到各集群的 PD / Prometheus / TiDB 端口要通。
`, ServiceName, RunUser, CurrentBin, DataDir, listenHost)
		e.printf("紧急只读：touch %s/READONLY\n以后升级：解压新发布包，在包内执行 sudo ./upgrade.sh；回滚：sudo %s admin rollback\n", EtcDir, CurrentBin)
		return
	}
	e.printf(`
下一步（依次执行）：
 1. 两名管理员 admin / admin2 已由安装器创建（口令见上面的「访问信息」）；还要增加管理员（写操作开关需要两人确认）时，口令从环境变量读取：
      sudo -u %[1]s env TIDBOPS_USER_PASSWORD='强口令1' %[2]s user add --data-dir %[3]s --name alice --role admin
      sudo -u %[1]s env TIDBOPS_USER_PASSWORD='强口令2' %[2]s user add --data-dir %[3]s --name bob --role admin
    再按需创建 dba / sre / approver / auditor / developer（--role dba 等；--env 可限定环境）。
 2. 在集群里建两个数据库账号：只读账号（慢 SQL / 配置采集，口令写进 %[4]s/env）与运维账号（仅 SET GLOBAL / SET CONFIG，口令写进 %[4]s/exec.env）。
 3. 编辑 %[4]s/exec.json（tiupBin / mysql 运维账号 / backupRoots / alertURL）。
 4. 验证 sudo 入口：echo '{}' | sudo -u %[1]s sudo -n -u %[5]s %[2]s exec --stdin   （应返回校验失败的 JSON，而不是 sudo 报错）
`, RunUser, CurrentBin, DataDir, EtcDir, o.TidbUser)
	if started {
		e.printf(" 5. 服务已启动并通过健康检查；反向代理（TLS + 访问控制）指向 %s。\n", listenHost)
	} else {
		e.printf(" 5. 启动：systemctl enable --now %s ；反向代理（TLS + 访问控制）指向 %s。\n", ServiceName, listenHost)
	}
	e.printf(` 6. 登录后到「权限与审计 → 部署自检」确认各项为绿色；由两名管理员依次确认「开启写操作」。
紧急只读：touch %s/READONLY
以后升级：解压新发布包，在包内执行 sudo ./bin/tidbops-server admin upgrade；回滚：sudo %s admin rollback
`, EtcDir, CurrentBin)
}
