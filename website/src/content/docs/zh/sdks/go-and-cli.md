---
title: Go SDK 与 CLI
description: 符合 Go 语言习惯的官方客户端库（pkg/client）与 cubepod 命令行工具，全面覆盖沙箱生命周期、archive_sync、模板构建及 WarmPool 管理。
---

CubePod 提供了零外部依赖的官方 Go 客户端库（`github.com/cubeplexai/cubepod/pkg/client`），以及功能完备的 `cubepod` 命令行工具（`cmd/cubepod`）。

## 第一部分：Go 客户端 SDK（`pkg/client`）

### 安装

```bash
go get github.com/cubeplexai/cubepod/pkg/client
```

### 初始化客户端

`client.New(baseURL, opts...)` 默认连接 `http://127.0.0.1:8080` 并使用 `60s` HTTP 超时：

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
)

func main() {
	ctx := context.Background()
	c := client.New(
		"http://localhost:8080",
		client.WithAPIKey("your-api-key"),
		client.WithTimeout(60*time.Second),
	)

	// 创建持久化沙箱
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: "cubeplex-ubuntu24-desktop",
		VolumeKey:  "agent-ws-01",
		Metadata:   map[string]string{"owner": "team-ai"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.DeleteSandbox(ctx, sb.SandboxID)

	res, err := c.RunCommand(ctx, sb.SandboxID, client.RunCommandRequest{
		Command: "uname -a",
		Workdir: "/workspace",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Output)
}
```

可用的构造选项：
- `client.WithAPIKey(apiKey string)`：附加 `Authorization: Bearer <apiKey>` 请求头。
- `client.WithTimeout(timeout time.Duration)`：覆盖默认的 `60s` `http.Client.Timeout`。
- `client.WithHTTPClient(httpClient *http.Client)`：传入自定义 `*http.Client`。

### 沙箱生命周期、重置与 CoW Fork

```go
// 查询与列表
sb, err := c.GetSandbox(ctx, sandboxID)
list, err := c.ListSandboxes(ctx, "cubeplex-ubuntu24-desktop") // 传 "" 列举全部

// 续期超时与独占租约保护
err = c.SetTimeout(ctx, sandboxID, 3600)
err = c.AcquireLease(ctx, sandboxID, 300)

// 暂停（删除 Pod，保留双卷 PVC）与自动唤醒连接
sb, err = c.PauseSandbox(ctx, sandboxID)
sb, err = c.ConnectSandbox(ctx, sandboxID)

// 清空 RootFS Upper 变更，同时完好保留 /workspace
sb, err = c.ResetRootFS(ctx, sandboxID)

// 约 1 秒完成 RootFS 与 Workspace 双卷写时复制克隆
child, err := c.ForkSandbox(ctx, sandboxID)

// 删除沙箱（支持选择是否同步销毁 PVC）
err = c.DeleteSandbox(ctx, sandboxID)
err = c.DeleteSandboxWithOptions(ctx, child.SandboxID, true)
```

### 命令执行与 UID/GID 语义

由于 Go 中 `int` 的零值也是 `0`（配合 `omitempty` 时无法区分未设置与 root），`RunCommandRequest` 专门提供了 `RunAsRoot`、`UIDPtr` 和 `GIDPtr`，用于精确区分*“由服务端/模板默认值决定”*与*“显式以 root（`uid=0, gid=0`）执行”*：

```go
// 后台执行命令
bg, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
	Command:    "python3 -u server.py",
	Background: true,
	Workdir:    "/workspace",
	Envs:       map[string]string{"PORT": "8080"},
})

// 显式以 root 身份执行
rootRes, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
	Command:   "apt-get update",
	RunAsRoot: true,
})

// 查询状态、增量游标拉取日志或终止进程组
status, err := c.GetCommandStatus(ctx, sandboxID, bg.ID)
logs, err := c.GetCommandLogs(ctx, sandboxID, bg.ID, 0) // 下次传入 logs.Cursor
err = c.KillCommand(ctx, sandboxID, bg.ID)
```

### 文件操作与递归 `ArchiveSyncDir`

```go
// 写入 / 读取 / 列举 / 删除文件
err := c.WriteFile(ctx, sandboxID, "/workspace/hello.txt", []byte("hi\n"), "0644")

