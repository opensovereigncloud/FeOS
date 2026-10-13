// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! KubeService — loopback listeners for Kubernetes bindings.
//!
//! A sandbox reaches a Kubernetes API server without ever holding a
//! credential. The kubeconfig in the guest says
//! `server: http://127.0.0.1:<port>` and has no `user`; that port is a
//! listener of FeOS. Every connection accepted there becomes one vsock
//! stream to the host (CID 2, port 10002), where the sandbox's jailed
//! kube-proxy adds the credential and dials the cluster.
//!
//! This is `PortForwardService` run backwards: FeOS writes a one-line
//! preamble naming the local port (`kube <port>\n`) and from then on only
//! splices bytes. HTTP, its upgrades (`exec`, `attach`, `port-forward`)
//! and chunked watches ride the pipe unchanged; FeOS parses none of it.
//! A half-close travels in both directions, so a client that ends its
//! request with a shutdown still reads the whole response.
//!
//! The preamble carries **no authority**. Identity comes from the socket
//! the stream arrives on host-side, which is the sandbox's own; a process
//! in the container that dialled the vsock port itself would gain nothing
//! it does not already have through loopback.
//!
//! **The listener set is whole-set and idempotent.** The manager pushes it
//! after every boot and resume and whenever the bindings change. Named
//! ports that already listen are kept, ports not named are closed, and
//! connections already accepted on a closed port are left to finish. A
//! port that cannot be bound lands in `failed`, never in a failed RPC:
//! one squatted port must not cost the others. The listeners live in guest
//! RAM, so a restored snapshot has them back and the push after a resume
//! simply finds them. Binding 127.0.0.1 needs `lo` up, which
//! NetworkService does; the manager pushes the network config first.
//!
//! **The connections do not survive a pause.** The host side of every
//! stream dies with the run's kube-proxy, but an idle vsock stream held
//! across a snapshot never wakes its reader in the restored guest
//! (concept-agent-os §6.3) — a quiet `kubectl get -w` would hang there for
//! good. So the push after a boot or resume carries `reset_connections`,
//! and FeOS then closes every kube connection it holds, both sockets of
//! each, before it reconciles the listeners. The client sees EOF and
//! reconnects to the new proxy. Every other push leaves connections alone.
//!
//! **A kube port is not forwardable**. The set of ports
//! FeOS listens on here ([`KubePorts`]) is shared with
//! `PortForwardService`, which refuses an `open` naming one. Otherwise a
//! port forward would publish a plaintext Kubernetes API that adds the
//! credential for whoever reaches it.
//!
//! When no proxy listens on the host (one crashed and is being respawned,
//! say), the guest client gets one fixed `502` instead of a bare reset it
//! would retry blindly. That response is the only HTTP FeOS ever speaks.

use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;
use std::io;
use std::net::{Ipv4Addr, SocketAddr};
use std::pin::Pin;
use std::sync::{Arc, Mutex, MutexGuard, RwLock};
use std::time::Duration;

use feos_proto::kube_service::kube_service_server::KubeService;
use feos_proto::kube_service::{ListenerFailure, SetListenersRequest, SetListenersResponse};
use nix::fcntl::{fcntl, FcntlArg, FdFlag};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpSocket, TcpStream};
use tokio::task::{JoinHandle, JoinSet};
use tokio_vsock::{VsockAddr, VsockStream, VMADDR_CID_HOST};
use tonic::{Request, Response, Status};

/// Port the sandbox's kube-proxy is reached on. Firecracker maps a guest
/// connection to (CID 2, 10002) to the host path `<uds_path>_10002`,
/// which the manager creates and hands to the proxy.
pub const KUBE_VSOCK_PORT: u32 = 10002;

/// Ports a binding may use: unprivileged TCP ports only.
const PORT_MIN: u32 = 1024;
const PORT_MAX: u32 = 65535;

/// Accept backlog per listener. kubectl opens a handful of connections;
/// this only has to absorb a burst of parallel client-go requests.
const BACKLOG: u32 = 128;

/// Upper bound for reaching the proxy. A vsock connect is answered by the
/// host's VMM at once (accepted or reset); this only guards against a
/// connect that is never answered at all.
const DIAL_TIMEOUT: Duration = Duration::from_secs(5);

