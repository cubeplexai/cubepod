---
title: Guest 内核运行时 (podd-init & podd)
description: 深入了解 CubePod 如何通过零污染注入将单线程 Rust PID 1 (podd-init) 与 49983 执行守护进程 (podd) 注入任意标准 OCI 镜像。
---

为了开箱即用地兼容任意用户 Docker 镜像，同时避免在用户的持久化 RootFS 层中残留平台二进制文件，CubePod 将 Guest 运行时拆分进独立的注入镜像（`images/podd/Dockerfile`）与两个静态编译的 Rust 二进制：**`podd-init`**（`crates/podd-init/`）和 **`podd`**（`crates/podd/`）。

## 零污染二进制注入机制

**用户沙箱镜像绝不应预先打包 `podd-init` 或 `podd`。** 标准社区镜像（如 `ubuntu:24.04`、`python:3.12` 或定制桌面/ML 镜像）可以保持完全纯净：

1. `images/podd/Dockerfile` 将编译好的 `podd-init` 与 `podd` 放入轻量级的 `busybox:1.37-musl` 镜像 `/opt/cubepod/dist/` 目录中。
2. **Kubernetes 模式**：Pod 启动阶段由名为 `podd-installer` 的 `initContainer` 将 `/opt/cubepod/dist/{podd-init,podd}` 复制到共享 `emptyDir` 卷 `/opt/cubepod/bin`。
3. **本地 Docker Loopback 模式**：`pkg/runtime/docker.go` 通过只读绑定挂载（`-v <binDir>:/opt/cubepod/bin:ro`）直接注入，并覆盖容器入口为 `--entrypoint /opt/cubepod/bin/podd-init`。
4. **跨 `pivot_root` 保留**：`podd-init` 在脱离旧根文件系统前，将 `/opt/cubepod` 递归绑定挂载（`--rbind`）进入合并后的新根目录。

由于 `/opt/cubepod` 始终位于临时 `emptyDir` 或宿主机只读挂载中而非持久化的 `upperdir` 内，Pod 重启或恢复时可随时平滑升级运行时二进制，绝不会污染用户 OS 卷。

## 为什么 `podd-init` 必须是单线程 Rust？

`podd-init`（`crates/podd-init/src/main.rs`）在执行 `pivot_root(2)` 与 `execvp(3)` 之前充当容器/MicroVM 的 **PID 1**：

- 如果进程在调用 Linux `pivot_root(2)` 或修改挂载命名空间时存在多个操作系统线程，内核会因共享线程状态/文件引用而返回 `EINVAL` 或泄漏旧挂载点。
- Go 运行时在进入 `main()` 之前即会拉起多个调度与监控（sysmon）OS 线程，因而不适合直接承担 PID 1 `pivot_root` 职责。
- 采用单线程、无 GC 的极简 Rust 编写 `podd-init`，能够以完全确定性的系统调用顺序完成块设备修复、`OverlayFS` 组装、根切换以及干净的 `execvp` 进程替换。

## PID 1 启动时序 (`crates/podd-init/src/main.rs`)

启动后，`podd-init` 读取环境变量配置（默认 `CUBEPOD_ROOTFS_DEV=/dev/cubepod-rootfs`、`CUBEPOD_WORKSPACE_DEV=/dev/cubepod-workspace`、`CUBEPOD_UPPER_MOUNT=/mnt/cubepod-upper`、`CUBEPOD_MERGED_ROOT=/mnt/cubepod-merged`、`CUBEPOD_UID/GID=1000`、`CUBEPOD_INIT_STATE_PATH=/run/cubepod/init-state.json`、`CUBEPOD_SKIP_PIVOT`），依次执行：

1. **隔离挂载传播**：调用 `make_mount_private("/")`（`MS_PRIVATE | MS_REC`），防止 Guest 内挂载事件反向泄漏至外部命名空间。
2. **回退分支判定（`volumeMode: Filesystem`）**：若设置了 `CUBEPOD_SKIP_PIVOT=1` 或块设备不存在，`podd-init` 会跳过格式化与 `pivot_root`，在 `/run/cubepod/init-state.json` 写入 `pivoted: false`，修正 `/workspace` 权限（`1000:1000`）后直接启动 `podd`。
3. **`ext4` 自愈准备 (`ensure_ext4`)**：
   - 使用 `blkid` 探测 `/dev/cubepod-rootfs` 与 `/dev/cubepod-workspace`。
   - **首次启动空白设备**：执行 `mkfs.ext4 -F -L <label> -m 0 -E lazy_itable_init=0,lazy_journal_init=0`，同步初始化文件系统元数据，避免启动后后台惰性初始化抖动。
   - **再次挂载或 CoW Fork 设备**：运行非交互式 `e2fsck -p`；若刚挂载的设备返回退出码 `8`（`EBUSY`），自动以 **500 ms 间隔重试最多 10 次**。
