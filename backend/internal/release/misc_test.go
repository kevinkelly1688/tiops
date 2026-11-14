// @author: kevinkelly
// @license: Apache License 2.0
// @file: misc_test.go
// @time: 2026/10/28

package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tidbops/internal/dataver"
)

// ---- 构造恶意 / 正常的归档 ----

type tarEnt struct {
	name     string
	typ      byte
	body     string
	linkname string
}

func writeTar(t *testing.T, path string, ents []tarEnt) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o600, Linkname: e.linkname}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsMaliciousArchives(t *testing.T) {
	bad := map[string][]tarEnt{
		"路径穿越":   {{name: "users.json", body: "ok"}, {name: "../evil.txt", body: "x"}},
		"嵌套穿越":   {{name: "a/../../evil.txt", body: "x"}},
		"绝对路径":   {{name: "/etc/passwd", body: "x"}},
		"反斜杠":    {{name: `..\evil.txt`, body: "x"}},
		"盘符":     {{name: `C:/evil.txt`, body: "x"}},
		"符号链接":   {{name: "users.json", body: "ok"}, {name: "link", typ: tar.TypeSymlink, linkname: "/etc/shadow"}},
		"硬链接":    {{name: "hard", typ: tar.TypeLink, linkname: "users.json"}},
		"设备文件":   {{name: "dev", typ: tar.TypeChar}},
		"FIFO":   {{name: "pipe", typ: tar.TypeFifo}},
		"空路径":    {{name: "", body: "x"}},
		"重复条目":   {{name: "users.json", body: "a"}, {name: "users.json", body: "b"}},
		"目录穿越后缀": {{name: "dir/..", typ: tar.TypeDir}},
	}
	for name, ents := range bad {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t)
			v.install("1.0.0", pkgOpt{})
			v.seedData("precious")
			arch := filepath.Join(t.TempDir(), "evil.tar.gz")
			writeTar(t, arch, ents)
			v.sys.calls = nil
			err := v.e.Restore(ctxBG(), arch)
			if err == nil {
				t.Fatal("恶意归档必须被拒绝")
			}
			if got := v.readData("users.json"); got != "precious" {
				t.Errorf("拒绝后数据必须保持不变，实际 %q", got)
			}
			if v.sys.called("systemctl stop") {
				t.Error("校验失败前不应停服务")
			}
			if len(backupFiles(t, v)) != 0 {
				t.Error("校验失败前不应备份")
			}
			// 数据目录之外也不能出现文件
			if exists(filepath.Join(v.root, "evil.txt")) || exists(filepath.Join(filepath.Dir(v.l.Data()), "evil.txt")) || exists(filepath.Join(v.l.Data(), "..", "evil.txt")) {
				t.Error("发生了路径穿越写入")
			}
		})
	}
}

func TestRestoreOK(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	bk := filepath.Join(t.TempDir(), "my.tar.gz")
	if err := v.e.Backup(ctxBG(), bk); err != nil {
		t.Fatal(err)
	}
	v.seedData("users-changed")
	_ = os.WriteFile(filepath.Join(v.l.Data(), "new.json"), []byte("n"), 0o600)
	v.sys.calls = nil
	if err := v.e.Restore(ctxBG(), bk); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	if v.readData("users.json") != "users-v1" || v.readData("new.json") != "<missing>" {
		t.Error("数据应恢复到备份时")
	}
	if !v.sys.called("systemctl stop") || !v.sys.called("systemctl start") || !v.sys.isActive() {
		t.Errorf("服务运行中恢复：应先停后启: %v", v.sys.calls)
	}
	if v.countRestoreBackups() != 1 {
		t.Error("恢复前应备份当前数据")
	}
	// 历史保留
	if len(v.history()) == 0 {
		t.Error("升级历史不应因恢复而丢失")
	}
	// 服务本来是停止的 → 不应启动
	_ = v.e.stopService(ctxBG())
	v.sys.calls = nil
	if err := v.e.Restore(ctxBG(), bk); err != nil {
		t.Fatal(err)
	}
	if v.sys.called("systemctl start") {
		t.Error("原本停止的服务不应被启动")
	}
	// 缺参数 / 非 root
	if err := v.e.Restore(ctxBG(), ""); err == nil {
		t.Error("缺 --file 应报错")
	}
	v.sys.euid = 1000
	if err := v.e.Restore(ctxBG(), bk); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("非 root 应拒绝: %v", err)
	}
}

