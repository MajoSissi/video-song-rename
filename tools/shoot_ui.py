#!/usr/bin/env python3
"""用系统自带的 Chrome 无头截图，把四个视图各截一张。

为什么要这么绕：界面排版这类改动**只读 HTML 源码是验证不了的**——
CSS 有没有生效、卡片有没有被挤变形、无边框标题栏是不是真的在顶上，
都只有截图能看出来。而为一张截图去装 Chromium 不值得。

做法是把 frontend/index.html 复制一份，在它前面注入 window.go / window.runtime
的桩（真实的那些只有 Wails 运行时才提供），再用本地静态服务喂给 Chrome。

用法：
    python tools/shoot_ui.py [输出目录]

输出目录默认 build/shots。
"""

from __future__ import annotations

import functools
import hashlib
import http.server
import json
import os
import pathlib
import re
import shutil
import socket
import subprocess
import sys
import threading
import time

REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
INDEX = REPO_ROOT / "frontend" / "index.html"

CHROME_CANDIDATES = [
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
    os.path.expandvars(r"%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe"),
]

VIEWS = ["tasks", "templates", "history", "settings"]

# 常规截图尺寸。几何探针必须用同一个值，见 view_state 的 size 参数。
WINDOW_SIZE = "1280,880"

# 窗口的最小尺寸，和 main.go 里的 MinWidth/MinHeight 保持一致。
MIN_WINDOW_SIZE = "960,620"

# 设置页左列的六个分组，和 index.html 里 #setNav 的 data-group 一一对应。
SETTINGS_GROUPS = [
    ("recognize", "识别"),
    ("scan", "扫描范围"),
    ("ffmpeg", "FFmpeg程序"),
    ("window", "窗口与后台"),
    ("network", "网络"),
    ("files", "文件位置"),
]

# boot() 里 showSettingsGroup() 的默认参数就是这一组。
# 直接进「设置」页看到的就应该是它 —— 所以它的截图**必然**和
# #settings 那张一模一样，这不是「分组没切过去」，不能当重复图报错。
# 反过来还能当一条正向断言用：这两张必须相同，不同就说明默认分组变了。
SETTINGS_DEFAULT_GROUP = "recognize"

# 桩数据。要让界面看起来是「真的在跑」，所以状态铺开一点。
STUB_ITEMS = [
    {"index": 0, "path": r"D:\Music\VID_20240103_210455.mp4", "dir": r"D:\Music",
     "name": "VID_20240103_210455.mp4", "ext": ".mp4", "size": 183_000_000,
     "status": "renamed", "title": "Back In Black", "artist": "AC/DC",
     "album": "Back In Black", "released": "1980-07-25", "newName": "Back In Black.mp4",
     "newPath": r"D:\Music\Back In Black.mp4", "shazamUrl": "https://www.shazam.com/track/1",
     "message": "已重命名"},
    {"index": 1, "path": r"D:\Music\rec_20240211.mp4", "dir": r"D:\Music",
     "name": "rec_20240211.mp4", "ext": ".mp4", "size": 96_000_000,
     "status": "running", "message": "识别中…"},
    {"index": 2, "path": r"D:\Music\clip_8831.mkv", "dir": r"D:\Music",
     "name": "clip_8831.mkv", "ext": ".mkv", "size": 402_000_000,
     "status": "no_match", "newName": "0_clip_8831.mkv",
     "newPath": r"D:\Music\0_clip_8831.mkv", "message": "未识别，已改为 0_clip_8831.mkv"},
    {"index": 3, "path": r"D:\Music\download (3).mp3", "dir": r"D:\Music",
     "name": "download (3).mp3", "ext": ".mp3", "size": 8_400_000,
     "status": "matched", "title": "夜空中最亮的星", "artist": "逃跑计划",
     "album": "世界", "newName": "夜空中最亮的星.mp3",
     "newPath": r"D:\Music\夜空中最亮的星.mp3"},
    {"index": 4, "path": r"D:\Music\broken.mp3", "dir": r"D:\Music",
     "name": "broken.mp3", "ext": ".mp3", "size": 1_200,
     "status": "failed", "message": "无法解码：unrecognized format"},
    {"index": 5, "path": r"D:\Music\A0001.mov", "dir": r"D:\Music",
     "name": "A0001.mov", "ext": ".mov", "size": 2_100_000_000,
     "status": "pending"},
]

STUB_TEMPLATES = [
    {"name": "只歌名", "separator": " - ", "slots": [{"var": "title"}], "active": False},
    {"name": "歌名 - 艺人", "separator": " - ",
     "slots": [{"var": "title"}, {"var": "artist"}], "active": False},
    {"name": "艺人 - 歌名", "separator": " - ",
     "slots": [{"var": "artist"}, {"var": "title"}], "active": False},
    # 「歌名 (年份)」：括号挂在这一格上，年份取不到时整格（含括号）消失。
    # separator 显式写成空串，表示「不要分隔符」——和「跟随全局」是两回事。
    {"name": "歌名 (年份)", "separator": " - ",
     "slots": [{"var": "title"},
               {"var": "year", "prefix": " (", "suffix": ")", "separator": ""}],
     "active": False},
    # 用户报的那个场景，**故意设为当前**：
    # 截图里「缺中文歌名」那一行会显示成 "BTS - Spring Day" 而不是 "BTS - Spring Day -"，
    # 这就是这次改动要证明的事，不摆出来截图就白截了。
    {"name": "艺人 - 英文名 - 中文名", "separator": " - ",
     "slots": [{"var": "artist"},
               {"var": "title", "script": "en"},
               {"var": "title", "script": "zh"}],
     "active": True},
]

# 变量 / 语种 / 分隔符目录。
#
# 不在这里再写一份：**唯一权威在 Go**（internal/naming 的 Choices / Separators），
# 而这份 JSON 是两个静态预览桩共用的副本（另一个是 tools/preview/stub.js）。
# frontend_contract_test.go 的 TestStubTemplateOptionsMatchGo 会逐项比对，漂移直接变红。
TEMPLATE_OPTIONS_PATH = REPO_ROOT / "tools" / "preview" / "template-options.json"
STUB_TEMPLATE_OPTIONS = json.loads(TEMPLATE_OPTIONS_PATH.read_text(encoding="utf-8"))

STUB_CONFIG = {
    "segment_seconds": 12, "concurrency": 3, "retries": 2,
    "language": "en-US", "country": "GB", "timeout_seconds": 20,
    "name_template": {"separator": " - ", "slots": [{"var": "artist"}, {"var": "title"}]},
    "templates": STUB_TEMPLATES, "active_template": "艺人 - 英文名 - 中文名",
    "unmatched_prefix": "0_", "unmatched_suffix": "", "max_name_runes": 120,
    "extensions": [".mp4", ".mkv", ".avi", ".mov", ".flv", ".wmv", ".webm", ".m4v", ".ts",
                   ".mp3", ".wav", ".flac", ".m4a", ".aac", ".ogg", ".wma"],
    "recursive": True, "skip_processed": True, "dry_run": False,
    "user_agent": "", "close_to_tray": True,
    "ffmpeg_path": "",
    "proxy": "",
}

# 记录页的三种状态各放一条，缺哪种那一格的样式就没被截图看见过：
# 可还原（有按钮）/ 已还原（没按钮）/ 文件已不在（没按钮）。
STUB_HISTORY = [
    {"ts": 1770000000000, "from": r"D:\Music\VID_20240103_210455.mp4",
     "to": r"D:\Music\Back In Black.mp4",
     "nowPath": r"D:\Music\Back In Black.mp4", "restored": False, "exists": True},
    {"ts": 1769999600000, "from": r"D:\Music\VID_20240103_211502.mp4",
     "to": r"D:\Music\Thunderstruck.mp4",
     "nowPath": r"D:\Music\Thunderstruck.mp4", "restored": False, "exists": True},
    # 还原过的记录：文件已经被改回原名了，所以 nowPath 就是 from。
    # 界面不该把它当成「被外部改过名」—— 那是还原的结果，不是外部改名。
    {"ts": 1769999000000, "from": r"D:\Music\clip_8831.mkv",
     "to": r"D:\Music\0_clip_8831.mkv",
     "nowPath": r"D:\Music\clip_8831.mkv", "restored": True, "exists": True},
    # 这一条演示「按唯一值认文件」：文件在程序外被手动改过名，所以 nowPath
    # 和记录里的现名不一样。界面要显示文件**实际**在的名字并挂一个「已改名」的
    # 小标 —— 不造这一条的话，那套逻辑在截图上永远看不见，
    # 而它正是这次改动的全部意义。
    {"ts": 1769998600000, "from": r"D:\Music\rec_0041.mkv",
     "to": r"D:\Music\Hells Bells.mkv",
     "nowPath": r"D:\Music\Hells Bells (1).mkv", "restored": False, "exists": True},
    {"ts": 1769998000000, "from": r"D:\Music\download (3).mp3",
     "to": r"D:\Music\夜空中最亮的星.mp3",
     "nowPath": r"D:\Music\夜空中最亮的星.mp3", "restored": False, "exists": False},
]

# 分页用的两份大样本。
#
# 只有 8 个条目 / 5 条记录的桩验不出分页 —— 页数永远是 1，
# 「第 2 页画的其实是第 1 到 50 条」这类错误一个都照不出来。
# 120 是挑的：每页 50 正好 3 页，最后一页只有 20 条（能验「尾页不满」），
# 换 100 一页是 2 页，换 200 一页是 1 页 —— 三档各有各的形状。
PAGER_SAMPLE = 120

# 样本的时间基准。**故意取一个过去的日子**（2026-02-02 前后）：
# 这样「今天」「本月」两档预设必定命中 0 条，而「0 条」正是它们唯一
# 能证明自己的方式 —— 预设要是写成了「全都返回」，这里就照不出来。
PAGER_BASE_TS = 1770000000000
PAGER_HOUR = 60 * 60 * 1000


def big_items() -> list:
    """任务页的分页样本：120 条。

    状态按四种轮着来，于是每个筛选档各 30 条 —— 筛选和分页叠在一起才验得动
    （筛完还剩几页、页码有没有回到第 1 页，都是这两件事交叉出来的）。
    """
    statuses = ["renamed", "no_match", "failed", "pending"]
    items = []
    for i in range(PAGER_SAMPLE):
        name = "clip_%04d.mp4" % (i + 1)
        new = "Track %d - Artist %d.mp4" % (i + 1, i + 1)
        items.append({
            "index": i,
            "path": "D:\\Media\\分页样本\\" + name,
            "dir": "D:\\Media\\分页样本",
            "name": name, "ext": ".mp4",
            "size": 40 * 1024 * 1024 + i * 1024,
            "status": statuses[i % len(statuses)],
            "title": "Track %d" % (i + 1),
            "artist": "Artist %d" % (i + 1),
            "album": "", "genre": "", "released": "", "label": "",
            "coverArt": "", "shazamUrl": "",
            "newName": new, "newPath": "D:\\Media\\分页样本\\" + new,
            "message": "", "attempts": 1,
        })
    return items


def big_history() -> list:
    """记录页的分页样本：120 条，最新的在前（和后端 History() 的顺序一致）。

    时间戳按小时递减铺开，于是「某一天」这个范围只命中其中一段 ——
    日期筛选的边界（含起、含止、右端半开）才验得出来。
    """
    entries = []
    for i in range(PAGER_SAMPLE):
        name = "rec_%04d.mp4" % (i + 1)
        to = "Song %04d.mp4" % (i + 1)
        entries.append({
            "ts": PAGER_BASE_TS - i * PAGER_HOUR,
            "from": "D:\\Media\\分页样本\\" + name,
            "to": "D:\\Media\\分页样本\\" + to,
            "nowPath": "D:\\Media\\分页样本\\" + to,
            # 每 5 条一条已还原、每 7 条一条文件已不在，于是三种状态都有，
            # 而且「全选只勾可还原的」和「移除所选对三种都生效」都能分辨出来。
            "restored": i % 5 == 4,
            "exists": i % 7 != 6,
        })
    return entries


STUB_JS = """
// ---- 截图专用的桩：真实的 window.go / window.runtime 只有 Wails 运行时才有 ----
window.runtime = {
  EventsOn: function () {}, EventsEmit: function () {}, EventsOff: function () {},
  OnFileDrop: function () {},
  WindowIsMaximised: function () { return Promise.resolve(false); },
  WindowMinimise: function () {}, WindowToggleMaximise: function () {},
  WindowHide: function () {}, WindowShow: function () {}, WindowUnminimise: function () {},
  Quit: function () {},
};
window.wails = { flags: { cssDragProperty: '--wails-draggable', cssDragValue: 'drag' } };

const __items = __STUB_ITEMS__;
const __templates = __STUB_TEMPLATES__;
const __templateOptions = __STUB_TEMPLATE_OPTIONS__;
const __config = __STUB_CONFIG__;
const __history = __STUB_HISTORY__;

function __ok(value) { return Promise.resolve(value); }

window.go = { main: { App: {
  SelfCheck: function () { return __ok({
    ok: true, sidecarPath: 'D:\\\\app\\\\shazam-sidecar.exe', sidecarVersion: '0.1.0',
    ffmpegPath: 'C:\\\\Users\\\\Majo\\\\AppData\\\\Local\\\\Microsoft\\\\WinGet\\\\Links\\\\ffmpeg.exe',
    ffmpegConfigured: '',
    dataDir: 'D:\\\\app\\\\data',
    configPath: 'D:\\\\app\\\\data\\\\video-song-rename.json',
    logPath: 'D:\\\\app\\\\data\\\\video-song-rename.log',
    problem: '', hint: '', configError: ''
  }); },
  GetConfig: function () { return __ok(__config); },
  SaveConfig: function (cfg) { return __ok(cfg); },
  // 代理连通性探测。桩里返回一个「通」的结果，好让设置页网络分组能截到
  // 测试通过时的样子；真实实现在 Go 侧，走一次真的 Shazam 请求。
  TestNetwork: function (proxy) { return __ok({
    ok: true, status: 'network_ok',
    proxy: proxy || '跟随环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）',
    http_status: 400, elapsed_ms: 186, error: ''
  }); },
  Templates: function () { return __ok(__templates); },
  SaveTemplate: function (input) {
    const found = __templates.find(function (t) { return t.name === input.name; });
    if (found) { found.separator = input.separator; found.slots = input.slots; }
    else __templates.push({ name: input.name, separator: input.separator, slots: input.slots, active: false });
    return __ok(__templates);
  },
  SetActiveTemplate: function (name) {
    __templates.forEach(function (t) { t.active = (t.name === name); });
    return __ok(__templates);
  },
  DeleteTemplate: function (name) {
    const i = __templates.findIndex(function (t) { return t.name === name; });
    if (i >= 0) __templates.splice(i, 1);
    if (!__templates.some(function (t) { return t.active; })) __templates[0].active = true;
    return __ok(__templates);
  },
  RenameTemplate: function (oldName, newName) {
    const found = __templates.find(function (t) { return t.name === oldName; });
    if (found) found.name = newName;
    return __ok(__templates);
  },
  DuplicateTemplate: function (name, newName) {
    const found = __templates.find(function (t) { return t.name === name; });
    if (found) {
      __templates.push(Object.assign({}, found, { name: newName, slots: (found.slots || []).slice() }));
    }
    return __ok(__templates);
  },
  ResetTemplates: function () { return __ok(__templates); },
  TemplateEditorOptions: function () { return __ok(__templateOptions); },
  PreviewTemplate: function (input) {
    // 桩里这份渲染**只为了把版式画出来**。
    // 真正的实现只有 Go 那一份（naming.Template.Render），别照这个改逻辑 ——
    // 之所以还是写了个简化版而不是返回死文本，是因为「缺中文歌名」那一行
    // 正是这个功能要证明的事，写成死文本截图就看不出来了。
    const rows = [
      { label: '全都识别到', note: '艺人、英文名、中文名、年份都有',
        data: { title: 'Spring Day', 'title:en': 'Spring Day', 'title:zh': '春日',
                artist: 'BTS', 'artist:en': 'BTS', album: 'You Never Walk Alone', year: '2017' } },
      { label: '缺中文歌名', note: '只有英文写法 —— 中文那一格整个消失，不留分隔符',
        data: { title: 'Spring Day', 'title:en': 'Spring Day',
                artist: 'BTS', 'artist:en': 'BTS', album: 'You Never Walk Alone', year: '2017' } },
      { label: '只识别到歌名', note: '艺人和专辑都没识别到',
        data: { title: 'Spring Day', 'title:en': 'Spring Day' } },
    ];

    function render(data) {
      let out = '';
      let written = false;
      (input.slots || []).forEach(function (slot) {
        // 带语种的槽位**只**看那个语种的键，不回退到通用键。
        // 回退的话「中文歌名」槽位会拿到通用歌名，渲染出 "Spring Day - Spring Day"，
        // 截图里看着像模板写错了 —— 真实现（naming.Slot.Value）也是不回退的。
        const key = slot.script ? (slot.var + ':' + slot.script) : slot.var;
        const value = slot.var === 'literal' ? (slot.text || '') : (data[key] || '');
        if (!value) return;                    // 空槽整个跳过 —— 关键就在这一行
        if (written) {
          const own = (slot.separator === null || slot.separator === undefined)
            ? (input.separator || ' - ') : slot.separator;
          out += own;
        }
        out += (slot.prefix || '') + value + (slot.suffix || '');
        written = true;
      });
      return out;
    }

    const cases = rows.map(function (row) {
      return { label: row.label, note: row.note, text: render(row.data), empty: false };
    });
    cases.push({ label: '完全没识别到', note: '走「未识别」前缀，模板不参与',
                 text: '0_VID_20240103_210455', empty: false });
    return __ok(cases);
  },
  Items: function () { return __ok(__items); },
  Progress: function () { return __ok({ total: 6, done: 5, matched: 2, unmatched: 1, failed: 1, skipped: 0, renamed: 1, running: true }); },
  History: function () { return __ok(__history); },
  // 移除记录。桩里真的从这份数组里删掉那几条，好让「移除所选（2）」
  // 和点完之后剩下的行数对得上。
  RemoveHistory: function (targets) {
    const drop = new Set(targets || []);
    for (let i = __history.length - 1; i >= 0; i--) {
      if (drop.has(__history[i].to)) __history.splice(i, 1);
    }
    return __ok(__history);
  },
  PreviewCommand: function (p) { return __ok('shazam-sidecar.exe --file "' + p + '" --seconds 12 --language en-US --country GB --timeout 20'); },
  AddPaths: function () { return __ok({ items: __items, added: 0, rejected: [], expanded: [] }); },
  CloseWindow: function () {}, ShowWindow: function () {}, HideWindow: function () {}, QuitApp: function () {},
  Pause: function () {}, Resume: function () {}, Stop: function () {}, Start: function () {},
  Clear: function () {}, RemoveItems: function () { return __ok(__items); },
  ScanPath: function () { return __ok(__items); },
  // 还原之后让队列跟上磁盘。**不新增也不删除**，所以这里照原样回一份。
  ResyncQueue: function () { return __ok(__items); },
  PickFiles: function () { return __ok([]); }, PickFolder: function () { return __ok(''); },
  PickFFmpeg: function () { return __ok(''); },
  ExportCSV: function () { return __ok(''); }, Undo: function () { return __ok({ restored: 0, missing: 0, skipped: 0, details: [] }); },
  UndoOne: function (to) { return __ok({ restored: 1, missing: 0, skipped: 0, details: [] }); },
  UndoMany: function (to) { return __ok({
    restored: (to || []).length, missing: 0, skipped: 0, details: [] }); },
  OpenLog: function () { return __ok(); }, OpenConfigFolder: function () { return __ok(); },
  RevealPath: function () { return __ok(); }, OpenURL: function () { return __ok(); },
} } };
"""

