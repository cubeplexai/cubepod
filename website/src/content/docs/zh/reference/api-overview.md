---
title: API 概览
description: 基于 api/openapi/cubepod-v1.yaml 与 pkg/gateway 的完整 HTTP、SSE、NDJSON、Connect-RPC、WebSocket/WebRTC Panel 代理与模板构建接口总览。
---

CubePod Gateway（`cmd/cubepod-gateway`）对外提供一套**兼容 E2B 的超集** HTTP 与流式接口，权威契约定义在 `api/openapi/cubepod-v1.yaml`（OpenAPI `3.1.0`，规范版本 `0.7.0`）。所有官方 SDK（[Python](/zh/sdks/python/)、[TypeScript](/zh/sdks/typescript/) 与 [Go 与 CLI](/zh/sdks/go-and-cli/)）均基于这一网关接口层构建。

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
| `POST /sandboxes/{id}/fork` *(v0.6.0 变更)* | 通过 CSI（`pvc-clone` 或 `snapshot` 模式）/ reflink 克隆出独立的子沙箱 | 可选分支参数 |
| `GET /sandboxes/{sandboxID}/metrics` *(v0.6.0 新增)* | 查询单个沙箱的时间序列 `SandboxMetric` 快照 | 查询参数：`start`、`end`（Unix 秒或 RFC3339）、`autoResume`（默认 `true`；`false` 仅读取缓存环形缓冲） |
| `GET /sandboxes/metrics` *(v0.6.0 新增)* | 批量查询多个沙箱的最新 `SandboxMetric` 快照 | 查询参数：`sandboxIds`（逗号分隔或重复参数）；返回 `{"sandboxes": {...}}`，不会唤醒暂停沙箱 |

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
- **持久化上下文管理**：沙箱内 `podd` 在内部暴露 `/contexts` 与 `/v1/code/contexts`（可通过子域名端口路由直接访问 `:49983`），支持创建、查询、重启与删除多轮有状态上下文。通过 Gateway 路由 `POST /sandboxes/{id}/code/execute` 调用代码时，客户端可在执行载荷中通过 `context_id` 直接指定已存在的上下文。

## 交互式桌面与签名 Panel 反向代理

针对带桌面的镜像（如 `cubeplex-ubuntu24-desktop`），Gateway 负责协调启动 Xvfb/x11vnc/noVNC/Selkies 并签发基于 HMAC 签名的短时反代链接：

| 方法与路径 | 用途 | 说明 |
| :--- | :--- | :--- |
| `POST /sandboxes/{id}/desktop/start` | 启动显示服务与桌面会话 | 校验 `resolution` 格式（`^[0-9]{3,5}x[0-9]{3,5}x(16\|24\|32)$`），支持可选 `initialUrl`、`expiresInSeconds` 与 `transport`（`"webrtc" \| "novnc"`，*v0.6.0 新增*）；若镜像未包含桌面组件则返回 `422 desktop_not_installed`。同时提供 `/v1/sandboxes/{id}/desktop/start` 别名 |
| `GET /sandboxes/{id}/desktop/status` | 查询桌面运行状态与各服务 URL | 同理提供 `/v1/sandboxes/{id}/desktop/status` 别名 |
| `POST /sandboxes/{id}/desktop/stop` | 停止桌面相关服务 | 同理提供 `/v1/sandboxes/{id}/desktop/stop` 别名 |
| `GET /sandboxes/{id}/endpoints/{port}` | 兼容 E2B `getHost` / `get_host` 的对外主机名 | 返回 `<port>-<sandboxID>.<domain>`。行为不变。 |
| `POST /sandboxes/{id}/endpoints/{port}` | 为沙箱端口签发 `/panel/{token}/` 签名代理令牌 | 请求体仅为 `{expiresInSeconds}`（默认 `3600`）。客户端提供的 `targetUrl` / `upstreamUrl` 会被忽略。*v0.7.0 变更。* 令牌格式：`base64url(sandboxID:port:expUnix).hmacSha256Sig` |
| `ANY /panel/{token}/...` | 校验 HMAC 后透明代理 HTTP、WebSocket 与 WebRTC 流量 | 剥离 `/panel/{token}` 前缀，并始终转发到该沙箱自己的 `PodIP:<port>`（仅在尚无沙箱主机时才使用进程内测试映射）。*v0.7.0 变更。* |

