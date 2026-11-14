// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_clusters.js
// @time: 2026/10/28

/* 平台配置 → 「集群接入」页（pages.clusters）：管理平台纳管的 TiDB 集群（多集群）。
     - 列表：集群（id / 显示名 / 描述）、环境、模式（live / replay）、状态（版本 / 节点数 / 上次快照 / 可达性）、写开关、操作；
     - 操作：切换到该集群（= 顶栏切换，刷新页面）、测试（只读探测）、编辑、删除（高危确认，需输入集群 id）；主集群标「主」，不可删除，只能改显示名 / 描述；
     - 「＋ 接入集群」抽屉：live（PD / TiDB status / Prometheus / Alertmanager / MySQL 连接 / 可执行文件路径）或 replay（fixtures 目录）；
       提交后展示后端返回的 warnings（如 sudo 策略文件需允许该 tiup 集群名）；错误按 code 友好展示。
   接口：GET /api/clusters；POST /api/clusters、/api/clusters/{id}、/api/clusters/{id}/delete、/api/clusters/{id}/test（均需 settings.edit）。
   非运维模式只显示说明卡片；非管理员（canManage=false）不显示菜单，直接打开地址时显示无权限说明。
   data-na / data-nc / data-ni 前缀：oc-。依赖 cluster.js（CL）、ops.js（OP / opApi / opEsc / OP_HOOKS / opRefresh）、app.js（openDrawer / closeDrawer / toast）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); if (isNaN(d) || d.getFullYear() < 2000) return '—'; return `${d.getMonth() + 1}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const tg = (t, c = '', title) => `<span class="tag ${c}"${title ? ` title="${E(title)}"` : ''}>${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview').split('/')[0] === h;
  const ENVS = [['test', '测试环境'], ['staging', '预发环境'], ['prod', '生产环境']];
  const ENV_TAG = {test: ['ok', '测试'], staging: ['warn', '预发'], prod: ['bad', '生产']};
  const envTag = e => { const t = ENV_TAG[e] || ['', e || '—']; return tg(t[1], t[0]); };
  const ERR = {
    invalid: '参数不合法', exists: '集群 id 已存在', not_found: '集群不存在（可能已被删除）', limit: '集群数量已达上限',
    primary: '主集群的连接配置来自平台启动参数：只能修改显示名与描述，且不能删除', in_use: '该集群有执行中或待审批的变更，请先处理完再操作',
    store: '保存失败（注册表写入失败，请检查数据目录的权限与磁盘空间）', start: '集群实例启动失败（连接信息可能有误，或集群暂时不可达）'
  };
  const errText = r => { const c = r.data && r.data.code, m = (r.data && r.data.error) || (r.code === 0 ? '无法连接后端' : '请求失败（' + r.code + '）'); return (ERR[c] && !m.includes(ERR[c]) ? ERR[c] + '：' : '') + m; };

  const OC = {list: [], canManage: false, standalone: false, localTiup: false, imp: null, limits: {}, roots: [], primary: '', loaded: false, loading: false, err: '', errCode: 0, at: 0, sig: '', form: null, flash: null, del: null, tok: 0};
  const max = () => OC.limits.max || 20;
  const idRe = () => { try { return new RegExp(OC.limits.idPattern || '^[a-z][a-z0-9-]{1,31}$'); } catch (e) { return /^[a-z][a-z0-9-]{1,31}$/; } };
  const find = id => OC.list.find(c => c.id === id);

  async function load() {
    if (OC.loading) return; OC.loading = true;
    const r = await opApi('GET', '/api/clusters');
    OC.loading = false; OC.at = Date.now();
    if (r.code === 200) {
      const sig = JSON.stringify(r.data);
      OC.list = r.data.clusters || []; OC.canManage = !!r.data.canManage; OC.limits = r.data.limits || {}; OC.roots = r.data.fixturesRoots || []; OC.primary = r.data.primary || '';
      OC.standalone = !!r.data.standalone;   // 独立部署：平台本身不代表任何集群；手工添加的集群都是远程接入（只读）
      OC.localTiup = !!r.data.localTiup;     // 中控机模式：本机是 tiup 中控机，可「从本机 tiup 导入」集群并做全部运维
      OC.err = ''; OC.errCode = 0;
      if (window.CL && CL.canManage !== OC.canManage) { CL.canManage = OC.canManage; if (typeof opRenderChip === 'function') opRenderChip(); }
      if (sig !== OC.sig || !OC.loaded) { OC.sig = sig; OC.loaded = true; paint(); } else OC.loaded = true;
    } else { OC.err = (r.data && r.data.error) || ('读取失败（' + r.code + '）'); OC.errCode = r.code; OC.loaded = true; OC.sig = ''; paint(); }
  }
  function paint() { if (OP.on && hashIs('clusters') && $('#ocBody')) pages.clusters.body(); }
  const setFlash = (lv, text) => { OC.flash = {lv, text}; paint(); };
  const icon = {ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'};

  /* ---------------- 列表 ---------------- */
  function statusCell(c) {
    const st = c.status || {}, s = c.state || st.state;
    const head = s === 'ok' ? tg('正常', 'ok') : s === 'error' ? tg('异常', 'bad') : tg('加载中', 'info');
    const reach = st.reachable === true ? tg('可达', 'ok') : st.reachable === false ? tg('不可达', 'bad', '最近一次采集时连不上集群组件') : '';
    const lines = [];
    if (st.version || st.nodes) lines.push(`${E(st.version || '版本未知')} · ${st.nodes != null ? E(st.nodes) + ' 个节点' : '节点数未知'}`);
    lines.push(`上次快照 ${E(fDT(st.snapshotAt))}${st.errors ? ` · <span style="color:var(--warn)">${E(st.errors)} 项数据缺失</span>` : ''}`);
    if (st.error) lines.push(`<span style="color:var(--bad)">${E(st.error)}</span>`);
    (c.warnings || []).forEach(w => lines.push(`<span style="color:var(--warn)">⚠ ${E(w)}</span>`));
    return `<div>${head} ${reach}</div><div class="small muted oc-lines">${lines.map(l => `<div>${l}</div>`).join('')}</div>`;
  }
  function writeCell(c) {
    const w = c.write || {};
    if (w.readOnlyForced) return tg('强制只读', 'bad', w.reason || 'READONLY 熔断文件存在');
    if (w.enabled && !w.reason) return tg('已开启', 'ok');
    if (w.pending) return tg('待确认', 'info', '已有管理员提议开启，等待另一名管理员确认');
    return tg('只读', 'warn', w.reason || '写操作开关未开启');
  }
  /* 接入方式：独立部署下才有区别——本机 tiup 导入（可运维）与手工添加的远程集群（只读） */
  function accessTag(c) {
    if (!OC.standalone || c.mode === 'replay') return '';
    return '<div style="margin-top:4px">' + (c.local ? tg('本机 tiup · 可运维', 'ok', '从本机 tiup 导入：变更 / 巡检经本机执行器完成（写操作仍需写开关与审批）')
      : tg('远程接入 · 只读', 'warn', '手工添加：平台只通过网络地址读取，不碰它的 tiup / SSH，恒为只读')) + '</div>';
  }
  function rowHtml(c) {
    const cur = window.CL && CL.cur === c.id, id = E(c.id);
    return `<tr class="${cur ? 'oc-cur' : ''}"><td><b>${E(c.display || c.id)}</b>${c.primary ? ' ' + tg('主', 'info', '主集群：来自平台启动参数') : ''}${cur ? ' ' + tg('当前', 'ok') : ''}
        <div class="mono small muted">${id}</div>${c.desc ? `<div class="small muted oc-desc">${E(c.desc)}</div>` : ''}</td>
      <td>${envTag(c.env)}</td><td>${c.mode === 'replay' ? tg('replay 回放', '', '读取 fixtures 数据包，不连接真实集群') : tg('live 实时', 'info')}${accessTag(c)}</td>
      <td class="small">${statusCell(c)}</td><td>${writeCell(c)}</td>
      <td><div class="acts"><button class="btn sm primary" data-na="oc-switch" data-id="${id}" ${cur ? 'disabled title="当前正在使用该集群"' : ''}>切换到该集群</button>
        <button class="btn sm" data-na="oc-test" data-id="${id}">测试</button>
        <button class="btn sm" data-na="oc-edit" data-id="${id}">编辑</button>
        <button class="btn sm danger" data-na="oc-del" data-id="${id}" ${c.primary ? 'disabled title="主集群来自平台启动参数，不能删除"' : ''}>删除</button></div></td></tr>`;
  }
  const NOTE_OFF = '<div class="card"><h2>集群接入</h2><div class="notice info"><span>ℹ</span><div>多集群接入需要运维模式（启动参数 <code>--data-dir</code>）。当前是原型 / 只读演示模式，只有一个集群，没有集群注册表。</div></div><div class="small muted">运维模式下，管理员可以在这里接入、编辑、测试和移除纳管的 TiDB 集群；顶栏的集群下拉用于在集群之间切换。</div></div>';
  const NOTE_DENY = '<div class="card"><h2>集群接入</h2><div class="notice warn"><span>🔒</span><div>只有集群管理员（拥有 settings.edit 能力）可以管理集群接入。你可以用顶栏的集群下拉在自己可见的集群之间切换。</div></div></div>';

  function flashHtml() {
    const f = OC.flash; if (!f) return '';
    return `<div class="notice ${f.lv}" style="margin-bottom:10px"><span>${icon[f.lv] || 'ℹ'}</span><div style="flex:1">${E(f.text)}</div><button class="btn sm" data-na="oc-flash-x">知道了</button></div>`;
  }
  function bodyHtml() {
    if (!OC.loaded) return '<div class="card"><h2>集群接入</h2><div class="empty">正在读取…</div></div>';
    if (OC.errCode === 403 || (OC.loaded && !OC.err && !OC.canManage)) return NOTE_DENY;
    const full = OC.list.length >= max();
    return `<div class="card"><h2>集群接入 <span class="sub">${OC.list.length} / ${max()} 个集群</span>
        <span class="right"><button class="btn sm" data-na="oc-reload">刷新</button>${OC.localTiup ? `<button class="btn primary sm" data-na="oc-import" ${full ? 'disabled' : ''}>从本机 tiup 导入</button>` : ''}<button class="btn ${OC.localTiup ? '' : 'primary '}sm" data-na="oc-new" ${full ? `disabled title="已达到上限（${max()} 个）"` : ''}>${OC.localTiup ? '＋ 手工接入远程集群（只读）' : '＋ 接入集群'}</button></span></h2>
      <div class="notice info"><span>ℹ</span><div>${OC.localTiup
        ? '平台部署在 <b>tiup 中控机</b>上。<b>从本机 tiup 导入</b>的集群可做<b>全部运维操作</b>（变更经本机受限执行器完成，tiup 用自己的 SSH 密钥连各节点，密钥不会交给平台；写操作仍需两名管理员开启写开关，高风险变更仍需审批）。<b>手工添加</b>的远程集群只通过 PD / Prometheus / Alertmanager / TiDB SQL 的网络地址读取，<b>恒为只读</b>。'
        : OC.standalone
        ? '平台是<b>独立部署</b>的：它不在任何集群的中控机上，通过 <b>PD / Prometheus / Alertmanager / TiDB SQL</b> 的网络地址<b>只读</b>接入每个集群，总览、拓扑、慢 SQL、热点、告警和诊断可用；扩缩容、配置变更、重启等写操作首期需要在集群的中控机上执行。'
        : '平台通过 <b>PD / Prometheus / Alertmanager / TiDB SQL / tiup</b> 管理每个集群。'}<b>每个集群有自己的</b>变更与审批队列、写操作开关、变更窗口与冻结期、通知渠道；<b>用户、角色、审批策略和审计日志全局共享</b>。
        用户在某个集群里能做什么，由「<b>该集群的环境</b>」（测试 / 预发 / 生产）决定——角色按环境授予，见「<a data-go="roles">角色权限</a>」。</div></div>
      ${flashHtml()}${OC.err ? `<div class="notice bad"><span>✖</span><div>${E(OC.err)}</div></div>` : ''}
      <div class="tbl-wrap"><table class="oc-tbl"><thead><tr><th>集群</th><th>环境</th><th>模式</th><th>状态</th><th>写开关</th><th>操作</th></tr></thead><tbody>
        ${OC.list.map(rowHtml).join('') || `<tr><td colspan="6" class="empty">${OC.localTiup ? '还没有集群：点右上角「从本机 tiup 导入」，把本机 tiup 管理的集群纳管进来。' : OC.standalone ? '还没有接入任何集群：点右上角「＋ 接入集群」，填写 PD 地址（建议同时填 Prometheus / Alertmanager / TiDB status 与只读 SQL 账号）。' : '没有集群'}</td></tr>`}</tbody></table></div>
      <div class="small muted" style="margin-top:10px">删除只是「移除纳管」：不会停止或删除 TiDB 集群，运行数据目录改名保留；有执行中 / 待审批的变更时会被拒绝。「测试」只做只读探测，不会改动集群。最多接入 ${max()} 个集群。</div></div>`;
  }

  pages.clusters = {
    title: '集群接入',
    render() {
      if (!OP.on) return NOTE_OFF;
      if (!OP.me) return NOTE_DENY;
      return '<div id="ocBody"></div>';
    },
    init() {
      if (!OP.on || !OP.me) return;
      if (OC.lastUser !== (OP.me.user && OP.me.user.name)) { OC.lastUser = OP.me.user && OP.me.user.name; Object.assign(OC, {list: [], loaded: false, err: '', sig: '', flash: null}); }
      this.body(); load();
    },
    body() { const el = $('#ocBody'); if (el) el.innerHTML = bodyHtml(); },
  };

  /* ---------------- 接入 / 编辑抽屉 ---------------- */
  const txt = v => (v == null ? '' : String(v));
  function newForm() {
    return {mode: 'add', id: '', name: '', display: '', desc: '', env: 'test', cmode: 'live', fixtures: '', pd: '', tidbStatus: '', prom: '', alert: '', mysqlHost: '', mysqlPort: '', mysqlUser: '', mysqlPassword: '', clearPassword: false, hasPassword: false, mysqlBin: '', tiup: '', home: '', primary: false, err: '', result: null};
  }
  function editForm(c) {
    const k = c.config || {};
    return Object.assign(newForm(), {mode: 'edit', id: c.id, name: txt(k.name), display: txt(c.display), desc: txt(c.desc), env: c.env || 'test', cmode: c.mode || 'live', fixtures: txt(k.fixtures), pd: txt(k.pd), tidbStatus: txt(k.tidbStatus), prom: txt(k.prom), alert: txt(k.alert),
      mysqlHost: txt(k.mysqlHost), mysqlPort: k.mysqlPort ? String(k.mysqlPort) : '', mysqlUser: txt(k.mysqlUser), hasPassword: !!k.hasPassword, mysqlBin: txt(k.mysqlBin), tiup: txt(k.tiup), home: txt(k.home), primary: !!c.primary});
  }
  function idHint(F) {
    if (F.mode !== 'add') return '';
    if (!F.id) return '<span class="muted">2–32 位：小写字母开头，只含小写字母 / 数字 / 连字符；创建后不可修改</span>';
    if (!idRe().test(F.id)) return '<span style="color:var(--bad)">✖ 格式不符：小写字母开头，只含小写字母 / 数字 / 连字符，2–32 位</span>';
    if (find(F.id)) return '<span style="color:var(--bad)">✖ 该 id 已存在</span>';
    return '<span style="color:var(--ok)">✔ 可用</span>';
  }
  function fld(label, k, F, opt) {
    opt = opt || {};
    const ro = opt.ro ? 'disabled' : '';
    return `<div class="field"><label>${label}</label><input type="${opt.type || 'text'}" data-ni="oc-f" data-k="${k}" value="${E(F[k])}" ${opt.ph ? `placeholder="${E(opt.ph)}"` : ''} ${opt.max ? `maxlength="${opt.max}"` : ''} ${opt.auto ? `autocomplete="${opt.auto}"` : 'autocomplete="off"'} spellcheck="false" ${ro}>${opt.hint ? `<div class="small muted">${opt.hint}</div>` : ''}</div>`;
  }
  function formHtml() {
    const F = OC.form, add = F.mode === 'add', prim = F.primary, lock = prim ? 'disabled' : '';
    const live = F.cmode === 'live';
    return `${prim ? '<div class="notice info"><span>ℹ</span><div><b>主集群</b>的环境与连接配置来自平台启动参数，这里只能修改<b>显示名</b>和<b>描述</b>；其余控件已禁用。主集群不能删除。</div></div>' : ''}
      <h3 class="ou-sec">基本信息</h3>
      ${add ? `<div class="field"><label>集群 id（平台内唯一，用于请求头与审计；创建后不可修改）</label><input type="text" data-ni="oc-f" data-k="id" value="${E(F.id)}" maxlength="32" autocomplete="off" spellcheck="false" placeholder="例如 stg、prod-sh">
        <div class="small" id="ocIdHint" style="min-height:16px">${idHint(F)}</div></div>`
      : `<div class="kv" style="margin-bottom:10px"><span>集群 id</span><span class="mono">${E(F.id)} <span class="muted small">（不可修改）</span></span></div>`}
      <div class="oc-g2">${fld('显示名（顶栏下拉里显示，默认同 id）', 'display', F, {max: 40})}${OC.standalone ? fld('集群名 name（仅作标识与审计记录，默认同 id）', 'name', F, {max: 64, ph: add ? (F.id || '默认同 id') : '', ro: prim}) : fld('tiup 集群名 name（<code>tiup cluster list</code> 里的名字，默认同 id）', 'name', F, {max: 64, ph: add ? (F.id || '默认同 id') : '', ro: prim})}</div>
      ${fld('描述（可选）', 'desc', F, {max: 200})}
      <div class="oc-g2"><div class="field"><label>环境（决定该集群里的角色权限与审批强度）</label><select data-nc="oc-f" data-k="env" ${lock}>${ENVS.map(([v, t]) => `<option value="${v}" ${F.env === v ? 'selected' : ''}>${t}</option>`).join('')}</select></div>
        <div class="field"><label>模式</label><div class="oc-radio"><label><input type="radio" name="ocMode" data-nc="oc-f" data-k="cmode" value="live" ${live ? 'checked' : ''} ${lock}> live 实时</label>${OC.standalone && !OC.roots.length && live ? '' : `<label><input type="radio" name="ocMode" data-nc="oc-f" data-k="cmode" value="replay" ${live ? '' : 'checked'} ${lock}> replay 回放</label>`}</div></div></div>
      ${F.env === 'prod' ? '<div class="notice warn"><span>⚠</span><div><b>生产环境：更严格审批</b>。高风险变更需要更多级审批 / MFA，写操作默认关闭并需两名管理员确认开启（具体以「审批策略」为准）。</div></div>' : ''}
      ${live ? `<h3 class="ou-sec">连接信息（live）</h3>
        <div class="small muted" style="margin-bottom:8px">${OC.standalone
          ? '平台所在机器不是该集群的中控机，没有 tiup 可用于自动探测，所以<b>PD 地址必填</b>。平台只读访问这些地址：Prometheus 决定总览趋势 / 主机指标，Alertmanager 决定告警，TiDB status 与只读 SQL 账号决定慢 SQL、集群版本与实例信息，没填的页面会显示「数据缺失」。'
          : '留空的地址会继承平台启动参数 / 自动探测。平台以只读方式访问 PD / Prometheus / Alertmanager；变更经 tiup 执行（中控机上的 sudo 策略需允许该 tiup 集群名，提交后会提示）。'}</div>
        <div class="oc-g2">${fld('PD 地址', 'pd', F, {ph: 'http://10.0.1.21:2379', ro: prim})}${fld('TiDB status 地址', 'tidbStatus', F, {ph: 'http://10.0.1.11:10080', ro: prim})}
          ${fld('Prometheus 地址', 'prom', F, {ph: 'http://10.0.1.31:9090', ro: prim})}${fld('Alertmanager 地址', 'alert', F, {ph: 'http://10.0.1.31:9093', ro: prim})}</div>
        <div class="oc-g3">${fld('MySQL host（TiDB SQL）', 'mysqlHost', F, {ph: '10.0.1.11', ro: prim})}${fld('端口（留空 = 4000）', 'mysqlPort', F, {ph: '4000', max: 5, ro: prim})}${fld('用户', 'mysqlUser', F, {ph: 'readonly', ro: prim})}</div>
        <div class="field"><label>密码 ${add ? '' : (F.hasPassword ? '<span class="tag ok">已保存密码</span>' : '<span class="tag">未保存密码</span>')}</label>
          <input type="password" data-ni="oc-f" data-k="mysqlPassword" value="${E(F.mysqlPassword)}" autocomplete="new-password" placeholder="${add ? '' : '留空 = 不修改'}" ${prim || F.clearPassword ? 'disabled' : ''}>
          <div class="small muted">密码加密保存在${OC.standalone ? '平台所在机器的数据目录' : '中控机'}，接口只返回「是否已保存」，不会回显。</div>
          ${add ? '' : `<label class="small" style="margin-top:4px"><input type="checkbox" data-nc="oc-f" data-k="clearPassword" ${F.clearPassword ? 'checked' : ''} ${prim || !F.hasPassword ? 'disabled' : ''}> 清除已保存的密码</label>`}</div>
        <details class="oc-adv"><summary>高级：可执行文件 / 目录（绝对路径，留空用默认）</summary>
          ${OC.standalone ? `<div class="oc-g2">${fld('mysql 可执行文件', 'mysqlBin', F, {ph: 'mysql', ro: prim})}</div>`
            : `<div class="oc-g2">${fld('mysql 可执行文件', 'mysqlBin', F, {ph: 'mysql', ro: prim})}${fld('tiup 可执行文件', 'tiup', F, {ph: 'tiup', ro: prim})}</div>
          ${fld('tiup 用户的 HOME 目录（用于定位 meta.yaml）', 'home', F, {ph: '/home/tidb', ro: prim})}`}</details>`
      : `<h3 class="ou-sec">数据包（replay）</h3>
        <div class="small muted" style="margin-bottom:8px">回放集群读取 fixtures 数据包，不连接任何真实组件，也不会执行变更到真实集群（用于演示 / 培训 / 联调）。fixtures 目录必须位于允许的根目录之下。</div>
        ${fld('fixtures 目录', 'fixtures', F, {ph: (OC.roots[0] || '') + '/dev', ro: prim, hint: OC.roots.length ? '可用根目录：' + OC.roots.map(r => `<code>${E(r)}</code>`).join('、') : '后端没有返回可用根目录：回放集群可能无法添加'})}`}
      <div id="ocFormErr" class="small" style="min-height:18px">${F.err ? `<div class="notice bad"><span>✖</span><div>${E(F.err)}</div></div>` : ''}</div>
      <div class="sticky-actions"><button class="btn primary" data-na="oc-form-go">${add ? '接入集群' : '保存'}</button><button class="btn" data-close="1">取消</button></div>`;
  }
  const formRender = () => { const el = $('#ocForm'); if (el && OC.form && !OC.form.result) el.innerHTML = formHtml(); };
  function formOpen(F, title) { OC.form = F; openDrawer(title, '<div id="ocForm"></div>'); formRender(); }
  const warnsHtml = ws => (ws || []).map(w => `<div class="notice warn"><span>⚠</span><div>${E(w)}</div></div>`).join('');

  async function formSubmit(btn) {
    const F = OC.form, add = F.mode === 'add', say = m => { F.err = m; const e = $('#ocFormErr'); if (e) e.innerHTML = m ? `<div class="notice bad"><span>✖</span><div>${E(m)}</div></div>` : ''; };
    const live = F.cmode === 'live';
    let body;
    if (F.primary) body = {display: F.display.trim(), desc: F.desc.trim()};
    else {
      if (add && !idRe().test(F.id)) return say('集群 id 格式不符：小写字母开头，只含小写字母 / 数字 / 连字符，2–32 位');
      if (add && find(F.id)) return say('集群 id 已存在');
      if (F.name.trim() && !/^[A-Za-z0-9_-]{1,64}$/.test(F.name.trim())) return say('tiup 集群名只能包含字母、数字、下划线和连字符（1–64 位）');
      const port = F.mysqlPort.trim() === '' ? 0 : Number(F.mysqlPort);
      if (live && (!Number.isInteger(port) || port < 0 || port > 65535)) return say('MySQL 端口应为 1–65535 的整数（留空 = 4000）');
      if (!live && !F.fixtures.trim()) return say('回放模式必须填写 fixtures 目录');
      body = {name: F.name.trim() || F.id, display: F.display.trim() || F.id, desc: F.desc.trim(), env: F.env, mode: F.cmode};
      if (add) body.id = F.id;
      if (live) {
        Object.assign(body, {pd: F.pd.trim(), tidbStatus: F.tidbStatus.trim(), prom: F.prom.trim(), alert: F.alert.trim(), mysqlHost: F.mysqlHost.trim(), mysqlPort: port, mysqlUser: F.mysqlUser.trim(), mysqlBin: F.mysqlBin.trim(), tiup: F.tiup.trim(), home: F.home.trim()});
        if (F.mysqlPassword) body.mysqlPassword = F.mysqlPassword;
        if (F.clearPassword) body.clearPassword = true;
      } else body.fixtures = F.fixtures.trim();
    }
    btn.disabled = true; say('');
    const r = await opApi('POST', add ? '/api/clusters' : `/api/clusters/${encodeURIComponent(F.id)}`, body);
    btn.disabled = false;
    if (r.code !== 200) { say(errText(r)); return; }
    F.result = r.data; F.mysqlPassword = '';
    const c = r.data.cluster || {}, changed = Array.isArray(r.data.changed) ? r.data.changed : [];
    const isCur = window.CL && CL.cur === F.id;
    $('#drTitle').textContent = add ? '已接入集群：' + (c.display || F.id) : '已保存：' + (c.display || F.id);
    $('#drBody').innerHTML = `<div class="notice ok"><span>✔</span><div>${add ? `集群 <b>${E(c.display || F.id)}</b>（<span class="mono">${E(F.id)}</span>）已接入，环境 ${E((ENV_TAG[c.env] || [0, c.env])[1])}，模式 ${E(c.mode)}。` : `集群 <b>${E(c.display || F.id)}</b> 的配置已保存${changed.length ? '，修改了：' + E(changed.join('、')) : '（没有任何改动）'}。`}</div></div>
      ${warnsHtml(r.data.warnings)}
      ${add ? (OC.standalone ? '<div class="small muted">手工接入的集群是<b>只读接入</b>（平台只通过网络地址读取，不碰它的 tiup / SSH）：现在可以看总览、拓扑、慢 SQL、热点、告警与诊断。建议点列表里的「测试」确认各组件地址都通；「数据缺失」提示里会说明哪些项没采到（例如没填 Prometheus 或只读 SQL 账号）。</div>' : '<div class="small muted">新集群的写操作开关默认关闭；需要执行变更时，请先切换到该集群，在「权限与审计 → 写操作开关」由两名管理员确认开启。如果提示了 sudo 策略，请在中控机上完成后再提交变更。</div>') : ''}
      ${!add && isCur ? '<div class="small muted">你正在使用的就是这个集群：环境 / 权限变化会在几秒内同步到页面。</div>' : ''}
      <div class="sticky-actions">${add ? `<button class="btn primary" data-na="oc-switch" data-id="${E(F.id)}">切换到该集群</button>` : ''}<button class="btn" data-close="1">关闭</button></div>`;
    OC.flash = null; await load(); opRefresh(true);   // 刷新列表与顶栏（/api/auth/me 的 clusters）
  }

  /* ---------------- 从本机 tiup 导入（中控机模式） ---------------- */
  /* 只做两件事：经本机受限执行器跑只读的 tiup cluster list / display，读出各组件地址并登记为「本机 tiup」集群。
     不改动集群，也不读取 / 传输任何 SSH 私钥（tiup 用 tidb 用户自己的密钥连各节点）。 */
  const impSel = () => (OC.imp ? OC.imp.items.filter(x => x.sel && x.state === 'new') : []);
  async function impOpen() {
    OC.imp = {loading: true, err: '', available: true, reason: '', items: [], defEnv: 'prod', mysqlUser: '', mysqlPassword: '', busy: false, result: null};
    openDrawer('从本机 tiup 导入集群', '<div id="ocImp"></div>'); impRender();
    const r = await opApi('GET', '/api/clusters/discover');
    const I = OC.imp; if (!I || !$('#ocImp')) return;
    I.loading = false;
    if (r.code !== 200) I.err = errText(r);
    else {
      I.available = r.data.available !== false; I.reason = r.data.reason || ''; I.err = r.data.error || ''; I.defEnv = r.data.defaultEnv || 'prod';
      I.items = (r.data.items || []).map(x => Object.assign({sel: false, display: x.name, env: I.defEnv}, x));
    }
    impRender();
  }
  const impState = x => x.state === 'imported' ? tg('已导入', 'ok', '已纳管：集群 id ' + x.id)
    : x.state === 'conflict' ? tg('同名手工集群', 'warn', '已有一个手工添加的同名集群（只读）。要导入请先在列表里删除它') : tg('可导入', 'info');
  function impHtml() {
    const I = OC.imp;
    if (I.result) return impResultHtml(I.result);
    if (I.loading) return '<div class="empty">正在经受限执行器读取本机 tiup 的集群列表（tiup cluster list）…</div>';
    if (!I.available) return `<div class="notice warn"><span>⚠</span><div>${E(I.reason || '当前部署不支持从本机 tiup 导入')}</div></div><div class="sticky-actions"><button class="btn" data-close="1">关闭</button></div>`;
    if (I.err) return `<div class="notice bad"><span>✖</span><div>${E(I.err)}</div></div><div class="sticky-actions"><button class="btn" data-na="oc-imp-retry">重试</button><button class="btn" data-close="1">关闭</button></div>`;
    const n = impSel().length;
    return `<div class="small muted" style="margin-bottom:10px">本机 tiup 管理着下面这些集群。导入只运行<b>只读</b>命令（<code>tiup cluster list / display</code>）读出各组件地址并登记，<b>不改动集群，也不读取或传输 SSH 私钥</b>。导入后可做全部运维操作，写操作仍需两名管理员开启写开关、高风险变更仍需审批。</div>
      <div class="tbl-wrap"><table><thead><tr><th></th><th>tiup 集群名</th><th>版本</th><th>状态</th><th>显示名</th><th>环境</th></tr></thead><tbody>
      ${I.items.map((x, i) => {
        const can = x.state === 'new';
        return `<tr><td><input type="checkbox" data-nc="oc-i" data-i="${i}" data-k="sel" ${x.sel ? 'checked' : ''} ${can ? '' : 'disabled'}></td>
          <td><b>${E(x.name)}</b>${x.id && x.id !== x.name ? `<div class="mono small muted">id ${E(x.id)}</div>` : ''}</td><td class="small">${E(x.version || '—')}</td><td>${impState(x)}</td>
          <td>${can ? `<input type="text" data-ni="oc-i" data-i="${i}" data-k="display" value="${E(x.display)}" maxlength="40" style="width:140px">` : '<span class="muted">—</span>'}</td>
          <td>${can ? `<select data-nc="oc-i" data-i="${i}" data-k="env">${ENVS.map(e => `<option value="${e[0]}" ${x.env === e[0] ? 'selected' : ''}>${e[1]}</option>`).join('')}</select>` : envTag(x.env)}</td></tr>`;
      }).join('') || '<tr><td colspan="6" class="empty">本机 tiup 里没有集群（<code>tiup cluster list</code> 为空）。请确认平台是装在 tiup 中控机上、且 tidb 用户下有集群。</td></tr>'}</tbody></table></div>
      <div class="field" style="margin-top:12px"><label>只读 SQL 账号（可选，对本次导入的全部集群生效）</label>
        <div class="oc-g2"><input type="text" data-ni="oc-ig" data-k="mysqlUser" value="${E(I.mysqlUser)}" placeholder="例如 tidbops_ro（留空 = 暂不配置）" autocomplete="off" spellcheck="false">
        <input type="password" data-ni="oc-ig" data-k="mysqlPassword" value="${E(I.mysqlPassword)}" placeholder="口令（加密保存，不回显）" autocomplete="new-password"></div>
        <div class="small muted">用于采集慢 SQL / 配置等。不填也能导入，之后可在列表里「编辑」补上；不同集群的账号不同就分批导入。</div></div>
      <div class="sticky-actions"><button class="btn primary" data-na="oc-imp-go" ${n && !I.busy ? '' : 'disabled'}>导入所选${n ? '（' + n + '）' : ''}</button><button class="btn" data-close="1">取消</button></div>`;
  }
  function impResultHtml(d) {
    const rs = d.results || [], ok = rs.filter(x => x.ok);
    return `<div class="notice ${d.ok ? 'ok' : ok.length ? 'warn' : 'bad'}"><span>${d.ok ? '✔' : ok.length ? '⚠' : '✖'}</span><div>${d.ok ? `已导入 <b>${ok.length}</b> 个集群。` : `成功 ${ok.length} / 共 ${rs.length}，下面标出了失败的原因。`}</div></div>
      <div class="tbl-wrap"><table><thead><tr><th>集群</th><th>结果</th></tr></thead><tbody>
      ${rs.map(x => `<tr class="${x.ok ? '' : 'flag'}"><td><b>${E(x.name)}</b>${x.id ? `<div class="mono small muted">${E(x.id)}</div>` : ''}</td>
        <td class="small">${x.ok ? tg('已导入', 'ok') : tg('失败', 'bad')}${x.error ? `<div style="color:var(--bad);word-break:break-all">${E(x.error)}</div>` : ''}${(x.warnings || []).map(w => `<div style="color:var(--warn)">⚠ ${E(w)}</div>`).join('')}</td></tr>`).join('')}</tbody></table></div>
      <div class="small muted" style="margin-top:8px">下一步：点列表里的「测试」确认各组件地址都通；运维操作前先切换到该集群，在「权限与审计 → 写操作开关」由两名管理员确认开启。</div>
      <div class="sticky-actions">${ok[0] ? `<button class="btn primary" data-na="oc-switch" data-id="${E(ok[0].id)}">切换到「${E(ok[0].name)}」</button>` : ''}<button class="btn" data-close="1">关闭</button></div>`;
  }
  const impRender = () => { const el = $('#ocImp'); if (el && OC.imp) el.innerHTML = impHtml(); };
  async function impGo(btn) {
    const I = OC.imp, sel = impSel(); if (!I || !sel.length || I.busy) return;
    I.busy = true; btn.disabled = true;
    const body = {items: sel.map(x => ({name: x.name, id: x.id, display: (x.display || '').trim() || x.name, env: x.env}))};
    if (I.mysqlUser.trim()) { body.mysqlUser = I.mysqlUser.trim(); if (I.mysqlPassword) body.mysqlPassword = I.mysqlPassword; }
    const r = await opApi('POST', '/api/clusters/import', body);
    I.busy = false; I.mysqlPassword = '';
    if (r.code !== 200) { I.err = errText(r); impRender(); return; }
    I.result = r.data; impRender();
    $('#drTitle').textContent = '导入结果';
    OC.flash = null; await load(); opRefresh(true);   // 刷新列表与顶栏（/api/auth/me 的 clusters）
  }

  /* ---------------- 测试 ---------------- */
  async function runTest(id) {
    const c = find(id), tok = ++OC.tok, name = c ? (c.display || id) : id;
    openDrawer('测试：' + name, '<div class="empty">正在只读探测…（replay 校验数据包，live 逐项访问 PD / Prometheus / Alertmanager / SQL / tiup）</div>');
    const r = await opApi('POST', `/api/clusters/${encodeURIComponent(id)}/test`, {});
    if (tok !== OC.tok || !$('#drBody')) return;
    if (r.code !== 200) { $('#drBody').innerHTML = `<div class="notice bad"><span>✖</span><div>${E(errText(r))}</div></div><div class="sticky-actions"><button class="btn" data-na="oc-test" data-id="${E(id)}">重试</button><button class="btn" data-close="1">关闭</button></div>`; return; }
    const items = r.data.items || [], bad = items.filter(i => i.status === 'bad').length, warn = items.filter(i => i.status === 'warn').length;
    const T = {ok: ['ok', '通过'], warn: ['warn', '警告'], bad: ['bad', '失败']};
    $('#drBody').innerHTML = `<div class="notice ${bad ? 'bad' : warn ? 'warn' : 'ok'}"><span>${bad ? '✖' : warn ? '⚠' : '✔'}</span><div>${bad ? `<b>${bad} 项失败</b>${warn ? `，${warn} 项警告` : ''}。` : warn ? `<b>可用，${warn} 项警告。</b>` : '<b>全部通过。</b>'}只读探测，不会改动集群。</div></div>
      <div class="tbl-wrap"><table><thead><tr><th>检查项</th><th>结果</th><th>耗时</th><th>详情</th></tr></thead><tbody>
      ${items.map(i => { const t = T[i.status] || ['', i.status]; return `<tr class="${i.status === 'bad' ? 'flag' : ''}"><td>${E(i.item)}</td><td>${tg(t[1], t[0])}</td><td class="small mono" style="white-space:nowrap">${i.ms != null ? E(i.ms) + ' ms' : '—'}</td><td class="small" style="word-break:break-all">${E(i.detail)}</td></tr>`; }).join('') || '<tr><td colspan="4" class="empty">没有检查项</td></tr>'}</tbody></table></div>
      <div class="sticky-actions"><button class="btn" data-na="oc-test" data-id="${E(id)}">重新测试</button><button class="btn" data-close="1">关闭</button></div>`;
  }

  /* ---------------- 删除 ---------------- */
  function delOpen(id) {
    const c = find(id); if (!c || c.primary) return;
    OC.del = {id, display: c.display || id};
    const cur = window.CL && CL.cur === id;
    openDrawer('删除集群：' + OC.del.display, `<div class="notice bad"><span>⚠</span><div><b>高危操作：移除集群 <span class="mono">${E(id)}</span> 的纳管。</b></div></div>
      <ul class="small oc-ul"><li>仅<b>移除纳管</b>：<b>不会停止或删除 TiDB 集群</b>，集群上的业务不受影响。</li>
        <li>平台内该集群的运行数据（变更记录、写开关、窗口 / 冻结期、通知渠道、预测等）所在目录会<b>改名保留</b>，不会被删除；完成后会告诉你保留的目录。</li>
        <li>该集群有<b>执行中或待审批的变更</b>时会被拒绝，请先处理完。</li>
        <li>审计日志全局保留，仍可按集群筛选。</li>
        ${cur ? '<li style="color:var(--warn)">你<b>正在使用</b>这个集群：删除后页面会切换到默认集群。</li>' : ''}</ul>
      <div class="field"><label>请输入集群 id <b class="mono">${E(id)}</b> 以确认</label><input type="text" id="ocDelId" data-ni="oc-delid" autocomplete="off" spellcheck="false" placeholder="${E(id)}"></div>
      <div id="ocDelErr" class="small" style="min-height:18px"></div>
      <div class="sticky-actions"><button class="btn danger" id="ocDelGo" data-na="oc-del-go" disabled>确认删除</button><button class="btn" data-close="1">取消</button></div>`);
    setTimeout(() => { const i = $('#ocDelId'); if (i) i.focus(); }, 30);
  }
  async function delGo(btn) {
    const D = OC.del; if (!D) return;
    if ($('#ocDelId').value.trim() !== D.id) return;
    const wasCur = window.CL && CL.cur === D.id, err = $('#ocDelErr');
    btn.disabled = true; err.innerHTML = '';
    if (wasCur) CL._holdLost = true;   // 删除过程中不要因为 cluster_not_found 自动刷新页面（要让用户先看到 keptDir）
    const r = await opApi('POST', `/api/clusters/${encodeURIComponent(D.id)}/delete`, {});
    if (r.code !== 200) { CL._holdLost = false; btn.disabled = false; err.innerHTML = `<div class="notice bad"><span>✖</span><div>${E(errText(r))}</div></div>`; return; }
    let left = false;
    if (wasCur && CL.leave) left = CL.leave(D.id);
    CL._holdLost = false;
    const kept = r.data.keptDir;
    $('#drTitle').textContent = '已移除纳管：' + D.display;
    $('#drBody').innerHTML = `<div class="notice ok"><span>✔</span><div>集群 <b>${E(D.display)}</b>（<span class="mono">${E(D.id)}</span>）已移除纳管。TiDB 集群本身没有被停止或删除。</div></div>
      ${kept ? `<div class="notice info"><span>ℹ</span><div>平台内该集群的运行数据目录已<b>改名保留</b>（不会被自动清理）：<br><code class="mono" style="word-break:break-all;user-select:all">${E(kept)}</code><br><span class="small muted">确认不再需要后，可在中控机上手工删除。</span></div></div>` : '<div class="small muted">该集群没有需要保留的运行数据目录。</div>'}
      ${left ? '<div class="notice warn"><span>⚠</span><div>这是你正在使用的集群：下次切换页面时会刷新并回到默认集群。</div></div>' : ''}
      <div class="sticky-actions">${left ? '<button class="btn primary" data-na="oc-reload-page">立即切换到默认集群</button>' : ''}<button class="btn" data-close="1">关闭</button></div>`;
    OC.del = null; OC.flash = null; await load(); opRefresh(true);
  }

  /* ---------------- 事件 ---------------- */
  const CLICK = {
    'oc-flash-x': () => { OC.flash = null; paint(); },
    'oc-reload': () => { OC.flash = null; load().then(() => toast('已刷新')); },
    'oc-reload-page': () => { location.reload(); },
    'oc-new': () => formOpen(newForm(), OC.localTiup ? '手工接入远程集群（只读）' : '接入集群'),
    'oc-import': () => impOpen(),
    'oc-imp-retry': () => impOpen(),
    'oc-imp-go': (d, el) => impGo(el),
    'oc-edit': d => { const c = find(d.id); if (c) formOpen(editForm(c), '编辑集群：' + (c.display || c.id)); },
    'oc-form-go': (d, el) => formSubmit(el),
    'oc-test': d => runTest(d.id),
    'oc-del': d => delOpen(d.id),
    'oc-del-go': (d, el) => delGo(el),
    'oc-switch': (d, el) => { if (window.CL) CL.switchTo(d.id, !!el.closest('.sticky-actions')); },   // 抽屉结果页里的按钮：没有未保存内容，不再二次确认
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na^="oc-"]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    const ie = e.target.closest && e.target.closest('[data-nc="oc-i"]');   // 导入抽屉：勾选 / 环境
    if (ie && OP.on && OC.imp) {
      const x = OC.imp.items[Number(ie.dataset.i)];
      if (x) { x[ie.dataset.k] = ie.type === 'checkbox' ? ie.checked : ie.value; if (ie.dataset.k === 'sel') impRender(); }
      return;
    }
    if (!OP.on) return;
    const el = e.target.closest('[data-nc="oc-f"]'); if (!el || !OC.form) return;
    const k = el.dataset.k;
    OC.form[k] = el.type === 'checkbox' ? el.checked : el.value;
    if (k === 'clearPassword' && el.checked) OC.form.mysqlPassword = '';
    if (['env', 'cmode', 'clearPassword'].includes(k)) formRender();
  });
  document.addEventListener('input', e => {
    const ie = e.target.closest && e.target.closest('[data-ni="oc-i"],[data-ni="oc-ig"]');   // 导入抽屉：显示名 / 共用的只读 SQL 账号
    if (ie && OP.on && OC.imp) {
      if (ie.dataset.ni === 'oc-i') { const x = OC.imp.items[Number(ie.dataset.i)]; if (x) x[ie.dataset.k] = ie.value; } else OC.imp[ie.dataset.k] = ie.value;
      return;
    }
    if (!OP.on) return;
    const el = e.target.closest('[data-ni^="oc-"]'); if (!el) return;
    if (el.dataset.ni === 'oc-f' && OC.form) {
      OC.form[el.dataset.k] = el.value;
      if (el.dataset.k === 'id') { const h = $('#ocIdHint'); if (h) h.innerHTML = idHint(OC.form); }
    } else if (el.dataset.ni === 'oc-delid' && OC.del) { const b = $('#ocDelGo'); if (b) b.disabled = el.value.trim() !== OC.del.id; }
  });
  document.addEventListener('keydown', e => {
    if (!OP.on || e.key !== 'Enter') return;
    const el = e.target.closest && e.target.closest('#ocDelId'); if (el && OC.del && el.value.trim() === OC.del.id) { e.preventDefault(); const b = $('#ocDelGo'); if (b) delGo(b); }
  });

  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || !OP.me) return;
    if (h === 'clusters' && !ctx.open && !ctx.typing && $('#ocBody') && Date.now() - OC.at > 9000) load();   // 状态 / 快照时间会随时间变化
  });

  /* 独立部署：「尚未接入集群」与「已有集群」两种状态切换时（接入第一个集群 / 最后一个集群被删除），
     集群数据页要换成引导页或换回真实数据：先重新取一次快照，再重绘当前页。 */
  let wasEmpty;
  OP_HOOKS.push(async (h, ctx) => {
    if (!OP.on || !OP.me || !window.CL) return;
    const now = CL.empty();
    if (wasEmpty === undefined) { wasEmpty = now; return; }
    if (now === wasEmpty) return;
    wasEmpty = now;
    if (now) { if (typeof liveApplyEmpty === 'function') liveApplyEmpty(); }
    else if (typeof liveRefresh === 'function') await liveRefresh(true);
    if (typeof liveBanner === 'function' && LIVE.snap) liveBanner();
    const page = (location.hash.slice(1) || 'overview').split('/')[0];
    if (!ctx.open && !ctx.typing && CL.gated(page) && typeof render === 'function') render(page);
  });
})();
