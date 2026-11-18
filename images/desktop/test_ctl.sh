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

# Allocate 5 free ephemeral TCP ports on 127.0.0.1.
read -r DESKTOP_PORT TERMINAL_PORT CDP_PORT VNC_PORT WEBRTC_PORT < <(python3 -c '
import socket
socks = []
ports = []
for _ in range(5):
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
export CUBEPOD_WEBRTC_PORT="$WEBRTC_PORT"

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

NEKO_ENV_MARKER="${TMP_ROOT}/neko_env.json"
NEKO_STATIC_FIXTURE="${TMP_ROOT}/neko-www"
mkdir -p "$NEKO_STATIC_FIXTURE"
printf '<!doctype html><title>neko-webrtc-client</title>\n' >"${NEKO_STATIC_FIXTURE}/index.html"

cat >"${MOCK_BIN_DIR}/mock_neko_http.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import signal
import sys

port = int(sys.argv[1])
env_marker = os.environ.get("MOCK_NEKO_ENV_MARKER", "")
static_dir = Path(os.environ.get("NEKO_STATIC", ""))

if env_marker:
    with open(env_marker, "w", encoding="utf-8") as fh:
        json.dump(
            {
                "NEKO_DISPLAY": os.environ.get("NEKO_DISPLAY", ""),
                "NEKO_BIND": os.environ.get("NEKO_BIND", ""),
                "NEKO_SCREEN": os.environ.get("NEKO_SCREEN", ""),
                "NEKO_EPR": os.environ.get("NEKO_EPR", ""),
                "NEKO_STATIC": os.environ.get("NEKO_STATIC", ""),
                "NEKO_SERVER_STATIC": os.environ.get("NEKO_SERVER_STATIC", ""),
                "NEKO_ICELITE": os.environ.get("NEKO_ICELITE", ""),
                "NEKO_WEBRTC_ICELITE": os.environ.get("NEKO_WEBRTC_ICELITE", ""),
                "argv": sys.argv[2:],
            },
            fh,
        )

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        idx = static_dir / "index.html"
        if self.path in ("/", "/index.html") and idx.is_file():
            body = idx.read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, fmt, *args):
        return

httpd = HTTPServer(("127.0.0.1", port), Handler)
httpd.timeout = 0.2
running = True

def handle_term(_s, _f):
    global running
    running = False

signal.signal(signal.SIGTERM, handle_term)
signal.signal(signal.SIGINT, handle_term)

while running:
    httpd.handle_request()
httpd.server_close()
PY

cat >"${MOCK_BIN_DIR}/mock-neko" <<EOF
#!/usr/bin/env bash
export MOCK_NEKO_ENV_MARKER="${NEKO_ENV_MARKER}"
exec python3 "${MOCK_BIN_DIR}/mock_neko_http.py" "${WEBRTC_PORT}" "\$@"
EOF

cat >"${MOCK_BIN_DIR}/mock-neko-crash" <<'EOF'
#!/usr/bin/env bash
# Simulate CGo/GStreamer/X11 failure occurring ~150ms after launch (past initial 50ms spawn check)
sleep 0.15
echo "simulated neko GStreamer/ICE init failure" >&2
exit 1
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

echo "=== (h) WebRTC requested without neko installed gracefully falls back to noVNC ==="
export CUBEPOD_NEKO_BIN="${TMP_ROOT}/missing-neko"
OUT_H="$("$CTL" start --transport webrtc)"
assert_single_line_json "$OUT_H"
python3 - "$OUT_H" "$DESKTOP_PORT" "$TERMINAL_PORT" "$CDP_PORT" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "novnc", d
assert d.get("fallbackReason") == "neko_not_installed", d
assert d["ports"] == {"desktop": int(sys.argv[2]), "terminal": int(sys.argv[3]), "cdp": int(sys.argv[4])}, d
assert "neko" not in d["pids"], d
PY
"$CTL" stop >/dev/null

