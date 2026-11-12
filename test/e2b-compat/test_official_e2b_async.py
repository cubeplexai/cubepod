"""Official E2B Python Async SDK Compatibility Test Suite conforming to CEP-0018.

Verifies drop-in interoperability of official upstream `e2b.AsyncSandbox`:
- await AsyncSandbox.create("base", timeout=300, envs={"TEST": "1"})
- await commands.run("echo 'hello'")
- await files.write("/workspace/test.txt", "data")
- await files.read("/workspace/test.txt")
- refreshes / timeout keepalive
- await kill()
"""

from __future__ import annotations

import asyncio
import os
import sys
try:
    import pytest
    async_test = pytest.mark.asyncio
except ImportError:
    def async_test(f):
        return f

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from mock_server import MockE2BGateway


@async_test
async def test_official_e2b_async_lifecycle(gateway_url: str | None = None) -> None:
    """Verify official e2b.AsyncSandbox lifecycle, asynchronous execution, and file persistence."""
    from e2b import AsyncSandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        # 1. Asynchronously create sandbox
        sandbox = await AsyncSandbox.create("base", timeout=300, envs={"TEST": "1"})
        assert sandbox.sandbox_id, "sandbox_id must be non-empty"

        # 2. Asynchronously run command
        cmd = await sandbox.commands.run("echo 'hello'")
        assert cmd.exit_code == 0, f"expected exit_code 0, got {cmd.exit_code}"
        assert "hello" in cmd.stdout, f"expected 'hello' in stdout, got {cmd.stdout}"

        # 3. Asynchronously write file
        write_info = await sandbox.files.write("/workspace/test.txt", "data")
        assert write_info.path == "/workspace/test.txt"

        # 4. Asynchronously read file
        content = await sandbox.files.read("/workspace/test.txt")
        assert content == "data", f"expected 'data', got {content!r}"

        # 5. Heartbeat / Timeout keepalive
        await sandbox.set_timeout(600)

        # 6. Asynchronously kill sandbox
        await sandbox.kill()
    finally:
        if gw:
            gw.stop()


@async_test
async def test_official_e2b_async_concurrent_operations(
    gateway_url: str | None = None,
) -> None:
    """Verify concurrent command executions and file operations over AsyncSandbox."""
    from e2b import AsyncSandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        sandbox = await AsyncSandbox.create("base", timeout=300)
        try:
            # Run concurrent writes
            await asyncio.gather(
                sandbox.files.write("/workspace/f1.txt", "content1"),
                sandbox.files.write("/workspace/f2.txt", "content2"),
            )

            # Run concurrent reads
            r1, r2 = await asyncio.gather(
                sandbox.files.read("/workspace/f1.txt"),
                sandbox.files.read("/workspace/f2.txt"),
            )
            assert r1 == "content1"
            assert r2 == "content2"

            # Execute command
            cmd = await sandbox.commands.run("echo 'async batch complete'")
            assert cmd.exit_code == 0
            assert "async batch complete" in cmd.stdout
        finally:
            await sandbox.kill()
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_official_e2b_async standalone...")
    asyncio.run(test_official_e2b_async_lifecycle())
    asyncio.run(test_official_e2b_async_concurrent_operations())
    print("All async E2B tests passed successfully!")
