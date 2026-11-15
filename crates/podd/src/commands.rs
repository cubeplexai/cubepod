//! Process Registry, Execution & Command Management for podd.

use std::collections::HashMap;
use std::fs::File;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::os::unix::io::FromRawFd;
use std::os::unix::process::CommandExt;
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use crate::files::decompress_if_gzip;
use crate::http::{write_json, write_response, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::startcmd::{configure_command_credentials, exit_code_from_status, spawn_tracked};
use crate::DaemonConfig;

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct RunRequest {
    pub command: Option<String>,
    pub id: Option<String>,
    pub workdir: Option<String>,
    pub background: bool,
    pub uid: Option<u32>,
    pub gid: Option<u32>,
    pub user: Option<String>,
    pub envs: HashMap<String, String>,
}

impl RunRequest {
    pub fn from_json(json: &JsonValue) -> Self {
        let command = json
            .get("command")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let id = json
            .get("id")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let workdir = json
            .get("workdir")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let background = json
            .get("background")
            .and_then(|v| v.as_bool())
            .unwrap_or(false);
        let uid = json.get("uid").and_then(|v| v.as_u64()).map(|u| u as u32);
        let gid = json.get("gid").and_then(|v| v.as_u64()).map(|g| g as u32);
        let user = json
            .get("user")
            .and_then(|v| v.as_str())
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty());

        let mut envs = HashMap::new();
        if let Some(env_obj) = json
            .get("envs")
            .or_else(|| json.get("envVars"))
            .and_then(|v| v.as_object())
        {
            for (k, v) in env_obj {
                if let Some(val) = v.as_str() {
                    envs.insert(k.clone(), val.to_string());
                }
            }
        }

        Self {
            command,
            id,
            workdir,
            background,
            uid,
            gid,
            user,
            envs,
        }
    }

    pub fn from_json_str(raw: &str) -> Result<Self, String> {
        let json = JsonValue::parse(raw)?;
        Ok(Self::from_json(&json))
    }
}

pub type ProcessOutputBuffer = Vec<u8>;

/// Connect replays only the trailing PTY bytes. Older output is dropped.
pub const PTY_REPLAY_CAP: usize = 64 * 1024;
/// Per Start/Connect subscriber. A full queue fails that subscriber; bytes are not dropped silently.
pub const PTY_SUBSCRIBER_QUEUE: usize = 128;

#[derive(Clone, Debug)]
pub enum PtyBroadcastEvent {
    Data(Vec<u8>),
    End(i32),
}

pub struct PtySub {
    tx: std::sync::mpsc::SyncSender<PtyBroadcastEvent>,
    overflow: Arc<AtomicBool>,
}

#[derive(Clone)]
pub struct PtyHandle {
    pub master_fd: Arc<AtomicI32>,
    pub subscribers: Arc<Mutex<Vec<PtySub>>>,
}

/// Keep the last `cap` bytes. Connect may replay only this tail.
pub fn append_replay(buf: &mut Vec<u8>, chunk: &[u8], cap: usize) {
    if cap == 0 {
        buf.clear();
        return;
    }
    if chunk.len() >= cap {
        buf.clear();
        buf.extend_from_slice(&chunk[chunk.len() - cap..]);
        return;
    }
    buf.extend_from_slice(chunk);
    if buf.len() > cap {
        let excess = buf.len() - cap;
        buf.drain(..excess);
    }
}

/// Append to the replay tail and fan out. Locks subscribers, then the process entry.
pub fn publish_pty_chunk(pty: &PtyHandle, entry: &Mutex<ProcessEntry>, chunk: &[u8]) {
    let mut subs = pty.subscribers.lock().unwrap();
    let mut proc = entry.lock().unwrap();
    append_replay(&mut proc.output, chunk, PTY_REPLAY_CAP);
    let owned = chunk.to_vec();
    subs.retain(
        |sub| match sub.tx.try_send(PtyBroadcastEvent::Data(owned.clone())) {
            Ok(()) => true,
            Err(std::sync::mpsc::TrySendError::Full(_)) => {
                sub.overflow.store(true, Ordering::SeqCst);
                false
            }
            Err(std::sync::mpsc::TrySendError::Disconnected(_)) => false,
        },
    );
}

pub fn publish_pty_end(pty: &PtyHandle, entry: &Mutex<ProcessEntry>, exit_code: i32) {
    let mut subs = pty.subscribers.lock().unwrap();
    let mut proc = entry.lock().unwrap();
    proc.running = false;
    proc.exit_code = Some(exit_code);
    subs.retain(
        |sub| match sub.tx.try_send(PtyBroadcastEvent::End(exit_code)) {
            Ok(()) => true,
            Err(std::sync::mpsc::TrySendError::Full(_)) => {
                sub.overflow.store(true, Ordering::SeqCst);
                false
            }
            Err(std::sync::mpsc::TrySendError::Disconnected(_)) => false,
        },
    );
    subs.clear();
}

pub struct PtySubscription {
    pub rx: std::sync::mpsc::Receiver<PtyBroadcastEvent>,
    pub overflow: Arc<AtomicBool>,
    pub snapshot: Vec<u8>,
    pub running: bool,
    pub exit_code: i32,
}

/// Snapshot `output` and register the subscriber under the same locks as publish.
/// A byte is either in `snapshot` or delivered live, not both, and not neither.
pub fn subscribe_pty(pty: &PtyHandle, entry: &Mutex<ProcessEntry>) -> PtySubscription {
    let (tx, rx) = std::sync::mpsc::sync_channel(PTY_SUBSCRIBER_QUEUE);
    let overflow = Arc::new(AtomicBool::new(false));
    let mut subs = pty.subscribers.lock().unwrap();
    let proc = entry.lock().unwrap();
    let snapshot = proc.output.clone();
    let running = proc.running;
    let exit_code = proc.exit_code.unwrap_or(0);
    if running {
        subs.push(PtySub {
            tx,
            overflow: Arc::clone(&overflow),
        });
    }
    PtySubscription {
        rx,
        overflow,
        snapshot,
        running,
        exit_code,
    }
}

pub fn close_pty_master(pty: &PtyHandle) {
    let fd = pty.master_fd.swap(-1, Ordering::SeqCst);
    if fd >= 0 {
        unsafe {
            libc::close(fd);
        }
    }
}

pub struct ProcessEntry {
    pub id: String,
    pub pid: u32,
    pub tag: Option<String>,
    pub running: bool,
    pub exit_code: Option<i32>,
    pub output: ProcessOutputBuffer,
    pub cmd: String,
    pub args: Vec<String>,
    pub envs: HashMap<String, String>,
    pub cwd: Option<String>,
    pub pty_handle: Option<PtyHandle>,
}

pub type ManagedProcess = ProcessEntry;

#[derive(Clone)]
pub struct ProcessRegistry {
    pub(crate) entries: Arc<Mutex<HashMap<String, Arc<Mutex<ProcessEntry>>>>>,
    pub(crate) counter: Arc<AtomicU64>,
}

