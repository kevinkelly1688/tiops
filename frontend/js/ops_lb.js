// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_lb.js
// @time: 2026/10/28

/* 拓扑图的「负载均衡层」+ 环境与设置页的「负载均衡」卡片。
   数据：GET /api/lb（自动发现的 TiProxy + 手工登记 + 生效判定 + 与拓扑的一致性提示）；写：POST /api/ops/lb、/api/ops/lb/{id}、/api/ops/lb/{id}/delete（settings.edit）。
   - 架构页：pages.arch.lbLayer() 被替换为按真实数据绘制（app.js 里只留默认的「示意」装饰框）：
       运维模式取 /api/lb；只读 / 回放（无运维模式）只画快照里自动发现的 TiProxy；没有 TiProxy 也没有登记时不画框，图下方给出登记提示；
       静态原型（无后端）保持演示框（文字带「（示意）」）。
   - 设置页：运维模式下追加「负载均衡」卡片（自动发现列表、手工登记表、登记 / 编辑抽屉、删除确认、生效判定与一致性提示）；非运维模式 pages.settings 原样不变。
   - 缓存：/api/lb 缓存 30 秒；进入页面（hashchange）与写操作后立即失效；架构页随快照每 30 秒重绘时不会每次都请求。请求失败降级为不画框 + 提示。
   事件：data-na / data-ni / data-nc 前缀 lbx-，文档级委托（CSP 禁止内联事件与内联脚本）。
   依赖 ops.js（OP / opApi / opCan / opEsc）、ops_pages.js（oppConfirm）、app.js（pages / openDrawer / toast / stCls / stText / NODES / LIVE）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); if (isNaN(d) || d.getFullYear() < 2000) return '—'; return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const tg = (t, c = '', title) => `<span class="tag ${c}"${title ? ` title="${E(title)}"` : ''}>${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview').split('/')[0] === h;
  const MAX_ENTRIES = 20, MAX_BACKENDS = 64;
  const TYPES = [['haproxy', 'HAProxy'], ['lvs', 'LVS'], ['nginx', 'Nginx'], ['slb', '云负载均衡(SLB)'], ['f5', 'F5'], ['tiproxy', 'TiProxy（自部署）'], ['other', '其他']];
  const TYPE_FULL = Object.fromEntries(TYPES);
  const TYPE_SHORT = {haproxy: 'HAProxy', lvs: 'LVS', nginx: 'Nginx', slb: 'SLB', f5: 'F5', tiproxy: 'TiProxy', other: '其他'};
  const typeFull = t => TYPE_FULL[t] || t || '—';
  const typeShort = t => TYPE_SHORT[t] || t || '—';
  const stColor = s => `var(--${{ok: 'ok', warn: 'warn', bad: 'bad', info: 'accent'}[stCls(s)] || 'warn'})`;
  const stName = s => (typeof stText !== 'undefined' && stText[s]) || s || '—';

  /* 状态：d 为最近一次成功读取的 /api/lb；err 为失败原因；gen 在失效（写操作 / 换用户）时自增，使过期响应作废 */
  const LB = {d: null, err: '', at: 0, p: null, gen: 0, user: '', sel: '', form: null, sig: ''};
  const can = () => opCan('settings.edit');
  const invalidate = () => { LB.at = 0; LB.gen++; LB.p = null; };
  function ensureUser() {
    const u = OP.me && OP.me.user && OP.me.user.name || '';
    if (LB.user !== u) { LB.user = u; LB.d = null; LB.err = ''; LB.sig = ''; invalidate(); }
  }
  const checkOf = id => ((LB.d && LB.d.check) || []).find(c => c.id === id) || null;
  const mismatch = c => !!(c && ((c.missing || []).length || (c.extra || []).length));

  /* ---------------------------------------------------------------- 数据 ---- */
  function load(force) {
    if (!OP.on || !OP.me) return Promise.resolve();
    ensureUser();
    if (LB.p) return LB.p;
    if (!force && LB.at && Date.now() - LB.at < 30000) return Promise.resolve();
    const gen = LB.gen;
    const p = opApi('GET', '/api/lb').then(r => {
      if (gen !== LB.gen) return;
      if (r.code === 200 && r.data && typeof r.data === 'object') {
        const d = r.data; d.entries = d.entries || []; d.proxies = d.proxies || []; d.check = d.check || []; d.effective = d.effective || {hasLB: false, reason: 'none'};
        const sig = JSON.stringify(d);
        LB.d = d; LB.err = ''; LB.at = Date.now();
        if (sig !== LB.sig) { LB.sig = sig; repaint(); }
      } else {
        LB.d = null; LB.at = Date.now(); LB.sig = '';
        LB.err = r.code === 403 ? '没有权限读取负载均衡信息（需要「查看总览」能力）' : r.code === 404 ? '后端不支持负载均衡接口（版本较旧？）' : (r.data && r.data.error) || (r.code === 0 ? '无法连接后端' : '读取失败（' + r.code + '）');
        repaint();
      }
    }).finally(() => { if (LB.p === p) LB.p = null; });
    LB.p = p;
    return p;
  }
  function repaint() { paintArch(); paintSettings(); }
  function paintArch() { if (hashIs('arch') && $('#archSvg') && pages.arch && pages.arch.draw) pages.arch.draw(); }
  function paintSettings() {
    const el = $('#lbxBox'); if (!el || !hashIs('settings')) return;
    const h = cardHtml();
    if (el._h === h) return;
    el._h = h; el.innerHTML = h;
  }

  /* ---------------------------------------------------------------- 校验（与后端 lb 包保持一致） ---- */
  const RE_ID = /^[a-z][a-z0-9-]{1,31}$/;
  const RE_LABEL = /^[A-Za-z0-9]([A-Za-z0-9_-]{0,61}[A-Za-z0-9])?$/;
  const isIPv4 = h => { const m = h.match(/^(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})$/); return !!m && m.slice(1).every(x => +x <= 255); };
  const isIPv6 = h => /^[0-9a-fA-F:.]+$/.test(h) && (h.match(/:/g) || []).length >= 2 && (h.match(/::/g) || []).length <= 1;
  function validHost(h) {
    if (!h) return false;
    if (isIPv4(h) || isIPv6(h)) return true;
    if (h.length > 253 || /^[0-9.]+$/.test(h)) return false;
    return h.split('.').every(l => RE_LABEL.test(l));
  }
  /* 返回 ''（合法）或错误说明 */
  function checkAddr(raw) {
    const s = String(raw).trim();
    if (!s) return '请填写 主机:端口，例如 10.0.0.5:3306';
    if (s.includes('://')) return '不能包含协议（如 http://），只填 主机:端口';
    if (/[@/?#\\]/.test(s)) return '不能包含凭据（@）或路径（/ ? #），只填 主机:端口';
    if (/\s/.test(s)) return '不能包含空白字符';
    let host, port;
    if (s[0] === '[') {
      const i = s.indexOf(']');
      if (i < 0 || s[i + 1] !== ':') return 'IPv6 格式应为 [::1]:3306';
      host = s.slice(1, i); port = s.slice(i + 2);
      if (!isIPv6(host)) return 'IPv6 地址不合法';
    } else {
      const i = s.lastIndexOf(':');
      if (i < 0) return '缺少端口，格式为 主机:端口，例如 10.0.0.5:3306';
      if (s.indexOf(':') !== i) return 'IPv6 请写成 [::1]:3306';
      host = s.slice(0, i); port = s.slice(i + 1);
      if (!validHost(host)) return '主机部分不是合法的 IP 或主机名';
    }
    if (!/^[0-9]+$/.test(port) || +port < 1 || +port > 65535) return '端口需为 1–65535 的整数';
    return '';
  }
  const canon = a => String(a).trim().toLowerCase();
  const wid = s => { let n = 0; for (const ch of String(s)) n += ch.charCodeAt(0) > 255 ? 2 : 1; return n; };
  const clip = (s, max) => { s = String(s == null ? '' : s); if (wid(s) <= max) return s; let o = '', n = 0; for (const ch of s) { const w = ch.charCodeAt(0) > 255 ? 2 : 1; if (n + w > max - 1) break; o += ch; n += w; } return o + '…'; };

  /* ---------------------------------------------------------------- 架构图：负载均衡层 ---- */
  const GOSET = '<a data-go="settings">环境与设置 → 负载均衡</a>';
  const hintBox = (lv, html) => `<div class="notice ${lv} lbx-hint"><span>${lv === 'warn' ? '⚠' : 'ℹ'}</span><div>${html}</div></div>`;

  /* 当前画图用的数据：{proxies, entries, ready, hint}；ready=false 表示没有可画的数据（只给提示） */
  function archData() {
    if (OP.on) {
      ensureUser();
      if (!LB.d) return {ready: false, proxies: [], entries: [], hint: LB.err ? hintBox('warn', `负载均衡信息读取失败：${E(LB.err)}。图中暂不画负载均衡层，不影响其余部分。`) : hintBox('info', '正在读取负载均衡信息…')};
      return {ready: true, proxies: LB.d.proxies, entries: LB.d.entries, hint: ''};
    }
    const px = (LIVE.snap && LIVE.snap.topology && LIVE.snap.topology.proxies) || [];
    return {ready: true, proxies: px.map(p => Object.assign({source: 'auto'}, p)), entries: [], hint: ''};
  }

  function entryBox(e, x, y, bw, bh, chk) {
    const bad = mismatch(chk), sel = LB.sel === 'e:' + e.id;
    const label = typeShort(e.type), badgeW = Math.round(wid(label) * 6.2 + 14);
    const nameMax = Math.floor((bw - 16 - badgeW - 6 - 10) / 6.9);
    const sub = bad ? '手工登记 · ⚠ 后端与拓扑不一致' : `手工登记${e.backends && e.backends.length ? ' · 后端 ' + e.backends.length : ' · 未登记后端'}`;
    const aria = `负载均衡 ${e.name}，类型 ${typeFull(e.type)}，入口 ${e.addr}，手工登记${bad ? '，后端与拓扑不一致' : ''}，按回车查看详情`;
    return `<g class="lbx-item${sel ? ' sel' : ''}" data-na="lbx-entry" data-id="${E(e.id)}" tabindex="0" role="button" aria-label="${E(aria)}">
      <rect class="lbx-box" x="${x}" y="${y}" width="${bw}" height="${bh}" rx="9" fill="var(--panel)" stroke="${bad ? 'var(--warn)' : 'var(--border)'}" stroke-width="${bad ? 2 : 1}"/>
      <rect x="${x}" y="${y}" width="6" height="${bh}" rx="3" fill="${bad ? 'var(--warn)' : 'var(--accent)'}"/>
      <rect x="${x + 16}" y="${y + 8}" width="${badgeW}" height="17" rx="5" fill="var(--accent-soft)"/>
      <text x="${x + 16 + badgeW / 2}" y="${y + 20.5}" text-anchor="middle" font-size="11" font-weight="700" fill="var(--accent)">${E(label)}</text>
      <text x="${x + 16 + badgeW + 6}" y="${y + 21}" font-size="13" font-weight="600" fill="var(--text)">${E(clip(e.name, nameMax))}</text>
      <text class="lbx-mono" x="${x + 16}" y="${y + 38}" font-size="11.5" fill="var(--muted)">${E(clip(e.addr, Math.floor((bw - 26) / 6.6)))}</text>
      <text x="${x + 16}" y="${y + 52}" font-size="10.5" ${bad ? 'font-weight="700" fill="var(--warn)"' : 'fill="var(--muted)"'}>${E(sub)}</text>
    </g>`;
  }
  function moreBox(n, x, y, bw, bh, what) {
    return `<g class="lbx-item" data-na="lbx-more" tabindex="0" role="button" aria-label="另有 ${n} 个${what}未显示，按回车查看全部">
      <rect class="lbx-box" x="${x}" y="${y}" width="${bw}" height="${bh}" rx="9" fill="var(--panel)" stroke="var(--border)" stroke-dasharray="5 3"/>
      <text x="${x + bw / 2}" y="${y + bh / 2 + 5}" text-anchor="middle" font-size="13" font-weight="600" fill="var(--accent)">+${n} 个${/^[A-Za-z]/.test(what) ? " " : ""}${E(what)}（查看全部）</text></g>`;
  }
  function proxyChip(p, x, y, w, h) {
    const col = stColor(p.status), sel = LB.sel === 'p:' + p.id, addr = `${p.host}:${p.sqlPort}`;
    return `<g class="lbx-item${sel ? ' sel' : ''}" data-na="lbx-proxy" data-id="${E(p.id)}" tabindex="0" role="button" aria-label="${E(`TiProxy ${p.id}，${addr}，${stName(p.status)}，按回车查看详情`)}">
      <rect class="lbx-box" x="${x}" y="${y}" width="${w}" height="${h}" rx="7" fill="var(--panel2)" stroke="var(--border)"/>
      <circle cx="${x + 12}" cy="${y + h / 2}" r="5" fill="${col}"/>
      <text class="lbx-mono" x="${x + 24}" y="${y + h / 2 + 4}" font-size="11.5" fill="var(--text)">${E(clip(addr, 24))}</text>
      <text x="${x + w - 8}" y="${y + h / 2 + 4}" text-anchor="end" font-size="11" font-weight="${p.status === 'up' ? 400 : 700}" fill="${p.status === 'up' ? 'var(--muted)' : col}">${E(stName(p.status))}</text></g>`;
  }

  /* 返回 {h, svg, hint}：h=0 表示不画负载均衡框（客户端箭头直接指向 TiDB 层）。svg 以 y=70 为框的顶部，x=40..740（与 TiDB 层同宽） */
  function archLayer() {
    const d = archData();
    if (!d.ready) return {h: 0, svg: '', hint: d.hint};
    const proxies = d.proxies, entries = d.entries;
    if (!proxies.length && !entries.length) {
      let hint;
      if (OP.on) {
        const flag = LB.d && LB.d.effective && LB.d.effective.reason === 'legacy-flag';
        hint = hintBox('info', flag
          ? `平台设置里已勾选「集群前有负载均衡」，但还没有登记具体设备，图中无法画出：请到「${GOSET}」登记（HAProxy / LVS / 云 SLB 等）。`
          : `未发现负载均衡：若集群前有 HAProxy / LVS / 云 SLB 等，请到「${GOSET}」登记。`);
      } else hint = hintBox('info', '未发现 TiProxy。若集群前有 HAProxy / LVS / 云 SLB 等，需在运维模式下到「环境与设置 → 负载均衡」登记后才会显示在图中。');
      return {h: 0, svg: '', hint};
    }
    const X0 = 40, W = 700, IN = 12, TOP = 70;
    let cy = TOP + 26, inner = '';
    if (proxies.length) {
      const MAXP = 9, shown = proxies.length > MAXP ? proxies.slice(0, MAXP - 1) : proxies, chips = shown.length + (proxies.length > MAXP ? 1 : 0);
      const rows = Math.ceil(chips / 3), gh = 26 + rows * 28 + 2, down = proxies.filter(p => p.status !== 'up').length;
      inner += `<rect x="${X0 + IN}" y="${cy}" width="${W - 2 * IN}" height="${gh}" rx="9" fill="var(--panel)" stroke="var(--border)"/>
        <text x="${X0 + IN + 12}" y="${cy + 18}" font-size="13" fill="var(--text)"><tspan font-weight="700">TiProxy ×${proxies.length}</tspan><tspan font-size="11" fill="var(--muted)"> · 自动发现</tspan>${down ? `<tspan font-size="11" font-weight="700" fill="var(--bad)"> · ${down} 个非运行中</tspan>` : ''}</text>`;
      shown.forEach((p, i) => { inner += proxyChip(p, X0 + IN + 12 + (i % 3) * 220, cy + 26 + Math.floor(i / 3) * 28, 212, 22); });
      if (proxies.length > MAXP) { const i = shown.length; inner += moreBox(proxies.length - shown.length, X0 + IN + 12 + (i % 3) * 220, cy + 26 + Math.floor(i / 3) * 28, 212, 22, 'TiProxy').replace('font-size="13"', 'font-size="11.5"'); }
      cy += gh + 10;
    }
    if (entries.length) {
      const bw = 218, bh = 58, MAXE = 12, over = entries.length > MAXE, shown = over ? entries.slice(0, MAXE - 1) : entries, cells = shown.length + (over ? 1 : 0), rows = Math.ceil(cells / 3);
      shown.forEach((e, i) => { inner += entryBox(e, X0 + IN + (i % 3) * 228, cy + Math.floor(i / 3) * 68, bw, bh, checkOf(e.id)); });
      if (over) { const i = shown.length; inner += moreBox(entries.length - shown.length, X0 + IN + (i % 3) * 228, cy + Math.floor(i / 3) * 68, bw, bh, '负载均衡'); }
      cy += rows * 68 - 10;
    }
    const h = cy - TOP + IN, nBad = entries.filter(e => mismatch(checkOf(e.id))).length;
    const label = `负载均衡层 · 客户端入口（${proxies.length ? 'TiProxy ' + proxies.length : ''}${proxies.length && entries.length ? ' + ' : ''}${entries.length ? '手工登记 ' + entries.length : ''}）`;
    const svg = `<g class="lbx-layer"><rect x="${X0}" y="${TOP}" width="${W}" height="${h}" rx="12" fill="var(--panel2)" stroke="var(--border)"/><text class="layer-label" x="${X0 + 12}" y="${TOP + 18}">${E(label)}</text>${inner}</g>`;
    const hint = nBad ? hintBox('warn', `有 ${nBad} 个手工登记的负载均衡，其后端与拓扑里的 TiDB 不一致（图中以警示色标出）：点击该条目查看差异，或到「${GOSET}」修正。`) : '';
    return {h, svg, hint};
  }

  /* 替换 app.js 里的默认实现：只有运维模式 / 有实时数据（后端可用）时才按真实数据画，静态原型保持演示框 */
  const _ll = pages.arch.lbLayer;
  pages.arch.lbLayer = function () {
    if (!OP.on && !LIVE.on) return _ll.call(this);
    return archLayer();
  };
  const _ai = pages.arch.init;
  pages.arch.init = function () {
    _ai.call(this);
    if (OP.on) load(false);
  };
  const _ad = pages.arch.detail;
  pages.arch.detail = function (id) { LB.sel = ''; return _ad.call(this, id); };

  /* ---------------------------------------------------------------- 详情抽屉 ---- */
  const kvRow = (k, v) => `<span>${k}</span><span>${v}</span>`;
  const mono = s => `<span class="mono" style="word-break:break-all">${E(s)}</span>`;
  const closeRow = (extra = '') => `<div class="sticky-actions">${extra}<button class="btn" data-close="1">关闭</button></div>`;
  const goBtn = '<button class="btn primary" data-go="settings">去管理（环境与设置）</button>';

  function entryDetail(id) {
    const e = ((LB.d && LB.d.entries) || []).find(x => x.id === id); if (!e) { toast('该负载均衡登记已不存在'); return; }
    const chk = checkOf(id), bk = e.backends || [], extra = new Set(((chk && chk.extra) || []).map(canon)), missing = (chk && chk.missing) || [];
    const state = !bk.length ? tg('未登记后端，不做一致性检查') : !chk ? tg('尚无拓扑快照，无法对照', 'warn') : mismatch(chk) ? tg('后端与拓扑不一致', 'warn') : tg('后端与拓扑一致', 'ok');
    openDrawer('负载均衡 · ' + e.name, `
      <div class="row">${tg(typeFull(e.type), 'info')}${tg('手工登记')}${state}</div>
      <div class="kv" style="margin:12px 0">
        ${kvRow('类型', E(typeFull(e.type)))}${kvRow('名称', E(e.name))}${kvRow('id', mono(e.id))}
        ${kvRow('入口地址', `${mono(e.addr)} <button class="btn sm" data-copy="${E(e.addr)}">复制</button>`)}
        ${kvRow('来源', '手工登记（平台元数据）')}${kvRow('备注', e.note ? E(e.note).replace(/\n/g, '<br>') : '<span class="muted">—</span>')}
        ${kvRow('创建', E(fDT(e.created)))}${kvRow('最近更新', `${E(fDT(e.updated))}${e.updatedBy ? ' · ' + E(e.updatedBy) : ''}`)}</div>
      <h3 class="ou-sec">后端 TiDB（${bk.length}）</h3>
      ${bk.length ? `<div class="lbx-bklist">${bk.map(b => { const bad = chk && extra.has(canon(b)); return !chk ? `<div class="lbx-bk"><span class="muted">·</span> ${mono(b)}</div>` : `<div class="lbx-bk ${bad ? 'bad' : 'ok'}"><span>${bad ? '⚠' : '✔'}</span> ${mono(b)} <span class="small">${bad ? '拓扑里没有这个 TiDB' : '在拓扑内'}</span></div>`; }).join('')}</div>`
        : '<div class="small muted">没有登记后端，因此不会与拓扑做一致性检查。需要时可在「编辑」里填入（支持「从拓扑填入当前全部 TiDB」）。</div>'}
      ${missing.length ? `<h3 class="ou-sec">拓扑里有但未登记的 TiDB（${missing.length}）</h3><div class="lbx-bklist">${missing.map(b => `<div class="lbx-bk bad"><span>⚠</span> ${mono(b)} <span class="small">没有登记为该负载均衡的后端</span></div>`).join('')}</div>` : ''}
      ${mismatch(chk) ? '<div class="notice warn"><span>⚠</span><div>后端与拓扑不一致：新增 / 下线 TiDB 后请同步更新该登记，缩容 / 维护时才不会漏摘或误判。</div></div>' : ''}
      <div class="small muted">登记是平台元数据，不会改动 ${E(typeShort(e.type))} 本身。</div>
      ${closeRow(goBtn)}`);
  }
  function proxyDetail(id) {
    const p = ((LB.d && LB.d.proxies) || (LIVE.snap && LIVE.snap.topology && LIVE.snap.topology.proxies) || []).find(x => x.id === id); if (!p) { toast('该 TiProxy 已不在拓扑里'); return; }
    openDrawer('TiProxy · ' + p.id, `
      <div class="row">${tg('TiProxy', 'info')}${tg('自动发现')}<span class="tag ${stCls(p.status)}">${E(stName(p.status))}</span></div>
      <div class="kv" style="margin:12px 0">
        ${kvRow('实例', mono(p.id))}${kvRow('主机', mono(p.host))}
        ${kvRow('SQL 端口', `${mono(p.sqlPort)} <span class="muted small">客户端连接地址 ${E(p.host + ':' + p.sqlPort)}</span> <button class="btn sm" data-copy="${E(p.host + ':' + p.sqlPort)}">复制</button>`)}
        ${kvRow('API 端口', p.apiPort ? mono(p.apiPort) : '<span class="muted">—（未采集）</span>')}
        ${kvRow('状态', `<span class="dot ${stCls(p.status)}"></span> ${E(stName(p.status))}（原始值 ${E(p.status || '—')}）`)}
        ${kvRow('版本', p.version ? mono(p.version) : '<span class="muted">—（tiup display 不给出单实例版本）</span>')}
        ${kvRow('来源', '自动发现（tiup cluster display）')}</div>
      <div class="notice info"><span>ℹ</span><div>自动发现的 TiProxy 为只读信息，不能在平台里编辑。平台目前<b>不把它算作「集群前有负载均衡」</b>来决定缩容 / 维护 TiDB 时的人工摘除步骤；如需该提醒，请登记或勾选「集群前有负载均衡」。</div></div>
      ${closeRow(OP.on ? goBtn : '')}`);
  }
  function moreList() {
    const d = archData();
    const rows = [];
    d.proxies.forEach(p => rows.push(`<tr><td>${tg('TiProxy', 'info')}</td><td><span class="mono">${E(p.host + ':' + p.sqlPort)}</span><div class="small muted">${E(p.id)}</div></td><td><span class="dot ${stCls(p.status)}"></span> ${E(stName(p.status))}</td><td>自动发现</td><td><button class="btn sm" data-na="lbx-proxy" data-id="${E(p.id)}">详情</button></td></tr>`));
    d.entries.forEach(e => rows.push(`<tr><td>${tg(typeShort(e.type), 'info')}</td><td><b>${E(e.name)}</b><div class="small mono muted">${E(e.addr)}</div></td><td>${mismatch(checkOf(e.id)) ? tg('后端与拓扑不一致', 'warn') : '<span class="muted">—</span>'}</td><td>手工登记</td><td><button class="btn sm" data-na="lbx-entry" data-id="${E(e.id)}">详情</button></td></tr>`));
    openDrawer('全部负载均衡', `<div class="tbl-wrap"><table><thead><tr><th>类型</th><th>名称 / 入口</th><th>状态</th><th>来源</th><th></th></tr></thead><tbody>${rows.join('')}</tbody></table></div>${closeRow(OP.on ? goBtn : '')}`);
  }

  /* ---------------------------------------------------------------- 设置页卡片 ---- */
  const effectiveHtml = () => {
    const ef = (LB.d && LB.d.effective) || {hasLB: false, reason: 'none'}, n = LB.d.entries.length;
    const why = {
      'legacy-flag': `依据：已勾选上方「平台设置」卡片里的复选框「集群前有负载均衡」（旧开关）${n ? `；同时也登记了 ${n} 条具体设备` : '；还没有登记具体设备'}。`,
      manual: `依据：已手工登记 ${n} 条负载均衡（未勾选旧开关）。`,
      none: '依据：既没有勾选「集群前有负载均衡」，也没有手工登记任何负载均衡。'}[ef.reason] || `依据：${E(ef.reason || '—')}`;
    const effect = ef.hasLB
      ? '缩容 / 维护 TiDB 节点时，变更计划里会出现「需人工确认」的摘除步骤' + (n ? '，并点名上面登记的具体入口。' : '。')
      : '缩容 / 维护 TiDB 节点时，不会要求人工先从负载均衡摘除：若集群前其实有负载均衡，请登记或勾选开关。';
    return `<div class="notice ${ef.hasLB ? 'ok' : 'warn'}"><span>${ef.hasLB ? '✔' : '⚠'}</span><div><b>当前平台${ef.hasLB ? '认为集群前有负载均衡' : '认为集群前没有负载均衡'}。</b>${E(why)}${E(effect)}
      <div class="small" style="margin-top:4px;opacity:.9">原有复选框「集群前有负载均衡」（在上方「平台设置」卡片）保持不变：未登记具体设备时可勾选，缩容 / 维护 TiDB 时会要求人工摘除。自动发现的 TiProxy 目前不参与这个判定。</div></div></div>`;
  };
  const checkOne = c => {
    const e = LB.d.entries.find(x => x.id === c.id), nm = e ? `${E(e.name)}（${E(e.addr)}）` : E(c.id);
    return `${(c.missing || []).length ? `<div class="notice warn"><span>⚠</span><div><b>拓扑里有但未登记的 TiDB</b>（负载均衡 ${nm}）：<span class="mono">${(c.missing).map(x => E(x)).join('、')}</span></div></div>` : ''}${(c.extra || []).length ? `<div class="notice warn"><span>⚠</span><div><b>登记了但拓扑里没有的后端</b>（负载均衡 ${nm}）：<span class="mono">${(c.extra).map(x => E(x)).join('、')}</span></div></div>` : ''}`;
  };
  const checkHtml = () => {
    const bad = (LB.d.check || []).filter(mismatch);
    return bad.slice(0, 3).map(checkOne).join('') + (bad.length > 3 ? `<div class="notice warn"><span>⚠</span><div>另有 ${bad.length - 3} 条登记的后端与拓扑不一致，见下表中标有「与拓扑不一致」的行（点「详情」查看差异）。</div></div>` : '');
  };

  function cardHtml() {
    ensureUser();
    const ok = can(), reqTip = '需要「修改平台设置」（settings.edit）能力', ctx = window.CL && CL.active() && CL.name() ? '集群 ' + E(CL.name()) + ' · ' : '';
    const head = `<h2>负载均衡 <span class="sub">${ctx}拓扑图的负载均衡层 · 自动发现 + 手工登记</span><span class="right"><button class="btn sm" data-na="lbx-reload">刷新</button>
      <button class="btn primary sm" data-na="lbx-new" ${ok && !(LB.d && LB.d.entries.length >= MAX_ENTRIES) ? '' : `disabled title="${E(ok ? '已达到上限（' + MAX_ENTRIES + ' 条）' : reqTip)}"`}>＋ 登记负载均衡</button></span></h2>`;
    if (!LB.d) return `<div class="card">${head}${LB.err ? `<div class="notice bad"><span>✖</span><div>${E(LB.err)}</div></div>` : '<div class="empty">正在读取…</div>'}</div>`;
    const d = LB.d, px = d.proxies, en = d.entries;
    const pxRows = px.map(p => `<tr><td class="mono">${E(p.id)}</td><td class="mono">${E(p.host + ':' + p.sqlPort)}</td><td class="mono">${p.apiPort ? E(p.apiPort) : '<span class="muted">—</span>'}</td><td><span class="dot ${stCls(p.status)}"></span> ${E(stName(p.status))}</td><td>${tg('自动发现', 'info')}</td></tr>`).join('');
    const enRows = en.map(e => {
      const c = checkOf(e.id), bad = mismatch(c);
      return `<tr class="${bad ? 'lbx-bad' : ''}"><td>${tg(typeFull(e.type), 'info')}</td><td><b>${E(e.name)}</b><div class="small mono muted">${E(e.id)}</div></td><td class="mono">${E(e.addr)}</td>
        <td>${(e.backends || []).length ? E(e.backends.length) + ' 个' : '<span class="muted">未登记</span>'}${bad ? ' ' + tg('与拓扑不一致', 'warn') : ''}</td>
        <td class="small lbx-note">${e.note ? E(e.note) : '<span class="muted">—</span>'}</td><td class="small">${e.updatedBy ? E(e.updatedBy) + '<br>' : ''}<span class="muted">${E(fDT(e.updated))}</span></td>
        <td><div class="acts"><button class="btn sm" data-na="lbx-entry" data-id="${E(e.id)}">详情</button>
          <button class="btn sm" data-na="lbx-edit" data-id="${E(e.id)}" ${ok ? '' : `disabled title="${E(reqTip)}"`}>编辑</button>
          <button class="btn sm danger" data-na="lbx-del" data-id="${E(e.id)}" ${ok ? '' : `disabled title="${E(reqTip)}"`}>删除</button></div></td></tr>`;
    }).join('');
    return `<div class="card">${head}
      ${ok ? '' : `<div class="notice warn"><span>⚠</span><div>你没有「修改平台设置」（settings.edit）权限，负载均衡登记为只读：登记 / 编辑 / 删除按钮已置灰。</div></div>`}
      ${effectiveHtml()}${checkHtml()}
      <h3 class="ou-sec">自动发现的 TiProxy <span class="small muted">来自 tiup cluster display · 只读 · ${px.length} 个</span></h3>
      <div class="tbl-wrap"><table class="lbx-tbl"><thead><tr><th>实例 id</th><th>地址 host:sqlPort</th><th>API 端口</th><th>状态</th><th>来源</th></tr></thead><tbody>
        ${pxRows || '<tr><td colspan="5" class="empty">未发现 TiProxy（集群里没有 TiProxy 组件，或平台还没有拓扑快照）</td></tr>'}</tbody></table></div>
      <h3 class="ou-sec">手工登记 <span class="small muted">HAProxy / LVS / Nginx / 云 SLB / F5 等 · ${en.length} / ${MAX_ENTRIES} 条</span></h3>
      <div class="tbl-wrap"><table class="lbx-tbl lbx-tbl2"><thead><tr><th>类型</th><th>名称</th><th>入口</th><th>后端数</th><th>备注</th><th>更新人 / 时间</th><th>操作</th></tr></thead><tbody>
        ${enRows || '<tr><td colspan="7" class="empty">还没有手工登记。若集群前有 HAProxy / LVS / 云 SLB 等，点右上角「＋ 登记负载均衡」。</td></tr>'}</tbody></table></div>
      <div class="small muted" style="margin-top:10px">登记是平台元数据，<b>不会改动 HAProxy / LVS 本身</b>，也不要求打开写操作开关、不走审批（会写入审计：lb.add / lb.update / lb.delete）。登记后端后，平台会与拓扑里的 TiDB 比对并提示不一致；登记的入口会出现在「集群架构」图的负载均衡层。最多登记 ${MAX_ENTRIES} 条。</div></div>`;
  }

  /* ---------------------------------------------------------------- 登记 / 编辑抽屉 ---- */
  const newForm = () => ({mode: 'add', id: '', type: 'haproxy', name: '', addr: '', backends: '', note: '', err: '', busy: false});
  const editForm = e => ({mode: 'edit', id: e.id, type: e.type, name: e.name, addr: e.addr, backends: (e.backends || []).join('\n'), note: e.note || '', err: '', busy: false});
  const parseBackends = txt => String(txt).split(/\r?\n/).map(s => s.trim()).filter(Boolean);
  const topoTidb = () => (typeof NODES !== 'undefined' ? NODES : []).filter(n => n.type === 'tidb').map(n => `${n.host}:${n.port}`);

  function idHint(F) {
    if (F.mode !== 'add') return '';
    if (!F.id) return '<span class="muted">2–32 位：小写字母开头，只含小写字母 / 数字 / 连字符；创建后不可修改</span>';
    if (!RE_ID.test(F.id)) return '<span style="color:var(--bad)">✖ 格式不符：小写字母开头，只含小写字母 / 数字 / 连字符，2–32 位</span>';
    if (LB.d && LB.d.entries.some(e => e.id === F.id)) return '<span style="color:var(--bad)">✖ 该 id 已存在</span>';
    return '<span style="color:var(--ok)">✔ 可用</span>';
  }
  function addrHint(F) {
    if (!F.addr.trim()) return '<span class="muted">主机:端口，不含协议 / 凭据，例如 10.0.0.5:3306</span>';
    const m = checkAddr(F.addr);
    return m ? `<span style="color:var(--bad)">✖ ${E(m)}</span>` : '<span style="color:var(--ok)">✔ 格式正确</span>';
  }
  /* 返回 {lines, bad: [{n, line, msg}], dup} */
  function bkState(F) {
    const lines = parseBackends(F.backends), seen = new Set(), bad = []; let dup = 0;
    lines.forEach((l, i) => { const m = checkAddr(l); if (m) bad.push({n: i + 1, line: l, msg: m}); if (seen.has(canon(l))) dup++; seen.add(canon(l)); });
    return {lines, bad, dup, uniq: seen.size, set: seen};
  }
  function bkHint(F) {
    const s = bkState(F);
    if (!s.lines.length) return '<span class="muted">留空 = 不做一致性检查；每行一个 host:port（TiDB 的 SQL 端口），最多 ' + MAX_BACKENDS + ' 个</span>';
    const out = [`<span class="muted">共 ${s.lines.length} 行</span>`];
    if (s.dup) out.push(`<span style="color:var(--warn)">⚠ 有 ${s.dup} 个重复项，保存时自动去重（实际 ${s.uniq} 个）</span>`);
    if (s.uniq > MAX_BACKENDS) out.push(`<span style="color:var(--bad)">✖ 最多 ${MAX_BACKENDS} 个后端</span>`);
    if (s.bad.length) out.push(`<span style="color:var(--bad)">✖ 第 ${s.bad[0].n} 行「${E(clip(s.bad[0].line, 40))}」：${E(s.bad[0].msg)}${s.bad.length > 1 ? `（另有 ${s.bad.length - 1} 行有问题）` : ''}</span>`);
    else if (LIVE.on) {
      const topo = topoTidb(), tset = new Set(topo.map(canon)), notIn = [...s.set].filter(a => !tset.has(a)), miss = topo.filter(a => !s.set.has(canon(a)));
      out.push(!notIn.length && !miss.length ? '<span style="color:var(--ok)">✔ 与当前拓扑里的 TiDB 完全一致</span>' : `<span style="color:var(--warn)">⚠ 与当前拓扑对比：${notIn.length ? `${notIn.length} 个后端不在拓扑里` : ''}${notIn.length && miss.length ? '；' : ''}${miss.length ? `拓扑里有 ${miss.length} 个 TiDB 未包含` : ''}（可保存，保存后会在卡片和图里提示）</span>`);
    }
    return out.join('<br>');
  }
  const nameHint = F => { const n = [...F.name.trim()].length; return n > 40 ? '<span style="color:var(--bad)">✖ 名称最多 40 个字</span>' : `<span class="muted">${n} / 40</span>`; };
  const noteHint = F => { const n = [...F.note.trim()].length; return n > 200 ? '<span style="color:var(--bad)">✖ 备注最多 200 个字</span>' : `<span class="muted">${n} / 200</span>`; };

  function formHtml() {
    const F = LB.form, add = F.mode === 'add';
    return `${add ? `<div class="field"><label>id（平台内唯一；创建后不可修改）</label><input type="text" data-ni="lbx-f" data-k="id" value="${E(F.id)}" maxlength="32" autocomplete="off" spellcheck="false" placeholder="例如 haproxy-1、lvs-prod">
        <div class="small" id="lbxIdHint" style="min-height:16px">${idHint(F)}</div></div>`
      : `<div class="kv" style="margin-bottom:10px">${kvRow('id', `${mono(F.id)} <span class="muted small">（不可修改）</span>`)}</div>`}
      <div class="lbx-g2"><div class="field"><label>类型</label><select data-nc="lbx-f" data-k="type">${TYPES.map(([v, t]) => `<option value="${v}" ${F.type === v ? 'selected' : ''}>${E(t)}</option>`).join('')}</select></div>
        <div class="field"><label>名称（1–40 字）</label><input type="text" data-ni="lbx-f" data-k="name" value="${E(F.name)}" autocomplete="off" placeholder="例如 生产 HAProxy 主" maxlength="60"><div class="small" id="lbxNameHint">${nameHint(F)}</div></div></div>
      <div class="field"><label>入口地址 addr（客户端连接的 VIP / 域名 : 端口）</label><input type="text" data-ni="lbx-f" data-k="addr" value="${E(F.addr)}" autocomplete="off" spellcheck="false" placeholder="10.0.0.5:3306"><div class="small" id="lbxAddrHint" style="min-height:16px">${addrHint(F)}</div></div>
      <div class="field"><label>后端 TiDB 列表（每行一个 host:port，使用 TiDB 的 SQL 端口；可选）</label>
        <textarea data-ni="lbx-f" data-k="backends" rows="6" spellcheck="false" class="mono" placeholder="10.0.1.11:4000&#10;10.0.1.12:4000">${E(F.backends)}</textarea>
        <div class="row" style="margin:2px 0"><button type="button" class="btn sm" data-na="lbx-fill" ${LIVE.on ? '' : 'disabled title="没有实时拓扑数据"'}>从拓扑填入当前全部 TiDB</button><span class="small muted">会覆盖文本框里的内容</span></div>
        <div class="small" id="lbxBkHint">${bkHint(F)}</div></div>
      <div class="field"><label>备注（可选，≤200 字）</label><textarea data-ni="lbx-f" data-k="note" rows="2" placeholder="例如 机房 A 的 VIP，由网络组维护">${E(F.note)}</textarea><div class="small" id="lbxNoteHint">${noteHint(F)}</div></div>
      <div class="notice info"><span>ℹ</span><div>登记是平台元数据：<b>不会改动 HAProxy / LVS 本身</b>，也不要求打开写操作开关。</div></div>
      <div id="lbxFormErr" class="small" style="min-height:18px">${F.err ? `<div class="notice bad"><span>✖</span><div>${E(F.err)}</div></div>` : ''}</div>
      <div class="sticky-actions"><button class="btn primary" data-na="lbx-save" ${F.busy ? 'disabled' : ''}>${add ? '登记' : '保存'}</button><button class="btn" data-close="1">取消</button></div>`;
  }
  const formOpen = (F, title) => { LB.form = F; openDrawer(title, formHtml()); };
  const say = m => { if (LB.form) LB.form.err = m; const e = $('#lbxFormErr'); if (e) e.innerHTML = m ? `<div class="notice bad"><span>✖</span><div>${E(m)}</div></div>` : ''; };

  const ERR = {invalid: '参数不合法', exists: '该 id 已被占用', limit: `已达上限（每个集群最多 ${MAX_ENTRIES} 条）`, not_found: '这条登记已不存在（可能已被他人删除）', store: '后端保存失败（请检查中控机数据目录的权限与磁盘空间）'};
  function errText(r) {
    const c = r.data && r.data.code, m = (r.data && r.data.error) || (r.code === 0 ? '无法连接后端' : '请求失败（' + r.code + '）');
    if (r.code === 403) return '没有权限：登记 / 编辑 / 删除负载均衡需要「修改平台设置」（settings.edit）能力。';
    if (c === 'not_found') return m + '（可能已被他人删除）';
    if (c === 'exists' || c === 'limit') return m;   // 后端文案已经足够清楚
    return (ERR[c] && !m.includes(ERR[c]) ? ERR[c] + '：' : '') + m;
  }

  async function save(btn) {
    const F = LB.form; if (!F || F.busy) return;
    const add = F.mode === 'add', s = bkState(F);
    if (add && !RE_ID.test(F.id)) return say('id 格式不符：小写字母开头，只含小写字母 / 数字 / 连字符，2–32 位');
    if (add && LB.d && LB.d.entries.some(e => e.id === F.id)) return say('该 id 已存在，请换一个');
    if (add && LB.d && LB.d.entries.length >= MAX_ENTRIES) return say(ERR.limit);
    if (!TYPE_FULL[F.type]) return say('类型不合法');
    const nm = F.name.trim(); if (!nm || [...nm].length > 40) return say('名称需为 1–40 个字');
    const am = checkAddr(F.addr); if (am) return say('入口地址：' + am);
    if (s.bad.length) return say(`后端第 ${s.bad[0].n} 行「${s.bad[0].line}」：${s.bad[0].msg}`);
    if (s.uniq > MAX_BACKENDS) return say(`后端最多 ${MAX_BACKENDS} 个`);
    if ([...F.note.trim()].length > 200) return say('备注最多 200 个字');
    const body = {type: F.type, name: nm, addr: F.addr.trim(), backends: s.lines, note: F.note.trim()};
    if (add) body.id = F.id;
    F.busy = true; btn.disabled = true; say('');
    const r = await opApi('POST', add ? '/api/ops/lb' : `/api/ops/lb/${encodeURIComponent(F.id)}`, body);
    F.busy = false; btn.disabled = false;
    if (r.code !== 200) { say(errText(r)); if (r.data && (r.data.code === 'not_found' || r.data.code === 'exists' || r.data.code === 'limit')) { invalidate(); load(true); } return; }
    LB.form = null; closeDrawer(); toast(add ? `已登记负载均衡 ${nm}` : `已保存 ${nm}`);
    invalidate(); await load(true);
  }
  function delOpen(id) {
    const e = ((LB.d && LB.d.entries) || []).find(x => x.id === id); if (!e) return;
    const last = LB.d.entries.length === 1 && LB.d.effective.reason === 'manual';
    oppConfirm('删除负载均衡登记', `确认删除负载均衡登记 <b>${E(e.name)}</b>（${E(typeFull(e.type))}，入口 <span class="mono">${E(e.addr)}</span>）？<br>只删除平台里的登记信息，<b>不会改动该负载均衡设备本身</b>。${last ? '<br>这是最后一条登记：删除后（且未勾选「集群前有负载均衡」），缩容 / 维护 TiDB 时将不再要求人工摘除。' : ''}`, '确认删除', async () => {
      const r = await opApi('POST', `/api/ops/lb/${encodeURIComponent(id)}/delete`, {});
      if (r.code === 200 || (r.data && r.data.code === 'not_found')) { toast(r.code === 200 ? `已删除 ${e.name}` : '该登记已不存在'); } else toast(errText(r));
      invalidate(); await load(true);
    }, true);
  }

  /* ---------------------------------------------------------------- 事件 ---- */
  const CLICK = {
    'lbx-entry': d => { const k = 'e:' + d.id; LB.sel = k; entryDetail(d.id); paintArch(); focusItem(k); },
    'lbx-proxy': d => { const k = 'p:' + d.id; LB.sel = k; proxyDetail(d.id); paintArch(); focusItem(k); },
    'lbx-more': () => moreList(),
    'lbx-reload': () => { invalidate(); load(true).then(() => toast(LB.d ? '已刷新' : '刷新失败')); },
    'lbx-new': () => { if (!can()) return; formOpen(newForm(), '登记负载均衡'); },
    'lbx-edit': d => { const e = ((LB.d && LB.d.entries) || []).find(x => x.id === d.id); if (e && can()) formOpen(editForm(e), '编辑负载均衡：' + e.name); },
    'lbx-del': d => { if (can()) delOpen(d.id); },
    'lbx-save': (d, el) => save(el),
    'lbx-fill': () => {
      const F = LB.form; if (!F) return; const list = topoTidb();
      if (!list.length) { toast('当前拓扑里没有 TiDB 实例'); return; }
      F.backends = list.join('\n'); const t = $('#drBody textarea[data-k="backends"]'); if (t) t.value = F.backends;
      const h = $('#lbxBkHint'); if (h) h.innerHTML = bkHint(F);
      toast(`已填入 ${list.length} 个 TiDB`);
    },
  };
  function focusItem(key) { const g = [...document.querySelectorAll('#archSvg .lbx-item')].find(x => (x.dataset.na === 'lbx-entry' ? 'e:' : 'p:') + x.dataset.id === key); if (g) g.focus(); }
  document.addEventListener('click', e => {
    const el = e.target.closest && e.target.closest('[data-na^="lbx-"]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  // 图里的可聚焦条目：回车 / 空格等同点击
  document.addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const el = e.target.closest && e.target.closest('.lbx-item'); if (!el || e.target !== el) return;
    e.preventDefault(); const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('input', e => {
    const el = e.target.closest && e.target.closest('[data-ni="lbx-f"]'); if (!el || !LB.form) return;
    const F = LB.form, k = el.dataset.k; F[k] = el.value;
    const set = (id, h) => { const x = $('#' + id); if (x) x.innerHTML = h; };
    if (k === 'id') set('lbxIdHint', idHint(F)); else if (k === 'name') set('lbxNameHint', nameHint(F)); else if (k === 'addr') set('lbxAddrHint', addrHint(F)); else if (k === 'backends') set('lbxBkHint', bkHint(F)); else if (k === 'note') set('lbxNoteHint', noteHint(F));
  });
  document.addEventListener('change', e => {
    const el = e.target.closest && e.target.closest('[data-nc="lbx-f"]'); if (!el || !LB.form) return;
    LB.form[el.dataset.k] = el.value;
  });
  // 设置页勾选 / 取消「集群前有负载均衡」会改变 effective.hasLB：ops_pages.js 保存成功后调用它，重新读取 /api/lb
  window.lbxRefresh = () => { invalidate(); return load(true); };
  // 「进入」页面（与上一次渲染的不是同一页）时让缓存失效；快照每 30 秒的自动重绘渲染的是同一页，不会触发
  const _render = window.render;
  let lastPage = '';
  window.render = function (name) {
    const n = String(name).split('/')[0];
    if (n !== lastPage) { lastPage = n; LB.at = 0; }
    return _render.apply(this, arguments);
  };

  /* ---------------------------------------------------------------- 包装设置页 ---- */
  const _st = pages.settings;
  pages.settings = {
    ..._st,
    render() {
      const h = _st.render.call(this);
      if (!OP.on) return h;
      const card = `<div id="lbxBox" class="lbx-box-wrap" style="margin:16px 0">${cardHtml()}</div>`;
      const mark = '<div class="grid" style="margin-top:16px"><div id="ontBox">';
      return h.includes(mark) ? h.replace(mark, card + mark) : h + card;
    },
    init() {
      _st.init.call(this);
      if (!OP.on) return;
      load(false).then(paintSettings);
    },
  };
})();
