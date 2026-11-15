//! Inotify directory watches for the official Filesystem watcher RPCs.
//!
//! `CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` is the unary poll API.
//! `WatchDir` is the server stream. Both use the same inotify watcher.
//!
//! Limits:
//! - Connect replays nothing. `GetWatcherEvents` keeps a ring of at most
//!   [`WATCH_RING_CAP`] events. Overflow does not return that truncated ring as
//!   success: the watcher is marked and both `GetWatcherEvents` and live
//!   `WatchDir` consumers fail with `resource_exhausted`.
//! - `WatchDir` subscribers use a bounded queue. A slow consumer is failed with
//!   `resource_exhausted` and is not sent a silently truncated success stream.
//! - `RENAME` is emitted separately for `MOVED_FROM` and `MOVED_TO` (the proto
//!   event has no old path). Names are relative to the watched directory.
//! - Recursive new-subdirectory race (Linux inotify, not closed): a directory
//!   created under a recursive watch is armed only after that create event is
//!   handled and `inotify_add_watch` returns. The kernel does not queue events
//!   for a watch that is not installed yet, so a file that appears in the new
//!   directory before `inotify_add_watch` returns is missed. A retry loop still
//!   races. This code does not scan the new directory to invent those events.
//! - Kernel `IN_Q_OVERFLOW` means events were dropped before userspace saw them.
//!   The watcher is failed with `resource_exhausted` instead of ignoring the flag.
//! - Symlink directories are not recursed into.
//! - Network/FUSE detection uses `statfs` magic (nfs, cifs, smb, fuse, 9p, ceph).
//!   Unknown types are treated as local. `proc`, `sysfs`, and `devpts` are always
//!   refused, as are `/proc`, `/dev`, and `/sys`.

