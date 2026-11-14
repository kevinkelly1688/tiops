// @author: kevinkelly
// @license: Apache License 2.0
// @file: engine.go
// @time: 2026/10/28

package release

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"tidbops/internal/dataver"
)

// Engine 是部署 / 升级 / 回滚引擎。所有磁盘路径经 Layout，所有系统副作用经 Sys，健康检查经 Health，因此可在临时目录里完整测试。
type Engine struct {
	L      Layout
	Sys    Sys
	Health HealthFunc
	Out    io.Writer
	Rand   io.Reader // 生成密钥用，默认 crypto/rand

	OS, Arch string // 本机 OS / 架构（默认 runtime.GOOS / GOARCH；测试可注入）

	Operator string // 操作人（SUDO_USER）

	StopTimeout    time.Duration // 等待服务退出，默认 30s
	HealthTimeout  time.Duration // 健康检查总超时，默认 60s
	HealthInterval time.Duration // 轮询间隔，默认 1s
}

// NewEngine 创建使用真实系统的引擎。
func NewEngine(l Layout, out io.Writer) *Engine {
	e := &Engine{L: l, Sys: RealSys{}, Health: HTTPHealth, Out: out}
	e.defaults()
	return e
}

func (e *Engine) defaults() {
	if e.Sys == nil {
		e.Sys = RealSys{}
	}
	if e.Health == nil {
		e.Health = HTTPHealth
	}
	if e.Out == nil {
		e.Out = io.Discard
	}
	if e.Rand == nil {
		e.Rand = rand.Reader
	}
	if e.OS == "" {
		e.OS = hostOS()
	}
	if e.Arch == "" {
		e.Arch = hostArch()
	}
	if e.StopTimeout == 0 {
		e.StopTimeout = 30 * time.Second
	}
	if e.HealthTimeout == 0 {
		e.HealthTimeout = 60 * time.Second
	}
	if e.HealthInterval == 0 {
		e.HealthInterval = time.Second
	}
}

func (e *Engine) printf(format string, a ...any) { fmt.Fprintf(e.Out, format, a...) }

// requireRoot 要求 root（写操作）。
func (e *Engine) requireRoot() error {
	if e.Sys.Geteuid() != 0 {
		return errors.New("需要 root 权限：请用 sudo 运行（只读的 status 与 --dry-run 不需要）")
	}
	return nil
}

// ---- 步骤执行（dry-run 只打印）----

type step struct {
	name string
	fn   func() error
}

func (e *Engine) runSteps(dry bool, steps []step) error {
	for i, s := range steps {
		if dry {
			e.printf("[dry-run] %d/%d %s\n", i+1, len(steps), s.name)
			continue
		}
		e.printf("== %d/%d %s ==\n", i+1, len(steps), s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// ---- 锁 ----

// lock 用 O_EXCL 创建锁文件；返回的函数负责删除。已存在则拒绝。
func (e *Engine) lock() (func(), error) {
	p := e.L.Lock()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			b, _ := os.ReadFile(p)
			return nil, fmt.Errorf("锁文件 %s 已存在（%s）：可能有另一个 admin 部署 / 升级 / 回滚正在进行；若确认没有（例如上次被强行终止），手动删除该文件后重试", p, strings.TrimSpace(string(b)))
		}
		return nil, err
	}
	fmt.Fprintf(f, "pid=%d operator=%s time=%s\n", os.Getpid(), e.Operator, e.Sys.Now().Format(time.RFC3339))
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}

// ---- 版本 / 包 ----

// currentVersion 返回 current 链接指向的版本号；未安装返回 ("", fs.ErrNotExist)。
func (e *Engine) currentVersion() (string, error) {
	t, err := readLink(e.L.Current())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fs.ErrNotExist
		}
		return "", fmt.Errorf("读取 %s 失败: %w", e.L.Current(), err)
	}
	v := filepath.Base(filepath.ToSlash(t))
	if _, err := ParseVersion(v); err != nil {
		return "", fmt.Errorf("current 链接指向的目录名 %q 不是版本号", t)
	}
	return v, nil
}

// releaseManifest 读取已安装 release 的清单。
func (e *Engine) releaseManifest(v string) (Manifest, error) {
	return ReadManifest(e.L.ReleaseDir(v))
}

// listReleases 返回 releases 目录下的版本，按版本号从大到小。
func (e *Engine) listReleases() ([]string, error) {
	ents, err := os.ReadDir(e.L.Releases())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var vs []Version
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		if v, err := ParseVersion(d.Name()); err == nil && v.String() == d.Name() {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Compare(vs[j]) > 0 })
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.String()
	}
	return out, nil
}

