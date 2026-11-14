// @author: kevinkelly
// @license: Apache License 2.0
// @file: spec.go
// @time: 2026/10/28

// Package clusters 是多集群的「集群注册表」：clusters.json 的读写、字段校验、凭据加密，以及只读连通性测试。
//
// 本包不依赖 server：它只回答「平台纳管了哪些集群、每个集群的连接信息是什么」。实例的构造、热生效、路由与授权在
// internal/server/hub.go。
//
// 约束摘要：
//   - id：^[a-z][a-z0-9-]{1,31}$，平台内唯一、创建后不可改；name 是 tiup 集群名（所有 tiup / 执行器命令用它），
//     字符集 [A-Za-z0-9_-]{1,64}，默认等于 id；id 与 name 在整个注册表里互不冲突（审计里的 cluster 字段是 name，必须无歧义）；
//   - 主集群（启动参数构建的那个）是虚拟条目：不入注册表文件的连接配置，只保存可改的 display / desc；
//   - URL 只允许 http / https，主机非空、端口合法，禁止把凭据写进 URL，拒绝控制字符；
//   - 回放集群的 fixtures 目录必须在白名单根目录之下（符号链接解析后再比较），防止任意路径读取；
//   - 数据库密码用主密钥 AES-256-GCM 加密落盘，API 永不回显（只回 hasPassword）。
package clusters

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// 取值常量。
const (
	ModeLive   = "live"
	ModeReplay = "replay"

	// MaxClusters 是纳管集群数量上限（独立部署时的平台实例不计入）。
	MaxClusters = 20

	// 独立部署（--standalone）时的主实例：平台本身，不代表任何集群；对用户不可见，也不计入集群数量。
	PlatformID   = "platform"
	PlatformName = "_platform"

	MaxDisplayRunes = 40
	MaxDescRunes    = 200
)

var (
	reID   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	reName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reUser = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
	reHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	rePath = regexp.MustCompile(`^[A-Za-z0-9_./@+:\\-]{1,255}$`)
)

// IDPattern / NamePattern 是对外展示的校验规则。
const (
	IDPattern   = `^[a-z][a-z0-9-]{1,31}$`
	NamePattern = `^[A-Za-z0-9_-]{1,64}$`
)

// 错误码（与 roles_api 风格一致）。
const (
	ErrInvalid  = "invalid"   // 字段不合法
	ErrExists   = "exists"    // id / name 已存在
	ErrNotFound = "not_found" // 没有这个集群
	ErrLimit    = "limit"     // 数量上限
	ErrPrimary  = "primary"   // 主集群的限制
	ErrInUse    = "in_use"    // 有进行中的变更
	ErrStore    = "store"     // 注册表保存失败
	ErrStart    = "start"     // 实例启动失败（由 server 使用）
)

// Error 是注册表规则错误。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func invalid(format string, a ...any) *Error {
	return &Error{Code: ErrInvalid, Msg: fmt.Sprintf(format, a...)}
}

