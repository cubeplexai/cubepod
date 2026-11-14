//! Unit & Integration Tests for podd.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::process::Command;
use std::time::Duration;

use crate::commands::ProcessRegistry;
use crate::http::find_subsequence;
use crate::json::{escape_json, JsonValue};
use crate::{serve, DaemonConfig};

fn http_request(
    addr: &str,
    method: &str,
    target: &str,
    headers: &[(&str, &str)],
    body: &[u8],
) -> (u16, HashMap<String, String>, Vec<u8>) {
    let mut stream = TcpStream::connect(addr).expect("failed to connect to test server");
    let mut req = format!("{method} {target} HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n");
    let mut has_cl = false;
    for (k, v) in headers {
        has_cl |= k.eq_ignore_ascii_case("content-length");
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    if !has_cl && !body.is_empty() {
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

    let (header_bytes, body_bytes) = match find_subsequence(&response_bytes, b"\r\n\r\n") {
        Some(pos) => (&response_bytes[..pos], &response_bytes[pos + 4..]),
        None => panic!("invalid http response: no header delimiter"),
    };

    let header_str = String::from_utf8_lossy(header_bytes);
    let mut lines = header_str.lines();
    let status_line = lines.next().expect("empty status line");
    let status_code: u16 = status_line
        .split_whitespace()
        .nth(1)
        .unwrap()
        .parse()
        .unwrap();

    let res_headers = lines
        .filter_map(|l| l.split_once(':'))
        .map(|(k, v)| (k.trim().to_ascii_lowercase(), v.trim().to_string()))
        .collect();

    (status_code, res_headers, body_bytes.to_vec())
}

fn get(addr: &str, path: &str) -> (u16, HashMap<String, String>, Vec<u8>) {
    http_request(addr, "GET", path, &[], &[])
}

fn post_raw(
    addr: &str,
    path: &str,
    ct: &str,
    body: &[u8],
) -> (u16, HashMap<String, String>, Vec<u8>) {
    http_request(addr, "POST", path, &[("Content-Type", ct)], body)
}

fn post_json(addr: &str, path: &str, body: &str) -> (u16, HashMap<String, String>, Vec<u8>) {
    post_raw(addr, path, "application/json", body.as_bytes())
}

fn put_raw(addr: &str, path: &str, body: &[u8]) -> (u16, HashMap<String, String>, Vec<u8>) {
    http_request(
        addr,
        "PUT",
        path,
        &[("Content-Type", "application/octet-stream")],
        body,
    )
}

fn delete(addr: &str, path: &str) -> (u16, HashMap<String, String>, Vec<u8>) {
    http_request(addr, "DELETE", path, &[], &[])
}

fn start_test_server(workdir: &str) -> (String, ProcessRegistry) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let registry = ProcessRegistry::new();
    let config = DaemonConfig {
        workdir: workdir.to_string(),
    };
    let reg_clone = registry.clone();
    std::thread::spawn(move || serve(listener, config, reg_clone));
    (addr.to_string(), registry)
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
    out.extend(std::iter::repeat_n(0u8, pad + 1024));
    out
}

#[test]
fn test_json_parser_and_escaping() {
    let raw = r#"{"string": "hello \"world\"\n\t\\", "bool_t": true, "bool_f": false, "null_val": null, "num_int": 42, "num_float": 3.14, "nested": {"key": "val"}, "list": [1, 2, "three"]}"#;

    let json = JsonValue::parse(raw).expect("json parse failed");
    let str_val = json.get("string").and_then(|v| v.as_str());
    assert_eq!(str_val, Some("hello \"world\"\n\t\\"));
    assert_eq!(json.get("bool_t").unwrap().as_bool(), Some(true));
    assert_eq!(json.get("bool_f").unwrap().as_bool(), Some(false));
    assert_eq!(json.get("null_val"), Some(&JsonValue::Null));
    assert_eq!(json.get("num_int").unwrap().as_u64(), Some(42));
    let nested = json
        .get("nested")
        .and_then(|v| v.get("key"))
        .and_then(|v| v.as_str());
    assert_eq!(nested, Some("val"));

    let escaped = escape_json("line1\nline2\t\"quoted\"");
    assert_eq!(escaped, "line1\\nline2\\t\\\"quoted\\\"");
}

#[test]
fn test_health_endpoint() {
    let (addr, _) = start_test_server("/tmp");
    let (status, _, body) = get(&addr, "/health");
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("status").and_then(|v| v.as_str()), Some("ok"));
    assert_eq!(json.get("daemon").and_then(|v| v.as_str()), Some("podd-rs"));
    assert_eq!(json.get("workdir").and_then(|v| v.as_str()), Some("/tmp"));
    assert_eq!(
        json.get("active_processes").and_then(|v| v.as_u64()),
        Some(0)
    );
}

#[test]
fn test_prepare_stop_endpoint() {
    let (addr, _) = start_test_server("/tmp");
    let (status, _, body) = http_request(&addr, "POST", "/v1/lifecycle/prepare-stop", &[], &[]);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("status").and_then(|v| v.as_str()), Some("synced"));
    assert_eq!(
        json.get("active_processes").and_then(|v| v.as_u64()),
        Some(0)
    );
}

#[test]
fn test_command_foreground_execution() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"command": "echo 'hello foreground'", "background": false}"#;
    let (status, _, body) = post_json(&addr, "/v1/commands/run", req_body);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("running").unwrap().as_bool(), Some(false));
    assert_eq!(json.get("exit_code").unwrap().as_u64(), Some(0));
    let out = json.get("output").unwrap().as_str().unwrap();
    assert_eq!(out, "hello foreground\n");
    assert_eq!(json.get("cursor").unwrap().as_u64(), Some(17));
    let id = json.get("id").unwrap().as_str().unwrap();

    let (status_code, _, status_body) = get(&addr, &format!("/v1/commands/{id}/status"));
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

    let (status, _, body) = post_json(&addr, "/v1/commands/run", &req_body);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    let output = json.get("output").unwrap().as_str().unwrap();
    let dir_name = dir.file_name().unwrap().to_string_lossy();
    assert!(output.contains("foobar123"), "output: {output}");
    assert!(output.contains(dir_name.as_ref()), "output: {output}");

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn test_command_foreground_exit_code() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"command": "exit 42", "background": false}"#;
    let (status, _, body) = post_json(&addr, "/v1/commands/run", req_body);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("exit_code").unwrap().as_u64(), Some(42));
    assert_eq!(json.get("running").unwrap().as_bool(), Some(false));
}

