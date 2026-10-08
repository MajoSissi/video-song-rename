# 把 frontend/index.html 复制一份并注入预览桩，产出可在浏览器里打开的页面。
#
# 只服务于「改完界面先看一眼」，不进构建、不进 exe。
#
# 用法：
#   python tools/preview/build_preview.py
#   cd tools/preview && python -m http.server 8765
#   chrome --headless=new --screenshot=shot.png --window-size=1240,820 \
#          "http://127.0.0.1:8765/index.html?view=templates"
#
# URL 参数：
#   view=tasks|templates|history|settings   直接切到某一页（截图用）
#   dump=1                                  把关键 DOM 值打成文本（配合 --dump-dom 读）
#
# 曾经的 menu=1 / probe=1 已经删掉：它们探的是「开始」旁边那个下拉菜单，
# 而那个菜单后来从界面上撤了（#btnStartMore / #startMenu 都不存在了）。
# 留着只会抛 TypeError，还让人以为「预览页坏了」。探一个不存在的东西，
# 比没有探针更糟 —— 它会把人引到错的方向。
#
# 注意 dump=1 必须配 --virtual-time-budget，否则读出来的是脚本源码：
#   chrome --headless=old --dump-dom --virtual-time-budget=5000 \
#          "http://127.0.0.1:8765/index.html?view=templates&dump=1"
# 原因：--timeout 是「加载的最长等待」而不是「加载完再等 N 毫秒」，
# 页面 load 一触发就拍了，而 dump 要等 boot() 完成 —— 差的就是这几十毫秒。
# （--virtual-time-budget 会推进计时器，让等待真的发生。）
#
# stub.js 提供假的 window.go / window.runtime，数据是编的，只为了看排版。
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
SRC = ROOT / "frontend" / "index.html"
OUT = pathlib.Path(__file__).resolve().parent / "index.html"
OPTIONS = pathlib.Path(__file__).resolve().parent / "template-options.json"

html = SRC.read_text(encoding="utf-8")

# 变量目录先注入：stub.js 要用它。真正的那份在 Go，
# 这份 JSON 是两个静态桩共用的副本（另一个是 tools/shoot_ui.py），
# frontend_contract_test.go 会逐项比对。
OPTIONS_JS = (
    '<script>window.__templateOptions = '
    + OPTIONS.read_text(encoding="utf-8")
    + ';</script>\n'
)

STUB = OPTIONS_JS + '<script src="stub.js"></script>\n<script>\n'
marker = "<script>\nconst App = window.go.main.App;"
if marker not in html:
    sys.exit("找不到主脚本入口，前端结构变了，改这个脚本的 marker")

# 错误陷阱必须放在主脚本**之前**。
#
# 踩过：原来把它和 __dump 一起放在页面末尾，于是主脚本自己抛的错
# 一个都收不到 —— 桩里少了 runtime.OnFileDrop，主脚本在那一行抛 TypeError，
# 后面的导航点击、boot()、模板加载全都没注册，页面「能显示但全是死的」，
# 而错误陷阱报的是「无错误」。找错方向找了好久。
# 放在前面才拦得住主脚本的同步异常和未处理的 Promise 拒绝。
TRAP = (
    "<script>"
    "window.__errors=[];"
    "window.addEventListener('error',function(e){"
    "window.__errors.push('error: '+(e.message||e)+' @'+(e.lineno||'?'));"
    "document.documentElement.setAttribute('data-js-error',(e.message||'')+' @'+(e.lineno||'?'));});"
    "window.addEventListener('unhandledrejection',function(e){"
    "window.__errors.push('reject: '+e.reason);"
    "document.documentElement.setAttribute('data-js-reject',String(e.reason));});"
    "</script>\n"
)

html = html.replace(marker, TRAP + STUB + "const App = window.go.main.App;", 1)

