---
title: Interactive Desktop & Signed Panel Proxy
description: How CubePod provides zero-overhead lazy GUI/browser/terminal desktops (Xvfb, Chromium CDP, noVNC, ttyd) and exposes sandbox UIs through HMAC-SHA256 /panel/{token}/ reverse proxying.
---

Computer-use and browser-automation agents frequently need a real X11 display, Chromium DevTools Protocol (CDP), human-in-the-loop VNC viewing, and a live browser shell — without forcing non-GUI sandboxes to pay idle RAM overhead or punch per-sandbox firewall holes in Kubernetes.

CubePod solves this with two tightly integrated mechanisms ([CEP-0004](/ceps/0004-gateway-and-panel-proxy/), [CEP-0021](/ceps/0021-interactive-desktop-and-panel-stack/)): a **lazy on-demand desktop stack (`images/desktop/`)** and a **stateless HMAC-SHA256 `/panel/{token}/` reverse proxy (`pkg/gateway/panel.go`)**.

## 1. Zero-Idle-Cost Lazy Desktop Stack

Containers built from CubePod's desktop image (`images/desktop/`) boot into `CMD ["sleep", "infinity"]` with **~0 MiB desktop daemon memory overhead**. Graphical daemons remain dormant until a client explicitly starts a desktop session.

All desktop processes are managed by `/opt/cubepod/desktop/cubepod-desktop-ctl` (`start`, `status`, `stop`), stored separately from `/opt/cubepod/bin` so runtime binaries remain decoupled:

| Daemon | Bind / Display | Role & Configuration |
| :--- | :--- | :--- |
| **`Xvfb`** | Display `:99` | Virtual X11 framebuffer defaulting to `1280x800x24` (customizable via validated `WxHx{16\|24\|32}` resolution). |
| **`openbox`** | Display `:99` | Lightweight X11 window manager for browser and desktop windows. |
| **`chromium-wrapper`** | `127.0.0.1:9222` | Headless/headed Chromium exposing Chrome DevTools Protocol (CDP), optionally navigating to `initialUrl`. Cleans stale `SingletonLock` / `SingletonSocket` / `SingletonCookie` files from persisted RootFS across sandbox pauses or restarts. |
| **`x11vnc`** | `127.0.0.1:5900` | Binds the `:99` X display locally over VNC. |
| **`websockify` + `noVNC`** | `0.0.0.0:8080` | Bridges VNC WebSockets into browser-viewable `vnc.html`. |
| **`ttyd`** | `:7681` | Streams an interactive web terminal (`/bin/bash`) starting in `/workspace`. |

### Reliability & Privilege Safety (`cubepod-desktop-ctl`)

- **Privilege Separation**: Although `podd` spawns control hooks cleanly, `cubepod-desktop-ctl` drops privileges to UID/GID `1000:1000` (`cubeplex`) so browser caches, downloads, and shell sessions run unprivileged and share `/workspace` ownership automatically.
- **Concurrency Locking**: Operations are serialized via `flock` so concurrent `start` / `stop` calls never spawn duplicate `Xvfb` or `x11vnc` instances.
- **RootFS-Safe Chromium Recovery**: Because CubePod persists RootFS across pod restarts, Chromium's previous PID profile locks (`Singleton*`) would normally prevent relaunching after a `Paused` -> `Running` transition. `cubepod-desktop-ctl` cleans stale lock files before starting `chromium-wrapper`.

> **Protocol Note:** The shipping `images/desktop/` stack serves **`noVNC` (`:8080`)**, **`ttyd` (`:7681`)**, and **Chromium CDP (`:9222`)**, while CubePod's panel reverse proxy and architecture blueprint also support proxying WebRTC/Neko streaming endpoints when supplied by custom images.

## 2. Gateway Desktop API (`pkg/gateway/desktop.go`)

`cubepod-gateway` exposes three lifecycle endpoints (also available under `/v1/sandboxes/{id}/desktop/...`):

- `POST /sandboxes/{id}/desktop/start` — validates resolution (`WxHx{16|24|32}`), auto-resumes the sandbox if `Paused`, verifies `/opt/cubepod/desktop/cubepod-desktop-ctl` exists (returning **`HTTP 422 desktop_not_installed`** on non-desktop images), starts the stack via `podd`, and returns ready URLs (`desktopUrl`, `terminalUrl`, `cdpUrl`).
- `GET /sandboxes/{id}/desktop/status` — returns live status of `Xvfb`, `openbox`, `chromium`, `x11vnc`/`noVNC`, and `ttyd`.
- `POST /sandboxes/{id}/desktop/stop` — gracefully tears down GUI daemons to reclaim RAM while preserving sandbox compute and files.

Example response URLs returned by `start`:

- `desktopUrl`: `/panel/<signed-token>/vnc.html?autoconnect=true&resize=scale`
- `terminalUrl`: interactive `ttyd` terminal endpoint (port `7681`)
- `cdpUrl`: Chrome DevTools Protocol (CDP) endpoint for headless/headed Chromium automation (port `9222`)

## 3. Stateless Signed Panel Proxy (`pkg/gateway/panel.go`)

Instead of provisioning per-sandbox Kubernetes `Ingress` objects or exposing container ports directly, `cubepod-gateway` issues short-lived, cryptographically signed panel URLs for **any** port inside a sandbox (desktop `8080`, `ttyd` `7681`, Jupyter, Vite/React dev servers, MCP UIs):

1. **Token Minting (`POST /sandboxes/{id}/endpoints/{port}`)**: Encodes payload `sandboxID:port:expUnix` (default TTL **`3600` seconds**), signs it with **HMAC-SHA256**, and outputs:
   ```text
   base64url(sandboxID:port:expUnix).base64url(hmacSha256)
   ```
2. **Stateless Verification Across Replicas**: Any `cubepod-gateway` replica receiving `GET /panel/{token}/{rest...}` (or a WebSocket upgrade handshake) verifies the HMAC signature and expiration timestamp without needing central session storage.
3. **Auto-Resume & Live Reverse Proxy**: Using `resolveSandbox`, the gateway wakes the sandbox if it is `Paused`, resolves its current `<status.podIP>:<port>`, and reverse-proxies HTTP assets and bidirectional WebSocket frames (`noVNC`, `ttyd`, HMR dev servers) directly to the MicroVM.

## Next Steps

- Try launching a desktop session in Python from the **[Quickstart](/getting-started/quickstart/#4-launch-an-interactive-desktop-optional)**.
- Inspect the full gateway endpoints in the **[API Reference](/api/)**.
- Read the design specifications in **[CEP-0004](/ceps/0004-gateway-and-panel-proxy/)** and **[CEP-0021](/ceps/0021-interactive-desktop-and-panel-stack/)**.
