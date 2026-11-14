// @author: kevinkelly
// @license: Apache License 2.0
// @file: validate.go
// @time: 2026/10/28

package source

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 本文件是「只读保证」的核心：任何请求在真正发出之前，都必须先通过这里的校验。
// 校验与 A3 文档中的命令白名单一致：未登记的命令、参数、SQL 一律拒绝。

var (
	clusterNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	endpointSet   = map[string]bool{"pd": true, "tidb": true, "prom": true, "alert": true}
	metaPathRe    = regexp.MustCompile(`^/[^\x00]*/\.tiup/storage/cluster/clusters/[A-Za-z0-9][A-Za-z0-9_-]{0,63}/meta\.yaml$`)
	fromRe        = regexp.MustCompile("(?i)\\b(?:from|join)\\s+([A-Za-z_][A-Za-z0-9_.`]*)")
	limitRe       = regexp.MustCompile(`(?i)\blimit\s+(\d+)\s*$`)
	forbiddenSQL  = regexp.MustCompile(`(?i)\b(into\s+(outfile|dumpfile)|load_file|sleep|benchmark|for\s+update|lock\s+in\s+share)\b`)
)

// MaxSQLLimit 是单条 SQL 允许的最大 LIMIT。
const MaxSQLLimit = 1000

// ValidateItem 校验一个条目是否允许执行。
func ValidateItem(it Item) error {
	if it.Name == "" {
		return fmt.Errorf("条目缺少名称")
	}
	switch it.Kind {
	case KindHTTP:
		if !endpointSet[it.Endpoint] {
			return fmt.Errorf("%s: 未知端点 %q", it.Name, it.Endpoint)
		}
		if !strings.HasPrefix(it.Path, "/") || strings.ContainsAny(it.Path, " \r\n\t") || strings.Contains(it.Path, "://") || strings.HasPrefix(it.Path, "//") {
			return fmt.Errorf("%s: 非法路径 %q", it.Name, it.Path)
		}
	case KindCmd:
		if err := ValidateCmd(it.Argv); err != nil {
			return fmt.Errorf("%s: %w", it.Name, err)
		}
	case KindSQL:
		if err := ValidateSQL(it.Query); err != nil {
			return fmt.Errorf("%s: %w", it.Name, err)
		}
	case KindFile:
		if err := ValidateFile(it.File); err != nil {
			return fmt.Errorf("%s: %w", it.Name, err)
		}
	default:
		return fmt.Errorf("%s: 未知种类 %q", it.Name, it.Kind)
	}
	return nil
}

// ValidateCmd 校验命令。只允许下列固定形式，不经过 shell，参数不可拼接。
func ValidateCmd(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("命令为空")
	}
	for _, a := range argv {
		if strings.ContainsAny(a, ";&|><`$(){}\\\n\r\"' ") {
			return fmt.Errorf("参数 %q 含 shell 元字符或空白", a)
		}
	}
	a := argv[1:]
	ok := false
	switch argv[0] {
	case "hostname":
		ok = len(a) == 0 || (len(a) == 1 && a[0] == "-I")
	case "uname":
		ok = len(a) == 1 && a[0] == "-r"
	case "nproc", "timedatectl":
		ok = len(a) == 0
	case "free":
		ok = len(a) == 1 && a[0] == "-m"
	case "cat":
		ok = len(a) == 1 && a[0] == "/etc/os-release"
	case "tiup":
		switch {
		case len(a) == 1 && a[0] == "--version":
			ok = true
		case len(a) == 2 && a[0] == "cluster" && a[1] == "--version":
			ok = true
		case len(a) == 2 && a[0] == "cluster" && a[1] == "list":
			ok = true
		case len(a) == 3 && a[0] == "cluster" && a[1] == "display" && clusterNameRe.MatchString(a[2]):
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("命令 %q 不在白名单内", strings.Join(argv, " "))
	}
	return nil
}

// ValidateSQL 校验 SQL：只允许单条 SELECT，只允许访问 information_schema / performance_schema，
// 必须带 LIMIT（≤ MaxSQLLimit），禁止注释、多语句、文件读写与长时间函数。
// 唯一的无 FROM 例外是 SELECT tidb_version()。
func ValidateSQL(q string) error {
	q = strings.TrimSpace(q)
	if q == "" {
		return fmt.Errorf("SQL 为空")
	}
	if strings.EqualFold(oneLine(q), "select tidb_version()") {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(q), "select ") {
		return fmt.Errorf("只允许 SELECT")
	}
	if strings.ContainsAny(q, ";#") || strings.Contains(q, "--") || strings.Contains(q, "/*") {
		return fmt.Errorf("SQL 含分号或注释")
	}
	if forbiddenSQL.MatchString(q) {
		return fmt.Errorf("SQL 含被禁止的子句或函数")
	}
	ms := fromRe.FindAllStringSubmatch(q, -1)
	if len(ms) == 0 {
		return fmt.Errorf("SQL 必须带 FROM（仅 select tidb_version() 例外）")
	}
	for _, m := range ms {
		t := strings.ToLower(strings.ReplaceAll(m[1], "`", ""))
		if !strings.HasPrefix(t, "information_schema.") && !strings.HasPrefix(t, "performance_schema.") {
			return fmt.Errorf("只允许访问 information_schema / performance_schema，发现 %q", t)
		}
	}
	lm := limitRe.FindStringSubmatch(q)
	if lm == nil {
		return fmt.Errorf("SQL 必须以 LIMIT n 结尾")
	}
	if n, _ := strconv.Atoi(lm[1]); n < 1 || n > MaxSQLLimit {
		return fmt.Errorf("LIMIT 必须在 1..%d 之间", MaxSQLLimit)
	}
	return nil
}

// ValidateFile 只允许读取 tiup 的 meta.yaml 与 /etc/os-release。
func ValidateFile(p string) error {
	if strings.Contains(p, "..") {
		return fmt.Errorf("路径含 ..")
	}
	if filepath.ToSlash(p) == "/etc/os-release" || metaPathRe.MatchString(filepath.ToSlash(p)) {
		return nil
	}
	return fmt.Errorf("文件 %q 不在白名单内", p)
}
