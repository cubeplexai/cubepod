---
title: Kata Containers 与块存储配置
description: 配置支持硬件虚拟化的 containerd、Kata RuntimeClass，以及 Longhorn V2 SPDK 或 OpenEBS LVM 裸块 CSI 存储驱动。
---

在 Kubernetes 生产集群中，CubePod 将每个沙箱运行在独立的 Kata Containers MicroVM 中，并挂载两块 CSI 裸块（`volumeMode: Block`）PVC（`/dev/cubepod-rootfs` 与 `/dev/cubepod-workspace`）。硬件级虚拟化为不受信任的 Agent 负载提供了内核、挂载命名空间和设备边界隔离，而精简置备裸块存储则支持快速快照克隆（`POST /sandboxes/{id}/fork`）以及跨暂停/恢复周期持久保留 OverlayFS 状态。

## 第一步：配置支持 Kata 与块设备的 `containerd`

运行沙箱的每个工作节点都必须具备 KVM 支持（`/dev/kvm` 且 CPU 开启 `vmx` 或 `svm`），并安装 `containerd`、`containerd-shim-kata-v2` 与 `kata-runtime`。

CubePod 在 `deploy/k8s/kata/configure-containerd-kata.sh` 提供了自动化节点配置脚本：

```bash
# 先预览计划修改，不改动节点文件
sudo bash deploy/k8s/kata/configure-containerd-kata.sh --dry-run

# 应用配置并重启 containerd
sudo bash deploy/k8s/kata/configure-containerd-kata.sh --restart
```

支持的脚本参数：

| 参数 | 默认值 / 用途 |
| :--- | :--- |
| `--dry-run` | 仅打印即将进行的改动，不实际写入配置文件 |
| `--config-path <path>` | `containerd` 配置文件路径（默认 `/etc/containerd/config.toml`） |
| `--kata-config-path <path>` | 覆盖 shim 引用的 Kata 配置文件路径 |
| `--allow-missing-kvm` | 当节点缺少 `/dev/kvm` 时允许继续校验（适用于非嵌套测试环境） |
| `--restart` | 写入更新后的配置后自动重启 `containerd` |

### 为什么必须开启 `privileged_without_host_devices`

在每个沙箱 Pod 中，PID 1 进程 `podd-init` 需要权限对 `/dev/cubepod-rootfs` 与 `/dev/cubepod-workspace` 执行 `e2fsck`、`mkfs.ext4`、`mount`、OverlayFS 组合及 `pivot_root`。`configure-containerd-kata.sh` 会在 `/etc/containerd/config.toml` 的 Kata 运行时段写入两项关键配置：

- `privileged_without_host_devices = true`
- `privileged_without_host_devices_all_devices_allowed = true`

若未开启这两项配置，`containerd` 会尝试把宿主机的全部 `/dev/*` 设备节点透传进特权 Kata 容器，从而在 Pod 启动阶段因设备节点冲突触发 `/dev/full: EEXIST` 等错误而启动失败。

## 第二步：应用 Kata `RuntimeClass` 清单

为支持 KVM 的工作节点打标（`kubectl label node <node> katacontainers.io/kata-runtime=true`），然后应用 `deploy/k8s/kata/runtimeclasses.yaml`：

```bash
kubectl apply -f deploy/k8s/kata/runtimeclasses.yaml
```

该清单定义了两个可通过 Helm `runtime.runtimeClassName` 选用的 `RuntimeClass`：

| RuntimeClass | Handler | 固定 Pod 开销（`cpu` / `memory`） | 节点选择器 |
| :--- | :--- | :--- | :--- |
| `kata-qemu` | `kata-qemu` | `250m` / `160Mi` | `katacontainers.io/kata-runtime: "true"` |
| `kata-clh` | `kata-clh` | `250m` / `128Mi` | `katacontainers.io/kata-runtime: "true"` |

## 第三步：部署裸块（Raw `Block`）CSI 存储

CubePod 需要支持 `volumeMode: Block` 的 CSI 驱动，并建议优先选用支持快速 CoW 快照克隆（支撑 `fork`）的方案。以下可在 **Longhorn V2 (SPDK)** 与 **OpenEBS LVM-LocalPV** 之间二选一。

### 方案 A：Longhorn V2（SPDK NVMe-oF + 快速 CoW 克隆）

Longhorn V2 采用基于 SPDK 的用户态存储引擎（`v2DataEngine`），支持精简置备与快速写时复制克隆。`deploy/k8s/longhorn-v2/` 目录下提供了完整的节点准备与一键安装脚本：

```bash
bash deploy/k8s/longhorn-v2/install.sh
```

`prep-node.sh` 与 `install.sh` 的执行流程如下：

1. **内核模块与回环块设备准备（`prep-node.sh`）**：
   - 加载内核模块 `nvme-tcp`、`nvme-fabrics`、`uio`、`uio_pci_generic` 与 `vfio-pci`。
   - 在 `$CUBEPOD_LONGHORN_IMG`（默认 `/storage/cubepod/longhorn-v2.img`，大小 `80G`）创建稀疏镜像文件，通过 `losetup --direct-io=on` 绑定至 `/dev/cubepod/longhorn-v2` 并注册 systemd 服务 `cubepod-longhorn-loop.service`，同时预留 `2Mi` 大页内存（`CUBEPOD_HUGEPAGES`，默认 `1024`）。
