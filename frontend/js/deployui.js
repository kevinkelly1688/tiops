// @author: kevinkelly
// @license: Apache License 2.0
// @file: deployui.js
// @time: 2026/10/28

/* 安装部署页：选择目标系统 / 操作系统用户 / TiDB 系列版本 / 在线或离线（含离线包完整目录）→ 实时校验 → 生成 deploy.conf 与命令，
   并提供离线包与部署工具包的下载入口。规则与 deploy/tidb-deploy.sh 保持一致（版本门槛、校验、拓扑生成）。 */
const DP_TOOL = 'tidb-deploy-0.3.1';
const DP_OS = [
  ['centos7', 'CentOS 7.x', 2.17, 'legacy', 'glibc 2.17 · 上游 2024-06 已停止维护，yum 源可能失效'],
  ['centos8', 'CentOS Linux 8', 2.28, 'legacy', 'glibc 2.28 · 上游 2021-12 已停止维护，yum 源可能失效'],
  ['stream8', 'CentOS Stream 8', 2.28, 'compat', 'glibc 2.28 · 官方兼容平台'],
  ['stream9', 'CentOS Stream 9', 2.34, 'compat', 'glibc 2.34 · 官方兼容平台'],
  ['stream10', 'CentOS Stream 10', 2.39, 'compat', 'glibc 2.39 · 未经本工具实测'],
  ['rocky8', 'Rocky Linux 8', 2.28, 'ok', 'glibc 2.28'], ['rocky9', 'Rocky Linux 9', 2.34, 'ok', 'glibc 2.34 · 官方推荐'],
  ['rocky10', 'Rocky Linux 10', 2.39, 'ok', 'glibc 2.39 · 未经本工具实测'],
  ['alma9', 'AlmaLinux 9', 2.34, 'ok', 'glibc 2.34'], ['alma10', 'AlmaLinux 10', 2.39, 'ok', 'glibc 2.39 · 未经本工具实测'],
  ['rhel9', 'RHEL 9', 2.34, 'ok', 'glibc 2.34'], ['rhel10', 'RHEL 10', 2.39, 'ok', 'glibc 2.39 · 未经本工具实测'],
];
const DP_SERIES = ['v6.5', 'v7.1', 'v7.5', 'v8.1', 'v8.5'];     // 官方 LTS 系列
const dpState = {
  os: 'rocky9', mode: 'single', cluster: 'tidb-test', user: 'tidb', userSel: 'tidb',
  sshKey: '', sshPort: '22',
  verMode: 'auto', series: 'v8.5', seriesCustom: '', full: '',
  install: 'online', offlineDir: '/opt/tidb-pkg', rpmDir: '', arch: 'amd64',
  singleHost: '', pd: '10.0.1.4 10.0.1.5 10.0.1.6', tidb: '10.0.1.1 10.0.1.2', tikv: '10.0.1.7 10.0.1.8 10.0.1.9', tiflash: '', monitor: '', zone: '',
  deployDir: '/tidb-deploy', dataDir: '/tidb-data', spec: 'dev', firewall: 'open', repoFix: 'skip', repoMirror: 'aliyun', withMonitor: true, allowCheckFail: false,
  dlVersion: '', dlToolkit: true, dlDir: '/opt/tidb-pkg', tab: 'conf',
};
const dpOs = () => DP_OS.find(o => o[0] === dpState.os);
const dpDir = s => String(s).trim().replace(/(.)\/+$/, '$1');
const dpHosts = s => String(s).replace(/[,;]/g, ' ').trim().split(/\s+/).filter(Boolean);
const dpIP = s => /^(\d{1,3}\.){3}\d{1,3}$/.test(s) && s.split('.').every(x => +x <= 255);
const dpParts = v => String(v).replace(/^v/, '').split('.').map(Number);
const dpGe = (a, b) => { const x = dpParts(a), y = dpParts(b); for (let i = 0; i < 3; i++) { const p = x[i] || 0, q = y[i] || 0; if (p !== q) return p > q; } return true; };
const dpGate = (ver, g) => !/^v?\d+\.\d+(\.\d+)?$/.test(ver) ? '版本号格式不正确'
  : (g < 2.28 && dpGe(ver, '8.4.0')) ? `当前系统 glibc 为 ${g}（CentOS 7 级别），而 TiDB ${ver} 需要 glibc ≥ 2.28；官方自 v8.4 起不再支持 CentOS 7。请改用 v8.1.x / v7.5.x LTS，或把系统升级到 Rocky Linux 9 / CentOS Stream 9。` : '';
const dpDefSeries = g => g >= 2.28 ? 'v8.5' : 'v7.5';

