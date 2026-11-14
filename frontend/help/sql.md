# SQL 优化

> 💡 找出最耗时的 SQL（按 Digest 聚合的慢查询 Top 列表），点开看真实执行计划和优化建议；另有「SQL 体检」（粘贴 SQL 做静态规则检查）和「调优流程」速查。本页只读：**平台不会替你执行任何 SQL、DDL 或 EXPLAIN**，建议里的语句只能复制。只读 / 回放模式无需登录；运维模式需先登录，且需要 `view.sql` 权限才能看到慢 SQL（见「前置条件」）。

## 这个页面做什么

- 「Top SQL / 慢查询」：按累计耗时从高到低列出慢 SQL 分组，点击一行打开「SQL 详情」抽屉，里面有执行计划表、红色标记的估算偏差行和优化建议。
- 「SQL 体检」：在浏览器里用一组正则规则检查你粘贴的 SQL（不上传、不连集群）。
- 「调优流程」：SQL 调优四步流程和两段常用命令（可复制）。

数据来源三种（静态原型 / 回放 / 真实只读采集）及其分辨方法见 [集群总览](help:overview)。

## 前置条件

| 项 | 要求 | 不满足时 |
|---|---|---|
| 角色 / 能力 | 只读 / 回放模式无限制。运维模式需要 `view.sql`：sre / dba / approver / admin 可看全部；developer 仅限被授权的库；viewer、auditor 没有 | 没有权限时列表为空，页面不会提示原因 |
| 后端配置（真实模式） | 后端要配置只读 SQL 账号（启动参数 `--mysql-host` / `--mysql-user`，密码走环境变量），账号能读 `information_schema` | 否则横幅提示「慢查询: ... 未配置只读 SQL 连接」，列表为空 |
| 集群侧 | 慢日志已开启，且有语句超过慢日志阈值（`tidb_slow_log_threshold`，官方默认 300 ms） | 没有慢 SQL 时列表为空 |
| 复制命令后执行 | 你自己的 TiDB SQL 账号需要有执行该语句的相应权限 | 由你们的变更流程决定 |

## 先看懂几个词

