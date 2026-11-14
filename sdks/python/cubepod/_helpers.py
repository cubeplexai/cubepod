"""Shared payload builders and response parsers for sync and async CubePod clients."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import Any, cast

import httpx

from .models import (
    CommandInfo,
    CommandLogs,
    DesktopSession,
    ExecutionResult,
    FileEntry,
    NetworkPolicySpec,
    PanelEndpoint,
    SandboxInfo,
    _build_tar_gz_bytes,
)


def _normalize_base_url(base_url: str) -> str:
    normalized = base_url.strip().rstrip("/")
    if normalized and not normalized.startswith(("http://", "https://")):
        normalized = f"http://{normalized}"
    return normalized


def _json_dict(resp: httpx.Response) -> dict[str, Any]:
    return cast(dict[str, Any], resp.json())


def _json_list(resp: httpx.Response, wrapper_key: str) -> list[dict[str, Any]]:
    data = resp.json()
    if isinstance(data, list):
        return cast(list[dict[str, Any]], data)
    return cast(list[dict[str, Any]], data.get(wrapper_key, []))


def _parse_info(data: dict[str, Any]) -> SandboxInfo:
    raw_metadata = data.get("metadata")
    metadata: dict[str, str] = (
        {str(k): str(v) for k, v in raw_metadata.items()}
        if isinstance(raw_metadata, dict)
        else {}
    )
    envd_port_raw = data.get("envdPort")
    rootfs_gen_raw = data.get("rootfsGeneration")
    return SandboxInfo(
        sandbox_id=str(data["sandboxID"]),
        template_id=str(data["templateID"]),
        state=str(data["state"]),
        envd_port=int(envd_port_raw) if envd_port_raw is not None else 49983,
        rootfs_generation=int(rootfs_gen_raw) if rootfs_gen_raw is not None else 1,
        metadata=metadata,
        raw=dict(data),
    )


def _parse_panel_endpoint(data: dict[str, Any]) -> PanelEndpoint:
    return PanelEndpoint(
        url=str(data["url"]),
        token=str(data["token"]),
        expires_at=str(data["expiresAt"]) if data.get("expiresAt") is not None else None,
    )


def _parse_desktop_session(data: dict[str, Any]) -> DesktopSession:
    cdp_raw = data.get("cdpUrl") if "cdpUrl" in data else data.get("cdp_url")
    expires_raw = data.get("expiresAt") if "expiresAt" in data else data.get("expires_at")
    return DesktopSession(
        status=str(data.get("status", "")),
        desktop_url=str(data.get("desktopUrl") or data.get("desktop_url") or ""),
        terminal_url=str(data.get("terminalUrl") or data.get("terminal_url") or ""),
        cdp_url=str(cdp_raw) if cdp_raw is not None else None,
        expires_at=str(expires_raw) if expires_raw is not None else None,
    )


def _parse_execution_result(data: dict[str, Any]) -> ExecutionResult:
    exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
    output = data.get("output", "") or data.get("stdout", "")
    cmd_id = data.get("id") or data.get("cmdID", "")
    return ExecutionResult(
        id=str(cmd_id),
        exit_code=int(exit_code) if exit_code is not None else None,
        output=str(output),
        running=bool(data.get("running", False)),
        pid=int(data["pid"]) if data.get("pid") is not None else None,
        cursor=int(data.get("cursor", 0)),
    )


def _parse_command_info(
    data: dict[str, Any],
    *,
    default_id: str = "",
    default_running: bool = False,
) -> CommandInfo:
    cmd_id = data.get("id") or data.get("cmdID", default_id)
    exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
    output = data.get("output", "") or data.get("stdout", "")
    return CommandInfo(
        id=str(cmd_id),
        pid=int(data["pid"]) if data.get("pid") is not None else None,
        running=bool(data.get("running", default_running)),
        exit_code=int(exit_code) if exit_code is not None else None,
        cursor=int(data.get("cursor", 0)),
        output=str(output),
    )


def _parse_command_logs(
    data: dict[str, Any],
    *,
    command_id: str,
    default_cursor: int,
) -> CommandLogs:
    cmd_id = data.get("id") or data.get("cmdID", command_id)
    exit_code = data.get("exit_code") if "exit_code" in data else data.get("exitCode")
    return CommandLogs(
        id=str(cmd_id),
        content=str(data.get("content", "")),
        cursor=int(data.get("cursor", default_cursor)),
        running=bool(data.get("running", False)),
        exit_code=int(exit_code) if exit_code is not None else None,
    )


def _parse_file_entries(data: Any) -> list[FileEntry]:
    entries = data if isinstance(data, list) else data.get("entries", [])
    result: list[FileEntry] = []
    for e in entries:
        raw_mod = e.get("modified_at") or e.get("modifiedAt") or e.get("mtime")
        result.append(
            FileEntry(
                name=str(e.get("name", "")),
                path=str(e.get("path", "")),
                is_dir=bool(e.get("is_dir", False)),
                size=int(e.get("size", 0)),
                mode=str(e.get("mode", "0644")),
                modified_at=str(raw_mod) if raw_mod is not None else None,
            )
        )
    return result


def _build_create_payload(
    template_id: str,
    *,
    volume_key: str | None = None,
    metadata: dict[str, str] | None = None,
    network_policy: NetworkPolicySpec | Mapping[str, Any] | None = None,
    secret_bindings: list[dict[str, Any]] | None = None,
    timeout_seconds: int | None = None,
    warm_pool_ref: str | None = None,
    resource: dict[str, str] | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {"templateID": template_id, "metadata": metadata or {}}
    if timeout_seconds is not None:
        payload["timeoutSeconds"] = timeout_seconds

    ext: dict[str, Any] = {}
    if volume_key:
        ext["volumeKey"] = volume_key
    if warm_pool_ref:
        ext["warmPoolRef"] = warm_pool_ref
    if resource:
        ext["resource"] = resource
    if network_policy is not None:
        ext["networkPolicy"] = dict(network_policy)
    if secret_bindings is not None:
        net_policy = dict(ext.get("networkPolicy") or {})
        net_policy["secretBindings"] = secret_bindings
        ext["networkPolicy"] = net_policy
    if ext:
        payload["extension"] = ext
    return payload


def _build_fork_payload(
    *,
    volume_key: str | None = None,
    metadata: dict[str, str] | None = None,
) -> dict[str, Any] | None:
    payload: dict[str, Any] = {}
    if metadata is not None:
        payload["metadata"] = metadata
    if volume_key:
        payload["extension"] = {"volumeKey": volume_key}
    return payload or None


def _build_command_payload(
    command: str,
    *,
    background: bool,
    workdir: str | None = None,
    envs: dict[str, str] | None = None,
    uid: int | None = 1000,
    gid: int | None = 1000,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "command": command,
        "background": background,
    }
    if uid is not None:
        payload["uid"] = uid
    if gid is not None:
        payload["gid"] = gid
    if workdir is not None:
        payload["workdir"] = workdir
    if envs is not None:
        payload["envs"] = envs
    return payload


def _build_archive_sync_request(
    files: dict[str, str | bytes] | bytes,
    *,
    dest: str = "/workspace",
    uid: int | None = None,
    gid: int | None = None,
    remove_paths: Sequence[str] | None = None,
) -> tuple[bytes, dict[str, str]]:
    if isinstance(files, (bytes, bytearray)):
        tar_bytes = bytes(files)
    else:
        tar_bytes = _build_tar_gz_bytes(
            files,
            uid=uid if uid is not None else 1000,
            gid=gid if gid is not None else 1000,
        )
    params: dict[str, str] = {"dest": dest}
    if uid is not None:
        params["uid"] = str(uid)
    if gid is not None:
        params["gid"] = str(gid)
    if remove_paths:
        joined = ",".join(p for p in remove_paths if p)
        if joined:
            params["remove_paths"] = joined
    return tar_bytes, params


def _build_desktop_start_payload(
    *,
    resolution: str | None = None,
    initial_url: str | None = None,
    expires_in_seconds: int = 3600,
) -> dict[str, Any]:
    payload: dict[str, Any] = {"expiresInSeconds": expires_in_seconds}
    if resolution is not None:
        payload["resolution"] = resolution
    if initial_url is not None:
        payload["initialUrl"] = initial_url
    return payload


def _build_template_register_payload(
    *,
    name: str | None,
    alias: str | None,
    tags: list[str] | None,
    cpu_count: int | None,
    memory_mb: int | None,
    min_free_disk_mb: int | None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {}
    if name is not None:
        payload["name"] = name
    if alias is not None:
        payload["alias"] = alias
    if tags is not None:
        payload["tags"] = tags
    if cpu_count is not None:
        payload["cpuCount"] = cpu_count
    if memory_mb is not None:
        payload["memoryMB"] = memory_mb
    if min_free_disk_mb is not None:
        payload["minFreeDiskMb"] = min_free_disk_mb
    return payload


def _build_template_result(created: dict[str, Any]) -> dict[str, Any]:
    return {
        "templateID": str(created.get("templateID", "")),
        "buildID": str(created.get("buildID", "")),
        "aliases": created.get("aliases", []),
        "names": created.get("names", []),
        "tags": created.get("tags", []),
    }


def _format_build_error(build_id: str, reason: Any) -> str:
    if isinstance(reason, dict):
        msg = reason.get("message") or str(reason)
        step = reason.get("step")
        if step:
            msg = f"{msg} (step: {step})"
    elif reason:
        msg = str(reason)
    else:
        msg = "unknown build error"
    return f"Template build {build_id} failed: {msg}"
