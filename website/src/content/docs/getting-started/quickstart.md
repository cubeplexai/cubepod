---
title: Quickstart
description: Start a local CubePod stack with Docker Compose, create a persistent sandbox, run commands and code, pause/resume, fork, and launch a browser desktop.
---

Get CubePod running locally in minutes using `docker-compose`, then interact with your persistent sandboxes through the Python SDK, TypeScript SDK, or `cubepod` CLI.

## Prerequisites

- **Docker Engine** with Docker Compose v2 (`docker compose`)
- **Python 3.11+** (for the Python SDK) and/or **Node.js 20+** (for the TypeScript SDK)
- *(Optional, if building binaries from source)* **Go 1.22+** and **Rust** (`cargo`)

## 1. Start the Local Control Plane

From the root of the CubePod repository, boot `cubepod-gateway` (`:8080`), `cubepod-controller` (`:8081`), and `cubepod-egress` (`:18080`):

```bash
docker compose -f deploy/docker-compose/docker-compose.yaml up -d --build
```

Verify that the gateway is healthy:

```bash
curl -fsS http://127.0.0.1:8080/health
```

> **Note:** `deploy/docker-compose/docker-compose.yaml` sets `CUBEPOD_RUNTIME_MODE=docker-loopback` on `cubepod-gateway`, mounts `/var/run/docker.sock` and `cubepod-volumes`, and starts `cubepod-egress` with `-default-allow=true` for local development. For building all Rust and Go binaries locally without Docker Compose, run `make build` (`bin/podd-init`, `bin/podd`, `bin/cubepod-controller`, `bin/cubepod-gateway`, `bin/cubepod-egress`, and `bin/cubepod`).

## 2. Create a Sandbox & Verify Persistence

Choose your preferred client SDK or the native CLI below.

### Python SDK

Install the Python SDK from PyPI or from `sdks/python/` in the repository:

```bash
pip install cubepod
```

Create a sandbox, write files to `/workspace`, execute commands, pause compute (`0 CPU / 0 RAM`), reconnect, and create a Copy-on-Write fork:

```python
from cubepod import CubePod

with CubePod(base_url="http://127.0.0.1:8080") as client:
    # 1. Create a persistent sandbox
    sbx = client.create("cubeplex-ubuntu24-desktop", timeout_seconds=3600)
    print("Created:", sbx.sandbox_id, "state:", sbx.state)

    # 2. Write a file in /workspace and run a command
    client.write_file(sbx.sandbox_id, "/workspace/hello.txt", b"persistent data\n")
    res = client.execute(sbx.sandbox_id, "cat /workspace/hello.txt && id")
    print("Output:", res.output)

    # 3. Pause compute (retains both rootfs and /workspace volumes)
    client.pause(sbx.sandbox_id)

    # 4. Resume on connect and verify state survived pause
    client.connect(sbx.sandbox_id)
    print("After resume:", client.read_text_file(sbx.sandbox_id, "/workspace/hello.txt"))

    # 5. Create an independent Copy-on-Write fork
    child = client.fork(sbx.sandbox_id)
    print("Forked child sandbox:", child.sandbox_id)

    # 6. Clean up both sandboxes
    client.delete(child.sandbox_id)
    client.delete(sbx.sandbox_id)
```

### TypeScript / Node.js SDK

The official TypeScript SDK has zero external runtime dependencies on Node.js 20+:

```bash
npm install cubepod
```

```typescript
import { Sandbox } from 'cubepod';

const sandbox = await Sandbox.create('cubeplex-ubuntu24-desktop', {
  baseUrl: 'http://127.0.0.1:8080',
  timeoutSeconds: 3600,
});

await sandbox.files.write('/workspace/hello.txt', 'persistent data\n');
const cmd = await sandbox.commands.run('cat /workspace/hello.txt');
console.log('Command output:', cmd.output);

// Run code via podd's built-in stateful interpreter (Python, Bash, JS, TS, R, Java)
const exec = await sandbox.code.runCode('print(6 * 7)', { language: 'python' });
console.log('Interpreter:', exec.output);

// Pause and later reconnect transparently
await sandbox.pause();
await sandbox.connect();

// Crash-consistent CoW fork
const child = await sandbox.fork();
await child.kill();
await sandbox.kill();
```

### `cubepod` CLI

Build the CLI with `make build-cli` (outputs `bin/cubepod`), then drive the full lifecycle from your terminal:

```bash
export CUBEPOD_GATEWAY_URL="http://127.0.0.1:8080"

# Check gateway health
./bin/cubepod health

# Create a sandbox and run a command
./bin/cubepod sandbox create --template cubeplex-ubuntu24-desktop
./bin/cubepod sandbox exec <sandbox-id> "uname -a && id"

# Upload/read files and sync a directory as tar.gz
./bin/cubepod sandbox write-file <sandbox-id> /workspace/app.py "print('hello from cubepod')"
./bin/cubepod sandbox read-file <sandbox-id> /workspace/app.py
./bin/cubepod sandbox archive-sync <sandbox-id> /workspace/project ./local-project

# Pause, reconnect, fork, reset OS rootfs (keeping /workspace), or delete
./bin/cubepod sandbox pause <sandbox-id>
./bin/cubepod sandbox connect <sandbox-id>
./bin/cubepod sandbox fork <sandbox-id>
./bin/cubepod sandbox reset-rootfs <sandbox-id>
./bin/cubepod sandbox delete <sandbox-id>
```

## 3. Reset RootFS Without Losing `/workspace`

If an agent installs broken system libraries or corrupts OS state under `/etc` or `/usr`, you can reset the RootFS layer while keeping `/workspace` intact:

```python
# Wipes the persistent OverlayFS upperdir (allocates rootfs-gen<N>)
# while re-attaching the exact same /workspace volume.
client.reset_rootfs(sbx.sandbox_id)
```

Read [Dual-Volume Persistence](/concepts/dual-volume-persistence/) to see how `rootfsGeneration` keeps OS upgrades and project data strictly separated.

## 4. Launch an Interactive Desktop (Optional)

When using a desktop-capable template image (see [Interactive Desktop & Panel Proxy](/concepts/desktop-and-panel-proxy/)), you can lazily start `Xvfb`, `openbox`, Chromium CDP, `noVNC`, and `ttyd` inside the sandbox:

```python
session = client.start_desktop(
    sbx.sandbox_id,
    resolution="1280x800x24",
    initial_url="https://example.com",
)
print("noVNC URL:", session.desktop_url)
print("Terminal URL:", session.terminal_url)
print("Chromium CDP URL:", session.cdp_url)
```

> **Tip:** If your sandbox template does not contain `/opt/cubepod/desktop/cubepod-desktop-ctl`, `start_desktop` fails fast with HTTP `422 desktop_not_installed`.

## Next Steps

- Run existing `e2b` / `e2b-code-interpreter` code unchanged via **[E2B Drop-in Compatibility](/getting-started/e2b-compatibility/)**.
- Explore the internals in **[Platform Architecture](/concepts/architecture/)**.
- Deploy CubePod on Kubernetes with Kata Containers and CSI raw block storage in **[Kubernetes & Helm Deployment](/deployment/kubernetes-helm/)**.
