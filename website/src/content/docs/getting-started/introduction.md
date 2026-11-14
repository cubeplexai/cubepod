---
title: Introduction
description: Learn what CubePod is, why long-running AI agents need persistent writable RootFS MicroVMs, and how CubePod unifies E2B compatibility with Kubernetes-native isolation.
---

**CubePod** is an open-source, cloud-native **Persistent Writable RootFS MicroVM Execution Platform for AI Agents**, built to power **CubePlex** and the broader **E2B / MCP** open-source ecosystem.

Unlike stateless container runners or ephemeral sandboxes that discard installed dependencies, browser state, and system configuration whenever a session pauses or restarts, CubePod gives every AI agent a hardware-isolated sandbox whose **entire OS filesystem (`/`, `/usr`, `/etc`, `/var/lib`) and user project directory (`/workspace`) both persist independently**.

## Why Persistent Writable RootFS Matters

Modern autonomous agents do far more than evaluate isolated Python snippets. Over multi-hour or multi-day tasks they install system packages (`apt`, `pip`, `npm`, Playwright browsers), configure systemd or background daemons, authenticate CLI tools, compile code, and edit repositories.

Standard sandbox platforms leave you with an awkward trade-off:

- **Ephemeral MicroVMs** offer strong hypervisor isolation, but their RootFS lives only in RAM/scratch layers — pausing or restarting wipes OS packages and system state unless you rebuild custom base images on every mutation.
- **Persistent Volume Containers** keep `/workspace` alive, but lack hardware VM boundaries and force agents to reinstall OS packages or lose system config across pod restarts.

CubePod removes this compromise by booting **stock, unmodified OCI images** inside **Kata Containers MicroVMs** backed by **dual persistent volumes**: one volume holds an `OverlayFS` writable `upperdir` for the entire Linux RootFS, while a second volume exclusively stores `/workspace`.

## Platform Comparison

| Capability | Plain Containers | Ephemeral MicroVMs | CubePod |
| :--- | :--- | :--- | :--- |
| **Compute Isolation** | Shared host Linux kernel | Hardware VM (`Firecracker` / `QEMU`) | Hardware VM (`Kata QEMU` / `Cloud Hypervisor`) + local Docker loopback |
| **OS RootFS Persistence** | Ephemeral writable layer | Ephemeral scratch layer | **Persistent `OverlayFS` `upperdir`** (`<box>-rootfs-pvc`) |
| **Workspace Persistence** | Optional PVC mount | Optional or lost on stop | **Decoupled persistent volume** (`<box>-workspace-pvc`) |
| **RootFS Reset / OS Upgrade** | Recreate container | Image rebuild | Monotonic `rootfsGeneration` swap — **`/workspace` stays intact** |
| **Cold Start Latency** | ~1–3 s | ~150 ms | **`< 50 ms`** via pre-attached `PodWarmPool` CAS claims |
| **Compute Pause-to-Zero** | Deletes state on stop | Snapshot or ephemeral stop | `desiredState: Paused` tears down compute (`0 CPU / 0 RAM`), keeps dual PVCs |
| **State Cloning (CoW Fork)** | Slow image commit | VM snapshot | **Crash-consistent CoW fork** (`FIFREEZE` -> CSI clone / reflink -> `FITHAW`) |
| **Secret & Egress Safety** | Real env vars inside sandbox | Real env vars inside sandbox | **`cpodref_` placeholders** + selective MITM over per-pod mTLS |
| **SDK & Protocol Ecosystem** | Custom APIs | E2B REST / Connect-RPC | **100% E2B drop-in compatible** + native Python, TS, Go, CLI, MCP |

## Core Architectural Pillars

1. **[Dual-Volume Persistence](/concepts/dual-volume-persistence/)**: Isolates the writable OS RootFS (`<box>-rootfs-pvc` or `<box>-rootfs-gen<N>-pvc`) from user project files (`<box>-workspace-pvc` mounted at `/workspace`). Incrementing `rootfsGeneration` wipes or upgrades OS mutations without losing a byte in `/workspace`.
2. **[Clean OCI Images via `podd-init` & `podd`](/concepts/in-guest-init-and-podd/)**: Runtime binaries are never baked into user images. An `initContainer` (`podd-installer`) or host bind-mount injects `/opt/cubepod/bin/{podd-init,podd}` at startup. A single-threaded Rust PID 1 (`podd-init`) checks/heals `ext4`, mounts `OverlayFS`, pivots root (`pivot_root`), and execs into `podd` (`:49983`).
3. **[WarmPool, Pause-to-Zero, CoW Fork & Stateless HA](/concepts/lifecycle-warmpool-fork/)**: `PodWarmPool` pre-attaches standby MicroVMs claimed atomically in `< 50 ms`. `spec.forkFrom` performs crash-consistent cloning (`FIFREEZE`/`FITHAW` + CSI or `cp --reflink=auto`), and `cubepod-gateway` reconstructs state directly from Kubernetes `PodBox` CRDs for multi-replica HA.
4. **[Drop-in E2B Compatibility](/getting-started/e2b-compatibility/)**: Point `E2B_DOMAIN` or `E2B_API_URL` at `cubepod-gateway` so existing `e2b`, `e2b-code-interpreter`, `@e2b/code-interpreter`, LangChain, LlamaIndex, and OpenHands workflows run unmodified.
5. **[Zero-Trust Egress & Secret Vault](/concepts/zero-trust-egress/)**: Real API keys never enter sandbox memory or disk. Only `cpodref_` 32-character Base32 placeholders are injected; `cubepod-egress` intercepts outbound traffic (`nftables` + DNS proxy + selective HTTPS MITM) and swaps placeholders over per-pod mTLS against `cubepod-gateway`.
6. **[Lazy Interactive Desktop & Signed Panel Proxy](/concepts/desktop-and-panel-proxy/)**: GUI images start with ~0 MiB desktop RAM overhead and launch `Xvfb`, `openbox`, Chromium CDP (`:9222`), `x11vnc`/`noVNC` (`:8080`), and `ttyd` (`:7681`) on demand, exposed securely via HMAC-SHA256 `/panel/{token}/` URLs.

## Dual-Runtime Fidelity

Every CubePod subsystem runs with behavioral parity across two environments:

- **Production Kubernetes (`Kata + CSI Block`)**: Hardware-isolated Pods (`kata-qemu` / `kata-clh`) attaching two `volumeMode: Block` PVCs (`/dev/cubepod-rootfs` and `/dev/cubepod-workspace`), with `volumeMode: Filesystem` fallback when CSI raw block storage is unavailable.
- **Local Development (`Docker + Loopback ext4`)**: Running `CUBEPOD_RUNTIME_MODE=docker-loopback` (including via Docker Compose) backs each sandbox with sparse `.ext4` loopback files and runs the exact same `podd-init` + `podd` in-guest stack on your laptop.

## Next Steps

- Launch a local stack and run your first persistent sandbox in the **[Quickstart](/getting-started/quickstart/)**.
- Connect existing E2B agents with **[E2B Drop-in Compatibility](/getting-started/e2b-compatibility/)**.
- Dive into the control and data plane in **[Platform Architecture](/concepts/architecture/)**.
- Deploy to production clusters with **[Kubernetes & Helm Deployment](/deployment/kubernetes-helm/)**.
