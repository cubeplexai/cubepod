"""Official E2B Python Sync SDK Compatibility Test Suite conforming to CEP-0018.

Verifies drop-in interoperability of official upstream `e2b.Sandbox`:
- Sandbox.create("base", timeout=300, envs={"TEST": "1"})
- commands.run("echo 'hello'")
- files.write("/workspace/test.txt", "data")
- files.read("/workspace/test.txt")
- refreshes / timeout keepalive
- kill()
"""

from __future__ import annotations

import os
import sys

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from mock_server import MockE2BGateway


def test_official_e2b_sync_lifecycle(gateway_url: str | None = None) -> None:
    """Verify official e2b.Sandbox lifecycle, execution, and file persistence."""
    from e2b import Sandbox

    # If running standalone without pytest fixture, start an ephemeral mock gateway
    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        # 1. Create sandbox with template, timeout, and environment variables
        sandbox = Sandbox.create("base", timeout=300, envs={"TEST": "1"})
        assert sandbox.sandbox_id, "sandbox_id must be non-empty"

        # 2. Execute command
        cmd = sandbox.commands.run("echo 'hello'")
        assert cmd.exit_code == 0, f"expected exit_code 0, got {cmd.exit_code}"
        assert "hello" in cmd.stdout, f"expected 'hello' in stdout, got {cmd.stdout}"

        # 3. Write file
        write_info = sandbox.files.write("/workspace/test.txt", "data")
        assert write_info.path == "/workspace/test.txt"

        # 4. Read file
        content = sandbox.files.read("/workspace/test.txt")
        assert content == "data", f"expected 'data', got {content!r}"

        # 5. Heartbeat / Timeout keepalive
        sandbox.set_timeout(600)

        # 6. Kill sandbox
        sandbox.kill()
    finally:
        if gw:
            gw.stop()


def test_official_e2b_sync_multiple_commands(gateway_url: str | None = None) -> None:
    """Verify consecutive command executions and environment inspection."""
    from e2b import Sandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        sandbox = Sandbox.create("base", timeout=300)
        try:
            cmd1 = sandbox.commands.run("echo 'e2b drop-in works'")
            assert cmd1.exit_code == 0
            assert "e2b drop-in works" in cmd1.stdout

            cmd2 = sandbox.commands.run("echo 'second command'")
            assert cmd2.exit_code == 0
            assert "second command" in cmd2.stdout
        finally:
            sandbox.kill()
    finally:
        if gw:
            gw.stop()


def test_official_e2b_sync_binary_filesystem(gateway_url: str | None = None) -> None:
    """Verify binary file write and read operations."""
    from e2b import Sandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        sandbox = Sandbox.create("base", timeout=300)
        try:
            binary_payload = b"\x00\x01\x02\x03\xff\xfe\xfd"
            sandbox.files.write("/workspace/binary.bin", binary_payload)
            read_bytes = sandbox.files.read("/workspace/binary.bin", format="bytes")
            assert bytes(read_bytes) == binary_payload
        finally:
            sandbox.kill()
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_official_e2b_sync standalone...")
    test_official_e2b_sync_lifecycle()
    test_official_e2b_sync_multiple_commands()
    test_official_e2b_sync_binary_filesystem()
    print("All sync E2B tests passed successfully!")
