#!/usr/bin/env python3
"""解析 exe 的 PE 资源目录，验证图标与版本资源真的进去了。

为什么要自己写一遍：`go build` 成功、exe 体积也涨了，都不代表资源是**对的**。
踩过一次——手写的 VS_VERSIONINFO 结构有误，资源确实被链接进去了，
但 Windows 读出来全是空的，而构建过程一声不吭。

用法：
    python tools/pe_resources.py dist/video-song-rename.exe assets/icon.ico

输出：资源清单、版本信息字段、以及 exe 里的图标与源 .ico 的逐帧比对。
"""

from __future__ import annotations

import pathlib
import struct
import sys

RT_ICON = 3
RT_GROUP_ICON = 14
RT_VERSION = 16

RESOURCE_NAMES = {
    1: "RT_CURSOR", 2: "RT_BITMAP", 3: "RT_ICON", 4: "RT_MENU", 5: "RT_DIALOG",
    6: "RT_STRING", 7: "RT_FONTDIR", 8: "RT_FONT", 9: "RT_ACCELERATOR",
    10: "RT_RCDATA", 11: "RT_MESSAGETABLE", 12: "RT_GROUP_CURSOR",
    14: "RT_GROUP_ICON", 16: "RT_VERSION", 17: "RT_DLGINCLUDE", 19: "RT_PLUGPLAY",
    20: "RT_VXD", 21: "RT_ANICURSOR", 22: "RT_ANIICON", 23: "RT_HTML", 24: "RT_MANIFEST",
}


class Pe:
    def __init__(self, data: bytes) -> None:
        self.data = data
        if data[:2] != b"MZ":
            raise ValueError("不是 PE 文件（没有 MZ 头）")
        pe_offset = struct.unpack_from("<I", data, 0x3C)[0]
        if data[pe_offset:pe_offset + 4] != b"PE\0\0":
            raise ValueError("不是 PE 文件（没有 PE 签名）")

        coff = pe_offset + 4
        self.section_count = struct.unpack_from("<H", data, coff + 2)[0]
        optional_size = struct.unpack_from("<H", data, coff + 16)[0]
        self.optional_at = coff + 20
        self.sections_at = self.optional_at + optional_size

        magic = struct.unpack_from("<H", data, self.optional_at)[0]
        # 数据目录的起始偏移：PE32+ 在可选头 +112，PE32 在 +96。
        # （可选头里 NumberOfRvaAndSizes 的位置也随位数变，别照抄一个常量。）
        if magic == 0x20B:
            self.image_base = struct.unpack_from("<Q", data, self.optional_at + 24)[0]
            dirs_at = self.optional_at + 112
        else:
            self.image_base = struct.unpack_from("<I", data, self.optional_at + 28)[0]
            dirs_at = self.optional_at + 96

        # 资源目录是数据目录的第 3 项（索引 2）
        self.resource_rva, self.resource_size = struct.unpack_from("<II", data, dirs_at + 2 * 8)

    def sections(self):
        for index in range(self.section_count):
            at = self.sections_at + index * 40
            name = self.data[at:at + 8].rstrip(b"\0").decode("ascii", "replace")
            virtual_size, virtual_address, raw_size, raw_pointer = struct.unpack_from("<IIII", self.data, at + 8)
            yield name, virtual_address, virtual_size, raw_pointer, raw_size

    def rva_to_offset(self, rva: int) -> int:
        for _name, va, vsize, raw_pointer, raw_size in self.sections():
            if va <= rva < va + max(vsize, raw_size):
                return raw_pointer + (rva - va)
        raise ValueError(f"RVA {rva:#x} 不在任何段里")


def walk_resources(pe: Pe):
    """走一遍 类型 → 名称 → 语言 → 数据 四层。

    每一层的偏移都是相对**资源目录起点**的，最高位 1 表示指向子目录。
    只屏蔽顶层的话，第二层会拿到一个天文数字偏移——这是最容易写错的地方。
    """
    base = pe.rva_to_offset(pe.resource_rva)
    found: dict[int, list[tuple[int, int, bytes]]] = {}

    def read_dir(offset: int):
        at = base + offset
        named, ids = struct.unpack_from("<HH", pe.data, at + 12)
        entries = []
        for index in range(named + ids):
            entry_at = at + 16 + index * 8
            name_id, child = struct.unpack_from("<II", pe.data, entry_at)
            entries.append((name_id, child & 0x7FFFFFFF, bool(child & 0x80000000)))
        return entries

    for type_id, type_child, type_is_dir in read_dir(0):
        if not type_is_dir:
            continue
        for name_id, name_child, name_is_dir in read_dir(type_child):
            if not name_is_dir:
                continue
            for lang_id, data_child, lang_is_dir in read_dir(name_child):
                if lang_is_dir:
                    continue
                entry_at = base + data_child
                data_rva, size, _codepage, _reserved = struct.unpack_from("<IIII", pe.data, entry_at)
                blob_at = pe.rva_to_offset(data_rva)
                blob = pe.data[blob_at:blob_at + size]
                found.setdefault(type_id, []).append((name_id, lang_id, blob))
    return found


