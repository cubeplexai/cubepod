"""Mock E2B Gateway and Data Plane Server for SDK Compatibility Verification.

Conforms to CubePod CEP-0018 specification:
- Official E2B REST Control Plane (/sandboxes, /v2/sandboxes, /refreshes, /timeout)
- Official E2B Connect-RPC Process Data Plane (:49983, /process.Process/Start)
- Official E2B Filesystem REST Data Plane (/files, /files/list)
- Official E2B Code Interpreter Protocol (:49999, /execute NDJSON streaming)
"""

from __future__ import annotations

import base64
import http.server
import json
import os
import re
import struct
import subprocess
import threading
import time
import urllib.parse
from typing import Any, Dict, List, Optional


def make_connect_frame(data: bytes, is_end: bool = False) -> bytes:
    """Pack ConnectRPC envelope frame (1 byte flag + 4 bytes big-endian length + payload)."""
    prefix = 2 if is_end else 0
    return struct.pack(">BI", prefix, len(data)) + data


class MockGatewayHandler(http.server.BaseHTTPRequestHandler):
    """HTTP Request Handler implementing E2B Control & Data Plane endpoints."""

    def log_message(self, format: str, *args: Any) -> None:
        """Suppress default HTTP server logging to stderr."""
        pass

    @property
    def gateway(self) -> MockE2BGateway:
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

    def do_GET(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        qs = urllib.parse.parse_qs(parsed.query)

        # 1. Health check
        if path in ("/health", "/healthz"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status": "healthy"}')
            return

        # 2. File read endpoint: GET /files?path=...
        if path == "/files":
            file_path = qs.get("path", [None])[0]
            sandbox_id = self.headers.get("E2b-Sandbox-Id", "default")
            data = self.gateway.get_file(sandbox_id, file_path)
            if data is None:
                # Fallback default if not explicitly written
                data = b"hello compatibility"

            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return

        # 3. File listing: GET /files/list?path=...
        if path in ("/files/list", "/sandboxes/files/list"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            entries = [
                {"name": "test.txt", "path": "/workspace/test.txt", "isDir": False, "size": 128}
            ]
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
            sandbox_id = self.headers.get("E2b-Sandbox-Id", "default")

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

            self.gateway.save_file(sandbox_id, file_path, data)

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
            try:
                # Strip 5-byte connect header if present in body
                payload_bytes = body
                if len(payload_bytes) >= 5 and payload_bytes[0] in (0, 1):
                    payload_bytes = payload_bytes[5:]
                rpc_req = json.loads(payload_bytes)
                proc_info = rpc_req.get("process", {})
                args = proc_info.get("args", [])
                if args and len(args) >= 3 and args[0] == "-l" and args[1] == "-c":
                    cmd_str = args[2]
                elif args:
                    cmd_str = " ".join(args)
                else:
                    cmd_str = proc_info.get("cmd", "")
            except Exception:
                cmd_str = ""

            # Execute command or evaluate echo
            stdout_bytes = b""
            stderr_bytes = b""
            exit_code = 0

            if cmd_str:
                try:
                    res = subprocess.run(
                        cmd_str,
                        shell=True,
                        capture_output=True,
                        timeout=15,
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
            code = ""
            try:
                data = json.loads(body)
                code = data.get("code", "")
            except Exception:
                code = ""

            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.end_headers()

            lines: List[str] = []
            if "matplotlib" in code or "plt." in code:
                lines.append(
                    json.dumps(
                        {
                            "type": "result",
                            "text": "<Figure size 640x480 with 1 Axes>",
                            "png": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
                            "is_main_result": True,
                        }
                    )
                )
            elif "1 / 0" in code or "ZeroDivision" in code:
                lines.append(
                    json.dumps(
                        {
                            "type": "error",
                            "name": "ZeroDivisionError",
                            "value": "division by zero",
                            "traceback": 'Traceback (most recent call last):\n  File "<stdin>", line 1, in <module>\nZeroDivisionError: division by zero',
                        }
                    )
                )
            else:
                lines.append(
                    json.dumps(
                        {
                            "type": "stdout",
                            "text": "2\n",
                            "timestamp": int(time.time() * 1000),
                        }
                    )
                )
                lines.append(
                    json.dumps(
                        {
                            "type": "result",
                            "text": "2",
                            "is_main_result": True,
                        }
                    )
                )

            lines.append(
                json.dumps(
                    {
                        "type": "number_of_executions",
                        "execution_count": 1,
                    }
                )
            )

            payload = "\n".join(lines) + "\n"
            self.wfile.write(payload.encode())
            self.wfile.flush()
            return

        self.send_response(404)
        self.end_headers()

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


class MockE2BGateway:
    """Threaded Mock Gateway implementing official E2B wire protocols."""

    def __init__(self, host: str = "127.0.0.1", port: int = 0) -> None:
        self.host = host
        self.port = port
        self.server: Optional[http.server.ThreadingHTTPServer] = None
        self.thread: Optional[threading.Thread] = None
        self.sandboxes: Dict[str, Dict[str, Any]] = {}
        self.files: Dict[str, Dict[str, bytes]] = {}

    def start(self) -> str:
        self.server = http.server.ThreadingHTTPServer(
            (self.host, self.port), MockGatewayHandler
        )
        self.server.gateway = self  # type: ignore[attr-defined]
        self.port = self.server.server_address[1]
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

    @property
    def url(self) -> str:
        return f"http://{self.host}:{self.port}"

    def create_sandbox(
        self,
        template_id: str,
        timeout: int = 300,
        envs: Optional[Dict[str, str]] = None,
        metadata: Optional[Dict[str, str]] = None,
    ) -> Dict[str, Any]:
        sbx_id = f"cpod-mock-{int(time.time() * 1000)}"
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
        self.files[sbx_id] = {}
        return rec

    def get_sandbox(self, sbx_id: str) -> Optional[Dict[str, Any]]:
        return self.sandboxes.get(sbx_id)

    def delete_sandbox(self, sbx_id: str) -> None:
        self.sandboxes.pop(sbx_id, None)
        self.files.pop(sbx_id, None)

    def save_file(self, sbx_id: str, path: str, data: bytes) -> None:
        if sbx_id not in self.files:
            self.files[sbx_id] = {}
        self.files[sbx_id][path] = data
        self.files.setdefault("default", {})[path] = data

    def get_file(self, sbx_id: str, path: Optional[str]) -> Optional[bytes]:
        if not path:
            return None
        if sbx_id in self.files and path in self.files[sbx_id]:
            return self.files[sbx_id][path]
        return self.files.get("default", {}).get(path)

    def __enter__(self) -> MockE2BGateway:
        self.start()
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.stop()
