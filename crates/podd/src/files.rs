//! File operations, Tar extraction, Archive-sync & prepare-stop for podd.

use std::collections::HashMap;
use std::io::Write;
use std::net::TcpStream;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::Command;

use crate::commands::ProcessRegistry;
use crate::http::{find_subsequence, write_json, write_response, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::startcmd::{
    lookup_passwd_by_name, lookup_passwd_by_uid, resolve_default_credentials, spawn_tracked,
};
use crate::DaemonConfig;

pub fn resolve_file_ownership(query_params: &HashMap<String, String>) -> (u32, u32) {
    let req_uid = query_params.get("uid").and_then(|u| u.parse::<u32>().ok());
    let req_gid = query_params.get("gid").and_then(|g| g.parse::<u32>().ok());
    let user_name = query_params
        .get("username")
        .or_else(|| query_params.get("user"));
    match (req_uid, req_gid) {
        (Some(u), Some(g)) => (u, g),
        (Some(u), None) => {
            let g = lookup_passwd_by_uid(u)
                .ok()
                .flatten()
                .map(|p| p.gid)
                .unwrap_or(u);
            (u, g)
        }
        (None, req_gid_opt) => {
            if let Some(uname) = user_name {
                if let Ok(u) = uname.parse::<u32>() {
                    let g = req_gid_opt.unwrap_or_else(|| {
                        lookup_passwd_by_uid(u)
                            .ok()
                            .flatten()
                            .map(|p| p.gid)
                            .unwrap_or(u)
                    });
                    return (u, g);
                } else if let Ok(Some(entry)) = lookup_passwd_by_name(uname) {
                    return (entry.uid, req_gid_opt.unwrap_or(entry.gid));
                }
            }
            let (def_uid, def_gid) = resolve_default_credentials()
                .ok()
                .flatten()
                .unwrap_or((1000, 1000));
            (def_uid, req_gid_opt.unwrap_or(def_gid))
        }
    }
}

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

    let (uid, gid) = resolve_file_ownership(query_params);
    chown_if_root(path, uid, gid);

    let resp = format!(
        r#"{{"path":"{}","size":{}}}"#,
        escape_json(raw_path),
        req.body.len()
    );
    write_json(stream, "200 OK", &resp);
}

pub fn extract_multipart_boundary(content_type: &str) -> Option<String> {
    for part in content_type.split(';') {
        let trimmed = part.trim();
        if let Some(eq_idx) = trimmed.find('=') {
            let key = trimmed[..eq_idx].trim();
            if key.eq_ignore_ascii_case("boundary") {
                let val = trimmed[eq_idx + 1..].trim().trim_matches('"');
                if !val.is_empty() {
                    return Some(val.to_string());
                }
            }
        }
    }
    None
}

pub fn split_header_params(s: &str) -> Vec<(String, String)> {
    let mut result = Vec::new();
    let mut in_quotes = false;
    let mut cur_token = String::new();

    for ch in s.chars() {
        if ch == '"' {
            in_quotes = !in_quotes;
            cur_token.push(ch);
        } else if ch == ';' && !in_quotes {
            if let Some((k, v)) = cur_token.split_once('=') {
                let key = k.trim().to_string();
                let val = v.trim().trim_matches('"').to_string();
                result.push((key, val));
            }
            cur_token.clear();
        } else {
            cur_token.push(ch);
        }
    }

    if let Some((k, v)) = cur_token.split_once('=') {
        let key = k.trim().to_string();
        let val = v.trim().trim_matches('"').to_string();
        result.push((key, val));
    }

    result
}

pub fn parse_content_disposition(header_str: &str) -> (Option<String>, Option<String>) {
    let mut part_name = None;
    let mut part_filename = None;

    for line in header_str.lines() {
        let trimmed = line.trim();
        if let Some((k, v)) = trimmed.split_once(':') {
            if k.trim().eq_ignore_ascii_case("content-disposition") {
                let params = split_header_params(v);
                for (param_key, param_val) in params {
                    if param_key.eq_ignore_ascii_case("name") {
                        part_name = Some(param_val);
                    } else if param_key.eq_ignore_ascii_case("filename")
                        || param_key.eq_ignore_ascii_case("filename*")
                    {
                        let clean = if let Some(stripped) = param_val.strip_prefix("UTF-8''") {
                            stripped.to_string()
                        } else {
                            param_val
                        };
                        part_filename = Some(clean);
                    }
                }
            }
        }
    }

    (part_name, part_filename)
}

