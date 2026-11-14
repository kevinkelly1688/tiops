// @author: kevinkelly
// @license: Apache License 2.0
// @file: spec.go
// @time: 2026/10/28

// Package deploy 实现「一键部署」：保存部署配置（带版本）、以 root 身份在中控机上运行 tidb-deploy.sh（预检 / 部署）、
// 跟踪任务状态与输出。
//
// 安全要点：
//   - deploy.conf 会被 tidb-deploy.sh 以 root 身份 source，所以平台从不接受前端传来的原始文本，只接受结构化的 Spec，
//     逐项按白名单字符校验后由 Render 生成（任何值都不含引号、$、反引号、换行等能改变 shell 语义的字符）。
//   - root 密码只在内存里使用一次，经 su 取得 root 后立即清零；不写入配置、任务记录、日志和审计。
package deploy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Spec 是一份部署配置（与前端「安装部署」页的表单一一对应）。主机列表用空格 / 逗号分隔的字符串，与页面一致。
type Spec struct {
	Cluster     string `json:"cluster"`
	Mode        string `json:"mode"`    // single | cluster
	Version     string `json:"version"` // auto | vX.Y | vX.Y.Z
	User        string `json:"user"`
	SSHPort     string `json:"sshPort"`
	SSHKey      string `json:"sshKey"`
	SingleHost  string `json:"singleHost"`
	PD          string `json:"pd"`
	TiDB        string `json:"tidb"`
	TiKV        string `json:"tikv"`
	TiFlash     string `json:"tiflash"`
	Monitor     string `json:"monitor"`
	Zone        string `json:"zone"`
	DeployDir   string `json:"deployDir"`
	DataDir     string `json:"dataDir"`
	Profile     string `json:"spec"` // dev | prod
	Firewall    string `json:"firewall"`
	RepoFix     string `json:"repoFix"`
	RepoMirror  string `json:"repoMirror"`
	WithMonitor bool   `json:"withMonitor"`
	Install     string `json:"install"` // online | offline
	OfflineDir  string `json:"offlineDir"`
	RPMDir      string `json:"rpmDir"`
	// AllowCheckFail 对应 deploy.conf 的 ALLOW_CHECK_FAIL：tiup check 复查仍有 Fail 时继续部署（会忽略全部 Fail 项）。
	// 只允许 dev 规格使用（见 Validate）；缺省 false，旧版本保存的配置没有这个字段，按 false 处理。
	AllowCheckFail bool `json:"allowCheckFail"`
}

var (
	reCluster = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	reUser    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reVersion = regexp.MustCompile(`^v\d+\.\d+(\.\d+)?$`)
	rePath    = regexp.MustCompile(`^/[A-Za-z0-9_./-]*$`)
	reZone    = regexp.MustCompile(`^[0-9.]+=[A-Za-z0-9_-]+$`)
)

// ValidCluster 判断集群名是否合法（用作目录名与单元名的一部分，所以字符集很窄）。
func ValidCluster(s string) bool { return reCluster.MatchString(s) }

// Hosts 把空格 / 逗号 / 分号分隔的主机串拆成列表。
func Hosts(s string) []string {
	return strings.Fields(strings.NewReplacer(",", " ", ";", " ").Replace(s))
}

func isIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

func safePath(p string) bool {
	return rePath.MatchString(p) && !strings.Contains(p, "..") && !strings.Contains(p, "//")
}

// Normalize 补默认值并去掉首尾空白（不改变语义）。
func (s Spec) Normalize() Spec {
	t := func(x string) string { return strings.TrimSpace(x) }
	s.Cluster, s.Mode, s.Version, s.User, s.SSHPort, s.SSHKey = t(s.Cluster), t(s.Mode), t(s.Version), t(s.User), t(s.SSHPort), t(s.SSHKey)
	s.SingleHost, s.Zone, s.DeployDir, s.DataDir = t(s.SingleHost), strings.Join(strings.Fields(s.Zone), ""), t(s.DeployDir), t(s.DataDir)
	s.Profile, s.Firewall, s.RepoFix, s.RepoMirror, s.Install = t(s.Profile), t(s.Firewall), t(s.RepoFix), t(s.RepoMirror), t(s.Install)
	s.OfflineDir, s.RPMDir = strings.TrimRight(t(s.OfflineDir), "/"), strings.TrimRight(t(s.RPMDir), "/")
	if s.Version == "" {
		s.Version = "auto"
	}
	if s.SSHPort == "" {
		s.SSHPort = "22"
	}
	if s.Profile == "" {
		s.Profile = "dev"
	}
	if s.Firewall == "" {
		s.Firewall = "open"
	}
	if s.RepoFix == "" {
		s.RepoFix = "skip"
	}
	if s.RepoMirror == "" {
		s.RepoMirror = "aliyun"
	}
	if s.Install == "" {
		s.Install = "online"
	}
	if s.DeployDir == "" {
		s.DeployDir = "/tidb-deploy"
	}
	if s.DataDir == "" {
		s.DataDir = "/tidb-data"
	}
	return s
}

