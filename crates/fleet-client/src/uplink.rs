// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The uplink (ADR 0015): feos.fleet.v1 to one instance of the control
//! plane at a time, moving to the next one at once when a call fails.

use crate::tls;
use feos_proto::fleet_service::fleet_service_client::FleetServiceClient;
use hyper_util::rt::TokioIo;
use log::{debug, info, warn};
use rustls::pki_types::ServerName;
use rustls::ClientConfig;
use std::future::Future;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::watch;
use tokio::time::timeout;
use tonic::transport::{Channel, Endpoint, Uri};
use tonic::{Response, Status};
use tower::service_fn;

/// How the control plane is reached.
pub(crate) struct Link {
    /// The name its certificate is checked for.
    pub(crate) name: ServerName<'static>,
    endpoint: Endpoint,
}

impl Link {
    pub(crate) fn new(name: &str) -> Result<Self, String> {
        let server_name = ServerName::try_from(name.to_string())
            .map_err(|e| format!("the control plane's name {name:?}: {e}"))?;
        // The URI names the control plane; the connector dials the address
        // and does TLS itself. tonic's own TLS stays off: with it, an https
        // URI would want tonic's TLS configuration.
        let endpoint = Endpoint::from_shared(format!("https://{name}"))
            .map_err(|e| format!("the control plane's name {name:?}: {e}"))?
            .http2_keep_alive_interval(Duration::from_secs(10))
            .keep_alive_timeout(Duration::from_secs(5))
            .keep_alive_while_idle(true);
        Ok(Self {
            name: server_name,
            endpoint,
        })
    }

    /// A channel to the instance at `address`, which connects when it is
    /// first used, and again after it lost its connection.
    pub(crate) fn channel(&self, address: &str, config: Arc<ClientConfig>) -> Channel {
        let address = address.to_string();
        let name = self.name.clone();
        self.endpoint
            .connect_with_connector_lazy(service_fn(move |_: Uri| {
                let address = address.clone();
                let name = name.clone();
                let config = config.clone();
                async move {
                    tls::connect(&address, &name, config)
                        .await
                        .map(TokioIo::new)
                }
            }))
    }
}

/// The uplink: a channel to one of the instances, which the events'
/// stream follows.
pub(crate) struct Uplink {
    link: Arc<Link>,
    config: Arc<ClientConfig>,
    /// `host:port` of the instances, in the order they are tried.
    targets: Vec<String>,
    current: usize,
    channel: Channel,
    published: watch::Sender<Option<Channel>>,
    failing: bool,
}

impl Uplink {
    pub(crate) fn new(
        link: Arc<Link>,
        config: Arc<ClientConfig>,
        targets: Vec<String>,
        published: watch::Sender<Option<Channel>>,
    ) -> Self {
        let channel = link.channel(targets.first().map_or("", String::as_str), config.clone());
        published.send_replace(Some(channel.clone()));
        Self {
            link,
            config,
            targets,
            current: 0,
            channel,
            published,
            failing: false,
        }
    }

    fn target(&self) -> &str {
        self.targets.get(self.current).map_or("", String::as_str)
    }

    /// A new channel to the current target, for the calls and the events'
    /// stream; the old one closes when they let go of it.
    fn reconnect(&mut self) {
        self.channel = self.link.channel(self.target(), self.config.clone());
        self.published.send_replace(Some(self.channel.clone()));
    }

    /// The instances to use, in order. The current one stays if it is
    /// listed.
    pub(crate) fn set_targets(&mut self, targets: Vec<String>) {
        if targets == self.targets {
            return;
        }
        let kept = targets.iter().position(|target| target == self.target());
        self.targets = targets;
        match kept {
            Some(current) => self.current = current,
            None => {
                self.current = 0;
                self.reconnect();
            }
        }
    }

    /// Uses a renewed certificate: a new uplink, to which the events'
    /// stream moves.
    pub(crate) fn set_config(&mut self, config: Arc<ClientConfig>) {
        self.config = config;
        self.reconnect();
    }

    /// Calls the current instance, and on a failure each of the others in
    /// turn, each with `deadline`. PERMISSION_DENIED says that the node
    /// has to join again, whichever instance says it, and is not retried.
    pub(crate) async fn call<T, F, Fut>(
        &mut self,
        what: &str,
        deadline: Duration,
        mut call: F,
    ) -> Result<T, Status>
    where
        F: FnMut(FleetServiceClient<Channel>) -> Fut,
        Fut: Future<Output = Result<Response<T>, Status>>,
    {
        let mut last = Status::unavailable("the control plane has no instance to call");
        for _ in 0..self.targets.len().max(1) {
            let client = FleetServiceClient::new(self.channel.clone());
            let status = match timeout(deadline, call(client)).await {
                Ok(Ok(response)) => {
                    if self.failing {
                        info!("Fleet: The uplink to {} works", self.target());
                        self.failing = false;
                    }
                    return Ok(response.into_inner());
                }
                Ok(Err(status)) => status,
                Err(_) => {
                    Status::deadline_exceeded(format!("no answer within {}s", deadline.as_secs()))
                }
            };
            if status.code() == tonic::Code::PermissionDenied {
                return Err(status);
            }
            let failed = self.target().to_string();
            self.current = (self.current + 1) % self.targets.len().max(1);
            self.reconnect();
            let message = format!(
                "Fleet: {what} on {failed} failed: {}; the uplink moves to {}",
                describe(&status),
                self.target()
            );
            if self.failing {
                debug!("{message}");
            } else {
                warn!("{message}");
            }
            last = status;
        }
        self.failing = true;
        Err(last)
    }
}

/// A status with the errors beneath it that it does not say already, such
/// as why a handshake failed.
pub(crate) fn describe(status: &Status) -> String {
    let mut text = format!("{:?}: {}", status.code(), status.message());
    let mut source = std::error::Error::source(status);
    while let Some(e) = source {
        let said = e.to_string();
        if !text.contains(&said) {
            text.push_str(&format!(": {said}"));
        }
        source = e.source();
    }
    text
}
