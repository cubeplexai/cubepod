---
title: In-Guest Runtime (podd-init & podd)
description: How CubePod injects zero-contamination Rust binaries into stock OCI images, pivots root safely as a single-threaded PID 1, and serves execution, files, quiescing, and code kernels on port 49983.
---

To support arbitrary user Docker images without rebuilding base images or polluting the user's writable RootFS layer, CubePod splits its in-guest runtime into a dedicated injector image (`images/podd/Dockerfile`) and two statically linked Rust binaries: **`podd-init`** (`crates/podd-init/`) and **`podd`** (`crates/podd/`).

## Zero-Contamination Binary Injection

**User sandbox images must never contain `podd-init` or `podd`.** Stock images such as `ubuntu:24.04`, `python:3.12`, or custom desktop/ML images remain completely untouched:

1. `images/podd/Dockerfile` packages compiled `podd-init` and `podd` into a `busybox:1.37-musl` image under `/opt/cubepod/dist/`.
2. **In Kubernetes**: an `initContainer` named `podd-installer` copies `/opt/cubepod/dist/{podd-init,podd}` into a shared Pod `emptyDir` mounted at `/opt/cubepod/bin`.
3. **In Local Docker Loopback**: `pkg/runtime/docker.go` bind-mounts the host binary directory read-only (`-v <binDir>:/opt/cubepod/bin:ro`) and sets `--entrypoint /opt/cubepod/bin/podd-init`.
4. **Across `pivot_root`**: `podd-init` recursively bind-mounts `/opt/cubepod` from the initial root into the newly pivoted merged root before detaching `.old_root`.

Because `/opt/cubepod` lives purely in the ephemeral `emptyDir` / read-only mount rather than the persistent `upperdir`, upgrading CubePod's runtime binaries on Pod resume never leaves stale runtime artifacts inside the user's persistent RootFS volume.

## Why `podd-init` Is Single-Threaded Rust

`podd-init` (`crates/podd-init/src/main.rs`) runs as **PID 1** inside the container/MicroVM prior to `pivot_root(2)` and `execvp(3)`:

- Linux `pivot_root(2)` and mount-namespace manipulations fail (`EINVAL`) or leak stale filesystem references if multiple OS threads exist inside the calling process.
- Go binaries spawn OS runtime scheduler/sysmon threads before entering `main()`, making pure-Go PID 1 `pivot_root` unsafe.
- Writing `podd-init` as a tiny, single-threaded, zero-GC Rust binary guarantees deterministic mount setup, filesystem healing, root pivoting, and clean `execvp` handoff into `podd`.

## PID 1 Boot Sequence (`crates/podd-init/src/main.rs`)

At container start, `podd-init` reads environment configuration (`CUBEPOD_ROOTFS_DEV` default `/dev/cubepod-rootfs`, `CUBEPOD_WORKSPACE_DEV` default `/dev/cubepod-workspace`, `CUBEPOD_UPPER_MOUNT` default `/mnt/cubepod-upper`, `CUBEPOD_MERGED_ROOT` default `/mnt/cubepod-merged`, `CUBEPOD_UID`/`CUBEPOD_GID` default `1000`, `CUBEPOD_INIT_STATE_PATH` default `/run/cubepod/init-state.json`, and `CUBEPOD_SKIP_PIVOT`) and executes:

1. **Private Mount Propagation**: Calls `make_mount_private("/")` (`MS_PRIVATE | MS_REC`) so mounts inside the guest never propagate back out.
2. **Fallback Check (`volumeMode: Filesystem`)**: If `CUBEPOD_SKIP_PIVOT=1` or the configured block devices do not exist, `podd-init` skips formatting and pivoting, writes `pivoted: false` to `/run/cubepod/init-state.json`, ensures `/workspace` ownership (`1000:1000`), and launches `podd` immediately.
3. **Self-Healing `ext4` Preparation (`ensure_ext4`)**:
   - Probes `/dev/cubepod-rootfs` and `/dev/cubepod-workspace` with `blkid`.
   - **First boot (blank block device)**: Formats with `mkfs.ext4 -F -L <label> -m 0 -E lazy_itable_init=0,lazy_journal_init=0` so metadata is synchronously initialized without post-boot background stall.
   - **Subsequent boots or forked volumes**: Runs non-interactive `e2fsck -p`, retrying up to **10 times with 500 ms backoff** if the kernel returns exit code `8` (`EBUSY` right after device attach).