SWITCH_JS = """
// 切视图必须**同步**做完。
//
// 无头 Chrome 的 --timeout 是「页面加载的最长等待」，不是「加载完再等 N 毫秒」——
// 截图在 load 事件一触发就拍下了。所以任何放在 setTimeout / setInterval 里的
// 切换都赶不上，只会得到几张一模一样的图（而且不报任何错）。
//
// 这段插在主脚本之后，此时 .nav 的 onclick 已经注册好，直接点一下就行。
// 切视图只是切 .hidden，不依赖 boot() 那些异步数据。
(function () {
  const parts = (location.hash || '#tasks').slice(1).split('+');
  const nav = document.querySelector('.nav[data-view="' + parts[0] + '"]');
  if (nav) nav.click();

  // #settings+group:ffmpeg 切到设置页的某个分组。
  // 设置页左列有六个分组，一次只显示一组 —— 不切过去就看不见别的组长什么样。
  const group = parts.find((p) => p.startsWith('group:'));
  if (group) {
    const button = document.querySelector('#setNav .wsg[data-group="' + group.slice(6) + '"]');
    if (button) button.click();
  }

  // #<view>+modal 额外弹一个确认框，专门给弹窗截图用。
  // 它是 fixed 遮罩盖在整页之上，遮罩深浅、卡片宽度、按钮排布只有截图看得出来。
  if (parts.includes('modal')) {
    askConfirm({
      title: '清空队列',
      body: '队列里的条目会全部移除。\\n磁盘上的文件不会被删除。',
      ok: '清空',
      danger: true,
    });
  }
})();
"""

# 弹窗的**行为**探针。截图只能证明它长得对，证明不了
# 「Esc 是不是真的取消了」「点卡片里面会不会误关」。
# 这几条都是异步的（askConfirm 返回 Promise），所以只能串成 async 序列。
TEMPLATE_PROBE_JS = """
// 槽位编辑器的行为断言。只在 #templates+slotprobe 时跑。
//
// 要等 boot() 跑完 —— chip 是 loadTemplates() 之后才建出来的，
// 而 SWITCH_JS 是同步切的视图，那时候 #slotList 里只有一个「+ 添加」。
(function () {
  if (!location.hash.split('+').includes('slotprobe')) return;

  function run() {
    const out = {};
    const chips = () => document.querySelectorAll('#slotList .slotchip');
    const keyOf = (i) => {
      const chip = chips()[i];
      const select = chip && chip.querySelector('select');
      return select ? select.value : '';
    };

    // 老的「模板内容」输入框必须已经不在了。
    // 改成槽位编辑之后留着它，就是一个没人写也没人读的僵尸控件。
    out.hasBodyInput = !!document.getElementById('tplBody');
    out.chips = chips().length;
    out.firstKey = keyOf(0);

    // ---- 模板列表不该再标「当前」 ----
    //
    // 用哪一份模板是**任务页那颗下拉**现场决定的；模板页里选中的只是
    // 「正在编辑哪一份」。列表上再挂一个「当前」小标，会让人以为
    // 在模板页点一下就等于换了识别用的模板。
    out.listBadges = document.querySelectorAll('#tplList .badge').length;
    out.listSaysCurrent = [...document.querySelectorAll('#tplList *')]
      .filter((n) => !n.children.length && n.textContent.trim() === '当前').length;
    // 原来包住「名字 + 角标」的那层 .row1 也该一起没了 ——
    // 只剩名字和一行说明之后，它就是个多余的 flex 容器。
    out.listRow1 = document.querySelectorAll('#tplList .row1').length;

    // ---- 模板页右边不该有滚动条 ----
    //
    // 这一页统共四段，正常窗口尺寸下就该一眼看全。
    // 量的是 scrollHeight - clientHeight：截图那一路带着 --hide-scrollbars，
    // **永远看不到滚动条**，所以「看起来没有」不算数 ——
    // 只有这个差值是 0，才是真的装得下、而不是滚不滚得动的问题。
    //
    // 必须在开头量：下面会反复增删槽位，那时候的布局已经不是默认那份模板了。
    const scroller = document.getElementById('tplScroll');
    out.scrollOver = scroller ? scroller.scrollHeight - scroller.clientHeight : -1;
    const prev = document.getElementById('tplPreview');
    out.previewOver = prev ? prev.scrollHeight - prev.clientHeight : -1;
    // 命名预览是这一页唯一允许被压的一块，所以它必须留得住 flex-shrink。
    // 改成 flex:none 的话窗口一矮整页就又开始滚。
    out.previewShrink = prev ? getComputedStyle(prev).flexShrink : '';

    // 「+ 添加」必须在同一行里，而且是最后一个 ——
    // 这正是 Majo 要的 `[a] [b] [c] [+]` 形状。
    const add = document.getElementById('btnAddSlot');
    out.addInRow = !!(add && add.closest('#slotList'));
    out.addIsLast = !!(add && add.parentElement && add === add.parentElement.lastElementChild);

    // 每个 chip 都得是一个下拉框 + 一个 ×，不能退化成纯文本。
    let shaped = 0;
    for (const chip of chips()) {
      if (chip.querySelector('select') && chip.querySelector('.chip-x')) shaped++;
    }
    out.chipsWithSelect = shaped;

    // 加一段 → 多一个 chip，而且加在末尾。
    add.click();
    out.chipsAfterAdd = chips().length;
    out.lastKeyAfterAdd = keyOf(chips().length - 1);

    // 排序：把第 0 段挪到第 1 位，前两个应当对调。
    // HTML5 拖放在无头 Chrome 里没法用脚本触发，所以直接调底层那个函数 ——
    // 要守的是「换位算得对」，不是「dragstart 绑上了没有」。
    //
    // 两个 key 必须非空且互不相同，不能只写 `a === before[1] && b === before[0]`。
    // 变异测试撞到过：把 chip 退化成纯文本时 keyOf 一律返回空串，
    // 而 `'' === '' && '' === ''` 恒真 —— 断言在最该报警的时候「通过」了。
    const before = [keyOf(0), keyOf(1)];
    out.swapKeys = before.join('|');
    moveSlotTo(0, 1);
    out.swapped = before[0] !== '' && before[1] !== '' && before[0] !== before[1]
      && keyOf(0) === before[1] && keyOf(1) === before[0];

    // 删除 → 少一个 chip。
    chips()[0].querySelector('.chip-x').click();
    out.chipsAfterDelete = chips().length;

    // ---- 补充设置常驻 ----
    //
    // 这三项（分隔符 / 前缀 / 后缀）原来挂在 chip 行下面一块独立的「第 N 段」面板里，
    // 后来改成点右边的 ⋯ 就地展开。现在**直接摊在每一行里**，一行一段竖着排：
    // 想知道某一段设了什么，不该先点开它 —— 这一页本来就是用来一眼看全的。
    //
    // 折叠那版的另一个代价是「展开会把这一格撑满一行、后面的 chip 全被挤走」。
    // 竖排之后这个代价没有了，所以「展开」这件事本身也没必要存在。

    // 后面要反复增删，先垫到 5 格，免得断言依赖桩里那个模板原本有几段。
    while (chips().length < 5) addSlot();

    // 展开按钮一个都不该剩。它是折叠那版的残留，留着就会有人去点。
    //
    // 这里**不数 `.chip-more`**：那个类已经删干净了，数它永远是 0 ——
    // 断言在最该报警的时候「通过」，等于没有断言（这轮就踩过这个洞）。
    // 改成数「每一格里除了 × 之外还有没有别的按钮」，谁再往里塞一颗都会被抓住。
    out.toggles = [...chips()].reduce(
      (n, c) => n + [...c.querySelectorAll('button')]
        .filter((b) => !b.classList.contains('chip-x')).length, 0);
    out.openClassLeft = document.querySelectorAll('#slotList .slotchip.open').length;

    // 字段必须是**看得见**的，不只是「在 DOM 里」。折叠那版是 display:none 收起来的。
    out.fieldsHidden = [...document.querySelectorAll('#slotList .chip-fields')]
      .filter((f) => f.offsetParent === null).length;

    // 每一格都自带一份字段，不是「只有展开的那一格有」。
    out.fieldsPerChip = [...chips()].map((c) => c.querySelectorAll('.chip-fields').length);
    out.fieldsInPage = document.querySelectorAll('#slotList .chip-fields').length;

    // 竖排：一格一行。有任意两格共用同一个 offsetTop 就说明还是横排的。
    out.rowsUsed = new Set([...chips()].map((c) => c.offsetTop)).size;

    // 第 2 格前面有东西，所以它有「分隔符」下拉；前缀后缀各一个输入框。
    out.sepInSecond = chips()[1].querySelectorAll('.chip-fields select').length;
    out.inputsInSecond = chips()[1].querySelectorAll('.chip-fields input').length;

    // 第 1 格前面没有东西，分隔符对它没意义，不该画出来。
    out.sepInFirst = chips()[0].querySelectorAll('.chip-fields select').length;

    // ---- 焦点外框 ----
    //
    // 原来给行里**每个控件**都画了自己的焦点框，而控件比行矮、还偏在一侧，
    // 于是看起来是「行框缺了一块」。现在焦点反馈交给整行
    // （.slotchip:focus-within），所以要验的是：行里的控件拿到焦点之后，
    // 这一行的边框颜色真的变了，而且只有这一行变。
    const ring = (node) => getComputedStyle(node).borderTopColor;
    out.ringDim = ring(chips()[1]);
    chips()[1].querySelector('.chip-fields input').focus();
    out.ringLit = ring(chips()[1]);
    out.focusRingOnRow = out.ringDim !== out.ringLit;
    out.otherRowUntouched = ring(chips()[0]) === out.ringDim;
    document.activeElement.blur();
    out.ringRestored = ring(chips()[1]) === out.ringDim;

    // ---- 「这一段被选中」要看得出来 ----
    //
    // 用户报的「块选中时样式有问题」。原来只有 `border-color: var(--accent)`
    // 一条 —— 1px 的蓝线在 #0f1115 底上几乎看不出变化。
    // 现在整行四层一起表达：左边一条 3px 竖条 + 外框 + 底色 + 外发光，
    // 所以要验的是「不止外框变了」：竖条和底色都得跟着变。
    const chip2 = chips()[1];
    const barDim = getComputedStyle(chip2).borderLeftColor;
    const bgDim = getComputedStyle(chip2).backgroundColor;
    const field = chip2.querySelector('.chip-fields input');
    field.focus();
    const litStyle = getComputedStyle(chip2);
    out.chipLeftBar = litStyle.borderLeftWidth;
    out.chipBarDim = barDim;
    out.chipBarLit = litStyle.borderLeftColor;
    out.chipBgDim = bgDim;
    out.chipBgLit = litStyle.backgroundColor;

    // 最要紧的一条：小输入框**聚焦时不能丢边框**。
    //
    // 踩过的坑：`.slotchip select:focus { border: none }` 写的是后代选择器，
    // 连 chip-fields 里那几个小控件一起命中 —— `border: none` 把 border-style
    // 也清成了 none，而下面 `.chip-fields ... :focus { border-color }` 只改颜色、
    // 改不回样式。表现就是「一点进那个小框，它的边直接消失」，
    // 看着像框缺了一块。现在这条收成了 `>`，只作用于行里的直接子控件。
    const fieldStyle = getComputedStyle(field);
    out.fieldBorderStyle = fieldStyle.borderTopStyle;
    out.fieldBorderWidth = fieldStyle.borderTopWidth;
    out.fieldBorderColor = fieldStyle.borderTopColor;
    out.fieldKeepsBorder =
      fieldStyle.borderTopStyle === 'solid' && parseFloat(fieldStyle.borderTopWidth) > 0;
    document.activeElement.blur();

    // 换位 / 删除之后每一格仍然自带字段。重画是整行重建的，
    // 很容易出现「只有第一格有字段」这种半截状态 —— 截图上看不出来。
    const moveKey = keyOf(0);
    moveSlotTo(0, 1);
    out.swappedKey = keyOf(1) === moveKey;
    out.fieldsAfterMove = [...chips()].every((c) => c.querySelector('.chip-fields'));

    chips()[0].querySelector('.chip-x').click();
    out.fieldsAfterDelete = [...chips()].every((c) => c.querySelector('.chip-fields'));

    // 预览要真的画出来了（后端算的，这里是桩）。
    out.previewRows = document.querySelectorAll('#tplPreview .row').length;

    document.documentElement.setAttribute('data-slot-probe', JSON.stringify(out));
  }

  // 等两个条件都满足再跑：boot 结束、且预览已经画出来。
  //
  // 不能用固定延时。--dump-dom 的时机是「加载完就拍」，踩过一次：
  // 这里写 setTimeout(run, 60)，而截图脚本那一次 dump 发生在 60ms 之前，
  // 于是探针属性根本没来得及写上去 —— 报出来的是「探针没跑完」，
  // 看起来像脚本报错，实际只是等太久。
  // 改成盯着「数据齐了没有」，齐了就立刻跑，一般 10ms 内就能落地。
  let tries = 0;
  (function tick() {
    const booted = document.documentElement.dataset.boot === 'done';
    const previewed = document.querySelectorAll('#tplPreview .row').length > 0;
    if ((booted && previewed) || tries++ > 400) {
      run();
      return;
    }
    setTimeout(tick, 10);
  })();
})();
"""


