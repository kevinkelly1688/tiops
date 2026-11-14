// @author: kevinkelly
// @license: Apache License 2.0
// @file: maint.js
// @time: 2026/10/28

/* 主机维护完整流程：计划 → 评估 → 排空并停机 → 维护中（倒计时 / 检查单 / 阈值告警 / 延长窗口）→ 恢复验证 → 归档。
   演示时间加速：1 秒 = 1 分钟。 */
const MAINTS = [];
let maintSeq = 1;
const mtState = {step: 1, ip: null, type: 'kernel', mins: 25, reason: '', ticket: '', keepData: true, strategy: 'auto', ack: false, confirm: ''};
const hostInsts = ip => NODES.filter(n => n.host === ip);
const ORDER_STOP = ['tidb', 'ticdc', 'tiflash', 'tikv', 'pd'];
const ORDER_START = ['pd', 'tikv', 'tiflash', 'ticdc', 'tidb'];
const MT_STATE_TAG = {draining: ['info', '进入维护中'], active: ['warn', '维护中'], recovering: ['info', '恢复中'], done: ['ok', '已完成'], cancelled: ['', '已取消']};
const isLive = m => MAINT_STATES.includes(m.state);
const sortBy = (list, order) => list.slice().sort((a, b) => order.indexOf(a.type) - order.indexOf(b.type));
const addrsOf = ns => ns.map(n => `${n.host}:${n.port}`).join(',');
const mtReset = () => Object.assign(mtState, {step: 1, ip: null, type: 'kernel', mins: 25, reason: '', ticket: '', keepData: true, strategy: 'auto', ack: false, confirm: ''});
const mtTypes = () => OP.on && Object.keys(OP.maintTypes || {}).length ? OP.maintTypes : MAINT_TYPES;   // 运维模式：维护类型取自服务端
const mtRefresh = () => { if (location.hash === '#nodes' && nodesState.tab === 'maint') pages.nodes.body(); else if (location.hash === '#arch') pages.arch.draw(); updateBadge(); };

function mtStart(ip) {
  Object.assign(mtState, {step: 1, ip: ip || null, type: 'kernel', mins: MAINT_TYPES.kernel.def, reason: '', ticket: '', keepData: true, strategy: 'auto', ack: false, confirm: ''});
  setTab('maint');
}
function recStrategy(mins) { return mins > pdThreshMin() - 5 ? 'extend' : 'standard'; }
const effStrategy = (hasTikv, mins) => !hasTikv ? 'standard' : mtState.strategy === 'auto' ? recStrategy(mins) : mtState.strategy;

/* ---------- 评估 ---------- */
function evalMaint(ip, mins, keepData, type) {
  const ns = hostInsts(ip), items = [], add = (lv, t) => items.push({lv, t});
  const thr = pdThreshMin(), hasTikv = ns.some(n => n.type === 'tikv'), others = NODES.filter(n => n.host !== ip);
  if (!ns.length) add('bad', '该主机上没有实例');
  MAINTS.filter(m => isLive(m) && m.ip !== ip).forEach(m => {
    const oT = m.nodes.some(id => { const x = nodeBy(id); return x && x.type === 'tikv'; });
    if (hasTikv && oT) add('bad', `主机 ${m.ip} 的 TiKV 正在维护：同一时刻只允许一台 TiKV 主机维护，否则部分 Region 会同时失去 2 个副本`);
    else add('warn', `主机 ${m.ip} 也在维护中，请确认两台同时下线不影响多数派与服务容量`);
  });
  ns.forEach(n => {
    if (busy(n.id)) add('bad', `${n.id} 有进行中的任务，请先等待或中止`);
    if (n.status === 'joining' || n.status === 'offline') add('bad', `${n.id} 处于「${stText[n.status]}」，请等待完成后再维护`);
    const left = others.filter(x => x.type === n.type && (x.status === 'up' || x.status === 'warn'));
    if (n.type === 'tidb') {
      if (!left.length) add('bad', `${n.id} 是唯一健康的 TiDB，维护期间 SQL 服务将中断`);
      else add('info', `${n.id}：约 ${n.conn} 个连接会被排空并重连；其余 ${left.length} 个 TiDB 平均 CPU 预计 ${Math.round((sumBy(left, 'cpu') + n.cpu) / left.length)}%`);
    } else if (n.type === 'pd') {
      if (left.length < 2) add('bad', `停掉 ${n.id} 后 PD 仅剩 ${left.length} 个，失去多数派，集群将不可用`);
      else add('ok', `停掉 ${n.id} 后 PD 仍有 ${left.length} 个健康成员，保持多数派`);
      if (n.leader) add('warn', `${n.id} 是 PD Leader，将先 transfer leader，TSO 有秒级抖动`);
    } else if (n.type === 'tikv') {
      if (left.length < 3) add('bad', `停掉 ${n.id} 后健康 TiKV 仅 ${left.length} 个，小于 3，副本无法满足多数派`);
      else add('ok', `停掉 ${n.id} 后仍有 ${left.length} 个健康 TiKV，Raft 多数派不受影响`);
      add('info', `${n.id}：将驱逐 ${fmt(n.leaders)} 个 Leader；约 ${fmt(n.regions)} 个 Region 在维护期间只有 2 个副本（仍可读写，但容错下降）`);
      const hot = left.filter(x => x.disk >= 80); if (hot.length) add('warn', `${hot.map(x => x.id).join('、')} 磁盘 ≥ 80%，若维护超时触发补副本，这些节点承接数据的能力受限`);
    } else if (n.type === 'tiflash') {
      if (!left.length) add('warn', `${n.id} 是唯一的 TiFlash，维护期间列存 / MPP 查询将回退到 TiKV 或失败`); else add('ok', `其余 ${left.length} 个 TiFlash 可继续提供列存副本`);
    } else if (n.type === 'ticdc') {
      if (!left.length) add('warn', `${n.id} 是唯一的 TiCDC，维护期间 changefeed 会中断，同步延迟将增长约 ${mins} 分钟`); else add('info', `${n.id} 的 changefeed 将迁移到其余 ${left.length} 个 capture`);
    }
  });
  if (hasTikv) {
    if (mins > thr - 5) add('warn', `计划 ${mins} 分钟已接近 / 超过 PD 的 max-store-down-time（${thr} 分钟）：超时后 PD 会判定 Store Down 并开始补副本。建议临时调大该阈值`);
    else add('ok', `计划 ${mins} 分钟小于 max-store-down-time（${thr} 分钟），不会触发补副本`);
  }
  const hr = new Date().getHours(); if (hr >= 1 && hr <= 3) add('warn', '当前时间接近每日 02:00 的全量备份窗口，建议错开');
  if (!keepData) add('bad', `「${MAINT_TYPES[type].label}」会丢失数据盘数据：这已不是「停机 → 恢复」，请改走「缩容下线 + 扩容新增」替换节点的流程`);
  return {items, blocked: items.some(i => i.lv === 'bad'), hasTikv, thr, rec: recStrategy(mins), ns};
}

