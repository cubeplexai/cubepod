# AGENTS.md — CubePod Engineering & Documentation Guidelines

CubePod is an open-source, cloud-native **Persistent Writable RootFS MicroVM Execution Platform for AI Agents**, built to power **CubePlex** and the broader **E2B / MCP** open-source agent ecosystem.

---

## 1. Repository Map

| Path | Responsibility | Tech Stack |
| :--- | :--- | :--- |
| [`docs/architecture.md`](docs/architecture.md) | North-star platform architecture & CubePlex integration blueprint | Markdown / Mermaid |
| [`docs/enhancements/`](docs/enhancements/README.md) | **CubePod Enhancement Proposals (CEPs)** | CEP YAML + Markdown |
| [`crates/podd-init/`](crates/podd-init/src/main.rs) | In-guest PID 1 raw block `ext4` healer/formatter & `OverlayFS` `pivot_root` mounter | Rust (single-threaded, zero-GC) |
| [`crates/podd/`](crates/podd/src/main.rs) | In-guest execution, background process (`cursor` logs), and `tar.gz` file sync daemon | Rust |
| [`api/v1alpha1/`](api/v1alpha1/types.go) | Kubernetes CRD schemas (`PodBox`, `PodTemplate`, `PodWarmPool`) | Go |
| [`api/openapi/`](api/openapi/cubepod-v1.yaml) | Canonical E2B-compatible superset OpenAPI 3.1 contract | OpenAPI 3.1 |
| [`cmd/cubepod-controller/`](cmd/cubepod-controller/main.go) & [`pkg/controller/`](pkg/controller/) | Kubernetes Operator managing dual `volumeMode: Block`/`Filesystem` PVCs, WarmPools, CoW Fork, and Egress | Go |
| [`cmd/cubepod-gateway/`](cmd/cubepod-gateway/main.go) & [`pkg/gateway/`](pkg/gateway/) | Stateless Control Plane Gateway, E2B router, Auto-Resume, and Panel HTTP/WS/WebRTC Proxy | Go |
| [`cmd/cubepod-egress/`](cmd/cubepod-egress/main.go) & [`pkg/egress/`](pkg/egress/) | Zero-Trust mTLS `cpodref_` secret vault and egress forward proxy sidecar | Go |
| [`cmd/cubepod/`](cmd/cubepod/main.go) & [`pkg/client/`](pkg/client/) | Official Go Client SDK and `cubepod` CLI | Go |
| [`sdks/python/`](sdks/python/) | Sync & Async Python SDK (`cubepod`) | Python 3.11+ (`httpx`) |
| [`deploy/`](deploy/) | Production Helm chart (`deploy/helm/cubepod`), K8s manifests (`deploy/k8s`), and local `docker-compose` | Helm / K8s / Docker |

---

## 2. Documentation & Design Workflow (CEP Standard — Feature Designs Only)

New product features, architectural changes, CRD/API modifications, and security subsystem designs MUST follow the **CubePod Enhancement Proposal (CEP)** specification before implementation.
- **When NOT to write a CEP**: Routine engineering tasks, bug fixes, code refactoring, test/verification additions, or wiring up already-designed features do **not** require a new CEP—decompose and dispatch them directly to `herdr` workers.

- **Skill Reference**: Always activate and follow **[`cep-doc-spec`](.agent/skills/cep-doc-spec/SKILL.md)** when proposing, writing, or updating design documents.
- **Templates**:
  - Metadata template: [`.agent/skills/cep-doc-spec/templates/cep.yaml.tmpl`](.agent/skills/cep-doc-spec/templates/cep.yaml.tmpl)
  - Proposal body template: [`.agent/skills/cep-doc-spec/templates/README.md.tmpl`](.agent/skills/cep-doc-spec/templates/README.md.tmpl)