func TestRestoreTooNewDataKeepsServiceStopped(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	arch := filepath.Join(t.TempDir(), "new.tar.gz")
	writeTar(t, arch, []tarEnt{{name: "VERSION", body: `{"schema": 9, "version": "9.9.9"}`}, {name: "users.json", body: "x"}})
	if err := v.e.Restore(ctxBG(), arch); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "输出", v.dumpOut(), "高于当前版本")
	if v.sys.isActive() {
		t.Error("数据 schema 过高时服务应保持停止")
	}
}

func TestBackupManual(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	_ = os.MkdirAll(filepath.Join(v.l.Data(), "tmp"), 0o700)
	_ = os.WriteFile(filepath.Join(v.l.Data(), "tmp", "scratch"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(v.l.Data(), "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(v.l.Data(), "sub", "a.json"), []byte("a"), 0o600)

	if err := v.e.Backup(ctxBG(), ""); err != nil {
		t.Fatal(err)
	}
	bs, _ := v.e.listBackups()
	if len(bs) != 1 || !strings.HasSuffix(bs[0].Name, "-1.0.0-manual.tar.gz") {
		t.Fatalf("备份 = %+v", bs)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(bs[0].Path); st.Mode().Perm() != 0o600 {
			t.Errorf("权限 = %v", st.Mode().Perm())
		}
	}
	if readBackupFile(t, bs[0].Path, "users.json") != "users-v1" || readBackupFile(t, bs[0].Path, "sub/a.json") != "a" || !strings.Contains(readBackupFile(t, bs[0].Path, "VERSION"), "schema") {
		t.Error("备份内容不对")
	}
	if readBackupFile(t, bs[0].Path, "tmp/scratch") != "<missing>" {
		t.Error("tmp 目录应被排除")
	}
	// --out 在数据目录内 → 拒绝；已存在 → 拒绝
	if err := v.e.Backup(ctxBG(), filepath.Join(v.l.Data(), "x.tar.gz")); err == nil {
		t.Error("备份文件不能放进数据目录")
	}
	if err := v.e.Backup(ctxBG(), bs[0].Path); err == nil {
		t.Error("不应覆盖已有文件")
	}
}

func TestUninstall(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	if err := v.e.Uninstall(ctxBG(), UninstallOpts{PurgeData: true}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("--purge-data 需要 --yes: %v", err)
	}
	if err := v.e.Uninstall(ctxBG(), UninstallOpts{DryRun: true}); err != nil || !exists(v.l.Unit()) {
		t.Fatalf("dry-run 不应改动: %v", err)
	}
	if err := v.e.Uninstall(ctxBG(), UninstallOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, v.dumpOut())
	}
	if exists(v.l.Unit()) || exists(v.l.Sudoers()) || exists(v.l.Current()) {
		t.Error("unit / sudoers / current 应已删除")
	}
	if !exists(v.l.Data()) || !exists(v.l.ReleaseDir("1.0.0")) || !exists(v.l.Backups()) || !exists(v.l.ExecKey()) {
		t.Error("默认应保留数据目录、releases、备份与密钥")
	}
	if !v.sys.called("systemctl disable --now tidbops") {
		t.Error("应停止并禁用服务")
	}
	if err := v.e.Uninstall(ctxBG(), UninstallOpts{PurgeData: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if exists(v.l.Data()) || exists(v.l.Backups()) {
		t.Error("--purge-data --yes 应删除数据与备份")
	}
	if !exists(v.l.ReleaseDir("1.0.0")) {
		t.Error("releases 仍应保留")
	}
}

func TestStatusJSON(t *testing.T) {
	v := newEnv(t)
	// 未安装
	s, err := v.e.Status(ctxBG())
	if err != nil || s.Installed {
		t.Fatalf("未安装: %+v %v", s, err)
	}
	v.install("1.0.0", pkgOpt{})
	v.seedData("users-v1")
	if _, err := v.upgrade("1.1.0", pkgOpt{schema: 2}, UpgradeOpts{}); err != nil {
		t.Fatal(err)
	}
	v.sys.euid = 1000 // status 不需要 root
	v.out.Reset()
	s, err = v.e.Status(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.e.PrintStatus(s, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Installed    bool           `json:"installed"`
		Current      string         `json:"current"`
		Releases     []string       `json:"releases"`
		Service      string         `json:"service"`
		DataSchema   *int           `json:"dataSchema"`
		BinarySchema int            `json:"binarySchema"`
		Env          string         `json:"env"`
		Cluster      string         `json:"cluster"`
		History      []HistoryEntry `json:"history"`
		Backups      []struct {
			Name string `json:"name"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(v.out.Bytes(), &got); err != nil {
		t.Fatalf("不是合法 JSON: %v\n%s", err, v.out.String())
	}
	if !got.Installed || got.Current != "1.1.0" || strings.Join(got.Releases, ",") != "1.1.0,1.0.0" || got.Service != "active" ||
		got.DataSchema == nil || *got.DataSchema != 2 || got.BinarySchema != 2 || got.Env != "staging" || got.Cluster != "prod-tidb-01" {
		t.Errorf("status = %+v", got)
	}
	if len(got.History) != 2 || got.History[1].Action != "upgrade" || len(got.Backups) != 1 {
		t.Errorf("history/backups = %+v %+v", got.History, got.Backups)
	}
	// 文本输出
	v.out.Reset()
	_ = v.e.PrintStatus(s, false)
	mustContain(t, "文本输出", v.out.String(), "当前版本：1.1.0", "服务状态：active", "数据 schema：2", "upgrade", "可用备份")
	// 只保留最近 5 条历史
	for i := 0; i < 8; i++ {
		appendHistory(v.l.History(), HistoryEntry{Time: time.Now(), Action: "rollback", From: "a", To: "b"})
	}
	s, _ = v.e.Status(ctxBG())
	if len(s.History) != 5 {
		t.Errorf("历史应只取最近 5 条: %d", len(s.History))
	}
	// 服务状态未知
	v.sys.onRun = func(name string, args []string) (bool, []byte, error) {
		if name == "systemctl" {
			return true, nil, os.ErrNotExist
		}
		return false, nil, nil
	}
	if s, _ := v.e.Status(ctxBG()); s.Service != "unknown" {
		t.Errorf("systemctl 失败时应显示 unknown: %q", s.Service)
	}
}

// ---- 单元与版本 ----

func TestSemver(t *testing.T) {
	less := [][2]string{{"1.0.0", "1.0.1"}, {"1.9.0", "1.10.0"}, {"1.0.0-rc.1", "1.0.0"}, {"1.0.0-rc.1", "1.0.0-rc.2"}, {"1.0.0-rc.2", "1.0.0-rc.10"},
		{"1.0.0-alpha", "1.0.0-alpha.1"}, {"1.0.0-1", "1.0.0-alpha"}, {"0.9.9", "1.0.0"}, {"v1.0.0", "1.0.1"}}
	for _, p := range less {
		a, err1 := ParseVersion(p[0])
		b, err2 := ParseVersion(p[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("解析 %v: %v %v", p, err1, err2)
		}
		if a.Compare(b) >= 0 || b.Compare(a) <= 0 {
			t.Errorf("%s 应小于 %s", p[0], p[1])
		}
	}
	if a, _ := ParseVersion("v1.2.3"); a.String() != "1.2.3" || a.Compare(a) != 0 {
		t.Error("v 前缀应被规范化")
	}
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.3+build", "../1.0.0", "1.0.0/../x", "a.b.c", "1.2.3-"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q 应不合法", bad)
		}
	}
}

func TestRenderUnitAndSudoers(t *testing.T) {
	p := UnitParams{Env: "prod", Cluster: "c1", TidbUser: "tidb", Listen: "0.0.0.0:8080"}
	u, err := RenderUnit(tmplUnit, p)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "unit", u, "ExecStart=/opt/tidbops/current/bin/tidbops-server \\", "--cluster c1", "--env prod", "[Install]", "ReadWritePaths=/var/lib/tidbops")
	// 渲染是幂等的：对输出再解析 / 渲染得到同样的结果
	pp, _ := ParseUnit(u)
	u2, _ := RenderUnit(u, pp)
	if u != u2 {
		t.Errorf("渲染不幂等:\n%s\n---\n%s", u, u2)
	}
	for _, bad := range []UnitParams{
		{Env: "prod", Cluster: "c1 --evil", TidbUser: "tidb", Listen: "127.0.0.1:8080"},
		{Env: "prod", Cluster: "c1", TidbUser: "tidb\nExecStartPre=/bin/sh", Listen: "127.0.0.1:8080"},
		{Env: "prod", Cluster: "c1", TidbUser: "tidb", Listen: "127.0.0.1:8080 --x"},
		{Env: "prod", Cluster: "c1", TidbUser: "tidb", Listen: "127.0.0.1:8080", PublicURL: "https://a.com/$(id)"},
		{Env: "prod", Cluster: "c1", TidbUser: "tidb", Listen: "127.0.0.1:8080", PublicURL: "https://a.com/%n"},
		{Env: "../prod", Cluster: "c1", TidbUser: "tidb", Listen: "127.0.0.1:8080"},
	} {
		if _, err := RenderUnit(tmplUnit, bad); err == nil {
			t.Errorf("应拒绝 %+v", bad)
		}
	}
	if _, err := RenderSudoers(tmplSudoers, "tidb;rm"); err == nil {
		t.Error("非法用户名应拒绝")
	}
	if _, err := RenderSudoers("tidbops ALL=(tidb) NOPASSWD: /bin/sh\n", "tidb"); err == nil {
		t.Error("模板命令行不符合预期应拒绝")
	}
	if _, err := RenderSudoers("tidbops ALL=(tidb) NOPASSWD: /opt/tidbops/bin/tidbops-server exec --stdin\n", "tidb"); err == nil {
		t.Error("缺少 shell-exec 入口的模板应拒绝")
	}
	if _, err := RenderExecJSON("{bad", "c1"); err == nil {
		t.Error("非法 JSON 模板应拒绝")
	}
	for in, want := range map[string]string{"127.0.0.1:8080": "http://127.0.0.1:8080/api/health", "0.0.0.0:9": "http://127.0.0.1:9/api/health", ":8080": "http://127.0.0.1:8080/api/health", "[::]:80": "http://127.0.0.1:80/api/health", "10.0.0.1:81": "http://10.0.0.1:81/api/health"} {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q) = %q", in, got)
		}
	}
}

// 与仓库里真实的 deploy/ops 模板保持兼容。
func TestRealTemplatesRender(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "deploy", "ops")
	ub, err1 := os.ReadFile(filepath.Join(dir, "tidbops.service"))
	sb, err2 := os.ReadFile(filepath.Join(dir, "sudoers.tidbops"))
	eb, err3 := os.ReadFile(filepath.Join(dir, "exec.json.example"))
	if err1 != nil || err2 != nil || err3 != nil {
		t.Skip("找不到 deploy/ops 模板，跳过")
	}
	u, err := RenderUnit(string(ub), UnitParams{Env: "staging", Cluster: "c1", TidbUser: "dbuser", Listen: "127.0.0.1:8080", PublicURL: "https://ops.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "真实 unit", u, "/opt/tidbops/current/bin/tidbops-server", "--cluster c1", "--env staging", "--public-url https://ops.example.com", "--exec-sudo-user dbuser", "ProtectSystem=strict")
	mustNotContain(t, "真实 unit", u, "/opt/tidbops/bin/", "/opt/tidbops/web ")
	s, err := RenderSudoers(string(sb), "dbuser")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "真实 sudoers", s, "tidbops ALL=(dbuser) NOPASSWD: /opt/tidbops/current/bin/tidbops-server exec --stdin",
		"tidbops ALL=(dbuser) NOPASSWD: /opt/tidbops/current/bin/tidbops-server shell-exec --stdin")
	mustNotContain(t, "真实 sudoers", s, "(tidb)")
	mustContain(t, "真实 unit", u, "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK")
	if _, err := RenderExecJSON(string(eb), "c1"); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTreeRejectsSymlinkAndSpecials(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Skip("当前环境不能创建符号链接")
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err == nil {
		t.Error("包里有符号链接必须拒绝")
	}
	// 经 VerifyDir 同样拒绝
	if _, err := BuildFileList(src); err == nil {
		t.Error("BuildFileList 应拒绝符号链接")
	}
}

func TestSafeTarName(t *testing.T) {
	ok := []string{"a", "a/b", "a/b/", "VERSION", "dir/file.json", "."}
	for _, n := range ok {
		if _, err := safeTarName(n); err != nil {
			t.Errorf("%q 应合法: %v", n, err)
		}
	}
	for _, n := range []string{"", "/a", "../a", "a/../b", "a/..", `a\b`, "C:/x", "a\x00b"} {
		if _, err := safeTarName(n); err == nil {
			t.Errorf("%q 应被拒绝", n)
		}
	}
}

func TestLockReleasedAfterBackup(t *testing.T) {
	v := newEnv(t)
	v.install("1.0.0", pkgOpt{})
	_ = v.e.Backup(ctxBG(), "")
	if exists(v.l.Lock()) {
		t.Error("锁应释放")
	}
}

// ---- CLI ----

func TestCLI(t *testing.T) {
	v := newEnv(t)
	mk := func(root string) *Engine { return v.e }
	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := Run(ctxBG(), args, &o, &e, mk)
		return code, o.String(), e.String()
	}
	if code, out, _ := run("help"); code != 0 || !strings.Contains(out, "install") || !strings.Contains(out, "rollback") {
		t.Errorf("help: %d %s", code, out)
	}
	if code, _, errs := run("nope"); code != 2 || !strings.Contains(errs, "未知子命令") {
		t.Errorf("未知子命令: %d %s", code, errs)
	}
	if code, _, _ := run(); code != 2 {
		t.Error("无参数应返回 2")
	}
	for _, sub := range []string{"install", "upgrade", "rollback", "status", "migrate", "backup", "restore", "uninstall"} {
		code, out, _ := run(sub, "-h")
		if code != 0 || !strings.Contains(out, "用法: tidbops-server admin "+sub) {
			t.Errorf("%s -h: %d %s", sub, code, out)
		}
	}
	if _, out, _ := run("install", "-h"); !strings.Contains(out, "--dry-run") || !strings.Contains(out, "tiup 集群名") {
		t.Errorf("install -h 应有中文参数说明: %s", out)
	}
	if code, _, _ := run("install", "--bogus"); code != 2 {
		t.Error("未知参数应返回 2")
	}
	if code, _, errs := run("status", "extra"); code != 2 || !strings.Contains(errs, "多余") {
		t.Errorf("多余参数: %d %s", code, errs)
	}

	pkg := v.makePkg("1.0.0", pkgOpt{})
	// install --dry-run（不需要 root）
	v.sys.euid = 1000
	if code, out, errs := run("install", "--package", pkg, "--cluster", "c1", "--dry-run"); code != 0 || !strings.Contains(out, "[dry-run]") {
		t.Errorf("install --dry-run: %d %s %s", code, out, errs)
	}
	// 非 root 真正安装：给出清晰提示，退出码 1
	if code, _, errs := run("install", "--package", pkg, "--cluster", "c1"); code != 1 || !strings.Contains(errs, "root") {
		t.Errorf("非 root: %d %s", code, errs)
	}
	v.sys.euid = 0
	if code, _, errs := run("install", "--package", pkg, "--cluster", "c1", "--env", "staging"); code != 0 {
		t.Fatalf("install: %d %s", code, errs)
	}
	if code, out, _ := run("status", "--json"); code != 0 || !strings.Contains(out, `"current": "1.0.0"`) {
		t.Errorf("status --json: %d %s", code, out)
	}
	if code, _, errs := run("install", "--package", pkg, "--cluster", "c1"); code != 1 || !strings.Contains(errs, "admin upgrade") {
		t.Errorf("重复安装: %d %s", code, errs)
	}
	pkg2 := v.makePkg("1.1.0", pkgOpt{})
	if code, _, errs := run("upgrade", "--package", pkg2, "--timeout", "10s"); code != 0 {
		t.Fatalf("upgrade: %d %s", code, errs)
	}
	if code, _, errs := run("rollback"); code != 0 {
		t.Fatalf("rollback: %d %s", code, errs)
	}
	if v.cur() != "1.0.0" {
		t.Error("CLI 回滚后应为 1.0.0")
	}
	if code, _, errs := run("uninstall", "--purge-data"); code != 1 || !strings.Contains(errs, "--yes") {
		t.Errorf("purge 无 --yes: %d %s", code, errs)
	}
}

func TestCLIMigrate(t *testing.T) {
	v := newEnv(t)
	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := Run(ctxBG(), args, &o, &e, func(string) *Engine { return v.e })
		return code, o.String(), e.String()
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "users.json"), []byte("legacy"), 0o600)
	if code, _, errs := run("migrate"); code != 2 || !strings.Contains(errs, "--data-dir") {
		t.Errorf("缺参数: %d %s", code, errs)
	}
	code, out, errs := run("migrate", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "0 → 1") {
		t.Fatalf("迁移: %d %s %s", code, out, errs)
	}
	if in, err := dataver.Read(dir); err != nil || in.Schema != dataver.Schema {
		t.Errorf("VERSION = %+v %v", in, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "users.json")); string(b) != "legacy" {
		t.Error("0→1 不应改任何业务文件")
	}
	// 幂等
	code, out, _ = run("migrate", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, "已是最新") {
		t.Errorf("幂等: %d %s", code, out)
	}
	// schema 过高 → 非 0
	_ = dataver.Write(dir, dataver.Info{Schema: dataver.Schema + 1, Version: "9.9.9"})
	code, _, errs = run("migrate", "--data-dir", dir)
	if code == 0 || !strings.Contains(errs, "数据目录由更新版本创建") {
		t.Errorf("schema 过高应失败: %d %s", code, errs)
	}
	if code, _, _ := run("migrate", "--data-dir", filepath.Join(dir, "nonexistent")); code == 0 {
		t.Error("数据目录不存在应失败")
	}
}

func TestPackageRootFromExe(t *testing.T) {
	v := newEnv(t)
	pkg := v.makePkg("1.0.0", pkgOpt{})
	root, err := PackageRootFromExe(filepath.Join(pkg, "bin", "tidbops-server"))
	if err != nil || root != evalPath(t, pkg) {
		t.Errorf("root = %q %v（期望 %q）", root, err, pkg)
	}
	// 不在包内
	d := t.TempDir()
	_ = os.MkdirAll(filepath.Join(d, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "bin", "tidbops-server"), []byte("x"), 0o755)
	if _, err := PackageRootFromExe(filepath.Join(d, "bin", "tidbops-server")); err == nil || !strings.Contains(err.Error(), "--package") {
		t.Errorf("不在包内应提示 --package: %v", err)
	}
	// resolvePackage：默认取自可执行文件
	old := executablePath
	defer func() { executablePath = old }()
	executablePath = func() (string, error) { return filepath.Join(pkg, "bin", "tidbops-server"), nil }
	if got, err := resolvePackage(""); err != nil || got != evalPath(t, pkg) {
		t.Errorf("resolvePackage = %q %v", got, err)
	}
	if got, err := resolvePackage(pkg); err != nil || got != pkg {
		t.Errorf("显式 --package: %q %v", got, err)
	}
}

func TestMainUsage(t *testing.T) {
	var o, e bytes.Buffer
	if code := Main(nil, &o, &e); code != 2 || !strings.Contains(e.String(), "用法") {
		t.Errorf("Main(nil) = %d %s", code, e.String())
	}
}

func evalPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 真实模板默认带 --allow-remote，安装时给 0.0.0.0:8018 就能直接起来；升级（解析已装单元再渲染）不丢它。
func TestRealTemplateAllowRemote(t *testing.T) {
	ub, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "ops", "tidbops.service"))
	if err != nil {
		t.Skip("找不到 deploy/ops 模板，跳过")
	}
	for name, p := range map[string]UnitParams{
		"同机":  {Env: "prod", Cluster: "c1", TidbUser: "tidb", Listen: "0.0.0.0:8018"},
		"独立":  {Env: "prod", TidbUser: "tidb", Listen: "0.0.0.0:8018", Standalone: true},
		"中控机": {Env: "prod", TidbUser: "tidb", Listen: "0.0.0.0:8018", Standalone: true, LocalTiup: true},
	} {
		u, err := RenderUnit(string(ub), p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mustContain(t, "unit "+name, u, "--listen 0.0.0.0:8018", "--allow-remote")
		mustNotContain(t, "unit "+name, u, "127.0.0.1:8080")
		pp, err := ParseUnit(u)
		if err != nil || pp.Listen != "0.0.0.0:8018" {
			t.Fatalf("%s: ParseUnit 应还原监听地址: %+v %v", name, pp, err)
		}
		u2, err := RenderUnit(string(ub), pp)
		if err != nil || u2 != u {
			t.Errorf("%s: 升级（解析再渲染）应得到相同的单元: %v", name, err)
		}
	}
}