// Spec 是一个集群的完整定义（内存形态，MySQLPassword 为明文，只存在于进程内存与测试 / 连接时）。
type Spec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Display  string `json:"display"`
	Desc     string `json:"desc"`
	Env      string `json:"env"`
	Mode     string `json:"mode"`
	Primary  bool   `json:"primary,omitempty"`
	Platform bool   `json:"platform,omitempty"` // 独立部署的平台实例（只会是主实例）：不代表任何集群，不出现在集群列表里
	Fixtures string `json:"fixtures,omitempty"` // replay：数据包目录（白名单根目录之下）
	// Local：从本机（中控机）tiup 一键导入的集群——变更 / 巡检经本机执行器（tiup）完成，可做全部运维操作。
	// 只能由「导入」接口置位；手工添加的集群恒为 false：只读（平台不碰它的 tiup / SSH）。
	Local bool `json:"local,omitempty"`

	// live 的连接信息（对应 tidbops-server 的同名启动参数；留空则继承启动参数 / 自动探测）。
	PD         string `json:"pd,omitempty"`
	TiDBStatus string `json:"tidbStatus,omitempty"`
	Prom       string `json:"prom,omitempty"`
	Alert      string `json:"alert,omitempty"`
	MySQLHost  string `json:"mysqlHost,omitempty"`
	MySQLPort  int    `json:"mysqlPort,omitempty"`
	MySQLUser  string `json:"mysqlUser,omitempty"`
	MySQLBin   string `json:"mysqlBin,omitempty"`
	Tiup       string `json:"tiup,omitempty"`
	Home       string `json:"home,omitempty"`

	// MySQLPassword 绝不进入 JSON（API 与文件里都没有明文）；落盘时加密成 mysqlPasswordEnc。
	MySQLPassword string `json:"-"`

	Created   time.Time `json:"created"`
	CreatedBy string    `json:"createdBy,omitempty"`
	Updated   time.Time `json:"updated"`
	UpdatedBy string    `json:"updatedBy,omitempty"`

	// LoadError 非空表示该条目从文件加载后无法使用（例如密文解密失败、fixtures 不再在白名单下）。不落盘。
	LoadError string `json:"-"`

	passwordEnc string // 解密失败时保留原密文，保存时原样写回（避免因一次主密钥问题永久丢失）
}

// HasPassword 报告是否保存了数据库密码。
func (s Spec) HasPassword() bool { return s.MySQLPassword != "" || s.passwordEnc != "" }

// Input 是添加 / 更新的请求体。指针字段 nil 表示「不改」（添加时按空值 / 默认值）。
type Input struct {
	ID            string  `json:"id"`
	Name          *string `json:"name"`
	Display       *string `json:"display"`
	Desc          *string `json:"desc"`
	Env           *string `json:"env"`
	Mode          *string `json:"mode"`
	Fixtures      *string `json:"fixtures"`
	PD            *string `json:"pd"`
	TiDBStatus    *string `json:"tidbStatus"`
	Prom          *string `json:"prom"`
	Alert         *string `json:"alert"`
	MySQLHost     *string `json:"mysqlHost"`
	MySQLPort     *int    `json:"mysqlPort"`
	MySQLUser     *string `json:"mysqlUser"`
	MySQLPassword *string `json:"mysqlPassword"` // 留空 / 缺省 = 不改
	ClearPassword bool    `json:"clearPassword"` // 显式清除已保存的密码
	MySQLBin      *string `json:"mysqlBin"`
	Tiup          *string `json:"tiup"`
	Home          *string `json:"home"`
}

// View 是返回给管理员的完整配置（密码只给 hasPassword）。
type View struct {
	Spec
	HasPassword bool   `json:"hasPassword"`
	Error       string `json:"loadError,omitempty"`
}

// ViewOf 构造 View。
func ViewOf(s Spec) View { return View{Spec: s, HasPassword: s.HasPassword(), Error: s.LoadError} }

// Slug 把 tiup 集群名转成合法的平台 id（用于主集群）：小写，非 [a-z0-9] 的字符变成 "-"，必须以字母开头，最长 32。
func Slug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "cluster"
	}
	if s[0] < 'a' || s[0] > 'z' {
		s = "c-" + s
	}
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if len(s) < 2 {
		s += "0"
	}
	return s
}

// ValidID / ValidName 供外部判断。
func ValidID(id string) bool   { return reID.MatchString(id) }
func ValidName(n string) bool  { return reName.MatchString(n) }
func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

func textField(label, v string, max int, allowEmpty bool) (string, error) {
	v = strings.TrimSpace(v)
	if hasControl(v) {
		return "", invalid("%s 不能包含控制字符或换行", label)
	}
	if v == "" && !allowEmpty {
		return "", invalid("%s 不能为空", label)
	}
	if len([]rune(v)) > max {
		return "", invalid("%s 不能超过 %d 个字符", label, max)
	}
	return v, nil
}

