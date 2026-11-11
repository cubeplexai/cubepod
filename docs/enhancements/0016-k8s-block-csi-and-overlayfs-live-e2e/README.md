# CEP-0016: OpenEBS LVM-LocalPV Block CSI Provisioning and Live In-Guest OverlayFS Pivot Verification

- **CEP Number**: `0016`
- **Status**: `implementable`
- **Target Version**: `v0.3.1`
- **Components**: `pkg/controller/manifests.go`, `pkg/controller/kubetypes.go`, `pkg/controller/kubeclient.go`, `deploy/k8s/`, `test/k8s-e2e/`

---

## 1. Summary

Deploys a real **Block CSI Driver** (**OpenEBS LVM-LocalPV** backed by an LVM Volume Group `cubepod-vg` on the Kubernetes worker node) in the live Kubernetes test cluster (`https://192.168.1.207:6443`) and verifies CubePod's flagship **`volumeMode: Block` + `podd-init` `ext4` format/heal + Guest Kernel `OverlayFS` (`upperdir`/`workdir`) + `pivot_root(2)`** path end-to-end on real Kubernetes Pods.

---

## 2. Motivation

### Goals
1. Provision an LVM Volume Group (`cubepod-vg`) and deploy the **OpenEBS LVM-LocalPV** CSI driver (`StorageClass: cubepod-lvm-block`) on the Kubernetes cluster so Kubernetes can dynamically provision `volumeMode: Block` PVCs and attach raw block devices (`/dev/cubepod-rootfs` and `/dev/cubepod-workspace`) via `volumeDevices`.
2. Ensure `BuildKataPod` (`pkg/controller/manifests.go` & `pkg/controller/kubetypes.go`) supports `volumeMode: Block` on both Kata MicroVM clusters (`runtimeClassName: kata-qemu`) and standard container clusters (`runtimeClassName: ""`, injecting `securityContext.privileged: true` so PID 1 `podd-init` has `CAP_SYS_ADMIN` to format/mount `ext4` on `/dev/cubepod-rootfs` & `/dev/cubepod-workspace`, mount `overlay`, and execute `pivot_root(2)`).
3. Verify the full in-guest `podd-init` block lifecycle in `test/k8s-e2e/k8s_block_e2e_test.go`:
   - `rootfs-pvc` and `workspace-pvc` are dynamically provisioned with `volumeMode: Block` on `cubepod-lvm-block` and mapped into the Pod at `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`.
   - `/run/cubepod/init-state.json` inside the running sandbox reports `"pivoted": true`, `"rootfs_action": "formatted"` (on initial creation) and `"mounted"` or `"healed"` (after Pause + Auto-Resume!).
   - System packages/files written outside `/workspace` (e.g., `/etc/cubepod-rootfs-persist.conf` or `/usr/local/bin/custom-tool`) **persist across Pause & Auto-Resume** because they live on the persistent `ext4` `upperdir` block volume (`/dev/cubepod-rootfs`).
   - `ResetRootFS` (`rootfsGeneration: 1 -> 2`) rotates `rootfs-pvc` to a fresh block device (wiping `/etc/cubepod-rootfs-persist.conf`), while `/workspace` (`/dev/cubepod-workspace`) remains 100% intact.

### Non-Goals
- Replacing existing `nfs-storage` (`volumeMode: Filesystem`) support; both `Block` and `Filesystem` storage modes remain first-class options.

---

## 3. Proposal

### User Stories
- **As an AI Agent Platform Engineer**, I want `pip install` / `apt-get install` or system configuration changes inside the sandbox RootFS to survive across Sandbox `Pause` (scale-to-zero) and `Auto-Resume` via `volumeMode: Block` + `OverlayFS`, while still being able to reset the RootFS cleanly without losing `/workspace`.

### Risks and Mitigations
- **Risk**: In standard `runc`/`containerd` Pods (when `runtimeClassName` is empty rather than `kata-qemu`), mounting `ext4` block devices and `overlayfs` inside the container requires `CAP_SYS_ADMIN` and access to loop/LVM block device nodes.
- **Mitigation**: When `volumeMode == "Block"`, `ContainerManifest` sets `Privileged: true` (`securityContext.privileged: true`) on the `sandbox` container so `podd-init` can mount `/dev/cubepod-rootfs`, `/dev/cubepod-workspace`, and `overlay` cleanly before dropping privileges (`1000:1000`) for user commands in `podd`.

---

## 4. Design Details

### 4.1 Controller Manifest Changes (`pkg/controller/manifests.go`, `kubetypes.go`, `kubeclient.go`)
- Add `SecurityContext *SecurityContext` (`Privileged *bool`) to `ContainerManifest` and `coreV1Container`.
- In `BuildKataPod`:
  - When `volumeMode == "Block"`, attach `VolumeDevices` (`/dev/cubepod-rootfs` and `/dev/cubepod-workspace`) and set `Privileged: true` on the `sandbox` container so PID 1 `podd-init` can execute `mount(2)` (`ext4` + `overlay`) and `pivot_root(2)`.
  - Respect explicit `RuntimeClassName` configuration (do not force `"kata-qemu"` when the cluster operator explicitly configures `--runtime-class=""` on a non-Kata test cluster).

### 4.2 OpenEBS LVM-LocalPV Block CSI Setup (`deploy/k8s/`)
- Create a 50GiB loopback-backed LVM Volume Group (`cubepod-vg`) on the worker node (`gpu-214`).
- Deploy OpenEBS LVM-LocalPV (`lvm.csi.openebs.io`) and `StorageClass` `cubepod-lvm-block` (`volgroup: cubepod-vg`, `volumeBindingMode: Immediate`).

### 4.3 Dual-Runtime Behavior
- **Production Kubernetes (`volumeMode: Block` + `cubepod-lvm-block`)**: LVM LV block devices attached at `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`, formatted as `ext4` by `podd-init`, mounted via `OverlayFS` (`lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work`), and pivoted via `pivot_root(2)`.
- **Local Development (`Docker + Loopback ext4`)**: Continues using sparse loopback `ext4` files via `pkg/runtime`.

---

## 5. Test Plan
1. Unit tests in `pkg/controller` verifying `volumeMode: Block` Pod spec generation (`volumeDevices`, `securityContext.privileged`, `runtimeClassName`).
2. Live Kubernetes E2E test (`TestK8sBlockLiveE2E` in `test/k8s-e2e/k8s_e2e_test.go`) verifying:
   - `volumeMode: Block` PVCs bound on `cubepod-lvm-block`;
   - `/run/cubepod/init-state.json` shows `"pivoted": true` and `"rootfs_action": "formatted"`;
   - RootFS changes (`/etc/cubepod-block-test.txt`) AND `/workspace` changes survive `Pause` + `Auto-Resume` (with `"rootfs_action": "mounted"` on resume);
   - `ResetRootFS` wipes `/etc/cubepod-block-test.txt` while preserving `/workspace`.

---

## 6. Graduation Criteria & PRR
- All offline unit/E2E tests (`make test`, `make test-e2e`) and live Kubernetes Block CSI E2E tests (`make test-k8s-e2e`) pass 100% with zero leaked Pods, PVCs, or LVM LVs.

---

## 7. Alternatives Considered
- **Longhorn / Rook-Ceph**: Excessive memory/CPU overhead and multi-daemon complexity for a single-node test cluster compared to OpenEBS LVM-LocalPV.
