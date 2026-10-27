// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Host side of the vsock transport to a FeOS guest.
//!
//! Firecracker exposes a guest's vsock device to the host as a Unix socket
//! ("hybrid vsock"). A host process connects to that socket, writes
//! `CONNECT <port>\n`, and the VMM answers `OK <host port>\n` once the guest
//! has accepted; from then on the stream carries the guest connection.

use hyper_util::rt::TokioIo;
use std::io;
use std::path::{Path, PathBuf};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

/// Guest port of the FeOS API.
pub const API_PORT: u32 = 10000;
/// Host port the guest's lifecycle uplink dials (LifecycleService).
pub const UPLINK_PORT: u32 = 10001;

/// The longest reply the handshake accepts: "OK " and a 32-bit port.
const MAX_REPLY: usize = 32;

/// Connects to `port` in the guest behind the Firecracker vsock socket `uds`.
pub async fn connect(uds: &Path, port: u32) -> io::Result<UnixStream> {
    let mut stream = UnixStream::connect(uds).await?;
    stream
        .write_all(format!("CONNECT {port}\n").as_bytes())
        .await?;

    // Read the reply one byte at a time: the guest may start talking right
    // after it, and a buffered reader would swallow those bytes.
    let mut reply = Vec::new();
    loop {
        let byte = stream.read_u8().await?;
        if byte == b'\n' {
            break;
        }
        if reply.len() == MAX_REPLY {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "vsock handshake reply too long",
            ));
        }
        reply.push(byte);
    }
    if !reply.starts_with(b"OK ") {
        return Err(io::Error::new(
            io::ErrorKind::ConnectionRefused,
            format!(
                "guest refused CONNECT {port}: {:?}",
                String::from_utf8_lossy(&reply)
            ),
        ));
    }
    Ok(stream)
}

/// A gRPC channel to `port` in the guest behind the Firecracker vsock socket
/// `uds`. Every new HTTP/2 connection does its own handshake.
pub async fn channel(
    uds: impl Into<PathBuf>,
    port: u32,
) -> Result<Channel, tonic::transport::Error> {
    let uds = uds.into();
    Endpoint::from_static("http://feos.vsock")
        .connect_with_connector(service_fn(move |_: Uri| {
            let uds = uds.clone();
            async move { connect(&uds, port).await.map(TokioIo::new) }
        }))
        .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::AsyncBufReadExt;
    use tokio::net::UnixListener;

    /// A fake VMM that answers the handshake with `reply` and then sends
    /// `payload` right behind it, in the same write.
    async fn fake_vmm(reply: &'static str, payload: &'static [u8]) -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("v.sock");
        let listener = UnixListener::bind(&path).unwrap();
        tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = tokio::io::BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            assert_eq!(line, "CONNECT 10000\n");
            let mut out = reply.as_bytes().to_vec();
            out.extend_from_slice(payload);
            stream.get_mut().write_all(&out).await.unwrap();
        });
        (dir, path)
    }

    #[tokio::test]
    async fn keeps_bytes_after_the_reply() {
        let (_dir, path) = fake_vmm("OK 1073741824\n", b"guest").await;
        let mut stream = connect(&path, API_PORT).await.unwrap();
        let mut buf = [0u8; 5];
        stream.read_exact(&mut buf).await.unwrap();
        assert_eq!(&buf, b"guest");
    }

    #[tokio::test]
    async fn refused_connect_is_an_error() {
        let (_dir, path) = fake_vmm("FAILURE\n", b"").await;
        let err = connect(&path, API_PORT).await.unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::ConnectionRefused);
    }
}
