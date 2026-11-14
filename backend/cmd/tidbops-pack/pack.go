// @author: kevinkelly
// @license: Apache License 2.0
// @file: pack.go
// @time: 2026/10/28

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"tidbops/internal/dataver"
	"tidbops/internal/release"
)

// frontendDir 是项目根目录下的前端目录（index.html、js/、css/、help/）。
const frontendDir = "frontend"

var (
	reVersion   = regexp.MustCompile(`^\d+\.\d+\.\d+(-[A-Za-z0-9.]+)?$`)
	reWebAsset  = regexp.MustCompile(`^[A-Za-z0-9_-]+\.(js|css)$`) // 与 server 的静态白名单一致
	reHelpDoc   = regexp.MustCompile(`^[a-z]+\.md$`)
	reDeployPkg = regexp.MustCompile(`^tidb-deploy-([0-9.]+)\.tar\.gz$`)
	supported   = map[string]bool{"amd64": true, "arm64": true}
)

type packOptions struct {
	Version    string
	Archs      []string
	OutDir     string
	NotesFile  string
	MinFrom    string
	DeployTool string
	Root       string            // 项目根目录
	Backend    string            // Go 模块目录
	Bins       map[string]string // 架构 -> 现成二进制（跳过构建）
	Stdout     io.Writer
	Stderr     io.Writer
}

type packResult struct {
	Version string
	Arch    string
	Path    string
	Size    int64
	SHA256  string
}

// 可执行文件（模式 0755）；其余 0644。
func fileMode(rel string) uint32 {
	switch rel {
	case "bin/tidbops-server", "install.sh", "upgrade.sh", "rollback.sh":
		return 0o755
	}
	return 0o644
}

