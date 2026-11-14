// @author: kevinkelly
// @license: Apache License 2.0
// @file: helpers_test.go
// @time: 2026/10/28

package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tidbops/internal/dataver"
)

// ---- 包内模板（与 deploy/ops 下的真实文件一致）----

const tmplUnit = `# TiDB Ops 平台（Web 进程）。
[Unit]
Description=TiDB Ops platform
After=network-online.target
Wants=network-online.target

[Service]
User=tidbops
Group=tidbops
# 生产：监听本机
ExecStart=/opt/tidbops/bin/tidbops-server \
  --web /opt/tidbops/web \
  --listen 127.0.0.1:8080 \
  --data-dir /var/lib/tidbops \
  --env prod \
  --exec sudo --exec-sudo-user tidb --exec-bin /opt/tidbops/bin/tidbops-server \
  --exec-policy /etc/tidb-ops/exec.json --key-file /etc/tidb-ops/exec.key \
  --home /home/tidb --mysql-user tidbops_ro
EnvironmentFile=-/etc/tidb-ops/env
Restart=on-failure
ReadWritePaths=/var/lib/tidbops

[Install]
WantedBy=multi-user.target
`

const tmplSudoers = `# /etc/sudoers.d/tidbops
Defaults:tidbops !requiretty
tidbops ALL=(tidb) NOPASSWD: /opt/tidbops/bin/tidbops-server exec --stdin
tidbops ALL=(tidb) NOPASSWD: /opt/tidbops/bin/tidbops-server shell-exec --stdin
`

const tmplExecJSON = `{
  "clusters": ["prod-tidb-01"],
  "tiupBin": "/home/tidb/.tiup/bin/tiup",
  "mysqlUser": "tidbops_ops"
}
`

// ---- 假 Sys ----

type failSpec struct {
	err   error
	times int // <0 表示一直失败
}

type fakeSys struct {
	mu      sync.Mutex
	calls   []string
	chowns  []string
	euid    int
	clock   time.Time
	active  bool
	users   map[string]bool
	fail    map[string]*failSpec // key：命令行前缀
	onRun   func(name string, args []string) (bool, []byte, error)
	free    uint64
	freeErr error
	noLook  map[string]bool
}

func newFakeSys() *fakeSys {
	return &fakeSys{euid: 0, clock: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), users: map[string]bool{"tidb": true},
		fail: map[string]*failSpec{}, free: 1 << 40, noLook: map[string]bool{}}
}

func (s *fakeSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.calls = append(s.calls, line)
	for prefix, f := range s.fail {
		if strings.HasPrefix(line, prefix) && f.times != 0 {
			if f.times > 0 {
				f.times--
			}
			return nil, f.err
		}
	}
	if s.onRun != nil {
		if ok, out, err := s.onRun(name, args); ok {
			return out, err
		}
	}
	switch name {
	case "systemctl":
		switch args[0] {
		case "is-active":
			if s.active {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		case "start":
			s.active = true
		case "stop":
			s.active = false
		case "enable":
			for _, a := range args {
				if a == "--now" {
					s.active = true
				}
			}
		}
	case "id":
		if !s.users[args[0]] {
			return nil, errors.New("id: no such user")
		}
	case "useradd":
		s.users[args[len(args)-1]] = true
	}
	return nil, nil
}

func (s *fakeSys) LookPath(name string) (string, error) {
	if s.noLook[name] {
		return "", errors.New("not found")
	}
	return "/usr/sbin/" + name, nil
}
func (s *fakeSys) Geteuid() int { return s.euid }
func (s *fakeSys) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.clock
	s.clock = s.clock.Add(time.Second)
	return t
}
func (s *fakeSys) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.clock = s.clock.Add(d)
	s.mu.Unlock()
	return ctx.Err()
}
func (s *fakeSys) Chown(path, user, group string, recursive bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chowns = append(s.chowns, fmt.Sprintf("%s %s:%s rec=%v", strings.TrimSuffix(filepath.Base(path), ".tmp"), user, group, recursive))
	return nil
}
func (s *fakeSys) FreeBytes(path string) (uint64, error) { return s.free, s.freeErr }

