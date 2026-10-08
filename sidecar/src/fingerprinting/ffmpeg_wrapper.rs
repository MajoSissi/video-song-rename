use std::error::Error;
use std::ffi::OsStr;
use std::fs::File;
use std::io::{BufReader, Write};
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

#[cfg(windows)]
use std::os::windows::process::CommandExt;

use rodio::Decoder;
use std::io::Cursor;
use std::process::Command;
use tempfile::Builder;

/// 用户在设置里指定的 ffmpeg 路径。
///
/// 设了就**只用它**，不再回退到 PATH——「我明明填了，它却还是用了别的」
/// 是个查不出来的问题。填错就直接报错，比悄悄用另一个更清楚。
static FFMPEG_OVERRIDE: OnceLock<PathBuf> = OnceLock::new();

/// 记录用户指定的 ffmpeg 路径。只能设一次（进程启动时）。
pub fn set_ffmpeg_override(path: Option<PathBuf>) {
    if let Some(value) = path {
        if value.as_os_str().is_empty() {
            return;
        }
        let _ = FFMPEG_OVERRIDE.set(value);
    }
}

/// 这个路径上的 ffmpeg 能不能跑。
fn can_run(path: &Path) -> bool {
    let mut command = Command::new(path);
    #[cfg(windows)]
    let command = command.creation_flags(0x08000000);
    let mut command = command;
    command.arg("-version");
    command
        .output()
        .map(|process| process.status.success())
        .unwrap_or(false)
}

/// 把 PATH 上的裸文件名展开成绝对路径。
///
/// 纯粹为了显示：`ffmpeg` 这个裸名字能让系统跑起来，但界面上告诉用户
/// 「用的是 ffmpeg」等于没说——他要知道到底用的是哪一个。
fn expand_on_path(name: &Path) -> Option<PathBuf> {
    let path_var = std::env::var_os("PATH")?;
    for dir in std::env::split_paths(&path_var) {
        if dir.as_os_str().is_empty() {
            continue;
        }
        let direct = dir.join(name);
        if direct.is_file() {
            return Some(direct);
        }
        // Windows 上 PATH 里的名字通常不带扩展名，要自己补一次。
        if name.extension().is_none() {
            let mut with_exe = direct.clone().into_os_string();
            with_exe.push(".exe");
            let with_exe = PathBuf::from(with_exe);
            if with_exe.is_file() {
                return Some(with_exe);
            }
        }
    }
    None
}

/// 找出这次真正会用的 ffmpeg。
///
/// 顺序：用户指定的 → PATH 上的 `ffmpeg` → 与 sidecar 同目录的 `ffmpeg.exe`。
/// 两条解码路径（按文件、按内存）共用这一个函数，避免两处各写一遍然后慢慢走偏。
pub fn resolve_ffmpeg() -> Option<PathBuf> {
    if let Some(explicit) = FFMPEG_OVERRIDE.get() {
        // 用户指定了就不再回退：填错就直接失败，别悄悄用另一个。
        return if can_run(explicit) {
            Some(explicit.clone())
        } else {
            None
        };
    }

    for candidate in [PathBuf::from("ffmpeg"), PathBuf::from("ffmpeg.exe")] {
        if can_run(&candidate) {
            return Some(expand_on_path(&candidate).unwrap_or(candidate));
        }
    }

    if let Ok(mut beside) = std::env::current_exe() {
        beside.pop();
        beside.push("ffmpeg.exe");
        if can_run(&beside) {
            return Some(beside);
        }
    }

    None
}

/// 报告当前实际会用的 ffmpeg 路径，给 `--env` 自检用。
pub fn resolved_ffmpeg_path() -> Option<PathBuf> {
    resolve_ffmpeg()
}

/// This function used to decode a file with FFMpeg, if it is installed on
/// the system, in the case where Rodio can't decode the concerned format
/// (for example with .WMA, .M4A, etc.).
pub fn decode_with_ffmpeg(file_path: &Path) -> Option<Decoder<BufReader<File>>> {
    // If FFMpeg is available, use it to convert the input file
    // from whichever format to a .WAV (because Rodio has its
    // decoding support limited to .WAV, .FLAC, .OGG, .MP3, which
    // makes that .MP4/.AAC, .OPUS or .WMA are not supported, and
    // Rodio's minimp3 .MP3 decoder seems to crash on Windows anyway)
    if let Some(ffmpeg_path) = resolve_ffmpeg() {
        // Create a sink file for FFMpeg

        let sink_file = Builder::new().suffix(".wav").tempfile().unwrap();

        let sink_file_path = sink_file.into_temp_path();
        // Try to convert the input video or audio file to a standard
        // .WAV s16le PCM file using FFMpeg, and pass it to Rodio
        // later in the case where it succeeded

        let mut command = Command::new(ffmpeg_path);

        #[cfg(windows)]
        let command = command.creation_flags(0x08000000);

        // One array, one element type: the two paths are `OsStr`, so the flags are
        //  spelled that way too rather than converting the paths to `&str`, which
        //  would fail on any path that is not valid UTF-8.
        let mut command = command;
        let command = command.args([
            OsStr::new("-y"),
            OsStr::new("-i"),
            file_path.as_os_str(),
            sink_file_path.as_os_str(),
        ]);

        // Set "CREATE_NO_WINDOW" on Windows, see
        // https://stackoverflow.com/a/60958956/662399

        if let Ok(process) = command.output() {
            if process.status.success() {
                let res = Decoder::new(BufReader::new(File::open(&sink_file_path).unwrap()))
                    .expect("failed to decode with ffmpeg");
                return Some(res);
            }
        } else {
            return None;
        }
    }
    None
}

pub fn decode_with_ffmpeg_from_bytes(
    bytes: &[u8],
) -> Result<Decoder<Cursor<Vec<u8>>>, Box<dyn Error>> {
    // Create a temporary file for the input
    let mut temp_file = Builder::new().suffix(".tmp").tempfile()?;
    temp_file.write_all(bytes)?;
    let file_path = temp_file.path().to_path_buf();

    if let Some(ffmpeg_path) = resolve_ffmpeg() {
        // Create a temporary file for the output
        let sink_file = Builder::new().suffix(".wav").tempfile()?;
        let sink_file_path = sink_file.path().to_path_buf();

        // Convert to WAV format
        let mut command = Command::new(ffmpeg_path);
        #[cfg(windows)]
        let command = command.creation_flags(0x08000000);
        // Same as above: one array, one element type, and no path forced through
        //  `&str`.
        let mut command = command;
        let command = command.args([
            OsStr::new("-y"),
            OsStr::new("-i"),
            file_path.as_os_str(),
            sink_file_path.as_os_str(),
        ]);

        if let Ok(process) = command.output() {
            if process.status.success() {
                // Read the converted file into a Vec<u8>
                let converted_bytes = std::fs::read(&sink_file_path)?;

                // Create a Cursor around the converted bytes and create a Decoder
                let cursor = Cursor::new(converted_bytes);
                let decoder = Decoder::new(cursor)?;

                return Ok(decoder);
            }
        }
    }
    Err("FFmpeg not found or failed to convert audio".into())
}