func buildPackages(o packOptions) ([]packResult, error) {
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return nil, err
	}
	// 版本号
	ver := strings.TrimSpace(o.Version)
	if ver == "" {
		b, err := os.ReadFile(filepath.Join(root, "VERSION"))
		if err != nil {
			return nil, fmt.Errorf("没有指定 --version，且读取 %s 失败：%w", filepath.Join(root, "VERSION"), err)
		}
		ver = strings.TrimSpace(string(b))
	}
	if !reVersion.MatchString(ver) {
		return nil, fmt.Errorf("版本号 %q 不合法：应形如 1.2.0 或 1.2.0-rc.1（不带 v 前缀）", ver)
	}
	if o.MinFrom != "" && !reVersion.MatchString(o.MinFrom) {
		return nil, fmt.Errorf("--min-from %q 不合法：应形如 1.0.0", o.MinFrom)
	}
	if len(o.Archs) == 0 {
		return nil, fmt.Errorf("没有指定目标架构")
	}
	for _, a := range o.Archs {
		if !supported[a] {
			return nil, fmt.Errorf("不支持的架构 %q（支持 amd64、arm64）", a)
		}
	}
	outDir, err := filepath.Abs(o.OutDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	buildTime := time.Now().UTC().Truncate(time.Second)
	commit := gitCommit(o.Backend)
	notes, err := readNotes(o.NotesFile)
	if err != nil {
		return nil, err
	}
	changelog, section := buildChangelog(root, ver, notes, buildTime)
	manifestNotes := strings.TrimSpace(notes)
	if manifestNotes == "" {
		// 没有 --notes：取项目 CHANGELOG 该版本段落里第一个三级标题之前的概述，避免清单过大
		manifestNotes = strings.TrimSpace(section)
		if i := strings.Index(manifestNotes, "\n### "); i >= 0 {
			manifestNotes = strings.TrimSpace(manifestNotes[:i])
		}
	}
	deploy, err := findDeployTool(root, o.DeployTool)
	if err != nil {
		return nil, err
	}
	if deploy == "" {
		fmt.Fprintln(o.Stderr, "提示：没有找到 tidb-deploy-*.tar.gz，包里不含集群部署工具（可用 --deploy-tool 指定）")
	}

	var results []packResult
	for _, arch := range o.Archs {
		fmt.Fprintf(o.Stdout, "==> 打包 %s linux/%s\n", ver, arch)
		r, err := packOne(o, root, outDir, ver, arch, commit, buildTime, changelog, manifestNotes, deploy)
		if err != nil {
			return nil, fmt.Errorf("打包 linux/%s 失败：%w", arch, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func packOne(o packOptions, root, outDir, ver, arch, commit string, buildTime time.Time, changelog, manifestNotes, deploy string) (packResult, error) {
	var res packResult
	tmp, err := os.MkdirTemp("", "tidbops-pack-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	name := fmt.Sprintf("tidbops-%s-linux-%s", ver, arch)
	stage := filepath.Join(tmp, name)
	if err := os.MkdirAll(filepath.Join(stage, "bin"), 0o755); err != nil {
		return res, err
	}

	// 1. 二进制
	binDst := filepath.Join(stage, "bin", "tidbops-server")
	if pre := o.Bins[arch]; pre != "" {
		if err := copyFile(pre, binDst, false); err != nil {
			return res, fmt.Errorf("复制指定的二进制失败：%w", err)
		}
		if !isELF(binDst) {
			fmt.Fprintf(o.Stderr, "警告：%s 不是 ELF 文件（仅测试时可接受）\n", pre)
		}
	} else {
		if err := goBuild(o.Backend, binDst, ver, commit, buildTime, arch, o.Stdout, o.Stderr); err != nil {
			return res, err
		}
	}

	// 2. web 白名单：frontend/ 下的 index.html、js/*.js、css/*.css（包内 web/ 保持同样的目录结构）
	fe := filepath.Join(root, frontendDir)
	if err := copyFile(filepath.Join(fe, "index.html"), filepath.Join(stage, "web", "index.html"), true); err != nil {
		return res, fmt.Errorf("项目根目录 %s 里没有 %s/index.html：%w", root, frontendDir, err)
	}
	webN := 1
	for _, sub := range []struct{ dir, ext string }{{"js", ".js"}, {"css", ".css"}} {
		ents, err := os.ReadDir(filepath.Join(fe, sub.dir))
		if err != nil {
			return res, err
		}
		for _, e := range ents {
			n := e.Name()
			if !e.Type().IsRegular() || !reWebAsset.MatchString(n) || filepath.Ext(n) != sub.ext {
				continue
			}
			if err := copyFile(filepath.Join(fe, sub.dir, n), filepath.Join(stage, "web", sub.dir, n), true); err != nil {
				return res, err
			}
			webN++
		}
	}
	// 部署工具包：web/deploy/dist 供「安装部署」页下载；deploy/tidb-deploy 是同一份拷贝
	if deploy != "" {
		base := filepath.Base(deploy)
		for _, dir := range []string{filepath.Join("web", "deploy", "dist"), filepath.Join("deploy", "tidb-deploy")} {
			if err := copyFile(deploy, filepath.Join(stage, dir, base), false); err != nil {
				return res, err
			}
			if err := copySha(deploy, filepath.Join(stage, dir, base+".sha256")); err != nil {
				return res, err
			}
		}
	}

	// 2b. 使用帮助：frontend/help/<主题>.md → web/help/（与 server 的 /help/ 白名单一致；README.md 是编写规范，不入包）
	helps, _ := filepath.Glob(filepath.Join(fe, "help", "*.md"))
	for _, h := range helps {
		if !reHelpDoc.MatchString(filepath.Base(h)) {
			continue
		}
		if err := copyFile(h, filepath.Join(stage, "web", "help", filepath.Base(h)), true); err != nil {
			return res, err
		}
	}

	// 3. deploy/ops
	for _, f := range []string{"tidbops.service", "sudoers.tidbops", "exec.json.example", "clusters.json.example", "platform.json"} {
		if err := copyFile(filepath.Join(root, "deploy", "ops", f), filepath.Join(stage, "deploy", "ops", f), true); err != nil {
			return res, fmt.Errorf("复制 deploy/ops/%s 失败：%w", f, err)
		}
	}

	// 4. docs
	docs, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	for _, d := range docs {
		if err := copyFile(d, filepath.Join(stage, "docs", filepath.Base(d)), true); err != nil {
			return res, err
		}
	}

	// 5. CHANGELOG / VERSION / 包装脚本
	if err := writeFile(filepath.Join(stage, "CHANGELOG.md"), []byte(changelog)); err != nil {
		return res, err
	}
	if err := writeFile(filepath.Join(stage, "VERSION"), []byte(ver+"\n")); err != nil {
		return res, err
	}
	for n, c := range wrapperScripts(arch) {
		if err := writeFile(filepath.Join(stage, n), []byte(c)); err != nil {
			return res, err
		}
	}

	// 6. 清单（Windows 上文件系统不保留 Unix 权限，模式统一按规则覆盖）
	files, err := release.BuildFileList(stage)
	if err != nil {
		return res, err
	}
	for i := range files {
		files[i].Mode = fileMode(files[i].Path)
	}
	m := release.Manifest{
		Product: release.Product, Version: ver, BuildTime: buildTime, GoVersion: runtime.Version(),
		OS: "linux", Arch: arch, SchemaVersion: dataver.Schema, MinFrom: o.MinFrom, Notes: manifestNotes, Files: files,
	}
	if err := release.WriteManifest(stage, m); err != nil {
		return res, err
	}

	// 7. tar.gz + sha256
	tgz := filepath.Join(outDir, name+".tar.gz")
	if err := writeTarGz(tgz, stage, name, m, buildTime); err != nil {
		return res, err
	}
	sum, size, err := release.HashFile(tgz)
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(tgz+".sha256", []byte(sum+"  "+filepath.Base(tgz)+"\n"), 0o644); err != nil {
		return res, err
	}
	fmt.Fprintf(o.Stdout, "    web 文件 %d 个，清单文件 %d 个\n", webN, len(files))
	return packResult{Version: ver, Arch: arch, Path: tgz, Size: size, SHA256: sum}, nil
}

// writeTarGz 写出可复现的 tar.gz：根目录 <name>/，条目按路径排序，固定 mtime、uid/gid 0，模式取清单。
func writeTarGz(dst, stage, root string, m release.Manifest, mtime time.Time) error {
	type ent struct {
		name string // 相对 root，正斜杠；目录以空 size 表示
		dir  bool
		mode uint32
		file string
	}
	modes := map[string]uint32{}
	for _, f := range m.Files {
		modes[f.Path] = f.Mode
	}
	modes[release.ManifestName] = 0o644
	var ents []ent
	dirs := map[string]bool{}
	for p, mode := range modes {
		ents = append(ents, ent{name: p, mode: mode, file: filepath.Join(stage, filepath.FromSlash(p))})
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		ents = append(ents, ent{name: d, dir: true, mode: 0o755})
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].name < ents[j].name })

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	hdr := func(name string, dir bool, mode uint32, size int64) *tar.Header {
		h := &tar.Header{Name: name, Mode: int64(mode), Uid: 0, Gid: 0, Uname: "root", Gname: "root", ModTime: mtime, Format: tar.FormatPAX}
		if dir {
			h.Typeflag = tar.TypeDir
			h.Name += "/"
		} else {
			h.Typeflag = tar.TypeReg
			h.Size = size
		}
		return h
	}
	werr := tw.WriteHeader(hdr(root, true, 0o755, 0))
	for _, e := range ents {
		if werr != nil {
			break
		}
		if e.dir {
			werr = tw.WriteHeader(hdr(root+"/"+e.name, true, e.mode, 0))
			continue
		}
		var src *os.File
		src, werr = os.Open(e.file)
		if werr != nil {
			break
		}
		var st os.FileInfo
		st, werr = src.Stat()
		if werr == nil {
			werr = tw.WriteHeader(hdr(root+"/"+e.name, false, e.mode, st.Size()))
		}
		if werr == nil {
			_, werr = io.Copy(tw, src)
		}
		src.Close()
	}
	if werr == nil {
		werr = tw.Close()
	}
	if werr == nil {
		werr = gz.Close()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(dst)
	}
	return werr
}

// goBuild 交叉编译 linux/<arch> 的 tidbops-server。
func goBuild(backend, out, ver, commit string, bt time.Time, arch string, stdout, stderr io.Writer) error {
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	ld := fmt.Sprintf("-s -w -X tidbops/internal/buildinfo.Version=%s -X tidbops/internal/buildinfo.Commit=%s -X tidbops/internal/buildinfo.BuildTime=%s",
		ver, commit, bt.Format(time.RFC3339))
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ld, "-o", abs, "./cmd/tidbops-server")
	cmd.Dir = backend
	cmd.Env = buildEnv(arch)
	var eb bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, io.MultiWriter(&eb, stderr)
	fmt.Fprintf(stdout, "    go build（linux/%s）…\n", arch)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build 失败：%w", err)
	}
	return nil
}

func buildEnv(arch string) []string {
	env := os.Environ()
	has := func(k string) bool {
		for _, e := range env {
			if strings.HasPrefix(strings.ToUpper(e), k+"=") {
				return true
			}
		}
		return false
	}
	env = append(env, "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	for k, v := range map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=mod", "GOPROXY": "off"} {
		if !has(k) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func gitCommit(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	c := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^[0-9a-f]{4,40}$`).MatchString(c) {
		return ""
	}
	return c
}

func readNotes(file string) (string, error) {
	if file == "" {
		return "", nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("读取 --notes 文件失败：%w", err)
	}
	return normalizeLF(string(b)), nil
}

// buildChangelog 生成包内 CHANGELOG.md：--notes 内容 + 项目根 CHANGELOG.md 里该版本的段落；都没有则生成一段占位。
// 返回完整文本与从项目 CHANGELOG 摘出的版本段落。
func buildChangelog(root, ver, notes string, bt time.Time) (full, section string) {
	if b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md")); err == nil {
		section = extractSection(normalizeLF(string(b)), ver)
	}
	var sb strings.Builder
	sb.WriteString("# TiDB Ops 变更说明\n\n")
	if strings.TrimSpace(notes) != "" {
		fmt.Fprintf(&sb, "## 本次发布说明\n\n%s\n\n", strings.TrimSpace(notes))
	}
	if section != "" {
		sb.WriteString(strings.TrimSpace(section) + "\n")
	} else if strings.TrimSpace(notes) == "" {
		fmt.Fprintf(&sb, "## 版本 %s（%s）\n\n- 本版本没有填写变更说明。\n", ver, bt.Format("2006-01-02"))
	}
	return sb.String(), section
}

// extractSection 取 "## ... <ver> ..." 标题开始、到下一个 "## " 之前的段落。
func extractSection(md, ver string) string {
	lines := strings.Split(md, "\n")
	// 版本号前不能紧跟数字 / 点；后面不能继续是版本号的一部分（1.2 不匹配 1.2.0，1.2.0 不匹配 1.2.0-rc.1 / 1.2.01）
	re := regexp.MustCompile(`^##\s+(?:.*[^0-9A-Za-z.])?v?` + regexp.QuoteMeta(ver) + `(?:$|[^0-9A-Za-z._-]|\.(?:$|[^0-9]))`)
	start := -1
	for i, l := range lines {
		if start < 0 {
			if re.MatchString(l) && !strings.HasPrefix(l, "###") {
				start = i
			}
		} else if strings.HasPrefix(l, "## ") {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n"))
		}
	}
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n"))
}

// findDeployTool 返回要放进包里的部署工具包路径；找不到返回空串。
func findDeployTool(root, explicit string) (string, error) {
	if explicit != "" {
		if !reDeployPkg.MatchString(filepath.Base(explicit)) {
			return "", fmt.Errorf("--deploy-tool 文件名应形如 tidb-deploy-0.2.0.tar.gz：%s", filepath.Base(explicit))
		}
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--deploy-tool：%w", err)
		}
		return explicit, nil
	}
	cands, _ := filepath.Glob(filepath.Join(root, "deploy", "dist", "tidb-deploy-*.tar.gz"))
	best, bestV := "", [3]int{-1}
	for _, c := range cands {
		m := reDeployPkg.FindStringSubmatch(filepath.Base(c))
		if m == nil {
			continue
		}
		var v [3]int
		for i, p := range strings.Split(m[1], ".") {
			if i < 3 {
				v[i], _ = strconv.Atoi(p)
			}
		}
		if best == "" || v[0] > bestV[0] || v[0] == bestV[0] && (v[1] > bestV[1] || v[1] == bestV[1] && v[2] > bestV[2]) {
			best, bestV = c, v
		}
	}
	return best, nil
}

// ---------------------------------------------------------------- 文件辅助 ----

func normalizeLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func writeFile(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// copyFile 复制文件；text 为真时把 CRLF 统一成 LF（Windows 上编辑的脚本 / 配置带 CR 会在 Linux 上出问题）。
func copyFile(src, dst string, text bool) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if text {
		b = []byte(normalizeLF(string(b)))
	}
	return writeFile(dst, b)
}

// copySha 为 src 现算 SHA-256，写成标准的 <hash>  <文件名> 格式（不沿用源旁已有的 .sha256，避免它与文件不一致）。
func copySha(src, dst string) error {
	base := filepath.Base(src)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	return writeFile(dst, []byte(hex.EncodeToString(h[:])+"  "+base+"\n"))
}

func isELF(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [4]byte
	_, err = io.ReadFull(f, b[:])
	return err == nil && string(b[:]) == "\x7fELF"
}
