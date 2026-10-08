#!/usr/bin/env bash
# 构建两个产物并组装到 dist/。
#
# 用法：
#   ./build.sh            发布构建（界面无控制台窗口）
#   ./build.sh --debug    调试构建（保留控制台，方便看 panic 与日志）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$ROOT/dist"
MODE="release"

if [[ "${1:-}" == "--debug" ]]; then
  MODE="debug"
fi

echo "[1/3] 构建识别核心（Rust sidecar，$MODE）..."
cd "$ROOT/sidecar"
if [[ "$MODE" == "release" ]]; then
  cargo build --release
  SIDECAR="$ROOT/sidecar/target/release/shazam-sidecar.exe"
else
  cargo build
  SIDECAR="$ROOT/sidecar/target/debug/shazam-sidecar.exe"
fi

cd "$ROOT"

# exe 的图标与版本资源。**这一步不能省**：`rsrc_windows_amd64.syso` 不在版本库里
# （见 .gitignore），新 clone 出来是没有的，直接 go build 会编出一个没有自定义图标的 exe，
# 而且不报任何错。
#
# 之所以敢不提交它：生成器是确定性的（重新生成的结果与当初手工跑的那份逐字节相同），
# 而它完全由 assets/icon.ico 决定 —— 放在仓库里只会变成「换了图标忘了重跑」的静默失效点。
echo "[2/3] 生成 exe 图标与版本资源（.syso）..."
go run tools/genrsrc.go

echo "[3/3] 构建界面（Go + Wails，$MODE）..."
mkdir -p "$OUT"

# `-tags production` 不是可选项。
#
# Wails v2 的 internal/app/app_default_windows.go 带约束
# `!dev && !production && !bindings`，不带标签时它提供一个空壳 Run()，
# 界面起来只会弹一个「will not build without the correct build tags」的框。
# 体积差也很明显：桩 5.4 MB，真身 12 MB。
#
# 调试构建用 `-tags dev`，会带上 DevTools（F12 打开）。
if [[ "$MODE" == "release" ]]; then
  go build -tags production -ldflags "-s -w -H windowsgui" -o "$OUT/video-song-rename.exe" .
else
  go build -tags dev -o "$OUT/video-song-rename.exe" .
fi

cp "$SIDECAR" "$OUT/"

echo
echo "完成，产物在 $OUT"
ls -la "$OUT"
echo
echo "把 dist/ 整个拷走即可运行。配置、日志、改名记录会写进它旁边的 data/ 目录。"