use std::collections::{HashMap, VecDeque};
use std::ffi::CString;
use std::io::Write;
use std::net::TcpStream;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::io::AsRawFd;
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, RecvTimeoutError, SyncSender, TrySendError};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::commands::{
    make_connect_frame, unpack_connect_payload, write_unary_connect_response, ProcessRegistry,
};
use crate::http::{write_json, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::DaemonConfig;

pub const WATCH_RING_CAP: usize = 4096;
pub const WATCH_SUB_QUEUE: usize = 64;
pub const MAX_WATCHERS_PER_SANDBOX: usize = 16;
pub const POLL_WATCHER_IDLE_TTL: Duration = Duration::from_secs(300);
const WATCH_KEEPALIVE_INTERVAL: Duration = Duration::from_secs(15);
pub const WATCH_BUFFER_OVERFLOW_MSG: &str = "watch event buffer overflow: events were dropped";
pub const WATCH_QUEUE_OVERFLOW_MSG: &str = "watch subscriber queue overflow";

const PROC_SUPER_MAGIC: u32 = 0x0000_9fa0;
const SYSFS_MAGIC: u32 = 0x6265_6572;
const DEVPTS_SUPER_MAGIC: u32 = 0x0000_1cd1;
const NFS_SUPER_MAGIC: u32 = 0x0000_6969;
const CIFS_MAGIC: u32 = 0xff53_4d42;
const SMB_SUPER_MAGIC: u32 = 0x0000_0517b;
const SMB2_MAGIC: u32 = 0xfe53_4d42;
const FUSE_SUPER_MAGIC: u32 = 0x6573_5546;
const NINEP_MAGIC: u32 = 0x0102_1997;
const CEPH_MAGIC: u32 = 0x00c3_6400;

const WATCH_MASK: u32 = libc::IN_CREATE
    | libc::IN_MODIFY
    | libc::IN_DELETE
    | libc::IN_DELETE_SELF
    | libc::IN_MOVED_FROM
    | libc::IN_MOVED_TO
    | libc::IN_ATTRIB
    | libc::IN_ONLYDIR;

#[derive(Clone, Debug)]
struct FsEvent {
    name: String,
    kind: String,
    entry: Option<String>,
}

impl FsEvent {
    fn to_json(&self) -> String {
        match &self.entry {
            Some(entry) => format!(
                r#"{{"name":"{}","type":"{}","entry":{}}}"#,
                escape_json(&self.name),
                escape_json(&self.kind),
                entry
            ),
            None => format!(
                r#"{{"name":"{}","type":"{}"}}"#,
                escape_json(&self.name),
                escape_json(&self.kind)
            ),
        }
    }
}

enum WatchMsg {
    Event(FsEvent),
}

struct WatchSub {
    tx: SyncSender<WatchMsg>,
    overflow: Arc<AtomicBool>,
    message: Arc<Mutex<String>>,
}

pub(crate) struct DirWatcher {
    id: String,
    root: PathBuf,
    recursive: bool,
    include_entry: bool,
    poll: bool,
    fd: AtomicI32,
    stop_fd: AtomicI32,
    watches: Mutex<HashMap<i32, String>>,
    pending: Mutex<VecDeque<FsEvent>>,
    subscribers: Mutex<Vec<WatchSub>>,
    stopped: AtomicBool,
    overflowed: AtomicBool,
    last_poll: Mutex<std::time::Instant>,
}

impl Drop for DirWatcher {
    fn drop(&mut self) {
        let stop_fd = self.stop_fd.swap(-1, Ordering::SeqCst);
        if stop_fd >= 0 {
            unsafe {
                libc::close(stop_fd);
            }
        }
        // Normally loop_events owns and closes fd; this covers early error paths
        // before loop_events is spawned.
        let fd = self.fd.swap(-1, Ordering::SeqCst);
        if fd >= 0 {
            unsafe {
                libc::close(fd);
            }
        }
    }
}

pub struct WatchSubHandle {
    rx: Receiver<WatchMsg>,
    overflow: Arc<AtomicBool>,
    message: Arc<Mutex<String>>,
}

#[derive(Clone)]
pub struct WatchRegistry {
    inner: Arc<Mutex<HashMap<String, Arc<DirWatcher>>>>,
    counter: Arc<AtomicU64>,
    max_watchers: usize,
}

#[derive(Debug)]
pub enum WatchPathError {
    NotFound(String),
    Invalid(String),
    AlreadyExists(String),
    Exhausted(String),
}

fn inotify_io_error(context: &str, err: std::io::Error) -> WatchPathError {
    if matches!(
        err.raw_os_error(),
        Some(libc::ENOSPC | libc::EMFILE | libc::ENFILE | libc::ENOMEM)
    ) {
        WatchPathError::Exhausted(format!("{context}: {err}"))
    } else {
        WatchPathError::Invalid(format!("{context}: {err}"))
    }
}

impl WatchRegistry {
    pub fn new() -> Self {
        Self {
            inner: Arc::new(Mutex::new(HashMap::new())),
            counter: Arc::new(AtomicU64::new(1)),
            max_watchers: MAX_WATCHERS_PER_SANDBOX,
        }
    }

    #[cfg(test)]
    pub fn with_max_watchers(max_watchers: usize) -> Self {
        Self {
            inner: Arc::new(Mutex::new(HashMap::new())),
            counter: Arc::new(AtomicU64::new(1)),
            max_watchers,
        }
    }

    pub fn active_count(&self) -> usize {
        self.inner.lock().unwrap().len()
    }

    pub fn open_fd_count(&self) -> usize {
        self.inner
            .lock()
            .unwrap()
            .values()
            .filter(|w| w.fd.load(Ordering::SeqCst) >= 0)
            .count()
    }

    pub(crate) fn get(&self, id: &str) -> Option<Arc<DirWatcher>> {
        self.reap_expired(POLL_WATCHER_IDLE_TTL);
        self.inner.lock().unwrap().get(id).cloned()
    }

    pub fn reap_expired(&self, ttl: Duration) -> usize {
        let now = std::time::Instant::now();
        let mut expired = Vec::new();
        {
            let mut map = self.inner.lock().unwrap();
            map.retain(|_, w| {
                if w.poll {
                    let last = *w.last_poll.lock().unwrap();
                    if now.duration_since(last) >= ttl {
                        expired.push(Arc::clone(w));
                        return false;
                    }
                }
                true
            });
        }
        let count = expired.len();
        for w in expired {
            w.stop();
        }
        count
    }

    pub fn remove(&self, id: &str) -> bool {
        let removed = self.inner.lock().unwrap().remove(id);
        if let Some(w) = removed {
            w.stop();
            true
        } else {
            false
        }
    }

    pub fn stop_all(&self) {
        let mut map = self.inner.lock().unwrap();
        for w in map.values() {
            w.stop();
        }
        map.clear();
    }

    pub(crate) fn create(
        &self,
        workdir: &str,
        path: &str,
        recursive: bool,
        include_entry: bool,
        allow_network: bool,
        poll: bool,
    ) -> Result<Arc<DirWatcher>, WatchPathError> {
        self.reap_expired(POLL_WATCHER_IDLE_TTL);
        if self.inner.lock().unwrap().len() >= self.max_watchers {
            return Err(WatchPathError::Exhausted("too many active watchers".into()));
        }
        let root = resolve_watch_path(workdir, path)?;
        check_mount(&root, allow_network)?;
        let fd = unsafe { libc::inotify_init1(libc::IN_CLOEXEC | libc::IN_NONBLOCK) };
        if fd < 0 {
            return Err(inotify_io_error(
                "inotify_init1",
                std::io::Error::last_os_error(),
            ));
        }
        let stop_fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        if stop_fd < 0 {
            let err = std::io::Error::last_os_error();
            unsafe {
                libc::close(fd);
            }
            return Err(inotify_io_error("eventfd", err));
        }
        let id = format!("w{}", self.counter.fetch_add(1, Ordering::SeqCst));
        let watcher = Arc::new(DirWatcher {
            id: id.clone(),
            root: root.clone(),
            recursive,
            include_entry,
            poll,
            fd: AtomicI32::new(fd),
            stop_fd: AtomicI32::new(stop_fd),
            watches: Mutex::new(HashMap::new()),
            pending: Mutex::new(VecDeque::new()),
            subscribers: Mutex::new(Vec::new()),
            stopped: AtomicBool::new(false),
            overflowed: AtomicBool::new(false),
            last_poll: Mutex::new(std::time::Instant::now()),
        });
        if let Err(e) = watcher.arm() {
            watcher.stop();
            return Err(e);
        }
        self.inner.lock().unwrap().insert(id, Arc::clone(&watcher));
        let reader = Arc::clone(&watcher);
        std::thread::spawn(move || reader.loop_events());
        Ok(watcher)
    }
}

impl Default for WatchRegistry {
    fn default() -> Self {
        Self::new()
    }
}

impl DirWatcher {
    fn stop(&self) {
        if self.stopped.swap(true, Ordering::SeqCst) {
            return;
        }
        // Wake loop_events via eventfd; loop_events owns and closes self.fd
        // so it can never be closed while poll/read is using the fd number.
        let stop_fd = self.stop_fd.load(Ordering::SeqCst);
        if stop_fd >= 0 {
            let val: u64 = 1;
            let bytes = val.to_ne_bytes();
            unsafe {
                libc::write(stop_fd, bytes.as_ptr() as *const libc::c_void, bytes.len());
            }
        }
        self.subscribers.lock().unwrap().clear();
    }

    fn arm(&self) -> Result<(), WatchPathError> {
        self.add_watch(&self.root, "")
            .map_err(|e| inotify_io_error("inotify_add_watch", e))?;
        if self.recursive {
            self.walk(&self.root, "")?;
        }
        Ok(())
    }

    fn walk(&self, dir: &Path, rel: &str) -> Result<(), WatchPathError> {
        let entries = match std::fs::read_dir(dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::PermissionDenied => return Ok(()),
            Err(e) => return Err(inotify_io_error("read_dir", e)),
        };
        for ent in entries.flatten() {
            let meta = match std::fs::symlink_metadata(ent.path()) {
                Ok(m) => m,
                Err(_) => continue,
            };
            if meta.file_type().is_symlink() || !meta.is_dir() {
                continue;
            }
            let name = ent.file_name();
            let name = name.to_string_lossy();
            let child_rel = join_rel(rel, &name);
            let full = ent.path();
            match self.add_watch(&full, &child_rel) {
                Ok(()) => {}
                Err(e) if e.kind() == std::io::ErrorKind::PermissionDenied => continue,
                Err(e) => return Err(inotify_io_error("inotify_add_watch", e)),
            }
            self.walk(&full, &child_rel)?;
        }
        Ok(())
    }

    fn add_watch(&self, dir: &Path, rel: &str) -> Result<(), std::io::Error> {
        let fd = self.fd.load(Ordering::SeqCst);
        if fd < 0 {
            return Err(std::io::Error::other("watcher stopped"));
        }
        let c = CString::new(dir.as_os_str().as_bytes()).map_err(|_| {
            std::io::Error::new(std::io::ErrorKind::InvalidInput, "path contains nul")
        })?;
        let wd = unsafe { libc::inotify_add_watch(fd, c.as_ptr(), WATCH_MASK) };
        if wd < 0 {
            return Err(std::io::Error::last_os_error());
        }
        self.watches.lock().unwrap().insert(wd, rel.to_string());
        Ok(())
    }

    fn subscribe(&self) -> WatchSubHandle {
        let (tx, rx) = sync_channel(WATCH_SUB_QUEUE);
        let overflow = Arc::new(AtomicBool::new(false));
        let message = Arc::new(Mutex::new(String::new()));
        self.subscribers.lock().unwrap().push(WatchSub {
            tx,
            overflow: Arc::clone(&overflow),
            message: Arc::clone(&message),
        });
        WatchSubHandle {
            rx,
            overflow,
            message,
        }
    }

    fn fail_subscribers(&self, message: &str) {
        let mut subs = self.subscribers.lock().unwrap();
        for sub in subs.drain(..) {
            sub.overflow.store(true, Ordering::SeqCst);
            *sub.message.lock().unwrap() = message.to_string();
        }
    }

    /// Mark the watcher tainted and fail every live WatchDir consumer.
    /// Holds the ring lock while the flag is set so GetWatcherEvents cannot
    /// observe a successful truncated drain.
    fn mark_overflow(&self, message: &str) {
        {
            let _pending = self.pending.lock().unwrap();
            self.overflowed.store(true, Ordering::SeqCst);
        }
        self.fail_subscribers(message);
    }

    fn publish(&self, ev: FsEvent) {
        if self.stopped.load(Ordering::SeqCst) {
            return;
        }
        if self.poll {
            let ring_overflow = {
                let mut pending = self.pending.lock().unwrap();
                if self.overflowed.load(Ordering::SeqCst) {
                    return;
                }
                if pending.len() >= WATCH_RING_CAP {
                    // Bound memory. The dropped event is not returned as success.
                    pending.pop_front();
                    self.overflowed.store(true, Ordering::SeqCst);
                    true
                } else {
                    pending.push_back(ev.clone());
                    false
                }
            };
            if ring_overflow {
                self.fail_subscribers(WATCH_BUFFER_OVERFLOW_MSG);
                return;
            }
        } else if self.overflowed.load(Ordering::SeqCst) {
            return;
        }
        let mut subs = self.subscribers.lock().unwrap();
        subs.retain(|sub| match sub.tx.try_send(WatchMsg::Event(ev.clone())) {
            Ok(()) => true,
            Err(TrySendError::Full(_)) => {
                sub.overflow.store(true, Ordering::SeqCst);
                *sub.message.lock().unwrap() = WATCH_QUEUE_OVERFLOW_MSG.to_string();
                false
            }
            Err(TrySendError::Disconnected(_)) => false,
        });
    }

    /// Events since the last successful poll. `Err` means the ring or the
    /// kernel queue overflowed and the events that remain are incomplete.
    fn poll_events(&self) -> Result<Vec<FsEvent>, ()> {
        *self.last_poll.lock().unwrap() = std::time::Instant::now();
        let mut pending = self.pending.lock().unwrap();
        if self.overflowed.load(Ordering::SeqCst) {
            return Err(());
        }
        Ok(pending.drain(..).collect())
    }

    #[cfg(test)]
    pub(crate) fn publish_test_event(&self, name: &str) {
        self.publish(FsEvent {
            name: name.to_string(),
            kind: "EVENT_TYPE_WRITE".into(),
            entry: None,
        });
    }

    #[cfg(test)]
    pub(crate) fn is_overflowed(&self) -> bool {
        self.overflowed.load(Ordering::SeqCst)
    }

    fn loop_events(self: Arc<Self>) {
        let mut buf = [0u8; 4096];
        let fd = self.fd.load(Ordering::SeqCst);
        let stop_fd = self.stop_fd.load(Ordering::SeqCst);
        while fd >= 0 && !self.stopped.load(Ordering::SeqCst) {
            let mut pfds = [
                libc::pollfd {
                    fd,
                    events: libc::POLLIN,
                    revents: 0,
                },
                libc::pollfd {
                    fd: stop_fd,
                    events: libc::POLLIN,
                    revents: 0,
                },
            ];
            let nfds = if stop_fd >= 0 { 2 } else { 1 };
            let rc = unsafe { libc::poll(pfds.as_mut_ptr(), nfds, 200) };
            if self.stopped.load(Ordering::SeqCst)
                || (stop_fd >= 0 && pfds[1].revents & libc::POLLIN != 0)
            {
                break;
            }
            if rc < 0 {
                let err = std::io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
            if rc == 0 {
                continue;
            }
            let n = unsafe { libc::read(fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
            if n > 0 {
                self.parse_buf(&buf[..n as usize]);
            } else if n < 0 {
                let code = std::io::Error::last_os_error().raw_os_error();
                if code == Some(libc::EINTR) || code == Some(libc::EAGAIN) {
                    continue;
                }
                break;
            } else {
                break;
            }
        }
        // Reader thread owns the inotify fd and closes it only after the loop exits.
        let closed = self.fd.swap(-1, Ordering::SeqCst);
        if closed >= 0 {
            unsafe {
                libc::close(closed);
            }
        }
    }

    fn parse_buf(&self, buf: &[u8]) {
        let mut off = 0;
        while off + 16 <= buf.len() {
            let wd = i32::from_ne_bytes(buf[off..off + 4].try_into().unwrap());
            let mask = u32::from_ne_bytes(buf[off + 4..off + 8].try_into().unwrap());
            let len = u32::from_ne_bytes(buf[off + 12..off + 16].try_into().unwrap()) as usize;
            let name_off = off + 16;
            if name_off + len > buf.len() {
                break;
            }
            let name_bytes = &buf[name_off..name_off + len];
            let name = name_bytes.split(|b| *b == 0).next().unwrap_or(&[]).to_vec();
            let name = String::from_utf8_lossy(&name).to_string();
            off = name_off + len;
            self.on_raw(wd, mask, &name);
        }
    }

    fn on_raw(&self, wd: i32, mask: u32, name: &str) {
        if mask & libc::IN_Q_OVERFLOW != 0 {
            // Events were dropped in the kernel queue. Do not skip the flag.
            self.mark_overflow(WATCH_BUFFER_OVERFLOW_MSG);
            return;
        }
        if self.overflowed.load(Ordering::SeqCst) {
            return;
        }
        if mask & libc::IN_IGNORED != 0 {
            self.watches.lock().unwrap().remove(&wd);
            return;
        }
        let dir_rel = self
            .watches
            .lock()
            .unwrap()
            .get(&wd)
            .cloned()
            .unwrap_or_default();
        if self.recursive
            && mask & libc::IN_CREATE != 0
            && mask & libc::IN_ISDIR != 0
            && !name.is_empty()
        {
            let rel = join_rel(&dir_rel, name);
            let full = self.root.join(&rel);
            // Linux inotify limitation, not closed: the new directory is armed
            // here, after the create is observed. Anything created in it before
            // inotify_add_watch returns is not queued by the kernel and is not
            // synthesized by a directory scan. A retry still races.
            if let Ok(meta) = std::fs::symlink_metadata(&full) {
                if meta.file_type().is_dir() && !meta.file_type().is_symlink() {
                    let _ = self.add_watch(&full, &rel);
                }
            }
        }
        let Some(kind) = map_mask(mask) else {
            return;
        };
        if name.is_empty() {
            return;
        }
        let rel = join_rel(&dir_rel, name);
        let entry = if self.include_entry && kind != "EVENT_TYPE_REMOVE" {
            entry_json(&self.root, &rel)
        } else {
            None
        };
        self.publish(FsEvent {
            name: rel,
            kind: kind.to_string(),
            entry,
        });
    }
}

fn map_mask(mask: u32) -> Option<&'static str> {
    if mask & (libc::IN_CREATE | libc::IN_MOVED_TO) != 0 {
        Some("EVENT_TYPE_CREATE")
    } else if mask & (libc::IN_DELETE | libc::IN_DELETE_SELF) != 0 {
        Some("EVENT_TYPE_REMOVE")
    } else if mask & libc::IN_MOVED_FROM != 0 {
        Some("EVENT_TYPE_RENAME")
    } else if mask & libc::IN_ATTRIB != 0 {
        Some("EVENT_TYPE_CHMOD")
    } else if mask & libc::IN_MODIFY != 0 {
        Some("EVENT_TYPE_WRITE")
    } else {
        None
    }
}

fn join_rel(dir: &str, name: &str) -> String {
    if name.is_empty() {
        return dir.to_string();
    }
    if dir.is_empty() {
        name.to_string()
    } else {
        format!("{dir}/{name}")
    }
}

fn entry_json(root: &Path, rel: &str) -> Option<String> {
    if rel.is_empty() {
        return None;
    }
    let full = root.join(rel);
    let meta = std::fs::symlink_metadata(&full).ok()?;
    let is_dir = meta.is_dir();
    let ftype = if is_dir {
        "FILE_TYPE_DIRECTORY"
    } else {
        "FILE_TYPE_FILE"
    };
    let name = Path::new(rel)
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_else(|| rel.to_string());
    let mode = meta.permissions().mode() & 0o777;
    let perm = mode_string(meta.permissions().mode(), is_dir);
    Some(format!(
        r#"{{"name":"{}","type":"{}","path":"{}","size":{},"mode":{},"permissions":"{}","isDir":{}}}"#,
        escape_json(&name),
        ftype,
        escape_json(rel),
        meta.len(),
        mode,
        perm,
        if is_dir { "true" } else { "false" }
    ))
}

fn mode_string(mode: u32, is_dir: bool) -> String {
    let mut s = String::with_capacity(10);
    s.push(if is_dir { 'd' } else { '-' });
    for (bit, ch) in [
        (0o400, 'r'),
        (0o200, 'w'),
        (0o100, 'x'),
        (0o040, 'r'),
        (0o020, 'w'),
        (0o010, 'x'),
        (0o004, 'r'),
        (0o002, 'w'),
        (0o001, 'x'),
    ] {
        if mode & bit != 0 {
            s.push(ch);
        } else {
            s.push('-');
        }
    }
    s
}

fn is_special_abs(path: &str) -> bool {
    path == "/proc"
        || path.starts_with("/proc/")
        || path == "/dev"
        || path.starts_with("/dev/")
        || path == "/sys"
        || path.starts_with("/sys/")
}

fn canonicalize_deepest_ancestor(path: &Path) -> Result<PathBuf, WatchPathError> {
    let mut cur = path.to_path_buf();
    let mut tail: Vec<std::ffi::OsString> = Vec::new();
    loop {
        match std::fs::canonicalize(&cur) {
            Ok(mut real) => {
                for seg in tail.into_iter().rev() {
                    real.push(seg);
                }
                return Ok(real);
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                let Some(name) = cur.file_name().map(|s| s.to_os_string()) else {
                    return Ok(path.to_path_buf());
                };
                tail.push(name);
                if !cur.pop() {
                    return Ok(path.to_path_buf());
                }
            }
            Err(e) => return Err(WatchPathError::Invalid(e.to_string())),
        }
    }
}

fn resolve_target_path(workdir: &str, user: &str) -> Result<PathBuf, WatchPathError> {
    let user = user.trim();
    if user.is_empty() {
        return Err(WatchPathError::Invalid("path is required".into()));
    }
    if is_special_abs(user) || is_special_abs(&normalize_logical(user)) {
        return Err(WatchPathError::Invalid(
            "refusing to watch proc, dev, or sys".into(),
        ));
    }
    let work = normalize_logical(workdir);
    let (logical, confined) = if user == "/workspace" {
        (work.clone(), true)
    } else if let Some(rest) = user.strip_prefix("/workspace/") {
        (format!("{}/{}", work.trim_end_matches('/'), rest), true)
    } else if user.starts_with('/') {
        // podd runs inside the sandbox trust boundary; explicit absolute paths
        // (e.g. /home/user) are allowed just like files.write, except /proc,/dev,/sys.
        (user.to_string(), false)
    } else {
        (format!("{}/{}", work.trim_end_matches('/'), user), true)
    };
    let clean = normalize_logical(&logical);
    if confined
        && clean != work
        && !clean.starts_with(&(work.trim_end_matches('/').to_string() + "/"))
    {
        return Err(WatchPathError::Invalid(
            "path escapes sandbox work tree".into(),
        ));
    }
    if is_special_abs(&clean) {
        return Err(WatchPathError::Invalid(
            "refusing to watch proc, dev, or sys".into(),
        ));
    }
    let resolved = canonicalize_deepest_ancestor(Path::new(&clean))?;
    let resolved_str = resolved.to_string_lossy();
    if is_special_abs(&resolved_str) {
        return Err(WatchPathError::Invalid(
            "refusing to watch proc, dev, or sys".into(),
        ));
    }
    if confined {
        let real_work = std::fs::canonicalize(&work).unwrap_or_else(|_| PathBuf::from(&work));
        if resolved != real_work && !resolved.starts_with(&real_work) {
            return Err(WatchPathError::Invalid(
                "path escapes sandbox work tree".into(),
            ));
        }
    }
    Ok(resolved)
}

pub fn resolve_watch_path(workdir: &str, user: &str) -> Result<PathBuf, WatchPathError> {
    let path = resolve_target_path(workdir, user)?;
    match std::fs::metadata(&path) {
        Ok(meta) if meta.is_dir() => Ok(path),
        Ok(_) => Err(WatchPathError::Invalid("path is not a directory".into())),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            Err(WatchPathError::NotFound("directory not found".into()))
        }
        Err(e) => Err(WatchPathError::Invalid(e.to_string())),
    }
}

fn resolve_mkdir_path(workdir: &str, user: &str) -> Result<PathBuf, WatchPathError> {
    resolve_target_path(workdir, user)
}

fn normalize_logical(path: &str) -> String {
    let mut parts: Vec<&str> = Vec::new();
    let abs = path.starts_with('/');
    for comp in Path::new(path).components() {
        match comp {
            Component::RootDir => {}
            Component::CurDir => {}
            Component::ParentDir => {
                if parts.pop().is_none() && !abs {
                    parts.push("..");
                }
            }
            Component::Normal(s) => parts.push(s.to_str().unwrap_or("")),
            Component::Prefix(_) => {}
        }
    }
    if abs {
        format!("/{}", parts.join("/"))
    } else {
        parts.join("/")
    }
}

fn check_mount(path: &Path, allow_network: bool) -> Result<(), WatchPathError> {
    let Some(magic) = statfs_magic(path) else {
        return Ok(());
    };
    if matches!(magic, PROC_SUPER_MAGIC | SYSFS_MAGIC | DEVPTS_SUPER_MAGIC) {
        return Err(WatchPathError::Invalid(
            "refusing to watch proc, sysfs, or devpts".into(),
        ));
    }
    let network = matches!(
        magic,
        NFS_SUPER_MAGIC
            | CIFS_MAGIC
            | SMB_SUPER_MAGIC
            | SMB2_MAGIC
            | FUSE_SUPER_MAGIC
            | NINEP_MAGIC
            | CEPH_MAGIC
    );
    if network && !allow_network {
        return Err(WatchPathError::Invalid(
            "network or fuse mount requires allowNetworkMounts".into(),
        ));
    }
    Ok(())
}

fn statfs_magic(path: &Path) -> Option<u32> {
    let c = CString::new(path.as_os_str().as_bytes()).ok()?;
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statfs(c.as_ptr(), &mut st) };
    if rc != 0 {
        return None;
    }
    Some(st.f_type as u32)
}

fn write_watch_error(stream: &mut TcpStream, err: WatchPathError) {
    let (status, code, message) = match err {
        WatchPathError::NotFound(m) => ("404 Not Found", "not_found", m),
        WatchPathError::Invalid(m) => ("400 Bad Request", "invalid_argument", m),
        WatchPathError::AlreadyExists(m) => ("409 Conflict", "already_exists", m),
        WatchPathError::Exhausted(m) => ("429 Too Many Requests", "resource_exhausted", m),
    };
    // Unary Connect error: HTTP status + raw JSON. Never a successful data frame,
    // even when the request Content-Type is application/connect+json.
    let body = format!(
        r#"{{"code":"{}","message":"{}"}}"#,
        code,
        escape_json(&message)
    );
    write_json(stream, status, &body);
}

fn connect_stream_code(err: WatchPathError) -> (String, String) {
    match err {
        WatchPathError::NotFound(m) => ("not_found".into(), m),
        WatchPathError::Invalid(m) => ("invalid_argument".into(), m),
        WatchPathError::AlreadyExists(m) => ("already_exists".into(), m),
        WatchPathError::Exhausted(m) => ("resource_exhausted".into(), m),
    }
}

/// Connect server-stream error. HTTP 200 + `application/connect+json` end-stream
/// frame. A non-200 status is not parsed by the stream client (404 becomes
/// unimplemented, 400 becomes internal, message `HTTP <status>`).
fn write_connect_stream_error(stream: &mut TcpStream, code: &str, message: &str) {
    let payload = format!(
        r#"{{"error":{{"code":"{}","message":"{}"}}}}"#,
        code,
        escape_json(message)
    );
    let frame = make_connect_frame(payload.as_bytes(), true);
    crate::http::write_response(stream, "200 OK", "application/connect+json", &frame);
}

fn write_watch_stream_error(stream: &mut TcpStream, err: WatchPathError) {
    let (code, message) = connect_stream_code(err);
    write_connect_stream_error(stream, &code, &message);
}

fn parse_watch_body(req: &HttpRequest) -> Result<JsonValue, String> {
    let payload = unpack_connect_payload(&req.body)?;
    if payload.is_empty() {
        return Ok(JsonValue::Object(Default::default()));
    }
    let text = std::str::from_utf8(&payload).map_err(|_| "invalid utf8 payload".to_string())?;
    JsonValue::parse(text).map_err(|e| format!("invalid json: {e}"))
}

fn json_bool(json: &JsonValue, keys: &[&str]) -> bool {
    for key in keys {
        if let Some(v) = json.get(key).and_then(|v| v.as_bool()) {
            return v;
        }
    }
    false
}

fn json_str<'a>(json: &'a JsonValue, keys: &[&str]) -> Option<&'a str> {
    for key in keys {
        if let Some(s) = json.get(key).and_then(|v| v.as_str()) {
            if !s.is_empty() {
                return Some(s);
            }
        }
    }
    None
}

