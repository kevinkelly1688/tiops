# 故障排查

> 💡 值班时遇到「写入变慢 / TiKV server is busy / 延迟升高」等问题，用决策树逐步缩小范围，并用当前集群的实时数据做参考；另有故障排查地图和常见日志错误速查。所有角色都能打开。本页**只读、只给建议**，不会执行任何命令，也不会自动修复。只读模式与运维模式内容相同（运维模式需先登录，实时证据只要求 view.overview）。

## 这个页面做什么

页面顶部有三个标签页：

| 标签页 | 作用 | 数据来自 |
|---|---|---|
| 故障排查地图 | 22 个常见故障条目，可搜索、按分类筛选，展开看可能原因、排查步骤、解决方案和示例命令 | 前端内置的知识库（整理自官方「TiDB 集群问题导图」） |
| 排障向导 | 选症状后回答一连串是/否问题，最后给出定位结果与建议处置；真实 / 回放数据下，部分问题会附带「实时数据建议」 | 决策树是前端内置的；实时证据来自后端接口 `GET /api/wizard/evidence`，该接口只读已采集的快照 |
| 日志错误速查 | 9 条常见日志报错的含义、处理建议与级别 | 前端内置 |

先认识几个词（向导和地图里反复出现）：

- **TiKV**：存数据的组件；**PD**：调度与元信息组件；**TiDB**：SQL 层。三者关系见 [集群架构](go:arch)。
- **Raftstore**：TiKV 里处理 Raft 日志（多副本一致性协议）的线程池；写入要先经过它。线程池大小由 `raftstore.store-pool-size` 配置。
- **Apply**：把已提交的 Raft 日志真正写入存储引擎的线程池，大小由 `raftstore.apply-pool-size` 配置。
- **RocksDB write stall**：TiKV 底层存储引擎 RocksDB 因 Compaction（后台合并整理）跟不上写入而主动限流，日志里会出现 `Stalling`。
- **Coprocessor**：TiKV 里执行下推过来的读请求（扫描、过滤、聚合）的模块。
- **Grafana 的 TiKV-Details / Performance Overview**：官方监控面板，平台不内嵌，需要在 Grafana 里看（地址见 [环境与设置](go:settings) 的「组件地址」，面板说明见文末「官方参考」）。
- **快照**：平台后台定期采集的一份集群数据（拓扑、指标、告警、诊断结论、DB Time 分解），实时证据只读它。

三种数据状态：静态原型（右上角「产品演示」，没有实时证据，向导只有决策树）、回放数据（顶部横幅「回放数据」）、真实数据（顶部横幅「真实数据（只读）」）。后两种才有实时证据。

> ⚠️ 实时证据只是建议：向导**不会替你选答案、不会自动跳步**，最后的选择必须由你点击；结论只是处置建议，页面不会执行其中任何一步。

> ⚠️ 平台从未对真实集群运行过，实时证据的触发阈值来自代码里的规则，是否合适以真实集群联调为准。

## 怎么用（按步骤）

### 用排障向导定位原因

**前置条件**：知道当前遇到的症状属于「写入变慢」「TiKV server is busy」「延迟升高」中的哪一类；能打开 Grafana（大多数问题要对照面板或日志回答）。

1. 在左侧菜单点「故障排查」，再点顶部「排障向导」标签页。
   - 你会看到：真实 / 回放数据下，上方先出现「当前集群事实」卡片（实例状态、磁盘、内存、严重告警等线索，带 ✖ 严重 / ⚠ 警告 / ✔ 正常图标）；下面是「选择你遇到的症状」，三个按钮，每个写着症状名和「按决策树逐步定位根因」。
   - 与预期不符：静态原型或取数失败时没有「当前集群事实」卡片，这是正常的。
2. 先扫一眼「当前集群事实」：有 ✖ 的线索（如实例停止、TiKV 磁盘 ≥ 80%）先处理，它们常常就是症状的原因；点线索后的「查看 →」跳到对应页面。
3. 点一个症状按钮：「写入变慢」、「TiKV server is busy」或「延迟升高」。
   - 你会看到：卡片标题变成「排障向导 · 症状名」，出现第一个问题和「是 / 否」按钮（「延迟升高」的第一个问题是「短暂尖峰」「持续偏高」两个选项按钮）。