// 指定自定义 UID/GID 属主
uid, gid := 1000, 1000
err = c.WriteFileWithOwner(ctx, sandboxID, "/workspace/bin/run", data, "0755", &uid, &gid)

raw, err := c.ReadFile(ctx, sandboxID, "/workspace/hello.txt")
entries, err := c.ListFiles(ctx, sandboxID, "/workspace")
err = c.DeleteFile(ctx, sandboxID, "/workspace/hello.txt")

// 内存文件映射 tar.gz 同步
err = c.ArchiveSync(ctx, sandboxID, "/workspace", map[string][]byte{
	"app/main.go": []byte("package main\n"),
})

// 遍历本地目录并通过单次 tar.gz 请求全量同步到沙箱
err = c.ArchiveSyncDir(ctx, sandboxID, "/workspace/skills", "./local-skills")
```

### 代理端点、WarmPool 与模板构建

```go
// 签发 HMAC-SHA256 Panel 代理 URL
ep, err := c.GetPanelEndpoint(ctx, sandboxID, 8080, 3600)

// WarmPool 预热池
wp, err := c.CreateWarmPool(ctx, "fast-pool", "cubeplex-ubuntu24-desktop", 3)
pools, err := c.ListWarmPools(ctx)

// 模板目录查询与构建（E2B 兼容接口）
templates, err := c.ListTemplates(ctx)
tpl, err := c.GetTemplateByAlias(ctx, "base")

// 无需 BuildKit，直接注册已有 OCI 镜像
created, err := c.CreateTemplateFromImage(ctx, client.CreateTemplateFromImageRequest{
	FromImage: "python:3.13-slim",
	Alias:     "py313-slim",
	CPUCount:  2,
	MemoryMB:  2048,
})

// 通过无特权 rootless BuildKit 执行构建
built, err := c.BuildTemplate(ctx, client.BuildTemplateRequest{
	Alias:     "custom-agent",
	FromImage: "ubuntu:24.04",
	Steps: []client.BuildStep{
		{Type: "RUN", Args: []string{"apt-get update && apt-get install -y curl"}},
	},
})
```

### 错误处理

所有非 `2xx`/`3xx` 的 HTTP 响应均会返回 `*client.APIError`：

```go
var apiErr *client.APIError
if errors.As(err, &apiErr) {
	fmt.Printf("HTTP %d: %s\n", apiErr.StatusCode, apiErr.Body)
}
```

---

## 第二部分：`cubepod` 命令行工具

### 编译与安装

```bash
# 在仓库根目录编译
make build-cli
./bin/cubepod --help

# 或通过 Go 工具链安装
go install github.com/cubeplexai/cubepod/cmd/cubepod@latest
```

### 全局标志与环境变量

:::caution[全局标志位置要求]
全局标志**必须置于子命令之前**（例如 `cubepod --json sandbox list`），因为 CLI 会先从前置参数中解析全局选项再分发子命令。
:::

| 标志 | 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `--gateway-url` | `CUBEPOD_GATEWAY_URL` | `http://127.0.0.1:8080` | CubePod Gateway 地址。 |
| `--api-key` | `CUBEPOD_API_KEY` | `""` | API Bearer 令牌。 |
| `--json` | — | `false` | 输出结构化 JSON，便于脚本自动化解析。 |

### 命令速查表

#### 健康检查与 MCP 服务

```bash
cubepod health
cubepod mcp    # 通过 stdio 启动 MCP JSON-RPC 2.0 服务端
```

#### 沙箱管理命令