/// Returns true only when the multipart part explicitly declares
/// `Content-Encoding: gzip`. Payload bytes are never sniffed: a user uploading
/// a real `.gz` file must get those exact bytes written to disk.
fn is_part_gzip(header_str: &str) -> bool {
    header_str.lines().any(|line| {
        line.trim().split_once(':').is_some_and(|(k, v)| {
            k.trim().eq_ignore_ascii_case("content-encoding")
                && v.to_ascii_lowercase().contains("gzip")
        })
    })
}

/// Decompresses a payload whose sender declared `Content-Encoding: gzip`.
/// Unlike [`decompress_if_gzip`], a payload that is not actually gzip is an
/// error rather than being passed through unchanged.
pub fn gunzip_declared(data: &[u8]) -> Result<Vec<u8>, String> {
    if data.len() < 2 || data[0] != 0x1f || data[1] != 0x8b {
        return Err("Content-Encoding is gzip but payload is not gzip data".to_string());
    }
    decompress_if_gzip(data)
}

/// Resolves a possibly-relative upload path against the daemon workdir so the
/// destination never depends on podd's own process working directory.
fn absolutize_upload_path(p: &str, workdir: &str) -> String {
    if p.starts_with('/') {
        p.to_string()
    } else {
        format!("{}/{}", workdir.trim_end_matches('/'), p)
    }
}

#[derive(Clone, Debug)]
pub struct MultipartPart {
    pub name: Option<String>,
    pub filename: Option<String>,
    pub data: Vec<u8>,
}

pub fn parse_multipart_body(body: &[u8], boundary: &str) -> Result<Vec<MultipartPart>, String> {
    let delim = format!("--{boundary}").into_bytes();
    let first_pos = match find_subsequence(body, &delim) {
        Some(p) => p,
        None => return Ok(Vec::new()),
    };

    let mut cur = first_pos + delim.len();
    let mut parts = Vec::new();

    while cur < body.len() {
        if body[cur..].starts_with(b"--") {
            break;
        }

        if body[cur..].starts_with(b"\r\n") {
            cur += 2;
        } else if body[cur..].starts_with(b"\n") {
            cur += 1;
        } else {
            let mut ws = 0;
            while cur + ws < body.len() && (body[cur + ws] == b' ' || body[cur + ws] == b'\t') {
                ws += 1;
            }
            if cur + ws < body.len() && body[cur + ws..].starts_with(b"\r\n") {
                cur += ws + 2;
            } else if cur + ws < body.len() && body[cur + ws..].starts_with(b"\n") {
                cur += ws + 1;
            } else {
                break;
            }
        }

        if cur >= body.len() {
            break;
        }

        let crlf = find_subsequence(&body[cur..], b"\r\n\r\n").map(|p| (p, 4));
        let lf = find_subsequence(&body[cur..], b"\n\n").map(|p| (p, 2));
        let (header_len, sep_len) = match (crlf, lf) {
            (Some((p1, l1)), Some((p2, l2))) => {
                if p1 <= p2 {
                    (p1, l1)
                } else {
                    (p2, l2)
                }
            }
            (Some(p), None) => p,
            (None, Some(p)) => p,
            (None, None) => break,
        };

        let header_bytes = &body[cur..cur + header_len];
        let content_start = cur + header_len + sep_len;

        let next_rel = match find_subsequence(&body[content_start..], &delim) {
            Some(p) => p,
            None => break,
        };

        let next_delim_pos = content_start + next_rel;
        let mut content_end = next_delim_pos;
        if content_end >= content_start + 2 && &body[content_end - 2..content_end] == b"\r\n" {
            content_end -= 2;
        } else if content_end > content_start && body[content_end - 1] == b'\n' {
            content_end -= 1;
        }

        let raw_part_data = &body[content_start..content_end];
        let header_str = String::from_utf8_lossy(header_bytes);
        let (name, filename) = parse_content_disposition(&header_str);

        let data = if is_part_gzip(&header_str) {
            gunzip_declared(raw_part_data)?
        } else {
            raw_part_data.to_vec()
        };

        parts.push(MultipartPart {
            name,
            filename,
            data,
        });

        cur = next_delim_pos + delim.len();
    }

    Ok(parts)
}

