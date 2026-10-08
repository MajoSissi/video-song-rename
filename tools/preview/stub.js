/* 前端预览桩：把 Wails 注入的 window.go / window.runtime 换成假数据。
   只用于在浏览器里看排版，不参与构建，也不进 exe。 */
(function () {
  const wait = (v, ms) => new Promise((r) => setTimeout(() => r(v), ms || 12));

  // 模板是**槽位**形状：separator 是全局分隔符，slots 按顺序拼。
  // 每一格的 separator 为 null 表示「跟随全局」，为空串表示「明确不要分隔符」。
  const templates = [
    { name: '只歌名', separator: ' - ', slots: [{ var: 'title' }], active: false },
    { name: '歌名 - 艺人', separator: ' - ',
      slots: [{ var: 'title' }, { var: 'artist' }], active: false },
    { name: '艺人 - 歌名', separator: ' - ',
      slots: [{ var: 'artist' }, { var: 'title' }], active: false },
    { name: '歌名 (年份)', separator: ' - ',
      slots: [{ var: 'title' },
              { var: 'year', prefix: ' (', suffix: ')', separator: '' }], active: false },
    { name: '艺人 - 英文名 - 中文名', separator: ' - ',
      slots: [{ var: 'artist' },
              { var: 'title', script: 'en' },
              { var: 'title', script: 'zh' }], active: true },
  ];

  // 变量 / 语种 / 分隔符目录。真实的那份在 Go（naming.Choices 等），
  // 这里用 build_preview.py 注入的那份 —— 它和 tools/shoot_ui.py 读的是
  // **同一个** template-options.json，frontend_contract_test.go 会逐项比对。
  const templateOptions = window.__templateOptions;

  const cfg = {
    segment_seconds: 12,
    concurrency: 3,
    retries: 2,
    language: 'zh',
    country: 'CN',
    timeout_seconds: 20,
    name_template: { separator: ' - ', slots: [{ var: 'artist' }, { var: 'title' }] },
    templates: templates,
    active_template: '艺人 - 英文名 - 中文名',
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
    RemoveItems: () => wait(items),
    Clear: () => wait(null),
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
    Templates: () => wait(templates),
    SaveTemplate: (input) => {
      const found = templates.find((t) => t.name === input.name);
      if (found) { found.separator = input.separator; found.slots = input.slots; }
      else templates.push({ name: input.name, separator: input.separator, slots: input.slots, active: false });
      return wait(templates);
    },
    SetActiveTemplate: (name) => {
      templates.forEach((t) => { t.active = (t.name === name); });
      return wait(templates);
    },
    DeleteTemplate: (name) => {
      const i = templates.findIndex((t) => t.name === name);
      if (i >= 0) templates.splice(i, 1);
      if (!templates.some((t) => t.active)) templates[0].active = true;
      return wait(templates);
    },
    RenameTemplate: (oldName, newName) => {
      const found = templates.find((t) => t.name === oldName);
      if (found) found.name = newName;
      return wait(templates);
    },
    DuplicateTemplate: (name, newName) => {
      const found = templates.find((t) => t.name === name);
      if (found) {
        // 槽位要拷一份：直接共用的话，改副本会连原件一起改，
        // 预览页上看着像「复制没生效」。
        templates.push(Object.assign({}, found, { name: newName, slots: (found.slots || []).slice() }));
      }
      return wait(templates);
    },
    ResetTemplates: () => wait(templates),
    TemplateEditorOptions: () => wait(templateOptions),
    // 桩里的预览文本是**编的**，只为把版式画出来。
    // 真正的渲染只有 Go 那一份（naming.Template.Render）。
    // 这个预览页是拿来看排版的，不是拿来验行为的。
    PreviewTemplate: (input) => wait([
      { label: '全都识别到', note: '艺人、英文名、中文名、年份都有',
        text: 'BTS - Spring Day - 春日', empty: false },
      { label: '缺中文歌名', note: '只有英文写法 —— 中文那一格整个消失，不留分隔符',
        text: 'BTS - Spring Day', empty: false },
      { label: '只识别到歌名', note: '艺人和专辑都没识别到',
        text: 'Spring Day', empty: false },
      { label: '完全没识别到', note: '走「未识别」前缀，模板不参与',
        text: '0_VID_20240103_210455', empty: false },
    ]),
    History: () => wait(history),
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
})();
