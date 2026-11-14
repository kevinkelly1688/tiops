# 样例数据包审阅清单

- 集群：prod-tidb-01（v8.5.2）
- 采集时间：2026-10-05T13:42:30Z
- 工具：tidbops-fixtures 0.1.0
- 脱敏选项：SQL 字面量=true · 密钥字段=true · IP=false

## 发送前请确认

1. 本数据包全部由**只读**操作产生（见下方“只读声明”）。
2. 打开 `data/sql/` 下的文件，确认 SQL 文本里没有手机号、身份证、订单号等业务数据（应只剩 `?`）。
3. 打开 `data/tiup/meta.yaml`，确认没有密码、密钥内容（应显示为 `"***"`）。
4. 如不希望暴露真实 IP，请用 `--mask-ip` 重新导出。
5. 用 `tidbops-fixtures verify <dir>` 校验完整性。

## 只读声明

- HTTP：仅 GET
- 命令：`free -m`
- 命令：`hostname -I`
- 命令：`nproc`
- 命令：`timedatectl`
- 命令：`tiup --version`
- 命令：`tiup cluster --version`
- 命令：`tiup cluster display prod-tidb-01`
- 命令：`tiup cluster list`
- 命令：`uname -r`
- 文件：`/etc/os-release`
- 文件：`/home/tidb/.tiup/storage/cluster/clusters/prod-tidb-01/meta.yaml`
- SQL：`SELECT * FROM information_schema.cluster_info LIMIT 500`
- SQL：`SELECT * FROM information_schema.tidb_hot_regions LIMIT 200`
- SQL：`SELECT DIGEST, SCHEMA_NAME, EXEC_COUNT, AVG_LATENCY, MAX_LATENCY, SUM_LATENCY, AVG_MEM, SUBSTRING(DIGEST_TEXT, 1, 2000) AS DIGEST_TEXT FROM information_schema.c…`
- SQL：`SELECT Digest, tidb_decode_plan(Plan) AS Plan FROM information_schema.cluster_slow_query WHERE Time > NOW() - INTERVAL 1 HOUR AND Plan != '' ORDER BY Query_time…`
- SQL：`SELECT TYPE, INSTANCE, `KEY`, VALUE FROM information_schema.cluster_config WHERE `KEY` IN ('storage.block-cache.capacity','raftstore.store-pool-size','raftstore…`
- SQL：`SELECT Time, Digest, Plan_digest, DB, User, Query_time, Parse_time, Compile_time, Process_time, Wait_time, Total_keys, Process_keys, Result_rows, Mem_max, Index…`
- SQL：`SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.session_variables WHERE VARIABLE_NAME IN ('tidb_mem_quota_query','tidb_enable_prepared_plan_cache',…`
- SQL：`SELECT tidb_version()`

## 条目