// loadPackage 读取并校验发布包：清单、版本号格式、OS / 架构、逐文件哈希、必要文件齐全。
func (e *Engine) loadPackage(dir string) (Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return m, err
	}
	v, err := ParseVersion(m.Version)
	if err != nil || v.String() != m.Version {
		return m, fmt.Errorf("发布包版本号 %q 不合法（不能带 v 前缀或构建元数据）", m.Version)
	}
	if m.SchemaVersion < 0 {
		return m, fmt.Errorf("发布包 schemaVersion 不合法：%d", m.SchemaVersion)
	}
	if m.MinFrom != "" {
		if _, err := ParseVersion(m.MinFrom); err != nil {
			return m, fmt.Errorf("发布包 minFrom 不合法: %w", err)
		}
	}
	if m.OS != e.OS || m.Arch != e.Arch {
		return m, fmt.Errorf("发布包是 %s/%s 版，本机是 %s/%s", m.OS, m.Arch, e.OS, e.Arch)
	}
	if err := VerifyDir(dir, m); err != nil {
		return m, fmt.Errorf("发布包校验失败（包可能被篡改或损坏）：%w", err)
	}
	for _, need := range []string{pkgBin, pkgUnit, pkgSudoers, pkgExecJSON} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(need))); err != nil {
			return m, fmt.Errorf("发布包缺少必要文件 %s", need)
		}
	}
	return m, nil
}

// installRelease 把包整体复制到 releases/<版本>/（先复制到 .partial 并校验，再 rename）。
// 目录已存在：校验与清单一致则复用；不一致时 replace 为真才替换，否则报错。返回是否复用。
func (e *Engine) installRelease(pkg string, m Manifest, replace bool) (reused bool, err error) {
	dst := e.L.ReleaseDir(m.Version)
	pm, err := os.ReadFile(filepath.Join(pkg, ManifestName))
	if err != nil {
		return false, err
	}
	if _, serr := os.Lstat(dst); serr == nil {
		dm, rerr := os.ReadFile(filepath.Join(dst, ManifestName))
		if rerr == nil && string(dm) == string(pm) && VerifyDir(dst, m) == nil {
			return true, nil
		}
		if !replace {
			return false, fmt.Errorf("%s 已存在但内容与发布包不一致（可能被改动）：请人工核对后删除该目录再重试", dst)
		}
		if err := os.RemoveAll(dst); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(e.L.Releases(), 0o755); err != nil {
		return false, err
	}
	tmp := dst + ".partial"
	_ = os.RemoveAll(tmp)
	if err := copyTree(pkg, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return false, err
	}
	if err := VerifyDir(tmp, m); err != nil {
		_ = os.RemoveAll(tmp)
		return false, fmt.Errorf("复制后校验失败: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return false, err
	}
	return false, nil
}

// switchCurrent 原子切换 current 链接到 releases/<版本>（相对链接）。
func (e *Engine) switchCurrent(version string) error {
	if _, err := os.Stat(e.L.ReleaseDir(version)); err != nil {
		return fmt.Errorf("release %s 不存在: %w", version, err)
	}
	return setLink(e.L.Current(), "releases/"+version)
}

// ---- 配置文件 ----

// installSudoers 写临时文件 → visudo -cf 校验 → 以 0440 安装。校验失败则中止且不留任何文件。
func (e *Engine) installSudoers(ctx context.Context, content string) error {
	dir := e.L.SudoersDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tidbops.sudoers.new") // sudo 忽略以 . 开头的文件
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(content), 0o440); err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if err := os.Chmod(tmp, 0o440); err != nil {
		cleanup()
		return err
	}
	if err := e.Sys.Chown(tmp, "root", "root", false); err != nil {
		cleanup()
		return err
	}
	if out, err := e.Sys.Run(ctx, "visudo", "-cf", tmp); err != nil {
		cleanup()
		return fmt.Errorf("sudoers 校验未通过，已中止（未改动 %s）：%w %s", e.L.Sudoers(), err, strings.TrimSpace(string(out)))
	}
	if runtime.GOOS == "windows" { // 开发机：Windows 不能覆盖只读文件
		_ = os.Remove(e.L.Sudoers())
	}
	if err := os.Rename(tmp, e.L.Sudoers()); err != nil {
		cleanup()
		return err
	}
	return nil
}

// syncConfigs 以 release 包内的模板重新生成 systemd 单元与 sudoers；与已安装的不同才写入。
// 参数 p 通常来自现有单元（保留 --env / 集群名 / listen 等）。回滚到旧版本时同样用旧版本的模板，
// 避免新版本单元里的新参数让旧二进制起不来。
func (e *Engine) syncConfigs(ctx context.Context, relDir string, p UnitParams) (unitChanged, sudoersChanged bool, err error) {
	ub, err := os.ReadFile(filepath.Join(relDir, filepath.FromSlash(pkgUnit)))
	if err != nil {
		return false, false, err
	}
	unit, err := RenderUnit(string(ub), p)
	if err != nil {
		return false, false, err
	}
	if p.UsesExec() { // 纯独立部署没有 sudo 执行器，也就没有 sudoers；中控机模式有
		sb, err := os.ReadFile(filepath.Join(relDir, filepath.FromSlash(pkgSudoers)))
		if err != nil {
			return false, false, err
		}
		sud, err := RenderSudoers(string(sb), p.TidbUser)
		if err != nil {
			return false, false, err
		}
		if cur, rerr := os.ReadFile(e.L.Sudoers()); rerr != nil || string(cur) != sud {
			if err := e.installSudoers(ctx, sud); err != nil {
				return false, false, err
			}
			sudoersChanged = true
		}
	}
	if cur, rerr := os.ReadFile(e.L.Unit()); rerr != nil || string(cur) != unit {
		if err := os.MkdirAll(filepath.Dir(e.L.Unit()), 0o755); err != nil {
			return false, sudoersChanged, err
		}
		if err := writeFileAtomic(e.L.Unit(), []byte(unit), 0o644); err != nil {
			return false, sudoersChanged, err
		}
		unitChanged = true
	}
	return unitChanged, sudoersChanged, nil
}

// installedParams 从已安装的单元里还原安装参数。
func (e *Engine) installedParams() (UnitParams, error) {
	b, err := os.ReadFile(e.L.Unit())
	if err != nil {
		return UnitParams{}, fmt.Errorf("读取 %s 失败（安装不完整？可用 admin install --force 修复）: %w", e.L.Unit(), err)
	}
	p, err := ParseUnit(string(b))
	if err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, fmt.Errorf("现有 systemd 单元里的参数不合法: %w", err)
	}
	return p, nil
}