#[test]
fn test_command_background_and_cursor_logs() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"command": "echo line1; sleep 0.2; echo line2", "background": true}"#;
    let (status, _, body) = post_json(&addr, "/v1/commands/run", req_body);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("running").unwrap().as_bool(), Some(true));
    assert_eq!(json.get("exit_code"), Some(&JsonValue::Null));
    let id = json.get("id").unwrap().as_str().unwrap();

    let mut cursor = 0;
    let mut accumulated = String::new();
    for _ in 0..20 {
        let (status_code, _, log_body) =
            get(&addr, &format!("/v1/commands/{id}/logs?cursor={cursor}"));
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
        "missing line1: {accumulated}"
    );
    assert!(
        accumulated.contains("line2\n"),
        "missing line2: {accumulated}"
    );

    let mut finished = false;
    for _ in 0..20 {
        let (status_code, _, status_body) = get(&addr, &format!("/v1/commands/{id}/status"));
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
    let (status, _, body) = post_json(&addr, "/v1/commands/run", req_body);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("running").unwrap().as_bool(), Some(true));
    let id = json.get("id").unwrap().as_str().unwrap();

    let (kill_status, _, kill_body) =
        http_request(&addr, "POST", &format!("/v1/commands/{id}/kill"), &[], &[]);
    assert_eq!(kill_status, 200);
    let kill_json = JsonValue::parse(std::str::from_utf8(&kill_body).unwrap()).unwrap();
    assert_eq!(kill_json.get("status").unwrap().as_str().unwrap(), "killed");

    let mut stopped = false;
    for _ in 0..20 {
        let (status_code, _, status_body) = get(&addr, &format!("/v1/commands/{id}/status"));
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
    let (put_status, _, put_resp) = put_raw(
        &addr,
        &format!("/v1/files?path={file_path_str}&mode=0644"),
        put_body,
    );
    assert_eq!(put_status, 200);
    let put_json = JsonValue::parse(std::str::from_utf8(&put_resp).unwrap()).unwrap();
    let put_path = put_json.get("path").unwrap().as_str().unwrap();
    assert_eq!(put_path, file_path_str);
    assert_eq!(
        put_json.get("size").unwrap().as_u64(),
        Some(put_body.len() as u64)
    );

    // GET file
    let (get_status, _, get_body) = get(&addr, &format!("/v1/files?path={file_path_str}"));
    assert_eq!(get_status, 200);
    assert_eq!(get_body, put_body);

    // GET file list
    let (list_status, _, list_body) = get(
        &addr,
        &format!("/v1/files/list?path={}", dir.to_string_lossy()),
    );
    assert_eq!(list_status, 200);
    let list_json = JsonValue::parse(std::str::from_utf8(&list_body).unwrap()).unwrap();
    let entries = match list_json.get("entries").unwrap() {
        JsonValue::Array(a) => a,
        _ => panic!("expected array for entries"),
    };
    assert_eq!(entries.len(), 1);
    let e0 = &entries[0];
    assert_eq!(e0.get("name").unwrap().as_str().unwrap(), "test.txt");
    assert_eq!(e0.get("is_dir").unwrap().as_bool(), Some(false));
    assert_eq!(
        e0.get("size").unwrap().as_u64(),
        Some(put_body.len() as u64)
    );

    // GET nonexistent file
    let (nf_status, _, _) = get(
        &addr,
        &format!("/v1/files?path={}/nonexistent.txt", dir.to_string_lossy()),
    );
    assert_eq!(nf_status, 404);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn test_404_routing() {
    let (addr, _) = start_test_server("/tmp");
    let (status, _, _) = get(&addr, "/invalid/route");
    assert_eq!(status, 404);

    let (status_cmd, _, _) = get(&addr, "/v1/commands/unknown-proc/status");
    assert_eq!(status_cmd, 404);

    let (status_kill, _, _) =
        http_request(&addr, "POST", "/v1/commands/unknown-proc/kill", &[], &[]);
    assert_eq!(status_kill, 404);
}

#[test]
fn test_archive_sync_tar_gz_and_remove_paths() {
    let test_root = std::env::temp_dir().join(format!("podd-archive-{}", std::process::id()));
    let dest_dir = test_root.join("workspace");
    let src_dir = test_root.join("src_files");
    std::fs::create_dir_all(&dest_dir).unwrap();
    std::fs::create_dir_all(src_dir.join("src")).unwrap();

    let stale_file = dest_dir.join("stale.txt");
    std::fs::write(&stale_file, b"old content").unwrap();
    assert!(stale_file.exists());

    let main_py = src_dir.join("src/main.py");
    let readme = src_dir.join("README.md");
    std::fs::write(&main_py, b"print('hello world')\n").unwrap();
    std::fs::write(&readme, b"# Sample Archive Sync Project\n").unwrap();

    let tar_gz_path = test_root.join("archive.tar.gz");
    let mut tar_cmd = Command::new("tar");
    tar_cmd.args([
        "-czf",
        tar_gz_path.to_str().unwrap(),
        "-C",
        src_dir.to_str().unwrap(),
        "src/main.py",
        "README.md",
    ]);
    let tar_status = crate::startcmd::spawn_tracked(&mut tar_cmd)
        .expect("failed to execute tar command")
        .wait()
        .expect("failed to wait for tar command");
    assert!(tar_status.success());
    let tar_gz_bytes = std::fs::read(&tar_gz_path).unwrap();

    let (addr, _) = start_test_server("/tmp");
    let target_url = format!(
        "/v1/files/archive-sync?dest={}&uid=1000&gid=1000&remove_paths=stale.txt",
        dest_dir.to_str().unwrap()
    );
    let (status, _, resp_body) = post_raw(&addr, &target_url, "application/gzip", &tar_gz_bytes);
    assert_eq!(status, 200);

    let json = JsonValue::parse(std::str::from_utf8(&resp_body).unwrap()).unwrap();
    assert_eq!(json.get("status").unwrap().as_str().unwrap(), "synced");
    assert_eq!(
        json.get("dest").unwrap().as_str().unwrap(),
        dest_dir.to_str().unwrap()
    );
    assert_eq!(json.get("extracted").unwrap().as_u64(), Some(2));
    assert!(!stale_file.exists(), "stale.txt should have been removed");
    let main_b = std::fs::read(dest_dir.join("src/main.py")).unwrap();
    assert_eq!(main_b, b"print('hello world')\n");
    let readme_b = std::fs::read(dest_dir.join("README.md")).unwrap();
    assert_eq!(readme_b, b"# Sample Archive Sync Project\n");

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

    let (status, _, resp_body) = post_raw(&addr, &target_url, "application/x-tar", &malicious_tar);
    assert_eq!(status, 400);

    let json = JsonValue::parse(std::str::from_utf8(&resp_body).unwrap()).unwrap();
    assert_eq!(
        json.get("error").unwrap().as_str().unwrap(),
        "invalid_archive_path"
    );

    let escaped_path = test_root.join("escape.txt");
    assert!(
        !escaped_path.exists(),
        "malicious file must not be extracted"
    );

    let leading_slash_tar = craft_tar_entry("/escape2.txt", b"malicious content 2");
    let (status2, _, _) = post_raw(&addr, &target_url, "application/x-tar", &leading_slash_tar);
    assert_eq!(status2, 400);

    let _ = std::fs::remove_dir_all(&test_root);
}

#[test]
fn test_archive_sync_empty_body_remove_paths() {
    let test_root = std::env::temp_dir().join(format!("podd-archive-empty-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&test_root);
    let dest_dir = test_root.join("workspace");
    std::fs::create_dir_all(dest_dir.join("sub")).unwrap();

    let keep_file = dest_dir.join("keep.txt");
    let file_a = dest_dir.join("old.txt");
    let dir_b = dest_dir.join("sub");
    let nested_b = dir_b.join("inner.txt");
    std::fs::write(&keep_file, b"keep").unwrap();
    std::fs::write(&file_a, b"remove me").unwrap();
    std::fs::write(&nested_b, b"remove dir").unwrap();

    let (addr, _) = start_test_server("/tmp");

    // 1. Empty body + empty remove_paths still returns 400 empty_archive_body
    let empty_url = format!("/v1/files/archive-sync?dest={}", dest_dir.to_str().unwrap());
    let (st_empty, _, body_empty) = post_raw(&addr, &empty_url, "application/gzip", b"");
    assert_eq!(st_empty, 400);
    let json_empty = JsonValue::parse(std::str::from_utf8(&body_empty).unwrap()).unwrap();
    assert_eq!(
        json_empty.get("error").unwrap().as_str().unwrap(),
        "empty_archive_body"
    );

    // Whitespace-only remove_paths also treats parsed list as empty
    let ws_url = format!(
        "/v1/files/archive-sync?dest={}&remove_paths=%20,%20",
        dest_dir.to_str().unwrap()
    );
    let (st_ws, _, _) = post_raw(&addr, &ws_url, "application/gzip", b"");
    assert_eq!(st_ws, 400);

    // 2. Traversal / leading '/' in remove_paths is rejected with 400
    for bad in ["../escape.txt", "/etc/passwd", "old.txt,../../secret"] {
        let bad_url = format!(
            "/v1/files/archive-sync?dest={}&remove_paths={}",
            dest_dir.to_str().unwrap(),
            bad
        );
        let (st_bad, _, body_bad) = post_raw(&addr, &bad_url, "application/gzip", b"");
        assert_eq!(st_bad, 400, "expected 400 for bad remove_paths {bad}");
        let json_bad = JsonValue::parse(std::str::from_utf8(&body_bad).unwrap()).unwrap();
        assert_eq!(
            json_bad.get("error").unwrap().as_str().unwrap(),
            "invalid_archive_path"
        );
    }
    assert!(file_a.exists(), "invalid request must not mutate disk");

    // 3. Empty body + comma-separated remove_paths succeeds with extracted: 0
    let del_url = format!(
        "/v1/files/archive-sync?dest={}&remove_paths=old.txt,%20sub",
        dest_dir.to_str().unwrap()
    );
    let (st_del, _, body_del) = post_raw(&addr, &del_url, "application/gzip", b"");
    assert_eq!(st_del, 200);
    let json_del = JsonValue::parse(std::str::from_utf8(&body_del).unwrap()).unwrap();
    assert_eq!(json_del.get("status").unwrap().as_str().unwrap(), "ok");
    assert_eq!(json_del.get("extracted").unwrap().as_u64(), Some(0));
    assert!(!file_a.exists(), "old.txt should be deleted");
    assert!(!dir_b.exists(), "sub directory should be deleted");
    assert!(keep_file.exists(), "keep.txt must remain intact");

    let _ = std::fs::remove_dir_all(&test_root);
}

#[test]
fn test_file_delete_endpoint() {
    let dir = std::env::temp_dir().join(format!("podd-del-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();

    let (addr, _) = start_test_server(&dir.to_string_lossy());

    let file_path = dir.join("to_delete.txt");
    std::fs::write(&file_path, b"delete me").unwrap();
    assert!(file_path.exists());

    let file_path_str = file_path.to_string_lossy().to_string();
    let (del_status, _, del_resp) = delete(&addr, &format!("/v1/files?path={file_path_str}"));
    assert_eq!(del_status, 200);
    let del_json = JsonValue::parse(std::str::from_utf8(&del_resp).unwrap()).unwrap();
    let del_str = del_json.get("deleted").unwrap().as_str().unwrap();
    assert_eq!(del_str, file_path_str);
    assert!(!file_path.exists(), "file should be deleted");

    let (nf_status, _, _) = delete(&addr, &format!("/v1/files?path={file_path_str}"));
    assert_eq!(nf_status, 404);

    let subdir = dir.join("subdir");
    std::fs::create_dir_all(&subdir).unwrap();
    std::fs::write(subdir.join("nested.txt"), b"nested").unwrap();
    assert!(subdir.exists());

    let subdir_str = subdir.to_string_lossy().to_string();
    let (dir_status, _, dir_resp) = delete(&addr, &format!("/v1/files?path={subdir_str}"));
    assert_eq!(dir_status, 200);
    let dir_json = JsonValue::parse(std::str::from_utf8(&dir_resp).unwrap()).unwrap();
    assert_eq!(
        dir_json.get("deleted").unwrap().as_str().unwrap(),
        subdir_str
    );
    assert!(!subdir.exists(), "directory should be deleted");

    let (bad_status, _, _) = delete(&addr, "/v1/files");
    assert_eq!(bad_status, 400);

    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn test_code_execute_stdout_and_result_streaming() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"code":"print('Hello from test interpreter')\n21 * 2"}"#;
    let (status, headers, body) =
        post_raw(&addr, "/execute", "application/json", req_body.as_bytes());
    assert_eq!(status, 200);
    assert!(headers
        .get("content-type")
        .map(|c| c.contains("application/x-ndjson"))
        .unwrap_or(false));

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert_eq!(
        lines.len(),
        3,
        "expected stdout, result, and number_of_executions: {:?}",
        lines
    );

    let l1 = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(l1.get("type").unwrap().as_str().unwrap(), "stdout");
    assert_eq!(
        l1.get("text").unwrap().as_str().unwrap(),
        "Hello from test interpreter\n"
    );
    assert!(l1.get("timestamp").unwrap().as_u64().is_some());

    let l2 = JsonValue::parse(lines[1]).unwrap();
    assert_eq!(l2.get("type").unwrap().as_str().unwrap(), "result");
    assert_eq!(l2.get("text").unwrap().as_str().unwrap(), "42");
    assert_eq!(l2.get("png").unwrap(), &JsonValue::Null);
    assert_eq!(l2.get("html").unwrap(), &JsonValue::Null);

    let l3 = JsonValue::parse(lines[2]).unwrap();
    assert_eq!(
        l3.get("type").unwrap().as_str().unwrap(),
        "number_of_executions"
    );
    assert_eq!(l3.get("execution_count").unwrap().as_u64(), Some(1));
}

#[test]
fn test_code_execute_stderr_streaming() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"code":"import sys\n_ = sys.stderr.write('Warning: deprecated API\\n')\n"}"#;
    let (status, headers, body) = post_raw(
        &addr,
        "/v1/code/execute",
        "application/json",
        req_body.as_bytes(),
    );
    assert_eq!(status, 200);
    assert!(headers
        .get("content-type")
        .map(|c| c.contains("application/x-ndjson"))
        .unwrap_or(false));

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert_eq!(
        lines.len(),
        2,
        "expected stderr and number_of_executions: {:?}",
        lines
    );

    let l1 = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(l1.get("type").unwrap().as_str().unwrap(), "stderr");
    assert_eq!(
        l1.get("text").unwrap().as_str().unwrap(),
        "Warning: deprecated API\n"
    );
    assert!(l1.get("timestamp").unwrap().as_u64().is_some());

    let l2 = JsonValue::parse(lines[1]).unwrap();
    assert_eq!(
        l2.get("type").unwrap().as_str().unwrap(),
        "number_of_executions"
    );
    assert_eq!(l2.get("execution_count").unwrap().as_u64(), Some(1));
}

#[test]
fn test_code_execute_error_formatting() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"code":"1 / 0"}"#;
    let (status, _, body) = post_raw(&addr, "/execute", "application/json", req_body.as_bytes());
    assert_eq!(status, 200);

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert_eq!(
        lines.len(),
        2,
        "expected error and number_of_executions: {:?}",
        lines
    );

    let err_obj = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(err_obj.get("type").unwrap().as_str().unwrap(), "error");
    assert_eq!(
        err_obj.get("name").unwrap().as_str().unwrap(),
        "ZeroDivisionError"
    );
    assert_eq!(
        err_obj.get("value").unwrap().as_str().unwrap(),
        "division by zero"
    );
    assert!(err_obj
        .get("traceback")
        .unwrap()
        .as_str()
        .unwrap()
        .contains("ZeroDivisionError"));

    let num_obj = JsonValue::parse(lines[1]).unwrap();
    assert_eq!(
        num_obj.get("type").unwrap().as_str().unwrap(),
        "number_of_executions"
    );
    assert_eq!(num_obj.get("execution_count").unwrap().as_u64(), Some(1));
}

