# CubePod

**Persistent Writable RootFS MicroVM Execution Platform for AI Agents**

CubePod is an open-source, cloud-native execution platform designed for long-lived, high-autonomy AI Agents. Together with [**CubePlex**](https://github.com/cubeplexai/cubeplex) (Agent Workspace Platform) and [**CubeLoop**](https://github.com/cubeplexai/cubeloop) (Agent Runtime), CubePod provides hardware-isolated MicroVM execution with instant hibernation/resume and full system-level persistence.

---

## Why CubePod?

Modern AI agents (DevOps agents, software engineering assistants, interactive data analysts) operate with broad shell privileges: they dynamically install system packages (`apt`, `pip`, `npm`, Rust/C++ compilers), launch headless or interactive GUI browsers, and interact with external APIs using privileged credentials.

Existing sandbox solutions suffer from four fundamental architectural limitations:

1. **Ephemeral RootFS (Only `/workspace` Persists)**: Standard container sandboxes discard modifications to `/usr`, `/etc`, and `/var` whenever a sandbox sleeps or restarts. Agents are forced to repeatedly reinstall toolchains and libraries, wasting minutes on every wakeup.
2. **Heavyweight Snapshot Overhead**: VM memory snapshotting (CRIU / VM dumps) produces gigabyte-scale state files sensitive to host CPU architecture, while OCI commit/export approaches require tens of seconds to push layers to registries. Modern agents follow a **"Decoupled Brain & Hand"** model—reasoning context resides in the agent framework, meaning the sandbox only needs **100% persistent disk state with zero CPU/RAM allocation during sleep**.
3. **Network Filesystem I/O Bottleneck**: Mounting NFS or CephFS across sandboxes causes catastrophic latency during `npm install`, `pip install`, or `git status` due to millions of small-file RPC round-trips.
4. **Plaintext Secret Exposure**: Injecting real API keys into container environment variables exposes credentials to untrusted LLM-generated code or host memory leaks.

CubePod solves these challenges with **Dual-Volume CSI Raw Block persistence**, **Kata Containers MicroVM isolation**, **Zero-Export instant hibernation**, and an **in-guest Zero-Trust egress vault**.

---

## CubePod vs. Upstream E2B & Standard Sandboxes

| Capability / Dimension | Upstream E2B Cloud / Standard Sandboxes | CubePod Open-Source Platform |
| :--- | :--- | :--- |
| **System Persistence** | Ephemeral RootFS; only `/workspace` persists (or none); system packages lost on sleep | **Full RootFS + Workspace Dual Persistence**; `apt`/`pip`/`npm` modifications survive indefinitely |
| **Storage Architecture** | Ephemeral container overlay or slow shared NFS | **Dual CSI Raw Block PVCs** (`volumeMode: Block` $\to$ `virtio-blk` $\to$ Guest `ext4`) with native NVMe-oF speed |
| **RootFS & Workspace Lifecycle** | Coupled (resetting workspace resets OS, or vice versa) | **Fully Decoupled**: increment `rootfsGeneration` to re-image OS while preserving `/workspace` intact |
| **Hibernation & Resumption** | Heavy container snapshot export or cold VM restart | **Zero-Export Suspend** (`fsfreeze`, delete Pod $\to$ 0 CPU/RAM) & **~1s Cross-Node Mount Resume** |
| **MicroVM Cold Start** | Standard VM boot latency (several seconds) | **Pre-Attached WarmPool (`PodWarmPool`)**: blank pre-formatted ext4 MicroVMs claimed in **< 50ms** |
| **Sandbox Forking** | Not supported / full volume copy | **< 1s Copy-on-Write Fork**: instantaneous sub-agent cloning via CSI block snapshots |
| **Isolation Boundary** | Shared-kernel containers or cloud black-box | **Hardware-Isolated Kata MicroVM** (dedicated Guest Linux Kernel per sandbox) |
| **Secret & Egress Security** | Plaintext API keys injected into environment variables | **Zero-Trust Egress Vault**: transparent `cpodref_` placeholder substitution via per-pod mTLS |
| **Interactive GUI & Browser** | Port mapping with port collision risks | **Signed Panel Reverse Proxy**: path/token-authenticated HTTP, WebSocket, & WebRTC (Chromium/Neko/ttyd) |
| **API & Ecosystem** | Proprietary API & SaaS vendor lock-in | **100% Wire-Compatible E2B Superset**: drop-in support for official E2B SDKs + MCP |
| **Deployment Model** | Proprietary cloud only | **100% Open Source**: Self-host on Kubernetes (Kata + CSI) or single-node Docker Compose |

---

## System Architecture

```text
+---------------------------------------------------------------------------------------+
|                       1. Access Layer: Agent Platforms & SDKs                         |
|   CubePlex Platform  |  Official E2B SDKs (Python / JS)  |  CubePod SDK  |  MCP Clients |
+---------------------------------------------------------------------------------------+
                                           |
                                           v
+---------------------------------------------------------------------------------------+
|              2. Control & Gateway Plane (Stateless High-Availability)                 |
|  cubepod-gateway (Go)               |  cubepod-controller (Go K8s Operator)           |
|  • E2B-compatible REST/SSE router   |  • Dual Block PVC & WarmPool lifecycle          |
|  • Auto-Resume traffic interceptor  |  • In-memory per-pod mTLS certificate minting   |
|  • Signed Panel reverse proxy       |  • Zero-Trust Egress sidecar injection          |
+---------------------------------------------------------------------------------------+
                                           |
                                           v
+---------------------------------------------------------------------------------------+
|                  3. Data Plane: Hardware-Isolated Kata MicroVM Pod                    |
|   +-------------------------------------------------------------------------------+   |
|   | Kata Guest Linux Kernel (virtio-blk direct attachment)                        |   |
|   |                                                                               |   |
|   |  podd-init (PID 1 Rust Mounter):                                              |   |
|   |  1. Auto e2fsck -p / mkfs.ext4 on raw block devices                           |   |
|   |  2. Assemble OverlayFS (lowerdir=Base Image, upperdir=/dev/cubepod-rootfs)    |   |
|   |  3. Mount /workspace (/dev/cubepod-workspace) & pivot_root                   |   |
|   |                                                                               |   |
|   |  Container Merged RootFS (/)                                                  |   |
|   |  • podd (Rust Daemon :49983): Execution, background logs cursor, archive sync |   |
|   |  • Desktop Stack (Lazy-loaded): Xorg + Chromium CDP + Neko WebRTC + ttyd      |   |
|   |  • Egress Sidecar: nftables transparent redirection + mTLS secret vault swap  |   |
|   +-------------------------------------------------------------------------------+   |
+---------------------------------------------------------------------------------------+
                                           |
                                           v
+---------------------------------------------------------------------------------------+
|                    4. Persistent Storage Plane (CSI Raw Block)                        |
|  Base OCI Image (RO)    |  RootFS Upper Block PVC (Thin)  |  Workspace Block PVC      |
|  (virtio-fs / EROFS)    |  (/dev/cubepod-rootfs -> ext4)  |  (/dev/cubepod-workspace) |
|  Storage Backends: Longhorn V2 (SPDK NVMe-oF) / OpenEBS Mayastor / Local Loopback     |
+---------------------------------------------------------------------------------------+
```

---

## Key Architectural Highlights

### 1. Dual-Volume Persistent Writable RootFS
CubePod decouples system dependencies from user files by provisioning two independent CSI Raw Block PVCs (`volumeMode: Block`):
- **`rootfs-pvc`**: Carries the OverlayFS `upperdir` and `workdir`. Stores all delta modifications to `/usr`, `/etc`, and `/var` created by `apt-get`, `pip`, or `npm`.
- **`workspace-pvc`**: Mounted directly at `/workspace` (with `1000:1000` ownership). Preserves source code, artifacts, and skills.

This decoupling enables **RootFS Re-imaging without Data Loss**: developers or agent frameworks can increment `rootfsGeneration` to purge a corrupted OS environment or upgrade the base image while keeping all `/workspace` project files intact.

### 2. Zero-Export Suspend & ~1s Cross-Node Resumption
- **Suspend**: When an agent sandbox is paused or hits its idle timeout, `podd` executes `sync` + `fsfreeze` to flush kernel page caches and quiesce filesystem transactions. The Kata MicroVM Pod is immediately deleted, reducing CPU and memory consumption to **absolute zero**.
- **Resume**: Any incoming request (HTTP, command execution, or panel access) triggers `cubepod-gateway` to schedule a fresh MicroVM Pod on **any cluster node**. The CSI driver re-attaches the existing Raw Block PVCs, and `podd-init` mounts the ext4 filesystems in **~1 second** without transporting multi-gigabyte memory state over the network.

### 3. Zero Host Intrusion (`podd-init`)
Standard Kubernetes cannot mount Raw Block devices as the container rootfs without custom host-level Kubelet or containerd patches. CubePod bypasses this via **In-Guest RootFS Pivot**:
- MicroVM binaries (`podd-init` and `podd`) are injected at Pod startup via a shared `emptyDir` volume populated by an init container (`cubepod-podd`, `< 1MB`).
- `podd-init` runs as PID 1 in the Kata Guest kernel, mounts `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`, builds the OverlayFS, calls `pivot_root(2)`, and hands execution over to `podd`.
- User container images remain standard OCI images without baking in infrastructure binaries.

### 4. Pre-Attached WarmPool (`PodWarmPool`)
To achieve sub-50ms cold starts without violating Kubernetes Pod volume immutability:
- Standby MicroVMs run in `PodWarmPool` with pre-created, pre-formatted, thin-provisioned block devices already pivoted into ext4.
- When a new sandbox is requested, `cubepod-controller` re-labels and transfers ownership of the standby Pod and volumes in **< 50ms**, completely bypassing container scheduling and image pull delays.

### 5. Built-in Zero-Trust Egress & Secret Vault
- Sensitive external credentials (e.g., GitHub tokens, payment keys) **never enter the sandbox filesystem, environment variables, or process memory**.
- The sandbox receives only opaque `cpodref_` references.
- All outbound traffic is intercepted by an in-guest Egress Sidecar via `nftables`. The sidecar inspects TLS SNI, enforces domain allowlists, and dynamically exchanges `cpodref_` tokens with the egress vault over dedicated per-sandbox mTLS.

---

## E2B Drop-in Compatibility

CubePod provides **100% wire-compatible drop-in interoperability with official E2B SDKs** ([CEP-0018](docs/enhancements/0018-e2b-protocol-compatibility/README.md)). Existing AI Agent pipelines (LangChain, LlamaIndex, AutoGen, OpenHands, Smolagents) connect to CubePod without changing code—simply point `E2B_DOMAIN` to `cubepod-gateway`.

### 1. Official Python SDK (`e2b`) — Sync & Async

```bash
# Point upstream E2B SDK to your CubePod Gateway
export E2B_DOMAIN="localhost:8080"
export E2B_API_KEY="cpod_live_secret"
```

```python
# Synchronous execution
from e2b import Sandbox

with Sandbox.create("base", timeout=300) as sandbox:
    # Run command
    cmd = sandbox.commands.run("echo 'Hello from CubePod Kata MicroVM!'")
    print(cmd.stdout)  # "Hello from CubePod Kata MicroVM!"

    # Read and write files with persistent state
    sandbox.files.write("/workspace/agent_data.txt", "persistent state")
    content = sandbox.files.read("/workspace/agent_data.txt")
```

```python
# Asynchronous execution (AsyncSandbox)
import asyncio
from e2b import AsyncSandbox

async def main():
    sandbox = await AsyncSandbox.create("base")
    try:
        cmd = await sandbox.commands.run("uname -a")
        print(cmd.stdout)
    finally:
        await sandbox.kill()

asyncio.run(main())
```

### 2. Official Code Interpreter (`e2b-code-interpreter`)

CubePod provides native newline-delimited JSON (NDJSON) streaming and rich display formatters (`png`, `svg`, `html`, `json`) compatible with Jupyter protocols:

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create() as sandbox:
    code = """
import matplotlib.pyplot as plt
import pandas as pd
import numpy as np

# 1. Rich tabular DataFrame representation (_repr_html_)
df = pd.DataFrame({"A": np.random.randn(5), "B": np.random.randn(5)})

# 2. Rich graphical Matplotlib chart capture (base64 PNG)
plt.figure()
plt.plot([1, 2, 3], [4, 5, 6], label="Trajectory")
plt.title("Agent Convergence")
plt.show()

df
"""
    execution = sandbox.run_code(code)

    # Access rich outputs
    for result in execution.results:
        if result.png:
            print("Chart captured! Base64 PNG length:", len(result.png))
        if result.html:
            print("HTML table captured:\n", result.html[:100], "...")
```

### 3. Multi-Language Code Execution

`e2b-code-interpreter` on CubePod supports 6 standard execution kernels out-of-the-box:

| Language | `language` Parameter | Execution Kernel | Capabilities |
| :--- | :--- | :--- | :--- |
| **Python** | `"python"` (default) | `ipykernel` (IPython) | Matplotlib/Seaborn charts, Pandas HTML tables, LaTeX |
| **JavaScript** | `"javascript"` | `ijavascript` (Node.js) | Node.js algorithms and Web data processing |
| **TypeScript** | `"typescript"` | `tslab` / `ts-node` | Typed TypeScript script execution |
| **R** | `"r"` | `IRkernel` | Statistical analysis, `ggplot2` plotting |
| **Java** | `"java"` | `ijava` (JShell) | Java classes and JVM runtime execution |
| **Bash** | `"bash"` | `bash_kernel` | Interactive shell commands and pipeline scripts |

---

## Enterprise Superset Capabilities

Beyond the standard E2B interface, CubePod exposes high-throughput enterprise extensions for mission-critical agent platforms:

- **1-Second Copy-on-Write Sandbox Fork (`POST /sandboxes/{id}/fork`)**: Clones an active sandbox and its dual block volumes in `< 1s` using CSI snapshots. Enables sub-agents to branch off speculative code execution in complete isolation.
- **Independent RootFS Reset**: Increment `rootfsGeneration` in the `PodBox` CRD or API to rebuild the system layer after a broken `apt` or compiler update while leaving user code in `/workspace` 100% untouched.
- **Stateless Log Cursors (`GET /commands/{pid}/logs?cursor={byte_offset}`)**: Process logs are persisted to ring buffers on disk. Any gateway replica or agent worker can reconnect and resume streaming logs from arbitrary offsets after network interruptions.
- **Atomic Skill Archive Sync (`POST /files/archive-sync`)**: Streams a `tar.gz` payload unpacked in-memory directly to `/workspace/.skills` with exact `uid:gid` ownership and atomic cleanup of removed tools in a single `< 50ms` HTTP call.
- **Signed Panel & GUI Proxy (`/panel/{jwt_token}/{path}`)**: Path- and JWT-authenticated reverse proxy supporting HTTP, WebSocket, and WebRTC streaming for Chromium CDP (`:9222`), Neko browser sharing (`:8080`), and ttyd web terminal (`:7681`).

---

## Repository Map

| Path | Responsibility | Tech Stack |
| :--- | :--- | :--- |
| [`docs/architecture.md`](docs/architecture.md) | Platform architecture specification & CubePlex integration blueprint | Markdown / Mermaid |
| [`docs/enhancements/`](docs/enhancements/README.md) | CubePod Enhancement Proposals (CEPs) | CEP Specification |
| [`api/v1alpha1/`](api/v1alpha1/types.go) | Kubernetes CRD schemas (`PodBox`, `PodTemplate`, `PodWarmPool`) | Go (`controller-gen`) |
| [`api/openapi/`](api/openapi/cubepod-v1.yaml) | Canonical E2B-compatible superset OpenAPI 3.1 specification | OpenAPI 3.1 |
| [`crates/podd-init/`](crates/podd-init/src/main.rs) | In-guest PID 1 raw block `ext4` healer/formatter & `OverlayFS` `pivot_root` mounter | Rust (single-threaded, zero-GC) |
| [`crates/podd/`](crates/podd/src/main.rs) | In-guest execution, background process (`cursor` logs), and `tar.gz` file sync daemon | Rust (`tokio`, `axum`) |
| [`cmd/cubepod-controller/`](cmd/cubepod-controller/main.go) & [`pkg/controller/`](pkg/controller/) | Kubernetes Operator reconciling dual Block PVCs, WarmPools, and Kata Pods | Go (`kubebuilder`, `client-go`) |
| [`cmd/cubepod-gateway/`](cmd/cubepod-gateway/main.go) & [`pkg/gateway/`](pkg/gateway/) | Control Plane Gateway, E2B router, Auto-Resume, and Panel HTTP/WS/WebRTC Proxy | Go |
| [`cmd/cubepod-egress/`](cmd/cubepod-egress/main.go) & [`pkg/egress/`](pkg/egress/) | Zero-Trust mTLS `cpodref_` secret vault and egress forward proxy sidecar | Go |
| [`cmd/cubepod/`](cmd/cubepod/main.go) & [`pkg/client/`](pkg/client/) | Official Go Client SDK and `cubepod` CLI | Go |
| [`sdks/python/`](sdks/python/) | Sync & Async Python SDK (`cubepod`) | Python 3.11+ (`httpx`) |
| [`deploy/`](deploy/) | Helm chart (`deploy/helm/cubepod`), K8s manifests, and local Docker Compose | Helm / K8s / Docker |

---

## Quick Start (Build & Test)

### Prerequisites

- Go 1.22+
- Rust 1.78+ (cargo)
- Docker / Podman (optional for container image verification)

### Build All Binaries

```bash
# Build all components (podd-init, podd, controller, gateway, egress, cubepod CLI)
make build
```

### Run Tests

```bash
# Format and lint Go & Rust sources
make fmt
make vet

# Run all unit and integration tests
make test

# Run multi-process end-to-end integration suite (including E2B drop-in & stress tests)
make test-e2e

# Run official E2B Python SDK & Code Interpreter compatibility test suite
make test-e2b-compat
```

---

## License

CubePod is open-source software licensed under the [Apache License, Version 2.0](LICENSE).
