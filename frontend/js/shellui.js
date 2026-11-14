// @author: kevinkelly
// @license: Apache License 2.0
// @file: shellui.js
// @time: 2026/10/28

/* 中控机命令页（侧栏「运维 → 中控机命令」）：运维人员在页面里向中控机发命令、实时看输出。后端见 internal/shell 与 server/shell_api.go。
   命令先按风险分级（只读 / 变更 / 禁止）：只读直接执行；变更要填原因、由另一名管理员批准（批准只对这一批命令有效，且只能用一次）；禁止的一律拒绝。
   一律以 tidb 身份执行、不需要 root 密码：只读命令在平台进程内执行（经 sudoers 放行的固定入口），变更命令由该入口用 tidb 的 sudo 免密起临时
   systemd 单元；需要 root 权限的命令在整行开头写 sudo。输出在写盘前就已遮蔽口令 / 密钥 / 手机号 / 身份证号。
   只有运维模式 + 中控机模式 + 有权限时侧栏才出现入口。 */
const SH = {avail: null, reason: '', info: null, me: '', canRun: false, canApprove: false, canSet: false, settings: null, reqs: [], jobs: [], jobId: '', job: null, log: '', off: 0, masks: 0,
  filter: 'all', verdict: null, reqId: '', reqKey: '', timer: null, ct: 0, tick: 0, lastUser: '*', msg: ''};
const shHas = () => typeof OP !== 'undefined' && OP.on && !!OP.me;
const shTm = t => (typeof opFmtTime === 'function' ? opFmtTime(t) : String(t || ''));
const shErr = r => (r.data && r.data.error) || ('请求失败（' + r.code + '）');
const shLevelTag = l => ({ro: '<span class="tag ok">只读</span>', mut: '<span class="tag warn">变更</span>', deny: '<span class="tag bad">禁止</span>'}[l] || '');
const shRouteTag = r => ({inline: '<span class="tag info">平台进程内</span>', unit: '<span class="tag info">临时单元</span>'}[r] || '');
const shJobTag = j => { const m = {running: ['info', '运行中'], ok: ['ok', '成功'], failed: ['bad', '失败'], unknown: ['warn', '状态未知']}[j.status] || ['', j.status]; return `<span class="tag ${m[0]}">${m[1]}${j.cancelled ? '（已取消）' : ''}</span>`; };
const SH_LIB = {
  ro: ['tiup cluster list', 'tiup cluster display <集群名>', 'df -h', 'free -m', 'uptime', 'ss -ltnp | grep 4000', 'systemctl status tidbops', 'tail -n 200 /var/log/messages', 'journalctl -u chronyd -n 100 --no-pager', 'ping -c 4 <主机>'],
  mut: ['tiup cluster restart <集群名> -R tikv', 'sudo systemctl restart chronyd', 'sudo tail -n 200 /var/log/messages'],
  deny: ['cat ~/.ssh/id_rsa', 'env', 'scp /tidb-data/x user@host:/tmp', 'rm -rf /tidb-data', 'tail -f /var/log/messages', 'sudo -i', 'ls | sudo cat /var/log/messages'],
};

function shBody() {
  const q = id => document.getElementById(id);
  return {cwd: q('shCwd').value.trim(), timeout: +q('shTmo').value, stopOnError: q('shStop').checked,
    commands: q('shCmd').value.split('\n').map(x => x.trim()).filter(Boolean)};
}
const shKey = b => JSON.stringify([b.cwd, b.timeout, b.stopOnError, b.commands]);

pages.shell = {
  title: '中控机命令',
  render() {
    return `<div id="shRoot"><div class="small muted">加载中…</div></div>`;
  },
  init() { shEnter(); },
};

async function shStatus() {
  if (!shHas()) { SH.avail = false; return; }
  const r = await opApi('GET', '/api/shell/status');
  if (r.code === 200) { SH.avail = !!r.data.available; SH.reason = r.data.reason || ''; SH.info = r.data.info || null; SH.canRun = !!r.data.canRun; SH.canApprove = !!r.data.canApprove; SH.canSet = !!r.data.canSettings; SH.me = r.data.me || ''; if (SH.info) SH.settings = SH.info.settings; }
  else { SH.avail = false; SH.reason = r.code === 403 || r.code === 404 ? '' : shErr(r); }
}
const shNav = () => { const a = document.getElementById('navShell'); if (a) a.hidden = SH.avail !== true; };

