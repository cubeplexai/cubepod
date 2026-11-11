//! `podd` — CubePod In-Guest Execution & File Daemon (Rust).
//!
//! Runs inside the Kata MicroVM after `podd-init` has completed the persistent OverlayFS `pivot_root`.
//! Exposes data-plane endpoints on `:49983` conforming to CEP-0002:
//! - Foreground & background command execution (`POST /v1/commands/run`)
//! - Managed process tracking and log streaming (`GET /v1/commands/{id}/status`, `GET /v1/commands/{id}/logs?cursor=N`)
//! - Process group termination (`POST /v1/commands/{id}/kill`)
//! - UID/GID-aware file read/write, deletion, directory listing, and atomic archive-sync (`PUT/GET/DELETE /v1/files`, `GET /v1/files/list`, `POST /v1/files/archive-sync`)
//! - Pre-suspend filesystem flush (`POST /v1/lifecycle/prepare-stop`)
//! - Daemon health (`GET /health`)

use std::collections::HashMap;
use std::env;
use std::fs::File;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::io::FromRawFd;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::Path;
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

// ============================================================================
// Minimal JSON Parser & Serializer (Zero-Dependency)
// ============================================================================

#[derive(Debug, Clone, PartialEq)]
pub enum JsonValue {
    Null,
    Bool(bool),
    Number(f64),
    String(String),
    Array(Vec<JsonValue>),
    Object(HashMap<String, JsonValue>),
}

impl JsonValue {
    pub fn as_str(&self) -> Option<&str> {
        match self {
            JsonValue::String(s) => Some(s.as_str()),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            JsonValue::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_u64(&self) -> Option<u64> {
        match self {
            JsonValue::Number(n) if *n >= 0.0 => Some(*n as u64),
            _ => None,
        }
    }

    pub fn get(&self, key: &str) -> Option<&JsonValue> {
        match self {
            JsonValue::Object(map) => map.get(key),
            _ => None,
        }
    }

    pub fn as_object(&self) -> Option<&HashMap<String, JsonValue>> {
        match self {
            JsonValue::Object(map) => Some(map),
            _ => None,
        }
    }

    pub fn parse(s: &str) -> Result<Self, String> {
        let mut p = JsonParser::new(s);
        let val = p.parse_value()?;
        p.skip_whitespace();
        if p.chars.next().is_some() {
            return Err("trailing characters after json".to_string());
        }
        Ok(val)
    }
}

struct JsonParser<'a> {
    chars: std::iter::Peekable<std::str::Chars<'a>>,
}

impl<'a> JsonParser<'a> {
    fn new(input: &'a str) -> Self {
        Self {
            chars: input.chars().peekable(),
        }
    }

    fn skip_whitespace(&mut self) {
        while let Some(&c) = self.chars.peek() {
            if c.is_whitespace() {
                self.chars.next();
            } else {
                break;
            }
        }
    }

    fn parse_value(&mut self) -> Result<JsonValue, String> {
        self.skip_whitespace();
        match self.chars.peek() {
            Some('"') => self.parse_string().map(JsonValue::String),
            Some('{') => self.parse_object().map(JsonValue::Object),
            Some('[') => self.parse_array().map(JsonValue::Array),
            Some('t') | Some('f') => self.parse_bool().map(JsonValue::Bool),
            Some('n') => self.parse_null().map(|_| JsonValue::Null),
            Some(&c) if c == '-' || c.is_ascii_digit() => {
                self.parse_number().map(JsonValue::Number)
            }
            Some(c) => Err(format!("unexpected character in json: {c}")),
            None => Err("unexpected end of json input".to_string()),
        }
    }

    fn parse_string(&mut self) -> Result<String, String> {
        self.chars.next(); // consume opening quote
        let mut s = String::new();
        while let Some(c) = self.chars.next() {
            match c {
                '"' => return Ok(s),
                '\\' => match self.chars.next() {
                    Some('"') => s.push('"'),
                    Some('\\') => s.push('\\'),
                    Some('/') => s.push('/'),
                    Some('b') => s.push('\u{08}'),
                    Some('f') => s.push('\u{0C}'),
                    Some('n') => s.push('\n'),
                    Some('r') => s.push('\r'),
                    Some('t') => s.push('\t'),
                    Some('u') => {
                        let mut hex = String::with_capacity(4);
                        for _ in 0..4 {
                            if let Some(h) = self.chars.next() {
                                hex.push(h);
                            } else {
                                return Err("incomplete unicode escape".to_string());
                            }
                        }
                        let code = u32::from_str_radix(&hex, 16)
                            .map_err(|e| format!("invalid hex in unicode escape: {e}"))?;
                        let ch = char::from_u32(code)
                            .ok_or_else(|| "invalid unicode code point".to_string())?;
                        s.push(ch);
                    }
                    Some(other) => s.push(other),
                    None => return Err("unterminated escape sequence".to_string()),
                },
                other => s.push(other),
            }
        }
        Err("unterminated string".to_string())
    }

    fn parse_object(&mut self) -> Result<HashMap<String, JsonValue>, String> {
        self.chars.next(); // consume '{'
        let mut map = HashMap::new();
        self.skip_whitespace();
        if let Some(&'}') = self.chars.peek() {
            self.chars.next();
            return Ok(map);
        }
        loop {
            self.skip_whitespace();
            let key = match self.chars.peek() {
                Some('"') => self.parse_string()?,
                _ => return Err("expected string key in object".to_string()),
            };
            self.skip_whitespace();
            match self.chars.next() {
                Some(':') => {}
                _ => return Err("expected ':' after object key".to_string()),
            }
            let val = self.parse_value()?;
            map.insert(key, val);
            self.skip_whitespace();
            match self.chars.next() {
                Some(',') => {
                    self.skip_whitespace();
                    if let Some(&'}') = self.chars.peek() {
                        self.chars.next();
                        return Ok(map);
                    }
                }
                Some('}') => return Ok(map),
                _ => return Err("expected ',' or '}' in object".to_string()),
            }
        }
    }

    fn parse_array(&mut self) -> Result<Vec<JsonValue>, String> {
        self.chars.next(); // consume '['
        let mut arr = Vec::new();
        self.skip_whitespace();
        if let Some(&']') = self.chars.peek() {
            self.chars.next();
            return Ok(arr);
        }
        loop {
            let val = self.parse_value()?;
            arr.push(val);
            self.skip_whitespace();
            match self.chars.next() {
                Some(',') => {
                    self.skip_whitespace();
                    if let Some(&']') = self.chars.peek() {
                        self.chars.next();
                        return Ok(arr);
                    }
                }
                Some(']') => return Ok(arr),
                _ => return Err("expected ',' or ']' in array".to_string()),
            }
        }
    }

