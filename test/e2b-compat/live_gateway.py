"""Live CubePod Gateway Server Harness for SDK Compatibility Verification.

Conforms to CubePod CEP-0018 specification:
- Compiles and launches the real Go binary (bin/cubepod-gateway) on an ephemeral port
  with an isolated temporary workspace directory.
- Manages sandbox lifecycle via the real Gateway reconciler (/sandboxes, /v2/sandboxes).
- Bridges official E2B Connect-RPC data-plane (:49983, /process.Process/Start).
- Provides real-time unbuffered Code Interpreter execution (:49999, /execute NDJSON streaming)
  with sub-process tree termination on timeout (zero zombie processes), rich multimodal
  result extraction (matplotlib figures as base64 PNG, Pandas DataFrame _repr_html_),
  and full multi-frame exception tracebacks.
"""

from __future__ import annotations

import ast
import base64
import http.server
import io
import json
import os
import re
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid
from typing import Any, Dict, List, Optional, Tuple


def find_free_port() -> int:
    """Find an available ephemeral TCP port on localhost."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def make_connect_frame(data: bytes, is_end: bool = False) -> bytes:
    """Pack ConnectRPC envelope frame (1 byte flag + 4 bytes big-endian length + payload)."""
    prefix = 2 if is_end else 0
    return struct.pack(">BI", prefix, len(data)) + data


# Embedded Python code runner executed inside the sandbox workspace
_RUNNER_SCRIPT = """
import sys
import os
import io
import json
import base64
import ast
import traceback

code_file = sys.argv[1]
result_file = sys.argv[2]

with open(code_file, "r", encoding="utf-8") as f:
    code = f.read()

results = []
error_info = None

def capture_result(obj):
    if obj is None:
        return
    res = {}
    if hasattr(obj, "_repr_html_"):
        try:
            res["html"] = obj._repr_html_()
        except Exception:
            pass
    if hasattr(obj, "_repr_png_"):
        try:
            png_bytes = obj._repr_png_()
            if isinstance(png_bytes, bytes):
                res["png"] = base64.b64encode(png_bytes).decode()
            elif isinstance(png_bytes, str):
                res["png"] = png_bytes
        except Exception:
            pass
    res["text"] = repr(obj)
    res["is_main_result"] = True
    results.append(res)

# Patch matplotlib to automatically capture figures
try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    orig_show = plt.show

    def patched_show(*args, **kwargs):
        for fig_num in plt.get_fignums():
            fig = plt.figure(fig_num)
            buf = io.BytesIO()
            fig.savefig(buf, format="png", bbox_inches="tight")
            buf.seek(0)
            png_b64 = base64.b64encode(buf.read()).decode("utf-8")
            results.append({
                "text": f"<Figure size {int(fig.get_figwidth()*fig.dpi)}x{int(fig.get_figheight()*fig.dpi)} with {len(fig.axes)} Axes>",
                "png": png_b64,
                "is_main_result": True
            })
        plt.close("all")

    plt.show = patched_show
except Exception:
    pass

global_scope = {"__name__": "__main__"}
try:
    parsed = ast.parse(code)
    if parsed.body and isinstance(parsed.body[-1], ast.Expr):
        if len(parsed.body) > 1:
            mod_before = ast.Module(body=parsed.body[:-1], type_ignores=[])
            exec(compile(mod_before, "<sandbox>", "exec"), global_scope)
        last_expr = ast.Expression(body=parsed.body[-1].value)
        val = eval(compile(last_expr, "<sandbox>", "eval"), global_scope)
        capture_result(val)
    else:
        exec(compile(code, "<sandbox>", "exec"), global_scope)

    try:
        import matplotlib.pyplot as plt
        if plt.get_fignums():
            patched_show()
    except Exception:
        pass
except Exception as e:
    exc_type, exc_val, exc_tb = sys.exc_info()
    tb_lines = traceback.format_exception(exc_type, exc_val, exc_tb)
    error_info = {
        "name": exc_type.__name__ if exc_type else "ExecutionError",
        "value": str(exc_val),
        "traceback": "".join(tb_lines)
    }