/* 用户选择的版本参数（对应 TIDB_VERSION）与用于校验的版本 */
function dpVersion() {
  const S = dpState, g = dpOs()[2];
  if (S.verMode === 'auto') return {spec: 'auto', check: dpDefSeries(g), label: `自动（按系统选 ${dpDefSeries(g)} 系列）`};
  if (S.verMode === 'series') { const v = S.series === 'custom' ? S.seriesCustom.trim() : S.series; const vv = v && !/^v/.test(v) ? 'v' + v : v; return {spec: vv, check: vv, label: `${vv} 系列（取最新补丁版本）`}; }
  const f = S.full.trim(), ff = f && !/^v/.test(f) ? 'v' + f : f; return {spec: ff, check: ff, label: ff};
}
const dpExactVersion = () => { const v = dpVersion(); return /^v\d+\.\d+\.\d+$/.test(v.spec) ? v.spec : ''; };

function dpValidate() {
  const S = dpState, E = [], W = [], O = [], os = dpOs(), g = os[2], ver = dpVersion();
  // 系统
  if (os[3] === 'legacy') W.push(`${os[1]} 上游已停止维护：官方 yum 源可能失效。可把「yum 源」设为自动切换 vault 镜像，生产环境建议迁移到 Rocky Linux 9 / CentOS Stream 9。`); else O.push(`${os[1]}：${os[4]}`);
  // 版本
  if (S.verMode === 'series' && !/^v\d+\.\d+$/.test(ver.spec)) E.push('系列版本格式应为 vX.Y，例如 v8.5');
  else if (S.verMode === 'full' && !/^v\d+\.\d+\.\d+$/.test(ver.spec)) E.push('完整版本格式应为 vX.Y.Z，例如 v7.5.4');
  else { const why = dpGate(ver.check, g); if (why) E.push(why); else O.push(`TiDB ${ver.check} 与 ${os[1]} 兼容`); }
  // 用户
  if (!/^[a-z_][a-z0-9_-]{0,31}$/.test(S.user)) E.push(`操作系统用户名不合法：只能含小写字母、数字、下划线、连字符，最长 32 位（当前「${S.user}」）`);
  else if (S.user === 'root') E.push('操作系统用户不能是 root：TiUP 应以普通用户运行，脚本会为其配置 sudo 免密');
  else O.push(`操作系统用户：${S.user}（不存在会自动创建，并配置 sudo 免密与 SSH 互信）`);
  W.push(`部署前请先在所有机器（含中控机）上配好用户 ${S.user}：开通 sudo 免密，且 ${S.user} 能用密钥免密 SSH 登录所有机器（含中控机自己）。脚本只检查、不代劳，不满足会中止（命令见「命令」页第 0 步）`);
  if (S.sshKey.trim() && !S.sshKey.trim().startsWith('/')) E.push('SSH 私钥路径需要是绝对路径');
  // 目录
  for (const [k, v] of [['部署目录', S.deployDir], ['数据目录', S.dataDir]]) if (!v.startsWith('/')) E.push(`${k}必须是绝对路径`);
  if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$/.test(S.cluster)) E.push('集群名只能含字母数字、下划线、连字符，且不超过 32 位');
  // 主机
  const lists = S.mode === 'single' ? {单机: dpHosts(S.singleHost)} : {PD: dpHosts(S.pd), TiDB: dpHosts(S.tidb), TiKV: dpHosts(S.tikv), TiFlash: dpHosts(S.tiflash), 监控: dpHosts(S.monitor)};
  for (const [r, l] of Object.entries(lists)) {
    l.forEach(h => { if (!dpIP(h)) E.push(`${r} 的主机「${h}」不是合法的 IPv4 地址`); });
    if (new Set(l).size !== l.length) E.push(`${r} 里有重复的主机（同一主机上同一角色只部署一个实例）`);
  }
  if (S.mode === 'single') {
    if (lists.单机.length > 1) E.push('单机模式只能填一个主机（留空则使用本机主 IP）');
    W.push('单机模式：所有组件同机、1 个 TiKV 副本，仅用于开发测试，不具备高可用；建议 ≥ 8 核 / 16GB');
  } else {
    if (!lists.PD.length) E.push('至少需要 1 台 PD'); if (!lists.TiDB.length) E.push('至少需要 1 台 TiDB'); if (!lists.TiKV.length) E.push('至少需要 1 台 TiKV');
    if (lists.PD.length && lists.PD.length % 2 === 0) W.push(`PD 为 ${lists.PD.length} 台（偶数），不提升容错，建议奇数`);
    if (lists.PD.length < 3 || lists.TiKV.length < 3 || lists.TiDB.length < 2) {
      if (S.spec === 'prod') E.push(`生产规格要求 PD ≥ 3、TiKV ≥ 3、TiDB ≥ 2（当前 ${lists.PD.length} / ${lists.TiKV.length} / ${lists.TiDB.length}）`);
      else W.push(`当前 PD ${lists.PD.length} · TiKV ${lists.TiKV.length} · TiDB ${lists.TiDB.length}，低于生产建议，仅适合测试`);
    }
    if (S.zone.trim()) {
      const pairs = S.zone.replace(/\s+/g, '').split(',').filter(Boolean);
      if (pairs.some(p => !/^[0-9.]+=[A-Za-z0-9_-]+$/.test(p))) E.push('拓扑标签格式应为 IP=zone，多个用逗号分隔');
      else { const m = Object.fromEntries(pairs.map(p => p.split('='))); lists.TiKV.forEach(h => { if (!m[h]) E.push(`拓扑标签缺少 TiKV 主机 ${h} 的 zone`); }); }
    }
    if (lists.TiFlash.length) W.push('TiFlash 要求 CPU 支持 AVX2，部署前检查会验证');
  }
  // 安装方式
  if (S.install === 'offline') {
    if (!S.offlineDir.trim()) E.push('离线安装需要填写离线包所在的完整目录');
    else if (!S.offlineDir.startsWith('/')) E.push(`离线包目录必须是完整的绝对路径（以 / 开头），当前「${S.offlineDir}」`);
    else if (S.offlineDir.trim() === '/') E.push('离线包目录不能是根目录 /');
    else O.push(`离线安装：脚本会在 ${S.offlineDir.replace(/\/$/, '')} 中按版本自动查找 tidb-community-server-*-linux-${S.arch}.tar.gz`);
    if (S.rpmDir && !S.rpmDir.startsWith('/')) E.push('离线 rpm 目录必须是绝对路径');
  } else O.push('在线安装：从 tiup-mirrors.pingcap.com 下载 TiUP，组件由 TiUP 在线拉取');
  if (!E.length) O.push('配置通过校验，可以下载 deploy.conf');
  return {E, W, O};
}

