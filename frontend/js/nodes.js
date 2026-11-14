// @author: kevinkelly
// @license: Apache License 2.0
// @file: nodes.js
// @time: 2026/10/28

/* 节点管理：主机池 / 扩容与新增节点 / 缩容与下线 / 单实例操作 / 任务中心（全部为前端模拟，状态存在内存里） */
const nodesState = {
  tab: 'inst', roleFilter: 'all', q: '',
  so: {step: 1, role: null, hosts: [], cfg: {}, ack: false, addOpen: false},
  si: {step: 1, picks: [], filter: 'all', confirm: ''},
  hostAdd: {ip: '', az: 'az1', rack: '', use: 'tikv'},
};
const TASKS = [];
let taskSeq = 1, sidSeq = 4001, pending = null;
const openCmds = new Set();
const MAINT_STATES = ['draining', 'active', 'recovering'];
const OP_LIVE_ST = ['pending', 'approved', 'scheduled', 'running'];   // 占用资源的服务端变更状态（运维模式）
/* 运维模式下：主机是否在维护取自服务端的 OP.maints；节点是否被占用取自服务端 live 变更的 plan.targets */
const inMaint = ip => OP.on ? OP.maints.some(m => m.ip === ip && MAINT_STATES.includes(m.state)) : typeof MAINTS !== 'undefined' && MAINTS.some(m => m.ip === ip && MAINT_STATES.includes(m.state));
const ROLE_IDS = ['tidb', 'tikv', 'pd', 'tiflash', 'ticdc'];
/* pd-ctl 前缀：真实数据模式取拓扑里第一个 PD 的地址与真实版本；静态模式用原型的演示地址 */
const PDCTL = () => {
  if (LIVE.on && LIVE.snap) { const pd = (LIVE.snap.topology.instances || []).find(i => i.type === 'pd'); return `tiup ctl:${VERSION} pd -u http://${pd ? pd.host + ':' + pd.port : '<pd-host>:2379'}`; }
  return `tiup ctl:${VERSION} pd -u http://10.0.1.21:2379`;
};
/* 可用区列表：真实模式取实际拓扑里出现的 zone，静态模式为原型的 az1~az3 */
const azList = () => { if (!LIVE.on) return ['az1', 'az2', 'az3']; const z = [...new Set(NODES.map(n => n.az).filter(a => a && a !== '—'))].sort(); return z.length ? z : ['az1']; };
const replicaCnt = () => (LIVE.on && LIVE.snap && LIVE.snap.topology.replicas) || 3;
const nowStr = () => new Date().toTimeString().slice(0, 8);
const nodeBy = id => NODES.find(n => n.id === id);
const tag = (t, c = '') => `<span class="tag ${c}">${t}</span>`;
const busyBy = id => OP.changes.find(c => OP_LIVE_ST.includes(c.state) && ((c.plan && c.plan.targets) || []).includes(id));
const busy = id => OP.on ? !!busyBy(id) : TASKS.some(t => t.state === 'running' && t.targets.includes(id));
/* 运维模式的权限判定：无权限的按钮置灰并说明 */
const OP_CAP_LABEL = {'inst.op': '实例操作', scale: '扩缩容', config: '配置变更', maint: '主机维护'};
const opDenied = cap => OP.on && !opCan(cap);
const opDeniedTitle = cap => opDenied(cap) ? OP_DENY_ATTR : '';   // 置灰但可点击，悬停 / 点击提示「权限不足」（见 ops.js）
const opDeniedAttr = cap => opDenied(cap) ? OP_DENY_ATTR : '';
const sumBy = (list, k) => list.reduce((a, n) => a + (n[k] || 0), 0);
HOST_POOL.forEach(h => { if (h.checked) h.check = genCheck(h); });

/* ================== 主机与预检 ================== */
/* 真实数据 / 运维模式（REAL）：集群内主机规格取自 GET /api/hosts；候选主机与预检结果取自 GET /api/hostpool（仅运维模式）。
   不再使用 data.js 里的模拟 SPEC / HOST_POOL / CHECK_ITEMS；静态原型模式保持原样（genCheck / runCheck / applyFix 仍是模拟）。 */
const REAL = () => !!(LIVE.on || OP.on);
const HP = {list: [], inCluster: [], loaded: false, loading: false, again: false, err: '', at: 0, checking: {}, notice: null, form: null, askRole: 'tikv'};
const HI = {byIp: {}, note: '', loaded: false, loading: false, again: false, err: '', at: 0};
const HP_CHECK_FRESH = 5 * 60 * 1000;   // 与后端 hostCheckFresh 一致：扩容规划只参考 5 分钟内的预检结果
const CHK_MAP = {ok: 'pass', warn: 'warn', bad: 'fail'};   // 后端 ok|warn|bad → 前端 pass|warn|fail
const poolAll = () => REAL() ? (OP.on ? HP.list : []) : HOST_POOL;
const hostByIp = ip => poolAll().find(h => h.ip === ip);
const hpInCluster = ip => HP.inCluster.includes(ip) || NODES.some(n => n.host === ip);
const liveAzs = () => [...new Set(NODES.map(n => n.az).filter(a => a && a !== '—'))].sort();
const hpRerender = () => { if (location.hash === '#nodes' && pages.nodes.body && ['hosts', 'scaleout'].includes(nodesState.tab)) pages.nodes.body(true); };

function hpApply(d) {
  const old = new Map(HP.list.map(h => [h.ip, h]));   // 按 IP 复用对象：扩容向导里已勾选的主机在刷新后仍是同一个引用
  HP.list = (d.hosts || []).map(r => {
    const h = old.get(r.ip) || {ip: r.ip};
    const lc = r.lastCheck || null;
    Object.assign(h, {az: r.az || '', rack: r.rack || '', note: r.note || '', createdBy: r.createdBy || '', createdAt: r.createdAt || '', lastCheck: lc,
      check: lc ? (lc.items || []).map(i => ({k: i.n, l: i.l || i.n, r: CHK_MAP[i.r] || 'warn', d: i.d || '', fix: i.fix || ''})) : null,
      checkRole: lc ? lc.role : '', checkAt: lc ? lc.at : '', checkOk: lc ? !!lc.ok : false});
    h.checking = !!HP.checking[r.ip];
    return h;
  });
  HP.inCluster = d.inCluster || [];
  nodesState.so.hosts = nodesState.so.hosts.filter(h => HP.list.includes(h));
}
async function hpLoad() {
  if (!OP.on || !OP.me) return;
  if (HP.loading) { HP.again = true; return; }   // 读取进行中又有新的请求：读完后再读一次，保证拿到最新结果
  HP.loading = true;
  try { hpApply(await liveGet('/api/hostpool')); HP.err = ''; HP.loaded = true; } catch (e) { HP.err = e.message || '读取失败'; }
  HP.loading = false; HP.at = Date.now();
  if (HP.again) { HP.again = false; return hpLoad(); }
  hpRerender();
}
async function hiLoad(refresh) {
  if (HI.loading) return;
  HI.loading = true;
  try {
    const d = await liveGet('/api/hosts' + (refresh ? '?refresh=1' : ''));
    HI.byIp = {}; (d.hosts || []).forEach(x => { HI.byIp[x.ip] = x; }); HI.note = d.note || ''; HI.err = ''; HI.loaded = true;
  } catch (e) { HI.err = e.message || '读取失败'; }
  HI.loading = false; HI.at = Date.now(); hpRerender();
}
/* 进入主机池 / 扩容页时按需读取（30 秒内不重复读取；失败也记时间，避免循环重试） */
function hostsEnsure() {
  if (!REAL()) return;
  if (!HI.loading && (!HI.at || Date.now() - HI.at > 30000)) hiLoad(false);
  if (OP.on && OP.me && !HP.loading && (!HP.at || Date.now() - HP.at > 30000)) hpLoad();
}
const hpNoticeHtml = () => HP.notice ? `<div class="notice ${HP.notice.lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[HP.notice.lv] || 'ℹ'}</span><div style="flex:1">${esc(HP.notice.t)}</div><a data-na="hp-dismiss">知道了</a></div>` : '';
const hpLoadHint = () => HP.err ? `<div class="notice bad"><span>✖</span><div>读取候选主机池失败：${esc(HP.err)}。<a data-na="hp-refresh">重试</a></div></div>` : !HP.loaded ? '<div class="small muted" style="margin:6px 0">正在读取候选主机池…</div>' : '';

function genCheck(h) {
  return CHECK_ITEMS.map(([k, l]) => { const p = h.preset && h.preset[k]; return p ? {k, l, r: p[0], d: p[1], fix: p[2]} : {k, l, r: 'pass', d: '正常', fix: false}; });
}
function checkSummary(h) {
  if (h.checking) return {t: REAL() ? '正在预检…' : '预检中…', c: 'info'};
  if (!h.check) return {t: '未预检', c: ''};
  const f = h.check.filter(i => i.r === 'fail').length, w = h.check.filter(i => i.r === 'warn').length;
  return f ? {t: `${f} 项失败`, c: 'bad'} : w ? {t: `通过（${w} 项警告）`, c: 'warn'} : {t: '预检通过', c: 'ok'};
}
/* 真实模式：预检只在检查进行中阻止继续；未预检 / 失败只给警示，由服务端规划再次提示（提交抽屉里可见） */
const hostBlocked = h => REAL() ? !!h.checking : (!h.check || h.checking || h.check.some(i => i.r === 'fail'));
const hasFixable = h => !!h.check && h.check.some(i => i.r !== 'pass' && i.fix);
const checkStale = h => !!h.checkAt && opNow() - new Date(h.checkAt).getTime() > HP_CHECK_FRESH;
const fixCmds = h => [...new Set((h.check || []).filter(i => i.r !== 'pass' && i.fix).map(i => i.fix))].join('\n');
/* 真实模式下对一台候选主机的警示文案（空串=无警示） */
function hostWarn(h, role) {
  if (h.checking) return '正在预检';
  if (!h.check) return '尚未预检';
  const f = h.check.filter(i => i.r === 'fail').length;
  if (f) return `最近一次预检有 ${f} 项失败`;
  if (role && h.checkRole && h.checkRole !== role) return `预检是按 ${(TYPE_META[h.checkRole] || {}).label || h.checkRole} 角色做的，与本次扩容角色 ${TYPE_META[role].label} 不同，端口 / 目录检查项可能不适用`;
  if (checkStale(h)) return '预检结果已超过 5 分钟（服务端规划不再参考）';
  return '';
}
function runCheck(h) { h.checking = true; refresh(true); setTimeout(() => { h.checking = false; h.check = genCheck(h); toast(`${h.ip} 预检完成`); refresh(true); }, 1100); }
function applyFix(h) {
  h.checking = true; refresh(true);
  setTimeout(() => { Object.keys(h.preset || {}).forEach(k => { if (h.preset[k][2]) delete h.preset[k]; }); h.checking = false; h.check = genCheck(h); toast('已执行 tiup cluster check --apply'); refresh(true); }, 1300);
}
/* 集群内主机：真实模式的规格取自 /api/hosts（取不到 known=false，显示「未采集」），静态模式用原型的 SPEC */
function clusterHosts() {
  const m = {}, real = REAL();
  NODES.forEach(n => {
    if (!m[n.host]) {
      const info = HI.byIp[n.host] || null;
      m[n.host] = {ip: n.host, az: n.az !== '—' || !info || !info.az ? n.az : info.az, rack: (n.labels && n.labels.rack !== '—' && n.labels.rack) || (info && info.rack) || '—', insts: [],
        ...(real ? {cores: info ? info.cores : 0, mem: info ? info.memGB : 0, disk: info ? info.diskGB : 0, os: info ? info.os : '', kernel: info ? info.kernel : '', known: !!(info && info.known)} : {...SPEC[n.type], os: 'Rocky Linux 9.3'})};
    }
    m[n.host].insts.push(n.id);
  });
  return Object.values(m);
}
function releaseHost(n) {
  if (REAL()) return;   // 真实 / 运维模式：缩容由后端执行，主机是否回到候选池由用户在主机池页手动添加
  if (NODES.some(x => x.host === n.host)) return;
  const h = {ip: n.host, az: n.az, rack: (n.labels && n.labels.rack) || '—', ...SPEC[n.type], os: 'Rocky Linux 9.3', note: `由 ${n.id} 下线释放，可复用或回收`, checked: true, preset: {}};
  h.check = genCheck(h); HOST_POOL.push(h);
}
function removeNode(n) { const i = NODES.indexOf(n); if (i >= 0) NODES.splice(i, 1); releaseHost(n); }

/* ---- 运维模式：候选主机池操作（新增 / 编辑 / 删除 / 预检 / 查看检查项） ---- */
const ipv4Err = s => /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/.test(s) ? '' : 'IP 格式不正确（需 IPv4，如 10.0.2.20，不带前导零）';
const fmtGB = g => g ? (g >= 1024 ? (g / 1024).toFixed(1) + ' TiB' : Math.round(g) + ' GB') : '—（未采集）';
const hpErrMsg = r => (r.data && r.data.error) || (r.code ? `操作失败（HTTP ${r.code}）` : '无法连接后端');

