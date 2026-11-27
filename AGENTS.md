# AGENTS.md

## Commands

Run the relevant checks before completing a task:

```bash
# Rust & Go (core control plane and in-guest daemons)
make fmt          # Format Rust and Go
make vet          # cargo clippy (-D warnings) + go vet
make build        # Build all binaries into bin/
make test         # Run Rust and Go unit tests (with -race)
make test-e2e     # Build binaries and run multi-process E2E tests

# Component-specific checks (when touching these directories)
python3 -m unittest discover -s sdks/python/tests
cd sdks/typescript && npm test
helm lint deploy/helm/cubepod && helm template cubepod-ci deploy/helm/cubepod
make docs-build   # Build and verify website/
```

Live cluster E2E tests (requires cluster/kubeconfig): `make test-k8s-e2e` and `make test-k8s-block`.

## Architecture & Security Invariants

1. **Dual-Volume Independence (`RootFS Upper` vs. `Workspace`)**: Never couple `/workspace` (`workspace-pvc`) lifecycle to container RootFS `upperdir` (`rootfs-pvc`). Resetting `rootfsGeneration` or upgrading the base image must preserve `/workspace` intact.
2. **Single-Threaded PID 1 & Runtime Binary Injection**: Keep `podd-init` (`crates/podd-init`) strictly single-threaded prior to `pivot_root(2)` and `execvp(3)`. Never bake `podd-init` or `podd` into user sandbox images; package them in `images/podd/Dockerfile` and inject at runtime at `/opt/cubepod/bin` (bind-mounted across `pivot_root` in K8s `initContainer` `emptyDir` or local Docker bind-mount).
3. **Zero-Trust Egress Secret Safety**: Never inject real external API secrets into the sandbox environment, filesystem, or process memory. Only `cpodref_` placeholders may enter the sandbox, swapped at the egress proxy over per-sandbox mTLS.
4. **UID/GID Privilege Separation**: Infrastructure hooks (`podd-init`, browser `supervisord` bootstrap) run as `root`; agent commands and uploaded files default to `cubeplex` (`1000:1000`) without follow-up `chown` workarounds.

## Design Proposals (CEPs)

- **When required**: New features, architectural changes, CRD/API changes, or security subsystem designs must have a CEP under `docs/enhancements/NNNN-<slug>/` before implementation. Follow [`.agent/skills/cep-doc-spec/SKILL.md`](.agent/skills/cep-doc-spec/SKILL.md).
- **When NOT required**: Routine bug fixes, refactoring, tests, or wiring already-designed features.

## Conventions

- **Commits**: Conventional Commits (`type(scope): imperative summary`, e.g., `feat(podd-init): ...`, `docs(cep): ...`).
- **E2B JSON Errors**: Official E2B API routes (`/sandboxes`, `/v2/sandboxes`, and other SDK-facing handlers) must return errors as `{"code": <status int>, "message": "..."}` with `Content-Type: application/json`. Use the existing `writeError` helper in `pkg/gateway`; never use `http.Error` on these routes. The official Python SDK calls `Error.from_dict(response.json())`, so a plaintext body becomes `json.JSONDecodeError` instead of a typed `SandboxException`.
- **File Naming**: Name source and test files strictly after their module or domain capability (e.g., `manifests_test.go`, `reconciler_test.go`). Never name files after CEP, PR, or issue numbers (e.g., never `cep0020_test.go`).
- **Documentation**: Update user-facing documentation (under `website/`) alongside feature development, API/CRD changes, or behavioral changes, and run `make docs-build` to verify.
- **Version Markers**: The docs site has no per-version snapshots, so readers on older releases rely on inline markers. Follow these rules:
  - **Use `vNEXT`, never guess a version**: write `*Added in vNEXT*`, `*Changed in vNEXT*`, or `*Deprecated in vNEXT*` (Chinese: `*vNEXT 新增*`, `*vNEXT 变更*`, `*vNEXT 弃用*`). The release version-bump PR resolves them with `python3 scripts/resolve-vnext.py v<semver>`, and `scripts/check-version-consistency.sh` fails the release while any `vNEXT` remains. Never edit a marker that already names a released version.
  - **Mark reference-level surfaces only**: API endpoints and request/response fields, CLI commands and flags, MCP tool parameters, SDK public APIs, environment variables, Helm values, CRD fields, feature gates, and metrics — plus breaking/behavioral changes and deprecations of anything users already rely on.
  - **Do not mark narrative prose**: concept explanations, tutorials, guides, and internal refactors get no inline markers; describe those changes in the release changelog (`website/src/content/docs/changelog/<semver>.md`) instead. One marker per item (on its heading, table row, or list entry) is enough; don't repeat it in surrounding sentences.
  - **Prune stale markers**: markers older than the oldest supported release may be removed when touching a page.
- **State Machine Verification**: Any code change affecting state machines or lifecycle transitions (e.g., `PodBox` reconciliation, `PodWarmPool`, template/build states, lease/reaper lifecycles) must run a `/statechart` check (TLA+/TLC model check + statechart review, replaying any counterexample paths against the real code) before completion.
