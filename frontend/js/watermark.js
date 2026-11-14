// @author: kevinkelly
// @license: Apache License 2.0
// @file: watermark.js
// @time: 2026/10/28

/* 页面水印：管理员在「平台配置 → 水印设置」（pages.watermark）里定义，登录后全站叠加显示。
     - 设置：启用开关、文字模板（变量 {user} 登录名 / {display} 显示名 / {env} 环境 / {date} 日期 / {time} 时间）、
       透明度、字号、旋转角度、颜色、水平 / 垂直间距；带实时预览；保存即在本页生效，其他用户最迟 60 秒内生效（或刷新页面）。
     - 显示：一个铺满视口的覆盖层（pointer-events:none，不挡任何操作），内容用 canvas 平铺；覆盖层被删除或被改样式会自动恢复。
       这只是前端的震慑手段，不是安全边界：有开发者工具的人仍可绕过，需要强约束请在截屏 / 录屏策略上另行处理。
     - 默认关闭；未登录（登录页）和非运维模式不显示。
   接口：GET /api/watermark（登录即可）；POST /api/watermark（settings.edit，即内置管理员）。
   原型模式（后端没有启用运维模式）：水印设置页始终可用，设置存本浏览器 localStorage（键 wmProto），校验规则与后端一致，示例账号「演示用户」。
   侧栏菜单由本脚本在「集群接入」之后注入，只有管理员可见。data-na / data-ni / data-nc 前缀：wm-。
   依赖 ops.js（OP / opApi / opEsc / opCan）、app.js（pages / toast / $ / $$）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const ENV = {prod: '生产', staging: '预发', test: '测试'};
  const WM = {cfg: null, defaults: null, limits: {}, canManage: false, user: '', at: 0, loading: false, minute: '', form: null, saving: false, err: ''};
  const OVERLAY_ID = 'wmOverlay';

  /* 原型模式（后端没有启用运维模式：静态原型 / 只读 / 回放）：没有登录与后端接口，水印设置存在本浏览器的 localStorage，
     页面始终可体验；示例账号是「演示用户」。PROTO 为 null 表示还没探测完。 */
  let PROTO = null;
  const LSK = 'wmProto';
  const lsGet = () => { try { return JSON.parse(localStorage.getItem(LSK) || 'null'); } catch (e) { return null; } };
  const lsSet = v => { try { localStorage.setItem(LSK, JSON.stringify(v)); return true; } catch (e) { return false; } };
  const PROTO_DEFAULTS = {enabled: false, text: '{display} {user} {date}', opacity: 0.12, fontSize: 16, angle: -20, color: '#808080', gapX: 120, gapY: 100};
  const PROTO_LIMITS = {maxText: 100, vars: ['{user}', '{display}', '{env}', '{date}', '{time}']};
  const active = () => PROTO === true || !!(OP.on && OP.me);
  const me = () => PROTO ? 'demo' : (OP.me && OP.me.user && OP.me.user.name) || '';
  /* 与后端 watermark_api.go 的校验保持一致（原型里没有后端可校验） */
  function check(f) {
    const t = String(f.text || '').trim();
    if ([...t].length > 100) return '水印文字最多 100 个字';
    if (/[\u0000-\u001f\u007f]/.test(t)) return '水印文字不能含换行或控制字符';
    for (const v of t.match(/\{[^{}]*\}/g) || []) if (!PROTO_LIMITS.vars.includes(v)) return '不支持的变量 ' + v + '，可用：' + PROTO_LIMITS.vars.join(' ');
    if (f.enabled && !t) return '启用水印时文字不能为空';
    if (!(f.opacity >= 0.02 && f.opacity <= 0.5)) return '透明度须在 0.02–0.5 之间';
    if (!(f.fontSize >= 10 && f.fontSize <= 48)) return '字号须在 10–48 之间';
    if (!(f.angle >= -90 && f.angle <= 90)) return '旋转角度须在 -90–90 之间';
    if (!/^#[0-9a-fA-F]{6}$/.test(f.color)) return '颜色须为 #rrggbb 格式';
    if (!(f.gapX >= 40 && f.gapX <= 400 && f.gapY >= 40 && f.gapY <= 400)) return '水平 / 垂直间距须在 40–400 之间';
    return '';
  }
  fetch('/api/health', {cache: 'no-store'}).then(r => r.ok ? r.json() : null).then(h => { PROTO = !(h && h.ops && h.ops.enabled); }).catch(() => { PROTO = true; });
  const hashIs = h => (location.hash.slice(1) || 'overview').split('/')[0] === h;
  const clone = o => JSON.parse(JSON.stringify(o));

  function vars() {
    const u = PROTO ? {name: 'demo', display: '演示用户'} : (OP.me && OP.me.user) || {}, d = new Date();
    return {user: u.name || '', display: u.display || u.name || '', env: PROTO ? '演示' : ENV[OP.env] || OP.env || '',
      date: `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`, time: `${p2(d.getHours())}:${p2(d.getMinutes())}`};
  }
  const expand = (t, v) => String(t || '').replace(/\{(user|display|env|date|time)\}/g, (_, k) => v[k]);

  /* 把一小块（旋转后的文字 + 间距）画成 canvas，返回 {url, w, h}，作为背景平铺 */
  function tile(c, text) {
    const S = 2, cv = document.createElement('canvas'), g = cv.getContext('2d');
    const font = `${c.fontSize}px "Microsoft YaHei", "PingFang SC", sans-serif`;
    g.font = font;
    const tw = Math.ceil(g.measureText(text).width), th = Math.ceil(c.fontSize * 1.3), r = c.angle * Math.PI / 180;
    const bw = Math.abs(tw * Math.cos(r)) + Math.abs(th * Math.sin(r)), bh = Math.abs(tw * Math.sin(r)) + Math.abs(th * Math.cos(r));
    const W = Math.ceil(bw + c.gapX), H = Math.ceil(bh + c.gapY);
    cv.width = W * S; cv.height = H * S;
    g.scale(S, S); g.font = font; g.fillStyle = c.color; g.textAlign = 'center'; g.textBaseline = 'middle';
    g.translate(W / 2, H / 2); g.rotate(r); g.fillText(text, 0, 0);
    return {url: cv.toDataURL('image/png'), w: W, h: H};
  }

  function paintStyle(el, c, text) {
    const t = tile(c, text);
    el.style.cssText = `position:fixed;left:0;top:0;right:0;bottom:0;width:100%;height:100%;z-index:2147483000;pointer-events:none;user-select:none;` +
      `opacity:${c.opacity};background-image:url(${t.url});background-size:${t.w}px ${t.h}px;background-repeat:repeat;display:block;visibility:visible`;
    el.dataset.sig = el.style.cssText;
  }

  function removeOverlay() { const el = document.getElementById(OVERLAY_ID); if (el) el.remove(); }

  function applyOverlay() {
    const c = WM.cfg;
    if (!active() || !c || !c.enabled || !c.text) { removeOverlay(); return; }
    let el = document.getElementById(OVERLAY_ID);
    if (!el) { el = document.createElement('div'); el.id = OVERLAY_ID; el.setAttribute('aria-hidden', 'true'); document.body.appendChild(el); }
    paintStyle(el, c, expand(c.text, vars()));
    WM.minute = vars().date + vars().time;
  }

  /* 覆盖层被删除 / 改样式 / 挪走时恢复 */
  let guardTimer = 0;
  new MutationObserver(() => {
    if (guardTimer) return;
    guardTimer = setTimeout(() => {
      guardTimer = 0;
      const c = WM.cfg, el = document.getElementById(OVERLAY_ID);
      if (!active() || !c || !c.enabled || !c.text) return;
      if (!el || el.style.cssText !== el.dataset.sig || el.parentNode !== document.body) applyOverlay();
    }, 50);
  }).observe(document.documentElement, {childList: true, subtree: true, attributes: true, attributeFilter: ['style', 'class', 'hidden', 'id']});

  async function load() {
    if (PROTO === null) { setTimeout(load, 300); return; }   // 等探测完再决定走原型还是后端
    if (WM.loading) return; WM.loading = true;
    if (PROTO) {   // 原型：设置在 localStorage，没有权限与后端
      const s = lsGet(), c = Object.assign({}, PROTO_DEFAULTS, s && check(Object.assign({}, PROTO_DEFAULTS, s)) === '' ? s : {});
      WM.loading = false; WM.at = Date.now(); WM.err = '';
      WM.cfg = c; WM.defaults = clone(PROTO_DEFAULTS); WM.limits = PROTO_LIMITS; WM.canManage = true; WM.user = me();
      applyOverlay(); navSync();
      if (hashIs('watermark')) { if (!WM.form) WM.form = clone(WM.cfg); paint(); }
      return;
    }
    const r = await opApi('GET', '/api/watermark');
    WM.loading = false; WM.at = Date.now();
    if (r.code !== 200) { WM.err = (r.data && r.data.error) || '读取水印设置失败'; if (hashIs('watermark')) paint(); return; }
    WM.err = '';
    WM.cfg = r.data.watermark; WM.defaults = r.data.defaults; WM.limits = r.data.limits || {}; WM.canManage = !!r.data.canManage; WM.user = me();
    applyOverlay(); navSync();
    if (hashIs('watermark')) { if (!WM.form) WM.form = clone(WM.cfg); paint(); }
  }

  /* ---------------- 侧栏菜单（在「集群接入」之后注入，管理员可见） ---------------- */
  function navSync() {
    const nav = document.querySelector('.nav'); if (!nav) return;
    let a = nav.querySelector('a[data-go="watermark"]');
    if (!a) {
      a = document.createElement('a'); a.dataset.go = 'watermark'; a.href = '#watermark'; a.hidden = true;
      a.innerHTML = '<span class="ico">◩</span>水印设置';
      const anchor = nav.querySelector('a[data-go="clusters"]') || nav.querySelector('a[data-go="roles"]');
      if (anchor) anchor.after(a); else nav.appendChild(a);
    }
    a.hidden = !(active() && WM.canManage);
    const grp = nav.querySelector('[data-plat-g]'); if (grp && WM.canManage && active()) grp.hidden = false;
  }

  /* 每 1.5 秒：登录状态变化 / 用户切换 / 超过 60 秒 / 分钟变化（模板里有 {time}）时刷新 */
  setInterval(() => {
    if (PROTO === null) return;   // 还没探测完
    if (!active()) { removeOverlay(); WM.user = ''; WM.cfg = null; WM.form = null; WM.canManage = false; const a = document.querySelector('.nav a[data-go="watermark"]'); if (a) a.hidden = true; return; }
    if (WM.user !== me() || !WM.cfg || Date.now() - WM.at > 60000) { if (WM.user !== me()) { WM.cfg = null; WM.form = null; removeOverlay(); } load(); return; }
    navSync();
    const c = WM.cfg;
    if (c.enabled && /\{time\}/.test(c.text) && WM.minute !== vars().date + vars().time) applyOverlay();
  }, 1500);

  /* ---------------- 设置页 ---------------- */
  function preview() {
    const el = $('#wmPrev'); if (!el || !WM.form) return;
    const f = normForm(); if (!f) return;
    paintStyle(el, f, expand(f.text || '（文字为空）', vars()));
    el.style.position = 'absolute'; el.style.zIndex = '1'; el.style.opacity = Math.max(f.opacity, 0.3); el.style.backgroundPosition = 'center';   // 预览放大一点透明度，且居中，长文字也能看到
  }

  function normForm() {
    const f = WM.form; if (!f) return null;
    const n = (v, d) => { const x = Number(v); return isFinite(x) ? x : d; };
    return {enabled: !!f.enabled, text: String(f.text || ''), opacity: n(f.opacity, 0.12), fontSize: Math.round(n(f.fontSize, 16)), angle: Math.round(n(f.angle, -20)),
      color: /^#[0-9a-fA-F]{6}$/.test(f.color) ? f.color : '#808080', gapX: Math.round(n(f.gapX, 120)), gapY: Math.round(n(f.gapY, 100))};
  }

  const dirty = () => !!(WM.cfg && WM.form && JSON.stringify(normForm()) !== JSON.stringify({enabled: WM.cfg.enabled, text: WM.cfg.text, opacity: WM.cfg.opacity, fontSize: WM.cfg.fontSize,
    angle: WM.cfg.angle, color: WM.cfg.color, gapX: WM.cfg.gapX, gapY: WM.cfg.gapY}));

  function paint() {
    const box = $('#wmPage'); if (!box) return;
    if (!PROTO && !OP.on) { box.innerHTML = '<div class="card"><h2>水印设置</h2><div class="notice info"><span>ℹ</span><div>水印设置只在运维模式（后端以 <code>--data-dir</code> 启动）下可用。</div></div></div>'; return; }
    if (WM.err) { box.innerHTML = `<div class="card"><h2>水印设置</h2><div class="notice bad"><span>✖</span><div>${E(WM.err)} <button class="btn sm" data-na="wm-reload">重试</button></div></div></div>`; return; }
    if (!WM.cfg) { box.innerHTML = '<div class="card"><h2>水印设置</h2><div class="small muted">加载中…</div></div>'; return; }
    if (!WM.canManage) { box.innerHTML = '<div class="card"><h2>水印设置</h2><div class="notice bad"><span>✖</span><div>没有权限：修改水印需要「修改平台设置」（settings.edit）能力，只有内置管理员具备。</div></div></div>'; return; }
    if (!WM.form) WM.form = clone(WM.cfg);
    const f = WM.form, c = WM.cfg, vs = (WM.limits.vars || []).map(v => `<code>${E(v)}</code>`).join(' ');
    const by = PROTO ? '原型模式：设置只保存在本浏览器（localStorage），不会发送到后端，也不会影响其他人；示例账号为「演示用户」。'
      : c.updatedBy ? `最近修改：${E(c.updatedBy)} · ${E(String(c.updatedAt || '').replace('T', ' ').slice(0, 19))}` : '尚未配置过（当前为默认值，水印关闭）';
    const inp = (k, type, extra = '') => `<input type="${type}" data-ni="wm-f" data-k="${k}" value="${E(f[k])}" ${extra}>`;
    box.innerHTML = `<div class="card"><h2>水印设置 <span class="sub">登录后全站叠加 · 对所有用户生效</span></h2>
      <div class="notice info"><span>ℹ</span><div>水印把「谁在看」铺满整个页面，用于截屏 / 拍照外泄后的溯源。它只是前端震慑手段，不是安全边界。保存不走审批，会写审计（<code>watermark.update</code>）。<br><span class="muted small">${by}</span></div></div>
      <div class="row" style="align-items:flex-start;gap:24px;flex-wrap:wrap">
        <div style="flex:1 1 360px;min-width:300px">
          <div class="field"><label><input type="checkbox" data-nc="wm-f" data-k="enabled" ${f.enabled ? 'checked' : ''}> 启用水印</label></div>
          <div class="field"><label>水印文字（≤ ${E(WM.limits.maxText || 100)} 字）</label>${inp('text', 'text', `maxlength="${E(WM.limits.maxText || 100)}" placeholder="{display} {user} {date}"`)}
            <div class="small muted" style="margin-top:4px">可用变量：${vs}；其余文字原样显示。例：<code>内部资料 {display} {date} {time}</code></div></div>
          <div class="field"><label>透明度 <b id="wmOpV">${E(f.opacity)}</b></label>${inp('opacity', 'range', 'min="0.02" max="0.5" step="0.01"')}</div>
          <div class="field"><label>字号（px，10–48）</label>${inp('fontSize', 'number', 'min="10" max="48" step="1"')}</div>
          <div class="field"><label>旋转角度（度，-90–90；负数为逆时针）<b id="wmAnV">${E(f.angle)}</b></label>${inp('angle', 'range', 'min="-90" max="90" step="1"')}</div>
          <div class="field"><label>颜色</label>${inp('color', 'color')}</div>
          <div class="field"><label>水平间距（px，40–400）</label>${inp('gapX', 'number', 'min="40" max="400" step="10"')}</div>
          <div class="field"><label>垂直间距（px，40–400）</label>${inp('gapY', 'number', 'min="40" max="400" step="10"')}</div>
          <div id="wmErr" class="notice bad" style="display:none"></div>
          <div class="row" style="gap:8px"><button class="btn primary" data-na="wm-save" ${WM.saving ? 'disabled' : ''}>${WM.saving ? '保存中…' : '保存并应用'}</button>
            <button class="btn" data-na="wm-default">恢复默认值</button><button class="btn" data-na="wm-revert">放弃修改</button></div>
        </div>
        <div style="flex:1 1 320px;min-width:280px"><div class="small muted" style="margin-bottom:6px">实时预览（示例数据用你当前的账号）</div>
          <div style="position:relative;height:300px;border:1px solid var(--line,#ddd);border-radius:8px;overflow:hidden;background:var(--card,#fff)"><div id="wmPrev"></div></div></div>
      </div></div>`;
    preview();
  }

  async function save() {
    const f = normForm(); if (!f || WM.saving) return;
    const err = $('#wmErr'); if (err) err.style.display = 'none';
    if (PROTO) {   // 原型：本地校验 + 存浏览器，不走后端
      const bad = check(f); f.text = f.text.trim(); f.color = f.color.toLowerCase();
      if (bad) { const e2 = $('#wmErr'); if (e2) { e2.textContent = bad; e2.style.display = ''; } return; }
      if (!lsSet(f)) { const e2 = $('#wmErr'); if (e2) { e2.textContent = '保存失败：浏览器禁用了本地存储'; e2.style.display = ''; } return; }
      WM.cfg = f; WM.form = clone(f); WM.at = Date.now(); applyOverlay(); paint(); toast('水印设置已保存（仅本浏览器）');
      return;
    }
    WM.saving = true; paint();
    const r = await opApi('POST', '/api/watermark', f);
    WM.saving = false;
    if (r.code === 200) { WM.cfg = r.data.watermark; WM.form = clone(WM.cfg); WM.at = Date.now(); applyOverlay(); paint(); toast('水印设置已保存'); return; }
    paint();
    const e2 = $('#wmErr'); if (e2) { e2.textContent = (r.data && r.data.error) || ('保存失败（' + r.code + '）'); e2.style.display = ''; }
  }

  pages.watermark = {
    title: '水印设置',
    render() { return '<div id="wmPage"></div>'; },
    init() { if (!WM.cfg) { paint(); load(); } else { WM.form = clone(WM.cfg); paint(); } },
  };

  const CLICK = {
    'wm-save': () => save(),
    'wm-default': () => { if (WM.defaults) { WM.form = clone(WM.defaults); paint(); } },
    'wm-revert': () => { if (WM.cfg) { WM.form = clone(WM.cfg); paint(); } },
    'wm-reload': () => { WM.err = ''; load(); },
  };
  document.addEventListener('click', e => { const el = e.target.closest('[data-na^="wm-"]'); if (!el || el.disabled) return; const f = CLICK[el.dataset.na]; if (f) f(); });
  document.addEventListener('change', e => {
    const el = e.target.closest('[data-nc="wm-f"]'); if (!el || !WM.form) return;
    WM.form[el.dataset.k] = el.checked;
  });
  document.addEventListener('input', e => {
    const el = e.target.closest('[data-ni="wm-f"]'); if (!el || !WM.form) return;
    WM.form[el.dataset.k] = el.value;
    const op = $('#wmOpV'), an = $('#wmAnV'); if (op) op.textContent = WM.form.opacity; if (an) an.textContent = WM.form.angle;
    preview();
  });
  window.addEventListener('beforeunload', ev => { if (hashIs('watermark') && dirty()) { ev.preventDefault(); ev.returnValue = ''; } });
})();