    fn parse_bool(&mut self) -> Result<bool, String> {
        let mut s = String::new();
        while let Some(&c) = self.chars.peek() {
            if c.is_ascii_alphabetic() {
                s.push(self.chars.next().unwrap());
            } else {
                break;
            }
        }
        match s.as_str() {
            "true" => Ok(true),
            "false" => Ok(false),
            _ => Err(format!("invalid boolean: {s}")),
        }
    }

    fn parse_null(&mut self) -> Result<(), String> {
        let mut s = String::new();
        while let Some(&c) = self.chars.peek() {
            if c.is_ascii_alphabetic() {
                s.push(self.chars.next().unwrap());
            } else {
                break;
            }
        }
        if s == "null" {
            Ok(())
        } else {
            Err(format!("invalid null: {s}"))
        }
    }

    fn parse_number(&mut self) -> Result<f64, String> {
        let mut s = String::new();
        while let Some(&c) = self.chars.peek() {
            if c.is_ascii_digit() || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
                s.push(self.chars.next().unwrap());
            } else {
                break;
            }
        }
        s.parse::<f64>()
            .map_err(|e| format!("invalid number '{s}': {e}"))
    }
}

pub fn escape_json(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 8);
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0C}' => out.push_str("\\f"),
            c if (c as u32) < 0x20 => {
                use std::fmt::Write as _;
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out
}

pub fn url_decode(s: &str) -> String {
    let mut bytes = Vec::with_capacity(s.len());
    let mut iter = s.as_bytes().iter().copied();
    while let Some(b) = iter.next() {
        if b == b'%' {
            let h1 = iter.next();
            let h2 = iter.next();
            if let (Some(c1), Some(c2)) = (h1, h2) {
                let hex = [c1, c2];
                if let Ok(hex_str) = std::str::from_utf8(&hex) {
                    if let Ok(val) = u8::from_str_radix(hex_str, 16) {
                        bytes.push(val);
                        continue;
                    }
                }
                bytes.push(b'%');
                bytes.push(c1);
                bytes.push(c2);
            } else {
                bytes.push(b'%');
                if let Some(c1) = h1 {
                    bytes.push(c1);
                }
            }
        } else if b == b'+' {
            bytes.push(b' ');
        } else {
            bytes.push(b);
        }
    }
    String::from_utf8_lossy(&bytes).to_string()
}

pub fn parse_target(target: &str) -> (&str, HashMap<String, String>) {
    let (path, query_str) = match target.split_once('?') {
        Some((p, q)) => (p, Some(q)),
        None => (target, None),
    };

    let mut params = HashMap::new();
    if let Some(q) = query_str {
        for pair in q.split('&') {
            if pair.is_empty() {
                continue;
            }
            let (k, v) = match pair.split_once('=') {
                Some((k, v)) => (k, v),
                None => (pair, ""),
            };
            params.insert(url_decode(k), url_decode(v));
        }
    }
    (path, params)
}

fn find_subsequence(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

// ============================================================================
// Process Registry & Daemon State
// ============================================================================

struct ProcessEntry {
    id: String,
    pid: u32,
    running: bool,
    exit_code: Option<i32>,
    output: Vec<u8>,
}

#[derive(Clone)]
pub struct ProcessRegistry {
    entries: Arc<Mutex<HashMap<String, Arc<Mutex<ProcessEntry>>>>>,
    counter: Arc<AtomicU64>,
}

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

    fn insert(&self, entry: ProcessEntry) -> Arc<Mutex<ProcessEntry>> {
        let id = entry.id.clone();
        let arc = Arc::new(Mutex::new(entry));
        let mut map = self.entries.lock().unwrap();
        map.insert(id, Arc::clone(&arc));
        arc
    }

    fn get(&self, id: &str) -> Option<Arc<Mutex<ProcessEntry>>> {
        let map = self.entries.lock().unwrap();
        map.get(id).cloned()
    }

    pub fn active_count(&self) -> usize {
        let map = self.entries.lock().unwrap();
        map.values().filter(|e| e.lock().unwrap().running).count()
    }
}

#[derive(Clone)]
pub struct DaemonConfig {
    pub workdir: String,
}

impl Default for DaemonConfig {
    fn default() -> Self {
        Self {
            workdir: "/workspace".to_string(),
        }
    }
}

// ============================================================================
// HTTP Server & Request Handling
// ============================================================================

pub struct HttpRequest {
    pub method: String,
    pub target: String,
    pub headers: HashMap<String, String>,
    pub body: Vec<u8>,
}

pub fn read_http_request(stream: &mut TcpStream) -> Result<HttpRequest, String> {
    let mut header_buf = Vec::new();
    let mut buf = [0u8; 1024];
    let header_end;

    loop {
        let n = stream
            .read(&mut buf)
            .map_err(|e| format!("read error: {e}"))?;
        if n == 0 {
            if header_buf.is_empty() {
                return Err("client closed connection before sending request".to_string());
            }
            return Err("unexpected EOF while reading headers".to_string());
        }
        header_buf.extend_from_slice(&buf[..n]);

        if let Some(pos) = find_subsequence(&header_buf, b"\r\n\r\n") {
            header_end = (pos, pos + 4);
            break;
        } else if let Some(pos) = find_subsequence(&header_buf, b"\n\n") {
            header_end = (pos, pos + 2);
            break;
        }

        if header_buf.len() > 65536 {
            return Err("request headers too large".to_string());
        }
    }

    let header_str = String::from_utf8_lossy(&header_buf[..header_end.0]);
    let mut lines = header_str.lines();
    let request_line = lines
        .next()
        .ok_or_else(|| "empty request line".to_string())?;

    let mut parts = request_line.split_whitespace();
    let method = parts
        .next()
        .ok_or_else(|| "missing method".to_string())?
        .to_uppercase();
    let target = parts
        .next()
        .ok_or_else(|| "missing target".to_string())?
        .to_string();

    let mut headers = HashMap::new();
    for line in lines {
        if let Some((k, v)) = line.split_once(':') {
            headers.insert(k.trim().to_ascii_lowercase(), v.trim().to_string());
        }
    }

    let content_length: usize = headers
        .get("content-length")
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);

    let mut body = Vec::with_capacity(content_length);
    let initial_body = &header_buf[header_end.1..];
    let to_take = initial_body.len().min(content_length);
    body.extend_from_slice(&initial_body[..to_take]);

    while body.len() < content_length {
        let needed = content_length - body.len();
        let chunk_size = needed.min(buf.len());
        let n = stream
            .read(&mut buf[..chunk_size])
            .map_err(|e| format!("body read error: {e}"))?;
        if n == 0 {
            break;
        }
        body.extend_from_slice(&buf[..n]);
    }

    Ok(HttpRequest {
        method,
        target,
        headers,
        body,
    })
}

