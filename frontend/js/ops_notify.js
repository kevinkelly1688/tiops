// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_notify.js
// @time: 2026/10/28

/* 运维模式：环境与设置页的「通知渠道」（企业微信 / 钉钉 / 飞书 / Lark / Slack / 其他 Webhook）与「变更窗口规则」两张卡片。
   只在 OP.on === true 时生效；OP.on === false 时 pages.settings 原样不变。
   做法：包装 ops_pages.js 已包装过的 pages.settings（保留其 render / init），在页面末尾追加两张卡片；
   事件全部用 data-na / data-nc + 文档级委托（CSP 禁止内联事件）。data-na 前缀：ont-（通知渠道） onw-（窗口规则）。
   接口：GET /api/ops/notify 与 POST /api/ops/notify/channels[/{id}[/delete|/test|/enabled]]（需 settings.edit）；
         GET / POST /api/ops/settings 的 windows 字段（读：登录即可；写：settings.edit）。
   依赖 ops.js（OP / opApi / opCan / opEsc）、ops_pages.js（oppConfirm / oppLoadSettings / OP_HOOKS）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); return `${d.getMonth() + 1}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;
  const hashIs = h => (location.hash.slice(1) || 'overview') === h;
  const canEdit = () => opCan('settings.edit');
  const clip = (s, n) => { s = String(s || ''); return s.length > n ? s.slice(0, n) + '…' : s; };

  const ON = {
    n: null, nErr: '', nLoading: false, test: {}, busy: {}, form: null,                    // 通知渠道
    w: null, wCustom: false, wDefaults: [], wErr: '', wLoading: false, wForm: null,        // 窗口规则
  };

  /* ======================================================================
     通知渠道
     ====================================================================== */
  const TYPE_TAG = {wecom: ['info', '企业微信'], dingtalk: ['ok', '钉钉'], feishu: ['info', '飞书'], lark: ['warn', 'Lark'], slack: ['ok', 'Slack'], other: ['', '其他']};
  const REASON = {'rate-limited': '超出每分钟限速，已丢弃', 'queue-full': '发送队列已满，已丢弃', 'channel-deleted': '渠道已删除', shutdown: '服务关闭，未发送'};
  const evGroup = k => k.startsWith('change.') ? '变更' : k.startsWith('maint.') ? '维护' : '告警与诊断';
  const evLabel = k => { if (k === 'test') return '测试消息'; const e = ((ON.n && ON.n.events) || []).find(x => x.key === k); return e ? e.label : k; };
  const typeInfo = t => ((ON.n && ON.n.types) || []).find(x => x.type === t) || {type: t, label: t, hostExamples: [], secretLabel: '签名密钥'};

  async function ontLoad() {
    if (!OP.on || !OP.me || !canEdit() || ON.nLoading) return;
    ON.nLoading = true;
    const r = await opApi('GET', '/api/ops/notify');
    ON.nLoading = false;
    if (r.code === 200) { ON.n = r.data; ON.nErr = ''; } else ON.nErr = r.data.error || ('读取失败（' + r.code + '）');
    ontPaint();
  }

  function ontPaint() {
    const el = $('#ontBox'); if (!el || !hashIs('settings')) return;
    const h = ontHtml();
    if (el._h === h) return;
    el._h = h; el.innerHTML = h;
  }

  function evSummary(c) {
    const ev = c.events || [];
    if (!ev.length) return `<span class="tag" title="未限定事件：订阅全部">全部事件</span>`;
    const names = ev.map(evLabel);
    return `<span class="small" title="${E(names.join('、'))}">${E(names.slice(0, 2).join('、'))}${names.length > 2 ? ` 等 ${names.length} 项` : ''}</span>`;
  }


  function channelRows(ro) {
    const list = ON.n.channels || [];
    if (!list.length) return '<tr><td colspan="6" class="empty">还没有通知渠道。点击右上角「添加渠道」，把变更审批 / 执行结果 / 严重告警推送到群里。</td></tr>';
    return list.map(c => {
      const tt = TYPE_TAG[c.type] || ['', c.type], res = ON.test[c.id], busy = ON.busy[c.id];
      const row = `<tr><td>${tg(tt[1], tt[0])}</td><td>${E(c.name)}${c.hasSecret ? ` <span class="tag" title="已配置加签 / 签名校验密钥">已加签</span>` : ''}</td>
        <td class="mono small" style="word-break:break-all;max-width:260px">${E(c.url)}</td><td>${evSummary(c)}</td>
        <td><label class="small"><input type="checkbox" data-nc="ont-en" data-id="${E(c.id)}" ${c.enabled ? 'checked' : ''} ${ro ? 'disabled' : ''}> ${c.enabled ? '启用' : '停用'}</label></td>
        <td><div class="acts"><button class="btn sm" data-na="ont-test" data-id="${E(c.id)}" ${busy ? 'disabled' : ''}>${busy ? '发送中…' : '发送测试'}</button>
          <button class="btn sm" data-na="ont-edit" data-id="${E(c.id)}" ${ro ? 'disabled' : ''}>编辑</button>
          <button class="btn sm danger" data-na="ont-del" data-id="${E(c.id)}" ${ro ? 'disabled' : ''}>删除</button></div></td></tr>`;
      const tr = res ? `<tr><td></td><td colspan="5" class="small" style="color:var(--${res.ok ? 'ok' : 'bad'})">${res.ok ? '✔ 测试消息已发送' : '✖ 测试失败：' + E(res.error || '未知错误')} <span class="muted">（${fDT(res.at)}）</span></td></tr>` : '';
      return row + tr;
    }).join('');
  }

  function logRows() {
    const log = ON.n.log || [];
    if (!log.length) return '<tr><td colspan="7" class="empty">还没有投递记录</td></tr>';
    return log.map(l => `<tr class="${l.ok ? '' : 'flag'}"><td class="small" style="white-space:nowrap">${fDT(l.time)}</td><td class="small">${E(l.channelName || l.channelId)}</td>
      <td class="small" title="${E(l.event)}">${E(evLabel(l.event))}</td><td class="small" style="max-width:240px">${E(l.title)}</td>
      <td>${l.ok ? tg('✔ 成功', 'ok') : tg('✖ 失败', 'bad')}</td><td class="small">${l.attempts || 0}</td>
      <td class="small" style="max-width:300px;word-break:break-all">${E(l.error || (l.reason ? REASON[l.reason] || l.reason : ''))}</td></tr>`).join('');
  }

  function ontHtml() {
    const head = `<h2>通知渠道 <span class="sub">${window.CL && CL.active() ? "集群 " + opEsc(CL.name()) + " · " : ""}Webhook 推送到企业微信 / 钉钉 / 飞书 / Lark / Slack / 其他（通用 Webhook）</span>`;
    if (!canEdit()) {
      return `<div class="card">${head}</h2><div class="notice warn"><span>⚠</span><div>需要管理员权限：查看与管理通知渠道需要「修改平台设置」（settings.edit）。</div></div></div>`;
    }
    if (!ON.n) {
      return `<div class="card">${head}</h2>${ON.nErr ? `<div class="notice bad"><span>✖</span><div>${E(ON.nErr)}</div></div>` : '<div class="empty">正在读取…</div>'}</div>`;
    }
    const n = ON.n, ro = !!n.readOnly, hosts = (n.allowedHosts || []).join('、');
    return `<div class="card">${head}<span class="right"><button class="btn primary sm" data-na="ont-new" ${ro ? 'disabled' : ''}>＋ 添加渠道</button></span></h2>
      ${ro ? '<div class="notice bad"><span>✖</span><div>渠道文件读取或解密失败，当前为只读：不能新增 / 修改 / 删除渠道（避免覆盖原文件）。请检查后端日志与主密钥配置。</div></div>' : ''}
      ${ON.nErr ? `<div class="notice bad"><span>✖</span><div>${E(ON.nErr)}</div></div>` : ''}
      <div class="tbl-wrap"><table><thead><tr><th>类型</th><th>名称</th><th>Webhook（已脱敏）</th><th>订阅事件</th><th>状态</th><th></th></tr></thead><tbody>${channelRows(ro)}</tbody></table></div>
      <div class="small muted" style="margin-top:8px;display:grid;gap:3px">
        <div>消息内容包含：环境 / 集群 / 变更标题 / 风险 / 申请人 / 原因；<b>不含命令输出与凭据</b>。</div>
        <div>Webhook 只允许 https 官方域名${hosts ? '（当前允许：' + E(hosts) + '）' : ''}与管理员配置的额外主机；URL 与密钥加密保存，保存后只显示末 4 位。</div>
        <div>渠道限制：每渠道每分钟最多 20 条（企业微信机器人官方限制同为每分钟约 20 条），超出丢弃；钉钉机器人若开启了「自定义关键词」安全设置，关键词需包含 “TiDB”（每条消息标题都含「[TiDB 运维]」）；相同事件 10～30 分钟内去重，失败自动重试 3 次。</div></div>
      <div class="hr"></div>
      <h3 style="font-size:14px;margin:0 0 6px">最近投递记录 <span class="small muted">最近 ${(n.log || []).length} 条 · 随本页自动刷新</span></h3>
      <div class="tbl-wrap" style="max-height:320px;overflow:auto"><table><thead><tr><th>时间</th><th>渠道</th><th>事件</th><th>标题</th><th>结果</th><th>尝试</th><th>错误</th></tr></thead><tbody>${logRows()}</tbody></table></div></div>`;
  }

  /* ---- 新建 / 编辑抽屉 ---- */
  function typeHintHtml(t) {
    const ti = typeInfo(t);
    return `<div class="small muted">示例：${(ti.hostExamples || []).map(x => `<span class="mono">${E(x)}</span>`).join('<br>')}</div>
      <div class="small muted">${t === 'other' ? '「其他」没有官方域名：主机必须由管理员在启动参数 <span class="mono">--notify-allow-hosts</span> 里加入；平台以 JSON POST 发送（字段 source / event / level / title / env / cluster / lines / link / time / text），配置了签名密钥时附带 X-TiDBOps-Timestamp 与 X-TiDBOps-Signature（sha256=HMAC）；任何 2xx 视为成功。' : '只允许官方域名 + 管理员配置的额外主机；'}URL 保存后只显示末 4 位。</div>`;
  }
  function ontOpen(id) {
    const n = ON.n; if (!n) return;
    const c = id ? (n.channels || []).find(x => x.id === id) : null;
    if (id && !c) { toast('渠道不存在，请刷新'); return; }
    const type = c ? c.type : ((n.types || [])[0] || {}).type || 'wecom', sel = new Set(c ? c.events || [] : []);
    ON.form = {id: c ? c.id : '', hasSecret: !!(c && c.hasSecret)};
    const groups = {}; (n.events || []).forEach(e => (groups[evGroup(e.key)] = groups[evGroup(e.key)] || []).push(e));
    openDrawer(c ? '编辑通知渠道' : '添加通知渠道', `
      <div class="field"><label>渠道类型${c ? '（创建后不可更改）' : ''}</label><select id="ontType" data-nc="ont-type" ${c ? 'disabled' : ''}>${(n.types || []).map(t => `<option value="${E(t.type)}" ${t.type === type ? 'selected' : ''}>${E(t.label)}</option>`).join('')}</select></div>
      <div class="field"><label>名称</label><input type="text" id="ontName" maxlength="40" value="${E(c ? c.name : '')}" placeholder="例如 DBA 值班群"></div>
      <div class="field"><label>Webhook URL</label><input type="text" id="ontUrl" autocomplete="off" spellcheck="false" placeholder="${c ? '留空表示不修改（当前 ' + E(c.url) + '）' : 'https://…'}"><div id="ontUrlHint">${typeHintHtml(type)}</div></div>
      <div class="field" id="ontSecBox"></div>
      <div class="field"><label>订阅事件 <span class="muted">（不勾选 = 订阅全部事件）</span>
        <span style="float:right"><a data-na="ont-evall">全选</a> · <a data-na="ont-evnone">清空</a></span></label>
        ${Object.entries(groups).map(([g, list]) => `<div style="margin:4px 0"><b class="small">${E(g)}</b><div style="display:flex;flex-wrap:wrap;gap:4px 16px;margin-top:2px">${list.map(e => `<label class="small"><input type="checkbox" data-ev="${E(e.key)}" ${sel.has(e.key) ? 'checked' : ''}> ${E(e.label)}</label>`).join('')}</div></div>`).join('')}</div>
      <div class="field"><label><input type="checkbox" id="ontEn" ${!c || c.enabled ? 'checked' : ''}> 启用该渠道</label></div>
      <div class="notice info"><span>ℹ</span><div>消息包含环境 / 集群 / 变更标题 / 风险 / 申请人 / 原因，不含命令输出与凭据。保存后可点「发送测试」验证连通性。</div></div>
      <div id="ontErr" class="small" style="color:var(--bad);min-height:18px;word-break:break-all"></div>
      <div class="sticky-actions"><button class="btn primary" data-na="ont-save">保存</button><button class="btn" data-close="1">取消</button></div>`);
    ontSecret(type);
  }
  function ontSecret(type) {
    const box = $('#ontSecBox'); if (!box) return;
    const f = ON.form || {}, ti = typeInfo(type), none = type === 'wecom' || type === 'slack';
    box.innerHTML = `<label>${none ? '加签密钥' : E(ti.secretLabel)}</label>
      <input type="password" id="ontSecret" autocomplete="new-password" ${none ? `disabled placeholder="${type === 'slack' ? 'Slack Incoming Webhook ' : '企业微信'}无签名校验，无需填写"` : `placeholder="${f.hasSecret ? '已设置（留空不修改）' : type === 'other' ? '可选：填写后每次请求附带 HMAC-SHA256 签名头' : '没有开启加签 / 签名校验可留空'}"`}>
      ${!none && f.hasSecret ? '<label class="small"><input type="checkbox" id="ontClear"> 清除已设置的密钥</label>' : ''}`;
    const u = $('#ontUrlHint'); if (u) u.innerHTML = typeHintHtml(type);
  }
  async function ontSave(btn) {
    const f = ON.form, err = $('#ontErr'); if (!f) return;
    const name = $('#ontName').value.trim(), url = $('#ontUrl').value.trim(), type = $('#ontType').value;
    if (!name) { err.textContent = '请填写渠道名称'; return; }
    if (!f.id && !url) { err.textContent = '请填写 Webhook URL'; return; }
    const body = {type, name, events: $$('#drBody [data-ev]').filter(x => x.checked).map(x => x.dataset.ev), enabled: $('#ontEn').checked};
    if (url) body.url = url;
    const sec = $('#ontSecret');
    if (sec && !sec.disabled && sec.value) body.secret = sec.value;
    if ($('#ontClear') && $('#ontClear').checked) body.clearSecret = true;
    err.textContent = ''; btn.disabled = true;
    const r = await opApi('POST', f.id ? `/api/ops/notify/channels/${encodeURIComponent(f.id)}` : '/api/ops/notify/channels', body);
    btn.disabled = false;
    if (r.code === 200) { closeDrawer(); toast(f.id ? '渠道已更新' : '渠道已添加'); await ontLoad(); }
    else err.textContent = r.data.error || ('保存失败（' + r.code + '）');
  }
  async function ontTest(id) {
    ON.busy[id] = true; ontPaint();
    const r = await opApi('POST', `/api/ops/notify/channels/${encodeURIComponent(id)}/test`, {});
    delete ON.busy[id];
    const res = r.code === 200 ? {ok: !!r.data.ok, error: r.data.error || ''} : {ok: false, error: r.data.error || ('请求失败（' + r.code + '）')};
    res.at = Date.now(); ON.test[id] = res;
    toast(res.ok ? '测试消息已发送' : '测试失败：' + clip(res.error, 70));
    ontPaint(); await ontLoad();
  }
  async function ontDelete(id) {
    const r = await opApi('POST', `/api/ops/notify/channels/${encodeURIComponent(id)}/delete`, {});
    if (r.code === 200) { delete ON.test[id]; toast('渠道已删除'); } else toast(r.data.error || '删除失败');
    await ontLoad();
  }
  async function ontEnable(id, on) {
    const r = await opApi('POST', `/api/ops/notify/channels/${encodeURIComponent(id)}/enabled`, {enabled: on});
    toast(r.code === 200 ? (on ? '渠道已启用' : '渠道已停用') : (r.data.error || '操作失败'));
    await ontLoad();
  }

  /* ======================================================================
     变更窗口规则
     ====================================================================== */
  const WMODE = {block: ['bad', '禁止'], avoid: ['warn', '不建议'], prefer: ['ok', '推荐']};
  const WRISK = {low: '低', mid: '中', high: '高'};
  const WDAY = [[1, '周一'], [2, '周二'], [3, '周三'], [4, '周四'], [5, '周五'], [6, '周六'], [0, '周日']];
  const WKIND = [['inst.op', '实例操作'], ['scale.out', '扩容'], ['scale.in', '缩容'], ['config.set', '配置变更'], ['maint.enter', '进入维护'], ['maint.exit', '结束维护'], ['maint.extend', '延长维护'],
    ['backup.run', '备份'], ['backup.restore', '恢复'], ['cluster.restart-role', '滚动重启'], ['cluster.upgrade', '滚动升级']];
  const kindName = k => (WKIND.find(x => x[0] === k) || [0, k])[1];
  const HM = /^([01]?\d|2[0-3]):([0-5]\d)$/;
  const hmMin = s => { s = String(s || '').trim(); if (s === '24:00') return 1440; const m = HM.exec(s); return m ? +m[1] * 60 + +m[2] : null; };
  const hmNorm = s => { const n = hmMin(s); return n === 1440 ? '24:00' : `${p2(Math.floor(n / 60))}:${p2(n % 60)}`; };

  function daysText(days) {
    if (!days || !days.length || days.length >= 7) return '每天';
    const ord = d => (d + 6) % 7, nm = {0: '周日', 1: '周一', 2: '周二', 3: '周三', 4: '周四', 5: '周五', 6: '周六'};
    const ds = days.slice().sort((a, b) => ord(a) - ord(b)), parts = [];
    for (let i = 0; i < ds.length;) {
      let j = i; while (j + 1 < ds.length && ord(ds[j + 1]) === ord(ds[j]) + 1) j++;
      if (j - i >= 2) parts.push(nm[ds[i]] + '至' + nm[ds[j]]); else for (let k = i; k <= j; k++) parts.push(nm[ds[k]]);
      i = j + 1;
    }
    return parts.join('、');
  }

  async function onwLoad(force) {
    if (!OP.on || !OP.me || ON.wLoading) return;
    ON.wLoading = true;
    const r = await opApi('GET', '/api/ops/settings');
    ON.wLoading = false;
    if (r.code === 200) {
      const s = r.data.settings || {};
      ON.w = Array.isArray(s.windows) ? s.windows : []; ON.wCustom = !!r.data.windowsCustom; ON.wDefaults = r.data.windowDefaults || []; ON.wErr = '';
    } else ON.wErr = r.data.error || ('读取失败（' + r.code + '）');
    onwPaint();
  }
  function onwPaint() {
    const el = $('#onwBox'); if (!el || !hashIs('settings')) return;
    const h = onwHtml();
    if (el._h === h) return;
    el._h = h; el.innerHTML = h;
  }

  function onwHtml() {
    const can = canEdit(), head = `<h2>变更窗口规则 <span class="sub">${window.CL && CL.active() ? "集群 " + opEsc(CL.name()) + " · " : ""}${ON.w ? ON.w.length + ' 条 · ' + (ON.wCustom && JSON.stringify(ON.w) !== JSON.stringify(ON.wDefaults) ? '已自定义' : '默认规则') : ''}</span>`;
    if (!ON.w) return `<div class="card">${head}</h2>${ON.wErr ? `<div class="notice bad"><span>✖</span><div>${E(ON.wErr)}</div></div>` : '<div class="empty">正在读取…</div>'}</div>`;
    const rows = ON.w.map(w => {
      const m = WMODE[w.mode] || ['', w.mode], cross = hmMin(w.from) > hmMin(w.to) && hmMin(w.to) !== 1440;
      const risks = (w.risks || []).length ? w.risks.map(r => tg(WRISK[r] || r)).join(' ') : '<span class="muted">全部</span>';
      const kinds = (w.kinds || []).length ? `<div class="small muted">${E(w.kinds.map(kindName).join('、'))}</div>` : '';
      return `<tr class="${w.enabled ? '' : 'muted'}"><td>${E(w.name)}${w.note ? `<div class="small muted">${E(w.note)}</div>` : ''}</td><td class="small">${E(daysText(w.days))}</td>
        <td class="mono small" style="white-space:nowrap">${E(w.from)}–${E(w.to)}${cross ? ' <span class="tag" title="跨午夜：按起点所在日判断星期">跨午夜</span>' : ''}</td>
        <td>${tg(m[1], m[0])}</td><td>${risks}${kinds}</td>
        <td><label class="small"><input type="checkbox" data-nc="onw-en" data-id="${E(w.id)}" ${w.enabled ? 'checked' : ''} ${can ? '' : 'disabled'}> ${w.enabled ? '启用' : '停用'}</label></td>
        <td>${can ? `<div class="acts"><button class="btn sm" data-na="onw-edit" data-id="${E(w.id)}">编辑</button><button class="btn sm danger" data-na="onw-del" data-id="${E(w.id)}">删除</button></div>` : ''}</td></tr>`;
    }).join('') || '<tr><td colspan="7" class="empty">没有窗口规则：任何时间都不做窗口检查。</td></tr>';
    return `<div class="card">${head}<span class="right">${can ? '<button class="btn sm" data-na="onw-reset">恢复默认</button><button class="btn primary sm" data-na="onw-new">＋ 新增规则</button>' : ''}</span></h2>
      ${can ? '' : '<div class="notice warn"><span>⚠</span><div>你没有「修改平台设置」（settings.edit）权限，窗口规则为只读。</div></div>'}
      ${ON.wErr ? `<div class="notice bad"><span>✖</span><div>${E(ON.wErr)}</div></div>` : ''}
      <div class="tbl-wrap"><table><thead><tr><th>名称</th><th>星期</th><th>时间</th><th>模式</th><th>适用风险 / 类型</th><th>状态</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>
      <div class="small muted" style="margin-top:8px;display:grid;gap:3px">
        <div><b>禁止</b>：该时段内不允许提交 / 执行变更（紧急变更除外）；<b>不建议</b>：提交时给出警告，并在推荐时间里降分；<b>推荐</b>：推荐时间里加分。</div>
        <div>时间按服务器本地时区；跨午夜规则（如 22:00–06:00）按起点所在日判断星期；「适用风险 / 类型」为空表示全部。规则会在「变更排期」周日历上着色，并用于提交变更时的窗口检查与推荐时间。</div></div></div>`;
  }

  /* 读取最新规则 → 修改 → 写回；返回 {ok, error} */
  async function onwMutate(mut) {
    await onwLoad(true);
    if (!ON.w) return {ok: false, error: ON.wErr || '读取规则失败'};
    const next = mut(ON.w.map(w => JSON.parse(JSON.stringify(w))));
    if (!next) return {ok: false, error: '规则已变化，请关闭后刷新重试'};
    const r = await opApi('POST', '/api/ops/settings', {windows: next});
    if (r.code !== 200) return {ok: false, error: r.data.error || ('保存失败（' + r.code + '）')};
    await onwLoad(true);
    if (typeof oppLoadSettings === 'function') oppLoadSettings(true);   // 让排期页的窗口着色同步
    return {ok: true};
  }

  function onwOpen(id) {
    const w = id ? (ON.w || []).find(x => x.id === id) : null;
    if (id && !w) { toast('规则不存在，请刷新'); return; }
    const d = w || {name: '', days: [], from: '22:00', to: '06:00', mode: 'avoid', risks: [], kinds: [], enabled: true, note: ''};
    ON.wForm = {id: w ? w.id : ''};
    const kinds = WKIND.slice(); (d.kinds || []).forEach(k => { if (!kinds.some(x => x[0] === k)) kinds.push([k, k]); });
    openDrawer(w ? '编辑窗口规则' : '新增窗口规则', `
      <div class="field"><label>名称</label><input type="text" id="onwName" maxlength="40" value="${E(d.name)}" placeholder="例如 业务高峰"></div>
      <div class="field"><label>星期 <span class="muted">（不勾选 = 每天）</span></label><div style="display:flex;flex-wrap:wrap;gap:4px 14px">${WDAY.map(([v, n]) => `<label class="small"><input type="checkbox" data-wd="${v}" ${(d.days || []).includes(v) ? 'checked' : ''}> ${n}</label>`).join('')}</div></div>
      <div class="grid g2"><div class="field"><label>开始（HH:MM）</label><input type="text" id="onwFrom" maxlength="5" value="${E(d.from)}" placeholder="22:00"></div>
        <div class="field"><label>结束（HH:MM，可小于开始表示跨午夜；24:00 表示当天结束）</label><input type="text" id="onwTo" maxlength="5" value="${E(d.to)}" placeholder="06:00"></div></div>
      <div class="field"><label>模式</label><select id="onwMode">${Object.entries(WMODE).map(([k, v]) => `<option value="${k}" ${d.mode === k ? 'selected' : ''}>${v[1]}（${{block: '禁止执行，紧急变更除外', avoid: '警告并在推荐里降分', prefer: '推荐里加分'}[k]}）</option>`).join('')}</select></div>
      <div class="field"><label>适用风险 <span class="muted">（不勾选 = 全部风险）</span></label><div style="display:flex;gap:14px">${Object.entries(WRISK).map(([k, n]) => `<label class="small"><input type="checkbox" data-rk="${k}" ${(d.risks || []).includes(k) ? 'checked' : ''}> ${n}风险</label>`).join('')}</div></div>
      <details class="field" ${(d.kinds || []).length ? 'open' : ''}><summary class="small" style="cursor:pointer">限定变更类型 <span class="muted">（默认全部类型）</span></summary>
        <div style="display:flex;flex-wrap:wrap;gap:4px 14px;margin-top:6px">${kinds.map(([k, n]) => `<label class="small"><input type="checkbox" data-kd="${E(k)}" ${(d.kinds || []).includes(k) ? 'checked' : ''}> ${E(n)}</label>`).join('')}</div></details>
      <div class="field"><label>备注（可选）</label><input type="text" id="onwNote" maxlength="200" value="${E(d.note || '')}"></div>
      <div class="field"><label><input type="checkbox" id="onwEn" ${d.enabled ? 'checked' : ''}> 启用该规则</label></div>
      <div id="onwErr" class="small" style="color:var(--bad);min-height:18px"></div>
      <div class="sticky-actions"><button class="btn primary" data-na="onw-save">保存</button><button class="btn" data-close="1">取消</button></div>`);
  }
  async function onwSave(btn) {
    const f = ON.wForm, err = $('#onwErr'); if (!f) return;
    const name = $('#onwName').value.trim(), from = $('#onwFrom').value.trim(), to = $('#onwTo').value.trim();
    if (!name) { err.textContent = '请填写规则名称'; return; }
    const a = hmMin(from), b = hmMin(to);
    if (a === null || a === 1440) { err.textContent = '开始时间格式应为 HH:MM（00:00–23:59）'; return; }
    if (b === null) { err.textContent = '结束时间格式应为 HH:MM（00:00–24:00）'; return; }
    if (a === b) { err.textContent = '开始与结束时间不能相同（全天请用 00:00–24:00）'; return; }
    const rule = {id: f.id, name, days: $$('#drBody [data-wd]').filter(x => x.checked).map(x => +x.dataset.wd), from: hmNorm(from), to: hmNorm(to), mode: $('#onwMode').value,
      risks: $$('#drBody [data-rk]').filter(x => x.checked).map(x => x.dataset.rk), kinds: $$('#drBody [data-kd]').filter(x => x.checked).map(x => x.dataset.kd),
      enabled: $('#onwEn').checked, note: $('#onwNote').value.trim()};
    err.textContent = ''; btn.disabled = true;
    const r = await onwMutate(list => {
      if (!f.id) { delete rule.id; list.push(rule); return list; }
      const i = list.findIndex(x => x.id === f.id); if (i < 0) return null; list[i] = rule; return list;
    });
    btn.disabled = false;
    if (r.ok) { closeDrawer(); toast(f.id ? '规则已更新' : '规则已添加'); } else err.textContent = r.error;
  }

  /* ======================================================================
     包装设置页
     ====================================================================== */
  const _st = pages.settings;
  const HIDE = new Set(['notify', 'win.backup', 'win.peak', 'win.good']);   // 原型里的占位项，由后端真实的规则 / 渠道替代
  pages.settings = {
    ..._st,
    render() {
      if (!OP.on) return _st.render.call(this);
      const saved = ST_SETS.slice(); ST_SETS.length = 0; saved.filter(s => !HIDE.has(s.k)).forEach(s => ST_SETS.push(s));
      let h; try { h = _st.render.call(this); } finally { ST_SETS.length = 0; saved.forEach(s => ST_SETS.push(s)); }
      return h + `<div class="grid" style="margin-top:16px"><div id="ontBox">${ontHtml()}</div><div id="onwBox">${onwHtml()}</div></div>`;
    },
    init() {
      _st.init.call(this);
      if (!OP.on) return;
      ontLoad(); onwLoad();
    },
  };

  /* ======================================================================
     事件委托
     ====================================================================== */
  const CLICK = {
    'ont-new': () => ontOpen(''),
    'ont-edit': d => ontOpen(d.id),
    'ont-save': (d, el) => ontSave(el),
    'ont-test': d => ontTest(d.id),
    'ont-del': d => { const c = ((ON.n && ON.n.channels) || []).find(x => x.id === d.id); oppConfirm('删除通知渠道', `确认删除通知渠道 <b>${E(c ? c.name : d.id)}</b>？删除后不再向该群推送消息，Webhook 配置无法找回。`, '确认删除', () => ontDelete(d.id), true); },
    'ont-evall': () => $$('#drBody [data-ev]').forEach(x => { x.checked = true; }),
    'ont-evnone': () => $$('#drBody [data-ev]').forEach(x => { x.checked = false; }),
    'onw-new': () => onwOpen(''),
    'onw-edit': d => onwOpen(d.id),
    'onw-save': (d, el) => onwSave(el),
    'onw-del': d => {
      const w = (ON.w || []).find(x => x.id === d.id);
      oppConfirm('删除窗口规则', `确认删除规则 <b>${E(w ? w.name : d.id)}</b>（${w ? E(daysText(w.days)) + ' ' + E(w.from) + '–' + E(w.to) : ''}）？`, '确认删除', async () => {
        const r = await onwMutate(list => { const i = list.findIndex(x => x.id === d.id); if (i < 0) return null; list.splice(i, 1); return list; });
        toast(r.ok ? '规则已删除' : r.error);
      }, true);
    },
    'onw-reset': () => oppConfirm('恢复默认窗口规则', '将用系统默认规则（备份窗口 / 业务高峰 / 夜间低峰）替换当前全部规则，自定义的规则会丢失。', '恢复默认', async () => {
      await onwLoad(true);
      const r = await opApi('POST', '/api/ops/settings', {windows: ON.wDefaults});
      if (r.code === 200) { await onwLoad(true); if (typeof oppLoadSettings === 'function') oppLoadSettings(true); toast('已恢复默认规则'); } else toast(r.data.error || '恢复失败');
    }, false),
  };
  document.addEventListener('click', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
    const f = CLICK[el.dataset.na]; if (f) f(el.dataset, el, e);
  });
  document.addEventListener('change', e => {
    if (!OP.on) return;
    const el = e.target.closest('[data-nc]'); if (!el) return; const nc = el.dataset.nc;
    if (nc === 'ont-type') ontSecret(el.value);
    else if (nc === 'ont-en') ontEnable(el.dataset.id, el.checked);
    else if (nc === 'onw-en') {
      const id = el.dataset.id, on = el.checked;
      onwMutate(list => { const w = list.find(x => x.id === id); if (!w) return null; w.enabled = on; return list; }).then(r => { toast(r.ok ? (on ? '规则已启用' : '规则已停用') : r.error); if (!r.ok) onwPaint(); });
    }
  });

  /* 设置页数据随后端刷新钩子自动更新（投递记录、他人修改的规则） */
  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || h !== 'settings' || ctx.open) return;
    ontLoad(); onwLoad();
  });
})();