/* ---------- 命令 / 步骤 ---------- */
function enterCmds(m) {
  const ns = m.nodes.map(nodeBy).filter(Boolean), c = [];
  if (m.strategy === 'extend') c.push(`${PDCTL()} config set max-store-down-time ${m.limit}m   # 临时调大，结束后恢复为 ${m.origThresh}`);
  sortBy(ns.filter(n => n.type === 'tidb'), ORDER_STOP).length && c.push(`# 从 HAProxy / TiProxy 后端摘除 ${addrsOf(ns.filter(n => n.type === 'tidb'))}，等待连接排空`);
  ns.filter(n => n.type === 'pd' && n.leader).forEach(n => { const o = NODES.find(x => x.type === 'pd' && x !== n); c.push(`${PDCTL()} member leader transfer ${o ? o.id : '<other-pd>'}`); });
  ns.filter(n => n.type === 'ticdc').length && c.push(`tiup cdc cli capture list --server=http://${(() => { const cdc = LIVE.on ? (NODES.find(x => x.type === 'ticdc' && !ns.includes(x)) || NODES.find(x => x.type === 'ticdc')) : null; return cdc ? cdc.host + ':' + cdc.port : '10.0.4.11:8300'; })()}   # 确认 changefeed 已迁移`);
  ns.filter(n => n.type === 'tikv').forEach(n => c.push(`${PDCTL()} scheduler add evict-leader-scheduler ${n.sid}`));
  c.push(`tiup cluster stop ${CLUSTER} -N ${addrsOf(sortBy(ns, ORDER_STOP))}`);
  return c;
}
function exitCmds(m) {
  const ns = m.nodes.map(nodeBy).filter(Boolean), c = [`tiup cluster check ${CLUSTER} --cluster   # 重启后复查主机参数`, `tiup cluster start ${CLUSTER} -N ${addrsOf(sortBy(ns, ORDER_START))}`];
  ns.filter(n => n.type === 'tikv').forEach(n => c.push(`${PDCTL()} scheduler remove evict-leader-scheduler-${n.sid}`));
  if (m.strategy === 'extend') c.push(`${PDCTL()} config set max-store-down-time ${m.origThresh}m   # 恢复阈值`);
  if (ns.some(n => n.type === 'tidb')) c.push('# 将 TiDB 重新加入负载均衡后端并恢复流量');
  c.push(`${PDCTL()} region --jq='.regions[] | select(.down_peers != null or .pending_peers != null) | .id'   # 应无输出`);
  return c;
}
function buildEnter(m) {
  const ns = m.nodes.map(nodeBy).filter(Boolean), tidbs = ns.filter(n => n.type === 'tidb'), tikvs = ns.filter(n => n.type === 'tikv'), pdl = ns.filter(n => n.type === 'pd' && n.leader);
  m.snap = {leaders: {}, conn: {}, qps: {}}; ns.forEach(n => { m.snap.leaders[n.id] = n.leaders; m.snap.conn[n.id] = n.conn; m.snap.qps[n.id] = n.qps; });
  const steps = [{name: '最终前置检查', ms: 700, log: '多数派 / 容量 / 拓扑检查通过'}];
  if (m.strategy === 'extend') steps.push({name: `临时调大 max-store-down-time：${m.origThresh}m → ${m.limit}m`, ms: 800, end: () => { clusterCfg('pd')['schedule.max-store-down-time'] = `${m.limit}m`; }});
  if (tidbs.length) steps.push({name: '从负载均衡摘除并排空连接', ms: 2200, ticks: 6, fn: p => tidbs.forEach(n => { n.conn = Math.round(m.snap.conn[n.id] * (1 - p)); n.qps = Math.round(m.snap.qps[n.id] * (1 - p)); })});
  if (pdl.length) steps.push({name: '迁移 PD Leader（member leader transfer）', ms: 900, end: () => { const o = NODES.find(x => x.type === 'pd' && !m.nodes.includes(x.id)); pdl.forEach(n => { n.leader = false; }); if (o) { o.leader = true; o.note = 'PD Leader'; } }});
  if (ns.some(n => n.type === 'ticdc')) steps.push({name: '迁移 changefeed capture 到其他 TiCDC', ms: 1200});
  if (tikvs.length) steps.push({name: '驱逐 Leader（evict-leader-scheduler）', ms: 1800, ticks: 5, fn: p => tikvs.forEach(n => { n.leaders = Math.round(m.snap.leaders[n.id] * (1 - p)); })});
  if (ns.some(n => n.type === 'tiflash')) steps.push({name: '确认 TiFlash 查询回退策略', ms: 600});
  steps.push({name: `停止实例（${sortBy(ns, ORDER_STOP).map(n => n.id).join(' → ')}）`, ms: 1200, end: () => {
    ns.forEach(n => { n.status = 'down'; n.note = `主机维护中（${m.id}）`; }); m.state = 'active'; m.elapsed = 0; m.alerted = false; mlog(m, '实例已全部停止，进入维护窗口，开始计时'); mtRefresh();
  }});
  return {title: `进入维护 ${m.ip}（${m.typeLabel}）`, cmds: enterCmds(m), steps, targets: m.nodes, abortable: true,
    onAbort: t => { ns.forEach(n => { if (n.status === 'down') { n.status = 'up'; n.note = ''; } n.leaders = m.snap.leaders[n.id]; n.conn = m.snap.conn[n.id]; n.qps = m.snap.qps[n.id]; }); if (m.strategy === 'extend') clusterCfg('pd')['schedule.max-store-down-time'] = `${m.origThresh}m`; m.state = 'cancelled'; m.ended = nowStr(); mlog(m, '进入维护被中止，已恢复实例与阈值'); tlog(t, '已恢复：实例 Up、Leader 与连接、阈值'); mtRefresh(); }};
}
function buildExit(m, urgent) {
  const ns = m.nodes.map(nodeBy).filter(Boolean), tikvs = ns.filter(n => n.type === 'tikv'), tidbs = ns.filter(n => n.type === 'tidb'), ord = sortBy(ns, ORDER_START);
  const steps = [{name: '主机预检（tiup cluster check：重启后参数是否还原）', ms: 1300, start: () => { m.state = 'recovering'; mlog(m, `${urgent ? '紧急' : '正常'}结束维护，开始恢复（已维护 ${m.elapsed} 分钟）`); mtRefresh(); }, log: urgent ? '已跳过部分检查（紧急恢复）' : '预检通过：THP / Swap / sysctl / NTP 均正常'},
    {name: `按依赖顺序启动实例（${ord.map(n => n.id).join(' → ')}）`, ms: 1800, ticks: ord.length, fn: p => ord.forEach((n, i) => { if (i < Math.ceil(p * ord.length)) { n.status = 'up'; n.note = ''; } })},
    {name: '健康检查（Store Up / 实例就绪）', ms: 800}];
  if (tikvs.length) steps.push({name: '移除 Leader 驱逐调度，Leader 回迁', ms: 2000, ticks: 5, fn: p => tikvs.forEach(n => { n.leaders = Math.round(m.snap.leaders[n.id] * p); })});
  if (m.strategy === 'extend') steps.push({name: `恢复 max-store-down-time = ${m.origThresh}m`, ms: 700, end: () => { clusterCfg('pd')['schedule.max-store-down-time'] = `${m.origThresh}m`; }});
  if (tidbs.length) steps.push({name: '重新加入负载均衡并恢复流量', ms: 2000, ticks: 5, fn: p => tidbs.forEach(n => { n.conn = Math.round(m.snap.conn[n.id] * p); n.qps = Math.round(m.snap.qps[n.id] * p); })});
  if (m.alerted) steps.push({name: '清理维护期间 PD 补出的多余副本（balance-region）', ms: 2400, ticks: 6});
  steps.push({name: '验证：无 down-peer / pending-peer，Region 状态正常', ms: 900, end: () => { m.state = 'done'; m.ended = nowStr(); m.actual = m.elapsed; mlog(m, `维护完成：计划 ${m.mins + m.extendedBy} 分钟，实际 ${m.elapsed} 分钟`); toast(`主机 ${m.ip} 维护已完成`); mtRefresh(); }, log: '集群状态正常'});
  return {title: `结束维护 ${m.ip}`, cmds: exitCmds(m), steps, targets: m.nodes, abortable: false};
}
function mlog(m, msg) { m.log.push(`[${nowStr()}] ${msg}`); }