架构原理与安全细节请参阅[桌面环境与签名 Panel 代理](/zh/concepts/desktop-and-panel-proxy/)。

## E2B Connect-RPC 兼容接口

为实现与 E2B 官方 SDK 的无痛对接（`pkg/gateway/connectrpc.go`），Gateway 原生解析并路由以下 Connect-RPC 服务——无论请求直接发往根路径 `/`（由 `E2b-Sandbox-Id`、`X-Sandbox-ID`、查询参数或子域名解析沙箱 ID），还是嵌套在 `/sandboxes/{id}/` 下（详见 **[E2B 兼容性与差异参考](/zh/reference/e2b-compatibility/)** 了解完整方法矩阵）：

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

自 *v0.7.0* 起默认启用（可通过 `--e2b-template-api=false` 或 `--feature-gate=E2BTemplateAPI=false` 回滚），默认开放完整的模板目录与 BuildKit 构建端点：

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

## 密钥、持久化卷、事件与 Webhook

- **E2B 密钥（`E2BSecretsAPI`，*v0.6.0 变更*）**：`POST /secrets`、`GET /secrets`、`GET /secrets/{secretID}`、`POST /secrets/{secretID}`、`DELETE /secrets/{secretID}` — 租户隔离的 Write-Only 密钥存储（`KubeSecretBackend` 多副本 HA 或 AES-256-GCM 文件存储），配合 `${e2b.secrets.<name>}`（`Secret.fill()`）解析。
- **E2B 卷（Volumes，*v0.7.0 变更*）**：`POST /volumes`、`GET /volumes`、`GET /volumes/{volumeID}`、`DELETE /volumes/{volumeID}`、`POST /volumes/rotate-key`（*v0.7.0 新增。* 通过请求头 `X-API-Key: <newKey>` 与请求体 `{"previousApiKey": "<oldKey>"}` 将租户卷与挂载所有权原子迁移至新密钥并吊销旧密钥，持久化于带 RWX `flock` 锁保护的 `tenants.json`），内容传输 `/volumecontent/{volumeID}/{file,dir,path}`，以及 `POST /sandboxes` 的 `volumeMounts`（K8s Kata RWX 挂载由 `K8sE2BVolumeMounts` / Helm `e2bVolumes.rwxStorageClassName` 控制）。支持基于 `CUBEPOD_E2B_VOLUMES_KEK` 或外部 KMS `CUBEPOD_E2B_VOLUMES_KMS_URL`（可选 `CUBEPOD_E2B_VOLUMES_KMS_TOKEN`）的卷令牌信封加密、单租户字节与数量配额（`CUBEPOD_E2B_MAX_TENANT_BYTES`、`CUBEPOD_E2B_MAX_VOLUMES_PER_TENANT`）以及 `deploy/monitoring/e2b-volumes-alerts.yaml` 中的 `PrometheusRule` 容量告警（*v0.7.0 新增。*）。
- **生命周期事件与 Webhook（`SandboxEventsAndWebhooks`，*v0.6.0 新增*）**：`GET /events`、`GET /sandboxes/{sandboxID}/events`、`POST /webhooks`、`GET /webhooks`、`GET /webhooks/{webhookID}`、`PATCH /webhooks/{webhookID}`、`DELETE /webhooks/{webhookID}`。
  - *v0.7.0 变更。* 在 Kubernetes 上，默认的 `k8s` 事件后端（`CUBEPOD_EVENTS_BACKEND`）将 Webhook 存储为 `SandboxWebhook` CR（HMAC 密钥仅保存在配套 `Secret` 中），并将生命周期事件写为 `PodBox` 上的 core/v1 `Event`，所有 Gateway 副本共享这些数据。`GET /webhooks` 与 `GET /webhooks/{webhookID}` 新增可选字段 `lastDeliveryTime`（RFC 3339）与 `consecutiveFailures`（投递成功后清零）。删除 Webhook 时会同时删除其死信。
- **Webhook 死信（`SandboxEventsAndWebhooks`，*v0.7.0 新增*）**：超过最大重试次数仍失败（`reason: delivery_failed`）或因投递队列已满被丢弃（`reason: queue_full`）的投递会保存为租户隔离的死信（file 后端每租户最多 500 条，`k8s` 后端每个 Webhook 最多 50 条）。所有接口均需 `X-API-Key`，未配置事件功能时返回 `501`。

