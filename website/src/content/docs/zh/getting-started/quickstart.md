---
title: 快速开始
description: 使用 Docker Compose 在本地启动 CubePod，创建持久化沙箱、执行命令与代码、体验暂停/恢复、CoW Fork 及按需桌面。
---

只需几分钟即可通过 `docker-compose` 在本地运行完整 CubePod 栈，并使用 Python SDK、TypeScript SDK 或 `cubepod` CLI 操作持久化沙箱。

## 前置条件

- **Docker Engine** 与 Docker Compose v2（`docker compose`）
- **Python 3.11+**（使用 Python SDK）和/或 **Node.js 20+**（使用 TypeScript SDK）
- *（可选，如需从源码构建二进制）* **Go 1.22+** 与 **Rust**（`cargo`）

## 1. 启动本地控制面

在 CubePod 仓库根目录启动 `cubepod-gateway`（`:8080`）、`cubepod-controller`（`:8081`）与 `cubepod-egress`（`:18080`）：

```bash
docker compose -f deploy/docker-compose/docker-compose.yaml up -d --build
```

验证网关健康状态：

```bash
curl -fsS http://127.0.0.1:8080/health
```

> **说明：** `deploy/docker-compose/docker-compose.yaml` 为 `cubepod-gateway` 配置了 `CUBEPOD_RUNTIME_MODE=docker-loopback`、挂载 `/var/run/docker.sock` 与 `cubepod-volumes`，并在本地开发模式下以 `-default-allow=true` 启动 `cubepod-egress`。如需在宿主机直接从源码构建全部 Rust 与 Go 二进制文件，可执行 `make build`（生成 `bin/podd-init`、`bin/podd`、`bin/cubepod-controller`、`bin/cubepod-gateway`、`bin/cubepod-egress` 与 `bin/cubepod`）。

## 2. 创建沙箱并验证持久化

按需选择下方的 SDK 或原生 CLI。

### Python SDK

从 PyPI 或仓库 `sdks/python/` 目录安装 Python SDK：

```bash
pip install cubepod
```

创建沙箱、向 `/workspace` 写入文件、执行命令、暂停算力（`0 CPU / 0 RAM`）、恢复连接并创建写时复制（CoW）Fork：

```python
from cubepod import CubePod

with CubePod(base_url="http://127.0.0.1:8080") as client:
    # 1. 创建持久化沙箱
    sbx = client.create("cubeplex-ubuntu24-desktop", timeout_seconds=3600)
    print("Created:", sbx.sandbox_id, "state:", sbx.state)

    # 2. 向 /workspace 写入文件并执行命令
    client.write_file(sbx.sandbox_id, "/workspace/hello.txt", b"persistent data\n")
    res = client.execute(sbx.sandbox_id, "cat /workspace/hello.txt && id")
    print("Output:", res.output)

    # 3. 暂停算力（保留 rootfs 与 /workspace 双持久卷）
    client.pause(sbx.sandbox_id)

    # 4. 重连自动唤醒，验证持久化数据依然存在
    client.connect(sbx.sandbox_id)
    print("After resume:", client.read_text_file(sbx.sandbox_id, "/workspace/hello.txt"))

    # 5. 创建独立的写时复制 Fork 子沙箱
    child = client.fork(sbx.sandbox_id)
    print("Forked child sandbox:", child.sandbox_id)

    # 6. 清理资源
    client.delete(child.sandbox_id)
    client.delete(sbx.sandbox_id)
```

### TypeScript / Node.js SDK

官方 TypeScript SDK 在 Node.js 20+ 上零第三方运行时依赖：

```bash
npm install cubepod
```

```typescript
import { Sandbox } from 'cubepod';

const sandbox = await Sandbox.create('cubeplex-ubuntu24-desktop', {
  baseUrl: 'http://127.0.0.1:8080',
  timeoutSeconds: 3600,
});

await sandbox.files.write('/workspace/hello.txt', 'persistent data\n');
const cmd = await sandbox.commands.run('cat /workspace/hello.txt');
console.log('Command output:', cmd.output);

// 通过 podd 内置有状态解释器执行代码（支持 Python, Bash, JS, TS, R, Java）
const exec = await sandbox.code.runCode('print(6 * 7)', { language: 'python' });
console.log('Interpreter:', exec.output);

// 暂停并在稍后透明唤醒恢复
await sandbox.pause();
await sandbox.connect();

// 崩溃一致性 CoW Fork
const child = await sandbox.fork();
await child.kill();
await sandbox.kill();
```

### `cubepod` CLI

使用 `make build-cli` 构建 CLI（产物位于 `bin/cubepod`），即可在终端驱动完整生命周期：

```bash
export CUBEPOD_GATEWAY_URL="http://127.0.0.1:8080"

# 检查网关健康状态
./bin/cubepod health

# 创建沙箱并执行命令
./bin/cubepod sandbox create --template cubeplex-ubuntu24-desktop
./bin/cubepod sandbox exec <sandbox-id> "uname -a && id"

# 读写文件并以 tar.gz 同步本地目录
./bin/cubepod sandbox write-file <sandbox-id> /workspace/app.py "print('hello from cubepod')"
./bin/cubepod sandbox read-file <sandbox-id> /workspace/app.py
./bin/cubepod sandbox archive-sync <sandbox-id> /workspace/project ./local-project

# 暂停、重连、Fork、重置 OS RootFS（保留 /workspace）或删除
./bin/cubepod sandbox pause <sandbox-id>
./bin/cubepod sandbox connect <sandbox-id>
./bin/cubepod sandbox fork <sandbox-id>
./bin/cubepod sandbox reset-rootfs <sandbox-id>
./bin/cubepod sandbox delete <sandbox-id>
```

## 3. 在保留 `/workspace` 的前提下重置 RootFS

当 Agent 意外破坏了 `/etc` 或 `/usr` 下的系统依赖库时，无需删除用户项目代码即可单独重置 OS 层：

```python
# 清空并分配新一代 OverlayFS upperdir（rootfs-gen<N>）
# 同时原封不动地重新挂载同一块 /workspace 持久卷
client.reset_rootfs(sbx.sandbox_id)
```

阅读 **[双卷持久化存储](/zh/concepts/dual-volume-persistence/)** 了解 `rootfsGeneration` 如何实现 OS 升级与工作区数据的严格解耦。

## 4. 启动交互式桌面（可选）

当沙箱使用桌面模板镜像时（参见 **[交互式桌面栈与签名面板代理](/zh/concepts/desktop-and-panel-proxy/)**），可以按需懒启动 `Xvfb`、`openbox`、Chromium CDP、`noVNC` 与 `ttyd`：

```python
session = client.start_desktop(
    sbx.sandbox_id,
    resolution="1280x800x24",
    initial_url="https://example.com",
)
print("noVNC URL:", session.desktop_url)
print("Terminal URL:", session.terminal_url)
print("Chromium CDP URL:", session.cdp_url)
```

> **提示：** 若模板镜像未安装 `/opt/cubepod/desktop/cubepod-desktop-ctl`，`start_desktop` 会快速返回 HTTP `422 desktop_not_installed`。

## 下一步

- 通过 **[E2B 无缝兼容指南](/zh/getting-started/e2b-compatibility/)** 零改造迁移现有 `e2b` / `e2b-code-interpreter` 代码。
- 在 **[平台架构](/zh/concepts/architecture/)** 中深入内核与控制面实现细节。
- 参考 **[Kubernetes 与 Helm 部署](/zh/deployment/kubernetes-helm/)** 在集群中启用 Kata Containers 与块存储。