// ---- 服务 ----

func (e *Engine) serviceActive(ctx context.Context) (bool, string) {
	out, err := e.Sys.Run(ctx, "systemctl", "is-active", ServiceName)
	st := strings.TrimSpace(string(out))
	if err != nil {
		if st == "" || strings.Contains(st, "\n") {
			return false, "unknown"
		}
		return false, st
	}
	return st == "active" || st == "activating", st
}

// stopService 停止服务并等待其退出。
func (e *Engine) stopService(ctx context.Context) error {
	if _, err := e.Sys.Run(ctx, "systemctl", "stop", ServiceName); err != nil {
		return fmt.Errorf("systemctl stop 失败: %w", err)
	}
	deadline := e.Sys.Now().Add(e.StopTimeout)
	for {
		out, err := e.Sys.Run(ctx, "systemctl", "is-active", ServiceName)
		st := strings.TrimSpace(string(out))
		if err != nil || (st != "active" && st != "activating" && st != "deactivating") {
			return nil
		}
		if !e.Sys.Now().Before(deadline) {
			return fmt.Errorf("等待服务退出超时（%s）", e.StopTimeout)
		}
		if err := e.Sys.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (e *Engine) startService(ctx context.Context) error {
	if _, err := e.Sys.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %w", err)
	}
	if _, err := e.Sys.Run(ctx, "systemctl", "start", ServiceName); err != nil {
		return fmt.Errorf("systemctl start 失败: %w", err)
	}
	return nil
}

// waitHealthy 轮询健康检查直到服务报告的版本等于 want。
func (e *Engine) waitHealthy(ctx context.Context, listen, want string) error {
	url := healthURL(listen)
	deadline := e.Sys.Now().Add(e.HealthTimeout)
	var last error
	for {
		v, err := e.Health(ctx, url)
		switch {
		case err != nil:
			last = err
		case sameVersion(v, want):
			return nil
		default:
			last = fmt.Errorf("服务报告版本 %s，期望 %s", v, want)
		}
		if !e.Sys.Now().Before(deadline) {
			return fmt.Errorf("健康检查在 %s 内未通过（%s）：%v", e.HealthTimeout, url, last)
		}
		if err := e.Sys.Sleep(ctx, e.HealthInterval); err != nil {
			return err
		}
	}
}

// ---- 数据：备份 / 恢复 ----

var backupNameRe = regexp.MustCompile(`^\d{8}-\d{6}-.+\.tar\.gz$`)

// makeBackup 把数据目录备份到 <备份目录>/<yyyymmdd-hhmmss>-<版本><suffix>.tar.gz（0600）。
func (e *Engine) makeBackup(version, suffix string) (string, error) {
	if err := os.MkdirAll(e.L.Backups(), 0o700); err != nil {
		return "", err
	}
	base := e.Sys.Now().Format("20060102-150405") + "-" + version + suffix
	dst := filepath.Join(e.L.Backups(), base+".tar.gz")
	for i := 2; ; i++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dst = filepath.Join(e.L.Backups(), fmt.Sprintf("%s-%d.tar.gz", base, i))
	}
	return e.backupTo(dst)
}