enum PathKind {
    Missing,
    Dir,
    Other,
}

fn classify_path(path: &Path) -> Result<PathKind, std::io::Error> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_symlink() => match std::fs::metadata(path) {
            Ok(followed) if followed.is_dir() => Ok(PathKind::Dir),
            Ok(_) => Ok(PathKind::Other),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(PathKind::Other),
            Err(e) => Err(e),
        },
        Ok(meta) if meta.is_dir() => Ok(PathKind::Dir),
        Ok(_) => Ok(PathKind::Other),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(PathKind::Missing),
        Err(e) => Err(e),
    }
}

fn mkdir_io_error(err: std::io::Error, last: bool) -> WatchPathError {
    if err.kind() == std::io::ErrorKind::AlreadyExists {
        return if last {
            WatchPathError::AlreadyExists("directory already exists".into())
        } else {
            WatchPathError::Invalid("parent path is not a directory".into())
        };
    }
    if err.raw_os_error() == Some(libc::ENOTDIR) {
        return WatchPathError::Invalid("parent path is not a directory".into());
    }
    WatchPathError::Invalid(err.to_string())
}

/// Create `path` and any missing parents, chowning newly created directories
/// to `(uid, gid)` when running as root. An existing directory is `already_exists`.
/// An existing file, or a file used as a parent, is `invalid_argument`.
fn mkdir_p(path: &Path, uid: u32, gid: u32) -> Result<(), WatchPathError> {
    let comps: Vec<Component> = path.components().collect();
    if comps.is_empty() {
        return Err(WatchPathError::Invalid("path is required".into()));
    }
    let mut built = PathBuf::new();
    for (i, comp) in comps.iter().enumerate() {
        built.push(comp);
        if matches!(comp, Component::RootDir | Component::Prefix(_)) {
            continue;
        }
        let last = i + 1 == comps.len();
        match classify_path(&built).map_err(|e| mkdir_io_error(e, last))? {
            PathKind::Dir => {
                if last {
                    return Err(WatchPathError::AlreadyExists(
                        "directory already exists".into(),
                    ));
                }
            }
            PathKind::Other => {
                return Err(WatchPathError::Invalid(if last {
                    "path exists and is not a directory".into()
                } else {
                    "parent path is not a directory".into()
                }));
            }
            PathKind::Missing => match std::fs::create_dir(&built) {
                Ok(()) => {
                    crate::files::chown_if_root(&built, uid, gid);
                }
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                    match classify_path(&built).map_err(|err| mkdir_io_error(err, last))? {
                        PathKind::Dir if last => {
                            return Err(WatchPathError::AlreadyExists(
                                "directory already exists".into(),
                            ));
                        }
                        PathKind::Dir => {}
                        _ => {
                            return Err(WatchPathError::Invalid(if last {
                                "path exists and is not a directory".into()
                            } else {
                                "parent path is not a directory".into()
                            }));
                        }
                    }
                }
                Err(e) => return Err(mkdir_io_error(e, last)),
            },
        }
    }
    Ok(())
}

