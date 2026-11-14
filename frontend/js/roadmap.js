// @author: kevinkelly
// @license: Apache License 2.0
// @file: roadmap.js
// @time: 2026/10/28

/* 开发路线图（与「变更排期」联动）。
   联动点：① 里程碑可一键创建「平台发布」排期；发布成功 → 里程碑已发布 → 其下任务自动完成 → 排期里对应能力变为「已上线」
          ② 排期的冻结期画在甘特图上，与生产发布冲突的任务 / 里程碑自动标红，并给出顺延建议
          ③ 甘特图叠加「已排期的运维变更」密度；任务可跳转到对应的原型页面 */
const RM_PHASES = {
  P0: {name: 'P0 原型与方案', color: '--c6'}, P1: {name: 'P1 只读对接', color: '--c1'}, P2: {name: 'P2 诊断引擎', color: '--c4'},
  P3: {name: 'P3 受控写操作', color: '--c5'}, P4: {name: 'P4 排期与自动化', color: '--c3'}, P5: {name: 'P5 上线与运营', color: '--c2'},
};
/* ms: 该任务随哪个里程碑发布；prod: 涉及生产环境的变更（受冻结期约束） */
const RM_TASKS = [
  {id: 'A1', ph: 'P0', name: '交互原型（总览 / 架构 / 诊断 / SQL / 热点 / 排障 / 节点 / 配置 / 维护 / 排期）', owner: '前端', start: 1, dur: 2, effort: 15, prio: 'P0', deps: [], status: 'done', ms: 'M1', deliver: '可演示的 Web 原型（本项目）'},
  {id: 'A2', ph: 'P0', name: '需求评审与范围确认（对象集群、版本、环境）', owner: '全员', start: 1, dur: 2, effort: 5, prio: 'P0', deps: [], status: 'done', ms: 'M1', deliver: '需求清单、版本兼容矩阵'},
  {id: 'A3', ph: 'P0', name: '权限模型与安全方案（RBAC、审计、凭据托管、中控机同机部署安全基线）', owner: '后端', start: 2, dur: 2, effort: 8, prio: 'P0', deps: ['A2'], status: 'done', ms: 'M1', deliver: '安全设计文档、权限矩阵'},

  {id: 'B1', ph: 'P1', name: '数据采集适配层（Go；Source 接口含 live / replay 两种实现；Prometheus / PD API / TiDB Status API / information_schema）', owner: '后端', start: 3, dur: 4, effort: 20, prio: 'P0', deps: ['A3', 'B7'], status: 'doing', ms: 'M1', deliver: '统一指标与元数据接口'},
  {id: 'B7', ph: 'P1', name: '样例数据导出脚本与 fixture 录制 / 回放（生产不能直连）', owner: '后端', start: 3, dur: 2, effort: 5, prio: 'P0', deps: ['A2'], status: 'doing', ms: 'M1', deliver: '只读导出工具 tidbops-fixtures（Go）、fixture 格式规范、回放 Source'},
  {id: 'B8', ph: 'P1', name: '环境探测与设置中心（自动获取中控机 tiup / TiDB 版本与拓扑，用户可覆盖）', owner: '后端', start: 5, dur: 2, effort: 8, prio: 'P0', deps: ['B2', 'B7'], status: 'doing', ms: 'M1', deliver: '探测服务、设置页、版本兼容矩阵'},
  {id: 'B2', ph: 'P1', name: '集群拓扑与实例发现（tiup meta / PD）', owner: '后端', start: 3, dur: 3, effort: 10, prio: 'P0', deps: ['A2'], status: 'doing', ms: 'M1', deliver: '拓扑服务、实例清单同步'},
  {id: 'B3', ph: 'P1', name: '总览与架构图真实数据化', owner: '前端', start: 6, dur: 3, effort: 12, prio: 'P0', deps: ['B1', 'B2'], status: 'doing', ms: 'M1', deliver: '总览、架构图接入真实数据'},
  {id: 'B4', ph: 'P1', name: '慢查询 / Statement Summary / Top SQL 接入与执行计划分析', owner: '后端', start: 6, dur: 4, effort: 15, prio: 'P0', deps: ['B1'], status: 'doing', ms: 'M1', deliver: 'SQL 优化页真实数据'},
  {id: 'B5', ph: 'P1', name: '热点分析（hot-regions / Key Visualizer 数据）', owner: '后端', start: 7, dur: 3, effort: 8, prio: 'P1', deps: ['B1'], status: 'doing', ms: 'M1', deliver: '热点热力图与 Top Region'},
  {id: 'B6', ph: 'P1', name: '告警对接（Alertmanager）与事件时间线', owner: '后端', start: 7, dur: 2, effort: 5, prio: 'P1', deps: ['B1'], status: 'doing', ms: 'M1', deliver: '告警列表、静默、关联诊断'},

  {id: 'C1', ph: 'P2', name: '规则引擎与诊断规则库（DB Time 分解、故障排查地图规则化）', owner: '后端', start: 9, dur: 5, effort: 25, prio: 'P0', deps: ['B1'], status: 'doing', ms: 'M2', deliver: '≥ 40 条诊断规则，带证据与建议'},
  {id: 'C2', ph: 'P2', name: '排障向导与知识库（决策树、日志错误速查）', owner: '前端', start: 11, dur: 3, effort: 8, prio: 'P1', deps: ['C1'], status: 'plan', ms: 'M2', deliver: '知识库与向导 UI'},
  {id: 'C3', ph: 'P2', name: '巡检任务与报告导出', owner: '后端', start: 12, dur: 3, effort: 10, prio: 'P1', deps: ['C1'], status: 'doing', ms: 'M2', deliver: '定时巡检、PDF/Markdown 报告'},

  {id: 'D1', ph: 'P3', name: '执行器（同机，特权分离：Web 进程 tidbops + sudo 受限 exec 入口）：tiup / pd-ctl 命令白名单、审批令牌校验、任务队列、幂等 / 超时 / 重试、审计日志', owner: '后端', start: 10, dur: 6, effort: 28, prio: 'P0', deps: ['A3'], status: 'doing', ms: 'M3', deliver: '可控、可审计的执行底座'},
  {id: 'D2', ph: 'P3', name: 'RBAC 与审批流（申请人不可自批、分级审批）', owner: '后端', start: 10, dur: 5, effort: 15, prio: 'P0', deps: ['A3'], status: 'doing', ms: 'M3', deliver: '角色权限、审批流引擎'},
  {id: 'D3', ph: 'P3', name: '单实例操作（重启 / 停止 / 启动 / 驱逐 Leader）', owner: '后端', start: 15, dur: 3, effort: 10, prio: 'P0', deps: ['D1', 'D2'], status: 'doing', ms: 'M3', deliver: '节点操作上线（测试环境）'},
  {id: 'D4', ph: 'P3', name: '扩缩容流程（预检 / 评估 / 执行 / 回滚）', owner: '后端', start: 16, dur: 5, effort: 25, prio: 'P0', deps: ['D1', 'D2', 'B2'], status: 'doing', ms: 'M3', deliver: '扩容、缩容、新增主机'},
  {id: 'D5', ph: 'P3', name: '配置变更（SET CONFIG / edit-config / reload / 回滚 / 漂移检测）', owner: '后端', start: 17, dur: 4, effort: 15, prio: 'P0', deps: ['D1'], status: 'doing', ms: 'M3', deliver: '配置管理与历史'},
  {id: 'D6', ph: 'P3', name: '主机维护流程（评估 / 排空 / 倒计时 / 恢复验证）', owner: '后端', start: 19, dur: 4, effort: 18, prio: 'P1', deps: ['D3', 'D4'], status: 'doing', ms: 'M3', deliver: '主机维护全流程'},
  {id: 'D7', ph: 'P3', name: '备份恢复（BR / PITR 管理与恢复演练）', owner: 'DBA/SRE', start: 20, dur: 4, effort: 15, prio: 'P1', deps: ['D1'], status: 'doing', ms: 'M4', deliver: '备份策略、恢复演练报告'},

  {id: 'E1', ph: 'P4', name: '变更排期：窗口规则、冲突检测、推荐窗口、冻结期', owner: '后端', start: 21, dur: 4, effort: 15, prio: 'P0', deps: ['D2', 'D6'], status: 'doing', ms: 'M4', deliver: '日历、冲突检测、到点触发'},
  {id: 'E2', ph: 'P4', name: '到点自动执行与通知（飞书 / 钉钉 / Slack / 邮件）', owner: '后端', start: 23, dur: 3, effort: 8, prio: 'P1', deps: ['E1'], status: 'doing', ms: 'M4', deliver: '自动执行、提醒、失败回滚通知'},
  {id: 'E3', ph: 'P4', name: '容量预测与扩容建议（磁盘 / Region / QPS 趋势）', owner: '后端', start: 23, dur: 4, effort: 12, prio: 'P1', deps: ['B1'], status: 'doing', ms: 'M5', deliver: '容量报告与扩容提示'},
  {id: 'E4', ph: 'P4', name: 'SQL 与参数智能建议（索引推荐、Binding 管理、参数基线）', owner: 'DBA/SRE', start: 23, dur: 4, effort: 15, prio: 'P2', deps: ['C1', 'B4'], status: 'plan', ms: 'M5', deliver: '优化建议闭环'},

  {id: 'F1', ph: 'P5', name: '压测与大集群（百万 Region / 百实例）性能优化', owner: '后端', start: 24, dur: 3, effort: 12, prio: 'P1', deps: ['B3', 'D4'], status: 'plan', ms: 'M5', deliver: '性能基线与优化报告'},
  {id: 'F2', ph: 'P5', name: '安全加固与渗透测试、密钥与权限评审', owner: '测试', start: 24, dur: 3, effort: 8, prio: 'P0', deps: ['D2'], status: 'plan', ms: 'M5', deliver: '安全评审通过'},
  {id: 'F3', ph: 'P5', name: '生产灰度发布与回滚预案（受变更冻结期约束）', owner: '全员', start: 26, dur: 3, effort: 12, prio: 'P0', deps: ['F1', 'F2', 'E1'], status: 'plan', ms: 'M5', prod: true, deliver: '生产灰度上线'},
  {id: 'F4', ph: 'P5', name: '文档、培训与运维手册', owner: 'DBA/SRE', start: 26, dur: 3, effort: 8, prio: 'P1', deps: ['E1'], status: 'plan', ms: 'M5', deliver: '用户手册、值班手册、培训'},
];
const RM_MILESTONES = [
  {id: 'M1', week: 9, name: '只读版内测', env: '测试环境', accept: ['总览 / 架构 / SQL / 热点 使用真实数据', '覆盖 ≥ 2 个真实集群，数据延迟 < 30s', '无任何写操作入口']},
  {id: 'M2', week: 14, name: '诊断能力上线', env: '测试环境', accept: ['≥ 40 条诊断规则，误报率 < 10%', '巡检报告可导出', '排障向导覆盖故障排查地图主要场景']},
  {id: 'M3', week: 22, name: '受控写操作（测试环境）', env: '测试环境', accept: ['扩缩容 / 配置 / 维护在测试集群完整演练', '全部操作有审批与审计', '失败可回滚或可中止']},
  {id: 'M4', week: 25, name: '排期与审批上线', env: '预发', accept: ['窗口规则与冲突检测生效', '到点自动执行成功率 ≥ 99%', '通知渠道打通']},
  {id: 'M5', week: 28, name: '生产灰度 GA', env: '生产', accept: ['安全评审通过，压测达标', '生产 1 个集群灰度 ≥ 2 周无严重事故', '文档与值班手册交付']},
];
/* 原型已覆盖的能力 → 跳转到原型页面 */
const RM_PROTO = {
  A1: {label: '全部页面', page: 'overview'}, A3: {label: '权限与审计', page: 'security'}, B8: {label: '环境与设置', page: 'settings'}, B3: {label: '总览 / 架构图', page: 'arch'}, B4: {label: 'SQL 优化', page: 'sql'}, B5: {label: '热点与调度', page: 'hotspot'}, B6: {label: '总览告警', page: 'overview', part: true},
  C1: {label: '智能诊断', page: 'diag'}, C2: {label: '故障排查', page: 'trouble'}, C3: {label: '巡检', page: 'ops', tab: 'inspect'}, D3: {label: '节点实例操作', page: 'nodes', tab: 'inst'},
  D4: {label: '扩缩容向导', page: 'nodes', tab: 'scaleout'}, D5: {label: '节点配置变更', page: 'nodes', tab: 'inst'}, D6: {label: '主机维护', page: 'nodes', tab: 'maint'}, D7: {label: '备份恢复', page: 'ops', tab: 'backup', part: true},
  E1: {label: '变更排期', page: 'schedule'}, E2: {label: '到点自动执行', page: 'schedule', part: true}, E4: {label: 'SQL 体检', page: 'sql', part: true},
};
const RM_RISKS = [
  ['写操作误伤生产', '高', '强制审批 + 预检 + dry-run（影子模式）+ 全量审计；先测试环境、再灰度；所有写操作可中止', '后端 / DBA'],
  ['TiDB / tiup / PD API 版本差异', '中', '适配层抽象 + 版本兼容矩阵 + 每个版本的回归用例', '后端'],
  ['平台与 tiup 同机部署（中控机）：平台被攻破 = 生产被攻破', '高', '专用低权限账号、命令白名单（禁止拼接 shell）、写操作默认关闭、审计只追加、凭据托管、仅内网监听；SSH 私钥不进入平台（tiup 在部署用户下自己使用）、手工添加的远程集群在结构上只读（不配执行器）；后续可演进为独立 agent', '后端 / 运维'],
  ['大集群（Region / 实例数多）下接口变慢', '中', '采样与聚合、结果缓存、分页，F1 阶段专项压测', '后端'],
  ['诊断规则误报引发信任下降', '中', '规则灰度与开关、用户反馈闭环、证据可追溯', 'DBA/SRE'],
  ['生产发布撞上变更冻结期', '高', '路线图与排期联动：冻结期自动标红冲突任务，发布排期受冻结规则约束，提供顺延建议', 'PM / 后端'],
];
const rmState = {phase: 'all', released: {}, shifted: false};
const RM_ORIG = {tasks: RM_TASKS.map(t => ({id: t.id, start: t.start})), ms: RM_MILESTONES.map(m => ({id: m.id, week: m.week}))};
const rmEnd = t => t.start + t.dur - 1;
const rmTask = id => RM_TASKS.find(t => t.id === id);
const rmMs = id => RM_MILESTONES.find(m => m.id === id);
const rmWeeks = () => Math.max(28, ...RM_TASKS.map(rmEnd), ...RM_MILESTONES.map(m => m.week));
const rmMsDate = m => planWeekStart(m.week) + 3 * DAY + 3 * HOUR;      // 里程碑发布默认：该周周四 03:00
const rmWeekDate = w => { const d = new Date(planWeekStart(w)); return `${d.getMonth() + 1}/${d.getDate()}`; };
const rmWeekOf = t => Math.floor((t - PLAN_W1) / (7 * DAY)) + 1;
const rmMsSched = m => SCHEDULES.filter(s => s.type === 'release' && s.payload.ms === m.id && ['pending', 'approved', 'ready', 'running', 'done'].includes(s.status)).sort((a, b) => b.start - a.start)[0];
const rmMsInFreeze = m => freezeOver(rmMsDate(m), rmMsDate(m) + HOUR);
const rmTaskFreeze = t => t.prod ? freezeOver(planWeekStart(t.start), planWeekStart(rmEnd(t) + 1)) : [];

