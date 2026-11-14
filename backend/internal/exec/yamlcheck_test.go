// @author: kevinkelly
// @license: Apache License 2.0
// @file: yamlcheck_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"strings"
	"testing"
)

const goodTiKV = `tikv_servers:
  - host: 10.0.2.21
    port: 20160
    status_port: 20180
    deploy_dir: /tidb-deploy/tikv-20160
    data_dir: /tidb-data/tikv-20160
    config:
      server.labels: { zone: "z1", rack: "r00", host: "10-0-2-21" }
      storage.block-cache.capacity: "16GB"
`

func TestScaleOutAccepts(t *testing.T) {
	tp := testTopo()
	ok := map[string]string{
		"tikv": goodTiKV,
		"tikv 嵌套写法": `tikv_servers:
  - host: 10.0.2.21
    config:
      server:
        labels:
          zone: z1
      storage:
        block-cache:
          capacity: 16GB
      raftstore.store-pool-size: 3
`,
		"tidb": "tidb_servers:\n  - host: 10.0.1.21\n    port: 4001\n    status_port: 10081\n    deploy_dir: /tidb-deploy/tidb-4001\n",
		"pd":   "pd_servers:\n  - host: 10.0.1.24\n    name: pd-10.0.1.24\n    client_port: 2379\n    peer_port: 2380\n    deploy_dir: /tidb-deploy/pd-2379\n    data_dir: /tidb-data/pd-2379\n",
		"tiflash": `tiflash_servers:
  - host: 10.0.3.21
    flash_service_port: 3930
    tcp_port: 9000
    http_port: 8123
    deploy_dir: /tidb-deploy/tiflash-3930
    data_dir: /tidb-data/tiflash-3930
    learner_config:
      server.labels: { zone: z1 }
`,
		"cdc": "cdc_servers:\n  - host: 10.0.4.13\n    port: 8300\n    deploy_dir: /tidb-deploy/cdc-8300\n    data_dir: /tidb-data/cdc-8300\n",
		"多个条目 + global": `global:
  user: tidb
  ssh_port: 22
  deploy_dir: /tidb-deploy
  data_dir: /tidb-data
tikv_servers:
  - host: 10.0.2.21
  - host: 10.0.2.22
tidb_servers:
  - host: 10.0.2.21
    port: 4000
`,
		"同机不同端口": "tikv_servers:\n  - host: 10.0.2.11\n    port: 20161\n    status_port: 20181\n",
		"注释":     "# 平台生成\ntikv_servers:\n  - host: 10.0.2.21 # new\n",
	}
	for name, y := range ok {
		if err := ValidateScaleOut(y, tp); err != nil {
			t.Errorf("%s 应通过: %v", name, err)
		}
	}
}

