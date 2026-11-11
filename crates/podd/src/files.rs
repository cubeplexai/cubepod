//! File operations, Tar extraction, Archive-sync & prepare-stop for podd.

use std::collections::HashMap;
use std::io::Write;
use std::net::TcpStream;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::Command;

use crate::commands::ProcessRegistry;
use crate::http::{write_json, write_response, HttpRequest};
use crate::json::escape_json;
use crate::DaemonConfig;

pub fn handle_prepare_stop(stream: &mut TcpStream, registry: &ProcessRegistry) {
    unsafe {
        libc::sync();
    }
    let active = registry.active_count();
    let body = format!(r#"{{"status":"synced","active_processes":{active}}}"#);
    write_json(stream, "200 OK", &body);
}

pub fn handle_file_put(
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
        let clean = mode_str.trim_start_matches("0o").trim_start_matches('0');
        let mode_val = u32::from_str_radix(clean, 8)
            .or_else(|_| mode_str.parse())
            .unwrap_or(0o644);
        let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode_val));
    }

    let uid = query_params
        .get("uid")
        .and_then(|u| u.parse().ok())
        .unwrap_or(1000);
    let gid = query_params
        .get("gid")
        .and_then(|g| g.parse().ok())
        .unwrap_or(1000);
    chown_if_root(path, uid, gid);

    let resp = format!(
        r#"{{"path":"{}","size":{}}}"#,
        escape_json(raw_path),
        req.body.len()
    );
    write_json(stream, "200 OK", &resp);
}

pub fn handle_file_get(stream: &mut TcpStream, query_params: &HashMap<String, String>) {
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

pub fn handle_file_list(
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

pub fn handle_file_delete(stream: &mut TcpStream, query_params: &HashMap<String, String>) {
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

pub struct TarEntry {
    pub path: String,
    pub mode: u32,
    pub typeflag: u8,
    pub linkname: String,
    pub data: Vec<u8>,
}

pub fn parse_octal(bytes: &[u8]) -> u64 {
    let s = String::from_utf8_lossy(bytes);
    let trimmed = s.trim_matches(|c: char| c.is_whitespace() || c == '\0');
    if trimmed.is_empty() {
        return 0;
    }
    u64::from_str_radix(trimmed, 8).unwrap_or(0)
}

pub fn parse_tar_string(bytes: &[u8]) -> String {
    let end = bytes.iter().position(|&b| b == 0).unwrap_or(bytes.len());
    String::from_utf8_lossy(&bytes[..end]).to_string()
}

pub fn is_invalid_tar_path(path: &str) -> bool {
    let trimmed = path.strip_prefix("./").unwrap_or(path);
    if trimmed.starts_with('/') || trimmed.starts_with('\\') {
        return true;
    }
    trimmed.split(&['/', '\\'][..]).any(|seg| seg == "..")
}

pub fn decompress_if_gzip(data: &[u8]) -> Result<Vec<u8>, String> {
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

pub fn parse_tar(data: &[u8]) -> Result<Vec<TarEntry>, String> {
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

pub fn chown_if_root(path: &Path, uid: u32, gid: u32) {
    if unsafe { libc::geteuid() == 0 } {
        if let Ok(c_path) = std::ffi::CString::new(path.to_string_lossy().as_bytes()) {
            unsafe {
                libc::chown(c_path.as_ptr(), uid, gid);
            }
        }
    }
}

pub fn apply_permissions_and_ownership(path: &Path, mode: u32, uid: u32, gid: u32) {
    if mode != 0 {
        let perms = std::fs::Permissions::from_mode(mode & 0o777);
        let _ = std::fs::set_permissions(path, perms);
    }
    chown_if_root(path, uid, gid);
}

pub fn handle_archive_sync(
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
        .or_else(|| query_params.get("path"))
        .map_or(config.workdir.as_str(), |s| s.as_str());

    let uid = query_params
        .get("uid")
        .and_then(|u| u.parse().ok())
        .unwrap_or(1000);
    let gid = query_params
        .get("gid")
        .and_then(|g| g.parse().ok())
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
