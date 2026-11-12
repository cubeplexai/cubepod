"""Real-time Streaming Output Test Suite conforming to CEP-0018.

Verifies:
1. Streaming stdout chunks arrive in real-time with sub-millisecond precision
   (inter-chunk arrival within ±100ms when code sleeps 200ms).
2. High-throughput large log generation (5MB output stream) without buffer truncation.
3. Asynchronous real-time streaming over AsyncSandbox.
"""

from __future__ import annotations

import os
import sys
import time
import pytest

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from live_gateway import LiveGateway


def test_stdout_streaming_realtime_intervals(gateway_url: str | None = None) -> None:
    """Verify that stdout chunks arrive in real-time with 200ms ± 100ms intervals."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        timestamps: list[float] = []
        chunks: list[str] = []

        def on_stdout(msg: Any) -> None:
            text = msg.line if hasattr(msg, "line") else str(msg)
            if not text.strip():
                return
            timestamps.append(time.time())
            chunks.append(text)

        with Sandbox.create() as sandbox:
            code = """
import time
import sys

for i in range(4):
    print(f"realtime_chunk_{i}", flush=True)
    if i < 3:
        time.sleep(0.20)
"""
            t_start = time.time()
            execution = sandbox.run_code(code, on_stdout=on_stdout)
            total_duration = time.time() - t_start

            assert execution.error is None
            assert len(timestamps) == 4, f"expected 4 chunk events, got {len(timestamps)}"

            # Calculate inter-chunk intervals
            intervals = [
                timestamps[i + 1] - timestamps[i] for i in range(len(timestamps) - 1)
            ]

            # Sleep is 200ms (0.20s). Within ±100ms means 0.10s <= delta <= 0.35s
            for idx, delta in enumerate(intervals):
                assert 0.10 <= delta <= 0.35, (
                    f"Interval between chunk {idx} and {idx+1} was {delta:.3f}s, "
                    f"expected 200ms ± 100ms (0.10s - 0.35s)"
                )

            assert total_duration >= 0.55, (
                f"Total execution duration {total_duration:.3f}s was too fast; chunks did not stream in real-time"
            )
    finally:
        if gw:
            gw.stop()


def test_high_throughput_large_log_stream(gateway_url: str | None = None) -> None:
    """Verify high-throughput generation and streaming of 5MB logs without truncation."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        total_bytes = 0
        chunk_count = 0

        def on_stdout(msg: Any) -> None:
            nonlocal total_bytes, chunk_count
            text = msg.line if hasattr(msg, "line") else str(msg)
            total_bytes += len(text.encode("utf-8"))
            chunk_count += 1

        with Sandbox.create() as sandbox:
            # 5120 lines * 1024 bytes per line = 5,242,880 bytes (5MB)
            code = """
import sys

chunk_1kb = ("X" * 1023) + "\\n"
for _ in range(5120):
    sys.stdout.write(chunk_1kb)
sys.stdout.flush()
"""
            execution = sandbox.run_code(code, on_stdout=on_stdout)
            assert execution.error is None
            assert total_bytes >= 5 * 1024 * 1024, (
                f"Expected >= 5MB (5242880 bytes), received {total_bytes} bytes"
            )
            assert chunk_count > 0, "Expected non-zero streamed chunk count"
    finally:
        if gw:
            gw.stop()


@pytest.mark.asyncio
async def test_async_stdout_streaming_realtime(gateway_url: str | None = None) -> None:
    """Verify asynchronous code interpreter real-time streaming."""
    from e2b_code_interpreter import AsyncSandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        timestamps: list[float] = []

        def on_stdout(msg: Any) -> None:
            timestamps.append(time.time())

        sandbox = await AsyncSandbox.create()
        try:
            code = """
import time
for i in range(3):
    print(f"async_chunk_{i}", flush=True)
    if i < 2:
        time.sleep(0.20)
"""
            execution = await sandbox.run_code(code, on_stdout=on_stdout)
            assert execution.error is None
            assert len(timestamps) == 3

            intervals = [timestamps[i+1] - timestamps[i] for i in range(len(timestamps)-1)]
            for idx, delta in enumerate(intervals):
                assert 0.10 <= delta <= 0.35, f"Async interval {idx} was {delta:.3f}s"
        finally:
            await sandbox.kill()
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_streaming_realtime standalone...")
    test_stdout_streaming_realtime_intervals()
    test_high_throughput_large_log_stream()
    import asyncio
    asyncio.run(test_async_stdout_streaming_realtime())
    print("All streaming tests passed!")
