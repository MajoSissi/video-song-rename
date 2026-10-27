/* 前端预览桩：把 Wails 注入的 window.go / window.runtime 换成假数据。
   只用于在浏览器里看排版，不参与构建，也不进 exe。 */
(function () {
  const wait = (v, ms) => new Promise((r) => setTimeout(() => r(v), ms || 12));

  // 命名规则是**槽位**形状：separator 是全局分隔符，slots 按顺序拼。
  // 每一格的 separator 为 null 表示「跟随全局」，为空串表示「明确不要分隔符」。
  //
  // 全局只有这一份 —— 模板库删掉之后，这一段就是「命名」页的全部内容。
  let naming = {
    separator: ' - ',
    slots: [
      { var: 'artist' },
      { var: 'title', script: 'en' },
      { var: 'title', script: 'zh' },
    ],
    unmatchedPrefix: '0_',
    unmatchedSuffix: '',
    maxNameRunes: 80,
    // 去重是默认行为（Go 那边零值就是 false = 去重），桩里写出来是为了
    // 让截图上的勾选框有明确来源，而不是靠「读不到就当 true」。
    keepDuplicates: false,
    move: {
      // 默认是关的（和 config.Default() 一致）。截图要拍的是「第一次打开
      // 这一页看到的样子」—— 开着的话那一段会把下面「命名预览」挤出去。
      enabled: false,
      mode: 'sibling',
      dir: '',
      name: '',
      prefix: '',
      suffix: '_已整理',
      matched_only: true,
    },
  };

  // 变量 / 语种 / 分隔符目录。真实的那份在 Go（naming.Choices 等），
  // 这里用 build_preview.py 注入的那份 —— 它和 tools/shoot_ui.py 读的是
  // **同一个** naming-options.json，frontend_contract_test.go 会逐项比对。
  const namingOptions = window.__namingOptions;

  const cfg = {
    segment_seconds: 12,
    concurrency: 3,
    retries: 2,
    request_interval_ms: 1500,
    language: 'zh',
    country: 'CN',
    timeout_seconds: 20,
    name_template: { separator: ' - ', slots: [{ var: 'artist' }, { var: 'title' }] },
    move: naming.move,
    unmatched_prefix: '0_',
    unmatched_suffix: '',
    extensions: ['mp4', 'mkv', 'mov', 'avi', 'mp3', 'flac', 'm4a', 'wav'],
    recursive: true,
    skip_processed: true,
    dry_run: false,
    max_name_runes: 80,
    title_preference: ['title', 'subtitle'],
    user_agent: '',
    close_to_tray: true,
    proxy: '',
  };

  const mk = (i, over) => Object.assign({
    index: i,
    path: 'D:\\Media\\演唱会\\录屏\\' + (i + 1) + '.mp4',
    dir: 'D:\\Media\\演唱会\\录屏',
    name: (i + 1) + '.mp4',
    ext: '.mp4',
    size: 40 * 1024 * 1024 + i * 7340032,
    status: 'renamed',
    title: '', artist: '', album: '', genre: '', released: '', label: '',
    coverArt: '', shazamUrl: '',
    newName: '', newPath: '', message: '', attempts: 1,
  }, over);

  const items = [
    mk(0, {
      status: 'renamed', size: 583 * 1024 * 1024, name: 'VID_20251003_211233.mp4',
      title: '晴天', artist: '周杰伦', album: '叶惠美', released: '2003-07-31',
      genre: 'Pop', label: 'Sony Music', attempts: 1,
      shazamUrl: 'https://www.shazam.com/song/1',
      newName: '晴天 - 周杰伦.mp4', newPath: 'D:\\Media\\演唱会\\录屏\\晴天 - 周杰伦.mp4',
    }),
    mk(1, {
      status: 'matched', size: 214 * 1024 * 1024, name: 'rec_0042.mkv',
      title: '起风了', artist: '买辣椒也用券', album: '单曲', released: '2017-11-06',
      genre: 'Mandopop', label: '独立发行', attempts: 2,
      shazamUrl: 'https://www.shazam.com/song/2',
      newName: '起风了 - 买辣椒也用券.mkv',
    }),
    mk(2, {
      status: 'running', size: 96 * 1024 * 1024, name: 'screen-2025-10-05 19-22-01.mkv',
      message: '正在识别第 12 秒窗口…', attempts: 1,
    }),
    mk(3, {
      status: 'no_match', size: 31 * 1024 * 1024, name: 'untitled_clip_final.mp4',
      message: '整段 12 秒都没匹配上', attempts: 2,
      newName: '0_untitled_clip_final.mp4',
    }),
    mk(4, {
      status: 'failed', size: 8 * 1024 * 1024, name: 'broken-sample.wav',
      message: '解码失败：不是合法的音频流', attempts: 3,
    }),
    mk(5, {
      status: 'skipped', size: 640 * 1024 * 1024, name: '晴天 - 周杰伦.mp4',
      title: '晴天', artist: '周杰伦', message: '文件名已是目标格式，跳过',
    }),
    mk(6, {
      status: 'pending', size: 118 * 1024 * 1024, name: 'PXL_20251006_001122.mp4',
    }),
    mk(7, {
      status: 'preview', size: 77 * 1024 * 1024, name: '直播回放 03.mp4',
      title: 'Lemon', artist: '米津玄師', album: 'BOOTLEG', released: '2018-03-14',
      genre: 'J-Pop', label: 'SME Records', attempts: 1,
      shazamUrl: 'https://www.shazam.com/song/3',
      newName: 'Lemon - 米津玄師.mp4', message: '预览模式：没有真的改',
    }),
    // 只搬走、没改名的条目：状态是「已归档」而不是「已改名」。
    // 少了这一条，那个状态徽标的样式就永远没被截图看见过。
    mk(9, {
      status: 'moved', size: 152 * 1024 * 1024, name: 'Hotel California.flac',
      title: 'Hotel California', artist: 'Eagles', attempts: 1, moved: true,
      newName: 'Hotel California.flac',
      newPath: 'D:\\Media\\演唱会\\录屏_已整理\\Hotel California.flac',
      message: '已移动到 D:\\Media\\演唱会\\录屏_已整理',
    }),
    // 限流也是一种「失败」，但界面上必须一眼分得出来：它有自己的颜色、
    // 自己的筛选项。缺这一条，那块样式就永远没被截图看见过。
    mk(8, {
      status: 'rate_limited', size: 64 * 1024 * 1024, name: 'live-set-02.mkv',
      message: '被 Shazam 限流（HTTP 429），等几分钟后重试', attempts: 2,
    }),
  ];

  // 记录页的四种形态都放一条：可还原、已还原、文件已不在，
  // 以及一条未识别的（前缀改名）。缺哪种，那一格的样式就没被截图看见过。
  //
  // nowPath 是文件**实际**在哪，绝大多数时候就等于 to。它和 to 不一样
  // 只有两种情况，两种都得有一条：
  //   · 这条已经还原过了（第 2 条）—— 文件被改回原名了，那是还原的结果；
  //   · 用户在程序外手动改过名（第 5 条）—— 界面要显示实际名字并挂「已改名」。
  // 少一条，就有一整条分支从没被截图看见过。
  const history = [
    { from: 'D:\\Media\\演唱会\\录屏\\VID_20251003_211233.mp4', to: 'D:\\Media\\演唱会\\录屏\\晴天 - 周杰伦.mp4', nowPath: 'D:\\Media\\演唱会\\录屏\\晴天 - 周杰伦.mp4', ts: Date.now() - 3 * 60000, restored: false, exists: true },
    { from: 'D:\\Media\\演唱会\\录屏\\rec_0041.mkv', to: 'D:\\Media\\演唱会\\录屏\\夜曲 - 周杰伦.mkv', nowPath: 'D:\\Media\\演唱会\\录屏\\rec_0041.mkv', ts: Date.now() - 6 * 60000, restored: true, exists: true },
    { from: 'D:\\Media\\演唱会\\录屏\\a.mp4', to: 'D:\\Media\\演唱会\\录屏\\0_a.mp4', nowPath: 'D:\\Media\\演唱会\\录屏\\0_a.mp4', ts: Date.now() - 9 * 60000, restored: false, exists: true },
    { from: 'D:\\Media\\演唱会\\录屏\\old.mp4', to: 'D:\\Media\\演唱会\\录屏\\稻香 - 周杰伦.mp4', nowPath: 'D:\\Media\\演唱会\\录屏\\稻香 - 周杰伦.mp4', ts: Date.now() - 26 * 60000, restored: false, exists: false },
    { from: 'D:\\Media\\演唱会\\录屏\\rec_0042.mkv', to: 'D:\\Media\\演唱会\\录屏\\烟花易冷 - 周杰伦.mkv', nowPath: 'D:\\Media\\演唱会\\录屏\\烟花易冷 我改的名字.mkv', ts: Date.now() - 34 * 60000, restored: false, exists: true },
  ];

  const progress = {
    total: 8, done: 5, matched: 2, unmatched: 1, failed: 1, skipped: 1, renamed: 1, running: true,
  };

  const events = {};
  // OnFileDrop 注册进来的回调。见文件末尾那段说明。
  let onFileDrop = null;

  const App = {
    PickFiles: () => wait(['D:\\Media\\演唱会\\录屏']),
    PickFolder: () => wait('D:\\Media\\演唱会\\录屏'),
    // 设置页「ffmpeg 路径」旁边那颗「浏览…」。
    // 少了它，点一下就在调用点抛 TypeError —— 预览页里那个按钮是死的。
    PickFFmpeg: () => wait('C:\\Users\\Majo\\AppData\\Local\\Microsoft\\WinGet\\Links\\ffmpeg.exe'),
    ScanPath: () => wait(items),
    AddPaths: () => wait(items),
    Items: () => wait(items),
    // 还原之后让队列跟上磁盘。**不新增也不删除**，只更新已入列的条目，
    // 所以这里照原样回一份 items 就够预览用了。
    ResyncQueue: () => wait(items),
    RemoveItems: () => wait(items),
    Clear: () => wait(null),
    // 重置：条目一个不少，只是每一行的状态回到「待处理」。
    // 桩里要**真的**改 items —— 预览页点一下就能看到表格从一片彩色变回灰的，
    // 否则这颗按钮在预览里永远是死的，样式也验不出来。
    Reset: () => {
      for (const item of items) {
        item.status = 'pending';
        item.message = '';
        item.newName = '';
        item.newPath = '';
        item.attempts = 0;
        item.moved = false;
      }
      return wait(items.slice());
    },
    Start: () => wait(null),
    Pause: () => wait(null),
    Resume: () => wait(null),
    Stop: () => wait(null),
    Progress: () => wait(progress),
    PreviewCommand: () => wait('shazam-sidecar.exe --probe "D:\\Media\\演唱会\\录屏\\rec_0042.mkv" --window 12'),
    ExportCSV: () => wait('D:\\Media\\演唱会\\录屏\\video-song-rename-20251007.csv'),
    OpenURL: () => wait(null),
    RevealPath: () => wait(null),
    OpenLog: () => wait(null),
    OpenConfigFolder: () => wait(null),
    CloseWindow: () => wait(null),
    Naming: () => wait(naming),
    NamingOptions: () => wait(namingOptions),
    SaveNaming: (input) => { naming = Object.assign({}, input); return wait(naming); },
    // 归档位置的试算。桩里只按 sibling 规则拼一个给人看的路径，
    // 真正的判断在 Go（config.MoveRule.TargetDir）。
    PreviewMove: (rule) => wait(
      !rule || !rule.enabled
        ? { dir: '', ok: false, note: '不移动，文件改名后留在原目录' }
        : {
          dir: 'D:\\Media\\演唱会\\' + (rule.name || '录屏') + (rule.suffix || ''),
          ok: true,
          note: '目录不存在会自动创建；遇到同名文件会加 (1)，不会覆盖',
        }),
    PickMoveFolder: () => wait('D:\\Media\\归档'),
    // 桩里的预览文本是**编的**，只为把版式画出来。
    // 真正的渲染只有 Go 那一份（naming.Template.Render）。
    PreviewNaming: () => wait([
      { label: '全都识别到', note: '艺人、英文名、中文名、年份都有',
        text: 'BTS - Spring Day - 春日', empty: false },
      { label: '缺中文歌名', note: '只有英文写法 —— 中文那一格整个消失，不留分隔符',
        text: 'BTS - Spring Day', empty: false },
      { label: '只识别到歌名', note: '艺人和专辑都没识别到',
        text: 'Spring Day', empty: false },
      { label: '完全没识别到', note: '走「未识别」前缀，规则不参与',
        text: '0_VID_20240103_210455', empty: false },
    ]),
    History: () => wait(history),
    // 移除记录。桩里按「勾了几条就少几条」真的改一遍这份数组，
    // 这样界面上「移除所选（2）」和点完之后剩下的行数能对上 ——
    // 返回一个原样不动的列表，截图里就看不出这个功能到底有没有生效。
    RemoveHistory: (targets) => {
      const drop = new Set(targets || []);
      for (let i = history.length - 1; i >= 0; i--) {
        if (drop.has(history[i].to)) history.splice(i, 1);
      }
      return wait(history);
    },
    // 形状必须和 Go 的 job.UndoResult 一致（restored / missing / skipped / details）。
    // 以前这里写的是 {restored, failed, message}，界面上打出来的是 undefined。
    Undo: () => wait({ restored: 1, missing: 0, skipped: 0, details: [] }),
    UndoOne: (to) => wait({
      restored: 1, missing: 0, skipped: 0,
      details: ['（桩数据）已把 ' + String(to).split(/[\\/]/).pop() + ' 改回原名'],
    }),
    // 批量还原。参数是路径数组，返回的形状和 UndoOne 一样 ——
    // 桩里按「勾了几个就还原几个」回，这样界面上的「还原所选（3）」
    // 和日志里的「成功 3」能对上，截图才有参考价值。
    UndoMany: (to) => wait({
      restored: (to || []).length, missing: 0, skipped: 0,
      details: (to || []).map((p) => '（桩数据）已把 ' + String(p).split(/[\\/]/).pop() + ' 改回原名'),
    }),
    GetConfig: () => wait(cfg),
    SaveConfig: (patch) => wait(Object.assign({}, cfg, patch)),
    // 代理连通性探测。桩里固定返回「通」，真实实现走一次真的 Shazam 请求。
    TestNetwork: (proxy) => wait({
      ok: true, status: 'network_ok',
      proxy: proxy || '跟随环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）',
      http_status: 400, elapsed_ms: 186, error: '',
    }),
    SelfCheck: () => wait({
      ok: true,
      sidecarPath: 'D:\\Code-Project\\video-song-rename\\dist\\shazam-sidecar.exe',
      // 必须和 `shazam-sidecar.exe --env` 的 version 字段同形：只有版本号。
      // 这里曾经写 'shazam-sidecar 0.3.1 (rodio)'，于是截图显示成带产品名的样子，
      // 而真机上顶栏只有 '0.1.0' —— 预览和真机不一致，截图就失去了意义。
      sidecarVersion: '0.1.0',
      ffmpegPath: 'C:\\Program Files\\ffmpeg\\bin\\ffmpeg.exe',
      dataDir: 'D:\\Code-Project\\video-song-rename\\dist\\data',
      configPath: 'D:\\Code-Project\\video-song-rename\\dist\\data\\config.json',
      logPath: 'D:\\Code-Project\\video-song-rename\\dist\\data\\video-song-rename.log',
      problem: '', hint: '', configError: '',
    }),
  };

  window.go = { main: { App: App } };
  window.runtime = {
    WindowMinimise() {}, WindowHide() {}, WindowShow() {}, WindowUnminimise() {},
    WindowToggleMaximise() {}, Quit() {},
    WindowIsMaximised: () => wait(false),
    EventsOn: (name, cb) => { (events[name] = events[name] || []).push(cb); },
    EventsOff() {}, EventsEmit() {},
    BrowserOpenURL() {}, ClipboardSetText: () => wait(true),
    LogPrint() {},
    // OnFileDrop 是**必须有的**：Wails 是由这个调用去装 DOM 的
    // dragover/drop 处理器的，前端在启动时就会调它。
    // 少了它，主脚本会在这一行抛 TypeError，**后面的代码全部不执行** ——
    // 导航点击、boot()、模板加载统统没注册，页面看起来「能显示但全是死的」。
    // 踩过：这个桩漏了它，于是 ?view=templates 切不过去、data-boot 永远不出现，
    // 排查了半天以为是时序问题。
    // 接住回调并暴露成 window.__dropFiles(paths)，好在预览页里手动模拟拖入。
    OnFileDrop: (cb) => { onFileDrop = cb; },
  };
  window.__stubEvents = events;
  window.__dropFiles = (paths) => onFileDrop && onFileDrop(0, 0, paths);

  // ?cooldown=45：让顶栏那个「限流冷却」倒计时出现。
  //
  // 冷却只在真的被 Shazam 挡下来时才有，静态预览里根本看不到 ——
  // 于是这块样式永远没被截图验证过。桩的数据本来就是编的，补一条进度事件即可。
  const cooldownSeconds = Number(new URLSearchParams(location.search).get('cooldown') || 0);
  if (cooldownSeconds > 0) {
    setTimeout(() => {
      (events['job:progress'] || []).forEach((cb) => cb({
        total: 12, done: 4, matched: 3, unmatched: 1, failed: 0, skipped: 0,
        rateLimited: 2, running: true,
        cooldownUntil: Date.now() + cooldownSeconds * 1000,
      }));
    }, 200);
  }
})();
