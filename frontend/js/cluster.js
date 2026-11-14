// @author: kevinkelly
// @license: Apache License 2.0
// @file: cluster.js
// @time: 2026/10/28

/* 多集群：当前集群状态（window.CL）、请求带集群、顶栏集群切换。
   必须在 data.js 之前加载（包装 window.fetch，之后所有脚本发出的同源 /api/ 请求都自动带上 X-Cluster 请求头）。
   - window.CL = {cur, list, primary, canManage, full}：cur 为当前集群 id；list 来自 /api/auth/me 的 clusters（可见集群）。
   - 请求头：只有选定了集群、且不是「唯一的主集群」时才加 X-Cluster（保持单集群时的旧行为）；调用方代码不需要改。
     无法带头的链接（<a href> 导出 / 下载）用 CL.url(path) 追加 ?cluster=<id>。
   - 切换集群 = 写入 localStorage.opCluster → location.reload()：快照、预测、配置当前值、洞察、变更 / 维护、轮询等缓存全部重置。
   - 任一集群端点返回 404 + code=cluster_not_found（集群被删除 / 权限变化）：清掉保存的选择，提示后刷新，回到默认集群。
   - 顶栏 <select id="clSel">：运维模式下由本文件按 me.clusters 渲染；非运维模式（原型 / 回放静态页）保持原样。
   事件：data-nc="cl-sel"（文档级委托，CSP 禁止内联事件）。依赖（运行时才用）：ops.js（OP / opApi / opRenderChip）、app.js（toast）。 */