fn entry_name_from_user(user: &str) -> String {
    let trimmed = user.trim().trim_end_matches('/');
    Path::new(trimmed)
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| trimmed.to_string())
}

fn make_dir_success_body(user_path: &str, full: &Path) -> String {
    let meta = std::fs::metadata(full).ok();
    let size = meta.as_ref().map(|m| m.len()).unwrap_or(0);
    let mode = meta
        .as_ref()
        .map(|m| m.permissions().mode() & 0o777)
        .unwrap_or(0o755);
    let name = entry_name_from_user(user_path);
    let perm = mode_string(mode, true);
    format!(
        r#"{{"entry":{{"name":"{}","type":"FILE_TYPE_DIRECTORY","path":"{}","size":{},"mode":{},"permissions":"{}","isDir":true,"is_dir":true}}}}"#,
        escape_json(&name),
        escape_json(user_path.trim()),
        size,
        mode,
        perm,
    )
}

fn parse_query_map(req_path: &str) -> HashMap<String, String> {
    let mut map = HashMap::new();
    let Some((_, qs)) = req_path.split_once('?') else {
        return map;
    };
    for pair in qs.split('&') {
        if let Some((k, v)) = pair.split_once('=') {
            if !k.is_empty() {
                map.insert(k.to_string(), v.to_string());
            }
        }
    }
    map
}