echo "=== (i) WebRTC requested with neko installed launches neko alongside stack ==="
export CUBEPOD_NEKO_BIN="${MOCK_BIN_DIR}/mock-neko"
export CUBEPOD_NEKO_STATIC="${NEKO_STATIC_FIXTURE}"
unset CUBEPOD_NEKO_ICELITE
OUT_I="$("$CTL" start --transport webrtc)"
assert_single_line_json "$OUT_I"
python3 - "$OUT_I" "$DESKTOP_PORT" "$TERMINAL_PORT" "$CDP_PORT" "$WEBRTC_PORT" "$NEKO_ENV_MARKER" "$NEKO_STATIC_FIXTURE" <<'PY'
import json, os, sys, urllib.request
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "webrtc", d
assert "fallbackReason" not in d, d
assert d["ports"] == {
    "desktop": int(sys.argv[2]),
    "terminal": int(sys.argv[3]),
    "cdp": int(sys.argv[4]),
    "webrtc": int(sys.argv[5]),
}, d
assert set(d["pids"].keys()) == {"xvfb", "wm", "chromium", "vnc", "websockify", "ttyd", "neko"}, d["pids"]
os.kill(d["pids"]["neko"], 0)

with open(sys.argv[6], encoding="utf-8") as fh:
    neko_env = json.load(fh)
assert neko_env["NEKO_STATIC"] == sys.argv[7], neko_env
assert neko_env["NEKO_SERVER_STATIC"] == sys.argv[7], neko_env
assert neko_env["NEKO_ICELITE"] == "1", neko_env
assert neko_env["NEKO_WEBRTC_ICELITE"] == "true", neko_env
assert neko_env["NEKO_BIND"] == f"0.0.0.0:{sys.argv[5]}", neko_env
assert neko_env["argv"] == ["serve"], neko_env
PY
wait_tcp_port "$WEBRTC_PORT"
python3 - "$WEBRTC_PORT" <<'PY'
import sys, urllib.request
with urllib.request.urlopen(f"http://127.0.0.1:{sys.argv[1]}/", timeout=2) as resp:
    body = resp.read().decode("utf-8")
    assert resp.status == 200, resp.status
    assert "neko-webrtc-client" in body, body
PY

OUT_I_STATUS="$("$CTL" status)"
assert_single_line_json "$OUT_I_STATUS"
python3 - "$OUT_I_STATUS" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "webrtc", d
assert "neko" in d["pids"], d
PY

# Switching back from webrtc to novnc must stop neko cleanly
NEKO_PID="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["pids"]["neko"])' "$OUT_I")"
OUT_SWITCH_NOVNC="$("$CTL" start --transport novnc)"
assert_single_line_json "$OUT_SWITCH_NOVNC"
python3 - "$OUT_SWITCH_NOVNC" "$NEKO_PID" <<'PY'
import json, os, sys
d = json.loads(sys.argv[1])
old_neko = int(sys.argv[2])
assert d["status"] == "running", d
assert d["transportUsed"] == "novnc", d
assert "neko" not in d["pids"], d
assert "webrtc" not in d["ports"], d
try:
    os.kill(old_neko, 0)
    raise AssertionError("neko still alive after switching back to novnc")
except ProcessLookupError:
    pass
PY

# Start webrtc again, then stop and verify status resets transportUsed/fallbackReason
OUT_I2="$("$CTL" start --transport webrtc)"
assert_single_line_json "$OUT_I2"
OUT_I_STOP="$("$CTL" stop)"
assert_single_line_json "$OUT_I_STOP"
python3 - "$OUT_I_STOP" "$OUT_I2" <<'PY'
import json, os, sys
stopped = json.loads(sys.argv[1])
prev = json.loads(sys.argv[2])
assert stopped["status"] == "stopped", stopped
assert stopped["pids"] == {}, stopped
assert stopped["transportUsed"] == "novnc", stopped
assert "fallbackReason" not in stopped, stopped
try:
    os.kill(prev["pids"]["neko"], 0)
    raise AssertionError("neko still alive after stop")
