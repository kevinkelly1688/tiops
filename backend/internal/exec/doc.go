// @author: kevinkelly
// @license: Apache License 2.0
// @file: doc.go
// @time: 2026/10/28

// 本文件只放包文档（给集成者看的对外 API 摘要），没有代码。

/*
对外 API 摘要（集成者必读）

# 定位与安全模型

exec 是平台对集群「做事」的唯一出口。平台与 tiup 同机，Web 进程被攻破等同集群被攻破，
所以本包的目标是：即使攻击者完全控制了 Web 进程，也只能执行「白名单内、参数合法、带有效审批令牌」的命令。
  - 不经 shell：argv 由固定模板生成，参数逐项校验；任何不使用的字段必须为空；
  - 所有字符串字段拒绝空白 / 控制字符 / shell 元字符 / 引号 / 反斜杠 / 非 ASCII；
  - 集群名 ∈ Policy.Clusters；节点 / 实例 / 角色必须来自拓扑（TopoFunc）或枚举；
    pd-ctl 的版本与 PD 地址只来自拓扑，从不来自请求；
  - 配置值先经 Normalize（输出仅含 [A-Za-z0-9._-]，注入安全）再进入 SQL / pd-ctl 参数；
  - 写命令必须带审批令牌（HMAC-SHA256，绑定「命令 + changeID + 过期时间」），读命令无需令牌；
  - 运维账号密码只经子进程环境变量 MYSQL_PWD 传递，不进 argv / 日志 / 输出（输出里出现会被替换为 ***）；
  - 子进程环境最小化：LANG、PATH、HOME、TIUP_HOME（Windows 另有系统必需变量），其余一概不继承；
  - 没有任何 Kind 能生成 destroy / clean / patch / rm / ssh / bash -c；
  - 唯一的例外是自动修复（repair.go）：命令因「tiup 组件文件缺失」没能跑起来时，对该命令自己用到的 ctl / br / cdc 组件版本
    执行一次 tiup uninstall + tiup install 后重试，版本取自 argv、格式严格校验，同一组件 10 分钟内只试一次。

# 部署方式

推荐特权分离：Web 进程里用 SudoRunner；exec 子进程（`tidbops-server exec --stdin`，以 tidb 用户运行）里用
Serve(ctx, os.Stdin, os.Stdout, DirectRunner{...})。演示 / 测试用 SimRunner（校验与令牌照常，不执行进程）。

	// Web 进程
	var r exec.Runner = exec.SudoRunner{User: "tidb", Bin: "/opt/tidb-ops/tidbops-server"}

	// exec 子进程
	topo := exec.DisplayTopo(runTiupDisplay)               // runTiupDisplay 由调用方注入，只能执行 tiup cluster display
	runner := exec.DirectRunner{Pol: pol, Topo: topo, Key: key, Log: audit}
	err := exec.Serve(ctx, os.Stdin, os.Stdout, runner)

# 导出符号

类型（cmd.go）：Cmd、Request、Result、Runner、Topo、Policy、TopoFunc、MarshalCanonical；Kind* 常量。
Topo 的可选扩展字段：User、SSHPort、SQLHost、SQLPort、EditBase。
Policy 的扩展字段：MinTiKV、AlertURL、PerCluster（多集群共用一份策略文件时按集群覆盖 SQL 地址 / Alertmanager，见 ClusterConn）。

配置目录（configcat.go）：

	type ConfigItem struct{ Role, Key, Label string; Type ConfigType; Min, Max int64; Opts []string; Default string; Online bool; Scope string; Sys, Danger bool; Desc string }
	func Lookup(role, key string) (ConfigItem, bool)        // role 接受 cdc 作为 ticdc 的别名；返回副本
	func Catalog() map[string][]ConfigItem
	func Normalize(item ConfigItem, raw string) (string, error)  // 幂等；输出注入安全
	func PDShortKey(key string) string                      // schedule.max-store-down-time → max-store-down-time
	func SizeBytes(norm string) (int64, error); func DurationSeconds(norm string) (int64, error)

白名单与 argv（allowlist.go / argv.go）：

	type KindInfo struct{ Known, Write bool; Timeout time.Duration; Risk string }
	func Info(kind string) KindInfo                         // 未知 Kind：Known=false、Write=true
	func Kinds() []string
	func Validate(ctx, pol Policy, topo TopoFunc, c Cmd) error
	func Argv(ctx, pol Policy, topo TopoFunc, c Cmd) (Plan, error)   // 会写 0600 临时文件，调用方须 Plan.Cleanup()
	type Plan struct{ Argv, Env, TempFiles []string; HTTP *HTTPCall }
	func (Plan) Cleanup(); func (Plan) String() string      // String 不含环境变量（无凭据）

拓扑 YAML（yamlcheck.go）：

	func ValidateScaleOut(content string, t Topo) error
	func ValidateEditConfig(content, wantUser string) error
	func ValidateEditConfigFull(content, wantUser string, wantSSHPort int, base string) error

令牌（token.go）：

	func Sign(key []byte, c Cmd, changeID string, exp time.Time) string     // 密钥 < 16 字节或 changeID 不合法返回 ""
	func Verify(key []byte, c Cmd, changeID, token string, now time.Time) error
	令牌格式 v1.<expUnix>.<base64url(mac)>；changeID 须匹配 ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$。

执行器（runner.go / sim.go / sudo.go / serve.go / silence.go）：

	type DirectRunner struct{ Pol Policy; Topo TopoFunc; Key []byte; Log func(string); Now func() time.Time; Consume func(changeID, token string) error }
	type SimRunner struct{ Pol; Topo; Key; Log; Now; Delay time.Duration; FailKind string; FailAfter int }; func (*SimRunner) Calls() []Request
	type SudoRunner struct{ Sudo, User, Bin string; Args []string; Timeout time.Duration }
	func Serve(ctx, in io.Reader, out io.Writer, runner Runner) error
	const MaxOutput = 256 << 10

	Run 的语义：err != nil 表示「没有跑完」（校验 / 令牌失败、无法启动、超时、取消），此时 Result.Exit == -1、Result.Err 为文案；
	命令已执行但以非 0 退出：err == nil，Result.Exit 为退出码（>0），Result.Err 形如「退出码 3」。
	输出按行回调 emit，累计超过 256KB 截断（Result.Output 与 emit 均带截断提示）。
	Consume 可选：令牌验证通过后、执行前调用，用于实现一次性令牌（返回错误则拒绝）；本包不内置重放防护，令牌在有效期内可重复使用。

拓扑（topo.go）：

	type RunFunc func(ctx, argv []string) ([]byte, error)   // argv = ["tiup","cluster","display",<name>]
	func DisplayTopo(run RunFunc, opts ...TopoOption) TopoFunc  // 10 秒缓存；WithTopoTTL / WithTopoClock
	func ParseTopo(display string) (Topo, error)
	func StaticTopo(t Topo) TopoFunc; func StaticTopos(m map[string]Topo) TopoFunc

行协议（Serve ↔ SudoRunner）：stdin 一个 JSON Request；stdout 若干 {"t":"out","line":"..."}，最后一行
{"t":"result","exit":0,"output":"...","err":"","millis":12}。校验 / 令牌 / 请求格式错误同样以 result 输出
（exit=-1），Serve 返回 nil；请求体启用 DisallowUnknownFields，限 1MB。

# 各 Kind 的字段约定

「必须」之外的字段一律必须为空。写 = 需令牌。

	cluster.list         读  （不带任何字段）                            tiup cluster list                                 30s
	tiup.version         读  （不带任何字段）                            tiup --version                                    15s
	tiup.cluster-version 读  （不带任何字段）                            tiup cluster --version                            15s
	cluster.display      读  Cluster                                    tiup cluster display <c>                         30s
	cluster.check        读  Cluster [Content]                          tiup cluster check <c> [<file>] --cluster       120s（Content 按 scale-out 规则校验）
	pd.query             读  Cluster Key∈{store,member,scheduler-show,region-down-peer,region-pending-peer,config-show}   30s
	cdc.captures         读  Cluster                                    tiup cdc:<ver> cli capture list --server http://<首个 ticdc>   30s
	br.log-status        读  Cluster                                    tiup br:<ver> log status --pd <pd>              60s
	cluster.restart      写  Cluster + (Nodes 或 Role 二选一)           tiup cluster restart <c> -N a,b | -R <role> -y  600s
	cluster.stop         写  Cluster Nodes                              ... stop <c> -N a,b -y                          600s
	cluster.start        写  Cluster Nodes                              ... start <c> -N a,b（无 -y）                    600s
	cluster.scale-out    写  Cluster Content                            ... scale-out <c> <file> -y                     3600s
	cluster.scale-in     写  Cluster Nodes                              ... scale-in <c> -N a,b -y（剩余 TiKV < Policy.MinTiKV(默认 3)、PD / TiDB 被清空则拒绝）  3600s
	cluster.prune        写  Cluster                                    ... prune <c> -y                                300s
	cluster.edit-config  写  Cluster Content                            ... edit-config <c> --topology-file <file> -y   120s
	cluster.reload       写  Cluster + (Nodes 或 Role 二选一)           ... reload <c> -N a,b | -R <role> -y            1800s
	cluster.upgrade      写  Cluster Value(vX.Y.Z，不得低于当前版本)    ... upgrade <c> <ver> -y                        3h
	pd.config-set        写  Cluster Key Value                          tiup ctl:<ver> pd -u <pd> config set <短键> <值>  30s（Key 须是 PD 在线的集群级参数）
	pd.evict-add         写  Cluster Store(>0)                          ... scheduler add evict-leader-scheduler <id>   30s
	pd.evict-remove      写  Cluster Store(>0)                          ... scheduler remove evict-leader-scheduler-<id>  30s
	pd.store-limit       写  Cluster Rate(1..200)                       ... store limit all <rate>                      30s
	pd.leader-transfer   写  Cluster Name(PD 成员名)                    ... member leader transfer <name>               30s
	sql.set-config       写  Cluster Key Value + (Role 或 Instance 二选一)  mysql --batch -h -P -u -e "SET CONFIG tikv `k` = 'v'" / SET CONFIG "host:statusPort" `k` = v   10s
	sql.set-global       写  Cluster Key Value                          mysql ... -e "SET GLOBAL <var> = <v>"（size 换算成字节）  10s
	br.backup-full       写  Cluster Value(备份存储)                    tiup br:<ver> backup full --pd <pd> --storage <v>  12h
	br.restore-full      写  Cluster Value                              ... restore full ...                            12h
	br.log-start         写  Cluster Name Value                         ... log start --pd <pd> --task-name <n> --storage <v>  60s
	br.log-stop          写  Cluster Name                               ... log stop --pd <pd> --task-name <n>          60s
	alert.silence        写  Cluster Key(告警名) Rate(1..1440 分钟) [Instance] [Value 备注≤200 字]   POST <Policy.AlertURL>/api/v2/silences   30s

字段取值：Nodes 为 host:port（∈ Topo.Nodes）；Role ∈ tidb|tikv|pd|tiflash|cdc（ticdc 等价）；Instance(set-config) ∈ Topo.Status，
Instance(alert.silence) ∈ Topo.Nodes 或 Topo.Status；Key/Value 对配置类按 configcat 校验；备份 Value 必须以某个 Policy.BackupRoots
为前缀、字符集 [A-Za-z0-9_./:@%+=,-]、不含 ..（凭据不可出现在 URL 里）。

scale-out / cluster.check 的 Content：只允许 global(user/ssh_port/deploy_dir/data_dir) 与 tidb/tikv/pd/tiflash/cdc_servers；host 为单播 IPv4；
端口为 1–65535 整数；目录在 /tidb-deploy 或 /tidb-data 之下；config 只允许 server.labels.* 与配置目录里登记的非系统变量；
不得与现有 host:port（含状态端口）冲突。
edit-config 的 Content：顶层键 ∈ {global,monitored,server_configs,pd/tidb/tikv/tiflash/cdc_servers,monitoring/grafana/alertmanager/tiproxy_servers}；
global.user 必须出现且等于 Topo.User，global.ssh_port 若 Topo.SSHPort>0 须相同；拒绝 rule_dir / dashboard_dir / config_file /
additional_scrape_conf 等会让 tiup 读取本机路径的键；拒绝锚点 / 别名 / 自定义标签 / 重复键 / 控制字符 / 非 ASCII。
提供 Topo.EditBase（当前拓扑）后，还强制「相对 EditBase 的变更项全部落在配置目录内」，host / 端口 / 目录 / 增删实例一律拒绝。

# 已知限制与需要在真实 tiup 上实测的点

 1. edit-config --topology-file -y：不同 tiup 版本是否支持 --topology-file、是否还会询问确认，需实测；
    Content 必须是 tiup 能接受的「拓扑」YAML（global/server_configs/*_servers...），而不是 meta.yaml 整体（那里多了 user/tidb_version/topology 层）。
    edit-config 的 global / 实例键白名单是保守设计，真实集群的拓扑若含白名单之外的键会被拒绝，需要对照实际 meta.yaml 补全。
 2. pd-ctl 子命令写法：`tiup ctl:<ver> pd -u <pd> ...` 的 store / member / scheduler show / region check down-peer|pending-peer /
    config show / config set <短键> <值> / scheduler add|remove evict-leader-scheduler / store limit all <rate> / member leader transfer <name>，
    以及 PD 短键与 `config set` 的对应关系（如 schedule.max-store-down-time → max-store-down-time）。
 3. tiup br：是否接受 `br:<ver> backup full --pd ... --storage ...`（cmd.go 注释里没有 --pd，这里补上，BR 必须指定 PD）；S3 凭据靠 tidb 用户环境 / 实例角色，
    本包不传任何凭据。br.log-status 不带 --task-name（默认列出全部任务）。
 4. tiup cdc:<ver> cli capture list --server：cli 子命令与 --server 写法；本包取按字典序第一个 TiCDC 实例。
 5. SET CONFIG 的值字面量：size / duration / enum 用单引号字符串，int 不加引号，bool 输出 true/false；
    SET GLOBAL：size 换算成字节数（tidb_mem_quota_query），bool 为 ON/OFF，enum 单引号。需在目标 TiDB 版本上确认。
    SET CONFIG "host:statusPort" 中 TiFlash 用 proxy status 端口（display 第 5 个端口），请核对。
 6. cluster.check <c> <file> --cluster 的预检写法（planner 已使用）、cluster.upgrade 的 -y。
 7. DisplayTopo 依赖 `tiup cluster display` 的列布局（复用 source.ParseDisplay）；TiFlash 的 ID 在不同版本是 host:tcp_port 还是 host:flash_service_port 以实际输出为准（本包直接用输出里的 ID）。
 8. alert.silence 的 instance matcher 取请求里的 Instance 原值；Prometheus 告警里的 instance 标签是 host:port 还是状态端口取决于 exporter 配置，需核对。
 9. 没有内置令牌重放防护（有效期内同一令牌可重复使用）；需要一次性语义请实现 DirectRunner.Consume（例如 exec 子进程用 0600 文件记录已用令牌）。
    写命令的「同一集群同一时刻只执行一条」由调用方的执行引擎保证。
 10. 超时只终止直接子进程（CommandContext）；tiup 派生的子孙进程在 Linux 上不会被一并杀死（WaitDelay=3s 防止挂住读管道）。需要进程组终止可在 Linux 上补 Setpgid。
 11. 令牌绑定的是 Cmd 的规范 JSON（含 Nodes 顺序与 Content 全文）：签发与提交的 Cmd 必须逐字节一致。
 12. 密码环境变量：SudoRunner 经 sudo 时环境会被 sudoers 重置，exec 子进程需自行从 0400 文件 / systemd EnvironmentFile 取得 Policy.MySQLPwdEnv 指向的变量。
*/
package exec