4. 读问题，到问题对应的位置核对（每个问题怎么查、怎么判断见下文「向导问题怎么回答」）。如果问题下方有蓝色提示「实时数据建议：是 / 否 —— 依据」，对应按钮会带「· 建议」字样。
   - 建议只是参考，请对照 Grafana 或日志自己确认；蓝色提示末尾的「查看 →」可跳到热点、诊断、节点或总览页核对依据。
5. 点「是」或「否」（或选项按钮）进入下一步。顶部会增加一枚路径小标签，格式是「问题前 16 个字…→你的回答」，用来回看走过的路。
   - 与预期不符：向导**没有「上一步」**，点错了只能点「重新选择症状」从头再来。
6. 重复第 4、5 步，直到出现绿色的「定位结果：…」，下面是说明和「建议处置」编号列表。
   - 如果结果下方有「查看相关页面 →」按钮，点它跳到相关页面（热点或 SQL 优化）。
7. 按「建议处置」逐项处理。涉及重启、配置变更、扩缩容的，到 [节点管理](go:nodes) 或 [日常运维](go:ops) 提交变更，不要只靠向导页。
8. 要排查另一个症状时点「重新选择症状」；切换到别的标签页再回来也会重置向导。

### 完整走查：从症状到结论

下面三个示例的数字只是示例。

**示例一：业务反馈「写入变慢」，实时证据指向写热点（最短路径）**

1. 打开「排障向导」，「当前集群事实」显示：✖ 1 条严重告警正在触发（TiKV_raftstore_thread_cpu_seconds_total）、✖ 诊断引擎发现 tikv-2 疑似写入热点（来自 [智能诊断](go:diag)）。
2. 点「写入变慢」。第一个问题 w1「TiKV-Details 中 Raftstore CPU 是否只有个别节点很高（与其他节点差距明显）？」下方出现蓝色提示：「实时数据建议：是 —— 诊断引擎判定：tikv-2 疑似写入热点（…）」，「是」按钮带「· 建议」。
3. 你到 Grafana 的 TiKV-Details →「Thread CPU」→「Raftstore CPU」核对，确实只有 tikv-2 的曲线明显高于其他节点，点「是」。
4. 页面给出绿色「定位结果：写热点」，建议处置是：在 Key Visualizer 确认「阶梯状斜线」；用 AUTO_RANDOM / SHARD_ROW_ID_BITS 打散；必要时手动拆分 Region。点「查看相关页面 →」跳到 [热点与调度](go:hotspot)，核对热点 Region 的表名与范围，再按 [热点与调度帮助](help:hotspot) 的「使用解决方案」操作。

**示例二：「写入变慢」，Raftstore CPU 各节点都不高，一路走到「事务层冲突」**

1. 点「写入变慢」，w1 问「Raftstore CPU 是否只有个别节点很高」。Grafana 看各 TiKV 的 Raftstore CPU 曲线差不多高低，点「否」。
2. w2「RocksDB 日志是否出现 Stalling，或 Write Stall 面板有数据？」。在 TiKV 日志里执行 `grep -i stalling tikv.log`（日志路径见 [节点管理](go:nodes) 实例详情的「日志目录」）没有输出，Grafana 的 Write stall duration 为 0，点「否」。
3. w3「WAL Sync Duration 是否偏高（> 数 ms 持续）？」。Grafana 的 Raft Engine →「WAL sync duration」只有零点几毫秒，点「否」。
4. w4「Apply log duration / Propose wait duration 是否偏高？」。两个面板都平稳，点「否」。
5. 页面给出「定位结果：事务层冲突 / 锁等待」，建议：看 latch wait duration；查 DATA_LOCK_WAITS / DEADLOCKS；缩短事务、拆分大批量写入。点「查看相关页面 →」到 [SQL 优化](go:sql) 找对应的慢 SQL。
6. 注意这是「排除法」的结论：前面所有问题都回答「否」才会落到这里，如果你在 w2–w4 里有一处判断不准，结论也会偏。有疑问就用 w3、w4 面板的真实数值重新走一遍。

**示例三：「延迟升高」，持续偏高，一路走到「Store 侧瓶颈」**