func (s *fakeSys) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *fakeSys) called(prefix string) bool { return s.indexOf(prefix) >= 0 }
func (s *fakeSys) indexOf(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}
func (s *fakeSys) countCalls(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
func (s *fakeSys) failOn(prefix string, times int) {
	s.mu.Lock()
	s.fail[prefix] = &failSpec{err: errors.New("injected failure: " + prefix), times: times}
	s.mu.Unlock()
}

// ---- 测试环境 ----

type tenv struct {
	t         *testing.T
	root      string
	l         Layout
	sys       *fakeSys
	e         *Engine
	out       *bytes.Buffer
	pkgs      string
	healthBad map[string]bool // 版本 → 服务「起不来 / 不健康」
	migFail   error           // 迁移命令注入失败（失败前已改动数据，模拟半途失败）
	adminErr  error           // 创建初始管理员的命令注入失败
	admin2Err error           // 只让第二名管理员 admin2 的创建失败
}

func newEnv(t *testing.T) *tenv {
	t.Helper()
	v := &tenv{t: t, root: t.TempDir(), sys: newFakeSys(), out: &bytes.Buffer{}, pkgs: t.TempDir(), healthBad: map[string]bool{}}
	v.l = Layout{Root: v.root}
	v.e = &Engine{L: v.l, Sys: v.sys, Out: v.out, OS: "linux", Arch: "amd64", Health: v.health,
		StopTimeout: 5 * time.Second, HealthTimeout: 8 * time.Second, HealthInterval: time.Second, Operator: "tester"}
	v.e.defaults()
	v.sys.onRun = v.onRun
	return v
}

// health 模拟真实服务：服务在运行时返回 current 链接指向版本的版本号。
func (v *tenv) health(ctx context.Context, url string) (string, error) {
	if !v.sys.isActive() {
		return "", errors.New("connection refused")
	}
	cur, err := v.e.currentVersion()
	if err != nil {
		return "", err
	}
	if v.healthBad[cur] {
		return "", errors.New("HTTP 503")
	}
	return cur, nil
}

// onRun 模拟 `<release>/bin/tidbops-server admin migrate --data-dir DIR`：读该 release 的清单，把数据迁移到它的 schema，
// 并改写 users.json 以便测试能区分「迁移前 / 迁移后」的数据。
func (v *tenv) onRun(name string, args []string) (bool, []byte, error) {
	// 模拟 `runuser -s /bin/sh tidbops -c "<bin> user add ... --gen-password"`：打印一行初始口令
	if name == "runuser" && len(args) >= 5 && strings.Contains(args[4], "user add") {
		if v.adminErr != nil {
			return true, []byte("boom\n"), v.adminErr
		}
		if strings.Contains(args[4], "--name admin2") {
			if v.admin2Err != nil {
				return true, []byte("boom\n"), v.admin2Err
			}
			return true, []byte("some noise\n" + AdminPasswordPrefix + testPW2 + "\n"), nil
		}
		return true, []byte("some noise\n" + AdminPasswordPrefix + testPW + "\n"), nil
	}
	if !strings.HasSuffix(filepath.ToSlash(name), "/bin/tidbops-server") || len(args) < 4 || args[0] != "admin" || args[1] != "migrate" {
		return false, nil, nil
	}
	rel := filepath.Dir(filepath.Dir(name))
	m, err := ReadManifest(rel)
	if err != nil {
		return true, nil, err
	}
	dir := args[3]
	cur, _ := dataver.Read(dir)
	if cur.Schema < m.SchemaVersion {
		b, _ := os.ReadFile(filepath.Join(dir, "users.json"))
		_ = os.WriteFile(filepath.Join(dir, "users.json"), append(b, []byte("|migrated-to-"+fmt.Sprint(m.SchemaVersion))...), 0o600)
		if v.migFail != nil {
			return true, []byte("migrating...\n"), v.migFail
		}
		_ = dataver.Write(dir, dataver.Info{Schema: m.SchemaVersion, Version: m.Version})
	}
	return true, []byte("migrate ok\n"), nil
}

type pkgOpt struct {
	schema  int
	minFrom string
	unit    string // 覆盖 systemd 模板
	osName  string
	arch    string
	extra   map[string]string
}

// makePkg 现场构造一个最小发布包目录并生成清单，返回包根目录。
func (v *tenv) makePkg(version string, o pkgOpt) string {
	v.t.Helper()
	if o.schema == 0 {
		o.schema = 1
	}
	if o.unit == "" {
		o.unit = tmplUnit
	}
	if o.osName == "" {
		o.osName = "linux"
	}
	if o.arch == "" {
		o.arch = "amd64"
	}
	root := filepath.Join(v.pkgs, "tidbops-"+version+"-"+o.osName+"-"+o.arch)
	files := map[string]string{
		"bin/tidbops-server":           "fake-binary-" + version,
		"web/index.html":               "<html>" + version + "</html>",
		"web/app.js":                   "console.log(1)",
		"deploy/ops/tidbops.service":   o.unit,
		"deploy/ops/sudoers.tidbops":   tmplSudoers,
		"deploy/ops/exec.json.example": tmplExecJSON,
		"VERSION":                      version + "\n",
		"CHANGELOG.md":                 "# " + version,
	}
	for k, c := range o.extra {
		files[k] = c
	}
	for rel, c := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			v.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if rel == "bin/tidbops-server" {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(c), mode); err != nil {
			v.t.Fatal(err)
		}
	}
	fl, err := BuildFileList(root)
	if err != nil {
		v.t.Fatal(err)
	}
	m := Manifest{Product: Product, Version: version, BuildTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), GoVersion: "go1.22", OS: o.osName, Arch: o.arch,
		SchemaVersion: o.schema, MinFrom: o.minFrom, Files: fl}
	if err := WriteManifest(root, m); err != nil {
		v.t.Fatal(err)
	}
	return root
}