/// How long the 502 path keeps reading after its response — see
/// [`bad_gateway`].
const LINGER: Duration = Duration::from_secs(2);

/// Pause after a failed `accept` (out of fds, say), so the loop does not
/// spin while the condition lasts.
const ACCEPT_BACKOFF: Duration = Duration::from_millis(100);

/// The answer when the proxy cannot be reached, byte for byte.
pub const BAD_GATEWAY: &[u8] = b"HTTP/1.1 502 Bad Gateway\r\n\
Content-Type: text/plain\r\n\
Content-Length: 33\r\n\
Connection: close\r\n\
\r\n\
FeOS: kube proxy is not running\r\n";

/// The ports FeOS currently listens on for Kubernetes bindings. Cheap to
/// clone; `PortForwardService` consults it on every `open`.
///
/// A port enters the set **before** its socket is bound and leaves it
/// only **after** the socket is closed. So whenever a kube listener
/// exists, its port is in the set — the invariant the forward refusal
/// rests on.
#[derive(Clone, Default)]
pub struct KubePorts(Arc<RwLock<BTreeSet<u16>>>);

impl KubePorts {
    pub fn contains(&self, port: u16) -> bool {
        // The set is updated one element at a time, so a panic elsewhere
        // can never leave it half-written: poisoning carries no meaning.
        self.0
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .contains(&port)
    }

    fn insert(&self, port: u16) {
        self.0
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .insert(port);
    }

    fn remove(&self, port: u16) {
        self.0
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .remove(&port);
    }
}

/// The host leg of one piped connection, whatever carries it. Shared with
/// `SshService`, whose agent socket is piped the same way.
pub trait HostIo: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send> HostIo for T {}

pub type DialFuture = Pin<Box<dyn Future<Output = io::Result<Box<dyn HostIo>>> + Send>>;

/// Opens the host leg. In the guest that is vsock; tests dial a Unix
/// socket instead, which is exactly what the proxy itself sees on the
/// host (Firecracker hands it a Unix connection per guest stream).
pub trait HostDialer: Send + Sync + 'static {
    fn dial(&self) -> DialFuture;
}

/// The production dialer: (CID 2, the given port) — 10002 for the
/// kube-proxy, 10003 for the ssh agent.
pub struct VsockDialer(pub u32);

impl HostDialer for VsockDialer {
    fn dial(&self) -> DialFuture {
        let port = self.0;
        Box::pin(async move {
            let stream = VsockStream::connect(VsockAddr::new(VMADDR_CID_HOST, port)).await?;
            // tokio-vsock 0.7 hands O_CLOEXEC to F_SETFL, where the kernel
            // ignores it, so its sockets are inherited across execve. Exec
            // forks FeOS itself; without this, every process an agent
            // starts while a piped connection is open would hold a copy of
            // the stream to the host and keep it alive past its end.
            fcntl(&stream, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC)).map_err(io::Error::from)?;
            Ok(Box::new(stream) as Box<dyn HostIo>)
        })
    }
}

/// The accepted connections of every listener, so a reset can end them
/// all. Each one is a task that owns both of its sockets; ending the task
/// closes them.
struct Conns {
    dialer: Arc<dyn HostDialer>,
    open: Mutex<JoinSet<()>>,
}

impl Conns {
    fn open(&self) -> MutexGuard<'_, JoinSet<()>> {
        // Spawns and swaps only; a panic elsewhere cannot leave it torn.
        self.open.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Pipe one accepted connection.
    fn spawn(&self, guest: TcpStream, port: u16) {
        let mut open = self.open();
        // Collect what has finished, so the set holds open connections
        // and not the history of every one.
        while open.try_join_next().is_some() {}
        open.spawn(pipe(guest, port, self.dialer.clone()));
    }

    /// End every open connection and wait until each is gone. Returns how
    /// many there were.
    async fn reset(&self) -> usize {
        let mut old = std::mem::take(&mut *self.open());
        while old.try_join_next().is_some() {}
        let n = old.len();
        old.shutdown().await;
        n
    }
}

pub struct KubeApiHandler(Arc<Listeners>);

struct Listeners {
    ports: KubePorts,
    /// One accept loop per listening port; the loop owns the listener.
    /// The async mutex also serializes reconciles, and a reconcile holds
    /// it while it waits for a closed listener to be gone — so a port
    /// leaving the set is free again before anything is bound.
    live: tokio::sync::Mutex<BTreeMap<u16, JoinHandle<()>>>,
    conns: Arc<Conns>,
}

