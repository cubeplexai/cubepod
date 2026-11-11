//! Process Registry, Execution & Command Management for podd.

use std::collections::HashMap;
use std::fs::File;
use std::io::Read;
use std::net::TcpStream;
use std::os::unix::io::FromRawFd;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::Path;
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use crate::http::{write_json, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::DaemonConfig;

pub type ProcessOutputBuffer = Vec<u8>;

pub struct ProcessEntry {
    pub id: String,
    pub pid: u32,
    pub running: bool,
    pub exit_code: Option<i32>,
    pub output: ProcessOutputBuffer,
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

    let command_str = match json.get("command").and_then(|v| v.as_str()) {
        Some(c) => c.to_string(),
        None => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"missing_command_field"}"#,
            );
            return;
        }
    };

    let id = json
        .get("id")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| registry.next_id());

    let eff_workdir = json
        .get("workdir")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .unwrap_or_else(|| config.workdir.clone());

    let background = json
        .get("background")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    let uid = json.get("uid").and_then(|v| v.as_u64()).map(|u| u as u32);
    let gid = json.get("gid").and_then(|v| v.as_u64()).map(|g| g as u32);

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

    let _ = std::fs::create_dir_all(&eff_workdir);

    let shell = if Path::new("/bin/bash").exists() {
        "/bin/bash"
    } else {
        "/bin/sh"
    };

    let mut cmd = Command::new(shell);
    cmd.arg("-c").arg(&command_str);
    cmd.current_dir(&eff_workdir);
    for (k, v) in &envs {
        cmd.env(k, v);
    }
    cmd.process_group(0);

    if unsafe { libc::geteuid() == 0 } {
        if let Some(u) = uid {
            cmd.uid(u);
        }
        if let Some(g) = gid {
            cmd.gid(g);
        }
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

    let mut child = match cmd.spawn() {
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
            running: true,
            exit_code: None,
            output: Vec::new(),
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
                Ok(status) => status
                    .code()
                    .unwrap_or_else(|| status.signal().map(|s| 128 + s).unwrap_or(-1)),
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
            Ok(status) => status
                .code()
                .unwrap_or_else(|| status.signal().map(|s| 128 + s).unwrap_or(-1)),
            Err(_) => -1,
        };
        let cursor = output.len();
        let output_str = String::from_utf8_lossy(&output).to_string();

        registry.insert(ProcessEntry {
            id: id.clone(),
            pid,
            running: false,
            exit_code: Some(exit_code),
            output,
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
        let pid = {
            let proc = entry.lock().unwrap();
            proc.pid
        };
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