#[test]
fn test_code_execute_syntax_error() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"code":"def bad_syntax(:"}"#;
    let (status, _, body) = post_raw(&addr, "/execute", "application/json", req_body.as_bytes());
    assert_eq!(status, 200);

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert_eq!(lines.len(), 2);

    let err_obj = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(err_obj.get("type").unwrap().as_str().unwrap(), "error");
    assert_eq!(
        err_obj.get("name").unwrap().as_str().unwrap(),
        "SyntaxError"
    );
    assert!(err_obj
        .get("traceback")
        .unwrap()
        .as_str()
        .unwrap()
        .contains("SyntaxError"));

    let num_obj = JsonValue::parse(lines[1]).unwrap();
    assert_eq!(
        num_obj.get("type").unwrap().as_str().unwrap(),
        "number_of_executions"
    );
}

#[test]
fn test_code_contexts_crud_and_persistence() {
    let (addr, _) = start_test_server("/workspace");

    // 1. POST /contexts
    let (status, _, body) = post_json(
        &addr,
        "/contexts",
        r#"{"language":"python","cwd":"/workspace"}"#,
    );
    assert_eq!(status, 200);
    let created = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    let ctx_id = created.get("id").unwrap().as_str().unwrap().to_string();
    assert_eq!(created.get("language").unwrap().as_str().unwrap(), "python");
    assert_eq!(created.get("cwd").unwrap().as_str().unwrap(), "/workspace");

    // 2. GET /contexts
    let (list_status, _, list_body) = get(&addr, "/contexts");
    assert_eq!(list_status, 200);
    let list_json = JsonValue::parse(std::str::from_utf8(&list_body).unwrap()).unwrap();
    match list_json {
        JsonValue::Array(arr) => {
            assert!(arr
                .iter()
                .any(|item| item.get("id").and_then(|i| i.as_str()) == Some(&ctx_id)));
        }
        _ => panic!("expected array for GET /contexts"),
    }

    // 3. POST /execute in context: define state
    let run1 = format!(
        r#"{{"context_id":"{ctx_id}","code":"stored_value = 100\nprint('defined')\nstored_value + 25"}}"#
    );
    let (r1_status, _, r1_body) = post_raw(&addr, "/execute", "application/json", run1.as_bytes());
    assert_eq!(r1_status, 200);
    let r1_body_str = std::str::from_utf8(&r1_body).unwrap();
    let r1_lines: Vec<&str> = r1_body_str.lines().filter(|l| !l.is_empty()).collect();
    assert_eq!(r1_lines.len(), 3);
    let r1_res = JsonValue::parse(r1_lines[1]).unwrap();
    assert_eq!(r1_res.get("text").unwrap().as_str().unwrap(), "125");
    let r1_cnt = JsonValue::parse(r1_lines[2]).unwrap();
    assert_eq!(r1_cnt.get("execution_count").unwrap().as_u64(), Some(1));

    // 4. POST /execute in context: use state from run 1
    let run2 = format!(r#"{{"context_id":"{ctx_id}","code":"stored_value * 2"}}"#);
    let (r2_status, _, r2_body) = post_raw(&addr, "/execute", "application/json", run2.as_bytes());
    assert_eq!(r2_status, 200);
    let r2_lines: Vec<&str> = std::str::from_utf8(&r2_body)
        .unwrap()
        .lines()
        .filter(|l| !l.is_empty())
        .collect();
    assert_eq!(r2_lines.len(), 2);
    let r2_res = JsonValue::parse(r2_lines[0]).unwrap();
    assert_eq!(r2_res.get("text").unwrap().as_str().unwrap(), "200");
    let r2_cnt = JsonValue::parse(r2_lines[1]).unwrap();
    assert_eq!(r2_cnt.get("execution_count").unwrap().as_u64(), Some(2));

    // 5. POST /contexts/{id}/restart: resets memory
    let (rst_status, _, rst_body) = post_raw(
        &addr,
        &format!("/contexts/{ctx_id}/restart"),
        "application/json",
        b"{}",
    );
    assert_eq!(rst_status, 200);
    let rst_json = JsonValue::parse(std::str::from_utf8(&rst_body).unwrap()).unwrap();
    assert_eq!(rst_json.get("id").unwrap().as_str().unwrap(), ctx_id);

    // 6. POST /execute in context after restart: stored_value should be undefined (NameError)
    let run3 = format!(r#"{{"context_id":"{ctx_id}","code":"stored_value"}}"#);
    let (r3_status, _, r3_body) = post_raw(&addr, "/execute", "application/json", run3.as_bytes());
    assert_eq!(r3_status, 200);
    let r3_lines: Vec<&str> = std::str::from_utf8(&r3_body)
        .unwrap()
        .lines()
        .filter(|l| !l.is_empty())
        .collect();
    assert_eq!(r3_lines.len(), 2);
    let r3_err = JsonValue::parse(r3_lines[0]).unwrap();
    assert_eq!(r3_err.get("name").unwrap().as_str().unwrap(), "NameError");

    // 7. DELETE /contexts/{id}
    let (del_status, _, del_body) = delete(&addr, &format!("/contexts/{ctx_id}"));
    assert_eq!(del_status, 200);
    let del_json = JsonValue::parse(std::str::from_utf8(&del_body).unwrap()).unwrap();
    assert_eq!(del_json.get("id").unwrap().as_str().unwrap(), ctx_id);

    // 8. Subsequent DELETE returns 404
    let (del2_status, _, _) = delete(&addr, &format!("/contexts/{ctx_id}"));
    assert_eq!(del2_status, 404);

    // 9. Subsequent execute on deleted context returns 404
    let (nf_status, _, _) = post_raw(&addr, "/execute", "application/json", run3.as_bytes());
    assert_eq!(nf_status, 404);
}

#[test]
fn test_code_execute_rich_display() {
    let (addr, _) = start_test_server("/tmp");
    let py_code = r#"
class VisualItem:
    def _repr_html_(self):
        return "<h1>Chart</h1>"
    def _repr_png_(self):
        return "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
VisualItem()
"#;
    let req = format!(r#"{{"code":"{}"}}"#, escape_json(py_code));
    let (status, _, body) = post_raw(&addr, "/execute", "application/json", req.as_bytes());
    assert_eq!(status, 200);

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert_eq!(lines.len(), 2);

    let res_obj = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(res_obj.get("type").unwrap().as_str().unwrap(), "result");
    assert_eq!(
        res_obj.get("html").unwrap().as_str().unwrap(),
        "<h1>Chart</h1>"
    );
    assert_eq!(
        res_obj.get("png").unwrap().as_str().unwrap(),
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
    );
}

#[test]
fn test_code_execute_shell_bash() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"code":"echo 'Hello from bash shell'","language":"bash"}"#;
    let (status, _, body) = post_raw(&addr, "/execute", "application/json", req_body.as_bytes());
    assert_eq!(status, 200);

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert!(lines.len() >= 2);

    let stdout_obj = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(stdout_obj.get("type").unwrap().as_str().unwrap(), "stdout");
    assert_eq!(
        stdout_obj.get("text").unwrap().as_str().unwrap(),
        "Hello from bash shell\n"
    );

    let num_obj = JsonValue::parse(lines.last().unwrap()).unwrap();
    assert_eq!(
        num_obj.get("type").unwrap().as_str().unwrap(),
        "number_of_executions"
    );
    assert_eq!(num_obj.get("execution_count").unwrap().as_u64(), Some(1));
}

#[test]
fn test_code_execute_missing_code() {
    let (addr, _) = start_test_server("/tmp");
    let (status, _, body) = post_json(&addr, "/execute", r#"{"language":"python"}"#);
    assert_eq!(status, 400);
    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(
        json.get("error").unwrap().as_str().unwrap(),
        "missing_code_field"
    );
}

fn parse_connect_frames(raw: &[u8]) -> Vec<(u8, Vec<u8>)> {
    let mut frames = Vec::new();
    let mut idx = 0;
    while idx + 5 <= raw.len() {
        let flag = raw[idx];
        let len =
            u32::from_be_bytes([raw[idx + 1], raw[idx + 2], raw[idx + 3], raw[idx + 4]]) as usize;
        let start = idx + 5;
        let end = start + len;
        if end > raw.len() {
            break;
        }
        frames.push((flag, raw[start..end].to_vec()));
        idx = end;
    }
    frames
}

fn base64_decode(input: &str) -> Vec<u8> {
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

#[test]
fn test_base64_encode_and_frames() {
    use crate::commands::{base64_encode, make_connect_frame, unpack_connect_payload};

    assert_eq!(base64_encode(b""), "");
    assert_eq!(base64_encode(b"f"), "Zg==");
    assert_eq!(base64_encode(b"fo"), "Zm8=");
    assert_eq!(base64_encode(b"foo"), "Zm9v");
    assert_eq!(base64_encode(b"hello\n"), "aGVsbG8K");

    let payload = b"{\"process\":{\"cmd\":\"echo hi\"}}";
    let frame = make_connect_frame(payload, false);
    assert_eq!(frame[0], 0);
    let len = u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]) as usize;
    assert_eq!(len, payload.len());
    assert_eq!(&frame[5..], payload);

    let unpacked = unpack_connect_payload(&frame).unwrap();
    assert_eq!(unpacked, payload);

    let raw_unpacked = unpack_connect_payload(payload).unwrap();
    assert_eq!(raw_unpacked, payload);
}

#[test]
fn test_connectrpc_start_raw_json() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"process":{"cmd":"echo 'connect-rpc raw json works'"}}"#;
    let (status, headers, body) = post_raw(
        &addr,
        "/process.Process/Start",
        "application/connect+json",
        req_body.as_bytes(),
    );
    assert_eq!(status, 200);
    assert_eq!(
        headers.get("content-type").map(|s| s.as_str()),
        Some("application/connect+json")
    );

    let frames = parse_connect_frames(&body);
    assert!(
        frames.len() >= 3,
        "expected at least start, data, end frames, got {}",
        frames.len()
    );

    // Frame 0: Start Event
    assert_eq!(frames[0].0, 0);
    let start_json: JsonValue =
        JsonValue::parse(std::str::from_utf8(&frames[0].1).unwrap()).unwrap();
    let pid = start_json
        .get("event")
        .unwrap()
        .get("start")
        .unwrap()
        .get("pid")
        .unwrap()
        .as_u64()
        .unwrap();
    assert!(pid > 0);

    // Frame 1: Data Event with stdout
    assert_eq!(frames[1].0, 0);
    let data_json: JsonValue =
        JsonValue::parse(std::str::from_utf8(&frames[1].1).unwrap()).unwrap();
    let b64_stdout = data_json
        .get("event")
        .unwrap()
        .get("data")
        .unwrap()
        .get("stdout")
        .unwrap()
        .as_str()
        .unwrap();
    let stdout_bytes = base64_decode(b64_stdout);
    let stdout_str = String::from_utf8_lossy(&stdout_bytes);
    assert_eq!(stdout_str, "connect-rpc raw json works\n");

    // Frame 2: End Event with exitCode 0
    let end_frame = &frames[frames.len() - 2];
    assert_eq!(end_frame.0, 0);
    let end_json: JsonValue = JsonValue::parse(std::str::from_utf8(&end_frame.1).unwrap()).unwrap();
    let exit_code = end_json
        .get("event")
        .unwrap()
        .get("end")
        .unwrap()
        .get("exitCode")
        .unwrap()
        .as_u64()
        .unwrap();
    assert_eq!(exit_code, 0);

    // Final Frame: EOS flag = 2
    let eos_frame = frames.last().unwrap();
    assert_eq!(eos_frame.0, 2);
}

