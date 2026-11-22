---
title: E2B 快速兼容接入
description: 通过环境变量将官方 E2B Python 与 TypeScript SDK 接入 CubePod。
---

CubePod 兼容核心 E2B REST 与 Connect-RPC 协议接口。现有的官方 E2B Python / TypeScript SDK 工作流只需配置对应环境变量，即可直接接入自建的 CubePod 持久化微虚拟机集群。

:::tip[详细兼容矩阵与架构差异参考]
本页为环境变量配置与极简调用代码的快速指引。关于按方法划分的详细兼容性矩阵、计算缩容至零 vs 物理内存快照等架构差异、实测版本约束及未测试边界，请参阅 **[E2B 兼容性与差异参考](/zh/reference/e2b-compatibility/)**。
:::

## 将官方 E2B SDK 指向 CubePod

根据 `cubepod-gateway` 是位于 HTTPS 通配符域名网关后方，还是本地直连 HTTP 端口，设置对应的环境变量：

### 生产环境 HTTPS + 通配符域名

```bash
export E2B_DOMAIN="cubepod.example.com"
export E2B_API_KEY="cpod_live_secret"
```

设置 `E2B_DOMAIN` 后，官方 E2B SDK 会自动将控制面请求发往 `api.<E2B_DOMAIN>`，并将沙箱端口访问路由至 `<port>-<sandboxID>.<E2B_DOMAIN>`（`cubepod-gateway` 通过匹配 `Host` 请求头的 `^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$` 自动识别端口与沙箱 ID）。

### 本地 HTTP / 直连网关

在本地连接 `http://127.0.0.1:8080` 且未配置 TLS 与通配符 DNS 时，可同时指定 `E2B_API_URL` 与 `E2B_SANDBOX_URL`（CubePod 仓库 `test/e2b-compat/` 集成测试同样采用此方式）：

```bash
export E2B_API_URL="http://127.0.0.1:8080"
export E2B_SANDBOX_URL="http://127.0.0.1:8080"
export E2B_API_KEY="cpod_test_api_key"
```

## 官方 E2B 调用示例

### 官方 `e2b` Python SDK（Sync & Async）

```python
from e2b import Sandbox

# 自动读取 E2B_API_URL / E2B_SANDBOX_URL / E2B_DOMAIN
with Sandbox.create("base", timeout=300) as sbx:
    sbx.files.write("/workspace/data.txt", "hello from e2b sdk")
    result = sbx.commands.run("cat /workspace/data.txt")
    print(result.stdout)
```

### 官方 `e2b-code-interpreter` SDK

`podd` 内置支持 **6 种语言**（`python`、`bash`、`javascript`、`typescript`、`r`、`java`）的有状态执行上下文，通过 NDJSON 流式协议与 `e2b-code-interpreter` 互通：

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create() as sbx:
    sbx.run_code("x = 21 * 2")
    execution = sbx.run_code("print(f'result={x}')")
    print(execution.logs.stdout)