# 记录页的勾选 / 批量还原 / 批量移除 / 筛选 / 搜索 / shift 连选。
# 只在 #history+histprobe 时跑。
#
# 要守的是几件截图看不出来的事：
#   * 三种状态的记录**都能勾**（移除记录对它们一样有效），
#     但只有「可还原」的能进「还原所选」；
#   * 按钮上的数字必须等于真正会动的条数，而且随筛选 / 搜索收窄；
#   * 全选框只作用于当前筛出来的那批；
#   * 搜不到东西时说的是「没有符合条件的记录」，不是「还没有改名记录」。
HISTORY_PROBE_JS = """
(function () {
  if (!location.hash.split('+').includes('histprobe')) return;

  function run() {
    // histpick 只为了截图：把能勾的都勾上，好看清两颗按钮上的数字
    // 和表头那颗全选框的状态。
    if (location.hash.split('+').includes('histpick')) {
      document.getElementById('histAll').click();
      return;
    }

    // histempty 跑在「一条记录都没有」那一版页面上。它和「筛完没剩下」是
    // 两种说法，混成一句的话，用户打错一个字会以为记录被清空了。
    if (location.hash.split('+').includes('histempty')) {
      document.documentElement.setAttribute('data-hist-probe', JSON.stringify({
        emptyRows: document.querySelectorAll('#histBody tr').length,
        emptyWrapHidden:
          getComputedStyle(document.getElementById('histWrap')).display === 'none',
        emptyShown:
          getComputedStyle(document.getElementById('histEmpty')).display !== 'none',
        emptyTitle: document.getElementById('histEmptyTitle').textContent,
        emptyCount: document.getElementById('histCount').textContent,
        emptyButtonDisabled: document.getElementById('btnUndoSelected').disabled,
        emptyRemoveDisabled: document.getElementById('btnRemoveHistory').disabled,
        emptyPagerHidden:
          getComputedStyle(document.getElementById('histPagerBar')).display === 'none',
      }));
      return;
    }

    const out = {};
    const rows = () => document.querySelectorAll('#histBody tr');
    const boxes = () => [...document.querySelectorAll('#histBody input[type=checkbox]')];
    const button = document.getElementById('btnUndoSelected');
    const remove = document.getElementById('btnRemoveHistory');
    const all = document.getElementById('histAll');
    const filter = document.getElementById('histFilter');
    const search = document.getElementById('histSearch');
    // 可还原 = 那一行有「还原」按钮。以前这里看的是「勾选框禁没禁用」，
    // 现在三种状态都能勾（移除记录对它们一样有效），只能看按钮。
    const restorable = () =>
      [...rows()].filter((tr) => tr.children[5].querySelector('button'));
    const boxAt = (i) => rows()[i].querySelector('input[type=checkbox]');
    // 把勾选推到一个确定的状态：先全取消，再全选。
    // （indeterminate 时点一下会变成「已勾」，所以要循环。）
    const pickAll = () => {
      while (all.checked || all.indeterminate) all.click();
      all.click();
    };
    const clearAll = () => {
      while (all.checked || all.indeterminate) all.click();
    };

    out.rows = rows().length;
    out.checkboxes = boxes().length;
    // 5 条全都可勾 —— 勾选是记录级的，不是「可还原」级的。
    out.enabledBoxes = boxes().filter((b) => !b.disabled).length;
    out.restorableRows = restorable().length;
    out.buttonAtStart = button.textContent;
    out.disabledAtStart = button.disabled;
    out.removeAtStart = remove.textContent;
    out.removeDisabledAtStart = remove.disabled;
    out.countAtStart = document.getElementById('histCount').textContent;

    // ---- 被手动改过名的文件 ----
    //
    // 桩里第 4 条的 nowPath 和记录里的现名不一样，模拟「用户在资源管理器里
    // 改过名」。这一格必须显示文件**实际**在的名字，并且挂上「已改名」的小标 ——
    // 显示记录里那个（磁盘上不存在的）名字，用户会以为文件没了。
    out.movedTags = document.querySelectorAll('#histBody .moved').length;
    const movedRow = [...rows()].find((tr) => tr.querySelector('.moved'));
    out.movedShownName = movedRow
      ? movedRow.children[3].firstChild.textContent.trim() : '';
    out.movedBadgeText = movedRow
      ? movedRow.querySelector('.moved').textContent : '';
    // 已还原的那一条 nowPath 当然也和记录里的现名不一样（文件被改回原名了），
    // 但它不是「被外部改过名」，不该挂小标。
    out.movedOnRestored = [...rows()].some(
      (tr) => tr.querySelector('.moved')
        && tr.textContent.indexOf('已还原') >= 0);

    // 全选 → 5 条**全部**被勾上（勾选是记录级的），但「还原所选」只数
    // 其中可还原的那 3 条 —— 两颗按钮认的不是同一批，数字也就该不一样。
    all.click();
    out.pickedByAll = boxes().filter((b) => b.checked).length;
    out.allChecked = all.checked;
    out.buttonAfterAll = button.textContent;
    out.removeAfterAll = remove.textContent;
    all.click();
    out.pickedAfterClear = boxes().filter((b) => b.checked).length;
    out.buttonAfterClear = button.textContent;
    out.disabledAfterClear = button.disabled;
    out.removeAfterClear = remove.textContent;
    out.removeDisabledAfterClear = remove.disabled;

    // 单独勾一条 → 按钮亮起，表头那颗全选框跟着变（只有部分勾选时是 indeterminate）。
    restorable()[0].querySelector('input[type=checkbox]').click();
    out.buttonAfterOne = button.textContent;
    out.enabledAfterOne = !button.disabled;
    out.allIndeterminate = all.indeterminate;

    // ---- shift 连选 ----
    //
    // 一屏几百条时一条条点不现实，而「全选」又太粗 —— 用户往往只要中间一段。
    // 从第 1 行 shift 点到第 3 行，三行都要勾上；再用 shift 点回第 1 行
    // （这时锚点已经落在第 3 行上）应当把这一段一起清掉。
    //
    // **不要手动设 box.checked**：浏览器在派发 click 之前会先把这一格的勾选
    // 状态翻过去（pre-click activation，对 dispatchEvent 出来的 click 一样生效），
    // 手动设的值正好被它翻掉 —— 于是「shift 点一下」变成了「shift 取消一下」，
    // 三条一个都没勾上。想要哪一格变成什么状态，只能靠它现在是什么状态。
    clearAll();
    boxAt(0).click();          // 勾上第 1 行，锚点落在这一格
    const shiftClick = (i) => {
      boxAt(i).dispatchEvent(new MouseEvent('click', { bubbles: true, shiftKey: true }));
    };
    shiftClick(2);             // shift 点第 3 行 → 第 1..3 行一起勾上
    out.shiftPicked = boxes().filter((b) => b.checked).length;
    out.shiftAnchorKept = all.indeterminate;
    shiftClick(0);             // 锚点已在第 3 行 → 这一下取消的是整段
    out.shiftCleared = boxes().filter((b) => b.checked).length;

    // ---- 勾选与筛选的联动 ----
    //
    // 全页最危险的地方在这儿：勾了五行、筛掉四行，按钮却还写着 5 ——
    // 按下去五行全被动，其中四行用户根本看不见。
    filter.value = 'all';
    filter.dispatchEvent(new Event('change'));
    pickAll();                                    // 勾上全部 5 条
    out.pickedBeforeFilter = boxes().filter((b) => b.checked).length;
    out.buttonBeforeFilter = button.textContent;
    out.removeBeforeFilter = remove.textContent;

    // 切到「可还原」：那三条还在屏幕上，勾选留着，不必让用户重新勾一遍。
    filter.value = 'undoable';
    filter.dispatchEvent(new Event('change'));
    out.undoableRows = rows().length;
    out.pickedOnUndoable = boxes().filter((b) => b.checked).length;

    // 切到「已还原」：刚才那三条可还原的全被筛掉了。
    // 屏幕上剩下的这一条是「已还原」的 —— 它能勾（移除记录用得上），
    // 但绝不能进「还原所选」，那颗按钮必须熄掉。
    filter.value = 'restored';
    filter.dispatchEvent(new Event('change'));
    out.restoredRows = rows().length;
    out.pickedAfterFilter = boxes().filter((b) => b.checked).length;
    out.buttonAfterFilter = button.textContent;
    out.disabledAfterFilter = button.disabled;
    out.removeAfterFilter = remove.textContent;

    filter.value = 'missing';
    filter.dispatchEvent(new Event('change'));
    out.missingRows = rows().length;

    filter.value = 'all';
    filter.dispatchEvent(new Event('change'));
    out.backToAllRows = rows().length;
    // 勾选一直留着（被筛掉的那几条也没丢）—— 这是刻意的：
    // 清掉它们对安全没有帮助（安全靠下面那个「目标从可见行推导」），
    // 只会让用户白勾一遍。
    out.pickedBackToAll = boxes().filter((b) => b.checked).length;

    // 搜索同样收窄可见范围：勾上五条，再搜一个只命中别处的词。
    pickAll();
    out.pickedBeforeSearch = boxes().filter((b) => b.checked).length;
    search.value = 'clip';
    search.dispatchEvent(new Event('input'));
    out.searchRows = rows().length;
    out.searchCount = document.getElementById('histCount').textContent;
    out.pickedAfterSearch = boxes().filter((b) => b.checked).length;
    out.buttonAfterSearch = button.textContent;
    out.removeAfterSearch = remove.textContent;

    // 搜不到：表格收起，空状态说的是「没有符合条件的记录」而不是「还没有记录」。
    search.value = 'zzz-查不到';
    search.dispatchEvent(new Event('input'));
    out.noMatchRows = rows().length;
    out.noMatchWrapHidden =
      getComputedStyle(document.getElementById('histWrap')).display === 'none';
    out.noMatchTitle = document.getElementById('histEmptyTitle').textContent;

    // 清空搜索 → 回到全部。
    search.value = '';
    search.dispatchEvent(new Event('input'));
    out.rowsAtEnd = rows().length;

    // ---- 「按下去到底会动哪些文件」----
    //
    // 前面那些断言看的都是 DOM，而 DOM 只反映看得见的行 —— 它证明不了
    // 勾选集合里没有残留一条已经被筛掉的路径。所以这里把 UndoMany 换成
    // 一个只记参数的桩，然后真的按一下，看后端收到什么。
    //
    // 造的场景正是最危险的那种：在「全部」下勾上三条，再搜一个只命中其中一条的词。
    // 屏幕上只有一行是勾的、按钮写着「还原所选（1）」，那按下去也只能动一条。
    filter.value = 'all';
    filter.dispatchEvent(new Event('change'));
    search.value = '';
    search.dispatchEvent(new Event('input'));

    pickAll();
    out.pickedBeforeSend = boxes().filter((b) => b.checked).length;

    // 只命中第一条（现名 Back In Black.mp4）。
    search.value = 'back in black';
    search.dispatchEvent(new Event('input'));
    out.visiblePickedBeforeSend = boxes().filter((b) => b.checked).length;
    out.buttonBeforeSend = button.textContent;

    let sent = null;
    App.UndoMany = (to) => {
      sent = to;
      return Promise.resolve({ restored: 0, missing: 0, skipped: 0, details: [] });
    };
    button.click();
    out.sent = sent ? sent.length : 'null';
    out.sentNames = sent ? sent.map((p) => String(p).split(/[\\/]/).pop()).join(' | ') : '';

    // ---- 移除所选送出去的是哪些 ----
    //
    // 和上面同一个道理：按钮上写着几，后端就必须收到几条。
    // 而且移除**不筛 canRestore** —— 「已还原」「已不在」的记录一样要能被清掉，
    // 它们往往才是最该清理的（已经没事可做了）。
    //
    // 这里把 RemoveHistory 也换成一个只记参数的桩，然后真的按一下。
    // 它带确认弹窗，所以要先点「确定」—— 顺便把弹窗的文案也记下来。
    filter.value = 'all';
    filter.dispatchEvent(new Event('change'));
    search.value = '';
    search.dispatchEvent(new Event('input'));
    pickAll();
    out.pickedBeforeRemove = boxes().filter((b) => b.checked).length;

    let dropped = null;
    App.RemoveHistory = (targets) => {
      dropped = targets;
      return Promise.resolve([]);
    };
    remove.click();

    // 确认弹窗是自绘的：先读文案，再点确定。
    const mask = document.getElementById('modalMask');
    out.removeAsked = !mask.classList.contains('hidden');
    out.removeBody = document.getElementById('modalBody').textContent;
    out.removeOkLabel = document.getElementById('modalOk').textContent;
    out.removeIsDanger = document.getElementById('modalOk').className === 'danger';
    document.getElementById('modalOk').click();

    // 「后端收到了什么」要等到 removePicked 从确认弹窗的 await 里回来才读得到
    // （它是个微任务），所以这里轮询等一下，最后才把探针结果落进 DOM。
    // 直接同步读会永远是 null —— 而那样写出来的断言是假的：它永远「通过」。
    let left = 200;
    (function settle() {
      if (dropped === null && left-- > 0) {
        setTimeout(settle, 10);
        return;
      }
      out.dropped = dropped ? dropped.length : 'null';
      out.droppedNames = dropped
        ? dropped.map((p) => String(p).split(/[\\/]/).pop()).join(' | ') : '';
      document.documentElement.setAttribute('data-hist-probe', JSON.stringify(out));
    })();
  }

  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }
    run();
  })();
})();
"""


# 分页 / 时间范围。跑在两份 120 条的大样本页面上（tasksmany / histmany）。
#
# 为什么非要有大样本：只有 8 条 / 5 条时页数永远是 1，「第 2 页画的是
# 第 51..100 条」「尾页只剩 20 条」「换每页条数要回到第 1 页」这些
# 一条都照不出来。而分页这东西出错的方式恰恰全是这种 —— 它不报错，
# 只是画错一截，用户翻到第 2 页看到重复的内容。
PAGER_PROBE_JS = """
(function () {
  const flags = location.hash.split('+');
  const wantTasks = flags.includes('taskpage');
  const wantHistory = flags.includes('histpage');
  if (!wantTasks && !wantHistory) return;

  function run() {
    const out = {};
    const ids = wantTasks
      ? { size: 'pageSize', info: 'pageInfo', prev: 'pagePrev', next: 'pageNext', body: 'tbody', name: 1 }
      : { size: 'histPageSize', info: 'histPageInfo', prev: 'histPagePrev',
          next: 'histPageNext', body: 'histBody', name: 2 };
    const el = (id) => document.getElementById(id);
    const body = el(ids.body);
    const info = el(ids.info);
    const prev = el(ids.prev);
    const next = el(ids.next);
    const sizeSel = el(ids.size);
    const count = () => body.querySelectorAll('tr').length;
    const first = () => (body.rows.length ? body.rows[0].cells[ids.name].textContent.trim() : '');
    const click = (node) => node.dispatchEvent(new MouseEvent('click', { bubbles: true }));

    out.sizeDefault = sizeSel.value;
    out.sizeOptions = [...sizeSel.options].map((o) => o.value).join(',');

    // 每页 50（默认）：120 条应当正好 3 页，尾页只有 20 条。
    out.rowsP1 = count();
    out.infoP1 = info.textContent;
    out.prevDisabledP1 = prev.disabled;
    out.nextDisabledP1 = next.disabled;
    // 第一页的第一条必须是**第 1 条**、第二页是第 51 条 ——
    // 光数行数的话，「每页都画前 50 条」这种错照样能过。
    out.firstP1 = first();

    click(next);
    out.rowsP2 = count();
    out.infoP2 = info.textContent;
    out.firstP2 = first();

    click(next);
    out.rowsP3 = count();
    out.infoP3 = info.textContent;
    out.nextDisabledP3 = next.disabled;
    out.firstP3 = first();

    // 翻回上一页：内容要跟着回来，不是停在第 3 页。
    click(prev);
    out.rowsBack = count();
    out.infoBack = info.textContent;

    // 换每页条数要回到第 1 页。停在第 3 页的话用户会看到一张
    // 从中间开始的表，而他刚刚才把每页调大。
    sizeSel.value = '300';
    sizeSel.dispatchEvent(new Event('change'));
    out.rowsSize300 = count();
    out.infoSize300 = info.textContent;

    sizeSel.value = '100';
    sizeSel.dispatchEvent(new Event('change'));
    out.rowsSize100 = count();
    out.infoSize100 = info.textContent;
    out.firstSize100 = first();

    if (wantHistory) {
      // ---- 时间范围 ----
      //
      // 样本数据固定在 2026-02（见 big_history），所以「今天」「本月」
      // 必须命中 0 条。这同时证明这两档不是「全都返回」。
      const rangeSel = el('histRange');
      const custom = el('histRangeCustom');
      const from = el('histFrom');
      const to = el('histTo');
      const wrap = el('histWrap');
      const pagerBar = el('histPagerBar');

      sizeSel.value = '50';
      sizeSel.dispatchEvent(new Event('change'));

      out.customHiddenAtStart = custom.classList.contains('hidden');

      rangeSel.value = 'today';
      rangeSel.dispatchEvent(new Event('change'));
      out.todayRows = count();
      out.todayInfo = info.textContent;
      out.todayWrapHidden = getComputedStyle(wrap).display === 'none';
      out.todayTitle = el('histEmptyTitle').textContent;
      out.todayPagerHidden = getComputedStyle(pagerBar).display === 'none';

      // 切到「自定义」才把两个日期框露出来 —— 预设档和日期框同时在场，
      // 谁说了算得靠猜。
      rangeSel.value = 'custom';
      rangeSel.dispatchEvent(new Event('change'));
      out.customShown = !custom.classList.contains('hidden');

      const iso = (ms) => {
        const d = new Date(ms);
        const p = (n) => String(n).padStart(2, '0');
        return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
      };
      const base = __BASE_TS__;

      // 只填「起」= 「从那以后」。样本里最新的那条就在这一天。
      from.value = iso(base);
      from.dispatchEvent(new Event('change'));
      out.fromOnlyRows = count();
      out.fromOnlyFirst = first();
      out.fromOnlyInfo = info.textContent;

      // 起止同一天：右端是半开区间（到次日 00:00），所以这一天之内的
      // 全都算 —— 写成闭区间的话最后那一条会被漏掉。
      to.value = iso(base);
      to.dispatchEvent(new Event('change'));
      out.sameDayRows = count();
      out.sameDayLast = body.rows.length
        ? body.rows[body.rows.length - 1].cells[ids.name].textContent.trim() : '';

      // 整个范围挪到数据之后：一条都不该剩，而且要说「没有符合条件的记录」，
      // 不是「还没有改名记录」。
      from.value = iso(base + 86400000);
      to.value = iso(base + 2 * 86400000);
      from.dispatchEvent(new Event('change'));
      out.pastRows = count();
      out.pastTitle = el('histEmptyTitle').textContent;
      out.pastWrapHidden = getComputedStyle(wrap).display === 'none';
      out.pastPagerHidden = getComputedStyle(pagerBar).display === 'none';

      // 切回「全部时间」：日期框收起来，行数回到全部。
      rangeSel.value = 'all';
      rangeSel.dispatchEvent(new Event('change'));
      out.backRows = count();
      out.customHiddenAgain = custom.classList.contains('hidden');
    }

    document.documentElement.setAttribute('data-pager-probe', JSON.stringify(out));
  }

  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }
    run();
  })();
})();
""".replace("__BASE_TS__", str(PAGER_BASE_TS))


# 这里原来有个 SLOT_OPEN_JS，用来把某一格的补充设置「点开」再截图。
# 字段现在常驻（一行一段竖着排），没有展开这个状态，拿 #templates 直接截就行。

# 字段现在常驻（一行一段竖着排），没有展开这个状态，拿 #templates 直接截就行。


MENU_PROBE_JS = """
// 模板列表的右键菜单。只在 #templates+menuprobe 时跑。
//
// 名称的改动（重命名 / 复制 / 删除）现在**只**挂在这份右键菜单上 ——
// 页面上那个「名称」输入框和底栏那两颗按钮都删掉了。所以这条路径一旦断了，
// 改名这件事就完全没有入口，而它断掉的样子是「右键没反应」，
// 不抛错、不写日志，只有手动点才会发现。
//
// 这里刻意走完整条链：右键 → 菜单 → 弹窗填字 → 后端 → 列表重画。
// 只断言「菜单弹出来了」是不够的：真正的风险在弹窗和后端那两步。
(async function () {
  if (!location.hash.split('+').includes('menuprobe')) return;

  // 等 boot()：列表是 loadTemplates() 之后才画出来的。
  for (let i = 0; i < 400 && document.documentElement.dataset.boot !== 'done'; i++) {
    await new Promise((r) => setTimeout(r, 10));
  }

  const out = {};
  const items = () => document.querySelectorAll('#tplList .tpl-item');
  // 容器是 HTML 里预置的，所以「弹出来了吗」要看它是不是被藏着，
  // 不能看它在不在 —— 它一直在。
  const menu = () => {
    const node = document.getElementById('ctxMenu');
    return node && !node.classList.contains('hidden') ? node : null;
  };
  const entries = () => (menu() ? [...menu().querySelectorAll('button')] : []);
  const names = () => [...items()].map((n) => n.querySelector('b').textContent.trim());
  const current = () => {
    const on = document.querySelector('#tplList .tpl-item.on b');
    return on ? on.textContent.trim() : '';
  };
  const openOn = (index) => {
    items()[index].dispatchEvent(new MouseEvent('contextmenu', {
      bubbles: true, cancelable: true, clientX: 40, clientY: 120,
    }));
  };
  const pick = (label) => entries().find((b) => b.textContent === label).click();

  // 推进异步链**只能刷微任务，不能用 setTimeout**。
  //
  // 无头 Chrome 的 --dump-dom 在 load 事件一触发就把 DOM 拍下来，
  // 之后定时器不再推进。探针开头那个等 boot 的循环能用 setTimeout，
  // 是因为那时页面还在解析、load 还没来；一旦走到这一句之后再用，
  // 后续步骤就永远跑不到 —— 表现是「属性没写上去」，看着像脚本报错。
  // 桩里每个方法都返回已经 resolve 的 Promise，所以刷微任务就够了。
  const flush = async (done) => {
    for (let i = 0; i < 200; i++) {
      if (done && done()) return;
      await Promise.resolve();
    }
  };

  out.items = items().length;
  out.namesBefore = names().join(' | ');

  // ---- 菜单本身 ----
  openOn(1);
  out.menuShown = !!menu();
  out.labels = entries().map((b) => b.textContent).join(' | ');
  // 右键顺手选中那一份：菜单里三项都是对着它做的，
  // 而右键落在没选中的那一份上时，用户多半以为已经选上了。
  out.selectedIsRightClicked = items()[1].classList.contains('on');
  out.deleteEnabled = entries().some((b) => b.textContent === '删除' && !b.disabled);
  // 菜单不能被窗口裁掉：点最下面一行时它得往上翻，否则看不见的那几项等于没有。
  const rect = menu().getBoundingClientRect();
  out.menuInsideViewport =
    rect.left >= 0 && rect.top >= 0
    && rect.right <= window.innerWidth && rect.bottom <= window.innerHeight;

  // 点别处 / Esc 都要收起。收不起来的话它就一直挂在最上层，
  // 挡住底下的按钮 —— 而用户以为自己只是点了一下空白。
  document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
  out.closedByOutsideClick = !menu();
  openOn(1);
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  out.closedByEscape = !menu();

  // ---- 重命名 ----
  openOn(1);
  const original = names()[1];
  pick('重命名');
  const input = document.getElementById('modalInput');
  out.renameDialogShown =
    !document.getElementById('modalField').classList.contains('hidden')
    && !document.getElementById('modalMask').classList.contains('hidden');
  out.renamePrefill = input.value;
  out.renameFocusIsInput = document.activeElement === input;

  input.value = '改过名的模板';
  document.getElementById('modalOk').click();
  await flush(() => names().indexOf('改过名的模板') >= 0);
  out.namesAfterRename = names().join(' | ');
  // 改名字，不是「存一份新的再删旧的」—— 位置和条数都不该变。
  out.renamedInPlace = items().length === out.items && names().indexOf(original) < 0;
  // 编辑器要跟着换过去。不换的话 editing 指向一个不存在的名字：
  // 底栏一直写「有未保存的改动」，按「保存」会把旧名字重新创建出来 ——
  // 用户以为改了名，结果是多了一份。
  out.editingFollowedRename = current() === '改过名的模板';

  // ---- 复制 ----
  openOn(0);
  const countBefore = items().length;
  pick('复制');
  out.dupPrefill = input.value;
  document.getElementById('modalOk').click();
  await flush(() => items().length !== countBefore);
  out.dupAdded = items().length - countBefore;
  out.dupNames = names().join(' | ');
  // 复制完切到新的那一份：复制多半就是为了改它，
  // 留在原地的话用户还得回列表里找一遍。
  out.dupSelected = current() === out.dupPrefill;

  // ---- 删除 ----
  const countBeforeDelete = items().length;
  openOn(items().length - 1);
  pick('删除');
  await flush();
  // 删东西必须过一道确认框。不过的话右键一滑就少一份模板，没有回头路。
  out.deleteConfirmShown = !document.getElementById('modalMask').classList.contains('hidden');
  document.getElementById('modalOk').click();
  await flush(() => items().length !== countBeforeDelete);
  out.deleteRemoved = countBeforeDelete - items().length;

  // ---- 只剩一份时「删除」要置灰 ----
  for (let guard = 0; guard < 20 && items().length > 1; guard++) {
    const n = items().length;
    openOn(0);
    pick('删除');
    await flush();
    document.getElementById('modalOk').click();
    await flush(() => items().length !== n);
  }
  out.itemsAtEnd = items().length;
  openOn(0);
  // 后端也会拒绝删最后一份，但「按下去才报错」不如一开始就是灰的。
  out.deleteDisabledOnLast =
    entries().some((b) => b.textContent === '删除' && b.disabled);
  document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));

  document.documentElement.setAttribute('data-menu-probe', JSON.stringify(out));
})();
"""


