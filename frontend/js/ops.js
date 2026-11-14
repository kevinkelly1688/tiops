// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops.js
// @time: 2026/10/28

/* 运维模式核心：登录 / 会话 / 接口客户端 / 变更提交与审批。
   只有后端以 --data-dir 启用运维模式（/api/health 返回 ops.enabled）时才生效；否则 OP.on=false，其余页面仍是原型模拟。
   其他脚本（nodes.js / config.js / maint.js / ops_*.js）通过下面的公共接口使用：
     OP.on / OP.me / OP.changes / OP.maints / OP.write
     opCan(cap)                      当前用户是否有某能力（caps 取自 /api/auth/me）
     opApi(method, path, body)       调用后端，返回 {code, data}；401 会自动弹出登录
     opIntentDrawer(intent, title)   服务端规划 → 展示检查 / 命令 / 审批级数 → 提交变更（原型里的 planDrawer / startTask 在运维模式下改走这里）
     opRefresh(force)                重新拉取变更与维护记录并刷新相关页面
     opEsc / opFmtTime / opStateTag  小工具 */
const OP = {on: false, env: '', icp: '', me: null, csrf: '', write: null, changes: [], maints: [], maintTypes: {}, summary: {}, serverSkew: 0, timer: null, loaded: false, loginResolve: null};

const opEsc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
const opFmtTime = t => { if (!t) return '—'; const d = new Date(t); const p = n => String(n).padStart(2, '0'); return `${d.getMonth() + 1}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`; };
const OP_STATE = {pending: ['info', '待审批'], approved: ['info', '排队中'], scheduled: ['warn', '已排期'], running: ['warn', '执行中'], succeeded: ['ok', '成功'], failed: ['bad', '失败'], aborted: ['', '已中止'], rejected: ['', '已驳回'], cancelled: ['', '已取消'], expired: ['', '已过期']};
const opStateTag = s => { const [c, t] = OP_STATE[s] || ['', s]; return `<span class="tag ${c}">${t}</span>`; };
const OP_RISK = {low: ['ok', '低风险'], mid: ['warn', '中风险'], high: ['bad', '高风险']};
const opRiskTag = r => { const [c, t] = OP_RISK[r] || ['', r]; return `<span class="tag ${c}">${t}</span>`; };
const opCan = cap => !!(OP.on && OP.me && OP.me.caps && OP.me.caps[cap]);
const opHasRole = r => !!(OP.on && OP.me && (OP.me.roles || []).includes(r));
/* 无权限的按钮：置灰但保持可点击（不加 disabled），悬停提示「权限不足」，点击后在按钮旁红字提示；真正的拦截仍在后端 */
const OP_DENY_ATTR = ' data-denied="1" aria-disabled="true" title="权限不足"';
let opDenyTimer = 0;
window.addEventListener('click', e => {
  const el = e.target.closest && e.target.closest('[data-denied]'); if (!el) return;
  e.preventDefault(); e.stopImmediatePropagation();
  let tip = document.getElementById('denyTip');
  if (!tip) { tip = document.createElement('div'); tip.id = 'denyTip'; tip.setAttribute('role', 'alert'); document.body.appendChild(tip); }
  tip.textContent = '权限不足'; tip.classList.add('show');
  const r = el.getBoundingClientRect();
  tip.style.left = Math.max(8, Math.min(window.innerWidth - 100, r.left)) + 'px'; tip.style.top = Math.min(window.innerHeight - 36, r.bottom + 6) + 'px';
  clearTimeout(opDenyTimer); opDenyTimer = setTimeout(() => tip.classList.remove('show'), 2500);
}, true);

async function opApi(method, path, body) {
  const opt = {method, cache: 'no-store', headers: {}};
  if (method !== 'GET') { opt.headers['Content-Type'] = 'application/json'; if (OP.csrf) opt.headers['X-CSRF-Token'] = OP.csrf; if (body !== undefined) opt.body = JSON.stringify(body); }
  let r; try { r = await fetch(path, opt); } catch (e) { return {code: 0, data: {error: '无法连接后端：' + e.message}}; }
  let data = null; const ct = r.headers.get('content-type') || '';
  try { data = ct.includes('json') ? await r.json() : {_raw: await r.text()}; } catch (e) { data = {}; }
  if (r.status === 401 && path !== '/api/auth/login' && path !== '/api/auth/me') { const had = !!OP.me; OP.me = null; opShowLogin(had ? '登录已过期或已在别处退出，请重新登录' : ''); }
  return {code: r.status, data: data || {}};
}

