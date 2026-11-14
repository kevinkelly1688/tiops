// @author: kevinkelly
// @license: Apache License 2.0
// @file: login.js
// @time: 2026/10/28

/* 运维模式登录页（替换 ops.js 里原来的简易登录卡片）。
   - 左侧品牌区（平台能力一览）+ 右侧登录表单；窄屏只显示表单。
   - 表单：用户名（可「记住账号」，只存用户名，口令永不落盘）、口令（显示 / 隐藏、大写锁定提示）、
     登录失败 / 锁定提示、提交中状态、回车提交。
   - 「忘记口令」：说明找回方式（本平台不做邮件自助找回：本地账号由管理员重置，管理员用中控机命令行重置）。
   - 演示模式（后端以 --dev-seed 启动，/api/health 的 ops.demo 为 true）：显示演示账号快捷填充；正式环境不出现。
   - 右上角主题切换（浅色 / 深色 / 护眼；与顶栏共用 app.js 的 setTheme 与 localStorage 'theme'）。
   - 会话过期（401）重新登录时显示「登录已过期」提示。
   登录只认用户名 + 口令，不含动态验证码；LDAP / SSO / 企业微信 / 钉钉 / 飞书登录未接入，页面不显示这些入口。
   事件全部在这里用 addEventListener 绑定（CSP 禁止内联脚本 / 事件）。依赖 ops.js（OP / opApi / opEsc / opApplyMe / opAfterLogin）。 */
