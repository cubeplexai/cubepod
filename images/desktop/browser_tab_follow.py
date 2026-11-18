#!/usr/bin/env python3
"""CubePod / CubePlex Browser Tab-Follow Helper (CEP-0021).

Keeps the live desktop/WebRTC view focused on the Chromium tab currently driven
by ``agent-browser`` over CDP (``http://127.0.0.1:${CUBEPOD_CDP_PORT:-9222}``).

Installed at ``/opt/cubepod/desktop/browser_tab_follow.py`` and symlinked at:
  - ``/usr/local/bin/cubeplex-focus-browser-tab``
  - ``/usr/local/bin/cubepod-focus-browser-tab``
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
from typing import Any
import urllib.error
import urllib.parse
import urllib.request

WRAPPER_TEMPLATE = r"""#!/bin/sh
# CUBEPLEX_TAB_FOLLOW=1
REAL="${AGENT_BROWSER_REAL:-__REAL_DEFAULT__}"
if [ "${AGENT_BROWSER_WRAPPER:-}" = "1" ]; then
    exec "$REAL" "$@"
fi
export AGENT_BROWSER_WRAPPER=1
export AGENT_BROWSER_REAL="$REAL"
"$REAL" "$@"
status=$?
case "${1:-}" in
    --help|-h|help|install|upgrade|skills) exit "$status" ;;
esac
if [ "$status" -eq 0 ]; then
    "${CUBEPOD_FOCUS_TAB_BIN:-__FOCUS_DEFAULT__}" >/dev/null 2>&1 || true