/* ---------------- 登录 ---------------- */
/* 登录页见 login.js（window.opShowLogin(msg)） */

function opApplyMe(me) {
  OP.me = me; OP.csrf = me.csrf || ''; OP.write = me.write || null; OP.env = me.env || OP.env;
  if (window.CL) CL.apply(me);   // 多集群：可见集群列表与当前集群（cluster.js）；me.env / write / caps 已是当前集群的值
  opRenderChip();
}

function opRenderChip() {
  let chip = document.getElementById('opChip');
  if (!chip) { chip = document.createElement('span'); chip.id = 'opChip'; const t = $('#themeBtn'); t.parentNode.insertBefore(chip, t); }
  if (!OP.me) { chip.innerHTML = ''; return; }
  const w = OP.write || {}, roles = (OP.me.roles || []).map(r => window.opRoleName ? window.opRoleName(r) : r).join('/') || '无角色';
  const envTag = {prod: ['bad', '生产'], staging: ['warn', '预发'], test: ['ok', '测试']}[OP.env] || ['', OP.env];
  const clTip = window.CL && CL.name() ? '集群 ' + CL.name() + ' 的环境' : '环境';
  chip.innerHTML = `<span class="tag ${envTag[0]}" title="${opEsc(clTip)}">${envTag[1]}</span>
    <span class="tag ${w.enabled && !w.reason ? 'ok' : 'warn'}" title="${opEsc(w.reason || '')}">${w.enabled && !w.reason ? '写操作已开启' : '只读'}</span>
    <span class="small" style="margin:0 8px"><b>${opEsc(OP.me.user.display || OP.me.user.name)}</b> · ${opEsc(roles)}</span>
    <button class="btn sm" data-na="op-logout">退出</button>`;
  if (window.CL) CL.renderTop();
  if (typeof LIVE !== 'undefined' && LIVE.on && LIVE.snap && typeof liveBanner === 'function') liveBanner();   // 写开关状态变了：顶栏「真实数据 · 只读 / 读写」随之刷新
}

async function opLogout() { await opApi('POST', '/api/auth/logout'); OP.me = null; opRenderChip(); opShowLogin(); }

/* ---------------- 启动 ---------------- */
async function opInit() {
  try {
    const r = await fetch('/api/health', {cache: 'no-store'});
    if (!r.ok) return;
    const h = await r.json();
    if (h && typeof h.icp === 'string') OP.icp = h.icp;   // 备案号（配置文件 icp）：只读模式 / 登录页 / 页脚都要用，放在运维模式判断之前
    if (!h || !h.ops || !h.ops.enabled) return;
    OP.on = true; OP.env = h.ops.env; OP.version = h.version || ''; OP.demo = !!h.ops.demo;
    OP.standalone = !!h.standalone; if (window.CL) { CL.standalone = OP.standalone; CL.localTiup = !!h.localTiup; }   // 独立部署：平台不代表任何集群；localTiup：本机是 tiup 中控机，可一键导入
  } catch (e) { return; }
  const {code, data} = await opApi('GET', '/api/auth/me');
  if (code === 200 && data.authenticated) { opApplyMe(data); if (window.CL) await CL.sync(); await opRefresh(true); opStartPolling(); return; }
  await new Promise(res => { OP.loginResolve = res; opShowLogin(); });
  if (window.CL) await CL.sync();
  await opRefresh(true); opStartPolling();
}

// 登录发生在页面已加载之后（会话过期再登录）
async function opAfterLogin() {
  if (window.CL) { await CL.sync(); if (CL.reloadIfChanged()) return; }   // 换了用户后选定的集群不同：页面数据属于上一个集群，直接刷新
  if (typeof liveRefresh === 'function') await liveRefresh(true);
  await opRefresh(true); opStartPolling();
}

function opStartPolling() {
  if (OP.timer) return;
  const tick = async () => { await opRefresh(false); OP.timer = setTimeout(tick, OP.changes.some(c => ['running', 'approved'].includes(c.state)) ? 2000 : 8000); };
  OP.timer = setTimeout(tick, 2000);
}