except ProcessLookupError:
    pass
PY

OUT_I_POST_STOP_STATUS="$("$CTL" status)"
assert_single_line_json "$OUT_I_POST_STOP_STATUS"
python3 - "$OUT_I_POST_STOP_STATUS" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "stopped", d
assert d["transportUsed"] == "novnc", d
assert "fallbackReason" not in d, d
assert "webrtc" not in d["ports"], d
PY

echo "=== (j) WebRTC requested when neko fails to start falls back cleanly to noVNC ==="
export CUBEPOD_NEKO_BIN="${MOCK_BIN_DIR}/mock-neko-crash"
OUT_J="$("$CTL" start --transport webrtc)"
assert_single_line_json "$OUT_J"
python3 - "$OUT_J" "$DESKTOP_PORT" "$TERMINAL_PORT" "$CDP_PORT" <<'PY'
import json, os, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "novnc", d
assert d.get("fallbackReason") == "neko_start_failed", d
assert d["ports"] == {"desktop": int(sys.argv[2]), "terminal": int(sys.argv[3]), "cdp": int(sys.argv[4])}, d
assert "neko" not in d["pids"], d
for name, pid in d["pids"].items():
    os.kill(pid, 0)
PY
OUT_J_STATUS="$("$CTL" status)"
assert_single_line_json "$OUT_J_STATUS"
python3 - "$OUT_J_STATUS" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "novnc", d
assert d.get("fallbackReason") == "neko_start_failed", d
PY
"$CTL" stop >/dev/null

echo "=== (k) browser_tab_follow.py wrapper installation and CDP tab activation ==="
CDP_ACTIVATED_LOG="${TMP_ROOT}/cdp_activated.log"
cat >"${MOCK_BIN_DIR}/mock_cdp_server.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
import signal
import sys

port = int(sys.argv[1])
activated_log = os.environ["MOCK_CDP_ACTIVATED_LOG"]

TARGETS = [
    {"id": "sw-bg", "type": "service_worker", "url": "https://ex.com/login"},
    {"id": "page-home", "type": "page", "url": "https://ex.com/"},
    {"id": "page-login", "type": "page", "url": "https://ex.com/login"},
]

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/json":
            raw = json.dumps(TARGETS).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            return
        if self.path.startswith("/json/activate/"):
            tid = self.path[len("/json/activate/"):]
            with open(activated_log, "a", encoding="utf-8") as fh:
                fh.write(tid + "\n")
            body = b"Target activated"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, fmt, *args):
        return

httpd = HTTPServer(("127.0.0.1", port), Handler)
httpd.timeout = 0.2
running = True

def handle_term(_s, _f):
    global running
    running = False

signal.signal(signal.SIGTERM, handle_term)
signal.signal(signal.SIGINT, handle_term)

while running:
    httpd.handle_request()
httpd.server_close()
PY

cat >"${MOCK_BIN_DIR}/mock-chromium-cdp" <<EOF
#!/usr/bin/env bash
export MOCK_CDP_ACTIVATED_LOG="${CDP_ACTIVATED_LOG}"
exec python3 "${MOCK_BIN_DIR}/mock_cdp_server.py" "${CDP_PORT}"
EOF
chmod +x "${MOCK_BIN_DIR}/mock-chromium-cdp"
export CUBEPOD_BROWSER_BIN="${MOCK_BIN_DIR}/mock-chromium-cdp"

AGENT_WRAP_DIR="${TMP_ROOT}/agent-bin"
mkdir -p "$AGENT_WRAP_DIR"
AGENT_CURRENT_URL="${TMP_ROOT}/agent_url.txt"
cat >"${AGENT_WRAP_DIR}/agent-browser" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "open" ]; then
  printf '%s\n' "\${2:-}" >"${AGENT_CURRENT_URL}"
  # Re-enter wrapper recursively to verify AGENT_BROWSER_WRAPPER=1 guard
  "${AGENT_WRAP_DIR}/agent-browser" noop-nested
  exit 0
