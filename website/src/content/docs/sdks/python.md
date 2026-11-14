---
title: Python SDK
description: Async-first and synchronous Python client (cubepod) for persistent MicroVM sandboxes, cursor logs, archive sync, CoW forks, desktop sessions, and template builds.
---

The official CubePod Python SDK (`cubepod`) provides both **async-first** (`AsyncCubePod`) and **synchronous** (`CubePod` / `SyncCubePod`) clients over the CubePod Control and Data Plane HTTP APIs.

- **Package**: `cubepod` (`0.4.0`, Apache-2.0)
- **Python**: `>= 3.11` with inline `py.typed` type annotations
- **Runtime dependency**: `httpx >= 0.27.0`

## Installation

```bash
pip install cubepod
```

:::tip[Using the Official `e2b` Python SDK?]
If you have existing agent code written against `e2b` or `e2b-code-interpreter`, CubePod supports drop-in protocol compatibility without rewriting your SDK calls. See [E2B Drop-in Compatibility](/getting-started/e2b-compatibility/).
:::

## Quickstart (Async & Sync)

Both clients support Python context managers (`async with` / `with`) to close the underlying `httpx` connection pool cleanly.

```python
import asyncio
from cubepod import AsyncCubePod, CubePod

async def main() -> None:
    async with AsyncCubePod(
        base_url="http://localhost:8080",
        api_key="your-api-key",
    ) as client:
        box = await client.create("cubeplex-ubuntu24-desktop")
        res = await client.execute(box.sandbox_id, "python3 --version")
        print(res.output)
        await client.delete(box.sandbox_id)

asyncio.run(main())

# Synchronous client (SyncCubePod is an alias for CubePod)
with CubePod(base_url="http://localhost:8080", api_key="your-api-key") as client:
    box = client.create("cubeplex-ubuntu24-desktop")
    print(client.execute(box.sandbox_id, "uname -a").output)
    client.delete(box.sandbox_id)
```

### Client Configuration

`AsyncCubePod(...)` and `CubePod(...)` share the same constructor options:

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `base_url` | `str` | `"http://localhost:8080"` | CubePod Gateway base URL (`http://` is prepended if scheme is omitted). |
| `api_key` | `str \| None` | `None` | Optional Bearer token sent as `Authorization: Bearer <api_key>`. |
| `transport` | `httpx transport \| None` | `None` | Custom `httpx.AsyncBaseTransport` or `httpx.BaseTransport` for testing or custom routing. |
| `max_retries` | `int` | `2` | Maximum automatic retries with exponential backoff on HTTP `502`, `503`, and `504`. |
| `timeout` | `float` | `60.0` | Default request timeout in seconds. |
| `trust_env` | `bool` | `False` | Whether `httpx` should read host `HTTP_PROXY` / `HTTPS_PROXY` environment variables. |

## Sandbox Lifecycle, Reset & 1s CoW Fork

CubePod separates the persistent container RootFS `upperdir` from `/workspace` (see [Dual-Volume Persistence](/concepts/dual-volume-persistence/)). Every lifecycle operation returns a `SandboxInfo` dataclass:

```python
box = await client.create(
    template_id="cubeplex-ubuntu24-desktop",
    volume_key="user-42-workspace",
    timeout_seconds=1800,
    warm_pool_ref="default-pool",
    metadata={"agent": "coder", "env": "dev"},
)

# Check terminal state helper
print(box.sandbox_id, box.state, box.rootfs_generation, box.is_terminal)

# List or inspect sandboxes
boxes = await client.list(template_id="cubeplex-ubuntu24-desktop")
same_box = await client.get(box.sandbox_id)
raw_json = await client.get_raw(box.sandbox_id)

# Keepalive timeout & lease protection against idle auto-pause
await client.renew_timeout(box.sandbox_id, timeout_seconds=3600)
await client.acquire_lease(box.sandbox_id, lease_seconds=300)

# Pause compute (0 CPU/RAM, retains both RootFS Upper and Workspace PVCs)
await client.pause(box.sandbox_id)

# Connect (automatically resumes a Paused sandbox)
await client.connect(box.sandbox_id)

# Wipe RootFS Upper changes (e.g., broken apt/pip state) while preserving /workspace
await client.reset_rootfs(box.sandbox_id)

# Create a ~1s Copy-on-Write clone of both RootFS and Workspace volumes
child = await client.fork(
    box.sandbox_id,
    volume_key="user-42-branch-a",
    metadata={"branch": "experiment-1"},
)

await client.delete(child.sandbox_id)
await client.delete(box.sandbox_id)
```

