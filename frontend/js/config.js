// @author: kevinkelly
// @license: Apache License 2.0
// @file: config.js
// @time: 2026/10/28

/* 节点详情抽屉：概览 / 配置变更 / 变更历史 / 操作。配置变更含校验、diff 预览、在线下发与滚动 reload、回滚、漂移检测。 */
const CLUSTER_CFG = {};
const ndState = {id: null, tab: 'overview', draft: {}, scope: 'instance', stage: 'edit', ack: false, confirm: ''};
const catalog = r => CONFIG_CATALOG[r] || [];
/* 真实数据模式下后端不提供主机规格：返回 null，validateCfg 里依赖主机内存 / CPU 的检查会被跳过（不用模拟规格误导用户）。 */
const hostMem = n => LIVE.on ? null : (SPEC[n.type] || {mem: 64}).mem;
const hostCores = n => LIVE.on ? null : (SPEC[n.type] || {cores: 16}).cores;
const shortKey = k => k.split('.').pop();
const statusAddr = n => n.type === 'tikv' ? `${n.host}:${n.port + 20}` : n.type === 'tidb' ? `${n.host}:10080` : `${n.host}:${n.port}`;
const SIZE_UNIT = {kb: 1 / 1048576, kib: 1 / 1048576, mb: 1 / 1024, mib: 1 / 1024, gb: 1, gib: 1, tb: 1024, tib: 1024};

/* ---------- 真实数据模式：配置当前值取自 /api/config-values ----------
   CFGV.data = {values:{角色:{配置键:{cluster, instances:{"host:port":值}, drift, majority, norm, instNorm?}}}}；取不到的项后端省略。
   getVal 在真实模式下取不到时返回 null（界面显示「—（未采集）」，仍允许修改）。 */
const CFGV = {state: 'idle', data: null, at: 0, rev: 0, err: '', p: null, sync: {}};
const NOVAL = '—（未采集）';
const curStr = v => v === null || v === undefined ? '' : String(v);
async function cfgLoad(refresh) {
  if (!LIVE.on) return null;
  if (window.CL && CL.empty()) return null;   // 独立部署且尚未接入集群
  if (CFGV.p) return refresh && !CFGV.pr ? CFGV.p.then(() => cfgLoad(true)) : CFGV.p;
  CFGV.state = 'loading'; CFGV.err = ''; CFGV.pr = !!refresh;
  CFGV.p = (async () => {
    try { const d = await liveGet('/api/config-values' + (refresh ? '?refresh=1' : '')); CFGV.data = d && d.values ? d : {values: {}}; CFGV.at = Date.now(); CFGV.rev++; CFGV.state = 'ok'; }
    catch (e) { CFGV.err = e.message || String(e); CFGV.state = 'err'; }
    finally { CFGV.p = null; }
    return CFGV.data;
  })();
  return CFGV.p;
}
const cfgEntry = (role, k) => CFGV.data && CFGV.data.values && CFGV.data.values[role] ? CFGV.data.values[role][k] || null : null;
/* 实例在 cluster_config 里的地址：优先 host:port，其次状态地址；都不在时，若该 host 在此配置项下只有一个实例则采用 */
function cfgInstKey(n, entry) {
  const insts = entry && entry.instances; if (!insts) return null;
  for (const a of [`${n.host}:${n.port}`, statusAddr(n)]) if (a in insts) return a;
  const hit = Object.keys(insts).filter(a => a.startsWith(n.host + ':'));
  return hit.length === 1 ? hit[0] : null;
}
/* 某配置项在某节点上的真实当前值（归一化形式，取不到为 null） */
function cfgRealVal(n, i) {
  const e = cfgEntry(n.type, i.k); if (!e) return null;
  if (i.scope === 'cluster') return e.cluster === undefined || e.cluster === null ? null : (e.norm || e.cluster);
  const a = cfgInstKey(n, e); if (a === null) return null;
  return (e.instNorm && e.instNorm[a]) || e.instances[a];
}
function cfgDrift(n, i) {
  if (LIVE.on) {
    const e = cfgEntry(n.type, i.k), cur = cfgRealVal(n, i);
    if (i.scope !== 'instance' || !e || !e.drift || cur === null) return {drift: false, maj: null};
    const maj = e.norm || e.majority || '';
    return {drift: maj !== '' && String(cur) !== String(maj), maj};
  }
  const maj = i.scope === 'instance' ? majorityVal(n, i) : null;
  return {drift: maj !== null && String(getVal(n, i)) !== maj, maj};
}

