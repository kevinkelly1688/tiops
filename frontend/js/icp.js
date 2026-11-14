// @author: kevinkelly
// @license: Apache License 2.0
// @file: icp.js
// @time: 2026/10/28

/* 备案号：来自后端配置文件（platform.json 的 icp，见 deploy/ops/platform.json.example），随 /api/health 下发，ops.js 的 opInit 存进 OP.icp。
   - 登录页：login.js 调 icpLink() 把备案号放在表单区底部。
   - 主界面：页脚 #siteFoot（index.html），boot.js 在 opInit（含登录）完成后调 icpRender()。
   没配置备案号时两处都不显示。备案号按纯文本输出（opEsc 转义），链接指向工信部备案系统。
   依赖 ops.js（OP / opEsc）。 */
const ICP_URL = 'https://beian.miit.gov.cn/';

function icpLink() {
  return OP.icp ? `<a href="${ICP_URL}" target="_blank" rel="noopener noreferrer">${opEsc(OP.icp)}</a>` : '';
}

function icpRender() {
  const el = document.getElementById('siteFoot'); if (!el) return;
  el.innerHTML = icpLink();
  el.hidden = !OP.icp;
}