function hpFormOpen(mode, ip) {
  if (opDenied('scale')) { toast('权限不足'); return; }
  const azs = liveAzs(), h = mode === 'edit' ? hostByIp(ip) : null;
  if (mode === 'edit' && !h) return;
  // 下拉与内部状态同源：新增时默认取真实拓扑里的第一个 az（没有则切到手填）；编辑时当前值在列表里就选中，否则切到手填
  const azSel = h ? (h.az && azs.includes(h.az) ? h.az : '__custom') : (azs[0] || '__custom');
  HP.form = {mode, ip: h ? h.ip : '', azSel, azCustom: h && azSel === '__custom' ? h.az : '', rack: h ? h.rack : '', note: h ? h.note : '', busy: false};
  hpFormRender();
}
function hpFormRender() {
  const f = HP.form, azs = liveAzs(), edit = f.mode === 'edit';
  openDrawer(edit ? `编辑候选主机 ${f.ip}` : '新增候选主机', `
    <div class="small muted" style="margin-bottom:12px">候选主机是尚未加入集群的机器。加入后请先「预检」，再用于扩容。可用区 / 机架会写入新 TiKV 的拓扑标签（zone / rack）。</div>
    <div class="field"><label>主机 IP（IPv4）</label><input type="text" data-ni="hp-ip" value="${esc(f.ip)}" placeholder="如 10.0.2.20" ${edit ? 'disabled' : ''}></div>
    <div class="field"><label>可用区 (zone)</label><select data-nc="hp-az">${azs.map(z => `<option value="${esc(z)}" ${f.azSel === z ? 'selected' : ''}>${esc(z)}</option>`).join('')}<option value="__custom" ${f.azSel === '__custom' ? 'selected' : ''}>其他（手填）…</option></select>
      <input type="text" id="hpAzC" data-ni="hp-azc" value="${esc(f.azCustom)}" placeholder="手填可用区，如 az4；留空表示暂不指定" style="margin-top:6px;${f.azSel === '__custom' ? '' : 'display:none'}"></div>
    <div class="field"><label>机架 (rack)</label><input type="text" data-ni="hp-rack" value="${esc(f.rack)}" placeholder="如 r15（可空）"></div>
    <div class="field"><label>备注</label><input type="text" data-ni="hp-note" value="${esc(f.note)}" placeholder="如：已采购，待加入（不超过 200 字）"></div>
    <div id="hpErr" class="small" style="color:var(--bad);min-height:18px"></div>
    <div class="sticky-actions"><button class="btn primary" id="hpFormGo" data-na="hp-form-do">${edit ? '保存' : '添加到候选主机池'}</button><button class="btn" data-close="1">取消</button></div>`);
}
async function hpFormSubmit() {
  const f = HP.form; if (!f || f.busy) return;
  const err = t => { const e = $('#hpErr'); if (e) e.textContent = t; };
  const az = f.azSel === '__custom' ? f.azCustom.trim() : f.azSel, rack = f.rack.trim(), note = f.note.trim();
  if (f.mode === 'add') {
    const ip = f.ip.trim(), e = ipv4Err(ip);
    if (e) return err(e);
    if (hpInCluster(ip)) return err(`${ip} 已在集群拓扑里，不能加入候选主机池`);
    if (HP.list.some(h => h.ip === ip)) return err(`${ip} 已在候选主机池里`);
    f.ip = ip;
  }
  if (!/^[A-Za-z0-9_.-]{0,32}$/.test(az)) return err('可用区只能包含字母、数字、_ . -，且不超过 32 个字符');
  if (!/^[A-Za-z0-9_.-]{0,32}$/.test(rack)) return err('机架只能包含字母、数字、_ . -，且不超过 32 个字符');
  if (note.length > 200) return err('备注不能超过 200 个字符');
  f.busy = true; const b = $('#hpFormGo'); if (b) b.disabled = true; err('');
  const r = f.mode === 'add' ? await opApi('POST', '/api/hostpool', {ip: f.ip, az, rack, note}) : await opApi('POST', '/api/hostpool/' + encodeURIComponent(f.ip), {az, rack, note});
  f.busy = false;
  if (r.code !== 200) { const b2 = $('#hpFormGo'); if (b2) b2.disabled = false; err(hpErrMsg(r)); if (r.code === 409) await hpLoad(); return; }
  HP.form = null; closeDrawer();
  toast(f.mode === 'add' ? `${f.ip} 已加入候选主机池` : `${f.ip} 已保存`);
  await hpLoad();
}
function hpDelDrawer(ip) {
  const h = hostByIp(ip); if (!h) return;
  openDrawer(`移出候选主机池 ${ip}`, `<div class="kv"><span>可用区 / 机架</span><span>${esc(h.az || '—')} / ${esc(h.rack || '—')}</span><span>备注</span><span>${esc(h.note || '—')}</span><span>最近预检</span><span>${h.check ? esc(checkSummary(h).t) + ' · ' + opFmtTime(h.checkAt) : '未预检'}</span></div>
    <div class="notice info" style="margin-top:12px"><span>ℹ</span><div>只是把它从候选主机池的清单里移除（含预检记录），不会对该主机做任何操作。之后可以重新添加。</div></div>
    <div id="hpErr" class="small" style="color:var(--bad);min-height:18px"></div>
    <div class="sticky-actions"><button class="btn danger" id="hpDelGo" data-na="hp-del-do" data-ip="${esc(ip)}">确认移出</button><button class="btn" data-close="1">取消</button></div>`);
}
async function hpDelete(ip, quiet) {
  const r = await opApi('POST', `/api/hostpool/${encodeURIComponent(ip)}/delete`, {});
  if (r.code === 200 || r.code === 404) { nodesState.so.hosts = nodesState.so.hosts.filter(h => h.ip !== ip); return true; }
  if (!quiet) { const e = $('#hpErr'); if (e) e.textContent = hpErrMsg(r); }
  return false;
}
/* 预检：选目标角色 → POST /check（最长约 2–3 分钟，期间按钮禁用、行内显示进度） */
function hpCheckAsk(ip) {
  if (opDenied('scale')) { toast('权限不足'); return; }
  const h = hostByIp(ip); if (!h || h.checking) return;
  HP.askRole = h.checkRole && ROLE_IDS.includes(h.checkRole) ? h.checkRole : roleGuess(h);
  openDrawer(`预检 ${ip}`, `<div class="small muted" style="margin-bottom:12px">预检由中控机对该主机运行只读的 <code>tiup cluster check</code>（不修改主机），最长约 2–3 分钟。请选择这台主机计划承担的角色——不同角色检查的端口与目录不同。</div>
    <div class="field"><label>目标角色</label><select data-nc="hp-role">${ROLE_IDS.map(r => `<option value="${r}" ${HP.askRole === r ? 'selected' : ''}>${TYPE_META[r].label}</option>`).join('')}</select></div>
    <div class="sticky-actions"><button class="btn primary" data-na="hp-check-do" data-ip="${esc(ip)}">开始预检</button><button class="btn" data-close="1">取消</button></div>`);
}
async function hpCheck(ip, role) {
  const h = hostByIp(ip); if (!h || HP.checking[ip]) return;
  HP.checking[ip] = role; h.checking = true; HP.notice = null; hpRerender();
  const r = await opApi('POST', `/api/hostpool/${encodeURIComponent(ip)}/check`, {role});
  delete HP.checking[ip];
  if (r.code === 200) {
    await hpLoad();
    const x = hostByIp(ip), f = x && x.check ? x.check.filter(i => i.r === 'fail').length : 0, w = x && x.check ? x.check.filter(i => i.r === 'warn').length : 0, p = x && x.check ? x.check.length - f - w : 0;
    HP.notice = {lv: f ? 'bad' : w ? 'warn' : 'ok', t: `${ip} 预检完成（${TYPE_META[role].label}）：${f} 项失败 / ${w} 项警告 / ${p} 项通过${f || w ? '。点「查看检查项」查看详情与修复命令' : ''}`};
    toast(`${ip} 预检完成`);
  } else {
    h.checking = false;
    const why = r.code === 503 ? '（服务端未配置执行器）' : r.code === 502 ? '（命令没有跑起来）' : r.code === 409 ? '（冲突）' : '';
    HP.notice = {lv: 'bad', t: `${ip} 预检失败${why}：${hpErrMsg(r)}`};
    toast(`${ip} 预检失败`);
    if (r.code === 409 || r.code === 404) await hpLoad();
  }
  hpRerender();
}
/* 检查项结果抽屉：失败 / 警告 / 通过 分组；平台不会自动修复主机，只给出修复命令（复制后在目标主机上以 root 执行） */
function hpViewDrawer(ip) {
  const h = hostByIp(ip); if (!h) return;
  const redo = `<button class="btn" data-na="host-check" data-ip="${esc(ip)}" ${h.checking ? 'disabled' : opDeniedAttr('scale')}>${h.check ? '重新预检' : '预检'}</button>`;
  if (!h.check) { openDrawer(`预检结果 · ${ip}`, `<div class="empty">${h.checking ? '正在预检（约 1–2 分钟）…' : '尚未预检。点击「预检」选择目标角色后运行。'}</div><div class="sticky-actions">${redo}<button class="btn" data-close="1">关闭</button></div>`); return; }
  const by = k => h.check.filter(i => i.r === k), fl = by('fail'), wn = by('warn'), ps = by('pass');
  const icon = {pass: '✔', warn: '⚠', fail: '✖'}, cls = {pass: 'ok', warn: 'warn', fail: 'bad'};
  const item = i => `<div class="notice ${cls[i.r]}"><span>${icon[i.r]}</span><div style="flex:1;min-width:0"><b>${esc(i.l)}</b> <span class="small muted mono">${esc(i.k)}</span><div class="small">${esc(i.d)}</div>
    ${i.fix ? `<pre class="code" style="margin:6px 0">${esc(i.fix)}</pre><button class="btn sm" data-na="fix-copy" data-cmd="${esc(i.fix)}">复制修复命令</button>` : i.r !== 'pass' ? '<div class="small muted" style="margin-top:4px">没有给出修复命令，需手动处理</div>' : ''}</div></div>`;
  const grp = (t, list) => list.length ? `<h3 style="font-size:14px;margin:14px 0 6px">${t}（${list.length}）</h3>${list.map(item).join('')}` : '';
  const fixAll = fixCmds(h), cs = checkSummary(h);
  openDrawer(`预检结果 · ${ip}`, `<div class="row">${tag(cs.t, cs.c)}<span class="small muted">目标角色 ${esc((TYPE_META[h.checkRole] || {}).label || h.checkRole)} · ${opFmtTime(h.checkAt)}${checkStale(h) ? ' · 已超过 5 分钟' : ''}</span></div>
    ${fixAll ? '<div class="notice info" style="margin-top:10px"><span>ℹ</span><div><b>平台不会自动修复主机。</b>请复制修复命令，在目标主机上以 root 执行，然后回到这里「重新预检」。</div></div>' : ''}
    ${grp('失败', fl)}${grp('警告', wn)}
    ${ps.length ? `<details class="fold" style="margin-top:14px" ${fl.length || wn.length ? '' : 'open'}><summary><span class="small">通过（${ps.length}）</span></summary><div class="body">${ps.map(item).join('')}</div></details>` : ''}
    <div class="sticky-actions">${redo}${fixAll ? `<button class="btn" data-na="fix-copy" data-cmd="${esc(fixAll)}">复制全部修复命令</button>` : ''}<button class="btn" data-close="1">关闭</button></div>`);
}

/* ================== 任务引擎 ================== */
function tlog(t, m) { t.log.push(`[${nowStr()}] ${m}`); }
function startTask(plan) {
  if (OP.on && plan.intent) { opIntentDrawer(plan.intent, plan.title); return {id: '-'}; }   // 运维模式：走服务端规划 / 审批 / 执行
  const {title, cmds, steps, targets = [], abortable = false, onAbort} = plan;
  const t = {id: 'T' + String(taskSeq++).padStart(4, '0'), title, cmds, targets, abortable, onAbort,
    steps: steps.map(s => ({ticks: 1, ms: 900, ...s, state: 'wait', pct: 0})), state: 'running', cur: 0, log: [], started: nowStr(), finished: null, iv: null};
  TASKS.unshift(t); tlog(t, `任务创建：${title}`); updateBadge(); runStep(t); return t;
}
function runStep(t) {
  if (t.state !== 'running') return;
  if (t.cur >= t.steps.length) { t.state = 'success'; t.finished = nowStr(); tlog(t, '✔ 任务完成'); toast(`${t.title}：已完成`); updateBadge(); refresh(); return; }
  const s = t.steps[t.cur]; s.state = 'run'; tlog(t, `▶ ${s.name}`);
  try { s.start && s.start(t); } catch (e) { console.error(e); }
  let k = 0; refresh();
  t.iv = setInterval(() => {
    if (t.state !== 'running') { clearInterval(t.iv); return; }
    k++; s.pct = k / s.ticks;
    try { s.fn && s.fn(s.pct, t); } catch (e) { console.error(e); }
    if (k >= s.ticks) {
      clearInterval(t.iv); s.state = 'done';
      try { s.end && s.end(t); } catch (e) { console.error(e); }
      if (s.log) tlog(t, '  ' + s.log);
      t.cur++; refresh(); runStep(t);
    } else refresh();
  }, Math.max(60, s.ms / s.ticks));
}
function abortTask(t) {
  if (t.state !== 'running') return;
  clearInterval(t.iv); t.state = 'aborted'; t.finished = nowStr(); tlog(t, '■ 已中止');
  t.steps[t.cur].state = 'wait'; if (t.onAbort) t.onAbort(t);
  updateBadge(); refresh(); toast('任务已中止');
}
function updateBadge() {
  const n = TASKS.filter(t => t.state === 'running').length, b = $('#taskBadge');
  if (b) { b.style.display = n ? '' : 'none'; b.textContent = n; }
}
function refresh(force) {
  const h = location.hash.slice(1);
  if (h === 'nodes' && pages.nodes.body && (force || ['inst', 'hosts', 'tasks'].includes(nodesState.tab))) pages.nodes.body();
  else if (h === 'nodes' && nodesState.tab === 'maint' && pages.nodes.maintLive) pages.nodes.maintLive();
  else if (h === 'arch' && pages.arch.draw) { pages.arch.draw(); }
}

/* ================== 扩容：创建节点与任务 ================== */
function mkNode(role, h, cfg) {
  const idx = Math.max(0, ...NODES.filter(n => n.type === role).map(n => +n.id.split('-')[1])) + 1;
  const n = {id: `${role}-${idx}`, type: role, host: h.ip, port: +cfg.port, az: h.az, status: 'joining', cpu: 3, mem: 8, disk: 3,
    labels: {zone: h.az, rack: h.rack || '—', host: h.ip.replace(/\./g, '-')}, note: '新加入节点，正在初始化'};
  if (role === 'tikv') Object.assign(n, {sid: sidSeq++, regions: 0, leaders: 0, wr: 0, rd: 0, raftCpu: 2});
  if (role === 'tidb') Object.assign(n, {qps: 0, conn: 0});
  if (role === 'pd') Object.assign(n, {leader: false});
  if (role === 'tiflash') Object.assign(n, {regions: 0});
  if (role === 'ticdc') Object.assign(n, {lag: '0s'});
  return n;
}
function genYaml(role, hosts, cfg) {
  const key = {tidb: 'tidb_servers', tikv: 'tikv_servers', pd: 'pd_servers', tiflash: 'tiflash_servers', ticdc: 'cdc_servers'}[role];
  const L = [`${key}:`];
  hosts.forEach(h => {
    L.push(`  - host: ${h.ip}`);
    if (role === 'pd') L.push(`    client_port: ${cfg.port}`, `    peer_port: 2380`);
    else if (role === 'tiflash') L.push(`    flash_service_port: ${cfg.port}`, `    tcp_port: 9000`, `    http_port: 8123`);
    else L.push(`    port: ${cfg.port}`);
    if (role === 'tikv') L.push(`    status_port: 20180`);
    if (role === 'tidb') L.push(`    status_port: 10080`);
    L.push(`    deploy_dir: /tidb-deploy/${role}-${cfg.port}`);
    if (role !== 'tidb') L.push(`    data_dir: /tidb-data/${role}-${cfg.port}`);
    if (role === 'tikv') { L.push(`    config:`, `      server.labels: { zone: "${h.az}", rack: "${h.rack || 'r00'}", host: "${h.ip.replace(/\./g, '-')}" }`); if (+cfg.cache > 0) L.push(`      storage.block-cache.capacity: "${cfg.cache}GB"`); }
  });
  return L.join('\n');
}
function soCmds(role, hosts, cfg) {
  const c = [`tiup cluster check ${CLUSTER} scale-out.yml --cluster`];
  if (role === 'tikv' && +cfg.rate !== 15) c.push(`${PDCTL()} store limit all ${cfg.rate}   # 调整 Region 迁移速度（个/分钟）`);
  c.push(`tiup cluster scale-out ${CLUSTER} scale-out.yml -y`, `tiup cluster display ${CLUSTER}`);
  if (role === 'tidb') c.push(`# 把 ${hosts.map(h => h.ip + ':' + cfg.port).join(', ')} 加入 ${cfg.lb === 'tiproxy' ? 'TiProxy' : 'HAProxy'} 后端并 reload`);
  if (role === 'tikv' && +cfg.rate !== 15) c.push(`${PDCTL()} store limit all 15   # 恢复默认速度`);
  return c;
}
function buildScaleOut(role, hosts, cfg) {
  const created = [], label = TYPE_META[role].label;
  const steps = [
    {name: '预检目标主机（tiup cluster check）', ms: 900, log: '预检通过'},
    {name: '分发组件包并初始化目录', ms: 1100, ticks: 3},
    {name: `启动 ${label} 实例`, ms: 900, start: () => hosts.forEach(h => { const n = mkNode(role, h, cfg); NODES.push(n); created.push(n); const i = HOST_POOL.indexOf(h); if (i >= 0) HOST_POOL.splice(i, 1); })},
  ];
  if (role === 'tikv') {
    let snap = [];
    steps.push(
      {name: '注册到 PD（Store 状态 Up）', ms: 700, start: () => created.forEach(n => n.note = '已注册到 PD，Region 均衡中')},
      {name: 'Region 均衡：PD 向新 Store 迁移副本', ms: 5200, ticks: 13,
        start: () => { snap = NODES.filter(n => n.type === 'tikv' && !created.includes(n)).map(n => ({n, r: n.regions, l: n.leaders, d: n.disk})); },
        fn: p => {
          const N = snap.length + created.length, avgR = sumBy(snap.map(s => ({v: s.r})), 'v') / N, avgL = sumBy(snap.map(s => ({v: s.l})), 'v') / N;
          const avgD = sumBy(snap.map(s => ({v: s.d})), 'v') / snap.length, f = snap.length / N;
          snap.forEach(s => { s.n.regions = Math.round(s.r + (avgR - s.r) * p); s.n.leaders = Math.round(s.l + (avgL - s.l) * p); s.n.disk = Math.round(s.d * (1 - (1 - f) * p)); });
          created.forEach(n => { n.regions = Math.round(avgR * p); n.leaders = Math.round(avgL * p); n.disk = Math.max(3, Math.round(avgD * f * p)); n.cpu = Math.round(10 + 34 * p); n.mem = Math.round(10 + 50 * p); n.rd = Math.round(55 * p); n.wr = Math.round(36 * p); n.raftCpu = Math.round(40 * p); });
        }, log: 'Region 分布已趋于均衡'},
      {name: '健康检查与监控接入', ms: 700, start: () => created.forEach(n => { n.status = 'up'; n.note = ''; })});
  } else if (role === 'tidb') {
    let snap = [];
    steps.push(
      {name: `更新负载均衡后端并 reload（${cfg.lb === 'tiproxy' ? 'TiProxy' : 'HAProxy'}）`, ms: 1500, ticks: 6,
        start: () => { snap = NODES.filter(n => n.type === 'tidb' && !created.includes(n)).map(n => ({n, q: n.qps, c: n.conn, u: n.cpu})); },
        fn: p => {
          const N = snap.length + created.length, avgQ = sumBy(snap.map(s => ({v: s.q})), 'v') / N, avgC = sumBy(snap.map(s => ({v: s.c})), 'v') / N, f = snap.length / N;
          snap.forEach(s => { s.n.qps = Math.round(s.q + (avgQ - s.q) * p); s.n.conn = Math.round(s.c + (avgC - s.c) * p); s.n.cpu = Math.round(s.u * (1 - (1 - f) * p)); });
          const avgU = sumBy(snap.map(s => ({v: s.u})), 'v') / snap.length;
          created.forEach(n => { n.qps = Math.round(avgQ * p); n.conn = Math.round(avgC * p); n.cpu = Math.round(avgU * f * p + 3); n.mem = Math.round(20 + 32 * p); });
        }, log: '流量已均衡到新节点'},
      {name: '健康检查', ms: 600, start: () => created.forEach(n => { n.status = 'up'; n.note = ''; })});
  } else if (role === 'pd') {
    steps.push(
      {name: '加入 PD 集群（member add）', ms: 1200},
      {name: '同步 Raft 日志与元信息', ms: 1500, ticks: 5},
      {name: '健康检查', ms: 600, start: () => created.forEach(n => { n.status = 'up'; n.note = ''; n.cpu = 4; n.mem = 15; })});
  } else if (role === 'tiflash') {
    steps.push(
      {name: '注册 TiFlash 并以 Learner 身份拉取副本数据', ms: 4200, ticks: 10, fn: p => created.forEach(n => { n.regions = Math.round(24100 * p); n.disk = Math.round(40 * p) + 3; n.cpu = Math.round(30 * p); n.mem = Math.round(50 * p); })},
      {name: '健康检查', ms: 600, start: () => created.forEach(n => { n.status = 'up'; n.note = ''; })});
  } else {
    steps.push(
      {name: '加入 CDC 集群并重新均衡 changefeed', ms: 1500, ticks: 3},
      {name: '健康检查', ms: 600, start: () => created.forEach(n => { n.status = 'up'; n.note = ''; n.cpu = 15; n.mem = 28; })});
  }
  return {title: `扩容 ${label} ×${hosts.length}（${hosts.map(h => h.ip).join('、')}）`, cmds: soCmds(role, hosts, cfg), steps, targets: [], abortable: false,
    intent: {kind: 'scale.out', params: {role, hosts: hosts.map(h => ({ip: h.ip, az: h.az, rack: h.rack && h.rack !== '—' ? h.rack : ''})), port: +cfg.port, cache: +cfg.cache, rate: +cfg.rate, lb: cfg.lb}}};
}

