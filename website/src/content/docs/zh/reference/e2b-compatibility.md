---
title: E2B 兼容性与差异参考
description: 官方 E2B SDK 与 CubePod Gateway 的兼容性矩阵与行为差异说明，范围限于本页已核对的接口，并非对上游 E2B API 的完整清单审计。
---

本参考页归纳官方 E2B SDK 与 CubePod Gateway（`cubepod-gateway`）在下列已核对接口上的兼容性与行为差异，并不是对上游 E2B API 或 SDK 表面的完整清单审计。

CubePod 实现了核心 E2B REST 与 Connect-RPC 端点，允许使用官方 E2B 客户端 SDK 的团队将流量重定向至私有化部署的 CubePod 实例。然而，CubePod **并非**托管版 E2B Cloud 的逐行克隆：它构建在基于 Kata Containers 微虚拟机的双块卷持久化架构之上，具备不同的生命周期语义、双卷存储模型与面向企业部署的扩展接口。

:::note[测试验证状态与证据说明]
- **集成测试套件**：仓库在 `test/e2b-compat/` 下包含自动化 Python 兼容性测试套件（`test_official_e2b_sync.py`、`test_official_e2b_async.py`、`test_official_code_interpreter.py`、`test_official_e2b_templates.py`、`test_streaming_realtime.py`、`test_timeout_and_zombie_kill.py`、`test_multimodal_data_science.py`、`test_concurrent_sandboxes_isolation.py`）。
- **测试执行状态**：仓库中存在的测试文件代表为 CEP-0018 与 CEP-0019 编写的测试覆盖范围；**它们并不代表在本次文档编写过程中执行了端到端运行验证**。维护者可在本地或测试环境通过 `make test-e2b-compat`（`bash test/e2b-compat/run_tests.sh`）对实时网关运行验证。
- **CI 流水线范围**：在 `.github/workflows/ci.yml` 中，`make test-e2b-compat` 当前**未**接入默认的 GitHub Actions 自动化流水线（默认流水线运行单元测试、SDK 单元测试以及多进程 `make test-e2e`）。
- **官方 JS/TS SDK**：仓库中**不存在**针对官方 `@e2b/code-interpreter` 或 `e2b` npm 包的自动化测试。因此，官方 TypeScript/JavaScript E2B SDK 兼容性标记为**未测试（Untested）**。
- **Agent 框架**：上层编排框架（LangChain、LlamaIndex、AutoGen、CrewAI、OpenHands、Smolagents）在此仓库中没有专门的集成测试用例，标记为**未测试（Untested）**。
:::

---

## 架构分层定义

在评估兼容性时，必须严格区分三个不同的技术层面，绝不能将它们混为一谈：

```
+---------------------------------------------------------------------------------------+
| 第 1 层：官方 E2B SDK 接入层（Official E2B SDK Drop-in）                              |
| 未经修改的上游官方客户端包（'e2b'、'e2b-code-interpreter'、'@e2b/code-interpreter'）   |
| 通过 E2B_DOMAIN 或 E2B_API_URL + E2B_SANDBOX_URL + E2B_API_KEY 指向 CubePod             |
+---------------------------------------------------------------------------------------+
                                           |
                                           v
+---------------------------------------------------------------------------------------+
| 第 3 层：网关协议与线级转译层（Protocol & Gateway Wire Translation）                   |
| 'cubepod-gateway'（:8080）REST 路由与 Connect-RPC 处理器                              |
| 转译 /sandboxes、/process.Process/*、/filesystem.Filesystem/*、/templates 等协议       |
+---------------------------------------------------------------------------------------+
                                           ^
                                           |
+---------------------------------------------------------------------------------------+
| 第 2 层：CubePod 原生 SDK 层（功能类似超集客户端）                                    |
| 命名为 'cubepod' 的原生 SDK（'sdks/python/'、'sdks/typescript/'、'pkg/client'）        |
| 绝非 'from e2b import Sandbox' 的代码别名；提供 fork、reset-rootfs、面板与凭据隔离超集 |
+---------------------------------------------------------------------------------------+
```

