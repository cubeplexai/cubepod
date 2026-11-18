#!/usr/bin/env bash
# Shared exact dual-SDK contract for kind and Compose. URLs must already target
# this invocation's real loopback gateway; test modules reject cloud credentials.
set -Eeuo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
: "${STATE:?}" "${ARTIFACTS:?}" "${E2B_API_URL:?}"
"${PYTHON:-python3}" -m venv "$STATE/venv"
timeout 180s "$STATE/venv/bin/pip" install --disable-pip-version-check -r "$HERE/requirements.txt"
mkdir -p "$STATE/sdk"
cp "$HERE/package.json" "$HERE/package-lock.json" "$HERE/sdk.test.mjs" "$STATE/sdk/"
timeout 180s npm ci --ignore-scripts --prefix "$STATE/sdk"
export E2B_SANDBOX_URL="$E2B_API_URL" E2B_VOLUME_API_URL="$E2B_API_URL" E2B_API_KEY=cpod_kind_test_only
unset E2B_DOMAIN E2B_ACCESS_TOKEN
printf '%s\n' "$E2B_API_URL" >"$ARTIFACTS/gateway-url.txt"
py=0; ts=0
timeout 600s "$STATE/venv/bin/python" -m pytest -v "$HERE/test_sdk.py" --junitxml="$ARTIFACTS/python.xml" >"$ARTIFACTS/python.log" 2>&1 || py=$?
timeout 600s node --test --test-concurrency=1 --test-reporter=junit "$STATE/sdk/sdk.test.mjs" >"$ARTIFACTS/typescript.xml" 2>"$ARTIFACTS/typescript-stderr.log" || ts=$?
echo "SDK exit codes: python=$py typescript=$ts"
python3 "$HERE/check_reports.py" "$ARTIFACTS/python.xml" "$ARTIFACTS/typescript.xml"
(( py == 0 && ts == 0 ))