| 命令 | 说明 |
| :--- | :--- |
| `cubepod sandbox create [--template <id>] [--volume-key <key>]` | 创建新沙箱（默认模板：`cubeplex-ubuntu24-desktop`）。 |
| `cubepod sandbox list [--template <id>]` | 列出活跃及暂停中的沙箱，支持按模板过滤。 |
| `cubepod sandbox get <sandbox-id>` | 查看状态、`rootfsGeneration` 与 `volumeKey`。 |
| `cubepod sandbox exec <sandbox-id> <cmd...>` | 在沙箱内运行前台命令，并沿用远程退出码退出。 |
| `cubepod sandbox write-file <id> <path> <file\|text>` | 若第三个参数为本地已存在文件路径则上传该文件，否则作为文本写入 `<path>`。 |
| `cubepod sandbox read-file <id> <path>` | 将沙箱内文件内容流式输出到 `stdout`。 |
| `cubepod sandbox ls <id> [path]` | 列出 `[path]` 下的目录项（默认 `/workspace`）。 |
| `cubepod sandbox rm <id> <path>` | 删除沙箱内文件或目录。 |
| `cubepod sandbox archive-sync <id> <dest> <localdir>` | 将本地目录打包为 `tar.gz` 原子同步至 `<dest>`。 |
| `cubepod sandbox pause <id>` | 暂停计算资源（`0` CPU/内存），保留持久 PVC。 |
| `cubepod sandbox connect <id>` | 自动将已暂停的沙箱唤醒恢复至 `Running`。 |
| `cubepod sandbox reset-rootfs <id>` | 重置 RootFS Upper 层，同时完好保留 `/workspace`。 |
| `cubepod sandbox fork <id>` | 对双卷执行约 1 秒的写时复制（CoW）克隆。 |
| `cubepod sandbox delete <id>` | 终止并删除沙箱。 |

#### 模板管理命令

当 Gateway 开启 `featureGates.e2bTemplateAPI=true` 时，可使用完整的模板目录与注册命令：

| 命令 | 说明 |
| :--- | :--- |
| `cubepod template list` | 列举可用模板（输出 `TEMPLATE ID`、`ALIASES`、`STATUS`、`CPU`、`MEMORY`）。 |
| `cubepod template get <id-or-alias> [--alias]` | 按 ID 或别名查询模板（遇到 404 或包含 `:`/`/` 时自动回退到别名查询）。 |
| `cubepod template create --from-image <img> --alias <alias> [--name <name>] [--cpu <n>] [--memory-mb <n>] [--disk-mb <n>]` | 无需 BuildKit，直接从已有 OCI 镜像注册模板。 |
| `cubepod template update <id> --public=true\|false` | 更新模板公开可见性。 |
| `cubepod template delete <id> [--force]` | 删除模板（`--force` 强制删除被引用的模板）。 |

#### WarmPool 命令

| 命令 | 说明 |
| :--- | :--- |
| `cubepod warmpool create <name> [--template <id>] [--replicas <n>]` | 创建或扩缩容 `PodWarmPool`（默认模板 `cubeplex-ubuntu24-desktop`，副本数 `2`）。 |
| `cubepod warmpool list` | 列举所有 WarmPool 及其就绪副本数量。 |

### CLI 工作流示例

```bash
export CUBEPOD_GATEWAY_URL="http://localhost:8080"

# 创建带有确定性工作区键的沙箱
SANDBOX_ID=$(cubepod --json sandbox create --template cubeplex-ubuntu24-desktop --volume-key ws-demo | jq -r '.sandboxID')

# 同步本地 skills 目录并运行脚本
cubepod sandbox archive-sync "$SANDBOX_ID" /workspace/skills ./skills
cubepod sandbox exec "$SANDBOX_ID" "ls -la /workspace/skills && python3 --version"

# 清空异常的系统包状态，/workspace 文件完整保留
cubepod sandbox reset-rootfs "$SANDBOX_ID"

# 秒级克隆出独立分支沙箱
FORK_ID=$(cubepod --json sandbox fork "$SANDBOX_ID" | jq -r '.sandboxID')
```

如需将 CubePod 直接暴露给兼容 MCP 的 IDE 与桌面助手，请继续阅读 [MCP Server 集成](/zh/sdks/mcp-server/)。