function clusterCfg(role) {
  if (!CLUSTER_CFG[role]) { CLUSTER_CFG[role] = {}; if (!LIVE.on) catalog(role).filter(i => i.scope === 'cluster').forEach(i => { CLUSTER_CFG[role][i.k] = i.def; }); }
  const o = CLUSTER_CFG[role];
  if (LIVE.on && CFGV.sync[role] !== CFGV.rev) {   // 真实模式：每次重新取到真实值后同步一次（之后前端模拟的写入保留到下次同步）
    CFGV.sync[role] = CFGV.rev;
    catalog(role).filter(i => i.scope === 'cluster').forEach(i => { const e = cfgEntry(role, i.k); o[i.k] = e && e.cluster !== undefined && e.cluster !== null ? (e.norm || e.cluster) : null; });
  }
  return o;
}
function nodeCfg(n) {
  if (LIVE.on) { const o = {}; catalog(n.type).filter(i => i.scope === 'instance').forEach(i => { o[i.k] = cfgRealVal(n, i); }); return o; }
  if (!n.cfg) {
    n.cfg = {}; catalog(n.type).filter(i => i.scope === 'instance').forEach(i => { n.cfg[i.k] = i.def; });
    if (n.id === 'tikv-3') n.cfg['raftstore.store-pool-size'] = 3;   // 演示：历史上被单独调过 → 配置漂移
  }
  return n.cfg;
}
const getVal = (n, i) => LIVE.on ? cfgRealVal(n, i) : (i.scope === 'cluster' ? clusterCfg(n.type)[i.k] : nodeCfg(n)[i.k]);
const parseSize = s => { const m = /^(\d+(?:\.\d+)?)\s*(kb|kib|mb|mib|gb|gib|tb|tib)$/i.exec(String(s).trim()); return m ? +m[1] * SIZE_UNIT[m[2].toLowerCase()] : null; };
const parseDur = s => { const m = /^(\d+)\s*(ms|s|m|h)$/.exec(String(s).trim()); return m ? +m[1] * {ms: 1 / 60000, s: 1 / 60, m: 1, h: 60}[m[2]] : null; };
const pdThreshMin = () => parseDur(clusterCfg('pd')['schedule.max-store-down-time']) || 30;

/* 校验 + 归一化：返回 {v, err, warn} */
function validateCfg(n, i, raw) {
  raw = String(raw).trim(); const r = {v: raw, err: '', warn: ''};
  if (i.type === 'int') {
    if (!/^-?\d+$/.test(raw)) { r.err = '需要整数'; return r; }
    r.v = +raw; if (r.v < i.min || r.v > i.max) { r.err = `取值范围 ${i.min} – ${i.max}`; return r; }
  } else if (i.type === 'size') {
    if (parseSize(raw) === null) { r.err = '格式如 16GB / 512MB'; return r; }
    r.v = raw.replace(/\s+/g, '').replace(/(kb|kib|mb|mib|gb|gib|tb|tib)$/i, u => u.toUpperCase().replace('IB', 'iB').replace('IB', 'B'));
  } else if (i.type === 'duration') {
    if (parseDur(raw) === null) { r.err = '格式如 30m / 2h / 90s'; return r; }
  } else if (i.type === 'bool') {
    r.v = raw.toUpperCase(); if (!['ON', 'OFF'].includes(r.v)) { r.err = '只能是 ON / OFF'; return r; }
  } else if (i.type === 'enum' && !i.opts.includes(raw)) { r.err = `可选：${i.opts.join(' / ')}`; return r; }
  const mem = hostMem(n), cores = hostCores(n), gb = i.type === 'size' ? parseSize(r.v) : 0, nTikv = NODES.filter(x => x.type === 'tikv').length;   // mem / cores 为 null：取不到主机规格，跳过相关检查
  switch (i.k) {
    case 'storage.block-cache.capacity':
      if (gb < 1) r.err = '不能小于 1GB'; else if (mem === null) break; else if (gb > mem * .8) r.err = `超过主机内存 ${mem}GB 的 80%，必然 OOM`;
      else if (gb > mem * .45) r.warn = `超过内存 ${mem}GB 的 45%（${(mem * .45).toFixed(0)}GB），有 OOM 风险`; break;
    case 'raftstore.store-pool-size': if (r.v > 5) r.warn = '常用范围 1–5，过大会抢占其他线程的 CPU'; break;
    case 'storage.scheduler-worker-pool-size': if (cores !== null && r.v > cores) r.warn = `大于 CPU 核数 ${cores}，收益有限且增加切换开销`; break;
    case 'tidb_mem_quota_query': if (mem !== null && gb > mem * .5) r.warn = `大于 TiDB 节点内存 ${mem}GB 的一半，少数大查询即可让节点 OOM`; if (gb < .1) r.warn = '过小会让正常查询被取消'; break;
    case 'schedule.max-store-down-time': { const m = parseDur(r.v); if (m < 10) r.warn = '过短：TiKV 短暂重启也会被判定 Down，引发不必要的补副本'; else if (m > 180) r.warn = '过长：真实故障时副本缺失窗口会很长，风险升高'; break; }
    case 'replication.max-replicas':
      if (r.v > nTikv) r.err = `大于 TiKV 数量 ${nTikv}，副本无法补齐`; else if (r.v % 2 === 0) r.warn = '偶数副本不提升容错，建议使用奇数';
      else if (r.v !== (LIVE.on && getVal(n, i) !== null ? +getVal(n, i) : i.def)) r.warn = '修改副本数将触发全量 Region 补齐 / 删除，I/O 与网络压力极大'; break;
    case 'schedule.region-schedule-limit': if (r.v > 4096) r.warn = '调度并发过高会影响线上延迟'; break;
    case 'tidb_distsql_scan_concurrency': if (r.v > 64) r.warn = '并发过高会加重 TiKV 负载'; break;
    case 'log.level': case 'logger.level': case 'log-level': if (r.v === 'debug') r.warn = 'debug 日志量巨大，会明显影响性能，仅限短时排障'; break;
  }
  return r;
}
const fmtV = v => v === null || v === undefined ? NOVAL : String(v);
const sqlVal = v => typeof v === 'number' ? String(v) : `'${v}'`;

