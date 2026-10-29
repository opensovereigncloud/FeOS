// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The calls on the node connections run on feosd's runtime, not on the
//! control plane's threads, which carry the connections themselves: TLS,
//! HTTP/2 and its pings. A call and its answer's body, however long they
//! take and whatever they block, hold neither the connections nor the
//! reports. A call whose stream the control plane resets, or whose
//! connection goes, is dropped on feosd's runtime as it would be there.
//!
//! When feosd stops (`Fleet::stop`), the server sends GOAWAY on every node
//! connection and takes no new call, and the calls in flight end as they
//! do, up to a bound; those still running then end with UNAVAILABLE "the
//! node stops: …" (`NodeCalls::cut`), a call not answered yet as its
//! answer, a stream with it as its trailers. That is done here, on the
//! control plane's threads, whatever feosd's runtime does.

use hyper::body::{Body as _, Frame};
use std::convert::Infallible;
use std::future::{poll_fn, Future};
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;
use tokio::runtime::Handle;
use tokio::sync::{mpsc, watch};
use tokio::task::JoinHandle;
use tonic::body::Body;
use tonic::codegen::http::{HeaderMap, HeaderValue, Request, Response};
use tonic::codegen::Bytes;
use tonic::Status;
use tower::{Layer, Service};

/// The calls on the node connections, and their end when feosd stops: one
/// for the server (`server`) and the node's side (`start`).
#[derive(Clone)]
pub struct NodeCalls(Arc<Shared>);

struct Shared {
    /// Set when feosd stops: the server sends GOAWAY and takes no call.
    goaway: watch::Sender<bool>,
    /// The answer of the calls still running at the stop's bound.
    cut: watch::Sender<Option<String>>,
    /// The server let go of every node connection, and with them of every
    /// call.
    served: watch::Sender<bool>,
    /// Whether a server serves the calls.
    serving: AtomicBool,
    /// The calls not answered yet and the bodies not ended yet.
    in_flight: AtomicUsize,
}

impl Default for NodeCalls {
    fn default() -> Self {
        Self(Arc::new(Shared {
            goaway: watch::channel(false).0,
            cut: watch::channel(None).0,
            served: watch::channel(false).0,
            serving: AtomicBool::new(false),
            in_flight: AtomicUsize::new(0),
        }))
    }
}

impl NodeCalls {
    pub fn new() -> Self {
        Self::default()
    }

    /// The calls and streams in flight.
    pub fn in_flight(&self) -> usize {
        self.0.in_flight.load(Ordering::SeqCst)
    }

    /// Whether feosd stops: the server takes no new call.
    pub fn stopping(&self) -> bool {
        *self.0.goaway.borrow()
    }

    /// Takes no new call: the server sends GOAWAY on every connection, and
    /// lets each go once its calls are done.
    pub(crate) fn goaway(&self) {
        self.0.goaway.send_replace(true);
    }

    /// Resolves once `goaway` was called: the server's shutdown signal.
    pub(crate) fn goaway_requested(&self) -> impl Future<Output = ()> + Send + 'static {
        let mut goaway = self.0.goaway.subscribe();
        async move {
            if goaway.wait_for(|stop| *stop).await.is_err() {
                std::future::pending::<()>().await;
            }
        }
    }

    /// Ends every call in flight with UNAVAILABLE `why`.
    pub(crate) fn cut(&self, why: String) {
        self.0.cut.send_replace(Some(why));
    }

    pub(crate) fn serving(&self) {
        self.0.serving.store(true, Ordering::SeqCst);
    }

    pub(crate) fn served(&self) {
        self.0.served.send_replace(true);
    }

    /// Waits up to `within` for the server to let go of every connection;
    /// whether it did. Without a server, there is nothing to wait for.
    pub(crate) async fn wait_served(&self, within: Duration) -> bool {
        if !self.0.serving.load(Ordering::SeqCst) {
            return true;
        }
        let mut served = self.0.served.subscribe();
        tokio::time::timeout(within, served.wait_for(|served| *served))
            .await
            .is_ok_and(|served| served.is_ok())
    }

    fn flight(&self) -> Flight {
        self.0.in_flight.fetch_add(1, Ordering::SeqCst);
        Flight(self.0.clone())
    }
}