(function () {
  'use strict';
  const LSK = 'opCluster', NOTICE = 'opClusterNotice', LOSTN = 'opClusterLostN';
  const store = st => ({
    get(k) { try { return st.getItem(k) || ''; } catch (e) { return ''; } },
    set(k, v) { try { if (v) st.setItem(k, v); else st.removeItem(k); } catch (e) { /* 浏览器禁用存储：忽略 */ } }
  });
  const ls = store(window.localStorage || {getItem() { return ''; }, setItem() {}, removeItem() {}});
  const ss = store(window.sessionStorage || {getItem() { return ''; }, setItem() {}, removeItem() {}});
  const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
  const ENV = {prod: ['bad', '生产'], staging: ['warn', '预发'], test: ['ok', '测试']};
  const envName = e => (ENV[e] || ['', e || '—'])[1];

  const CL = window.CL = {cur: '', list: [], primary: '', canManage: false, full: null, bootCur: null, _manageKey: '', _syncing: false, _sig: ''};
  CL.envName = envName;
  CL.ENV = ENV;
  CL.info = id => CL.list.find(c => c.id === (id || CL.cur)) || null;
  CL.name = () => { const c = CL.info(); return c ? (c.display || c.id) : ''; };
  CL.active = () => !!(typeof OP !== 'undefined' && OP.on && CL.list.length);   // 多集群模式（运维模式且后端返回了可见集群）
  /* 独立部署（/api/health 的 standalone）：平台本身不代表任何集群；一个集群都没接入（或当前账号一个都看不到）时 CL.list 为空 */
  CL.standalone = false;
  CL.empty = () => typeof OP !== 'undefined' && OP.on && !!OP.me && CL.list.length === 0;
  const NEEDS_CLUSTER = ['overview', 'arch', 'diag', 'sql', 'hotspot', 'nodes', 'schedule', 'ops', 'settings'];   // 展示 / 操作某个集群数据的页面
  CL.gated = name => CL.empty() && NEEDS_CLUSTER.indexOf(name) >= 0;
  /* 没有可用集群时，集群数据页换成引导：独立部署 → 去「集群接入」；否则说明当前账号看不到任何集群 */
  CL.gate = name => {
    if (!CL.gated(name)) return '';
    const manage = CL.canManage;
    if (CL.standalone && CL.localTiup) {
      return `<div class="card"><h2>尚未导入集群</h2>
        <div class="notice info"><span>ℹ</span><div>平台部署在 <b>tiup 中控机</b>上，本身不代表任何 TiDB 集群。${manage ? '请先把本机 tiup 管理的集群导入进来，导入后这里才会有数据。' : '需要平台管理员先导入集群，导入后这里才会有数据。'}</div></div>
        ${manage ? `<div class="row" style="margin:12px 0"><button class="btn primary" data-go="clusters">去导入集群 →</button></div>
        <ol class="small" style="padding-left:20px;line-height:1.9;margin:0">
          <li>打开「平台配置 → 集群接入」，点「从本机 tiup 导入」。</li>
          <li>勾选要纳管的集群，选择它们的环境（测试 / 预发 / 生产），可选填一个只读 SQL 账号，点「导入所选」。</li>
          <li>导入的集群可做<b>全部运维操作</b>（写操作仍需两名管理员开启写开关、高风险变更仍需审批）；手工添加的远程集群只读。</li>
        </ol>` : ''}</div>`;
    }
    if (CL.standalone) {
      return `<div class="card"><h2>尚未接入集群</h2>
        <div class="notice info"><span>ℹ</span><div>这个平台是<b>独立部署</b>的，本身不代表任何 TiDB 集群。${manage ? '请先接入一个已有集群，接入后这里才会有数据。' : '需要平台管理员先接入集群，接入后这里才会有数据。'}</div></div>
        ${manage ? `<div class="row" style="margin:12px 0"><button class="btn primary" data-go="clusters">去接入集群 →</button></div>
        <ol class="small" style="padding-left:20px;line-height:1.9;margin:0">
          <li>打开「平台配置 → 集群接入」，点「＋ 接入集群」。</li>
          <li>填写 <b>PD 地址</b>（必填，例如 <code>http://10.0.1.21:2379</code>）；建议同时填写 Prometheus、Alertmanager、TiDB status 和一个只读 SQL 账号。</li>
          <li>点「测试」确认各组件地址都通，然后回到「集群总览」查看数据。</li>
        </ol>
        <div class="small muted" style="margin-top:10px">首期为只读接入：总览、拓扑、慢 SQL、热点、告警和诊断可用；扩缩容、配置变更、重启等写操作需要在集群的中控机上执行。</div>` : ''}</div>`;
    }
    return `<div class="card"><h2>没有可查看的集群</h2><div class="notice warn"><span>🔒</span><div>你的账号在平台已接入的所有集群里都没有查看权限。请联系平台管理员为你在相应环境里授予角色。</div></div></div>`;
  };
  /* 顶栏集群下拉：没有集群时给一个明确的占位，而不是原型里写死的示例集群 */
  CL.renderEmpty = () => {
    const sel = document.getElementById('clSel');
    if (!sel) return;
    sel.innerHTML = '<option>（尚未接入集群）</option>'; sel.disabled = true; sel.className = 'oc-sel'; sel.title = '平台还没有接入任何你可见的集群';
    const tag = document.getElementById('clEnv'); if (tag) { tag.hidden = true; tag.textContent = ''; }
  };
  /* 是否给请求加 X-Cluster：已选定集群，且不是「只有一个集群且它就是主集群」 */
  CL.send = () => !!CL.cur && CL.list.length > 0 && !(CL.list.length === 1 && CL.list[0].primary);
  /* 给无法带请求头的链接（<a href> / window.open / location）追加 ?cluster=<id> */
  CL.url = path => {
    if (!CL.send() || !/^\/api\//.test(path) || /[?&]cluster=/.test(path)) return path;
    return path + (path.indexOf('?') >= 0 ? '&' : '?') + 'cluster=' + encodeURIComponent(CL.cur);
  };

  /* ---------------- fetch 包装 ---------------- */
  const _fetch = window.fetch.bind(window);
  window.fetch = function (input, init) {
    let url = '';
    try { url = typeof input === 'string' ? input : (input && input.url) || String(input); } catch (e) { /* 忽略 */ }
    let api = false;
    try { const u = new URL(url, location.href); api = u.origin === location.origin && u.pathname.indexOf('/api/') === 0 && !/(^|&)cluster=/.test(u.search.slice(1)); } catch (e) { /* 忽略 */ }
    if (!api || !CL.send()) return _fetch(input, init);
    const sent = CL.cur;
    let h;
    try { h = new Headers((init && init.headers) || (typeof Request !== 'undefined' && input instanceof Request ? input.headers : undefined)); } catch (e) { return _fetch(input, init); }
    if (!h.has('X-Cluster')) h.set('X-Cluster', sent);
    return _fetch(input, Object.assign({}, init, {headers: h})).then(r => {
      if (r.status === 404 && sent === CL.cur) {
        r.clone().json().then(j => { if (j && j.code === 'cluster_not_found' && sent === CL.cur) CL.lost(); }, () => {});
      }
      return r;
    });
  };

  /* ---------------- 集群不可用 / 切换 ---------------- */
  CL.lost = function () {
    if (CL._lost || CL._holdLost) return;   // _holdLost：集群接入页正在删除当前集群，先让用户看完结果再切换
    const n = parseInt(ss.get(LOSTN) || '0', 10) || 0;
    if (n >= 3) return;   // 防止异常情况下无限刷新
    CL._lost = true; ss.set(LOSTN, String(n + 1));
    ls.set(LSK, ''); ss.set(NOTICE, '1');
    location.reload();
  };
  /* 正在使用的集群被（本人）删除：立即改用默认集群，页面其余数据在下次切换页面时随 reload 重置 */
  CL.leave = function (goneId) {
    if (CL.cur !== goneId) return false;
    ls.set(LSK, '');
    CL.list = CL.list.filter(c => c.id !== goneId);
    CL.cur = (CL.list.find(c => c.primary) || CL.list[0] || {}).id || '';
    CL._sig = ''; CL.renderTop(true);
    window.addEventListener('hashchange', () => { CL._lost = true; location.reload(); }, {once: true});
    return true;
  };
  CL.switchTo = function (id, noConfirm) {
    if (!CL.info(id) || id === CL.cur) { CL.renderTop(true); return; }
    const dr = document.getElementById('drawer');
    const dirty = (dr && dr.classList.contains('open')) || document.querySelector('.or-dirty');
    if (dirty && !noConfirm &&!window.confirm(`当前有打开的抽屉或未保存的内容，切换到集群「${(CL.info(id).display || id)}」会刷新页面并丢弃它们。确定切换吗？`)) { CL.renderTop(true); return; }
    ls.set(LSK, id); CL._lost = true; ss.set(LOSTN, '');
    location.reload();
  };

  /* ---------------- 从 /api/auth/me 初始化 / 同步 ---------------- */
  CL.apply = function (me) {
    const list = me && Array.isArray(me.clusters) ? me.clusters : [];
    CL.list = list; CL.primary = (list.find(c => c.primary) || {}).id || '';
    if (!list.length) {   // 没有可见集群（独立部署尚未接入，或账号看不到任何集群）：仍要读取「能否管理集群」，引导页据此显示接入按钮
      CL.cur = '';
      const k0 = (me.user && me.user.name || '') + '|' + JSON.stringify(me.roles || []) + JSON.stringify(me.caps || {});
      if (k0 !== CL._manageKey) { CL._manageKey = k0; CL.loadManage(); }
      CL.renderEmpty(); return;
    }
    let cur = CL.cur;
    if (!cur) {
      const saved = ls.get(LSK);
      if (saved && !list.some(c => c.id === saved)) ls.set(LSK, '');   // 保存的集群已不可见：退回 me.cluster
      cur = list.some(c => c.id === saved) ? saved : ((me.cluster && me.cluster.id) || list[0].id);
      // 通知里的「查看」链接形如 /?cluster=<id>#schedule：URL 指定的集群（且当前用户可见）优先，并从地址栏去掉该参数
      try {
        const q = new URLSearchParams(location.search), want = q.get('cluster');
        if (want) {
          if (list.some(c => c.id === want)) { cur = want; ls.set(LSK, want); }
          q.delete('cluster');
          history.replaceState(null, '', location.pathname + (q.toString() ? '?' + q.toString() : '') + location.hash);
        }
      } catch (e) { /* 忽略 */ }
      if (ss.get(NOTICE)) { ss.set(NOTICE, ''); ss.set(LOSTN, ''); setTimeout(() => { if (typeof toast === 'function') toast('当前集群已不可用，已切换到默认集群'); }, 700); }
    } else if (!list.some(c => c.id === cur)) { CL.lost(); return; }
    CL.cur = cur;
    // 是否需要重新读取「该用户在选定集群下」的权限：由 CL.sync 处理
    const key = (me.user && me.user.name || '') + '|' + JSON.stringify(me.roles || []) + JSON.stringify(me.caps || {});
    if (key !== CL._manageKey) { CL._manageKey = key; CL.loadManage(); }
    CL.renderTop();
  };
  /* 初次 /api/auth/me 不带头，返回的是默认集群；如果选中的是别的集群，带头重读一次，让 env / roles / caps / write 对齐 */
  CL.sync = async function () {
    if (CL._syncing || typeof OP === 'undefined' || !OP.me) return;
    if (!CL.cur || !OP.me.cluster || OP.me.cluster.id === CL.cur) { if (CL.bootCur === null && CL.cur) CL.bootCur = CL.cur; return; }
    CL._syncing = true;
    try {
      const r = await opApi('GET', '/api/auth/me');
      if (r.code === 200 && r.data.authenticated) opApplyMe(r.data);
    } finally { CL._syncing = false; }
    if (CL.bootCur === null) CL.bootCur = CL.cur;
  };
  /* 登录后换了用户 / 集群可见范围变化：页面上已加载的数据属于上一个集群，直接刷新 */
  CL.reloadIfChanged = function () { if (CL.bootCur !== null && CL.cur && CL.cur !== CL.bootCur) { CL._lost = true; location.reload(); return true; } return false; };
  CL.loadManage = async function () {
    if (typeof opApi !== 'function') return;
    const r = await opApi('GET', '/api/clusters');
    const can = r.code === 200 && !!r.data.canManage;
    CL.full = r.code === 200 ? r.data : null;
    if (can !== CL.canManage) {
      CL.canManage = can; if (typeof opRenderChip === 'function') opRenderChip();
      // 引导页的内容取决于能否管理集群（管理员看到「去接入集群」按钮）：权限读到后重绘当前页
      const page = (location.hash.slice(1) || 'overview').split('/')[0];
      if (CL.gated(page) && typeof render === 'function') render(page);
    }
  };

  /* ---------------- 顶栏 ---------------- */
  CL.renderTop = function (force) {
    const sel = document.getElementById('clSel');
    if (!sel || typeof OP === 'undefined' || !OP.on || !OP.me || !CL.list.length) return;
    const sig = JSON.stringify([CL.cur, CL.list.map(c => [c.id, c.display, c.env, c.state])]);
    const cur = CL.info();
    if (force || sig !== CL._sig) {
      CL._sig = sig;
      sel.innerHTML = CL.list.map(c => `<option value="${esc(c.id)}"${c.id === CL.cur ? ' selected' : ''}>${esc(c.display || c.id)}（${esc(envName(c.env))}）${c.state === 'error' ? ' · 异常' : ''}</option>`).join('');
      sel.value = CL.cur;
    }
    sel.disabled = CL.list.length < 2;
    sel.title = CL.list.length < 2 ? '当前账号只能看到这一个集群' : '切换集群（会刷新页面）';
    sel.setAttribute('aria-label', '选择集群');
    sel.className = 'oc-sel' + (cur ? ' oc-env-' + cur.env : '');
    let tag = document.getElementById('clEnv');
    if (!tag) { tag = document.createElement('span'); tag.id = 'clEnv'; sel.insertAdjacentElement('afterend', tag); }
    if (cur && cur.env === 'prod') { tag.className = 'oc-envtag'; tag.textContent = '生产环境'; tag.title = '当前集群是生产环境：操作需更严格审批，请谨慎'; tag.hidden = false; }
    else { tag.hidden = true; tag.textContent = ''; }
  };

  document.addEventListener('change', e => {
    const el = e.target.closest && e.target.closest('[data-nc="cl-sel"]'); if (!el) return;
    CL.switchTo(el.value);
  });
})();
