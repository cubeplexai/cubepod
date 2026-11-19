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
| **`neko` (WebRTC)** | `0.0.0.0:8081` | Streams X11 `:99` over low-latency WebRTC (`NEKO_STATIC=/opt/cubepod/desktop/neko-www`, `NEKO_ICELITE=1`) when `transport: "webrtc"` is requested (*Added in v0.7.0*). |
| **`ttyd`** | `:7681` | Streams an interactive web terminal (`/bin/bash`) starting in `/workspace`. |

### Reliability & Privilege Safety (`cubepod-desktop-ctl`)

- **Privilege Separation**: Although `podd` spawns control hooks cleanly, `cubepod-desktop-ctl` drops privileges to UID/GID `1000:1000` (`cubeplex`) so browser caches, downloads, and shell sessions run unprivileged and share `/workspace` ownership automatically.
- **Concurrency Locking**: Operations are serialized via `flock` so concurrent `start` / `stop` calls never spawn duplicate `Xvfb` or `x11vnc` instances.
- **RootFS-Safe Chromium Recovery**: Because CubePod persists RootFS across pod restarts, Chromium's previous PID profile locks (`Singleton*`) would normally prevent relaunching after a `Paused` -> `Running` transition. `cubepod-desktop-ctl` cleans stale lock files before starting `chromium-wrapper`.
- **Packaged Neko WebRTC & Graceful Fallback**: *Added in v0.7.0.* `images/desktop/Dockerfile` packages `/usr/local/bin/neko`, `/opt/cubepod/desktop/neko-www`, and GStreamer/Opus/VP8/H.264 runtime plugins out of the box. If `transport: "webrtc"` is requested on a custom image without Neko or if Neko fails to initialize, `cubepod-desktop-ctl` keeps `noVNC` healthy and reports `transportUsed: "novnc"` with `fallbackReason: "neko_not_installed"` or `"neko_start_failed"`.

### CubePlex Tab-Follow Integration (`browser_tab_follow.py`)

*Added in v0.7.0.*

Because `x11vnc` and Neko capture the active X11 Chromium window rather than individual CDP page sessions, an agent driving a background tab via `agent-browser` over CDP (`127.0.0.1:9222`) could otherwise leave human viewers watching an inactive foreground tab.

`images/desktop/` installs `/opt/cubepod/desktop/browser_tab_follow.py` (symlinked at `/usr/local/bin/cubeplex-focus-browser-tab` and `/usr/local/bin/cubepod-focus-browser-tab`):

1. Queries `http://127.0.0.1:${CUBEPOD_CDP_PORT:-9222}/json` for `"type": "page"` targets.
2. Resolves the active URL via `--url <url>` or `$AGENT_BROWSER_REAL get url` (guarded by `AGENT_BROWSER_WRAPPER=1` to prevent wrapper recursion).
3. Performs **exact normalized URL matching** (trailing `/` ignored; never prefix-matching so `https://ex.com` never matches `https://ex.com/login`) and activates the matching tab via `http://127.0.0.1:${CDP_PORT}/json/activate/<id>`.
4. Supports `--install-wrapper` (invoked non-fatally by `cubepod-desktop-ctl start` when `agent-browser` is present and writable, or via `cubepod.browser_tab_follow.tab_follow_install_command()` in the Python SDK) to idempotently wrap `/usr/local/bin/agent-browser` (`# CUBEPLEX_TAB_FOLLOW=1`, preserving `agent-browser.real`).

## 2. Gateway Desktop API (`pkg/gateway/desktop.go`)

`cubepod-gateway` exposes three lifecycle endpoints (also available under `/v1/sandboxes/{id}/desktop/...`):