pub fn resolve_file_path(
    part_filename: Option<&str>,
    query_path: Option<&str>,
    is_single_part: bool,
    workdir: &str,
) -> Result<String, String> {
    let clean_filename = part_filename.map(|s| s.trim()).filter(|s| !s.is_empty());
    let clean_query = query_path.map(|s| s.trim()).filter(|s| !s.is_empty());

    // Single file: the `path` query parameter is authoritative. A trailing `/`
    // marks it as a directory, in which case the part filename's basename is
    // appended.
    if is_single_part {
        if let Some(qp) = clean_query {
            if qp.ends_with('/') {
                if let Some(fn_str) = clean_filename {
                    let base = Path::new(fn_str)
                        .file_name()
                        .and_then(|n| n.to_str())
                        .unwrap_or(fn_str);
                    return Ok(absolutize_upload_path(&format!("{qp}{base}"), workdir));
                }
            }
            return Ok(absolutize_upload_path(qp, workdir));
        }
    }

    // Multiple files (or no query path): each part filename is the target,
    // relative ones resolved under the query directory or the workdir.
    if let Some(fn_str) = clean_filename {
        if fn_str.starts_with('/') {
            return Ok(fn_str.to_string());
        }
        let base_dir = absolutize_upload_path(clean_query.unwrap_or(workdir), workdir);
        let trimmed = base_dir.trim_end_matches('/');
        return Ok(format!("{trimmed}/{fn_str}"));
    }

    if let Some(qp) = clean_query {
        return Ok(absolutize_upload_path(qp, workdir));
    }

    Err("missing path for file upload".to_string())
}