MENU_SHOT_JS = """
// 只为了截图：把模板列表的右键菜单弹出来。只在 #templates+ctxmenu 时跑。
//
// 菜单只在右键时出现，而截图脚本没法「右键」—— 不专门弹一次的话，
// 这块新 UI 从没被看见过，配色和边距只能靠想象。
//
// 这里**必须刷微任务，不能等 setTimeout**。截图在 load 事件一触发就拍下了
// （同 SWITCH_JS 那条注释），而 setTimeout 排在 load 后面 —— 菜单还没弹出来
// 图就拍完了，表现是「截图里干干净净、也没有报错」，看着像菜单没做出来。
// 桩里每个方法都返回已经 resolve 的 Promise，所以 boot() 整条链都是微任务，
// 刷微任务就能赶在 load 之前跑完。
(async function () {
  if (!location.hash.split('+').includes('ctxmenu')) return;

  const ready = () => document.documentElement.dataset.boot === 'done'
    && document.querySelectorAll('#tplList .tpl-item').length > 1;
  for (let i = 0; i < 400 && !ready(); i++) await Promise.resolve();
  if (!ready()) return;

  const items = document.querySelectorAll('#tplList .tpl-item');
  // 点在行自己的位置附近，这样截图里的相对位置和真机上一样。
  const rect = items[1].getBoundingClientRect();
  items[1].dispatchEvent(new MouseEvent('contextmenu', {
    bubbles: true, cancelable: true,
    clientX: Math.round(rect.left + 70), clientY: Math.round(rect.bottom),
  }));

  // 菜单已经在 DOM 里了，但 --screenshot 拍不到它 —— 那一帧就是不画这个
  // 刚建出来的浮层（元素有布局、getBoundingClientRect 正常、命中测试也正常）。
  //
  // 这不是菜单做错了：拿 CDP 的 Page.captureScreenshot 拍同一个页面，
  // 菜单显示得好好的（临时脚本比对过）。也排除了「没弹出来」「弹出来又被关掉」
  // 「被别的元素盖住」这几种可能 —— 逐条验证的经过见下面这段的对照实验：
  //   预置容器 vs 每次新建、去 focus、去夹边、改 z-index/阴影/display/position、
  //   强制重绘、--headless=new、--run-all-compositor-stages-before-draw，
  //   都拍不到；而**新插入**的元素画得出来。
  //
  // 所以这里把菜单原样搬一份到新元素上再收掉原件：class、内容、位置完全一样，
  // 图里看到的就是真菜单的样子。行为对不对由 menuprobe 那条断言保证，
  // 不靠这张图。
  const menu = document.getElementById('ctxMenu');
  if (menu) {
    const shell = menu.cloneNode(true);
    shell.removeAttribute('id');
    shell.style.left = menu.style.left;
    shell.style.top = menu.style.top;
    menu.remove();
    document.body.appendChild(shell);
  }
})();
"""


SLOTFOCUS_SHOT_JS = """
// 只为了截图：把命名区第 2 段的某个控件点着，好看清「这一段被选中」长什么样。
// 只在 #templates+slotfocus 时跑。
//
// 为什么必须单独截一张：选中态是**交互之后才有**的，静态截图里永远看不到它。
// 而这正是用户报的那个毛病（「块选中时样式有问题」）—— 修完了没人看得见，
// 下一轮改 CSS 时也没人知道碰坏了什么。
//
// 和 MENU_SHOT_JS 一样**刷微任务、不等 setTimeout**：截图在 load 一触发就拍了。
(async function () {
  const parts = location.hash.split('+');
  if (!parts.includes('slotfocus')) return;

  // 默认点「这一段放什么」那个下拉；带 focus:field 时改点右边补充设置里的小输入框 ——
  // 那个小框的边框曾经一聚焦就消失（`.slotchip select:focus { border: none }`
  // 是后代选择器，连它一起命中了），单独留一张对照。
  const which = parts.find((p) => p.startsWith('focus:')) ? 'field' : 'select';

  const ready = () => document.documentElement.dataset.boot === 'done'
    && document.querySelectorAll('#slotList .slotchip').length > 1;
  for (let i = 0; i < 400 && !ready(); i++) await Promise.resolve();
  if (!ready()) return;

  const chip = document.querySelectorAll('#slotList .slotchip')[1];
  const target = which === 'field'
    ? chip.querySelector('.chip-fields input')
    : chip.querySelector('select');
  if (target) target.focus();
})();
"""


DRAG_PROBE_JS = """
// 拖放遮罩只认**文件**。只在 #tasks+dragprobe 时跑。
//
// 用户报的原话：「我发现在界面拖动文本，或一些可拖动的东西都会显示
// 『松开鼠标，把音乐/视频加进队列』」。根因是 dragenter 完全不看
// dataTransfer 里是什么，而 Chromium 拖文本、拖槽位排序一样会发 dragenter。
//
// 截图证不了这件事：遮罩是整页盖一层，截出来只是「有一张盖着」，
// 而截图那一次永远没有在拖东西。
(function () {
  if (!location.hash.split('+').includes('dragprobe')) return;

  const zone = document.getElementById('dropzone');
  const on = () => zone.classList.contains('on');
  const fire = (type, transfer) => window.dispatchEvent(
    new DragEvent(type, { dataTransfer: transfer, bubbles: true, cancelable: true }));

  const out = {};

  // 1. 拖一段被选中的文本 —— 不该亮。这就是用户报的那一种。
  const text = new DataTransfer();
  text.setData('text/plain', '一段被选中的文字');
  // 这一行是整条判断的依据：Chromium 拖文本时 types 里**也有** text/plain，
  // 所以「按 types 判断」必然误判，只能看 items 里的 kind。
  out.typesForText = [...text.types].join(',');
  fire('dragenter', text);
  out.onForText = on();
  fire('dragleave', text);
  out.offAfterTextLeave = !on();

  // 2. 拖槽位排序。makeDraggable 也是 setData('text/plain', ...)，
  //    和上面同一类，但它证明的是「本程序自己的拖拽也不会误触发」。
  const slot = new DataTransfer();
  slot.setData('text/plain', '0');
  fire('dragenter', slot);
  out.onForSlotDrag = on();
  fire('dragleave', slot);

  // 3. 真的拖一个文件进来 —— 必须亮，否则用户不知道该松手了。
  const files = new DataTransfer();
  files.items.add(new File(['x'], 'clip.mp4', { type: 'video/mp4' }));
  out.typesForFile = [...files.types].join(',');
  fire('dragenter', files);
  out.onForFile = on();
  // 从子元素上划过会再发一次 dragenter，得靠计数撑住；
  // 对应地 dragleave 也要发两次才该收起来。
  fire('dragenter', files);
  fire('dragleave', files);
  out.stillOnAfterInnerLeave = on();
  fire('dragleave', files);
  out.offAfterFileLeave = !on();

  // 4. 落下之后一定得收起来，不管手里是什么。
  fire('dragenter', files);
  fire('drop', files);
  out.offAfterDrop = !on();

  document.documentElement.setAttribute('data-drag-probe', JSON.stringify(out));
})();
"""


SCROLLBAR_PROBE_JS = """
// 滚动条必须是自己画的那一套，不是原生的。只在 #tasks+scrollprobe 时跑。
//
// 这条**只能靠 DOM 量**：截图脚本带 --hide-scrollbars（不带的后果不只是每张图右边
// 多一根条 —— 条的有无取决于内容高度，而内容高度逐页不同，那几张图的 md5
// 比对就废了），所以截图里永远看不到滚动条，也永远证明不了它长什么样。
//
// 原生那根在深色主题上是一块浅灰，和面板的 #161920 差着好几个明度 ——
// 它不像界面的一部分，像界面里嵌了另一个程序。所以量的是「有没有换成自己画的」：
// 条宽、滑块颜色、圆角、轨道透明，四条一起对上才算数。
//
// 同时守**反方向**：模板页的 #tplScroll 和预览框 .preview 是**故意**把滚动条
// 藏掉的（那一页要一屏看得全，见那边的 CSS 注释）。全局那套一旦写得不够具体
// （比如挂到 * 上），就会把它们一起变回 10px —— 那两条断言就是防这个的。
(function () {
  if (!location.hash.split('+').includes('scrollprobe')) return;

  const probe = (node) => {
    if (!node) return null;
    return {
      width: getComputedStyle(node, '::-webkit-scrollbar').width,
      track: getComputedStyle(node, '::-webkit-scrollbar-track').backgroundColor,
      thumb: getComputedStyle(node, '::-webkit-scrollbar-thumb').backgroundColor,
      radius: getComputedStyle(node, '::-webkit-scrollbar-thumb').borderRadius,
    };
  };

  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }

    const out = { pane: probe(document.querySelector('.pane')) };

    // 那两处要切到模板页再量。切视图是同步的（只切 .hidden），切完立刻能读。
    const nav = [...document.querySelectorAll('.nav')]
      .find((n) => n.dataset.view === 'templates');
    if (nav) nav.onclick();

    out.tplScroll = probe(document.getElementById('tplScroll'));
    out.preview = probe(document.querySelector('#view-templates .preview'));
    document.documentElement.setAttribute('data-scrollbar-probe', JSON.stringify(out));
  })();
})();
"""

EMPTY_PROBE_JS = """
// 空队列的互斥断言。只在 #tasks+emptyprobe 时跑，而且要跑在「空 items」那一版页面上。
//
// 「表格」和「空状态」是互斥的两条渲染分支。同时显示就是两个面板各带一条 1px 边框，
// 用户看到的是「中间多了一条分割线，面板像被切成两半」——
// 截图能看出不好看，但看不出到底是哪个元素多出来了，所以这里量「恰好一个可见」。
(function () {
  if (!location.hash.split('+').includes('emptyprobe')) return;

  const visible = (id) => {
    const node = document.getElementById(id);
    if (!node) return false;
    const cs = getComputedStyle(node);
    return cs.display !== 'none' && cs.visibility !== 'hidden';
  };

  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }
    document.documentElement.setAttribute('data-empty-probe', JSON.stringify({
      boot: document.documentElement.dataset.boot || '',
      tableVisible: visible('tablewrap'),
      emptyVisible: visible('empty'),
      rows: document.querySelectorAll('#tbody tr').length,
    }));
  })();
})();
"""


NUMBER_PROBE_JS = """
// 数字输入框的增减按钮。只在 #settings+numprobe 时跑。
//
// 守两件事：
//   1. 每个 number 输入都被换成了自己画的 − / +（新增一个输入框忘了处理就会露馅）；
//   2. 点 + 真的加 1，而且**派发了 input 事件** —— 直接改 .value 不会触发监听器，
//      这些框是靠 oninput 去 markSettingsDirty() 的，不派发就会出现
//      「值改了但底栏还写着已保存」，保存下去的还是旧值。
(function () {
  if (!location.hash.split('+').includes('numprobe')) return;

  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }

    const out = {};
    const inputs = [...document.querySelectorAll('input[type=number]')];
    out.count = inputs.length;
    out.wrapped = inputs.filter((i) => i.closest('.numwrap')).length;
    out.withTwoButtons = inputs.filter((i) => {
      const box = i.closest('.numwrap');
      return box && box.querySelectorAll('.numbtn').length === 2;
    }).length;

    const probe = document.getElementById('cfgSegment');
    if (probe) {
      const style = getComputedStyle(probe);
      out.appearance = style.appearance || style.webkitAppearance || '';

      const before = probe.value;
      let sawInput = false;
      probe.addEventListener('input', () => { sawInput = true; }, { once: true });

      const buttons = probe.closest('.numwrap').querySelectorAll('.numbtn');
      buttons[1].click();
      out.incremented = Number(probe.value) === Number(before) + 1;
      out.sawInput = sawInput;

      // 到顶之后 + 必须置灰，否则点着没反应，看起来像坏了。
      probe.value = probe.max;
      buttons[1].click();
      out.plusDisabledAtMax = buttons[1].disabled;
      // 到底之后 − 必须置灰。
      probe.value = probe.min;
      buttons[0].click();
      out.minusDisabledAtMin = buttons[0].disabled;

      probe.value = before;
      probe.dispatchEvent(new Event('input', { bubbles: true }));
    }

    document.documentElement.setAttribute('data-number-probe', JSON.stringify(out));
  })();
})();
"""


SELECT_PROBE_JS = """
// 下拉框弹出列表的配色。只在 #templates+selectprobe 时跑。
//
// 踩过的坑：Chromium 画选项列表时，列表底色是从 select 自己的
// background-color 推的。select 写成 transparent 就推不出颜色，于是退回
// 系统浅色默认值 —— 白底；而选项文字继承的是 select 的 color（浅色），
// 结果是白底白字，几乎看不见。这跟 color-scheme 无关，:root 上写 dark 也救不了。
//
// 弹出列表本身是**独立绘制的层**，--screenshot 和 --dump-dom 都抓不到，
// 所以这里钉住它的**因**：任何一个 select 都不许是透明的。
(function () {
  if (!location.hash.split('+').includes('selectprobe')) return;

  let tries = 0;
  (function tick() {
    if (document.documentElement.dataset.boot !== 'done' && tries++ < 400) {
      setTimeout(tick, 10);
      return;
    }

    const rows = [];
    for (const el of document.querySelectorAll('select')) {
      const style = getComputedStyle(el);
      const background = style.backgroundColor || '';
      rows.push({
        name: el.title || el.id || '(无标识)',
        parent: el.parentElement ? el.parentElement.className : '',
        background: background,
        // 只认这两种写法：Chromium 两种都报过。
        transparent: background === 'transparent' ||
                     background === 'rgba(0, 0, 0, 0)',
        options: el.options.length,
      });
    }

    document.documentElement.setAttribute('data-select-probe', JSON.stringify({
      boot: document.documentElement.dataset.boot || '',
      count: rows.length,
      transparent: rows.filter((row) => row.transparent),
    }));
  })();
})();
"""


MODAL_PROBE_JS = """
(async function () {
  // 只在 #<view>+modalprobe 时才跑：它会把弹窗开开关关好几轮，
  // 别的视图没必要陪着演一遍。
  if (!location.hash.split('+').includes('modalprobe')) return;

  const mask = document.getElementById('modalMask');
  const ok = document.getElementById('modalOk');
  const hidden = () => mask.classList.contains('hidden');
  const out = {};

  let p = askConfirm({ title: 'T', body: 'B', ok: 'O', danger: true });
  out.shownAfterCall = !hidden();
  // 危险操作默认焦点必须在「取消」上，否则一个回车就把东西删了。
  out.focusOn = document.activeElement.id;
  out.okClassWhenDanger = ok.className;
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  out.escapeResult = await p;
  out.hiddenAfterEscape = hidden();

  p = askConfirm({ title: 'T', body: 'B' });
  out.okClassWhenNormal = ok.className;
  mask.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
  out.backdropResult = await p;
  out.hiddenAfterBackdrop = hidden();

  p = askConfirm({ title: 'T', body: 'B' });
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }));
  out.enterResult = await p;

  // 点卡片**里面**不该关。
  p = askConfirm({ title: 'T', body: 'B' });
  document.querySelector('.modal').dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
  out.openAfterInnerClick = !hidden();
  document.getElementById('modalCancel').click();
  await p;

  document.documentElement.setAttribute('data-modal-probe', JSON.stringify(out));
})();
"""


LAYOUT_JS = """
// 把「骨架」的几何量写成 data-layout，交给 Python 侧断言。
//
// 为什么不能只看截图：截图能看出「歪了」，看不出「差 44px 还是差 12px」，
// 更看不出某条边框是 0 还是 1px。这几条都是明确的产品要求，
// 必须有一个改坏了就变红的检查。
(function () {
  const rect = (n) => (n ? n.getBoundingClientRect() : { top: 0, left: 0, width: 0, height: 0, right: 0 });
  const side = document.querySelector('.sidebar');
  const logo = document.querySelector('.logo img');
  const bar = document.querySelector('.titlebar');
  if (!side || !logo || !bar) {
    document.documentElement.setAttribute('data-layout', 'missing=1');
    return;
  }
  const s = rect(side), l = rect(logo), b = rect(bar);
  const cs = getComputedStyle(bar);
  document.documentElement.setAttribute('data-layout', [
    'sidebarTop=' + Math.round(s.top),
    'sidebarH=' + Math.round(s.height),
    'viewportH=' + window.innerHeight,
    'logoW=' + Math.round(l.width),
    'logoTop=' + Math.round(l.top),
    'barTop=' + Math.round(b.top),
    'barLeft=' + Math.round(b.left),
    'sidebarRight=' + Math.round(s.right),
    'barBorderBottom=' + cs.borderBottomWidth,
    'pageLayers=' + document.querySelectorAll('.page').length,
    // 窄窗口下真正会翻车的不是滚动条，是**内容被裁掉**。
    // 踩过两次：
    //   1. 量 document.scrollWidth —— .view.scroll 自带 overflow:auto，滚动条在视图内部，
    //      文档宽度纹丝不动，等于没量；
    //   2. 量 .view 的 scrollWidth —— .panel 是 overflow:hidden，超出的列直接被裁，
    //      滚动宽度也不会变，还是等于没量。
    // 所以直接量「有没有元素伸出了它所在面板的框」。
    'clipped=' + (function () {
      let bad = 0;
      const view = document.querySelector('.view:not(.hidden)');
      if (!view) return -1;
      for (const f of view.querySelectorAll('.field, button')) {
        const panel = f.closest('.panel');
        if (!panel) continue;
        const a = f.getBoundingClientRect(), b = panel.getBoundingClientRect();
        if (a.right > b.right + 1 || a.left < b.left - 1) bad++;
      }
      return bad;
    })(),
    // 禁用输入框里的字还得看得见。
    // 踩过：给 input:disabled 写了 opacity:.5，而占位符又没写颜色（走浏览器默认暗灰），
    // 两个一叠加，User-Agent 那条几乎全黑 —— 可用户恰恰要靠它知道「留空会用什么」。
    // 这里取「占位符色 / 背景色 / 元素透明度」三个值，回 Python 算对比度。
    'uaPlaceholder=' + (function () {
      const ua = document.getElementById('cfgUserAgent');
      if (!ua) return 'missing';
      const cs = getComputedStyle(ua);
      let ph = 'none';
      try { ph = getComputedStyle(ua, '::placeholder').color || 'none'; } catch (e) { ph = 'none'; }
      return ph + '|' + cs.backgroundColor + '|' + cs.opacity;
    })(),
    // 工具条和文件列表之间不能多出一条线。
    //
    // 踩过：进度条原来是 `height:2px; background:var(--panel-2); margin:8px 0 10px`，
    // 静止（进度 0）时就是一条横贯全宽的浅色线悬在两者中间，
    // 用户报「工具条和列表之间多了一条分割线，上面那块像被切成两个面板」。
    // 它什么都没表示，却把布局切开了。
    //
    // 量三件事：进度条自己画不画底色、它跟列表上沿贴不贴、静止时填充宽度是不是 0。
    'progressBand=' + (function () {
      const pb = document.getElementById('progressBar');
      const tw = document.getElementById('tablewrap');
      if (!pb || !tw) return 'missing';
      const cs = getComputedStyle(pb);
      const fill = document.getElementById('progressFill');
      const gap = Math.round(tw.getBoundingClientRect().top - pb.getBoundingClientRect().bottom);
      const fillW = fill ? Math.round(fill.getBoundingClientRect().width) : -1;
      return cs.backgroundColor + '|' + gap + '|' + fillW;
    })(),
  ].join(';'));
})();
"""


