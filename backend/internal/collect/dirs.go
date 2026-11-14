// @author: kevinkelly
// @license: Apache License 2.0
// @file: dirs.go
// @time: 2026/10/28

package collect

import (
	"context"
	"path"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// metaDirs 是从 tiup meta.yaml 读到的目录信息，用于补全实例的 LogDir。
type metaDirs struct {
	// logDir：实例地址（host:port，每个端口键各登记一份）→ 该实例在 meta.yaml 里明确写的 log_dir。
	logDir map[string]string
	// globalLog：topology.global.log_dir 是否有值。有值时实例没写 log_dir 的日志目录取决于全局设置，不再推断。
	globalLog bool
	// ok：meta.yaml 读到并解析成功。读不到时不做推断（无法确认没有自定义 log_dir）。
	ok bool
}

// instanceServerKeys 是 meta.yaml topology 下的实例列表键。
var instanceServerKeys = []string{"tidb_servers", "tikv_servers", "pd_servers", "tiflash_servers", "cdc_servers"}

// portKeys 是实例条目里可能出现在 display 的 ID（host:port）里的端口键。
var portKeys = []string{"port", "client_port", "tcp_port", "flash_service_port"}

// readMetaDirs 读取 tiup meta.yaml（采集项 tiup/meta，可选）里的 log_dir。读不到 / 解析失败返回 ok=false。
func (c *Collector) readMetaDirs(ctx context.Context) metaDirs {
	md := metaDirs{logDir: map[string]string{}}
	b, err := c.get(ctx, "tiup/meta")
	if err != nil {
		return md
	}
	var meta struct {
		Topology map[string]any `yaml:"topology"`
	}
	if yaml.Unmarshal(b, &meta) != nil || meta.Topology == nil {
		return md
	}
	md.ok = true
	if g, ok := meta.Topology["global"].(map[string]any); ok {
		md.globalLog = strings.TrimSpace(str(g["log_dir"])) != ""
	}
	for _, key := range instanceServerKeys {
		list, _ := meta.Topology[key].([]any)
		for _, it := range list {
			e, _ := it.(map[string]any)
			host, ld := str(e["host"]), strings.TrimSpace(str(e["log_dir"]))
			if host == "" || ld == "" {
				continue
			}
			for _, pk := range portKeys {
				if p := str(e[pk]); p != "" {
					md.logDir[host+":"+p] = ld
				}
			}
		}
	}
	return md
}

// str 把 YAML 标量（字符串 / 整数）转成字符串，其余类型返回空。
func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

// resolveLogDir 给出一个实例的日志目录。addrs 是该实例可能出现在 meta.yaml 里的地址（display 的 ID、host:port）。
// 返回 (目录, 是否推断)。只在能确定时返回非空：
//  1. meta.yaml 里该实例明确写了 log_dir：绝对路径原样返回；相对路径拼在 deploy_dir 下（tiup 约定），deploy_dir 未知则留空；
//  2. 否则 meta.yaml 可读、没有全局 log_dir、且 deploy_dir 是绝对路径：按 tiup 默认约定推断为 <deploy_dir>/log；
//  3. 其余情况为空。
func (md metaDirs) resolveLogDir(deployDir string, addrs ...string) (string, bool) {
	for _, a := range addrs {
		if ld, ok := md.logDir[a]; ok {
			if path.IsAbs(ld) {
				return path.Clean(ld), false
			}
			if path.IsAbs(deployDir) {
				return path.Join(deployDir, ld), false
			}
			return "", false
		}
	}
	if md.ok && !md.globalLog && path.IsAbs(deployDir) {
		return path.Join(deployDir, "log"), true
	}
	return "", false
}
