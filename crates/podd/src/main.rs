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

pub mod commands;
pub mod files;
pub mod http;
pub mod interpreter;
pub mod json;

#[cfg(test)]
mod tests;

use std::env;
use std::net::{TcpListener, TcpStream};

pub use commands::{
    handle_command_kill, handle_command_logs, handle_command_run, handle_command_status,
    kill_all_managed, ManagedProcess, ProcessEntry, ProcessOutputBuffer, ProcessRegistry,
    ProcessTable,
};
pub use files::{
    apply_permissions_and_ownership, chown_if_root, decompress_if_gzip, handle_archive_sync,
    handle_file_delete, handle_file_get, handle_file_list, handle_file_put, handle_prepare_stop,
    is_invalid_tar_path, parse_octal, parse_tar, parse_tar_string, TarEntry,
};
pub use http::{find_subsequence, read_http_request, write_json, write_response, HttpRequest};
pub use interpreter::{
    handle_context_create, handle_context_delete, handle_context_get, handle_context_list,
    handle_context_restart, handle_execute, ContextRegistry, ExecutionContext,
};
pub use json::{escape_json, parse_target, url_decode, JsonParser, JsonValue};

#[derive(Clone, Debug)]
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

pub fn handle_health(stream: &mut TcpStream, config: &DaemonConfig, registry: &ProcessRegistry) {
    let active = registry.active_count();
    let body = format!(
        r#"{{"status":"ok","daemon":"podd-rs","workdir":"{}","active_processes":{}}}"#,
        escape_json(&config.workdir),
        active
    );
    write_json(stream, "200 OK", &body);
}

pub fn route_request(
    mut stream: TcpStream,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
    contexts: &ContextRegistry,
) {
    let _ = stream.set_read_timeout(Some(std::time::Duration::from_secs(120)));
    let _ = stream.set_write_timeout(Some(std::time::Duration::from_secs(120)));

    let req = match read_http_request(&mut stream) {
        Ok(r) => r,
        Err(_) => return,
    };

    let (raw_path, query_params) = parse_target(&req.target);
    let path = raw_path.trim_end_matches('/');

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
        ("POST", "/execute") | ("POST", "/v1/code/execute") => {
            handle_execute(&mut stream, &req, config, contexts);
        }
        ("POST", "/contexts") | ("POST", "/v1/code/contexts") => {
            handle_context_create(&mut stream, &req, config, contexts);
        }
        ("GET", "/contexts") | ("GET", "/v1/code/contexts") => {
            handle_context_list(&mut stream, contexts);
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
            } else if req.method == "DELETE"
                && (path.starts_with("/contexts/") || path.starts_with("/v1/code/contexts/"))
            {
                let id = path
                    .strip_prefix("/v1/code/contexts/")
                    .or_else(|| path.strip_prefix("/contexts/"))
                    .unwrap_or("");
                handle_context_delete(&mut stream, id.trim_matches('/'), contexts);
            } else if req.method == "POST"
                && (path.starts_with("/contexts/") || path.starts_with("/v1/code/contexts/"))
                && path.ends_with("/restart")
            {
                let prefix_stripped = path
                    .strip_prefix("/v1/code/contexts/")
                    .or_else(|| path.strip_prefix("/contexts/"))
                    .unwrap_or("");
                let id = prefix_stripped
                    .strip_suffix("/restart")
                    .unwrap_or(prefix_stripped);
                handle_context_restart(&mut stream, id.trim_matches('/'), contexts);
            } else if req.method == "GET"
                && (path.starts_with("/contexts/") || path.starts_with("/v1/code/contexts/"))
            {
                let id = path
                    .strip_prefix("/v1/code/contexts/")
                    .or_else(|| path.strip_prefix("/contexts/"))
                    .unwrap_or("");
                handle_context_get(&mut stream, id.trim_matches('/'), contexts);
            } else {
                write_json(&mut stream, "404 Not Found", r#"{"error":"not_found"}"#);
            }
        }
    }
}

pub fn handle_client(stream: TcpStream, config: &DaemonConfig, registry: &ProcessRegistry) {
    let contexts = ContextRegistry::new();
    route_request(stream, config, registry, &contexts);
}

pub fn run_server(
    listener: TcpListener,
    config: DaemonConfig,
    registry: ProcessRegistry,
    contexts: ContextRegistry,
) {
    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let config = config.clone();
                let registry = registry.clone();
                let contexts = contexts.clone();
                std::thread::spawn(move || {
                    route_request(stream, &config, &registry, &contexts);
                });
            }
            Err(e) => {
                eprintln!("[podd] accept error: {e}");
            }
        }
    }
}

pub fn serve(listener: TcpListener, config: DaemonConfig, registry: ProcessRegistry) {
    run_server(listener, config, registry, ContextRegistry::new());
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
    let contexts = ContextRegistry::new();
    let listener = TcpListener::bind(&addr).expect("failed to bind podd listener");
    run_server(listener, config, registry, contexts);
}
