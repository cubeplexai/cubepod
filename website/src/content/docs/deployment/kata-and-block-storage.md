---
title: Kata Containers & Block Storage
description: Configure containerd and Kata RuntimeClasses alongside Longhorn V2 SPDK or OpenEBS LVM raw block storage for hardware-isolated CubePod sandboxes.
---

In production Kubernetes clusters, CubePod runs each sandbox inside a dedicated Kata Containers microVM backed by two CSI raw `Block` PVCs (`/dev/cubepod-rootfs` and `/dev/cubepod-workspace`). Hardware virtualization ensures kernel, mount-namespace, and device isolation for untrusted agent workloads, while thin raw block storage unlocks fast snapshot cloning (`POST /sandboxes/{id}/fork`) and persistent OverlayFS state across pause/resume cycles.

## Step 1: Configure `containerd` for Kata + Block Devices

Every worker node running sandboxes needs KVM support (`/dev/kvm` with `vmx` or `svm`), `containerd`, `containerd-shim-kata-v2`, and `kata-runtime`.

CubePod ships an automated configuration helper at `deploy/k8s/kata/configure-containerd-kata.sh`:

```bash
# Preview changes without modifying the node
sudo bash deploy/k8s/kata/configure-containerd-kata.sh --dry-run

# Apply configuration and restart containerd
sudo bash deploy/k8s/kata/configure-containerd-kata.sh --restart
```

Supported flags:

| Flag | Default / Purpose |
| :--- | :--- |
| `--dry-run` | Print planned edits without modifying config files |
| `--config-path <path>` | Path to containerd config (default `/etc/containerd/config.toml`) |
| `--kata-config-path <path>` | Override the Kata configuration file path referenced by the shim |
| `--allow-missing-kvm` | Continue validation even if `/dev/kvm` is absent (for non-nested test setups) |
| `--restart` | Restart `containerd` after writing updated configuration |

### Why `privileged_without_host_devices` Is Required

Inside each sandbox pod, `podd-init` runs as PID 1 with privileges to run `e2fsck`, `mkfs.ext4`, `mount`, OverlayFS setup, and `pivot_root` on `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`. `configure-containerd-kata.sh` sets two critical flags on the Kata runtime handler in `/etc/containerd/config.toml`:

- `privileged_without_host_devices = true`
- `privileged_without_host_devices_all_devices_allowed = true`

Without these settings, `containerd` attempts to bind-mount every host `/dev/*` node into the privileged container inside the Kata guest, causing device conflicts such as `/dev/full: EEXIST` during pod startup.

## Step 2: Apply Kata `RuntimeClass` Manifests

Label GPU/KVM-capable worker nodes (`kubectl label node <node> katacontainers.io/kata-runtime=true`) and apply `deploy/k8s/kata/runtimeclasses.yaml`:

```bash
kubectl apply -f deploy/k8s/kata/runtimeclasses.yaml
```

The manifest defines two `RuntimeClass` options selectable via Helm `runtime.runtimeClassName`:

| RuntimeClass | Handler | Fixed Pod Overhead (`cpu` / `memory`) | Node Selector |
| :--- | :--- | :--- | :--- |
| `kata-qemu` | `kata-qemu` | `250m` / `160Mi` | `katacontainers.io/kata-runtime: "true"` |
| `kata-clh` | `kata-clh` | `250m` / `128Mi` | `katacontainers.io/kata-runtime: "true"` |

## Step 3: Provision Raw `Block` CSI Storage

CubePod requires a CSI driver supporting `volumeMode: Block` and ideally fast snapshot/CoW cloning for `fork`. Pick either **Longhorn V2 (SPDK)** or **OpenEBS LVM-LocalPV**.

### Option A: Longhorn V2 (SPDK NVMe-oF + Fast Clones)

Longhorn V2 runs an SPDK user-space storage engine (`v2DataEngine`) capable of fast thin provisioning and copy-on-write cloning. Scripts under `deploy/k8s/longhorn-v2/` automate single-node or multi-node bring-up:

```bash
bash deploy/k8s/longhorn-v2/install.sh
```

What `prep-node.sh` and `install.sh` configure:

1. **Kernel modules & loopback block backing (`prep-node.sh`)**:
   - Loads `nvme-tcp`, `nvme-fabrics`, `uio`, `uio_pci_generic`, and `vfio-pci`.
   - Creates a sparse backing file at `$CUBEPOD_LONGHORN_IMG` (default `/storage/cubepod/longhorn-v2.img`, size `80G`), attaches it via `losetup --direct-io=on`, symlinks `/dev/cubepod/longhorn-v2`, installs `cubepod-longhorn-loop.service`, and reserves `2Mi` hugepages (`CUBEPOD_HUGEPAGES`, default `1024`).
