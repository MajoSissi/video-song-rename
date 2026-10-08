#!/usr/bin/env python3
"""生成 assets/icon.ico —— 程序图标与托盘图标共用这一份。

没有用 PIL：这台机器上没装，而这个图标的形状简单到用距离场就能画出来。
自己算还能保证可复现——图标是二进制文件，改坏了在 diff 里看不出来，所以
生成脚本必须跟着一起进仓库。

用法：
    python tools/make_icon.py

输出：
    assets/icon.ico      多尺寸，给 exe 资源与托盘用
    frontend/icon.png    256px，给界面里的标题栏用（浏览器对 PNG 的支持最稳）
    assets/icon-256.png  预览用，方便肉眼看效果

ICO 里每个尺寸都写成 32bpp 的 DIB（BITMAPINFOHEADER + BGRA + AND 掩码），
不是 PNG 压缩。Windows 只在 256 这一档保证支持 PNG 压缩的图标项，
小尺寸用 PNG 在某些壳里会画不出来——托盘恰恰只用小尺寸。
"""

from __future__ import annotations

import pathlib
import struct
import zlib

# 配色跟界面保持一致：--accent #3b82f6，logo 渐变 #2563eb → #1d4ed8。
GRAD_TOP = (0x3B, 0x82, 0xF6)
GRAD_BOTTOM = (0x1D, 0x4E, 0xD8)
NOTE_COLOR = (0xFF, 0xFF, 0xFF)

SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256]
SUPERSAMPLE = 4  # 每边放大 4 倍再缩小，靠这个做抗锯齿


# ------------------------------------------------------------------ 几何

def in_rounded_rect(x: float, y: float, radius: float) -> bool:
    """点是否落在 0..1 的圆角方块里。"""
    cx = min(max(x, radius), 1 - radius)
    cy = min(max(y, radius), 1 - radius)
    dx, dy = x - cx, y - cy
    return dx * dx + dy * dy <= radius * radius


def in_rotated_ellipse(x: float, y: float, cx: float, cy: float,
                       rx: float, ry: float, angle_deg: float) -> bool:
    """点是否落在旋转过的椭圆里。角度是弧度制的度数，顺时针为正。"""
    import math
    a = math.radians(angle_deg)
    dx, dy = x - cx, y - cy
    # 把点反向旋转回椭圆坐标系
    ux = dx * math.cos(a) + dy * math.sin(a)
    uy = -dx * math.sin(a) + dy * math.cos(a)
    return (ux / rx) ** 2 + (uy / ry) ** 2 <= 1.0


def in_stem(x: float, y: float) -> bool:
    return 0.503 <= x <= 0.579 and 0.243 <= y <= 0.742


def in_flag(x: float, y: float) -> bool:
    """符尾：沿一条二次贝塞尔铺一串半径递减的圆，凑出一条飘带。"""
    import math
    # 起点在符干顶端，终点甩到右下方
    p0 = (0.541, 0.276)
    p1 = (0.812, 0.262)
    p2 = (0.734, 0.523)
    for step in range(26):
        t = step / 25
        mt = 1 - t
        bx = mt * mt * p0[0] + 2 * mt * t * p1[0] + t * t * p2[0]
        by = mt * mt * p0[1] + 2 * mt * t * p1[1] + t * t * p2[1]
        radius = 0.074 * (1 - t) + 0.030 * t
        dx, dy = x - bx, y - by
        if dx * dx + dy * dy <= radius * radius:
            return True
    return False


def in_note(x: float, y: float) -> bool:
    return (
        in_rotated_ellipse(x, y, 0.383, 0.726, 0.163, 0.126, -18)
        or in_stem(x, y)
        or in_flag(x, y)
    )


# ------------------------------------------------------------------ 绘制

def render(size: int) -> list[list[tuple[int, int, int, int]]]:
    """画一张 size×size 的 RGBA 图，返回逐行的像素。"""
    big = size * SUPERSAMPLE
    radius = 0.215

    # 先在放大后的网格上算覆盖率
    rows: list[list[tuple[int, int, int, int]]] = []
    for py in range(size):
        row = []
        for px in range(size):
            note_hits = 0
            shape_hits = 0
            for sy in range(SUPERSAMPLE):
                for sx in range(SUPERSAMPLE):
                    x = (px * SUPERSAMPLE + sx + 0.5) / big
                    y = (py * SUPERSAMPLE + sy + 0.5) / big
                    if not in_rounded_rect(x, y, radius):
                        continue
                    shape_hits += 1
                    if in_note(x, y):
                        note_hits += 1

            total = SUPERSAMPLE * SUPERSAMPLE
            coverage = shape_hits / total
            if coverage <= 0:
                row.append((0, 0, 0, 0))
                continue

            # 底色：沿对角线做渐变
            t = (px / max(size - 1, 1) + py / max(size - 1, 1)) / 2
            base = tuple(
                round(GRAD_TOP[i] + (GRAD_BOTTOM[i] - GRAD_TOP[i]) * t) for i in range(3)
            )

            # 音符盖在底色上，按覆盖率混
            note_alpha = note_hits / shape_hits
            color = tuple(
                round(base[i] + (NOTE_COLOR[i] - base[i]) * note_alpha) for i in range(3)
            )
            row.append((color[0], color[1], color[2], round(coverage * 255)))
        rows.append(row)
    return rows