```

## `cubepod-gateway` 协议实现概览

`cubepod-gateway`（`pkg/gateway/`）实现了核心 E2B 控制面与数据面（含基于 `:49983` 语义的 Connect-RPC 协议）：

<div class="cp-nowrap-col2">

| 协议面 | 已实现端点 | CubePod 内部行为 |
| :--- | :--- | :--- |
| **控制面** | `POST /sandboxes`<br/>`POST / GET /v2/sandboxes`<br/>`GET /sandboxes`<br/>`GET /sandboxes/{id}`<br/>`DELETE /sandboxes/{id}` | 创建或列举 `PodBox` 沙箱；响应体包含 E2B 必填字段（`sandboxID`、`templateID`、`clientID`、`envdVersion`、`state`、`startedAt`、`endAt`、`cpuCount`、`memoryMB`、`diskSizeMB`）。 |
| **生命周期与租约** | `POST /sandboxes/{id}/refreshes`<br/>`POST /sandboxes/{id}/timeout`<br/>`POST /sandboxes/{id}/pause`<br/>`POST /sandboxes/{id}/connect`<br/>`POST /v2/sandboxes/{id}/connect` | 续期超时或租约时间；暂停时释放 Pod 算力并保留双 PVC；收到 `connect` 或数据面流量时透明自动唤醒。 |
| **命令执行与流式日志** | `POST /sandboxes/{id}/commands`<br/>`POST /sandboxes/{id}/commands/run`<br/>`GET .../logs?cursor=N`<br/>`POST .../kill`<br/>Connect-RPC `/process.Process/*`:<br/>`Start`, `SendSignal`, `Connect`, `List`,<br/>`SendInput`, `StreamInput`, `Update` | 在 Guest 内默认以非特权 UID/GID `1000:1000`（`cubeplex`）运行命令与交互式 PTY 会话，支持游标增量拉取、64 KiB PTY 重连回放、窗口 resize 及 PGID 进程组级清理。 |
| **文件系统与归档同步** | `PUT / GET / DELETE /sandboxes/{id}/files`<br/>`GET .../files/list`<br/>`POST .../files/archive-sync`<br/>Connect-RPC `/filesystem.Filesystem/*`:<br/>`Read`, `Write`, `List`, `ListDir`, `Stat`,<br/>`Remove`, `MakeDir`, `WatchDir`,<br/>`CreateWatcher`, `GetWatcherEvents`,<br/>`RemoveWatcher` | 直接读写持久化 `/workspace` 或 RootFS 文件、基于 `inotify` 的目录变更监听，并支持带路径穿越防护的 `.tar`/`.tar.gz` 批量目录同步。 |
| **持久化卷与内容传输** | `POST / GET / DELETE /volumes`<br/>`/volumecontent/{id}/file`<br/>`/volumecontent/{id}/dir`<br/>`/volumecontent/{id}/path`<br/>`POST /sandboxes` 的 `volumeMounts` | 管理租户隔离的持久化卷及文件内容上传下载，支持挂载至 Docker 与 K8s Kata 沙箱（*v0.8.0 变更：* K8s 需配置 `CUBEPOD_K8S_VOLUME_MOUNTS=1` 与 `CUBEPOD_VOLUMES_SHARED_PVC`）。 |
| **沙箱资源指标** *(v0.6.0 新增)* | `GET /sandboxes/{sandboxID}/metrics`<br/>`GET /sandboxes/metrics?sandboxIds=...` | 向 `podd`（`GET /metrics`）实时采集 Guest CPU、内存、缓存与 `/workspace` 磁盘用量，支持 `start`/`end` 时间窗口过滤及单沙箱有界环形缓存（`120` 条），兼容 `sbx.get_metrics()` / `Sandbox.get_metrics()`。 |
| **密钥与网络规则注入** *(v0.6.0 支持 HA)* | `POST / GET /secrets`<br/>`GET / POST / DELETE /secrets/{id}`<br/>`network.rules.*.transform.headers`<br/>（`Secret.fill()` 生成 `${e2b.secrets.<name>}`） | 支持 `KubeSecretBackend` 多副本 HA 或 AES-256-GCM 本地加密存储 Write-Only 密钥，将 `Secret.fill()` 生成的 `${e2b.secrets.<name>}` 解析为每沙箱 `cpodref_` 占位符，并由 `cubepod-egress` 通过严格 mTLS 实时替换。 |
| **生命周期事件与 Webhook** *(v0.6.0 新增)* | `POST / GET /webhooks`<br/>`GET / PATCH / DELETE /webhooks/{id}`<br/>`GET /events`<br/>`GET /sandboxes/{id}/events` | 记录兼容 E2B `v1` 格式的沙箱生命周期事件（`created`、`paused`、`resumed`、`updated`、`forked`、`killed`，含 K8s 异步回收事件），并通过防 SSRF、HMAC 签名（`e2b-signature`）异步推送。 |
| **有状态代码解释器** | `POST /sandboxes/{id}/execute`<br/>`POST /sandboxes/{id}/code/execute`<br/>`POST /execute` | 管理 `podd` 内部 6 种语言解释器上下文（`python`、`bash`、`javascript`、`typescript`、`r`、`java`）。上下文生命周期在沙箱内维护。 |
| **子域名端口路由** | `Host: <port>-<sandboxID>.<domain>`<br/>`GET / POST /sandboxes/{id}/endpoints/{port}` | 自动唤醒已暂停沙箱，将 `/envd/*` 路径转发至 `podd`（`:49983`），并反代应用端口或签发 `/panel/{token}/` 访问地址。 |
| **模板目录与构建** | `/templates`, `/v2/templates`, `/v3/templates`<br/>`/templates/{id}/builds/{buildID}/status`<br/>`/templates/{id}/files/{hash}` | 启用 `--e2b-template-api=true` 与 `--template-build=true` 后即可使用 BuildKit 或本地 Docker 执行镜像构建。 |

</div>

## 超越 E2B 的 CubePod 扩展能力

使用 CubePod 原生 SDK（[Python](/zh/sdks/python/)、[TypeScript](/zh/sdks/typescript/)、[Go 与 CLI](/zh/sdks/go-and-cli/)）或 REST 超集接口时，还可直接调用标准 E2B 不具备的高级特性：

- **持久化可写 RootFS 与无损系统重置**：调用 `POST /sandboxes/{id}/reset-rootfs` 递增 `spec.rootfsGeneration`，分配干净的新 `upperdir` PVC 并完好保留 `/workspace`。
- **1 秒级 CoW Fork 分支克隆**：调用 `POST /sandboxes/{id}/fork`，通过 `FIFREEZE` 静默 -> CSI/reflink 克隆 -> `FITHAW` 瞬间克隆运行中沙箱的 OS 与工作区状态。
- **零信任 `cpodref_` 出站保险库**：在创建沙箱时注入 `secretBindings`，确保 Agent 仅见占位符而无法读取真实 API 密钥。
- **按需桌面与签名面板**：调用 `/sandboxes/{id}/desktop/{start,status,stop}` 懒启动 `Xvfb`、Chromium CDP、`noVNC` 与 `ttyd`，通过 `/panel/{token}/` 安全访问。

## 下一步

- 参阅 **[E2B 兼容性与差异参考](/zh/reference/e2b-compatibility/)** 了解完整接口矩阵、版本约束与暂停语义对比。
- 查看 **[API 参考](/zh/api/)** 浏览完整 OpenAPI 3.1 契约。
- 对比原生客户端特性：**[Python SDK](/zh/sdks/python/)** 与 **[TypeScript SDK](/zh/sdks/typescript/)**。
- 查看 **[CRD 参考](/zh/reference/crds/)** 了解 `PodBox` 与 `PodTemplate` 的 Kubernetes Schema。