pub fn handle_file_post(
    stream: &mut TcpStream,
    req: &HttpRequest,
    query_params: &HashMap<String, String>,
    config: &DaemonConfig,
) {
    let content_type = req
        .headers
        .get("content-type")
        .map(|s| s.as_str())
        .unwrap_or("");

    let (uid, gid) = resolve_file_ownership(query_params);

    let is_multipart = content_type
        .trim()
        .to_ascii_lowercase()
        .starts_with("multipart/form-data");

    if is_multipart {
        let boundary = match extract_multipart_boundary(content_type) {
            Some(b) => b,
            None => {
                write_json(
                    stream,
                    "400 Bad Request",
                    r#"{"error":"missing_multipart_boundary"}"#,
                );
                return;
            }
        };

        let parts = match parse_multipart_body(&req.body, &boundary) {
            Ok(p) => p,
            Err(e) => {
                let resp = format!(
                    r#"{{"error":"invalid_multipart_body","details":"{}"}}"#,
                    escape_json(&e)
                );
                write_json(stream, "400 Bad Request", &resp);
                return;
            }
        };

        let file_parts: Vec<&MultipartPart> = parts
            .iter()
            .filter(|p| p.filename.is_some() || p.name.as_deref() == Some("file"))
            .collect();

        if file_parts.is_empty() {
            write_json(stream, "400 Bad Request", r#"{"error":"no_files_found"}"#);
            return;
        }

        let is_single = file_parts.len() == 1;
        let query_path = query_params.get("path").map(|s| s.as_str());
        let mut written_files = Vec::new();

        for part in file_parts {
            let target_path = match resolve_file_path(
                part.filename.as_deref(),
                query_path,
                is_single,
                &config.workdir,
            ) {
                Ok(p) => p,
                Err(e) => {
                    let resp = format!(
                        r#"{{"error":"missing_file_path","details":"{}"}}"#,
                        escape_json(&e)
                    );
                    write_json(stream, "400 Bad Request", &resp);
                    return;
                }
            };

            let path = Path::new(&target_path);
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

            if let Err(e) = std::fs::write(path, &part.data) {
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

            chown_if_root(path, uid, gid);

            let name = path
                .file_name()
                .and_then(|n| n.to_str())
                .unwrap_or(target_path.as_str());
            written_files.push((name.to_string(), target_path));
        }

        let mut json_items = Vec::new();
        for (name, fpath) in written_files {
            json_items.push(format!(
                r#"{{"name":"{}","path":"{}","type":"file"}}"#,
                escape_json(&name),
                escape_json(&fpath)
            ));
        }
        let resp = format!("[{}]", json_items.join(","));
        write_json(stream, "200 OK", &resp);
    } else {
        // Octet-stream or raw/JSON body upload
        let mut target_path = query_params.get("path").cloned();
        let mut final_data: Option<Vec<u8>> = None;

        if target_path.is_none() && !req.body.is_empty() && req.body[0] == b'{' {
            if let Ok(json_str) = std::str::from_utf8(&req.body) {
                if let Ok(val) = JsonValue::parse(json_str) {
                    if let Some(p) = val.get("path").and_then(|v| v.as_str()) {
                        target_path = Some(p.to_string());
                        if let Some(c) = val.get("content").and_then(|v| v.as_str()) {
                            final_data = Some(c.as_bytes().to_vec());
                        }
                    }
                }
            }
        }

        let raw_path = match target_path {
            Some(p) if !p.trim().is_empty() => absolutize_upload_path(p.trim(), &config.workdir),
            _ => {
                write_json(
                    stream,
                    "400 Bad Request",
                    r#"{"error":"missing_path_parameter"}"#,
                );
                return;
            }
        };

        // Only honor an explicit Content-Encoding header; never sniff magic
        // bytes, or uploads of genuine .gz files would be silently inflated.
        let is_gzip = req
            .headers
            .get("content-encoding")
            .map(|v| v.to_ascii_lowercase().contains("gzip"))
            .unwrap_or(false);

        let data = if let Some(fd) = final_data {
            fd
        } else if is_gzip {
            match gunzip_declared(&req.body) {
                Ok(decompressed) => decompressed,
                Err(e) => {
                    let resp = format!(
                        r#"{{"error":"gzip_decompression_error","details":"{}"}}"#,
                        escape_json(&e)
                    );
                    write_json(stream, "400 Bad Request", &resp);
                    return;
                }
            }
        } else {
            req.body.clone()
        };

        let path = Path::new(&raw_path);
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

        if let Err(e) = std::fs::write(path, &data) {
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

        chown_if_root(path, uid, gid);

        let name = path
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or(raw_path.as_str());
        let resp = format!(
            r#"[{{"name":"{}","path":"{}","type":"file"}}]"#,
            escape_json(name),
            escape_json(&raw_path)
        );
        write_json(stream, "200 OK", &resp);
    }
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
        let mut cmd = Command::new("gzip");
        cmd.arg("-dc")
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped());
        let mut child =
            spawn_tracked(&mut cmd).map_err(|e| format!("failed to spawn gzip: {e}"))?;

        if let Some(mut stdin) = child.stdin().take() {
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

fn is_invalid_remove_path(path: &str) -> bool {
    if is_invalid_tar_path(path) {
        return true;
    }
    let rel = path
        .strip_prefix("./")
        .unwrap_or(path)
        .trim_start_matches('/');
    rel.is_empty() || rel == "."
}

fn remove_archive_paths(dest_path: &Path, remove_list: &[&str]) -> Result<(), std::io::Error> {
    for p in remove_list {
        let rel_p = p.strip_prefix("./").unwrap_or(p).trim_start_matches('/');
        if rel_p.is_empty() || rel_p == "." {
            continue;
        }
        let target = dest_path.join(rel_p);
        if let Ok(meta) = std::fs::symlink_metadata(&target) {
            if meta.file_type().is_dir() {
                std::fs::remove_dir_all(&target)?;
            } else {
                std::fs::remove_file(&target)?;
            }
        }
    }
    Ok(())
}

pub fn handle_archive_sync(
    stream: &mut TcpStream,
    req: &HttpRequest,
    query_params: &HashMap<String, String>,
    config: &DaemonConfig,
) {
    let dest_str = query_params
        .get("dest")
        .or_else(|| query_params.get("destination"))
        .or_else(|| query_params.get("path"))
        .map_or(config.workdir.as_str(), |s| s.as_str());

    let (uid, gid) = resolve_file_ownership(query_params);

    let remove_list: Vec<&str> = query_params
        .get("remove_paths")
        .or_else(|| query_params.get("removePaths"))
        .map(|raw| {
            raw.split(',')
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .collect()
        })
        .unwrap_or_default();

    for p in &remove_list {
        if is_invalid_remove_path(p) {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"invalid_archive_path"}"#,
            );
            return;
        }
    }

    if req.body.is_empty() {
        if remove_list.is_empty() {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"empty_archive_body"}"#,
            );
            return;
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

        if let Err(e) = remove_archive_paths(dest_path, &remove_list) {
            let resp = format!(
                r#"{{"error":"failed_to_remove_path","details":"{}"}}"#,
                escape_json(&e.to_string())
            );
            write_json(stream, "500 Internal Server Error", &resp);
            return;
        }

        write_json(stream, "200 OK", r#"{"status":"ok","extracted":0}"#);
        return;
    }

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
    if !remove_list.is_empty() {
        if let Err(e) = remove_archive_paths(dest_path, &remove_list) {
            let resp = format!(
                r#"{{"error":"failed_to_remove_path","details":"{}"}}"#,
                escape_json(&e.to_string())
            );
            write_json(stream, "500 Internal Server Error", &resp);
            return;
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