/* ---------- 提交 / 结束 / 延长 ---------- */
function mtSubmit(quiet) {
  if (OP.on) {   // 运维模式：不在前端评估，由服务端规划 / 检查 / 审批
    const S = mtState; if (!S.ip || !(+S.mins > 0)) return;
    const T = mtTypes()[S.type] || {label: S.type}, reason = (S.ticket ? `[${S.ticket}] ` : '') + S.reason.trim();
    opIntentDrawer({kind: 'maint.enter', params: {ip: S.ip, type: S.type, mins: +S.mins, keepData: S.keepData, reason, strategy: S.strategy}}, `进入维护 ${S.ip}（${T.label}）`);
    return;
  }
  const ev = evalMaint(mtState.ip, mtState.mins, mtState.keepData, mtState.type); if (ev.blocked) return;
  const strategy = effStrategy(ev.hasTikv, mtState.mins), orig = ev.thr, T = MAINT_TYPES[mtState.type];
  const m = {id: 'M' + String(maintSeq++).padStart(4, '0'), ip: mtState.ip, type: mtState.type, typeLabel: T.label, mins: +mtState.mins, reason: mtState.reason, ticket: mtState.ticket, strategy, origThresh: orig,
    limit: strategy === 'extend' ? Math.max(orig, +mtState.mins + 30) : orig, hasTikv: ev.hasTikv, state: 'draining', elapsed: 0, extendedBy: 0, alerted: false, nodes: ev.ns.map(n => n.id), todo: T.todo.map(t => ({t, done: false})), log: [], started: nowStr(), ended: null};
  mlog(m, `创建维护计划：${T.label}，计划 ${m.mins} 分钟，策略 ${strategy === 'extend' ? '延长 max-store-down-time 至 ' + m.limit + 'm' : '标准（阈值 ' + orig + 'm）'}`);
  MAINTS.unshift(m);
  const plan = buildEnter(m); mtState.step = 1; mtState.ip = null; mtState.reason = ''; mtState.ticket = ''; mtState.ack = false; mtState.confirm = '';
  const t = startTask(plan); m.taskId = t.id; if (!quiet) toast('维护已开始：正在排空并停机'); mtRefresh(); if (!quiet && nodesState.tab !== 'maint') setTab('maint');
}
function mtEnd(id, urgent) {
  if (OP.on) { const m = OP.maints.find(x => x.id === id); if (m) opIntentDrawer({kind: 'maint.exit', params: {maintId: id, urgent: !!urgent}}, `${urgent ? '紧急恢复' : '结束维护'} ${m.ip}`); return; }
  const m = MAINTS.find(x => x.id === id); if (!m) return;
  const undone = m.todo.filter(x => !x.done), checks = [];
  if (undone.length && !urgent) checks.push(['warn', `检查单还有 ${undone.length} 项未勾选：${undone.map(x => x.t).join('；')}`]);
  if (urgent) checks.push(['warn', '紧急恢复：跳过检查单，直接启动实例；请在恢复后补做主机预检']);
  if (m.alerted) checks.push(['warn', `维护已超过 PD 阈值（${m.limit} 分钟），PD 已为该 Store 补副本；恢复后会有额外的 Region 均衡`]);
  if (m.elapsed > m.mins + m.extendedBy) checks.push(['info', `已超出计划窗口 ${m.elapsed - m.mins - m.extendedBy} 分钟`]);
  checks.push(['ok', '先预检主机、再按 pd → tikv → tiflash → ticdc → tidb 顺序启动，最后做 Region 健康验证']);
  const plan = buildExit(m, urgent); planDrawer({...plan, risk: 'mid', checks, stay: true});
}
function mtExtend(id, add) {
  if (OP.on) { const m = OP.maints.find(x => x.id === id); if (m) opIntentDrawer({kind: 'maint.extend', params: {maintId: id, add: +add}}, `延长维护窗口 +${add} 分钟（${m.ip}）`); return; }
  const m = MAINTS.find(x => x.id === id); if (!m) return;
  const newEnd = m.mins + m.extendedBy + add, needLimit = m.hasTikv && m.limit < newEnd, checks = [];
  if (m.alerted) checks.push(['warn', 'PD 已判定 Store Down 并开始补副本，延长窗口无法撤销，只能延后恢复时间']);
  if (needLimit) checks.push(['warn', `新窗口 ${newEnd} 分钟超过当前 PD 阈值 ${m.limit} 分钟，需同步调大 max-store-down-time 至 ${newEnd + 15}m`]); else checks.push(['ok', '新窗口仍在 PD 阈值之内，无需调整 PD 配置']);
  const cmds = needLimit ? [`${PDCTL()} config set max-store-down-time ${newEnd + 15}m`] : ['# 仅延长维护窗口，不需要执行集群命令'];
  planDrawer({title: `延长维护窗口 +${add} 分钟（${m.ip}）`, risk: 'low', cmds, checks, targets: [], stay: true, steps: [{name: '更新维护窗口' + (needLimit ? ' 并调大 PD 阈值' : ''), ms: 600, end: () => {
    m.extendedBy += add; if (needLimit) { m.limit = newEnd + 15; if (m.strategy !== 'extend') m.strategy = 'extend'; clusterCfg('pd')['schedule.max-store-down-time'] = `${m.limit}m`; } mlog(m, `窗口延长 ${add} 分钟（现计划 ${m.mins + m.extendedBy} 分钟）`); mtRefresh();
  }}]});
}