/// A call in flight, until it is dropped.
struct Flight(Arc<Shared>);

impl Drop for Flight {
    fn drop(&mut self) {
        self.0.in_flight.fetch_sub(1, Ordering::SeqCst);
    }
}

/// Resolves with why the calls are cut, once they are.
async fn cut_of(mut cut: watch::Receiver<Option<String>>) -> String {
    let why = cut
        .wait_for(Option::is_some)
        .await
        .map(|why| why.clone().unwrap_or_default());
    match why {
        Ok(why) => why,
        Err(_) => std::future::pending().await,
    }
}

/// Runs the services below it on `runtime`, and cuts their calls when
/// `calls` says so.
#[derive(Clone)]
pub struct CallsOn {
    runtime: Handle,
    calls: NodeCalls,
}

impl CallsOn {
    pub fn new(runtime: Handle, calls: NodeCalls) -> Self {
        Self { runtime, calls }
    }
}

impl<S> Layer<S> for CallsOn {
    type Service = OnRuntime<S>;

    fn layer(&self, inner: S) -> OnRuntime<S> {
        OnRuntime {
            inner,
            runtime: self.runtime.clone(),
            calls: self.calls.clone(),
        }
    }
}

#[derive(Clone)]
pub struct OnRuntime<S> {
    inner: S,
    runtime: Handle,
    calls: NodeCalls,
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
        let flight = self.calls.flight();
        let cut = self.calls.0.cut.subscribe();
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
            let response = tokio::select! {
                biased;
                answered = call => answered
                    .unwrap_or_else(|e| Status::internal(format!("the call ended: {e}")).into_http()),
                // Dropping the call aborts it where it runs.
                why = cut_of(cut.clone()) => return Ok(Status::unavailable(why).into_http()),
            };
            Ok(cuttable(response, cut, flight))
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

/// The answer, whose body, until it ends, the cut ends with its status as
/// the trailers. A body that has ended already goes as it is.
fn cuttable(
    response: Response<Body>,
    cut: watch::Receiver<Option<String>>,
    flight: Flight,
) -> Response<Body> {
    if response.body().is_end_stream() {
        return response;
    }
    response.map(|body| {
        Body::new(Cuttable {
            body,
            cut: Box::pin(cut_of(cut)),
            ended: false,
            _flight: flight,
        })
    })
}

/// A body that the cut ends.
struct Cuttable {
    body: Body,
    cut: Pin<Box<dyn Future<Output = String> + Send>>,
    ended: bool,
    _flight: Flight,
}

impl hyper::body::Body for Cuttable {
    type Data = Bytes;
    type Error = Status;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, Status>>> {
        let this = self.get_mut();
        if this.ended {
            return Poll::Ready(None);
        }
        if let Poll::Ready(why) = this.cut.as_mut().poll(cx) {
            this.ended = true;
            return Poll::Ready(Some(Ok(Frame::trailers(trailers(&Status::unavailable(
                why,
            ))))));
        }
        let frame = Pin::new(&mut this.body).poll_frame(cx);
        match &frame {
            Poll::Ready(None) | Poll::Ready(Some(Err(_))) => this.ended = true,
            Poll::Ready(Some(Ok(frame))) if frame.is_trailers() => this.ended = true,
            _ => {}
        }
        frame
    }