function dpTopology() {
  const S = dpState, single = S.mode === 'single', h = single ? (dpHosts(S.singleHost)[0] || '<本机IP>') : null;
  const pd = single ? [h] : dpHosts(S.pd), td = single ? [h] : dpHosts(S.tidb), kv = single ? [h] : dpHosts(S.tikv), tf = dpHosts(S.tiflash), mon = S.withMonitor ? (single ? [h] : (dpHosts(S.monitor).length ? dpHosts(S.monitor) : pd.slice(0, 1))) : [];
  const rep = Math.max(1, Math.min(3, kv.length)), zone = Object.fromEntries(S.zone.replace(/\s+/g, '').split(',').filter(Boolean).map(p => p.split('=')));
  let t = `# 预览：部署时由 tidb-deploy.sh 生成\nglobal:\n  user: "${S.user}"\n  ssh_port: ${S.sshPort}\n  deploy_dir: "${S.deployDir}"\n  data_dir: "${S.dataDir}"\n\nserver_configs:\n  pd:\n    replication.max-replicas: ${rep}\n`;
  if (!single && S.zone.trim()) t += '    replication.location-labels: ["zone", "host"]\n';
  if (single) t += '  # single 模式：部署时按目标机内存的 25% 设置 tikv 的 storage.block-cache.capacity\n';
  t += '\npd_servers:\n' + pd.map(x => `  - host: ${x}\n`).join('') + '\ntidb_servers:\n' + td.map(x => `  - host: ${x}\n`).join('') + '\ntikv_servers:\n';
  t += kv.map(x => `  - host: ${x}\n` + (zone[x] ? `    config:\n      server.labels: { zone: "${zone[x]}", host: "${x.replace(/\./g, '-')}" }\n` : '')).join('');
  if (tf.length) t += '\ntiflash_servers:\n' + tf.map(x => `  - host: ${x}\n`).join('');
  if (mon.length) t += `\nmonitoring_servers:\n  - host: ${mon[0]}\n\ngrafana_servers:\n  - host: ${mon[0]}\n\nalertmanager_servers:\n  - host: ${mon[0]}\n`;
  return t;
}