/* ---------------- 变更与维护数据 ---------------- */
async function opRefresh(force) {
  if (!OP.on || !OP.me) return;
  if (!(window.CL && CL.empty())) {   // 没有任何可见集群（独立部署尚未接入）时没有变更 / 维护可取
    const a = await opApi('GET', '/api/changes');
    if (a.code === 200) { OP.changes = a.data.changes || []; OP.summary = a.data.summary || {}; if (a.data.serverTime) OP.serverSkew = new Date(a.data.serverTime) - Date.now(); }
    const m = await opApi('GET', '/api/maints');
    if (m.code === 200) { OP.maints = m.data.maints || []; OP.maintTypes = m.data.types || {}; }
  } else { OP.changes = []; OP.maints = []; OP.summary = {}; }
  const me = await opApi('GET', '/api/auth/me');
  if (me.code === 200 && me.data.authenticated) {
    OP.write = me.data.write; if (me.data.env) OP.env = me.data.env;
    if (window.CL) CL.apply(me.data);   // 集群列表 / 环境 / 写开关随时间变化（管理员改了集群环境、接入或删除了集群）
    // 角色被管理员修改后，已登录用户的能力 / 角色立即跟随（后端按请求实时鉴权，前端缓存也要同步；页面重绘见 ops_users.js 的钩子）
    if (OP.me) { OP.me.caps = me.data.caps || {}; OP.me.roles = me.data.roles || []; OP.me.roleInfo = me.data.roleInfo || []; if (me.data.user) OP.me.user = me.data.user; }
    opRenderChip();
  }
  OP.loaded = true;
  opUpdateBadges();
  const h = location.hash.slice(1) || 'overview', open = $('#drawer') && $('#drawer').classList.contains('open');
  const typing = document.activeElement && ['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName);
  OP_HOOKS.forEach(f => { try { f(h, {force, open, typing}); } catch (e) { console.error(e); } });
}

function opUpdateBadges() {
  const live = OP.changes.filter(c => ['running', 'approved'].includes(c.state)).length;
  const b = $('#taskBadge'); if (b) { b.style.display = live ? '' : 'none'; b.textContent = live; }
  const pend = OP.changes.filter(c => c.state === 'pending').length;
  const sb = $('#schedBadge'); if (sb) { sb.style.display = pend ? '' : 'none'; sb.textContent = pend; }
}

/* 各页面 / 模块注册数据刷新钩子：OP_HOOKS.push((hash, {force, open, typing}) => {...})，在变更 / 维护数据更新后被调用 */
const OP_HOOKS = [];
const opNow = () => Date.now() + OP.serverSkew;

/* ---------------- 提交变更：服务端规划 → 审阅 → 创建 ---------------- */
const opLvIcon = {ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'};
const opWinHtml = (checks, blocked) => ((checks || []).map(c => `<div class="notice ${c.lv === 'info' ? 'info' : c.lv}"><span>${opLvIcon[c.lv] || 'ℹ'}</span><div>${opEsc(c.t)}</div></div>`).join('') || '<div class="small muted">未命中任何窗口规则</div>')
  + (blocked ? '<div class="notice bad"><span>✖</span><div><b>窗口规则禁止（紧急变更除外）</b>：请改选其他时间（可点「推荐时间」），或勾选「紧急变更」。</div></div>' : '');
const opLocalInput = t => { const d = new Date(t), p = n => String(n).padStart(2, '0'); return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`; };
async function opIntentDrawer(intent, title) {
  const tok = opIntentDrawer.tok = (opIntentDrawer.tok || 0) + 1;   // 抽屉被新的提交面板顶替后，旧请求的结果不再写入
  openDrawer(title || '提交变更', '<div class="empty">正在由服务端规划并检查…</div>');
  const {code, data} = await opApi('POST', '/api/changes/preview', intent);
  if (tok !== opIntentDrawer.tok) return;
  if (code !== 200) { $('#drBody').innerHTML = `<div class="notice bad"><span>✖</span><div>${opEsc(data.error || '规划失败')}</div></div><div class="sticky-actions"><button class="btn" data-close="1">关闭</button></div>`; return; }
  const plan = data.plan, blocked = plan.blocked, gate = data.gate;
  const dtv = new Date(opNow() + 3600e3); const p2 = n => String(n).padStart(2, '0');
  const dflt = `${dtv.getFullYear()}-${p2(dtv.getMonth() + 1)}-${p2(dtv.getDate())}T${p2(dtv.getHours())}:00`;
  const needOtp = data.mfa, hasWin = Array.isArray(data.windowChecks);
  $('#drTitle').textContent = plan.title;
  $('#drBody').innerHTML = `
    <div class="row">${opRiskTag(plan.risk)}${window.CL && CL.active() ? `<span class="tag" title="本变更将提交到此集群">集群 ${opEsc(CL.name())}</span>` : ''}<span class="tag info">${opEsc(data.env)} 环境需 ${data.levels} 级审批</span>${plan.estMins ? `<span class="small muted">预计 ${plan.estMins} 分钟</span>` : ''}<a class="small" style="margin-left:auto" href="#help/${/^backup\./.test(intent.kind) ? 'ops' : 'nodes'}" target="_blank" rel="noopener" title="在新标签页打开使用帮助（不丢失当前表单）">操作说明与后果 ↗</a></div>
    ${gate ? `<div class="notice warn"><span>⚠</span><div>当前不能提交：${opEsc(gate)}</div></div>` : ''}
    <h3 style="font-size:14px;margin:14px 0 4px">前置检查（服务端）</h3>
    ${(plan.checks || []).map(c => `<div class="notice ${c.lv === 'info' ? 'info' : c.lv}"><span>${opLvIcon[c.lv] || 'ℹ'}</span><div>${opEsc(c.t)}</div></div>`).join('') || '<div class="small muted">无</div>'}
    ${hasWin ? `<h3 style="font-size:14px;margin:14px 0 4px">窗口检查 <span class="small muted" id="opWinBasis">按当前时间（审批通过后立即执行）</span></h3><div id="opWinBody">${opWinHtml(data.windowChecks, data.windowBlocked)}</div>` : ''}
    <h3 style="font-size:14px;margin:14px 0 4px">将执行的命令</h3><pre class="code mono">${opEsc((plan.display || []).join('\n') || '（无集群命令）')}</pre>
    <h3 style="font-size:14px;margin:14px 0 4px">执行步骤</h3><ol class="small">${(plan.steps || []).map(s => `<li>${opEsc(s.name)}${s.manual ? ' <span class="tag warn">需人工确认</span>' : ''}${s.irreversible ? ' <span class="tag">不可中止</span>' : ''}</li>`).join('')}</ol>
    ${blocked ? '' : `<div class="field"><label>变更说明 / 工单号</label><input type="text" id="opNote" placeholder="例如 OPS-1234 内核补丁"></div>
    <div class="field"><label>执行时间</label><select id="opWhen"><option value="now">审批通过后立即执行</option><option value="at">定时执行…</option></select><input type="datetime-local" id="opAt" value="${dflt}" style="display:none;margin-top:6px"><div class="op-rec" id="opRec"></div></div>
    <div class="field"><label><input type="checkbox" id="opEmg"> 紧急变更（可进入冻结期，审批更严，事后必须复盘）</label></div>
    ${needOtp ? '<div class="field"><label>申请人动态验证码（MFA）</label><input type="text" id="opOtp" inputmode="numeric" maxlength="6"></div>' : ''}`}
    <div id="opSubErr" class="small" style="color:var(--bad);min-height:18px"></div>
    <div class="sticky-actions"><button class="btn ${plan.risk === 'high' ? 'danger' : 'primary'}" id="opSubmit" ${blocked || gate || data.windowBlocked ? 'disabled' : ''}>${blocked ? '存在阻断项' : data.windowBlocked ? '窗口规则禁止' : '提交变更'}</button><button class="btn" data-close="1">取消</button></div>`;
  const sub = $('#opSubmit'), w = $('#opWhen');
  let winBlocked = !!data.windowBlocked, submitting = false, seq = 0, timer = null;
  const syncSub = () => { if (!sub || blocked || gate || submitting) return; sub.disabled = winBlocked; sub.textContent = winBlocked ? '窗口规则禁止' : '提交变更'; };
  const atValue = () => (w && w.value === 'at' && $('#opAt')) ? $('#opAt').value : '';
  // 选定时执行 / 改时间 / 勾紧急变更后，带上 scheduledAt 与 emergency 重新预检窗口规则（防抖 400ms）
  const winRefresh = delay => {
    if (!hasWin) return;
    const my = ++seq; clearTimeout(timer);
    timer = setTimeout(async () => {
      if (my !== seq || tok !== opIntentDrawer.tok || !$('#opWinBody')) return;
      const at = atValue(), emg = !!($('#opEmg') && $('#opEmg').checked), body = {kind: intent.kind, params: intent.params, emergency: emg};
      if (at) { const d = new Date(at); if (isNaN(d)) return; body.scheduledAt = d.toISOString(); }
      const r = await opApi('POST', '/api/changes/preview', body);
      if (my !== seq || tok !== opIntentDrawer.tok || !$('#opWinBody')) return;
      const bs = $('#opWinBasis'); if (bs) bs.textContent = (at ? '按计划时间 ' + at.replace('T', ' ') : '按当前时间（审批通过后立即执行）') + (emg ? ' · 紧急变更' : '');
      if (r.code !== 200) { $('#opWinBody').innerHTML = `<div class="notice warn"><span>⚠</span><div>窗口检查失败：${opEsc(r.data.error || '请求失败')}</div></div>`; return; }
      winBlocked = !!r.data.windowBlocked;
      $('#opWinBody').innerHTML = opWinHtml(r.data.windowChecks, winBlocked);
      syncSub();
    }, delay);
  };
  const markRec = () => { const at = atValue(), box = $('#opRec'); if (!box) return; box.querySelectorAll('[data-start]').forEach(b => b.classList.toggle('active', !!at && opLocalInput(b.dataset.start) === at)); };
  if (w) w.onchange = () => { $('#opAt').style.display = w.value === 'at' ? '' : 'none'; markRec(); winRefresh(0); };
  const at = $('#opAt'); if (at) { at.oninput = () => { markRec(); winRefresh(400); }; at.onchange = () => { markRec(); winRefresh(400); }; }
  const emg = $('#opEmg'); if (emg) emg.onchange = () => winRefresh(0);
  // 推荐时间：按窗口规则打分的候选时段，点击回填
  const rec = $('#opRec');
  if (rec) {
    const dm = t => { const d = new Date(t), p = n => String(n).padStart(2, '0'); return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`; };
    rec.innerHTML = '<span class="muted">正在计算推荐时间…</span>';
    opApi('POST', '/api/changes/recommend', {kind: intent.kind, params: intent.params, days: 7, count: 5}).then(r => {
      if (tok !== opIntentDrawer.tok || !$('#opRec')) return;
      if (r.code !== 200) { rec.innerHTML = `<span class="muted">推荐时间暂不可用：${opEsc(r.data.error || '请求失败')}</span>`; return; }
      const slots = r.data.slots || [];
      if (!slots.length) { rec.innerHTML = '<span class="muted">7 天内没有符合规则的时段</span>'; return; }
      rec.innerHTML = `<span class="muted">推荐时间：</span>${slots.map(sl => `<button type="button" class="chip" data-start="${opEsc(sl.start)}" title="${opEsc((sl.reasons || []).join('；') + (sl.end ? '（预计结束 ' + dm(sl.end) + '）' : ''))}">${dm(sl.start)} · 评分 ${opEsc(sl.score)}</button>`).join('')}`;
      markRec();
    });
    rec.onclick = e => {
      const b = e.target.closest('[data-start]'); if (!b) return;
      w.value = 'at'; at.style.display = ''; at.value = opLocalInput(b.dataset.start);
      markRec(); winRefresh(0);
    };
  }
  if (sub) sub.onclick = async () => {
    const wantAt = w && w.value === 'at', atD = wantAt ? new Date($('#opAt').value) : null;
    if (wantAt && isNaN(atD)) { $('#opSubErr').textContent = '请选择有效的执行时间'; return; }
    submitting = true; sub.disabled = true;
    const body = {kind: intent.kind, params: intent.params, note: $('#opNote').value, emergency: $('#opEmg').checked, otp: $('#opOtp') ? $('#opOtp').value : ''};
    if (wantAt) body.scheduledAt = atD.toISOString();
    const r = await opApi('POST', '/api/changes', body);
    if (r.code === 200) {
      closeDrawer(); await opRefresh(true);
      toast(r.data.state === 'pending' ? `变更 ${r.data.id} 已提交，等待审批` : r.data.state === 'scheduled' ? `变更 ${r.data.id} 已排期` : `变更 ${r.data.id} 已开始执行`);
      if (typeof opGoTasks === 'function') opGoTasks(r.data.id);
      return;
    }
    submitting = false; syncSub();
    const e = $('#opSubErr'); e.textContent = r.data.error || '提交失败';
    if (r.data.code === 'need_mfa_enroll') e.textContent += '。你尚未绑定 MFA，请先在「权限与审计」里绑定动态验证码。';
  };
}

document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el) return;
  if (el.dataset.na === 'op-logout') opLogout();
});
