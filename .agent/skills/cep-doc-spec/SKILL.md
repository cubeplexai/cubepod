---
name: cep-doc-spec
description: >-
  CubePod Enhancement Proposal (CEP) and technical documentation writing
  specification modeled after Kubernetes KEPs. Activate this skill whenever
  drafting, reviewing, or updating architectural proposals, feature
  enhancements, CRD/API changes, or technical design documents in CubePod.
---

# CubePod Enhancement Proposal (CEP) Documentation Specification (`cep-doc-spec`)

CubePod adopts the **CubePod Enhancement Proposal (CEP)** standard (modeled after Kubernetes KEPs) for all non-trivial architecture changes, CRD/API modifications, runtime/storage mechanisms, and security boundaries.

Every feature or architectural design in CubePod must be documented as a **CEP** under `docs/enhancements/` before implementation begins.

---

## 1. Directory Layout & File Naming

All enhancement proposals live under `docs/enhancements/`:

```text
docs/
├── architecture.md                                  # High-level platform overview & north-star blueprint
└── enhancements/
    ├── README.md                                    # Index table of all CEPs (number, title, status, stage)
    └── NNNN-<short-kebab-slug>/                     # e.g., 0001-in-guest-overlayfs-pivot/
        ├── cep.yaml                                 # Machine-readable CEP metadata
        └── README.md                                # The enhancement proposal body
```

### Rules for Naming & Numbering
1. **Four-digit zero-padded number (`NNNN`)**: Sequential (`0001`, `0002`, ...). Check `docs/enhancements/README.md` for the next available number.
2. **Short kebab-case slug**: Descriptive, lowercase, hyphen-separated (e.g., `0001-in-guest-overlayfs-pivot`, `0002-preattached-block-warmpool`).
3. **Update the Index**: Every new or updated proposal MUST update `docs/enhancements/README.md`.

---

## 2. Metadata Specification (`cep.yaml`)

Every proposal folder `docs/enhancements/NNNN-<slug>/` MUST contain a `cep.yaml` file with the following schema:

```yaml
title: "<Concise Title of the Enhancement>"
cep-number: "NNNN"
authors:
  - "@<github-handle>"
owning-area: "<area: runtime | storage | controller | gateway | egress | sdk>"
participating-areas:
  - "<optional secondary areas>"
status: "<provisional | implementable | implemented | deferred | rejected | withdrawn | replaced>"
creation-date: "YYYY-MM-DD"
reviewers:
  - "@<github-handle>"
approvers:
  - "@<github-handle>"
stage: "<alpha | beta | stable>"
latest-milestone: "v0.X"
milestone:
  alpha: "v0.1"
  beta: "v0.2"
  stable: "v1.0"
feature-gates:
  - name: "<PascalCaseFeatureName>"
    components:
      - "podd-init"
      - "podd"
      - "cubepod-controller"
      - "cubepod-gateway"
disable-supported: true
metrics:
  - "<prometheus_metric_name>"
```

### Status Lifecycle Definitions
- `provisional`: Problem statement and goals agreed upon; design details being iterated.
- `implementable`: Design, graduation criteria, and test plan approved; ready for coding.
- `implemented`: Code merged and verified for the current `stage` (`alpha`, `beta`, or `stable`).
- `deferred` / `rejected` / `withdrawn` / `replaced`: Terminal or paused states (must record rationale in the document).

---

## 3. Document Body Structure (`README.md`)

Every `docs/enhancements/NNNN-<slug>/README.md` MUST follow the canonical CEP structure below (adapted from [`templates/README.md.tmpl`](./templates/README.md.tmpl)). Do not skip required sections; if a section does not apply, explicitly write `N/A — <brief reason>`.

### Required Sections Checklist

1. **Header & Table of Contents**
   - Title: `# CEP-NNNN: <Title>`
   - Status table (`Stage`, `Status`, `Created`, `Updated`, `Owning Area`)
2. **`## Release Signoff Checklist`**
   - Markdown checkboxes verifying scope, API compatibility, security review, test plan, and graduation criteria.
3. **`## Summary`**
   - 1–2 paragraphs written in plain, jargon-free language summarizing *what* is changing and *why* it matters to users/operators.
