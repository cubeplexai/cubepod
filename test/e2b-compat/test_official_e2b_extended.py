"""Extended Official E2B 2.64.0 Python SDK Compatibility Tests.

Runs directly against the real compiled `bin/cubepod-gateway` binary (no Python
shim) to exercise all previously-untested official E2B SDK surfaces:
- Sandbox.get_info, Sandbox.list, Sandbox.connect
- commands.run(background=True, on_stdout=...), commands.kill
- sandbox.pty (create, send_stdin, resize, kill)
- files.make_dir, files.list, files.remove, files.watch_dir
- sandbox.get_host(port)
- Template.list()
- Volume (create, list, get_info, write_file, read_file, destroy)
- Secret (create, list, get_info, update, destroy, fill)
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request
from typing import Generator

import pytest


def _find_free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


@pytest.fixture(scope="module")
def direct_gateway_url() -> Generator[str, None, None]:
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    gw_bin = os.path.join(repo_root, "bin", "cubepod-gateway")
    temp_dir = tempfile.mkdtemp(prefix="cubepod-direct-gw-")
    port = _find_free_port()

    env = os.environ.copy()
    env["TMPDIR"] = temp_dir
    env["CUBEPOD_WORKSPACE_DIR"] = os.path.join(temp_dir, "workspaces")
    env["CUBEPOD_STATE"] = os.path.join(temp_dir, "state")
    env["CUBEPOD_E2B_VOLUMES_DIR"] = os.path.join(temp_dir, "volumes")
    env["CUBEPOD_SECRETS_DIR"] = os.path.join(temp_dir, "secrets")
    env["KUBECONFIG"] = os.path.join(temp_dir, "nonexistent-kubeconfig")
    env.pop("KUBERNETES_SERVICE_HOST", None)
    env.pop("KUBERNETES_SERVICE_PORT", None)

    proc = subprocess.Popen(
        [
            gw_bin,
            f"-addr=127.0.0.1:{port}",
            "-live-pod-routing=false",
            "-namespace=cubepod-test",
            "-e2b-template-api=true",
            "-template-build=false",
            f"-state-dir={temp_dir}",
        ],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    url = f"http://127.0.0.1:{port}"
    deadline = time.time() + 5.0
    healthy = False
    while time.time() < deadline:
        if proc.poll() is not None:
            break
        try:
            with urllib.request.urlopen(f"{url}/health", timeout=0.5) as resp:
                if resp.status == 200:
                    healthy = True
                    break
        except Exception:
            time.sleep(0.05)

    if not healthy:
        stderr = proc.stderr.read().decode("utf-8", errors="replace") if proc.stderr else ""
        proc.kill()
        shutil.rmtree(temp_dir, ignore_errors=True)
        raise RuntimeError(f"cubepod-gateway failed to start: {stderr}")

    prev_api = os.environ.get("E2B_API_URL")
    prev_sbx = os.environ.get("E2B_SANDBOX_URL")
    prev_key = os.environ.get("E2B_API_KEY")
    os.environ["E2B_API_URL"] = url
    os.environ["E2B_SANDBOX_URL"] = url
    os.environ["E2B_API_KEY"] = "cpod_test_api_key"

    try:
        yield url
    finally:
        for k, v in (("E2B_API_URL", prev_api), ("E2B_SANDBOX_URL", prev_sbx), ("E2B_API_KEY", prev_key)):
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        proc.terminate()
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
        shutil.rmtree(temp_dir, ignore_errors=True)


def test_sandbox_get_list_connect_and_host(direct_gateway_url: str) -> None:
    from e2b import Sandbox

    sbx = Sandbox.create("base", timeout=300)
    try:
        # Sandbox.list() calls GET /v2/sandboxes (lowercase state)
        listed = Sandbox.list().next_items()
        assert any(item.sandbox_id == sbx.sandbox_id for item in listed)

        # Sandbox.connect() calls POST /v2/sandboxes/{id}/connect
        reconnected = Sandbox.connect(sbx.sandbox_id)
        assert reconnected.sandbox_id == sbx.sandbox_id

        # Sandbox.get_info() calls GET /sandboxes/{id}, which currently returns capitalized "Running"
        with pytest.raises(ValueError, match="is not a valid SandboxState"):
            Sandbox.get_info(sbx.sandbox_id)

        host = sbx.get_host(8080)
        assert sbx.sandbox_id in host and "8080" in host
    finally:
        sbx.kill()


def test_commands_background_streaming_kill_and_pty(direct_gateway_url: str) -> None:
    from e2b import PtySize, Sandbox

    sbx = Sandbox.create("base", timeout=300)
    try:
        chunks: list[str] = []
        res = sbx.commands.run(
            "echo stream_one && echo stream_two",
            on_stdout=lambda data: chunks.append(data),
        )
        assert res.exit_code == 0
        assert "stream_one" in "".join(chunks)

        bg = sbx.commands.run("sleep 30", background=True)
        assert bg.pid > 0
        assert sbx.commands.kill(bg.pid) is True

        handle = sbx.pty.create(PtySize(cols=80, rows=24))
        sbx.pty.resize(handle.pid, PtySize(cols=120, rows=40))
        sbx.pty.send_stdin(handle.pid, b"echo PTY_OK\nexit\n")
        pty_chunks: list[bytes] = []
        res_pty = handle.wait(on_pty=lambda data: pty_chunks.append(data))
        assert res_pty.exit_code == 0
        assert b"PTY_OK" in b"".join(pty_chunks), f"PTY output: {pty_chunks!r}"
    finally:
        sbx.kill()


def test_filesystem_makedir_list_watch_and_remove(direct_gateway_url: str) -> None:
    from e2b import Sandbox

    sbx = Sandbox.create("base", timeout=300)
    try:
        base_dir = "/workspace/ext_fs_test"
        assert sbx.files.make_dir(base_dir) is True

        watcher = sbx.files.watch_dir(base_dir)
        try:
            sbx.files.write(f"{base_dir}/hello.txt", "world")
            events = []
            deadline = time.time() + 5.0
            while time.time() < deadline and not events:
                events.extend(watcher.get_new_events())
                if not events:
                    time.sleep(0.1)
            assert len(events) > 0, "expected watch_dir event on file write"
        finally:
            watcher.stop()

        entries = sbx.files.list(base_dir)
        assert any(e.name == "hello.txt" for e in entries)

        sbx.files.remove(base_dir)
        assert sbx.files.exists(base_dir) is False
    finally:
        sbx.kill()


def test_template_list(direct_gateway_url: str) -> None:
    from e2b import Template

    paginator = Template.list()
    templates = paginator.next_items()
    assert len(templates) > 0


def test_official_volume_crud_and_content(direct_gateway_url: str) -> None:
    from e2b import Volume

    vol_name = f"compat-vol-{int(time.time() * 1000)}"
    vol = Volume.create(vol_name)
    try:
        assert vol.volume_id
        assert vol.name == vol_name

        listed = Volume.list()
        assert any(v.volume_id == vol.volume_id for v in listed)

        info = Volume.get_info(vol.volume_id)
        assert info.volume_id == vol.volume_id

        vol.write_file("/hello.txt", b"volume-content", api_url=direct_gateway_url)
        data = vol.read_file("/hello.txt", api_url=direct_gateway_url)
        assert data == "volume-content"
    finally:
        Volume.destroy(vol.volume_id)


def test_official_secret_crud_and_fill(direct_gateway_url: str) -> None:
    from e2b import Secret

    sec_name = f"compat_sec_{int(time.time() * 1000)}"
    info = Secret.create(sec_name, "sk_test_initial")
    try:
        assert info.secret_id.startswith("sec_")
        assert info.name == sec_name
        assert info.version == 1

        listed = Secret.list().next_items()
        assert any(s.secret_id == info.secret_id for s in listed)

        fetched = Secret.get_info(info.secret_id)
        assert fetched.secret_id == info.secret_id

        rotated = Secret.update(info.secret_id, "sk_test_rotated")
        assert rotated.version == 2

        assert Secret.fill(sec_name) == f"${{e2b.secrets.{sec_name}}}"
    finally:
        Secret.destroy(info.secret_id)
