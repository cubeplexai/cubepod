"""CubePod data models and archive serialization helpers."""

from dataclasses import dataclass, field
import io
import tarfile
import time
from typing import Any, Literal, TypedDict


class NetworkEgressRule(TypedDict):
    action: Literal["allow", "deny"] | str
    target: str


class NetworkPolicySpec(TypedDict, total=False):
    defaultAction: Literal["allow", "deny", "Allow", "Deny"] | str
    allowHosts: list[str]
    denyHosts: list[str]
    egress: list[NetworkEgressRule]
    upstreamProxy: str
    secretBindings: list[dict[str, Any]]


_TERMINAL_STATES = frozenset({"stopped", "terminated", "failed", "killed", "succeed"})


@dataclass
class SandboxInfo:
    sandbox_id: str
    template_id: str
    state: str
    envd_port: int = 49983
    rootfs_generation: int = 1
    metadata: dict[str, str] = field(default_factory=dict)
    raw: dict[str, Any] = field(default_factory=dict)

    @property
    def is_terminal(self) -> bool:
        """Return True if the sandbox is in a terminal lifecycle state."""
        return self.state.casefold() in _TERMINAL_STATES


@dataclass
class ExecutionResult:
    id: str
    exit_code: int | None
    output: str
    running: bool = False
    pid: int | None = None
    cursor: int = 0


@dataclass
class CommandInfo:
    id: str
    pid: int | None = None
    running: bool = True
    exit_code: int | None = None
    cursor: int = 0
    output: str = ""


@dataclass
class CommandLogs:
    id: str
    content: str
    cursor: int
    running: bool
    exit_code: int | None = None


@dataclass
class PanelEndpoint:
    url: str
    token: str
    expires_at: str | None = None


@dataclass
class FileEntry:
    name: str
    path: str
    is_dir: bool
    size: int
    mode: str = "0644"
    modified_at: str | None = None


@dataclass
class DesktopSession:
    status: str
    desktop_url: str
    terminal_url: str
    cdp_url: str | None = None
    expires_at: str | None = None
    webrtc_url: str | None = None
    transport_used: str | None = None
    fallback_reason: str | None = None


@dataclass
class SecretInfo:
    secret_id: str
    name: str
    current_version: int = 1
    metadata: dict[str, str] = field(default_factory=dict)
    created_at: str | None = None
    updated_at: str | None = None
    raw: dict[str, Any] = field(default_factory=dict)

    @property
    def id(self) -> str:
        return self.secret_id

    @property
    def version(self) -> int:
        return self.current_version


class Secret:
    """Helper utilities for E2B/CubePod write-only secrets."""

    @staticmethod
    def fill(name: str) -> str:
        """Return the placeholder template ``${e2b.secrets.<name>}``."""
        return f"${{e2b.secrets.{name}}}"


def fill_secret(name: str) -> str:
    """Return the placeholder template ``${e2b.secrets.<name>}``."""
    return Secret.fill(name)


@dataclass
class VolumeInfo:
    volume_id: str
    name: str
    token: str | None = None
    raw: dict[str, Any] = field(default_factory=dict)


@dataclass
class VolumeEntry:
    name: str
    type: str
    path: str
    size: int = 0
    mode: int = 0o644
    uid: int = 1000
    gid: int = 1000
    atime: str | None = None
    mtime: str | None = None
    ctime: str | None = None
    raw: dict[str, Any] = field(default_factory=dict)


@dataclass
class LifecycleEvent:
    version: str
    id: str
    type: str
    timestamp: str
    event_category: str
    event_label: str
    sandbox_id: str
    sandbox_team_id: str = ""
    sandbox_template_id: str = ""
    sandbox_build_id: str = ""
    sandbox_execution_id: str = ""
    event_data: dict[str, Any] | None = None
    raw: dict[str, Any] = field(default_factory=dict)


@dataclass
class WebhookInfo:
    id: str
    name: str
    url: str
    events: list[str] = field(default_factory=list)
    enabled: bool = True
    signature_key: str | None = None
    created_at: str | None = None
    updated_at: str | None = None
    raw: dict[str, Any] = field(default_factory=dict)


@dataclass
class SandboxMetric:
    timestamp: str
    timestamp_unix: int
    cpu_count: int
    cpu_used_pct: float
    mem_used: int
    mem_total: int
    mem_cache: int = 0
    disk_used: int = 0
    disk_total: int = 0
    raw: dict[str, Any] = field(default_factory=dict)


def _build_tar_gz_bytes(
    files: dict[str, str | bytes],
    uid: int = 1000,
    gid: int = 1000,
) -> bytes:
    """Pack a dict of file paths and contents into an in-memory tar.gz archive."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        for file_path, content in files.items():
            data = content.encode("utf-8") if isinstance(content, str) else content
            rel_name = file_path.lstrip("/")
            ti = tarfile.TarInfo(name=rel_name)
            ti.size = len(data)
            ti.mtime = int(time.time())
            ti.mode = 0o644
            ti.uid = uid
            ti.gid = gid
            tar.addfile(ti, io.BytesIO(data))
    buf.seek(0)
    return buf.getvalue()