/* ================== 缩容评估与任务 ================== */
function evalScaleIn(ids) {
  const picks = ids.map(nodeBy).filter(Boolean), items = [], proj = [];
  const add = (lv, t) => items.push({lv, t});
  const rem = NODES.filter(n => !ids.includes(n.id));
  if (!picks.length) add('bad', '尚未选择要下线的节点');
  picks.forEach(n => {
    if (n.status === 'joining' || n.status === 'offline') add('bad', `${n.id} 正处于「${stText[n.status]}」，请等待当前变更完成`);
    if (busy(n.id)) add('bad', `${n.id} 有进行中的任务，请先等待或中止`);
  });
  [...new Set(picks.map(n => n.type))].forEach(role => {
    const ps = picks.filter(n => n.type === role), left = rem.filter(n => n.type === role), label = TYPE_META[role].label;
    if (role === 'tikv') {
      const all = NODES.filter(n => n.type === 'tikv');
      if (left.length < replicaCnt()) add('bad', `剩余 TiKV 仅 ${left.length} 个，小于 max-replicas=${replicaCnt()}，${replicaCnt()} 副本无法满足，Region 将长期缺副本`);
      else add('ok', `剩余 ${left.length} 个 TiKV，仍满足 ${replicaCnt()} 副本（max-replicas=${replicaCnt()}）`);
      // zone 打散只在 TiKV 带了 zone 标签时才有意义（「—」/ 空不算）；只有下线让可用区数从够用降到不够用才阻断，本来就不足的只提示（与服务端一致）
      const zoneSet = ns => new Set(ns.map(n => n.az).filter(a => a && a !== '—')), before = zoneSet(all), after = zoneSet(left), lost = [...before].filter(z => !after.has(z)).sort();
      if (lost.length) {
        const rc = replicaCnt();
        if (after.size >= rc) add('ok', `${lost.join('、')} 将没有 TiKV，但剩余 ${after.size} 个可用区仍满足 ${rc} 副本按 zone 打散`);
        else if (before.size >= rc) add('bad', `${lost.join('、')} 将没有 TiKV，可用区数由 ${before.size} 降到 ${after.size}，小于 max-replicas=${rc}，副本无法再按 zone 打散（location-labels: zone）`);
        else add('warn', `${lost.join('、')} 将没有 TiKV：集群本来只有 ${before.size} 个带标签的可用区（小于 max-replicas=${rc}），副本本就不能完全按 zone 打散，这次下线不改变这一点`);
      }
      if (ps.length > 1) add('warn', '一次下线多个 TiKV：Region 迁移量叠加，建议逐个下线，待 Tombstone 后再继续');
      const moveD = sumBy(ps, 'disk'), move = sumBy(ps, 'regions');
      left.forEach(n => proj.push({id: n.id, k: '磁盘', a: n.disk, b: Math.round(n.disk + moveD / left.length), warn: 70, bad: 80, u: '%'}));
      const worst = Math.max(0, ...left.map(n => n.disk + moveD / left.length));
      if (left.length) {
        if (worst >= 80) add('bad', `迁入数据后剩余 TiKV 磁盘最高约 ${Math.round(worst)}%，超过 80% 水位，PD 将限制向其调度，存在写满风险；请先扩容`);
        else if (worst >= 70) add('warn', `迁入数据后剩余 TiKV 磁盘最高约 ${Math.round(worst)}%，余量偏小`);
        else add('ok', `迁入数据后剩余 TiKV 磁盘最高约 ${Math.round(worst)}%，容量充足`);
        add('info', `需迁移约 ${fmt(move)} 个 Region（≈ ${(move * 96 / 1048576).toFixed(2)} TiB）；按默认 store limit 15/分钟、${left.length} 个目标 Store 估算，约 ${(move / (15 * left.length) / 60).toFixed(1)} 小时`);
      }
      const hot = ps.filter(n => n.wr > 100);
      if (hot.length) add('info', `${hot.map(n => n.id).join('、')} 是写热点节点，下线后热点 Region 会迁到其他 Store，热点问题依旧存在`);
    } else if (role === 'pd') {
      if (left.length < 3) add('bad', `缩容后 PD 仅剩 ${left.length} 个，无法容忍任何节点故障（3 副本 Raft 至少需 2 个存活，推荐保持 3 或 5）`);
      else if (left.length % 2 === 0) add('warn', `缩容后 PD 为 ${left.length} 个（偶数），容错能力与 ${left.length - 1} 个相同，建议保持奇数`);
      else add('ok', `缩容后 PD 为 ${left.length} 个（奇数），仍可容忍 ${(left.length - 1) / 2} 个节点故障`);
      if (ps.some(n => n.leader)) add('warn', 'PD Leader 在下线范围内，将先执行 member leader transfer，TSO 会有秒级抖动');
    } else if (role === 'tidb') {
      if (!left.length) add('bad', '这是最后一个 TiDB，下线后集群将无法提供 SQL 服务');
      else {
        if (left.length === 1) add('warn', '缩容后只剩 1 个 TiDB，成为单点');
        const cpu = Math.round(sumBy(NODES.filter(n => n.type === 'tidb'), 'cpu') / left.length);
        left.forEach(n => proj.push({id: n.id, k: 'CPU', a: n.cpu, b: Math.round(n.cpu + sumBy(ps, 'cpu') / left.length), warn: 70, bad: 85, u: '%'}));
        add(cpu >= 85 ? 'bad' : cpu >= 70 ? 'warn' : 'ok', `缩容后剩余 TiDB 平均 CPU 约 ${cpu}%`);
        add('info', `约 ${sumBy(ps, 'conn')} 个连接会在摘除后断开并重连，请先从负载均衡摘除并等待排空`);
      }
    } else if (role === 'tiflash') {
      if (!left.length) add('bad', '这是最后一个 TiFlash，下线后依赖列存 / MPP 的查询将失败或回退到 TiKV');
      else if (left.length < 2) add('warn', '表的 TiFlash 副本数为 2，剩余节点不足；需先 ALTER TABLE ... SET TIFLASH REPLICA 1');
      else add('ok', `剩余 ${left.length} 个 TiFlash，可满足 2 副本`);
    } else {
      if (!left.length) add('bad', '这是最后一个 TiCDC，下线后所有 changefeed 将中断');
      else add('info', `${label} 的 changefeed 会迁移到剩余 ${left.length} 个 capture，期间同步延迟会短暂上升`);
    }
  });
  return {items, proj, blocked: items.some(i => i.lv === 'bad'), picks};
}
function siCmds(picks) {
  const cmds = [], tikvs = picks.filter(n => n.type === 'tikv');
  tikvs.forEach(n => cmds.push(`${PDCTL()} scheduler add evict-leader-scheduler ${n.sid}   # 先驱逐 ${n.id} 的 Leader`));
  cmds.push(`tiup cluster scale-in ${CLUSTER} -N ${picks.map(n => n.host + ':' + n.port).join(',')}`);
  if (tikvs.length) cmds.push(`${PDCTL()} store   # 观察 store 状态：Up → Offline → Tombstone`, `tiup cluster prune ${CLUSTER}   # Tombstone 后清理残留`);
  if (picks.some(n => n.type === 'tidb')) cmds.unshift('# 先把 TiDB 从负载均衡（HAProxy / TiProxy）后端摘除，并等待连接排空');
  cmds.push(`tiup cluster display ${CLUSTER}`);
  return cmds;
}
function buildScaleIn(picks) {
  const steps = [{name: '最终前置检查', ms: 700, log: '副本数 / 拓扑 / 容量检查通过'}];
  const del = (nodes) => ({name: '清理实例并更新拓扑', ms: 900, start: () => nodes.forEach(n => { n.status = 'offline'; }), end: () => nodes.forEach(removeNode)});
  ['tidb', 'tiflash', 'ticdc', 'pd', 'tikv'].forEach(role => {
    const ns = picks.filter(n => n.type === role); if (!ns.length) return;
    const names = ns.map(n => n.id).join('、');
    if (role === 'tikv') {
      let snap = [];
      steps.push(
        {name: `驱逐 Leader（evict-leader-scheduler）· ${names}`, ms: 1600, ticks: 4, start: () => { snap = ns.map(n => ({n, l: n.leaders})); }, fn: p => snap.forEach(s => { s.n.leaders = Math.round(s.l * (1 - p)); })},
        {name: 'tiup scale-in：Store 进入 Offline', ms: 800, start: () => ns.forEach(n => { n.status = 'offline'; n.note = 'Store Offline，Region 迁出中'; })},
        {name: 'Region 迁出：PD 将副本调度到其他 Store', ms: 6000, ticks: 15,
          start: () => { snap = ns.map(n => ({n, r: n.regions, d: n.disk})); },
          fn: p => {
            const left = NODES.filter(n => n.type === 'tikv' && !ns.includes(n)), moveR = sumBy(snap.map(s => ({v: s.r})), 'v'), moveD = sumBy(snap.map(s => ({v: s.d})), 'v');
            if (!snap.base) snap.base = left.map(n => ({n, r: n.regions, d: n.disk}));
            snap.forEach(s => { s.n.regions = Math.round(s.r * (1 - p)); s.n.disk = Math.round(s.d * (1 - p)); });
            snap.base.forEach(b => { b.n.regions = Math.round(b.r + moveR / snap.base.length * p); b.n.disk = Math.round(b.d + moveD / snap.base.length * p); });
          }, log: 'Region 已全部迁出'},
        {name: 'Store 变为 Tombstone', ms: 700},
        {...del(ns), name: '清理实例（tiup cluster prune）并更新拓扑'});
    } else if (role === 'tidb') {
      let snap = [];
      steps.push(
        {name: `从负载均衡摘除后端 · ${names}`, ms: 900},
        {name: '等待连接排空', ms: 2400, ticks: 6, start: () => { snap = ns.map(n => ({n, c: n.conn, q: n.qps})); },
          fn: p => {
            const left = NODES.filter(n => n.type === 'tidb' && !ns.includes(n));
            if (!snap.base) snap.base = left.map(n => ({n, c: n.conn, q: n.qps, u: n.cpu}));
            const mc = sumBy(snap.map(s => ({v: s.c})), 'v'), mq = sumBy(snap.map(s => ({v: s.q})), 'v'), mu = sumBy(ns, 'cpu');
            snap.forEach(s => { s.n.conn = Math.round(s.c * (1 - p)); s.n.qps = Math.round(s.q * (1 - p)); s.n.cpu = Math.round(s.n.cpu * (1 - p * .5)); });
            snap.base.forEach(b => { b.n.conn = Math.round(b.c + mc / snap.base.length * p); b.n.qps = Math.round(b.q + mq / snap.base.length * p); b.n.cpu = Math.round(b.u + mu / snap.base.length * p * .6); });
          }, log: '连接已排空'},
        del(ns));
    } else if (role === 'pd') {
      steps.push(
        {name: '迁移 PD Leader（member leader transfer）', ms: 900, end: () => { const rest = NODES.filter(n => n.type === 'pd' && !ns.includes(n)); if (ns.some(n => n.leader) && rest.length) { ns.forEach(n => n.leader = false); rest[0].leader = true; rest[0].note = 'PD Leader'; } }},
        {name: '从 PD 集群移除成员（member delete）', ms: 900}, del(ns));
    } else if (role === 'tiflash') {
      let snap = [];
      steps.push(
        {name: '迁出 TiFlash Learner 副本数据', ms: 3200, ticks: 8, start: () => { snap = ns.map(n => ({n, r: n.regions})); }, fn: p => snap.forEach(s => { s.n.regions = Math.round(s.r * (1 - p)); })},
        del(ns));
    } else {
      steps.push({name: '迁移 changefeed capture 到其他 TiCDC', ms: 1200}, del(ns));
    }
  });
  return {title: `缩容 ${picks.map(n => n.id).join('、')}`, cmds: siCmds(picks), steps, targets: picks.map(n => n.id), abortable: true,
    intent: {kind: 'scale.in', params: {nodes: picks.map(n => n.id)}},
    onAbort: t => { picks.forEach(n => { if (NODES.includes(n) && n.status === 'offline') { n.status = 'up'; n.note = ''; } }); tlog(t, '已把 Store 恢复为 Up（PD API: store state Up），已迁移的数据保留在新位置'); }};
}

