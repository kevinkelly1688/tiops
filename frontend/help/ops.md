# 日常运维

> 💡 集中放置值班常用的集群级操作：只读状态 / 预检、滚动重启与升级、集群巡检、备份与恢复、关键参数建议、变更审计。运维模式下查看与执行按角色授权；回放 / 只读模式下只有巡检和参数建议是真实的服务端数据，其他标签页是提示或原型演示。告警静默入口在「集群总览」页，本篇一并说明。

> ⚠️ 平台从未在真实集群上执行过命令。本篇描述的行为来自代码和模拟执行器（sim）下的测试，凡涉及 tiup、br、Alertmanager 的真实输出与耗时，一律以真实集群联调为准。

> 💡 **多集群**：平台可以纳管多个集群（见 [集群接入帮助](help:clusters)）。本页的状态、巡检结果、参数建议、备份、变更审计、以及总览页上的告警静默，都是**顶栏当前集群**的；巡检保存的是该集群最近一次结果；提交面板顶部的「集群 X」标签写着变更将提交到哪个集群，提交前先核对。回放集群恒用模拟执行器，不会作用于真实集群。

## 这个页面做什么

页面有 5 个标签页：

| 标签页 | 做什么 | 回放 / 只读模式 | 运维模式 |
|---|---|---|---|
| 集群操作 | 查看状态、预检、滚动重启、滚动升级 | 原型：生成命令和前置检查，点按钮只弹出「原型：已模拟执行」，不执行任何东西 | 查看状态 / 预检真实只读；重启与升级走「预览 → 提交变更 → 审批 → 执行」 |
| 巡检 | 一键生成集群健康巡检报告 | 真实采集（只读，无主机检查），「重新巡检」、浏览器「导出 / 打印」 | 可含主机检查，可导出 Markdown / HTML，保留最近一次结果 |
| 备份与恢复 | PITR 状态、发起备份、恢复 | 提示「需要启用运维模式」 | 可用 |
| 参数管理 | 当前值与推荐值对比 | 真实读取，可复制命令 | 同左 |
| 变更审计 | 最近的平台变更 | 提示「需要启用运维模式」 | 最近 30 条变更 |

> 💡 纯静态打开页面（没有后端）时，所有标签页都是原型模拟数据，只用于了解界面，顶栏「产品演示」标识会提示这一点。

运维模式下，各按钮需要的能力（角色）：

| 操作 | 能力 | 默认拥有该能力的角色 |
|---|---|---|
| 查看状态 / 预检 / PITR 状态、巡检、告警静默 | 运行诊断 / 巡检（diag.run） | 值班 SRE、DBA、审批人、管理员 |
| 导出巡检报告 | 导出诊断 / 巡检报告（report.export） | 值班 SRE、DBA、审批人、管理员 |
| 滚动重启 | 单实例操作（inst.op） | SRE、DBA（均为「需审批」） |
| 滚动升级 | 扩缩容 / 升级（scale） | DBA（需审批） |
| 备份 / 恢复 | 备份恢复操作（backup） | DBA（需审批） |
| 查看参数建议、变更审计列表 | 登录即可 | 所有登录用户 |

没有权限时，对应按钮置灰，并在页面上方显示黄色提示「你的角色没有「…」（xx）权限，操作按钮已置灰」。写操作（重启、升级、备份、恢复、告警静默）还要求「写操作开关」已开启，否则提交面板提示「当前不能提交：…」。

## 怎么用（按步骤）

### 通用前提

1. 用浏览器打开平台地址并登录（运维模式）。登录后顶栏右侧显示三块信息：环境标签（测试 / 预发 / 生产）、「写操作已开启」或「只读」、你的用户名与角色。
2. 看「写操作已开启 / 只读」：只读时，本页所有写操作都不能提交（只读命令、巡检、参数建议不受影响）。开启需要两名管理员确认，见「[权限与审计](go:security)」。鼠标悬停该标签可看原因（如存在 READONLY 熔断文件）。
3. 点侧栏「日常运维」，再点页内标签页。

### 一、查看集群状态与预检（只读）

**前置条件**：运维模式，且拥有「运行诊断 / 巡检」能力；执行器不是「未配置」。

1. 「集群操作」标签页，「操作类型」选「查看状态 display」。右侧出现蓝色说明：只读操作，运行 `tiup cluster display <集群名>`（`<集群名>` 是平台安装时 `--cluster` 指定的名称，页面会自动代入）。
2. 点「执行 display」。按钮变成「执行中…」（最长 30 秒）。你会看到下方黑底输出框列出每个实例的 ID、角色、主机、端口、状态、数据目录和部署目录，框下灰字「执行于 日期 时间」；状态列正常应为 Up。
3. 预检：「操作类型」选「预检 check」，点「执行 check」。运行 `tiup cluster check <集群名> --cluster`，会 SSH 到每台主机读取系统配置，较慢（执行器限制最长 120 秒）。输出是逐主机逐项的 Pass / Warn / Fail 表。
4. 与预期不符：输出框上方出现红色提示时，里面就是失败原因（超时、SSH 失败、未配置执行器等）。check 超时可改为在中控机上手工执行同一条命令。

**后果**：两条命令都只读，不加 `--apply`，不修复任何东西；每次点击都写入审计（cmd.read）。check 会访问所有主机，请避免高频点击。