// AllHosts 返回配置里涉及的全部主机（去重，保持出现顺序）。
func (s Spec) AllHosts() []string {
	var l []string
	if s.Mode == "single" {
		if h := strings.TrimSpace(s.SingleHost); h != "" {
			l = append(l, h)
		}
	} else {
		for _, g := range []string{s.PD, s.TiDB, s.TiKV, s.TiFlash, s.Monitor} {
			l = append(l, Hosts(g)...)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range l {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Validate 返回全部校验错误（空 = 通过）。规则与前端 dpValidate 一致，并且更严：这里的字符白名单是安全边界。
func (s Spec) Validate() []string {
	var e []string
	add := func(f string, a ...any) { e = append(e, fmt.Sprintf(f, a...)) }
	if !reCluster.MatchString(s.Cluster) {
		add("集群名只能含字母数字、下划线、连字符，且不超过 32 位")
	}
	if s.Mode != "single" && s.Mode != "cluster" {
		add("部署模式只能是 single 或 cluster")
	}
	if s.Version != "auto" && !reVersion.MatchString(s.Version) {
		add("TiDB 版本只能是 auto、vX.Y（系列）或 vX.Y.Z")
	}
	switch {
	case !reUser.MatchString(s.User):
		add("操作系统用户名不合法：只能含小写字母、数字、下划线、连字符，最长 32 位")
	case s.User == "root":
		add("操作系统用户不能是 root")
	}
	if n, err := strconv.Atoi(s.SSHPort); err != nil || n < 1 || n > 65535 {
		add("SSH 端口必须是 1–65535 的整数")
	}
	if s.SSHKey != "" && !safePath(s.SSHKey) {
		add("SSH 私钥路径必须是绝对路径，且只含字母数字和 _ . / -")
	}
	for _, d := range []struct{ n, p string }{{"部署目录", s.DeployDir}, {"数据目录", s.DataDir}} {
		if !safePath(d.p) || d.p == "/" {
			add("%s必须是绝对路径（不能是 /），且只含字母数字和 _ . / -", d.n)
		}
	}
	lists := map[string][]string{}
	if s.Mode == "single" {
		if h := strings.TrimSpace(s.SingleHost); h != "" {
			lists["单机"] = []string{h}
			if len(strings.Fields(h)) > 1 {
				add("单机模式只能填一个主机（留空则使用本机主 IP）")
			}
		}
	} else {
		lists["PD"], lists["TiDB"], lists["TiKV"], lists["TiFlash"], lists["监控"] = Hosts(s.PD), Hosts(s.TiDB), Hosts(s.TiKV), Hosts(s.TiFlash), Hosts(s.Monitor)
	}
	for _, role := range []string{"单机", "PD", "TiDB", "TiKV", "TiFlash", "监控"} {
		seen := map[string]bool{}
		for _, h := range lists[role] {
			if !isIPv4(h) {
				add("%s 的主机「%s」不是合法的 IPv4 地址", role, h)
			}
			if seen[h] {
				add("%s 里有重复的主机 %s", role, h)
			}
			seen[h] = true
		}
	}
	if s.Mode == "cluster" {
		if len(lists["PD"]) == 0 {
			add("至少需要 1 台 PD")
		}
		if len(lists["TiDB"]) == 0 {
			add("至少需要 1 台 TiDB")
		}
		if len(lists["TiKV"]) == 0 {
			add("至少需要 1 台 TiKV")
		}
		if s.Profile == "prod" && len(lists["PD"]) > 0 && len(lists["TiDB"]) > 0 && len(lists["TiKV"]) > 0 &&
			(len(lists["PD"]) < 3 || len(lists["TiKV"]) < 3 || len(lists["TiDB"]) < 2) {
			add("生产规格要求 PD ≥ 3、TiKV ≥ 3、TiDB ≥ 2（当前 %d / %d / %d）", len(lists["PD"]), len(lists["TiKV"]), len(lists["TiDB"]))
		}
		if s.Zone != "" {
			zm := map[string]string{}
			bad := false
			for _, p := range strings.Split(s.Zone, ",") {
				if p == "" {
					continue
				}
				if !reZone.MatchString(p) {
					bad = true
					break
				}
				kv := strings.SplitN(p, "=", 2)
				zm[kv[0]] = kv[1]
			}
			if bad {
				add("拓扑标签格式应为 IP=zone，多个用逗号分隔")
			} else {
				for _, h := range lists["TiKV"] {
					if zm[h] == "" {
						add("拓扑标签缺少 TiKV 主机 %s 的 zone", h)
					}
				}
			}
		}
	}
	if s.Profile != "dev" && s.Profile != "prod" {
		add("规格只能是 dev 或 prod")
	}
	if s.AllowCheckFail && s.Profile != "dev" {
		add("「忽略 tiup check 的失败项」只允许 dev 规格使用：生产规格必须先修复检查失败项")
	}
	if s.Firewall != "open" && s.Firewall != "disable" && s.Firewall != "keep" {
		add("防火墙策略只能是 open、disable 或 keep")
	}
	if s.RepoFix != "skip" && s.RepoFix != "auto" {
		add("yum 源处理只能是 skip 或 auto")
	}
	if s.RepoMirror != "aliyun" && s.RepoMirror != "vault" {
		add("yum 镜像只能是 aliyun 或 vault")
	}
	switch s.Install {
	case "online":
	case "offline":
		if s.OfflineDir == "" || !safePath(s.OfflineDir) {
			add("离线安装需要填写离线包所在的完整目录（绝对路径，只含字母数字和 _ . / -）")
		}
		if s.RPMDir != "" && !safePath(s.RPMDir) {
			add("离线 rpm 目录必须是绝对路径，且只含字母数字和 _ . / -")
		}
	default:
		add("安装方式只能是 online 或 offline")
	}
	return e
}

// Render 生成 deploy.conf。调用前必须 Validate 通过：所有值都在白名单字符内，所以直接放进双引号即可。
func (s Spec) Render() string {
	q := func(x string) string { return `"` + x + `"` }
	L := []string{"# 由「安装部署」页面生成；用法: ./tidb-deploy.sh deploy --config deploy.conf --dry-run",
		"CLUSTER_NAME=" + s.Cluster, "TIDB_VERSION=" + s.Version, "DEPLOY_MODE=" + s.Mode}
	if s.Mode == "single" {
		L = append(L, "SINGLE_HOST="+q(s.SingleHost))
	} else {
		L = append(L, "PD_HOSTS="+q(strings.Join(Hosts(s.PD), " ")), "TIDB_HOSTS="+q(strings.Join(Hosts(s.TiDB), " ")), "TIKV_HOSTS="+q(strings.Join(Hosts(s.TiKV), " ")),
			"TIFLASH_HOSTS="+q(strings.Join(Hosts(s.TiFlash), " ")), "MONITOR_HOST="+q(strings.Join(Hosts(s.Monitor), " ")))
		if s.Zone != "" {
			L = append(L, "ZONE_MAP="+q(s.Zone))
		}
	}
	L = append(L, "WITH_MONITOR="+strconv.FormatBool(s.WithMonitor), "", "# 操作系统用户（运行 TiUP 与集群）", "DEPLOY_USER="+s.User, "SSH_PORT="+s.SSHPort,
		"DEPLOY_DIR="+s.DeployDir, "DATA_DIR="+s.DataDir, "",
		"# 前置条件：所有机器（含中控机）已配好 "+s.User+" 用户、sudo 免密、"+s.User+" 免密 SSH 登录所有机器；脚本以 "+s.User+"@IP + sudo 登录，只检查不代劳",
		"# 部署用户的 SSH 私钥（绝对路径）；留空 = 该用户家目录的 ~/.ssh/id_rsa", "SSH_KEY="+q(s.SSHKey),
		"", "SPEC="+s.Profile, "FIREWALL="+s.Firewall, "REPO_FIX="+s.RepoFix, "REPO_MIRROR="+s.RepoMirror, "", "INSTALL_MODE="+s.Install)
	if s.AllowCheckFail { // 只在勾选时输出：不勾选的配置渲染结果与旧版本完全一致（版本哈希不变）
		L = append(L, "", "# 仅测试环境：tiup check 复查仍有 Fail 时继续部署（会忽略全部 Fail 项）", "ALLOW_CHECK_FAIL=true")
	}
	if s.Install == "offline" {
		L = append(L, "OFFLINE_PKG_DIR="+q(s.OfflineDir)+"   # 离线包所在的完整目录，脚本按版本自动查找", "OFFLINE_RPM_DIR="+q(s.RPMDir))
	}
	return strings.Join(L, "\n") + "\n"
}
