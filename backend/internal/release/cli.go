// @author: kevinkelly
// @license: Apache License 2.0
// @file: cli.go
// @time: 2026/10/28

package release

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"tidbops/internal/buildinfo"
	"tidbops/internal/dataver"
)

func hostOS() string   { return runtime.GOOS }
func hostArch() string { return runtime.GOARCH }

// executablePath 便于测试替换。
var executablePath = os.Executable

// PackageRootFromExe 由二进制路径推出发布包根目录：<包根>/bin/tidbops-server → 包根。
func PackageRootFromExe(exe string) (string, error) {
	p, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(p))
	if _, err := os.Stat(filepath.Join(root, ManifestName)); err != nil {
		return "", fmt.Errorf("%s 不在发布包内（没有找到 %s）：请在解压后的发布包里运行 ./bin/tidbops-server，或用 --package DIR 指定包根目录", exe, filepath.Join(root, ManifestName))
	}
	return root, nil
}

func resolvePackage(flagVal string) (string, error) {
	if flagVal != "" {
		return filepath.Abs(flagVal)
	}
	exe, err := executablePath()
	if err != nil {
		return "", err
	}
	return PackageRootFromExe(exe)
}

// SelfVersion 返回本程序的版本：优先用构建时注入的版本；开发构建（dev）则尝试读取所在发布包的清单，最后用 fallback。
func SelfVersion(fallback string) string {
	if buildinfo.Version != "dev" && buildinfo.Version != "" {
		return buildinfo.Version
	}
	if exe, err := executablePath(); err == nil {
		if root, err := PackageRootFromExe(exe); err == nil {
			if m, err := ReadManifest(root); err == nil {
				return m.Version
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "dev"
}

const adminUsage = `用法: tidbops-server admin <子命令> [参数]

子命令：
  install    首次部署（在解压后的发布包内运行；需要 root）
  upgrade    一键升级（自动备份、数据迁移、健康检查，失败自动回滚；需要 root）
  rollback   一键回滚到上一个版本（需要 root）
  status     查看当前版本、服务状态、数据 schema、升级历史、备份（不需要 root）
  migrate    迁移数据目录到本程序的 schema（幂等；升级流程内部调用，也可手动执行）
  backup     手动备份数据目录（需要 root）
  restore    从备份恢复数据目录（需要 root）
  uninstall  卸载服务（默认保留数据、备份与 releases；需要 root）

每个子命令用 -h 查看参数。发布包根目录默认取自 <包根>/bin/tidbops-server 的上两级，也可用 --package 指定。
`

// Main 是 tidbops-server admin 的入口，args 不含 "admin"。返回进程退出码。
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, args, stdout, stderr, nil)
}

// Run 与 Main 相同，但可注入 Engine 配置（测试用）：mk 为 nil 时用真实系统。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, mk func(root string) *Engine) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, adminUsage)
		return 0
	}
	fs := flag.NewFlagSet("admin "+cmd, flag.ContinueOnError)
	fs.SetOutput(stdout)
	pkg := fs.String("package", "", "发布包根目录（默认取自本二进制所在包：<包根>/bin/tidbops-server 的上两级）")
	root := fs.String("root", "", "目标根目录（默认 /；仅用于演练 / 测试，生产不要设置）")

	var run func(e *Engine) error
	switch cmd {
	case "install":
		var o InstallOpts
		fs.BoolVar(&o.Standalone, "standalone", false, "独立部署：平台装在任意机器上，不依赖本机 tiup / tidb 用户、不预设集群（与 --cluster 互斥）；集群之后在页面「集群接入」里添加（首期只读接入）")
		fs.BoolVar(&o.LocalTiup, "local-tiup", false, "中控机模式（隐含 --standalone）：平台装在 tiup 中控机上、不预设集群；管理员可在页面一键导入本机 tiup 管理的集群并做全部运维，手工添加的远程集群仍只读（需要 tidb 用户与 sudo）")
		fs.StringVar(&o.Cluster, "cluster", "", "tiup 集群名（必填，字母数字与 _-，最长 64；--standalone 时不填）")
		fs.StringVar(&o.Env, "env", "prod", "本集群所属环境：test | staging | prod")
		fs.StringVar(&o.TidbUser, "tidb-user", "tidb", "tiup 部署用户（受限 exec 入口以该用户身份运行）")
		fs.StringVar(&o.Listen, "listen", "127.0.0.1:8080", "Web 服务监听地址")
		fs.StringVar(&o.PublicURL, "public-url", "", "平台对外访问地址（通知消息里的链接用），如 https://ops.example.com")
		fs.BoolVar(&o.DryRun, "dry-run", false, "只打印将做的每一步，不改动任何东西")
		fs.BoolVar(&o.Start, "start", false, "安装后启动服务并做健康检查")
		fs.BoolVar(&o.Force, "force", false, "已安装时重装当前版本（不覆盖已有密钥与配置文件）")
		run = func(e *Engine) error {
			p, err := resolvePackage(*pkg)
			if err != nil {
				return err
			}
			o.Package = p
			return e.Install(ctx, o)
		}
	case "upgrade":
		var o UpgradeOpts
		fs.BoolVar(&o.DryRun, "dry-run", false, "只打印计划与预检结果，不改动任何东西")
		fs.BoolVar(&o.Force, "force", false, "忽略「有执行中的变更」（该变更会被标记为中断）；允许重新应用同版本")
		fs.BoolVar(&o.AllowDowngrade, "allow-downgrade", false, "允许版本降级（通常应使用 admin rollback）")
		fs.BoolVar(&o.NoBackup, "no-backup", false, "跳过数据备份（危险：迁移失败时无法恢复数据）")
		fs.IntVar(&o.Keep, "keep", 3, "保留最近几个 release（current 与上一个版本永不删除）")
		fs.DurationVar(&o.Timeout, "timeout", 60*time.Second, "健康检查总超时")
		run = func(e *Engine) error {
			p, err := resolvePackage(*pkg)
			if err != nil {
				return err
			}
			o.Package = p
			return e.Upgrade(ctx, o)
		}
	case "rollback":
		var o RollbackOpts
		fs.StringVar(&o.To, "to", "", "回到指定版本（默认上一个版本）")
		fs.BoolVar(&o.RestoreData, "restore-data", false, "强制恢复升级前的数据备份（会丢失升级之后产生的审计 / 变更记录）")
		fs.BoolVar(&o.NoRestoreData, "no-restore-data", false, "只切换二进制，不恢复数据")
		fs.StringVar(&o.BackupFile, "backup", "", "指定要恢复的备份文件（配合 --restore-data）")
		fs.BoolVar(&o.DryRun, "dry-run", false, "只打印回滚计划")
		run = func(e *Engine) error { return e.Rollback(ctx, o) }
	case "status":
		asJSON := fs.Bool("json", false, "以 JSON 输出")
		run = func(e *Engine) error {
			s, err := e.Status(ctx)
			if err != nil {
				return err
			}
			return e.PrintStatus(s, *asJSON)
		}
	case "migrate":
		dir := fs.String("data-dir", "", "数据目录（必填）")
		run = func(e *Engine) error {
			if *dir == "" {
				return errUsage("需要 --data-dir")
			}
			return runMigrate(*dir, SelfVersion(""), stdout)
		}
	case "backup":
		out := fs.String("out", "", "备份文件路径（默认写到 /var/backups/tidbops/<时间>-<版本>-manual.tar.gz）")
		run = func(e *Engine) error { return e.Backup(ctx, *out) }
	case "restore":
		file := fs.String("file", "", "备份文件（必填）")
		run = func(e *Engine) error { return e.Restore(ctx, *file) }
	case "uninstall":
		var o UninstallOpts
		fs.BoolVar(&o.PurgeData, "purge-data", false, "同时删除数据目录与全部备份（需再加 --yes）")
		fs.BoolVar(&o.Yes, "yes", false, "确认永久删除（配合 --purge-data）")
		fs.BoolVar(&o.DryRun, "dry-run", false, "只打印将做的步骤")
		run = func(e *Engine) error { return e.Uninstall(ctx, o) }
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n\n%s", cmd, adminUsage)
		return 2
	}
	fs.Usage = func() {
		fmt.Fprintf(stdout, "用法: tidbops-server admin %s [参数]\n\n参数：\n", cmd)
		fs.VisitAll(func(f *flag.Flag) {
			typ, usage := flag.UnquoteUsage(f)
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = "（默认 " + f.DefValue + "）"
			}
			fmt.Fprintf(stdout, "  --%s %s\n        %s%s\n", f.Name, typ, usage, def)
		})
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "多余的参数：%v\n", fs.Args())
		return 2
	}

	var e *Engine
	if mk != nil {
		e = mk(*root)
	} else {
		e = NewEngine(Layout{Root: *root}, stdout)
	}
	e.Out = stdout
	e.Operator = operatorName()
	e.defaults()
	if err := run(e); err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "错误:", err)
			return 2
		}
		fmt.Fprintln(stderr, "错误:", err)
		return 1
	}
	return 0
}

type usageError string

func (u usageError) Error() string { return string(u) }
func errUsage(s string) error      { return usageError(s) }

func operatorName() string {
	for _, k := range []string{"SUDO_USER", "USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// runMigrate 执行数据迁移：数据 schema 高于本程序支持时返回错误（退出码非 0）。
func runMigrate(dir, version string, out io.Writer) error {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("数据目录 %s 不存在或不是目录", dir)
	}
	from, to, err := dataver.Migrate(dir, version, func(s string) { fmt.Fprintln(out, s) })
	if err != nil {
		return err
	}
	if from == to {
		fmt.Fprintf(out, "数据 schema %d，已是最新\n", to)
	} else {
		fmt.Fprintf(out, "数据 schema %d → %d 迁移完成\n", from, to)
	}
	return nil
}
