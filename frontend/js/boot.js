// @author: kevinkelly
// @license: Apache License 2.0
// @file: boot.js
// @time: 2026/10/28

/* 启动：先尝试连接后端获取真实 / 回放数据，再渲染页面（外部脚本，满足 CSP 禁止内联脚本的要求） */
opInit().then(() => { icpRender(); return liveInit(); }).then(boot, boot);
// 通用事件：关闭抽屉 / 打印（替代内联 onclick）
document.addEventListener('click', e => {
  if (e.target.closest('[data-close]')) closeDrawer();
  if (e.target.closest('[data-print]')) window.print();
});