def build_preview(dest: pathlib.Path, items: list | None = None,
                  history: list | None = None) -> None:
    """把 frontend/index.html 加上桩数据，写成可以在浏览器里打开的页面。

    `items` 传空列表就得到「空队列」那一版 —— 空状态是另一条渲染分支，
    不单独搭一份就永远测不到它（踩过：表格和空状态同时显示，
    两个面板各带一条边框，用户报「中间多了一条分割线」）。

    `history` 同理：记录页的空状态有**两种**说法 ——「一条记录都没有」和
    「筛完没剩下」。不搭一份空记录的页面，第一种就永远只是纸上的分支。
    """
    html = INDEX.read_text(encoding="utf-8")

    stub = STUB_JS
    stub = stub.replace("__STUB_ITEMS__", json.dumps(STUB_ITEMS if items is None else items, ensure_ascii=False))
    stub = stub.replace("__STUB_TEMPLATES__", json.dumps(STUB_TEMPLATES, ensure_ascii=False))
    stub = stub.replace("__STUB_TEMPLATE_OPTIONS__", json.dumps(STUB_TEMPLATE_OPTIONS, ensure_ascii=False))
    stub = stub.replace("__STUB_CONFIG__", json.dumps(STUB_CONFIG, ensure_ascii=False))
    stub = stub.replace("__STUB_HISTORY__",
                        json.dumps(STUB_HISTORY if history is None else history, ensure_ascii=False))

    # 错误陷阱单独放一个 script 块，**不能**和桩写在一起。
    # 踩过：桩里多了一个花括号 → 整块解析失败 → 连陷阱一起没注册 →
    # 界面停在初始状态，看起来像「截图时机太早」，查了半天。
    trap = (
        "<script>"
        "window.addEventListener('error',function(e){"
        "document.documentElement.setAttribute('data-js-error',e.message+' @'+(e.lineno||'?'));});"
        "window.addEventListener('unhandledrejection',function(e){"
        "document.documentElement.setAttribute('data-js-reject',String(e.reason));});"
        "</script>\n"
    )

    # 截图前把所有过渡和动画按死。
    #
    # 为什么必须这么做：boot() 会把 .nav 的 active 从「任务」挪到「设置」，
    # 而 button 上有 `transition: background .12s`（index.html 第 57 行）。
    # Chrome 的 --screenshot 是「load 完就拍」，经常正好拍在过渡中间，
    # 于是同一页两次截出来的图不一样（实测：同一 URL 出现 2 种 md5，
    # 差异集中在侧栏两个导航按钮上，颜色是过渡中的中间值）。
    #
    # 后果不只是「图不好看」：下面那条「两张图 md5 相同 = 视图没切换」的检查，
    # 前提就是截图确定 —— 抖动的截图既会误报，也会把真没切换的情况盖掉。
    # 断言的是最终状态，不是动画过程，所以这里直接按死是诚实的做法。
    freeze = (
        "<style id=\"freeze-for-screenshot\">"
        "*,*::before,*::after{"
        "transition:none!important;animation:none!important;"
        "caret-color:transparent!important;}"
        "</style>\n"
    )

    # 桩放在主脚本之前（主脚本第一行就读 window.go.main.App），
    # 切换脚本放在主脚本**之后**（那时 .nav 的 onclick 才注册好）。
    html = html.replace("<body>", "<body>\n" + trap + "<script>" + stub + "</script>\n", 1)
    # 按死过渡的样式放进 head，确保首帧之前就生效。
    html = html.replace("</head>", freeze + "</head>", 1)
    # 几何探针放最后：此时布局已经稳定（切视图只切 .hidden，不改骨架）。
    html = html.replace(
        "</body>",
        "<script>" + SWITCH_JS + "</script>\n"
        "<script>" + MODAL_PROBE_JS + "</script>\n"
        "<script>" + TEMPLATE_PROBE_JS + "</script>\n"
        "<script>" + MENU_PROBE_JS + "</script>\n"
        "<script>" + MENU_SHOT_JS + "</script>\n"
        "<script>" + SLOTFOCUS_SHOT_JS + "</script>\n"
        "<script>" + DRAG_PROBE_JS + "</script>\n"
        "<script>" + SCROLLBAR_PROBE_JS + "</script>\n"
        "<script>" + HISTORY_PROBE_JS + "</script>\n"
        "<script>" + PAGER_PROBE_JS + "</script>\n"
        "<script>" + EMPTY_PROBE_JS + "</script>\n"
        "<script>" + NUMBER_PROBE_JS + "</script>\n"
        "<script>" + SELECT_PROBE_JS + "</script>\n"
        "<script>" + LAYOUT_JS + "</script>\n</body>",
        1,
    )

    dest.write_text(html, encoding="utf-8")

    # 这两个替换一旦没生效，页面会**静默**退回到「有过渡、没探针」的状态：
    # 截图开始随机抖动、几何断言不再输出 —— 而工具照样打印「✅」。
    # 所以这里必须硬断言，不能靠 `str.replace` 默默返回原串。
    if "freeze-for-screenshot" not in html:
        raise SystemExit("注入失败：没找到 </head>，截图会受过渡影响而抖动")
    if "data-layout" not in html:
        raise SystemExit("注入失败：几何探针没进页面")
    if "slotprobe" not in html:
        raise SystemExit("注入失败：槽位探针没进页面")
    if "selectprobe" not in html:
        raise SystemExit("注入失败：下拉框探针没进页面")

    verify_scripts(html)


def verify_scripts(html: str) -> None:
    """每一段内联脚本都要先过一遍 node --check。

    这一步不是多余的：脚本解析失败时页面上**什么症状都没有**，
    只是停在初始状态。先在这里炸掉，比对着四张一样的截图猜要省事得多。
    """
    node = r"C:\Users\Majo\.workbuddy-ai\binaries\node\versions\22.22.2-3\node.exe"
    if not pathlib.Path(node).is_file():
        return

    temp = pathlib.Path(os.environ.get("TEMP", "/tmp"))
    for index, block in enumerate(re.findall(r"<script>(.*?)</script>", html, re.S)):
        path = temp / f"vsr-block-{index}.js"
        path.write_text(block, encoding="utf-8")
        result = subprocess.run([node, "--check", str(path)], capture_output=True, text=True)
        if result.returncode != 0:
            first = (result.stderr or "").strip().splitlines()
            raise SystemExit(f"第 {index} 段内联脚本语法错误：\n" + "\n".join(first[:6]))


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def find_chrome() -> str:
    for path in CHROME_CANDIDATES:
        if path and pathlib.Path(path).is_file():
            return path
    raise SystemExit("找不到 Chrome，试过：" + "、".join(CHROME_CANDIDATES))


def run_chrome(chrome: str, profile: pathlib.Path, extra: list[str], url: str) -> subprocess.CompletedProcess:
    command = [
        chrome,
        "--headless=old",
        "--disable-gpu",
        "--no-sandbox",
        "--no-proxy-server",  # 别让环境里的 http_proxy 把本机请求劫走
        "--hide-scrollbars",
        f"--user-data-dir={profile}",
        "--timeout=4000",
    ] + extra + [url]
    return subprocess.run(command, capture_output=True, text=True, timeout=120)


def view_state(chrome: str, profile: pathlib.Path, url: str, view: str, size: str = "") -> tuple[bool, str]:
    """用 dump-dom 确认这一页真的渲染成了目标视图。

    只检查截图文件大小是不够的——服务没起来时截出来是一张纯背景色的图，
    字节数照样大于阈值，看起来「成功」了。DOM 是硬证据。

    `size` 必须和稍后截图用的 `--window-size` 一致，否则几何断言量的
    是另一个视口 —— 那等于没量。
    """
    extra = ["--dump-dom"]
    if size:
        extra.insert(0, f"--window-size={size}")
    result = run_chrome(chrome, profile, extra, url)
    dom = result.stdout or ""
    if len(dom) < 5000:
        return False, f"DOM 只有 {len(dom)} 字符，页面多半没加载"

    error = re.search(r'data-js-error="([^"]*)"', dom)
    if error:
        return False, "页面脚本报错：" + error.group(1)

    reject = re.search(r'data-js-reject="([^"]*)"', dom)
    if reject:
        return False, "未处理的 Promise 拒绝：" + reject.group(1)

    # 页面自己打的「启动完成」标记。比盯着某段文案猜可靠。
    if 'data-boot="done"' not in dom:
        state = re.search(r'data-boot="([^"]*)"', dom)
        return False, "boot() 没有跑完（data-boot=%s）" % (state.group(1) if state else "缺失")

    # 目标视图不该带 hidden，其余视图都该带。
    for name in VIEWS:
        match = re.search(r'id="view-%s" class="([^"]*)"' % name, dom)
        if not match:
            return False, f"DOM 里找不到 view-{name}"
        hidden = "hidden" in match.group(1)
        if name == view and hidden:
            return False, f"view-{view} 仍然是 hidden，视图没切换"
        if name != view and not hidden:
            return False, f"view-{name} 没有隐藏"

    return check_layout(dom)


def check_modal(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """确认弹窗的行为断言。

    原生 confirm() 只有「确定 / 取消」两个结果，换成自绘弹窗之后多出三个入口：
    点遮罩、按 Esc、按 Enter。任何一个接错都会变成「点取消却执行了」这种事故，
    所以必须逐条验，不能只看截图。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-modal-probe="([^"]*)"', dom)
    if not match:
        return False, "弹窗探针没跑完（脚本报错？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    checks = [
        (probe.get("shownAfterCall") is True, "调用后弹窗没显示出来"),
        (probe.get("focusOn") == "modalCancel", "默认焦点不在「取消」上（在 %s）" % probe.get("focusOn")),
        (probe.get("okClassWhenDanger") == "danger", "危险操作的确认按钮不是红色（%s）" % probe.get("okClassWhenDanger")),
        (probe.get("okClassWhenNormal") == "primary", "普通确认的按钮不是主色（%s）" % probe.get("okClassWhenNormal")),
        (probe.get("escapeResult") is False, "按 Esc 返回了「确定」"),
        (probe.get("hiddenAfterEscape") is True, "按 Esc 之后弹窗没收起来"),
        (probe.get("backdropResult") is False, "点遮罩返回了「确定」"),
        (probe.get("hiddenAfterBackdrop") is True, "点遮罩之后弹窗没收起来"),
        (probe.get("enterResult") is True, "按 Enter 没返回「确定」"),
        (probe.get("openAfterInnerClick") is True, "点卡片里面把弹窗关掉了（应该只有点遮罩才关）"),
    ]
    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def _rgb(text: str) -> tuple[int, int, int] | None:
    match = re.match(r"rgba?\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)", text or "")
    if not match:
        return None
    return int(match.group(1)), int(match.group(2)), int(match.group(3))


def _alpha(text: str) -> float | None:
    """取出一个颜色值的 alpha。

    `_rgb` 只关心对比度，故意丢掉 alpha；但「这条线画不画」恰恰只看 alpha ——
    静止的进度条 `rgba(0,0,0,0)` 和 `--panel-2` 都是「有条 2px 的元素」，
    区别全在透明度上。Chrome 把 `transparent` 算成 `rgba(0, 0, 0, 0)`。
    """
    value = (text or "").strip()
    if value in ("", "transparent"):
        return 0.0
    match = re.match(r"rgba\(\s*\d+[,\s]+\d+[,\s]+\d+[,\s]+([0-9.]+)\s*\)", value)
    if match:
        return float(match.group(1))
    if value.startswith("rgb("):
        return 1.0
    return None


def _luminance(rgb: tuple[int, int, int]) -> float:
    def channel(value: int) -> float:
        srgb = value / 255
        return srgb / 12.92 if srgb <= 0.04045 else ((srgb + 0.055) / 1.055) ** 2.4

    r, g, b = (channel(c) for c in rgb)
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def contrast_ratio(fg: tuple[int, int, int], bg: tuple[int, int, int]) -> float:
    a, b = _luminance(fg), _luminance(bg)
    hi, lo = max(a, b), min(a, b)
    return (hi + 0.05) / (lo + 0.05)


def check_slots(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """槽位编辑器的行为断言。

    这些点截图看不出来：加一段有没有真的多一格、换位有没有真的换顺序、
    老的「模板内容」输入框是不是已经拆干净了。全是「看着像那么回事」的地方。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-slot-probe="([^"]*)"', dom)
    if not match:
        return False, "槽位探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    chips = probe.get("chips")
    checks = [
        (probe.get("hasBodyInput") is False, "还留着旧的「模板内容」输入框（#tplBody）"),
        (chips == 3, "当前模板应当有 3 段，实际 %s 段" % chips),
        # 桩里当前模板是「艺人 - 英文名 - 中文名」，第 1 段就该是 artist。
        # 这条同时证明了 `select.value = key` 真的赋上了 —— 赋不上的话
        # 下拉框会显示第一项（原版歌名），而存进去的却是别的，最难发现的那种不一致。
        (
            probe.get("firstKey") == "artist",
            "当前模板第 1 段应当是「艺人」（artist），实际 %r" % probe.get("firstKey"),
        ),
        # `[a] [b] [c] [+]` —— 加号在同一行里、在最后，这就是 Majo 要的形状。
        (probe.get("addInRow") is True, "「+ 添加」不在槽位那一行里"),
        (probe.get("addIsLast") is True, "「+ 添加」不是行内最后一个元素（应当排在所有段之后）"),
        (
            probe.get("chipsWithSelect") == chips,
            "有 %s/%s 个段不是「下拉框 + ×」的形状" % (probe.get("chipsWithSelect"), chips),
        ),
        (
            probe.get("chipsAfterAdd") == 4,
            "点「添加」之后应当变成 4 段，实际 %s" % probe.get("chipsAfterAdd"),
        ),
        (
            probe.get("lastKeyAfterAdd") == "title",
            "新加的一段应当默认是「原版歌名」，实际 %s" % probe.get("lastKeyAfterAdd"),
        ),
        (
            probe.get("swapped") is True,
            "把第 1 段挪到第 2 位之后前两段没对调（换位前是 %r）—— 排序算错了"
            % probe.get("swapKeys"),
        ),
        (
            probe.get("chipsAfterDelete") == 3,
            "删掉一段之后应当剩 3 段，实际 %s" % probe.get("chipsAfterDelete"),
        ),
        # ---- 补充设置常驻 ----
        (
            probe.get("toggles") == 0 and probe.get("openClassLeft") == 0,
            "还留着「展开 / 收起」的按钮（%s 个）或 .open 状态（%s 格）—— "
            "折叠那版已经撤掉了，字段现在是常驻的"
            % (probe.get("toggles"), probe.get("openClassLeft")),
        ),
        (
            probe.get("fieldsHidden") == 0,
            "有 %s 份补充设置是藏着的（display:none 之类）—— 折叠那版才会藏它们"
            % (probe.get("fieldsHidden"),),
        ),
        (
            probe.get("fieldsPerChip") == [1, 1, 1, 1, 1],
            "每一格都该自带一份补充设置，实际 %r" % (probe.get("fieldsPerChip"),),
        ),
        (
            probe.get("fieldsInPage") == 5,
            "页面上应当恰好有 5 份 .chip-fields（每格一份），实际 %s"
            % probe.get("fieldsInPage"),
        ),
        (
            probe.get("rowsUsed") == 5,
            "5 格只占了 %s 行 —— 应当一行一段竖着排" % probe.get("rowsUsed"),
        ),
        (
            probe.get("sepInSecond") == 1 and probe.get("inputsInSecond") == 2,
            "第 2 格的补充设置应当是「1 个分隔符下拉 + 2 个输入框（前缀 / 后缀）」，"
            "实际 %s / %s" % (probe.get("sepInSecond"), probe.get("inputsInSecond")),
        ),
        (
            probe.get("sepInFirst") == 0,
            "第 1 格前面没有东西，却画出了「分隔符」下拉（%s 个）—— 它没有意义"
            % probe.get("sepInFirst"),
        ),
        # ---- 焦点外框 ----
        #
        # 「行里的控件拿到焦点，整行亮起来」是修掉「蓝色外框有缺口」的办法：
        # 以前每个控件画自己的框，而控件比行矮、还偏在一侧，看着就是框缺了一块。
        (
            probe.get("focusRingOnRow") is True,
            "行里的控件拿到焦点之后，这一行的边框颜色没变（%s → %s）—— "
            "焦点反馈没落到整行上，用户看不到自己在哪一段"
            % (probe.get("ringDim"), probe.get("ringLit")),
        ),
        (
            probe.get("otherRowUntouched") is True,
            "焦点在第 2 格，第 1 格的边框却跟着亮了 —— 焦点框挂错了元素",
        ),
        (
            probe.get("ringRestored") is True,
            "焦点移走之后第 2 格的边框没恢复（还是 %s）" % probe.get("ringLit"),
        ),
        # ---- 模板列表不该再标「当前」 ----
        (
            probe.get("listBadges") == 0
            and probe.get("listSaysCurrent") == 0
            and probe.get("listRow1") == 0,
            "模板列表上还挂着「当前」小标（.badge %s 个 / 写着「当前」的文字 %s 处 / "
            ".row1 容器 %s 个）—— 用哪一份模板由任务页那颗下拉决定，"
            "列表不该再标一个「当前」"
            % (probe.get("listBadges"), probe.get("listSaysCurrent"), probe.get("listRow1")),
        ),
        # ---- 模板页右边不该有滚动条 ----
        (
            probe.get("scrollOver") == 0,
            "模板页右边还有 %s px 装不下（scrollHeight - clientHeight）—— "
            "正常窗口尺寸下这一页应当一眼看全" % probe.get("scrollOver"),
        ),
        (
            probe.get("previewOver") == 0,
            "命名预览框内部还溢出 %s px —— 它是这一页唯一允许被压的一块，"
            "但压到会裁掉样例，就说明别处的留白还没压干净" % probe.get("previewOver"),
        ),
        (
            probe.get("previewShrink") not in ("", "0", None),
            "命名预览的 flex-shrink 是 %r —— 它是唯一能被压缩的一块，"
            "写死成 0 的话窗口一矮整页又开始滚" % (probe.get("previewShrink"),),
        ),
        # ---- 「这一段被选中」要看得出来 ----
        (
            probe.get("chipLeftBar") == "3px",
            "选中标记那条左竖条应当是 3px，实际 %s" % probe.get("chipLeftBar"),
        ),
        (
            probe.get("chipBarDim") != probe.get("chipBarLit")
            and probe.get("chipBgDim") != probe.get("chipBgLit"),
            "选中之后左竖条和底色都没变（竖条 %s → %s，底色 %s → %s）—— "
            "只换 1px 外框颜色在深底上看不出来，这就是用户报的那个毛病"
            % (probe.get("chipBarDim"), probe.get("chipBarLit"),
               probe.get("chipBgDim"), probe.get("chipBgLit")),
        ),
        (
            probe.get("fieldKeepsBorder") is True,
            "chip-fields 里的小输入框一聚焦边框就没了"
            "（style=%s width=%s）—— `.slotchip select:focus { border: none }` "
            "是后代选择器，把 border-style 一起清成了 none，"
            "后面那条只改 border-color 的改不回来"
            % (probe.get("fieldBorderStyle"), probe.get("fieldBorderWidth")),
        ),
        (
            probe.get("swappedKey") is True,
            "把第 1 段挪到第 2 位之后没换过去 —— 排序算错了",
        ),
        (
            probe.get("fieldsAfterMove") is True,
            "换位之后有格子丢了补充设置 —— 整行重建时漏了字段",
        ),
        (
            probe.get("fieldsAfterDelete") is True,
            "删掉一段之后有格子丢了补充设置 —— 整行重建时漏了字段",
        ),
        (probe.get("previewRows") == 4, "命名预览应当有 4 行，实际 %s" % probe.get("previewRows")),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_drag(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """拖放遮罩只认文件。

    用户报过：「在界面拖动文本，或一些可拖动的东西都会显示『松开鼠标，
    把音乐/视频加进队列』」。根因是 dragenter 不看 dataTransfer 里是什么，
    而 Chromium 拖文本、拖槽位排序一样会发 dragenter。

    截图证不了这件事：遮罩是整页盖一层，截出来只是「有一张盖着」，
    而截图那一次永远没有在拖东西。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-drag-probe="([^"]*)"', dom)
    if not match:
        return False, "拖放探针没跑完（脚本报错？页面没加载？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    checks = [
        # 前提：Chromium 拖文本时 types 里也有 text/plain。
        # 这一条断言的是「为什么不能按 types 判断」—— 哪天判断改成看 types，
        # 上面那条 onForText 会红，而这条能说明它为什么红。
        (
            probe.get("typesForText") == "text/plain",
            "拖文本时 dataTransfer.types 应当是 text/plain，实际 %r —— "
            "判断依据变了，下面几条的前提要重看" % probe.get("typesForText"),
        ),
        (
            probe.get("onForText") is False,
            "拖一段文本就亮起了「松开鼠标，把音乐/视频加进队列」—— "
            "用户手里根本没有文件，这正是他报的那个问题",
        ),
        (
            probe.get("offAfterTextLeave") is True,
            "拖文本离开之后遮罩没收起来",
        ),
        (
            probe.get("onForSlotDrag") is False,
            "拖槽位排序时亮起了拖放遮罩 —— 那是本程序自己的拖拽，不是要加文件",
        ),
        (
            probe.get("typesForFile") == "Files",
            "拖文件时 dataTransfer.types 应当是 Files，实际 %r" % probe.get("typesForFile"),
        ),
        (
            probe.get("onForFile") is True,
            "真的拖一个文件进来却不亮 —— 用户不知道该松手了",
        ),
        # 从子元素上划过会再发一次 dragenter，靠计数撑住；少一次 dragleave
        # 也不该提前收起来，否则遮罩在拖动过程中一闪一闪。
        (
            probe.get("stillOnAfterInnerLeave") is True,
            "拖文件划过子元素时遮罩就收起来了 —— 拖的过程中会一闪一闪",
        ),
        (
            probe.get("offAfterFileLeave") is True,
            "拖文件离开窗口之后遮罩没收起来",
        ),
        (
            probe.get("offAfterDrop") is True,
            "松手之后遮罩还留着 —— 它会一直盖着界面",
        ),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_scrollbars(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """滚动条要契合主题，不能是原生那根。

    用户报的：「整个界面的滚动条都是原生的样式，要契合主题的风格样式去修改」。
    原生那根在深色主题上是一块浅灰，和面板的 #161920 差着好几个明度。

    截图证不了这件事 —— 截图脚本带 --hide-scrollbars，图里永远没有滚动条。
    所以读的是 ::-webkit-scrollbar 那几个伪元素的计算值。Chromium 支持给
    getComputedStyle 传伪元素名，读到的就是 CSS 里写的值（实测过）。

    另外守反方向：模板页的 #tplScroll 和 .preview 是故意藏掉滚动条的，
    全局那套一旦写得不够具体就会把它们一起变回来。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-scrollbar-probe="([^"]*)"', dom)
    if not match:
        return False, "滚动条探针没跑完（脚本报错？页面没加载？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    pane = probe.get("pane")
    if not pane:
        return False, "没量到底栏那个滚动容器（.pane）—— 选择器变了？"

    checks = [
        (
            pane.get("width") == "10px",
            "滚动条宽度是 %r，不是 10px —— 这条 CSS 没生效，界面上还是原生那根" % pane.get("width"),
        ),
        (
            pane.get("thumb") == "rgb(35, 40, 51)",
            "滑块颜色是 %r，不是主题里的 #232833" % pane.get("thumb"),
        ),
        (
            pane.get("track") == "rgba(0, 0, 0, 0)",
            "轨道不是透明的（%r）—— 会在面板上画出一条比面板更亮的底" % pane.get("track"),
        ),
        (
            pane.get("radius") == "6px",
            "滑块圆角是 %r，不是 6px" % pane.get("radius"),
        ),
    ]
    for ok, reason in checks:
        if not ok:
            return False, reason

    for name, label in (("tplScroll", "模板页的滚动区"), ("preview", "模板页的预览框")):
        item = probe.get(name)
        if not item:
            return False, f"没量到{label}（{name}）—— 选择器变了？"
        if item.get("width") != "0px":
            return False, (
                f"{label}（{name}）又出滚动条了（宽 {item.get('width')}）—— "
                "那一页要一屏看得全，全局那套滚动条样式别把它一起盖住"
            )
    return True, ""