| 名称 | 结果 | 大小 | 用途 |
|---|---|---|---|
| alert/alerts | 成功 | 670 | Alertmanager 活跃告警 |
| ctl/free | 成功 | 135 | 内存 |
| ctl/hostname | 成功 | 10 | 中控机主机名与地址 |
| ctl/kernel | 成功 | 22 | 内核版本 |
| ctl/nproc | 成功 | 2 | CPU 核数 |
| ctl/os-release | 成功 | 115 | 中控机操作系统 |
| ctl/timedatectl | 成功 | 140 | 时区与时间同步 |
| pd/cluster | 成功 | 48 | 集群基本信息 |
| pd/config | 成功 | 270 | PD 配置（调度、副本） |
| pd/health | 成功 | 215 | PD 健康 |
| pd/hot-read | 成功 | 269 | 读热点 Region |
| pd/hot-write | 成功 | 269 | 写热点 Region |
| pd/members | 成功 | 451 | PD 成员与 Leader |
| pd/regions-readflow | 成功 | 331 | 读流量 Top 50 Region（含起止 Key，用于解码热点范围） |
| pd/regions-sample | 成功 | 1515 | Region 抽样 |
| pd/regions-writeflow | 成功 | 627 | 写流量 Top 50 Region（含起止 Key，用于解码热点范围） |
| pd/stats-region | 成功 | 98 | Region 统计 |
| pd/stores | 成功 | 5382 | TiKV / TiFlash Store 列表与容量 |
| pd/version | 成功 | 25 | PD 版本 |
| prom/connections | 成功 | 4551 | TiDB 连接数 |
| prom/dbtime-compile | 成功 | 4429 | DB Time：编译 Compile |
| prom/dbtime-exec-distsql | 成功 | 4429 | 执行阶段细分：分布式 SQL 请求 |
| prom/dbtime-exec-kv | 成功 | 35090 | 执行阶段细分：TiKV 客户端请求（按请求类型） |
| prom/dbtime-exec-tso | 成功 | 4429 | 执行阶段细分：TSO 等待 |
| prom/dbtime-execute | 成功 | 4429 | DB Time：执行 Execute |
| prom/dbtime-parse | 成功 | 4429 | DB Time：解析 Parse |
| prom/dbtime-token | 成功 | 4711 | DB Time：取令牌 Get Token（该指标单位为微秒） |
| prom/dbtime-type | 成功 | 39488 | DB Time：按 SQL 类型（不含内部 SQL） |
| prom/forecast-capacity | 成功 | 13341 | 容量预测：全部 TiKV 磁盘容量字节（7 天，每小时） |
| prom/forecast-regions | 成功 | 11820 | 容量预测：Region 总数（各 TiKV 的 Leader 数之和，7 天，每小时） |
| prom/forecast-used | 成功 | 13341 | 容量预测：全部 TiKV 已用字节（7 天，每小时） |
| prom/node-cores | 成功 | 75324 | 各主机 CPU 核数（主机规格） |
| prom/node-cpu | 成功 | 53455 | 各主机 CPU 使用率（%） |
| prom/node-disk-size | 成功 | 86548 | 各主机最大文件系统容量字节（主机规格，近似数据盘容量） |
| prom/node-mem | 成功 | 53455 | 各主机内存使用率（%） |
| prom/node-mem-total | 成功 | 85145 | 各主机内存总量字节（主机规格） |
| prom/node-os-info | 成功 | 77118 | 各主机操作系统名称（主机规格，较新的 node_exporter 才有，值在标签里） |
| prom/node-uname | 成功 | 77136 | 各主机内核版本与主机名（主机规格，值在标签里） |
| prom/p99 | 成功 | 4429 | 查询 P99 延迟（秒） |
| prom/pd-tso-p99 | 成功 | 4429 | PD TSO 请求 P99（秒） |
| prom/qps | 成功 | 4673 | 每秒查询数 |
| prom/tidb-conn-instance | 成功 | 13618 | 各 TiDB 的连接数 |
| prom/tidb-qps-instance | 成功 | 13801 | 各 TiDB 的 QPS |
| prom/tikv-cpu | 成功 | 26413 | 各 TiKV 的 CPU 使用 |
| prom/tikv-raftstore-cpu | 成功 | 26413 | 各 TiKV 的 Raftstore 线程 CPU |
| prom/tikv-region-count | 成功 | 26413 | 各 TiKV 的 Region 数 |
| prom/tikv-store-capacity | 成功 | 30805 | 各 TiKV 磁盘容量字节 |
| prom/tikv-store-used | 成功 | 30805 | 各 TiKV 磁盘已用字节 |
| prom/tikv-write-bytes | 成功 | 29036 | 各 TiKV 的写入字节/秒 |
| sql/cluster-config | 成功 | 3587 | 配置目录中全部实例级配置项在各实例上的当前值（配置变更页、参数建议与配置漂移检测） |
| sql/cluster-info | 成功 | 580 | 集群各组件实例信息 |
| sql/hot-regions | 成功 | 96 | 热点 Region |
| sql/slow-plan | 成功 | 1589 | 慢查询的执行计划（tidb_decode_plan 解码，仅字符串字面量脱敏） |
| sql/slow-query | 成功 | 1598 | 最近 1 小时最慢的 200 条查询 |
| sql/statements-summary | 成功 | 1018 | 累计耗时 Top 100 的 SQL 摘要 |
| sql/variables | 成功 | 206 | 关键系统变量的当前值（参数建议页） |
| sql/version | 成功 | 86 | TiDB 版本（交叉验证 tiup 的版本） |
| tidb/info-all | 成功 | 145 | TiDB 实例注册信息 |
| tidb/status | 成功 | 93 | TiDB 实例状态与版本 |
| tiup/cluster-display | 成功 | 3058 | 集群拓扑与状态 |
| tiup/cluster-list | 成功 | 333 | 中控机管理的集群列表 |
| tiup/cluster-version | 成功 | 26 | tiup cluster 组件版本 |
| tiup/meta | 成功 | 444 | tiup 拓扑元数据（用于地址探测） |
| tiup/version | 成功 | 50 | tiup 版本 |
