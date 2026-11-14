// @author: kevinkelly
// @license: Apache License 2.0
// @file: security.js
// @time: 2026/10/28

/* 权限与审计（对应路线图 A3）：权限矩阵 / 权限模拟器 / 审批策略 / 审计防篡改 / 命令白名单 / 部署自检。
   所有规则与 docs/用户手册.md 第 5 章、docs/运维部署手册.md 第 10 章一致；这里是可交互的“规格演示”，真实实现见 D1 / D2。 */
const SEC_ROLES = [['viewer', '访客'], ['developer', '开发'], ['sre', '值班 SRE'], ['dba', 'DBA'], ['approver', '审批人'], ['admin', '管理员'], ['auditor', '审计员']];
const SEC_ENVS = ['测试', '预发', '生产'];
/* [id, 名称, 风险标签, 默认风险, 是否写操作(受环境写开关约束), 矩阵(访客,开发,SRE,DBA,审批人,管理员,审计员), 将走的命令] */
const SEC_CAPS = [
  ['overview', '查看总览 / 拓扑', '读', 'low', false, 'YYYYYYY', 'tiup cluster display <cluster>（只读）'],
  ['sqltext', '查看慢 SQL / 执行计划（含 SQL 文本）', '读·敏感', 'low', false, '-SYYYY-', 'SELECT … FROM information_schema.cluster_slow_query（只读账号）'],
  ['diag', '运行诊断 / 巡检', '读', 'low', false, '--YYYY-', 'tiup cluster check <cluster> --cluster；只读 SQL'],
  ['report', '导出诊断 / 巡检报告', '读', 'low', false, '--YYYY-', '无（平台内部生成）'],
  ['inst', '单实例操作（重启 / 停止 / 驱逐 Leader）', '中 / 高', 'mid', true, '--RR---', 'tiup cluster restart|stop|start <cluster> -N <addr>；pd-ctl scheduler add evict-leader-scheduler <id>'],
  ['scale', '扩缩容 / 新增主机', '中 / 高', 'high', true, '---R---', 'tiup cluster scale-out|scale-in <cluster> …；tiup cluster prune <cluster>'],
  ['config', '配置变更', '中 / 高', 'mid', true, '---R---', 'tiup cluster edit-config … --topology-file；reload；SET CONFIG；pd-ctl config set'],
  ['maint', '主机维护', '高', 'high', true, '---R---', 'pd-ctl scheduler …；tiup cluster stop|start <cluster> -N <addrs>'],
  ['backup', '备份恢复操作', '高', 'high', true, '---R---', 'tiup br backup|restore …'],
  ['schedcreate', '创建变更排期', '中', 'mid', false, '--YY---', '无（只创建排期，执行时仍要走对应能力的鉴权）'],
  ['approvemid', '审批中风险变更', '—', 'low', false, '----Y--', '无'],
  ['approvehigh', '审批高风险变更（2 级）', '—', 'low', false, '----Y--', '无'],
  ['release', '平台发布（路线图里程碑）', '中 / 高', 'mid', false, '-----R-', '无（平台自身发布流程）'],
  ['platcfg', '修改平台设置', '中', 'mid', false, '-----Y-', '无'],
  ['prodwrite', '开启 / 关闭生产写操作开关', '高', 'high', false, '-----R-', '无（需另一名管理员复核）'],
  ['iam', '用户与角色管理', '高', 'high', false, '-----Y-', '无'],
  ['auditview', '查看审计日志', '读', 'low', false, '----YYY', '无'],
  ['auditexport', '导出审计日志', '读·敏感', 'low', false, '------Y', '无'],
  ['shellrun', '中控机命令：执行', '高', 'high', false, '-----Y-', '平台经 sudoers 固定入口以 tidb 身份执行；只读命令在平台进程内，变更命令在临时 systemd 单元里（需另一人批准）'],
  ['shellappr', '中控机命令：批准变更', '高', 'high', false, '-----Y-', '无（批准他人提交的变更命令，申请人不能批准自己的）'],
];
const SEC_APPROVAL = {low: [0, 0, 0], mid: [0, 1, 1], high: [1, 2, 2]};      // [测试, 预发, 生产]
const secCap = id => SEC_CAPS.find(c => c[0] === id);
const secLevel = (role, capId) => { const i = SEC_ROLES.findIndex(r => r[0] === role); return secCap(capId)[5][i]; };
const RISK_LBL = {low: '低', mid: '中', high: '高'};