function dpConf() {
  const S = dpState, v = dpVersion(), q = x => `"${String(x).replace(/"/g, '\\"')}"`;
  const L = ['# 由「安装部署」页面生成；用法: ./tidb-deploy.sh deploy --config deploy.conf --dry-run', `CLUSTER_NAME=${S.cluster}`, `TIDB_VERSION=${v.spec}`, `DEPLOY_MODE=${S.mode}`];
  if (S.mode === 'single') L.push(`SINGLE_HOST=${q(S.singleHost.trim())}`);
  else { L.push(`PD_HOSTS=${q(dpHosts(S.pd).join(' '))}`, `TIDB_HOSTS=${q(dpHosts(S.tidb).join(' '))}`, `TIKV_HOSTS=${q(dpHosts(S.tikv).join(' '))}`, `TIFLASH_HOSTS=${q(dpHosts(S.tiflash).join(' '))}`, `MONITOR_HOST=${q(dpHosts(S.monitor).join(' '))}`); if (S.zone.trim()) L.push(`ZONE_MAP=${q(S.zone.replace(/\s+/g, ''))}`); }
  L.push(`WITH_MONITOR=${S.withMonitor}`, '', '# 操作系统用户（运行 TiUP 与集群）', `DEPLOY_USER=${S.user}`, `SSH_PORT=${S.sshPort}`, `DEPLOY_DIR=${S.deployDir}`, `DATA_DIR=${S.dataDir}`,
    '', `# 前置条件：所有机器（含中控机）已配好 ${S.user} 用户、sudo 免密、${S.user} 免密 SSH 登录所有机器；脚本以 ${S.user}@IP + sudo 登录，只检查不代劳`, '# 部署用户的 SSH 私钥（绝对路径）；留空 = 该用户家目录的 ~/.ssh/id_rsa', `SSH_KEY=${q(S.sshKey.trim())}`,
    '', `SPEC=${S.spec}`, `FIREWALL=${S.firewall}`, `REPO_FIX=${S.repoFix}`, `REPO_MIRROR=${S.repoMirror}`, '', `INSTALL_MODE=${S.install}`);
  if (S.allowCheckFail && S.spec === 'dev') L.push('', '# 仅测试环境：tiup check 复查仍有 Fail 时继续部署（会忽略全部 Fail 项）', 'ALLOW_CHECK_FAIL=true');
  if (S.install === 'offline') { L.push(`OFFLINE_PKG_DIR=${q(S.offlineDir.trim().replace(/(.)\/$/, '$1'))}   # 离线包所在的完整目录，脚本按版本自动查找`, `OFFLINE_RPM_DIR=${q(S.rpmDir)}`); }
  return L.join('\n') + '\n';
}

function dpCmds() {
  const S = dpState, v = dpVersion(), ex = dpExactVersion(), os = dpOs(), C = [];
  const dlv = S.dlVersion.trim() || ex;
  const U = S.user, key = S.sshKey.trim() || '~/.ssh/id_rsa', port = S.sshPort || '22';
  const hosts = S.mode === 'single' ? [dpHosts(S.singleHost)[0] || '<本机IP>'] : [...new Set([...dpHosts(S.pd), ...dpHosts(S.tidb), ...dpHosts(S.tikv), ...dpHosts(S.tiflash), ...dpHosts(S.monitor)])];
  C.push([`0. 前置条件（部署前由你完成，脚本只检查、不代劳）· 在每台部署机器和中控机上，用 root 执行`,
    `useradd -m -s /bin/bash ${U} && passwd ${U}\necho '${U} ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/${U} && chmod 440 /etc/sudoers.d/${U} && visudo -cf /etc/sudoers.d/${U}`]);
  C.push([`0. 前置条件 · 分发公钥要用口令登录：若 sshd 禁用了口令登录，先临时打开（每台机器，root 执行；已允许可跳过）`,
    "sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config && systemctl restart sshd"]);
  C.push([`0. 前置条件 · 在中控机以 ${U} 身份生成密钥，并分发到所有机器（包括中控机自己）；每台输入一次 ${U} 的口令`,
    `sudo -iu ${U}\n[ -f ${key} ] || ssh-keygen -t rsa -b 4096 -N '' -f ${key}\n${[...hosts, ...(S.mode === 'cluster' ? ['<中控机IP>'] : [])].map(h => `ssh-copy-id -i ${key}.pub -p ${port} ${U}@${h}`).join('\n')}`]);
  C.push([`0. 前置条件 · 复测：对每台机器（含中控机自己）都不输入口令、不报错才算通过`, `ssh -p ${port} ${S.sshKey.trim() ? `-i ${key} ` : ''}${U}@<IP> 'sudo -n true' && echo OK`]);
  C.push([`0. 前置条件 · 所有机器都分发完、复测通过后，如上面改过 sshd，改回禁用口令登录（每台机器，root 执行）`, "sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config && systemctl restart sshd"]);
  C.push(['1. 解压部署工具包（先从右侧「离线包与工具下载」获取，传到中控机）', `tar xzf ${DP_TOOL}.tar.gz && cd ${DP_TOOL}`]);
  C.push(['2. 把本页生成的配置保存为 deploy.conf', 'chmod 600 deploy.conf']);
  if (S.install === 'offline') C.push(['3. （联网机器上）下载离线包到指定目录，并校验 SHA-256', `./tidb-deploy.sh download-offline --tidb-version ${dlv || 'vX.Y.Z'} --dir ${dpDir(S.offlineDir) || '/opt/tidb-pkg'} --with-toolkit --target-os ${os[0]}`]);
  C.push(['只读环境检查（不修改系统；先检查前置条件，再检查机器环境）', 'sudo ./tidb-deploy.sh check --config deploy.conf']);
  C.push(['预览全过程（不做任何修改）', 'sudo ./tidb-deploy.sh deploy --config deploy.conf --dry-run']);
  C.push(['审阅将经 sudo 在各机器上执行的初始化脚本', './tidb-deploy.sh show-prep --config deploy.conf | less']);
  C.push(['正式部署（root）', 'sudo ./tidb-deploy.sh deploy --config deploy.conf']);
  C.push(['一行写法（命令行参数优先于配置文件）', `sudo ./tidb-deploy.sh deploy --config deploy.conf --user ${S.user} --tidb-version ${v.spec}${S.install === 'offline' ? ` --offline-dir ${dpDir(S.offlineDir) || '/opt/tidb-pkg'}` : ''}`]);
  return C;
}

