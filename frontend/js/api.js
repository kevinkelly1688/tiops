// @author: kevinkelly
// @license: Apache License 2.0
// @file: api.js
// @time: 2026/10/28

/* 数据层：如果同源提供了 tidbops-server 的只读接口（/api/snapshot），就把真实（或回放）数据映射进原型的数据模型，
   这样总览 / 架构图 / SQL 优化 / 热点 / 诊断 / 环境与设置等页面直接显示真实数据；否则（静态托管 / file://）保持原型的模拟数据。
   后端只提供只读数据；扩缩容、配置变更、主机维护等写操作在真实数据模式下仍是前端模拟，不会作用于集群。 */
const LIVE = {on: false, mode: '', snap: null, env: {}, at: 0, health: null};

/* 拓扑里的标识类字段（实例 id / 主机 / 可用区 / 标签 / 版本 / 集群名）在页面里有几百处直接拼进 innerHTML，而且同时用作查找键、data-* 值和命令文本，
   不能做 HTML 转义（会和 dataset 里解码后的值对不上）。正常取值只会是字母数字与 . _ - : 等；这里把 HTML 特殊字符换成「?」，在入口一处兜住。 */
const safeTxt = v => String(v == null ? '' : v).replace(/[<>"'&`\u0000-\u001f]/g, '?');
const mapNode = i => ({
  id: safeTxt(i.id), type: i.type, host: safeTxt(i.host), port: i.port, az: safeTxt(i.az) || '—', status: i.status, cpu: i.cpu || 0, mem: i.mem || 0, disk: i.disk || 0,
  regions: i.regions || 0, leaders: i.leaders || 0, wr: i.wr || 0, rd: i.rd || 0, raftCpu: i.raftCpu || 0, qps: i.qps || 0, conn: i.conn || 0,
  leader: !!i.leader, sid: i.sid, note: i.note || '', lag: safeTxt(i.lag) || '—', version: safeTxt(i.version),
  deployDir: i.deployDir || '', dataDir: i.dataDir || '', logDir: i.logDir || '', logDirInferred: !!i.logDirInferred,   // 实例目录：取不到为空串，页面显示「—（未采集）」
  labels: {zone: safeTxt((i.labels && i.labels.zone) || i.az) || '—', rack: safeTxt(i.labels && i.labels.rack) || '—', host: safeTxt((i.labels && i.labels.host) || String(i.host).replace(/\./g, '-'))},
});
const mapSlow = g => ({id: g.id, digest: g.digest, db: g.db, user: g.user, sql: g.sql, exec: g.exec, avg: g.avg, p99: g.p99, total: g.total, keys: g.keys, pkeys: g.pkeys, rows: Math.round(g.rows),
  mem: g.mem || '—', index: g.index, tag: g.tag, plan: g.plan || [], tips: (g.tips || []).map(t => ({t: t.t, cmd: t.cmd}))});
const mapFinding = f => ({id: f.id, sev: f.sev, title: f.title, scope: f.scope, evidence: f.evidence || [], cause: f.cause, actions: (f.actions || []).map(a => ({t: a.t, cmd: a.cmd})),
  link: f.link || {page: 'diag', label: '查看'}, ref: f.ref});

/* 读取后端只读洞察接口：运维模式（需登录）走 opApi，只读模式直接 fetch；失败抛错 */
async function liveGet(path) {
  if (typeof OP !== 'undefined' && OP.on) {
    const r = await opApi('GET', path);
    if (r.code !== 200) throw new Error(r.code === 401 ? '未登录或会话已过期' : (r.data && r.data.error) || ('HTTP ' + r.code));
    return r.data;
  }
  const r = await fetch(path, {cache: 'no-store'});
  if (!r.ok) { let m = 'HTTP ' + r.status; try { const j = await r.json(); if (j && j.error) m = j.error; } catch (e) {} throw new Error(m); }
  return r.json();
}

function liveApply(s) {
  if (s.trends) ['qps', 'p99ms', 'conns'].forEach(k => { if (!s.trends[k]) s.trends[k] = []; });   // 不可达 / 刚接入的集群：趋势为 null
  LIVE.snap = s; LIVE.at = Date.now(); LIVE.env = {}; (s.env || []).forEach(e => { LIVE.env[e.key] = e; });
  if (s.topology.cluster) CLUSTER = safeTxt(s.topology.cluster);
  if (s.topology.version) VERSION = safeTxt(s.topology.version);
  NODES.length = 0; (s.topology.instances || []).forEach(i => NODES.push(mapNode(i)));
  NODES.forEach(n => { if (!AZS[n.az]) AZS[n.az] = n.az; });
  SQLS.length = 0; (s.slow || []).forEach(g => SQLS.push(mapSlow(g)));
  ALERTS.length = 0; (s.alerts || []).forEach(a => ALERTS.push({lv: a.lv, name: a.name, inst: a.inst, time: a.time, desc: a.desc}));  FINDINGS.length = 0; (s.findings || []).forEach(f => FINDINGS.push(mapFinding(f)));
  REGIONS.length = 0; const seen = new Set();
  (s.hot || []).forEach(h => { if (seen.has(h.id)) return; seen.add(h.id); REGIONS.push({id: h.id, table: h.table || '—', range: h.range || '—', peers: h.leader ? [h.leader] : [], leader: h.leader || '', hot: h.hot}); });
}

function liveKpi() {
  const o = LIVE.snap.overview, num = v => (v || v === 0) ? Math.round(v).toLocaleString('en-US') : '—';
  const pct = (a, b) => b > 0 ? Math.round(a / b * 100) : 0;
  return [
    ['QPS', o.qps ? num(o.qps) : '—', `${o.qpsChangePct >= 0 ? '▲' : '▼'} ${Math.abs(o.qpsChangePct)}% 较窗口起点`, o.qpsChangePct > 10 ? 'up' : 'flat'],
    ['P99 延迟', o.p99ms ? `${o.p99ms} ms` : '—', `${o.p99ChangeMs >= 0 ? '▲' : '▼'} ${Math.abs(o.p99ChangeMs)}ms 较 1 小时前`, o.p99ChangeMs > 5 ? 'up' : 'flat'],
    ['连接数', o.connections ? num(o.connections) : '—', `Region ${num(o.regions)} · Store ${o.storesUp}/${o.storesTotal} Up`],
    ['存储使用', o.storageCapGB ? `${(o.storageUsedGB / 1024).toFixed(1)} TiB / ${(o.storageCapGB / 1024).toFixed(1)} TiB` : '—', `${pct(o.storageUsedGB, o.storageCapGB)}% · TiKV Store 合计`, 'flat'],
  ];
}

async function liveFetch(refresh) {
  const r = await fetch('/api/snapshot' + (refresh ? '?refresh=1' : ''), {cache: 'no-store'});
  if (r.status === 401 && typeof OP !== 'undefined' && OP.on) { opShowLogin('会话已过期，请重新登录'); throw new Error('未登录'); }
  if (!r.ok) throw new Error('HTTP ' + r.status);
  return r.json();
}

/* 当前集群的写操作是否已开启（两名管理员已确认、没有别的总闸挡着）：与右上角「写操作已开启 / 只读」标签用同一个判据（OP.write） */
const liveRW = () => LIVE.mode !== 'replay' && typeof OP !== 'undefined' && !!OP.on && !!OP.write && !!OP.write.enabled && !OP.write.reason;

function liveBanner() {
  if (window.CL && CL.empty()) {   // 独立部署且尚未接入集群：平台实例没有数据，不显示「真实数据」横幅
    const old = document.getElementById('liveBanner'); if (old) old.remove();
    const pill = document.querySelector('.pill.mock'); if (pill) pill.textContent = CL.standalone ? (CL.localTiup ? '中控机 · 尚未导入集群' : '独立部署 · 尚未接入集群') : '没有可见集群';
    CL.renderEmpty();
    return;
  }
  let b = document.getElementById('liveBanner');
  if (!b) { b = document.createElement('div'); b.id = 'liveBanner'; const top = document.querySelector('.top'); top.insertAdjacentElement('afterend', b); }
  const s = LIVE.snap, errs = s.errors || [], t = new Date(s.collectedAt).toLocaleTimeString();
  b.innerHTML = `<div class="notice ${errs.length ? 'warn' : 'info'}" style="margin:0;border-radius:0;align-items:center;flex-wrap:wrap">
    <span>${LIVE.mode === 'replay' ? '⏪' : '🟢'}</span>
    <div style="flex:1;min-width:260px"><b>${LIVE.mode === 'replay' ? '回放数据' : (liveRW() ? '真实数据（读写）' : '真实数据（只读）')}</b> · 集群 ${esc(s.cluster || '—')} · 采集于 ${t}${errs.length ? ` · <a data-na="live-errs">${errs.length} 项数据缺失 ▸</a>` : ''}
      <span class="small"> ｜ ${liveRW() ? '写操作已开启：扩缩容 / 配置变更等经审批后由受限执行器作用于集群' : '扩缩容 / 配置变更 / 主机维护等写操作仍是原型模拟，不会作用于集群'}</span></div>
    <button class="btn sm" data-na="live-refresh">立即刷新</button></div>
    <div id="liveErrs" style="display:none;padding:8px 24px;background:var(--warn-soft);color:var(--warn);font-size:12px">${errs.map(e => '· ' + esc(e)).join('<br>')}</div>`;
  const csel = document.querySelector('.top select'); if (csel && !(window.CL && CL.active())) csel.innerHTML = `<option>${esc(CLUSTER)}（${esc(VERSION)}）</option>`;   // 集群选择：只有当前这一个真实集群
  const pill = document.querySelector('.pill.mock'); if (pill) pill.textContent = LIVE.mode === 'replay' ? '回放数据' : (liveRW() ? '真实数据 · 读写' : '真实数据 · 只读');
}

/* 独立部署且尚未接入集群：平台实例没有集群数据。把原型的示例数据清空（避免被当成真实数据），页面由 CL.gate 换成引导页 */
function liveApplyEmpty() {
  liveApply({trends: {}, topology: {cluster: '', version: '', instances: []}, overview: {}, slow: [], alerts: [], findings: [], hot: [], env: [], errors: [], collectedAt: new Date().toISOString()});
}

async function liveRefresh(manual) {
  if (window.CL && CL.empty()) { if (manual && typeof toast === 'function') toast('还没有接入集群'); return; }
  try {
    liveApply(await liveFetch(!!manual)); liveBanner();
    if (typeof cfgLoad === 'function' && typeof CFGV !== 'undefined' && Date.now() - CFGV.at > 300000) cfgLoad(false);   // 配置当前值最多 5 分钟刷新一次
    const h = location.hash.slice(1) || 'overview', open = document.getElementById('drawer') && document.getElementById('drawer').classList.contains('open');
    const typing = document.activeElement && ['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName);
    if (['overview', 'arch', 'sql', 'hotspot', 'diag', 'settings'].includes(h) && (manual || (!open && !typing))) render(h);
    if (manual) toast('已刷新');
  } catch (e) { if (manual) toast('刷新失败：' + e.message); }
}

async function liveInit() {
  try {
    const r = await fetch('/api/health', {cache: 'no-store'});
    if (!r.ok) return;
    const h = await r.json(); if (!h || !h.ok) return;
    LIVE.health = h; LIVE.mode = h.mode;
    if (window.CL && CL.empty()) liveApplyEmpty(); else liveApply(await liveFetch(false));
    LIVE.on = true; liveBanner();
    if (typeof cfgLoad === 'function') cfgLoad(false);   // 后台预取配置当前值（pdThreshMin 等需要）；不阻塞首屏
    setInterval(() => liveRefresh(false), 30000);
  } catch (e) { /* 没有后端：保持原型的模拟数据 */ }
}

document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el) return;
  if (el.dataset.na === 'live-refresh') liveRefresh(true);
  if (el.dataset.na === 'live-errs') { const x = document.getElementById('liveErrs'); x.style.display = x.style.display === 'none' ? 'block' : 'none'; }
});
