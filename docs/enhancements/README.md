# CubePod Enhancement Proposals (CEPs)

This directory contains **CubePod Enhancement Proposals (CEPs)**.
Before writing or modifying any CEP, read and follow the skill at [`.agent/skills/cep-doc-spec/SKILL.md`](../../.agent/skills/cep-doc-spec/SKILL.md).

## Proposal Index (`v0.1` Roadmap)

| Number | Title | Owning Area | Status | Stage |
| :--- | :--- | :--- | :--- | :--- |
| [CEP-0001](./0001-in-guest-overlayfs-pivot/README.md) | In-Guest Persistent OverlayFS Pivot and Loopback Verification (`podd-init`) | `runtime` | `implemented` | `alpha` |
| [CEP-0002](./0002-podd-execution-daemon/README.md) | In-Guest Execution, Background Process Cursor, and Archive Sync Daemon (`podd`) | `runtime` | `implemented` | `alpha` |
| [CEP-0003](./0003-controller-warmpool-and-fork/README.md) | Dual-Block-PVC, Pre-Attached WarmPool, and CoW Fork Kubernetes Operator (`cubepod-controller`) | `controller` | `implemented` | `alpha` |
| [CEP-0004](./0004-gateway-and-panel-proxy/README.md) | Unified E2B-Superset Gateway, Local Loopback Runtime, and Signed Panel Proxy (`cubepod-gateway`) | `gateway` | `implemented` | `alpha` |
| [CEP-0005](./0005-zero-trust-egress-and-sdk/README.md) | Zero-Trust Egress Secret Injection (`cbxref_` mTLS Exchange) & Python SDK `v0.1.0` | `egress` / `sdk` | `implemented` | `alpha` |
