# CEP-0017: Longhorn V2 SPDK Block CSI Deployment and Live volumeMode Block Verification

| Field | Value |
| :--- | :--- |
| Stage | beta |
| Status | implemented |
| Created | 2026-11-11 |
| Updated | 2026-11-18 |
| Owning Area | storage |

## Release Signoff Checklist

- [x] Enhancement issue / CEP filed and reviewed against the current test cluster (Kubernetes v1.27.8).
- [x] Design covers Production Kubernetes (`Kata + CSI Block`) and Local Development (`Docker + Loopback ext4`).
- [x] Test plan names the live invariant: a `volumeMode: Block` PVC on `longhorn-v2-block-thin` binds and the consumer sees an NVMe block device.
- [x] Graduation: `make test-k8s-longhorn-v2` passed on the live cluster on 2026-11-11. Kubelet on `gpu-214` advertised `hugepages-2Mi: 2Gi` after the pages were allocated, before the restart Pod ran, so kubelet was not restarted.
- [x] Security review of the privileged Longhorn DaemonSet is recorded before this StorageClass becomes the default for CubePod sandboxes *(recorded in [Security Review: Privileged Longhorn V2 DaemonSet](#security-review-privileged-longhorn-v2-daemonset) on 2026-11-18; Longhorn V2 remains optional/opt-in in `deploy/k8s/longhorn-v2/` with `persistence.defaultClass: false` so `nfs-storage` stays the cluster-wide default `StorageClass`, while CubePod sandbox controllers/gateways explicitly target `--storage-class=longhorn-v2-block-thin` on labeled storage nodes)*.

> [!NOTE]
> **Beta Promotion & Opt-In Security Posture (Updated 2026-11-18):**
> The formal security review for Longhorn V2 (`longhorn/longhorn` `v1.8.2`) and live in-guest Kata `virtio-blk` verification (`TestK8sKataVirtioBlkGuestVerification` under `runtimeClassName: kata-clh`) are complete. Longhorn V2 remains **opt-in at the cluster `StorageClass` level** (`persistence.defaultClass: false`, node-pinned via `cubepod.io/longhorn-v2=true`, and binary `--storage-class` flags defaulting to `nfs-storage`). Promotion to `stable` additionally requires multi-replica (`numberOfReplicas >= 2`) physical NVMe backing on an upstream-supported Kubernetes and Longhorn release.

## Summary

Adds a pinned **Longhorn v1.8.2 V2 Data Engine** install and a live verification that a `volumeMode: Block` PVC is provisioned by SPDK and published to a Pod as an NVMe block device. The StorageClass name is `longhorn-v2-block-thin`, the same name the Helm chart already defaults to.

The current live suite stays on OpenEBS LVM-LocalPV (`cubepod-lvm-block`, `--runtime-class=none`). This CEP does not switch the running controller or gateway to the new class. An operator flips `-storage-class=longhorn-v2-block-thin` only after `make test-k8s-longhorn-v2` passes.

## Motivation

CEP-0016 proved the in-guest `ext4` + OverlayFS + `pivot_root` path against a loop-backed LVM logical volume that kubelet bind-mounts into a privileged `runc` container. That is not the production block path. The architecture calls for a CSI that exports a raw block volume and, under Kata, hands that host block device to the guest as `virtio-blk`. Longhorn V2 does the host half of that path: the V2 instance-manager runs `spdk_tgt`, stores replicas on a block disk, and exposes the volume to the node through NVMe-oF (`nvme-tcp`). Kubelet then maps that NVMe namespace into the Pod with `volumeDevices`.

### Goals

1. Install Longhorn v1.8.2 on the existing test cluster, restricted to the single worker that can host SPDK (`gpu-214`, kernel 7.0, containerd 2.2.2).
2. Back the V2 engine with one AIO block disk: an 80Gi sparse file on `/storage/cubepod/longhorn-v2.img`, attached as a loop device and linked at `/dev/cubepod/longhorn-v2`.
3. Publish StorageClass `longhorn-v2-block-thin` (`dataEngine: v2`, `numberOfReplicas: "1"`, `diskSelector: cubepod-v2`) without replacing the cluster default class `nfs-storage`.
4. Verify with `test/k8s-e2e/longhorn_v2_e2e_test.go` that a `volumeMode: Block` PVC binds, the Longhorn Volume CR reports `spec.dataEngine: v2`, and a consumer Pod can read the device at `/dev/cubepod-longhorn` and see it under the NVMe subsystem.

### Non-Goals

- Replacing `cubepod-lvm-block` as the storage class used by the already-running controller and gateway.
- Installing Kata (`kata-qemu` / `kata-clh`) in this CEP. The Pod contract (`volumeDevices`) is the same one Kata hotplugs as `virtio-blk`; the runtime switch stays a separate change.
- Deploying Mayastor on this cluster.
- Moving the control-plane nodes (kernel 5.15, `docker://24.0.7`) onto the V2 engine.
- Enabling Longhorn V1 iSCSI, replication greater than 1, or a backup target.

## Proposal

Pin Longhorn to the newest release that still runs on Kubernetes 1.27 and that already ships the SPDK V2 engine. Schedule every Longhorn pod onto nodes labeled `cubepod.io/longhorn-v2=true`. Give the engine one loop-backed block disk tagged `cubepod-v2`. Create volumes with one replica so a single worker is a valid placement. Prove the CSI publish path with a short-lived privileged Pod that consumes the PVC as a raw block device.

```mermaid
flowchart LR
  img["/storage/cubepod/longhorn-v2.img"] --> loop["loop device /dev/cubepod/longhorn-v2"]
  loop --> spdk["spdk_tgt AIO bdev on gpu-214"]
  spdk --> nvme["nvme-tcp target"]
  nvme --> csi["Longhorn CSI NodePublish"]
  csi --> dev["Pod volumeDevice /dev/cubepod-longhorn"]
```

### User Stories

1. As a cluster operator, I want a documented install that puts Longhorn V2 only on `gpu-214`, so the control-plane nodes and the other tenants on that worker are not given a V2 instance-manager.
2. As a CubePod engineer, I want a live test that fails unless the bound volume is a Longhorn V2 NVMe block device, so a later flip of `-storage-class` is based on an observed publish path.

### Notes / Constraints / Caveats

- The test cluster API server is Kubernetes v1.27.8. Longhorn v1.13 requires Kubernetes >= 1.34. Longhorn v1.8.2 is the pin: it requires Kubernetes >= 1.25 and its V2 engine is present.
- V2 in v1.8.2 requires 1024 hugepages of 2Mi (2Gi) to be visible in the node's allocatable `hugepages-2Mi`. The kernel accepts the pages as soon as they are written to `/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages`. Kubelet on this cluster reads that value at start, so the storage node needs one kubelet restart before the V2 instance-manager can leave `Pending`.
- `spdk_tgt` polls. v1.8.2 reserves `v2DataEngineGuaranteedInstanceManagerCPU` (set to 1000m here) on the storage node for as long as the V2 engine is enabled.
- The AIO bdev driver, not the NVMe bdev driver, owns the loop disk. The file size is 80Gi, an exact multiple of 4096 bytes. SPDK AIO does not support TRIM on this disk.
- V2 `dataLocality: strict-local` is not supported. The class uses `best-effort` with `numberOfReplicas: "1"`.
- Control-plane nodes `k8s-test-207` and `k8s-test-208` have no taint and run `docker://24.0.7` on kernel 5.15. The Longhorn node selector keeps manager, CSI node plugin, and instance-manager off those nodes.

### Risks and Mitigations

| Risk | Mitigation |
| :--- | :--- |
| Kubelet restart on `gpu-214` restarts every Pod on that node, including workloads outside `cubepod-*`. | `install.sh` never restarts kubelet. The operator passes `CUBEPOD_RESTART_KUBELET=1` once. |
| A default Longhorn StorageClass would replace `nfs-storage` as the cluster default. | Helm `persistence.defaultClass` is `false`. The CubePod class is applied separately and has no default-class annotation. |
| V2 instance-manager scheduled onto a control-plane node or a node without the loop disk. | `global.nodeSelector` and `systemManagedComponentsNodeSelector` require `cubepod.io/longhorn-v2=true`. The class `diskSelector` is `cubepod-v2`. |
| Hugepage allocation pins 2Gi that other tenants on `gpu-214` can no longer use. | The node has 128Gi. The prep script allocates 1024 pages and does not raise the count further. |
| Loop device number changes across reboot and Longhorn keeps a stale `/dev/loopN` path. | The prep script maintains the stable symlink `/dev/cubepod/longhorn-v2` and a systemd unit that recreates it. |
| Leaving V1 enabled would start a second instance-manager and require `open-iscsi`. | `defaultSettings.v1DataEngine` is `false`. |

## Design Details

### API / CRD / Wire Changes

No CubePod CRD or OpenAPI change. The controller already takes `-storage-class`. After this CEP's test passes, the test manifest args change from `-storage-class=cubepod-lvm-block` to `-storage-class=longhorn-v2-block-thin`. Pod spec shape is unchanged: two `volumeMode: Block` PVCs, `volumeDevices` at `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`.

StorageClass applied by this CEP:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: longhorn-v2-block-thin
provisioner: driver.longhorn.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  numberOfReplicas: "1"
  staleReplicaTimeout: "2880"
  dataLocality: "best-effort"
  dataEngine: "v2"
  diskSelector: "cubepod-v2"
  fsType: ext4
```

`fsType` is used only when a consumer asks for a filesystem volume. CubePod PVCs set `volumeMode: Block`, so Longhorn publishes the NVMe namespace without making a filesystem.

### State Machine and Data Flow

1. `deploy/k8s/longhorn-v2/prep-node.sh` runs in the host mount namespace of `gpu-214`. It loads `nvme-tcp`, creates the sparse image, attaches a free loop device, points `/dev/cubepod/longhorn-v2` at it, writes 1024 into the 2Mi hugepage pool, and installs `cubepod-longhorn-loop.service`.
2. The operator restarts kubelet on that node. `kubectl describe node gpu-214` then shows `hugepages-2Mi: 2Gi`.
3. `install.sh` labels the node, installs chart `longhorn/longhorn` version `1.8.2` into namespace `longhorn-system`, applies the StorageClass, and merges a `diskType: block` disk named `v2-aio` onto `nodes.longhorn.io/gpu-214`.
4. A PVC with `storageClassName: longhorn-v2-block-thin` and `volumeMode: Block` causes the CSI provisioner to create a Longhorn Volume with `spec.dataEngine: v2` and one replica on `v2-aio`.
5. On attach, `spdk_tgt` exports the replica over `nvme-tcp`. The CSI node plugin connects that namespace and kubelet bind-mounts the resulting block device onto the Pod's `volumeDevices` path.
6. Under `runtimeClassName: kata-qemu` or `kata-clh` the same host block device is what the Kata shim hotplugs into the guest as `virtio-blk`. This CEP's test stops at the host NVMe device, because Kata is not installed on the cluster.

### Boundary Conditions and Failure Recovery

- Hugepages not allocatable: the V2 instance-manager stays `Pending` with a hugepage request it cannot meet. No volume is created. Recovery is to allocate the pages and restart kubelet, then delete the stuck instance-manager pod so Longhorn recreates it.
- Loop setup fails: the Longhorn disk stays unschedulable and the PVC stays `Pending`. The prep Job log prints the loop path and the hugepage count. Re-running the Job is safe; it reuses the existing image and loop association.
- Node reboot: `cubepod-longhorn-loop.service` reattaches the image and restores the symlink. Hugepages come back only if `vm.nr_hugepages=1024` still succeeds at boot. A full reboot of `gpu-214` also needs the kubelet to observe the pages again.
- PVC delete: reclaim policy is `Delete`, so the Longhorn volume and its replica are removed. The loop disk file is not deleted.
- Uninstall: `helm uninstall longhorn -n longhorn-system` leaves the loop image and the hugepage pool in place. Drop the pages by writing `0` to `nr_hugepages` only after every V2 volume is gone.

### Test Plan

Unit tests: none. The CubePod API is unchanged. `make test` skips the new test because it requires `CUBEPOD_K8S_LONGHORN_V2=1`.

Integration / E2E: `make test-k8s-longhorn-v2` runs `TestK8sLonghornV2Block` against the current kubeconfig.

The test creates a 1Gi `volumeMode: Block` PVC on `longhorn-v2-block-thin` in `cubepod-workloads`, waits until it is `Bound`, and checks:

- the PV provisioner is `driver.longhorn.io`;
- `volumes.longhorn.io` `spec.dataEngine` is `v2`;
- a privileged Pod with `volumeDevices.devicePath: /dev/cubepod-longhorn` reaches `Succeeded`;
- the Pod log reports a non-zero `blockdev --getsize64` and a successful 4KiB read;
- the published block device is an NVMe namespace, or a device-mapper node whose slave subsystem is `nvme`. Longhorn v1.8.2 puts a linear dm device on top of the `nvme-tcp` namespace before kubelet bind-mounts it.

The test deletes the Pod and the PVC before it returns.

In addition, `TestK8sKataVirtioBlkGuestVerification` (`CUBEPOD_K8S_E2E=1 go test -v -timeout 5m ./test/k8s-e2e/ -run TestK8sKataVirtioBlkGuestVerification`) creates one ephemeral CubePod sandbox via `cubepod-gateway` on `--storage-class=longhorn-v2-block-thin --volume-mode=Block --runtime-class=kata-clh`, verifies inside the live Kata MicroVM guest:

- isolated guest Linux kernel (`uname -r` `6.18.56` inside the Cloud Hypervisor VM vs. host worker kernel `7.0.0-38-generic` on `gpu-214`);
- `/run/cubepod/init-state.json` `pivoted: true` with `/` mounted as `overlay` (`upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work`) and `/workspace` mounted as `ext4`;
- `/sys/bus/virtio/drivers/{virtio_blk,virtio_scsi}` and `/sys/class/block/*` confirming both `/dev/cubepod-rootfs` and `/dev/cubepod-workspace` traverse Kata's `virtio` hypervisor boundary rather than exposing host `dm-*` or `nvme*n1` devices into the guest;
- `/workspace` ownership `1000:1000` (`cubeplex`) and unprivileged default command execution (`1000:1000`), followed by clean Pod and dual Block PVC teardown.

### Graduation Criteria

- Alpha (`v0.3.1`, *Completed 2026-11-11*): chart values, StorageClass, node prep, and `TestK8sBlockPublish` exist, and the test passes once on `gpu-214`.
- Beta (`v0.7.0`, *Completed 2026-11-18*): the live controller and gateway run with `--storage-class=longhorn-v2-block-thin --volume-mode=Block --runtime-class=kata-clh`, formal security review of the privileged Longhorn V2 DaemonSet is recorded below, and both `make test-k8s-e2e` (including Pause / Auto-Resume and `ResetRootFS`) and `TestK8sKataVirtioBlkGuestVerification` pass on that class.
- Stable (`v1.0.0`, *Pending multi-replica physical NVMe & supported K8s upgrade*): the same class is served by a multi-replica (`numberOfReplicas >= 2`) physical NVMe V2 disk (or OpenEBS Mayastor) with isolated `storageNetwork` on a cluster whose Kubernetes and Longhorn versions are inside the upstream support window.

### Upgrade / Downgrade Strategy

Installing Longhorn does not rewrite existing `cubepod-lvm-block` PVCs. Sandboxes already bound to LVM keep those volumes. Downgrade is `helm uninstall` plus deletion of StorageClass `longhorn-v2-block-thin`. Workspace and RootFS data that was written to LVM is untouched. Data written later to a Longhorn V2 volume lives only on that volume; uninstalling Longhorn while those PVCs still exist deletes it if the reclaim policy has already run, and leaves unattached Longhorn volumes if the PVCs were retained. Do not uninstall while a CubePod sandbox still references the class.

### Dual-Runtime Behavior

- Production Kubernetes (`Kata + CSI Block`): this StorageClass is the host block provider. Kata maps the published NVMe device into the guest over `virtio` (`virtio_blk` `vd*` or `virtio_scsi` `sd*`) at `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`. `podd-init` then formats `ext4`, mounts OverlayFS, and calls `pivot_root`.
- Local Development (`Docker + Loopback ext4`): unchanged. `pkg/runtime` still bind-mounts sparse files. It does not talk to Longhorn.

## Security Review: Privileged Longhorn V2 DaemonSet

**Review Date**: `2026-11-18`  
**Scope**: `deploy/k8s/longhorn-v2/{install.sh,prep-node.sh,node-prep-job.yaml,values.yaml,storageclass.yaml,volumesnapshotclass.yaml}` and live `longhorn-system` workloads on `gpu-214`.  
**Disposition**: **Approved for Beta (Opt-In StorageClass on Dedicated Labeled Nodes)**; cluster-wide default promotion (`persistence.defaultClass: true`) remains gated on the Stable prerequisites below.

### 1. Privileged Capabilities, Host Namespaces, and HostPath Surface

Auditing the live `longhorn-system` workloads (`longhorn/longhorn` `v1.8.2`) and `deploy/k8s/longhorn-v2/` manifests identifies the exact privileged footprint:

| Workload | Kind | Privileged / Capabilities | Host Namespaces & HostPath Mounts | Purpose & Containment |
| :--- | :--- | :--- | :--- | :--- |
| `cubepod-longhorn-v2-prep` (`kube-system`) | One-shot `Job` | `privileged: true`, `runAsUser: 0` | `hostPID: true`, `nsenter -t 1 -m -u -i -n` | Runs `prep-node.sh` once during operator install to load `nvme-tcp`, attach `/dev/cubepod/longhorn-v2`, allocate `1024 x 2Mi` hugepages, and install systemd units; auto-GC'd (`ttlSecondsAfterFinished: 86400`). |
| `longhorn-manager` | `DaemonSet` | `privileged: true` (`longhorn-manager` container; `pre-pull-share-manager-image` is unprivileged) | HostPaths: `/boot`, `/dev`, `/proc`, `/etc`, `/var/lib/longhorn`; TLS Secret `longhorn-grpc-tls` | Orchestrates node disks, instance-managers, and Longhorn CRDs under `longhorn-service-account`. |
| `longhorn-csi-plugin` | `DaemonSet` | `privileged: true` + `CAP_SYS_ADMIN` (`longhorn-csi-plugin`, `node-driver-registrar`; `liveness-probe` unprivileged) | HostPaths: `/var/lib/kubelet/plugins/kubernetes.io/csi`, `/var/lib/kubelet/plugins_registry`, `/var/lib/kubelet/plugins/driver.longhorn.io`, `/var/lib/kubelet/pods`, `/dev`, `/proc`, `/sys`, `/lib/modules` | Performs `nvme connect` (`nvme-tcp`), `dm-linear` device-mapper setup, and kubelet `NodeStageVolume` / `NodePublishVolume` raw block bind-mounts. |
| `instance-manager-*` | Managed `Pod` | `privileged: true` | HostPaths: `host:/`, `/var/lib/longhorn/engine-binaries`, `/var/lib/longhorn/unix-domain-socket`, `hugepage` (`hugepages-2Mi: 2Gi`), `longhorn-grpc-tls` | Runs user-space `spdk_tgt` owning the `v2-aio` block disk and exporting NVMe-oF TCP subsystems. |
| `engine-image-ei-*` | `DaemonSet` | `privileged: true` | HostPath: `/var/lib/longhorn/engine-binaries/longhornio-longhorn-engine-v1.8.2` | Stages Longhorn engine binaries onto the node filesystem. |
| `csi-attacher`, `csi-provisioner`, `csi-resizer`, `csi-snapshotter`, `longhorn-driver-deployer`, `longhorn-ui` | `Deployment` (`replicas: 1`) | Unprivileged (`privileged` unset, no extra capabilities) | Standard projected ServiceAccount token only | Kubernetes CSI sidecars communicating with `kube-apiserver` and `longhorn-manager` gRPC. |

**Key Finding**: Because `instance-manager`, `longhorn-manager`, and `longhorn-csi-plugin` hold `privileged: true` with `/dev`, `/proc`, `/sys`, and `/` host mounts, compromise of any pod in `longhorn-system` is equivalent to root compromise on the labeled storage node (`gpu-214`). Consequently, `longhorn-system` MUST be treated as a Tier-0 infrastructure namespace restricted to cluster administrators, with `longhorn-ui` kept strictly `ClusterIP`-internal (never exposed via unauthenticated Ingress or NodePort).

### 2. Kernel Attack Surface (`nvme-tcp`, `dm-linear`, Loop/AIO) vs. Kata Guest Isolation

1. **Host NVMe-oF & Device-Mapper Path**: On `NodePublishVolume`, `spdk_tgt` exports a volume bdev over local `nvme-tcp` (`nvme-fabrics`), which the host Linux kernel (`7.0.0-38-generic`) connects as `/dev/nvmeXnY` wrapped in a linear device-mapper node (`/dev/dm-Z`) and bind-mounted to kubelet's `volumeDevices` path.
2. **Hardware VM Decoupling for Untrusted Sandboxes**: Untrusted agent code in `cubepod-workloads` **never** shares the host kernel, never sees `/dev/nvme*` or `/dev/dm-*`, and never mounts `ext4` or `OverlayFS` in the host kernel. Instead, `containerd-shim-kata-v2` (`RuntimeClass: kata-clh`, Cloud Hypervisor with `privileged_without_host_devices = true`) passes the host block file descriptor across the KVM boundary as a `virtio` block device (`/dev/sda` and `/dev/sdb` on `virtio-scsi`, or `/dev/vda` and `/dev/vdb` on `virtio-blk`). All `e2fsck`, `mkfs.ext4`, `mount`, `OverlayFS`, and `pivot_root(2)` syscalls executed by `podd-init` occur exclusively inside the isolated Kata guest kernel (`6.18.56`), neutralizing crafted-filesystem host kernel exploits (`ext4`/`overlayfs` CVEs).
3. **Kernel Module Tightening Note**: `prep-node.sh` loads `nvme-tcp` and `nvme-fabrics` (required for V2 attach) alongside `uio`, `uio_pci_generic`, and `vfio-pci` (checked by Longhorn's SPDK preflight). On nodes using `v2-aio` loop/block files without raw PCIe NVMe passthrough, no PCI devices are bound to `vfio-pci`/`uio_pci_generic`; when transitioning to physical NVMe PCIe passthrough, operators must enable IOMMU (`iommu=pt` / Intel VT-d / AMD-Vi) and avoid `uio_pci_generic` (which lacks DMA remapping isolation) in favor of `vfio-pci`.

### 3. PodSecurity Admission (PSA) & Multi-Namespace Boundary Isolation

CubePod enforces strict separation across three Kubernetes namespaces:
- **`longhorn-system` (Storage Data Plane)**: Requires `pod-security.kubernetes.io/enforce: privileged`. Contains zero tenant code and zero external ingress routes; inter-daemon control traffic uses `longhorn-grpc-tls`.
- **`cubepod-system` (Control Plane)**: Runs unprivileged Go Deployments (`cubepod-controller`, `cubepod-gateway`) with scoped RBAC (`ClusterRole` granting CRUD on `pods`, `persistentvolumeclaims`, `volumesnapshots`, and `cubepod.io` CRDs, with zero access to `longhorn-system` Secrets).
- **`cubepod-workloads` (Untrusted Tenant Sandboxes)**: Runs hardware-isolated Kata MicroVMs (`runtimeClassName: kata-clh`). Even though the inner sandbox container sets `privileged: true` so single-threaded `podd-init` can mount `OverlayFS` inside the guest VM, `configure-containerd-kata.sh` enforces `privileged_without_host_devices = true` in `/etc/containerd/config.toml`, preventing `containerd` from exposing host `/dev/*` nodes to the VM, and `podd` drops execution privileges to `cubeplex` (`1000:1000`) for all user commands and file writes.

### 4. Node-Selector Blast Radius Containment (`cubepod.io/longhorn-v2=true`)

`deploy/k8s/longhorn-v2/values.yaml` sets `cubepod.io/longhorn-v2: "true"` across all four scheduling gates:
- `global.nodeSelector`
- `longhornManager.nodeSelector`
- `longhornDriver.nodeSelector`
- `defaultSettings.systemManagedComponentsNodeSelector` (controlling dynamically spawned `instance-manager` and `engine-image` pods)

On the verification cluster, untainted control-plane nodes (`k8s-test-207`, `k8s-test-208`, `k8s-test-217`, kernel `5.15`, `docker://24.0.7`) never receive privileged Longhorn pods or SPDK hugepage allocations. Furthermore, `persistence.defaultClass: false` and `defaultSettings.v1DataEngine: false` ensure `nfs-storage` remains the cluster default `StorageClass` and `open-iscsi` V1 daemons are never started.

### 5. SPDK Hugepage & Polling CPU Starvation Bounds

`spdk_tgt` uses poll-mode reactors that would otherwise spin at 100% `SCHED_OTHER` CPU and pin non-swappable RAM:
1. **Memory Bound**: `prep-node.sh` caps `2Mi` hugepages at `1024` (`2Gi` out of `128Gi` RAM on `gpu-214`), and `instance-manager` sets `resources.limits.hugepages-2Mi: 2Gi`.
2. **CPU 0 Housekeeping Protection**: `values.yaml` sets `v2DataEngineGuaranteedInstanceManagerCPU: 1000` (`1` vCPU) and `v2DataEngineCPUMask: "0x2"`, keeping SPDK off CPU 0 so kernel timer/RCU housekeeping on CPU 0 is unaffected.
3. **EEVDF `SCHED_IDLE` + `cpu.max` Guard**: `prep-node.sh` installs `/usr/local/sbin/cubepod-spdk-sched-idle` (`cubepod-spdk-sched-idle.timer`, every `5s`), setting `spdk_tgt` `reactor_*` threads to `SCHED_IDLE` (`policy=5`) and enforcing `cpu.max = 95000 100000` (`95%` per reactor) on the container cgroup so root-cgroup kernel `kworker` threads (such as `nvme` / `dmsetup` teardown) cannot livelock under Linux 7.0 EEVDF scheduling.

### 6. Storage Network Isolation (`nvme-tcp` Fabric Exposure)

- **Single-Node Beta Layout (`numberOfReplicas: "1"`)**: On `gpu-214`, `spdk_tgt` and the kernel `nvme-tcp` initiator communicate locally on the storage node, while tenant sandboxes in `cubepod-workloads` are isolated behind `cubepod-egress` (`TransparentEgress` `nftables` + mTLS sidecar). However, `spdk_tgt` and `instance-manager` TCP ports (`8000–9500` / dynamic NVMe-oF ports) bind within the pod network namespace (`10.1.39.179`) without NVMe-oF DH-HMAC-CHAP in Longhorn `v1.8.2`.
- **Mandatory Control Before Multi-Node Replication (`numberOfReplicas >= 2`)**: Before enabling cross-node replica fan-out, operators MUST configure Longhorn's `storageNetwork` setting onto a dedicated non-routable storage VLAN/CNI (`Multus`) OR apply a default-deny Kubernetes `NetworkPolicy` in `longhorn-system` permitting ingress to `instance-manager` and `longhorn-manager` pods exclusively from `longhorn-system` pods and host node IPs, blocking any Pod-to-Pod TCP connection attempts from workload namespaces.

### 7. Kubernetes & Longhorn Version Maintenance Caveats

- **Upstream Support Window**: The verification cluster runs Kubernetes `v1.27.8` with Longhorn `v1.8.2` (pinned because Longhorn `v1.10+` / `v1.13+` requires newer Kubernetes APIs). Both `v1.27.8` and Longhorn `v1.8.2` are outside active upstream CVE/bugfix maintenance, and V2 volumes in `v1.8.2` do not support live engine upgrades (all sandboxes using `longhorn-v2-block-thin` must be paused/detached prior to upgrading Longhorn).
- **Single-Replica Loop AIO vs. Physical NVMe**: The current `v2-aio` backing disk (`/storage/cubepod/longhorn-v2.img`, `numberOfReplicas: "1"`) validates functional SPDK NVMe-oF block provisioning and Kata `virtio` guest attachment, but provides single-disk durability and omits NVMe TRIM/deallocate. Promoting CEP-0017 from `beta` to `stable` requires: (a) upgrading Kubernetes (`>= v1.32`) and Longhorn V2 (or OpenEBS Mayastor) into an upstream-maintained release window, (b) migrating from sparse loop AIO to dedicated `vfio-pci` physical NVMe devices with `numberOfReplicas >= 2`, and (c) enforcing dedicated `storageNetwork` / `NetworkPolicy` isolation.

## Production Readiness Review Questionnaire (PRR)

### Feature Enablement and Rollback

Enable by installing the chart and applying the StorageClass. CubePod ignores the class until `-storage-class` changes. Rollback of the verification path is deleting the test PVC. Rollback of the driver is `helm uninstall` after every Longhorn volume is deleted. `/workspace` data on `cubepod-lvm-block` is not on this driver.

### Rollout, Upgrade and Rollback Planning

Longhorn v1.8.2 cannot live-upgrade a V2 volume. A later chart upgrade requires every V2 volume to be detached first, which means every CubePod sandbox using the class must be paused or deleted. Paused sandboxes keep the volume detached, so they survive the upgrade as long as the volume CR is not deleted. The LVM class is unaffected.

### Monitoring Requirements

- `kubectl get pods -n longhorn-system` shows the V2 instance-manager `Running` on `gpu-214`.
- `kubectl get nodes.longhorn.io -n longhorn-system -o yaml` shows disk `v2-aio` with `allowScheduling: true` and a positive `storageAvailable`.
- The V2 instance-manager log contains `spdk_tgt`. A failed disk add logs an AIO open error against `/dev/cubepod/longhorn-v2`.
- The e2e Pod log line `OK size=... subsys=...nvme...` is the attach success signal.

### Dependencies

- Longhorn chart 1.8.2 and images under `longhornio/*:v1.8.2`.
- Kernel modules on the storage node: `nvme-tcp`, `nvme-fabrics`. `vfio-pci` and `uio_pci_generic` are loaded for the SPDK prerequisite check; the AIO loop disk does not bind a PCI device.
- 2Gi of 2Mi hugepages allocatable on the storage node.
- Host binaries used by the prep script: `truncate`, `losetup`, `modprobe`, `systemctl`.
- Kata is not a dependency of this CEP's test. It is a dependency of the production guest path.

### Scalability

One V2 instance-manager on one node reserves about one CPU core whether or not a volume is attached. Replica count is 1, so a volume does not fan out. CSI controller replicas are set to 1 so they fit on the single labeled node. This layout is a verification layout, not a multi-node production layout.

### Troubleshooting

- PVC `Pending` and the Longhorn volume event says the disk selector matched nothing: the node disk tag `cubepod-v2` is missing or the symlink is gone. Re-run the prep Job and re-apply the disk patch.
- Instance-manager `Pending` with `Insufficient hugepages-2Mi`: the pages are not in kubelet allocatable. Confirm `/proc/meminfo` shows `HugePages_Total: 1024`, then restart kubelet on `gpu-214`.
- Pod attach timeout: `nvme-tcp` is not loaded, or the instance-manager is not running. `dmesg` on the node shows the `nvme connect` result.

## Implementation History

- 2026-11-11: CEP opened. Longhorn v1.8.2 selected over Mayastor and over Longhorn v1.13 because of the cluster's Kubernetes version. Manifests, prep script, and `TestK8sLonghornV2Block` added.
- 2026-11-11: Installed on `gpu-214`. Hugepages became allocatable without a kubelet restart. `TestK8sLonghornV2Block` passed: 1Gi `volumeMode: Block` PVC bound, published device `dm-0` with slave `nvme0n1` (`/sys/class/nvme-subsystem`).
- 2026-11-11: Controller and gateway on the test cluster now use `-storage-class=longhorn-v2-block-thin` and `-runtime-class=none`. `TestK8sLiveE2E` passed in 99s: `podd-init` formatted both block devices and pivoted, Pause retained the PVCs, Auto-Resume mounted them, `ResetRootFS` kept `/workspace` and dropped the RootFS file, and teardown deleted the Pod and PVCs.
- 2026-11-18: Recorded formal security review of the privileged Longhorn V2 DaemonSet (`## Security Review: Privileged Longhorn V2 DaemonSet`), added `deploy/k8s/longhorn-v2/volumesnapshotclass.yaml` (`longhorn-v2-snapclass`), and ran live non-disruptive `TestK8sKataVirtioBlkGuestVerification` on `gpu-214` (`--storage-class=longhorn-v2-block-thin --volume-mode=Block --runtime-class=kata-clh`). Promoted CEP-0017 to `beta` (keeping `stable` gated on multi-replica physical NVMe on an upstream-supported Kubernetes release). Observed live output (`45.91s`):
  ```text
  === RUN   TestK8sKataVirtioBlkGuestVerification
      longhorn_v2_e2e_test.go:463: [K8s-E2E] cluster config gateway=http://192.168.1.214:32380 name=cubepod-gateway namespace=cubepod-workloads image=hub.sensedeal.vip/library/cubeplex-sandbox:0.8.0-260917 storageClass=longhorn-v2-block-thin
      longhorn_v2_e2e_test.go:523: [Kata-VirtioBlk] Created ephemeral sandbox: id=cpod-1794978438059621934 state=Running
      longhorn_v2_e2e_test.go:613: [Kata-VirtioBlk] Verified isolated VM kernel: runtimeClass=kata-clh node=gpu-214 hostKernel=7.0.0-38-generic guestKernel=6.18.56 hypervisor=cpuinfo:hypervisor
      longhorn_v2_e2e_test.go:615: [Kata-VirtioBlk] Verified virtio-blk & mounts: virtio_blk=[virtio_scsi:virtio1] rootfs=sda(8:0,driver=sd,syspath=/sys/devices/pci0000:00/0000:00:03.0/virtio1/host0/target0:0:0/0:0:0:0/block/sda) workspace=sdb(8:16,driver=sd,syspath=/sys/devices/pci0000:00/0000:00:03.0/virtio1/host0/target0:0:0/0:0:0:1/block/sdb) rootMount=overlay|/|overlay|rw,relatime,lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work,uuid=on wsMount=/dev/cubepod-workspace|/workspace|ext4|rw,noatime uid_gid=1000:1000 initDurationMs=10735
      longhorn_v2_e2e_test.go:632: [Kata-VirtioBlk] Ephemeral sandbox cpod-1794978438059621934 and dual Block PVCs cleaned up
  --- PASS: TestK8sKataVirtioBlkGuestVerification (45.91s)
  ```

## Drawbacks

Longhorn v1.8.2 is outside upstream maintenance. The V2 engine in that release still requires hugepages and a polling `spdk_tgt`, so one shared worker permanently gives up 2Gi of memory and about one CPU core. A loop file on `/storage` is an AIO disk, not an NVMe SSD, so the test proves the NVMe-oF publish path and does not prove SPDK-on-NVMe performance. The install pulls `longhornio` images onto a cluster whose other images come from `hub.sensedeal.vip`.

## Alternatives

### OpenEBS Mayastor (Replicated PV Mayastor) 2.12

Rejected for this cluster. Mayastor 2.12.1 is tested on Kubernetes 1.32 and 1.35 and on kernel 6.8. This API server is 1.27.8. The io-engine still needs 2Gi of hugepages plus two exclusive CPU cores, and the project recommends three storage nodes. The only node here with a new enough kernel and containerd is `gpu-214`.

### Longhorn v1.13 V2 with hugepages disabled

Rejected. Current Longhorn requires Kubernetes >= 1.34. The ability to set `data-engine-hugepage-enabled` to false arrived in the 1.10 line, which also does not support 1.27. Staying on 1.8.2 keeps the install on a chart that can register its CRDs with this API server.

### Keep OpenEBS LVM-LocalPV as the only block verification

Rejected as the only path. LVM publishes a local device node into the `runc` container. It does not exercise SPDK, NVMe-oF, or the host block device that Kata later maps with `virtio-blk`. LVM stays in place until the Longhorn test is green and the controller args are switched.

### Give SPDK a physical NVMe on `gpu-214`

Not chosen for the first install. The node has other tenants and a large `/storage` filesystem. A dedicated NVMe, or an NVMe namespace, is the right disk once the publish path is proven. The loop disk is enough to show AIO replica placement and NVMe-oF attach.