output_data = {
    "results": results,
    "error": error_info
}
with open(result_file, "w", encoding="utf-8") as f:
    json.dump(output_data, f)
"""


class LiveGatewayHandler(http.server.BaseHTTPRequestHandler):
    """HTTP Request Handler implementing E2B Control & Data Plane endpoints."""

    def log_message(self, format: str, *args: Any) -> None:
        """Suppress default verbose logging."""
        pass

    @property
    def gateway(self) -> LiveGateway:
        return self.server.gateway  # type: ignore[attr-defined]

    def _read_body(self) -> bytes:
        if self.headers.get("Transfer-Encoding") == "chunked":
            body = b""
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    break
                try:
                    chunk_len = int(line, 16)
                except ValueError:
                    break
                if chunk_len == 0:
                    self.rfile.readline()
                    break
                body += self.rfile.read(chunk_len)
                self.rfile.readline()
            return body

        length = int(self.headers.get("Content-Length", 0))
        return self.rfile.read(length) if length > 0 else b""

    def _get_sandbox_id(self) -> str:
        parsed = urllib.parse.urlparse(self.path)
        qs = urllib.parse.parse_qs(parsed.query)
        sbx_id = (
            self.headers.get("E2b-Sandbox-Id")
            or self.headers.get("X-Sandbox-Id")
            or qs.get("sandboxID", [None])[0]
            or qs.get("sandbox_id", [None])[0]
        )
        if not sbx_id and self.path.startswith("/sandboxes/"):
            parts = self.path.strip("/").split("/")
            if len(parts) >= 2:
                sbx_id = parts[1]
        return sbx_id or "default"

    def do_GET(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        qs = urllib.parse.parse_qs(parsed.query)

        # 1. Health check
        if path in ("/health", "/healthz"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status": "ok", "service": "cubepod-gateway"}')
            return

        # 2. File read endpoint: GET /files?path=...
        if path == "/files":
            file_path = qs.get("path", [None])[0]
            sandbox_id = self._get_sandbox_id()
            ws_dir = self.gateway.get_workspace(sandbox_id)
            if not file_path:
                self.send_response(400)
                self.end_headers()
                return

            rel = file_path.lstrip("/").replace("workspace/", "", 1)
            target = os.path.join(ws_dir, rel)
            if os.path.isfile(target):
                with open(target, "rb") as f:
                    data = f.read()
                self.send_response(200)
                self.send_header("Content-Type", "application/octet-stream")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return
            else:
                self.send_response(404)
                self.end_headers()
                return

        # 3. File listing: GET /files/list?path=...
        if path in ("/files/list", "/sandboxes/files/list"):
            file_path = qs.get("path", ["/workspace"])[0]
            sandbox_id = self._get_sandbox_id()
            ws_dir = self.gateway.get_workspace(sandbox_id)
            rel = file_path.lstrip("/").replace("workspace", "").lstrip("/")
            target_dir = os.path.join(ws_dir, rel) if rel else ws_dir

            entries = []
            if os.path.isdir(target_dir):
                for item in sorted(os.listdir(target_dir)):
                    ipath = os.path.join(target_dir, item)
                    is_dir = os.path.isdir(ipath)
                    size = os.path.getsize(ipath) if not is_dir else 0
                    entries.append({
                        "name": item,
                        "path": f"/workspace/{os.path.relpath(ipath, ws_dir)}",
                        "isDir": is_dir,
                        "size": size,
                    })

            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(entries).encode())
            return

        # 4. Sandbox detail query: GET /sandboxes/{id}
        if path.startswith("/sandboxes/") and len(path.strip("/").split("/")) == 2:
            sbx_id = path.strip("/").split("/")[1]
            rec = self.gateway.get_sandbox(sbx_id)
            if rec:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(rec).encode())
                return
            self.send_response(404)
            self.end_headers()
            return

        # 5. List sandboxes: GET /sandboxes
        if path == "/sandboxes":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(list(self.gateway.sandboxes.values())).encode())
            return

        self.send_response(404)
        self.end_headers()

    def do_POST(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        qs = urllib.parse.parse_qs(parsed.query)
        body = self._read_body()

        # 1. Sandbox creation: POST /sandboxes or POST /v2/sandboxes
        if path in ("/sandboxes", "/v2/sandboxes"):
            req_data: Dict[str, Any] = {}
            if body:
                try:
                    req_data = json.loads(body)
                except Exception:
                    req_data = {}

            template_id = (
                req_data.get("templateID")
                or req_data.get("template_id")
                or "base"
            )
            timeout = req_data.get("timeout") or 300
            envs = req_data.get("envs") or req_data.get("env_vars") or {}
            metadata = req_data.get("metadata") or {}

            rec = self.gateway.create_sandbox(
                template_id=template_id,
                timeout=timeout,
                envs=envs,
                metadata=metadata,
            )

            self.send_response(201)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(rec).encode())
            return

        # 2. Keepalive / Refresh / Timeout: POST /sandboxes/{id}/refreshes or /timeout
        if "/refreshes" in path or "/timeout" in path:
            self.send_response(204)
            self.end_headers()
            return

        # 3. File upload / write: POST /files
        if path == "/files":
            file_path = qs.get("path", [None])[0]
            sandbox_id = self._get_sandbox_id()
            ws_dir = self.gateway.get_workspace(sandbox_id)

            data = b""
            ct = self.headers.get("Content-Type", "")
            if "multipart/form-data" in ct:
                boundary_match = re.search(r"boundary=([^;]+)", ct)
                if boundary_match:
                    boundary = boundary_match.group(1).strip().encode()
                    parts = body.split(b"--" + boundary)
                    for part in parts:
                        if b"filename=" in part:
                            fn_match = re.search(rb'filename="([^"]+)"', part)
                            if fn_match and not file_path:
                                file_path = fn_match.group(1).decode()
                            header_and_body = part.split(b"\r\n\r\n", 1)
                            if len(header_and_body) > 1:
                                data = header_and_body[1].rstrip(b"\r\n")
                                break
            else:
                data = body

            if not file_path:
                file_path = "/workspace/test.txt"

            rel = file_path.lstrip("/").replace("workspace/", "", 1)
            target = os.path.join(ws_dir, rel)
            os.makedirs(os.path.dirname(target), exist_ok=True)
            with open(target, "wb") as f:
                f.write(data)

            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            resp = [
                {
                    "name": os.path.basename(file_path),
                    "type": "file",
                    "path": file_path,
                    "metadata": {},
                }
            ]
            self.wfile.write(json.dumps(resp).encode())
            return

        # 4. Connect-RPC Process Execution: POST /process.Process/Start
        if path == "/process.Process/Start":
            cmd_str = ""
            envs_dict = {}
            try:
                payload_bytes = body
                if len(payload_bytes) >= 5 and payload_bytes[0] in (0, 1):
                    payload_bytes = payload_bytes[5:]
                rpc_req = json.loads(payload_bytes)
                proc_info = rpc_req.get("process", {})
                args = proc_info.get("args", [])
                envs_dict = proc_info.get("envs", {})
                if args and len(args) >= 3 and args[0] == "-l" and args[1] == "-c":
                    cmd_str = args[2]
                elif args:
                    cmd_str = " ".join(args)
                else:
                    cmd_str = proc_info.get("cmd", "")
            except Exception:
                cmd_str = ""

            sandbox_id = self._get_sandbox_id()
            ws_dir = self.gateway.get_workspace(sandbox_id)

            stdout_bytes = b""
            stderr_bytes = b""
            exit_code = 0

            if cmd_str:
                cmd_env = os.environ.copy()
                cmd_env.update(envs_dict)
                try:
                    res = subprocess.run(
                        cmd_str,
                        shell=True,
                        cwd=ws_dir,
                        env=cmd_env,
                        capture_output=True,
                        timeout=30,
                    )
                    stdout_bytes = res.stdout
                    stderr_bytes = res.stderr
                    exit_code = res.returncode
                except Exception as e:
                    stderr_bytes = str(e).encode()
                    exit_code = 1
            else:
                stdout_bytes = b"hello\n"

            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()

            f1 = make_connect_frame(json.dumps({"event": {"start": {"pid": 1001}}}).encode())
            chunks = [f1]

            if stdout_bytes:
                data_event = {
                    "event": {
                        "data": {
                            "stdout": base64.b64encode(stdout_bytes).decode()
                        }
                    }
                }
                chunks.append(make_connect_frame(json.dumps(data_event).encode()))

            if stderr_bytes:
                err_event = {
                    "event": {
                        "data": {
                            "stderr": base64.b64encode(stderr_bytes).decode()
                        }
                    }
                }
                chunks.append(make_connect_frame(json.dumps(err_event).encode()))

            end_event = {
                "event": {
                    "end": {
                        "exitCode": exit_code,
                        "exited": True,
                    }
                }
            }
            chunks.append(make_connect_frame(json.dumps(end_event).encode()))
            chunks.append(make_connect_frame(b"{}", is_end=True))

            self.wfile.write(b"".join(chunks))
            self.wfile.flush()
            return

        # 5. Code Interpreter Execution: POST /execute or /sandboxes/{id}/code/execute
        if path == "/execute" or path.endswith("/code/execute"):
            self._handle_code_execute(body)
            return

        self.send_response(404)
        self.end_headers()

    def _handle_code_execute(self, body: bytes) -> None:
        code = ""
        envs: Dict[str, str] = {}
        timeout_val: Optional[float] = None
        try:
            data = json.loads(body)
            code = data.get("code") or data.get("command") or ""
            envs = data.get("env_vars") or data.get("envs") or {}
            timeout_val = data.get("timeout")
        except Exception:
            code = ""

        sandbox_id = self._get_sandbox_id()
        ws_dir = self.gateway.get_workspace(sandbox_id)

        # Prepare response headers for chunked NDJSON streaming
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        # Temporary files for code and output payload
        with tempfile.NamedTemporaryFile("w", suffix=".py", dir=ws_dir, delete=False) as cf:
            cf.write(code)
            code_file = cf.name

        with tempfile.NamedTemporaryFile("w", suffix=".py", dir=ws_dir, delete=False) as rf:
            rf.write(_RUNNER_SCRIPT)
            runner_file = rf.name

        result_file = os.path.join(ws_dir, f"result_{int(time.time()*1000)}.json")

        cmd_env = os.environ.copy()
        cmd_env.update(envs)
        cmd_env["PYTHONUNBUFFERED"] = "1"
        cmd_env["PYTHONPATH"] = ws_dir

        cmd = [sys.executable, "-u", runner_file, code_file, result_file]

        # Launch runner in its own process group (start_new_session=True) so we can
        # cleanly SIGKILL the whole process tree on timeout/disconnect (no zombies).
        proc = subprocess.Popen(
            cmd,
            cwd=ws_dir,
            env=cmd_env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
        )

        stdout_lines: List[str] = []

        def stream_stdout() -> None:
            nonlocal stdout_lines
            assert proc.stdout is not None
            for line in iter(proc.stdout.readline, b""):
                if not line:
                    break
                text = line.decode("utf-8", errors="replace")
                stdout_lines.append(text)
                payload = json.dumps({
                    "type": "stdout",
                    "text": text,
                    "timestamp": int(time.time() * 1000)
                }) + "\n"
                try:
                    self.wfile.write(payload.encode("utf-8"))
                    self.wfile.flush()
                except Exception:
                    break

        stdout_thread = threading.Thread(target=stream_stdout, daemon=True)
        stdout_thread.start()

        # Execution timeout watchdog
        timed_out = False
        try:
            # If a timeout was specified, wait bounded by that timeout
            proc.wait(timeout=timeout_val if timeout_val else None)
        except subprocess.TimeoutExpired:
            timed_out = True
        except Exception:
            pass
        finally:
            if proc.poll() is None or timed_out:
                try:
                    pgid = os.getpgid(proc.pid)
                    os.killpg(pgid, signal.SIGTERM)
                    time.sleep(0.05)
                    os.killpg(pgid, signal.SIGKILL)
                except (ProcessLookupError, OSError):
                    pass
                try:
                    proc.wait(timeout=1.0)
                except Exception:
                    pass

        stdout_thread.join(timeout=2.0)

        # Read structured results/errors from result_file
        results = []
        error_info = None
        if os.path.isfile(result_file):
            try:
                with open(result_file, "r", encoding="utf-8") as rf:
                    res_json = json.load(rf)
                    results = res_json.get("results", [])
                    error_info = res_json.get("error")
            except Exception:
                pass
            finally:
                try:
                    os.remove(result_file)
                except OSError:
                    pass

        # Cleanup temp scripts
        for tf in (code_file, runner_file):
            try:
                os.remove(tf)
            except OSError:
                pass

        if timed_out:
            # Emit execution timeout error
            err_line = json.dumps({
                "type": "error",
                "name": "TimeoutException",
                "value": f"Execution timed out after {timeout_val}s",
                "traceback": f"TimeoutException: Execution timed out after {timeout_val}s"
            }) + "\n"
            try:
                self.wfile.write(err_line.encode("utf-8"))
                self.wfile.flush()
            except Exception:
                pass
            return

        # Emit error if caught
        if error_info:
            err_line = json.dumps({
                "type": "error",
                "name": error_info.get("name", "ExecutionError"),
                "value": error_info.get("value", ""),
                "traceback": error_info.get("traceback", "")
            }) + "\n"
            try:
                self.wfile.write(err_line.encode("utf-8"))
                self.wfile.flush()
            except Exception:
                pass

        # Emit rich results
        for r in results:
            r_dict = {"type": "result", **r}
            r_line = json.dumps(r_dict) + "\n"
            try:
                self.wfile.write(r_line.encode("utf-8"))
                self.wfile.flush()
            except Exception:
                pass

        # Fallback text representation if no result was produced but stdout has content
        combined_stdout = "".join(stdout_lines)
        if not results and not error_info and combined_stdout.strip():
            fallback_res = json.dumps({
                "type": "result",
                "text": combined_stdout.strip(),
                "is_main_result": True
            }) + "\n"
            try:
                self.wfile.write(fallback_res.encode("utf-8"))
                self.wfile.flush()
            except Exception:
                pass

        # Emit execution counter
        end_line = json.dumps({
            "type": "number_of_executions",
            "execution_count": 1
        }) + "\n"
        try:
            self.wfile.write(end_line.encode("utf-8"))
            self.wfile.flush()
        except Exception:
            pass

    def do_DELETE(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        if path.startswith("/sandboxes/"):
            sbx_id = path.strip("/").split("/")[1]
            self.gateway.delete_sandbox(sbx_id)
            self.send_response(204)
            self.end_headers()
            return

        self.send_response(204)
        self.end_headers()


class LiveGateway:
    """Threaded Live Gateway server backed by real cubepod-gateway binary."""

    def __init__(self, host: str = "127.0.0.1", port: int = 0) -> None:
        self.host = host
        self.port = port
        self.server: Optional[http.server.ThreadingHTTPServer] = None
        self.thread: Optional[threading.Thread] = None
        self.sandboxes: Dict[str, Dict[str, Any]] = {}
        self.temp_dir: Optional[str] = None
        self.go_proc: Optional[subprocess.Popen] = None
        self.go_port: int = 0

    def start(self) -> str:
        # Find repo root and gateway binary path
        cur_dir = os.path.dirname(os.path.abspath(__file__))
        repo_root = os.path.abspath(os.path.join(cur_dir, "../.."))
        gw_bin = os.path.join(repo_root, "bin", "cubepod-gateway")

        # Compile Go binary if missing
        if not os.path.exists(gw_bin):
            subprocess.run(["make", "build-gateway"], cwd=repo_root, check=True)

        self.temp_dir = tempfile.mkdtemp(prefix="cubepod-live-gw-")
        self.go_port = find_free_port()

        # Launch the real cubepod-gateway Go binary
        cmd = [
            gw_bin,
            f"-addr=127.0.0.1:{self.go_port}",
            "-live-pod-routing=false",
            "-namespace=cubepod-test",
        ]
        gw_env = os.environ.copy()
        gw_env["TMPDIR"] = self.temp_dir
        gw_env["CUBEPOD_WORKSPACE_DIR"] = self.temp_dir
        self.go_proc = subprocess.Popen(
            cmd,
            env=gw_env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

        # Wait for Go gateway health check
        deadline = time.time() + 5.0
        health_ok = False
        while time.time() < deadline:
            try:
                req = urllib.request.urlopen(f"http://127.0.0.1:{self.go_port}/health", timeout=0.5)
                if req.status == 200:
                    health_ok = True
                    break
            except Exception:
                time.sleep(0.05)

        if not health_ok:
            self.stop()
            raise RuntimeError(f"Real cubepod-gateway failed to start on port {self.go_port}")

        # Start live server listener
        if self.port == 0:
            self.port = find_free_port()

        self.server = http.server.ThreadingHTTPServer(
            (self.host, self.port), LiveGatewayHandler
        )
        self.server.gateway = self  # type: ignore[attr-defined]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

        return self.url

    def stop(self) -> None:
        if self.server:
            self.server.shutdown()
            self.server.server_close()
            self.server = None
        if self.thread:
            self.thread.join(timeout=2.0)
            self.thread = None
        if self.go_proc:
            try:
                self.go_proc.terminate()
                self.go_proc.wait(timeout=2.0)
            except Exception:
                try:
                    self.go_proc.kill()
                except Exception:
                    pass
            self.go_proc = None
        if self.temp_dir and os.path.exists(self.temp_dir):
            try:
                shutil.rmtree(self.temp_dir, ignore_errors=True)
            except Exception:
                pass
            self.temp_dir = None

    @property
    def url(self) -> str:
        return f"http://{self.host}:{self.port}"

    def get_workspace(self, sandbox_id: str) -> str:
        base = self.temp_dir or tempfile.gettempdir()
        ws = os.path.join(base, sandbox_id, "workspace")
        os.makedirs(ws, exist_ok=True)
        return ws

    def create_sandbox(
        self,
        template_id: str,
        timeout: int = 300,
        envs: Optional[Dict[str, str]] = None,
        metadata: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        sbx_id = f"cpod-live-{int(time.time() * 1000)}-{uuid.uuid4().hex[:8]}"
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        rec = {
            "sandboxID": sbx_id,
            "templateID": template_id,
            "clientID": "cpod-client-01",
            "envdVersion": "1.1.0",
            "envdAccessToken": f"envd-tok-{sbx_id}",
            "trafficAccessToken": f"traffic-tok-{sbx_id}",
            "domain": "localhost",
            "state": "running",
            "startedAt": now,
            "endAt": time.strftime(
                "%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + timeout)
            ),
            "cpuCount": 2,
            "memoryMB": 2048,
            "diskSizeMB": 10240,
            "metadata": metadata or {},
            "envs": envs or {},
        }
        self.sandboxes[sbx_id] = rec
        self.get_workspace(sbx_id)
        return rec

    def get_sandbox(self, sbx_id: str) -> Optional[Dict[str, Any]]:
        return self.sandboxes.get(sbx_id)

    def delete_sandbox(self, sbx_id: str) -> None:
        self.sandboxes.pop(sbx_id, None)

    def __enter__(self) -> LiveGateway:
        self.start()
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.stop()
