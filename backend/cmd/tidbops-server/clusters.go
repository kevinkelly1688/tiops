// @author: kevinkelly
// @license: Apache License 2.0
// @file: clusters.go
// @time: 2026/10/28

package main

// 多集群：由启动参数构建的集群是「主集群」；其余集群来自 <data-dir>/clusters.json，经 server.Hub 热管理。
// 本文件负责把集群定义（clusters.Spec）转成后端（采集器 + 执行器），以及启动逻辑。

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tidbops/internal/audit"
	"tidbops/internal/change"
	"tidbops/internal/clusters"
	"tidbops/internal/collect"
	xexec "tidbops/internal/exec"
	"tidbops/internal/server"
	"tidbops/internal/source"
)

type hubArgs struct {
	dir, env, mode, fixtures, fixturesRoot, cluster string
	sudoUser, execBin                               string
	devSeed                                         bool
	pd, tidb, prom, alert                           string
	myHost, myUser, myBin, tiup, home               string
	myPort                                          int
	timeout, cacheTTL, snapTTL, simDelay            time.Duration
	noAuto                                          bool
	standalone                                      bool // 独立部署：主实例是平台本身，手工接入的集群都是远程集群（只读）
	localTiup                                       bool // 独立部署 + 本机是 tiup 中控机：一键导入的集群（Spec.Local）经本机执行器做全部运维
}

// isLocal 报告该集群是否走本机 tiup（一键导入、且本进程声明了本机是中控机）。非独立部署（旧的同机部署）的集群一律走本机 tiup。
func (a hubArgs) isLocal(spec clusters.Spec) bool {
	if !a.standalone {
		return true
	}
	return a.localTiup && spec.Local
}