    fn is_end_stream(&self) -> bool {
        self.ended
    }
}

/// `status` as gRPC's trailers.
fn trailers(status: &Status) -> HeaderMap {
    let mut trailers = HeaderMap::new();
    if status.add_header(&mut trailers).is_err() {
        trailers.insert("grpc-status", HeaderValue::from(status.code() as i32));
    }
    trailers
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
        let service =
            CallsOn::new(elsewhere.handle(), NodeCalls::new()).layer(tower::service_fn(answer));
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
        let service = CallsOn::new(elsewhere.handle(), NodeCalls::new()).layer(refuse);
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
        let service = CallsOn::new(elsewhere.handle(), NodeCalls::new()).layer(hang);
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

    /// A call that runs at the stop's bound ends with UNAVAILABLE "the
    /// node stops", where it would have ended without an answer, and is
    /// dropped where it runs; one that answered meanwhile is not.
    #[tokio::test]
    async fn the_cut_answers_a_call_that_runs_and_drops_it() {
        let elsewhere = Elsewhere::new();
        let calls = NodeCalls::new();
        let (entered, mut came) = mpsc::channel::<()>(1);
        let hang = tower::service_fn(move |_: Request<Body>| {
            let entered = entered.clone();
            async move {
                let _ = entered.send(()).await;
                // Ends only when dropped: the channel closes then.
                entered.closed().await;
                Ok::<_, Infallible>(Status::ok("never").into_http())
            }
        });
        let service = CallsOn::new(elsewhere.handle(), calls.clone()).layer(hang);
        let call = tokio::spawn(service.oneshot(Request::new(Body::empty())));
        came.recv().await.unwrap();
        assert_eq!(calls.in_flight(), 1);
        calls.cut("the node stops: the call did not end within 1s".into());
        let response = call.await.unwrap().unwrap();
        assert_eq!(response.headers()["grpc-status"], "14");
        let status = Status::from_header_map(response.headers()).unwrap();
        assert_eq!(
            status.message(),
            "the node stops: the call did not end within 1s"
        );
        assert!(response.body().is_end_stream());
        // Dropped on its runtime: its sender is gone.
        tokio::time::timeout(Duration::from_secs(5), came.recv())
            .await
            .expect("the call runs on");
        assert_eq!(calls.in_flight(), 0);
    }

    /// A stream that runs at the stop's bound ends with the cut's status as
    /// its trailers, after what it sent; one that ended with its own
    /// trailers before keeps them.
    #[tokio::test]
    async fn the_cut_ends_a_stream_with_its_status() {
        let elsewhere = Elsewhere::new();
        let calls = NodeCalls::new();
        let stream = tower::service_fn(|_: Request<Body>| async {
            let (frames, relayed) = mpsc::channel(2);
            tokio::spawn(async move {
                let _ = frames.send(Ok(Frame::data(Bytes::from("out")))).await;
                frames.closed().await;
            });
            Ok::<_, Infallible>(Response::new(Body::new(Relayed(relayed))))
        });
        let service = CallsOn::new(elsewhere.handle(), calls.clone()).layer(stream);
        let response = service
            .clone()
            .oneshot(Request::new(Body::empty()))
            .await
            .unwrap();
        let mut body = response.into_body();
        let first = poll_fn(|cx| Pin::new(&mut body).poll_frame(cx))
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.data_ref().unwrap(), "out");
        assert_eq!(calls.in_flight(), 1);
        calls.cut("the node stops".into());
        let rest = frames(body).await;
        assert_eq!(rest.len(), 1, "{rest:?}");
        let trailers = rest[0].trailers_ref().expect("trailers");
        assert_eq!(trailers["grpc-status"], "14");
        assert_eq!(
            Status::from_header_map(trailers).unwrap().message(),
            "the node stops"
        );
        assert_eq!(calls.in_flight(), 0);

        // A stream that ended before keeps its own trailers.
        let calls = NodeCalls::new();
        let service =
            CallsOn::new(elsewhere.handle(), calls.clone()).layer(tower::service_fn(answer));
        let response = service.oneshot(Request::new(Body::empty())).await.unwrap();
        let mut body = response.into_body();
        let mut got = Vec::new();
        while let Some(frame) = poll_fn(|cx| Pin::new(&mut body).poll_frame(cx)).await {
            got.push(frame.unwrap());
            if got.len() == 2 {
                calls.cut("the node stops".into());
            }
        }
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!(got[1].trailers_ref().unwrap()["grpc-status"], "0");
    }
}
