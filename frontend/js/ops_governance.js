// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_governance.js
// @time: 2026/10/28

/* 运维模式下的「双人复核提案」与「可配置审批策略」：叠在 ops_security.js 之上，包装 pages.security（保留原有全部标签页）。
     新增标签页「待确认提案」：授予管理员 / 修改审批策略都要另一名管理员确认；
     「审批策略」标签页在运维模式下改为动态（读 GET /api/ops/policy，管理员可提交修改提案 / 恢复默认）；
   OP.on === false 时完全不介入（pages.security 仍是 security.js 的原型实现）。
   data-na / data-nc 前缀：gov-（另外对 ops_security.js 的 osec-tab 做附加处理）。
   依赖 ops.js（OP / opApi / opCan / OP_HOOKS）、security.js（secState）、ops_security.js、ops_pages.js（oppConfirm）。
   对外：window.govAfterProposal(data, form) —— 「用户管理」（ops_users.js）创建用户 / 改角色返回 202 时调用。
   （原「权限模拟器」的审批级数同步已随模拟器迁到「角色权限」页 ops_roles.js。） */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview') === h;
  const me = () => (OP.me && OP.me.user && OP.me.user.name) || '';
  const isMe = n => String(n || '').toLowerCase() === me().toLowerCase();
  const roleLabel = id => (window.opRoleName ? window.opRoleName(id) : id);
  const _prev = pages.security;

  const KIND = {'grant-admin': '授予管理员', 'approval-policy': '修改审批策略'};
  const PST = {pending: ['info', '待确认'], applied: ['ok', '已生效'], rejected: ['bad', '已拒绝'], cancelled: ['', '已撤回'], expired: ['warn', '已过期']};
  const RISK_L = {low: '低风险', mid: '中风险', high: '高风险'};
  const ENV_L = {test: '测试', staging: '预发', prod: '生产'};
  const envKeyLabel = e => e === '*' ? '所有环境' : (ENV_L[e] || e);
  const kindLabel = k => KIND[k] || k;
  const lvText = (n, m) => (n ? n + ' 级' : '免审批') + (m ? ' + MFA' : '');
  const lvTag = n => n ? tg(n + ' 级', n > 1 ? 'bad' : 'warn') : tg('免审批', 'ok');
  const clone = o => JSON.parse(JSON.stringify(o));

  const gov = {
    user: '', props: null, propsErr: '', propsSig: '', loadedAt: 0, skew: 0, loading: false,
    pol: null, polErr: '', polSig: '', edit: null, editStale: false, showDiff: false,
    flash: null, seen: new Set(),
  };
  // 换了登录用户：丢弃上一个用户的数据
  function ensureUser() {
    if (gov.user === me()) return false;
    const prev = gov.user;
    Object.assign(gov, {user: me(), props: null, propsErr: '', propsSig: '', loadedAt: 0, pol: null, polErr: '', polSig: '', edit: null, editStale: false, showDiff: false, flash: null});
    gov.seen = new Set();
    if (prev) gov.rerender = true;   // 会话期间换了登录用户（不是首次登录）：权限与审计页需要整页重绘
    return !!prev;
  }
  function paintIf(t) { if (OP.on && hashIs('security') && secState.tab === t && $('#secBody')) pages.security.body(); }

  /* ---------------- 数据 ---------------- */
  async function propsLoad() {
    ensureUser();
    if (gov.loading) return; gov.loading = true;
    let r; try { r = await opApi('GET', '/api/ops/proposals'); } finally { gov.loading = false; }
    if (r.code !== 200) { gov.propsErr = r.data.error || ('读取失败（' + r.code + '）'); if (!gov.props) paintIf('props'); return; }
    const d = r.data; gov.propsErr = '';
    gov.skew = d.serverTime ? Date.parse(d.serverTime) - Date.now() : 0; gov.loadedAt = Date.now();
    const sig = JSON.stringify([d.pending, d.history, d.admin, d.ttlMinutes]), changed = sig !== gov.propsSig;
    gov.props = d; gov.propsSig = sig;
    noteNew(); updateBadges();
    if (d.pending.some(p => p.kind === 'approval-policy') && !gov.pol) polLoad();
    if (changed) paintIf('props');
  }
  async function polLoad() {
    ensureUser();
    const r = await opApi('GET', '/api/ops/policy');
    if (r.code !== 200) { gov.polErr = r.data.error || ('读取失败（' + r.code + '）'); if (!gov.pol) paintIf('policy'); return; }
    gov.polErr = '';
    const sig = JSON.stringify(r.data); if (sig === gov.polSig) return;
    const had = !!gov.pol; gov.polSig = sig; gov.pol = r.data;
    if (gov.edit && had) { gov.editStale = true; const s = $('#govStale'); if (s) s.innerHTML = staleHtml(); }
    else { paintIf('policy'); }
    paintIf('props');
  }
  const confirmable = () => gov.props ? gov.props.pending.filter(p => p.canConfirm) : [];
  function noteNew() {
    const mine = confirmable(), fresh = mine.filter(p => !gov.seen.has(p.id));
    mine.forEach(p => gov.seen.add(p.id));
    if (fresh.length && !(hashIs('security') && secState.tab === 'props')) toast(`有 ${fresh.length} 条提案等待你确认（权限与审计 → 待确认提案）`);
  }
  function updateBadges() {
    const d = gov.props, n = d ? d.pending.length : 0, c = confirmable().length;
    const nav = document.querySelector('.nav a[data-go="security"]');
    if (nav) {
      let b = $('#govBadge'); if (!b) { b = document.createElement('span'); b.id = 'govBadge'; b.className = 'badge'; b.title = '待你确认的提案'; nav.appendChild(b); }
      b.style.display = c ? '' : 'none'; b.textContent = c;
    }
    const tb = $('#secTabs [data-t="props"]'); if (tb) tb.innerHTML = tabLabel();
    updateBanner();
    void n;
  }
  const tabLabel = () => { const d = gov.props, n = d ? d.pending.length : 0, c = confirmable().length; return '待确认提案' + (n ? ` <span class="tag ${c ? 'bad' : 'info'}" style="margin-left:4px">${n}</span>` : ''); };
  function updateBanner() {
    const el = $('#govBanner'); if (!el) return;
    const d = gov.props, c = confirmable();
    let h = '';
    if (d && secState.tab !== 'props') {
      if (c.length) h = `<div class="notice warn" style="margin-bottom:12px"><span>⚠</span><div style="flex:1"><b>有 ${c.length} 条提案等待你确认</b>：${E(c[0].summary)}${c.length > 1 ? ` 等` : ''}（发起人 ${E(c[0].by)}）</div><button class="btn sm primary" data-na="gov-goto" data-t="props">去处理</button></div>`;
      else if (d.pending.length) h = `<div class="notice info" style="margin-bottom:12px"><span>ℹ</span><div style="flex:1">你发起的 ${d.pending.length} 条提案正在等待另一名管理员确认。</div><button class="btn sm" data-na="gov-goto" data-t="props">查看</button></div>`;
    }
    el.innerHTML = h;
  }

  /* ---------------- 公共：提案详情 ---------------- */
  const leftText = ms => { if (ms <= 0) return '已到期'; const s = Math.floor(ms / 1000); return `${Math.floor(s / 60)} 分 ${p2(s % 60)} 秒`; };
  const leftNow = exp => Date.parse(exp) - (Date.now() + gov.skew);
  const flashHtml = tab => {
    const f = gov.flash; if (!f || !f.tabs.includes(tab) || Date.now() - f.at > 600000) return '';
    return `<div class="notice ${f.lv}" style="margin-bottom:12px"><span>${{ok: '✔', bad: '✖', warn: '⚠', info: 'ℹ'}[f.lv] || 'ℹ'}</span><div style="flex:1">${E(f.text)}</div>${f.go && secState.tab !== f.go ? `<button class="btn sm" data-na="gov-goto" data-t="${f.go}">${E(f.goText || '查看')}</button>` : ''}<button class="btn sm" data-na="gov-flash-x">知道了</button></div>`;
  };
  const setFlash = (lv, text, tabs, go, goText) => { gov.flash = {lv, text, tabs, go, goText, at: Date.now()}; };

  // 3×3 差异表：cur（当前生效）vs next（提案）；cur 为空时只展示 next
  function diffTable(cur, next, o) {
    const risks = (o && o.risks) || ['low', 'mid', 'high'], envs = (o && o.envs) || ['test', 'staging', 'prod'];
    const cell = (r, e) => {
      const nl = ((next.levels || {})[r] || {})[e] | 0, nm = !!((next.mfa || {})[r] || {})[e];
      if (!cur) return `<td style="text-align:center">${E(lvText(nl, nm))}</td>`;
      const cl = ((cur.levels || {})[r] || {})[e] | 0, cm = !!((cur.mfa || {})[r] || {})[e], ch = cl !== nl || cm !== nm;
      return `<td style="text-align:center;${ch ? 'background:var(--warn-soft)' : ''}">${ch ? `<span class="diff-old">${E(lvText(cl, cm))}</span><br><span class="diff-new">${E(lvText(nl, nm))}</span>` : `<span class="muted">${E(lvText(nl, nm))}</span>`}</td>`;
    };
    return `<div class="tbl-wrap"><table><thead><tr><th>风险</th>${envs.map(e => `<th style="text-align:center">${E(ENV_L[e] || e)}</th>`).join('')}</tr></thead><tbody>
      ${risks.map(r => `<tr><td>${E(RISK_L[r] || r)}</td>${envs.map(e => cell(r, e)).join('')}</tr>`).join('')}</tbody></table></div>`;
  }
  function policyDetail(p) {
    const cur = gov.pol ? gov.pol.policy : null;
    return `<div style="margin-top:10px"><div class="small muted" style="margin-bottom:4px">${cur ? '与当前生效的策略对比（<span class="diff-old">旧值</span> → <span class="diff-new">新值</span>，高亮格为变化）' : '提案内容（正在读取当前策略，暂时无法对比）'}</div>${diffTable(cur, p.payload || {}, gov.pol)}</div>`;
  }
  function grantDetail(p) {
    const pl = p.payload || {}, base = pl.base || {}, roles = pl.roles || {}, envs = [...new Set([...Object.keys(base), ...Object.keys(roles)])];
    const show = (rs, ref, add) => (rs || []).length ? (rs || []).map(r => { const isNew = add ? !(ref || []).includes(r) : !(ref || []).includes(r); return isNew ? `<span class="${add ? 'diff-new' : 'diff-old'}">${E(roleLabel(r))}</span>` : E(roleLabel(r)); }).join('、') : '<span class="muted">无</span>';
    return `<div class="tbl-wrap" style="margin-top:10px"><table><thead><tr><th>环境</th><th>当前角色</th><th>确认后的角色</th></tr></thead><tbody>
      ${envs.map(env => { const b = base[env] || [], n = roles[env] || [], newAdmin = n.includes('admin') && !b.includes('admin'); return `<tr style="${newAdmin ? 'background:var(--warn-soft)' : ''}"><td>${E(envKeyLabel(env))}</td><td>${show(b, n, false)}</td><td>${show(n, b, true)}${newAdmin ? ' ' + tg('新增管理员', 'bad') : ''}</td></tr>`; }).join('')}</tbody></table></div>
      ${(pl.dbs || []).length ? `<div class="small muted" style="margin-top:6px">授权库：${E(pl.dbs.join(', '))}</div>` : ''}`;
  }

  /* ---------------- 标签页：待确认提案 ---------------- */
  function propCard(p, ttl) {
    const self = isMe(p.by), left = leftNow(p.expires);
    const acts = [];
    if (p.canConfirm) acts.push(`<button class="btn primary" data-na="gov-confirm" data-id="${E(p.id)}">确认并生效</button>`);
    if (p.canCancel) acts.push(self ? `<button class="btn danger" data-na="gov-withdraw" data-id="${E(p.id)}">撤回</button>` : `<button class="btn danger" data-na="gov-reject" data-id="${E(p.id)}">拒绝</button>`);
    return `<div class="card" style="margin-bottom:12px;${p.canConfirm ? 'border-color:var(--warn);box-shadow:0 0 0 2px var(--warn-soft)' : ''}">
      <h2>${tg(kindLabel(p.kind), p.kind === 'grant-admin' ? 'bad' : 'warn')} <span>${E(p.summary)}</span><span class="right mono small muted">${E(p.id)}</span></h2>
      <div class="kv"><span>发起人</span><span>${E(p.by)}${self ? ' ' + tg('我', 'info') : ''}</span><span>发起时间</span><span>${fDT(p.at)}</span>
        <span>剩余有效时间</span><span><b data-exp="${E(p.expires)}" style="${left < 300000 ? 'color:var(--bad)' : ''}">${leftText(left)}</b> <span class="muted small">（有效期 ${ttl} 分钟，到期自动作废）</span></span></div>
      ${p.kind === 'approval-policy' ? policyDetail(p) : p.kind === 'grant-admin' ? grantDetail(p) : ''}
      ${!p.canConfirm && self ? '<div class="notice info" style="margin-top:10px"><span>ℹ</span><div>你是发起人，不能确认自己的提案；需要<b>另一名管理员</b>登录后确认。</div></div>' : ''}
      <div class="sticky-actions" style="position:static">${acts.join('') || '<span class="small muted">你没有处理该提案的权限</span>'}</div></div>`;
  }
  function propsHtml() {
    const d = gov.props;
    if (!d) return flashHtml('props') + (gov.propsErr ? `<div class="notice bad"><span>✖</span><div>${E(gov.propsErr)}</div></div>` : '<div class="empty">正在读取提案…</div>');
    const c = confirmable().length, ttl = d.ttlMinutes || 60;
    const head = c ? `<div class="notice warn"><span>⚠</span><div><b>有 ${c} 条提案等待你确认</b>。确认前请核对内容：确认后立即生效，并写入审计。</div></div>`
      : d.pending.length ? '<div class="notice info"><span>ℹ</span><div>没有需要你确认的提案；以下是等待另一名管理员处理的提案。</div></div>' : '';
    return `${flashHtml('props')}${gov.propsErr ? `<div class="notice bad"><span>✖</span><div>${E(gov.propsErr)}</div></div>` : ''}
      <div class="card" style="margin-bottom:12px"><h2>待确认提案 <span class="sub">${d.admin ? '管理员可看到全部提案' : '只显示你自己发起的提案'} · 双人复核：发起人不能自己确认 · ${ttl} 分钟内未确认自动作废</span></h2>
        ${head}
        ${d.pending.map(p => propCard(p, ttl)).join('') || '<div class="empty">没有待确认的提案</div>'}
        <div class="small muted" style="margin-top:6px">授予管理员角色、修改审批策略都需要另一名管理员确认；开启生产写操作开关的双人确认在「写操作开关」页。</div></div>
      <div class="card"><h2>最近历史 <span class="sub">最近 ${d.history.length} 条已处理的提案</span></h2>
        <div class="tbl-wrap"><table><thead><tr><th>编号</th><th>种类</th><th>摘要</th><th>发起人</th><th>发起时间</th><th>状态</th><th>处理人</th><th>备注</th></tr></thead><tbody>
        ${d.history.map(p => { const s = PST[p.state] || ['', p.state]; return `<tr><td class="mono small">${E(p.id)}</td><td class="small">${E(kindLabel(p.kind))}</td><td class="small" style="max-width:340px">${E(p.summary)}</td><td>${E(p.by)}</td><td class="small" style="white-space:nowrap">${fDT(p.at)}</td><td>${tg(s[1], s[0])}</td><td>${E(p.state === 'expired' ? '—' : p.confirmedBy || '—')}</td><td class="small" style="max-width:240px">${E(p.note || '')}</td></tr>`; }).join('') || '<tr><td colspan="8" class="empty">还没有历史提案</td></tr>'}</tbody></table></div></div>`;
  }

  async function propAct(id, act, body, okMsg) {
    const r = await opApi('POST', `/api/ops/proposals/${encodeURIComponent(id)}/${act}`, body || {});
    if (r.code === 200) { toast(okMsg); setFlash('ok', okMsg, ['props']); }
    else { const m = r.data.error || ('操作失败（' + r.code + '）'); toast(m); setFlash('bad', m, ['props']); }
    await Promise.all([propsLoad(), polLoad()]);
    if (act === 'confirm' && r.code === 200) opRefresh(true);   // 角色 / 能力可能变了
    paintIf('props'); updateBanner();
  }
  const findProp = id => gov.props && gov.props.pending.find(p => p.id === id);

  /* ---------------- 标签页：审批策略 ---------------- */
  const sameTable = (a, b, o) => o.risks.every(r => o.envs.every(e => ((a.levels[r] || {})[e] | 0) === ((b.levels[r] || {})[e] | 0) && !!(a.mfa[r] || {})[e] === !!(b.mfa[r] || {})[e]));
  function violations(o, t) {
    const out = {};
    (o.floors || []).forEach(f => {
      const m = [];
      if (((t.levels[f.risk] || {})[f.env] | 0) < f.minLevels) m.push(`${ENV_L[f.env] || f.env}环境的${RISK_L[f.risk] || f.risk}至少 ${f.minLevels} 级（硬性下限）`);
      if (f.mfa && !(t.mfa[f.risk] || {})[f.env]) m.push(`${ENV_L[f.env] || f.env}环境的${RISK_L[f.risk] || f.risk}必须要求 MFA（硬性下限）`);
      if (m.length) out[f.risk + '|' + f.env] = m;
    });
    return out;
  }
  const staleHtml = () => gov.editStale ? '<div class="notice warn"><span>⚠</span><div>生效中的审批策略在你编辑期间已变化（可能有提案刚被确认）。建议「取消」后重新编辑，否则提交的提案会整体覆盖当前策略。</div></div>' : '';
  function policyHtml() {
    const o = gov.pol;
    if (!o) return flashHtml('policy') + (gov.polErr ? `<div class="notice bad"><span>✖</span><div>${E(gov.polErr)}</div></div>` : '<div class="empty">正在读取审批策略…</div>');
    const P = o.policy, D = o.defaults, ed = gov.edit, risks = o.risks, envs = o.envs, pend = o.pendingProposal;
    const nDiff = risks.reduce((n, r) => n + envs.filter(e => ((P.levels[r] || {})[e] | 0) !== ((D.levels[r] || {})[e] | 0) || !!(P.mfa[r] || {})[e] !== !!(D.mfa[r] || {})[e]).length, 0);
    const viol = ed ? violations(o, ed) : {};
    const cell = (r, e) => {
      if (ed) {
        const lv = (ed.levels[r] || {})[e] | 0, mf = !!(ed.mfa[r] || {})[e], v = viol[r + '|' + e], ch = lv !== ((P.levels[r] || {})[e] | 0) || mf !== !!(P.mfa[r] || {})[e];
        return `<td id="govc-${r}-${e}" class="${v ? 'gov-bad' : ch ? 'gov-chg' : ''}" style="text-align:center;min-width:120px"><select data-nc="gov-lv" data-r="${r}" data-e="${e}" aria-label="${E(RISK_L[r] + ' / ' + ENV_L[e] + ' 审批级数')}">${Array.from({length: o.maxLevels + 1}, (_, i) => `<option value="${i}" ${i === lv ? 'selected' : ''}>${i ? i + ' 级' : '免审批'}</option>`).join('')}</select>
          <label class="small" style="display:flex;gap:4px;justify-content:center;margin-top:4px"><input type="checkbox" data-nc="gov-mfa" data-r="${r}" data-e="${e}" ${mf ? 'checked' : ''}> 申请人 MFA</label><div class="small gov-cellmsg" style="color:var(--bad)">${E((v || []).join('；'))}</div></td>`;
      }
      const lv = (P.levels[r] || {})[e] | 0, mf = !!(P.mfa[r] || {})[e], dl = (D.levels[r] || {})[e] | 0, dm = !!(D.mfa[r] || {})[e], ch = lv !== dl || mf !== dm;
      return `<td style="text-align:center;${ch ? 'background:var(--accent-soft)' : ''}">${lvTag(lv)}${mf ? '<div class="small" style="margin-top:2px">+ 申请人 MFA</div>' : ''}${ch ? `<div class="small muted">自定义（默认：${E(lvText(dl, dm))}）</div>` : ''}</td>`;
    };
    const emg = `<tr><td>紧急变更（冻结期）<div class="small muted">固定规则，不可配置</div></td><td style="text-align:center" class="muted">无紧急通道，按常规</td><td style="text-align:center">${tg('至少 2 级', 'bad')}</td><td style="text-align:center">${tg('至少 2 级', 'bad')}<div class="small muted">+ 事后复盘必填</div></td></tr>`;
    const same = ed && sameTable(ed, P, o), nv = Object.keys(viol).length;
    const table = `<div class="tbl-wrap"><table><thead><tr><th>风险</th>${envs.map(e => `<th style="text-align:center">${E(ENV_L[e] || e)}${e === o.env ? ' ' + tg('当前环境', 'info') : ''}</th>`).join('')}</tr></thead><tbody>
      ${risks.map(r => `<tr><td>${E(RISK_L[r] || r)}</td>${envs.map(e => cell(r, e)).join('')}</tr>`).join('')}${emg}</tbody></table></div>`;
    const pendHtml = pend ? `<div class="notice warn"><span>⚠</span><div style="flex:1">已有一条待确认的策略提案 <b class="mono">${E(pend.id)}</b>（发起人 ${E(pend.by)}，剩余 <b data-exp="${E(pend.expires)}">${leftText(leftNow(pend.expires))}</b>）：${E(pend.summary)}${ed ? '。你现在提交的新提案会取代它。' : ''}
        ${pend.canConfirm ? '你可以在「待确认提案」里确认或拒绝。' : isMe(pend.by) ? '你是发起人，需要另一名管理员确认。' : ''}</div><button class="btn sm" data-na="gov-pol-diff">${gov.showDiff ? '收起差异' : '查看差异'}</button><button class="btn sm" data-na="gov-goto" data-t="props">去提案页</button></div>
        ${gov.showDiff ? `<div style="margin:6px 0 12px">${diffTable(P, pend.payload || {}, o)}</div>` : ''}` : '';
    return `${flashHtml('policy')}
      <div class="grid g-main" style="grid-template-columns:minmax(0,1fr) 380px"><div class="grid" style="align-content:start"><div class="card"><h2>审批级数（风险 × 环境）<span class="sub">${o.usingDefault || !nDiff ? '当前与默认策略一致' : `自定义策略 · ${nDiff} 格与默认不同`}</span>
        <span class="right">${ed ? '' : `<button class="btn sm" data-na="gov-pol-reload">刷新</button>${o.canEdit ? `<button class="btn primary sm" data-na="gov-pol-edit">修改</button><button class="btn sm" data-na="gov-pol-default" ${sameTable(P, D, o) ? 'disabled title="当前已是默认策略"' : ''}>恢复默认</button>` : ''}`}</span></h2>
        ${pendHtml}
        ${o.canEdit ? '' : '<div class="notice info"><span>ℹ</span><div>只有管理员（settings.edit 能力）可以修改审批策略；当前账号只读。</div></div>'}
        ${o.mfaDisabled ? '<div class="notice warn"><span>⚠</span><div>当前部署关闭了 MFA（仅限测试 / POC）：表中的「申请人 MFA」要求暂不生效。</div></div>' : ''}
        ${ed ? `<div id="govStale">${staleHtml()}</div><div class="notice info"><span>✎</span><div>编辑中：调整级数与 MFA，违反硬性下限的格子会标红并给出原因。提交后<b>不会立即生效</b>，而是成为提案，需要另一名管理员确认。</div></div>` : ''}
        ${table}
        ${ed ? `<div id="govEditStatus" class="small" style="margin-top:8px">${editStatus(nv, same)}</div><div id="govEditErr" class="small" style="color:var(--bad);min-height:18px;margin-top:4px"></div>
          <div class="sticky-actions" style="position:static"><button class="btn primary" id="govSubmit" data-na="gov-pol-submit" ${same ? 'disabled' : ''}>提交修改提案</button><button class="btn" data-na="gov-pol-cancel">取消</button></div>` : ''}</div></div>
      <div class="grid" style="align-content:start"><div class="card"><h2>规则与硬性下限</h2><ul class="small" style="padding-left:18px;margin:0;display:grid;gap:6px">${(o.limits || []).map(t => `<li>${E(t)}</li>`).join('')}</ul></div>
        <div class="card"><h2>审批规则</h2><ul class="small" style="padding-left:18px;margin:0;display:grid;gap:6px"><li>申请人<b>不能审批自己</b>的变更；同一审批人不能重复审批同一变更</li><li>多级审批必须是<b>不同的人</b>，且具备对应环境的审批角色</li><li>审批有效期 24 小时，过期作废，需重新申请</li><li>审批通过后平台签发短期审批令牌，执行器校验令牌才执行</li><li>这张表决定「要不要审批、几级」；「变更排期」决定「什么时候能执行」，两者都通过才会执行</li></ul></div></div></div>`;
  }
  const editStatus = (nv, same) => same ? '<span class="muted">与当前生效的策略相同，无需提交。</span>' : nv ? `<span style="color:var(--bad)">有 ${nv} 个格子违反硬性下限，提交会被后端拒绝（以后端校验为准）。</span>` : '<span style="color:var(--ok)">满足硬性下限，可以提交；提交后等待另一名管理员确认。</span>';
  // 编辑态的单格联动：不重绘整页，避免丢失焦点
  function editRefresh() {
    const o = gov.pol, ed = gov.edit; if (!o || !ed) return;
    const viol = violations(o, ed);
    o.risks.forEach(r => o.envs.forEach(e => {
      const td = $(`#govc-${r}-${e}`); if (!td) return;
      const v = viol[r + '|' + e], ch = ((ed.levels[r] || {})[e] | 0) !== ((o.policy.levels[r] || {})[e] | 0) || !!(ed.mfa[r] || {})[e] !== !!(o.policy.mfa[r] || {})[e];
      td.className = v ? 'gov-bad' : ch ? 'gov-chg' : '';
      const m = td.querySelector('.gov-cellmsg'); if (m) m.textContent = (v || []).join('；');
    }));
    const same = sameTable(ed, o.policy, o), st = $('#govEditStatus'); if (st) st.innerHTML = editStatus(Object.keys(viol).length, same);
    const b = $('#govSubmit'); if (b) b.disabled = same;
  }
  function submitOk(r, label) {
    const id = (r.data.proposal && r.data.proposal.id) || '';
    const text = `${label}已提交为提案 ${id}，需要另一名管理员确认后才会生效。`;
    toast('已提交，需要另一名管理员确认'); setFlash('ok', text, ['policy', 'props'], 'props', '查看提案');
  }
  async function polPropose(policy, label) {
    const r = await opApi('POST', '/api/ops/policy', {policy});
    if (r.code === 202) { gov.edit = null; gov.editStale = false; submitOk(r, label); await Promise.all([propsLoad(), polLoad()]); pages.security.body(); return true; }
    const m = r.data.error || ('提交失败（' + r.code + '）');
    if (gov.edit && $('#govEditErr')) $('#govEditErr').textContent = m; else { toast(m); setFlash('bad', m, ['policy']); paintIf('policy'); }
    return false;
  }

  /* ---------------- 页面包装 ---------------- */
  pages.security = {
    ..._prev,
    render() {
      if (!OP.on) return _prev.render.call(this);
      ensureUser();
      const tab = `<button data-na="osec-tab" data-t="props" class="${secState.tab === 'props' ? 'active' : ''}">${tabLabel()}</button>`;
      return '<div id="govBanner"></div>' + _prev.render.call(this).replace('<div class="tabs" id="secTabs">', '<div class="tabs" id="secTabs">' + tab);
    },
    async init() {
      if (!OP.on) return _prev.init.call(this);
      ensureUser();
      const na = window.navArgs;
      if (na && na.tab === 'props') { window.navArgs = null; secState.tab = 'props'; }
      if (secState.tab === 'props') this.body(); else await _prev.init.call(this);
      updateBanner();
      propsLoad(); if (['props', 'policy'].includes(secState.tab)) polLoad();
    },
    body() {
      if (!OP.on) return _prev.body.call(this);
      const el = $('#secBody'); if (!el) return;
      if (secState.tab === 'props') el.innerHTML = propsHtml();
      else if (secState.tab === 'policy') el.innerHTML = policyHtml();
      else {
        _prev.body.call(this);
      }
      $$('#secTabs button').forEach(b => b.classList.toggle('active', b.dataset.t === secState.tab));
      updateBanner();
    },
  };

  /* ---------------- 事件 ---------------- */
  const CLICK = {
    'gov-goto': d => { secState.tab = d.t; pages.security.body(); if (d.t === 'props') { propsLoad(); polLoad(); } },
    'gov-flash-x': () => { gov.flash = null; pages.security.body(); },
    'gov-confirm': d => {
      const p = findProp(d.id); if (!p) { toast('提案已不在待确认列表，请刷新'); return; }
      const eff = p.kind === 'grant-admin' ? '该用户将立即获得管理员角色，其现有会话会被吊销，需要重新登录。' : '新的审批策略将立即生效，影响此后提交的所有变更的审批级数。';
      oppConfirm('确认提案 ' + p.id, `<b>${E(kindLabel(p.kind))}</b>：${E(p.summary)}<br>发起人 ${E(p.by)}。${eff}此操作写入审计。`, '确认并生效', () => propAct(d.id, 'confirm', {}, `提案 ${d.id} 已确认并生效`));
    },
    'gov-withdraw': d => { const p = findProp(d.id); oppConfirm('撤回提案', `确认撤回你发起的提案 <b class="mono">${E(d.id)}</b>${p ? '：' + E(p.summary) : ''}？撤回后不会生效，需要时请重新发起。`, '确认撤回', () => propAct(d.id, 'reject', {note: ''}, `提案 ${d.id} 已撤回`), true); },
    'gov-reject': d => {
      const p = findProp(d.id);
      openDrawer('拒绝提案 ' + d.id, `<div class="notice warn"><span>⚠</span><div>${p ? `<b>${E(kindLabel(p.kind))}</b>：${E(p.summary)}<br>发起人 ${E(p.by)}。` : ''}拒绝后该提案作废，发起人可重新发起。</div></div>
        <div class="field"><label>拒绝原因（选填，最多 200 字，会显示在历史里）</label><textarea id="govNote" rows="3" maxlength="200" placeholder="例如：未走工单 / 范围过大"></textarea></div>
        <div class="sticky-actions"><button class="btn danger" data-na="gov-reject-go" data-id="${E(d.id)}">确认拒绝</button><button class="btn" data-close="1">取消</button></div>`);
    },
    'gov-reject-go': d => { const n = ($('#govNote') || {value: ''}).value.trim(); closeDrawer(); propAct(d.id, 'reject', {note: n}, `提案 ${d.id} 已拒绝`); },
    'gov-pol-reload': async () => { gov.polSig = ''; await polLoad(); await propsLoad(); toast('已刷新'); },
    'gov-pol-diff': () => { gov.showDiff = !gov.showDiff; paintIf('policy'); },
    'gov-pol-edit': () => { if (!gov.pol) return; gov.edit = {levels: clone(gov.pol.policy.levels), mfa: clone(gov.pol.policy.mfa)}; gov.editStale = false; paintIf('policy'); },
    'gov-pol-cancel': () => { gov.edit = null; gov.editStale = false; paintIf('policy'); },
    'gov-pol-submit': async (d, el) => { if (!gov.edit) return; el.disabled = true; const ok = await polPropose({levels: gov.edit.levels, mfa: gov.edit.mfa}, '审批策略修改'); if (!ok && $('#govSubmit')) $('#govSubmit').disabled = false; },
    'gov-pol-default': () => {
      const o = gov.pol; if (!o) return;
      oppConfirm('恢复默认审批策略', `把审批策略恢复为默认值：<br>${E(o.risks.map(r => RISK_L[r] + ' ' + o.envs.map(e => ENV_L[e] + ' ' + lvText(o.defaults.levels[r][e] | 0, !!o.defaults.mfa[r][e])).join(' / ')).join('；'))}。<br>恢复同样作为提案提交，需要另一名管理员确认后才生效。`, '提交恢复提案', () => polPropose(clone({levels: o.defaults.levels, mfa: o.defaults.mfa}), '恢复默认审批策略'));
    },
    // ops_security.js 切换标签页之外的附加动作：进入提案 / 策略时拉取数据
    'osec-tab': d => {
      if (d.t === 'props') { propsLoad(); polLoad(); }
      else if (d.t === 'policy') { propsLoad(); polLoad(); }
      updateBanner();
    },
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-nc^="gov-"]'); if (!el || !gov.edit) return; const d = el.dataset;
    if (d.nc === 'gov-lv') gov.edit.levels[d.r][d.e] = +el.value;
    else if (d.nc === 'gov-mfa') gov.edit.mfa[d.r][d.e] = el.checked;
    else return;
    editRefresh();
  });

  // 用户管理里创建用户 / 改角色返回 202（授予 admin 需要另一名管理员确认）时由 ops_security.js 调用
  window.govAfterProposal = function (data, F) {
    const id = (data && data.proposal && data.proposal.id) || '';
    const text = data && data.created
      ? `用户 ${F.name} 已创建（不含管理员角色）。授予管理员角色已提交为提案 ${id}，需要另一名管理员确认后才会生效。`
      : `角色变更已提交为提案 ${id}，需要另一名管理员确认后才会生效；在此之前用户 ${F.name} 的角色保持不变。`;
    toast(data && data.created ? `用户 ${F.name} 已创建（不含管理员角色），授予管理员需要另一名管理员确认` : '角色变更已提交为提案，需要另一名管理员确认');
    setFlash('warn', text, ['props']);
    secState.tab = 'props'; ensureUser();
    if (hashIs('security') && $('#secBody')) pages.security.body();
    propsLoad(); polLoad();
  };

  /* ---------------- 轮询与倒计时 ---------------- */
  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || !OP.me) return;
    ensureUser();
    if (gov.rerender && h === 'security') { gov.rerender = false; render('security'); return; }   // 换了登录用户：标签页 / 权限随之变化，整页重绘
    if (h === 'security') {
      if (ctx.typing) return;
      propsLoad();
      if (['policy', 'props'].includes(secState.tab) && !gov.edit) polLoad();
    } else if (Date.now() - gov.loadedAt > 20000) propsLoad();   // 其他页面低频刷新，只为更新侧栏徽标
  });
  setInterval(() => {
    if (!OP.on || !OP.me || !gov.props) return;
    let expired = false;
    document.querySelectorAll('[data-exp]').forEach(el => {
      const ms = leftNow(el.dataset.exp); el.textContent = leftText(ms); if (ms <= 0) expired = true;
    });
    if (expired && Date.now() - gov.loadedAt > 3000) propsLoad();
  }, 1000);
})();