### `create(...)` Parameters

- `template_id` (`str = "cubeplex-ubuntu24-desktop"`): Template ID, alias, or OCI image reference.
- `volume_key` (`str | None`): Deterministic persistent storage key (`extension.volumeKey`) for re-attaching workspace state across sandboxes.
- `metadata` (`dict[str, str] | None`): Arbitrary key-value labels attached to the sandbox.
- `network_policy` (`dict[str, Any] | None`): Zero-Trust outbound firewall specification (`defaultAction`, `allowHosts`, `denyHosts`, `upstreamProxy`).
- `secret_bindings` (`list[dict[str, Any]] | None`): `cpodref_` credential bindings merged into `networkPolicy.secretBindings`.
- `timeout_seconds` (`int | None`): Idle timeout before automatic pause.
- `warm_pool_ref` (`str | None`): Claim a pre-attached MicroVM from a `PodWarmPool`.
- `resource` (`dict[str, str] | None`): Optional resource overrides placed in `extension.resource`.

## Command Execution & Cursor Log Streaming

Foreground commands run via `execute(...)` and default to `uid=1000, gid=1000` (override with `uid=0, gid=0` when root privileges are needed, or `uid=None, gid=None` to omit).

```python
# Foreground execution
res = await client.execute(
    box.sandbox_id,
    "pytest -q",
    workdir="/workspace",
    envs={"PYTHONUNBUFFERED": "1"},
    uid=1000,
    gid=1000,
    timeout=120.0,
)
print(res.exit_code, res.output)

# Background execution + stateless byte-offset cursor polling
cmd = await client.start_command(
    box.sandbox_id,
    "for i in 1 2 3; do echo tick-$i; sleep 1; done",
    workdir="/workspace",
)

status = await client.get_command_status(box.sandbox_id, cmd.id)
logs = await client.poll_command(box.sandbox_id, cmd.id, cursor=0)
print(logs.content, logs.cursor, logs.running)

# Stream incremental CommandLogs chunks until completion
async for chunk in client.stream_command(box.sandbox_id, cmd.id, poll_interval=0.05):
    if chunk.content:
        print(chunk.content, end="")

# Terminate a background command process group
await client.kill_command(box.sandbox_id, cmd.id)
```

## Files, Streaming Reads & `archive_sync`

```python
# Write UTF-8 text or raw bytes
await client.write_file(
    box.sandbox_id,
    "/workspace/app.py",
    "print('hello cubepod')\n",
    uid=1000,
    gid=1000,
    mode="0644",
)

# Read full bytes, stream raw chunks, or bounded text read
raw_bytes = await client.read_file(box.sandbox_id, "/workspace/app.py")

stream = await client.stream_file(box.sandbox_id, "/workspace/large.bin", chunk_size=65536)
async for chunk in stream:
    process_chunk(chunk)

# Raises CubePodFileTooLargeError if file exceeds max_bytes
from cubepod import CubePodFileTooLargeError

try:
    text = await client.read_text_file(
        box.sandbox_id,
        "/workspace/app.py",
        max_bytes=1024 * 1024,
        encoding="utf-8",
    )
except CubePodFileTooLargeError as exc:
    print("File too large:", exc)

# List and delete files
entries = await client.list_files(box.sandbox_id, "/workspace")
for entry in entries:
    print(entry.name, entry.is_dir, entry.size, entry.mode)

await client.delete_file(box.sandbox_id, "/workspace/old.txt")

# Atomic tar.gz directory sync (accepts a dict of path -> content, or prebuilt .tar.gz bytes)
await client.archive_sync(
    box.sandbox_id,
    {
        "skills/research/SKILL.md": "# Research Skill\n",
        "skills/research/run.py": b"print('ready')\n",
    },
    dest="/workspace",
    uid=1000,
    gid=1000,
    remove_paths=["skills/deprecated"],
)
```

