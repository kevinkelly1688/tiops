# 智能诊断

> 💡 把采集到的拓扑、热点、慢 SQL、告警套进一组内置规则，给出带证据、可能根因、处置建议和官方文档链接的诊断结果；同时展示 DB Time（数据库时间）三种分解，帮你自上而下找瓶颈。所有角色可打开（权限项 `view.overview`），本页只读，**不会自动执行任何修复**——建议里的命令只能复制，由你自己决定是否执行。只读 / 回放模式无需登录；运维模式需先登录。

## 这个页面做什么

- 「诊断结果」：按严重度排序的诊断项列表，每项可展开看证据、可能根因、处置建议、关联页面按钮和官方文档链接。
- 「健康评分」：根据结果数量算出的一个粗略分数（经验公式；选了「仅 TiDB / 仅 TiKV·PD」时只算当前范围）。
- 「数据库时间 (DB Time) 分解」：按 SQL 类型 / 按处理阶段 / Execute 阶段细分三条堆叠条。
- 诊断项来自后端的规则引擎（真实 / 回放模式），或前端内置的 8 条演示项（静态原型）。分辨数据来源见 [集群总览](help:overview)。

## 前置条件

| 项 | 要求 | 不满足时 |
|---|---|---|
| 角色 / 能力 | 任何角色（`view.overview`）可看；本页没有任何写操作 | 运维模式需先登录 |
| 模式 | 只读 / 回放 / 运维模式均可 | — |
| 数据 | 诊断依据的是**已采集的快照**：拓扑、热点、慢 SQL、告警、Prometheus 指标 | 某类数据缺失（横幅「N 项数据缺失」）时，依赖它的规则无从判断，不会报错也不会触发；规则依赖的数据：PD 副本 / TiKV 写入链路 / 主机层规则读 Prometheus 里的 PD、TiKV、node_exporter 指标，慢日志对比读 `cluster_slow_query` 的 `Stats`、`Backoff_types` 等列，取不到的项对应规则不触发、也不会在横幅提示 |
| 复制命令后执行 | 需要你自己有对应的执行权限（TiDB SQL 账号 / tiup 中控机用户） | 集群类变更请走 [节点管理](go:nodes) 流程 |

## 先看懂几个词

