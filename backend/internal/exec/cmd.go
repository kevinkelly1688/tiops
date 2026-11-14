// @author: kevinkelly
// @license: Apache License 2.0
// @file: cmd.go
// @time: 2026/10/28

// Package exec 是平台唯一允许对集群「做事」的出口（A3 §3）：命令白名单、结构化参数、审批令牌、特权分离。
//
// 约束：
//   - 不经过 shell；argv 由本包按 Kind 固定模板生成，参数逐项校验；
//   - 未登记的 Kind / 参数一律拒绝；永远禁止 destroy / clean / patch / 任意 rm / ssh / bash -c；
//   - 写类命令必须带有效的审批令牌（HMAC，绑定命令摘要与过期时间）；
//   - 同一时刻同一集群只执行一条写命令（由调用方的执行引擎保证，本包提供 Lock 辅助）。
package exec

import (
	"context"
	"encoding/json"
	"time"
)

// 命令种类。Kind 与 argv 模板一一对应，见 allowlist.go。
const (
	// 读（无需令牌）
	KindClusterList    = "cluster.list"         // tiup cluster list：本机 tiup 管理的全部集群（不带集群名；一键导入用）
	KindTiupVersion    = "tiup.version"         // tiup --version（只读；不带任何字段；采集「环境信息」里的 tiup 版本）
	KindTiupClusterVer = "tiup.cluster-version" // tiup cluster --version（只读；不带任何字段）
	KindClusterDisplay = "cluster.display"      // tiup cluster display <c>
	KindClusterCheck   = "cluster.check"        // tiup cluster check <c> --cluster；可选 Content（扩容拓扑 YAML）时为 tiup cluster check <c> <file> --cluster（扩容预检，仍是读类）
	KindPDQuery        = "pd.query"             // pd-ctl 只读查询：Key ∈ {store, member, scheduler-show, region-down-peer, region-pending-peer, config-show}
	KindCDCCaptures    = "cdc.captures"         // tiup cdc cli capture list

	// 写（必须带令牌）
	KindClusterRestart   = "cluster.restart"     // tiup cluster restart <c> -N <nodes> -y，或 -R <role> -y（Nodes 与 Role 二选一）
	KindClusterStop      = "cluster.stop"        // tiup cluster stop <c> -N <nodes> -y
	KindClusterStart     = "cluster.start"       // tiup cluster start <c> -N <nodes>
	KindClusterScaleOut  = "cluster.scale-out"   // tiup cluster scale-out <c> <file> -y；Content 为平台生成的拓扑 YAML
	KindClusterScaleIn   = "cluster.scale-in"    // tiup cluster scale-in <c> -N <nodes> -y
	KindClusterPrune     = "cluster.prune"       // tiup cluster prune <c> -y
	KindClusterEditCfg   = "cluster.edit-config" // tiup cluster edit-config <c> --topology-file <file>；Content 为完整拓扑 YAML
	KindClusterReload    = "cluster.reload"      // tiup cluster reload <c> -N <nodes> | -R <role> -y
	KindPDConfigSet      = "pd.config-set"       // pd-ctl config set <Key> <Value>；Key ∈ PD 参数白名单
	KindPDEvictAdd       = "pd.evict-add"        // pd-ctl scheduler add evict-leader-scheduler <Store>
	KindPDEvictRemove    = "pd.evict-remove"     // pd-ctl scheduler remove evict-leader-scheduler-<Store>
	KindPDStoreLimit     = "pd.store-limit"      // pd-ctl store limit all <Rate>
	KindPDLeaderTransfer = "pd.leader-transfer"  // pd-ctl member leader transfer <Name>
	KindSQLSetConfig     = "sql.set-config"      // SET CONFIG <Role|"Instance"> `<Key>` = <Value>（运维账号）
	KindSQLSetGlobal     = "sql.set-global"      // SET GLOBAL <Key> = <Value>（运维账号）
	KindBRBackupFull     = "br.backup-full"      // tiup br backup full --storage <Value>
	KindBRRestoreFull    = "br.restore-full"     // tiup br restore full --storage <Value>
	KindBRLogStart       = "br.log-start"        // tiup br log start --task-name <Name> --storage <Value>
	KindBRLogStop        = "br.log-stop"         // tiup br log stop --task-name <Name>
	KindBRLogStatus      = "br.log-status"       // tiup br log status（读）
	KindClusterUpgrade   = "cluster.upgrade"     // tiup cluster upgrade <c> <Value=目标版本，如 v8.5.3> -y
	KindAlertSilence     = "alert.silence"       // Alertmanager 静默：Key=告警名，Instance 可选，Rate=分钟数(1..1440)，Value=备注；POST <Policy.AlertURL>/api/v2/silences
)

// Cmd 是一条结构化命令。字段含义随 Kind 而定，未使用的字段必须为空（校验时会拒绝多余字段）。
type Cmd struct {
	Kind     string   `json:"kind"`
	Cluster  string   `json:"cluster,omitempty"`
	Nodes    []string `json:"nodes,omitempty"`    // host:port，必须在集群拓扑内
	Role     string   `json:"role,omitempty"`     // tidb | tikv | pd | tiflash | cdc
	Store    int64    `json:"store,omitempty"`    // PD store id
	Name     string   `json:"name,omitempty"`     // PD member 名 / BR 任务名
	Key      string   `json:"key,omitempty"`      // 配置项 / 系统变量 / pd.query 的查询名
	Value    string   `json:"value,omitempty"`    // 配置值 / 存储路径
	Instance string   `json:"instance,omitempty"` // SET CONFIG 的实例状态地址 host:statusPort
	Rate     int      `json:"rate,omitempty"`     // store limit
	Content  string   `json:"content,omitempty"`  // 拓扑 YAML（scale-out / edit-config 必填；cluster.check 可选，用于扩容预检）
}

