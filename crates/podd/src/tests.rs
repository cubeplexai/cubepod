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