/* ================== 单实例 / 主机维护计划 ================== */
function instPlan(n, act) {
  const p = instPlanSim(n, act);
  if (p) p.intent = {kind: 'inst.op', params: {node: n.id, action: act}};
  return p;
}
function instPlanSim(n, act) {
  const a = `${n.host}:${n.port}`, tikv = n.type === 'tikv', label = TYPE_META[n.type].label;
  const same = NODES.filter(x => x.type === n.type && x !== n && (x.status === 'up' || x.status === 'warn'));
  const checks = [];
  const evictCmd = tikv ? [`${PDCTL()} scheduler add evict-leader-scheduler ${n.sid}`] : [];
  const restoreCmd = tikv ? [`${PDCTL()} scheduler remove evict-leader-scheduler-${n.sid}`] : [];
  let l0 = n.leaders;
  const evictStep = {name: '驱逐 Leader', ms: 1200, ticks: 4, start: () => { l0 = n.leaders; }, fn: p => { n.leaders = Math.round(l0 * (1 - p)); }};
  const restoreStep = {name: '移除驱逐调度，Leader 回迁', ms: 1500, ticks: 4, fn: p => { n.leaders = Math.round(l0 * p); }};
  const stopStep = {name: '停止实例', ms: 900, end: () => { n.status = 'down'; n.note = '手动停止'; }};
  const startStep = {name: '启动实例', ms: 1200, end: () => { n.status = 'up'; n.note = ''; }};
  const health = {name: '健康检查', ms: 700, log: '实例健康'};
  if (act === 'restart') {
    if (!same.length) checks.push(['bad', `${label} 仅此一个健康实例，重启期间该组件不可用`]);
    if (tikv) checks.push(['ok', '重启前先驱逐 Leader，业务抖动通常为秒级'], same.length < 2 ? ['warn', '其他健康 TiKV 少于 2 个，重启期间多数派风险升高'] : ['ok', `其余 ${same.length} 个 TiKV 健康，Raft 多数派不受影响`]);
    if (n.type === 'pd' && n.leader) checks.push(['warn', 'PD Leader 重启会触发重新选举，TSO 短暂不可用']);
    if (n.type === 'tidb') checks.push(['warn', `该节点约 ${n.conn} 个连接会断开，需应用具备重连机制；建议先从负载均衡摘除`]);
    return {title: `重启 ${n.id}`, risk: 'mid', cmds: [...evictCmd, `tiup cluster restart ${CLUSTER} -N ${a}`, ...restoreCmd], checks,
      steps: tikv ? [evictStep, {...stopStep, name: '停止实例（重启）'}, startStep, health, restoreStep] : [{...stopStep, name: '停止实例（重启）'}, startStep, health], targets: [n.id]};
  }
  if (act === 'stop') {
    if (!same.length) checks.push(['bad', `${label} 仅此一个健康实例，停止后该组件整体不可用`]);
    if (tikv) { checks.push(['warn', 'PD 在 max-store-down-time（默认 30 分钟）后会把该 Store 判为 Down 并开始补副本，计划内停机请在此之前恢复']); if (same.length < 2) checks.push(['bad', '停止后健康 TiKV 少于 3 个，部分 Region 将失去多数派']); }
    if (n.type === 'pd' && same.length < 2) checks.push(['bad', '停止后 PD 失去多数派，集群将不可用']);
    if (n.type === 'tidb' && n.conn) checks.push(['warn', `约 ${n.conn} 个连接会立即中断`]);
    return {title: `停止 ${n.id}`, risk: 'high', cmds: [...evictCmd, `tiup cluster stop ${CLUSTER} -N ${a}`], checks, steps: tikv ? [evictStep, stopStep] : [stopStep], targets: [n.id]};
  }
  if (act === 'start') {
    checks.push(['ok', '启动后会自动重新加入集群；TiKV 将逐步恢复 Leader / Region 均衡']);
    return {title: `启动 ${n.id}`, risk: 'low', cmds: [`tiup cluster start ${CLUSTER} -N ${a}`, ...(n.evict ? restoreCmd : [])], checks,
      steps: [startStep, health, ...(tikv ? [{name: 'Leader 回迁', ms: 1200, ticks: 4, fn: p => { n.leaders = Math.round(5350 * p); }}] : [])], targets: [n.id]};
  }
  if (act === 'evict') {
    checks.push(['ok', 'Leader 会通过 TransferLeader 迁移到其他 Store，不涉及数据拷贝'], ['warn', `${n.leaders} 个 Leader 将迁到其余 TiKV，其读写压力随之转移，请确认剩余节点余量`]);
    return {title: `驱逐 ${n.id} 的 Leader`, risk: 'mid', cmds: evictCmd, checks, targets: [n.id],
      steps: [{name: '添加 evict-leader-scheduler', ms: 600}, {...evictStep, end: () => { n.evict = true; n.note = 'Leader 已驱逐（维护中）'; }}]};
  }
  if (act === 'unevict') {
    return {title: `恢复 ${n.id} 的 Leader 调度`, risk: 'low', cmds: restoreCmd, checks: [['ok', '移除驱逐调度后，PD 会逐步把 Leader 均衡回该 Store']], targets: [n.id],
      steps: [{name: '移除 evict-leader-scheduler', ms: 600}, {name: 'Leader 回迁', ms: 1500, ticks: 5, fn: p => { n.leaders = Math.round(5350 * p); }, end: () => { n.evict = false; n.note = ''; }}]};
  }
}
function planDrawer(plan, extra = '') {
  if (OP.on && plan.intent) { opIntentDrawer(plan.intent, plan.title); return; }   // 运维模式：服务端规划
  pending = plan;
  const risk = {low: ['ok', '低风险'], mid: ['warn', '中风险'], high: ['bad', '高风险']}[plan.risk];
  const blocked = plan.checks.some(c => c[0] === 'bad');
  openDrawer(plan.title, `
    <div class="row"><span class="tag ${risk[0]}">${risk[1]}</span>${extra}</div>
    <h3 style="font-size:14px;margin:14px 0 4px">将执行的命令</h3>${code(plan.cmds.join('\n'))}
    <h3 style="font-size:14px;margin:14px 0 4px">前置检查</h3>
    ${plan.checks.map(([lv, t]) => `<div class="notice ${lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖'}[lv]}</span><div>${t}</div></div>`).join('')}
    <h3 style="font-size:14px;margin:14px 0 4px">执行步骤</h3>
    <ol class="small">${plan.steps.map(s => `<li>${s.name}</li>`).join('')}</ol>
    ${plan.risk === 'high' && !blocked ? `<div class="field" style="margin-top:12px"><label>高风险操作，请输入「确认」继续</label><input type="text" data-ni="drawer-confirm" placeholder="确认"></div>` : ''}
    <div class="sticky-actions"><button class="btn ${plan.risk === 'high' ? 'danger' : 'primary'}" id="drRun" data-na="run-pending" ${blocked || plan.risk === 'high' ? 'disabled' : ''}>${blocked ? '存在阻断项' : '提交并执行（模拟）'}</button><button class="btn" data-close="1">取消</button></div>`);
}

/* ================== 页面 ================== */
const stepper = (list, cur) => `<div class="stepper">${list.map((t, i) => `<div class="st ${i + 1 === cur ? 'cur' : i + 1 < cur ? 'done' : ''}"><b>${i + 1 < cur ? '✓' : i + 1}</b>${t}</div>`).join('')}</div>`;
const poolList = () => REAL() ? poolAll().filter(h => !hpInCluster(h.ip)) : HOST_POOL;   // 真实 / 运维模式：候选主机来自 /api/hostpool，已在服务端拓扑里的主机不再算「未加入集群」
const roleGuess = h => /TiFlash/i.test(h.note || '') ? 'tiflash' : /TiDB/i.test(h.note || '') ? 'tidb' : 'tikv';
const azCount = (role, az) => NODES.filter(n => n.type === role && n.az === az).length;

const maintTabLabel = () => `主机维护${(OP.on ? OP.maints.some(m => MAINT_STATES.includes(m.state)) : typeof MAINTS !== 'undefined' && MAINTS.some(m => MAINT_STATES.includes(m.state))) ? ' · 进行中' : ''}`;
const taskTabLabel = () => {
  if (OP.on) { const run = OP.changes.filter(c => ['running', 'approved'].includes(c.state)).length, pend = OP.changes.filter(c => c.state === 'pending').length; return `任务中心${pend ? ` · ${pend} 待审批` : ''}${run ? ` · ${run} 运行中` : !pend && OP.changes.length ? ` · ${OP.changes.length}` : ''}`; }
  const r = TASKS.filter(x => x.state === 'running').length; return `任务中心${r ? ` · ${r} 运行中` : TASKS.length ? ` · ${TASKS.length}` : ''}`;
};

pages.nodes = {
  title: '节点管理',
  render() {
    return `<div class="tabs" id="ndTabs">${[['inst', '节点实例'], ['hosts', '主机池'], ['scaleout', '扩容 / 新增节点'], ['scalein', '缩容 / 下线'], ['maint', maintTabLabel()], ['tasks', taskTabLabel()]].map(([k, t]) => `<button data-na="tab" data-t="${k}" class="${nodesState.tab === k ? 'active' : ''}">${t}</button>`).join('')}</div><div id="ndBody"></div>`;
  },
  init() {
    const a = window.navArgs || {}; window.navArgs = null;
    if (a.tab) nodesState.tab = a.tab;
    if (a.tab === 'scaleout') { Object.assign(nodesState.so, {step: a.role ? 2 : 1, role: a.role || null, hosts: [], ack: false}); nodesState.so.cfg = {}; }
    if (a.tab === 'scalein' && a.node) { nodesState.si = {step: 2, picks: [a.node], filter: 'all', confirm: ''}; }
    if (a.tab === 'inst' && a.node) { nodesState.q = a.node; nodesState.roleFilter = 'all'; }
    $$('#ndTabs button').forEach(b => b.classList.toggle('active', b.dataset.t === nodesState.tab));
    this.body();
  },
  body(soft) {   // soft：由服务端数据轮询触发的刷新——内容没变就不重绘，并保留日志框的滚动位置
    const el = $('#ndBody'); if (!el) return;
    const keep = $('#ndQ') && document.activeElement === $('#ndQ');
    const t = nodesState.tab;
    if (REAL() && (t === 'hosts' || t === 'scaleout')) hostsEnsure();   // 真实 / 运维模式：按需读取主机规格与候选主机池
    const html = t === 'inst' ? this.inst() : t === 'hosts' ? this.hosts() : t === 'scaleout' ? this.scaleOut() : t === 'scalein' ? this.scaleIn() : t === 'maint' ? this.maint() : this.tasks();
    $$('#ndTabs button').forEach(b => { b.classList.toggle('active', b.dataset.t === t); if (b.dataset.t === 'maint') b.textContent = maintTabLabel(); if (b.dataset.t === 'tasks') b.textContent = taskTabLabel(); });
    if (soft && el.dataset.sig === t + '|' + html) return;
    const scr = {}; $$('#ndBody pre.log[data-lg]').forEach(p => { scr[p.dataset.lg] = p.scrollHeight - p.scrollTop - p.clientHeight < 8 ? -1 : p.scrollTop; });
    el.innerHTML = html; el.dataset.sig = t + '|' + html;
    if (keep) { const q = $('#ndQ'); q.focus(); q.setSelectionRange(q.value.length, q.value.length); }
    $$('#ndBody pre.log').forEach(p => { const k = p.dataset.lg; p.scrollTop = k in scr && scr[k] >= 0 ? scr[k] : p.scrollHeight; });
  },

  /* ---- 节点实例 ---- */
  inst() {
    const st = nodesState, list = NODES.filter(n => (st.roleFilter === 'all' || n.type === st.roleFilter) && (!st.q || (n.id + n.host).includes(st.q)));
    const cnt = r => NODES.filter(n => n.type === r).length;
    const cls = (v, w, b) => v >= b ? 'bad' : v >= w ? 'warn' : '';
    return `
    <div class="grid g4" style="margin-bottom:16px">${ROLE_IDS.slice(0, 4).map(r => `<div class="card kpi"><div class="l">${TYPE_META[r].label}</div><div class="v">${cnt(r)}</div><div class="d flat">${NODES.filter(n => n.type === r && n.status === 'up').length} 运行中 · ${NODES.filter(n => n.type === r && n.status !== 'up').length} 异常/变更中</div></div>`).join('')}</div>
    <div class="card"><h2>节点实例 <span class="sub">${list.length} / ${NODES.length}</span>
      <span class="right"><button class="btn primary sm" data-na="tab" data-t="scaleout">＋ 扩容 / 新增节点</button><button class="btn sm" data-na="tab" data-t="scalein">缩容 / 下线</button></span></h2>
      <div class="row" style="margin-bottom:10px"><div class="chips">${['all', ...ROLE_IDS].map(r => `<span class="chip ${st.roleFilter === r ? 'active' : ''}" data-na="rf" data-r="${r}">${r === 'all' ? '全部' : TYPE_META[r].label}</span>`).join('')}</div>
        <input type="search" id="ndQ" data-ni="q" placeholder="搜索实例名 / IP" value="${esc(st.q)}" style="min-width:200px"></div>
      <div class="tbl-wrap"><table><thead><tr><th>实例</th><th>地址</th><th>可用区</th><th>CPU / 内存 / 磁盘</th><th>负载</th><th>操作</th></tr></thead><tbody>
      ${list.map(n => {
        const down = n.status === 'down', trans = n.status === 'joining' || n.status === 'offline', b = busy(n.id) || trans || inMaint(n.host);
        const dis = opDenied('inst.op') ? opDeniedAttr('inst.op') : b ? `disabled${OP.on ? ` title="${inMaint(n.host) ? '所在主机正在维护' : trans ? '实例正处于变更中' : '有未结束的变更占用该实例'}"` : ''}` : '';
        const disScale = opDenied('scale') ? opDeniedAttr('scale') : dis;
        const disStart = OP.on && !opDenied('inst.op') && !busy(n.id) && !trans ? '' : dis;   // 运维模式：主机维护中服务端允许「启动」，按钮不因此置灰
        const bc = busyBy(n.id);
        const load = n.type === 'tikv' ? `Region ${fmt(n.regions)} · Leader ${fmt(n.leaders)}` : n.type === 'tidb' ? `QPS ${fmt(n.qps)} · 连接 ${n.conn}` : n.type === 'pd' ? (n.leader ? 'Leader' : 'Follower') : n.type === 'tiflash' ? `Learner ${fmt(n.regions)}` : `lag ${n.lag}`;
        return `<tr><td><b>${esc(n.id)}</b> ${tag(stText[n.status], stCls(n.status))}${n.evict ? ' ' + tag('Leader 已驱逐', 'info') : ''}${inMaint(n.host) ? ' ' + tag('主机维护中', 'warn') : ''}${OP.on ? (bc ? ` <a data-na="oc-goto" data-id="${esc(bc.id)}" title="${esc(bc.id + ' · ' + bc.title)}">${tag(esc('变更 ' + bc.id + '（' + (OP_STATE[bc.state] || [0, bc.state])[1] + '）'), 'info')}</a>` : '') : (busy(n.id) ? ' ' + tag('任务中', 'info') : '')}</td>
          <td class="mono small">${esc(n.host)}:${esc(n.port)}</td><td>${esc(n.az)}</td>
          <td class="small"><span class="${cls(n.cpu, 70, 85)}" style="color:var(--${cls(n.cpu, 70, 85) || 'text'})">${n.cpu}%</span> / <span style="color:var(--${cls(n.mem, 70, 85) || 'text'})">${n.mem}%</span> / <span style="color:var(--${cls(n.disk, 75, 85) || 'text'})">${n.disk}%</span></td>
          <td class="small">${load}</td>
          <td><div class="acts">
            ${down ? `<button class="btn sm" data-na="ia" data-act="start" data-id="${esc(n.id)}" ${disStart}>启动</button>` : `<button class="btn sm" data-na="ia" data-act="restart" data-id="${esc(n.id)}" ${dis}>重启</button><button class="btn sm" data-na="ia" data-act="stop" data-id="${esc(n.id)}" ${dis}>停止</button>`}
            ${n.type === 'tikv' && !down ? (n.evict || OP.on ? `<button class="btn sm" data-na="ia" data-act="unevict" data-id="${esc(n.id)}" ${dis}>恢复 Leader</button>` : '') + (!n.evict ? `<button class="btn sm" data-na="ia" data-act="evict" data-id="${esc(n.id)}" ${dis}>驱逐 Leader</button>` : '') : ''}
            <button class="btn sm danger" data-na="scalein-one" data-id="${esc(n.id)}" ${disScale}>缩容</button><button class="btn sm" data-na="nd-open" data-id="${esc(n.id)}" title="详情与配置变更">详情 / 配置</button><button class="btn sm" data-go="arch" data-node="${esc(n.id)}" title="在架构图中查看">拓扑</button></div></td></tr>`;
      }).join('') || '<tr><td colspan="6" class="empty">没有匹配的节点</td></tr>'}
      </tbody></table></div></div>`;
  },

  /* ---- 主机池（真实数据 / 运维模式）：集群内主机取自 /api/hosts，候选主机池取自 /api/hostpool ---- */
  hostsLive() {
    const ch = clusterHosts(), op = OP.on, denied = opDenied('scale');
    const specCells = h => {
      if (!HI.loaded) return `<td colspan="5" class="small muted">${HI.err ? '读取主机规格失败：' + esc(HI.err) : '正在读取主机规格…'}</td>`;
      if (!h.known) return `<td colspan="5" class="small muted">${tag('未采集')} 没有该主机的规格数据（node_exporter 未被 Prometheus 抓取？）</td>`;
      return `<td class="small">${h.cores ? h.cores + ' 核' : NOVAL}</td><td class="small">${h.mem ? h.mem + ' GB' : NOVAL}</td><td class="small">${esc(h.os) || NOVAL}</td><td class="small mono">${esc(h.kernel) || NOVAL}</td><td class="small">${fmtGB(h.disk)}</td>`;
    };
    const clusterCard = `<div class="card"><h2>集群内主机 <span class="sub">${ch.length} 台${HI.at ? ' · 规格读取于 ' + new Date(HI.at).toLocaleTimeString() : ''}</span>
      <span class="right"><button class="btn sm" data-na="hi-refresh" ${HI.loading ? 'disabled' : ''}>${HI.loading ? '读取中…' : '刷新规格'}</button></span></h2>
      ${HI.loaded && HI.note ? `<div class="notice warn"><span>⚠</span><div>${esc(HI.note)}</div></div>` : ''}
      <div class="tbl-wrap"><table><thead><tr><th>主机</th><th>可用区 / 机架</th><th>CPU</th><th>内存</th><th>系统</th><th>内核</th><th>数据盘</th><th>运行的实例</th><th>状态</th><th>操作</th></tr></thead><tbody>
      ${ch.map(h => { const m = inMaint(h.ip), rec = typeof MAINTS !== 'undefined' && MAINTS.find(x => x.ip === h.ip && MAINT_STATES.includes(x.state));
        return `<tr><td class="mono">${esc(h.ip)}</td><td>${esc(h.az)} / ${esc(h.rack)}</td>${specCells(h)}<td class="small">${h.insts.map(i => `<span class="tag">${esc(i)}</span>`).join(' ')}</td>
        <td>${m ? tag(rec && rec.state === 'active' ? '维护中' : rec && rec.state === 'draining' ? '进入维护中' : '恢复中', 'warn') : tag('在线', 'ok')}</td>
        <td><div class="acts">${m ? `<button class="btn sm" data-na="tab" data-t="maint">查看维护</button>` : `<button class="btn sm" data-na="mt-start" data-ip="${esc(h.ip)}" ${opDeniedAttr('maint')}>进入维护</button>`}</div></td></tr>`; }).join('') || '<tr><td colspan="10" class="empty">拓扑里没有主机</td></tr>'}
      </tbody></table></div></div>`;
    if (!op) return clusterCard + `<div class="card" style="margin-top:16px"><h2>候选主机池</h2><div class="notice info"><span>ℹ</span><div>主机池管理（新增候选主机、预检、扩容申请）需要运维模式：请使用 <code>--data-dir</code> 启动后端并登录。</div></div></div>`;
    const pool = poolList(), orphan = HP.list.filter(h => hpInCluster(h.ip));
    const when = h => h.checking ? `${tag('正在预检（约 1–2 分钟）…', 'info')} <span class="small muted">${esc(TYPE_META[HP.checking[h.ip]] ? TYPE_META[HP.checking[h.ip]].label : '')}</span>`
      : h.check ? `${tag(checkSummary(h).t, checkSummary(h).c)}<div class="small muted">${opFmtTime(h.checkAt)} · ${esc((TYPE_META[h.checkRole] || {}).label || h.checkRole)}${checkStale(h) ? ' · 已超过 5 分钟' : ''}</div>` : tag('未预检');
    const poolCard = `<div class="card" style="margin-top:16px"><h2>候选主机池 <span class="sub">尚未加入集群的机器（${pool.length} 台）</span>
      <span class="right"><button class="btn sm" data-na="hp-refresh" ${HP.loading ? 'disabled' : ''}>${HP.loading ? '读取中…' : '刷新'}</button><button class="btn primary sm" data-na="hp-add" ${opDeniedAttr('scale')}>＋ 新增候选主机</button></span></h2>
      ${hpNoticeHtml()}${hpLoadHint()}
      ${orphan.length ? `<div class="notice info"><span>ℹ</span><div style="flex:1">${orphan.map(h => esc(h.ip)).join('、')} 已在集群拓扑里，不再作为候选主机显示。<a data-na="hp-purge" ${denied ? 'title="没有扩缩容权限"' : ''}>清理这些记录</a></div></div>` : ''}
      ${HP.loaded || pool.length ? `<div class="tbl-wrap"><table><thead><tr><th>主机 IP</th><th>可用区</th><th>机架</th><th>备注</th><th>最近预检</th><th>操作</th></tr></thead><tbody>
      ${pool.map(h => `<tr><td class="mono"><b>${esc(h.ip)}</b></td><td>${esc(h.az) || '—'}</td><td>${esc(h.rack) || '—'}</td><td class="small">${esc(h.note) || '<span class="muted">—</span>'}${h.createdBy ? `<div class="muted">${esc(h.createdBy)} 添加</div>` : ''}</td><td>${when(h)}</td>
        <td><div class="acts"><button class="btn sm" data-na="host-check" data-ip="${esc(h.ip)}" ${h.checking ? 'disabled' : opDeniedAttr('scale')}>${h.check ? '重新预检' : '预检'}</button>${h.check ? `<button class="btn sm" data-na="host-detail" data-ip="${esc(h.ip)}">查看检查项</button>` : ''}<button class="btn sm" data-na="hp-edit" data-ip="${esc(h.ip)}" ${opDeniedAttr('scale')}>编辑</button><button class="btn sm" data-na="host-join" data-ip="${esc(h.ip)}" title="带着这台主机进入扩容向导">用于扩容</button><button class="btn sm danger" data-na="host-remove" data-ip="${esc(h.ip)}" ${opDeniedAttr('scale')}>删除</button></div></td></tr>`).join('') || '<tr><td colspan="6" class="empty">候选主机池为空。点击「新增候选主机」登记已准备好的机器（尚未加入集群）。</td></tr>'}
      </tbody></table></div>` : ''}
      <div class="small muted" style="margin-top:8px">预检只读，不会改动主机；平台不会自动修复，失败项请按修复命令在目标主机上以 root 处理后重新预检。</div></div>`;
    return clusterCard + poolCard;
  },

  /* ---- 主机池（静态原型） ---- */
  hosts() {
    if (REAL()) return this.hostsLive();
    const ch = clusterHosts(), ha = nodesState.hostAdd, bad = !/^(\d{1,3}\.){3}\d{1,3}$/.test(ha.ip);
    if (!azList().includes(ha.az)) ha.az = azList()[0];   // 下拉显示与内部状态保持一致
    return `
    <div class="card"><h2>集群内主机 <span class="sub">${ch.length} 台</span></h2>
      <div class="tbl-wrap"><table><thead><tr><th>主机</th><th>可用区 / 机架</th><th>规格</th><th>运行的实例</th><th>状态</th><th>操作</th></tr></thead><tbody>
      ${ch.map(h => { const m = inMaint(h.ip), rec = typeof MAINTS !== 'undefined' && MAINTS.find(x => x.ip === h.ip && MAINT_STATES.includes(x.state)); return `<tr><td class="mono">${h.ip}</td><td>${h.az} / ${h.rack}</td><td class="small">${h.cores}C · ${h.mem}G · ${h.disk}</td><td class="small">${h.insts.map(i => `<span class="tag">${i}</span>`).join(' ')}</td>
        <td>${m ? tag(rec.state === 'active' ? '维护中' : rec.state === 'draining' ? '进入维护中' : '恢复中', 'warn') : tag('在线', 'ok')}</td><td><div class="acts">${m ? `<button class="btn sm" data-na="tab" data-t="maint">查看维护</button>` : `<button class="btn sm" data-na="mt-start" data-ip="${h.ip}" ${opDeniedAttr('maint')}>进入维护</button>`}<button class="btn sm" data-na="host-detail" data-ip="${h.ip}">预检详情</button></div></td></tr>`; }).join('')}
      </tbody></table></div></div>
    <div class="card" style="margin-top:16px"><h2>主机池 <span class="sub">尚未加入集群的机器（${poolList().length} 台）</span>
      <span class="right"><button class="btn primary sm" data-na="host-add-toggle">＋ 新增主机</button></span></h2>
      ${nodesState.so.addOpen ? `<div class="card" style="background:var(--panel2);margin-bottom:12px"><div class="grid g4">
        <div class="field"><label>主机 IP</label><input type="text" data-ni="ha-ip" value="${esc(ha.ip)}" placeholder="如 10.0.2.20"></div>
        <div class="field"><label>可用区 (zone)</label><select data-nc="ha-az">${azList().map(z => `<option ${ha.az === z ? 'selected' : ''}>${z}</option>`).join('')}</select></div>
        <div class="field"><label>机架 (rack)</label><input type="text" data-ni="ha-rack" value="${esc(ha.rack)}" placeholder="r15"></div>
        <div class="field"><label>用途（决定默认规格）</label><select data-nc="ha-use">${ROLE_IDS.map(r => `<option value="${r}" ${ha.use === r ? 'selected' : ''}>${TYPE_META[r].label}</option>`).join('')}</select></div></div>
        <div class="small muted" style="margin-bottom:8px">SSH 用户 tidb，端口 22，免密登录；加入后需先通过预检才能用于扩容。</div>
        <div class="row"><button class="btn primary sm" data-na="host-add-submit">添加到主机池</button><button class="btn sm" data-na="host-add-toggle">取消</button>${ha.ip && bad ? '<span class="small" style="color:var(--bad)">IP 格式不正确</span>' : ''}</div></div>` : ''}
      ${poolList().length ? poolList().map(h => { const cs = checkSummary(h); return `<div class="hostrow c5">
        <div><b class="mono">${h.ip}</b><div class="small muted">${h.os}</div></div><div class="small">${h.az} / ${h.rack}</div><div class="small">${h.cores}C · ${h.mem}G · ${h.disk}</div>
        <div>${tag(cs.t, cs.c)} <span class="small muted">${esc(h.note || '')}</span></div>
        <div class="acts"><button class="btn sm" data-na="host-check" data-ip="${h.ip}" ${h.checking ? 'disabled' : ''}>${h.check ? '重新预检' : '预检'}</button>${hasFixable(h) ? `<button class="btn sm" data-na="host-fix" data-ip="${h.ip}" ${h.checking ? 'disabled' : ''}>自动修复</button>` : ''}<button class="btn sm" data-na="host-detail" data-ip="${h.ip}">详情</button><button class="btn sm primary" data-na="host-join" data-ip="${h.ip}">加入集群</button><button class="btn sm" data-na="host-remove" data-ip="${h.ip}">移出</button></div></div>`; }).join('') : '<div class="empty">主机池为空，点击「新增主机」</div>'}
    </div>`;
  },

  /* ---- 扩容 ---- */
  scaleOut() {
    const so = nodesState.so, steps = ['选择角色', '选择主机并预检', '配置参数', '预览与执行'];
    let html = stepper(steps, so.step);
    if (so.step === 1) {
      const sug = [];
      const full = NODES.filter(n => n.type === 'tikv' && n.disk >= 80);
      if (full.length) sug.push(['tikv', `${full.map(n => `${n.id} 磁盘 ${n.disk}%`).join('、')} 超过 80% 水位 → 建议扩容 TiKV（保持各可用区数量均衡）`]);
      const hotT = NODES.filter(n => n.type === 'tidb' && (n.mem >= 80 || n.cpu >= 70));
      if (hotT.length) sug.push(['tidb', `${hotT.map(n => `${n.id} 内存 ${n.mem}% / CPU ${n.cpu}%`).join('、')} → 可考虑扩容 TiDB 分担连接和查询`]);
      const info = {
        tidb: ['QPS / 连接数 / CPU 持续 > 70%，查询在 TiDB 侧排队', '无数据迁移，在线完成；需把新节点加入负载均衡', '不能解决 TiKV 侧瓶颈或热点'],
        tikv: ['磁盘 > 70%–80%、Region 数过多、整体读写能力不足', 'PD 会向新 Store 迁移副本，期间磁盘 / 网络 I/O 上升，建议低峰执行', '单 Region 写热点无法靠扩容缓解，请先打散热点'],
        pd: ['需要把 PD 从 3 个提升到 5 个以增强容错', '保持奇数个成员，一次加入 1 个；对性能提升有限', '不是性能扩容手段'],
        tiflash: ['分析型查询慢、需要更多列存副本或 MPP 算力', '新节点会拉取 Learner 副本，期间网络和磁盘压力较大', '需配合表的 TiFlash 副本数设置'],
        ticdc: ['changefeed checkpoint lag 持续增大，单 capture 资源不足', 'changefeed 会在 capture 之间重新均衡，短暂抖动', '下游写入慢时扩容 TiCDC 无效'],
      };
      html += `${sug.map(([r, t]) => `<div class="notice warn"><span>💡</span><div>${t} <a data-na="so-role" data-role="${r}">选择 ${TYPE_META[r].label} →</a></div></div>`).join('')}
      <div class="grid g3" style="margin-top:10px">${ROLE_IDS.map(r => `<button class="role-card ${so.role === r ? 'sel' : ''}" data-na="so-role" data-role="${r}"><h3><i class="sw" style="background:${TYPE_META[r].color}"></i>${TYPE_META[r].label}<span class="tag" style="margin-left:auto">现有 ${NODES.filter(n => n.type === r).length}</span></h3>
        <div class="small"><b>何时扩容：</b>${info[r][0]}</div><div class="small muted"><b>影响：</b>${info[r][1]}</div><div class="small muted"><b>注意：</b>${info[r][2]}</div></button>`).join('')}</div>`;
    } else if (so.step === 2) {
      const role = so.role, single = role === 'pd', real = REAL();
      const sel = so.hosts, pool = poolList();
      const azMsg = ['tikv', 'tidb'].includes(role) ? `<div class="small muted" style="margin:6px 0">当前 ${TYPE_META[role].label} 各可用区数量：${azList().map(z => `${z} ×${azCount(role, z)}`).join(' · ')} ；选中后：${azList().map(z => `${z} ×${azCount(role, z) + sel.filter(h => h.az === z).length}`).join(' · ')}</div>` : '';
      const uneven = role === 'tikv' && sel.length && new Set(azList().map(z => azCount(role, z) + sel.filter(h => h.az === z).length)).size > 1;
      const minC = {tidb: 16, tikv: 32, pd: 8, tiflash: 32, ticdc: 16}[role];
      const hostRow = h => {
        const cs = checkSummary(h), on = sel.includes(h);
        if (!real) {
          const low = h.cores < minC;
          return `<div class="hostrow c5b ${on ? 'sel' : ''}">
          <input type="checkbox" data-nc="so-pick" data-ip="${h.ip}" ${on ? 'checked' : ''} aria-label="选择 ${h.ip}">
          <div><b class="mono">${h.ip}</b><div class="small muted">${h.az} / ${h.rack}</div></div>
          <div class="small">${h.cores}C · ${h.mem}G · ${h.disk}<div>${low ? tag('规格偏低', 'warn') : tag('规格匹配', 'ok')}</div></div>
          <div>${tag(cs.t, cs.c)}<div class="small muted">${esc(h.note || '')}</div></div>
          <div class="acts"><button class="btn sm" data-na="host-check" data-ip="${h.ip}" ${h.checking ? 'disabled' : ''}>${h.check ? '重新预检' : '预检'}</button>${hasFixable(h) ? `<button class="btn sm" data-na="host-fix" data-ip="${h.ip}" ${h.checking ? 'disabled' : ''}>自动修复</button>` : ''}<button class="btn sm" data-na="host-detail" data-ip="${h.ip}">详情</button></div></div>`;
        }
        // 真实 / 运维模式：规格取自 /api/hosts 里同 IP 的记录（候选主机还没加入集群，通常没有，显示「—」）；预检状态取自候选池的最近一次预检
        const info = HI.byIp[h.ip], known = !!(info && info.known), low = known && info.cores && info.cores < minC;
        const spec = known ? `${info.cores ? info.cores + 'C' : '—'} · ${info.memGB ? info.memGB + 'G' : '—'} · ${fmtGB(info.diskGB)}` : '—';
        const failed = !h.checking && h.check && h.check.some(i => i.r === 'fail');
        return `<div class="hostrow c5b ${on ? 'sel' : ''}">
          <input type="checkbox" data-nc="so-pick" data-ip="${esc(h.ip)}" ${on ? 'checked' : ''} aria-label="选择 ${esc(h.ip)}">
          <div><b class="mono">${esc(h.ip)}</b><div class="small muted">${esc(h.az) || '—'} / ${esc(h.rack) || '—'}</div></div>
          <div class="small">${spec}${known ? `<div>${low ? tag('规格偏低', 'warn') : tag('规格匹配', 'ok')}</div>` : ''}</div>
          <div>${tag(cs.t, cs.c)}${failed ? ' ' + tag('⚠ 预检未通过', 'bad') : ''}${!h.checking && h.check && !failed && checkStale(h) ? ' ' + tag('预检已过期', 'warn') : ''}<div class="small muted">${h.check ? opFmtTime(h.checkAt) + ' · ' + esc((TYPE_META[h.checkRole] || {}).label || h.checkRole) + ' · ' : ''}${esc(h.note || '')}</div></div>
          <div class="acts"><button class="btn sm" data-na="host-check" data-ip="${esc(h.ip)}" ${h.checking ? 'disabled' : opDeniedAttr('scale')}>${h.check ? '重新预检' : '预检'}</button>${hasFixable(h) ? `<button class="btn sm" data-na="host-fix" data-ip="${esc(h.ip)}">复制修复命令</button>` : ''}<button class="btn sm" data-na="host-detail" data-ip="${esc(h.ip)}">${h.check ? '查看检查项' : '详情'}</button></div></div>`;
      };
      const emptyMsg = real ? (!OP.on ? '真实数据（只读）模式没有候选主机池：主机池管理需要运维模式' : HP.err ? '' : HP.loaded ? '候选主机池没有可用主机，请先新增候选主机' : '正在读取候选主机池…') : '主机池没有可用主机，请先新增主机';
      const ws = real ? sel.map(h => [h.ip, hostWarn(h, role)]).filter(x => x[1]) : [];
      html += `<div class="card"><h2>选择目标主机 <span class="sub">${TYPE_META[role].label}${single ? '（一次加入 1 个）' : '（可多选）'}</span><span class="right"><button class="btn sm" data-na="tab" data-t="hosts">管理主机池 →</button></span></h2>
        ${azMsg}${uneven ? '<div class="notice warn"><span>⚠</span><div>各可用区的 TiKV 数量将不均衡；PD 仍能按 zone 打散副本，但会出现某个 AZ 承载更多 Region。建议每个 AZ 同步增加。</div></div>' : ''}
        ${real ? hpNoticeHtml() + (OP.on ? hpLoadHint() : '') : ''}
        ${pool.length ? pool.map(hostRow).join('') : emptyMsg ? `<div class="empty">${emptyMsg}</div>` : ''}
        <div class="row" style="margin-top:8px">${real ? (OP.on ? `<button class="btn sm" data-na="hp-add" ${opDeniedAttr('scale')}>＋ 新增候选主机</button>` : '') : '<button class="btn sm" data-na="host-add-toggle2">＋ 新增主机</button>'}</div>
        ${!real && so.addOpen ? this.addForm() : ''}
        ${real ? (ws.length ? `<div class="notice warn" style="margin-top:10px"><span>⚠</span><div>${ws.map(([ip, t]) => `${esc(ip)}：${t}`).join('；')}。可以继续，但建议先处理并重新预检；预检未通过的主机，服务端规划里也会给出警告。</div></div>` : '')
          : sel.some(hostBlocked) ? `<div class="notice bad" style="margin-top:10px"><span>✖</span><div>${sel.filter(hostBlocked).map(h => `${h.ip}：${!h.check ? '尚未预检' : '预检存在失败项'}`).join('；')}。需通过预检（可「自动修复」可修复项）后才能继续。</div></div>` : ''}
      </div>`;
    } else if (so.step === 3) {
      const role = so.role, c = so.cfg, hosts = so.hosts, real = REAL();
      const mems = real ? hosts.map(h => (HI.byIp[h.ip] || {}).memGB).filter(Boolean) : hosts.map(h => h.mem), minMem = mems.length ? Math.min(...mems) : 0;   // 真实模式：候选主机没有采集到内存时不猜，block-cache 默认 0（不写入，用 TiKV 默认值）
      if (!c.port) Object.assign(c, {port: DEFAULT_PORTS[role], cache: Math.round(minMem * .45), rate: 15, lb: 'haproxy'});
      html += `<div class="grid g2"><div class="card"><h2>实例参数 <span class="sub">${TYPE_META[role].label}</span></h2>
        <div class="field inline"><label>服务端口</label><input type="text" data-ni="so-cfg" data-k="port" value="${c.port}"></div>
        <div class="field inline"><label>部署目录</label><input type="text" disabled value="/tidb-deploy/${role}-${c.port}"></div>
        ${role !== 'tidb' ? `<div class="field inline"><label>数据目录</label><input type="text" disabled value="/tidb-data/${role}-${c.port}"></div>` : ''}
        <div class="field inline"><label>部署用户 / 版本</label><input type="text" disabled value="tidb · ${VERSION}（与集群一致）"></div>
        ${role === 'tikv' ? `<div class="field inline"><label>block-cache 容量 (GB)</label><input type="text" data-ni="so-cfg" data-k="cache" value="${c.cache}"></div><div class="small muted" style="margin:-4px 0 8px 152px">${minMem ? `默认取主机内存 ${minMem} GB 的 45%；与其他服务混部请调低，过大会导致 TiKV OOM` : '没有采集到目标主机的内存，默认 0 = 不写入该配置（使用 TiKV 默认值）；如需指定请手填'}</div>
          <div class="field inline"><label>迁移速度 (个/分钟)</label><input type="text" data-ni="so-cfg" data-k="rate" value="${c.rate}"></div><div class="small muted" style="margin:-4px 0 0 152px">PD store limit：越大迁移越快，对线上影响越大（默认 15）</div>` : ''}
        ${role === 'tidb' ? `<div class="field inline"><label>负载均衡</label><select data-nc="so-lb"><option value="haproxy" ${c.lb === 'haproxy' ? 'selected' : ''}>HAProxy</option><option value="tiproxy" ${c.lb === 'tiproxy' ? 'selected' : ''}>TiProxy</option></select></div>` : ''}
      </div>
      <div class="card"><h2>拓扑标签 <span class="sub">用于副本隔离</span></h2>
        <div class="tbl-wrap"><table><thead><tr><th>主机</th><th>zone</th><th>rack</th><th>host</th></tr></thead><tbody>${hosts.map(h => `<tr><td class="mono">${h.ip}</td><td>${h.az}</td><td>${h.rack || '—'}</td><td class="mono small">${h.ip.replace(/\./g, '-')}</td></tr>`).join('')}</tbody></table></div>
        <div class="small muted" style="margin-top:8px">标签与集群 <code>location-labels: zone,host</code> 对应；缺失标签会导致 PD 无法按故障域打散副本。</div></div></div>`;
    } else {
      const role = so.role, c = so.cfg, hosts = so.hosts, label = TYPE_META[role].label, m = hosts.length;
      const all = NODES.filter(n => n.type === role), n0 = all.length;
      const impact = [];
      if (role === 'tikv') {
        const tot = sumBy(all, 'regions'), avg = Math.round(tot / (n0 + m)), eta = avg / Math.max(1, +c.rate);
        impact.push(['每个新 Store 预计承载 Region', `${fmt(avg)}（≈ ${(avg * 96 / 1048576).toFixed(2)} TiB）`], ['迁移速度 / 预计耗时', `${c.rate} 个/分钟/Store · 约 ${eta > 90 ? (eta / 60).toFixed(1) + ' 小时' : Math.round(eta) + ' 分钟'}`],
          ['磁盘平均使用率', `${Math.round(sumBy(all, 'disk') / n0)}% → ${Math.round(sumBy(all, 'disk') / (n0 + m))}%`], ['Leader 数（每个 Store）', `${fmt(Math.round(sumBy(all, 'leaders') / n0))} → ${fmt(Math.round(sumBy(all, 'leaders') / (n0 + m)))}`]);
      } else if (role === 'tidb') impact.push(['平均 QPS / 节点', `${fmt(Math.round(sumBy(all, 'qps') / n0))} → ${fmt(Math.round(sumBy(all, 'qps') / (n0 + m)))}`], ['平均 CPU', `${Math.round(sumBy(all, 'cpu') / n0)}% → ${Math.round(sumBy(all, 'cpu') / (n0 + m))}%`], ['数据迁移', '无']);
      else if (role === 'pd') impact.push(['PD 成员数', `${n0} → ${n0 + m}`], ['容错能力', `可容忍 ${Math.floor((n0 - 1) / 2)} → ${Math.floor((n0 + m - 1) / 2)} 个节点故障`]);
      else if (role === 'tiflash') impact.push(['TiFlash 节点数', `${n0} → ${n0 + m}`], ['数据同步', '新节点以 Learner 拉取所有 TiFlash 副本表的数据']);
      else impact.push(['TiCDC capture 数', `${n0} → ${n0 + m}`]);
      const warns = [];
      if (role === 'tikv') { const hot = NODES.filter(n => n.type === 'tikv' && n.wr > 100); if (hot.length) warns.push(['warn', `${hot.map(n => n.id).join('、')} 存在写热点（写流量约为均值 4.8×）。扩容不能缓解单 Region 热点，建议同时用 AUTO_RANDOM / SHARD_ROW_ID_BITS 打散。`]); if (+c.rate > 30) warns.push(['warn', `迁移速度 ${c.rate} 个/分钟高于默认值，可能影响线上延迟，建议低峰执行并观察 P99`]); }
      if (REAL()) {
        const ws = hosts.map(h => [h.ip, hostWarn(h, role)]).filter(x => x[1]);
        if (ws.length) ws.forEach(([ip, t]) => warns.push(['warn', `${ip}：${t}`])); else warns.push(['ok', '目标主机最近一次预检全部通过']);
        if (role === 'tikv' && hosts.some(h => !h.az)) warns.push(['warn', `${hosts.filter(h => !h.az).map(h => h.ip).join('、')} 没有设置可用区（zone 标签），TiKV 扩容需要 zone；请先在主机池里编辑补全`]);
      } else warns.push(['ok', '目标主机预检全部通过'], ['ok', '拓扑标签与 location-labels 一致']);
      if (role === 'pd' && (n0 + m) % 2 === 0) warns.push(['warn', `加入后 PD 为 ${n0 + m} 个（偶数），建议保持奇数`]);
      const cmds = soCmds(role, hosts, c);
      html += `<div class="grid g2"><div class="card"><h2>scale-out.yml</h2>${code(genYaml(role, hosts, c))}<h2 style="margin-top:14px">执行命令</h2>${code(cmds.join('\n'))}</div>
        <div class="card"><h2>影响评估</h2><div class="kv">${impact.map(([k, v]) => `<span>${k}</span><span>${v}</span>`).join('')}</div><div class="hr"></div>${warns.map(([lv, t]) => `<div class="notice ${lv}"><span>${lv === 'ok' ? '✔' : '⚠'}</span><div>${t}</div></div>`).join('')}
        <label class="row small" style="margin-top:10px"><input type="checkbox" data-nc="so-ack" ${so.ack ? 'checked' : ''}> 我已确认变更窗口、回滚方案与影响范围</label></div></div>`;
    }
    const canNext = so.step === 1 ? !!so.role : so.step === 2 ? so.hosts.length > 0 && !so.hosts.some(hostBlocked) : true;
    html += `<div class="sticky-actions">${so.step > 1 ? '<button class="btn" data-na="so-prev">← 上一步</button>' : ''}
      ${so.step < 4 ? `<button class="btn primary" data-na="so-next" ${canNext ? '' : 'disabled'}>下一步 →</button>` : OP.on ? `<button class="btn primary" data-na="so-submit" ${opDenied('scale') ? OP_DENY_ATTR : so.ack ? '' : 'disabled'}>提交扩容申请…</button>` : `<button class="btn primary" data-na="so-submit" ${so.ack ? '' : 'disabled'}>开始扩容（模拟执行）</button><button class="btn" data-na="sched-from" data-src="scaleout">预约到低峰窗口…</button>`}
      ${so.step === 2 && !canNext ? `<span class="small muted">${REAL() ? (so.hosts.some(hostBlocked) ? '有主机正在预检，请等待完成' : '请至少选择 1 台候选主机') : '请至少选择 1 台已通过预检的主机'}</span>` : ''}</div>`;
    return html;
  },
  addForm() {
    const ha = nodesState.hostAdd;
    if (!azList().includes(ha.az)) ha.az = azList()[0];   // 下拉显示与内部状态保持一致
    return `<div class="card" style="background:var(--panel2);margin-top:10px"><div class="grid g4">
      <div class="field"><label>主机 IP</label><input type="text" data-ni="ha-ip" value="${esc(ha.ip)}" placeholder="如 10.0.2.20"></div>
      <div class="field"><label>可用区 (zone)</label><select data-nc="ha-az">${azList().map(z => `<option ${ha.az === z ? 'selected' : ''}>${z}</option>`).join('')}</select></div>
      <div class="field"><label>机架 (rack)</label><input type="text" data-ni="ha-rack" value="${esc(ha.rack)}" placeholder="r15"></div>
      <div class="field"><label>用途</label><select data-nc="ha-use">${ROLE_IDS.map(r => `<option value="${r}" ${ha.use === r ? 'selected' : ''}>${TYPE_META[r].label}</option>`).join('')}</select></div></div>
      <div class="row"><button class="btn primary sm" data-na="host-add-submit">添加并预检</button><button class="btn sm" data-na="host-add-toggle2">取消</button></div></div>`;
  },

  /* ---- 缩容 ---- */
  scaleIn() {
    const si = nodesState.si, steps = ['选择节点', '影响评估', '确认并执行'];
    let html = stepper(steps, si.step);
    const ev = evalScaleIn(si.picks);
    if (si.step === 1) {
      const list = NODES.filter(n => si.filter === 'all' || n.type === si.filter);
      html += `<div class="card"><h2>选择要下线的节点 <span class="sub">已选 ${si.picks.length}</span></h2>
        <div class="chips" style="margin-bottom:10px">${['all', ...ROLE_IDS].map(r => `<span class="chip ${si.filter === r ? 'active' : ''}" data-na="sf" data-r="${r}">${r === 'all' ? '全部' : TYPE_META[r].label}</span>`).join('')}</div>
        <div class="tbl-wrap"><table><thead><tr><th></th><th>实例</th><th>地址</th><th>可用区</th><th>状态</th><th>数据 / 负载</th></tr></thead><tbody>
        ${list.map(n => { const dis = n.status === 'joining' || n.status === 'offline' || busy(n.id); return `<tr><td><input type="checkbox" data-nc="si-pick" data-id="${esc(n.id)}" ${si.picks.includes(n.id) ? 'checked' : ''} ${dis ? 'disabled' : ''} aria-label="选择 ${esc(n.id)}"></td><td><b>${esc(n.id)}</b></td><td class="mono small">${esc(n.host)}:${esc(n.port)}</td><td>${esc(n.az)}</td><td>${tag(stText[n.status], stCls(n.status))}</td>
          <td class="small">${n.type === 'tikv' ? `Region ${fmt(n.regions)} · 磁盘 ${n.disk}%` : n.type === 'tidb' ? `连接 ${n.conn} · QPS ${fmt(n.qps)}` : n.type === 'pd' ? (n.leader ? 'Leader' : 'Follower') : n.type === 'tiflash' ? `Learner ${fmt(n.regions)}` : `lag ${n.lag}`}</td></tr>`; }).join('')}
        </tbody></table></div></div>`;
    } else if (si.step === 2) {
      html += `<div class="grid g-main"><div class="card"><h2>前置检查 <span class="sub">${si.picks.join('、')}</span></h2>
        ${ev.items.map(i => `<div class="notice ${i.lv === 'info' ? 'info' : i.lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[i.lv]}</span><div>${i.t}</div></div>`).join('')}
        ${ev.blocked ? '<div class="notice bad"><b>存在阻断项，无法继续。</b>请调整所选节点，或先扩容 / 修复后再缩容。</div>' : ''}</div>
        <div class="card"><h2>剩余节点影响预测</h2>${ev.proj.length ? `<div class="tbl-wrap"><table><thead><tr><th>节点</th><th>指标</th><th class="num">缩容前</th><th class="num">缩容后</th></tr></thead><tbody>${ev.proj.map(p => `<tr><td>${p.id}</td><td>${p.k}</td><td class="num">${p.a}${p.u}</td><td class="num" style="color:var(--${p.b >= p.bad ? 'bad' : p.b >= p.warn ? 'warn' : 'ok'});font-weight:600">${p.b}${p.u}</td></tr>`).join('')}</tbody></table></div><div class="small muted" style="margin-top:6px">假设各节点容量相同、数据均匀迁移。</div>` : '<div class="muted small">该组件缩容不涉及资源再分配</div>'}</div></div>`;
    } else {
      html += `<div class="grid g2"><div class="card"><h2>将执行的命令</h2>${code(siCmds(ev.picks).join('\n'))}
        <div class="small muted">TiKV 缩容是异步的：Store 先变为 Offline，Region 迁移完成后变为 Tombstone，随后清理实例。期间请勿再对该节点做其他操作。</div></div>
        <div class="card"><h2>最终确认</h2><div class="kv"><span>下线节点</span><span>${si.picks.join('、')}</span><span>预计影响</span><span>${ev.picks.some(n => n.type === 'tikv') ? 'Region 迁移，磁盘 / 网络 I/O 升高' : '短暂连接断开 / 抖动'}</span></div>
        <div class="field" style="margin-top:12px"><label>此操作不可直接撤销，请输入「确认缩容」</label><input type="text" data-ni="si-confirm" value="${esc(si.confirm)}" placeholder="确认缩容"></div></div></div>`;
    }
    const canNext = si.step === 1 ? si.picks.length > 0 : si.step === 2 ? !ev.blocked : si.confirm === '确认缩容';
    html += `<div class="sticky-actions">${si.step > 1 ? '<button class="btn" data-na="si-prev">← 上一步</button>' : ''}
      ${si.step < 3 ? `<button class="btn primary" data-na="si-next" ${canNext ? '' : 'disabled'}>下一步 →</button>` : OP.on ? `<button class="btn danger" id="siGo" data-na="si-submit" ${opDenied('scale') ? OP_DENY_ATTR : canNext ? '' : 'disabled'}>提交缩容申请…</button>` : `<button class="btn danger" id="siGo" data-na="si-submit" ${canNext ? '' : 'disabled'}>开始缩容（模拟执行）</button><button class="btn" data-na="sched-from" data-src="scalein">预约到低峰窗口…</button>`}</div>`;
    return html;
  },

  /* ---- 任务中心 ---- */
  tasks() {
    if (OP.on) return opTasksHtml();
    if (!TASKS.length) return '<div class="card"><div class="empty">暂无任务。在「节点实例」「扩容」「缩容」中提交操作后，这里会显示进度和日志。</div></div>';
    const stc = {running: ['info', '执行中'], success: ['ok', '成功'], aborted: ['warn', '已中止']};
    return TASKS.map(t => {
      const done = t.steps.filter(s => s.state === 'done').length, cur = t.steps[t.cur], pct = Math.round((done + (cur && t.state === 'running' ? cur.pct : 0)) / t.steps.length * 100);
      return `<div class="task"><div class="hd"><span class="mono small muted">${t.id}</span><b>${esc(t.title)}</b>${tag(stc[t.state][1], stc[t.state][0])}<span class="small muted">开始 ${t.started}${t.finished ? ' · 结束 ' + t.finished : ''}</span>
        <span style="margin-left:auto;display:flex;gap:8px;align-items:center;min-width:220px"><div class="pbar ${t.state === 'success' ? 'ok' : ''}" style="flex:1"><b style="width:${t.state === 'success' ? 100 : pct}%"></b></div><span class="small">${t.state === 'success' ? 100 : pct}%</span></span>
        ${t.state === 'running' && t.abortable ? `<button class="btn sm danger" data-na="task-abort" data-id="${t.id}">中止</button>` : ''}</div>
        <div class="stepsl">${t.steps.map(s => `<div class="${s.state}"><span>${{done: '✔', run: '◔', wait: '○'}[s.state]}</span>${esc(s.name)}${s.ticks > 1 && s.state === 'run' ? `<span class="w pbar"><b style="width:${Math.round(s.pct * 100)}%"></b></span>` : ''}</div>`).join('')}</div>
        <details class="fold" data-tid="${t.id}" style="margin:0 14px 12px" ${openCmds.has(t.id) ? 'open' : ''}><summary><span class="small">执行命令</span></summary><div class="body">${code(t.cmds.join('\n'))}</div></details>
        <pre class="code log">${esc(t.log.join('\n'))}</pre></div>`;
    }).join('');
  },
};

/* ================== 运维模式：任务中心（渲染服务端变更 OP.changes） ================== */
const ocUi = {open: new Set(), filter: 'all', form: null};
const OC_ACTIVE = ['approved', 'scheduled', 'running'], OC_FINAL = ['succeeded', 'failed', 'aborted'];
const ocMe = () => ((OP.me && OP.me.user && OP.me.user.name) || '').toLowerCase();
const ocOwner = c => c.applicant.toLowerCase() === ocMe() || opHasRole('dba');
const ocCap = c => c.risk === 'high' ? opCan('approve.high') : c.risk === 'mid' ? opCan('approve.mid') : false;
const ocCanApprove = c => c.state === 'pending' && ocCap(c) && c.applicant.toLowerCase() !== ocMe() && !(c.approvals || []).some(a => a.user.toLowerCase() === ocMe());
const OC_ICON = {done: '✔', run: '◔', wait: '○', manual: '⏸', failed: '✖', skipped: '↷'};
const ocDur = (a, b) => {
  if (!a) return '';
  const s = Math.max(0, Math.round(((b ? new Date(b).getTime() : opNow()) - new Date(a).getTime()) / 1000));
  return s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s` : `${Math.floor(s / 3600)}h${String(Math.floor(s % 3600 / 60)).padStart(2, '0')}m`;
};
const ocPct = c => { const st = c.steps || []; if (!st.length) return 0; if (c.state === 'succeeded') return 100; return Math.round(st.filter(x => x.state === 'done' || x.state === 'skipped').length / st.length * 100); };
const ocMatch = (c, f) => f === 'all' || (f === 'pending' ? c.state === 'pending' : f === 'active' ? OC_ACTIVE.includes(c.state) : !['pending', ...OC_ACTIVE].includes(c.state));
const ocBtn = (na, id, label, cls = '') => `<button class="btn sm ${cls}" data-na="${na}" data-id="${id}">${label}</button>`;

function ocActions(c) {
  const a = [];
  if (ocCanApprove(c)) a.push(ocBtn('oc-approve', c.id, '批准', 'primary'));
  if (c.state === 'pending' && ocCap(c)) a.push(ocBtn('oc-reject', c.id, '驳回', 'danger'));
  if (['pending', 'approved', 'scheduled'].includes(c.state) && ocOwner(c)) a.push(ocBtn('oc-cancel', c.id, '取消'));
  if (c.state === 'running' && c.plan && c.plan.abortable && ocOwner(c)) a.push(ocBtn('oc-abort', c.id, '中止', 'danger'));
  if (c.emergency && OC_FINAL.includes(c.state) && c.applicant.toLowerCase() === ocMe()) a.push(ocBtn('oc-review', c.id, c.postReview ? '修改复盘' : '填写复盘', c.postReview ? '' : 'primary'));
  return a.join('');
}

function ocDetail(c) {
  const plan = c.plan || {}, st = c.steps || [], ap = c.approvals || [];
  const kv = [['变更类型', `${esc(c.kind)}${c.emergency ? ' · 紧急变更' : ''}`], ['环境 / 集群', `${esc(c.env)} / ${esc(c.cluster)}`], ['目标实例', esc((plan.targets || []).join('、') || '—')],
    ['申请说明', esc(c.note || '—')]];
  if (c.scheduledAt) kv.push(['定时执行', opFmtTime(c.scheduledAt)]);
  if (c.state === 'pending') kv.push(['审批有效期至', opFmtTime(c.expires)]);
  if (c.started) kv.push(['开始 / 结束', `${opFmtTime(c.started)} → ${c.finished ? opFmtTime(c.finished) : '进行中'}（${ocDur(c.started, c.finished)}）`]);
  if (c.reason) kv.push([c.state === 'rejected' ? '驳回原因' : c.state === 'failed' ? '失败原因' : '说明', esc(c.reason)]);
  const apHtml = `<div><b class="small">审批记录</b> <span class="small muted">${ap.length}/${c.need} 级${c.needMfa ? ' · 申请人需 MFA' : ''}</span>
    ${ap.map(a => `<div class="small">✔ 第 ${a.level} 级 · <b>${esc(a.user)}</b> · ${opFmtTime(a.time)}${a.comment ? ' · ' + esc(a.comment) : ''}</div>`).join('')}
    ${c.state === 'pending' ? `<div class="small" style="color:var(--warn)">还需 ${Math.max(0, c.need - ap.length)} 级审批${ocCanApprove(c) ? '（你可以审批）' : ''}</div>` : !ap.length ? `<div class="small muted">${c.need ? '无' : '低风险，无需审批（已留痕）'}</div>` : ''}</div>`;
  const steps = `<div class="stepsl" style="padding:0">${st.map((s, i) => {
    const pst = (plan.steps || [])[i] || {}, cls = ['done', 'run', 'wait', 'manual', 'failed', 'skipped'].includes(s.state) ? s.state : 'wait';
    return `<div class="${cls}"><span>${OC_ICON[cls]}</span><span>${esc(s.name)}</span>${s.started ? `<span class="small muted">${ocDur(s.started, s.ended)}</span>` : ''}${s.detail ? `<span class="small muted">· ${esc(s.detail)}</span>` : ''}
      ${pst.irreversible ? tag('不可中止') : ''}${s.state === 'manual' ? (ocOwner(c) ? `<button class="btn sm primary" data-na="oc-confirm" data-id="${c.id}" style="margin-left:auto">我已完成，继续</button>` : tag('等待申请人 / DBA 确认', 'warn')) : ''}</div>
      ${s.state === 'manual' && pst.manual ? `<div class="notice warn" style="margin:0 0 4px 22px"><span>⏸</span><div>${esc(pst.manual)}</div></div>` : ''}`;
  }).join('')}</div>`;
  const checks = (plan.checks || []).map(k => `<div class="notice ${k.lv === 'info' ? 'info' : k.lv}"><span>${opLvIcon[k.lv] || 'ℹ'}</span><div>${esc(k.t)}</div></div>`).join('');
  return `<div class="detail">
    <div class="kv">${kv.map(([k, v]) => `<span>${k}</span><span>${v}</span>`).join('')}</div>
    ${apHtml}${steps}
    <details class="fold" data-tid="${c.id}:ck" style="margin:0" ${openCmds.has(c.id + ':ck') ? 'open' : ''}><summary><span class="small">前置检查（${(plan.checks || []).length}）</span></summary><div class="body">${checks || '<div class="small muted">无</div>'}</div></details>
    <details class="fold" data-tid="${c.id}" style="margin:0" ${openCmds.has(c.id) ? 'open' : ''}><summary><span class="small">执行命令</span></summary><div class="body">${code((plan.display || []).join('\n') || '（无集群命令）')}</div></details>
    ${c.emergency ? `<div class="notice ${c.postReview ? 'info' : 'warn'}"><span>📝</span><div><b>事后复盘</b>：${c.postReview ? esc(c.postReview) : '尚未填写（紧急变更要求事后复盘）'}</div></div>` : ''}
    <div><b class="small">执行日志（最近 ${(c.log || []).length} 行）</b><pre class="code log" data-lg="${c.id}" style="margin:4px 0 0;max-height:220px">${esc((c.log || []).join('\n'))}</pre></div></div>`;
}

function ocCard(c) {
  const open = ocUi.open.has(c.id), pct = ocPct(c), cur = (c.steps || []).find(x => x.state === 'run' || x.state === 'manual');
  const needTag = c.need ? tag(`审批 ${(c.approvals || []).length}/${c.need}`, (c.approvals || []).length >= c.need ? 'ok' : c.state === 'pending' ? 'warn' : '') : tag('免审批', '');
  return `<div class="task ${ocCanApprove(c) ? 'hot' : ''}" data-ch="${c.id}"><div class="hd" data-na="oc-toggle" data-id="${c.id}" style="cursor:pointer">
    <span class="small muted">${open ? '▾' : '▸'}</span><span class="mono small muted">${c.id}</span><b>${esc(c.title)}</b>${opStateTag(c.state)}${opRiskTag(c.risk)}${c.emergency ? tag('紧急', 'bad') : ''}${c.emergency && !c.postReview && OC_FINAL.includes(c.state) ? tag('待复盘', 'warn') : ''}${needTag}
    <span class="small muted">${esc(c.applicant)} · 创建 ${opFmtTime(c.created)}${c.started ? ' · 开始 ' + opFmtTime(c.started) : c.scheduledAt && !c.finished ? ' · 定于 ' + opFmtTime(c.scheduledAt) : ''}${c.finished ? ' · 结束 ' + opFmtTime(c.finished) : ''}</span>
    <span style="margin-left:auto;display:flex;gap:8px;align-items:center;min-width:170px"><div class="pbar ${c.state === 'succeeded' ? 'ok' : ''}" style="flex:1"><b style="width:${pct}%"></b></div><span class="small">${pct}%</span></span>
    <span class="acts">${ocActions(c)}</span></div>
    ${!open && cur ? `<div class="small muted" style="padding:0 14px 10px">${OC_ICON[cur.state]} ${esc(cur.name)}${cur.state === 'manual' ? '（等待人工确认）' : ''}</div>` : ''}
    ${open ? ocDetail(c) : ''}</div>`;
}

function opTasksHtml() {
  if (!OP.loaded) return '<div class="card"><div class="empty">正在加载服务端变更…</div></div>';
  const f = ocUi.filter, list = OP.changes.filter(c => ocMatch(c, f)), pend = OP.changes.filter(ocCanApprove);
  const cnt = k => OP.changes.filter(c => ocMatch(c, k)).length;
  const block = pend.length ? `<div class="pendblk"><div class="row"><b>⚠ 待你审批（${pend.length}）</b><span class="small muted">审批人不能是申请人；高风险需两级审批，且不能同一人重复审批</span></div>
    ${pend.map(c => `<div class="it"><span class="mono small muted">${c.id}</span><b>${esc(c.title)}</b>${opRiskTag(c.risk)}${c.emergency ? tag('紧急', 'bad') : ''}<span class="small muted">${esc(c.applicant)} 申请 · ${opFmtTime(c.created)}${c.note ? ' · ' + esc(c.note) : ''} · 已批 ${(c.approvals || []).length}/${c.need}</span>
      <span class="acts" style="margin-left:auto">${ocBtn('oc-approve', c.id, '批准', 'primary')}${ocBtn('oc-reject', c.id, '驳回', 'danger')}${ocBtn('oc-goto', c.id, '查看详情')}</span></div>`).join('')}</div>` : '';
  const chips = `<div class="chips" style="margin-bottom:12px">${[['all', '全部'], ['pending', '待审批'], ['active', '排队 / 执行中'], ['done', '已结束']].map(([k, t]) => `<span class="chip ${f === k ? 'active' : ''}" data-na="oc-filter" data-f="${k}">${t} ${cnt(k)}</span>`).join('')}
    <span class="small muted" style="margin-left:auto">服务端变更 · 每 2–8 秒自动刷新</span></div>`;
  if (!OP.changes.length) return block + '<div class="card"><div class="empty">暂无变更。在「节点实例」「扩容」「缩容」「主机维护」「配置变更」中提交后，这里会显示审批进度、执行步骤和日志。</div></div>';
  return block + chips + (list.length ? list.slice(0, 80).map(ocCard).join('') + (list.length > 80 ? `<div class="small muted">仅显示最近 80 条，共 ${list.length} 条</div>` : '') : '<div class="card"><div class="empty">没有符合条件的变更</div></div>');
}

/* 跳到「节点管理 → 任务中心」并展开某个变更 */
function ocGoto(id) {
  if (id) ocUi.open.add(id);
  closeDrawer(); ocUi.filter = 'all'; nodesState.tab = 'tasks';
  if (location.hash !== '#nodes') location.hash = 'nodes'; else pages.nodes.body();
  setTimeout(() => { const el = id && document.querySelector(`[data-ch="${id}"]`); if (el) el.scrollIntoView({block: 'start', behavior: 'smooth'}); }, 250);
}
/* 约定接口（ops.js 在变更提交成功后调用）：重置各向导 / 草稿，跳到任务中心并展开该变更 */
function opGoTasks(id) {
  nodesState.so = {step: 1, role: null, hosts: [], cfg: {}, ack: false, addOpen: false};
  nodesState.si = {step: 1, picks: [], filter: 'all', confirm: ''};
  if (typeof ndState !== 'undefined') Object.assign(ndState, {draft: {}, stage: 'edit', ack: false, confirm: ''});
  if (typeof mtReset === 'function') mtReset();
  const c = OP.changes.find(x => x.id === id);
  if (c && c.kind === 'maint.extend') { nodesState.tab = 'maint'; if (location.hash !== '#nodes') location.hash = 'nodes'; else pages.nodes.body(); return; }   // 延长窗口通常立即完成，留在维护页
  ocGoto(id);
}

async function ocPost(id, act, body, okMsg) {
  const r = await opApi('POST', `/api/changes/${encodeURIComponent(id)}/${act}`, body || {});
  toast(r.code === 200 ? okMsg : (r.data.error || '操作失败（' + r.code + '）'));
  await opRefresh(true);
  return r.code === 200;
}
const OC_FORMS = {
  approve: {title: '批准变更', intro: '批准后该变更按计划排队或定时执行，不能撤销。请核对下面的内容与你要审批的变更一致。', label: '审批意见（可选）', need: false, btn: '确认批准', danger: false},
  reject: {title: '驳回变更', intro: '驳回后该变更终止，申请人需要重新提交。', label: '驳回原因（必填）', need: true, btn: '确认驳回', danger: true},
  cancel: {title: '取消变更', intro: '取消后该变更不会再执行，占用的实例会被释放。', label: '取消原因（可选）', need: false, btn: '确认取消', danger: true},
  abort: {title: '中止执行', intro: '中止会停止后续步骤，并尽力执行计划里的回滚命令。已越过不可逆步骤的变更无法中止。', label: '', need: false, btn: '确认中止', danger: true},
  review: {title: '事后复盘', intro: '紧急变更必须在事后说明：为什么紧急、做了什么、有无遗留风险与改进项。', label: '复盘内容（必填）', need: true, btn: '提交复盘', danger: false},
};
/* 批准时让审批人看清楚在批什么：申请人、类型、环境 / 集群、目标实例、申请说明（都来自服务端数据，已转义） */
function ocBrief(c) {
  const plan = c.plan || {};
  return `<div class="kv" style="margin-bottom:10px"><span>申请人</span><span>${esc(c.applicant)}</span><span>变更类型</span><span>${esc(c.kind)}${c.emergency ? ' · 紧急变更' : ''}</span>
    <span>环境 / 集群</span><span>${esc(c.env)} / ${esc(c.cluster)}</span><span>目标实例</span><span>${esc((plan.targets || []).join('、') || '—')}</span>
    <span>申请说明</span><span>${esc(c.note || '—')}</span></div>`;
}
function ocForm(act, c) {
  const F = OC_FORMS[act];
  openDrawer(`${F.title} · ${c.id}`, `<div id="ocFormRoot"><div class="small" style="margin-bottom:8px"><b>${esc(c.title)}</b> ${opRiskTag(c.risk)}</div>${act === 'approve' ? ocBrief(c) : ''}<div class="small muted" style="margin-bottom:10px">${F.intro}</div>
    ${F.label ? `<div class="field"><label>${F.label}</label><textarea id="ocTxt" rows="4" style="width:100%">${act === 'review' ? esc(c.postReview || '') : ''}</textarea></div>` : ''}
    <div id="ocErr" class="small" style="color:var(--bad);min-height:18px"></div>
    <div class="sticky-actions"><button class="btn ${F.danger ? 'danger' : 'primary'}" data-na="oc-form-do">${F.btn}</button><button class="btn" data-close="1">返回</button></div></div>`);
  ocUi.form = {act, id: c.id, root: $('#ocFormRoot')};   // 提交时只认这张表单里的按钮，动作和变更 id 取自这里，不取按钮上的 data-*
}
/* 批准 / 驳回 / 取消 / 中止 / 确认这类按钮只认任务中心（和维护页）自己渲染的；别处（诊断结果、慢 SQL 等）拼进页面的同名 data-na 按钮不触发 */
const ocFromHere = (el, tabs) => location.hash === '#nodes' && tabs.includes(nodesState.tab) && !!el.closest('#ndBody') && !el.closest('#drawer');

document.addEventListener('click', async e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled || !OP.on) return;
  const d = el.dataset, c = d.id && OP.changes.find(x => x.id === d.id);
  switch (d.na) {
    case 'oc-toggle': if (window.getSelection && String(window.getSelection())) break; ocUi.open.has(d.id) ? ocUi.open.delete(d.id) : ocUi.open.add(d.id); pages.nodes.body(); break;
    case 'oc-filter': ocUi.filter = d.f; pages.nodes.body(); break;
    case 'oc-goto': ocGoto(d.id); break;
    case 'oc-approve': if (c && ocCanApprove(c) && ocFromHere(el, ['tasks'])) ocForm('approve', c); break;   // 先弹确认（看清内容、可写意见），不直接提交
    case 'oc-confirm': if (!ocFromHere(el, ['tasks', 'maint'])) break; el.disabled = true; await ocPost(d.id, 'confirm', {}, '已确认，继续执行'); break;
    case 'oc-reject': case 'oc-cancel': case 'oc-abort': case 'oc-review': if (c && ocFromHere(el, ['tasks', 'maint'])) ocForm(d.na.slice(3), c); break;
    case 'oc-form-do': {
      const f = ocUi.form;
      if (!f || !f.root || !f.root.isConnected || !f.root.contains(el)) break;
      const F = OC_FORMS[f.act], t = $('#ocTxt') ? $('#ocTxt').value.trim() : '';
      if (F.need && !t) { $('#ocErr').textContent = '请填写内容'; break; }
      el.disabled = true;
      const body = f.act === 'review' ? {text: t} : f.act === 'abort' ? {} : f.act === 'approve' ? {comment: t} : {reason: t};
      const ok = await ocPost(f.id, f.act, body, {approve: '已批准', reject: '已驳回', cancel: '已取消', abort: '已发出中止指令', review: '复盘已保存'}[f.act] + ' ' + f.id);
      if (ok) { ocUi.form = null; closeDrawer(); } else { el.disabled = false; $('#ocErr').textContent = '操作失败，详见提示'; }
      break;
    }
  }
});

