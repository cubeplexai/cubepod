---
title: Python SDK
description: 面向持久化 MicroVM 沙箱的异步优先与同步 Python 客户端（cubepod），支持游标日志流、archive_sync 目录同步、CoW Fork 克隆、桌面会话与模板构建。
---

CubePod 官方 Python SDK（`cubepod`）针对 CubePod 控制面与数据面 HTTP API 同时提供**异步优先**（`AsyncCubePod`）和**同步**（`CubePod` / `SyncCubePod`）两套客户端。

- **包名**：`cubepod`（`0.4.1`，Apache-2.0）
- **Python 版本**：`>= 3.11`，内置完整 `py.typed` 类型注解
- **运行时依赖**：`httpx >= 0.27.0`

## 安装

```bash
pip install cubepod
```

:::tip[正在使用官方 `e2b` Python SDK？]
如果您已有基于 `e2b` 或 `e2b-code-interpreter` 编写的 Agent 代码，CubePod 原生兼容其协议接口，无需重写现有 SDK 调用。请参阅 [E2B 无缝兼容指南](/zh/getting-started/e2b-compatibility/)。
:::

## 快速开始（异步与同步）

两种客户端均支持 Python 上下文管理器（`async with` / `with`），退出时自动优雅关闭底层的 `httpx` 连接池。

```python
import asyncio
from cubepod import AsyncCubePod, CubePod

async def main() -> None:
    async with AsyncCubePod(
        base_url="http://localhost:8080",
        api_key="your-api-key",
    ) as client:
        box = await client.create("cubeplex-ubuntu24-desktop")
        res = await client.execute(box.sandbox_id, "python3 --version")
        print(res.output)
        await client.delete(box.sandbox_id)

asyncio.run(main())

# 同步客户端（SyncCubePod 为 CubePod 的别名）
with CubePod(base_url="http://localhost:8080", api_key="your-api-key") as client:
    box = client.create("cubeplex-ubuntu24-desktop")
    print(client.execute(box.sandbox_id, "uname -a").output)
    client.delete(box.sandbox_id)
```

### 客户端配置项

`AsyncCubePod(...)` 与 `CubePod(...)` 共享以下构造参数：

| 参数 | 类型 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `base_url` | `str` | `"http://localhost:8080"` | CubePod Gateway 地址（若省略协议头，将自动补充 `http://`）。 |
| `api_key` | `str \| None` | `None` | 可选 Bearer 令牌，通过 `Authorization: Bearer <api_key>` 发送。 |
| `transport` | `httpx transport \| None` | `None` | 自定义 `httpx.AsyncBaseTransport` 或 `httpx.BaseTransport`，便于单测或自定义路由。 |
| `max_retries` | `int` | `2` | 遇到 HTTP `502`、`503`、`504` 时采用指数退避的最大自动重试次数。 |
| `timeout` | `float` | `60.0` | 默认单次请求超时时间（秒）。 |
| `trust_env` | `bool` | `False` | 是否允许 `httpx` 读取宿主机 `HTTP_PROXY` / `HTTPS_PROXY` 环境变量。 |

## 沙箱生命周期、RootFS 重置与秒级 CoW Fork

CubePod 将持久化的容器 RootFS `upperdir` 与 `/workspace` 解耦存储（参见[双卷持久化架构](/zh/concepts/dual-volume-persistence/)）。每次生命周期调用都会返回 `SandboxInfo` 数据类：

```python
box = await client.create(
    template_id="cubeplex-ubuntu24-desktop",
    volume_key="user-42-workspace",
    timeout_seconds=1800,
    warm_pool_ref="default-pool",
    metadata={"agent": "coder", "env": "dev"},
)

# 检查状态属性
print(box.sandbox_id, box.state, box.rootfs_generation, box.is_terminal)

# 列举与查询沙箱
boxes = await client.list(template_id="cubeplex-ubuntu24-desktop")
same_box = await client.get(box.sandbox_id)
raw_json = await client.get_raw(box.sandbox_id)

# 续期超时时间，或通过租约保护沙箱免于空闲自动暂停
await client.renew_timeout(box.sandbox_id, timeout_seconds=3600)
await client.acquire_lease(box.sandbox_id, lease_seconds=300)

# 暂停计算资源（0 CPU/内存占用，完整保留 RootFS Upper 与 Workspace PVC）
await client.pause(box.sandbox_id)

# 连接沙箱（若处于 Paused 状态则自动唤醒恢复）
await client.connect(box.sandbox_id)

# 清空 RootFS Upper 变更（如修复损坏的 apt/pip 状态），同时完好保留 /workspace
await client.reset_rootfs(box.sandbox_id)

# ~1s 内对 RootFS 与 Workspace 双卷执行写时复制（CoW）克隆
child = await client.fork(
    box.sandbox_id,
    volume_key="user-42-branch-a",
    metadata={"branch": "experiment-1"},
)

await client.delete(child.sandbox_id)
await client.delete(box.sandbox_id)
```