1. 点「延迟升高」，l1 选「持续偏高」（若延迟只是突然上去又回落，选「短暂尖峰」，会进入 l2，问是否同时发生了 PD Leader 切换 / TiKV 掉 Leader）。
2. l5「Performance Overview 中 TiDB–TiKV 延迟差是否 > 5ms（同机房应 < 2ms）？」。在 Grafana 的 Performance Overview 里比较 TiDB 侧看到的请求延迟与 TiKV 侧的处理延迟（具体面板见文末官方 Performance Overview 说明），差不到 1ms，点「否」。
3. l6「DB Time 中 Compile 占比是否很高（橙色）？」。蓝色提示「实时数据建议：否 —— Compile 仅占 DB Time 8%」，你在 [总览](go:overview) 的「数据库时间 (DB Time) 分解」卡片核对一致，点「否」。
4. 页面给出「定位结果：Store 侧瓶颈」，建议处置：增加 store 线程；启用 Raft Engine；排查磁盘 / Compaction。到 [节点管理](go:nodes) 看 TiKV 实例的磁盘与 Raftstore CPU，需要改配置时提交变更。

### 用故障排查地图查已知故障

1. 点「故障排查地图」标签页（进入本页时的默认标签页）。
   - 你会看到：一个搜索框、分类标签（全部 / 服务不可用 / 延迟升高 / TiDB / TiKV / PD / 生态工具）、右侧「官方排查地图 ↗」链接，下面是条目列表。
2. 在搜索框输入现象、报错或关键词，例如 `server is busy`、`OOM`、`热点`；也可点分类标签筛选。
   - 你会看到：列表即时过滤；没有匹配时显示「没有匹配的条目」。搜索和分类选择在你离开本页再回来时仍保留，直到刷新浏览器。
3. 点某一行展开，依次看「可能原因 / 排查步骤 / 解决方案」；部分条目底部有示例命令块，点「复制」。
   - 复制的是演示命令，**必须先替换占位值再执行**，见下文提示。
4. 要看官方原文，点「官方排查地图 ↗」。

### 用日志错误速查查一条报错

1. 点「日志错误速查」标签页。
   - 你会看到：一张表，列为「日志 / 报错」「含义」「处理建议」「级别」。
2. 在表里找到与你日志相同或相近的报错，读「含义」和「处理建议」；「级别」标签告诉你紧急程度：可自愈（蓝）、需关注（黄）、需处理（红）。
3. 级别为「需处理」的（Region is unavailable、TiKV server is busy）立即排查，并到「故障排查地图」或「排障向导」按对应条目继续。

## 区域与参数说明

### 故障排查地图

| 控件 / 区域 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 搜索框 | 对每个条目的全部文字（标题、症状、原因、步骤、方案、命令）做不区分大小写的包含匹配 | 默认空；输入会立即过滤，并保留光标位置 | 仅前端过滤 |
| 分类标签 | 按分类筛选 | 全部（默认）/ 服务不可用 / 延迟升高 / TiDB / TiKV / PD / 生态工具 | 与搜索条件同时生效 |
| 官方排查地图 ↗ | 外链到官方文档 | 新窗口打开 | 无 |
| 条目折叠面板 | 点击展开 | 每条含分类标签、标题、症状；展开后是可能原因、排查步骤、解决方案和（可选）命令 | 无 |
| 命令块的「复制」 | 复制示例命令 | 无 | 只复制，不执行 |

各分类的条目：

| 分类 | 条目 |
|---|---|
| 服务不可用（2） | Region is Unavailable；PD 异常导致集群不可用 |
| 延迟升高（2） | 延迟短暂升高；延迟持续升高 |
| TiDB（6） | DDL 卡住 / 很慢；information schema is changed；TiDB OOM；执行计划不对 / SQL 突然变慢；锁冲突 / Write Conflict；数据一致性检查 |
| TiKV（5） | TiKV 启动失败 entries is unavailable；TiKV OOM；TiKV server is busy；大量掉 Leader；写入慢（按阶段拆解） |
| PD（4） | Region merge 卡住 / 跨表无法合并；副本补齐慢 / Region 不均衡；PD Leader 频繁切换 / 选不出 Leader；PD OOM |
| 生态工具（3） | DM 连接类错误；TiDB Lightning 导入慢 / checksum 失败；TiCDC 同步延迟变大 |

> ⚠️ 地图条目里的示例命令是**固定的演示写法**，真实数据模式下**不会**被替换成你集群的实际值，复制后必须先按下表改成自己的再执行：