4. **RootFS `OverlayFS` Mount**: Mounts the RootFS `ext4` partition at `/mnt/cubepod-upper` (`-o noatime`), ensures `upper/` and `work/` directories exist, and mounts Linux `overlay` (`lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work`) onto `/mnt/cubepod-merged`.
5. **Workspace Mount & Unprivileged Ownership**: Mounts `/dev/cubepod-workspace` inside `<merged>/workspace` and applies `chown 1000:1000` (`cubeplex`) so agent commands can read/write immediately without follow-up `chown` hacks.
6. **Virtual FS & Config Carry-Over**: Recursively bind-mounts (`--rbind`) `/proc`, `/sys`, `/dev`, `/run`, and `/opt/cubepod`, and bind-mounts (`/etc/resolv.conf`, `/etc/hosts`, `/etc/hostname`) into `<merged>`.
7. **Atomic `pivot_root` & Old Root Detach**: Transitions the root filesystem via `pivot_root` into `.old_root` (with `chroot` fallback if `pivot_root` is restricted), then calls `umount2(MNT_DETACH)` on `/.old_root`.
8. **State Record & Handoff**: Writes `/run/cubepod/init-state.json` (`pivoted: true`, device/mount diagnostics) and `execvp`s into `/opt/cubepod/bin/podd`, replacing itself while retaining PID 1.

## The `podd` Execution & Control Daemon (`:49983`)

Once `podd` (`crates/podd/src/main.rs`) takes over PID 1:

- **Subreaper & Zombie Collection**: Installs a zombie reaper (`install_zombie_reaper`) so orphaned agent subprocesses are cleanly reaped.
- **Bootstrap Command & Privilege Separation**: Optionally launches `CUBEPOD_START_CMD` in the background (for template services) while running agent commands and file writes as unprivileged UID/GID `1000:1000` (`CUBEPOD_RUN_AS_USER`, `CUBEPOD_RUN_AS_GROUP`, `CUBEPOD_RUN_AS_USERNAME`).

`podd` exposes an HTTP/1.1 + Connect-RPC listener on **`:49983`** consumed exclusively by `cubepod-gateway` and `cubepod-controller`:

### Lifecycle & Crash-Consistent Quiescing (`crates/podd/src/lifecycle.rs`)

| Endpoint | Kernel Syscalls & Safety Mechanisms | Purpose |
| :--- | :--- | :--- |
| `GET /health` | Process readiness check | Polled by `cubepod-gateway` (`waitForPoddReady`) after cold start or resume. |
| `POST /v1/lifecycle/prepare-stop` | `libc::sync()` | Flushes dirty page cache prior to clean shutdown or pause. |
| `POST /v1/lifecycle/quiesce` | `sync` + `syncfs` + `FIFREEZE` (`0xC0045877`) | Freezes guest filesystems cleanly before CSI snapshot / `reflink` fork. Accepts `maxFreezeMs` (default `1500` ms, clamped to `[100, 5000]` ms) backed by a monotonic watchdog timer that auto-thaws even if the controller crashes mid-fork. |
| `POST /v1/lifecycle/thaw` | `FITHAW` (`0xC0045878`) | Immediately unfreezes guest filesystems once the snapshot/clone finishes. |

### Command Execution & Process Control (`crates/podd/src/commands.rs`)

- **REST Execution**: `POST /v1/commands/run` (foreground or background), `GET /v1/commands/{id}/status`, `GET /v1/commands/{id}/logs?cursor=N` (byte-offset cursor streaming without re-sending earlier output), and `POST /v1/commands/{id}/kill` (terminates the full POSIX process group via negative `PGID`, eliminating zombie children).
- **Connect-RPC Compatibility**: Serves `/process.Process/Start` and `/process.Process/SendSignal` for E2B SDK interoperability.

### Files & High-Throughput `archive-sync` (`crates/podd/src/files.rs`)

- **Single-File REST**: `PUT /v1/files`, `GET /v1/files`, `DELETE /v1/files`, `GET /v1/files/list` with UID/GID metadata handling.
- **Streaming `POST /v1/files/archive-sync`**: Accepts raw `.tar` or `.tar.gz` archives and extracts directly inside the guest using verified path-traversal guards (rejects `..` components and leading `/`), applying `dest`, `uid`/`gid` (`1000:1000`), and optional `remove_paths` deletion — syncing thousands of project files in a single round-trip.

### Multi-Language Stateful Code Interpreter (`crates/podd/src/interpreter.rs`)

Serves `/execute`, `/v1/code/execute`, and `/contexts` with **6 persistent execution kernels**:

- `python`
- `bash`
- `javascript`
- `typescript`
- `r`
- `java`

Outputs stream back as NDJSON events (`stdout`, `stderr`, `result`, `error`) preserving variables and working state across repeated executions inside the same context.

## Next Steps

- See how `podd`'s `FIFREEZE`/`FITHAW` hooks power 1-second forks in **[Lifecycle, WarmPool, CoW Fork & HA](/concepts/lifecycle-warmpool-fork/)**.
- Inspect the [`crates/podd-init/`](https://github.com/cubeplexai/cubepod/tree/main/crates/podd-init) and [`crates/podd/`](https://github.com/cubeplexai/cubepod/tree/main/crates/podd) source trees or read **[CEP-0001](/ceps/0001-in-guest-overlayfs-pivot/)** and **[CEP-0002](/ceps/0002-podd-execution-daemon/)**.
