"""CubePod data models and archive serialization helpers."""

from dataclasses import dataclass
import io
import tarfile
import time


@dataclass
class SandboxInfo:
    sandbox_id: str
    template_id: str
    state: str
    envd_port: int = 49983
    rootfs_generation: int = 1


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
