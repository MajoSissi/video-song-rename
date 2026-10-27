//! 与 Shazam 的 HTTP 通信。
//!
//! 请求的形状（URL、请求头、body 字段）完全对齐 `shazamio` 的
//! `api.py::send_recognize_request_v2`，指纹部分由 `fingerprinting` 模块产出。

use std::error::Error as _;

use reqwest::header::{HeaderMap, HeaderValue, ACCEPT, ACCEPT_LANGUAGE, USER_AGENT};
use reqwest::StatusCode;
use serde_json::{json, Value};

use crate::fingerprinting::communication::get_signature_json;
use crate::fingerprinting::signature_format::DecodedSignature;

/// 与上游一致：`discovery/v5` 只在 `amp.` 与 `cdn.` 上应答，
/// 换成 `www.` 会返回 1.7MB 的单页应用外壳。
const SEARCH_FROM_FILE: &str = concat!(
    "https://amp.shazam.com/discovery/v5/{language}/{country}/iphone/-/tag/",
    "{uuid_1}/{uuid_2}?sync=true&webv3=true&sampling=true&connected=",
    "&shazamapiversion=v3&sharehub=true&hubv5minorversion=v5.1&hidelb=true&video=v3"
);

/// 上游固定用 IPHONE 这个设备档位：`web` 档不返回 Spotify / Deezer 链接，
/// `android` 档把 `apple_music_url` 返成浏览器打不开的 `intent://`。
const X_SHAZAM_PLATFORM: &str = "IPHONE";
const X_SHAZAM_APP_VERSION: &str = "14.1.0";

/// 识别结果。`matched == false` 表示 Shazam 正常应答但没有匹配到曲目。
#[derive(Debug, Default)]
pub struct Track {
    pub matched: bool,
    pub title: String,
    pub artist: String,
    pub album: String,
    pub genre: String,
    pub released: String,
    pub label: String,
    pub cover_art: String,
    pub shazam_url: String,
    pub track_key: String,
}

/// 一次识别请求的失败原因。Go 侧靠这个分类决定要不要重试。
#[derive(Debug)]
pub enum LookupError {
    /// 429。必须退避，重试越快越糟。带的是应答里 `Retry-After` 的值（秒），
    /// 没有这个头就是 None —— 实测它经常没有，所以不能指望。
    RateLimited(Option<u64>),
    /// 连接超时、DNS、TLS 等网络层问题。可重试。
    Network(String),
    /// 5xx 或无法解析的应答。可重试。
    Upstream(String),
    /// 4xx（429 除外）。请求本身有问题，重试无用。
    Rejected(String),
}

impl std::fmt::Display for LookupError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            LookupError::RateLimited(None) => write!(f, "Shazam 限流（HTTP 429）"),
            LookupError::RateLimited(Some(seconds)) => {
                write!(f, "Shazam 限流（HTTP 429，Retry-After {seconds} 秒）")
            }
            LookupError::Network(msg) => write!(f, "网络错误: {msg}"),
            LookupError::Upstream(msg) => write!(f, "Shazam 应答异常: {msg}"),
            LookupError::Rejected(msg) => write!(f, "Shazam 拒绝了请求: {msg}"),
        }
    }
}

impl LookupError {
    /// 供 Go 侧判断是否重试。
    pub fn kind(&self) -> &'static str {
        match self {
            LookupError::RateLimited(_) => "rate_limited",
            LookupError::Network(_) => "network",
            LookupError::Upstream(_) => "upstream",
            LookupError::Rejected(_) => "rejected",
        }
    }

    pub fn retryable(&self) -> bool {
        matches!(
            self,
            LookupError::RateLimited(_) | LookupError::Network(_) | LookupError::Upstream(_)
        )
    }

    /// 429 应答里的 `Retry-After`（秒）。不是限流错误就返回 None。
    pub fn retry_after(&self) -> Option<u64> {
        match self {
            LookupError::RateLimited(seconds) => *seconds,
            _ => None,
        }
    }
}

