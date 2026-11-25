# Staging WarmPool, Cold-Start Spike, and Kata CLH Profiling Baseline

- **Date**: `2026-11-25`
- **Cluster / Node**: `gpu-214` (`192.168.1.214`), Intel Xeon Gold 5218 @ 2.30GHz (64 vCPU), 123 GiB RAM, Ubuntu 26.04.1 LTS, Kernel `7.0.0-38-generic`, `containerd://2.2.2`, Kubernetes `v1.27.8`
- **Target Deployment**: `cubepod-staging` (`v0.8.0`), gateway `http://192.168.1.214:32381`, workload namespace `cubepod-staging-workloads`, `runtimeClassName: kata-clh`, `defaultEgressEnabled: true`, `transparentEgress: true`, controller `syncInterval: 5s`
- **Benchmark Script**: [`scripts/staging-warmpool-bench.py`](../../scripts/staging-warmpool-bench.py)

---

## 1. WarmPool Baseline: `longhorn-v2-block-thin` vs `cubepod-lvm-block`

### Test Configuration

- `longhorn-v2-block-thin`: `driver.longhorn.io` (SPDK v2 AIO on `/dev/cubepod/longhorn-v2`, 80 GiB thin-provisioned).
- `cubepod-lvm-block`: `lvm.csi.openebs.io` (thick LVM VG `cubepod-vg` on `/dev/loop0`, `<50.00g` total).
- Because `cubepod-vg` is 50 GiB thick-provisioned, multi-replica (`replicas=3`) WarmPool + burst replenishment runs used `rootfs=2048Mi`, `workspace=2Gi` (`4 GiB` per sandbox) on both StorageClasses for an apples-to-apples comparison.

### Results (`replicas=3`, `rootfs=2048Mi`, `workspace=2Gi`)

| Operation | `longhorn-v2-block-thin` (p50 / mean ms) | `cubepod-lvm-block` (p50 / mean ms) | Notes |
| :--- | ---: | ---: | :--- |
| `cold_create` (1/1) | `29,024.3` / `29,024.3` | `14,647.7` / `14,647.7` | K8s `create_to_ready`: 27.0s vs 12.0s (`scheduled_to_init`: 18.0s vs 5.0s) |
| `warmpool_prewarm` (`replicas=3`) | `102,545.4` / `102,545.4` | `31,257.3` / `31,257.3` | `POST /warmpools` provisions 3 standbys sequentially under `Reconciler.mu` |
| `warmpool_single_claim` (3/3) | `551.3` / `619.2` (min `365.8`, max `940.4`) | `341.2` / `363.9` (min `237.7`, max `512.8`) | 100% `WarmPoolClaimed`; cold-path ~52x / ~40x speedup |
| `warmpool_replenish_single` (1/1) | `36,236.1` / `36,236.1` | `10,557.9` / `10,557.9` | Controller `syncInterval=5s` + 1 standby pod provisioning |
| `warmpool_burst_claim` (3 concurrent, 3/3) | `565.0` / `574.7` (max `774.8`) | `649.8` / `653.5` (max `1,031.9`) | All 3 concurrent claims hit `WarmPoolClaimed` (0 cold fallbacks) |
| `warmpool_burst_wall` (3 concurrent) | `776.8` / `776.8` | `1,033.4` / `1,033.4` | Serialized in-process by `Reconciler.mu` + `SyncFromKube` on empty cache |
| `warmpool_replenish_burst` (3 slots) | `94,707.5` / `94,707.5` | `30,637.3` / `30,637.3` | Controller replenishes 3 standbys sequentially |

Post-fix `1`-replica smoke verification on `cubepod-lvm-block` (verifying `POST /sandboxes/{id}/commands/run` `exit_code` field):
- `warmpool_prewarm`: `8,325.7 ms`, `warmpool_single_claim`: `236.0 ms`, `warmpool_claim_exec_rtt`: `80.8 ms` (`1/1 OK`), `warmpool_replenish_single`: `18,968.0 ms`, `warmpool_burst_claim`: `637.9 ms`, `warmpool_replenish_burst`: `14,438.3 ms`.

### Key WarmPool Observations

1. **Pre-warm & Replenishment Bottleneck**: Longhorn v2 CSI attachment/mapping adds ~13s per pod compared to local LVM (`scheduled_to_init`: 18s vs 5s). Because `ReconcileWarmPool` provisions missing replicas sequentially while holding `Reconciler.mu`, `replicas=3` pre-warm and burst replenishment scale linearly (~102.5s / ~94.7s on Longhorn v2; ~31.3s / ~30.6s on LVM).
2. **Claim Latency vs `<200ms` Target**: Single claim p50 (`341–551 ms`) and burst wall (`777–1,033 ms`) exceed the 200ms E2E claim SLO because `Reconcile` holds `Reconciler.mu` across synchronous Kubernetes `ClaimPod` PATCH + `PodBox` POST/PUT status + event calls, and `CreateSandbox` runs a synchronous `SyncFromKube` (`pkg/gateway/sandboxes.go`) when the gateway's in-memory warm standby cache is empty.

