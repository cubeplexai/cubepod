#!/usr/bin/env bash
# Integration & unit test suite for cubepod-desktop-ctl (CEP-0021).
# Runs completely unprivileged using lightweight Python TCP mock daemons on ephemeral ports.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CTL="${SCRIPT_DIR}/cubepod-desktop-ctl"

if [ ! -x "$CTL" ]; then
  chmod +x "$CTL"
fi

TMP_ROOT="$(mktemp -d -t cubepod-desktop-test-XXXXXX)"
cleanup() {
  local rc=$?
  set +e
  if [ -n "${CUBEPOD_DESKTOP_RUN_DIR:-}" ] && [ -d "${CUBEPOD_DESKTOP_RUN_DIR}" ]; then
    "$CTL" stop >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP_ROOT"
  exit "$rc"
}
trap cleanup EXIT

export CUBEPOD_DESKTOP_RUN_DIR="${TMP_ROOT}/run"
export CUBEPOD_BROWSER_PROFILE_DIR="${TMP_ROOT}/chromium-profile"
export CUBEPOD_WORKDIR="${TMP_ROOT}/workspace"
export CUBEPOD_DISPLAY=":99"
mkdir -p "$CUBEPOD_BROWSER_PROFILE_DIR" "$CUBEPOD_WORKDIR"

# Allocate 4 free ephemeral TCP ports on 127.0.0.1.
read -r DESKTOP_PORT TERMINAL_PORT CDP_PORT VNC_PORT < <(python3 -c '
import socket
socks = []
ports = []
for _ in range(4):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    ports.append(str(s.getsockname()[1]))
    socks.append(s)
for s in socks:
    s.close()
print(" ".join(ports))
')

export CUBEPOD_DESKTOP_PORT="$DESKTOP_PORT"
export CUBEPOD_TERMINAL_PORT="$TERMINAL_PORT"
export CUBEPOD_CDP_PORT="$CDP_PORT"
export CUBEPOD_VNC_PORT="$VNC_PORT"

MOCK_BIN_DIR="${TMP_ROOT}/mocks"
mkdir -p "$MOCK_BIN_DIR"

# Shared mock TCP/sleeper helper in Python.
cat >"${MOCK_BIN_DIR}/mock_daemon.py" <<'PY'
import os
import signal
import socket
import sys
import time

running = True
def handle_term(_signum, _frame):
    global running
    running = False

signal.signal(signal.SIGTERM, handle_term)
signal.signal(signal.SIGINT, handle_term)

role = sys.argv[1]
port = int(sys.argv[2]) if len(sys.argv) > 2 else 0

cwd_marker = os.environ.get("MOCK_CWD_MARKER", "")
if cwd_marker:
    with open(cwd_marker, "w", encoding="utf-8") as f:
        f.write(os.getcwd())

srv = None
if port > 0:
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", port))
    srv.listen(8)
    srv.settimeout(0.2)

while running:
    if srv is not None:
        try:
            conn, _ = srv.accept()
            conn.close()
        except socket.timeout:
            pass
    else:
        time.sleep(0.1)

if srv is not None:
    srv.close()
PY

cat >"${MOCK_BIN_DIR}/mock-xvfb" <<EOF
#!/usr/bin/env bash
exec python3 "${MOCK_BIN_DIR}/mock_daemon.py" xvfb 0
EOF

cat >"${MOCK_BIN_DIR}/mock-wm" <<EOF
#!/usr/bin/env bash
exec python3 "${MOCK_BIN_DIR}/mock_daemon.py" wm 0
EOF

cat >"${MOCK_BIN_DIR}/mock-chromium" <<EOF
#!/usr/bin/env bash
exec python3 "${MOCK_BIN_DIR}/mock_daemon.py" chromium "${CDP_PORT}"
EOF

cat >"${MOCK_BIN_DIR}/mock-x11vnc" <<EOF
#!/usr/bin/env bash
exec python3 "${MOCK_BIN_DIR}/mock_daemon.py" vnc "${VNC_PORT}"
EOF

cat >"${MOCK_BIN_DIR}/mock-websockify" <<EOF
#!/usr/bin/env bash
exec python3 "${MOCK_BIN_DIR}/mock_daemon.py" websockify "${DESKTOP_PORT}"
EOF

TTYD_CWD_MARKER="${TMP_ROOT}/ttyd_cwd.txt"
cat >"${MOCK_BIN_DIR}/mock-ttyd" <<EOF
#!/usr/bin/env bash
export MOCK_CWD_MARKER="${TTYD_CWD_MARKER}"
exec python3 "${MOCK_BIN_DIR}/mock_daemon.py" ttyd "${TERMINAL_PORT}"
EOF

chmod +x "${MOCK_BIN_DIR}"/mock-*

export CUBEPOD_XVFB_BIN="${MOCK_BIN_DIR}/mock-xvfb"
export CUBEPOD_WM_BIN="${MOCK_BIN_DIR}/mock-wm"
export CUBEPOD_BROWSER_BIN="${MOCK_BIN_DIR}/mock-chromium"
export CUBEPOD_X11VNC_BIN="${MOCK_BIN_DIR}/mock-x11vnc"
export CUBEPOD_WEBSOCKIFY_BIN="${MOCK_BIN_DIR}/mock-websockify"
export CUBEPOD_TTYD_BIN="${MOCK_BIN_DIR}/mock-ttyd"

wait_tcp_port() {
  local port="$1"
  python3 - "$port" <<'PY'
import socket, sys, time
port = int(sys.argv[1])
deadline = time.time() + 5.0
while time.time() < deadline:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=0.5):
            sys.exit(0)
    except OSError:
        time.sleep(0.05)
print(f"timeout waiting for TCP 127.0.0.1:{port}", file=sys.stderr)
sys.exit(1)
PY
}

