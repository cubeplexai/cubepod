"""Official Python SDK against the real kind deployment; no fallback or silent skips.
Scenarios derive from test/e2b-compat/test_official_e2b_extended.py.

On Kata Firecracker only, failures caused by issue #120 (virtio-mmio hotplug EIO)
or issue #122 (E2B volume mounts do not persist) are reported as documented skips
and must be re-enabled when those issues are fixed. kind, Compose and Cloud
Hypervisor still fail closed on any skip.
"""
import os
import time
import urllib.request
import uuid
from urllib.parse import urlparse

import pytest
from e2b import CommandExitException, PtySize, Sandbox, Volume

# Never let a direct pytest invocation fall back to paid E2B Cloud credentials.
_url = os.environ.get("E2B_API_URL", "")
_parsed = urlparse(_url)
if (_parsed.scheme != "http" or _parsed.hostname != "127.0.0.1"
        or not _parsed.port or os.environ.get("E2B_SANDBOX_URL") != _url
        or os.environ.get("E2B_VOLUME_API_URL") != _url
        or os.environ.get("E2B_API_KEY") != "cpod_kind_test_only"):
    raise RuntimeError("Use the kind runner: all SDK URLs must be its loopback endpoint and dummy key")

_FC_ISSUE_120 = "https://github.com/cubeplexai/cubepod/issues/120"
_FC_ISSUE_122 = "https://github.com/cubeplexai/cubepod/issues/122"
_ISSUE_120_MARKERS = (
    "failed to mount /dev/vdb",
    "EIO: I/O error",
    "EIO",
    "failed to ensure Pod",
    "timed out waiting for pod",
)


def _is_firecracker():
    return os.environ.get("KATA_VMM") == "fc"


def _exception_text(exc):
    parts = []
    seen = set()
    current = exc
    while current is not None and id(current) not in seen:
        seen.add(id(current))
        parts.append(str(current))
        if doc := getattr(current, "doc", None):
            parts.append(str(doc))
        if (response := getattr(current, "response", None)) is not None:
            try:
                parts.append(getattr(response, "text", "") or "")
            except Exception:
                pass
        current = current.__cause__ or current.__context__
    return "\n".join(parts)


def _skip_fc_issue_120(exc):
    if _is_firecracker() and any(marker in _exception_text(exc) for marker in _ISSUE_120_MARKERS):
        pytest.skip(f"{_FC_ISSUE_120}: Firecracker virtio-mmio hotplug race: {exc}")


@pytest.fixture
def sandbox():
    try:
        sbx = Sandbox.create("kind-e2e", timeout=120, request_timeout=90)
    except Exception as exc:
        _skip_fc_issue_120(exc)
        raise
    try:
        yield sbx
    finally:
        assert sbx.kill() is True


def test_lifecycle_commands_connect(sandbox):
    sbx = Sandbox.connect(sandbox.sandbox_id)
    assert sbx.commands.run("hostname").stdout.strip() == sbx.sandbox_id
    if expected := os.environ.get("E2E_GUEST_KERNEL"):
        assert sbx.commands.run("uname -r").stdout.strip() == expected
    result = sbx.commands.run("printf python-kind; printf err >&2")
    assert result.exit_code == 0
    assert result.stdout == "python-kind"
    assert result.stderr == "err"
    with pytest.raises(CommandExitException) as exc:
        sbx.commands.run("exit 42")
    assert exc.value.exit_code == 42
    assert sbx.get_info().state == "running"


def test_files_and_watch(sandbox):
    directory = "/workspace/python-files"
    assert sandbox.files.make_dir(directory) is True
    watcher = sandbox.files.watch_dir(directory)
    try:
        sandbox.files.write(f"{directory}/hello.txt", "python-kind")
        deadline = time.monotonic() + 10
        events = []
        while time.monotonic() < deadline and not events:
            events.extend(watcher.get_new_events())
            if not events:
                time.sleep(0.1)
        assert events, "watch_dir did not observe a real file write"
    finally:
        watcher.stop()
    assert sandbox.files.read(f"{directory}/hello.txt") == "python-kind"
    assert any(entry.name == "hello.txt" for entry in sandbox.files.list(directory))


def test_background_and_pty(sandbox):
    bg = sandbox.commands.run("sleep 60", background=True)
    assert bg.pid > 0
    assert sandbox.commands.kill(bg.pid) is True
    handle = sandbox.pty.create(PtySize(cols=80, rows=24))
    sandbox.pty.resize(handle.pid, PtySize(cols=120, rows=40))
    sandbox.pty.send_stdin(handle.pid, b"echo PYTHON_PTY_OK; exit\n")
    chunks = []
    result = handle.wait(on_pty=chunks.append)
    assert result.exit_code == 0
    assert b"PYTHON_PTY_OK" in b"".join(chunks)


def test_network_and_service_port(sandbox):
    result = sandbox.commands.run("curl --fail --max-time 10 http://network-target.cubepod-kind.svc:8080/hostname")
    assert result.exit_code == 0
    assert "network-target-" in result.stdout
    sandbox.files.write("/workspace/probe", "PYTHON_PORT_OK")
    server = sandbox.commands.run("python3 -u -m http.server 8080 --directory /workspace", background=True)
    try:
        deadline = time.monotonic() + 10
        response = None
        while time.monotonic() < deadline:
            try:
                req = urllib.request.Request(os.environ["E2B_API_URL"] + "/probe", headers={"Host": sandbox.get_host(8080)})
                with urllib.request.urlopen(req, timeout=2) as r:
                    response = r.read().decode()
                break
            except OSError:
                time.sleep(0.1)
        assert response == "PYTHON_PORT_OK"
    finally:
        sandbox.commands.kill(server.pid)


@pytest.mark.skipif(
    os.environ.get("KATA_VMM") == "fc",
    reason=f"{_FC_ISSUE_122}: E2B filesystem volume mounts do not persist on Kata Firecracker",
)
def test_volume_mount_persistence():
    volume = Volume.create("kind-python-" + uuid.uuid4().hex)
    try:
        for expected in (None, "persisted-python"):
            try:
                sbx = Sandbox.create("kind-e2e", timeout=120, request_timeout=90, volume_mounts={"/data": volume})
            except Exception as exc:
                _skip_fc_issue_120(exc)
                raise
            try:
                if expected:
                    assert sbx.files.read("/data/persist.txt") == expected
                else:
                    sbx.files.write("/data/persist.txt", "persisted-python")
                    assert sbx.commands.run("cat /data/persist.txt").stdout == "persisted-python"
            finally:
                assert sbx.kill() is True
    finally:
        Volume.destroy(volume.volume_id)