pub type ProcessTable = ProcessRegistry;

impl Default for ProcessRegistry {
    fn default() -> Self {
        Self::new()
    }
}

impl ProcessRegistry {
    pub fn new() -> Self {
        Self {
            entries: Arc::new(Mutex::new(HashMap::new())),
            counter: Arc::new(AtomicU64::new(1)),
        }
    }

    pub fn next_id(&self) -> String {
        let n = self.counter.fetch_add(1, Ordering::SeqCst);
        format!("proc-{n}")
    }

    pub fn insert(&self, entry: ProcessEntry) -> Arc<Mutex<ProcessEntry>> {
        let id = entry.id.clone();
        let arc = Arc::new(Mutex::new(entry));
        let mut map = self.entries.lock().unwrap();
        map.insert(id, Arc::clone(&arc));
        arc
    }

    pub fn get(&self, id: &str) -> Option<Arc<Mutex<ProcessEntry>>> {
        let map = self.entries.lock().unwrap();
        map.get(id).cloned()
    }

    pub fn get_by_pid(&self, pid: u32) -> Option<Arc<Mutex<ProcessEntry>>> {
        let map = self.entries.lock().unwrap();
        for entry in map.values() {
            if entry.lock().unwrap().pid == pid {
                return Some(Arc::clone(entry));
            }
        }
        None
    }

    pub fn get_by_tag(&self, tag: &str) -> Option<Arc<Mutex<ProcessEntry>>> {
        let map = self.entries.lock().unwrap();
        for entry in map.values() {
            if entry.lock().unwrap().tag.as_deref() == Some(tag) {
                return Some(Arc::clone(entry));
            }
        }
        None
    }

    pub fn list_running(&self) -> Vec<Arc<Mutex<ProcessEntry>>> {
        let map = self.entries.lock().unwrap();
        map.values()
            .filter(|e| e.lock().unwrap().running)
            .cloned()
            .collect()
    }

    pub fn active_count(&self) -> usize {
        let map = self.entries.lock().unwrap();
        map.values().filter(|e| e.lock().unwrap().running).count()
    }
}

pub fn kill_all_managed(registry: &ProcessRegistry) -> usize {
    let map = registry.entries.lock().unwrap();
    let mut killed = 0;
    for entry in map.values() {
        let proc = entry.lock().unwrap();
        if proc.running && proc.pid > 1 {
            unsafe {
                libc::kill(-(proc.pid as i32), libc::SIGKILL);
                libc::kill(proc.pid as i32, libc::SIGKILL);
            }
            if let Some(ref pty) = proc.pty_handle {
                close_pty_master(pty);
            }
            killed += 1;
        }
    }
    killed
}

pub fn handle_command_run(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let body_str = match std::str::from_utf8(&req.body) {
        Ok(s) => s,
        Err(_) => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"invalid_utf8_body"}"#,
            );
            return;
        }
    };

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

    let run_req = RunRequest::from_json(&json);

    let command_str = match run_req.command {
        Some(c) => c,
        None => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"missing_command_field"}"#,
            );
            return;
        }
    };

    let id = run_req.id.unwrap_or_else(|| registry.next_id());
    let eff_workdir = run_req.workdir.unwrap_or_else(|| config.workdir.clone());
    let background = run_req.background;

    let _ = std::fs::create_dir_all(&eff_workdir);

    let shell = if Path::new("/bin/bash").exists() {
        "/bin/bash"
    } else {
        "/bin/sh"
    };

    let mut cmd = Command::new(shell);
    cmd.arg("-c").arg(&command_str);
    cmd.current_dir(&eff_workdir);
    cmd.process_group(0);

    if let Err(e) =
        configure_command_credentials(&mut cmd, run_req.uid, run_req.gid, run_req.user.as_deref())
    {
        let resp = format!(
            r#"{{"error":"invalid_credentials","details":"{}"}}"#,
            escape_json(&e)
        );
        write_json(stream, "400 Bad Request", &resp);
        return;
    }

    for (k, v) in &run_req.envs {
        cmd.env(k, v);
    }

    let mut fds = [0i32; 2];
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        write_json(
            stream,
            "500 Internal Server Error",
            r#"{"error":"failed_to_create_pipe"}"#,
        );
        return;
    }

    let dup_write_fd = unsafe { libc::fcntl(fds[1], libc::F_DUPFD_CLOEXEC, 0) };
    if dup_write_fd < 0 {
        write_json(
            stream,
            "500 Internal Server Error",
            r#"{"error":"failed_to_clone_pipe_cloexec"}"#,
        );
        return;
    }

    let read_file = unsafe { File::from_raw_fd(fds[0]) };
    let write_file = unsafe { File::from_raw_fd(fds[1]) };
    let write_file_dup = unsafe { File::from_raw_fd(dup_write_fd) };

    cmd.stdout(std::process::Stdio::from(write_file));
    cmd.stderr(std::process::Stdio::from(write_file_dup));

    let mut child = match spawn_tracked(&mut cmd) {
        Ok(c) => c,
        Err(e) => {
            let resp = format!(
                r#"{{"error":"failed_to_spawn_command","details":"{}"}}"#,
                escape_json(&e.to_string())
            );
            write_json(stream, "500 Internal Server Error", &resp);
            return;
        }
    };
    // Drop `cmd` immediately so its owned Stdio write-end descriptors are closed in the parent.
    drop(cmd);

    let pid = child.id();

    if background {
        let entry = registry.insert(ProcessEntry {
            id: id.clone(),
            pid,
            tag: None,
            running: true,
            exit_code: None,
            output: Vec::new(),
            cmd: shell.to_string(),
            args: vec!["-c".to_string(), command_str.clone()],
            envs: run_req.envs.clone(),
            cwd: Some(eff_workdir.clone()),
            pty_handle: None,
        });

        let entry_clone = Arc::clone(&entry);
        std::thread::spawn(move || {
            let mut reader = read_file;
            let mut buf = [0u8; 8192];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) => break,
                    Ok(n) => {
                        let mut proc = entry_clone.lock().unwrap();
                        proc.output.extend_from_slice(&buf[..n]);
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                    Err(_) => break,
                }
            }
            let exit_code = match child.wait() {
                Ok(status) => exit_code_from_status(&status),
                Err(_) => -1,
            };
            let mut proc = entry_clone.lock().unwrap();
            proc.running = false;
            proc.exit_code = Some(exit_code);
        });

        let resp = format!(
            r#"{{"id":"{}","pid":{},"running":true,"exit_code":null,"output":"","cursor":0}}"#,
            escape_json(&id),
            pid
        );
        write_json(stream, "200 OK", &resp);
    } else {
        let mut reader = read_file;
        let mut output = Vec::new();
        let mut buf = [0u8; 8192];
        loop {
            match reader.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => output.extend_from_slice(&buf[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                Err(_) => break,
            }
        }
        let exit_code = match child.wait() {
            Ok(status) => exit_code_from_status(&status),
            Err(_) => -1,
        };
        let cursor = output.len();
        let output_str = String::from_utf8_lossy(&output).to_string();

        registry.insert(ProcessEntry {
            id: id.clone(),
            pid,
            tag: None,
            running: false,
            exit_code: Some(exit_code),
            output,
            cmd: shell.to_string(),
            args: vec!["-c".to_string(), command_str.clone()],
            envs: run_req.envs.clone(),
            cwd: Some(eff_workdir.clone()),
            pty_handle: None,
        });

        let resp = format!(
            r#"{{"id":"{}","pid":{},"running":false,"exit_code":{},"output":"{}","cursor":{}}}"#,
            escape_json(&id),
            pid,
            exit_code,
            escape_json(&output_str),
            cursor
        );
        write_json(stream, "200 OK", &resp);
    }
}

