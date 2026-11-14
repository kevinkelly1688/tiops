// @author: kevinkelly
// @license: Apache License 2.0
// @file: allowlist_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const pdURL = "http://10.0.1.21:2379"

func ctlArgs(rest ...string) []string {
	return append([]string{"tiup", "ctl:v8.5.2", "pd", "-u", pdURL}, rest...)
}

func TestArgvExact(t *testing.T) {
	t.Setenv("TIDBOPS_DB_PWD", "s3cret-pw")
	pol := testPolicy(t.TempDir())
	topo := mustTopo(t)
	c := testCluster
	mysql := func(stmt string) []string {
		return []string{"mysql", "--batch", "-h", "10.0.1.11", "-P", "4000", "-u", "tidbops_ops", "-e", stmt}
	}
	br := func(rest ...string) []string {
		return append([]string{"tiup", "br:v8.5.2"}, rest...)
	}
	cases := []struct {
		name string
		cmd  Cmd
		want []string
	}{
		{"list", Cmd{Kind: KindClusterList}, []string{"tiup", "cluster", "list"}},
		{"tiup.version", Cmd{Kind: KindTiupVersion}, []string{"tiup", "--version"}},
		{"tiup.cluster-version", Cmd{Kind: KindTiupClusterVer}, []string{"tiup", "cluster", "--version"}},
		{"display", Cmd{Kind: KindClusterDisplay, Cluster: c}, []string{"tiup", "cluster", "display", c}},
		{"check", Cmd{Kind: KindClusterCheck, Cluster: c}, []string{"tiup", "cluster", "check", c, "--cluster"}},
		{"pd.query store", Cmd{Kind: KindPDQuery, Cluster: c, Key: "store"}, ctlArgs("store")},
		{"pd.query member", Cmd{Kind: KindPDQuery, Cluster: c, Key: "member"}, ctlArgs("member")},
		{"pd.query scheduler-show", Cmd{Kind: KindPDQuery, Cluster: c, Key: "scheduler-show"}, ctlArgs("scheduler", "show")},
		{"pd.query down-peer", Cmd{Kind: KindPDQuery, Cluster: c, Key: "region-down-peer"}, ctlArgs("region", "check", "down-peer")},
		{"pd.query pending-peer", Cmd{Kind: KindPDQuery, Cluster: c, Key: "region-pending-peer"}, ctlArgs("region", "check", "pending-peer")},
		{"pd.query config-show", Cmd{Kind: KindPDQuery, Cluster: c, Key: "config-show"}, ctlArgs("config", "show")},
		{"cdc.captures", Cmd{Kind: KindCDCCaptures, Cluster: c}, []string{"tiup", "cdc:v8.5.2", "cli", "capture", "list", "--server", "http://10.0.4.11:8300"}},
		{"restart nodes", Cmd{Kind: KindClusterRestart, Cluster: c, Nodes: []string{"10.0.2.11:20160", "10.0.2.12:20160"}},
			[]string{"tiup", "cluster", "restart", c, "-N", "10.0.2.11:20160,10.0.2.12:20160", "-y"}},
		{"restart role", Cmd{Kind: KindClusterRestart, Cluster: c, Role: "tikv"}, []string{"tiup", "cluster", "restart", c, "-R", "tikv", "-y"}},
		{"restart role ticdc", Cmd{Kind: KindClusterRestart, Cluster: c, Role: "ticdc"}, []string{"tiup", "cluster", "restart", c, "-R", "cdc", "-y"}},
		{"stop", Cmd{Kind: KindClusterStop, Cluster: c, Nodes: []string{"10.0.2.11:20160"}}, []string{"tiup", "cluster", "stop", c, "-N", "10.0.2.11:20160", "-y"}},
		{"start (无 -y)", Cmd{Kind: KindClusterStart, Cluster: c, Nodes: []string{"10.0.2.11:20160"}}, []string{"tiup", "cluster", "start", c, "-N", "10.0.2.11:20160"}},
		{"scale-in", Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.2.16:20160"}}, []string{"tiup", "cluster", "scale-in", c, "-N", "10.0.2.16:20160", "-y"}},
		{"prune", Cmd{Kind: KindClusterPrune, Cluster: c}, []string{"tiup", "cluster", "prune", c, "-y"}},
		{"reload nodes", Cmd{Kind: KindClusterReload, Cluster: c, Nodes: []string{"10.0.2.11:20160"}}, []string{"tiup", "cluster", "reload", c, "-N", "10.0.2.11:20160", "-y"}},
		{"reload role", Cmd{Kind: KindClusterReload, Cluster: c, Role: "tikv"}, []string{"tiup", "cluster", "reload", c, "-R", "tikv", "-y"}},
		{"reload role cdc", Cmd{Kind: KindClusterReload, Cluster: c, Role: "cdc"}, []string{"tiup", "cluster", "reload", c, "-R", "cdc", "-y"}},
		{"pd.config-set duration", Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "schedule.max-store-down-time", Value: "1h30m"}, ctlArgs("config", "set", "max-store-down-time", "90m")},
		{"pd.config-set int", Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "replication.max-replicas", Value: "5"}, ctlArgs("config", "set", "max-replicas", "5")},
		{"pd.evict-add", Cmd{Kind: KindPDEvictAdd, Cluster: c, Store: 7}, ctlArgs("scheduler", "add", "evict-leader-scheduler", "7")},
		{"pd.evict-remove", Cmd{Kind: KindPDEvictRemove, Cluster: c, Store: 7}, ctlArgs("scheduler", "remove", "evict-leader-scheduler-7")},
		{"pd.store-limit", Cmd{Kind: KindPDStoreLimit, Cluster: c, Rate: 15}, ctlArgs("store", "limit", "all", "15")},
		{"pd.leader-transfer", Cmd{Kind: KindPDLeaderTransfer, Cluster: c, Name: "pd-10.0.1.22-2379"}, ctlArgs("member", "leader", "transfer", "pd-10.0.1.22-2379")},
		{"sql.set-config role", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "storage.block-cache.capacity", Value: "16gb"},
			mysql("SET CONFIG tikv `storage.block-cache.capacity` = '16GB'")},
		{"sql.set-config instance", Cmd{Kind: KindSQLSetConfig, Cluster: c, Instance: "10.0.2.11:20180", Key: "raftstore.store-pool-size", Value: "3"},
			mysql("SET CONFIG \"10.0.2.11:20180\" `raftstore.store-pool-size` = 3")},
		{"sql.set-config enum", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "log.level", Value: "WARN"},
			mysql("SET CONFIG tikv `log.level` = 'warn'")},
		{"sql.set-global size→bytes", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_mem_quota_query", Value: "1GB"}, mysql("SET GLOBAL tidb_mem_quota_query = 1073741824")},
		{"sql.set-global bool", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_enable_prepared_plan_cache", Value: "off"}, mysql("SET GLOBAL tidb_enable_prepared_plan_cache = OFF")},
		{"sql.set-global int", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_slow_log_threshold", Value: "500"}, mysql("SET GLOBAL tidb_slow_log_threshold = 500")},
		{"sql.set-global enum", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_replica_read", Value: "closest-replicas"}, mysql("SET GLOBAL tidb_replica_read = 'closest-replicas'")},
		{"br.backup-full", Cmd{Kind: KindBRBackupFull, Cluster: c, Value: "local:///data/backup/2025-10-05"},
			br("backup", "full", "--pd", pdURL, "--storage", "local:///data/backup/2025-10-05")},
		{"br.restore-full", Cmd{Kind: KindBRRestoreFull, Cluster: c, Value: "s3://bkt/tidb/full-1"},
			br("restore", "full", "--pd", pdURL, "--storage", "s3://bkt/tidb/full-1")},
		{"br.log-start", Cmd{Kind: KindBRLogStart, Cluster: c, Name: "pitr-1", Value: "s3://bkt/tidb/log"},
			br("log", "start", "--pd", pdURL, "--task-name", "pitr-1", "--storage", "s3://bkt/tidb/log")},
		{"br.log-stop", Cmd{Kind: KindBRLogStop, Cluster: c, Name: "pitr-1"}, br("log", "stop", "--pd", pdURL, "--task-name", "pitr-1")},
		{"br.log-status", Cmd{Kind: KindBRLogStatus, Cluster: c}, br("log", "status", "--pd", pdURL)},
		{"cluster.upgrade", Cmd{Kind: KindClusterUpgrade, Cluster: c, Value: "v8.5.3"}, []string{"tiup", "cluster", "upgrade", c, "v8.5.3", "-y"}},
		{"alert.silence (仅展示)", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "tikv_down", Rate: 30, Value: "维护窗口 静默"},
			[]string{"POST", "http://10.0.1.31:9093/api/v2/silences"}},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.cmd.Kind] = true
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(context.Background(), pol, topo, tc.cmd); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			p, err := Argv(context.Background(), pol, topo, tc.cmd)
			if err != nil {
				t.Fatalf("Argv: %v", err)
			}
			if !reflect.DeepEqual(p.Argv, tc.want) {
				t.Fatalf("argv 不符:\n got  %q\n want %q", p.Argv, tc.want)
			}
			for _, a := range p.Argv {
				if strings.Contains(a, "s3cret-pw") {
					t.Fatalf("密码出现在 argv: %q", a)
				}
			}
		})
	}
	// scale-out / edit-config / 带 Content 的 check 需要临时文件，在 TestTempFiles 里断言。
	for _, k := range []string{KindClusterScaleOut, KindClusterEditCfg} {
		covered[k] = true
	}
	for _, k := range Kinds() {
		if !covered[k] {
			t.Errorf("Kind %s 没有 argv 精确断言", k)
		}
	}
}