(function () {
  'use strict';
  const E = opEsc;
  const LS = {
    get(k) { try { return localStorage.getItem(k) || ''; } catch (e) { return ''; } },
    set(k, v) { try { if (v) localStorage.setItem(k, v); else localStorage.removeItem(k); } catch (e) { /* 浏览器禁用存储：忽略 */ } }
  };
  const DEMO = [
    ['admin', '平台管理员', 'admin'], ['admin2', '平台管理员（复核）', 'admin'], ['dba', 'DBA', 'dba'], ['sre', '值班 SRE', 'sre'],
    ['approver', '审批人', 'approver'], ['approver2', '审批人（二级）', 'approver'], ['viewer', '访客', 'viewer'], ['auditor', '审计员', 'auditor'], ['dev', '业务开发', 'developer']
  ];
  const ENV = {prod: ['bad', '生产环境'], staging: ['warn', '预发环境'], test: ['ok', '测试环境']};
  const FEATS = [
    ['✚ 智能诊断', '规则诊断、慢 SQL 与执行计划分析、热点定位'],
    ['▤ 变更管理', '重启、扩缩容、配置变更：先预检，再审批、可定时，全程可追踪'],
    ['⛨ 权限与审计', '角色权限、双人复核、哈希链审计日志'],
    ['⬇ 部署与升级', 'TiDB 集群一键部署工具，平台自身一键升级与回滚']
  ];
  const S = {busy: false};


  function html(msg) {
    const env = ENV[OP.env] || ['', OP.env || '—'];
    const last = LS.get('opLastUser');
    return `<div class="lg-brand">
        <div class="lg-logo"><i>Ti</i><div>TiDB Ops<small>运维 · 诊断 · 优化</small></div></div>
        <h2>TiDB 集群运维平台</h2>
        <p>把诊断、变更、权限和审计放在同一个入口：每一次写操作都经过预检与审批，全部留痕，可追溯。</p>
        <div class="lg-feat">${FEATS.map(([t, d]) => `<div><b>${t}</b>${d}</div>`).join('')}</div>
        <div class="lg-foot">${OP.version ? '平台 v' + E(OP.version) + ' · ' : ''}知识来源：docs.pingcap.com/zh</div>
        <svg width="380" height="380" viewBox="0 0 100 100" aria-hidden="true"><g fill="none" stroke="#fff" stroke-width="1.2"><circle cx="50" cy="50" r="46"/><circle cx="50" cy="50" r="32"/><circle cx="50" cy="50" r="18"/><path d="M50 4v92M4 50h92"/></g></svg>
      </div>
      <div class="lg-pane${OP.icp ? ' has-icp' : ''}">
        <div class="lg-tools"><div class="theme-sw" data-theme-sw role="group" aria-label="界面主题"></div></div>
        <form class="lg-box" id="lgForm" autocomplete="off" novalidate>
          <h3>欢迎登录</h3>
          <div class="lg-sub"><span class="tag ${env[0]}">${E(env[1])}</span><span class="muted small">所有操作都会被记录审计</span></div>
          <div class="lg-msg ${msg ? 'show' : ''}" id="lgInfo" role="status">${E(msg || '')}</div>
          <div class="lg-fld"><label for="lgU">用户名</label><input id="lgU" type="text" autocomplete="username" autocapitalize="off" spellcheck="false" placeholder="请输入用户名" value="${E(last)}"></div>
          <div class="lg-fld"><label for="lgP">口令</label><div class="lg-pw"><input id="lgP" type="password" autocomplete="current-password" placeholder="请输入口令"><button type="button" id="lgEye" aria-label="显示或隐藏口令" title="显示 / 隐藏口令">👁</button></div><div class="lg-caps" id="lgCaps">⚠ 大写锁定已开启</div></div>
          <div class="lg-err" id="lgErr" role="alert"></div>
          <div class="lg-row"><label class="lg-chk"><input type="checkbox" id="lgRem" ${last ? 'checked' : ''}> 记住用户名</label><a href="#" id="lgForgot">忘记口令？</a></div>
          <div class="lg-help" id="lgHelp" hidden>
            <b>本地账号</b>：平台不提供邮件自助找回。请联系平台管理员，在「用户管理」里重置口令；重置后请立即自行修改。<br>
            <b>管理员自己忘记口令</b>：登录中控机，用安装平台的系统用户执行 <code>TIDBOPS_USER_PASSWORD=新口令 tidbops-server user passwd --data-dir &lt;数据目录&gt; --name &lt;用户名&gt;</code>（口令不会出现在命令行参数里）。
          </div>
          <button class="btn primary lg-go" type="submit" id="lgGo">登 录</button>
          ${OP.demo ? `<div class="lg-demo"><b>演示环境</b>：下面的账号仅用于演示，口令公开。选择后自动填充用户名；统一口令见页面下方说明。
            <select id="lgDemo" aria-label="选择演示账号"><option value="">— 选择演示账号 —</option>${DEMO.map(([u, n, r]) => `<option value="${u}">${u} · ${n}（${r}）</option>`).join('')}</select>
            <div class="small muted" style="margin-top:6px">除 <code>admin</code> 外统一口令为 <code>Tidbops-Demo-1</code>；<code>admin</code> 的口令以启动时的设置为准。</div></div>` : ''}
          <div class="lg-tip">连续输错口令会被临时锁定；账号被禁用时请联系管理员。</div>
        </form>
        ${OP.icp ? `<div class="lg-icp">${icpLink()}</div>` : ''}
      </div>`;
  }

  window.opShowLogin = function (msg) {
    let el = document.getElementById('opLogin');
    if (!el) { el = document.createElement('div'); el.id = 'opLogin'; document.body.appendChild(el); }
    const paint = () => { el.className = 'lg'; el.innerHTML = html(msg); el.style.display = 'grid'; wire(el); };
    paint();
  };

  function wire(el) {
    const $q = s => el.querySelector(s);
    const u = $q('#lgU'), p = $q('#lgP'), err = $q('#lgErr'), go = $q('#lgGo');
    const fail = m => { err.textContent = m; err.classList.toggle('show', !!m); };
    const submit = async ev => {
      if (ev) ev.preventDefault();
      if (S.busy) return;
      const user = u.value.trim();
      if (!user) { fail('请输入用户名'); u.focus(); return; }
      if (!p.value) { fail('请输入口令'); p.focus(); return; }
      fail(''); S.busy = true; go.disabled = true; go.textContent = '登录中…';
      const {code, data} = await opApi('POST', '/api/auth/login', {user, password: p.value});
      S.busy = false; go.disabled = false; go.textContent = '登 录';
      if (code !== 200) {
        fail(data.error || (code === 0 ? '无法连接后端' : '登录失败（' + code + '）'));
        p.value = '';
        // 提示信息里已经有「请在 hh:mm:ss 后重试」时不再多说；其余情况把焦点放回口令
        p.focus();
        return;
      }
      LS.set('opLastUser', $q('#lgRem').checked ? user : '');
      p.value = '';
      opApplyMe(data); el.style.display = 'none'; el.innerHTML = '';
      if (OP.loginResolve) { const f = OP.loginResolve; OP.loginResolve = null; f(); } else { opAfterLogin(); }
    };
    $q('#lgForm').addEventListener('submit', submit);
    $q('#lgEye').addEventListener('click', () => { p.type = p.type === 'password' ? 'text' : 'password'; p.focus(); });
    $q('#lgForgot').addEventListener('click', e => { e.preventDefault(); const h = $q('#lgHelp'); h.hidden = !h.hidden; });
    paintThemeSw();   // 主题按钮由 app.js 的文档级委托处理点击
    const caps = $q('#lgCaps');
    const capsCheck = e => { if (e.getModifierState) caps.classList.toggle('show', e.getModifierState('CapsLock')); };
    p.addEventListener('keydown', capsCheck); p.addEventListener('keyup', capsCheck); p.addEventListener('blur', () => caps.classList.remove('show'));
    const demo = $q('#lgDemo');
    if (demo) demo.addEventListener('change', () => { if (!demo.value) return; u.value = demo.value; p.value = demo.value === 'admin' ? '' : 'Tidbops-Demo-1'; fail(''); (p.value ? go : p).focus(); });
    setTimeout(() => (u.value ? p : u).focus(), 30);
  }
})();