/* ---------- 计时器（1 秒 = 1 分钟） ---------- */
setInterval(() => {
  if (OP.on) { opMaintTick(); return; }
  MAINTS.filter(m => m.state === 'active').forEach(m => {
    m.elapsed++;
    const plan = m.mins + m.extendedBy, over = m.elapsed > plan;
    const q = s => document.querySelector(`[data-mt="${m.id}"] ${s}`);
    const el = q('[data-el]'); if (el) el.textContent = `${m.elapsed} 分钟`;
    const bar = q('.pbar b'); if (bar) bar.style.width = Math.min(100, Math.round(m.elapsed / plan * 100)) + '%';
    const lim = q('[data-lim]'); if (lim) lim.textContent = m.hasTikv ? (m.alerted ? '已超过阈值，PD 正在补副本' : `距 PD 补副本阈值还剩 ${Math.max(0, m.limit - m.elapsed)} 分钟`) : '—';
    const card = document.querySelector(`[data-mt="${m.id}"]`); if (card) card.classList.toggle('over', over || m.alerted);
    if (m.hasTikv && !m.alerted && m.elapsed >= m.limit) {
      m.alerted = true; mlog(m, `已超过 max-store-down-time（${m.limit}m）：PD 判定 TiKV Store Down，开始补副本`);
      m.nodes.map(nodeBy).filter(n => n && n.type === 'tikv').forEach(n => { n.note = 'PD 已判定 Store Down，正在补副本'; });
      toast(`⚠ ${m.ip} 维护超过 ${m.limit} 分钟，PD 已开始补副本`); mtRefresh();
    }
  });
}, 1000);