pub fn handle_make_dir(stream: &mut TcpStream, req: &HttpRequest, config: &DaemonConfig) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let path = json_str(&json, &["path"]).unwrap_or("");
    let full = match resolve_mkdir_path(&config.workdir, path) {
        Ok(p) => p,
        Err(e) => {
            write_watch_error(stream, e);
            return;
        }
    };
    let mut ownership_params = parse_query_map(&req.target);
    if !ownership_params.contains_key("user") && !ownership_params.contains_key("username") {
        if let Some(u) = json_str(&json, &["user", "username"]) {
            ownership_params.insert("user".to_string(), u.to_string());
        }
    }
    let (uid, gid) = crate::files::resolve_file_ownership(&ownership_params);
    if let Err(e) = mkdir_p(&full, uid, gid) {
        write_watch_error(stream, e);
        return;
    }
    let body = make_dir_success_body(path, &full);
    write_unary_connect_response(stream, req, "200 OK", &body);
}

const MAX_LIST_DEPTH: usize = 32;

fn format_entry_json(name: &str, user_path: &str, meta: &std::fs::Metadata) -> String {
    let is_dir = meta.is_dir();
    let ftype = if is_dir {
        "FILE_TYPE_DIRECTORY"
    } else {
        "FILE_TYPE_FILE"
    };
    let size = meta.len();
    let mode = meta.permissions().mode() & 0o777;
    let perm = mode_string(mode, is_dir);
    let is_dir_str = if is_dir { "true" } else { "false" };
    format!(
        r#"{{"name":"{}","type":"{}","path":"{}","size":{},"mode":{},"permissions":"{}","isDir":{},"is_dir":{}}}"#,
        escape_json(name),
        ftype,
        escape_json(user_path),
        size,
        mode,
        perm,
        is_dir_str,
        is_dir_str,
    )
}

fn collect_list_entries(
    dir: &Path,
    user_prefix: &str,
    depth: usize,
    out: &mut Vec<String>,
) -> Result<(), WatchPathError> {
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(WatchPathError::NotFound("directory not found".into()));
        }
        Err(e) => return Err(WatchPathError::Invalid(e.to_string())),
    };

    let mut children: Vec<(String, PathBuf, std::fs::Metadata, bool)> = Vec::new();
    for ent in entries.flatten() {
        let name = ent.file_name().to_string_lossy().to_string();
        let child_path = ent.path();
        let sym_meta = match std::fs::symlink_metadata(&child_path) {
            Ok(m) => m,
            Err(_) => continue,
        };
        let is_symlink = sym_meta.file_type().is_symlink();
        let meta = std::fs::metadata(&child_path).unwrap_or(sym_meta);
        children.push((name, child_path, meta, is_symlink));
    }
    children.sort_by(|a, b| a.0.cmp(&b.0));

    let prefix = user_prefix.trim_end_matches('/');
    for (name, child_path, meta, is_symlink) in children {
        let item_user_path = format!("{prefix}/{name}");
        out.push(format_entry_json(&name, &item_user_path, &meta));
        if depth > 1 && meta.is_dir() && !is_symlink {
            collect_list_entries(&child_path, &item_user_path, depth - 1, out)?;
        }
    }
    Ok(())
}

pub fn handle_list_dir(stream: &mut TcpStream, req: &HttpRequest, config: &DaemonConfig) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let user_path = json_str(&json, &["path"]).unwrap_or("/workspace");
    let depth = match json.get("depth") {
        None | Some(JsonValue::Null) => 1usize,
        Some(v) => match v.as_i64() {
            Some(n) if n < 0 => {
                write_watch_error(
                    stream,
                    WatchPathError::Invalid("depth must be non-negative".into()),
                );
                return;
            }
            Some(0) => 1usize,
            Some(n) => (n as usize).min(MAX_LIST_DEPTH),
            None => {
                write_watch_error(stream, WatchPathError::Invalid("invalid depth".into()));
                return;
            }
        },
    };
    let full = match resolve_target_path(&config.workdir, user_path) {
        Ok(p) => p,
        Err(e) => {
            write_watch_error(stream, e);
            return;
        }
    };
    match std::fs::metadata(&full) {
        Ok(m) if m.is_dir() => {}
        Ok(_) => {
            write_watch_error(
                stream,
                WatchPathError::Invalid("path is not a directory".into()),
            );
            return;
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            write_watch_error(
                stream,
                WatchPathError::NotFound("directory not found".into()),
            );
            return;
        }
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e.to_string()));
            return;
        }
    }
    let mut entries = Vec::new();
    if let Err(e) = collect_list_entries(&full, user_path.trim(), depth, &mut entries) {
        write_watch_error(stream, e);
        return;
    }
    let body = format!(r#"{{"entries":[{}]}}"#, entries.join(","));
    write_unary_connect_response(stream, req, "200 OK", &body);
}

