// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Where feosctl reaches FeOS. An address is one of:
//!
//! - `http://[::1]:1337`: the TCP API of a host;
//! - `unix:/run/feos/api.sock`: the local API socket;
//! - `vsock:/path/to/v.sock`: a guest behind a Firecracker vsock socket.

use anyhow::{Context, Result};
use hyper_util::rt::TokioIo;
use std::path::PathBuf;
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

pub async fn channel(address: &str) -> Result<Channel> {
    if let Some(path) = address.strip_prefix("vsock:") {
        let uds = PathBuf::from(path);
        return feos_utils::vsock::channel(uds, feos_utils::vsock::API_PORT)
            .await
            .with_context(|| format!("Failed to connect to the guest behind {path}"));
    }
    if let Some(path) = address.strip_prefix("unix:") {
        let path = PathBuf::from(path);
        return Endpoint::from_static("http://feos.local")
            .connect_with_connector(service_fn(move |_: Uri| {
                let path = path.clone();
                async move { UnixStream::connect(path).await.map(TokioIo::new) }
            }))
            .await
            .with_context(|| format!("Failed to connect to {address}"));
    }
    Endpoint::from_shared(address.to_string())
        .with_context(|| format!("Invalid address {address}"))?
        .connect()
        .await
        .with_context(|| format!("Failed to connect to {address}"))
}
