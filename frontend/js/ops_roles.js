// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_roles.js
// @time: 2026/10/28

/* 平台配置 → 「角色权限」页（pages.roles）。
   运维模式：左栏角色列表（内置在前、自定义在后）+ 右侧详情（功能权限 / 成员 / 权限模拟器），顶部「对比矩阵」抽屉（全部角色 × 全部能力，可打印）。
     - 功能权限：按后端能力目录（GET /api/roles 的 catalog）分组；每项能力 = 中文名 + cap 码 + 风险点 + 说明 + 授予方式分段选择
       （无 / 允许 / 需审批 / 范围内，按 allowedGrants 显示）；仅内置管理员的能力（locked）不可选；查看总览必选；
       自定义角色可编辑，内置角色只读；保存前弹出变更差异确认（新增 / 移除 / 变更），确认后 POST /api/roles/{id}。
     - 新建 / 复制 / 改名与描述 / 删除自定义角色；删除有成员的角色会被拒绝并列出成员。
     - 权限模拟器：从原「权限与审计」页迁来，读取真实角色数据（含自定义角色）。
     - 全页对只有 audit.view 的账号（审计员 / 审批人）只读。
   非运维模式：说明卡片 + 内置角色的静态矩阵（security.js）。
   data-na / data-nc / data-ni 前缀：or-。依赖 ops.js、ops_users.js（opRoleCacheReplace）、ops_pages.js（oppConfirm）、security.js（SEC_*、secAuthorize）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); if (isNaN(d)) return '—'; return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}`; };
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview').split('/')[0] === h;
  const me = () => (OP.me && OP.me.user && OP.me.user.name) || '';
  const clone = o => JSON.parse(JSON.stringify(o));
  const runes = s => [...String(s || '')].length;
  const canW = () => opCan('users.manage');
  const canView = () => opCan('users.manage') || opCan('audit.view');
  const NEED_W = '需要 users.manage 能力（只有 audit.view 时只读）';

  const GRANT_L = {none: '无', allow: '允许', approval: '需审批', scoped: '范围内'};
  const GRANT_C = {none: '', allow: 'ok', approval: 'warn', scoped: 'info'};
  const HINT = {
    locked_cap: '该能力仅内置管理员角色可持有，自定义角色不可授予。',
    escalation: '防提权：你只能授予自己已拥有、且授予方式不低于所授予方式的能力。',
    builtin: '内置角色不可修改或删除；可先「复制」为自定义角色再调整。',
    exists: '角色 ID 或名称已被占用（含内置角色的 ID 与显示名）。',
    limit: '自定义角色数量已达上限。',
    in_use: '仍有成员持有该角色：请先在「用户管理」里把成员的角色改掉。',
    invalid: '参数不合法，请按提示修改。',
    not_found: '角色不存在（可能已被他人删除），请刷新后重试。',
    store: '角色库保存失败：请检查数据目录的权限与磁盘空间后重试。',
  };
  const errHtml = (data, code) => {
    const c = (data && data.code) || '', h = c === 'limit' ? `自定义角色数量已达上限（${OR.limits.maxCustom} 个）。` : HINT[c];
    const msg = (data && data.error) || ('请求失败（' + code + '）');
    return `${h ? `<b>${E(h)}</b><br>` : ''}后端返回：${E(msg)}`;
  };

  const OR = {
    roles: [], catalog: [], limits: {maxCustom: 50, maxName: 40, maxDesc: 200, idPattern: '^[a-z][a-z0-9_-]{1,31}$'}, warnings: [],
    loaded: false, loading: false, err: '', errCode: 0, at: 0, sig: '',
    sel: '', tab: 'perm', draft: null, draftFor: '', draftBase: '', stale: false, form: null, flash: null, lastUser: '',
    sim: {role: '', cap: 'scale', env: '生产', risk: 'high', flags: {测试: true, 预发: false, 生产: false}, versionOk: true, mfa: false},
    pol: null, polAt: 0,
  };
  const role = id => OR.roles.find(r => r.id === id);
  const cur = () => role(OR.sel);
  const allCaps = () => OR.catalog.flatMap(g => g.caps);
  const capInfo = c => allCaps().find(x => x.cap === c);
  const customCount = () => OR.roles.filter(r => !r.builtin).length;
  const normCaps = c => { const o = {}; Object.keys(c || {}).sort().forEach(k => { if (c[k] && c[k] !== 'none') o[k] = c[k]; }); return o; };
  const capsJson = c => JSON.stringify(normCaps(c));
  const dirty = () => { const r = cur(); return !!(r && !r.builtin && OR.draft && capsJson(OR.draft) !== capsJson(r.caps)); };
  const grantsOf = r => (r.id === OR.sel && !r.builtin && OR.draft) ? OR.draft : r.caps;

  /* ---------------- 数据 ---------------- */
  async function rolesLoad(force) {
    if (OR.loading) return; OR.loading = true;
    const r = await opApi('GET', '/api/roles');
    OR.loading = false; OR.at = Date.now();
    if (r.code === 200) {
      const d = r.data, sig = JSON.stringify([d.roles, d.catalog, d.warnings]);
      OR.roles = d.roles || []; OR.catalog = d.catalog || []; OR.warnings = d.warnings || []; if (d.limits) OR.limits = d.limits;
      window.opRoleCacheReplace(OR.roles.map(x => ({id: x.id, name: x.name, builtin: x.builtin})));
      OR.err = ''; OR.errCode = 0;
      if (!role(OR.sel)) { OR.sel = (OR.roles[0] || {}).id || ''; OR.draftFor = ''; }
      syncDraft();
      if (sig !== OR.sig || force || !OR.loaded) { OR.sig = sig; OR.loaded = true; paint(); } else OR.loaded = true;
    } else { OR.err = r.data.error || ('读取失败（' + r.code + '）'); OR.errCode = r.code; OR.loaded = true; OR.sig = ''; paint(); }
  }
  function syncDraft() {
    const r = cur(); if (!r) { OR.draft = null; return; }
    const base = capsJson(r.caps);
    if (OR.draftFor !== OR.sel || !dirty()) { OR.draft = clone(normCaps(r.caps)); OR.draftFor = OR.sel; OR.draftBase = base; OR.stale = false; }
    else if (base !== OR.draftBase) OR.stale = true;   // 编辑期间，别人改了该角色
  }
  async function polSync(force) {
    if (!force && OR.pol && Date.now() - OR.polAt < 15000) return;
    const r = await opApi('GET', '/api/ops/policy'); OR.polAt = Date.now();
    if (r.code !== 200 || !r.data.policy) return;
    OR.pol = r.data; const envs = ['test', 'staging', 'prod'];
    ['low', 'mid', 'high'].forEach(k => { if (r.data.policy.levels && r.data.policy.levels[k]) SEC_APPROVAL[k] = envs.map(e => r.data.policy.levels[k][e] | 0); });
    if (OR.tab === 'sim') paint();
  }
  function paint() {
    if (!(OP.on && hashIs('roles') && $('#orBody'))) return;
    const y = window.scrollY; pages.roles.body(); if (y) window.scrollTo(0, y);
  }
  const setFlash = (lv, html) => { OR.flash = {lv, html}; paint(); };

  /* ---------------- 页面外壳 ---------------- */
  const NOTE_OFF = () => `<div class="card"><h2>角色权限</h2><div class="notice info"><span>ℹ</span><div>角色与权限管理需要运维模式（启动参数 <code>--data-dir</code>）。当前是原型 / 只读演示模式，没有真实的角色库，下面只展示 7 个内置角色的静态权限矩阵。</div></div>
    <div class="small muted">运维模式下，这里可以查看每个角色的功能权限、成员，创建 / 复制 / 修改自定义角色（每项能力选「允许 / 需审批 / 范围内」），并用权限模拟器核对判定过程。</div></div>
    <div style="margin-top:16px">${pages.security && pages.security.matrix ? pages.security.matrix.call(pages.security) : ''}</div>`;
  const NOTE_DENY = '<div class="card"><h2>角色权限</h2><div class="notice warn"><span>🔒</span><div>当前账号没有查看角色与权限的能力（需要 users.manage 或 audit.view）。</div></div></div>';

  pages.roles = {
    title: '角色权限',
    render() {
      if (!OP.on) return NOTE_OFF();
      if (!OP.me || !canView()) return NOTE_DENY;
      return '<div id="orBody"></div>';
    },
    init() {
      if (!OP.on || !OP.me || !canView()) return;
      const na = window.navArgs; window.navArgs = null;
      if (OR.lastUser !== me()) { OR.lastUser = me(); Object.assign(OR, {roles: [], loaded: false, err: '', sig: '', flash: null, form: null, draft: null, draftFor: '', sel: ''}); }
      if (na && na.role && role(na.role)) OR.sel = na.role;
      this.body(); rolesLoad(true); polSync(true);
    },
    body() {
      const el = $('#orBody'); if (!el) return;
      if (!OR.loaded) { el.innerHTML = '<div class="card"><div class="empty">正在读取角色…</div></div>'; return; }
      if (OR.err) { el.innerHTML = `<div class="card"><h2>角色权限</h2><div class="notice bad"><span>✖</span><div>${E(OR.err)}</div></div><button class="btn sm" data-na="or-reload">重试</button></div>`; return; }
      el.innerHTML = layoutHtml();
    },
  };

  function layoutHtml() {
    const r = cur();
    const warn = OR.warnings.length ? `<div class="notice warn"><span>⚠</span><div><b>角色库加载时有降级处理</b>（已丢弃不安全的内容，不会提权）：${E(OR.warnings.join('；'))}</div></div>` : '';
    const f = OR.flash ? `<div class="notice ${OR.flash.lv}" style="margin-bottom:10px"><span>${{ok: '✔', bad: '✖', warn: '⚠', info: 'ℹ'}[OR.flash.lv] || 'ℹ'}</span><div style="flex:1">${OR.flash.html}</div><button class="btn sm" data-na="or-flash-x">知道了</button></div>` : '';
    return `${warn}${f}<div class="or-wrap">${sideHtml()}<div class="card or-main">${r ? detailHtml(r) : '<div class="empty">没有角色</div>'}</div></div>`;
  }
  function sideHtml() {
    const bi = OR.roles.filter(r => r.builtin), cu = OR.roles.filter(r => !r.builtin), max = OR.limits.maxCustom;
    const it = r => `<button type="button" class="or-it ${r.id === OR.sel ? 'sel' : ''}" data-na="or-sel" data-id="${E(r.id)}"><span class="or-itn"><span><b>${E(r.name)}</b>${r.id === OR.sel && dirty() ? ' <span class="or-dot" title="有未保存的修改">●</span>' : ''}</span><span class="mono small muted">${E(r.id)}</span></span>${r.builtin ? tg('内置') : `<span class="small muted">${r.members} 名成员</span>`}</button>`;
    const full = cu.length >= max;
    return `<div class="card or-side"><h2>角色 <span class="sub">${OR.roles.length} 个</span><span class="right"><button class="btn sm" data-na="or-new" title="${canW() ? (full ? '自定义角色已达上限 ' + max + ' 个' : '新建角色') : NEED_W}" ${canW() && !full ? '' : 'disabled'}>＋</button></span></h2>
      <div class="or-grp">内置角色 <span>${bi.length}</span></div>${bi.map(it).join('')}
      <div class="or-grp">自定义角色 <span>${cu.length} / ${max}</span></div>${cu.map(it).join('') || '<div class="small muted" style="padding:4px 8px">还没有自定义角色。点击右上角「＋」新建，或在右侧「复制」内置角色再调整。</div>'}
      <div class="hr"></div><a data-na="or-matrix" class="or-link">▦ 查看全部角色权限矩阵</a></div>`;
  }
  function detailHtml(r) {
    const w = canW(), tabs = [['perm', '功能权限'], ['members', `成员 (${r.members})`], ['sim', '权限模拟器']];
    const hdr = `<h2 class="or-title"><span>${E(r.name)}</span> ${r.builtin ? tg('内置') : tg('自定义', 'info')} <span class="mono small muted">${E(r.id)}</span>
      <span class="right"><button class="btn sm" data-na="or-matrix">对比矩阵</button>
        <button class="btn sm" data-na="or-copy" ${w ? '' : `disabled title="${NEED_W}"`}>复制</button>
        ${r.builtin ? '' : `<button class="btn sm" data-na="or-meta" ${w ? '' : `disabled title="${NEED_W}"`}>改名 / 描述</button><button class="btn sm danger" data-na="or-del" ${w ? '' : `disabled title="${NEED_W}"`}>删除</button>`}</span></h2>
      <div class="small muted" style="margin:-6px 0 10px">${r.desc ? E(r.desc) : '（无说明）'}${r.builtin ? '' : ` · 最后修改 ${E(fDT(r.updated || r.created))}${r.updatedBy ? ' by ' + E(r.updatedBy) : ''}`}</div>`;
    const notices = (r.builtin ? '<div class="notice info"><span>🔒</span><div><b>内置角色不可修改</b>（只读展示）。需要不同权限时，点右上角「复制」得到一个自定义角色再调整。</div></div>'
      : !w ? '<div class="notice warn"><span>🔒</span><div>只读：当前账号没有 users.manage 能力，无法修改角色。</div></div>' : '')
      + (OR.stale && dirty() ? '<div class="notice warn"><span>⚠</span><div>你编辑期间，该角色已被其他人修改。保存会整体覆盖当前已保存的权限；建议先「撤销」重新载入后再改。</div></div>' : '');
    return `${hdr}${notices}<div class="tabs">${tabs.map(([k, t]) => `<button data-na="or-tab" data-t="${k}" class="${OR.tab === k ? 'active' : ''}">${t}</button>`).join('')}</div>
      ${OR.tab === 'perm' ? permHtml(r) : OR.tab === 'members' ? membersHtml(r) : simHtml()}`;
  }

  /* ---------------- 功能权限 ---------------- */
  const REQUIRED = 'view.overview';
  function capRow(c, r, edit, draft) {
    const g = (edit ? draft[c.cap] : r.caps[c.cap]) || 'none', saved = r.caps[c.cap] || 'none', chg = edit && g !== saved;
    let ctl;
    if (r.builtin) ctl = c.locked && g === 'none' ? '<span class="or-lock">🔒 仅内置管理员角色</span>' : `${tg(GRANT_L[g], GRANT_C[g])}${c.locked ? ' <span class="or-lock" title="仅内置管理员角色可持有">🔒</span>' : ''}`;
    else if (c.locked) ctl = '<span class="or-lock">🔒 仅内置管理员角色</span>';
    else if (c.cap === REQUIRED) ctl = `<span class="or-seg"><button type="button" class="on" disabled>${GRANT_L.allow}</button></span> <span class="small muted">必选</span>`;
    else ctl = `<span class="or-seg">${c.allowedGrants.map(x => `<button type="button" class="${g === x ? 'on' : ''}" data-na="or-g" data-cap="${E(c.cap)}" data-g="${x}" ${edit ? '' : 'disabled'}>${GRANT_L[x] || x}</button>`).join('')}</span>`;
    const risk = c.risk === 'high' ? '<span class="or-risk high" title="高风险">●</span>' : c.risk === 'mid' ? '<span class="or-risk mid" title="中风险">●</span>' : '';
    return `<div class="or-cap ${chg ? 'chg' : ''}"><div class="or-capi"><div class="or-capn">${E(c.name)}${risk}<code class="or-code">${E(c.cap)}</code></div><div class="small muted">${E(c.desc)}</div></div><div class="or-capg">${ctl}</div></div>`;
  }
  function permHtml(r) {
    const edit = !r.builtin && canW(), draft = OR.draft || {}, caps = edit ? draft : r.caps, n = Object.keys(normCaps(caps)).length, total = allCaps().length;
    const quick = edit ? `<button class="btn sm" data-na="or-quick" data-k="all" title="除「仅内置管理员」能力外，全部设为「允许」">全选允许</button><button class="btn sm" data-na="or-quick" data-k="clear" title="只保留必选的「查看总览」">清空</button><button class="btn sm" data-na="or-quick" data-k="view" title="查看总览 + 诊断巡检 + 导出报告（均为只读、无集群影响）">仅查看</button>` : '';
    const d = dirty();
    return `<div class="row" style="margin-bottom:8px"><span class="small muted">已授予 <b>${n}</b> / ${total} 项能力 · <span class="or-risk high">●</span> 高风险 <span class="or-risk mid">●</span> 中风险 · 「需审批」= 可发起，按风险 × 环境走审批；「范围内」= 仅限用户被授权的库</span><span style="flex:1"></span>${quick}</div>
      ${OR.catalog.map(g => `<div class="or-gh">${E(g.group)} <span class="small muted">${g.caps.filter(c => (caps[c.cap] || 'none') !== 'none').length} / ${g.caps.length}</span></div>${g.caps.map(c => capRow(c, r, edit, draft)).join('')}`).join('')}
      ${edit ? `<div class="or-savebar">${d ? `<span class="or-dirty">● 有未保存的修改（${diffOf(r).count} 项）</span>` : '<span class="small muted">没有未保存的修改</span>'}<span style="flex:1"></span><button class="btn" data-na="or-undo" ${d ? '' : 'disabled'}>撤销</button><button class="btn primary" data-na="or-save" ${d ? '' : 'disabled'}>保存</button></div>` : ''}`;
  }
  function diffOf(r) {
    const a = normCaps(r.caps), b = normCaps(OR.draft || {}), add = [], del = [], chg = [];
    allCaps().forEach(c => { const x = a[c.cap] || 'none', y = b[c.cap] || 'none'; if (x === y) return; if (x === 'none') add.push([c, y]); else if (y === 'none') del.push([c, x]); else chg.push([c, x, y]); });
    return {add, del, chg, count: add.length + del.length + chg.length};
  }

  /* ---------------- 成员 ---------------- */
  function membersHtml(r) {
    const names = r.memberNames || [];
    return `<div class="row" style="margin-bottom:10px"><span class="small muted">持有该角色的用户（任一环境）。在「用户管理 → 编辑」里为用户分配 / 移除角色。</span><span style="flex:1"></span><button class="btn sm" data-go="users" data-role="${E(r.id)}">在用户管理中查看 →</button></div>
      ${names.length ? `<div class="tbl-wrap"><table><thead><tr><th>账号</th><th></th></tr></thead><tbody>${names.map(n => `<tr><td class="mono"><b>${E(n)}</b></td><td style="text-align:right"><a data-na="or-member" data-n="${E(n)}">在用户管理中定位 →</a></td></tr>`).join('')}</tbody></table></div>` : '<div class="empty">暂无成员</div>'}
      <div class="small muted" style="margin-top:8px">共 ${names.length} 名成员（含已禁用的用户）。${r.builtin ? '' : '自定义角色仍有成员时不能删除。'}</div>`;
  }

  /* ---------------- 权限模拟器 ---------------- */
  const CAPMAP = {'view.overview': 'overview', 'view.sql': 'sqltext', 'diag.run': 'diag', 'report.export': 'report', 'inst.op': 'inst', 'scale': 'scale', 'config': 'config', 'maint': 'maint', 'backup': 'backup', 'sched.create': 'schedcreate', 'approve.mid': 'approvemid', 'approve.high': 'approvehigh', 'release': 'release', 'settings.edit': 'platcfg', 'writeswitch': 'prodwrite', 'users.manage': 'iam', 'audit.view': 'auditview', 'audit.export': 'auditexport', 'shell.run': 'shellrun', 'shell.approve': 'shellappr'};
  const LVL = {allow: 'Y', approval: 'R', scoped: 'S', none: '-'};
  function simHtml() {
    const S = OR.sim, R = role(S.role) || cur(); if (!R) return '';
    if (!S.role || !role(S.role)) S.role = R.id;
    if (!capInfo(S.cap)) S.cap = (allCaps()[0] || {}).cap || 'view.overview';
    const sc = CAPMAP[S.cap] ? secCap(CAPMAP[S.cap]) : null; if (!sc) return '<div class="empty">该能力暂无模拟规则</div>';
    const caps = grantsOf(R), g = caps[S.cap] || 'none', usingDraft = R.id === OR.sel && !R.builtin && dirty();
    const r = secAuthorize({role: R.id, roleName: R.name, cap: CAPMAP[S.cap], env: S.env, risk: S.risk, flags: S.flags, versionOk: S.versionOk, mfa: S.mfa, lvl: LVL[g]});
    const dec = {allow: ['ok', '允许'], approval: ['warn', `需 ${r.needs} 级审批`], deny: ['bad', '拒绝']}[r.decision];
    const audit = {ts: '2026-xx-xx hh:mm:ss', actor: '(当前用户)', role: R.id, env: S.env, capability: S.cap, risk: RISK_LBL[S.risk], result: r.decision === 'deny' ? '拒绝：' + r.reason : r.decision === 'approval' ? `待审批(0/${r.needs})` : '允许', prev_hash: '…', hash: 'sha256(…)'};
    const polNote = OR.pol ? `审批级数取自当前生效的审批策略${OR.pol.usingDefault ? '（默认）' : '（自定义）'}；MFA 提示仍按默认规则演示，实际要求见「权限与审计 → 审批策略」。` : '审批级数暂用默认值（审批策略读取中或不可用）。';
    return `<div class="notice info" style="margin-bottom:12px"><span>ℹ</span><div>${E(polNote)}${usingDraft ? ' <b>当前角色有未保存的修改，模拟按草稿计算。</b>' : ' 模拟按已保存的角色权限计算。'}</div></div>
      <div class="grid g-main" style="grid-template-columns:340px minmax(0,1fr)"><div class="card" style="box-shadow:none"><h2>输入</h2>
      <div class="field"><label>角色</label><select data-nc="or-sim" data-k="role">${OR.roles.map(x => `<option value="${E(x.id)}" ${S.role === x.id ? 'selected' : ''}>${E(x.name)}${x.builtin ? '' : '（自定义）'}</option>`).join('')}</select></div>
      <div class="field"><label>能力 / 动作</label><select data-nc="or-sim" data-k="cap">${OR.catalog.map(gr => `<optgroup label="${E(gr.group)}">${gr.caps.map(x => `<option value="${E(x.cap)}" ${S.cap === x.cap ? 'selected' : ''}>${E(x.name)}</option>`).join('')}</optgroup>`).join('')}</select></div>
      <div class="field"><label>环境</label><select data-nc="or-sim" data-k="env">${SEC_ENVS.map(e => `<option ${S.env === e ? 'selected' : ''}>${e}</option>`).join('')}</select></div>
      <div class="field"><label>风险等级</label><select data-nc="or-sim" data-k="risk">${['low', 'mid', 'high'].map(k => `<option value="${k}" ${S.risk === k ? 'selected' : ''}>${RISK_LBL[k]}</option>`).join('')}</select></div>
      <div class="hr"></div><b class="small">环境条件</b>
      ${SEC_ENVS.map(e => `<label class="small" style="display:flex;gap:6px;margin-top:4px"><input type="checkbox" data-nc="or-flag" data-e="${e}" ${S.flags[e] ? 'checked' : ''}> ${e}环境写操作开关${e === '生产' ? '（默认关闭）' : ''}</label>`).join('')}
      <label class="small" style="display:flex;gap:6px;margin-top:4px"><input type="checkbox" data-nc="or-ver" ${S.versionOk ? 'checked' : ''}> 集群版本在支持矩阵内</label>
      <label class="small" style="display:flex;gap:6px;margin-top:4px"><input type="checkbox" data-nc="or-mfa" ${S.mfa ? 'checked' : ''}> 申请人已通过 MFA</label></div>
      <div class="grid" style="align-content:start"><div class="card" style="box-shadow:none"><h2>判定结果 <span class="small muted">${E(R.name)} · ${E(capInfo(S.cap).name)}（${GRANT_L[g]}）</span><span class="right"><span class="tag ${dec[0]}" style="font-size:14px;padding:2px 14px">${dec[1]}</span></span></h2>
        ${r.steps.map(([lv, n, t]) => `<div class="notice ${lv === 'info' ? 'info' : lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[lv]}</span><div><b>${E(n)}</b>：${E(t)}</div></div>`).join('')}</div>
        <div class="card" style="box-shadow:none"><h2>执行路径与命令</h2><div class="small">${sc[4] && r.decision !== 'deny' ? '<div class="flow"><span class="st done">浏览器</span>›<span class="st done">Web 进程（tidbops）</span>›<span class="st cur">sudo -u tidb tidb-ops exec</span>›<span class="st">白名单校验 + 审批令牌</span>›<span class="st">tiup</span></div>' : r.decision === 'deny' ? '<span class="muted">已被拒绝，不会进入执行器</span>' : '<span class="muted">读操作 / 非集群变更：由 Web 进程用只读账号或内部逻辑完成，不经过 exec 入口</span>'}</div>
          ${code(sc[6])}</div>
        <div class="card" style="box-shadow:none"><h2>将写入的审计记录</h2>${code(JSON.stringify(audit, null, 2))}</div></div></div>`;
  }

  /* ---------------- 对比矩阵（抽屉） ---------------- */
  function matrixHtml() {
    const sym = {allow: '<span style="color:var(--ok);font-weight:700">✔</span>', approval: '<span class="tag warn">审</span>', scoped: '<span class="tag info">范</span>', none: '<span class="muted">—</span>'};
    return `<div class="row" style="margin-bottom:8px"><span class="small muted">${OR.roles.length} 个角色 × ${allCaps().length} 项能力（含自定义角色，实时数据）。</span><span style="flex:1"></span><button class="btn sm" data-na="or-mx-reload">刷新</button><button class="btn sm" data-na="or-print">打印 / 导出</button></div>
      <div class="legend" style="margin:0 0 8px"><span>✔ 允许</span><span><span class="tag warn">审</span> 需审批</span><span><span class="tag info">范</span> 范围内</span><span>— 无</span><span><span class="or-risk high">●</span> 高风险</span></div>
      <div class="tbl-wrap"><table class="or-mx"><thead><tr><th>能力</th>${OR.roles.map(r => `<th style="text-align:center" class="${r.id === OR.sel ? 'or-mxsel' : ''}">${E(r.name)}${r.builtin ? '' : '<div class="small" style="font-weight:400;color:var(--accent)">自定义</div>'}</th>`).join('')}</tr></thead><tbody>
      ${OR.catalog.map(g => `<tr><td colspan="${OR.roles.length + 1}" style="background:var(--panel2);font-weight:600">${E(g.group)}</td></tr>${g.caps.map(c => `<tr><td>${E(c.name)}${c.risk === 'high' ? ' <span class="or-risk high">●</span>' : ''}<div class="small muted mono">${E(c.cap)}</div></td>${OR.roles.map(r => `<td style="text-align:center" class="${r.id === OR.sel ? 'or-mxsel' : ''}">${sym[grantsOf(r)[c.cap] || 'none']}</td>`).join('')}</tr>`).join('')}`).join('')}</tbody></table></div>`;
  }
  function matrixOpen() { openDrawer('全部角色权限矩阵', '<div id="orMx"></div>'); $('#drawer').classList.add('wide'); $('#orMx').innerHTML = matrixHtml(); }

  /* ---------------- 新建 / 复制 / 改名 抽屉 ---------------- */
  const reservedNames = () => { const s = new Set(); OR.roles.forEach(r => { s.add(r.name.trim().toLowerCase()); s.add(r.id); }); return s; };
  function validate(F) {
    const o = {id: '', name: '', desc: ''}, L = OR.limits;
    if (F.mode !== 'meta') {
      if (!F.id) o.id = '请填写角色 ID';
      else if (!new RegExp(L.idPattern).test(F.id)) o.id = '需 2–32 位，小写字母开头，只能含小写字母、数字、_ 和 -';
      else if (role(F.id)) o.id = role(F.id).builtin ? '与内置角色 ID 重名' : '该 ID 已存在';
    }
    const n = F.name.trim();
    if (!n) o.name = '请填写角色名称';
    else if (runes(n) > L.maxName) o.name = `名称不能超过 ${L.maxName} 个字（当前 ${runes(n)}）`;
    else if (OR.roles.some(r => r.id !== F.roleId && r.name.trim().toLowerCase() === n.toLowerCase())) o.name = '名称与已有角色（含内置角色显示名）重复';
    else if (n.toLowerCase() !== (F.roleId || '') && OR.roles.some(r => r.builtin && r.id === n.toLowerCase())) o.name = '名称与内置角色 ID 相同';
    if (runes(F.desc) > L.maxDesc) o.desc = `说明不能超过 ${L.maxDesc} 个字（当前 ${runes(F.desc)}）`;
    return o;
  }
  function lockedOf(from) { const r = role(from); if (!r) return []; return allCaps().filter(c => c.locked && (r.caps[c.cap] || 'none') !== 'none'); }
  function formHtml() {
    const F = OR.form, v = validate(F), meta = F.mode === 'meta', copy = F.mode === 'copy';
    const lk = F.from ? lockedOf(F.from) : [];
    return `${meta ? `<div class="kv" style="margin-bottom:10px"><span>角色 ID</span><span class="mono">${E(F.roleId)}</span></div>` : `<div class="field"><label>角色 ID（小写字母开头，a-z 0-9 _ -，2–32 位；创建后不可改）</label><input type="text" data-ni="or-f" data-k="id" value="${E(F.id)}" maxlength="32" autocomplete="off" spellcheck="false"><div class="small" id="orHintId" style="min-height:16px;color:var(--bad)">${E(v.id)}</div></div>`}
      <div class="field"><label>名称（1–${OR.limits.maxName} 字；不能与内置角色显示名重复）</label><input type="text" data-ni="or-f" data-k="name" value="${E(F.name)}" autocomplete="off"><div class="small" id="orHintName" style="min-height:16px;color:var(--bad)">${E(v.name)}</div></div>
      <div class="field"><label>说明（可选，≤${OR.limits.maxDesc} 字）</label><textarea data-ni="or-f" data-k="desc" rows="3">${E(F.desc)}</textarea><div class="small" id="orHintDesc" style="min-height:16px;color:var(--bad)">${E(v.desc)}</div></div>
      ${meta ? '' : `<div class="field"><label>初始权限</label>
        ${copy ? `<div class="small">复制自 <b>${E((role(F.from) || {}).name || F.from)}</b>（<span class="mono">${E(F.from)}</span>）${dirty() && F.from === OR.sel ? '。<span style="color:var(--warn)">复制的是已保存的权限，不含未保存的修改。</span>' : ''}</div>`
        : `<label class="small"><input type="radio" name="orInit" data-nc="or-f" data-k="initMode" value="blank" ${F.from ? '' : 'checked'}> 空白（只有必选的「查看总览」）</label>
          <label class="small"><input type="radio" name="orInit" data-nc="or-f" data-k="initMode" value="copy" ${F.from ? 'checked' : ''}> 复制自：</label> <select data-nc="or-f" data-k="from" ${F.from ? '' : 'disabled'}>${OR.roles.map(r => `<option value="${E(r.id)}" ${F.from === r.id ? 'selected' : ''}>${E(r.name)}${r.builtin ? '' : '（自定义）'}</option>`).join('')}</select>`}
        ${lk.length ? `<div class="notice info" style="margin-top:6px"><span>ℹ</span><div>已去掉仅管理员能力：${E(lk.map(c => c.cap).join('、'))}（自定义角色不可持有）。</div></div>` : ''}</div>`}
      <div id="orFormErr" class="small" style="min-height:18px;color:var(--bad)">${F.err || ''}</div>
      <div class="sticky-actions"><button class="btn primary" data-na="or-f-go" ${v.id || v.name || v.desc || F.busy ? 'disabled' : ''}>${meta ? '保存' : copy ? '复制并创建' : '创建角色'}</button><button class="btn" data-close="1">取消</button></div>`;
  }
  const formRender = () => { const el = $('#orForm'); if (el && OR.form) el.innerHTML = formHtml(); };
  function formOpen(F, title) { OR.form = F; openDrawer(title, '<div id="orForm"></div>'); formRender(); }
  function formLive() {   // 输入时只刷新校验提示与提交按钮，不重绘输入框（保持焦点）
    const F = OR.form; if (!F) return; const v = validate(F);
    [['orHintId', v.id], ['orHintName', v.name], ['orHintDesc', v.desc]].forEach(([id, t]) => { const el = $('#' + id); if (el) el.textContent = t; });
    const b = $('#orForm [data-na="or-f-go"]'); if (b) b.disabled = !!(v.id || v.name || v.desc || F.busy);
  }
  function suggestId(src) { const base = (src + '-copy').slice(0, 32); let id = base, i = 2; while (role(id)) { const suf = '-copy' + i++; id = (src.slice(0, 32 - suf.length) + suf); } return id; }
  async function formSubmit() {
    const F = OR.form, err = $('#orFormErr'); if (!F || F.busy) return;
    const v = validate(F); if (v.id || v.name || v.desc) return;
    F.busy = true; formLive(); if (err) err.textContent = '';
    let r;
    if (F.mode === 'meta') {
      const old = role(F.roleId), body = {}; if (F.name.trim() !== old.name) body.name = F.name.trim(); if (F.desc.trim() !== (old.desc || '')) body.desc = F.desc.trim();
      if (!Object.keys(body).length) { F.busy = false; closeDrawer(); OR.form = null; toast('没有修改'); return; }
      r = await opApi('POST', `/api/roles/${encodeURIComponent(F.roleId)}`, body);
    } else {
      const body = {id: F.id, name: F.name.trim(), desc: F.desc.trim(), caps: F.from ? {} : {[REQUIRED]: 'allow'}};
      if (F.from) body.copyFrom = F.from;
      r = await opApi('POST', '/api/roles', body);
    }
    F.busy = false;
    if (r.code === 200) {
      const id = F.mode === 'meta' ? F.roleId : r.data.role.id; closeDrawer(); OR.form = null;
      if (F.mode !== 'meta') { OR.sel = id; OR.tab = 'perm'; OR.draftFor = ''; OR.sim.role = id; }
      toast(F.mode === 'meta' ? '已保存名称 / 说明' : `已创建角色「${F.name.trim()}」`); OR.flash = null;
      await rolesLoad(true);
    } else { if ($('#orFormErr')) { $('#orFormErr').innerHTML = errHtml(r.data, r.code); F.err = $('#orFormErr').innerHTML; } formLive(); }
  }

  /* ---------------- 保存 / 删除 ---------------- */
  function saveConfirm() {
    const r = cur(); if (!r || !dirty()) return;
    const d = diffOf(r), n = r.members, names = r.memberNames || [];
    const li = (c, g) => `<span class="tag ${GRANT_C[g] || ''}" style="margin:2px">${E(c.name)} · ${GRANT_L[g]}</span>`;
    openDrawer(`保存角色「${r.name}」`, `<div class="notice ${n ? 'warn' : 'info'}"><span>${n ? '⚠' : 'ℹ'}</span><div>${n ? `将<b>立即对 ${n} 名成员生效</b>：${E(names.slice(0, 12).join('、'))}${names.length > 12 ? ' 等' : ''}。` : '该角色目前没有成员，修改只影响以后分配到该角色的用户。'}</div></div>
      ${d.add.length ? `<h4 style="color:var(--ok);margin:12px 0 4px">＋ 新增 ${d.add.length} 项</h4><div class="small">${d.add.map(([c, g]) => li(c, g)).join('')}</div>` : ''}
      ${d.del.length ? `<h4 style="color:var(--bad);margin:12px 0 4px">－ 移除 ${d.del.length} 项</h4><div class="small">${d.del.map(([c, g]) => `<span class="tag bad" style="margin:2px">${E(c.name)} · ${GRANT_L[g]}</span>`).join('')}</div>` : ''}
      ${d.chg.length ? `<h4 style="color:var(--warn);margin:12px 0 4px">~ 变更授予方式 ${d.chg.length} 项</h4><div class="small">${d.chg.map(([c, a, b]) => `<div style="margin:2px 0">${E(c.name)}：<span class="diff-old">${GRANT_L[a]}</span> → <span class="diff-new">${GRANT_L[b]}</span></div>`).join('')}</div>` : ''}
      <div id="orSaveErr" class="notice bad" style="display:none;margin-top:12px"><span>✖</span><div></div></div>
      <div class="sticky-actions"><button class="btn primary" data-na="or-save-go">确认保存</button><button class="btn" data-close="1">返回继续编辑</button></div>`);
  }
  async function saveGo(btn) {
    const r = cur(); if (!r) return; btn.disabled = true;
    const caps = normCaps(OR.draft); caps[REQUIRED] = 'allow';
    const res = await opApi('POST', `/api/roles/${encodeURIComponent(r.id)}`, {caps});
    btn.disabled = false;
    if (res.code === 200) {
      closeDrawer(); const n = res.data.affected != null ? res.data.affected : r.members;
      toast(res.data.changed ? `已保存「${r.name}」，已对 ${n} 名成员即时生效` : '没有变化'); OR.flash = null; OR.draftFor = ''; await rolesLoad(true);
    } else { const box = $('#orSaveErr'); if (box) { box.style.display = ''; box.querySelector('div').innerHTML = errHtml(res.data, res.code); } }
  }
  async function deleteFlow() {
    await rolesLoad(true); const r = cur(); if (!r || r.builtin) return;
    if (r.members > 0) return inUse(r.name, r.memberNames || []);
    oppConfirm('删除自定义角色', `确认删除自定义角色 <b>${E(r.name)}</b>（<span class="mono">${E(r.id)}</span>，含 ${Object.keys(r.caps || {}).length} 项能力）？不可恢复。目前没有成员持有它。`, '确认删除', async () => {
      const res = await opApi('POST', `/api/roles/${encodeURIComponent(r.id)}/delete`, {});
      if (res.code === 200) { toast(`已删除角色「${r.name}」`); OR.flash = null; OR.sel = ''; OR.draftFor = ''; await rolesLoad(true); }
      else if (res.code === 409 && res.data.code === 'in_use') inUse(r.name, res.data.members || [], res.data.error);
      else setFlash('bad', errHtml(res.data, res.code));
    }, true);
  }
  function inUse(name, members, msg) {
    openDrawer('无法删除角色', `<div class="notice bad"><span>✖</span><div><b>角色「${E(name)}」仍有 ${members.length} 名成员，不能删除。</b><br>${msg ? '后端返回：' + E(msg) + '<br>' : ''}请先在「用户管理」里把这些用户的角色改掉（含已禁用的用户），再删除角色。</div></div>
      <h4 style="margin:12px 0 6px">仍持有该角色的用户</h4><div class="row" style="gap:6px">${members.map(n => `<a class="tag" data-na="or-member" data-n="${E(n)}" style="cursor:pointer">${E(n)}</a>`).join('')}</div>
      <div class="sticky-actions"><button class="btn primary" data-go="users" data-role="${E((OR.roles.find(r => r.name === name) || {}).id || '')}" data-close="1">去用户管理处理 →</button><button class="btn" data-close="1">知道了</button></div>`);
  }

  /* ---------------- 事件 ---------------- */
  function selectRole(id) { OR.sel = id; OR.draftFor = ''; OR.sim.role = id; syncDraft(); paint(); }
  const CLICK = {
    'or-reload': () => rolesLoad(true),
    'or-flash-x': () => { OR.flash = null; paint(); },
    'or-sel': d => {
      if (d.id === OR.sel) return;
      if (dirty()) oppConfirm('放弃未保存的修改？', `角色「${E(cur().name)}」有未保存的权限修改，切换后会丢失。`, '放弃并切换', () => selectRole(d.id));
      else selectRole(d.id);
    },
    'or-tab': d => { OR.tab = d.t; if (d.t === 'sim') polSync(true); paint(); },
    'or-g': d => { if (!OR.draft) return; if (d.g === 'none') delete OR.draft[d.cap]; else OR.draft[d.cap] = d.g; paint(); },
    'or-quick': d => {
      const r = cur(); if (!r || r.builtin || !OR.draft) return; const nd = {};
      OR.catalog.forEach((g, gi) => g.caps.forEach(c => {
        if (c.locked) return;
        if (c.cap === REQUIRED) nd[c.cap] = 'allow';
        else if (d.k === 'all') nd[c.cap] = 'allow';
        else if (d.k === 'view' && gi === 0 && c.risk === 'none') nd[c.cap] = 'allow';
      }));
      OR.draft = nd; paint();
    },
    'or-undo': () => { const r = cur(); if (!r) return; OR.draft = clone(normCaps(r.caps)); OR.draftBase = capsJson(r.caps); OR.stale = false; paint(); },
    'or-save': () => saveConfirm(),
    'or-save-go': (d, el) => saveGo(el),
    'or-new': () => formOpen({mode: 'create', id: '', name: '', desc: '', from: '', busy: false}, '新建角色'),
    'or-copy': () => { const r = cur(); if (!r) return; let nm = r.name + '（副本）'; if (runes(nm) > OR.limits.maxName) nm = [...r.name].slice(0, OR.limits.maxName - 4).join('') + '（副本）'; formOpen({mode: 'copy', id: suggestId(r.id), name: nm, desc: r.desc || '', from: r.id, busy: false}, `复制角色：${r.name}`); },
    'or-meta': () => { const r = cur(); if (!r || r.builtin) return; formOpen({mode: 'meta', roleId: r.id, id: r.id, name: r.name, desc: r.desc || '', busy: false}, `改名 / 描述：${r.name}`); },
    'or-f-go': () => formSubmit(),
    'or-del': () => deleteFlow(),
    'or-matrix': () => { matrixOpen(); rolesLoad(true).then(() => { if ($('#orMx')) $('#orMx').innerHTML = matrixHtml(); }); },
    'or-mx-reload': () => rolesLoad(true).then(() => { if ($('#orMx')) { $('#orMx').innerHTML = matrixHtml(); toast('已刷新'); } }),
    'or-print': () => { document.body.classList.add('or-pm'); const done = () => { document.body.classList.remove('or-pm'); window.removeEventListener('afterprint', done); }; window.addEventListener('afterprint', done); window.print(); setTimeout(done, 1500); },
    'or-member': d => { closeDrawer(); window.navArgs = null; if (window.opUsersSearch) window.opUsersSearch(d.n); go('users'); },
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na^="or-"]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-nc^="or-"]'); if (!el) return; const d = el.dataset, S = OR.sim;
    if (d.nc === 'or-sim') { S[d.k] = el.value; if (d.k === 'cap' && CAPMAP[el.value]) S.risk = secCap(CAPMAP[el.value])[3]; paint(); }
    else if (d.nc === 'or-flag') { S.flags[d.e] = el.checked; paint(); }
    else if (d.nc === 'or-ver') { S.versionOk = el.checked; paint(); }
    else if (d.nc === 'or-mfa') { S.mfa = el.checked; paint(); }
    else if (d.nc === 'or-f' && OR.form) {
      if (d.k === 'initMode') { OR.form.from = el.value === 'copy' ? (OR.roles.find(r => r.id === 'viewer') ? 'viewer' : OR.roles[0].id) : ''; formRender(); }
      else if (d.k === 'from') { OR.form.from = el.value; formRender(); }
    }
  });
  document.addEventListener('input', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-ni="or-f"]'); if (!el || !OR.form) return;
    OR.form[el.dataset.k] = el.value; formLive();
  });

  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || !OP.me) return;
    if (OR.lastUser !== me()) {
      const prev = OR.lastUser; OR.lastUser = me();
      Object.assign(OR, {roles: [], loaded: false, err: '', sig: '', flash: null, form: null, draft: null, draftFor: '', sel: ''});
      if (prev && h === 'roles') { render('roles'); return; }
    }
    if (h === 'roles' && !ctx.open && !ctx.typing && $('#orBody') && Date.now() - OR.at > 9000) { rolesLoad(); polSync(); }
  });
})();
