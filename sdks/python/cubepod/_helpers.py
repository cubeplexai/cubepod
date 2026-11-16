"""Shared payload builders and response parsers for sync and async CubePod clients."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from datetime import datetime
from typing import Any, cast

import httpx

from .models import (
    CommandInfo,
    CommandLogs,
    DesktopSession,
    ExecutionResult,
    FileEntry,
    LifecycleEvent,
    NetworkPolicySpec,
    PanelEndpoint,
    SandboxInfo,
    SandboxMetric,
    SecretInfo,
    VolumeEntry,
    VolumeInfo,
    WebhookInfo,
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
    webrtc_raw = data.get("webrtcUrl") if "webrtcUrl" in data else data.get("webrtc_url")
    transport_raw = data.get("transportUsed") if "transportUsed" in data else data.get("transport_used")
    fallback_raw = data.get("fallbackReason") if "fallbackReason" in data else data.get("fallback_reason")
    return DesktopSession(
        status=str(data.get("status", "")),
        desktop_url=str(data.get("desktopUrl") or data.get("desktop_url") or ""),
        terminal_url=str(data.get("terminalUrl") or data.get("terminal_url") or ""),
        cdp_url=str(cdp_raw) if cdp_raw is not None else None,
        expires_at=str(expires_raw) if expires_raw is not None else None,
        webrtc_url=str(webrtc_raw) if webrtc_raw is not None else None,
        transport_used=str(transport_raw) if transport_raw is not None else None,
        fallback_reason=str(fallback_raw) if fallback_raw is not None else None,
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
    network: Mapping[str, Any] | None = None,
    volume_mounts: Sequence[Mapping[str, str]] | None = None,
    secret_bindings: list[dict[str, Any]] | None = None,
    timeout_seconds: int | None = None,
    warm_pool_ref: str | None = None,
    resource: dict[str, str] | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {"templateID": template_id, "metadata": metadata or {}}
    if timeout_seconds is not None:
        payload["timeoutSeconds"] = timeout_seconds
    if network is not None:
        payload["network"] = dict(network)
    if volume_mounts is not None:
        payload["volumeMounts"] = [dict(m) for m in volume_mounts]

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
    transport: str | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {"expiresInSeconds": expires_in_seconds}
    if resolution is not None:
        payload["resolution"] = resolution
    if initial_url is not None:
        payload["initialUrl"] = initial_url
    if transport is not None:
        payload["transport"] = transport
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



def _parse_secret_info(data: dict[str, Any]) -> SecretInfo:
    raw_meta = data.get("metadata")
    metadata: dict[str, str] = (
        {str(k): str(v) for k, v in raw_meta.items()}
        if isinstance(raw_meta, dict)
        else {}
    )
    sec_id = data.get("secretID") or data.get("id") or ""
    ver_raw = data.get("currentVersion") if "currentVersion" in data else data.get("version", 1)
    return SecretInfo(
        secret_id=str(sec_id),
        name=str(data.get("name", "")),
        current_version=int(ver_raw) if ver_raw is not None else 1,
        metadata=metadata,
        created_at=str(data["createdAt"]) if data.get("createdAt") is not None else None,
        updated_at=str(data["updatedAt"]) if data.get("updatedAt") is not None else None,
        raw=dict(data),
    )


def _parse_volume_info(data: dict[str, Any]) -> VolumeInfo:
    return VolumeInfo(
        volume_id=str(data.get("volumeID") or data.get("volumeId") or ""),
        name=str(data.get("name", "")),
        token=str(data["token"]) if data.get("token") is not None else None,
        raw=dict(data),
    )


def _parse_volume_entry(data: dict[str, Any]) -> VolumeEntry:
    return VolumeEntry(
        name=str(data.get("name", "")),
        type=str(data.get("type", "")),
        path=str(data.get("path", "")),
        size=int(data.get("size", 0)),
        mode=int(data.get("mode", 0o644)),
        uid=int(data.get("uid", 1000)),
        gid=int(data.get("gid", 1000)),
        atime=str(data["atime"]) if data.get("atime") is not None else None,
        mtime=str(data["mtime"]) if data.get("mtime") is not None else None,
        ctime=str(data["ctime"]) if data.get("ctime") is not None else None,
        raw=dict(data),
    )


def _parse_lifecycle_event(data: dict[str, Any]) -> LifecycleEvent:
    raw_event_data = data.get("eventData")
    return LifecycleEvent(
        version=str(data.get("version", "")),
        id=str(data.get("id", "")),
        type=str(data.get("type", "")),
        timestamp=str(data.get("timestamp", "")),
        event_category=str(data.get("eventCategory", "")),
        event_label=str(data.get("eventLabel", "")),
        sandbox_id=str(data.get("sandboxId", "")),
        sandbox_team_id=str(data.get("sandboxTeamId", "")),
        sandbox_template_id=str(data.get("sandboxTemplateId", "")),
        sandbox_build_id=str(data.get("sandboxBuildId", "")),
        sandbox_execution_id=str(data.get("sandboxExecutionId", "")),
        event_data=dict(raw_event_data) if isinstance(raw_event_data, dict) else None,
        raw=dict(data),
    )


def _parse_webhook_info(data: dict[str, Any]) -> WebhookInfo:
    raw_events = data.get("events")
    events_list = [str(e) for e in raw_events] if isinstance(raw_events, list) else []
    sig_raw = data.get("signatureKey") if "signatureKey" in data else data.get("signature_key")
    return WebhookInfo(
        id=str(data.get("id", "")),
        name=str(data.get("name", "")),
        url=str(data.get("url", "")),
        events=events_list,
        enabled=bool(data.get("enabled", True)),
        signature_key=str(sig_raw) if sig_raw is not None else None,
        created_at=str(data["createdAt"]) if data.get("createdAt") is not None else None,
        updated_at=str(data["updatedAt"]) if data.get("updatedAt") is not None else None,
        raw=dict(data),
    )


def _parse_sandbox_metric(data: dict[str, Any]) -> SandboxMetric:
    return SandboxMetric(
        timestamp=str(data.get("timestamp", "")),
        timestamp_unix=int(data.get("timestampUnix", 0)),
        cpu_count=int(data.get("cpuCount", 0)),
        cpu_used_pct=float(data.get("cpuUsedPct", 0.0)),
        mem_used=int(data.get("memUsed", 0)),
        mem_total=int(data.get("memTotal", 0)),
        mem_cache=int(data.get("memCache", 0)),
        disk_used=int(data.get("diskUsed", 0)),
        disk_total=int(data.get("diskTotal", 0)),
        raw=dict(data),
    )


def _format_metric_time_param(value: int | float | str | datetime | None) -> str | None:
    if value is None:
        return None
    if isinstance(value, datetime):
        return value.isoformat()
    if isinstance(value, (int, float)):
        return str(int(value))
    return str(value)