**怎么读 check 的结果**：Fail 是必须处理的项，Warn 建议处理。常见检查项包括时间同步、swap、透明大页、内核参数、磁盘挂载选项（ext4 需要 nodelalloc）、端口占用、SELinux、防火墙；各项含义与修复办法见官方 [tiup cluster check](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-check) 与 [环境与系统配置检查](https://docs.pingcap.com/zh/tidb/stable/check-before-deployment)。平台不会替你修复。

### 二、集群巡检并导出报告

**前置条件**：「运行诊断 / 巡检」能力；导出还需要「导出诊断 / 巡检报告」能力。

1. 点「巡检」标签页。如果服务端保留了上一次巡检结果，页面会直接显示，右上角有按钮。
2. 需要检查主机层面（磁盘、内核参数、NTP 等）时，勾选「同时检查主机（tiup cluster check，较慢）」。不勾选则只采集监控与集群接口数据。
3. 点「执行巡检」。按钮变成「巡检中…」，勾选了主机检查时页面写「正在巡检（含主机检查，可能需要几分钟）…」。完成后右上角提示「巡检完成」。
4. 读报告：顶部四个计数标签「通过 / 警告 / 异常 / 未采集」，后面是集群名、版本、数据来源和时间。下面按分组（集群状态、存储、性能、资源、告警、配置、诊断、数据保护、主机、合规）逐项列出，每项一个状态标签、检查项名称和说明。先处理红色「异常」，再看橙色「警告」；灰色半透明的「未采集」表示取不到数据，平台不猜测，不是通过。各项判定与建议见下文「巡检项」表。
5. 留档：点「导出 Markdown」或「导出 HTML」，浏览器下载 `inspection.md` / `inspection.html`。没有按钮说明你缺少「导出诊断 / 巡检报告」能力或还没有结果。

**后果**：巡检不修改集群，写审计（inspect.run）；导出写审计（report.export）。服务端只保留**最近一次**巡检报告（数据目录下的 `inspect-last.json`，多集群时是各集群自己的目录），平台重启 / 升级后仍在；**所有有权限的人导出的是最近一次巡检**，所以导出前最好自己重新点一次「执行巡检」。注意这不是历史列表：再次巡检会覆盖上一份，每次巡检的记录只在「审计」里（`inspect.run`，含通过 / 警告 / 异常数）。

**与预期不符**：主机检查失败时，对应项显示「未采集」，说明里写「执行失败：…」，此时其余项仍有效；回放 / 只读模式没有「同时检查主机」选项，也不能导出文件，只能「导出 / 打印」调用浏览器打印。

### 三、提交滚动重启

**前置条件**：「单实例操作」能力（SRE / DBA）；写开关已开；变更的审批、定时、执行完整流程见「[变更排期](go:schedule)」。

1. 「集群操作」标签页，「操作类型」选「滚动重启 restart」。
2. 「角色」下拉框选要重启的角色：tikv / tidb / pd / tiflash / ticdc（默认 tikv）。选错角色会重启错误的组件，提交前再核对一次。
3. 点「预览并提交变更」。面板里看「前置检查」：该角色没有实例会报错；所在主机正在维护、健康实例不超过 1 个、TiKV 有主机正在维护都是红色阻断；另有 TiKV / PD / TiDB 各自的黄色提醒。有红色时「提交」按钮灰掉，先处理原因。
4. 「执行时间」建议选「定时执行…」并点推荐时间，避开业务高峰与备份窗口；填写变更说明 / 工单号；点「提交变更」。成功提示「变更 CHG-xxxx 已提交，等待审批」，页面跳到任务中心。
5. 审批人通过后，到点自动执行；到「节点管理 → 任务中心」看进度：第一步「滚动重启（tiup cluster restart -R）」，随后是每个实例一步「等待 xxx 就绪」（每个实例最多等 15 分钟，每 10 秒查一次是否 Up）。

> ⚠️ 实际执行的命令是 `tiup cluster restart <集群名> -R <角色> -y`，一旦开始**不可中止，也没有自动恢复命令**。TiKV 重启会有 Leader 切换，业务出现短暂抖动；TiDB 重启会断开连接；PD Leader 重启触发选举，TSO 秒级不可用。预计耗时约 3 分钟乘以实例数；执行器对这条命令的默认超时是 10 分钟（可由执行器策略调整），实例很多时可能不够。官方 [tiup cluster restart](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-restart) 页面写明重启期间会有一段时间服务不可用，请见下文「已知限制」。

### 四、提交滚动升级

**前置条件**：「扩缩容 / 升级」能力（DBA）；写开关已开；**升级前必须完成的准备**：

1. 在「备份与恢复」做一次全量备份（见下一节），并确认备份可用。平台只提醒，**不检查**最近是否真的有备份。
2. 阅读目标版本的 Release Notes 与兼容性变更，建议先在预发环境演练。
3. 官方升级文档要求先在中控机更新 TiUP 与 TiUP Cluster：`tiup update --self`，再 `tiup update cluster`；并用 `tiup cluster check <集群名> --cluster` 确认 Region 健康（官方提示结果应为 All regions are healthy）。**平台不会替你执行 tiup update**，请由 DBA 在中控机的部署用户下手工完成。离线环境需先导入目标版本的离线镜像，见官方 [搭建私有镜像](https://docs.pingcap.com/zh/tidb/stable/tiup-mirror)。

**步骤**

1. 「集群操作」标签页，「操作类型」选「滚动升级 upgrade」。
2. 「目标版本（格式 v8.5.3）」输入完整版本号，格式必须是 vX.Y.Z。格式不对点按钮时提示「版本格式应为 v8.5.3」。
3. 点「预览并提交变更」。看前置检查：目标版本等于当前或低于当前（不支持降级）是红色阻断；跨大版本或跨多个小版本是黄色提醒；任何实例不健康、任何主机在维护是红色阻断；固定黄色提醒「升级前必须完成一次全量备份…」。
4. 填写变更说明，选执行时间，提交。高风险：预发 / 生产需 2 级审批，生产还要求申请人填 MFA 验证码。
5. 到点执行时任务中心依次显示：升级前预检（tiup cluster check，失败不阻断）→ 滚动升级（`tiup cluster upgrade <集群名> <版本> -y`）→ 每个实例「等待 xxx 就绪」（每个最多 30 分钟）→ 查看集群状态。
6. 成功后到「集群操作」点「执行 display」，确认版本号与所有实例状态为 Up；再跑一次「巡检」。

> ⚠️ 升级开始后**不可中止**，失败只能人工处理，平台**不会自动回退版本**。预计耗时约 20 分钟加每个实例 4 分钟，执行器超时 3 小时；期间该集群不能同时执行其他变更（同一集群内串行；其他集群不受影响）。官方升级顺序是先 TiFlash（如有），再 PD、TiKV、TiDB；本页前置检查里写的「PD → TiKV → TiDB」是简化说法，以官方 [使用 TiUP 升级 TiDB](https://docs.pingcap.com/zh/tidb/stable/upgrade-tidb-using-tiup) 为准。

### 五、备份：全量备份与 PITR 日志备份

先理解两个概念（官方概述见 [备份与恢复概述](https://docs.pingcap.com/zh/tidb/stable/backup-and-restore-overview)）：

- **BR（Backup & Restore）**：TiDB 官方的备份恢复工具，平台通过 `tiup br` 调用它。
- **快照备份（全量备份）**：备份某一时刻整个集群的数据。**日志备份**：持续记录 TiKV 的数据变更日志，配合快照可恢复到任意时间点，这种能力叫 **PITR（Point-in-Time Recovery，时间点恢复）**。

**前置条件**：「备份恢复操作」能力（DBA）；写开关已开；已确认**存储路径前缀**：必须以执行器策略里 backupRoots 列出的前缀之一开头，例如 `local:///data/backup/` 或 `s3://tidb-backup/prod/`，具体前缀问平台管理员（配置在 `/etc/tidb-ops/exec.json`）。官方文档列出的备份存储是 Amazon S3、GCS、Azure Blob Storage 和 NFS（见 [备份存储](https://docs.pingcap.com/zh/tidb/stable/backup-and-restore-storages/)），用 `local://` 路径时其可见性请与 DBA 确认。路径里只允许字母、数字和 `_ . / : @ % + = , -`，最长 512 字符，且不能含 `..`，所以**不能把 S3 密钥写进路径**（`?` `&` 会被拒绝），凭据需要由执行用户的环境或实例角色提供。

**5.1 全量备份**

1. 点「备份与恢复」标签页，在「发起备份」卡片里「模式」选「全量备份」。
2. 「存储路径」填 `local:///data/backup/snap-20261006`（前缀 `local:///data/backup/` 来自 backupRoots，后面的 `snap-20261006` 建议用日期区分；填错前缀会被前置检查红色阻断，空着点按钮会提示「请填写存储路径」）。
3. 点「预览并提交变更」。面板显示：中风险、预计 60 分钟、将执行 `tiup br:<版本> backup full --pd <PD 地址> --storage <路径>`（`<版本>` 是集群版本，`<PD 地址>` 取自集群拓扑，均由平台代入）、前置检查和窗口检查。
4. 「执行时间」选「定时执行…」避开备份窗口与业务高峰，填工单号，点「提交变更」。生产环境中风险需要 1 级审批。
5. 执行后在「节点管理 → 任务中心」看日志；备份结束回到本页，「平台发起的备份记录」表里该行状态变为「成功」，点击行可打开变更详情。

> ⚠️ 「成功」只表示 br 命令退出码为 0，平台**不会校验备份文件是否可用**。请定期做恢复演练。全量备份读取全部数据，占用 TiKV 读 I/O 与网络，请在低峰执行；平台命令不带限速参数（没有 `--ratelimit` 入口）。官方 [快照备份与恢复指南](https://docs.pingcap.com/zh/tidb/stable/br-snapshot-guide) 指出，快照数据被 GC 清理后 `br backup` 会报错退出，备份耗时长时请评估 `tidb_gc_life_time`（含义见 [GC 配置](https://docs.pingcap.com/zh/tidb/stable/garbage-collection-configuration)）。

**5.2 启动 PITR 日志备份**

1. 「发起备份」卡片「模式」选「启动日志备份」。页面多出「日志备份任务名」输入框。
2. 「日志备份任务名」填 `pitr-prod`（只允许字母、数字、下划线、连字符，字母或数字开头，最长 64 位；这是你给任务起的名字，之后停止、查询都要用它）。空着提示「请填写日志备份任务名」。
3. 「存储路径」填 `s3://tidb-backup/prod/pitr`（同样要在 backupRoots 前缀内）。
4. 点「预览并提交变更」，风险「中风险」、预计 5 分钟，命令 `tiup br:<版本> log start --pd <PD> --task-name <名> --storage <路径>`，随后再执行一次 `log status`。提交、审批、执行同上。
5. 执行完成后点上方「PITR 日志备份状态」卡片的「查询 PITR 状态」，输出里应能看到你的任务和 checkpoint（见下一节）。

日志备份启动后会持续把变更日志写入存储，需保证存储容量与网络。命令与概念详见官方 [日志备份与 PITR 使用指南](https://docs.pingcap.com/zh/tidb/stable/br-pitr-guide)。

**5.3 查看 PITR 状态（只读，无需审批）**

1. 在「备份与恢复」标签页，「PITR 日志备份状态」卡片点「查询 PITR 状态」。按钮变成「查询中…」（最长 60 秒）。
2. 输出框显示 `tiup br:<版本> log status --pd <PD 地址>` 的结果：全部日志备份任务及其状态、checkpoint 时间。checkpoint 越接近当前时间说明日志备份越新；落后很多说明日志备份有延迟，需要排查。命令不带 `--task-name`，列出全部任务。需要「运行诊断 / 巡检」能力，写审计。

**5.4 停止 PITR 日志备份**

1. 「模式」选「停止日志备份」。「存储路径」输入框消失，只剩「日志备份任务名」，填要停止的任务名。
2. 点「预览并提交变更」，风险「高风险」、预计 5 分钟，命令 `tiup br:<版本> log stop --pd <PD> --task-name <名>`。预发 / 生产默认需 2 级审批，生产还要求申请人填 MFA 验证码。

> ⚠️ 停止后不再记录日志，**停止之后产生的数据变更无法再通过 PITR 恢复**；`br log stop` 还会清理该任务的元信息（见官方 [日志备份与 PITR 命令行手册](https://docs.pingcap.com/zh/tidb/stable/br-pitr-manual)）。没有明确理由不要停止。

### 六、从备份恢复

> ⚠️ **恢复是破坏性、不可逆的高风险操作。** 平台提供的是 `br restore full`（快照全量恢复）。官方要求目标是**空集群或不存在数据冲突（相同库 / 表）的集群**，并建议避免把数据恢复到正在提供服务的生产集群；从 v7.6.0 起 BR 默认连 `mysql` 库的系统表（含账号权限 `mysql.user`、SQL Binding、统计信息）一起恢复，平台命令没有加 `--with-sys-table=false`，恢复后账号和权限可能被备份里的内容覆盖（具体范围以官方说明和真实环境为准）。恢复期间集群性能显著下降，且没有自动恢复命令。平台只显示警告，**不检查目标是否为空**。强烈建议先在隔离的演练集群上验证备份可用。详见官方 [备份与恢复概述](https://docs.pingcap.com/zh/tidb/stable/backup-and-restore-overview) 与 [快照备份与恢复命令手册](https://docs.pingcap.com/zh/tidb/stable/br-snapshot-manual)。

**前置条件**：「备份恢复操作」能力（DBA）；写开关已开；已确认目标集群可以被恢复（空集群或已评估冲突）；备份存储路径在 backupRoots 前缀内；该备份确实存在且完整。

1. 「备份与恢复」标签页，左下「从备份恢复」卡片，卡片里有红色提示「恢复为高风险操作：要求目标集群为空…」。
2. 「备份存储路径」填备份所在位置，例如 `s3://tidb-backup/prod/snap-20261006`（需与备份时一致）。空着点按钮提示「请填写备份存储路径」。
3. 点红色「预览并提交恢复变更」。面板显示：高风险、预计 120 分钟、命令 `tiup br:<版本> restore full --pd <PD> --storage <路径>`（占位符含义同 5.1，由平台代入），黄色警告两条（目标集群要求为空、建议先演练），步骤旁有「不可中止」标签。
4. 填写变更说明（写清楚恢复目标与原因），选执行时间，提交。预发 / 生产需 2 级审批，生产还需 MFA。审批人必须逐项核对路径与目标集群。
5. 执行后在任务中心看日志，最长 12 小时；结束后「平台发起的备份记录」里该行类型为红色「恢复」，状态「成功」仅表示命令退出码为 0。恢复后请人工核对数据与账号权限。

**PITR 恢复（`br restore point`）平台没有入口**，也不支持恢复单库 / 单表；需要时由 DBA 在中控机上手工执行并留痕。官方 [备份与恢复概述](https://docs.pingcap.com/zh/tidb/stable/backup-and-restore-overview) 的使用限制写明 PITR 只支持恢复到**全新的空集群**。

**撤销**：无法撤销。唯一的补救是用另一份备份再恢复一次。

### 七、查看关键参数建议

**前置条件**：登录即可；只读模式也可以用。

1. 点「参数管理」标签页，页面先显示「正在读取…」，随后给出表格；右上角标签「需调整 N」「配置漂移 N」是汇总。
2. 看每行的「当前值」（取自集群）、「推荐」和「状态」：「需调整」橙色、「符合」绿色、「未采集」灰色（取不到值，平台不猜测默认值）。「配置漂移」表示各实例取值不一致，当前值用「、」连接。只读模式下「需调整」叫「建议调整」。
3. 点一行末尾的「复制命令」，命令被放进剪贴板，**平台不会执行**，也不会修改任何参数。
4. 想真正修改：到「[节点管理](go:nodes) → 配置变更」提交，会校验参数范围与类型、规划在线 / 滚动 reload 步骤、走审批并写审计。自己在命令行执行也可以，但 TiKV 的 `<集群>` 需要替换为真实集群名，`pd-ctl` 需要你的机器上有对应命令（官方 [PD Control 使用说明](https://docs.pingcap.com/zh/tidb/stable/pd-control)）。
5. 点「刷新」（只读模式叫「重新读取」）重新读取。

TiKV 在线用 SET CONFIG 修改后，官方提示还需用 `tiup cluster edit-config` 同步修改，否则后续 `upgrade`、`reload` 会把在线修改的结果覆盖；PD 在线修改持久化到 etcd。详见 [在线修改集群配置](https://docs.pingcap.com/zh/tidb/stable/dynamic-config)。

### 八、查看变更审计

1. 点「变更审计」标签页（只在运维模式可用）。
2. 表格列出最近 30 条平台变更：时间、编号、申请人、类型、内容、风险、审批进度、状态。点任意一行打开变更详情。
3. 这里只反映**通过本平台提交的变更**，不是 `tiup cluster audit`。更完整的审计（登录、拒绝、审批、每条执行的命令、设置修改，带哈希链校验）点右上角「查看完整审计日志（哈希链）→」进入「[权限与审计](go:security)」。

### 九、为已知告警做静默

**前置条件**：运维模式；「运行诊断 / 巡检」能力；**当前集群**的写开关已开；Alertmanager 地址已在执行器策略（`exec.json` 的 alertURL）里配置好——**多个集群共用 sudo 策略文件时，要在 `perCluster` 里为每个集群配置各自的 `alertURL`**，否则在非主集群做的静默会创建到全局 `alertURL` 所属的 Alertmanager 上（见 [集群接入帮助](help:clusters) 第四节的警告框）。静默属低风险写操作，**不走审批**。

1. 打开「[集群总览](go:overview)」，在「活跃告警」列表里找到该告警。有 diag.run 能力时，每行右侧有「静默」按钮（回放 / 只读模式或无能力时看不到）。
2. 点「静默」。右侧抽屉「静默告警」显示只读的「告警」「实例」「描述」，这三项就是将要匹配的条件。
3. 先看「实例」是否为空、是否是 `主机:端口` 形式。空表示会静默**整个集群同名的所有告警**，见下方警告。
4. 「静默时长（分钟，1–1440）」改为 `30`（默认 60；填 0 或超过 1440 会提示「时长需在 1–1440 分钟之间」）。
5. 「备注」写 `tikv-4 磁盘扩容中 CHG-0007`（最多 200 字）。
6. 点「确认静默」。成功时抽屉关闭，提示「已静默 告警名 30 分钟」；失败时抽屉里红字和右上角提示显示原因（如「实例 xxx 不在集群拓扑内」「静默失败：…」「写操作开关未开启：…」）。
7. 核对：打开 Alertmanager 自己的页面，在 Silences 里应有一条静默，匹配器为 `alertname` 与 `instance`（均为精确匹配，不是正则），创建者 `tidbops`，备注末尾带有操作人用户名。提前取消只能在 Alertmanager 里让该静默过期，平台没有查看或撤销静默的界面。Alertmanager 的静默概念见 [Alertmanager 官方文档](https://prometheus.io/docs/alerting/latest/alertmanager/)。

> ⚠️ 如果某条告警的 instance 为空，静默条件里就只剩 alertname，会静默**整个集群里同名的所有告警**，影响范围远大于你看到的这一行。静默前确认告警行的实例不为空。

> ⚠️ instance 必须是 `主机:端口` 形式，并且在集群拓扑（含状态端口）内，否则执行器拒绝。例如 node_exporter 的 9100 端口不在拓扑内时无法在这里静默。

静默期间，Alertmanager 不再把匹配的告警发给通知渠道（电话 / 钉钉 / 邮件等）；告警本身仍在产生，Prometheus 里仍可见；到期后如果问题还在，会重新通知。写审计（alert.silence，含告警名、实例、时长）。

## 区域与参数说明

### 集群操作标签页

「变更向导」左侧选操作类型，右侧显示该操作的状态卡、扩缩容入口卡和变更规范卡。

| 操作类型 | 运维模式下的行为 | 风险 | 可选值 / 默认 |
|---|---|---|---|
| 查看状态 display | 「执行 display」运行 `tiup cluster display <集群名>` | 只读 | 无参数；最长 30 秒 |
| 预检 check | 「执行 check」运行 `tiup cluster check <集群名> --cluster` | 只读 | 无参数；最长 120 秒 |
| 滚动重启 restart | 「预览并提交变更」打开提交面板 | 中 | 角色：tikv / tidb / pd / tiflash / ticdc，默认 tikv |
| 重载配置 edit-config + reload | 只显示说明：配置变更统一到「节点管理 → 配置变更」 | — | 本页不提供入口 |
| 滚动升级 upgrade | 输入目标版本后「预览并提交变更」 | 高 | 格式 vX.Y.Z，默认空 |
| 热修复 patch / 清理数据 clean | 红色「平台禁止」：不在执行器白名单内，没有提交入口 | — | 需要时由 DBA 在中控机上手工完成并留痕 |

### 巡检标签页

| 字段 / 按钮 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 同时检查主机（运维模式） | 额外运行 `tiup cluster check <集群名> --cluster`，结果并入「主机」分组 | 默认不勾选；需要 diag.run | 只读，较慢；超时则该项显示「未采集」并带失败原因 |
| 执行巡检 / 重新巡检 | 基于一次最新的数据快照生成报告 | — | 不修改集群 |
| 导出 Markdown / 导出 HTML（运维模式） | 下载最近一次巡检结果 | 需 report.export，且已有结果 | 写审计 |
| 导出 / 打印（只读模式） | 调用浏览器打印 | — | 无 |

状态含义：通过、警告、异常、未采集（取不到数据，平台不猜测）。只读模式下报告不按分组而按状态分块（异常 / 警告 / 未采集 / 通过），每项前有分组标签。

**巡检项**（以代码为准；只有取到数据的检查项才会出现）：

| 分组 | 检查项 | 判定 | 看到后怎么办 |
|---|---|---|---|
| 集群状态 | 拓扑可用 | 没采到任何实例 → 异常 | 检查平台到集群的采集配置 |
| 集群状态 | 所有组件 Up | 有实例状态不是 up / warn → 异常并列出实例 | 到「集群操作」执行 display，再去该主机查日志；参考[故障排查](go:trouble) |
| 集群状态 | PD 健康 / 多数派 | 健康 PD 不过半 → 异常；有不健康但仍过半 → 警告 | 异常需立即处理；警告说明容错已下降，尽快恢复 |
| 集群状态 | PD 数量（奇数且 ≥ 3） | 少于 3 个或偶数 → 警告 | 规划扩容到 3 个 |
| 存储 | TiKV 磁盘使用率 < 80% | 任一 ≥ 80% → 异常；≥ 70% → 警告 | 提前扩容或清理，参考 [节点管理](go:nodes) |
| 存储 | Region 分布均衡度 | 最大节点 Region 数 ≥ 均值 1.3 倍 → 警告 | 到[热点与调度](go:hotspot)查看 |
| 存储 | 写流量均衡度 | 最大节点写流量 ≥ 均值 3 倍 → 警告（疑似写热点） | 同上 |
| 存储 | TiKV 数量 ≥ 副本数 | TiKV 个数小于 max-replicas → 异常 | 补足 TiKV 或评估副本数设置 |
| 存储 | PD Store 状态 | 有 Store 不是 Up → 异常 | 对照 display 的状态列排查 |
| 性能 | P99 延迟 | ≥ 500 ms → 异常；≥ 100 ms → 警告 | 到[智能诊断](go:diag)、[SQL 优化](go:sql)定位 |
| 性能 | 慢查询（近 1 小时） | 没有 → 通过；不超过 2 类且总执行不足 100 次 → 通过；否则警告 | 到 SQL 优化页看具体语句；官方 [Statement 分析](https://docs.pingcap.com/zh/tidb/stable/dashboard-statement-list) |
| 资源 | TiDB 内存使用率 < 80% | 任一 ≥ 80% → 警告 | 排查大查询，评估内存配额 |
| 资源 | TiKV CPU 使用率 < 85% | 任一 ≥ 85% → 警告 | 排查热点与负载 |
| 告警 | 活跃告警 | 有严重告警 → 异常；只有警告级 → 警告 | 到[集群总览](go:overview)逐条处理；告警规则含义见官方 [报警规则](https://docs.pingcap.com/zh/tidb/stable/alert-rules) |
| 配置 | 副本数 max-replicas | 不是 3 或 5 → 警告 | 官方默认 3，见 [PD 配置](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file) |
| 配置 | max-store-down-time | 小于 10 分钟或大于 180 分钟 → 警告 | 官方默认 30 分钟，同上 |
| 合规 | 版本在平台支持范围内 | 不是 v7.5 / v8.1 / v8.5 系列 → 警告（平台写操作降级为只读） | 升级到支持的系列 |
| 诊断 | 规则诊断发现 | 有诊断发现 → 警告；其中有严重级 → 异常 | 到[智能诊断](go:diag)查看；官方还有 [TiDB Dashboard 集群诊断](https://docs.pingcap.com/zh/tidb/stable/dashboard-diagnostics-access) |
| 数据保护 | 最近一次全量备份 | 永远「未采集」：平台没有只读通道取备份历史 | 到「备份与恢复」看 PITR 状态与平台发起的备份记录 |
| 主机（可选） | tiup cluster check 的每个检查项 | 按检查名汇总：任一主机 Fail → 异常，Warn → 警告，否则通过；说明里最多列 3 条详情 | 见上文「怎么读 check 的结果」 |

### 备份与恢复标签页（仅运维模式）

「发起备份」卡片：

| 字段 | 含义 | 可选值 / 默认 | 填错后果 |
|---|---|---|---|
| 模式 | 全量备份 / 启动日志备份 / 停止日志备份 | 默认全量备份 | 决定风险与命令 |
| 存储路径 | 备份存放位置；全量备份和启动日志备份必填，停止日志备份不显示 | 必须在 backupRoots 前缀内，字符集见上文 | 不在前缀内：前置检查红色阻断，执行器也会再次校验 |
| 日志备份任务名 | PITR 任务名；启动 / 停止日志备份必填 | 例如 pitr-prod | 作为 `--task-name`；停止时名字不对则停不掉目标任务 |

三种模式的风险与命令：

| 模式 | 风险 | 预计 | 命令 | 备注 |
|---|---|---|---|---|
| 全量备份 | 中 | 60 分钟 | `tiup br:<版本> backup full --pd <PD> --storage <路径>` | 不可中止；执行器超时 12 小时 |
| 启动日志备份 | 中 | 5 分钟 | `tiup br:<版本> log start --pd <PD> --task-name <名> --storage <路径>`，随后 `log status` | 日志会持续写入存储；执行器超时 60 秒 |
| 停止日志备份 | 高 | 5 分钟 | `tiup br:<版本> log stop --pd <PD> --task-name <名>` | 预发 / 生产需 2 级审批，生产另需 MFA；停止后无法再恢复到停止之后的任意时间点；执行器超时 60 秒 |

「从备份恢复」卡片只有「备份存储路径」一个字段，命令 `tiup br:<版本> restore full --pd <PD> --storage <路径>`，高风险、预计 120 分钟、执行器超时 12 小时、不可逆。

「平台发起的备份记录」表：编号、类型（全量备份 / 启动日志备份 / 停止日志备份 / 恢复）、内容与存储路径、时间、申请人、状态；点击行打开变更详情。只列通过本平台提交的备份 / 恢复，不包含 cron 或其他工具做的备份。

页面右下「注意事项」是固定文字，不来自你的真实备份策略。

### 参数管理标签页

读取集群当前值（TiDB 系统变量来自 information_schema 的 session_variables，TiKV / TiDB / PD 实例配置来自 cluster_config，PD 集群级参数来自 PD 配置接口），与内置的推荐值对比。取不到的值显示「—」，状态「未采集」。

| 列 | 含义 |
|---|---|
| 范围 | tidb / tikv / pd |
| 参数 | 参数名 |
| 当前值 | 集群里的取值；各实例不一致时用「、」连接并标「配置漂移」，状态判为需调整 |
| 推荐 | 内置的参考建议，只作参考 |
| 原因 / 状态 / 复制命令 | 为什么这样建议；需调整 / 符合 / 未采集；只复制，不执行 |

内置参数与「需调整」的判定条件：

| 范围 | 参数 | 推荐 | 判为需调整的条件 |
|---|---|---|---|
| tidb | tidb_mem_quota_query | 2 GiB，并配合 tidb_mem_oom_action=CANCEL | 当前值恰好等于 1073741824（1 GiB） |
| tidb | tidb_enable_prepared_plan_cache | ON | OFF 或 0 |
| tidb | tidb_gc_life_time | ≥ 最长事务时长；备份期间 ≥ 备份时长 | 无判定，只展示 |
| tidb | tidb_replica_read | closest-replicas（跨 AZ 部署） | 无判定，只展示 |
| tidb | tidb_slow_log_threshold | 300（OLTP，毫秒） | 大于 0 且小于 100 |
| tikv | storage.block-cache.capacity | ≤ 内存的 40% 到 45% | 无判定，只展示 |
| tikv | raftstore.store-pool-size | 2 到 5 | 大于 5 |
| tikv | storage.scheduler-worker-pool-size | 按 CPU 核数 | 无判定，只展示 |
| pd | schedule.region-schedule-limit | 按迁移需求，高峰调小 | 大于 4096 |
| pd | replication.location-labels | zone,rack,host（按真实机房） | 非空且不含 zone |
| pd | schedule.max-store-down-time | 30m（计划内长维护需临时调大） | 无判定，只展示 |

> ⚠️ **「符合」只表示取到了值且没触发上面的判定条件**。标「无判定，只展示」的参数，只要取到值就显示「符合」，并不代表已是最优。请结合硬件规格、版本和官方文档自行判断，参数含义见官方 [TiKV 配置文件](https://docs.pingcap.com/zh/tidb/stable/tikv-configuration-file)、[PD 配置文件](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file)、[系统变量](https://docs.pingcap.com/zh/tidb/stable/system-variables)。

「复制命令」的细节：

- TiDB 变量是 `SET GLOBAL ...;`，TiKV / PD 参数是 `tiup cluster edit-config <集群>` 加 `reload`，或 `pd-ctl config set ...`。
- 第一条 tidb_mem_quota_query 的命令只设置内存配额，**不包含**推荐里提到的 tidb_mem_oom_action=CANCEL，需要另行设置。tidb_gc_life_time 复制出来的 `'30m'` 只是示例值，不要原样套用。

### 总览页上的告警静默字段

| 字段 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 告警 / 实例 / 描述 | 只读，取自该告警行 | — | 决定静默的匹配条件 |
| 静默时长（分钟，1–1440） | 静默持续时间 | 整数 1 到 1440，默认 60 | 到期自动失效 |
| 备注 | 说明原因 | 最多 200 字，可空 | 服务端会在后面追加「(by 你的用户名)」，Alertmanager 里还会再带一段 [by 用户名]；备注靠近 200 字会因此超限 |

**匹配范围**：向 Alertmanager 创建一条静默，条件是 `alertname` 等于该告警名，并且（告警带有 instance 标签时）`instance` 等于该实例，都是精确匹配，不用正则。

### 总览页上的事件时间线与部署自检

- 「集群总览」里的「事件时间线」汇总近期（最多显示 40 条）的告警、变更创建 / 结束、主机维护、严重诊断发现，点变更条目可打开变更详情。
- 「权限与审计 → 部署自检」检查平台自身的安全基线（是否以 root 运行、数据文件权限、审计链、执行器模式、是否有至少两名管理员、MFA、READONLY 熔断文件、集群版本是否在支持矩阵内、生产是否误用模拟执行器等）。上线前和每次升级后建议看一遍。

## 执行后果与风险

| 操作 | 实际命令 | 影响 | 能否撤销 | 审批（生产默认） |
|---|---|---|---|---|
| 查看状态 / 预检 / PITR 状态 | display / check / log status | 只读，会访问各主机 / PD | 不涉及 | 不需要，写审计 |
| 滚动重启 | `tiup cluster restart -R` | 服务抖动，不可中止 | 否 | 1 级 |
| 滚动升级 | `tiup cluster upgrade -y` | 版本变更，不可中止，不自动回退 | 否 | 2 级 + MFA |
| 全量备份 | `br backup full` | 占用读 I/O 与网络 | 不涉及 | 1 级 |
| 启动日志备份 | `br log start` | 持续写入存储 | 可停止 | 1 级 |
| 停止日志备份 | `br log stop` | 之后的时间点无法 PITR | 否 | 2 级 + MFA |
| 恢复 | `br restore full` | 覆盖 / 冲突、性能下降、可能覆盖账号权限 | 否 | 2 级 + MFA |
| 告警静默 | Alertmanager `POST /api/v2/silences` | 临时不通知 | 可在 Alertmanager 让其过期 | 不需要，写审计 |

> ⚠️ 执行器为 sim（模拟）时，上述所有命令只在平台内模拟输出，不会改动或读取任何真实集群；巡检里的主机检查输出也是模拟的。

> ⚠️ 升级期间该集群不能同时执行其他变更（同一集群内串行；其他集群不受影响）；备份与恢复的「成功」只表示命令退出码为 0。

## 常见问题与注意

- **「执行巡检」「查询 PITR 状态」按钮是灰的？** 你没有 diag.run 权限（只有 SRE、DBA、审批人、管理员有），或正在执行中。
- **提交面板提示「当前不能提交」？** 写操作开关未开启、存在 READONLY 熔断文件、未配置执行器，或集群版本不在 v7.5 / v8.1 / v8.5 系列。请联系管理员处理。
- **想修改参数怎么办？** 参数管理页只提示，不修改。请走「节点管理 → 配置变更」。
- **巡检的主机检查超时？** 执行器对 tiup cluster check 的单次限制是 120 秒，节点多时可能超时，该项显示「执行失败」并算作未采集。可改用在中控机上手工执行 `tiup cluster check <集群名> --cluster`。
- **巡检结果和刚才不一样？** 报告基于点击那一刻采集的一次快照，集群状态在变化，结果自然会变。
- **想要静默的告警在总览里没有「静默」按钮？** 你没有 diag.run 权限，或当前是回放 / 只读模式。
- **静默提示「备注过长」？** 请把备注缩短到 180 字以内。
- **备份记录里没有我定时备份的记录？** 记录只包含通过本平台提交的备份。
- **备份提示存储路径不在允许的前缀内？** 前置检查红色阻断，让管理员核对 `/etc/tidb-ops/exec.json` 的 backupRoots，路径需以其中一项开头。

## 已知限制与易混淆点

核对于代码与官方文档，格式为「界面 / 文档写的是 A，实际是 B」。

| 界面 / 文档写的是 | 实际是 | 建议 |
|---|---|---|
| 「滚动重启」，前置检查写「驱逐 Leader 后逐个重启」 | 平台只执行 `tiup cluster restart -R <角色> -y`，不带驱逐 Leader 或逐个重启的参数；官方 restart 页写明重启期间会有一段时间服务不可用，「依次重启、保持可用」的描述在官方 reload 页 | 以真实集群联调为准；对只有少量实例的 TiKV / PD 谨慎选择时间，不确定时由 DBA 在中控机上评估 |
| 「升级顺序 PD → TiKV → TiDB」 | 官方升级文档的顺序是 TiFlash（如有）→ PD → TiKV → TiDB | 以官方 [升级指南](https://docs.pingcap.com/zh/tidb/stable/upgrade-tidb-using-tiup) 为准 |
| 升级只需提交「滚动升级」 | 平台不执行 `tiup update --self` 与 `tiup update cluster`，也不检查是否有备份 | 升级前手工更新 TiUP 并先做全量备份 |
| 备份前置检查提示「建议…限速（--ratelimit）」 | 平台命令不带任何限速参数，没有填写入口 | 低峰执行，或由 DBA 评估 TiKV 侧限速 |
| 路径白名单为空时前置检查看起来全部放行 | 前置检查在 backupRoots 为空时不拦，但执行器要求路径以某个前缀开头，执行时仍会拒绝 | 安装后务必配置 backupRoots |
| 「恢复」像是通用恢复入口 | 只有快照全量恢复，没有 PITR 恢复、单库 / 单表恢复；也不带 `--with-sys-table=false` | 其余场景由 DBA 手工执行 |
| 「备份成功」 | 只表示 br 退出码为 0，巡检的「最近一次全量备份」永远是「未采集」 | 定期做恢复演练，自行监控备份与 checkpoint |
| 静默后告警从总览消失 | 平台读取 Alertmanager 的 /api/v2/alerts 时没有过滤已静默的告警，而该接口默认包含已静默的告警（官方接口规范里 silenced 默认 true），所以静默后总览与巡检的「活跃告警」仍可能显示它 | 以 Alertmanager 页面的静默状态为准，看通知是否已停止 |
| 静默备注「(by 用户名)」 | 备注里实际出现两个操作人标记：服务端追加 (by 用户名)，执行器再追加 [by 用户名] | 无需处理 |
| 「check」预检是纯只读 | 不改系统配置，但会 SSH 到每台主机，耗时长，每次点击都写审计 | 避免高频点击 |
| 巡检「导出」导出的是我刚才的结果 | 导出的是服务端保存的最近一次巡检，可能是别人做的 | 导出前先自己「执行巡检」 |
| 「变更审计」对应 tiup cluster audit | 只列平台提交的最近 30 条变更，不是 tiup 的审计 | 完整审计去「权限与审计」 |
| 参数「符合」 | 部分参数无判定，取到值即「符合」 | 结合官方文档与硬件评估 |
| 页面注意事项「每日 02:00 有计划备份窗口」 | 固定文字，与实际备份策略和默认窗口规则（01:30 到 04:00，仅「不建议」）都不一致 | 以你的真实备份计划为准 |

## 官方参考

以下链接均已打开核实；平台支持 v7.5 / v8.1 / v8.5，页面以 stable 为准，**以你集群的版本为准**，必要时把地址里的 stable 换成对应版本（如 v8.1）。

- [tiup cluster check](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-check)、[环境与系统配置检查](https://docs.pingcap.com/zh/tidb/stable/check-before-deployment)：check 的检查项（时间同步、swap、THP、内核参数、nodelalloc 等）与修复办法。
- [tiup cluster display](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-display)：状态列取值 Up / Down 等含义。
- [tiup cluster restart](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-restart)、[reload](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-reload)、[edit-config](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-edit-config)：重启、依次重启、改配置后需 reload。
- [使用 TiUP 升级 TiDB](https://docs.pingcap.com/zh/tidb/stable/upgrade-tidb-using-tiup)、[tiup cluster upgrade](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-upgrade)、[搭建私有镜像](https://docs.pingcap.com/zh/tidb/stable/tiup-mirror)：升级前准备、顺序、命令选项、离线镜像。
- [备份与恢复概述](https://docs.pingcap.com/zh/tidb/stable/backup-and-restore-overview)、[快照备份与恢复指南](https://docs.pingcap.com/zh/tidb/stable/br-snapshot-guide)、[快照备份与恢复命令手册](https://docs.pingcap.com/zh/tidb/stable/br-snapshot-manual)、[日志备份与 PITR 使用指南](https://docs.pingcap.com/zh/tidb/stable/br-pitr-guide)、[日志备份与 PITR 命令行手册](https://docs.pingcap.com/zh/tidb/stable/br-pitr-manual)、[备份存储](https://docs.pingcap.com/zh/tidb/stable/backup-and-restore-storages/)：BR 概念、命令、限制、系统表恢复、存储类型。
- [GC 配置](https://docs.pingcap.com/zh/tidb/stable/garbage-collection-configuration)：tidb_gc_life_time 的含义。
- [系统变量](https://docs.pingcap.com/zh/tidb/stable/system-variables)、[TiKV 配置文件](https://docs.pingcap.com/zh/tidb/stable/tikv-configuration-file)、[PD 配置文件](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file)、[在线修改集群配置](https://docs.pingcap.com/zh/tidb/stable/dynamic-config)、[PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control)：参数含义与修改方式。
- [TiDB 集群报警规则](https://docs.pingcap.com/zh/tidb/stable/alert-rules)、[TiDB 监控框架](https://docs.pingcap.com/zh/tidb/stable/tidb-monitoring-framework)、[Dashboard 集群诊断](https://docs.pingcap.com/zh/tidb/stable/dashboard-diagnostics-access)、[Dashboard SQL 语句分析](https://docs.pingcap.com/zh/tidb/stable/dashboard-statement-list)：告警、监控组件、诊断与 Statement 概念。
- [Alertmanager（Prometheus 官方文档）](https://prometheus.io/docs/alerting/latest/alertmanager/)：静默按匹配器工作，匹配的告警不发通知。
- [Alertmanager API v2 规范](https://raw.githubusercontent.com/prometheus/alertmanager/main/api/v2/openapi.yaml)：GET /alerts 默认包含已静默的告警；创建静默的字段。

## 相关页面

- [变更排期](go:schedule)：定时执行、审批、变更冻结期与窗口规则。
- [节点管理](go:nodes)：实例操作、扩缩容、配置变更、主机维护与任务中心。
- [集群总览](go:overview)：活跃告警、事件时间线与告警静默入口。
- [环境与设置](go:settings)：变更窗口规则、通知渠道、版本与升级信息。
- [集群接入](go:clusters)：多集群的接入与切换（本页的数据与变更都是当前集群的），帮助见 [集群接入帮助](help:clusters)。
- [权限与审计](go:security)：审计日志、部署自检、写操作开关与审批策略。
- 帮助：[变更排期](help:schedule)、[常见问题](help:faq)
