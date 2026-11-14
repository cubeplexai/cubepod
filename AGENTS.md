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
- **File Naming**: Name source and test files strictly after their module or domain capability (e.g., `manifests_test.go`, `reconciler_test.go`). Never name files after CEP, PR, or issue numbers (e.g., never `cep0020_test.go`).