- `POST /sandboxes/{id}/desktop/start` — validates resolution (`WxHx{16|24|32}`) and optional `transport` (`"webrtc" | "novnc"`, *Added in v0.6.0*), auto-resumes the sandbox if `Paused`, verifies `/opt/cubepod/desktop/cubepod-desktop-ctl` exists (returning **`HTTP 422 desktop_not_installed`** on non-desktop images), starts the stack via `podd`, and returns ready URLs (`desktopUrl`, `terminalUrl`, `cdpUrl`, plus `transportUsed`, optional `webrtcUrl` on port `8081`, and `fallbackReason`). When `transport: "webrtc"` is requested on images without Neko/WebRTC binaries, `cubepod-desktop-ctl` and the gateway fall back gracefully to `noVNC` (`transportUsed: "novnc"`, `fallbackReason: "neko_not_installed"`).
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
   The request body only accepts `expiresInSeconds`. Client `targetUrl` / `upstreamUrl` values are ignored (*Changed in v0.7.0*). Official E2B `getHost` / `get_host` uses `GET /sandboxes/{id}/endpoints/{port}` and is unchanged.
2. **Stateless Verification Across Replicas**: Any `cubepod-gateway` replica receiving `GET /panel/{token}/{rest...}` (or a WebSocket upgrade handshake) verifies the HMAC signature and expiration timestamp without needing central session storage. Tokens are signed with `CUBEPOD_PANEL_SECRET`, which is required at gateway startup. The Helm chart generates a random Secret when `gateway.panelSecret` is omitted and reuses it on upgrade; there is no compiled-in default (*Changed in v0.7.0*).
3. **Auto-Resume & Live Reverse Proxy**: Using `resolveSandbox`, the gateway wakes the sandbox if it is `Paused`, always resolves its current `<status.podIP>:<port>` from sandbox status (not a client-supplied upstream), and reverse-proxies HTTP assets and bidirectional WebSocket frames (`noVNC`, `neko` signaling, `ttyd`, HMR dev servers) directly to the MicroVM (*Changed in v0.7.0*).

## 4. Production Sizing, Audio & GPU Roadmap

*Added in v0.7.0.*

- **50+ Concurrent Browser Session Sizing**: `cubepod-gateway` is race-tested across 55 simultaneous active HTTP + WebSocket desktop sessions (`TestDesktopConcurrentSessions50Plus`) with zero session leaks (`cubepod_panel_active_sessions`). For active multi-tab Chromium + Neko WebRTC sandboxes at `1280x800@30`, size `PodTemplate` limits to `2 vCPU / 2Gi` RAM (`1Gi` minimum floor for single-tab `noVNC`), ~2 Mbps egress per WebRTC stream (`NEKO_EPR=52000-52100`, plus coturn TURN relay when clients traverse strict NAT), and `ulimit -n >= 65536` across 2+ gateway replicas.
- **PulseAudio + GStreamer Opus Audio**: `images/desktop/` includes `pulseaudio`, `gstreamer1.0-pulseaudio`, and `libopus0` so WebRTC sessions can capture virtual null-sink monitor audio (`audio_output.monitor` -> 128 kbps Opus) and optional microphone uplink (`audio_input`) as unprivileged UID `1000:1000` (`noVNC` RFB remains video-only).
- **GPU Passthrough (`vfio-pci`) Evaluation**: CPU software rendering (`--disable-gpu` + `vp8enc`) remains the default so MicroVMs retain node-agnostic pause/resume and WarmPool portability; discrete `vfio-pci` / vGPU + hardware `nvh264enc` is reserved as an opt-in `gpu-desktop` `PodTemplate` profile on the `v1.0.0` roadmap (see [CEP-0021](/ceps/0021-interactive-desktop-and-panel-stack/)).

## Next Steps

- Try launching a desktop session in Python from the **[Quickstart](/getting-started/quickstart/#4-launch-an-interactive-desktop-optional)**.
- Inspect the full gateway endpoints in the **[API Reference](/api/)**.
- Read the design specifications in **[CEP-0004](/ceps/0004-gateway-and-panel-proxy/)** and **[CEP-0021](/ceps/0021-interactive-desktop-and-panel-stack/)**.