2. **集群初始化与 Helm 安装（`install.sh`）**：
   - 为目标节点打上 `cubepod.io/longhorn-v2=true` 标签，并运行 `cubepod-longhorn-v2-prep` Job（`node-prep-job.yaml`）。
   - 若设置 `CUBEPOD_RESTART_KUBELET=1`，自动重启 `kubelet` 使其立即感知新分配的大页内存资源。
   - 使用 `deploy/k8s/longhorn-v2/values.yaml`（`v1DataEngine: false`、`v2DataEngine: true`、`v2DataEngineCPUMask: "0x1"`、`v2DataEngineGuaranteedInstanceManagerCPU: 1000`）向 `longhorn-system` 命名空间安装 `longhorn/longhorn`（`--version 1.8.2`）。
   - 为 `nodes.longhorn.io/<node>` 添附带 `cubepod-v2` 标签的 `diskType: block` 块磁盘，并应用 `deploy/k8s/longhorn-v2/storageclass.yaml`：

| 字段 | `storageclass.yaml` 中的配置值 |
| :--- | :--- |
| `metadata.name` | `longhorn-v2-block-thin` |
| `provisioner` | `driver.longhorn.io` |
| `parameters.numberOfReplicas` | `"1"` |
| `parameters.dataEngine` | `v2` |
| `parameters.diskSelector` | `cubepod-v2` |
| `allowVolumeExpansion` | `true` |
| `volumeBindingMode` | `Immediate` |

### 方案 B：OpenEBS LVM-LocalPV（裸 NVMe 卷组）

如果您的节点拥有独立裸 NVMe 磁盘且倾向于简洁的本地 LVM 管理方式，可使用 OpenEBS LVM-LocalPV：

```bash
# 在每台拥有裸盘 /dev/nvme1n1 的工作节点上执行：
sudo pvcreate /dev/nvme1n1
sudo vgcreate cubepod-vg /dev/nvme1n1

# 安装 Operator 与 CubePod StorageClass：
kubectl apply -f deploy/k8s/openebs-lvm-operator.yaml
kubectl apply -f deploy/k8s/cubepod-lvm-sc.yaml
```

`deploy/k8s/cubepod-lvm-sc.yaml` 会创建 `StorageClass` `cubepod-lvm-block`（`provisioner: lvm.csi.openebs.io`、`storage: "lvm"`、`volgroup: "cubepod-vg"`、`volumeBindingMode: Immediate`、`allowVolumeExpansion: true`）。使用时请将 Helm 配置中的 `storage.storageClassName` 设为 `"cubepod-lvm-block"`。

## 第四步：集群验证与性能基准测试

运行 Kubernetes 集成验证前，请先将 `deploy/k8s/env.example` 复制为 `deploy/k8s/env.local`，并填好 `KUBECONFIG`、存储类及运行时参数（`Makefile` 相关目标会自动加载 `deploy/k8s/env.local`）。

### 功能 E2E 与块存储测试套件

```bash
# K8s E2E 辅助函数离线单元测试
go test -race -v ./test/k8s-e2e/...

# 集群裸块 CSI 与 Kata Pod 生命周期测试套件
make test-k8s-block   # 等价别名：make test-k8s-longhorn-v2

# 完整 Kubernetes 集群端到端测试套件
make test-k8s-e2e
```

### 基准测试与目标 SLO

针对真实集群运行 `TestK8sPerformanceBenchmark`，全面采集冷启动、命令延迟、目录同步、暂停/自动唤醒、RootFS 重置、CoW Fork 以及 WarmPool 认领耗时：

```bash
CUBEPOD_K8S_BENCH=1 \
CUBEPOD_BENCH_ITERATIONS=3 \
CUBEPOD_BENCH_OUT=/tmp/cubepod-kata-benchmark.md \
  go test -v -timeout 20m ./test/k8s-e2e/ -run TestK8sPerformanceBenchmark
```

基准测试覆盖以下关键操作与 `deploy/k8s/kata/README.md` 中定义的 CubePod SLO 目标：

| 测试项 | 目标 SLO | 度量内容 |
| :--- | :--- | :--- |
| `cold_create` | — | 端到端冷启动耗时（`PodBox` + PVC 绑定 + Kata + `podd` 就绪） |
| `exec_foreground_rtt` | — | 经由 Gateway → `podd` 的前台命令执行往返延迟 |
| `archive_sync` | `< 100ms` | 压缩 `tar.gz` 工作区上传并在 `podd` 内流式解包耗时 |
| `pause` / `pause_auto_resume` | `< 1500ms` | Pod 缩容至零后通过 `POST /connect` 或子域名流量自动唤醒的恢复耗时 |
| `reset_rootfs` | — | 递增 `rootfsGeneration` 清空 OS 系统状态并完好保留 `/workspace` 的耗时 |
| `cow_fork` | `< 1500ms` | 通过 CSI 块快照克隆并拉起子沙箱的耗时 |
| `warmpool_prewarm` / `warmpool_claim_e2e` | `< 200ms` E2E HTTP（控制器开销 `< 50ms`） | 经由 Gateway 从 `PodWarmPool` 认领预热 MicroVM 的耗时 |

## 下一步

- 返回 [Kubernetes 与 Helm 部署](/zh/deployment/kubernetes-helm/) 调整网关与控制器副本数。
- 阅读[生命周期管理、WarmPool 与 CoW Fork](/zh/concepts/lifecycle-warmpool-fork/) 深入了解 `PodWarmPool` 与 `cow_fork` 的内部协同机制。
