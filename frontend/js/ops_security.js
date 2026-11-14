// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_security.js
// @time: 2026/10/28

/* 运维模式下的「权限与审计」页：真实审计日志 + 哈希链校验 + 导出、我的账号（口令 / MFA）、写操作开关、部署自检。
   用户管理已迁到「平台配置 → 用户管理」（ops_users.js），权限矩阵 / 权限模拟器已迁到「平台配置 → 角色权限」（ops_roles.js）。
   OP.on === false 时 pages.security 走 security.js 原型实现，行为不变。命令白名单仍是静态规格演示（待确认提案与审批策略见 ops_governance.js）。
   data-na 前缀：osec-。依赖 ops.js（OP / opApi / opCan 等）、security.js（secState）、ops_pages.js（oppConfirm）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview') === h;
  const me = () => (OP.me && OP.me.user && OP.me.user.name) || '';
  const roleLabel = id => (window.opRoleName ? window.opRoleName(id) : id);
  const _sec = pages.security;
  // 审计条目的 cluster 是 tiup 集群名（可能与平台集群 id 不同，如 id=stg / name=stg-tidb-01）：按 id 或 name 找集群（name 只有管理员的 /api/clusters 才返回）
  const clKnown = k => CL.info(k) || ((CL.full && CL.full.clusters) || []).find(c => c.id === k || (c.config && c.config.name === k)) || null;

  const osec = {
    audit: {entries: [], loaded: false, loading: false, err: '', verify: null, f: {user: '', action: '', result: '', q: '', since: '', limit: '200', cluster: ''}},
    selfcheck: null, selfErr: '', totp: null,
  };
  const tabsList = () => [['policy', '审批策略'], opCan('audit.view') ? ['audit', '审计日志'] : null, ['wl', '命令白名单'],
    ['wswitch', '写操作开关'], ['check', '部署自检'], ['account', '我的账号']].filter(Boolean);

  /* ---------------- 审计日志 ---------------- */
  const SINCE = {'1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3, '30d': 30 * 86400e3};
  const RES = {allowed: ['info', '允许'], denied: ['warn', '拒绝'], success: ['ok', '成功'], failed: ['bad', '失败']};
  async function auditLoad() {
    const A = osec.audit, f = A.f; A.loading = true; A.err = '';
    const q = new URLSearchParams();
    if (f.user.trim()) q.set('user', f.user.trim());
    if (f.action.trim()) q.set('action', f.action.trim());
    if (f.result) q.set('result', f.result);
    if (f.cluster) q.set('cluster', f.cluster);   // 审计是全局的：按条目的 cluster 字段筛选（空 = 全部集群）
    if (f.q.trim()) q.set('q', f.q.trim());
    if (f.since) q.set('since', new Date(Date.now() - SINCE[f.since]).toISOString());
    q.set('limit', f.limit || '200');
    paintIf('audit');
    const r = await opApi('GET', '/api/audit?' + q.toString());
    A.loading = false; A.loaded = true;
    if (r.code === 200) A.entries = r.data.entries || []; else { A.entries = []; A.err = r.data.error || ('读取失败（' + r.code + '）'); }
    paintIf('audit');
  }
  async function auditVerify() {
    const r = await opApi('GET', '/api/audit/verify');
    osec.audit.verify = r.code === 200 ? {ok: !!r.data.ok, entries: r.data.entries || 0, error: r.data.error || '', at: Date.now()} : {ok: false, entries: 0, error: r.data.error || '校验请求失败', at: Date.now(), reqFail: true};
    paintIf('audit');
  }
  function auditHtml() {
    const A = osec.audit, f = A.f, v = A.verify, acts = [...new Set(A.entries.map(e => e.action))].sort();
    const banner = v ? (v.ok ? `<div class="notice ok"><span>✔</span><div><b>哈希链完整</b>：共 ${v.entries} 条记录全部校验通过（${fDT(v.at)}）</div></div>`
      : `<div class="notice bad"><span>✖</span><div><b>${v.reqFail || !v.entries ? '校验失败' : `哈希链断裂：第 ${v.entries} 条被篡改或缺失`}</b>${v.error ? '：' + E(v.error) : ''}。真实环境此时应立即告警并核查审计文件，部署自检也会标红。</div></div>`)
      : '<div class="small muted" style="margin-bottom:8px">点击「校验哈希链」：后端从头重算整条链，任何记录被修改、删除或插入都会指出第一处断裂的序号。</div>';
    return `<div class="card"><h2>审计日志 <span class="sub">SHA-256 哈希链 · 只追加${A.loaded ? ` · 本次查询 ${A.entries.length} 条` : ''}</span>
      <span class="right"><button class="btn primary sm" data-na="osec-verify">校验哈希链</button>${opCan('audit.export') ? `<a class="btn sm" href="/api/audit/export?format=csv${f.cluster ? '&cluster=' + encodeURIComponent(f.cluster) : ''}" download="audit.csv" title="${f.cluster ? '仅导出所选集群的记录' : '导出全部集群的记录'}">导出 CSV</a><a class="btn sm" href="/api/audit/export?format=jsonl${f.cluster ? '&cluster=' + encodeURIComponent(f.cluster) : ''}" download="audit.jsonl" title="${f.cluster ? '仅导出所选集群的记录' : '导出全部集群的记录'}">导出 JSONL</a>` : ''}</span></h2>
      ${banner}
      <div class="row" style="margin-bottom:10px;align-items:flex-end">
        <div class="field" style="margin:0;width:120px"><label>用户</label><input type="text" data-ni="osec-af" data-k="user" value="${E(f.user)}" placeholder="如 dba"></div>
        <div class="field" style="margin:0;width:170px"><label>动作</label><input type="text" list="osecActs" data-ni="osec-af" data-k="action" value="${E(f.action)}" placeholder="如 change.create"><datalist id="osecActs">${acts.map(a => `<option value="${E(a)}">`).join('')}</datalist></div>
        ${CL.list.length ? `<div class="field" style="margin:0;width:150px"><label>集群</label><select data-nc="osec-af" data-k="cluster"><option value="">全部集群</option>${CL.list.map(c => `<option value="${E(c.id)}" ${f.cluster === c.id ? 'selected' : ''}>${E(c.display || c.id)}</option>`).join('')}${[...new Set(A.entries.map(e => e.cluster).concat(f.cluster ? [f.cluster] : []).filter(id => id && !clKnown(id)))].sort().map(id => `<option value="${E(id)}" ${f.cluster === id ? 'selected' : ''}>${E(id)}（历史记录 / 已删除）</option>`).join('')}</select></div>` : ''}
        <div class="field" style="margin:0;width:110px"><label>结果</label><select data-nc="osec-af" data-k="result"><option value="">全部</option>${Object.entries(RES).map(([k, v2]) => `<option value="${k}" ${f.result === k ? 'selected' : ''}>${v2[1]}</option>`).join('')}</select></div>
        <div class="field" style="margin:0;width:130px"><label>时间范围</label><select data-nc="osec-af" data-k="since">${[['', '全部'], ['1h', '最近 1 小时'], ['24h', '最近 24 小时'], ['7d', '最近 7 天'], ['30d', '最近 30 天']].map(([k, t]) => `<option value="${k}" ${f.since === k ? 'selected' : ''}>${t}</option>`).join('')}</select></div>
        <div class="field" style="margin:0;width:200px"><label>关键字</label><input type="text" data-ni="osec-af" data-k="q" value="${E(f.q)}" placeholder="目标 / 摘要 / IP …"></div>
        <div class="field" style="margin:0;width:90px"><label>条数</label><select data-nc="osec-af" data-k="limit">${['100', '200', '500', '1000'].map(n => `<option ${f.limit === n ? 'selected' : ''}>${n}</option>`).join('')}</select></div>
        <button class="btn primary" data-na="osec-query" ${A.loading ? 'disabled' : ''}>${A.loading ? '查询中…' : '查询'}</button></div>
      ${A.err ? `<div class="notice bad"><span>✖</span><div>${E(A.err)}</div></div>` : ''}
      <div class="tbl-wrap" style="max-height:560px"><table><thead><tr><th>#</th><th>时间</th><th>用户</th><th>角色</th><th>环境</th><th>集群</th><th>动作</th><th>目标</th><th>风险</th><th>摘要</th><th>结果</th><th>哈希</th></tr></thead><tbody>
      ${A.entries.map(e => { const r = RES[e.result] || ['', e.result]; return `<tr class="${e.result === 'failed' ? 'flag' : ''}"><td>${e.seq}</td><td class="mono small" style="white-space:nowrap">${fDT(e.time)}</td><td>${E(e.user)}</td><td class="small">${E(e.role)}</td><td class="small">${E(e.env)}</td><td class="small mono" title="${E((clKnown(e.cluster) || {}).display || '')}">${e.cluster ? E(e.cluster) : '<span class="muted">—</span>'}</td><td class="mono small">${E(e.action)}</td><td class="small">${E(e.target)}</td><td>${e.risk ? opRiskTag(e.risk) : ''}</td><td class="small" style="max-width:300px">${E(e.summary)}</td><td>${tg(r[1], r[0])}</td><td class="mono small muted" title="${E(e.hash)}">${E((e.hash || '').slice(0, 10))}…</td></tr>`; }).join('') || `<tr><td colspan="12" class="empty">${A.loading ? '正在读取…' : '没有符合条件的记录'}</td></tr>`}</tbody></table></div>
      <div class="small muted" style="margin-top:8px">记录范围：登录 / 读写请求（含被拒绝）/ 审批 / 设置修改 / 写开关变化 / 执行器每条命令。结果取值：允许 / 拒绝 / 成功 / 失败。导出本身也会写入审计。</div></div>`;
  }

  /* ---------------- 写操作开关 ---------------- */
  function wsHtml() {
    const w = OP.write || {}, p = w.pending, can = opCan('writeswitch'), dis = can ? '' : 'disabled';
    const expired = p && (opNow() - Date.parse(p.at) > 3600e3), mine = p && p.by === me(), on = !!w.enabled;
    let acts = '';
    if (!on && (!p || !p.enabled)) acts = `<button class="btn primary" data-na="osec-ws-on" ${dis}>提议开启写操作</button>`;
    else if (!on) acts = (mine && !expired ? '' : expired ? `<button class="btn primary" data-na="osec-ws-on" ${dis}>重新提议开启</button>` : `<button class="btn primary" data-na="osec-ws-on" ${dis}>确认开启写操作</button>`)
      + `<button class="btn" data-na="osec-ws-cancel" ${dis}>撤销提议</button>`;
    else acts = `<button class="btn danger" data-na="osec-ws-off" ${dis}>立即关闭（熔断）</button>`;
    return `<div class="grid g2"><div class="card"><h2>写操作开关 <span class="sub">${CL.active() ? '集群 ' + E(CL.name()) + ' · ' : ''}${E(OP.env)} 环境</span><span class="right">${on && !w.reason ? tg('写操作已开启', 'ok') : tg('只读', 'warn')}</span></h2>
      <div class="kv"><span>开关</span><span>${on ? '已开启' : '已关闭'}</span><span>当前不能写的原因</span><span>${w.reason ? E(w.reason) : '—（可以提交变更）'}</span>
        <span>READONLY 熔断</span><span>${w.readOnlyForced ? tg('文件存在：强制只读', 'bad') : '未触发'}</span>
        <span>待确认提议</span><span>${p ? `${E(p.by)} 于 ${fDT(p.at)} 提议${p.enabled ? '开启' : '关闭'}${expired ? ' ' + tg('已超时（1 小时）', 'warn') : ''}` : '无'}</span>
        <span>执行器</span><span>${E(w.exec || '—')}${w.sim ? ' ' + tg('模拟：不会对集群做变更', 'warn') : ''}</span></div>
      ${can ? '' : '<div class="notice warn"><span>⚠</span><div>只有管理员（writeswitch 能力）可以操作写开关；当前账号只能查看状态。</div></div>'}
      ${p && mine && !expired && !on ? '<div class="notice info"><span>ℹ</span><div>你已提议开启，需要<b>另一名管理员</b>登录后确认；同一人不能自己确认。提议 1 小时内有效。</div></div>' : ''}
      <div class="sticky-actions" style="position:static">${acts}</div></div>
      <div class="card"><h2>规则</h2><ul class="small" style="padding-left:18px;margin:0;display:grid;gap:6px"><li>生产 / 预发默认关闭写操作；开启需要<b>两名不同的管理员</b>先后确认（双人复核）</li><li>关闭（熔断）只需一名管理员，立即生效，并清除未决提议</li><li>写操作关闭时，所有会改变集群的提交（变更、告警静默）都会被后端拒绝并说明原因</li><li>中控机上存在 READONLY 文件时强制只读，与开关无关</li><li>集群版本不在支持矩阵内时，写操作自动降级为只读</li><li>每次提议 / 确认 / 撤销 / 关闭都写入审计</li></ul></div></div>`;
  }
  async function wsCall(path, body, okMsg) {
    const r = await opApi('POST', path, body);
    if (r.code === 200) { toast(typeof okMsg === 'function' ? okMsg(r.data) : okMsg); } else toast(r.data.error || '操作失败');
    await opRefresh(true); paintIf('wswitch');
  }

  /* ---------------- 部署自检 ---------------- */
  async function selfLoad() {
    const r = await opApi('GET', '/api/ops/selfcheck');
    if (r.code === 200) { osec.selfcheck = r.data.items || []; osec.selfErr = ''; } else osec.selfErr = r.data.error || '读取失败';
    paintIf('check');
  }
  function checkHtml() {
    const it = osec.selfcheck, bad = it ? it.filter(x => x.r === 'bad').length : 0, warn = it ? it.filter(x => x.r === 'warn').length : 0;
    return `<div class="card"><h2>部署自检 <span class="sub">后端实时检查平台自身的部署安全基线</span><span class="right"><button class="btn primary sm" data-na="osec-selfcheck">重新自检</button></span></h2>
      ${osec.selfErr ? `<div class="notice bad"><span>✖</span><div>${E(osec.selfErr)}</div></div>` : ''}
      ${it ? (bad ? `<div class="notice bad"><span>✖</span><div><b>${bad} 项未通过</b>：请先修复再开启写操作。</div></div>` : warn ? `<div class="notice warn"><span>⚠</span><div>关键项通过，另有 ${warn} 项警告（如模拟执行器 / Windows 运行环境），生产前请处理。</div></div>` : '<div class="notice ok"><span>✔</span><div>全部通过。</div></div>') : '<div class="empty">正在自检…</div>'}
      ${it ? `<div class="tbl-wrap"><table><thead><tr><th>检查项</th><th>结果</th><th>详情</th></tr></thead><tbody>${it.map(c => `<tr class="${c.r === 'bad' ? 'flag' : ''}"><td>${E(c.n)}</td><td>${tg({ok: '通过', warn: '警告', bad: '失败'}[c.r] || c.r, c.r)}</td><td class="small mono">${E(c.d)}</td></tr>`).join('')}</tbody></table></div>` : ''}</div>`;
  }

  /* ---------------- 我的账号 ---------------- */
  function accountHtml() {
    const u = OP.me.user, caps = Object.keys(OP.me.caps || {}), T = osec.totp;
    return `<div class="grid g2"><div class="grid" style="align-content:start"><div class="card"><h2>我的账号</h2>
        <div class="kv"><span>用户名</span><span class="mono">${E(u.name)}</span><span>显示名</span><span>${E(u.display)}</span>${CL.active() ? `<span>当前集群</span><span>${E(CL.name())} <span class="muted mono small">${E(CL.cur)}</span></span>` : ''}<span>当前环境</span><span>${E(OP.env)}</span><span>角色</span><span>${(OP.me.roles || []).map(r => tg(roleLabel(r), 'info')).join(' ') || '无'}</span><span>MFA</span><span>${u.hasTotp ? tg('已绑定', 'ok') : tg('未绑定', 'warn')}</span></div>
        <div class="hr"></div><div class="small muted" style="margin-bottom:6px">当前环境下拥有的能力（${caps.length}）</div><div class="row" style="gap:4px">${caps.map(c => `<span class="tag" title="${E(OP.me.caps[c])}">${E(c)}</span>`).join('')}</div></div></div>
      <div class="grid" style="align-content:start"><div class="card"><h2>修改口令</h2>
        <div class="field"><label>当前口令</label><input type="password" id="osecPw0" autocomplete="current-password"></div>
        <div class="field"><label>新口令（≥10 位、≥3 类字符、不含用户名）</label><input type="password" id="osecPw1" autocomplete="new-password"></div>
        <div class="field"><label>确认新口令</label><input type="password" id="osecPw2" autocomplete="new-password"></div>
        <button class="btn primary" data-na="osec-pw">修改口令</button></div>
      <div class="card"><h2>多因素认证（TOTP）</h2>
        ${u.hasTotp ? '<div class="notice ok"><span>✔</span><div>已绑定动态验证码。生产高风险变更提交时需要输入。如需重置，请联系管理员（用户管理 → 重置 TOTP）。</div></div>'
        : !T ? '<div class="small muted" style="margin-bottom:8px">绑定后，提交生产高风险变更需要输入 6 位动态验证码。点击开始后会生成密钥，请在认证器 App（如 Google Authenticator）中手动添加。</div><button class="btn primary" data-na="osec-totp-begin">开始绑定</button>'
        : `<div class="field"><label>密钥（Base32，手动输入到认证器）</label><div class="row"><code class="mono" style="font-size:14px;user-select:all">${E(T.secret)}</code><button class="btn sm" data-copy="${E(T.secret)}">复制</button></div></div>
          <div class="field"><label>otpauth URI（也可粘贴到支持的 App）</label><pre class="code mono" style="word-break:break-all;white-space:pre-wrap;user-select:all">${E(T.uri)}</pre></div>
          <div class="field"><label>输入认证器当前显示的 6 位码以确认</label><input type="text" id="osecTotp" inputmode="numeric" maxlength="6" autocomplete="one-time-code"></div>
          <div class="row"><button class="btn primary" data-na="osec-totp-ok">确认绑定</button><button class="btn" data-na="osec-totp-cancel">放弃</button></div>`}</div></div></div>`;
  }

  /* ---------------- 页面包装 ---------------- */
  function tabHtml(t) {
    if (t === 'audit') return auditHtml();
    if (t === 'wswitch') return wsHtml();
    if (t === 'check') return checkHtml();
    if (t === 'account') return accountHtml();
    return _sec[t].call(pages.security);
  }
  function enter(t) {
    if (t === 'audit' && !osec.audit.loaded && !osec.audit.loading) auditLoad();
    else if (t === 'check') selfLoad();
    else if (t === 'wswitch') opRefresh(true);
  }
  function paintIf(t) { if (OP.on && hashIs('security') && secState.tab === t && $('#secBody')) pages.security.body(); }

  pages.security = {
    ..._sec,
    render() {
      if (!OP.on) return _sec.render.call(this);
      return `<div class="tabs" id="secTabs">${tabsList().map(([k, t]) => `<button data-na="osec-tab" data-t="${k}" class="${secState.tab === k ? 'active' : ''}">${t}</button>`).join('')}</div><div id="secBody"></div>`;
    },
    async init() {
      if (!OP.on) return _sec.init.call(this);
      const na = window.navArgs; window.navArgs = null;
      if (na && na.tab && tabsList().some(t => t[0] === na.tab)) secState.tab = na.tab;
      if (!tabsList().some(t => t[0] === secState.tab)) secState.tab = tabsList()[0][0];
      this.body(); enter(secState.tab);
    },
    body() {
      if (!OP.on) return _sec.body.call(this);
      const el = $('#secBody'); if (!el) return;
      if (!tabsList().some(t => t[0] === secState.tab)) secState.tab = tabsList()[0][0];
      const h = tabHtml(secState.tab); if (secState.tab === 'wswitch') osec.lastWs = h;
      el.innerHTML = h;
      $$('#secTabs button').forEach(b => b.classList.toggle('active', b.dataset.t === secState.tab));
    },
  };

  /* ---------------- 事件 ---------------- */
  const CLICK = {
    'osec-tab': d => { secState.tab = d.t; pages.security.body(); enter(d.t); },
    'osec-query': () => auditLoad(),
    'osec-verify': () => auditVerify(),
    'osec-ws-on': () => wsCall('/api/ops/write-switch', {enabled: true}, r => r.enabled ? '写操作已开启' : '已提议开启，等待另一名管理员确认'),
    'osec-ws-cancel': () => wsCall('/api/ops/write-switch/cancel', {}, '已撤销提议'),
    'osec-ws-off': () => oppConfirm('立即关闭写操作（熔断）', `关闭后，所有变更提交与告警静默都会被拒绝，直到再次由两名管理员确认开启。已在执行中的变更不会被中断。`, '立即关闭', () => wsCall('/api/ops/write-switch', {enabled: false}, '写操作已关闭'), true),
    'osec-selfcheck': () => { osec.selfcheck = null; paintIf('check'); selfLoad(); },
    'osec-pw': async () => {
      const a = $('#osecPw0').value, b = $('#osecPw1').value, c = $('#osecPw2').value;
      if (!a || !b) { toast('请填写当前口令和新口令'); return; }
      if (b !== c) { toast('两次输入的新口令不一致'); return; }
      if (b.length < 10) { toast('新口令至少 10 位'); return; }
      const r = await opApi('POST', '/api/auth/password', {old: a, new: b});
      if (r.code === 200) { toast('口令已修改'); ['osecPw0', 'osecPw1', 'osecPw2'].forEach(i => { $('#' + i).value = ''; }); } else toast(r.data.error || '修改失败');
    },
    'osec-totp-begin': async () => { const r = await opApi('POST', '/api/auth/totp/begin', {}); if (r.code === 200) { osec.totp = {secret: r.data.secret, uri: r.data.uri}; paintIf('account'); } else toast(r.data.error || '无法开始绑定'); },
    'osec-totp-cancel': () => { osec.totp = null; paintIf('account'); },
    'osec-totp-ok': async () => {
      const code = $('#osecTotp').value.trim(); if (!/^\d{6}$/.test(code)) { toast('请输入 6 位数字验证码'); return; }
      const r = await opApi('POST', '/api/auth/totp/confirm', {code});
      if (r.code === 200) { osec.totp = null; const m = await opApi('GET', '/api/auth/me'); if (m.code === 200 && m.data.authenticated) opApplyMe(m.data); toast('MFA 已绑定'); paintIf('account'); } else toast(r.data.error || '验证码不正确');
    },
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-nc]'); if (!el) return; const d = el.dataset;
    if (d.nc === 'osec-af') { osec.audit.f[d.k] = el.value; if (d.k === 'cluster') paintIf('audit'); }   // 导出链接带着筛选的集群：重绘以更新链接
  });
  document.addEventListener('input', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-ni]'); if (!el) return;
    if (el.dataset.ni === 'osec-af') osec.audit.f[el.dataset.k] = el.value;
  });
  document.addEventListener('keydown', e => {
    if (!OP.on || e.key !== 'Enter') return;
    if (e.target.closest && e.target.closest('[data-ni="osec-af"]')) auditLoad();
  });

  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || h !== 'security' || ctx.open || ctx.typing) return;
    if (secState.tab === 'wswitch' && $('#secBody') && wsHtml() !== osec.lastWs) pages.security.body();   // 仅在内容变化时重绘
  });
})();