| 示例里的值 | 换成什么 | 从哪里取 |
|---|---|---|
| `10.0.1.21:2379` | 你集群任一 PD 的地址 | [环境与设置](go:settings) 的「组件地址」里的 PD |
| `prod-tidb-01` | 你的集群名 | 顶部横幅「集群 xxx」或 [环境与设置](go:settings) 的「接入的集群」 |
| `v8.5.2`（出现在 `tiup ctl:v8.5.2`） | 你集群的实际版本 | [环境与设置](go:settings) 的「TiDB 版本」 |
| `/tidb-deploy/tikv-20160/log/tikv.log` 等路径 | 该实例的日志文件 | [节点管理](go:nodes) 实例详情里的目录信息；取不到时，到该主机上用 `tiup cluster display <集群名>` 查看 Deploy Dir，日志通常在其下的 `log` 目录 |

### 排障向导：症状入口与决策树

向导目前有 3 个症状。下表列出每个问题、回答后的去向，以及该问题是否有实时证据（见下一节）。

写入变慢：

| 问题 | 选「是」 | 选「否」 | 实时证据 |
|---|---|---|---|
| w1 TiKV-Details 中 Raftstore CPU 是否只有个别节点很高？ | 结论「写热点」 | 进入 w2 | 有 |
| w2 RocksDB 日志是否出现 Stalling，或 Write Stall 面板有数据？ | 结论「RocksDB Write Stall」 | 进入 w3 | 仅在有相关告警时给「是」 |
| w3 WAL Sync Duration 是否偏高（持续超过数毫秒）？ | 结论「磁盘写入瓶颈（Append Log 阶段）」 | 进入 w4 | 无 |
| w4 Apply log duration / Propose wait duration 是否偏高？ | 结论「线程池不足（Raftstore / Apply）」 | 结论「事务层冲突 / 锁等待」 | 无 |

TiKV server is busy：

| 问题 | 选「是」 | 选「否」 | 实时证据 |
|---|---|---|---|
| b1 日志中是否有 "Stalling" 字样？ | 结论「RocksDB write stall」 | 进入 b2 | 仅在有相关告警时给「是」 |
| b2 latch wait duration 是否很高（写写冲突严重）？ | 结论「写冲突 / 热点行」 | 进入 b3 | 无 |
| b3 日志出现 channel full 或 raftstore 线程 CPU 接近 100%？ | 结论「raftstore 繁忙」 | 结论「Coprocessor 请求排队」 | 有 |

延迟升高：

| 问题 | 选项 | 去向 | 实时证据 |
|---|---|---|---|
| l1 延迟是「短暂尖峰」还是「持续偏高」？ | 短暂尖峰 | 进入 l2 | 无（需要看时间序列） |
| | 持续偏高 | 进入 l5 | |
| l2 尖峰时刻是否同时发生了 PD Leader 切换 / TiKV 掉 Leader？ | 是 | 结论「选举 / 掉 Leader 引发」 | 有 |
| | 否 | 结论「执行计划突变」 | |
| l5 Performance Overview 中 TiDB–TiKV 延迟差是否大于 5ms（同机房应小于 2ms）？ | 是 | 结论「网络延迟」 | 无 |
| | 否 | 进入 l6 | |
| l6 DB Time 中 Compile 占比是否很高（橙色）？ | 是 | 结论「编译开销大」 | 有 |
| | 否 | 结论「Store 侧瓶颈」 | |

### 向导问题怎么回答

向导里的问题大多要你去 Grafana 或日志核对。下表给出每个问题去哪里看、怎么判断。**判断标准里写「官方」的出自官方文档，写「平台」的是平台内置的经验阈值**，真实集群上是否合适以联调为准。

