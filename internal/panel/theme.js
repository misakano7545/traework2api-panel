'use strict';
/* theme.js 首屏主题预热：由 index.html 的 <head> 以同步脚本加载（必须在 <style> 之前）。
 *
 * 为什么需要单独一个文件：index.html 的 <html> 写死 data-theme="dark"，而真实偏好在
 * localStorage['tw2a_theme'] 里，读取它的是 app.js 的 applyTheme()。app.js 挂在 <body>
 * 末尾，刷新时浏览器早已按深色画完第一帧，之后才翻成用户选的浅色——这就是「刷新先闪
 * 一下夜间模式」的成因。把同样的判定提前到样式表解析之前，第一帧就是最终主题。
 *
 * 不能写成内联脚本：CSP 是 script-src 'self'，内联块会被直接拦掉（主题永远停在深色）。
 *
 * 判定规则必须与 app.js 的 effTheme() 逐字一致，否则「预热一套、app.js 再翻一套」二次闪烁：
 * 只有存了 light / dark 才按存的来，其余（auto、缺失）跟随系统。
 */
(function () {
  var saved = 'auto';
  try {
    saved = localStorage.getItem('tw2a_theme') || 'auto';
  } catch (e) {
    /* 隐私模式或禁用存储：退回跟随系统 */
  }
  var eff = saved === 'auto'
    ? (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark')
    : saved;
  document.documentElement.setAttribute('data-theme', eff);
})();
