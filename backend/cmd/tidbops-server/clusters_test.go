// @author: kevinkelly
// @license: Apache License 2.0
// @file: clusters_test.go
// @time: 2026/10/28

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tidbops/internal/clusters"
	"tidbops/internal/collect"
	xexec "tidbops/internal/exec"
	"tidbops/internal/fakecluster"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

func fixturesDir(t *testing.T) string {
	t.Helper()
	d, _ := filepath.Abs(filepath.Join("..", "..", "..", "fixtures"))
	for _, n := range []string{"demo", "stg", "dev"} {
		if _, err := os.Stat(filepath.Join(d, n, "manifest.json")); err != nil {
			t.Skipf("缺少回放数据包 %s", n)
		}
	}
	return d
}

func replayServer(t *testing.T, fx string) *server.Server {
	t.Helper()
	rp, err := source.NewReplay(filepath.Join(fx, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	return server.New(server.Config{Col: collect.New(rp, source.CatalogParams{Cluster: rp.Manifest().Cluster, Home: "/home/tidb", Now: time.Now()}), Mode: "replay", Web: t.TempDir(), Logf: func(string, ...any) {}})
}

// configureOps 的规则：回放集群恒用模拟执行器；直连集群在进程是 sim 时不配执行器；策略集群名只含自己，并与策略文件的显式白名单取交集；
// SQL 地址 / Alertmanager 取自该集群而不继承主集群。
func TestConfigureOpsRules(t *testing.T) {
	s := replayServer(t, fixturesDir(t))
	base := xexec.Policy{Clusters: []string{"prod-tidb-01"}, TiupBin: "/usr/bin/tiup", MySQLHost: "10.0.1.11", MySQLPort: 4000, MySQLUser: "ops", AlertURL: "http://10.0.1.31:9093", BackupRoots: []string{"s3://b/p/"}}
	build := func(em string, polClusters []string, spec clusters.Spec) (server.OpsConfig, error) {
		b := &backendBuilder{a: hubArgs{sudoUser: "tidb", execBin: "/opt/tidbops/bin/tidbops-server", simDelay: time.Millisecond}, sh: &opsShared{pol: base, polClusters: polClusters, key: []byte("0123456789abcdef0123456789abcdef"), em: em}}
		var cfg server.OpsConfig
		err := b.configureOps(s, &cfg, spec, spec.Alert, spec.MySQLHost, spec.MySQLPort)
		return cfg, err
	}
	replay := clusters.Spec{ID: "stg", Name: "stg-tidb-01", Env: "staging", Mode: clusters.ModeReplay}
	live := clusters.Spec{ID: "liv", Name: "liv-tidb", Env: "prod", Mode: clusters.ModeLive, Alert: "http://10.9.0.1:9093", MySQLHost: "10.9.0.2", MySQLPort: 4001}

	for _, em := range []string{"sim", "direct", "sudo", "none"} {
		cfg, err := build(em, nil, replay)
		if err != nil {
			t.Fatal(err)
		}
		sim, ok := cfg.Runner.(*xexec.SimRunner)
		if !ok || cfg.ExecMode != "sim" || !cfg.Sim {
			t.Errorf("进程 exec=%s：回放集群必须用模拟执行器，得到 %T / %s", em, cfg.Runner, cfg.ExecMode)
			continue
		}
		if len(sim.Pol.Clusters) != 1 || sim.Pol.Clusters[0] != "stg-tidb-01" {
			t.Errorf("策略集群名应只含该集群: %v", sim.Pol.Clusters)
		}
	}
	if cfg, _ := build("sim", nil, live); cfg.Runner != nil || cfg.ExecMode != "none" || cfg.Sim {
		t.Errorf("进程是 sim 时直连集群不应配执行器（避免假装执行）: %T %s", cfg.Runner, cfg.ExecMode)
	}
	cfg, err := build("direct", nil, live)
	if err != nil {
		t.Fatal(err)
	}
	dr, ok := cfg.Runner.(xexec.DirectRunner)
	if !ok || cfg.ExecMode != "direct" || cfg.Prober == nil {
		t.Fatalf("direct: %T %s", cfg.Runner, cfg.ExecMode)
	}
	if len(dr.Pol.Clusters) != 1 || dr.Pol.Clusters[0] != "liv-tidb" || dr.Pol.AlertURL != "http://10.9.0.1:9093" || dr.Pol.MySQLHost != "10.9.0.2" || dr.Pol.MySQLPort != 4001 {
		t.Errorf("集群相关的策略项应取自该集群而非主集群: %+v", dr.Pol)
	}
	if dr.Pol.TiupBin != "/usr/bin/tiup" || dr.Pol.MySQLUser != "ops" || len(dr.Pol.BackupRoots) != 1 {
		t.Errorf("进程级的策略项应继承: %+v", dr.Pol)
	}
	// 策略文件显式列了集群白名单：不在其中的集群，执行器白名单为空（命令会被拒绝，由预检给警告）
	cfg, _ = build("direct", []string{"prod-tidb-01"}, live)
	if dr := cfg.Runner.(xexec.DirectRunner); len(dr.Pol.Clusters) != 0 {
		t.Errorf("策略文件白名单不含该集群时，不应放行: %v", dr.Pol.Clusters)
	}
	cfg, _ = build("direct", []string{"prod-tidb-01", "liv-tidb"}, live)
	if dr := cfg.Runner.(xexec.DirectRunner); len(dr.Pol.Clusters) != 1 || dr.Pol.Clusters[0] != "liv-tidb" {
		t.Errorf("在白名单里应放行: %v", dr.Pol.Clusters)
	}
	cfg, _ = build("sudo", nil, live)
	if sr, ok := cfg.Runner.(xexec.SudoRunner); !ok || sr.User != "tidb" || sr.Bin != "/opt/tidbops/bin/tidbops-server" {
		t.Errorf("sudo: %#v", cfg.Runner)
	}
	// 集群自己的 tiup 路径覆盖进程的
	live.Tiup = "/opt/tiup/bin/tiup"
	cfg, _ = build("direct", nil, live)
	if dr := cfg.Runner.(xexec.DirectRunner); dr.Pol.TiupBin != "/opt/tiup/bin/tiup" {
		t.Errorf("集群级 tiup 路径: %v", dr.Pol.TiupBin)
	}
}

func devSeedArgs(dir, fx string, devSeed bool, mode string) hubArgs {
	return hubArgs{dir: dir, env: "staging", mode: mode, fixtures: filepath.Join(fx, "demo"), cluster: "prod-tidb-01", devSeed: devSeed, simDelay: time.Millisecond, cacheTTL: time.Second, snapTTL: time.Second}
}

func startForTest(t *testing.T, dir, fx string, devSeed bool, mode string) (*server.Hub, func()) {
	t.Helper()
	s := replayServer(t, fx)
	sh := &opsShared{}
	oa := opsArgs{dir: dir, env: "staging", mode: "replay", devSeed: devSeed, simDelay: time.Millisecond, cluster: "prod-tidb-01", readonly: filepath.Join(dir, "READONLY")}
	if err := enableOps(s, oa, sh); err != nil {
		t.Fatal(err)
	}
	hub := startHub(s, sh, devSeedArgs(dir, fx, devSeed, mode))
	return hub, func() { hub.Close(); s.Close(); _ = sh.audit.Close() }
}

// --dev-seed 的回放模式：数据目录里没有 clusters.json 时自动登记 stg / dev 两个演示集群；只登记一次；非 --dev-seed 绝不登记。
func TestDevSeedRegistersDemoClustersOnce(t *testing.T) {
	fx := fixturesDir(t)

	// 生产模式（无 --dev-seed）：不登记任何集群
	dirProd := t.TempDir()
	hub, closeFn := startForTest(t, dirProd, fx, false, "replay")
	if n := len(hub.Registry().List()); n != 1 {
		t.Errorf("非 --dev-seed 不应自动登记集群，得到 %d 个", n)
	}
	closeFn()
	if _, err := os.Stat(filepath.Join(dirProd, "clusters.json")); err == nil {
		t.Error("非 --dev-seed 不应创建 clusters.json")
	}

	// 演示模式：首次启动登记两个
	dir := t.TempDir()
	hub, closeFn = startForTest(t, dir, fx, true, "replay")
	got := map[string]string{}
	for _, sp := range hub.Registry().List() {
		got[sp.ID] = sp.Env + "/" + sp.Mode + "/" + sp.Name
	}
	if got["stg"] != "staging/replay/stg-tidb-01" || got["dev"] != "test/replay/dev-tidb-01" || len(got) != 3 {
		t.Errorf("演示集群: %v", got)
	}
	if hub.Instance("stg") == nil || hub.Instance("dev") == nil {
		t.Error("演示集群的实例应已启动")
	}
	// 删掉一个再重启：已有 clusters.json，不再自动登记
	if _, _, err := hub.Remove("dev"); err != nil {
		t.Fatal(err)
	}
	closeFn()
	hub, closeFn = startForTest(t, dir, fx, true, "replay")
	defer closeFn()
	ids := map[string]bool{}
	for _, sp := range hub.Registry().List() {
		ids[sp.ID] = true
	}
	if !ids["stg"] || ids["dev"] || len(ids) != 2 {
		t.Errorf("重启后不应重新登记已删除的演示集群: %v", ids)
	}
}

// 回放模式之外（live 主集群）即使带 --dev-seed 也不自动登记。
func TestDevSeedNeverRegistersInLiveMode(t *testing.T) {
	fx := fixturesDir(t)
	dir := t.TempDir()
	s := replayServer(t, fx)
	sh := &opsShared{}
	oa := opsArgs{dir: dir, env: "prod", mode: "replay", simDelay: time.Millisecond, cluster: "prod-tidb-01", readonly: filepath.Join(dir, "READONLY")}
	if err := enableOps(s, oa, sh); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer func() { _ = sh.audit.Close() }()
	a := devSeedArgs(dir, fx, true, "live")
	hub := startHub(s, sh, a)
	defer hub.Close()
	if n := len(hub.Registry().List()); n != 1 {
		t.Errorf("live 模式不应自动登记演示集群: %d", n)
	}
}

// live 集群：地址取自集群定义（不是启动参数），采集器按该集群名工作；开启自动探测时缺失的地址由 tiup display 补全（这里用 --no-auto 关闭）。
func TestBuildLiveBackendUsesClusterSpec(t *testing.T) {
	fc := fakecluster.StartProfile(fakecluster.StagingProfile())
	defer fc.Close()
	ep := fc.Endpoints()
	b := &backendBuilder{a: hubArgs{noAuto: true, tiup: "tiup", myBin: "mysql", timeout: 5 * time.Second, cacheTTL: time.Second, snapTTL: time.Second, home: "/home/tidb"},
		sh: &opsShared{pol: xexec.Policy{}, key: []byte("0123456789abcdef0123456789abcdef"), em: "none"}}
	be, err := b.build(clusters.Spec{ID: "stg", Name: "stg-tidb-01", Env: "staging", Mode: clusters.ModeLive, PD: ep["pd"], TiDBStatus: ep["tidb"], Prom: ep["prom"], Alert: ep["alert"]})
	if err != nil {
		t.Fatal(err)
	}
	if be.Mode != "live" || be.Col.Params.Cluster != "stg-tidb-01" {
		t.Errorf("backend: mode=%s cluster=%s", be.Mode, be.Col.Params.Cluster)
	}
	snap := be.Col.Snapshot(context.Background(), "live")
	if snap.Cluster != "stg-tidb-01" {
		t.Errorf("快照的集群名: %q", snap.Cluster)
	}
	stores := 0
	for _, in := range snap.Topology.Instances {
		if in.Type == "tikv" {
			stores++
		}
	}
	if stores != 3 {
		t.Errorf("应从该集群自己的 PD 读到 3 个 TiKV（stg 预设），得到 %d；错误: %v", stores, snap.Errors)
	}
	// 回放集群缺目录：构造失败（不 panic）
	if _, err := b.build(clusters.Spec{ID: "x", Name: "x", Mode: clusters.ModeReplay, Fixtures: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("数据包不存在应构造失败")
	}
}