| 术语 | 一句话解释 | 官方文档 |
|---|---|---|
| 慢查询 / 慢日志 | 执行时间超过阈值（默认 300 ms）的语句会被记入慢日志，可通过 `SLOW_QUERY` / `CLUSTER_SLOW_QUERY` 表查询 | [慢查询日志](https://docs.pingcap.com/zh/tidb/stable/identify-slow-queries)、[SLOW_QUERY 表](https://docs.pingcap.com/zh/tidb/stable/information-schema-slow-query) |
| Digest | 把 SQL 归一化（忽略常量、空白、大小写）后算出的指纹，同一类语句 Digest 相同 | [Statement Summary 表](https://docs.pingcap.com/zh/tidb/stable/statement-summary-tables) |
| Statement Summary（语句摘要） | 按 Digest 聚合的执行统计（次数、延迟等），按固定窗口滚动，默认窗口 30 分钟 | 同上 |
| Total_keys / Process_keys | Coprocessor 扫过的 Key 数 / 处理的 Key 数；后者不含 MVCC 旧版本，两者差很大说明旧版本多 | [慢查询日志](https://docs.pingcap.com/zh/tidb/stable/identify-slow-queries) |
| Wait_time | 语句在 TiKV 的等待时间之和（Coprocessor 线程有限，忙时请求要排队），不等于行锁等待 | 同上 |
| 执行计划 / estRows / actRows | 优化器给出的执行步骤；estRows 是估算行数，actRows 是实际行数 | [执行计划概览](https://docs.pingcap.com/zh/tidb/stable/explain-overview)、[EXPLAIN ANALYZE](https://docs.pingcap.com/zh/tidb/stable/sql-statement-explain-analyze) |
| task（root / cop[tikv] / TiFlash） | 算子在哪里执行：TiDB 本身 / 下推到 TiKV 的 Coprocessor / TiFlash | [执行计划概览](https://docs.pingcap.com/zh/tidb/stable/explain-overview) |
| 统计信息 / pseudo | 优化器估算行数的依据；`stats:pseudo` 表示用的是没有真实统计的「伪统计信息」 | [常规统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics) |
| SQL Binding | 不改 SQL 就固定它使用的执行计划 | [执行计划管理（SPM）](https://docs.pingcap.com/zh/tidb/stable/sql-plan-management) |
| Plan Replayer | 导出某条 SQL 的表结构、统计信息和执行计划等现场（不含表数据），便于复现 | [PLAN REPLAYER](https://docs.pingcap.com/zh/tidb/stable/sql-plan-replayer) |

## 怎么用（按步骤）

### 找出最值得优化的 SQL，并读懂一条

1. 在左侧栏「诊断与优化」分组点「SQL 优化」。你会看到三个标签「Top SQL / 慢查询」（默认选中）「SQL 体检」「调优流程」，下面是「累计耗时 Top SQL」表。
2. 看表头：#、SQL（Digest）、执行次数、平均耗时、P99、Total_keys、Process_keys、问题标签。排名按**累计耗时**，也就是「平均耗时 × 执行次数」（该列不直接显示），排在前面收益最大。
3. 判断优先级：
  - 「执行次数」大、「平均耗时」中等：往往比偶发超慢的更值得优化。
  - 「P99」远大于「平均耗时」：长尾，偶发的锁等待、TiKV 抖动或计划抖动。
  - 「Total_keys」远大于「Process_keys」：旧版本多（GC 不及时、频繁更新删除），见 [GC 机制简介](https://docs.pingcap.com/zh/tidb/stable/garbage-collection-overview)。
  - 「Process_keys」远大于返回行：读了很多最终被过滤掉的数据，通常缺索引或统计信息不准。
4. 看「问题标签」（橙色）：平台根据统计特征和 SQL 文本给出的初步判断，只是启发式规则的结果，不是结论。各标签的触发条件和下一步见下文「问题标签与触发条件」。
5. 点击要看的那一行。你会看到右侧滑出抽屉「SQL 详情 · Digest」，依次是：SQL 文本、平均耗时 / P99、执行次数、最大内存、一行汇总、「执行计划（EXPLAIN ANALYZE）」表、「优化建议」。
6. 读执行计划：红色行是估算与实际行数偏差 ≥10 倍的算子；表下方可能出现红色提示「estRows 与 actRows 偏差 ≥ 10×，通常意味着统计信息不准，优化器可能选错计划」。读法见「执行计划表与判定规则」。
7. 读「优化建议」。需要命令时点命令框的「复制」，你会看到弹出「已复制到剪贴板」。**平台不会执行它**：到 MySQL 客户端在合适的时间、按你们的变更流程自行执行。复制前先核对表名（见「已知限制与易混淆点」）。
8. 想留存现场给同事或官方支持，点抽屉底部「复制 Plan Replayer 命令」，到 TiDB 里自行执行（后果见「执行后果与风险」）。
9. 关闭抽屉：点「关闭 ✕」或按 Esc。
10. 与预期不符：
  - 表是空的：见「常见问题与排查」。
  - 详情里没有执行计划，而是蓝色提示「慢日志里没有这条 SQL 的执行计划」：这条 SQL 的 Plan 列为空，或不在最近 1 小时最慢的前 100 条里，可手动 `EXPLAIN ANALYZE` 或用 Plan Replayer。

### 使用示例：处理一条全表扫描的慢 SQL

场景：晨检后，Top 列表第 1 行是 `SELECT * FROM orders WHERE user_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?`（静态原型自带的演示数据，真实环境数字会不同）。

1. 列表里：执行次数 `1.8 万`，平均耗时 `2.84s`，P99 `6.1s`，Total_keys `420 万`，Process_keys `12 万`，标签「统计信息过期 + 缺联合索引」。累计耗时约 5.1 万秒，是最大的一块。
2. 点该行打开抽屉：返回行 20，索引「—（TableFullScan）」。返回 20 行却扫了 420 万个 Key，比例远超 1000 倍，典型的缺索引。
3. 执行计划里 `Selection_15`、`TableReader_16` 两行是红色：estRows 只有 12，actRows 却有 38 万，偏差数万倍；最底下的 `TableFullScan_14` 的访问信息里有 `stats:pseudo`，说明这张表没有可用的统计信息。（官方的 [EXPLAIN 走查](https://docs.pingcap.com/zh/tidb/stable/explain-walkthrough) 用类似的例子演示了 TableFullScan、`stats:pseudo` 与 ANALYZE、加索引前后的对比。）
4. 优化建议依次是：先 `ANALYZE TABLE orders;`（更新统计信息），再建联合索引 `ALTER TABLE orders ADD INDEX idx_user_status_created (user_id, status, created_at);`，并避免 `SELECT *`。
5. 动手顺序（建议）：先更新统计信息，看计划是否已变；仍是全表扫描再建索引。建索引属于在线 DDL，会占用 TiKV 资源，放在低峰期，按 [变更排期](go:schedule) 或你们团队的变更流程操作。
6. 平台不会执行以上任何一条：点「复制」只是把语句放进剪贴板。

### 用「SQL 体检」检查自己写的 SQL

前置条件：无（纯浏览器内运行，任何角色可用）。

1. 点「SQL 体检」标签。你会看到左边「粘贴 SQL 做静态体检」文本框，里面已预填一条示例；右边「体检结果」已经对这条示例分析过一次。
2. 删掉示例，粘贴你的 SQL。你的 SQL 只在本浏览器里被检查，不上传、不连集群。
3. 点「分析」。你会看到右边出现若干条结果，每条有严重度色标、粗体标题和一行修改建议；没有问题时显示绿色「未发现明显问题，可进一步使用 EXPLAIN ANALYZE 验证」。
4. 对照下文「SQL 体检规则」逐条处理。这只是文本模式匹配，不理解语义，命中不一定是真问题，没命中也不代表没问题；确认请用 `EXPLAIN` 看计划。

### 使用「调优流程」标签

1. 点「调优流程」标签。你会看到标题「SQL 调优标准流程」（旁边有「官方文档 ↗」，指向官方 [SQL 性能调优](https://docs.pingcap.com/zh/tidb/stable/sql-tuning-overview)）、四张步骤卡片（1 定位 / 2 看计划 / 3 修根因 / 4 控计划），下面「常用命令」有两段可复制的命令。
2. 第一段是「最近 1 小时最慢 Digest」的查询（只读 SELECT），第二段是 `EXPLAIN ANALYZE SELECT ...;` 与 `PLAN REPLAYER DUMP EXPLAIN SELECT ...;`。点代码框右上角「复制」，到 MySQL 客户端执行，其中 `SELECT ...` 要换成你的语句。
3. 注意 `EXPLAIN ANALYZE` 会**真正执行**语句（见「执行后果与风险」）。

## 区域与参数说明

### 慢查询数据怎么来的（口径）

| 项 | 内容 | 影响 / 说明 |
|---|---|---|
| 慢日志样本 | `cluster_slow_query` 中最近 1 小时、按 Query_time 降序的前 200 条执行记录，按 Digest 分组 | 只包含超过慢日志阈值的执行；最多 200 条样本，慢查询特别多时靠后的分组会被丢掉 |
| 语句摘要 | `cluster_statements_summary` 按累计耗时排名前 100 的 Digest | 与慢日志样本按 Digest 对上时，用摘要里的「执行次数 / 平均耗时 / 累计耗时」覆盖；对不上则退回样本口径 |
| 慢日志对比（最近 2 小时的聚合） | 另有一条只读查询，把 `cluster_slow_query` 最近 2 小时按 Digest + Plan_digest 聚合（只取计数、耗时合计、`Stats` / `Backoff_types` 的出现次数、`Resolve_lock_time` 等，**不取 SQL 文本**），用来对比「最近 1 小时」和「再早的 1 小时」 | 只对比慢日志样本；这条查询取不到（版本缺列、无权限）时，上面的标签照常工作，只是没有下表末尾的补充标签 |
| SQL 文本 | 该 Digest 下最慢的那一次的具体语句，最长取 2000 字符 | 含真实参数值，注意敏感信息；超长语句被截断，复制到别处可能不完整 |
| 库 / 用户 / 索引 | 同样取自最慢的那一次 | — |
| 执行计划 | 慢日志的 Plan 列，经 `tidb_decode_plan` 解码，是当时**实际执行**的计划，含实际行数 | 只对最近 1 小时最慢的前 100 条且 Plan 非空的记录取到，每个 Digest 取最慢的一条；不需要在线上跑 EXPLAIN ANALYZE |
| 刷新 | 页面每 30 秒取一次快照；点顶部横幅「立即刷新」可手动取 | 服务端快照缓存 10 秒，数据源缓存 15 秒 |

> 💡 口径是混合的：命中摘要的 Digest，「执行次数 / 平均耗时 / 累计」来自摘要（包含快的执行），Total_keys / Process_keys / 返回行 / 内存仍是慢日志样本均值；没命中摘要的 Digest，执行次数只是样本条数、平均耗时只是慢样本平均，会偏高。语句摘要按固定窗口滚动，官方默认 1800 秒（`tidb_stmt_summary_refresh_interval`，见 [Statement Summary 表](https://docs.pingcap.com/zh/tidb/stable/statement-summary-tables)），窗口刚滚动后数据会变少。

### Top SQL 表的列

表格没有搜索框、没有筛选、没有分页，所有分组一次列出。右上角的下拉框（按总耗时 / 按平均耗时 / 按执行次数 / 按扫描 Keys）**目前没有接入逻辑，选什么顺序都不变**，始终按累计耗时降序。

| 列 | 含义 | 口径 | 看数要点 |
|---|---|---|---|
| # | 排名 | 按累计耗时降序后的序号 | 会随刷新变化，别当作固定编号 |
| SQL（Digest） | 上行：SQL 文本（单行省略）；下行：`Digest · 库.用户` | Digest 是归一化后的哈希 | 同一 Digest 就是同一类 SQL |
| 执行次数 | 执行了多少次 | 命中摘要为摘要的执行次数，否则为慢日志样本条数 | 见上文优先级判断 |
| 平均耗时 | 单次平均耗时（秒） | 命中摘要为摘要的平均延迟，否则为慢样本的平均 Query_time | 与业务 SLA 对照，平台不设阈值 |
| P99 | 列名叫 P99 | 样本 Query_time 的 P99；若摘要里的最大延迟更大则取最大延迟（所以它可能其实是最大值） | 远大于平均说明长尾 |
| Total_keys | TiKV 扫过的 Key 数 | 慢日志样本的平均值，≥1 万显示为「x.x 万」 | 含 MVCC 旧版本 |
| Process_keys | 实际处理的 Key 数（不含旧版本） | 慢日志样本的平均值 | 见上文 |
| 问题标签 | 平台的初步判断（橙色标签） | 见下表 | 启发式结果，不是结论 |

表格下方的提示：Total_keys 远大于 Process_keys / 返回行，说明扫描了大量无效数据，通常是缺索引或统计信息不准。

### 问题标签与触发条件

按下表自上而下判断，**第一个命中的标签**被显示，所有命中的建议都会列出；没有命中任何一条时显示「耗时较长」。表末标「补充标签」的几项来自慢日志对比（见上文口径），只在没有更具体的标签时才替换「耗时较长」，其建议则总是列出、排在前面。阈值是平台自定的经验值，不是 TiDB 官方标准。

| 标签 | 触发条件 | 给出的建议 | 下一步与官方参考 |
|---|---|---|---|
| TiKV 排队等待（原叫「热点行锁等待」） | 慢样本平均 Wait_time ÷ 平均 Query_time ≥ 0.7 | Wait_time 是 TiKV 端 Coprocessor 线程的**排队等待**，不是锁等待；这类语句本身处理量不大，通常是被同时运行的重 SQL 或繁忙的 TiKV 拖慢。先找同时段处理量大的语句、看 TiKV 线程 CPU 与热点；怀疑锁冲突时再查 `information_schema.DATA_LOCK_WAITS` 与 `DEADLOCKS`，慢日志里 `Backoff_types` 含 `txnLock`、或 `Resolve_lock_time` 偏大才是锁冲突的证据 | 官方：wait_time 很大而 process_time 很小的语句通常不是问题语句。诊断页里它只作「提示」级别 |
| 索引列上使用函数 | SQL 中出现 `date / year / month / day / lower / upper / substr / substring / ifnull / cast` 套在单个列上并与 `= > < >= <= in` 比较 | 改写为范围条件 | 改完用 `EXPLAIN` 确认走了索引 |
| 前导通配符 LIKE | 出现 `like '%...` | 改用全文检索，或评估 TiFlash（建议命令 `ALTER TABLE <表> SET TIFLASH REPLICA 2;`） | [TiFlash 简介](https://docs.pingcap.com/zh/tidb/stable/tiflash-overview) |
| 缺索引 / 统计信息过期 | 是 SELECT，且平均 Total_keys ≥ 100 万、Process_keys > 0、Total_keys ÷ max(返回行, 1) ≥ 1000、且 Index_names 为空（显示「—（TableFullScan）」） | `ANALYZE TABLE <表>;`，再给过滤条件列建（联合）索引；建索引前可用 SQL Binding 固定计划验证 | [ANALYZE](https://docs.pingcap.com/zh/tidb/stable/sql-statement-analyze-table)、[统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics)、[SQL Binding](https://docs.pingcap.com/zh/tidb/stable/sql-plan-management) |
| 大查询占用内存高 | 样本中最大 Mem_max ≥ 1 GiB | 限制单 SQL 内存 `SET SESSION tidb_mem_quota_query = 2147483648;`；报表类查询放独立资源组或下推 TiFlash | 变量见 [系统变量](https://docs.pingcap.com/zh/tidb/stable/system-variables) |
| 高频写入（注意写热点） | 以 insert 开头且执行次数 ≥ 10 万 | 主键改用 AUTO_RANDOM，或 SHARD_ROW_ID_BITS 打散 | [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues) |
| 统计信息缺失 | 执行计划里有算子带 `stats:pseudo`（仅在原本是「耗时较长」时才替换标签） | `ANALYZE TABLE <表名>;` | [统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics) |
| 估算与实际行数偏差大 | 执行计划里有算子估算与实际行数偏差 ≥10 倍（同上，仅替换「耗时较长」） | 更新统计信息并检查直方图 | [EXPLAIN 走查](https://docs.pingcap.com/zh/tidb/stable/explain-walkthrough) |
| 耗时较长 | 以上都没命中 | 提示用 `EXPLAIN ANALYZE <该 SQL>;` 对比 estRows 与 actRows | [SQL 性能调优](https://docs.pingcap.com/zh/tidb/stable/sql-tuning-overview) |
| 锁冲突（补充标签） | 最近 1 小时该 Digest 的慢日志里 ≥ 30% 的 `Backoff_types` 含 `txnLock`；可以替换「耗时较长」和「TiKV 排队等待」 | 给出 `DATA_LOCK_WAITS` / `DEADLOCKS` 查询；证据里有 Resolve_lock_time | 这是比 Wait_time 更直接的锁冲突证据；官方 [锁冲突排查](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts) |
| 执行计划突变（补充标签） | 上一小时有样本，最近 1 小时出现了上一小时没有的执行计划（Plan_digest）；仅替换「耗时较长」 | 列出新旧计划摘要和平均耗时变化，给出查看近期计划、按旧计划绑定的语句 | 绑定前先 `EXPLAIN ANALYZE` 验证旧计划确实更好；官方 [执行计划管理](https://docs.pingcap.com/zh/tidb/stable/sql-plan-management) |
| 统计信息缺失（补充标签，来自慢日志） | ≥ 50% 的慢日志样本 `Stats` 含 `pseudo`；仅替换「耗时较长」 | `SHOW STATS_HEALTHY` 与 `ANALYZE TABLE` | 与上面「来自执行计划」的同名标签是两个来源 |
| TiKV 繁忙（补充标签） | ≥ 30% 的慢日志 `Backoff_types` 含 `tikvServerBusy`；仅替换「耗时较长」 | 看 TiKV 线程池 / 磁盘再决定限流或扩容 | 症状，不是根因 |
| MVCC 旧版本多（补充标签） | 平均 Total_keys ≥ 10 万且 ÷ Process_keys ≥ 10（没被判为「缺索引」）；仅替换「耗时较长」 | 确认 GC 在推进、没有长事务 | 官方：Total_keys 远大于 Process_keys 说明旧版本多 |

另外：SQL 以 `select *` 开头时，额外追加「避免 SELECT *」的建议（不改变标签）。表名由正则从 SQL 里的第一个 `from / update / into` 后面取，多表 / 子查询时可能不是你关心的那张表；取不到会写成 `<表名>`，复制前必须核对并替换成真实表名。

### SQL 详情抽屉

点击表格行打开，标题「SQL 详情 · Digest」。

| 区块 | 内容 | 口径 / 说明 |
|---|---|---|
| SQL 文本 | 完整（最长 2000 字符）的语句 | 含具体参数值 |
| 平均耗时 / P99 | 如「2.84s / 6.1s」 | 同表格 |
| 执行次数 | 同表格 | — |
| 最大内存 | 样本中最大的 Mem_max，显示为 B / KiB / MiB / GiB | 取不到显示「—」 |
| 汇总行 | `Total_keys · Process_keys · 返回行 · 索引` | 返回行是样本平均并取整；索引是 Index_names，空则显示「—（TableFullScan）」。这个标注只是「没有记录使用索引」，点查、聚簇主键范围扫描等也可能出现，请以执行计划为准 |
| 执行计划（EXPLAIN ANALYZE） | 执行计划表 | 见下节 |
| 优化建议 | 有序列表，部分带可复制命令 | 执行计划相关的建议排在前面 |
| 复制 Plan Replayer 命令 | 复制 `PLAN REPLAYER DUMP EXPLAIN <SQL 文本>`，其中 SQL 文本已填好 | 只复制，不执行 |
| 慢查询字段说明 ↗ | 官方 information_schema.slow_query 文档 | 新窗口打开 |

### 执行计划表与判定规则

| 列 | 含义 |
|---|---|
| 算子 | 带树形缩进的算子 ID，如 `└─TableFullScan_14` |
| estRows | 优化器估算的行数（≥1 万显示为「x.x 万」） |
| actRows | 实际执行返回的行数；计划里没有该列时取 estRows，此时不会被判为偏差 |
| task | 执行位置：root（TiDB）、cop[tikv]（下推到 TiKV）、cop[tiflash] / mpp（TiFlash）等 |
| access object | 访问对象（表、分区、索引）；下一行灰字为算子详情（operator info） |

| 规则 | 页面（红色行） | 后端建议 |
|---|---|---|
| 估算与实际偏差 | 行变红：actRows ÷ max(estRows, 1) ≥ 10，或 estRows ÷ max(actRows, 1) ≥ 10 且 estRows > 1000。有红行时下方出现红色提示 | 追加「某某的 estRows 与 actRows 偏差 ≥ 10 倍…」建议，条件更严：actRows ≥ 1000 且 actRows ÷ estRows ≥ 10，或 estRows > 1000 且 estRows ÷ actRows ≥ 10 |
| 伪统计信息 | 页面不单独标记，在 operator info 里能看到 `stats:pseudo` | 追加「X 使用了 pseudo 统计信息…」建议，并给 `ANALYZE TABLE <表名>;` |

所以你可能看到红色行但没有对应文字建议（行数小于 1000 时前端也会标红）；带 LIMIT / TopN 的算子上游估算偏大、实际很小可能是正常现象，需结合算子判断（经验，非平台判定）。全表扫描的识别靠你看计划里是否有 `TableFullScan`；平台没有单独的「全表扫描」判定，只有上面「缺索引」标签的统计条件。

### SQL 体检规则（浏览器内静态规则）

打开标签就会分析一次预填示例；点「分析」重新检查。页面注明「基于规则的示例」。

| 命中规则 | 级别 | 触发条件 | 建议 |
|---|---|---|---|
| 使用 SELECT * | 警告 | 含 `select *` | 只查询需要的列 |
| LIKE 前导通配符，无法使用索引 | 警告 | 含 `like '%` | 改用前缀匹配、全文检索，或评估 TiFlash |
| 在条件列上使用函数，索引失效 | 严重 | WHERE 之后出现 `date / year / month / lower / upper / substr / ifnull / cast` 套单个列并与 `= > <` 比较 | 改为范围条件 |
| 深分页 OFFSET n | 警告 | `LIMIT n, m` 或 `OFFSET n` 且 n ≥ 10000 | 改游标分页 |
| 缺少 WHERE 条件 | 严重 | 没有 where 且含 select / update / delete | 补过滤条件；大批量 DELETE / UPDATE 请分批 |
| IN 子查询 | 提示 | `in (select` | 评估改写为 JOIN / EXISTS |
| ORDER BY 无 LIMIT | 提示 | 有 order by 而无 limit | 确认是否真的需要全部结果 |
| 条件中包含 OR | 提示 | WHERE 之后含 ` or ` | 评估改写为 UNION ALL |

这只是文本模式匹配，可能误报或漏报（例如不带 WHERE 的 `SELECT 1` 也会被判为缺少 WHERE）。

## 执行后果与风险

> ⚠️ 本页没有任何执行入口：所有按钮只是「复制」或「跳转」。平台不会替你执行 DDL、`ANALYZE`、`SET GLOBAL`、`CREATE BINDING` 或 `EXPLAIN ANALYZE`。运维模式也是如此（SQL 页没有写操作）。

> ⚠️ **`EXPLAIN ANALYZE` 会真正执行语句**（官方说明见 [EXPLAIN ANALYZE](https://docs.pingcap.com/zh/tidb/stable/sql-statement-explain-analyze)）。对 `UPDATE` / `DELETE` / `INSERT` 复制「`EXPLAIN ANALYZE <该 SQL>`」去执行，数据会被真的修改。只想看计划不执行，用不带 ANALYZE 的 `EXPLAIN`。

> ⚠️ 复制出去的语句有后果，请在你们的变更流程里评估后再执行，逐项如下。

- `ANALYZE TABLE`：会对表做统计信息收集，大表耗费 TiKV 读资源，宜低峰期。
- `ALTER TABLE ... ADD INDEX`：在线 DDL，会占用 TiKV / TiDB 资源并扩大存储，耗时随表大小增长。
- `ALTER TABLE ... SET TIFLASH REPLICA 2`：为整张表创建 TiFlash 副本，占用存储与复制带宽，且需要集群里有足够的 TiFlash 节点；仅在确有分析型查询需求时使用。
- `SET GLOBAL` / `SET SESSION`：全局变量会影响所有新会话；会话级只影响当前连接。
- `CREATE GLOBAL BINDING`：固定执行计划，数据分布变化后可能反而变差。
- `PLAN REPLAYER DUMP EXPLAIN ...`：只生成现场，不执行语句；导出包含表结构、统计信息、执行计划等，但不含表数据（官方说明），分享前仍请注意脱敏。
- 查询 `DATA_LOCK_WAITS`：会实时访问所有 TiKV，大集群高负载时可能引起性能波动。

> ⚠️ SQL 文本取自慢日志原文，可能包含手机号、金额等真实参数值；回放 / 导出样例数据包时平台会对字符串字面量做脱敏，但实时页面上看到的是原文。只读模式没有登录，请不要把页面暴露给无权限的人。

## 已知限制与易混淆点

| 界面写的 | 实际是 | 怎么办 |
|---|---|---|
| 右上角排序下拉（按总耗时 / 平均耗时 / 执行次数 / 扫描 Keys） | 没有接入逻辑，始终按累计耗时降序；表格也没有搜索、筛选、分页 | 自己在浏览器里用 Ctrl+F 找 |
| 标题旁「模拟，对应 Statement Summary / SLOW_QUERY」 | 静态原型的固定文案，真实 / 回放模式下同样显示，数据并非模拟 | 以顶栏角标和横幅为准 |
| 列名「P99」 | 样本 P99 与摘要最大延迟取大者，可能其实是最大值 | 当作「长尾上限」看 |
| 「平均耗时」 | 命中摘要时含快的执行；没命中时只是慢样本平均，偏高 | 与 Grafana 对不上属正常 |
| 汇总行索引「—（TableFullScan）」 | 只表示没有记录使用索引，不一定是全表扫描 | 以执行计划里是否有 `TableFullScan` 为准 |
| 红色行 | 前端阈值比后端建议宽松（行数 <1000 也会标红） | 结合算子判断，带 LIMIT / TopN 时可能正常 |
| 标签「TiKV 排队等待」 | 按 Wait_time 占比判断，而 Wait_time 是 TiKV 端累计的 Coprocessor 排队时间，不是行锁等待（旧版本把它叫「热点行锁等待」，会把排队误报成锁） | 先找同时段处理量大的语句；怀疑锁冲突再用只读查询确认 |
| 从诊断页点「查看该 SQL」 | 只打开本页列表，不会定位到那一条 | 按 Digest 或 SQL 文本找 |
| 建议里的 `<表名>` | 没能从 SQL 解析出表时的占位符 | 复制后手动替换成真实表名 |
| 「SQL 体检」 | 静态正则示例，不理解语义 | 以 `EXPLAIN` 为准 |
| 每 30 秒自动刷新 | 按代码，自动重绘会让页面回到「Top SQL / 慢查询」标签，「SQL 体检」文本框恢复成示例（文本框正在输入时不重绘，抽屉打开时不重绘）；在「调优流程」页签停留超过 30 秒也会被切回（以真实浏览器表现为准） | 读执行计划请先点开抽屉；体检时保持光标在文本框内，或先把 SQL 另存 |

## 常见问题与排查

- **列表是空的**：可能是没有超过慢日志阈值的 SQL、没配置只读 SQL 连接（看横幅）、最近 1 小时没有慢查询，或运维模式下你的角色没有 `view.sql` 权限（页面不会提示原因）。
- **平均耗时比我在 Grafana 看到的低 / 高**：口径不同，见「慢查询数据怎么来的」。
- **详情里没有执行计划**：Plan 列为空（可能 `tidb_record_plan_in_slow_log` 已关闭），或该 Digest 不在最近 1 小时最慢的前 100 条里；执行计划只在有慢日志样本时才有。
- **想用图形界面看慢查询 / 语句分析**：官方内置的 TiDB Dashboard 有「慢查询」「SQL 语句分析」「Top SQL」页面，见 [慢查询页面](https://docs.pingcap.com/zh/tidb/stable/dashboard-slow-query)、[SQL 语句分析](https://docs.pingcap.com/zh/tidb/stable/dashboard-statement-list)、[Top SQL](https://docs.pingcap.com/zh/tidb/stable/top-sql/)；Dashboard 内置在 PD 中（[简介](https://docs.pingcap.com/zh/tidb/stable/dashboard-intro)）。
- **刷新后抽屉被关闭**：打开抽屉期间不会自动刷新；若你手动点了「立即刷新」，页面会重绘并关闭抽屉。

## 官方参考

以下链接均已打开核实，版本以你集群的为准（链接指向 stable 版）。

- [慢查询日志](https://docs.pingcap.com/zh/tidb/stable/identify-slow-queries)：慢日志阈值默认 300 ms；Wait_time、Total_keys、Process_keys、Mem_max、Index_names、Plan 字段含义
- [SLOW_QUERY 表](https://docs.pingcap.com/zh/tidb/stable/information-schema-slow-query)：`SLOW_QUERY` / `CLUSTER_SLOW_QUERY` 的列
- [Statement Summary 表](https://docs.pingcap.com/zh/tidb/stable/statement-summary-tables)：SQL Digest、`cluster_statements_summary`、默认窗口 1800 秒
- [SQL 性能调优](https://docs.pingcap.com/zh/tidb/stable/sql-tuning-overview)：调优总览
- [执行计划概览](https://docs.pingcap.com/zh/tidb/stable/explain-overview)：estRows、task、access object、operator info
- [EXPLAIN](https://docs.pingcap.com/zh/tidb/stable/sql-statement-explain)：只显示计划、不执行
- [EXPLAIN ANALYZE](https://docs.pingcap.com/zh/tidb/stable/sql-statement-explain-analyze)：会执行语句；actRows、execution info
- [EXPLAIN 走查](https://docs.pingcap.com/zh/tidb/stable/explain-walkthrough)：TableFullScan、`stats:pseudo`、ANALYZE 与加索引的示例
- [常规统计信息](https://docs.pingcap.com/zh/tidb/stable/statistics)：统计信息、健康度、伪统计信息
- [ANALYZE](https://docs.pingcap.com/zh/tidb/stable/sql-statement-analyze-table)：更新统计信息
- [执行计划管理（SPM）](https://docs.pingcap.com/zh/tidb/stable/sql-plan-management)：SQL Binding
- [Optimizer Hints](https://docs.pingcap.com/zh/tidb/stable/optimizer-hints)：`USE_INDEX` 等提示
- [PLAN REPLAYER](https://docs.pingcap.com/zh/tidb/stable/sql-plan-replayer)：导出 / 导入现场信息，不导出表数据
- [DATA_LOCK_WAITS](https://docs.pingcap.com/zh/tidb/stable/information-schema-data-lock-waits)、[DEADLOCKS](https://docs.pingcap.com/zh/tidb/stable/information-schema-deadlocks)、[锁冲突问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-lock-conflicts)：锁排查
- [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)：AUTO_RANDOM、SHARD_ROW_ID_BITS
- [TiFlash 简介](https://docs.pingcap.com/zh/tidb/stable/tiflash-overview)：列存与 Raft Learner 异步复制
- [GC 机制简介](https://docs.pingcap.com/zh/tidb/stable/garbage-collection-overview)：MVCC 旧版本与 `tidb_gc_life_time`
- [系统变量](https://docs.pingcap.com/zh/tidb/stable/system-variables)：TiDB 系统变量总览（页面存在，具体变量条目请在页内搜索）
- [TiDB Dashboard 慢查询](https://docs.pingcap.com/zh/tidb/stable/dashboard-slow-query)、[SQL 语句分析](https://docs.pingcap.com/zh/tidb/stable/dashboard-statement-list)、[Top SQL](https://docs.pingcap.com/zh/tidb/stable/top-sql/)：官方图形化工具

## 相关页面

- [智能诊断](go:diag)：慢 SQL 会生成诊断项（最多 4 条）
- [热点与调度](go:hotspot)：写热点与 Region
- [集群总览](go:overview)：DB Time 按 SQL 类型占比
- [故障排查](go:trouble)：官方故障排查地图
- [日常运维](go:ops)：参数建议（慢日志阈值、计划缓存等）
- 帮助：[集群总览](help:overview)