func TestSQLPasswordViaEnvOnly(t *testing.T) {
	t.Setenv("TIDBOPS_DB_PWD", "s3cret-pw")
	pol := testPolicy(t.TempDir())
	p, err := Argv(context.Background(), pol, mustTopo(t), Cmd{Kind: KindSQLSetGlobal, Cluster: testCluster, Key: "tidb_slow_log_threshold", Value: "500"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Env, []string{"MYSQL_PWD=s3cret-pw"}) {
		t.Fatalf("env = %q", p.Env)
	}
	if strings.Contains(p.String(), "s3cret-pw") {
		t.Fatal("密码出现在可记录的命令行里")
	}
	// 非 SQL 命令不带密码环境变量。
	p, _ = Argv(context.Background(), pol, mustTopo(t), Cmd{Kind: KindClusterPrune, Cluster: testCluster})
	if len(p.Env) != 0 {
		t.Fatalf("非 SQL 命令不应带环境变量: %q", p.Env)
	}
}

func TestPolicyTopoOverride(t *testing.T) {
	pol := testPolicy(t.TempDir())
	pol.MySQLHost, pol.MySQLPort = "db.internal", 4001
	p, err := Argv(context.Background(), pol, mustTopo(t), Cmd{Kind: KindSQLSetGlobal, Cluster: testCluster, Key: "tidb_slow_log_threshold", Value: "500"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Argv[3] != "db.internal" || p.Argv[5] != "4001" {
		t.Fatalf("应使用 Policy 的连接地址: %q", p.Argv)
	}
	pol.MySQLHost, pol.MySQLPort = "", 0
	topo := func(context.Context, string) (Topo, error) { tp := testTopo(); tp.SQLHost = ""; return tp, nil }
	if _, err := Argv(context.Background(), pol, topo, Cmd{Kind: KindSQLSetGlobal, Cluster: testCluster, Key: "tidb_slow_log_threshold", Value: "500"}); err == nil {
		t.Fatal("没有任何连接地址应被拒绝")
	}
}

// cluster.list 不针对任何集群：带任何字段都拒绝（防夹带）；不需要拓扑、不需要令牌。
func TestClusterListIsBare(t *testing.T) {
	pol := testPolicy(t.TempDir())
	if err := Validate(context.Background(), pol, nil, Cmd{Kind: KindClusterList}); err != nil {
		t.Fatalf("cluster.list 应放行: %v", err)
	}
	for name, c := range map[string]Cmd{
		"带集群名":               {Kind: KindClusterList, Cluster: testCluster},
		"version 带集群名":       {Kind: KindTiupVersion, Cluster: testCluster},
		"cluster-version 带键": {Kind: KindTiupClusterVer, Key: "x"},
		"带节点":                {Kind: KindClusterList, Nodes: []string{"10.0.2.11:20160"}},
		"带内容":                {Kind: KindClusterList, Content: "x"},
		"带键":                 {Kind: KindClusterList, Key: "store"},
	} {
		if err := Validate(context.Background(), pol, nil, c); err == nil {
			t.Errorf("cluster.list %s 应被拒绝", name)
		}
	}
	if Info(KindClusterList).Write {
		t.Error("cluster.list 是读命令")
	}
}

// Policy.Clusters 含 "*"：任何格式合法的集群名都通过「集群名白名单」这一层，但其余校验不放宽。
func TestAnyClusterWildcard(t *testing.T) {
	pol := testPolicy(t.TempDir())
	pol.Clusters = []string{AnyCluster}
	topo := mustTopo(t)
	if err := Validate(context.Background(), pol, topo, Cmd{Kind: KindClusterDisplay, Cluster: "brand-new"}); err != nil {
		t.Fatalf("通配策略应放行任意合法集群名: %v", err)
	}
	for _, bad := range []string{"", "a b", "../x", "x;y", "-x"} {
		if err := Validate(context.Background(), pol, topo, Cmd{Kind: KindClusterDisplay, Cluster: bad}); err == nil {
			t.Errorf("集群名 %q 即使在通配策略下也应被拒绝", bad)
		}
	}
	// 其他层不放宽：不在拓扑里的节点、未知 Kind、字段夹带仍被拒绝
	if err := Validate(context.Background(), pol, topo, Cmd{Kind: KindClusterRestart, Cluster: "brand-new", Nodes: []string{"9.9.9.9:20160"}}); err == nil {
		t.Error("不在拓扑里的节点应被拒绝")
	}
	if err := Validate(context.Background(), pol, topo, Cmd{Kind: "cluster.destroy", Cluster: "brand-new"}); err == nil {
		t.Error("未知 Kind 应被拒绝")
	}
	// 写命令仍需令牌：无令牌的重启在 Runner 层被拒绝（见 runner_test 的令牌用例），这里只确认通配不改变 Kind 属性
	if !Info(KindClusterRestart).Write {
		t.Error("restart 必须是写命令")
	}
}

func TestInfo(t *testing.T) {
	writes := map[string]bool{
		KindClusterList: false, KindTiupVersion: false, KindTiupClusterVer: false, KindClusterDisplay: false, KindClusterCheck: false, KindPDQuery: false, KindCDCCaptures: false, KindBRLogStatus: false,
		KindClusterRestart: true, KindClusterStop: true, KindClusterStart: true, KindClusterScaleOut: true, KindClusterScaleIn: true,
		KindClusterPrune: true, KindClusterEditCfg: true, KindClusterReload: true, KindPDConfigSet: true, KindPDEvictAdd: true,
		KindPDEvictRemove: true, KindPDStoreLimit: true, KindPDLeaderTransfer: true, KindSQLSetConfig: true, KindSQLSetGlobal: true,
		KindBRBackupFull: true, KindBRRestoreFull: true, KindBRLogStart: true, KindBRLogStop: true, KindClusterUpgrade: true, KindAlertSilence: true,
	}
	for k, w := range writes {
		in := Info(k)
		if !in.Known || in.Write != w || in.Timeout <= 0 || in.Risk == "" {
			t.Errorf("Info(%s) = %+v，期望 Write=%v", k, in, w)
		}
	}
	if len(Kinds()) != len(writes) {
		t.Errorf("Kinds 数量 %d != %d", len(Kinds()), len(writes))
	}
	for _, k := range []string{"", "cluster.destroy", "bash"} {
		if in := Info(k); in.Known || !in.Write {
			t.Errorf("未知 Kind %q 必须按写命令处理: %+v", k, in)
		}
	}
	if Info(KindClusterUpgrade).Timeout.Hours() != 3 || Info(KindClusterDisplay).Timeout.Seconds() != 30 || Info(KindClusterScaleOut).Timeout.Hours() != 1 {
		t.Error("超时与 A3 表不一致")
	}
}

func rejects(t *testing.T, name string, c Cmd) {
	t.Helper()
	pol := testPolicy(t.TempDir())
	topo := mustTopo(t)
	if err := Validate(context.Background(), pol, topo, c); err == nil {
		t.Errorf("%s: Validate 应拒绝: %+v", name, c)
	}
	if p, err := Argv(context.Background(), pol, topo, c); err == nil {
		t.Errorf("%s: Argv 应拒绝: %+v → %q", name, c, p.Argv)
	}
}

func TestRejectInjection(t *testing.T) {
	c := testCluster
	node := "10.0.2.11:20160"
	// 节点
	for _, n := range []string{
		node + ";reboot", node + "&&id", "$(id):20160", node + " -y", node + "\n", node + "\r\n", node + "`id`",
		"10.0.2.99:20160", "10.0.2.11:20161", node + "|cat", node + ">/tmp/x", "10.0.2.11:20160,10.0.2.12:20160",
		"-N:20160", "", "10.0.2.11", "10.0.2.11:99999999", "10.0.2.11:20160'", `10.0.2.11:20160"`, node + `\`, node + "\x00",
		"${IFS}:20160", "10.0.2.11:20160#",
	} {
		rejects(t, "node "+n, Cmd{Kind: KindClusterRestart, Cluster: c, Nodes: []string{n}})
		rejects(t, "stop "+n, Cmd{Kind: KindClusterStop, Cluster: c, Nodes: []string{n}})
	}
	rejects(t, "重复节点", Cmd{Kind: KindClusterStop, Cluster: c, Nodes: []string{node, node}})
	rejects(t, "空 nodes", Cmd{Kind: KindClusterStop, Cluster: c})
	// 集群名
	for _, cl := range []string{"../etc", "prod-tidb-01/../x", "prod-tidb-01;ls", "unknown", "", "prod-tidb-01 ", "-prod", `prod"`, "prod\n", "..", "/etc/passwd", "prod-tidb-01$(id)", "prod`id`"} {
		rejects(t, "cluster "+cl, Cmd{Kind: KindClusterDisplay, Cluster: cl})
		rejects(t, "cluster "+cl, Cmd{Kind: KindClusterPrune, Cluster: cl})
	}
	// 值
	for _, v := range []string{"30m'", `30m"`, `30m\`, "30m\n", "30m; DROP USER x", "1 or 1", "30m`id`", "$(id)", "", " 30m", "30m ", "-1", "9999999999999h", "30", "30M", "1m1h", "1h1h", "٣٠m"} {
		rejects(t, "pd value "+v, Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "schedule.max-store-down-time", Value: v})
	}
	for _, v := range []string{"16GB' ; DROP USER x; --", `16GB\`, "16GB\n", "16 GB", "16", "-16GB", "1e3GB", "16GB;", "99999999GB", "16PB", "0x10GB", "16GB'", `16GB"`} {
		rejects(t, "size value "+v, Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "storage.block-cache.capacity", Value: v})
	}
	for _, v := range []string{"3; DROP", "3'", "0", "17", "-1", "3.5", " 3", "3\n", "0x3", "1e1", "+3", "99999999999", ""} {
		rejects(t, "int value "+v, Cmd{Kind: KindSQLSetConfig, Cluster: c, Instance: "10.0.2.11:20180", Key: "raftstore.store-pool-size", Value: v})
	}
	for _, v := range []string{"info'", "trace", "warn;", "inf", "debug\n", "info or 1", `"info"`} {
		rejects(t, "enum value "+v, Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "log.level", Value: v})
	}
	for _, v := range []string{"maybe", "ON;", "ON'", "2"} {
		rejects(t, "bool value "+v, Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_enable_prepared_plan_cache", Value: v})
	}
	// Key 不在目录 / 不适用
	rejects(t, "未知 key", Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "schedule.foo", Value: "1"})
	rejects(t, "key 带尾部空格", Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "replication.max-replicas ", Value: "3"})
	rejects(t, "key 注入", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "log.level` = 'x'; DROP", Value: "info"})
	rejects(t, "key 反引号", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "log.level`", Value: "info"})
	rejects(t, "pd 非在线项", Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "log.level", Value: "info"})
	rejects(t, "pd 配 tikv 的项", Cmd{Kind: KindPDConfigSet, Cluster: c, Key: "storage.block-cache.capacity", Value: "16GB"})
	rejects(t, "set-config 用系统变量", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tidb", Key: "tidb_mem_quota_query", Value: "1GB"})
	rejects(t, "set-global 用配置项", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "log.level", Value: "info"})
	rejects(t, "set-global 任意变量", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "sql_mode", Value: "ON"})
	rejects(t, "set-global 变量名注入", Cmd{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_slow_log_threshold;DROP", Value: "500"})
	rejects(t, "非在线项 SET CONFIG", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "server.grpc-concurrency", Value: "5"})
	rejects(t, "ticdc 不支持 SET CONFIG", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "ticdc", Key: "log-level", Value: "info"})
	rejects(t, "set-config 缺 role/instance", Cmd{Kind: KindSQLSetConfig, Cluster: c, Key: "log.level", Value: "info"})
	rejects(t, "role 与 instance 同时", Cmd{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Instance: "10.0.2.11:20180", Key: "log.level", Value: "info"})
	rejects(t, "instance 用的是业务端口", Cmd{Kind: KindSQLSetConfig, Cluster: c, Instance: "10.0.2.11:20160", Key: "log.level", Value: "info"})
	rejects(t, "instance 不在拓扑", Cmd{Kind: KindSQLSetConfig, Cluster: c, Instance: "10.0.9.9:20180", Key: "log.level", Value: "info"})
	rejects(t, "instance 注入", Cmd{Kind: KindSQLSetConfig, Cluster: c, Instance: `10.0.2.11:20180" OR "1`, Key: "log.level", Value: "info"})
	// 未登记的 Kind
	for _, k := range []string{"cluster.destroy", "cluster.clean", "cluster.patch", "bash", "sh", "ssh", "rm", "", "cluster.display ", "CLUSTER.DISPLAY", "cluster.exec", "cluster.deploy", "cluster.rename", "pd.config-set;id", "tiup", "bash -c id", "cluster.import", "../cluster.display"} {
		rejects(t, "kind "+k, Cmd{Kind: k, Cluster: c})
	}
	// pd.query 只允许固定名字
	for _, k := range []string{"region-all", "store;x", "config set x 1", "", "Store", "store ", "region", "scheduler", "scheduler-add"} {
		rejects(t, "pd.query "+k, Cmd{Kind: KindPDQuery, Cluster: c, Key: k})
	}
	// store / rate / name / role
	rejects(t, "store 0", Cmd{Kind: KindPDEvictAdd, Cluster: c})
	rejects(t, "store 负数", Cmd{Kind: KindPDEvictAdd, Cluster: c, Store: -1})
	rejects(t, "rate 0", Cmd{Kind: KindPDStoreLimit, Cluster: c})
	rejects(t, "rate 201", Cmd{Kind: KindPDStoreLimit, Cluster: c, Rate: 201})
	rejects(t, "rate 负数", Cmd{Kind: KindPDStoreLimit, Cluster: c, Rate: -5})
	for _, n := range []string{"", "pd 1", "pd;1", "-pd", "pd\n1", "pd$(id)", `pd"`} {
		rejects(t, "member "+n, Cmd{Kind: KindPDLeaderTransfer, Cluster: c, Name: n})
	}
	for _, r := range []string{"root", "tikv;x", "TIKV", "", "prometheus", "tikv ", "-R"} {
		rejects(t, "role "+r, Cmd{Kind: KindClusterReload, Cluster: c, Role: r})
	}
	rejects(t, "reload nodes+role", Cmd{Kind: KindClusterReload, Cluster: c, Role: "tikv", Nodes: []string{node}})
	rejects(t, "reload 都没有", Cmd{Kind: KindClusterReload, Cluster: c})
	rejects(t, "restart nodes+role", Cmd{Kind: KindClusterRestart, Cluster: c, Role: "tikv", Nodes: []string{node}})
	rejects(t, "restart 都没有", Cmd{Kind: KindClusterRestart, Cluster: c})
	// 备份路径
	for _, v := range []string{"local:///data/backup/../../etc", "local:///etc/passwd", "s3://other/x", "local:///data/backup/x;id", "local:///data/backupx", "", "-rf", "local:///data/backup/ x",
		"local:///data/backup/$(id)", "local:///data/backup/`id`", "s3://bkt/tidb/x?access-key=AK&secret-access-key=SK", "local:///data/backup/..", "local:///data/backup/a\nb", "s3://bkt/tidb", "S3://bkt/tidb/x", "--storage"} {
		rejects(t, "backup "+v, Cmd{Kind: KindBRBackupFull, Cluster: c, Value: v})
		rejects(t, "restore "+v, Cmd{Kind: KindBRRestoreFull, Cluster: c, Value: v})
		rejects(t, "logstart "+v, Cmd{Kind: KindBRLogStart, Cluster: c, Name: "t1", Value: v})
	}
	for _, n := range []string{"", "t 1", "-t", "t;1", "t\n", strings.Repeat("a", 65)} {
		rejects(t, "task "+n, Cmd{Kind: KindBRLogStop, Cluster: c, Name: n})
	}
	// upgrade
	for _, v := range []string{"v8.5", "8.5.3", "v8.5.3; id", "latest", "nightly", "", "v8.5.3-rc", "v8.5.3 ", "v8.1.0", "v7.5.0", "v8.5.3\n"} {
		rejects(t, "upgrade "+v, Cmd{Kind: KindClusterUpgrade, Cluster: c, Value: v})
	}
	// alert.silence
	rejects(t, "alert 名含空格", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "a b", Rate: 5})
	rejects(t, "alert 名注入", Cmd{Kind: KindAlertSilence, Cluster: c, Key: `a";x`, Rate: 5})
	rejects(t, "alert 名为空", Cmd{Kind: KindAlertSilence, Cluster: c, Rate: 5})
	rejects(t, "alert rate 0", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "x"})
	rejects(t, "alert rate 1441", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 1441})
	rejects(t, "alert instance 不在拓扑", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 5, Instance: "10.9.9.9:1"})
	rejects(t, "alert 备注换行", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 5, Value: "a\nb"})
	rejects(t, "alert 备注控制字符", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 5, Value: "a\x07b"})
	rejects(t, "alert 备注过长", Cmd{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 5, Value: strings.Repeat("字", 201)})
}