pub fn handle_command_status(stream: &mut TcpStream, id: &str, registry: &ProcessRegistry) {
    if let Some(entry) = registry.get(id) {
        let proc = entry.lock().unwrap();
        let exit_code_str = match proc.exit_code {
            Some(c) => c.to_string(),
            None => "null".to_string(),
        };
        let resp = format!(
            r#"{{"id":"{}","pid":{},"running":{},"exit_code":{},"cursor":{}}}"#,
            escape_json(id),
            proc.pid,
            proc.running,
            exit_code_str,
            proc.output.len()
        );
        write_json(stream, "200 OK", &resp);
    } else {
        write_json(stream, "404 Not Found", r#"{"error":"process_not_found"}"#);
    }
}

pub fn handle_command_logs(
    stream: &mut TcpStream,
    id: &str,
    cursor: usize,
    registry: &ProcessRegistry,
) {
    if let Some(entry) = registry.get(id) {
        let proc = entry.lock().unwrap();
        let total_len = proc.output.len();
        let start = cursor.min(total_len);
        let slice = &proc.output[start..total_len];
        let content = String::from_utf8_lossy(slice).to_string();
        let exit_code_str = match proc.exit_code {
            Some(c) => c.to_string(),
            None => "null".to_string(),
        };
        let resp = format!(
            r#"{{"id":"{}","content":"{}","cursor":{},"running":{},"exit_code":{}}}"#,
            escape_json(id),
            escape_json(&content),
            total_len,
            proc.running,
            exit_code_str
        );
        write_json(stream, "200 OK", &resp);
    } else {
        write_json(stream, "404 Not Found", r#"{"error":"process_not_found"}"#);
    }
}

pub fn handle_command_kill(stream: &mut TcpStream, id: &str, registry: &ProcessRegistry) {
    if let Some(entry) = registry.get(id) {
        let (pid, pty) = {
            let proc = entry.lock().unwrap();
            (proc.pid, proc.pty_handle.clone())
        };
        if let Some(pty) = pty {
            close_pty_master(&pty);
        }
        if pid > 1 {
            unsafe {
                libc::kill(-(pid as i32), libc::SIGKILL);
                libc::kill(pid as i32, libc::SIGKILL);
            }
        }
        let resp = format!(r#"{{"status":"killed","id":"{}"}}"#, escape_json(id));
        write_json(stream, "200 OK", &resp);
    } else {
        write_json(stream, "404 Not Found", r#"{"error":"process_not_found"}"#);
    }
}

pub fn base64_encode(data: &[u8]) -> String {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let b0 = chunk[0];
        let b1 = chunk.get(1).copied().unwrap_or(0);
        let b2 = chunk.get(2).copied().unwrap_or(0);

        out.push(TABLE[(b0 >> 2) as usize] as char);
        out.push(TABLE[(((b0 & 0x03) << 4) | (b1 >> 4)) as usize] as char);
        if chunk.len() > 1 {
            out.push(TABLE[(((b1 & 0x0f) << 2) | (b2 >> 6)) as usize] as char);
        } else {
            out.push('=');
        }
        if chunk.len() > 2 {
            out.push(TABLE[(b2 & 0x3f) as usize] as char);
        } else {
            out.push('=');
        }
    }
    out
}

pub fn make_connect_frame(payload: &[u8], is_end: bool) -> Vec<u8> {
    let flag = if is_end { 2u8 } else { 0u8 };
    let len = payload.len() as u32;
    let mut frame = Vec::with_capacity(5 + payload.len());
    frame.push(flag);
    frame.extend_from_slice(&len.to_be_bytes());
    frame.extend_from_slice(payload);
    frame
}

pub fn unpack_connect_payload(body: &[u8]) -> Result<Vec<u8>, String> {
    if body.is_empty() {
        return Ok(Vec::new());
    }

    // Check if entire payload is gzipped
    if body.len() >= 2 && body[0] == 0x1f && body[1] == 0x8b {
        return decompress_if_gzip(body);
    }

    let mut payload = body;
    let mut is_compressed = false;

    // Check for Connect-RPC 5-byte envelope framing
    // Flags: 0 = uncompressed data, 1 = compressed data, 2 = end of stream
    if payload.len() >= 5
        && payload[0] <= 2
        && payload[0] != b'{'
        && payload[0] != b'['
        && payload[0] != b' '
        && payload[0] != b'\t'
        && payload[0] != b'\r'
        && payload[0] != b'\n'
    {
        let flag = payload[0];
        let len = u32::from_be_bytes([payload[1], payload[2], payload[3], payload[4]]) as usize;
        is_compressed = flag == 1;
        if payload.len() >= 5 + len && len > 0 {
            payload = &payload[5..5 + len];
        } else if payload.len() >= 5 {
            payload = &payload[5..];
        }
    }

    if is_compressed || (payload.len() >= 2 && payload[0] == 0x1f && payload[1] == 0x8b) {
        decompress_if_gzip(payload)
    } else {
        Ok(payload.to_vec())
    }
}

pub fn base64_decode(input: &str) -> Vec<u8> {
    let mut out = Vec::new();
    let clean: Vec<u8> = input
        .bytes()
        .filter(|b| !b.is_ascii_whitespace() && *b != b'=')
        .collect();
    for chunk in clean.chunks(4) {
        let mut val = 0u32;
        for (i, &b) in chunk.iter().enumerate() {
            let v = match b {
                b'A'..=b'Z' => b - b'A',
                b'a'..=b'z' => b - b'a' + 26,
                b'0'..=b'9' => b - b'0' + 52,
                b'+' => 62,
                b'/' => 63,
                _ => 0,
            };
            val |= (v as u32) << (18 - i * 6);
        }
        if chunk.len() >= 2 {
            out.push((val >> 16) as u8);
        }
        if chunk.len() >= 3 {
            out.push((val >> 8) as u8);
        }
        if chunk.len() >= 4 {
            out.push(val as u8);
        }
    }
    out
}

pub fn unpack_all_connect_frames(body: &[u8]) -> Vec<(u8, Vec<u8>)> {
    if body.is_empty() {
        return Vec::new();
    }
    let decompressed = if body.len() >= 2 && body[0] == 0x1f && body[1] == 0x8b {
        decompress_if_gzip(body).unwrap_or_else(|_| body.to_vec())
    } else {
        body.to_vec()
    };

    let mut frames = Vec::new();
    let mut idx = 0;
    while idx + 5 <= decompressed.len() {
        let flag = decompressed[idx];
        let len = u32::from_be_bytes([
            decompressed[idx + 1],
            decompressed[idx + 2],
            decompressed[idx + 3],
            decompressed[idx + 4],
        ]) as usize;
        let start = idx + 5;
        let end = start + len;
        if end > decompressed.len() {
            break;
        }
        frames.push((flag, decompressed[start..end].to_vec()));
        idx = end;
    }
    if frames.is_empty() && !decompressed.is_empty() {
        frames.push((0, decompressed));
    }
    frames
}

pub fn extract_selector(json: &JsonValue) -> (Option<u32>, Option<String>) {
    let proc_val = json.get("process");
    let mut pid = None;
    let mut tag = None;

    if let Some(p) = proc_val {
        if let Some(num) = p.as_u64() {
            pid = Some(num as u32);
        } else if let Some(obj) = p.as_object() {
            if let Some(n) = obj.get("pid").and_then(|v| v.as_u64()) {
                pid = Some(n as u32);
            } else if let Some(n) = obj
                .get("selector")
                .and_then(|s| s.get("pid"))
                .and_then(|v| v.as_u64())
            {
                pid = Some(n as u32);
            }
            if let Some(t) = obj.get("tag").and_then(|v| v.as_str()) {
                tag = Some(t.to_string());
            } else if let Some(t) = obj
                .get("selector")
                .and_then(|s| s.get("tag"))
                .and_then(|v| v.as_str())
            {
                tag = Some(t.to_string());
            }
        }
    }
    if pid.is_none() {
        if let Some(n) = json.get("pid").and_then(|v| v.as_u64()) {
            pid = Some(n as u32);
        }
    }
    if tag.is_none() {
        if let Some(t) = json.get("tag").and_then(|v| v.as_str()) {
            tag = Some(t.to_string());
        }
    }
    (pid, tag)
}

pub fn find_process_entry(
    registry: &ProcessRegistry,
    pid: Option<u32>,
    tag: Option<&str>,
) -> Option<Arc<Mutex<ProcessEntry>>> {
    if let Some(p) = pid {
        if let Some(entry) = registry.get_by_pid(p) {
            return Some(entry);
        }
    }
    if let Some(t) = tag {
        if let Some(entry) = registry.get(t) {
            return Some(entry);
        }
        if let Some(entry) = registry.get_by_tag(t) {
            return Some(entry);
        }
    }
    None
}

pub fn extract_pty_size(json: &JsonValue) -> (u16, u16) {
    let pty_obj = json.get("pty").and_then(|v| v.as_object());
    let size_obj = pty_obj
        .and_then(|p| p.get("size"))
        .and_then(|v| v.as_object());
    let cols = size_obj
        .and_then(|s| s.get("cols"))
        .and_then(|v| v.as_u64())
        .unwrap_or(80) as u16;
    let rows = size_obj
        .and_then(|s| s.get("rows"))
        .and_then(|v| v.as_u64())
        .unwrap_or(24) as u16;
    let cols = if cols == 0 { 80 } else { cols };
    let rows = if rows == 0 { 24 } else { rows };
    (cols, rows)
}

pub fn extract_input_data(json: &JsonValue) -> Option<Vec<u8>> {
    let b64_str = json
        .get("pty")
        .or_else(|| json.get("stdin"))
        .or_else(|| {
            json.get("input")
                .and_then(|i| i.get("pty").or_else(|| i.get("stdin")))
        })
        .or_else(|| {
            json.get("input")
                .and_then(|i| i.get("input"))
                .and_then(|i| i.get("pty").or_else(|| i.get("stdin")))
        })
        .and_then(|v| v.as_str());
    b64_str.map(base64_decode)
}

fn write_pty_data(stream: &mut TcpStream, data: &[u8]) -> bool {
    let b64 = base64_encode(data);
    let msg = format!(r#"{{"event":{{"data":{{"pty":"{}"}}}}}}"#, b64);
    let frame = make_connect_frame(msg.as_bytes(), false);
    stream.write_all(&frame).is_ok() && stream.flush().is_ok()
}

fn write_pty_end(stream: &mut TcpStream, exit_code: i32) {
    let end_json = format!(
        r#"{{"event":{{"end":{{"exitCode":{},"exited":true}}}}}}"#,
        exit_code
    );
    let end_frame = make_connect_frame(end_json.as_bytes(), false);
    let _ = stream.write_all(&end_frame);
    let _ = stream.flush();
    let eos_frame = make_connect_frame(b"{}", true);
    let _ = stream.write_all(&eos_frame);
    let _ = stream.flush();
}

fn write_pty_overflow(stream: &mut TcpStream) {
    let err =
        br#"{"error":{"code":"resource_exhausted","message":"pty subscriber queue overflow"}}"#;
    let frame = make_connect_frame(err, true);
    let _ = stream.write_all(&frame);
    let _ = stream.flush();
}

/// Stream replay + live PTY bytes. Returning because the client went away does not kill the process.
fn pump_pty_subscription(stream: &mut TcpStream, sub: PtySubscription, replay: bool) {
    if replay && !sub.snapshot.is_empty() && !write_pty_data(stream, &sub.snapshot) {
        return;
    }
    if !sub.running {
        write_pty_end(stream, sub.exit_code);
        return;
    }
    loop {
        match sub.rx.recv() {
            Ok(PtyBroadcastEvent::Data(data)) => {
                if !write_pty_data(stream, &data) {
                    return;
                }
            }
            Ok(PtyBroadcastEvent::End(code)) => {
                if sub.overflow.load(Ordering::SeqCst) {
                    write_pty_overflow(stream);
                } else {
                    write_pty_end(stream, code);
                }
                return;
            }
            Err(_) => {
                if sub.overflow.load(Ordering::SeqCst) {
                    write_pty_overflow(stream);
                }
                return;
            }
        }
    }
}

pub fn write_unary_connect_response(
    stream: &mut TcpStream,
    req: &HttpRequest,
    status: &str,
    json_body: &str,
) {
    let req_ct = req
        .headers
        .get("content-type")
        .map(|v| v.as_str())
        .unwrap_or("");
    if req_ct.contains("application/connect+json") {
        let mut resp = Vec::new();
        resp.extend_from_slice(&make_connect_frame(json_body.as_bytes(), false));
        resp.extend_from_slice(&make_connect_frame(b"{}", true));
        write_response(stream, status, "application/connect+json", &resp);
    } else {
        write_json(stream, status, json_body);
    }
}

pub fn handle_connectrpc_pty_start(
    stream: &mut TcpStream,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
    json: &JsonValue,
) {
    let proc_obj = json.get("process").unwrap_or(json);

    let (cols, rows) = extract_pty_size(json);

    let cmd_name = proc_obj
        .get("cmd")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| {
            if Path::new("/bin/bash").exists() {
                "/bin/bash".to_string()
            } else {
                "/bin/sh".to_string()
            }
        });

    let cmd_args: Vec<String> = proc_obj
        .get("args")
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|v| v.as_str().map(|s| s.to_string()))
                .collect()
        })
        .unwrap_or_default();

    let eff_workdir = proc_obj
        .get("cwd")
        .or_else(|| proc_obj.get("workdir"))
        .or_else(|| json.get("workdir"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| config.workdir.clone());

    let _ = std::fs::create_dir_all(&eff_workdir);

    let id = json
        .get("id")
        .or_else(|| proc_obj.get("id"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| registry.next_id());

    let tag = json
        .get("tag")
        .or_else(|| proc_obj.get("tag"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string());

    let mut envs = HashMap::new();
    if let Some(env_obj) = proc_obj
        .get("envs")
        .or_else(|| proc_obj.get("envVars"))
        .or_else(|| json.get("envs"))
        .or_else(|| json.get("envVars"))
        .and_then(|v| v.as_object())
    {
        for (k, v) in env_obj {
            if let Some(val) = v.as_str() {
                envs.insert(k.clone(), val.to_string());
            }
        }
    }
    envs.entry("TERM".to_string())
        .or_insert_with(|| "xterm-256color".to_string());

    let uid = proc_obj
        .get("uid")
        .or_else(|| json.get("uid"))
        .and_then(|v| v.as_u64())
        .map(|u| u as u32);
    let gid = proc_obj
        .get("gid")
        .or_else(|| json.get("gid"))
        .and_then(|v| v.as_u64())
        .map(|g| g as u32);
    let req_user = proc_obj
        .get("user")
        .or_else(|| json.get("user"))
        .and_then(|v| v.as_str())
        .map(|s| s.trim())
        .filter(|s| !s.is_empty());

    let mut master: libc::c_int = -1;
    let mut slave: libc::c_int = -1;
    let ws = libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    let ret = unsafe {
        libc::openpty(
            &mut master,
            &mut slave,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            &ws,
        )
    };
    if ret != 0 {
        let err_msg = std::io::Error::last_os_error().to_string();
        let err_json = format!(
            r#"{{"code":"internal","message":"failed to openpty: {}"}}"#,
            escape_json(&err_msg)
        );
        write_response(
            stream,
            "500 Internal Server Error",
            "application/json",
            err_json.as_bytes(),
        );
        return;
    }
    unsafe {
        let flags = libc::fcntl(master, libc::F_GETFD);
        if flags != -1 {
            libc::fcntl(master, libc::F_SETFD, flags | libc::FD_CLOEXEC);
        }
        let status = libc::fcntl(master, libc::F_GETFL);
        if status != -1 {
            libc::fcntl(master, libc::F_SETFL, status | libc::O_NONBLOCK);
        }
    }

    let mut cmd = Command::new(&cmd_name);
    cmd.args(&cmd_args);
    cmd.current_dir(&eff_workdir);
    for (k, v) in &envs {
        cmd.env(k, v);
    }
    if let Err(e) = configure_command_credentials(&mut cmd, uid, gid, req_user) {
        unsafe {
            libc::close(master);
            libc::close(slave);
        }
        let err_json = format!(
            r#"{{"code":"invalid_argument","message":"{}"}}"#,
            escape_json(&e)
        );
        write_response(
            stream,
            "400 Bad Request",
            "application/json",
            err_json.as_bytes(),
        );
        return;
    }

    let slave_in = unsafe { std::fs::File::from_raw_fd(libc::dup(slave)) };
    let slave_out = unsafe { std::fs::File::from_raw_fd(libc::dup(slave)) };
    let slave_err = unsafe { std::fs::File::from_raw_fd(libc::dup(slave)) };
    cmd.stdin(Stdio::from(slave_in));
    cmd.stdout(Stdio::from(slave_out));
    cmd.stderr(Stdio::from(slave_err));

    unsafe {
        cmd.pre_exec(move || {
            if libc::setsid() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            let _ = libc::ioctl(0, libc::TIOCSCTTY, 0);
            Ok(())
        });
    }

    let mut child = match spawn_tracked(&mut cmd) {
        Ok(c) => c,
        Err(e) => {
            unsafe {
                libc::close(master);
                libc::close(slave);
            }
            let err_json = format!(
                r#"{{"code":"internal","message":"failed to spawn command: {}"}}"#,
                escape_json(&e.to_string())
            );
            write_response(
                stream,
                "500 Internal Server Error",
                "application/json",
                err_json.as_bytes(),
            );
            return;
        }
    };
    drop(cmd);
    unsafe {
        libc::close(slave);
    }
    let pid = child.id();

    let master_atomic = Arc::new(AtomicI32::new(master));
    let subscribers = Arc::new(Mutex::new(Vec::new()));

    let pty_handle = PtyHandle {
        master_fd: Arc::clone(&master_atomic),
        subscribers: Arc::clone(&subscribers),
    };

    let entry = registry.insert(ProcessEntry {
        id: id.clone(),
        pid,
        tag: tag.clone(),
        running: true,
        exit_code: None,
        output: Vec::new(),
        cmd: cmd_name.clone(),
        args: cmd_args.clone(),
        envs: envs.clone(),
        cwd: Some(eff_workdir.clone()),
        pty_handle: Some(pty_handle.clone()),
    });

    // Subscribe before the reader starts so the first bytes are queued, not dropped.
    let sub = subscribe_pty(&pty_handle, &entry);

    let master_clone = Arc::clone(&master_atomic);
    let pty_reader = pty_handle.clone();
    let entry_clone = Arc::clone(&entry);

    std::thread::spawn(move || {
        let mut buf = [0u8; 4096];
        loop {
            let fd = master_clone.load(Ordering::SeqCst);
            if fd < 0 {
                break;
            }
            let mut pfd = libc::pollfd {
                fd,
                events: libc::POLLIN,
                revents: 0,
            };
            let rc = unsafe { libc::poll(&mut pfd, 1, 200) };
            if master_clone.load(Ordering::SeqCst) != fd {
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
                publish_pty_chunk(&pty_reader, &entry_clone, &buf[..n as usize]);
            } else if n < 0 {
                let err = std::io::Error::last_os_error();
                let code = err.raw_os_error();
                if code == Some(libc::EINTR) || code == Some(libc::EAGAIN) {
                    continue;
                }
                break;
            } else {
                break;
            }
        }

        let exit_code = match child.wait() {
            Ok(status) => exit_code_from_status(&status),
            Err(_) => -1,
        };
        publish_pty_end(&pty_reader, &entry_clone, exit_code);
        close_pty_master(&pty_reader);
    });

    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        // Client already gone. The PTY process keeps running.
        return;
    }

    let start_json = format!(r#"{{"event":{{"start":{{"pid":{}}}}}}}"#, pid);
    let start_frame = make_connect_frame(start_json.as_bytes(), false);
    if stream.write_all(&start_frame).is_err() || stream.flush().is_err() {
        return;
    }

    // SDK timeoutMs is a client ConnectRPC stream deadline, not a server kill timer.
    pump_pty_subscription(stream, sub, false);
}

pub fn handle_connectrpc_process_start(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let payload = match unpack_connect_payload(&req.body) {
        Ok(p) => p,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"failed to unpack payload: {}"}}"#,
                escape_json(&e)
            );
            write_response(
                stream,
                "400 Bad Request",
                "application/json",
                err_json.as_bytes(),
            );
            return;
        }
    };

    let payload_str = match std::str::from_utf8(&payload) {
        Ok(s) => s,
        Err(_) => {
            write_response(
                stream,
                "400 Bad Request",
                "application/json",
                br#"{"code":"invalid_argument","message":"invalid utf8 payload"}"#,
            );
            return;
        }
    };

    let json = match JsonValue::parse(payload_str) {
        Ok(j) => j,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"invalid json: {}"}}"#,
                escape_json(&e)
            );
            write_response(
                stream,
                "400 Bad Request",
                "application/json",
                err_json.as_bytes(),
            );
            return;
        }
    };

    let is_pty = json
        .get("pty")
        .map(|v| !matches!(v, JsonValue::Null))
        .unwrap_or(false);
    if is_pty {
        handle_connectrpc_pty_start(stream, config, registry, &json);
        return;
    }

    let proc_obj = json.get("process").unwrap_or(&json);

    let mut command_str = String::new();
    if let Some(args_arr) = proc_obj.get("args").and_then(|v| v.as_array()) {
        let args: Vec<String> = args_arr
            .iter()
            .filter_map(|v| v.as_str().map(|s| s.to_string()))
            .collect();
        if args.len() >= 3 && args[0] == "-l" && args[1] == "-c" {
            command_str = args[2].clone();
        } else if args.len() >= 2 && args[0] == "-c" {
            command_str = args[1].clone();
        } else if !args.is_empty() {
            command_str = args.join(" ");
        }
    }
    if command_str.is_empty() {
        if let Some(c) = proc_obj.get("cmd").and_then(|v| v.as_str()) {
            command_str = c.to_string();
        }
    }
    if command_str.is_empty() {
        if let Some(c) = json.get("command").and_then(|v| v.as_str()) {
            command_str = c.to_string();
        }
    }

    let eff_workdir = proc_obj
        .get("cwd")
        .or_else(|| proc_obj.get("workdir"))
        .or_else(|| json.get("workdir"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| config.workdir.clone());

    let id = json
        .get("id")
        .or_else(|| proc_obj.get("id"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| registry.next_id());

    let tag = json
        .get("tag")
        .or_else(|| proc_obj.get("tag"))
        .and_then(|v| v.as_str())
        .map(|s| s.to_string());

    let mut envs = HashMap::new();
    if let Some(env_obj) = proc_obj
        .get("envs")
        .or_else(|| proc_obj.get("envVars"))
        .or_else(|| json.get("envs"))
        .or_else(|| json.get("envVars"))
        .and_then(|v| v.as_object())
    {
        for (k, v) in env_obj {
            if let Some(val) = v.as_str() {
                envs.insert(k.clone(), val.to_string());
            }
        }
    }

    let uid = proc_obj
        .get("uid")
        .or_else(|| json.get("uid"))
        .and_then(|v| v.as_u64())
        .map(|u| u as u32);
    let gid = proc_obj
        .get("gid")
        .or_else(|| json.get("gid"))
        .and_then(|v| v.as_u64())
        .map(|g| g as u32);
    let req_user = proc_obj
        .get("user")
        .or_else(|| json.get("user"))
        .and_then(|v| v.as_str())
        .map(|s| s.trim())
        .filter(|s| !s.is_empty());

    let _ = std::fs::create_dir_all(&eff_workdir);

    let shell = if Path::new("/bin/bash").exists() {
        "/bin/bash"
    } else {
        "/bin/sh"
    };

    let mut cmd = Command::new(shell);
    cmd.arg("-c").arg(&command_str);
    cmd.current_dir(&eff_workdir);
    // Spawn process in a dedicated process group
    cmd.process_group(0);

    if let Err(e) = configure_command_credentials(&mut cmd, uid, gid, req_user) {
        let err_json = format!(
            r#"{{"code":"invalid_argument","message":"{}"}}"#,
            escape_json(&e)
        );
        write_response(
            stream,
            "400 Bad Request",
            "application/json",
            err_json.as_bytes(),
        );
        return;
    }

    for (k, v) in &envs {
        cmd.env(k, v);
    }

    cmd.stdout(std::process::Stdio::piped());
    cmd.stderr(std::process::Stdio::piped());

    let mut child = match spawn_tracked(&mut cmd) {
        Ok(c) => c,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"internal","message":"failed to spawn command: {}"}}"#,
                escape_json(&e.to_string())
            );
            write_response(
                stream,
                "500 Internal Server Error",
                "application/json",
                err_json.as_bytes(),
            );
            return;
        }
    };
    drop(cmd);

    let pid = child.id();

    let entry = registry.insert(ProcessEntry {
        id: id.clone(),
        pid,
        tag: tag.clone(),
        running: true,
        exit_code: None,
        output: Vec::new(),
        cmd: shell.to_string(),
        args: vec!["-c".to_string(), command_str.clone()],
        envs: envs.clone(),
        cwd: Some(eff_workdir.clone()),
        pty_handle: None,
    });

    let child_stdout = child.stdout().take();
    let child_stderr = child.stderr().take();

    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        return;
    }

    let start_json = format!(r#"{{"event":{{"start":{{"pid":{}}}}}}}"#, pid);
    let start_frame = make_connect_frame(start_json.as_bytes(), false);
    if stream.write_all(&start_frame).is_err() || stream.flush().is_err() {
        return;
    }

    let (tx, rx) = std::sync::mpsc::channel::<(bool, Vec<u8>)>();

    if let Some(mut out) = child_stdout {
        let tx_out = tx.clone();
        std::thread::spawn(move || {
            let mut buf = [0u8; 8192];
            let mut line_buf = Vec::new();
            while let Ok(n) = out.read(&mut buf) {
                if n == 0 {
                    if !line_buf.is_empty() {
                        let _ = tx_out.send((false, std::mem::take(&mut line_buf)));
                    }
                    break;
                }
                for &b in &buf[..n] {
                    line_buf.push(b);
                    if b == b'\n' {
                        let _ = tx_out.send((false, std::mem::take(&mut line_buf)));
                    }
                }
                if line_buf.len() >= 65536 {
                    let _ = tx_out.send((false, std::mem::take(&mut line_buf)));
                }
            }
        });
    }

    if let Some(mut err) = child_stderr {
        let tx_err = tx.clone();
        std::thread::spawn(move || {
            let mut buf = [0u8; 8192];
            let mut line_buf = Vec::new();
            while let Ok(n) = err.read(&mut buf) {
                if n == 0 {
                    if !line_buf.is_empty() {
                        let _ = tx_err.send((true, std::mem::take(&mut line_buf)));
                    }
                    break;
                }
                for &b in &buf[..n] {
                    line_buf.push(b);
                    if b == b'\n' {
                        let _ = tx_err.send((true, std::mem::take(&mut line_buf)));
                    }
                }
                if line_buf.len() >= 65536 {
                    let _ = tx_err.send((true, std::mem::take(&mut line_buf)));
                }
            }
        });
    }

    drop(tx);

    while let Ok((is_err, data)) = rx.recv() {
        let b64 = base64_encode(&data);
        let event_json = if is_err {
            format!(r#"{{"event":{{"data":{{"stderr":"{}"}}}}}}"#, b64)
        } else {
            format!(r#"{{"event":{{"data":{{"stdout":"{}"}}}}}}"#, b64)
        };
        let frame = make_connect_frame(event_json.as_bytes(), false);
        if stream.write_all(&frame).is_err() || stream.flush().is_err() {
            break;
        }
        let mut proc = entry.lock().unwrap();
        proc.output.extend_from_slice(&data);
    }

    let exit_code = match child.wait() {
        Ok(status) => exit_code_from_status(&status),
        Err(_) => -1,
    };

    {
        let mut proc = entry.lock().unwrap();
        proc.running = false;
        proc.exit_code = Some(exit_code);
    }

    let end_json = format!(
        r#"{{"event":{{"end":{{"exitCode":{},"exited":true}}}}}}"#,
        exit_code
    );
    let end_frame = make_connect_frame(end_json.as_bytes(), false);
    let _ = stream.write_all(&end_frame);
    let _ = stream.flush();

    let eos_frame = make_connect_frame(b"{}", true);
    let _ = stream.write_all(&eos_frame);
    let _ = stream.flush();
}

pub fn handle_connectrpc_send_signal(
    stream: &mut TcpStream,
    req: &HttpRequest,
    registry: &ProcessRegistry,
) {
    let payload = match unpack_connect_payload(&req.body) {
        Ok(p) => p,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"failed to unpack payload: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let payload_str = match std::str::from_utf8(&payload) {
        Ok(s) => s,
        Err(_) => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"code":"invalid_argument","message":"invalid utf8 payload"}"#,
            );
            return;
        }
    };

    let json = match JsonValue::parse(payload_str) {
        Ok(j) => j,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"invalid json: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let (pid, tag) = extract_selector(&json);
    let signal_str = json.get("signal").and_then(|v| {
        v.as_str()
            .map(|s| s.to_string())
            .or_else(|| v.as_u64().map(|n| n.to_string()))
    });

    let sig = match signal_str.as_deref() {
        Some("SIGNAL_SIGTERM") | Some("SIGTERM") | Some("15") => libc::SIGTERM,
        Some("SIGNAL_SIGKILL") | Some("SIGKILL") | Some("9") => libc::SIGKILL,
        _ => libc::SIGKILL,
    };

    if let Some(entry) = find_process_entry(registry, pid, tag.as_deref()) {
        let (p, was_running, pty_opt) = {
            let mut proc = entry.lock().unwrap();
            let running = proc.running;
            proc.running = false;
            proc.exit_code = Some(128 + sig);
            (proc.pid, running, proc.pty_handle.clone())
        };

        if let Some(pty) = pty_opt {
            // Closing the master wakes the reader; explicit SendSignal is what reaps the PTY.
            close_pty_master(&pty);
        }

        if p > 1 {
            unsafe {
                libc::kill(-(p as i32), sig);
                libc::kill(p as i32, sig);
            }
        }

        if was_running {
            write_unary_connect_response(stream, req, "200 OK", "{}");
            return;
        } else {
            write_json(
                stream,
                "404 Not Found",
                r#"{"code":"not_found","message":"process not found"}"#,
            );
            return;
        }
    }

    if let Some(p) = pid {
        if p > 1 {
            let res_grp = unsafe { libc::kill(-(p as i32), sig) };
            let res_pid = unsafe { libc::kill(p as i32, sig) };
            if res_grp == 0 || res_pid == 0 {
                write_unary_connect_response(stream, req, "200 OK", "{}");
                return;
            }
        }
    }

    write_json(
        stream,
        "404 Not Found",
        r#"{"code":"not_found","message":"process not found"}"#,
    );
}