| 接口 | 说明 | 错误码 |
| :--- | :--- | :--- |
| `GET /webhooks/dead-letters` | 列出租户死信；查询参数 `webhookID`、`sandboxID`、`reason`、`limit`（≤ 100）、`nextToken`（下一页游标见 `X-Next-Token`） | `501` |
| `GET /webhooks/{webhookID}/dead-letters` | 列出单个 Webhook 的死信（过滤参数同上，不含 `webhookID`） | `404` Webhook 不属于当前租户、`501` |
| `POST /webhooks/{webhookID}/dead-letters/{deadLetterID}/retry` | 使用当前 `signatureKey` 重新签名并重新入队，返回 `202` + `DeadLetter`；重试失败时以相同 id 重新记录且 `retryCount + 1` | `404`、`409` Webhook 已禁用、`503` 队列已满、`501` |
| `DELETE /webhooks/{webhookID}/dead-letters/{deadLetterID}` | 丢弃死信（`204`） | `404`、`501` |

`DeadLetter` 包含 `id`（`dlq_...`）、`webhookId`、`eventId`、`eventType`、`sandboxId`、`reason`、`attempts`、可选 `lastStatusCode`、可选的脱敏 `lastError`（`timeout`、`connection_error`、`http_status`、`ssrf_blocked`）、可选 `retryCount`、`createdAt`、`updatedAt` 以及原始生命周期 `event`。死信永远不会包含 `signatureKey` 或 Webhook URL。

```bash
curl -s -H "X-API-Key: $CUBEPOD_API_KEY" \
  "$CUBEPOD_URL/webhooks/dead-letters?reason=delivery_failed&limit=20"
curl -s -X POST -H "X-API-Key: $CUBEPOD_API_KEY" \
  "$CUBEPOD_URL/webhooks/whk_123/dead-letters/dlq_abc/retry"
```

## 特性开关（`--feature-gate`） *(v0.6.0 新增)*

`cubepod-gateway` 与 `cubepod-controller` 均支持 `--feature-gate=GateA=true,GateB=false`：

| 特性开关名称 | 默认值 | 生效组件 | 说明 |
| :--- | :--- | :--- | :--- |
| `E2BTemplateAPI` | `true` *(v0.7.0 变更)* | `gateway`、`controller` | 启用 E2B JSON 数组格式 `GET /templates` 与 `/v2`–`/v3` 模板接口（可通过 `--e2b-template-api=false` 回滚至旧版包装响应）。 |
| `K8sE2BVolumeMounts` | `false` | `gateway` | 启用 Kubernetes RWX E2B `volumeMounts`（在 Helm 中设置 `e2bVolumes.rwxStorageClassName` 时会自动开启）。 |
| `TransparentEgress` | `true` | `gateway`、`controller` | 控制 `CUBEPOD_TRANSPARENT=1` + `NET_ADMIN` `nftables` 透明劫持及 `/internal/v1/egress/exchange`。 |
| `DesktopStack` | `true` | `gateway` | 控制 `/sandboxes/{id}/desktop/{start,status,stop}` 接口。 |
| `StatelessGatewayHA` | `true` | `gateway` | 控制多副本网关跨副本基于 `PodBox` CRD 的缓存未命中重建。 |
| `E2BFilesystemWatch` | `true` | `gateway` | 控制 Connect-RPC `WatchDir` 目录变更监听接口。 |
| `E2BSecretsAPI` | `true` | `gateway` | 控制 `/secrets` 增删改查与 `${e2b.secrets.<name>}` 解析。 |
| `SandboxEventsAndWebhooks` | `true` | `gateway` | 控制 `/events`、`/webhooks`、（*v0.7.0*）`/webhooks/dead-letters` 接口及异步 HMAC Webhook 投递。 |

## Prometheus 可观测性与 `podd` 指标 *(v0.6.0 新增)*

### Guest 内置 `podd` 指标接口（`:49983`）

