//! 命令行识别 sidecar：读一个媒体文件 → 算指纹 → 问 Shazam → 输出 JSON。
//!
//! 这个程序是 Go GUI 的「耳朵」。它只做一件事，所有输出都是 stdout 上的一行 JSON，
//!  日志一律走 stderr，方便调用方直接解析。
//!
//! 除了识别本身，还有三个不碰文件（或只碰文件不联网）的旁路，界面上的自检与
//!  排错都靠它们：
//!   `--env`            报告运行环境：版本、ffmpeg 实际路径、代理怎么走
//!   `--check-network`  按当前代理设置真的问一次 Shazam，验证连通性
//!   `--probe`          只算指纹不联网，用来把「算法问题」和「网络问题」分开
//!
//! 退出码：0 匹配成功 / 2 未匹配 / 3 解码或容器不支持 / 4 网络失败 /
//!  5 被限流 / 6 Shazam 应答异常 / 1 参数错误。

mod fingerprinting;
mod shazam;

use std::io::Write;
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::Duration;

use fingerprinting::algorithm::SignatureGenerator;
use fingerprinting::signature_format::DecodedSignature;
use serde_json::json;

const VERSION: &str = env!("CARGO_PKG_VERSION");

/// 上游 `Shazam()` 用的窗口。源码注释写明：Shazam 对 ≥15 秒的片段一律返回
///  `matches: []`，客户端发的是 12 秒。
const DEFAULT_SEGMENT_SECONDS: u32 = 12;
const DEFAULT_LANGUAGE: &str = "en-US";
const DEFAULT_COUNTRY: &str = "GB";
const DEFAULT_TIMEOUT_SECONDS: u64 = 20;

/// 上游的注释：只有「App 的」User-Agent 能进 API。浏览器 UA 会被回一整个
///  `200 text/html` 的网页，较新的 Chrome 串直接 `405`。
const DEFAULT_USER_AGENT: &str = "Dalvik/2.1.0 (Linux; U; Android 5.1.1; SM-J700H Build/LMY48B)";

const EXIT_MATCHED: u8 = 0;
const EXIT_USAGE: u8 = 1;
const EXIT_NO_MATCH: u8 = 2;
const EXIT_DECODE: u8 = 3;
const EXIT_NETWORK: u8 = 4;
const EXIT_RATE_LIMITED: u8 = 5;
const EXIT_UPSTREAM: u8 = 6;

const HELP: &str = "\
shazam-sidecar - 识别媒体文件里的歌曲，输出 JSON

用法:
  shazam-sidecar --file <路径> [选项]

选项:
  --file <路径>            要识别的媒体文件（必填，--env 时不需要）
  --seconds <秒>           取样窗口，默认 12。≥15 秒 Shazam 一律不返回结果
  --language <语言>        首选语言，默认 en-US
  --country <国家>         首选接口国家代码，默认 GB
  --locale <语言>/<国家>   额外再查一次，可重复。每次都是一次完整的接口调用
  --min-interval <毫秒>    两次请求之间的最小间隔，默认 0。额外语种的几次调用
                           会按这个间隔依次发出，而不是连成一串
  --timeout <秒>           单次请求超时，默认 20
  --user-agent <字符串>    覆盖 User-Agent（必须是 App 串，浏览器串会被拒）
  --proxy <地址>           代理。不给就跟随 HTTP_PROXY / HTTPS_PROXY 环境变量；
                           填 - / off / direct 表示强制直连，连环境变量也不看；
                           其余当作代理地址，要带协议头，如 socks5://127.0.0.1:1080
  --ffmpeg <路径>          指定 ffmpeg。不给就按 PATH → 同目录 探测
  --probe                  只算指纹，不联网。用于自检与排错
  --check-network          只测网络连通性：按当前代理设置问一次 Shazam，不识别文件
  --env                    只报告运行环境（ffmpeg 在哪、代理怎么走），不识别任何文件
  --dump-signature <路径>  把签名 URI 写到文件，用于与上游逐字节比对
  --pretty                 输出缩进过的 JSON
  -h, --help               显示本帮助
  -V, --version            显示版本

