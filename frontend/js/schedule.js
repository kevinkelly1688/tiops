// @author: kevinkelly
// @license: Apache License 2.0
// @file: schedule.js
// @time: 2026/10/28

/* 变更排期：日历 / 时间线 / 列表，预约、审批、窗口规则、冲突检测、推荐窗口、到点自动执行（演示时钟可快进）。 */
const MIN = 60000, HOUR = 3600000, DAY = 86400000;
let simOffset = 0;
const simNow = () => Date.now() + simOffset;
const sod = t => { const d = new Date(t); d.setHours(0, 0, 0, 0); return d.getTime(); };
const p2 = n => String(n).padStart(2, '0');
const fT = t => { const d = new Date(t); return `${p2(d.getHours())}:${p2(d.getMinutes())}`; };
const fD = t => { const d = new Date(t); return `${d.getMonth() + 1}月${d.getDate()}日`; };
const dateVal = t => { const d = new Date(t); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`; };
const fDT = t => `${dateVal(t)} ${fT(t)}`;
const mmToT = m => `${p2(Math.floor(m / 60))}:${p2(m % 60)}`;
const TODAY0 = sod(Date.now());
const at = (d, h, m) => TODAY0 + d * DAY + h * HOUR + m * MIN;

const SCHED_TYPES = {
  maint: {label: '主机维护', color: '--c3'}, scaleout: {label: '扩容', color: '--c2'}, scalein: {label: '缩容', color: '--c5'},
  config: {label: '配置变更', color: '--c1'}, restart: {label: '滚动重启', color: '--c4'}, release: {label: '平台发布', color: '--c6'},
};
const SCHED_STATUS = {pending: ['info', '待审批'], approved: ['ok', '已批准 · 待执行'], ready: ['warn', '到点 · 待确认'], running: ['info', '执行中'], done: ['ok', '已完成'], failed: ['bad', '执行失败'], rejected: ['bad', '已驳回'], cancelled: ['', '已取消']};
const SCHED_ACTIVE = ['pending', 'approved', 'ready', 'running'];
const RISK_TAG = {low: ['ok', '低风险'], mid: ['warn', '中风险'], high: ['bad', '高风险']};
const RULE = {backup: [120, 160], peaks: [[600, 720], [1140, 1380]], good: [160, 360]};      // 分钟（当天）
const PLAN_W1 = (() => { const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - ((d.getDay() + 6) % 7)); return d.getTime(); })();   // 路线图 W1 = 本周一
const planWeekStart = w => PLAN_W1 + (w - 1) * 7 * DAY;
const FREEZES = [
  {id: 'FZ1', from: TODAY0 + 9 * DAY, to: TODAY0 + 12 * DAY, label: '年度大促保障期 · 变更冻结'},
  {id: 'FZ2', from: planWeekStart(27), to: planWeekStart(29), label: '重大活动保障期 · 变更冻结'},
];
const freezeOver = (a, b) => FREEZES.filter(f => a < f.to && b > f.from);
const SCHED_CAP = {maint: 'D6', scaleout: 'D4', scalein: 'D4', config: 'D5', restart: 'D3'};   // 变更类型 → 依赖的路线图任务
function capInfo(type) {
  const id = SCHED_CAP[type]; if (!id || typeof rmTask !== 'function') return null;
  const t = rmTask(id), m = RM_MILESTONES.find(x => x.id === t.ms);
  return {id, task: t, live: t.status === 'done', ms: m};
}
const relEnv = s => { const m = typeof rmMs === 'function' ? rmMs(s.payload.ms) : null; return m ? m.env : '测试环境'; };
const TRAFFIC = [20, 12, 8, 6, 5, 6, 10, 25, 45, 70, 85, 80, 60, 70, 80, 75, 70, 65, 75, 90, 95, 85, 60, 35];
const SCHED_USERS = {you: '你（申请人）', ops_li: 'ops_li（审批人 · DBA）', dba_chen: 'dba_chen（审批人 · 负责人）'};
const schedState = {view: 'cal', month: sod(Date.now()), sel: TODAY0, user: 'you', fStatus: 'all', fType: 'all', openId: null, strict: false};
let schedSeq = 7;

const SCHEDULES = [
  {id: 'S0001', type: 'maint', title: '主机维护：tikv-5 内核升级', payload: {ip: '10.0.2.15', mtype: 'kernel', mins: 25, reason: '内核升级修复 CVE'}, start: at(-1, 2, 50), dur: 45, owner: 'ops_wang', ticket: 'CHG-20881', auto: true, risk: 'high', needs: 2,
    approvals: [{by: 'ops_li', at: at(-3, 10, 0)}, {by: 'dba_chen', at: at(-3, 11, 5)}], status: 'done', created: at(-4, 9, 0)},
  {id: 'S0002', type: 'scaleout', title: '扩容 TiKV ×1：10.0.2.17（缓解 tikv-4 磁盘 86%）', payload: {role: 'tikv', ips: ['10.0.2.17'], cfg: null}, start: at(1, 3, 0), dur: 90, owner: 'dba_li', ticket: 'CHG-20902', auto: true, risk: 'mid', needs: 1,
    approvals: [{by: 'ops_li', at: at(0, 9, 30)}], status: 'approved', created: at(-1, 17, 0)},
  {id: 'S0003', type: 'maint', title: '主机维护：tikv-4 磁盘检修', payload: {ip: '10.0.2.14', mtype: 'hardware', mins: 60, reason: '磁盘告警，检修 RAID 卡'}, start: at(2, 2, 50), dur: 80, owner: 'ops_zhang', ticket: 'CHG-20915', auto: true, risk: 'high', needs: 2,
    approvals: [{by: 'ops_li', at: at(0, 11, 0)}], status: 'pending', created: at(-1, 18, 30)},
  {id: 'S0004', type: 'config', title: '配置变更：tidb_mem_quota_query → 2GB（tidb-3 OOM 缓解）', payload: {nodeId: 'tidb-3', scope: 'instance', changes: [{k: 'tidb_mem_quota_query', to: '2GB'}]}, start: at(3, 3, 30), dur: 20, owner: 'ops_wang', ticket: 'CHG-20920', auto: true, risk: 'mid', needs: 1,
    approvals: [{by: 'dba_chen', at: at(0, 10, 0)}], status: 'approved', created: at(-1, 15, 0)},
  {id: 'S0005', type: 'restart', title: 'PD 滚动重启（max-store-down-time 调整后生效验证）', payload: {role: 'pd'}, start: at(5, 3, 0), dur: 45, owner: 'ops_zhang', ticket: 'CHG-20933', auto: false, risk: 'mid', needs: 1, approvals: [], status: 'pending', created: at(0, 8, 40)},
  {id: 'S0006', type: 'scalein', title: '缩容 TiKV：tikv-2', payload: {ids: ['tikv-2']}, start: at(1, 20, 30), dur: 180, owner: 'ops_wang', ticket: 'CHG-20899', auto: true, risk: 'high', needs: 2, approvals: [], status: 'rejected', reason: '驳回：处于业务高峰；且 tikv-2 是写热点节点，缩容不能解决问题', created: at(-2, 14, 0)},
];

/* ---------- 资源与风险 ---------- */
function schedRisk(type, p) {
  if (type === 'release') { const m = rmMs(p.ms); return !m || m.env === '测试环境' ? 'low' : m.env === '预发' ? 'mid' : 'high'; }
  if (type === 'scalein') return 'high';
  if (type === 'maint') return hostInsts(p.ip).some(n => n.type === 'tikv') ? 'high' : 'mid';
  if (type === 'config') { const n = nodeBy(p.nodeId); return (p.changes || []).some(c => { const i = n && catalog(n.type).find(x => x.k === c.k); return i && i.danger; }) ? 'high' : 'mid'; }
  return 'mid';
}
const needsOf = (risk, emergency) => emergency ? 2 : risk === 'high' ? 2 : risk === 'mid' ? 1 : 0;
function configHasReload(p) { const n = nodeBy(p.nodeId); return n && (p.changes || []).some(c => { const i = catalog(n.type).find(x => x.k === c.k); return i && !i.online && i.scope !== 'cluster'; }); }
function schedRes(s) {
  const p = s.payload, ids = new Set(), roles = new Set(), hosts = new Set();
  if (s.type === 'maint') { hostInsts(p.ip).forEach(n => { ids.add(n.id); roles.add(n.type); }); hosts.add(p.ip); }
  else if (s.type === 'scaleout') { roles.add(p.role); p.ips.forEach(ip => hosts.add(ip)); }
  else if (s.type === 'scalein') p.ids.forEach(id => { const n = nodeBy(id); ids.add(id); if (n) { roles.add(n.type); hosts.add(n.host); } });
  else if (s.type === 'config') {
    const n = nodeBy(p.nodeId);
    if (n && configHasReload(p)) { roles.add(n.type); (p.scope === 'role' ? NODES.filter(x => x.type === n.type) : [n]).forEach(x => { ids.add(x.id); hosts.add(x.host); }); }
  } else if (s.type === 'restart') { NODES.filter(n => n.type === p.role).forEach(n => { ids.add(n.id); hosts.add(n.host); }); roles.add(p.role); }
  return {ids, roles, hosts};
}
const inter = (a, b) => [...a].some(x => b.has(x));
const hasRes = r => r.ids.size || r.hosts.size;
function payloadSummary(s) {
  const p = s.payload;
  if (s.type === 'maint') return `${p.ip} · ${MAINT_TYPES[p.mtype].label} · 预计停机 ${p.mins} 分钟`;
  if (s.type === 'scaleout') return `${TYPE_META[p.role].label} ×${p.ips.length}：${p.ips.join('、')}`;
  if (s.type === 'scalein') return `下线 ${p.ids.join('、')}`;
  if (s.type === 'config') return `${p.nodeId}（${p.scope === 'role' ? '全部同角色' : '仅此实例'}）：${p.changes.map(c => `${shortKey(c.k)}→${c.to}`).join('、')}`;
  if (s.type === 'release') { const m = rmMs(p.ms); return `${p.ms} ${m ? m.name : ''}（${m ? m.env : ''}）`; }
  return `全部 ${TYPE_META[p.role].label}（${NODES.filter(n => n.type === p.role).length} 个）依次重启`;
}
function estDur(type, p) {
  if (type === 'release') return 60;
  if (type === 'maint') return (+p.mins || 30) + 20;
  if (type === 'scaleout') return 90; if (type === 'scalein') return 180;
  if (type === 'config') { const n = nodeBy(p.nodeId); return configHasReload(p) ? 15 * (p.scope === 'role' && n ? NODES.filter(x => x.type === n.type).length : 1) + 5 : 20; }
  return 15 * NODES.filter(n => n.type === p.role).length;
}

/* ---------- 规则检查 ---------- */
function schedChecks(s, selfId, opt = {}) {
  const items = [], add = (lv, t) => items.push({lv, t});
  if (!s.start || isNaN(s.start)) { add('bad', '请选择开始时间'); return {items, blocked: true}; }
  const end = s.start + s.dur * MIN, now = simNow(), res = schedRes(s);
  if (opt.fresh && s.start < now + 5 * MIN) add('bad', '开始时间已过或距现在不足 5 分钟');
  const relaxed = s.type === 'release' && relEnv(s) !== '生产';
  if (schedState.strict && !opt.fast) { const cap = capInfo(s.type); if (cap && !cap.live) add('bad', `严格模式：${SCHED_TYPES[s.type].label}依赖路线图任务 ${cap.id}，尚未发布（计划 ${cap.ms.id} W${cap.ms.week}）`); }
  const seen = new Set();
  for (let d = sod(s.start); !relaxed && d <= sod(end); d += DAY) {
    const ov = (a, b) => s.start < d + b * MIN && end > d + a * MIN;
    if (ov(...RULE.backup) && !seen.has('b')) { seen.add('b'); add('bad', '与每日全量备份窗口（02:00–02:40）重叠，请错开'); }
    RULE.peaks.forEach(([a, b]) => { if (ov(a, b) && !seen.has('p' + a)) { seen.add('p' + a); add(s.risk === 'high' ? 'bad' : 'warn', `处于业务高峰（${mmToT(a)}–${mmToT(b)}）${s.risk === 'high' ? '：高风险变更不允许在高峰执行' : '，建议改到低峰'}`); } });
  }
  if (s.type !== 'release' || relEnv(s) === '生产') freezeOver(s.start, end).forEach(f => {
    if (s.emergency) add(s.reason ? 'warn' : 'bad', s.reason ? `处于「${f.label}」，按紧急变更处理：需 2 级审批并留痕` : '紧急变更必须填写原因');
    else add('bad', `处于「${f.label}」（${fD(f.from)}–${fD(f.to - DAY)}），非紧急变更不允许`);
  });
  const d0 = sod(s.start); if (s.start >= d0 + RULE.good[0] * MIN && end <= d0 + RULE.good[1] * MIN) add('ok', `位于推荐低峰窗口（${mmToT(RULE.good[0])}–${mmToT(RULE.good[1])}），平均流量约 ${Math.round(TRAFFIC.slice(2, 6).reduce((a, b) => a + b, 0) / 4)}%`);
  SCHEDULES.filter(o => o.id !== selfId && SCHED_ACTIVE.includes(o.status) && s.start < o.start + o.dur * MIN && end > o.start).forEach(o => {
    const r2 = schedRes(o), tag2 = `${o.id}「${o.title.slice(0, 18)}」`;
    if (inter(res.ids, r2.ids) || inter(res.hosts, r2.hosts)) add('bad', `与 ${tag2} 时间重叠且涉及同一实例 / 主机`);
    else if (res.roles.has('tikv') && r2.roles.has('tikv') && hasRes(res) && hasRes(r2)) add('bad', `与 ${tag2} 同时段都涉及 TiKV：同一时刻只允许一个 TiKV 相关变更`);
    else if (res.roles.has('pd') && r2.roles.has('pd') && hasRes(res) && hasRes(r2)) add('bad', `与 ${tag2} 同时段都涉及 PD，可能同时失去多数派`);
    else add('warn', `同时段还有 ${tag2}，请确认互不影响`);
  });
  if (!opt.fast) {
    const p = s.payload, cap = capInfo(s.type);
    if (cap) add(cap.live ? 'ok' : 'info', `能力状态：对应路线图任务 ${cap.id}（W${cap.task.start}–W${rmEnd(cap.task)}）${cap.live ? '已上线' : `尚在原型阶段，计划 ${cap.ms.id}（W${cap.ms.week}）发布后用于生产`}`);
    if (s.type === 'release') { const m = rmMs(p.ms); if (!m) add('bad', '里程碑不存在'); else if (rmState.released[m.id]) add('bad', `${m.id} 已发布`); else add('info', `发布范围：${m.accept.join('；')}`); }
    if (s.type === 'maint') { if (!hostInsts(p.ip).length) add('bad', `主机 ${p.ip} 不在集群中`); else { const ev = evalMaint(p.ip, +p.mins, true, p.mtype), b = ev.items.find(i => i.lv === 'bad'); if (b) add('warn', `按当前状态评估：${b.t}（执行时会再次评估）`); } }
    if (s.type === 'scaleout') { if (!p.ips.length) add('bad', '请至少选择 1 台目标主机'); p.ips.forEach(ip => { const h = hostByIp(ip); if (!h) add('bad', `${ip} 不在主机池中`); else if (hostBlocked(h)) add('warn', `${ip} 尚未通过预检，执行前需通过`); }); }
    if (s.type === 'scalein') { if (!p.ids.length) add('bad', '请至少选择 1 个下线节点'); else { const ev = evalScaleIn(p.ids), b = ev.items.find(i => i.lv === 'bad'); if (b) add('warn', `按当前状态评估：${b.t}（执行时会再次评估）`); } }
    if (s.type === 'config') { const n = nodeBy(p.nodeId); if (!n) add('bad', '节点不存在'); else p.changes.forEach(c => { const i = catalog(n.type).find(x => x.k === c.k); if (!i) add('bad', `未知配置项 ${c.k}`); else { const v = validateCfg(n, i, c.to); if (v.err) add('bad', `${i.k}：${v.err}`); else if (v.warn) add('warn', `${i.k}：${v.warn}`); } }); }
    const need = needsOf(s.risk, s.emergency); add('info', `${RISK_TAG[s.risk][1]}：${need ? `需要 ${need} 级审批（申请人不能审批自己的变更）` : '无需审批，提交即生效'}`);
  }
  return {items, blocked: items.some(i => i.lv === 'bad')};
}
/* 推荐窗口：未来 14 天里，规则全通过且流量最低的时段 */
function recommendSlots(s) {
  const base = sod(simNow()), out = [];
  for (let d = 0; d < 14; d++) {
    let best = null;
    for (let m = 0; m < 1440; m += 30) {
      const start = base + d * DAY + m * MIN; if (start < simNow() + 30 * MIN) continue;
      const c = schedChecks({...s, start}, s.id, {fresh: true, fast: true}); if (c.blocked) continue;
      const hrs = []; for (let t = start; t < start + s.dur * MIN; t += HOUR) hrs.push(TRAFFIC[new Date(t).getHours()]);
      const score = hrs.reduce((a, b) => a + b, 0) / hrs.length + c.items.filter(i => i.lv === 'warn').length * 30 + d * 0.8;
      if (!best || score < best.score) best = {start, score, warns: c.items.filter(i => i.lv === 'warn').length};
    }
    if (best) out.push(best);
  }
  return out.sort((a, b) => a.score - b.score).slice(0, 3);
}

/* ---------- 执行 ---------- */
function runSched(s) {
  const p = s.payload, fail = reason => ({ok: false, reason});
  const oldScope = ndState.scope;
  try {
    if (s.type === 'maint') {
      const ev = evalMaint(p.ip, +p.mins, true, p.mtype); if (ev.blocked) return fail(ev.items.find(i => i.lv === 'bad').t);
      Object.assign(mtState, {ip: p.ip, type: p.mtype, mins: +p.mins, reason: p.reason || s.title, ticket: s.ticket || '', keepData: true, strategy: 'auto'}); mtSubmit(true);
      return {ok: true, taskId: MAINTS[0].taskId};
    }
    if (s.type === 'scaleout') {
      const hosts = poolAll().filter(h => p.ips.includes(h.ip)); if (!hosts.length) return fail('目标主机已不在主机池');
      const bad = hosts.find(hostBlocked); if (bad) return fail(`${bad.ip} 未通过预检`);
      const cfg = p.cfg || {port: DEFAULT_PORTS[p.role], cache: Math.round(Math.min(...hosts.map(h => h.mem)) * .45), rate: 15, lb: 'haproxy'};
      return {ok: true, taskId: startTask(buildScaleOut(p.role, hosts, cfg)).id};
    }
    if (s.type === 'scalein') {
      const ev = evalScaleIn(p.ids); if (ev.blocked) return fail(ev.items.find(i => i.lv === 'bad').t);
      return {ok: true, taskId: startTask(buildScaleIn(ev.picks)).id};
    }
    if (s.type === 'config') {
      const n = nodeBy(p.nodeId); if (!n) return fail('节点已不存在');
      ndState.scope = p.scope; const changes = [];
      for (const c of p.changes) { const i = catalog(n.type).find(x => x.k === c.k), v = validateCfg(n, i, c.to); if (v.err) return fail(`${i.k}：${v.err}`); changes.push({i, from: getVal(n, i), to: v.v}); }
      const bad = cfgChecks(n, changes, p.scope).find(x => x[0] === 'bad'); if (bad) return fail(bad[1]);
      return {ok: true, taskId: startTask(buildCfgChange(n, changes, p.scope)).id};
    }
    if (s.type === 'release') {
      const m = rmMs(p.ms); if (!m) return fail('里程碑不存在'); if (rmState.released[m.id]) return fail(`${m.id} 已发布`);
      const t = startTask({title: `平台发布 ${m.id} ${m.name}（${m.env}）`, cmds: [`git tag release-${m.id}`, `./deploy.sh --env ${m.env} --canary 10,50,100`, './smoke-test.sh --suite regression'], targets: [], abortable: false, steps: [
        {name: '构建并运行回归测试', ms: 1400}, {name: '数据库迁移与配置检查', ms: 900}, {name: `灰度发布到${m.env}（10% → 50% → 100%）`, ms: 2100, ticks: 3},
        {name: '冒烟验证与回滚预案确认', ms: 1000, end: () => rmRelease(m.id, s.id), log: `${m.id} 已发布`}]});
      return {ok: true, taskId: t.id};
    }
    const nodes = NODES.filter(n => n.type === p.role && n.status !== 'down'); if (nodes.some(n => busy(n.id))) return fail('存在进行中的任务');
    if (nodes.length < 1) return fail('没有可重启的实例');
    const plans = nodes.map(n => instPlan(n, 'restart'));
    return {ok: true, taskId: startTask({title: `滚动重启 ${TYPE_META[p.role].label}（排期 ${s.id}）`, cmds: plans.flatMap(x => x.cmds), steps: plans.flatMap(x => x.steps.map(st => ({...st, name: `[${x.title.replace('重启 ', '')}] ${st.name}`}))), targets: nodes.map(n => n.id), abortable: false}).id};
  } catch (e) { console.error(e); return fail('执行异常：' + e.message); } finally { ndState.scope = oldScope; }
}
function fireSched(s) {
  const r = runSched(s);
  if (r.ok) { s.status = 'running'; s.taskId = r.taskId; s.firedAt = simNow(); toast(`排期 ${s.id} 已开始执行`); }
  else { s.status = 'failed'; s.reason = r.reason; toast(`排期 ${s.id} 执行失败：${r.reason}`); }
}
function schedRefresh() { updateSchedBadge(); if (location.hash === '#schedule' && pages.schedule.body) pages.schedule.body(); if (schedState.openId && $('#drawer').classList.contains('open') && $('#schWrap')) renderSchedDetail(); }
function updateSchedBadge() {
  const n = SCHEDULES.filter(s => s.status === 'pending' || s.status === 'ready').length, b = $('#schedBadge'); if (b) { b.style.display = n ? '' : 'none'; b.textContent = n; }
}
function nextSchedNotice() {
  const list = SCHEDULES.filter(s => ['pending', 'approved', 'ready'].includes(s.status) && s.start < simNow() + 3 * DAY).sort((a, b) => a.start - b.start); if (!list.length) return '';
  const pend = SCHEDULES.filter(s => s.status === 'pending').length, s = list[0];
  return `<div class="notice info" style="margin-bottom:12px"><span>📅</span><div><b>下一个变更排期：</b>${fDT(s.start)} · ${esc(s.title)}（${SCHED_STATUS[s.status][1]}）${pend ? ` · 另有 ${pend} 个待审批` : ''} <a data-go="schedule">查看排期 →</a></div></div>`;
}

/* ---------- 引擎：每秒检查 ---------- */
setInterval(() => {
  const now = simNow(); let changed = false;
  SCHEDULES.forEach(s => {
    if (s.status === 'approved' && s.start <= now) { if (s.auto) fireSched(s); else { s.status = 'ready'; toast(`排期 ${s.id} 已到点，等待人工确认执行`); } changed = true; }
    else if (s.status === 'pending' && s.start <= now) { s.status = 'cancelled'; s.reason = '超过计划开始时间仍未通过审批，已自动取消'; changed = true; }
    else if (s.status === 'running') {
      const t = TASKS.find(x => x.id === s.taskId);
      if (t && t.state === 'success') { s.status = 'done'; s.endedAt = now; changed = true; }
      else if (t && t.state === 'aborted') { s.status = 'failed'; s.reason = '任务被中止'; changed = true; }
    }
  });
  $$('[data-sclock]').forEach(el => { el.textContent = fDT(now) + (simOffset ? '（演示时钟）' : ''); });
  if (changed) schedRefresh();
}, 1000);

/* ---------- 审批 / 操作 ---------- */
function approveSched(s) {
  const u = schedState.user;
  if (u === s.owner || (u === 'you' && s.owner === 'you')) return toast('申请人不能审批自己的变更');
  if (u === 'you') return toast('当前身份是申请人，请在右上角切换为审批人');
  if (s.approvals.some(a => a.by === u)) return toast('你已审批过该变更');
  s.approvals.push({by: u, at: simNow()}); if (s.approvals.length >= s.needs) { s.status = 'approved'; toast(`${s.id} 已批准，将在 ${fDT(s.start)} 执行`); } else toast(`已通过（${s.approvals.length}/${s.needs}），还需 ${s.needs - s.approvals.length} 级审批`);
  schedRefresh();
}
function rescheduleSched(s, start) {
  const c = schedChecks({...s, start}, s.id, {fresh: true}); if (c.blocked) { toast('新时间未通过规则检查：' + c.items.find(i => i.lv === 'bad').t); return false; }
  s.start = start; if (s.status === 'ready') s.status = 'approved'; toast(`已改期到 ${fDT(start)}`); return true;
}

/* ---------- 页面 ---------- */
pages.schedule = {
  title: '变更排期',
  render() {
    const pend = SCHEDULES.filter(s => s.status === 'pending').length;
    return `<div class="card" style="margin-bottom:14px"><div class="row">
      <div class="chips"><span class="chip ${schedState.view === 'cal' ? 'active' : ''}" data-na="sc-view" data-v="cal">日历</span><span class="chip ${schedState.view === 'list' ? 'active' : ''}" data-na="sc-view" data-v="list">列表</span></div>
      <span class="small muted">当前时间：<b data-sclock>${fDT(simNow())}</b></span>
      <button class="btn sm" data-na="sc-ff">⏩ 快进到下一个排期</button><button class="btn sm" data-na="sc-reset">↺ 重置时钟</button><label class="small" title="开启后，依赖尚未发布的路线图能力的变更类型不允许排期"><input type="checkbox" data-nc="sc-strict" ${schedState.strict ? 'checked' : ''}> 仅允许已上线能力</label>
      <span style="margin-left:auto" class="row"><label class="small muted">当前身份</label><select data-nc="sc-user">${Object.entries(SCHED_USERS).map(([k, v]) => `<option value="${k}" ${schedState.user === k ? 'selected' : ''}>${v}</option>`).join('')}</select>
      <button class="btn primary" data-na="sc-new">＋ 新建排期</button></span></div>
      <div class="small muted" style="margin-top:8px">窗口规则：每日 02:00–02:40 备份窗口禁止变更 · 高峰 10:00–12:00 / 19:00–23:00 高风险禁止 · 推荐低峰 02:40–06:00 · ${FREEZES.map(f => `${f.label}：${fD(f.from)}–${fD(f.to - DAY)}`).join(' · ')}。${pend ? `<b>${pend}</b> 个待审批。` : ''}</div></div>
      <div id="schBody"></div>`;
  },
  init() { this.body(); const na = window.navArgs; window.navArgs = null; if (na && na.node && SCHEDULES.some(x => x.id === na.node)) openSched(na.node); },
  body() {
    const el = $('#schBody'); if (!el) return;
    el.innerHTML = schedState.view === 'cal' ? this.cal() : this.list();
    $$('.chip[data-na="sc-view"]').forEach(c => c.classList.toggle('active', c.dataset.v === schedState.view));
  },
  cal() {
    const m0 = new Date(schedState.month), y = m0.getFullYear(), mo = m0.getMonth(), off = (new Date(y, mo, 1).getDay() + 6) % 7, today = sod(simNow());
    const cells = Array.from({length: 42}, (_, i) => new Date(y, mo, 1 - off + i).getTime());
    const byDay = {}; SCHEDULES.forEach(s => (byDay[dateVal(s.start)] = byDay[dateVal(s.start)] || []).push(s));
    const grid = cells.map(t => {
      const evs = (byDay[dateVal(t)] || []).sort((a, b) => a.start - b.start), d = new Date(t);
      return `<div class="day ${d.getMonth() !== mo ? 'other' : ''} ${t === today ? 'today' : ''} ${t === schedState.sel ? 'sel' : ''} ${freezeOver(t, t + DAY).length ? 'freeze' : ''}" data-na="sc-day" data-t="${t}">
        <div class="dn">${d.getDate()}</div>${RM_MILESTONES.filter(m => dateVal(rmMsDate(m)) === dateVal(t)).map(m => `<div class="ev ms" title="路线图里程碑 ${m.id} ${esc(m.name)}（${m.env}）">🚩 ${m.id} ${m.name}</div>`).join('')}${evs.slice(0, 3).map(s => `<div class="ev ${s.status}" style="--c:var(${SCHED_TYPES[s.type].color})" data-na="sc-open" data-id="${s.id}" title="${esc(s.title)}">${fT(s.start)} ${SCHED_TYPES[s.type].label}</div>`).join('')}${evs.length > 3 ? `<div class="small muted">+${evs.length - 3} 项</div>` : ''}</div>`;
    }).join('');
    const sel = schedState.sel, dayItems = SCHEDULES.filter(s => s.start < sel + DAY && s.start + s.dur * MIN > sel).sort((a, b) => a.start - b.start);
    const pxm = 28 / 60, zone = (a, b, bg, label) => `<div class="zone" style="top:${a * pxm}px;height:${(b - a) * pxm}px;background:${bg}"><span>${label}</span></div>`;
    let tl = Array.from({length: 24}, (_, h) => `<div class="hr24" style="top:${h * 28}px;height:28px">${p2(h)}:00</div>`).join('');
    tl += zone(RULE.good[0], RULE.good[1], 'var(--ok-soft)', '推荐低峰') + zone(RULE.backup[0], RULE.backup[1], 'var(--bad-soft)', '备份窗口') + RULE.peaks.map(([a, b]) => zone(a, b, 'var(--warn-soft)', '业务高峰')).join('');
    const fzs = freezeOver(sel, sel + DAY); if (fzs.length) tl += `<div class="zone" style="top:0;height:672px;background:repeating-linear-gradient(135deg,var(--bad-soft),var(--bad-soft) 8px,transparent 8px,transparent 16px);opacity:.7"><span style="top:auto;bottom:4px">${fzs[0].label}</span></div>`;
    tl += dayItems.map(s => { const a = Math.max(s.start, sel), b = Math.min(s.start + s.dur * MIN, sel + DAY); return `<div class="blk ${s.status}" style="--c:var(${SCHED_TYPES[s.type].color});top:${(a - sel) / MIN * pxm}px;height:${Math.max(18, (b - a) / MIN * pxm)}px" data-na="sc-open" data-id="${s.id}">${fT(s.start)} ${SCHED_TYPES[s.type].label} · ${esc(s.title.replace(/^[^：]*：/, '').slice(0, 20))}</div>`; }).join('');
    if (sel === today) tl += `<div class="now" style="top:${(simNow() - sel) / MIN * pxm}px"></div>`;
    return `<div class="grid g-main" style="grid-template-columns:minmax(0,1fr) 380px"><div class="card"><h2>${y} 年 ${mo + 1} 月<span class="right"><button class="btn sm" data-na="sc-month" data-d="-1">‹ 上月</button><button class="btn sm" data-na="sc-month" data-d="0">今天</button><button class="btn sm" data-na="sc-month" data-d="1">下月 ›</button></span></h2>
      <div class="cal">${WEEKS.map(w => `<div class="wd">周${w}</div>`).join('')}${grid}</div>
      <div class="legend">${Object.values(SCHED_TYPES).map(t => `<span><i style="background:var(${t.color})"></i>${t.label}</span>`).join('')}<span>虚线 = 待审批</span><span style="color:var(--bad)">▨ 冻结期</span></div></div>
      <div class="card"><h2>${fD(sel)} <span class="sub">${dayItems.length} 项</span><span class="right"><button class="btn sm" data-na="sc-new" data-d="${sel}">＋ 当天新建</button></span></h2><div class="tl24">${tl}</div></div></div>`;
  },
  list() {
    const st = schedState, list = SCHEDULES.filter(s => (st.fStatus === 'all' || s.status === st.fStatus) && (st.fType === 'all' || s.type === st.fType)).sort((a, b) => a.start - b.start);
    return `<div class="card"><h2>排期列表 <span class="sub">${list.length} / ${SCHEDULES.length}</span></h2>
      <div class="row" style="margin-bottom:10px"><div class="chips">${[['all', '全部状态'], ...Object.entries(SCHED_STATUS).map(([k, v]) => [k, v[1]])].map(([k, l]) => `<span class="chip ${st.fStatus === k ? 'active' : ''}" data-na="sc-fs" data-k="${k}">${l}</span>`).join('')}</div>
      <div class="chips">${[['all', '全部类型'], ...Object.entries(SCHED_TYPES).map(([k, v]) => [k, v.label])].map(([k, l]) => `<span class="chip ${st.fType === k ? 'active' : ''}" data-na="sc-ft" data-k="${k}">${l}</span>`).join('')}</div></div>
      <div class="tbl-wrap"><table><thead><tr><th>编号</th><th>开始时间</th><th>类型</th><th>内容</th><th>风险</th><th>审批</th><th>状态</th><th>申请人</th></tr></thead><tbody>
      ${list.map(s => `<tr class="click" data-na="sc-open" data-id="${s.id}"><td class="mono small">${s.id}</td><td>${fDT(s.start)}<div class="small muted">${s.dur} 分钟</div></td><td><span class="tag" style="border-left:3px solid var(${SCHED_TYPES[s.type].color})">${SCHED_TYPES[s.type].label}</span></td><td style="max-width:360px">${esc(s.title)}</td>
        <td>${tag(RISK_TAG[s.risk][1], RISK_TAG[s.risk][0])}</td><td class="small">${s.needs ? `${s.approvals.length}/${s.needs}` : '免审批'}</td><td>${tag(SCHED_STATUS[s.status][1], SCHED_STATUS[s.status][0])}</td><td class="small">${s.owner}</td></tr>`).join('') || '<tr><td colspan="8" class="empty">没有符合条件的排期</td></tr>'}</tbody></table></div></div>`;
  },
};
const WEEKS = ['一', '二', '三', '四', '五', '六', '日'];

/* ---------- 详情抽屉 ---------- */
function openSched(id) { schedState.openId = id; openDrawer('', '<div id="schWrap"></div>'); renderSchedDetail(); }
function renderSchedDetail() {
  const s = SCHEDULES.find(x => x.id === schedState.openId), el = $('#schWrap'); if (!s || !el) return;
  $('#drTitle').textContent = `${s.id} · ${SCHED_TYPES[s.type].label}`;
  const c = schedChecks(s, s.id, {fresh: false}), u = schedState.user, canApprove = s.status === 'pending' && u !== 'you' && u !== s.owner && !s.approvals.some(a => a.by === u);
  const steps = ['提交申请', ...Array.from({length: s.needs}, (_, i) => `${i + 1} 级审批`), '到点执行', '完成'];
  const stIdx = s.status === 'pending' ? 1 + s.approvals.length : s.status === 'approved' || s.status === 'ready' ? 1 + s.needs : s.status === 'running' ? 2 + s.needs : s.status === 'done' ? 3 + s.needs : 1 + s.approvals.length;
  const t = s.taskId && TASKS.find(x => x.id === s.taskId);
  el.innerHTML = `<div class="row" style="margin-bottom:10px">${tag(SCHED_STATUS[s.status][1], SCHED_STATUS[s.status][0])}${tag(RISK_TAG[s.risk][1], RISK_TAG[s.risk][0])}${s.emergency ? tag('紧急变更', 'bad') : ''}${tag(s.auto ? '到点自动执行' : '到点人工确认')}</div>
    <h3 style="font-size:15px;margin:0 0 8px">${esc(s.title)}</h3>
    <div class="kv"><span>内容</span><span>${esc(payloadSummary(s))}</span><span>开始时间</span><span>${fDT(s.start)}</span><span>预计结束</span><span>${fDT(s.start + s.dur * MIN)}（${s.dur} 分钟）</span><span>申请人</span><span>${s.owner}</span><span>工单</span><span>${esc(s.ticket || '—')}</span>${s.reason ? `<span>说明</span><span>${esc(s.reason)}</span>` : ''}</div>
    <h3 style="font-size:14px;margin:14px 0 6px">审批与执行流程</h3>
    <div class="flow">${steps.map((l, i) => `<span class="st ${s.status === 'rejected' && i === s.approvals.length + 1 ? 'bad' : i < stIdx ? 'done' : i === stIdx ? 'cur' : ''}">${l}</span>${i < steps.length - 1 ? '<span class="muted">›</span>' : ''}`).join('')}</div>
    ${s.approvals.length ? `<div class="small muted" style="margin-top:6px">${s.approvals.map(a => `${a.by} 于 ${fDT(a.at)} 通过`).join('；')}</div>` : ''}
    ${t ? `<div class="notice info"><span>⚙</span><div>任务 ${t.id}：${t.state === 'running' ? '执行中' : t.state === 'success' ? '已成功' : '已中止'} <a data-na="nd-goto" data-t="tasks">任务中心 →</a></div></div>` : ''}
    ${s.type === 'release' ? `<div class="notice info"><span>⇢</span><div>路线图里程碑 ${s.payload.ms}：${rmMs(s.payload.ms) ? esc(rmMs(s.payload.ms).name) : ''}</div></div>` : ''}
    ${s.type === 'maint' && s.status === 'done' ? `<div class="notice warn"><span>ℹ</span><div>排期只负责“进入维护”。维护窗口内的倒计时、检查单和结束维护请到 <a data-na="nd-goto" data-t="maint">主机维护</a> 完成。</div></div>` : ''}
    <h3 style="font-size:14px;margin:14px 0 6px">${['done', 'failed', 'rejected', 'cancelled'].includes(s.status) ? '创建时规则检查' : '窗口与冲突检查'}</h3>
    ${c.items.map(i => `<div class="notice ${i.lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[i.lv]}</span><div>${i.t}</div></div>`).join('')}
    ${['pending', 'approved', 'ready'].includes(s.status) ? `<h3 style="font-size:14px;margin:14px 0 6px">改期</h3><div class="row"><input type="date" id="rsD" value="${dateVal(s.start)}"><input type="time" id="rsT" value="${fT(s.start)}"><button class="btn sm" data-na="sc-resched" data-id="${s.id}">应用并重新检查</button></div>` : ''}
    <div class="sticky-actions">
      ${canApprove ? `<button class="btn primary" data-na="sc-approve" data-id="${s.id}">通过（${u === 'ops_li' ? 'ops_li' : 'dba_chen'}）</button><button class="btn danger" data-na="sc-reject" data-id="${s.id}">驳回</button>` : s.status === 'pending' ? '<span class="small muted">需审批人处理：请在页面右上角切换身份（申请人不能审批自己的变更）</span>' : ''}
      ${s.status === 'ready' ? `<button class="btn primary" data-na="sc-confirm" data-id="${s.id}">确认执行</button>` : ''}
      ${s.status === 'approved' ? `<button class="btn" data-na="sc-runnow" data-id="${s.id}">跳过等待，立即执行</button>` : ''}
      ${['pending', 'approved', 'ready'].includes(s.status) ? `<button class="btn danger" data-na="sc-cancel" data-id="${s.id}">取消排期</button>` : ''}
      ${['done', 'failed', 'rejected', 'cancelled'].includes(s.status) ? `<button class="btn" data-na="sc-copy" data-id="${s.id}">复制为新排期</button>` : ''}</div>`;
}

/* ---------- 新建 / 预约表单 ---------- */
const sf = {type: 'maint', payload: {}, date: '', time: '03:00', dur: 60, ticket: '', auto: true, emergency: false, reason: '', locked: false, wizard: false};
function defPayload(type) {
  return {
    maint: {ip: '10.0.2.13', mtype: 'kernel', mins: 25, reason: ''}, scaleout: {role: 'tikv', ips: [], cfg: null}, scalein: {ids: []},
    config: {nodeId: 'tidb-1', scope: 'instance', changes: [{k: 'tidb_mem_quota_query', to: '2GB'}]}, restart: {role: 'pd'}, release: {ms: (RM_MILESTONES.find(m => !rmState.released[m.id]) || RM_MILESTONES[0]).id},
  }[type];
}
function openSchedForm(type, payload, dayTs, wizard) {
  Object.assign(sf, {type: type || 'maint', payload: payload || defPayload(type || 'maint'), ticket: '', auto: true, emergency: false, reason: '', locked: !!wizard, wizard: !!wizard});
  const base = dayTs ? dayTs : sod(simNow()) + DAY; sf.date = dateVal(base); sf.time = '03:00'; sf.dur = estDur(sf.type, sf.payload);
  openDrawer('新建变更排期', '<div id="sfWrap"></div>'); $('#drawer').classList.add('wide'); renderSf();
}
function sfDraft() {
  const start = new Date(`${sf.date}T${sf.time}`).getTime(), risk = schedRisk(sf.type, sf.payload);
  return {id: null, type: sf.type, payload: sf.payload, start, dur: Math.max(5, +sf.dur || 30), owner: 'you', risk, auto: sf.auto, emergency: sf.emergency, reason: sf.reason};
}
function sfPayloadForm() {
  const p = sf.payload, t = sf.type, hosts = clusterHosts();
  if (t === 'release') { const m = rmMs(p.ms); return `<div class="field"><label>路线图里程碑</label><select data-nc="sf-p" data-k="ms">${RM_MILESTONES.map(x => `<option value="${x.id}" ${p.ms === x.id ? 'selected' : ''}>${x.id} W${x.week} ${x.name}（${x.env}）${rmState.released[x.id] ? ' · 已发布' : ''}</option>`).join('')}</select></div><div class="notice info"><span>🚀</span><div><b>发布范围</b><ul style="margin:4px 0 0;padding-left:18px">${m.accept.map(a => `<li>${a}</li>`).join('')}</ul>发布成功后，该里程碑下的任务会在路线图中自动标记为完成。</div></div>`; }
  if (t === 'maint') return `<div class="field"><label>维护主机</label><select data-nc="sf-p" data-k="ip">${hosts.map(h => `<option ${p.ip === h.ip ? 'selected' : ''} value="${h.ip}">${h.ip}（${h.insts.join('、')}）</option>`).join('')}</select></div>
    <div class="field"><label>维护类型</label><select data-nc="sf-p" data-k="mtype">${Object.entries(MAINT_TYPES).map(([k, v]) => `<option value="${k}" ${p.mtype === k ? 'selected' : ''}>${v.label}</option>`).join('')}</select></div>
    <div class="field"><label>预计停机（分钟）</label><input type="text" data-ni="sf-p" data-k="mins" value="${p.mins}"></div><div class="field"><label>维护原因</label><input type="text" data-ni="sf-p" data-k="reason" value="${esc(p.reason || '')}"></div>`;
  if (t === 'scaleout') return `<div class="field"><label>扩容角色</label><select data-nc="sf-p" data-k="role">${ROLE_IDS.map(r => `<option value="${r}" ${p.role === r ? 'selected' : ''}>${TYPE_META[r].label}</option>`).join('')}</select></div>
    <div class="field"><label>目标主机（来自主机池）</label>${poolAll().map(h => { const cs = checkSummary(h); return `<label class="small" style="display:flex;gap:6px"><input type="checkbox" data-nc="sf-ip" value="${h.ip}" ${p.ips.includes(h.ip) ? 'checked' : ''}> <span class="mono">${h.ip}</span> ${h.az} ${tag(cs.t, cs.c)}</label>`; }).join('')}</div>`;
  if (t === 'scalein') return `<div class="field"><label>下线节点</label>${NODES.map(n => `<label class="small" style="display:flex;gap:6px"><input type="checkbox" data-nc="sf-id" value="${n.id}" ${p.ids.includes(n.id) ? 'checked' : ''}> ${n.id} <span class="muted">${n.host} · ${n.az}</span></label>`).join('')}</div>`;
  if (t === 'config') {
    const n = nodeBy(p.nodeId) || NODES[0], items = catalog(n.type), c = p.changes[0] || {k: items[0].k, to: ''}, it = items.find(x => x.k === c.k) || items[0];
    return `<div class="field"><label>节点</label><select data-nc="sf-p" data-k="nodeId">${NODES.map(x => `<option ${p.nodeId === x.id ? 'selected' : ''}>${x.id}</option>`).join('')}</select></div>
      <div class="field"><label>配置项</label><select data-nc="sf-cfgk">${items.map(i => `<option value="${i.k}" ${it.k === i.k ? 'selected' : ''}>${i.k}（${i.online ? '在线' : '需 reload'}）</option>`).join('')}</select></div>
      <div class="field"><label>新值（当前 ${fmtV(getVal(n, it))}）</label><input type="text" data-ni="sf-cfgv" value="${esc(c.to)}"></div>
      ${it.scope === 'instance' ? `<div class="field"><label>应用范围</label><select data-nc="sf-p" data-k="scope"><option value="instance" ${p.scope === 'instance' ? 'selected' : ''}>仅此实例</option><option value="role" ${p.scope === 'role' ? 'selected' : ''}>全部同角色实例</option></select></div>` : ''}`;
  }
  return `<div class="field"><label>滚动重启角色</label><select data-nc="sf-p" data-k="role">${ROLE_IDS.map(r => `<option value="${r}" ${p.role === r ? 'selected' : ''}>${TYPE_META[r].label}（${NODES.filter(n => n.type === r).length} 个）</option>`).join('')}</select></div>`;
}
function sfChecksHtml() {
  const d = sfDraft(), c = schedChecks(d, null, {fresh: true}), need = needsOf(d.risk, d.emergency), rec = d.type === 'release' ? null : recommendSlots(d);
  return `<h3 style="font-size:14px;margin:0 0 6px">规则与冲突检查</h3>${c.items.map(i => `<div class="notice ${i.lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[i.lv]}</span><div>${i.t}</div></div>`).join('')}
    ${rec ? `<h3 style="font-size:14px;margin:12px 0 6px">推荐窗口 <span class="small muted">未来 14 天内通过全部规则、流量最低</span></h3>` : '<div class="small muted" style="margin-top:8px">平台发布的日期跟随路线图里程碑，可直接修改日期；变更冻结期仅约束生产发布。</div>'}
    ${(rec || []).map((r, i) => `<div class="slot ${i === 0 ? 'best' : ''}" data-na="sf-slot" data-t="${r.start}"><b>${fDT(r.start)}</b><span class="small muted">至 ${fT(r.start + d.dur * MIN)} · ${['周日', '周一', '周二', '周三', '周四', '周五', '周六'][new Date(r.start).getDay()]}</span>${i === 0 ? tag('最优', 'ok') : ''}${r.warns ? tag(`${r.warns} 项提醒`, 'warn') : ''}<span style="margin-left:auto" class="small muted">点击采用</span></div>`).join('') || (rec ? '<div class="muted small">未找到可用窗口，请缩短时长或调整内容</div>' : '')}
    <div class="sticky-actions"><button class="btn primary" id="sfGo" data-na="sf-submit" ${c.blocked ? 'disabled' : ''}>${need ? `提交审批（需 ${need} 级）` : '提交排期'}</button><button class="btn" data-close="1">取消</button>${c.blocked ? '<span class="small" style="color:var(--bad)">存在阻断项，请调整时间或内容</span>' : ''}</div>`;
}
function renderSf() {
  const el = $('#sfWrap'); if (!el) return;
  el.innerHTML = `<div class="grid g2"><div>
    <div class="field"><label>变更类型</label><div class="chips">${Object.entries(SCHED_TYPES).map(([k, v]) => `<span class="chip ${sf.type === k ? 'active' : ''}" ${sf.locked ? 'style="opacity:.5;pointer-events:none"' : ''} data-na="sf-type" data-k="${k}">${v.label}</span>`).join('')}</div></div>
    ${sf.wizard ? `<div class="notice info"><span>ℹ</span><div>已带入向导中的参数：<b>${esc(payloadSummary({type: sf.type, payload: sf.payload}))}</b></div></div>` : sfPayloadForm()}
    <div class="grid g2"><div class="field"><label>开始日期</label><input type="date" data-ni="sf-date" value="${sf.date}"></div><div class="field"><label>开始时间</label><input type="time" data-ni="sf-time" value="${sf.time}"></div></div>
    <div class="field"><label>预计时长（分钟）</label><input type="text" data-ni="sf-dur" value="${sf.dur}"></div><div class="field"><label>变更工单号</label><input type="text" data-ni="sf-ticket" value="${esc(sf.ticket)}" placeholder="CHG-12345"></div>
    <label class="small" style="display:flex;gap:6px"><input type="checkbox" data-nc="sf-auto" ${sf.auto ? 'checked' : ''}> 到点自动执行（取消则到点只提醒，需人工确认）</label>
    <label class="small" style="display:flex;gap:6px;margin-top:6px"><input type="checkbox" data-nc="sf-emg" ${sf.emergency ? 'checked' : ''}> 紧急变更（可在冻结期执行，需 2 级审批并留痕）</label>
    ${sf.emergency ? `<div class="field" style="margin-top:6px"><label>紧急原因（必填）</label><input type="text" data-ni="sf-reason" value="${esc(sf.reason)}"></div>` : ''}</div>
    <div id="sfChecks">${sfChecksHtml()}</div></div>`;
}
const sfUpdate = () => { const c = $('#sfChecks'); if (c) c.innerHTML = sfChecksHtml(); };

/* ---------- 事件 ---------- */
document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
  const d = el.dataset, S = schedState, byId = id => SCHEDULES.find(x => x.id === id);
  switch (d.na) {
    case 'sc-view': S.view = d.v; pages.schedule.body(); break;
    case 'sc-month': { const m = new Date(S.month); S.month = d.d === '0' ? sod(simNow()) : new Date(m.getFullYear(), m.getMonth() + +d.d, 1).getTime(); if (d.d === '0') S.sel = sod(simNow()); pages.schedule.body(); break; }
    case 'sc-day': S.sel = +d.t; pages.schedule.body(); break;
    case 'sc-open': e.stopPropagation(); openSched(d.id); break;
    case 'sc-new': openSchedForm('maint', null, d.d ? +d.d : null, false); break;
    case 'sc-ff': { const n = SCHEDULES.filter(s => s.status === 'approved' && s.start > simNow()).sort((a, b) => a.start - b.start)[0]; if (!n) { toast('没有待执行的已批准排期'); break; } simOffset += n.start - simNow() - 3000; S.sel = sod(n.start); S.month = sod(n.start); toast(`时钟已快进到 ${fDT(simNow())}，${n.id} 将在 3 秒后触发`); pages.schedule.body(); break; }
    case 'sc-reset': simOffset = 0; S.sel = sod(simNow()); S.month = sod(simNow()); toast('已重置为真实时间'); pages.schedule.body(); break;
    case 'sc-fs': S.fStatus = d.k; pages.schedule.body(); break;
    case 'sc-ft': S.fType = d.k; pages.schedule.body(); break;
    case 'sc-approve': approveSched(byId(d.id)); break;
    case 'sc-reject': { const s = byId(d.id); s.status = 'rejected'; s.reason = `驳回：由 ${S.user} 驳回（演示）`; toast('已驳回'); schedRefresh(); break; }
    case 'sc-cancel': { const s = byId(d.id); s.status = 'cancelled'; toast('排期已取消'); schedRefresh(); break; }
    case 'sc-runnow': case 'sc-confirm': { const s = byId(d.id); fireSched(s); schedRefresh(); break; }
    case 'sc-resched': { const s = byId(d.id), t = new Date(`${$('#rsD').value}T${$('#rsT').value}`).getTime(); if (rescheduleSched(s, t)) schedRefresh(); break; }
    case 'sc-copy': { const s = byId(d.id); openSchedForm(s.type, JSON.parse(JSON.stringify(s.payload)), null, false); break; }
    case 'sf-type': sf.type = d.k; sf.payload = defPayload(d.k); sf.dur = estDur(d.k, sf.payload); if (d.k === 'release') { const t = rmMsDate(rmMs(sf.payload.ms)); sf.date = dateVal(t); sf.time = fT(t); } renderSf(); break;
    case 'sf-slot': { const t = +d.t; sf.date = dateVal(t); sf.time = fT(t); renderSf(); break; }
    case 'sf-submit': {
      const dr = sfDraft(), c = schedChecks(dr, null, {fresh: true}); if (c.blocked) break;
      const need = needsOf(dr.risk, dr.emergency), id = 'S' + String(schedSeq++).padStart(4, '0');
      SCHEDULES.push({id, type: sf.type, title: `${SCHED_TYPES[sf.type].label}：${payloadSummary({type: sf.type, payload: sf.payload})}`, payload: JSON.parse(JSON.stringify(sf.payload)), start: dr.start, dur: dr.dur, owner: 'you', ticket: sf.ticket, auto: sf.auto, emergency: sf.emergency, reason: sf.emergency ? sf.reason : '',
        risk: dr.risk, needs: need, approvals: [], status: need ? 'pending' : 'approved', created: simNow()});
      closeDrawer(); toast(need ? `${id} 已提交，等待 ${need} 级审批` : `${id} 已创建`); S.sel = sod(dr.start); S.month = sod(dr.start); S.view = 'cal'; schedRefresh();
      if (location.hash !== '#schedule') location.hash = 'schedule'; break;
    }
    case 'sched-from': {
      const st = nodesState; let type = d.src, payload;
      if (type === 'maint') payload = {ip: mtState.ip, mtype: mtState.type, mins: +mtState.mins, reason: mtState.reason};
      else if (type === 'scaleout') payload = {role: st.so.role, ips: st.so.hosts.map(h => h.ip), cfg: JSON.parse(JSON.stringify(st.so.cfg))};
      else if (type === 'scalein') payload = {ids: st.si.picks.slice()};
      else { const n = nodeBy(ndState.id), ch = draftChanges(n); payload = {nodeId: n.id, scope: ndState.scope, changes: ch.map(c => ({k: c.i.k, to: c.to}))}; }
      openSchedForm(type, payload, null, true); break;
    }
  }
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc]'); if (!el) return; const d = el.dataset;
  switch (d.nc) {
    case 'sc-user': schedState.user = el.value; schedRefresh(); break;
    case 'sc-strict': schedState.strict = el.checked; schedRefresh(); break;
    case 'sf-p': sf.payload[d.k] = el.value; if (d.k === 'ms') { const t = rmMsDate(rmMs(el.value)); sf.date = dateVal(t); sf.time = fT(t); } if (d.k === 'role' || d.k === 'ip' || d.k === 'nodeId') { if (d.k === 'role' && sf.type === 'scaleout') sf.payload.ips = []; if (d.k === 'nodeId') { const n = nodeBy(el.value); sf.payload.changes = [{k: catalog(n.type)[0].k, to: ''}]; } } sf.dur = estDur(sf.type, sf.payload); renderSf(); break;
    case 'sf-ip': sf.payload.ips = el.checked ? [...sf.payload.ips, el.value] : sf.payload.ips.filter(x => x !== el.value); sfUpdate(); break;
    case 'sf-id': sf.payload.ids = el.checked ? [...sf.payload.ids, el.value] : sf.payload.ids.filter(x => x !== el.value); sf.dur = estDur(sf.type, sf.payload); renderSf(); break;
    case 'sf-cfgk': sf.payload.changes = [{k: el.value, to: ''}]; renderSf(); break;
    case 'sf-auto': sf.auto = el.checked; break;
    case 'sf-emg': sf.emergency = el.checked; renderSf(); break;
  }
});
document.addEventListener('input', e => {
  const el = e.target.closest('[data-ni]'); if (!el) return; const d = el.dataset;
  switch (d.ni) {
    case 'sf-p': sf.payload[d.k] = el.value; if (d.k === 'mins') sf.dur = estDur(sf.type, sf.payload); sfUpdate(); break;
    case 'sf-cfgv': sf.payload.changes = [{k: sf.payload.changes[0].k, to: el.value}]; sfUpdate(); break;
    case 'sf-date': sf.date = el.value; sfUpdate(); break;
    case 'sf-time': sf.time = el.value; sfUpdate(); break;
    case 'sf-dur': sf.dur = el.value; sfUpdate(); break;
    case 'sf-ticket': sf.ticket = el.value.trim(); break;
    case 'sf-reason': sf.reason = el.value; sfUpdate(); break;
  }
});
updateSchedBadge();
