---
title: 交互式桌面栈与签名面板代理
description: 了解 CubePod 如何按需懒启动零闲置开销的图形桌面环境（Xvfb、Chromium CDP、noVNC、ttyd），并通过无状态 HMAC-SHA256 /panel/{token}/ 代理安全暴露。
---

面向 Computer-Use 与浏览器自动化任务的 AI Agent，往往需要真实的 X11 图形环境、Chromium DevTools Protocol (CDP)、人机协同 VNC 查看界面以及内嵌 Web 终端——同时又不能让无头代码沙箱白白消耗图形内存，也不应为每个沙箱单独创建集群 Ingress 或暴露端口。

CubePod 通过两大协同设计解决了这一问题（参见 **[CEP 提案总览](/zh/ceps/)** 中的 CEP-0004 与 CEP-0021）：**零闲置开销的按需桌面栈（`images/desktop/`）** 与 **无状态 HMAC-SHA256 `/panel/{token}/` 反向代理（`pkg/gateway/panel.go`）**。

## 1. 零闲置开销的按需桌面栈

基于 CubePod 桌面镜像（`images/desktop/`）启动的沙箱默认执行 `CMD ["sleep", "infinity"]`，**闲置期间图形进程内存占用接近 0 MiB**。只有在客户端显式触发时才会按需拉起桌面组件。

整个桌面栈由 `/opt/cubepod/desktop/cubepod-desktop-ctl`（`start`、`status`、`stop`）统一管理，存放路径与 `/opt/cubepod/bin` 严格分离，互不干扰：

| 守护进程 | 监听 / 显示 | 职责与默认配置 |
| :--- | :--- | :--- |
| **`Xvfb`** | 显示屏 `:99` | 虚拟 X11 帧缓冲，默认分辨率 `1280x800x24`（支持通过 `WxHx{16\|24\|32}` 校验格式自定义）。 |
| **`openbox`** | 显示屏 `:99` | 轻量级 X11 窗口管理器，承载浏览器与应用窗口。 |
| **`chromium-wrapper`** | `127.0.0.1:9222` | 暴露 Chrome DevTools Protocol (CDP)，支持指定 `initialUrl`，并在启动前自动清理跨暂停/重启持久化的 RootFS 残留 `SingletonLock` / `SingletonSocket` / `SingletonCookie` 锁文件。 |
| **`x11vnc`** | `127.0.0.1:5900` | 将 `:99` X11 显示画面本地绑定为 VNC 服务。 |
| **`websockify` + `noVNC`** | `0.0.0.0:8080` | 将 VNC 转换为 WebSocket，并在浏览器提供 `vnc.html` 交互视图。 |
| **`ttyd`** | `:7681` | 提供基于浏览器的新建 `/bin/bash` Web 终端（默认工作目录 `/workspace`）。 |

### 可靠性与权限隔离 (`cubepod-desktop-ctl`)

- **最小权限降权**：`cubepod-desktop-ctl` 会自动降权为非特权用户 UID/GID `1000:1000`（`cubeplex`），确保浏览器缓存、下载产物和交互终端与 Agent 工作区 `/workspace` 的文件属主完全一致。
- **并发锁保护**：所有启停操作通过 `flock` 串行化，防止并发调用 `start`/`stop` 产生重复的 `Xvfb` 或 `x11vnc` 进程。
- **持久化 RootFS 锁文件自愈**：由于 CubePod 会持久化整个 RootFS，沙箱经历 `Paused` -> `Running` 后旧 PID 留下的 Chromium `Singleton*` 锁文件原本会阻塞浏览器重新启动；`cubepod-desktop-ctl` 会在拉起 `chromium-wrapper` 前自动清除过期锁。

> **流媒体协议说明：** 当前仓库内置的 `images/desktop/` 镜像交付 **`noVNC`（`:8080`）**、**`ttyd`（`:7681`）** 与 **Chromium CDP（`:9222`）**；同时 `cubepod-gateway` 的面板代理与平台架构蓝图亦支持针对自定义镜像暴露的 WebRTC / Neko 媒体端点进行反向代理。

## 2. 网关桌面控制 API (`pkg/gateway/desktop.go`)

`cubepod-gateway` 对外暴露以下桌面控制接口（同步支持 `/v1/sandboxes/{id}/desktop/...` 路径）：

- `POST /sandboxes/{id}/desktop/start` — 校验分辨率格式（`WxHx{16|24|32}`），若沙箱处于 `Paused` 则自动唤醒，检查 `/opt/cubepod/desktop/cubepod-desktop-ctl` 是否存在（非桌面镜像直接返回 **`HTTP 422 desktop_not_installed`**），随后通过 `podd` 启动桌面栈并返回访问端点（`desktopUrl`、`terminalUrl`、`cdpUrl`）。
- `GET /sandboxes/{id}/desktop/status` — 实时查询 `Xvfb`、`openbox`、`chromium`、`x11vnc`/`noVNC` 与 `ttyd` 的运行状态。
- `POST /sandboxes/{id}/desktop/stop` — 优雅停止图形守护进程以释放内存，同时不影响沙箱计算或 `/workspace` 数据。

典型 `start` 返回地址包括：

- `desktopUrl`：`/panel/<signed-token>/vnc.html?autoconnect=true&resize=scale`
- `terminalUrl`：`ttyd` 交互式 Web 终端端点（端口 `7681`）
- `cdpUrl`：用于无头或有头浏览器自动化的 Chrome DevTools Protocol (CDP) 端点（端口 `9222`）

## 3. 无状态签名面板反向代理 (`pkg/gateway/panel.go`)

CubePod 无需为每个沙箱动态创建 Kubernetes `Ingress`，即可通过短效加密签名 URL 安全代理沙箱内**任意端口**（`noVNC` `8080`、`ttyd` `7681`、Jupyter、Vite/React 开发服务器或 MCP 面板）：

1. **令牌签发 (`POST /sandboxes/{id}/endpoints/{port}`)**：将 `sandboxID:port:expUnix`（默认 TTL 为 **`3600` 秒**）与 **HMAC-SHA256** 签名编码为：
   ```text
   base64url(sandboxID:port:expUnix).base64url(hmacSha256)
   ```
2. **多副本无状态校验**：任意 `cubepod-gateway` 副本收到 `GET /panel/{token}/{rest...}`（或 WebSocket 握手请求）时，均可本地完成 HMAC 签名与过期时间验证，不依赖集中式 Session 存储。
3. **自动唤醒与透明反代**：网关通过 `resolveSandbox` 自动唤醒可能处于 `Paused` 状态的沙箱，定位当前 `<status.podIP>:<port>`，并将 HTTP 资源与双向 WebSocket 帧（`noVNC`、`ttyd`、前端热更新 HMR 等）直接反向代理至目标 MicroVM。

## 下一步

- 参考 **[快速开始](/zh/getting-started/quickstart/#4-启动交互式桌面可选)** 使用 Python SDK 拉起桌面会话。
- 查看完整接口参数：**[API 参考](/zh/api/)**。
- 阅读英文原版设计提案：**[CEP 提案总览](/zh/ceps/)**。