| 问题 | 去哪里看 | 怎么判断 |
|---|---|---|
| w1 Raftstore CPU 是否只有个别节点很高 | Grafana → TiKV-Details →「Thread CPU」→「Raftstore CPU」，按实例对比 | 平台：最高节点明显高于其余节点（实时证据口径：≥ 30% 且 ≥ 其余平均 2 倍算「是」，≤ 其余平均 1.5 倍算「否」）。官方：Raftstore CPU 不宜超过 `store-pool-size` × 80% |
| w2 / b1 是否出现 Stalling | TiKV 日志；Grafana → TiKV-Details → RocksDB - kv 的「Write stall duration」 | 日志里能 grep 到 `Stalling`，或面板有非零值即为「是」。官方：正常情况下 write stall 应为零 |
| w3 WAL Sync Duration 是否偏高 | Grafana → TiKV-Details → Raft Engine 的「WAL sync duration」（RocksDB 的 WAL 另有同名面板） | 平台：持续超过数毫秒算偏高 |
| w4 Apply / Propose 耗时是否偏高 | Grafana → TiKV-Details → Raft IO 的「Apply log duration」；Raft propose 的「Propose wait duration」 | 平台：与自身历史和其他节点比明显偏高。线程池偏小时同时看「Thread CPU」里「Async apply CPU」是否接近饱和 |
| b2 latch wait 是否很高 | Grafana → TiKV-Details → Scheduler 各分组的「Latch wait duration」 | 平台：明显高于平时、与写冲突告警同时出现 |
| b3 channel full / raftstore CPU 接近 100% | TiKV 日志里的 `channel full`；「Raftstore CPU」；「Errors」分组的「Server is busy」 | 日志里有 `channel full`，或 Raftstore CPU 贴近线程池上限即为「是」 |
| l1 尖峰还是持续 | Grafana 的 P99 延迟曲线或 [总览](go:overview) 的 P99 趋势 | 回落后恢复为尖峰；长期保持高位为持续 |
| l2 是否同时发生了 Leader 切换 | PD、TiKV 日志的切换记录；[总览](go:overview) 事件时间线；Grafana 的 Leader 数量曲线 | 尖峰时间点附近出现切换或 TiKV 重启即为「是」 |
| l5 TiDB–TiKV 延迟差 | Grafana → Performance Overview，比较 TiDB 侧看到的请求延迟与 TiKV 侧的处理延迟（面板名见官方 Performance Overview 说明） | 平台：差值 > 5ms 算偏高（同机房应 < 2ms） |
| l6 Compile 占比 | Grafana → Performance Overview 的「Database Time by SQL Phase」；或 [总览](go:overview) 的「数据库时间 (DB Time) 分解」卡片 | 平台实时证据口径：Compile 占比 ≥ 30% 算很高，≤ 15% 算不高。官方指出未启用执行计划缓存时 compile 阶段可达几毫秒到几十毫秒 |

### 各结论的含义与建议处置

| 结论 | 含义 | 建议处置要点 | 「查看相关页面」 |
|---|---|---|---|
| 写热点 | 写入集中在少数 Region / 节点 | 在 Key Visualizer 确认阶梯状斜线；用 AUTO_RANDOM / SHARD_ROW_ID_BITS 打散；必要时手动拆分 Region | [热点与调度](go:hotspot) |
| RocksDB Write Stall | Compaction 跟不上写入，触发写入限流 | `grep -i stalling tikv.log`；查磁盘 IOPS 与吞吐；评估调整 soft/hard-pending-compaction-bytes-limit；扩容 TiKV | 无 |
| 磁盘写入瓶颈（Append Log 阶段） | Raft 日志落盘慢 | 用 fio / iostat 查磁盘延迟；Raft Engine 与数据盘分离；换更快的 NVMe | 无 |
| 线程池不足（Raftstore / Apply） | store-pool-size 或 apply-pool-size 偏小 | store-pool-size 调到 1–5；增加 apply-pool-size；观察 Thread CPU 是否仍饱和 | 无 |
| 事务层冲突 / 锁等待 | 更可能是锁冲突或大事务 | 看 latch wait duration；查 DATA_LOCK_WAITS / DEADLOCKS；缩短事务、拆分大批量写入 | [SQL 优化](go:sql) |
| RocksDB write stall（busy 症状下） | 写入积压触发限流 | 调整 pending-compaction-bytes 限制；排查磁盘性能；降低写入压力 | 无 |
| 写冲突 / 热点行 | 相同 Key 并发写 | 降低并发；乐观事务改悲观事务；拆分热点行 | 无 |
| raftstore 繁忙 | 线程池不足或 append log 慢 | 检查 append log 耗时；调大 store-pool-size | 无 |
| Coprocessor 请求排队 | 读请求过多或计划劣化 | 定位大查询（Top SQL）；优化执行计划 / 增加索引；考虑 Follower Read 分担 | [SQL 优化](go:sql) |
| 选举 / 掉 Leader 引发 | Leader 切换期间请求需要重试 | 检查 PD 与 TiKV 日志中的切换原因；排查磁盘 I/O、网络、节点重启 | 无 |
| 执行计划突变 | 统计信息变化导致 plan_digest 改变 | 对比 EXPLAIN ANALYZE；ANALYZE TABLE；SQL Binding 固定计划 | [SQL 优化](go:sql) |
| 网络延迟 | TiDB 与 TiKV 间链路慢 | 检查跨 AZ 部署；Follower Read / closest-replicas；网络质量排查 | 无 |
| 编译开销大 | 未使用执行计划缓存 | 使用 Prepared Statement；开启 Prepared Plan Cache；避免过度 StmtClose | 无 |
| Store 侧瓶颈 | Commit Log Duration 高等 | 增加 store 线程；启用 Raft Engine；排查磁盘 / Compaction | 无 |

