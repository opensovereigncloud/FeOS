// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! PortForwardService — inbound connections to a guest TCP port.
//!
//! One RPC per connection: the caller opens the stream, names the port,
//! and from then on the two halves are spliced. FeOS understands nothing
//! of what rides it — HTTP, a WebSocket after an upgrade, a terminal — so
//! there is no framing to keep in sync and nothing to parse.
//!
//! Loopback is up as soon as the host has pushed the network config
//! (NetworkService), which it does after every boot and every resume.
//! Containers run without their own network namespace, so a server bound
//! to 127.0.0.1 inside the container is reachable here.
//!
//! One kind of port is never forwarded: the loopback listeners FeOS
//! itself holds for Kubernetes bindings (`crate::kube`). Each
//! of them adds a credential on the way out; forwarding one would hand
//! that to whoever reaches the front door.

use std::pin::Pin;

use feos_proto::portforward_service::forward_request::Frame as ReqFrame;
use feos_proto::portforward_service::forward_response::Frame as RespFrame;
use feos_proto::portforward_service::port_forward_service_server::PortForwardService;
use feos_proto::portforward_service::{ForwardRequest, ForwardResponse, Ready};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::mpsc;
use tokio_stream::wrappers::UnboundedReceiverStream;
use tonic::{Request, Response, Status, Streaming};

use crate::kube::KubePorts;

/// Read chunk towards the caller. Matches Exec's frame size: big enough
/// for a dev server's asset bursts, small enough to keep a terminal's
/// keystrokes moving.
const CHUNK: usize = 16 * 1024;

pub struct PortForwardApiHandler {
    /// The kube listener set, shared with `KubeApiHandler`.
    kube: KubePorts,
}

impl PortForwardApiHandler {
    pub fn new(kube: KubePorts) -> Self {
        PortForwardApiHandler { kube }
    }

    /// The port an `open` names, if it may be forwarded.
    fn admit(&self, port: u32) -> Result<u16, Status> {
        let port: u16 = port
            .try_into()
            .map_err(|_| Status::invalid_argument(format!("port {port} out of range")))?;
        if port == 0 {
            return Err(Status::invalid_argument("port 0 is not a port"));
        }
        self.refuse_kube(port)?;
        Ok(port)
    }

    fn refuse_kube(&self, port: u16) -> Result<(), Status> {
        if self.kube.contains(port) {
            return Err(Status::failed_precondition(format!(
                "port {port} is a kube listener"
            )));
        }
        Ok(())
    }
}

#[tonic::async_trait]
impl PortForwardService for PortForwardApiHandler {
    type ForwardStream =
        Pin<Box<dyn tokio_stream::Stream<Item = Result<ForwardResponse, Status>> + Send>>;

    async fn forward(
        &self,
        req: Request<Streaming<ForwardRequest>>,
    ) -> Result<Response<Self::ForwardStream>, Status> {
        let mut inbound = req.into_inner();
        let port = match inbound.message().await? {
            Some(ForwardRequest {
                frame: Some(ReqFrame::Open(open)),
            }) => open.port,
            Some(_) => return Err(Status::invalid_argument("first frame must be `open`")),
            None => return Err(Status::invalid_argument("stream ended before `open`")),
        };
        let port = self.admit(port)?;

        // Dial before answering: "nothing listens there" has to reach the
        // caller as a failed RPC, not as a silent stream.
        let conn = TcpStream::connect(("127.0.0.1", port))
            .await
            .map_err(|e| Status::unavailable(format!("127.0.0.1:{port}: {e}")))?;
        // Checked again now that the dial is done: a kube listener bound
        // while it was under way must not slip through. KubeApiHandler enters a
        // port before binding it, so if this connection reached one, its
        // port is in the set by now. Dropping `conn` closes it.
        self.refuse_kube(port)?;
        // Small writes are keystrokes and hot-reload pings, not bulk.
        let _ = conn.set_nodelay(true);
        let (mut guest_rd, mut guest_wr) = conn.into_split();

        let (tx, rx) = mpsc::unbounded_channel();
        let _ = tx.send(Ok(ForwardResponse {
            frame: Some(RespFrame::Ready(Ready {})),
        }));

        // guest → caller. Ends the response stream when the guest closes
        // (Ok(0)) or the caller went away (send fails).
        let out = tx.clone();
        tokio::spawn(async move {
            let mut buf = vec![0u8; CHUNK];
            loop {
                match guest_rd.read(&mut buf).await {
                    Ok(0) => break,
                    Ok(n) => {
                        let frame = ForwardResponse {
                            frame: Some(RespFrame::Data(buf[..n].to_vec())),
                        };
                        if out.send(Ok(frame)).is_err() {
                            break;
                        }
                    }
                    Err(e) => {
                        let _ = out.send(Err(Status::internal(format!("read from guest: {e}"))));
                        break;
                    }
                }
            }
        });

        // caller → guest. A finished request stream is a half-close, which
        // is what an HTTP client without keep-alive does; anything else
        // drops the connection.
        tokio::spawn(async move {
            loop {
                match inbound.message().await {
                    Ok(Some(ForwardRequest {
                        frame: Some(ReqFrame::Data(data)),
                    })) => {
                        if guest_wr.write_all(&data).await.is_err() {
                            break;
                        }
                    }
                    // A second `open`, or an empty frame: ignore rather
                    // than tear down a working connection.
                    Ok(Some(_)) => continue,
                    Ok(None) => {
                        let _ = guest_wr.shutdown().await;
                        break;
                    }
                    Err(_) => break,
                }
            }
        });

        Ok(Response::new(Box::pin(UnboundedReceiverStream::new(rx))))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kube::KubeApiHandler;
    use feos_proto::kube_service::kube_service_server::KubeService;
    use feos_proto::kube_service::SetListenersRequest;

    async fn set(svc: &KubeApiHandler, ports: &[u32]) {
        svc.set_listeners(Request::new(SetListenersRequest {
            ports: ports.to_vec(),
            reset_connections: false,
        }))
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn a_kube_listener_is_not_forwardable() {
        let kube = KubeApiHandler::default();
        let fwd = PortForwardApiHandler::new(kube.ports());
        let port = {
            let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            l.local_addr().unwrap().port()
        };

        // Not (yet) a kube port: admitted.
        assert_eq!(fwd.admit(u32::from(port)).unwrap(), port);

        set(&kube, &[u32::from(port)]).await;
        let refused = fwd.admit(u32::from(port)).unwrap_err();
        assert_eq!(refused.code(), tonic::Code::FailedPrecondition);
        assert_eq!(refused.message(), format!("port {port} is a kube listener"));

        // Out of the set again, forwardable again.
        set(&kube, &[]).await;
        assert_eq!(fwd.admit(u32::from(port)).unwrap(), port);
    }

    #[test]
    fn ports_that_are_no_ports_are_invalid() {
        let fwd = PortForwardApiHandler::new(KubePorts::default());
        for bad in [0, 65536, u32::MAX] {
            assert_eq!(
                fwd.admit(bad).unwrap_err().code(),
                tonic::Code::InvalidArgument
            );
        }
    }
}