1. **第 1 层：官方 E2B SDK 接入层（Official E2B SDK Drop-in）**：
   专指直接使用上游官方 Python 包（`e2b`、`e2b-code-interpreter`）或 npm 包（`@e2b/code-interpreter`、`e2b`），通过配置环境变量将请求目标指向 CubePod。这是**唯一**可被称为“官方 SDK 兼容性”的技术层面。
2. **第 2 层：CubePod 原生 SDK 层（功能重叠与超集客户端）**：
   `cubepod` 官方客户端库（`sdks/python/` 与 `sdks/typescript/`，以及位于 `pkg/client` 的 Go 客户端与 `cmd/cubepod` CLI）提供了现代化的沙箱操作句柄（`.commands`、`.files`、`.code`）。但是，它们是**原生客户端**，并非 `from e2b import Sandbox` 的平替别名。它们直接调用 CubePod 原生 `/sandboxes/{id}/*` 接口，并开放 CubePod 专属的企业级特性（`fork`、`reset-rootfs`、`archive-sync`、`secretBindings`、桌面与面板代理）。
3. **第 3 层：网关协议与路由实现层（Gateway Protocol Route Presence）**：
   在 `cubepod-gateway`（`pkg/gateway/server.go` 与 `pkg/gateway/connectrpc.go`）中注册了 HTTP REST 或 Connect-RPC 路由，说明代码中存在对应的协议转译逻辑。然而，单纯存在路由并**不**意味着上游 SDK 的所有版本和边界方法都已在生产环境中完成无缝互通。

---

## 将官方 E2B SDK 指向 CubePod

官方 E2B SDK 可以通过标准环境变量重定向至 `cubepod-gateway`：

### 生产环境 HTTPS + 通配符 DNS Ingress

当 `cubepod-gateway` 部署在具备通配符 DNS（`*.<E2B_DOMAIN>`）的 Ingress Controller 后方时：

```bash
export E2B_DOMAIN="cubepod.example.com"
export E2B_API_KEY="cpod_live_secret"
```

上游 E2B SDK 会自动将控制面请求发往 `api.<E2B_DOMAIN>`（或根域名），并将沙箱端口访问路由至 `<port>-<sandboxID>.<E2B_DOMAIN>`。`cubepod-gateway` 通过匹配 `Host` 请求头的 `^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$` 自动将流量精准反向代理至沙箱微虚拟机。

### 本地直连 HTTP / 直连网关

在本地开发调试且未配置 TLS 与通配符 DNS 时（如连接 `http://127.0.0.1:8080`）：

```bash
export E2B_API_URL="http://127.0.0.1:8080"
export E2B_SANDBOX_URL="http://127.0.0.1:8080"
export E2B_API_KEY="cpod_test_api_key"
```

同时设置 `E2B_API_URL` 与 `E2B_SANDBOX_URL` 会强制 SDK 将控制面与数据面流量均直接发往指定的 Gateway 地址。这也是仓库 `test/e2b-compat/` 自动化测试所采用的配置。

---

## 接口兼容性矩阵

### 状态定义

- **兼容（Compatible）**：`test/e2b-compat/` 中对应的官方 Python SDK 测试用例已覆盖下列基本场景。这不表示所有参数或边界均已实现或测过，也不表示编写本页时重新跑过这些测试。
- **部分兼容（Partial）**：已有对应处理器或协议路径，但存在已知缺漏、语义差异或需要打开特性开关。
- **未测试（Untested）**：可能存在代码路径或路由，或官方 SDK 助手并不依赖 CubePod 专用路由，但本仓库没有覆盖该官方 SDK 方法的自动化测试。
- **未支持（Unsupported）**：未找到对应的 CubePod 处理器、路由或等价协议支持，且该上游能力不仅仅是对另一项已覆盖 API 的 SDK 包装。

表格中的 `e2b>=2.0.0` 一类写法是 `test/e2b-compat/run_tests.sh` 声明的依赖下限（模板套件还会断言已安装包的主版本 `>= 2`），不是已测版本的全集，也不是本次文档更新时的实测结果。

---

### 1. 沙箱生命周期（Sandbox Lifecycle）