// NormalizeURL 校验并规整一个 http / https 基础地址：主机非空、端口合法、禁止凭据 / 查询串 / 片段 / 控制字符，去掉末尾的 "/"。
// 空串原样返回（表示「未配置」）。
func NormalizeURL(label, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if hasControl(raw) || strings.ContainsAny(raw, " \t\\") {
		return "", invalid("%s 不能包含空白、反斜杠或控制字符", label)
	}
	if len(raw) > 256 {
		return "", invalid("%s 过长（最多 256 个字符）", label)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", invalid("%s 不是合法的 URL", label)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", invalid("%s 只允许 http / https 地址（形如 http://10.0.1.21:2379）", label)
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return "", invalid("%s 不能在 URL 里携带用户名 / 口令（凭据请放在专用字段）", label)
	}
	if u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "?#") {
		return "", invalid("%s 不能带查询串或片段", label)
	}
	h := u.Hostname()
	if h == "" {
		return "", invalid("%s 缺少主机名", label)
	}
	if net.ParseIP(h) == nil && !reHost.MatchString(h) {
		return "", invalid("%s 的主机名不合法", label)
	}
	if p := u.Port(); p != "" {
		if n, err := parsePort(p); err != nil || n < 1 || n > 65535 {
			return "", invalid("%s 的端口不合法（1–65535）", label)
		}
	}
	return strings.TrimRight(raw, "/"), nil
}

func parsePort(s string) (int, error) {
	n := 0
	if s == "" || len(s) > 5 {
		return 0, fmt.Errorf("bad port")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad port")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// normalizePath 校验可执行文件 / 目录路径：绝对路径、字符集受限；want 非空时要求文件名（去掉 .exe）必须在 want 之内。
func normalizePath(label, v string, want ...string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if hasControl(v) || !rePath.MatchString(v) || strings.Contains(v, "..") {
		return "", invalid("%s 只能是不含空白与特殊字符的路径", label)
	}
	if !filepath.IsAbs(v) && !strings.HasPrefix(v, "/") {
		return "", invalid("%s 必须是绝对路径", label)
	}
	if len(want) > 0 {
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(v, "\\", "/"))), ".exe")
		ok := false
		for _, w := range want {
			if base == w {
				ok = true
			}
		}
		if !ok {
			return "", invalid("%s 的文件名必须是 %s", label, strings.Join(want, " / "))
		}
	}
	return v, nil
}

// ResolveFixtures 校验回放数据包目录：必须存在、是目录，且（解析符号链接后）位于某个白名单根目录之下（或等于根目录）。
// 返回解析后的真实路径。
func ResolveFixtures(p string, roots []string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", invalid("回放模式必须指定 fixtures 目录")
	}
	if hasControl(p) || len(p) > 512 {
		return "", invalid("fixtures 路径不合法")
	}
	if len(roots) == 0 {
		return "", invalid("未配置回放数据包的白名单根目录（启动参数 --fixtures-root，或与 --fixtures 同级的目录），不能添加回放集群")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", invalid("fixtures 路径不合法")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", invalid("fixtures 目录不存在或无法访问")
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", invalid("fixtures 必须是一个目录")
	}
	for _, r := range roots {
		ra, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		rr, err := filepath.EvalSymlinks(ra)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rr, real)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return real, nil
		}
	}
	return "", invalid("fixtures 目录不在允许的根目录之下（启动参数 --fixtures-root）")
}

func validEnv(e string) bool { return e == "test" || e == "staging" || e == "prod" }