2. **Cluster preparation & Helm installation (`install.sh`)**:
   - Labels the target node `cubepod.io/longhorn-v2=true` and executes the `cubepod-longhorn-v2-prep` job (`node-prep-job.yaml`).
   - Optionally restarts `kubelet` if `CUBEPOD_RESTART_KUBELET=1` is set so kubelet recognizes hugepages immediately.
   - Installs Helm chart `longhorn/longhorn` (`--version 1.8.2`) into namespace `longhorn-system` using `deploy/k8s/longhorn-v2/values.yaml` (`v1DataEngine: false`, `v2DataEngine: true`, `v2DataEngineCPUMask: "0x1"`, `v2DataEngineGuaranteedInstanceManagerCPU: 1000`).
   - Patches `nodes.longhorn.io/<node>` with a `diskType: block` disk tagged `cubepod-v2` and applies `deploy/k8s/longhorn-v2/storageclass.yaml`:

| Field | Value in `storageclass.yaml` |
| :--- | :--- |
| `metadata.name` | `longhorn-v2-block-thin` |
| `provisioner` | `driver.longhorn.io` |
| `parameters.numberOfReplicas` | `"1"` |
| `parameters.dataEngine` | `v2` |
| `parameters.diskSelector` | `cubepod-v2` |
| `allowVolumeExpansion` | `true` |
| `volumeBindingMode` | `Immediate` |

### Option B: OpenEBS LVM-LocalPV (Bare NVMe Volume Group)

If your nodes have a dedicated local NVMe device and you prefer local LVM thin/thick provisioning without SPDK:

```bash
# On each worker node with raw disk /dev/nvme1n1:
sudo pvcreate /dev/nvme1n1
sudo vgcreate cubepod-vg /dev/nvme1n1

# Install operator and CubePod StorageClass:
kubectl apply -f deploy/k8s/openebs-lvm-operator.yaml
kubectl apply -f deploy/k8s/cubepod-lvm-sc.yaml
```

`deploy/k8s/cubepod-lvm-sc.yaml` creates `StorageClass` `cubepod-lvm-block` (`provisioner: lvm.csi.openebs.io`, `storage: "lvm"`, `volgroup: "cubepod-vg"`, `volumeBindingMode: Immediate`, `allowVolumeExpansion: true`). Point `storage.storageClassName: "cubepod-lvm-block"` in your Helm values file when using OpenEBS LVM.

## Step 4: Live Cluster Verification & Performance Benchmarks

Before running Kubernetes verification suites, copy `deploy/k8s/env.example` to `deploy/k8s/env.local` and adjust `KUBECONFIG`, storage class, and runtime settings (`Makefile` targets source `deploy/k8s/env.local` automatically).

### Functional E2E & Block Suites

```bash
# Offline unit checks for K8s E2E helpers
go test -race -v ./test/k8s-e2e/...

# Live cluster block CSI & Kata pod lifecycle suite
make test-k8s-block   # alias: make test-k8s-longhorn-v2

# Full live Kubernetes E2E suite
make test-k8s-e2e
```

### Benchmark Suite & Target SLOs

Run `TestK8sPerformanceBenchmark` against your live cluster to profile cold boot, execution latency, archive sync, pause/auto-resume, RootFS reset, CoW fork, and WarmPool claim speeds:

```bash
CUBEPOD_K8S_BENCH=1 \
CUBEPOD_BENCH_ITERATIONS=3 \
CUBEPOD_BENCH_OUT=/tmp/cubepod-kata-benchmark.md \
  go test -v -timeout 20m ./test/k8s-e2e/ -run TestK8sPerformanceBenchmark
```

The benchmark suite tracks the following operations and CubePod SLO targets documented in `deploy/k8s/kata/README.md`:

| Benchmark Operation | Target SLO | What It Measures |
| :--- | :--- | :--- |
| `cold_create` | — | End-to-end cold sandbox pod boot (`PodBox` + PVC bind + Kata + `podd` ready) |
| `exec_foreground_rtt` | — | Foreground command round-trip latency over Gateway → `podd` |
| `archive_sync` | `< 100ms` | Compressed `tar.gz` workspace upload & streaming extraction via `podd` |
| `pause` / `pause_auto_resume` | `< 1500ms` | Pod scale-down followed by transparent wake-up via `POST /connect` or subdomain traffic |
| `reset_rootfs` | — | Incrementing `rootfsGeneration` and clearing OS state while keeping `/workspace` intact |
| `cow_fork` | `< 1500ms` | CSI block snapshot clone into a newly booted child sandbox |
| `warmpool_prewarm` / `warmpool_claim_e2e` | `< 200ms` E2E HTTP (`< 50ms` controller overhead) | Pre-warmed microVM claim latency through the Gateway |

## Next Steps

- Review the [Kubernetes & Helm Deployment](/deployment/kubernetes-helm/) guide to customize controller and gateway replicas.
- Read [Lifecycle, WarmPool & CoW Fork](/concepts/lifecycle-warmpool-fork/) to understand how `PodWarmPool` and `cow_fork` interact under the hood.