/* ---------- 页面 ---------- */
pages.nodes.maintLive = function () {
  $$('[data-mt-task]').forEach(el => {
    const t = TASKS.find(x => x.id === el.dataset.mtTask); if (!t) return;
    const done = t.steps.filter(s => s.state === 'done').length, cur = t.steps[t.cur], pct = Math.round((done + (cur && t.state === 'running' ? cur.pct : 0)) / t.steps.length * 100);
    el.querySelector('b').style.width = pct + '%'; const lb = el.parentElement.querySelector('[data-mt-step]'); if (lb) lb.textContent = cur && t.state === 'running' ? `${cur.name}（${pct}%）` : '收尾中…';
  });
};
pages.nodes.maint = function () {
  if (OP.on) return opMaintHtml();
  const ch = clusterHosts(), S = mtState, T = MAINT_TYPES[S.type];
  const cards = MAINTS.map(m => {
    const [tc, tt] = MT_STATE_TAG[m.state], plan = m.mins + m.extendedBy, over = m.elapsed > plan || m.alerted;
    const ns = m.nodes.map(nodeBy).filter(Boolean);
    let body = '';
    if (m.state === 'draining' || m.state === 'recovering') body = `<div class="small muted" data-mt-step>${m.state === 'draining' ? '正在排空并停机…' : '正在恢复…'}</div><div data-mt-task="${m.taskId || ''}" class="pbar" style="margin:6px 0"><b style="width:5%"></b></div>`
      + (m.state === 'draining' && m.taskId ? `<div class="acts"><button class="btn sm danger" data-na="mt-abort" data-id="${m.id}">中止并恢复</button><button class="btn sm" data-na="tab" data-t="tasks">任务中心 →</button></div>` : `<button class="btn sm" data-na="tab" data-t="tasks">任务中心 →</button>`);
    else if (m.state === 'active') body = `<div class="row" style="gap:24px"><div><div class="muted small">已维护</div><div class="big" data-el>${m.elapsed} 分钟</div></div><div><div class="muted small">计划窗口</div><div class="big">${plan} 分钟</div></div>
        <div style="flex:1;min-width:200px"><div class="pbar" style="margin-top:8px"><b style="width:${Math.min(100, Math.round(m.elapsed / plan * 100))}%;${over ? 'background:var(--bad)' : ''}"></b></div><div class="small muted" style="margin-top:4px" data-lim>${m.hasTikv ? (m.alerted ? '已超过阈值，PD 正在补副本' : `距 PD 补副本阈值还剩 ${Math.max(0, m.limit - m.elapsed)} 分钟`) : '—'}</div></div></div>
      <div class="grid g2" style="margin-top:10px"><div><b class="small">维护检查单</b><div class="checklist">${m.todo.map((x, i) => `<label><input type="checkbox" data-nc="mt-todo" data-id="${m.id}" data-i="${i}" ${x.done ? 'checked' : ''}> ${x.t}</label>`).join('')}</div></div>
        <div><b class="small">主机实例</b><div style="margin-top:4px">${ns.map(n => `<div class="small">${stDot(n.status)} ${esc(n.id)} <span class="muted">${stText[n.status]}${n.note ? ' · ' + esc(n.note) : ''}</span></div>`).join('')}</div></div></div>
      <div class="acts" style="margin-top:10px"><button class="btn primary sm" data-na="mt-end" data-id="${m.id}">结束维护（恢复）</button><button class="btn sm" data-na="mt-ext" data-id="${m.id}" data-add="15">延长 15 分钟</button><button class="btn sm" data-na="mt-ext" data-id="${m.id}" data-add="30">延长 30 分钟</button><button class="btn sm danger" data-na="mt-urgent" data-id="${m.id}">紧急恢复</button></div>`;
    else body = `<div class="small">${m.state === 'done' ? `计划 ${m.mins + m.extendedBy} 分钟 · 实际 ${m.actual} 分钟${m.alerted ? ' · <span style="color:var(--bad)">超过 PD 阈值，触发过补副本</span>' : ''}` : '计划已取消'}</div>`;
    return `<div class="mcard ${m.state === 'done' ? 'done' : ''} ${over && isLive(m) ? 'over' : ''}" data-mt="${m.id}"><div class="row"><b>${esc(m.ip)}</b>${tag(tt, tc)}<span>${esc(m.typeLabel)}</span><span class="small muted">${esc(m.id)} · ${m.started}${m.ticket ? ' · 工单 ' + esc(m.ticket) : ''}</span>
      ${m.hasTikv ? tag(m.strategy === 'extend' ? `阈值已调至 ${m.limit}m` : `阈值 ${m.limit}m`, m.strategy === 'extend' ? 'info' : '') : ''}</div>
      <div class="small muted" style="margin:4px 0 8px">原因：${esc(m.reason || '—')}</div>${body}
      <details class="fold" style="margin-top:10px"><summary><span class="small">维护日志（${m.log.length}）</span></summary><div class="body"><pre class="code log" style="margin:0">${esc(m.log.join('\n'))}</pre></div></details></div>`;
  }).join('');
  let wiz = stepper(['选择主机与类型', '评估与策略', '确认并启动'], S.step);
  if (S.step === 1) {
    wiz += `<div class="grid g2"><div class="card"><h2>维护主机</h2>${ch.map(h => { const live = inMaint(h.ip); return `<label class="radio-card ${S.ip === h.ip ? 'sel' : ''}" style="${live ? 'opacity:.5' : ''}"><input type="radio" name="mtHost" data-nc="mt-host" value="${esc(h.ip)}" ${S.ip === h.ip ? 'checked' : ''} ${live ? 'disabled' : ''}><div><b class="mono">${esc(h.ip)}</b> <span class="small muted">${esc(h.az)} / ${esc(h.rack)}</span>${live ? ' ' + tag('维护中', 'warn') : ''}<div class="small">${h.insts.map(i => `<span class="tag">${esc(i)}</span>`).join(' ')}</div></div></label>`; }).join('')}</div>
      <div class="card"><h2>维护信息</h2><div class="field"><label>维护类型</label><div class="chips">${Object.entries(MAINT_TYPES).map(([k, t]) => `<span class="chip ${S.type === k ? 'active' : ''}" data-na="mt-type" data-k="${k}">${t.label}</span>`).join('')}</div></div>
        <div class="field"><label>预计停机时长（分钟）</label><input type="text" data-ni="mt-mins" value="${S.mins}"></div>
        ${S.type === 'disk' ? `<div class="field"><label>数据盘上的数据是否保留？</label><label class="small"><input type="radio" name="mtKeep" data-nc="mt-keep" value="1" ${S.keepData ? 'checked' : ''}> 保留（扩容 / 重新挂载）</label><label class="small"><input type="radio" name="mtKeep" data-nc="mt-keep" value="0" ${!S.keepData ? 'checked' : ''}> 不保留（换盘后数据丢失）</label></div>` : ''}
        <div class="field"><label>维护原因</label><textarea data-ni="mt-reason" rows="2" placeholder="例如：升级内核修复 CVE-xxxx">${esc(S.reason)}</textarea></div>
        <div class="field"><label>变更工单号（可选）</label><input type="text" data-ni="mt-ticket" value="${esc(S.ticket)}" placeholder="CHG-12345"></div></div></div>`;
  } else if (S.step === 2) {
    const ev = evalMaint(S.ip, +S.mins, S.keepData, S.type), eff = effStrategy(ev.hasTikv, +S.mins), thr = ev.thr;
    wiz += `<div class="grid g-main"><div class="card"><h2>评估结果 <span class="sub">${S.ip} · ${T.label} · ${S.mins} 分钟</span></h2>
      ${ev.items.map(i => `<div class="notice ${i.lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[i.lv]}</span><div>${i.t}</div></div>`).join('')}
      ${!S.keepData ? `<div class="row" style="margin-top:8px"><button class="btn" data-na="nd-goto" data-t="scalein">去缩容下线 →</button><button class="btn" data-na="nd-goto" data-t="scaleout">去扩容新增 →</button></div>` : ''}</div>
      <div class="card"><h2>PD 补副本阈值策略</h2>${ev.hasTikv ? `<div class="small muted" style="margin-bottom:8px">主机上有 TiKV：失联超过 max-store-down-time（当前 ${thr}m）PD 就会判定 Down 并开始补副本。</div>
        ${[['auto', `自动（推荐：${ev.rec === 'extend' ? '延长阈值' : '标准'}）`, ''], ['standard', '标准：保持阈值不变', `适合 ≤ ${thr - 5} 分钟的短维护；超时会补副本`], ['extend', `延长：临时调到 ${Math.max(thr, +S.mins + 30)}m`, '适合长维护，结束后自动恢复']].map(([k, a, b]) => `<label class="radio-card ${S.strategy === k ? 'sel' : ''}"><input type="radio" name="mtStrat" data-nc="mt-strat" value="${k}" ${S.strategy === k ? 'checked' : ''}><div><b class="small">${a}</b><div class="small muted">${b}</div></div></label>`).join('')}
        <div class="small">将采用：<b>${eff === 'extend' ? '延长阈值' : '标准'}</b></div>` : '<div class="muted small">主机上没有 TiKV，不涉及补副本阈值。</div>'}</div></div>`;
  } else {
    const ev = evalMaint(S.ip, +S.mins, S.keepData, S.type), eff = effStrategy(ev.hasTikv, +S.mins), limit = eff === 'extend' ? Math.max(ev.thr, +S.mins + 30) : ev.thr;
    const fake = {ip: S.ip, nodes: ev.ns.map(n => n.id), strategy: eff, limit, origThresh: ev.thr}, cmds = enterCmds(fake), ns = sortBy(ev.ns, ORDER_STOP);
    wiz += `<div class="grid g2"><div class="card"><h2>执行计划</h2><ol class="small">${eff === 'extend' ? `<li>临时调大 max-store-down-time 至 ${limit}m</li>` : ''}${ns.some(n => n.type === 'tidb') ? '<li>从负载均衡摘除 TiDB 并排空连接</li>' : ''}${ns.some(n => n.type === 'pd' && n.leader) ? '<li>迁移 PD Leader</li>' : ''}${ns.some(n => n.type === 'ticdc') ? '<li>迁移 changefeed capture</li>' : ''}${ns.some(n => n.type === 'tikv') ? '<li>驱逐 TiKV Leader</li>' : ''}<li>按 ${ns.map(n => n.id).join(' → ')} 顺序停止实例</li><li>进入维护窗口：倒计时 ${S.mins} 分钟，勾选检查单</li><li>结束维护：预检 → 启动 → 健康检查 → Leader 回迁 → 恢复阈值 → 验证</li></ol>
      <h2 style="margin-top:12px">命令</h2>${code(cmds.join('\n'))}</div>
      <div class="card"><h2>确认</h2><div class="kv"><span>主机</span><span>${S.ip}</span><span>类型</span><span>${T.label}</span><span>计划时长</span><span>${S.mins} 分钟</span><span>原因</span><span>${esc(S.reason)}</span></div>
        <div class="field" style="margin-top:12px"><label>请输入「确认维护」</label><input type="text" data-ni="mt-confirm" value="${esc(S.confirm)}" placeholder="确认维护"></div></div></div>`;
  }
  const ev2 = S.step >= 2 && S.ip ? evalMaint(S.ip, +S.mins, S.keepData, S.type) : null;
  const next = S.step === 1 ? !!S.ip && +S.mins > 0 && !!S.reason.trim() : S.step === 2 ? !ev2.blocked : S.confirm === '确认维护';
  wiz += `<div class="sticky-actions">${S.step > 1 ? '<button class="btn" data-na="mt-prev">← 上一步</button>' : ''}${S.step < 3 ? `<button class="btn primary" data-na="mt-next" ${next ? '' : 'disabled'}>下一步 →</button>` : `<button class="btn danger" id="mtGo" data-na="mt-submit" ${next ? '' : 'disabled'}>开始维护（排空并停机）</button><button class="btn" data-na="sched-from" data-src="maint">预约到低峰窗口…</button>`}${S.step === 1 && !next ? '<span class="small muted">请选择主机并填写预计时长与原因</span>' : ''}</div>`;
  return `${cards ? `<h3 style="font-size:15px;margin:0 0 10px">维护计划与记录 <span class="small muted">演示加速：1 秒 = 1 分钟</span></h3>${cards}` : ''}<div class="card" style="margin-top:${cards ? 8 : 0}px"><h2>新建主机维护计划 <span class="sub">完整流程：评估 → 排空停机 → 维护 → 恢复验证</span></h2>${wiz}</div>`;
};

/* ---------- 运维模式：主机维护（渲染服务端 OP.maints） ---------- */
const mtElapsed = m => m.activeAt ? Math.max(0, Math.floor((opNow() - new Date(m.activeAt).getTime()) / 60000)) : 0;
const mtChange = id => id && OP.changes.find(c => c.id === id);
const mtPendingChange = m => OP.changes.find(c => ['pending', 'approved', 'scheduled', 'running'].includes(c.state) && ['maint.exit', 'maint.extend'].includes(c.kind) && c.params && c.params.maintId === m.id);
const mtProg = c => {
  if (!c) return {pct: 5, txt: '等待服务端变更…'};
  const st = c.steps || [], done = st.filter(x => x.state === 'done' || x.state === 'skipped').length, cur = st.find(x => x.state === 'run' || x.state === 'manual');
  const pct = c.state === 'pending' ? 0 : Math.round(done / Math.max(1, st.length) * 100);
  return {pct, txt: c.state === 'pending' ? `变更 ${c.id} 等待审批` : cur ? `${cur.name}${cur.state === 'manual' ? '（等待人工确认）' : ''}（${pct}%）` : OP_STATE[c.state] ? OP_STATE[c.state][1] : c.state};
};

function mtLimitText(m, el) { return m.hasTikv ? (m.alerted || el >= m.limit ? '已超过 PD 阈值，PD 正在补副本' : `距 PD 补副本阈值还剩 ${Math.max(0, m.limit - el)} 分钟（阈值 ${m.limit}m）`) : '—'; }

function mtLiveCard(m) {
  const [tc, tt] = MT_STATE_TAG[m.state] || ['', m.state], plan = m.mins + m.extendedBy, el = mtElapsed(m), alerted = m.alerted || (m.hasTikv && m.state === 'active' && el >= m.limit), over = m.state === 'active' && (el > plan || alerted);
  const ns = m.nodes.map(nodeBy).filter(Boolean), pc = mtPendingChange(m), can = opCan('maint'), dis = can ? '' : ' disabled title="当前账号没有「主机维护」权限（需 DBA 角色）"', endDis = !can ? OP_DENY_ATTR : pc ? ' disabled' : '';
  let body = '';
  if (m.state === 'draining' || m.state === 'recovering') {
    const c = mtChange(m.state === 'draining' ? m.enterId : m.exitId), pg = mtProg(c);
    body = `<div class="small muted">${esc(pg.txt)}</div><div class="pbar" style="margin:6px 0"><b style="width:${pg.pct}%"></b></div>
      <div class="acts">${c && c.state === 'running' && c.plan && c.plan.abortable && (c.applicant.toLowerCase() === ocMe() || opHasRole('dba')) ? ocBtn('oc-abort', c.id, '中止并恢复', 'danger') : ''}${c ? ocBtn('oc-goto', c.id, `变更 ${c.id} →`) : ''}</div>`;
  } else if (m.state === 'active') {
    body = `${alerted ? `<div class="notice bad"><span>✖</span><div><b>已超过 PD 阈值 ${m.limit} 分钟</b>：PD 已判定 TiKV Store Down 并开始补副本。请尽快结束维护；恢复后会有额外的 Region 均衡。</div></div>` : m.hasTikv && m.limit - el <= 5 ? `<div class="notice warn"><span>⚠</span><div>距 PD 补副本阈值仅剩 ${Math.max(0, m.limit - el)} 分钟，请尽快结束维护或延长窗口（延长会同步调大阈值）。</div></div>` : ''}
      <div class="row" style="gap:24px"><div><div class="muted small">已维护</div><div class="big" data-el>${el} 分钟</div></div><div><div class="muted small">计划窗口</div><div class="big">${plan} 分钟</div></div>
        <div style="flex:1;min-width:200px"><div class="pbar" style="margin-top:8px"><b style="width:${Math.min(100, Math.round(el / plan * 100))}%;${over ? 'background:var(--bad)' : ''}"></b></div><div class="small muted" style="margin-top:4px" data-lim>${mtLimitText(m, el)}</div></div></div>
      ${pc ? `<div class="notice info" style="margin-top:8px"><span>ℹ</span><div>变更 <a data-na="oc-goto" data-id="${pc.id}">${pc.id}</a>（${esc(pc.title)}）${(OP_STATE[pc.state] || [0, pc.state])[1]}，期间不能再提交结束 / 延长。</div></div>` : ''}
      <div class="grid g2" style="margin-top:10px"><div><b class="small">维护检查单</b><div class="checklist">${m.todo.map((x, i) => `<label><input type="checkbox" data-nc="mt-todo-op" data-id="${m.id}" data-i="${i}" ${x.done ? 'checked' : ''}${dis}> ${esc(x.t)}</label>`).join('')}</div></div>
        <div><b class="small">主机实例</b><div style="margin-top:4px">${ns.map(n => `<div class="small">${stDot(n.status)} ${esc(n.id)} <span class="muted">${stText[n.status]}${n.note ? ' · ' + esc(n.note) : ''}</span></div>`).join('')}</div></div></div>
      <div class="acts" style="margin-top:10px"><button class="btn primary sm" data-na="mt-end" data-id="${m.id}"${endDis}>结束维护（恢复）</button>${[15, 30, 60].map(a => `<button class="btn sm" data-na="mt-ext" data-id="${m.id}" data-add="${a}"${endDis}>延长 ${a} 分钟</button>`).join('')}<button class="btn sm danger" data-na="mt-urgent" data-id="${m.id}"${endDis}>紧急恢复</button></div>`;
  }
  return `<div class="mcard ${over ? 'over' : ''}" data-mt="${m.id}" data-mt-op="1"><div class="row"><b>${esc(m.ip)}</b>${tag(tt, tc)}<span>${esc(m.typeLabel)}</span><span class="small muted">${esc(m.id)} · ${opFmtTime(m.started)}${m.ticket ? ' · 工单 ' + esc(m.ticket) : ''}</span>
    ${m.hasTikv ? tag(m.strategy === 'extend' ? `阈值已调至 ${m.limit}m` : `阈值 ${m.limit}m`, m.strategy === 'extend' ? 'info' : '') : ''}</div>
    <div class="small muted" style="margin:4px 0 8px">原因：${esc(m.reason || '—')}</div>${body}
    <details class="fold" data-tid="${m.id}:log" style="margin-top:10px" ${openCmds.has(m.id + ':log') ? 'open' : ''}><summary><span class="small">维护日志（${(m.log || []).length}）</span></summary><div class="body"><pre class="code log" data-lg="${m.id}" style="margin:0">${esc((m.log || []).join('\n'))}</pre></div></details></div>`;
}

function opMaintHtml() {
  if (!OP.loaded) return '<div class="card"><div class="empty">正在加载服务端维护记录…</div></div>';
  const S = mtState, types = mtTypes(), ch = clusterHosts(), live = OP.maints.filter(isLive), hist = OP.maints.filter(m => !isLive(m));
  const hasTikv = S.ip && hostInsts(S.ip).some(n => n.type === 'tikv'), denied = !opCan('maint');
  const ready = !!S.ip && +S.mins > 0 && !!S.reason.trim() && !denied;
  const form = `<div class="grid g2"><div class="card"><h2>维护主机</h2>${ch.map(h => { const lv = inMaint(h.ip), bc = h.insts.map(busyBy).find(Boolean); return `<label class="radio-card ${S.ip === h.ip ? 'sel' : ''}" style="${lv ? 'opacity:.5' : ''}"><input type="radio" name="mtHost" data-nc="mt-host" value="${esc(h.ip)}" ${S.ip === h.ip ? 'checked' : ''} ${lv ? 'disabled' : ''}><div><b class="mono">${esc(h.ip)}</b> <span class="small muted">${esc(h.az)} / ${esc(h.rack)}</span>${lv ? ' ' + tag('维护中', 'warn') : ''}${bc ? ' ' + tag('有变更 ' + bc.id, 'info') : ''}<div class="small">${h.insts.map(i => `<span class="tag">${esc(i)}</span>`).join(' ')}</div></div></label>`; }).join('')}</div>
    <div class="card"><h2>维护信息</h2><div class="field"><label>维护类型</label><div class="chips">${Object.entries(types).map(([k, t]) => `<span class="chip ${S.type === k ? 'active' : ''}" data-na="mt-type" data-k="${k}">${esc(t.label)}</span>`).join('')}</div></div>
      <div class="field"><label>预计停机时长（分钟）</label><input type="text" data-ni="mt-mins" value="${esc(S.mins)}"></div>
      ${S.type === 'disk' ? `<div class="field"><label>数据盘上的数据是否保留？</label><label class="small"><input type="radio" name="mtKeep" data-nc="mt-keep" value="1" ${S.keepData ? 'checked' : ''}> 保留（扩容 / 重新挂载）</label><label class="small"><input type="radio" name="mtKeep" data-nc="mt-keep" value="0" ${!S.keepData ? 'checked' : ''}> 不保留（换盘后数据丢失，服务端会阻断）</label></div>` : ''}
      ${hasTikv ? `<div class="field"><label>PD 补副本阈值策略（主机上有 TiKV）</label><select data-nc="mt-strat-op">${[['auto', '自动（推荐，由服务端按时长决定）'], ['standard', '标准：保持 max-store-down-time 不变'], ['extend', '延长：临时调大阈值，结束后恢复']].map(([k, t]) => `<option value="${k}" ${S.strategy === k ? 'selected' : ''}>${t}</option>`).join('')}</select></div>` : ''}
      <div class="field"><label>维护原因</label><textarea data-ni="mt-reason" rows="2" placeholder="例如：升级内核修复 CVE-xxxx">${esc(S.reason)}</textarea></div>
      <div class="field"><label>变更工单号（可选）</label><input type="text" data-ni="mt-ticket" value="${esc(S.ticket)}" placeholder="CHG-12345"></div>
      <div class="small muted">评估、风险检查、审批级数均由服务端给出；提交前会先展示规划结果。</div></div></div>
    <div class="sticky-actions"><button class="btn danger" id="mtGo" data-na="mt-submit" ${denied ? OP_DENY_ATTR : ready ? '' : 'disabled'}>评估并提交维护申请…</button>${!ready && !denied ? '<span class="small muted">请选择主机并填写预计时长与原因</span>' : ''}</div>`;
  const histHtml = hist.length ? `<div class="card" style="margin-top:16px"><h2>历史维护 <span class="sub">${hist.length} 条</span></h2><div class="tbl-wrap"><table><thead><tr><th>编号</th><th>主机</th><th>类型</th><th>状态</th><th>开始</th><th>结束</th><th class="num">计划 / 实际（分钟）</th><th></th></tr></thead><tbody>
    ${hist.map(m => { const [tc, tt] = MT_STATE_TAG[m.state] || ['', m.state], act = m.activeAt && m.ended ? Math.round((new Date(m.ended) - new Date(m.activeAt)) / 60000) : '—'; return `<tr><td class="mono small">${m.id}</td><td class="mono">${m.ip}</td><td>${esc(m.typeLabel)}</td><td>${tag(tt, tc)}${m.alerted ? ' ' + tag('触发过补副本', 'bad') : ''}</td><td class="small">${opFmtTime(m.started)}</td><td class="small">${opFmtTime(m.ended)}</td><td class="num">${m.mins + m.extendedBy} / ${act}</td><td>${ocBtn('oc-goto', m.exitId || m.enterId, '变更')}</td></tr>`; }).join('')}</tbody></table></div></div>` : '';
  return `${live.length ? `<h3 style="font-size:15px;margin:0 0 10px">进行中的维护</h3>${live.map(mtLiveCard).join('')}` : ''}
    <div class="card" style="margin-top:${live.length ? 8 : 0}px"><h2>新建主机维护计划 <span class="sub">服务端评估 → 审批 → 排空停机 → 维护 → 恢复验证</span></h2>${form}</div>${histHtml}`;
}

/* 每秒刷新进行中维护的倒计时（时间取服务端 activeAt 与校准后的 opNow） */
function opMaintTick() {
  OP.maints.filter(m => m.state === 'active').forEach(m => {
    const card = document.querySelector(`[data-mt="${m.id}"][data-mt-op]`); if (!card) return;
    const plan = m.mins + m.extendedBy, el = mtElapsed(m), alerted = m.alerted || (m.hasTikv && el >= m.limit);
    const e = card.querySelector('[data-el]'); if (e) e.textContent = `${el} 分钟`;
    const bar = card.querySelector('.pbar b'); if (bar) { bar.style.width = Math.min(100, Math.round(el / plan * 100)) + '%'; bar.style.background = el > plan || alerted ? 'var(--bad)' : ''; }
    const lim = card.querySelector('[data-lim]'); if (lim) lim.textContent = mtLimitText(m, el);
    card.classList.toggle('over', el > plan || alerted);
  });
}

/* ---------- 事件 ---------- */
document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
  const d = el.dataset, S = mtState;
  switch (d.na) {
    case 'mt-type': S.type = d.k; S.mins = mtTypes()[d.k].def; if (d.k !== 'disk') S.keepData = true; pages.nodes.body(); break;
    case 'mt-next': S.step = Math.min(3, S.step + 1); pages.nodes.body(); break;
    case 'mt-prev': S.step = Math.max(1, S.step - 1); pages.nodes.body(); break;
    case 'mt-submit': mtSubmit(); break;
    case 'mt-end': mtEnd(d.id, false); break;
    case 'mt-urgent': mtEnd(d.id, true); break;
    case 'mt-ext': mtExtend(d.id, +d.add); break;
    case 'mt-abort': if (OP.on) break; { const m = MAINTS.find(x => x.id === d.id), t = TASKS.find(x => x.id === (m && m.taskId)); if (t) abortTask(t); break; }
  }
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc]'); if (!el) return; const S = mtState;
  switch (el.dataset.nc) {
    case 'mt-host': S.ip = el.value; pages.nodes.body(); break;
    case 'mt-keep': S.keepData = el.value === '1'; break;
    case 'mt-strat': S.strategy = el.value; pages.nodes.body(); break;
    case 'mt-strat-op': S.strategy = el.value; break;
    case 'mt-todo-op': {   // 运维模式：检查单写回服务端
      const m = OP.maints.find(x => x.id === el.dataset.id), i = +el.dataset.i; if (!m) break;
      el.disabled = true;
      opApi('POST', `/api/maints/${encodeURIComponent(m.id)}/todo`, {idx: i, done: el.checked}).then(r => { if (r.code !== 200) toast(r.data.error || '保存检查单失败'); opRefresh(true); });
      break;
    }
    case 'mt-todo': { const m = MAINTS.find(x => x.id === el.dataset.id); if (m) { m.todo[+el.dataset.i].done = el.checked; mlog(m, `检查单：${el.checked ? '完成' : '取消'}「${m.todo[+el.dataset.i].t}」`); } break; }
  }
});
document.addEventListener('input', e => {
  const el = e.target.closest('[data-ni]'); if (!el) return; const S = mtState;
  switch (el.dataset.ni) {
    case 'mt-mins': S.mins = el.value.trim(); break;
    case 'mt-reason': S.reason = el.value; break;
    case 'mt-ticket': S.ticket = el.value.trim(); break;
    case 'mt-confirm': S.confirm = el.value.trim(); { const b = $('#mtGo'); if (b) b.disabled = S.confirm !== '确认维护'; } break;
  }
  if (['mt-mins', 'mt-reason'].includes(el.dataset.ni)) { const b = $('[data-na="mt-next"]'); if (b) b.disabled = !(S.ip && +S.mins > 0 && S.reason.trim()); }
  if (OP.on && ['mt-mins', 'mt-reason'].includes(el.dataset.ni)) { const g = $('#mtGo'); if (g) g.disabled = opCan('maint') && !(S.ip && +S.mins > 0 && S.reason.trim()); }
});
