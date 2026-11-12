//! HTTP Server & Request Handling for the podd daemon.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::net::TcpStream;

pub fn find_subsequence(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

pub struct HttpRequest {
    pub method: String,
    pub target: String,
    pub headers: HashMap<String, String>,
    pub body: Vec<u8>,
}

pub fn read_chunked_body(stream: &mut TcpStream, initial: &[u8]) -> Result<Vec<u8>, String> {
    let mut body = Vec::new();
    let mut buf = initial.to_vec();
    let mut tmp = [0u8; 1024];

    loop {
        // Read until we find line delimiter for chunk size
        let line_end = loop {
            if let Some(pos) = find_subsequence(&buf, b"\r\n") {
                break (pos, pos + 2);
            } else if let Some(pos) = find_subsequence(&buf, b"\n") {
                break (pos, pos + 1);
            }
            let n = stream
                .read(&mut tmp)
                .map_err(|e| format!("chunk size read error: {e}"))?;
            if n == 0 {
                return Err("unexpected EOF reading chunk size".to_string());
            }
            buf.extend_from_slice(&tmp[..n]);
        };

        let chunk_header = String::from_utf8_lossy(&buf[..line_end.0]);
        let hex_str = chunk_header.split(';').next().unwrap_or("").trim();
        let chunk_size = usize::from_str_radix(hex_str, 16)
            .map_err(|e| format!("invalid chunk size '{hex_str}': {e}"))?;

        buf.drain(..line_end.1);

        if chunk_size == 0 {
            // End of chunks. Consume trailing CRLF if present.
            if buf.starts_with(b"\r\n") {
                buf.drain(..2);
            } else if buf.starts_with(b"\n") {
                buf.drain(..1);
            }
            break;
        }

        // Read chunk data of length chunk_size
        while buf.len() < chunk_size {
            let n = stream
                .read(&mut tmp)
                .map_err(|e| format!("chunk data read error: {e}"))?;
            if n == 0 {
                return Err("unexpected EOF reading chunk data".to_string());
            }
            buf.extend_from_slice(&tmp[..n]);
        }

        body.extend_from_slice(&buf[..chunk_size]);
        buf.drain(..chunk_size);

        // Consume trailing CRLF after chunk data
        while buf.is_empty() {
            let n = stream
                .read(&mut tmp)
                .map_err(|e| format!("chunk crlf read error: {e}"))?;
            if n == 0 {
                break;
            }
            buf.extend_from_slice(&tmp[..n]);
        }
        if buf.starts_with(b"\r\n") {
            buf.drain(..2);
        } else if buf.starts_with(b"\n") {
            buf.drain(..1);
        }
    }

    Ok(body)
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

    let is_chunked = headers
        .get("transfer-encoding")
        .map(|v| v.to_ascii_lowercase().contains("chunked"))
        .unwrap_or(false);

    let initial_body = &header_buf[header_end.1..];
    let body = if is_chunked {
        read_chunked_body(stream, initial_body)?
    } else {
        let mut body = Vec::with_capacity(content_length);
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
        body
    };

    Ok(HttpRequest {
        method,
        target,
        headers,
        body,
    })
}

pub fn write_response(stream: &mut TcpStream, status: &str, content_type: &str, body: &[u8]) {
    let header = format!(
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    let _ = stream.write_all(header.as_bytes());
    let _ = stream.write_all(body);
    let _ = stream.flush();
}

pub fn write_json(stream: &mut TcpStream, status: &str, json: &str) {
    write_response(stream, status, "application/json", json.as_bytes());
}
