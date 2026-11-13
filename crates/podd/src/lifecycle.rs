//! Bounded filesystem quiesce (`sync` + `FIFREEZE`) and monotonic watchdog `thaw` (`FITHAW`)
//! for crash-consistent Copy-on-Write volume forks (CEP-0022).

use std::any::Any;
use std::collections::HashSet;
use std::fs::File;
use std::net::TcpStream;
use std::os::unix::fs::MetadataExt;
use std::os::unix::io::AsRawFd;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::http::{write_json, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::DaemonConfig;

pub const DEFAULT_MAX_FREEZE_MS: u64 = 1500;
pub const MIN_MAX_FREEZE_MS: u64 = 100;
pub const MAX_MAX_FREEZE_MS: u64 = 5000;

/// Linux `FIFREEZE` ioctl request (`_IOWR('X', 119, int)`).
pub const FIFREEZE: libc::c_ulong = 0xC004_5877;
/// Linux `FITHAW` ioctl request (`_IOWR('X', 120, int)`).
pub const FITHAW: libc::c_ulong = 0xC004_5878;

/// Clamps requested freeze duration into `[100, 5000]` ms.
pub fn clamp_max_freeze_ms(ms: u64) -> u64 {
    ms.clamp(MIN_MAX_FREEZE_MS, MAX_MAX_FREEZE_MS)
}

/// Abstracts kernel filesystem sync/freeze/thaw operations for production and unit testing.
pub trait FreezeOps: Send + Sync {
    fn sync_all(&self);
    fn freeze_paths(&self, paths: &[&Path]) -> Vec<Box<dyn Any + Send>>;
    fn thaw_handle(&self, handle: Box<dyn Any + Send>);
}

/// Real Linux `libc::sync`, `libc::syncfs`, and `FIFREEZE`/`FITHAW` `ioctl` implementation.
pub struct RealFreezeOps;

impl FreezeOps for RealFreezeOps {
    fn sync_all(&self) {
        unsafe {
            libc::sync();
        }
    }

    fn freeze_paths(&self, paths: &[&Path]) -> Vec<Box<dyn Any + Send>> {
        let mut seen_devs = HashSet::new();
        let mut frozen: Vec<Box<dyn Any + Send>> = Vec::new();

        for path in paths {
            let Ok(meta) = std::fs::metadata(path) else {
                continue;
            };
            // FIFREEZE operates at superblock granularity; skip distinct paths sharing st_dev.
            if !seen_devs.insert(meta.dev()) {
                continue;
            }
            let Ok(file) = File::open(path) else {
                continue;
            };
            let fd = file.as_raw_fd();
            unsafe {
                let _ = libc::syncfs(fd);
                let rc = libc::ioctl(fd, FIFREEZE as _, 0);
                if rc == 0 {
                    frozen.push(Box::new(file));
                } else {
                    let err = std::io::Error::last_os_error().raw_os_error().unwrap_or(0);
                    // Gracefully skip unprivileged / unsupported filesystems (EPERM, ENOTTY, EINVAL, etc.).
                    match err {
                        libc::EPERM
                        | libc::EACCES
                        | libc::ENOTTY
                        | libc::EINVAL
                        | libc::EOPNOTSUPP
                        | libc::ENOSYS
                        | libc::EBUSY => {}
                        _ => {}
                    }
                }
            }
        }

        frozen
    }

    fn thaw_handle(&self, handle: Box<dyn Any + Send>) {
        if let Ok(file) = handle.downcast::<File>() {
            unsafe {
                let _ = libc::ioctl(file.as_raw_fd(), FITHAW as _, 0);
            }
        }
    }
}

struct QuiesceInner {
    generation: u64,
    frozen_fds: Vec<Box<dyn Any + Send>>,
    frozen: bool,
    auto_thawed_count: u64,
    ops: Arc<dyn FreezeOps>,
}

impl Drop for QuiesceInner {
    fn drop(&mut self) {
        for h in self.frozen_fds.drain(..) {
            self.ops.thaw_handle(h);
        }
        self.frozen = false;
    }
}

/// Outcome returned by [`QuiesceState::quiesce`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct QuiesceOutcome {
    pub synced: bool,
    pub frozen: bool,
    pub max_freeze_ms: u64,
}

/// Tracks active frozen filesystem handles and enforces bounded monotonic auto-thaw.
#[derive(Clone)]
pub struct QuiesceState {
    inner: Arc<Mutex<QuiesceInner>>,
    ops: Arc<dyn FreezeOps>,
}

impl Default for QuiesceState {
    fn default() -> Self {
        Self::new()
    }
}

impl QuiesceState {
    pub fn new() -> Self {
        Self::with_ops(Arc::new(RealFreezeOps))
    }