// Validate 校验并规整一个完整的 Spec（原地修改规整后的值）。roots 是 fixtures 白名单根目录。
// 主集群（Primary）跳过连接信息校验（它来自启动参数）。
func Validate(s *Spec, roots []string) error {
	if !reID.MatchString(s.ID) {
		return invalid("id 只能由小写字母、数字和连字符组成，以字母开头，长度 2–32（%s）", IDPattern)
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	if !reName.MatchString(s.Name) {
		return invalid("name（tiup 集群名）只能包含字母、数字、下划线和连字符，长度 1–64")
	}
	var err error
	if s.Display, err = textField("display", s.Display, MaxDisplayRunes, true); err != nil {
		return err
	}
	if s.Display == "" {
		s.Display = s.ID
	}
	if s.Desc, err = textField("desc", s.Desc, MaxDescRunes, true); err != nil {
		return err
	}
	if s.Primary {
		return nil
	}
	if s.ID == "import" || s.ID == "discover" { // 与 /api/clusters/import、/discover 的路由冲突
		return invalid("id %q 是保留字，请换一个", s.ID)
	}
	if !validEnv(s.Env) {
		return invalid("env 只能是 test | staging | prod")
	}
	if s.Local && s.Mode != ModeLive {
		return invalid("只有 live 集群可以由本机 tiup 管理")
	}
	switch s.Mode {
	case ModeReplay:
		if s.Fixtures, err = ResolveFixtures(s.Fixtures, roots); err != nil {
			return err
		}
		// 回放集群不使用任何连接信息：清空，避免残留的凭据被保存
		s.PD, s.TiDBStatus, s.Prom, s.Alert, s.MySQLHost, s.MySQLUser, s.MySQLBin, s.Tiup, s.Home = "", "", "", "", "", "", "", "", ""
		s.MySQLPort, s.MySQLPassword, s.passwordEnc = 0, "", ""
	case ModeLive:
		s.Fixtures = ""
		for _, f := range []struct {
			label string
			p     *string
		}{{"pd", &s.PD}, {"tidbStatus", &s.TiDBStatus}, {"prom", &s.Prom}, {"alert", &s.Alert}} {
			if *f.p, err = NormalizeURL(f.label, *f.p); err != nil {
				return err
			}
		}
		s.MySQLHost = strings.TrimSpace(s.MySQLHost)
		if s.MySQLHost != "" && net.ParseIP(strings.Trim(s.MySQLHost, "[]")) == nil && !reHost.MatchString(s.MySQLHost) {
			return invalid("mysqlHost 不是合法的主机名 / IP")
		}
		if s.MySQLPort < 0 || s.MySQLPort > 65535 {
			return invalid("mysqlPort 不合法（1–65535，0 表示默认 4000）")
		}
		s.MySQLUser = strings.TrimSpace(s.MySQLUser)
		if s.MySQLUser != "" && !reUser.MatchString(s.MySQLUser) {
			return invalid("mysqlUser 只能包含字母、数字和 . _ @ -")
		}
		if s.MySQLHost != "" && s.MySQLUser == "" {
			return invalid("配置了 mysqlHost 就需要 mysqlUser")
		}
		if hasControl(s.MySQLPassword) || len(s.MySQLPassword) > 256 {
			return invalid("mysqlPassword 不能包含控制字符，且不超过 256 字节")
		}
		if s.MySQLBin, err = normalizePath("mysqlBin", s.MySQLBin, "mysql", "mariadb"); err != nil {
			return err
		}
		if s.Tiup, err = normalizePath("tiup", s.Tiup, "tiup"); err != nil {
			return err
		}
		if s.Home, err = normalizePath("home", s.Home); err != nil {
			return err
		}
	default:
		return invalid("mode 只能是 live | replay")
	}
	return nil
}

// apply 把 Input 叠加到 s 上（nil 字段不改）。
func (in Input) apply(s *Spec) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&s.Name, in.Name)
	set(&s.Display, in.Display)
	set(&s.Desc, in.Desc)
	set(&s.Env, in.Env)
	set(&s.Mode, in.Mode)
	set(&s.Fixtures, in.Fixtures)
	set(&s.PD, in.PD)
	set(&s.TiDBStatus, in.TiDBStatus)
	set(&s.Prom, in.Prom)
	set(&s.Alert, in.Alert)
	set(&s.MySQLHost, in.MySQLHost)
	set(&s.MySQLUser, in.MySQLUser)
	set(&s.MySQLBin, in.MySQLBin)
	set(&s.Tiup, in.Tiup)
	set(&s.Home, in.Home)
	if in.MySQLPort != nil {
		s.MySQLPort = *in.MySQLPort
	}
	if in.MySQLPassword != nil && *in.MySQLPassword != "" {
		s.MySQLPassword, s.passwordEnc, s.LoadError = *in.MySQLPassword, "", ""
	}
	if in.ClearPassword {
		s.MySQLPassword, s.passwordEnc = "", ""
	}
}
