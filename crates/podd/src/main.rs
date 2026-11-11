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

pub fn route_request(mut stream: TcpStream, config: &DaemonConfig, registry: &ProcessRegistry) {
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

pub fn handle_client(stream: TcpStream, config: &DaemonConfig, registry: &ProcessRegistry) {
    route_request(stream, config, registry);
}

pub fn run_server(listener: TcpListener, config: DaemonConfig, registry: ProcessRegistry) {
    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let config = config.clone();
                let registry = registry.clone();
                std::thread::spawn(move || {
                    route_request(stream, &config, &registry);
                });
            }
            Err(e) => {
                eprintln!("[podd] accept error: {e}");
            }
        }
    }
}

pub fn serve(listener: TcpListener, config: DaemonConfig, registry: ProcessRegistry) {
    run_server(listener, config, registry);
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
    run_server(listener, config, registry);
}