    pub fn with_ops(ops: Arc<dyn FreezeOps>) -> Self {
        Self {
            inner: Arc::new(Mutex::new(QuiesceInner {
                generation: 0,
                frozen_fds: Vec::new(),
                frozen: false,
                auto_thawed_count: 0,
                ops: Arc::clone(&ops),
            })),
            ops,
        }
    }

    /// Flushes dirty buffers, attempts `FIFREEZE` on `workdir` and `/` (if distinct),
    /// and spawns a monotonic watchdog thread that guarantees `FITHAW` after `max_freeze_ms`.
    pub fn quiesce(&self, workdir: &str, max_freeze_ms: u64) -> QuiesceOutcome {
        let clamped_ms = clamp_max_freeze_ms(max_freeze_ms);

        let (frozen, gen) = {
            let mut guard = self.inner.lock().unwrap();
            // Thaw any prior frozen fds before re-freezing so handles never leak.
            for h in guard.frozen_fds.drain(..) {
                self.ops.thaw_handle(h);
            }
            guard.frozen = false;
            guard.generation += 1;

            self.ops.sync_all();

            let work_path = Path::new(workdir);
            let root_path = Path::new("/");
            let fds = if work_path.exists() {
                self.ops.freeze_paths(&[work_path, root_path])
            } else {
                self.ops.freeze_paths(&[root_path])
            };

            let is_frozen = !fds.is_empty();
            guard.frozen_fds = fds;
            guard.frozen = is_frozen;
            (is_frozen, guard.generation)
        };

        if frozen {
            let inner_clone = Arc::clone(&self.inner);
            let ops_clone = Arc::clone(&self.ops);
            std::thread::spawn(move || {
                let start = Instant::now();
                let target = Duration::from_millis(clamped_ms);
                while let Some(rem) = target.checked_sub(start.elapsed()) {
                    if rem.is_zero() {
                        break;
                    }
                    std::thread::sleep(rem);
                }

                let mut guard = inner_clone.lock().unwrap();
                if guard.generation == gen && !guard.frozen_fds.is_empty() {
                    for h in guard.frozen_fds.drain(..) {
                        ops_clone.thaw_handle(h);
                    }
                    guard.frozen = false;
                    guard.auto_thawed_count += 1;
                }
            });
        }

        QuiesceOutcome {
            synced: true,
            frozen,
            max_freeze_ms: clamped_ms,
        }
    }

    /// Immediately invokes `FITHAW` on all frozen handles; idempotent when already thawed.
    pub fn thaw(&self) -> usize {
        let mut guard = self.inner.lock().unwrap();
        guard.generation += 1;
        let count = guard.frozen_fds.len();
        for h in guard.frozen_fds.drain(..) {
            self.ops.thaw_handle(h);
        }
        guard.frozen = false;
        count
    }

    pub fn is_frozen(&self) -> bool {
        self.inner.lock().unwrap().frozen
    }

    pub fn auto_thawed_count(&self) -> u64 {
        self.inner.lock().unwrap().auto_thawed_count
    }
}

/// Handles `POST /v1/lifecycle/quiesce`.
pub fn handle_quiesce(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    quiesce: &QuiesceState,
) {
    let mut max_freeze_ms = DEFAULT_MAX_FREEZE_MS;

    if !req.body.is_empty() {
        let body_str = match std::str::from_utf8(&req.body) {
            Ok(s) => s.trim(),
            Err(_) => {
                write_json(
                    stream,
                    "400 Bad Request",
                    r#"{"error":"invalid_utf8_body"}"#,
                );
                return;
            }
        };
        if !body_str.is_empty() {
            let json = match JsonValue::parse(body_str) {
                Ok(j) => j,
                Err(e) => {
                    let resp = format!(
                        r#"{{"error":"invalid_json","details":"{}"}}"#,
                        escape_json(&e)
                    );
                    write_json(stream, "400 Bad Request", &resp);
                    return;
                }
            };
            if let Some(val) = json
                .get("maxFreezeMs")
                .or_else(|| json.get("max_freeze_ms"))
            {
                if let Some(n) = val.as_i64() {
                    max_freeze_ms = if n <= 0 { MIN_MAX_FREEZE_MS } else { n as u64 };
                }
            }
        }
    }

    let outcome = quiesce.quiesce(&config.workdir, max_freeze_ms);
    let body = format!(
        r#"{{"status":"quiesced","synced":{},"frozen":{},"maxFreezeMs":{}}}"#,
        outcome.synced, outcome.frozen, outcome.max_freeze_ms
    );
    write_json(stream, "200 OK", &body);
}

/// Handles `POST /v1/lifecycle/thaw`.
pub fn handle_thaw(stream: &mut TcpStream, quiesce: &QuiesceState) {
    let _ = quiesce.thaw();
    write_json(stream, "200 OK", r#"{"status":"thawed"}"#);
}