pub fn handle_connectrpc_send_input(
    stream: &mut TcpStream,
    req: &HttpRequest,
    registry: &ProcessRegistry,
) {
    let payload = match unpack_connect_payload(&req.body) {
        Ok(p) => p,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"failed to unpack payload: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let payload_str = match std::str::from_utf8(&payload) {
        Ok(s) => s,
        Err(_) => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"code":"invalid_argument","message":"invalid utf8 payload"}"#,
            );
            return;
        }
    };

    let json = match JsonValue::parse(payload_str) {
        Ok(j) => j,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"invalid json: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let (pid, tag) = extract_selector(&json);
    let entry = match find_process_entry(registry, pid, tag.as_deref()) {
        Some(e) => e,
        None => {
            write_json(
                stream,
                "404 Not Found",
                r#"{"code":"not_found","message":"process not found"}"#,
            );
            return;
        }
    };

    let (is_running, pty_handle) = {
        let proc = entry.lock().unwrap();
        (proc.running, proc.pty_handle.clone())
    };

    if !is_running {
        write_json(
            stream,
            "404 Not Found",
            r#"{"code":"not_found","message":"process not running"}"#,
        );
        return;
    }

    if let Some(pty) = pty_handle {
        if let Some(input_bytes) = extract_input_data(&json) {
            let fd = pty.master_fd.load(Ordering::SeqCst);
            if fd >= 0 && !input_bytes.is_empty() {
                unsafe {
                    libc::write(
                        fd,
                        input_bytes.as_ptr() as *const libc::c_void,
                        input_bytes.len(),
                    );
                }
            }
        }
    }

    write_unary_connect_response(stream, req, "200 OK", "{}");
}

