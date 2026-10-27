// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node connections (ADR 0015): one to every instance of the control
//! plane, which feosd dials and on which it is the HTTP/2 server of the
//! API it serves on its Unix socket. The dialers hand their connections to
//! that server as its incoming stream, and learn from the connection when
//! the server dropped it.

use crate::tls;
use log::{debug, info, warn};
use rustls::pki_types::ServerName;
use rustls::ClientConfig;
use std::collections::HashMap;
use std::convert::Infallible;
use std::future::Future;
use std::io;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, oneshot, watch};
use tokio::time::{sleep, Instant};
use tokio_rustls::client::TlsStream;
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::server::{Connected, TcpConnectInfo};
use tonic::transport::Server;

/// The node connections, as the server takes them.
pub type Incoming = ReceiverStream<Result<NodeConn, Infallible>>;

/// The first wait before a redial, doubled up to the last.
const BACKOFF: (Duration, Duration) = (Duration::from_secs(1), Duration::from_secs(30));
/// A connection that lived this long starts the backoff again.
const STABLE: Duration = Duration::from_secs(30);
/// How long a connection of an older certificate may drain at most. The
/// control plane closes it before, when its calls are done or when the
/// certificate's grace ends.
const DRAIN: Duration = Duration::from_secs(3600);

/// The server for node connections: the control plane may run up to 1024
/// calls and streams at once on one, and pings it every 10 s, idle or not,
/// which h2 answers whatever their rate. Its own pings notice a dead
/// connection.
pub fn server() -> Server {
    Server::builder()
        .max_concurrent_streams(1024)
        .http2_keepalive_interval(Some(Duration::from_secs(30)))
        .http2_keepalive_timeout(Some(Duration::from_secs(10)))
}

/// A node connection, as the server serves it. Dropping it tells its
/// dialer that it is gone; the dialer closes it by failing its I/O.
pub struct NodeConn {
    stream: TlsStream<TcpStream>,
    info: TcpConnectInfo,
    _gone: oneshot::Sender<()>,
    /// Resolves when the dialer lets go of the connection.
    close: Option<oneshot::Receiver<()>>,
}

impl NodeConn {
    fn closed(&mut self, cx: &mut Context<'_>) -> bool {
        match &mut self.close {
            None => true,
            Some(close) => match Pin::new(close).poll(cx) {
                Poll::Ready(_) => {
                    self.close = None;
                    true
                }
                Poll::Pending => false,
            },
        }
    }
}

fn aborted() -> io::Error {
    io::Error::new(
        io::ErrorKind::ConnectionAborted,
        "the node connection was closed",
    )
}

impl AsyncRead for NodeConn {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        if self.closed(cx) {
            return Poll::Ready(Err(aborted()));
        }
        Pin::new(&mut self.stream).poll_read(cx, buf)
    }
}

impl AsyncWrite for NodeConn {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        if self.closed(cx) {
            return Poll::Ready(Err(aborted()));
        }
        Pin::new(&mut self.stream).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        if self.closed(cx) {
            return Poll::Ready(Err(aborted()));
        }
        Pin::new(&mut self.stream).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_shutdown(cx)
    }
}

impl Connected for NodeConn {
    type ConnectInfo = TcpConnectInfo;

    fn connect_info(&self) -> TcpConnectInfo {
        self.info.clone()
    }
}

/// What a dialer does: hold a connection, or let the one it has drain and
/// dial no more. A dialer whose sender is gone closes its connection.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Mode {
    Hold,
    Retire,
}

/// The dialers: one per instance of the control plane, with the current
/// certificate, and those of older certificates that drain.
pub(crate) struct Nodes {
    incoming: mpsc::Sender<Result<NodeConn, Infallible>>,
    name: ServerName<'static>,
    config: Option<Arc<ClientConfig>>,
    current: HashMap<String, watch::Sender<Mode>>,
    retired: Vec<(String, watch::Sender<Mode>)>,
}

impl Nodes {
    /// Dialers for the control plane `name`, and the server's incoming
    /// stream they feed.
    pub(crate) fn new(name: ServerName<'static>) -> (Self, Incoming) {
        let (incoming, rx) = mpsc::channel(16);
        let nodes = Self {
            incoming,
            name,
            config: None,
            current: HashMap::new(),
            retired: Vec::new(),
        };
        (nodes, ReceiverStream::new(rx))
    }

