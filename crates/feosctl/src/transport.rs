// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Where feosctl reaches FeOS. An address is one of:
//!
//! - `unix:/run/feos/api.sock`: the local API socket, the default;
//! - `http://[::1]:1337`: the API of a node on TCP (`feosd --tcp`);
//! - `vsock:/path/to/v.sock`: a guest behind a Firecracker vsock socket.
//!
//! With `--pod <id>`, the calls to a node carry the metadata `feos-pod-id`,
//! and feosd forwards them to that pod's guest (ADR 0004).

use anyhow::{Context, Result};
use hyper_util::rt::TokioIo;
use std::path::PathBuf;
use std::task::{Context as TaskContext, Poll};
use tokio::net::UnixStream;
use tonic::body::Body;
use tonic::codegen::http::{HeaderValue, Request, Response};
use tonic::transport::{Channel, Endpoint, Uri};
use tower::{service_fn, Service};

/// A connection whose calls name a pod, if one is given.
#[derive(Clone)]
pub struct Conn {
    channel: Channel,
    pod: Option<HeaderValue>,
}

pub async fn connect(address: &str, pod: Option<&str>) -> Result<Conn> {
    let pod = pod
        .map(|pod| HeaderValue::from_str(pod).with_context(|| format!("Invalid pod ID {pod}")))
        .transpose()?;
    Ok(Conn {
        channel: channel(address).await?,
        pod,
    })
}

impl Service<Request<Body>> for Conn {
    type Response = Response<Body>;
    type Error = tonic::transport::Error;
    type Future = <Channel as Service<Request<Body>>>::Future;

    fn poll_ready(&mut self, cx: &mut TaskContext<'_>) -> Poll<Result<(), Self::Error>> {
        self.channel.poll_ready(cx)
    }

    fn call(&mut self, mut request: Request<Body>) -> Self::Future {
        if let Some(pod) = &self.pod {
            request.headers_mut().insert("feos-pod-id", pod.clone());
        }
        self.channel.call(request)
    }
}

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