async function shEnter() {
  await shStatus(); shNav();
  const root = document.getElementById('shRoot'); if (!root) return;
  if (SH.avail !== true) { root.innerHTML = `<div class="notice warn"><span>⚠</span><div>中控机命令不可用：${esc(SH.reason || '当前账号没有权限，或平台不是中控机模式（安装时加 --standalone --local-tiup）')}</div></div>`; return; }
  await Promise.all([shLoadReqs(), shLoadJobs()]);
  shPaint();
  if (!SH.jobId && SH.jobs.length) await shSelect(SH.jobs[0].id);
  shStart();
}

/* ---------------- 页面 ---------------- */
function shChannel() {
  const i = SH.info;
  if (!i.agent) return `<div class="notice bad"><span>✖</span><div>执行通道不可用：${esc(i.agentErr || '')}。请运维检查 /etc/sudoers.d/tidbops 是否放行了 shell-exec 入口（随本版本的 upgrade.sh 重新生成）。</div></div>`;
  if (i.sudo === false) return `<div class="notice warn"><span>⚠</span><div>${esc(i.sudoErr || 'tidb 在中控机上没有 sudo 免密（sudo -n 失败）')}。以 sudo 开头的命令和变更命令都会失败；请运维按部署手册为 tidb 开通 sudo 免密（tidb-deploy.sh 的前置条件之一）。</div></div>`;
  return '';
}
function shPaint() {
  const root = document.getElementById('shRoot'); if (!root) return;
  const i = SH.info;
  root.innerHTML = `
    <div class="card" style="margin-bottom:12px;display:flex;gap:16px;flex-wrap:wrap;align-items:center"><span>中控机 <b>${esc(i.host || '—')}</b> <span class="muted small">${esc(i.os || '')}</span></span>
      <span class="small muted">当前账号 ${esc(SH.me)}</span><span class="small muted">执行身份 <b>tidb</b>${i.sudo === true ? '（sudo 免密可用）' : ''}</span><span class="small muted">不是交互终端：一次一批命令，不能 vim / top / ssh；需要 root 权限的命令在整行开头写 sudo</span></div>
    ${shChannel()}
    ${SH.canRun ? shCmdCard() : `<div class="notice info"><span>ℹ</span><div>你的账号没有「中控机命令：执行」权限（shell.run），不能执行命令${SH.canApprove ? '，但可以批准他人提交的变更申请（见下方）' : ''}；${SH.canApprove ? '同时' : ''}可以查看执行历史与输出。需要执行权限请联系平台管理员在「角色权限」里授予。</div></div>`}
    <div class="card" style="margin-bottom:12px"><h2>输出 <span class="sub">口令、密钥、手机号、身份证号等会被自动遮蔽</span></h2><div id="shOutBox"></div></div>
    <div id="shReqs"></div>
    <div class="card" style="margin-bottom:12px"><h2>执行历史 <span class="sub">命令已打码；输出按保留策略保存；看别人的输出会记审计</span></h2><div id="shHist"></div></div>
    ${SH.canSet ? '<div id="shSet"></div>' : ''}
    <div class="card"><h2>平台的防护规则 <span class="sub">写进代码，页面上不能改</span></h2><div class="small" style="line-height:1.9">
      · 命令按风险分级：<b>只读</b>直接执行；<b>变更</b>要填原因、另一名管理员批准，批准只对这一批命令 / 目录有效，且只能用一次；<b>禁止</b>一律拒绝；认不出的命令按变更处理<br>
      · 一律以 <b>tidb</b> 身份执行，不需要 root 密码：只读命令在平台进程内执行（仍在服务沙箱里，文件系统只读）；变更命令在沙箱外的临时 systemd 单元里执行<br>
      · 需要 root 权限的命令在整行开头写 <code>sudo</code>（只认 <code>sudo 命令</code> / <code>sudo -n 命令</code>；<code>-u / -i / -s</code> 等、管道后面的 sudo 一律拒绝）；<b>带 sudo 的命令一律按变更处理</b>（要原因、另一名管理员批准，在临时单元里以 root 执行），哪怕命令本身是只读的<br>
      · 禁止读密钥与口令（~/.ssh、/etc/shadow、*.pem、平台数据目录、环境变量）、读业务数据（mysql、dumpling、tiup ctl…）、把数据送出去（scp、nc、curl 上传…）、命令替换 / here-doc（会绕过检查）<br>
      · 读文件内容的命令（cat / head / tail / grep…）只允许日志与系统信息目录，不允许数据目录（符号链接指向目录之外的也不行），且要写绝对路径；<code>find</code> 的起始目录同理<br>
      · 同一账号同时只能运行一批；超时自动终止；输出最多 1 MiB；命令与输出摘要写进审计<br>
      · <b>这是基于命令文本的护栏，防的是误操作和随手滥用，不是对抗恶意管理员的边界</b>；真正的边界是 sudoers 放行了谁、两人审批和审计</div></div>`;
  shOut(); shHistPaint(); shReqPaint(); if (SH.canSet) shSetPaint(); if (SH.canRun) shCheckSoon();
}
function shCmdCard() {
  const i = SH.info;
  const libBtn = (c, cls) => `<button class="btn sm ${cls}" data-sh="lib" data-c="${esc(c)}" style="font-family:var(--mono,monospace)">${esc(c)}</button>`;
  return `<div class="card" style="margin-bottom:12px"><h2>命令库 <span class="sub">点一下填入输入框（尖括号处要自己改）</span></h2>
      <div class="small muted" style="margin:4px 0">只读（直接执行，不需要密码）</div><div class="row">${SH_LIB.ro.map(c => libBtn(c, '')).join('')}</div>
      <div class="small muted" style="margin:8px 0 4px">变更（需要审批）</div><div class="row">${SH_LIB.mut.map(c => libBtn(c, '')).join('')}</div>
      <div class="small muted" style="margin:8px 0 4px">会被拒绝的例子</div><div class="row">${SH_LIB.deny.map(c => libBtn(c, 'danger')).join('')}</div></div>
    <div class="card" style="margin-bottom:12px"><h2>命令 <span class="sub">每行一条，最多 ${i.limits.maxLines} 条；支持管道和 &&，不支持交互；以 tidb 身份执行，需要 root 权限时整行开头写 sudo</span></h2>
      <div class="field"><textarea id="shCmd" rows="4" spellcheck="false" class="mono" placeholder="例如：tiup cluster display prod-tidb-01" data-shi="cmd" style="width:100%"></textarea></div>
      <div class="row" style="align-items:flex-end">
        <div class="field" style="width:120px"><label for="shTmo">每条的超时</label><select id="shTmo" data-shc="x"><option value="30">30 秒</option><option value="120">2 分钟</option><option value="600">10 分钟</option></select></div>
        <div class="field" style="flex:1;min-width:220px"><label for="shCwd">工作目录 <span class="muted">（自动取 tidb 的家目录，可修改）</span></label><input type="text" id="shCwd" value="${esc(i.home || '')}" data-shi="cwd"></div>
        <label class="small" style="display:flex;gap:6px;align-items:center;margin-bottom:12px"><input type="checkbox" id="shStop" checked data-shc="x"> 某条失败就停止</label></div>
      <div id="shVerdict"></div><div id="shAppr"></div>
      <div class="row" style="margin-top:8px"><button class="btn primary" data-sh="run">执行</button><span id="shErr" class="small" style="color:var(--bad)"></span></div></div>`;
}
function shVerdictPaint() {
  const el = document.getElementById('shVerdict'); if (!el) return;
  const v = SH.verdict, ap = document.getElementById('shAppr');
  if (!v) { el.innerHTML = '<div class="small muted">输入命令后，这里显示风险级别和会发生什么。</div>'; if (ap) ap.innerHTML = ''; return; }
  const act = {run: v.route === 'unit' ? '直接执行（临时 systemd 单元，不需要密码）；输出自动遮蔽；记审计' : '直接执行（平台进程内，不需要密码）；输出自动遮蔽；记审计', approve: '需要填写原因并由另一名管理员批准；批准后在临时 systemd 单元里执行，不需要密码', deny: '会被拒绝，不会执行'}[v.action];
  const sudoWarn = v.sudo && SH.info && SH.info.sudo === false ? ' <span style="color:var(--bad)">含 sudo 行，但 tidb 没有 sudo 免密，执行会失败。</span>' : '';
  el.innerHTML = `<div>${shLevelTag(v.level === 'ro' && v.action === 'deny' ? 'deny' : v.level)} ${v.action !== 'deny' ? shRouteTag(v.route) : ''} <span class="small muted">${esc(act)}${v.why ? '。' + esc(v.why) : ''}</span>${sudoWarn}</div>` +
    (v.items.length > 1 || v.action !== 'run' ? `<div style="margin-top:6px">${v.items.map(it => `<div class="small" style="padding:2px 0">${shLevelTag(it.level)} <code>${esc(it.line)}</code>${it.why ? ` <span class="muted">${esc(it.why)}</span>` : ''}</div>`).join('')}</div>` : '');
  if (!ap) return;
  const b = shBody(), key = shKey(b), mine = SH.reqs.find(r => r.id === SH.reqId);
  if (v.action !== 'approve') { ap.innerHTML = ''; return; }
  if (SH.reqId && SH.reqKey === key && mine) {
    ap.innerHTML = mine.state === 'approved' ? `<div class="notice info" style="margin-top:6px"><span>✔</span><div>${esc(mine.approvedBy)} 已批准这批命令（1 小时内执行有效，只能用一次），现在可以执行。</div></div>`
      : mine.state === 'pending' ? '<div class="notice warn" style="margin-top:6px"><span>⏳</span><div>已提交申请，等待另一名管理员批准（1 小时内有效）。改动任何一个字都要重新申请。</div></div>'
      : `<div class="notice bad" style="margin-top:6px"><span>✖</span><div>申请${mine.state === 'used' ? '已经用过了' : '已过期'}，请重新提交。</div></div>`;
    if (['used', 'expired'].includes(mine.state)) ap.innerHTML += shReqForm();
    return;
  }
  ap.innerHTML = shReqForm();
}
const shReqForm = () => `<div class="field" style="max-width:520px;margin-top:8px"><label for="shWhy">变更原因（必填，会写进审计）</label><input type="text" id="shWhy" placeholder="例如：时间同步偏差大，重启 chronyd"></div>
  <div class="row"><button class="btn" data-sh="req">提交申请</button><span class="small muted">申请只对这一批命令、这个目录和超时有效</span></div>`;

