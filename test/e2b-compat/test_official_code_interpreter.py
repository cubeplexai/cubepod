"""Official E2B Code Interpreter Compatibility Test Suite conforming to CEP-0018.

Verifies drop-in interoperability with `e2b-code-interpreter`:
- run_code("print(1 + 1)") and text/stdout extraction
- Rich graphical result parsing (matplotlib PNG images, formats, is_main_result)
- Exception and traceback parsing (ZeroDivisionError)
- Async code interpreter execution
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


def test_code_interpreter_basic_execution(gateway_url: str | None = None) -> None:
    """Verify standard code execution with text output and stdout streaming."""
    from e2b_code_interpreter import Sandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            execution = sandbox.run_code("print(1 + 1)")
            # Verify main text result representation
            assert execution.text == "2", f"expected text '2', got {execution.text!r}"
            # Verify stdout logs
            assert "2\n" in execution.logs.stdout or "2" in "".join(execution.logs.stdout)
            # Verify no error
            assert execution.error is None
            # Verify execution count
            assert execution.execution_count >= 1
    finally:
        if gw:
            gw.stop()


def test_code_interpreter_rich_result_parsing(gateway_url: str | None = None) -> None:
    """Verify rich graphical MIME types (matplotlib PNG) and format inspection."""
    from e2b_code_interpreter import Sandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            code = (
                "import matplotlib.pyplot as plt\n"
                "plt.figure()\n"
                "plt.plot([1, 2, 3], [4, 5, 6])\n"
                "plt.title('Test Chart')\n"
                "plt.show()"
            )
            execution = sandbox.run_code(code)

            assert len(execution.results) >= 1, "expected at least 1 rich result"
            res = execution.results[0]
            assert res.png is not None, "expected base64 PNG in rich result"
            assert len(res.png) > 10, "expected non-empty base64 PNG string"
            assert "png" in res.formats(), "expected 'png' in result formats"
            assert res._repr_png_() == res.png
            assert execution.error is None
    finally:
        if gw:
            gw.stop()


def test_code_interpreter_error_handling(gateway_url: str | None = None) -> None:
    """Verify execution exceptions and structured traceback serialization."""
    from e2b_code_interpreter import Sandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            execution = sandbox.run_code("1 / 0")

            assert execution.error is not None, "expected ExecutionError on division by zero"
            assert execution.error.name == "ZeroDivisionError"
            assert "division by zero" in execution.error.value
            assert "ZeroDivisionError" in execution.error.traceback
    finally:
        if gw:
            gw.stop()


@async_test
async def test_code_interpreter_async_execution(gateway_url: str | None = None) -> None:
    """Verify asynchronous code interpreter execution over AsyncSandbox."""
    from e2b_code_interpreter import AsyncSandbox

    gw: MockE2BGateway | None = None
    if not gateway_url:
        gw = MockE2BGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        sandbox = await AsyncSandbox.create()
        try:
            execution = await sandbox.run_code("print(1 + 1)")
            assert execution.text == "2"
            assert execution.error is None
        finally:
            await sandbox.kill()
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_official_code_interpreter standalone...")
    test_code_interpreter_basic_execution()
    test_code_interpreter_rich_result_parsing()
    test_code_interpreter_error_handling()
    asyncio.run(test_code_interpreter_async_execution())
    print("All code interpreter tests passed successfully!")