/* 授权判定（与文档 2.3 一致） */
function secAuthorize({role, cap, env, risk, flags, versionOk, mfa, lvl: lvlIn, roleName}) {   // lvlIn / roleName：自定义角色（ops_roles.js）直接给出授予等级 Y/R/S/- 与显示名
  const c = secCap(cap), ei = SEC_ENVS.indexOf(env), steps = []; let decision = 'allow', needs = 0;
  const lvl = lvlIn !== undefined ? lvlIn : secLevel(role, cap), roleLabel = roleName || SEC_ROLES.find(r => r[0] === role)[1];
  steps.push(lvl === '-' ? ['bad', '角色', `${roleLabel}在「${env}」没有「${c[1]}」权限`] : ['ok', '角色', `${roleLabel}拥有该能力（${{Y: '直接允许', R: '需审批', S: '仅限被授权范围'}[lvl]}）`]);
  if (lvl === '-') return {decision: 'deny', steps, needs: 0, reason: '角色无此权限'};
  if (c[4]) {
    const on = flags[env]; steps.push(on ? ['ok', '环境写开关', `${env}环境写操作已开启`] : ['bad', '环境写开关', `${env}环境写操作处于关闭状态（生产默认关闭）`]);
    if (!on) return {decision: 'deny', steps, needs: 0, reason: '环境写操作未开启'};
    steps.push(versionOk ? ['ok', '版本兼容', '集群版本在支持矩阵内'] : ['bad', '版本兼容', '集群版本不在支持矩阵内，写操作自动降级为只读']);
    if (!versionOk) return {decision: 'deny', steps, needs: 0, reason: '版本未验证'};
  }
  if (lvl === 'R') {
    needs = cap === 'prodwrite' ? 2 : SEC_APPROVAL[risk][ei];
    steps.push(needs ? ['warn', '审批', cap === 'prodwrite' ? '需要两名不同的管理员依次确认（双人复核）' : `${env}环境 · ${RISK_LBL[risk]}风险：需要 ${needs} 级审批（申请人不能自批）`] : ['ok', '审批', `${env}环境 · ${RISK_LBL[risk]}风险：免审批，但会留痕`]);
    if (env === '生产' && risk === 'high') steps.push(mfa ? ['ok', 'MFA', '申请人已通过 MFA'] : ['warn', 'MFA', '生产高风险：申请人需通过 MFA（TOTP）']);
    if (c[4]) steps.push(['info', '排期规则', '写操作还要满足变更排期规则：备份窗口 / 业务高峰 / 冻结期']);
    decision = needs ? 'approval' : 'allow';
  } else if (lvl === 'S') { steps.push(['info', '范围', '仅限被授权的库 / 集群；SQL 文本按脱敏策略展示']); }
  else steps.push(['ok', '审批', '无需审批']);
  steps.push(['info', '审计', '无论结果，本次请求都会写入审计（含被拒绝的请求）']);
  return {decision, steps, needs, reason: ''};
}

