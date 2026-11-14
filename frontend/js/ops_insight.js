// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_insight.js
// @time: 2026/10/28

/* 只读后端下的「日常运维」：LIVE.on 且 !OP.on（后端没有 --data-dir，没有运维模式）时生效。
   巡检 → GET /api/inspect；参数管理 → GET /api/params；备份与恢复 / 变更审计 → 明确提示需要运维模式（不显示模拟数据）。
   做法：包装（叠在 ops_pages.js 的包装之上）pages.ops 的 inspect / backup / param / audit，不满足条件时原样走下层实现，
   所以静态托管与运维模式的行为都不变。事件用 data-na="oi-*" + 文档级委托（CSP 禁止内联事件）。 */
(function () {
  'use strict';
  const _ops = pages.ops;
  const active = () => LIVE.on && !OP.on;
  const E = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
  const st = {report: null, loading: false, err: '', params: null, ploading: false, perr: ''};
  const stillOn = tab => active() && (location.hash.slice(1) || 'overview') === 'ops' && typeof opsState !== 'undefined' && opsState.tab === tab && $('#opBody');

  const needOps = (title, what) => `<div class="card"><h2>${title}</h2>
    <div class="notice info"><span>ℹ</span><div><b>需要启用运维模式（后端加 --data-dir）。</b><br>当前后端是只读模式：${what}</div></div>
    <div class="small muted">启用运维模式后，这里会显示服务端记录的真实数据；只读模式不会展示模拟数据，以免被误认为集群的真实状态。</div></div>`;

  /* ---------- 巡检 ---------- */
  const IN_GROUPS = [['bad', '异常', 'bad'], ['warn', '警告', 'warn'], ['na', '未采集 / 不适用', ''], ['ok', '通过', 'ok']];
  function reportHtml(r) {
    const items = r.items || [], when = r.time ? new Date(r.time).toLocaleString() : '';
    const sum = `<div class="row" style="margin-bottom:10px"><span class="tag ok">通过 ${r.ok || 0}</span><span class="tag warn">警告 ${r.warn || 0}</span><span class="tag bad">异常 ${r.bad || 0}</span><span class="tag">未采集 ${r.na || 0}</span>
      <span class="small muted">${E(r.cluster || '')} ${E(r.version || '')} · 巡检于 ${E(when)}</span></div>`;
    const groups = IN_GROUPS.map(([k, label, cls]) => {
      const list = items.filter(x => x.r === k); if (!list.length) return '';
      return `<h3 style="font-size:13px;margin:14px 0 4px">${label} <span class="small muted">${list.length} 项</span></h3>` + list.map(c =>
        `<div class="row" style="padding:6px 0;border-bottom:1px solid var(--border);${k === 'na' ? 'opacity:.65' : ''}"><span class="tag ${cls}" style="width:48px;text-align:center">${{ok: '通过', warn: '警告', bad: '异常', na: '未采集'}[k]}</span><span class="tag">${E(c.g)}</span><b>${E(c.n)}</b><span class="muted small">${E(c.d)}</span></div>`).join('');
    }).join('');
    return sum + (groups || '<div class="empty">没有巡检项</div>');
  }
  function inspectLoad() {
    if (st.loading) return; st.loading = true; st.err = '';
    liveGet('/api/inspect').then(r => { st.report = r; }, e => { st.err = e.message; }).finally(() => { st.loading = false; if (stillOn('inspect')) pages.ops.inspect(); });
  }
  function inspect() {
    const rep = st.report;
    $('#opBody').innerHTML = `<div class="card"><h2>集群巡检 <span class="sub">${rep ? (rep.items || []).length + ' 项' : '服务端采集'}</span>
      <span class="right"><button class="btn primary" data-na="oi-inspect" ${st.loading ? 'disabled' : ''}>${st.loading ? '巡检中…' : '重新巡检'}</button><button class="btn" data-print="1">导出 / 打印</button></span></h2>
      <div class="small muted" style="margin-bottom:8px">只读巡检，基于监控与集群接口采集。<b>主机检查需运维模式</b>（tiup cluster check 需要后端的执行器）。</div>
      ${st.err ? `<div class="notice bad"><span>✖</span><div>巡检失败：${E(st.err)}</div></div>` : ''}
      <div id="inOut">${st.loading && !rep ? '<div class="empty">正在巡检…</div>' : rep ? reportHtml(rep) : st.err ? '' : '<div class="empty">正在读取…</div>'}</div></div>`;
    if (!rep && !st.loading && !st.err) inspectLoad();
  }

  /* ---------- 参数管理 ---------- */
  function paramsLoad() {
    if (st.ploading) return; st.ploading = true; st.perr = '';
    liveGet('/api/params').then(d => { st.params = d.params || []; }, e => { st.perr = e.message; }).finally(() => { st.ploading = false; if (stillOn('param')) pages.ops.param(); });
  }
  function param() {
    const ps = st.params;
    const stTag = p => p.state === 'warn' ? '<span class="tag warn">建议调整</span>' : p.state === 'ok' ? '<span class="tag ok">符合</span>' : '<span class="tag">未采集</span>';
    $('#opBody').innerHTML = `<div class="card"><h2>关键参数建议 <span class="sub">当前值取自集群 vs 推荐</span><span class="right"><button class="btn" data-na="oi-params" ${st.ploading ? 'disabled' : ''}>${st.ploading ? '读取中…' : '重新读取'}</button></span></h2>
      ${st.perr ? `<div class="notice bad"><span>✖</span><div>读取失败：${E(st.perr)}</div></div>` : ''}
      ${ps ? `<div class="tbl-wrap"><table><thead><tr><th>范围</th><th>参数</th><th>当前值</th><th>推荐</th><th>状态</th><th>原因</th><th></th></tr></thead><tbody>
      ${ps.map(p => `<tr><td><span class="tag">${E(p.scope)}</span></td><td class="mono small">${E(p.name)}</td><td${p.state === 'warn' ? ' style="color:var(--warn)"' : ''}>${E(p.cur)}${p.drift ? ' <span class="tag bad">配置漂移</span>' : ''}</td><td>${E(p.rec)}</td><td>${stTag(p)}</td><td class="small muted">${E(p.why)}</td>
        <td>${p.set ? `<button class="btn sm" data-copy="${E(p.set)}">复制命令</button>` : ''}</td></tr>`).join('') || '<tr><td colspan="7" class="muted">没有参数</td></tr>'}</tbody></table></div>`
      : st.perr ? '' : '<div class="empty">正在读取…</div>'}
      <div class="small muted" style="margin-top:8px">配置漂移 = 各实例取值不一致。推荐值来自官方调优建议，仅作参考，请结合硬件规格与版本判断。修改参数请使用「节点管理 → 配置变更」（需运维模式才会真正下发），或自行执行上面的命令。</div></div>`;
    if (!ps && !st.ploading && !st.perr) paramsLoad();
  }

  pages.ops = {
    ..._ops,
    inspect() { return active() ? inspect.call(this) : _ops.inspect.call(this); },
    param() { return active() ? param.call(this) : _ops.param.call(this); },
    backup() { return active() ? void ($('#opBody').innerHTML = needOps('备份与恢复', '备份记录、备份 / 恢复操作都由运维模式的服务端提供。')) : _ops.backup.call(this); },
    audit() { return active() ? void ($('#opBody').innerHTML = needOps('变更审计', '变更与操作的审计记录由运维模式的服务端保存。')) : _ops.audit.call(this); },
  };

  document.addEventListener('click', e => {
    const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
    if (el.dataset.na === 'oi-inspect') { st.report = null; if (stillOn('inspect')) pages.ops.inspect(); }
    else if (el.dataset.na === 'oi-params') { st.params = null; if (stillOn('param')) pages.ops.param(); }
  });
})();