assert_single_line_json() {
  local raw="$1"
  local line_count
  line_count="$(printf '%s\n' "$raw" | wc -l)"
  if [ "$line_count" -ne 1 ]; then
    echo "FAIL: expected single-line JSON, got $line_count lines: $raw" >&2
    exit 1
  fi
}

echo "=== (a) Initial status is stopped ==="
OUT_A="$("$CTL" status)"
assert_single_line_json "$OUT_A"
python3 - "$OUT_A" "$DESKTOP_PORT" "$TERMINAL_PORT" "$CDP_PORT" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "stopped", d
assert d["display"] == ":99", d
assert d["ports"] == {"desktop": int(sys.argv[2]), "terminal": int(sys.argv[3]), "cdp": int(sys.argv[4])}, d
assert d["pids"] == {}, d
PY

echo "=== (b) Stale Singleton* locks are purged on start ==="
ln -s "dead-pod-99999" "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonLock"
touch "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonCookie" "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonSocket"
[ -L "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonLock" ]

OUT_C="$("$CTL" start --resolution 1440x900x24 --url https://example.com)"
assert_single_line_json "$OUT_C"

if [ -L "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonLock" ] || [ -e "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonLock" ]; then
  echo "FAIL: SingletonLock was not removed on start" >&2
  exit 1
fi
if [ -e "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonCookie" ] || [ -e "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonSocket" ]; then
  echo "FAIL: SingletonCookie/SingletonSocket were not removed on start" >&2
  exit 1
fi

echo "=== (c) Start transitions to running with valid JSON and live ports/PIDs ==="
python3 - "$OUT_C" "$DESKTOP_PORT" "$TERMINAL_PORT" "$CDP_PORT" <<'PY'
import json, os, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["display"] == ":99", d
assert d["resolution"] == "1440x900x24", d
assert d["ports"] == {"desktop": int(sys.argv[2]), "terminal": int(sys.argv[3]), "cdp": int(sys.argv[4])}, d
expected_daemons = {"xvfb", "wm", "chromium", "vnc", "websockify", "ttyd"}
assert set(d["pids"].keys()) == expected_daemons, d["pids"]
for name, pid in d["pids"].items():
    assert isinstance(pid, int) and pid > 0, (name, pid)
    os.kill(pid, 0)
PY

wait_tcp_port "$DESKTOP_PORT"
wait_tcp_port "$TERMINAL_PORT"
wait_tcp_port "$CDP_PORT"
wait_tcp_port "$VNC_PORT"

if [ "$(cat "$TTYD_CWD_MARKER")" != "$CUBEPOD_WORKDIR" ]; then
  echo "FAIL: ttyd cwd mismatch: expected $CUBEPOD_WORKDIR, got $(cat "$TTYD_CWD_MARKER")" >&2
  exit 1
fi

