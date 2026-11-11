# CEP-0015: Real Kubernetes Cluster (`kubectl`) Deployment and Live End-to-End Verification Suite

- **CEP Number**: `0015`
- **Status**: `implementable`
- **Target Version**: `v0.3.0`
- **Components**: `test/k8s-e2e/k8s_e2e_test.go`, `deploy/k8s/cubepod-test-cluster.yaml`, `Makefile`

---

## 1. Summary

Validates CubePod end-to-end on a **real multi-node Kubernetes cluster** (`https://192.168.1.207:6443`) using `kubectl`, real container images pushed to `hub.sensedeal.vip/library/`, real `nfs-storage` PersistentVolumeClaims (`rootfs-pvc` + `workspace-pvc`), real `initContainer` binary injection (`cubepod-podd` injecting `/opt/cubepod/bin/{podd-init,podd}` into `hub.sensedeal.vip/library/cubeplex-sandbox:0.8.0-260917`), and live `cubepod-gateway` / `cubepod-controller` deployments running inside Kubernetes!

---

## 2. Motivation

### Goals
1. Build and push real multi-component container images (`hub.sensedeal.vip/library/cubepod-podd:v0.3.0`, `cubepod-controller:v0.3.0`, `cubepod-gateway:v0.3.0`, `cubepod-egress:v0.3.0`).
2. Deploy CubePod (`CRDs`, `RBAC`, `cubepod-controller`, `cubepod-gateway` NodePort/Service) into namespace `cubepod-system` (managing workloads in `cubepod-workloads`) on the `kubectl` test cluster.
3. Provide an automated Kubernetes E2E verification suite (`test/k8s-e2e/k8s_e2e_test.go` & `make test-k8s-e2e`) that tests the live Kubernetes deployment end-to-end:
   - Sandbox creation -> Kubernetes schedules Pod with `initContainers: [podd-installer]` injecting `/opt/cubepod/bin/{podd-init,podd}` into `cubeplex-sandbox:0.8.0-260917` (zero modifications to the base sandbox image!) and binds `rootfs-pvc` + `workspace-pvc` on `nfs-storage`.
   - Foreground & background command execution (`POST /sandboxes/{id}/commands/run`), cursor log streaming, and process kill inside the real Kubernetes Pod.
   - File write/read/list/delete and `tar.gz` `archive-sync` inside the real Kubernetes Pod `/workspace` PVC.
   - `Pause` (deletes the Kubernetes Pod while retaining the bound `nfs-storage` PVCs) followed by **Auto-Resume** (schedules a fresh Kubernetes Pod re-mounting the same PVCs and verifying `/workspace` files persisted across Pod deletion!).
   - `ResetRootFS` (rotates `rootfs-pvc` generation 1 -> 2 while verifying `/workspace` PVC files remain 100% intact on the Kubernetes cluster!).
   - Signed `/panel/{token}/` reverse proxy routing into an HTTP server started inside the Kubernetes Sandbox Pod.

### Non-Goals
- Modifying existing workloads in unrelated namespaces on the shared test cluster. All CubePod resources are strictly isolated to `cubepod-system` and `cubepod-workloads`.

---

## 3. Design Details

### 3.1 Cluster Deployment Manifest (`deploy/k8s/cubepod-test-cluster.yaml`)
- Defines `Namespace` (`cubepod-system`, `cubepod-workloads`), CRDs (`podboxes.cubepod.io`, `podtemplates.cubepod.io`, `podwarmpools.cubepod.io`), `ServiceAccount` + `ClusterRole` + `ClusterRoleBinding`, `Deployment/cubepod-controller`, `Deployment/cubepod-gateway`, and `Service/cubepod-gateway`.

### 3.2 Live Kubernetes E2E Test Suite (`test/k8s-e2e/k8s_e2e_test.go`)
- Guarded by `CUBEPOD_K8S_E2E=1` (or `make test-k8s-e2e`) so unit tests (`make test`) can still run offline in environments without `kubectl`.
- Connects to the live `cubepod-gateway` (either in-cluster via NodePort/port-forward or against the Kubernetes API server) and verifies the actual Pods and PVCs in `cubepod-workloads` via `kubectl get pods/pvc -n cubepod-workloads`.

---

## 4. Test Plan
- Execute `make test-k8s-e2e` against `https://192.168.1.207:6443` and verify every lifecycle step passes with real Kubernetes Pods and PVCs.

---

## 5. Graduation Criteria & PRR
- Clean teardown of test sandbox Pods/PVCs after test completion so no orphaned pods waste cluster resources.

---

## 6. Alternatives Considered
- **Kind/Minikube-only testing**: Testing directly on the real multi-node `kubectl` cluster (`192.168.1.207:6443`) validates real CNI (Calico), real StorageClass (`nfs-storage`), real image pulling (`hub.sensedeal.vip`), and real multi-node scheduling.
