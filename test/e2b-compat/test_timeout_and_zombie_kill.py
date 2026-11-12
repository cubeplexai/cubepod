"""Timeout Enforcement and Zombie Process Cleanup Test Suite conforming to CEP-0018.

Verifies:
1. Infinite loop execution (while True: pass) with timeout=2s raises TimeoutException.
2. The underlying guest/host OS process is cleanly terminated via SIGTERM/SIGKILL
   without leaving orphaned zombie processes spinning in the background.
3. The sandbox remains healthy and receptive to subsequent executions after a killed process.
4. Asynchronous timeout enforcement over AsyncSandbox.
"""

from __future__ import annotations

import os
import subprocess
import sys
import time
import pytest

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from live_gateway import LiveGateway


def test_infinite_loop_timeout_and_zombie_kill(gateway_url: str | None = None) -> None:
    """Verify timeout=2s terminates infinite loop and kills OS process tree."""
    from e2b_code_interpreter import Sandbox
    from e2b.exceptions import TimeoutException

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        marker = f"cpod_zombie_kill_target_{int(time.time() * 1000)}"

        with Sandbox.create() as sandbox:
            code = f"""
# {marker}
import time

while True:
    time.sleep(0.01)
"""
            t_start = time.time()
            timed_out = False

            try:
                sandbox.run_code(code, timeout=2)
            except TimeoutException:
                timed_out = True
            except Exception as e:
                # Upstream exceptions format as TimeoutException or contain timeout in message
                if "timeout" in str(e).lower() or "TimeoutException" in type(e).__name__:
                    timed_out = True
                else:
                    raise

            duration = time.time() - t_start
            assert timed_out, "Expected TimeoutException on infinite loop execution"
            assert 1.5 <= duration <= 5.0, f"Expected timeout around 2s, took {duration:.2f}s"

            # Allow brief moment for process group kill to register in OS kernel
            time.sleep(0.3)

            # Check process table: ensure no process with the marker is still running
            res = subprocess.run(["pgrep", "-f", marker], capture_output=True, text=True)
            assert res.returncode != 0, (
                f"Zombie process detected! Process with marker {marker} is still alive: PID {res.stdout.strip()}"
            )

            # Verify sandbox remains completely healthy and responsive
            exec_after = sandbox.run_code("print('recovered_successfully')")
            assert exec_after.error is None
            assert "recovered_successfully" in "".join(exec_after.logs.stdout)
    finally:
        if gw:
            gw.stop()


def test_cpu_spinning_infinite_loop_kill(gateway_url: str | None = None) -> None:
    """Verify pure CPU-bound busy loop (while True: pass) without sleep is killed on timeout."""
    from e2b_code_interpreter import Sandbox
    from e2b.exceptions import TimeoutException

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        marker = f"cpod_busy_loop_target_{int(time.time() * 1000)}"

        with Sandbox.create() as sandbox:
            code = f"""
# {marker}
while True:
    pass
"""
            timed_out = False
            try:
                sandbox.run_code(code, timeout=2)
            except TimeoutException:
                timed_out = True
            except Exception as e:
                if "timeout" in str(e).lower() or "TimeoutException" in type(e).__name__:
                    timed_out = True
                else:
                    raise

            assert timed_out, "Expected TimeoutException on busy infinite loop"

            time.sleep(0.3)
            res = subprocess.run(["pgrep", "-f", marker], capture_output=True, text=True)
            assert res.returncode != 0, f"Busy loop process PID {res.stdout.strip()} was not killed"
    finally:
        if gw:
            gw.stop()


@pytest.mark.asyncio
async def test_async_sandbox_timeout_and_kill(gateway_url: str | None = None) -> None:
    """Verify asynchronous code interpreter timeout and zombie kill."""
    from e2b_code_interpreter import AsyncSandbox
    from e2b.exceptions import TimeoutException

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        marker = f"cpod_async_zombie_target_{int(time.time() * 1000)}"

        sandbox = await AsyncSandbox.create()
        try:
            code = f"""
# {marker}
import time
while True:
    time.sleep(0.01)
"""
            timed_out = False
            try:
                await sandbox.run_code(code, timeout=2)
            except TimeoutException:
                timed_out = True
            except Exception as e:
                if "timeout" in str(e).lower() or "TimeoutException" in type(e).__name__:
                    timed_out = True
                else:
                    raise

            assert timed_out, "Expected TimeoutException on async infinite loop"

            time.sleep(0.3)
            res = subprocess.run(["pgrep", "-f", marker], capture_output=True, text=True)
            assert res.returncode != 0, f"Async zombie process still alive: {res.stdout.strip()}"
        finally:
            await sandbox.kill()
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_timeout_and_zombie_kill standalone...")
    test_infinite_loop_timeout_and_zombie_kill()
    test_cpu_spinning_infinite_loop_kill()
    import asyncio
    asyncio.run(test_async_sandbox_timeout_and_kill())
    print("All timeout and zombie kill tests passed successfully!")
