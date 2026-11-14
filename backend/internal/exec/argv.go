// @author: kevinkelly
// @license: Apache License 2.0
// @file: argv.go
// @time: 2026/10/28

package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Plan 是一条命令的执行计划。Argv 直接交给 os/exec（不经 shell）。
type Plan struct {
	Argv      []string  // argv[0] 为可执行文件；路径一律是 Linux 风格
	Env       []string  // 仅传给子进程的额外环境变量（可能含密码，绝不可记录）
	TempFiles []string  // 本机临时文件（操作系统路径），执行后必须删除
	HTTP      *HTTPCall // 非空表示这是一次 HTTP 调用（alert.silence），由 DirectRunner 直接发起，Argv 仅用于展示
}

// Cleanup 删除临时文件。可重复调用。
func (p Plan) Cleanup() {
	for _, f := range p.TempFiles {
		_ = os.Remove(f)
	}
}

// String 返回可记录的命令行（不含环境变量，因此不含任何凭据）。
func (p Plan) String() string { return strings.Join(p.Argv, " ") }

// Argv 校验命令并生成 argv。固定模板、不经 shell；SQL 语句由模板 + 已归一化的值生成；
// 需要拓扑文件的命令（scale-out / edit-config / 带 Content 的 check）会把 Content 写入
// Policy.TempDir 下 0600 的临时文件，调用方必须在执行后调用 Plan.Cleanup。
// 函数内部会重新做完整校验（纵深防御），即使调用方此前已调用过 Validate。
func Argv(ctx context.Context, pol Policy, topo TopoFunc, c Cmd) (Plan, error) {
	p, _, err := plan(ctx, pol, topo, c, false)
	return p, err
}

// dryArgv 与 Argv 相同，但不写临时文件（用于 SimRunner 展示命令行）。
func dryArgv(ctx context.Context, pol Policy, topo TopoFunc, c Cmd) (Plan, Topo, error) {
	return plan(ctx, pol, topo, c, true)
}

func plan(ctx context.Context, pol Policy, topo TopoFunc, c Cmd, dry bool) (Plan, Topo, error) {
	pol = pol.forCluster(c.Cluster)
	t, err := check(ctx, pol, topo, c)
	if err != nil {
		return Plan{}, Topo{}, err
	}
	tm := &tempMaker{dir: pol.TempDir, dry: dry}
	p, err := build(pol, t, c, tm)
	p.TempFiles = tm.files
	if err != nil {
		p.Cleanup()
		return Plan{}, Topo{}, err
	}
	if err := guardArgv(p.Argv); err != nil {
		p.Cleanup()
		return Plan{}, Topo{}, err
	}
	return p, t, nil
}

// 模板里出现的全部固定选项；其它以 - 开头的参数一律视为注入并拒绝。
var fixedFlags = map[string]bool{
	"-N": true, "-R": true, "-y": true, "--cluster": true, "--topology-file": true,
	"--pd": true, "--storage": true, "--task-name": true, "--server": true,
	"-u": true, "--batch": true, "-h": true, "-P": true, "-e": true,
	"--version": true, // tiup --version / tiup cluster --version（只读，不带任何用户输入）
}

func guardArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("内部错误: 空命令")
	}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") && !fixedFlags[a] {
			return fmt.Errorf("内部错误: 参数 %q 以 - 开头", truncate(a, 30))
		}
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("内部错误: 参数含 NUL")
		}
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func build(pol Policy, t Topo, c Cmd, tm *tempMaker) (Plan, error) {
	tiup := orDefault(pol.TiupBin, "tiup")
	var a []string
	var env []string
	var httpCall *HTTPCall
	nodes := strings.Join(c.Nodes, ",")
	ctl := func(rest ...string) []string {
		return append([]string{tiup, "ctl:" + t.Version, "pd", "-u", t.PD}, rest...)
	}
	br := func(rest ...string) []string {
		return append([]string{tiup, "br:" + t.Version}, rest...)
	}

	switch c.Kind {
	case KindClusterList:
		a = []string{tiup, "cluster", "list"}
	case KindTiupVersion:
		a = []string{tiup, "--version"}
	case KindTiupClusterVer:
		a = []string{tiup, "cluster", "--version"}
	case KindClusterDisplay:
		a = []string{tiup, "cluster", "display", c.Cluster}
	case KindClusterCheck:
		a = []string{tiup, "cluster", "check", c.Cluster}
		if c.Content != "" {
			f, err := tm.write("check", c.Content)
			if err != nil {
				return Plan{}, err
			}
			a = append(a, f)
		}
		a = append(a, "--cluster")
	case KindPDQuery:
		a = ctl(pdQueryCmds[c.Key]...)
	case KindCDCCaptures:
		srv, err := cdcServer(t)
		if err != nil {
			return Plan{}, err
		}
		a = []string{tiup, "cdc:" + t.Version, "cli", "capture", "list", "--server", srv}

	case KindClusterRestart:
		a = []string{tiup, "cluster", "restart", c.Cluster}
		if c.Role != "" {
			a = append(a, "-R", tiupRoleName(c.Role))
		} else {
			a = append(a, "-N", nodes)
		}
		a = append(a, "-y")
	case KindClusterStop:
		a = []string{tiup, "cluster", "stop", c.Cluster, "-N", nodes, "-y"}
	case KindClusterStart:
		a = []string{tiup, "cluster", "start", c.Cluster, "-N", nodes}
	case KindClusterScaleIn:
		a = []string{tiup, "cluster", "scale-in", c.Cluster, "-N", nodes, "-y"}
	case KindClusterPrune:
		a = []string{tiup, "cluster", "prune", c.Cluster, "-y"}
	case KindClusterScaleOut:
		f, err := tm.write("scale-out", c.Content)
		if err != nil {
			return Plan{}, err
		}
		a = []string{tiup, "cluster", "scale-out", c.Cluster, f, "-y"}
	case KindClusterEditCfg:
		f, err := tm.write("edit-config", c.Content)
		if err != nil {
			return Plan{}, err
		}
		a = []string{tiup, "cluster", "edit-config", c.Cluster, "--topology-file", f, "-y"}
	case KindClusterReload:
		a = []string{tiup, "cluster", "reload", c.Cluster}
		if c.Role != "" {
			a = append(a, "-R", tiupRoleName(c.Role))
		} else {
			a = append(a, "-N", nodes)
		}
		a = append(a, "-y")

	case KindPDConfigSet:
		g, err := resolveConfig(t, c)
		if err != nil {
			return Plan{}, err
		}
		a = ctl("config", "set", PDShortKey(g.item.Key), g.value)
	case KindPDEvictAdd:
		a = ctl("scheduler", "add", "evict-leader-scheduler", strconv.FormatInt(c.Store, 10))
	case KindPDEvictRemove:
		a = ctl("scheduler", "remove", "evict-leader-scheduler-"+strconv.FormatInt(c.Store, 10))
	case KindPDStoreLimit:
		a = ctl("store", "limit", "all", strconv.Itoa(c.Rate))
	case KindPDLeaderTransfer:
		a = ctl("member", "leader", "transfer", c.Name)

	case KindSQLSetConfig, KindSQLSetGlobal:
		g, err := resolveConfig(t, c)
		if err != nil {
			return Plan{}, err
		}
		host, port, user, err := sqlTarget(pol, t)
		if err != nil {
			return Plan{}, err
		}
		stmt, err := sqlStatement(c, g)
		if err != nil {
			return Plan{}, err
		}
		a = []string{orDefault(pol.MySQLBin, "mysql"), "--batch", "-h", host, "-P", strconv.Itoa(port), "-u", user, "-e", stmt}
		if pol.MySQLPwdEnv != "" {
			// 密码只经环境变量（mysql 客户端读取 MYSQL_PWD）传给子进程，不进 argv、不进日志。
			if pw := os.Getenv(pol.MySQLPwdEnv); pw != "" {
				env = append(env, "MYSQL_PWD="+pw)
			}
		}

	case KindBRBackupFull:
		a = br("backup", "full", "--pd", t.PD, "--storage", c.Value)
	case KindBRRestoreFull:
		a = br("restore", "full", "--pd", t.PD, "--storage", c.Value)
	case KindBRLogStart:
		a = br("log", "start", "--pd", t.PD, "--task-name", c.Name, "--storage", c.Value)
	case KindBRLogStop:
		a = br("log", "stop", "--pd", t.PD, "--task-name", c.Name)
	case KindBRLogStatus:
		a = br("log", "status", "--pd", t.PD)

	case KindClusterUpgrade:
		a = []string{tiup, "cluster", "upgrade", c.Cluster, c.Value, "-y"}
	case KindAlertSilence:
		url := pol.AlertURL + "/api/v2/silences"
		a = []string{"POST", url}
		httpCall = &HTTPCall{Method: "POST", URL: url, Alertname: c.Key, Instance: c.Instance, Minutes: c.Rate, Comment: c.Value}

	default:
		return Plan{}, fmt.Errorf("命令 %q 不在白名单内", truncate(c.Kind, 40))
	}
	return Plan{Argv: a, Env: env, HTTP: httpCall}, nil
}

