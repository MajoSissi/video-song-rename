"""只跑记录页那两个行为探针，用来做变异测试。

为什么单独开一个入口：完整套件要跑十几张截图加七个探针，一轮四十多秒；
而验证「断言真的会变红」需要连着改坏好几处、每处跑一次，
用完整套件做这件事太慢，慢到会让人不想做 —— 不想做的检查等于没有检查。

这里要跑两份页面，因为记录页的空状态有**两种**说法：
`index.html`（有记录）和 `histempty.html`（一条记录都没有）。
只跑前一份的话，「还没有改名记录」那句永远只是纸上的分支。

用法：
    python tools/probe_history.py            # 跑一次，打印结论
    python tools/probe_history.py --dom      # 顺带把探针的原始输出打出来
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

    stage = pathlib.Path(shoot_ui.os.environ.get("TEMP", "/tmp")) / "vsr-history-probe"
    site = stage / "site"
    profile = stage / "chrome-profile"
    site.mkdir(parents=True, exist_ok=True)

    shoot_ui.build_preview(site / "index.html")
    shoot_ui.build_preview(site / "histempty.html", history=[])
    icon = shoot_ui.INDEX.parent / "icon.png"
    if icon.is_file():
        shutil.copyfile(icon, site / "icon.png")

    port = shoot_ui.free_port()
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(site))
    server = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    chrome = shoot_ui.find_chrome()
    base = f"http://127.0.0.1:{port}"

    cases = [
        ("记录页", f"{base}/index.html#history+histprobe", shoot_ui.check_history),
        ("记录空态", f"{base}/histempty.html#history+histprobe+histempty",
         shoot_ui.check_history_empty),
    ]

    failures = 0
    for label, url, check in cases:
        if want_dom:
            result = shoot_ui.run_chrome(
                chrome, profile, ["--dump-dom", f"--window-size={shoot_ui.WINDOW_SIZE}"], url
            )
            match = re.search(r'data-hist-probe="([^"]*)"', result.stdout or "")
            print(f"原始探针输出（{label}）：")
            print(match.group(1).replace("&quot;", '"') if match else "(没有 data-hist-probe)")

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
