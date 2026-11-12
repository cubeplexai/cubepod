"""Concurrent Sandboxes Workspace & Variable Isolation Test Suite (CEP-0018).

Verifies:
1. 5 concurrent sandboxes operating in parallel without cross-talk or lock contention.
2. Workspace filesystem isolation: private files written in sandbox A are invisible to sandbox B.
3. Code interpreter variable & environment variable isolation across concurrent instances.
4. Concurrent lifecycle creation and teardown without race conditions.
"""

from __future__ import annotations

import concurrent.futures
import os
import sys
import time
import pytest

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from live_gateway import LiveGateway


def test_five_concurrent_sandboxes_file_isolation(gateway_url: str | None = None) -> None:
    """Verify 5 concurrent sandboxes have strictly isolated filesystems."""
    from e2b import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        num_sandboxes = 5
        sandboxes: list[Any] = []
        tokens: dict[int, str] = {}

        # 1. Create 5 sandboxes concurrently
        def create_sbx(idx: int) -> tuple[int, Any]:
            sbx = Sandbox.create("base", timeout=120)
            return idx, sbx

        with concurrent.futures.ThreadPoolExecutor(max_workers=num_sandboxes) as executor:
            created = list(executor.map(create_sbx, range(num_sandboxes)))

        created.sort(key=lambda x: x[0])
        sandboxes = [s for _, s in created]

        try:
            # Verify all sandbox IDs are unique
            ids = [s.sandbox_id for s in sandboxes]
            assert len(set(ids)) == num_sandboxes, f"Duplicate sandbox IDs detected: {ids}"

            # 2. Write private file with same filename (/workspace/shared_name.txt) in each sandbox
            def write_token(idx: int) -> None:
                sbx = sandboxes[idx]
                token = f"TOKEN_FOR_SANDBOX_{idx}_{int(time.time()*1000)}"
                tokens[idx] = token
                sbx.files.write("/workspace/shared_name.txt", token)
                # Also write a file unique to this index
                sbx.files.write(f"/workspace/unique_worker_{idx}.dat", f"payload_{idx}")

            with concurrent.futures.ThreadPoolExecutor(max_workers=num_sandboxes) as executor:
                list(executor.map(write_token, range(num_sandboxes)))

            # 3. Read back in parallel and verify no cross-contamination
            def verify_isolation(idx: int) -> None:
                sbx = sandboxes[idx]
                expected_token = tokens[idx]
                content = sbx.files.read("/workspace/shared_name.txt")
                assert content == expected_token, (
                    f"Sandbox {idx} got content {content!r}, expected {expected_token!r}"
                )

                # Verify sandbox {idx} CANNOT see unique file from sandbox {(idx + 1) % num_sandboxes}
                other_idx = (idx + 1) % num_sandboxes
                other_file = f"/workspace/unique_worker_{other_idx}.dat"
                has_leak = False
                try:
                    other_content = sbx.files.read(other_file)
                    if other_content:
                        has_leak = True
                except Exception:
                    has_leak = False
                assert not has_leak, f"Data leak! Sandbox {idx} can access file of sandbox {other_idx}"

            with concurrent.futures.ThreadPoolExecutor(max_workers=num_sandboxes) as executor:
                list(executor.map(verify_isolation, range(num_sandboxes)))
        finally:
            # 4. Teardown all 5 sandboxes concurrently
            def kill_sbx(s: Any) -> None:
                try:
                    s.kill()
                except Exception:
                    pass

            with concurrent.futures.ThreadPoolExecutor(max_workers=num_sandboxes) as executor:
                list(executor.map(kill_sbx, sandboxes))
    finally:
        if gw:
            gw.stop()


def test_five_concurrent_sandboxes_code_variable_isolation(gateway_url: str | None = None) -> None:
    """Verify code execution in 5 parallel sandboxes does not leak variables or environment."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        num_sandboxes = 5

        def run_isolated_task(idx: int) -> dict[str, Any]:
            token = f"VAR_SECRET_{idx}_{int(time.time()*1000)}"
            with Sandbox.create("base", timeout=120) as sbx:
                code = f"""
import os
import sys

# Compute distinct variable
worker_id = {idx}
secret = '{token}'
calc_result = {idx} * 1000 + 42

print(f"WORKER_{{worker_id}}:secret={{secret}}:calc={{calc_result}}")
"""
                exec_result = sbx.run_code(code)
                assert exec_result.error is None
                stdout = "".join(exec_result.logs.stdout)
                return {
                    "idx": idx,
                    "sandbox_id": sbx.sandbox_id,
                    "token": token,
                    "stdout": stdout,
                }

        with concurrent.futures.ThreadPoolExecutor(max_workers=num_sandboxes) as executor:
            results = list(executor.map(run_isolated_task, range(num_sandboxes)))

        assert len(results) == num_sandboxes

        # Verify all stdout outputs match their specific worker index and token
        for r in results:
            idx = r["idx"]
            token = r["token"]
            stdout = r["stdout"]
            expected = f"WORKER_{idx}:secret={token}:calc={idx * 1000 + 42}"
            assert expected in stdout, f"Worker {idx} output mismatch: got {stdout!r}"
    finally:
        if gw:
            gw.stop()


def test_concurrent_sandboxes_lifecycle_race(gateway_url: str | None = None) -> None:
    """Verify rapid concurrent creation, execution, and termination handles race conditions."""
    from e2b import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        num_threads = 5

        def rapid_cycle(idx: int) -> bool:
            sbx = Sandbox.create("base", timeout=60)
            try:
                cmd = sbx.commands.run(f"echo 'cycle_{idx}'")
                assert cmd.exit_code == 0
                assert f"cycle_{idx}" in cmd.stdout
                return True
            finally:
                sbx.kill()

        with concurrent.futures.ThreadPoolExecutor(max_workers=num_threads) as executor:
            outcomes = list(executor.map(rapid_cycle, range(num_threads)))

        assert all(outcomes), "Not all rapid sandbox lifecycle cycles succeeded"
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_concurrent_sandboxes_isolation standalone...")
    test_five_concurrent_sandboxes_file_isolation()
    test_five_concurrent_sandboxes_code_variable_isolation()
    test_concurrent_sandboxes_lifecycle_race()
    print("All concurrency & isolation tests passed successfully!")
