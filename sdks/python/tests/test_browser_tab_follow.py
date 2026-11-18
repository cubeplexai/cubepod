"""Unit and integration tests for CubePod / CubePlex browser tab-follow helper."""

from __future__ import annotations

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import threading
from typing import Any
import unittest

# Ensure sdks/python is on sys.path
REPO_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO_ROOT / "sdks" / "python"))

import cubepod
from cubepod.browser_tab_follow import (
    FOCUS_SCRIPT,
    WRAPPER_SCRIPT,
    choose_target_id,
    tab_follow_install_command,
)


class _MockCDPServer:
    def __init__(self, targets: list[dict[str, Any]]) -> None:
        self.targets = targets
        self.activated: list[str] = []
        self.json_hits = 0
        self._lock = threading.Lock()

        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802
                if self.path == "/json":
                    with outer._lock:
                        outer.json_hits += 1
                        payload = json.dumps(outer.targets).encode("utf-8")
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                    return

                prefix = "/json/activate/"
                if self.path.startswith(prefix):
                    tid = self.path[len(prefix) :]
                    with outer._lock:
                        outer.activated.append(tid)
                    body = b"Target activated"
                    self.send_response(200)
                    self.send_header("Content-Type", "text/plain")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return

                self.send_response(404)
                self.end_headers()

            def log_message(self, fmt: str, *args: Any) -> None:
                return

        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port: int = self._httpd.server_address[1]
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)

    def __enter__(self) -> _MockCDPServer:
        self._thread.start()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=2)


