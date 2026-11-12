#!/usr/bin/env bash
set -euo pipefail

# Official E2B SDK Compatibility Test Runner (CEP-0018)
# Runs the full official E2B sync, async, and code-interpreter test suite.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

cd "${REPO_ROOT}"

echo "====================================================================="
echo " Running CubePod Official E2B SDK Compatibility Test Suite (CEP-0018)"
echo "====================================================================="

if command -v uv >/dev/null 2>&1; then
    echo "[e2b-compat] Using uv test runner with pinned dependencies..."
    uv run \
        --with "e2b>=2.0.0" \
        --with "e2b-code-interpreter>=2.0.0" \
        --with "pytest>=8.0.0" \
        --with "pytest-asyncio>=0.23.0" \
        python -m pytest -c "${SCRIPT_DIR}/pytest.ini" "${SCRIPT_DIR}" -v "$@"
elif command -v pytest >/dev/null 2>&1; then
    echo "[e2b-compat] Using local pytest..."
    pytest -c "${SCRIPT_DIR}/pytest.ini" "${SCRIPT_DIR}" -v "$@"
else
    echo "[e2b-compat] Falling back to standalone python execution..."
    python3 "${SCRIPT_DIR}/test_official_e2b_sync.py"
    python3 "${SCRIPT_DIR}/test_official_e2b_async.py"
    python3 "${SCRIPT_DIR}/test_official_code_interpreter.py"
fi

echo "====================================================================="
echo " All Official E2B SDK Compatibility Tests Passed Cleanly! (CEP-0018) "
echo "====================================================================="