| 官方 E2B 接口 | CubePod 网关路由 / 协议 | 状态 | 代码证据 / 实现指针 | 仓库测试声明 / 版本约束 | 行为差异与限制说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `Sandbox.create(...)` [Python] | `POST /sandboxes`<br/>`POST /v2/sandboxes` | **兼容** | `pkg/gateway/sandboxes.go:handleCreateSandbox`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`test/e2b-compat/test_concurrent_sandboxes_isolation.py` | `e2b>=2.0.0`（依赖下限，非已测版本全集） | 创建响应会填充所列 E2B 必填字段（`sandboxID`、`templateID`、`clientID`、`envdVersion`、`state`、`startedAt`、`endAt`、`cpuCount`、`memoryMB`、`diskSizeMB`）。若存在可用的 `PodWarmPool` 备用实例，网关可以认领预热 Pod；认领耗时取决于集群与存储，本页不给出测量过的时延保证。支持通过扩展字段传递 CubePod 超集参数。 |
| `Sandbox.create(...)` [JS/TS] | `POST /sandboxes`<br/>`POST /v2/sandboxes` | **未测试** | `pkg/gateway/sandboxes.go:handleCreateSandbox` | 仓库内无用例 | 仓库内无针对官方 `@e2b/code-interpreter` 或 `e2b` npm 包的自动化测试。 |
| `Sandbox.get_info(...)` | `GET /sandboxes/{id}` | **部分兼容** | `pkg/gateway/sandboxes.go:handleGetSandbox`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | **对用户的影响：** 返回首字母大写的 `"Running"` / `"Paused"`，而非小写 `"running"` / `"paused"`，导致官方 `e2b >= 2.0.0` Python SDK 调用 `Sandbox.get_info()` 时抛出 `ValueError`；可改用 `Sandbox.list()` 获取沙箱状态。 |
| `Sandbox.list()` | `GET /v2/sandboxes`<br/>`GET /sandboxes` | **兼容** | `pkg/gateway/sandboxes.go:handleListSandboxesV2`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | `/v2/sandboxes` 返回小写状态，兼容官方 `Sandbox.list()` 分页器。 |
| `sandbox.kill()` / `delete` | `DELETE /sandboxes/{id}` | **兼容** | `pkg/gateway/sandboxes.go:handleDeleteSandbox`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py` | `e2b>=2.0.0`（依赖下限，非已测版本全集） | 释放计算 Pod。临时沙箱在销毁时自动回收临时存储；命名持久化工作区默认保留，除非显式指定删除。 |
| `sandbox.set_timeout(...)` / 租约续期 | `POST /sandboxes/{id}/refreshes`<br/>`POST /sandboxes/{id}/timeout` | **兼容** | `pkg/gateway/sandboxes.go:handleRefreshSandbox`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`pkg/gateway/e2b_conformance_test.go` | `e2b>=2.0.0`（依赖下限，非已测版本全集） | 续期沙箱租约并重置后台空闲回收计时。 |
| `Sandbox.connect(...)` | `POST /v2/sandboxes/{id}/connect`<br/>`POST /sandboxes/{id}/connect` | **兼容** | `pkg/gateway/sandboxes.go:handleConnectSandbox`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 重新连接运行中沙箱，或自动唤醒已暂停沙箱（但不保留暂停前的内存进程状态）。 |

---

### 2. 命令与进程执行（Commands & Process Execution）

| 官方 E2B 接口 | CubePod 网关路由 / 协议 | 状态 | 代码证据 / 实现指针 | 仓库测试声明 / 版本约束 | 行为差异与限制说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.commands.run(...)` (同步) | `POST /sandboxes/{id}/commands`<br/>`POST /sandboxes/{id}/commands/run`<br/>Connect-RPC `/process.Process/Start` | **兼容** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py` | `e2b>=2.0.0`（依赖下限，非已测版本全集） | 在 Guest 内通过 `podd` 以非特权 UID/GID `1000:1000`（`cubeplex`）运行，工作目录默认为 `/workspace`。返回 `stdout`、`stderr` 与 `exitCode`。仅前台同步执行。 |
| `sandbox.commands.run(..., background=True)` | `POST /sandboxes/{id}/commands`（`background: true`）<br/>Connect-RPC `/process.Process/Start` | **兼容** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 在 Guest 内创建后台进程组并返回含 `pid` 的句柄。 |
| 日志流式推送与回调（`on_stdout`、`on_stderr`） | `GET /sandboxes/{id}/commands/{cmdId}/logs`<br/>`GET /sandboxes/{id}/commands/{cmdId}/stream`<br/>Connect-RPC `/process.Process/Connect` | **兼容** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 实时将命令输出增量推送给 `on_stdout` / `on_stderr` 回调。 |
| `sandbox.commands.kill(pid)` | `POST /sandboxes/{id}/commands/{cmdId}/kill`<br/>Connect-RPC `/process.Process/SendSignal` | **兼容** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 终止后台进程组并在停止后返回 `True`。 |
| `sandbox.pty` / 交互式伪终端 | Connect-RPC `/process.Process/{Start,Connect,SendInput,StreamInput,Update,SendSignal}`（`pty: true`） | **部分兼容** | `pkg/gateway/connectrpc.go`<br/>`crates/podd/src/commands.rs`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | **对用户的影响：** 交互式 PTY 创建、标准输入、窗口大小调整（resize）、信号发送与 64 KiB 重连回放均可正常使用，但不支持半双工关闭标准输入（`CloseStdin` 返回 `501 Not Implemented`）。 |

---

### 3. 文件系统操作（Filesystem Operations）

| 官方 E2B 接口 | CubePod 网关路由 / 协议 | 状态 | 代码证据 / 实现指针 | 仓库测试声明 / 版本约束 | 行为差异与限制说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.files.write(path, data)` | `PUT /sandboxes/{id}/files`<br/>`PUT /files`<br/>Connect-RPC `/filesystem.Filesystem/Write` | **兼容** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`test/e2b-compat/test_concurrent_sandboxes_isolation.py` | `e2b>=2.0.0`（依赖下限，非已测版本全集） | 写入文件时可创建父目录。`pkg/gateway/files.go` 中的路径检查会尝试拒绝包含 `..` 的路径段；`test/e2b-compat` 覆盖的是向 `/workspace` 写入文本和二进制内容，并未枚举路径穿越用例。上传文件默认所有者为 `1000:1000`。 |
| `sandbox.files.read(path)` | `GET /sandboxes/{id}/files`<br/>`GET /files`<br/>Connect-RPC `/filesystem.Filesystem/Read` | **兼容** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`test/e2b-compat/test_concurrent_sandboxes_isolation.py` | `e2b>=2.0.0`（依赖下限，非已测版本全集） | 从沙箱持久化 `/workspace` 或 RootFS 读取原始文件字节流（支持文本及 `format="bytes"` 二进制读取）。 |
| `sandbox.files.list(path)` | `GET /sandboxes/{id}/files/list`<br/>`GET /files/list`<br/>Connect-RPC `/filesystem.Filesystem/{List,ListDir}` | **兼容** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 返回目录条目列表及元数据（`name`、`size`、`isDir`、`permissions`）。 |
| `sandbox.files.remove(path)` | `DELETE /sandboxes/{id}/files`<br/>`DELETE /files`<br/>Connect-RPC `/filesystem.Filesystem/Remove` | **兼容** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 递归删除沙箱内的目标文件或目录。 |
| `sandbox.files.make_dir(path)` | Connect-RPC `/filesystem.Filesystem/MakeDir` | **兼容** | `pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 递归创建目录（权限 `0755`，默认归属 `1000:1000`）。 |
| `sandbox.files.watch_dir(path)` | Connect-RPC `/filesystem.Filesystem/{WatchDir,CreateWatcher,GetWatcherEvents,RemoveWatcher}` | **兼容** | `pkg/gateway/watch.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 实时监听目录创建/写入/删除事件并支持流式与轮询拉取。 |

