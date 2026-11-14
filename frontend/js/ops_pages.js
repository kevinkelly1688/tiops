// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_pages.js
// @time: 2026/10/28

/* 运维模式页面：变更排期 / 环境与设置 / 日常运维 / 总览（事件时间线与告警静默）。
   只在 OP.on === true（后端以 --data-dir 启用运维模式）时生效；OP.on === false 时全部走原型原函数，行为不变。
   做法：加载时包装 pages.schedule / pages.settings / pages.ops / pages.overview，OP.on 为真时走本文件的实现，
   并通过 OP_HOOKS 在后端数据刷新时重绘。事件全部用 data-na / data-nc / data-ni + 文档级委托（CSP 禁止内联事件）。
   本文件的 data-na 前缀：osc-（排期） ost-（设置） oop-（日常运维） oov-（总览） opp-（通用）。
   对外暴露：oscOpen(id) 打开变更详情抽屉；oppConfirm(...) 确认抽屉；oppLoadSettings(force) 读取 /api/ops/settings。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fT = t => { const d = new Date(t); return `${p2(d.getHours())}:${p2(d.getMinutes())}`; };
  const fDT = t => { if (!t) return '—'; const d = new Date(t); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${fT(d)}`; };
  const fMD = t => { const d = new Date(t); return `${d.getMonth() + 1}月${d.getDate()}日`; };
  const toLocalInput = t => { const d = new Date(t); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}T${p2(d.getHours())}:${p2(d.getMinutes())}`; };
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview') === h;
  const typingNow = () => !!(document.activeElement && ['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName));
  const drawerIsOpen = () => !!($('#drawer') && $('#drawer').classList.contains('open'));
  const me = () => (OP.me && OP.me.user && OP.me.user.name) || '';
  const sameUser = (a, b) => String(a || '').trim().toLowerCase() === String(b || '').trim().toLowerCase();
  const DAY = 86400000, MIN = 60000;

  const KIND = {'inst.op': '实例操作', 'scale.out': '扩容', 'scale.in': '缩容', 'config.set': '配置变更', 'maint.enter': '进入维护', 'maint.exit': '结束维护', 'maint.extend': '延长维护',
    'backup.run': '备份', 'backup.restore': '恢复', 'cluster.restart-role': '滚动重启', 'cluster.upgrade': '滚动升级'};
  const kindLabel = k => KIND[k] || k;
  const DONE_ST = ['succeeded', 'failed', 'aborted', 'rejected', 'cancelled', 'expired'];
  const LIVE_ST = ['pending', 'approved', 'scheduled', 'running'];
  const OPP = {settings: null, settingsAt: 0, timeline: null, timelineAt: 0, confirmFn: null};

  async function oppLoadSettings(force) {
    if (!OP.on || !OP.me) return OPP.settings;
    if (!force && OPP.settings && Date.now() - OPP.settingsAt < 15000) return OPP.settings;
    const r = await opApi('GET', '/api/ops/settings');
    if (r.code === 200) { OPP.settings = r.data.settings || {}; OPP.settingsAt = Date.now(); }
    return OPP.settings;
  }
  window.oppLoadSettings = oppLoadSettings;

  /* 通用确认抽屉 */
  function oppConfirm(title, msgHtml, okText, fn, danger) {
    OPP.confirmFn = fn;
    openDrawer(title, `<div class="notice ${danger ? 'bad' : 'warn'}"><span>⚠</span><div>${msgHtml}</div></div>
      <div class="sticky-actions"><button class="btn ${danger ? 'danger' : 'primary'}" data-na="opp-yes">${E(okText)}</button><button class="btn" data-close="1">取消</button></div>`);
  }
  window.oppConfirm = oppConfirm;

  /* ======================================================================
     1. 变更排期（服务端 OP.changes）
     ====================================================================== */
  const _sched = pages.schedule;
  const OSC_VIEW_KEY0 = 'tidbops.sched.view';
  const oscLoadView = () => { try { return localStorage.getItem(OSC_VIEW_KEY0) === 'cal' ? 'cal' : 'table'; } catch (e) { return 'table'; } };
  const osc = {week: 0, showDone: true, openId: null, view: oscLoadView(), stF: 'all', rkF: 'all', q: '', group: true, hideEmpty: true};
  const weekStartOf = t => { const d = new Date(t); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - ((d.getDay() + 6) % 7)); return d.getTime(); };
  const nextDayOf = t => { const d = new Date(t); d.setDate(d.getDate() + 1); d.setHours(0, 0, 0, 0); return d.getTime(); };
  const winOf = c => { const s = new Date(c.scheduledAt).getTime(); return [s, s + Math.max(5, (c.plan && c.plan.estMins) || 30) * MIN]; };
  const PXM = 28 / 60;
  const WD = ['一', '二', '三', '四', '五', '六', '日'];
  const ST_FLOW_BAD = ['rejected', 'cancelled', 'expired', 'failed', 'aborted'];

  // 原型里的模拟排期在运维模式下不再使用：清空，避免其定时器误触发模拟任务
  let schedNeutralized = false;
  function neutralizeSim() {
    if (schedNeutralized) return; schedNeutralized = true;
    try { if (typeof SCHEDULES !== 'undefined') SCHEDULES.length = 0; } catch (e) { /* ignore */ }
    if (typeof updateSchedBadge === 'function') window.updateSchedBadge = () => { if (OP.on) opUpdateBadges(); };
    if (typeof nextSchedNotice === 'function') {
      const orig = nextSchedNotice;
      window.nextSchedNotice = () => {
        if (!OP.on) return orig();
        const now = opNow(), list = OP.changes.filter(c => c.scheduledAt && ['pending', 'approved', 'scheduled'].includes(c.state) && new Date(c.scheduledAt) < now + 3 * DAY && new Date(c.scheduledAt) > now - 3600e3)
          .sort((a, b) => new Date(a.scheduledAt) - new Date(b.scheduledAt));
        if (!list.length) return '';
        const pend = OP.changes.filter(c => c.state === 'pending').length, c = list[0];
        return `<div class="notice info" style="margin-bottom:12px"><span>📅</span><div><b>下一个变更排期：</b>${fDT(c.scheduledAt)} · ${E(c.title)}（${E((OP_STATE[c.state] || [0, c.state])[1])}）${pend ? ` · 另有 ${pend} 个待审批` : ''} <a data-go="schedule">查看排期 →</a></div></div>`;
      };
    }
  }

  function approveCheck(c) {
    if (c.state !== 'pending') return {ok: false, why: '该变更不在待审批状态'};
    if (sameUser(c.applicant, me())) return {ok: false, why: '申请人不能审批自己的变更'};
    if ((c.approvals || []).some(a => sameUser(a.user, me()))) return {ok: false, why: '你已审批过该变更'};
    const cap = c.risk === 'high' ? 'approve.high' : c.risk === 'mid' ? 'approve.mid' : '';
    if (!cap || !opCan(cap)) return {ok: false, why: '你没有审批该风险等级变更的权限'};
    return {ok: true};
  }
  const rejectCheck = c => {
    if (c.state !== 'pending') return {ok: false};
    if (sameUser(c.applicant, me())) return {ok: false, why: '申请人不能驳回自己的变更，请使用取消'};
    const cap = c.risk === 'high' ? 'approve.high' : c.risk === 'mid' ? 'approve.mid' : '';
    return cap && opCan(cap) ? {ok: true} : {ok: false, why: '你没有审批该风险等级变更的权限'};
  };
  const cancelCheck = c => ['pending', 'approved', 'scheduled'].includes(c.state) && (sameUser(c.applicant, me()) || opHasRole('dba'));

  const resOf = c => { const s = new Set(); ((c.plan && c.plan.targets) || []).forEach(t => s.add('i:' + t)); ((c.plan && c.plan.hosts) || []).forEach(h => s.add('h:' + h)); return s; };
  function conflictsOf(list) {
    const out = [];
    for (let i = 0; i < list.length; i++) for (let j = i + 1; j < list.length; j++) {
      const [s1, e1] = winOf(list[i]), [s2, e2] = winOf(list[j]);
      if (!(s1 < e2 && s2 < e1)) continue;
      const a = resOf(list[i]), shared = [...resOf(list[j])].filter(x => a.has(x)).map(x => x.slice(2));
      if (shared.length) out.push({a: list[i], b: list[j], shared});
    }
    return out;
  }
  const freezesOf = () => ((OPP.settings && OPP.settings.freezes) || []).map(f => ({from: Date.parse(f.from), to: Date.parse(f.to), label: f.label || '变更冻结', raw: f}));

  /* 变更窗口规则着色：settings.windows 里 enabled 的规则按「星期 + 时间段」映射到周视图，跨午夜规则拆成两段（次日 00:00 起的部分按起点所在日判断星期） */
  const hmMin = s => { const m = /^(\d{1,2}):(\d{2})$/.exec(String(s || '').trim()); return m ? +m[1] * 60 + +m[2] : null; };
  const WZ_ORDER = {prefer: 0, avoid: 1, block: 2}, WZ_CN = {block: '禁止', avoid: '不建议', prefer: '推荐'};
  const windowsOf = () => ((OPP.settings && OPP.settings.windows) || []).filter(w => w && w.enabled && WZ_ORDER[w.mode] !== undefined);
  function wzHtml(wins, d) {
    const dow = new Date(d).getDay(), prev = (dow + 6) % 7, out = [];
    wins.forEach(w => {
      const f = hmMin(w.from), t = hmMin(w.to); if (f === null || t === null || f === t) return;
      const on = x => !w.days || !w.days.length || w.days.includes(x);
      if (on(dow)) out.push({w, a: f, b: f < t ? t : 1440});
      if (f > t && on(prev)) out.push({w, a: 0, b: t});
    });
    return out.sort((x, y) => WZ_ORDER[x.w.mode] - WZ_ORDER[y.w.mode]).map(x => {
      const rk = (x.w.risks || []).length ? '，适用风险 ' + x.w.risks.map(r => ({low: '低', mid: '中', high: '高'}[r] || r)).join('/') : '';
      return `<div class="osc-wz ${x.w.mode}" style="top:${x.a * PXM}px;height:${(x.b - x.a) * PXM}px" title="${E(x.w.name + '（' + WZ_CN[x.w.mode] + rk + '）')}"><span>${(x.b - x.a) * PXM >= 16 ? E(x.w.name) : ''}</span></div>`;
    }).join('');
  }

  function oscBlocks(shown, d, d1, now) {
    const segs = shown.map(c => { const [s, e] = winOf(c), a = Math.max(s, d), b = Math.min(e, d1); return a < b ? {c, s, a, b} : null; }).filter(Boolean).sort((x, y) => x.a - y.a);
    const laneEnd = [];
    segs.forEach(sg => { let l = laneEnd.findIndex(end => end <= sg.a); if (l < 0) { l = laneEnd.length; laneEnd.push(0); } laneEnd[l] = sg.b; sg.lane = l; });
    const n = Math.max(1, laneEnd.length);
    return segs.map(sg => {
      const h = Math.max(18, (sg.b - sg.a) / MIN * PXM), c = sg.c;
      return `<div class="osc-blk osc-st-${c.state}" style="top:${(sg.a - d) / MIN * PXM}px;height:${h}px;left:calc(${sg.lane * 100 / n}% + 1px);width:calc(${100 / n}% - 2px)" data-na="osc-open" data-id="${E(c.id)}" title="${E(c.id + ' ' + c.title + '（' + ((OP_STATE[c.state] || [0, c.state])[1]) + '，' + fT(sg.s) + ' 开始）')}">${fT(sg.s)} ${E(kindLabel(c.kind))}${h >= 36 ? `<br>${E(c.title)}` : ''}</div>`;
    }).join('');
  }

  /* ---- 排期表（表格视图）：与日历共用 OP.changes / 冻结期 / 窗口规则 ---- */
  const fMd = t => { const d = new Date(t); return `${d.getMonth() + 1}/${d.getDate()}`; };
  const OSC_ST_GROUP = {pending: ['pending'], scheduled: ['scheduled', 'approved'], running: ['running'], done: DONE_ST};
  const WZ_TAG = {block: 'bad', avoid: 'warn', prefer: 'ok'};
  const OSC_VIEW_KEY = 'tidbops.sched.view';
  const oscSaveView = v => { try { localStorage.setItem(OSC_VIEW_KEY, v); } catch (e) { /* 浏览器禁用存储时只是不记住 */ } };

  /* 窗口规则命中：与日历底色同一套规则（含跨午夜拆段）；规则限定了适用风险时，只对这些风险的变更算命中 */
  function oscRuleHits(c, wins) {
    const [s, e] = winOf(c), out = [];
    for (let d = new Date(s).setHours(0, 0, 0, 0); d < e; d = nextDayOf(d)) {
      const dow = new Date(d).getDay(), prev = (dow + 6) % 7;
      wins.forEach(w => {
        if ((w.risks || []).length && !w.risks.includes(c.risk)) return;
        const f = hmMin(w.from), t = hmMin(w.to); if (f === null || t === null || f === t) return;
        const on = x => !w.days || !w.days.length || w.days.includes(x), seg = [];
        if (on(dow)) seg.push([f, f < t ? t : 1440]);
        if (f > t && on(prev)) seg.push([0, t]);
        if (seg.some(([a, b]) => s < d + b * MIN && e > d + a * MIN) && !out.includes(w)) out.push(w);
      });
    }
    return out.sort((x, y) => WZ_ORDER[y.mode] - WZ_ORDER[x.mode]);
  }
  function oscDayRules(wins, d) {
    const dow = new Date(d).getDay(), prev = (dow + 6) % 7;
    return wins.filter(w => {
      const f = hmMin(w.from), t = hmMin(w.to); if (f === null || t === null || f === t) return false;
      const on = x => !w.days || !w.days.length || w.days.includes(x);
      return on(dow) || (f > t && on(prev));
    });
  }

  function oscRows() {
    const now = opNow(), w0 = osc.week || weekStartOf(now);
    const days = Array.from({length: 7}, (_, i) => { const d = new Date(w0); d.setDate(d.getDate() + i); d.setHours(0, 0, 0, 0); return d.getTime(); });
    const wEnd = nextDayOf(days[6]), fz = freezesOf(), wins = windowsOf();
    const conf = conflictsOf(OP.changes.filter(c => c.scheduledAt && LIVE_ST.includes(c.state)));
    const q = osc.q.trim().toLowerCase();
    const list = OP.changes.filter(c => {
      if (!c.scheduledAt) return false;
      const s = winOf(c)[0]; if (s < days[0] || s >= wEnd) return false;
      if (!osc.showDone && DONE_ST.includes(c.state)) return false;
      if (osc.stF !== 'all' && !OSC_ST_GROUP[osc.stF].includes(c.state)) return false;
      if (osc.rkF !== 'all' && c.risk !== osc.rkF) return false;
      if (q && ![c.id, c.title, c.applicant, ...((c.plan && c.plan.targets) || []), ...((c.plan && c.plan.hosts) || [])].join(' ').toLowerCase().includes(q)) return false;
      return true;
    }).sort((a, b) => winOf(a)[0] - winOf(b)[0]);
    const row = c => {
      const [s, e] = winOf(c), hits = oscRuleHits(c, wins), f = fz.find(x => s < x.to && e > x.from);
      const cf = conf.filter(x => x.a.id === c.id || x.b.id === c.id).map(x => x.a.id === c.id ? x.b.id : x.a.id);
      const targets = (c.plan && c.plan.targets) || [];
      const tgt = targets.slice(0, 3).join('、') + (targets.length > 3 ? ` 等 ${targets.length} 个` : '') || ((c.plan && c.plan.hosts) || []).join('、') || '—';
      const win = hits.length ? hits.map(w => tg(`${w.name} · ${WZ_CN[w.mode]}`, WZ_TAG[w.mode])).join(' ') : '<span class="muted small">无规则命中</span>';
      const a = c.state === 'pending' ? approveCheck(c) : null;
      return `<tr class="click${DONE_ST.includes(c.state) ? ' osc-past' : ''}" data-na="osc-open" data-id="${E(c.id)}">
        <td class="mono small" style="white-space:nowrap"><span class="osc-bar st-${E(c.state)}"></span>${fMd(s)} ${fT(s)}–${fT(e)}<div class="muted" style="margin-left:11px">${Math.round((e - s) / MIN)} 分钟</div></td>
        <td class="mono small">${E(c.id)}</td>
        <td><b>${E(c.title)}</b><div class="small muted">${E(kindLabel(c.kind))}${c.emergency ? ' · ' + tg('紧急', 'bad') : ''}${c.reason && ST_FLOW_BAD.includes(c.state) ? ' · ' + E(c.reason) : ''}</div></td>
        <td>${opRiskTag(c.risk)}</td><td>${opStateTag(c.state)}</td><td class="small">${E(tgt)}</td>
        <td class="small">${E(c.applicant)}<div class="muted">${c.need ? `审批 ${(c.approvals || []).length}/${c.need}` : '免审批'}</div></td>
        <td class="small">${win}${f ? ' ' + tg('冻结期：' + f.label, 'bad') : ''}${cf.length ? ' ' + tg('冲突 ' + cf.join('、'), 'bad') : ''}</td>
        <td style="white-space:nowrap">${a && a.ok ? `<button class="btn sm primary" data-na="osc-approve" data-id="${E(c.id)}" title="打开详情，核对后再确认通过">审批</button>` : ''}</td></tr>`;
    };
    let html = '';
    if (!osc.group) html = list.map(row).join('');
    else days.forEach(d => {
      const d1 = nextDayOf(d), items = list.filter(c => { const s = winOf(c)[0]; return s >= d && s < d1; });
      if (!items.length && osc.hideEmpty) return;
      const today = now >= d && now < d1, frozen = fz.some(x => d < x.to && d1 > x.from);
      const rules = oscDayRules(wins, d).map(w => `${w.name}（${WZ_CN[w.mode]} ${w.from}–${w.to}）`).join('；');
      html += `<tr class="osc-day${today ? ' today' : ''}"><td colspan="9">周${WD[(new Date(d).getDay() + 6) % 7]} ${fMd(d)}${today ? ' · 今天' : ''} <span class="small muted" style="font-weight:400">· ${items.length} 个排期${rules ? ' · 当日窗口：' + E(rules) : ''}</span>${frozen ? ' ' + tg('冻结期', 'bad') : ''}</td></tr>`
        + (items.length ? items.map(row).join('') : '<tr class="osc-none"><td colspan="9">无排期</td></tr>');
    });
    if (!html) html = '<tr class="osc-none"><td colspan="9">没有符合条件的排期</td></tr>';
    return {html, n: list.length};
  }

  function oscTableCard() {
    const {html, n} = oscRows();
    const chip = (act, v, t, cur) => `<span class="chip ${cur === v ? 'active' : ''}" data-na="${act}" data-v="${v}">${t}</span>`;
    return `<div class="card"><h2>排期表 <span class="sub">${window.CL && CL.active() ? '集群 ' + E(CL.name()) + ' · ' : ''}<span id="oscTblSub">本周 ${n} 个排期</span></span></h2>
      <div class="row" style="margin-bottom:10px;gap:10px">
        <div class="chips">${[['all', '全部'], ['pending', '待审批'], ['scheduled', '已排期'], ['running', '执行中'], ['done', '已结束']].map(([v, t]) => chip('osc-st', v, t, osc.stF)).join('')}</div>
        <div class="chips">${[['all', '风险：全部'], ['high', '高'], ['mid', '中'], ['low', '低']].map(([v, t]) => chip('osc-rk', v, t, osc.rkF)).join('')}</div>
        <input type="search" data-ni="osc-q" placeholder="搜索编号 / 标题 / 实例 / 申请人" value="${E(osc.q)}" style="min-width:220px">
        <label class="small"><input type="checkbox" data-nc="osc-group" ${osc.group ? 'checked' : ''}> 按日期分组</label>
        <label class="small"><input type="checkbox" data-nc="osc-hide" ${osc.hideEmpty ? 'checked' : ''} ${osc.group ? '' : 'disabled'}> 隐藏没有排期的日期</label></div>
      <div class="tbl-wrap"><table><thead><tr><th>计划时间</th><th>编号</th><th>变更</th><th>风险</th><th>状态</th><th>影响对象</th><th>申请人 / 审批</th><th>窗口 / 冻结 / 冲突</th><th></th></tr></thead><tbody id="oscTb">${html}</tbody></table></div></div>`;
  }
  /* 搜索框输入时只刷新表体，不重绘整页（否则输入框会丢失焦点） */
  function oscTableOnly() {
    const tb = $('#oscTb'); if (!tb) return;
    const {html, n} = oscRows();
    tb.innerHTML = html;
    const sub = $('#oscTblSub'); if (sub) sub.textContent = `本周 ${n} 个排期`;
  }

  function oscRulesCard(wins) {
    const DN = ['日', '一', '二', '三', '四', '五', '六'];
    return `<div class="card" style="margin-top:16px"><h2>窗口规则 <span class="sub">来自「环境与设置 → 变更窗口规则」；表格「窗口」列按这些规则判定</span></h2>
      ${wins.length ? `<div class="tbl-wrap"><table><thead><tr><th>名称</th><th>类型</th><th>星期</th><th>时段</th><th>适用风险</th></tr></thead><tbody>
        ${wins.map(w => `<tr><td>${E(w.name)}</td><td>${tg(WZ_CN[w.mode], WZ_TAG[w.mode])}</td><td class="small">${(w.days || []).length ? w.days.map(x => '周' + DN[x]).join(' ') : '每天'}</td><td class="mono small">${E(w.from)}–${E(w.to)}</td><td class="small">${(w.risks || []).length ? w.risks.map(r => ({low: '低', mid: '中', high: '高'}[r] || r)).join(' / ') : '全部'}</td></tr>`).join('')}</tbody></table></div>`
      : '<div class="small muted">没有启用的窗口规则。</div>'}</div>`;
  }

  function oscBody() {
    const el = $('#oscBody'); if (!el) return;
    const now = opNow(); if (!osc.week) osc.week = weekStartOf(now);
    const days = Array.from({length: 7}, (_, i) => { const d = new Date(osc.week); d.setDate(d.getDate() + i); d.setHours(0, 0, 0, 0); return d.getTime(); });
    const sched = OP.changes.filter(c => c.scheduledAt);
    const shown = sched.filter(c => osc.showDone || !DONE_ST.includes(c.state));
    const liveSched = sched.filter(c => ['pending', 'approved', 'scheduled', 'running'].includes(c.state));
    const pend = OP.changes.filter(c => c.state === 'pending').sort((a, b) => new Date(a.created) - new Date(b.created));
    const fz = freezesOf(), conf = conflictsOf(liveSched);
    const inFz = liveSched.map(c => { const [s, e] = winOf(c), f = fz.find(x => s < x.to && e > x.from); return f ? {c, f} : null; }).filter(Boolean);
    const canEdit = opCan('settings.edit');
    const weekEnd = nextDayOf(days[6]) - DAY, wins = windowsOf();
    const cols = days.map((d, di) => {
      const d1 = nextDayOf(d), today = now >= d && now < d1;
      const zones = fz.map(f => { const a = Math.max(f.from, d), b = Math.min(f.to, d1); return a < b ? `<div class="osc-fz" style="top:${(a - d) / MIN * PXM}px;height:${(b - a) / MIN * PXM}px"><span>${E(f.label)}</span></div>` : ''; }).join('');
      return `<div class="osc-col ${today ? 'today' : ''}">${wzHtml(wins, d)}${zones}${oscBlocks(shown, d, d1, now)}${today ? `<div class="osc-now" style="top:${(now - d) / MIN * PXM}px"></div>` : ''}</div>`;
    }).join('');
    const heads = days.map((d, i) => { const d1 = nextDayOf(d), today = now >= d && now < d1, f = fz.some(x => d < x.to && d1 > x.from); return `<div class="osc-dh ${today ? 'today' : ''}">周${WD[i]} ${new Date(d).getMonth() + 1}/${new Date(d).getDate()}${f ? ' <span class="tag bad" style="line-height:16px">冻结</span>' : ''}</div>`; }).join('');
    const stCnt = k => OP.changes.filter(c => c.state === k).length;
    const html = `
      <div class="card" style="margin-bottom:14px"><div class="row">
        <b>${fMD(days[0])} – ${fMD(weekEnd)}</b>
        <div class="chips"><button class="btn sm" data-na="osc-week" data-d="-1">‹ 上一周</button><button class="btn sm" data-na="osc-week" data-d="0">本周</button><button class="btn sm" data-na="osc-week" data-d="1">下一周 ›</button></div>
        <span class="small muted">服务器时间 <b>${fDT(now)}</b> · ${tg('待审批 ' + stCnt('pending'), stCnt('pending') ? 'info' : '')} ${tg('已排期 ' + stCnt('scheduled'), stCnt('scheduled') ? 'warn' : '')} ${tg('执行中 ' + stCnt('running'), stCnt('running') ? 'info' : '')}</span>
        <label class="small"><input type="checkbox" data-nc="osc-done" ${osc.showDone ? 'checked' : ''}> 显示已结束</label>
        <span style="margin-left:auto" class="row"><span class="chips"><span class="chip ${osc.view === 'table' ? 'active' : ''}" data-na="osc-view" data-v="table">表格</span><span class="chip ${osc.view === 'cal' ? 'active' : ''}" data-na="osc-view" data-v="cal">日历</span></span><button class="btn primary" data-na="osc-new">＋ 新建排期</button></span></div>
        <div class="small muted" style="margin-top:8px">数据来自后端变更引擎：只有选择了「定时执行」的变更会出现在表格和日历里；日历里颜色按状态区分，虚线 = 待审批，斜纹 = 冻结期；底色来自「环境与设置 → 变更窗口规则」（红斜纹 = 禁止、橙 = 不建议、绿 = 推荐）。在「提交变更」面板的「推荐时间」里可一键选用按窗口规则打分的时段。创建时若与已排期变更在同一资源、同一时间窗口冲突，后端会直接拒绝。</div></div>
      ${conf.map(x => `<div class="notice bad"><span>✖</span><div>冲突：<a data-na="osc-open" data-id="${E(x.a.id)}">${E(x.a.id)}</a> 与 <a data-na="osc-open" data-id="${E(x.b.id)}">${E(x.b.id)}</a> 的计划窗口重叠，且涉及同一资源（${E(x.shared.slice(0, 4).join('、'))}）</div></div>`).join('')}
      ${inFz.map(x => `<div class="notice warn"><span>⚠</span><div><a data-na="osc-open" data-id="${E(x.c.id)}">${E(x.c.id)}</a> 的计划窗口落在冻结期「${E(x.f.label)}」内${x.c.emergency ? '（紧急变更）' : '：非紧急变更不应被批准，请核对'}</div></div>`).join('')}
      ${osc.view === 'cal' ? '' : oscTableCard()}
      <div class="card"${osc.view === 'cal' ? '' : ' style="display:none"'}><h2>周视图 <span class="sub">${window.CL && CL.active() ? "集群 " + E(CL.name()) + " · " : ""}${shown.length} 个排期</span></h2>
        <div class="osc-wk"><div class="osc-corner"></div>${heads}<div class="osc-hours">${Array.from({length: 24}, (_, h) => `<div style="top:${h * 28}px">${p2(h)}:00</div>`).join('')}</div>${cols}</div>
        <div class="legend"><span><i style="background:var(--accent)"></i>待审批 / 排队</span><span><i style="background:var(--warn)"></i>已排期</span><span><i style="background:var(--brand)"></i>执行中</span><span><i style="background:var(--ok)"></i>成功</span><span><i style="background:var(--bad)"></i>失败</span><span><i style="background:var(--muted)"></i>已取消 / 驳回 / 中止</span><span style="color:var(--bad)">▨ 冻结期</span><span><i class="sw-block"></i>窗口：禁止</span><span><i class="sw-avoid"></i>窗口：不建议</span><span><i class="sw-prefer"></i>窗口：推荐</span></div></div>
      <div class="grid g2" style="margin-top:16px">
        <div class="card"><h2>待审批队列 <span class="sub">${pend.length}</span></h2>${oscPendHtml(pend)}</div>
        <div class="card"><h2>变更冻结期 <span class="sub">${fz.length} 个</span></h2>${oscFreezeHtml(fz, canEdit)}</div></div>${osc.view === 'cal' ? '' : oscRulesCard(wins)}`;
    if (el._h === html) return;   // 内容没变化就不重绘（避免轮询打断点击 / 滚动）
    el._h = html; el.innerHTML = html;
  }

  function oscPendHtml(pend) {
    if (!pend.length) return '<div class="empty">没有待审批的变更</div>';
    return `<div class="tbl-wrap"><table><thead><tr><th>编号</th><th>内容</th><th>风险</th><th>申请人</th><th>审批</th><th></th></tr></thead><tbody>
      ${pend.map(c => { const a = approveCheck(c);
        return `<tr><td class="mono small">${E(c.id)}</td><td style="max-width:220px">${E(c.title)}<div class="small muted">${c.scheduledAt ? '计划 ' + fDT(c.scheduledAt) : '审批后立即执行'}${c.emergency ? ' · ' + tg('紧急', 'bad') : ''}</div></td><td>${opRiskTag(c.risk)}</td><td class="small">${E(c.applicant)}</td><td class="small">${(c.approvals || []).length}/${c.need}</td>
        <td><div class="acts"><button class="btn sm" data-na="osc-open" data-id="${E(c.id)}">详情</button>${a.ok ? `<button class="btn sm primary" data-na="osc-approve" data-id="${E(c.id)}" title="打开详情，核对后再确认通过">审批</button>` : `<button class="btn sm" disabled title="${E(a.why || '')}">审批</button>`}</div></td></tr>`; }).join('')}</tbody></table></div>`;
  }

  function oscFreezeHtml(fz, canEdit) {
    const now = opNow(), list = fz.map((f, i) => ({...f, i})).sort((a, b) => a.from - b.from);
    return `${list.length ? `<div class="tbl-wrap"><table><thead><tr><th>名称</th><th>起</th><th>止</th><th>状态</th><th></th></tr></thead><tbody>${list.map(f => `<tr><td>${E(f.label)}</td><td class="small">${fDT(f.from)}</td><td class="small">${fDT(f.to)}</td><td>${now >= f.to ? tg('已结束') : now >= f.from ? tg('进行中', 'bad') : tg('未开始', 'warn')}</td><td>${canEdit ? `<button class="btn sm danger" data-na="osc-fz-del" data-i="${f.i}">删除</button>` : ''}</td></tr>`).join('')}</tbody></table></div>` : '<div class="small muted">当前没有设置冻结期。</div>'}
      <div class="small muted" style="margin-top:8px">冻结期内非紧急变更会被后端拒绝创建；紧急变更需 2 级审批并事后复盘。</div>
      ${canEdit ? `<div class="hr"></div><b class="small">新增冻结期</b>
        <div class="field" style="margin-top:6px"><label>名称</label><input type="text" id="oscFzL" placeholder="例如 年度大促保障期"></div>
        <div class="grid g2"><div class="field"><label>开始</label><input type="datetime-local" id="oscFzF"></div><div class="field"><label>结束</label><input type="datetime-local" id="oscFzT"></div></div>
        <button class="btn primary sm" data-na="osc-fz-add">添加冻结期</button>`
      : '<div class="small muted" style="margin-top:8px">增删冻结期需要「修改平台设置」（settings.edit）权限。</div>'}`;
  }

  async function oscSaveFreezes(mut, okMsg) {
    const s = await oppLoadSettings(true), list = ((s && s.freezes) || []).slice();
    const next = mut(list); if (!next) return;
    const r = await opApi('POST', '/api/ops/settings', {freezes: next});
    if (r.code === 200) { await oppLoadSettings(true); toast(okMsg); oscBody(); } else toast(r.data.error || '保存失败');
  }

  function oscOpen(id) { osc.openId = id; openDrawer('', '<div id="oscDetail"></div>'); oscDetail(); }
  window.oscOpen = oscOpen;
  const STEP_ST = {wait: ['', '等待'], run: ['warn', '执行中'], done: ['ok', '完成'], skipped: ['', '跳过'], failed: ['bad', '失败'], manual: ['warn', '待人工确认']};

  function oscDetail() {
    const el = $('#oscDetail'); if (!el) return;
    const c = OP.changes.find(x => x.id === osc.openId);
    if (!c) { el.innerHTML = '<div class="empty">找不到该变更（可能已被清理）</div>'; return; }
    $('#drTitle').textContent = `${c.id} · ${kindLabel(c.kind)}`;
    const ap = c.approvals || [], need = c.need || 0, st = c.state;
    const steps = ['提交申请', ...Array.from({length: need}, (_, i) => `${i + 1} 级审批`), c.scheduledAt ? '到点执行' : '执行', '完成'];
    const stIdx = st === 'pending' ? 1 + ap.length : ['approved', 'scheduled', 'running', 'failed', 'aborted'].includes(st) ? need + 1 : st === 'succeeded' ? need + 3 : 1 + ap.length;
    const flowBad = ST_FLOW_BAD.includes(st);
    const aC = approveCheck(c), rC = rejectCheck(c), cC = cancelCheck(c);
    const fz = freezesOf(), win = c.scheduledAt ? winOf(c) : null, hitFz = win ? fz.find(x => win[0] < x.to && win[1] > x.from) : null;
    const confl = c.scheduledAt && LIVE_ST.includes(st) ? conflictsOf(OP.changes.filter(x => x.scheduledAt && LIVE_ST.includes(x.state))).filter(x => x.a.id === c.id || x.b.id === c.id) : [];
    const para = (() => { try { return JSON.stringify(c.params); } catch (e) { return ''; } })();
    el.innerHTML = `<div class="row" style="margin-bottom:10px">${opStateTag(st)}${opRiskTag(c.risk)}${c.emergency ? tg('紧急变更', 'bad') : ''}${c.needMfa ? tg('需 MFA', 'info') : ''}${tg(kindLabel(c.kind))}</div>
      <h3 style="font-size:15px;margin:0 0 8px">${E(c.title)}</h3>
      <div class="kv"><span>申请人</span><span>${E(c.applicant)}</span><span>创建时间</span><span>${fDT(c.created)}</span>
        <span>计划执行</span><span>${c.scheduledAt ? `${fDT(c.scheduledAt)}（预计 ${(c.plan && c.plan.estMins) || 30} 分钟）` : '审批通过后立即执行'}</span>
        <span>审批</span><span>${need ? `${ap.length}/${need} 级` : '免审批'}${ap.length ? '：' + ap.map(a => `${E(a.user)}（${fDT(a.time)}${a.comment ? '，' + E(a.comment) : ''}）`).join('；') : ''}</span>
        ${st === 'pending' ? `<span>审批有效期</span><span>${fDT(c.expires)}</span>` : ''}
        ${c.ticket || c.note ? `<span>工单 / 说明</span><span>${E([c.ticket, c.note].filter(Boolean).join(' · '))}</span>` : ''}
        ${c.reason ? `<span>原因</span><span>${E(c.reason)}</span>` : ''}${c.started ? `<span>开始</span><span>${fDT(c.started)}</span>` : ''}${c.finished ? `<span>结束</span><span>${fDT(c.finished)}</span>` : ''}
        <span>参数</span><span class="mono small" style="word-break:break-all">${E(para)}</span></div>
      <h3 style="font-size:14px;margin:14px 0 6px">审批与执行流程</h3>
      <div class="flow">${steps.map((l, i) => `<span class="st ${i < stIdx ? 'done' : i === stIdx ? (flowBad ? 'bad' : 'cur') : ''}">${E(l)}</span>${i < steps.length - 1 ? '<span class="muted">›</span>' : ''}`).join('')}</div>
      ${confl.map(x => `<div class="notice bad"><span>✖</span><div>与 ${E(x.a.id === c.id ? x.b.id : x.a.id)} 的计划窗口重叠，且涉及同一资源（${E(x.shared.slice(0, 4).join('、'))}）</div></div>`).join('')}
      ${hitFz ? `<div class="notice warn"><span>⚠</span><div>计划窗口落在冻结期「${E(hitFz.label)}」内${c.emergency ? '（紧急变更）' : ''}</div></div>` : ''}
      ${(c.plan && c.plan.checks || []).length ? `<h3 style="font-size:14px;margin:14px 0 6px">前置检查（创建时）</h3>${c.plan.checks.map(x => `<div class="notice ${x.lv === 'info' ? 'info' : x.lv}"><span>${opLvIcon[x.lv] || 'ℹ'}</span><div>${E(x.t)}</div></div>`).join('')}` : ''}
      ${(c.plan && c.plan.display || []).length ? `<h3 style="font-size:14px;margin:14px 0 6px">将执行的命令</h3><pre class="code mono">${E(c.plan.display.join('\n'))}</pre>` : ''}
      ${(c.steps || []).length ? `<h3 style="font-size:14px;margin:14px 0 6px">执行步骤</h3><ol class="small">${c.steps.map(s => `<li>${E(s.name)} ${tg((STEP_ST[s.state] || ['', s.state])[1], (STEP_ST[s.state] || [''])[0])}${s.detail ? ` <span class="muted">${E(s.detail)}</span>` : ''}</li>`).join('')}</ol>` : ''}
      ${(c.log || []).length ? `<h3 style="font-size:14px;margin:14px 0 6px">日志（最近 ${Math.min(15, c.log.length)} 行）</h3><pre class="code mono" style="max-height:200px;overflow:auto">${E(c.log.slice(-15).join('\n'))}</pre>` : ''}
      ${st === 'pending' ? `<h3 style="font-size:14px;margin:14px 0 6px">审批</h3>
        ${aC.ok || rC.ok ? `<div class="field"><label>审批意见 / 驳回原因（驳回必填）</label><input type="text" id="oscCmt" maxlength="200"></div>` : `<div class="small muted">${E(aC.why || rC.why || '')}</div>`}` : ''}
      <div class="sticky-actions">
        ${st === 'pending' && aC.ok ? `<button class="btn primary" data-na="osc-approve" data-id="${E(c.id)}" data-from="drawer">通过</button>` : ''}
        ${st === 'pending' && rC.ok ? `<button class="btn danger" data-na="osc-reject" data-id="${E(c.id)}">驳回</button>` : ''}
        ${cC ? `<button class="btn danger" data-na="osc-cancel" data-id="${E(c.id)}">取消变更</button>` : ''}
        ${typeof opGoTasks === 'function' && st !== 'pending' ? `<button class="btn" data-na="osc-task" data-id="${E(c.id)}">在任务中心查看</button>` : ''}
        <button class="btn" data-close="1">关闭</button></div>`;
  }

  async function oscAct(id, act, body, okMsg) {
    const r = await opApi('POST', `/api/changes/${encodeURIComponent(id)}/${act}`, body || {});
    if (r.code === 200) { toast(okMsg); await opRefresh(true); if ($('#oscDetail')) oscDetail(); if (hashIs('schedule')) oscBody(); }
    else toast(r.data.error || '操作失败');
  }

  /* 新建排期：引导到节点管理等入口（排期是在提交变更时选「定时执行」） */
  function oscNewGuide() {
    openDrawer('新建排期', `<div class="notice info"><span>ℹ</span><div>排期不是单独的表单：在对应操作的「提交变更」面板里，把<b>执行时间</b>选为「定时执行…」并指定时间，审批通过后到点自动执行。面板里的<b>推荐时间</b>会按窗口规则给出评分最高的时段，点一下即可回填；创建时后端会检查冻结期、窗口规则与同资源时间冲突。</div></div>
      <div class="grid" style="gap:10px;margin-top:10px">
        <div class="card"><b>实例 / 扩缩容 / 配置变更</b><div class="small muted" style="margin:4px 0 8px">在节点管理里选择实例或主机，发起重启、扩容、缩容、配置变更。</div><div class="row"><button class="btn primary sm" data-go="nodes" data-tab="inst">节点实例</button><button class="btn sm" data-go="nodes" data-tab="scaleout">扩容</button><button class="btn sm" data-go="nodes" data-tab="scalein">缩容</button></div></div>
        <div class="card"><b>主机维护</b><div class="small muted" style="margin:4px 0 8px">进入主机维护流程（排空 → 停机 → 恢复）。</div><button class="btn sm" data-go="nodes" data-tab="maint">主机维护</button></div>
        <div class="card"><b>滚动重启 / 滚动升级 / 备份 / 恢复</b><div class="small muted" style="margin:4px 0 8px">在日常运维里发起。</div><button class="btn sm" data-go="ops">日常运维</button></div></div>
      <div class="sticky-actions"><button class="btn" data-close="1">关闭</button></div>`);
  }

  pages.schedule = {
    ..._sched,
    render() { return OP.on ? this.opRender() : _sched.render.call(this); },
    opRender() { neutralizeSim(); return '<div id="oscBody"><div class="empty">正在读取变更…</div></div>'; },
    init() {
      if (!OP.on) return _sched.init.call(this);
      const na = window.navArgs; window.navArgs = null;
      oscBody();
      oppLoadSettings(true).then(() => { if (hashIs('schedule') && !drawerIsOpen()) oscBody(); });
      if (na && na.node && OP.changes.some(c => c.id === na.node)) oscOpen(na.node);
    },
    body() { return OP.on ? oscBody() : _sched.body.call(this); },
  };

  /* ======================================================================
     2. 环境与设置（覆盖值持久化到后端）
     ====================================================================== */
  const _st = pages.settings;
  let ostSig = '';
  const ostCanEdit = () => opCan('settings.edit');

  function ostApply(ovr) {
    stState.ovr = {};
    ST_ITEMS.forEach(it => { if (ovr && ovr[it.k] !== undefined) stState.ovr[it.k] = ovr[it.k]; });
    ST_SETS.forEach(s => { if (ovr && ovr[s.k] !== undefined) stState.set[s.k] = ovr[s.k]; else delete stState.set[s.k]; });
  }
  function ostCollect() {
    const m = {};
    Object.entries(stState.ovr).forEach(([k, v]) => { m[k] = String(v); });
    ST_SETS.forEach(s => { const v = stState.set[s.k]; if (v !== undefined && String(v) !== String(s.def)) m[s.k] = String(v); });
    return m;
  }
  async function ostPersist() {
    const r = await opApi('POST', '/api/ops/settings', {overrides: ostCollect()});
    if (r.code === 200) { OPP.settings = null; await oppLoadSettings(true); ostSig = JSON.stringify([OPP.settings.overrides, OPP.settings.lb]); toast('设置已保存到后端'); }
    else { toast(r.data.error || '保存失败'); await ostLoad(true); }
  }
  async function ostLoad(force) {
    const s = await oppLoadSettings(force); if (!s) return;
    const sig = JSON.stringify([s.overrides, s.lb]);
    if (sig === ostSig && !force) return;
    ostSig = sig; ostApply(s.overrides || {});
    if (hashIs('settings') && !stState.edit && !stState.running && !typingNow()) $('#view').innerHTML = pages.settings.render();
  }

  pages.settings = {
    ..._st,
    render() {
      if (!OP.on) return _st.render.call(this);
      // 写操作开关与冻结期由后端真实管理，这里不再显示原型里的占位设置项
      const hide = new Set(['wr.test', 'wr.stage', 'wr.prod', 'win.freeze']), saved = ST_SETS.slice();
      ST_SETS.length = 0; saved.filter(s => !hide.has(s.k)).forEach(s => ST_SETS.push(s));
      let h; try { h = _st.render.call(this); } finally { ST_SETS.length = 0; saved.forEach(s => ST_SETS.push(s)); }
      const can = ostCanEdit(), s = OPP.settings || {}, w = OP.write || {}, fzN = ((s.freezes) || []).length, ovrN = Object.keys(ostCollect()).length;
      if (!can) h = h.replace(/data-na="st-(edit|save|auto)"/g, OP_DENY_ATTR + ' data-na="st-$1"').replace(/data-nc="st-set"/g, 'disabled data-nc="st-set"').replace(/data-ni="st-set"/g, 'disabled data-ni="st-set"');
      const card = `<div class="card"><h2>平台设置 <span class="sub">${window.CL && CL.active() ? "集群 " + E(CL.name()) + " · " : ""}保存在后端 · 修改写入审计</span></h2>
        ${can ? '' : '<div class="notice warn"><span>⚠</span><div>你没有「修改平台设置」（settings.edit）权限，以下设置为只读。</div></div>'}
        <label class="small" style="display:flex;gap:6px;align-items:flex-start"><input type="checkbox" data-nc="ost-lb" ${s.lb ? 'checked' : ''} ${can ? '' : 'disabled'}><span>集群前有负载均衡<br><span class="muted">开启后，缩容 / 维护 TiDB 节点时要求人工先从负载均衡摘除（变更计划里会出现「需人工确认」步骤）。</span></span></label>
        <div class="hr"></div>
        <div class="kv"><span>写操作开关</span><span>${w.enabled && !w.reason ? tg('已开启', 'ok') : tg('只读', 'warn')} <a data-go="security" data-tab="wswitch">查看 / 管理 →</a></span>
          <span>变更冻结期</span><span>${fzN} 个 <a data-go="schedule">在变更排期管理 →</a></span><span>用户自定义覆盖</span><span>${ovrN} 项</span></div></div>`;
      const mark = '<div class="card"><h2>版本兼容性';
      return h.includes(mark) ? h.replace(mark, card + mark) : h + card;
    },
    init() { _st.init.call(this); if (OP.on) { ostSig = ''; ostLoad(true); } },
  };

  /* ======================================================================
     3. 日常运维
     ====================================================================== */
  const _ops = pages.ops;
  const oop = {run: {}, out: {}, role: 'tikv', ver: '', insOs: false, inspecting: false, report: null, reportTried: false, params: null, paramsErr: '', paramsAt: 0,
    bk: {mode: 'full', storage: '', task: '', rstorage: ''}};
  const opsTab = () => (typeof opsState !== 'undefined' ? opsState.tab : 'cluster');
  const repaintOps = tab => { if (hashIs('ops') && $('#opBody') && opsTab() === tab) pages.ops[tab](); };
  const reqCap = (cap, label) => opCan(cap) ? '' : `<div class="notice warn"><span>⚠</span><div>你的角色没有「${label}」（${cap}）权限，操作按钮已置灰。</div></div>`;
  const outBox = o => !o ? '' : `${o.error ? `<div class="notice bad"><span>✖</span><div>${E(o.error)}</div></div>` : ''}<pre class="code mono opp-pre">${E(o.output || '（无输出）')}</pre><div class="small muted">执行于 ${fDT(o.at)}</div>`;

  function oopClusterPanel() {
    const sel = $('#opSel'); if (!sel) return;
    sel.onchange = e => { opsState.op = e.target.value; pages.ops.cluster(); };
    const op = opsState.op, o = OPS[op], d = $('#opFields'), out = $('#opOut'); if (!d || !out) return;
    d.innerHTML = ''; let h = '';
    if (op === 'display' || op === 'check') {
      const can = opCan('diag.run');
      h = `<div class="notice info"><span>ℹ</span><div>只读操作，由后端执行器运行白名单命令：<code>tiup cluster ${op === 'display' ? 'display' : 'check'} ${E(CLUSTER)}${op === 'check' ? ' --cluster' : ''}</code>。${op === 'check' ? '主机检查较慢，请耐心等待。' : ''}</div></div>
        ${reqCap('diag.run', '运行诊断 / 巡检')}
        <div class="row" style="margin:8px 0"><button class="btn primary" data-na="oop-read" data-c="${op}" ${!can ? OP_DENY_ATTR : oop.run[op] ? 'disabled' : ''}>${oop.run[op] ? '执行中…' : '执行 ' + op}</button></div>${outBox(oop.out[op])}`;
    } else if (op === 'restart') {
      const roles = o.fields[0].opts, can = opCan('inst.op');
      h = `<div class="field"><label>角色</label><select data-nc="oop-role">${roles.map(r => `<option ${oop.role === r ? 'selected' : ''}>${r}</option>`).join('')}</select></div>
        <div class="notice warn"><span>⚠</span><div>按角色滚动重启：服务端会规划步骤并做前置检查，提交后按审批流程执行。</div></div>${reqCap('inst.op', '单实例操作 / 滚动重启')}
        <div class="row" style="margin-top:8px"><button class="btn primary" data-na="oop-restart" ${can ? '' : OP_DENY_ATTR}>预览并提交变更</button></div>`;
    } else if (op === 'upgrade') {
      const can = opCan('scale');
      h = `<div class="field"><label>目标版本（格式 v8.5.3）</label><input type="text" data-ni="oop-ver" value="${E(oop.ver)}" placeholder="v8.5.3" maxlength="20"></div>
        <div class="notice warn"><span>⚠</span><div>滚动升级为高风险变更：需 2 级审批；升级前必须完成一次全量备份并阅读目标版本 Release Notes。</div></div>${reqCap('scale', '扩缩容 / 升级')}
        <div class="row" style="margin-top:8px"><button class="btn primary" data-na="oop-upgrade" ${can ? '' : OP_DENY_ATTR}>预览并提交变更</button></div>`;
    } else if (op === 'reload') {
      h = `<div class="notice info"><span>ℹ</span><div>配置变更（edit-config + reload）已统一到「节点管理 → 配置变更」：那里会校验参数、规划滚动步骤并走审批。<br><a data-go="nodes" data-tab="inst">前往节点管理 →</a></div></div>`;
    } else {
      h = `<div class="notice bad"><span>⛔</span><div><b>平台禁止</b>：<code>tiup cluster ${op}</code> 不在执行器白名单内，平台不提供提交入口。${op === 'clean' ? '该操作会清除数据目录。' : '热修复请在维护窗口由 DBA 在中控机上手工完成并留痕。'}</div></div>`;
    }
    out.innerHTML = h;
    $('#opFields').querySelectorAll('[data-k]').forEach(x => { x.oninput = null; });
  }

  async function oopRunRead(cmd) {
    oop.run[cmd] = true; repaintOps('cluster');
    const r = await opApi('POST', '/api/ops/read-cmd', {cmd});
    oop.run[cmd] = false;
    oop.out[cmd] = r.code === 200 ? {output: r.data.output || '', error: r.data.error || '', at: Date.now()} : {output: '', error: r.data.error || ('请求失败（' + r.code + '）'), at: Date.now()};
    repaintOps('cluster'); if (cmd === 'br-status') repaintOps('backup');
  }

  /* 巡检 */
  function oopReportHtml(rep) {
    const groups = []; (rep.items || []).forEach(it => { let g = groups.find(x => x.g === it.g); if (!g) { g = {g: it.g, items: []}; groups.push(g); } g.items.push(it); });
    const T = {ok: ['ok', '通过'], warn: ['warn', '警告'], bad: ['bad', '异常'], na: ['', '未采集']};
    return `<div class="row" style="margin:6px 0 10px">${tg('通过 ' + rep.ok, 'ok')}${tg('警告 ' + rep.warn, 'warn')}${tg('异常 ' + rep.bad, 'bad')}${tg('未采集 ' + rep.na)}<span class="small muted">${E(rep.cluster)}（${E(rep.version)}）· 数据来源 ${E(rep.mode)} · ${fDT(rep.time)}</span></div>
      ${groups.map(g => `<h3 style="font-size:14px;margin:12px 0 4px">${E(g.g)}</h3>${g.items.map(c => { const t = T[c.r] || ['', c.r]; return `<div class="row" style="padding:6px 0;border-bottom:1px solid var(--border);${c.r === 'na' ? 'opacity:.6' : ''}"><span class="tag ${t[0]}" style="width:52px;text-align:center">${t[1]}</span><b>${E(c.n)}</b><span class="muted small">${E(c.d)}</span></div>`; }).join('')}`).join('')}`;
  }
  async function oopInspect() {
    oop.inspecting = true; repaintOps('inspect');
    const r = await opApi('POST', '/api/inspect', {os: !!oop.insOs});
    oop.inspecting = false;
    if (r.code === 200) { oop.report = r.data; toast('巡检完成'); } else toast(r.data.error || '巡检失败');
    repaintOps('inspect');
  }

  /* 参数 */
  async function oopLoadParams() {
    const r = await opApi('GET', '/api/params');
    if (r.code === 200) { oop.params = r.data.params || []; oop.paramsErr = ''; oop.paramsAt = Date.now(); } else oop.paramsErr = r.data.error || '读取失败';
    repaintOps('param');
  }

  const bkChanges = () => OP.changes.filter(c => c.kind === 'backup.run' || c.kind === 'backup.restore').sort((a, b) => new Date(b.created) - new Date(a.created));
  const MODE_CN = {full: '全量备份', 'log-start': '启动日志备份', 'log-stop': '停止日志备份'};

  pages.ops = {
    ..._ops,
    cluster() { _ops.cluster.call(this); if (OP.on) oopClusterPanel(); },
    inspect() {
      if (!OP.on) return _ops.inspect.call(this);
      const can = opCan('diag.run'), exp = opCan('report.export'), rep = oop.report;
      $('#opBody').innerHTML = `<div class="card"><h2>集群巡检 <span class="sub">${rep ? (rep.items || []).length + ' 项' : '服务端采集'}</span>
        <span class="right"><label class="small"><input type="checkbox" data-nc="oop-insos" ${oop.insOs ? 'checked' : ''} ${can ? '' : 'disabled'}> 同时检查主机（tiup cluster check，较慢）</label>
        <button class="btn primary" data-na="oop-inspect" ${!can ? OP_DENY_ATTR : oop.inspecting ? 'disabled' : ''}>${oop.inspecting ? '巡检中…' : '执行巡检'}</button>
        ${exp && rep ? `<a class="btn" href="${CL.url('/api/inspect/report?format=md')}" download="inspection.md">导出 Markdown</a><a class="btn" href="${CL.url('/api/inspect/report?format=html')}" download="inspection.html">导出 HTML</a>` : ''}</span></h2>
        ${reqCap('diag.run', '运行诊断 / 巡检')}
        <div id="inOut">${oop.inspecting ? '<div class="empty">正在巡检' + (oop.insOs ? '（含主机检查，可能需要几分钟）' : '') + '…</div>' : rep ? oopReportHtml(rep) : '<div class="empty">点击「执行巡检」生成报告</div>'}</div></div>`;
      if (!rep && !oop.reportTried && can) {
        oop.reportTried = true;
        opApi('GET', '/api/inspect/last').then(r => { if (r.code === 200 && r.data.report) { oop.report = r.data.report; repaintOps('inspect'); } });
      }
    },
    backup() {
      if (!OP.on) return _ops.backup.call(this);
      const bk = oop.bk, rec = bkChanges(), canB = opCan('backup'), canD = opCan('diag.run');
      $('#opBody').innerHTML = `<div class="grid g2">
        <div class="card"><h2>PITR 日志备份状态 ${docLink('br-pitr-guide')}</h2>
          <div class="small muted">运行只读命令 <code>tiup br log status</code>，查看日志备份任务与 checkpoint。</div>${reqCap('diag.run', '运行诊断 / 巡检')}
          <div class="row" style="margin:8px 0"><button class="btn primary" data-na="oop-read" data-c="br-status" ${!canD ? OP_DENY_ATTR : oop.run['br-status'] ? 'disabled' : ''}>${oop.run['br-status'] ? '查询中…' : '查询 PITR 状态'}</button></div>${outBox(oop.out['br-status'])}</div>
        <div class="card"><h2>发起备份</h2>${reqCap('backup', '备份恢复操作')}
          <div class="field"><label>模式</label><select data-nc="oop-bk" data-k="mode" ${canB ? '' : 'disabled'}>${Object.entries(MODE_CN).map(([k, v]) => `<option value="${k}" ${bk.mode === k ? 'selected' : ''}>${v}</option>`).join('')}</select></div>
          ${bk.mode !== 'log-stop' ? `<div class="field"><label>存储路径</label><input type="text" data-ni="oop-bk" data-k="storage" value="${E(bk.storage)}" placeholder="例如 local:///backup/snap-20261005 或 s3://bucket/prefix" ${canB ? '' : 'disabled'}></div>` : ''}
          ${bk.mode !== 'full' ? `<div class="field"><label>日志备份任务名</label><input type="text" data-ni="oop-bk" data-k="task" value="${E(bk.task)}" placeholder="例如 pitr-prod" ${canB ? '' : 'disabled'}></div>` : ''}
          <div class="notice info"><span>ℹ</span><div>备份存储路径必须在执行器策略的白名单前缀内，否则会被执行器拒绝。</div></div>
          <button class="btn primary" data-na="oop-backup" ${canB ? '' : OP_DENY_ATTR}>预览并提交变更</button></div></div>
        <div class="grid g2" style="margin-top:16px">
        <div class="card"><h2>从备份恢复</h2>
          <div class="notice bad"><span>✖</span><div>恢复为高风险操作：要求目标集群为空（或目标库表不存在），期间性能显著下降；建议先在演练集群验证。</div></div>
          <div class="field"><label>备份存储路径</label><input type="text" data-ni="oop-bk" data-k="rstorage" value="${E(bk.rstorage)}" placeholder="同样需在白名单前缀内" ${canB ? '' : 'disabled'}></div>
          <button class="btn danger" data-na="oop-restore" ${canB ? '' : OP_DENY_ATTR}>预览并提交恢复变更</button></div>
        <div class="card"><h2>注意事项</h2><ul class="small" style="padding-left:18px;margin:0"><li>备份期间 tidb_gc_life_time 需大于备份耗时</li><li>停止日志备份后将无法恢复到停止之后的任意时间点</li><li>备份文件应异地存放并定期校验、演练恢复</li><li>每日 02:00 有计划备份窗口，避免与之重叠</li></ul></div></div>
        <div class="card" style="margin-top:16px"><h2>平台发起的备份记录 <span class="sub">${rec.length}</span></h2>${rec.length ? `<div class="tbl-wrap"><table><thead><tr><th>编号</th><th>类型</th><th>内容</th><th>时间</th><th>申请人</th><th>状态</th></tr></thead><tbody>
          ${rec.map(c => { const p = c.params || {}; return `<tr class="click" data-na="osc-open" data-id="${E(c.id)}"><td class="mono small">${E(c.id)}</td><td>${tg(c.kind === 'backup.restore' ? '恢复' : (MODE_CN[p.mode] || '备份'), c.kind === 'backup.restore' ? 'bad' : '')}</td><td class="small">${E(c.title)}${p.storage ? `<div class="mono muted">${E(p.storage)}</div>` : ''}</td><td class="small">${fDT(c.created)}</td><td class="small">${E(c.applicant)}</td><td>${opStateTag(c.state)}</td></tr>`; }).join('')}</tbody></table></div>` : '<div class="empty">还没有通过平台发起的备份 / 恢复</div>'}</div>`;
    },
    param() {
      if (!OP.on) return _ops.param.call(this);
      const ps = oop.params;
      if (!ps) { $('#opBody').innerHTML = `<div class="card"><h2>关键参数建议</h2><div class="empty">${oop.paramsErr ? E(oop.paramsErr) : '正在读取…'}</div></div>`; if (!oop.paramsErr && !oop.paramsLoading) { oop.paramsLoading = true; oopLoadParams().finally(() => { oop.paramsLoading = false; }); } return; }
      const warn = ps.filter(p => p.state === 'warn').length, drift = ps.filter(p => p.drift).length;
      $('#opBody').innerHTML = `<div class="card"><h2>关键参数建议 <span class="sub">当前值取自集群，推荐值来自官方调优建议</span><span class="right">${tg('需调整 ' + warn, warn ? 'warn' : '')}${tg('配置漂移 ' + drift, drift ? 'warn' : '')}<button class="btn sm" data-na="oop-params">刷新</button></span></h2>
        <div class="notice info"><span>ℹ</span><div>平台不在此处直接修改参数；修改命令仅展示，请通过 <a data-go="nodes" data-tab="inst">节点管理 → 配置变更</a> 提交（会校验、规划并走审批）。</div></div>
        <div class="tbl-wrap"><table><thead><tr><th>范围</th><th>参数</th><th>当前值</th><th>推荐</th><th>原因</th><th>状态</th><th></th></tr></thead><tbody>
        ${ps.map(p => `<tr class="${p.state === 'warn' ? '' : ''}"><td><span class="tag">${E(p.scope)}</span></td><td class="mono small">${E(p.name)}</td><td class="mono small" style="word-break:break-all">${E(p.cur)}${p.drift ? ' ' + tg('配置漂移', 'warn') : ''}</td><td class="small">${E(p.rec)}</td><td class="small muted">${E(p.why)}</td>
          <td>${p.state === 'warn' ? tg('需调整', 'warn') : p.state === 'ok' ? tg('符合', 'ok') : tg('未采集')}</td><td>${p.set ? `<button class="btn sm" data-copy="${E(p.set)}" title="${E(p.set)}">复制命令</button>` : ''}</td></tr>`).join('')}</tbody></table></div>
        <div class="small muted" style="margin-top:8px">TiDB 系统变量可在线 SET GLOBAL；TiKV / PD 配置可用 tiup edit-config + reload，部分支持 SET CONFIG 在线调整。「配置漂移」表示各实例取值不一致。${oop.paramsAt ? ' 读取于 ' + fDT(oop.paramsAt) + '。' : ''}</div></div>`;
    },
    audit() {
      if (!OP.on) return _ops.audit.call(this);
      const list = OP.changes.slice().sort((a, b) => new Date(b.created) - new Date(a.created)).slice(0, 30);
      $('#opBody').innerHTML = `<div class="card"><h2>变更审计 <span class="sub">平台变更最近 ${list.length} 条</span><span class="right"><a data-go="security" data-tab="audit">查看完整审计日志（哈希链）→</a></span></h2>
        ${list.length ? `<div class="tbl-wrap"><table><thead><tr><th>时间</th><th>编号</th><th>申请人</th><th>类型</th><th>内容</th><th>风险</th><th>审批</th><th>状态</th></tr></thead><tbody>
        ${list.map(c => `<tr class="click" data-na="osc-open" data-id="${E(c.id)}"><td class="small">${fDT(c.created)}</td><td class="mono small">${E(c.id)}</td><td class="small">${E(c.applicant)}</td><td>${tg(kindLabel(c.kind))}</td><td class="small" style="max-width:300px">${E(c.title)}</td><td>${opRiskTag(c.risk)}</td><td class="small">${c.need ? `${(c.approvals || []).length}/${c.need}` : '免审批'}</td><td>${opStateTag(c.state)}</td></tr>`).join('')}</tbody></table></div>`
        : '<div class="empty">还没有变更记录</div>'}
        <div class="small muted" style="margin-top:8px">每条变更的完整过程（登录、审批、命令执行、拒绝）都写入带哈希链的审计日志，可在「权限与审计 → 审计日志」按用户 / 动作 / 结果检索并校验完整性。</div></div>`;
    },
  };

  /* ======================================================================
     4. 总览：事件时间线 + 告警静默
     ====================================================================== */
  const _ov = pages.overview;
  const TL_ICON = {alert: '🔔', change: '⚙', maint: '🛠', diag: '✚', audit: '⛨'};
  const TL_LV = {bad: 'bad', warn: 'warn', ok: 'ok', info: 'info'};

  function oovTimelineHtml() {
    const ev = OPP.timeline;
    if (!ev) return '<div class="empty">正在读取…</div>';
    if (!ev.length) return '<div class="empty">近期没有事件</div>';
    return ev.slice(0, 40).map(x => {
      const attr = x.kind === 'change' && x.ref ? `data-na="osc-open" data-id="${E(x.ref)}" style="cursor:pointer"` : x.kind === 'diag' && x.ref ? `data-go="diag" data-open="${E(x.ref)}" style="cursor:pointer"` : x.kind === 'maint' ? 'data-go="nodes" data-tab="maint" style="cursor:pointer"' : '';
      return `<div class="row" style="padding:6px 0;border-bottom:1px solid var(--border);align-items:flex-start;flex-wrap:nowrap" ${attr}><span class="dot ${TL_LV[x.level] || 'info'}" style="margin-top:6px;flex:none"></span><div style="flex:1;min-width:0"><div class="small"><b>${TL_ICON[x.kind] || '•'} ${E(x.title)}</b></div><div class="small muted" style="word-break:break-all">${opFmtTime(x.time)}${x.detail ? ' · ' + E(x.detail) : ''}</div></div></div>`;
    }).join('');
  }
  async function oovLoadTimeline(force) {
    if (!force && OPP.timeline && Date.now() - OPP.timelineAt < 15000) return;
    if (window.CL && CL.empty()) return;   // 独立部署且尚未接入集群：没有集群数据
    const r = await opApi('GET', '/api/timeline');
    if (r.code === 200) { OPP.timeline = r.data.events || []; OPP.timelineAt = Date.now(); }
    const b = $('#oovTlBody'); if (b && hashIs('overview')) b.innerHTML = oovTimelineHtml();
  }
  function oovInit() {
    const ac = $$('#view .card').find(c => { const h = c.querySelector('h2'); return h && h.textContent.includes('活跃告警'); });
    if (!ac) return;
    if (opCan('diag.run')) {
      [...ac.children].filter(x => x.tagName === 'DIV' && x.style.padding).forEach((r, i) => {
        const a = ALERTS[i], row = r.querySelector('.row'); if (!a || !row) return;
        row.insertAdjacentHTML('beforeend', `<button class="btn sm" style="margin-left:auto" data-na="oov-silence" data-i="${i}" title="静默该告警（经执行器写入 Alertmanager，受写操作开关约束）">静默</button>`);
      });
    }
    ac.insertAdjacentHTML('afterend', `<div class="card" id="oovTl"><h2>事件时间线 <span class="sub">告警 · 变更 · 维护 · 诊断</span></h2><div id="oovTlBody" style="max-height:380px;overflow:auto">${oovTimelineHtml()}</div></div>`);
    oovLoadTimeline(false);
  }
  function oovSilence(i) {
    const a = ALERTS[i]; if (!a) return;
    openDrawer('静默告警', `<div class="kv"><span>告警</span><span>${E(a.name)}</span><span>实例</span><span class="mono">${E(a.inst)}</span><span>描述</span><span>${E(a.desc)}</span></div>
      <div class="notice info" style="margin-top:10px"><span>ℹ</span><div>静默会在 Alertmanager 创建一条临时静默规则，到期自动失效；属于低风险写操作，不走审批，但受「写操作开关」约束并写入审计。</div></div>
      <div class="field"><label>静默时长（分钟，1–1440）</label><input type="number" id="oovMin" min="1" max="1440" value="60"></div>
      <div class="field"><label>备注</label><input type="text" id="oovCmt" maxlength="200" placeholder="例如：磁盘扩容中，已知问题"></div>
      <div id="oovErr" class="small" style="color:var(--bad);min-height:18px"></div>
      <div class="sticky-actions"><button class="btn primary" data-na="oov-silence-go" data-i="${i}">确认静默</button><button class="btn" data-close="1">取消</button></div>`);
  }
  async function oovSilenceGo(i, btn) {
    const a = ALERTS[i]; if (!a) return;
    const minutes = parseInt($('#oovMin').value, 10), err = $('#oovErr');
    if (!(minutes >= 1 && minutes <= 1440)) { err.textContent = '时长需在 1–1440 分钟之间'; return; }
    btn.disabled = true;
    const r = await opApi('POST', '/api/alerts/silence', {alertname: a.name, instance: a.inst, minutes, comment: $('#oovCmt').value.trim()});
    btn.disabled = false;
    if (r.code === 200) { closeDrawer(); toast(`已静默 ${a.name} ${minutes} 分钟`); oovLoadTimeline(true); }
    else { const msg = r.data.error || '静默失败'; err.textContent = msg; toast(msg); }
  }

  pages.overview = {
    ..._ov,
    init() { if (_ov.init) _ov.init.call(this); if (OP.on) oovInit(); },
  };

  /* ======================================================================
     事件委托
     ====================================================================== */
  const CLICK = {
    'opp-yes': () => { const f = OPP.confirmFn; OPP.confirmFn = null; closeDrawer(); if (f) f(); },
    'osc-open': d => oscOpen(d.id),
    'osc-week': d => { osc.week = d.d === '0' ? weekStartOf(opNow()) : osc.week + (+d.d) * 7 * DAY; const t = new Date(osc.week); osc.week = weekStartOf(t.getTime() + 12 * 3600e3); oscBody(); },
    'osc-new': () => oscNewGuide(),
    'osc-view': d => { osc.view = d.v === 'cal' ? 'cal' : 'table'; oscSaveView(osc.view); oscBody(); },
    'osc-st': d => { osc.stF = d.v; oscBody(); },
    'osc-rk': d => { osc.rkF = d.v; oscBody(); },
    'osc-task': d => { closeDrawer(); if (typeof opGoTasks === 'function') opGoTasks(d.id); },
    'osc-approve': (d, el) => {
      // 只有详情抽屉里的「通过」才真正提交（那里列出了变更全貌，并有审批意见框）；列表里的「审批」，以及页面别处拼出来的同名按钮，只打开详情，由审批人核对后再点
      if (!(el.closest('#oscDetail') && osc.openId === d.id)) { oscOpen(d.id); return; }
      const i = $('#oscCmt'); oscAct(d.id, 'approve', {comment: i ? i.value.trim() : ''}, `已通过 ${d.id}`);
    },
    'osc-reject': d => { const i = $('#oscCmt'), r = i ? i.value.trim() : ''; if (!r) { toast('驳回必须填写原因'); if (i) i.focus(); return; } oscAct(d.id, 'reject', {reason: r}, `已驳回 ${d.id}`); },
    'osc-cancel': d => oppConfirm('取消变更', `确认取消变更 <b>${E(d.id)}</b>？取消后不可恢复，需要时请重新提交。`, '确认取消', () => oscAct(d.id, 'cancel', {reason: '用户取消'}, `已取消 ${d.id}`), true),
    'osc-fz-add': () => {
      const l = $('#oscFzL').value.trim(), f = $('#oscFzF').value, t = $('#oscFzT').value;
      if (!l || !f || !t) { toast('请填写名称、开始与结束时间'); return; }
      const a = new Date(f), b = new Date(t); if (!(a < b)) { toast('结束时间必须晚于开始时间'); return; }
      oscSaveFreezes(list => { list.push({from: a.toISOString(), to: b.toISOString(), label: l}); return list; }, '冻结期已添加');
    },
    'osc-fz-del': d => oscSaveFreezes(list => { const i = +d.i; if (!list[i]) { toast('冻结期已变化，请刷新后重试'); return null; } list.splice(i, 1); return list; }, '冻结期已删除'),
    'oop-read': d => oopRunRead(d.c),
    'oop-restart': () => { const role = oop.role; opIntentDrawer({kind: 'cluster.restart-role', params: {role}}, '滚动重启 ' + role); },
    'oop-upgrade': () => { const v = (oop.ver || '').trim(); if (!/^v\d+\.\d+\.\d+$/.test(v)) { toast('版本格式应为 v8.5.3'); return; } opIntentDrawer({kind: 'cluster.upgrade', params: {version: v}}, '滚动升级到 ' + v); },
    'oop-inspect': () => oopInspect(),
    'oop-params': () => { oop.params = null; oop.paramsErr = ''; pages.ops.param(); },
    'oop-backup': () => {
      const b = oop.bk, params = {mode: b.mode};
      if (b.mode !== 'log-stop') { if (!b.storage.trim()) { toast('请填写存储路径'); return; } params.storage = b.storage.trim(); }
      if (b.mode !== 'full') { if (!b.task.trim()) { toast('请填写日志备份任务名'); return; } params.task = b.task.trim(); }
      opIntentDrawer({kind: 'backup.run', params}, MODE_CN[b.mode]);
    },
    'oop-restore': () => { const s = oop.bk.rstorage.trim(); if (!s) { toast('请填写备份存储路径'); return; } opIntentDrawer({kind: 'backup.restore', params: {storage: s}}, '从备份恢复'); },
    'oov-silence': d => oovSilence(+d.i),
    'oov-silence-go': (d, el) => oovSilenceGo(+d.i, el),
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-nc]'); if (!el) return; const nc = el.dataset.nc;
    if (nc === 'osc-done') { osc.showDone = el.checked; oscBody(); }
    else if (nc === 'osc-group') { osc.group = el.checked; oscBody(); }
    else if (nc === 'osc-hide') { osc.hideEmpty = el.checked; oscBody(); }
    else if (nc === 'oop-role') oop.role = el.value;
    else if (nc === 'oop-insos') oop.insOs = el.checked;
    else if (nc === 'oop-bk') { oop.bk[el.dataset.k] = el.value; pages.ops.backup(); }
    else if (nc === 'ost-lb') {
      const want = el.checked;
      opApi('POST', '/api/ops/settings', {lb: want}).then(async r => { if (r.code === 200) { toast(want ? '已开启：负载均衡摘除为人工步骤' : '已关闭'); } else { toast(r.data.error || '保存失败'); } await oppLoadSettings(true); if (window.lbxRefresh) await window.lbxRefresh(); if (hashIs('settings')) $('#view').innerHTML = pages.settings.render(); });
    }
    else if (nc === 'st-set' && ostCanEdit()) ostPersist();   // 原型处理函数已更新 stState.set，这里写入后端
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-ni="st-set"]'); if (el && ostCanEdit()) ostPersist();
  });
  document.addEventListener('input', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-ni]'); if (!el) return;
    if (el.dataset.ni === 'osc-q') { osc.q = el.value; oscTableOnly(); }
    else if (el.dataset.ni === 'oop-ver') oop.ver = el.value;
    else if (el.dataset.ni === 'oop-bk') oop.bk[el.dataset.k] = el.value;
  });
  // 设置页：覆盖值保存 / 恢复自动后持久化（原型处理函数先执行，更新 stState.ovr，这里再写入后端）
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
    if ((el.dataset.na === 'st-save' || el.dataset.na === 'st-auto') && ostCanEdit()) ostPersist();
  });

  /* ======================================================================
     数据刷新钩子
     ====================================================================== */
  OP_HOOKS.push((h, ctx) => {
    if (!OP.on) return;
    neutralizeSim();
    if (ctx.open || ctx.typing) return;
    if (h === 'schedule') oppLoadSettings(false).then(() => { if (hashIs('schedule') && !drawerIsOpen() && !typingNow()) oscBody(); });
    else if (h === 'overview') oovLoadTimeline(false);
    else if (h === 'settings') ostLoad(false);
    else if (h === 'ops' && ['backup', 'audit'].includes(opsTab()) && !typingNow()) pages.ops[opsTab()]();
  });
})();