- **Location**: Place every proposal under `docs/enhancements/NNNN-<short-kebab-slug>/` (`cep.yaml` + `README.md`) and register it in [`docs/enhancements/README.md`](docs/enhancements/README.md).
- **Core CEP Invariants**:
  1. Every proposal must explicitly define **Summary**, **Motivation (Goals / Non-Goals)**, **Proposal (User Stories, Risks & Mitigations)**, **Design Details (API/CRD changes, Dual-Runtime behavior, Test Plan, Graduation Criteria, Upgrade/Downgrade Strategy)**, **Production Readiness Review (PRR)**, and **Alternatives Considered**.
  2. Use plain, direct technical language. State what literally happens in the kernel, VMM, CSI driver, or wire protocol and why.
  3. Every CEP must address both **Production Kubernetes (`Kata + CSI Raw Block`)** and **Local Development (`Docker + Loopback ext4`)**.

---

## 3. Core Architecture & Security Invariants

1. **Dual-Volume Independence (`RootFS Upper` vs. `Workspace`)**:
   - Never couple the lifecycle of `/workspace` (`workspace-pvc`) to the container RootFS `upperdir` (`rootfs-pvc`). Resetting `rootfsGeneration` or upgrading the Base Image MUST preserve `/workspace` intact.
2. **Single-Threaded Rust PID 1 & Runtime Binary Injection (`podd-init` / `podd`)**:
   - `podd-init` (`crates/podd-init`) must remain a tiny, single-threaded Rust binary prior to `pivot_root(2)` and `execvp(3)` to avoid multi-threaded runtime issues with Linux mount namespaces and root pivoting.
   - **Never bake `podd-init` or `podd` into user sandbox images.** They must be packaged in the dedicated `cubepod-podd` injector image (`images/podd/Dockerfile`) and injected at Pod startup via an `initContainer` shared `emptyDir` volume mounted at `/opt/cubepod/bin` (or read-only host bind-mount in local Docker mode). `podd-init` bind-mounts `/opt/cubepod` across `pivot_root` so any stock OCI image works out-of-the-box without polluting the user's persistent RootFS `upperdir`.
3. **Zero-Trust Egress Secret Safety**:
   - Real external API secrets must **never** enter the sandbox environment, filesystem, or process memory. Only `cpodref_` placeholders may be injected into the sandbox, swapped at the egress boundary over per-sandbox mTLS.
4. **UID/GID Privilege Separation**:
   - Infrastructure hooks (`podd-init`, browser supervisord bootstrap) run as `root`, while agent commands and uploaded files default to the unprivileged sandbox user (`cubeplex`, `1000:1000`) without requiring follow-up shell `chown` hacks.

---

## 4. Controller & Parallel `herdr` Worker Orchestration Workflow

When operating as the **Controller Agent**, follow this strict multi-agent delegation workflow:

### 4.1 Domain-Classified `herdr` Agent Roster (Workspace `w5`)

| Pane ID | Agent Name | Role & Architectural Domain | Owned Paths |
| :--- | :--- | :--- | :--- |
| `w5:p1` | **`cubepod-lead`** | **Controller / Lead Architect**: CEP authoring, task decomposition, worktree orchestration, PR review (`gh pr diff`/`comment`), merge, and release publishing | `docs/enhancements/`, `AGENTS.md`, `README.md` |
| `w5:p2` | **`cubepod-runtime`** | **In-Guest Runtime & Local Driver**: Rust `podd-init` (PID 1 `OverlayFS`/`ext4`), Rust `podd` execution/archive daemon, and local/Docker runtime driver | `crates/podd-init/`, `crates/podd/`, `pkg/runtime/` |
| `w5:p3` | **`cubepod-gateway`** | **Control Plane Gateway, Panel Proxy & Zero-Trust Egress**: E2B-compatible HTTP/SSE router, `/panel/{token}/` proxy, `cpodref_` mTLS Egress, and OpenAPI 3.1 contract | `pkg/gateway/`, `pkg/egress/`, `cmd/cubepod-gateway/`, `cmd/cubepod-egress/`, `api/openapi/` |
| `w5:p4` | **`cubepod-sdk`** | **Client SDKs & CLI**: Sync/Async Python SDK, Go Client SDK, and `cubepod` CLI | `sdks/python/`, `pkg/client/`, `cmd/cubepod/` |
| `w5:p5` | **`cubepod-operator`** | **Kubernetes Operator, Packaging & CI/CD**: `PodBox`/`PodWarmPool` reconciler, `RESTKubeClient`, Helm chart, CRDs, Dockerfiles, and GitHub Actions | `pkg/controller/`, `cmd/cubepod-controller/`, `api/v1alpha1/`, `deploy/helm/`, `images/`, `.github/workflows/` |
| `w5:p6` | **`cubepod-k8s-qa`** | **Kubernetes Cluster Deployment & E2E Verification**: Live `kubectl` cluster deployment, image builds/pushes, multi-process E2E, and live K8s E2E suites | `deploy/k8s/`, `test/e2e/`, `test/k8s-e2e/`, `Makefile` |

