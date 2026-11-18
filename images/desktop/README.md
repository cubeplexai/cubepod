# CubePod Interactive Desktop & Browser Stack (`images/desktop`)

Implements **CEP-0021**: an Ubuntu 24.04 sandbox image providing a lazy-activated X11 desktop (`Xvfb` + `openbox`), Playwright-compatible Chromium with loopback CDP (`:9222`), `x11vnc` + `noVNC`/`websockify` (`:8080`), Neko WebRTC streaming (`:8081`), CubePlex Tab-Follow (`browser_tab_follow.py`), and a writable `ttyd` web terminal (`:7681`).

## Key Invariants

- **Zero Idle Overhead**: No GUI or browser processes start on container boot (`CMD ["sleep", "infinity"]`). Pure CLI/Python agent turns consume ~0 MiB GUI RAM until `/opt/cubepod/desktop/cubepod-desktop-ctl start` is invoked.
- **Runtime Binary Injection (`podd-init` / `podd` Decoupled)**: Neither `podd-init` nor `podd` is baked into this image. All desktop tooling lives under `/opt/cubepod/desktop/` so the `/opt/cubepod/bin` `emptyDir` mount never shadows it.
- **Privilege Separation**: GUI daemons (`Xvfb`, `openbox`, Chromium, `x11vnc`, `websockify`, `ttyd`, `neko`) drop privileges to unprivileged user `cubeplex` (`1000:1000`) when invoked as `root`, with `ttyd` starting in `/workspace`.
- **Resume-Safe Chromium Locking**: Before launching Chromium, `cubepod-desktop-ctl start` checks whether a live Chromium process owns the profile directory and removes stale `SingletonLock`, `SingletonCookie`, and `SingletonSocket` files left behind by cross-node VM pause/resume.
- **Concurrency & Partial Crash Recovery**: `cubepod-desktop-ctl` serializes calls via `flock` on `${CUBEPOD_DESKTOP_RUN_DIR:-/run/cubepod/desktop}/ctl.lock` (falling back to `/tmp/cubepod-desktop` if `/run` is not writable). Calling `start` restarts only dead daemons while preserving healthy PIDs.

## Build

Run from the repository root:

```bash
docker build -f images/desktop/Dockerfile -t cubepod-desktop:latest .
```

The Dockerfile copies `/usr/bin/neko` (`-> /usr/local/bin/neko`) and `/var/www` (`-> /opt/cubepod/desktop/neko-www`) from the multi-arch `ghcr.io/m1k1o/neko/base:latest` stage (`ARG NEKO_IMAGE`) and installs GStreamer/Opus/VP8/H.264/PulseAudio runtime libraries in `ubuntu:24.04`.

## Supervisor CLI (`/opt/cubepod/desktop/cubepod-desktop-ctl`)

```bash
/opt/cubepod/desktop/cubepod-desktop-ctl start [--resolution 1280x800x24] [--url about:blank] [--transport novnc|webrtc]
/opt/cubepod/desktop/cubepod-desktop-ctl status
/opt/cubepod/desktop/cubepod-desktop-ctl stop
```

When `--transport webrtc` is requested, `cubepod-desktop-ctl` checks `CUBEPOD_NEKO_BIN` (default `/usr/local/bin/neko`) and launches Neko on `CUBEPOD_WEBRTC_PORT` (default `8081`) with `NEKO_STATIC="${CUBEPOD_NEKO_STATIC:-/opt/cubepod/desktop/neko-www}"` and `NEKO_ICELITE="${CUBEPOD_NEKO_ICELITE:-1}"` alongside `noVNC` (`transportUsed: "webrtc"`). If Neko is absent or exits prematurely on startup, `cubepod-desktop-ctl` preserves the rest of the desktop stack and falls back cleanly to `noVNC` (`transportUsed: "novnc"`, `fallbackReason: "neko_not_installed"` or `"neko_start_failed"`).

All subcommands emit single-line JSON on `stdout`:

```json
{"status":"running","display":":99","resolution":"1280x800x24","transportUsed":"webrtc","ports":{"desktop":8080,"terminal":7681,"cdp":9222,"webrtc":8081},"pids":{"xvfb":142,"wm":148,"chromium":155,"vnc":169,"websockify":174,"ttyd":180,"neko":186}}
```

## CubePlex Tab-Follow Helper (`/opt/cubepod/desktop/browser_tab_follow.py`)

Installed at `/opt/cubepod/desktop/browser_tab_follow.py` and symlinked at `/usr/local/bin/cubeplex-focus-browser-tab` and `/usr/local/bin/cubepod-focus-browser-tab`:

- Queries `http://127.0.0.1:${CUBEPOD_CDP_PORT:-9222}/json` for `"type": "page"` targets.
- Resolves the active agent URL via `--url <url>` or `$AGENT_BROWSER_REAL get url` (guarded by `AGENT_BROWSER_WRAPPER=1` against recursion).
- Performs exact normalized URL matching (trailing `/` ignored, never prefix-matching) and activates the matching tab via `http://127.0.0.1:${CDP_PORT}/json/activate/<id>`.
- Supports `--install-wrapper` to idempotently wrap `/usr/local/bin/agent-browser` (`# CUBEPLEX_TAB_FOLLOW=1`, preserving `agent-browser.real`), invoked automatically and non-fatally during `cubepod-desktop-ctl start` when `agent-browser` is present and writable.

## Tests

```bash
bash -n images/desktop/cubepod-desktop-ctl
bash -n images/desktop/chromium-wrapper
bash images/desktop/test_ctl.sh
```