### `create(...)` 核心参数

- `template_id`（`str = "cubeplex-ubuntu24-desktop"`）：模板 ID、别名或 OCI 镜像引用。
- `volume_key`（`str | None`）：确定性持久存储键（`extension.volumeKey`），便于跨沙箱重新挂载工作区状态。
- `metadata`（`dict[str, str] | None`）：附加到沙箱的任意键值标签。
- `network_policy`（`dict[str, Any] | None`）：Zero-Trust 出站防火墙策略（`defaultAction`、`allowHosts`、`denyHosts`、`upstreamProxy`）。
- `secret_bindings`（`list[dict[str, Any]] | None`）：`cpodref_` 凭证绑定列表，自动合入 `networkPolicy.secretBindings`。
- `timeout_seconds`（`int | None`）：触发空闲自动暂停前的超时秒数。
- `warm_pool_ref`（`str | None`）：从指定 `PodWarmPool` 中直接认领预热好的 MicroVM。
- `resource`（`dict[str, str] | None`）：写入 `extension.resource` 的可选资源规格覆盖。

## 命令执行与游标日志流

前台命令通过 `execute(...)` 运行，默认使用 `uid=1000, gid=1000`（如需 root 权限可显式传入 `uid=0, gid=0`，若希望完全省略则传入 `uid=None, gid=None`）：

```python
# 前台同步执行
res = await client.execute(
    box.sandbox_id,
    "pytest -q",
    workdir="/workspace",
    envs={"PYTHONUNBUFFERED": "1"},
    uid=1000,
    gid=1000,
    timeout=120.0,
)
print(res.exit_code, res.output)

# 后台执行 + 无状态字节偏移游标轮询
cmd = await client.start_command(
    box.sandbox_id,
    "for i in 1 2 3; do echo tick-$i; sleep 1; done",
    workdir="/workspace",
)

status = await client.get_command_status(box.sandbox_id, cmd.id)
logs = await client.poll_command(box.sandbox_id, cmd.id, cursor=0)
print(logs.content, logs.cursor, logs.running)

# 持续流式消费增量 CommandLogs，直至进程退出
async for chunk in client.stream_command(box.sandbox_id, cmd.id, poll_interval=0.05):
    if chunk.content:
        print(chunk.content, end="")

# 终止后台命令进程组
await client.kill_command(box.sandbox_id, cmd.id)
```

## 文件操作、流式读取与 `archive_sync`

```python
# 写入 UTF-8 文本或原始字节
await client.write_file(
    box.sandbox_id,
    "/workspace/app.py",
    "print('hello cubepod')\n",
    uid=1000,
    gid=1000,
    mode="0644",
)

# 完整读取字节、分块流式读取或有界文本读取
raw_bytes = await client.read_file(box.sandbox_id, "/workspace/app.py")

stream = await client.stream_file(box.sandbox_id, "/workspace/large.bin", chunk_size=65536)
async for chunk in stream:
    process_chunk(chunk)

# 若文件超过 max_bytes，将抛出 CubePodFileTooLargeError
from cubepod import CubePodFileTooLargeError

try:
    text = await client.read_text_file(
        box.sandbox_id,
        "/workspace/app.py",
        max_bytes=1024 * 1024,
        encoding="utf-8",
    )
except CubePodFileTooLargeError as exc:
    print("文件过大:", exc)

# 列举与删除文件
entries = await client.list_files(box.sandbox_id, "/workspace")
for entry in entries:
    print(entry.name, entry.is_dir, entry.size, entry.mode)

await client.delete_file(box.sandbox_id, "/workspace/old.txt")

# 原子级 tar.gz 目录同步（支持 path -> content 映射或预构建的 .tar.gz 字节流）
await client.archive_sync(
    box.sandbox_id,
    {
        "skills/research/SKILL.md": "# Research Skill\n",
        "skills/research/run.py": b"print('ready')\n",
    },
    dest="/workspace",
    uid=1000,
    gid=1000,
    remove_paths=["skills/deprecated"],
)
```