### 4.2 Orchestration Rules

1. **Strict Role Separation (Controller vs. `herdr` Workers)**:
   - **Controller Agent Responsibilities**: Authoring CEPs (`docs/enhancements/NNNN-*/`), decomposing milestones into parallelizable tasks with non-overlapping file ownership, creating isolated Git worktrees, dispatching `herdr` workers (`agy --model gemini-3.8-flash-medium --mode auto --dangerously-skip-permissions`), reviewing PRs (`gh pr diff` / `gh pr comment`), merging PRs (`gh pr merge --squash`), and cutting releases.
   - **Delegate All Execution & K8s Verification Details to Workers**: Never perform low-level coding, container image builds, or interactive Kubernetes cluster (`kubectl`) debugging directly inside the Controller session. Always assign a `herdr` worker session to execute implementation details, build/push test images, deploy manifests to the Kubernetes test cluster, and run `test/k8s-e2e`.

2. **Mandatory `/new` Context Reset When Reusing Worker Sessions (Token Efficiency)**:
   - Whenever reusing an existing `herdr` worker pane for a new task or CEP that is unrelated to its previous conversation turn, **you MUST first reset its session with `/new`** before sending the new prompt. Failing to run `/new` carries over the entire prior conversation context and wastes massive amounts of tokens:
     ```bash
     # 1. Reset worker conversation history before a new unrelated task
     herdr agent prompt <worker-name> "/new"
     sleep 2

     # 2. Dispatch the new self-contained task prompt
     herdr agent prompt <worker-name> "<task-prompt>"
     ```

3. **Isolated Git Worktrees & Non-Overlapping File Ownership**:
   - Always create a dedicated Git worktree per parallel worker (`/tmp/cubepod-wt-cep<N>` on branch `feat/cep-<NNNN>-<slug>`) so concurrent workers never contend on `.git/index.lock` or overwrite each other's working tree:
     ```bash
     git worktree add -b feat/cep-0011-example /tmp/cubepod-wt-cep11 main
     ```
   - Partition file paths across concurrent workers so each worker owns distinct packages/directories.

4. **Pull Request Review & Merge Gate**:
   - Each worker must format, lint, test, commit, push its branch, and open a GitHub Pull Request (`gh pr create`) from its worktree.
   - The Controller Agent inspects `gh pr diff <id>`, verifies or delegates fixups if needed, leaves a structured review comment (`gh pr comment <id>`), merges into `main` (`gh pr merge <id> --squash`), and prunes completed worktrees (`git worktree remove --force ... && git worktree prune`).

---

## 5. Build, Verification & Code Quality Rules

Always verify both Rust and Go toolchains before claiming a task is complete:

```bash
# Format all Rust and Go code
make fmt

# Run Rust clippy and Go vet
make vet

# Build all binaries (bin/podd-init, bin/podd, bin/cubepod-controller, bin/cubepod-gateway, bin/cubepod-egress, bin/cubepod)
make build

# Run all Rust and Go unit/integration tests
make test

# Run multi-process E2E suite
make test-e2e
```

- **Conventional Commits**: Use `type(scope): imperative summary` (e.g., `feat(podd-init): ...`, `docs(cep): ...`).