4. **`## Motivation`**
   - Context, concrete pain points in existing systems (e.g., CubePlex workloads, Kata MicroVMs, CSI Block storage), and why existing workarounds are insufficient.
   - **`### Goals`**: Bullet points of specific, verifiable outcomes this CEP will achieve.
   - **`### Non-Goals`**: Explicit scope boundaries to prevent scope creep (YAGNI).
5. **`## Proposal`**
   - High-level architectural overview of the solution.
   - **`### User Stories`**: Concrete scenarios (`Story 1: As a CubePlex workspace user...`, `Story 2: As a cluster operator...`).
   - **`### Notes / Constraints / Caveats`**: Kernel/VMM/CSI assumptions (e.g., Kata `privileged_without_host_devices`, ext4 journal behavior).
   - **`### Risks and Mitigations`**: Tabular breakdown of failure modes, security risks (container/VM escape, secret leakage), data loss risks, and exact mitigations.
6. **`## Design Details`**
   - Deep technical specification:
     - **API / CRD / Wire Changes**: Exact Go structs (`api/v1alpha1`), OpenAPI schemas (`api/openapi`), or `podd` endpoints.
     - **State Machine & Data Flow**: Mermaid sequence or state diagrams (`flowchart` or `stateDiagram-v2`).
     - **Boundary Conditions & Failure Recovery**: What happens on crash, network partition, unclean Pod kill, CSI detach timeout, or out-of-disk?
   - **`### Test Plan`** *(Mandatory — E2E & Invariant Driven)*:
     - **Unit Tests**: Edge cases and state transitions covered in Rust (`cargo test`) and Go (`go test`).
     - **Integration / E2E Tests**: Real business-flow invariants verified against actual block devices, Kata runtime, or local loopback ext4 mounts.
   - **`### Graduation Criteria`**:
     - Criteria required to enter **Alpha** (initial working implementation behind flag/default off), **Beta** (enabled by default, E2E verified, metrics/recoverability complete), and **GA / Stable** (production bake time in CubePlex).
   - **`### Upgrade / Downgrade Strategy`**:
     - Impact on existing `RootFS Upper PVC` and `Workspace PVC` data, CRD schema compatibility, and base image upgrades.
7. **`## Production Readiness Review Questionnaire (PRR)`**
   - **Feature Enablement and Rollback**: How can an operator enable/disable or roll back this feature without losing `/workspace` data?
   - **Rollout, Upgrade and Rollback Planning**: How do in-flight or `Paused` sandboxes behave during controller/gateway rollout?
   - **Monitoring Requirements**: Logs, metrics, and health checks that prove the feature is working or failing.
   - **Dependencies**: External CSI drivers (Longhorn V2 / Mayastor), kernel modules (`overlay`, `ext4`), or Kata versions required.
   - **Scalability**: Impact on API server, CSI attach/detach concurrency, and node density.
   - **Troubleshooting**: How an operator diagnoses and recovers from a stuck state (e.g., `Pausing` stuck, `e2fsck` failure).
8. **`## Implementation History`**
   - Dated log of major milestones (Proposal drafted, marked `implementable`, Alpha merged, etc.).
9. **`## Drawbacks`**
   - Honest assessment of why we might *not* want to do this (complexity, resource overhead).
10. **`## Alternatives`**
    - Every rejected approach MUST be documented here with concrete technical reasons why it was not chosen (e.g., CRIU checkpointing vs. OCI commit vs. NFS PVC vs. Raw Block OverlayFS).

---

## 4. Writing Style & Quality Rules

1. **Plain, Direct Language**: State what literally happens in the kernel, controller, or wire protocol and why. Avoid marketing fluff or invented buzzwords.
2. **Concrete Code & Wire Examples**: Always show exact YAML snippets, JSON payloads, or syscall sequences rather than vague prose.
3. **Dual-Runtime Awareness**: Every design MUST state how it behaves in both **Production Kubernetes (`Kata + CSI Block`)** and **Local Development (`Docker + Loopback ext4`)**.
4. **Zero-Trust & Data Safety First**: Never propose a design that puts raw user secrets inside the sandbox environment or risks corrupting the `/workspace` PVC during a `RootFS` reset.
