// @author: kevinkelly
// @license: Apache License 2.0
// @file: layout.go
// @time: 2026/10/28

package release

import (
	"path/filepath"
	"strings"
)

// 目标机上的逻辑路径（写进 systemd 单元 / sudoers 的都是这些绝对路径，不带 Layout 根前缀）。
const (
	OptDir      = "/opt/tidbops"
	ReleasesDir = "/opt/tidbops/releases"
	CurrentLink = "/opt/tidbops/current"
	// CurrentBin 是稳定的二进制路径：systemd 的 ExecStart、sudoers 与 --exec-bin 都指向它，切换符号链接即切换版本。
	CurrentBin = "/opt/tidbops/current/bin/tidbops-server"
	CurrentWeb = "/opt/tidbops/current/web"

	DataDir    = "/var/lib/tidbops"
	BackupDir  = "/var/backups/tidbops"
	EtcDir     = "/etc/tidb-ops"
	UnitPath   = "/etc/systemd/system/tidbops.service"
	SudoersDir = "/etc/sudoers.d"
	SudoersFil = "/etc/sudoers.d/tidbops"
	LockPath   = "/var/lock/tidbops-admin.lock"

	// ExecTempDir 是执行器的临时目录（exec.json 的 tempDir 默认值）。不能放在 /tmp 下：服务单元带 PrivateTmp=yes，
	// 经 sudo 起来的执行器子进程看到的是服务的私有 /tmp，在系统 /tmp 里建的目录它看不到。
	ExecTempDir = "/var/lib/tidbops-exec"
	// DropInFile 放开沙箱里执行器 / tiup 需要写的路径（ReadWritePaths）。单元文件每次升级都会按包内模板重新生成，drop-in 不会被覆盖。
	DropInDir  = "/etc/systemd/system/tidbops.service.d"
	DropInFile = "/etc/systemd/system/tidbops.service.d/exec-rw.conf"

	ServiceName = "tidbops"
	RunUser     = "tidbops"
)

// AdminPasswordPrefix 是 `tidbops-server user add --gen-password` 打印初始口令的那一行的前缀（安装器据此取回口令）。
const AdminPasswordPrefix = "TIDBOPS_ADMIN_PASSWORD="

// 发布包内的相对路径。
const (
	pkgBin      = "bin/tidbops-server"
	pkgUnit     = "deploy/ops/tidbops.service"
	pkgSudoers  = "deploy/ops/sudoers.tidbops"
	pkgExecJSON = "deploy/ops/exec.json.example"
)

// Layout 表达目标机目录布局。Root 为空或 "/" 表示真实根；测试把它指到临时目录。
// 所有磁盘路径都必须经 Layout 拼接。
type Layout struct {
	Root string
}

// P 把逻辑绝对路径映射到磁盘路径。
func (l Layout) P(logical string) string {
	if l.Root == "" || l.Root == "/" {
		return logical
	}
	return filepath.Join(l.Root, filepath.FromSlash(strings.TrimPrefix(logical, "/")))
}

func (l Layout) Releases() string           { return l.P(ReleasesDir) }
func (l Layout) Current() string            { return l.P(CurrentLink) }
func (l Layout) Data() string               { return l.P(DataDir) }
func (l Layout) Backups() string            { return l.P(BackupDir) }
func (l Layout) Etc() string                { return l.P(EtcDir) }
func (l Layout) Unit() string               { return l.P(UnitPath) }
func (l Layout) SudoersDir() string         { return l.P(SudoersDir) }
func (l Layout) Sudoers() string            { return l.P(SudoersFil) }
func (l Layout) Lock() string               { return l.P(LockPath) }
func (l Layout) ExecTemp() string           { return l.P(ExecTempDir) }
func (l Layout) DropIn() string             { return l.P(DropInFile) }
func (l Layout) Opt() string                { return l.P(OptDir) }
func (l Layout) ReleaseDir(v string) string { return filepath.Join(l.Releases(), v) }
func (l Layout) ExecKey() string            { return filepath.Join(l.Etc(), "exec.key") }
func (l Layout) ExecJSON() string           { return filepath.Join(l.Etc(), "exec.json") }
func (l Layout) ExecEnv() string            { return filepath.Join(l.Etc(), "exec.env") }
func (l Layout) Env() string                { return filepath.Join(l.Etc(), "env") }
func (l Layout) History() string            { return filepath.Join(l.Data(), "upgrades.jsonl") }