pub fn handle_connectrpc_process_update(
    stream: &mut TcpStream,
    req: &HttpRequest,
    registry: &ProcessRegistry,
) {
    let payload = match unpack_connect_payload(&req.body) {
        Ok(p) => p,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"failed to unpack payload: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let payload_str = match std::str::from_utf8(&payload) {
        Ok(s) => s,
        Err(_) => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"code":"invalid_argument","message":"invalid utf8 payload"}"#,
            );
            return;
        }
    };

    let json = match JsonValue::parse(payload_str) {
        Ok(j) => j,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"invalid json: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let (pid, tag) = extract_selector(&json);
    let entry = match find_process_entry(registry, pid, tag.as_deref()) {
        Some(e) => e,
        None => {
            write_json(
                stream,
                "404 Not Found",
                r#"{"code":"not_found","message":"process not found"}"#,
            );
            return;
        }
    };

    let (cols, rows) = extract_pty_size(&json);
    let (is_running, proc_pid, pty_handle) = {
        let proc = entry.lock().unwrap();
        (proc.running, proc.pid, proc.pty_handle.clone())
    };

    if !is_running {
        write_json(
            stream,
            "404 Not Found",
            r#"{"code":"not_found","message":"process not running"}"#,
        );
        return;
    }

    if let Some(pty) = pty_handle {
        let fd = pty.master_fd.load(Ordering::SeqCst);
        if fd >= 0 {
            let ws = libc::winsize {
                ws_row: rows,
                ws_col: cols,
                ws_xpixel: 0,
                ws_ypixel: 0,
            };
            unsafe {
                libc::ioctl(fd, libc::TIOCSWINSZ, &ws);
            }
        }
        if proc_pid > 1 {
            unsafe {
                libc::kill(-(proc_pid as i32), libc::SIGWINCH);
                libc::kill(proc_pid as i32, libc::SIGWINCH);
            }
        }
    }

    write_unary_connect_response(stream, req, "200 OK", "{}");
}

