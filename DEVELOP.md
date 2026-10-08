# 开发说明

## 架构：为什么是两个进程

```
video-song-rename.exe   Go + Wails v2    界面、扫描、并发、改名、撤销、配置
        │  exec + JSON over stdout
        ▼
shazam-sidecar.exe      Rust             解码 → 指纹 → 请求 Shazam → 输出一行 JSON
```

拆成两个进程不是随手决定的：

1. **指纹算法不能重写。** Shazam 的 `signature` 是一段真正的音频指纹（16 kHz 单声道 →
   128 点 FFT → hanning 窗 → 峰值扩散/识别 → 频段峰值 → 打包成
   `data:audio/vnd.shazam.sig;base64,…`），不是「PCM 加个哈希」。用 Go 重写等于重新实现
   一套 DSP，还要逐字节对齐——已经有一份 MIT 的 Rust 实现，没必要。
2. **本机的构建环境没有 C 编译器。** `CGO_ENABLED=0`，没有 gcc/clang/MSVC，所以
   cgo 链接 Rust 静态库这条路走不通。进程边界是最省事的替代。
3. **进程边界顺带是隔离。** sidecar 崩了只影响一个文件，不会带走整个界面。

## 目录结构

```
├── main.go                      Wails 启动、窗口选项、内嵌前端与图标
├── app.go                       绑定给前端的 API（门面层）
├── tray.go                      托盘菜单的纯逻辑：菜单布局、点击动作、ICO 解析（可单测）
├── tray_windows.go              Win32 托盘：Shell_NotifyIconW + 隐藏窗口 + 自己的消息泵
├── dpi_windows.go               进程 DPI 感知声明（必须在建窗口之前）
├── frontend/index.html          整个界面。手写的单个 HTML，不走 npm、不走打包器
├── assets/icon.ico              窗口与托盘共用的图标（多尺寸）
├── tools/make_icon.py           生成 icon.ico，纯标准库，不依赖 PIL
├── internal/
│   ├── config/                  设置默认值、读写、归一化、模板库与旧配置迁移
│   ├── media/                   文件扫描、文件名清洗、唯一路径
│   ├── naming/                  标题脚本判定、优先级挑选、模板渲染
│   ├── recognizer/              sidecar 进程调用、JSON 解析、重试节奏
│   └── job/                     任务编排：并发、进度、改名、状态文件、撤销
├── sidecar/                     Rust 识别核心
│   ├── src/main.rs              CLI 入口，输出一行 JSON
│   ├── src/shazam.rs            HTTP 通信
│   ├── src/fingerprinting/      vendor 自 shazamio-core（见下）
│   ├── third_party/shazamio-core/LICENSE   上游 MIT 许可原文
│   └── tools/compare_with_upstream.py      逐字节比对工具
├── .github/workflows/build.yml  手动构建（workflow_dispatch）
├── .gitignore                   不进版本库的东西，每条都写了理由
└── build.sh                     构建两个产物并组装到 dist/
```

## 构建

```bash
./build.sh              # 发布构建
./build.sh --debug      # 调试构建（带控制台 + DevTools）
```

手动构建：

```bash
go run tools/genrsrc.go         # exe 图标与版本资源，见下
cd sidecar && cargo build --release
cd .. && go build -tags production -ldflags "-s -w -H windowsgui" -o dist/video-song-rename.exe .
cp sidecar/target/release/shazam-sidecar.exe dist/
```

### 仓库里放什么（`.gitignore`）

判断标准只有一条：**能从仓库里的源文件重新生成的东西，都不进仓库。**

被过滤掉的大头：`sidecar/target/`（1.4 GB —— 不忽略的话第一次 `git add .`
就要往仓库里塞一整个 Rust 构建目录，而且推不上去的那一刻才发现）、
`dist/`（17 MB 的组装产物，含运行时写出来的 `dist/data/`）、
`build/`（截图）、`tools/preview/index.html` 与 `tools/preview/shots/`（生成物）、
`rsrc_windows_amd64.syso`、`.workbuddy-ai/`。

反过来说，有几样**看着像产物但必须提交**，漏了它们别人 clone 下来编出来的
东西和这里不是同一份：

- `sidecar/Cargo.lock` —— 二进制项目，锁死依赖版本；
- `go.sum` —— 同上；
- `sidecar/third_party/shazamio-core/LICENSE` —— **MIT 许可要求随分发附带**，
  它被删掉比编译不过严重得多（编译不会报错）；
- `assets/icon.ico` / `frontend/icon.png` —— 图标源文件。

`.gitignore` 里每一条都写了「为什么」和「不提交的代价」。要临时放开某一项时
用 `git add -f <路径>`，**别去改那个文件** —— 改了容易忘记改回来，
而这条规则一旦松掉，下次就没人知道它原来为什么在。

### `rsrc_windows_amd64.syso` 不进版本库

它由 `tools/genrsrc.go` 从 `assets/icon.ico` 生成，`build.sh` 每次都会重跑。
**所以它不在 `.gitignore` 之外——它在里面**，新 clone 出来是没有的。

敢不提交它，是因为生成器是**确定性**的：重新生成的结果与当初手工跑出来的那份
逐字节相同（`cmp` 验过）。放在仓库里反而多一个静默失效点 ——
换了 `assets/icon.ico` 却忘了重跑生成器，exe 里就还是旧图标，且不报任何错。

代价只有一个，而且很小：**绕过 `build.sh` 直接 `go build` 不会生成它**，
编出来的 exe 在资源管理器里没有自定义图标。窗口图标和托盘图标不受影响
（那两个走运行时读 `assets/icon.ico`）。上面的手动构建步骤里因此把
`go run tools/genrsrc.go` 写在最前面。

### 手动构建的 GitHub Action

`.github/workflows/build.yml`，只有 `workflow_dispatch` 一个触发方式 ——
在 Actions 页面点「Run workflow」，选 release / debug，跑完下载 artifact。

**没有挂 push / pull_request**：一次构建要跑一遍 Rust（`sidecar/target/` 1.4 GB）
再跑一遍 Go，几分钟；每次推送都跑等于把额度烧在「本地反正也要跑一遍」的事情上。
想在合并前先验一遍的话，在 `on:` 下面临时加一行 `pull_request:` 就行。

几个刻意的选择：

- **直接调 `./build.sh`，不在这里重抄一遍构建命令。** 抄一遍的代价是
  「CI 编出来的和本地不是同一份」，而且以后要改两处。
  `windows-latest` 上 `shell: bash` 走的是 Git Bash，`set -euo pipefail` / `[[ ]]`
  / `$( )` 它都认。
- **Go 版本从 `go.mod` 读**（`go-version-file`），不在 workflow 里再写一个版本号。
  两处各写一份迟早会对不上，而对不上的表现是「本地能编、CI 报语法错误」。
- **校验产物按尺寸卡一道。** 只验「文件在不在」不够：忘了带 `-tags production`
  的话，Wails 会编出一个能启动但什么都不做的空壳，尺寸从 12 MB 掉到 5.4 MB。
  一个编坏了的 exe 比没有 exe 更糟。debug 模式跳过这条（那边本来就是小）。
- **只上传那两个 exe，不传整个 `dist/`。** `dist/data/` 是运行时写出来的配置和
  日志（本地跑过一次就会在里面），打进去等于把自己的设置和文件路径一起发出去。
  `if-no-files-found: error` 也留着：路径写错时 artifact 会静默变成空包。

### `-tags production` 不是可选项

Wails v2 的 `internal/app/app_default_windows.go` 带构建约束
`!dev && !production && !bindings && windows`，不带标签时会编出一个空的 `Run()`，
界面起来只弹一个「Wails applications will not build without the correct build tags」。

体积上也能一眼看出来：**桩 5.4 MB，真身 12 MB**。如果构建产物突然变小了，就是这个原因。

调试构建用 `-tags dev`，会带上 DevTools（F12）。

## Rust 核心：vendoring 与许可

