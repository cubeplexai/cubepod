# CubePod Interactive Desktop & Browser Stack (`images/desktop`)

Implements **CEP-0021**: an Ubuntu 24.04 sandbox image providing a lazy-activated X11 desktop (`Xvfb` + `openbox`), Playwright-compatible Chromium with loopback CDP (`:9222`), `x11vnc` + `noVNC`/`websockify` (`:8080`), and a writable `ttyd` web terminal (`:7681`).

## Key Invariants

- **Zero Idle Overhead**: No GUI or browser processes start on container boot (`CMD ["sleep", "infinity"]`). Pure CLI/Python agent turns consume ~0 MiB GUI RAM until `/opt/cubepod/desktop/cubepod-desktop-ctl start` is invoked.
- **Runtime Binary Injection (`podd-init` / `podd` Decoupled)**: Neither `podd-init` nor `podd` is baked into this image. All desktop tooling lives under `/opt/cubepod/desktop/` so the `/opt/cubepod/bin` `emptyDir` mount never shadows it.
- **Privilege Separation**: GUI daemons (`Xvfb`, `openbox`, Chromium, `x11vnc`, `websockify`, `ttyd`) drop privileges to unprivileged user `cubeplex` (`1000:1000`) when invoked as `root`, with `ttyd` starting in `/workspace`.
- **Resume-Safe Chromium Locking**: Before launching Chromium, `cubepod-desktop-ctl start` checks whether a live Chromium process owns the profile directory and removes stale `SingletonLock`, `SingletonCookie`, and `SingletonSocket` files left behind by cross-node VM pause/resume.
- **Concurrency & Partial Crash Recovery**: `cubepod-desktop-ctl` serializes calls via `flock` on `${CUBEPOD_DESKTOP_RUN_DIR:-/run/cubepod/desktop}/ctl.lock` (falling back to `/tmp/cubepod-desktop` if `/run` is not writable). Calling `start` restarts only dead daemons while preserving healthy PIDs.

## Build

Run from the repository root:

```bash
docker build -f images/desktop/Dockerfile -t cubepod-desktop:latest .
```

## Supervisor CLI (`/opt/cubepod/desktop/cubepod-desktop-ctl`)

```bash
/opt/cubepod/desktop/cubepod-desktop-ctl start [--resolution 1280x800x24] [--url about:blank]
/opt/cubepod/desktop/cubepod-desktop-ctl status
/opt/cubepod/desktop/cubepod-desktop-ctl stop
```

All subcommands emit single-line JSON on `stdout`:

```json
{"status":"running","display":":99","resolution":"1280x800x24","ports":{"desktop":8080,"terminal":7681,"cdp":9222},"pids":{"xvfb":142,"wm":148,"chromium":155,"vnc":169,"websockify":174,"ttyd":180}}
```

## Tests

```bash
bash -n images/desktop/cubepod-desktop-ctl
bash -n images/desktop/chromium-wrapper
bash images/desktop/test_ctl.sh
```