4. **挂载 RootFS `OverlayFS`**：将 RootFS `ext4` 以 `-o noatime` 挂载到 `/mnt/cubepod-upper`，创建 `upper/` 与 `work/` 目录，再将 `overlay`（`lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work`）挂载至 `/mnt/cubepod-merged`。
5. **挂载与赋权 `/workspace`**：将 `/dev/cubepod-workspace` 挂载到 `<merged>/workspace` 并 `chown 1000:1000`（`cubeplex`），确保后续 Agent 命令天然具备读写权限。
6. **传递虚拟文件系统与网络配置**：将 `/proc`、`/sys`、`/dev`、`/run`、`/opt/cubepod` 以 `--rbind` 映射进 `<merged>`，同时 `--bind` `/etc/resolv.conf`、`/etc/hosts` 和 `/etc/hostname`。
7. **执行 `pivot_root` 与卸载旧根**：切换到 `<merged>` 并把原根置于 `.old_root`（受限环境自动回退至 `chroot`），随后对 `/.old_root` 调用 `umount2(MNT_DETACH)`。
8. **写入诊断状态并移交控制权**：写入 `/run/cubepod/init-state.json`（`pivoted: true` 及设备挂载信息），最后 `execvp` 替换为 `/opt/cubepod/bin/podd` 继续担任 PID 1。

## `podd` 执行与控制守护进程 (`:49983`)

接管 PID 1 后，`podd`（`crates/podd/src/main.rs`）承担以下运行时职责：

- **Subreaper 与僵尸进程回收**：安装 `install_zombie_reaper`，彻底清除 Agent 子进程退出后的僵尸进程残留。
- **引导命令与最小权限执行**：若环境变量设置了 `CUBEPOD_START_CMD` 则后台拉起该服务；所有面向 Agent 的命令执行与文件写入默认降权至非特权身份 `1000:1000`（可通过 `CUBEPOD_RUN_AS_USER`、`CUBEPOD_RUN_AS_GROUP`、`CUBEPOD_RUN_AS_USERNAME` 覆盖）。

`podd` 在 **`:49983`** 监听 HTTP/1.1 与 Connect-RPC 请求，专供 `cubepod-gateway` 与 `cubepod-controller` 内部调用：

### 生命周期与崩溃一致性冻结 (`crates/podd/src/lifecycle.rs`)

| 端点 | 内核系统调用与安全机制 | 用途 |
| :--- | :--- | :--- |
| `GET /health` | 就绪检查 | 在冷启动或唤醒恢复期间由 `cubepod-gateway`（`waitForPoddReady`）轮询。 |
| `POST /v1/lifecycle/prepare-stop` | `libc::sync()` | 优雅停机或暂停前将脏页刷入块设备。 |
| `POST /v1/lifecycle/quiesce` | `sync` + `syncfs` + `FIFREEZE` (`0xC0045877`) | 在 CSI 快照或 `reflink` Fork 前冻结文件系统写入。接收 `maxFreezeMs`（默认 `1500` ms，限制在 `[100, 5000]` ms），由单调时钟看门狗守护——即便控制器崩溃也能自动解冻以防死锁。 |
| `POST /v1/lifecycle/thaw` | `FITHAW` (`0xC0045878`) | 快照/克隆完成后立即解除文件系统冻结。 |

### 命令执行与进程管理 (`crates/podd/src/commands.rs`)

- **REST 执行**：`POST /v1/commands/run`（支持前台阻塞与后台异步）、`GET /v1/commands/{id}/status`、`GET /v1/commands/{id}/logs?cursor=N`（基于字节偏移游标的增量流式读取）以及 `POST /v1/commands/{id}/kill`（按负 `PGID` 杀死整棵进程树，杜绝孤儿进程）。
- **Connect-RPC 兼容**：提供 `/process.Process/Start` 与 `/process.Process/SendSignal` 以兼容官方 E2B SDK。

### 文件传输与流式 `archive-sync` (`crates/podd/src/files.rs`)

- **单文件操作**：`PUT /v1/files`、`GET /v1/files`、`DELETE /v1/files`、`GET /v1/files/list`，自动应用 UID/GID 权限。
- **高吞吐 `POST /v1/files/archive-sync`**：直接接收 `.tar` 或 `.tar.gz` 字节流并在 Guest 内流式解压，强制拦截 `..` 与绝对前导斜杠等路径穿越攻击，支持指定 `dest`、文件属主 `uid`/`gid`（`1000:1000`）与 `remove_paths` 同步清理。

### 多语言有状态代码解释器 (`crates/podd/src/interpreter.rs`)

通过 `/execute`、`/v1/code/execute` 与 `/contexts` 提供 **6 种持久化内核**：

- `python`
- `bash`
- `javascript`
- `typescript`
- `r`
- `java`

执行结果以 NDJSON 事件流（`stdout`、`stderr`、`result`、`error`）实时返回，并在同一上下文中持续保留变量与状态。

## 下一步

- 了解 `podd` 的 `FIFREEZE`/`FITHAW` 机制如何支撑秒级 Fork：**[生命周期、WarmPool、CoW Fork 与高可用](/zh/concepts/lifecycle-warmpool-fork/)**。
- 阅读架构设计文档：**[CEP 提案总览](/zh/ceps/)**。