fn write_response(stream: &mut TcpStream, status: &str, content_type: &str, body: &[u8]) {
    let header = format!(
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    let _ = stream.write_all(header.as_bytes());
    let _ = stream.write_all(body);
    let _ = stream.flush();
}

fn write_json(stream: &mut TcpStream, status: &str, json: &str) {
    write_response(stream, status, "application/json", json.as_bytes());
}

fn handle_health(stream: &mut TcpStream, config: &DaemonConfig, registry: &ProcessRegistry) {
    let active = registry.active_count();
    let body = format!(
        r#"{{"status":"ok","daemon":"podd-rs","workdir":"{}","active_processes":{}}}"#,
        escape_json(&config.workdir),
        active
    );
    write_json(stream, "200 OK", &body);
}

fn handle_prepare_stop(stream: &mut TcpStream, registry: &ProcessRegistry) {
    unsafe {
        libc::sync();
    }
    let active = registry.active_count();
    let body = format!(r#"{{"status":"synced","active_processes":{active}}}"#);
    write_json(stream, "200 OK", &body);
}

fn handle_command_run(
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

fn handle_command_status(stream: &mut TcpStream, id: &str, registry: &ProcessRegistry) {
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

fn handle_command_logs(
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

fn handle_command_kill(stream: &mut TcpStream, id: &str, registry: &ProcessRegistry) {
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

fn handle_file_put(
    stream: &mut TcpStream,
    req: &HttpRequest,
    query_params: &HashMap<String, String>,
) {
    let raw_path = match query_params.get("path") {
        Some(p) if !p.is_empty() => p,
        _ => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"missing_path_parameter"}"#,
            );
            return;
        }
    };

    let path = Path::new(raw_path);
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() {
            if let Err(e) = std::fs::create_dir_all(parent) {
                let resp = format!(
                    r#"{{"error":"failed_to_create_parent_dir","details":"{}"}}"#,
                    escape_json(&e.to_string())
                );
                write_json(stream, "500 Internal Server Error", &resp);
                return;
            }
        }
    }

    if let Err(e) = std::fs::write(path, &req.body) {
        let resp = format!(
            r#"{{"error":"failed_to_write_file","details":"{}"}}"#,
            escape_json(&e.to_string())
        );
        write_json(stream, "500 Internal Server Error", &resp);
        return;
    }

    if let Some(mode_str) = query_params.get("mode") {
        let mode_val =
            u32::from_str_radix(mode_str.trim_start_matches("0o").trim_start_matches('0'), 8)
                .or_else(|_| mode_str.parse::<u32>())
                .unwrap_or(0o644);
        let perms = std::fs::Permissions::from_mode(mode_val);
        let _ = std::fs::set_permissions(path, perms);
    }

    if unsafe { libc::geteuid() == 0 } {
        let uid = query_params
            .get("uid")
            .and_then(|u| u.parse::<u32>().ok())
            .unwrap_or(1000);
        let gid = query_params
            .get("gid")
            .and_then(|g| g.parse::<u32>().ok())
            .unwrap_or(1000);
        if let Ok(c_path) = std::ffi::CString::new(raw_path.as_bytes()) {
            unsafe {
                libc::chown(c_path.as_ptr(), uid, gid);
            }
        }
    }

    let resp = format!(
        r#"{{"path":"{}","size":{}}}"#,
        escape_json(raw_path),
        req.body.len()
    );
    write_json(stream, "200 OK", &resp);
}

fn handle_file_get(stream: &mut TcpStream, query_params: &HashMap<String, String>) {
    let raw_path = match query_params.get("path") {
        Some(p) if !p.is_empty() => p,
        _ => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"missing_path_parameter"}"#,
            );
            return;
        }
    };

    let path = Path::new(raw_path);
    if !path.exists() {
        write_json(stream, "404 Not Found", r#"{"error":"file_not_found"}"#);
        return;
    }

    if path.is_dir() {
        write_json(
            stream,
            "400 Bad Request",
            r#"{"error":"path_is_directory"}"#,
        );
        return;
    }

    match std::fs::read(path) {
        Ok(data) => {
            write_response(stream, "200 OK", "application/octet-stream", &data);
        }
        Err(e) => {
            let resp = format!(
                r#"{{"error":"failed_to_read_file","details":"{}"}}"#,
                escape_json(&e.to_string())
            );
            write_json(stream, "500 Internal Server Error", &resp);
        }
    }
}

