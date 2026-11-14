// @author: kevinkelly
// @license: Apache License 2.0
// @file: app.js
// @time: 2026/10/28

/* TiDB 运维平台原型 —— 纯前端、无依赖。数据见 data.js（模拟）。 */
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = s => String(s == null ? '' : s).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const rng = seed => { let s = seed >>> 0; return () => (s = (s * 1664525 + 1013904223) >>> 0) / 4294967296; };
const fmt = n => n >= 1e8 ? (n / 1e8).toFixed(1) + ' 亿' : n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万' : String(Math.round(n * 100) / 100);
const sevText = {critical:'严重', warning:'警告', info:'提示'};
const sevTag = {critical:'bad', warning:'warn', info:'info'};
const stText = {up:'运行中', warn:'告警', down:'已停止', joining:'扩容中', offline:'下线中'};
const stCls = s => ({up:'ok', warn:'warn', down:'bad', joining:'info', offline:'info'}[s] || 'warn');
const docLink = (slug, text) => `<a class="ref" href="${DOC}${esc(slug)}" target="_blank" rel="noopener">${text || '官方文档 ↗'}</a>`;

let toastTimer;
function toast(msg) {
  const t = $('#toast'); t.textContent = msg; t.classList.add('show');
  clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove('show'), 1600);
}
function copyText(text, msg) {
  const done = () => toast(msg || '已复制到剪贴板');
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fallback);
  else fallback();
  function fallback() {
    const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta);
    ta.select(); try { document.execCommand('copy'); } catch (e) {} ta.remove(); done();
  }
}
const code = (text) => `<pre class="code"><button class="cp" data-copy="${esc(text)}">复制</button>${esc(text)}</pre>`;

/* ---------- 迷你图表 ---------- */
function series(seed, n, base, amp, spikeAt) {
  const r = rng(seed); const out = [];
  for (let i = 0; i < n; i++) {
    let v = base + (Math.sin(i / 6) * amp * .5) + (r() - .5) * amp;
    if (spikeAt && i >= spikeAt[0] && i <= spikeAt[1]) v += spikeAt[2] * (1 - Math.abs(i - (spikeAt[0] + spikeAt[1]) / 2) / ((spikeAt[1] - spikeAt[0]) / 2 + 1));
    out.push(Math.max(0, v));
  }
  return out;
}
function lineChart(list, {w = 560, h = 170, unit = '', labels = []} = {}) {
  const pad = {l:44, r:10, t:10, b:22};
  const all = list.flatMap(s => s.data); const max = (Math.max(...all) * 1.1) || 1;
  const n = list[0].data.length;
  const X = i => pad.l + (w - pad.l - pad.r) * i / (n - 1), Y = v => pad.t + (h - pad.t - pad.b) * (1 - v / max);
  let g = '';
  for (let k = 0; k <= 4; k++) {
    const v = max * k / 4, y = Y(v);
    g += `<line x1="${pad.l}" x2="${w - pad.r}" y1="${y}" y2="${y}" stroke="var(--border)"/><text x="${pad.l - 6}" y="${y + 4}" text-anchor="end" font-size="10" fill="var(--muted)">${fmt(v)}${unit}</text>`;
  }
  const xl = ['-60m','-45m','-30m','-15m','现在'];
  xl.forEach((t, i) => g += `<text x="${pad.l + (w - pad.l - pad.r) * i / 4}" y="${h - 6}" text-anchor="middle" font-size="10" fill="var(--muted)">${t}</text>`);
  const paths = list.map(s => `<path d="${s.data.map((v, i) => (i ? 'L' : 'M') + X(i).toFixed(1) + ' ' + Y(v).toFixed(1)).join('')}" fill="none" stroke="${s.color}" stroke-width="1.8"/>`).join('');
  const lg = list.length > 1 ? `<div class="legend">${list.map(s => `<span><i style="background:${s.color}"></i>${s.name}</span>`).join('')}</div>` : '';
  return `<svg viewBox="0 0 ${w} ${h}" role="img" aria-label="趋势图" style="width:100%;height:auto">${g}${paths}</svg>${lg}`;
}
const stackCol = c => String(c).startsWith('--') ? `var(${c})` : c;
function stackBar(items) {
  const total = items.reduce((a, b) => a + b[1], 0) || 1;
  return `<div class="bar">${items.map(([n, v, c]) => `<span style="width:${v / total * 100}%;background:${stackCol(c)}" title="${esc(n)} ${v}%">${v >= 7 ? `${esc(n)} ${v}%` : ''}</span>`).join('')}</div>
  <div class="legend">${items.map(([n, v, c]) => `<span><i style="background:${stackCol(c)}"></i>${esc(n)} ${v}%</span>`).join('')}</div>`;
}
/* DB Time 堆叠条：真实数据模式取快照的 dbTime（byType / byStage / byExec），空维度提示未采集；静态模式用原型的模拟数据 */
const DBT_PALETTE = ['--c1', '--c2', '--c3', '--c4', '#4aa3c9', '#c97b4a', '#7a8cff'];
function dbStack(dim) {
  if (!LIVE.on) return stackBar(DBTIME[dim]);
  const list = ((LIVE.snap.dbTime || {})[dim] || []).filter(x => x && x.pct > 0);
  if (!list.length) return '<div class="muted small">未采集（需要 Prometheus 的 DB Time 指标）</div>';
  let k = 0;
  return stackBar(list.map(x => [x.name, Math.round(x.pct * 10) / 10, /锁|重试|lock|retry/i.test(x.name) ? '--c5' : x.name === '其他' ? '--c6' : DBT_PALETTE[k++ % DBT_PALETTE.length]]));
}
/* 集群事实：副本数（真实模式取 PD 的 max-replicas，取不到回落 3） */
const replicaN = () => (LIVE.on && LIVE.snap && LIVE.snap.topology.replicas) || 3;
/* 容量预测（/api/forecast，异步；5 分钟内复用） */
const FC = {data: null, at: 0, p: null, err: ''};
function fcText(f) {
  if (!f.enough) return '数据不足 24 小时，暂无法预测';
  if (f.daysTo80 === null || f.daysTo80 === undefined) return '近 7 天存储基本不增长，无达到 80% 的预测';
  if (f.daysTo80 <= 0) return '已超过 80% 水位';
  const d = f.daysTo80;
  return `预计 ${d < 1 ? '不到 1' : Math.round(d)} 天后达 80%（按最近 7 天线性回归）`;
}
function fcFill() {
  const el = $('#kpiStoreD'); if (!el || !FC.data) return;
  el.textContent = `${el.dataset.base} · ${fcText(FC.data)}`; el.title = FC.data.basis || '';
  if (!FC.data.enough || FC.data.daysTo80 === 0) el.classList.add('up'); else el.classList.remove('up');
}
function fcLoad() {
  if (!LIVE.on) return;
  if (FC.data && Date.now() - FC.at < 300000) { fcFill(); return; }
  if (!FC.p) FC.p = liveGet('/api/forecast').then(d => { FC.data = d; FC.at = Date.now(); FC.err = ''; }, e => { FC.err = e.message; }).finally(() => { FC.p = null; });
  FC.p.then(fcFill);
}
const meter = (v, warnAt = 70, badAt = 85) => `<div class="meter"><b class="${v >= badAt ? 'bad' : v >= warnAt ? 'warn' : ''}" style="width:${v}%"></b></div>`;
const stDot = s => `<span class="dot ${stCls(s)}"></span>`;

/* ================== 页面定义 ================== */
const pages = {};

/* ---------- 总览 ---------- */
pages.overview = {
  title: '集群总览',
  render() {
    const up = NODES.filter(n => n.status === 'up').length, warn = NODES.filter(n => n.status === 'warn').length;
    const trans = NODES.filter(n => n.status === 'joining' || n.status === 'offline').length, down = NODES.filter(n => n.status === 'down').length;
    const tikv = NODES.filter(n => n.type === 'tikv');
    const kpi = (l, v, d, cls = 'flat', id = '') => `<div class="card kpi"><div class="l">${l}</div><div class="v">${v}</div><div class="d ${cls}" ${id ? `id="${id}" data-base="${esc(d)}"` : ''}>${d}</div></div>`;
    const mt = (typeof MAINTS !== 'undefined' ? MAINTS : []).filter(m => ['draining', 'active', 'recovering'].includes(m.state));
    return `
    ${typeof nextSchedNotice === 'function' ? nextSchedNotice() : ''}
    ${mt.map(m => `<div class="notice warn" style="margin-bottom:12px"><span>🛠</span><div><b>主机 ${esc(m.ip)} 正在维护</b>（${esc(m.typeLabel)}，计划 ${m.mins} 分钟）· ${m.state === 'active' ? '维护中' : m.state === 'draining' ? '正在排空' : '正在恢复'}。<a data-go="nodes" data-tab="maint">查看维护流程 →</a></div></div>`).join('')}
    <div class="grid g4">
      ${LIVE.on ? liveKpi().map((k, idx) => kpi(k[0], k[1], k[2], k[3], idx === 3 ? 'kpiStoreD' : '')).join('') : `${kpi('QPS', '10,530', '▲ 6.2% 较昨日同期', 'up')}
      ${kpi('P99 延迟', '38 ms', '▲ 12ms 较 1 小时前', 'up')}
      ${kpi('连接数', '731', '— 与昨日持平')}
      ${kpi('存储使用', '8.4 TiB / 15 TiB', '56% · 预计 41 天后达 80%', 'flat')}`}
    </div>
    <div class="grid g-main" style="margin-top:16px">
      <div class="grid" style="align-content:start">
        <div class="card"><h2>QPS 与延迟 <span class="sub">最近 60 分钟（${LIVE.on ? (LIVE.mode === 'replay' ? '回放数据' : '真实数据') : '模拟'}）</span></h2>
          <div class="grid g2">
            ${LIVE.on ? (LIVE.snap.trends.qps.length > 1 ? `<div>${lineChart([{name:'QPS',color:'var(--c1)',data:LIVE.snap.trends.qps}])}</div>
            <div>${lineChart([{name:'P99(ms)',color:'var(--c5)',data:LIVE.snap.trends.p99ms}],{unit:''})}</div>` : '<div class="muted small">未取得 Prometheus 趋势数据（见页面顶部的数据缺失说明）</div>') : `<div>${lineChart([{name:'QPS',color:'var(--c1)',data:series(3,60,10000,1500,[38,50,2400])}])}</div>
            <div>${lineChart([{name:'P99(ms)',color:'var(--c5)',data:series(8,60,26,6,[38,50,22])},{name:'P95(ms)',color:'var(--c3)',data:series(9,60,14,3,[38,50,10])}],{unit:''})}</div>`}
          </div></div>
        <div class="card"><h2>数据库时间分解 <span class="sub">按 SQL 类型</span><span class="right"><a data-go="diag">深入诊断 →</a></span></h2>${dbStack('byType')}</div>
        <div class="card"><h2>实例健康矩阵 <span class="sub">${up} 正常 · ${warn} 告警${trans ? ` · ${trans} 变更中` : ''} · ${down} 已停止</span><span class="right"><a data-go="arch">查看架构图 →</a></span></h2>
          <div class="tbl-wrap"><table><thead><tr><th>组件</th><th>实例</th><th>状态</th><th>CPU</th><th>内存</th><th>磁盘</th></tr></thead><tbody>
          ${NODES.filter(n => ['tidb','tikv','pd'].includes(n.type)).map(n => `<tr class="click" data-node="${esc(n.id)}"><td>${TYPE_META[n.type].label}</td><td class="mono">${esc(n.host)}:${esc(n.port)}</td><td>${stDot(n.status)} ${stText[n.status]}</td><td style="width:110px">${meter(n.cpu)}</td><td style="width:110px">${meter(n.mem)}</td><td style="width:110px">${meter(n.disk, 75, 85)}</td></tr>`).join('')}
          </tbody></table></div></div>
      </div>
      <div class="grid" style="align-content:start">
        <div class="card"><h2>集群信息</h2>
          <div class="row small"><span class="muted">名称</span><b>${esc(CLUSTER)}</b></div>
          <div class="row small"><span class="muted">版本</span><b>${esc(VERSION)}</b></div>
          <div class="row small"><span class="muted">部署</span><b>${LIVE.on ? (LIVE.snap.topology.azCount || '—') : 3} AZ · ${NODES.length} 实例（TiKV ${tikv.length} · TiDB ${NODES.filter(n => n.type === 'tidb').length}）</b></div>
          <div class="row small"><span class="muted">Region</span><b>${LIVE.on ? `${fmt(LIVE.snap.overview.regions || Math.round(tikv.reduce((a, n) => a + n.regions, 0) / replicaN()))}（×${replicaN()} 副本）` : `${fmt(Math.round(tikv.reduce((a, n) => a + n.regions, 0) / 3))}（×3 副本）`}</b></div>
          <div class="row small"><span class="muted">副本数 / 位置标签</span><b>${LIVE.on ? `${LIVE.snap.topology.replicas || '—'} / ${esc((LIVE.snap.topology.locationLabels || []).join(', ')) || '—'}` : '3 / zone, host'}</b></div>
          <div class="hr"></div>
          <div class="row"><button class="btn primary" data-go="diag">一键诊断</button><button class="btn" data-go="ops">执行巡检</button></div>
        </div>
        <div class="card"><h2>活跃告警 <span class="sub">${ALERTS.length}</span></h2>
          ${ALERTS.map(a => `<div style="padding:8px 0;border-bottom:1px solid var(--border)"><div class="row"><span class="tag ${sevTag[a.lv]}">${sevText[a.lv]}</span><b class="small">${esc(a.name)}</b></div><div class="small muted">${esc(a.inst)} · ${esc(a.time)} · ${esc(a.desc)}</div></div>`).join('')}
        </div>
        <div class="card"><h2>优先处理建议</h2>
          ${FINDINGS.filter(f => f.sev === 'critical').map(f => `<div class="notice bad"><span>●</span><div><b>${esc(f.title)}</b><br><a data-go="diag" data-open="${esc(f.id)}">查看根因与处置 →</a></div></div>`).join('')}
        </div>
      </div>
    </div>`;
  },
  init() { if (LIVE.on) fcLoad(); }   // 存储预测异步补写，不阻塞首屏
};