impl Default for KubeApiHandler {
    fn default() -> Self {
        Self::with_dialer(Arc::new(VsockDialer(KUBE_VSOCK_PORT)))
    }
}

impl KubeApiHandler {
    pub fn with_dialer(dialer: Arc<dyn HostDialer>) -> Self {
        KubeApiHandler(Arc::new(Listeners {
            ports: KubePorts::default(),
            live: tokio::sync::Mutex::new(BTreeMap::new()),
            conns: Arc::new(Conns {
                dialer,
                open: Mutex::new(JoinSet::new()),
            }),
        }))
    }

    /// The listening set, for `PortForwardService`.
    pub fn ports(&self) -> KubePorts {
        self.0.ports.clone()
    }
}

impl Listeners {
    /// Converge on `requested`, after ending every open connection if
    /// `reset` says the host side of them is gone. Never fails as a whole:
    /// every port that cannot listen is reported on its own.
    async fn reconcile(&self, requested: &[u32], reset: bool) -> SetListenersResponse {
        let mut want = BTreeSet::new();
        let mut failed = BTreeMap::new();
        for &port in requested {
            match check_port(port) {
                Ok(p) => {
                    want.insert(p);
                }
                Err(reason) => {
                    failed.insert(port, reason);
                }
            }
        }

        let mut live = self.live.lock().await;
        let before: Vec<u16> = live.keys().copied().collect();

        // Before the listeners: a connection accepted from here on is one
        // to the new proxy, and must stay.
        if reset {
            let n = self.conns.reset().await;
            if n > 0 {
                log::info!("kube: reset {n} connection(s)");
            }
        }

        // Close first, so a port that leaves the set frees its address
        // before anything new is bound. An accept loop never ends on its
        // own; one that did (a panic) is dropped here and bound afresh
        // below if it is still wanted.
        let stale: Vec<u16> = live
            .iter()
            .filter(|(port, task)| !want.contains(port) || task.is_finished())
            .map(|(&port, _)| port)
            .collect();
        for port in stale {
            if let Some(task) = live.remove(&port) {
                close(task).await;
            }
            self.ports.remove(port);
        }

        for &port in &want {
            if live.contains_key(&port) {
                continue;
            }
            self.ports.insert(port);
            match bind(port) {
                Ok(listener) => {
                    let conns = self.conns.clone();
                    live.insert(port, tokio::spawn(accept_loop(listener, port, conns)));
                }
                Err(e) => {
                    self.ports.remove(port);
                    failed.insert(u32::from(port), format!("bind 127.0.0.1:{port}: {e}"));
                }
            }
        }

        let listening: Vec<u32> = live.keys().map(|&p| u32::from(p)).collect();
        let after: Vec<u16> = live.keys().copied().collect();
        if after != before || !failed.is_empty() {
            let mut msg = format!("kube listeners: {after:?}");
            for (port, reason) in &failed {
                msg.push_str(&format!("; {port} failed: {reason}"));
            }
            log::info!("{msg}");
        }

        SetListenersResponse {
            listening,
            failed: failed
                .into_iter()
                .map(|(port, reason)| ListenerFailure { port, reason })
                .collect(),
        }
    }
}

#[tonic::async_trait]
impl KubeService for KubeApiHandler {
    async fn set_listeners(
        &self,
        req: Request<SetListenersRequest>,
    ) -> Result<Response<SetListenersResponse>, Status> {
        let listeners = self.0.clone();
        let SetListenersRequest {
            ports,
            reset_connections,
        } = req.into_inner();
        // A reconcile runs to completion even if the caller gives up
        // halfway. Cancelled between closing a listener and dropping its
        // port from the shared set, it would leave a port that nothing
        // listens on refused for forwarding.
        tokio::spawn(async move { listeners.reconcile(&ports, reset_connections).await })
            .await
            .map(Response::new)
            .map_err(|e| Status::internal(format!("kube listeners: {e}")))
    }
}

/// A port a binding may name, or why not.
fn check_port(port: u32) -> Result<u16, String> {
    if (PORT_MIN..=PORT_MAX).contains(&port) {
        Ok(port as u16)
    } else {
        Err(format!("port {port} out of range ({PORT_MIN}-{PORT_MAX})"))
    }
}

