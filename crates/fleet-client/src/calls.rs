// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The calls on the node connections run on feosd's runtime, not on the
//! control plane's threads, which carry the connections themselves: TLS,
//! HTTP/2 and its pings. A call and its answer's body, however long they
//! take and whatever they block, hold neither the connections nor the
//! reports. A call whose stream the control plane resets, or whose
//! connection goes, is dropped on feosd's runtime as it would be there.

use hyper::body::{Body as _, Frame};
use std::convert::Infallible;
use std::future::{poll_fn, Future};
use std::pin::Pin;
use std::task::{Context, Poll};
use tokio::runtime::Handle;
use tokio::sync::mpsc;
use tokio::task::JoinHandle;
use tonic::body::Body;
use tonic::codegen::http::{Request, Response};
use tonic::codegen::Bytes;
use tonic::Status;
use tower::{Layer, Service};

/// Runs the services below it on `runtime`.
#[derive(Clone)]
pub struct CallsOn {
    runtime: Handle,
}

impl CallsOn {
    pub fn new(runtime: Handle) -> Self {
        Self { runtime }
    }
}

impl<S> Layer<S> for CallsOn {
    type Service = OnRuntime<S>;

    fn layer(&self, inner: S) -> OnRuntime<S> {
        OnRuntime {
            inner,
            runtime: self.runtime.clone(),
        }
    }
}

#[derive(Clone)]
pub struct OnRuntime<S> {
    inner: S,
    runtime: Handle,
}

impl<S> Service<Request<Body>> for OnRuntime<S>
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
        // The inner service is the one that was polled ready.
        let ready = self.inner.clone();
        let mut inner = std::mem::replace(&mut self.inner, ready);
        let call = Aborting(self.runtime.spawn(async move {
            let response = match inner.call(request).await {
                Ok(response) => response,
                Err(never) => match never {},
            };
            relay(response)
        }));
        Box::pin(async move {
            Ok(call
                .await
                .unwrap_or_else(|e| Status::internal(format!("the call ended: {e}")).into_http()))
        })
    }
}

/// A call on feosd's runtime, aborted when the connection's side lets go
/// of it.
struct Aborting<T>(JoinHandle<T>);

impl<T> Future for Aborting<T> {
    type Output = Result<T, tokio::task::JoinError>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        Pin::new(&mut self.0).poll(cx)
    }
}

impl<T> Drop for Aborting<T> {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// The answer with its body read here, on feosd's runtime, and handed to
/// the connection a frame at a time. A body that has ended already goes
/// as it is: an answer whose status is in its headers ends the stream with
/// them (pod-service's forward.rs), which a body of frames would not.
fn relay(response: Response<Body>) -> Response<Body> {
    if response.body().is_end_stream() {
        return response;
    }
    let (parts, mut body) = response.into_parts();
    let (frames, relayed) = mpsc::channel(1);
    tokio::spawn(async move {
        loop {
            let frame = tokio::select! {
                frame = poll_fn(|cx| Pin::new(&mut body).poll_frame(cx)) => frame,
                () = frames.closed() => return,
            };
            let Some(frame) = frame else {
                return;
            };
            let failed = frame.is_err();
            if frames.send(frame).await.is_err() || failed {
                return;
            }
        }
    });
    Response::from_parts(parts, Body::new(Relayed(relayed)))
}

/// The frames of a body that is read elsewhere.
struct Relayed(mpsc::Receiver<Result<Frame<Bytes>, Status>>);

impl hyper::body::Body for Relayed {
    type Data = Bytes;
    type Error = Status;

    fn poll_frame(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, Status>>> {
        self.0.poll_recv(cx)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;
    use std::time::Duration;
    use tower::ServiceExt;

    /// A runtime of the test's own, shut down without waiting at the end.
    struct Elsewhere(Option<tokio::runtime::Runtime>);

    impl Elsewhere {
        fn new() -> Self {
            Self(Some(
                tokio::runtime::Builder::new_multi_thread()
                    .worker_threads(1)
                    .thread_name("elsewhere")
                    .enable_all()
                    .build()
                    .unwrap(),
            ))
        }

        fn handle(&self) -> Handle {
            self.0.as_ref().unwrap().handle().clone()
        }
    }

    impl Drop for Elsewhere {
        fn drop(&mut self) {
            if let Some(runtime) = self.0.take() {
                runtime.shutdown_background();
            }
        }
    }

    fn thread_name() -> String {
        std::thread::current()
            .name()
            .unwrap_or_default()
            .to_string()
    }

    /// Answers with the name of the thread it ran on, in its body, its
    /// status in the trailers, as gRPC does.
    async fn answer(_: Request<Body>) -> Result<Response<Body>, Infallible> {
        let (frames, relayed) = mpsc::channel(2);
        let name = thread_name();
        tokio::spawn(async move {
            let _ = frames.send(Ok(Frame::data(Bytes::from(name)))).await;
            let mut trailers = tonic::codegen::http::HeaderMap::new();
            trailers.insert("grpc-status", "0".parse().unwrap());
            let _ = frames.send(Ok(Frame::trailers(trailers))).await;
        });
        Ok(Response::new(Body::new(Relayed(relayed))))
    }

    async fn frames(body: Body) -> Vec<Frame<Bytes>> {
        let mut body = body;
        let mut frames = Vec::new();
        while let Some(frame) = poll_fn(|cx| Pin::new(&mut body).poll_frame(cx)).await {
            frames.push(frame.unwrap());
        }
        frames
    }

    #[tokio::test]
    async fn a_call_and_its_body_run_on_the_runtime_given() {
        let elsewhere = Elsewhere::new();
        let service = CallsOn::new(elsewhere.handle()).layer(tower::service_fn(answer));
        let response = service.oneshot(Request::new(Body::empty())).await.unwrap();
        let frames = frames(response.into_body()).await;
        assert_eq!(frames.len(), 2);
        assert_eq!(frames[0].data_ref().unwrap(), "elsewhere");
        assert_eq!(frames[1].trailers_ref().unwrap()["grpc-status"], "0");
    }

    #[tokio::test]
    async fn a_status_in_the_headers_still_ends_the_stream() {
        let elsewhere = Elsewhere::new();
        let refuse = tower::service_fn(|_: Request<Body>| async {
            Ok::<_, Infallible>(Status::unavailable("no").into_http())
        });
        let service = CallsOn::new(elsewhere.handle()).layer(refuse);
        let response = service.oneshot(Request::new(Body::empty())).await.unwrap();
        assert_eq!(response.headers()["grpc-status"], "14");
        assert!(response.body().is_end_stream());
    }

    #[tokio::test]
    async fn a_call_that_is_let_go_of_is_dropped_where_it_runs() {
        let elsewhere = Elsewhere::new();
        let dropped = Arc::new(AtomicBool::new(false));
        let flag = dropped.clone();
        let hang = tower::service_fn(move |_: Request<Body>| {
            struct Flag(Arc<AtomicBool>);
            impl Drop for Flag {
                fn drop(&mut self) {
                    self.0.store(true, Ordering::SeqCst);
                }
            }
            let flag = Flag(flag.clone());
            async move {
                let _flag = flag;
                std::future::pending::<Result<Response<Body>, Infallible>>().await
            }
        });
        let service = CallsOn::new(elsewhere.handle()).layer(hang);
        let call = service.oneshot(Request::new(Body::empty()));
        assert!(tokio::time::timeout(Duration::from_millis(100), call)
            .await
            .is_err());
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        while !dropped.load(Ordering::SeqCst) {
            assert!(tokio::time::Instant::now() < deadline, "the call runs on");
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }
}
