---
title: API 概览
description: 基于 api/openapi/cubepod-v1.yaml 与 pkg/gateway 的完整 HTTP、SSE、NDJSON、Connect-RPC、WebSocket/WebRTC Panel 代理与模板构建接口总览。
---

CubePod Gateway（`cmd/cubepod-gateway`）对外提供一套**兼容 E2B 的超集** HTTP 与流式接口，权威契约定义在 `api/openapi/cubepod-v1.yaml`（OpenAPI `3.1.0`，规范版本 `0.4.0`）。所有官方 SDK（[Python](/zh/sdks/python/)、[TypeScript](/zh/sdks/typescript/) 与 [Go 与 CLI](/zh/sdks/go-and-cli/)）均基于这一网关接口层构建。

如需在线交互调试或查看完整 Schema，请访问[交互式 API 参考](/zh/api/)或下载原始 OpenAPI YAML 文件 [`/openapi.yaml`](/openapi.yaml)（运行中的 Gateway 同样通过 `GET /openapi.yaml` 实时提供该规范）。AI 工具或文档检索代理也可直接拉取 [`/llms.txt`](/llms.txt) 或 [`/llms-full.txt`](/llms-full.txt)。

## 基础地址、认证与子域名路由

- **认证方式**：当 Gateway 启用鉴权时，在请求头传入 `Authorization: Bearer <token>`；本地无鉴权开发模式下可省略。
- **健康检查与可观测性**：
  - `GET /health` — Gateway 存活与就绪状态 JSON。
  - `GET /metrics` — Prometheus 监控指标。
  - `GET /openapi.yaml` — 返回规范的 OpenAPI 3.1 YAML 文档。
- **子域名 Host 路由**：匹配正则 `^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$` 的请求（例如 `https://3000-sbx-abc123.sandboxes.example.com/`）会自动解析目标 `port`（`3000`）与 `sandboxID`（`sbx-abc123`）；若沙箱处于 `Paused` 状态则触发自动唤醒，随后将 HTTP 或 WebSocket 流量直接反向代理至沙箱内的 `PodIP:<port>`（并对本地内置端口 `49999` `/execute` 与 `49983` Connect-RPC 提供直接转发支持）。

## 沙箱生命周期

| 方法与路径 | 用途 | 核心查询/请求体参数 |
| :--- | :--- | :--- |
| `POST /sandboxes`（`POST /v2/sandboxes`） | 创建沙箱（存在可用 WarmPool 时直接认领） | `templateID`、`timeout` / `ttlSeconds`、`cpuCount`、`memoryMB`、`envVars`（支持 `cpodref_`）、`workspacePvcSize`、`metadata` |
| `GET /sandboxes` | 列举沙箱 | 查询过滤参数：`state`、`templateID` |
| `GET /sandboxes/{id}` | 查询沙箱状态、IP、`rootfsGeneration` 与阶段信息 | 路径参数：`id` |
| `DELETE /sandboxes/{id}` | 销毁沙箱 | 查询参数：`deleteVolumes`（`false` 保留 PVC；`true` 同步删除 PVC） |
| `POST /sandboxes/{id}/pause` | 将计算资源缩容至零，同时保留 `rootfs` 与 `/workspace` PVC | 路径参数：`id` |
| `POST /sandboxes/{id}/connect` | 唤醒处于 `Paused` 状态的沙箱并等待 `podd` 就绪 | 可选超时更新字段 |
| `POST /sandboxes/{id}/timeout` | 设置或延长沙箱超时时间 | 请求体：`{ "timeoutSeconds": <int> }` |
| `POST /sandboxes/{id}/refreshes` | 刷新租约与延长 TTL（E2B 兼容别名） | 请求体：`{ "duration": <int> }` |
| `POST /sandboxes/{id}/lease` | 续期沙箱独占租约 | 请求体：租约时长 / TTL |
| `POST /sandboxes/{id}/reset-rootfs` | 递增 `rootfsGeneration` 清空 OS RootFS 层，同时完好保留 `/workspace` | 可选重置/模板参数 |
| `POST /sandboxes/{id}/fork` | 通过 CSI 块快照（`cow_fork`）克隆出独立的子沙箱 | 可选分支参数 |

## 命令执行、游标日志与 SSE 流式输出

所有前台与后台命令均通过沙箱内的 Rust `podd` 守护进程（`:49983`）执行，且 stdout/stderr 会按单调字节偏移游标记录，便于断线续传。