/* 命令白名单（与文档 3.1 一致） */
const SEC_CLUSTERS = [CLUSTER, 'stg-tidb-01', 'dev-tidb-01'];
const secAddrOk = a => NODES.some(n => `${n.host}:${n.port}` === a);
const SEC_WL = [
  {re: /^tiup cluster list$/, name: 'tiup cluster list', risk: '读', cap: 'overview', to: '15s', chk: () => ''},
  {re: /^tiup cluster display (\S+)$/, name: 'tiup cluster display <cluster>', risk: '读', cap: 'overview', to: '30s', chk: m => SEC_CLUSTERS.includes(m[1]) ? '' : `集群 ${m[1]} 未接入`},
  {re: /^tiup cluster check (\S+) --cluster$/, name: 'tiup cluster check <cluster> --cluster', risk: '读', cap: 'diag', to: '120s', chk: m => SEC_CLUSTERS.includes(m[1]) ? '' : `集群 ${m[1]} 未接入`},
  {re: /^tiup cluster (restart|stop|start) (\S+) -N (\S+)$/, name: 'tiup cluster restart|stop|start <cluster> -N <addr>', risk: '中 / 高', cap: 'inst', to: '600s', chk: m => !SEC_CLUSTERS.includes(m[2]) ? `集群 ${m[2]} 未接入` : !secAddrOk(m[3]) ? `实例 ${m[3]} 不在集群拓扑中` : ''},
  {re: /^tiup cluster scale-in (\S+) -N (\S+) -y$/, name: 'tiup cluster scale-in <cluster> -N <addr> -y', risk: '高', cap: 'scale', to: '3600s', chk: m => !SEC_CLUSTERS.includes(m[1]) ? `集群 ${m[1]} 未接入` : !secAddrOk(m[2]) ? `实例 ${m[2]} 不在集群拓扑中` : ''},
  {re: /^tiup cluster reload (\S+) (-N|-R) (\S+)$/, name: 'tiup cluster reload <cluster> -N <addr> | -R <role>', risk: '中', cap: 'config', to: '1800s', chk: m => !SEC_CLUSTERS.includes(m[1]) ? `集群 ${m[1]} 未接入` : m[2] === '-N' && !secAddrOk(m[3]) ? `实例 ${m[3]} 不在集群拓扑中` : m[2] === '-R' && !['tidb', 'tikv', 'pd', 'tiflash', 'cdc'].includes(m[3]) ? `角色 ${m[3]} 无效` : ''},
  {re: /^tiup cluster prune (\S+)$/, name: 'tiup cluster prune <cluster>', risk: '中', cap: 'scale', to: '300s', chk: m => SEC_CLUSTERS.includes(m[1]) ? '' : `集群 ${m[1]} 未接入`},
  {re: /^tiup ctl:(\S+) pd -u (\S+) scheduler (add|remove) (evict-leader-scheduler(?:-\d+)?)(?: (\d+))?$/, name: 'tiup ctl:<ver> pd -u <pd> scheduler add|remove evict-leader-scheduler <id>', risk: '中', cap: 'inst', to: '30s', chk: () => ''},
  {re: /^tiup ctl:(\S+) pd -u (\S+) config set (\S+) (\S+)$/, name: 'tiup ctl:<ver> pd -u <pd> config set <key> <value>', risk: '中 / 高', cap: 'config', to: '30s', chk: m => ['max-store-down-time', 'region-schedule-limit', 'leader-schedule-limit', 'max-replicas', 'location-labels'].includes(m[3]) ? '' : `PD 参数 ${m[3]} 不在白名单`},
];
const SEC_FORBID = ['destroy', 'clean', 'patch', 'rm ', 'ssh ', 'bash', 'sh -c', 'sudo', 'curl', 'wget'];
function secCheckCmd(cmd) {
  cmd = cmd.trim(); if (!cmd) return {ok: false, why: '命令为空'};
  if (/[;&|><`$(){}\\\n]/.test(cmd)) return {ok: false, why: '包含 shell 元字符（; & | > < ` $ ( ) { } \\），白名单要求结构化参数，禁止拼接 shell'};
  const f = SEC_FORBID.find(w => cmd.includes(w)); if (f) return {ok: false, why: `命中永远禁止的操作：${f.trim()}`};
  for (const w of SEC_WL) { const m = w.re.exec(cmd); if (m) { const e = w.chk(m); return e ? {ok: false, why: e, tpl: w} : {ok: true, tpl: w}; } }
  return {ok: false, why: '不在白名单模板中（未知子命令或参数格式不符）'};
}

/* 审计日志（哈希链） */
const SEC_EVENTS = [
  ['09:02:11', 'ops_zhang', 'sre', '生产', '登录', '—', '允许'], ['09:03:40', 'ops_zhang', 'sre', '生产', '运行诊断', 'prod-tidb-01', '成功'],
  ['09:12:05', 'dba_li', 'dba', '生产', '创建变更排期', 'S0002 扩容 TiKV', '成功'], ['09:30:18', 'ops_li', 'approver', '生产', '审批（1/1）', 'S0002', '允许'],
  ['09:41:02', 'ops_zhang', 'sre', '生产', '重启实例', 'tikv-1', '拒绝：生产写开关关闭'], ['10:05:27', 'admin_wu', 'admin', '生产', '修改平台设置', '通知渠道 → 飞书', '成功'],
  ['10:20:44', 'dba_li', 'dba', '测试', '配置变更', 'tikv.store-pool-size', '成功'], ['10:48:09', 'dev_chen', 'developer', '生产', '查看慢 SQL', 'shop 库', '允许（脱敏）'],
  ['11:02:51', 'dev_chen', 'developer', '生产', '查看慢 SQL', 'pay 库', '拒绝：无该库授权'], ['11:30:12', 'admin_wu', 'admin', '生产', '开启生产写开关（1/2）', '—', '待复核'],
  ['11:36:40', 'admin_zhao', 'admin', '生产', '开启生产写开关（2/2）', '—', '成功'], ['11:40:03', 'auditor_sun', 'auditor', '生产', '导出审计日志', '近 7 天', '成功'],
];
const secState = {tab: 'matrix', sim: {role: 'dba', cap: 'scale', env: '生产', risk: 'high', flags: {测试: true, 预发: false, 生产: false}, versionOk: true, mfa: false}, chain: [], verify: null, cmd: 'tiup cluster restart prod-tidb-01 -N 10.0.2.11:20160', check: {run: false, shown: 0, failOne: false}};
async function secHash(s) {
  if (window.crypto && crypto.subtle) { const b = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s)); return [...new Uint8Array(b)].map(x => x.toString(16).padStart(2, '0')).join(''); }
  let h = 2166136261; for (const c of s) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); } return ('00000000' + (h >>> 0).toString(16)).repeat(8);
}
const secBody = e => JSON.stringify(e.f);
async function secBuild() {
  secState.chain = []; let prev = '0'.repeat(64);
  for (let i = 0; i < SEC_EVENTS.length; i++) { const f = SEC_EVENTS[i], e = {seq: i + 1, f: [...f], prev}; e.hash = await secHash(prev + secBody(e)); secState.chain.push(e); prev = e.hash; }
  secState.verify = null;
}
async function secVerify() {
  let prev = '0'.repeat(64); const bad = [];
  for (const e of secState.chain) {
    if (e.prev !== prev) bad.push({seq: e.seq, why: '与前一条记录的哈希不连续（中间有记录被删除或替换）'});
    else if (await secHash(e.prev + secBody(e)) !== e.hash) bad.push({seq: e.seq, why: '记录内容与其哈希不一致（内容被修改）'});
    prev = e.hash;
  }
  secState.verify = {bad, at: new Date()};
}
const SEC_CHECKS = [
  ['运行账号为非 root 专用用户 tidbops', true, 'ok', 'uid=1002(tidbops) gid=1002，无登录 shell'],
  ['Web 进程无法读取 SSH 私钥 / tiup 目录', true, 'ok', 'open(~tidb/.ssh/id_rsa) → Permission denied'],
  ['sudoers 只允许唯一的 exec 入口', true, 'ok', '/etc/sudoers.d/tidb-ops：tidbops ALL=(tidb) NOPASSWD: /opt/tidb-ops/tidb-ops exec --stdin'],
  ['systemd 加固（NoNewPrivileges / ProtectSystem=strict / PrivateTmp）', true, 'ok', 'systemd-analyze security：1.8 OK'],
  ['仅监听内网地址', false, 'warn', '当前监听 0.0.0.0:8080，建议改为 10.0.0.5:8080 并由反向代理对外'],
  ['对外访问经 TLS（反向代理）', true, 'ok', 'TLS 1.3，证书剩余 212 天'],
  ['审计表对平台账号仅 INSERT / SELECT', true, 'ok', "SHOW GRANTS FOR 'tidbops'@'%'：无 UPDATE / DELETE"],
  ['审计哈希链校验通过', true, 'ok', '最近 30,000 条记录无断裂'],
  ['密钥文件权限为 0400，属主 tidbops', true, 'ok', '/etc/tidb-ops/master.key  0400'],
  ['默认口令已修改（无 admin/admin）', true, 'ok', '未发现默认口令'],
  ['READONLY 熔断文件不存在', false, 'ok', '/etc/tidb-ops/READONLY 不存在 → 允许按设置开启写操作'],
  ['依赖漏洞扫描', false, 'warn', '发现 2 个中危（非关键），建议下个版本修复'],
  ['时间同步正常（审计时间可信）', false, 'ok', 'chronyc tracking：偏移 3ms'],
];