---

## 2. Root Cause of `cubepod-lvm-block` Iteration 1 `15s` `main_start_to_ready` Spike

Correlating `kubelet` and `containerd` journal logs on `gpu-214` for the `09:04:23Z–09:04:48Z` Iteration 1 pod (`cpod-1795597462819519009`) proves the 15s spike was caused by **node-wide serialized image pulling (`serializeImagePulls: true`) blocking the `egress` sidecar container pull**:

1. `/var/lib/kubelet/config.yaml` on `gpu-214` sets **`serializeImagePulls: true`**.
2. `sandbox` started at `09:04:29.546Z` (`podd-init` + `podd` became healthy in `<1s`).
3. Concurrently at `09:04:29.822Z` (`firstStartedPulling`), kubelet queued `PullImage "hub.sensedeal.vip/library/cubepod-egress:v0.8.0"` because that repo tag was not yet present locally.
4. That pull sat in kubelet's serial pull queue for **13.2s** behind an unrelated 782 MB pull (`ai-teaching/admin-web`, finished `09:04:42.175Z`) and failing `ai-teaching` pulls (`09:04:42.257Z–09:04:42.923Z`).
5. `PullImage "hub.sensedeal.vip/library/cubepod-egress:v0.8.0"` finally started at `09:04:43.016Z` and completed in `480.2 ms` (`09:04:43.497Z`, reusing local `ghcr.io` blobs), `egress` started at `09:04:43.774Z–09:04:44Z`, and Pod `Ready` flipped `True` at `09:04:45Z–09:04:48Z` — producing the ~15s `main_start_to_ready` (`sandbox.startedAt` -> `Pod Ready`) spike.
6. In subsequent iterations (and in our 3-iteration `10Gi`/`20Gi` probe at `09:34Z` with `cubepod-egress:v0.8.0` cached), `main_start_to_ready` dropped immediately to `1.000s` (cold wall `10.24s`, `12.21s`, `11.59s`).

### Additional Staging Node Findings

- **Broken Harbor blob + Kubelet Image GC**: At `09:07:12Z`, kubelet `image_gc_manager` (`imageGCHighThresholdPercent: 85`) evicted `cubepod-egress:v0.8.0`. Re-pulling `hub.sensedeal.vip/library/cubepod-egress:v0.8.0` failed with HTTP 404 on layer `sha256:598fe9cdf448bd1f2cbc2ac3c76f739d4e09009f79d165889db69633e80c1d78`. (Restored into `gpu-214` `k8s.io` containerd store from local cache for benchmarking; Harbor image should be re-pushed.)
- **Missing Pull Secrets in Workload Namespace**: `hub.sensedeal.vip` and `regcred` exist in `cubepod-staging-system`, not `cubepod-staging-workloads`, generating per-pod kubelet warnings (`secret "hub.sensedeal.vip" not found`).

---

## 3. Kata `kata-clh` MicroVM Startup Profiling on `gpu-214`

### Critical Configuration Bug: `kata-clh` Silently Falls Back to QEMU

`/etc/containerd/conf.d/kata.toml` on `gpu-214` configures:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata-clh]
  runtime_type = 'io.containerd.kata.v2'
  privileged_without_host_devices = true
  privileged_without_host_devices_all_devices_allowed = true
