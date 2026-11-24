"""CubePod Python SDK — Sync & Async client for persistent MicroVM agent sandboxes."""

from . import browser_tab_follow
from .async_client import AsyncCubePod
from .browser_tab_follow import (
    FOCUS_SCRIPT,
    WRAPPER_SCRIPT,
    choose_target_id,
    tab_follow_install_command,
)
from .exceptions import CubePodFileTooLargeError
from .models import (
    CommandInfo,
    CommandLogs,
    DesktopSession,
    EventPage,
    ExecutionResult,
    FileEntry,
    LifecycleEvent,
    NetworkEgressRule,
    NetworkPolicySpec,
    PanelEndpoint,
    SandboxInfo,
    SandboxMetric,
    Secret,
    SecretInfo,
    SecretPage,
    VolumeEntry,
    VolumeInfo,
    WebhookInfo,
    _build_tar_gz_bytes,
    fill_secret,
)
from .sync_client import CubePod, SyncCubePod

__version__ = "0.8.0"

__all__ = [
    "__version__",
    "SandboxInfo",
    "SandboxMetric",
    "Secret",
    "SecretInfo",
    "SecretPage",
    "fill_secret",
    "VolumeInfo",
    "VolumeEntry",
    "LifecycleEvent",
    "EventPage",
    "WebhookInfo",
    "NetworkEgressRule",
    "NetworkPolicySpec",
    "ExecutionResult",
    "CommandInfo",
    "CommandLogs",
    "PanelEndpoint",
    "FileEntry",
    "DesktopSession",
    "CubePodFileTooLargeError",
    "_build_tar_gz_bytes",
    "AsyncCubePod",
    "CubePod",
    "SyncCubePod",
    "browser_tab_follow",
    "FOCUS_SCRIPT",
    "WRAPPER_SCRIPT",
    "choose_target_id",
    "tab_follow_install_command",
]
