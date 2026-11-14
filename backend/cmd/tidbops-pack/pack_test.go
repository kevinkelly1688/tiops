// @author: kevinkelly
// @license: Apache License 2.0
// @file: pack_test.go
// @time: 2026/10/28

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"tidbops/internal/release"
)

// fakeProject 在临时目录里造一个项目根：前端文件、垃圾文件、docs、deploy/ops、部署工具包。
func fakeProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	w := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("frontend/index.html", "<html>home</html>\r\n")
	w("frontend/js/app.js", "console.log(1)\r\n")
	w("frontend/css/style.css", "body{}\n")
	w("_x.py", "print('x')\n")                 // 不得入包
	w("README.md", "# readme\n")               // 不得入包
	w("frontend/js/sub/evil.js", "alert(1)\n") // js/ 下的子目录不得入包
	w("frontend/js/bad name.js", "x\n")        // 文件名不在白名单
	w("frontend/js/note.css", "x\n")           // js/ 里只收 .js
	w("frontend/other/x.js", "x\n")            // 非 js/ css/ 目录不入包
	w("app.js", "console.log(2)\n")            // 项目根目录里的旧位置不再入包
	w("data.json", "{}\n")                     // 不得入包
	w("VERSION", "1.2.0\n")
	w("CHANGELOG.md", "# 变更\n\n## 1.2.0 (2026-10-05)\n\n- 新功能 A\n\n## 1.1.0\n\n- 旧功能 B\n")
	w("docs/A.md", "doc A\n")
	w("docs/B.md", "doc B\r\n")
	w("docs/notes.txt", "不入包\n")
	w("frontend/help/quickstart.md", "# 快速入门\r\n")
	w("frontend/help/nodes.md", "# 节点管理\n")
	w("frontend/help/README.md", "# 编写规范\n") // 不入包
	w("frontend/help/notes.txt", "不入包\n")
	w("deploy/ops/tidbops.service", "[Unit]\r\n")
	w("deploy/ops/sudoers.tidbops", "tidbops ALL=(tidb) NOPASSWD: /x\n")
	w("deploy/ops/exec.json.example", "{}\n")
	w("deploy/ops/clusters.json.example", "{}\n")
	w("deploy/ops/platform.json", "{\"icp\": \"\"}\n")
	w("deploy/ops/install-ops.sh", "#!/bin/bash\n") // 遗留脚本不入包
	w("deploy/dist/tidb-deploy-0.1.0.tar.gz", "old")
	w("deploy/dist/tidb-deploy-0.2.0.tar.gz", "new-deploy-tool")
	w("deploy/dist/tidb-deploy-0.2.0.tar.gz.sha256", "bogus")
	return root
}

func fakeBin(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "tidbops-server")
	if err := os.WriteFile(p, []byte("\x7fELF fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type tarEnt struct {
	hdr  tar.Header
	data []byte
}

func readTgz(t *testing.T, p string) []tarEnt {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []tarEnt
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, tarEnt{*h, b})
	}
}

func writeTgz(t *testing.T, p string, ents []tarEnt) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		h := e.hdr
		h.Size = int64(len(e.data))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write(e.data)
	}
	tw.Close()
	gz.Close()
}

func runPack(t *testing.T, args ...string) (string, error) {
	var so, se bytes.Buffer
	err := run(args, &so, &se)
	return so.String() + se.String(), err
}

// 打一个 1.2.0 / amd64 包，返回包路径。
func buildTestPkg(t *testing.T) (pkg, root, out string) {
	t.Helper()
	root = fakeProject(t)
	out = filepath.Join(t.TempDir(), "out")
	notes := filepath.Join(t.TempDir(), "notes.md")
	os.WriteFile(notes, []byte("- 本次手写说明\r\n"), 0o644)
	log, err := runPack(t, "--version", "1.2.0", "--arch", "amd64", "--out", out, "--web", root, "--bin-amd64", fakeBin(t), "--notes", notes, "--min-from", "1.0.0")
	if err != nil {
		t.Fatalf("打包失败：%v\n%s", err, log)
	}
	return filepath.Join(out, "tidbops-1.2.0-linux-amd64.tar.gz"), root, out
}