fn handle_file_list(
    stream: &mut TcpStream,
    query_params: &HashMap<String, String>,
    config: &DaemonConfig,
) {
    let raw_path = query_params
        .get("path")
        .map(|s| s.as_str())
        .unwrap_or(&config.workdir);

    let path = Path::new(raw_path);
    if !path.exists() || !path.is_dir() {
        write_json(
            stream,
            "404 Not Found",
            r#"{"error":"directory_not_found"}"#,
        );
        return;
    }

    match std::fs::read_dir(path) {
        Ok(entries) => {
            let mut list = Vec::new();
            for entry in entries.flatten() {
                let name = entry.file_name().to_string_lossy().to_string();
                let full_path = entry.path().to_string_lossy().to_string();
                let meta = entry.metadata().ok();
                let is_dir = meta.as_ref().map(|m| m.is_dir()).unwrap_or(false);
                let size = meta.as_ref().map(|m| m.len()).unwrap_or(0);
                list.push((name, full_path, is_dir, size));
            }
            list.sort_by(|a, b| a.0.cmp(&b.0));

            let mut json_items = Vec::new();
            for (name, fpath, is_dir, size) in list {
                json_items.push(format!(
                    r#"{{"name":"{}","path":"{}","is_dir":{},"size":{}}}"#,
                    escape_json(&name),
                    escape_json(&fpath),
                    is_dir,
                    size
                ));
            }
            let resp = format!(r#"{{"entries":[{}]}}"#, json_items.join(","));
            write_json(stream, "200 OK", &resp);
        }
        Err(e) => {
            let resp = format!(
                r#"{{"error":"failed_to_read_dir","details":"{}"}}"#,
                escape_json(&e.to_string())
            );
            write_json(stream, "500 Internal Server Error", &resp);
        }
    }
}

fn handle_file_delete(stream: &mut TcpStream, query_params: &HashMap<String, String>) {
    let raw_path = match query_params.get("path") {
        Some(p) if !p.is_empty() => p,
        _ => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"missing_path_parameter"}"#,
            );
            return;
        }
    };

    let path = Path::new(raw_path);
    if !path.exists() && !path.is_symlink() {
        write_json(stream, "404 Not Found", r#"{"error":"file_not_found"}"#);
        return;
    }

    let result = if path.is_dir() {
        std::fs::remove_dir_all(path)
    } else {
        std::fs::remove_file(path)
    };

    match result {
        Ok(_) => {
            let resp = format!(r#"{{"deleted":"{}"}}"#, escape_json(raw_path));
            write_json(stream, "200 OK", &resp);
        }
        Err(e) => {
            let resp = format!(
                r#"{{"error":"failed_to_delete","details":"{}"}}"#,
                escape_json(&e.to_string())
            );
            write_json(stream, "500 Internal Server Error", &resp);
        }
    }
}

// ============================================================================
// Tar & Archive Handling (Zero-Dependency)
// ============================================================================

struct TarEntry {
    path: String,
    mode: u32,
    typeflag: u8,
    linkname: String,
    data: Vec<u8>,
}

fn parse_octal(bytes: &[u8]) -> u64 {
    let s = String::from_utf8_lossy(bytes);
    let trimmed = s.trim_matches(|c: char| c.is_whitespace() || c == '\0');
    if trimmed.is_empty() {
        return 0;
    }
    u64::from_str_radix(trimmed, 8).unwrap_or(0)
}

fn parse_tar_string(bytes: &[u8]) -> String {
    let end = bytes.iter().position(|&b| b == 0).unwrap_or(bytes.len());
    String::from_utf8_lossy(&bytes[..end]).to_string()
}

fn is_invalid_tar_path(path: &str) -> bool {
    let trimmed = path.strip_prefix("./").unwrap_or(path);
    if trimmed.starts_with('/') || trimmed.starts_with('\\') {
        return true;
    }
    for seg in trimmed.split(&['/', '\\'][..]) {
        if seg == ".." {
            return true;
        }
    }
    false
}

fn decompress_if_gzip(data: &[u8]) -> Result<Vec<u8>, String> {
    if data.len() >= 2 && data[0] == 0x1f && data[1] == 0x8b {
        let mut child = Command::new("gzip")
            .arg("-dc")
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .spawn()
            .map_err(|e| format!("failed to spawn gzip: {e}"))?;

        if let Some(mut stdin) = child.stdin.take() {
            let data_vec = data.to_vec();
            std::thread::spawn(move || {
                let _ = stdin.write_all(&data_vec);
            });
        }

        let output = child
            .wait_with_output()
            .map_err(|e| format!("gzip wait failed: {e}"))?;

        if !output.status.success() {
            let err = String::from_utf8_lossy(&output.stderr);
            return Err(format!("gzip decompression error: {err}"));
        }
        Ok(output.stdout)
    } else {
        Ok(data.to_vec())
    }
}

fn parse_tar(data: &[u8]) -> Result<Vec<TarEntry>, String> {
    let mut entries = Vec::new();
    let mut offset = 0;
    let mut next_long_name: Option<String> = None;

    while offset + 512 <= data.len() {
        let block = &data[offset..offset + 512];
        if block.iter().all(|&b| b == 0) {
            // End of tar archive
            break;
        }

        let raw_name = parse_tar_string(&block[0..100]);
        let mode = parse_octal(&block[100..108]) as u32;
        let size = parse_octal(&block[124..136]) as usize;
        let typeflag = block[156];
        let linkname = parse_tar_string(&block[157..257]);
        let prefix = parse_tar_string(&block[345..500]);

        offset += 512;
        let padded_size = size.div_ceil(512) * 512;
        if offset + size > data.len() {
            return Err("corrupt tar archive: unexpected end of data".to_string());
        }

        let entry_data = if size > 0 {
            data[offset..offset + size].to_vec()
        } else {
            Vec::new()
        };
        let advance = if offset + padded_size <= data.len() {
            padded_size
        } else {
            size
        };
        offset += advance;

        if typeflag == b'L' {
            let name = parse_tar_string(&entry_data);
            next_long_name = Some(name);
            continue;
        }

        if typeflag == b'x' || typeflag == b'g' {
            continue;
        }

        let full_name = if let Some(long_name) = next_long_name.take() {
            long_name
        } else if !prefix.is_empty() {
            format!("{prefix}/{raw_name}")
        } else {
            raw_name
        };

        if full_name.is_empty() {
            continue;
        }

        entries.push(TarEntry {
            path: full_name,
            mode: if mode == 0 { 0o644 } else { mode },
            typeflag,
            linkname,
            data: entry_data,
        });
    }

    Ok(entries)
}

fn apply_permissions_and_ownership(path: &Path, mode: u32, uid: u32, gid: u32) {
    if mode != 0 {
        let perms = std::fs::Permissions::from_mode(mode & 0o777);
        let _ = std::fs::set_permissions(path, perms);
    }
    if unsafe { libc::geteuid() == 0 } {
        if let Ok(c_path) = std::ffi::CString::new(path.to_string_lossy().as_bytes()) {
            unsafe {
                libc::chown(c_path.as_ptr(), uid, gid);
            }
        }
    }
}

fn handle_archive_sync(
    stream: &mut TcpStream,
    req: &HttpRequest,
    query_params: &HashMap<String, String>,
    config: &DaemonConfig,
) {
    if req.body.is_empty() {
        write_json(
            stream,
            "400 Bad Request",
            r#"{"error":"empty_archive_body"}"#,
        );
        return;
    }

    let dest_str = query_params
        .get("dest")
        .or_else(|| query_params.get("destination"))
        .map(|s| s.as_str())
        .unwrap_or(&config.workdir);

    let uid = query_params
        .get("uid")
        .and_then(|u| u.parse::<u32>().ok())
        .unwrap_or(1000);

    let gid = query_params
        .get("gid")
        .and_then(|g| g.parse::<u32>().ok())
        .unwrap_or(1000);

    let remove_paths = query_params
        .get("remove_paths")
        .or_else(|| query_params.get("removePaths"));

    let tar_data = match decompress_if_gzip(&req.body) {
        Ok(d) => d,
        Err(e) => {
            let resp = format!(
                r#"{{"error":"decompression_failed","details":"{}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &resp);
            return;
        }
    };

    let entries = match parse_tar(&tar_data) {
        Ok(e) => e,
        Err(e) => {
            let resp = format!(
                r#"{{"error":"invalid_tar_archive","details":"{}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &resp);
            return;
        }
    };

    // Reject any entry with ".." or leading "/" BEFORE extraction
    for entry in &entries {
        if is_invalid_tar_path(&entry.path)
            || ((entry.typeflag == b'1' || entry.typeflag == b'2')
                && is_invalid_tar_path(&entry.linkname))
        {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"invalid_archive_path"}"#,
            );
            return;
        }
    }

    let dest_path = Path::new(dest_str);
    if let Err(e) = std::fs::create_dir_all(dest_path) {
        let resp = format!(
            r#"{{"error":"failed_to_create_dest","details":"{}"}}"#,
            escape_json(&e.to_string())
        );
        write_json(stream, "500 Internal Server Error", &resp);
        return;
    }

    // Remove uninstalled paths under dest if specified
    if let Some(remove_list) = remove_paths {
        for p in remove_list.split(',') {
            let p = p.trim();
            if p.is_empty() || is_invalid_tar_path(p) {
                continue;
            }
            let rel_p = p.strip_prefix("./").unwrap_or(p).trim_start_matches('/');
            let target = dest_path.join(rel_p);
            if target.exists() || target.is_symlink() {
                if target.is_dir() {
                    let _ = std::fs::remove_dir_all(&target);
                } else {
                    let _ = std::fs::remove_file(&target);
                }
            }
        }
    }

    let mut extracted_count = 0;
    for entry in entries {
        let rel_path = entry
            .path
            .strip_prefix("./")
            .unwrap_or(&entry.path)
            .trim_start_matches('/');
        if rel_path.is_empty() || rel_path == "." {
            continue;
        }

        let target = dest_path.join(rel_path);

        if entry.typeflag == b'5' || rel_path.ends_with('/') {
            if let Err(e) = std::fs::create_dir_all(&target) {
                let resp = format!(
                    r#"{{"error":"failed_to_create_dir","details":"{}"}}"#,
                    escape_json(&e.to_string())
                );
                write_json(stream, "500 Internal Server Error", &resp);
                return;
            }
            apply_permissions_and_ownership(&target, entry.mode, uid, gid);
        } else if entry.typeflag == b'2' {
            if let Some(parent) = target.parent() {
                let _ = std::fs::create_dir_all(parent);
            }
            let _ = std::fs::remove_file(&target);
            if let Ok(c_link) = std::ffi::CString::new(entry.linkname.as_bytes()) {
                if let Ok(c_target) = std::ffi::CString::new(target.to_string_lossy().as_bytes()) {
                    unsafe {
                        libc::symlink(c_link.as_ptr(), c_target.as_ptr());
                    }
                }
            }
            extracted_count += 1;
        } else {
            if let Some(parent) = target.parent() {
                if let Err(e) = std::fs::create_dir_all(parent) {
                    let resp = format!(
                        r#"{{"error":"failed_to_create_parent_dir","details":"{}"}}"#,
                        escape_json(&e.to_string())
                    );
                    write_json(stream, "500 Internal Server Error", &resp);
                    return;
                }
            }
            if let Err(e) = std::fs::write(&target, &entry.data) {
                let resp = format!(
                    r#"{{"error":"failed_to_write_file","details":"{}"}}"#,
                    escape_json(&e.to_string())
                );
                write_json(stream, "500 Internal Server Error", &resp);
                return;
            }
            apply_permissions_and_ownership(&target, entry.mode, uid, gid);
            extracted_count += 1;
        }
    }

    let resp = format!(
        r#"{{"status":"synced","dest":"{}","extracted":{}}}"#,
        escape_json(dest_str),
        extracted_count
    );
    write_json(stream, "200 OK", &resp);
}

pub fn handle_client(mut stream: TcpStream, config: &DaemonConfig, registry: &ProcessRegistry) {
    let _ = stream.set_read_timeout(Some(std::time::Duration::from_secs(120)));
    let _ = stream.set_write_timeout(Some(std::time::Duration::from_secs(120)));

    let req = match read_http_request(&mut stream) {
        Ok(r) => r,
        Err(_) => return,
    };

    let (path, query_params) = parse_target(&req.target);

    match (req.method.as_str(), path) {
        ("GET", "/health") => {
            handle_health(&mut stream, config, registry);
        }
        ("POST", "/v1/lifecycle/prepare-stop") => {
            handle_prepare_stop(&mut stream, registry);
        }
        ("POST", "/v1/commands/run") => {
            handle_command_run(&mut stream, &req, config, registry);
        }
        ("PUT", "/v1/files") => {
            handle_file_put(&mut stream, &req, &query_params);
        }
        ("GET", "/v1/files") => {
            handle_file_get(&mut stream, &query_params);
        }
        ("DELETE", "/v1/files") => {
            handle_file_delete(&mut stream, &query_params);
        }
        ("POST", "/v1/files/archive-sync") => {
            handle_archive_sync(&mut stream, &req, &query_params, config);
        }
        ("GET", "/v1/files/list") => {
            handle_file_list(&mut stream, &query_params, config);
        }
        _ => {
            if req.method == "GET" && path.starts_with("/v1/commands/") && path.ends_with("/status")
            {
                let id = &path["/v1/commands/".len()..path.len() - "/status".len()];
                handle_command_status(&mut stream, id, registry);
            } else if req.method == "GET"
                && path.starts_with("/v1/commands/")
                && path.ends_with("/logs")
            {
                let id = &path["/v1/commands/".len()..path.len() - "/logs".len()];
                let cursor = query_params
                    .get("cursor")
                    .and_then(|c| c.parse::<usize>().ok())
                    .unwrap_or(0);
                handle_command_logs(&mut stream, id, cursor, registry);
            } else if req.method == "POST"
                && path.starts_with("/v1/commands/")
                && path.ends_with("/kill")
            {
                let id = &path["/v1/commands/".len()..path.len() - "/kill".len()];
                handle_command_kill(&mut stream, id, registry);
            } else {
                write_json(&mut stream, "404 Not Found", r#"{"error":"not_found"}"#);
            }
        }
    }
}

pub fn serve(listener: TcpListener, config: DaemonConfig, registry: ProcessRegistry) {
    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let config = config.clone();
                let registry = registry.clone();
                std::thread::spawn(move || {
                    handle_client(stream, &config, &registry);
                });
            }
            Err(e) => {
                eprintln!("[podd] accept error: {e}");
            }
        }
    }
}

