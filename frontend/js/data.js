// @author: kevinkelly
// @license: Apache License 2.0
// @file: data.js
// @time: 2026/10/28

/* 模拟数据：所有数字均为演示用途，真实平台应来自 Prometheus / PD API / information_schema / TiDB Dashboard API */
const DOC = 'https://docs.pingcap.com/zh/tidb/stable/';
let CLUSTER = 'prod-tidb-01';
let VERSION = 'v8.5.2';

const NODES = [
  // TiDB（无状态 SQL 层）
  {id:'tidb-1',type:'tidb',host:'10.0.1.11',port:4000,az:'az1',status:'up',cpu:38,mem:52,disk:41,qps:3210,conn:212,note:''},
  {id:'tidb-2',type:'tidb',host:'10.0.1.12',port:4000,az:'az2',status:'up',cpu:41,mem:58,disk:39,qps:3340,conn:231,note:''},
  {id:'tidb-3',type:'tidb',host:'10.0.1.13',port:4000,az:'az3',status:'warn',cpu:63,mem:83,disk:44,qps:3980,conn:288,note:'内存使用率 83%，近 1h 出现 expensive_query'},
  // PD
  {id:'pd-1',type:'pd',host:'10.0.1.21',port:2379,az:'az1',status:'up',cpu:9,mem:21,disk:18,leader:true,regionCount:48210,note:'PD Leader'},
  {id:'pd-2',type:'pd',host:'10.0.1.22',port:2379,az:'az2',status:'up',cpu:6,mem:19,disk:18,leader:false,note:''},
  {id:'pd-3',type:'pd',host:'10.0.1.23',port:2379,az:'az3',status:'up',cpu:5,mem:19,disk:17,leader:false,note:''},
  // TiKV
  {id:'tikv-1',sid:1,type:'tikv',host:'10.0.2.11',port:20160,az:'az1',status:'up',cpu:46,mem:61,disk:62,regions:16120,leaders:5380,wr:38,rd:61,raftCpu:44,note:''},
  {id:'tikv-2',sid:2,type:'tikv',host:'10.0.2.12',port:20160,az:'az1',status:'warn',cpu:89,mem:66,disk:64,regions:16090,leaders:5410,wr:214,rd:58,raftCpu:92,note:'Raftstore CPU 92%，写流量为集群均值 4.8×，疑似写热点'},
  {id:'tikv-3',sid:3,type:'tikv',host:'10.0.2.13',port:20160,az:'az2',status:'up',cpu:44,mem:60,disk:60,regions:16130,leaders:5350,wr:41,rd:66,raftCpu:40,note:''},
  {id:'tikv-4',sid:4,type:'tikv',host:'10.0.2.14',port:20160,az:'az2',status:'warn',cpu:47,mem:63,disk:86,regions:16240,leaders:5390,wr:35,rd:60,raftCpu:43,note:'磁盘使用率 86% > 80%，PD 将降低其调度优先级'},
  {id:'tikv-5',sid:5,type:'tikv',host:'10.0.2.15',port:20160,az:'az3',status:'up',cpu:43,mem:59,disk:58,regions:16100,leaders:5330,wr:37,rd:57,raftCpu:41,note:''},
  {id:'tikv-6',sid:6,type:'tikv',host:'10.0.2.16',port:20160,az:'az3',status:'up',cpu:45,mem:62,disk:61,regions:16110,leaders:5360,wr:36,rd:59,raftCpu:42,note:''},
  // TiFlash
  {id:'tiflash-1',type:'tiflash',host:'10.0.3.11',port:3930,az:'az1',status:'up',cpu:31,mem:55,disk:48,regions:24100,note:'TiFlash 副本同步延迟 < 1s'},
  {id:'tiflash-2',type:'tiflash',host:'10.0.3.12',port:3930,az:'az2',status:'up',cpu:29,mem:53,disk:47,regions:24110,note:''},
  // TiCDC
  {id:'ticdc-1',type:'ticdc',host:'10.0.4.11',port:8300,az:'az1',status:'up',cpu:22,mem:35,disk:20,lag:'1.2s',note:'changefeed: kafka-sync，checkpoint lag 1.2s'},
  {id:'ticdc-2',type:'ticdc',host:'10.0.4.12',port:8300,az:'az2',status:'up',cpu:20,mem:33,disk:20,lag:'1.2s',note:''},
];
const TYPE_META = {
  tidb:{label:'TiDB Server',role:'SQL 层：协议解析、优化器、执行器；无状态，可水平扩展。',color:'var(--c1)'},
  pd:{label:'PD',role:'调度层：元信息、TSO 时间戳分配、Region 调度；内部 Raft 选主。',color:'var(--c4)'},
  tikv:{label:'TiKV',role:'行存储引擎：RocksDB + Raft，数据按 Region（默认 ~96MiB）切分，多副本。',color:'var(--c2)'},
  tiflash:{label:'TiFlash',role:'列存引擎：以 Raft Learner 身份异步复制 TiKV 数据，支撑 HTAP/MPP 分析。',color:'var(--c3)'},
  ticdc:{label:'TiCDC',role:'变更数据捕获：从 TiKV 拉取增量，同步到 Kafka / MySQL / 对象存储。',color:'var(--c6)'},
};
const AZS = {az1:'可用区 A（az1）',az2:'可用区 B（az2）',az3:'可用区 C（az3）'};

const REGIONS = [
  {id:20871,table:'orders',range:'t_112_r_8800…',peers:['tikv-2','tikv-3','tikv-5'],leader:'tikv-2',hot:'写热点'},
  {id:10455,table:'users',range:'t_98_r_120000…',peers:['tikv-1','tikv-4','tikv-6'],leader:'tikv-4',hot:''},
  {id:30211,table:'event_log',range:'t_130_r_9100000…',peers:['tikv-2','tikv-4','tikv-5'],leader:'tikv-2',hot:'写热点'},
];

/* ---------- 数据库时间分解（对应 Performance Overview 的三种分解） ---------- */
const DBTIME = {
  byType:[['Select',38,'--c1'],['Insert',13,'--c2'],['Update',17,'--c3'],['Delete',3,'--c4'],['Commit',12,'--c6'],['悲观锁等待',11,'--c5'],['其他',6,'--panel2']],
  byStage:[['Get Token',1,'--c6'],['Parse',3,'--c4'],['Compile',17,'--c3'],['Execute',79,'--c1']],
  byExec:[['TiDB 执行器',21,'--c1'],['KV 请求',36,'--c2'],['TSO 等待',9,'--c3'],['重试/锁等待',13,'--c5']],
};