func TestPackContents(t *testing.T) {
	pkg, _, out := buildTestPkg(t)
	ents := readTgz(t, pkg)

	files := map[string]tarEnt{}
	var names []string
	for _, e := range ents {
		names = append(names, e.hdr.Name)
		if e.hdr.Typeflag == tar.TypeReg {
			files[strings.TrimPrefix(e.hdr.Name, "tidbops-1.2.0-linux-amd64/")] = e
		}
		if !strings.HasPrefix(e.hdr.Name, "tidbops-1.2.0-linux-amd64/") && e.hdr.Name != "tidbops-1.2.0-linux-amd64/" {
			t.Errorf("条目不在根目录下：%s", e.hdr.Name)
		}
		if e.hdr.Uid != 0 || e.hdr.Gid != 0 {
			t.Errorf("%s uid/gid 应为 0", e.hdr.Name)
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("条目没有按路径排序：%v", names)
	}

	want := []string{
		"MANIFEST.json", "VERSION", "CHANGELOG.md", "install.sh", "upgrade.sh", "rollback.sh",
		"bin/tidbops-server",
		"web/index.html", "web/js/app.js", "web/css/style.css",
		"web/deploy/dist/tidb-deploy-0.2.0.tar.gz", "web/deploy/dist/tidb-deploy-0.2.0.tar.gz.sha256",
		"deploy/tidb-deploy/tidb-deploy-0.2.0.tar.gz", "deploy/tidb-deploy/tidb-deploy-0.2.0.tar.gz.sha256",
		"deploy/ops/tidbops.service", "deploy/ops/sudoers.tidbops", "deploy/ops/exec.json.example", "deploy/ops/clusters.json.example", "deploy/ops/platform.json",
		"docs/A.md", "docs/B.md",
		"web/help/quickstart.md", "web/help/nodes.md",
	}
	sort.Strings(want)
	var got []string
	for k := range files {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("包内文件集合不符\n得到：\n%s\n期望：\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for k := range files {
		if strings.Contains(k, ".py") || strings.HasSuffix(k, "README.md") || strings.Contains(k, "evil") || strings.Contains(k, "install-ops") {
			t.Errorf("白名单之外的文件进了包：%s", k)
		}
	}

	// 模式：脚本与二进制 0755，其余 0644
	for k, e := range files {
		wantMode := int64(0o644)
		if k == "bin/tidbops-server" || k == "install.sh" || k == "upgrade.sh" || k == "rollback.sh" {
			wantMode = 0o755
		}
		if e.hdr.Mode != wantMode {
			t.Errorf("%s 模式 %o，期望 %o", k, e.hdr.Mode, wantMode)
		}
		if !e.hdr.ModTime.Equal(files["VERSION"].hdr.ModTime) {
			t.Errorf("%s mtime 不统一", k)
		}
	}
	// 脚本：LF、shebang、架构与子命令
	for _, s := range []string{"install.sh", "upgrade.sh", "rollback.sh"} {
		b := files[s].data
		if bytes.Contains(b, []byte("\r")) {
			t.Errorf("%s 含 CR", s)
		}
		if !bytes.HasPrefix(b, []byte("#!/usr/bin/env bash\n")) || !bytes.Contains(b, []byte(`PKG_ARCH="amd64"`)) {
			t.Errorf("%s 内容不对：\n%s", s, b)
		}
	}
	if !bytes.Contains(files["install.sh"].data, []byte(`admin install --listen 0.0.0.0:8018 "$@"`)) || !bytes.Contains(files["upgrade.sh"].data, []byte(`admin upgrade "$@"`)) {
		t.Error("install.sh / upgrade.sh 没有调用对应的 admin 子命令")
	}
	if !bytes.Contains(files["rollback.sh"].data, []byte(`/opt/tidbops/current/bin/tidbops-server`)) || !bytes.Contains(files["rollback.sh"].data, []byte(`admin rollback "$@"`)) {
		t.Error("rollback.sh 应调用 /opt/tidbops/current 下的二进制")
	}
	// 文本文件统一 LF
	for _, k := range []string{"web/index.html", "docs/B.md", "deploy/ops/tidbops.service"} {
		if bytes.Contains(files[k].data, []byte("\r")) {
			t.Errorf("%s 应已转成 LF", k)
		}
	}
	// CHANGELOG：notes + 版本段落；VERSION
	cl := string(files["CHANGELOG.md"].data)
	if !strings.Contains(cl, "本次手写说明") || !strings.Contains(cl, "新功能 A") || strings.Contains(cl, "旧功能 B") {
		t.Errorf("CHANGELOG 内容不对：\n%s", cl)
	}
	if string(files["VERSION"].data) != "1.2.0\n" {
		t.Errorf("VERSION = %q", files["VERSION"].data)
	}
	// sha256 文件：格式与内容
	sidecar, err := os.ReadFile(pkg + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	sum, _, _ := release.HashFile(pkg)
	if string(sidecar) != sum+"  tidbops-1.2.0-linux-amd64.tar.gz\n" {
		t.Errorf("sha256 文件内容不对：%q（期望 %s）", sidecar, sum)
	}
	// 包内部署工具的 sha256 是现算的，而不是源目录里那份假的
	if strings.Contains(string(files["deploy/tidb-deploy/tidb-deploy-0.2.0.tar.gz.sha256"].data), "bogus") {
		t.Error("部署工具 sha256 不应沿用源目录里的旧文件")
	}
	if string(files["deploy/tidb-deploy/tidb-deploy-0.2.0.tar.gz"].data) != "new-deploy-tool" {
		t.Error("应取最新版本（0.2.0）的部署工具包")
	}
	// 输出目录里只有包与 sha256
	des, _ := os.ReadDir(out)
	if len(des) != 2 {
		t.Errorf("输出目录应只有包与 .sha256，实际 %d 项", len(des))
	}

	// 清单：与 VerifyDir 一致，字段正确
	var mb []byte
	for _, e := range ents {
		if strings.HasSuffix(e.hdr.Name, "/MANIFEST.json") {
			mb = e.data
		}
	}
	if len(mb) == 0 {
		t.Fatal("缺少 MANIFEST.json")
	}
	tmp := t.TempDir()
	rootName, _, err := extractSafe(pkg, tmp)
	if err != nil {
		t.Fatal(err)
	}
	m, err := release.ReadManifest(filepath.Join(tmp, rootName))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.2.0" || m.Arch != "amd64" || m.OS != "linux" || m.Product != release.Product || m.SchemaVersion < 1 || m.MinFrom != "1.0.0" || m.GoVersion == "" {
		t.Errorf("清单字段不对：%+v", m)
	}
	if time.Since(m.BuildTime) > time.Minute || !strings.Contains(m.Notes, "本次手写说明") {
		t.Errorf("BuildTime / Notes 不对：%v %q", m.BuildTime, m.Notes)
	}
	if err := release.VerifyDir(filepath.Join(tmp, rootName), m); err != nil {
		t.Errorf("VerifyDir：%v", err)
	}
	for _, f := range m.Files {
		if f.Path == "bin/tidbops-server" && f.Mode != 0o755 || f.Path == "web/app.js" && f.Mode != 0o644 {
			t.Errorf("清单模式不对：%+v", f)
		}
	}
}

func TestVerifyCommand(t *testing.T) {
	pkg, _, _ := buildTestPkg(t)
	log, err := runPack(t, "--verify", pkg)
	if err != nil {
		t.Fatalf("--verify 应通过：%v\n%s", err, log)
	}
	for _, s := range []string{"校验通过", "1.2.0", "linux/amd64", "整包 sha256 校验通过"} {
		if !strings.Contains(log, s) {
			t.Errorf("--verify 输出缺少 %q：\n%s", s, log)
		}
	}

	// 1. 篡改整包一个字节：与 .sha256 不一致
	raw, _ := os.ReadFile(pkg)
	bad := append([]byte{}, raw...)
	bad[len(bad)/2] ^= 0xff
	badPkg := filepath.Join(t.TempDir(), "tidbops-1.2.0-linux-amd64.tar.gz")
	os.WriteFile(badPkg, bad, 0o644)
	os.WriteFile(badPkg+".sha256", mustRead(t, pkg+".sha256"), 0o644)
	if _, err := runPack(t, "--verify", badPkg); err == nil {
		t.Error("整包被篡改应校验失败")
	}

	// 2. 篡改包内一个文件的一个字节（重新打 tar，没有 .sha256）：清单校验失败
	ents := readTgz(t, pkg)
	for i := range ents {
		if strings.HasSuffix(ents[i].hdr.Name, "/bin/tidbops-server") {
			ents[i].data = append([]byte{}, ents[i].data...)
			ents[i].data[len(ents[i].data)-1] ^= 1
		}
	}
	tampered := filepath.Join(t.TempDir(), "tidbops-1.2.0-linux-amd64.tar.gz")
	writeTgz(t, tampered, ents)
	log, err = runPack(t, "--verify", tampered)
	if err == nil || !strings.Contains(err.Error(), "bin/tidbops-server") {
		t.Errorf("文件被篡改应校验失败并指出文件：%v\n%s", err, log)
	}

	// 3. 多出清单之外的文件
	ents = readTgz(t, pkg)
	ents = append(ents, tarEnt{tar.Header{Name: "tidbops-1.2.0-linux-amd64/extra.txt", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("x")})
	extra := filepath.Join(t.TempDir(), "tidbops-1.2.0-linux-amd64.tar.gz")
	writeTgz(t, extra, ents)
	if _, err := runPack(t, "--verify", extra); err == nil || !strings.Contains(err.Error(), "清单之外") {
		t.Errorf("多出文件应失败：%v", err)
	}

	// 4. 模式被改：与清单不一致
	ents = readTgz(t, pkg)
	for i := range ents {
		if strings.HasSuffix(ents[i].hdr.Name, "/install.sh") {
			ents[i].hdr.Mode = 0o644
		}
	}
	mode := filepath.Join(t.TempDir(), "tidbops-1.2.0-linux-amd64.tar.gz")
	writeTgz(t, mode, ents)
	if _, err := runPack(t, "--verify", mode); err == nil || !strings.Contains(err.Error(), "模式") {
		t.Errorf("模式被改应失败：%v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyRejectsUnsafeTar(t *testing.T) {
	base := "tidbops-1.2.0-linux-amd64"
	file := func(name, body string) tarEnt {
		return tarEnt{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, []byte(body)}
	}
	cases := []struct {
		name string
		ents []tarEnt
		want string
	}{
		{"路径穿越", []tarEnt{file(base+"/../evil.txt", "x")}, "不安全"},
		{"穿越到上级", []tarEnt{file(base+"/a/../../evil.txt", "x")}, "不安全"},
		{"绝对路径", []tarEnt{file("/etc/passwd", "x")}, "不安全"},
		{"盘符", []tarEnt{file("C:/Windows/x", "x")}, "不安全"},
		{"反斜杠", []tarEnt{file(base+`\..\evil`, "x")}, "不安全"},
		{"符号链接", []tarEnt{{tar.Header{Name: base + "/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}, nil}}, "不允许的条目类型"},
		{"硬链接", []tarEnt{{tar.Header{Name: base + "/hl", Typeflag: tar.TypeLink, Linkname: base + "/x", Mode: 0o644}, nil}}, "不允许的条目类型"},
		{"多个根目录", []tarEnt{file(base+"/a", "x"), file("tidbops-9.9.9-linux-amd64/b", "x")}, "多个根目录"},
		{"根目录名不对", []tarEnt{file("evil/a", "x")}, "根目录名"},
		{"重复条目", []tarEnt{file(base+"/a", "x"), file(base+"/a", "y")}, "重复"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "evil.tar.gz")
			writeTgz(t, p, c.ents)
			_, err := runPack(t, "--verify", p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应因 %q 被拒绝，实际：%v", c.want, err)
			}
			// 不能在解包目录之外落任何文件
			if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
				t.Error("发生了路径穿越写文件")
			}
		})
	}
}

func TestVersionValidation(t *testing.T) {
	root := fakeProject(t)
	bin := fakeBin(t)
	for _, v := range []string{"v1.2.0", "1.2", "1.2.x", "1.2.0 ", "a.b.c", "1.2.0-", "1.2.0/../x", "1.2.0_rc"} {
		out := filepath.Join(t.TempDir(), "o")
		args := []string{"--version", v, "--arch", "amd64", "--out", out, "--web", root, "--bin-amd64", bin}
		if v == "1.2.0 " { // 首尾空白会被去掉，换成中间含空格的
			args[1] = "1.2 .0"
		}
		if _, err := runPack(t, args...); err == nil {
			t.Errorf("版本号 %q 应被拒绝", v)
		}
		if _, err := os.Stat(out); err == nil {
			if des, _ := os.ReadDir(out); len(des) > 0 {
				t.Errorf("版本号 %q 被拒绝后不应产生文件", v)
			}
		}
	}
	for _, v := range []string{"1.2.0", "10.20.30", "1.2.0-rc.1", "1.2.0-beta"} {
		out := filepath.Join(t.TempDir(), "o")
		if log, err := runPack(t, "--version", v, "--arch", "amd64", "--out", out, "--web", root, "--bin-amd64", bin); err != nil {
			t.Errorf("版本号 %q 应通过：%v\n%s", v, err, log)
		}
	}
	// 不支持的架构
	if _, err := runPack(t, "--version", "1.0.0", "--arch", "riscv64", "--out", t.TempDir(), "--web", root); err == nil {
		t.Error("不支持的架构应报错")
	}
}

func TestVersionFromFile(t *testing.T) {
	root := fakeProject(t)
	out := filepath.Join(t.TempDir(), "o")
	// 不给 --version：读 <web>/VERSION（1.2.0），两个架构都用现成二进制
	log, err := runPack(t, "--arch", "amd64,arm64", "--out", out, "--web", root, "--bin-amd64", fakeBin(t), "--bin-arm64", fakeBin(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	for _, a := range []string{"amd64", "arm64"} {
		if _, err := os.Stat(filepath.Join(out, "tidbops-1.2.0-linux-"+a+".tar.gz")); err != nil {
			t.Errorf("缺少 %s 包：%v", a, err)
		}
	}
	if !strings.Contains(log, "sha256：") {
		t.Errorf("输出里应打印 sha256：\n%s", log)
	}
	// arm64 包的脚本架构
	for _, e := range readTgz(t, filepath.Join(out, "tidbops-1.2.0-linux-arm64.tar.gz")) {
		if strings.HasSuffix(e.hdr.Name, "/install.sh") && !bytes.Contains(e.data, []byte(`PKG_ARCH="arm64"`)) {
			t.Error("arm64 包的 install.sh 架构不对")
		}
	}
	// 没有 VERSION 也没有 --version
	empty := t.TempDir()
	if _, err := runPack(t, "--out", out, "--web", empty); err == nil {
		t.Error("没有版本号应报错")
	}
}

func TestPackIsReproducibleInContent(t *testing.T) {
	// 同一份输入、同一个 BuildTime 下，tar 条目顺序固定（排序）；这里只验证两次打包文件集合一致。
	p1, _, _ := buildTestPkg(t)
	a := names(readTgz(t, p1))
	p2, _, _ := buildTestPkg(t)
	b := names(readTgz(t, p2))
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Error("两次打包的条目不一致")
	}
}

func names(es []tarEnt) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.hdr.Name)
	}
	return out
}

func TestExtractSection(t *testing.T) {
	md := "# 变更\n\n## [1.2.0] - 2026-10-05\n\n- a\n\n### 子节\n\n- b\n\n## [1.1.0]\n\n- c\n"
	got := extractSection(md, "1.2.0")
	if !strings.Contains(got, "- a") || !strings.Contains(got, "子节") || strings.Contains(got, "- c") {
		t.Errorf("段落提取不对：%q", got)
	}
	if extractSection(md, "1.2") != "" || extractSection(md, "2.0.0") != "" {
		t.Error("不应匹配到不存在的版本")
	}
	if extractSection("## 版本 1.20.0\n- x\n", "1.2.0") != "" {
		t.Error("1.2.0 不应匹配 1.20.0")
	}
}

func TestChangelogFallback(t *testing.T) {
	// 既没有 --notes 也没有项目 CHANGELOG 段落：生成「版本 x.y.z（日期）」
	full, _ := buildChangelog(t.TempDir(), "3.0.0", "", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(full, "## 版本 3.0.0（2026-10-05）") {
		t.Errorf("兜底段落不对：%q", full)
	}
}

// 包装脚本语法检查（本机有可用的 bash 时）。
func TestScriptsSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash")
	}
	if err := exec.Command(bash, "-c", "true").Run(); err != nil {
		t.Skip("bash 不可用")
	}
	for n, c := range wrapperScripts("amd64") {
		p := filepath.Join(t.TempDir(), n)
		os.WriteFile(p, []byte(c), 0o755)
		if out, err := exec.Command(bash, "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s 语法错误：%v\n%s", n, err, out)
		}
	}
}

// install.sh 默认监听 0.0.0.0:8018，并且在 "$@" 之前（用户自己传 --listen 会覆盖）；upgrade.sh / rollback.sh 不带默认监听。
func TestWrapperDefaultListen(t *testing.T) {
	sc := wrapperScripts("amd64")
	if want := `admin install --listen 0.0.0.0:8018 "$@"`; !strings.Contains(sc["install.sh"], want) {
		t.Errorf("install.sh 应带默认监听 %q:\n%s", want, sc["install.sh"])
	}
	for _, n := range []string{"upgrade.sh", "rollback.sh"} {
		if strings.Contains(sc[n], "--listen") || strings.Contains(sc[n], "@DEFAULTS@") || strings.Contains(sc[n], "@SUB@") {
			t.Errorf("%s 不应带默认监听或残留占位符:\n%s", n, sc[n])
		}
	}
	if !strings.Contains(sc["upgrade.sh"], `admin upgrade "$@"`) {
		t.Errorf("upgrade.sh 调用不对:\n%s", sc["upgrade.sh"])
	}
	if !strings.Contains(sc["upgrade.sh"], "platform.json") || strings.Contains(sc["rollback.sh"], "platform.json") {
		t.Error("install.sh / upgrade.sh 要放置 platform.json，rollback.sh 不动它")
	}
}

// 放置备案号配置文件的片段：不存在才放、已存在不覆盖、预演 / 帮助不动系统、包里没有就什么都不做。
func TestScriptPlatformConf(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || exec.Command(bash, "-c", "true").Run() != nil {
		t.Skip("没有可用的 bash")
	}
	run := func(t *testing.T, pkgFile *string, etcFile *string, args ...string) (etc, out string) {
		t.Helper()
		here, etcDir := t.TempDir(), filepath.Join(t.TempDir(), "etc-tidb-ops")
		if pkgFile != nil {
			os.MkdirAll(filepath.Join(here, "deploy", "ops"), 0o755)
			os.WriteFile(filepath.Join(here, "deploy", "ops", "platform.json"), []byte(*pkgFile), 0o644)
		}
		if etcFile != nil {
			os.MkdirAll(etcDir, 0o755)
			os.WriteFile(filepath.Join(etcDir, "platform.json"), []byte(*etcFile), 0o644)
		}
		snippet := strings.ReplaceAll(scriptPlatformConf, `/etc/tidb-ops`, filepath.ToSlash(etcDir))
		script := "set -euo pipefail\nHERE=\"" + filepath.ToSlash(here) + "\"\n" + snippet
		cmd := exec.Command(bash, append([]string{"-c", script, "x"}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("片段执行失败: %v\n%s", err, b)
		}
		data, _ := os.ReadFile(filepath.Join(etcDir, "platform.json"))
		return string(data), string(b)
	}
	pkg, user := `{"icp": ""}`, `{"icp": "京ICP备1号"}`

	if etc, out := run(t, &pkg, nil); etc != pkg || !strings.Contains(out, "platform.json") {
		t.Errorf("不存在时应放置包里的文件并提示: etc=%q out=%q", etc, out)
	}
	if etc, out := run(t, &pkg, &user); etc != user || out != "" {
		t.Errorf("已存在时绝不能覆盖（用户改过的内容）: etc=%q out=%q", etc, out)
	}
	for _, a := range []string{"--dry-run", "-dry-run", "--dry-run=true", "-h", "--help"} {
		if etc, _ := run(t, &pkg, nil, "--standalone", a); etc != "" {
			t.Errorf("%s 时不应改动系统: etc=%q", a, etc)
		}
	}
	if etc, out := run(t, nil, nil); etc != "" || out != "" {
		t.Errorf("包里没有 platform.json（旧包）时什么都不做: etc=%q out=%q", etc, out)
	}
}
