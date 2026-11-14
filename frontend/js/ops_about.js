// @author: kevinkelly
// @license: Apache License 2.0
// @file: ops_about.js
// @time: 2026/10/28

/* 运维模式：环境与设置页末尾的「关于与升级」卡片 + 侧栏标题下的版本号。
   - 卡片只在 OP.on === true 时出现（无后端 / 非运维模式 pages.settings 原样不变）。
     内容：版本、提交、构建时间、Go 版本、系统 / 架构、数据 schema、已运行时长、安装布局（当前 release、已安装的 releases）、
     最近升级历史，以及升级与回滚操作说明。数据来自 GET /api/ops/version（登录即可，只读）。
   - 侧栏版本号（标题下 + 左下角 #sideVer）：所有模式从 /api/health 读取 version（发布包由打包时的 ldflags 注入）；没有后端（静态打开 / 原型）时不显示。
   做法：包装 ops_notify.js 已包装过的 pages.settings（保留其 render / init），在页面末尾追加卡片。
   事件用 data-na（前缀 oab-）+ 文档级委托（CSP 禁止内联事件与内联脚本）。
   依赖 ops.js（OP / opApi / opEsc）、ops_pages.js（OP_HOOKS）。 */
(function () {
  'use strict';
  const E = opEsc;
  const p2 = n => String(n).padStart(2, '0');
  const fDT = t => { if (!t) return '—'; const d = new Date(t); if (isNaN(d)) return E(String(t)); return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`; };
  const hashIs = h => (location.hash.slice(1) || 'overview') === h;
  const tg = (t, c = '') => `<span class="tag ${c}">${E(t)}</span>`;

  const AB = {d: null, err: '', loading: false, at: 0};

  /* ---------------------------------------------------------------- 侧栏版本号 ---- */
  async function sideVersion() {
    try {
      const r = await fetch('/api/health', {cache: 'no-store'});
      if (!r.ok) return;
      const h = await r.json();
      if (!h || !h.version) return;
      const tip = [h.commit ? '提交 ' + h.commit : '', h.buildTime ? '构建 ' + h.buildTime : ''].filter(Boolean).join(' · ');
      const foot = document.getElementById('sideVer');   // 侧栏左下角（index.html）
      if (foot) { foot.textContent = 'v' + h.version; if (tip) foot.title = tip; foot.hidden = false; }
      const host = document.querySelector('.logo > div'); if (!host || host.querySelector('.oab-ver')) return;
      const s = document.createElement('small');
      s.className = 'oab-ver';
      s.textContent = 'v' + h.version;
      if (tip) s.title = tip;
      host.appendChild(s);
    } catch (e) { /* 没有后端：不显示版本号 */ }
  }

  /* ---------------------------------------------------------------- 数据 ---- */
  async function load(force) {
    if (!OP.on || !OP.me || AB.loading) return;
    if (!force && AB.d && Date.now() - AB.at < 20000) return;
    AB.loading = true;
    const r = await opApi('GET', '/api/ops/version');
    AB.loading = false;
    if (r.code === 200) { AB.d = r.data; AB.err = ''; AB.at = Date.now(); } else AB.err = (r.data && r.data.error) || ('读取失败（' + r.code + '）');
    paint();
  }

  function paint() {
    const el = $('#oabBox'); if (!el || !hashIs('settings')) return;
    const h = html();
    if (el._h === h) return;
    el._h = h; el.innerHTML = h;
  }

  /* ---------------------------------------------------------------- 渲染 ---- */
  function fmtUp(sec) {
    sec = Math.max(0, Math.floor(+sec || 0));
    const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    if (d) return `${d} 天 ${h} 小时 ${m} 分`;
    if (h) return `${h} 小时 ${m} 分`;
    if (m) return `${m} 分 ${sec % 60} 秒`;
    return `${sec} 秒`;
  }

  const ACTION = {install: '安装', upgrade: '升级', rollback: '回滚', migrate: '数据迁移'};
  // 历史记录字段名兼容几种写法
  const pick = (h, ...ks) => { for (const k of ks) if (h[k] !== undefined && h[k] !== null && h[k] !== '') return h[k]; return ''; };

  function resultTag(h) {
    let r = pick(h, 'result', 'status', 'outcome');
    if (r === '' && typeof h.ok === 'boolean') r = h.ok ? 'success' : 'failed';
    if (r === '' && typeof h.success === 'boolean') r = h.success ? 'success' : 'failed';
    const s = String(r).toLowerCase();
    if (!s) return '<span class="muted">—</span>';
    if (['success', 'succeeded', 'ok', '成功'].includes(s)) return tg('✔ 成功', 'ok');
    if (['rolledback', 'rolled_back', 'rolled-back'].includes(s)) return tg('↺ 已回滚', 'warn');
    if (['dry-run', 'dryrun', 'dry_run'].includes(s)) return tg('预演', 'info');
    if (['failed', 'fail', 'error', '失败'].includes(s)) return tg('✖ 失败', 'bad');
    return tg(String(r));
  }

  function historyRows(list) {
    if (!list.length) return '<tr><td colspan="7" class="empty">还没有升级记录（首次安装或尚未用发布包升级过）</td></tr>';
    return list.map(h => {
      const act = String(pick(h, 'action', 'kind', 'type'));
      const rb = pick(h, 'rolledBack', 'rollback', 'rolled_back');
      const rbText = rb === true || rb === 'true' ? tg('已回滚', 'warn') : (rb === false || rb === 'false' ? '<span class="muted">否</span>' : '<span class="muted">—</span>');
      const bk = String(pick(h, 'backup', 'backupFile', 'backupPath'));
      const from = String(pick(h, 'from', 'fromVersion')), to = String(pick(h, 'to', 'toVersion'));
      const bad = /fail|error|失败/i.test(String(pick(h, 'result', 'status', 'outcome'))) || h.ok === false || h.success === false;
      return `<tr class="${bad ? 'flag' : ''}"><td class="small" style="white-space:nowrap">${fDT(pick(h, 'time', 'at', 'ts', 'startedAt'))}</td>
        <td>${E(ACTION[act] || act || '—')}</td>
        <td class="mono small">${E(from || '—')} → ${E(to || '—')}</td>
        <td title="${E(String(pick(h, 'error', 'message')))}">${resultTag(h)}</td><td>${rbText}</td>
        <td class="mono small" style="word-break:break-all;max-width:240px" title="${E(bk)}">${bk ? E(bk) : '—'}</td>
        <td class="small">${E(String(pick(h, 'operator', 'actor', 'user', 'by')) || '—')}</td></tr>`;
    }).join('');
  }

  const cmd = lines => `<pre class="mono small" style="margin:4px 0 0;padding:8px 10px;background:var(--panel2);border:1px solid var(--border);border-radius:6px;overflow:auto;white-space:pre">${E(lines.join('\n'))}</pre>`;

  function guideHtml() {
    return `<h3 style="font-size:14px;margin:0 0 8px">升级与回滚操作说明</h3>
      <ol style="margin:0;padding-left:20px;display:grid;gap:10px;line-height:1.7">
        <li>把新版本发布包与校验文件放到中控机（如 /tmp）：<span class="mono small">tidbops-&lt;版本&gt;-linux-&lt;架构&gt;.tar.gz</span> 与 <span class="mono small">.tar.gz.sha256</span>。</li>
        <li>校验完整性（必须显示 OK）：${cmd(['sha256sum -c tidbops-<版本>-linux-<架构>.tar.gz.sha256'])}</li>
        <li>解压并进入目录：${cmd(['tar xzf tidbops-<版本>-linux-<架构>.tar.gz', 'cd tidbops-<版本>-linux-<架构>'])}</li>
        <li>先预演（只检查、不改动；可用普通账号运行）：${cmd(['./bin/tidbops-server admin upgrade --dry-run'])}</li>
        <li>正式升级：${cmd(['sudo ./upgrade.sh'])}
          <div class="small muted" style="margin-top:4px">流程：校验清单 → 备份数据目录 → 安装到 releases/&lt;新版本&gt; → 数据迁移 → 切换 current → 重启 → 健康检查。<b>任一步失败会自动回滚</b>到升级前的版本与数据。</div></li>
        <li>需要手动回滚到上一版本时：${cmd(['sudo /opt/tidbops/current/bin/tidbops-server admin rollback'])}</li>
      </ol>
      <div class="small muted" style="margin-top:10px;display:grid;gap:3px">
        <div>升级前的数据备份在 <span class="mono">/var/backups/tidbops/</span>（<span class="mono">&lt;时间&gt;-&lt;旧版本&gt;.tar.gz</span>）；旧版本的程序保留在 <span class="mono">/opt/tidbops/releases/</span>。</div>
        <div>首次部署使用 <span class="mono">sudo ./install.sh --local-tiup</span>（装在 tiup 中控机上，登录后一键导入本机集群）、<span class="mono">--standalone</span>（任意机器，只读接入）或 <span class="mono">--cluster &lt;集群名&gt;</span>（同机部署，绑定一个集群）。详细说明见发布包内 docs/运维部署手册.md。</div></div>
      <div class="notice warn" style="margin-top:10px"><span>⚠</span><div>升级期间平台约 10–30 秒不可用；<b>执行中的变更会阻止升级</b>，请等变更结束或中止后再升级。升级前确认没有进行中的变更与维护。</div></div>`;
  }

  function html() {
    const head = `<h2>关于与升级 <span class="sub">版本 · 安装布局 · 升级历史</span></h2>`;
    const d = AB.d;
    if (!d) {
      return `<div class="card">${head}${AB.err ? `<div class="notice bad"><span>✖</span><div>${E(AB.err)}</div></div>` : '<div class="empty">正在读取…</div>'}${guideHtml()}</div>`;
    }
    const lay = d.installLayout || {}, rel = lay.releases || [], hist = lay.history || [];
    const kv = (k, v, mono) => `<div style="display:flex;gap:10px;padding:3px 0"><span class="muted" style="min-width:84px">${k}</span><span class="${mono ? 'mono ' : ''}" style="word-break:break-all">${v}</span></div>`;
    const schemaNote = d.dataSchema && d.dataSchema !== d.schema ? ` ${tg(d.dataSchema < d.schema ? '待迁移' : '数据较新', 'warn')}` : '';
    const relHtml = lay.installed
      ? (rel.length ? rel.map(v => v === lay.current ? tg(v + '（当前）', 'ok') : tg(v)).join(' ') : '<span class="muted">—</span>')
      : '<span class="muted">未按发布包布局安装（开发 / 手工部署）</span>';
    return `<div class="card">${head}
      <div class="grid g2" style="gap:0 32px">
        <div>${kv('版本', E(d.version || '—'), true)}${kv('提交', E(d.commit || '—'), true)}${kv('构建时间', E(d.buildTime ? fDT(d.buildTime) : '—'))}${kv('Go 版本', E(d.goVersion || '—'), true)}</div>
        <div>${kv('系统 / 架构', E((d.os || '—') + ' / ' + (d.arch || '—')), true)}${kv('数据 schema', `${E(d.dataSchema)}（程序支持 ${E(d.schema)}）${schemaNote}`)}${kv('已运行', E(fmtUp(d.uptimeSec)) + ` <span class="muted small">（自 ${fDT(d.startedAt)}）</span>`)}</div>
      </div>
      <div class="hr"></div>
      <div style="display:grid;gap:4px">
        ${kv('当前 release', lay.installed ? E(lay.current || '—') : '—', true)}
        ${kv('已安装版本', relHtml)}
      </div>
      <h3 style="font-size:14px;margin:14px 0 6px">最近升级历史 <span class="small muted">最近 ${hist.length} 条</span></h3>
      <div class="tbl-wrap"><table><thead><tr><th>时间</th><th>动作</th><th>版本</th><th>结果</th><th>回滚</th><th>备份</th><th>操作人</th></tr></thead><tbody>${historyRows(hist)}</tbody></table></div>
      <div class="hr"></div>
      ${guideHtml()}</div>`;
  }

  /* ---------------------------------------------------------------- 包装设置页 ---- */
  const _st = pages.settings;
  pages.settings = {
    ..._st,
    render() {
      const h = _st.render.call(this);
      if (!OP.on) return h;
      return h + `<div id="oabBox" style="margin-top:16px">${html()}</div>`;
    },
    init() {
      _st.init.call(this);
      if (!OP.on) return;
      load(true);
    },
  };

  OP_HOOKS.push((h, ctx) => {
    if (!OP.on || h !== 'settings' || ctx.open) return;
    load(false);
  });

  sideVersion();
})();