### 实时证据取自哪里（只读）

触发条件：必须是真实或回放数据模式（顶部有「回放数据」或「真实数据（只读）」横幅）。纯静态原型没有后端，向导与原型一致，不显示任何实时内容。

取数方式：进入向导时前端调用 `GET /api/wizard/evidence`，后端用**最近一次快照**逐条规则求值，**不会重新连接集群，也不执行任何命令**。前端最多每 15 秒取一次；取数失败会被静默忽略，向导保持无证据的原样。证据到达后，如果你仍在向导页且没有打开抽屉，页面会原地重绘，不会丢掉你已经走过的路径。

每条证据的格式：蓝色提示「实时数据建议：是 / 否 —— 依据（含具体数字）」，必要时后面带「查看 →」链接，末尾标明依据的是回放还是实时数据与采集时间。

| 问题 | 给「是」的条件 | 给「否」的条件 | 其他情况 |
|---|---|---|---|
| 写入变慢 / w1 | 诊断引擎已判定「写入热点」；或某台 TiKV 写流量不低于 20 MB/s 且不低于其余 TiKV 平均值的 3 倍；或某台的 Raftstore 线程 CPU 不低于 30% 且不低于其余平均值的 2 倍 | Raftstore 线程 CPU 各节点都低于 30%，或最高不超过其余平均值的 1.5 倍 | 差距介于两者之间时，不显示建议。没有命中前两个「是」条件时：TiKV 少于 3 台，或没有 Raftstore CPU 数据，也不给证据 |
| 写入变慢 / w2 和 TiKV server is busy / b1 | 快照里有告警名或描述含 stall | 永远不会给「否」 | 没有相关告警时不给证据（日志内容快照里没有，需要你自己 grep） |
| TiKV server is busy / b3 | 某台 TiKV 的 Raftstore 线程 CPU 不低于 90%，或有 raftstore_thread_cpu 相关告警 | 各节点最高低于 70% 且没有相关告警 | 介于 70% 与 90% 之间时不显示建议。channel full 只出现在日志里，需要你自己确认 |
| 延迟升高 / l2 | 有 Leader 切换相关告警，或有 TiKV / PD 实例已停止 / 下线 | 永远不会给「否」 | 其余情况不显示建议（快照没有切换历史） |
| 延迟升高 / l6 | DB Time 的 Compile 阶段占比不低于 30% | Compile 占比不高于 15% | 介于 15% 与 30% 之间时不显示建议 |

没有实时证据的问题：写入变慢 w3、w4；TiKV server is busy b2；延迟升高 l1、l5。这些问题的数据（WAL Sync Duration、Apply / Propose 耗时、latch wait、延迟时间序列等）快照里没有，只能你去 Grafana 看。

> 💡 访客、审计员等没有 SQL 查看权限的角色，后端会把快照里的慢 SQL 裁掉再求值，所以「当前集群事实」里「慢 SQL 走全表扫描」之类的线索对他们可能不出现。

### 当前集群事实

这张卡片只出现在「选择症状」那一屏（进入具体问题后不显示），列出与具体问题无关的实时线索，带图标：✖ 严重、⚠ 警告、✔ 正常。线索都可能带「查看 →」链接。

| 线索 | 级别 | 触发条件 | 跳转 |
|---|---|---|---|
| 有实例停止 / 下线 | 严重 | 任一实例状态为 down 或 offline | 节点管理 |
| 全部 N 个实例状态正常 | 正常 | 没有停止 / 下线实例 | 无 |
| TiKV 磁盘 ≥ 80% | 严重 | 存活 TiKV 的磁盘使用率不低于 80% | 节点管理 |
| TiDB 内存 ≥ 80% | 警告 | 任一 TiDB 内存不低于 80% | SQL 优化 |
| N 条严重告警正在触发 | 严重 | Alertmanager 中有 critical 级告警 | 智能诊断 |
| 慢 SQL 中有 N 类走全表扫描 | 警告 | 慢 SQL 分组里索引信息为「—」或含 TableFullScan | SQL 优化 |
| TiDB 当前连接数合计 N | 正常 | 连接数大于 0 | 无 |
| 诊断引擎的「疑似写入热点」标题 | 严重 | 智能诊断判定有写入热点 | 热点与调度 |