退出码:
  0 匹配成功   2 未匹配   3 解码失败或不支持的容器
  4 网络失败   5 被限流   6 Shazam 应答异常   1 参数错误
";

/// 一次额外的语种查询。
///
/// 同一份指纹换 `{language}/{country}` 再问一次，看看同一首歌在别的
/// storefront 下是不是给了另一个写法。每加一条就是一次完整的接口调用，
/// 所以默认一条都不加——只有用户显式配了才会发生。
#[derive(Debug, Clone)]
struct Locale {
    language: String,
    country: String,
}

/// 代理怎么走。
///
/// 三态而不是 `Option<String>`：`--proxy` 缺席与 `--proxy -` 是两件不同的事。
/// 前者是「我没意见，按环境变量来」，后者是「我知道环境里有代理，但这次
/// 不要用」。合成一个 `None` 会让设了 `HTTP_PROXY` 的用户永远绕不开它。
#[derive(Debug, Clone, PartialEq)]
enum ProxyChoice {
    /// 没给 `--proxy`：交给 reqwest 自己读 HTTP_PROXY / HTTPS_PROXY / NO_PROXY。
    FollowEnv,
    /// 给了 `--proxy -`（或 off / direct）：显式直连，环境变量也不看。
    Direct,
    /// 显式指定的代理地址。
    Url(String),
}

impl ProxyChoice {
    /// 给人看的说明，会写进 `--env` 的输出。
    fn describe(&self) -> String {
        match self {
            ProxyChoice::FollowEnv => "跟随环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）".to_string(),
            ProxyChoice::Direct => "强制直连".to_string(),
            ProxyChoice::Url(url) => url.clone(),
        }
    }
}

/// 解析 `--proxy` 的取值。
///
/// 要求带协议头是有意的：`127.0.0.1:7890` 这种写法 reqwest 会当成一个没有
/// scheme 的 URL 拒掉，错误消息里只有一句 "builder error"，用户根本猜不到
/// 是少了 `http://`。在这里挡掉，就能直接说清楚缺什么。
fn parse_proxy(raw: &str) -> Result<ProxyChoice, String> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return Err(
            "--proxy 的值是空的。想强制直连请写 -，想跟随环境变量请不要传 --proxy".to_string(),
        );
    }
    match trimmed.to_ascii_lowercase().as_str() {
        "-" | "off" | "none" | "direct" => Ok(ProxyChoice::Direct),
        _ => {
            if !trimmed.contains("://") {
                return Err(format!(
                    "--proxy 要带协议头，例如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080，收到: {raw}"
                ));
            }
            Ok(ProxyChoice::Url(trimmed.to_string()))
        }
    }
}

/// 按代理设置建一个 HTTP 客户端。
///
/// 返回的第二个值是要写进 `--env` / `--check-network` 的说明文字。
fn build_client(
    proxy: &ProxyChoice,
    timeout_seconds: u64,
) -> Result<reqwest::Client, String> {
    let builder = reqwest::Client::builder().timeout(Duration::from_secs(timeout_seconds));

    let builder = match proxy {
        // 不设任何 proxy：reqwest 的默认行为就是读环境变量。
        ProxyChoice::FollowEnv => builder,
        // `no_proxy()` 关掉的是「自动探测」，显式设的 proxy 不受影响，
        // 所以这一支必须单独走，不能靠「不设」来达成。
        ProxyChoice::Direct => builder.no_proxy(),
        ProxyChoice::Url(url) => {
            // `all()` 而不是 `http()` + `https()`：一个地址同时管两种协议，
            // 分两次设迟早漏一个，而漏掉的那一半会静默直连。
            let parsed = reqwest::Proxy::all(url)
                .map_err(|error| format!("代理地址无法解析（{url}）: {error}"))?;
            builder.proxy(parsed)
        }
    };

    builder
        .build()
        .map_err(|error| format!("创建 HTTP 客户端失败: {error}"))
}