| 方法与路径 | 用途 | 说明 |
| :--- | :--- | :--- |
| `POST /sandboxes/{id}/commands`（`POST /sandboxes/{id}/commands/run`） | 执行命令 | 支持 `cmd`/`command`、`args`、`cwd`、`envs`/`env`、`background`、`timeout`/`timeoutSeconds`、`uid`、`gid` |
| `GET /sandboxes/{id}/commands/{cmdID}/status` | 查询命令运行状态与退出码 | 返回 `exitCode`、`running` 及信号信息 |
| `GET /sandboxes/{id}/commands/{cmdID}/logs` | 增量拉取命令日志 | 查询参数：`cursor=<int>`；返回日志文本与最新 `nextCursor` |
| `GET /sandboxes/{id}/commands/{cmdID}/stream` | 以 Server-Sent Events（`text/event-stream`）流式推送日志 | Gateway 每 `50ms` 轮询一次 `podd` 日志接口并持续下发 SSE 帧直至进程退出 |
| `POST /sandboxes/{id}/commands/{cmdID}/kill` | 终止运行中的命令进程 | 向沙箱内目标进程发送终止信号 |

## 文件读写与 `archive-sync` 同步

| 方法与路径 | 用途 | 说明 |
| :--- | :--- | :--- |
| `PUT /sandboxes/{id}/files`（`PUT /files`） | 写入原始文件字节 | 查询参数：`path`，可选 `uid`、`gid`、`mode` |
| `GET /sandboxes/{id}/files`（`GET /files`） | 读取原始文件字节 | 查询参数：`path` |
| `DELETE /sandboxes/{id}/files`（`DELETE /files`） | 删除文件或目录 | 查询参数：`path` |
| `GET /sandboxes/{id}/files/list`（`GET /files/list`） | 列举目录条目 | 查询参数：`path` |
| `POST /sandboxes/{id}/files/archive-sync` | 上传压缩 `tar.gz` 原子同步工作区 | 查询参数：`dest` / `destination`、`uid`、`gid`、`remove_paths` / `removePaths`；`podd` 校验相对路径条目，严格拒绝 `..` 或以 `/` 开头的路径穿越 |

:::tip
为兼容 E2B `envd` 客户端路径习惯，直接路径 `/files` 与 `/files/list` 会自动通过 `E2b-Sandbox-Id`、`X-Sandbox-ID`、查询参数或子域名 Host 头识别目标沙箱。
:::

## Code Interpreter（`NDJSON` 与多语言内核）

CubePod 在 `podd`（`crates/podd/src/interpreter.rs`）内原生集成多语言状态化代码解释器，支持 **`python`**、**`bash`**、**`javascript`**、**`typescript`**、**`r`** 与 **`java`** 六种内核：

- **执行代码**：`POST /sandboxes/{id}/code/execute`（别名 `POST /sandboxes/{id}/execute` 与 `POST /execute`）。
  - 流式返回 `application/x-ndjson` 事件行，包含 `stdout`、`stderr`、`result`（支持 `text`、Base64 `png`、`html`、`json`）、`error`（`name`、`value`、`traceback`）与 `number_of_executions`。
- **持久化上下文管理**：沙箱内 `podd` 暴露 `/contexts` 与 `/v1/code/contexts`（Gateway 对应路由为 `/sandboxes/{id}/code/contexts` 及 `/sandboxes/{id}/code/contexts/{contextId}`），支持创建、查询、重启与删除多轮有状态上下文。

## 交互式桌面与签名 Panel 反向代理

针对带桌面的镜像（如 `cubeplex-ubuntu24-desktop`），Gateway 负责协调启动 Xvfb/x11vnc/noVNC/Selkies 并签发基于 HMAC 签名的短时反代链接：

| 方法与路径 | 用途 | 说明 |
| :--- | :--- | :--- |
| `POST /sandboxes/{id}/desktop/start` | 启动显示服务与桌面会话 | 校验 `resolution` 格式（`^[0-9]{3,5}x[0-9]{3,5}x(16\|24\|32)$`），支持可选 `initialUrl` 与 `expiresInSeconds`；若镜像未包含桌面组件则返回 `422 desktop_not_installed`。同时提供 `/v1/sandboxes/{id}/desktop/start` 别名 |
| `GET /sandboxes/{id}/desktop/status` | 查询桌面运行状态与各服务 URL | 同理提供 `/v1/sandboxes/{id}/desktop/status` 别名 |
| `POST /sandboxes/{id}/desktop/stop` | 停止桌面相关服务 | 同理提供 `/v1/sandboxes/{id}/desktop/stop` 别名 |
| `GET /sandboxes/{id}/endpoints/{port}` 与 `POST /sandboxes/{id}/endpoints/{port}` | 为沙箱内任意 TCP/WebSocket 端口生成签名访问令牌 | 令牌格式：`base64url(sandboxID:port:expUnix).hmacSha256Sig` |
| `ANY /panel/{token}/...` | 校验 HMAC 后透明代理 HTTP、WebSocket 与 WebRTC 流量 | 剥离 `/panel/{token}` 前缀并转发至沙箱内 `PodIP:<port>` |