/// 把 reqwest 的错误展开成「一句话 + 最里层的原因」。
///
/// `reqwest::Error` 的 `Display` 只有一句 `error sending request for url (…)`，
/// 真正有用的信息全在 `source()` 链上：连接被拒、DNS 解析不了、TLS 握手失败。
/// 代理配错的时候，这两者的差别就是「一句废话」和「连接被拒绝」——
/// 前者用户完全无从下手，后者他立刻知道是代理端口没开。
///
/// 只取**最里层**那一条，不整条链拼出来：中间几层是 `client error (Connect)`
/// 这类框架自己的话，对用户没有意义，堆在一起反而把关键那句淹掉。
fn describe_error(error: &reqwest::Error) -> String {
    let mut message = error.to_string();

    let mut source = error.source();
    let mut deepest: Option<String> = None;
    while let Some(cause) = source {
        let text = cause.to_string();
        if !text.trim().is_empty() {
            deepest = Some(text);
        }
        source = cause.source();
    }

    if let Some(text) = deepest {
        if !message.contains(&text) {
            message.push_str(" → ");
            message.push_str(&text);
        }
    }
    message
}

fn build_headers(language: &str, user_agent: &str) -> Result<HeaderMap, LookupError> {
    let mut headers = HeaderMap::new();
    headers.insert(
        "X-Shazam-Platform",
        HeaderValue::from_static(X_SHAZAM_PLATFORM),
    );
    headers.insert(
        "X-Shazam-AppVersion",
        HeaderValue::from_static(X_SHAZAM_APP_VERSION),
    );
    headers.insert(ACCEPT, HeaderValue::from_static("*/*"));
    headers.insert(
        ACCEPT_LANGUAGE,
        HeaderValue::from_str(language)
            .map_err(|error| LookupError::Rejected(format!("Accept-Language 非法: {error}")))?,
    );
    headers.insert(
        USER_AGENT,
        HeaderValue::from_str(user_agent)
            .map_err(|error| LookupError::Rejected(format!("User-Agent 非法: {error}")))?,
    );
    Ok(headers)
}

/// 把一段音频的指纹发去 Shazam，返回识别结果。
pub async fn lookup(
    client: &reqwest::Client,
    signature: &DecodedSignature,
    language: &str,
    country: &str,
    user_agent: &str,
) -> Result<Track, LookupError> {
    let envelope = get_signature_json(signature)
        .map_err(|error| LookupError::Upstream(format!("构造请求体失败: {error}")))?;

    let url = SEARCH_FROM_FILE
        .replace("{language}", language)
        .replace("{country}", country)
        .replace("{uuid_1}", &uuid::Uuid::new_v4().to_string().to_uppercase())
        .replace("{uuid_2}", &uuid::Uuid::new_v4().to_string().to_uppercase());

    // 字段顺序与取值对齐上游 Converter.data_search：geolocation 与 context 都是空对象，
    //  fingerprint 里算出来的那份并不发出去。
    let body = json!({
        "timezone": envelope.timezone,
        "signature": {
            "uri": envelope.signature.uri,
            "samplems": envelope.signature.samples,
        },
        "timestamp": envelope.timestamp,
        "context": {},
        "geolocation": {},
    });

    let response = client
        .post(&url)
        .headers(build_headers(language, user_agent)?)
        .json(&body)
        .send()
        .await
        .map_err(|error| {
            if error.is_timeout() || error.is_connect() {
                LookupError::Network(describe_error(&error))
            } else {
                LookupError::Upstream(describe_error(&error))
            }
        })?;

    let status = response.status();
    if status == StatusCode::TOO_MANY_REQUESTS {
        // 有 `Retry-After` 就带上：Go 侧会拿它当冷却时长（有上限）。
        // 这个头也可能是 HTTP-date 形式，那种解析不了 —— 报 None，让 Go 侧
        // 用自己的退避曲线，别在这边猜。
        let retry_after = response
            .headers()
            .get(reqwest::header::RETRY_AFTER)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.trim().parse::<u64>().ok());
        return Err(LookupError::RateLimited(retry_after));
    }

    let text = response
        .text()
        .await
        .map_err(|error| LookupError::Upstream(format!("读取应答失败: {error}")))?;

    if !status.is_success() {
        let body = text.chars().take(200).collect::<String>();
        return Err(if status.is_server_error() {
            LookupError::Upstream(format!("HTTP {status}: {body}"))
        } else {
            LookupError::Rejected(format!("HTTP {status}: {body}"))
        });
    }

    let value: Value = serde_json::from_str(&text)
        .map_err(|error| LookupError::Upstream(format!("应答不是 JSON: {error}")))?;

    Ok(parse_track(&value))
}