function shCheckSoon() { clearTimeout(SH.ct); SH.ct = setTimeout(shCheck, 300); }
async function shCheck() {
  const q = document.getElementById('shCmd'); if (!q) return;
  const b = shBody();
  if (!b.commands.length) { SH.verdict = null; shVerdictPaint(); return; }
  const r = await opApi('POST', '/api/shell/check', b);
  SH.verdict = r.code === 200 ? r.data.verdict : {level: 'deny', action: 'deny', why: shErr(r), items: []};
  shVerdictPaint();
}

function shOut() {
  const box = document.getElementById('shOutBox'); if (!box) return;
  const j = SH.job;
  if (!j) { box.innerHTML = '<div class="small muted">执行后在这里实时显示输出。</div>'; return; }
  const lines = SH.log.split('\n').filter(l => l !== '').map(l => [/^\$ /.test(l) ? 'c' : /^\[平台\]/.test(l) ? 'p' : /(command not found|No such file|Permission denied|a password is required|错误|失败)/i.test(l) ? 'e' : 'o', l])
    .filter(([c]) => SH.filter === 'all' || SH.filter === c || (SH.filter === 'e' && c === 'e'));
  const col = {c: 'color:var(--c1)', p: 'color:var(--muted)', e: 'color:var(--bad)', o: ''};
  box.innerHTML = `<div class="row" style="margin-bottom:8px">${shJobTag(j)} ${shRouteTag(j.route)} <span class="small muted">${esc(j.by)} · ${esc(j.cwd)}${j.exit != null ? ' · 退出码 ' + j.exit : ''} · ${esc(shTm(j.started))}${j.reason ? ' · 变更原因：' + esc(j.reason) + '（批准人 ' + esc(j.approver) + '）' : ''}</span>
      <span style="flex:1"></span>${[['all', '全部'], ['e', '只看错误']].map(([k, l]) => `<button class="btn sm ${SH.filter === k ? 'primary' : ''}" data-sh="filter" data-f="${k}">${l}</button>`).join('')}
      <a class="btn sm" href="/api/shell/jobs/${esc(j.id)}/download" download>下载</a>${j.status === 'running' && SH.canRun ? '<button class="btn sm danger" data-sh="cancel">取消</button>' : ''}</div>
    ${j.launchErr ? `<div class="notice bad"><span>✖</span><div>没能启动：${esc(j.launchErr)}</div></div>` : ''}
    <pre class="code" id="shLog" style="max-height:360px;overflow:auto;margin:0">${lines.length ? lines.map(([c, l]) => `<div style="${col[c]}">${esc(l)}</div>`).join('') : '<span class="muted">（还没有输出）</span>'}</pre>
    <span id="shErrC" class="small" style="color:var(--bad)"></span>
    <div class="small muted" style="margin-top:6px">${j.status !== 'running' ? '输出按保留策略保存；你查看的是已遮蔽的版本。' : '运行中，每 1.5 秒刷新。'}${SH.masks ? ' 已遮蔽 ' + SH.masks + ' 处敏感内容（口令 / 手机号 / 身份证号等），原文没有保存。' : ''}</div>`;
  const lg = document.getElementById('shLog'); if (lg) lg.scrollTop = lg.scrollHeight;
}
function shHistPaint() {
  const el = document.getElementById('shHist'); if (!el) return;
  el.innerHTML = SH.jobs.length ? `<div class="tbl-wrap"><table><thead><tr><th>时间</th><th>账号</th><th>路径</th><th>命令（已打码）</th><th>结果</th><th></th></tr></thead><tbody>${SH.jobs.map(j => `<tr><td class="small">${esc(shTm(j.started))}</td><td>${esc(j.by)}</td><td>${shRouteTag(j.route)}</td>
    <td class="mono small" style="word-break:break-all">${esc(j.lines.map(l => l.line).join(' ⏎ '))}</td><td>${shJobTag(j)}${j.exit != null ? ` <span class="small muted">${j.exit}</span>` : ''}${j.level === 'mut' ? ' <span class="tag warn">变更</span>' : ''}</td><td><button class="btn sm" data-sh="job" data-id="${esc(j.id)}">查看</button></td></tr>`).join('')}</tbody></table></div>` : '<div class="small muted">还没有执行记录。</div>';
}
function shReqPaint() {
  const el = document.getElementById('shReqs'); if (!el) return;
  const list = SH.reqs.filter(r => ['pending', 'approved'].includes(r.state));
  if (!list.length) { el.innerHTML = ''; return; }
  el.innerHTML = `<div class="card" style="margin-bottom:12px"><h2>待处理的变更申请</h2>${list.map(r => `<div style="padding:8px 0;border-top:1px solid var(--border)"><div class="row">${r.state === 'pending' ? '<span class="tag warn">待批准</span>' : '<span class="tag info">已批准</span>'}
    <span class="small">${esc(r.by)} · ${esc(r.cwd)} · ${esc(shTm(r.at))}</span></div>${r.lines.map(l => `<div class="small mono" style="padding:1px 0">${esc(l.line)}</div>`).join('')}<div class="small muted">原因：${esc(r.reason)}</div>
    <div class="row" style="margin-top:4px">${r.state === 'pending' ? (r.by === SH.me ? '<span class="small muted">等待另一名管理员批准</span>' : SH.canApprove ? `<button class="btn sm primary" data-sh="approve" data-id="${esc(r.id)}">批准</button>` : '')
      : (r.by === SH.me ? `<button class="btn sm" data-sh="use" data-id="${esc(r.id)}">用这个申请执行（确认上面的命令与申请一致）</button>` : `<span class="small muted">由 ${esc(r.approvedBy)} 批准，申请人执行</span>`)}</div></div>`).join('')}</div>`;
}
function shSetPaint() {
  const el = document.getElementById('shSet'), s = SH.settings; if (!el || !s) return;
  el.innerHTML = `<div class="card" style="margin-bottom:12px"><h2>策略设置 <span class="sub">仅平台管理员；修改会写审计（高风险）</span></h2>
    <div class="row" style="align-items:flex-end"><div class="field" style="width:260px"><label for="shPol">策略档位</label><select id="shPol"><option value="std" ${s.policy === 'std' ? 'selected' : ''}>标准：只读直接执行，变更需审批</option><option value="strict" ${s.policy === 'strict' ? 'selected' : ''}>严格：只允许只读命令</option><option value="loose" ${s.policy === 'loose' ? 'selected' : ''}>宽松：变更免审批、只记录（不推荐）</option></select></div>
      <div class="field" style="width:130px"><label for="shRet">输出保留（天）</label><input type="text" id="shRet" value="${s.retainDays}"></div>
      <label class="small" style="display:flex;gap:6px;align-items:center;margin-bottom:12px"><input type="checkbox" id="shSum" ${s.summaryOnly ? 'checked' : ''}> 只存摘要（结束 10 分钟后只留前 3 行）</label>
      <label class="small" style="display:flex;gap:6px;align-items:center;margin-bottom:12px"><input type="checkbox" id="shUnit" ${s.roViaUnit ? 'checked' : ''}> 只读命令也用临时单元执行<span class="muted">（默认在平台进程内执行；沙箱挡住了某些命令时打开）</span></label></div>
    <div class="field"><label for="shRoots">追加的「可读内容」目录（每行一个；内置：/var/log、/tidb-deploy 等；不允许系统目录与数据目录）</label><textarea id="shRoots" rows="2" class="mono" style="width:100%" placeholder="/data/tidb-deploy">${esc((s.readRoots || []).join('\n'))}</textarea></div>
    <div class="row"><button class="btn" data-sh="saveset">保存设置</button><span id="shSetMsg" class="small muted">${esc(SH.msg)}</span></div></div>`;
}

