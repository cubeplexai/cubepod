//! Process Registry, Execution & Command Management for podd.

use std::collections::HashMap;
use std::fs::File;
use std::io::{Read, Write};
use std::net::TcpStream;
use std::os::unix::io::FromRawFd;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::Path;
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use crate::files::decompress_if_gzip;
use crate::http::{write_json, write_response, HttpRequest};
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

    pub fn get_by_pid(&self, pid: u32) -> Option<Arc<Mutex<ProcessEntry>>> {
        let map = self.entries.lock().unwrap();
        for entry in map.values() {
            if entry.lock().unwrap().pid == pid {
                return Some(Arc::clone(entry));
            }
        }
        None
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

    // Spawn process in a dedicated process group (libc::setpgid)
    cmd.process_group(0);
    unsafe {
        cmd.pre_exec(|| {
            libc::setpgid(0, 0);
            Ok(())
        });
    }

    if unsafe { libc::geteuid() == 0 } {
        if let Some(u) = uid {
            cmd.uid(u);
        }
        if let Some(g) = gid {
            cmd.gid(g);
        }
    }

    cmd.stdout(std::process::Stdio::piped());
    cmd.stderr(std::process::Stdio::piped());

    let mut child = match cmd.spawn() {
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
        running: true,
        exit_code: None,
        output: Vec::new(),
    });

    let child_stdout = child.stdout.take();
    let child_stderr = child.stderr.take();

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
        Ok(status) => status
            .code()
            .unwrap_or_else(|| status.signal().map(|s| 128 + s).unwrap_or(-1)),
        Err(_) => -1,
    };

    {
        let mut proc = entry.lock().unwrap();
        proc.running = false;
        proc.exit_code = Some(exit_code);
    }

    let end_json = format!(
        r#"{{"event":{{"end":{{"exitCode":{},"exit_code":{},"exited":true}}}}}}"#,
        exit_code, exit_code
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

    let pid = json
        .get("process")
        .and_then(|p| {
            p.get("pid")
                .or_else(|| p.get("selector").and_then(|s| s.get("pid")))
                .or_else(|| p.as_u64().map(|_| p))
        })
        .or_else(|| json.get("pid"))
        .and_then(|v| v.as_u64())
        .map(|v| v as u32);

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

    if let Some(p) = pid {
        let mut found = false;
        if let Some(entry) = registry.get_by_pid(p) {
            let mut proc = entry.lock().unwrap();
            proc.running = false;
            proc.exit_code = Some(128 + sig);
            found = true;
        }

        if p > 1 {
            let res_grp = unsafe { libc::kill(-(p as i32), sig) };
            let res_pid = unsafe { libc::kill(p as i32, sig) };
            if res_grp == 0 || res_pid == 0 {
                found = true;
            }
        }

        if found {
            write_json(stream, "200 OK", "{}");
        } else {
            write_json(
                stream,
                "404 Not Found",
                r#"{"code":"not_found","message":"process not found"}"#,
            );
        }
    } else {
        let tag = json
            .get("process")
            .and_then(|p| {
                p.get("tag")
                    .or_else(|| p.get("selector").and_then(|s| s.get("tag")))
            })
            .or_else(|| json.get("tag"))
            .or_else(|| json.get("id"))
            .and_then(|v| v.as_str());

        if let Some(t) = tag {
            if let Some(entry) = registry.get(t) {
                let p = entry.lock().unwrap().pid;
                if p > 1 {
                    unsafe {
                        libc::kill(-(p as i32), sig);
                        libc::kill(p as i32, sig);
                    }
                }
                let mut proc = entry.lock().unwrap();
                proc.running = false;
                proc.exit_code = Some(128 + sig);
                write_json(stream, "200 OK", "{}");
                return;
            }
        }

        write_json(
            stream,
            "404 Not Found",
            r#"{"code":"not_found","message":"process not found"}"#,
        );
    }
}
