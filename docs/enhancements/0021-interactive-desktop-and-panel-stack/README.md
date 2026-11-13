# CEP-0021: Interactive Desktop & Browser GUI Stack with Lazy Activation and Panel Streaming

| Field | Value |
| :--- | :--- |
| **CEP Number** | `0021` |
| **Status** | `implementable` (`alpha`) |
| **Owning Area** | `runtime` |
| **Participating Areas** | `gateway`, `controller` |
| **Authors** | @cubepod-maintainers |
| **Created** | 2026-11-13 |
| **Last Updated** | 2026-11-13 |

<!-- toc -->
- [Release Signoff Checklist](#release-signoff-checklist)
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Notes / Constraints / Caveats](#notes--constraints--caveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API / CRD / Wire Changes](#api--crd--wire-changes)
  - [Architecture & State Machine](#architecture--state-machine)
  - [Boundary Conditions & Failure Recovery](#boundary-conditions--failure-recovery)
  - [Dual-Runtime Behavior (K8s Kata vs. Local Docker)](#dual-runtime-behavior-k8s-kata-vs-local-docker)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
  - [Upgrade / Downgrade Strategy](#upgrade--downgrade-strategy)
- [Production Readiness Review Questionnaire (PRR)](#production-readiness-review-questionnaire-prr)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

---

## Release Signoff Checklist

- [x] Enhancement scope, Goals, and Non-Goals are agreed upon (`provisional` -> `implementable`)
- [x] API / OpenAPI (`POST /sandboxes/{sandboxID}/desktop/start`, `GET /sandboxes/{sandboxID}/desktop/status`) and image supervisor contracts are specified
- [x] Dual-runtime behavior (Kubernetes Kata + CSI Block AND Local Docker Loopback) is defined
- [x] Security boundary (`podd` not baked into image, browser/terminal run as `1000:1000`, HMAC-signed `/panel/{token}/` proxy) is reviewed
- [x] Test plan includes supervisor lifecycle unit tests and gateway panel WebSocket E2E tests
- [x] Production Readiness Review (PRR) section is complete

---

## Summary

CubePod's architecture (`docs/architecture.md` §2.2 & §4.2) specifies an on-demand interactive GUI & Web Terminal stack inside sandboxes so AI agents can drive Chromium via Chrome DevTools Protocol (CDP `:9222`) while human users seamlessly view or take over the live browser/desktop (`:8080`) and interactive shell (`ttyd` `:7681`) through CubePod's HMAC-signed `/panel/{token}/` reverse proxy. Today, `pkg/gateway/panel.go` implements token signing and HTTP/WebSocket proxying, but the repository lacks the `images/desktop` workload image, the in-guest lazy supervisor (`cubepod-desktop-ctl`), and the control-plane `POST /sandboxes/{sandboxID}/desktop/start` API.

This CEP delivers:
1. **`images/desktop` Workload Image & Lazy Supervisor (`cubepod-desktop-ctl`)**: An Ubuntu 24.04 OCI image containing `Xvfb` (`:99`), `openbox`, `chromium` / Playwright-ready CDP (`:9222`), `x11vnc` + `noVNC`/`websockify` (`:8080`, zero-UDP iframe-compatible over `/panel/{token}/` plus optional Neko WebRTC hook), and `ttyd` (`:7681`). Nothing starts on idle boot (`0 MiB` GUI RAM overhead for pure CLI tasks); invoking `/opt/cubepod/desktop/cubepod-desktop-ctl start` idempotently boots the stack as unprivileged user `cubeplex` (`1000:1000`) in `< 1.5s`.
2. **Gateway Desktop Lifecycle & Signed Panel Bundle (`POST /sandboxes/{sandboxID}/desktop/start`)**: Idempotently activates the desktop stack via `podd`, registers upstream ports (`8080`, `7681`, `9222`), and returns ready-to-embed signed `/panel/{token}/` URLs for Browser VNC/WebSocket, Web Terminal, and CDP.

---

## Motivation

In CubePlex (`~/cubeplex`), roughly 80% of agent turns execute pure shell/Python commands, while 20% launch browser automation (`playwright` / `browser-use`) or require human-in-the-loop takeover (e.g., logging into an internal SaaS dashboard).
- Eagerly launching X11 + Chromium at container PID 1 consumes `350–600 MiB` RAM on every WarmPool standby and delays boot.
- Conversely, requiring users to manually run shell commands to start Xvfb, window managers, VNC/WebSocket bridges, and `ttyd` creates race conditions, port mismatches, and root-vs-`1000` permission bugs on `/home/cubeplex/.config/chromium`.

### Goals

- Keep idle RAM overhead of `images/desktop` identical to `images/runtime` (`< 25 MiB` inside guest userspace) until `desktop/start` is called or an agent connects to CDP.
- Ensure `podd-init` and `podd` are **never** baked into `images/desktop` (preserving Core Invariant #2: runtime injection at `/opt/cubepod/bin`).
- Run `Xvfb`, `openbox`, `chromium`, `x11vnc`/`websockify`, and `ttyd` strictly as UID/GID `1000:1000` (`cubeplex`) with `HOME=/home/cubeplex` and persistent browser profile optionally symlinked or stored cleanly on OverlayFS upper.
- Provide `POST /sandboxes/{sandboxID}/desktop/start`, `GET /sandboxes/{sandboxID}/desktop/status`, and `POST /sandboxes/{sandboxID}/desktop/stop` on `cubepod-gateway`, returning signed `/panel/{token}/` endpoints that work out-of-the-box inside browser `<iframe>`s (relative asset paths preserved).

### Non-Goals

- GPU hardware passthrough (`vfio-pci`) in Alpha. Rendering uses CPU software rasterization (`--disable-gpu --use-gl=angle --use-angle=swiftshader`), which is deterministic across Kata QEMU/CLH and Docker nodes.
- Audio capture/playback in Alpha.

---

## Proposal

### User Stories

#### Story 1: Lazy Browser Takeover from CubePlex UI
As a CubePlex user whose agent encounters a login screen, I click "Open Browser", which calls `POST /sandboxes/{id}/desktop/start`, receives `{"desktopUrl": "/panel/<tok1>/vnc.html?autoconnect=true", "terminalUrl": "/panel/<tok2>/", "cdpPort": 9222}`, and renders the interactive desktop inside an `<iframe>` within 1.5 seconds.

#### Story 2:Persistent Login Cookies Across Pause/Resume
As an agent user, after logging into an internal portal in Chromium (`1000:1000`) and pausing the sandbox (`0 CPU / 0 RAM`), resuming the sandbox on another node and calling `desktop/start` restores my `/home/cubeplex/.config/chromium` session from `rootfs-pvc` OverlayFS upper (with `/SingletonLock` stale symlinks automatically cleaned by `cubepod-desktop-ctl`).

### Notes / Constraints / Caveats

- **Chromium `SingletonLock` Cleanup After Resume**: When a sandbox is paused (`fsfreeze` + Pod delete) while Chromium is running, `/home/cubeplex/.config/chromium/SingletonLock` (a symlink containing the previous Pod's hostname + PID) persists on `rootfs-pvc`. On resume in a new Pod with a new hostname, Chromium refuses to start unless stale `Singleton*` locks are removed when no `chromium` process is running. `cubepod-desktop-ctl start` explicitly checks `pgrep -u 1000 chromium` and purges stale `SingletonLock` / `SingletonCookie` / `SingletonSocket` before launch.
- **Kata `/dev/shm` Sizing**: Chromium crashes on complex pages if `/dev/shm` is 64MiB. `cubepod-desktop-ctl` passes `--disable-dev-shm-usage` (using `/tmp` on persistent ext4 / tmpfs) so Chromium remains stable without custom host volume mounts.
- **WebSocket Upgrade in `panel.go`**: Go 1.22+ `httputil.ReverseProxy` natively supports HTTP/1.1 `Connection: Upgrade` (`Upgrade: websocket`) bidirectional tunneling, allowing both `noVNC`/`websockify` (`:8080/websockify`) and `ttyd` (`:7681/ws`) to stream over a single HTTPS port on `cubepod-gateway`.

### Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
| :--- | :--- | :--- | :--- |
| Stale Chromium `SingletonLock` on `rootfs-pvc` blocks browser restart after cross-node resume | High | High | `cubepod-desktop-ctl start` atomically removes `Singleton*` files when no live Chromium PID exists. |
| CDP `:9222` exposed externally without auth | High | Low | Chromium binds `:9222` to `127.0.0.1` inside the sandbox (with optional `socat` forwarder only when `exposeCdp=true` via short-lived HMAC `/panel/{token}/`). |
| Concurrent calls to `POST /desktop/start` spawn duplicate Xvfb/Chromium instances | Med | Med | `cubepod-desktop-ctl` acquires an `flock(1)` lock on `/run/cubepod/desktop/ctl.lock` before checking PID files and probing ports `:9222`, `:8080`, `:7681`. |

---

## Design Details

### API / CRD / Wire Changes

#### In-Guest CLI Contract (`/opt/cubepod/desktop/cubepod-desktop-ctl`)

Installed inside `images/desktop` (under `/opt/cubepod/desktop/`, distinct from `/opt/cubepod/bin` where `initContainer` mounts `podd-init`/`podd`!):
- `cubepod-desktop-ctl start [--resolution 1280x800x24] [--url about:blank]`
- `cubepod-desktop-ctl status`
- `cubepod-desktop-ctl stop`

All subcommands emit single-line JSON on `stdout` (`exit 0` when healthy):
```json
{
  "status": "running",
  "display": ":99",
  "resolution": "1280x800x24",
  "ports": {
    "desktop": 8080,
    "terminal": 7681,
    "cdp": 9222
  },
  "pids": {
    "xvfb": 142,
    "wm": 148,
    "chromium": 155,
    "vnc": 169,
    "websockify": 174,
    "ttyd": 180
  }
}
```

#### Gateway HTTP Endpoints (Wave 2 Wiring)

- `POST /sandboxes/{sandboxID}/desktop/start`
  - Request (optional JSON): `{"resolution": "1280x800x24", "initialUrl": "about:blank", "expiresInSeconds": 3600}`
  - Behavior: Auto-resumes sandbox if `Paused`, invokes `/opt/cubepod/desktop/cubepod-desktop-ctl start` via `podd`, registers ports `8080`, `7681`, `9222`, mints signed panel tokens, and returns `200 OK`:
    ```json
    {
      "status": "running",
      "desktopUrl": "/panel/<token-8080>/vnc.html?autoconnect=true&resize=scale",
      "terminalUrl": "/panel/<token-7681>/",
      "cdpUrl": "/panel/<token-9222>/",
      "expiresAt": "2026-11-13T16:30:00Z"
    }
    ```
- `GET /sandboxes/{sandboxID}/desktop/status`
- `POST /sandboxes/{sandboxID}/desktop/stop`

### Architecture & State Machine

```mermaid
flowchart LR
    UI["CubePlex / User Browser"] -->|"POST /sandboxes/{id}/desktop/start"| GW["cubepod-gateway"]
    GW -->|"POST /v1/commands/run (uid=1000)"| Podd["podd (:49983)"]
    Podd --> Ctl["cubepod-desktop-ctl (flock)"]
    Ctl --> Xvfb["Xvfb :99 + Openbox"]
    Xvfb --> Chrome["Chromium (:9222 CDP)"]
    Xvfb --> VNC["x11vnc + noVNC/websockify (:8080)"]
    Ctl --> TTYD["ttyd (:7681 /bin/bash)"]
    UI <-->|"WS /panel/{token}/websockify"| GW
    GW <-->|"ReverseProxy WS"| VNC
```

### Boundary Conditions & Failure Recovery

- **Missing Desktop Stack in Base Image**: If `POST /desktop/start` is called on a minimal `ubuntu:24.04` sandbox that does not have `/opt/cubepod/desktop/cubepod-desktop-ctl`, Gateway returns `422 Unprocessable Entity` `{"error": "desktop_not_installed", "message": "sandbox image does not include cubepod-desktop stack"}`.
- **Process Crash Recovery**: Calling `cubepod-desktop-ctl start` again detects any dead component (e.g., user closed Chromium) and restarts only the missing daemon without tearing down `Xvfb` or `ttyd`.

### Dual-Runtime Behavior (K8s Kata vs. Local Docker)

- **Kubernetes Production (`Kata + CSI Raw Block`)**: Runs inside the Kata MicroVM; `cubepod-gateway` proxies `/panel/{token}/` directly to `<PodIP>:8080` and `<PodIP>:7681` over the cluster network. Notice `/opt/cubepod/bin` (`emptyDir` mount) only shadows `/opt/cubepod/bin`, leaving `/opt/cubepod/desktop` in the image completely intact!
- **Local Development (`Docker + Loopback ext4`)**: Identical behavior; `pkg/runtime` exposes container IP or mapped ephemeral ports registered with `Server.RegisterPort`.

### Test Plan

- **Supervisor Unit / Container Tests (`images/desktop/test_ctl.sh`)**:
  - Verify `flock` concurrency safety, `SingletonLock` stale-symlink cleanup, idempotent `start` -> `status` -> `start` -> `stop` -> `status` JSON output, and `1000:1000` UID ownership.
- **Gateway Integration Tests (`pkg/gateway`)**:
  - Verify `POST /sandboxes/{id}/desktop/start` mints valid `/panel/{token}/` URLs and streams WebSocket frames end-to-end.

### Graduation Criteria

- **Alpha (`v0.5.0`)**: `images/desktop` Dockerfile + `cubepod-desktop-ctl` + Gateway `desktop/{start,status,stop}` merged and verified via WebSocket E2E tests.
- **Beta (`v0.6.0`)**: Optional Neko WebRTC H.264/VP8 transport alongside `noVNC` WebSocket fallback, plus CubePlex Tab-Follow integration (`browser_tab_follow.py`).
- **Stable (`v1.0.0`)**: Verified under 50+ concurrent browser sessions in CubePlex production.

### Upgrade / Downgrade Strategy

- Purely additive image (`ghcr.io/cubeplexai/cubepod-desktop`) and additive Gateway routes. Existing sandboxes and PVCs require zero migration.

---

## Production Readiness Review Questionnaire (PRR)

### Feature Enablement and Rollback
- **How can this feature be enabled / disabled in a live cluster?** Select `cubepod-desktop` in `PodTemplate.spec.image` and enable `DesktopStack` on `cubepod-gateway`.
- **Does disabling or rolling back corrupt `/workspace` or `RootFS` block volumes?** No; `/workspace` is untouched and browser state in `/home/cubeplex/.config` is ordinary user data on `rootfs-pvc`.

### Rollout, Upgrade and Rollback Planning
- Upgrading `images/desktop` updates the read-only `lowerdir` for newly started Pods while preserving existing `/workspace` PVCs.

### Monitoring Requirements
- `cubepod_desktop_activations_total{result="ok|unsupported|error"}` and `cubepod_panel_active_sessions` gauges on `/metrics`.

### Dependencies
- Standard Ubuntu 24.04 packages (`xvfb`, `openbox`, `x11vnc`, `novnc`, `websockify`, `ttyd`, `chromium` / Playwright Chromium).

### Scalability
- Lazy activation ensures 0 MiB GUI memory footprint on idle sandboxes; active desktop sessions require `>= 1Gi` memory limit on the `PodTemplate`.

### Troubleshooting
- Component logs are written inside the sandbox at `/run/cubepod/desktop/{xvfb,chromium,vnc,ttyd}.log`, readable via `cubepod exec` or `GET /files`.

---

## Implementation History

- `2026-11-13`: Initial CEP drafted and marked `implementable` for `v0.5.0`.

---

## Drawbacks

- `images/desktop` compressed OCI size (~450–600 MB with Chromium & fonts) is larger than minimal `sandbox-base` (~60 MB), mitigated by node-level containerd image layer sharing across all MicroVMs on a node.

---

## Alternatives

### Alternative 1: Eagerly Launch `supervisord` as Container Entrypoint on Boot
- **Description**: Start Xorg, Chromium, VNC, and `ttyd` automatically whenever any Pod boots.
- **Reason for Rejection**: Wastes ~500 MiB RAM per WarmPool Pod and slows MicroVM readiness for the 80% of agent sessions that never open a browser.

### Alternative 2: WebRTC-Only Transport (Neko Without WebSocket Fallback)
- **Description**: Use UDP WebRTC exclusively for desktop streaming.
- **Reason for Rejection**: Corporate firewalls and standard K8s L7 Ingresses frequently block arbitrary UDP ports unless TURN servers are deployed; `noVNC` over WebSocket (`/panel/{token}/`) works universally through any HTTPS reverse proxy with zero extra ports, while allowing Neko WebRTC as an optional Beta accelerator.
