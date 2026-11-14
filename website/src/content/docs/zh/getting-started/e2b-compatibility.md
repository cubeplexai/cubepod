---
title: E2B 无缝兼容指南
description: 零改代码将官方 E2B Python / TypeScript SDK（e2b、e2b-code-interpreter、@e2b/code-interpreter）及 AI Agent 框架接入 CubePod。
---

依据 **[CEP-0018](/zh/ceps/)** 与 **[CEP-0019](/zh/ceps/)** 设计规范，CubePod 为官方 E2B SDK（`e2b`、`e2b-code-interpreter`、`@e2b/code-interpreter`）提供**协议级无缝兼容（Wire-Level Drop-in Compatibility）**。现有 LangChain、LlamaIndex、AutoGen、CrewAI、OpenHands 与 Smolagents 工作流无需改动业务代码，只需配置环境变量即可从托管 E2B 切换至自建持久化 Kata MicroVM 平台。

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

## 零改代码的官方 E2B 示例

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

`podd` 内置支持 **6 种语言**（`python`、`bash`、`javascript`、`typescript`、`r`、`java`）的有状态执行上下文，通过 NDJSON 流式协议与 `e2b-code-interpreter` 完整互通：

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create() as sbx:
    sbx.run_code("x = 21 * 2")
    execution = sbx.run_code("print(f'result={x}')")
    print(execution.logs.stdout)
```

## `cubepod-gateway` 协议兼容矩阵

`cubepod-gateway`（`pkg/gateway/`）同时实现了 E2B 控制面与数据面（含基于 `:49983` 语义的 Connect-RPC 协议）：

| 协议面 | 已实现端点 | CubePod 内部行为 |
| :--- | :--- | :--- |
| **控制面** | `POST /sandboxes`、`POST /v2/sandboxes`、`GET /sandboxes`、`GET /v2/sandboxes`、`GET /sandboxes/{id}`、`DELETE /sandboxes/{id}` | 创建或列举 `PodBox` 沙箱；响应体始终包含 E2B 必填字段（`sandboxID`、`templateID`、`clientID`、`envdVersion`、`state`、`startedAt`、`endAt`、`cpuCount`、`memoryMB`、`diskSizeMB`）。 |
| **生命周期与租约** | `POST /sandboxes/{id}/refreshes`、`POST /sandboxes/{id}/timeout`、`POST /sandboxes/{id}/pause`、`POST /sandboxes/{id}/connect` | 续期超时或租约时间；暂停时释放 Pod 算力并保留双 PVC；收到 `connect` 或数据面流量时透明自动唤醒。 |
| **命令执行与流式日志** | `POST /sandboxes/{id}/commands`、`POST /sandboxes/{id}/commands/run`、`GET .../logs?cursor=N`、`POST .../kill`、Connect-RPC `/process.Process/{Start,SendSignal,Connect,List}` | 在 Guest 内默认以非特权 UID/GID `1000:1000`（`cubeplex`）运行命令，支持游标增量拉取日志及 PGID 进程组级清理。 |
| **文件系统与归档同步** | `PUT/GET/DELETE /sandboxes/{id}/files`、`GET .../files/list`、`POST .../files/archive-sync`、Connect-RPC `/filesystem.Filesystem/{Read,Write,List,ListDir,Stat,Remove,MakeDir}` | 直接读写持久化 `/workspace` 或 RootFS 文件，并支持带路径穿越防护的 `.tar`/`.tar.gz` 批量目录同步。 |
| **有状态代码解释器** | `POST /sandboxes/{id}/execute`、`POST /sandboxes/{id}/code/execute`、`/contexts` | 管理 `podd` 内部 6 种语言解释器上下文（`python`、`bash`、`javascript`、`typescript`、`r`、`java`）。 |
| **子域名端口路由** | `Host: <port>-<sandboxID>.<domain>` 与 `GET/POST /sandboxes/{id}/endpoints/{port}` | 自动唤醒已暂停沙箱，将 `/envd/*` 路径转发至 `podd`（`:49983`），并反代应用端口或签发 `/panel/{token}/` 访问地址。 |
| **模板目录与构建** | `/templates`、`/v2/templates`、`/v3/templates`、`/templates/{id}/builds/{buildID}/status`、`/templates/{id}/files/{hash}` | 启用 `--e2b-template-api=true`（或 `-feature-gate E2BTemplateAPI=true`）后即可兼容 `e2b template build` 等工作流。 |

## 超越 E2B 的 CubePod 扩展能力

使用 CubePod 原生 SDK（[Python](/zh/sdks/python/)、[TypeScript](/zh/sdks/typescript/)、[Go 与 CLI](/zh/sdks/go-and-cli/)）或 REST 超集接口时，还可直接调用标准 E2B 不具备的高级特性：

- **持久化可写 RootFS 与无损系统重置**：调用 `POST /sandboxes/{id}/reset-rootfs` 递增 `spec.rootfsGeneration`，分配干净的新 `upperdir` PVC 并完好保留 `/workspace`。
- **1 秒级 CoW Fork 分支克隆**：调用 `POST /sandboxes/{id}/fork`，通过 `FIFREEZE` 静默 -> CSI/reflink 克隆 -> `FITHAW` 瞬间克隆运行中沙箱的 OS 与工作区状态。
- **零信任 `cpodref_` 出站保险库**：在创建沙箱时注入 `secretBindings`，确保 Agent 仅见占位符而无法读取真实 API 密钥。
- **按需桌面与签名面板**：调用 `/sandboxes/{id}/desktop/{start,status,stop}` 懒启动 `Xvfb`、Chromium CDP、`noVNC` 与 `ttyd`，通过 `/panel/{token}/` 安全访问。

## 下一步

- 查看 **[API 参考](/zh/api/)** 浏览完整 OpenAPI 3.1 契约。
- 对比原生客户端特性：**[Python SDK](/zh/sdks/python/)** 与 **[TypeScript SDK](/zh/sdks/typescript/)**。
- 查看 **[CRD 参考](/zh/reference/crds/)** 了解 `PodBox` 与 `PodTemplate` 的 Kubernetes Schema。