func TestScaleOutRejects(t *testing.T) {
	tp := testTopo()
	long := "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      log.level: " + strings.Repeat("a", 5000) + "\n"
	many := "tikv_servers:\n" + strings.Repeat("  - host: 10.0.2.21\n", 21)
	bad := map[string]string{
		"空":               "",
		"只有空白":            "  \n",
		"非映射根":            "- a\n- b\n",
		"标量根":             "hello",
		"非法 YAML":         "tikv_servers: [\n",
		"多文档":             "tikv_servers:\n  - host: 10.0.2.21\n---\ntikv_servers:\n  - host: 10.0.2.22\n",
		"未知顶层键":           "monitoring_servers:\n  - host: 10.0.2.21\n",
		"server_configs":  "server_configs:\n  tikv: {}\ntikv_servers:\n  - host: 10.0.2.21\n",
		"没有实例":            "global:\n  user: tidb\n",
		"空列表":             "tikv_servers: []\n",
		"servers 是映射":     "tikv_servers:\n  host: 10.0.2.21\n",
		"条目不是映射":          "tikv_servers:\n  - 10.0.2.21\n",
		"条目过多":            many,
		"缺 host":          "tikv_servers:\n  - port: 20160\n",
		"host 主机名":        "tikv_servers:\n  - host: tikv-21.example.com\n",
		"host 注入":         "tikv_servers:\n  - host: \"10.0.2.21; id\"\n",
		"host 环回":         "tikv_servers:\n  - host: 127.0.0.1\n",
		"host 0.0.0.0":    "tikv_servers:\n  - host: 0.0.0.0\n",
		"host 组播":         "tikv_servers:\n  - host: 224.0.0.1\n",
		"host 前导零":        "tikv_servers:\n  - host: 010.0.2.21\n",
		"host 越界":         "tikv_servers:\n  - host: 10.0.2.256\n",
		"host IPv6":       "tikv_servers:\n  - host: \"::1\"\n",
		"host 是整数":        "tikv_servers:\n  - host: 10\n",
		"host 列表":         "tikv_servers:\n  - host: [10.0.2.21]\n",
		"端口字符串":           "tikv_servers:\n  - host: 10.0.2.21\n    port: \"20161\"\n",
		"端口 0":            "tikv_servers:\n  - host: 10.0.2.21\n    port: 0\n",
		"端口 70000":        "tikv_servers:\n  - host: 10.0.2.21\n    port: 70000\n",
		"端口负数":            "tikv_servers:\n  - host: 10.0.2.21\n    port: -1\n",
		"端口十六进制":          "tikv_servers:\n  - host: 10.0.2.21\n    port: 0x4E21\n",
		"端口前导零":           "tikv_servers:\n  - host: 10.0.2.21\n    port: 020161\n",
		"端口小数":            "tikv_servers:\n  - host: 10.0.2.21\n    port: 2.5\n",
		"端口布尔":            "tikv_servers:\n  - host: 10.0.2.21\n    port: true\n",
		"端口重复":            "tikv_servers:\n  - host: 10.0.2.21\n    port: 20180\n",
		"ssh_port 非法":     "tikv_servers:\n  - host: 10.0.2.21\n    ssh_port: 99999\n",
		"目录 /etc":         "tikv_servers:\n  - host: 10.0.2.21\n    deploy_dir: /etc\n",
		"目录 ..":           "tikv_servers:\n  - host: 10.0.2.21\n    deploy_dir: /tidb-deploy/../etc\n",
		"目录前缀伪装":          "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: /tidb-datax/tikv\n",
		"目录相对":            "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: tidb-data/tikv\n",
		"目录含空格":           "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: \"/tidb-data/a b\"\n",
		"目录含分号":           "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: \"/tidb-data/a;b\"\n",
		"目录含双斜杠":          "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: /tidb-data//a\n",
		"目录含 $":           "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: \"/tidb-data/$HOME\"\n",
		"目录是整数":           "tikv_servers:\n  - host: 10.0.2.21\n    data_dir: 5\n",
		"未知条目键":           "tikv_servers:\n  - host: 10.0.2.21\n    resource_control: {memory_limit: 1G}\n",
		"条目键 numa":        "tikv_servers:\n  - host: 10.0.2.21\n    numa_node: \"0; id\"\n",
		"条目键 rule_dir":    "tikv_servers:\n  - host: 10.0.2.21\n    rule_dir: /etc\n",
		"name 在 tikv":     "tikv_servers:\n  - host: 10.0.2.21\n    name: x\n",
		"pd name 注入":      "pd_servers:\n  - host: 10.0.1.24\n    name: \"pd 1\"\n",
		"learner 在 tikv":  "tikv_servers:\n  - host: 10.0.2.21\n    learner_config: {server.labels: {zone: z1}}\n",
		"learner 非标签":     "tiflash_servers:\n  - host: 10.0.3.21\n    learner_config: {log.level: info}\n",
		"config 不在目录":     "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      storage.data-dir: /etc\n",
		"config 系统变量":     "tidb_servers:\n  - host: 10.0.1.21\n    config:\n      tidb_mem_quota_query: 1GB\n",
		"config 别的角色的键":   "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      schedule.max-store-down-time: 30m\n",
		"config 值非法":      "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      storage.block-cache.capacity: \"16GB; id\"\n",
		"config 值越界":      "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      raftstore.store-pool-size: 99\n",
		"config 值为空":      "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      log.level:\n",
		"config 是列表":      "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      - a\n",
		"config 值是列表":     "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      log.level: [info]\n",
		"label 值非法":       "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      server.labels: { zone: \"z 1\" }\n",
		"label 名非法":       "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      server.labels: { \"z.1\": z }\n",
		"label 值是映射":      "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      server.labels: { zone: {a: b} }\n",
		"tidb 的 labels":   "tidb_servers:\n  - host: 10.0.1.21\n    config:\n      server.labels: { zone: z1 }\n",
		"config 重复键":      "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      log.level: info\n      log:\n        level: warn\n",
		"与现有业务端口冲突":       "tikv_servers:\n  - host: 10.0.2.11\n    port: 20160\n    status_port: 20180\n",
		"与现有状态端口冲突":       "tidb_servers:\n  - host: 10.0.2.11\n    port: 20180\n    status_port: 10099\n",
		"默认端口冲突":          "tikv_servers:\n  - host: 10.0.2.11\n",
		"新实例之间冲突":         "tikv_servers:\n  - host: 10.0.2.21\n  - host: 10.0.2.21\n",
		"跨角色冲突":           "tikv_servers:\n  - host: 10.0.2.21\ncdc_servers:\n  - host: 10.0.2.21\n    port: 20160\n",
		"global 含其它键":     "global:\n  user: tidb\n  arch: arm64\ntikv_servers:\n  - host: 10.0.2.21\n",
		"global 用户不符":     "global:\n  user: root\ntikv_servers:\n  - host: 10.0.2.21\n",
		"global ssh_port": "global:\n  ssh_port: 2222\ntikv_servers:\n  - host: 10.0.2.21\n",
		"global 目录":       "global:\n  deploy_dir: /opt\ntikv_servers:\n  - host: 10.0.2.21\n",
		"global 不是映射":     "global: 1\ntikv_servers:\n  - host: 10.0.2.21\n",
		"锚点":              "tikv_servers:\n  - &a\n    host: 10.0.2.21\n",
		"别名":              "x: &a 10.0.2.21\ntikv_servers:\n  - host: *a\n",
		"合并键":             "base: &b {host: 10.0.2.21}\ntikv_servers:\n  - <<: *b\n",
		"自定义标签":           "tikv_servers:\n  - host: !!python/object:os.system 10.0.2.21\n",
		"自定义标签2":          "tikv_servers:\n  - host: !evil 10.0.2.21\n",
		"重复键":             "tikv_servers:\n  - host: 10.0.2.21\n    host: 10.0.2.22\n",
		"控制字符":            "tikv_servers:\n  - host: \"10.0.2.21\\x07\"\n",
		"换行":              "tikv_servers:\n  - host: \"10.0.2.21\\n\"\n",
		"多行标量":            "tikv_servers:\n  - host: |\n      10.0.2.21\n",
		"非 ASCII":         "tikv_servers:\n  - host: 10.0.2.21\n    config:\n      server.labels: { zone: 区 }\n",
		"键非法":             "tikv_servers:\n  - host: 10.0.2.21\n    \"a b\": 1\n",
		"键是整数":            "tikv_servers:\n  - host: 10.0.2.21\n    1: 1\n",
		"过长标量":            long,
		"NUL":             "tikv_servers:\n  - host: 10.0.2.21\x00\n",
		"过大":              "tikv_servers:\n  - host: 10.0.2.21\n#" + strings.Repeat("x", 70000) + "\n",
	}
	for name, y := range bad {
		if err := ValidateScaleOut(y, tp); err == nil {
			t.Errorf("%s 应被拒绝:\n%s", name, truncate(y, 200))
		}
	}
}