/* ---------- 架构图 ---------- */
let archState = {metric: 'cpu', sel: null, region: ''};
pages.arch = {
  title: '集群架构',
  render() {
    return `
    <div class="grid g-main">
      <div class="card arch"><h2>集群拓扑 <span class="sub">${esc(CLUSTER)} · 点击任意节点查看详情</span>
        <span class="right"><label class="small muted">着色指标</label>
        <select id="archMetric"><option value="cpu">CPU</option><option value="mem">内存</option><option value="disk">磁盘</option><option value="status">健康状态</option></select>
        <label class="small muted">Region 副本</label>
        <select id="archRegion"><option value="">不显示</option>${REGIONS.map(r => `<option value="${esc(r.id)}">Region ${esc(r.id)}（${esc(r.table)}）</option>`).join('')}</select></span></h2>
        <div id="archSvg"></div>
        <div id="lbxHint"></div>
        <div class="legend"><span><i style="background:var(--ok)"></i>健康 &lt;70%</span><span><i style="background:var(--warn)"></i>关注 70–85%</span><span><i style="background:var(--bad)"></i>异常 ≥85%</span><span>— 实线：读写请求</span><span>┈ 虚线：调度 / 异步复制</span><span>▭ 蓝色虚线框：扩容中 / 下线中</span></div>
      </div>
      <div class="grid" style="align-content:start">
        <div class="card" id="nodeDetail"></div>
        <div class="card"><h2>组件职责速查</h2>
          ${Object.entries(TYPE_META).map(([k, m]) => `<div style="margin-bottom:8px"><span class="tag" style="border-left:3px solid ${m.color}">${m.label}</span><div class="small muted" style="margin-top:2px">${m.role}</div></div>`).join('')}
          ${docLink('tidb-computing', '计算层说明 ↗')} · ${docLink('tidb-scheduling', '调度说明 ↗')}</div>
      </div>
    </div>
    <div class="grid g3" style="margin-top:16px">
      <div class="card"><h2>数据如何落到 TiKV</h2><div class="small">表数据：<code>t{TableID}_r{RowID}</code>；索引：<code>t{TableID}_i{IndexID}_{值}</code>。整个 KV 空间按 Key 范围切为 Region，每个 Region 有 ${replicaN()} 个副本组成 Raft Group，只有 Leader 对外提供读写。</div></div>
      <div class="card"><h2>PD 如何调度</h2><div class="small">TiKV 上报 Store 心跳（容量、Region 数、读写速度），Leader 上报 Region 心跳；PD 据此生成 AddReplica / RemoveReplica / TransferLeader 三种基础操作，使副本数、Leader/Region 分布、热点、存储空间保持均衡，并满足 location-labels 隔离要求。</div></div>
      <div class="card"><h2>HTAP 如何协作</h2><div class="small">TiFlash 以 Raft Learner 异步接收 TiKV 数据，保持强一致读；优化器基于代价决定走 TiKV 行存还是 TiFlash 列存/MPP，交易与分析互不抢占资源。</div></div>
    </div>`;
  },
  init() {
    $('#archMetric').value = archState.metric; $('#archRegion').value = archState.region;
    $('#archMetric').onchange = e => { archState.metric = e.target.value; this.draw(); };
    $('#archRegion').onchange = e => { archState.region = e.target.value; this.draw(); };
    const na = window.navArgs; window.navArgs = null;
    this.draw(); this.detail((na && na.node) || archState.sel || 'tikv-2');
  },
  color(n) {
    const m = archState.metric;
    if (m === 'status' || ['joining', 'offline', 'down'].includes(n.status)) return `var(--${{ok:'ok', warn:'warn', bad:'bad', info:'accent'}[stCls(n.status)]})`;
    const v = n[m]; return v >= 85 ? 'var(--bad)' : v >= 70 ? 'var(--warn)' : 'var(--ok)';
  },
  /* 负载均衡层（默认：静态原型的演示框，不是真实数据）。有后端 / 实时数据时由 ops_lb.js 替换为按真实数据绘制 */
  lbLayer() {
    return {h: 38, hint: '', svg: `<rect x="300" y="70" width="360" height="38" rx="9" fill="var(--panel)" stroke="var(--border)"/><text x="480" y="94" text-anchor="middle" font-size="13" fill="var(--text)">负载均衡 · TiProxy / HAProxy / LVS（示意）</text>`};
  },
  draw() {
    const W = 1040, bw = 156, bh = 58; let H = 710;
    const byType = t => NODES.filter(n => n.type === t);
    const region = REGIONS.find(r => String(r.id) === archState.region);
    const pos = {};
    let s = '';
    const layer = (x, y, w, h, label, fill) => s += `<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="12" fill="${fill || 'var(--panel2)'}" stroke="var(--border)"/><text class="layer-label" x="${x + 12}" y="${y + 18}">${label}</text>`;
    const arrow = (x1, y1, x2, y2, label, dash) => s += `<line x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" stroke="var(--muted)" stroke-width="1.5" ${dash ? 'stroke-dasharray="5 4"' : ''} marker-end="url(#ah)"/>${label ? `<text class="edge-label" x="${(x1 + x2) / 2 + 8}" y="${(y1 + y2) / 2 + 4}">${label}</text>` : ''}`;
    const box = (n, x, y) => {
      pos[n.id] = {x, y};
      const col = this.color(n), isRegion = region && region.peers.includes(n.id), isLeader = region && region.leader === n.id;
      const transient = ['joining', 'offline', 'down'].includes(n.status);
      const dim = region && n.type === 'tikv' && !isRegion ? .35 : n.status === 'down' ? .55 : 1;
      let metric = n.type === 'tikv' ? `Region ${fmt(n.regions)} · L ${fmt(n.leaders)}` : n.type === 'pd' ? (n.leader ? 'Leader' : 'Follower') : n.type === 'ticdc' ? `lag ${esc(n.lag)}` : n.type === 'tiflash' ? `Learner ${fmt(n.regions)}` : `QPS ${fmt(n.qps)}`;
      s += `<g class="node ${archState.sel === n.id ? 'sel' : ''}" data-node="${esc(n.id)}" opacity="${dim}" tabindex="0" role="button" aria-label="${esc(n.id)} ${stText[n.status]}">
        <rect class="box" x="${x}" y="${y}" width="${bw}" height="${bh}" rx="9" fill="var(--panel)" stroke="${isRegion ? 'var(--accent)' : transient ? 'var(--accent)' : 'var(--border)'}" stroke-width="${isRegion ? 2.5 : 1}" ${transient ? 'stroke-dasharray="5 3"' : ''}/>
        <rect x="${x}" y="${y}" width="6" height="${bh}" rx="3" fill="${col}"/>
        <text x="${x + 16}" y="${y + 20}" font-size="13" font-weight="600" fill="var(--text)">${esc(n.id)}</text>
        <text x="${x + 16}" y="${y + 36}" font-size="11" fill="var(--muted)">${esc(n.host)}</text>
        <text x="${x + 16}" y="${y + 51}" font-size="10.5" fill="var(--muted)">${metric}</text>
        ${transient ? `<text x="${x + bw - 10}" y="${y + 20}" text-anchor="end" font-size="11.5" font-weight="700" fill="${col}">${stText[n.status]}</text>` : archState.metric !== 'status' ? `<text x="${x + bw - 10}" y="${y + 20}" text-anchor="end" font-size="12" font-weight="700" fill="${col}">${n[archState.metric]}%</text>` : `<circle cx="${x + bw - 14}" cy="${y + 16}" r="5" fill="${col}"/>`}
        ${isRegion ? `<g><circle cx="${x + bw - 14}" cy="${y + bh - 14}" r="10" fill="${isLeader ? 'var(--accent)' : 'var(--panel2)'}" stroke="var(--accent)"/><text x="${x + bw - 14}" y="${y + bh - 10}" text-anchor="middle" font-size="11" font-weight="700" fill="${isLeader ? '#fff' : 'var(--accent)'}">${isLeader ? 'L' : 'F'}</text></g>` : ''}
      </g>`;
    };
    s += `<defs><marker id="ah" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0L10 5L0 10z" fill="var(--muted)"/></marker></defs>`;
    // 客户端 + 负载均衡层：lbLayer() 返回 {h, svg, hint}，h=0 表示不画负载均衡框（客户端直连 TiDB 层）；真实数据的画法由 ops_lb.js 替换
    const lbd = this.lbLayer();
    s += `<rect x="330" y="8" width="300" height="38" rx="9" fill="var(--panel)" stroke="var(--border)"/><text x="480" y="32" text-anchor="middle" font-size="13" fill="var(--text)">应用 / MySQL 客户端 / BI 工具</text>`;
    let tidbY = 84;
    if (lbd.h) { arrow(480, 46, 480, 70); s += lbd.svg; tidbY = 70 + lbd.h + 30; arrow(480, 70 + lbd.h, 480, tidbY); } else arrow(480, 46, 480, tidbY);
    const lbHint = $('#lbxHint'); if (lbHint) lbHint.innerHTML = lbd.hint || '';
    // 通用「横向排布」层：每行最多 perRow 个节点，行数随节点数增长
    const flow = (y, label, list, perRow, x0 = 40, w = 700) => {
      const rows = Math.max(1, Math.ceil(list.length / perRow)), h = 38 + rows * 58 + (rows - 1) * 10;
      layer(x0, y, w, h, label);
      list.forEach((n, i) => {
        const r = Math.floor(i / perRow), inRow = Math.min(perRow, list.length - r * perRow), c = i % perRow;
        const total = inRow * 166 - 10, bx = x0 + (w - total) / 2 + c * 166;
        box(n, bx, y + 28 + r * 68);
      });
      return h;
    };
    const tidbH = flow(tidbY, 'SQL 层 · TiDB Server（无状态，可横向扩展）', byType('tidb'), 4);
    const pdY = tidbY + tidbH + 38;
    arrow(390, tidbY + tidbH, 390, pdY, '获取 TSO / 路由', true);
    const pdH = flow(pdY, '调度层 · Placement Driver（元信息 · TSO · 调度，内部 Raft）', byType('pd'), 4);
    const kvY = pdY + pdH + 36;
    arrow(150, pdY + pdH, 150, kvY, 'gRPC：读写 / Coprocessor 下推');
    // TiKV 按可用区分组，每组纵向堆叠
    const zones = (() => { const z = [...new Set(byType('tikv').map(n => n.az))].sort(); return z.length ? z.slice(0, 3) : ['az1']; })();   // 真实集群的 zone 名不固定，最多分 3 列，多余的并入最后一列
    const kvByAz = zones.map((az, gi) => byType('tikv').filter(n => n.az === az || (gi === zones.length - 1 && !zones.includes(n.az))));
    const maxC = Math.max(2, ...kvByAz.map(l => l.length)), grpH = 30 + maxC * 60, kvH = grpH + 28;
    layer(40, kvY, 700, kvH, `存储层 · TiKV（Region ${replicaN()} 副本，Raft 多数派写入）`);
    zones.forEach((az, gi) => {
      const x = 56 + gi * 226;
      s += `<rect x="${x}" y="${kvY + 20}" width="212" height="${grpH}" rx="9" fill="none" stroke="var(--border)" stroke-dasharray="4 3"/><text class="layer-label" x="${x + 8}" y="${kvY + 36}" font-size="11">${esc(AZS[az] || az)} · ${kvByAz[gi].length} 个</text>`;
      kvByAz[gi].forEach((n, i) => box(n, x + 28, kvY + 42 + i * 60));
    });
    // TiFlash / TiCDC
    const lowY = kvY + kvH + 32;
    arrow(215, kvY + kvH, 215, lowY, 'Raft Learner 异步复制', true);
    const fh = flow(lowY, '分析层 · TiFlash（列存 / MPP）', byType('tiflash'), 2, 40, 350);
    arrow(570, kvY + kvH, 570, lowY, '变更日志', true);
    const ch = flow(lowY, '同步 · TiCDC', byType('ticdc'), 2, 400, 340);
    H = lowY + Math.max(fh, ch) + 14;
    // 右侧：监控 + 下游
    layer(770, tidbY, 250, 230, '运维 & 监控');
    ['Prometheus 指标采集','Grafana 仪表盘','Alertmanager 告警','TiDB Dashboard（PD 内置）'].forEach((t, i) => s += `<rect x="786" y="${tidbY + 26 + i * 48}" width="218" height="38" rx="8" fill="var(--panel)" stroke="var(--border)"/><text x="895" y="${tidbY + 50 + i * 48}" text-anchor="middle" font-size="12.5" fill="var(--text)">${t}</text>`);
    s += `<path d="M740 ${tidbY + 48} H770" stroke="var(--muted)" stroke-dasharray="4 3" marker-end="url(#ah)" fill="none"/><text class="edge-label" x="744" y="${tidbY + 42}">指标</text>`;
    layer(770, kvY, 250, 144, '下游 / 备份');
    ['Kafka / MySQL / 对象存储','BR 备份 → S3 / OSS / NFS','TiDB Lightning / DM 迁移'].forEach((t, i) => s += `<rect x="786" y="${kvY + 26 + i * 38}" width="218" height="30" rx="8" fill="var(--panel)" stroke="var(--border)"/><text x="895" y="${kvY + 46 + i * 38}" text-anchor="middle" font-size="12" fill="var(--text)">${t}</text>`);
    arrow(740, lowY + 40, 770, kvY + 120, '', true);
    // Raft 连线
    if (region && region.peers.length === 3 && region.peers.every(id => pos[id])) {
      const p = region.peers.map(id => ({id, x: pos[id].x + bw / 2, y: pos[id].y + bh / 2}));
      s += p.map((a, i) => { const b = p[(i + 1) % 3]; return `<path d="M${a.x} ${a.y} Q ${(a.x + b.x) / 2 + 60} ${(a.y + b.y) / 2} ${b.x} ${b.y}" stroke="var(--accent)" stroke-width="2" fill="none" stroke-dasharray="6 4" opacity=".8"/>`; }).join('');
    }
    $('#archSvg').innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="TiDB 集群架构拓扑图">${s}</svg>`;
    $$('#archSvg .node').forEach(g => {
      g.onclick = () => this.detail(g.dataset.node);
      g.onkeydown = e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this.detail(g.dataset.node); } };
    });
  },
  detail(id) {
    archState.sel = id; this.draw();
    let n = NODES.find(x => x.id === id);
    if (!n) { n = NODES.find(x => x.type === 'tikv') || NODES[0]; if (!n) { $('#nodeDetail').innerHTML = '<div class="card"><div class="empty">该集群没有可显示的实例（还没有采集到拓扑：集群不可达或刚接入）</div></div>'; return; } archState.sel = n.id; this.draw(); }
    const m = TYPE_META[n.type];
    const region = REGIONS.find(r => String(r.id) === archState.region);
    const relF = FINDINGS.filter(f => f.scope.includes(n.id));
    const tikvAvg = k => Math.round(NODES.filter(x => x.type === 'tikv').reduce((a, x) => a + x[k], 0) / NODES.filter(x => x.type === 'tikv').length);
    $('#nodeDetail').innerHTML = `
      <h2>${esc(n.id)} <span class="tag ${stCls(n.status)}">${stText[n.status]}</span><span class="right small muted">${m.label}</span></h2>
      <div class="small muted mono">${esc(n.host)}:${esc(n.port)} · ${esc(AZS[n.az])} · ${esc(VERSION)}</div>
      <div style="margin:10px 0">
        <div class="mrow"><span>CPU</span>${meter(n.cpu)}<span>${n.cpu}%</span></div>
        <div class="mrow"><span>内存</span>${meter(n.mem)}<span>${n.mem}%</span></div>
        <div class="mrow"><span>磁盘</span>${meter(n.disk, 75, 85)}<span>${n.disk}%</span></div>
        ${n.type === 'tikv' ? `<div class="mrow"><span>Raftstore</span>${meter(n.raftCpu)}<span>${n.raftCpu}%</span></div>` : ''}
      </div>
      ${n.type === 'tikv' ? `<div class="small"><b>Region</b> ${fmt(n.regions)} · <b>Leader</b> ${fmt(n.leaders)} · <b>写</b> ${n.wr} MB/s（均值 ${tikvAvg('wr')}） · <b>读</b> ${n.rd} MB/s</div>` : ''}
      ${n.type === 'tidb' ? `<div class="small"><b>QPS</b> ${fmt(n.qps)} · <b>连接数</b> ${n.conn}</div>` : ''}
      ${n.type === 'pd' ? `<div class="small"><b>角色</b> ${n.leader ? 'Leader（负责 TSO 与调度）' : 'Follower'}</div>` : ''}
      ${n.note ? `<div class="notice ${n.status === 'warn' ? 'warn' : 'info'}"><span>${n.status === 'warn' ? '⚠' : 'ℹ'}</span><div>${esc(n.note)}</div></div>` : ''}
      ${region && region.peers.includes(n.id) ? `<div class="notice info"><span>◆</span><div>持有 Region ${esc(region.id)}（${esc(region.table)}）的${region.leader === n.id ? ' <b>Leader</b> 副本' : ' Follower 副本'}；写入需多数派（${Math.floor(replicaN() / 2) + 1}/${replicaN()}）确认。</div></div>` : ''}
      <div class="small muted" style="margin:6px 0">${m.role}</div>
      ${relF.length ? `<div class="hr"></div><div class="small"><b>关联诊断</b></div>${relF.map(f => `<div style="margin-top:4px"><span class="tag ${sevTag[f.sev]}">${sevText[f.sev]}</span> <a data-go="diag" data-open="${esc(f.id)}">${esc(f.title)}</a></div>`).join('')}` : ''}
      <div class="hr"></div>
      <div class="row"><button class="btn sm primary" data-na="nd-open" data-id="${esc(n.id)}">详情 / 配置 →</button>
      <button class="btn sm danger" data-go="nodes" data-tab="scalein" data-node="${esc(n.id)}">缩容该节点</button>
      <button class="btn sm" data-copy="${esc(`tiup cluster display ${CLUSTER} -N ${n.host}:${n.port}`)}">复制 display</button></div>`;
  }
};

/* ---------- 智能诊断 ---------- */
let diagState = {ran: false, filter: 'all', scope: 'all'};
/* 诊断项属于哪一层：用于「诊断范围」下拉框筛选（按诊断项 ID / 范围判断；告警汇总两边都算） */
const dgLayer = f => {
  if (/^(tidbres-|slow-|slowx-|dbt-)/.test(f.id)) return 'tidb';
  if (/^(disk-|hotw-|balance$|forecast$|tikvw-|pd-|pdp-|pds-)/.test(f.id)) return 'store';
  if (f.id === 'alerts') return 'both';
  if (/^tidb/.test(f.scope)) return 'tidb';
  if (/^(tikv|pd|tiflash)/.test(f.scope)) return 'store';
  return 'both';
};
const dgScoped = () => FINDINGS.filter(f => diagState.scope === 'all' || dgLayer(f) === 'both' || dgLayer(f) === diagState.scope);
pages.diag = {
  title: '智能诊断',
  render() {
    return `
    <div class="grid g-main">
      <div class="grid" style="align-content:start">
        <div class="card"><h2>诊断范围</h2>
          <div class="row"><select id="dgRange" title="采集窗口目前固定为最近 1 小时（慢查询与趋势都按 1 小时取），其他时间范围暂不支持"><option>最近 1 小时</option><option disabled>最近 6 小时（暂不支持）</option><option disabled>最近 24 小时（暂不支持）</option><option disabled>自定义对比基线（暂不支持）</option></select>
          <select id="dgScope" title="按诊断项所在层筛选结果；告警汇总两边都显示"><option value="all" ${diagState.scope === 'all' ? 'selected' : ''}>全集群</option><option value="tidb" ${diagState.scope === 'tidb' ? 'selected' : ''}>仅 TiDB</option><option value="store" ${diagState.scope === 'store' ? 'selected' : ''}>仅 TiKV / PD</option></select>
          <button class="btn primary" id="dgRun">开始诊断</button>
          <span class="small muted">规则来源：官方「故障排查地图」「性能调优方法」「日常巡检」</span></div>
          <ul class="progress" id="dgProg" style="margin-top:10px;display:none"></ul>
        </div>
        <div class="card"><h2>数据库时间 (DB Time) 分解 <span class="sub">自上而下定位瓶颈</span><span class="right">${docLink('performance-tuning-methods')}</span></h2>
          <div class="small muted" style="margin-bottom:6px">① 按 SQL 类型</div>${dbStack('byType')}
          <div class="small muted" style="margin:14px 0 6px">② 按处理阶段</div>${dbStack('byStage')}
          <div class="small muted" style="margin:14px 0 6px">③ Execute 阶段细分</div>${dbStack('byExec')}${LIVE.on ? '<div class="small muted" style="margin-top:4px">各项有交叠，仅用于相对比较。</div>' : ''}
          <div class="hr"></div>
          <div class="small"><b>判读规则：</b>红色（悲观锁）突出 → 锁冲突；Compile 占比高 → 缺少计划缓存；TSO 等待明显 → PD/TSO 压力；TiDB–TiKV 延迟差同机房应 &lt; 2ms，跨 AZ &lt; 5ms。</div>
        </div>
        <div id="dgResult"></div>
      </div>
      <div class="grid" style="align-content:start">
        <div class="card"><h2>健康评分</h2>
          <div style="font-size:44px;font-weight:700;line-height:1" id="dgScore">—</div>
          <div class="small muted" id="dgScoreSub">尚未诊断</div>
          <div class="hr"></div>
          <div class="small muted">评分 = 100 − 严重×15 − 警告×6 − 提示×2（经验公式；选了「仅 TiDB / 仅 TiKV·PD」时只算当前范围）</div></div>
        <div class="card"><h2>诊断覆盖项</h2><ul class="small" style="padding-left:18px;margin:0">
          <li>实例状态、TiKV 磁盘水位、容量趋势预测</li><li>写热点（单个 TiKV 写流量明显偏高）</li><li>Region 分布不均</li><li>TiDB 内存 / CPU 偏高</li><li>慢 SQL（缺索引、索引列用函数、TiKV 排队、大内存）及对比上一小时：执行计划突变、耗时跳变；统计信息缺失、锁冲突 / TiKV 繁忙（Backoff）、MVCC 旧版本</li><li>DB Time 判读：计划缓存、Get Token、TSO 等待、悲观锁</li><li>PD 与副本健康：缺副本、down / pending peer、TSO 延迟、etcd 磁盘与网络、慢节点</li><li>TiKV 写入链路：latch、日志追加、Raftstore / Apply 线程、write stall</li><li>主机层：文件系统只读、inode、磁盘延迟、负载、TCP 连接数</li><li>严重告警汇总</li><li class="muted">暂未覆盖：全库统计信息健康度、PD leader 切换、跨 AZ 网络延迟、Region 数量过多</li></ul></div>
      </div>
    </div>`;
  },
  init() {
    $('#dgRun').onclick = () => this.run();
    $('#dgScope').onchange = e => { diagState.scope = e.target.value; if (diagState.ran) this.show(); };
    if (diagState.ran) this.show();
  },
  /* 开始诊断：真实 / 回放数据模式下，先向后端强制重新采集一次快照（跳过缓存）再出结果；静态原型没有后端，仍用内置演示数据。
     进度条是动画，但「生成诊断报告」一步要等采集真正完成才打勾。 */
  run() {
    const steps = ['采集监控指标（Prometheus）','分解 DB Time，识别阶段瓶颈','检查热点 Region 与写入分布','检查实例状态、磁盘水位与容量趋势','检查 TiDB 资源与慢 SQL','生成诊断报告'];
    const ul = $('#dgProg'); ul.style.display = 'block'; $('#dgRun').disabled = true; $('#dgResult').innerHTML = '';
    let err = null;
    const fetching = (typeof LIVE !== 'undefined' && LIVE.on && !(window.CL && CL.empty()))
      ? liveFetch(true).then(s => { liveApply(s); liveBanner(); }).catch(e => { err = e; }) : Promise.resolve();
    let i = 0;
    const paint = () => { ul.innerHTML = steps.map((t, k) => `<li class="${k < i ? 'done' : k === i ? 'run' : ''}"><span class="s">${k < i ? '✔' : k === i ? '◔' : '○'}</span>${t}</li>`).join(''); };
    const finish = () => {
      const b = $('#dgRun'); if (b) b.disabled = false;
      if (!$('#dgResult')) return;   // 采集期间页面被定时刷新重绘或已离开本页
      if (err) { $('#dgResult').innerHTML = `<div class="notice bad"><span>✖</span><div>诊断失败：重新采集数据出错（${esc(err.message || String(err))}）。下面不显示结果，请稍后重试；横幅里的「立即刷新」可以单独刷新数据。</div></div>`; return; }
      diagState.ran = true; this.show();
    };
    const tick = () => {
      paint();
      if (i < steps.length - 1) { i++; setTimeout(tick, 380); return; }
      fetching.then(() => { i = steps.length; paint(); finish(); });   // 最后一步等采集完成
    };
    tick();
  },
  show() {
    const scoped = dgScoped(), scopeName = {all: '', tidb: '（仅 TiDB 层）', store: '（仅 TiKV / PD 层）'}[diagState.scope] || '';
    const cnt = k => scoped.filter(f => f.sev === k).length;
    const score = Math.max(0, 100 - cnt('critical') * 15 - cnt('warning') * 6 - cnt('info') * 2);
    $('#dgScore').textContent = score; $('#dgScore').style.color = score < 60 ? 'var(--bad)' : score < 80 ? 'var(--warn)' : 'var(--ok)';
    $('#dgScoreSub').textContent = `${cnt('critical')} 个严重 · ${cnt('warning')} 个警告 · ${cnt('info')} 个提示${scopeName}`;
    const list = scoped.filter(f => diagState.filter === 'all' || f.sev === diagState.filter);
    $('#dgResult').innerHTML = `<div class="card"><h2>诊断结果 <span class="sub">共 ${scoped.length} 项${scopeName}</span>
      <span class="right chips">${[['all','全部'],['critical','严重'],['warning','警告'],['info','提示']].map(([k, t]) => `<span class="chip ${diagState.filter === k ? 'active' : ''}" data-f="${k}">${t}</span>`).join('')}</span></h2>
      ${list.map(f => `<details class="finding ${esc(f.sev)}" id="fd-${esc(f.id)}"><summary><span class="tag ${sevTag[f.sev]}">${sevText[f.sev]}</span><b>${esc(f.title)}</b><span class="small muted">${esc(f.scope)}</span></summary>
        <div class="body">
          <div><h4>证据</h4><ul>${f.evidence.map(e => `<li>${esc(e)}</li>`).join('')}</ul></div>
          <div><h4>可能根因</h4>${esc(f.cause)}</div>
          <div><h4>优化 / 处置建议</h4><ol>${f.actions.map(a => `<li>${esc(a.t)}${a.cmd ? code(a.cmd) : ''}</li>`).join('')}</ol></div>
          <div class="row"><button class="btn sm" data-go="${esc(f.link.page)}" ${Object.entries(f.link.args || {}).filter(([k]) => /^[a-z][a-z0-9-]*$/.test(k)).map(([k, v]) => `data-${k}="${esc(v)}"`).join(' ')}>${esc(f.link.label)} →</button>${docLink(f.ref)}</div>
        </div></details>`).join('')}</div>`;
    $$('#dgResult .chip').forEach(c => c.onclick = () => { diagState.filter = c.dataset.f; this.show(); });
    if (pages.diag.pendingOpen) { const d = $('#fd-' + pages.diag.pendingOpen); if (d) { d.open = true; d.scrollIntoView({block: 'center'}); } pages.diag.pendingOpen = null; }
  }
};

/* ---------- SQL 优化 ---------- */
pages.sql = {
  title: 'SQL 优化',
  render() {
    return `
    <div class="tabs" id="sqlTabs"><button class="active" data-t="top">Top SQL / 慢查询</button><button data-t="lint">SQL 体检</button><button data-t="flow">调优流程</button></div>
    <div id="sqlBody"></div>`;
  },
  init() {
    $$('#sqlTabs button').forEach(b => b.onclick = () => { $$('#sqlTabs button').forEach(x => x.classList.toggle('active', x === b)); this.tab(b.dataset.t); });
    this.tab('top');
  },
  tab(t) {
    const el = $('#sqlBody');
    if (t === 'top') {
      el.innerHTML = `<div class="card"><h2>累计耗时 Top SQL <span class="sub">按 Digest 聚合（模拟，对应 Statement Summary / SLOW_QUERY）</span>
        <span class="right"><select><option>按总耗时</option><option>按平均耗时</option><option>按执行次数</option><option>按扫描 Keys</option></select></span></h2>
        <div class="tbl-wrap"><table><thead><tr><th>#</th><th>SQL（Digest）</th><th class="num">执行次数</th><th class="num">平均耗时</th><th class="num">P99</th><th class="num">Total_keys</th><th class="num">Process_keys</th><th>问题标签</th></tr></thead><tbody>
        ${SQLS.slice().sort((a, b) => b.total - a.total).map(q => `<tr class="click" data-sql="${esc(q.id)}"><td>${esc(q.id)}</td><td style="max-width:420px"><div class="mono small" style="white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:420px">${esc(q.sql)}</div><span class="muted small">${esc(q.digest)} · ${esc(q.db)}.${esc(q.user)}</span></td><td class="num">${fmt(q.exec)}</td><td class="num">${q.avg}s</td><td class="num">${q.p99}s</td><td class="num">${fmt(q.keys)}</td><td class="num">${fmt(q.pkeys)}</td><td><span class="tag warn">${esc(q.tag)}</span></td></tr>`).join('')}
        </tbody></table></div>
        <div class="small muted" style="margin-top:8px">提示：Total_keys 远大于 Process_keys / Result_rows，说明扫描了大量无效数据，通常是缺索引或统计信息不准。</div></div>`;
      $$('[data-sql]').forEach(tr => tr.onclick = () => this.detail(+tr.dataset.sql));
    } else if (t === 'lint') {
      el.innerHTML = `<div class="grid g2"><div class="card"><h2>粘贴 SQL 做静态体检</h2>
        <textarea id="lintIn" rows="9" style="width:100%" class="mono">SELECT * FROM orders o
WHERE DATE(o.created_at) = '2025-10-01'
  AND o.remark LIKE '%退款%'
ORDER BY o.id
LIMIT 100000, 20;</textarea>
        <div class="row" style="margin-top:8px"><button class="btn primary" id="lintGo">分析</button><span class="small muted">基于规则的示例；真实平台可结合 EXPLAIN / 统计信息</span></div></div>
        <div class="card"><h2>体检结果</h2><div id="lintOut"></div></div></div>`;
      $('#lintGo').onclick = () => this.lint(); this.lint();
    } else {
      el.innerHTML = `<div class="card"><h2>SQL 调优标准流程 ${docLink('sql-tuning-overview')}</h2>
        <div class="grid g4">${[['1 定位','慢日志 / Statement Summary / Top SQL，按 Digest 聚合找出最耗时的语句'],['2 看计划','EXPLAIN ANALYZE：对比 estRows 与 actRows，找出偏差最大的算子'],['3 修根因','ANALYZE 更新统计信息；建索引；改写 SQL；调整 Join'],['4 控计划','SQL Binding / Optimizer Hints 固定计划；Plan Replayer 留存现场']].map(([a, b]) => `<div class="card" style="background:var(--panel2)"><b>${a}</b><div class="small muted" style="margin-top:4px">${b}</div></div>`).join('')}</div>
        <div class="hr"></div><h2>常用命令</h2>
        ${code("-- 最近 1 小时最慢 Digest\nSELECT digest, COUNT(*) cnt, AVG(query_time) avg_t, MAX(mem_max) mem\nFROM information_schema.cluster_slow_query\nWHERE time > NOW() - INTERVAL 1 HOUR\nGROUP BY digest ORDER BY avg_t * cnt DESC LIMIT 10;")}
        ${code("EXPLAIN ANALYZE SELECT ...;\nPLAN REPLAYER DUMP EXPLAIN SELECT ...;   -- 导出现场，便于复现")}</div>`;
    }
  },
  lint() {
    const sql = $('#lintIn').value, out = [];
    const add = (sev, t, fix) => out.push({sev, t, fix});
    if (/select\s+\*/i.test(sql)) add('warning', '使用 SELECT *', '只查询需要的列，减少回表和网络开销，利于覆盖索引。');
    if (/like\s+'%/i.test(sql)) add('warning', 'LIKE 前导通配符，无法使用索引', '改用前缀匹配、全文检索，或评估 TiFlash 扫描。');
    if (/where[\s\S]*\b(date|year|month|lower|upper|substr|ifnull|cast)\s*\(\s*\w+(\.\w+)?\s*\)\s*(=|>|<)/i.test(sql)) add('critical', '在条件列上使用函数，索引失效', "改写为范围条件：col >= '2025-10-01' AND col < '2025-10-02'。");
    const off = sql.match(/limit\s+(\d+)\s*,\s*\d+|offset\s+(\d+)/i);
    if (off && +(off[1] || off[2]) >= 10000) add('warning', `深分页 OFFSET ${(off[1] || off[2])}`, '改用游标分页：WHERE id > 上次最大 id ORDER BY id LIMIT n。');
    if (!/where/i.test(sql) && /select|update|delete/i.test(sql)) add('critical', '缺少 WHERE 条件，将触发全表扫描/全表修改', '补充过滤条件；大批量 DELETE/UPDATE 请分批（每批 ≤ 数千行）。');
    if (/\bin\s*\(\s*select/i.test(sql)) add('info', 'IN 子查询', '确认是否可改写为 JOIN / EXISTS，并检查子查询结果规模。');
    if (/order\s+by/i.test(sql) && !/limit/i.test(sql)) add('info', 'ORDER BY 无 LIMIT', '全量排序耗内存，确认是否真的需要全部结果。');
    if (/or\s+/i.test(sql) && /where/i.test(sql) && / or /i.test(sql)) add('info', '条件中包含 OR', '可能无法利用单一索引；评估是否改写为 UNION ALL。');
    $('#lintOut').innerHTML = out.length ? out.map(o => `<div class="notice ${o.sev === 'critical' ? 'bad' : o.sev === 'warning' ? 'warn' : 'info'}"><span>●</span><div><b>${o.t}</b><br><span class="small">${o.fix}</span></div></div>`).join('') : '<div class="notice ok"><span>✔</span><div>未发现明显问题，可进一步使用 EXPLAIN ANALYZE 验证。</div></div>';
  },
  detail(id) {
    const q = SQLS.find(x => x.id === id);
    $$('[data-sql]').forEach(tr => tr.classList.toggle('sel', +tr.dataset.sql === id));
    const flagged = p => { const e = +p[1], a = +p[2]; return (a / Math.max(e, 1) >= 10) || (e / Math.max(a, 1) >= 10 && e > 1000); };
    openDrawer(`SQL 详情 · ${q.digest}`, `
      <pre class="code mono" style="margin-top:0">${esc(q.sql)}</pre>
      <div class="grid g3" style="margin:10px 0">
        <div class="card kpi"><div class="l">平均耗时 / P99</div><div class="v" style="font-size:20px">${q.avg}s / ${q.p99}s</div></div>
        <div class="card kpi"><div class="l">执行次数</div><div class="v" style="font-size:20px">${fmt(q.exec)}</div></div>
        <div class="card kpi"><div class="l">最大内存</div><div class="v" style="font-size:20px">${esc(q.mem)}</div></div></div>
      <div class="small muted">Total_keys ${fmt(q.keys)} · Process_keys ${fmt(q.pkeys)} · 返回行 ${esc(q.rows)} · 索引 ${esc(q.index)}</div>
      <h3 style="font-size:14px;margin:16px 0 6px">执行计划（EXPLAIN ANALYZE）</h3>
      ${q.plan.length ? '' : '<div class="notice info"><span>ℹ</span><div>慢日志里没有这条 SQL 的执行计划（Plan 列为空）：可能 tidb_record_plan_in_slow_log 已关闭，或该 SQL 近 1 小时没有进入慢日志。可手动执行 EXPLAIN ANALYZE，或使用 Plan Replayer。</div></div>'}
      <div class="tbl-wrap"><table><thead><tr><th>算子</th><th class="num">estRows</th><th class="num">actRows</th><th>task</th><th>access object</th></tr></thead><tbody>
      ${q.plan.map(p => `<tr class="${flagged(p) ? 'flag' : ''}"><td class="mono small" style="white-space:pre">${esc(p[0])}</td><td class="num">${fmt(+p[1])}</td><td class="num">${fmt(+p[2])}</td><td>${esc(p[3])}</td><td class="small">${esc(p[4])}<div class="muted">${esc(p[5])}</div></td></tr>`).join('')}
      </tbody></table></div>
      ${q.plan.some(flagged) ? '<div class="notice bad"><span>⚠</span><div>红色行 estRows 与 actRows 偏差 ≥ 10×，通常意味着统计信息不准，优化器可能选错计划。</div></div>' : ''}
      <h3 style="font-size:14px;margin:16px 0 6px">优化建议</h3>
      <ol>${q.tips.map(t => `<li style="margin-bottom:6px">${esc(t.t)}${t.cmd ? code(t.cmd) : ''}</li>`).join('')}</ol>
      <div class="hr"></div><div class="row"><button class="btn sm" data-copy="PLAN REPLAYER DUMP EXPLAIN ${esc(q.sql)}">复制 Plan Replayer 命令</button>${docLink('information-schema-slow-query', '慢查询字段说明 ↗')}</div>`);
  }
};

/* ---------- 故障排查 ---------- */
let trState = {cat: '全部', q: ''};
/* 排障向导的实时证据缓存（GET /api/wizard/evidence，仅真实 / 回放数据模式；静态模式永远为空，向导与原型一致） */
const trEv = {data: null, at: 0, loading: false};
const trEvTime = () => { const d = trEv.data && trEv.data.collectedAt ? new Date(trEv.data.collectedAt) : null; return d && !isNaN(d) ? d.toLocaleTimeString() : ''; };
const trEvLink = ref => (ref && pages[ref]) ? ` <a data-go="${esc(ref)}">查看 →</a>` : '';
function trEvHtml(e) {
  return `<div class="notice info wiz-ev"><span>📡</span><div><b>实时数据建议：${e.answer === 'yes' ? '是' : '否'}</b> —— ${esc(e.why || '')}${trEvLink(e.ref)}<div class="small muted">依据${trEv.data && trEv.data.mode === 'replay' ? '回放' : '实时'}数据${trEvTime() ? '（采集于 ' + trEvTime() + '）' : ''}，仅供参考；请对照下方问题自行确认后再点击。</div></div></div>`;
}
function trFactsHtml() {
  const d = trEv.data; if (!d || !Array.isArray(d.facts) || !d.facts.length) return '';
  const ic = {bad: '✖', warn: '⚠', ok: '✔'};
  return `<div class="card wiz-facts" style="margin-bottom:16px"><h2>当前集群事实 <span class="sub">来自${d.mode === 'replay' ? '回放' : '实时'}数据${trEvTime() ? ' · 采集于 ' + trEvTime() : ''}</span></h2>
    ${d.facts.map(f => `<div class="fact"><span class="tag ${f.level === 'bad' || f.level === 'warn' || f.level === 'ok' ? f.level : ''}">${ic[f.level] || 'ℹ'}</span><span>${esc(f.text || '')}${trEvLink(f.ref)}</span></div>`).join('')}</div>`;
}
pages.trouble = {
  title: '故障排查',
  render() {
    return `<div class="tabs" id="trTabs"><button class="active" data-t="map">故障排查地图</button><button data-t="wiz">排障向导</button><button data-t="log">日志错误速查</button></div><div id="trBody"></div>`;
  },
  init() {
    this._sym = null; this._path = [];
    $$('#trTabs button').forEach(b => b.onclick = () => { $$('#trTabs button').forEach(x => x.classList.toggle('active', x === b)); this._sym = null; this._path = []; this[b.dataset.t](); });
    this.map();
  },
  map() {
    this._tab = 'map';
    const list = TROUBLE.filter(t => (trState.cat === '全部' || t.cat === trState.cat) && (!trState.q || JSON.stringify(t).toLowerCase().includes(trState.q.toLowerCase())));
    $('#trBody').innerHTML = `
      <div class="row" style="margin-bottom:12px"><input type="search" id="trQ" placeholder="搜索现象、报错或关键词，例如 server is busy / OOM / 热点" style="min-width:320px;flex:1" value="${esc(trState.q)}">
        <div class="chips">${TROUBLE_CATS.map(c => `<span class="chip ${trState.cat === c ? 'active' : ''}" data-c="${c}">${c}</span>`).join('')}</div>${docLink('tidb-troubleshooting-map', '官方排查地图 ↗')}</div>
      ${list.length ? list.map(t => `<details class="fold"><summary><span class="tag info">${t.cat}</span><b>${t.title}</b><span class="small muted">${t.symptom}</span></summary><div class="body">
        <div><b class="small muted">可能原因</b><ul>${t.causes.map(c => `<li>${c}</li>`).join('')}</ul></div>
        <div><b class="small muted">排查步骤</b><ol>${t.steps.map(c => `<li>${c}</li>`).join('')}</ol></div>
        <div><b class="small muted">解决方案</b><ul>${t.fix.map(c => `<li>${c}</li>`).join('')}</ul></div>
        ${t.cmd ? code(t.cmd) : ''}</div></details>`).join('') : '<div class="empty">没有匹配的条目</div>'}`;
    $$('#trBody .chip').forEach(c => c.onclick = () => { trState.cat = c.dataset.c; this.map(); });
    $('#trQ').oninput = e => { trState.q = e.target.value; const pos = e.target.selectionStart; this.map(); const q = $('#trQ'); q.focus(); q.setSelectionRange(pos, pos); };
  },
  /* 拉取实时证据；仅 LIVE.on（真实 / 回放数据，含只读后端）时有效，失败静默。到达后若仍停在向导页就原地重绘（不重置决策路径） */
  async evLoad() {
    if (typeof LIVE === 'undefined' || !LIVE.on || trEv.loading || (trEv.data && Date.now() - trEv.at < 15000)) return;
    trEv.loading = true;
    try {
      const d = await liveGet('/api/wizard/evidence');
      trEv.data = d; trEv.at = Date.now();
      if (location.hash.slice(1) === 'trouble' && this._tab === 'wiz' && $('#trBody') && !$('#drawer').classList.contains('open')) this.wiz(this._sym, this._cur);
    } catch (e) { /* 静默：保持原有向导行为 */ } finally { trEv.loading = false; }
  },
  wiz(sym, nodeId) {
    this._tab = 'wiz';
    const trees = WIZARD; sym = sym || this._sym; this._sym = sym; if (!nodeId) this._path = [];
    this._cur = sym ? nodeId : undefined;
    this.evLoad();
    const evs = (typeof LIVE !== 'undefined' && LIVE.on && trEv.data && trEv.data.evidence) || {};
    if (!sym) {
      $('#trBody').innerHTML = trFactsHtml() + `<div class="card"><h2>选择你遇到的症状</h2><div class="grid g3">${Object.keys(trees).map(k => `<button class="card btn" data-s="${k}" style="text-align:left"><b>${k}</b><div class="small muted">按决策树逐步定位根因</div></button>`).join('')}</div></div>`;
      $$('#trBody [data-s]').forEach(b => b.onclick = () => this.wiz(b.dataset.s, trees[b.dataset.s].start));
      return;
    }
    const tree = trees[sym], n = tree.nodes[nodeId || tree.start];
    const go = (id, label) => { this._path.push(`${n.q ? n.q.slice(0, 16) + '…' : ''}→${label}`); this.wiz(sym, id); };
    let body;
    if (n.q) {
      const opts = n.opts || [['是', n.y], ['否', n.n]];
      const ev = evs[`${sym}/${nodeId || tree.start}`], rec = ev && (ev.answer === 'yes' || ev.answer === 'no') ? (ev.answer === 'yes' ? '是' : '否') : null;   // unknown / 没有证据：不显示
      body = `<div class="q">${n.q}</div>${rec ? trEvHtml(ev) : ''}<div class="row">${opts.map(([l, id]) => `<button class="btn ${l === '是' ? 'primary' : ''} ${l === rec ? 'rec' : ''}" data-n="${id}" data-l="${l}">${l}</button>`).join('')}</div>`;
    } else {
      body = `<div class="notice ok"><span>✔</span><div><b>定位结果：${n.r}</b><br>${n.desc}</div></div><b>建议处置</b><ol>${n.steps.map(s => `<li>${s}</li>`).join('')}</ol>
        <div class="row">${n.link ? `<button class="btn" data-go="${n.link}">查看相关页面 →</button>` : ''}<button class="btn" id="wizReset">重新选择症状</button></div>`;
    }
    $('#trBody').innerHTML = `<div class="card wiz"><h2>排障向导 · ${sym}</h2><div class="path">${this._path.map(p => `<span class="tag">${esc(p)}</span>`).join('')}</div>${body}</div>`;
    $$('#trBody [data-n]').forEach(b => b.onclick = () => go(b.dataset.n, b.dataset.l));
    const r = $('#wizReset'); if (r) r.onclick = () => { this._sym = null; this.wiz(); };
  },
  log() {
    this._tab = 'log';
    $('#trBody').innerHTML = `<div class="card"><h2>常见日志错误速查 ${docLink('tidb-troubleshooting-map')}</h2>
      <div class="tbl-wrap"><table><thead><tr><th>日志 / 报错</th><th>含义</th><th>处理建议</th><th>级别</th></tr></thead><tbody>
      ${LOG_ERRORS.map(e => `<tr><td class="mono small">${esc(e.msg)}</td><td>${e.mean}</td><td>${e.act}</td><td><span class="tag ${e.sev}">${{info:'可自愈', warn:'需关注', bad:'需处理'}[e.sev]}</span></td></tr>`).join('')}
      </tbody></table></div></div>`;
  }
};

/* ---------- 热点 ---------- */
let hotState = {mode: 'write'};
pages.hotspot = {
  title: '热点与调度',
  render() {
    const tikv = NODES.filter(n => n.type === 'tikv');
    const maxW = Math.max(1, ...tikv.map(n => n.wr));
    // 真实数据模式：按各 TiKV 的写流量实时计算不均衡文案（最大值 / 均值 ≥ 3× 才提示）；静态模式保留原型文案
    const wrAvg = tikv.length ? tikv.reduce((a, n) => a + n.wr, 0) / tikv.length : 0, wrTop = tikv.reduce((a, n) => n.wr > (a ? a.wr : -1) ? n : a, null);
    const wrNotice = !LIVE.on ? '<div class="notice warn"><span>⚠</span><div>tikv-2 写流量是均值的 4.8×，节点间不均衡。</div></div>'
      : !wrTop || wrAvg <= 0 ? '<div class="notice info"><span>ℹ</span><div>未采集到各 TiKV 的写流量。</div></div>'
      : wrTop.wr / wrAvg >= 3 ? `<div class="notice warn"><span>⚠</span><div>${esc(wrTop.id)} 写流量是均值的 ${(wrTop.wr / wrAvg).toFixed(1)}×，节点间不均衡。</div></div>`
      : `<div class="notice ok"><span>✔</span><div>各 TiKV 写流量较均衡（最大为均值的 ${(wrTop.wr / wrAvg).toFixed(1)}×）。</div></div>`;
    const hotReg = REGIONS.filter(r => r.hot), splitId = hotReg[0] ? hotReg[0].id : '<region_id>';
    const splitCmd = LIVE.on ? `${PDCTL()} operator add split-region ${splitId} --policy=approximate\n${PDCTL()} hot write`
      : 'tiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 operator add split-region 20871 --policy=approximate\ntiup ctl:v8.5.2 pd -u http://10.0.1.21:2379 hot write';
    return `
    <div class="grid g-main">
      <div class="card"><h2>流量热力图 <span class="sub">${LIVE.on ? '各 TiKV 写入流量 × 时间（Prometheus）' : 'Key Visualizer（模拟）'}</span>
        <span class="right chips"><span class="chip ${hotState.mode === 'write' ? 'active' : ''}" data-m="write">写流量</span><span class="chip ${hotState.mode === 'read' ? 'active' : ''}" data-m="read">读流量</span></span></h2>
        <div class="heat-wrap"><canvas id="heat" width="720" height="400"></canvas><div class="heat-tip" id="heatTip"></div></div>
        <div class="small muted" style="margin-top:8px">颜色越亮流量越大。<b>写热点</b>表现为「阶梯状斜线」（顺序递增主键），<b>读热点</b>表现为「明亮的横线」（少量 Key 被集中读取）。${docLink('troubleshoot-hot-spot-issues')}</div></div>
      <div class="grid" style="align-content:start">
        <div class="card"><h2>各 TiKV 写流量 <span class="sub">MB/s</span></h2>
          ${tikv.map(n => `<div class="mrow"><span>${esc(n.id)}</span><div class="meter"><b class="${n.wr > 100 || (LIVE.on && wrAvg > 0 && n.wr / wrAvg >= 3) ? 'bad' : ''}" style="width:${n.wr / maxW * 100}%"></b></div><span>${n.wr}</span></div>`).join('')}
          ${wrNotice}</div>
        <div class="card"><h2>热点 Region Top</h2><div class="small">
          ${LIVE.on && !hotReg.length ? '<div class="muted">当前没有热点 Region</div>' : ''}${hotReg.map(r => `<div style="padding:6px 0;border-bottom:1px solid var(--border)"><b>Region ${esc(r.id)}</b> <span class="tag bad">${esc(r.hot)}</span><br><span class="muted">${esc(r.table)} · Leader ${esc(r.leader)} · ${esc(r.range)}</span></div>`).join('')}</div></div>
      </div>
    </div>
    <div class="card" style="margin-top:16px"><h2>解决方案选择器 <span class="sub">按场景推荐</span></h2>
      <div class="grid g2">
      ${[['写热点 · 表有整数主键，可改为随机','AUTO_RANDOM：替代自增主键，新写入离散分布。','CREATE TABLE t (\n  id BIGINT PRIMARY KEY AUTO_RANDOM,\n  ...\n);'],
         ['写热点 · 无主键 / 非聚簇索引','SHARD_ROW_ID_BITS：打散隐式 RowID（4 表示 16 个分片），可配合 PRE_SPLIT_REGIONS 预切分。','ALTER TABLE t SHARD_ROW_ID_BITS = 4;\n-- 建表时\nCREATE TABLE t (...) SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=3;'],
         ['读热点 · Leader 排队严重','负载自适应副本读取（v7.1.0+）：Leader 排队超阈值时转由 Follower 读取，官方给出读吞吐可提升 70%–200%。','SET GLOBAL tidb_load_based_replica_read_threshold = "1s";'],
         ['单个热点 Region · 临时止血','手动 split 热点 Region，让调度器把片段分散到不同 Store。',splitCmd]].map(([a, b, c]) => `<div class="card" style="background:var(--panel2)"><b>${a}</b><div class="small muted" style="margin:4px 0">${b}</div>${code(c)}</div>`).join('')}
      </div>
      <div class="small muted">另有：基于 CPU 的热点调度（较新版本）、TiKV MVCC 内存引擎（缓解多版本扫描），详见官方文档。</div></div>`;
  },
  init() {
    $$('.chip[data-m]').forEach(c => c.onclick = () => { hotState.mode = c.dataset.m; $$('.chip[data-m]').forEach(x => x.classList.toggle('active', x === c)); this.heat(); });
    this.heat();
  },
  heatLive() {
    const cv = $('#heat'), ctx = cv.getContext('2d'), tr = LIVE.snap.trends, rows = tr.storeWrite || [];
    ctx.clearRect(0, 0, cv.width, cv.height);
    if (!rows.length) { ctx.fillStyle = '#888'; ctx.font = '14px sans-serif'; ctx.fillText('没有采集到各 TiKV 的写入流量（Prometheus 指标 tikv_engine_flow_bytes）', 20, 40); return; }
    const C = rows[0].vals.length, R = rows.length, lab = 90, cw = (cv.width - lab) / C, ch = cv.height / R;
    const mx = Math.max(1, ...rows.flatMap(r => r.vals));
    const ramp = v => { const t = Math.min(1, v); const stops = [[10,16,40],[30,60,140],[40,160,190],[240,200,60],[255,250,240]]; const p = t * 4, i = Math.min(3, Math.floor(p)), f = p - i; return stops[i].map((c, k) => Math.round(c + (stops[i + 1][k] - c) * f)); };
    ctx.font = '12px sans-serif'; ctx.textBaseline = 'middle';
    rows.forEach((r, y) => {
      ctx.fillStyle = getComputedStyle(document.body).color; ctx.fillText(r.inst, 4, y * ch + ch / 2);
      r.vals.forEach((v, x) => { const [a, b, c] = ramp(v / mx); ctx.fillStyle = `rgb(${a},${b},${c})`; ctx.fillRect(lab + x * cw, y * ch, cw + .5, ch - 1); });
    });
    const tip = $('#heatTip');
    cv.onmousemove = e => {
      const rc = cv.getBoundingClientRect(), px = (e.clientX - rc.left) / rc.width * cv.width, py = (e.clientY - rc.top) / rc.height * cv.height;
      const x = Math.floor((px - lab) / cw), y = Math.floor(py / ch);
      if (x < 0 || y < 0 || x >= C || y >= R) { tip.style.display = 'none'; return; }
      const t = new Date((tr.start + x * tr.step) * 1000);
      tip.style.display = 'block'; tip.style.left = (e.clientX - rc.left + 12) + 'px'; tip.style.top = (e.clientY - rc.top + 12) + 'px';
      tip.textContent = `${rows[y].inst} · ${t.toLocaleTimeString()} · ${rows[y].vals[x]} MB/s`;
    };
    cv.onmouseleave = () => tip.style.display = 'none';
  },
  heat() {
    if (LIVE.on) return this.heatLive();
    const cv = $('#heat'), ctx = cv.getContext('2d'), C = 72, R = 40, cw = cv.width / C, ch = cv.height / R;
    const r = rng(hotState.mode === 'write' ? 11 : 29); const data = [];
    for (let y = 0; y < R; y++) { data[y] = []; for (let x = 0; x < C; x++) {
      let v = r() * 0.16 + (y > 20 ? .05 : 0);
      if (hotState.mode === 'write') {
        const hy = R - 1 - ((x * 0.52 + 4) % R); const d = Math.abs(y - hy); if (d < 2) v += (1 - d / 2) * (.75 + r() * .25);
        if (y === 30 || y === 31) v += .12;
      } else {
        if (y === 14 || y === 15) v += x > 12 ? .8 + r() * .2 : .1;
        if (y === 28) v += .35 * r();
      }
      data[y][x] = Math.min(1, v);
    } }
    const ramp = v => { const t = Math.min(1, v); const stops = [[10,16,40],[30,60,140],[40,160,190],[240,200,60],[255,250,240]]; const p = t * 4, i = Math.min(3, Math.floor(p)), f = p - i; return stops[i].map((c, k) => Math.round(c + (stops[i + 1][k] - c) * f)); };
    for (let y = 0; y < R; y++) for (let x = 0; x < C; x++) { const [a, b, c] = ramp(data[y][x]); ctx.fillStyle = `rgb(${a},${b},${c})`; ctx.fillRect(x * cw, y * ch, cw + .5, ch + .5); }
    const tip = $('#heatTip');
    cv.onmousemove = e => {
      const rc = cv.getBoundingClientRect(), x = Math.floor((e.clientX - rc.left) / rc.width * C), y = Math.floor((e.clientY - rc.top) / rc.height * R);
      if (x < 0 || y < 0 || x >= C || y >= R) return;
      tip.style.display = 'block'; tip.style.left = (e.clientX - rc.left + 12) + 'px'; tip.style.top = (e.clientY - rc.top + 12) + 'px';
      tip.textContent = `${(R - y) * 3 - 3}–${(R - y) * 3} min 前 · Key 区间 #${y + 1} · ${(data[y][x] * (hotState.mode === 'write' ? 260 : 340)).toFixed(0)} MB/s`;
    };
    cv.onmouseleave = () => tip.style.display = 'none';
  }
};