pub fn handle_connectrpc_stream_input(
    stream: &mut TcpStream,
    req: &HttpRequest,
    registry: &ProcessRegistry,
) {
    let frames = unpack_all_connect_frames(&req.body);
    let mut current_entry: Option<Arc<Mutex<ProcessEntry>>> = None;

    for (flag, frame_data) in frames {
        if flag == 2 {
            break;
        }
        if let Ok(s) = std::str::from_utf8(&frame_data) {
            if let Ok(json) = JsonValue::parse(s) {
                let event_val = json.get("event").unwrap_or(&json);
                if let Some(start_obj) = event_val.get("start").and_then(|v| v.as_object()) {
                    let (pid, tag) = extract_selector(&JsonValue::Object(start_obj.clone()));
                    current_entry = find_process_entry(registry, pid, tag.as_deref());
                } else if let Some(data_obj) = event_val.get("data").and_then(|v| v.as_object()) {
                    if let Some(ref entry) = current_entry {
                        let (is_running, pty_handle) = {
                            let proc = entry.lock().unwrap();
                            (proc.running, proc.pty_handle.clone())
                        };
                        if is_running {
                            if let Some(pty) = pty_handle {
                                if let Some(input_bytes) =
                                    extract_input_data(&JsonValue::Object(data_obj.clone()))
                                {
                                    let fd = pty.master_fd.load(Ordering::SeqCst);
                                    if fd >= 0 && !input_bytes.is_empty() {
                                        unsafe {
                                            libc::write(
                                                fd,
                                                input_bytes.as_ptr() as *const libc::c_void,
                                                input_bytes.len(),
                                            );
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    write_unary_connect_response(stream, req, "200 OK", "{}");
}

pub fn handle_connectrpc_process_connect(
    stream: &mut TcpStream,
    req: &HttpRequest,
    registry: &ProcessRegistry,
) {
    let payload = match unpack_connect_payload(&req.body) {
        Ok(p) => p,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"failed to unpack payload: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let payload_str = match std::str::from_utf8(&payload) {
        Ok(s) => s,
        Err(_) => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"code":"invalid_argument","message":"invalid utf8 payload"}"#,
            );
            return;
        }
    };

    let json = match JsonValue::parse(payload_str) {
        Ok(j) => j,
        Err(e) => {
            let err_json = format!(
                r#"{{"code":"invalid_argument","message":"invalid json: {}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &err_json);
            return;
        }
    };

    let (pid, tag) = extract_selector(&json);
    let entry = match find_process_entry(registry, pid, tag.as_deref()) {
        Some(e) => e,
        None => {
            write_json(
                stream,
                "404 Not Found",
                r#"{"code":"not_found","message":"process not found"}"#,
            );
            return;
        }
    };

    let (proc_pid, is_pty, pty_opt) = {
        let proc = entry.lock().unwrap();
        (proc.pid, proc.pty_handle.is_some(), proc.pty_handle.clone())
    };

    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        return;
    }

    let start_json = format!(r#"{{"event":{{"start":{{"pid":{}}}}}}}"#, proc_pid);
    let start_frame = make_connect_frame(start_json.as_bytes(), false);
    if stream.write_all(&start_frame).is_err() || stream.flush().is_err() {
        return;
    }

    if is_pty {
        let pty = pty_opt.unwrap();
        let sub = subscribe_pty(&pty, &entry);
        pump_pty_subscription(stream, sub, true);
    } else {
        let (output, exit_code) = {
            let proc = entry.lock().unwrap();
            (proc.output.clone(), proc.exit_code.unwrap_or(0))
        };
        if !output.is_empty() {
            let b64 = base64_encode(&output);
            let msg = format!(r#"{{"event":{{"data":{{"stdout":"{}"}}}}}}"#, b64);
            let frame = make_connect_frame(msg.as_bytes(), false);
            let _ = stream.write_all(&frame);
            let _ = stream.flush();
        }
        let end_json = format!(
            r#"{{"event":{{"end":{{"exitCode":{},"exited":true}}}}}}"#,
            exit_code
        );
        let end_frame = make_connect_frame(end_json.as_bytes(), false);
        let _ = stream.write_all(&end_frame);
        let _ = stream.flush();
        let eos_frame = make_connect_frame(b"{}", true);
        let _ = stream.write_all(&eos_frame);
        let _ = stream.flush();
    }
}

pub fn handle_connectrpc_process_list(
    stream: &mut TcpStream,
    req: &HttpRequest,
    registry: &ProcessRegistry,
) {
    let running = registry.list_running();
    let mut procs_json = Vec::new();
    for entry_arc in running {
        let proc = entry_arc.lock().unwrap();
        let mut envs_json = Vec::new();
        for (k, v) in &proc.envs {
            envs_json.push(format!(r#""{}":"{}""#, escape_json(k), escape_json(v)));
        }
        let args_json: Vec<String> = proc
            .args
            .iter()
            .map(|a| format!(r#""{}""#, escape_json(a)))
            .collect();
        let tag_field = match &proc.tag {
            Some(t) => format!(r#","tag":"{}""#, escape_json(t)),
            None => String::new(),
        };
        let cwd_field = match &proc.cwd {
            Some(c) => format!(r#","cwd":"{}""#, escape_json(c)),
            None => String::new(),
        };
        procs_json.push(format!(
            r#"{{"pid":{},"config":{{"cmd":"{}","args":[{}],"envs":{{{}}}{}}}{}}}"#,
            proc.pid,
            escape_json(&proc.cmd),
            args_json.join(","),
            envs_json.join(","),
            cwd_field,
            tag_field
        ));
    }
    let resp = format!(r#"{{"processes":[{}]}}"#, procs_json.join(","));
    write_unary_connect_response(stream, req, "200 OK", &resp);
}

pub fn handle_connectrpc_close_stdin(
    stream: &mut TcpStream,
    req: &HttpRequest,
    _registry: &ProcessRegistry,
) {
    write_unary_connect_response(stream, req, "200 OK", "{}");
}