## 交互式桌面与签名 Panel 端点

CubePod 支持按需懒启动桌面环境（XFCE + noVNC、`ttyd` Web 终端与 Chromium CDP）以及基于 HMAC-SHA256 签名的 `/panel/{token}/` 反向代理端点：

```python
# 在支持桌面的沙箱内按需启动交互式桌面栈
desktop = await client.start_desktop(
    box.sandbox_id,
    resolution="1280x800x24",
    initial_url="https://example.com",
    expires_in_seconds=3600,
)
print(desktop.desktop_url, desktop.terminal_url, desktop.cdp_url)

status = await client.get_desktop_status(box.sandbox_id)
await client.stop_desktop(box.sandbox_id)

# 为沙箱内任意 HTTP/WS/WebRTC 端口签发短时 Panel 代理 URL
panel = await client.get_panel_endpoint(box.sandbox_id, port=8080, expires_in_seconds=3600)
print(panel.url, panel.token, panel.expires_at)
```

## Zero-Trust `cpodref_` 出站密钥绑定

切勿将真实的外部 API 密钥直接放入 `envs` 或沙箱文件中。请在创建沙箱时注册 `secret_bindings`，确保 MicroVM 内部仅存在不透明的 `cpodref_` 占位符，而由 `cubepod-egress` 在放行目标域名时再注入真实凭证：

```python
box = await client.create(
    "cubeplex-ubuntu24-desktop",
    network_policy={
        "defaultAction": "Deny",
        "allowHosts": ["api.openai.com", "api.github.com"],
    },
    secret_bindings=[
        {
            "envName": "OPENAI_API_KEY",
            "rawSecret": os.environ["REAL_OPENAI_KEY"],
            "allowedHosts": ["api.openai.com"],
            "allowedHeaders": ["Authorization"],
        }
    ],
)
```

完整的 mTLS 密钥交换流程请参阅[零信任出站代理与密钥隔离](/zh/concepts/zero-trust-egress/)。

## WarmPool 预热池与模板管理

```python
# 创建或扩缩容待命 PodWarmPool，实现毫秒级沙箱认领
await client.create_warmpool("fast-pool", template_id="cubeplex-ubuntu24-desktop", replicas=3)
pools = await client.list_warmpools()

# 模板目录查询与构建
templates = await client.list_templates()
tpl = await client.get_template_by_alias("base")

# 注册现有 OCI 镜像（无需 BuildKit）
registered = await client.create_template_from_image(
    from_image="python:3.13-slim",
    alias="py313-slim",
    cpu_count=2,
    memory_mb=2048,
    wait=True,
)

# 通过 BuildKit 执行多步自定义构建
built = await client.build_template(
    name="data-science:v1",
    from_image="python:3.13",
    steps=[{"type": "RUN", "args": ["pip install pandas numpy"]}],
    cpu_count=4,
    memory_mb=4096,
    wait=True,
)
```

完整的模板构建规则与约束请参阅[模板目录与构建](/zh/sdks/templates/)。

## 数据模型参考

| 模型类 | 核心字段 |
| :--- | :--- |
| `SandboxInfo` | `sandbox_id`、`template_id`、`state`、`envd_port`（`49983`）、`rootfs_generation`（`1`）、`metadata`、`raw`、`.is_terminal` |
| `ExecutionResult` | `id`、`exit_code`、`output`、`running`、`pid`、`cursor` |
| `CommandInfo` | `id`、`pid`、`running`、`exit_code`、`cursor`、`output` |
| `CommandLogs` | `id`、`content`、`cursor`、`running`、`exit_code` |
| `FileEntry` | `name`、`path`、`is_dir`、`size`、`mode`、`modified_at` |
| `PanelEndpoint` | `url`、`token`、`expires_at` |
| `DesktopSession` | `status`、`desktop_url`、`terminal_url`、`cdp_url`、`expires_at` |
| `CubePodFileTooLargeError` | `ValueError` 子类，当 `read_text_file` 超出 `max_bytes` 限制时抛出 |