fn main() {
    let addr = env::var("CUBEPOD_ADDR").unwrap_or_else(|_| "0.0.0.0:49983".into());
    let workdir = env::var("CUBEPOD_WORKDIR").unwrap_or_else(|_| "/workspace".into());

    eprintln!(
        "[podd] starting CubePod Rust In-Guest Daemon (pid={}, addr={}, workdir={})",
        std::process::id(),
        addr,
        workdir
    );

    let config = DaemonConfig { workdir };
    let registry = ProcessRegistry::new();
    let listener = TcpListener::bind(&addr).expect("failed to bind podd listener");
    serve(listener, config, registry);
}

// ============================================================================
// Unit & Integration Tests
// ============================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    fn http_request(
        addr: &str,
        method: &str,
        target: &str,
        headers: &[(&str, &str)],
        body: &[u8],
    ) -> (u16, HashMap<String, String>, Vec<u8>) {
        let mut stream = TcpStream::connect(addr).expect("failed to connect to test server");
        let mut req =
            format!("{method} {target} HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n");
        let mut has_content_length = false;
        for (k, v) in headers {
            if k.eq_ignore_ascii_case("content-length") {
                has_content_length = true;
            }
            req.push_str(&format!("{k}: {v}\r\n"));
        }
        if !has_content_length && !body.is_empty() {
            req.push_str(&format!("Content-Length: {}\r\n", body.len()));
        }
        req.push_str("\r\n");
        stream.write_all(req.as_bytes()).unwrap();
        if !body.is_empty() {
            stream.write_all(body).unwrap();
        }
        stream.flush().unwrap();

        let mut response_bytes = Vec::new();
        stream.read_to_end(&mut response_bytes).unwrap();

        let (header_bytes, body_bytes) =
            if let Some(pos) = find_subsequence(&response_bytes, b"\r\n\r\n") {
                (&response_bytes[..pos], &response_bytes[pos + 4..])
            } else {
                panic!("invalid http response: no header delimiter");
            };

        let header_str = String::from_utf8_lossy(header_bytes);
        let mut lines = header_str.lines();
        let status_line = lines.next().expect("empty status line");
        let status_code: u16 = status_line
            .split_whitespace()
            .nth(1)
            .expect("missing status code")
            .parse()
            .expect("invalid status code");

        let mut res_headers = HashMap::new();
        for line in lines {
            if let Some((k, v)) = line.split_once(':') {
                res_headers.insert(k.trim().to_ascii_lowercase(), v.trim().to_string());
            }
        }

        (status_code, res_headers, body_bytes.to_vec())
    }

    fn start_test_server(workdir: &str) -> (String, ProcessRegistry) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let registry = ProcessRegistry::new();
        let config = DaemonConfig {
            workdir: workdir.to_string(),
        };
        let reg_clone = registry.clone();
        std::thread::spawn(move || {
            serve(listener, config, reg_clone);
        });
        (addr.to_string(), registry)
    }

    #[test]
    fn test_json_parser_and_escaping() {
        let raw = r#"{
            "string": "hello \"world\"\n\t\\",
            "bool_t": true,
            "bool_f": false,
            "null_val": null,
            "num_int": 42,
            "num_float": 3.14,
            "nested": {"key": "val"},
            "list": [1, 2, "three"]
        }"#;

        let json = JsonValue::parse(raw).expect("json parse failed");
        assert_eq!(
            json.get("string").unwrap().as_str().unwrap(),
            "hello \"world\"\n\t\\"
        );
        assert_eq!(json.get("bool_t").unwrap().as_bool(), Some(true));
        assert_eq!(json.get("bool_f").unwrap().as_bool(), Some(false));
        assert_eq!(json.get("null_val"), Some(&JsonValue::Null));
        assert_eq!(json.get("num_int").unwrap().as_u64(), Some(42));
        assert_eq!(
            json.get("nested")
                .unwrap()
                .get("key")
                .unwrap()
                .as_str()
                .unwrap(),
            "val"
        );

        let escaped = escape_json("line1\nline2\t\"quoted\"");
        assert_eq!(escaped, "line1\\nline2\\t\\\"quoted\\\"");
    }

    #[test]
    fn test_health_endpoint() {
        let (addr, _) = start_test_server("/tmp");
        let (status, _, body) = http_request(&addr, "GET", "/health", &[], &[]);
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        assert_eq!(json.get("status").unwrap().as_str().unwrap(), "ok");
        assert_eq!(json.get("daemon").unwrap().as_str().unwrap(), "podd-rs");
        assert_eq!(json.get("workdir").unwrap().as_str().unwrap(), "/tmp");
        assert_eq!(json.get("active_processes").unwrap().as_u64(), Some(0));
    }

    #[test]
    fn test_prepare_stop_endpoint() {
        let (addr, _) = start_test_server("/tmp");
        let (status, _, body) = http_request(&addr, "POST", "/v1/lifecycle/prepare-stop", &[], &[]);
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        assert_eq!(json.get("status").unwrap().as_str().unwrap(), "synced");
        assert_eq!(json.get("active_processes").unwrap().as_u64(), Some(0));
    }

    #[test]
    fn test_command_foreground_execution() {
        let (addr, _) = start_test_server("/tmp");
        let req_body = r#"{"command": "echo 'hello foreground'", "background": false}"#;
        let (status, _, body) = http_request(
            &addr,
            "POST",
            "/v1/commands/run",
            &[("Content-Type", "application/json")],
            req_body.as_bytes(),
        );
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        assert_eq!(json.get("running").unwrap().as_bool(), Some(false));
        assert_eq!(json.get("exit_code").unwrap().as_u64(), Some(0));
        assert_eq!(
            json.get("output").unwrap().as_str().unwrap(),
            "hello foreground\n"
        );
        assert_eq!(json.get("cursor").unwrap().as_u64(), Some(17));
        let id = json.get("id").unwrap().as_str().unwrap();

        // Verify status endpoint for foreground command
        let (status_code, _, status_body) =
            http_request(&addr, "GET", &format!("/v1/commands/{id}/status"), &[], &[]);
        assert_eq!(status_code, 200);
        let status_json = JsonValue::parse(std::str::from_utf8(&status_body).unwrap()).unwrap();
        assert_eq!(status_json.get("running").unwrap().as_bool(), Some(false));
        assert_eq!(status_json.get("exit_code").unwrap().as_u64(), Some(0));
    }

    #[test]
    fn test_command_foreground_envs_and_workdir() {
        let dir = std::env::temp_dir().join(format!("podd-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();

        let (addr, _) = start_test_server("/tmp");
        let req_body = format!(
            r#"{{"command": "pwd && echo $TEST_VAL", "workdir": "{}", "envs": {{"TEST_VAL": "foobar123"}}, "background": false}}"#,
            escape_json(&dir.to_string_lossy())
        );

        let (status, _, body) = http_request(
            &addr,
            "POST",
            "/v1/commands/run",
            &[("Content-Type", "application/json")],
            req_body.as_bytes(),
        );
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        let output = json.get("output").unwrap().as_str().unwrap();
        assert!(
            output.contains("foobar123"),
            "expected foobar123 in output: {output}"
        );
        assert!(
            output.contains(&dir.file_name().unwrap().to_string_lossy().to_string()),
            "expected dir in output: {output}"
        );

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn test_command_foreground_exit_code() {
        let (addr, _) = start_test_server("/tmp");
        let req_body = r#"{"command": "exit 42", "background": false}"#;
        let (status, _, body) = http_request(
            &addr,
            "POST",
            "/v1/commands/run",
            &[("Content-Type", "application/json")],
            req_body.as_bytes(),
        );
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        assert_eq!(json.get("exit_code").unwrap().as_u64(), Some(42));
        assert_eq!(json.get("running").unwrap().as_bool(), Some(false));
    }

    #[test]
    fn test_command_background_and_cursor_logs() {
        let (addr, _) = start_test_server("/tmp");
        let req_body = r#"{"command": "echo line1; sleep 0.2; echo line2", "background": true}"#;
        let (status, _, body) = http_request(
            &addr,
            "POST",
            "/v1/commands/run",
            &[("Content-Type", "application/json")],
            req_body.as_bytes(),
        );
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        assert_eq!(json.get("running").unwrap().as_bool(), Some(true));
        assert_eq!(json.get("exit_code"), Some(&JsonValue::Null));
        let id = json.get("id").unwrap().as_str().unwrap();

        // Poll logs until we get line1
        let mut cursor = 0;
        let mut accumulated = String::new();
        for _ in 0..20 {
            let (status_code, _, log_body) = http_request(
                &addr,
                "GET",
                &format!("/v1/commands/{id}/logs?cursor={cursor}"),
                &[],
                &[],
            );
            assert_eq!(status_code, 200);
            let log_json = JsonValue::parse(std::str::from_utf8(&log_body).unwrap()).unwrap();
            let content = log_json.get("content").unwrap().as_str().unwrap();
            accumulated.push_str(content);
            cursor = log_json.get("cursor").unwrap().as_u64().unwrap();
            if accumulated.contains("line2") {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }

        assert!(
            accumulated.contains("line1\n"),
            "expected line1 in {accumulated}"
        );
        assert!(
            accumulated.contains("line2\n"),
            "expected line2 in {accumulated}"
        );

        // Verify status endpoint after completion
        let mut finished = false;
        for _ in 0..20 {
            let (status_code, _, status_body) =
                http_request(&addr, "GET", &format!("/v1/commands/{id}/status"), &[], &[]);
            assert_eq!(status_code, 200);
            let status_json = JsonValue::parse(std::str::from_utf8(&status_body).unwrap()).unwrap();
            if status_json.get("running").unwrap().as_bool() == Some(false) {
                assert_eq!(status_json.get("exit_code").unwrap().as_u64(), Some(0));
                finished = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(finished, "process did not finish running");
    }

    #[test]
    fn test_command_background_kill() {
        let (addr, _) = start_test_server("/tmp");
        let req_body = r#"{"command": "sleep 10", "background": true}"#;
        let (status, _, body) = http_request(
            &addr,
            "POST",
            "/v1/commands/run",
            &[("Content-Type", "application/json")],
            req_body.as_bytes(),
        );
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
        assert_eq!(json.get("running").unwrap().as_bool(), Some(true));
        let id = json.get("id").unwrap().as_str().unwrap();

        // Kill the process group
        let (kill_status, _, kill_body) =
            http_request(&addr, "POST", &format!("/v1/commands/{id}/kill"), &[], &[]);
        assert_eq!(kill_status, 200);
        let kill_json = JsonValue::parse(std::str::from_utf8(&kill_body).unwrap()).unwrap();
        assert_eq!(kill_json.get("status").unwrap().as_str().unwrap(), "killed");

        // Verify it stops running
        let mut stopped = false;
        for _ in 0..20 {
            let (status_code, _, status_body) =
                http_request(&addr, "GET", &format!("/v1/commands/{id}/status"), &[], &[]);
            assert_eq!(status_code, 200);
            let status_json = JsonValue::parse(std::str::from_utf8(&status_body).unwrap()).unwrap();
            if status_json.get("running").unwrap().as_bool() == Some(false) {
                stopped = true;
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(stopped, "killed process did not transition to stopped");
    }

    #[test]
    fn test_file_endpoints() {
        let dir = std::env::temp_dir().join(format!("podd-files-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();

        let (addr, _) = start_test_server(&dir.to_string_lossy());

        let target_file = dir.join("test.txt");
        let file_path_str = target_file.to_string_lossy().to_string();

        // PUT file
        let put_body = b"hello cubepod file content";
        let (put_status, _, put_resp) = http_request(
            &addr,
            "PUT",
            &format!("/v1/files?path={}&mode=0644", file_path_str),
            &[("Content-Type", "application/octet-stream")],
            put_body,
        );
        assert_eq!(put_status, 200);
        let put_json = JsonValue::parse(std::str::from_utf8(&put_resp).unwrap()).unwrap();
        assert_eq!(
            put_json.get("path").unwrap().as_str().unwrap(),
            file_path_str
        );
        assert_eq!(
            put_json.get("size").unwrap().as_u64(),
            Some(put_body.len() as u64)
        );

        // GET file
        let (get_status, _, get_body) = http_request(
            &addr,
            "GET",
            &format!("/v1/files?path={}", file_path_str),
            &[],
            &[],
        );
        assert_eq!(get_status, 200);
        assert_eq!(get_body, put_body);

        // GET file list
        let (list_status, _, list_body) = http_request(
            &addr,
            "GET",
            &format!("/v1/files/list?path={}", dir.to_string_lossy()),
            &[],
            &[],
        );
        assert_eq!(list_status, 200);
        let list_json = JsonValue::parse(std::str::from_utf8(&list_body).unwrap()).unwrap();
        let entries = match list_json.get("entries").unwrap() {
            JsonValue::Array(a) => a,
            _ => panic!("expected array for entries"),
        };
        assert_eq!(entries.len(), 1);
        assert_eq!(
            entries[0].get("name").unwrap().as_str().unwrap(),
            "test.txt"
        );
        assert_eq!(entries[0].get("is_dir").unwrap().as_bool(), Some(false));
        assert_eq!(
            entries[0].get("size").unwrap().as_u64(),
            Some(put_body.len() as u64)
        );

        // GET nonexistent file
        let (nf_status, _, _) = http_request(
            &addr,
            "GET",
            &format!("/v1/files?path={}/nonexistent.txt", dir.to_string_lossy()),
            &[],
            &[],
        );
        assert_eq!(nf_status, 404);

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn test_404_routing() {
        let (addr, _) = start_test_server("/tmp");
        let (status, _, _) = http_request(&addr, "GET", "/invalid/route", &[], &[]);
        assert_eq!(status, 404);

        let (status_cmd, _, _) =
            http_request(&addr, "GET", "/v1/commands/unknown-proc/status", &[], &[]);
        assert_eq!(status_cmd, 404);

        let (status_kill, _, _) =
            http_request(&addr, "POST", "/v1/commands/unknown-proc/kill", &[], &[]);
        assert_eq!(status_kill, 404);
    }

    fn craft_tar_entry(name: &str, content: &[u8]) -> Vec<u8> {
        let mut header = [0u8; 512];
        let name_bytes = name.as_bytes();
        let len = name_bytes.len().min(100);
        header[..len].copy_from_slice(&name_bytes[..len]);
        header[100..108].copy_from_slice(b"0000644\0");
        header[108..116].copy_from_slice(b"0001750\0");
        header[116..124].copy_from_slice(b"0001750\0");
        let size_str = format!("{:011o}\0", content.len());
        header[124..136].copy_from_slice(size_str.as_bytes());
        header[136..148].copy_from_slice(b"00000000000\0");
        header[156] = b'0';
        header[257..263].copy_from_slice(b"ustar\0");
        header[263..265].copy_from_slice(b"00");

        header[148..156].copy_from_slice(b"        ");
        let sum: u32 = header.iter().map(|&b| b as u32).sum();
        let chksum_str = format!("{:06o}\0 ", sum);
        header[148..156].copy_from_slice(chksum_str.as_bytes());

        let mut out = Vec::new();
        out.extend_from_slice(&header);
        out.extend_from_slice(content);
        let pad = (512 - (content.len() % 512)) % 512;
        out.extend(std::iter::repeat_n(0u8, pad));
        out.extend(std::iter::repeat_n(0u8, 1024));
        out
    }

    #[test]
    fn test_archive_sync_tar_gz_and_remove_paths() {
        let test_root = std::env::temp_dir().join(format!("podd-archive-{}", std::process::id()));
        let dest_dir = test_root.join("workspace");
        let src_dir = test_root.join("src_files");
        std::fs::create_dir_all(&dest_dir).unwrap();
        std::fs::create_dir_all(src_dir.join("src")).unwrap();

        // 1. Create a stale file in dest that should be removed
        let stale_file = dest_dir.join("stale.txt");
        std::fs::write(&stale_file, b"old content").unwrap();
        assert!(stale_file.exists());

        // 2. Prepare files to package into .tar.gz
        let main_py = src_dir.join("src/main.py");
        let readme = src_dir.join("README.md");
        std::fs::write(&main_py, b"print('hello world')\n").unwrap();
        std::fs::write(&readme, b"# Sample Archive Sync Project\n").unwrap();

        // 3. Create .tar.gz archive using system tar
        let tar_gz_path = test_root.join("archive.tar.gz");
        let tar_status = Command::new("tar")
            .args([
                "-czf",
                tar_gz_path.to_str().unwrap(),
                "-C",
                src_dir.to_str().unwrap(),
                "src/main.py",
                "README.md",
            ])
            .status()
            .expect("failed to execute tar command");
        assert!(tar_status.success());
        let tar_gz_bytes = std::fs::read(&tar_gz_path).unwrap();

        // 4. Start test server and send POST /v1/files/archive-sync
        let (addr, _) = start_test_server("/tmp");
        let target_url = format!(
            "/v1/files/archive-sync?dest={}&uid=1000&gid=1000&remove_paths=stale.txt",
            dest_dir.to_str().unwrap()
        );
        let (status, _, resp_body) = http_request(
            &addr,
            "POST",
            &target_url,
            &[("Content-Type", "application/gzip")],
            &tar_gz_bytes,
        );
        assert_eq!(status, 200);

        let json = JsonValue::parse(std::str::from_utf8(&resp_body).unwrap()).unwrap();
        assert_eq!(json.get("status").unwrap().as_str().unwrap(), "synced");
        assert_eq!(
            json.get("dest").unwrap().as_str().unwrap(),
            dest_dir.to_str().unwrap()
        );
        assert_eq!(json.get("extracted").unwrap().as_u64(), Some(2));

        // 5. Verify disk contents: stale.txt deleted, src/main.py and README.md extracted
        assert!(!stale_file.exists(), "stale.txt should have been removed");
        assert_eq!(
            std::fs::read(dest_dir.join("src/main.py")).unwrap(),
            b"print('hello world')\n"
        );
        assert_eq!(
            std::fs::read(dest_dir.join("README.md")).unwrap(),
            b"# Sample Archive Sync Project\n"
        );

        let _ = std::fs::remove_dir_all(&test_root);
    }

    #[test]
    fn test_archive_sync_rejects_path_traversal() {
        let test_root = std::env::temp_dir().join(format!("podd-traversal-{}", std::process::id()));
        let dest_dir = test_root.join("workspace");
        std::fs::create_dir_all(&dest_dir).unwrap();

        let malicious_tar = craft_tar_entry("../escape.txt", b"malicious content");

        let (addr, _) = start_test_server("/tmp");
        let target_url = format!(
            "/v1/files/archive-sync?dest={}&uid=1000&gid=1000",
            dest_dir.to_str().unwrap()
        );

        let (status, _, resp_body) = http_request(
            &addr,
            "POST",
            &target_url,
            &[("Content-Type", "application/x-tar")],
            &malicious_tar,
        );
        assert_eq!(status, 400);

        let json = JsonValue::parse(std::str::from_utf8(&resp_body).unwrap()).unwrap();
        assert_eq!(
            json.get("error").unwrap().as_str().unwrap(),
            "invalid_archive_path"
        );

        // Verify that ../escape.txt was NEVER created on disk
        let escaped_path = test_root.join("escape.txt");
        assert!(
            !escaped_path.exists(),
            "malicious file must not be extracted"
        );

        // Also test leading slash
        let leading_slash_tar = craft_tar_entry("/escape2.txt", b"malicious content 2");
        let (status2, _, _) = http_request(
            &addr,
            "POST",
            &target_url,
            &[("Content-Type", "application/x-tar")],
            &leading_slash_tar,
        );
        assert_eq!(status2, 400);

        let _ = std::fs::remove_dir_all(&test_root);
    }

    #[test]
    fn test_file_delete_endpoint() {
        let dir = std::env::temp_dir().join(format!("podd-del-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();

        let (addr, _) = start_test_server(&dir.to_string_lossy());

        // 1. Create file and delete it
        let file_path = dir.join("to_delete.txt");
        std::fs::write(&file_path, b"delete me").unwrap();
        assert!(file_path.exists());

        let file_path_str = file_path.to_string_lossy().to_string();
        let (del_status, _, del_resp) = http_request(
            &addr,
            "DELETE",
            &format!("/v1/files?path={}", file_path_str),
            &[],
            &[],
        );
        assert_eq!(del_status, 200);
        let del_json = JsonValue::parse(std::str::from_utf8(&del_resp).unwrap()).unwrap();
        assert_eq!(
            del_json.get("deleted").unwrap().as_str().unwrap(),
            file_path_str
        );
        assert!(!file_path.exists(), "file should be deleted");

        // 2. Delete non-existent file -> 404
        let (nf_status, _, _) = http_request(
            &addr,
            "DELETE",
            &format!("/v1/files?path={}", file_path_str),
            &[],
            &[],
        );
        assert_eq!(nf_status, 404);

        // 3. Delete directory tree
        let subdir = dir.join("subdir");
        std::fs::create_dir_all(&subdir).unwrap();
        std::fs::write(subdir.join("nested.txt"), b"nested").unwrap();
        assert!(subdir.exists());

        let subdir_str = subdir.to_string_lossy().to_string();
        let (dir_status, _, dir_resp) = http_request(
            &addr,
            "DELETE",
            &format!("/v1/files?path={}", subdir_str),
            &[],
            &[],
        );
        assert_eq!(dir_status, 200);
        let dir_json = JsonValue::parse(std::str::from_utf8(&dir_resp).unwrap()).unwrap();
        assert_eq!(
            dir_json.get("deleted").unwrap().as_str().unwrap(),
            subdir_str
        );
        assert!(!subdir.exists(), "directory should be deleted");

        // 4. Missing path param -> 400
        let (bad_status, _, _) = http_request(&addr, "DELETE", "/v1/files", &[], &[]);
        assert_eq!(bad_status, 400);

        let _ = std::fs::remove_dir_all(&dir);
    }
}