`sidecar/src/fingerprinting/` 来自 [`shazamio-core`](https://github.com/shazamio/shazamio-core)
**1.2.0 的 PyPI sdist**，MIT 许可，许可原文在 `sidecar/third_party/shazamio-core/LICENSE`。

改动只有三类，都与算法无关：

- 删掉 `pyo3` 包装层（`params.rs` / `response.rs` / `errors.rs`）——没有自己的逻辑。
- 删掉 `opus_decoder.rs` ——它捆绑 `libopus`，要编译 C，而构建机没有 C 工具链。
  代价是 Ogg / Matroska 里的 Opus 音频读不了，会归为 `unsupported`。
- 删掉各文件的 `#[cfg(test)]` 模块——它们依赖没有一起 vendor 进来的音频样本。

### ⚠️ 必须用 sdist，不能用 GitHub master

这两者**不是同一份实现**：

| | 解码 | 说明 |
|---|---|---|
| PyPI 1.2.0 sdist（本仓库用的） | `rodio` | 和已发布的 wheel 一致 |
| GitHub master | `symphonia` + `rubato` | 换过解码器 |

指纹的 DSP 部分（`do_fft` / `do_peak_spreading` / `do_peak_recognition`）两边**逐字节相同**，
但重采样出来的波形不同，**峰值集就不同**——实测同一段 12 秒音频，master 出 621 字节的
签名，wheel 出 1833 字节。

指纹只要错一位，Shazam 就永远匹配不上，而且它只会安静地返回空结果，不会报错。
所以这个仓库跟 sdist 走，并且有下面的比对工具把这件事钉住。

## 指纹正确性验证

```bash
pip install shazamio-core          # 只需要上游的 Rust 扩展，不装整个 shazamio
python sidecar/tools/compare_with_upstream.py <媒体文件> --seconds 12
```

它会用上游 wheel 和本仓库的 sidecar 各算一次指纹，逐字节比对，不一致时报告首个差异位置。

最近一次实测（全部一致，sha256 取前 16 位）：

| 文件 | 签名长度 | sha256[:16] | 结果 |
|---|---|---|---|
| 16 kHz 单声道 WAV | 189 字节 | `e8becd2ac5f5daca` | 一致 |
| 44.1 kHz 立体声 WAV | 1833 字节 | `73f72a572c729f01` | 一致 |
| MP3 | 1345 字节 | `36d1be4614fc0e7e` | 一致 |
| 多音 + 粉噪声 MP3 | 12881 字节 | `69f664cd0ed7dd24` | 一致 |
| M4A（走 ffmpeg 回退） | 1833 字节 | `74ab3a516d3e97e3` | 一致 |
| M4A（300 秒长文件） | 833 字节 | `ef222a7cd4f125e9` | 一致 |
| MP4（含视频轨） | 5517 字节 | `25bc809527e63eb4` | 一致 |

**改 `fingerprinting/` 里任何东西之后都要重跑这个比对。** 这是唯一能发现指纹被改坏的信号。

## 测试

```bash
go test ./...                                       # 全部
go test ./internal/job/ -run TestIntegration -v     # 只跑真 sidecar 的集成测试
go test ./internal/job/ -run TestTwoFiles -v        # 单跑一条
go test . -run TestFrontend -v                      # 只跑前端契约测试
VSR_TRAY_SMOKE=1 go test . -run TestTrayLive -v     # 托盘真机冒烟（会短暂占一个托盘位）
```

分四层：

**单元层** —— `internal/job/job_test.go` 用一个假的识别器跑完整的编排流程，覆盖：改名、
冲突加序号、预览模式、解码失败不改名、撤销（整批 / 单条 / 链式还原的顺序 / 记录不被删掉）、
跳过已处理、追加不清空、超长截断、失败重试。
这一层不联网、不起进程，改逻辑时跑它最快。

**配置层** —— `internal/config/config_test.go` 钉住「旧配置迁移不能吞掉用户改过的模板」。
只看结构体是不是空的会被 `Default()` 骗过去（它预填了字段），所以迁移判断改成看
JSON 里有没有 `templates` 这个键。

**前端契约层** —— `frontend_contract_test.go`。前端和后端之间是**字符串约定**，
编译器完全看不见，三处写错都只会在用户点下去时才炸：

| 约定 | 写错的症状 |
|---|---|
| `el('foo')` ↔ `id="foo"` | `Cannot read properties of null`，整个初始化中断 |
| `App.Bar()` ↔ 导出的 `(*App).Bar` | 「按钮点了没反应」，控制台一句 `not a function` |
| `RT.Baz()` ↔ Wails 运行时方法 | 同上；英式拼写 `Minimise` 写成 `Minimize` 时尤其隐蔽 |

这个测试直接读 `frontend/index.html`，用反射对 `*App` 的方法集，再检查无边框标题栏的
`no-drag` 声明还在。它自己验证过会红：改坏这三处中的任意一处，测试都会失败。

**集成层** —— `internal/job/integration_test.go` 拉**真的 `shazam-sidecar.exe`**，
覆盖单元层永远碰不到的那条缝：

```
Go → exec sidecar → 解析 stdout JSON → 错误分类 → 改名 / 不改名
```

- `TestIntegrationUndecodableFileNotRenamed` —— 拿一段文本冒充 mp3。完全离线、完全确定：
  sidecar 必然报解码错误，编排层必须归到 `failed` 并且**一个文件名都不动**。
- `TestIntegrationRealSidecarRoundTrip` —— 用 12 秒正弦波走完整流程，断言拿到
  `no_match` 后按 `0_` 前缀改名、写下撤销记录与状态文件、并且撤销能把名字还原。
  机器没网时会跳过改名断言（只确认进程边界是通的），免得离线的 CI 变红。

**托盘真机层** —— `tray_live_test.go`。单元测试只能证明结构体大小、菜单布局这些静态事实，
而托盘最容易踩的坑全是运行期才暴露、而且不报错的（见上文表格）。这个测试真的把图标挂到
任务栏上，验证 `Shell_NotifyIconW(NIM_ADD)` 成功、图标句柄非零、刷新与气泡不炸、
能干净拆掉。**需要交互式桌面会话，默认 `t.Skip`**，要显式 `VSR_TRAY_SMOKE=1` 才跑。

**界面几何层** —— `tools/shoot_ui.py`。排版这类改动只读源码是验证不了的，
所以它把前端喂给无头 Chrome，逐页截图并**在 DOM 上量几何量**：侧栏是否顶到窗口最上面、
标题栏是不是紧贴侧栏、顶部那条分割横线有没有回来、logo 够不够大、视图里有没有多套一层
`.page`、最小窗口下有没有控件被面板裁掉、禁用输入框的对比度够不够。
截图只说明「看着像」，这些断言才说明「量得对」。

它一共输出三种信号：

| 信号 | 从哪来 | 守什么 |
|---|---|---|
| `data-layout` | `LAYOUT_JS`，每个视图都跑 | 骨架几何量 + 对比度 |
| `data-modal-probe` | `MODAL_PROBE_JS`，只跑 `+modalprobe` | 弹窗的五个行为入口 |
| 截图 md5 | 每张图互相比对 | 视图/分组没切过去时几张图会一模一样 |

**截图必须先按死过渡。** `build_preview` 往 `<head>` 注入一段
`*{transition:none!important;animation:none!important;caret-color:transparent!important}`。
不注入的话截图会抖：`boot()` 把 `.nav` 的 active 从「任务」挪到「设置」，
而 `button` 上有 `transition: background .12s`，Chrome 的 `--screenshot` 是 load 完就拍，
经常拍在过渡中间——实测同一个 URL 出现两种 md5，差异集中在侧栏两个导航按钮上。
这不只是图难看：上面那条 md5 检查的**前提就是截图确定**，抖动的截图既会误报，
也会把真没切换的情况盖掉。注入点若失效会直接 `SystemExit`，不会静默退化。

顺带一个坑：设置页的默认分组（`SETTINGS_DEFAULT_GROUP`）截图**必然**和 `#settings` 一样，
那是 `boot()` 里 `showSettingsGroup('recognize')` 决定的，不是「分组没切过去」。
所以它走单独一条分支：断言两者**必须相同**，不同反而报错。

每一轮都做过变异验证（把 CSS 改回去看它变不变红）——不会红的断言等于没有断言。
这条规则在这里尤其重要，因为**量错对象是常态**：横向溢出这条断言先后量过
`document.scrollWidth` 和 `.view.scrollWidth`，两次都因为 `overflow:auto` / `overflow:hidden`
把超出的部分吞掉而永远不变红，第三次改成量「有没有元素伸出它所在的 `.panel`」才真的有效。

本机（150% 缩放）实测输出：

```
DPI 感知：Per-Monitor V2
托盘图标已就绪：24×24（系统要求 24×24）
```

`24×24` 而不是 `16×16`，就是「DPI 感知真的生效了」的硬证据。

几个测试在找不到 sidecar 时都会 `t.Skip`，不会把「没编 Rust」变成失败。
它们会自己往上找源码树里的 `sidecar/target/release/`，所以直接 `go test ./...` 就行。

## Shazam 接口要点

逆向出来的协议细节，改通信部分时要守住：

- **端点**：`https://amp.shazam.com/discovery/v5/{lang}/{country}/iphone/-/tag/{uuid1}/{uuid2}?…`
  - 必须是 `amp.` 或 `cdn.`。换 `www.` 会返回 1.7 MB 的单页应用外壳。
  - 设备段用 `iphone`：`web` 档不返回 Spotify / Deezer 链接，`android` 档把
    `apple_music_url` 返回成浏览器打不开的 `intent://`。
- **请求头**：`X-Shazam-Platform: IPHONE`、`X-Shazam-AppVersion: 14.1.0`。
- **User-Agent 必须是 App 串**（`Dalvik/…` 这类）。浏览器串会拿到 `200 text/html`
  的整个网页，较新的 Chrome 串直接 `405`。
- **取样窗口 12 秒。** Shazam 对 ≥15 秒的片段一律返回 `matches: []`。
- **请求体**：`{"timezone","signature":{"uri","samplems"},"timestamp","context":{},"geolocation":{}}`。
  `geolocation` 与 `context` 都是空对象——指纹模块里算出来的那份并不发出去。

## 网络与代理

识别是唯一联网的动作，而且只发生在 sidecar 里——所以代理设置也只需要在 sidecar 落地，
Go 侧纯粹是透传。链路是：

```
config.Proxy ──► recognizer.Options.Proxy ──► sidecar --proxy ──► reqwest
```

### 三种取值，缺一不可

| 界面里填 | 传给 sidecar | sidecar 行为 |
|---|---|---|
| 留空 | **不传** `--proxy` | 跟随 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` |
| `-`（`off` / `direct` 同义） | `--proxy -` | 强制直连，**环境变量也不看** |
| 其它 | `--proxy <地址>` | 用这个地址，必须带协议头 |

留空与 `-` 必须分开，因为 **reqwest 默认就会读环境变量**。如果只有「留空」一种表达方式，
那么设了 `HTTP_PROXY` 的用户既没法指定别的代理，也没法说「这次别用」——他绕不开那个变量。

这条语义整条链路上只由一个字符承载，所以三个地方各钉了一条测试：

- `TestProxyDirectButtonIsNotBlank`（前端）——「强制直连」按钮必须写入 `-`，**不是清空**。
  这是唯一反直觉的地方：想绕开系统代理的用户会本能地按「留空」，如果按钮真的清空输入框，
  它做的就和用户期望完全相反，而且不报任何错。
- `TestBuildArgsOmitsProxyWhenEmpty` / `TestBuildArgsPassesDirectFlag`（Go）——
  空值不传参，`-` 原样传。
- `proxy_absent_follows_environment` / `proxy_off_switches_to_direct`（Rust）——
  解析层不把两者混为一谈。

### `socks` feature 不是可选的

`Cargo.toml` 里 reqwest 开了 `socks`。不开的话 `socks5://` 会以一个语焉不详的
URL 解析错误失败——用户填的是对的地址，得到的却是一句看不出原因的报错。
实测对比（端口没人听时）：

```
开了 socks：SOCKS error: failed to create underlying connection
没开 socks：builder error（甚至到不了连接这一步）
```

### 报错要说人话

`reqwest::Error` 的 `Display` 只有一句 `error sending request for url (…)`，
真正有用的信息全在 `source()` 链上。`shazam.rs::describe_error` 取**最里层**那一条接在后面：

```
error sending request for url (https://…) → 由于目标计算机积极拒绝，无法连接。 (os error 10061)
```

只取最里层，不整条链拼出来：中间几层是 `client error (Connect)` 这类框架自己的话，
对用户没有意义，堆在一起反而把关键那句淹掉。这条改进同时惠及批量识别——
代理挂了的时候，用户看到的不再是一句无从下手的废话。

### `--check-network`：把「代理配错了」提前暴露

```
shazam-sidecar --check-network [--proxy …] [--timeout …]
```

走一次**真实**的 Shazam 请求（`shazam::probe`，同 host、同路径、不带签名），
任何 HTTP 应答都算通——实测被回 404 是正常的，它已经证明 DNS、代理、TLS、TCP 四段全通了。
返回 `status: network_ok` 时带上 `http_status` 和 `elapsed_ms`，失败时带 `error`。

为什么值得花一次真实请求：代理填错在批量跑之前**完全不可见**，用户要等到第一批文件
全部失败才发现，而那时错误信息里通常只有一句「网络错误」，分不清是代理没通、
地址写错，还是 Shazam 那边的问题。

界面上它挂在 设置 → 网络 的「测试连接」上，用的是**表单里当前的值**而不是已保存的值——
用户改完代理想先试一下再保存，这个顺序是合理的，不该强迫他先存一个可能是错的配置。
地址一改，上一次的结论就清成 `—`：留着一个绿色的「连接正常」配一个刚被改掉的地址，
比什么都不显示更容易误导。

### `--env` 里带上代理

`--env` 除了版本和 ffmpeg 路径，还会报 `proxy`（人话描述）和 `proxy_env`
（环境里实际存在的代理变量）。后者是为了回答「我明明没设代理啊」——
reqwest 会自己读这些变量，但用户在界面上一个都看不到。实测输出：

```json
"proxy": "跟随环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）",
"proxy_env": { "HTTP_PROXY": "http://127.0.0.1:51400", "HTTPS_PROXY": "…" }
```

## 无边框窗口、托盘与 DPI

这三件事绑在一起，因为它们的失败方式都是**静默的**——不报错、不崩溃，
只是「按钮点不动」「图标糊」「双击没反应」。所以每一处都留了客观信号。

### 无边框窗口（`Frameless: true`）

系统那圈框架整个去掉，标题栏由前端自绘（`.titlebar`），这样它才能和深色主题同色。
边缘缩放不用自己写：Wails 在 `Frameless && !DisableResize` 时会自动开。
`DisableFramelessWindowDecorations` 保持 `false`，否则 Win11 的圆角和投影也没了。

拖拽靠 CSS 自定义属性 `--wails-draggable: drag`，**但自定义属性会继承**：

```css
.titlebar { --wails-draggable: drag; }
.wbtns    { --wails-draggable: no-drag; }   /* 容器 */
.wbtn     { --wails-draggable: no-drag; }   /* 每个按钮，缺一不可 */
```

漏了 `no-drag` 的症状是「点最小化按钮变成拖窗口」，关闭按钮永远点不动——
而窗口是无边框的，这就等于关不掉了。`frontend_contract_test.go` 会检查这两条声明还在。

窗口状态没有事件可听：Wails v2 不推「最大化状态变了」。而 `WindowIsMaximised()`
返回的是 Promise，不是同步值。所以最大化/还原的图标只能轮询（当前 400 ms）。

### 托盘（`tray_windows.go`）

Wails v2 **没有托盘 API**，得自己写。手写而不是引 `getlantern/systray`，是因为
托盘本来就只要 user32/shell32 几个函数，自己写还能显式 `runtime.LockOSThread()`，
把「消息泵必须固定在创建窗口的那个 OS 线程」这条约束焊死。

Wails 的 Windows 前端是**逐调用** `LockOSThread`，不是整体占住主线程，
所以托盘可以在独立 goroutine 里跑自己的消息泵，两者互不干扰。

三个不报错的坑：

| 坑 | 症状 | 处理 |
|---|---|---|
| `NOTIFYICONDATAW.cbSize` 写错 | `Shell_NotifyIconW` 返回 0，图标不出现，无任何提示 | 用 `unsafe.Sizeof` 算（x64 下 976），别手写数字 |
| 窗口类漏了 `CS_DBLCLKS` | 只收到两次 `WM_LBUTTONUP`，**永远收不到** `WM_LBUTTONDBLCLK`，双击静默失效 | 窗口类固定带 `0x0008` |
| `TaskbarCreated` 没处理 | 资源管理器重启后图标消失，再也回不来 | `RegisterWindowMessageW` 拿动态消息号，收到就重新 `NIM_ADD` |

菜单里那条灰色状态行有个细节：Win32 的禁用项在键盘导航等路径下**仍会发命令**，
所以 `dispatch` 里对 `trayIDStatus` 显式 `return` 吃掉它。顺带一个好处——
ID 一旦和动作项撞号，这里会出现重复 `case`，编译器直接报错。

左键单击**故意什么都不做**（误触概率高），打开窗口要双击。

### DPI 感知（`dpi_windows.go`）

必须在**创建任何窗口之前**声明，所以它是 `main()` 的第一行。三级回退：

```
SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)   Win10 1703+
  → SetProcessDpiAwareness(PROCESS_PER_MONITOR_DPI_AWARE)   Win8.1+
    → SetProcessDPIAware()                                 Vista+
```

注意 `DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2` 是个**负的伪句柄** `-4`，
以 `uintptr` 传要写补码（`^uintptr(3)`），不能直接写 `-4`。

不声明的话，150% 缩放的屏幕上：

- 调 `GetSystemMetrics(SM_CXSMICON)` 只会拿到虚拟化后的 **16**，而任务栏实际要 **24**，
  交出 16 让外壳放大 → 托盘图标是糊的；
- 整个窗口和 WebView 也会被系统拉伸一遍，文字发虚。

`applyIcon` 会把**实际用了哪一档**写进日志：

```
托盘图标已就绪：24×24（系统要求 24×24）
```

这行是唯一能证明 DPI 生效的客观信号。看到 `16×16（系统要求 16×16）` 就是没生效。

### 图标

`assets/icon.ico` 由 `tools/make_icon.py` 生成，多尺寸 16/20/24/32/40/48/64/128/256。
脚本不依赖 PIL——用距离场画圆角矩形、旋转椭圆和贝塞尔旗子，4 倍超采样，
ICO 直接按 32bpp DIB + AND 掩码编码，PNG 用标准库 `zlib` 写。

选帧策略：**相等 > 更大的里最小的 > 最大的**。宁可缩小也不放大，
放大一定糊。`pickICO` 有这个逻辑的单元测试——它曾经因为 `bestScore` 初值写成 `-1`
（而所有得分都是负数）退化成「永远返回第一帧」，在 150% 屏上就是糊的。

ICO 里每个条目的数据是**裸 DIB**（`BITMAPINFOHEADER` + 自下而上的 BGRA + AND 掩码），
可以喂给 `CreateIconFromResourceEx`。256 在目录项里编码成 0。

### 只允许一个实例（`internal/single`）

两个实例不只是「多一个托盘图标」：它们各自读一份配置、各自跑一批改名，
而改名是**写磁盘**的；配置是读—改—写，后写的那份会把先写的覆盖掉。
这些都不会报错，只会安静地把状态弄乱。

用**命名内核对象**（互斥体 + 事件），不用锁文件：进程无论怎么退出
（正常退出、崩溃、被任务管理器结束），内核都会替它释放句柄，
不会留下一个「上次没清干净」的锁把用户挡在门外。

- `Local\` 前缀 = 每个登录会话一个命名空间。同一个用户开第二个会被拦下；
  换一个 Windows 用户登录还能再开一个 —— 那本来就是两份独立的配置和队列。
  （`Global\` 需要 `SeCreateGlobalPrivilege`，普通权限的进程建不出来。）
- **`windows.CreateMutex` 把「名字已存在」当错误返回**（`ERROR_ALREADY_EXISTS`），
  所以判定要用 `errors.Is(err, windows.ERROR_ALREADY_EXISTS)`；
  而且这时返回的句柄**仍然有效**，得自己 `CloseHandle`，否则每次启动漏一个句柄。
- **抢名额必须在建窗口、读配置之前**，所以它在 `main()` 里排在
  `enableDPIAwareness()` 前面（DPI 那条又是「必须在建窗口之前」，两者不冲突）。

第二个实例不是安静地退出，而是**通知已有实例把窗口叫出来**：用户双击图标
多半是因为「刚才那个窗口找不到了」（收在托盘里、或者被别的窗口盖住）。

走命名**事件**而不是 `FindWindow` + `ShowWindow`：窗口可能收在托盘里，
而 Wails 自己记着「窗口现在可见吗」。绕过它直接 `ShowWindow` 会让它的记账
和实际状态对不上（托盘菜单里那两项就错了）。发个通知让它自己调 `WindowShow`，
走的才是同一条路径。唤醒方用 `OpenEvent(EVENT_MODIFY_STATE)` 而不是
`CreateEvent` —— 后者会在对方刚好已经退出时顺手留下一个陈旧的事件对象。

事件必须是**自动复位**的（`CreateEvent` 的 manualReset 传 0）：手动复位的话
信号不会被清掉，等待循环里 `show()` 会被反复调用，连点两次图标就变成叫个不停。
`TestWatch_OneWakeCallsShowOnce` 钉住这条（变异：把那个 0 改成 1 → 变红）。

`Acquire` 出错时返回「放行」（`nil, true`）：宁可偶尔漏掉一次拦截，
也不要把用户锁在门外 —— 单实例防的是「手滑双击两次」，不是安全边界。

测试用 `acquireNamed` 传一套自己的名字（带 PID + 用例名）。直接用包里那两个
常量的话，用户恰好正开着程序时 `Acquire` 会返回「已经有一个实例」，
测试就成了随机失败 —— 那种测试没人敢信、也没人敢跑，等于没写。

### 启动时不该有控制台黑框（`internal/winproc`）

用户报的「打开软件会有个命令窗口闪过去」。这个程序自己是 GUI 程序
（`-H windowsgui`，没有控制台），而 **GUI 程序拉起控制台子进程时，
Windows 默认给它新建一个控制台窗口** —— 那就是闪过去的那一帧。

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    HideWindow:    true,
    CreationFlags: windows.CREATE_NO_WINDOW,
}
```

两个标志缺一不可，而且**只设 `HideWindow` 挡不住**：它的意思是
「建出来，但别显示」，窗口确实建了，中间那一帧仍然会闪。
`CREATE_NO_WINDOW`（`0x08000000`）才是「压根不要建」。

所以统一走 `internal/winproc.NoConsole(cmd)`，**必须在 `Start` / `Output` / `Run`
之前调用**（之后再设 `SysProcAttr` 不起作用）。当前挂在 6 处：识别器的
`Version` / `Env` / `CheckNetwork` / `Recognize`，以及 app 里两处 `cmd /c start`
（打开日志、打开链接）。`explorer` 是 GUI 程序，不用管。

`TestNoConsole_SuppressesTheConsoleWindow` 盯的是 `CreationFlags` 而不是
`HideWindow` —— 只验后者的话，把它删掉、退回会闪的写法，测试照样通过。
（变异：把标志换成 `CREATE_NEW_CONSOLE` → 变红。）

## 前端

`frontend/index.html` 是单个文件，CSS 与 JS 都内联，没有构建步骤。
`main.go` 用 `//go:embed all:frontend` 编进 exe，再用 `fs.Sub` 切到 `frontend` 这一层
（Wails 的 assetserver 把 FS 根当站点根，不切的话 `index.html` 会挂在 `/frontend/` 下）。

### 界面骨架：最多一层框

骨架就两层，没有 `.app` 这类中间包装：

```
.shell
├── .sidebar        一整条到底：顶部 .logo（48px），下面 .nav
└── .main           纵向 flex
    ├── .titlebar   和侧栏同一行、也在顶部；左端放状态，右端放窗口按钮
    └── .view ×4    直接是 .main 的孩子
```

三条约束，改布局时别破坏：

- **侧栏顶到窗口最上面**（`top = 0`、高度 = 视口高度），标题栏和它同排。
- **顶部没有分割横线**。`.titlebar` 上曾经有一条 `border-bottom`，那是「一层套一层」观感的
  主要来源——窗口边、标题栏线、面板边三层叠在一起。现在一条不留。
- **视图里不再套 `.page`**。单列长页的视图（模板/记录/设置）自己就是滚动容器，
  用 `.view.scroll`；任务页是「工具条 + 表格 + 底部面板」三段式，用纵向 flex。
  少一层包装就少一层内边距，也就少一层看起来像「框」的东西。

这些不是审美偏好，是量得出来的几何量，所以由 `tools/shoot_ui.py` 逐条断言
（`check_layout`）：侧栏 top、侧栏高、标题栏 top/left、`border-bottom-width`、
logo 宽度、`.page` 层数、最小窗口下有没有控件被裁掉、禁用输入框的对比度。
改坏了截图会全部标红，不会静静地退回去。

### 工作区：左栏 + 右区 + 底部动作条

模板页和设置页是同一个版式（`.view.ws`），因为它们的形状是一样的：
左边一堆可选项，右边是当前选中项的全部字段，改动攒着，最后一次性保存。

```
.view.ws                横向 flex，自己不带内边距
├── .ws-nav    236px    左栏：搜索框 + 列表 + 新建按钮（.ws-nav-top / .ws-nav-list）
└── .ws-main   flex:1
    ├── .ws-scroll      滚动区，里面是若干 <section class="sect">
    └── .ws-foot        固定底部动作条：左边状态文字，右边按钮
```

分组用 `.sect`，**不加卡片边框**：一行「图标 + 标题 + 右侧说明」（`.sect-head`），
下面直接跟字段网格，分组之间只用一条很淡的 `border-top` 分开。
再给每组套一层框，就又是「一层套一层」了——那正是上一节在拆的东西。

`.ws-foot` 左边那行状态文字（`.foot-state`）不是装饰，是**脏标记**：
`ok` 绿色「已保存」，`warn` 黄色「有未保存的改动」。两个页面各有一份比较逻辑：

- 模板页比 `name` 和 `body`；
- 设置页比 `SETTINGS_KEYS` 里列出的字段——**只比这份表单管得着的**。
  模板库、当前模板这些不在这页改，把它们一起比进去，底栏会永远显示「有未保存的改动」。

`markSettingsDirty` 挂在容器的 `input`/`change` 上（capture 阶段），
省得给每个控件单独挂一遍；模板页那几个在表单外的输入框单独挂。

### 确认弹窗：自绘，不用原生

WebView 原生的 `confirm()` 是系统样式的浅色框，和深色主题放在一起非常突兀。
所有确认都走 `askConfirm({title, body, ok, danger})`，返回 `Promise<boolean>`。

比原生多出三个入口，每一个接错都是「点取消却执行了」级别的事故，所以逐条钉死
（`tools/shoot_ui.py` 的 `MODAL_PROBE_JS` + `check_modal`）：

| 入口 | 约定 |
|---|---|
| 点「取消」/ 点遮罩 / 按 Esc | 一律 `false` |
| 点「确定」/ 按 Enter | 一律 `true` |
| 点卡片**内部** | 不关（只认 `event.target === mask`） |
| 打开时焦点 | 落在「取消」上 —— 危险操作不该一个回车就执行 |
| 关闭后焦点 | 还给触发它的那个按钮 |
| `danger: true` | 图标和确认按钮都变红，按钮文案换成动作名（如「清空」） |

`frontend_contract_test.go` 里还有一条 `TestFrontendHasNoNativeDialogs`：
它会剥掉 `//` 注释后拒绝 `confirm(` / `alert(` / `prompt(` 重新出现在源码里。

### 命名模板：槽位模型，不是字符串模板

模板渲染时，没识别到的槽位会留空，`{artist}-{title:en}-{title:zh}` 在中文歌名缺失时
会渲染成 `BTS-Spring Day-`——尾巴上多一个杠。

根因不是「少写了一次 trim」，而是**字符串模板这个模型本身就不成立**：一条
`"艺人-英文名-中文名"` 的字符串里，分隔符和槽位是同一种东西（都是字符），
渲染器只能靠猜「这个 `-` 是用户想要的分隔符，还是上一个槽位留下的残渣」。
旧实现正是这么猜的——折叠连续分隔符用一份清单，收尾裁剪却写死成
`strings.Trim(out, " -_.")`。两份清单必然对不上：后者不认识 `/`、`|`、`+`、`、`、
全角 `－`，而且单字符分隔符不算「连续」，折叠那步也不会碰它。

现在换成**槽位模型**（`internal/naming/template.go`）：

```go
type Slot struct {
    Var       string  // title / artist / album / year / original / literal
    Script    string  // 语种，只有 title 用得上
    Text      string  // Var == literal 时的字面量
    Prefix    string  // 只在有值时一起写出去
    Suffix    string
    Separator *string // nil = 继承全局；&"" = 这一格前面不要分隔符
}

type Template struct {
    Separator string // 全局分隔符
    Slots     []Slot
}
```

`Render` **只为非空槽位写分隔符**，空槽位从结构上就消失，没有任何东西需要事后擦除。

三处细节是承重的，改之前先看这里：

- **`Separator` 必须是指针。** `nil`（继承全局）和 `&""`（这一格不要分隔符）是两件
  不同的事，塌成一个值的话 `{title} ({year})` 会变成 `Song - (2017)`——
  year 那一格显式声明了「括号紧跟标题，中间不要横杠」，丢掉这个区别就多出一个横杠。
- **前缀/后缀属于「后面那个」槽位。** `(` 是 year 槽位的前缀，不是独立字面量。
  归错的话年份缺失时会留下一个孤零零的 `(`。
- **`Equal` 必须手写，不能用 `reflect.DeepEqual`。** `Separator` 是指针，
  两份结构完全相同的模板如果指针地址不同，`DeepEqual` 会判不等——
  而它被用来判断「这是不是内置模板」，判错的后果是每次启动都重新迁移一遍用户模板。

**旧配置靠 `UnmarshalJSON` 迁移，不是靠版本号。** JSON 的键名刻意没改
（`name_template`、`templates`），`naming.Template.UnmarshalJSON` 同时接受
旧的 `"字符串"` 和新的 `{"separator":…,"slots":[…]}`，`config.Template.UnmarshalJSON`
同时接受 `body` 键和 `separator`+`slots`。这样旧文件在 `Load` 时自动升级，
不需要用户做任何事。中间还短暂存在过 `{"name":…,"body":"…"}` 这个形态
（有 `templates` 键、但模板还是字符串），它才是最容易被漏掉的一版——
`TestLoadMigratesLegacyTemplateBody` 专门钉住它。

**前端不再持有任何目录。** 变量、语种、分隔符候选全部来自
`App.TemplateEditorOptions()`，预览由 `App.PreviewTemplate()` 在 Go 里算。
老实现是前后端各写一份清单再用 `TestSeparatorListsMatchBetweenGoAndJS` 钉住一致——
现在副本没了，那个测试也删了，换成 `TestFrontendDoesNotHardcodeTemplateOptions`
守「别再写回去」。这条重要是因为变量名是纯字符串约定：`{title:zh}` 在前端写成
`{title:cn}` 不会有任何编译错误，只在用户保存之后才悄悄出错。

**编辑器形状：一格一行 + 加号。** 编辑区从上到下就是文件名里的先后顺序，
每一格一行：左边是下拉框，右边是这一格的补充设置，末尾一个「＋ 添加」加一格。

整页从上到下是「分隔符 → 命名 → 未识别命名 → 命名预览」：全局分隔符在最上面
（它管着所有没单独设过的格），然后是命名，再是识别不到时怎么办，最后是预览。
**预览放最下面**是有意的 —— 它是「改完看一眼」的地方，摆在中间会把
「改哪里」和「看结果」两件事挤在一起。四个决定是承重的：

- **下拉目录被拍平成一个 `choices` 数组**（`naming.Choice{Var, Script, Label, Group}`），
  不再是「变量清单 + 语种清单」两张表。旧的两张表只能表达
  「先选变量、再选语种」两级联动，而这里每一格只需要一个扁平的选择；
  `Group` 字段负责给 `<optgroup>` 分组（歌名和艺人各带六个语种变体，
  不分组的会铺成一条十七项的平表）。**顺序也是契约**——
  `TestStubTemplateOptionsMatchGo` 连顺序一起比对。
- **`ChoiceLabel` 要有兜底。** 配置里可能存着目录中没有的 `var:script`
  （手改过配置、跨版本升级）。不补这一项的话 `select.value` 赋不上，
  界面会显示成第一项，而存进去的是另一项——用户看到的和实际存的不一样，
  这是最难发现的一类不一致。
- **加号按钮写在 HTML 里，不能被重画。** `renderSlotList` 只往它**前面**插 chip
  （`box.insertBefore(chip, add)`）。把它一起重建的话，挂在它身上的 `onclick`
  就没了——重画类代码最常见的坑。
- **每一格的「分隔符 / 前缀 / 后缀」就长在它自己那一行里，而且是常驻的。**
  原先这三项挂在 chip 行**下面**一块独立的「第 N 段」面板里，两处别扭：
  一是得先点中某一格才知道面板在改谁，chip 一多就得来回数；二是「点中」这个动作
  其实点在它里面的 `<select>` 上，**一定会弹出系统下拉菜单**，还得先按 Esc 关掉。
  摊进 chip 之后两个别扭一起没了。
  - 中间还有过一版「点 `⋯` 就地展开」，后来连 `⋯` 一起删了。理由是**折叠本身没换来什么**：
    补充设置一共三个输入框，收起来省不下多少高度，却多出「现在开的是哪一格」
    这个状态——而它得跟着增删换位一起重算（`openSlot` 那套下标维护：删掉的正是
    展开那格要置 `-1`、删的是它前面那格要减一、拖走它要跟到新下标），三种情况
    缺一种就会出现「我动的是这一段，张开的却是隔壁」。现在一格一行、竖着排，
    没有「哪一格开着」这个状态，那三条断言（`openFollowsMove` /
    `openFollowsDelete` / `openAfterRemovingOpen`）连同 `openSlot` 一起删掉了。
  - 一竖排之后 `check_slots` 改量两件事：**每一格占一行**（同一行里没有别的 chip）、
    **补充设置始终可见**（不用先点谁）。行级的焦点框用 `.slotchip:focus-within`，
    不给每个输入框各画一圈 —— 那样一格里有几个输入框就会看到几个框。

排序用 HTML5 拖放，但**换位逻辑抽成 `moveSlotTo(from, to)` 单独调用**：
无头 Chrome 里没法用脚本触发 `dragstart`，而真正要守的是
「换位算得对」，不是「dragstart 绑上了没有」。

`tools/shoot_ui.py` 的 `check_slots` 钉住上面这些形状。写断言时踩过一个坑：
`swapped` 一开始写成 `keyOf(0) === before[1] && keyOf(1) === before[0]`，
把 chip 退化成纯文本做变异测试时两个 key 都是空串，`'' === ''` 恒真，
**断言在最该报警的时候「通过」了**。现在要求两个 key 非空且互不相同。
同一轮变异测试还确认了 `addIsLast`（chip 追加到加号之后 → 变红）、
`swapped`（换位下标算错 → 变红）、`chipsWithSelect`（chip 退化成纯文本 → 变红）
三条都能真的变红。只想跑这一个探针时用 `python tools/probe_slots.py`，
不必跑完整套件。

**模板页不出现滚动条：压留白 + 只让「命名预览」这一段可压缩。**

用户报过「模板界面右边不要滚动条」。量过（1258×726，就是 `WINDOW_SIZE`）：
`.ws-scroll` 的 clientH 630 / scrollH 772 —— **默认尺寸下就超了 142px**。
所以这不是「偶尔要滚」，是「一直都多出来一截」。

两手一起上：

1. **压纯留白**（都挂在 `#tplScroll` 上）：容器 padding 18/22 → 12/12、
   段间距 `20+20` → `12+12`、段标题下边距 13 → 9、段行 padding 10 → 7、
   hint 上边距 10 → 6。只压留白，不压字号、不压控件高度，信息量一点不减。
2. **命名预览那一段自己吸收**：它是最后一段，`flex: 0 1 auto` + `min-height: 96px`，
   窗口矮就它先缩，缩到底之后自己内部滚（`overflow: auto`）。其余三段是 `flex: none`。

**最省的一刀在预览自己的行里**：原来是「值一行 + 灰色说明一行」，一组样例 37px、
四组连间隔要 188px。改成「标签 | 值 | 说明」一行三列之后一组 19px，
四组只要 112px —— 这一刀省下的 76px 比上面所有留白加起来还多。
（值那一格**不能** `flex: 1`：撑满会把说明顶到最右边，中间拖出一大片空白。）

滚动条本身也一并藏掉（`scrollbar-width: none` + `::-webkit-scrollbar`）：
窗口被拖到比各段 min-height 之和还矮时内容仍要能滚，但那时候显示一根条
只会让人以为界面坏了。

**这一整套只作用于 `#tplScroll`**：设置页共用 `.ws-scroll` / `.sect` /
`.sect-head`，而那边内容多、真的需要滚动条。模板页那个 `.ws-scroll` 因此
单独加了 `id="tplScroll"`。

`check_slots` 里几条断言钉住它：`scrollOver == 0`（scrollHeight - clientHeight；
**截图那一路带着 `--hide-scrollbars`，永远看不到滚动条**，「看起来没有」不算数）、
`previewOver == 0`（预览被压到裁掉样例，就说明别处留白还没压干净）、
`previewShrink` 非 0（写死成 `flex: none` 的话窗口一矮整页又开始滚）。
变异测试：段间距放大到 40px → 报「还有 17 px 装不下」。

**「这一段被选中」由整行一起表达，内层控件一律不画框。**

用户报的「命名那边的块选中时样式有问题」。量出来是两个原因叠在一起：

- 行框太弱：只有 `border-color: var(--accent)` 一条，`rgb(38,43,53)` →
  `rgb(59,130,246)`，1px 的蓝线在 `#0f1115` 底上几乎看不出变化；
- 同一时刻 `chip-fields` 里的小输入框自己也会变蓝 —— 行框看不见，
  剩下的就只是「一个和行框错位的小蓝框」，两个框谁也不像选中。

重做之后选中信息**全部由整行承担**，四层一起上：左边一条 3px 强调色竖条、
外框换色、底色提亮一档并偏蓝（`#1c2029` → `#1e2637`）、外加 3px 半透明外发光。
内层控件不画框，只在**被聚焦的那一个**上把底色再提亮一档（`#26304a`）。

那条 3px 竖条用的是 `border-left: 3px solid transparent`，**不是绝对定位的
`::before`**：绝对定位的后代画在非定位的流内子元素**之上**，会正好压住下拉框的
左圆角。写成透明边框则宽度一直占着、选中时只换颜色，既不横向位移也不叠层。

顺带说清「可点」这件事：`.slotchip > select` 和 `.slotchip > input` 现在各有一块
比行底色深的凹槽（`#171b25`）。原来两个都是完全透明的裸文字，用户看不出能点、
看不出那 88px 是能打字的区域 —— 这是「选中时样式有问题」的另一半。

**修这条时挖出一个真 bug**：`.slotchip select:focus { border: none }` 写的是
**后代**选择器，把 `chip-fields` 里那几个小控件也一起命中了。`border: none`
会把 `border-style` 一并清成 `none`，而下面 `.chip-fields ... :focus` 那条只改
`border-color`、改不回样式 —— 于是**一点进「分隔符」那个小框，它的边框直接消失**，
看着像框缺了一块。现在这条收成了 `>`（只作用于行里的直接子控件），
小框聚焦时保住边框、只是变蓝。

`check_slots` 的 `fieldKeepsBorder` 钉住这个 bug（变异：把 `>` 改回后代选择器 →
立刻变红，报出来的就是 `style=none width=0px`）；`chipLeftBar == "3px"` 和
`chipBarDim != chipBarLit && chipBgDim != chipBgLit` 钉住「不止外框变了」
（变异：把 `focus-within` 退回只换边框颜色 → 变红）。
另外 `listBadges / listSaysCurrent / listRow1` 三条钉住「模板列表不再标「当前」」
（变异：把那个小标加回去 → 报「.badge 5 个 / 写着「当前」的文字 5 处」）。

**模板的「重命名 / 复制 / 删除」挂在左栏的右键菜单上，页面上没有别的入口。**
原先「名称」是一个输入框、底栏还有「设为当前」和「恢复内置模板」两颗按钮，
都撤掉了：名称和模板内容一起存，「保存」一颗按钮就够；用哪一份模板是**每个任务
自己选的**（任务页工具条上那个下拉框），模板页不该再有一个全局的「当前模板」。
撤掉之后改名这件事就只剩右键这一条路 —— 而它断掉的样子是「右键没反应」，
不抛错、不写日志，所以 `check_tpl_menu` 刻意走完整条链：右键 → 菜单 →
弹窗填字 → 后端 → 列表重画，只验「菜单弹出来了」是不够的，风险在弹窗和后端那两步。

几条容易漏的：

- **右键要顺手把那一份选中。** 菜单里三项都是对着它做的，而右键落在没选中的
  那一份上时，用户多半以为已经选上了。
- **重命名必须就地改。** 写成「存一份新的再删旧的」的话，条数和位置都会变，
  而且 `editing` 会指向一个不存在的名字 —— 底栏一直写「有未保存的改动」，
  再按「保存」就把旧名字重新创建出来。探针里 `renamedInPlace` 和
  `editingFollowedRename` 两条分别钉住这两半（变异：把 `editing = next` 那行
  改掉 → 报「改名之后编辑器没跟着换过去」）。
- **只剩一份时「删除」置灰。** 后端也会拒绝，但「按下去才报错」不如一开始就是灰的。
- **弹窗要能收能弹。** `askInput` 走的是 `#modalField`（单独一行输入框），
  取消和空串都返回 `null` —— 空串当成「没改名」而不是「改成空名字」。
- **自绘菜单，不用 WebView 原生的。** 原生那份是浅色系统样式，从这套深色界面里
  弹出来像另一个程序；而且在 Wails 的无边框窗口里，它按系统位置弹，
  贴边时会被窗口边界裁掉。自绘的那份要自己夹回视口内（`menuInsideViewport` 钉住）：
  右键点在最下面一行时，照原样往下展开会有一半在窗口外，而它没有滚动条。
- **收起要挂全**：点别处（`mousedown` 而不是 `click`，否则按下到抬起之间菜单还挂着）、
  Esc、滚动、改窗口大小。少一个，菜单就会一直挡着底下的按钮。

**工具条和文件列表之间不能多出一条线。** 用户报过「上面板像被切成了两个」。
根因是进度条的 track：`height:2px; background:var(--panel-2); margin:8px 0 10px`——
进度为 0 时它仍然画一条横贯全宽的浅色线，悬在两者中间，什么都没表示。
改法是 track 透明 + `margin: 0 0 -1px` + `z-index: 2`，
让填充线**压住**表格自己那条 1px 上边框（否则跑起来会看到 2px 蓝 + 1px 灰叠在一起）。
另外**跑完必须把进度复位**，否则它会停在 100% 留下一条常驻蓝线——
`renderProgress` 现在按 `running` 把关。`check_layout` 的 `progressBand`
量三件事：track 的 alpha、和列表上沿的间距、静止时的填充宽度。

**空队列是另一条渲染分支，`refreshEmptyState()` 必须在 boot 里也调一次。**

表格和空状态是互斥的两块，各自带一条 1px 边框。切这两块的
`refreshEmptyState()` 原本只在 `rebuild()` / `repaintAll()` 里调，
而**空队列时 boot() 根本不会走 rebuild**（`if (list && list.length) rebuild(list)`）——
于是两块同时显示，用户看到的就是「中间多了一条分割线，面板像被切成两半」。
和有数据时那条进度条 track 是**两个不同的原因**，症状却几乎一样。

`tools/shoot_ui.py` 为此单独搭了一份空 items 的页面（`empty.html`）并截图 ——
这个 bug 在有数据的截图里根本不会出现，不专门测就永远测不到。

**原生控件在深色页面上是白的 —— 但这是两套互相独立的机制，别混。**

共同点是：`<select>` 弹出来的选项列表、`<input type=number>` 的增减按钮、
滚动条、checkbox 的勾选框，**全都是浏览器自己绘制的**，CSS 里给控件写
`background` / `color` 管不到它们。要修得按各自的规则来。

**机制一：`color-scheme`。** 一行 `color-scheme: dark` 写在 `:root` 上，
能让滚动条、checkbox 勾选框、`input[type=number]` 的原生三角变成深色长相。

**机制二：`<select>` 自己的 `background-color`。** 这条和 `color-scheme`
**完全无关**，`:root` 上写了 `dark` 也救不了它 —— 曾经误以为一行 `color-scheme`
就能包住四样东西，漏掉的就是这一个。

Chromium 画选项列表时，列表底色是从 select 自己的 `background-color` 推的。
写成 `transparent` 就推不出颜色，于是**退回系统浅色默认值：白底**；
而选项文字继承 select 的 `color`（浅色），结果是白底白字，几乎看不见。

踩点很隐蔽：`.slotchip select` 原本为了「让整个 chip 看起来是一个整体」写了
`background: transparent`，于是**模板页那三个槽位下拉框的列表是白的，而其它
select 走全局规则本来就是实心的 —— 只有模板页出问题**。

修法换过两次，最后落在**一块写死的实色** `#171b25` 上：

1. 先写成 `background-color: inherit`。它确实解决了白底白字（chip 的底色是
   `--panel-2`，跟着变深），但它的前提是「chip 自己也有底色」——
2. 后来 chip 有了选中态（`focus-within` 时底色变 `#1e2637`），`inherit` 就跟着
   一起浮起来，「下拉是一块凹槽」的意思没了。所以现在写死。

写死之后有个新约束：**这个颜色必须和行底色有可见的深浅差**，否则下拉又变回
「看不出能点」。当前是行 `#1c2029` / 选中行 `#1e2637`、下拉 `#171b25`。
（`>` 只圈直接子元素，`.chip-fields` 里那几个小控件有自己的框和底色，不受影响。）

`check_selects` 钉住它的**因**：任何一个 `<select>` 都不许是透明的。
弹出列表是独立绘制的一层，`--screenshot` 和 `--dump-dom` 都抓不到它本身，
所以只能断言因。变异测试：把它改回 `transparent`，立刻变红并点名是哪几个。

数字输入框的增减按钮另外**换成了自己画的** `−` / `+`（`enhanceNumberInputs`）：
`color-scheme` 能让原生三角变深，但它仍然是系统长相，和整套界面不搭。
两个必须注意的点：

- **包装是套在原有 input 外面，input 的 id 和取值逻辑一点都不动**，
  所以满页面的 `el('cfgSegment').value` 一行都不用改。
- **改完值必须手动派发 `input` 事件。** 直接写 `.value` 不触发任何监听器，
  而这些输入框是靠 `oninput` 去 `markSettingsDirty()` 的 ——
  不派发就会出现「值改了但底栏还写着已保存」，保存下去的还是旧值。
  `check_numbers` 专门断言这一条（变异测试确认过：去掉派发立刻变红）。
- `fillSettings` 是直接赋值的，所以它末尾要补一次 `paintAllNumberSteps()`，
  否则增减按钮的置灰状态会按空值算错。

**额外语种是派生值，不是配置项。** 设置页里原来有个手填的「多语种标题」文本框，
要用户自己记住「模板里写了 `{title:ko}`，就得把 `ko-KR/KR` 填进去」。
这两件事本来就该绑在一起，拆开设只会漏填——而漏填是**静默失效**：
那一格永远取不到值，在预览里安静地消失，看起来就像「这首歌本来就没有韩文名」。

现在由 `naming.ExtraLocales(template)` 从模板里的 `script` 推导，
`Config.ExtraLocales()` 拿当前模板算。三条细节：

- **顺序写死成 英→中→日→韩。** 不固定的话 map 的遍历顺序会让
  「命令预览」每次显示的命令行都不一样，看着像有 bug。
- **`ScriptOther` 不在映射表里。** 它是 `Classify` 认不出文字种类时的兜底分类，
  不是一种能主动去查的语言，没有对应的 storefront，拿它去查只会白跑一次请求。
  下拉框里也**不再提供**「其他语种」这一项，理由同上。
- **旧配置的 `title_locales` 键直接忽略。** 已经删掉的字段不需要写迁移代码——
  `json.Unmarshal` 对未知字段不报错，下次保存它自然就消失了。
  `TestLegacyTitleLocalesKeyIsIgnored` 钉住这条：升级不能让人打不开程序。

**未识别命名是「前缀 + 原文件名 + 后缀」，用 `naming.Unmatched` 承载。**

用一个结构体而不是两个相邻的 `string` 参数，是因为 prefix 和 suffix 类型一样、
位置相邻，写成位置参数的话传反了编译器一声不吭——而后果是
`0_song.mp4` 悄悄变成 `song0_.mp4`，得跑起来才看得见。

`Unmatched.Marked(stem)` 判断「是不是已经带过标记了」（避免套娃改名）。
**不能直接写 `strings.HasPrefix(name, prefix)`**：空串当标记时它**恒为真**，
于是把前缀清空、只留后缀的用户会发现未识别的一个都不改名了——
界面上看起来一切正常，只是那些文件纹丝不动。`Marked` 要求标记非空才算数。

**前后缀都留空 = 「未识别的别动」，这时必须先比一次同名再决定改不改。**
算出来的新名字会和原名一模一样，而 `Marked` 拦不住它（没有标记，恒为 false）。
真去改的后果不是「白跑一次 `os.Rename(p, p)`」那么轻：`job.rename` 会先向
`media.UniquePath` 要一个不冲突的路径，而它只看目标是否存在——此刻
**源文件自己正占着那个名字**，于是判定冲突并返回 `mystery (1).mp4`。
用户看到的是一个什么都没认出来的文件被莫名加上 `" (1)"`，且像是正常改名。
`finishUnmatched` 里因此有一道 `media.SamePath` 判断，和 `finishMatched` 里那道同源。
`TestUnmatchedWithNoMarkerLeavesFileAlone` 钉住它（关掉那道判断就会红，
报错是「不该动文件，实际 `[mystery (1).mp4]`」）。

两个静态预览桩（`tools/shoot_ui.py` / `tools/preview/stub.js`）跑在浏览器里，
够不着 Go，所以**不得不**复制一份目录。既然躲不掉，就让两份桩共用同一个
`tools/preview/template-options.json`，由 `TestStubTemplateOptionsMatchGo`
按解析后的 JSON 逐项比对——不是比对文本，避免被格式差异绊倒。

### 改名记录与还原

`undo.jsonl` 是**纯追加**的事件流，一行一件事，两种形态：

```
{"from":"D:\\Music\\a.mp4","to":"D:\\Music\\晴天.mp4","ts":1770000000000}
{"restore":"D:\\Music\\晴天.mp4","ts":1770000060000}
```

**为什么是「追加一条还原记录」而不是「删掉 / 回写那一行」。**
这个文件是纯追加的，中途崩溃最多丢掉最后一行；改成读—改—写就可能在写一半时
把整份记录弄坏 —— 而用户最不能接受的就是「改了一堆文件，记录还没了」。
代价是读的时候要重放一遍：`loadUndo` 按顺序扫，遇到还原记录就把它盖到
**最近一条还没还原过的同 `to` 记录**上（同一个 `to` 可能被改过不止一次）。

**撤销之后记录不删。** 老实现撤完就 `os.Remove(undo.jsonl)`，注释写的是
「免得重复撤销」。加了单条还原之后这个做法就不成立了：删掉等于把「原文件名」
一起删掉，而用户回头就是来查这个的。防重复撤销改用 `Restored` 标记 ——
已经还原过的条目在界面上没有按钮，`UndoOne` 也会拒绝。

**`Undo` 必须倒着走。** 同一批里可能有链式改名（A→B→C）。正序还原的话，
第一步去还原 A→B 时 B 还被 C 占着，会被判成「原名已被占用」而跳过 ——
用户看到的是「撤销完了，但文件没动」。
`TestUndoReplaysChainedRenamesInReverse` 把顺序钉住（改成正序就红，
报的是「文件已不在: b.mp4」）。

**`UndoOne` 用现名（完整路径）当钥匙，不用列表下标。** 记录页的列表会随时间
重排，下标是会漂的；路径不会。同一个 `to` 出现多次时还原最近的那一条。

**`UndoMany`（批量还原）为什么收一个数组、而不是让前端循环调 `UndoOne`。**
两个原因，第二个是硬的：一是每调一次都要把整份 `undo.jsonl` 读出来重放一遍，
勾几十行就是几十遍；二是**链式改名要求倒着还原**，拆成多次调用就没法保证这个顺序
（前端是按列表顺序勾的，而列表是倒序的，看着像对，其实是巧合）。
`UndoMany` 里还多一条规矩：同一个 `to` 在一批里只还原最新的一条 ——
列表按「一条改名记录一行」铺，同一个现名可能出现在好几行，用户全勾上很自然，
不去重的话第二条必然撞「原名已被占用」，凭空多出一串「跳过」。
`TestUndoManyTouchesEachPathOnce` 钉住它（去掉去重就红，报的是 `Missing:1`）。

**还原认的是文件身份，不是文件名。** 每条改名记录都带一个 `id` —— 那个文件在磁盘上的
唯一编号（`internal/job/fileid_windows.go`，用 `GetFileInformationByHandleEx(..., FileIdInfo)`
取「卷序列号 + 16 字节 FileID」）。编号在文件改名、挪位置之后都不变，
所以用户在程序外面把文件改了个名字，还原照样找得到它。

三条规矩，顺序不能换：

1. **先按身份找、再按路径回退。** 反过来的话，那个位置上蹲着的如果是**另一个**文件
   （用户手动把旧的删了、又放了个同名的进去），就会把别人的文件改成记录里的名字 ——
   这是这一页能造成的最坏后果。所以先拿 id 在目录里找，找不到才按路径认。
2. **按路径找到时还要回头比一次身份。** 记录里有 id、而现名那个文件的 id 对不上，
   说明「现名已经换成别的文件了」，跳过并说明原因，不动它。
   `TestUndoRefusesWhenTheNameIsNowADifferentFile` 钉住它（占位文件要**先建再顶替**，
   直接建的话 NTFS 可能复用刚删掉的 MFT 记录，编号撞上，测试会偶发绿灯）。
3. **没有 id 的老记录照旧按路径走。** 早期版本写下的记录没有 `id` 字段，
   不能因为缺字段就把它们全判成找不到。`TestUndoStillWorksForRecordsWithoutIdentity`。

`id` 是在**改名之后**取的：以「改完之后文件现在是什么」为准，而不是「改之前是什么」。

**目录级的身份索引要记得失效。** 一次还原要处理几十条记录，每条都去扫一遍目录太慢，
所以 `identityIndex` 按目录缓存一份「id → 现名」。但**改完文件必须 `forget(那个目录)`**：
链式还原的第二步会照着上一轮缓存里的路径去找文件，不清的话它会去一个已经不存在的位置
找 —— 报出来的还是「文件刚被移走或删掉」，看着像用户删了文件，其实是自己缓存过期。
`TestUndoReplaysChainedRenamesInReverse` / `TestUndoManyTouchesEachPathOnce` 就是被这条抓出来的。

硬链接会共享同一个 FileID。媒体文件里几乎不出现；真出现了也只是「挑中另一个名字」，
不会改错文件（哪个名字指的都是同一个文件）。

**记录页那一行显示的是文件「此刻」在哪，不是当初改成了什么。** `HistoryEntry.NowPath`
由同一个 `locate()` 算出来，跟原名不一致时挂一个小标「已改名」——
用户手动改过名之后最想知道的就是「它现在叫什么」，只显示记录里的旧现名等于让他自己去翻。
搜索也要能搜到 `NowPath`（记得住文件夹、记不住当时改成什么名字的情况更多）。

**「勾选」和「会动哪些文件」必须永远一致 —— 记录页的安全线。**
这一页最危险的事是「用户看不见的文件也被改了」，而它有两种造法：
筛掉、或者搜掉一部分已勾的行。兜住它的写法只有一句：

```js
// restorePicked 里
const targets = visibleHistoryRows()
  .filter(canRestore).map((e) => e.to).filter((to) => historyPicked.has(to));
```

目标是从**当前看得见的那批行**推出来的，不是直接把 `historyPicked` 丢给后端。

这里原先还有第二套等价机制：在 `paintHistoryBar` 里把「已经不在列表里的勾选」剪掉。
删了。两套互相兜底的结果是 —— **把其中任何一套删掉，测试都照样通过**（另一套接住了），
等于两套都没被验过。留下的是在使用点强制的那一套，因为它不依赖
「某个渲染一定先跑过」这种时序假设。代价是被筛掉的勾选会留着，搜索清空后还是勾着的；
这是可以接受的，数字始终等于屏幕上勾着的行数。

对应的断言在 `check_history` 里，是唯一一条**不看 DOM、直接看后端收到什么**的：

```js
App.UndoMany = (to) => { sent = to; return Promise.resolve({...}); };
button.click();
// 在「全部」下勾 3 条、再搜成只剩 1 条可见 → 按钮写着「还原所选（1）」
// → 后端必须只收到 1 条
```

DOM 断言在这里是不够的：DOM 只反映看得见的行，它证明不了集合里没有残留
一条已经被筛掉的路径。把它换回 `[...historyPicked]`，这条会红并**点名**那三个文件。

**「跳过」和「找不到」分开报，而且跳过时不能标成已还原。**
原名被别的文件占了 → 跳过，按钮留着，用户把占位文件挪开就能再点一次；
标成已还原的话那颗按钮就没了，只能去手删记录文件 —— 一条界面上的死路。

**记录页的两种「空」要分开说。** 「还没有改名记录」和「没有符合条件的记录」
各管一段：前者是新装好还没跑过任务，后者是筛错了。说反了，用户会去翻设置、
找记录文件，而其实只是关键词打错了一个字。第二种由 `check_history` 守，
第一种由 `check_history_empty` 守 —— 它跑在单独搭的 `histempty.html` 上
（`build_preview(history=[])`）。记录页的空状态和空队列一样，是另一条渲染分支，
不单独搭一份就永远只是纸上的分支。

**勾不了的勾选框要压暗。** 已还原 / 现名不在的行给的是 `disabled` 的勾选框，
但 `disabled` 本身和「还没勾」长得一样，用户点它没反应会以为界面卡了，
所以显式压了 `opacity`。

记录页的两个静态桩里，`History` 的 fixture 要**三种状态各放一条**
（可还原 / 已还原 / 已不在）。缺哪种，那一格的样式就没被截图看见过。
另外 `App.*` 的方法在不在，由 `TestPreviewStubsProvideEveryAppMethodTheFrontendCalls`
守着 —— 它第一次跑就抓到 `stub.js` 少了 `PickFFmpeg`（设置页那颗「浏览…」
在预览里点了就抛 TypeError）。顺带发现 `Undo` 桩返回的是
`{restored, failed, message}`，和 Go 的 `UndoResult` 对不上，
界面上打出来是「还原 undefined」——**桩的形状也要跟真机一致，不然截图在骗人**。

### 本地预览

改完界面不用起整个程序（WebView2 + 无边框窗口调起来比看图慢得多）。**想确认没改坏，
直接跑 `python tools/shoot_ui.py`** —— 它自己起服务、逐页截图、跑几何断言，
截图落在 `build/shots/`。下面这些是手动看单个页面的办法。

改的是模板页（槽位编辑器 / 右键菜单 / 拖放遮罩）时，用 `python tools/probe_slots.py`
只跑那三个行为探针；改的是记录页时用 `python tools/probe_history.py`。两者都不用陪着跑整套
（十几张截图 + 九个行为探针），加 `--dom` 会把探针的原始 JSON 打出来。
做变异测试时一轮要跑好几次，完整套件慢到会让人不想做——不想做的检查等于没有检查。

套件里除了十几张页面截图，还有九个**行为探针**（截图看不出来的那些）：
`确认弹窗`（Esc / 点遮罩 / 点里面）、`空队列`（表格与空状态只能看到一个）、
`数字输入框`（− / + 真的加 1 且派发 input、到顶到底置灰）、
`下拉框`（没有 select 是透明的 —— 弹出列表的底色是它推出来的）、
`槽位编辑器`（加段 / 拖动换位 / 删除 / 补充设置常驻 / 焦点落在整行 / 预览行数）、
`模板右键菜单`（重命名 / 复制 / 删除，走完整条链）、
`拖放遮罩`（拖文本不亮、拖文件才亮）、
`记录页`（只能勾可还原的 / 按钮数字 = 真正会动的文件数 / 筛选搜索）、
`记录空态`（没有记录 vs 筛完没剩下，两种说法不能混）。
另有 `check_layout` 在每次切页时顺带跑，量的是几何（比如进度条那条不该存在的线）。

**`--screenshot` 是「load 一触发就拍」，不是「加载完再等 N 毫秒」。**
所以任何放在 `setTimeout` 里的状态都赶不上 —— 只会得到几张一模一样的图，
而且**不报任何错**。`SWITCH_JS` 的切视图是同步做完的，`MENU_SHOT_JS` 弹右键菜单
则是刷**微任务**（桩里每个方法都返回已 resolve 的 Promise，`boot()` 整条链都是微任务）。
写新探针时如果发现「属性没写上去」，先怀疑这里，别怀疑脚本报错。

**还有一个坑：`--screenshot` 拍不到某些「刚建出来的浮层」。**
右键菜单弹出之后元素确实在 DOM 里、`getBoundingClientRect()` 正常、
`elementFromPoint` 也命中它，但那一帧就是不画它。逐条排除过：预置容器 vs 每次新建、
去 `focus()`、去夹边、改 z-index / 阴影 / display / position、强制重绘、
`--headless=new`、`--run-all-compositor-stages-before-draw`，全都拍不到；
而**同一份标记和样式，换成一个新插入的元素就画得出来**。拿 CDP 的
`Page.captureScreenshot` 拍同一页，菜单显示得好好的 —— 所以这是截图路径的问题，
不是菜单做错了。`MENU_SHOT_JS` 的处理办法是把菜单原样 `cloneNode` 一份新元素再收掉原件，
图里看到的就是真菜单的样子；**行为对不对由 `menuprobe` 那条断言保证，不靠这张图**。

**「要交互才出现」的状态得单独截一张。** 静态页面截图里看不到它们，
不专门截的话这块 UI 从没被看见过，配色和边距只能靠想象，
而下一轮改 CSS 也没人知道碰坏了什么。现在有两张：

- `界面-模板-右键菜单.png` —— `MENU_SHOT_JS`，`#templates+ctxmenu`；
- `界面-模板-选中某一段.png` —— `SLOTFOCUS_SHOT_JS`，`#templates+slotfocus`，
  把第 2 段的控件点着。用户报的「命名那边的块选中时样式有问题」改的就是它。

两张都靠 **digest 比对**确认「真的进了状态」：和普通页面截图撞了 digest 就报 ❌。
只验「图出来了」的话，脚本哪天没点着，图跟普通截图一模一样也照样通过。

`tools/preview/` 把 `window.go` / `window.runtime` 换成桩数据，产出可以在普通浏览器里打开的页面：

```bash
python tools/preview/build_preview.py
cd tools/preview && python -m http.server 8765

# 逐页截图
chrome --headless=new --screenshot=tasks.png --window-size=1240,820 \
       "http://127.0.0.1:8765/index.html?view=tasks"
```

`?view=` 直接切页（`tasks` / `templates` / `history` / `settings`），
`?dump=1` 把关键 DOM 值打成文本——配合 `--dump-dom` 就能在命令行里断言「值对不对」，
而不只是「看着像」：

```bash
chrome --headless=new --dump-dom --virtual-time-budget=5000 \
       "http://127.0.0.1:8765/index.html?view=templates&dump=1" | sed -n '/<pre id="DUMP">/,/<\/pre>/p'
```

截图能证明的只有「排版没崩」，值对不对、有没有 JS 报错要看 `dump` 里的
`errors` 数组（它挂在 `window.onerror` 与 `unhandledrejection` 上）。

**桩数据是编的**，只为了看排版，别拿它当功能验证。

**两个坑踩过，都修了，改这个工具时别再踩回去：**

- **错误陷阱必须在主脚本之前。** 原来它和 `__dump` 一起放在页面末尾，
  于是主脚本自己抛的错一个都收不到 —— 报出来是「无错误」，
  而页面其实是死的。放在前面才拦得住主脚本的同步异常和未处理的 Promise 拒绝。
- **桩的 `window.runtime` 漏方法 = 整个页面全废。** 前端启动时同步调
  `RT.OnFileDrop`，`tools/preview/stub.js` 当时没有它 → 在那一行抛 `TypeError`
  → **后面的代码全部不执行**：导航点击没注册、`boot()` 没调用、模板没加载。
  表现是「页面能显示但全是死的」，`?view=templates` 切不过去、
  `data-boot` 永远不出现 —— 很容易误判成「时序问题」。
  而 `tools/shoot_ui.py` 的桩有这个方法，所以截图套件一直是好的：
  两套桩各写一份，漂移就是这么来的。
  现在 `TestPreviewStubsProvideEveryRuntimeMethodTheFrontendCalls`
  钉住「前端用到的每个 `RT.*` 都必须在两个桩里存在」。
- 顺带把 `?view=` / `?dump=` 从固定延时改成**盯着 `data-boot="done"`**。
  `--timeout` 是「加载的最长等待」而不是「加载完再等 N 毫秒」，
  固定延时是碰运气。

### 改了 id 或绑定方法之后

跑 `go test . -run TestFrontend`。前端和后端之间是字符串约定，编译器看不见，
这个测试会把失配提前到提交前（见上面「前端契约层」）。

## 已知限制

- **只做 Windows/amd64。** 没有为其他平台留冗余代码。
- **没有 Opus 解码。** 见上，需要 C 工具链。
- **AVI / WMV / RMVB 需要 ffmpeg。** rodio 不认识这些容器。
- **暂停是软的。** 在下一个文件开始前生效，不会掐断正在跑的那一次识别
  （一次识别只有几十毫秒 CPU 加一次网络往返，中途放弃反而要重新解码）。
- **撤销记录不删，只追加「已还原」标记。** 见下面「改名记录」那一节。
