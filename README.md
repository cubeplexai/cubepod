# CubePod

**Persistent Writable RootFS MicroVM Execution Platform for AI Agents**

CubePod is an open-source, cloud-native execution environment designed for long-lived AI Agents. Together with [**CubePlex**](https://github.com/cubeplexai/cubeplex) (Agent Workspace Platform) and [**CubeLoop**](https://github.com/cubeplexai/cubeloop) (Agent Runtime), CubePod provides hardware-isolated MicroVM execution with instant hibernation/resume and full system-level persistence.

## Key Capabilities

- **Dual-Volume Persistent Writable RootFS**: Decouples the container RootFS `upperdir` (`/usr`, `/etc`, `/var` changes from `apt`/`pip`/`npm`) and `/workspace` into two independent CSI Raw Block PVCs (`volumeMode: Block`) passed directly into Kata MicroVMs via `virtio-blk`.
- **Zero-Export Suspend & ~1s Cross-Node Resume**: Pausing a sandbox flushes filesystems (`sync` + `fsfreeze`) and deletes the MicroVM Pod (0 CPU / 0 RAM cost). Resuming schedules a fresh MicroVM on any node and mounts the existing `ext4` block volumes in ~1 second.
- **Zero Host Intrusion (`podd-init`)**: Assembles the persistent `OverlayFS` and performs `pivot_root` directly inside the Kata Guest VM kernel—requiring zero modifications to Kubelet or host `containerd`.
- **E2B-Compatible Superset API**: Drop-in compatible with standard E2B SDKs while adding enterprise extensions: independent RootFS reset (preserving `/workspace`), 1-second Copy-on-Write sandbox `fork`, stateless background command log cursors, atomic `tar.gz` skill synchronization, and signed HTTP/WS/WebRTC panel proxying.
- **Built-in Zero-Trust Egress & Secret Vault**: Native per-sandbox mTLS identity, DNS/nftables allowlisting, and just-in-time `cbxref_` placeholder secret substitution.

## E2B Drop-in Compatibility

CubePod implements **100% wire-compatible drop-in interoperability with official E2B SDKs** ([CEP-0018](docs/enhancements/0018-e2b-protocol-compatibility/README.md)). Existing AI Agent pipelines (LangChain, LlamaIndex, AutoGen, OpenHands, Smolagents) and custom applications written for E2B connect to CubePod without changing application code—simply point `E2B_DOMAIN` (or `E2B_API_URL` and `E2B_SANDBOX_URL`) to `cubepod-gateway`.

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

    # Read and write files
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

`e2b-code-interpreter` on CubePod supports 6 first-class standard languages out-of-the-box, plus extensible Jupyter kernels:

| Language | `language` Parameter | Execution Kernel | Capabilities |
| :--- | :--- | :--- | :--- |
| **Python** | `"python"` (default) | `ipykernel` (IPython) | Matplotlib/Seaborn rich charts, Pandas HTML tables, LaTeX |
| **JavaScript** | `"javascript"` | `ijavascript` (Node.js) | Node.js algorithms and Web data processing |
| **TypeScript** | `"typescript"` | `tslab` / `ts-node` | Typed TypeScript script execution |
| **R** | `"r"` | `IRkernel` | Statistical analysis, `ggplot2` plotting |
| **Java** | `"java"` | `ijava` (JShell) | Java classes and JVM runtime execution |
| **Bash** | `"bash"` | `bash_kernel` | Interactive shell commands and pipeline scripts |

### 4. Why Run E2B on CubePod? (Enterprise Superset)

| Feature | Upstream E2B Cloud | CubePod Open-Source |
| :--- | :--- | :--- |
| **Storage Architecture** | Ephemeral only (state lost on timeout) | **Dual CSI Block PVCs** (`rootfs` + `workspace`) |
| **Hibernation & Resume** | Cold reboot from scratch | **Zero-Export Suspend** & `< 250ms` Auto-Resume |
| **RootFS Independence** | Coupled | **Reset RootFS without losing `/workspace`** |
| **Sandbox Forking** | Not available | **`< 1s` Copy-on-Write Sub-agent Cloning** |
| **Secret Security** | Injected as plain container envs | **Zero-Trust Egress `cbxref_` mTLS Substitution** |
| **Infrastructure Deployment** | Proprietary SaaS lock-in | **Self-hosted Kubernetes (Kata MicroVMs) or Docker** |

---

## Repository Layout

- [`docs/architecture.md`](docs/architecture.md) — Architecture design, technical rationale, and integration roadmap with CubePlex.
- [`api/v1alpha1/`](api/v1alpha1/types.go) — Kubernetes CRD schemas (`PodBox`, `PodTemplate`, `PodWarmPool`).
- [`api/openapi/`](api/openapi/cubepod-v1.yaml) — Canonical E2B-compatible superset OpenAPI 3.1 specification.
- [`crates/podd-init/`](crates/podd-init/src/main.rs) — In-guest PID 1 OverlayFS `pivot_root` mounter (`podd-init`, written in Rust).
- [`crates/podd/`](crates/podd/src/main.rs) — In-guest command, process, and file execution daemon (`podd`, written in Rust).
- [`cmd/cubepod-controller/`](cmd/cubepod-controller/main.go) & [`pkg/controller/`](pkg/controller/) — Kubernetes Operator reconciling dual Block PVCs, WarmPools, and Kata Pods.
- [`cmd/cubepod-gateway/`](cmd/cubepod-gateway/main.go) & [`pkg/gateway/`](pkg/gateway/) — Unified Control Plane Gateway and Panel Reverse Proxy.
- [`sdks/python/`](sdks/python/) — Async-first `cubepod` Python SDK.
- [`deploy/`](deploy/) — Helm chart and `docker-compose` configurations.

## Quick Start (Build & Test)

```bash
# Build all binaries (bin/podd-init, bin/podd, bin/cubepod-controller, bin/cubepod-gateway)
make build

# Run unit and integration tests (Rust & Go)
make test

# Run multi-process end-to-end integration suite (including E2B fullstack drop-in & stress)
make test-e2e

# Run official E2B Python SDK & Code Interpreter compatibility test suite (22 tests)
make test-e2b-compat
```

## License

CubePod is open-source software licensed under the [Apache License, Version 2.0](LICENSE).

