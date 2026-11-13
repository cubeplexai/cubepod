"""Async client for CubePod Control & Data Plane."""

from __future__ import annotations

import asyncio
import builtins
from collections.abc import AsyncIterator, Sequence
import time
from typing import Any
from urllib.parse import quote

import httpx

from ._helpers import (
    _build_archive_sync_request,
    _build_command_payload,
    _build_create_payload,
    _build_desktop_start_payload,
    _build_fork_payload,
    _build_template_register_payload,
    _build_template_result,
    _format_build_error,
    _json_dict,
    _json_list,
    _normalize_base_url,
    _parse_command_info,
    _parse_command_logs,
    _parse_desktop_session,
    _parse_execution_result,
    _parse_file_entries,
    _parse_info,
    _parse_panel_endpoint,
)
from .exceptions import CubePodFileTooLargeError
from .models import (
    CommandInfo,
    CommandLogs,
    DesktopSession,
    ExecutionResult,
    FileEntry,
    PanelEndpoint,
    SandboxInfo,
)


class AsyncCubePod:
    """Async client for CubePod Control & Data Plane."""

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        api_key: str | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
        max_retries: int = 2,
        timeout: float = 60.0,
        trust_env: bool = False,
    ) -> None:
        headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._max_retries = max_retries
        self._client = httpx.AsyncClient(
            base_url=_normalize_base_url(base_url),
            headers=headers,
            timeout=timeout,
            trust_env=trust_env,
            transport=transport,
        )

    async def __aenter__(self) -> AsyncCubePod:
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
        timeout: float | None = None,
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
                timeout=timeout if timeout is not None else httpx.USE_CLIENT_DEFAULT,
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
        secret_bindings: builtins.list[dict[str, Any]] | None = None,
        timeout_seconds: int | None = None,
        warm_pool_ref: str | None = None,
        resource: dict[str, str] | None = None,
    ) -> SandboxInfo:
        """Create a new persistent sandbox from a template."""
        payload = _build_create_payload(
            template_id,
            volume_key=volume_key,
            metadata=metadata,
            network_policy=network_policy,
            secret_bindings=secret_bindings,
            timeout_seconds=timeout_seconds,
            warm_pool_ref=warm_pool_ref,
            resource=resource,
        )
        resp = await self._request("POST", "/sandboxes", json=payload)
        resp.raise_for_status()
        return _parse_info(_json_dict(resp))

    async def list(self, template_id: str | None = None) -> builtins.list[SandboxInfo]:
        """List all managed sandboxes, optionally filtered by template ID."""
        params = {"templateID": template_id} if template_id else None
        resp = await self._request("GET", "/sandboxes", params=params)
        resp.raise_for_status()
        return [_parse_info(item) for item in _json_list(resp, "sandboxes")]

    async def get(self, sandbox_id: str) -> SandboxInfo:
        """Fetch status and metadata for an existing sandbox."""
        return _parse_info(await self.get_raw(sandbox_id))

    async def get_raw(self, sandbox_id: str) -> dict[str, Any]:
        """Fetch the raw JSON payload for an existing sandbox."""
        resp = await self._request("GET", f"/sandboxes/{sandbox_id}")
        resp.raise_for_status()
        return _json_dict(resp)

    async def connect(self, sandbox_id: str) -> SandboxInfo:
        """Connect to a sandbox, automatically resuming it if Paused."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/connect")
        resp.raise_for_status()
        return _parse_info(_json_dict(resp))

    async def pause(self, sandbox_id: str) -> SandboxInfo:
        """Suspend compute while retaining RootFS Upper and Workspace block PVCs."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/pause")
        resp.raise_for_status()
        return _parse_info(_json_dict(resp))

    async def reset_rootfs(self, sandbox_id: str) -> SandboxInfo:
        """Reset the persistent RootFS Upper volume while preserving /workspace."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/reset-rootfs")
        resp.raise_for_status()
        return _parse_info(_json_dict(resp))

    async def fork(
        self,
        sandbox_id: str,
        *,
        volume_key: str | None = None,
        metadata: dict[str, str] | None = None,
    ) -> SandboxInfo:
        """Clone both RootFS and Workspace volumes in ~1s via Copy-on-Write."""
        payload = _build_fork_payload(volume_key=volume_key, metadata=metadata)
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/fork",
            json=payload,
        )
        resp.raise_for_status()
        return _parse_info(_json_dict(resp))

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
        return _json_dict(resp)

    async def acquire_lease(self, sandbox_id: str, lease_seconds: int = 300) -> dict[str, Any]:
        """Acquire or renew an exclusive active lease on the sandbox."""
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/lease",
            json={"leaseSeconds": lease_seconds},
        )
        resp.raise_for_status()
        return _json_dict(resp)

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
        return _parse_panel_endpoint(_json_dict(resp))

    async def start_desktop(
        self,
        sandbox_id: str,
        *,
        resolution: str | None = None,
        initial_url: str | None = None,
        expires_in_seconds: int = 3600,
    ) -> DesktopSession:
        """Start or attach to the interactive desktop stack inside the sandbox."""
        payload = _build_desktop_start_payload(
            resolution=resolution,
            initial_url=initial_url,
            expires_in_seconds=expires_in_seconds,
        )
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/desktop/start",
            json=payload,
        )
        resp.raise_for_status()
        return _parse_desktop_session(_json_dict(resp))

    async def get_desktop_status(self, sandbox_id: str) -> dict[str, Any]:
        """Fetch interactive desktop daemon status for a sandbox."""
        resp = await self._request("GET", f"/sandboxes/{sandbox_id}/desktop/status")
        resp.raise_for_status()
        return _json_dict(resp)

    async def stop_desktop(self, sandbox_id: str) -> dict[str, Any]:
        """Stop the interactive desktop stack inside a sandbox."""
        resp = await self._request("POST", f"/sandboxes/{sandbox_id}/desktop/stop")
        resp.raise_for_status()
        if resp.content:
            return _json_dict(resp)
        return {"status": "stopped"}

    async def execute(
        self,
        sandbox_id: str,
        command: str,
        *,
        workdir: str | None = None,
        envs: dict[str, str] | None = None,
        uid: int | None = 1000,
        gid: int | None = 1000,
        timeout: float | None = None,
    ) -> ExecutionResult:
        """Execute a foreground command and wait for completion."""
        payload = _build_command_payload(
            command,
            background=False,
            workdir=workdir,
            envs=envs,
            uid=uid,
            gid=gid,
        )
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/commands/run",
            json=payload,
            timeout=timeout,
        )
        resp.raise_for_status()
        return _parse_execution_result(_json_dict(resp))

    async def start_command(
        self,
        sandbox_id: str,
        command: str,
        *,
        workdir: str | None = None,
        envs: dict[str, str] | None = None,
        uid: int | None = 1000,
        gid: int | None = 1000,
        timeout: float | None = None,
    ) -> CommandInfo:
        """Launch an asynchronous background command."""
        payload = _build_command_payload(
            command,
            background=True,
            workdir=workdir,
            envs=envs,
            uid=uid,
            gid=gid,
        )
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/commands/run",
            json=payload,
            timeout=timeout,
        )
        resp.raise_for_status()
        return _parse_command_info(_json_dict(resp), default_running=True)

    async def get_command_status(
        self,
        sandbox_id: str,
        command_id: str,
    ) -> CommandInfo:
        """Get the latest process status of a background command."""
        resp = await self._request("GET", f"/sandboxes/{sandbox_id}/commands/{command_id}/status")
        resp.raise_for_status()
        return _parse_command_info(_json_dict(resp), default_id=command_id, default_running=False)

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
        return _parse_command_logs(_json_dict(resp), command_id=command_id, default_cursor=cursor)

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
        return _json_dict(resp)

    async def write_file(
        self,
        sandbox_id: str,
        path: str,
        content: str | bytes,
        *,
        uid: int | None = None,
        gid: int | None = None,
        mode: str = "0644",
    ) -> dict[str, Any]:
        """Write a file directly inside the sandbox with optional UID/GID and mode."""
        data = content.encode("utf-8") if isinstance(content, str) else content
        params: dict[str, str] = {"path": path, "mode": mode}
        if uid is not None:
            params["uid"] = str(uid)
        if gid is not None:
            params["gid"] = str(gid)
        resp = await self._request(
            "PUT",
            f"/sandboxes/{sandbox_id}/files",
            params=params,
            content=data,
            headers={"Content-Type": "application/octet-stream"},
        )
        resp.raise_for_status()
        return _json_dict(resp)

    async def read_file(self, sandbox_id: str, path: str) -> bytes:
        """Read a file from inside the sandbox."""
        resp = await self._request(
            "GET",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        return resp.content

    async def stream_file(
        self,
        sandbox_id: str,
        path: str,
        *,
        chunk_size: int = 65536,
    ) -> AsyncIterator[bytes]:
        """Stream raw bytes of a file from inside the sandbox."""
        retries = 0
        while True:
            req = self._client.build_request(
                "GET",
                f"/sandboxes/{sandbox_id}/files",
                params={"path": path},
            )
            resp = await self._client.send(req, stream=True)
            if resp.status_code in (502, 503, 504) and retries < self._max_retries:
                await resp.aclose()
                retries += 1
                await asyncio.sleep(0.01 * (2 ** (retries - 1)))
                continue
            break

        try:
            resp.raise_for_status()
        except Exception:
            await resp.aclose()
            raise

        async def _iter() -> AsyncIterator[bytes]:
            try:
                async for chunk in resp.aiter_bytes(chunk_size=chunk_size):
                    yield chunk
            finally:
                await resp.aclose()

        return _iter()

    async def read_text_file(
        self,
        sandbox_id: str,
        path: str,
        *,
        max_bytes: int | None = None,
        encoding: str = "utf-8",
    ) -> str:
        """Stream and decode a text file, raising CubePodFileTooLargeError if max_bytes is exceeded."""
        if max_bytes is not None and max_bytes < 0:
            raise ValueError("max_bytes must be non-negative")
        stream = await self.stream_file(sandbox_id, path)
        chunks: builtins.list[bytes] = []
        total = 0
        aclose = getattr(stream, "aclose", None)
        try:
            async for chunk in stream:
                total += len(chunk)
                if max_bytes is not None and total > max_bytes:
                    raise CubePodFileTooLargeError(
                        f"File {path!r} exceeds max_bytes limit ({total} > {max_bytes})"
                    )
                chunks.append(chunk)
        finally:
            if aclose is not None:
                await aclose()
        return b"".join(chunks).decode(encoding)

    async def delete_file(self, sandbox_id: str, path: str) -> dict[str, Any]:
        """Delete a file or directory from inside the sandbox."""
        resp = await self._request(
            "DELETE",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        try:
            return _json_dict(resp)
        except Exception:
            return {"status": "ok", "path": path}

    async def list_files(self, sandbox_id: str, path: str = "/workspace") -> builtins.list[FileEntry]:
        """List files and directories at path inside the sandbox."""
        resp = await self._request(
            "GET",
            f"/sandboxes/{sandbox_id}/files/list",
            params={"path": path},
        )
        resp.raise_for_status()
        return _parse_file_entries(resp.json())

    async def archive_sync(
        self,
        sandbox_id: str,
        files: dict[str, str | bytes] | bytes,
        *,
        dest: str = "/workspace",
        uid: int | None = None,
        gid: int | None = None,
        remove_paths: Sequence[str] | None = None,
    ) -> dict[str, Any]:
        """Upload a tar.gz archive (dict or prebuilt bytes) and extract it inside the sandbox."""
        tar_bytes, params = _build_archive_sync_request(
            files,
            dest=dest,
            uid=uid,
            gid=gid,
            remove_paths=remove_paths,
        )
        resp = await self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/files/archive-sync",
            params=params,
            content=tar_bytes,
            headers={"Content-Type": "application/gzip"},
        )
        resp.raise_for_status()
        return _json_dict(resp)

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
        return _json_dict(resp)

    async def list_warmpools(self) -> builtins.list[dict[str, Any]]:
        """List all active PodWarmPools and their standby replica counts."""
        resp = await self._request("GET", "/warmpools")
        resp.raise_for_status()
        return _json_list(resp, "warmpools")

    async def list_templates(self) -> builtins.list[dict[str, Any]]:
        """List available sandbox templates."""
        resp = await self._request("GET", "/templates")
        resp.raise_for_status()
        return _json_list(resp, "templates")

    async def get_template(self, template_id: str) -> dict[str, Any]:
        """Fetch status and metadata for a specific template."""
        resp = await self._request("GET", f"/templates/{quote(template_id, safe='')}")
        resp.raise_for_status()
        return _json_dict(resp)

    async def get_template_by_alias(self, alias: str) -> dict[str, Any]:
        """Fetch template metadata by alias name or tag."""
        resp = await self._request("GET", f"/templates/aliases/{quote(alias, safe='')}")
        resp.raise_for_status()
        return _json_dict(resp)

    async def get_template_build_status(
        self,
        template_id: str,
        build_id: str,
        *,
        logs_offset: int | None = None,
        limit: int | None = None,
        level: str | None = None,
    ) -> dict[str, Any]:
        """Fetch build status and incremental logs for a template build."""
        params: dict[str, Any] = {}
        if logs_offset is not None:
            params["logsOffset"] = logs_offset
        if limit is not None:
            params["limit"] = limit
        if level is not None:
            params["level"] = level
        resp = await self._request(
            "GET",
            f"/templates/{quote(template_id, safe='')}/builds/{quote(build_id, safe='')}/status",
            params=params or None,
        )
        resp.raise_for_status()
        return _json_dict(resp)

    async def create_template_from_image(
        self,
        from_image: str,
        alias: str | None = None,
        name: str | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        min_free_disk_mb: int | None = None,
        tags: builtins.list[str] | None = None,
        from_image_registry: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
        wait: bool = True,
        poll_interval: float = 0.5,
        timeout: float = 300.0,
    ) -> dict[str, Any]:
        """Register a template from an existing OCI image without invoking BuildKit."""
        build_payload: dict[str, Any] = {
            "fromImage": from_image,
            "steps": [],
        }
        if from_image_registry is not None:
            build_payload["fromImageRegistry"] = from_image_registry

        return await self._create_and_start_template_build(
            name=name,
            alias=alias,
            tags=tags,
            cpu_count=cpu_count,
            memory_mb=memory_mb,
            min_free_disk_mb=min_free_disk_mb,
            idempotency_key=idempotency_key,
            build_payload=build_payload,
            wait=wait,
            poll_interval=poll_interval,
            timeout=timeout,
        )

    async def delete_template(self, template_id: str, force: bool = False) -> None:
        """Delete a template by templateID. If force is True, appends ?force=true."""
        params = {"force": "true"} if force else None
        resp = await self._request("DELETE", f"/templates/{quote(template_id, safe='')}", params=params)
        resp.raise_for_status()

    async def set_template_public(self, template_id: str, public: bool = True) -> dict[str, Any] | None:
        """Update template visibility via PATCH /templates/{templateID}."""
        resp = await self._request(
            "PATCH",
            f"/templates/{quote(template_id, safe='')}",
            json={"public": public},
        )
        resp.raise_for_status()
        if resp.content:
            return _json_dict(resp)
        return None

    async def build_template(
        self,
        name: str | None = None,
        alias: str | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        min_free_disk_mb: int | None = None,
        from_image: str | None = None,
        from_template: str | None = None,
        steps: builtins.list[dict[str, Any]] | None = None,
        start_cmd: str | None = None,
        ready_cmd: str | None = None,
        force: bool = False,
        tags: builtins.list[str] | None = None,
        from_image_registry: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
        wait: bool = True,
        poll_interval: float = 0.5,
        timeout: float = 300.0,
    ) -> dict[str, Any]:
        """Thin helper for Beta template builds: registers via POST /v3/templates,
        triggers build via POST /v2/templates/{templateID}/builds/{buildID}, and polls status when wait=True.
        """
        build_payload: dict[str, Any] = {
            "steps": steps if steps is not None else [],
        }
        if from_image is not None:
            build_payload["fromImage"] = from_image
        if from_template is not None:
            build_payload["fromTemplate"] = from_template
        if from_image_registry is not None:
            build_payload["fromImageRegistry"] = from_image_registry
        if start_cmd is not None:
            build_payload["startCmd"] = start_cmd
        if ready_cmd is not None:
            build_payload["readyCmd"] = ready_cmd
        if force:
            build_payload["force"] = True

        return await self._create_and_start_template_build(
            name=name,
            alias=alias,
            tags=tags,
            cpu_count=cpu_count,
            memory_mb=memory_mb,
            min_free_disk_mb=min_free_disk_mb,
            idempotency_key=idempotency_key,
            build_payload=build_payload,
            wait=wait,
            poll_interval=poll_interval,
            timeout=timeout,
        )

    async def _create_and_start_template_build(
        self,
        *,
        name: str | None,
        alias: str | None,
        tags: builtins.list[str] | None,
        cpu_count: int | None,
        memory_mb: int | None,
        min_free_disk_mb: int | None,
        idempotency_key: str | None,
        build_payload: dict[str, Any],
        wait: bool,
        poll_interval: float,
        timeout: float,
    ) -> dict[str, Any]:
        payload = _build_template_register_payload(
            name=name,
            alias=alias,
            tags=tags,
            cpu_count=cpu_count,
            memory_mb=memory_mb,
            min_free_disk_mb=min_free_disk_mb,
        )
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else None
        resp = await self._request("POST", "/v3/templates", json=payload, headers=headers)
        resp.raise_for_status()
        created = _json_dict(resp)

        template_id = str(created.get("templateID", ""))
        build_id = str(created.get("buildID", ""))

        build_resp = await self._request(
            "POST",
            f"/v2/templates/{quote(template_id, safe='')}/builds/{quote(build_id, safe='')}",
            json=build_payload,
        )
        build_resp.raise_for_status()

        if wait:
            await self._wait_for_template_build(
                template_id,
                build_id,
                poll_interval=poll_interval,
                timeout=timeout,
            )

        return _build_template_result(created)

    async def _wait_for_template_build(
        self,
        template_id: str,
        build_id: str,
        *,
        poll_interval: float = 0.5,
        timeout: float = 300.0,
    ) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while True:
            status_data = await self.get_template_build_status(template_id, build_id)
            status = status_data.get("status", "")
            if status == "ready":
                return status_data
            if status == "error":
                raise RuntimeError(_format_build_error(build_id, status_data.get("reason")))
            if status not in ("waiting", "building", "uploaded", ""):
                raise RuntimeError(f"Unexpected build status {status!r} for build {build_id}")
            if time.monotonic() >= deadline:
                raise TimeoutError(f"Timed out waiting for template build {build_id}")
            await asyncio.sleep(poll_interval)

    async def close(self) -> None:
        """Close the underlying HTTP client session."""
        await self._client.aclose()

    @staticmethod
    def _parse_info(data: dict[str, Any]) -> SandboxInfo:
        return _parse_info(data)