function rmRelease(msId) {
  rmState.released[msId] = true;
  RM_TASKS.filter(t => t.ms === msId && t.status !== 'done').forEach(t => { t.status = 'done'; });
  toast(`🚀 ${msId} 已发布：路线图任务已自动标记完成`);
  if (typeof schedRefresh === 'function') schedRefresh();
}
function rmMsStatus(m) {
  if (rmState.released[m.id]) return ['ok', '已发布'];
  const s = rmMsSched(m); if (!s) return ['bad', '未排期'];
  return s.status === 'running' ? ['info', '发布中'] : s.status === 'pending' ? ['info', '已排期 · 待审批'] : ['warn', '已排期'];
}
function rmShift() {
  const f3 = rmTask('F3'), fz = rmTaskFreeze(f3)[0]; if (!fz) return;
  f3.start = rmWeekOf(fz.to); const m5 = rmMs('M5'); m5.week = rmEnd(f3); rmState.shifted = true;
  const s = rmMsSched(m5); if (s && ['pending', 'approved', 'ready'].includes(s.status)) rescheduleSched(s, rmMsDate(m5));
  toast(`已顺延：F3 调整为 W${f3.start}–W${rmEnd(f3)}，M5 调整到 W${m5.week}`);
}
function rmUnshift() {
  RM_ORIG.tasks.forEach(o => { rmTask(o.id).start = o.start; }); RM_ORIG.ms.forEach(o => { rmMs(o.id).week = o.week; }); rmState.shifted = false; toast('已撤销顺延');
}
/* 联动检查 */
function rmLinkChecks() {
  const out = [];
  RM_TASKS.forEach(t => rmTaskFreeze(t).forEach(f => out.push({lv: 'bad', t: `${t.id}「${t.name.slice(0, 16)}…」（W${t.start}–W${rmEnd(t)}）涉及生产变更，与「${f.label}」（${fD(f.from)}–${fD(f.to - DAY)}）冲突`, shift: true})));
  RM_MILESTONES.forEach(m => {
    if (rmMsInFreeze(m).length && m.env === '生产' && !rmState.released[m.id]) out.push({lv: 'bad', t: `${m.id} ${m.name} 的默认发布日（${fDT(rmMsDate(m))}）落在「${rmMsInFreeze(m)[0].label}」内，生产发布会被排期规则拦下`, shift: true});
    const s = rmMsSched(m); if (s && !rmState.released[m.id] && Math.abs(s.start - rmMsDate(m)) > 3 * DAY) out.push({lv: 'warn', t: `${m.id} 的发布排期 ${s.id}（${fDT(s.start)}）与路线图计划日 ${fDT(rmMsDate(m))} 相差超过 3 天`});
  });
  const un = RM_MILESTONES.filter(m => !rmState.released[m.id] && !rmMsSched(m)); if (un.length) out.push({lv: 'info', t: `${un.map(m => m.id).join('、')} 尚未创建发布排期（可在下方里程碑卡片一键创建）`});
  const cnt = {}; SCHEDULES.filter(s => SCHED_ACTIVE.includes(s.status) && SCHED_CAP[s.type]).forEach(s => { const c = capInfo(s.type); if (c && !c.live) cnt[c.id] = (cnt[c.id] || 0) + 1; });
  const ks = Object.keys(cnt); if (ks.length) out.push({lv: 'info', t: `当前排期中有 ${ks.reduce((a, k) => a + cnt[k], 0)} 项变更使用了尚未发布的能力（${ks.map(k => `${k}×${cnt[k]}`).join('、')}）：仅限原型 / 测试环境演示，生产需等对应里程碑发布`});
  if (rmState.shifted) out.push({lv: 'ok', t: 'F3 / M5 已顺延到冻结期之后，当前计划与排期规则无冲突'});
  return out;
}
function rmMarkdown() {
  const L = ['# TiDB 运维平台开发路线图', '', `共 ${rmWeeks()} 周 · ${RM_TASKS.reduce((a, t) => a + t.effort, 0)} 人天 · ${RM_MILESTONES.length} 个里程碑（W1 = ${rmWeekDate(1)}）`, '', '## 里程碑', '', ...RM_MILESTONES.map(m => `- **${m.id} W${m.week} ${m.name}**（${m.env}，${rmMsStatus(m)[1]}）：${m.accept.join('；')}`), ''];
  Object.entries(RM_PHASES).forEach(([k, p]) => { L.push(`## ${p.name}`, '', '| 编号 | 任务 | 负责 | 周期 | 人天 | 优先级 | 依赖 | 状态 | 交付物 |', '|---|---|---|---|---|---|---|---|---|', ...RM_TASKS.filter(t => t.ph === k).map(t => `| ${t.id} | ${t.name} | ${t.owner} | W${t.start}–W${rmEnd(t)} | ${t.effort} | ${t.prio} | ${t.deps.join(',') || '—'} | ${{done: '已完成', doing: '进行中', plan: '计划'}[t.status]} | ${t.deliver} |`), ''); });
  L.push('## 风险与应对', '', ...RM_RISKS.map(r => `- **${r[0]}**（${r[1]}）：${r[2]}`));
  return L.join('\n');
}
function rmCritical() {
  const last = RM_TASKS.slice().sort((a, b) => rmEnd(b) - rmEnd(a))[0], path = [last]; let cur = last;
  while (cur.deps.length) { cur = cur.deps.map(rmTask).sort((a, b) => rmEnd(b) - rmEnd(a))[0]; path.unshift(cur); }
  return path;
}