/* ---------- 诊断规则输出（来自「故障排查地图」「性能调优方法」） ---------- */
const FINDINGS = [
  {id:'f1',sev:'critical',title:'tikv-2 疑似写入热点',scope:'tikv-2 / orders、event_log',
   evidence:['tikv-2 Raftstore CPU 92%，其余 TiKV 为 40%–44%','tikv-2 写流量 214 MB/s，集群均值 44 MB/s（4.8×）','Key Visualizer 出现「阶梯状斜线」，event_log 主键为 AUTO_INCREMENT'],
   cause:'顺序递增主键 / 自增 RowID 导致新写入始终落在同一个 Region，单个 TiKV 的 Raftstore 线程成为瓶颈。',
   actions:[{t:'event_log 改用 AUTO_RANDOM 主键（新表/重建表）',cmd:'CREATE TABLE event_log (\n  id BIGINT PRIMARY KEY AUTO_RANDOM,\n  payload JSON,\n  created_at DATETIME\n);'},
            {t:'无整数主键的表使用 SHARD_ROW_ID_BITS 打散 RowID',cmd:'ALTER TABLE orders SHARD_ROW_ID_BITS = 4;  -- 16 个分片'},
            {t:'必要时手动拆分热点 Region',cmd:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 operator add split-region 20871 --policy=approximate'}],
   link:{page:'hotspot',label:'查看热点分析'},ref:'troubleshoot-hot-spot-issues'},
  {id:'f2',sev:'critical',title:'tikv-4 磁盘使用率 86%，超过 80% 水位',scope:'tikv-4',
   evidence:['/data 使用率 86%（阈值 80%）','PD 对该 store 的 Region 调度打分下降，副本补齐速度变慢','集群其余 TiKV 磁盘 58%–64%'],
   cause:'数据倾斜 + 未及时扩容，磁盘使用率 ≥ 80% 时副本补齐和 Region 均衡会受限，≥ 90% 有写入失败风险。',
   actions:[{t:'扩容 TiKV 节点，让 PD 自动均衡',cmd:'tiup cluster scale-out prod-tidb-01 scale-out-tikv.yml'},
            {t:'临时降低该 store 的 Region 权重（谨慎）',cmd:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 store weight 4 1 0.5'},
            {t:'确认 GC 正常推进，清理历史版本',cmd:"SELECT * FROM mysql.tidb WHERE variable_name LIKE 'tikv_gc%';"}],
   link:{page:'nodes',label:'去扩容 TiKV',args:{tab:'scaleout',role:'tikv'}},ref:'tidb-troubleshooting-map'},
  {id:'f3',sev:'warning',title:'Compile 阶段占数据库时间 17%，执行计划缓存命中率低',scope:'tidb-1 ~ tidb-3',
   evidence:['Compile 在 DB Time 中占 17%（橙色占比偏高）','Plan Cache 命中率 21%，avg-miss ≈ StmtExecute','应用使用拼接 SQL，未使用 Prepared Statement'],
   cause:'每条语句都要走优化器，CPU 消耗大，尤其是 Compile 长尾。',
   actions:[{t:'应用侧改用 Prepared Statement，避免每次 StmtClose',cmd:'// JDBC\nuseServerPrepStmts=true&cachePrepStmts=true&prepStmtCacheSize=1000'},
            {t:'确认已开启 Prepared Plan Cache',cmd:'SHOW VARIABLES LIKE \'tidb_enable_prepared_plan_cache\';\nSET GLOBAL tidb_enable_prepared_plan_cache = ON;'}],
   link:{page:'sql',label:'查看 Top SQL'},ref:'performance-tuning-methods'},
  {id:'f4',sev:'warning',title:'TiDB → TiKV 请求网络延迟偏高（跨 AZ 6.2ms）',scope:'tidb-3 → tikv-5/6',
   evidence:['TiDB KV Request 延迟 − TiKV gRPC 处理延迟 = 6.2ms（跨可用区参考值 < 5ms）','同机房链路为 1.4ms，处于健康范围'],
   cause:'读请求大量走到异地 AZ 的 Leader；跨机房链路抖动。',
   actions:[{t:'开启 Follower Read / 就近读，降低跨 AZ 读取',cmd:"SET GLOBAL tidb_replica_read = 'closest-replicas';"},
            {t:'为 TiKV/TiDB 配置 zone label，并设置 location-labels',cmd:'tiup ctl:v8.5.2 pd config set location-labels zone,host'},
            {t:'检查机房间网络质量（丢包 / 重传）'}],
   link:{page:'arch',label:'查看拓扑'},ref:'performance-tuning-methods'},
  {id:'f5',sev:'warning',title:'悲观锁等待占 DB Time 11%，疑似热点行更新',scope:'inventory 表',
   evidence:['Performance Overview 红色（悲观锁）占比突出','Top SQL #3：update inventory set stock=stock-? where sku=?，平均 Wait_time 0.41s','同一 sku 并发度高'],
   cause:'秒杀类场景下对少量热点行并发更新，事务排队等待锁。',
   actions:[{t:'将热点库存拆成多行（分桶），聚合时再求和'},
            {t:'缩短事务：把非必要逻辑移出事务，尽早提交'},
            {t:'对确定无冲突的场景可评估 Fair Locking / 乐观事务'}],
   link:{page:'sql',label:'查看该 SQL'},ref:'tidb-troubleshooting-map'},
  {id:'f6',sev:'warning',title:'3 张表统计信息健康度 < 60%',scope:'orders / order_item / users',
   evidence:['orders 健康度 38%，Top SQL #1 估算行数 12、实际 380,000（偏差 > 10000×）','最近一次自动 ANALYZE 为 9 天前'],
   cause:'数据量剧增后统计信息过期，优化器误选 TableFullScan / 错误的 Join 顺序。',
   actions:[{t:'立即手动收集统计信息',cmd:'ANALYZE TABLE orders;\nANALYZE TABLE order_item;\nANALYZE TABLE users;'},
            {t:'检查自动收集窗口是否合理',cmd:"SHOW VARIABLES LIKE 'tidb_auto_analyze%';"}],
   link:{page:'sql',label:'查看执行计划'},ref:'sql-tuning-overview'},
  {id:'f7',sev:'info',title:'TSO 等待占 DB Time 9%',scope:'PD Leader：pd-1',
   evidence:['TSO Wait 为深褐色，占 9%','PD Leader 24h 内切换 2 次，最近一次在 03:12'],
   cause:'PD Leader 短暂抖动 / TiDB 到 PD 网络波动；高并发下可使用 TSO 批量请求。',
   actions:[{t:'检查 pd-1 磁盘 I/O、系统 load 及网络'},
            {t:'确认 PD 日志中 Leader 切换原因',cmd:'grep -E "campaign|become leader|lost leader" /tidb-deploy/pd-2379/log/pd.log | tail'}],
   link:{page:'ops',label:'查看巡检'},ref:'tidb-troubleshooting-map'},
  {id:'f8',sev:'info',title:'tidb-3 出现 expensive_query，内存 83%',scope:'tidb-3',
   evidence:['日志中 24 次 expensive_query，最大一次 Mem_max 2.1 GiB','来源：Top SQL #2，HashJoin 构建端过大'],
   cause:'Join 算法/顺序不优，或单条 SQL 内存配额过大；v3.0+ 可在日志里搜索 expensive_query 定位。',
   actions:[{t:'限制单条 SQL 内存并启用超限行为',cmd:'SET GLOBAL tidb_mem_quota_query = 2147483648;\nSET GLOBAL tidb_mem_oom_action = \'CANCEL\';'},
            {t:'为 Join 指定更优的算法/顺序',cmd:'SELECT /*+ HASH_JOIN_BUILD(o) */ ...'}],
   link:{page:'sql',label:'查看该 SQL'},ref:'tidb-troubleshooting-map'},
];

/* ---------- 慢 SQL / Top SQL ---------- */
const SQLS = [
 {id:1,digest:'a3f9c1',db:'shop',user:'app_order',
  sql:"SELECT * FROM orders WHERE user_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?",
  exec:18240,avg:2.84,p99:6.1,total:51801,keys:4200000,pkeys:120000,rows:20,mem:'96 MiB',index:'—（TableFullScan）',tag:'统计信息过期 + 缺联合索引',
  plan:[
   ['TopN_8',12,20,'root','','orders.created_at:desc, offset:0, count:20'],
   ['└─TableReader_16',12,380000,'root','','data:Selection_15'],
   ['  └─Selection_15',12,380000,'cop[tikv]','','eq(orders.user_id, 10086), eq(orders.status, "PAID")'],
   ['    └─TableFullScan_14',2400000,4200000,'cop[tikv]','table:orders','keep order:false, stats:pseudo']],
  tips:[
   {t:'统计信息缺失/过期（stats:pseudo，估算 12 vs 实际 380,000）',cmd:'ANALYZE TABLE orders;'},
   {t:'建立联合索引，覆盖等值条件 + 排序列，避免全表扫描和 TopN 排序',cmd:'ALTER TABLE orders ADD INDEX idx_user_status_created (user_id, status, created_at);'},
   {t:'避免 SELECT *，只取需要的列，可进一步用覆盖索引',cmd:'SELECT id, user_id, status, amount, created_at FROM orders WHERE ...'},
   {t:'索引上线前，可先用 SQL Binding 固定计划（验证后再放开）',cmd:"CREATE GLOBAL BINDING FOR\n  SELECT * FROM orders WHERE user_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?\nUSING\n  SELECT /*+ USE_INDEX(orders, idx_user_status_created) */ * FROM orders WHERE user_id = ? AND status = ? ORDER BY created_at DESC LIMIT ?;"}]},
 {id:2,digest:'7bd024',db:'shop',user:'app_report',
  sql:"SELECT o.region, COUNT(*) FROM order_item oi JOIN orders o ON oi.order_id = o.id WHERE oi.created_at > ? GROUP BY o.region",
  exec:620,avg:9.7,p99:21.3,total:6014,keys:98000000,pkeys:61000000,rows:36,mem:'2.1 GiB',index:'idx_created',tag:'大表 Join，内存占用高',
  plan:[
   ['HashAgg_11',36,36,'root','','group by:o.region, funcs:count(1)'],
   ['└─HashJoin_23',9.2e6,6.1e7,'root','','inner join, equal:[eq(oi.order_id, o.id)]'],
   ['  ├─TableReader_30(Build)',2.4e6,2.4e6,'root','','data:TableFullScan_29'],
   ['  │ └─TableFullScan_29',2.4e6,2.4e6,'cop[tikv]','table:o','keep order:false'],
   ['  └─IndexLookUp_28(Probe)',9.2e6,6.1e7,'root','',''],
   ['    ├─IndexRangeScan_26',9.2e6,6.1e7,'cop[tikv]','table:oi, index:idx_created','range:(2025-09-01,+inf]'],
   ['    └─TableRowIDScan_27',9.2e6,6.1e7,'cop[tikv]','table:oi','keep order:false']],
  tips:[
   {t:'该报表类查询适合下推到 TiFlash（列存 + MPP），可避免大 Join 内存压力',cmd:'ALTER TABLE orders SET TIFLASH REPLICA 2;\nALTER TABLE order_item SET TIFLASH REPLICA 2;\nSELECT /*+ READ_FROM_STORAGE(TIFLASH[oi, o]) */ ...'},
   {t:'限制该类 SQL 的内存，避免拖垮 TiDB 节点',cmd:'SET SESSION tidb_mem_quota_query = 2147483648;'},
   {t:'用资源管控 (Resource Control) 把报表用户隔离到独立资源组',cmd:"CREATE RESOURCE GROUP rg_report RU_PER_SEC = 2000;\nALTER USER app_report RESOURCE GROUP rg_report;"}]},
 {id:3,digest:'e810aa',db:'shop',user:'app_stock',
  sql:"UPDATE inventory SET stock = stock - ? WHERE sku = ? AND stock >= ?",
  exec:210400,avg:0.41,p99:3.2,total:86264,keys:3,pkeys:3,rows:0,mem:'4 KiB',index:'uniq_sku',tag:'热点行锁等待',
  plan:[
   ['Update_4',0,0,'root','','N/A'],
   ['└─Point_Get_1',1,1,'root','table:inventory, index:uniq_sku(sku)','']],
  tips:[
   {t:'计划本身最优（Point_Get）。耗时来自对同一行的悲观锁排队，需业务侧分桶减热点',cmd:'-- 将 1 行库存拆成 N 行（bucket），随机命中\nUPDATE inventory_bucket SET stock = stock - ? WHERE sku = ? AND bucket = FLOOR(RAND()*8) AND stock >= ?;'},
   {t:'缩短事务持有锁的时间，减少事务内的远程调用'},
   {t:'检查锁等待，定位持锁事务',cmd:'SELECT * FROM information_schema.DATA_LOCK_WAITS;\nSELECT * FROM information_schema.DEADLOCKS;'}]},
 {id:4,digest:'5c7e12',db:'shop',user:'app_web',
  sql:"SELECT id, name FROM users WHERE DATE(created_at) = ?",
  exec:3900,avg:1.62,p99:2.9,total:6318,keys:1800000,pkeys:1800000,rows:420,mem:'48 MiB',index:'—（TableFullScan）',tag:'索引列上使用函数',
  plan:[
   ['TableReader_7',1800,420,'root','','data:Selection_6'],
   ['└─Selection_6',1800,420,'cop[tikv]','','eq(date(users.created_at), 2025-09-30)'],
   ['  └─TableFullScan_5',1800000,1800000,'cop[tikv]','table:users','keep order:false']],
  tips:[
   {t:'对索引列使用 DATE() 会令索引失效，改写为范围条件',cmd:"SELECT id, name FROM users\nWHERE created_at >= '2025-09-30 00:00:00' AND created_at < '2025-10-01 00:00:00';"},
   {t:'确认 created_at 上存在索引',cmd:'ALTER TABLE users ADD INDEX idx_created (created_at);'}]},
 {id:5,digest:'91ab33',db:'log',user:'app_log',
  sql:"INSERT INTO event_log (payload, created_at) VALUES (?, NOW())",
  exec:884000,avg:0.012,p99:0.18,total:10608,keys:2,pkeys:2,rows:0,mem:'2 KiB',index:'PRIMARY（AUTO_INCREMENT）',tag:'顺序写热点',
  plan:[['Insert_1',0,0,'root','','N/A']],
  tips:[
   {t:'AUTO_INCREMENT 主键使新数据集中写入最后一个 Region（写热点），改为 AUTO_RANDOM',cmd:'CREATE TABLE event_log_new (\n  id BIGINT PRIMARY KEY AUTO_RANDOM,\n  payload JSON,\n  created_at DATETIME\n);'},
   {t:'若业务依赖自增有序，可使用 NONCLUSTERED 主键 + SHARD_ROW_ID_BITS + PRE_SPLIT_REGIONS',cmd:'CREATE TABLE event_log_new (\n  id BIGINT, payload JSON, created_at DATETIME,\n  PRIMARY KEY (id) NONCLUSTERED\n) SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4;'}]},
 {id:6,digest:'2dd7f0',db:'shop',user:'app_web',
  sql:"SELECT id, title FROM product WHERE title LIKE '%手机%' LIMIT 50",
  exec:15800,avg:0.88,p99:1.7,total:13904,keys:760000,pkeys:760000,rows:50,mem:'22 MiB',index:'—（TableFullScan）',tag:'前导通配符 LIKE',
  plan:[
   ['Limit_8',50,50,'root','','offset:0, count:50'],
   ['└─TableReader_14',50,50,'root','','data:Limit_13'],
   ['  └─Limit_13',50,50,'cop[tikv]','','offset:0, count:50'],
   ['    └─Selection_12',50,50,'cop[tikv]','','like(product.title, "%手机%", 92)'],
   ['      └─TableFullScan_11',76000,760000,'cop[tikv]','table:product','keep order:false']],
  tips:[
   {t:'前导 % 的 LIKE 无法走 B-Tree 索引。可改用全文检索（外部 ES）或 TiDB 全文/向量检索能力',},
   {t:'若查询是分析型，可使用 TiFlash 副本加速扫描',cmd:'ALTER TABLE product SET TIFLASH REPLICA 2;'}]},
];

/* ---------- 故障排查地图（来自官方文档 tidb-troubleshooting-map） ---------- */
const TROUBLE = [
 {cat:'服务不可用',title:'Region is Unavailable',symptom:'客户端报错 Region is Unavailable',
  causes:['Region 长时间没有 Leader','多台 TiKV 同时 OOM 重启','出现 TiKV server is busy','Label / 副本放置配置错误导致副本不满足多数派'],
  steps:['检查 TiKV 状态：tiup cluster display','查看 TiKV 日志中 panic / OOM 记录','Grafana → TiKV-Details → Errors 面板','pd-ctl region --jq 查看无 Leader 的 Region'],
  fix:['修复网络或配置问题，使多数副本恢复','必要时使用 tikv-ctl 对多副本失败的 Region 做 unsafe-recover'],
  cmd:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 region --jq=\'.regions[] | select(has("leader")|not) | .id\''},
 {cat:'服务不可用',title:'PD 异常导致集群不可用',symptom:'TiDB 无法获取 TSO / 路由，集群整体不可用',
  causes:['PD 多数派不可用','PD 节点磁盘 I/O 或网络故障'],steps:['tiup cluster display 看 PD 状态与 Leader','检查 PD 日志是否有选举失败'],fix:['恢复多数 PD 节点；无法恢复时按 pd-recover 流程重建'],cmd:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 member'},
 {cat:'延迟升高',title:'延迟短暂升高',symptom:'P99 延迟突发上升后回落',
  causes:['执行计划突变（统计信息变化）','PD 选举 / Leader 切换','TiKV 掉 Leader（重启、write stall、网络隔离）'],
  steps:['对比异常时间段 EXPLAIN ANALYZE 与历史 Plan_digest','查看 PD 日志 Leader 切换记录','TiKV 面板检查 Leader 数量变化与 Errors'],
  fix:['SQL Binding 固定执行计划','升级版本修复已知缺陷'],cmd:"SELECT digest, plan_digest, COUNT(*), AVG(query_time)\nFROM information_schema.slow_query\nWHERE time > NOW() - INTERVAL 1 HOUR\nGROUP BY digest, plan_digest ORDER BY 4 DESC LIMIT 10;"},
 {cat:'延迟升高',title:'延迟持续升高',symptom:'延迟长期保持高位',
  causes:['TiKV 单线程瓶颈（Raftstore / Apply 线程）','CPU 负载整体偏高','写入性能下降（磁盘、Compaction）'],
  steps:['Grafana → TiKV-Details → Thread CPU，看 Raftstore / Apply / gRPC CPU','检查磁盘 I/O 与 RocksDB 写停顿'],fix:['开启 Hibernate Region（Region 数量多且大部分空闲时）','扩容 TiKV 或升级版本']},
 {cat:'TiDB',title:'DDL 卡住 / 很慢',symptom:'ADMIN SHOW DDL JOBS 中 Job 长时间不推进',
  causes:['存在长时间未提交的事务（元数据锁）','TiDB → PD / TiKV 网络问题','集群负载过高'],steps:['ADMIN SHOW DDL JOBS 查看状态','检查未提交的长事务','必要时迁移 DDL Owner'],fix:['结束阻塞的长事务','降低业务压力后重试'],cmd:'ADMIN SHOW DDL JOBS 10;\nSELECT * FROM information_schema.cluster_tidb_trx ORDER BY start_time LIMIT 10;'},
 {cat:'TiDB',title:'information schema is changed',symptom:'报错 information schema is changed',
  causes:['DDL 变更过于频繁，Schema 版本增长过快','TiDB 加载 Schema 超时'],steps:['查看 DDL 变更频率','检查 TiDB 到 TiKV 延迟'],fix:['减少列变更等 DDL 的执行频率，避免与高峰期并行']},
 {cat:'TiDB',title:'TiDB OOM',symptom:'连接断开、TiDB 进程重启',
  causes:['Join 算法不优','大批量 UPDATE/DELETE','UNION 子查询过多'],steps:['v3.0+：日志中搜索 expensive_query','v3.0 以下：搜索 memory exceeds quota'],fix:['限制 tidb_mem_quota_query 并 CANCEL','优化 SQL / 分批执行','升级版本，开启 SWAP 缓解'],cmd:'grep -E "expensive_query|memory exceeds quota" /tidb-deploy/tidb-4000/log/tidb.log | tail -20'},
 {cat:'TiDB',title:'执行计划不对 / SQL 突然变慢',symptom:'同一条 SQL 之前快现在慢',
  causes:['统计信息过期（EXPLAIN ANALYZE 中 estRows 与 actRows 差距大）','Scan Keys 异常增多'],steps:['对比 EXPLAIN ANALYZE 的估算与实际','慢日志检查 Total_keys / Process_keys'],fix:['ANALYZE TABLE 更新统计信息','SQL Binding 固定计划','Optimizer Hints 干预'],cmd:'EXPLAIN ANALYZE SELECT ...;\nSHOW STATS_HEALTHY WHERE db_name = \'shop\';'},
 {cat:'TiDB',title:'锁冲突 / Write Conflict',symptom:'日志出现 write conflict、key is locked',
  causes:['乐观事务冲突','热点行并发更新'],steps:['区分乐观 / 悲观事务模式','查看 DATA_LOCK_WAITS / DEADLOCKS'],fix:['改用悲观事务','降低并发或拆分热点行'],cmd:'SELECT * FROM information_schema.DATA_LOCK_WAITS;'},
 {cat:'TiDB',title:'数据一致性检查',symptom:'怀疑数据与索引不一致',causes:['异常宕机、Bug、手工误操作'],steps:['ADMIN CHECK TABLE / ADMIN CHECK INDEX'],fix:['按报告修复，必要时 ADMIN RECOVER INDEX'],cmd:'ADMIN CHECK TABLE shop.orders;'},
 {cat:'TiKV',title:'TiKV 启动失败：entries is unavailable',symptom:'启动日志 entries is unavailable',causes:['虚拟机 fsync 不可靠，Raft 日志损坏'],steps:['查看 TiKV 日志'],fix:['使用 tikv-ctl 恢复相关 Region，并迁移到可靠存储']},
 {cat:'TiKV',title:'TiKV OOM',symptom:'TiKV 进程被 OOM Killer 杀死',
  causes:['block-cache 配置过大','Coprocessor 返回数据量过大（response size > 网络出口）'],steps:['RocksDB 监控查看 block cache 占用','比较 Coprocessor response size 与 network outbound'],fix:['调小 storage.block-cache.capacity','优化大查询，或增加网络带宽'],cmd:'tiup cluster edit-config prod-tidb-01\n# storage.block-cache.capacity: "16GB"\ntiup cluster reload prod-tidb-01 -R tikv'},
 {cat:'TiKV',title:'TiKV server is busy',symptom:'客户端报 ServerIsBusy',
  causes:['RocksDB write stall（日志出现 Stalling）','写冲突严重：latch wait duration 偏高','scheduler 堆积超过阈值 (100MB)','raftstore 繁忙：channel full','Coprocessor 请求排队'],
  steps:['grep "Stalling" 检查日志','检查 latch wait duration / scheduler pending bytes','检查 append log 与线程池使用率'],fix:['调整 soft/hard-pending-compaction-bytes-limit','降低并发或改用悲观锁','调整线程池大小','优化查询执行计划'],cmd:'grep -i "stalling" /tidb-deploy/tikv-20160/log/tikv.log | tail'},
 {cat:'TiKV',title:'大量掉 Leader',symptom:'Leader 数量剧烈波动',causes:['TiKV 重启 / panic','write stall','网络隔离'],steps:['查看 panic 记录','errors 面板','检查节点间网络'],fix:['修复 panic 根因','恢复网络','参考 server is busy 排查']},
 {cat:'TiKV',title:'写入慢（按阶段拆解）',symptom:'TiKV 写入延迟高',
  causes:['Scheduler CPU 繁忙','Append log：WAL Sync Duration 高','Raftstore：propose wait duration 高','Apply log duration 高','Raft commit log duration 高'],
  steps:['按 Scheduler → Append → Raftstore → Apply → Commit 逐阶段看延迟面板'],
  fix:['增大 scheduler-worker-pool-size','检查磁盘写入性能','store-pool-size 调至 1–5 之间','增大 apply-pool-size','raft-max-inflight-msgs 调到 1024 等']},
 {cat:'PD',title:'Region merge 卡住 / 跨表无法合并',symptom:'小 Region 过多、merge 不动',causes:['split-region-on-table 开启，跨表 Region 不合并'],steps:['查看 PD config 与 operator'],fix:['按需设置 [coprocessor] split-region-on-table = false']},
 {cat:'PD',title:'副本补齐慢 / Region 不均衡',symptom:'miss peer 数量长时间不下降',causes:['store 磁盘使用率 ≥ 80%','调度限速过严','Label 拓扑约束无可用 store'],steps:['pd-ctl store 查看 available / region_score','pd-ctl config show 查看 schedule 配置'],fix:['磁盘告警及时扩容','适当调大 replica-schedule-limit / region-schedule-limit'],cmd:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 config show\ntiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 store'},
 {cat:'PD',title:'PD Leader 频繁切换 / 选不出 Leader',symptom:'日志 lease is not expired；SQL 超时',causes:['磁盘 I/O、网络或系统 load 高','版本缺陷','加载 Region 慢导致选举慢'],steps:['检查 PD 机器磁盘、网络、load','查看日志切换记录'],fix:['隔离 PD 与其他高 I/O 服务','升级到修复版本','开启 use-region-storage = true']},
 {cat:'PD',title:'PD OOM',symptom:'PD 内存暴涨被 OOM 杀死',causes:['调用 /api/v1/regions 接口（旧版本）','滚动升级期间的缺陷（旧版本）'],steps:['确认版本与是否有人高频调用该 API'],fix:['升级版本；避免全量拉取 Region API']},
 {cat:'生态工具',title:'DM：Access denied / bad connection',symptom:'同步任务报连接类错误',causes:['密码未加密或权限不足','网络故障或 TiDB 重启'],steps:['dmctl query-status','检查上下游权限与网络'],fix:['使用 dmctl encrypt 加密密码','v1.0.0+ 会自动重试，否则 stop-task / start-task']},
 {cat:'生态工具',title:'TiDB Lightning 导入慢 / checksum 失败',symptom:'导入速度低、checksum mismatch',causes:['region-concurrency 设置不当','数据源问题或目标表已有数据','源文件非 UTF-8'],steps:['查看 Lightning 日志','检查目标表是否为空'],fix:['region-concurrency 调为 CPU 核数的约 75%','清空目标表重试','转换为 UTF-8']},
 {cat:'生态工具',title:'TiCDC 同步延迟变大',symptom:'changefeed checkpoint lag 持续增长',causes:['下游写入慢','单表数据量大 / 大事务','TiCDC 节点资源不足'],steps:['cdc cli changefeed list/query','Grafana → TiCDC 面板查看 resolved ts / checkpoint lag'],fix:['扩容 TiCDC','调大下游并发','拆分大事务'],cmd:'tiup cdc cli changefeed list --server=http://10.0.4.11:8300'},
];
const TROUBLE_CATS = ['全部','服务不可用','延迟升高','TiDB','TiKV','PD','生态工具'];

const LOG_ERRORS = [
 {msg:'GC life time is shorter than transaction duration',mean:'事务运行时间超过 GC 保留时间，被 GC 回收了读快照',act:'缩短事务；谨慎调大 tidb_gc_life_time（会增加存储占用）',sev:'warn'},
 {msg:'key is locked',mean:'读写冲突，读请求遇到未提交事务的锁',act:'少量无影响；大量出现需优化业务逻辑、缩短事务',sev:'info'},
 {msg:'write conflict',mean:'乐观事务提交时冲突',act:'改用悲观事务或降低并发',sev:'warn'},
 {msg:'stale_epoch / EpochNotMatch',mean:'Region 路由信息过期（发生了分裂、合并或迁移）',act:'TiDB 自动刷新路由并重试，业务无感；持续大量出现需查调度',sev:'info'},
 {msg:'peer is not leader / NotLeader',mean:'请求发给了非 Leader 副本',act:'TiDB 自动更新路由重试；频繁出现说明掉 Leader / 调度频繁',sev:'info'},
 {msg:'Region is unavailable',mean:'Region 无可用 Leader',act:'立即检查 TiKV 存活与网络，见「故障排查地图」',sev:'bad'},
 {msg:'TiKV server is busy',mean:'TiKV 过载，主动限流',act:'检查 write stall / scheduler / raftstore，见 TiKV server is busy',sev:'bad'},
 {msg:'information schema is changed',mean:'执行期间 Schema 版本变化过快',act:'降低 DDL 频率，避开高峰',sev:'warn'},
 {msg:'memory exceeds quota',mean:'单条 SQL 内存超过 tidb_mem_quota_query',act:'优化 SQL 或调整配额，v3.0+ 用 expensive_query 检索',sev:'warn'},
];

/* ---------- 排障向导决策树 ---------- */
const WIZARD = {
 '写入变慢':{start:'w1',nodes:{
  w1:{q:'TiKV-Details 中 Raftstore CPU 是否只有个别节点很高（与其他节点差距明显）？',y:'w_hot',n:'w2'},
  w_hot:{r:'写热点',desc:'写入集中在少数 Region / 节点。',steps:['在 Key Visualizer 确认「阶梯状斜线」','AUTO_RANDOM / SHARD_ROW_ID_BITS 打散','必要时手动拆分 Region'],link:'hotspot'},
  w2:{q:'RocksDB 日志是否出现 Stalling，或 Write Stall 面板有数据？',y:'w_stall',n:'w3'},
  w_stall:{r:'RocksDB Write Stall',desc:'Compaction 跟不上写入，触发写入限流。',steps:['grep -i stalling tikv.log','检查磁盘 IOPS / 吞吐','评估调整 soft/hard-pending-compaction-bytes-limit','扩容 TiKV 分担写入']},
  w3:{q:'WAL Sync Duration 是否偏高（> 数 ms 持续）？',y:'w_disk',n:'w4'},
  w_disk:{r:'磁盘写入瓶颈（Append Log 阶段）',desc:'Raft 日志落盘慢。',steps:['fio / iostat 检查磁盘延迟','Raft Engine 与数据盘分离','更换更高性能 NVMe']},
  w4:{q:'Apply log duration / Propose wait duration 是否偏高？',y:'w_pool',n:'w_lock'},
  w_pool:{r:'线程池不足（Raftstore / Apply）',desc:'store-pool-size 或 apply-pool-size 偏小。',steps:['store-pool-size 调至 1–5','增加 apply-pool-size','观察 Thread CPU 是否仍然饱和']},
  w_lock:{r:'事务层冲突 / 锁等待',desc:'更可能是锁冲突或大事务。',steps:['查看 latch wait duration','DATA_LOCK_WAITS / DEADLOCKS','缩短事务、拆分大批量写入'],link:'sql'}}},
 'TiKV server is busy':{start:'b1',nodes:{
  b1:{q:'日志中是否有 "Stalling" 字样？',y:'b_stall',n:'b2'},
  b_stall:{r:'RocksDB write stall',desc:'写入积压触发限流。',steps:['调整 pending-compaction-bytes 限制','排查磁盘性能','降低写入压力']},
  b2:{q:'latch wait duration 是否很高（写写冲突严重）？',y:'b_latch',n:'b3'},
  b_latch:{r:'写冲突 / 热点行',desc:'相同 Key 并发写。',steps:['降低并发','乐观事务改悲观事务','拆分热点行']},
  b3:{q:'日志出现 channel full 或 raftstore 线程 CPU 接近 100%？',y:'b_raft',n:'b_cop'},
  b_raft:{r:'raftstore 繁忙',desc:'线程池不足或 append log 慢。',steps:['检查 append log 耗时','调大 store-pool-size']},
  b_cop:{r:'Coprocessor 请求排队',desc:'读请求过多或计划劣化。',steps:['定位大查询（Top SQL）','优化执行计划 / 增加索引','考虑 Follower Read 分担'],link:'sql'}}},
 '延迟升高':{start:'l1',nodes:{
  l1:{q:'延迟是「短暂尖峰」还是「持续偏高」？',opts:[['短暂尖峰','l2'],['持续偏高','l5']]},
  l2:{q:'尖峰时刻是否同时发生了 PD Leader 切换 / TiKV 掉 Leader？',y:'l_leader',n:'l_plan'},
  l_leader:{r:'选举 / 掉 Leader 引发',desc:'Leader 切换期间请求需要重试。',steps:['检查 PD 与 TiKV 日志中的切换原因','排查磁盘 I/O、网络、节点重启']},
  l_plan:{r:'执行计划突变',desc:'统计信息变化导致 plan_digest 改变。',steps:['对比 EXPLAIN ANALYZE','ANALYZE TABLE','SQL Binding 固定计划'],link:'sql'},
  l5:{q:'Performance Overview 中 TiDB–TiKV 延迟差是否 > 5ms（同机房应 < 2ms）？',y:'l_net',n:'l6'},
  l_net:{r:'网络延迟',desc:'TiDB 与 TiKV 间链路慢。',steps:['检查跨 AZ 部署','Follower Read / closest-replicas','网络质量排查']},
  l6:{q:'DB Time 中 Compile 占比是否很高（橙色）？',y:'l_compile',n:'l_store'},
  l_compile:{r:'编译开销大',desc:'未使用执行计划缓存。',steps:['使用 Prepared Statement','开启 Prepared Plan Cache','避免过度 StmtClose']},
  l_store:{r:'Store 侧瓶颈',desc:'Commit Log Duration 高等。',steps:['增加 store 线程','启用 Raft Engine','排查磁盘 / Compaction']}}},
};

/* ---------- 参数 ---------- */
const PARAMS = [
 {scope:'tidb',name:'tidb_mem_quota_query',cur:'1 GiB',rec:'2 GiB 并配合 tidb_mem_oom_action=CANCEL',why:'限制单条 SQL 内存，防止 OOM 影响整个节点',set:"SET GLOBAL tidb_mem_quota_query = 2147483648;"},
 {scope:'tidb',name:'tidb_enable_prepared_plan_cache',cur:'ON',rec:'ON',why:'启用后可明显降低 Compile 开销',set:"SET GLOBAL tidb_enable_prepared_plan_cache = ON;"},
 {scope:'tidb',name:'tidb_gc_life_time',cur:'10m0s',rec:'≥ 最长事务时长；备份期间需 ≥ 备份时长',why:'过短会导致 GC life time is shorter 报错，过长占用存储',set:"SET GLOBAL tidb_gc_life_time = '30m';"},
 {scope:'tidb',name:'tidb_replica_read',cur:'leader',rec:'closest-replicas（跨 AZ 部署）',why:'读请求就近读取，降低跨机房延迟',set:"SET GLOBAL tidb_replica_read = 'closest-replicas';"},
 {scope:'tidb',name:'slow-threshold / tidb_slow_log_threshold',cur:'300ms',rec:'300ms（OLTP）',why:'慢日志阈值，过低会产生大量日志',set:"SET GLOBAL tidb_slow_log_threshold = 300;"},
 {scope:'tikv',name:'storage.block-cache.capacity',cur:'约 45% 内存',rec:'≤ 40%–45% 内存（与其他服务混部需更低）',why:'过大可能引发 TiKV OOM',set:'tiup cluster edit-config prod-tidb-01   # storage.block-cache.capacity\ntiup cluster reload prod-tidb-01 -R tikv'},
 {scope:'tikv',name:'raftstore.store-pool-size',cur:'2',rec:'2–5（视 Raftstore CPU 而定）',why:'propose wait duration 高时可调大',set:'tiup cluster edit-config prod-tidb-01   # raftstore.store-pool-size: 3\ntiup cluster reload prod-tidb-01 -R tikv'},
 {scope:'tikv',name:'storage.scheduler-worker-pool-size',cur:'4',rec:'按 CPU 核数，≥ 8 核可适当调大',why:'Scheduler 线程繁忙时增大',set:'tiup cluster edit-config prod-tidb-01   # storage.scheduler-worker-pool-size: 6\ntiup cluster reload prod-tidb-01 -R tikv'},
 {scope:'pd',name:'schedule.region-schedule-limit',cur:'2048',rec:'按迁移速度需求，业务高峰调小',why:'控制 Region 调度速度，保护在线业务',set:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 config set region-schedule-limit 1024'},
 {scope:'pd',name:'replication.location-labels',cur:'zone,host',rec:'zone,rack,host（按真实机房拓扑）',why:'保证副本分散在不同故障域',set:'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 config set location-labels zone,rack,host'},
];

/* ---------- 备份 ---------- */
const BACKUPS = [
 {type:'快照备份',id:'snap-20251004-0200',time:'2025-10-04 02:00',size:'1.82 TiB',dur:'38 min',status:'成功',store:'s3://tidb-backup/prod/snap-20251004'},
 {type:'快照备份',id:'snap-20251003-0200',time:'2025-10-03 02:00',size:'1.81 TiB',dur:'37 min',status:'成功',store:'s3://tidb-backup/prod/snap-20251003'},
 {type:'快照备份',id:'snap-20251002-0200',time:'2025-10-02 02:00',size:'1.79 TiB',dur:'41 min',status:'成功',store:'s3://tidb-backup/prod/snap-20251002'},
 {type:'快照备份',id:'snap-20251001-0200',time:'2025-10-01 02:00',size:'—',dur:'—',status:'失败',store:'S3 AccessDenied（凭证过期）'},
];

/* ---------- 巡检项 ---------- */
const CHECKS = [
 {g:'集群状态',n:'所有组件 Up',r:'ok',d:'16/16 个实例 Up'},
 {g:'集群状态',n:'PD Leader 稳定性（24h）',r:'warn',d:'Leader 切换 2 次，最近 03:12'},
 {g:'集群状态',n:'PD 健康 / 多数派',r:'ok',d:'3/3 成员健康'},
 {g:'存储',n:'TiKV 磁盘使用率 < 80%',r:'bad',d:'tikv-4：86%'},
 {g:'存储',n:'Region 缺副本 / 多副本',r:'ok',d:'miss-peer 0，extra-peer 0'},
 {g:'存储',n:'Region 健康（down / pending peer）',r:'ok',d:'0'},
 {g:'存储',n:'Leader / Region 分布均衡度',r:'warn',d:'tikv-2 写流量偏高 4.8×'},
 {g:'性能',n:'慢查询（> 300ms）数量 / 小时',r:'warn',d:'4,210 条，较昨日 +37%'},
 {g:'性能',n:'统计信息健康度 ≥ 80%',r:'warn',d:'3 张表 < 60%'},
 {g:'性能',n:'Plan Cache 命中率',r:'warn',d:'21%'},
 {g:'资源',n:'TiDB 内存使用率 < 80%',r:'warn',d:'tidb-3：83%'},
 {g:'资源',n:'TiKV Block Cache / 内存',r:'ok',d:'59%–66%'},
 {g:'数据保护',n:'最近一次全量备份成功',r:'ok',d:'2025-10-04 02:00'},
 {g:'数据保护',n:'PITR 日志备份延迟 < 5min',r:'ok',d:'checkpoint lag 38s'},
 {g:'数据保护',n:'GC safepoint 推进正常',r:'ok',d:'safepoint 滞后 11 分钟'},
 {g:'生态',n:'TiCDC changefeed 延迟 < 10s',r:'ok',d:'1.2s'},
 {g:'生态',n:'TiFlash 副本同步',r:'ok',d:'所有副本 available'},
 {g:'合规',n:'版本是否在支持周期内',r:'ok',d:VERSION},
];

const ALERTS = [
 {lv:'critical',name:'TiKV_disk_used_more_than_80%',inst:'10.0.2.14:20160',time:'09:12',desc:'磁盘使用率 86%'},
 {lv:'critical',name:'TiKV_raftstore_thread_cpu_seconds_total',inst:'10.0.2.12:20160',time:'09:40',desc:'Raftstore 线程 CPU 超过阈值'},
 {lv:'warning',name:'TiDB_memory_high',inst:'10.0.1.13:4000',time:'10:03',desc:'内存使用率 83%'},
 {lv:'warning',name:'PD_leader_change',inst:'10.0.1.21:2379',time:'03:12',desc:'PD Leader 24h 内切换 ≥ 2 次'},
 {lv:'info',name:'TiDB_slow_query_increase',inst:'cluster',time:'08:55',desc:'慢查询数量较昨日增加 37%'},
];

const AUDIT = [
 ['2025-10-05 09:31','ops_zhang','edit-config','TiKV storage.block-cache.capacity 18GB → 16GB','成功'],
 ['2025-10-05 09:33','ops_zhang','reload -R tikv','滚动重载 6 个 TiKV 实例','成功'],
 ['2025-10-03 22:10','dba_li','scale-out','新增 tidb-3（10.0.1.13）','成功'],
 ['2025-09-29 01:00','dba_li','upgrade','v8.5.1 → v8.5.2 滚动升级','成功'],
 ['2025-09-22 14:20','ops_wang','patch','tikv-20160 替换临时补丁包','已回滚'],
];


/* ---------- 主机池（尚未加入集群的候选机器）与预检 ---------- */
const CHECK_ITEMS = [
  ['os','操作系统版本 / 内核'],['ssh','SSH 连通性与免密'],['swap','Swap 已关闭'],['thp','透明大页 (THP) 已关闭'],
  ['ntp','时间同步 (chrony / ntp)'],['selinux','SELinux 已关闭'],['limits','文件句柄 ulimit ≥ 1000000'],
  ['disk-mount','数据盘 nodelalloc / noatime'],['ports','服务端口未占用'],['sysctl','内核网络参数'],
  ['governor','CPU 调频 performance'],['irq','irqbalance 运行']
];
const SPEC = {
  tidb:{cores:16,mem:64,disk:'SSD 500G'},pd:{cores:8,mem:16,disk:'SSD 200G'},tikv:{cores:32,mem:128,disk:'NVMe 3.5T'},
  tiflash:{cores:32,mem:128,disk:'NVMe 7T'},ticdc:{cores:16,mem:32,disk:'SSD 500G'}
};
const DEFAULT_PORTS = {tidb:4000,pd:2379,tikv:20160,tiflash:3930,ticdc:8300};
/* preset: 预检时会命中的问题 [结果, 描述, 可否 --apply 自动修复] */
const HOST_POOL = [
  {ip:'10.0.2.17',az:'az1',rack:'r07',cores:32,mem:128,disk:'NVMe 3.5T',os:'Rocky Linux 9.3',note:'已采购，待加入',checked:true,preset:{}},
  {ip:'10.0.2.18',az:'az2',rack:'r12',cores:32,mem:128,disk:'NVMe 3.5T',os:'Rocky Linux 9.3',note:'已采购，待加入',checked:true,
    preset:{swap:['fail','Swap 已开启（3.9 GB）',true],thp:['warn','透明大页为 always',true]}},
  {ip:'10.0.2.19',az:'az3',rack:'r21',cores:32,mem:128,disk:'NVMe 3.5T',os:'Rocky Linux 9.3',note:'已采购，待加入',checked:false,
    preset:{ntp:['fail','与 NTP 服务器偏移 156 ms，超过 50 ms 阈值',true],'disk-mount':['warn','数据盘缺少 nodelalloc 挂载选项，需手动 remount，无法 --apply 修复',false]}},
  {ip:'10.0.1.14',az:'az1',rack:'r03',cores:16,mem:64,disk:'SSD 500G',os:'Rocky Linux 9.3',note:'TiDB 扩容备用机',checked:true,preset:{}},
  {ip:'10.0.1.15',az:'az2',rack:'r09',cores:16,mem:64,disk:'SSD 500G',os:'Rocky Linux 9.3',note:'TiDB 扩容备用机',checked:false,
    preset:{ssh:['fail','SSH 连接超时：22 端口不可达，请检查安全组 / 防火墙 / 公钥',false]}},
  {ip:'10.0.3.13',az:'az3',rack:'r30',cores:32,mem:128,disk:'NVMe 7T',os:'Rocky Linux 9.3',note:'TiFlash 备用机',checked:true,preset:{}},
];