// Request 是提交给执行器的请求。
type Request struct {
	Cmd      Cmd    `json:"cmd"`
	ChangeID string `json:"changeId,omitempty"`
	Actor    string `json:"actor,omitempty"` // 申请人，仅用于审计
	Token    string `json:"token,omitempty"` // 写命令必填
}

// Result 是一次执行的结果。
type Result struct {
	Exit   int    `json:"exit"`
	Output string `json:"output,omitempty"`
	Err    string `json:"err,omitempty"`
	Millis int64  `json:"millis"`
}

// Runner 执行一条已通过校验的请求。emit 实时接收输出行（可为 nil）。
type Runner interface {
	Run(ctx context.Context, req Request, emit func(line string)) (Result, error)
}

// Topo 是执行器校验参数所需的集群信息。
type Topo struct {
	Version string            // 如 v8.5.2，用于 tiup ctl:<ver>
	PD      string            // 形如 http://10.0.1.21:2379
	Nodes   map[string]string // host:port → 角色(tidb|tikv|pd|tiflash|ticdc)
	Status  map[string]string // 状态地址 host:statusPort → host:port（SET CONFIG 的 Instance 校验）

	// 以下为向后兼容的可选扩展字段。
	User     string // tiup 部署用户（display 的 Deploy user）。edit-config / scale-out 校验 global.user 时必需
	SSHPort  int    // 集群的 ssh_port；>0 时 edit-config / scale-out 要求 global.ssh_port 与之相同
	SQLHost  string // 运维账号连接的 TiDB 地址；Policy.MySQLHost 为空时使用
	SQLPort  int    // 同上
	EditBase string // 当前拓扑 YAML；非空时 edit-config 额外要求「变更项全部落在配置目录内」
}

// AnyCluster 写在 Policy.Clusters 里表示「本机 tiup 管理的任何集群」（中控机模式：集群由管理员在页面一键导入，
// 不需要每次改策略文件）。它只放宽「集群名白名单」这一层；节点 / 角色 / 参数仍按各自的规则校验，写命令仍要审批令牌。
const AnyCluster = "*"

// Policy 是执行器的静态策略（由 /etc/tidb-ops 下的配置或启动参数提供，不接受请求里的任何覆盖）。
type Policy struct {
	Clusters    []string                 `json:"clusters"`
	TiupBin     string                   `json:"tiupBin,omitempty"`
	MySQLBin    string                   `json:"mysqlBin,omitempty"`
	MySQLHost   string                   `json:"mysqlHost,omitempty"`
	MySQLPort   int                      `json:"mysqlPort,omitempty"`
	MySQLUser   string                   `json:"mysqlUser,omitempty"`   // 运维账号（仅能 SET GLOBAL / SET CONFIG）
	MySQLPwdEnv string                   `json:"mysqlPwdEnv,omitempty"` // 存放运维账号密码的环境变量名
	BackupRoots []string                 `json:"backupRoots,omitempty"` // 备份存储前缀白名单，如 local:///data/backup/、s3://bucket/tidb/
	TempDir     string                   `json:"tempDir,omitempty"`     // 存放拓扑临时文件的私有目录
	Timeouts    map[string]time.Duration `json:"-"`                     // 覆盖默认超时（测试用）
	MinTiKV     int                      `json:"minTiKV,omitempty"`     // scale-in 后至少保留的 TiKV 数（0 = 默认 3；小型测试环境可显式调低）
	AlertURL    string                   `json:"alertURL,omitempty"`    // Alertmanager 地址，如 http://10.0.1.31:9093（alert.silence 使用）

	// PerCluster 是多集群部署的按集群连接覆盖（键 = 集群名）：命令的 Cluster 在其中时，SQL 地址与 Alertmanager 取该项的值，
	// 而不是上面的全局值。没有该项的集群沿用全局值（单集群部署 / 旧策略文件行为不变）。
	// 多个集群共用一份策略文件（sudo 模式）时，必须为每个集群配置，且全局 mysqlHost / alertURL 留空，
	// 否则其他集群的 SET CONFIG / SET GLOBAL / 告警静默会指向全局值所属的那个集群。
	PerCluster map[string]ClusterConn `json:"perCluster,omitempty"`
}

// ClusterConn 是一个集群自己的连接信息（覆盖 Policy 的全局值；空字段表示「没有」：SQL 地址为空时回退到拓扑里的 TiDB）。
type ClusterConn struct {
	MySQLHost string `json:"mysqlHost,omitempty"`
	MySQLPort int    `json:"mysqlPort,omitempty"`
	AlertURL  string `json:"alertURL,omitempty"`
}

// forCluster 返回应用了该集群覆盖项的策略副本。
func (p Policy) forCluster(name string) Policy {
	if pc, ok := p.PerCluster[name]; ok {
		p.MySQLHost, p.MySQLPort, p.AlertURL = pc.MySQLHost, pc.MySQLPort, pc.AlertURL
	}
	return p
}

// TopoFunc 取得集群的最新拓扑信息。执行器每次校验都会调用（实现里应有短缓存）。
type TopoFunc func(ctx context.Context, cluster string) (Topo, error)

// MarshalCanonical 返回命令的规范化 JSON（字段顺序固定），用于令牌摘要与审计。
func MarshalCanonical(c Cmd) []byte {
	b, _ := json.Marshal(c) // 结构体字段顺序固定
	return b
}
