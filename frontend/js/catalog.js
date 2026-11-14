// @author: kevinkelly
// @license: Apache License 2.0
// @file: catalog.js
// @time: 2026/10/28

/* ---------- 配置项目录（节点详情 → 配置变更）----------
   scope: instance 实例级（配置文件 / SET CONFIG，可单实例覆盖）；cluster 集群级（系统变量 / PD 参数，所有实例共享）
   online: true 可在线生效；false 需 tiup edit-config + 滚动 reload */
const CONFIG_CATALOG = {
  tikv: [
    {k:'storage.block-cache.capacity',label:'Block Cache 容量',type:'size',def:'16GB',online:true,scope:'instance',desc:'RocksDB Block Cache（各 CF 共享）。过大是 TiKV OOM 的常见原因，建议 ≤ 内存 45%。'},
    {k:'raftstore.store-pool-size',label:'Raftstore 线程数',type:'int',def:2,min:1,max:16,online:true,scope:'instance',desc:'处理 Raft 消息与 propose 的线程数。propose wait duration 偏高时调大，常用范围 1–5。'},
    {k:'raftstore.apply-pool-size',label:'Apply 线程数',type:'int',def:2,min:1,max:16,online:true,scope:'instance',desc:'把已提交日志应用到 RocksDB 的线程数。apply log duration 偏高时调大。'},
    {k:'storage.scheduler-worker-pool-size',label:'Scheduler 线程数',type:'int',def:4,min:1,max:32,online:true,scope:'instance',desc:'事务调度线程，Scheduler CPU 繁忙时调大；不应超过 CPU 核数。'},
    {k:'readpool.unified.max-thread-count',label:'统一读线程池上限',type:'int',def:16,min:4,max:64,online:true,scope:'instance',desc:'点查与 Coprocessor 共用的读线程池上限，Coprocessor 排队时可调大。'},
    {k:'rocksdb.max-background-jobs',label:'RocksDB 后台任务数',type:'int',def:8,min:2,max:32,online:true,scope:'instance',desc:'Flush / Compaction 并发数；write stall 频繁时可适度调大，注意占用 CPU 与 I/O。'},
    {k:'raftstore.raft-max-inflight-msgs',label:'Raft 在途消息上限',type:'int',def:256,min:64,max:4096,online:false,scope:'instance',desc:'Raft commit log duration 偏高时可调到 1024；需重启生效。'},
    {k:'server.grpc-concurrency',label:'gRPC 线程数',type:'int',def:5,min:1,max:32,online:false,scope:'instance',desc:'gRPC 服务线程数；gRPC CPU 打满时调大。需重启生效。'},
    {k:'log.level',label:'日志级别',type:'enum',opts:['debug','info','warn','error'],def:'info',online:true,scope:'instance',desc:'debug 日志量很大，只建议短时排障使用。'},
  ],
  tidb: [
    {k:'tidb_mem_quota_query',label:'单条 SQL 内存配额',type:'size',def:'1GB',online:true,scope:'cluster',sys:true,desc:'超过后按 tidb_mem_oom_action 处理（日志 / 取消）。避免个别 SQL 拖垮节点。'},
    {k:'tidb_enable_prepared_plan_cache',label:'Prepared 执行计划缓存',type:'bool',def:'ON',online:true,scope:'cluster',sys:true,desc:'开启可明显降低 Compile 耗时。'},
    {k:'tidb_slow_log_threshold',label:'慢日志阈值 (ms)',type:'int',def:300,min:50,max:10000,online:true,scope:'cluster',sys:true,desc:'超过该耗时的 SQL 记入慢日志；OLTP 常用 300ms。'},
    {k:'tidb_distsql_scan_concurrency',label:'扫描并发度',type:'int',def:15,min:1,max:256,online:true,scope:'cluster',sys:true,desc:'单条 SQL 向 TiKV 发起扫描的并发，调大会加重 TiKV 负载。'},
    {k:'tidb_replica_read',label:'读副本策略',type:'enum',opts:['leader','follower','closest-replicas','leader-and-follower'],def:'leader',online:true,scope:'cluster',sys:true,desc:'跨 AZ 部署可用 closest-replicas 降低读延迟。'},
    {k:'performance.max-procs',label:'max-procs（0=全部 CPU）',type:'int',def:0,min:0,max:128,online:false,scope:'instance',desc:'TiDB 使用的 CPU 核数上限，混部时可限制。需重启生效。'},
    {k:'performance.token-limit',label:'并发 token 上限',type:'int',def:1000,min:100,max:20000,online:false,scope:'instance',desc:'同时执行的会话数上限，Get Token 耗时高时调大。需重启生效。'},
    {k:'log.level',label:'日志级别',type:'enum',opts:['debug','info','warn','error'],def:'info',online:false,scope:'instance',desc:'需重启生效。'},
  ],
  pd: [
    {k:'schedule.max-store-down-time',label:'Store 判定 Down 的时间',type:'duration',def:'30m',online:true,scope:'cluster',desc:'TiKV 失联超过该时间，PD 判定其 Down 并开始补副本。计划内维护务必关注该值。'},
    {k:'schedule.region-schedule-limit',label:'Region 调度并发上限',type:'int',def:2048,min:1,max:8192,online:true,scope:'cluster',desc:'同时进行的 Region 调度数。调大迁移更快、对线上影响更大。'},
    {k:'schedule.leader-schedule-limit',label:'Leader 调度并发上限',type:'int',def:4,min:1,max:512,online:true,scope:'cluster',desc:'同时进行的 Leader 调度数。'},
    {k:'replication.max-replicas',label:'副本数 max-replicas',type:'int',def:3,min:1,max:7,online:true,scope:'cluster',danger:true,desc:'每个 Region 的副本数。修改会触发全量副本补齐/删除，影响极大。'},
    {k:'log.level',label:'日志级别',type:'enum',opts:['debug','info','warn','error'],def:'info',online:false,scope:'instance',desc:'需重启生效。'},
  ],
  tiflash: [
    {k:'profiles.default.max_memory_usage',label:'单查询内存上限',type:'size',def:'0GB',online:false,scope:'instance',desc:'0 表示不限制。分析型查询占用过大时设置上限。需重启生效。'},
    {k:'profiles.default.max_threads',label:'查询线程数',type:'int',def:0,min:0,max:128,online:false,scope:'instance',desc:'0 表示按 CPU 核数自动决定。'},
    {k:'logger.level',label:'日志级别',type:'enum',opts:['debug','info','warning','error'],def:'info',online:false,scope:'instance',desc:'需重启生效。'},
  ],
  ticdc: [
    {k:'gc-ttl',label:'上游 GC 保留时长 (秒)',type:'int',def:86400,min:600,max:604800,online:false,scope:'instance',desc:'changefeed 暂停期间上游保留数据的时长；过大占用 TiKV 空间。'},
    {k:'per-table-memory-quota',label:'单表内存配额 (字节)',type:'int',def:10485760,min:1048576,max:1073741824,online:false,scope:'instance',desc:'同步大表时可适当调大。'},
    {k:'log-level',label:'日志级别',type:'enum',opts:['debug','info','warn','error'],def:'info',online:false,scope:'instance',desc:'需重启生效。'},
  ],
};
const CFG_HISTORY = [
  {ts:'2025-10-05 09:31',user:'ops_zhang',role:'tikv',target:'全部 TiKV',k:'storage.block-cache.capacity',from:'18GB',to:'16GB',mode:'reload',status:'已生效'},
  {ts:'2025-10-03 22:12',user:'dba_li',role:'tidb',target:'集群',k:'tidb_mem_quota_query',from:'512MB',to:'1GB',mode:'online',status:'已生效'},
  {ts:'2025-09-29 01:20',user:'dba_li',role:'pd',target:'集群',k:'schedule.region-schedule-limit',from:'1024',to:'2048',mode:'online',status:'已生效'},
  {ts:'2025-09-22 14:05',user:'ops_wang',role:'tikv',target:'tikv-3',nodeId:'tikv-3',k:'raftstore.store-pool-size',from:'2',to:'3',mode:'online',status:'已生效'},
];

/* ---------- 主机维护类型 ---------- */
const MAINT_TYPES = {
  kernel:{label:'内核 / OS 升级',def:25,todo:['升级内核 / 系统包并重启主机','确认 THP、Swap、sysctl 等参数未被还原','确认时间同步（chrony / ntp）正常']},
  disk:{label:'磁盘更换 / 扩容',def:90,todo:['更换或扩容磁盘并重新挂载（nodelalloc,noatime）','确认数据目录权限、空间与文件系统','抽样校验数据目录文件完整']},
  hardware:{label:'硬件检修（内存 / 电源 / 网卡）',def:120,todo:['完成硬件检修并上电','确认 BIOS 电源策略为高性能','确认网卡速率与双工模式']},
  net:{label:'网络变更',def:30,todo:['完成网络变更','验证到 PD / TiKV / TiDB 各节点互通','确认跨机房时延在阈值内（< 5ms）']},
  reboot:{label:'仅重启主机',def:10,todo:['重启主机','确认系统服务与 tiup 目录挂载正常']},
};