### 日志错误速查

| 日志 / 报错 | 级别 |
|---|---|
| GC life time is shorter than transaction duration | 需关注 |
| key is locked | 可自愈 |
| write conflict | 需关注 |
| stale_epoch / EpochNotMatch | 可自愈 |
| peer is not leader / NotLeader | 可自愈 |
| Region is unavailable | 需处理 |
| TiKV server is busy | 需处理 |
| information schema is changed | 需关注 |
| memory exceeds quota | 需关注 |

含义与处理建议见页面表格。级别标签含义：

| 标签 | 含义 |
|---|---|
| 可自愈（蓝） | TiDB 会自动刷新路由并重试，业务一般无感，持续大量出现才需查 |
| 需关注（黄） | 可能影响业务，需要优化业务或调整参数 |
| 需处理（红） | 需要立即排查 |

## 执行后果与风险

本页的所有内容都是**只读和建议**：

- 向导的按钮只是在决策树里跳转，「查看相关页面 →」只是跳转到平台页面，「重新选择症状」只是清空向导状态；
- 实时证据只读取已缓存的快照，不会连接集群、不会执行命令，也不会写审计；
- 命令块的「复制」只写剪贴板，页面没有任何「执行」按钮。

> ⚠️ 向导结论里的「建议处置」（如调大线程池、扩容、改 SQL Binding）以及地图条目里的命令，都需要你自己判断后在对应页面或中控机上操作。涉及重启、配置变更、扩缩容的，请走 [节点管理](go:nodes) 的变更流程（有预检、审批和审计）；直接在中控机上手工执行不会经过平台的审批和审计。

> ⚠️ 地图条目里的命令使用演示地址、集群名与版本（见上文替换表），盲目复制执行可能连不上或作用到错误的对象。`tiup cluster edit-config` + `reload` 属于滚动重启，会有短暂抖动，务必先确认影响范围。

> ⚠️ 实时证据来自「最近一次快照」，数据可能有几十秒延迟，且只覆盖少数问题；证据说「否」不等于确认没问题，说「是」也不等于已经确诊。

## 常见问题与注意

- **向导里没有蓝色的实时建议**：可能是静态原型（无后端）、取数失败、该问题没有证据规则，或证据结论为「无法判断」。这是正常的，按问题自己判断即可。
- **某个问题给了「是」，但我觉得不对**：证据只来自快照里的告警、指标和诊断结论，可能与你看到的 Grafana 面板有出入。请以你的核对为准，直接点你认为对的按钮。
- **向导里提到的面板（Raftstore CPU、WAL Sync Duration 等）在哪**：在 Grafana 的 TiKV-Details / Performance Overview，平台不内嵌这些面板。
- **走到一半想回退**：没有「上一步」，点「重新选择症状」从头开始；证据缓存 15 秒内不会重复请求。
- **向导只有 3 个症状**：这是当前的决策树范围；其他故障请用「故障排查地图」或「日志错误速查」。排障向导与知识库（C2）已经可用，即本页。
- 「日志错误速查」与地图的内容是静态知识库，不会因集群版本不同而变化，具体行为以你集群的版本文档为准。

## 已知限制与易混淆点

| 界面写的是 | 实际是 | 建议 |
|---|---|---|
| 向导问题「Raftstore 线程 CPU 接近 100%」（b3） | 实时证据用的指标是所有 raftstore 线程的 CPU 秒数之和（按单个核折算，诊断证据里写「按单线程折算」），线程池有多个线程时可以超过 100%，例如 `store-pool-size` 为 2 时理论上限约 200%；证据里的 90% 在该配置下只占线程池的一半左右 | 不要只看证据里的百分比，在 Grafana 的「Raftstore CPU」上对照线程池大小判断；官方建议不超过 `store-pool-size` × 80% |
| 向导里的 w1 / b3 等问题写「TiKV-Details 中……」，并带「实时数据建议」 | 建议只来自平台最近一次快照（最多几十秒前），不是 Grafana 的实时曲线 | 以 Grafana 的实际曲线为准 |
| 实时建议给了「否」 | 「否」只是「快照里没有异常迹象」，不是排除；w2、b1、l2 永远不会给「否」 | 把「否」当成参考，不要据此跳过核对 |
| 向导页没有「上一步」，却有路径小标签 | 路径小标签只是记录，点击不能回退 | 答错了点「重新选择症状」从头开始 |
| 地图条目示例命令 | PD 地址、集群名、版本、路径都是固定演示值，不会随你的集群变化 | 复制后按上文替换表改写，再执行 |
| 地图、日志速查「不会因版本变化」 | 内容整理自官方文档某个版本，参数名和默认值可能随版本不同 | 以你集群版本的官方文档为准 |
| 地图页的搜索 / 分类 | 离开本页再回来仍保留上次的搜索词和分类，容易误以为条目丢了 | 点「全部」并清空搜索框 |
| 「当前集群事实」里的磁盘、内存线索 | 只在「选择症状」那一屏显示，进入具体问题后不再显示 | 选症状前先看一眼，或点「重新选择症状」回到该屏 |