## Interactive Desktop & Signed Panel Endpoints

CubePod supports lazy desktop activation (XFCE + noVNC, `ttyd` web terminal, and Chromium CDP) and HMAC-SHA256 signed `/panel/{token}/` proxy endpoints:

```python
# Start interactive desktop stack lazily inside a desktop-enabled sandbox
desktop = await client.start_desktop(
    box.sandbox_id,
    resolution="1280x800x24",
    initial_url="https://example.com",
    expires_in_seconds=3600,
)
print(desktop.desktop_url, desktop.terminal_url, desktop.cdp_url)

status = await client.get_desktop_status(box.sandbox_id)
await client.stop_desktop(box.sandbox_id)

# Mint a signed panel URL for any HTTP/WS/WebRTC port inside the sandbox
panel = await client.get_panel_endpoint(box.sandbox_id, port=8080, expires_in_seconds=3600)
print(panel.url, panel.token, panel.expires_at)
```

## Zero-Trust `cpodref_` Egress Secret Bindings

Never pass raw external API keys inside `envs` or files. Register `secret_bindings` at sandbox creation time so only opaque `cpodref_` placeholders exist inside the MicroVM, while `cubepod-egress` substitutes the real credential exclusively on approved target hosts:

```python
box = await client.create(
    "cubeplex-ubuntu24-desktop",
    network_policy={
        "defaultAction": "Deny",
        "allowHosts": ["api.openai.com", "api.github.com"],
    },
    secret_bindings=[
        {
            "envName": "OPENAI_API_KEY",
            "rawSecret": os.environ["REAL_OPENAI_KEY"],
            "allowedHosts": ["api.openai.com"],
            "allowedHeaders": ["Authorization"],
        }
    ],
)
```

See [Zero-Trust Egress & Secrets](/concepts/zero-trust-egress/) for the full mTLS exchange flow.

## WarmPools & Templates

```python
# Create or scale a standby PodWarmPool for <50ms sandbox claims
await client.create_warmpool("fast-pool", template_id="cubeplex-ubuntu24-desktop", replicas=3)
pools = await client.list_warmpools()

# Template catalog & builds
templates = await client.list_templates()
tpl = await client.get_template_by_alias("base")

# Register an existing OCI image (no BuildKit required)
registered = await client.create_template_from_image(
    from_image="python:3.13-slim",
    alias="py313-slim",
    cpu_count=2,
    memory_mb=2048,
    wait=True,
)

# Full recipe build via BuildKit
built = await client.build_template(
    name="data-science:v1",
    from_image="python:3.13",
    steps=[{"type": "RUN", "args": ["pip install pandas numpy"]}],
    cpu_count=4,
    memory_mb=4096,
    wait=True,
)
```

For full template build rules and constraints, see [Template Catalog & Builds](/sdks/templates/).

## Data Models Reference

| Model | Key Fields |
| :--- | :--- |
| `SandboxInfo` | `sandbox_id`, `template_id`, `state`, `envd_port` (`49983`), `rootfs_generation` (`1`), `metadata`, `raw`, `.is_terminal` |
| `ExecutionResult` | `id`, `exit_code`, `output`, `running`, `pid`, `cursor` |
| `CommandInfo` | `id`, `pid`, `running`, `exit_code`, `cursor`, `output` |
| `CommandLogs` | `id`, `content`, `cursor`, `running`, `exit_code` |
| `FileEntry` | `name`, `path`, `is_dir`, `size`, `mode`, `modified_at` |
| `PanelEndpoint` | `url`, `token`, `expires_at` |
| `DesktopSession` | `status`, `desktop_url`, `terminal_url`, `cdp_url`, `expires_at` |
| `CubePodFileTooLargeError` | Subclass of `ValueError` raised by `read_text_file` when `max_bytes` is exceeded |
