//! Shazam 音频指纹，vendor 自 `shazamio-core` 1.2.0（MIT）。
//!
//! 上游只通过 `pyo3` 暴露这个模块，这里去掉的是三类与算法无关的东西：
//!
//! * `pyo3` 包装层（`params` / `response` / `errors`）——没有自己的逻辑。
//! * `libopus` 解码器——要编译 C，而本仓库的构建机没有 C 工具链。
//! * 测试模块——它们依赖没有一起 vendor 进来的音频样本。
//!
//! **源码取自 PyPI 的 sdist，不是 GitHub master。** 两者不是同一份实现：
//!  master 把解码换成了 `symphonia` + `rubato`，而 1.2.0 的轮子用 `rodio`。
//!  指纹的 DSP 部分（`do_fft` / `do_peak_spreading` / `do_peak_recognition`）
//!  两边逐字节相同，但**重采样出来的波形不同，峰值集就不同**。用 master 那份
//!  算出来的指纹与已发布的轮子不一致——而指纹只要错一位，Shazam 就永远匹配
//!  不上，且只会安静地返回空结果。所以这里跟 sdist 走。
//!
//! 许可与出处见 `third_party/shazamio-core/`。

pub mod algorithm;
pub mod communication;
pub mod ffmpeg_wrapper;
pub mod hanning;
pub mod signature_format;