/// `127.0.0.1:<port>`, IPv4 loopback only: reachable from inside the
/// guest, where the container shares FeOS's network namespace, and from
/// nowhere else.
fn bind(port: u16) -> io::Result<TcpListener> {
    let socket = TcpSocket::new_v4()?;
    // Connections accepted by an earlier listener on this port stay open
    // when it closes, and closed ones linger in TIME_WAIT. SO_REUSEADDR
    // lets the port listen again while they do; it never lets two
    // listeners share a port, so a squatted port still fails here.
    socket.set_reuseaddr(true)?;
    socket.bind(SocketAddr::from((Ipv4Addr::LOCALHOST, port)))?;
    socket.listen(BACKLOG)
}

/// Stop a listener and wait until it is gone. Aborting only schedules the
/// cancellation; the listener's socket closes when the task's future is
/// dropped, and awaiting the handle is what makes "not named = closed"
/// true by the time the RPC answers. Connections already accepted run in
/// tasks of their own and are not touched.
async fn close(task: JoinHandle<()>) {
    task.abort();
    let _ = task.await;
}

async fn accept_loop(listener: TcpListener, port: u16, conns: Arc<Conns>) {
    loop {
        match listener.accept().await {
            Ok((conn, _peer)) => conns.spawn(conn, port),
            Err(e) => {
                // Out of fds, or a connection aborted before it was
                // accepted. Neither ends the listener.
                log::info!("kube 127.0.0.1:{port}: accept: {e}");
                tokio::time::sleep(ACCEPT_BACKOFF).await;
            }
        }
    }
}

/// The one line that tells the proxy which binding this stream is for.
fn preamble(port: u16) -> String {
    format!("kube {port}\n")
}

/// One accepted connection: reach the proxy, then splice until both
/// directions are done.
async fn pipe(mut guest: TcpStream, port: u16, dialer: Arc<dyn HostDialer>) {
    // `exec` and `attach` carry keystrokes.
    let _ = guest.set_nodelay(true);

    let mut host = match connect_host(dialer.as_ref(), port).await {
        Ok(host) => host,
        Err(_) => return bad_gateway(guest).await,
    };

    // EOF on either side becomes a shutdown(Write) on the other, and the
    // opposite direction keeps flowing until it ends too. An error on
    // either side drops both.
    let _ = tokio::io::copy_bidirectional(&mut guest, &mut host).await;
}

/// Dial the proxy and announce the port. Nothing of the guest's has been
/// relayed yet, so a failure in either step is the same "proxy not
/// reachable".
async fn connect_host(dialer: &dyn HostDialer, port: u16) -> io::Result<Box<dyn HostIo>> {
    let mut host = tokio::time::timeout(DIAL_TIMEOUT, dialer.dial())
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "dial timed out"))??;
    host.write_all(preamble(port).as_bytes()).await?;
    host.flush().await?;
    Ok(host)
}