echo "=== (d) Second start is idempotent (same PIDs preserved) ==="
OUT_D="$("$CTL" start --resolution 1440x900x24)"
assert_single_line_json "$OUT_D"
python3 - "$OUT_C" "$OUT_D" <<'PY'
import json, sys
c = json.loads(sys.argv[1])
d = json.loads(sys.argv[2])
assert d["status"] == "running", d
assert d["pids"] == c["pids"], f"expected identical pids, got {d['pids']} vs {c['pids']}"
PY

echo "=== (e) Killing one daemon heals ONLY that daemon on start ==="
CHROME_PID="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["pids"]["chromium"])' "$OUT_C")"
kill -KILL "$CHROME_PID"
# Wait until chromium process is completely reaped/gone.
for _ in $(seq 1 40); do
  if ! kill -0 "$CHROME_PID" 2>/dev/null; then
    break
  fi
  sleep 0.05
done

OUT_DEGRADED="$("$CTL" status)"
assert_single_line_json "$OUT_DEGRADED"
python3 - "$OUT_DEGRADED" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "degraded", d
assert "chromium" not in d["pids"], d
assert set(d["pids"].keys()) == {"xvfb", "wm", "vnc", "websockify", "ttyd"}, d
PY

ln -s "stale-after-crash-123" "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonLock"
OUT_E="$("$CTL" start)"
assert_single_line_json "$OUT_E"
if [ -L "${CUBEPOD_BROWSER_PROFILE_DIR}/SingletonLock" ]; then
  echo "FAIL: SingletonLock was not cleaned during partial crash recovery" >&2
  exit 1
fi
python3 - "$OUT_C" "$OUT_E" <<'PY'
import json, os, sys
before = json.loads(sys.argv[1])
after = json.loads(sys.argv[2])
assert after["status"] == "running", after
assert after["pids"]["chromium"] != before["pids"]["chromium"], "chromium PID should have changed"
os.kill(after["pids"]["chromium"], 0)
for k in ("xvfb", "wm", "vnc", "websockify", "ttyd"):
    assert after["pids"][k] == before["pids"][k], f"{k} PID changed unexpectedly: {before['pids'][k]} -> {after['pids'][k]}"
PY
wait_tcp_port "$CDP_PORT"

echo "=== (f) Concurrent parallel start calls serialize safely via flock ==="
OUT_STOP_PRE_CONC="$("$CTL" stop)"
python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["status"]=="stopped" and d["pids"]=={}' "$OUT_STOP_PRE_CONC"

CONC_DIR="${TMP_ROOT}/concurrent"
mkdir -p "$CONC_DIR"
for i in 1 2 3 4; do
  "$CTL" start >"${CONC_DIR}/out_${i}.json" &
done
wait

python3 - "${CONC_DIR}"/out_*.json <<'PY'
import json, sys
first_pids = None
for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as f:
        lines = [ln for ln in f.read().splitlines() if ln.strip()]
    assert len(lines) == 1, (path, lines)
    d = json.loads(lines[0])
    assert d["status"] == "running", (path, d)
    if first_pids is None:
        first_pids = d["pids"]
    else:
        assert d["pids"] == first_pids, f"concurrent start PID mismatch in {path}: {d['pids']} != {first_pids}"
assert len(first_pids) == 6, first_pids
PY

echo "=== (g) Stop cleanly terminates all daemons and transitions status to stopped ==="
OUT_G="$("$CTL" stop)"
assert_single_line_json "$OUT_G"
python3 - "$OUT_G" "${CONC_DIR}/out_1.json" <<'PY'
import json, os, sys
stopped = json.loads(sys.argv[1])
assert stopped["status"] == "stopped", stopped
assert stopped["pids"] == {}, stopped
with open(sys.argv[2], encoding="utf-8") as f:
    prev = json.loads(f.read().strip())
for name, pid in prev["pids"].items():
    try:
        os.kill(pid, 0)
        raise AssertionError(f"daemon {name} (pid {pid}) still alive after stop")
    except ProcessLookupError:
        pass
PY

OUT_FINAL_STATUS="$("$CTL" status)"
assert_single_line_json "$OUT_FINAL_STATUS"
python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["status"]=="stopped" and d["pids"]=={}, d' "$OUT_FINAL_STATUS"

echo "ALL TESTS PASSED"