fi
if [ "\${1:-}" = "get" ] && [ "\${2:-}" = "url" ]; then
  [ -f "${AGENT_CURRENT_URL}" ] && cat "${AGENT_CURRENT_URL}"
  exit 0
fi
exit 0
EOF
chmod +x "${AGENT_WRAP_DIR}/agent-browser"

export CUBEPOD_AGENT_BROWSER_BIN="${AGENT_WRAP_DIR}/agent-browser"
export CUBEPOD_FOCUS_TAB_BIN="${AGENT_WRAP_DIR}/cubeplex-focus-browser-tab"
export CUBEPOD_NEKO_BIN="${MOCK_BIN_DIR}/mock-neko"

OUT_K1="$("$CTL" start --transport webrtc)"
assert_single_line_json "$OUT_K1"
# Second start verifies wrapper installation is idempotent (.real is not overwritten)
OUT_K2="$("$CTL" start --transport webrtc)"
assert_single_line_json "$OUT_K2"
wait_tcp_port "$CDP_PORT"

[ -f "${AGENT_WRAP_DIR}/agent-browser.real" ] || { echo "FAIL: agent-browser.real missing" >&2; exit 1; }
grep -q "CUBEPLEX_TAB_FOLLOW=1" "${AGENT_WRAP_DIR}/agent-browser" || { echo "FAIL: wrapper marker missing" >&2; exit 1; }
if grep -q "CUBEPLEX_TAB_FOLLOW=1" "${AGENT_WRAP_DIR}/agent-browser.real"; then
  echo "FAIL: agent-browser.real was overwritten by wrapper" >&2
  exit 1
fi

"${AGENT_WRAP_DIR}/agent-browser" open "https://ex.com/login/"
"${AGENT_WRAP_DIR}/agent-browser" open "https://ex.com/login/subpath"
"${AGENT_WRAP_DIR}/agent-browser" open "https://ex.com"

python3 - "$CDP_ACTIVATED_LOG" <<'PY'
import sys
with open(sys.argv[1], encoding="utf-8") as fh:
    activated = [ln.strip() for ln in fh.read().splitlines() if ln.strip()]
assert activated == ["page-login", "page-home"], f"unexpected activated targets: {activated}"
PY

"$CTL" stop >/dev/null

if [ -x /usr/local/bin/neko ] && command -v Xvfb >/dev/null 2>&1 && [ -d /opt/cubepod/desktop/neko-www ]; then
  echo "=== (l) Real /usr/local/bin/neko + Xvfb WebRTC end-to-end verification ==="
  export CUBEPOD_XVFB_BIN="Xvfb"
  export CUBEPOD_NEKO_BIN="/usr/local/bin/neko"
  export CUBEPOD_NEKO_STATIC="/opt/cubepod/desktop/neko-www"
  export CUBEPOD_DISPLAY=":98"
  OUT_L="$("$CTL" start --transport webrtc)"
  assert_single_line_json "$OUT_L"
  python3 - "$OUT_L" "$WEBRTC_PORT" <<'PY'
import json, os, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "webrtc", d
assert "fallbackReason" not in d, d
assert d["ports"]["webrtc"] == int(sys.argv[2]), d
os.kill(d["pids"]["neko"], 0)
PY
  wait_tcp_port "$WEBRTC_PORT"
  python3 - "$WEBRTC_PORT" <<'PY'
import sys, time, urllib.request
port = int(sys.argv[1])
deadline = time.time() + 5.0
last_err = None
while time.time() < deadline:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=2) as resp:
            body = resp.read().decode("utf-8", errors="replace")
            assert resp.status == 200, resp.status
            assert "<html" in body.lower() or "neko" in body.lower(), body[:200]
            last_err = None
            break
    except Exception as exc:
        last_err = exc
        time.sleep(0.1)