func TestScaleOutDeepNesting(t *testing.T) {
	deep := "tikv_servers:\n  - host: 10.0.2.21\n    config:\n"
	indent := "      "
	for i := 0; i < 20; i++ {
		deep += indent + "a:\n"
		indent += "  "
	}
	deep += indent + "b: 1\n"
	if err := ValidateScaleOut(deep, testTopo()); err == nil {
		t.Error("过深嵌套应拒绝")
	}
}

func TestScaleOutGlobalUserNeedsKnownUser(t *testing.T) {
	tp := testTopo()
	tp.User = ""
	if err := ValidateScaleOut("global:\n  user: tidb\ntikv_servers:\n  - host: 10.0.2.21\n", tp); err == nil {
		t.Error("不知道集群部署用户时不应放行 global.user")
	}
	tp = testTopo()
	tp.User = "tidb"
	if err := ValidateScaleOut("global:\n  user: tidb\ntikv_servers:\n  - host: 10.0.2.21\n", tp); err != nil {
		t.Error(err)
	}
}

const goodEdit = `global:
  user: tidb
  ssh_port: 22
  ssh_type: builtin
  deploy_dir: /tidb-deploy
  data_dir: /tidb-data
  os: linux
  arch: amd64
monitored:
  node_exporter_port: 9100
  blackbox_exporter_port: 9115
server_configs:
  tidb:
    security.cluster-ssl-key: ""
    performance.token-limit: 1000
  tikv:
    storage.block-cache.capacity: 16GB
    server.labels: { zone: z1 }
    security.cert-allowed-cn: [a, b]
  pd:
    replication.max-replicas: 3
pd_servers:
  - host: 10.0.1.21
    name: pd-10.0.1.21-2379
    client_port: 2379
    peer_port: 2380
    deploy_dir: /tidb-deploy/pd-2379
    data_dir: /tidb-data/pd-2379
tikv_servers:
  - host: 10.0.2.11
    port: 20160
    status_port: 20180
    config:
      raftstore.store-pool-size: 3
tidb_servers:
  - host: 10.0.1.11
    port: 4000
tiflash_servers:
  - host: 10.0.3.11
    tcp_port: 9000
cdc_servers:
  - host: 10.0.4.11
    port: 8300
monitoring_servers:
  - host: 10.0.1.31
    port: 9090
    retention: 30d
grafana_servers:
  - host: 10.0.1.31
    port: 3000
alertmanager_servers:
  - host: 10.0.1.31
    web_port: 9093
    cluster_port: 9094
`