/* ---------- 日常运维 ---------- */
const OPS = {
  display:{label:'查看状态 display',risk:'low',fields:[],gen:()=>`tiup cluster display ${CLUSTER}`,checks:[['ok','只读操作，无风险']]},
  check:{label:'预检 check',risk:'low',fields:[],gen:()=>`tiup cluster check ${CLUSTER} --cluster`,checks:[['ok','检查 OS 参数、磁盘、NTP、端口，仅读取']]},
  restart:{label:'滚动重启 restart',risk:'mid',fields:[{k:'role',l:'角色',opts:['tikv','tidb','pd','tiflash','ticdc']}],gen:f=>`tiup cluster restart ${CLUSTER} -R ${f.role}`,
    checks:f=>[['warn',f.role==='tikv'?'TiKV 会驱逐 Leader 后逐个重启，业务有短暂抖动':'逐个重启，请确认负载均衡健康检查已配置'],['ok','避开业务高峰与备份窗口']]},
  reload:{label:'重载配置 edit-config + reload',risk:'mid',fields:[{k:'role',l:'角色',opts:['tikv','tidb','pd']}],gen:f=>`tiup cluster edit-config ${CLUSTER}\n# 修改后滚动生效\ntiup cluster reload ${CLUSTER} -R ${f.role}`,checks:[['warn','reload 为滚动重启，会有短暂抖动'],['ok','可通过 SET CONFIG 在线修改的参数无需重启']]},
  upgrade:{label:'滚动升级 upgrade',risk:'high',fields:[{k:'ver',l:'目标版本',opts:['v8.5.3','v8.5.4','v9.0.0']}],gen:f=>`tiup update --self && tiup update cluster\ntiup cluster check ${CLUSTER} --cluster\ntiup cluster upgrade ${CLUSTER} ${f.ver}`,
    checks:f=>[['bad',LIVE.on?'升级前必须完成一次全量备份（请先确认最近一次备份的时间与可恢复性）':'升级前必须完成一次全量备份（最近备份：2025-10-04 02:00）'],['warn','阅读目标版本 Release Notes 与兼容性变更'],['warn','建议先在预发环境演练'],['ok','滚动升级顺序：PD → TiKV → TiDB，业务无需停机']]},
  patch:{label:'热修复 patch',risk:'high',fields:[{k:'role',l:'角色',opts:['tikv','tidb','pd']}],gen:f=>`tiup cluster patch ${CLUSTER} ./${f.role}-hotfix.tar.gz -R ${f.role}`,checks:[['bad','仅用于官方提供的临时补丁包'],['warn','需记录回滚方案']]},
  clean:{label:'清理数据 clean',risk:'high',fields:[],gen:()=>`tiup cluster clean ${CLUSTER} --data   # 会清除数据，生产环境禁止`,checks:[['bad','破坏性操作：会清除数据目录，生产集群请勿执行']]},
};
let opsState = {tab: 'cluster', op: 'restart'};
pages.ops = {
  title: '日常运维',
  render() {
    return `<div class="tabs" id="opTabs">${[['cluster','集群操作'],['inspect','巡检'],['backup','备份与恢复'],['param','参数管理'],['audit','变更审计']].map(([k, t]) => `<button data-t="${k}" class="${opsState.tab === k ? 'active' : ''}">${t}</button>`).join('')}</div><div id="opBody"></div>`;
  },
  init() { $$('#opTabs button').forEach(b => b.onclick = () => { opsState.tab = b.dataset.t; $$('#opTabs button').forEach(x => x.classList.toggle('active', x === b)); this[opsState.tab](); }); this[opsState.tab](); },
  cluster() {
    const op = OPS[opsState.op];
    $('#opBody').innerHTML = `<div class="grid g-main">
      <div class="card"><h2>变更向导 <span class="sub">基于 tiup cluster 生成命令与前置检查</span><span class="right">${docLink('tiup-cluster')}</span></h2>
        <div class="field"><label>操作类型</label><select id="opSel">${Object.entries(OPS).map(([k, o]) => `<option value="${k}" ${k === opsState.op ? 'selected' : ''}>${o.label}</option>`).join('')}</select></div>
        <div id="opFields">${op.fields.map(f => `<div class="field"><label>${f.l}</label>${f.opts ? `<select data-k="${f.k}">${f.opts.map(o => `<option>${o}</option>`).join('')}</select>` : `<input type="text" data-k="${f.k}" value="${f.def || ''}">`}</div>`).join('')}</div>
        <div id="opOut"></div></div>
      <div class="grid" style="align-content:start"><div class="card"><h2>集群状态 <span class="sub">tiup cluster display</span></h2>
        <div class="small">${['tidb','pd','tikv','tiflash','ticdc'].map(t => { const l = NODES.filter(n => n.type === t); return `<div class="row" style="justify-content:space-between;padding:3px 0"><span>${TYPE_META[t].label}</span><span>${l.filter(n => n.status === 'up').length}/${l.length} Up ${l.some(n => n.status === 'warn') ? '<span class="tag warn">有告警</span>' : ''}</span></div>`; }).join('')}</div></div>
        <div class="card"><h2>扩容 / 缩容 / 节点操作</h2><div class="small muted" style="margin-bottom:8px">TiDB、TiKV 等的扩缩容、新增主机、单节点重启 / 停止 / 驱逐 Leader 已迁移到「节点管理」，含预检、影响评估和进度跟踪。</div><div class="row"><button class="btn primary sm" data-go="nodes" data-tab="scaleout">扩容 / 新增节点</button><button class="btn sm" data-go="nodes" data-tab="scalein">缩容 / 下线</button></div></div>
        <div class="card"><h2>变更规范</h2><ul class="small" style="padding-left:18px;margin:0"><li>变更前确认备份与回滚方案</li><li>避开业务高峰与备份窗口</li><li>高风险操作需双人复核</li><li>变更后做健康检查并观察 30 分钟</li></ul></div></div></div>`;
    $('#opSel').onchange = e => { opsState.op = e.target.value; this.cluster(); };
    $$('#opFields [data-k]').forEach(el => el.oninput = () => this.gen());
    this.gen();
  },
  gen() {
    const op = OPS[opsState.op], f = {};
    $$('#opFields [data-k]').forEach(el => f[el.dataset.k] = el.value);
    const cmd = op.gen(f), checks = typeof op.checks === 'function' ? op.checks(f) : op.checks;
    const risk = {low:['ok','低风险'], mid:['warn','中风险'], high:['bad','高风险']}[op.risk];
    const hasBad = checks.some(c => c[0] === 'bad');
    $('#opOut').innerHTML = `<div class="row" style="margin:8px 0"><b>生成命令</b><span class="tag ${risk[0]}">${risk[1]}</span></div>${code(cmd)}
      <b>前置检查</b>${checks.map(([lv, t]) => `<div class="notice ${lv}"><span>${{ok:'✔', warn:'⚠', bad:'✖'}[lv]}</span><div>${t}</div></div>`).join('')}
      <div class="row" style="margin-top:10px"><button class="btn primary" id="opSubmit" ${op.risk === 'low' ? '' : ''}>${op.risk === 'low' ? '立即执行' : '提交变更审批'}</button><button class="btn" data-copy="${esc(cmd)}">复制命令</button>${hasBad ? '<span class="small" style="color:var(--bad)">存在阻断项，需处理后才可提交</span>' : ''}</div>`;
    const b = $('#opSubmit'); b.disabled = hasBad && op.risk === 'high';
    b.onclick = () => { toast(op.risk === 'low' ? '原型：已模拟执行' : '原型：已创建变更单，等待审批'); AUDIT.unshift([new Date().toISOString().slice(0, 16).replace('T', ' '), 'you', opsState.op, cmd.split('\n').slice(-1)[0].slice(0, 60), op.risk === 'low' ? '成功' : '待审批']); };
  },
  inspect() {
    $('#opBody').innerHTML = `<div class="card"><h2>集群巡检 <span class="sub">${CHECKS.length} 项</span><span class="right"><button class="btn primary" id="inRun">执行巡检</button><button class="btn" data-print="1">导出 / 打印</button></span></h2><div id="inOut"><div class="empty">点击「执行巡检」生成报告</div></div></div>`;
    $('#inRun').onclick = () => {
      const out = $('#inOut'); let i = 0; out.innerHTML = '';
      const t = setInterval(() => {
        if (i >= CHECKS.length) { clearInterval(t); const c = k => CHECKS.filter(x => x.r === k).length; out.insertAdjacentHTML('afterbegin', `<div class="row" style="margin-bottom:10px"><span class="tag ok">通过 ${c('ok')}</span><span class="tag warn">警告 ${c('warn')}</span><span class="tag bad">异常 ${c('bad')}</span></div>`); return; }
        const c = CHECKS[i++];
        out.insertAdjacentHTML('beforeend', `<div class="row" style="padding:6px 0;border-bottom:1px solid var(--border)"><span class="tag ${c.r}" style="width:48px;text-align:center">${{ok:'通过', warn:'警告', bad:'异常'}[c.r]}</span><span class="tag">${c.g}</span><b>${c.n}</b><span class="muted small">${c.d}</span></div>`);
      }, 90);
    };
  },
  backup() {
    $('#opBody').innerHTML = `<div class="grid g3"><div class="card kpi"><div class="l">最近快照备份</div><div class="v" style="font-size:20px">2025-10-04 02:00</div><div class="d down">成功 · 1.82 TiB · 38 min</div></div>
      <div class="card kpi"><div class="l">PITR 日志备份</div><div class="v" style="font-size:20px">运行中</div><div class="d down">checkpoint lag 38s · 可恢复到任意时间点</div></div>
      <div class="card kpi"><div class="l">备份策略</div><div class="v" style="font-size:20px">每日 02:00 全量 + 持续日志</div><div class="d flat">保留 14 天</div></div></div>
      <div class="card" style="margin-top:16px"><h2>备份记录 ${docLink('br-snapshot-guide')}</h2><div class="tbl-wrap"><table><thead><tr><th>类型</th><th>ID</th><th>时间</th><th>大小</th><th>耗时</th><th>状态</th><th>存储位置</th></tr></thead><tbody>
      ${BACKUPS.map(b => `<tr><td>${b.type}</td><td class="mono small">${b.id}</td><td>${b.time}</td><td>${b.size}</td><td>${b.dur}</td><td><span class="tag ${b.status === '成功' ? 'ok' : 'bad'}">${b.status}</span></td><td class="small mono">${b.store}</td></tr>`).join('')}</tbody></table></div>
      <div class="notice warn"><span>⚠</span><div>10-01 备份失败：对象存储凭证过期。建议为备份失败配置告警，并定期做恢复演练。</div></div></div>
      <div class="grid g2" style="margin-top:16px"><div class="card"><h2>命令生成</h2>
        ${code("# 全量快照备份\ntiup br backup full --pd \"10.0.1.21:2379\" \\\n  --storage \"s3://tidb-backup/prod/snap-$(date +%Y%m%d)\" \\\n  --backupts \"$(date '+%Y-%m-%d %H:%M:%S %z')\"")}
        ${code("# 恢复指定库 / 表\ntiup br restore db --pd \"10.0.1.21:2379\" --db shop \\\n  --storage \"s3://tidb-backup/prod/snap-20251004\"\ntiup br restore table --db shop --table orders ...")}</div>
        <div class="card"><h2>注意事项</h2><ul class="small" style="padding-left:18px;margin:0"><li>备份线程 ≤ 8 且 CPU ≤ 60% 时对业务影响很小，可通过 backup.num-threads 控制</li><li>备份期间 tidb_gc_life_time 需大于备份耗时</li><li>恢复期间目标表处于 restore 模式，禁止读写</li><li>备份文件应异地存放并定期校验、演练恢复</li></ul></div></div>`;
  },
  param() {
    $('#opBody').innerHTML = `<div class="card"><h2>关键参数建议 <span class="sub">当前值 vs 推荐</span></h2><div class="tbl-wrap"><table><thead><tr><th>范围</th><th>参数</th><th>当前值</th><th>推荐</th><th>原因</th><th></th></tr></thead><tbody>
      ${PARAMS.map((p, i) => `<tr><td><span class="tag">${p.scope}</span></td><td class="mono small">${p.name}</td><td>${p.cur}</td><td>${p.rec}</td><td class="small muted">${p.why}</td><td><button class="btn sm" data-copy="${esc(p.set)}">复制命令</button></td></tr>`).join('')}</tbody></table></div>
      <div class="small muted" style="margin-top:8px">原型中的推荐值仅为示意；请结合硬件规格、版本和官方文档确定。TiDB 系统变量可在线 SET GLOBAL；TiKV / PD 配置可用 tiup edit-config + reload，部分支持 SET CONFIG 在线调整。</div></div>`;
  },
  audit() {
    $('#opBody').innerHTML = `<div class="card"><h2>变更审计 <span class="sub">对应 tiup cluster audit</span></h2><div class="tbl-wrap"><table><thead><tr><th>时间</th><th>操作人</th><th>操作</th><th>内容</th><th>结果</th></tr></thead><tbody>
      ${AUDIT.map(a => `<tr><td>${a[0]}</td><td>${a[1]}</td><td class="mono small">${a[2]}</td><td>${esc(a[3])}</td><td><span class="tag ${a[4] === '成功' ? 'ok' : a[4] === '待审批' ? 'info' : 'warn'}">${a[4]}</span></td></tr>`).join('')}</tbody></table></div></div>`;
  }
};