/* ---------------- 数据与轮询 ---------------- */
async function shLoadReqs() { const r = await opApi('GET', '/api/shell/requests'); if (r.code === 200) SH.reqs = r.data.requests || []; }
async function shLoadJobs() { const r = await opApi('GET', '/api/shell/jobs'); if (r.code === 200) SH.jobs = r.data.jobs || []; }
async function shSelect(id) { SH.jobId = id; SH.log = ''; SH.off = 0; SH.masks = 0; SH.job = null; await shPoll(true); }
async function shPoll(full) {
  if (!SH.jobId) return;
  const r = await opApi('GET', `/api/shell/jobs/${SH.jobId}?offset=${full ? 0 : SH.off}`);
  if (r.code !== 200) { if (r.code === 403) toast(shErr(r)); return; }
  if (full) { SH.log = ''; SH.masks = 0; }
  SH.log += r.data.log || ''; SH.off = r.data.offset || 0; SH.masks += r.data.masks || 0;
  const was = SH.job && SH.job.status; SH.job = r.data.job;
  if (was === 'running' && SH.job.status !== 'running') { await Promise.all([shLoadJobs(), shLoadReqs()]); shHistPaint(); shReqPaint(); shVerdictPaint(); }
  shOut();
}
function shStart() {
  clearInterval(SH.timer);
  SH.timer = setInterval(async () => {
    if (!document.getElementById('shRoot')) { clearInterval(SH.timer); return; }
    SH.tick++;
    if (SH.job && SH.job.status === 'running') await shPoll(false);
    if (SH.tick % 4 === 0) {   // 别的管理员的批准 / 新任务：每 6 秒看一次
      const before = JSON.stringify(SH.reqs.map(r => r.state + r.id)) + SH.jobs.length;
      await Promise.all([shLoadReqs(), shLoadJobs()]);
      if (JSON.stringify(SH.reqs.map(r => r.state + r.id)) + SH.jobs.length !== before) { shHistPaint(); shReqPaint(); shVerdictPaint(); }
    }
  }, 1500);
}

