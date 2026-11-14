// @author: kevinkelly
// @license: Apache License 2.0
// @file: deployrun.js
// @time: 2026/10/28

/* 一键部署：安装部署页上方的「保存配置」条 + 右侧的「一键部署」标签。后端见 internal/deploy 与 server/deploy_api.go。
   流程：保存配置（服务端校验、渲染 deploy.conf、带版本）→ 预检（只读）→ 提交申请 → 另一名管理员批准 → 输入中控机 root 密码开始部署
   → 实时输出 / 结果（含新集群 TiDB root 随机密码，只显示一次）。root 密码只在点按钮时从输入框取出，发出后立即清空，不保存在页面状态里。
   只有运维模式 + 中控机模式（后端 /api/deploy/status 返回 available）时才显示；静态原型与只读模式下什么都不出现。 */
const dpRun = (() => {
  const Z = {avail: null, reason: '', canApprove: false, saved: null, versions: [], reqs: [], jobs: [], jobId: '', job: null, log: '', off: 0,
    filter: 'all', logTab: 'log', aux: {pre: '', conf: ''}, msg: '', msgBad: false, secret: '', timer: null, tick: 0, reqSig: ''};
  const hasOP = () => typeof OP !== 'undefined' && OP.on && !!OP.me;
  const me = () => (hasOP() && OP.me.user && OP.me.user.name) || '';
  const cluster = () => String(dpState.cluster || '').trim();
  const body = () => {
    const S = dpState;
    return {cluster: cluster(), mode: S.mode, version: dpVersion().spec, user: S.user, sshPort: String(S.sshPort), sshKey: S.sshKey, singleHost: S.singleHost, pd: S.pd, tidb: S.tidb, tikv: S.tikv,
      tiflash: S.tiflash, monitor: S.monitor, zone: S.zone, deployDir: S.deployDir, dataDir: S.dataDir, spec: S.spec, firewall: S.firewall, repoFix: S.repoFix, repoMirror: S.repoMirror,
      withMonitor: !!S.withMonitor, allowCheckFail: S.spec === 'dev' && !!S.allowCheckFail, install: S.install, offlineDir: S.offlineDir, rpmDir: S.rpmDir};
  };
  const key = () => JSON.stringify(body());
  const on = () => hasOP() && Z.avail === true;
  const tm = t => (typeof opFmtTime === 'function' ? opFmtTime(t) : String(t || ''));
  const errOf = r => (r.data && r.data.error) || ('请求失败（' + r.code + '）');

  /* ---------------- 页面上方的「保存配置」条 ---------------- */
  const bar = () => '<div id="dprBar"></div>';
  function after() {
    const g = $('#dpGrid'), f = $('#dpForm'), runTab = dpState.tab === 'run' && on();
    if (g && f) { f.style.display = runTab ? 'none' : ''; g.style.gridTemplateColumns = runTab ? 'minmax(0,1fr)' : 'minmax(0,1fr) minmax(0,1.1fr)'; }
    const b = $('#dprBar'); if (!b) return;
    if (!hasOP()) { b.innerHTML = ''; return; }
    if (Z.avail !== true) { b.innerHTML = Z.avail === false && Z.reason ? `<div class="small muted" style="margin-bottom:10px">一键部署不可用：${esc(Z.reason)}</div>` : ''; return; }
    if (Z.verCluster !== cluster()) {   // 集群名改了：稍等片刻（输入过程中不要每个字符都请求），再取该名字已保存的版本
      clearTimeout(Z.vt); Z.vt = setTimeout(async () => { await loadVersions(); after(); }, 400);
    }
    adopt();
    const dirty = !Z.saved || Z.saved.key !== key();
    const latest = Z.versions.length ? Z.versions[Z.versions.length - 1] : null;
    const st = !Z.saved ? `<span class="tag warn">尚未保存本次表单</span>${latest ? `<span class="small muted"> 该集群名最近已保存 v${latest.ver}（${esc(tm(latest.at))}，${esc(latest.by)}）</span>` : ''}`
      : dirty ? `<span class="tag warn">有未保存的修改</span><span class="small muted"> 最近保存 v${Z.saved.ver}</span>`
      : `<span class="tag ok">已保存 v${Z.saved.ver}，与表单一致</span><span class="small muted"> sha256 ${esc(Z.saved.sha.slice(0, 12))}…</span>`;
    b.innerHTML = `<div class="card" style="margin-bottom:12px;display:flex;gap:10px;align-items:center;flex-wrap:wrap"><span class="tag info">新</span><span>配置：<b>${esc(cluster() || '（未命名）')}</b></span>${st}<span style="flex:1"></span>
      <button class="btn sm" data-dpr="save">保存配置</button></div>${Z.msg ? `<div class="small" style="margin:-4px 0 10px;color:var(--${Z.msgBad ? 'bad' : 'muted'})">${esc(Z.msg)}</div>` : ''}`;
  }
  async function save() {
    const r = await opApi('POST', '/api/deploy/configs', body());
    if (r.code === 200) {
      const v = r.data.version;
      Z.saved = {ver: v.ver, sha: v.sha256, key: key(), conf: r.data.conf};
      Z.msg = r.data.created ? `已保存为 v${v.ver}：写入平台数据目录，权限 600，记入审计；配置里不含任何密码` : `没有变化，仍是 v${v.ver}`; Z.msgBad = false;
      await loadVersions();
    } else if (r.code === 400 && r.data.errors) { Z.msg = '配置有 ' + r.data.errors.length + ' 个问题：' + r.data.errors.slice(0, 3).join('；'); Z.msgBad = true; }
    else { Z.msg = errOf(r); Z.msgBad = true; }
    after(); if (dpState.tab === 'run') paint();
  }

  /* ---------------- 数据 ---------------- */
  async function status() {
    if (!hasOP()) { Z.avail = false; return; }
    const r = await opApi('GET', '/api/deploy/status');
    if (r.code === 200) { Z.avail = !!r.data.available; Z.reason = r.data.reason || ''; Z.canApprove = !!r.data.canApprove; }
    else { Z.avail = false; Z.reason = r.code === 404 || r.code === 403 ? '' : errOf(r); }
  }
  async function loadVersions() {
    const c = cluster(); Z.versions = []; Z.verCluster = c;
    if (!c) return;
    const r = await opApi('GET', '/api/deploy/configs?cluster=' + encodeURIComponent(c));
    if (r.code === 200) Z.versions = r.data.versions || [];
    // 刷新页面后：表单内容与最近保存的版本完全一致时，直接认作「已保存」，不必再点一次保存
    adopt();
  }
  function adopt() {
    const last = Z.versions[Z.versions.length - 1];
    if ((!Z.saved || Z.saved.key !== key()) && last && sameSpec(last.spec, body())) Z.saved = {ver: last.ver, sha: last.sha256, key: key(), conf: last.conf};
  }
  function sameSpec(a, b) {
    const n = (k, v) => k === 'zone' ? String(v == null ? '' : v).replace(/\s+/g, '') : (k === 'offlineDir' || k === 'rpmDir') ? String(v == null ? '' : v).trim().replace(/\/+$/, '') : String(v == null ? '' : v).trim();
    return Object.keys(b).every(k => (k === 'withMonitor' || k === 'allowCheckFail') ? !!a[k] === !!b[k] : n(k, a[k]) === n(k, b[k]));
  }
  async function loadReqs() {
    const r = await opApi('GET', '/api/deploy/requests');
    if (r.code === 200) { Z.reqs = (r.data.requests || []).filter(x => x.cluster === cluster()); }
  }
  async function loadJobs() {
    const r = await opApi('GET', '/api/deploy/jobs?cluster=' + encodeURIComponent(cluster()));
    if (r.code === 200) Z.jobs = r.data.jobs || [];
  }
  async function selectJob(id) {
    Z.jobId = id; Z.log = ''; Z.off = 0; Z.job = null; Z.secret = ''; Z.logTab = 'log'; Z.aux = {pre: '', conf: ''};
    await pollJob(true);
  }
  async function pollJob(full) {
    if (!Z.jobId) return;
    const r = await opApi('GET', `/api/deploy/jobs/${Z.jobId}?offset=${full ? 0 : Z.off}`);
    if (r.code !== 200) return;
    if (full) Z.log = '';
    Z.log += r.data.log || ''; Z.off = r.data.offset || 0;
    const was = Z.job && Z.job.status;
    Z.job = r.data.job;
    if (full || was !== Z.job.status) { if (was === 'running' && Z.job.status !== 'running') { await Promise.all([loadReqs(), loadJobs()]); } paint(); } else paintJob();
  }

  /* ---------------- 日志解析 ---------------- */
  const lvl = l => /^\s*✖|\[错误\]|^Error:|\[ERR/.test(l) ? 'e' : /^\s*⚠|\[警告\]|\[WARN/.test(l) ? 'w' : /^\s*✔|\[ ok \]/.test(l) ? 'o' : /^==> /.test(l) ? 's' : 'p';
  const lvlColor = {e: 'var(--bad)', w: 'var(--warn)', o: 'var(--ok)', s: 'var(--c1)', p: ''};
  const logHtml = (text, filter) => text.split('\n').filter(l => l !== '').map(l => [lvl(l), l]).filter(([c]) => filter === 'all' || c === filter)
    .map(([c, l]) => `<div style="${lvlColor[c] ? 'color:' + lvlColor[c] : ''}">${esc(l)}</div>`).join('');
  const phasesOf = text => text.split('\n').filter(l => l.startsWith('==> ')).map(l => l.slice(4).trim());
  const counts = text => { const c = {o: 0, w: 0, e: 0}; text.split('\n').forEach(l => { const m = l.match(/^\s*(✔|⚠|✖)/); if (m) c[{'✔': 'o', '⚠': 'w', '✖': 'e'}[m[1]]]++; }); return c; };
  function access(text) {
    const i = text.lastIndexOf('部署完成'); if (i < 0) return [];
    return text.slice(i).split('\n').map(l => l.match(/^(集群名|TiDB 版本|SQL 连接|root 密码|Dashboard|Grafana|管理命令|拓扑 \/ 日志)\s*:\s*(.*)$/)).filter(Boolean).map(m => [m[1], m[2]]);
  }
  const JOB_TAG = {running: ['warn', '运行中'], ok: ['ok', '成功'], failed: ['bad', '失败'], unknown: ['warn', '状态未知']};
  const jobTag = j => { const [c, t] = JOB_TAG[j.status] || ['', j.status]; return `<span class="tag ${c}">${t}${j.cancelled ? '（已取消）' : ''}</span>`; };
  const REQ_TAG = {pending: ['warn', '待批准'], approved: ['info', '已批准'], expired: ['', '已过期'], done: ['ok', '已完成']};

  /* ---------------- 绘制 ---------------- */
  const render = () => { setTimeout(enter, 0); return '<div id="dprRoot"><div class="small muted">加载中…</div></div>'; };
  async function enter() {
    await status();
    if (Z.avail !== true) { const r = $('#dprRoot'); if (r) r.innerHTML = `<div class="notice warn"><span>⚠</span><div>一键部署不可用：${esc(Z.reason || '当前账号没有 settings.edit 权限，或不是中控机模式')}</div></div>`; after(); return; }
    await Promise.all([loadVersions(), loadReqs(), loadJobs()]);
    if (!Z.jobId && Z.jobs.length) await selectJob(Z.jobs[0].id); else paint();
    after(); startPoll();
  }
  const pwField = (id, label) => `<div class="field" style="max-width:320px"><label for="${id}">${label}</label><input type="password" id="${id}" autocomplete="new-password" placeholder="中控机 root 密码（CentOS）"></div>`;
  const PW_NOTE = '<div class="small muted" style="margin-bottom:8px">tidb-deploy.sh 要求 root。平台用它通过 su 启动脚本：只在内存中用一次，不写入配置、日志、审计和下载的输出，启动后立即清除；连续输错 3 次锁定 10 分钟。</div>';

  function sec1() {
    const dirty = !Z.saved || Z.saved.key !== key();
    const lastPre = Z.saved && Z.jobs.find(j => j.kind === 'precheck' && j.confVer === Z.saved.ver && j.confSha === Z.saved.sha);
    const hasReq = lastPre && Z.reqs.some(r => r.precheck === lastPre.id);
    return `<div class="card" style="margin-bottom:12px"><h2>1 · 预检 <span class="sub">只读，不改任何机器</span></h2>
      ${!Z.saved ? '<div class="notice warn"><span>⚠</span><div>还没有保存本次的配置：先点页面上方的「保存配置」。部署只使用已保存的版本。</div></div>'
        : dirty ? `<div class="notice warn"><span>⚠</span><div>表单有未保存的修改：预检和部署只使用已保存的 v${Z.saved.ver}，请先保存。</div></div>`
        : `<div class="small">将使用已保存的配置 <b>v${Z.saved.ver}</b>（sha256 ${esc(Z.saved.sha.slice(0, 12))}…），执行期间不可修改。部署用户 <b>${esc(dpState.user)}</b> 须已在所有机器上配好 sudo 免密与 SSH 免密，预检会逐台检查。</div>`}
      ${PW_NOTE}${pwField('dprPw1', '中控机 root 密码')}
      <div class="row"><button class="btn primary" data-dpr="pre">开始预检</button><span id="dprE1" class="small" style="color:var(--bad)"></span></div>
      ${lastPre ? `<div class="small" style="margin-top:10px">最近一次预检：${jobTag(lastPre)} <span class="muted">${esc(lastPre.id)} · ${esc(tm(lastPre.started))}</span>
        <button class="btn sm" data-dpr="job" data-id="${esc(lastPre.id)}">查看输出</button>
        ${lastPre.status === 'ok' && !hasReq ? `<button class="btn sm primary" data-dpr="req" data-id="${esc(lastPre.id)}">提交部署申请</button>` : ''}
        ${lastPre.status === 'ok' && hasReq ? '<span class="muted">已提交申请，见下方</span>' : ''}</div>` : ''}</div>`;
  }
  function sec2() {
    const list = Z.reqs.slice(0, 5);
    return `<div class="card" style="margin-bottom:12px"><h2>2 · 部署申请 <span class="sub">高风险：需要另一名管理员批准</span></h2>
      ${list.length ? list.map(r => {
        const [c, t] = REQ_TAG[r.state] || ['', r.state];
        let act = '';
        if (r.state === 'pending') act = r.by === me() ? '<span class="small muted">等待另一名管理员批准（1 小时内有效）</span>'
          : Z.canApprove ? `<button class="btn sm primary" data-dpr="approve" data-id="${esc(r.id)}">批准</button>` : '<span class="small muted">需要有 settings.edit 权限的管理员批准</span>';
        if (r.state === 'approved') {
          const lastJ = r.lastJob && Z.jobs.find(j => j.id === r.lastJob);
          act = `<div class="notice info" style="margin-top:6px"><span>ℹ</span><div>${esc(r.approvedBy)} 已批准。${lastJ && lastJ.status !== 'ok' ? '上次部署' + (lastJ.cancelled ? '已取消' : '失败') + '，已完成的阶段不会重做，可从断点继续。' : ''}</div></div>
            <div class="field" style="max-width:320px"><label for="dprCf-${esc(r.id)}">输入集群名 ${esc(r.cluster)} 以确认</label><input type="text" id="dprCf-${esc(r.id)}" autocomplete="off"></div>
            ${pwField('dprPw-' + r.id, '中控机 root 密码')}
            <div class="row"><button class="btn primary" data-dpr="start" data-id="${esc(r.id)}">${lastJ && lastJ.status !== 'ok' ? '从断点继续部署' : '开始部署'}</button><span id="dprE-${esc(r.id)}" class="small" style="color:var(--bad)"></span></div>`;
        }
        return `<div style="padding:8px 0;border-top:1px solid var(--border)"><div class="row"><span class="tag ${c}">${t}</span><span class="small">申请 <span class="mono">${esc(r.id)}</span> · 配置 v${r.confVer} · ${esc(r.by)} · ${esc(tm(r.at))}</span></div>${act}</div>`;
      }).join('') : '<div class="small muted">还没有部署申请。预检通过后，在上面点「提交部署申请」。</div>'}</div>`;
  }
  function sec3() {
    const j = Z.job;
    if (!j) return `<div class="card"><h2>3 · 输出</h2><div class="small muted">预检或部署开始后，这里实时显示脚本的全部输出。${Z.jobs.length ? '' : ''}</div>${jobList()}</div>`;
    const ph = phasesOf(Z.log), c = counts(Z.log), done = j.status !== 'running';
    const fails = j.kind === 'precheck' ? Z.log.split('\n').filter(l => /^\s*✖/.test(l)) : [];
    const acc = j.kind === 'deploy' && j.status === 'ok' ? access(Z.log) : [];
    const req = Z.reqs.find(r => r.id === j.request);
    const tabs = [['log', j.kind === 'deploy' ? '部署日志' : '预检输出']];
    if (j.kind === 'deploy' && req) tabs.push(['pre', '预检报告']);
    tabs.push(['conf', 'deploy.conf v' + j.confVer]);
    return `<div class="card"><h2>3 · 输出 <span class="sub">${j.kind === 'deploy' ? '部署' : '预检'} · ${esc(j.cluster)} · 配置 v${j.confVer}</span></h2>
      <div class="row" id="dprHead">${headHtml(j)}</div>
      ${fails.length ? `<div class="notice bad" style="flex-direction:column"><b>预检没有通过：</b>${fails.map(l => `<div>${esc(l.trim())}</div>`).join('')}<div class="small">按上面的提示在对应机器上处理（命令见「命令」标签第 0 步），处理好后重新预检。</div></div>` : ''}
      ${j.status === 'failed' && j.kind === 'deploy' && !j.cancelled ? '<div class="notice bad"><span>✖</span><div>部署失败：看下面输出里最后的错误；已完成的阶段不会重做，修好后在「部署申请」里点「从断点继续部署」（要重新输入 root 密码）。</div></div>' : ''}
      ${j.launchErr ? `<div class="notice bad"><span>✖</span><div>没能启动：${esc(j.launchErr)}</div></div>` : ''}
      <div class="row" style="margin:8px 0"><div class="tabs" style="margin:0;border:0">${tabs.map(([k, l]) => `<button data-dpr="tab" data-t="${k}" class="${Z.logTab === k ? 'active' : ''}">${l}</button>`).join('')}</div>
        <span style="flex:1"></span>${[['all', '全部'], ['w', '警告'], ['e', '错误']].map(([k, l]) => `<button class="btn sm ${Z.filter === k ? 'primary' : ''}" data-dpr="filter" data-f="${k}">${l}</button>`).join('')}</div>
      <div id="dprPhases">${ph.length ? '<div class="small muted" style="margin-bottom:6px">' + ph.map((p, i) => (i === ph.length - 1 && !done ? '⏳ ' : (j.status === 'failed' && i === ph.length - 1 ? '✖ ' : '✔ ')) + esc(p)).join(' → ') + '</div>' : ''}</div>
      <pre class="code mono" id="dprLog" style="max-height:340px;overflow:auto;margin:0">${bodyHtml()}</pre>
      <div id="dprCnt" class="small muted" style="margin-top:6px">${j.kind === 'precheck' ? `检查项：通过 ${c.o} · 警告 ${c.w} · 失败 ${c.e}` : ''}</div>
      ${acc.length ? `<div style="margin-top:12px"><b>访问信息</b><div style="display:grid;grid-template-columns:110px 1fr;gap:4px 12px;margin-top:6px">${acc.filter(([k]) => k !== 'root 密码').map(([k, v]) => `<span class="muted">${esc(k)}</span><span class="mono small">${esc(v)}</span>`).join('')}
        <span class="muted">TiDB root 密码</span><span>${Z.secret ? `<code class="mono">${esc(Z.secret)}</code> <button class="btn sm" data-copy="${esc(Z.secret)}">复制</button> <span class="small muted">已显示，平台不再保存；请立即改密码</span>`
          : j.secret ? '<button class="btn sm" data-dpr="secret">显示（只能显示一次）</button>' : '<span class="small muted">没有可显示的密码（已显示过或未生成）；原文件在中控机，权限 600</span>'}</span></div>
        <div class="small muted" style="margin-top:6px">这是新集群 TiDB 的 root 随机密码，与 CentOS 的 root 密码无关（后者平台没有保存）。首次登录后请改密码并删除中控机上的密码文件。</div>
        <div class="row" style="margin-top:8px"><button class="btn sm" data-dpr="imp">导入到「集群接入」，开始运维</button></div></div>` : ''}
      <div class="row" style="margin-top:10px"><a class="btn sm" href="/api/deploy/jobs/${esc(j.id)}/download" download>下载完整输出</a>
        ${j.status === 'running' ? '<span style="flex:1"></span>' : ''}</div>
      ${j.status === 'running' ? `<div style="margin-top:10px">${pwField('dprPwC', '取消需要再次输入 root 密码')}<div class="row"><button class="btn danger sm" data-dpr="cancel">取消部署</button><span id="dprEC" class="small" style="color:var(--bad)"></span></div>
        <div class="small muted">取消会停止脚本（systemctl stop）；已完成的阶段保留，可从断点继续。</div></div>` : ''}
      ${jobList()}</div>`;
  }
  const headHtml = j => `${jobTag(j)} <span class="small muted mono">${esc(j.id)}</span> <span class="small muted">开始 ${esc(tm(j.started))}${j.ended ? ' · 结束 ' + esc(tm(j.ended)) : ''}${j.exit != null ? ' · 退出码 ' + j.exit : ''} · ${esc(j.by)}</span>`;
  function bodyHtml() {
    if (Z.logTab === 'pre') return Z.aux.pre ? logHtml(Z.aux.pre, Z.filter) : '加载中…';
    if (Z.logTab === 'conf') { const v = Z.versions.find(x => x.ver === Z.job.confVer); return v ? esc(v.conf).split('\n').map(l => `<div>${l}</div>`).join('') : '（找不到该版本）'; }
    return Z.log ? logHtml(Z.log, Z.filter) : '<div class="muted">（还没有输出）</div>';
  }
  function jobList() {
    const others = Z.jobs.filter(x => !Z.job || x.id !== Z.job.id).slice(0, 6);
    return others.length ? `<div class="small" style="margin-top:12px"><b>其他任务</b>${others.map(x => `<div style="padding:3px 0">${jobTag(x)} ${x.kind === 'deploy' ? '部署' : '预检'} <span class="muted mono">${esc(x.id)}</span> <span class="muted">v${x.confVer} · ${esc(tm(x.started))}</span> <button class="btn sm" data-dpr="job" data-id="${esc(x.id)}">查看</button></div>`).join('')}</div>` : '';
  }
  function paint() {
    const r = $('#dprRoot'); if (!r) return;
    r.innerHTML = `<div id="dprS1">${sec1()}</div><div id="dprS2">${sec2()}</div><div id="dprS3">${sec3()}</div>`;
    Z.reqSig = JSON.stringify(Z.reqs);
  }
  function paintJob() {   // 轮询时只更新输出区，不动输入框
    const j = Z.job, box = $('#dprLog'); if (!j || !box) return;
    const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
    box.innerHTML = bodyHtml(); if (atBottom) box.scrollTop = box.scrollHeight;
    const h = $('#dprHead'); if (h) h.innerHTML = headHtml(j);
    const ph = phasesOf(Z.log), p = $('#dprPhases'); if (p) p.innerHTML = ph.length ? '<div class="small muted" style="margin-bottom:6px">' + ph.map((x, i) => (i === ph.length - 1 && j.status === 'running' ? '⏳ ' : '✔ ') + esc(x)).join(' → ') + '</div>' : '';
    const cnt = $('#dprCnt'); if (cnt && j.kind === 'precheck') { const c = counts(Z.log); cnt.textContent = `检查项：通过 ${c.o} · 警告 ${c.w} · 失败 ${c.e}`; }
  }
  function startPoll() {
    clearInterval(Z.timer);
    Z.timer = setInterval(async () => {
      if (!$('#dprRoot')) { clearInterval(Z.timer); return; }
      Z.tick++;
      if (Z.job && Z.job.status === 'running') await pollJob(false);
      if (Z.tick % 3 === 0) {   // 别的管理员的批准 / 新任务：每 6 秒看一次；输入框里有字时不重绘，免得丢掉输入
        await Promise.all([loadReqs(), loadJobs()]);
        const act = document.activeElement, s2 = $('#dprS2');
        if (JSON.stringify(Z.reqs) !== Z.reqSig && !(act && s2 && s2.contains(act) && act.tagName === 'INPUT')) { const e1 = $('#dprS1'); if (e1 && !(act && e1.contains(act))) e1.innerHTML = sec1(); if (s2) s2.innerHTML = sec2(); Z.reqSig = JSON.stringify(Z.reqs); }
      }
    }, 2000);
  }

  /* ---------------- 动作 ---------------- */
  const takePw = id => { const el = document.getElementById(id); const v = el ? el.value : ''; if (el) el.value = ''; return v; };
  const setErr = (id, t) => { const e = document.getElementById(id); if (e) e.textContent = t || ''; };
  const failText = r => r.data && r.data.code === 'bad_password' ? r.data.error : errOf(r);
  async function act(a, el) {
    const id = el.dataset.id;
    if (a === 'save') return save();
    if (a === 'tab') { Z.logTab = el.dataset.t; if (Z.logTab === 'pre' && !Z.aux.pre) { const req = Z.reqs.find(r => r.id === Z.job.request); if (req) { const r = await opApi('GET', `/api/deploy/jobs/${req.precheck}?offset=0`); if (r.code === 200) Z.aux.pre = r.data.log || ''; } } return paintAll3(); }
    if (a === 'filter') { Z.filter = el.dataset.f; return paintAll3(); }
    if (a === 'job') { await selectJob(id); return; }
    if (a === 'imp') { if (typeof go === 'function') go('clusters'); return; }
    if (a === 'pre') {
      if (!Z.saved || Z.saved.key !== key()) { setErr('dprE1', '请先保存配置（表单有未保存的修改）'); return; }
      const pw = takePw('dprPw1'); if (!pw) { setErr('dprE1', '请输入中控机 root 密码'); return; }
      setErr('dprE1', '正在通过 su 启动预检…');
      const r = await opApi('POST', '/api/deploy/precheck', {cluster: cluster(), ver: Z.saved.ver, rootPassword: pw});
      if (r.code !== 200) { setErr('dprE1', failText(r)); return; }
      await Promise.all([loadReqs(), loadJobs()]); await selectJob(r.data.job.id); startPoll(); return;
    }
    if (a === 'req') { const r = await opApi('POST', '/api/deploy/requests', {precheck: id}); if (r.code !== 200) { toast(errOf(r)); return; } await loadReqs(); paint(); toast('已提交部署申请，等待另一名管理员批准'); return; }
    if (a === 'approve') { const r = await opApi('POST', `/api/deploy/requests/${id}/approve`); if (r.code !== 200) { toast(errOf(r)); return; } await loadReqs(); paint(); toast('已批准'); return; }
    if (a === 'start') {
      const cf = (document.getElementById('dprCf-' + id) || {}).value || '', pw = takePw('dprPw-' + id), e = 'dprE-' + id;
      if (!pw) { setErr(e, '请输入中控机 root 密码'); return; }
      setErr(e, '正在通过 su 启动部署…');
      const r = await opApi('POST', `/api/deploy/requests/${id}/start`, {confirm: cf, rootPassword: pw});
      if (r.code !== 200) { setErr(e, failText(r)); return; }
      await Promise.all([loadReqs(), loadJobs()]); await selectJob(r.data.job.id); startPoll(); return;
    }
    if (a === 'cancel') {
      const pw = takePw('dprPwC'); if (!pw) { setErr('dprEC', '请输入 root 密码'); return; }
      const r = await opApi('POST', `/api/deploy/jobs/${Z.job.id}/cancel`, {rootPassword: pw});
      if (r.code !== 200) { setErr('dprEC', failText(r)); return; }
      await Promise.all([loadReqs(), loadJobs()]); await selectJob(Z.job.id); return;
    }
    if (a === 'secret') {
      const r = await opApi('POST', `/api/deploy/jobs/${Z.job.id}/secret`);
      if (r.code !== 200) { toast(errOf(r)); return; }
      Z.secret = r.data.password; Z.job.secret = false; paintAll3();
    }
  }
  function paintAll3() { const s = $('#dprS3'); if (s) s.innerHTML = sec3(); }
  document.addEventListener('click', e => {
    const el = e.target.closest('[data-dpr]'); if (!el || !hasOP()) return;
    e.preventDefault(); act(el.dataset.dpr, el);
  });
  return {on, bar, render, after, init: () => { status().then(() => { if (Z.avail) return loadVersions(); }).then(() => { if (typeof dpRenderOut === 'function') dpRenderOut(); else after(); }); }, reset: () => { Z.saved = null; Z.versions = []; }};
})();
