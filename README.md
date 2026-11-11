# CubePod

**Persistent Writable RootFS MicroVM Execution Platform for AI Agents**

CubePod is an open-source, cloud-native execution environment designed for long-lived AI Agents. Together with **CubePlex** (Workspace Platform) and **CubeLoop** (Agent Runtime), CubePod provides hardware-isolated MicroVM execution with instant hibernation/resume and full system-level persistence.

## Key Capabilities

- **Dual-Volume Persistent Writable RootFS**: Decouples the container RootFS `upperdir` (`/usr`, `/etc`, `/var` changes from `apt`/`pip`/`npm`) and `/workspace` into two independent CSI Raw Block PVCs (`volumeMode: Block`) passed directly into Kata MicroVMs via `virtio-blk`.
- **Zero-Export Suspend & ~1s Cross-Node Resume**: Pausing a sandbox flushes filesystems (`sync` + `fsfreeze`) and deletes the MicroVM Pod (0 CPU / 0 RAM cost). Resuming schedules a fresh MicroVM on any node and mounts the existing `ext4` block volumes in ~1 second.
- **Zero Host Intrusion (`podd-init`)**: Assembles the persistent `OverlayFS` and performs `pivot_root` directly inside the Kata Guest VM kernel—requiring zero modifications to Kubelet or host `containerd`.
- **E2B-Compatible Superset API**: Drop-in compatible with standard E2B SDKs while adding enterprise extensions: independent RootFS reset (preserving `/workspace`), 1-second Copy-on-Write sandbox `fork`, stateless background command log cursors, atomic `tar.gz` skill synchronization, and signed HTTP/WS/WebRTC panel proxying.
- **Built-in Zero-Trust Egress & Secret Vault**: Native per-sandbox mTLS identity, DNS/nftables allowlisting, and just-in-time `cbxref_` placeholder secret substitution.

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
# Build all binaries (bin/podd, bin/cubepod-controller, bin/cubepod-gateway)
make build

# Run unit and integration tests
make test
```

## License

CubePod is open-source software licensed under the [Apache License, Version 2.0](LICENSE).

