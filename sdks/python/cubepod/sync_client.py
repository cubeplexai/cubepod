"""Synchronous client for CubePod Control & Data Plane."""

from __future__ import annotations

from collections.abc import Iterator
import time
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


class CubePod:
    """Synchronous client for CubePod Control & Data Plane."""

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        api_key: str | None = None,
        transport: httpx.BaseTransport | None = None,
        max_retries: int = 2,
    ) -> None:
        headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._max_retries = max_retries
        self._client = httpx.Client(
            base_url=base_url.rstrip("/"),
            headers=headers,
            timeout=60.0,
            transport=transport,
        )

    def __enter__(self) -> "CubePod":
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.close()

    def _request(
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
            resp = self._client.request(
                method,
                url,
                params=params,
                json=json,
                content=content,
                headers=headers,
            )
            if resp.status_code in (502, 503, 504) and retries < self._max_retries:
                retries += 1
                time.sleep(0.01 * (2 ** (retries - 1)))
                continue
            return resp

    def create(
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
        resp = self._request("POST", "/sandboxes", json=payload)
        resp.raise_for_status()
        return self._parse_info(resp.json())

    def list(self, template_id: str | None = None) -> list[SandboxInfo]:
        """List all managed sandboxes, optionally filtered by template ID."""
        params = {"templateID": template_id} if template_id else None
        resp = self._request("GET", "/sandboxes", params=params)
        resp.raise_for_status()
        data = resp.json()
        items = data if isinstance(data, list) else data.get("sandboxes", [])
        return [self._parse_info(item) for item in items]

    def get(self, sandbox_id: str) -> SandboxInfo:
        """Fetch status and metadata for an existing sandbox."""
        resp = self._request("GET", f"/sandboxes/{sandbox_id}")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    def connect(self, sandbox_id: str) -> SandboxInfo:
        """Connect to a sandbox, automatically resuming it if Paused."""
        resp = self._request("POST", f"/sandboxes/{sandbox_id}/connect")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    def pause(self, sandbox_id: str) -> SandboxInfo:
        """Suspend compute while retaining RootFS Upper and Workspace block PVCs."""
        resp = self._request("POST", f"/sandboxes/{sandbox_id}/pause")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    def reset_rootfs(self, sandbox_id: str) -> SandboxInfo:
        """Reset the persistent RootFS Upper volume while preserving /workspace."""
        resp = self._request("POST", f"/sandboxes/{sandbox_id}/reset-rootfs")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    def fork(self, sandbox_id: str) -> SandboxInfo:
        """Clone both RootFS and Workspace volumes in ~1s via Copy-on-Write."""
        resp = self._request("POST", f"/sandboxes/{sandbox_id}/fork")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    def delete(self, sandbox_id: str) -> None:
        """Terminate and permanently delete a sandbox."""
        resp = self._request("DELETE", f"/sandboxes/{sandbox_id}")
        resp.raise_for_status()

    def renew_timeout(self, sandbox_id: str, timeout_seconds: int = 3600) -> dict[str, Any]:
        """Renew the idle auto-pause timeout for a running sandbox."""
        resp = self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/timeout",
            json={"timeoutSeconds": timeout_seconds},
        )
        resp.raise_for_status()
        return resp.json()

    def acquire_lease(self, sandbox_id: str, lease_seconds: int = 300) -> dict[str, Any]:
        """Acquire or renew an exclusive active lease on the sandbox."""
        resp = self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/lease",
            json={"leaseSeconds": lease_seconds},
        )
        resp.raise_for_status()
        return resp.json()

    def get_panel_endpoint(
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
        resp = self._request(
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

    def execute(
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

        resp = self._request("POST", f"/sandboxes/{sandbox_id}/commands/run", json=payload)
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

    def start_command(
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

        resp = self._request("POST", f"/sandboxes/{sandbox_id}/commands/run", json=payload)
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

    def get_command_status(
        self,
        sandbox_id: str,
        command_id: str,
    ) -> CommandInfo:
        """Get the latest process status of a background command."""
        resp = self._request("GET", f"/sandboxes/{sandbox_id}/commands/{command_id}/status")
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

    def poll_command(
        self,
        sandbox_id: str,
        command_id: str,
        *,
        cursor: int = 0,
    ) -> CommandLogs:
        """Read incremental stdout/stderr output from a background command using byte-offset cursor."""
        resp = self._request(
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

    def stream_command(
        self,
        sandbox_id: str,
        command_id: str,
        *,
        poll_interval: float = 0.05,
    ) -> Iterator[CommandLogs]:
        """Stream incremental logs from a background command until execution terminates."""
        cursor = 0
        while True:
            logs = self.poll_command(sandbox_id, command_id, cursor=cursor)
            cursor = logs.cursor
            yield logs
            if not logs.running:
                break
            time.sleep(poll_interval)

    def kill_command(self, sandbox_id: str, command_id: str) -> dict[str, Any]:
        """Terminate a background command process group."""
        resp = self._request("POST", f"/sandboxes/{sandbox_id}/commands/{command_id}/kill")
        resp.raise_for_status()
        return resp.json()

    def write_file(
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
        resp = self._request(
            "PUT",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path, "uid": str(uid), "gid": str(gid), "mode": mode},
            content=data,
            headers={"Content-Type": "application/octet-stream"},
        )
        resp.raise_for_status()
        return resp.json()

    def read_file(self, sandbox_id: str, path: str) -> bytes:
        """Read a file from inside the sandbox."""
        resp = self._request(
            "GET",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        return resp.content

    def delete_file(self, sandbox_id: str, path: str) -> dict[str, Any]:
        """Delete a file or directory from inside the sandbox."""
        resp = self._request(
            "DELETE",
            f"/sandboxes/{sandbox_id}/files",
            params={"path": path},
        )
        resp.raise_for_status()
        try:
            return resp.json()
        except Exception:
            return {"status": "ok", "path": path}

    def list_files(self, sandbox_id: str, path: str = "/workspace") -> list[FileEntry]:
        """List files and directories at path inside the sandbox."""
        resp = self._request(
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

    def archive_sync(
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

        resp = self._request(
            "POST",
            f"/sandboxes/{sandbox_id}/files/archive-sync",
            params={"dest": dest},
            content=tar_bytes,
            headers={"Content-Type": "application/gzip"},
        )
        resp.raise_for_status()
        return resp.json()

    def create_warmpool(
        self,
        name: str,
        template_id: str = "cubeplex-ubuntu24-desktop",
        replicas: int = 2,
    ) -> dict[str, Any]:
        """Create or scale a PodWarmPool for standby MicroVM allocation."""
        payload = {"name": name, "templateID": template_id, "replicas": replicas}
        resp = self._request("POST", "/warmpools", json=payload)
        resp.raise_for_status()
        return resp.json()

    def list_warmpools(self) -> list[dict[str, Any]]:
        """List all active PodWarmPools and their standby replica counts."""
        resp = self._request("GET", "/warmpools")
        resp.raise_for_status()
        data = resp.json()
        return data if isinstance(data, list) else data.get("warmpools", [])

    def list_templates(self) -> list[dict[str, Any]]:
        """List available sandbox templates."""
        resp = self._request("GET", "/templates")
        resp.raise_for_status()
        data = resp.json()
        return data if isinstance(data, list) else data.get("templates", [])

    def get_template(self, template_id: str) -> dict[str, Any]:
        """Fetch status and metadata for a specific template."""
        resp = self._request("GET", f"/templates/{template_id}")
        resp.raise_for_status()
        return resp.json()

    def get_template_by_alias(self, alias: str) -> dict[str, Any]:
        """Fetch template metadata by alias name or tag."""
        resp = self._request("GET", f"/templates/aliases/{alias}")
        resp.raise_for_status()
        return resp.json()

    def create_template_from_image(
        self,
        from_image: str,
        alias: str | None = None,
        name: str | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        min_free_disk_mb: int | None = None,
    ) -> dict[str, Any]:
        """Register a template from an existing OCI image without invoking BuildKit.

        Calls POST /v3/templates with only the fields provided by the caller,
        followed by POST /v2/templates/{templateID}/builds/{buildID} with fromImage
        and an empty steps array. Returns a dict containing templateID, buildID, aliases, names.
        """
        payload: dict[str, Any] = {}
        if name is not None:
            payload["name"] = name
        if alias is not None:
            payload["alias"] = alias
        if cpu_count is not None:
            payload["cpuCount"] = cpu_count
        if memory_mb is not None:
            payload["memoryMB"] = memory_mb
        if min_free_disk_mb is not None:
            payload["minFreeDiskMb"] = min_free_disk_mb

        resp = self._request("POST", "/v3/templates", json=payload)
        resp.raise_for_status()
        created = resp.json()

        template_id = created.get("templateID")
        build_id = created.get("buildID")

        build_payload: dict[str, Any] = {
            "fromImage": from_image,
            "steps": [],
        }
        build_resp = self._request(
            "POST",
            f"/v2/templates/{template_id}/builds/{build_id}",
            json=build_payload,
        )
        build_resp.raise_for_status()

        return {
            "templateID": template_id,
            "buildID": build_id,
            "aliases": created.get("aliases", []),
            "names": created.get("names", []),
        }

    def delete_template(self, template_id: str, force: bool = False) -> None:
        """Delete a template by templateID. If force is True, appends ?force=true."""
        params = {"force": "true"} if force else None
        resp = self._request("DELETE", f"/templates/{template_id}", params=params)
        resp.raise_for_status()

    def set_template_public(self, template_id: str) -> dict[str, Any] | None:
        """Mark a template as public via PATCH /templates/{templateID} with {"public":true}."""
        resp = self._request("PATCH", f"/templates/{template_id}", json={"public": True})
        resp.raise_for_status()
        if resp.content:
            return resp.json()
        return None

    def build_template(
        self,
        name: str | None = None,
        alias: str | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        min_free_disk_mb: int | None = None,
        from_image: str | None = None,
        from_template: str | None = None,
        steps: list[dict[str, Any]] | None = None,
        start_cmd: str | None = None,
        ready_cmd: str | None = None,
        force: bool = False,
    ) -> dict[str, Any]:
        """Thin helper for Beta template builds: registers via POST /v3/templates
        and triggers build via POST /v2/templates/{templateID}/builds/{buildID}.

        Note: uploading context files (tar archives) via PUT /templates/{templateID}/files/{hash}
        is the caller's responsibility.
        """
        payload: dict[str, Any] = {}
        if name is not None:
            payload["name"] = name
        if alias is not None:
            payload["alias"] = alias
        if cpu_count is not None:
            payload["cpuCount"] = cpu_count
        if memory_mb is not None:
            payload["memoryMB"] = memory_mb
        if min_free_disk_mb is not None:
            payload["minFreeDiskMb"] = min_free_disk_mb

        resp = self._request("POST", "/v3/templates", json=payload)
        resp.raise_for_status()
        created = resp.json()

        template_id = created.get("templateID")
        build_id = created.get("buildID")

        build_payload: dict[str, Any] = {
            "steps": steps if steps is not None else [],
        }
        if from_image is not None:
            build_payload["fromImage"] = from_image
        if from_template is not None:
            build_payload["fromTemplate"] = from_template
        if start_cmd is not None:
            build_payload["startCmd"] = start_cmd
        if ready_cmd is not None:
            build_payload["readyCmd"] = ready_cmd
        if force:
            build_payload["force"] = True

        build_resp = self._request(
            "POST",
            f"/v2/templates/{template_id}/builds/{build_id}",
            json=build_payload,
        )
        build_resp.raise_for_status()

        return {
            "templateID": template_id,
            "buildID": build_id,
            "aliases": created.get("aliases", []),
            "names": created.get("names", []),
        }

    def close(self) -> None:
        """Close the underlying HTTP client session."""
        self._client.close()

    @staticmethod
    def _parse_info(data: dict[str, Any]) -> SandboxInfo:
        return SandboxInfo(
            sandbox_id=data["sandboxID"],
            template_id=data["templateID"],
            state=data["state"],
            envd_port=data.get("envdPort", 49983),
            rootfs_generation=data.get("rootfsGeneration", 1),
        )


# Alias SyncCubePod to CubePod for naming symmetry with AsyncCubePod
SyncCubePod = CubePod