func (v *tenv) install(version string, o pkgOpt) string {
	v.t.Helper()
	pkg := v.makePkg(version, o)
	err := v.e.Install(context.Background(), InstallOpts{Package: pkg, Cluster: "prod-tidb-01", Env: "staging", TidbUser: "tidb", Listen: "127.0.0.1:9090", Start: true})
	if err != nil {
		v.t.Fatalf("install %s: %v\n%s", version, err, v.out.String())
	}
	return pkg
}

func (v *tenv) upgrade(version string, o pkgOpt, uo UpgradeOpts) (string, error) {
	v.t.Helper()
	pkg := v.makePkg(version, o)
	uo.Package = pkg
	v.out.Reset()
	return pkg, v.e.Upgrade(context.Background(), uo)
}

func (v *tenv) seedData(content string) {
	v.t.Helper()
	if err := os.MkdirAll(v.l.Data(), 0o700); err != nil {
		v.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.l.Data(), "users.json"), []byte(content), 0o600); err != nil {
		v.t.Fatal(err)
	}
}

func (v *tenv) readData(name string) string {
	b, err := os.ReadFile(filepath.Join(v.l.Data(), name))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (v *tenv) cur() string {
	c, err := v.e.currentVersion()
	if err != nil {
		return ""
	}
	return c
}

func (v *tenv) history() []HistoryEntry {
	h, _ := readHistory(v.l.History())
	return h
}

func (v *tenv) dumpOut() string { return v.out.String() }

func mustContain(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s 应包含 %q，实际:\n%s", what, sub, s)
		}
	}
}

func mustNotContain(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("%s 不应包含 %q，实际:\n%s", what, sub, s)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func ctxBG() context.Context { return context.Background() }