fi
exit "$status"
"""


def _cdp_base_url(cdp_port: int | None = None) -> str:
    if cdp_port is None:
        raw = os.environ.get("CUBEPOD_CDP_PORT", "9222").strip()
        try:
            cdp_port = int(raw)
        except ValueError:
            cdp_port = 9222
    return f"http://127.0.0.1:{cdp_port}"


def list_pages(cdp_port: int | None = None) -> list[dict[str, Any]]:
    base = _cdp_base_url(cdp_port)
    with urllib.request.urlopen(f"{base}/json", timeout=2) as resp:
        data = json.loads(resp.read().decode("utf-8", errors="replace"))
    if not isinstance(data, list):
        return []
    return [p for p in data if isinstance(p, dict) and p.get("type") == "page"]


def _resolve_real_agent_browser() -> str:
    explicit = os.environ.get("AGENT_BROWSER_REAL", "").strip()
    if explicit:
        return explicit
    for candidate in (
        "/usr/local/bin/agent-browser.real",
        "/usr/bin/agent-browser",
    ):
        if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    found = shutil.which("agent-browser")
    return found or "/usr/bin/agent-browser"


def current_url(real_bin: str) -> str | None:
    env = os.environ.copy()
    env["AGENT_BROWSER_WRAPPER"] = "1"
    try:
        out = subprocess.run(
            [real_bin, "get", "url"],
            capture_output=True,
            text=True,
            timeout=5,
            env=env,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if out.returncode != 0:
        return None
    lines = [ln.strip() for ln in out.stdout.splitlines() if ln.strip()]
    return lines[-1] if lines else None


def _norm(u: str | None) -> str:
    text = str(u or "").strip()
    if not text:
        return ""
    return text.rstrip("/") or text


def choose_target_id(pages: list[dict[str, Any]], url: str | None) -> str | None:
    """Pick the CDP page target id matching ``url`` (exact normalized match only)."""
    wanted = _norm(url)
    if not pages or not wanted:
        return None
    exact = [
        p
        for p in pages
        if isinstance(p, dict)
        and p.get("type", "page") == "page"
        and _norm(p.get("url") if p.get("url") is not None else "") == wanted
    ]
    if not exact:
        return None
    tid = exact[0].get("id")
    return str(tid) if tid else None


def activate(target_id: str, cdp_port: int | None = None) -> None:
    base = _cdp_base_url(cdp_port)
    encoded_id = urllib.parse.quote(str(target_id), safe="")
    with urllib.request.urlopen(f"{base}/json/activate/{encoded_id}", timeout=2) as resp:
        resp.read()


def _write_executable(path: Path, content: str) -> None:
    if path.is_symlink():
        path.unlink()
    path.write_text(content, encoding="utf-8")
    mode = path.stat().st_mode
    path.chmod(mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def install_wrapper(
    agent_browser_path: str | None = None,
    focus_bin_path: str | None = None,
    force: bool = False,
) -> int:
    wrap_str = (
        agent_browser_path
        or os.environ.get("CUBEPOD_AGENT_BROWSER_BIN", "").strip()
        or "/usr/local/bin/agent-browser"
    )
    focus_str = (
        focus_bin_path
        or os.environ.get("CUBEPOD_FOCUS_TAB_BIN", "").strip()
        or "/usr/local/bin/cubeplex-focus-browser-tab"
    )
    wrap_path = Path(wrap_str)
    real_path = Path(f"{wrap_path}.real")
    focus_path = Path(focus_str)

    usr_bin_fallback = Path("/usr/bin/agent-browser")
    has_candidate = (
        wrap_path.exists()
        or wrap_path.is_symlink()
        or real_path.exists()
        or (wrap_str == "/usr/local/bin/agent-browser" and usr_bin_fallback.exists())
        or force
    )
    if not has_candidate:
        return 0

    wrap_path.parent.mkdir(parents=True, exist_ok=True)

    if wrap_path.exists() or wrap_path.is_symlink():
        if wrap_path.exists():
            raw = wrap_path.read_bytes()
            if b"CUBEPLEX_TAB_FOLLOW=1" not in raw:
                if real_path.is_symlink() or real_path.exists():
                    real_path.unlink()
                wrap_path.replace(real_path)
        else:
            wrap_path.unlink()

    if real_path.exists():
        real_default = str(real_path)
    elif usr_bin_fallback.exists():
        real_default = str(usr_bin_fallback)
    else:
        real_default = str(real_path)

    self_path = Path(__file__).resolve()
    if not focus_path.exists():
        if focus_path.parent.exists() and os.access(focus_path.parent, os.W_OK):
            _write_executable(focus_path, self_path.read_text(encoding="utf-8"))
        else:
            focus_path = self_path

    wrapper_body = WRAPPER_TEMPLATE.replace("__REAL_DEFAULT__", real_default).replace(
        "__FOCUS_DEFAULT__", str(focus_path)
    )
    _write_executable(wrap_path, wrapper_body)
    return 0


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Focus the Chromium CDP tab matching agent-browser's active URL."
    )
    parser.add_argument(
        "--url",
        default=None,
        help="Explicit URL to focus (defaults to querying `$AGENT_BROWSER_REAL get url`).",
    )
    parser.add_argument(
        "--cdp-port",
        type=int,
        default=None,
        help="Override loopback CDP port (defaults to $CUBEPOD_CDP_PORT or 9222).",
    )
    parser.add_argument(
        "--install-wrapper",
        action="store_true",
        help="Idempotently wrap /usr/local/bin/agent-browser with CUBEPLEX_TAB_FOLLOW=1.",
    )
    parser.add_argument(
        "--agent-browser-path",
        default=None,
        help="Override agent-browser binary path when using --install-wrapper.",
    )
    parser.add_argument(
        "--focus-bin",
        default=None,
        help="Override focus helper path embedded by --install-wrapper.",
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="Write wrapper even if agent-browser is not yet installed.",
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if args.install_wrapper:
        return install_wrapper(
            agent_browser_path=args.agent_browser_path,
            focus_bin_path=args.focus_bin,
            force=args.force,
        )

    target_url = args.url
    if target_url is None:
        target_url = current_url(_resolve_real_agent_browser())
    if not _norm(target_url):
        return 0

    try:
        pages = list_pages(args.cdp_port)
    except (urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError, ValueError):
        return 0

    tid = choose_target_id(pages, target_url)
    if not tid:
        return 0

    try:
        activate(tid, args.cdp_port)
    except (urllib.error.URLError, TimeoutError, OSError, ValueError):
        return 0
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
