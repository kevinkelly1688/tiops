// @author: kevinkelly
// @license: Apache License 2.0
// @file: help.js
// @time: 2026/10/28

/* 使用帮助：页面「使用帮助」（#help、#help/<主题>）+ 顶栏「? 帮助」按钮。
   内容来自 help/<id>.md（唯一来源，编写规范见 help/README.md），在浏览器里用下面的小型 Markdown 渲染器显示。
   - 顶栏「? 帮助」：打开当前页面同名的帮助；不在帮助页时记下来源页，帮助页里可「返回」。
   - 左侧主题列表 + 搜索（载入全部帮助后按行匹配）；右侧「本页目录」。
   - 事件全部用 data-* + 文档级委托（CSP 禁止内联脚本 / 事件）。
   - 无后端（file:// 或纯静态打开）取不到 help/*.md 时给出提示，不影响其他页面。
   依赖 app.js（pages / esc / code / go / $ / $$）。 */
(function () {
  'use strict';
  const TOPICS = [
    {g: '入门', id: 'quickstart', t: '快速入门'},
    {g: '概览', id: 'overview', t: '集群总览'},
    {g: '概览', id: 'arch', t: '集群架构'},
    {g: '诊断与优化', id: 'diag', t: '智能诊断'},
    {g: '诊断与优化', id: 'sql', t: 'SQL 优化'},
    {g: '诊断与优化', id: 'hotspot', t: '热点与调度'},
    {g: '诊断与优化', id: 'trouble', t: '故障排查'},
    {g: '运维', id: 'nodes', t: '节点管理'},
    {g: '运维', id: 'schedule', t: '变更排期'},
    {g: '运维', id: 'deploy', t: '安装部署'},
    {g: '运维', id: 'shell', t: '中控机命令'},
    {g: '运维', id: 'ops', t: '日常运维'},
    {g: '运维', id: 'settings', t: '环境与设置'},
    {g: '运维', id: 'security', t: '权限与审计'},
    {g: '平台配置', id: 'users', t: '用户管理'},
    {g: '平台配置', id: 'roles', t: '角色权限'},
    {g: '平台配置', id: 'clusters', t: '集群接入'},
    {g: '平台配置', id: 'watermark', t: '水印设置'},
    {g: '其他', id: 'faq', t: '常见问题与提示'}
  ];
  const IDS = TOPICS.map(x => x.id);
  const H = {cur: 'quickstart', back: '', cache: {}, err: {}, q: '', all: false, timer: 0};

  /* ---------------------------------------------------------------- Markdown ---- */
  function inline(s) {
    // 先把行内代码换成占位符（粗体 / 链接可以包住代码），转义后再还原
    const codes = [];
    let h = s.replace(/`([^`]+)`/g, (m, c) => { codes.push(`<code>${esc(c)}</code>`); return '' + (codes.length - 1) + ''; });
    h = esc(h);
    h = h.replace(/\*\*(.+?)\*\*/g, '<b>$1</b>');
    h = h.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, t, u) => {
      u = u.replace(/&amp;/g, '&');
      let x;
      if ((x = /^go:([a-z]+)$/.exec(u)) && IDS.concat(Object.keys(pages)).includes(x[1])) return `<a data-go="${x[1]}">${t}</a>`;
      if ((x = /^help:([a-z]+)$/.exec(u)) && IDS.includes(x[1])) return `<a data-help="${x[1]}">${t}</a>`;
      if (/^https?:\/\//.test(u)) return `<a class="ref" href="${esc(u)}" target="_blank" rel="noopener">${t}</a>`;
      return t;   // 不认识的链接按纯文本处理
    });
    return h.replace(/(\d+)/g, (m, i) => codes[+i]);
  }
  const cells = line => { let s = line.trim(); if (s[0] === '|') s = s.slice(1); if (s.endsWith('|')) s = s.slice(0, -1); return s.split('|').map(c => c.trim()); };
  const isSep = line => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(line) && line.includes('-');

  /* 渲染：返回 {html, toc:[{id,text}], title} */
  function md(src) {
    const lines = src.replace(/\r\n?/g, '\n').split('\n');
    const out = [], toc = []; let title = '', i = 0, hn = 0;
    const para = [];
    const flush = () => { if (para.length) { out.push(`<p>${inline(para.join(' '))}</p>`); para.length = 0; } };
    while (i < lines.length) {
      const ln = lines[i];
      let m;
      if (/^```/.test(ln)) {                                  // 代码块
        flush(); const buf = []; i++;
        while (i < lines.length && !/^```/.test(lines[i])) buf.push(lines[i++]);
        i++; out.push(code(buf.join('\n'))); continue;
      }
      if (!ln.trim()) { flush(); i++; continue; }
      if ((m = /^(#{1,4})\s+(.*)$/.exec(ln))) {                // 标题
        flush(); const lv = m[1].length, tx = m[2].trim();
        if (lv === 1) { if (!title) title = tx; out.push(`<h1 class="hp-h1">${inline(tx)}</h1>`); }
        else { const id = 'hp-' + (++hn); if (lv === 2) toc.push({id, text: tx.replace(/[`*]/g, '')}); out.push(`<h${lv + 1} id="${id}" class="hp-h${lv}">${inline(tx)}</h${lv + 1}>`); }
        i++; continue;
      }
      if (/^\s*\|/.test(ln) && i + 1 < lines.length && isSep(lines[i + 1])) {   // 表格
        flush(); const head = cells(ln); i += 2; const rows = [];
        while (i < lines.length && /^\s*\|/.test(lines[i])) rows.push(cells(lines[i++]));
        out.push(`<div class="tbl-wrap"><table class="hp-tbl"><thead><tr>${head.map(c => `<th>${inline(c)}</th>`).join('')}</tr></thead><tbody>${rows.map(r => `<tr>${head.map((_, k) => `<td>${inline(r[k] || '')}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`);
        continue;
      }
      if (/^>/.test(ln)) {                                     // 引用 / 提示框
        flush(); const buf = [];
        while (i < lines.length && /^>/.test(lines[i])) buf.push(lines[i++].replace(/^>\s?/, ''));
        const tx = buf.join(' ').trim();
        const cls = /^(⚠|❗|🚫)/.test(tx) ? 'warn' : /^(💡|ℹ)/.test(tx) ? 'info' : '';
        out.push(`<div class="notice hp-note ${cls}"><div>${inline(tx)}</div></div>`); continue;
      }
      if ((m = /^(\s*)([-*]|\d+[.)])\s+(.*)$/.exec(ln))) {     // 列表（按缩进嵌套）
        flush(); const stack = []; let html = '';
        const close = n => { while (stack.length > n) { html += `</li></${stack.pop().tag}>`; } };
        while (i < lines.length && (m = /^(\s*)([-*]|\d+[.)])\s+(.*)$/.exec(lines[i]))) {
          const ind = m[1].replace(/\t/g, '  ').length, tag = /\d/.test(m[2]) ? 'ol' : 'ul';
          while (stack.length && ind < stack[stack.length - 1].ind) { html += `</li></${stack.pop().tag}>`; }
          if (!stack.length || ind > stack[stack.length - 1].ind) { html += tag === 'ol' ? `<ol start="${parseInt(m[2], 10) || 1}">` : '<ul>'; stack.push({ind, tag}); } else html += '</li>';   // 有序列表按第一项的编号开始（被表格隔开后序号接着走）
          html += `<li>${inline(m[3])}`; i++;
          // 续行（缩进的文本）并入当前条目
          while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !/^(\s*)([-*]|\d+[.)])\s+/.test(lines[i])) html += ' ' + inline(lines[i++].trim());
        }
        close(0); out.push(html); continue;
      }
      if (/^\s*(-{3,}|\*{3,})\s*$/.test(ln)) { flush(); out.push('<hr class="hr">'); i++; continue; }
      para.push(ln.trim()); i++;
    }
    flush();
    return {html: out.join('\n'), toc, title};
  }

  /* ---------------------------------------------------------------- 加载 ---- */
  function fetchTopic(id) {
    if (H.cache[id] !== undefined) return Promise.resolve(H.cache[id]);
    return fetch('help/' + id + '.md', {cache: 'no-cache'}).then(r => {
      if (!r.ok) throw new Error(r.status === 404 ? '这一篇帮助尚未提供' : 'HTTP ' + r.status);
      return r.text();
    }).then(t => { H.cache[id] = t; delete H.err[id]; return t; }, e => { H.err[id] = e.message || String(e); throw e; });
  }
  const loadAll = () => Promise.all(IDS.map(id => fetchTopic(id).catch(() => '')));

  /* ---------------------------------------------------------------- 页面 ---- */
  const curHash = () => (location.hash.slice(1) || 'overview');
  function topicFromHash() { const m = /^help\/([a-z]+)$/.exec(curHash()); return m && IDS.includes(m[1]) ? m[1] : null; }
  function sideHtml() {
    let g = '', h = '';
    TOPICS.forEach(x => {
      if (x.g !== g) { g = x.g; h += `<div class="hp-grp">${esc(g)}</div>`; }
      h += `<a class="hp-it${!H.q && x.id === H.cur ? ' on' : ''}" data-help="${x.id}">${esc(x.t)}</a>`;
    });
    return h;
  }
  function body() {
    const box = $('#hpBody'); if (!box) return;
    if (H.q) return searchView(box);
    const id = H.cur;
    $('#hpSide').innerHTML = sideHtml();
    const done = () => {
      const r = md(H.cache[id]);
      box.innerHTML = r.html;
      $('#hpToc').innerHTML = r.toc.length ? `<div class="hp-grp">本页目录</div>${r.toc.map(t => `<a data-hscroll="${t.id}">${esc(t.text)}</a>`).join('')}` : '';
      const idx = IDS.indexOf(id), nav = [];
      if (idx > 0) nav.push(`<a data-help="${IDS[idx - 1]}">← ${esc(TOPICS[idx - 1].t)}</a>`); else nav.push('<span></span>');
      if (idx < IDS.length - 1) nav.push(`<a data-help="${IDS[idx + 1]}">${esc(TOPICS[idx + 1].t)} →</a>`);
      box.insertAdjacentHTML('beforeend', `<div class="hp-pn">${nav.join('')}</div>`);
    };
    if (H.cache[id] !== undefined) { done(); return; }
    box.innerHTML = '<div class="muted">加载中…</div>';
    fetchTopic(id).then(() => { if (H.cur === id && !H.q) done(); }, e => {
      if (H.cur !== id) return;
      box.innerHTML = `<div class="notice warn"><span>⚠</span><div><b>无法加载帮助：${esc(e.message || String(e))}</b><br>帮助文件随平台服务提供（<code>help/${id}.md</code>）。请通过平台服务地址访问；直接双击打开 index.html 时看不到帮助。源文件也可在发布包的 <code>web/help/</code> 目录里直接阅读。</div></div>`;
      $('#hpToc').innerHTML = '';
    });
  }
  function searchView(box) {
    const q = H.q.trim().toLowerCase();
    $('#hpSide').innerHTML = sideHtml(); $('#hpToc').innerHTML = '';
    box.innerHTML = '<div class="muted">搜索中…</div>';
    const run = () => {
      if (H.q.trim().toLowerCase() !== q) return;
      const hits = [];
      TOPICS.forEach(tp => {
        const src = H.cache[tp.id]; if (!src) return;
        let sec = '';
        src.split('\n').forEach(l => {
          const h = /^(#{1,4})\s+(.*)$/.exec(l); if (h) { sec = h[2]; }
          if (l.toLowerCase().includes(q) && !/^```/.test(l)) hits.push({tp, sec, l});
        });
      });
      if (!hits.length) { box.innerHTML = `<h1 class="hp-h1">搜索「${esc(H.q)}」</h1><div class="muted">没有找到匹配内容。换个关键词试试，或从左侧主题浏览。</div>`; return; }
      const shown = hits.slice(0, 60);
      const re = new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'ig');
      box.innerHTML = `<h1 class="hp-h1">搜索「${esc(H.q)}」</h1><div class="muted small" style="margin-bottom:8px">共 ${hits.length} 处匹配${hits.length > shown.length ? '，仅显示前 60 处' : ''}</div>` +
        shown.map(h => {
          const t = h.l.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1').replace(/[`*]/g, '').replace(/\s*\|\s*/g, ' · ').replace(/^[\s>#·-]+/, '').replace(/[\s·]+$/, '').slice(0, 160);
          return `<div class="hp-hit"><a data-help="${h.tp.id}"><b>${esc(h.tp.t)}</b>${h.sec ? ' › ' + esc(h.sec) : ''}</a><div class="small muted">${esc(t).replace(re, m => `<mark>${m}</mark>`)}</div></div>`;
        }).join('');
    };
    if (H.all) run(); else loadAll().then(() => { H.all = true; run(); });
  }
  function open(id) { location.hash = id ? 'help/' + id : 'help'; }
  pages.help = {
    title: '使用帮助',
    render() {
      const t = topicFromHash(); if (t) H.cur = t;
      return `<div class="hp">
        <nav class="hp-side"><input id="hpQ" class="hp-q" type="search" placeholder="搜索帮助…" aria-label="搜索帮助" value="${esc(H.q)}"><div id="hpSide"></div>
          ${H.back ? `<div class="hp-grp">返回</div><a class="hp-it" data-hback="1">← 回到刚才的页面</a>` : ''}</nav>
        <article class="card hp-main" id="hpBody"></article>
        <aside class="hp-toc" id="hpToc"></aside></div>`;
    },
    init() { body(); }
  };

  /* ---------------------------------------------------------------- 事件 ---- */
  document.addEventListener('click', e => {
    const hb = e.target.closest('[data-help]');
    if (hb) { e.preventDefault(); H.q = ''; const id = hb.dataset.help; if (!IDS.includes(id)) return; H.cur = id; if (curHash() === 'help/' + id) { const q = $('#hpQ'); if (q) q.value = ''; body(); window.scrollTo(0, 0); } else open(id); return; }
    const sc = e.target.closest('[data-hscroll]');
    if (sc) { e.preventDefault(); const el = document.getElementById(sc.dataset.hscroll); if (el) el.scrollIntoView({behavior: 'smooth', block: 'start'}); return; }
    if (e.target.closest('[data-hback]')) { e.preventDefault(); location.hash = H.back || 'overview'; return; }
    if (e.target.closest('#helpBtn')) {
      e.preventDefault();
      const cur = curHash();
      if (cur.split('/')[0] === 'help') { H.q = ''; H.cur = 'quickstart'; open('quickstart'); return; }
      H.back = cur; H.q = ''; H.cur = IDS.includes(cur) ? cur : 'quickstart'; open(H.cur);
    }
  });
  document.addEventListener('input', e => {
    if (e.target.id !== 'hpQ') return;
    clearTimeout(H.timer);
    const v = e.target.value;
    H.timer = setTimeout(() => { H.q = v; body(); }, 200);
  });
  /* 其他页面的「查看帮助」入口：helpOpen('nodes') */
  window.helpOpen = id => { H.back = curHash().split('/')[0] === 'help' ? H.back : curHash(); H.q = ''; H.cur = IDS.includes(id) ? id : 'quickstart'; open(H.cur); };
  window.HELP_TOPICS = TOPICS;
})();