if last_err is not None:
    raise AssertionError(f"real neko HTTP check failed on 127.0.0.1:{port}: {last_err}")
PY
  "$CTL" stop >/dev/null
elif command -v docker >/dev/null 2>&1 && docker image inspect ghcr.io/m1k1o/neko/base:latest >/dev/null 2>&1; then
  echo "=== (l) Real /usr/bin/neko WebRTC + X11 GStreamer container verification ==="
  docker run --rm --entrypoint bash -v "${SCRIPT_DIR}:/src:ro" ghcr.io/m1k1o/neko/base:latest -c '
    set -euo pipefail
    /usr/bin/X :98 -config /etc/neko/xorg.conf -noreset -nolisten tcp >/tmp/x.log 2>&1 &
    XPID=$!
    trap "kill \$XPID 2>/dev/null || true" EXIT
    for _ in $(seq 1 30); do
      [ -S /tmp/.X11-unix/X98 ] && break
      sleep 0.05
    done
    cat >/tmp/noop-daemon <<EOF
#!/usr/bin/env bash
trap "exit 0" TERM INT
while true; do sleep 0.2; done
EOF
    chmod +x /tmp/noop-daemon
    export CUBEPOD_DESKTOP_RUN_DIR=/tmp/ctl-run
    export CUBEPOD_BROWSER_PROFILE_DIR=/tmp/profile
    export CUBEPOD_WORKDIR=/tmp/ws
    export CUBEPOD_DISPLAY=":98"
    export CUBEPOD_XVFB_BIN=/tmp/noop-daemon
    export CUBEPOD_WM_BIN=/tmp/noop-daemon
    export CUBEPOD_BROWSER_BIN=/tmp/noop-daemon
    export CUBEPOD_X11VNC_BIN=/tmp/noop-daemon
    export CUBEPOD_WEBSOCKIFY_BIN=/tmp/noop-daemon
    export CUBEPOD_TTYD_BIN=/tmp/noop-daemon
    export CUBEPOD_NEKO_BIN=/usr/bin/neko
    export CUBEPOD_NEKO_STATIC=/var/www
    export CUBEPOD_WEBRTC_PORT=18081
    OUT_REAL="$(/src/cubepod-desktop-ctl start --transport webrtc)"
    python3 - "$OUT_REAL" <<PY
import json, sys, urllib.request
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "webrtc", d
assert "fallbackReason" not in d, d
assert d["ports"]["webrtc"] == 18081, d
assert "neko" in d["pids"], d
# Immediate single-shot HTTP request (no retry loop) proves wait_neko_ready eliminated the 502 race
with urllib.request.urlopen("http://127.0.0.1:18081/", timeout=2) as r:
    body = r.read().decode("utf-8", errors="replace")
    assert r.status == 200 and "<html" in body.lower(), (r.status, body[:120])
PY
    OUT_STOP_REAL="$(/src/cubepod-desktop-ctl stop)"
    python3 -c "import json,sys; d=json.loads(sys.argv[1]); assert d[\"status\"]==\"stopped\" and d[\"pids\"]=={}, d" "$OUT_STOP_REAL"

    # Also verify real /usr/bin/neko against a nonexistent X display (:77) triggers neko_start_failed fallback
    export CUBEPOD_DISPLAY=":77"
    OUT_REAL_FB="$(/src/cubepod-desktop-ctl start --transport webrtc)"
    python3 - "$OUT_REAL_FB" <<PY
import json, sys
d = json.loads(sys.argv[1])
assert d["status"] == "running", d
assert d["transportUsed"] == "novnc", d
assert d.get("fallbackReason") == "neko_start_failed", d
assert "neko" not in d["pids"], d
PY
    /src/cubepod-desktop-ctl stop >/dev/null
  '
else
  echo "=== (l) Skipping live X11+neko binary check (neither local Xvfb+neko nor cached neko image found) ==="
fi

echo "ALL TESTS PASSED"