pub fn handle_stat(stream: &mut TcpStream, req: &HttpRequest, config: &DaemonConfig) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let user_path = json_str(&json, &["path"]).unwrap_or("");
    let full = match resolve_target_path(&config.workdir, user_path) {
        Ok(p) => p,
        Err(e) => {
            write_watch_error(stream, e);
            return;
        }
    };
    let meta = match std::fs::metadata(&full) {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            write_watch_error(
                stream,
                WatchPathError::NotFound("file or directory not found".into()),
            );
            return;
        }
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e.to_string()));
            return;
        }
    };
    let name = entry_name_from_user(user_path);
    let entry_json = format_entry_json(&name, user_path.trim(), &meta);
    let body = format!(r#"{{"entry":{entry_json}}}"#);
    write_unary_connect_response(stream, req, "200 OK", &body);
}

fn resolve_remove_path(workdir: &str, user: &str) -> Result<PathBuf, WatchPathError> {
    let user = user.trim();
    if user.is_empty() {
        return Err(WatchPathError::Invalid("path is required".into()));
    }
    if is_special_abs(user) || is_special_abs(&normalize_logical(user)) {
        return Err(WatchPathError::Invalid(
            "refusing to remove proc, dev, or sys".into(),
        ));
    }
    let work = normalize_logical(workdir);
    let (logical, confined) = if user == "/workspace" {
        (work.clone(), true)
    } else if let Some(rest) = user.strip_prefix("/workspace/") {
        (format!("{}/{}", work.trim_end_matches('/'), rest), true)
    } else if user.starts_with('/') {
        (user.to_string(), false)
    } else {
        (format!("{}/{}", work.trim_end_matches('/'), user), true)
    };
    let clean = normalize_logical(&logical);
    if confined {
        if clean == work {
            return Err(WatchPathError::Invalid(
                "refusing to remove workspace root".into(),
            ));
        }
        if !clean.starts_with(&(work.trim_end_matches('/').to_string() + "/")) {
            return Err(WatchPathError::Invalid(
                "path escapes sandbox work tree".into(),
            ));
        }
    }
    if is_special_abs(&clean) {
        return Err(WatchPathError::Invalid(
            "refusing to remove proc, dev, or sys".into(),
        ));
    }
    let clean_path = Path::new(&clean);
    let Some(file_name) = clean_path.file_name() else {
        return Err(WatchPathError::Invalid("cannot remove root path".into()));
    };
    let parent = clean_path.parent().unwrap_or_else(|| Path::new("/"));
    let real_parent = canonicalize_deepest_ancestor(parent)?;
    if is_special_abs(&real_parent.to_string_lossy()) {
        return Err(WatchPathError::Invalid(
            "refusing to remove proc, dev, or sys".into(),
        ));
    }
    if confined {
        let real_work = std::fs::canonicalize(&work).unwrap_or_else(|_| PathBuf::from(&work));
        if real_parent != real_work && !real_parent.starts_with(&real_work) {
            return Err(WatchPathError::Invalid(
                "path escapes sandbox work tree".into(),
            ));
        }
    }
    Ok(real_parent.join(file_name))
}

pub fn handle_remove(stream: &mut TcpStream, req: &HttpRequest, config: &DaemonConfig) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let user_path = json_str(&json, &["path"]).unwrap_or("");
    let target = match resolve_remove_path(&config.workdir, user_path) {
        Ok(p) => p,
        Err(e) => {
            write_watch_error(stream, e);
            return;
        }
    };
    let meta = match std::fs::symlink_metadata(&target) {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            write_watch_error(
                stream,
                WatchPathError::NotFound("file or directory not found".into()),
            );
            return;
        }
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e.to_string()));
            return;
        }
    };
    let res = if meta.file_type().is_symlink() || !meta.is_dir() {
        std::fs::remove_file(&target)
    } else {
        std::fs::remove_dir_all(&target)
    };
    if let Err(e) = res {
        if e.kind() == std::io::ErrorKind::NotFound {
            write_watch_error(
                stream,
                WatchPathError::NotFound("file or directory not found".into()),
            );
        } else {
            write_watch_error(stream, WatchPathError::Invalid(e.to_string()));
        }
        return;
    }
    write_unary_connect_response(stream, req, "200 OK", "{}");
}