def check_tpl_menu(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """模板列表右键菜单的重命名 / 复制 / 删除。

    名称的改动现在**只**挂在这份菜单上（页面上那个「名称」输入框和底栏那两颗
    按钮都撤掉了）。所以这条路径断了，改名就完全没有入口 —— 而它断掉的样子是
    「右键没反应」，不抛错、不写日志，只有手动点才会发现。

    断言刻意走完整条链：右键 → 菜单 → 弹窗填字 → 后端 → 列表重画。
    只验「菜单弹出来了」不够，真正的风险在弹窗和后端那两步。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-menu-probe="([^"]*)"', dom)
    if not match:
        return False, "右键菜单探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    checks = [
        (
            probe.get("menuShown") is True and probe.get("labels") == "重命名 | 复制 | 删除",
            "右键之后菜单里应当恰好是「重命名 | 复制 | 删除」，实际 %r（弹出来=%s）"
            % (probe.get("labels"), probe.get("menuShown")),
        ),
        (
            probe.get("selectedIsRightClicked") is True,
            "右键没有顺手选中那一份 —— 菜单里的三项都是对着它做的，"
            "用户会以为已经选上了",
        ),
        (
            probe.get("menuInsideViewport") is True,
            "菜单被窗口裁掉了一部分 —— 它在最上层又没有滚动条，"
            "看不见的那几项等于不存在",
        ),
        (
            probe.get("deleteEnabled") is True,
            "还有好几份模板时「删除」就该是可用的",
        ),
        (
            probe.get("closedByOutsideClick") is True,
            "点菜单外面没收起来 —— 它会一直挡着底下的按钮，"
            "而用户以为自己只是点了一下空白",
        ),
        (
            probe.get("closedByEscape") is True,
            "按 Esc 没收起来菜单",
        ),
        # ---- 重命名 ----
        (
            probe.get("renameDialogShown") is True
            and probe.get("renameFocusIsInput") is True,
            "点「重命名」之后要弹一个输入框、焦点落在输入框里（弹窗=%s，焦点=%s）"
            % (probe.get("renameDialogShown"), probe.get("renameFocusIsInput")),
        ),
        (
            probe.get("renamePrefill") == "歌名 - 艺人",
            "重命名弹窗里应当预填原名（第 2 份是「歌名 - 艺人」），实际 %r"
            % probe.get("renamePrefill"),
        ),
        (
            probe.get("renamedInPlace") is True
            and probe.get("namesAfterRename", "").find("改过名的模板") >= 0,
            "重命名应当**就地**改名：条数和位置都不变，旧名字消失。实际 %r"
            % probe.get("namesAfterRename"),
        ),
        (
            probe.get("editingFollowedRename") is True,
            "改名之后编辑器没跟着换过去 —— editing 指向一个不存在的名字，"
            "底栏会一直写「有未保存的改动」，按「保存」会把旧名字重新创建出来",
        ),
        # ---- 复制 ----
        (
            probe.get("dupAdded") == 1 and probe.get("dupPrefill") == "只歌名 副本",
            "复制应当正好多出一份、默认名字是「原名 副本」，实际多了 %s 份（预填 %r）"
            % (probe.get("dupAdded"), probe.get("dupPrefill")),
        ),
        (
            probe.get("dupSelected") is True,
            "复制完没切到新的那一份 —— 复制多半就是为了改它，"
            "留在原地用户还得回列表里找一遍",
        ),
        # ---- 删除 ----
        (
            probe.get("deleteConfirmShown") is True,
            "删除没过确认框 —— 右键一滑就少一份模板，没有回头路",
        ),
        (
            probe.get("deleteRemoved") == 1,
            "确认删除之后应当正好少一份，实际少了 %s 份" % probe.get("deleteRemoved"),
        ),
        (
            probe.get("itemsAtEnd") == 1 and probe.get("deleteDisabledOnLast") is True,
            "删到最后一份时「删除」应当置灰（后端也会拒绝，但按下去才报错不如"
            "一开始就是灰的）。实际剩 %s 份、置灰=%s"
            % (probe.get("itemsAtEnd"), probe.get("deleteDisabledOnLast")),
        ),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_history(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """记录页的勾选 / 批量还原 / 批量移除 / 筛选 / 搜索 / shift 连选。

    这一页最危险的地方是「按钮上的数字和真正会动的条数对不上」：
    勾了五行、筛掉四行、按下去却动了五行 —— 用户根本不知道被改了什么。
    所以最重的两条是「切筛选之后计数跟着收窄」和
    「按下去时后端收到的条数 = 按钮上那个数字」。

    另一件是「谁能勾」：勾选是记录级的，三种状态都要能勾（移除记录对它们
    一样有效）；但只有「可还原」的能进「还原所选」。两件事混起来的话，
    「清理记录」这个用法根本做不了。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-hist-probe="([^"]*)"', dom)
    if not match:
        return False, "记录页探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    # 桩里 5 条：3 条可还原、1 条已还原、1 条文件已不在。
    checks = [
        (
            probe.get("rows") == 5 and probe.get("checkboxes") == 5,
            "桩里 5 条记录应当有 5 行 5 个勾选框，实际 %s 行 %s 个"
            % (probe.get("rows"), probe.get("checkboxes")),
        ),
        # 三种状态都能勾 —— 勾选是记录级的，而「移除所选」对它们一样有效。
        # 只有「可还原」的那 3 条才有「还原」按钮。
        (
            probe.get("enabledBoxes") == 5 and probe.get("restorableRows") == 3,
            "5 条记录应当都能勾、其中 3 条有「还原」按钮，实际能勾 %s 条、有按钮 %s 条"
            % (probe.get("enabledBoxes"), probe.get("restorableRows")),
        ),
        (
            probe.get("disabledAtStart") is True and probe.get("buttonAtStart") == "还原所选"
            and probe.get("removeDisabledAtStart") is True
            and probe.get("removeAtStart") == "移除所选",
            "一个都没勾时两颗按钮都该是禁用的，实际还原 %r（disabled=%s）/ 移除 %r（disabled=%s）"
            % (probe.get("buttonAtStart"), probe.get("disabledAtStart"),
               probe.get("removeAtStart"), probe.get("removeDisabledAtStart")),
        ),
        (
            probe.get("countAtStart") == "5 条",
            "没筛选时应当显示「5 条」，实际 %r" % probe.get("countAtStart"),
        ),
        # ---- 被手动改过名的文件 ----
        #
        # 桩里第 4 条模拟「用户在资源管理器里改过名」。现名那一格必须显示
        # 文件**实际**在的名字并挂上小标 —— 显示记录里那个磁盘上不存在的
        # 名字，用户会以为文件没了，而它其实还在、还原也照样有效。
        (
            probe.get("movedTags") == 1 and probe.get("movedBadgeText") == "已改名",
            "应当恰好有一行挂「已改名」小标，实际 %s 个（文案 %r）"
            % (probe.get("movedTags"), probe.get("movedBadgeText")),
        ),
        (
            probe.get("movedShownName") == "Hells Bells (1).mkv",
            "被外部改过名的那一行要显示文件实际在的名字，实际 %r"
            % probe.get("movedShownName"),
        ),
        (
            probe.get("movedOnRestored") is False,
            "「已还原」那一行被挂上了「已改名」小标 —— 还原本来就会把文件改回原名，"
            "那是还原的结果，不是外部改名",
        ),
        # 全选：5 条全部勾上（勾选是记录级的），而两颗按钮各自数自己认的那批。
        (
            probe.get("pickedByAll") == 5 and probe.get("allChecked") is True,
            "全选应当勾上全部 5 条、并且表头也变勾上，实际勾了 %s（表头 %s）"
            % (probe.get("pickedByAll"), probe.get("allChecked")),
        ),
        (
            probe.get("buttonAfterAll") == "还原所选（3）"
            and probe.get("removeAfterAll") == "移除所选（5）",
            "全选之后应当写「还原所选（3）」和「移除所选（5）」（3 条可还原、5 条可移除），"
            "实际 %r / %r" % (probe.get("buttonAfterAll"), probe.get("removeAfterAll")),
        ),
        (
            probe.get("pickedAfterClear") == 0 and probe.get("disabledAfterClear") is True
            and probe.get("removeDisabledAfterClear") is True,
            "取消全选之后应当一个都不剩、两颗按钮都回到禁用，"
            "实际勾了 %s（还原 disabled=%s / 移除 disabled=%s）"
            % (probe.get("pickedAfterClear"), probe.get("disabledAfterClear"),
               probe.get("removeDisabledAfterClear")),
        ),
        # 勾一部分时表头必须是 indeterminate —— 空框会让用户以为勾选丢了。
        (
            probe.get("buttonAfterOne") == "还原所选（1）"
            and probe.get("enabledAfterOne") is True
            and probe.get("allIndeterminate") is True,
            "只勾一条时按钮应当写「还原所选（1）」且表头呈「部分勾选」，"
            "实际 %r / %s" % (probe.get("buttonAfterOne"), probe.get("allIndeterminate")),
        ),
        # ---- shift 连选 ----
        #
        # 一屏几百条时一条条点不现实，而「全选」又太粗。
        # 从第 1 行 shift 点到第 3 行 = 三行都勾上；再 shift 点回第 1 行
        # （这次是取消）应当把这一段一起清掉。
        (
            probe.get("shiftPicked") == 3 and probe.get("shiftAnchorKept") is True,
            "从第 1 行 shift 点到第 3 行应当勾上 3 行、表头呈部分勾选，实际 %s 行"
            % probe.get("shiftPicked"),
        ),
        (
            probe.get("shiftCleared") == 0,
            "再 shift 点回第 1 行（取消）应当把这一段一起清掉，实际还剩 %s 行"
            % probe.get("shiftCleared"),
        ),
        # 最重的一条：切筛选之后，计数必须跟着收窄到「当前看得见」的那批。
        (
            probe.get("pickedBeforeFilter") == 5
            and probe.get("buttonBeforeFilter") == "还原所选（3）"
            and probe.get("removeBeforeFilter") == "移除所选（5）",
            "勾上全部 5 条之后应当写「还原所选（3）」「移除所选（5）」，"
            "实际 %r / %r（勾了 %s）"
            % (probe.get("buttonBeforeFilter"), probe.get("removeBeforeFilter"),
               probe.get("pickedBeforeFilter")),
        ),
        # 切到「已还原」：刚才那三条可还原的全被筛掉了，剩下一行是「已还原」的。
        # 它能勾（移除记录用得上），但绝不能进「还原所选」——
        # 那颗按钮上写着 3 的话，这一按下去就会改掉用户看不见的文件。
        (
            probe.get("pickedAfterFilter") == 1
            and probe.get("disabledAfterFilter") is True
            and probe.get("buttonAfterFilter") == "还原所选"
            and probe.get("removeAfterFilter") == "移除所选（1）",
            "勾了 5 条之后切到「已还原」档（可还原的三条全被筛掉），"
            "「还原所选」必须熄掉、「移除所选」只数剩下的那 1 条，"
            "实际还原 %r（disabled=%s）/ 移除 %r"
            % (probe.get("buttonAfterFilter"), probe.get("disabledAfterFilter"),
               probe.get("removeAfterFilter")),
        ),
        # 反过来：切到「可还原」时那三条还在屏幕上，勾选该留着。
        (
            probe.get("undoableRows") == 3 and probe.get("pickedOnUndoable") == 3,
            "切到「可还原」档时那三条还在屏幕上，勾选不该被清掉（实际剩 %s 条）"
            % probe.get("pickedOnUndoable"),
        ),
        (
            probe.get("restoredRows") == 1 and probe.get("missingRows") == 1
            and probe.get("backToAllRows") == 5,
            "三档筛选的行数应当是 已还原 1 / 已不在 1 / 全部 5，实际 %s / %s / %s"
            % (probe.get("restoredRows"), probe.get("missingRows"),
               probe.get("backToAllRows")),
        ),
        # 搜索把五条收窄到一条（而且是不可还原的那条）时，按钮必须跟着收窄。
        (
            probe.get("pickedBeforeSearch") == 5 and probe.get("searchRows") == 1
            and probe.get("pickedAfterSearch") == 1
            and probe.get("buttonAfterSearch") == "还原所选"
            and probe.get("removeAfterSearch") == "移除所选（1）",
            "勾了 5 条之后搜「clip」（只剩 1 条不可还原的行），"
            "应当变成「还原所选」禁用 +「移除所选（1）」，实际 %r / %r"
            % (probe.get("buttonAfterSearch"), probe.get("removeAfterSearch")),
        ),
        (
            probe.get("searchCount") == "显示 1 / 5 条",
            "搜「clip」时计数应当写「显示 1 / 5 条」，实际 %r" % probe.get("searchCount"),
        ),
        (
            probe.get("pickedBackToAll") == 5,
            "切回「全部」时原来勾的五条应当还勾着（实际 %s 条）—— "
            "筛掉它们对安全没有帮助，只会让用户白勾一遍" % (probe.get("pickedBackToAll")),
        ),
        # 搜不到 ≠ 没有记录。这两句话混了，用户会以为记录被清空了。
        (
            probe.get("noMatchRows") == 0 and probe.get("noMatchWrapHidden") is True
            and probe.get("noMatchTitle") == "没有符合条件的记录",
            "搜不到时应当收起表格、并说「没有符合条件的记录」，实际 %s 行 / 隐藏=%s / %r"
            % (probe.get("noMatchRows"), probe.get("noMatchWrapHidden"),
               probe.get("noMatchTitle")),
        ),
        (
            probe.get("rowsAtEnd") == 5,
            "清空搜索之后应当回到 5 行，实际 %s" % probe.get("rowsAtEnd"),
        ),
        # 最重的一条：按钮上写着 1，后端就必须只收到 1 条。
        # 前面那些都是 DOM 断言，只有这一条真的看了「后端收到什么」。
        (
            probe.get("visiblePickedBeforeSend") == 1
            and probe.get("buttonBeforeSend") == "还原所选（1）"
            and probe.get("sent") == 1,
            "在「全部」下勾了 %s 条、再搜成只剩 1 条可见时，按钮写着 %r，"
            "但后端收到 %s 条（%s）—— 那几条用户根本看不见的文件也会被改掉"
            % (probe.get("pickedBeforeSend"), probe.get("buttonBeforeSend"),
               probe.get("sent"), probe.get("sentNames")),
        ),
        # ---- 移除所选 ----
        #
        # 它带确认弹窗（丢的是「这个文件原来叫什么」，不可再生），
        # 而且文案里要写明磁盘上的文件不受影响 —— 用户最怕的就是
        # 「移除」把文件也删了。
        (
            probe.get("removeAsked") is True
            and probe.get("removeOkLabel") == "移除"
            and "磁盘上的文件不会被删除" in (probe.get("removeBody") or ""),
            "「移除所选」应当弹确认、按钮写「移除」、并说明磁盘上的文件不会被删除，"
            "实际 弹出=%s / 按钮=%r / 正文=%r"
            % (probe.get("removeAsked"), probe.get("removeOkLabel"), probe.get("removeBody")),
        ),
        # 这一批里有能还原的，所以是危险色。
        (
            probe.get("removeIsDanger") is True,
            "这一批里有还能还原的记录，确认按钮该走危险色 —— "
            "拿掉之后那些文件就再也改不回原名了",
        ),
        # 和还原同一条规矩：按钮上写着几，后端就得收到几条。
        (
            probe.get("pickedBeforeRemove") == 5 and probe.get("dropped") == 5,
            "勾了 %s 条之后点「移除所选」，后端应当收到 5 条，实际 %s 条（%s）"
            % (probe.get("pickedBeforeRemove"), probe.get("dropped"),
               probe.get("droppedNames")),
        ),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_history_empty(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """一条记录都没有时，说的是「还没有改名记录」而不是「没有符合条件的记录」。

    这两句话各管一段：前者是新装好还没跑过任务，后者是筛错了。
    说反了，用户会去翻设置、找记录文件，而其实只是关键词打错了一个字。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-hist-probe="([^"]*)"', dom)
    if not match:
        return False, "空记录探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    if probe.get("emptyRows") != 0:
        return False, "空记录页面上 tbody 里还有 %s 行" % probe.get("emptyRows")
    if probe.get("emptyShown") is not True:
        return False, "空记录页面上空状态没显示出来，用户会看到一片空白"
    if probe.get("emptyWrapHidden") is not True:
        return False, "空记录页面上还显示着表格（应当只显示空状态）"
    if probe.get("emptyTitle") != "还没有改名记录":
        return False, ("一条记录都没有时应当说「还没有改名记录」，实际 %r —— "
                       "写成「没有符合条件的记录」会让用户去翻筛选，其实根本没记录"
                       % probe.get("emptyTitle"))
    if probe.get("emptyCount") != "0 条":
        return False, "空记录时计数应当是「0 条」，实际 %r" % probe.get("emptyCount")
    if probe.get("emptyButtonDisabled") is not True:
        return False, "一条记录都没有时「还原所选」应当是禁用的"
    if probe.get("emptyRemoveDisabled") is not True:
        return False, "一条记录都没有时「移除所选」应当是禁用的"
    # 分页条也要一起收起来：记录一条都没有的时候还挂一句「第 1 / 1 页」，
    # 用户会以为列表被筛空了。
    if probe.get("emptyPagerHidden") is not True:
        return False, "一条记录都没有时分页条还显示着（应当和表格一起收起来）"
    return True, ""


def read_probe(dom: str, attr: str):
    """从 dump-dom 里取一个探针属性的 JSON。取不到时返回 (None, 原因)。

    三条分页检查共用这一份 —— 它们只是属性名和断言不同，
    解析这段（取属性、反转义、解析 JSON、报错措辞）不该抄三遍。
    """
    match = re.search(attr + r'="([^"]*)"', dom)
    if not match:
        return None, "探针没跑完（脚本报错？boot 没结束？）"
    raw = match.group(1).replace("&quot;", '"')
    try:
        return json.loads(raw), ""
    except json.JSONDecodeError as error:
        return None, f"探针输出不是 JSON：{error} —— {raw[:120]}"


def check_task_page(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """任务列表的分页。跑在 120 条的样本页上（tasksmany.html）。

    只有 8 条的桩验不出分页 —— 页数永远是 1。而分页出错是**不报错**的，
    它只是画错一截：所以这里除了数行数，还把「这一页的第一条是哪一条」
    一起钉住。「每页都画前 50 条」这种错，光数行数照样能过。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    probe, reason = read_probe(result.stdout or "", "data-pager-probe")
    if probe is None:
        return False, reason

    checks = [
        (
            probe.get("sizeDefault") == "50"
            and probe.get("sizeOptions") == "50,100,200,300",
            "每页条数应当默认 50、可选 50/100/200/300，实际默认 %r（可选 %r）"
            % (probe.get("sizeDefault"), probe.get("sizeOptions")),
        ),
        (
            probe.get("rowsP1") == 50 and probe.get("infoP1") == "第 1 / 3 页"
            and probe.get("prevDisabledP1") is True
            and probe.get("nextDisabledP1") is False
            and probe.get("firstP1") == "clip_0001.mp4",
            "120 条每页 50：第一页该是 50 行 / 第 1 / 3 页 / 从第 1 条开始，"
            "实际 %s 行 / %r / 首行 %r（上页禁用=%s 下页禁用=%s）"
            % (probe.get("rowsP1"), probe.get("infoP1"), probe.get("firstP1"),
               probe.get("prevDisabledP1"), probe.get("nextDisabledP1")),
        ),
        (
            probe.get("rowsP2") == 50 and probe.get("infoP2") == "第 2 / 3 页"
            and probe.get("firstP2") == "clip_0051.mp4",
            "第二页该从第 51 条开始、50 行，实际 %s 行 / %r / 首行 %r —— "
            "每页都画前 50 条的话这里就露馅了"
            % (probe.get("rowsP2"), probe.get("infoP2"), probe.get("firstP2")),
        ),
        (
            probe.get("rowsP3") == 20 and probe.get("infoP3") == "第 3 / 3 页"
            and probe.get("nextDisabledP3") is True
            and probe.get("firstP3") == "clip_0101.mp4",
            "尾页该只剩 20 行（120 - 100）且「下一页」置灰，实际 %s 行 / %r / 首行 %r"
            % (probe.get("rowsP3"), probe.get("infoP3"), probe.get("firstP3")),
        ),
        (
            probe.get("rowsBack") == 50 and probe.get("infoBack") == "第 2 / 3 页",
            "翻回上一页该回到第 2 页、50 行，实际 %s 行 / %r"
            % (probe.get("rowsBack"), probe.get("infoBack")),
        ),
        (
            probe.get("rowsSize300") == 120 and probe.get("infoSize300") == "第 1 / 1 页",
            "每页 300 时 120 条该一页装完，实际 %s 行 / %r"
            % (probe.get("rowsSize300"), probe.get("infoSize300")),
        ),
        (
            probe.get("rowsSize100") == 100 and probe.get("infoSize100") == "第 1 / 2 页"
            and probe.get("firstSize100") == "clip_0001.mp4",
            "每页 100 时该是 100 行 / 第 1 / 2 页 / **从第 1 条开始**"
            "（换每页条数要回到第 1 页），实际 %s 行 / %r / 首行 %r"
            % (probe.get("rowsSize100"), probe.get("infoSize100"),
               probe.get("firstSize100")),
        ),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_history_page(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """记录列表的分页 + 时间范围筛选。跑在 120 条的样本页上（histmany.html）。

    时间范围那一半靠样本的**固定**时间戳（见 big_history）：它是 2026-02 的，
    所以「今天」「本月」必须命中 0 条 —— 预设要是写成了「全都返回」，这里就照出来。
    自定义那半用从样本算出来的日期，于是不依赖跑在哪个时区。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    probe, reason = read_probe(result.stdout or "", "data-pager-probe")
    if probe is None:
        return False, reason

    checks = [
        (
            probe.get("sizeDefault") == "50"
            and probe.get("sizeOptions") == "50,100,200,300",
            "每页条数应当默认 50、可选 50/100/200/300，实际默认 %r（可选 %r）"
            % (probe.get("sizeDefault"), probe.get("sizeOptions")),
        ),
        (
            probe.get("rowsP1") == 50 and probe.get("infoP1") == "第 1 / 3 页"
            and probe.get("firstP1") == "rec_0001.mp4",
            "120 条每页 50：第一页该是 50 行 / 第 1 / 3 页 / 从最新那条开始，"
            "实际 %s 行 / %r / 首行 %r"
            % (probe.get("rowsP1"), probe.get("infoP1"), probe.get("firstP1")),
        ),
        (
            probe.get("rowsP2") == 50 and probe.get("firstP2") == "rec_0051.mp4",
            "第二页该从第 51 条开始，实际 %s 行 / 首行 %r"
            % (probe.get("rowsP2"), probe.get("firstP2")),
        ),
        (
            probe.get("rowsP3") == 20 and probe.get("infoP3") == "第 3 / 3 页"
            and probe.get("nextDisabledP3") is True
            and probe.get("firstP3") == "rec_0101.mp4",
            "尾页该只剩 20 行且「下一页」置灰，实际 %s 行 / %r / 首行 %r"
            % (probe.get("rowsP3"), probe.get("infoP3"), probe.get("firstP3")),
        ),
        (
            probe.get("rowsSize300") == 120 and probe.get("infoSize300") == "第 1 / 1 页",
            "每页 300 时 120 条该一页装完，实际 %s 行 / %r"
            % (probe.get("rowsSize300"), probe.get("infoSize300")),
        ),
        # ---- 时间范围 ----
        (
            probe.get("customHiddenAtStart") is True,
            "没选「自定义…」时两个日期框不该露出来 —— 预设档和日期框同时在场，"
            "谁说了算得靠猜",
        ),
        # 样本是 2026-02 的，所以「今天」必须 0 条。这同时证明这一档不是「全都返回」。
        (
            probe.get("todayRows") == 0 and probe.get("todayInfo") == "—"
            and probe.get("todayWrapHidden") is True
            and probe.get("todayTitle") == "没有符合条件的记录"
            and probe.get("todayPagerHidden") is True,
            "样本是 2026-02 的记录，切到「今天」该一条都不剩、表格和分页条一起收起、"
            "并说「没有符合条件的记录」，实际 %s 行 / %r / 表格隐藏=%s / %r / 分页隐藏=%s"
            % (probe.get("todayRows"), probe.get("todayInfo"),
               probe.get("todayWrapHidden"), probe.get("todayTitle"),
               probe.get("todayPagerHidden")),
        ),
        (
            probe.get("customShown") is True,
            "选到「自定义…」之后两个日期框该露出来",
        ),
        # 只填「起」= 「从那以后」。样本最新的一条就在这一天，所以它必须还在。
        (
            isinstance(probe.get("fromOnlyRows"), int)
            and 0 < probe.get("fromOnlyRows") < 120
            and probe.get("fromOnlyFirst") == "rec_0001.mp4",
            "只填起始日期时该收窄到这一天及以后（最新那条必须还在），"
            "实际 %s 行 / 首行 %r" % (probe.get("fromOnlyRows"), probe.get("fromOnlyFirst")),
        ),
        # 起止同一天：右端是半开区间（到次日 00:00），所以这一天之内的全都算。
        # 把 to 当成「那一天 00:00」的写法会得到 0 行 —— 而用户选「今天」时
        # 最想看到的恰恰是今天。
        (
            probe.get("sameDayRows") == probe.get("fromOnlyRows")
            and probe.get("sameDayRows") > 0,
            "起止填同一天时该命中这一天内的全部记录（实际 %s 行，只填起始时 %s 行）—— "
            "右端写成「那一天 00:00」的话这里会是 0"
            % (probe.get("sameDayRows"), probe.get("fromOnlyRows")),
        ),
        (
            probe.get("pastRows") == 0 and probe.get("pastWrapHidden") is True
            and probe.get("pastPagerHidden") is True
            and probe.get("pastTitle") == "没有符合条件的记录",
            "范围挪到样本之后该一条都不剩、表格和分页条一起收起，"
            "实际 %s 行 / 表格隐藏=%s / 分页隐藏=%s / %r"
            % (probe.get("pastRows"), probe.get("pastWrapHidden"),
               probe.get("pastPagerHidden"), probe.get("pastTitle")),
        ),
        (
            probe.get("backRows") == 50 and probe.get("customHiddenAgain") is True,
            "切回「全部时间」该收起日期框、回到第一页 50 行，实际 %s 行 / 日期框还露着=%s"
            % (probe.get("backRows"), not probe.get("customHiddenAgain")),
        ),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_empty_state(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """空队列下「表格」和「空状态」只能看到一个。

    这两块是互斥的渲染分支，各自带一条 1px 边框。同时显示就是两个面板叠在一起，
    用户报的「中间多了一条分割线」正是这个。
    根因：切这两个的 refreshEmptyState() 只在 rebuild() 里调，而空队列时
    boot() 根本不会走 rebuild —— 分支没被切过去。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-empty-probe="([^"]*)"', dom)
    if not match:
        return False, "空队列探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    if probe.get("boot") != "done":
        return False, "boot() 没跑完（data-boot=%r）" % probe.get("boot")

    table, empty = probe.get("tableVisible"), probe.get("emptyVisible")
    if table and empty:
        return False, (
            "空队列下文件表格和空状态同时显示了 —— 两个面板各带一条边框，"
            "看着就是「中间多了一条分割线」"
        )
    if table:
        return False, "空队列下还显示着文件表格（应当只显示空状态）"
    if not empty:
        return False, "空队列下空状态没显示出来，用户会看到一片空白"
    if probe.get("rows"):
        return False, "空队列下 tbody 里还有 %s 行" % probe.get("rows")
    return True, ""


def check_numbers(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """数字输入框的增减按钮。

    原生那对三角是浏览器按系统主题画的，深色界面上很突兀，用户点名提过。
    换成自己画的之后，最容易出的问题是「新增了输入框但忘了处理」——
    界面上会同时出现自己画的按钮和原生三角两套，所以这里断言「全部都换了」。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-number-probe="([^"]*)"', dom)
    if not match:
        return False, "数字输入框探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    count = probe.get("count")
    checks = [
        (count and count > 0, "页面上一个 input[type=number] 都没找到，选择器或结构变了"),
        (
            probe.get("wrapped") == count,
            "有 %s/%s 个数字输入框没有被包成 .numwrap（新增输入框忘了处理？）"
            % (probe.get("wrapped"), count),
        ),
        (
            probe.get("withTwoButtons") == count,
            "有 %s/%s 个数字输入框没有一对 − / + 按钮" % (probe.get("withTwoButtons"), count),
        ),
        (
            probe.get("appearance") == "textfield",
            "原生 spinner 没藏掉（appearance=%r）—— 会和自己画的按钮并排出现两套"
            % probe.get("appearance"),
        ),
        (probe.get("incremented") is True, "点「+」没有让值加 1"),
        (
            probe.get("sawInput") is True,
            "点「+」改了值但没派发 input 事件 —— 底栏会一直显示「已保存」，"
            "而保存下去的还是旧值",
        ),
        (probe.get("plusDisabledAtMax") is True, "到上限后「+」没有置灰，点着没反应像坏了"),
        (probe.get("minusDisabledAtMin") is True, "到下限后「−」没有置灰，点着没反应像坏了"),
    ]

    for ok, message in checks:
        if not ok:
            return False, message
    return True, ""


def check_selects(chrome: str, profile: pathlib.Path, url: str) -> tuple[bool, str]:
    """下拉框的弹出列表必须是深色的。

    Chromium 画选项列表时，底色是从 select 自己的 background-color 推的：
    写成 transparent 就推不出颜色，于是退回系统浅色默认值 —— 白底，
    而选项文字继承 select 的 color（浅色），结果是白底白字，几乎看不见。
    这跟 color-scheme 无关，:root 上写 dark 也救不了。

    弹出列表是独立绘制的一层，截图和 dump-dom 都抓不到它本身，
    所以这里钉住它的**因**：任何一个 select 都不许是透明的。
    """
    result = run_chrome(chrome, profile, ["--dump-dom", f"--window-size={WINDOW_SIZE}"], url)
    dom = result.stdout or ""

    match = re.search(r'data-select-probe="([^"]*)"', dom)
    if not match:
        return False, "下拉框探针没跑完（脚本报错？boot 没结束？）"

    raw = match.group(1).replace("&quot;", '"')
    try:
        probe = json.loads(raw)
    except json.JSONDecodeError as error:
        return False, f"探针输出不是 JSON：{error} —— {raw[:120]}"

    if probe.get("boot") != "done":
        return False, "boot() 没跑完（data-boot=%r）" % probe.get("boot")

    count = probe.get("count")
    if not count:
        return False, "页面上一个 <select> 都没有？探针八成没接对"

    bad = probe.get("transparent") or []
    if bad:
        names = "、".join(
            f"{row['name']}(父={row['parent']})" for row in bad[:3]
        )
        return False, (
            "%d 个 <select> 的背景是透明的（%s）—— Chromium 会因此把选项列表画成"
            "白底，而选项文字是浅色，结果是白底白字几乎看不见。"
            "给它一个和所在容器同色的实心底，别写 transparent" % (len(bad), names)
        )
    return True, ""


def check_layout(dom: str) -> tuple[bool, str]:
    """骨架的硬性要求，逐条钉住。

    这些都是 Majo 明确提过的：
      · 侧栏要顶到窗口最上面、一整条到底；
      · 标题栏和侧栏同一行，都在顶部，左端紧贴侧栏（不重叠、不留缝）；
      · 顶部不要分割横线；
      · 顶部放一个大 logo；
      · 内部不要再一层套一层。
    只靠截图看，这五条里任何一条悄悄退化都不会有人发现。
    """
    match = re.search(r'data-layout="([^"]*)"', dom)
    if not match:
        return False, "页面没打出 data-layout，几何探针没跑（脚本报错？）"

    raw = match.group(1)
    if "missing=1" in raw:
        return False, "找不到 .sidebar / .logo img / .titlebar，骨架结构变了"

    values = {}
    for pair in raw.split(";"):
        key, _, value = pair.partition("=")
        values[key] = value

    def number(key: str) -> float:
        return float(values.get(key, "nan"))

    checks = [
        (abs(number("sidebarTop")) <= 1, "侧栏没顶到窗口最上面（sidebarTop=%s）" % values.get("sidebarTop")),
        (
            abs(number("sidebarH") - number("viewportH")) <= 1,
            "侧栏没有一整条到底（%s vs 视口 %s）" % (values.get("sidebarH"), values.get("viewportH")),
        ),
        (abs(number("barTop")) <= 1, "标题栏不在顶部（barTop=%s）" % values.get("barTop")),
        (
            abs(number("barLeft") - number("sidebarRight")) <= 1,
            "标题栏没有紧贴侧栏（left=%s, 侧栏右边=%s）"
            % (values.get("barLeft"), values.get("sidebarRight")),
        ),
        (
            # 原来这里有一条 border-bottom，就是 Majo 说的「上面分割横线」。
            # 检查 border-top 是没有意义的——它本来就是 0，永远不会红。
            values.get("barBorderBottom") in ("0px", "0", ""),
            "标题栏下面还有分割横线（border-bottom=%s）" % values.get("barBorderBottom"),
        ),
        (number("logoW") >= 40, "顶部 logo 太小（宽 %spx，要求 ≥40）" % values.get("logoW")),
        (values.get("pageLayers") == "0", "还有 %s 层 .page 包装" % values.get("pageLayers")),
        (
            # 窗口最小宽度是 960（main.go 的 MinWidth）。这条拿最小宽度当最坏情况，
            # 检查有没有元素被面板裁掉 —— 多列 grid 不换行、表格列太宽都会造成。
            values.get("clipped") == "0",
            "有 %s 个控件伸出了所在面板（被 overflow:hidden 裁掉），最小窗口下会看不全"
            % values.get("clipped"),
        ),
    ]

    # 工具条和文件列表之间不能多出一条线。
    #
    # Majo 报的原话：「任务文件列表上面板似乎被分成了两个，中间还有分割线，不应该」。
    # 根因是进度条的 track：`height:2px; background:var(--panel-2); margin:8px 0 10px`
    # —— 进度为 0 时它仍然画一条横贯全宽的浅色线，悬在工具条和列表中间，
    # 什么都没表示却把上面那块切成了两半。
    #
    # 三个值分别对应修法的三处，缺一条都拦不住回归：
    #   · 底色必须透明（不能自己画线）；
    #   · 必须紧贴列表上沿（gap ≤ 0，靠 -1px 负边距压住表格的上边框）；
    #   · 静止时填充宽度必须是 0（跑完不复位的话会留一条常驻蓝线）。
    raw_band = values.get("progressBand", "missing")
    if raw_band not in ("", "missing"):
        parts = raw_band.split("|")
        if len(parts) == 3:
            bg, gap, fill_w = parts[0], int(parts[1]), int(parts[2])
            if _alpha(bg) not in (0.0, None):
                return False, (
                    "进度条静止时会画出一条横贯全宽的线（background=%s）——"
                    "看着就是「工具条和列表之间多了一条分割线」" % bg
                )
            if gap > 0:
                return False, (
                    "进度条没贴在文件列表上沿（离了 %dpx）——"
                    "悬在工具条和列表中间就会被当成一条分割线" % gap
                )
            if fill_w != 0:
                return False, (
                    "静止时进度条还填着 %dpx —— 跑完没复位，会留下一条常驻蓝线" % fill_w
                )

    for ok, message in checks:
        if not ok:
            return False, message

    # 禁用输入框的可读性。
    #
    # 门限 3.5 是量出来的，不是拍的：
    #   · 改坏之前（opacity:.5 压整条）实测 1.87:1 —— 字基本糊在底色里；
    #   · 现在（禁用态只降背景和边框，文字保持 --faint）实测 3.95:1。
    # 3.5 卡在两者中间，既高于 WCAG 大字号 3:1 的底线，又留了 0.45 的余量。
    # 颜色和背景都是写死的十六进制，没有随机性，所以这点余量足够稳。
    raw_ua = values.get("uaPlaceholder", "missing")
    if raw_ua not in ("", "missing"):
        parts = raw_ua.split("|")
        if len(parts) == 3:
            fg, bg, opacity = _rgb(parts[0]), _rgb(parts[1]), parts[2]
            if fg and bg:
                if float(opacity or "1") < 1:
                    return False, (
                        "禁用输入框又用 opacity 压整条了（opacity=%s）——"
                        "字会跟着一起变淡，应该改背景和文字色" % opacity
                    )
                ratio = contrast_ratio(fg, bg)
                if ratio < 3.5:
                    return False, (
                        "禁用输入框里的占位符看不清（对比度 %.2f:1，要求 ≥3.5；"
                        "前景 %s / 背景 %s）" % (ratio, parts[0], parts[1])
                    )

    return True, ""


def main() -> int:
    out_dir = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else REPO_ROOT / "build" / "shots"
    out_dir.mkdir(parents=True, exist_ok=True)

    stamp = time.strftime("%H%M%S")
    temp_root = pathlib.Path(os.environ.get("TEMP", "/tmp"))
    # 每次都用一个全新目录：这个环境里删目录会撞上 safe-delete，
    # 与其想办法删，不如根本不删。
    stage = temp_root / ("vsr-shots-" + stamp)
    site = stage / "site"
    profile = stage / "chrome-profile"
    site.mkdir(parents=True, exist_ok=True)

    build_preview(site / "index.html")
    # 空队列是另一条渲染分支，单独搭一份页面 —— 不这么做它永远测不到。
    build_preview(site / "empty.html", items=[])
    # 记录页的空状态同理，而且是**两种**说法：一条记录都没有 / 筛完没剩下。
    # 不搭一份空记录的页面，第一种永远只是纸上的分支。
    build_preview(site / "histempty.html", history=[])
    # 分页得用大样本才验得动：8 条 / 5 条时页数永远是 1，
    # 「第 2 页画的是第 51..100 条」「尾页只剩 20 条」一条都照不出来。
    build_preview(site / "tasksmany.html", items=big_items())
    build_preview(site / "histmany.html", history=big_history())
    # 标题栏那个图标。真实运行时它和 index.html 一起被 //go:embed 编进 exe，
    # 静态服务这边要单独拷一份，否则截图里会是一个碎图。
    icon = INDEX.parent / "icon.png"
    if icon.is_file():
        shutil.copyfile(icon, site / "icon.png")
    else:
        print("警告：frontend/icon.png 不存在，先跑 python tools/make_icon.py")

    port = free_port()
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(site))
    server = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    print(f"静态服务：http://127.0.0.1:{port}/")

    chrome = find_chrome()
    base = f"http://127.0.0.1:{port}/index.html"

    failures = []
    digests = {}
    for view in VIEWS:
        url = f"{base}#{view}"

        # 探针和截图必须用同一个视口尺寸，否则断言量的不是截图里那一张。
        ok, reason = view_state(chrome, profile, url, view, size=WINDOW_SIZE)
        if not ok:
            failures.append(view)
            print(f"  ❌ {view:10s} {reason}")
            continue

        shot = stage / f"shot-{view}.png"
        run_chrome(chrome, profile, [f"--window-size={WINDOW_SIZE}", f"--screenshot={shot}"], url)

        if not (shot.is_file() and shot.stat().st_size > 2000):
            failures.append(view)
            print(f"  ❌ {view:10s} 没有产出截图")
            continue

        digest = hashlib.md5(shot.read_bytes()).hexdigest()[:12]
        # 视图没切成功时，几张图会**一模一样而且不报任何错**。
        # 这里主动喊出来，别让人对着一堆重复图以为成功了。
        if digest in digests:
            failures.append(view)
            print(f"  ❌ {view:10s} 与 {digests[digest]} 内容完全相同 —— 视图没切换")
            continue
        digests[digest] = view

        target = out_dir / f"界面-{view}.png"
        shutil.copyfile(shot, target)
        print(f"  ✅ {view:10s} → {target}  ({shot.stat().st_size} 字节, {digest})")

    # 设置页的六个分组各截一张。一次只显示一组，不切过去就看不见别的组长什么样。
    settings_digest = next((d for d, name in digests.items() if name == "settings"), None)

    for group, label in SETTINGS_GROUPS:
        url = f"{base}#settings+group:{group}"
        ok, reason = view_state(chrome, profile, url, "settings", size=WINDOW_SIZE)
        if not ok:
            failures.append("设置-" + group)
            print(f"  ❌ 设置-{label:8s} {reason}")
            continue

        shot = stage / f"shot-set-{group}.png"
        run_chrome(chrome, profile, [f"--window-size={WINDOW_SIZE}", f"--screenshot={shot}"], url)
        digest = hashlib.md5(shot.read_bytes()).hexdigest()[:12] if shot.is_file() else ""
        if not (shot.is_file() and shot.stat().st_size > 2000):
            failures.append("设置-" + group)
            print(f"  ❌ 设置-{label:8s} 没有产出截图")
            continue

        # 默认分组这一张**必须**和 #settings 相同：直接点进设置页看到的就该是它。
        # 反过来，它和其它任何一张相同都说明分组没切过去。
        if group == SETTINGS_DEFAULT_GROUP:
            if digest != settings_digest:
                failures.append("设置-" + group)
                print(
                    f"  ❌ 设置-{label:8s} 和「设置」页首屏不一致 —— "
                    f"默认分组可能改了（期望 {settings_digest}，实得 {digest}）"
                )
                continue
            target = out_dir / f"设置-{label}.png"
            shutil.copyfile(shot, target)
            print(f"  ✅ 设置-{label:8s} → {target}  ({shot.stat().st_size} 字节, {digest})  = 默认分组")
            continue

        if digest in digests:
            failures.append("设置-" + group)
            print(f"  ❌ 设置-{label:8s} 截图与 {digests[digest]} 相同 —— 分组没切过去")
            continue

        digests[digest] = "设置-" + group
        target = out_dir / f"设置-{label}.png"
        shutil.copyfile(shot, target)
        print(f"  ✅ 设置-{label:8s} → {target}  ({shot.stat().st_size} 字节, {digest})")

    # 确认弹窗单独截一张，并且跑一遍行为探针。
    # 它是 fixed 遮罩盖在整页上，长得对不对只有截图看得出来；
    # 但「Esc 取消」「点外面关掉」「点卡片里面不关」这些只有探针能证明。
    # 探针和截图必须分两次跑：探针会把弹窗开开关关一整轮，
    # 收尾时它是关着的 —— 拿同一次访问去截图，截到的是没有弹窗的任务页
    # （踩过：两张图字节数一模一样，差点当成「弹窗长得跟任务页一样」）。
    ok, reason = check_modal(chrome, profile, f"{base}#tasks+modal+modalprobe")
    if not ok:
        failures.append("modal")
        print(f"  ❌ 确认弹窗    {reason}")
    else:
        modal = stage / "shot-modal.png"
        run_chrome(
            chrome, profile,
            [f"--window-size={WINDOW_SIZE}", f"--screenshot={modal}"],
            f"{base}#tasks+modal",
        )
        digest = hashlib.md5(modal.read_bytes()).hexdigest()[:12] if modal.is_file() else ""
        if modal.is_file() and modal.stat().st_size > 2000 and digest not in digests:
            target = out_dir / "界面-确认弹窗.png"
            shutil.copyfile(modal, target)
            print(f"  ✅ 确认弹窗    → {target}  ({modal.stat().st_size} 字节, {digest})")
        else:
            failures.append("modal")
            print("  ❌ 确认弹窗 截图没有弹窗（与 %s 相同），或没产出" % digests.get(digest, "?"))

    # 空队列。它走的是另一条渲染分支，跟上面那 12 张图完全不重叠 ——
    # 「表格和空状态同时显示」这个 bug 在有数据的截图里根本不会出现。
    empty_base = base.replace("/index.html", "/empty.html")
    ok, reason = check_empty_state(chrome, profile, f"{empty_base}#tasks+emptyprobe")
    if not ok:
        failures.append("空队列")
        print(f"  ❌ 空队列      {reason}")
    else:
        empty_shot = stage / "shot-empty.png"
        run_chrome(
            chrome, profile,
            [f"--window-size={WINDOW_SIZE}", f"--screenshot={empty_shot}"],
            f"{empty_base}#tasks",
        )
        digest = hashlib.md5(empty_shot.read_bytes()).hexdigest()[:12] if empty_shot.is_file() else ""
        if empty_shot.is_file() and empty_shot.stat().st_size > 2000 and digest not in digests:
            target = out_dir / "界面-任务-空队列.png"
            shutil.copyfile(empty_shot, target)
            print(f"  ✅ 空队列      → {target}  ({empty_shot.stat().st_size} 字节, {digest})")
        else:
            failures.append("空队列")
            print("  ❌ 空队列 截图没产出，或与 %s 相同" % digests.get(digest, "?"))

    # 数字输入框的增减按钮（设置页有四个，拿它当样本）。
    ok, reason = check_numbers(chrome, profile, f"{base}#settings+numprobe")
    if not ok:
        failures.append("数字输入框")
        print(f"  ❌ 数字输入框   {reason}")
    else:
        print("  ✅ 数字输入框   − / + 全换上、真的加 1、到顶到底会置灰")

    # 下拉框弹出列表的配色。弹出列表抓不到，所以钉住它的因（select 不能透明）。
    ok, reason = check_selects(chrome, profile, f"{base}#templates+selectprobe")
    if not ok:
        failures.append("下拉框")
        print(f"  ❌ 下拉框      {reason}")
    else:
        print("  ✅ 下拉框      没有透明的 select（弹出列表会跟着变深）")

    # 槽位编辑器的行为探针。跑一次 Chrome，不改动截图。
    ok, reason = check_slots(chrome, profile, f"{base}#templates+slotprobe")
    if not ok:
        failures.append("slots")
        print(f"  ❌ 槽位编辑器  {reason}")
    else:
        print("  ✅ 槽位编辑器   一行一段竖排 / 字段常驻 / 换位删除 / 焦点在整行 / 预览行数都对")

    # 模板列表的右键菜单：重命名 / 复制 / 删除。
    # 名称现在**只**能从这儿改，所以这条路断了就没有改名的入口。
    ok, reason = check_tpl_menu(chrome, profile, f"{base}#templates+menuprobe")
    if not ok:
        failures.append("模板菜单")
        print(f"  ❌ 模板菜单    {reason}")
    else:
        print("  ✅ 模板菜单    右键选中 / 重命名就地改 / 复制切过去 / 删除要确认 / 最后一份置灰")

    # 拖放遮罩只认文件。用户报过「拖文本也会显示松开鼠标加进队列」。
    ok, reason = check_drag(chrome, profile, f"{base}#tasks+dragprobe")
    if not ok:
        failures.append("拖放遮罩")
        print(f"  ❌ 拖放遮罩    {reason}")
    else:
        print("  ✅ 拖放遮罩    拖文本/拖槽位不亮，拖文件才亮，离开和松手都收得掉")

    # 滚动条。截图带 --hide-scrollbars，看不见它，只能读伪元素的计算值。
    ok, reason = check_scrollbars(chrome, profile, f"{base}#tasks+scrollprobe")
    if not ok:
        failures.append("滚动条")
        print(f"  ❌ 滚动条      {reason}")
    else:
        print("  ✅ 滚动条      自己画的（10px / #232833 / 圆角 / 透明轨道），"
              "模板页那两处照旧藏着")

    # 记录页的勾选 / 批量还原 / 筛选 / 搜索。跑一次 Chrome，不改动截图。
    ok, reason = check_history(chrome, profile, f"{base}#history+histprobe")
    if not ok:
        failures.append("记录页")
        print(f"  ❌ 记录页      {reason}")
    else:
        print("  ✅ 记录页      只能勾可还原的 / 按钮数字=真正会动的文件数 / 筛选搜索都对")

    # 一条记录都没有的那一版。跟「筛完没剩下」是两种说法，得分开验。
    hist_empty_base = base.replace("/index.html", "/histempty.html")
    ok, reason = check_history_empty(chrome, profile, f"{hist_empty_base}#history+histprobe+histempty")
    if not ok:
        failures.append("记录空态")
        print(f"  ❌ 记录空态    {reason}")
    else:
        print("  ✅ 记录空态    没有记录时说「还没有改名记录」，不是「没有符合条件的记录」")

    # 分页。8 条 / 5 条的桩页页数永远是 1，验不动 —— 换成 120 条的样本页。
    #
    # 分页出错的方式很特别：它**不报错**，只是画错一截。翻到第 2 页看到的是
    # 第 1..50 条，界面照样一片正常。所以除了数行数，还得钉住「这一页的
    # 第一条到底是哪一条」—— 光数行数，「每页都画前 50 条」这种错照样能过。
    many_tasks = base.replace("/index.html", "/tasksmany.html")
    ok, reason = check_task_page(chrome, profile, f"{many_tasks}#tasks+taskpage")
    if not ok:
        failures.append("任务分页")
        print(f"  ❌ 任务分页    {reason}")
    else:
        print("  ✅ 任务分页    默认 50 / 可选 50·100·200·300 / 120 条切成 50·50·20 / 换档回第 1 页")

    # 记录页的分页 + 时间范围。同一份 120 条的样本也拿来验日期筛选：
    # 样本的时间固定在 2026-02，所以「今天」「本月」必须命中 0 条 ——
    # 预设要是写成了「全都返回」，只有这种「故意取过去的日子」的样本照得出来。
    many_hist = base.replace("/index.html", "/histmany.html")
    ok, reason = check_history_page(chrome, profile, f"{many_hist}#history+histpage")
    if not ok:
        failures.append("记录分页")
        print(f"  ❌ 记录分页    {reason}")
    else:
        print("  ✅ 记录分页    分页同任务页 / 日期框按需露出 / 起止同一天不漏 / 范围外说「没有符合条件」")

    # 勾选态单独截一张：表头全选框和「还原所选（3）」平时是看不见的，
    # 不勾上就截不到，那张图也就证明不了它们长什么样。
    picked = stage / "shot-histpick.png"
    run_chrome(
        chrome, profile,
        [f"--window-size={WINDOW_SIZE}", f"--screenshot={picked}"],
        f"{base}#history+histprobe+histpick",
    )
    digest = hashlib.md5(picked.read_bytes()).hexdigest()[:12] if picked.is_file() else ""
    if picked.is_file() and picked.stat().st_size > 2000 and digest not in digests:
        target = out_dir / "界面-记录-已勾选.png"
        shutil.copyfile(picked, target)
        print(f"  ✅ 记录勾选态   → {target}  ({picked.stat().st_size} 字节, {digest})")
    else:
        failures.append("histpick")
        print("  ❌ 记录勾选态 截图没产出，或与 %s 相同（没真的勾上）"
              % digests.get(digest, "?"))

    # 这里原来还有一张「模板-展开某一段」的截图。
    # 补充设置现在常驻（一行一段竖着排），没有「展开」这个状态，
    # 上面那张 界面-templates.png 截到的就是全貌，再单独截一张是同一张图。

    # 右键菜单单独截一张：它只在右键时出现，截图脚本没法「右键」——
    # 不专门弹一次的话，这块新 UI 从没被看见过。
    menu_shot = stage / "shot-ctxmenu.png"
    run_chrome(
        chrome, profile,
        [f"--window-size={WINDOW_SIZE}", f"--screenshot={menu_shot}"],
        f"{base}#templates+ctxmenu",
    )
    digest = hashlib.md5(menu_shot.read_bytes()).hexdigest()[:12] if menu_shot.is_file() else ""
    if menu_shot.is_file() and menu_shot.stat().st_size > 2000 and digest not in digests:
        target = out_dir / "界面-模板-右键菜单.png"
        shutil.copyfile(menu_shot, target)
        print(f"  ✅ 模板右键菜单 → {target}  ({menu_shot.stat().st_size} 字节, {digest})")
    else:
        failures.append("ctxmenu")
        print("  ❌ 模板右键菜单 截图没产出，或与 %s 相同（菜单没弹出来）"
              % digests.get(digest, "?"))

    # 「这一段被选中」单独截一张。选中态是**交互之后才有**的，静态截图里看不到它，
    # 而这一轮改的正是它（用户报「命名那边的块选中时样式有问题」）。
    # 不单独截的话，修完没人看得见，下一轮改 CSS 也没人知道碰坏了什么。
    focus_shot = stage / "shot-slotfocus.png"
    run_chrome(
        chrome, profile,
        [f"--window-size={WINDOW_SIZE}", f"--screenshot={focus_shot}"],
        f"{base}#templates+slotfocus",
    )
    digest = hashlib.md5(focus_shot.read_bytes()).hexdigest()[:12] if focus_shot.is_file() else ""
    if focus_shot.is_file() and focus_shot.stat().st_size > 2000 and digest not in digests:
        target = out_dir / "界面-模板-选中某一段.png"
        shutil.copyfile(focus_shot, target)
        print(f"  ✅ 模板选中态   → {target}  ({focus_shot.stat().st_size} 字节, {digest})")
    else:
        failures.append("slotfocus")
        print("  ❌ 模板选中态 截图没产出，或与 %s 相同（没真的点着）"
              % digests.get(digest, "?"))

    # 最小窗口：960×620，就是 main.go 里 MinWidth/MinHeight 那个尺寸。
    #
    # 曾经这里是 900×760 —— 比窗口最小宽度还窄，是个用户永远到不了的状态，
    # 截图好看也没意义。而且注释写着「确认多列布局收成单列」，但 CSS 里
    # 根本没有媒体查询，它从来没收过。假信号比没有信号更糟，一并改掉：
    # 现在压的是「最小宽度下不出现横向滚动条」，这才是真正会翻车的地方。
    narrow = stage / "shot-narrow.png"
    narrow_url = f"{base}#settings"
    ok, reason = view_state(chrome, profile, narrow_url, "settings", size=MIN_WINDOW_SIZE)
    if not ok:
        failures.append("narrow")
        print(f"  ❌ 最小窗口    {reason}")
    else:
        run_chrome(chrome, profile, [f"--window-size={MIN_WINDOW_SIZE}", f"--screenshot={narrow}"], narrow_url)
        if narrow.is_file() and narrow.stat().st_size > 2000:
            target = out_dir / "界面-设置-最小窗口.png"
            shutil.copyfile(narrow, target)
            print(f"  ✅ 最小窗口    → {target}  ({narrow.stat().st_size} 字节)")
        else:
            failures.append("narrow")
            print("  ❌ 最小窗口 没有产出截图")

    server.shutdown()

    print()
    if failures:
        print("失败：" + "、".join(failures))
        return 1
    print(f"全部成功，截图在 {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
