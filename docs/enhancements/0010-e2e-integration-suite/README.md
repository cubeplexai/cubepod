# CEP-0010: End-to-End Real Daemon Integration Suite and CLI Tooling (`test/e2e`)

| Field | Value |
| :--- | :--- |
| **CEP Number** | `0010` |
| **Title** | End-to-End Real Daemon Integration Suite and CLI Tooling (`test/e2e`) |
| **Authors** | `@cubeplex-arch` |
| **Status** | `implemented` |
| **Target Version** | `v0.2.0` |
| **Created** | `2026-11-10` |
| **Last Updated** | `2026-11-10` |

---

## Summary

This CEP delivers an automated End-to-End (E2E) integration test suite (`test/e2e/e2e_test.go`) and Python SDK E2E verification (`sdks/python/tests/test_e2e_live.py`) that boots the real compiled Rust binaries (`bin/podd-init` and `bin/podd`), the real Go `cubepod-controller` + `cubepod-gateway` stack, and verifies the full lifecycle, persistent sparse `ext4` volume formatting/healing, background cursor logs, `archive-sync`, `/panel/{token}/` reverse proxy, and zero-trust `cbxref_` mTLS egress secret exchange in one cohesive system test.

---

## Motivation

Unit tests validate individual packages in isolation. Before cutting `v0.2.0` and integrating CubePod into CubePlex, we need a deterministic, automated end-to-end test suite (`test/e2e`) that exercises the real compiled Rust `podd-init` and `podd` binaries together with the Go Control Plane Gateway, Controller, Egress Vault, and Python SDK over real TCP sockets.

### Goals

- Create `test/e2e/e2e_test.go` running real multi-process integration tests:
  1. **`TestE2E_PoddInitAndSparseExt4Lifecycle`**: Executes the real compiled `bin/podd-init` binary against a 64MB sparse block image with `CUBEPOD_SKIP_PIVOT=1` and direct `ensure_ext4` verification, confirming `init-state.json` is written and `bin/podd` is `execvp`'d as child process serving `:49983`.
  2. **`TestE2E_GatewayToRealPoddDaemon`**: Starts a real `bin/podd` Rust daemon process on a live TCP port, wires `cubepod-gateway` to it, and exercises foreground commands, background commands with `?cursor=N` incremental log polling, process group `kill`, `PUT/GET /files`, `.tar.gz` `archive-sync`, `pause` + transparent Auto-Resume, `reset-rootfs` preserving `/workspace`, CoW `fork`, and `/panel/{token}/` proxying to an HTTP server started *inside* `podd`!
  3. **`TestE2E_PythonSDKAgainstLiveGateway`**: Runs the Python `AsyncCubePod` SDK (`sdks/python`) in a child `python3` process talking over real HTTP to the live Go `cubepod-gateway` + Rust `podd` stack!
- Add `make test-e2e` target to `Makefile`.

### Non-Goals

- Requiring privileged root access or cluster-admin Kubernetes credentials to run `make test-e2e` in CI.

---

## Proposal

### Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
| :--- | :--- | :--- | :--- |
| Port collisions during parallel E2E runs | Low | Medium | Bind all test servers (`podd`, `cubepod-gateway`, in-sandbox web server) to `127.0.0.1:0` (ephemeral OS-assigned ports). |

---

## Design Details

### E2E Architecture Flow

```
[Python SDK (AsyncCubePod)]
         │ (real HTTP over 127.0.0.1:<gw-port>)
         ▼
[Go cubepod-gateway + cubepod-controller]
         │ (real HTTP over 127.0.0.1:<podd-port>)
         ▼
[Rust bin/podd + bin/podd-init]
         │
         ├── Spawns background HTTP server on 127.0.0.1:<app-port> (verified via /panel/{token}/)
         └── Reads/writes persistent /workspace and RootFS files
```

### Test Plan

- Run `go test -v -race ./test/e2e/...` and `make test`.

### Graduation Criteria

- `make test` runs Rust unit tests, Go unit tests, and the full `test/e2e` multi-language (Python -> Go -> Rust) integration suite with 0 failures.

### Upgrade / Downgrade Strategy

- Additive `test/e2e` package.

---

## Production Readiness Review (PRR)

- **Observability**: Logs exact round-trip latencies for `podd-init` startup, `podd` command execution, Auto-Resume, and `/panel/{token}/` proxy requests.

---

## Alternatives Considered

- **Manual shell script testing only**: Rejected because `go test ./test/e2e/...` runs automatically as part of `make test` with race detection enabled.
