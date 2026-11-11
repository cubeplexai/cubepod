# CEP-0011: Dual-StorageMode Kubernetes Provisioning (`Block` + `Filesystem`), Pod Readiness Polling, and Live Gateway-to-Pod Routing

- **CEP Number**: `0011`
- **Status**: `implementable`
- **Target Version**: `v0.3.0`
- **Components**: `pkg/controller/{manifests.go,kubeclient.go,reconciler.go}`, `pkg/gateway/server.go`, `cmd/cubepod-gateway/main.go`, `crates/podd-init/src/main.rs`

---

## 1. Summary

To run CubePod across both bare-metal Kata + SPDK Raw Block clusters (`volumeMode: Block`) and standard Kubernetes test/staging clusters backed by Filesystem CSI drivers such as `nfs-storage` (`volumeMode: Filesystem`, `runc`/`containerd`), this CEP introduces:
1. **Configurable `VolumeMode` (`Block` vs. `Filesystem`) and `RuntimeClassName`** in `pkg/controller/manifests.go` and `Reconciler`.
2. **In-Guest `podd-init` Loopback/Filesystem Adaptation**: When running on `volumeMode: Filesystem` where `/workspace` is already mounted by kubelet and `CAP_SYS_ADMIN` pivot is skipped or unprivileged (`CUBEPOD_SKIP_PIVOT=1` or non-block mode), `podd-init` cleanly prepares `/workspace` and `/rootfs-upper` and `execvp`s `/opt/cubepod/bin/podd`.
3. **Real Kubernetes API Authentication & Pod Readiness Polling (`RESTKubeClient`)**: Supports kubeconfig/in-cluster ServiceAccount tokens, optional TLS `InsecureSkipVerify`, and polling `GET /api/v1/namespaces/{ns}/pods/{name}` until `status.phase == "Running"` and `status.podIP != ""` (with fallback/timeout).
4. **Live Gateway-to-Pod Routing**: When `reconciler.Reconcile` returns a non-empty `status.PodIP` (excluding the mock `10.244.0.42` unless live routing is enabled), `cubepod-gateway` automatically polls `GET http://<podIP>:49983/health` and registers `rec.PoddURL = "http://" + status.PodIP + ":49983"`.

---

## 2. Motivation

### Goals
- Allow CubePod to deploy and run real sandbox Pods on the `192.168.1.207:6443` Kubernetes cluster (`StorageClass: nfs-storage`, `volumeMode: Filesystem`, containerd runtime) without sacrificing `volumeMode: Block` + `kata-qemu` support.
- Automatically wire `cubepod-gateway` to the scheduled Kubernetes Pod IP (`<podIP>:49983`) on sandbox creation and Auto-Resume (`connect` / data-plane wake-up).
- Support `DeleteBox` on `Reconciler` so `DELETE /sandboxes/{id}` deletes the underlying Kubernetes Pod (and optionally PVCs when `deleteVolumes=true`).

### Non-Goals
- Modifying the external E2B wire format.

---

## 3. Proposal & Design Details

### 3.1 `pkg/controller/manifests.go` & `pkg/controller/reconciler.go`
- Add `VolumeMode` (`"Block"` or `"Filesystem"`, default `"Block"`) to `BlockPVCManifest` and `Reconciler` options (`WithVolumeMode(mode string)`, `WithDefaultStorageClass(sc string)`, `WithDefaultRuntimeClass(rc string)`, `WithPoddInjectorImage(img string)`, `WithDefaultSandboxImage(img string)`).
- When `VolumeMode == "Filesystem"`:
  - `BuildBlockPVCs` sets `VolumeMode: "Filesystem"` on both `rootfs-pvc` and `workspace-pvc` (and omits CSI `DataSource` if `ForkFrom` is used on a Filesystem StorageClass that lacks VolumeSnapshot/clone support, instead allowing Gateway/Controller data copy).
  - `BuildKataPod` mounts `rootfs-pvc` at `/mnt/cubepod/rootfs` and `workspace-pvc` at `/workspace` via `VolumeMounts` instead of `VolumeDevices`, and sets `CUBEPOD_SKIP_PIVOT=1` in the container env if `RuntimeClassName == ""` (standard unprivileged container runtime).
- Add `DeleteBox(ctx context.Context, name string, deleteVolumes bool) error` to `Reconciler` so terminating a sandbox deletes its Pod (and PVCs if `deleteVolumes == true`) via `KubeClient`.

### 3.2 `pkg/controller/kubeclient.go`
- Add `NewRESTKubeClientFromEnv()` / `RESTKubeClientOptions` supporting:
  - `APIServerURL`, `Namespace`, `BearerToken`, `InsecureTLS` (`tls.Config{InsecureSkipVerify: true}`).
  - Automatic loading from `KUBECONFIG` (`~/.kube/config` client-certificate-data / client-key-data or token) or in-cluster `/var/run/secrets/kubernetes.io/serviceaccount/token`!
- In `RESTKubeClient.EnsurePod(ctx, pod)`:
  - When `WaitForRunning` is enabled on `RESTKubeClient`, after creating/getting the Pod, poll `GET /api/v1/namespaces/{ns}/pods/{name}` every 500ms (up to context deadline / 60s) until `status.phase == "Running"` and `status.podIP != ""`.

### 3.3 `pkg/gateway/server.go` & `cmd/cubepod-gateway/main.go`
- Add `SetLivePodRouting(enabled bool)` on `gateway.Server`. When `livePodRouting` is true and `status.PodIP != ""`, `ensureRunning` and `handleCreateSandbox` set `rec.PoddURL = fmt.Sprintf("http://%s:%d", status.PodIP, rec.EnvdPort)` and wait up to 30s for `GET <PoddURL>/health` to return `200 OK`.
- In `handlePanelProxy` / `handleCreateEndpoint`, when `rec.PoddURL != ""` and no explicit `targetUrl` override was passed, default the panel upstream to `fmt.Sprintf("http://%s:%d", status.PodIP, port)` so `/panel/{token}/` reverse-proxies directly into the Kubernetes Pod's `<port>`!
- Update `cmd/cubepod-gateway/main.go` to accept controller flags (`--kube-api`, `--kubeconfig`, `--namespace`, `--storage-class`, `--volume-mode`, `--runtime-class`, `--podd-injector-image`, `--default-sandbox-image`, `--live-pod-routing`).

---

## 4. Test Plan
- Unit tests in `pkg/controller/kubeclient_test.go` verifying `VolumeMode: "Filesystem"` Pod/PVC generation, `DeleteBox`, kubeconfig client-cert/token parsing, and `EnsurePod` readiness polling.
- Unit tests in `pkg/gateway/server_test.go` verifying automatic `PoddURL` binding and panel upstream resolution from `PodIP`.

---

## 5. Graduation Criteria & PRR
- Zero breaking changes to existing `Block` mode tests.
- Clean fallback when running in-memory without a Kubernetes cluster.

---

## 6. Alternatives Considered
- **Require `volumeMode: Block` on all clusters**: Rejected because many developer and staging Kubernetes clusters use NFS (`nfs-storage`) or `local-path` provisioners that only support `volumeMode: Filesystem`.