/* ================== 抽屉 / 路由 / 全局事件 ================== */
function openDrawer(title, html) { $('#drTitle').textContent = title; $('#drBody').innerHTML = html; $('#drawer').classList.add('open'); }
function closeDrawer() { $('#drawer').classList.remove('open', 'wide'); $$('[data-sql]').forEach(t => t.classList.remove('sel')); }

function go(name, opts = {}) {
  if (!pages[name]) name = 'overview';
  if (opts.open) pages.diag.pendingOpen = opts.open;
  if (location.hash.split('/')[0] !== '#' + name) { location.hash = name; return; }
  render(name);
}
function render(name) { name = String(name).split('/')[0];   // '#help/nodes' 这类带子路径的哈希只取页面名
  const p = pages[name] || pages.overview; closeDrawer();
  $$('.nav a').forEach(a => a.classList.toggle('active', a.dataset.go === name));
  $('#pageTitle').textContent = p.title; document.title = `${p.title}${window.CL && CL.active() && CL.name() ? ' · ' + CL.name() : ''} · TiDB 运维平台原型`;
  const gate = window.CL && CL.gate ? CL.gate(name) : '';   // 独立部署且尚未接入集群：集群数据页换成引导页
  $('#view').innerHTML = gate || p.render();
  if (!gate && p.init) p.init.call(p);
  window.scrollTo(0, 0);
}
window.addEventListener('hashchange', () => render(location.hash.slice(1) || 'overview'));

