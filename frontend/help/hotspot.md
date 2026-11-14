# 热点与调度

> 💡 看清「哪台 TiKV 的写入明显比别人高、PD 认为哪些 Region 是热点」，并给出常见的打散办法与可复制的命令。所有角色（含访客）都能打开；本页只读，**不会对集群执行任何操作**，只展示数据。只读模式与运维模式下内容相同（运维模式需先登录，权限要求只是 view.overview，所有角色都有）。

## 这个页面做什么

先认识几个词（后文反复用到）：

- **Region**：TiKV 里数据的最小调度单位，把整个 Key 空间按范围切成许多段，每段一个 Region，默认约 256 MiB，每个 Region 有多个副本，其中一个是 **Leader**，读写默认都走 Leader（[TiDB 数据存储](https://docs.pingcap.com/zh/tidb/stable/tidb-storage)）。
- **热点 Region**：读或写流量明显高于其他 Region、集中压在少数 Region 和少数 TiKV 上的 Region。分 **写热点**（写入集中，常见原因是顺序递增主键）和 **读热点**（少量 Key 被集中读取）（[热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)）。
- **PD**（Placement Driver）：集群的调度中心，负责热点调度、Region / Leader 均衡等（[TiDB 数据库的调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling)）。
- **pd-ctl**：PD 的命令行工具，通过 `tiup ctl:<版本> pd -u <PD 地址> ...` 使用（[PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control)）。

页面分为四块：

1. **流量热力图**：每台 TiKV 一行、时间为横轴，颜色越亮写入越多，用来判断「是不是某一台 TiKV 持续比别人忙」。
2. **各 TiKV 写流量**：每台 TiKV 当前的写入速率（MB/s）条形图，并在不均衡时给出提示。
3. **热点 Region Top**：PD 判定为读热点或写热点的 Region 列表（含所属表与 Key 范围）。
4. **解决方案选择器**：按场景列出 4 种常见处理办法与可复制的命令（AUTO_RANDOM、SHARD_ROW_ID_BITS、负载自适应副本读取、手动 split）。

页面名叫「热点与调度」，但**本页不展示 PD 的调度器、operator 或调度参数**（如 `region-schedule-limit`）。调度参数请看 [日常运维](go:ops) 的「参数管理」或 [节点管理](go:nodes) 的配置变更页。

三种数据状态，先确认你在哪一种（看右上角标签和顶部横幅）：

| 状态 | 怎么识别 | 本页数据 |
|---|---|---|
| 静态原型 | 没有后端（直接打开 html），右上角标签是「产品演示」 | 热力图与热点 Region 均为内置模拟数据，卡片副标题写「Key Visualizer（模拟）」，仅用于演示，**不要据此做判断** |
| 回放模式 | 顶部横幅「回放数据」，右上角标签「回放数据」 | 来自录制的 fixture，不连接任何集群 |
| 真实数据（只读） | 顶部横幅「真实数据（只读）」，右上角标签「真实数据 · 只读」 | 来自 Prometheus、PD API 与 TiDB 的 information_schema，每 30 秒自动刷新一次 |

> ⚠️ 平台从未对真实集群运行过。下文对真实数据的说明来自代码阅读与回放数据，PD / Prometheus 的实际返回格式、指标名与版本差异以真实集群联调为准。

## 怎么用（按步骤）

下面是一次完整的「看数据 → 下判断」流程。每步只做一件事。

1. 在左侧菜单点「热点与调度」。
   - 你会看到：页面分成左边的热力图、右边的「各 TiKV 写流量」与「热点 Region Top」，下方是「解决方案选择器」。
2. 看页面顶部横幅，确认数据状态与采集时间。
   - 你会看到：形如「真实数据（只读） · 集群 prod-tidb-01 · 采集于 14:32:10」。
   - 如果横幅右侧有「N 项数据缺失 ▸」，点它展开明细；缺的是「热点数据」（PD 热点接口）或 Prometheus 指标时，对应区域会为空，先修采集再判断。
   - 如果横幅写的是「回放数据」或根本没有横幅，说明看到的不是你集群的实时数据。
3. 看右上的「各 TiKV 写流量」卡片：有没有一台条形明显最长、变红。
   - 你会看到：每台 TiKV 一行「tikv-N、条形、数字（MB/s）」，卡片底部有黄色（不均衡）、绿色（较均衡）或蓝色（未采集）提示。
   - 判读见下文「区域怎么读 · 各 TiKV 写流量」。
4. 看左侧「流量热力图」：把鼠标放到色块上，悬停框显示「主机 · 时间 · X MB/s」，看是不是某一行长时间偏亮。
   - 你会看到：真实 / 回放数据下每行一个主机 IP、横轴是最近 1 小时（每分钟一格）。
5. 看「热点 Region Top」：热点是否集中在同一张表、同一个 Leader（tikv-N）上，记下表名与 Region 编号。
   - 你会看到：每条一行「Region 编号、红色标签（写热点 / 读热点）、表名 · Leader · 范围」；没有热点时显示「当前没有热点 Region」。
6. 综合第 3–5 步，对照下文「如何判断真热点与正常波动」下结论。
7. 如果判断是热点，到「解决方案选择器」按场景找卡片，点命令块右上角的「复制」，再到 SQL 客户端或中控机**自行评估后**执行（详见下文「使用解决方案」）。
   - 你会看到：点「复制」后右下角提示「已复制到剪贴板」；页面本身**不会执行**命令。
8. 需要立即重新取数时，点顶部横幅右侧的「立即刷新」。
   - 你会看到：提示「已刷新」，页面按最新快照重绘；后端对强制刷新有 2 秒节流，连点只会取一次。
   - 如果提示「刷新失败：…」，说明后端或 Prometheus / PD 暂时不可达，见「常见问题与注意」。

## 区域怎么读

### 流量热力图

| 项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 行（真实 / 回放数据） | 一台 TiKV 一行，行标签是主机 IP（不带端口），按 IP 排序 | 取决于 Prometheus 中有 `tikv_engine_flow_bytes` 的实例 | 同一主机上部署多个 TiKV 时，行标签相同，需靠位置区分 |
| 列（真实 / 回放数据） | 时间，最近 1 小时、每分钟一格 | 固定窗口，页面上不可调 | 悬停显示该格的时间 |
| 颜色 | 该 TiKV 在该分钟的写入速率，色阶由暗蓝到青、黄、近白 | 以画面内最大值为 1 归一（最大值不足 1 MB/s 时以 1 MB/s 为上限，整体偏暗） | 颜色只表示相对强弱，不同刷新之间不能比较绝对值，要看悬停的 MB/s 数字 |
| 数据口径 | 各 TiKV 的 `tikv_engine_flow_bytes{db="kv",type="wal_file_bytes"}` 的 1 分钟速率 | 无 | 这是 WAL（预写日志）写入字节，用来近似「写流量」，不等于业务层的写 QPS |
| 「写流量 / 读流量」按钮 | 原型里用来切换两种模拟图 | 默认「写流量」 | **真实 / 回放数据下只有写流量图**：点「读流量」只改变按钮高亮，图不会变（见已知限制） |
| 没有数据 | 画布上提示「没有采集到各 TiKV 的写入流量（Prometheus 指标 tikv_engine_flow_bytes）」 | 无 | 先检查 Prometheus 地址与 TiKV 指标 |

热力图下方那句「写热点表现为阶梯状斜线、读热点表现为明亮的横线」描述的是 TiDB Dashboard 的 **Key Visualizer**（流量可视化，横轴时间、纵轴是按 Key 排序的连续 Region，见 [Key Visualizer 官方说明](https://docs.pingcap.com/zh/tidb/stable/dashboard-key-visualizer)）。本页真实数据下的热力图是「**Store × 时间**」，不是 Key 范围，因此读法不同：

- 写入集中在某一台 TiKV：表现为**某一行持续偏亮**，而不是斜线。
- 所有行一起变亮：通常是全局写入增加（批量导入、备份、业务高峰），不是热点。
- 想看 Key 范围维度（具体哪段 Key 热）：到 TiDB Dashboard 的「流量可视化」页。Dashboard 内置在 PD 中，浏览器打开 `http://<PD 地址>/dashboard`（PD 地址见 [环境与设置](go:settings) 的「组件地址」，示例 `10.0.1.21:2379`），详见 [TiDB Dashboard 介绍](https://docs.pingcap.com/zh/tidb/stable/dashboard-overview/)。

### 各 TiKV 写流量

| 项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 条形与数字 | 每台 TiKV 的写入速率，单位 MB/s，条长相对于最大值 | 无 | 数字取 Prometheus 最近一个点（按主机归并）；该值为 0 时，用 PD 热点统计里该 Store 作为 Leader 的总写流量补上 |
| 条形变红 | 该节点写入速率超过 100 MB/s，或（真实 / 回放数据下）是各 TiKV 平均值的 3 倍以上 | 阈值写在页面代码里，不可配置 | 仅是颜色提示，不触发任何动作 |
| 提示：不均衡（黄） | 最大值 ÷ 所有 TiKV 的平均值（含自己）≥ 3，文案形如「tikv-2 写流量是均值的 4.8×，节点间不均衡」 | 无 | 仅提示 |
| 提示：较均衡（绿） | 最大值不到平均值的 3 倍，文案「各 TiKV 写流量较均衡（最大为均值的 N×）」 | 无 | 仅提示 |
| 提示：未采集（蓝） | 平均值为 0 或没有数据，文案「未采集到各 TiKV 的写流量。」 | 无 | 先检查 Prometheus |

### 热点 Region Top

| 列 / 元素 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| Region 编号 | PD 的 Region ID，显示为「Region 20871」 | 数字 | 复制 split 命令、查 `pd-ctl region` 时用到 |
| 红色标签 | 「写热点」或「读热点」 | 来自 PD 的 `hotspot/regions/write` 与 `hotspot/regions/read` | 一个 Region 同时出现在读写热点里时，只显示流量较大的那一条 |
| 表名 | 该 Region 所属的表 | 来自 `information_schema.tidb_hot_regions`，取不到显示「—」 | 需要平台有只读 SQL 账号；取不到时仍能看 Region 与范围 |
| Leader | 该热点 Region 所在 Store 对应的 TiKV 实例编号，如 tikv-2 | 按 PD 的 `as_leader` 视角汇总 | 用来对照「各 TiKV 写流量」 |
| 范围 | 该 Region 的 Key 范围，尽力解码成人话 | 见下表 | 用来反推是哪张表、哪个索引 |
| 没有数据 | 真实 / 回放数据下显示「当前没有热点 Region」 | 无 | 说明 PD 当前没有判定热点（不等于没有写入不均衡） |

列表按流量从大到小排序；页面上**不显示流量数值本身**，只用于排序（想看流量请在中控机用 `hot write` / `hot read`，见下文）。

范围列的几种显示形式：

| 显示 | 含义 |
|---|---|
| `table_id=112 row 8800 … row 9100` | 同一张表内从 RowID 8800 到 9100，`table_id` 可在 `information_schema.tables` 的 `TIDB_TABLE_ID` 里反查 |
| `table_id=98 index 1` | 某张表某个索引（`index 1` 是索引 ID）的范围 |
| `row（聚簇索引键）` | 行键的主键不是 8 字节整数（聚簇索引 / 非整数主键），无法显示具体数字 |
| `-∞ … +∞` | 起止都是 Key 空间的两端（整个空间或某一端是空） |
| `TiDB 元数据` | 以 `m` 开头的 TiDB 元数据 Key |
| 一串十六进制，中间带「…」，再接「→」 | 解码失败时的回退显示，保留前后各 8 个字符 |

### 数据来源与刷新

| 数据 | 来源 | 刷新 |
|---|---|---|
| 热力图与各 TiKV 写流量 | Prometheus（最近 1 小时、步长 60 秒）；缺失时用 PD 热点统计补写流量 | 随后端快照刷新，页面每 30 秒自动重绘一次（正在输入或打开抽屉时不重绘） |
| 热点 Region | PD 的 `/pd/api/v1/hotspot/regions/write` 与 `read` | 同上 |
| 表名 | `information_schema.tidb_hot_regions`（只读 SQL，列含 TABLE_ID、DB_NAME、TABLE_NAME、REGION_ID、TYPE，见 [TIDB_HOT_REGIONS](https://docs.pingcap.com/zh/tidb/stable/information-schema-tidb-hot-regions)） | 同上 |
| Key 范围 | 先用热点统计自带的起止 Key，没有就从 PD 的 Region 抽样 / 读写流量 Top 50 里按 Region ID 补 | 同上 |

## 如何判断真热点与正常波动

先分清几种形态：

| 现象 | 更像什么 | 怎么确认 |
|---|---|---|
| 某一台 TiKV 的条形明显最高，且热力图里那一行长时间偏亮 | 写热点（单点写入集中） | 看「热点 Region Top」里是否有多条属于同一张表、同一个 Leader；再看该节点的 Raftstore CPU（[节点管理](go:nodes) 实例详情） |
| 所有 TiKV 的条形一起升高、热力图整体变亮 | 全局写入增加（批量导入、备份、业务高峰） | 对照 [总览](go:overview) 的 QPS 与业务发布 / 备份时间，这是容量问题，不是热点 |
| 「热点 Region Top」只有零星几条、表名各不相同、Leader 分散 | 一般性的 Region 波动 | 持续观察，刷新后是否仍在列表里 |
| 列表里全是「读热点」，没有「写热点」 | 读热点 | 本页没有读流量热力图，需结合慢 SQL（[SQL 优化](go:sql)）与 TiKV Coprocessor 指标判断 |

数字阈值（以代码为准）：

| 判断 | 阈值 | 用在哪 |
|---|---|---|
| 条形变红 | 写入速率 > 100 MB/s，或 ≥ 所有 TiKV 平均值的 3 倍 | 本页「各 TiKV 写流量」 |
| 黄色「不均衡」提示 | 最大值 ÷ 所有 TiKV 平均值（含自己）≥ 3 | 本页「各 TiKV 写流量」 |
| 「疑似写入热点」诊断项 | TiKV 不少于 3 台；某台写流量 ≥ 20 MB/s；且 ≥ 其余 TiKV 平均值的 3 倍（写流量取 Prometheus 与 PD 热点统计中较大者） | [智能诊断](go:diag)，命中为严重级别，并生成给 [故障排查](go:trouble) 向导用的证据 |

> 💡 本页黄色提示与诊断规则的口径不同：本页的平均值**含自己**，诊断规则是「其余节点」的平均值，且诊断有 20 MB/s 的下限。TiKV 台数少时本页更难提示：只有 3 台时，除非另外两台几乎没有写入，倍数到不了 3；6 台时，本页要求最高值约为其余节点平均值的 5 倍才变黄（诊断只要 3 倍）；10 台时约 3.9 倍。两者结论不一致时，**以智能诊断为准**，本页提示仅作参考。

### 判断后的下一步

| 你的结论 | 下一步页面与操作 |
|---|---|
| 确认是写热点，且表有 AUTO_INCREMENT / 顺序递增主键 | 本页「解决方案选择器」第一、二张卡片，按下文「使用解决方案」操作 |
| 需要立刻止血（业务已受影响） | 第四张卡片手动 split，同时联系业务方安排主键改造 |
| 热点 Region 的 Leader 所在节点还同时磁盘 / CPU 高 | [节点管理](go:nodes) 看该实例详情；需要驱逐 Leader 或扩容时走变更流程 |
| 全局写入增加 | [总览](go:overview) 看 QPS 与事件时间线，评估扩容，不是打散热点 |
| 读热点 | [SQL 优化](go:sql) 找慢 SQL；评估第三张卡片（负载自适应副本读取） |
| 不确定 | [智能诊断](go:diag) 看是否命中「疑似写入热点」；[故障排查](go:trouble) 走「写入变慢」向导 |

## 使用解决方案

「解决方案选择器」的 4 张卡片都只是给出说明和命令，页面不执行。以下按场景写完整步骤。

**前置条件**：你有目标数据库的相应权限（建表 / 改表 / 设置全局变量需要较高权限）；先在测试库验证；了解业务是否依赖主键递增。执行这些命令**不经过平台的审批与审计**，需要留痕的变更请先在变更系统登记。

### 场景一：写热点，表有整数主键（AUTO_RANDOM）

AUTO_RANDOM 让主键值随机分布，新写入落到不同 Region，用来替代自增主键（[官方说明](https://docs.pingcap.com/zh/tidb/stable/auto-random)）。

1. 先确认表结构：在 SQL 客户端执行 `SHOW CREATE TABLE <库名>.<表名>;`（库名、表名来自「热点 Region Top」的表名列，或用 `table_id` 反查，示例 `shop.event_log`）。
   - 你会看到：主键定义及 `AUTO_INCREMENT`；主键后带 `CLUSTERED` 表示聚簇索引。
2. 判断主键是否为 `BIGINT` 整数、业务是否依赖主键递增（如按 id 范围分页、按 id 排序取最新）。若依赖，AUTO_RANDOM 会破坏这些假设，需先改业务，或改用场景二。
3. 点第一张卡片「写热点 · 表有整数主键，可改为随机」命令块的「复制」，在**测试库**建一张新表验证：`CREATE TABLE t (id BIGINT PRIMARY KEY AUTO_RANDOM, ...)`，把 `t` 与 `...` 换成你的表名与列。
   - 你会看到：建表成功；写入后 id 不再连续递增。
4. 生产上建新表后做数据迁移与切换。官方文档对已有表的说明：不支持用 ALTER TABLE 添加或移除 AUTO_RANDOM，但支持把 AUTO_INCREMENT 列改成 AUTO_RANDOM（列最大值已接近 BIGINT 上限时会失败）；你的版本是否支持以官方文档为准，建议新建表。
5. 切换后回到本页，等几个刷新周期（每 30 秒一次），确认该表不再占据「热点 Region Top」的多条，对应 TiKV 条形回落。
   - 与预期不符：热点仍在 → 检查是否还有别的顺序写入的二级索引；见 [故障排查](go:trouble) 向导。

### 场景二：写热点，无主键或非聚簇索引（SHARD_ROW_ID_BITS）

SHARD_ROW_ID_BITS 把隐式 RowID（TiDB 给没有聚簇主键的行自动分配的递增编号）高位打散，`4` 表示 16 个分片（[官方说明](https://docs.pingcap.com/zh/tidb/stable/shard-row-id-bits)）。官方说明它适用于没有聚簇主键或没有主键的表；有聚簇主键的表不适用。

1. 同场景一第 1 步，确认主键是 `NONCLUSTERED` 或无主键。
2. 点第二张卡片的「复制」，得到 `ALTER TABLE t SHARD_ROW_ID_BITS = 4;`（改已有表）或建表时带 `SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3`（建表预切分，`PRE_SPLIT_REGIONS` 表示建表后立即按 2 的 N 次方预先切出多个 Region，且不能大于 SHARD_ROW_ID_BITS，见 [Split Region](https://docs.pingcap.com/zh/tidb/stable/sql-statement-split-region)）。
3. 把 `t` 换成你的表名，在测试库执行验证，再按变更流程上生产。
   - 你会看到：语句成功返回；`SHOW CREATE TABLE` 的表选项里出现 `SHARD_ROW_ID_BITS=4`（可能被 `/*T! ... */` 注释包着）。
4. 回到本页观察热点是否分散，方法同场景一第 5 步。
   - 注意：打散 RowID 后，按 RowID 顺序扫描的行为会变化；如何选值，官方建议约为 TiKV 台数的以 2 为底的对数。

### 场景三：读热点，Leader 排队严重（负载自适应副本读取）

当 Leader 预估排队时间超过阈值，改由 Follower 副本读取（[热点问题处理 · 打散读热点](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)）。

1. 先记录当前值：`SELECT @@GLOBAL.tidb_load_based_replica_read_threshold;`，把结果记在变更单里（回滚时要用）。
   - 你会看到：一个时间值。若报「Unknown system variable」，说明你的 TiDB 版本不支持，不要继续。
2. 点第三张卡片的「复制」，得到 `SET GLOBAL tidb_load_based_replica_read_threshold = "1s";`，确认阈值（`"1s"` 是示例，请按业务可接受的排队时间调整）。
3. 在业务低峰期用有权限的账号执行。
   - 你会看到：`Query OK`。变量是集群级，**立即影响所有新会话**的读路径。
4. 观察读延迟与 Leader 排队是否改善；若变差，执行 `SET GLOBAL tidb_load_based_replica_read_threshold = "<原值>";` 回滚（`<原值>` 是第 1 步记下的值）。
   - Follower 读为保证一致性仍需 ReadIndex 操作，小查询的额外开销更明显，详见 [Follower Read](https://docs.pingcap.com/zh/tidb/stable/follower-read)。

### 场景四：单个热点 Region 临时止血（手动 split）

1. 在「热点 Region Top」确认要处理的 Region 编号（例：20871），并确认它仍在列表里（点一次「立即刷新」）。
2. 点第四张卡片的「复制」。真实 / 回放数据下，命令里的 PD 地址取自拓扑里的第一个 PD 实例、版本取自集群版本、Region 编号取热点列表**第一条**；没有热点时是占位符 `<region_id>`，必须手工替换为实际编号。静态原型里是演示地址 `10.0.1.21:2379`，不要直接用。
3. 在中控机执行第一行：`tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 operator add split-region 20871 --policy=approximate`。其中 `v8.5.2` 是你集群的版本，`http://10.0.1.21:2379` 是任一 PD 的地址，`20871` 是上面确认的 Region 编号（[pd-ctl operator 说明](https://docs.pingcap.com/zh/tidb/stable/pd-control)）。
   - 你会看到：pd-ctl 通常返回 `Success!`（不同版本文案可能略有差异）；如果提示 Region 不存在，说明 Region 已被调度或合并，回到本页重新确认编号。
4. 执行第二行 `... hot write` 查看当前各 Store 的写热点与流量。
   - 你会看到：按 Store 列出的热点 Region 与流量字节数。
5. 等几个刷新周期，回本页确认表现。
   - 与预期不符：拆分后热点又回来 → 根因是顺序递增主键，需要场景一或场景二的根治。

## 执行后果与风险

本页没有任何会改动集群的按钮：热力图切换、「立即刷新」、命令「复制」都只是前端行为或只读取数。因此本页自身**不需要审批，也不会写审计**。

但页面给出的命令如果你自己去执行，会产生下面的后果。这些命令都由人在中控机或 SQL 客户端里执行，不经过平台的审批与审计；需要留痕的变更请改走 [节点管理](go:nodes) 或 [日常运维](go:ops) 的变更流程。

> ⚠️ `ALTER TABLE ... SHARD_ROW_ID_BITS`、`CREATE TABLE ... AUTO_RANDOM`、重建表都是 DDL / 表结构变更：会改变主键或 RowID 的分布方式，影响应用对主键的假设（如依赖自增有序、范围扫描）；大表重建会占用大量写入与存储。请先在测试库验证，并确认业务能接受主键不再递增。

> ⚠️ `SET GLOBAL tidb_load_based_replica_read_threshold` 是集群级变量，改动立即影响所有新会话的读路径，Follower 读可能带来额外的 ReadIndex 开销。请记录原值以便回滚。

> ⚠️ `pd-ctl operator add split-region` 会让 PD 立刻对指定 Region 发起拆分，只是临时止血：如果热点根因是顺序递增的主键，拆分后新写入仍会落到最新的 Region，热点会再次出现。命令里的 Region 编号取自「热点 Region Top」第一条，执行前务必确认它仍然是你要处理的那个 Region。

> ⚠️ 平台从未对真实集群运行过，以上命令的效果与耗时以真实集群联调为准。

## 完整使用示例

场景：值班时收到「tikv-2 疑似写入热点」的严重告警，要判断是否真的是热点并给出处理建议。（下面的数字仅为示例。）

1. 打开「热点与调度」，横幅显示「真实数据（只读） · 集群 prod-tidb-01 · 采集于 14:32:10」。
2. 右侧「各 TiKV 写流量」：共 6 台 TiKV，tikv-2 约 214 MB/s，其余五台各约 40 MB/s（平均约 69 MB/s）。tikv-2 的条形是红色，下方黄色提示「tikv-2 写流量是均值的 3.1×，节点间不均衡」。
3. 回到热力图：tikv-2 那一行在过去 40 分钟里一直偏亮，其余几行颜色浅且平稳，说明不是整体写入上升，而是集中在一台。
4. 「热点 Region Top」里有 3 条写热点，表名都是 `event_log`，Leader 都是 tikv-2，范围形如 `table_id=130 row 9100000 … row 9300000`，且范围一条接一条向后推进，符合「顺序递增主键，新写入总落在最新 Region」的特征。
5. 到 [智能诊断](go:diag) 确认出现「tikv-2 疑似写入热点」（214 ≥ 20 MB/s 且是其余平均 40 的 5.35 倍），两处结论一致。
6. 回到「解决方案选择器」，对 `event_log` 执行 `SHOW CREATE TABLE` 判断：有整数自增主键 → 场景一（AUTO_RANDOM，新建表）；没有整数主键 → 场景二（SHARD_ROW_ID_BITS）。先在测试库验证。
7. 业务已受影响需要立即止血时，按场景四对第一条热点 Region 做 split，同时在变更记录里留下工单号。
8. 处理后回到本页，等几个刷新周期，确认 tikv-2 的条形回落、热点列表不再集中在同一个 Leader。
   - 小集群提醒：如果只有 3 台 TiKV，同样的数据平均值约 98 MB/s，倍数只有 2.2×，本页会提示「较均衡」，但 tikv-2 的条形仍会因超过 100 MB/s 变红；小集群上请以数字、条形颜色和智能诊断为准。

## 常见问题与注意

- **热点列表为空，但某台 TiKV 明显更忙**：PD 的热点统计有自己的阈值与持续时间要求，写入不均衡不一定被判定为热点 Region；以各 TiKV 写流量与智能诊断为准。
- **表名显示「—」**：没有可用的只读 SQL 账号，或 `information_schema.tidb_hot_regions` 没返回该 Region；用 `table_id` 反查表名：`SELECT TABLE_SCHEMA, TABLE_NAME FROM information_schema.tables WHERE TIDB_TABLE_ID = <table_id>;`（`<table_id>` 取自「范围」列，如 112）。
- **范围显示十六进制**：该版本的 Key 编码无法按 TiDB 的 `t` / `m` 前缀解码，属于已知的尽力而为。
- **热力图一片「没有采集到…」**：检查 Prometheus 地址（[环境与设置](go:settings) 的组件地址）、`tikv_engine_flow_bytes` 指标是否存在，以及横幅里「数据缺失」的详情。
- **点「立即刷新」提示「刷新失败」**：后端暂时取不到快照（网络、认证过期等）；运维模式下可能是会话过期，按提示重新登录。
- **想做主动的调度控制**（驱逐 Leader、调整调度限速等）：本页不提供，请使用 [节点管理](go:nodes) 的实例操作与配置变更，它们走审批和审计。
- 页面不提供流量绝对数字之外的历史对比；需要更长的历史请到 Grafana 或 TiDB Dashboard。
- PD 自身的热点调度、较新版本基于 CPU 的热点调度（`balance-hot-region-scheduler` 的 `read-priorities` 默认含 `cpu`）和 TiKV MVCC 内存引擎（v8.5.0 引入）只是卡片底部的文字提示，见官方文档；平台支持的 v7.5 / v8.1 集群没有 MVCC 内存引擎。

## 已知限制与易混淆点

| 界面写的是 | 实际是 | 建议 |
|---|---|---|
| 热力图右上有「写流量 / 读流量」两个按钮 | 真实 / 回放数据下只有写流量图，点「读流量」图不变，只有按钮高亮变化 | 读热点请看「热点 Region Top」里标「读热点」的条目，或在中控机用 `pd-ctl hot read` |
| 热力图下方写「写热点表现为阶梯状斜线、读热点表现为明亮的横线」 | 这是 Key Visualizer（Key 范围 × 时间）的读法；真实数据下本页热力图是「Store × 时间」，写热点表现为某一行持续偏亮 | 看 Key 范围维度请去 TiDB Dashboard 的「流量可视化」 |
| 热力图卡片的数字单位 MB/s，条形图的数字单位也是 MB/s | 热力图悬停数字按 1 MB = 1048576 字节换算，「各 TiKV 写流量」按 1 MB = 1,000,000 字节换算，同一时刻两处数字相差约 5% | 比较强弱即可，不要拿两处数字做精确对账 |
| 黄色「不均衡」提示（倍数 ≥ 3） | 平均值含自己，TiKV 越少越难触发；与智能诊断的「疑似写入热点」口径不同 | 以智能诊断为准 |
| 「热点 Region Top」 | 页面不显示流量数值，只按流量排序；同一 Region 同时有读写热点时只显示流量较大的一条 | 要看流量数值用 `pd-ctl hot write` / `hot read` |
| 页面名叫「热点与调度」 | 不展示 PD 调度器、operator 与调度参数 | 调度参数看 [日常运维](go:ops) 的「参数管理」 |
| 第三张卡片写「负载自适应副本读取（v7.1.0+）」 | 官方文档标注该变量自 v7.0.0 引入 | 平台支持的 v7.5 / v8.1 / v8.5 都应可用，执行前仍用 `SELECT @@GLOBAL.tidb_load_based_replica_read_threshold;` 确认变量存在 |
| 第三张卡片写「读吞吐提升 70%–200%」 | 这是官方文档给出的读热点场景数字，不是对你集群的承诺 | 以你集群的实测为准 |
| 第一张卡片只写「AUTO_RANDOM：替代自增主键」 | 新建表直接写 AUTO_RANDOM；已有表官方文档有条件地支持把 AUTO_INCREMENT 列改成 AUTO_RANDOM，不支持用 ALTER 添加或移除 AUTO_RANDOM | 优先新建表迁移，已有表以你版本的官方文档为准 |
| 第四张卡片命令里的 Region 编号 | 取自热点列表第一条；没有热点时是占位符 `<region_id>`；静态原型是演示地址与编号 | 执行前人工确认编号与 PD 地址 |
| 页面标「回放数据」或「产品演示」 | 不是你集群的实时数据 | 以顶部横幅为准，不要据此处置真实集群 |

## 官方参考

以下链接均为 docs.pingcap.com/zh 的 `stable` 版本页面，**以你集群的版本为准**（平台支持 v7.5 / v8.1 / v8.5，把链接里的 `stable` 换成 `v7.5` 等可查对应版本）。

- [热点问题处理](https://docs.pingcap.com/zh/tidb/stable/troubleshoot-hot-spot-issues)：热点场景、识别、AUTO_RANDOM、SHARD_ROW_ID_BITS、打散读热点与 `tidb_load_based_replica_read_threshold`。
- [TiDB Dashboard 流量可视化](https://docs.pingcap.com/zh/tidb/stable/dashboard-key-visualizer)：Key Visualizer 热力图的读法。
- [TiDB Dashboard 介绍](https://docs.pingcap.com/zh/tidb/stable/dashboard-overview/)：Dashboard 是什么、如何访问。
- [TiDB 数据存储](https://docs.pingcap.com/zh/tidb/stable/tidb-storage)：Region、Raft 副本、Leader。
- [TiDB 数据库的调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling)：PD 的热点调度与均衡。
- [PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control)：`hot read` / `hot write`、`operator add split-region`。
- [PD 配置文件描述](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file)：`region-schedule-limit`、`hot-region-schedule-limit`。
- [AUTO_RANDOM](https://docs.pingcap.com/zh/tidb/stable/auto-random)、[SHARD_ROW_ID_BITS](https://docs.pingcap.com/zh/tidb/stable/shard-row-id-bits)、[Split Region](https://docs.pingcap.com/zh/tidb/stable/sql-statement-split-region)：打散写热点的三种手段与 `PRE_SPLIT_REGIONS`。
- [Follower Read](https://docs.pingcap.com/zh/tidb/stable/follower-read)：从 Follower 读取的一致性与开销。
- [TIDB_HOT_REGIONS](https://docs.pingcap.com/zh/tidb/stable/information-schema-tidb-hot-regions)：本页表名的来源。
- [TiKV MVCC 内存引擎](https://docs.pingcap.com/zh/tidb/stable/tikv-in-memory-engine)：缓解多版本扫描导致的读热点（v8.5.0 起）。

## 相关页面

- [智能诊断](go:diag)：「疑似写入热点」诊断项与证据、处置建议
- [故障排查](go:trouble)：「写入变慢」向导会引导到本页，见 [故障排查帮助](help:trouble)
- [节点管理](go:nodes)：驱逐 Leader、PD 调度参数等需要审批的变更
- [日常运维](go:ops)：参数管理页展示 `region-schedule-limit` 等调度参数的当前值与建议
- [SQL 优化](go:sql)：定位读热点背后的慢 SQL
- [总览](go:overview)：全局 QPS、延迟与告警
- [环境与设置](go:settings)：Prometheus / PD 等组件地址