/// 读出环境里所有能影响代理的变量。
///
/// reqwest 会自己看这些，但用户在界面上一个都看不到。代理配错时最常见的一句
/// 就是「我明明没设代理啊」——把这份快照放进 `--env` 的输出，才能当场看见
/// 是哪个变量在起作用（Windows 上 `HTTP_PROXY` 是大小写不敏感匹配的，
/// 所以两种写法都要收）。
fn proxy_env_snapshot() -> serde_json::Value {
    let mut found = serde_json::Map::new();
    for name in [
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "NO_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
        "no_proxy",
    ] {
        if let Ok(value) = std::env::var(name) {
            if !value.trim().is_empty() {
                found.insert(name.to_string(), json!(value));
            }
        }
    }
    serde_json::Value::Object(found)
}

/// 建一个供网络请求用的 tokio 运行时。
fn network_runtime() -> Result<tokio::runtime::Runtime, String> {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .map_err(|error| format!("启动运行时失败: {error}"))
}

#[derive(Debug)]
struct Args {
    file: PathBuf,
    seconds: u32,
    language: String,
    country: String,
    locales: Vec<Locale>,
    min_interval_ms: u64,
    timeout_seconds: u64,
    user_agent: String,
    proxy: ProxyChoice,
    ffmpeg: Option<PathBuf>,
    probe: bool,
    dump_signature: Option<PathBuf>,
    pretty: bool,
}

enum Parsed {
    Run(Box<Args>),
    /// 只报告运行环境，不碰任何文件。
    Env { pretty: bool, proxy: ProxyChoice },
    /// 只测网络连通性，不碰任何文件。
    CheckNetwork(Box<CheckNetworkArgs>),
    Help,
    Version,
}

/// `--check-network` 需要的参数。比 `Args` 少一大截——探测不看文件，
/// 也不关心语种。
struct CheckNetworkArgs {
    proxy: ProxyChoice,
    timeout_seconds: u64,
    user_agent: String,
    pretty: bool,
}

