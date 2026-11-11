"""CubePod Python SDK — Async-first client for persistent MicroVM agent sandboxes."""

from dataclasses import dataclass
from typing import Any

import httpx


@dataclass
class SandboxInfo:
    sandbox_id: str
    template_id: str
    state: str
    envd_port: int = 49983
    rootfs_generation: int = 1


class AsyncCubePod:
    """Async client for CubePod Control & Data Plane."""

    def __init__(self, base_url: str = "http://localhost:8080", api_key: str | None = None) -> None:
        headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._client = httpx.AsyncClient(base_url=base_url.rstrip("/"), headers=headers, timeout=60.0)

    async def create(
        self,
        template_id: str = "cubeplex-ubuntu24-desktop",
        *,
        volume_key: str | None = None,
        metadata: dict[str, str] | None = None,
    ) -> SandboxInfo:
        payload: dict[str, Any] = {"templateID": template_id, "metadata": metadata or {}}
        if volume_key:
            payload["extension"] = {"volumeKey": volume_key}
        resp = await self._client.post("/sandboxes", json=payload)
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def connect(self, sandbox_id: str) -> SandboxInfo:
        """Connect to a sandbox, automatically resuming it if Paused."""
        resp = await self._client.post(f"/sandboxes/{sandbox_id}/connect")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def pause(self, sandbox_id: str) -> SandboxInfo:
        """Suspend compute while retaining RootFS Upper and Workspace block PVCs."""
        resp = await self._client.post(f"/sandboxes/{sandbox_id}/pause")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def reset_rootfs(self, sandbox_id: str) -> SandboxInfo:
        """Reset the persistent RootFS Upper volume while preserving /workspace."""
        resp = await self._client.post(f"/sandboxes/{sandbox_id}/reset-rootfs")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def fork(self, sandbox_id: str) -> SandboxInfo:
        """Clone both RootFS and Workspace volumes in ~1s via Copy-on-Write."""
        resp = await self._client.post(f"/sandboxes/{sandbox_id}/fork")
        resp.raise_for_status()
        return self._parse_info(resp.json())

    async def close(self) -> None:
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