class TestBrowserTabFollow(unittest.TestCase):
    def test_top_level_exports(self) -> None:
        self.assertIs(cubepod.choose_target_id, choose_target_id)
        self.assertIs(cubepod.tab_follow_install_command, tab_follow_install_command)
        self.assertEqual(cubepod.FOCUS_SCRIPT, FOCUS_SCRIPT)
        self.assertEqual(cubepod.WRAPPER_SCRIPT, WRAPPER_SCRIPT)

    def test_choose_target_id_unique_url(self) -> None:
        pages = [
            {"id": "a", "type": "page", "url": "https://a.example/"},
            {"id": "b", "type": "page", "url": "https://b.example/"},
        ]
        self.assertEqual(choose_target_id(pages, "https://b.example/"), "b")

    def test_choose_target_id_prefers_exact_match_and_rejects_prefix(self) -> None:
        pages = [
            {"id": "home", "type": "page", "url": "https://ex.com"},
            {"id": "login", "type": "page", "url": "https://ex.com/login"},
        ]
        self.assertEqual(choose_target_id(pages, "https://ex.com/login"), "login")
        self.assertEqual(choose_target_id(pages, "https://ex.com"), "home")
        self.assertIsNone(choose_target_id(pages, "https://ex.com/other"))
        # Parent page alone must NEVER match child URL via prefix
        parent_only = [{"id": "home", "type": "page", "url": "https://ex.com"}]
        self.assertIsNone(choose_target_id(parent_only, "https://ex.com/login"))

    def test_choose_target_id_treats_trailing_slash_as_same_page(self) -> None:
        pages = [{"id": "home", "type": "page", "url": "https://ex.com/"}]
        self.assertEqual(choose_target_id(pages, "https://ex.com"), "home")
        pages_no_slash = [{"id": "home2", "type": "page", "url": "https://ex.com"}]
        self.assertEqual(choose_target_id(pages_no_slash, "https://ex.com/"), "home2")

    def test_choose_target_id_ignores_non_page_and_empty_urls(self) -> None:
        targets = [
            {"id": "sw", "type": "service_worker", "url": "https://ex.com/app"},
            {"id": "iframe", "type": "iframe", "url": "https://ex.com/app"},
            {"id": "blank", "type": "page", "url": ""},
            {"id": "none_url", "type": "page", "url": None},
        ]
        self.assertIsNone(choose_target_id(targets, "https://ex.com/app"))
        self.assertIsNone(choose_target_id(targets, "None"))
        self.assertIsNone(choose_target_id(targets, None))
        self.assertIsNone(choose_target_id([], "https://ex.com/app"))

    def test_install_command_embeds_wrapper_and_focus_helper(self) -> None:
        cmd = tab_follow_install_command()
        self.assertTrue(cmd.startswith("python3 -c "))
        self.assertIn("cubeplex-focus-browser-tab", cmd)
        self.assertIn("CUBEPLEX_TAB_FOLLOW=1", WRAPPER_SCRIPT)
        self.assertIn("/json/activate/", FOCUS_SCRIPT)
        self.assertIn("__REAL_DEFAULT__", WRAPPER_SCRIPT)
        self.assertIn("wrap_path.replace", cmd)
        self.assertIn("_norm", FOCUS_SCRIPT)

    def test_sdk_installer_and_recursion_guard_with_live_mock_cdp(self) -> None:
        targets = [
            {"id": "bg-sw", "type": "service_worker", "url": "https://ex.com/login"},
            {"id": "tab-home", "type": "page", "url": "https://ex.com/"},
            {"id": "tab-login", "type": "page", "url": "https://ex.com/login"},
        ]
        with tempfile.TemporaryDirectory() as tmpdir, _MockCDPServer(targets) as cdp:
            tmp = Path(tmpdir)
            wrap_bin = tmp / "agent-browser"
            focus_bin = tmp / "cubeplex-focus-browser-tab"
            state_file = tmp / "current_url.txt"
            calls_log = tmp / "calls.log"

            # Create a fake real `agent-browser` binary that ALSO tests recursion safety:
            # on `open <url>`, it re-invokes `wrap_bin ping-recursive` while AGENT_BROWSER_WRAPPER=1
            # to verify the wrapper passes straight through to `.real` without infinite recursion.
            real_script = f"""#!/usr/bin/env python3
import os, subprocess, sys
from pathlib import Path

calls = Path({str(calls_log)!r})
state = Path({str(state_file)!r})
args = sys.argv[1:]
with calls.open("a", encoding="utf-8") as fh:
    fh.write(" ".join(args) + "|wrapper=" + os.environ.get("AGENT_BROWSER_WRAPPER", "") + "\\n")

if args[:1] == ["open"] and len(args) >= 2:
    state.write_text(args[1], encoding="utf-8")
    # Trigger nested call through the wrapper path to test AGENT_BROWSER_WRAPPER=1 guard
    nested = subprocess.run([{str(wrap_bin)!r}, "ping-recursive"], check=False)
    sys.exit(nested.returncode)
if args == ["get", "url"]:
    if state.exists():
        print(state.read_text(encoding="utf-8").strip())
    sys.exit(0)
if args == ["fail-cmd"]:
    sys.exit(2)
sys.exit(0)
"""
            wrap_bin.write_text(real_script, encoding="utf-8")
            wrap_bin.chmod(wrap_bin.stat().st_mode | stat.S_IXUSR)

            env = os.environ.copy()
            env["CUBEPOD_AGENT_BROWSER_BIN"] = str(wrap_bin)
            env["CUBEPOD_FOCUS_TAB_BIN"] = str(focus_bin)
            env["CUBEPOD_CDP_PORT"] = str(cdp.port)
            env.pop("AGENT_BROWSER_WRAPPER", None)
            env.pop("AGENT_BROWSER_REAL", None)

            install_cmd = tab_follow_install_command()
            # Run installer twice to prove idempotency (.real preserved, never overwritten by wrapper)
            for _ in range(2):
                res = subprocess.run(["sh", "-c", install_cmd], env=env, check=True)
                self.assertEqual(res.returncode, 0)

            real_backup = Path(f"{wrap_bin}.real")
            self.assertTrue(real_backup.exists())
            self.assertNotIn("CUBEPLEX_TAB_FOLLOW=1", real_backup.read_text(encoding="utf-8"))
            self.assertIn("CUBEPLEX_TAB_FOLLOW=1", wrap_bin.read_text(encoding="utf-8"))

            # 1. `open https://ex.com/login/` -> exact match (trailing slash ignored) activates `tab-login`
            open_res = subprocess.run(
                [str(wrap_bin), "open", "https://ex.com/login/"],
                env=env,
                capture_output=True,
                text=True,
                check=True,
            )
            self.assertEqual(open_res.returncode, 0)
            self.assertEqual(cdp.activated, ["tab-login"])

            # Verify recursion guard prevented `ping-recursive` from re-running tab focus
            logged = calls_log.read_text(encoding="utf-8").splitlines()
            self.assertEqual(
                logged,
                [
                    "open https://ex.com/login/|wrapper=1",
                    "ping-recursive|wrapper=1",
                    "get url|wrapper=1",
                ],
            )

            # 2. `--help` and failing commands must NOT trigger CDP tab activation
            subprocess.run([str(wrap_bin), "--help"], env=env, check=True)
            fail_res = subprocess.run([str(wrap_bin), "fail-cmd"], env=env, check=False)
            self.assertEqual(fail_res.returncode, 2)
            self.assertEqual(cdp.activated, ["tab-login"])

            # 3. Prefix-only URL (`https://ex.com/settings`) must NOT match `https://ex.com/`
            subprocess.run([str(wrap_bin), "open", "https://ex.com/settings"], env=env, check=True)
            self.assertEqual(cdp.activated, ["tab-login"])

    def test_desktop_script_cli_and_idempotent_install_wrapper(self) -> None:
        desktop_script = REPO_ROOT / "images" / "desktop" / "browser_tab_follow.py"
        self.assertTrue(desktop_script.is_file())

        targets = [
            {"id": "p1", "type": "page", "url": "https://docs.example.org"},
            {"id": "p2", "type": "page", "url": "https://docs.example.org/guide"},
        ]
        with tempfile.TemporaryDirectory() as tmpdir, _MockCDPServer(targets) as cdp:
            tmp = Path(tmpdir)
            env = os.environ.copy()
            env["CUBEPOD_CDP_PORT"] = str(cdp.port)

            # Explicit --url activates exact target and rejects prefix
            subprocess.run(
                [sys.executable, str(desktop_script), "--url", "https://docs.example.org/guide/"],
                env=env,
                check=True,
            )
            self.assertEqual(cdp.activated, ["p2"])

            subprocess.run(
                [sys.executable, str(desktop_script), "--url", "https://docs.example.org/guide/sub"],
                env=env,
                check=True,
            )
            self.assertEqual(cdp.activated, ["p2"])

            # --install-wrapper on a missing binary is a safe no-op (does not create broken stub)
            missing_wrap = tmp / "no-such-bin" / "agent-browser"
            subprocess.run(
                [
                    sys.executable,
                    str(desktop_script),
                    "--install-wrapper",
                    "--agent-browser-path",
                    str(missing_wrap),
                ],
                env=env,
                check=True,
            )
            self.assertFalse(missing_wrap.exists())

            # --install-wrapper on an existing binary wraps it idempotently and routes end-to-end
            wrap_bin = tmp / "bin" / "agent-browser"
            focus_bin = tmp / "bin" / "cubeplex-focus-browser-tab"
            wrap_bin.parent.mkdir(parents=True, exist_ok=True)
            wrap_bin.write_text(
                "#!/bin/sh\nif [ \"$1\" = \"get\" ] && [ \"$2\" = \"url\" ]; then\n"
                "  echo https://docs.example.org\nfi\nexit 0\n",
                encoding="utf-8",
            )
            wrap_bin.chmod(0o755)

            for _ in range(2):
                subprocess.run(
                    [
                        sys.executable,
                        str(desktop_script),
                        "--install-wrapper",
                        "--agent-browser-path",
                        str(wrap_bin),
                        "--focus-bin",
                        str(focus_bin),
                    ],
                    env=env,
                    check=True,
                )

            self.assertTrue(Path(f"{wrap_bin}.real").is_file())
            self.assertTrue(focus_bin.is_file())
            self.assertIn("CUBEPLEX_TAB_FOLLOW=1", wrap_bin.read_text(encoding="utf-8"))

            subprocess.run([str(wrap_bin), "click", "#btn"], env=env, check=True)
            self.assertEqual(cdp.activated, ["p2", "p1"])

    def test_install_command_does_not_clobber_symlinked_target_or_broken_symlink(self) -> None:
        desktop_script = REPO_ROOT / "images" / "desktop" / "browser_tab_follow.py"
        original_desktop_content = desktop_script.read_text(encoding="utf-8")
        self.assertIn("--install-wrapper", original_desktop_content)

        with tempfile.TemporaryDirectory() as tmpdir:
            tmp = Path(tmpdir)
            # 1. Symlinked focus_bin -> copied desktop browser_tab_follow.py target must NOT clobber target
            opt_target = tmp / "opt" / "browser_tab_follow.py"
            opt_target.parent.mkdir(parents=True, exist_ok=True)
            opt_target.write_text(original_desktop_content, encoding="utf-8")
            opt_target.chmod(0o755)

            focus_link = tmp / "bin" / "cubeplex-focus-browser-tab"
            focus_link.parent.mkdir(parents=True, exist_ok=True)
            focus_link.symlink_to(opt_target)

            # 2. Broken symlink at wrap_path must be cleanly replaced without FileNotFoundError
            wrap_link = tmp / "bin" / "agent-browser"
            wrap_link.symlink_to(tmp / "nonexistent-dir" / "missing-agent-browser")
            self.assertTrue(wrap_link.is_symlink())
            self.assertFalse(wrap_link.exists())

            env = os.environ.copy()
            env["CUBEPOD_FOCUS_TAB_BIN"] = str(focus_link)
            env["CUBEPOD_AGENT_BROWSER_BIN"] = str(wrap_link)

            subprocess.run(["sh", "-c", tab_follow_install_command()], env=env, check=True)

            # Target file behind the symlink MUST remain 100% intact (retaining --install-wrapper CLI)
            self.assertEqual(opt_target.read_text(encoding="utf-8"), original_desktop_content)
            self.assertIn("--install-wrapper", opt_target.read_text(encoding="utf-8"))
            self.assertFalse(focus_link.is_symlink())
            self.assertTrue(focus_link.is_file())
            self.assertFalse(wrap_link.is_symlink())
            self.assertIn("CUBEPLEX_TAB_FOLLOW=1", wrap_link.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
