---
title: 简介
description: 了解 CubePod 的核心定位、为什么长周期 AI Agent 需要持久化可写 RootFS MicroVM，以及 CubePod 如何融合 E2B 兼容性与云原生隔离。
---

**CubePod** 是一个开源、云原生的 **AI Agent 持久化可写 RootFS MicroVM 执行平台（Persistent Writable RootFS MicroVM Execution Platform for AI Agents）**，为 **CubePlex** 以及更广泛的 **E2B / MCP** 开源 Agent 生态提供底层算力与沙箱底座。

与在会话暂停或重启后即丢弃已安装依赖、浏览器状态与系统配置的无状态容器或瞬态沙箱不同，CubePod 为每个 AI Agent 提供具备硬件级隔离的沙箱，确保 **完整操作系统根目录（`/`、`/usr`、`/etc`、`/var/lib`）与用户工作区（`/workspace`）双双独立持久化**。

## 为什么持久化可写 RootFS 至关重要

现代自主 AI Agent 远不止执行单段 Python 脚本。在持续数小时乃至数天的复杂任务中，Agent 需要安装系统软件包（`apt`、`pip`、`npm`、Playwright 浏览器）、配置系统服务或守护进程、认证 CLI 工具、编译构建产物并持续修改代码仓库。

传统沙箱平台往往迫使开发者在两难中妥协：

- **瞬态 MicroVM**：提供强 Hypervisor 隔离，但其 RootFS 仅驻留于内存或临时可写层——一旦暂停或重启，操作系统内安装的软件与系统配置便全部丢失，除非每次变更都重新构建镜像。
- **单卷持久化容器**：保留 `/workspace` 中的代码，但缺乏硬件级 VM 边界，且在 Pod 重建后依旧会丢失操作系统级软件安装与环境配置。

CubePod 彻底消除了这一取舍：直接在 **Kata Containers MicroVM** 中启动**纯净无修改的标准 OCI 镜像**，并挂载**双持久化存储卷**——一卷作为 Linux 完整 RootFS 的 `OverlayFS` 可写 `upperdir`，另一卷独立专供 `/workspace`。

## 平台能力对比

| 能力维度 | 普通容器 | 瞬态 MicroVM | CubePod |
| :--- | :--- | :--- | :--- |
| **计算隔离层** | 共享宿主机 Linux 内核 | 硬件 VM（`Firecracker` / `QEMU`） | 硬件 VM（`Kata QEMU` / `Cloud Hypervisor`）+ 本地 Docker Loopback |
| **OS RootFS 持久化** | 临时可写层 | 临时 Scratch 层 | **持久化 `OverlayFS` `upperdir`**（`<box>-rootfs-pvc`） |
| **Workspace 持久化** | 可选 PVC 挂载 | 可选或随停止丢失 | **解耦的独立持久卷**（`<box>-workspace-pvc`） |
| **RootFS 重置 / 系统升级** | 重建整套容器 | 重新打包镜像 | 单调递增 `rootfsGeneration` 替换——**`/workspace` 保持完好** |
| **冷启动延迟** | ~1–3 秒 | ~150 毫秒 | **`< 50 ms`**（基于预挂载 `PodWarmPool` 原子 CAS 认领） |
| **算力休眠降本** | 停止即丢弃运行态 | 快照或销毁 | `desiredState: Paused` 销毁计算 Pod（`0 CPU / 0 RAM`），保留双 PVC |
| **状态分支 (CoW Fork)** | 镜像 Commit 耗时较长 | VM 快照 | **崩溃一致性 CoW Fork**（`FIFREEZE` -> CSI 克隆 / reflink -> `FITHAW`） |
| **密钥与出站安全** | 真实环境变量注入沙箱 | 真实环境变量注入沙箱 | **`cpodref_` 占位符** + 基于每 Pod mTLS 的选择性 HTTPS MITM |
| **SDK 与协议生态** | 自定义 API | E2B REST / Connect-RPC | **100% E2B 无缝协议兼容** + 原生 Python、TS、Go、CLI 与 MCP |

## 核心架构支柱