架构原理与安全细节请参阅[桌面环境与签名 Panel 代理](/zh/concepts/desktop-and-panel-proxy/)。

## E2B Connect-RPC 兼容接口

为实现与 E2B 官方 SDK 的无痛对接（`pkg/gateway/connectrpc.go`），Gateway 原生解析并路由以下 Connect-RPC 服务——无论请求直接发往根路径 `/`（由 `E2b-Sandbox-Id`、`X-Sandbox-ID`、查询参数或子域名解析沙箱 ID），还是嵌套在 `/sandboxes/{id}/` 下：

- **Process 服务**：
  - `POST /process.Process/Start`
  - `POST /process.Process/SendSignal`
  - `POST /process.Process/Connect`
  - `POST /process.Process/List`
- **Filesystem 服务**：
  - `POST /filesystem.Filesystem/Read`
  - `POST /filesystem.Filesystem/Write`
  - `POST /filesystem.Filesystem/List`
  - `POST /filesystem.Filesystem/ListDir`
  - `POST /filesystem.Filesystem/Stat`
  - `POST /filesystem.Filesystem/Remove`
  - `POST /filesystem.Filesystem/MakeDir`

接口同时支持纯 `application/json` 载荷以及带有 5 字节信封头的 `application/connect+json` 流式帧（1 字节标志位：`0` 表示消息帧、`2` 表示流结束帧 + 4 字节大端无符号 `uint32` 长度）。

## WarmPool 预热池

| 方法与路径 | 用途 |
| :--- | :--- |
| `POST /warmpools` | 创建或更新指定模板与副本数的 `PodWarmPool` |
| `GET /warmpools` | 列举已配置的 WarmPool 及当前就绪/可用副本数 |

## 模板目录与构建（`E2BTemplateAPI`）

当 Gateway 与 Controller 开启 `--e2b-template-api=true` 时，完整的模板目录与 BuildKit 构建端点即刻生效：

| 方法与路径 | 用途 | 关键请求头与约束 |
| :--- | :--- | :--- |
| `GET /templates` 与 `GET /v2/templates` | 列举模板 | `/v2/templates` 支持游标分页，通过响应头 `X-Next-Token` 返回下一页游标 |
| `POST /templates` 与 `POST /v2/templates` | 发起模板构建请求 | 返回 `templateID`、`buildID` 与状态 |
| `POST /v3/templates` | 幂等异步模板构建申请（v3） | 支持 `Idempotency-Key` 请求头，返回 `202 Accepted` |
| `GET /templates/{id}`、`PATCH /templates/{id}`、`PATCH /v2/templates/{id}`、`DELETE /templates/{id}` | 查询、更新（`public` 可见性）或删除模板 | — |
| `GET /templates/aliases/{alias}` | 通过人类可读别名解析模板 ID 与元数据 | — |
| `GET /templates/{id}/files/{hash}` 与 `PUT /templates/{id}/files/{hash}` | 查询或上传基于 SHA-256 内容寻址的构建上下文压缩包 | `{hash}` 必须匹配 `^[0-9a-f]{64}$` |
| `POST /v2/templates/{id}/builds/{buildID}` | 提交 Dockerfile 配方步骤并启动构建 | 支持本地 Docker 或集群内 BuildKit |
| `GET /templates/{id}/builds/{buildID}/status` | 查询构建状态、`kubeTemplateName` 与 `imageRef` | — |
| `GET /templates/{id}/builds/{buildID}/logs` | 获取结构化构建日志流 | — |
| `POST /templates/tags`、`DELETE /templates/tags`、`GET /templates/{id}/tags` | 为模板分配、移除或查询标签 | — |

## 内部零信任出站凭证交换

| 方法与路径 | 用途 |
| :--- | :--- |
| `POST /internal/v1/egress/exchange` | 由 `cubepod-egress` 边车代理通过单沙箱 mTLS 发起调用，在放行出站请求前将 `cpodref_` 占位符实时置换为真实上游密钥 |

## 下一步

- 前往[交互式 API 参考](/zh/api/)在线调试请求。
- 查看各语言客户端实现：[Python SDK](/zh/sdks/python/)、[TypeScript SDK](/zh/sdks/typescript/) 与 [Go SDK 与 CLI](/zh/sdks/go-and-cli/)。
