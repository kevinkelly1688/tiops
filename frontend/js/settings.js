// @author: kevinkelly
// @license: Apache License 2.0
// @file: settings.js
// @time: 2026/10/28

/* 环境与设置（对应路线图 B8）：自动探测中控机 / 集群环境 + 用户可覆盖；用户自定义设置带默认值；版本兼容矩阵。
   原型中的“探测结果”来自模拟的 fixture；真实实现通过 Source 接口 live / replay 读取只读白名单命令的输出。 */
const ST_SUPPORTED = ['7.5', '8.1', '8.5'];
const ST_VERSIONS = ['v6.5.9', 'v7.1.5', 'v7.5.3', 'v8.1.1', 'v8.5.2', 'v8.5.3', 'v9.0.0'];
const stState = {ovr: {}, set: {}, detectedAt: Date.now() - 3 * 60000, simVer: 'v8.5.2', lastVer: 'v8.5.2', running: false, shown: 0, edit: null, drift: ''};
const stCnt = r => NODES.filter(n => n.type === r).length;
/* 真实数据模式下优先使用后端探测值；未探测到返回 null */
const stDet = it => { if (typeof LIVE !== 'undefined' && LIVE.on && LIVE.env[it.k]) { const e = LIVE.env[it.k]; return e.detected ? e.value : null; } return it.det(); };

