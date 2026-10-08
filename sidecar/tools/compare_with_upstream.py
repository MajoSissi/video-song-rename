#!/usr/bin/env python3
"""把本仓库的指纹与上游实现逐字节比对。

上游 `shazamio-core` 是 Rust 编译出来的扩展，本仓库的 `sidecar` 是把同一份
源码 vendor 过来、去掉 `pyo3` 与 `libopus` 之后自己编的。这个脚本是两者
「算出的是同一串字节」的唯一证据——指纹错一位，Shazam 就永远匹配不上，
而它只会安静地回一个空结果，不会报错。

用法：
    python compare_with_upstream.py <媒体文件> [--seconds 12]

依赖（只在开发机上装，不参与打包）：
    pip install shazamio-core

退出码：0 完全一致 / 1 不一致 / 2 环境问题
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_SIDECAR = REPO_ROOT / "sidecar" / "target" / "release" / "shazam-sidecar.exe"


def find_sidecar() -> pathlib.Path:
    override = os.environ.get("SHAZAM_SIDECAR")
    if override:
        return pathlib.Path(override)
    if DEFAULT_SIDECAR.is_file():
        return DEFAULT_SIDECAR
    found = shutil.which("shazam-sidecar") or shutil.which("shazam-sidecar.exe")
    if found:
        return pathlib.Path(found)
    raise SystemExit(f"找不到 sidecar，先跑 cargo build --release（找过 {DEFAULT_SIDECAR}）")


async def upstream_uri(media: pathlib.Path, seconds: int) -> str:
    try:
        from shazamio_core import Recognizer, SearchParams
    except ImportError:
        raise SystemExit("缺少 shazamio-core，先 pip install shazamio-core") from None

    recognizer = Recognizer(segment_duration_seconds=seconds)
    signature = await recognizer.recognize_path(
        value=str(media),
        options=SearchParams(segment_duration_seconds=seconds),
    )
    return signature.signature.uri


def local_uri(sidecar: pathlib.Path, media: pathlib.Path, seconds: int) -> str:
    with tempfile.TemporaryDirectory() as tmp:
        dump = pathlib.Path(tmp) / "signature.txt"
        completed = subprocess.run(
            [str(sidecar), "--file", str(media), "--seconds", str(seconds),
             "--dump-signature", str(dump)],
            capture_output=True,
            text=True,
            check=False,
        )
        if not dump.is_file():
            raise SystemExit(f"sidecar 没有产出签名（退出码 {completed.returncode}）\n"
                             f"stdout: {completed.stdout}\nstderr: {completed.stderr}")
        return dump.read_text(encoding="utf-8")


def digest(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:16]


def main() -> int:
    parser = argparse.ArgumentParser(description="与上游 shazamio-core 逐字节比对指纹")
    parser.add_argument("media", type=pathlib.Path, help="用来比对的媒体文件")
    parser.add_argument("--seconds", type=int, default=12, help="取样窗口，默认 12")
    args = parser.parse_args()

    if not args.media.is_file():
        raise SystemExit(f"文件不存在: {args.media}")

    sidecar = find_sidecar()
    reference = asyncio.run(upstream_uri(args.media, args.seconds))
    mine = local_uri(sidecar, args.media, args.seconds)

    same = reference == mine
    print(f"文件      : {args.media}")
    print(f"窗口      : {args.seconds}s")
    print(f"sidecar   : {sidecar}")
    print(f"上游长度  : {len(reference)} 字节  sha256[:16]={digest(reference)}")
    print(f"本仓库长度: {len(mine)} 字节  sha256[:16]={digest(mine)}")
    print(f"结论      : {'完全一致' if same else '不一致'}")

    if not same:
        for index, (left, right) in enumerate(zip(reference, mine)):
            if left != right:
                print(f"首个差异在第 {index} 个字符: 上游={left!r} 本仓库={right!r}")
                break
        else:
            print("前缀相同，长度不同")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
