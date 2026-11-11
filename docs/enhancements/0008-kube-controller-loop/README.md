# CEP-0008: Kubernetes API Client, CRD Sync Loop, and Background Reap Worker (`pkg/controller`)

| Field | Value |
| :--- | :--- |
| **CEP Number** | `0008` |
| **Title** | Kubernetes API Client, CRD Sync Loop, and Background Reap Worker (`pkg/controller`) |
| **Authors** | `@cubeplex-arch` |
| **Status** | `implementable` |
| **Target Version** | `v0.2.0` |
| **Created** | `2026-11-10` |
| **Last Updated** | `2026-11-10` |

---

## Summary

This CEP upgrades `cubepod-controller` (`pkg/controller` and `cmd/cubepod-controller`) from a purely passive in-memory reconciler into a complete Kubernetes Operator with:
1. A pluggable `KubeClient` interface and lightweight HTTP/REST Kubernetes API client (`pkg/controller/kubeclient.go`) capable of applying/deleting `PersistentVolumeClaim` (`volumeMode: Block`), `Pod` (`runtimeClassName: kata-qemu`), and `Secret` resources against a real Kubernetes APIServer (or an `httptest.Server` K8s mock).
2. A background controller manager loop (`RunControllerLoop`) that periodically reconciles `PodWarmPool` replenishment, executes `ReapIdle` for expired sandboxes, and exposes `/healthz` and `/metrics` endpoints on `:8081`.

---

## Motivation

In `v0.1.0`, `pkg/controller` synthesized `BlockPVCManifest` and `PodManifest` structs in memory, and `cmd/cubepod-controller/main.go` blocked on `SIGTERM` without running a reconciliation ticker or health server. For `v0.2.0`, `cubepod-controller` must actively drive a `KubeClient` backend and run an observable background control loop.

### Goals

- Define `KubeClient` interface in `pkg/controller/kubeclient.go`:
  - `EnsurePVC(ctx context.Context, pvc BlockPVCManifest) error`
  - `DeletePVC(ctx context.Context, namespace, name string) error`
  - `EnsurePod(ctx context.Context, pod PodManifest) (podIP string, err error)`
  - `DeletePod(ctx context.Context, namespace, name string) error`
- Implement `RESTKubeClient` (talking standard Kubernetes JSON REST endpoints `/api/v1/namespaces/{ns}/persistentvolumeclaims` and `/api/v1/namespaces/{ns}/pods` with Bearer token / TLS support) and `MemoryKubeClient` (recording applied K8s objects for inspection).
- Wire `Reconciler` to invoke its configured `KubeClient` whenever `Reconcile`, `ReconcileWarmPool`, `Pause`, `ResetRootFS`, or `Delete` transitions state.
- Implement `ControllerManager` (`pkg/controller/manager.go`) with:
  - Periodic `ReapIdle` and `PodWarmPool` replenishment loop (`Start(ctx)`).
  - HTTP `/healthz` and `/metrics` (JSON/Prometheus counters for `cubepod_active_sandboxes`, `cubepod_paused_sandboxes`, `cubepod_warmpool_ready`) handler.
- Update `cmd/cubepod-controller/main.go` to start `ControllerManager` and serve `:8081` (`/healthz`, `/metrics`).

### Non-Goals

- Pulling in the multi-hundred-megabyte `k8s.io/client-go` dependency tree; standard Go `net/http` against the Kubernetes v1 REST API keeps compilation fast and dependency-free.

---

## Proposal

### Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
| :--- | :--- | :--- | :--- |
| Transient K8s APIServer 409 Conflict on PVC/Pod creation | Low | High | Treat `409 Conflict` (AlreadyExists) on `POST /pods` or `POST /persistentvolumeclaims` as idempotent success in `RESTKubeClient`. |

---

## Design Details

### K8s REST Serialization (`pkg/controller/kubeclient.go`)

`RESTKubeClient` serializes `BlockPVCManifest` into standard Kubernetes `v1.PersistentVolumeClaim` JSON (`apiVersion: v1`, `kind: PersistentVolumeClaim`, `spec.volumeMode: "Block"`, `spec.dataSource` when `DataSourcePVC != ""`) and `PodManifest` into standard Kubernetes `v1.Pod` JSON (`apiVersion: v1`, `kind: Pod`, `spec.runtimeClassName`, `spec.initContainers`, `spec.containers`, `spec.volumes`).

### Test Plan

- **Unit & Integration Tests (`pkg/controller/kubeclient_test.go`)**:
  - Stand up a mock Kubernetes APIServer using `httptest.NewServer` handling `/api/v1/namespaces/cubepod-workloads/persistentvolumeclaims` and `/api/v1/namespaces/cubepod-workloads/pods`.
  - Verify `Reconciler` configured with `RESTKubeClient` creates dual `volumeMode: Block` PVCs and the Kata Pod on `DesiredStateRunning`, deletes only the Pod (keeping both PVCs) on `DesiredStatePaused`, deletes old RootFS PVC and creates `gen2` RootFS PVC (keeping Workspace PVC) on `RootfsGeneration: 2`, and populates `dataSource` on `ForkFrom`.
  - Verify `ControllerManager` background loop runs `ReapIdle` and serves `/healthz` and `/metrics`.

### Graduation Criteria

- `go test -v -race ./pkg/controller/...` and `go build ./cmd/cubepod-controller` pass with zero warnings.

### Upgrade / Downgrade Strategy

- Default `Reconciler` uses `MemoryKubeClient` when no `KubeClient` is injected, preserving 100% backward compatibility with `v0.1.0` callers.

---

## Production Readiness Review (PRR)

- **Observability**: `/healthz` and `/metrics` endpoints exposed by `ControllerManager`.

---

## Alternatives Considered

- **`k8s.io/client-go`**: Rejected to keep the repository lightweight, fast to build, and free of transitive dependency bloat.