# ------------------------------------------------------------------ 编码

def encode_dib(rows: list[list[tuple[int, int, int, int]]]) -> bytes:
    """编成一个 ICO 图标项：BITMAPINFOHEADER + BGRA 倒序位图 + AND 掩码。"""
    size = len(rows)
    header = struct.pack(
        "<IiiHHIIiiII",
        40,          # biSize
        size,        # biWidth
        size * 2,    # biHeight：XOR 与 AND 两张图叠起来，所以是两倍
        1,           # biPlanes
        32,          # biBitCount
        0,           # biCompression = BI_RGB
        0,           # biSizeImage（BI_RGB 可以留 0）
        0, 0, 0, 0,  # 分辨率与调色板
    )

    # DIB 的行是自下而上的，而且通道顺序是 BGRA
    xor_bitmap = bytearray()
    for y in range(size - 1, -1, -1):
        for (r, g, b, a) in rows[y]:
            xor_bitmap += bytes((b, g, r, a))

    # AND 掩码：1 表示透明。32bpp 下这张图其实用不到，但格式要求它必须在。
    # 每行按 4 字节对齐。
    row_bytes = ((size + 31) // 32) * 4
    and_bitmap = bytearray()
    for y in range(size - 1, -1, -1):
        bits = bytearray(row_bytes)
        for x in range(size):
            if rows[y][x][3] == 0:
                bits[x // 8] |= 0x80 >> (x % 8)
        and_bitmap += bits

    return header + bytes(xor_bitmap) + bytes(and_bitmap)


def encode_ico(entries: list[tuple[int, bytes]]) -> bytes:
    count = len(entries)
    out = struct.pack("<HHH", 0, 1, count)  # reserved, type=icon, count

    offset = 6 + 16 * count
    directory = bytearray()
    for size, blob in entries:
        dimension = 0 if size >= 256 else size  # 0 表示 256
        directory += struct.pack(
            "<BBBBHHII",
            dimension, dimension,
            0, 0,        # 调色板颜色数、保留位
            1, 32,       # 色彩平面、位深
            len(blob), offset,
        )
        offset += len(blob)

    return out + bytes(directory) + b"".join(blob for _, blob in entries)


def encode_png(rows: list[list[tuple[int, int, int, int]]]) -> bytes:
    """顺手写一张 PNG 预览。只用标准库的 zlib。"""
    height = len(rows)
    width = len(rows[0]) if height else 0
    raw = bytearray()
    for row in rows:
        raw.append(0)  # 过滤器：None
        for (r, g, b, a) in row:
            raw += bytes((r, g, b, a))

    def chunk(tag: bytes, payload: bytes) -> bytes:
        body = tag + payload
        return struct.pack(">I", len(payload)) + body + struct.pack(">I", zlib.crc32(body))

    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


def main() -> int:
    root = pathlib.Path(__file__).resolve().parents[1]
    assets = root / "assets"
    assets.mkdir(exist_ok=True)

    entries: list[tuple[int, bytes]] = []
    for size in SIZES:
        rows = render(size)
        entries.append((size, encode_dib(rows)))
        print(f"  {size}x{size}  {len(entries[-1][1])} 字节")

    icon_path = assets / "icon.ico"
    icon_path.write_bytes(encode_ico(entries))

    preview = render(256)
    (assets / "icon-256.png").write_bytes(encode_png(preview))

    # 界面里那个标题栏图标也用它。frontend/ 整个被 //go:embed 编进 exe，
    # 所以放这里就等于「内嵌一份给界面用的图标」，运行时不需要外部文件。
    frontend = root / "frontend"
    frontend.mkdir(exist_ok=True)
    (frontend / "icon.png").write_bytes(encode_png(preview))

    print(f"\n写入 {icon_path}  ({icon_path.stat().st_size} 字节，{len(entries)} 个尺寸)")
    print(f"写入 {assets / 'icon-256.png'}")
    print(f"写入 {frontend / 'icon.png'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