pub fn handle_create_watcher(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let path = json_str(&json, &["path"]).unwrap_or("");
    let recursive = json_bool(&json, &["recursive"]);
    let include_entry = json_bool(&json, &["includeEntry", "include_entry"]);
    let allow_network = json_bool(&json, &["allowNetworkMounts", "allow_network_mounts"]);
    match registry.watchers.create(
        &config.workdir,
        path,
        recursive,
        include_entry,
        allow_network,
        true,
    ) {
        Ok(w) => {
            let body = format!(r#"{{"watcherId":"{}"}}"#, escape_json(&w.id));
            write_unary_connect_response(stream, req, "200 OK", &body);
        }
        Err(e) => write_watch_error(stream, e),
    }
}

pub fn handle_get_watcher_events(
    stream: &mut TcpStream,
    req: &HttpRequest,
    _config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let id = match json_str(&json, &["watcherId", "watcher_id"]) {
        Some(id) => id,
        None => {
            write_watch_error(
                stream,
                WatchPathError::Invalid("watcherId is required".into()),
            );
            return;
        }
    };
    let Some(w) = registry.watchers.get(id) else {
        write_watch_error(stream, WatchPathError::NotFound("watcher not found".into()));
        return;
    };
    if w.stopped.load(Ordering::SeqCst) {
        write_watch_error(stream, WatchPathError::NotFound("watcher not found".into()));
        return;
    }
    let events = match w.poll_events() {
        Ok(evs) => evs,
        Err(()) => {
            write_watch_error(
                stream,
                WatchPathError::Exhausted(WATCH_BUFFER_OVERFLOW_MSG.into()),
            );
            return;
        }
    };
    let rendered: Vec<String> = events.iter().map(FsEvent::to_json).collect();
    let body = format!(r#"{{"events":[{}]}}"#, rendered.join(","));
    write_unary_connect_response(stream, req, "200 OK", &body);
}

pub fn handle_remove_watcher(
    stream: &mut TcpStream,
    req: &HttpRequest,
    _config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let id = match json_str(&json, &["watcherId", "watcher_id"]) {
        Some(id) => id,
        None => {
            write_watch_error(
                stream,
                WatchPathError::Invalid("watcherId is required".into()),
            );
            return;
        }
    };
    if registry.watchers.remove(id) {
        write_unary_connect_response(stream, req, "200 OK", "{}");
    } else {
        write_watch_error(stream, WatchPathError::NotFound("watcher not found".into()));
    }
}

pub fn handle_watch_dir(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let json = match parse_watch_body(req) {
        Ok(j) => j,
        Err(e) => {
            write_watch_stream_error(stream, WatchPathError::Invalid(e));
            return;
        }
    };
    let path = json_str(&json, &["path"]).unwrap_or("");
    let recursive = json_bool(&json, &["recursive"]);
    let include_entry = json_bool(&json, &["includeEntry", "include_entry"]);
    let allow_network = json_bool(&json, &["allowNetworkMounts", "allow_network_mounts"]);
    let watcher = match registry.watchers.create(
        &config.workdir,
        path,
        recursive,
        include_entry,
        allow_network,
        false,
    ) {
        Ok(w) => w,
        Err(e) => {
            write_watch_stream_error(stream, e);
            return;
        }
    };
    let id = watcher.id.clone();
    let sub = watcher.subscribe();
    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        registry.watchers.remove(&id);
        return;
    }
    let start = make_connect_frame(br#"{"start":{}}"#, false);
    if stream.write_all(&start).is_err() || stream.flush().is_err() {
        registry.watchers.remove(&id);
        return;
    }
    // Client timeoutMs is a stream deadline. Disconnect stops this watch.
    pump_watch_subscription(stream, sub, &id, &registry.watchers);
}

fn pump_watch_subscription(
    stream: &mut TcpStream,
    sub: WatchSubHandle,
    id: &str,
    watchers: &WatchRegistry,
) {
    let mut last_sent = std::time::Instant::now();
    loop {
        match sub.rx.recv_timeout(Duration::from_millis(200)) {
            Ok(WatchMsg::Event(ev)) => {
                let payload = format!(r#"{{"filesystem":{}}}"#, ev.to_json());
                let frame = make_connect_frame(payload.as_bytes(), false);
                if stream.write_all(&frame).is_err() || stream.flush().is_err() {
                    break;
                }
                last_sent = std::time::Instant::now();
            }
            Err(RecvTimeoutError::Timeout) => {
                if client_gone(stream) {
                    break;
                }
                if last_sent.elapsed() >= WATCH_KEEPALIVE_INTERVAL {
                    let frame = make_connect_frame(br#"{"keepalive":{}}"#, false);
                    if stream.write_all(&frame).is_err() || stream.flush().is_err() {
                        break;
                    }
                    last_sent = std::time::Instant::now();
                }
            }
            Err(RecvTimeoutError::Disconnected) => {
                if sub.overflow.load(Ordering::SeqCst) {
                    let message = sub.message.lock().unwrap().clone();
                    let message = if message.is_empty() {
                        WATCH_QUEUE_OVERFLOW_MSG.to_string()
                    } else {
                        message
                    };
                    let err = format!(
                        r#"{{"error":{{"code":"resource_exhausted","message":"{}"}}}}"#,
                        escape_json(&message)
                    );
                    let frame = make_connect_frame(err.as_bytes(), true);
                    let _ = stream.write_all(&frame);
                    let _ = stream.flush();
                } else {
                    // Server-side stop (RemoveWatcher, prepare_stop, stop_all): emit clean Connect end-stream frame.
                    let end = make_connect_frame(b"{}", true);
                    let _ = stream.write_all(&end);
                    let _ = stream.flush();
                }
                break;
            }
        }
    }
    watchers.remove(id);
}

fn client_gone(stream: &TcpStream) -> bool {
    let fd = stream.as_raw_fd();
    let mut pfd = libc::pollfd {
        fd,
        events: libc::POLLIN,
        revents: 0,
    };
    let rc = unsafe { libc::poll(&mut pfd, 1, 0) };
    if rc <= 0 {
        return false;
    }
    if pfd.revents & (libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0 {
        return true;
    }
    if pfd.revents & libc::POLLIN != 0 {
        let mut b = [0u8; 1];
        let n = unsafe {
            libc::recv(
                fd,
                b.as_mut_ptr() as *mut libc::c_void,
                1,
                libc::MSG_PEEK | libc::MSG_DONTWAIT,
            )
        };
        return n == 0;
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn subscriber_overflow_fails_instead_of_dropping() {
        let dir = std::env::temp_dir().join(format!("podd-watch-ovf-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                false,
            )
            .unwrap();
        let sub = w.subscribe();
        for i in 0..(WATCH_SUB_QUEUE + 3) {
            w.publish(FsEvent {
                name: format!("n{i}"),
                kind: "EVENT_TYPE_CREATE".into(),
                entry: None,
            });
        }
        assert!(
            sub.overflow.load(Ordering::SeqCst),
            "slow subscriber must be failed"
        );
        let mut n = 0;
        while sub.rx.try_recv().is_ok() {
            n += 1;
        }
        assert!(n <= WATCH_SUB_QUEUE);
        assert!(sub.rx.try_recv().is_err());
        reg.remove(&w.id);
        assert_eq!(reg.active_count(), 0);
        assert_eq!(reg.open_fd_count(), 0);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn resolve_rejects_escape_missing_and_file() {
        let dir = std::env::temp_dir().join(format!("podd-watch-path-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let work = dir.to_str().unwrap();
        assert!(resolve_watch_path(work, "/workspace").is_ok());
        assert!(matches!(
            resolve_watch_path(work, "/workspace/nope"),
            Err(WatchPathError::NotFound(_))
        ));
        let file = dir.join("f.txt");
        std::fs::write(&file, b"x").unwrap();
        assert!(matches!(
            resolve_watch_path(work, "/workspace/f.txt"),
            Err(WatchPathError::Invalid(_))
        ));
        assert!(matches!(
            resolve_watch_path(work, "/workspace/../../etc"),
            Err(WatchPathError::Invalid(_))
        ));
        assert!(matches!(
            resolve_watch_path(work, "../etc"),
            Err(WatchPathError::Invalid(_))
        ));
        assert!(matches!(
            resolve_watch_path(work, "/proc"),
            Err(WatchPathError::Invalid(_))
        ));
        assert!(matches!(
            resolve_watch_path(work, "/dev"),
            Err(WatchPathError::Invalid(_))
        ));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn ring_overflow_fails_poll_instead_of_truncating() {
        let dir = std::env::temp_dir().join(format!(
            "podd-watch-ring-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                true,
            )
            .unwrap();
        for i in 0..WATCH_RING_CAP {
            w.publish(FsEvent {
                name: format!("f{i}"),
                kind: "EVENT_TYPE_WRITE".into(),
                entry: None,
            });
        }
        let evs = w.poll_events().expect("a full ring is still complete");
        assert_eq!(evs.len(), WATCH_RING_CAP);
        assert_eq!(evs[0].name, "f0");
        assert!(
            w.poll_events().unwrap().is_empty(),
            "a successful poll clears the ring"
        );

        let sub = w.subscribe();
        for i in 0..WATCH_RING_CAP {
            w.publish(FsEvent {
                name: format!("g{i}"),
                kind: "EVENT_TYPE_WRITE".into(),
                entry: None,
            });
            let _ = sub.rx.try_recv();
        }
        assert!(!w.is_overflowed());
        w.publish(FsEvent {
            name: "overflow".into(),
            kind: "EVENT_TYPE_WRITE".into(),
            entry: None,
        });
        assert!(
            w.is_overflowed(),
            "the event past the cap must taint the watcher"
        );
        assert!(
            w.poll_events().is_err(),
            "GetWatcherEvents must not return a truncated ring"
        );
        assert!(sub.overflow.load(Ordering::SeqCst));
        assert_eq!(
            sub.message.lock().unwrap().as_str(),
            WATCH_BUFFER_OVERFLOW_MSG
        );
        assert!(sub.rx.try_recv().is_err());

        reg.remove(&w.id);
        assert_eq!(reg.open_fd_count(), 0);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn stream_watcher_does_not_overflow_past_ring_cap() {
        let dir = std::env::temp_dir().join(format!(
            "podd-watch-stream-cap-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                false,
            )
            .unwrap();
        let sub = w.subscribe();
        let total = WATCH_RING_CAP + 100;
        for i in 0..total {
            w.publish(FsEvent {
                name: format!("s{i}"),
                kind: "EVENT_TYPE_WRITE".into(),
                entry: None,
            });
            let got = sub
                .rx
                .try_recv()
                .expect("stream subscriber must receive all events");
            match got {
                WatchMsg::Event(ev) => assert_eq!(ev.name, format!("s{i}")),
            }
        }
        assert!(!w.is_overflowed());
        assert!(!sub.overflow.load(Ordering::SeqCst));
        reg.remove(&w.id);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn kernel_overflow_fails_subscribers() {
        let dir = std::env::temp_dir().join(format!(
            "podd-watch-qovf-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                false,
            )
            .unwrap();
        let sub = w.subscribe();
        w.on_raw(0, libc::IN_Q_OVERFLOW, "");
        assert!(w.is_overflowed());
        assert!(sub.overflow.load(Ordering::SeqCst));
        assert_eq!(
            sub.message.lock().unwrap().as_str(),
            WATCH_BUFFER_OVERFLOW_MSG
        );
        assert!(w.poll_events().is_err());
        assert!(matches!(
            sub.rx.try_recv(),
            Err(std::sync::mpsc::TryRecvError::Disconnected)
        ));
        reg.remove(&w.id);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn recursive_arm_does_not_replay_files_created_before_add_watch() {
        // Linux inotify limitation: a new subdirectory is armed only when the
        // create event is handled. Files that already exist in that directory
        // (the race window before inotify_add_watch returns) are not delivered
        // and are not invented by a scan. This test does not try to close the race.
        let dir = std::env::temp_dir().join(format!(
            "podd-watch-race-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let fd = unsafe { libc::inotify_init1(libc::IN_CLOEXEC | libc::IN_NONBLOCK) };
        assert!(fd >= 0);
        let w = DirWatcher {
            id: "wrace".into(),
            root: dir.clone(),
            recursive: true,
            include_entry: false,
            poll: true,
            fd: AtomicI32::new(fd),
            stop_fd: AtomicI32::new(-1),
            watches: Mutex::new(HashMap::new()),
            pending: Mutex::new(VecDeque::new()),
            subscribers: Mutex::new(Vec::new()),
            stopped: AtomicBool::new(false),
            overflowed: AtomicBool::new(false),
            last_poll: Mutex::new(std::time::Instant::now()),
        };
        w.add_watch(&dir, "").unwrap();
        let child = dir.join("child");
        std::fs::create_dir(&child).unwrap();
        std::fs::write(child.join("early.txt"), b"x").unwrap();
        let root_wd = *w.watches.lock().unwrap().keys().next().unwrap();
        w.on_raw(root_wd, libc::IN_CREATE | libc::IN_ISDIR, "child");
        let evs = w.poll_events().unwrap();
        assert!(
            evs.iter().any(|e| e.name == "child"),
            "the directory create itself is reported: {evs:?}"
        );
        assert!(
            evs.iter().all(|e| e.name != "child/early.txt"),
            "files created before add_watch are not synthesized: {evs:?}"
        );
        assert!(
            w.watches.lock().unwrap().values().any(|rel| rel == "child"),
            "the new directory is armed for later events only"
        );
        w.stop();
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn watch_dir_pump_emits_resource_exhausted() {
        use std::io::Read;
        use std::net::TcpListener;
        use std::time::Duration;

        let dir = std::env::temp_dir().join(format!(
            "podd-watch-pump-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                false,
            )
            .unwrap();
        let sub = w.subscribe();
        for i in 0..(WATCH_SUB_QUEUE + 3) {
            w.publish(FsEvent {
                name: format!("n{i}"),
                kind: "EVENT_TYPE_WRITE".into(),
                entry: None,
            });
        }
        assert!(sub.overflow.load(Ordering::SeqCst));

        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let id = w.id.clone();
        let reg_thread = reg.clone();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            pump_watch_subscription(&mut stream, sub, &id, &reg_thread);
        });

        let mut client = std::net::TcpStream::connect(addr).unwrap();
        client
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut buf = Vec::new();
        client.read_to_end(&mut buf).unwrap();
        server.join().unwrap();

        let err = br#"{"error":{"code":"resource_exhausted","message":"watch subscriber queue overflow"}}"#;
        let expected = make_connect_frame(err, true);
        assert!(
            buf.windows(expected.len())
                .any(|w| w == expected.as_slice()),
            "pump must end the stream with resource_exhausted, got {} bytes",
            buf.len()
        );
        assert_eq!(reg.active_count(), 0);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn kernel_overflow_pump_emits_dropped_events_error() {
        use std::io::Read;
        use std::net::TcpListener;
        use std::time::Duration;

        let dir = std::env::temp_dir().join(format!(
            "podd-watch-kpump-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                false,
            )
            .unwrap();
        let sub = w.subscribe();
        w.on_raw(0, libc::IN_Q_OVERFLOW, "");
        assert!(sub.overflow.load(Ordering::SeqCst));

        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let id = w.id.clone();
        let reg_thread = reg.clone();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            pump_watch_subscription(&mut stream, sub, &id, &reg_thread);
        });
        let mut client = std::net::TcpStream::connect(addr).unwrap();
        client
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut buf = Vec::new();
        client.read_to_end(&mut buf).unwrap();
        server.join().unwrap();
        let err = format!(
            r#"{{"error":{{"code":"resource_exhausted","message":"{WATCH_BUFFER_OVERFLOW_MSG}"}}}}"#
        );
        let expected = make_connect_frame(err.as_bytes(), true);
        assert!(
            buf.windows(expected.len())
                .any(|w| w == expected.as_slice()),
            "kernel overflow must end the stream with resource_exhausted, got {}",
            String::from_utf8_lossy(&buf)
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn watch_dir_pump_emits_clean_end_frame_on_server_stop() {
        use std::io::Read;
        use std::net::TcpListener;
        use std::time::Duration;

        let dir = std::env::temp_dir().join(format!(
            "podd-watch-clean-end-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::new();
        let w = reg
            .create(
                dir.to_str().unwrap(),
                dir.to_str().unwrap(),
                false,
                false,
                false,
                false,
            )
            .unwrap();
        let sub = w.subscribe();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let id = w.id.clone();
        let reg_thread = reg.clone();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            pump_watch_subscription(&mut stream, sub, &id, &reg_thread);
        });

        let mut client = std::net::TcpStream::connect(addr).unwrap();
        client
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        reg.remove(&w.id);
        let mut buf = Vec::new();
        client.read_to_end(&mut buf).unwrap();
        server.join().unwrap();

        let expected = make_connect_frame(b"{}", true);
        assert_eq!(
            buf, expected,
            "server-side stop must emit a clean Connect end-stream frame"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn resolve_allows_explicit_abs_and_blocks_symlink_escape() {
        let base = std::env::temp_dir().join(format!(
            "podd-watch-sym-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let work = base.join("work");
        let outside = base.join("outside");
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&work).unwrap();
        std::fs::create_dir_all(&outside).unwrap();

        let work_str = work.to_str().unwrap();
        // Explicit absolute path outside workdir is allowed in podd (trust boundary = sandbox).
        assert!(resolve_watch_path(work_str, outside.to_str().unwrap()).is_ok());

        // Symlink inside /workspace pointing outside /workspace is rejected.
        let link = work.join("escape");
        std::os::unix::fs::symlink(&outside, &link).unwrap();
        assert!(matches!(
            resolve_watch_path(work_str, "/workspace/escape"),
            Err(WatchPathError::Invalid(_))
        ));
        assert!(matches!(
            resolve_mkdir_path(work_str, "/workspace/escape/sub"),
            Err(WatchPathError::Invalid(_))
        ));

        // Symlink pointing to /proc is rejected even for explicit absolute paths.
        let proc_link = outside.join("to_proc");
        std::os::unix::fs::symlink("/proc", &proc_link).unwrap();
        assert!(matches!(
            resolve_watch_path(work_str, proc_link.to_str().unwrap()),
            Err(WatchPathError::Invalid(_))
        ));

        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn watcher_cap_and_idle_poll_ttl() {
        let dir = std::env::temp_dir().join(format!(
            "podd-watch-cap-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let reg = WatchRegistry::with_max_watchers(1);
        reg.create(
            dir.to_str().unwrap(),
            dir.to_str().unwrap(),
            false,
            false,
            false,
            true,
        )
        .unwrap();
        match reg.create(
            dir.to_str().unwrap(),
            dir.to_str().unwrap(),
            false,
            false,
            false,
            true,
        ) {
            Err(WatchPathError::Exhausted(msg)) => {
                assert!(msg.contains("too many active watchers"), "got: {msg}");
            }
            Err(e) => panic!("expected Exhausted(too many active watchers), got {e:?}"),
            Ok(_) => panic!("expected Exhausted(too many active watchers), got Ok"),
        }
        // Reaping with zero TTL expires idle poll watchers.
        assert_eq!(reg.reap_expired(Duration::ZERO), 1);
        assert_eq!(reg.active_count(), 0);
        reg.stop_all();
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn map_mask_matches_fsnotify_and_drops_close_write() {
        assert_eq!(map_mask(libc::IN_CREATE), Some("EVENT_TYPE_CREATE"));
        assert_eq!(map_mask(libc::IN_MOVED_TO), Some("EVENT_TYPE_CREATE"));
        assert_eq!(map_mask(libc::IN_MOVED_FROM), Some("EVENT_TYPE_RENAME"));
        assert_eq!(map_mask(libc::IN_MODIFY), Some("EVENT_TYPE_WRITE"));
        assert_eq!(map_mask(libc::IN_CLOSE_WRITE), None);
        assert_eq!(WATCH_MASK & libc::IN_CLOSE_WRITE, 0);
    }
}