```

Because `[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata-clh.options]` **`ConfigPath = "/etc/kata-containers/configuration-clh.toml"`** is omitted, `containerd-shim-kata-v2` (`runtime-rs`) logs:

```text
get config path ""
load configuration from: /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-qemu-runtime-rs.toml
```

and spawns `/opt/kata/bin/qemu-system-x86_64` (`q35`, `virtio-scsi`, `nvdimm` `kata-ubuntu-resolute.image` 256M) rather than `/opt/kata/bin/cloud-hypervisor`.

### Microsecond Cold-Create Phase Breakdown (`cpod-1795599260505539138`, `cubepod-lvm-block`, `10Gi`/`20Gi`)

| Phase | Timestamp (UTC) | Duration | Details |
| :--- | :--- | ---: | :--- |
| PVC + Scheduler | `09:34:21.0 -> 09:34:24.216` | ~3,200 ms | OpenEBS LVM provisioning + 1 `FailedScheduling` retry |
| CSI `MapPodDevice` | `09:34:24.216 -> 09:34:26.028` | 1,812 ms | 2 block PVCs mapped on `gpu-214` |
| `RunPodSandbox` -> Shim | `09:34:26.028 -> 09:34:26.347` | 319 ms | Calico IPAM (`10.1.39.186/26`) + veth + `kata` shim start |
| Shim -> VMM Spawn | `09:34:26.347 -> 09:34:26.405` | 58 ms | TOML load + netns tap + `virtiofsd` + `qemu-system-x86_64` spawn |
| **VMM + Guest Boot -> vsock** | `09:34:26.405 -> 09:34:28.136` | **1,731 ms** | Kernel `6.18.56-208` + `systemd` + `kata-agent` (`vsock` 8 attempts) |
| Pause Container | `09:34:28.136 -> 09:34:28.230` | 94 ms | `RunPodSandbox` returns `09:34:28.213` |
| `podd-installer` Init | `09:34:28.230 -> 09:34:28.678` | 448 ms | `CreateContainer` + `StartContainer` + binary copy |
| `sandbox` + `egress` Start | `09:34:28.678 -> 09:34:28.905` | 227 ms | Container creation via `virtiofsd` + task start |
| `podd-init` In-Guest | `09:34:28.810 -> 09:34:29.316` | 506 ms | `mkfs.ext4` 10Gi (~185ms) + 20Gi (~135ms) + OverlayFS + `pivot_root` + `podd :49983` |
| Readiness Probe + Poll | `09:34:29.316 -> ~09:34:30.2` | ~900 ms | `initialDelaySeconds: 1`, `periodSeconds: 1`, controller `pollInterval: 500ms` |

**Total `RunPodSandbox` -> `podd` listening**: `3.29s` (of which `1.73s` is QEMU + guest kernel/systemd/agent boot, and `1.56s` is CNI/pause/`podd-installer`/`podd-init`/containers).

### Direct `cloud-hypervisor` v54.0 Boot Profile on `gpu-214`

Running `/opt/kata/bin/cloud-hypervisor` directly on `gpu-214` with `vmlinux-6.18.56-208` and `kata-ubuntu-resolute.image` (`readonly=on,image_type=Raw`, 2 vCPU, 2048M):
- `0.004s`: `cloud-hypervisor` VMM started
- `0.065s -> 0.495s` (**430 ms**): Guest Linux `6.18.56-208` init + `EXT4-fs` root mount
- `0.495s -> 1.087s` (**592 ms**): `systemd` `259.5` init -> `kata-agent` `4.3.1` announce & vsock listen (`1.087s` total vs `1.731s` under QEMU — **~640 ms faster**).

### Tunable Parameters & Optimization Recommendations

1. **Fix `kata-clh` `ConfigPath` in `/etc/containerd/conf.d/kata.toml`**:
   Add `[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata-clh.options]` with `ConfigPath = '/etc/kata-containers/configuration-clh.toml'` (~640 ms immediate VMM boot savings).
2. **Kata / Cloud Hypervisor (`configuration-clh-runtime-rs.toml`)**:
   - `hypervisor.clh.factory.enable_template = true`: VM memory/device templating to skip the ~1.09s kernel+systemd boot on warm clones.
   - Replace full `systemd` rootfs (`kata-ubuntu-resolute.image`, 256M) with a minimal initrd or custom direct-to-`kata-agent` init (~590 ms savings on cold VMM boot).
   - Tune `virtio_fs_extra_args` (currently `--thread-pool-size=1`) and `virtio_fs_cache = "always"` for read-only container image layers; test `num_queues = 2` (matching `default_vcpus`) for `virtio-blk-pci`.
3. **Node / Kubelet (`gpu-214`)**:
   - Set `serializeImagePulls: false` (`maxParallelImagePulls: 5`) so unrelated pod pulls never stall sandbox sidecars.
   - Re-push `hub.sensedeal.vip/library/cubepod-egress:v0.8.0` (fix missing Harbor layer) and provision `hub.sensedeal.vip`/`regcred` secrets in `cubepod-staging-workloads`.
4. **Control Plane & `podd-init` (Track A / Track B)**:
   - Parallelize `ReconcileWarmPool` provisioning outside `Reconciler.mu` and move K8s I/O out of the claim lock to push `warmpool_single_claim` and `warmpool_burst_claim` below the 200 ms SLO.
   - Optimize `podd-init` (`mkfs.ext4` `-E nodiscard` / fast formatting) and `podd-installer` / `ReadinessProbe` (`initialDelaySeconds: 0` or direct `podd` health check).
