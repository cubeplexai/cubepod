# CEP-0007: Local Docker and Sparse Loopback `ext4` Runtime Driver (`pkg/runtime`)

| Field | Value |
| :--- | :--- |
| **CEP Number** | `0007` |
| **Title** | Local Docker and Sparse Loopback `ext4` Runtime Driver (`pkg/runtime`) |
| **Authors** | `@cubeplex-arch` |
| **Status** | `implementable` |
| **Target Version** | `v0.2.0` |
| **Created** | `2026-11-10` |
| **Last Updated** | `2026-11-10` |

---

## Summary

This CEP introduces `pkg/runtime` with a unified `Backend` interface and two concrete implementations:
1. `LocalProcessBackend` (lightweight host-process fallback when Docker is unavailable or for fast unit tests, spawning a real `podd` daemon process on a free localhost port per sandbox with persistent `rootfs-upper/` and `workspace/` directories).
2. `DockerBackend` (spawning real Docker containers with sparse `rootfs.ext4` and `workspace.ext4` images, injecting host `/opt/cubepod/bin/{podd-init,podd}` read-only at `/opt/cubepod/bin`, and exposing `podd`'s `:49983` port to the Gateway).

---

## Motivation

In `v0.1.0`, `cubepod-gateway` executed commands directly via `os/exec` in the Gateway process. To make `v0.2.0` genuinely usable end-to-end, every sandbox must be backed by a real runtime (`pkg/runtime.Backend`) that manages persistent storage volumes (`rootfs` + `workspace`), runs the Rust `podd` daemon inside the sandbox, and returns the live `podd` endpoint URL (`http://127.0.0.1:<port>` or `http://<container-ip>:49983`) so the Gateway can proxy all data-plane operations to `podd`.

### Goals

- Define `pkg/runtime/runtime.go` with the `Backend` interface:
  - `EnsureRunning(ctx, SandboxSpec) (*InstanceInfo, error)`
  - `Pause(ctx, sandboxID) error`
  - `ResetRootFS(ctx, sandboxID) error`
  - `Fork(ctx, sourceSandboxID, newSandboxID) (*InstanceInfo, error)`
  - `Delete(ctx, sandboxID) error`
  - `Inspect(ctx, sandboxID) (*InstanceInfo, error)`
- Implement `pkg/runtime/local_podd.go` (`PoddProcessBackend`):
  - Allocates persistent per-sandbox `rootfs/` and `workspace/` directories (plus sparse `rootfs.ext4` and `workspace.ext4` image files when `EnableSparseExt4` is set).
  - Spawns the real compiled `bin/podd` binary on a dynamically allocated localhost port (`127.0.0.1:<port>`), waits for `GET /health` to return `200 OK`, and tracks the daemon PID.
  - On `Pause`, invokes `POST /v1/lifecycle/prepare-stop`, terminates the `podd` daemon process, and preserves both `rootfs/` and `workspace/` on disk.
  - On subsequent `EnsureRunning` (Auto-Resume / `connect`), restarts `bin/podd` against the same persistent directories.
  - On `ResetRootFS`, wipes `rootfs/` (and recreates `rootfs.ext4`) while preserving `workspace/` completely intact.
  - On `Fork`, copies both `rootfs/` and `workspace/` (`cp -a --reflink=auto`) to the child sandbox ID and launches a new `podd` instance.
- Implement `pkg/runtime/docker.go` (`DockerBackend`):
  - Creates sparse `rootfs.ext4` and `workspace.ext4` files on the host, formats them with `mkfs.ext4` (via `podd-init` or host `mkfs.ext4`), and runs `docker run -d` with `-v <podd-bin-dir>:/opt/cubepod/bin:ro` and entrypoint `/opt/cubepod/bin/podd-init /opt/cubepod/bin/podd`.

### Non-Goals

- Replacing the Kubernetes CRD Operator (`pkg/controller`).

---

## Proposal

### Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
| :--- | :--- | :--- | :--- |
| Orphaned `podd` child processes during test or gateway shutdown | Medium | Medium | Implement `Close()` on `PoddProcessBackend` that cleanly kills all active `podd` child processes, and send `SIGTERM`/`SIGKILL` on `Pause` and `Delete`. |

---

## Design Details

### API Changes (`pkg/runtime`)

```go
package runtime

type SandboxSpec struct {
    SandboxID        string
    TemplateID       string
    VolumeKey        string
    RootfsGeneration int64
    Image            string
    Env              map[string]string
}

type InstanceInfo struct {
    SandboxID    string
    PoddURL      string // e.g., "http://127.0.0.1:54321"
    WorkspaceDir string
    RootfsDir    string
    PID          int
    ContainerID  string
}
```

### Runtime Behavior (Production Kubernetes vs. Local Development)

- **Production Mode (`Kata + CSI Raw Block`)**: Uses Pod IP `:49983` provisioned by `cubepod-controller`.
- **Local Development Mode (`Docker / PoddProcessBackend`)**: Uses `DockerBackend` when Docker container mode is configured or `PoddProcessBackend` spawning the real Rust `bin/podd` binary directly.

### Test Plan

- **Unit & Integration Tests (`pkg/runtime/runtime_test.go`)**:
  - `TestPoddProcessBackend_LifecycleAndPersistence`: Launch a sandbox via `PoddProcessBackend` using the real `bin/podd` binary (or auto-building it), verify `/health` on `PoddURL`, write a file in `workspace` and `rootfs`, call `Pause` (verifying the `podd` process stops), call `EnsureRunning` again (Auto-Resume), and verify both files survived!
  - `TestPoddProcessBackend_ResetRootFSPreservesWorkspace`: Call `ResetRootFS`, verify `rootfs` marker is wiped while `workspace` file remains intact.
  - `TestPoddProcessBackend_CoWFork`: Call `Fork(parent, child)`, verify child has an independent copy of `rootfs` and `workspace` and its own live `PoddURL`.
  - `TestDockerBackend_CommandBuilder`: Verify `DockerBackend.BuildRunArgs` injects `-v <bin>:/opt/cubepod/bin:ro`, `--entrypoint /opt/cubepod/bin/podd-init`, and sparse image mounts without baking `podd` into the sandbox image.

### Graduation Criteria

- `go test -v -race ./pkg/runtime/...` passes all lifecycle, persistence, RootFS reset, and CoW fork tests.

### Upgrade / Downgrade Strategy

- New package `pkg/runtime`; clean additive interface.

---

## Production Readiness Review (PRR)

- **Observability**: Health-checks `GET /health` with exponential backoff up to 5s when starting `podd`.
- **Security**: `DockerBackend` mounts `/opt/cubepod/bin` read-only (`:ro`).

---

## Alternatives Considered

- **In-process mock execution in Gateway**: Rejected for `v0.2.0` because it bypasses `crates/podd` and cannot validate the real wire protocol between Gateway and `podd`.
