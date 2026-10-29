// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's services through feosd (ADR 0004): a call that carries the
//! metadata `feos-pod-id` goes unchanged to that pod's guest, on vsock; a
//! call without it is for feosd's own services.
//!
//! A call that does not reach the guest fails `UNAVAILABLE` with "the
//! guest of pod <id>: …", or with the pod's state when it does not run; one
//! whose guest does not answer the connect within 5 s, as while its
//! microVM is paused for a snapshot, fails `UNAVAILABLE` with "the guest of
//! pod <id> did not answer within 5s; the VM is busy: <why>";
//! what the guest answers comes back as it is. So a port forward to a
//! port nothing listens on fails `UNAVAILABLE` with "127.0.0.1:<port>: …",
//! the guest's text, and to a Kubernetes listener `FAILED_PRECONDITION`.

use crate::pods::Pods;
use std::convert::Infallible;
use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};
use tonic::body::Body;
use tonic::codegen::http::{Request, Response};
use tonic::Status;
use tower::{Layer, Service, ServiceExt};

/// The metadata that names the pod a call is for.
pub const POD_ID_METADATA: &str = "feos-pod-id";

/// The services of a guest that clients reach through feosd. The others
/// are feosd's business: the guest's host service and its Kubernetes and
/// SSH listeners. A port forward reaches the guest's loopback, but never a
/// Kubernetes listener there: the guest refuses those itself.
const FORWARDED: &[&str] = &[
    "/feos.container.v1.ContainerService/",
    "/feos.image.v1.ImageService/",
    "/feos.portforward.v1.PortForwardService/",
    "/feos.tool.v1.ToolService/",
    "/feos.watch.v1.WatchService/",
];

/// Puts the forwarding in front of feosd's services.
#[derive(Clone)]
pub struct ForwardLayer {
    pods: Pods,
}

impl ForwardLayer {
    pub fn new(pods: Pods) -> Self {
        Self { pods }
    }
}

impl<S> Layer<S> for ForwardLayer {
    type Service = Forward<S>;

    fn layer(&self, inner: S) -> Forward<S> {
        Forward {
            inner,
            pods: self.pods.clone(),
        }
    }
}

#[derive(Clone)]
pub struct Forward<S> {
    inner: S,
    pods: Pods,
}

impl<S> Service<Request<Body>> for Forward<S>
where
    S: Service<Request<Body>, Response = Response<Body>, Error = Infallible>
        + Clone
        + Send
        + 'static,
    S::Future: Send + 'static,
{
    type Response = Response<Body>;
    type Error = Infallible;
    type Future = Pin<Box<dyn Future<Output = Result<Response<Body>, Infallible>> + Send>>;

    fn poll_ready(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), Infallible>> {
        self.inner.poll_ready(cx)
    }

    fn call(&mut self, request: Request<Body>) -> Self::Future {
        let pod_id = request
            .headers()
            .get(POD_ID_METADATA)
            .map(|value| value.to_str().map(str::to_string));
        match pod_id {
            None => {
                // The inner service is the one that was polled ready.
                let ready = self.inner.clone();
                let mut inner = std::mem::replace(&mut self.inner, ready);
                Box::pin(inner.call(request))
            }
            Some(pod_id) => {
                let pods = self.pods.clone();
                Box::pin(async move {
                    let response = match pod_id {
                        Ok(pod_id) => forward(&pods, &pod_id, request).await,
                        Err(_) => Err(Status::invalid_argument(format!(
                            "{POD_ID_METADATA} is not text"
                        ))),
                    };
                    Ok(response.unwrap_or_else(|status| status.into_http()))
                })
            }
        }
    }
}

async fn forward(
    pods: &Pods,
    pod_id: &str,
    request: Request<Body>,
) -> Result<Response<Body>, Status> {
    check_forwarded(request.uri().path())?;
    pods.check_reachable(pod_id)?;
    let channel = pods.guest_channel(pod_id).await?;
    let response = channel
        .oneshot(request)
        .await
        .map_err(|e| Status::unavailable(format!("the guest of pod {pod_id}: {e}")))?;
    Ok(trailers_only(response))
}

/// A response whose headers carry its status, as the guest's to a call it
/// failed before its first message, goes on without a body: as one HEADERS
/// frame that ends the stream. When the guest failed a streaming call that
/// way, it also reset the call's request (RST_STREAM NO_ERROR), after
/// which the guest's body no longer says that it ended; feosd would send
/// the headers and then an empty DATA frame that ends the stream, and a
/// client that reads the status only from a frame that ends the stream, as
/// grpc-go does, would see a stream that ended without one.
fn trailers_only(response: Response<Body>) -> Response<Body> {
    if !response.headers().contains_key("grpc-status") {
        return response;
    }
    let (parts, _) = response.into_parts();
    Response::from_parts(parts, Body::empty())
}

fn check_forwarded(path: &str) -> Result<(), Status> {
    if FORWARDED.iter().any(|service| path.starts_with(service)) {
        Ok(())
    } else {
        Err(Status::unimplemented(format!(
            "{path} is not served for pods"
        )))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_pod_services_are_forwarded() {
        assert!(check_forwarded("/feos.container.v1.ContainerService/Exec").is_ok());
        assert!(check_forwarded("/feos.tool.v1.ToolService/Read").is_ok());
        assert!(check_forwarded("/feos.portforward.v1.PortForwardService/Forward").is_ok());
        let refused = check_forwarded("/feos.host.v1.HostService/Shutdown").unwrap_err();
        assert_eq!(refused.code(), tonic::Code::Unimplemented);
        assert!(check_forwarded("/feos.container.v1.ContainerServiceX/Exec").is_err());
        assert!(check_forwarded("/feos.kube.v1.KubeService/SetListeners").is_err());
        assert!(check_forwarded("/feos.ssh.v1.SshService/SetListeners").is_err());
    }

    #[test]
    fn a_status_in_the_headers_ends_the_response() {
        use tonic::codegen::Body as _;
        // A body that does not say that it ended, as a reset guest's.
        let open = || Body::new(String::from("open"));
        let (parts, _) = Status::unavailable("127.0.0.1:1: refused")
            .into_http::<Body>()
            .into_parts();
        let failed = trailers_only(Response::from_parts(parts, open()));
        assert!(failed.body().is_end_stream());
        assert_eq!(failed.headers()["grpc-status"], "14");

        let answer = trailers_only(Response::new(open()));
        assert!(!answer.body().is_end_stream());
    }
}
