"""Keep the live desktop / Neko WebRTC view on the tab ``agent-browser`` drives.

Neko and ``x11vnc`` stream the active Chromium X11 window rather than individual
CDP targets. When ``agent-browser`` operates a background tab over CDP, the
human viewer would otherwise remain on an older tab. After each CLI command, the
installed wrapper activates the matching ``"type": "page"`` target via CDP
``http://127.0.0.1:${CUBEPOD_CDP_PORT:-9222}/json/activate/<id>``.

The helper is pre-installed in ``images/desktop`` at
``/opt/cubepod/desktop/browser_tab_follow.py`` (symlinked to
``/usr/local/bin/cubeplex-focus-browser-tab`` and
``/usr/local/bin/cubepod-focus-browser-tab``), and can also be injected into
already-built sandboxes at runtime via :func:`tab_follow_install_command`.
"""

from __future__ import annotations

import base64
import json
import shlex
from typing import Any

FOCUS_SCRIPT = r"""#!/usr/bin/env python3
import argparse
import json
import os
import shutil
import subprocess
import urllib.error
import urllib.parse
import urllib.request


def _cdp_base(port_override=None):
    if port_override is not None:
        port = int(port_override)
    else:
        raw = os.environ.get("CUBEPOD_CDP_PORT", "9222").strip()
        try:
            port = int(raw)
        except ValueError:
            port = 9222
    return f"http://127.0.0.1:{port}"


def list_pages(cdp_port=None):
    base = _cdp_base(cdp_port)
    with urllib.request.urlopen(f"{base}/json", timeout=2) as resp:
        data = json.loads(resp.read().decode("utf-8", errors="replace"))
    if not isinstance(data, list):
        return []
    return [p for p in data if isinstance(p, dict) and p.get("type") == "page"]


def _resolve_real_bin():
    explicit = os.environ.get("AGENT_BROWSER_REAL", "").strip()
    if explicit:
        return explicit
    for candidate in ("/usr/local/bin/agent-browser.real", "/usr/bin/agent-browser"):
        if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    return shutil.which("agent-browser") or "/usr/bin/agent-browser"


def current_url(real_bin):
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


def _norm(u):
    u = str(u or "").strip()
    if not u:
        return ""
    return u.rstrip("/") or u


def choose_target_id(pages, url):
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


def activate(target_id, cdp_port=None):
    base = _cdp_base(cdp_port)
    encoded = urllib.parse.quote(str(target_id), safe="")
    with urllib.request.urlopen(f"{base}/json/activate/{encoded}", timeout=2) as resp:
        resp.read()


def main(argv=None):
    parser = argparse.ArgumentParser(add_help=True)
    parser.add_argument("--url", default=None)
    parser.add_argument("--cdp-port", type=int, default=None)
    args = parser.parse_args(argv)

    target_url = args.url if args.url is not None else current_url(_resolve_real_bin())
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
"""

WRAPPER_SCRIPT = r"""#!/bin/sh
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
    "${CUBEPOD_FOCUS_TAB_BIN:-/usr/local/bin/cubeplex-focus-browser-tab}" >/dev/null 2>&1 || true
fi
exit "$status"
"""

_INSTALLER = r"""
import os
import stat
from pathlib import Path
import json, base64
files = json.loads(base64.b64decode("@@PAYLOAD@@").decode())
focus_path = Path(os.environ.get("CUBEPOD_FOCUS_TAB_BIN", "/usr/local/bin/cubeplex-focus-browser-tab"))
wrap_path = Path(os.environ.get("CUBEPOD_AGENT_BROWSER_BIN", "/usr/local/bin/agent-browser"))
real_path = Path(f"{wrap_path}.real")
desktop_helper = Path("/opt/cubepod/desktop/browser_tab_follow.py")

def write_exec(path, body):
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.is_symlink():
        path.unlink()
    path.write_text(body, encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)

if wrap_path.exists() or wrap_path.is_symlink():
    if wrap_path.exists():
        raw = wrap_path.read_bytes()
        if b"CUBEPLEX_TAB_FOLLOW=1" not in raw:
            # Always keep the current real CLI as .real, even if an older copy exists.
            if real_path.is_symlink() or real_path.exists():
                real_path.unlink()
            wrap_path.replace(real_path)
    else:
        wrap_path.unlink()

real = str(real_path)
if not Path(real).exists():
    real = "/usr/bin/agent-browser"

wrapper = files["wrapper"].replace("__REAL_DEFAULT__", real)
if not (focus_path.is_symlink() and focus_path.exists() and focus_path.resolve() == desktop_helper):
    write_exec(focus_path, files["focus"])
write_exec(wrap_path, wrapper)
"""


def _normalize_url(url: str | None) -> str:
    text = str(url or "").strip()
    if not text:
        return ""
    return text.rstrip("/") or text


def choose_target_id(pages: list[dict[str, Any]], url: str | None) -> str | None:
    """Pick the CDP target id whose URL matches the agent's current page.

    Exact match only (trailing slash ignored). Prefix matching would activate
    ``https://ex.com`` when the agent is on ``https://ex.com/login``.
    """
    wanted = _normalize_url(url)
    if not pages or not wanted:
        return None
    exact = [
        p
        for p in pages
        if isinstance(p, dict)
        and p.get("type", "page") == "page"
        and _normalize_url(p.get("url") if p.get("url") is not None else "") == wanted
    ]
    if not exact:
        return None
    tid = exact[0].get("id")
    return str(tid) if tid else None


def tab_follow_install_command() -> str:
    """Shell command (root) that writes the tab-follow helper & wrapper into a sandbox."""
    payload = base64.b64encode(
        json.dumps({"focus": FOCUS_SCRIPT, "wrapper": WRAPPER_SCRIPT}).encode("utf-8")
    ).decode("ascii")
    src = _INSTALLER.replace("@@PAYLOAD@@", payload)
    return "python3 -c " + shlex.quote(src)