document.addEventListener('click', e => {
  const cp = e.target.closest('[data-copy]'); if (cp) { copyText(cp.dataset.copy); return; }
  const g = e.target.closest('[data-go]'); if (g) { e.preventDefault(); window.navArgs = {tab: g.dataset.tab, role: g.dataset.role, node: g.dataset.node}; go(g.dataset.go, {open: g.dataset.open}); return; }
  const nd = e.target.closest('tr[data-node]'); if (nd) { archState.sel = nd.dataset.node; go('arch'); }
});
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeDrawer(); });

/* ---------- 主题：浅色 / 深色 / 护眼（顶栏与登录页共用 data-theme-sw 容器 + data-theme-t 按钮，文档级委托） ---------- */
const THEMES = [['light', '☀ 浅色'], ['dark', '☾ 深色'], ['eye', '🌿 护眼']];
const curTheme = () => { const t = document.documentElement.dataset.theme; return THEMES.some(x => x[0] === t) ? t : 'light'; };
function paintThemeSw() {
  $$('[data-theme-sw]').forEach(el => { el.innerHTML = THEMES.map(([id, n]) => `<button type="button" data-theme-t="${id}" class="${curTheme() === id ? 'on' : ''}" title="${n.slice(2)}主题" aria-pressed="${curTheme() === id}">${n}</button>`).join(''); });
}
function setTheme(t) {
  if (!THEMES.some(x => x[0] === t)) t = 'light';
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem('theme', t); } catch (e) {}
  paintThemeSw();
  const cur = (location.hash.slice(1) || 'overview').split('/')[0]; if (cur === 'arch' && pages.arch.draw) pages.arch.draw();   // 拓扑图是 SVG，换主题后重画
}
document.addEventListener('click', e => { const b = e.target.closest('[data-theme-t]'); if (b) { e.preventDefault(); setTheme(b.dataset.themeT); } });
(function initTheme() {
  let t = null; try { t = localStorage.getItem('theme'); } catch (e) {}
  if (!THEMES.some(x => x[0] === t)) t = 'light';   // 默认浅色（不跟随系统深色模式）；用户在顶栏切换过的主题记在 localStorage
  document.documentElement.dataset.theme = t;
  paintThemeSw();
})();
function boot() {
  $('#alertBadge').textContent = ALERTS.filter(a => a.lv !== 'info').length;
  render(location.hash.slice(1) || 'overview');
}