func TestAlertSilenceAccepts(t *testing.T) {
	pol := testPolicy(t.TempDir())
	for _, c := range []Cmd{
		{Kind: KindAlertSilence, Cluster: testCluster, Key: "TiKV_down:1.%-x", Rate: 1},
		{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 1440, Instance: "10.0.2.11:20160", Value: strings.Repeat("字", 200)},
		{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 60, Instance: "10.0.2.11:20180"},
	} {
		if err := Validate(context.Background(), pol, mustTopo(t), c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	pol.AlertURL = ""
	if err := Validate(context.Background(), pol, mustTopo(t), Cmd{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 5}); err == nil {
		t.Error("未配置 AlertURL 应被拒绝")
	}
	pol.AlertURL = "http://x;rm -rf /"
	if err := Validate(context.Background(), pol, mustTopo(t), Cmd{Kind: KindAlertSilence, Cluster: testCluster, Key: "x", Rate: 5}); err == nil {
		t.Error("非法 AlertURL 应被拒绝")
	}
}

func TestRejectExtraFields(t *testing.T) {
	c := testCluster
	node := []string{"10.0.2.11:20160"}
	cases := []Cmd{
		{Kind: KindClusterDisplay, Cluster: c, Nodes: node},
		{Kind: KindClusterDisplay, Cluster: c, Value: "x"},
		{Kind: KindClusterDisplay, Cluster: c, Key: "x"},
		{Kind: KindClusterDisplay, Cluster: c, Content: "x: 1"},
		{Kind: KindClusterDisplay, Cluster: c, Role: "tikv"},
		{Kind: KindClusterDisplay, Cluster: c, Store: 1},
		{Kind: KindClusterDisplay, Cluster: c, Rate: 1},
		{Kind: KindClusterDisplay, Cluster: c, Name: "x"},
		{Kind: KindClusterDisplay, Cluster: c, Instance: "10.0.2.11:20180"},
		{Kind: KindClusterRestart, Cluster: c, Nodes: node, Content: "x: 1"},
		{Kind: KindClusterRestart, Cluster: c, Nodes: node, Key: "x"},
		{Kind: KindClusterStart, Cluster: c, Nodes: node, Role: "tikv"}, // start 不支持 Role
		{Kind: KindClusterStop, Cluster: c, Nodes: node, Value: "x"},
		{Kind: KindClusterPrune, Cluster: c, Nodes: node},
		{Kind: KindClusterScaleIn, Cluster: c, Nodes: node, Rate: 1},
		{Kind: KindPDQuery, Cluster: c, Key: "store", Value: "x"},
		{Kind: KindPDQuery, Cluster: c, Key: "store", Nodes: node},
		{Kind: KindPDEvictAdd, Cluster: c, Store: 1, Key: "x"},
		{Kind: KindPDEvictAdd, Cluster: c, Store: 1, Nodes: node},
		{Kind: KindPDStoreLimit, Cluster: c, Rate: 15, Store: 1},
		{Kind: KindPDLeaderTransfer, Cluster: c, Name: "pd-1", Value: "x"},
		{Kind: KindPDConfigSet, Cluster: c, Key: "replication.max-replicas", Value: "3", Role: "pd"},
		{Kind: KindPDConfigSet, Cluster: c, Key: "replication.max-replicas", Value: "3", Instance: "10.0.1.21:2379"},
		{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_slow_log_threshold", Value: "500", Role: "tidb"},
		{Kind: KindSQLSetGlobal, Cluster: c, Key: "tidb_slow_log_threshold", Value: "500", Content: "x"},
		{Kind: KindSQLSetConfig, Cluster: c, Role: "tikv", Key: "log.level", Value: "info", Nodes: node},
		{Kind: KindBRBackupFull, Cluster: c, Value: "local:///data/backup/x", Name: "x"},
		{Kind: KindBRBackupFull, Cluster: c, Value: "local:///data/backup/x", Key: "x"},
		{Kind: KindBRLogStop, Cluster: c, Name: "t", Value: "local:///data/backup/x"},
		{Kind: KindBRLogStatus, Cluster: c, Name: "t"},
		{Kind: KindCDCCaptures, Cluster: c, Nodes: node},
		{Kind: KindClusterUpgrade, Cluster: c, Value: "v8.5.3", Nodes: node},
		{Kind: KindClusterUpgrade, Cluster: c, Value: "v8.5.3", Key: "x"},
		{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 5, Nodes: node},
		{Kind: KindAlertSilence, Cluster: c, Key: "x", Rate: 5, Content: "x"},
		{Kind: KindClusterScaleOut, Cluster: c, Content: "tikv_servers: []", Nodes: node},
		{Kind: KindClusterEditCfg, Cluster: c, Content: "global: {user: tidb}", Value: "x"},
	}
	for i, cm := range cases {
		rejects(t, "extra#"+string(rune('A'+i%26)), cm)
	}
}

func TestScaleInGuards(t *testing.T) {
	c := testCluster
	// 6 个 TiKV：缩 3 个剩 3，允许；缩 4 个剩 2，拒绝。
	ok := Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.2.14:20160", "10.0.2.15:20160", "10.0.2.16:20160"}}
	if err := Validate(context.Background(), testPolicy(""), mustTopo(t), ok); err != nil {
		t.Fatalf("剩 3 个 TiKV 应允许: %v", err)
	}
	rejects(t, "剩 2 个 TiKV", Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.2.13:20160", "10.0.2.14:20160", "10.0.2.15:20160", "10.0.2.16:20160"}})
	rejects(t, "移除全部 PD", Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.1.21:2379", "10.0.1.22:2379", "10.0.1.23:2379"}})
	rejects(t, "移除全部 TiDB", Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.1.11:4000", "10.0.1.12:4000", "10.0.1.13:4000"}})
	// MinTiKV 可下调（小型测试环境）。
	pol := testPolicy("")
	pol.MinTiKV = 1
	big := Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.2.12:20160", "10.0.2.13:20160", "10.0.2.14:20160", "10.0.2.15:20160", "10.0.2.16:20160"}}
	if err := Validate(context.Background(), pol, mustTopo(t), big); err != nil {
		t.Fatalf("MinTiKV=1 应允许: %v", err)
	}
	// 缩容非 TiKV 不受 TiKV 下限影响。
	if err := Validate(context.Background(), testPolicy(""), mustTopo(t), Cmd{Kind: KindClusterScaleIn, Cluster: c, Nodes: []string{"10.0.1.13:4000"}}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeAcceptsSameAndHigher(t *testing.T) {
	for _, v := range []string{"v8.5.2", "v8.5.3", "v8.6.0", "v9.0.0"} {
		if err := Validate(context.Background(), testPolicy(""), mustTopo(t), Cmd{Kind: KindClusterUpgrade, Cluster: testCluster, Value: v}); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}

func TestTopoRules(t *testing.T) {
	pol := testPolicy(t.TempDir())
	// display / prune 不需要拓扑（DisplayTopo 自己要调 display，必须避免递归）。
	boom := func(context.Context, string) (Topo, error) { return Topo{}, errors.New("boom") }
	for _, c := range []Cmd{{Kind: KindClusterDisplay, Cluster: testCluster}, {Kind: KindClusterPrune, Cluster: testCluster}, {Kind: KindClusterCheck, Cluster: testCluster}} {
		if err := Validate(context.Background(), pol, boom, c); err != nil {
			t.Errorf("%s 不应依赖拓扑: %v", c.Kind, err)
		}
	}
	// 其它命令拓扑不可用就拒绝（而不是放行）。
	if err := Validate(context.Background(), pol, boom, Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}); err == nil {
		t.Error("拓扑失败应拒绝")
	}
	if err := Validate(context.Background(), pol, nil, Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}); err == nil {
		t.Error("没有拓扑来源应拒绝")
	}
	// 拓扑里版本 / PD 异常时，pd-ctl 类命令必须拒绝（版本与 PD 只来自拓扑）。
	bad := testTopo()
	bad.Version = "v8.5.2; id"
	if err := Validate(context.Background(), pol, StaticTopo(bad), Cmd{Kind: KindPDQuery, Cluster: testCluster, Key: "store"}); err == nil {
		t.Error("非法版本应拒绝")
	}
	bad = testTopo()
	bad.PD = "http://10.0.1.21:2379 --evil"
	if err := Validate(context.Background(), pol, StaticTopo(bad), Cmd{Kind: KindPDQuery, Cluster: testCluster, Key: "store"}); err == nil {
		t.Error("非法 PD 地址应拒绝")
	}
	bad = testTopo()
	bad.PD = ""
	if err := Validate(context.Background(), pol, StaticTopo(bad), Cmd{Kind: KindPDEvictAdd, Cluster: testCluster, Store: 1}); err == nil {
		t.Error("没有 PD 应拒绝")
	}
	// 集群不在 Policy 里，即使拓扑函数认识它。
	pol2 := pol
	pol2.Clusters = []string{"other"}
	if err := Validate(context.Background(), pol2, mustTopo(t), Cmd{Kind: KindClusterDisplay, Cluster: testCluster}); err == nil {
		t.Error("不在 Policy.Clusters 里的集群应拒绝")
	}
	// 没有 TiCDC 实例时 cdc.captures 拒绝。
	noCDC := testTopo()
	for k, v := range noCDC.Nodes {
		if v == "ticdc" {
			delete(noCDC.Nodes, k)
		}
	}
	if err := Validate(context.Background(), pol, StaticTopo(noCDC), Cmd{Kind: KindCDCCaptures, Cluster: testCluster}); err == nil {
		t.Error("没有 TiCDC 应拒绝")
	}
}

func TestPolicyChecks(t *testing.T) {
	topo := mustTopo(t)
	sqlCmd := Cmd{Kind: KindSQLSetGlobal, Cluster: testCluster, Key: "tidb_slow_log_threshold", Value: "500"}
	for name, mod := range map[string]func(*Policy){
		"无运维账号":    func(p *Policy) { p.MySQLUser = "" },
		"账号以 - 开头": func(p *Policy) { p.MySQLUser = "-u" },
		"密码变量名非法":  func(p *Policy) { p.MySQLPwdEnv = "A B" },
	} {
		pol := testPolicy("")
		mod(&pol)
		if err := Validate(context.Background(), pol, topo, sqlCmd); err == nil {
			t.Errorf("%s 应拒绝", name)
		}
	}
	pol := testPolicy("")
	pol.BackupRoots = nil
	if err := Validate(context.Background(), pol, topo, Cmd{Kind: KindBRBackupFull, Cluster: testCluster, Value: "local:///data/backup/x"}); err == nil {
		t.Error("没有备份前缀白名单应拒绝")
	}
	pol.BackupRoots = []string{"-evil"}
	if err := Validate(context.Background(), pol, topo, Cmd{Kind: KindBRBackupFull, Cluster: testCluster, Value: "-evil/x"}); err == nil {
		t.Error("以 - 开头的备份前缀应拒绝")
	}
}

func TestTimeoutOverride(t *testing.T) {
	pol := testPolicy("")
	if timeoutFor(pol, KindClusterDisplay) != Info(KindClusterDisplay).Timeout {
		t.Error("默认超时")
	}
	pol.Timeouts = map[string]timeDuration{KindClusterDisplay: 123}
	if timeoutFor(pol, KindClusterDisplay) != 123 {
		t.Error("覆盖超时")
	}
}

func TestTempFiles(t *testing.T) {
	dir := t.TempDir()
	pol := testPolicy(dir)
	topo := mustTopo(t)
	scale := "tikv_servers:\n  - host: 10.0.2.21\n    deploy_dir: /tidb-deploy/tikv-20160\n    data_dir: /tidb-data/tikv-20160\n    config:\n      server.labels: { zone: \"z1\", rack: \"r00\", host: \"10-0-2-21\" }\n      storage.block-cache.capacity: \"16GB\"\n"
	edit := "global:\n  user: tidb\n  ssh_port: 22\nserver_configs:\n  tikv:\n    storage.block-cache.capacity: 16GB\n"
	cases := []struct {
		name string
		cmd  Cmd
		pre  []string
		post []string
		body string
	}{
		{"scale-out", Cmd{Kind: KindClusterScaleOut, Cluster: testCluster, Content: scale}, []string{"tiup", "cluster", "scale-out", testCluster}, []string{"-y"}, scale},
		{"edit-config", Cmd{Kind: KindClusterEditCfg, Cluster: testCluster, Content: edit}, []string{"tiup", "cluster", "edit-config", testCluster, "--topology-file"}, []string{"-y"}, edit},
		{"check+content", Cmd{Kind: KindClusterCheck, Cluster: testCluster, Content: scale}, []string{"tiup", "cluster", "check", testCluster}, []string{"--cluster"}, scale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Argv(context.Background(), pol, topo, tc.cmd)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.TempFiles) != 1 {
				t.Fatalf("临时文件: %v", p.TempFiles)
			}
			file := p.TempFiles[0]
			if filepath.Dir(file) != dir {
				t.Errorf("临时文件不在 TempDir: %s", file)
			}
			// argv 形如 pre... <file> post...
			want := append(append(append([]string{}, tc.pre...), filepath.ToSlash(file)), tc.post...)
			if !reflect.DeepEqual(p.Argv, want) {
				t.Errorf("argv:\n got  %q\n want %q", p.Argv, want)
			}
			assertPerm0600(t, file)
			b := readFile(t, file)
			if b != tc.body {
				t.Errorf("内容不符: %q", b)
			}
			p.Cleanup()
			if fileExists(file) {
				t.Error("Cleanup 后临时文件应被删除")
			}
			p.Cleanup() // 可重复调用
		})
	}
}

func TestTempFileRejections(t *testing.T) {
	topo := mustTopo(t)
	cmd := Cmd{Kind: KindClusterScaleOut, Cluster: testCluster, Content: "tikv_servers:\n  - host: 10.0.2.21\n"}
	for name, dir := range map[string]string{"未配置": "", "相对路径": "relative/dir", "不存在": filepath.Join(t.TempDir(), "nope")} {
		pol := testPolicy(dir)
		if _, err := Argv(context.Background(), pol, topo, cmd); err == nil {
			t.Errorf("TempDir %s 应拒绝", name)
		}
	}
	// 校验失败时不留下临时文件。
	dir := t.TempDir()
	bad := Cmd{Kind: KindClusterScaleOut, Cluster: testCluster, Content: "pd_servers:\n  - host: 10.0.2.21\n    evil: 1\n"}
	if _, err := Argv(context.Background(), testPolicy(dir), topo, bad); err == nil {
		t.Fatal("非法拓扑应拒绝")
	}
	if n := countFiles(t, dir); n != 0 {
		t.Errorf("校验失败不应留下临时文件，剩 %d 个", n)
	}
}