# 追加：视图选择器（?view= 直接切页）+ 诊断输出（?dump=1 把关键 DOM 值打成文本）。
# 截图只能证明「看着像」，dump 才能证明「值是对的」。
TAIL = """
<script>
// 错误陷阱已经在主脚本之前装好了（见 TRAP）。这里**不能**再写一遍
// `window.__errors = []` —— 那会把主脚本报的错清空，等于又变回看不见。
function __dump() {
  const val = (id) => { const n = document.getElementById(id); return n ? n.value : '<missing>'; };
  const txt = (id) => { const n = document.getElementById(id); return n ? n.textContent.trim() : '<missing>'; };
  const out = {
    errors: window.__errors,
    // tplName 那个「名称」输入框已经删掉了 —— 名字现在只在左栏右键里改。
    // 留着旧键只会永远输出 <missing>，看起来像「元素没渲染出来」。
    // 改成打印「左栏里被选中的那一份叫什么」。
    tplEditing: (() => {
      const on = document.querySelector('#tplList .tpl-item.on b');
      return on ? on.textContent.trim() : null;
    })(),
    // tplBody 那个输入框已经拆掉了，改成打印每一段的下拉框取值。
    // 留着旧键只会永远输出 <missing>，看起来像「元素没渲染出来」。
    slotKeys: [...document.querySelectorAll('#slotList .slotchip select')].map((s) => s.value),
    slotCount: document.querySelectorAll('#slotList .slotchip').length,
    // 每一段自己的「分隔符 / 前缀 / 后缀」是**常驻**的，没有展开按钮。
    // slotToggles 必须是 0：它守着「折叠那版别再回来」。
    // 不数 `.chip-more`（那个类已删干净，数它永远 0，断言等于没有），
    // 数的是「每一格里除了 × 之外的按钮」。
    slotFieldCounts: [...document.querySelectorAll('#slotList .slotchip')]
      .map((c) => c.querySelectorAll('.chip-fields select, .chip-fields input').length),
    slotToggles: [...document.querySelectorAll('#slotList .slotchip')].reduce(
      (n, c) => n + [...c.querySelectorAll('button')]
        .filter((b) => !b.classList.contains('chip-x')).length, 0),
    tplListItems: [...document.querySelectorAll('#tplList .tpl-item')].map((n) => n.textContent.replace(/\\s+/g, ' ').trim()),
    tplListActive: [...document.querySelectorAll('#tplList .tpl-item')].filter((n) => n.classList.contains('on')).length,
    tplPreview: txt('tplPreview'),
    selTemplateOptions: [...document.querySelectorAll('#selTemplate option')].map((o) => o.textContent),
    rows: document.querySelectorAll('#tbody tr').length,
    cfgSegment: val('cfgSegment'),
    cfgCloseToTray: document.getElementById('cfgCloseToTray').checked,
    cfgExtensions: val('cfgExtensions'),
    winCloseTitle: document.getElementById('winClose').title,
    histRows: document.querySelectorAll('#histBody tr').length,
    navActive: [...document.querySelectorAll('.nav.active')].map((n) => n.dataset.view),
    visibleView: [...document.querySelectorAll('.view')].filter((n) => !n.classList.contains('hidden')).map((n) => n.id),
  };
  document.body.innerHTML = '<pre id="DUMP">' + JSON.stringify(out, null, 2) + '</pre>';
}

(function () {
  const q = new URLSearchParams(location.search);

  // 所有动作都等 boot() 跑完再发。
  //
  // 踩过两回：
  //   1. 切视图写成 setTimeout(click, 80) —— 和 boot() 抢时序，
  //      慢一点就被 boot 的首次渲染盖回去，截出来还是默认页；
  //   2. dump 写成 setTimeout(__dump, 1600) —— 而 --dump-dom 的时机是
  //      「加载完就拍」，dump 根本没轮到，读出来的是 `<pre id="DUMP">`
  //      那段脚本源码，看着像「页面没渲染」。
  // 前端会打 data-boot="done"，盯着它就行，不用猜时间。
  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }

    const want = q.get('view');
    if (want) {
      const btn = document.querySelector('.nav[data-view="' + want + '"]');
      if (btn) btn.click();
    }
    if (q.get('dump')) __dump();
  })();
})();
</script>
</body>"""

html = html.replace("</body>", TAIL, 1)
OUT.write_text(html, encoding="utf-8")
print(f"ok -> {OUT}")