const dpCopyBtn = t => `<button class="btn sm" data-copy="${esc(t)}">复制</button>`;
function dpOutput() {
  const S = dpState, V = dpValidate(), ok = !V.E.length, tab = S.tab;
  const tabs = [['conf', 'deploy.conf'], ['cmd', '命令'], ['topo', '拓扑预览'], ['check', `校验${V.E.length ? ` · ${V.E.length} 错误` : ''}`], ['dl', '离线包与工具下载']];
  if (typeof dpRun !== 'undefined' && dpRun.on()) tabs.push(['run', '一键部署 <span class="tag info">新</span>']);
  let body = '';
  if (tab === 'conf') body = `<div class="row" style="margin-bottom:8px">${dpCopyBtn(dpConf())}<button class="btn sm primary" data-na="dp-save" ${ok ? '' : 'disabled'}>下载 deploy.conf</button>${ok ? '' : '<span class="small" style="color:var(--bad)">存在错误，请先修正（见「校验」）</span>'}</div>${code(dpConf())}`;
  else if (tab === 'cmd') body = dpCmds().map(([t, c]) => `<div class="small muted" style="margin-top:8px">${t}</div>${code(c)}`).join('');
  else if (tab === 'topo') body = `<div class="small muted">与脚本生成的拓扑一致（单机模式的 block-cache 由脚本按内存计算）。</div>${code(dpTopology())}`;
  else if (tab === 'check') body = `${V.E.map(t => `<div class="notice bad"><span>✖</span><div>${t}</div></div>`).join('')}${V.W.map(t => `<div class="notice warn"><span>⚠</span><div>${t}</div></div>`).join('')}${V.O.map(t => `<div class="notice ok"><span>✔</span><div>${t}</div></div>`).join('')}`;
  else if (tab === 'run' && typeof dpRun !== 'undefined' && dpRun.on()) body = dpRun.render();
  else body = dpDownload();
  return `<div class="tabs" id="dpTabs">${tabs.map(([k, l]) => `<button data-na="dp-tab" data-t="${k}" class="${tab === k ? 'active' : ''}">${l}</button>`).join('')}</div>${body}`;
}

function dpDownload() {
  const S = dpState, os = dpOs(), ex = dpExactVersion(), v = S.dlVersion.trim() || ex, base = 'https://download.pingcap.org';
  const okv = /^v\d+\.\d+\.\d+$/.test(v), gate = okv ? dpGate(v, os[2]) : '';
  const sv = okv ? `tidb-community-server-${v}-linux-${S.arch}.tar.gz` : '', tk = okv ? `tidb-community-toolkit-${v}-linux-${S.arch}.tar.gz` : '';
  const dir = dpDir(S.dlDir) || '/opt/tidb-pkg', dirOk = dir.startsWith('/') && dir !== '/';
  return `<div class="card" style="margin-bottom:12px"><h2>下载离线安装包 <span class="sub">官方 download.pingcap.org</span></h2>
    <div class="grid g2"><div class="field"><label>TiDB 完整版本（离线包需要精确版本号）</label><input type="text" data-ni="dp" data-k="dlVersion" value="${esc(S.dlVersion)}" placeholder="${ex || '如 v7.5.4'}"></div>
      <div class="field"><label>CPU 架构</label><select data-nc="dp" data-k="arch"><option value="amd64" ${S.arch === 'amd64' ? 'selected' : ''}>amd64（x86_64）</option><option value="arm64" ${S.arch === 'arm64' ? 'selected' : ''}>arm64（aarch64）</option></select></div></div>
    <div class="field"><label>保存到（完整目录路径，在运行脚本的机器上）</label><input type="text" data-ni="dp" data-k="dlDir" value="${esc(S.dlDir)}"></div>
    <label class="small" style="display:flex;gap:6px"><input type="checkbox" data-nc="dp" data-k="dlToolkit" ${S.dlToolkit ? 'checked' : ''}> 同时下载 toolkit 包（含 tiup mirror merge 所需的工具）</label>
    ${!okv ? `<div class="notice warn"><span>⚠</span><div>请填写完整版本号（如 v7.5.4）。当前选择的是${esc(dpVersion().label)}，下载需要具体的补丁版本。</div></div>` : ''}
    ${gate ? `<div class="notice bad"><span>✖</span><div>${gate}</div></div>` : ''}
    ${!dirOk ? '<div class="notice bad"><span>✖</span><div>保存目录必须是完整的绝对路径（以 / 开头）</div></div>' : ''}
    ${okv && !gate ? `<div class="small" style="margin-top:10px"><b>命令行下载（推荐）</b>：自动校验 SHA-256、支持断点续传、同时检查与目标系统的版本兼容</div>
      ${code(`./tidb-deploy.sh download-offline --tidb-version ${v} --dir ${dir} --arch ${S.arch}${S.dlToolkit ? ' --with-toolkit' : ''} --target-os ${os[0]}`)}
      <div class="small" style="margin-top:8px"><b>浏览器下载链接</b>（包体较大；下载后请放进上面的目录，文件名不要修改）</div>
      <ul class="small" style="margin:4px 0 0;padding-left:18px;line-height:1.9">
        <li><a href="${base}/${sv}" target="_blank" rel="noopener">${sv}</a> · <a href="${base}/${sv}.sha256" target="_blank" rel="noopener">.sha256</a></li>
        ${S.dlToolkit ? `<li><a href="${base}/${tk}" target="_blank" rel="noopener">${tk}</a> · <a href="${base}/${tk}.sha256" target="_blank" rel="noopener">.sha256</a></li>` : ''}</ul>
      <div class="small muted" style="margin-top:6px">校验：<code>sha256sum -c</code> 或对比 .sha256 文件里的值。目录里放好后，部署时设置 <code>OFFLINE_PKG_DIR=${esc(dir)}</code> 即可，脚本会按版本自动选包。</div>` : ''}</div>
    <div class="card"><h2>部署工具包 <span class="sub">${DP_TOOL}</span></h2>
      <div class="small">包含 tidb-deploy.sh、配置示例、README 与离线逻辑测试。传到中控机后解压即可使用。</div>
      <div class="row" style="margin-top:8px"><a class="btn primary sm" href="deploy/dist/${DP_TOOL}.tar.gz" download>下载 ${DP_TOOL}.tar.gz</a><a class="btn sm" href="deploy/dist/${DP_TOOL}.tar.gz.sha256" target="_blank">SHA-256</a></div>
      <div class="small muted" style="margin-top:6px">传到中控机后先核对 SHA-256：<code>sha256sum ${DP_TOOL}.tar.gz</code></div></div>`;
}