/* 可自动探测的项：k / 分组 / 名称 / 探测值（null = 未探测到）/ 来源命令 */
const ST_ITEMS = [
  {k: 'ctl.host', g: '中控机', label: '主机', det: () => 'ctl-01（10.0.0.5）', cmd: 'hostname; hostname -I'},
  {k: 'ctl.os', g: '中控机', label: '操作系统', det: () => 'Rocky Linux 9.3 (Blue Onyx)', cmd: 'cat /etc/os-release'},
  {k: 'ctl.kernel', g: '中控机', label: '内核', det: () => '5.14.0-362.el9.x86_64', cmd: 'uname -r'},
  {k: 'ctl.res', g: '中控机', label: 'CPU / 内存', det: () => '8 核 / 32 GiB', cmd: 'nproc; free -m'},
  {k: 'ctl.tz', g: '中控机', label: '时区', det: () => 'Asia/Shanghai（UTC+08:00）', cmd: 'timedatectl'},
  {k: 'ctl.user', g: '中控机', label: 'SSH 部署用户', det: () => 'tidb', cmd: "grep user ~/.tiup/storage/cluster/clusters/<name>/meta.yaml"},
  {k: 'tiup.ver', g: 'tiup', label: 'tiup 版本', det: () => 'v1.16.1', cmd: 'tiup --version'},
  {k: 'tiup.cluster', g: 'tiup', label: 'tiup cluster 组件版本', det: () => 'v1.16.1', cmd: 'tiup cluster --version'},
  {k: 'cl.list', g: '集群', label: '中控机管理的集群', det: () => `${CLUSTER}、stg-tidb-01、dev-tidb-01`, cmd: 'tiup cluster list'},
  {k: 'cl.name', g: '集群', label: '接入的集群', det: () => CLUSTER, cmd: 'tiup cluster list（多个集群时由用户选择）'},
  {k: 'cl.ver', g: '集群', label: 'TiDB 版本', det: () => stState.simVer, cmd: `tiup cluster display ${CLUSTER}（Cluster version）；只读账号 select tidb_version() 交叉验证`},
  {k: 'cl.inst', g: '集群', label: '实例规模', det: () => `TiDB ${stCnt('tidb')} · PD ${stCnt('pd')} · TiKV ${stCnt('tikv')} · TiFlash ${stCnt('tiflash')} · TiCDC ${stCnt('ticdc')}`, cmd: `tiup cluster display ${CLUSTER}`},
  {k: 'cl.region', g: '集群', label: 'Region 数（估）', det: () => '约 48,210', cmd: 'GET /pd/api/v1/stats/region'},
  {k: 'addr.pd', g: '组件地址', label: 'PD', det: () => 'http://10.0.1.21:2379', cmd: 'meta.yaml → pd_servers'},
  {k: 'addr.prom', g: '组件地址', label: 'Prometheus', det: () => 'http://10.0.1.31:9090', cmd: 'meta.yaml → monitoring_servers'},
  {k: 'addr.grafana', g: '组件地址', label: 'Grafana', det: () => 'http://10.0.1.31:3000', cmd: 'meta.yaml → grafana_servers'},
  {k: 'addr.alert', g: '组件地址', label: 'Alertmanager', det: () => null, cmd: 'meta.yaml → alertmanager_servers（该集群未部署）'},
];
/* 用户自定义项（探测不到，带默认值） */
const ST_SETS = [
  {k: 'auth', g: '账号与存储', label: '登录方式', type: 'select', opts: ['本地账号', 'LDAP', 'OIDC / 企业 SSO'], def: '本地账号', hint: '多人使用时建议接入统一身份'},
  {k: 'store', g: '账号与存储', label: '平台元数据存储', type: 'select', opts: ['SQLite（单机）', 'MySQL', 'TiDB'], def: 'SQLite（单机）', hint: '审批、排期、审计等平台自身数据；多实例部署需 MySQL / TiDB'},
  {k: 'notify', g: '通知与审计', label: '通知渠道', type: 'select', opts: ['关闭', '企业微信', '飞书', 'Lark', '钉钉', 'Slack', '其他'], def: '关闭', hint: '审批、到点提醒、失败告警'},
  {k: 'audit', g: '通知与审计', label: '审计保留期（天）', type: 'text', def: '180', hint: '审计日志只追加、不可被平台账号修改'},
  {k: 'win.backup', g: '变更窗口', label: '备份窗口（禁止变更）', type: 'text', def: '02:00–02:40', hint: '与每日全量备份错开'},
  {k: 'win.peak', g: '变更窗口', label: '业务高峰（高风险禁止）', type: 'text', def: '10:00–12:00, 19:00–23:00', hint: '可多段，逗号分隔'},
  {k: 'win.good', g: '变更窗口', label: '推荐低峰', type: 'text', def: '02:40–06:00', hint: '排期推荐窗口优先使用'},
  {k: 'win.freeze', g: '变更窗口', label: '变更冻结期', type: 'text', def: '（无）', hint: '如大促、春节保障期；此处只演示，排期页的冻结期来自示例数据'},
  {k: 'wr.test', g: '写操作开关', label: '测试环境允许写操作', type: 'select', opts: ['开启', '关闭'], def: '开启', hint: 'M3 起才会提供写操作'},
  {k: 'wr.stage', g: '写操作开关', label: '预发环境允许写操作', type: 'select', opts: ['开启', '关闭'], def: '关闭', hint: ''},
  {k: 'wr.prod', g: '写操作开关', label: '生产环境允许写操作', type: 'select', opts: ['开启', '关闭'], def: '关闭', hint: '安全基线：生产默认关闭，需显式开启并留痕', lock: true},
];
const stEff = it => { const o = stState.ovr[it.k], d = stDet(it); return o !== undefined ? {v: o, src: 'ovr'} : d !== null ? {v: d, src: 'det'} : {v: null, src: 'none'}; };
const stSrcTag = s => ({ovr: tag('用户自定义', 'info'), det: tag('自动探测', 'ok'), none: tag('未探测到', 'warn'), def: tag('默认值')}[s]);
const stVer = v => { const m = /^v?(\d+)\.(\d+)/.exec(v || ''); return m ? `${m[1]}.${m[2]}` : null; };
function stCompat(v) {
  const mm = stVer(v); if (!mm) return {lv: 'bad', t: '未获取到 TiDB 版本', write: false, feats: ['所有功能不可用，请填写版本']};
  if (ST_SUPPORTED.includes(mm)) return {lv: 'ok', t: `${v} 在支持矩阵内（v${ST_SUPPORTED.join(' / v')} LTS）`, write: true, feats: ['采集接口：' + mm + ' 适配集', '读取与诊断：可用', '写操作：可用（受功能开关与审批约束）']};
  const maj = +mm.split('.')[0];
  return {lv: 'warn', t: `${v} 不在已验证的支持矩阵内（${maj < 7 ? '版本偏旧' : maj >= 9 ? '版本较新，尚未适配' : '未验证'}）`, write: false, feats: ['采集接口：按最接近版本尝试，部分指标可能缺失', '读取与诊断：可用（带“未验证”标记）', '写操作：自动降级为不可用，只保留只读']};
}