1. **[双卷持久化存储](/zh/concepts/dual-volume-persistence/)**：严格隔离可写操作系统 RootFS（`<box>-rootfs-pvc` 或 `<box>-rootfs-gen<N>-pvc`）与用户项目数据（挂载至 `/workspace` 的 `<box>-workspace-pvc`）。递增 `rootfsGeneration` 即可清理或升级 OS 状态，而 `/workspace` 字节不损。
2. **[基于 `podd-init` 与 `podd` 的纯净镜像运行机制](/zh/concepts/in-guest-init-and-podd/)**：用户镜像无需内嵌任何专用 Agent。Kubernetes 中通过 `initContainer`（`podd-installer`）或本地 Docker 绑定挂载在启动期注入 `/opt/cubepod/bin/{podd-init,podd}`。单线程 Rust PID 1（`podd-init`）完成 `ext4` 自愈检查、挂载 `OverlayFS`、执行 `pivot_root` 并 `execvp` 移交至 `podd`（`:49983`）。
3. **[WarmPool、Pause-to-Zero、CoW Fork 与无状态高可用](/zh/concepts/lifecycle-warmpool-fork/)**：`PodWarmPool` 预启动并预挂载备用 MicroVM，实现 `< 50 ms` 原子认领；`spec.forkFrom` 提供 `FIFREEZE`/`FITHAW` 崩溃一致性克隆；`cubepod-gateway` 直接基于 Kubernetes `PodBox` CRD 重建状态，实现无状态多副本高可用。
4. **[E2B 无缝兼容](/zh/getting-started/e2b-compatibility/)**：只需将 `E2B_DOMAIN` 或 `E2B_API_URL` 指向 `cubepod-gateway`，现有 `e2b`、`e2b-code-interpreter`、`@e2b/code-interpreter` 以及 LangChain、LlamaIndex、OpenHands 工作流即可零改代码运行。
5. **[零信任出站代理与凭证保险库](/zh/concepts/zero-trust-egress/)**：真实 API 密钥绝不进入沙箱内存或磁盘。沙箱内仅暴露 32 位 Base32 的 `cpodref_` 占位符；`cubepod-egress` 通过 `nftables` 透明劫持、DNS 策略拦截与选择性 HTTPS MITM，在每 Pod mTLS 认证下与 `cubepod-gateway` 实时完成密钥替换。
6. **[按需桌面与签名面板代理](/zh/concepts/desktop-and-panel-proxy/)**：桌面镜像待机内存接近 0 MiB，仅在调用时懒启动 `Xvfb`、`openbox`、Chromium CDP（`:9222`）、`x11vnc`/`noVNC`（`:8080`）与 `ttyd`（`:7681`），并通过 HMAC-SHA256 `/panel/{token}/` 代理安全暴露。

## 双运行时对等设计

CubePod 的每一项核心机制均在两类运行环境中保持一致语义：

- **生产级 Kubernetes（`Kata + CSI Block`）**：以 `kata-qemu` / `kata-clh` 硬件隔离 Pod 挂载两块 `volumeMode: Block` PVC（`/dev/cubepod-rootfs` 与 `/dev/cubepod-workspace`），并在集群不支持块存储时优雅回退至 `volumeMode: Filesystem`。
- **本地开发（`Docker + Loopback ext4`）**：启用 `CUBEPOD_RUNTIME_MODE=docker-loopback`（例如通过 Docker Compose），在本地稀疏 `.ext4` 回环文件之上运行与生产完全一致的 `podd-init` + `podd` Guest 栈。

## 下一步

- 通过 **[快速开始](/zh/getting-started/quickstart/)** 在本地启动栈并运行首个持久化沙箱。
- 阅读 **[E2B 无缝兼容指南](/zh/getting-started/e2b-compatibility/)** 接入现有 E2B Agent 代码。
- 深入 **[平台架构](/zh/concepts/architecture/)** 了解控制面与数据面协作机制。
- 前往 **[Kubernetes 与 Helm 部署](/zh/deployment/kubernetes-helm/)** 将 CubePod 部署到生产集群。