| 术语 | 一句话解释 | 官方文档 |
|---|---|---|
| 诊断项 / 规则 | 平台内置的一条判断逻辑，满足条件就生成一项带证据的结果；规则写死，阈值不能在页面调整 | 依据官方 [TiDB 集群问题导图](https://docs.pingcap.com/zh/tidb/stable/tidb-troubleshooting-map) 与 [性能分析和优化方法](https://docs.pingcap.com/zh/tidb/stable/performance-tuning-methods) |
| DB Time | TiDB 处理 SQL 累计花费的时间；按 SQL 类型相加，也可按阶段（Get Token、Parse、Compile、Execute）相加 | [性能分析和优化方法](https://docs.pingcap.com/zh/tidb/stable/performance-tuning-methods) |
| TSO | PD 发出的全局时间戳，事务开始 / 提交都要取；「TSO 等待」高说明取时间戳慢 | 同上 |
| 写热点 | 写入集中到少数 Region，单个 TiKV 的 Raftstore 线程成为瓶颈 | [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues) |
| 执行计划缓存（Plan Cache） | 对 Prepare / Execute 请求缓存执行计划，减少编译（Compile）开销 | [Prepared Plan Cache](https://docs.pingcap.com/zh/tidb/stable/sql-prepared-plan-cache) |
| 统计信息 | 优化器估算行数和选计划的依据，过期会导致选错计划 | [常规统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics) |

## 怎么用（按步骤）

### 运行诊断并读懂一个诊断项

前置条件：已打开本页；若来自总览的「优先处理建议」，先看下文「使用示例」。

1. 在左侧栏「诊断与优化」分组点「智能诊断」。你会看到：上方「诊断范围」卡片、DB Time 分解卡片，右侧「健康评分」显示「—」、副标题「尚未诊断」，结果区是空的。**这是正常的**，需要点一次「开始诊断」才显示结果。
2. 点「开始诊断」。你会看到「诊断范围」卡片里出现一列 6 步进度（采集监控指标、分解 DB Time……）逐项打勾，按钮在此期间置灰；约 2–3 秒后页面出现「诊断结果」卡片，右侧健康评分变成数字。这 6 步只是动画，实际做了什么见下文「『开始诊断』实际做了什么」。「诊断范围」两个下拉框目前不起作用，不用选。
3. 看「诊断结果」标题处的「共 N 项」和健康评分。N = 0 表示此刻没有规则命中（评分 100），不代表集群一定健康，先看横幅有没有数据缺失。
4. 用「全部 / 严重 / 警告 / 提示」筛选标签缩小范围，先处理「严重」。你会看到列表只显示该严重度的项；标题的「共 N 项」仍是总数。
5. 点一项的标题行展开。你会看到四块内容：**证据**（触发规则的具体数值）、**可能根因**（推断，不是已确认的事实）、**优化 / 处置建议**（有序步骤，部分带命令）、底部按钮与「官方文档 ↗」。
6. 先看「证据」判断是否属实（对照 [集群架构](go:arch) 或 Grafana）；再读「可能根因」；最后按「处置建议」逐条评估。
7. 需要命令时，点命令框右上角的「复制」。你会看到底部弹出「已复制到剪贴板」。**平台不会执行它**，请到你们的变更流程里评估后再执行（后果见「执行后果与风险」）。
8. 点底部的关联页面按钮（如「查看热点分析 →」「去扩容 TiKV →」）。你会跳到对应页面，部分带预置参数（如节点管理的扩容页已预选 TiKV）。点「官方文档 ↗」会在新窗口打开 PingCAP 官方文档（stable 版，可能与你的集群版本有出入）。
9. 与预期不符：点了「开始诊断」仍无结果列表，见「常见问题与排查」。

### 读 DB Time 分解

DB Time 分解不用点「开始诊断」，打开页面就能看（真实 / 回放模式）。

1. 在页面中部「数据库时间 (DB Time) 分解」卡片里，从上到下是三条堆叠条：① 按 SQL 类型、② 按处理阶段、③ Execute 阶段细分。鼠标悬停在色块上可见名称和占比；占比 ≥7% 才在色块里写字，其余只在图例里。
2. 先看 ①：占比最大的 SQL 类型，去 [SQL 优化](go:sql) 看这一类的慢 SQL。
3. 再看 ②：Execute 通常占大头，属正常；对照下表判断是否异常。
4. 再看 ③：只能做相对比较；含「锁」「重试」「lock」「retry」字样的类型会自动标红，占比突出说明锁冲突或重试多。
5. 某条显示「未采集（需要 Prometheus 的 DB Time 指标）」说明没取到该指标，需要确认 Prometheus 是否正常。此提示不会进入「数据缺失」横幅。

### 使用示例：磁盘水位严重项的处理

场景：值班在 [集群总览](go:overview) 的「优先处理建议」里看到「tikv-4 磁盘使用率 86%，超过 80% 水位」，点「查看根因与处置 →」。

1. 跳到本页后结果区还是空的；点「开始诊断」，等动画结束，对应项自动展开并滚动到屏幕中间。
2. 读「证据」：`/data 使用率 86.0%（阈值 80%）`、集群 TiKV 平均 62%。其余 TiKV 都在 58%–64%，说明是单台偏高（数据倾斜），不是整体快满。
3. 读「可能根因」：数据倾斜或未及时扩容；≥80% 时副本补齐、Region 均衡受限，≥90% 有写入失败风险。
4. 读「处置建议」：1）扩容 TiKV（命令 `tiup cluster scale-out <集群名> scale-out-tikv.yml`）；2）确认 GC 正常推进（只读 SQL，复制到 MySQL 客户端查看 `mysql.tidb` 里 `tikv_gc%` 的变量）。其中 `<集群名>` 取自 [集群总览](go:overview)「集群信息」的名称；`scale-out-tikv.yml` 是你自己编写的扩容拓扑文件，写法见 [使用 TiUP 扩缩容](https://docs.pingcap.com/zh/tidb/stable/scale-tidb-using-tiup)。
5. 判断：集群整体都在 75% 以上，走扩容；只有这一台偏高，先看 [热点与调度](go:hotspot) 与 [集群架构](go:arch) 里 Region / Leader 是否失衡。
6. 点「去扩容 TiKV →」，跳转到 [节点管理](go:nodes) 的「扩容 / 新增节点」页并预选角色 TiKV——这只是打开向导，真正扩容要在那里预检、审批、确认后才会执行。

## 区域与参数说明

### 「开始诊断」实际做了什么

| 步骤 / 控件 | 实际行为 | 影响 |
|---|---|---|
| 「诊断范围」里的时间下拉（最近 1 小时 / 6 小时 / 24 小时 / 自定义） | **只有「最近 1 小时」可选**，其余灰掉并标「暂不支持」：采集窗口目前固定为 1 小时（慢查询与趋势都按最近 1 小时取） | 需要更长窗口的对比分析要等后续版本 |
| 「诊断范围」里的范围下拉（全集群 / 仅 TiDB / 仅 TiKV / PD） | **生效**：按诊断项所在层筛选结果，「共 N 项」「健康评分」也只算当前范围；告警汇总两边都显示。TiDB 层 = TiDB 资源、慢 SQL 及其对比（S1–S7）、DB Time 判读；TiKV / PD 层 = 实例状态、磁盘水位、写热点、Region 不均、容量预测、PD 与副本健康（P1–P7）、TiKV 写入链路（T1）；主机层（H1–H5）两边都显示 | 筛选只影响显示，不影响后端怎么判断 |
| 「开始诊断」 | 真实 / 回放数据模式下，**先向后端强制重新采集一次快照（跳过 10 秒的快照缓存），采集完成才显示结果**；进度条是展示用的，最后一步「生成诊断报告」要等采集真正完成才打勾。采集失败会提示错误，不显示旧结果。静态原型没有后端，仍显示内置演示数据。不执行任何命令 | 数据源自身还有缓存（默认 15 秒），所以连续点击短时间内不一定拿到更新的数据；页面仍每 30 秒自动取一次快照 |
| 进度条里前 5 步的文字 | 只是展示用文字，不对应真实的逐步执行（真实执行的是一次整体采集） | 无 |
| 「规则来源：官方『故障排查地图』『性能调优方法』」 | 说明规则依据 | 无 |
| 总览页的「一键诊断」 | 只是跳到本页 | 到这里还要点「开始诊断」（不点也会显示最近一次快照里的诊断项，点了会重新采集） |

> 💡 「开始诊断」全程只读：规则引擎只读取已采集的拓扑、热点、慢 SQL 与告警数据做判断，不连接集群执行写操作。

### 诊断结果列表

| 字段 / 元素 | 含义 | 可选值 / 默认 | 影响 / 说明 |
|---|---|---|---|
| 筛选标签 | 按严重度过滤列表 | 全部（默认）/ 严重 / 警告 / 提示 | 只改变显示 |
| 严重度标签 | 严重（红）/ 警告（黄）/ 提示（蓝） | 见下表 | 决定排序和健康评分扣分 |
| 标题 | 诊断结论一句话 | — | — |
| 范围（标题右侧小字） | 受影响的实例编号、库.用户、或 Alertmanager | — | [集群架构](go:arch) 里实例详情的「关联诊断」按这个文字是否包含实例编号来匹配 |
| 证据 | 触发规则的具体数值 | 列表 | 平台**没有「置信度」字段**：是否属实要你对照证据和监控自行判断 |
| 可能根因 | 对原因的推断文字 | — | 是推断，不是已确认的事实 |
| 优化 / 处置建议 | 有序步骤，部分带命令 | — | 命令只可复制；运行前先在测试环境评估 |
| 关联页面按钮 | 「某某 →」跳到相关页面，部分带预置参数 | — | 只跳转 |
| 官方文档 ↗ | 链接到 `docs.pingcap.com/zh/tidb/stable/<页面>` | 新窗口打开 | 指向 stable 版，可能与你的集群版本有出入 |

严重度含义与建议响应（响应时限是本文建议，平台没有定义 SLA）：

| 严重度 | 含义 | 本平台里的典型项 | 建议响应 |
|---|---|---|---|
| 严重 | 已经或即将影响可用性 / 数据安全 | 实例已停止或下线中、TiKV 磁盘 ≥80%、疑似写热点、文件系统只读、inode ≥ 90% | 立即处理，通知负责人 |
| 警告 | 性能或均衡问题，暂未故障 | TiDB 资源偏高、Region 分布不均、慢 SQL（缺索引、执行计划突变等）、PD 副本异常、TiKV 写入链路偏慢、主机磁盘延迟高 | 本班次内排查，必要时排期处理 |
| 提示 | 供参考、需关注 | 大内存查询、存在严重告警的汇总项、TiKV 繁忙退避、处于「观察中」的项 | 记录并观察趋势 |

### 内置规则：什么条件会触发，看到后怎么办（真实 / 回放模式）

规则写死在后端（路线图里称 C1-lite），阈值不能在页面上配置。每次快照重新计算，结果按严重 → 警告 → 提示排序。**以下阈值都是平台自定的经验值**，不是 TiDB 官方标准。

| 编号 | 诊断项 | 严重度 | 触发条件 | 先核实什么 | 下一步 |
|---|---|---|---|---|---|
| R1 | 「实例 状态异常（已停止 / 失联 或 下线中）」 | 严重 | 任一类型实例已停止（tiup 状态不以 Up 开头，或 PD 里为 Down / Disconnected / Tombstone）或下线中 | 是不是计划内维护 | 点「查看节点」到 [节点管理](go:nodes) 的「节点实例」（已预填实例）；中控机运行 `tiup cluster display`；TiKV 失联超过 PD 的 `max-store-down-time`（默认 30 分钟）会开始补副本，见 [PD 配置文件](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file) |
| R2 | 「TiKV 磁盘使用率 x%，超过 80% 水位」 | 严重 | 未停止 / 下线的 TiKV，（PD 容量 − 可用）/ 容量 ≥ 80% | 其余 TiKV 是否也高（证据里有集群平均） | 整体高：「去扩容 TiKV」；只有一台高：看 [热点与调度](go:hotspot)。80% 与 PD 的 `low-space-ratio` 默认值一致，超过后 PD 会尽量避免往该节点迁移数据 |
| R3 | 「TiKV 疑似写入热点」 | 严重 | 至少 3 台可用 TiKV；该台写流量 ≥ 20 MB/s 且 ≥ 其余 TiKV 平均值的 3 倍 | 证据里的写流量倍数、Raftstore CPU、热点 Region 所属表 | 「查看热点分析」到 [热点与调度](go:hotspot)；官方处理方法见 [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)（AUTO_RANDOM、SHARD_ROW_ID_BITS）。扩容不能缓解单 Region 热点 |
| R4 | 「TiDB 资源使用偏高」 | 警告 | TiDB 所在主机内存 ≥ 80% 或 CPU ≥ 85% | 内存、CPU、QPS、连接数；是否个别大查询 | 「查看 Top SQL」到 [SQL 优化](go:sql) 找大内存 SQL；建议里的 `tidb_mem_quota_query` 属系统变量，见 [系统变量](https://docs.pingcap.com/zh/tidb/stable/system-variables) |
| R5 | 「TiKV 之间 Region 分布不均」 | 警告 | 至少 3 台可用 TiKV；Region 数最多 / 最少 > 1.25（最少 > 0） | 是否刚扩缩容、有无高水位 Store | 「查看拓扑」到 [集群架构](go:arch)；调度机制见 [TiDB 数据库的调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling)，命令见 [PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control) |
| R6 | 「慢 SQL（标签）：SQL 前 90 字符」 | 警告；标签为「大查询占用内存高」「TiKV 排队等待」时为提示 | 慢 SQL 的问题标签为「缺索引 / 统计信息过期」「TiKV 排队等待」「大查询占用内存高」「索引列上使用函数」之一；按累计耗时排序，最多 4 条 | 平均 / P99 / 累计耗时、执行次数、扫描与处理 Key、返回行数 | 「查看该 SQL」到 [SQL 优化](go:sql)，按那里的步骤读执行计划；官方概览见 [SQL 性能调优](https://docs.pingcap.com/zh/tidb/stable/sql-tuning-overview) |
| R7 | 「N 条严重告警正在触发」 | 提示 | Alertmanager 里有 ≥1 条 critical 告警 | 证据里的告警名（实例）列表 | 「查看总览」回 [集群总览](go:overview)；到 Alertmanager / Grafana 确认详情，官方告警项见 [报警规则](https://docs.pingcap.com/zh/tidb/stable/alert-rules) |
| D1 | 「SQL 编译（Compile）占数据库时间 x%：执行计划缓存可能没有生效」 | 警告 | QPS ≥ 10，且 DB Time「按处理阶段」里 Compile ≥ 20% | 证据里各阶段占比；Grafana「Queries Using Plan Cache OPS」的 avg-hit 是否长期为 0；`statements_summary` 的 `PLAN_IN_CACHE` / `PLAN_CACHE_HITS` | 应用改用 Prepared Statement 并复用；每次 Execute 后都 Close 时，v6.0.0 起可设 `tidb_ignore_prepared_cache_close_stmt = ON`；官方 [性能调优方法](https://docs.pingcap.com/zh/tidb/stable/performance-tuning-methods) |
| D2 | 「获取令牌（Get Token）占数据库时间 x%：并发请求在 TiDB 内排队」 | 警告 | QPS ≥ 10，且 Get Token ≥ 5%（正常接近 0） | 各 TiDB 的 `performance.token-limit`（建议里有查询语句）；是否被慢 SQL 占满并发 | 先处理慢 SQL；连接数过高用连接池限制，或扩容 TiDB |
| D3 | 「TSO 等待占 Execute 阶段 x%：获取时间戳偏慢」 | 警告 | QPS ≥ 10，且 Execute 细分里「TSO 等待」≥ 30%（细分各项有交叠，只是相对比较） | Grafana「PD TSO Wait Duration」；TiDB 到 PD 的网络延迟（`ping`）、TiDB 与 PD 的负载 | 官方日常巡检：偏高时排查 TiDB 到 PD 的网络延迟、TiDB 负载、PD 负载 |
| D4 | 「悲观锁请求占 Execute 阶段 x%：疑似存在锁冲突 / 热点行」 | 警告 | QPS ≥ 10，且 Execute 细分里「KV PessimisticLock」≥ 30% | `DATA_LOCK_WAITS`、`DEADLOCKS`、`CLUSTER_TIDB_TRX` 里处于 LockWaiting 的事务（只覆盖悲观锁、只反映当前时刻） | 从业务侧降低同一行的并发更新；官方 [锁冲突排查](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts) |
| F1 | 「按近期增长，约 N 天后 TiKV 容量到 80% 水位」 | 严重（≤ 7 天）/ 警告（≤ 30 天） | 容量预测样本足够（≥ 24 小时），当前使用率还不到 80%，且按近 7 天线性回归推算 ≤ 30 天到 80% | 证据里的增长速度、到 80% / 95% 的天数；近期有没有大批量导入或清理（会让预测失准） | 提前规划扩容 TiKV（留出 PD 均衡 Region 的时间）；核对可清理的数据（TTL）与 GC 是否正常 |
| S1 | 「执行计划发生变化：SQL 前 90 字符」 | 警告（变慢 ≥ 1.5 倍）/ 提示 | 最近 2 小时的慢日志按 Digest + Plan_digest 聚合：上一小时（再早的 1 小时）有样本，而最近 1 小时出现了上一小时没有的执行计划 | 证据里的新 / 旧计划摘要、两个窗口的平均耗时；注意比较的是**慢日志样本**，上一小时没有记录只说明那时没有超过慢日志阈值 | 建议里有「查看该 SQL 近期用过的计划」和「按旧计划摘要绑定」（`CREATE GLOBAL BINDING FROM HISTORY USING PLAN DIGEST`，v6.6.0 起支持，绑定前先 `EXPLAIN ANALYZE` 验证旧计划更好）；官方 [执行计划管理](https://docs.pingcap.com/zh/tidb/stable/sql-plan-management) |
| S2 | 「同一计划下耗时明显上升：SQL」 | 警告 | 执行计划没变，两个窗口各 ≥ 5 条慢日志，最近 1 小时平均 ≥ 0.5 秒且 ≥ 上一小时的 2 倍 | 同时段数据量是否增长、有没有其它诊断项（热点、锁冲突、TiKV 繁忙） | 用 `EXPLAIN ANALYZE` 对比 estRows 与 actRows，看慢在哪一步；官方 [慢查询日志](https://docs.pingcap.com/zh/tidb/stable/identify-slow-queries) |
| S3 | 「统计信息缺失（pseudo）：SQL」 | 警告 | 最近 1 小时该 Digest 的慢日志里，≥ 50% 的 `Stats` 含 `pseudo` | 表是不是新建 / 刚大批量导入、自动 ANALYZE 是否开着 | `SHOW STATS_HEALTHY` 看健康度，再 `ANALYZE TABLE`；官方 [统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics) |
| S4 | 「慢日志显示锁冲突：SQL」 | 警告 | 最近 1 小时该 Digest 的慢日志里，≥ 30% 的 `Backoff_types` 含 `txnLock` | 证据里的 Resolve_lock_time、重试次数；`DATA_LOCK_WAITS` / `DEADLOCKS`（只覆盖悲观锁，只反映当前时刻） | 从业务侧降低同一行的并发更新、缩短事务；官方 [锁冲突排查](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts)。这是比 Wait_time 更可靠的锁冲突证据 |
| S5 | 「TiKV 繁忙导致退避：SQL」 | 提示 | ≥ 30% 的慢日志 `Backoff_types` 含 `tikvServerBusy` | 症状而非根因：对照下面 T1（TiKV 写入链路）和磁盘延迟 | 看 TiKV 的 CPU / 线程池 / 磁盘，再决定限流还是扩容 |
| S6 | 「Region 信息过期导致重试：SQL」 | 提示 | ≥ 50% 的慢日志 `Backoff_types` 含 `regionMiss`，且没有 `pdRPC` | 是否正在扩缩容 / 迁移，Region 调度是否频繁 | 对照 PD 调度监控（`operator show`）；官方 [调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling) |
| S7 | 「扫描的 Key 远多于处理的 Key：SQL」 | 提示 | 平均 Total_keys ≥ 10 万，且 Total_keys ÷ Process_keys ≥ 10，并且没被判定为「缺索引」 | 证据里的比例、单次最多跳过的 tombstone 数（`Rocksdb_delete_skipped_count`） | 官方：两者相差大说明 MVCC 旧版本多；确认 GC safe point 在推进、没有长事务卡住 GC（建议里有查询语句） |
| P1 | 「N 个 Region 缺少副本」 | 警告 | PD 指标 `miss-peer-region-count` > 100，持续 5 分钟 | 是否有 Store 刚下线 / 宕机、剩余 Store 水位是否 ≥ 80% | `pd-ctl region check miss-peer`；官方 [报警规则](https://docs.pingcap.com/zh/tidb/stable/alert-rules) |
| P2 | 「N 个 Region 有副本失联（down-peer）」 | 警告 | `down-peer-region-count` > 0，持续 5 分钟 | 失联副本所在的 Store | `pd-ctl region check down-peer`；PD 在超过 `max-store-down-time` 后补副本 |
| P3 | 「至少 N 个 Region 的副本落后，已持续 30 分钟以上」 | 警告 | `pending-peer-region-count` 在**最近 30 分钟内的最小值** > 100（官方：持续超过 30 分钟才算异常，所以这一项自带持续判定，不再叠加「观察中」） | 新加入的 TiKV 是否在同步、磁盘 I/O 与网络 | `pd-ctl region check pending-peer` |
| P4 | 「PD TSO 请求 P99 x，偏高」 | 警告 | TSO 请求 P99 > 0.1 秒，持续 5 分钟 | PD leader 的负载、TiDB 到 PD 的网络 | 与上面的 D3 对照看 |
| P5 | 「pd-N 的 etcd WAL fsync P99 x，磁盘写入慢」 | 警告 | etcd WAL fsync P99 > 1 秒，持续 5 分钟 | PD 是否与 TiKV 等混部在同一块盘 | `iostat -x`；PD 应使用独立的 SSD |
| P6 | 「pd-N 与其它 PD 节点的往返延迟 P99 x」 | 警告 | etcd 节点间往返延迟 P99 > 1 秒，持续 5 分钟 | PD 节点之间的网络（跨机房尤其注意） | `ping` 其它 PD |
| P7 | 「tikv-N 被 PD 判定为慢节点（慢分数 x）」 | 警告 | PD 指标 `store_slow_score` > 80，持续 5 分钟 | 该机器磁盘健康与延迟 | `iostat -x`、`dmesg`；`pd-ctl scheduler show` 看是否已触发慢节点驱逐 |
| T1 | 「tikv-N 写入链路偏慢（段落列表）」 | 警告 | 每台 TiKV 一条，下列任一段超阈值，持续 5 分钟：scheduler latch 等待 P99 > 1 秒；append log P99 > 1 秒（正常 < 50 ms）；apply log P99 > 1 秒（正常 < 100 ms）；Raftstore 线程 CPU ≥ 线程池大小的 80%；Apply 线程 CPU ≥ 线程池大小的 80%；近 10 分钟 write stall 增量 > 0 | 证据里逐段列出数值；线程池大小取自 `cluster_config`（取不到按默认值 2） | 官方把「写入慢」拆成分段逐段定位，往往最靠前的一段就是根因；见 [磁盘 I/O 过高的处理办法](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-high-disk-io) |
| H1 | 「N 台主机的文件系统变成只读」 | 严重 | `node_filesystem_readonly` = 1（排除虚拟文件系统和光盘镜像） | 证据里的主机与挂载点；如果是数据盘，上面的实例已经或即将异常 | `dmesg -T` 查磁盘 / 文件系统错误 |
| H2 | 「N 台主机的 inode 使用率 ≥ 80%」 | 警告；≥ 90% 为严重 | 该主机最高的文件系统 inode 使用率（排除虚拟文件系统） | 找出文件数最多的目录（日志、临时文件、备份小文件） | `df -i` |
| H3 | 「N 台主机的磁盘延迟偏高」 | 警告 | 该主机最慢的一块盘：读延迟 > 32 ms 或写延迟 > 16 ms（官方告警阈值），读 / 写 IOPS > 5，持续 5 分钟 | 证据里区分读 / 写；对照 T1、P5 | `iostat -x`、`iotop` |
| H4 | 「N 台主机的系统负载超过 CPU 核数」 | 警告 | 5 分钟平均负载 ÷ CPU 核数 > 1，持续 5 分钟 | 是 CPU 排队还是磁盘 I/O 等待；混部的其它进程 | `top` |
| H5 | 「N 台主机的 TCP 连接数 > 50000」 | 警告 | `node_netstat_Tcp_CurrEstab` > 50000，持续 5 分钟 | TiDB 所在机器多为应用连接过多；其它角色注意连接泄漏 | `ss -tan state established` 按对端统计 |

要点与局限：

- 以上就是真实 / 回放模式下全部的诊断规则。计划缓存命中率的直接指标、全库统计信息健康度（S3 只看慢日志里出现 pseudo 的语句）、PD leader 切换、跨 AZ 网络延迟、Region 数量过多等**目前没有对应的规则项**（D1 只能从 DB Time 的 Compile 占比间接推测计划缓存）；右侧「诊断覆盖项」列表已按真实规则改写，并单独标出暂未覆盖的几项。
- 没有触发项不等于集群健康：数据缺失会让相关规则无从判断。例如没有 node_exporter 时 CPU / 内存为 0，R4 永远不会触发。
- 状态与资源阈值分两套：矩阵 / 拓扑把磁盘 ≥85% 标成「告警」，而 R2 在 ≥80% 就报严重，所以一台 82% 的 TiKV 在总览里仍是「运行中」，这里却是严重项。
- 慢 SQL 项的「可能根因」文字是固定的「详见建议…」，具体原因看证据和建议。
- **「持续一段时间才按原级别提示」**：官方文档多处强调「较小的非零值通常正常」，瞬时抖动不该直接报严重。所以 R3（写热点）要持续 3 分钟、R4（TiDB 资源）5 分钟、R5（Region 不均）30 分钟、D1–D4（DB Time 判读）5 分钟、P1/P2/P4–P7（PD 与副本）5 分钟、T1（TiKV 写入链路）5 分钟、H3–H5（磁盘延迟、负载、TCP）5 分钟；持续时间不够时该项**先按「提示」级别显示**，标题带「（观察中：已持续 x / 需 n 分钟）」，满了再恢复原级别，并在证据里写明「已持续 x（首次发现于 hh:mm:ss）」。实例停止（R1）、磁盘水位（R2）、容量预测（F1）、慢 SQL（R6）、慢日志对比（S1–S7，本身已经是 1 小时窗口内的统计）、P3（自带 30 分钟判定）、H1/H2（文件系统只读、inode）、告警汇总（R7）不做这个判定。
- 持续时间只在**有人打开页面（或平台后台定时采集）时**才累计：快照中断超过 3 分钟、平台重启，或这一项中间消失过一次，都会重新计时；回放模式（静态数据，时间不会推进）不做持续判定。

静态原型（打开 index.html 无后端）显示的是内置的 8 条演示项（写热点、磁盘水位、Compile 占比高、跨 AZ 延迟、悲观锁等待、统计信息健康度、TSO 等待、expensive_query），数字都是假的，不要据此做决定。

### 健康评分

| 项 | 含义 | 口径 |
|---|---|---|
| 分数 | 100 − 严重数 × 15 − 警告数 × 6 − 提示数 × 2，最低 0 | 页面注明「经验公式」，仅作粗略参考；选了范围时只算当前范围 |
| 颜色 | 小于 60 红，小于 80 黄，其余绿 | 例如 1 严重 + 2 警告 = 73，黄色 |
| 副标题 | 「n 个严重 · n 个警告 · n 个提示」 | 点「开始诊断」之前显示「尚未诊断」 |

### 数据库时间 (DB Time) 分解

三条堆叠条，每条各项占比之和为 100%。取点：最近 1 小时、步长 60 秒，每项取窗口内各点的平均，再换算成占比；是**相对占比**，不是绝对耗时。官方对 DB Time 的定义与分解见 [性能分析和优化方法](https://docs.pingcap.com/zh/tidb/stable/performance-tuning-methods)，对应的 Grafana 面板见 [Performance Overview 面板](https://docs.pingcap.com/zh/tidb/stable/grafana-performance-overview-dashboard)。

| 分解 | 内容 | 口径（PromQL 指标） | 看数要点与下一步 |
|---|---|---|---|
| ① 按 SQL 类型 | select / insert / update / commit 等类型，取前 6 名 + 「其他」 | `tidb_server_handle_query_duration_seconds_sum` 按 `sql_type`，不含内部 SQL | 占比最大的类型就是优先优化的对象，到 [SQL 优化](go:sql) 查这一类 |
| ② 按处理阶段 | Get Token / Parse / Compile / Execute | 各自 `*_duration_seconds_sum` 的 rate；Get Token 原始单位是微秒，已换算；没有 Execute 数据则整条不显示 | Execute 通常占大头，属正常。Compile 占比高 → 多半没有命中执行计划缓存（先对照系统变量 `tidb_enable_prepared_plan_cache`，见 [Prepared Plan Cache](https://docs.pingcap.com/zh/tidb/stable/sql-prepared-plan-cache)）；Get Token 占比高 → 并发令牌不够（TiDB 配置 `performance.token-limit`，见 [TiDB 配置文件](https://docs.pingcap.com/zh/tidb/stable/tidb-configuration-file)）；Parse 占比高 → 大量复杂 / 超长 SQL |
| ③ Execute 阶段细分 | TSO 等待、DistSQL 请求、各类 KV 请求（取前 4 名 + 「KV 其他」） | `pd_client_cmd_handle_cmds_duration_seconds_sum{type="wait"}`、`tidb_distsql_handle_query_duration_seconds_sum`、`tidb_tikvclient_request_seconds_sum` 按 type | 各项互相有交叠（DistSQL 包含其中的 Cop 请求），**只能做相对比较**，页面也会提示。TSO 等待占比明显 → 查 PD / TSO 与 TiDB 到 PD 的网络；红色（含锁 / 重试字样）占比突出 → 锁冲突，官方排查见 [锁冲突问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts) |

页面自带的「判读规则」文字：红色（悲观锁）突出 → 锁冲突；Compile 占比高 → 缺少计划缓存；TSO 等待明显 → PD / TSO 压力；TiDB 到 TiKV 的延迟差同机房应小于 2ms，跨 AZ 应小于 5ms。注意：这段文字按静态原型的数据编写；真实 / 回放模式下「按 SQL 类型」里没有「悲观锁」这一项（锁相关耗时体现在 ③ 的 KV 请求类型里），平台也没有采集 TiDB 到 TiKV 的延迟差；2ms / 5ms 是页面自带的经验数字，我们没有在官方文档里核实到出处。

## 执行后果与风险

> ⚠️ 本页不会执行任何命令、不会修改集群、不会自动修复。「开始诊断」只读取并展示已采集的数据；运维模式也一样（没有诊断相关的写操作）。

> ⚠️ 建议里的命令（`ALTER TABLE`、`ANALYZE TABLE`、`SET GLOBAL`、`tiup cluster scale-out`、`pd-ctl operator add split-region`、`store weight` 等）只是文本。复制后在别处执行才会产生后果：`scale-out`、`split-region` 会触发数据迁移与调度，`SET GLOBAL` 影响全局并发会话，`ALTER TABLE` 在大表上会占用资源。执行前请评估并在变更窗口内进行；集群类变更请走 [节点管理](go:nodes) 的流程。

> ⚠️ 慢 SQL 诊断项的标题带 SQL 文本的前 90 个字符（可能含真实参数值）。运维模式下，没有 `view.sql` 权限的角色在 [SQL 优化](go:sql) 页看不到慢 SQL，但这里的慢 SQL 诊断项目前不受该权限裁剪（按代码核对，疑似问题），请留意信息暴露范围。

## 已知限制与易混淆点

| 界面写的 | 实际是 | 怎么办 |
|---|---|---|
| 「诊断范围」时间下拉 | 只有「最近 1 小时」可用，其余灰掉 | 采集窗口固定为 1 小时 |
| 「开始诊断」+ 6 步进度 | 会重新采集一次快照（跳过 10 秒快照缓存）；进度条本身是展示用的 | 数据源还有默认 15 秒的缓存，连点不一定更新 |
| 右侧「诊断覆盖项」 | 静态文案，已按真实规则改写；列了「暂未覆盖」的几项 | 以上文规则表为准 |
| 「健康评分」 | 经验公式，仅按条数扣分，不反映严重程度细节；处于「观察中」的项按提示（−2）计 | 以具体诊断项为准 |
| 磁盘 80% 与 85% | 本页 R2 在 ≥80% 报严重，总览 / 架构图在 ≥85% 才标「告警」 | 两套阈值，以更严的 80% 为准处理 |
| 诊断项「证据」 | 没有「置信度」字段 | 自己对照监控判断 |
| 「判读规则」里的「红色（悲观锁）」「2ms / 5ms」 | 按静态原型编写；真实 / 回放模式没有「悲观锁」项、没有采集 TiDB 到 TiKV 延迟 | 锁看 ③ 中红色 KV 类型；延迟看 Grafana |
| 官方文档 ↗ | 指向 stable 版 | 对照你集群的版本 |
| 每 30 秒自动刷新 | 按代码，自动重绘后页面回到顶部，已展开的诊断项会被收起（筛选标签保留）（以真实浏览器表现为准） | 读证据前先复制要用的命令，或读完一项再展开下一项 |

## 常见问题与排查

- **点了「开始诊断」没有任何结果列表**：该时刻规则没有命中任何项，评分为 100；先确认横幅没有数据缺失。
- **结果和我点「立即刷新」后不一样**：结果随快照更新，页面每 30 秒自动重绘一次。
- **从总览跳来看不到展开项**：如果这次打开平台后还没点过「开始诊断」，先点一次，结果生成后才会自动展开并滚动到目标项；点过之后再跳转则直接展开。
- **想知道某条 SQL 的细节**：点关联按钮「查看该 SQL」。注意它只打开 [SQL 优化](go:sql) 列表，不会自动定位，需按 Digest 或 SQL 文本在列表里找，再点开详情抽屉。
- **想调整规则阈值**：目前只能改后端代码，页面不支持。

## 官方参考

以下链接均已打开核实，版本以你集群的为准（链接指向 stable 版）。

- [TiDB 集群问题导图](https://docs.pingcap.com/zh/tidb/stable/tidb-troubleshooting-map)：服务不可用、延迟升高、TiDB / TiKV / PD 问题的排查思路
- [性能分析和优化方法](https://docs.pingcap.com/zh/tidb/stable/performance-tuning-methods)：DB Time 按 SQL 类型 / 阶段分解、TSO 等待、Plan Cache
- [Performance Overview 面板](https://docs.pingcap.com/zh/tidb/stable/grafana-performance-overview-dashboard)：Database Time by SQL Type / SQL Phase
- [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)：AUTO_RANDOM、SHARD_ROW_ID_BITS
- [PD 配置文件](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file)：`low-space-ratio`（默认 0.8）、`max-store-down-time`（默认 30m）
- [TiDB 数据库的调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling)：PD 调度原理
- [PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control)：`store`、`config show`、`hot` 等命令
- [Prepared Plan Cache](https://docs.pingcap.com/zh/tidb/stable/sql-prepared-plan-cache)：`tidb_enable_prepared_plan_cache`，仅针对 Prepare / Execute 请求
- [TiDB 配置文件](https://docs.pingcap.com/zh/tidb/stable/tidb-configuration-file)：`token-limit`（并发执行请求的 session 个数，默认 1000）
- [系统变量](https://docs.pingcap.com/zh/tidb/stable/system-variables)：TiDB 系统变量总览（页面存在，具体变量条目请在页内搜索）
- [锁冲突问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts)：悲观 / 乐观锁冲突、DATA_LOCK_WAITS、DEADLOCKS
- [SQL 性能调优](https://docs.pingcap.com/zh/tidb/stable/sql-tuning-overview)：SQL 调优总览
- [统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics)：统计信息、健康度、伪统计信息
- [TiDB 集群报警规则](https://docs.pingcap.com/zh/tidb/stable/alert-rules)：官方告警项（含 `TiKV_space_used_more_than_80%`）
- [使用 TiUP 扩缩容](https://docs.pingcap.com/zh/tidb/stable/scale-tidb-using-tiup)：scale-out / scale-in

## 相关页面

- [集群总览](go:overview)：KPI、告警、「优先处理建议」入口
- [集群架构](go:arch)：节点详情里的「关联诊断」
- [SQL 优化](go:sql)：慢 SQL 与执行计划
- [热点与调度](go:hotspot)：写 / 读热点
- [故障排查](go:trouble)：官方故障排查地图与排障向导
- [节点管理](go:nodes)：扩容、缩容、配置变更（需审批）
- 帮助：[常见问题与提示](help:faq)