/* 服务端数据更新后：同步维护记录给只读引用 MAINTS 的页面（总览 / 主机池），并刷新节点管理页 */
function opSyncMaints() { if (typeof MAINTS === 'undefined') return; MAINTS.length = 0; OP.maints.forEach(m => MAINTS.push({...m})); }
OP_HOOKS.push((hash, ctx) => {
  opSyncMaints();
  const ae = document.activeElement, typing = ae && ['INPUT', 'TEXTAREA', 'SELECT'].includes(ae.tagName) && ae.closest('#view, #drawer');   // 登录框里的焦点不算「正在输入」
  if (hash !== 'nodes' || typing || !OP.me || !pages.nodes.body) return;
  if (['inst', 'tasks', 'maint'].includes(nodesState.tab)) pages.nodes.body(true);
  else $$('#ndTabs button').forEach(b => { if (b.dataset.t === 'maint') b.textContent = maintTabLabel(); if (b.dataset.t === 'tasks') b.textContent = taskTabLabel(); });
});

/* ================== 事件 ================== */
function setTab(t) { nodesState.tab = t; if (location.hash !== '#nodes') { location.hash = 'nodes'; } else pages.nodes.body(); }
function afterSubmit(plan) { if (OP.on && plan.intent) { opIntentDrawer(plan.intent, plan.title); return; } startTask(plan); closeDrawer(); toast(plan.stay ? '已提交，进度见本页或任务中心' : '任务已创建，可在任务中心查看进度'); if (!plan.stay) nodesState.tab = 'tasks'; if (location.hash.slice(1) === 'nodes') pages.nodes.body(); else location.hash = 'nodes'; }