function dpForm() {
  const S = dpState, os = dpOs(), g = os[2], V = dpValidate();
  const opt = (v, t, sel, dis) => `<option value="${v}" ${sel ? 'selected' : ''} ${dis ? 'disabled' : ''}>${t}</option>`;
  const radio = (k, v, t) => `<label class="small" style="display:flex;gap:6px;align-items:center"><input type="radio" name="dp-${k}" data-nc="dp" data-k="${k}" value="${v}" ${S[k] === v ? 'checked' : ''}> ${t}</label>`;
  const inp = (k, label, ph = '', hint = '') => `<div class="field"><label>${label}</label><input type="text" data-ni="dp" data-k="${k}" value="${esc(S[k])}" placeholder="${ph}">${hint ? `<div class="small muted">${hint}</div>` : ''}</div>`;
  return `
  <div class="card"><h2>1 · 目标系统与模式</h2>
    <div class="field"><label>目标操作系统</label><select data-nc="dp" data-k="os">${DP_OS.map(o => opt(o[0], `${o[1]}（${o[4].split(' · ')[0]}）`, S.os === o[0])).join('')}</select></div>
    ${os[3] === 'legacy' ? `<div class="notice warn"><span>⚠</span><div>${os[4]}</div></div>` : ''}
    ${g < 2.28 ? '<div class="notice info"><span>ℹ</span><div>CentOS 7（glibc 2.17）只能安装 ≤ v8.3 的 TiDB，推荐 v7.5 / v8.1 LTS。</div></div>' : ''}
    <div class="field"><label>部署模式</label><div class="row">${radio('mode', 'single', '单机（全部组件同机，仅测试）')}${radio('mode', 'cluster', '多机集群')}</div></div>
    ${inp('cluster', '集群名', 'tidb-test')}</div>
  <div class="card" style="margin-top:12px"><h2>2 · 操作系统用户</h2>
    <div class="field"><label>运行 TiUP 与集群的用户（须由你事先在所有机器上创建，并配好 sudo 免密与 SSH 免密登录）</label>
      <div class="row"><select data-nc="dp" data-k="userSel" style="width:auto">${opt('tidb', 'tidb（官方默认）', S.userSel === 'tidb')}${opt('custom', '自定义…', S.userSel === 'custom')}</select>
        ${S.userSel === 'custom' ? `<input type="text" data-ni="dp" data-k="user" value="${esc(S.user)}" placeholder="如 deploy01" style="flex:1">` : `<span class="mono small">${esc(S.user)}</span>`}</div></div>
    <div class="notice info"><span>ℹ</span><div><b>部署前由你完成（脚本只检查，不代劳）</b>：每台部署机器和中控机上 ① 创建用户 ${esc(S.user)} ② 开通 sudo 免密 ③ ${esc(S.user)} 能用密钥免密 SSH 登录集群所有机器（包括中控机自己）。命令见「命令」页第 0 步。脚本用 root 运行，登录各机器一律 <code>${esc(S.user)}@IP</code> + sudo，不再用 root 登录。</div></div>
    <div class="grid g2">${inp('sshPort', 'SSH 端口')}${inp('sshKey', `${esc(S.user)} 的 SSH 私钥路径（可选，绝对路径）`, `留空 = ~${esc(S.user)}/.ssh/id_rsa`)}</div></div>
  <div class="card" style="margin-top:12px"><h2>3 · TiDB 版本</h2>
    <div class="field"><div style="display:grid;gap:8px">
      ${radio('verMode', 'auto', `自动（按系统选择：当前为 ${dpDefSeries(g)} 系列）`)}
      <div class="row">${radio('verMode', 'series', '指定系列')}<select data-nc="dp" data-k="series" style="width:auto" ${S.verMode === 'series' ? '' : 'disabled'}>${DP_SERIES.map(v => opt(v, `${v} LTS${g < 2.28 && dpGe(v, '8.4.0') ? '（CentOS 7 不支持）' : ''}`, S.series === v, g < 2.28 && dpGe(v, '8.4.0'))).join('')}${opt('custom', '其他系列…', S.series === 'custom')}</select>
        ${S.verMode === 'series' && S.series === 'custom' ? `<input type="text" data-ni="dp" data-k="seriesCustom" value="${esc(S.seriesCustom)}" placeholder="如 v8.2" style="width:100px">` : ''}</div>
      <div class="row">${radio('verMode', 'full', '指定完整版本')}<input type="text" data-ni="dp" data-k="full" value="${esc(S.full)}" placeholder="如 v7.5.4" style="width:140px" ${S.verMode === 'full' ? '' : 'disabled'}></div></div></div>
    <div class="small muted">当前选择：<b>${esc(dpVersion().label)}</b>${S.verMode === 'series' || S.verMode === 'auto' ? '。在线安装取该系列最新补丁版本；离线安装取离线包目录里该系列的最高版本。' : ''}</div></div>
  <div class="card" style="margin-top:12px"><h2>4 · 安装方式</h2>
    <div class="field"><div class="row">${radio('install', 'online', '在线安装')}${radio('install', 'offline', '离线安装（使用离线包）')}</div></div>
    ${S.install === 'offline' ? `${inp('offlineDir', '离线包所在的完整目录路径', '/opt/tidb-pkg', '目录中放 tidb-community-server-<版本>-linux-' + S.arch + '.tar.gz（可选 tidb-community-toolkit-…）；脚本按所选版本自动查找，无需写文件名')}
      ${inp('rpmDir', '依赖 rpm 所在目录（可选）', '/opt/tidb-pkg/rpm', '无外网时放 numactl、chrony、libaio 等 rpm')}
      <div class="row"><button class="btn sm" data-na="dp-goto-dl">没有离线包？去下载 →</button></div>` : '<div class="small muted">从 tiup-mirrors.pingcap.com 在线下载。内网无外网时请选择离线安装。</div>'}</div>
  <div class="card" style="margin-top:12px"><h2>5 · 主机与目录</h2>
    ${S.mode === 'single' ? inp('singleHost', '单机 IP（留空 = 本机主 IP）', '10.0.0.5') : `${inp('pd', 'PD 主机（空格或逗号分隔，建议 3 台）')}${inp('tidb', 'TiDB 主机（建议 ≥ 2 台）')}${inp('tikv', 'TiKV 主机（建议 ≥ 3 台）')}${inp('tiflash', 'TiFlash 主机（可选，需 AVX2）')}${inp('monitor', '监控主机（空 = 第一台 PD）')}${inp('zone', '拓扑标签（可选）', '10.0.1.7=az1,10.0.1.8=az2,10.0.1.9=az3', 'IP=zone，启用后 PD 按 zone,host 打散副本')}`}
    <div class="grid g2">${inp('deployDir', '部署目录')}${inp('dataDir', '数据目录', '', '建议独立数据盘（xfs，或 ext4 且含 nodelalloc）')}</div>
    <div class="grid g2"><div class="field"><label>规格</label><select data-nc="dp" data-k="spec">${opt('dev', 'dev 开发测试（低配仅警告）', S.spec === 'dev')}${opt('prod', 'prod 生产（不达标则中止）', S.spec === 'prod')}</select></div>
      <div class="field"><label>防火墙</label><select data-nc="dp" data-k="firewall">${opt('open', '只放行 TiDB 所需端口', S.firewall === 'open')}${opt('disable', '关闭 firewalld', S.firewall === 'disable')}${opt('keep', '保持不变', S.firewall === 'keep')}</select></div></div>
    ${os[3] === 'legacy' ? `<div class="grid g2"><div class="field"><label>yum 源失效时</label><select data-nc="dp" data-k="repoFix">${opt('skip', '不处理（默认）', S.repoFix === 'skip')}${opt('auto', '自动切换 vault 镜像', S.repoFix === 'auto')}</select></div>
      <div class="field"><label>镜像</label><select data-nc="dp" data-k="repoMirror">${opt('aliyun', '阿里云 vault', S.repoMirror === 'aliyun')}${opt('vault', 'vault.centos.org', S.repoMirror === 'vault')}</select></div></div>` : ''}
    <label class="small" style="display:flex;gap:6px"><input type="checkbox" data-nc="dp" data-k="withMonitor" ${S.withMonitor ? 'checked' : ''}> 部署监控（Prometheus / Grafana / Alertmanager）</label>
    <label class="small" style="display:flex;gap:6px;margin-top:6px${S.spec === 'dev' ? '' : ';opacity:.55'}" title="${S.spec === 'dev' ? '' : '生产规格必须先修复检查失败项'}"><input type="checkbox" data-nc="dp" data-k="allowCheckFail" ${S.allowCheckFail && S.spec === 'dev' ? 'checked' : ''} ${S.spec === 'dev' ? '' : 'disabled'}> <span>忽略 tiup check 的失败项继续部署（<b>仅测试环境</b>，会忽略<b>全部</b>失败项，不只是 nodelalloc；仅 dev 规格可选）</span></label></div>`;
}

