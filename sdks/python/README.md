# CubePod Python SDK

Async-first Python client for **CubePod** — a persistent, writable-RootFS MicroVM execution platform for AI agents with an E2B-compatible Control & Data Plane API.

## Installation

```bash
pip install cubepod
```

Requires Python 3.11+.

## Quickstart (Async & Sync)

```python
import asyncio
from cubepod import AsyncCubePod, CubePod

async def main() -> None:
    async with AsyncCubePod(base_url="http://localhost:8080", api_key="...") as client:
        box = await client.create("cubeplex-ubuntu24-desktop")
        res = await client.execute(box.sandbox_id, "python3 --version")
        print(res.stdout)
        await client.delete(box.sandbox_id)

asyncio.run(main())

# Synchronous client
with CubePod(base_url="http://localhost:8080", api_key="...") as client:
    box = client.create("cubeplex-ubuntu24-desktop")
    print(client.execute(box.sandbox_id, "uname -a").stdout)
    client.delete(box.sandbox_id)
```

## Command Execution & Cursor Logs

- **Foreground**: `await client.execute(sandbox_id, "echo hello", workdir="/workspace", envs={"FOO": "bar"})`
- **Background & Incremental Polling**:
  ```python
  cmd = await client.start_command(sandbox_id, "for i in 1 2 3; do echo $i; sleep 1; done")
  logs = await client.poll_command(sandbox_id, cmd.command_id, cursor=0)
  # Or stream until completion:
  async for chunk in client.stream_command(sandbox_id, cmd.command_id):
      print(chunk.stdout, end="")
  ```

## Files & Streaming Archive Sync

```python
await client.write_file(sandbox_id, "/workspace/hello.txt", "hello cubepod\n")

# Stream large files incrementally
stream = await client.stream_file(sandbox_id, "/workspace/hello.txt")
async for chunk in stream:
    print(chunk)

# Atomic tar.gz directory sync into the sandbox
await client.archive_sync(
    sandbox_id,
    {"app/main.py": "print('synced')\n"},
    dest="/workspace",
)
```

## Persistent Lifecycle, Reset & 1s CoW Fork

CubePod decouples the container RootFS `upperdir` from `/workspace`:

- `await client.pause(sandbox_id)` — suspend compute while retaining both RootFS Upper and Workspace block PVCs.
- `await client.connect(sandbox_id)` — resume a paused sandbox automatically.
- `await client.reset_rootfs(sandbox_id)` — wipe RootFS `upperdir` changes while keeping `/workspace` intact.
- `await client.fork(sandbox_id)` — create a Copy-on-Write clone of both RootFS and Workspace volumes.

## Zero-Trust `cpodref_` Egress

Never inject raw API credentials into agent sandboxes. Pass `secret_bindings` with `cpodref_` placeholders at `create(...)` time; `cubepod-egress` swaps placeholders for real credentials over per-sandbox mTLS only when outbound requests leave the sandbox toward allowed target hosts.