// backendBuilder 把集群定义变成后端。进程级的参数（执行器模式 / 策略 / 密钥 / 缓存时间……）来自启动参数，
// 集群级的（地址、账号、密码、tiup 路径、集群名）来自 clusters.Spec。
type backendBuilder struct {
	a  hubArgs
	sh *opsShared
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func (b *backendBuilder) build(spec clusters.Spec) (server.Backend, error) {
	a := b.a
	params := source.CatalogParams{Cluster: spec.Name, Home: a.home, Now: time.Now(), Remote: !a.isLocal(spec)}
	var src source.Source
	var alertAddr, sqlHost string
	var sqlPort int
	switch spec.Mode {
	case clusters.ModeReplay:
		rp, err := source.NewReplay(spec.Fixtures)
		if err != nil {
			return server.Backend{}, err
		}
		src = rp
	default:
		if spec.Home != "" {
			params.Home = spec.Home
		}
		cfg := source.LiveConfig{Endpoints: map[string]string{}, TiupBin: firstNonEmpty(spec.Tiup, a.tiup), Timeout: a.timeout,
			MySQL: source.MySQLConfig{Bin: firstNonEmpty(spec.MySQLBin, a.myBin), Host: spec.MySQLHost, Port: spec.MySQLPort, User: spec.MySQLUser, Password: spec.MySQLPassword}}
		for k, v := range map[string]string{"pd": spec.PD, "tidb": spec.TiDBStatus, "prom": spec.Prom, "alert": spec.Alert} {
			if v != "" {
				cfg.Endpoints[k] = v
			}
		}
		if a.standalone && a.localTiup && spec.Local && b.sh.em == "sudo" {
			// 中控机模式下导入的集群：Web 进程（tidbops）直接执行不了 tiup（不在 PATH，也进不去 tidb 的家目录），
			// 采集用的 tiup cluster display / list 改走受限执行器。
			bin := a.execBin
			if bin == "" {
				bin, _ = os.Executable()
			}
			cfg.Exec = localTiupExec(xexec.SudoRunner{User: a.sudoUser, Bin: bin}, spec.Name, source.DefaultExec(32<<20))
		}
		if !a.noAuto && !a.standalone { // 独立部署：本机没有 tiup，地址必须在集群接入里填写
			_, notes, err := source.Detect(context.Background(), &cfg, spec.Name)
			if err != nil {
				return server.Backend{}, err
			}
			for _, n := range notes {
				log.Printf("[集群 %s] %s", spec.ID, n)
			}
		}
		alertAddr, sqlHost, sqlPort = cfg.Endpoints["alert"], cfg.MySQL.Host, cfg.MySQL.Port
		src = source.NewLive(cfg)
	}
	src = source.NewCached(src, a.cacheTTL)
	be := server.Backend{Col: collect.New(src, params), Mode: spec.Mode, Fixtures: spec.Fixtures, SnapTTL: a.snapTTL}
	be.Ops = func(s *server.Server, cfg *server.OpsConfig) error {
		return b.configureOps(s, cfg, spec, alertAddr, sqlHost, sqlPort)
	}
	be.Preflight = func(ctx context.Context, s *server.Server) []string { return server.RunnerPreflight(ctx, s, spec.Name) }
	return be, nil
}

// configureOps 按集群设置执行器。规则：
//   - 回放集群恒用模拟执行器（绝不对一个只有回放数据的「集群」调用真实 tiup）；
//   - 直连集群沿用进程的 --exec；进程是 sim（主集群是回放）时直连集群不配执行器（none），避免假装执行；
//   - 策略 Clusters 只含该集群的 tiup 名；策略文件里若显式列了集群白名单，则取交集（不在其中 = 执行器会拒绝，并在预检里警告）；
//   - 集群相关的策略项（SQL 地址、Alertmanager）取自该集群，不继承主集群的。
func (b *backendBuilder) configureOps(s *server.Server, cfg *server.OpsConfig, spec clusters.Spec, alertAddr, sqlHost string, sqlPort int) error {
	sh := b.sh
	em := sh.em
	switch {
	case spec.Mode == clusters.ModeReplay:
		em = "sim"
	case em == "sim":
		em = "none"
	case !b.a.isLocal(spec): // 手工接入的远程集群：平台碰不到它的 tiup / SSH，恒为只读（不配执行器）
		cfg.ReadOnlyWhy = "该集群是手工接入的远程集群：平台只读，不能执行变更（要运维请在中控机上「一键导入」）"
		em = "none"
	}
	pol := sh.pol
	pol.Clusters = []string{spec.Name}
	if len(sh.polClusters) > 0 {
		pol.Clusters = nil
		for _, c := range sh.polClusters {
			if c == spec.Name {
				pol.Clusters = []string{spec.Name}
			}
		}
	}
	if spec.Tiup != "" {
		pol.TiupBin = spec.Tiup
	}
	if spec.MySQLBin != "" {
		pol.MySQLBin = spec.MySQLBin
	}
	pol.MySQLHost, pol.MySQLPort = sqlHost, sqlPort
	pol.AlertURL = alertAddr
	cfg.ExecMode, cfg.BackupRoots = em, pol.BackupRoots
	switch em {
	case "sim":
		if pol.MySQLUser == "" {
			pol.MySQLUser = "tidbops_ops"
		}
		if len(pol.BackupRoots) == 0 {
			pol.BackupRoots = []string{"local:///data/backup/"}
		}
		if pol.AlertURL == "" {
			pol.AlertURL = "http://127.0.0.1:9093"
		}
		cfg.BackupRoots = pol.BackupRoots
		cfg.Sim, cfg.WaitScale = true, 0.02
		cfg.Runner = &xexec.SimRunner{Pol: pol, Topo: s.ExecTopo(), Key: sh.key, Delay: b.a.simDelay}
		cfg.Prober = &change.SimProber{Polls: 2}
	case "direct":
		tiup := pol.TiupBin
		run := func(ctx context.Context, argv []string) ([]byte, error) {
			if len(argv) > 0 && argv[0] == "tiup" && tiup != "" {
				argv = append([]string{tiup}, argv[1:]...)
			}
			return osRun(ctx, argv)
		}
		cfg.Runner = xexec.DirectRunner{Pol: pol, Topo: xexec.DisplayTopo(run), Key: sh.key}
		cfg.Prober = s.NewPDProber(cfg.Runner)
	case "sudo":
		// 特权端读的是全局的 --exec-policy 文件：它必须允许这个集群名（预检会提示）。
		bin := b.a.execBin
		if bin == "" {
			bin, _ = os.Executable()
		}
		cfg.Runner = xexec.SudoRunner{User: b.a.sudoUser, Bin: bin}
		cfg.Prober = s.NewPDProber(cfg.Runner)
	}
	return nil
}

// startHub 创建 Hub：读取 clusters.json 并构造其余集群的实例；--dev-seed 的回放演示下按需自动登记演示集群。
func startHub(s *server.Server, sh *opsShared, a hubArgs) *server.Hub {
	roots := splitCSV(a.fixturesRoot)
	if len(roots) == 0 && a.fixtures != "" {
		if abs, err := filepath.Abs(a.fixtures); err == nil {
			roots = []string{filepath.Dir(abs)}
		}
	}
	primary := clusters.Spec{ID: clusters.Slug(a.cluster), Name: a.cluster, Display: a.cluster, Env: a.env, Mode: a.mode,
		PD: a.pd, TiDBStatus: a.tidb, Prom: a.prom, Alert: a.alert, MySQLHost: a.myHost, MySQLPort: a.myPort, MySQLUser: a.myUser, MySQLBin: a.myBin, Tiup: a.tiup, Home: a.home,
		MySQLPassword: os.Getenv("TIDBOPS_MYSQL_PWD")}
	if a.standalone { // 平台实例：不代表任何集群，不入集群列表
		primary = clusters.Spec{ID: clusters.PlatformID, Name: clusters.PlatformName, Display: "平台", Env: a.env, Mode: clusters.ModeLive, Platform: true}
	}
	if a.fixtures != "" {
		if abs, err := filepath.Abs(a.fixtures); err == nil {
			primary.Fixtures = abs
		}
	}
	seedDemo := false
	if a.devSeed && a.mode == "replay" {
		if _, err := os.Stat(filepath.Join(a.dir, "clusters.json")); os.IsNotExist(err) {
			seedDemo = true
		}
	}
	bb := &backendBuilder{a: a, sh: sh}
	hub, err := server.NewHub(s, server.HubConfig{Dir: a.dir, Primary: primary, FixturesRoots: roots, Factory: bb.build,
		Prober: clusters.Prober{DefaultTiup: a.tiup, DefaultMySQLBin: a.myBin, NoTiup: a.standalone}, Warm: true, LocalTiup: a.standalone && a.localTiup})
	if err != nil {
		log.Fatalf("启用多集群失败: %v", err)
	}
	if seedDemo && a.fixtures != "" {
		seedDemoClusters(hub, sh.audit, filepath.Dir(primary.Fixtures))
	}
	return hub
}

// seedDemoClusters 在 --dev-seed 的回放模式下登记演示用的两个回放集群（fixtures/stg、fixtures/dev），仅当数据目录里还没有
// clusters.json 时执行一次。生产模式（非 --dev-seed / 非回放）绝不会走到这里。
func seedDemoClusters(hub *server.Hub, al *audit.Log, fixturesParent string) {
	demos := []struct{ id, name, display, desc, env string }{
		{"stg", "stg-tidb-01", "预发集群", "演示：预发环境（回放数据 fixtures/stg）", "staging"},
		{"dev", "dev-tidb-01", "开发集群", "演示：开发测试环境（回放数据 fixtures/dev）", "test"},
	}
	for _, d := range demos {
		dir := filepath.Join(fixturesParent, d.id)
		if st, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil || st.IsDir() {
			continue
		}
		in := clusters.Input{ID: d.id, Name: &d.name, Display: &d.display, Desc: &d.desc, Env: &d.env, Mode: strp(clusters.ModeReplay), Fixtures: &dir}
		sp, _, err := hub.Add(in, "system")
		if err != nil {
			log.Printf("自动登记演示集群 %s 失败: %v", d.id, err)
			continue
		}
		log.Printf("已自动登记演示集群 %s（%s，%s）", sp.ID, sp.Env, sp.Fixtures)
		if al != nil {
			_, _ = al.Append(audit.Entry{User: "system", Env: sp.Env, Cluster: sp.Name, Action: "cluster.add", Target: sp.ID, Risk: "mid",
				Summary: "--dev-seed 自动登记演示集群（回放）：" + strings.TrimSpace(sp.Display), Result: "success"})
		}
	}
}

func strp(s string) *string { return &s }