#[test]
fn test_connectrpc_start_framed_envelope_and_args() {
    use crate::commands::make_connect_frame;

    let (addr, _) = start_test_server("/tmp");
    let payload =
        r#"{"process":{"cmd":"/bin/bash","args":["-l","-c","echo 'framed envelope args'"]}}"#;
    let framed_body = make_connect_frame(payload.as_bytes(), false);

    let (status, headers, body) = post_raw(
        &addr,
        "/process.Process/Start",
        "application/connect+json",
        &framed_body,
    );
    assert_eq!(status, 200);
    assert_eq!(
        headers.get("content-type").map(|s| s.as_str()),
        Some("application/connect+json")
    );

    let frames = parse_connect_frames(&body);
    assert!(frames.len() >= 3);

    let data_json: JsonValue =
        JsonValue::parse(std::str::from_utf8(&frames[1].1).unwrap()).unwrap();
    let b64_stdout = data_json
        .get("event")
        .unwrap()
        .get("data")
        .unwrap()
        .get("stdout")
        .unwrap()
        .as_str()
        .unwrap();
    let stdout_bytes = base64_decode(b64_stdout);
    assert_eq!(
        String::from_utf8_lossy(&stdout_bytes),
        "framed envelope args\n"
    );
}

#[test]
fn test_connectrpc_start_stderr_separation() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"process":{"cmd":"echo 'out line' && echo 'err line' >&2"}}"#;
    let (status, _, body) = post_raw(
        &addr,
        "/process.Process/Start",
        "application/connect+json",
        req_body.as_bytes(),
    );
    assert_eq!(status, 200);

    let frames = parse_connect_frames(&body);
    let mut saw_stdout = false;
    let mut saw_stderr = false;

    for (flag, payload) in &frames {
        if *flag == 0 {
            if let Ok(j) = JsonValue::parse(std::str::from_utf8(payload).unwrap()) {
                if let Some(data) = j.get("event").and_then(|e| e.get("data")) {
                    if let Some(out) = data.get("stdout").and_then(|s| s.as_str()) {
                        let text = String::from_utf8_lossy(&base64_decode(out)).to_string();
                        if text.contains("out line") {
                            saw_stdout = true;
                        }
                    }
                    if let Some(err) = data.get("stderr").and_then(|s| s.as_str()) {
                        let text = String::from_utf8_lossy(&base64_decode(err)).to_string();
                        if text.contains("err line") {
                            saw_stderr = true;
                        }
                    }
                }
            }
        }
    }

    assert!(saw_stdout, "expected stdout 'out line'");
    assert!(saw_stderr, "expected stderr 'err line'");
}

#[test]
fn test_connectrpc_send_signal_kill() {
    let (addr, registry) = start_test_server("/tmp");

    // Spawn long-running sleep command in a thread via Connect-RPC Start
    let addr_clone = addr.clone();
    let start_thread = std::thread::spawn(move || {
        let req_body = r#"{"process":{"cmd":"sleep 30"}}"#;
        post_raw(
            &addr_clone,
            "/process.Process/Start",
            "application/connect+json",
            req_body.as_bytes(),
        )
    });

    // Give it a moment to spawn and retrieve PID from registry
    let mut target_pid = 0;
    for _ in 0..50 {
        std::thread::sleep(Duration::from_millis(50));
        let active = registry.entries.lock().unwrap();
        if let Some(entry) = active.values().next() {
            let proc = entry.lock().unwrap();
            if proc.running && proc.pid > 0 {
                target_pid = proc.pid;
                break;
            }
        }
    }
    assert!(target_pid > 0, "target process should be running");

    // Send SIGKILL via /process.Process/SendSignal
    let kill_body = format!(
        r#"{{"process":{{"pid":{}}},"signal":"SIGNAL_SIGKILL"}}"#,
        target_pid
    );
    let (status, _, body) = post_raw(
        &addr,
        "/process.Process/SendSignal",
        "application/json",
        kill_body.as_bytes(),
    );
    assert_eq!(status, 200);
    assert_eq!(std::str::from_utf8(&body).unwrap(), "{}");

    // The start stream should now terminate
    let (start_status, _, start_body) = start_thread.join().unwrap();
    assert_eq!(start_status, 200);
    let frames = parse_connect_frames(&start_body);
    let end_frame = &frames[frames.len() - 2];
    let end_json: JsonValue = JsonValue::parse(std::str::from_utf8(&end_frame.1).unwrap()).unwrap();
    let exit_code = end_json
        .get("event")
        .unwrap()
        .get("end")
        .unwrap()
        .get("exitCode")
        .unwrap()
        .as_i64()
        .unwrap();
    // Killed by SIGKILL (signal 9 -> 128 + 9 = 137 or -1)
    assert!(
        exit_code == 137 || exit_code == -1,
        "expected killed exitCode 137 or -1, got {exit_code}"
    );
}

#[test]
fn test_connectrpc_send_signal_framed_and_not_found() {
    use crate::commands::make_connect_frame;

    let (addr, _) = start_test_server("/tmp");

    // Unknown PID returns 404
    let not_found_req = r#"{"process":{"pid":9999999},"signal":"SIGNAL_SIGKILL"}"#;
    let (status, _, body) = post_raw(
        &addr,
        "/process.Process/SendSignal",
        "application/json",
        not_found_req.as_bytes(),
    );
    assert_eq!(status, 404);
    assert!(std::str::from_utf8(&body).unwrap().contains("not_found"));

    // Framed payload also accepted
    let framed = make_connect_frame(not_found_req.as_bytes(), false);
    let (status2, _, _) = post_raw(
        &addr,
        "/process.Process/SendSignal",
        "application/connect+json",
        &framed,
    );
    assert_eq!(status2, 404);
}

#[test]
fn test_interpreter_zero_latency_flushing() {
    let (addr, _) = start_test_server("/tmp");
    let req_body = r#"{"code":"print('line one')\nprint('line two')"}"#;
    let (status, _, body) = post_raw(&addr, "/execute", "application/json", req_body.as_bytes());
    assert_eq!(status, 200);

    let body_str = String::from_utf8(body).unwrap();
    let lines: Vec<&str> = body_str.lines().filter(|l| !l.is_empty()).collect();
    assert!(
        lines.len() >= 3,
        "expected at least 2 stdout lines and 1 counter line"
    );

    let line1 = JsonValue::parse(lines[0]).unwrap();
    assert_eq!(line1.get("type").unwrap().as_str().unwrap(), "stdout");
    assert_eq!(line1.get("text").unwrap().as_str().unwrap(), "line one\n");

    let line2 = JsonValue::parse(lines[1]).unwrap();
    assert_eq!(line2.get("type").unwrap().as_str().unwrap(), "stdout");
    assert_eq!(line2.get("text").unwrap().as_str().unwrap(), "line two\n");
}