// sqlStatement 由模板与已归一化的值生成语句。键取自配置目录（而不是请求），值只含 [A-Za-z0-9._-]。
func sqlStatement(c Cmd, g cfgTarget) (string, error) {
	switch c.Kind {
	case KindSQLSetGlobal:
		lit, err := globalLiteral(g.item, g.value)
		if err != nil {
			return "", err
		}
		return "SET GLOBAL " + g.item.Key + " = " + lit, nil
	case KindSQLSetConfig:
		target := g.role
		if c.Instance != "" {
			target = `"` + c.Instance + `"`
		}
		return "SET CONFIG " + target + " `" + g.item.Key + "` = " + configLiteral(g.item, g.value), nil
	}
	return "", fmt.Errorf("内部错误: 不是 SQL 命令")
}

func quote(v string) string { return "'" + v + "'" }

// configLiteral：SET CONFIG 的值字面量。
func configLiteral(it ConfigItem, v string) string {
	switch it.Type {
	case TypeInt:
		return v
	case TypeBool:
		if v == "ON" {
			return "true"
		}
		return "false"
	}
	return quote(v) // size / duration / enum 作为字符串
}

// globalLiteral：SET GLOBAL 的值字面量。size 换算成字节数，bool 用 ON/OFF。
func globalLiteral(it ConfigItem, v string) (string, error) {
	switch it.Type {
	case TypeInt, TypeBool:
		return v, nil
	case TypeSize:
		n, err := SizeBytes(v)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	case TypeDuration:
		n, err := DurationSeconds(v)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	}
	return quote(v), nil
}

// tempMaker 把拓扑内容写入私有临时目录（0600、O_EXCL、随机文件名）。dry 时只返回占位路径。
type tempMaker struct {
	dir   string
	dry   bool
	files []string
}

func (m *tempMaker) write(prefix, content string) (string, error) {
	if m.dry {
		return "<tempdir>/tidbops-" + prefix + ".yaml", nil
	}
	if m.dir == "" || !filepath.IsAbs(m.dir) {
		return "", fmt.Errorf("未配置有效的临时目录（Policy.TempDir 必须是绝对路径）")
	}
	fi, err := os.Stat(m.dir)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("临时目录不可用")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("临时目录权限过宽（必须为 0700）")
	}
	var rnd [12]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("生成随机文件名失败: %w", err)
	}
	name := "tidbops-" + prefix + "-" + hex.EncodeToString(rnd[:]) + ".yaml"
	full := filepath.Join(m.dir, name)
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	m.files = append(m.files, full)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	_, werr := f.WriteString(content)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return "", fmt.Errorf("写入临时文件失败")
	}
	_ = os.Chmod(full, 0o600)
	return path.Join(filepath.ToSlash(m.dir), name), nil
}

// HTTPCall 描述 alert.silence 对 Alertmanager 的一次 POST。
type HTTPCall struct {
	Method    string
	URL       string // <AlertURL>/api/v2/silences
	Alertname string
	Instance  string // 可选
	Minutes   int
	Comment   string
}

// Body 生成 Alertmanager v2 silences 的 JSON 请求体。所有字段经 encoding/json 序列化，不做字符串拼接。
func (h HTTPCall) Body(now time.Time, actor string) []byte {
	type matcher struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual bool   `json:"isEqual"`
	}
	ms := []matcher{{"alertname", h.Alertname, false, true}}
	if h.Instance != "" {
		ms = append(ms, matcher{"instance", h.Instance, false, true})
	}
	comment := h.Comment
	if comment == "" {
		comment = "tidbops silence"
	}
	if a := logSafe(actor); a != "" {
		if len(a) > 64 {
			a = a[:64]
		}
		comment += " [by " + a + "]"
	}
	b, _ := json.Marshal(struct {
		Matchers  []matcher `json:"matchers"`
		StartsAt  string    `json:"startsAt"`
		EndsAt    string    `json:"endsAt"`
		CreatedBy string    `json:"createdBy"`
		Comment   string    `json:"comment"`
	}{ms, now.UTC().Format(time.RFC3339), now.Add(time.Duration(h.Minutes) * time.Minute).UTC().Format(time.RFC3339), "tidbops", comment})
	return b
}