fn parse_args(argv: Vec<String>) -> Result<Parsed, String> {
    let mut file: Option<PathBuf> = None;
    let mut seconds = DEFAULT_SEGMENT_SECONDS;
    let mut language = DEFAULT_LANGUAGE.to_string();
    let mut country = DEFAULT_COUNTRY.to_string();
    let mut locales: Vec<Locale> = Vec::new();
    let mut min_interval_ms: u64 = 0;
    let mut timeout_seconds = DEFAULT_TIMEOUT_SECONDS;
    let mut user_agent = DEFAULT_USER_AGENT.to_string();
    let mut proxy = ProxyChoice::FollowEnv;
    let mut ffmpeg: Option<PathBuf> = None;
    let mut probe = false;
    let mut env = false;
    let mut check_network = false;
    let mut dump_signature: Option<PathBuf> = None;
    let mut pretty = false;

    let mut iter = argv.into_iter();
    while let Some(arg) = iter.next() {
        // 取下一个值，缺了就是用法错误。
        let mut value = |name: &str| -> Result<String, String> {
            iter.next().ok_or_else(|| format!("{name} 缺少取值"))
        };
        match arg.as_str() {
            "-h" | "--help" => return Ok(Parsed::Help),
            "-V" | "--version" => return Ok(Parsed::Version),
            "--file" => file = Some(PathBuf::from(value("--file")?)),
            "--seconds" => {
                let raw = value("--seconds")?;
                let parsed = raw
                    .parse::<u32>()
                    .map_err(|_| format!("--seconds 不是正整数: {raw}"))?;
                if parsed == 0 {
                    return Err("--seconds 至少为 1".to_string());
                }
                seconds = parsed;
            }
            "--language" => language = value("--language")?,
            "--country" => country = value("--country")?,
            "--locale" => {
                let raw = value("--locale")?;
                // 格式是「语言/国家」，例如 zh-CN/CN。缺斜杠就当用法错误，
                // 免得把 "zh-CN" 当成语言、国家留空然后发一个 404 出去。
                let (lang, country) = raw
                    .split_once('/')
                    .ok_or_else(|| format!("--locale 要写成 语言/国家，例如 zh-CN/CN，收到: {raw}"))?;
                if lang.trim().is_empty() || country.trim().is_empty() {
                    return Err(format!("--locale 的语言和国家都不能为空: {raw}"));
                }
                locales.push(Locale {
                    language: lang.trim().to_string(),
                    country: country.trim().to_string(),
                });
            }
            "--timeout" => {
                let raw = value("--timeout")?;
                timeout_seconds = raw
                    .parse::<u64>()
                    .map_err(|_| format!("--timeout 不是正整数: {raw}"))?;
            }
            "--min-interval" => {
                let raw = value("--min-interval")?;
                min_interval_ms = raw
                    .parse::<u64>()
                    .map_err(|_| format!("--min-interval 不是正整数（毫秒）: {raw}"))?;
            }
            "--user-agent" => user_agent = value("--user-agent")?,
            "--proxy" => proxy = parse_proxy(&value("--proxy")?)?,
            "--ffmpeg" => ffmpeg = Some(PathBuf::from(value("--ffmpeg")?)),
            "--probe" => probe = true,
            "--env" => env = true,
            "--check-network" => check_network = true,
            "--dump-signature" => dump_signature = Some(PathBuf::from(value("--dump-signature")?)),
            "--pretty" => pretty = true,
            other => return Err(format!("未知参数: {other}")),
        }
    }

    if env {
        return Ok(Parsed::Env { pretty, proxy });
    }

    // `--check-network` 优先于 `--file`：探测模式本来就不需要文件，
    // 两个都给时按探测走，而不是报「缺 --file」。
    if check_network {
        return Ok(Parsed::CheckNetwork(Box::new(CheckNetworkArgs {
            proxy,
            timeout_seconds,
            user_agent,
            pretty,
        })));
    }

    let file = file.ok_or_else(|| "--file 是必填项".to_string())?;
    Ok(Parsed::Run(Box::new(Args {
        file,
        seconds,
        language,
        country,
        locales,
        min_interval_ms,
        timeout_seconds,
        user_agent,
        proxy,
        ffmpeg,
        probe,
        dump_signature,
        pretty,
    })))
}

/// 追加一个非空、且还没出现过的字符串。
///
/// 去重是必要的：大多数曲目在各个 storefront 下返回的是同一个标题，
/// 不过滤的话 `titles` 里会是三份一模一样的字符串。
fn push_unique(list: &mut Vec<String>, value: &str) {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        return;
    }
    if list
        .iter()
        .any(|existing| existing.eq_ignore_ascii_case(trimmed))
    {
        return;
    }
    list.push(trimmed.to_string());
}