- **`GET /metrics`**：采样 `/proc/stat`、`/proc/meminfo`、cgroup v2/v1 限制与 `statvfs("/workspace")`，返回 JSON 格式 `SandboxMetric`（`timestamp`、`timestampUnix`、`cpuCount`、`cpuUsedPct`、`memUsed`、`memTotal`、`memCache`、`diskUsed`、`diskTotal`）；传入 `?format=prometheus` 或 `Accept: text/plain` 时返回 Prometheus 文本格式。
- **`GET /v1/prometheus`**：导出 Prometheus `0.0.4` 指标：
  - `cubepod_podd_active_processes`
  - `cubepod_podd_init_duration_milliseconds`（读取 `/run/cubepod/init-state.json` 的 `duration_ms`）
  - `cubepod_podd_init_e2b_bind_duration_ms`（读取 `/run/cubepod/init-state.json` 的 `e2b_bind_duration_ms`）
  - `cubepod_podd_cpu_used_percent`、`cubepod_podd_memory_used_bytes`、`cubepod_podd_memory_total_bytes`、`cubepod_podd_disk_used_bytes`、`cubepod_podd_disk_total_bytes`
  - *v0.7.0 新增。* 文件监听指标：`cubepod_podd_watchers_active`（gauge）、`cubepod_podd_watch_events_emitted_total`（counter）、`cubepod_podd_watch_overflows_total{kind="inotify_queue|ring_buffer|subscriber_queue"}`（counter）。`GET /metrics?format=prometheus` 返回相同指标。

### Gateway（`:8080/metrics`）、Controller（`:8081/metrics`）与 Egress（`:19090/metrics`）指标清单

| 组件 | 指标名称 | 类型 | 说明与标签 |
| :--- | :--- | :--- | :--- |
| **Gateway** | `cubepod_gateway_http_requests_total` / `cubepod_gateway_requests_total` | Counter | `cubepod-gateway` 处理的 HTTP 请求总数 |
| **Gateway** | `cubepod_e2b_requests_total` | Counter | 归一化路径后的 E2B API 请求数（`endpoint`、`status`） |
| **Gateway** | `cubepod_e2b_request_duration_seconds` | Histogram | E2B API 请求耗时直方图 |
| **Gateway** | `cubepod_gateway_sandboxes_created_total` | Counter | 累计创建的沙箱数 |
| **Gateway** | `cubepod_gateway_autoresume_total` / `cubepod_gateway_auto_resumes_total` | Counter | 累计透明自动唤醒次数 |
| **Gateway** | `cubepod_gateway_commands_executed_total` | Counter | 沙箱内累计执行命令数 |
| **Gateway** | `cubepod_gateway_sandboxes_active` | Gauge | 当前活跃沙箱数（`state`：`Running`、`Paused`） |
| **Gateway** | `cubepod_gateway_warmpools_total` | Gauge | 已配置的 WarmPool 数量 |
| **Gateway** | `cubepod_gateway_crd_resyncs_total` | Counter | 网关执行 `PodBox`/`PodWarmPool` CRD 重同步次数 |
| **Gateway** | `cubepod_sandbox_metrics_samples_total` | Counter | 后台沙箱指标采样器结果（`result="ok|error"`）。*v0.7.0 新增。* |
| **Gateway** | `cubepod_sandbox_cpu_count` / `cubepod_sandbox_cpu_used_pct` / `cubepod_sandbox_memory_used_bytes` / `cubepod_sandbox_memory_total_bytes` / `cubepod_sandbox_disk_used_bytes` / `cubepod_sandbox_disk_total_bytes` | Gauge | 运行中沙箱最近一次采样的资源使用情况（带 `sandbox_id` 标签）。*v0.7.0 新增。* |
| **Gateway** | `cubepod_panel_active_sessions` | Gauge | 当前活跃 `/panel/{token}/` HTTP/WebSocket 反代会话数 |
| **Gateway** | `cubepod_desktop_activations_total` | Counter | 桌面启动次数（`result`：`ok`、`unsupported`、`error`） |
| **Gateway** | `cubepod_secrets_operations_total` | Counter | 密钥存储操作数（`op`、`status`） |
| **Gateway** | `cubepod_secrets_store_count` | Gauge | 全租户已存储的 E2B 密钥总数 |
| **Gateway** | `cubepod_secrets_operation_duration_seconds` *(v0.7.0 新增)* | Histogram | 按 `operation`（`create`、`list`、`get`、`update`、`delete`、`resolve`）统计的密钥存储耗时；桶 0.005s–10s |
| **Gateway** | `cubepod_watch_streams_active` / `cubepod_watchers_active` *(v0.7.0 新增)* | Gauge | 活跃 `WatchDir` 流数与活跃轮询监听器（`CreateWatcher`）数 |
| **Gateway** | `cubepod_watch_events_total` / `cubepod_watch_overflows_total` *(v0.7.0 新增)* | Counter | 已发出的监听事件数与 `resource_exhausted` 监听溢出次数 |
| **Gateway** | `cubepod_e2b_volume_mounts_total` / `cubepod_e2b_volume_mount_errors_total` | Counter | E2B 卷挂载次数与挂载规划/执行错误数 |
| **Gateway** | `cubepod_e2b_volume_quota_rejections_total` *(v0.7.0 新增。)* | Counter | 因超出单租户卷数量或字节配额上限而被拒绝（`429`）的 E2B 卷创建或文件上传请求数 |
| **Gateway** | `cubepod_e2b_volumes_storage_capacity_bytes` / `cubepod_e2b_volumes_storage_used_bytes` / `cubepod_e2b_volumes_storage_available_bytes` *(v0.7.0 新增。)* | Gauge | E2B 卷存储目录的文件系统总容量、已用字节与可用字节（`statfs`） |
| **Gateway** | `cubepod_e2b_volumes_total` *(v0.7.0 新增。)* | Gauge | 全租户当前存储的活跃 E2B 卷总数 |
| **Gateway** | `cubepod_events_recorded_total` | Counter | 已记录的沙箱生命周期事件总数（启用事件管理器时导出） |
| **Gateway** | `cubepod_webhook_deliveries_total` / `cubepod_webhook_queue_depth` | Counter / Gauge | Webhook 投递结果（`result`：`ok`、`failed`、`dropped`）与队列深度 |
| **Gateway** | `cubepod_webhook_dead_letters_total` / `cubepod_webhook_dead_letters_pending` | Counter / Gauge | *v0.7.0 新增。* 已记录与当前待处理的死信数量（告警见 `deploy/monitoring/webhook-alerts.yaml`） |
| **Gateway & Controller** | `cubepod_sandbox_forks_total` / `cubepod_sandbox_fork_duration_seconds` | Counter / Histogram | CoW Fork 次数（`mode`、`result`）与耗时分布 |
| **Gateway & Controller** | `cubepod_templates` / `cubepod_gateway_templates_total` | Gauge | 按 `status` 统计的模板数（开启 `E2BTemplateAPI` 时）或模板总数 |
| **Controller** | `cubepod_active_sandboxes` / `cubepod_paused_sandboxes` | Gauge | 控制器调谐的 `Running` 与 `Paused` `PodBox` 数量 |
| **Controller** | `cubepod_warmpool_ready` / `cubepod_warmpool_available_replicas` | Gauge | WarmPool 就绪及未被认领的待命副本数 |
| **Controller** | `cubepod_template_builds_total` / `cubepod_template_build_duration_seconds` | Counter / Histogram | 模板构建完成数（`result`）与构建耗时 |
| **Egress** | `cubepod_egress_requests_total` | Counter | 出站请求计数（`mode`、`decision`） |
| **Egress** | `cubepod_egress_secret_exchanges_total` | Counter | 单沙箱 mTLS `cpodref_` 交换次数（`result`） |
| **Egress** | `cubepod_egress_blocked_hosts_total` | Counter | 被策略拒绝的出站目标域名计数（`host`） |