---

### 4. 代码解释器（Code Interpreter, `e2b-code-interpreter`）

| 官方 E2B 接口 | CubePod 网关路由 / 协议 | 状态 | 代码证据 / 实现指针 | 仓库测试声明 / 版本约束 | 行为差异与限制说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.run_code(code)` [Python REPL] | `POST /sandboxes/{id}/code/execute`<br/>`POST /sandboxes/{id}/execute`<br/>`POST /execute` | **兼容** | `pkg/gateway/server.go:handleCodeExecute`<br/>`crates/podd/src/interpreter.rs`<br/>`test/e2b-compat/test_official_code_interpreter.py`<br/>`test/e2b-compat/test_multimodal_data_science.py`<br/>`test/e2b-compat/test_streaming_realtime.py`<br/>`test/e2b-compat/test_timeout_and_zombie_kill.py` | `e2b-code-interpreter>=2.0.0`（依赖下限，非已测版本全集） | 在 `podd` 内部执行 Python 代码，支持跨调用内存状态保持。流式输出 NDJSON 事件，包含 `stdout`、`stderr`、富文本/多模态结果（`text`、Base64 `png` 图表、`html`、`json`）及运行时错误调用栈。实时 `on_stdout` 分块流式回调由 `test_streaming_realtime.py` 验证，多模态输出由 `test_multimodal_data_science.py` 验证，超时强制终止由 `test_timeout_and_zombie_kill.py` 验证。 |
| 多语言执行内核（`bash`、`js`、`ts`、`r`、`java`） | `POST /sandboxes/{id}/code/execute` | **部分兼容** | `crates/podd/src/interpreter.rs`<br/>`crates/podd/src/tests.rs` | `crates/podd` 单元测试 | 沙箱内部 `podd` 守护进程原生支持 6 种有状态 REPL 内核。官方 Python `e2b-code-interpreter` SDK 测试主要覆盖 Python 环境。 |
| 上下文管理接口（`/contexts`） | Guest 内部 `podd`（`:49983`） | **部分兼容** | `crates/podd/src/main.rs:147`<br/>`crates/podd/src/tests.rs` | `crates/podd` 单元测试 | Guest 内部 `podd` 支持 `POST /contexts`、`GET /contexts`、`DELETE /contexts/{id}` 与 `POST /contexts/{id}/restart`。**协议差异**：Gateway 未将 `/sandboxes/{id}/code/contexts` 暴露为顶层 REST 路由。客户端在 `POST /sandboxes/{id}/code/execute` 的请求体中通过 `context_id` 指定目标上下文，或直接向 `podd` 端口请求。 |

:::note[代码解释器流式与超时测试归属说明]
`test/e2b-compat/test_streaming_realtime.py` 专门验证 `e2b_code_interpreter.Sandbox.run_code(..., on_stdout=...)` 的实时 stdout 分块推送机制。类似地，`test/e2b-compat/test_timeout_and_zombie_kill.py` 验证的是代码解释器的执行超时控制（`run_code(..., timeout=2)`）及后续 Guest 进程清理。这两个测试覆盖的是 REPL 执行层，而非底层 `sandbox.commands` 后台进程管理。
:::

---

### 5. 模板目录与自定义构建（`E2BTemplateAPI`）

| 官方 E2B 接口 | CubePod 网关路由 / 协议 | 状态 | 代码证据 / 实现指针 | 仓库测试声明 / 版本约束 | 行为差异与限制说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `Template.list()` | `GET /templates`<br/>`GET /v2/templates` | **部分兼容** | `pkg/gateway/templates.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | **对用户的影响：** 需要网关开启模板 API 开关，否则 `GET /templates` 返回旧版非标准格式。 |
| `Template.build(...)` | `POST /templates`<br/>`POST /v2/templates`<br/>`POST /v3/templates`<br/>`POST /v2/templates/{id}/builds/{buildID}` | **部分兼容** | `pkg/gateway/templates.go`<br/>`pkg/controller/builder.go`<br/>`test/e2b-compat/test_official_e2b_templates.py` | `e2b>=2.0.0` | **对用户的影响：** 需要开启模板构建开关（否则返回 `501`）；构建产物不包含虚拟机内存快照（每次创建沙箱都会重新执行 `startCmd`），且任何针对 `/workspace` 的 `COPY` 或 `WORKDIR` 指令会被拒绝并返回 HTTP `400`。 |
| 构建状态与日志查询 | `GET /templates/{id}/builds/{buildID}/status`<br/>`GET /templates/{id}/builds/{buildID}/logs` | **兼容** | `pkg/gateway/templates.go`<br/>`test/e2b-compat/test_official_e2b_templates.py` | `e2b>=2.0.0` | 返回构建状态（`building`、`ready`、`error`）与构建日志。 |
| `Template.exists(alias)` / 模板别名与标签管理 | `GET /templates/aliases/{alias}`<br/>`POST /templates/tags`<br/>`DELETE /templates/tags` | **部分兼容** | `pkg/gateway/templates.go`<br/>`test/e2b-compat/test_official_e2b_templates.py` | `e2b>=2.0.0` | **对用户的影响：** 开启模板 API 后支持别名解析与 `Template.exists(alias)`；标签增删接口尚未纳入官方 SDK 兼容测试。 |