document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
  const d = el.dataset, st = nodesState, so = st.so, si = st.si, ip = d.ip, host = ip && hostByIp(ip);
  switch (d.na) {
    case 'tab': setTab(d.t); break;
    case 'rf': st.roleFilter = d.r; pages.nodes.body(); break;
    case 'ia': { const n = nodeBy(d.id); if (n) planDrawer(instPlan(n, d.act)); break; }
    case 'run-pending': if (pending) { const p = pending; pending = null; afterSubmit(p); } break;
    case 'scalein-one': st.tab = 'scalein'; st.si = {step: 2, picks: [d.id], filter: 'all', confirm: ''}; pages.nodes.body(); $$('#ndTabs button').forEach(b => b.classList.toggle('active', b.dataset.t === 'scalein')); break;
    case 'host-check':
      if (!host) break;
      if (!REAL()) { runCheck(host); break; }
      if (opDenied('scale')) { toast('权限不足'); break; }
      if (host.checking) break;
      if (st.tab === 'scaleout' && so.role) { closeDrawer(); hpCheck(ip, so.role); } else hpCheckAsk(ip);   // 扩容向导里按已选角色直接预检；主机池页先选目标角色
      break;
    case 'host-fix':
      if (!host) break;
      if (REAL()) copyText(fixCmds(host), '已复制修复命令。平台不会自动修复主机，请在目标主机上以 root 执行');
      else applyFix(host);
      break;
    case 'fix-copy': copyText(d.cmd, '已复制修复命令。平台不会自动修复主机，请在目标主机上以 root 执行'); break;
    case 'hp-add': hpFormOpen('add'); break;
    case 'hp-edit': hpFormOpen('edit', ip); break;
    case 'hp-form-do': hpFormSubmit(); break;
    case 'hp-del-do': el.disabled = true; hpDelete(ip).then(async ok => { if (ok) { closeDrawer(); toast(`${ip} 已移出候选主机池`); await hpLoad(); } else el.disabled = false; }); break;
    case 'hp-check-do': closeDrawer(); hpCheck(ip, HP.askRole); break;
    case 'hp-refresh': HP.notice = null; hpLoad(); break;
    case 'hi-refresh': hiLoad(true); break;
    case 'hp-dismiss': HP.notice = null; hpRerender(); break;
    case 'hp-purge':
      if (opDenied('scale')) { toast('权限不足'); break; }
      (async () => { for (const h of HP.list.filter(x => hpInCluster(x.ip))) await hpDelete(h.ip, true); toast('已清理已加入集群的主机池记录'); await hpLoad(); })();
      break;
    case 'host-detail': {
      if (REAL()) { if (host) hpViewDrawer(ip); break; }
      const h = host || clusterHosts().find(x => x.ip === ip); if (!h) break;
      const chk = h.check || genCheck({preset: {}});
      openDrawer(`主机 ${ip}`, `<div class="kv"><span>可用区 / 机架</span><span>${h.az} / ${h.rack || '—'}</span><span>规格</span><span>${h.cores}C · ${h.mem}G · ${h.disk}</span><span>系统</span><span>${h.os}</span></div>
        <h3 style="font-size:14px;margin:14px 0 6px">tiup cluster check 预检结果${h.check || !host ? '' : '（尚未预检）'}</h3>
        ${h.check || !host ? chk.map(i => `<div class="notice ${i.r === 'pass' ? 'ok' : i.r === 'warn' ? 'warn' : 'bad'}"><span>${{pass: '✔', warn: '⚠', fail: '✖'}[i.r]}</span><div><b>${i.l}</b> · ${i.d}${i.r !== 'pass' ? (i.fix ? ' <span class="tag info">可自动修复</span>' : ' <span class="tag">需手动处理</span>') : ''}</div></div>`).join('') : '<div class="empty">请先点击「预检」</div>'}
        ${code(`tiup cluster check ${CLUSTER} scale-out.yml --cluster --user tidb\n# 自动修复可修复项\ntiup cluster check ${CLUSTER} scale-out.yml --cluster --user tidb --apply`)}`);
      break;
    }
    case 'host-join': if (host) { so.role = host.checkRole && ROLE_IDS.includes(host.checkRole) ? host.checkRole : roleGuess(host); so.hosts = [host]; so.step = 2; so.cfg = {}; so.ack = false; st.tab = 'scaleout'; pages.nodes.body(); } break;
    case 'host-remove': if (REAL()) { if (host) hpDelDrawer(ip); break; }
      if (host) { HOST_POOL.splice(HOST_POOL.indexOf(host), 1); so.hosts = so.hosts.filter(h => h !== host); toast(`${ip} 已移出主机池`); pages.nodes.body(); } break;
    case 'mt-start': mtStart(ip); break;
    case 'host-add-toggle': case 'host-add-toggle2': if (REAL()) { hpFormOpen('add'); break; } so.addOpen = !so.addOpen; pages.nodes.body(); break;
    case 'host-add-submit': {
      const ha = st.hostAdd;
      if (!/^(\d{1,3}\.){3}\d{1,3}$/.test(ha.ip) || ha.ip.split('.').some(x => +x > 255)) { toast('IP 格式不正确'); break; }
      if (HOST_POOL.some(h => h.ip === ha.ip) || NODES.some(n => n.host === ha.ip)) { toast('该主机已存在于主机池或集群中'); break; }
      const sp = SPEC[ha.use], odd = +ha.ip.split('.')[3] % 2 === 1;
      const h = {ip: ha.ip, az: ha.az, rack: ha.rack || 'r00', ...sp, os: 'Rocky Linux 9.3', note: '新增主机', checked: false, preset: odd ? {swap: ['fail', 'Swap 已开启（2.0 GB）', true], thp: ['warn', '透明大页为 always', true]} : {}};
      HOST_POOL.push(h); st.hostAdd = {ip: '', az: 'az1', rack: '', use: 'tikv'}; so.addOpen = false; toast(`${h.ip} 已添加，开始预检`); pages.nodes.body(); runCheck(h); break;
    }
    case 'so-role': so.role = d.role; if (so.step === 1) { so.step = 2; so.hosts = []; so.cfg = {}; } pages.nodes.body(); break;
    case 'so-next': so.step = Math.min(4, so.step + 1); pages.nodes.body(); break;
    case 'so-prev': so.step = Math.max(1, so.step - 1); pages.nodes.body(); break;
    case 'so-submit': { const plan = buildScaleOut(so.role, so.hosts, so.cfg); if (OP.on) { afterSubmit(plan); break; }   // 运维模式：取消提交时保留向导状态，提交成功后由 opGoTasks 重置
      so.step = 1; so.role = null; so.hosts = []; so.cfg = {}; so.ack = false; afterSubmit(plan); break; }
    case 'sf': si.filter = d.r; pages.nodes.body(); break;
    case 'si-next': si.step = Math.min(3, si.step + 1); pages.nodes.body(); break;
    case 'si-prev': si.step = Math.max(1, si.step - 1); pages.nodes.body(); break;
    case 'si-submit': { const ev = evalScaleIn(si.picks); if (ev.blocked) break; const plan = buildScaleIn(ev.picks); if (OP.on) { afterSubmit(plan); break; } nodesState.si = {step: 1, picks: [], filter: 'all', confirm: ''}; afterSubmit(plan); break; }
    case 'task-abort': { const t = TASKS.find(x => x.id === d.id); if (t) abortTask(t); break; }
  }
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc]'); if (!el) return;
  const d = el.dataset, so = nodesState.so, si = nodesState.si;
  switch (d.nc) {
    case 'so-pick': { const h = hostByIp(d.ip); if (!h) break; if (el.checked) { so.hosts = so.role === 'pd' ? [h] : [...so.hosts, h]; } else so.hosts = so.hosts.filter(x => x !== h); so.cfg = {}; pages.nodes.body(); break; }
    case 'so-lb': so.cfg.lb = el.value; break;
    case 'so-ack': so.ack = el.checked; pages.nodes.body(); break;
    case 'si-pick': si.picks = el.checked ? [...si.picks, d.id] : si.picks.filter(x => x !== d.id); pages.nodes.body(); break;
    case 'hp-az': if (HP.form) { HP.form.azSel = el.value; const c = $('#hpAzC'); if (c) { c.style.display = el.value === '__custom' ? '' : 'none'; if (el.value === '__custom') c.focus(); } } break;
    case 'hp-role': HP.askRole = el.value; break;
    case 'ha-az': nodesState.hostAdd.az = el.value; break;
    case 'ha-use': nodesState.hostAdd.use = el.value; break;
  }
});
document.addEventListener('input', e => {
  const el = e.target.closest('[data-ni]'); if (!el) return;
  const d = el.dataset, st = nodesState;
  switch (d.ni) {
    case 'q': st.q = el.value; pages.nodes.body(); break;
    case 'so-cfg': st.so.cfg[d.k] = el.value; break;
    case 'si-confirm': st.si.confirm = el.value; { const b = $('#siGo'); if (b) b.disabled = !opDenied('scale') && el.value !== '确认缩容'; } break;
    case 'hp-ip': if (HP.form) HP.form.ip = el.value.trim(); break;
    case 'hp-azc': if (HP.form) HP.form.azCustom = el.value; break;
    case 'hp-rack': if (HP.form) HP.form.rack = el.value; break;
    case 'hp-note': if (HP.form) HP.form.note = el.value; break;
    case 'ha-ip': st.hostAdd.ip = el.value.trim(); break;
    case 'ha-rack': st.hostAdd.rack = el.value.trim(); break;
    case 'drawer-confirm': { const b = $('#drRun'); if (b) b.disabled = el.value.trim() !== '确认'; } break;
  }
});
document.addEventListener('toggle', e => { const t = e.target; if (t.dataset && t.dataset.tid) { t.open ? openCmds.add(t.dataset.tid) : openCmds.delete(t.dataset.tid); } }, true);

