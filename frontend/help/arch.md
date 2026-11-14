# 集群架构

> 💡 把整个 TiDB 集群画成一张分层拓扑图（SQL 层 / 调度层 / 存储层 / 分析层 / 同步层），点任意节点在右侧查看它的状态、资源、Region / Leader 数和关联诊断。所有角色可看（权限项 `view.overview`），本页只读：页面上的「缩容该节点」只是跳转，不会执行任何操作。只读 / 回放模式与运维模式看到的拓扑主体相同，运维模式需先登录；图最上方的「负载均衡层」随模式不同，见下文「负载均衡层」。

## 这个页面做什么

- 展示集群里每个 TiDB、PD、TiKV、TiFlash、TiCDC 实例的位置、状态和一个可切换的着色指标。
- 在右侧「节点详情」里查看单个实例的 CPU / 内存 / 磁盘、Region / Leader / 读写流量、备注、关联诊断。
- 选一个热点 Region，在图上高亮它所在的 TiKV 与 Leader。
- 在客户端与 TiDB 层之间画出「负载均衡层」：自动发现的 TiProxy，加上你在 [环境与设置](go:settings) 里手工登记的 HAProxy / LVS / 云 SLB 等；点它们看详情，并核对登记的后端与拓扑里的 TiDB 是否一致。
- 附带「组件职责速查」和三张科普卡（数据如何落到 TiKV、PD 如何调度、HTAP 如何协作）。
- 画的是**顶栏集群下拉里选中的「当前集群」**：运维模式下平台可以纳管多个集群，切换集群（会整页刷新）后这张图换成新集群的拓扑；下文「复制 display」生成的命令里的 `<集群名>` 也是当前集群的 tiup 集群名。切换方法见 [集群接入帮助](help:clusters)。

数据来源有三种（静态原型模拟 / 回放 / 真实只读采集），分辨方法见 [集群总览](help:overview) 的「如何分辨数据来源」。拓扑与实例数据来自 `tiup cluster display`、PD 的 stores / members、`information_schema.cluster_info` 与 Prometheus 主机指标；tiup 不可用时退化为从 PD / cluster_info 推导（缺少 TiCDC 等组件，横幅会提示）。

## 前置条件

| 项 | 要求 | 不满足时 |
|---|---|---|
| 角色 / 能力 | 任何角色（`view.overview`） | 运维模式需先登录 |
| 模式 | 只读 / 回放模式或运维模式均可；拓扑主体相同，「负载均衡层」不同（只有运维模式能看到手工登记的负载均衡） | 见下文「负载均衡层」 |
| 数据 | 真实 / 回放模式下拓扑来自后端采集；CPU / 内存需要 node_exporter 指标 | 缺失时对应数值显示 0%，横幅提示「缺少 Prometheus 节点指标」 |
| 「缩容该节点」之后 | 要真正缩容，需要在 [节点管理](go:nodes) 有缩容权限并走审批 | 本页无法缩容 |

## 先看懂几个词

