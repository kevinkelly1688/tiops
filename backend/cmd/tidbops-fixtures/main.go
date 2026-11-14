// @author: kevinkelly
// @license: Apache License 2.0
// @file: main.go
// @time: 2026/10/28

// tidbops-fixtures：在 tiup 中控机上以「只读」方式导出样例数据包，并提供校验 / 查看 / 演示命令。
//
// 用法：
//
//	tidbops-fixtures export  [flags]   导出（只读、脱敏、打包）
//	tidbops-fixtures export --dry-run  只打印将要访问的全部内容，不执行任何请求
//	tidbops-fixtures verify  <dir>     校验数据包完整性（哈希）
//	tidbops-fixtures list    <dir>     列出数据包内容
//	tidbops-fixtures show    <dir> <name>  打印某个条目
//	tidbops-fixtures demo    [--out dir]   用内置模拟集群生成演示数据包
//	tidbops-fixtures version
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tidbops/internal/fakecluster"
	"tidbops/internal/fixtures"
	"tidbops/internal/source"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "export":
		err = runExport(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "list":
		err = runList(os.Args[2:])
	case "show":
		err = runShow(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("tidbops-fixtures", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		if ee, ok := err.(exitErr); ok {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

type exitErr struct {
	msg  string
	code int
}

func (e exitErr) Error() string { return e.msg }

func usage() {
	fmt.Fprint(os.Stderr, `tidbops-fixtures `+version+` —— 只读导出 TiDB 样例数据包

  export   导出样例数据包（只读、脱敏、打包）；加 --dry-run 只预览
  verify   <dir>         校验数据包完整性
  list     <dir>         列出数据包内容
  show     <dir> <name>  打印某个条目
  demo     [--out dir]   用内置模拟集群生成演示数据包
  version

先运行 "export --dry-run" 查看将访问的全部内容。
`)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	out := fs.String("out", "fixtures-export", "输出目录（同时生成 <out>.tar.gz）")
	cluster := fs.String("cluster", "", "tiup 集群名；留空则自动探测（仅有一个集群时）")
	pd := fs.String("pd", "", "PD 地址，如 http://10.0.1.21:2379；留空则自动探测")
	tidb := fs.String("tidb-status", "", "TiDB 状态端口地址，如 http://10.0.1.11:10080")
	prom := fs.String("prom", "", "Prometheus 地址")
	alert := fs.String("alert", "", "Alertmanager 地址")
	myHost := fs.String("mysql-host", "", "只读 SQL 的 TiDB 地址")
	myPort := fs.Int("mysql-port", 0, "TiDB SQL 端口，默认 4000")
	myUser := fs.String("mysql-user", "", "只读账号（密码请放环境变量 TIDBOPS_MYSQL_PWD）")
	myBin := fs.String("mysql-bin", "mysql", "mysql 客户端路径")
	tiup := fs.String("tiup", "tiup", "tiup 可执行文件路径")
	home := fs.String("home", "", "tiup 用户主目录，默认当前用户主目录")
	since := fs.Duration("since", time.Hour, "Prometheus 趋势窗口")
	regions := fs.Int("regions", 200, "Region 抽样数量")
	only := fs.String("only", "", "只导出这些前缀的条目，逗号分隔，如 pd/,prom/")
	skip := fs.String("skip", "", "跳过这些前缀的条目，逗号分隔，如 sql/")
	timeout := fs.Duration("timeout", 20*time.Second, "单个请求超时")
	maskIP := fs.Bool("mask-ip", false, "把 IP 地址替换为一致的假地址")
	noMaskSQL := fs.Bool("no-mask-sql", false, "不对 SQL 文本做字面量脱敏（不推荐）")
	noRedact := fs.Bool("no-redact", false, "不脱敏密码 / 密钥字段（不推荐）")
	dry := fs.Bool("dry-run", false, "只打印将要访问的内容，不执行任何请求")
	noAuto := fs.Bool("no-auto", false, "关闭自动探测（集群名与各地址必须手动给出）")
	noArchive := fs.Bool("no-archive", false, "不生成 tar.gz")
	fs.Parse(args)

	h := *home
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	cfg := source.LiveConfig{
		Endpoints: map[string]string{},
		MySQL:     source.MySQLConfig{Bin: *myBin, Host: *myHost, Port: *myPort, User: *myUser, Password: os.Getenv("TIDBOPS_MYSQL_PWD")},
		TiupBin:   *tiup, Timeout: *timeout,
	}
	for k, v := range map[string]string{"pd": *pd, "tidb": *tidb, "prom": *prom, "alert": *alert} {
		if v != "" {
			cfg.Endpoints[k] = v
		}
	}
	opt := fixtures.Options{MaskIP: *maskIP, MaskSQLLiterals: !*noMaskSQL, RedactSecrets: !*noRedact}
	params := source.CatalogParams{Cluster: *cluster, Home: h, Since: *since, Regions: *regions, Now: time.Now()}
	return doExport(exportArgs{
		out: *out, cfg: cfg, opt: opt, params: params, only: splitCSV(*only), skip: splitCSV(*skip),
		dry: *dry, noAuto: *noAuto, archive: !*noArchive,
	}, nil)
}

type exportArgs struct {
	out     string
	cfg     source.LiveConfig
	opt     fixtures.Options
	params  source.CatalogParams
	only    []string
	skip    []string
	dry     bool
	noAuto  bool
	archive bool
}

func doExport(a exportArgs, w io.Writer) error {
	if w == nil {
		w = os.Stdout
	}
	ctx := context.Background()
	live := source.NewLive(a.cfg)

	// 自动探测：只读命令 tiup cluster list / display
	if !a.noAuto && !a.dry {
		name, notes, err := source.Detect(ctx, &a.cfg, a.params.Cluster)
		if err != nil {
			return err
		}
		a.params.Cluster = name
		for _, n := range notes {
			fmt.Fprintln(w, n)
		}
		live = source.NewLive(a.cfg)
	}

	items := source.FilterItems(source.DefaultCatalog(a.params), a.only, a.skip)
	for _, it := range items {
		if err := source.ValidateItem(it); err != nil {
			return fmt.Errorf("内部错误：采集目录包含未通过校验的条目: %w", err)
		}
	}
	if a.dry {
		printPlan(w, items, a)
		return nil
	}
	if a.params.Cluster == "" {
		return fmt.Errorf("需要集群名：请用 --cluster 指定，或去掉 --no-auto")
	}
	if a.out == "" {
		return fmt.Errorf("--out 不能为空")
	}
	if st, err := os.Stat(a.out); err == nil && st.IsDir() {
		if ents, _ := os.ReadDir(a.out); len(ents) > 0 {
			return fmt.Errorf("输出目录 %s 已存在且非空，请换一个目录", a.out)
		}
	}

	rec := source.NewRecorder(live, a.out, a.params.Cluster, version, a.opt)
	rec.Hook = func(r fixtures.ItemRecord) {
		st := "✔"
		if !r.OK {
			st = "✖"
			if r.Optional {
				st = "-"
			}
		}
		line := fmt.Sprintf("  %s %-28s %7d B", st, r.Name, r.Bytes)
		if !r.OK {
			line += "  " + shorten(r.Error, 90)
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "开始导出 %d 项（只读）→ %s\n", len(items), a.out)
	rec.RecordAll(ctx, items)
	if display := rec.Man.Find("tiup/cluster-display"); display != nil && display.OK {
		if b, err := os.ReadFile(filepath.Join(a.out, filepath.FromSlash(display.File))); err == nil {
			rec.Man.ClusterVersion = source.ParseDisplay(string(b)).Version
		}
	}
	if err := rec.Finish(items); err != nil {
		return err
	}
	if err := writeReview(a.out, rec.Man); err != nil {
		return err
	}
	ok, failed, optFailed := rec.Man.Summary()
	fmt.Fprintf(w, "\n完成：成功 %d · 必需项失败 %d · 可选项失败 %d\n", ok, failed, optFailed)
	if a.archive {
		tgz := strings.TrimRight(a.out, `/\`) + ".tar.gz"
		sum, err := archive(a.out, tgz)
		if err != nil {
			return fmt.Errorf("打包失败: %w", err)
		}
		fmt.Fprintf(w, "已打包：%s\nSHA-256：%s\n", tgz, sum)
	}
	fmt.Fprintf(w, "\n发送前请先阅读 %s，确认其中没有不该外传的内容。\n", filepath.Join(a.out, "REVIEW.md"))
	if failed > 0 {
		return exitErr{msg: fmt.Sprintf("%d 个必需项采集失败（数据包仍已生成，失败原因见 manifest.json）", failed), code: 2}
	}
	return nil
}

func printPlan(w io.Writer, items []source.Item, a exportArgs) {
	fmt.Fprintln(w, "【预览 / dry-run】不会执行任何请求。导出时只会进行下列只读操作：")
	fmt.Fprintf(w, "\n集群: %s\n\n", orDefault(a.params.Cluster, "(自动探测)"))
	for _, it := range items {
		fmt.Fprintf(w, "  %-28s %s\n", it.Name, it.Request())
	}
	at := source.Attest(items)
	fmt.Fprintf(w, "\n只读声明：HTTP 方法仅 %v；命令 %d 种；SQL %d 条；文件 %d 个\n", at.HTTPMethods, len(at.Commands), len(at.SQL), len(at.Files))
	fmt.Fprintf(w, "脱敏：SQL 字面量=%v · 密钥字段=%v · IP=%v\n", a.opt.MaskSQLLiterals, a.opt.RedactSecrets, a.opt.MaskIP)
	fmt.Fprintln(w, "所有请求在发出前都会再次通过白名单校验；不经过 shell；密码只通过环境变量传给 mysql 客户端。")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func writeReview(dir string, m *fixtures.Manifest) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# 样例数据包审阅清单\n\n- 集群：%s（%s）\n- 采集时间：%s\n- 工具：%s %s\n- 脱敏选项：SQL 字面量=%v · 密钥字段=%v · IP=%v\n\n",
		m.Cluster, orDefault(m.ClusterVersion, "版本未知"), m.CollectedAt.Format(time.RFC3339), m.Tool, m.ToolVersion, m.Options.MaskSQLLiterals, m.Options.RedactSecrets, m.Options.MaskIP)
	b.WriteString("## 发送前请确认\n\n1. 本数据包全部由**只读**操作产生（见下方“只读声明”）。\n2. 打开 `data/sql/` 下的文件，确认 SQL 文本里没有手机号、身份证、订单号等业务数据（应只剩 `?`）。\n3. 打开 `data/tiup/meta.yaml`，确认没有密码、密钥内容（应显示为 `\"***\"`）。\n4. 如不希望暴露真实 IP，请用 `--mask-ip` 重新导出。\n5. 用 `tidbops-fixtures verify <dir>` 校验完整性。\n\n")
	b.WriteString("## 只读声明\n\n- HTTP：仅 GET\n")
	for _, c := range m.ReadOnly.Commands {
		fmt.Fprintf(&b, "- 命令：`%s`\n", c)
	}
	for _, f := range m.ReadOnly.Files {
		fmt.Fprintf(&b, "- 文件：`%s`\n", f)
	}
	for _, s := range m.ReadOnly.SQL {
		fmt.Fprintf(&b, "- SQL：`%s`\n", shorten(s, 160))
	}
	b.WriteString("\n## 条目\n\n| 名称 | 结果 | 大小 | 用途 |\n|---|---|---|---|\n")
	for _, it := range m.Items {
		st := "成功"
		if !it.OK {
			st = "失败：" + shorten(it.Error, 60)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", it.Name, st, it.Bytes, it.Purpose)
	}
	return os.WriteFile(filepath.Join(dir, "REVIEW.md"), []byte(b.String()), 0o644)
}

// archive 把目录打成 tar.gz，返回文件的 SHA-256。
func archive(dir, dest string) (string, error) {
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	base := filepath.Base(filepath.Clean(dir))
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr := &tar.Header{Name: base + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: info.Size(), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		return "", err
	}
	return fixtures.Hash(b), nil
}

func runVerify(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: verify <dir>")
	}
	m, err := fixtures.LoadManifest(args[0])
	if err != nil {
		return err
	}
	errs := fixtures.Dir{Root: args[0]}.Verify(m)
	fmt.Println(m)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Println("  ✖", e)
		}
		return exitErr{msg: fmt.Sprintf("校验失败：%d 个问题", len(errs)), code: 1}
	}
	fmt.Println("校验通过：所有数据文件与清单哈希一致")
	return nil
}

func runList(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: list <dir>")
	}
	m, err := fixtures.LoadManifest(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("集群 %s（%s） 采集于 %s\n", m.Cluster, orDefault(m.ClusterVersion, "版本未知"), m.CollectedAt.Format(time.RFC3339))
	for _, it := range m.Items {
		st := "ok "
		if !it.OK {
			st = "ERR"
		}
		fmt.Printf("  %s %-28s %8d B  %s\n", st, it.Name, it.Bytes, it.Request)
	}
	return nil
}

func runShow(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("用法: show <dir> <name>")
	}
	r, err := source.NewReplay(args[0])
	if err != nil {
		return err
	}
	b, err := r.Fetch(context.Background(), source.Item{Name: args[1]})
	if err != nil {
		return err
	}
	os.Stdout.Write(b)
	return nil
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	out := fs.String("out", filepath.Join("fixtures", "demo"), "输出目录")
	profile := fs.String("profile", "demo", "模拟集群预设：demo（prod-tidb-01，默认）| stg（预发 stg-tidb-01，v8.1.5）| dev（开发 dev-tidb-01，v8.1.2）")
	fs.Parse(args)
	prof, err := fakecluster.ProfileByName(*profile)
	if err != nil {
		return err
	}
	c := fakecluster.StartProfile(prof)
	defer c.Close()
	cfg := source.LiveConfig{
		Endpoints: c.Endpoints(), MySQL: source.MySQLConfig{Host: prof.SQLHost(), Port: 4000, User: "readonly"},
		Exec: prof.Exec(), ReadFile: prof.ReadFile,
	}
	if err := os.RemoveAll(*out); err != nil {
		return err
	}
	fmt.Println("使用内置模拟集群生成演示数据包（数据为模拟，不含任何真实信息）")
	return doExport(exportArgs{
		out: *out, cfg: cfg, opt: fixtures.Options{MaskSQLLiterals: true, RedactSecrets: true},
		params: source.CatalogParams{Cluster: prof.Name, Home: "/home/tidb", Since: time.Hour, Now: time.Now()}, noAuto: true, archive: false,
	}, nil)
}
