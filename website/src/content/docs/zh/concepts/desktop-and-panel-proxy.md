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
| **`neko`（WebRTC）** | `0.0.0.0:8081` | 请求 `transport: "webrtc"` 时通过低延迟 WebRTC 推流 `:99` 桌面（`NEKO_STATIC=/opt/cubepod/desktop/neko-www`、`NEKO_ICELITE=1`；*v0.7.0 新增。*）。 |
| **`ttyd`** | `:7681` | 提供基于浏览器的新建 `/bin/bash` Web 终端（默认工作目录 `/workspace`）。 |

### 可靠性与权限隔离 (`cubepod-desktop-ctl`)

- **最小权限降权**：`cubepod-desktop-ctl` 会自动降权为非特权用户 UID/GID `1000:1000`（`cubeplex`），确保浏览器缓存、下载产物和交互终端与 Agent 工作区 `/workspace` 的文件属主完全一致。
- **并发锁保护**：所有启停操作通过 `flock` 串行化，防止并发调用 `start`/`stop` 产生重复的 `Xvfb` 或 `x11vnc` 进程。
- **持久化 RootFS 锁文件自愈**：由于 CubePod 会持久化整个 RootFS，沙箱经历 `Paused` -> `Running` 后旧 PID 留下的 Chromium `Singleton*` 锁文件原本会阻塞浏览器重新启动；`cubepod-desktop-ctl` 会在拉起 `chromium-wrapper` 前自动清除过期锁。
- **内置 Neko WebRTC 与自动降级保护**：*v0.7.0 新增。* `images/desktop/Dockerfile` 开箱预置 `/usr/local/bin/neko`、`/opt/cubepod/desktop/neko-www` 以及 GStreamer/Opus/VP8/H.264 运行时插件。若在未包含 Neko 的自定义镜像中请求 `transport: "webrtc"` 或 Neko 启动异常，`cubepod-desktop-ctl` 会保持 `noVNC` 正常运行，并自动回退至 `transportUsed: "novnc"`（`fallbackReason: "neko_not_installed"` 或 `"neko_start_failed"`）。

### CubePlex Tab-Follow 标签页自动跟随 (`browser_tab_follow.py`)

*v0.7.0 新增。*

由于 `x11vnc` 与 Neko 捕获的是当前前台 X11 Chromium 窗口而非独立的 CDP 标签页，当 Agent 通过 `agent-browser` 在后台标签页操作时，前端用户原本可能仍停留在旧标签页画面上。

`images/desktop/` 内置 `/opt/cubepod/desktop/browser_tab_follow.py`（软链接至 `/usr/local/bin/cubeplex-focus-browser-tab` 与 `/usr/local/bin/cubepod-focus-browser-tab`）：

1. 查询 `http://127.0.0.1:${CUBEPOD_CDP_PORT:-9222}/json` 中所有 `"type": "page"` 标签页目标。
2. 通过 `--url <url>` 或 `$AGENT_BROWSER_REAL get url` 获取当前目标地址（附带 `AGENT_BROWSER_WRAPPER=1` 递归保护）。
3. 执行**精确归一化 URL 匹配**（忽略末尾 `/`，严禁前缀匹配，确保 `https://ex.com` 绝不会误匹配 `https://ex.com/login`），并调用 `http://127.0.0.1:${CDP_PORT}/json/activate/<id>` 切换前台标签页。
4. 支持 `--install-wrapper`（`cubepod-desktop-ctl start` 在检测到可写 `agent-browser` 时自动非致命调用，也可通过 Python SDK 的 `cubepod.browser_tab_follow.tab_follow_install_command()` 按需注入），幂等地为 `/usr/local/bin/agent-browser` 安装 `# CUBEPLEX_TAB_FOLLOW=1` 包装脚本并完整保留 `agent-browser.real`。

## 2. 网关桌面控制 API (`pkg/gateway/desktop.go`)

`cubepod-gateway` 对外暴露以下桌面控制接口（同步支持 `/v1/sandboxes/{id}/desktop/...` 路径）：

- `POST /sandboxes/{id}/desktop/start` — 校验分辨率格式（`WxHx{16|24|32}`）与可选 `transport` 参数（`"webrtc" | "novnc"`，*v0.6.0 新增*），若沙箱处于 `Paused` 则自动唤醒，检查 `/opt/cubepod/desktop/cubepod-desktop-ctl` 是否存在（非桌面镜像直接返回 **`HTTP 422 desktop_not_installed`**），随后通过 `podd` 启动桌面栈并返回访问端点（`desktopUrl`、`terminalUrl`、`cdpUrl`，以及 `transportUsed`、可选端口 `8081` 的 `webrtcUrl` 与 `fallbackReason`）。若在未安装 Neko/WebRTC 二进制的镜像上请求 `transport: "webrtc"`，`cubepod-desktop-ctl` 与网关会优雅降级回退至 `noVNC`（`transportUsed: "novnc"`、`fallbackReason: "neko_not_installed"`）。
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
3. **自动唤醒与透明反代**：网关通过 `resolveSandbox` 自动唤醒可能处于 `Paused` 状态的沙箱，定位当前 `<status.podIP>:<port>`，并将 HTTP 资源与双向 WebSocket 帧（`noVNC`、`neko` 信令、`ttyd`、前端热更新 HMR 等）直接反向代理至目标 MicroVM。

## 4. 50+ 并发桌面资源评估、音频与 GPU 演进路线

*v0.7.0 新增。*

- **50+ 并发浏览器会话容量规划**：`cubepod-gateway` 已通过 `-race` 下 55 路并发 HTTP + WebSocket 桌面面板连接压测（`TestDesktopConcurrentSessions50Plus`），断开后活跃会话计数（`cubepod_panel_active_sessions`）严格归零、无连接泄漏。针对 `1280x800@30` 多标签页 Chromium + Neko WebRTC 活跃沙箱，建议将 `PodTemplate` 资源上限配置为 `2 vCPU / 2Gi` 内存（单标签页 `noVNC` 最低建议 `1Gi`），每路 WebRTC 预留约 `2 Mbps` 带宽（`NEKO_EPR=52000-52100`，跨严格 NAT 场景建议配合 coturn TURN 中继），网关侧建议部署 2+ 副本并设置 `ulimit -n >= 65536`。
- **PulseAudio + GStreamer Opus 双向音频**：`images/desktop/` 已内置 `pulseaudio`、`gstreamer1.0-pulseaudio` 与 `libopus0`，支持在非特权 `1000:1000` 用户态下采集虚拟声卡监听源（`audio_output.monitor` -> 128 kbps Opus）及可选麦克风上行输入（`audio_input`；注：`noVNC` RFB 协议仅传输视频画面）。
- **GPU 直通（`vfio-pci`）评估结论**：默认桌面模板保持 CPU 软件渲染（`--disable-gpu` + `vp8enc`），以确保跨节点秒级暂停/唤醒与零 GPU 成本的 WarmPool 能力；基于 `vfio-pci` / vGPU + `nvh264enc` 硬件编码的能力规划为 `v1.0.0` 可选专用 `gpu-desktop` `PodTemplate` 档位（详见 **[CEP 提案总览](/zh/ceps/)** 中的 CEP-0021）。

## 下一步

- 参考 **[快速开始](/zh/getting-started/quickstart/#4-启动交互式桌面可选)** 使用 Python SDK 拉起桌面会话。
- 查看完整接口参数：**[API 参考](/zh/api/)**。
- 阅读英文原版设计提案：**[CEP 提案总览](/zh/ceps/)**。