/* 当前草稿 → 变更列表（过滤掉无变化的项） */
function draftChanges(n) {
  const out = [];
  catalog(n.type).forEach(i => {
    if (!(i.k in ndState.draft)) return;
    const val = validateCfg(n, i, ndState.draft[i.k]), cur = getVal(n, i);
    if (String(val.v) !== curStr(cur) || val.err) out.push({i, from: cur === undefined ? null : cur, to: val.v, err: val.err, warn: val.warn});
  });
  return out;
}
function affectedNodes(n, i, scope = ndState.scope) {
  if (i.scope === 'cluster') return NODES.filter(x => x.type === n.type);
  return scope === 'role' ? NODES.filter(x => x.type === n.type) : [n];
}
function cfgCommands(n, changes, scope = ndState.scope) {
  const online = [], persist = [], reload = [], inst = scope === 'instance';
  const yamlRole = {};
  changes.forEach(({i, to}) => {
    if (i.sys) online.push(`SET GLOBAL ${i.k} = ${sqlVal(to)};`);
    else if (n.type === 'pd' && i.scope === 'cluster') online.push(`${PDCTL()} config set ${shortKey(i.k)} ${to}`);
    else {
      if (i.online) online.push(inst ? `SET CONFIG "${statusAddr(n)}" \`${i.k}\` = ${sqlVal(to)};` : `SET CONFIG ${n.type} \`${i.k}\` = ${sqlVal(to)};`);
      (yamlRole[i.k] = {to, online: i.online});
    }
  });
  const keys = Object.keys(yamlRole);
  if (keys.length) {
    const body = keys.map(k => `      ${k}: ${typeof yamlRole[k].to === 'number' ? yamlRole[k].to : `"${yamlRole[k].to}"`}`).join('\n');
    const yaml = inst
      ? `# tiup cluster edit-config ${CLUSTER}\n${{tikv: 'tikv', tidb: 'tidb', pd: 'pd', tiflash: 'tiflash', ticdc: 'cdc'}[n.type]}_servers:\n  - host: ${n.host}\n    port: ${n.port}\n    config:\n${body.replace(/^ {6}/gm, '      ')}`
      : `# tiup cluster edit-config ${CLUSTER}\nserver_configs:\n  ${n.type === 'ticdc' ? 'cdc' : n.type}:\n${body.replace(/^ {6}/gm, '    ')}`;
    persist.push(yaml);
    if (changes.some(c => !c.i.online)) reload.push(inst ? `tiup cluster reload ${CLUSTER} -N ${n.host}:${n.port}` : `tiup cluster reload ${CLUSTER} -R ${n.type === 'ticdc' ? 'cdc' : n.type}`);
  }
  return {online, persist, reload};
}
function cfgChecks(n, changes, scope = ndState.scope) {
  const checks = [];
  const reloadItems = changes.filter(c => !c.i.online && c.i.scope !== 'cluster');
  const targets = [...new Set(changes.flatMap(c => affectedNodes(n, c.i, scope)))];
  targets.forEach(t => {
    if (busy(t.id)) checks.push(['bad', `${t.id} 有进行中的任务，请等待完成后再变更`]);
    if (inMaint(t.host)) checks.push(['bad', `${t.id} 所在主机 ${t.host} 正在维护，不能变更配置`]);
  });
  if (reloadItems.length) {
    const roll = scope === 'role' ? NODES.filter(x => x.type === n.type) : [n];
    checks.push(['warn', `需滚动 reload ${roll.length} 个 ${TYPE_META[n.type].label}，逐个重启（TiKV 先驱逐 Leader），每个约 2–3 分钟`]);
    if (n.type === 'tikv' && typeof MAINTS !== 'undefined' && MAINTS.some(m => MAINT_STATES.includes(m.state) && NODES.some(x => x.host === m.ip && x.type === 'tikv')))
      checks.push(['bad', '当前有 TiKV 主机处于维护中，此时再滚动重启 TiKV 可能导致部分 Region 失去多数派']);
    if (roll.length === 1 && NODES.filter(x => x.type === n.type && (x.status === 'up' || x.status === 'warn')).length <= 1) checks.push(['bad', `${TYPE_META[n.type].label} 只有 1 个健康实例，reload 期间服务不可用`]);
    if (n.type === 'pd' && roll.some(x => x.leader)) checks.push(['warn', 'PD Leader 在 reload 范围内，会触发选举，TSO 有秒级抖动']);
  } else checks.push(['ok', '全部为在线生效项，无需重启实例']);
  changes.forEach(({i, to}) => {
    if (i.k === 'schedule.max-store-down-time' && typeof MAINTS !== 'undefined' && MAINTS.some(m => m.state === 'active')) checks.push(['warn', '当前有进行中的主机维护，修改该阈值会影响其"是否触发补副本"的判断']);
    if (i.danger) checks.push(['bad', `${i.k} 是高危参数：修改会触发全量副本变更，请确认已评估容量并安排低峰`]);
    void to;
  });
  checks.push(['ok', '变更前将备份当前配置（tiup cluster edit-config 快照），可随时回滚']);
  return checks;
}

