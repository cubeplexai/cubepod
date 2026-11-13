# CubePod Kata MicroVM Node Setup & Performance Benchmarks

This directory provides Kubernetes `RuntimeClass` definitions and host `containerd` configuration helpers for running CubePod sandboxes inside hardware-isolated **Kata Containers MicroVMs** (`kata-qemu` and `kata-clh`) backed by `volumeMode: Block` PVCs.

---

## 1. Host & `containerd` Preparation

### Why `privileged_without_host_devices` is Required

CubePod mounts two `volumeMode: Block` PVCs (`/dev/cubepod-rootfs` and `/dev/cubepod-workspace`) into the sandbox container and executes `podd-init` inside the Kata Guest VM (`mount` / `e2fsck` / `mkfs.ext4` / `pivot_root`), which requires a privileged security context. Without `privileged_without_host_devices = true` in `containerd`'s Kata runtime stanza, `containerd` passes host device nodes into the guest and fails container creation with `/dev/full: EEXIST` (see `docs/architecture.md` §3.3).

### Run `configure-containerd-kata.sh`

1. Preview diagnostics and planned TOML edits safely with `--dry-run`:

```bash
./deploy/k8s/kata/configure-containerd-kata.sh --dry-run
```

2. Apply on each Kata worker node and restart `containerd`:

```bash
sudo ./deploy/k8s/kata/configure-containerd-kata.sh --restart
```

You can also target a non-default config file or Kata hypervisor TOML:

```bash
sudo ./deploy/k8s/kata/configure-containerd-kata.sh \
  --config-path /etc/containerd/config.toml \
  --kata-config-path /opt/kata/share/defaults/kata-containers/configuration-qemu.toml \
  --restart
```

The script verifies `/dev/kvm`, CPU virtualization flags (`vmx`/`svm`), and Kata/containerd binaries, and idempotently sets:

```toml
privileged_without_host_devices = true
privileged_without_host_devices_all_devices_allowed = true
```

---

## 2. Apply Kubernetes `RuntimeClass` Resources

Label worker nodes that have `/dev/kvm` and Kata installed, then apply `runtimeclasses.yaml`:

```bash
kubectl label node <worker-node> katacontainers.io/kata-runtime=true --overwrite
kubectl apply -f deploy/k8s/kata/runtimeclasses.yaml
kubectl get runtimeclasses
```

This creates:
- `kata-qemu` (`overhead.podFixed`: `160Mi` RAM, `250m` CPU)
- `kata-clh` (`overhead.podFixed`: `128Mi` RAM, `250m` CPU)

---

## 3. Storage Backend Preparation (Choose One)

### Option A: Longhorn V2 (SPDK NVMe-oF + CoW Clone)

Longhorn V2 provides thin-provisioned SPDK NVMe-oF `volumeMode: Block` PVCs with Copy-on-Write volume cloning (`fork`).

1. Ensure kernel modules (`nvme-tcp`, `nvme-fabrics`, `uio`, `uio_pci_generic`, `vfio-pci`) and `2Mi` hugepages (`>= 1024`) are configured on the storage node.
2. Run the Longhorn V2 installer (see `deploy/k8s/longhorn-v2/`):

```bash
CUBEPOD_LONGHORN_NODE=<worker-node> \
CUBEPOD_SANDBOX_IMAGE=<your-sandbox-image> \
CUBEPOD_RESTART_KUBELET=1 \
  ./deploy/k8s/longhorn-v2/install.sh
```

3. Verify `StorageClass` `longhorn-v2-block-thin` and test raw block publishing:

```bash
make test-k8s-block
```

### Option B: OpenEBS LVM-LocalPV (Single-Node / Bare-Metal Local NVMe)

For fast local NVMe benchmarking without SPDK hugepages:

1. Create the LVM volume group `cubepod-vg` on the target node's spare disk/loopback device:

```bash
sudo pvcreate /dev/nvme1n1
sudo vgcreate cubepod-vg /dev/nvme1n1
```

2. Install OpenEBS LVM-LocalPV and create `cubepod-lvm-block`:

```bash
kubectl apply -f deploy/k8s/openebs-lvm-operator.yaml
kubectl apply -f deploy/k8s/cubepod-lvm-sc.yaml
```

3. Point CubePod controller/gateway `-storage-class=cubepod-lvm-block` (or `longhorn-v2-block-thin`).

---

## 4. Running the CubePod K8s Performance Benchmark Suite

### Unit Tests (No Live Cluster Required)

```bash
go test -race -v ./test/k8s-e2e/...
```

### Live Cluster Benchmark (`TestK8sPerformanceBenchmark`)

`TestK8sPerformanceBenchmark` discovers the cluster from your active `kubeconfig` (or `deploy/k8s/env.local`) and benchmarks:
1. Cold Sandbox Create (`cold_create`)
2. Foreground Exec RTT (`exec_foreground_rtt`)
3. `archive-sync` tar.gz extraction (`archive_sync`, SLO `< 100ms`)
4. Pause & Transparent Auto-Resume (`pause`, `pause_auto_resume`, SLO `< 1500ms`)
5. `reset-rootfs` preserving `/workspace` (`reset_rootfs`)
6. CoW Volume Fork (`cow_fork`, SLO `< 1500ms`)
7. WarmPool Pre-Warm & Claim (`warmpool_prewarm`, `warmpool_claim_e2e`, SLO `< 200ms` E2E HTTP / `< 50ms` controller overhead)

Run against your live cluster and export a Markdown report:

```bash
CUBEPOD_K8S_BENCH=1 \
CUBEPOD_BENCH_ITERATIONS=3 \
CUBEPOD_BENCH_OUT=/tmp/cubepod-kata-benchmark.md \
  go test -v -timeout 20m ./test/k8s-e2e/ -run TestK8sPerformanceBenchmark
```

Optional environment variables:
- `CUBEPOD_GATEWAY_URL`: Override discovered gateway Service URL.
- `CUBEPOD_SANDBOX_IMAGE`: Override discovered `-sandbox-image`.
- `CUBEPOD_WORKLOADS_NAMESPACE`: Target workloads namespace (default `cubepod-workloads`).
- `CUBEPOD_BENCH_ITERATIONS`: Number of samples for repeatable operations (default `3`).
- `CUBEPOD_BENCH_OUT`: Path to write the Markdown benchmark report.
- `CUBEPOD_BENCH_JSON_OUT`: Optional path to write the raw JSON benchmark report.
