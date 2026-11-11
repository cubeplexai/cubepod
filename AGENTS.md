# AGENTS.md — CubePod Engineering & Documentation Guidelines

CubePod is an open-source, cloud-native **Persistent Writable RootFS MicroVM Execution Platform for AI Agents**, built to power **CubePlex** and the broader **E2B / MCP** open-source agent ecosystem.

---

## 1. Repository Map

| Path | Responsibility | Tech Stack |
| :--- | :--- | :--- |
| [`docs/architecture.md`](docs/architecture.md) | North-star platform architecture & CubePlex integration blueprint | Markdown / Mermaid |
| [`docs/enhancements/`](docs/enhancements/README.md) | **CubePod Enhancement Proposals (CEPs)** following the K8s KEP standard | KEP YAML + Markdown |
| [`crates/podd-init/`](crates/podd-init/src/main.rs) | In-guest PID 1 raw block `ext4` healer/formatter & `OverlayFS` `pivot_root` mounter | Rust (single-threaded, zero-GC) |
| [`crates/podd/`](crates/podd/src/main.rs) | In-guest execution, background process (`cursor` logs), and `tar.gz` file sync daemon | Rust |
| [`api/v1alpha1/`](api/v1alpha1/types.go) | Kubernetes CRD schemas (`PodBox`, `PodTemplate`, `PodWarmPool`) | Go |
| [`api/openapi/`](api/openapi/cubepod-v1.yaml) | Canonical E2B-compatible superset OpenAPI 3.1 contract | OpenAPI 3.1 |
| [`cmd/cubepod-controller/`](cmd/cubepod-controller/main.go) & [`pkg/controller/`](pkg/controller/) | Kubernetes Operator managing dual `volumeMode: Block` PVCs, WarmPools, CoW Fork, and Egress | Go |
| [`cmd/cubepod-gateway/`](cmd/cubepod-gateway/main.go) & [`pkg/gateway/`](pkg/gateway/) | Stateless Control Plane Gateway, E2B router, Auto-Resume, and Panel HTTP/WS/WebRTC Proxy | Go |
| [`sdks/python/`](sdks/python/) | Async-first Python SDK (`cubepod`) | Python 3.11+ (`httpx`) |
| [`deploy/`](deploy/) | Production Helm chart (`deploy/helm/cubepod`) and local `docker-compose` | Helm / Docker |

---

## 2. Documentation & Design Workflow (KEP Standard — Mandatory)

All non-trivial features, architectural changes, CRD/API modifications, storage/runtime behaviors, and security designs MUST follow the **Kubernetes Enhancement Proposal (KEP)** specification before implementation.

- **Skill Reference**: Always activate and follow **[`kep-doc-spec`](.agent/skills/kep-doc-spec/SKILL.md)** when proposing, writing, or updating design documents.
- **Templates**:
  - Metadata template: [`.agent/skills/kep-doc-spec/templates/kep.yaml.tmpl`](.agent/skills/kep-doc-spec/templates/kep.yaml.tmpl)
  - Proposal body template: [`.agent/skills/kep-doc-spec/templates/README.md.tmpl`](.agent/skills/kep-doc-spec/templates/README.md.tmpl)
- **Location**: Place every proposal under `docs/enhancements/NNNN-<short-kebab-slug>/` (`kep.yaml` + `README.md`) and register it in [`docs/enhancements/README.md`](docs/enhancements/README.md).
- **Core KEP Invariants**:
  1. Every proposal must explicitly define **Summary**, **Motivation (Goals / Non-Goals)**, **Proposal (User Stories, Risks & Mitigations)**, **Design Details (API/CRD changes, Dual-Runtime behavior, Test Plan, Graduation Criteria, Upgrade/Downgrade Strategy)**, **Production Readiness Review (PRR)**, and **Alternatives Considered**.
  2. Use plain, direct technical language. State what literally happens in the kernel, VMM, CSI driver, or wire protocol and why.
  3. Every CEP must address both **Production Kubernetes (`Kata + CSI Raw Block`)** and **Local Development (`Docker + Loopback ext4`)**.

---

## 3. Core Architecture & Security Invariants

1. **Dual-Volume Independence (`RootFS Upper` vs. `Workspace`)**:
   - Never couple the lifecycle of `/workspace` (`workspace-pvc`) to the container RootFS `upperdir` (`rootfs-pvc`). Resetting `rootfsGeneration` or upgrading the Base Image MUST preserve `/workspace` intact.
2. **Single-Threaded Rust PID 1 for `pivot_root`**:
   - `podd-init` (`crates/podd-init`) must remain a tiny, single-threaded Rust binary prior to `pivot_root(2)` and `execvp(3)` to avoid multi-threaded runtime issues with Linux mount namespaces and root pivoting.
3. **Zero-Trust Egress Secret Safety**:
   - Real external API secrets must **never** enter the sandbox environment, filesystem, or process memory. Only `cbxref_` placeholders may be injected into the sandbox, swapped at the egress boundary over per-sandbox mTLS.
4. **UID/GID Privilege Separation**:
   - Infrastructure hooks (`podd-init`, browser supervisord bootstrap) run as `root`, while agent commands and uploaded files default to the unprivileged sandbox user (`cubeplex`, `1000:1000`) without requiring follow-up shell `chown` hacks.

---

## 4. Build, Verification & Code Quality Rules

Always verify both Rust and Go toolchains before claiming a task is complete:

```bash
# Format all Rust and Go code
make fmt

# Run Rust clippy and Go vet
make vet

# Build all binaries (bin/podd-init, bin/podd, bin/cubepod-controller, bin/cubepod-gateway)
make build

# Run all Rust and Go unit/integration tests
make test
```

- **Conventional Commits**: Use `type(scope): imperative summary` (e.g., `feat(podd-init): ...`, `docs(cep): ...`).