pages.settings = {
  title: '环境与设置',
  render() {
    const secs = {}; ST_ITEMS.forEach(it => (secs[it.g] = secs[it.g] || []).push(it));
    const age = Math.round((Date.now() - stState.detectedAt) / 60000), cv = stCompat(stEff(ST_ITEMS.find(i => i.k === 'cl.ver')).v);
    const none = ST_ITEMS.filter(i => stEff(i).src === 'none').length, ovr = Object.keys(stState.ovr).length;
    return `
    <div class="card" style="margin-bottom:16px"><div class="row"><b>环境探测</b><span class="small muted">数据来源：<code>replay</code>（<code>fixtures/control-node/</code> 录制数据）· 生产部署后切换为 <code>live</code></span>
      <span style="margin-left:auto" class="row"><span class="small muted">上次探测：${age <= 0 ? '刚刚' : age + ' 分钟前'}</span><button class="btn primary" data-na="st-detect" ${stState.running ? 'disabled' : ''}>${stState.running ? '探测中…' : '重新探测'}</button></span></div>
      <div class="row" style="margin-top:8px"><span class="small">取值优先级：</span>${tag('用户自定义', 'info')}<span class="muted">＞</span>${tag('自动探测', 'ok')}<span class="muted">＞</span>${tag('默认值')}<span class="small muted">· 探测失败不阻塞，会降级到自定义 / 默认值</span></div>
      ${stState.drift ? `<div class="notice warn" style="margin-top:8px"><span>⚠</span><div>${stState.drift}</div></div>` : ''}
      <div id="stProg"></div></div>
    <div class="grid g-main" style="grid-template-columns:minmax(0,1fr) 360px">
      <div class="grid" style="align-content:start">
        ${Object.entries(secs).map(([g, list]) => `<div class="card"><h2>${g}${g === '集群' ? ` <span class="sub">${esc(CLUSTER)}</span>` : ''}</h2><div class="tbl-wrap"><table><thead><tr><th>项目</th><th>当前生效值</th><th>来源</th><th>探测命令</th><th></th></tr></thead><tbody>
          ${list.map(it => this.row(it)).join('')}</tbody></table></div></div>`).join('')}
        ${this.setsCard()}
      </div>
      <div class="grid" style="align-content:start">
        <div class="card"><h2>版本兼容性 <span class="sub">由探测到的 TiDB 版本决定</span></h2>
          <div class="notice ${cv.lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖'}[cv.lv]}</span><div>${cv.t}</div></div>
          <ul class="small" style="padding-left:18px;margin:6px 0">${cv.feats.map(f => `<li>${f}</li>`).join('')}</ul>
          <div class="field" style="margin-top:10px"><label>演示：模拟探测到的版本（模拟集群升级 / 换集群）</label><select data-nc="st-ver">${ST_VERSIONS.map(v => `<option ${stState.simVer === v ? 'selected' : ''}>${v}</option>`).join('')}</select></div>
          <div class="small muted">切换后会自动重新探测，并对比旧值，提示兼容性变化。</div></div>
        <div class="card"><h2>探测概况</h2><div class="kv"><span>可自动探测</span><span>${ST_ITEMS.length} 项</span><span>未探测到</span><span>${none} 项${none ? '（已降级）' : ''}</span><span>用户覆盖</span><span>${ovr} 项</span></div>
          ${none ? '<div class="notice warn" style="margin-top:8px"><span>⚠</span><div>Alertmanager 未探测到：告警相关页面会提示“未配置”。可在左侧手动填写地址。</div></div>' : ''}</div>
        <div class="card"><h2>设计要点</h2><ul class="small" style="padding-left:18px;margin:0;display:grid;gap:5px">
          <li>探测只用<b>只读白名单命令</b>，结果带来源命令和时间</li><li>每项可<b>覆盖</b>或<b>恢复自动</b>；与探测值不一致会提示</li><li>探测失败<b>不阻塞</b>，降级为自定义 / 默认值</li>
          <li>版本不在支持矩阵 → <b>写操作自动降级</b>为只读</li><li>集群升级后<b>自动重新探测</b>并对比旧值</li><li>多集群时由用户选择接入哪些</li><li>密码、密钥<b>不探测、不入库</b></li></ul></div>
      </div>
    </div>`;
  },
  row(it) {
    const e = stEff(it), det = stDet(it), editing = stState.edit === it.k, mismatch = e.src === 'ovr' && det !== null && String(det) !== String(e.v);
    return `<tr><td>${it.label}</td><td>${editing ? `<input type="text" id="stInp" value="${esc(e.v === null ? '' : e.v)}" style="width:100%">` : e.v === null ? '<span class="muted">—</span>' : `<span class="mono small">${esc(e.v)}</span>`}${mismatch ? `<div class="small" style="color:var(--warn)">与探测值不一致：${esc(det)}</div>` : ''}</td>
      <td>${stSrcTag(e.src)}</td><td class="small muted mono" style="max-width:240px;word-break:break-all">${esc(it.cmd)}</td>
      <td><div class="acts">${editing ? `<button class="btn sm primary" data-na="st-save" data-k="${it.k}">保存</button><button class="btn sm" data-na="st-cancel">取消</button>` : `<button class="btn sm" data-na="st-edit" data-k="${it.k}">覆盖</button>${e.src === 'ovr' ? `<button class="btn sm" data-na="st-auto" data-k="${it.k}">恢复自动</button>` : ''}`}</div></td></tr>`;
  },
  setsCard() {
    const secs = {}; ST_SETS.forEach(s => (secs[s.g] = secs[s.g] || []).push(s));
    return Object.entries(secs).map(([g, list]) => `<div class="card"><h2>${g} <span class="sub">用户自定义（探测不到）</span></h2><div class="tbl-wrap"><table><thead><tr><th>设置项</th><th>值</th><th>来源</th><th>说明</th></tr></thead><tbody>
      ${list.map(s => { const v = stState.set[s.k] !== undefined ? stState.set[s.k] : s.def, changed = String(v) !== String(s.def);
        return `<tr><td>${s.label}</td><td style="min-width:200px">${s.type === 'select' ? `<select data-nc="st-set" data-k="${s.k}" ${s.lock && v === '关闭' ? '' : ''}>${s.opts.map(o => `<option ${v === o ? 'selected' : ''}>${o}</option>`).join('')}</select>` : `<input type="text" data-ni="st-set" data-k="${s.k}" value="${esc(v)}" style="width:100%">`}</td>
          <td>${changed ? tag('用户自定义', 'info') : tag('默认值')}</td><td class="small muted">${s.hint}${s.lock && v === '开启' ? ' <b style="color:var(--bad)">已开启生产写操作，请确认审批与审计已就绪</b>' : ''}</td></tr>`; }).join('')}</tbody></table></div></div>`).join('');
  },
  detect(simChange) {
    if (stState.running) return; stState.running = true; stState.shown = 0; const old = stState.lastVer;
    const steps = ST_ITEMS.map(it => ({it, out: stDet(it)}));
    const rerender = () => { if (location.hash === '#settings') { $('#view').innerHTML = this.render(); this.paintProg(steps); } };
    rerender();
    const t = setInterval(() => {
      stState.shown++; this.paintProg(steps);
      if (stState.shown >= steps.length) {
        clearInterval(t); stState.running = false; stState.detectedAt = Date.now();
        const nv = stState.simVer; stState.drift = old !== nv ? `TiDB 版本由 ${old} 变为 ${nv}：${stCompat(nv).lv === 'ok' ? '仍在支持矩阵内，采集接口已切换到对应适配集' : '不在支持矩阵内，写操作已自动降级为只读'}。` : '';
        stState.lastVer = nv; rerender(); toast(simChange ? '已重新探测并对比版本变化' : '探测完成');
      }
    }, 130);
  },
  paintProg(steps) {
    const el = $('#stProg'); if (!el) return;
    el.innerHTML = `<div class="small" style="margin-top:10px;max-height:150px;overflow:auto;border:1px solid var(--border);border-radius:8px;padding:6px 10px;background:var(--panel2)">${steps.slice(0, stState.shown).map(s => `<div class="mono">$ ${esc(s.it.cmd)} → ${s.out === null ? '<span style="color:var(--warn)">未探测到</span>' : '<span style="color:var(--ok)">' + esc(s.out) + '</span>'}</div>`).join('')}</div>`;
  },
  init() {},
};
document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled) return; const d = el.dataset, rr = () => { $('#view').innerHTML = pages.settings.render(); };
  switch (d.na) {
    case 'st-detect': pages.settings.detect(false); break;
    case 'st-edit': stState.edit = d.k; rr(); { const i = $('#stInp'); if (i) { i.focus(); i.select(); } } break;
    case 'st-cancel': stState.edit = null; rr(); break;
    case 'st-save': { const v = $('#stInp').value.trim(); if (!v) { toast('值不能为空；如需回到自动请点“恢复自动”'); break; } stState.ovr[d.k] = v; stState.edit = null; toast('已覆盖，来源变为“用户自定义”'); rr(); break; }
    case 'st-auto': delete stState.ovr[d.k]; toast('已恢复为自动探测'); rr(); break;
  }
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc]'); if (!el) return;
  if (el.dataset.nc === 'st-ver') { stState.simVer = el.value; pages.settings.detect(true); }
  if (el.dataset.nc === 'st-set') { stState.set[el.dataset.k] = el.value; $('#view').innerHTML = pages.settings.render(); }
});
document.addEventListener('input', e => { const el = e.target.closest('[data-ni="st-set"]'); if (el) stState.set[el.dataset.k] = el.value; });