## 官方参考

以下链接均为 docs.pingcap.com/zh 的 `stable` 版本页面，**以你集群的版本为准**（平台支持 v7.5 / v8.1 / v8.5，把链接里的 `stable` 换成 `v7.5` 等可查对应版本）。

- [TiDB 集群问题导图](https://docs.pingcap.com/zh/tidb/stable/tidb-troubleshooting-map)：故障排查地图与日志速查的来源。
- [TiKV Grafana 面板说明](https://docs.pingcap.com/zh/tidb/stable/grafana-tikv-dashboard)：Thread CPU、Raft IO（Append / Apply log duration）、Raft Engine（WAL sync duration）、Scheduler（Latch wait duration）、RocksDB（Write stall duration）、Errors（Server is busy）等面板。
- [Performance Overview 面板详解](https://docs.pingcap.com/zh/tidb/stable/grafana-performance-overview-dashboard)：Database Time by SQL Phase（get token / parse / compile / execute）。
- [TiDB 性能分析和优化方法](https://docs.pingcap.com/zh/tidb/stable/performance-tuning-methods)：Database Time 分解与 compile 阶段影响。
- [TiKV 线程池性能调优](https://docs.pingcap.com/zh/tidb/stable/tune-tikv-thread-performance/)：Raftstore / Apply / Scheduler 线程池大小与 CPU 利用率建议。
- [TiKV 配置文件描述](https://docs.pingcap.com/zh/tidb/stable/tikv-configuration-file)：`raftstore.store-pool-size`、`raftstore.apply-pool-size`、`soft-pending-compaction-bytes-limit`、`hard-pending-compaction-bytes-limit` 等。
- [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)：写热点结论的处理。
- [乐观事务写写冲突排查](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-write-conflicts)、[锁冲突问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts)：Write Conflict、`DATA_LOCK_WAITS`、`DEADLOCKS`。
- [慢查询日志](https://docs.pingcap.com/zh/tidb/stable/identify-slow-queries)、[Top SQL](https://docs.pingcap.com/zh/tidb/stable/top-sql)：定位大查询。
- [Prepare 语句执行计划缓存](https://docs.pingcap.com/zh/tidb/stable/sql-prepared-plan-cache)、[执行计划管理（SQL Binding）](https://docs.pingcap.com/zh/tidb/stable/sql-plan-management)：「编译开销大」「执行计划突变」的处置。
- [Follower Read](https://docs.pingcap.com/zh/tidb/stable/follower-read)：Coprocessor 排队、网络延迟时的读分担。
- [TiDB 集群报警规则](https://docs.pingcap.com/zh/tidb/stable/alert-rules)：`TiKV_raftstore_thread_cpu_seconds_total` 等告警的含义。
- [TiDB 监控框架概述](https://docs.pingcap.com/zh/tidb/stable/tidb-monitoring-framework)：Prometheus、Grafana、Alertmanager 与 Dashboard 的关系。
- [TiKV Control](https://docs.pingcap.com/zh/tidb/stable/tikv-control)：地图里提到的 `tikv-ctl`（`unsafe-recover` 已被官方标注不推荐，优先用 PD 的在线 Unsafe Recovery）。

## 相关页面

- [热点与调度](go:hotspot)：写热点结论的跳转目标，见 [热点与调度帮助](help:hotspot)
- [SQL 优化](go:sql)：锁冲突、Coprocessor 排队、执行计划突变的跳转目标
- [智能诊断](go:diag)：诊断引擎结论与告警，向导的实时证据与它共用同一批规则
- [节点管理](go:nodes)：停止 / 磁盘 / 扩缩容与配置变更
- [总览](go:overview)：DB Time 分解、告警与事件时间线
- [环境与设置](go:settings)：Grafana / PD 等组件地址
- [常见问题](help:faq)