### 告警规则 *(v0.7.0 新增)*

[`deploy/monitoring/watcher-secret-alerts.yaml`](https://github.com/cubeplexai/cubepod/blob/main/deploy/monitoring/watcher-secret-alerts.yaml) 是 Prometheus Operator `PrometheusRule`，包含 `CubePodSecretStoreHighErrorRate`、`CubePodSecretStoreHighLatency`（p99 > 0.5s）、`CubePodSecretExchangeErrors` 与 `CubePodWatchOverflowDetected`。

## 内部零信任出站凭证交换

| 方法与路径 | 用途 |
| :--- | :--- |
| `POST /internal/v1/egress/exchange` | 由 `cubepod-egress` 边车代理通过严格单沙箱 mTLS（独立 `--egress-mtls-addr` 或主监听器）发起调用，在放行出站请求前将 `cpodref_` 占位符实时置换为真实上游密钥 |

## 下一步

- 参阅 **[E2B 兼容性与差异参考](/zh/reference/e2b-compatibility/)** 了解官方 SDK 方法矩阵、实测版本约束与架构差异。
- 前往[交互式 API 参考](/zh/api/)在线调试请求。
- 查看各语言客户端实现：[Python SDK](/zh/sdks/python/)、[TypeScript SDK](/zh/sdks/typescript/) 与 [Go SDK 与 CLI](/zh/sdks/go-and-cli/)。