    /// Keeps a node connection to each of `targets`, `host:port`, and drops
    /// those to others.
    pub(crate) fn set(&mut self, targets: &[String]) {
        self.current.retain(|target, _| targets.contains(target));
        self.retired
            .retain(|(target, mode)| targets.contains(target) && !mode.is_closed());
        for target in targets {
            if !self.current.contains_key(target)
                && let Some(mode) = self.spawn(target)
            {
                self.current.insert(target.clone(), mode);
            }
        }
    }

    /// Dials with `config` from now on, the node's first certificate or a
    /// renewed one: new connections to every instance, while those of the
    /// old certificate drain.
    pub(crate) fn renew(&mut self, config: Arc<ClientConfig>) {
        self.config = Some(config);
        let targets: Vec<String> = self.current.keys().cloned().collect();
        for (target, mode) in self.current.drain() {
            mode.send_replace(Mode::Retire);
            self.retired.push((target, mode));
        }
        self.set(&targets);
    }

    /// Closes every node connection: for a node that has to join again.
    pub(crate) fn clear(&mut self) {
        self.config = None;
        self.current.clear();
        self.retired.clear();
    }

    fn spawn(&self, target: &str) -> Option<watch::Sender<Mode>> {
        let config = self.config.clone()?;
        let (mode, rx) = watch::channel(Mode::Hold);
        tokio::spawn(dial(
            target.to_string(),
            self.name.clone(),
            config,
            rx,
            self.incoming.clone(),
        ));
        Some(mode)
    }
}

/// Holds a node connection to `target` until the dialer is stopped, and
/// dials it again when it is lost.
async fn dial(
    target: String,
    name: ServerName<'static>,
    config: Arc<ClientConfig>,
    mut mode: watch::Receiver<Mode>,
    incoming: mpsc::Sender<Result<NodeConn, Infallible>>,
) {
    let mut wait = BACKOFF.0;
    let mut failing = false;
    loop {
        let dialed = tokio::select! {
            dialed = tls::connect(&target, &name, config.clone()) => dialed,
            _ = stopped(&mut mode) => return,
        };
        match dialed {
            Ok(stream) => {
                let started = Instant::now();
                let tcp = stream.get_ref().0;
                let info = TcpConnectInfo {
                    local_addr: tcp.local_addr().ok(),
                    remote_addr: tcp.peer_addr().ok(),
                };
                let (gone_tx, mut gone) = oneshot::channel();
                let (close, close_rx) = oneshot::channel::<()>();
                let conn = NodeConn {
                    stream,
                    info,
                    _gone: gone_tx,
                    close: Some(close_rx),
                };
                if incoming.send(Ok(conn)).await.is_err() {
                    return;
                }
                info!("Fleet: Node connection to {target} is up");
                failing = false;
                // Until the server drops it, or the dialer is stopped.
                loop {
                    tokio::select! {
                        _ = &mut gone => break,
                        changed = mode.changed() => {
                            if changed.is_err() {
                                return;
                            }
                            if *mode.borrow() == Mode::Retire {
                                tokio::select! {
                                    _ = &mut gone => {}
                                    _ = sleep(DRAIN) => {}
                                    _ = dropped(&mut mode) => {}
                                }
                                drop(close);
                                return;
                            }
                        }
                    }
                }
                drop(close);
                let lived = started.elapsed();
                if lived > STABLE {
                    wait = BACKOFF.0;
                }
                warn!(
                    "Fleet: Node connection to {target} ended after {}s",
                    lived.as_secs()
                );
            }
            Err(e) if failing => debug!("Fleet: Failed to dial {target} again: {e}"),
            Err(e) => {
                warn!("Fleet: Failed to dial {target} for a node connection: {e}");
                failing = true;
            }
        }
        tokio::select! {
            _ = sleep(wait) => {}
            _ = stopped(&mut mode) => return,
        }
        wait = (wait * 2).min(BACKOFF.1);
    }
}

/// Resolves when the dialer is stopped.
async fn dropped(mode: &mut watch::Receiver<Mode>) {
    while mode.changed().await.is_ok() {}
}

/// Resolves when the dialer is to dial no more: stopped, or retired.
async fn stopped(mode: &mut watch::Receiver<Mode>) {
    loop {
        if *mode.borrow_and_update() == Mode::Retire {
            return;
        }
        if mode.changed().await.is_err() {
            return;
        }
    }
}
