"""Multimodal Data Science Compatibility Test Suite conforming to CEP-0018.

Verifies:
1. Multiple Matplotlib figures capture and base64 PNG serialization.
2. Pandas DataFrame rich HTML display representation (_repr_html_).
3. Structured multi-layer exception and traceback propagation.
4. Combined multimodal data science workflow (code, stdout, plots, tables).
"""

from __future__ import annotations

import base64
import os
import sys
import pytest

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from live_gateway import LiveGateway


def test_multiple_matplotlib_figures(gateway_url: str | None = None) -> None:
    """Verify multiple matplotlib figures are each captured with valid PNG payloads."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            code = """
import matplotlib.pyplot as plt
import numpy as np

# Figure 1: Line Chart
plt.figure()
x = np.linspace(0, 10, 50)
plt.plot(x, np.sin(x), label='sin')
plt.title('Sine Curve')
plt.show()

# Figure 2: Bar Chart
plt.figure()
plt.bar(['A', 'B', 'C', 'D'], [12, 24, 18, 30], color='skyblue')
plt.title('Category Distribution')
plt.show()

# Figure 3: Histogram
plt.figure()
data = np.random.normal(0, 1, 100)
plt.hist(data, bins=10, color='salmon')
plt.title('Distribution')
plt.show()
"""
            execution = sandbox.run_code(code)
            assert execution.error is None, f"Execution failed: {execution.error}"
            assert len(execution.results) >= 3, (
                f"Expected at least 3 graphical results, got {len(execution.results)}"
            )

            for idx, res in enumerate(execution.results[:3]):
                assert res.png is not None, f"Result {idx} missing png payload"
                assert len(res.png) > 100, f"Result {idx} png payload too short"
                assert "png" in res.formats(), f"Result {idx} formats missing 'png'"
                assert res._repr_png_() == res.png

                # Validate standard PNG magic bytes header
                decoded = base64.b64decode(res.png)
                assert decoded.startswith(b"\x89PNG\r\n\x1a\n"), (
                    f"Result {idx} does not have valid PNG binary signature"
                )
    finally:
        if gw:
            gw.stop()


def test_pandas_dataframe_repr_html(gateway_url: str | None = None) -> None:
    """Verify Pandas DataFrame HTML table representation via _repr_html_."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            code = """
import pandas as pd

df = pd.DataFrame({
    "Framework": ["CubePod", "E2B", "Docker"],
    "Persistence": ["Block/Filesystem CoW", "Ephemeral Only", "OverlayFS"],
    "ColdStartMS": [12, 150, 450],
})
df
"""
            execution = sandbox.run_code(code)
            assert execution.error is None, f"Execution failed: {execution.error}"
            assert len(execution.results) >= 1, (
                f"Expected >= 1 rich result for DataFrame, got {len(execution.results)}"
            )

            res = execution.results[0]
            assert res.html is not None, "Result missing HTML representation"
            assert "<table" in res.html, "Expected <table element in HTML repr"
            assert "CubePod" in res.html
            assert "Persistence" in res.html
            assert "ColdStartMS" in res.html
            assert "html" in res.formats()
            assert res._repr_html_() == res.html
    finally:
        if gw:
            gw.stop()


def test_multilayer_traceback_exception(gateway_url: str | None = None) -> None:
    """Verify nested multi-frame function call traceback serialization."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            code = """
def deep_calculation(denominator):
    return 1000 / denominator

def intermediate_transform(val):
    adjusted = val - 42
    return deep_calculation(adjusted)

def entrypoint_handler(seed):
    return intermediate_transform(seed * 2)

entrypoint_handler(21)  # 21 * 2 = 42 -> 42 - 42 = 0 -> ZeroDivisionError
"""
            execution = sandbox.run_code(code)
            assert execution.error is not None, "Expected ExecutionError"
            err = execution.error

            assert err.name == "ZeroDivisionError", f"Expected ZeroDivisionError, got {err.name}"
            assert "division by zero" in err.value
            # Verify stack frames appear in the structured traceback
            assert "entrypoint_handler" in err.traceback, "Missing entrypoint frame in traceback"
            assert "intermediate_transform" in err.traceback, "Missing intermediate frame in traceback"
            assert "deep_calculation" in err.traceback, "Missing deep calculation frame in traceback"
            assert "ZeroDivisionError" in err.traceback
    finally:
        if gw:
            gw.stop()


def test_combined_multimodal_workflow(gateway_url: str | None = None) -> None:
    """Verify complex notebook-style workflow combining print logs, charts, and dataframe tables."""
    from e2b_code_interpreter import Sandbox

    gw: LiveGateway | None = None
    if not gateway_url:
        gw = LiveGateway()
        gateway_url = gw.start()
        os.environ["E2B_API_URL"] = gateway_url
        os.environ["E2B_SANDBOX_URL"] = gateway_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_key")

    try:
        with Sandbox.create() as sandbox:
            code = """
import pandas as pd
import matplotlib.pyplot as plt

print("Initializing data analysis pipeline...")

# Step 1: Create dataset
data = {
    "epoch": [1, 2, 3, 4],
    "loss": [0.82, 0.54, 0.31, 0.18],
    "accuracy": [0.72, 0.85, 0.91, 0.96]
}
df = pd.DataFrame(data)

# Step 2: Plot convergence
plt.figure()
plt.plot(df["epoch"], df["loss"], marker='o', label='Train Loss')
plt.plot(df["epoch"], df["accuracy"], marker='s', label='Accuracy')
plt.title('Training Metrics')
plt.legend()
plt.show()

print("Pipeline completed successfully.")

df
"""
            execution = sandbox.run_code(code)
            assert execution.error is None

            # Verify stdout logs
            all_stdout = "".join(execution.logs.stdout)
            assert "Initializing data analysis pipeline..." in all_stdout
            assert "Pipeline completed successfully." in all_stdout

            # Verify rich results contain both figure (PNG) and dataframe (HTML)
            has_png = any(r.png is not None for r in execution.results)
            has_html = any(r.html is not None for r in execution.results)
            assert has_png, "Expected at least one PNG chart in execution results"
            assert has_html, "Expected at least one HTML table in execution results"
    finally:
        if gw:
            gw.stop()


if __name__ == "__main__":
    print("Running test_multimodal_data_science standalone...")
    test_multiple_matplotlib_figures()
    test_pandas_dataframe_repr_html()
    test_multilayer_traceback_exception()
    test_combined_multimodal_workflow()
    print("All multimodal data science tests passed!")
