// 只用 node 拼一份预览页，等价于 tools/preview/build_preview.py 的注入部分。
//
// 存在的理由：这台机器上没有能用的 python（`python --version` 什么都不打印），
// 而改完界面总得看一眼。逻辑刻意和 py 那份保持一致：同一个 marker、同一份
// naming-options.json、同一个 stub.js。
const fs = require('fs');
const path = require('path');

const dir = __dirname;
const src = path.join(dir, '..', '..', 'frontend', 'index.html');
const out = path.join(dir, 'index.html');

let html = fs.readFileSync(src, 'utf8');
const options = fs.readFileSync(path.join(dir, 'naming-options.json'), 'utf8');

// 换行符要按文件实际情况拼：仓库里的 index.html 是 CRLF，
// 而 py 那份脚本跑在 LF 化的工作副本上，两边 marker 对不上就是静默失败。
const eol = html.includes('\r\n') ? '\r\n' : '\n';
const marker = '<script>' + eol + 'const App = window.go.main.App;';
if (!html.includes(marker)) {
  console.error('找不到主脚本入口，前端结构变了，改这个脚本的 marker');
  process.exit(1);
}

const inject = '<script>window.__namingOptions = ' + options + ';</script>' + eol
  + '<script src="stub.js"></script>' + eol + '<script>' + eol;

html = html.replace(marker, inject + 'const App = window.go.main.App;');

// ?view=tasks|templates|history|settings 直接切页（截图用）。
//
// 等 data-boot="done" 再点，不猜时间：前端启动是异步的，用固定 setTimeout
// 会和 boot() 抢时序 —— 慢一点就被首次渲染盖回去，截出来还是默认页。
const tail = [
  '<script>',
  '(function () {',
  '  const want = new URLSearchParams(location.search).get("view");',
  '  if (!want) return;',
  '  let tries = 0;',
  '  (function tick() {',
  '    if (document.documentElement.dataset.boot !== "done" && tries++ < 400) {',
  '      setTimeout(tick, 10);',
  '      return;',
  '    }',
  '    const btn = document.querySelector(".nav[data-view=\\"" + want + "\\"]");',
  '    if (btn) btn.click();',
  '  })();',
  '})();',
  '</script>',
  '</body>',
].join(eol);

html = html.replace('</body>', tail);

fs.writeFileSync(out, html, 'utf8');
console.log('wrote ' + out);