func TestEditConfigAccepts(t *testing.T) {
	if err := ValidateEditConfig(goodEdit, "tidb"); err != nil {
		t.Fatalf("合法拓扑被拒绝: %v", err)
	}
	if err := ValidateEditConfigFull(goodEdit, "tidb", 22, ""); err != nil {
		t.Fatal(err)
	}
	// 与 fakecluster 的 meta.yaml 拓扑风格一致的最小形式。
	min := "global:\n  user: tidb\n  ssh_port: 22\nmonitored:\n  node_exporter_port: 9100\nserver_configs:\n  tidb:\n    security.cluster-ssl-key: \"\"\n    password: \"x\"\npd_servers:\n  - host: 10.0.1.21\n"
	if err := ValidateEditConfig(min, "tidb"); err != nil {
		t.Fatal(err)
	}
}

func TestEditConfigRejects(t *testing.T) {
	repl := func(old, new string) string {
		if !strings.Contains(goodEdit, old) {
			t.Fatalf("fixture 里没有 %q", old)
		}
		return strings.Replace(goodEdit, old, new, 1)
	}
	bad := map[string]string{
		"空":              "",
		"非法 YAML":        "global: [\n",
		"非映射":            "- a\n",
		"缺 global":       "monitored:\n  node_exporter_port: 9100\n",
		"缺 user":         "global:\n  ssh_port: 22\n",
		"user 被改":        repl("user: tidb", "user: root"),
		"user 是列表":       repl("user: tidb", "user: [tidb]"),
		"未知顶层键":          goodEdit + "topology:\n  x: 1\n",
		"顶层 imports":     goodEdit + "imports: {}\n",
		"顶层 pump":        goodEdit + "pump_servers: []\n",
		"global 未知键":     repl("os: linux", "os: linux\n  custom: 1"),
		"ssh_type none":  repl("ssh_type: builtin", "ssh_type: none"),
		"global 目录":      repl("deploy_dir: /tidb-deploy\n", "deploy_dir: /etc\n"),
		"global 目录穿越":    repl("data_dir: /tidb-data\n", "data_dir: /tidb-data/../etc\n"),
		"host 非 IPv4":    repl("host: 10.0.1.21", "host: pd1.example.com"),
		"host 注入":        repl("host: 10.0.1.21", "host: \"10.0.1.21;id\""),
		"端口非法":           repl("client_port: 2379", "client_port: 99999"),
		"端口字符串":          repl("client_port: 2379", "client_port: \"2379\""),
		"条目目录":           repl("deploy_dir: /tidb-deploy/pd-2379", "deploy_dir: /var/lib"),
		"条目未知键":          repl("name: pd-10.0.1.21-2379", "name: pd-10.0.1.21-2379\n    command: rm"),
		"条目 rule_dir":    repl("port: 9090", "port: 9090\n    rule_dir: /etc"),
		"条目 config_file": repl("web_port: 9093", "web_port: 9093\n    config_file: /etc/passwd"),
		"dashboard_dir":  repl("port: 3000", "port: 3000\n    dashboard_dir: /etc"),
		"scrape 配置":      repl("port: 9090", "port: 9090\n    additional_scrape_conf: {a: b}"),
		"条目不是映射":         repl("tidb_servers:\n  - host: 10.0.1.11\n    port: 4000\n", "tidb_servers:\n  - 10.0.1.11\n"),
		"servers 是映射":    repl("cdc_servers:\n  - host: 10.0.4.11\n    port: 8300\n", "cdc_servers:\n  host: 10.0.4.11\n"),
		"组件未知":           repl("  pd:\n    replication.max-replicas: 3\n", "  pd:\n    replication.max-replicas: 3\n  evil:\n    a: 1\n"),
		"配置里的映射值带注入键":    repl("security.cert-allowed-cn: [a, b]", "\"a b\": 1"),
		"列表里嵌套映射":        repl("security.cert-allowed-cn: [a, b]", "security.cert-allowed-cn: [{a: 1}]"),
		"numa 注入":        repl("port: 20160\n    status_port", "port: 20160\n    numa_node: \"0;id\"\n    status_port"),
		"resource 注入":    repl("tikv_servers:\n", "tikv_servers:\n  - host: 10.0.2.12\n    resource_control: {memory_limit: \"1G\\nExecStart=/bin/sh\"}\n"),
		"锚点":             repl("user: tidb", "user: &u tidb"),
		"别名":             repl("os: linux", "os: *u"),
		"自定义标签":          repl("os: linux", "os: !evil linux"),
		"多文档":            goodEdit + "---\nglobal: {user: tidb}\n",
		"重复键":            goodEdit + "global: {user: tidb}\n",
		"控制字符":           repl("os: linux", "os: \"li\\x07nux\""),
		"非 ASCII":        repl("os: linux", "os: 林"),
	}
	for name, y := range bad {
		if err := ValidateEditConfig(y, "tidb"); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	// wantUser 必须给出；ssh_port 期望值不符。
	if err := ValidateEditConfig(goodEdit, ""); err == nil {
		t.Error("没有期望用户应拒绝")
	}
	if err := ValidateEditConfig(goodEdit, "root"); err == nil {
		t.Error("期望用户不符应拒绝")
	}
	if err := ValidateEditConfigFull(goodEdit, "tidb", 2222, ""); err == nil {
		t.Error("ssh_port 与期望不符应拒绝")
	}
}

func TestEditConfigDiffAgainstBase(t *testing.T) {
	base := goodEdit
	mod := func(old, new string) string { return strings.Replace(base, old, new, 1) }
	ok := map[string]string{
		"无变化":                base,
		"server_configs 在线项": mod("storage.block-cache.capacity: 16GB", "storage.block-cache.capacity: 32GB"),
		"server_configs 新增项": mod("  pd:\n    replication.max-replicas: 3\n", "  pd:\n    replication.max-replicas: 3\n    schedule.region-schedule-limit: 1024\n"),
		"实例级 config":         mod("raftstore.store-pool-size: 3", "raftstore.store-pool-size: 4"),
		"实例级新增":              mod("raftstore.store-pool-size: 3", "raftstore.store-pool-size: 3\n      rocksdb.max-background-jobs: 10"),
		"删除目录内配置项（恢复默认）":     mod("      raftstore.store-pool-size: 3\n", "      {}\n"),
		"label 变更":           mod("server.labels: { zone: z1 }", "server.labels: { zone: z2 }"),
		"TiDB 配置文件项":         mod("performance.token-limit: 1000", "performance.token-limit: 2000"),
	}
	for name, y := range ok {
		if err := ValidateEditConfigFull(y, "tidb", 22, base); err != nil {
			t.Errorf("%s 应通过: %v", name, err)
		}
	}
	bad := map[string]string{
		"目录外配置项":            mod("security.cluster-ssl-key: \"\"", "security.cluster-ssl-key: \"/etc/x\""),
		"新增目录外配置项":          mod("  pd:\n    replication.max-replicas: 3\n", "  pd:\n    replication.max-replicas: 3\n    security.cacert-path: /etc/shadow\n"),
		"值不合法":              mod("raftstore.store-pool-size: 3", "raftstore.store-pool-size: 99"),
		"改 host":            mod("host: 10.0.2.11", "host: 10.0.2.99"),
		"改端口":               mod("port: 20160", "port: 20161"),
		"改目录":               mod("data_dir: /tidb-data/pd-2379", "data_dir: /tidb-data/pd-other"),
		"改 global 目录":       mod("deploy_dir: /tidb-deploy\n", "deploy_dir: /tidb-data/x\n"),
		"新增实例":              base + "",
		"改 monitored 端口":    mod("node_exporter_port: 9100", "node_exporter_port: 9101"),
		"改 retention":       mod("retention: 30d", "retention: 90d"),
		"目录外实例 config":      mod("raftstore.store-pool-size: 3", "evil.key: 1"),
		"改 tiflash-learner": mod("  pd:\n    replication.max-replicas: 3\n", "  pd:\n    replication.max-replicas: 3\n  tiflash-learner:\n    log.level: info\n"),
		"系统变量混入":            mod("performance.token-limit: 1000", "performance.token-limit: 1000\n    tidb_mem_quota_query: 1GB"),
	}
	delete(bad, "新增实例")
	bad["新增实例"] = base + "" // 占位，下面改成真正新增
	bad["新增实例"] = strings.Replace(base, "tikv_servers:\n", "tikv_servers:\n  - host: 10.0.2.12\n", 1)
	for name, y := range bad {
		if err := ValidateEditConfigFull(y, "tidb", 22, base); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

// 校验 Validate 把 Topo.User / SSHPort / EditBase 接到了 edit-config 上。
func TestValidateEditConfigWiring(t *testing.T) {
	pol := testPolicy(t.TempDir())
	c := Cmd{Kind: KindClusterEditCfg, Cluster: testCluster, Content: goodEdit}
	if err := Validate(context.Background(), pol, mustTopo(t), c); err != nil {
		t.Fatalf("应通过: %v", err)
	}
	tp := testTopo()
	tp.User = ""
	if err := Validate(context.Background(), pol, StaticTopo(tp), c); err == nil {
		t.Error("拓扑里没有部署用户时必须拒绝 edit-config")
	}
	tp = testTopo()
	tp.User = "root"
	if err := Validate(context.Background(), pol, StaticTopo(tp), c); err == nil {
		t.Error("global.user 与部署用户不符必须拒绝")
	}
	tp = testTopo()
	tp.SSHPort = 2222
	if err := Validate(context.Background(), pol, StaticTopo(tp), c); err == nil {
		t.Error("ssh_port 与拓扑不符必须拒绝")
	}
	tp = testTopo()
	tp.EditBase = goodEdit
	c2 := c
	c2.Content = strings.Replace(goodEdit, "host: 10.0.2.11", "host: 10.0.2.99", 1)
	if err := Validate(context.Background(), pol, StaticTopo(tp), c2); err == nil {
		t.Error("相对 EditBase 的结构性变更必须拒绝")
	}
	c3 := c
	c3.Content = strings.Replace(goodEdit, "storage.block-cache.capacity: 16GB", "storage.block-cache.capacity: 8GB", 1)
	if err := Validate(context.Background(), pol, StaticTopo(tp), c3); err != nil {
		t.Errorf("目录内的变更应通过: %v", err)
	}
}

// cluster.check 带 Content 时按 scale-out 规则校验。
func TestCheckContentUsesScaleOutRules(t *testing.T) {
	pol := testPolicy(t.TempDir())
	good := Cmd{Kind: KindClusterCheck, Cluster: testCluster, Content: goodTiKV}
	if err := Validate(context.Background(), pol, mustTopo(t), good); err != nil {
		t.Fatal(err)
	}
	if Info(KindClusterCheck).Write {
		t.Error("cluster.check 是读类，无需令牌")
	}
	bad := good
	bad.Content = "tikv_servers:\n  - host: 10.0.2.11\n"
	if err := Validate(context.Background(), pol, mustTopo(t), bad); err == nil {
		t.Error("与现有实例冲突的拓扑不得预检")
	}
	bad.Content = "tikv_servers:\n  - host: \"10.0.2.21; id\"\n"
	if err := Validate(context.Background(), pol, mustTopo(t), bad); err == nil {
		t.Error("非法拓扑不得预检")
	}
}
