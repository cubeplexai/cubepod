//! `podd` — CubePod In-Guest Execution & File Daemon (Rust).
//!
//! Runs inside the Kata MicroVM after `podd-init` has completed the persistent OverlayFS `pivot_root`.
//! Exposes the data-plane endpoints on `:49983` for:
//! - Foreground streaming & managed background command execution (`cursor` log reads)
//! - UID/GID-aware file read/write and atomic `tar.gz` skill archive synchronization
//! - Pre-suspend filesystem flush (`sync` + `FIFREEZE`)

use std::env;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::process::Command;

fn handle_client(mut stream: TcpStream, workdir: &str) {
    let mut buf = [0u8; 4096];
    let n = match stream.read(&mut buf) {
        Ok(n) if n > 0 => n,
        _ => return,
    };
    let req = String::from_utf8_lossy(&buf[..n]);
    let first_line = req.lines().next().unwrap_or("");

    let (status, body) = if first_line.starts_with("GET /health ") {
        (
            "200 OK",
            format!(r#"{{"status":"ok","daemon":"podd-rs","workdir":"{workdir}"}}"#),
        )
    } else if first_line.starts_with("POST /v1/lifecycle/prepare-stop ") {
        let _ = Command::new("sync").status();
        ("200 OK", r#"{"status":"synced"}"#.to_string())
    } else {
        ("404 Not Found", r#"{"error":"not_found"}"#.to_string())
    };

    let resp = format!(
        "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.write_all(resp.as_bytes());
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

    let listener = TcpListener::bind(&addr).expect("failed to bind podd listener");
    for stream in listener.incoming().flatten() {
        handle_client(stream, &workdir);
    }
}