#[test]
fn test_start_cmd_unset_serves_daemon_without_process_or_log() {
    // 1. CUBEPOD_START_CMD unset: serving the daemon does not create a start process and does not require the log file.
    let log_tmp_dir =
        std::env::temp_dir().join(format!("podd-test-unset-log-{}", std::process::id()));
    let _ = std::fs::create_dir_all(&log_tmp_dir);

    // Point CUBEPOD_START_CMD_LOG_DIR at temp directory, clear CUBEPOD_START_CMD
    std::env::set_var("CUBEPOD_START_CMD_LOG_DIR", log_tmp_dir.to_str().unwrap());
    std::env::remove_var("CUBEPOD_START_CMD");

    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let registry = ProcessRegistry::new();
    let config = DaemonConfig {
        workdir: "/tmp".to_string(),
    };
    let reg_clone = registry.clone();
    std::thread::spawn(move || serve(listener, config, reg_clone));

    let (status, _, body) = get(&addr.to_string(), "/health");
    assert_eq!(status, 200);
    let json = JsonValue::parse(std::str::from_utf8(&body).unwrap()).unwrap();
    assert_eq!(json.get("status").unwrap().as_str(), Some("ok"));
    assert_eq!(json.get("active_processes").unwrap().as_u64(), Some(0));

    // Assert that temp directory does not gain a log or status file
    assert!(!log_tmp_dir.join("start-cmd.log").exists());
    assert!(!log_tmp_dir.join("start-cmd.status").exists());

    std::env::remove_var("CUBEPOD_START_CMD_LOG_DIR");
    let _ = std::fs::remove_dir_all(&log_tmp_dir);
}