pages.deploy = {
  title: '安装部署',
  render() {
    return `<div class="notice info" style="margin-bottom:14px"><span>🚀</span><div>选择系统、操作系统用户、TiDB 版本和安装方式，页面会实时校验并生成 <b>deploy.conf</b> 与命令；离线安装可指定离线包所在的完整目录，也可以直接下载离线包。实际部署由中控机上的 <code>tidb-deploy.sh</code> 执行。</div></div>
    ${typeof dpRun !== 'undefined' ? dpRun.bar() : ''}
    <div class="grid g-main" id="dpGrid" style="grid-template-columns:minmax(0,1fr) minmax(0,1.1fr)"><div id="dpForm">${dpForm()}</div><div class="card" id="dpOut" style="align-self:start">${dpOutput()}</div></div>`;
  },
  init() { if (typeof dpRun !== 'undefined') dpRun.init(); },
};
const dpRenderOut = () => { const o = $('#dpOut'); if (o) o.innerHTML = dpOutput(); if (typeof dpRun !== 'undefined') dpRun.after(); };
const dpRenderAll = () => { const f = $('#dpForm'); if (f) f.innerHTML = dpForm(); dpRenderOut(); };

document.addEventListener('input', e => {
  const el = e.target.closest('[data-ni="dp"]'); if (!el) return; dpState[el.dataset.k] = el.value;
  if (el.dataset.k === 'user') dpState.userSel = 'custom';
  dpRenderOut();
});
document.addEventListener('change', e => {
  const el = e.target.closest('[data-nc="dp"]'); if (!el) return; const k = el.dataset.k, S = dpState;
  const v = el.type === 'checkbox' ? el.checked : el.value;
  if (k === 'userSel') { S.userSel = v; if (v !== 'custom') S.user = v; }
  else S[k] = v;
  if (k === 'spec' && v !== 'dev') S.allowCheckFail = false;   // 生产规格不允许忽略检查失败项
  if (k === 'os') {                        // 换系统：CentOS 7 不支持 ≥ v8.4，自动调整
    const g = dpOs()[2];
    if (g < 2.28 && S.verMode === 'series' && dpGe(S.series === 'custom' ? S.seriesCustom : S.series, '8.4.0')) { S.series = 'v7.5'; toast('CentOS 7 不支持 ≥ v8.4，系列已切换为 v7.5'); }
    if (g < 2.28 && S.verMode === 'full' && S.full && dpGe(S.full, '8.4.0')) toast('当前完整版本在 CentOS 7 上不受支持，请修改');
  }
  if (k === 'arch' || k === 'dlToolkit') return dpRenderOut();
  dpRenderAll();
});
document.addEventListener('click', e => {
  const el = e.target.closest('[data-na]'); if (!el || el.disabled) return; const d = el.dataset;
  if (d.na === 'dp-tab') { dpState.tab = d.t; dpRenderOut(); }
  else if (d.na === 'dp-goto-dl') { dpState.tab = 'dl'; dpRenderOut(); }
  else if (d.na === 'dp-save') {
    const blob = new Blob([dpConf()], {type: 'text/plain;charset=utf-8'}), a = document.createElement('a');
    a.href = URL.createObjectURL(blob); a.download = 'deploy.conf'; document.body.appendChild(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    toast('已下载 deploy.conf，请传到中控机并执行 chmod 600 deploy.conf');
  }
});
