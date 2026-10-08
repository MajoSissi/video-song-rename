"""只跑模板页那三个行为探针，用来做变异测试。

为什么单独开一个入口：完整套件要截十几张图加九个探针，跑一轮四十多秒；
而验证「断言真的会变红」需要连着改坏好几处、每处跑一次，
用完整套件做这件事太慢，慢到会让人不想做 —— 不想做的检查等于没有检查。

三个探针各跑一次页面加载：槽位编辑器那个会反复增删槽位，
右键菜单那个会把模板删到只剩一份，拖放那个只碰窗口级的遮罩 ——
放同一页里会互相干扰。

用法：
    python tools/probe_slots.py            # 跑一次，打印结论
    python tools/probe_slots.py --dom      # 顺带把探针的原始输出打出来
"""

import functools
import http.server
import pathlib
import re
import shutil
import sys
import threading

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import shoot_ui  # noqa: E402


def main() -> int:
    want_dom = "--dom" in sys.argv

    stage = pathlib.Path(shoot_ui.os.environ.get("TEMP", "/tmp")) / "vsr-slot-probe"
    site = stage / "site"
    profile = stage / "chrome-profile"
    site.mkdir(parents=True, exist_ok=True)

    shoot_ui.build_preview(site / "index.html")
    icon = shoot_ui.INDEX.parent / "icon.png"
    if icon.is_file():
        shutil.copyfile(icon, site / "icon.png")

    port = shoot_ui.free_port()
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(site))
    server = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    chrome = shoot_ui.find_chrome()
    base = f"http://127.0.0.1:{port}/index.html"

    cases = [
        ("槽位编辑器", f"{base}#templates+slotprobe", "data-slot-probe", shoot_ui.check_slots),
        ("模板右键菜单", f"{base}#templates+menuprobe", "data-menu-probe", shoot_ui.check_tpl_menu),
        ("拖放遮罩", f"{base}#tasks+dragprobe", "data-drag-probe", shoot_ui.check_drag),
    ]

    failures = 0
    for label, url, attr, check in cases:
        if want_dom:
            result = shoot_ui.run_chrome(
                chrome, profile, ["--dump-dom", f"--window-size={shoot_ui.WINDOW_SIZE}"], url
            )
            match = re.search(attr + r'="([^"]*)"', result.stdout or "")
            print(f"原始探针输出（{label}）：")
            print(match.group(1).replace("&quot;", '"') if match else f"(没有 {attr})")

        ok, reason = check(chrome, profile, url)
        if ok:
            print(f"✅ {label} 探针通过")
        else:
            print(f"❌ {label} 探针失败：{reason}")
            failures += 1

    server.shutdown()
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