/// Answer the fixed 502 and close politely. The client has usually sent
/// its request already, and closing a socket with unread input makes the
/// kernel answer with a reset, which can destroy the response before the
/// client has read it. So: send, half-close, and drain until the client
/// closes too — briefly.
async fn bad_gateway(mut guest: TcpStream) {
    if guest.write_all(BAD_GATEWAY).await.is_err() {
        return;
    }
    let _ = guest.shutdown().await;
    let mut sink = [0u8; 4096];
    let _ = tokio::time::timeout(LINGER, async {
        while let Ok(n) = guest.read(&mut sink).await {
            if n == 0 {
                break;
            }
        }
    })
    .await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU32, Ordering};
    use tokio::net::{UnixListener, UnixStream};

    static SEQ: AtomicU32 = AtomicU32::new(0);

    /// Stands in for vsock: the proxy end of a guest stream is a Unix
    /// connection on the host anyway.
    struct UnixDialer(PathBuf);

    impl HostDialer for UnixDialer {
        fn dial(&self) -> DialFuture {
            let path = self.0.clone();
            Box::pin(async move {
                let s = UnixStream::connect(path).await?;
                Ok(Box::new(s) as Box<dyn HostIo>)
            })
        }
    }

    /// A socket path that is short enough for sun_path whatever TMPDIR is.
    fn sock_path() -> PathBuf {
        let p = PathBuf::from(format!(
            "/tmp/feos-kube-{}-{}.sock",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_file(&p);
        p
    }

    /// A fake kube-proxy: the listening end of `v.sock_10002`.
    fn fake_proxy() -> (Arc<KubeApiHandler>, UnixListener) {
        let path = sock_path();
        let proxy = UnixListener::bind(&path).expect("bind fake proxy");
        let svc = Arc::new(KubeApiHandler::with_dialer(Arc::new(UnixDialer(path))));
        (svc, proxy)
    }

    /// No proxy at all: the dial fails like a vsock connect with nobody
    /// listening on the host.
    fn no_proxy() -> Arc<KubeApiHandler> {
        Arc::new(KubeApiHandler::with_dialer(Arc::new(UnixDialer(
            sock_path(),
        ))))
    }

    /// A port nothing listens on right now. The kernel hands ephemeral
    /// ports out at random, so two tests colliding on one is unlikely
    /// enough.
    fn free_port() -> u32 {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        u32::from(l.local_addr().unwrap().port())
    }

    async fn set(svc: &KubeApiHandler, ports: &[u32]) -> SetListenersResponse {
        push(svc, ports, false).await
    }

    /// The push after a boot or resume.
    async fn set_and_reset(svc: &KubeApiHandler, ports: &[u32]) -> SetListenersResponse {
        push(svc, ports, true).await
    }

    async fn push(svc: &KubeApiHandler, ports: &[u32], reset: bool) -> SetListenersResponse {
        svc.set_listeners(Request::new(SetListenersRequest {
            ports: ports.to_vec(),
            reset_connections: reset,
        }))
        .await
        .expect("SetListeners never fails as a whole")
        .into_inner()
    }

    async fn within<F: Future>(f: F) -> F::Output {
        tokio::time::timeout(Duration::from_secs(5), f)
            .await
            .expect("timed out")
    }

    async fn connect(port: u32) -> io::Result<TcpStream> {
        TcpStream::connect(("127.0.0.1", port as u16)).await
    }

    /// Accept the guest stream on the fake proxy and check its preamble.
    async fn accept_stream(proxy: &UnixListener, port: u32) -> UnixStream {
        let (mut s, _) = within(proxy.accept()).await.unwrap();
        let want = format!("kube {port}\n");
        let mut got = vec![0u8; want.len()];
        within(s.read_exact(&mut got)).await.unwrap();
        assert_eq!(String::from_utf8(got).unwrap(), want);
        s
    }

    async fn task_id(svc: &KubeApiHandler, port: u32) -> tokio::task::Id {
        svc.0.live.lock().await[&(port as u16)].id()
    }

    #[test]
    fn the_preamble_is_one_line_naming_the_port() {
        assert_eq!(preamble(6443), "kube 6443\n");
        assert_eq!(preamble(1024), "kube 1024\n");
        assert_eq!(preamble(65535), "kube 65535\n");
    }

    #[test]
    fn ports_are_1024_to_65535() {
        assert_eq!(check_port(1024), Ok(1024));
        assert_eq!(check_port(65535), Ok(65535));
        for bad in [0, 80, 1023, 65536, u32::MAX] {
            assert!(check_port(bad).is_err(), "{bad} accepted");
        }
    }

    #[test]
    fn the_502_says_what_it_carries() {
        let text = std::str::from_utf8(BAD_GATEWAY).unwrap();
        let (head, body) = text.split_once("\r\n\r\n").unwrap();
        assert!(head.starts_with("HTTP/1.1 502 Bad Gateway\r\n"));
        assert!(head.contains(&format!("\r\nContent-Length: {}", body.len())));
        assert!(head.contains("\r\nConnection: close"));
    }

    #[tokio::test]
    async fn out_of_range_ports_fail_alone() {
        let svc = no_proxy();
        let ok = free_port();
        let resp = set(&svc, &[0, 80, ok, 1023, 65536, u32::MAX]).await;
        assert_eq!(resp.listening, vec![ok]);
        let failed: Vec<u32> = resp.failed.iter().map(|f| f.port).collect();
        assert_eq!(failed, vec![0, 80, 1023, 65536, u32::MAX]);
        assert!(resp.failed.iter().all(|f| f.reason.contains("1024-65535")));
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn the_set_is_whole_and_kept_listeners_are_not_rebound() {
        let svc = no_proxy();
        let (a, b) = (free_port(), free_port());
        let mut both = vec![a, b];
        both.sort();

        let resp = set(&svc, &[a]).await;
        assert_eq!(resp.listening, vec![a]);
        assert!(resp.failed.is_empty());
        let a_id = task_id(&svc, a).await;

        // Adding b keeps a's listener as it is — same accept loop.
        let resp = set(&svc, &[b, a, b]).await;
        assert_eq!(resp.listening, both);
        assert!(resp.failed.is_empty());
        assert_eq!(task_id(&svc, a).await, a_id);
        assert!(svc.ports().contains(a as u16) && svc.ports().contains(b as u16));

        // Whatever is not named is gone by the time the call answers.
        let resp = set(&svc, &[b]).await;
        assert_eq!(resp.listening, vec![b]);
        assert!(!svc.ports().contains(a as u16));
        let refused = connect(a).await.expect_err("a still listens");
        assert_eq!(refused.kind(), io::ErrorKind::ConnectionRefused);
        connect(b).await.expect("b listens");

        // The same set again changes nothing.
        let b_id = task_id(&svc, b).await;
        let resp = set(&svc, &[b]).await;
        assert_eq!(resp.listening, vec![b]);
        assert_eq!(task_id(&svc, b).await, b_id);

        let resp = set(&svc, &[]).await;
        assert!(resp.listening.is_empty());
        assert!(!svc.ports().contains(b as u16));
        assert!(connect(b).await.is_err());
    }

    #[tokio::test]
    async fn a_port_in_use_is_reported_not_fatal() {
        let svc = no_proxy();
        let squatter = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let taken = u32::from(squatter.local_addr().unwrap().port());
        let free = free_port();

        let resp = set(&svc, &[taken, free]).await;
        assert_eq!(resp.listening, vec![free]);
        assert_eq!(resp.failed.len(), 1);
        assert_eq!(resp.failed[0].port, taken);
        assert!(
            resp.failed[0]
                .reason
                .contains(&format!("127.0.0.1:{taken}")),
            "{}",
            resp.failed[0].reason
        );
        // Not ours, so not a kube port: forwarding it stays possible.
        assert!(!svc.ports().contains(taken as u16));

        // The next push retries; once the squatter is gone it listens.
        drop(squatter);
        let resp = set(&svc, &[taken, free]).await;
        let mut both = vec![taken, free];
        both.sort();
        assert_eq!(resp.listening, both);
        assert!(resp.failed.is_empty());
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn the_guest_half_close_reaches_the_proxy_and_the_answer_comes_back() {
        let (svc, proxy) = fake_proxy();
        let port = free_port();
        set(&svc, &[port]).await;

        let mut client = connect(port).await.unwrap();
        let mut host = accept_stream(&proxy, port).await;

        client
            .write_all(b"GET /version HTTP/1.1\r\nHost: x\r\n\r\n")
            .await
            .unwrap();
        client.shutdown().await.unwrap();

        // The proxy sees the request and then EOF — while its own
        // direction is still open.
        let mut req = Vec::new();
        within(host.read_to_end(&mut req)).await.unwrap();
        assert_eq!(req, b"GET /version HTTP/1.1\r\nHost: x\r\n\r\n");

        host.write_all(b"HTTP/1.1 200 OK\r\n\r\n").await.unwrap();
        host.shutdown().await.unwrap();
        let mut resp = Vec::new();
        within(client.read_to_end(&mut resp)).await.unwrap();
        assert_eq!(resp, b"HTTP/1.1 200 OK\r\n\r\n");
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn the_proxy_half_close_leaves_the_guest_direction_open() {
        let (svc, proxy) = fake_proxy();
        let port = free_port();
        set(&svc, &[port]).await;

        let mut client = connect(port).await.unwrap();
        let mut host = accept_stream(&proxy, port).await;

        host.write_all(b"first").await.unwrap();
        host.shutdown().await.unwrap();
        let mut got = Vec::new();
        within(client.read_to_end(&mut got)).await.unwrap();
        assert_eq!(got, b"first");

        // The client has seen EOF and can still talk.
        client.write_all(b"after").await.unwrap();
        client.shutdown().await.unwrap();
        let mut late = Vec::new();
        within(host.read_to_end(&mut late)).await.unwrap();
        assert_eq!(late, b"after");
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn without_a_proxy_the_client_gets_the_fixed_502() {
        let svc = no_proxy();
        let port = free_port();
        set(&svc, &[port]).await;

        let mut client = connect(port).await.unwrap();
        client
            .write_all(b"GET /api HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
            .await
            .unwrap();
        let mut resp = Vec::new();
        within(client.read_to_end(&mut resp)).await.unwrap();
        assert_eq!(resp, BAD_GATEWAY);
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn closing_a_listener_leaves_its_connections_alone() {
        let (svc, proxy) = fake_proxy();
        let port = free_port();
        set(&svc, &[port]).await;

        let mut client = connect(port).await.unwrap();
        let mut host = accept_stream(&proxy, port).await;

        let resp = set(&svc, &[]).await;
        assert!(resp.listening.is_empty());
        assert!(connect(port).await.is_err());

        client.write_all(b"ping").await.unwrap();
        let mut buf = [0u8; 4];
        within(host.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"ping");
        host.write_all(b"pong").await.unwrap();
        within(client.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"pong");

        // The port listens again while that connection is still open.
        let resp = set(&svc, &[port]).await;
        assert_eq!(resp.listening, vec![port]);
        assert!(resp.failed.is_empty(), "{:?}", resp.failed);
        let _second = connect(port).await.unwrap();
        accept_stream(&proxy, port).await;
        set(&svc, &[]).await;
    }
    /// The idle `kubectl get -w` across a pause: nothing moves on the
    /// connection, so only the reset can end it.
    #[tokio::test]
    async fn a_reset_ends_idle_connections_and_keeps_the_listeners() {
        let (svc, proxy) = fake_proxy();
        let port = free_port();
        set(&svc, &[port]).await;
        let listener = task_id(&svc, port).await;

        let mut idle = connect(port).await.unwrap();
        let mut old_host = accept_stream(&proxy, port).await;
        // A second one: a reset ends all of them, not just the first.
        let mut idle2 = connect(port).await.unwrap();
        let mut old_host2 = accept_stream(&proxy, port).await;

        let resp = set_and_reset(&svc, &[port]).await;
        assert_eq!(resp.listening, vec![port]);
        assert!(resp.failed.is_empty());

        // Both sides of both connections see EOF, at once — the call has
        // already waited for them to be closed.
        let mut buf = [0u8; 16];
        for client in [&mut idle, &mut idle2] {
            assert_eq!(within(client.read(&mut buf)).await.unwrap(), 0);
        }
        for host in [&mut old_host, &mut old_host2] {
            assert_eq!(within(host.read(&mut buf)).await.unwrap(), 0);
        }

        // The listener itself was kept, and the reconnect works.
        assert_eq!(task_id(&svc, port).await, listener);
        let mut fresh = connect(port).await.unwrap();
        let mut host = accept_stream(&proxy, port).await;
        fresh.write_all(b"ping").await.unwrap();
        let mut got = [0u8; 4];
        within(host.read_exact(&mut got)).await.unwrap();
        assert_eq!(&got, b"ping");
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn without_a_reset_open_connections_are_left_alone() {
        let (svc, proxy) = fake_proxy();
        let (a, b) = (free_port(), free_port());
        set(&svc, &[a]).await;

        let mut client = connect(a).await.unwrap();
        let mut host = accept_stream(&proxy, a).await;

        // A binding change while running: listeners change, connections
        // do not.
        let resp = set(&svc, &[a, b]).await;
        assert_eq!(resp.listening.len(), 2);

        client.write_all(b"ping").await.unwrap();
        let mut buf = [0u8; 4];
        within(host.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"ping");
        host.write_all(b"pong").await.unwrap();
        within(client.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"pong");
        set(&svc, &[]).await;
    }

    #[tokio::test]
    async fn a_reset_with_nothing_open_is_a_plain_reconcile() {
        let svc = no_proxy();
        let port = free_port();
        let resp = set_and_reset(&svc, &[port]).await;
        assert_eq!(resp.listening, vec![port]);
        let resp = set_and_reset(&svc, &[]).await;
        assert!(resp.listening.is_empty());
    }
}