#[test]
fn test_start_cmd_set_runs_once_outputs_logs_and_second_poll_no_op() {
    use crate::startcmd::{
        StartCmdConfig, StartCmdRunner, StartCmdStatus, TemplateUserCredentials,
    };
    use std::fs;

    // 2. CUBEPOD_START_CMD set to a command that prints a marker and exits (requested uid equals the current euid):
    // the command runs once; stdout and stderr are in the log; the exit code is in start-cmd.status;
    // a second poll does not run it again.
    let current_euid = unsafe { libc::geteuid() };
    let temp_dir = std::env::temp_dir().join(format!("podd-test-marker-{}", std::process::id()));
    let _ = fs::create_dir_all(&temp_dir);

    let log_path = temp_dir.join("start-cmd.log");
    let status_path = temp_dir.join("start-cmd.status");

    let stdout_marker = "MARKER_OUT_88219";
    let stderr_marker = "MARKER_ERR_99120";

    let config = StartCmdConfig {
        command: Some(format!(
            "echo {} && echo {} >&2",
            stdout_marker, stderr_marker
        )),
        workdir: temp_dir.to_string_lossy().to_string(),
        credentials: TemplateUserCredentials {
            uid: Some(current_euid),
            gid: None,
            username: None,
        },
        log_path: log_path.clone(),
        status_path: status_path.clone(),
    };

    let runner = StartCmdRunner::new(config);

    // First poll starts the command
    let first_status = runner.poll();
    match first_status {
        StartCmdStatus::Running { .. } => {}
        other => panic!("expected Running status, got {:?}", other),
    }

    // Wait for the exit code to be written to start-cmd.status
    let mut exit_found = false;
    for _ in 0..100 {
        if status_path.exists() {
            if let Ok(content) = fs::read_to_string(&status_path) {
                if !content.trim().is_empty() {
                    assert_eq!(content.trim(), "0");
                    exit_found = true;
                    break;
                }
            }
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(exit_found, "start-cmd.status was not written");

    // Verify stdout and stderr in log
    let log_content = fs::read_to_string(&log_path).expect("failed to read start-cmd.log");
    assert!(
        log_content.contains(stdout_marker),
        "log missing stdout marker"
    );
    assert!(
        log_content.contains(stderr_marker),
        "log missing stderr marker"
    );
    let count_before = log_content.matches(stdout_marker).count();
    assert_eq!(count_before, 1);

    // Second poll does not run it again
    let second_status = runner.poll();
    assert_eq!(second_status, StartCmdStatus::AlreadyStarted);

    std::thread::sleep(Duration::from_millis(50));
    let log_content_after = fs::read_to_string(&log_path).expect("failed to read start-cmd.log");
    let count_after = log_content_after.matches(stdout_marker).count();
    assert_eq!(
        count_after, 1,
        "second poll must not execute start command again"
    );

    let _ = fs::remove_dir_all(&temp_dir);
}

#[test]
fn test_child_that_exits_is_reaped_no_zombie() {
    use crate::startcmd::{
        ensure_subreaper, install_zombie_reaper, is_pid_tracked_or_cached, reaped_orphan_count,
        StartCmdConfig, StartCmdRunner, StartCmdStatus, TemplateUserCredentials,
    };
    use std::fs;

    // 3. A child that exits is reaped. Spawn a command that forks an untracked grandchild
    // which exits immediately, and assert it is reaped without leaking into TRACKED_PIDS/REAPED_STATUSES.
    ensure_subreaper();
    install_zombie_reaper();

    let orphans_before = reaped_orphan_count();
    let temp_dir = std::env::temp_dir().join(format!("podd-test-reap-{}", std::process::id()));
    let _ = fs::create_dir_all(&temp_dir);

    let log_path = temp_dir.join("start-cmd.log");
    let status_path = temp_dir.join("start-cmd.status");

    // Command forks grandchild that exits immediately, prints grandchild PID to stdout, then child exits.
    let config = StartCmdConfig {
        command: Some("sh -c 'sleep 0.05; exit 0' & echo $! && exit 0".to_string()),
        workdir: temp_dir.to_string_lossy().to_string(),
        credentials: TemplateUserCredentials::default(),
        log_path: log_path.clone(),
        status_path: status_path.clone(),
    };

    let runner = StartCmdRunner::new(config);
    let child_pid = match runner.poll() {
        StartCmdStatus::Running { pid } => pid,
        other => panic!("expected Running status, got {:?}", other),
    };

    // Wait for the start command (direct child) to exit and write status
    let mut exit_code_found = false;
    for _ in 0..100 {
        if status_path.exists() {
            if let Ok(content) = fs::read_to_string(&status_path) {
                if !content.trim().is_empty() {
                    assert_eq!(content.trim(), "0");
                    exit_code_found = true;
                    break;
                }
            }
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(exit_code_found, "start-cmd.status was not written");

    // Parse grandchild PID from log
    let mut gpid: Option<u32> = None;
    for _ in 0..50 {
        if let Ok(content) = fs::read_to_string(&log_path) {
            for line in content.lines() {
                if let Ok(p) = line.trim().parse::<u32>() {
                    gpid = Some(p);
                    break;
                }
            }
            if gpid.is_some() {
                break;
            }
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    let gpid = gpid.expect("could not parse grandchild pid from start-cmd.log");
    assert_ne!(gpid, child_pid);

    // Wait for grandchild to exit and be reaped by the SIGCHLD thread (without calling reap_zombies directly).
    // Once reaped, kill(gpid, 0) fails with ESRCH and /proc/<gpid>/status disappears (not a zombie).
    let mut reaped = false;
    for _ in 0..150 {
        let rc = unsafe { libc::kill(gpid as libc::pid_t, 0) };
        if rc == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH) {
            reaped = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(20));
    }

    assert!(
        reaped,
        "grandchild pid {} was not reaped by the SIGCHLD reaper thread",
        gpid
    );
    assert!(
        reaped_orphan_count() > orphans_before,
        "expected untracked orphan reap counter to increment"
    );
    assert!(
        !is_pid_tracked_or_cached(gpid as i32),
        "untracked grandchild pid {} must not be cached in TRACKED_PIDS/REAPED_STATUSES",
        gpid
    );

    // Verify /proc/<gpid>/status if still accessible: must not be in Zombie state
    let proc_status_path = format!("/proc/{}/status", gpid);
    if let Ok(content) = fs::read_to_string(&proc_status_path) {
        for line in content.lines() {
            if line.starts_with("State:") {
                assert!(
                    !line.contains("Z (zombie)"),
                    "grandchild remains as zombie: {}",
                    line
                );
            }
        }
    }

    let _ = fs::remove_dir_all(&temp_dir);
}

#[test]
fn test_parsing_uid_zero_distinct_from_unset() {
    use crate::startcmd::{
        StartCmdConfig, StartCmdRunner, StartCmdStatus, TemplateUserCredentials,
    };
    use std::fs;

    // 4. Parsing: value 0 is a real uid request distinct from unset.
    // Assert the credential struct requests uid 0. Do not expect setuid(0) to succeed in this test.
    let unset_creds = TemplateUserCredentials::from_env_getter(|_| None);
    assert_eq!(unset_creds.uid, None);
    assert_eq!(unset_creds.requested_uid(), None);

    let zero_creds = TemplateUserCredentials::from_env_getter(|key| {
        if key == "CUBEPOD_RUN_AS_USER" {
            Some("0".to_string())
        } else {
            None
        }
    });
    // Assert the credential struct requests uid 0
    assert_eq!(zero_creds.uid, Some(0));
    assert_eq!(zero_creds.requested_uid(), Some(0));

    let temp_dir = std::env::temp_dir().join(format!("podd-test-uid0-{}", std::process::id()));
    let _ = fs::create_dir_all(&temp_dir);

    let log_path = temp_dir.join("start-cmd.log");
    let status_path = temp_dir.join("start-cmd.status");

    let config = StartCmdConfig {
        command: Some("echo should_never_run".to_string()),
        workdir: temp_dir.to_string_lossy().to_string(),
        credentials: zero_creds,
        log_path: log_path.clone(),
        status_path: status_path.clone(),
    };

    let runner = StartCmdRunner::new(config);
    let status = runner.poll();

    // Since test runs unprivileged (current euid != 0), setuid(0) fails
    match status {
        StartCmdStatus::CredentialSwitchFailed(msg) => {
            assert!(msg.contains("Operation not permitted") || msg.contains("setuid"));
        }
        other => panic!("expected CredentialSwitchFailed, got {:?}", other),
    }

    // Verify failure logged to start-cmd.log and start-cmd.status
    let log_content = fs::read_to_string(&log_path).expect("failed to read start-cmd.log");
    assert!(log_content.contains("Operation not permitted") || log_content.contains("setuid"));
    assert!(!log_content.contains("should_never_run"));

    let status_content = fs::read_to_string(&status_path).expect("failed to read start-cmd.status");
    assert_ne!(status_content.trim(), "0");

    let _ = fs::remove_dir_all(&temp_dir);
}

#[test]
fn test_username_nonexistent_does_not_spawn_and_writes_error_log() {
    use crate::startcmd::{
        StartCmdConfig, StartCmdRunner, StartCmdStatus, TemplateUserCredentials,
    };
    use std::fs;

    // 5. Username env with a name that does not exist does not spawn and writes an error log.
    let nonexistent = "nonexistent_cubepod_user_xyz_987654";
    let temp_dir = std::env::temp_dir().join(format!("podd-test-baduser-{}", std::process::id()));
    let _ = fs::create_dir_all(&temp_dir);

    let log_path = temp_dir.join("start-cmd.log");
    let status_path = temp_dir.join("start-cmd.status");

    let creds = TemplateUserCredentials {
        uid: None,
        gid: None,
        username: Some(nonexistent.to_string()),
    };

    let config = StartCmdConfig {
        command: Some("echo should_never_run".to_string()),
        workdir: temp_dir.to_string_lossy().to_string(),
        credentials: creds,
        log_path: log_path.clone(),
        status_path: status_path.clone(),
    };

    let runner = StartCmdRunner::new(config);
    let status = runner.poll();

    match status {
        StartCmdStatus::UsernameResolutionFailed(msg) => {
            assert!(msg.contains(nonexistent));
        }
        other => panic!("expected UsernameResolutionFailed, got {:?}", other),
    }

    // Verify error was written into start-cmd.log
    let log_content = fs::read_to_string(&log_path).expect("failed to read start-cmd.log");
    assert!(log_content.contains(nonexistent));
    assert!(
        log_content.contains("failed to resolve username")
            || log_content.contains("user not found")
    );
    assert!(!log_content.contains("should_never_run"));

    // Verify non-zero status written into start-cmd.status
    let status_content = fs::read_to_string(&status_path).expect("failed to read start-cmd.status");
    assert_ne!(status_content.trim(), "0");

    let _ = fs::remove_dir_all(&temp_dir);
}

#[test]
fn test_numeric_user_takes_precedence_over_username() {
    use crate::startcmd::{resolve_credentials, TemplateUserCredentials};

    let creds = TemplateUserCredentials {
        uid: Some(1000),
        gid: Some(1000),
        username: Some("nonexistent_cubepod_user_xyz".to_string()),
    };
    let resolved = resolve_credentials(&creds).unwrap();
    assert_eq!(resolved.uid, Some(1000));
    assert_eq!(resolved.gid, Some(1000));
}

#[test]
fn test_tracked_child_concurrent_gzip_and_sh_no_echild_no_leaks() {
    use crate::files::decompress_if_gzip;
    use crate::startcmd::{
        ensure_subreaper, exit_code_from_status, install_zombie_reaper, is_pid_tracked_or_cached,
        spawn_tracked,
    };
    use std::process::Stdio;
    use std::sync::{Arc, Mutex};

    ensure_subreaper();
    install_zombie_reaper();

    let payload = b"hello cubepod tracked child concurrency payload ".repeat(64);
    let mut gz_cmd = Command::new("gzip");
    gz_cmd
        .arg("-c")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut gz_child = spawn_tracked(&mut gz_cmd).expect("spawn gzip -c");
    {
        let mut stdin = gz_child.stdin().take().expect("gzip stdin");
        stdin.write_all(&payload).expect("write gzip stdin");
    }
    let gz_out = gz_child.wait_with_output().expect("wait_with_output gzip");
    assert!(gz_out.status.success());
    let gz_bytes = Arc::new(gz_out.stdout);

    let spawned_pids = Arc::new(Mutex::new(Vec::<i32>::new()));
    let mut handles = Vec::new();

    for _ in 0..10 {
        let gz_bytes = Arc::clone(&gz_bytes);
        let payload = payload.clone();
        let spawned_pids = Arc::clone(&spawned_pids);
        handles.push(std::thread::spawn(move || {
            for i in 0..6 {
                let decompressed = decompress_if_gzip(&gz_bytes).expect("decompress_if_gzip");
                assert_eq!(decompressed, payload);

                let mut cmd = Command::new("/bin/sh");
                cmd.arg("-c").arg("exit 7");
                let mut child = spawn_tracked(&mut cmd).expect("spawn sh exit 7");
                let pid = child.id() as i32;
                spawned_pids.lock().unwrap().push(pid);
                let status = child.wait().expect("wait must not fail with ECHILD");
                assert_eq!(exit_code_from_status(&status), 7);
                assert!(
                    !is_pid_tracked_or_cached(pid),
                    "pid {pid} leaked after wait"
                );

                let mut out_cmd = Command::new("/bin/sh");
                out_cmd
                    .arg("-c")
                    .arg("printf 'ok'")
                    .stdout(Stdio::piped())
                    .stderr(Stdio::piped());
                let mut out_child = spawn_tracked(&mut out_cmd).expect("spawn sh printf");
                let out_pid = out_child.id() as i32;
                spawned_pids.lock().unwrap().push(out_pid);
                let _ = out_child.try_wait().expect("try_wait must not ECHILD");
                let output = out_child.wait_with_output().expect("wait_with_output");
                assert!(output.status.success());
                assert_eq!(output.stdout, b"ok");
                assert!(
                    !is_pid_tracked_or_cached(out_pid),
                    "pid {out_pid} leaked after wait_with_output"
                );

                if i % 2 == 0 {
                    let mut drop_cmd = Command::new("/bin/sh");
                    drop_cmd.arg("-c").arg("exit 0");
                    let drop_child = spawn_tracked(&mut drop_cmd).expect("spawn drop child");
                    let drop_pid = drop_child.id() as i32;
                    spawned_pids.lock().unwrap().push(drop_pid);
                    drop(drop_child);
                    assert!(
                        !is_pid_tracked_or_cached(drop_pid),
                        "pid {drop_pid} leaked after drop"
                    );
                }
            }
        }));
    }

    for h in handles {
        h.join().expect("worker thread panicked");
    }

    for pid in spawned_pids.lock().unwrap().iter() {
        assert!(
            !is_pid_tracked_or_cached(*pid),
            "pid {} still present in TRACKED_PIDS/REAPED_STATUSES",
            pid
        );
    }
}

#[test]
fn test_resolve_default_credentials_cases() {
    use crate::startcmd::{
        configure_command_credentials, lookup_passwd_by_name, lookup_passwd_by_uid,
        resolve_default_credentials_from_getter,
    };
    use std::process::Stdio;

    // 1. CUBEPOD_RUN_AS_USER=0 -> (0, 0) regardless of euid
    let get_root = |k: &str| match k {
        "CUBEPOD_RUN_AS_USER" => Some("0".to_string()),
        _ => None,
    };
    assert_eq!(
        resolve_default_credentials_from_getter(get_root, 0, 0).unwrap(),
        Some((0, 0))
    );
    assert_eq!(
        resolve_default_credentials_from_getter(get_root, 1000, 1000).unwrap(),
        Some((0, 0))
    );

    // 2. CUBEPOD_RUN_AS_USER=1001, CUBEPOD_RUN_AS_GROUP=1002 -> (1001, 1002)
    let get_1001 = |k: &str| match k {
        "CUBEPOD_RUN_AS_USER" => Some("1001".to_string()),
        "CUBEPOD_RUN_AS_GROUP" => Some("1002".to_string()),
        _ => None,
    };
    assert_eq!(
        resolve_default_credentials_from_getter(get_1001, 0, 0).unwrap(),
        Some((1001, 1002))
    );

    // 3. CUBEPOD_RUN_AS_USERNAME
    let get_root_name = |k: &str| match k {
        "CUBEPOD_RUN_AS_USERNAME" => Some("root".to_string()),
        _ => None,
    };
    assert_eq!(
        resolve_default_credentials_from_getter(get_root_name, 0, 0).unwrap(),
        Some((0, 0))
    );

    let get_uid_gid_str = |k: &str| match k {
        "CUBEPOD_RUN_AS_USERNAME" => Some("1001:1002".to_string()),
        _ => None,
    };
    assert_eq!(
        resolve_default_credentials_from_getter(get_uid_gid_str, 0, 0).unwrap(),
        Some((1001, 1002))
    );

    let get_bad_name = |k: &str| match k {
        "CUBEPOD_RUN_AS_USERNAME" => Some("nonexistent_cubepod_user_xyz_123".to_string()),
        _ => None,
    };
    assert!(resolve_default_credentials_from_getter(get_bad_name, 0, 0).is_err());

    // 4. All CUBEPOD_RUN_AS_* unset:
    //    euid == 0 -> Some((1000, 1000)); euid != 0 -> None.
    let get_unset = |_: &str| None;
    assert_eq!(
        resolve_default_credentials_from_getter(get_unset, 0, 0).unwrap(),
        Some((1000, 1000))
    );
    assert_eq!(
        resolve_default_credentials_from_getter(get_unset, 1000, 1000).unwrap(),
        None
    );

    // 5. Reentrant getpwnam_r / getpwuid_r sanity check
    let pw_by_name = lookup_passwd_by_name("root").unwrap().expect("root passwd");
    assert_eq!(pw_by_name.uid, 0);
    assert_eq!(pw_by_name.gid, 0);
    assert_eq!(pw_by_name.username, "root");
    assert!(!pw_by_name.home_dir.is_empty());

    let pw_by_uid = lookup_passwd_by_uid(0).unwrap().expect("uid 0 passwd");
    assert_eq!(pw_by_uid.username, "root");

    // 6. configure_command_credentials sets HOME/USER/LOGNAME and allows caller envs override
    let current_euid = unsafe { libc::geteuid() };
    let mut cmd = Command::new("/bin/sh");
    cmd.arg("-c")
        .arg(r#"printf '%s|%s|%s' "$HOME" "$USER" "$LOGNAME""#)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let resolved = configure_command_credentials(&mut cmd, Some(current_euid), None, None).unwrap();
    let expected_home = resolved.home_dir.unwrap();
    let expected_user = resolved.username.unwrap();
    let out = crate::startcmd::spawn_tracked(&mut cmd)
        .expect("spawn sh")
        .wait_with_output()
        .expect("wait sh");
    assert!(out.status.success());
    assert_eq!(
        String::from_utf8_lossy(&out.stdout),
        format!("{expected_home}|{expected_user}|{expected_user}")
    );

    let mut override_cmd = Command::new("/bin/sh");
    override_cmd
        .arg("-c")
        .arg(r#"printf '%s' "$HOME""#)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    configure_command_credentials(&mut override_cmd, Some(current_euid), None, None).unwrap();
    override_cmd.env("HOME", "/custom/override");
    let out2 = crate::startcmd::spawn_tracked(&mut override_cmd)
        .expect("spawn sh override")
        .wait_with_output()
        .expect("wait sh override");
    assert!(out2.status.success());
    assert_eq!(String::from_utf8_lossy(&out2.stdout), "/custom/override");
}

#[test]
fn test_run_request_serde() {
    use crate::commands::RunRequest;

    let empty = RunRequest::from_json_str("{}").unwrap();
    assert_eq!(empty.uid, None);
    assert_eq!(empty.gid, None);
    assert_eq!(empty.user, None);

    let explicit_zero = RunRequest::from_json_str(r#"{"uid":0,"gid":0,"command":"id"}"#).unwrap();
    assert_eq!(explicit_zero.uid, Some(0));
    assert_eq!(explicit_zero.gid, Some(0));
    assert_eq!(explicit_zero.command.as_deref(), Some("id"));

    let null_fields = RunRequest::from_json_str(r#"{"uid":null,"gid":null,"user":"   "}"#).unwrap();
    assert_eq!(null_fields.uid, None);
    assert_eq!(null_fields.gid, None);
    assert_eq!(null_fields.user, None);

    let custom = RunRequest::from_json_str(r#"{"uid":1001,"gid":1002,"user":"cubeplex"}"#).unwrap();
    assert_eq!(custom.uid, Some(1001));
    assert_eq!(custom.gid, Some(1002));
    assert_eq!(custom.user.as_deref(), Some("cubeplex"));
}

fn parse_ndjson_lines(body: &[u8]) -> Vec<JsonValue> {
    std::str::from_utf8(body)
        .unwrap()
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| JsonValue::parse(l).unwrap_or_else(|e| panic!("bad ndjson line {l:?}: {e}")))
        .collect()
}

#[test]
fn test_interpreter_python_stateful_and_aliases() {
    let (addr, _) = start_test_server("/tmp");
    let (st, _, ctx_body) = post_json(&addr, "/contexts", r#"{"language":"py","cwd":"/tmp"}"#);
    assert_eq!(st, 200);
    let ctx_json = JsonValue::parse(std::str::from_utf8(&ctx_body).unwrap()).unwrap();
    assert_eq!(ctx_json.get("language").unwrap().as_str(), Some("python"));
    let ctx_id = ctx_json.get("id").unwrap().as_str().unwrap().to_string();

    let run1 = format!(
        r#"{{"context_id":"{ctx_id}","code":"x = 41\nprint('py-out')\nimport sys\nsys.stderr.write('py-err\\n')"}}"#
    );
    let (s1, _, b1) = post_raw(&addr, "/execute", "application/json", run1.as_bytes());
    assert_eq!(s1, 200);
    let msgs1 = parse_ndjson_lines(&b1);
    assert!(msgs1.iter().any(|m| {
        m.get("type").and_then(|v| v.as_str()) == Some("stdout")
            && m.get("text").and_then(|v| v.as_str()) == Some("py-out\n")
    }));
    assert!(msgs1.iter().any(|m| {
        m.get("type").and_then(|v| v.as_str()) == Some("stderr")
            && m.get("text").and_then(|v| v.as_str()) == Some("py-err\n")
    }));

    let run2 = format!(r#"{{"context_id":"{ctx_id}","code":"x + 1"}}"#);
    let (s2, _, b2) = post_raw(&addr, "/execute", "application/json", run2.as_bytes());
    assert_eq!(s2, 200);
    let msgs2 = parse_ndjson_lines(&b2);
    assert!(msgs2.iter().any(|m| {
        m.get("type").and_then(|v| v.as_str()) == Some("result")
            && m.get("text").and_then(|v| v.as_str()) == Some("42")
    }));
}

#[test]
fn test_interpreter_bash_stateful_pwd_vars_and_exit_error() {
    let (addr, _) = start_test_server("/tmp");
    let (st, _, ctx_body) = post_json(&addr, "/contexts", r#"{"language":"bash","cwd":"/tmp"}"#);
    assert_eq!(st, 200);
    let ctx_json = JsonValue::parse(std::str::from_utf8(&ctx_body).unwrap()).unwrap();
    assert_eq!(ctx_json.get("language").unwrap().as_str(), Some("bash"));
    let ctx_id = ctx_json.get("id").unwrap().as_str().unwrap().to_string();

    let run1 = format!(
        r#"{{"context_id":"{ctx_id}","code":"cd /tmp\nx=41\nexport EXP_VAR=cubepod\necho bash-out\necho bash-err >&2"}}"#
    );
    let (s1, _, b1) = post_raw(&addr, "/execute", "application/json", run1.as_bytes());
    assert_eq!(s1, 200);
    let msgs1 = parse_ndjson_lines(&b1);
    assert!(
        msgs1.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("stdout")
                && m.get("text").and_then(|v| v.as_str()) == Some("bash-out\n")
        }),
        "missing bash stdout: {msgs1:?}"
    );
    assert!(
        msgs1.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("stderr")
                && m.get("text").and_then(|v| v.as_str()) == Some("bash-err\n")
        }),
        "missing bash stderr: {msgs1:?}"
    );

    let run2 = format!(r#"{{"context_id":"{ctx_id}","code":"pwd\necho \"$((x + 1))-$EXP_VAR\""}}"#);
    let (s2, _, b2) = post_raw(&addr, "/execute", "application/json", run2.as_bytes());
    assert_eq!(s2, 200);
    let msgs2 = parse_ndjson_lines(&b2);
    let stdout_all: String = msgs2
        .iter()
        .filter(|m| m.get("type").and_then(|v| v.as_str()) == Some("stdout"))
        .filter_map(|m| m.get("text").and_then(|v| v.as_str()))
        .collect();
    assert!(stdout_all.contains("/tmp\n"), "pwd lost: {stdout_all:?}");
    assert!(
        stdout_all.contains("42-cubepod\n"),
        "bash vars lost: {stdout_all:?}"
    );

    let run3 = format!(r#"{{"context_id":"{ctx_id}","code":"false"}}"#);
    let (s3, _, b3) = post_raw(&addr, "/execute", "application/json", run3.as_bytes());
    assert_eq!(s3, 200);
    let msgs3 = parse_ndjson_lines(&b3);
    assert!(
        msgs3.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("error")
                && m.get("name").and_then(|v| v.as_str()) == Some("ProcessExitError")
        }),
        "expected ProcessExitError on non-zero exit: {msgs3:?}"
    );
}

#[test]
fn test_interpreter_javascript_stateful_and_top_level_await() {
    use crate::interpreter::find_runtime_binary;
    if find_runtime_binary(
        Some("CUBEPOD_NODE_BIN"),
        &["node", "/usr/bin/node", "/usr/local/bin/node"],
        "node",
    )
    .is_err()
    {
        eprintln!("skipping javascript test: node not installed on host");
        return;
    }

    let (addr, _) = start_test_server("/tmp");
    let (st, _, ctx_body) = post_json(&addr, "/contexts", r#"{"language":"js","cwd":"/tmp"}"#);
    assert_eq!(st, 200);
    let ctx_json = JsonValue::parse(std::str::from_utf8(&ctx_body).unwrap()).unwrap();
    assert_eq!(
        ctx_json.get("language").unwrap().as_str(),
        Some("javascript")
    );
    let ctx_id = ctx_json.get("id").unwrap().as_str().unwrap().to_string();

    let run1 = format!(
        r#"{{"context_id":"{ctx_id}","code":"const x = 41;\nconsole.log('js-out');\nconsole.error('js-err');\nprocess.stdout.write('raw-out');"}}"#
    );
    let (s1, _, b1) = post_raw(&addr, "/execute", "application/json", run1.as_bytes());
    assert_eq!(s1, 200);
    let msgs1 = parse_ndjson_lines(&b1);
    let out1: String = msgs1
        .iter()
        .filter(|m| m.get("type").and_then(|v| v.as_str()) == Some("stdout"))
        .filter_map(|m| m.get("text").and_then(|v| v.as_str()))
        .collect();
    let err1: String = msgs1
        .iter()
        .filter(|m| m.get("type").and_then(|v| v.as_str()) == Some("stderr"))
        .filter_map(|m| m.get("text").and_then(|v| v.as_str()))
        .collect();
    assert!(out1.contains("js-out\n"), "out1={out1:?}");
    assert!(out1.contains("raw-out"), "out1={out1:?}");
    assert!(err1.contains("js-err\n"), "err1={err1:?}");

    let run2 = format!(r#"{{"context_id":"{ctx_id}","code":"x + 1"}}"#);
    let (s2, _, b2) = post_raw(&addr, "/execute", "application/json", run2.as_bytes());
    assert_eq!(s2, 200);
    let msgs2 = parse_ndjson_lines(&b2);
    assert!(
        msgs2.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("result")
                && m.get("text").and_then(|v| v.as_str()) == Some("42")
        }),
        "expected result 42: {msgs2:?}"
    );

    let run3 = format!(
        r#"{{"context_id":"{ctx_id}","code":"const z = await Promise.resolve(10);\nz + 32"}}"#
    );
    let (s3, _, b3) = post_raw(&addr, "/execute", "application/json", run3.as_bytes());
    assert_eq!(s3, 200);
    let msgs3 = parse_ndjson_lines(&b3);
    assert!(
        msgs3.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("result")
                && m.get("text").and_then(|v| v.as_str()) == Some("42")
        }),
        "expected async result 42: {msgs3:?}"
    );

    let run4 = format!(r#"{{"context_id":"{ctx_id}","code":"throw new Error('boom-js')"}}"#);
    let (s4, _, b4) = post_raw(&addr, "/execute", "application/json", run4.as_bytes());
    assert_eq!(s4, 200);
    let msgs4 = parse_ndjson_lines(&b4);
    assert!(
        msgs4.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("error")
                && m.get("value").and_then(|v| v.as_str()) == Some("boom-js")
                && !m
                    .get("traceback")
                    .and_then(|v| v.as_str())
                    .unwrap_or("")
                    .is_empty()
        }),
        "expected error with stack trace: {msgs4:?}"
    );
}

#[test]
fn test_interpreter_typescript_stateful() {
    use crate::interpreter::find_runtime_binary;
    if find_runtime_binary(
        Some("CUBEPOD_NODE_BIN"),
        &["node", "/usr/bin/node", "/usr/local/bin/node"],
        "node",
    )
    .is_err()
    {
        eprintln!("skipping typescript test: node not installed on host");
        return;
    }

    let (addr, _) = start_test_server("/tmp");
    let (st, _, ctx_body) = post_json(&addr, "/contexts", r#"{"language":"ts","cwd":"/tmp"}"#);
    assert_eq!(st, 200);
    let ctx_json = JsonValue::parse(std::str::from_utf8(&ctx_body).unwrap()).unwrap();
    assert_eq!(
        ctx_json.get("language").unwrap().as_str(),
        Some("typescript")
    );
    let ctx_id = ctx_json.get("id").unwrap().as_str().unwrap().to_string();

    let run1 = format!(
        r#"{{"context_id":"{ctx_id}","code":"interface Box {{ val: number }}\nconst box: Box = {{ val: 41 }};\nconsole.log('ts-ready');"}}"#
    );
    let (s1, _, b1) = post_raw(&addr, "/execute", "application/json", run1.as_bytes());
    assert_eq!(s1, 200);
    let msgs1 = parse_ndjson_lines(&b1);
    assert!(msgs1.iter().any(|m| {
        m.get("type").and_then(|v| v.as_str()) == Some("stdout")
            && m.get("text").and_then(|v| v.as_str()) == Some("ts-ready\n")
    }));

    let run2 = format!(
        r#"{{"context_id":"{ctx_id}","code":"const add = (a: number, b: number): number => a + b;\nadd(box.val, 1)"}}"#
    );
    let (s2, _, b2) = post_raw(&addr, "/execute", "application/json", run2.as_bytes());
    assert_eq!(s2, 200);
    let msgs2 = parse_ndjson_lines(&b2);
    assert!(
        msgs2.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("result")
                && m.get("text").and_then(|v| v.as_str()) == Some("42")
        }),
        "expected ts result 42: {msgs2:?}"
    );
}

#[test]
fn test_interpreter_java_stateful() {
    use crate::interpreter::find_runtime_binary;
    let has_java = find_runtime_binary(
        Some("CUBEPOD_JAVA_BIN"),
        &["java", "/usr/bin/java", "/usr/local/bin/java"],
        "java",
    )
    .is_ok();
    let has_jshell = find_runtime_binary(
        Some("CUBEPOD_JSHELL_BIN"),
        &["jshell", "/usr/bin/jshell", "/usr/local/bin/jshell"],
        "jshell",
    )
    .is_ok();
    if !has_java || !has_jshell {
        eprintln!("skipping java stateful test: java/jshell not installed on host");
        return;
    }

    let (addr, _) = start_test_server("/tmp");
    let (st, _, ctx_body) = post_json(&addr, "/contexts", r#"{"language":"java","cwd":"/tmp"}"#);
    assert_eq!(st, 200);
    let ctx_json = JsonValue::parse(std::str::from_utf8(&ctx_body).unwrap()).unwrap();
    let ctx_id = ctx_json.get("id").unwrap().as_str().unwrap().to_string();

    let run1 = format!(
        r#"{{"context_id":"{ctx_id}","code":"int x = 41;\nSystem.out.println(\"java-ok\");"}}"#
    );
    let (s1, _, b1) = post_raw(&addr, "/execute", "application/json", run1.as_bytes());
    assert_eq!(s1, 200);
    let msgs1 = parse_ndjson_lines(&b1);
    assert!(msgs1.iter().any(|m| {
        m.get("type").and_then(|v| v.as_str()) == Some("stdout")
            && m.get("text").and_then(|v| v.as_str()) == Some("java-ok\n")
    }));

    let run2 = format!(r#"{{"context_id":"{ctx_id}","code":"x + 1"}}"#);
    let (s2, _, b2) = post_raw(&addr, "/execute", "application/json", run2.as_bytes());
    assert_eq!(s2, 200);
    let msgs2 = parse_ndjson_lines(&b2);
    assert!(
        msgs2.iter().any(|m| {
            m.get("type").and_then(|v| v.as_str()) == Some("result")
                && m.get("text").and_then(|v| v.as_str()) == Some("42")
        }),
        "expected java result 42: {msgs2:?}"
    );
}

#[test]
fn test_interpreter_unsupported_language_and_missing_runtime() {
    use crate::interpreter::{find_runtime_binary, WorkerSpawnError};

    match find_runtime_binary(None, &["__cubepod_nonexistent_runtime_xyz__"], "fake_rt") {
        Err(WorkerSpawnError::NotInstalled { bin }) => assert_eq!(bin, "fake_rt"),
        other => panic!("expected NotInstalled error, got {other:?}"),
    }

    let (addr, _) = start_test_server("/tmp");

    let (s1, _, b1) = post_json(&addr, "/contexts", r#"{"language":"brainfuck"}"#);
    assert_eq!(s1, 400);
    let j1 = JsonValue::parse(std::str::from_utf8(&b1).unwrap()).unwrap();
    assert_eq!(
        j1.get("name").and_then(|v| v.as_str()),
        Some("UnsupportedLanguageError")
    );

    let (s2, _, b2) = post_json(&addr, "/execute", r#"{"code":"1+1","language":"cobol"}"#);
    assert_eq!(s2, 400);
    let j2 = JsonValue::parse(std::str::from_utf8(&b2).unwrap()).unwrap();
    assert_eq!(
        j2.get("name").and_then(|v| v.as_str()),
        Some("UnsupportedLanguageError")
    );

    // Verify end-to-end KernelNotInstalledError for R when R/Rscript is absent on host.
    if find_runtime_binary(
        Some("CUBEPOD_R_BIN"),
        &[
            "R",
            "/usr/bin/R",
            "/usr/local/bin/R",
            "Rscript",
            "/usr/bin/Rscript",
            "/usr/local/bin/Rscript",
        ],
        "R",
    )
    .is_err()
    {
        let (sr, _, br) = post_json(&addr, "/execute", r#"{"code":"41 + 1","language":"r"}"#);
        assert_eq!(sr, 200);
        let msgs = parse_ndjson_lines(&br);
        assert!(
            msgs.iter().any(|m| {
                m.get("type").and_then(|v| v.as_str()) == Some("error")
                    && m.get("name").and_then(|v| v.as_str()) == Some("KernelNotInstalledError")
                    && m.get("value")
                        .and_then(|v| v.as_str())
                        .unwrap_or("")
                        .contains("not installed in this sandbox image")
            }),
            "expected KernelNotInstalledError for missing R: {msgs:?}"
        );
    }
}

#[test]
fn test_lifecycle_quiesce_thaw_and_watchdog() {
    use crate::lifecycle::{FreezeOps, QuiesceState};
    use std::any::Any;
    use std::path::Path;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;

    // 1. HTTP endpoints (unprivileged host gracefully reports synced:true, frozen:false)
    let (addr, _) = start_test_server("/tmp");

    let (q1_st, _, q1_body) = post_json(&addr, "/v1/lifecycle/quiesce", "");
    assert_eq!(q1_st, 200);
    let q1 = JsonValue::parse(std::str::from_utf8(&q1_body).unwrap()).unwrap();
    assert_eq!(q1.get("status").and_then(|v| v.as_str()), Some("quiesced"));
    assert_eq!(q1.get("synced").and_then(|v| v.as_bool()), Some(true));
    assert_eq!(q1.get("maxFreezeMs").and_then(|v| v.as_u64()), Some(1500));

    let (q2_st, _, q2_body) = post_json(&addr, "/v1/lifecycle/quiesce", r#"{"maxFreezeMs": 10}"#);
    assert_eq!(q2_st, 200);
    let q2 = JsonValue::parse(std::str::from_utf8(&q2_body).unwrap()).unwrap();
    assert_eq!(q2.get("maxFreezeMs").and_then(|v| v.as_u64()), Some(100));

    let (q3_st, _, q3_body) =
        post_json(&addr, "/v1/lifecycle/quiesce", r#"{"maxFreezeMs": 99999}"#);
    assert_eq!(q3_st, 200);
    let q3 = JsonValue::parse(std::str::from_utf8(&q3_body).unwrap()).unwrap();
    assert_eq!(q3.get("maxFreezeMs").and_then(|v| v.as_u64()), Some(5000));

    // Idempotent thaw
    let (t1_st, _, t1_body) = http_request(&addr, "POST", "/v1/lifecycle/thaw", &[], &[]);
    assert_eq!(t1_st, 200);
    let t1 = JsonValue::parse(std::str::from_utf8(&t1_body).unwrap()).unwrap();
    assert_eq!(t1.get("status").and_then(|v| v.as_str()), Some("thawed"));

    let (t2_st, _, _) = http_request(&addr, "POST", "/v1/lifecycle/thaw", &[], &[]);
    assert_eq!(t2_st, 200);

    // 2. Bounded monotonic watchdog auto-thaw test with MockFreezeOps
    struct MockFreezeOps {
        syncs: AtomicUsize,
        freezes: AtomicUsize,
        thaws: AtomicUsize,
    }
    impl FreezeOps for MockFreezeOps {
        fn sync_all(&self) {
            self.syncs.fetch_add(1, Ordering::SeqCst);
        }
        fn freeze_paths(&self, _paths: &[&Path]) -> Vec<Box<dyn Any + Send>> {
            self.freezes.fetch_add(1, Ordering::SeqCst);
            vec![Box::new(42u64)]
        }
        fn thaw_handle(&self, _handle: Box<dyn Any + Send>) {
            self.thaws.fetch_add(1, Ordering::SeqCst);
        }
    }

    let ops = Arc::new(MockFreezeOps {
        syncs: AtomicUsize::new(0),
        freezes: AtomicUsize::new(0),
        thaws: AtomicUsize::new(0),
    });
    let state = QuiesceState::with_ops(ops.clone());

    let out = state.quiesce("/tmp", 100);
    assert!(out.synced);
    assert!(out.frozen);
    assert_eq!(out.max_freeze_ms, 100);
    assert!(state.is_frozen());
    assert_eq!(ops.thaws.load(Ordering::SeqCst), 0);

    // Wait for watchdog thread to auto-thaw
    let mut thawed = false;
    for _ in 0..40 {
        if !state.is_frozen() {
            thawed = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(thawed, "watchdog must auto-thaw frozen handles");
    assert_eq!(ops.thaws.load(Ordering::SeqCst), 1);
    assert_eq!(state.auto_thawed_count(), 1);

    // Subsequent explicit thaw is idempotent and does not double-thaw
    assert_eq!(state.thaw(), 0);
    assert_eq!(ops.thaws.load(Ordering::SeqCst), 1);

    // Explicit thaw before watchdog expires cancels auto-thaw
    let out2 = state.quiesce("/tmp", 300);
    assert!(out2.frozen);
    assert_eq!(state.thaw(), 1);
    assert!(!state.is_frozen());
    assert_eq!(ops.thaws.load(Ordering::SeqCst), 2);
    std::thread::sleep(Duration::from_millis(350));
    assert_eq!(ops.thaws.load(Ordering::SeqCst), 2);
    assert_eq!(state.auto_thawed_count(), 1);
}