---

### 6. 端口访问与子域名反向代理

| 官方 E2B 接口 | CubePod 网关路由 / 协议 | 状态 | 代码证据 / 实现指针 | 仓库测试声明 / 版本约束 | 行为差异与限制说明 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.get_host(port)` | `GET /sandboxes/{id}/endpoints/{port}` | **兼容** | `pkg/gateway/server.go:handleGetEndpoint`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | 返回 `<port>-<sandboxID>.<domain>` 用于访问沙箱内暴露端口。 |
| 子域名 Ingress 路由 | `Host: <port>-<sandboxID>.<domain>` | **兼容** | `pkg/gateway/server.go:hostRouter`<br/>`pkg/gateway/server_test.go` | Go 单元/集成测试 | 根据 `<port>-<sandboxID>.<domain>` 转发流量至对应沙箱端口，并在收到流量时自动唤醒已暂停沙箱。 |

---

### 7. 未测试或部分兼容生态边界

| 上游 E2B 生态组件 | 状态 | 对用户的影响与说明 |
| :--- | :--- | :--- |
| **官方 JS/TS SDK**（`@e2b/code-interpreter`、`e2b`） | **未测试** | 仓库内尚未运行官方 JS/TS npm 客户端的自动化兼容测试。 |
| **Agent 编排框架**（LangChain、LlamaIndex、AutoGen、CrewAI、OpenHands、Smolagents） | **未测试** | 仓库内尚未集成针对各第三方框架的端到端测试套件。 |
| **官方 E2B Volumes API**（`POST /volumes`、`/volumecontent/{id}/*`、`volumeMounts`） | **部分兼容** | **对用户的影响：** 卷管理与文件读写在 `e2b>=2.0.0` 下可用，但调用 `vol.write_file()` / `vol.read_file()` 连接自建网关时必须显式传入 `api_url`（否则官方 SDK 默认请求 `e2b.app`）。在 Kubernetes 集群上挂载卷到沙箱需要配置集群级 RWX 共享存储，否则创建带 `volumeMounts` 的沙箱会返回 `501 Not Implemented`。 |
| **官方 E2B Secrets API**（`POST /secrets`、`GET /secrets`、`POST /secrets/{id}`、`DELETE /secrets/{id}`、`network.rules`） | **部分兼容** | **对用户的影响：** 单实例网关支持 Write-Only 密钥增删改查（`Secret.create`/`list`/`get_info`/`update`/`destroy`/`fill`）与 `network.rules` 请求头注入；暂不支持多副本 Kubernetes Gateway 之间的密钥状态同步，且本地 Docker 模式下不运行出站代理，无法对容器出站流量执行密钥头注入。 |
| **官方 E2B `sandbox.git` 助手** | **未测试** | 上游文档将 `sandbox.git` 说明为已弃用的 SDK 助手，它绑定在命令执行器上（`Git` 由 `Commands` 对象构造），并不是专用 HTTP Git 接口。`test/e2b-compat` 没有调用 `sandbox.git`。通过 `sandbox.commands.run()` 执行 `git` 只是普通 shell 命令路径，不能证明官方 git helper 兼容（例如 clone 选项、凭据助手、`dangerously_authenticate`、结构化 status/branch API 等）。 |

---

## 核心架构与行为语义差异

### 1. 暂停与唤醒 vs. 内存快照

托管版 E2B 与 CubePod 在沙箱暂停与恢复的机制上存在根本性的架构语义差异：

- **上游 E2B Cloud**：官方文档说明其暂停机制会同时保存文件系统状态与**运行内存（RAM）**，在恢复时保留内存变量与运行中的后台守护进程。
- **CubePod**：采用云原生 Kubernetes 算力缩容至零（Scale-to-Zero）机制。调用暂停时（`POST /sandboxes/{id}/pause`），计算 Pod 被终止销毁，但**会保留** `rootfs` 与 `workspace` 两块 CSI 原始块持久卷：
  - `rootfs-pvc`：挂载 OverlayFS `upperdir`，用于保存 RootFS 上的系统级修改与新装软件包。
  - `workspace-pvc`：保存持久化的 `/workspace` 工作区。
- **唤醒时的核心语义差异（`connect` 或数据面流量到达）**：
  - CubePod 会启动新的计算 Pod，并重新挂载保留下来的 `rootfs` 与 `workspace` 块卷。恢复耗时取决于部署与存储后端，本页不把它写成已测量的 SLA。
  - 已落在这些持久卷上的文件系统变更在重新挂载后仍然可用；本页不承诺客户 Guest 内每一种状态都逐位一致。
  - 仅存在于上一轮计算 Pod 中的内存和进程不会被恢复。若唤醒后仍需要守护进程，应写在模板 `startCmd` 中，或由 Agent 再次启动。

### 2. 模板构建语义与 `/workspace` 拒绝机制

在 `--e2b-template-api=true` 下使用 `Template.build()` 时：

- CubePod 在 Kubernetes 中通过无特权 rootless BuildKit（或在本地模式下通过 Docker 守护进程）构建标准 OCI 容器镜像。
- 该过程**不会**生成处于暂停状态的虚拟机内存快照。`startCmd` 会在每个从该模板派生的新沙箱启动时重新执行。
- **核心安全与架构约束**：Dockerfile 中任何针对 `/workspace` 路径的 `COPY` 或 `WORKDIR` 指令均会被**直接拒绝并返回 HTTP `400 Bad Request`（`BuildException`）**。因为 `/workspace` 在沙箱运行时由独立的持久化块 PVC（`workspace-pvc`）动态挂载，若将文件固化在镜像的 `/workspace` 路径下，会被后续卷挂载完全遮盖。模板配方中的自定义文件请统一放置在 `/opt/`、`/usr/local/` 或其它系统路径下。

### 3. 代码解释器上下文管理路径（`/contexts`）

在 E2B 规范中，代码解释器上下文可通过专门的上下文端点进行管理：

- 在 CubePod 中，支持 6 种语言（`python`、`bash`、`javascript`、`typescript`、`r`、`java`）的状态化执行上下文由 Guest 内部的 Rust `podd` 守护进程（`:49983`）直接维护。
- `cubepod-gateway` **未**将 `/sandboxes/{id}/code/contexts` 注册为公开 REST 路由。
- 业务方在需要针对特定有状态上下文运行代码时，可直接在 `POST /sandboxes/{id}/code/execute`（或 `POST /sandboxes/{id}/execute`）请求体的 JSON 中携带 `context_id`。Gateway 会自动将上下文标识透传至 `podd`。

### 4. 错误响应信封与异常映射

`cubepod-gateway` 内部不同子系统的错误响应信封格式略有不同：

- **模板 API 处理器**（`pkg/gateway/templates.go`）：通过 `writeError` 返回标准 JSON 错误体：
  ```json
  {
    "code": 404,
    "message": "template not found"
  }
  ```
- **Connect-RPC 处理器**（`pkg/gateway/connectrpc.go`）：通过 `writeConnectError` 返回 Connect-RPC 格式的 JSON 错误体：
  ```json
  {
    "code": "not_found",
    "message": "sandbox not found"
  }
  ```
- **核心 REST 沙箱处理器**（`pkg/gateway/sandboxes.go`）：通过标准 Go `http.Error` 返回纯文本错误体：
  ```
  sandbox not found
  ```

由于各子系统的错误信封格式存在差异，上游 SDK 在捕获特定错误类型（如特定 Python 异常类）时的映射行为可能与托管版 E2B 不完全一致。

---

## CubePod 原生超集能力（扩展特性）

CubePod 提供了标准 E2B 协议所不具备的企业级高阶能力。这些特性可通过 CubePod 原生 SDK（`sdks/python/`、`sdks/typescript/`、Go 客户端 `pkg/client` 与 CLI `cmd/cubepod`）或 REST 超集端点调用：

1. **持久化可写 RootFS 与无损系统重置**：
   调用 `POST /sandboxes/{id}/reset-rootfs` 递增 `spec.rootfsGeneration`，并分配新的 OS `upperdir` PVC。该操作不会替换沙箱的 `workspace-pvc`。
2. **Copy-on-Write 沙箱分支克隆（CoW Fork）**：
   调用 `POST /sandboxes/{id}/fork` 通过 `FIFREEZE` 静默文件系统，在存储层对 `rootfs` 与 `workspace` 块卷做 CoW 克隆，再调用 `FITHAW` 解冻，从而得到独立的子沙箱。耗时取决于存储后端，本页不给出测量过的时延保证。
3. **零信任出站凭据隔离（`cpodref_`）**：
   创建沙箱时传入 `secretBindings`。沙箱内部只接触不透明占位符（如 `cpodref_openai_key`），真实 API 密钥仅在流量通过 `cubepod-egress` 边车代理时在 mTLS 边界处动态替换。
4. **流式工作区目录同步（`archive-sync`）**：
   调用 `POST /sandboxes/{id}/files/archive-sync` 将本地 `.tar` / `.tar.gz` 目录树以流式传输注入沙箱。`pkg/gateway/files.go` 在解包时会检查并尝试拒绝包含 `..` 的路径段，本页不把它写成已经过完整审计的安全保证。该接口面向批量同步，而不是每个文件单独发一次 HTTP 请求。
5. **按需桌面与签名面板代理**：
   调用 `POST /sandboxes/{id}/desktop/start` 懒启动 `Xvfb`、Chromium CDP、`noVNC` 与 `ttyd`，签发带 HMAC 签名的 `/panel/{token}/` 反向代理地址，用于实时可视化交互。

---

## 迁移指南与注意事项

从托管 E2B 迁移到私有化部署的 CubePod 时的检查清单：

1. **确认环境变量配置**：
   在使用官方 SDK 前，确保已导出 `E2B_DOMAIN`（配合通配符 DNS）或同时导出了 `E2B_API_URL` 与 `E2B_SANDBOX_URL`。
2. **检查模板特性开关**（`cmd/cubepod-gateway/main.go` 与 `cmd/cubepod-controller/main.go` 都定义了 `--e2b-template-api` / `--feature-gate=E2BTemplateAPI=true` 以及 `--template-build`，但职责不同）：
   - 在 **cubepod-gateway** 上开启 `--e2b-template-api=true`（或等价 feature gate），才会注册 E2B 模板 HTTP 路由。
   - **cubepod-gateway** 也需要 `--template-build=true`，否则构建相关路由（上下文上传、日志、启动构建）会直接返回 HTTP 501，不会转到 Controller。
   - 在 Kubernetes 中，真正执行 BuildKit 或 Docker 构建流水线的是 **cubepod-controller**，需要在 Controller 上开启 `--template-build=true`。Controller 也接受 `--e2b-template-api`。
   - 在本地单进程模式（没有 Kubernetes 客户端，也没有 `--controller-url`）下，由 Gateway 进程内的 reconciler 使用 `LocalDockerBuilder`，不必单独再跑一个 Controller。
3. **排查模板中的 `/workspace` 路径**：
   检查用于构建模板的 Dockerfile，确保未包含针对 `/workspace` 的 `COPY` 指令。
4. **适配进程生命周期**：
   注意后台内存常驻进程不会在暂停期间被持久化，关键业务数据请统一落盘至 `/workspace` 或系统目录。
5. **本地运行兼容性验证套件**：
   维护人员可通过以下命令验证网关的 Python SDK 协议兼容性：
   ```bash
   make test-e2b-compat
   ```

---

## 下一步

- 在 **[API 概览与流式协议](/zh/reference/api-overview/)** 中查阅完整的 REST 与流式协议规范。
- 打开 **[交互式 OpenAPI 控制台](/zh/api/)** 进行在线接口调试。
- 查阅原生多语言客户端指南：**[Python SDK](/zh/sdks/python/)** 与 **[TypeScript SDK](/zh/sdks/typescript/)**。
- 参考 **[Kubernetes 与 Helm 生产部署](/zh/deployment/kubernetes-helm/)** 配置 Kata 容器与块存储存储类。