/* 构造执行任务 */
function buildCfgChange(n, changes, scope) {
  const targets = [...new Set(changes.flatMap(c => affectedNodes(n, c.i, scope)))].filter(x => x.status !== 'offline');
  const hasOnline = changes.some(c => c.i.online), hasReload = changes.some(c => !c.i.online && c.i.scope !== 'cluster');
  const reloadNodes = hasReload ? (scope === 'role' ? NODES.filter(x => x.type === n.type) : [n]) : [];
  const cmds = cfgCommands(n, changes, scope);
  const steps = [{name: '校验参数并备份当前配置（edit-config 快照）', ms: 900, log: `已备份：backup-${new Date().toISOString().slice(0, 16).replace(/[-:T]/g, '')}`}];
  if (hasOnline) steps.push({name: `在线下发配置到 ${targets.length} 个实例`, ms: 1500, ticks: 3, log: '在线配置已生效，无需重启'});
  if (cmds.persist.length) steps.push({name: '写入 tiup 拓扑（edit-config），避免之后 reload 被覆盖', ms: 800});
  reloadNodes.forEach(x => {
    let l0 = 0;
    steps.push({name: `滚动 reload ${x.id}${x.type === 'tikv' ? '（驱逐 Leader → 重启 → 回迁）' : ''}`, ms: 2300, ticks: 5,
      start: () => { l0 = x.leaders || 0; },
      fn: p => {
        if (x.type === 'tikv') x.leaders = p <= .4 ? Math.round(l0 * (1 - p / .4)) : p <= .6 ? 0 : Math.round(l0 * (p - .6) / .4);
        x.status = p > .4 && p < .8 ? 'down' : 'up';
      },
      end: () => { x.status = 'up'; x.note = ''; if (x.type === 'tikv') x.leaders = l0; }});
  });
  steps.push({name: '验证生效（SHOW CONFIG / 日志检查）并记录变更', ms: 800, end: t => {
    const taskNo = t.id;
    changes.forEach(({i, from, to}) => {
      if (i.scope === 'cluster') clusterCfg(n.type)[i.k] = to; else affectedNodes(n, i, scope).forEach(x => { nodeCfg(x)[i.k] = to; });
      CFG_HISTORY.unshift({ts: new Date().toLocaleString('sv').slice(0, 16), user: 'you', role: n.type, k: i.k, from: fmtV(from), to: fmtV(to), mode: i.online ? 'online' : 'reload', status: '已生效', task: taskNo,
        target: i.scope === 'cluster' ? '集群' : scope === 'role' ? `全部 ${TYPE_META[n.type].label}` : n.id, nodeId: i.scope === 'instance' && scope === 'instance' ? n.id : undefined});
    });
  }, log: '配置验证通过'});
  return {title: `配置变更 · ${changes.map(c => shortKey(c.i.k)).join('、')}（${scope === 'role' && changes.some(c => c.i.scope === 'instance') ? '全部 ' + TYPE_META[n.type].label : n.id}）`,
    cmds: [...cmds.online, ...cmds.persist, ...cmds.reload].filter(Boolean), steps, targets: targets.map(x => x.id), abortable: false,
    intent: {kind: 'config.set', params: {node: n.id, scope, changes: changes.map(c => ({key: c.i.k, value: String(c.to), from: curStr(c.from)}))}}};
}