/* ---------------- 动作 ---------------- */
const shSetErr = (id, t) => { const e = document.getElementById(id); if (e) e.textContent = t || ''; };
const shFail = r => { const d = r.data || {}; if (d.code === 'denied' && d.verdict) { const bad = d.verdict.items.filter(x => x.level === 'deny'); return '被拒绝：' + (bad.length ? bad.map(x => x.line + '（' + x.why + '）').join('；') : d.verdict.why); } return shErr(r); };
async function shAct(a, el) {
  const id = el.dataset.id;
  if (a === 'lib') { const t = document.getElementById('shCmd'); if (t) { t.value = el.dataset.c; shCheckSoon(); } return; }
  if (a === 'filter') { SH.filter = el.dataset.f; shOut(); return; }
  if (a === 'job') { await shSelect(id); document.getElementById('shOutBox').scrollIntoView({behavior: 'smooth', block: 'center'}); return; }
  if (a === 'use') { SH.reqId = id; const r = SH.reqs.find(x => x.id === id); if (r) { SH.reqKey = shKey(shBody()); } shVerdictPaint(); toast('已选用该申请；服务端会核对命令是否与申请完全一致'); return; }
  if (a === 'approve') { const r = await opApi('POST', `/api/shell/requests/${id}/approve`); if (r.code !== 200) { toast(shErr(r)); return; } await shLoadReqs(); shReqPaint(); toast('已批准'); return; }
  if (a === 'req') {
    const b = shBody(), why = (document.getElementById('shWhy') || {}).value || '';
    const r = await opApi('POST', '/api/shell/requests', Object.assign(b, {reason: why}));
    if (r.code !== 200) { shSetErr('shErr', shFail(r)); return; }
    SH.reqId = r.data.request.id; SH.reqKey = shKey(b); await shLoadReqs(); shReqPaint(); shVerdictPaint(); toast('已提交申请，等待另一名管理员批准'); return;
  }
  if (a === 'run') {
    const b = shBody(); shSetErr('shErr', '');
    if (!b.commands.length) { shSetErr('shErr', '请输入命令'); return; }
    if (SH.reqId && SH.reqKey === shKey(b)) b.request = SH.reqId;
    shSetErr('shErr', '正在启动…');
    const r = await opApi('POST', '/api/shell/run', b);
    if (r.code !== 200) { shSetErr('shErr', shFail(r)); if (r.code === 502 && r.data && r.data.job) { await shLoadJobs(); shHistPaint(); } return; }
    shSetErr('shErr', ''); SH.reqId = ''; SH.reqKey = '';
    await Promise.all([shLoadJobs(), shLoadReqs()]); await shSelect(r.data.job.id); shHistPaint(); shReqPaint(); shVerdictPaint(); return;
  }
  if (a === 'cancel') {
    shSetErr('shErrC', '正在取消…');
    const r = await opApi('POST', `/api/shell/jobs/${SH.job.id}/cancel`);
    if (r.code !== 200) { shSetErr('shErrC', shFail(r)); return; }
    await shLoadJobs(); await shSelect(SH.job.id); shHistPaint(); return;
  }
  if (a === 'saveset') {
    const body = {policy: document.getElementById('shPol').value, retainDays: +document.getElementById('shRet').value, summaryOnly: document.getElementById('shSum').checked,
      roViaUnit: document.getElementById('shUnit').checked, readRoots: document.getElementById('shRoots').value.split('\n').map(x => x.trim()).filter(Boolean)};
    const r = await opApi('POST', '/api/shell/settings', body);
    SH.msg = r.code === 200 ? '已保存' : shErr(r); if (r.code === 200) { SH.settings = r.data.settings; shCheckSoon(); }
    shSetPaint(); return;
  }
}
document.addEventListener('click', e => {
  const el = e.target.closest('[data-sh]'); if (!el || !shHas()) return;
  e.preventDefault(); shAct(el.dataset.sh, el);
});
document.addEventListener('input', e => {
  const t = e.target; if (!t || !t.dataset) return;
  if (t.dataset.shi) shCheckSoon();
});
document.addEventListener('change', e => {
  const t = e.target; if (!t || !t.dataset || !t.dataset.shc) return;
  shCheckSoon();
});
/* 侧栏入口：登录后（或换账号后）查一次可用性，可用才显示 */
setInterval(async () => {
  if (!shHas()) { if (SH.lastUser !== '') { SH.lastUser = ''; SH.avail = false; shNav(); } return; }
  if (SH.lastUser === OP.me.user.name) return;
  SH.lastUser = OP.me.user.name; await shStatus(); shNav();
}, 2000);
