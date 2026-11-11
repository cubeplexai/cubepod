"""Async client for CubePod Control & Data Plane."""

import asyncio
from collections.abc import AsyncIterator
from typing import Any

import httpx

from .models import (
    CommandInfo,
    CommandLogs,
    ExecutionResult,
    FileEntry,
    PanelEndpoint,
    SandboxInfo,
    _build_tar_gz_bytes,
)


class AsyncCubePod:
    """Async client for CubePod Control & Data Plane."""

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        api_key: str | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        max_retries: int = 2,
    ) -> None:
        headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._max_retries = max_retries
        self._client = httpx.AsyncClient(
            base_url=base_url.rstrip("/"),
            headers=headers,
            timeout=60.0,
            transport=transport,
        )

    async def __aenter__(self) -> "AsyncCubePod":
        return self

    async def __aexit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        await self.close()

    async def _request(
        self,
        method: str,
        url: str,
        *,
        params: dict[str, Any] | None = None,
        json: Any = None,
        content: bytes | None = None,
        headers: dict[str, str] | None = None,
    ) -> httpx.Response:
        retries = 0
        while True:
            resp = await self._client.request(
                method,
                url,
                params=params,
                json=json,
                content=content,
                headers=headers,
            )
            if resp.status_code in (502, 503, 504) and retries < self._max_retries:
                retries += 1
                await asyncio.sleep(0.01 * (2 ** (retries - 1)))
                continue
            return resp

    async def create(
        self,
        template_id: str = "cubeplex-ubuntu24-desktop",
        *,
        volume_key: str | None = None,
        metadata: dict[str, str] | None = None,
        network_policy: dict[str, Any] | None = None,
        secret_bindings: list[dict[str, Any]] | None = None,
    ) -> SandboxInfo:
        payload: dict[str, Any] = {"templateID": template_id, "metadata": metadata or {}}
        if volume_key:
            payload["extension"] = {"volumeKey": volume_key}
        if network_policy is not None:
            payload["networkPolicy"] = network_policy
        if secret_bindings is not None:
            if "networkPolicy" not in payload:
                payload["networkPolicy"] = {}
            payload["networkPolicy"]["secretBindings"] = secret_bindings
        resp = await self._request("POST", "/sandboxes", json=payload)
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def list(self, template_id: str | None = None) -> list[SandboxInfo]:
        """List all managed sandboxes, optionally filtered by template ID."""
        params = {"templateID": template_id} if template_id else None
        resp = await self._request("GET", "/sandboxes", params=params)
        resp.raise_for_status()
        data = resp.json()
        items = data if isinstance(data, list) else data.get("sandboxes", [])
        return [self._parse_info(item) for item in items]

    async def get(self, sandbox_id: str) -> SandboxInfo:
        """Fetch status and metadata for an existing sandbox."""
        resp = await self._request("GET", f"/sandboxes/{sandbox_id}")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def connect(self, sandbox_id: str) -> SandboxInfo:
        """Connect to a sandbox, automatically resuming it if Paused."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/connect")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def pause(self, sandbox_id: str) -> SandboxInfo:
        """Suspend compute while retaining RootFS Upper and Workspace block PVCs."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/pause")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def reset_rootfs(self, sandbox_id: str) -> SandboxInfo:
        """Reset the persistent RootFS Upper volume while preserving /workspace."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/reset-rootfs")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def fork(self, sandbox_id: str) -> SandboxInfo:
        """Clone both RootFS and Workspace volumes in ~1s via Copy-on-Write."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/fork")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def delete(self, sandbox_id: str) -> None:
        """Terminate and permanently delete a sandbox."""
        resp = await self._request("DELETE", f"/sandboxes/{sandbox_id}")
        resp.raise_for_status()

    async def renew_timeout(self, sandbox_id: str, timeout_seconds: int = 3600) -> dict[str, Any]:
        """Renew the idle auto-pause timeout for a running sandbox."""
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/timeout",
            json={"timeoutSeconds": timeout_seconds},
        )
        resp.raise_for_status()
        return resp.json()

    async def acquire_lease(self, sandbox_id: str, lease_seconds: int = 300) -> dict[str, Any]:
        """Acquire or renew an exclusive active lease on the sandbox."""
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/lease",
            json={"leaseSeconds": lease_seconds},
        )
        resp.raise_for_status()
        return resp.json()

    async def get_panel_endpoint(
        self,
        sandbox_id: str,
        port: int = 8080,
        *,
        expires_in_seconds: int = 3600,
        target_url: str | None = None,
    ) -> PanelEndpoint:
        """Request an HMAC-SHA256 signed panel proxy URL for browser live-view or web terminal."""
        payload: dict[str, Any] = {"expiresInSeconds": expires_in_seconds}
        if target_url is not None:
            payload["targetUrl"] = target_url
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/endpoints/{port}",
            json=payload,
        )
        resp.raise_for_status()
        data = resp.json()
        return PanelEndpoint(
            url=data["url"],
            token=data["token"],
            expires_at=data.get("expiresAt"),
        )

    async def execute(
        self,
        sandbox_id: str,
        command: str,
        *,
        workdir: str | None = None,
        envs: dict[str, str] | None = None,
        uid: int = 1000,
        gid: int = 1000,
    ) -> ExecutionResult:
        """Execute a foreground command and wait for completion."""
        payload: dict[str, Any] = {
            "command": command,
            "background": False,
            "uid": uid,
            "gid": gid,
        }
        if workdir is not None:
            payload["workdir"] = workdir
        if envs is not None:
            payload["envs"] = envs

        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/commands/run", json=payload)
        resp.raise_for_status()
        data = resp.json()
        exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
        output = data.get("output", "") or data.get("stdout", "")
        cmd_id = data.get("id") or data.get("cmdID", "")
        return ExecutionResult(
            id=cmd_id,
            exit_code=exit_code,
            output=output,
            running=data.get("running", False),
            pid=data.get("pid"),
            cursor=data.get("cursor", 0),
        )

    async def start_command(
        self,
        sandbox_id: str,
        command: str,
        *,
        workdir: str | None = None,
        envs: dict[str, str] | None = None,
        uid: int = 1000,
        gid: int = 1000,
    ) -> CommandInfo:
        """Launch an asynchronous background command."""
        payload: dict[str, Any] = {
            "command": command,
            "background": True,
            "uid": uid,
            "gid": gid,
        }
        if workdir is not None:
            payload["workdir"] = workdir
        if envs is not None:
            payload["envs"] = envs

        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/commands/run", json=payload)
        resp.raise_for_status()
        data = resp.json()
        cmd_id = data.get("id") or data.get("cmdID", "")
        exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
        output = data.get("output", "") or data.get("stdout", "")
        return CommandInfo(
            id=cmd_id,
            pid=data.get("pid"),
            running=data.get("running", True),
            exit_code=exit_code,
            cursor=data.get("cursor", 0),
            output=output,
        )

    async def get_command_status(
        self,
        sandbox_id: str,
        command_id: str,
    ) -> CommandInfo:
        """Get the latest process status of a background command."""
        resp = await self._request("GET", f"/sandboxes/{sandbox_id}/commands/{command_id}/status")
        resp.raise_for_status()
        data = resp.json()
        cmd_id = data.get("id") or data.get("cmdID", command_id)
        exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
        output = data.get("output", "") or data.get("stdout", "")
        return CommandInfo(
            id=cmd_id,
            pid=data.get("pid"),
            running=data.get("running", False),
            exit_code=exit_code,
            cursor=data.get("cursor", 0),
            output=output,
        )

    async def poll_command(
        self,
        sandbox_id: str,
        command_id: str,
        *,
        cursor: int = 0,
    ) -> CommandLogs:
        """Read incremental stdout/stderr output from a background command using byte-offset cursor."""
        resp = await self._request(
            "GET",
            f"/sandboxes/{sandbox_id}/commands/{command_id}/logs",
            params={"cursor": cursor},
        )
        resp.raise_for_status()
        data = resp.json()
        cmd_id = data.get("id") or data.get("cmdID", command_id)
        exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
        return CommandLogs(
            id=cmd_id,
            content=data.get("content", ""),
            cursor=data.get("cursor", cursor),
            running=data.get("running", False),
            exit_code=exit_code,
        )

    async def stream_command(
        self,
        sandbox_id: str,
        command_id: str,
        *,
        poll_interval: float = 0.05,
    ) -> AsyncIterator[CommandLogs]:
        """Stream incremental logs from a background command until execution terminates."""
        cursor = 0
        while True:
            logs = await self.poll_command(sandbox_id, command_id, cursor=cursor)
            cursor = logs.cursor
            yield logs
            if not logs.running:
                break
            await asyncio.sleep(poll_interval)

    async def kill_command(self, sandbox_id: str, command_id: str) -> dict[str, Any]:
        """Terminate a background command process group."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/commands/{command_id}/kill")
        resp.raise_for_status()
        return resp.json()

    async def write_file(
        self,
        sandbox_id: str,
        path: str,
        content: str | bytes,
        *,
        uid: int = 1000,
        gid: int = 1000,
        mode: str = "0644",
    ) -> dict[str, Any]:
        """Write a file directly inside the sandbox with specified UID/GID and mode."""
        data = content.encode("utf-8") if isinstance(content, str) else content
        resp = await self._request(
            "PUT",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path, "uid": str(uid), "gid": str(gid), "mode": mode},
            content=data,
            headers={"Content-Type": "application/octet-stream"},
        )
        resp.raise_for_status()
        return resp.json()

    async def read_file(self, sandbox_id: str, path: str) -> bytes:
        """Read a file from inside the sandbox."""
        resp = await self._request(
            "GET",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        return resp.content

    async def delete_file(self, sandbox_id: str, path: str) -> dict[str, Any]:
        """Delete a file or directory from inside the sandbox."""
        resp = await self._request(
            "DELETE",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        try:
            return resp.json()
        except Exception:
            return {"status": "ok", "path": path}

    async def list_files(self, sandbox_id: str, path: str = "/workspace") -> list[FileEntry]:
        """List files and directories at path inside the sandbox."""
        resp = await self._request(
            "GET",
            f"/sandboxes/{sandbox_id}/files/list",
            params={"path": path},
        )
        resp.raise_for_status()
        data = resp.json()
        entries = data if isinstance(data, list) else data.get("entries", [])
        return [
            FileEntry(
                name=e.get("name", ""),
                path=e.get("path", ""),
                is_dir=e.get("is_dir", False),
                size=e.get("size", 0),
                mode=e.get("mode", "0644"),
            )
            for e in entries
        ]

    async def archive_sync(
        self,
        sandbox_id: str,
        files: dict[str, str | bytes],
        *,
        dest: str = "/workspace",
        uid: int = 1000,
        gid: int = 1000,
    ) -> dict[str, Any]:
        """Pack files into an in-memory tar.gz archive and extract inside the sandbox in a single HTTP request."""
        tar_bytes = _build_tar_gz_bytes(files, uid=uid, gid=gid)

        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/files/archive-sync",
            params={"dest": dest},
            content=tar_bytes,
            headers={"Content-Type": "application/gzip"},
        )
        resp.raise_for_status()
        return resp.json()

    async def create_warmpool(
        self,
        name: str,
        template_id: str = "cubeplex-ubuntu24-desktop",
        replicas: int = 2,
    ) -> dict[str, Any]:
        """Create or scale a PodWarmPool for standby MicroVM allocation."""
        payload = {"name": name, "templateID": template_id, "replicas": replicas}
        resp = await self._request("POST", "/warmpools", json=payload)
        resp.raise_for_status()
        return resp.json()

    async def list_warmpools(self) -> list[dict[str, Any]]:
        """List all active PodWarmPools and their standby replica counts."""
        resp = await self._request("GET", "/warmpools")
        resp.raise_for_status()
        data = resp.json()
        return data if isinstance(data, list) else data.get("warmpools", [])

    async def list_templates(self) -> list[dict[str, Any]]:
        """List available sandbox templates."""
        resp = await self._request("GET", "/templates")
        resp.raise_for_status()
        data = resp.json()
        return data if isinstance(data, list) else data.get("templates", [])

    async def close(self) -> None:
        """Close the underlying HTTP client session."""
        await self._client.aclose()

    @staticmethod
    def _parse_info(data: dict[str, Any]) -> SandboxInfo:
        return SandboxInfo(
            sandbox_id=data["sandboxID"],
            template_id=data["templateID"],
            state=data["state"],
            envd_port=data.get("envdPort", 49983),
            rootfs_generation=data.get("rootfsGeneration", 1),
        )
