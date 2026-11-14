// @author: kevinkelly
// @license: Apache License 2.0
// @file: main.go
// @time: 2026/10/28

// 命令 tidbops-pack 生成 TiDB Ops 平台的发布包（Linux tar.gz），也可以校验一个发布包。
//
// 在 Windows / macOS / Linux 开发机上都能运行；产物是 Linux 发布包（内含交叉编译的 tidbops-server）。
//
//	go run ./cmd/tidbops-pack --version 1.2.0 [--arch amd64,arm64] [--out ../dist/release] \
//	    [--notes 片段.md] [--min-from 1.0.0] [--deploy-tool ../deploy/dist/tidb-deploy-0.2.0.tar.gz] \
//	    [--web ..] [--bin-amd64 路径] [--bin-arm64 路径]
//	go run ./cmd/tidbops-pack --verify ../dist/release/tidbops-1.2.0-linux-amd64.tar.gz
//
// 包结构与清单格式见 internal/release/manifest.go；使用说明见 docs/运维部署手册.md 12.9。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tidbops-pack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		version    = fs.String("version", "", "版本号 x.y.z[-后缀]；缺省读 <web>/VERSION")
		archs      = fs.String("arch", "amd64,arm64", "目标架构，逗号分隔（amd64 / arm64）")
		out        = fs.String("out", filepath.Join("..", "dist", "release"), "输出目录")
		notes      = fs.String("notes", "", "本次发布的变更说明文件（Markdown 片段）")
		minFrom    = fs.String("min-from", "", "允许直接升级的最低版本；空表示不限")
		deployTool = fs.String("deploy-tool", "", "tidb-deploy-<版本>.tar.gz 路径；缺省取 <web>/deploy/dist 里最新的一个")
		web        = fs.String("web", "..", "项目根目录（前端文件在 frontend/，另有 docs、deploy/ops、CHANGELOG.md、VERSION，都从这里取）")
		backend    = fs.String("backend", ".", "Go 模块目录（含 go.mod 与 cmd/tidbops-server）")
		binAmd64   = fs.String("bin-amd64", "", "指定现成的 amd64 二进制，跳过构建（测试用）")
		binArm64   = fs.String("bin-arm64", "", "指定现成的 arm64 二进制，跳过构建（测试用）")
		verify     = fs.String("verify", "", "校验一个发布包（tar.gz）后退出：安全解包到临时目录并用清单校验每个文件")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "用法：tidbops-pack [--version x.y.z] [--arch amd64,arm64] [--out 目录] [选项]")
		fmt.Fprintln(stderr, "      tidbops-pack --verify <包.tar.gz>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("多余的参数：%s", strings.Join(fs.Args(), " "))
	}
	if *verify != "" {
		return verifyPackage(*verify, stdout)
	}
	opts := packOptions{
		Version:    *version,
		Archs:      splitList(*archs),
		OutDir:     *out,
		NotesFile:  *notes,
		MinFrom:    *minFrom,
		DeployTool: *deployTool,
		Root:       *web,
		Backend:    *backend,
		Bins:       map[string]string{"amd64": *binAmd64, "arm64": *binArm64},
		Stdout:     stdout,
		Stderr:     stderr,
	}
	res, err := buildPackages(opts)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "发布包已生成（版本 %s）：\n", res[0].Version)
	for _, r := range res {
		fmt.Fprintf(stdout, "  %s\n    大小：%s（%d 字节）\n    sha256：%s\n", r.Path, humanSize(r.Size), r.Size, r.SHA256)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func humanSize(n int64) string {
	const mb = 1 << 20
	if n >= mb {
		return fmt.Sprintf("%.1f MiB", float64(n)/mb)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
