# CubePod Enhancement Proposals (CEPs)

This directory contains all **CubePod Enhancement Proposals (CEPs)**—the official design and architectural decision records for **CubePod**, modeled after the Kubernetes Enhancement Proposal (KEP) process.

> [!IMPORTANT]
> Every new feature, API/CRD change, storage/runtime modification, or security subsystem MUST have an approved CEP in this directory before implementation. Refer to the **[`cep-doc-spec`](../../.agent/skills/cep-doc-spec/SKILL.md)** skill for formatting rules and templates.

---

## CEP Index

| CEP # | Title | Status | Target Version | Implementation Location |
| :--- | :--- | :--- | :--- | :--- |
| **[CEP-0000](../architecture.md)** | CubePod Architecture & CubePlex Integration Blueprint | `implemented` | `v0.1.0` | Repository-wide |
| **[CEP-0001](0001-in-guest-overlayfs-pivot/README.md)** | In-Guest Persistent OverlayFS Pivot and Loopback Verification (`podd-init`) | `implemented` | `v0.1.0` | [`crates/podd-init/`](../../crates/podd-init/src/main.rs) |
| **[CEP-0002](0002-podd-execution-daemon/README.md)** | In-Guest Execution, Background Process Cursor, and Archive Sync Daemon (`podd`) | `implemented` | `v0.1.0` | [`crates/podd/`](../../crates/podd/src/main.rs) |
| **[CEP-0003](0003-controller-warmpool-and-fork/README.md)** | Dual-Block-PVC, Pre-Attached WarmPool, and CoW Fork Kubernetes Operator (`cubepod-controller`) | `implemented` | `v0.1.0` | [`pkg/controller/`](../../pkg/controller/reconciler.go) |
| **[CEP-0004](0004-gateway-and-panel-proxy/README.md)** | Unified E2B-Superset Gateway, Local Loopback Runtime, and Signed Panel Proxy (`cubepod-gateway`) | `implemented` | `v0.1.0` | [`pkg/gateway/`](../../pkg/gateway/server.go) |
| **[CEP-0005](0005-zero-trust-egress-and-sdk/README.md)** | Zero-Trust Egress Secret Injection (`cbxref_` mTLS Exchange) & Python SDK `v0.1.0` | `implemented` | `v0.1.0` | [`pkg/egress/`](../../pkg/egress/) & [`sdks/python/`](../../sdks/python/) |
| **[CEP-0006](0006-podd-archive-sync/README.md)** | `podd` Native Streaming `archive-sync` and Process Output Tail Enhancements | `implementable` | `v0.2.0` | [`crates/podd/`](../../crates/podd/src/main.rs) |
| **[CEP-0007](0007-docker-loopback-runtime/README.md)** | Local Docker and Sparse Loopback `ext4` Runtime Driver (`pkg/runtime`) | `implementable` | `v0.2.0` | `pkg/runtime/` |
| **[CEP-0008](0008-kube-controller-loop/README.md)** | Kubernetes API Client, CRD Sync Loop, and Background Reap Worker (`pkg/controller`) | `implementable` | `v0.2.0` | [`pkg/controller/`](../../pkg/controller/) & [`cmd/cubepod-controller/`](../../cmd/cubepod-controller/main.go) |
| **[CEP-0009](0009-gateway-egress-sidecar/README.md)** | Gateway Live `podd` Proxy Engine and Zero-Trust Egress Sidecar Daemon (`cmd/cubepod-egress`) | `implementable` | `v0.2.0` | [`pkg/gateway/`](../../pkg/gateway/) & [`pkg/egress/`](../../pkg/egress/) & `cmd/cubepod-egress/` |
| **[CEP-0010](0010-e2e-integration-suite/README.md)** | End-to-End Real Daemon Integration Suite and CLI Tooling (`test/e2e`) | `implementable` | `v0.2.0` | `test/e2e/` |