// backupTo 备份到指定文件（先写 .partial 再 rename）。
func (e *Engine) backupTo(dst string) (string, error) {
	if _, err := os.Stat(e.L.Data()); err != nil {
		return "", fmt.Errorf("数据目录不可用: %w", err)
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("%s 已存在，拒绝覆盖", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp := dst + ".partial"
	_ = os.Remove(tmp)
	skipped, err := writeTarGz(e.L.Data(), tmp, 0o600)
	if err != nil {
		return "", fmt.Errorf("备份数据目录失败: %w", err)
	}
	for _, s := range skipped {
		e.printf("  备份跳过特殊文件（符号链接等）：%s\n", s)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// backupInfo 是备份目录里的一个备份。
type backupInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// listBackups 返回备份，最新在前（文件名以时间戳开头）。
func (e *Engine) listBackups() ([]backupInfo, error) {
	ents, err := os.ReadDir(e.L.Backups())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []backupInfo
	for _, d := range ents {
		if d.IsDir() || !backupNameRe.MatchString(d.Name()) {
			continue
		}
		var size int64
		if info, err := d.Info(); err == nil {
			size = info.Size()
		}
		out = append(out, backupInfo{Name: d.Name(), Path: filepath.Join(e.L.Backups(), d.Name()), Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// pruneBackups 只保留最近 keep 个备份。
func (e *Engine) pruneBackups(keep int) {
	all, err := e.listBackups()
	if err != nil {
		return
	}
	var bs []backupInfo // 只清理升级自动备份；手动备份（-manual）与恢复前快照（-pre-restore）一律保留
	for _, b := range all {
		if !strings.Contains(b.Name, "-manual") && !strings.Contains(b.Name, "-pre-restore") {
			bs = append(bs, b)
		}
	}
	if len(bs) <= keep {
		return
	}
	for _, b := range bs[keep:] {
		if err := os.Remove(b.Path); err == nil {
			e.printf("  清理旧备份 %s\n", b.Name)
		}
	}
}

// restoreData 用归档恢复数据目录：先完整解到旁边的暂存目录（校验路径穿越 / 符号链接），
// 通过后才清空数据目录并移入；upgrades.jsonl 取恢复前的内容（升级历史不因恢复而丢失）。
func (e *Engine) restoreData(archive string) error {
	data := e.L.Data()
	staging := data + ".restore-tmp"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractTarGz(archive, staging); err != nil {
		return fmt.Errorf("备份文件校验 / 解压失败，数据目录未改动: %w", err)
	}
	hist, _ := readHistory(e.L.History())
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	if err := removeContents(data); err != nil {
		return fmt.Errorf("清空数据目录失败: %w", err)
	}
	ents, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, d := range ents {
		if err := os.Rename(filepath.Join(staging, d.Name()), filepath.Join(data, d.Name())); err != nil {
			return fmt.Errorf("移入 %s 失败（数据目录处于不完整状态，请用 pre-restore 备份手工恢复）: %w", d.Name(), err)
		}
	}
	if len(hist) > 0 {
		if err := writeHistory(e.L.History(), hist); err != nil {
			e.printf("警告：保留升级历史失败：%v\n", err)
		}
	}
	return e.Sys.Chown(data, RunUser, RunUser, true)
}

// currentSchema 返回数据目录当前 schema（没有 VERSION 视为 0）；不检查是否过高。
func (e *Engine) currentSchema() (int, error) {
	in, err := dataver.Read(e.L.Data())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return in.Schema, nil
}

// dataHasFiles 数据目录里是否已有文件。
func (e *Engine) dataHasFiles() bool {
	ents, err := os.ReadDir(e.L.Data())
	return err == nil && len(ents) > 0
}