/// 连通性探测用的地址。
///
/// 与识别同一个 host、同一条路径，只是不带签名。用同一个 host 是有意的：
/// 探测要回答的是「我这台机器现在能不能问到 Shazam」，换成 example.com
/// 之类的靶子只会证明「能上外网」，证明不了代理真的把流量送到了这里。
const PROBE_URL: &str = "https://amp.shazam.com/discovery/v5/en-US/GB/iphone/-/tag/";

/// 探测一次到 Shazam 的连通性。
///
/// 返回 `Ok(status)` 表示**拿到了 HTTP 应答**——哪怕是 4xx。这条路径不带签名，
/// 被回 404 是正常的（实测），它已经证明 DNS、代理、TLS、TCP 四段全通了。
/// `Err` 表示连应答都没拿到，那才是网络层的问题。
///
/// 故意用 GET 而不是 POST：探测要尽量轻，也不该在服务端留下一个看起来像
/// 「识别请求」的东西。
pub async fn probe(client: &reqwest::Client, user_agent: &str) -> Result<u16, LookupError> {
    let response = client
        .get(PROBE_URL)
        .headers(build_headers("en-US", user_agent)?)
        .send()
        .await
        .map_err(|error| {
            if error.is_timeout() || error.is_connect() {
                LookupError::Network(describe_error(&error))
            } else {
                LookupError::Upstream(describe_error(&error))
            }
        })?;
    Ok(response.status().as_u16())
}

/// 从应答里取出曲目信息。`matches` 为空或没有 `track` 就是没匹配上。
fn parse_track(value: &Value) -> Track {
    let has_match = value
        .get("matches")
        .and_then(Value::as_array)
        .is_some_and(|matches| !matches.is_empty());

    let Some(track) = value.get("track") else {
        return Track::default();
    };
    if !has_match {
        return Track::default();
    }

    let mut parsed = Track {
        matched: true,
        title: str_field(track, &["title"]),
        // 上游的 `subtitle` 就是艺人名。
        artist: str_field(track, &["subtitle"]),
        cover_art: track
            .get("images")
            .map(|images| str_field(images, &["coverarthq", "coverart"]))
            .unwrap_or_default(),
        shazam_url: str_field(track, &["url"]),
        track_key: str_field(track, &["key"]),
        ..Track::default()
    };

    // SONG 段落里的元数据是「标签 -> 值」的数组，标签大小写不固定。
    if let Some(sections) = track.get("sections").and_then(Value::as_array) {
        for section in sections {
            if section.get("type").and_then(Value::as_str) != Some("SONG") {
                continue;
            }
            let Some(metadata) = section.get("metadata").and_then(Value::as_array) else {
                continue;
            };
            for entry in metadata {
                let label = str_field(entry, &["title"]).to_lowercase();
                let text = str_field(entry, &["text"]);
                if text.is_empty() {
                    continue;
                }
                match label.as_str() {
                    "album" if parsed.album.is_empty() => parsed.album = text,
                    "genre" if parsed.genre.is_empty() => parsed.genre = text,
                    "released" if parsed.released.is_empty() => parsed.released = text,
                    "label" if parsed.label.is_empty() => parsed.label = text,
                    // 有些应答把艺人放在 SONG 段里，而顶层 subtitle 是空的。
                    "artist" if parsed.artist.is_empty() => parsed.artist = text,
                    _ => {}
                }
            }
        }
    }

    parsed
}

fn str_field(value: &Value, keys: &[&str]) -> String {
    keys.iter()
        .find_map(|key| value.get(*key).and_then(Value::as_str))
        .unwrap_or_default()
        .trim()
        .to_string()
}