def decode_version(blob: bytes) -> dict[str, str]:
    """解 VS_VERSIONINFO，把 StringFileInfo 里的键值对抠出来。"""
    def align4(value: int) -> int:
        return (value + 3) & ~3

    def read_node(at: int, limit: int):
        if at + 6 > limit:
            return None
        length, value_length, value_type = struct.unpack_from("<HHH", blob, at)
        if length == 0 or at + length > limit:
            return None
        # szKey 是 UTF-16LE、NUL 结尾
        cursor = at + 6
        while cursor + 1 < at + length and blob[cursor:cursor + 2] != b"\0\0":
            cursor += 2
        key = blob[at + 6:cursor].decode("utf-16-le", "replace")
        cursor = align4(cursor + 2)

        value_at = cursor
        if value_type == 1:  # 字符串值
            end = value_at
            while end + 1 < at + length and blob[end:end + 2] != b"\0\0":
                end += 2
            value = blob[value_at:end].decode("utf-16-le", "replace")
            children_at = align4(end + 2)
        else:
            value = ""
            children_at = align4(value_at + value_length)

        children = []
        cursor = children_at
        while cursor + 6 <= at + length:
            child = read_node(cursor, at + length)
            if child is None or child["length"] == 0:
                break
            children.append(child)
            cursor = align4(cursor + child["length"])
        return {"key": key, "value": value, "length": length, "children": children}

    root = read_node(0, len(blob))
    if root is None:
        return {"__error__": "VS_VERSIONINFO 解析失败"}

    out: dict[str, str] = {"__root__": root["key"], "__blob__": f"{len(blob)} 字节"}
    for child in root["children"]:
        if child["key"] != "StringFileInfo":
            continue
        for table in child["children"]:
            out["__langtable__"] = table["key"]
            for item in table["children"]:
                out[item["key"]] = item["value"]
    return out


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2

    exe_path = pathlib.Path(sys.argv[1])
    ico_path = pathlib.Path(sys.argv[2]) if len(sys.argv) > 2 else None

    pe = Pe(exe_path.read_bytes())
    resources = walk_resources(pe)

    print(f"文件：{exe_path}")
    print(f"段：  " + "、".join(name for name, *_ in pe.sections()))
    print()
    print("资源清单：")
    for type_id in sorted(resources):
        label = RESOURCE_NAMES.get(type_id, f"type {type_id}")
        print(f"  {label:16s} {len(resources[type_id])} 项")

    problems: list[str] = []

    # ---- 版本资源 ----
    print()
    if RT_VERSION not in resources:
        problems.append("没有 RT_VERSION")
        print("版本资源：❌ 不存在")
    else:
        _name, lang, blob = resources[RT_VERSION][0]
        info = decode_version(blob)
        print(f"版本资源：语言 {lang}，{len(blob)} 字节")
        if "__error__" in info:
            problems.append(info["__error__"])
            print("  ❌ " + info["__error__"])
        else:
            print(f"  根键 {info.get('__root__')}，字符串表 {info.get('__langtable__')}")
            for key in ("FileDescription", "ProductName", "FileVersion",
                        "ProductVersion", "InternalName", "OriginalFilename"):
                value = info.get(key)
                mark = "✅" if value else "❌"
                if not value:
                    problems.append(f"版本字段 {key} 是空的")
                print(f"    {mark} {key:18s} {value!r}")

    # ---- 图标 ----
    print()
    if RT_ICON not in resources or RT_GROUP_ICON not in resources:
        problems.append("缺 RT_ICON 或 RT_GROUP_ICON")
        print("图标资源：❌ 不完整")
    else:
        icons = sorted(resources[RT_ICON], key=lambda item: item[0])
        print(f"图标资源：RT_ICON {len(icons)} 档，RT_GROUP_ICON {len(resources[RT_GROUP_ICON])} 组")

        if ico_path and ico_path.is_file():
            raw = ico_path.read_bytes()
            count = struct.unpack_from("<H", raw, 4)[0]
            mismatches = []
            for index in range(count):
                entry = 6 + index * 16
                width = raw[entry] or 256
                size, offset = struct.unpack_from("<II", raw, entry + 8)
                source = raw[offset:offset + size]
                if index >= len(icons):
                    mismatches.append(f"{width}×{width} 在 exe 里找不到对应项")
                    continue
                if icons[index][2] != source:
                    mismatches.append(
                        f"{width}×{width} 不一致：源 {size} 字节，exe {len(icons[index][2])} 字节")
            if mismatches:
                problems.extend(mismatches)
                print("  ❌ 逐帧比对失败：")
                for item in mismatches:
                    print("     " + item)
            else:
                print(f"  ✅ 逐帧一致（{count} 档，尺寸/字节数全同）")

    print()
    if problems:
        print("结论：❌ 有 " + str(len(problems)) + " 个问题")
        for item in problems:
            print("   - " + item)
        return 1
    print("结论：✅ 资源齐全且正确")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