| 术语 | 一句话解释 | 官方文档 |
|---|---|---|
| TiDB / PD / TiKV / TiFlash / TiCDC | SQL 层 / 调度与元信息 / 行存储 / 列存分析副本 / 变更数据同步 | [TiDB 整体架构](https://docs.pingcap.com/zh/tidb/stable/tidb-architecture) |
| Region、副本、Leader、Follower | 数据按 Key 范围切成 Region；每个 Region 有多个副本组成 Raft 组，Leader 对外读写，写入需多数派确认 | [TiDB 数据库的存储](https://docs.pingcap.com/zh/tidb/stable/tidb-storage) |
| 表数据如何映射到 KV | 行存成 `t{表ID}_r{行ID}` 形式的键，索引存成 `t{表ID}_i{索引ID}_{值}` 形式 | [TiDB 数据库的计算](https://docs.pingcap.com/zh/tidb/stable/tidb-computing) |
| 调度 | PD 通过 Store 心跳与 Region 心跳掌握全局，生成补副本、删副本、转移 Leader 等操作，使副本、Leader、热点、空间保持均衡 | [TiDB 数据库的调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling) |
| zone / location-labels（可用区、位置标签） | 给 TiKV 打的拓扑标签，PD 据此把同一 Region 的副本分散到不同可用区 / 主机 | [通过拓扑 label 进行副本调度](https://docs.pingcap.com/zh/tidb/stable/schedule-replicas-by-topology-labels) |
| TiFlash 与 Raft Learner | TiFlash 以 Raft Learner 角色异步复制 TiKV 的数据，列式存储，用于分析加速 | [TiFlash 简介](https://docs.pingcap.com/zh/tidb/stable/tiflash-overview) |
| TiProxy | PingCAP 官方提供的、位于客户端与 TiDB 之间的可选代理：能自动发现 TiDB server 列表，并在 TiDB 缩容、滚动升级、滚动重启时把连接迁移到其他 TiDB；也可以不用它，改用 HAProxy 等第三方负载均衡，或让应用直连 | [TiProxy 简介](https://docs.pingcap.com/zh/tidb/stable/tiproxy-overview/) |
| 负载均衡层（本页的叫法） | 图中客户端与 TiDB 之间的一层框，放「客户端实际连接的入口」：自动发现的 TiProxy + 手工登记的 HAProxy / LVS / Nginx / 云 SLB / F5 等 | HAProxy 的官方用法见 [HAProxy 在 TiDB 中的最佳实践](https://docs.pingcap.com/zh/best-practices/haproxy-best-practices/) |
| Raftstore | TiKV 里处理 Raft 的线程池，默认 2 个线程（`raftstore.store-pool-size`） | [TiKV 线程池性能调优](https://docs.pingcap.com/zh/tidb/stable/tune-tikv-thread-performance/) |

## 怎么用（按步骤）

### 查看拓扑并定位一个异常节点

1. 在左侧栏「概览」分组点「集群架构」；或在 [集群总览](go:overview) 的健康矩阵里点某一行（会自动选中该实例）。你会看到：左边大卡片「集群拓扑」，里面是分层图；右边是「节点详情」卡片，默认已选中 tikv-2（没有这个实例就选第一个 TiKV）。
2. 在卡片标题右侧的「着色指标」下拉里选 CPU / 内存 / 磁盘 / 健康状态（默认 CPU）。你会看到每个节点左侧的色条和右上角的数值同时变化；图下方的图例说明颜色：绿「健康 <70%」、黄「关注 70–85%」、红「异常 ≥85%」。
3. 找红色或黄色的节点。点它（或用 Tab 键聚焦后按 Enter / 空格）。你会看到该节点方框描边变粗，右侧「节点详情」刷新成这个实例。
4. 读「节点详情」，对照下文「节点详情」表和「异常判读与下一步」表。重点看：状态标签、CPU / 内存 / 磁盘进度条、Region / Leader / 写流量与均值的比、黄色或蓝色备注条。
5. 如果详情下方有「关联诊断」，点其中一行的标题。你会跳到 [智能诊断](go:diag)；那里还需要点「开始诊断」，对应项才会展开。
6. 需要在命令行确认实例状态：点详情底部的「复制 display」。你会看到底部弹出「已复制到剪贴板」。到 tiup 中控机上粘贴执行（命令格式见「详情卡片底部三个按钮」）。
7. 与预期不符：
  - 图上没有 TiCDC / TiFlash：见「常见问题与排查」。
  - 节点全是 0%：没有 node_exporter 指标，见 [集群总览](help:overview)。

### 在图上查看某个热点 Region 落在哪台 TiKV

1. 先按上一节选好要看的 TiKV。
2. 在「Region 副本」下拉里选一项，选项形如「Region 20871（orders）」，括号里是表名（取不到时是「—」）。选项来自 [热点与调度](go:hotspot) 里的热点 Region；集群没有热点时下拉只有「不显示」。
3. 你会看到：
  - 静态原型：该 Region 的 3 个副本节点描边变粗，Leader 带「L」圆徽标、Follower 带「F」，三者之间画出虚线，其余 TiKV 变淡。
  - 真实 / 回放模式：只知道 Leader 所在节点，所以只有 1 个节点带「L」且没有连线，其余 TiKV 变淡。
4. 选中该 Region 的节点，详情里会多一条 ◆ 提示：它是该 Region 的 Leader 还是 Follower，写入需多数派（如 2/3）确认。
5. 选回「不显示」恢复。该选择只改变显示，不影响集群。

### 从这里跳去缩容（只是跳转）

1. 选中要下线的节点，点详情底部红色的「缩容该节点」。
2. 你会看到：页面跳到 [节点管理](go:nodes) 的「缩容 / 下线」标签，该节点已被勾选，直接进入第 2 步。
3. 之后的预检、确认、审批在节点管理页完成，本页不执行任何缩容。若想放弃，直接离开该页即可，不会产生任何变更。

### 怎么知道我的集群前面有没有负载均衡

**前置条件**：想看到手工登记的负载均衡，需要在运维模式（已登录）；只读 / 回放模式只能看到自动发现的 TiProxy。先在顶栏集群下拉确认选对了集群——**每个集群各自一份登记**，在集群 A 登记的负载均衡在集群 B 的图上看不到。

1. 点左侧「集群架构」，看图最上方：「应用 / MySQL 客户端 / BI 工具」那个框的下面。
2. 对照下面三种情况：
   - **有一个标题为「负载均衡层 · 客户端入口（…）」的大框**：括号里写着有几个自动发现的 TiProxy、几个手工登记的负载均衡（例如「TiProxy 2」「手工登记 1」）。框里的每个小方框都能点开看详情，见下文「负载均衡层」。
   - **没有这个框，图下方（图例上方）有一条蓝色提示**「未发现负载均衡：若集群前有 HAProxy / LVS / 云 SLB 等，请到「环境与设置 → 负载均衡」登记。」：说明平台里既没有自动发现 TiProxy，也没有任何人登记过。**这不等于集群前面真的没有负载均衡**——HAProxy、LVS、云 SLB 等平台无法自动发现。请自己确认：看应用的数据库连接串指向哪个地址（指向 VIP / 域名，还是直接指向某台 TiDB 的 4000 端口），或问网络 / 应用同事。确实有的话，按 [环境与设置帮助](help:settings) 的「登记一条负载均衡」补登记。
   - **没有这个框，提示是「负载均衡信息读取失败：…」或「正在读取负载均衡信息…」**：前者是读取接口失败（原因写在提示里，如没有权限、后端版本较旧），图的其余部分不受影响；后者稍等几秒，页面会自动重绘。
3. 如果框里只有「TiProxy ×N · 自动发现」，说明集群是用 tiup 部署了 TiProxy 组件。**应用实际连不连它，平台看不出来**——以应用连接串里的地址为准。同时前面还有 HAProxy / 云 SLB 的话，仍需另行登记。

### 图上看到「后端与拓扑不一致」怎么办

**前置条件**：只有运维模式下、登记了「后端 TiDB 列表」的负载均衡才会做这个检查。修改登记需要「修改平台设置」（settings.edit，内置管理员）；没有该权限请把下面第 2 步看到的内容告诉管理员。

1. 在「负载均衡层」里找黄色描边的手工登记方框，它的最后一行写「手工登记 · ⚠ 后端与拓扑不一致」；图下方同时有黄色提示「有 N 个手工登记的负载均衡，其后端与拓扑里的 TiDB 不一致（图中以警示色标出）：点击该条目查看差异，或到「环境与设置 → 负载均衡」修正。」
2. 点这个方框（或 Tab 聚焦后按 Enter / 空格）。你会看到右侧抽屉「负载均衡 · <名称>」，往下看两段：
   - 「后端 TiDB（N）」里带 ⚠ 的行，写着「拓扑里没有这个 TiDB」：**登记里有、拓扑里没有**。常见原因：该 TiDB 已缩容下线但登记没改、地址或端口写错、后端写成了域名（见第 4 步）。
   - 「拓扑里有但未登记的 TiDB（N）」，每行写着「没有登记为该负载均衡的后端」：**拓扑里有、登记里没有**。常见原因：新扩容的 TiDB 还没加进登记；或者这台 TiDB 本来就不在这个负载均衡后面（例如你把读写流量分给了两套负载均衡）。
3. 先判断是登记过期，还是负载均衡本身漏配：到 HAProxy 的配置文件（或 LVS / 云 SLB 控制台）核对它**实际**转发给哪些 TiDB。
   - 登记过期（负载均衡本身是对的）：点抽屉底部「去管理（环境与设置）」，在「手工登记」表里点该行「编辑」，改「后端 TiDB 列表」后点「保存」，步骤见 [环境与设置帮助](help:settings) 的「编辑一条登记」。
   - 负载均衡本身漏配（例如新扩容的 TiDB 没加进后端）：**平台不会替你改负载均衡**，请自己在 HAProxy / LVS / 云 SLB 上加好。登记与拓扑一致后警示才会消失。
4. 域名与 IP 的陷阱：平台拿登记的后端与拓扑里 TiDB 的 `IP:端口` 做**字符串比较**，不会解析域名。如果你把后端登记成 `tidb-1.example.com:4000`，而拓扑里是 `10.0.1.11:4000`，这一台会**同时**出现在「拓扑里没有这个 TiDB」和「没有登记为该负载均衡的后端」两处。请按 IP 登记后端。
5. 这个负载均衡只负责一部分 TiDB、你不想看到警示：把该登记的「后端 TiDB 列表」留空（留空 = 不做一致性检查）。
6. 处理完后，图会在下一次快照重绘（约 30 秒）或你重新进入本页时更新；你会看到该方框描边恢复为普通颜色，副标题变成「手工登记 · 后端 N」。

### 使用示例：定位一个「告警」的 TiKV

场景：[集群总览](go:overview) 的健康矩阵里 tikv-2 显示黄色「告警」（以下是回放样例数字）。

1. 点矩阵里 tikv-2 那一行，自动跳到本页并选中 tikv-2。
2. 把「着色指标」切到「CPU」：tikv-2 的色条与数值变红（≥85%），其余 TiKV 为绿色，说明只有这一台忙。
3. 看右侧详情：Raftstore 条很高、「写 214 MB/s（均值 44）」，约是均值的 4.8 倍；黄色提示条写着备注（例如「疑似写热点」）。
4. 在「Region 副本」里选热点 Region（如 Region 20871）：tikv-2 带 L 徽标，其余 TiKV 变淡（真实 / 回放模式只会高亮 Leader 所在节点）。
5. 点「关联诊断」里的「tikv-2 疑似写入热点」，到智能诊断读证据与处置建议；本页没有「修复」按钮。
6. 需要对该节点做配置变更，点「详情 / 配置 →」打开节点抽屉，在那里走审批流程（见 [节点管理](help:nodes)）。

## 区域与参数说明

### 拓扑图

| 区域 | 内容 | 说明 |
|---|---|---|
| 顶部 | 「应用 / MySQL 客户端 / BI 工具」→「负载均衡层」→ TiDB 层 | 负载均衡层是真实数据（自动发现的 TiProxy + 手工登记），**只有静态原型里是固定的「示意」框**；没有可画的内容时整层不画，客户端箭头直接指向 TiDB 层。详见下一节「负载均衡层」 |
| SQL 层 · TiDB Server | 所有 TiDB 实例，每行最多 4 个 | 无状态，可横向扩展 |
| 调度层 · Placement Driver | 所有 PD 实例，每行最多 4 个 | TiDB 到 PD 的虚线标注「获取 TSO / 路由」 |
| 存储层 · TiKV | 按 zone 分组，每个 zone 一列纵向堆叠，最多 3 列，超出的 zone 并入最后一列 | 标题里的副本数来自 PD 的 `max-replicas`，取不到按 3 显示；zone 名取自 TiKV 的 `zone` 标签，没有标签时归为「—」一组 |
| 分析层 · TiFlash / 同步 · TiCDC | 每行最多 2 个 | TiFlash 经虚线「Raft Learner 异步复制」；TiCDC 经「变更日志」虚线 |
| 右侧「运维 & 监控」「下游 / 备份」 | Prometheus / Grafana / Alertmanager / TiDB Dashboard；Kafka / BR / Lightning、DM | 固定示意框，不反映这些组件的真实状态 |

连线：实线 = 读写请求，虚线 = 调度 / 异步复制。图不能拖动、缩放、平移，会随页面宽度自适应；要放大请用浏览器缩放（Ctrl + 滚轮）。切换亮 / 暗主题会重绘。

### 负载均衡层

位于图的最上方、「应用 / MySQL 客户端 / BI 工具」与「SQL 层 · TiDB Server」之间，表示**客户端实际连接的入口**。它由两类来源组成：

| 来源 | 是什么 | 怎么来的 |
|---|---|---|
| 自动发现（TiProxy） | 用 tiup 部署在集群里的 TiProxy 实例 | 平台读取 `tiup cluster display` 输出里角色为 `tiproxy` 的实例；不需要你做任何事 |
| 手工登记 | HAProxy / LVS / Nginx / 云负载均衡（SLB）/ F5 / tiup 之外自己部署的 TiProxy / 其他 | 管理员在 [环境与设置](go:settings) 的「负载均衡」卡片登记；每个集群各自一份，最多 20 条 |

**什么时候画、什么时候不画**

| 你所在的模式 | 画什么 | 没有可画的内容时 |
|---|---|---|
| 静态原型（页面顶栏角标「产品演示」，没有后端） | 一个固定的演示框，文字「负载均衡 · TiProxy / HAProxy / LVS（示意）」 | 总是画这个框，**不代表真实是否有负载均衡** |
| 只读 / 回放模式（有后端、没有登录） | 只画快照里自动发现的 TiProxy | 不画框，图下方提示「未发现 TiProxy。若集群前有 HAProxy / LVS / 云 SLB 等，需在运维模式下到「环境与设置 → 负载均衡」登记后才会显示在图中。」 |
| 运维模式（带登录） | 自动发现的 TiProxy + 当前集群手工登记的负载均衡 | 不画框，图下方提示「未发现负载均衡：若集群前有 HAProxy / LVS / 云 SLB 等，请到「环境与设置 → 负载均衡」登记。」 |
| 运维模式，且已勾选旧复选框「集群前有负载均衡」但没有登记具体设备 | 同上，没有东西可画 | 提示改为「平台设置里已勾选「集群前有负载均衡」，但还没有登记具体设备，图中无法画出：请到「环境与设置 → 负载均衡」登记（HAProxy / LVS / 云 SLB 等）。」 |
| 运维模式，读取接口失败 | 不画框，其余部分不受影响 | 提示「负载均衡信息读取失败：<原因>。图中暂不画负载均衡层，不影响其余部分。」；读取中显示「正在读取负载均衡信息…」 |

如何判断自己处在哪种模式，见 [集群总览](help:overview) 的「如何分辨数据来源」。

**大框与标题**：框左上角标题形如「负载均衡层 · 客户端入口（TiProxy 2 + 手工登记 1）」，括号里只列出存在的那几类。

**自动发现的 TiProxy（框里的灰色小分组）**

| 元素 | 含义 | 说明 |
|---|---|---|
| 分组标题「TiProxy ×N · 自动发现」 | N 个 TiProxy 实例 | 若有不是「运行中」的，标题后追加红字「· M 个非运行中」 |
| 每个小条：圆点 + `主机:SQL 端口` + 状态文字 | 客户端连接该 TiProxy 用的地址，例如 `10.20.1.41:6000` | 圆点与状态文字：绿「运行中」、黄「告警」、红「已停止」、蓝「扩容中 / 下线中」；非运行中的状态文字加粗并带颜色 |
| 点击小条 | 打开右侧抽屉「TiProxy · <实例编号>」 | 字段见下表 |

TiProxy 详情抽屉（只读，不能编辑）：

| 字段 | 含义 | 看数要点 |
|---|---|---|
| 实例 | 平台生成的编号，如 tiproxy-1 | 按「主机:端口」排序编号，不是 tiup 的节点 ID |
| 主机 | TiProxy 所在主机 IP | tiup 拓扑里 `tiproxy_servers` 的 host，官方说明该处**只支持 IP，不支持域名** |
| SQL 端口 | 客户端连接端口，旁边写「客户端连接地址 主机:端口」，可点「复制」 | 官方拓扑模板示例是 `port: 6000` |
| API 端口 | TiProxy 的 HTTP 状态端口 | 官方模板示例是 `status_port: 3080`；取不到显示「—（未采集）」 |
| 状态 | 带圆点的中文状态 +「原始值」（up / down / offline / joining） | 只反映 tiup 的实例状态，**不代表业务连接能通过它** |
| 版本 | TiProxy 的版本 | 通常显示「—（tiup display 不给出单实例版本）」；只有集群信息里恰好能查到该实例时才有值，平台**不会拿 TiDB 版本冒充** |
| 来源 | 「自动发现（tiup cluster display）」 | — |

抽屉里的蓝色说明：自动发现的 TiProxy 为只读信息，平台目前**不把它算作**「集群前有负载均衡」来决定缩容 / 维护 TiDB 时的人工摘除步骤（原因见 [环境与设置帮助](help:settings) 的「与原有复选框的关系」）。

> 💡 `tiup cluster display` 里 TiProxy 一行的实际输出格式，官方文档没有给出样例。平台按拓扑模板推断（角色 `tiproxy`，端口列 `6000/3080`），**以真实集群为准**。如果你的集群里明明部署了 TiProxy 却没出现在图上，可能是真实输出里的角色名与推断不同（平台只认角色 `tiproxy`），或 tiup 不可用、拓扑退化为从 PD 推导（此时没有 TiProxy）；这两种情况平台都不会报错，只是少了这一块，请在中控机执行 `tiup cluster display <集群名>` 核对（`<集群名>` 见本页卡片标题「集群拓扑 · …」）。

> ⚠️ **tiup 之外部署的 TiProxy、以及 HAProxy、LVS、云 SLB、F5、Nginx 等，平台无法自动发现**——它们不在 `tiup cluster display` 里。图上看不到它们，不代表不存在；要在图上显示，必须在 [环境与设置](go:settings) 手工登记。

**手工登记的负载均衡（框里的白色方框）**

| 元素 | 含义 | 说明 |
|---|---|---|
| 左上角蓝色小标签 | 类型：HAProxy / LVS / Nginx / SLB / F5 / TiProxy / 其他 | 「SLB」即云负载均衡；类型 TiProxy 用于 tiup 之外自己部署的 TiProxy |
| 粗体名称、下一行 `入口地址` | 你登记的名称与客户端连接的 VIP / 域名 : 端口 | 过长会被截断，详情里看全 |
| 最后一行「手工登记 · 后端 N」 | 来源是手工登记，已登记 N 个后端 TiDB | 没登记后端时写「手工登记 · 未登记后端」 |
| 最后一行变黄加粗「手工登记 · ⚠ 后端与拓扑不一致」，方框描边变黄变粗、左侧色条变黄 | 登记的后端与拓扑里的 TiDB 对不上 | 处理方法见上文「图上看到「后端与拓扑不一致」怎么办」 |
| 点击方框 | 打开右侧抽屉「负载均衡 · <名称>」 | 字段见下表 |

手工登记详情抽屉：

| 字段 | 含义 | 说明 |
|---|---|---|
| 顶部标签 | 类型、「手工登记」、一致性状态 | 状态四选一：「后端与拓扑一致」（绿）/「后端与拓扑不一致」（黄）/「未登记后端，不做一致性检查」/「尚无拓扑快照，无法对照」（黄，稍后刷新） |
| 类型、名称 | 登记时填的 | — |
| id | 平台内唯一标识，创建后不能改 | 审计日志里的目标就是它 |
| 入口地址 | 客户端连接的 `主机:端口`，旁有「复制」 | — |
| 来源 | 「手工登记（平台元数据）」 | 登记只是平台里的一条记录 |
| 备注 | 登记时填的备注 | 没填显示「—」 |
| 创建 / 最近更新 | 时间；最近更新后面带操作人 | — |
| 后端 TiDB（N） | 登记的每个后端；有拓扑可对照时，✔ + 「在拓扑内」或 ⚠ + 「拓扑里没有这个 TiDB」 | 比较的是 `主机:TiDB SQL 端口`（通常 4000），主机名不区分大小写，不解析域名 |
| 拓扑里有但未登记的 TiDB（N） | 拓扑里有、但这个登记的后端里没有的 TiDB，每行写「没有登记为该负载均衡的后端」 | 只在有差异时出现 |
| 黄色提示「后端与拓扑不一致：新增 / 下线 TiDB 后请同步更新该登记…」 | 提醒登记要跟着拓扑更新 | 否则缩容 / 维护时容易漏摘或误判 |
| 底部按钮 | 「去管理（环境与设置）」跳到登记卡片；「关闭」 | 在这里不能直接编辑 |

抽屉末尾一行小字「登记是平台元数据，不会改动 <类型> 本身」——**平台不连接你的 HAProxy / LVS / TiProxy，不探测它们是否健康，也不会替你摘除 / 加回后端**。

**框里放不下时的折叠「+N」**：自动发现的 TiProxy 最多画 9 格，超过时前 8 个正常画，第 9 格变成虚线方框「+N 个 TiProxy（查看全部）」；手工登记最多画 12 格，超过时前 11 个正常画，第 12 格是虚线方框「+N 个负载均衡（查看全部）」。点虚线方框（或聚焦后按 Enter / 空格），右侧抽屉「全部负载均衡」列出全部条目（类型 / 名称与入口 / 状态 / 来源），每行「详情」打开对应抽屉。

**刷新节奏**：负载均衡信息读取后缓存 30 秒；每次进入本页、在「环境与设置」做过登记 / 编辑 / 删除后立即重新读取。登记的一致性对照用的是平台缓存的拓扑快照（页面顶部横幅的「采集于 hh:mm:ss」），扩缩容后要等下一次快照才会反映；急用点横幅的「立即刷新」。

### 节点方框与图例

| 元素 | 含义 | 判定 |
|---|---|---|
| 方框第一行 | 实例编号，如 tikv-2 | 编号由平台按「角色 → 主机:端口」排序生成，不等于 PD 的 Store ID（Store ID 在节点抽屉的概览里） |
| 第二行 | 主机 IP | — |
| 第三行 | TiKV：`Region 数 · L Leader 数`；PD：Leader / Follower；TiFlash：`Learner Region 数`；TiDB：`QPS`；TiCDC：`lag 延迟` | 数值 ≥1 万显示为「x.x 万」。真实模式下 TiCDC 的 lag 未采集，显示「lag —」 |
| 右上角 | 「着色指标」对应的百分比；选「健康状态」时显示一个状态圆点；扩容中 / 下线中 / 已停止的节点改为显示状态文字 | — |
| 左侧色条 | 绿「健康 <70%」、黄「关注 70–85%」、红「异常 ≥85%」 | CPU / 内存 / 磁盘三种指标采用同一组阈值（右侧详情的磁盘进度条用 75% 作黄色起点，与图上略有出入） |
| 蓝色虚线框 | 图例写「扩容中 / 下线中」；代码对「已停止」也画虚线框 | 已停止的节点同时红色色条、整体半透明 |
| 方框描边加粗（蓝） | 当前被选中的节点，或被选中的 Region 的副本所在节点 | — |
| L / F 圆徽标 | 选中 Region 后：L = 该 Region 的 Leader 副本所在节点，F = Follower 副本 | 与第三行里 TiKV 的「L」（Leader 数）含义不同 |
| 变淡的 TiKV | 选中 Region 后不持有该 Region 的 TiKV | — |

状态与颜色（「健康状态」着色，或节点为非运行状态时）：

| 状态 | 文字 | 颜色 |
|---|---|---|
| up | 运行中 | 绿 |
| warn | 告警 | 黄 |
| down | 已停止 | 红 |
| joining | 扩容中 | 蓝 |
| offline | 下线中 | 蓝 |

状态的判定规则（磁盘 ≥85% / CPU ≥85% / 内存 ≥80% 即「告警」；tiup 与 PD 状态映射）见 [集群总览](help:overview) 的「实例健康矩阵」。

### 顶部两个下拉框

| 字段 / 选项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 着色指标 | 决定色条颜色与右上角数值 | CPU（默认）/ 内存 / 磁盘 / 健康状态 | 只改变显示。CPU、内存是**主机**使用率；真实 / 回放模式下 TiDB、PD、TiCDC 没有磁盘数据，选「磁盘」时显示 0%（绿色），不是真的空 |
| Region 副本 | 选一个 Region，在图上标出它的副本分布 | 「不显示」（默认）/ 热点 Region 列表 | 见上文「在图上查看某个热点 Region」 |

### 节点详情（右侧卡片）

打开页面时默认选中 tikv-2（没有则第一个 TiKV）；30 秒自动刷新后保持选中。

| 字段 | 含义 | 口径 / 阈值 | 看数要点 |
|---|---|---|---|
| 标题 + 状态标签 + 组件名 | 实例编号、运行状态、组件类型 | 状态见上表 | — |
| 主机:端口 · 可用区 · 版本 | 服务地址、AZ、版本号 | AZ 取 zone 标签，没有则沿用同主机上已知的 zone；版本显示的是**集群版本**，不是该实例自己的版本 | 滚动升级期间实例版本可能不同，需到节点管理或 `tiup cluster display` 核对 |
| CPU / 内存 | 进度条 + 百分比 | 所在主机的使用率；≥70% 黄、≥85% 红 | 同主机多实例读数相同 |
| 磁盘 | 进度条 + 百分比 | TiKV / TiFlash：Store 的（容量 − 可用）/ 容量；≥75% 黄、≥85% 红 | ≥80% 智能诊断给出严重项；PD 的 `low-space-ratio` 默认 0.8，超过后 PD 会尽量避免往该节点迁移数据（[PD 配置文件](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file)） |
| Raftstore（仅 TiKV） | Raftstore 线程 CPU | `sum(rate(tikv_thread_cpu_seconds_total{name=~"raftstore_.*"}[1m]))` × 100，即「占用核数 × 100」 | 进度条在 100% 处封顶，但 `store-pool-size` 默认 2（上限约 200%）。官方 TiKV Details 面板说明建议 Raft store CPU 通常低于 `raftstore.store-pool-size` 的 80%（默认约 160%），见 [TiKV Grafana 面板](https://docs.pingcap.com/zh/tidb/stable/grafana-tikv-dashboard)；以你集群的配置为准 |
| Region / Leader / 写 / 读（仅 TiKV） | Region 数、Leader 数、写流量、读流量 | Region、Leader 来自 PD Store；写流量取 `tikv_engine_flow_bytes`（WAL）速率换算为 MB/s，括号里是全体 TiKV 的平均值 | 单台 Leader 数或写流量显著高于均值是热点 / 失衡信号。**真实 / 回放模式下「读」没有采集，恒显示 0 MB/s** |
| QPS / 连接数（仅 TiDB） | 该 TiDB 的 QPS 与连接数 | `sum(rate(tidb_server_query_total[1m])) by (instance)` 等，最近 3 点平均 | 各 TiDB 差距过大说明负载均衡不均 |
| 角色（仅 PD） | Leader（负责 TSO 与调度）或 Follower | tiup display 的 `L` 标记或 PD members | — |
| 黄 / 蓝提示条 | 实例备注 | 采集端自动生成：「磁盘使用率 86% ≥ 85%」「CPU … ≥ 85%」「内存 … ≥ 80%」「PD 中未找到对应 Store」等 | 「PD 中未找到对应 Store」多见于刚扩容尚未注册，或 tiup 与 PD 不一致 |
| ◆ Region 提示 | 当前选中的 Region 在该节点上是 Leader 还是 Follower，并说明写入需多数派确认（如 2/3） | 副本数取 PD 配置 | — |
| 组件职责说明 | 该组件的一句话职责 | 固定文案 | — |
| 关联诊断 | 与该实例相关的诊断项，带严重度标签，点击跳到 [智能诊断](go:diag) | 按诊断项「范围」文字中是否包含该实例编号匹配 | 没有则不显示这一块 |

### 异常判读与下一步

| 你看到 | 可能意味着 | 下一步 |
|---|---|---|
| 节点红色「已停止」或蓝色「下线中」 | 进程退出、主机故障、网络隔离，或计划内下线。TiKV 失联超过 PD 的 `max-store-down-time`（默认 30 分钟）后 PD 开始补副本 | 点「复制 display」在中控机确认状态；到 [节点管理](go:nodes) 的「节点实例」处理；先确认是否计划内维护 |
| 磁盘红（≥85%）或智能诊断提示 ≥80% | 空间不足或数据倾斜 | 看其余 TiKV 是否也高：整体高就扩容，只有一台高就看 Region / Leader 是否失衡，见 [智能诊断](go:diag) 磁盘项 |
| 某台 TiKV 的 Leader 数 / 写流量明显高于「均值」 | 热点或调度失衡 | 看 [热点与调度](go:hotspot)；写流量 ≥20 MB/s 且 ≥其余平均 3 倍时诊断页会报「疑似写入热点」 |
| 某台 TiKV 的 Raftstore 长期接近 store-pool-size 的 80% | Raftstore 线程成为瓶颈 | 先排查热点（单 Region 热点扩容无法缓解）；再考虑官方的线程池调优 |
| 各 TiKV 的 Region 数相差较多（最多 / 最少 >1.25） | 刚扩缩容、调度限速或某 Store 水位高 | 诊断页会报「Region 分布不均」，在 [热点与调度](go:hotspot) 查看调度；也可用官方 [pd-ctl](https://docs.pingcap.com/zh/tidb/stable/pd-control) 的 `store`、`config show` 命令核对 |
| 各 TiDB 的 QPS 差距大 | 负载均衡不均 | 检查 HAProxy / LVS / TiProxy 配置。图上的「负载均衡层」只显示入口与你登记的后端，**不反映它实际怎么转发**；先点开登记详情核对后端是否齐全（见「图上看到「后端与拓扑不一致」怎么办」），再到负载均衡本身排查 |
| 备注「PD 中未找到对应 Store」 | 新扩容实例尚未注册，或 tiup 与 PD 不一致 | 等几分钟刷新；仍无，在中控机 `tiup cluster display` 与 pd-ctl `store` 对照 |

### 详情卡片底部三个按钮

| 按钮 | 做什么 | 会不会改动集群 |
|---|---|---|
| 详情 / 配置 → | 打开右侧「节点抽屉」，有「概览」「配置变更」「变更历史」「操作」四个标签；概览含部署 / 数据 / 日志目录、拓扑标签、Store ID | 打开本身只读；抽屉里的配置变更与操作属于 [节点管理](go:nodes) 的流程，运维模式才真正走审批与执行，只读模式仍是前端模拟。「进程 PID」「已运行」在真实模式显示「未采集」，静态原型显示假数据 |
| 缩容该节点 | 跳转到 [节点管理](go:nodes)「缩容 / 下线」页，已选中该节点并进入第 2 步 | **本页不执行任何缩容** |
| 复制 display | 复制 `tiup cluster display <集群名> -N 主机:端口` 到剪贴板 | 只复制，不执行；该命令本身是只读查询 |

占位符：`<集群名>` 取自页面卡片标题「集群拓扑 · …」里的集群名，也是 [集群总览](go:overview)「集群信息」的名称（例如 prod-tidb-01）；`主机:端口` 取自详情第二行（例如 10.0.1.31:20160，以页面为准）。复制出来的命令已填好这两项，不需要自己替换。命令的官方说明见 [tiup cluster display](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-display)。

### 组件职责速查 与 科普卡片

固定文案：各组件一句话职责，并链到官方「计算层说明 ↗」「调度说明 ↗」；「数据如何落到 TiKV」「PD 如何调度」「HTAP 如何协作」三张卡为静态说明（其中 Region 副本数取自 PD 配置）。它们不随集群状态变化。

## 执行后果与风险

> ⚠️ 本页没有任何会修改集群的操作：选择指标、选择 Region、点击节点、复制命令都只影响浏览器里的显示。

> ⚠️ 「缩容该节点」是跳转按钮，红色只是提示后续操作风险较高。缩容 TiKV 会触发 Region 迁移、占用网络与磁盘 I/O，需在 [节点管理](go:nodes) 里完成预检并走审批；回放模式下是模拟执行器，不是真实执行。平台从未对真实集群运行过。

> ⚠️ 「详情 / 配置 →」打开的抽屉里含可提交的配置变更与实例操作；在那里的操作才有后果，请先阅读 [节点管理](help:nodes)。

## 已知限制与易混淆点

| 界面写的 | 实际是 | 怎么办 |
|---|---|---|
| 「Region 副本」下拉 + 「F」徽标 | 真实 / 回放模式下列表来自热点 Region，且只知道 Leader，只会高亮 1 个节点、没有 Raft 连线；Follower 位置不显示 | 要看完整副本分布用官方 pd-ctl 的 `region` 命令（见 [PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control)） |
| 选「磁盘」后 TiDB、PD 是绿色 0% | 这些组件的磁盘没采集 | 不要当成「很空」 |
| 详情里的「版本」 | 集群版本，不是实例版本 | 滚动升级期间用 `tiup cluster display` 核对 |
| 「读」0 MB/s | 真实 / 回放模式没有采集读流量 | 读流量看 Grafana |
| CPU / 内存 | 所在主机的使用率；QPS、写流量、Raftstore 在 Prometheus 里按主机取值，同机部署多个 TiDB / TiKV 时读数会重复或只反映其中一个 | 同机多实例的场景以 Grafana 为准 |
| 图例「蓝色虚线框：扩容中 / 下线中」 | 代码对「已停止」也画虚线框 | 以状态文字为准 |
| 右侧详情磁盘条黄色起点 75%，图上色条 70% | 两套刻度略有出入 | 以 ≥85% 红为准 |
| Raftstore 进度条 | 封顶 100%，而 `store-pool-size` 默认 2，满载约 200% | 看百分比数字，不要只看条 |
| 关联诊断 | 按文字包含匹配，tikv-1 可能同时匹配到范围写着 tikv-10、tikv-12 的诊断项（疑似问题） | 集群较大时点进诊断页核对范围 |
| 「进程 PID」「已运行」 | 抽屉概览里真实模式显示「未采集」，静态原型是假数据 | 在主机上自行查看 |
| 图中右侧的「运维 & 监控」「下游 / 备份」 | 固定示意框，不反映真实部署与状态 | 以 [节点管理](go:nodes) 与 Grafana 为准 |
| 图上方的「负载均衡 · TiProxy / HAProxy / LVS（示意）」 | **只有静态原型**（无后端）才是固定示意框；有后端时负载均衡层是真实数据：自动发现的 TiProxy + 手工登记，没有可画的内容时整层不画 | 见上文「负载均衡层」；看不到框不代表没有负载均衡，HAProxy / LVS / 云 SLB 必须手工登记才会显示 |
| 负载均衡层里 TiProxy 圆点是绿色「运行中」 | 只表示 `tiup cluster display` 里该实例状态为 Up，平台不探测 TiProxy / HAProxy 的健康，也不测试客户端能不能经它连上 TiDB | 业务是否可连以应用侧与 Grafana / 负载均衡自己的监控为准 |
| 手工登记框里写的后端 | 是登记簿里的文字，不是从 HAProxy / LVS 读来的；平台不连接这些设备 | 负载均衡配置改了，要同步更新登记，否则会出现「后端与拓扑不一致」或漏报 |
| 「后端与拓扑不一致」 | 只比较登记的后端与拓扑里 TiDB 的 `主机:SQL 端口`，不识别域名、不比较权重 / 健康检查配置；后端留空的登记不做检查 | 后端按 IP 登记；见上文「图上看到「后端与拓扑不一致」怎么办」 |
| 图上有 TiProxy | 不会因此在缩容 / 维护 TiDB 时多出「从负载均衡摘除」的人工确认步骤；该步骤只由「集群前有负载均衡」复选框或手工登记触发 | 见 [环境与设置帮助](help:settings) 的「与原有复选框的关系」与 [节点管理帮助](help:nodes) |
| 每 30 秒自动刷新 | 按代码，重绘后页面回到顶部，选中节点保持不变；弹出抽屉或下拉框正在聚焦时不重绘 | 读详情时若被拉回顶部属正常 |

## 常见问题与排查

- **图上没有 TiCDC / TiFlash**：tiup 拓扑不可用时只能从 PD 推导，缺少 TiCDC 等组件；看顶部横幅的数据缺失说明，并确认中控机上 `tiup cluster display <集群名>` 能正常执行。
- **图上没有负载均衡框 / 我有 HAProxy 但图上没有 / 看到「后端与拓扑不一致」**：见 [常见问题](help:faq) 的「负载均衡」小节，以及上文两节操作说明。
- **所有 CPU / 内存都是 0%**：没有 node_exporter 指标，见 [集群总览](help:overview)。
- **「Region 副本」下拉是空的**：集群当前没有热点 Region。
- **找不到「启动时间」「进程 PID」「实例版本」**：本页没有这些字段，见上表。

## 官方参考

以下链接均已打开核实，版本以你集群的为准（链接指向 stable 版）。

- [TiDB 整体架构](https://docs.pingcap.com/zh/tidb/stable/tidb-architecture)：各组件职责
- [TiDB 数据库的存储](https://docs.pingcap.com/zh/tidb/stable/tidb-storage)：Region、Raft、Leader / Follower、默认 Region 大小
- [TiDB 数据库的计算](https://docs.pingcap.com/zh/tidb/stable/tidb-computing)：表数据与索引到 KV 的映射
- [TiDB 数据库的调度](https://docs.pingcap.com/zh/tidb/stable/tidb-scheduling)：PD 调度
- [通过拓扑 label 进行副本调度](https://docs.pingcap.com/zh/tidb/stable/schedule-replicas-by-topology-labels)：zone、location-labels
- [TiFlash 简介](https://docs.pingcap.com/zh/tidb/stable/tiflash-overview)：Raft Learner 异步复制
- [TiKV 线程池性能调优](https://docs.pingcap.com/zh/tidb/stable/tune-tikv-thread-performance/)：Raftstore 线程池默认 2
- [TiKV Grafana 面板](https://docs.pingcap.com/zh/tidb/stable/grafana-tikv-dashboard)：Raft store CPU 经验阈值
- [PD 配置文件](https://docs.pingcap.com/zh/tidb/stable/pd-configuration-file)：`low-space-ratio`、`max-store-down-time`
- [PD Control](https://docs.pingcap.com/zh/tidb/stable/pd-control)：`store`、`region`、`config show`、`hot read / hot write`
- [tiup cluster display](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-display)：查看实例状态（官方只列出输出的列名，没有 TiProxy 的输出样例）
- [TiProxy 简介](https://docs.pingcap.com/zh/tidb/stable/tiproxy-overview/)：TiProxy 是什么；自动发现 TiDB server 列表；TiDB 缩容、滚动升级、滚动重启时迁移连接；`graceful-wait-before-shutdown`
- [TiProxy 部署拓扑](https://docs.pingcap.com/zh/tidb/stable/tiproxy-deployment-topology/)：`tiproxy_servers` 的拓扑模板，示例 `port: 6000`、`status_port: 3080`，实例的 `host` 目前只支持 IP
- [HAProxy 在 TiDB 中的最佳实践](https://docs.pingcap.com/zh/best-practices/haproxy-best-practices/)：用 HAProxy 做 TiDB 的 TCP 负载均衡（这是 `best-practices` 路径，不分 stable / 版本号，按你的版本酌情参考）
- [TiDB Dashboard 简介](https://docs.pingcap.com/zh/tidb/stable/dashboard-intro)：内置在 PD 的图形化界面

## 相关页面

- [集群总览](go:overview)：健康矩阵（点击行跳到本页）
- [智能诊断](go:diag)：节点相关的诊断项
- [热点与调度](go:hotspot)：热点 Region 详情
- [节点管理](go:nodes)：实例详情、缩容 / 扩容、主机维护
- [环境与设置](go:settings)：「负载均衡」卡片，登记 / 编辑 / 删除手工登记的负载均衡
- 帮助：[集群总览](help:overview)、[节点管理](help:nodes)、[环境与设置](help:settings)