pages.security = {
  title: '权限与审计',
  render() {
    return `<div class="tabs" id="secTabs">${[['matrix', '权限矩阵'], ['sim', '权限模拟器'], ['policy', '审批策略'], ['audit', '审计日志'], ['wl', '命令白名单'], ['check', '部署自检']].map(([k, t]) => `<button data-na="sec-tab" data-t="${k}" class="${secState.tab === k ? 'active' : ''}">${t}</button>`).join('')}</div><div id="secBody"></div>`;
  },
  async init() { if (!secState.chain.length) await secBuild(); this.body(); },
  body() {
    const el = $('#secBody'); if (!el) return; el.innerHTML = this[secState.tab]();
    $$('#secTabs button').forEach(b => b.classList.toggle('active', b.dataset.t === secState.tab));
  },
  matrix() {
    const sym = {Y: '<span style="color:var(--ok);font-weight:700">✔</span>', R: '<span class="tag warn">审</span>', S: '<span class="tag info">范</span>', '-': '<span class="muted">—</span>'};
    return `<div class="card"><h2>角色 × 能力 <span class="sub">${SEC_ROLES.length} 个角色 · ${SEC_CAPS.length} 项能力</span></h2>
      <div class="legend" style="margin:0 0 10px"><span>✔ 直接允许</span><span><span class="tag warn">审</span> 需按“风险 × 环境”走审批</span><span><span class="tag info">范</span> 仅限被授权范围</span><span>— 无</span></div>
      <div class="tbl-wrap"><table><thead><tr><th>能力</th><th>风险</th>${SEC_ROLES.map(r => `<th style="text-align:center">${r[1]}</th>`).join('')}</tr></thead><tbody>
      ${SEC_CAPS.map(c => `<tr><td>${c[1]}${c[4] ? ' <span class="tag bad">写操作</span>' : ''}</td><td class="small">${c[2]}</td>${[...c[5]].map(l => `<td style="text-align:center">${sym[l]}</td>`).join('')}</tr>`).join('')}</tbody></table></div>
      <div class="grid g3" style="margin-top:14px">
        <div class="notice info"><span>⚖</span><div><b>职责分离</b><br>执行者 ≠ 审批者 ≠ 管理者：申请人不能自批；管理员不能直接执行集群变更；审批人不能执行变更。</div></div>
        <div class="notice info"><span>🌍</span><div><b>按环境授权</b><br>角色可分环境、分集群授予：同一个人在测试是 DBA，在生产可以只是 SRE。</div></div>
        <div class="notice info"><span>🧪</span><div>切到「权限模拟器」，选角色 / 能力 / 环境，看每一步判定。</div></div></div></div>`;
  },
  sim() {
    const S = secState.sim, r = secAuthorize(S), c = secCap(S.cap), dec = {allow: ['ok', '允许'], approval: ['warn', `需 ${r.needs} 级审批`], deny: ['bad', '拒绝']}[r.decision];
    const audit = {ts: '2026-xx-xx hh:mm:ss', actor: '(当前用户)', role: S.role, env: S.env, capability: S.cap, risk: RISK_LBL[S.risk], result: r.decision === 'deny' ? '拒绝：' + r.reason : r.decision === 'approval' ? `待审批(0/${r.needs})` : '允许', prev_hash: '…', hash: 'sha256(…)'};
    return `<div class="grid g-main" style="grid-template-columns:360px minmax(0,1fr)"><div class="card"><h2>输入</h2>
      <div class="field"><label>角色</label><select data-nc="sec-sim" data-k="role">${SEC_ROLES.map(r => `<option value="${r[0]}" ${S.role === r[0] ? 'selected' : ''}>${r[1]}</option>`).join('')}</select></div>
      <div class="field"><label>能力 / 动作</label><select data-nc="sec-sim" data-k="cap">${SEC_CAPS.map(x => `<option value="${x[0]}" ${S.cap === x[0] ? 'selected' : ''}>${x[1]}</option>`).join('')}</select></div>
      <div class="field"><label>环境</label><select data-nc="sec-sim" data-k="env">${SEC_ENVS.map(e => `<option ${S.env === e ? 'selected' : ''}>${e}</option>`).join('')}</select></div>
      <div class="field"><label>风险等级</label><select data-nc="sec-sim" data-k="risk">${['low', 'mid', 'high'].map(k => `<option value="${k}" ${S.risk === k ? 'selected' : ''}>${RISK_LBL[k]}</option>`).join('')}</select></div>
      <div class="hr"></div><b class="small">环境条件</b>
      ${SEC_ENVS.map(e => `<label class="small" style="display:flex;gap:6px;margin-top:4px"><input type="checkbox" data-nc="sec-flag" data-e="${e}" ${S.flags[e] ? 'checked' : ''}> ${e}环境写操作开关${e === '生产' ? '（默认关闭）' : ''}</label>`).join('')}
      <label class="small" style="display:flex;gap:6px;margin-top:4px"><input type="checkbox" data-nc="sec-ver" ${S.versionOk ? 'checked' : ''}> 集群版本在支持矩阵内</label>
      <label class="small" style="display:flex;gap:6px;margin-top:4px"><input type="checkbox" data-nc="sec-mfa" ${S.mfa ? 'checked' : ''}> 申请人已通过 MFA</label></div>
      <div class="grid" style="align-content:start"><div class="card"><h2>判定结果 <span class="right"><span class="tag ${dec[0]}" style="font-size:14px;padding:2px 14px">${dec[1]}</span></span></h2>
        ${r.steps.map(([lv, n, t]) => `<div class="notice ${lv === 'info' ? 'info' : lv}"><span>${{ok: '✔', warn: '⚠', bad: '✖', info: 'ℹ'}[lv]}</span><div><b>${n}</b>：${t}</div></div>`).join('')}</div>
        <div class="card"><h2>执行路径与命令</h2><div class="small">${c[4] && r.decision !== 'deny' ? `<div class="flow"><span class="st done">浏览器</span>›<span class="st done">Web 进程（tidbops）</span>›<span class="st cur">sudo -u tidb tidb-ops exec</span>›<span class="st">白名单校验 + 审批令牌</span>›<span class="st">tiup</span></div>` : r.decision === 'deny' ? '<span class="muted">已被拒绝，不会进入执行器</span>' : '<span class="muted">读操作 / 非集群变更：由 Web 进程用只读账号或内部逻辑完成，不经过 exec 入口</span>'}</div>
          ${code(c[6])}</div>
        <div class="card"><h2>将写入的审计记录</h2>${code(JSON.stringify(audit, null, 2))}</div></div></div>`;
  },
  policy() {
    return `<div class="grid g2"><div class="card"><h2>审批级数（风险 × 环境）</h2><div class="tbl-wrap"><table><thead><tr><th>风险</th>${SEC_ENVS.map(e => `<th style="text-align:center">${e}</th>`).join('')}</tr></thead><tbody>
      ${['low', 'mid', 'high'].map(k => `<tr><td>${RISK_LBL[k]}</td>${SEC_APPROVAL[k].map((n, i) => `<td style="text-align:center">${n ? tag(n + ' 级', n > 1 ? 'bad' : 'warn') : tag('免审批', 'ok')}${k === 'high' && i === 2 ? '<div class="small muted">+ 申请人 MFA</div>' : ''}</td>`).join('')}</tr>`).join('')}
      <tr><td>紧急（冻结期）</td><td style="text-align:center" class="muted">—</td><td style="text-align:center">${tag('2 级', 'bad')}</td><td style="text-align:center">${tag('2 级', 'bad')}<div class="small muted">+ 事后复盘必填</div></td></tr></tbody></table></div></div>
      <div class="card"><h2>审批规则</h2><ul class="small" style="padding-left:18px;margin:0;display:grid;gap:6px">
        <li>申请人<b>不能审批自己</b>的变更；同一审批人不能重复审批同一变更</li><li>2 级审批必须是<b>两个不同的人</b>，且具备对应环境的审批角色</li><li>审批<b>有效期 24 小时</b>，过期作废，需重新申请</li><li>审批通过后，平台签发<b>短期审批令牌</b>（HMAC，含请求摘要与过期时间），执行器校验令牌才执行</li>
        <li><b>双人复核</b>：开启 / 关闭生产写开关、修改审批策略、授予管理员角色，必须两名管理员先后确认</li><li><b>紧急熔断</b>：管理员可一键关闭所有写操作；本机存在 <code>/etc/tidb-ops/READONLY</code> 文件时强制只读</li></ul></div></div>
      <div class="card" style="margin-top:16px"><h2>与「变更排期」的关系</h2><div class="small">审批策略决定“要不要审批、几级”；变更排期决定“什么时候能执行”（备份窗口、业务高峰、冻结期、冲突）。两者都通过，变更才会到点自动执行。变更排期页里的“申请人不能自批、高风险 2 级审批”就是本策略的体现。</div></div>`;
  },
  audit() {
    const v = secState.verify, badSeq = v ? new Set(v.bad.map(b => b.seq)) : new Set(), first = v && v.bad[0] ? v.bad[0].seq : null;
    return `<div class="card"><h2>审计日志 <span class="sub">SHA-256 哈希链 · 数据库仅允许追加</span><span class="right"><button class="btn primary sm" data-na="sec-verify">校验哈希链</button><button class="btn sm danger" data-na="sec-tamper">模拟篡改第 5 条</button><button class="btn sm danger" data-na="sec-delete">模拟删除第 4 条</button><button class="btn sm" data-na="sec-reset">重置</button></span></h2>
      ${v ? (v.bad.length ? `<div class="notice bad"><span>✖</span><div><b>哈希链断裂</b>：从第 ${first} 条起校验失败。${v.bad.map(b => `第 ${b.seq} 条：${b.why}`).join('；')}。真实系统此时会触发告警并停止开启写操作。</div></div>` : `<div class="notice ok"><span>✔</span><div>哈希链完整：${secState.chain.length} 条记录全部校验通过（${v.at.toLocaleTimeString()}）</div></div>`) : '<div class="small muted" style="margin-bottom:8px">点击“校验哈希链”；也可以先“模拟篡改 / 删除”，再校验看会发生什么。每条记录的哈希 = SHA-256(前一条哈希 + 本条内容)。</div>'}
      <div class="tbl-wrap"><table><thead><tr><th>#</th><th>时间</th><th>操作人</th><th>角色</th><th>环境</th><th>动作</th><th>目标</th><th>结果</th><th>哈希</th></tr></thead><tbody>
      ${secState.chain.map(e => `<tr class="${badSeq.has(e.seq) ? 'flag' : ''}"><td>${e.seq}</td><td class="mono small">${e.f[0]}</td><td>${e.f[1]}</td><td>${tag(e.f[2])}</td><td>${e.f[3]}</td><td>${e.f[4]}</td><td class="small">${esc(e.f[5])}</td><td class="small">${e.f[6].startsWith('拒绝') ? `<span style="color:var(--bad)">${esc(e.f[6])}</span>` : esc(e.f[6])}</td><td class="mono small muted" title="${e.hash}">${e.hash.slice(0, 10)}…</td></tr>`).join('')}</tbody></table></div>
      <div class="small muted" style="margin-top:8px">记录范围：登录 / 读写请求（含被拒绝）/ 审批 / 设置修改 / 写开关变化 / 执行器每条命令。保留 180 天（可配置，到期归档）；可选实时转发 syslog。</div></div>`;
  },
  wl() {
    const ck = secCheckCmd(secState.cmd);
    return `<div class="grid g-main" style="grid-template-columns:minmax(0,1fr) 420px"><div class="card"><h2>白名单模板 <span class="sub">${SEC_WL.length} 条（执行器仅允许这些）</span></h2><div class="tbl-wrap"><table><thead><tr><th>命令模板</th><th>风险</th><th>所需能力</th><th>超时</th></tr></thead><tbody>
      ${SEC_WL.map(w => `<tr><td class="mono small">${esc(w.name)}</td><td>${tag(w.risk, w.risk.includes('高') ? 'bad' : w.risk === '读' ? 'ok' : 'warn')}</td><td class="small">${secCap(w.cap)[1]}</td><td class="small">${w.to}</td></tr>`).join('')}</tbody></table></div>
      <div class="notice bad" style="margin-top:10px"><span>⛔</span><div><b>永远禁止</b>：tiup cluster destroy / clean / patch；任意 rm / ssh / bash -c / sudo / curl；未知 flag；管道、重定向、反引号、$()；用户可控的文件路径。</div></div></div>
      <div class="card"><h2>命令校验器</h2><div class="small muted" style="margin-bottom:6px">模拟执行器的校验逻辑：输入一条命令，看是否放行。</div>
        <textarea data-ni="sec-cmd" rows="3" style="width:100%" class="mono">${esc(secState.cmd)}</textarea>
        <div class="row" style="margin:8px 0"><button class="btn sm" data-na="sec-ex" data-c="tiup cluster restart prod-tidb-01 -N 10.0.2.11:20160">合法：重启实例</button><button class="btn sm" data-na="sec-ex" data-c="tiup cluster destroy prod-tidb-01">危险：destroy</button><button class="btn sm" data-na="sec-ex" data-c="tiup cluster display prod-tidb-01; rm -rf /">注入：; rm</button><button class="btn sm" data-na="sec-ex" data-c="tiup cluster restart prod-tidb-01 -N 10.9.9.9:20160">越界：非拓扑实例</button></div>
        <div class="notice ${ck.ok ? 'ok' : 'bad'}"><span>${ck.ok ? '✔' : '✖'}</span><div><b>${ck.ok ? '放行' : '拒绝'}</b>${ck.ok ? `：匹配模板「${esc(ck.tpl.name)}」，需要能力「${secCap(ck.tpl.cap)[1]}」，超时 ${ck.tpl.to}` : `：${esc(ck.why)}`}</div></div></div></div>`;
  },
  check() {
    const C = secState.check, items = SEC_CHECKS.map((c, i) => i === 9 && C.failOne ? [c[0], c[1], 'bad', '检测到默认口令 admin/admin'] : c), shown = C.run ? C.shown : items.length;
    const critBad = items.slice(0, shown).some(c => c[1] && c[2] === 'bad'), done = shown >= items.length && C.run;
    return `<div class="card"><h2>部署自检 <span class="sub">启动时与每日执行；对应 A3 第 3.3 节</span><span class="right"><label class="small"><input type="checkbox" data-nc="sec-fail" ${C.failOne ? 'checked' : ''}> 模拟一项关键检查失败</label><button class="btn primary sm" data-na="sec-run">运行自检</button></span></h2>
      ${done ? (critBad ? '<div class="notice bad"><span>✖</span><div><b>关键项未通过</b>：生产环境写操作被强制保持关闭（只读），并向管理员告警。修复后重新自检才可开启。</div></div>' : '<div class="notice ok"><span>✔</span><div>关键项全部通过，允许按设置开启写操作。非关键项的警告请在下个版本处理。</div></div>') : ''}
      <div class="tbl-wrap"><table><thead><tr><th>检查项</th><th>级别</th><th>结果</th><th>详情</th></tr></thead><tbody>
      ${items.slice(0, shown).map(c => `<tr class="${c[2] === 'bad' ? 'flag' : ''}"><td>${c[0]}</td><td>${c[1] ? tag('关键', 'bad') : tag('一般')}</td><td>${tag({ok: '通过', warn: '警告', bad: '失败'}[c[2]], c[2])}</td><td class="small mono">${esc(c[3])}</td></tr>`).join('')}</tbody></table></div></div>`;
  },
};
document.addEventListener('click', async e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled || !el.dataset.na.startsWith('sec-')) return; const d = el.dataset, P = pages.security;
  switch (d.na) {
    case 'sec-tab': secState.tab = d.t; P.body(); break;
    case 'sec-verify': await secVerify(); P.body(); break;
    case 'sec-tamper': if (secState.chain[4]) { secState.chain[4].f[6] = '允许（被篡改）'; secState.verify = null; toast('已修改第 5 条的结果字段（未更新哈希）'); P.body(); } break;
    case 'sec-delete': if (secState.chain.length > 3) { secState.chain.splice(3, 1); secState.verify = null; toast('已删除第 4 条记录'); P.body(); } break;
    case 'sec-reset': await secBuild(); P.body(); toast('已重置'); break;
    case 'sec-ex': secState.cmd = d.c; P.body(); break;
    case 'sec-run': { const C = secState.check; C.run = true; C.shown = 0; const t = setInterval(() => { C.shown++; if (location.hash === '#security' && secState.tab === 'check') P.body(); if (C.shown >= SEC_CHECKS.length) clearInterval(t); }, 220); break; }
  }
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc]'); if (!el) return; const S = secState.sim, d = el.dataset;
  switch (d.nc) {
    case 'sec-sim': S[d.k] = el.value; if (d.k === 'cap') S.risk = secCap(el.value)[3]; pages.security.body(); break;
    case 'sec-flag': S.flags[d.e] = el.checked; pages.security.body(); break;
    case 'sec-ver': S.versionOk = el.checked; pages.security.body(); break;
    case 'sec-mfa': S.mfa = el.checked; pages.security.body(); break;
    case 'sec-fail': secState.check.failOne = el.checked; pages.security.body(); break;
  }
});
document.addEventListener('input', e => { const el = e.target.closest('[data-ni="sec-cmd"]'); if (el) { secState.cmd = el.value; const ck = secCheckCmd(el.value), box = el.parentElement.querySelector('.notice'); if (box) { box.className = 'notice ' + (ck.ok ? 'ok' : 'bad'); box.innerHTML = `<span>${ck.ok ? '✔' : '✖'}</span><div><b>${ck.ok ? '放行' : '拒绝'}</b>${ck.ok ? `：匹配模板「${esc(ck.tpl.name)}」，需要能力「${secCap(ck.tpl.cap)[1]}」，超时 ${ck.tpl.to}` : `：${esc(ck.why)}`}</div>`; } } });