/// 输出一行 JSON 并返回退出码。`value` 由调用方决定形状，这里只负责落盘。
fn emit(value: serde_json::Value, pretty: bool, code: u8) -> ExitCode {
    let rendered = if pretty {
        serde_json::to_string_pretty(&value)
    } else {
        serde_json::to_string(&value)
    }
    .unwrap_or_else(|_| r#"{"ok":false,"status":"internal","error":"序列化失败"}"#.to_string());

    let stdout = std::io::stdout();
    let mut handle = stdout.lock();
    let _ = writeln!(handle, "{rendered}");
    let _ = handle.flush();

    ExitCode::from(code)
}

/// 解码失败的原因分类。
///
/// `make_signature_from_file` 把底层错误装进 `Box<dyn Error>`：rodio 读不了的
///  容器、IO 错误、以及两者都失败时 ffmpeg 的错误。能下转成 `io::Error` 的按
///  IO 归类，其余按消息判断——rodio 对无法识别的容器给的是
///  `Unrecognized format`，这跟「文件坏了」是两回事，GUI 要能区分开。
fn classify_decode_error(error: &(dyn std::error::Error + 'static)) -> &'static str {
    if let Some(io_error) = error.downcast_ref::<std::io::Error>() {
        if io_error.kind() == std::io::ErrorKind::NotFound {
            return "missing_file";
        }
        return "io";
    }

    let message = error.to_string().to_lowercase();
    if message.contains("unrecognized")
        || message.contains("unsupported")
        || message.contains("no suitable")
        || message.contains("no stream")
    {
        "unsupported"
    } else {
        "decode"
    }
}

/// `--check-network`：按当前的代理设置真的问一次 Shazam。
///
/// 存在的理由：代理填错这件事在批量跑之前是不可见的，用户要等到第一批文件
/// 全部失败才发现，而且失败信息里通常只有一句「网络错误」。这里花一次轻量
/// 请求把它提前暴露出来，并且把「用的是哪个代理」一并回报。
///
/// 退出码沿用整套约定：0 通，4 网络层不通，6 应答异常，1 参数错误。
fn check_network(args: &CheckNetworkArgs) -> ExitCode {
    let client = match build_client(&args.proxy, args.timeout_seconds) {
        Ok(client) => client,
        Err(message) => {
            return emit(
                json!({
                    "ok": false,
                    "status": "internal",
                    "error": message,
                    "proxy": args.proxy.describe(),
                }),
                args.pretty,
                EXIT_USAGE,
            );
        }
    };

    let runtime = match network_runtime() {
        Ok(runtime) => runtime,
        Err(message) => {
            return emit(
                json!({ "ok": false, "status": "internal", "error": message }),
                args.pretty,
                EXIT_UPSTREAM,
            );
        }
    };

    let started = std::time::Instant::now();
    let outcome = runtime.block_on(shazam::probe(&client, &args.user_agent));
    let elapsed_ms = started.elapsed().as_millis() as u64;

    match outcome {
        // 拿到应答就算通——哪怕 4xx。这条路径不带签名，实测被回 404 是正常的。
        Ok(http_status) => emit(
            json!({
                "ok": true,
                "status": "network_ok",
                "proxy": args.proxy.describe(),
                "http_status": http_status,
                "elapsed_ms": elapsed_ms,
            }),
            args.pretty,
            EXIT_MATCHED,
        ),
        Err(error) => {
            let kind = error.kind();
            emit(
                json!({
                    "ok": false,
                    "status": kind,
                    "error": error.to_string(),
                    "proxy": args.proxy.describe(),
                    "elapsed_ms": elapsed_ms,
                }),
                args.pretty,
                if kind == "network" {
                    EXIT_NETWORK
                } else {
                    EXIT_UPSTREAM
                },
            )
        }
    }
}

fn main() -> ExitCode {
    let argv: Vec<String> = std::env::args().skip(1).collect();
    let args = match parse_args(argv) {
        Ok(Parsed::Help) => {
            print!("{HELP}");
            return ExitCode::SUCCESS;
        }
        Ok(Parsed::Version) => {
            println!("shazam-sidecar {VERSION}");
            return ExitCode::SUCCESS;
        }
        Ok(Parsed::Env { pretty, proxy }) => {
            let ffmpeg = fingerprinting::ffmpeg_wrapper::resolved_ffmpeg_path();
            return emit(
                json!({
                    "ok": true,
                    "status": "env",
                    "version": VERSION,
                    "ffmpeg_path": ffmpeg.map(|path| path.display().to_string()).unwrap_or_default(),
                    "proxy": proxy.describe(),
                    "proxy_env": proxy_env_snapshot(),
                }),
                pretty,
                EXIT_MATCHED,
            );
        }
        Ok(Parsed::CheckNetwork(args)) => return check_network(&args),
        Ok(Parsed::Run(args)) => args,
        Err(message) => {
            eprintln!("参数错误: {message}\n\n{HELP}");
            return ExitCode::from(EXIT_USAGE);
        }
    };

    // 用户指定的 ffmpeg 要在任何解码动作之前登记进去。
    fingerprinting::ffmpeg_wrapper::set_ffmpeg_override(args.ffmpeg.clone());

    if !args.file.is_file() {
        return emit(
            json!({ "ok": false, "status": "missing_file", "error": format!("文件不存在: {}", args.file.display()) }),
            args.pretty,
            EXIT_USAGE,
        );
    }

    // 指纹是同步的 CPU 活，先算完再进网络，失败也不浪费一次请求。
    let signature: DecodedSignature =
        match SignatureGenerator::make_signature_from_file(&args.file, Some(args.seconds)) {
            Ok(signature) => signature,
            Err(error) => {
                let kind = classify_decode_error(error.as_ref());
                return emit(
                    json!({
                        "ok": false,
                        "status": kind,
                        "error": error.to_string(),
                        "file": args.file.display().to_string(),
                    }),
                    args.pretty,
                    EXIT_DECODE,
                );
            }
        };

    let peak_count: usize = signature
        .frequency_band_to_sound_peaks
        .values()
        .map(Vec::len)
        .sum();

    if let Some(path) = &args.dump_signature {
        let uri = match signature.encode_to_uri() {
            Ok(uri) => uri,
            Err(error) => {
                return emit(
                    json!({ "ok": false, "status": "decode", "error": format!("编码签名失败: {error}") }),
                    args.pretty,
                    EXIT_DECODE,
                );
            }
        };
        if let Err(error) = std::fs::write(path, uri.as_bytes()) {
            return emit(
                json!({ "ok": false, "status": "io", "error": format!("写入签名失败: {error}") }),
                args.pretty,
                EXIT_DECODE,
            );
        }
    }

    if args.probe {
        let uri = signature.encode_to_uri().unwrap_or_default();
        return emit(
            json!({
                "ok": true,
                "status": "probe",
                "file": args.file.display().to_string(),
                "segment_seconds": args.seconds,
                "sample_rate_hz": signature.sample_rate_hz,
                "number_samples": signature.number_samples,
                "duration_ms": (signature.number_samples as f64 / signature.sample_rate_hz as f64 * 1000.0).round() as u64,
                "peak_count": peak_count,
                "signature_uri_bytes": uri.len(),
            }),
            args.pretty,
            EXIT_MATCHED,
        );
    }

    let runtime = match network_runtime() {
        Ok(runtime) => runtime,
        Err(message) => {
            return emit(
                json!({ "ok": false, "status": "internal", "error": message }),
                args.pretty,
                EXIT_UPSTREAM,
            );
        }
    };

    runtime.block_on(async {
        let client = match build_client(&args.proxy, args.timeout_seconds) {
            Ok(client) => client,
            Err(message) => {
                return emit(
                    json!({ "ok": false, "status": "internal", "error": message }),
                    args.pretty,
                    EXIT_USAGE,
                );
            }
        };

        match shazam::lookup(
            &client,
            &signature,
            &args.language,
            &args.country,
            &args.user_agent,
        )
        .await
        {
            Ok(track) if track.matched => {
                // 额外语种：同一份指纹换 {language}/{country} 再问几次，
                // 看看同一首歌在别的 storefront 下是不是给了另一个写法。
                //
                // 每条都是一次完整的接口调用。默认 locales 是空的，所以
                // 不配就一次多余的请求都不发。
                let mut titles: Vec<String> = Vec::new();
                let mut artists: Vec<String> = Vec::new();
                push_unique(&mut titles, &track.title);
                push_unique(&mut artists, &track.artist);

                // 额外语种循环里吃到 429 时记在这里：文件本身已经有结果了，
                // 但这条 IP 已经被盯上，Go 侧要凭它让整条流水线慢下来。
                let mut limited: Option<u64> = None;

                for locale in args.locales.iter() {
                    // 这几次调用也得守同一个节奏。编排层只能节流「文件之间」，
                    // 文件内部的这几次它根本看不见 —— 不在这里睡，一个配了
                    // 四个语种的文件就是五次请求零间隔齐射。
                    if args.min_interval_ms > 0 {
                        tokio::time::sleep(std::time::Duration::from_millis(
                            args.min_interval_ms,
                        ))
                        .await;
                    }

                    match shazam::lookup(
                        &client,
                        &signature,
                        &locale.language,
                        &locale.country,
                        &args.user_agent,
                    )
                    .await
                    {
                        Ok(other) => {
                            if other.matched {
                                push_unique(&mut titles, &other.title);
                                push_unique(&mut artists, &other.artist);
                            }
                        }
                        // 被限流就停手：剩下的语种再问也是一样的结果，
                        // 而每问一次都会把封禁窗口撞得更长。
                        Err(shazam::LookupError::RateLimited(seconds)) => {
                            limited = Some(seconds.unwrap_or(0));
                            break;
                        }
                        // 其它错误（网络抖动、5xx）换个语种还有可能成。
                        Err(_) => continue,
                    }
                }

                emit(
                    json!({
                        "ok": true,
                        "status": "matched",
                        "file": args.file.display().to_string(),
                        "title": track.title,
                        "artist": track.artist,
                        "album": track.album,
                        "genre": track.genre,
                        "released": track.released,
                        "label": track.label,
                        "cover_art": track.cover_art,
                        "shazam_url": track.shazam_url,
                        "track_key": track.track_key,
                        "titles": titles,
                        "artists": artists,
                        "rate_limited": limited.is_some(),
                        "retry_after": limited.unwrap_or(0),
                        "segment_seconds": args.seconds,
                    }),
                    args.pretty,
                    EXIT_MATCHED,
                )
            }
            Ok(_) => emit(
                json!({
                    "ok": true,
                    "status": "no_match",
                    "file": args.file.display().to_string(),
                    "segment_seconds": args.seconds,
                }),
                args.pretty,
                EXIT_NO_MATCH,
            ),
            Err(error) => {
                let code = match error {
                    shazam::LookupError::RateLimited(_) => EXIT_RATE_LIMITED,
                    shazam::LookupError::Network(_) => EXIT_NETWORK,
                    shazam::LookupError::Upstream(_) => EXIT_UPSTREAM,
                    shazam::LookupError::Rejected(_) => EXIT_UPSTREAM,
                };
                emit(
                    json!({
                        "ok": false,
                        "status": error.kind(),
                        "retryable": error.retryable(),
                        "retry_after": error.retry_after().unwrap_or(0),
                        "error": error.to_string(),
                        "file": args.file.display().to_string(),
                    }),
                    args.pretty,
                    code,
                )
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn proxy_absent_follows_environment() {
        // 不传 --proxy 与「传了个空值」是两件事，前者不能被解析成直连。
        let parsed = parse_args(vec!["--file".into(), "a.mp4".into()]).unwrap();
        match parsed {
            Parsed::Run(args) => assert_eq!(args.proxy, ProxyChoice::FollowEnv),
            _ => panic!("应当解析成 Run"),
        }
    }

    #[test]
    fn proxy_off_switches_to_direct() {
        for raw in ["-", "off", "OFF", "none", "direct", " direct "] {
            assert_eq!(
                parse_proxy(raw).unwrap(),
                ProxyChoice::Direct,
                "{raw} 应当表示强制直连"
            );
        }
    }

    #[test]
    fn proxy_url_is_kept_verbatim() {
        assert_eq!(
            parse_proxy(" http://127.0.0.1:7890 ").unwrap(),
            ProxyChoice::Url("http://127.0.0.1:7890".to_string())
        );
        assert_eq!(
            parse_proxy("socks5://127.0.0.1:1080").unwrap(),
            ProxyChoice::Url("socks5://127.0.0.1:1080".to_string())
        );
    }

    #[test]
    fn proxy_without_scheme_is_rejected_with_a_useful_message() {
        // reqwest 对这种输入只会回一句 "builder error"，用户猜不到缺什么，
        // 所以必须在解析阶段挡掉并把话说清楚。
        let error = parse_proxy("127.0.0.1:7890").unwrap_err();
        assert!(error.contains("协议头"), "错误消息要指出缺协议头: {error}");
        assert!(error.contains("127.0.0.1:7890"), "错误消息要回显原值: {error}");
    }

    #[test]
    fn proxy_empty_value_is_rejected() {
        let error = parse_proxy("   ").unwrap_err();
        assert!(error.contains("-"), "要告诉用户想直连该写什么: {error}");
    }

    #[test]
    fn check_network_does_not_require_a_file() {
        // 探测模式本来就不看文件，此时报「--file 是必填项」是纯粹的误伤。
        let parsed = parse_args(vec![
            "--check-network".into(),
            "--proxy".into(),
            "http://127.0.0.1:7890".into(),
        ])
        .unwrap();
        match parsed {
            Parsed::CheckNetwork(args) => {
                assert_eq!(args.proxy, ProxyChoice::Url("http://127.0.0.1:7890".into()));
            }
            _ => panic!("应当解析成 CheckNetwork"),
        }
    }

    #[test]
    fn env_reports_the_proxy_choice() {
        let parsed = parse_args(vec![
            "--env".into(),
            "--proxy".into(),
            "socks5://127.0.0.1:1080".into(),
        ])
        .unwrap();
        match parsed {
            Parsed::Env { proxy, .. } => {
                assert_eq!(proxy, ProxyChoice::Url("socks5://127.0.0.1:1080".into()));
            }
            _ => panic!("应当解析成 Env"),
        }
    }

    #[test]
    fn direct_client_builds_and_url_client_builds() {
        // `no_proxy()` 与 `proxy()` 走的是两条不同的分支，都要真的建一次，
        // 免得其中一条只在运行时才炸。
        assert!(build_client(&ProxyChoice::FollowEnv, 5).is_ok());
        assert!(build_client(&ProxyChoice::Direct, 5).is_ok());
        assert!(build_client(&ProxyChoice::Url("http://127.0.0.1:7890".into()), 5).is_ok());
    }

    #[test]
    fn bad_proxy_url_surfaces_the_address() {
        let error = build_client(&ProxyChoice::Url("http://".into()), 5).unwrap_err();
        assert!(error.contains("http://"), "错误里要带上填的那个地址: {error}");
    }

    #[test]
    fn min_interval_is_optional_and_parsed_in_ms() {
        // 不传就是 0：此时 sidecar 一次多余的一次睡眠都不做，
        // 老版本 Go 侧（不传这个参数）的行为完全不变。
        match parse_args(vec!["--file".into(), "a.mp4".into()]).unwrap() {
            Parsed::Run(args) => assert_eq!(args.min_interval_ms, 0),
            _ => panic!("应当解析成 Run"),
        }

        match parse_args(vec![
            "--file".into(),
            "a.mp4".into(),
            "--min-interval".into(),
            "1500".into(),
        ])
        .unwrap()
        {
            Parsed::Run(args) => assert_eq!(args.min_interval_ms, 1500),
            _ => panic!("应当解析成 Run"),
        }
    }

    #[test]
    fn rate_limited_error_carries_retry_after() {
        // Go 侧靠这个字段决定冷却多久；写成 HTTP-date 那种形式解析不了就给 None，
        // 让它用自己的退避曲线，别在这边猜。
        let err = shazam::LookupError::RateLimited(Some(42));
        assert_eq!(err.kind(), "rate_limited");
        assert_eq!(err.retry_after(), Some(42));
        assert!(err.to_string().contains("42"));

        assert_eq!(shazam::LookupError::RateLimited(None).retry_after(), None);
    }
}