/* ================== 抽屉渲染 ================== */
function openNodeDrawer(id, tab) {
  const n = nodeBy(id); if (!n) return;
  if (ndState.id !== id) Object.assign(ndState, {id, draft: {}, scope: 'instance', stage: 'edit', ack: false, confirm: ''});
  ndState.tab = tab || ndState.tab || 'overview';
  openDrawer(`${n.id} · ${TYPE_META[n.type].label}`, '<div id="ndWrap"></div>');
  $('#drawer').classList.add('wide'); renderNd();
  if (LIVE.on) {   // 真实数据模式：进入时异步读取一次配置当前值（refresh=1），读取期间配置页显示骨架
    const pr = cfgLoad(true); renderNd();
    pr.then(() => { if (ndState.id === id && $('#ndWrap')) renderNd(); });
  }
}
function renderNd() {
  const n = nodeBy(ndState.id), el = $('#ndWrap'); if (!n || !el) return;
  const t = ndState.tab;
  el.innerHTML = `<div class="row" style="margin-bottom:10px">${tag(stText[n.status], stCls(n.status))}<span class="mono small muted">${esc(n.host)}:${esc(n.port)}</span><span class="small muted">${esc(AZS[n.az])} · ${esc(VERSION)}</span>${inMaint(n.host) ? tag('主机维护中', 'warn') : ''}${busy(n.id) ? tag('任务进行中', 'info') : ''}</div>
    <div class="dtabs">${[['overview', '概览'], ['config', '配置变更'], ['history', '变更历史'], ['ops', '操作']].map(([k, l]) => `<button data-na="nd-tab" data-t="${k}" class="${t === k ? 'active' : ''}">${l}${k === 'config' && Object.keys(ndState.draft).length ? ` <span class="tag info">${draftChanges(n).length}</span>` : ''}</button>`).join('')}</div>
    <div id="ndPane">${t === 'overview' ? ndOverview(n) : t === 'config' ? (ndState.stage === 'preview' ? cfgPreview(n) : cfgEdit(n)) : t === 'history' ? ndHistory(n) : ndOps(n)}</div>`;
}
function ndOverview(n) {
  const h = [...n.id].reduce((a, c) => a + c.charCodeAt(0), 0), days = 3 + h % 40, pid = 10000 + h * 37 % 50000;
  const lab = n.labels || {zone: n.az, rack: '—', host: n.host.replace(/\./g, '-')}, rel = FINDINGS.filter(f => f.scope.includes(n.id));
  // 目录：真实数据模式取自后端（tiup display / meta.yaml），logDirInferred 标「推断」，取不到显示「—（未采集）」；静态原型模式沿用 tiup 默认约定的演示路径
  const dir = (v, inf) => v ? `${esc(v)}${inf ? ' <span class="muted">（推断）</span>' : ''}` : NOVAL;
  const dirs = LIVE.on
    ? `<span>部署目录</span><span class="mono small">${dir(n.deployDir)}</span>${n.type !== 'tidb' || n.dataDir ? `<span>数据目录</span><span class="mono small">${dir(n.dataDir)}</span>` : ''}<span>日志目录</span><span class="mono small">${dir(n.logDir, n.logDirInferred)}</span>`
    : `<span>部署目录</span><span class="mono small">/tidb-deploy/${n.type}-${n.port}</span>${n.type !== 'tidb' ? `<span>数据目录</span><span class="mono small">/tidb-data/${n.type}-${n.port}</span>` : ''}<span>日志目录</span><span class="mono small">/tidb-deploy/${n.type}-${n.port}/log</span>`;
  return `<div class="grid g2"><div class="card"><h2>实例信息</h2><div class="kv">
      <span>实例</span><span>${esc(n.id)}</span><span>角色</span><span>${TYPE_META[n.type].label}</span><span>服务地址</span><span class="mono">${esc(n.host)}:${esc(n.port)}</span>
      <span>状态地址</span><span class="mono">${esc(statusAddr(n))}</span><span>拓扑标签</span><span class="mono small">zone=${esc(lab.zone)}, rack=${esc(lab.rack)}, host=${esc(lab.host)}</span>
      ${dirs}<span>进程 PID</span><span>${LIVE.on ? NOVAL : pid}</span><span>已运行</span><span>${LIVE.on ? NOVAL : n.status === 'down' ? '—（已停止）' : days + ' 天'}</span>
      ${n.sid ? `<span>Store ID</span><span>${n.sid}</span>` : ''}</div></div>
    <div class="card"><h2>资源使用</h2>
      <div class="mrow"><span>CPU</span>${meter(n.cpu)}<span>${n.cpu}%</span></div><div class="mrow"><span>内存</span>${meter(n.mem)}<span>${n.mem}%</span></div><div class="mrow"><span>磁盘</span>${meter(n.disk, 75, 85)}<span>${n.disk}%</span></div>
      ${n.type === 'tikv' ? `<div class="small" style="margin-top:8px"><b>Region</b> ${fmt(n.regions)} · <b>Leader</b> ${fmt(n.leaders)} · <b>写</b> ${n.wr} MB/s · <b>读</b> ${n.rd} MB/s</div>` : ''}
      ${n.type === 'tidb' ? `<div class="small" style="margin-top:8px"><b>QPS</b> ${fmt(n.qps)} · <b>连接数</b> ${n.conn}</div>` : ''}
      ${n.note ? `<div class="notice ${n.status === 'warn' ? 'warn' : 'info'}"><span>ℹ</span><div>${esc(n.note)}</div></div>` : ''}</div></div>
    ${rel.length ? `<div class="card" style="margin-top:12px"><h2>关联诊断</h2>${rel.map(f => `<div style="margin-bottom:6px"><span class="tag ${sevTag[f.sev]}">${sevText[f.sev]}</span> <a data-go="diag" data-open="${esc(f.id)}" data-close="1">${esc(f.title)}</a></div>`).join('')}</div>` : ''}
    <div class="row" style="margin-top:12px"><button class="btn primary" data-na="nd-tab" data-t="config">配置变更 →</button><button class="btn" data-na="nd-goto" data-t="hosts">查看所在主机 →</button></div>`;
}
function majorityVal(n, i) {
  const cnt = {}; NODES.filter(x => x.type === n.type).forEach(x => { const v = String(nodeCfg(x)[i.k]); cnt[v] = (cnt[v] || 0) + 1; });
  const top = Object.entries(cnt).sort((a, b) => b[1] - a[1])[0]; return top && top[1] > 1 ? top[0] : null;
}
function cfgRow(n, i) {
  const cur = getVal(n, i), has = i.k in ndState.draft, shown = has ? ndState.draft[i.k] : curStr(cur), unk = cur === null || cur === undefined;
  const {drift, maj} = cfgDrift(n, i), chg = has && String(shown) !== curStr(cur);
  const input = i.type === 'bool' || i.type === 'enum'
    ? `<select data-nc="cfg-draft" data-k="${i.k}">${unk ? `<option value="" ${shown === '' ? 'selected' : ''}>${NOVAL}</option>` : ''}${(i.type === 'bool' ? ['ON', 'OFF'] : i.opts).map(o => `<option ${String(shown) === o ? 'selected' : ''}>${o}</option>`).join('')}</select>`
    : `<input type="text" data-ni="cfg-draft" data-k="${i.k}" value="${esc(shown)}" ${unk ? `placeholder="${NOVAL}"` : ''} ${n.status === 'offline' ? 'disabled' : ''}>`;
  return `<div class="cfg ${chg ? 'chg' : ''}" data-row="${i.k}">
    <div><b>${i.label}</b><div class="k">${i.k}</div><div class="row" style="gap:4px;margin-top:4px">${tag(i.online ? '在线生效' : '需 reload', i.online ? 'ok' : 'warn')}${tag(i.scope === 'cluster' ? '集群级' : '实例级')}${drift ? tag('配置漂移', 'bad') : ''}</div></div>
    <div>${input}<div class="d">默认 ${fmtV(i.def)}${LIVE.on ? ` · 当前 ${unk ? `<span class="muted">${NOVAL}</span>` : esc(fmtV(cur))}` : chg ? ` · 当前 ${fmtV(cur)}` : ''}</div><div data-msg="${i.k}"></div></div>
    <div class="d">${i.desc}${drift ? `<div style="color:var(--bad);margin-top:4px">与同角色多数实例（${esc(maj)}）不一致 <a data-na="cfg-align" data-k="${i.k}" data-v="${esc(maj)}">对齐 →</a></div>` : ''}</div></div>`;
}
function cfgEdit(n) {
  const items = catalog(n.type), inst = items.filter(i => i.scope === 'instance'), clu = items.filter(i => i.scope === 'cluster');
  if (LIVE.on && CFGV.state === 'loading') return `<div class="small muted" style="margin-bottom:8px">正在读取集群里的配置当前值（/api/config-values）…</div>${items.map(() => '<div class="cfg"><div><div class="skel" style="width:60%"></div><div class="skel" style="width:85%;margin-top:6px;height:10px"></div></div><div><div class="skel" style="width:100%;height:30px"></div></div><div><div class="skel" style="width:90%"></div></div></div>').join('')}`;
  const nChg = draftChanges(n).length, roleN = NODES.filter(x => x.type === n.type).length, anyInst = Object.keys(ndState.draft).some(k => (items.find(i => i.k === k) || {}).scope === 'instance');
  const unkN = LIVE.on ? items.filter(i => getVal(n, i) === null).length : 0;
  return `${LIVE.on && CFGV.state === 'err' ? `<div class="notice warn"><span>⚠</span><div>读取配置当前值失败：${esc(CFGV.err)}。以下各项显示「${NOVAL}」，仍可修改；<a data-na="cfg-reload">重新读取</a></div></div>` : LIVE.on && unkN ? `<div class="notice info"><span>ℹ</span><div>${unkN} 项没有取到当前值（显示「${NOVAL}」），仍可修改；提交时「原值」会留空。</div></div>` : ''}
    <div class="small muted" style="margin-bottom:8px">${LIVE.on ? '「当前」为从集群读取的真实值（' + (CFGV.at ? new Date(CFGV.at).toLocaleTimeString() : '—') + '）。' : ''}在输入框中修改后点击「预览变更」，会先校验、展示 diff 与将执行的命令，确认后才会下发。</div>
    ${inst.length ? `<h3 style="font-size:14px;margin:6px 0">实例级配置 <span class="small muted">（${n.id}；可选择应用到全部 ${roleN} 个实例）</span></h3>${inst.map(i => cfgRow(n, i)).join('')}` : ''}
    ${clu.length ? `<h3 style="font-size:14px;margin:16px 0 6px">集群级配置 <span class="small muted">（所有 ${TYPE_META[n.type].label} 共享，修改影响整个集群）</span></h3>${clu.map(i => cfgRow(n, i)).join('')}` : ''}
    <div class="sticky-actions"><button class="btn primary" id="cfgPrev" data-na="cfg-preview" ${nChg ? '' : 'disabled'}>预览变更（<span id="cfgCnt">${nChg}</span>）</button><button class="btn" data-na="cfg-reset" ${nChg ? '' : 'disabled'} id="cfgReset">重置</button>
      <span id="cfgScopeBox" style="${anyInst ? '' : 'display:none'}" class="small"> 应用范围：<label><input type="radio" name="cfgScope" data-nc="cfg-scope" value="instance" ${ndState.scope === 'instance' ? 'checked' : ''}> 仅 ${n.id}</label> <label><input type="radio" name="cfgScope" data-nc="cfg-scope" value="role" ${ndState.scope === 'role' ? 'checked' : ''}> 全部 ${roleN} 个 ${TYPE_META[n.type].label}</label></span></div>`;
}
const cfgDenied = () => opDenied('config');
function cfgPreview(n) {
  const ch = draftChanges(n), err = ch.some(c => c.err), checks = cfgChecks(n, ch), blocked = err || checks.some(c => c[0] === 'bad');
  const cmds = cfgCommands(n, ch), hasReload = ch.some(c => !c.i.online && c.i.scope !== 'cluster'), high = ch.some(c => c.i.danger);
  const roleN = NODES.filter(x => x.type === n.type).length;
  return `<h3 style="font-size:14px;margin:0 0 8px">变更预览 <span class="small muted">${ch.some(c => c.i.scope === 'instance') ? (ndState.scope === 'role' ? `应用到全部 ${roleN} 个 ${TYPE_META[n.type].label}` : `仅 ${n.id}`) : '集群级'}</span></h3>
    <div class="tbl-wrap"><table><thead><tr><th>配置项</th><th>变更</th><th>生效方式</th><th>校验</th></tr></thead><tbody>
    ${ch.map(c => `<tr><td><div>${c.i.label}</div><div class="mono small muted">${c.i.k}</div></td><td><span class="diff-old">${fmtV(c.from)}</span> → <span class="diff-new">${fmtV(c.to)}</span></td><td>${tag(c.i.online ? '在线生效' : '需 reload', c.i.online ? 'ok' : 'warn')}</td>
      <td class="small">${c.err ? `<span style="color:var(--bad)">✖ ${c.err}</span>` : c.warn ? `<span style="color:var(--warn)">⚠ ${c.warn}</span>` : '<span style="color:var(--ok)">✔ 通过</span>'}</td></tr>`).join('')}</tbody></table></div>
    <h3 style="font-size:14px;margin:14px 0 4px">风险与影响</h3>${checks.map(([lv, t]) => `<div class="notice ${lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖'}[lv]}</span><div>${t}</div></div>`).join('')}
    <h3 style="font-size:14px;margin:14px 0 4px">将执行的命令</h3>${code([...cmds.online, ...cmds.persist, ...cmds.reload].join('\n\n'))}
    ${cmds.online.length && cmds.persist.length ? '<div class="small muted">在线下发后同步写入 tiup 拓扑，避免之后 reload / 升级时被旧配置覆盖。</div>' : ''}
    ${high && !blocked ? '' : ''}
    <div class="sticky-actions"><button class="btn" data-na="cfg-back">← 返回修改</button>
      ${blocked ? '<span class="small" style="color:var(--bad)">存在错误 / 阻断项，请返回修改</span>' : `<label class="small"><input type="checkbox" data-nc="cfg-ack" ${ndState.ack ? 'checked' : ''}> 我已确认影响${hasReload ? '（含滚动重启）' : ''}</label>
        ${high ? `<input type="text" data-ni="cfg-confirm" placeholder="高危变更，输入「确认」" value="${esc(ndState.confirm)}" style="width:180px">` : ''}
        <button class="btn primary" id="cfgSubmit" data-na="cfg-submit" ${cfgDenied() ? OP_DENY_ATTR : ndState.ack && (!high || ndState.confirm === '确认') ? '' : 'disabled'}>${OP.on ? '提交变更申请…' : '提交变更（模拟）'}</button>
        ${OP.on ? '' : '<button class="btn" data-na="sched-from" data-src="config">预约到低峰窗口…</button>'}`}</div>`;
}
function ndHistory(n) {
  if (OP.on) {   // 运维模式：来自服务端的配置变更记录
    const list = OP.changes.filter(c => c.kind === 'config.set' && c.params && (c.params.node === n.id || (c.params.scope === 'role' && (nodeBy(c.params.node) || {}).type === n.type)));
    if (!list.length) return '<div class="empty">该实例暂无配置变更记录（服务端）</div>';
    return `<div class="timeline">${list.map(c => `<div class="tl"><div class="row"><b>${opFmtTime(c.created)}</b><span class="small muted">${esc(c.applicant)}</span>${opStateTag(c.state)}${opRiskTag(c.risk)}<a data-na="oc-goto" data-id="${c.id}" class="small mono">${c.id}</a></div>
      ${(c.params.changes || []).map(x => `<div class="mono small">${esc(x.key)}：<span class="diff-old">${esc(x.from || '—')}</span> → <span class="diff-new">${esc(x.value)}</span></div>`).join('')}</div>`).join('')}</div>`;
  }
  const list = CFG_HISTORY.map((h, idx) => ({h, idx})).filter(({h}) => h.nodeId === n.id || (!h.nodeId && h.role === n.type));
  if (!list.length) return '<div class="empty">该实例暂无配置变更记录</div>';
  return `<div class="timeline">${list.map(({h, idx}) => `<div class="tl"><div class="row"><b>${h.ts}</b><span class="small muted">${h.user}</span>${tag(h.target)}${tag(h.mode === 'online' ? '在线生效' : '重载生效', h.mode === 'online' ? 'ok' : 'warn')}${tag(h.status, h.status === '已生效' ? 'ok' : '')}${h.task ? `<span class="small muted mono">${h.task}</span>` : ''}</div>
    <div class="mono small">${h.k}：<span class="diff-old">${esc(h.from)}</span> → <span class="diff-new">${esc(h.to)}</span></div>
    ${h.status === '已生效' && catalog(n.type).some(i => i.k === h.k) ? `<button class="btn sm" style="margin-top:4px" data-na="cfg-rollback" data-i="${idx}">回滚到 ${esc(h.from)}</button>` : ''}</div>`).join('')}</div>`;
}
function ndOps(n) {
  const down = n.status === 'down', blk = busy(n.id) || inMaint(n.host) || n.status === 'joining' || n.status === 'offline';
  const dis = opDenied('inst.op') ? opDeniedAttr('inst.op') : blk ? 'disabled' : '', disScale = opDenied('scale') ? opDeniedAttr('scale') : dis;
  return `<div class="grid g2"><div class="card"><h2>实例操作</h2><div class="acts">
      ${down ? `<button class="btn" data-na="ia" data-act="start" data-id="${n.id}" ${dis}>启动</button>` : `<button class="btn" data-na="ia" data-act="restart" data-id="${n.id}" ${dis}>重启</button><button class="btn" data-na="ia" data-act="stop" data-id="${n.id}" ${dis}>停止</button>`}
      ${n.type === 'tikv' && !down ? (n.evict || OP.on ? `<button class="btn" data-na="ia" data-act="unevict" data-id="${n.id}" ${dis}>恢复 Leader</button>` : '') + (!n.evict ? `<button class="btn" data-na="ia" data-act="evict" data-id="${n.id}" ${dis}>驱逐 Leader</button>` : '') : ''}
      <button class="btn danger" data-na="nd-scalein" data-id="${n.id}" ${disScale}>缩容 / 下线</button></div>
      ${opDenied('inst.op') ? '<div class="small muted" style="margin-top:8px">权限不足：当前账号没有实例操作权限。</div>' : blk ? '<div class="small muted" style="margin-top:8px">该实例有进行中的任务或所在主机正在维护，操作已禁用。</div>' : ''}</div>
    <div class="card"><h2>常用命令</h2>${code(`tiup cluster display ${CLUSTER} -N ${n.host}:${n.port}\n${LIVE.on ? (n.logDir ? `tiup cluster exec ${CLUSTER} -N ${n.host} --command "tail -n 100 ${n.logDir}/${n.type === 'ticdc' ? 'cdc' : n.type}.log"${n.logDirInferred ? '\n# 日志目录是按 tiup 默认约定推断的，请以实际为准' : ''}` : '# 日志目录未采集，无法生成 tail 命令') : `tiup cluster exec ${CLUSTER} -N ${n.host} --command "tail -n 100 /tidb-deploy/${n.type}-${n.port}/log/${n.type}.log"`}`)}</div></div>`;
}

/* ================== 事件 ================== */
function updateCfgRow(el) {
  const n = nodeBy(ndState.id), i = catalog(n.type).find(x => x.k === el.dataset.k); if (!i) return;
  const cur = curStr(getVal(n, i)), raw = el.value.trim();
  if (raw === cur) delete ndState.draft[i.k]; else ndState.draft[i.k] = raw;
  const row = el.closest('.cfg'), msg = row.querySelector(`[data-msg="${i.k}"]`), changed = raw !== cur;
  row.classList.toggle('chg', changed);
  if (changed) { const v = validateCfg(n, i, raw); msg.innerHTML = v.err ? `<div class="err">✖ ${v.err}</div>` : v.warn ? `<div class="wn">⚠ ${v.warn}</div>` : ''; } else msg.innerHTML = '';
  const cnt = draftChanges(n).length; $('#cfgCnt').textContent = cnt; $('#cfgPrev').disabled = !cnt; $('#cfgReset').disabled = !cnt;
  const anyInst = Object.keys(ndState.draft).some(k => (catalog(n.type).find(x => x.k === k) || {}).scope === 'instance'); $('#cfgScopeBox').style.display = anyInst ? '' : 'none';
  const tb = $('#ndWrap .dtabs [data-t="config"]'); if (tb) tb.innerHTML = `配置变更${cnt ? ` <span class="tag info">${cnt}</span>` : ''}`;
}
document.addEventListener('input', e => { const el = e.target.closest('[data-ni="cfg-draft"]'); if (el) updateCfgRow(el); const c = e.target.closest('[data-ni="cfg-confirm"]'); if (c) { ndState.confirm = c.value.trim(); const b = $('#cfgSubmit'); if (b) b.disabled = !cfgDenied() && !(ndState.ack && ndState.confirm === '确认'); } });
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc]'); if (!el) return;
  switch (el.dataset.nc) {
    case 'cfg-draft': updateCfgRow(el); break;
    case 'cfg-scope': ndState.scope = el.value; break;
    case 'cfg-ack': { ndState.ack = el.checked; const n = nodeBy(ndState.id), high = draftChanges(n).some(c => c.i.danger); const b = $('#cfgSubmit'); if (b) b.disabled = !cfgDenied() && !(ndState.ack && (!high || ndState.confirm === '确认')); break; }
  }
});
document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled) return;
  const d = el.dataset, n = nodeBy(ndState.id);
  switch (d.na) {
    case 'nd-open': ndState.tab = 'overview'; openNodeDrawer(d.id, 'overview'); break;
    case 'nd-tab': ndState.tab = d.t; if (d.t !== 'config') ndState.stage = 'edit'; renderNd(); break;
    case 'nd-goto': closeDrawer(); setTab(d.t); break;
    case 'nd-scalein': closeDrawer(); nodesState.tab = 'scalein'; nodesState.si = {step: 2, picks: [d.id], filter: 'all', confirm: ''}; if (location.hash !== '#nodes') location.hash = 'nodes'; else pages.nodes.body(); break;
    case 'cfg-reload': { const pr = cfgLoad(true); renderNd(); pr.then(() => { if ($('#ndWrap')) renderNd(); }); break; }
    case 'cfg-preview': ndState.stage = 'preview'; ndState.ack = false; ndState.confirm = ''; renderNd(); break;
    case 'cfg-back': ndState.stage = 'edit'; renderNd(); break;
    case 'cfg-reset': ndState.draft = {}; renderNd(); break;
    case 'cfg-align': ndState.draft[d.k] = d.v; renderNd(); break;
    case 'cfg-rollback': {
      const h = CFG_HISTORY[+d.i]; if (!h || !n) break; const i = catalog(n.type).find(x => x.k === h.k); if (!i) break;
      ndState.draft = {[h.k]: h.from}; ndState.scope = h.nodeId || i.scope === 'cluster' ? 'instance' : 'role'; ndState.tab = 'config'; ndState.stage = 'preview'; ndState.ack = false; ndState.confirm = ''; renderNd(); break;
    }
    case 'cfg-submit': {
      if (!n) break; const ch = draftChanges(n); if (!ch.length) break;
      const plan = buildCfgChange(n, ch, ndState.scope);
      if (!OP.on) { ndState.draft = {}; ndState.stage = 'edit'; ndState.ack = false; ndState.confirm = ''; }   // 运维模式：提交成功后由 opGoTasks 重置草稿
      afterSubmit(plan); break;
    }
  }
});
