// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_users.js
// @time: 2026/10/28

/* 平台配置 → 「用户管理」页（pages.users）。从原「权限与审计 → 用户管理」标签迁来并扩展：
     - 工具栏：搜索（账号 / 姓名 / 部门 / 邮箱 / 手机）、状态筛选（正常 / 已禁用 / 已锁定）、角色筛选；
     - 表格：账号 / 姓名 / 部门 / 邮箱·手机 / 角色（环境 → 角色，含自定义角色）/ 授权库 / MFA / 状态 / 最近登录 / 操作；
     - 操作：编辑（资料 + 角色 + 授权库）、重置口令、重置 TOTP、禁用 / 启用、解锁、删除；新建用户抽屉带资料字段；
     - 只有 audit.view 的账号（审计员）只读：所有写按钮置灰并提示所需能力。
   非运维模式（OP.on === false）只显示说明卡片。
   同时提供：
     - 侧栏「平台配置」分组的可见性（OP.on 且拥有 users.manage 或 audit.view 才显示；会话变化时刷新）；
     - window.opRoleName(id) / window.opRoleIsCustom(id)：角色中文名（含自定义角色），供 ops_roles.js / ops_security.js / ops_governance.js 使用。
   data-na / data-nc / data-ni 前缀：ou-。依赖 ops.js（OP / opApi / opCan / opEsc / OP_HOOKS）、ops_pages.js（oppConfirm）、
   ops_governance.js（govAfterProposal，授予管理员 202 提案流程）、security.js（SEC_ROLES，仅作内置角色名的兜底）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); if (isNaN(d)) return '—'; return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const fT = t => { const d = new Date(t); return `${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview').split('/')[0] === h;
  const me = () => (OP.me && OP.me.user && OP.me.user.name) || '';
  const ENVS = [['*', '所有环境（默认）'], ['test', '测试'], ['staging', '预发'], ['prod', '生产']];
  const envLabel = e => (ENVS.find(x => x[0] === e) || [0, e])[1];
  const envShort = e => e === '*' ? '全部环境' : envLabel(e);
  const ENV_ORDER = ['*', 'test', 'staging', 'prod'];
  const clone = o => JSON.parse(JSON.stringify(o));

  /* ---------------- 角色名缓存（含自定义角色） ---------------- */
  const RC = {};   // id -> {name, builtin}
  const rcSet = list => { (list || []).forEach(r => { if (r && r.id) RC[r.id] = {name: r.name || r.id, builtin: !!r.builtin}; }); };
  const rcReplace = list => { Object.keys(RC).forEach(k => delete RC[k]); rcSet(list); };
  const meInfo = id => ((OP.me && OP.me.roleInfo) || []).find(r => r.id === id);
  window.opRoleCacheReplace = rcReplace;
  window.opRoleName = id => {
    if (RC[id]) return RC[id].name;
    const mi = meInfo(id); if (mi) return mi.name;
    const r = typeof SEC_ROLES !== 'undefined' ? SEC_ROLES.find(x => x[0] === id) : null;
    return r ? r[1] : id;
  };
  window.opRoleIsCustom = id => { if (RC[id]) return !RC[id].builtin; const mi = meInfo(id); return mi ? !mi.builtin : false; };
  window.opRoleKnown = id => !!(RC[id] || meInfo(id) || (typeof SEC_ROLES !== 'undefined' && SEC_ROLES.some(x => x[0] === id)));
  const roleName = id => window.opRoleName(id);

  /* ---------------- 侧栏「平台配置」可见性 ---------------- */
  function navSync() {
    if (OP.me && OP.me.roleInfo) rcSet(OP.me.roleInfo);
    const show = !!(OP.on && OP.me && (opCan('users.manage') || opCan('audit.view')));
    const showCl = !!(OP.on && OP.me && window.CL && CL.canManage);   // 「集群接入」只给集群管理员（GET /api/clusters 的 canManage，即 settings.edit）
    $$('.nav [data-plat]').forEach(el => { el.hidden = el.dataset.platG ? !(show || showCl) : el.dataset.platCl ? !showCl : !show; });
    // 每个登录会话拉一次角色表：让「权限与审计」「待确认提案」等页面也能显示自定义角色的中文名
    if (show && rcUser !== me()) {
      rcUser = me();
      opApi('GET', '/api/roles').then(r => { if (r.code === 200 && r.data.roles) rcReplace(r.data.roles.map(x => ({id: x.id, name: x.name, builtin: x.builtin}))); });
    } else if (!show && !OP.me) rcUser = '';
  }
  let rcUser = '';
  // 登录 / 退出 / 会话刷新都会调用 opRenderChip：在它后面顺带同步侧栏（不改 ops.js）
  const _chip = opRenderChip;
  opRenderChip = function () { _chip.apply(this, arguments); navSync(); };
  navSync();

  /* ---------------- 状态 ---------------- */
  const OU = {list: [], roleInfo: [], loaded: false, loading: false, err: '', errCode: 0, at: 0, sig: '', f: {q: '', st: '', role: ''}, form: null, flash: null, lastUser: ''};
  window.opUsersSearch = q => { OU.f = {q: String(q || ''), st: '', role: ''}; };   // 角色权限页「成员」跳转用
  const canW = () => opCan('users.manage');
  const NEED_W = '需要 users.manage 能力（审计员只读）';
  const isLocked = u => !!u.lockedUntil && new Date(u.lockedUntil) > new Date(opNow());
  const hasRoleU = (u, id) => Object.values(u.roles || {}).some(rs => (rs || []).includes(id));

  async function usersLoad() {
    if (OU.loading) return; OU.loading = true;
    const r = await opApi('GET', '/api/users');
    OU.loading = false; OU.at = Date.now();
    if (r.code === 200) {
      const sig = JSON.stringify(r.data);
      OU.list = r.data.users || []; OU.roleInfo = r.data.roleInfo || []; rcReplace(OU.roleInfo);
      OU.err = ''; OU.errCode = 0;
      if (sig !== OU.sig || !OU.loaded) { OU.sig = sig; OU.loaded = true; paint(); } else OU.loaded = true;
    } else { OU.err = r.data.error || ('读取失败（' + r.code + '）'); OU.errCode = r.code; OU.loaded = true; OU.sig = ''; paint(); }
  }
  function paint() { if (OP.on && hashIs('users') && $('#ouBody')) pages.users.body(); }
  const setFlash = (lv, text, extra) => { OU.flash = Object.assign({lv, text}, extra || {}); paint(); };

  /* ---------------- 页面 ---------------- */
  const roleTag = id => {
    if (!window.opRoleKnown(id)) return `<span class="tag warn" title="该角色已被删除，按无权限处理">${E(id)}（已删除）</span>`;
    return window.opRoleIsCustom(id) ? `<span class="tag info" title="自定义角色 ${E(id)}">${E(roleName(id))}<span class="ou-cust">自定义</span></span>` : tg(roleName(id));
  };
  function rolesCell(u) {
    const rs = u.roles || {}, envs = Object.keys(rs).sort((a, b) => (ENV_ORDER.indexOf(a) + 1 || 9) - (ENV_ORDER.indexOf(b) + 1 || 9));
    return envs.map(env => `<div class="ou-renv"><span class="ou-env">${E(envShort(env))}</span>${(rs[env] || []).map(roleTag).join('') || '<span class="muted small">无</span>'}</div>`).join('') || '<span class="muted">无角色</span>';
  }
  function statusCell(u) {
    const out = [u.disabled ? tg('已禁用', 'bad') : isLocked(u) ? '' : tg('正常', 'ok')];
    if (isLocked(u)) out.push(`<span class="tag warn" title="登录失败次数过多，锁定至 ${E(fDT(u.lockedUntil))}">已锁定</span><div class="small muted">至 ${fT(u.lockedUntil)}</div>`);
    return out.join('');
  }
  function filtered() {
    const f = OU.f, q = f.q.trim().toLowerCase();
    return OU.list.filter(u => (!q || [u.name, u.display, u.dept, u.email, u.phone].some(x => String(x || '').toLowerCase().includes(q)))
      && (!f.st || (f.st === 'disabled' ? u.disabled : f.st === 'locked' ? isLocked(u) : (!u.disabled && !isLocked(u))))
      && (!f.role || hasRoleU(u, f.role)));
  }
  function rowHtml(u) {
    const self = u.name === me(), nm = E(u.name), w = canW(), locked = isLocked(u);
    const dis = (extra, why) => !w ? `disabled title="${NEED_W}"` : extra ? `disabled title="${E(why)}"` : '';
    const noSelf = '不能对自己的账号执行该操作（改自己的口令 / MFA 请到「权限与审计 → 我的账号」）';
    return `<tr><td class="mono"><b>${nm}</b>${self ? ' ' + tg('当前', 'info') : ''}</td><td>${E(u.display)}</td><td class="small">${E(u.dept) || '<span class="muted">—</span>'}</td>
      <td class="small">${u.email ? E(u.email) : '<span class="muted">—</span>'}${u.phone ? '<br>' + E(u.phone) : ''}</td><td class="small">${rolesCell(u)}</td>
      <td class="small mono">${E((u.dbs || []).join(', ') || '—')}</td><td>${u.hasTotp ? tg('已绑定', 'ok') : tg('未绑定')}</td><td>${statusCell(u)}</td>
      <td class="small" style="white-space:nowrap">${u.lastLogin ? E(fDT(u.lastLogin)) : '<span class="muted">从未登录</span>'}</td>
      <td><div class="acts"><button class="btn sm" data-na="ou-edit" data-n="${nm}" ${dis()}>编辑</button>
        <button class="btn sm" data-na="ou-pw" data-n="${nm}" ${dis(self, noSelf)}>重置口令</button>
        <button class="btn sm" data-na="ou-totp" data-n="${nm}" ${w ? (u.hasTotp ? '' : 'disabled title="该用户未绑定 MFA"') : `disabled title="${NEED_W}"`}>重置 TOTP</button>
        <button class="btn sm" data-na="ou-dis" data-n="${nm}" data-v="${u.disabled ? 0 : 1}" ${dis(self, '不能禁用自己的账号')}>${u.disabled ? '启用' : '禁用'}</button>
        <button class="btn sm" data-na="ou-unlock" data-n="${nm}" ${w ? (locked ? '' : 'disabled title="该用户当前未被锁定"') : `disabled title="${NEED_W}"`}>解锁</button>
        <button class="btn sm danger" data-na="ou-del" data-n="${nm}" ${dis(self, '不能删除自己的账号')}>删除</button></div></td></tr>`;
  }
  function tblHtml() {
    const list = filtered();
    return `<div class="tbl-wrap"><table><thead><tr><th>账号</th><th>姓名</th><th>部门</th><th>邮箱 / 手机</th><th>角色（环境 → 角色）</th><th>授权库</th><th>MFA</th><th>状态</th><th>最近登录</th><th>操作</th></tr></thead><tbody>
      ${list.map(rowHtml).join('') || `<tr><td colspan="10" class="empty">${OU.loaded ? (OU.list.length ? '没有符合筛选条件的用户' : '没有用户') : '正在读取…'}</td></tr>`}</tbody></table></div>`;
  }
  const countText = () => OU.loaded ? (filtered().length === OU.list.length ? `${OU.list.length} 个用户` : `${filtered().length} / ${OU.list.length} 个用户`) : '读取中…';
  function tblPaint() { const el = $('#ouTbl'); if (el) el.innerHTML = tblHtml(); const c = $('#ouCount'); if (c) c.textContent = countText(); }

  function flashHtml() {
    const f = OU.flash; if (!f) return '';
    return `<div class="notice ${f.lv}" style="margin-bottom:10px"><span>${{ok: '✔', bad: '✖', warn: '⚠', info: 'ℹ'}[f.lv] || 'ℹ'}</span><div style="flex:1">${E(f.text)}</div>${f.go ? `<button class="btn sm" data-go="${f.go}" ${f.tab ? `data-tab="${f.tab}"` : ''}>${E(f.goText || '查看')}</button>` : ''}<button class="btn sm" data-na="ou-flash-x">知道了</button></div>`;
  }
  function bodyHtml() {
    const w = canW(), ro = !w;
    const info = OU.roleInfo.length ? OU.roleInfo : (typeof SEC_ROLES !== 'undefined' ? SEC_ROLES.map(r => ({id: r[0], name: r[1], builtin: true})) : []);
    const err = OU.err ? (OU.errCode === 403
      ? `<div class="notice warn"><span>🔒</span><div>当前账号没有读取用户列表的权限（需要 users.manage 或 audit.view 能力）。可到「<a data-go="roles">角色权限</a>」查看各角色及其成员。</div></div>`
      : `<div class="notice bad"><span>✖</span><div>${E(OU.err)}</div></div>`) : '';
    if (OU.errCode === 403) return `<div class="card"><h2>用户管理 <span class="right"><button class="btn primary sm" disabled title="${NEED_W}">＋ 新建用户</button></span></h2>${err}<div class="notice info"><span>ℹ</span><div>只读：当前账号没有 users.manage 能力，创建 / 编辑 / 禁用 / 重置 / 删除均不可用。</div></div></div>`;
    return `<div class="card"><h2>用户管理 <span class="sub" id="ouCount">${countText()} · 修改角色 / 禁用 / 重置口令 / 删除会吊销其会话</span>
        <span class="right"><button class="btn sm" data-na="ou-reload">刷新</button><button class="btn primary sm" data-na="ou-new" ${w ? '' : `disabled title="${NEED_W}"`}>＋ 新建用户</button></span></h2>
      ${ro && !OU.err ? `<div class="notice info"><span>ℹ</span><div>只读：当前账号没有 users.manage 能力，创建 / 编辑 / 禁用 / 重置 / 删除均不可用。</div></div>` : ''}
      ${flashHtml()}${err}
      <div class="row ou-bar"><div class="field"><label>搜索</label><input type="search" data-ni="ou-f" data-k="q" value="${E(OU.f.q)}" placeholder="账号 / 姓名 / 部门 / 邮箱 / 手机"></div>
        <div class="field"><label>状态</label><select data-nc="ou-f" data-k="st">${[['', '全部状态'], ['ok', '正常'], ['disabled', '已禁用'], ['locked', '已锁定']].map(([k, t]) => `<option value="${k}" ${OU.f.st === k ? 'selected' : ''}>${t}</option>`).join('')}</select></div>
        <div class="field"><label>角色</label><select data-nc="ou-f" data-k="role"><option value="">全部角色</option>${info.map(r => `<option value="${E(r.id)}" ${OU.f.role === r.id ? 'selected' : ''}>${E(r.name)}${r.builtin ? '' : '（自定义）'}</option>`).join('')}</select></div>
        ${OU.f.q || OU.f.st || OU.f.role ? '<button class="btn sm" data-na="ou-clear" style="align-self:flex-end;margin-bottom:10px">清除筛选</button>' : ''}</div>
      <div id="ouTbl">${tblHtml()}</div>
      <div class="small muted" style="margin-top:10px">最终权限 = 用户在当前环境下<b>所有角色能力的并集</b>；<b>环境即数据范围</b>：角色按环境（所有环境 / 测试 / 预发 / 生产）授予，某环境未单独设置时沿用「所有环境」。
        口令策略：至少 10 位、至少 3 类字符（小写 / 大写 / 数字 / 其他）、不得包含用户名。授予管理员角色需要另一名管理员确认（作为提案提交，见「权限与审计 → 待确认提案」）；降级与其他改动立即生效。最后一名管理员不能被删除、禁用或降级。自己的口令 / MFA 请在「权限与审计 → 我的账号」里操作。
        解锁只清除「用户名」维度的失败计数；同一 IP 连续失败仍可能被限流（HTTP 429，最长 10 分钟）。</div></div>`;
  }
  const NOTE_OFF = '<div class="card"><h2>用户管理</h2><div class="notice info"><span>ℹ</span><div>用户与权限管理需要运维模式（启动参数 <code>--data-dir</code>）。当前是原型 / 只读演示模式，没有真实的用户库，这里不提供用户的增删改。</div></div><div class="small muted">运维模式下，这里可以创建用户、维护资料、分配角色（按环境）、禁用 / 解锁 / 重置口令与 MFA；角色本身在「<a data-go="roles">角色权限</a>」里管理。</div></div>';
  const NOTE_DENY = '<div class="card"><h2>用户管理</h2><div class="notice warn"><span>🔒</span><div>当前账号没有查看用户与权限的能力（需要 users.manage 或 audit.view）。</div></div></div>';

  pages.users = {
    title: '用户管理',
    render() {
      if (!OP.on) return NOTE_OFF;
      if (!OP.me || !(opCan('users.manage') || opCan('audit.view'))) return NOTE_DENY;
      return '<div id="ouBody"></div>';
    },
    init() {
      if (!OP.on || !OP.me || !(opCan('users.manage') || opCan('audit.view'))) return;
      const na = window.navArgs; window.navArgs = null;
      if (na && na.role) { OU.f.role = na.role; OU.f.q = ''; OU.f.st = ''; }
      if (OU.lastUser !== me()) { OU.lastUser = me(); Object.assign(OU, {list: [], loaded: false, err: '', sig: '', flash: null}); }
      this.body(); usersLoad();
    },
    body() { const el = $('#ouBody'); if (el) el.innerHTML = bodyHtml(); },
  };

  /* ---------------- 新建 / 编辑抽屉 ---------------- */
  const PROF = ['display', 'dept', 'email', 'phone', 'note'];
  function pwHint(name, pw) {
    if (!pw) return '';
    const kinds = [/[a-z]/, /[A-Z]/, /\d/, /[^A-Za-z0-9]/].filter(r => r.test(pw)).length, bad = [];
    if (pw.length < 10) bad.push('至少 10 位（当前 ' + pw.length + '）');
    if (kinds < 3) bad.push('至少 3 类字符（当前 ' + kinds + ' 类）');
    if (name && pw.toLowerCase().includes(String(name).toLowerCase())) bad.push('不得包含用户名');
    return bad.length ? `<span style="color:var(--warn)">不满足：${E(bad.join('；'))}</span>` : '<span style="color:var(--ok)">格式符合口令策略（最终以后端校验为准）</span>';
  }
  function formHtml() {
    const F = OU.form, create = F.mode === 'create';
    const info = OU.roleInfo.length ? OU.roleInfo : (typeof SEC_ROLES !== 'undefined' ? SEC_ROLES.map(r => ({id: r[0], name: r[1], builtin: true})) : []);
    const builtin = info.filter(r => r.builtin), custom = info.filter(r => !r.builtin);
    const devSel = Object.values(F.roles).some(rs => rs.includes('developer'));
    const newAdm = Object.entries(F.roles).filter(([env, rs]) => rs.includes('admin') && !(((F.base || {})[env]) || []).includes('admin')).map(([env]) => envLabel(env).replace('（默认）', ''));
    const ro = F.selfLock ? 'disabled' : '';
    const roleBox = (env, r, on, cur) => `<label class="small"><input type="checkbox" data-nc="ou-role" data-env="${env}" data-role="${E(r.id)}" ${cur.includes(r.id) ? 'checked' : ''} ${on && !F.selfLock ? '' : 'disabled'}> ${E(r.name)}${r.id === 'admin' ? '<span class="muted">（需另一名管理员确认）</span>' : ''}</label>`;
    return `<h3 class="ou-sec">${create ? '账号与资料' : '资料'}</h3>
      ${create ? `<div class="field"><label>用户名（小写字母 / 数字 / . _ @ -，2–64 位，大写会自动转小写）</label><input type="text" data-ni="ou-uf" data-k="name" value="${E(F.name)}" autocomplete="off" maxlength="64"></div>` : `<div class="kv" style="margin-bottom:10px"><span>账号</span><span class="mono">${E(F.name)}${F.self ? ' ' + tg('当前', 'info') : ''}</span></div>`}
      <div class="ou-g2"><div class="field"><label>姓名（显示名）</label><input type="text" data-ni="ou-uf" data-k="display" value="${E(F.display)}" maxlength="40"></div>
        <div class="field"><label>部门</label><input type="text" data-ni="ou-uf" data-k="dept" value="${E(F.dept)}" maxlength="40"></div>
        <div class="field"><label>邮箱</label><input type="text" data-ni="ou-uf" data-k="email" value="${E(F.email)}" maxlength="120" autocomplete="off"></div>
        <div class="field"><label>手机</label><input type="text" data-ni="ou-uf" data-k="phone" value="${E(F.phone)}" maxlength="30" autocomplete="off"></div></div>
      <div class="field"><label>备注</label><textarea data-ni="ou-uf" data-k="note" rows="2" maxlength="200">${E(F.note)}</textarea></div>
      ${create ? `<div class="field"><label>初始口令（≥10 位、≥3 类字符、不含用户名）</label><input type="password" data-ni="ou-uf" data-k="password" value="${E(F.password)}" autocomplete="new-password"><div class="small" id="ouPwHint" style="min-height:16px">${pwHint(F.name, F.password)}</div></div>` : ''}
      <h3 class="ou-sec">角色（环境 → 角色）</h3>
      ${F.selfLock ? '<div class="notice info"><span>ℹ</span><div>不能修改自己的角色（避免把自己降权后无法恢复）；请让另一名管理员操作。这里只能修改自己的资料。</div></div>' : ''}
      ${newAdm.length ? `<div class="notice warn"><span>⚠</span><div><b>授予管理员需要另一名管理员确认</b>（${E(newAdm.join('、'))}）。${create ? '提交后用户会先以其余角色创建（不含管理员），管理员角色作为提案提交。' : '提交后本次角色变更（含其他改动）会作为提案提交，确认前用户的角色保持不变。'}</div></div>` : ''}
      <div class="ou-rolegrid">${ENVS.map(([env, label]) => { const on = env === '*' || F.roles[env] !== undefined, cur = F.roles[env] || [];
        return `<div>${E(label)}${env !== '*' ? `<div><label class="small"><input type="checkbox" data-nc="ou-envon" data-env="${env}" ${on ? 'checked' : ''} ${ro}> 单独设置</label></div>` : ''}</div>
          <div class="ou-roles"><div class="ou-rg"><span class="ou-rgt">内置</span>${builtin.map(r => roleBox(env, r, on, cur)).join('')}</div>${custom.length ? `<div class="ou-rg"><span class="ou-rgt">自定义</span>${custom.map(r => roleBox(env, r, on, cur)).join('')}</div>` : ''}${on ? '' : '<span class="small muted">沿用「所有环境」</span>'}</div>`; }).join('')}</div>
      <div class="field" style="margin-top:12px"><label>开发角色可查看 SQL 的库（逗号分隔）${devSel ? '' : '（仅「开发」角色或「范围内」授予 view.sql 的角色使用）'}</label><input type="text" data-ni="ou-uf" data-k="dbs" value="${E(F.dbs)}" placeholder="例如 shop, pay" ${ro}></div>
      <div id="ouFormErr" class="small" style="min-height:18px">${F.msgs ? F.msgs : ''}</div>
      <div class="sticky-actions"><button class="btn primary" data-na="ou-uf-go">${create ? '创建用户' : '保存'}</button><button class="btn" data-close="1">取消</button></div>`;
  }
  const formRender = () => { const el = $('#ouForm'); if (el && OU.form) el.innerHTML = formHtml(); };
  function formOpen(F, title) { OU.form = F; openDrawer(title, '<div id="ouForm"></div>'); formRender(); }
  function rolesPayload(roles) {
    const out = {};
    Object.entries(roles).forEach(([env, rs]) => { if (env === '*' ? rs.length : true) out[env] = rs.slice().sort(); });
    return out;
  }
  const errLine = (lv, t) => `<div style="color:var(--${lv === 'ok' ? 'ok' : 'bad'});margin-top:2px">${lv === 'ok' ? '✔' : '✖'} ${E(t)}</div>`;

  async function formSubmit(btn) {
    const F = OU.form, err = $('#ouFormErr'), roles = rolesPayload(F.roles), dbs = F.dbs.split(',').map(s => s.trim()).filter(Boolean);
    const say = h => { F.msgs = h; if (err) err.innerHTML = h; };
    if (F.mode === 'create') {
      if (!F.name.trim()) return say(errLine('bad', '请填写用户名'));
      if (F.password.length < 10) return say(errLine('bad', '口令至少 10 位'));
    }
    if (!Object.values(roles).some(rs => rs.length)) return say(errLine('bad', '请至少为一个环境选择一个角色'));
    btn.disabled = true; say('');
    if (F.mode === 'create') {
      const r = await opApi('POST', '/api/users', {name: F.name.trim(), display: F.display.trim() || F.name.trim(), password: F.password, roles, dbs, dept: F.dept.trim(), email: F.email.trim(), phone: F.phone.trim(), note: F.note.trim()});
      btn.disabled = false;
      if (r.code === 202) { closeDrawer(); OU.form = null; usersLoad(); afterProposal(r.data, F, false); }
      else if (r.code === 200) { closeDrawer(); toast(`已创建用户 ${F.name}`); OU.form = null; await usersLoad(); }
      else say(errLine('bad', r.data.error || '操作失败'));
      return;
    }
    // 编辑：资料与角色 / 授权库是两个接口，按实际改动分别发送，结果分别提示
    const prof = {}; PROF.forEach(k => { if (String(F[k]).trim() !== String(F.baseProf[k] || '').trim()) prof[k] = String(F[k]).trim(); });
    const rolesChanged = !F.selfLock && (JSON.stringify(roles) !== JSON.stringify(rolesPayload(F.base)) || JSON.stringify(dbs) !== JSON.stringify(F.baseDbs));
    if (!Object.keys(prof).length && !rolesChanged) { btn.disabled = false; return say('<span class="muted">没有任何改动</span>'); }
    const out = []; let proposal = null, anyFail = false;
    if (Object.keys(prof).length) {
      const r = await opApi('POST', `/api/users/${encodeURIComponent(F.name)}/profile`, prof);
      if (r.code === 200) { out.push(errLine('ok', '资料已保存')); PROF.forEach(k => { F.baseProf[k] = String(F[k]).trim(); }); }
      else { anyFail = true; out.push(errLine('bad', '资料未保存：' + (r.data.error || ('请求失败（' + r.code + '）')))); }
    }
    if (rolesChanged) {
      const r = await opApi('POST', `/api/users/${encodeURIComponent(F.name)}/roles`, {roles, dbs});
      if (r.code === 202) { proposal = r.data; out.push(errLine('ok', '角色变更已提交为提案，需要另一名管理员确认')); }
      else if (r.code === 200) { out.push(errLine('ok', '角色 / 授权库已保存（其会话已吊销）')); F.base = clone(F.roles); F.baseDbs = dbs; }
      else { anyFail = true; out.push(errLine('bad', '角色 / 授权库未保存：' + (r.data.error || ('请求失败（' + r.code + '）')))); }
    }
    btn.disabled = false;
    if (proposal) { closeDrawer(); OU.form = null; await usersLoad(); afterProposal(proposal, F, out.some(x => x.includes('资料已保存'))); return; }
    await usersLoad();
    if (!anyFail) { closeDrawer(); OU.form = null; toast(`已更新 ${F.name}${Object.keys(prof).length ? ' 的资料' : ''}${rolesChanged ? (Object.keys(prof).length ? '与' : ' 的') + '角色' : ''}`); }
    else say(out.join(''));
  }
  // 授予 admin → 双人复核提案（HTTP 202）
  function afterProposal(data, F, profileSaved) {
    if (typeof govAfterProposal === 'function') govAfterProposal(data, F);
    else toast((data && data.message) || '已提交提案，需要另一名管理员确认');
    const created = data && data.created;
    setFlash('warn', (created ? `用户 ${F.name} 已创建（不含管理员角色）。` : `用户 ${F.name} 的角色变更${profileSaved ? '（资料已保存）' : ''}已提交为提案，确认前其角色保持不变。`) + '授予管理员角色需要另一名管理员在「权限与审计 → 待确认提案」里确认后才生效。', {go: 'security', tab: 'props', goText: '去看提案'});
  }
  async function userAct(path, body, msg, okTitle) {
    const r = await opApi('POST', path, body || {});
    if (r.code === 200) { OU.flash = null; toast(msg); if (r.data && r.data.wasLocked !== undefined) setFlash('ok', msg); await usersLoad(); paint(); }
    else setFlash('bad', (okTitle ? okTitle + '失败：' : '') + (r.data.error || ('操作失败（' + r.code + '）')));
    return r;
  }

  /* ---------------- 事件 ---------------- */
  const CLICK = {
    'ou-flash-x': () => { OU.flash = null; paint(); },
    'ou-reload': () => { OU.flash = null; usersLoad().then(() => toast('已刷新')); },
    'ou-clear': () => { OU.f = {q: '', st: '', role: ''}; paint(); },
    'ou-new': () => formOpen({mode: 'create', name: '', display: '', dept: '', email: '', phone: '', note: '', password: '', roles: {'*': ['viewer']}, base: {}, dbs: '', baseProf: {}, baseDbs: []}, '新建用户'),
    'ou-edit': d => {
      const u = OU.list.find(x => x.name === d.n); if (!u) return;
      const roles = {}; Object.entries(u.roles || {}).forEach(([k, v]) => { roles[k] = (v || []).slice(); });
      const dbs = (u.dbs || []).slice();
      formOpen({mode: 'edit', name: u.name, self: u.name === me(), selfLock: u.name === me(), display: u.display || '', dept: u.dept || '', email: u.email || '', phone: u.phone || '', note: u.note || '',
        roles, base: clone(roles), dbs: dbs.join(', '), baseDbs: dbs, baseProf: {display: u.display || '', dept: u.dept || '', email: u.email || '', phone: u.phone || '', note: u.note || ''}}, '编辑用户：' + u.name);
    },
    'ou-uf-go': (d, el) => formSubmit(el),
    'ou-dis': d => { const dis = d.v === '1'; oppConfirm(dis ? '禁用用户' : '启用用户', `确认${dis ? '禁用' : '启用'}用户 <b>${E(d.n)}</b>？${dis ? '其现有会话会被吊销。' : ''}`, dis ? '确认禁用' : '确认启用', () => userAct(`/api/users/${encodeURIComponent(d.n)}/disable`, {disabled: dis}, dis ? `已禁用 ${d.n}` : `已启用 ${d.n}`, dis ? '禁用' : '启用'), dis); },
    'ou-pw': d => {
      openDrawer('重置口令：' + d.n, `<div class="notice warn"><span>⚠</span><div>重置后该用户的所有会话会被吊销，需用新口令重新登录。</div></div>
        <div class="field"><label>新口令（≥10 位、≥3 类字符、不含用户名）</label><input type="password" id="ouNewPw" data-ni="ou-pwi" data-n="${E(d.n)}" autocomplete="new-password"><div class="small" id="ouPwHint2" style="min-height:16px"></div></div><div id="ouPwErr" class="small" style="color:var(--bad);min-height:18px"></div>
        <div class="sticky-actions"><button class="btn primary" data-na="ou-pw-go" data-n="${E(d.n)}">重置口令</button><button class="btn" data-close="1">取消</button></div>`);
    },
    'ou-pw-go': async (d, el) => {
      const v = $('#ouNewPw').value, err = $('#ouPwErr'); if (v.length < 10) { err.textContent = '口令至少 10 位'; return; }
      el.disabled = true; const r = await opApi('POST', `/api/users/${encodeURIComponent(d.n)}/password`, {password: v}); el.disabled = false;
      if (r.code === 200) { closeDrawer(); toast('口令已重置，其会话已吊销'); OU.flash = null; await usersLoad(); } else err.textContent = r.data.error || '重置失败';
    },
    'ou-totp': d => oppConfirm('重置 TOTP', `确认重置用户 <b>${E(d.n)}</b> 的动态验证码绑定？重置后需要其重新绑定。`, '确认重置', () => userAct(`/api/users/${encodeURIComponent(d.n)}/totp-reset`, {}, `已重置 ${d.n} 的 TOTP`, '重置 TOTP')),
    'ou-unlock': d => userAct(`/api/users/${encodeURIComponent(d.n)}/unlock`, {}, `已解锁 ${d.n}。注意：解锁只清除「用户名」维度的失败计数；同一 IP 连续失败仍可能被限流（HTTP 429，最长 10 分钟），届时请稍后再试。`, '解锁'),
    'ou-del': d => oppConfirm('删除用户', `确认删除用户 <b>${E(d.n)}</b>？不可恢复，其会话会被吊销（历史审计记录保留）。`, '确认删除', () => userAct(`/api/users/${encodeURIComponent(d.n)}/delete`, {}, `已删除用户 ${d.n}`, '删除'), true),
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na^="ou-"]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-nc^="ou-"]'); if (!el) return; const d = el.dataset;
    if (d.nc === 'ou-f') { OU.f[d.k] = el.value; tblPaint(); paintBar(); }
    else if (d.nc === 'ou-role' && OU.form) {
      const cur = OU.form.roles[d.env] || (OU.form.roles[d.env] = []);
      OU.form.roles[d.env] = el.checked ? [...new Set([...cur, d.role])] : cur.filter(x => x !== d.role); formRender();
    } else if (d.nc === 'ou-envon' && OU.form) {
      if (el.checked) OU.form.roles[d.env] = OU.form.roles[d.env] || []; else delete OU.form.roles[d.env];
      formRender();
    }
  });
  // 清除筛选按钮的显隐依赖筛选值：筛选变化后重绘工具栏（不动输入框焦点时才安全，所以只在 change / 清除时调用）
  function paintBar() { const f = OU.f, b = $('.ou-bar .btn[data-na="ou-clear"]'); const has = !!(f.q || f.st || f.role); if (has && !b) paint(); else if (!has && b) b.remove(); }
  document.addEventListener('input', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-ni^="ou-"]'); if (!el) return; const d = el.dataset;
    if (d.ni === 'ou-f') { OU.f[d.k] = el.value; tblPaint(); }
    else if (d.ni === 'ou-uf' && OU.form) {
      OU.form[d.k] = el.value;
      if (d.k === 'password' || d.k === 'name') { const h = $('#ouPwHint'); if (h) h.innerHTML = pwHint(OU.form.name, OU.form.password); }
    } else if (d.ni === 'ou-pwi') { const h = $('#ouPwHint2'); if (h) h.innerHTML = pwHint(d.n, el.value); }
  });

  let capSig = '';
  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || !OP.me) return;
    navSync();
    const sig = JSON.stringify(OP.me.caps || {});   // 管理员改了本人角色的权限：能力变化后重绘当前页（按钮 / 菜单即时变化）
    if (capSig && sig !== capSig && OU.lastUser === me() && !ctx.open && !ctx.typing && h !== 'help') { capSig = sig; render(h); return; }
    capSig = sig;
    if (OU.lastUser !== me()) {   // 会话期间换了登录用户：丢弃上一个用户看到的数据；若正停在本页则整页重绘
      const prev = OU.lastUser; OU.lastUser = me();
      Object.assign(OU, {list: [], loaded: false, err: '', sig: '', flash: null, form: null, f: {q: '', st: '', role: ''}});
      if (prev && h === 'users') { render('users'); return; }
    }
    if (h === 'users' && !ctx.open && !ctx.typing && $('#ouBody') && Date.now() - OU.at > 9000) usersLoad();
  });
})();
