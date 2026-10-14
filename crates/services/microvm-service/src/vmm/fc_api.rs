// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Firecracker's API: JSON over HTTP/1.1 on a Unix socket. A request is a
//! few hundred bytes and so is the answer: 204, 200 with a JSON body, or an
//! error with `{"fault_message": ...}`. One connection per request.

use super::VmmError;
use serde_json::Value;
use std::path::Path;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

/// Firecracker answers within milliseconds; a request that takes longer is
/// stuck.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
/// Larger than any answer Firecracker gives.
const MAX_RESPONSE: usize = 64 * 1024;

/// Sends one request and returns the JSON body of a successful answer, if
/// it has one.
pub async fn request(
    socket: &Path,
    method: &str,
    path: &str,
    body: Option<&Value>,
) -> Result<Option<Value>, VmmError> {
    request_within(socket, method, path, body, REQUEST_TIMEOUT).await
}

/// `request` for one that takes longer, such as writing a snapshot.
pub async fn request_within(
    socket: &Path,
    method: &str,
    path: &str,
    body: Option<&Value>,
    timeout: Duration,
) -> Result<Option<Value>, VmmError> {
    tokio::time::timeout(timeout, exchange(socket, method, path, body))
        .await
        .map_err(|_| {
            VmmError::ApiOperationFailed(format!(
                "{method} {path}: no answer within {}s",
                timeout.as_secs()
            ))
        })?
}

async fn exchange(
    socket: &Path,
    method: &str,
    path: &str,
    body: Option<&Value>,
) -> Result<Option<Value>, VmmError> {
    let mut stream = UnixStream::connect(socket)
        .await
        .map_err(|e| VmmError::ApiConnectionFailed(format!("{}: {e}", socket.display())))?;
    let body = body.map(Value::to_string).unwrap_or_default();
    let request = format!(
        "{method} {path} HTTP/1.1\r\nHost: localhost\r\nAccept: application/json\r\n\
         Content-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
        body.len()
    );
    let io_error =
        |e: std::io::Error| VmmError::ApiConnectionFailed(format!("{method} {path}: {e}"));
    stream
        .write_all(request.as_bytes())
        .await
        .map_err(io_error)?;

    // Firecracker keeps the connection open: the answer ends where its
    // Content-Length says.
    let mut buf = Vec::with_capacity(1024);
    let mut chunk = [0u8; 4096];
    let (status, body) = loop {
        let n = stream.read(&mut chunk).await.map_err(io_error)?;
        if n == 0 {
            return Err(VmmError::ApiConnectionFailed(format!(
                "{method} {path}: connection closed before the answer was complete"
            )));
        }
        buf.extend_from_slice(&chunk[..n]);
        if let Some(head) = parse_head(&buf)? {
            let end = head.len + head.content_length;
            if buf.len() >= end {
                break (head.status, buf[head.len..end].to_vec());
            }
        }
        if buf.len() > MAX_RESPONSE {
            return Err(VmmError::ApiOperationFailed(format!(
                "{method} {path}: answer larger than {MAX_RESPONSE} bytes"
            )));
        }
    };

    let json = if body.is_empty() {
        None
    } else {
        Some(serde_json::from_slice::<Value>(&body).map_err(|e| {
            VmmError::ApiOperationFailed(format!("{method} {path}: answer is not JSON: {e}"))
        })?)
    };
    if (200..300).contains(&status) {
        return Ok(json);
    }
    let fault = json
        .as_ref()
        .and_then(|j| j.get("fault_message"))
        .and_then(Value::as_str)
        .unwrap_or("no fault message");
    Err(VmmError::ApiOperationFailed(format!(
        "{method} {path}: {status}: {fault}"
    )))
}

#[derive(Debug, PartialEq)]
struct Head {
    /// Bytes up to and including the blank line.
    len: usize,
    status: u16,
    content_length: usize,
}

/// Parses status line and headers once they are complete.
fn parse_head(buf: &[u8]) -> Result<Option<Head>, VmmError> {
    let Some(pos) = buf.windows(4).position(|w| w == b"\r\n\r\n") else {
        return Ok(None);
    };
    let bad = || VmmError::ApiOperationFailed("malformed answer from Firecracker".to_string());
    let text = std::str::from_utf8(&buf[..pos]).map_err(|_| bad())?;
    let mut lines = text.split("\r\n");
    let status = lines
        .next()
        .and_then(|line| line.split(' ').nth(1))
        .and_then(|code| code.parse().ok())
        .ok_or_else(bad)?;
    let mut content_length = 0;
    for line in lines {
        if let Some((name, value)) = line.split_once(':')
            && name.trim().eq_ignore_ascii_case("content-length")
        {
            content_length = value.trim().parse().map_err(|_| bad())?;
        }
    }
    Ok(Some(Head {
        len: pos + 4,
        status,
        content_length,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::net::UnixListener;

    #[test]
    fn heads_are_parsed_once_complete() {
        assert_eq!(parse_head(b"HTTP/1.1 204 No Content\r\n").unwrap(), None);
        assert_eq!(
            parse_head(b"HTTP/1.1 204 No Content\r\nServer: Firecracker API\r\n\r\n").unwrap(),
            Some(Head {
                len: 52,
                status: 204,
                content_length: 0
            })
        );
        let head = parse_head(b"HTTP/1.1 400 Bad\r\ncontent-length: 7\r\n\r\n{}").unwrap();
        assert_eq!(head.unwrap().content_length, 7);
        assert!(parse_head(b"garbage\r\n\r\n").is_err());
    }

    /// A Firecracker stand-in that answers each connection with `status`
    /// and `body`, split in two writes, and keeps the connection open.
    async fn fake_vmm(status: &str, body: &str) -> (tempfile::TempDir, std::path::PathBuf) {
        let answer = format!(
            "HTTP/1.1 {status}\r\nServer: Firecracker API\r\nContent-Length: {}\r\n\r\n{body}",
            body.len()
        );
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("fc.sock");
        let listener = UnixListener::bind(&path).unwrap();
        tokio::spawn(async move {
            while let Ok((mut conn, _)) = listener.accept().await {
                let mut buf = [0u8; 1024];
                let _ = conn.read(&mut buf).await;
                let (a, b) = answer.split_at(answer.len() / 2);
                let _ = conn.write_all(a.as_bytes()).await;
                tokio::time::sleep(Duration::from_millis(10)).await;
                let _ = conn.write_all(b.as_bytes()).await;
                tokio::time::sleep(Duration::from_secs(60)).await;
            }
        });
        (dir, path)
    }

    #[tokio::test]
    async fn no_content_is_none() {
        let (_dir, sock) = fake_vmm("204 No Content", "").await;
        let body = serde_json::json!({"state": "Paused"});
        let answer = request(&sock, "PATCH", "/vm", Some(&body)).await;
        assert_eq!(answer.unwrap(), None);
    }

    #[tokio::test]
    async fn a_body_is_returned() {
        let (_dir, sock) = fake_vmm("200 OK", r#"{"state":"Running"}"#).await;
        let answer = request(&sock, "GET", "/", None).await.unwrap().unwrap();
        assert_eq!(answer["state"], "Running");
    }

    #[tokio::test]
    async fn faults_become_errors() {
        let (_dir, sock) = fake_vmm("400 Bad Request", r#"{"fault_message":"no such vm"}"#).await;
        let err = request(&sock, "PUT", "/actions", None).await.unwrap_err();
        assert!(err.to_string().contains("400: no such vm"), "{err}");
    }
}
