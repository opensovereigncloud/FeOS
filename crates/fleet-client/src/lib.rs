// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's side of the control plane (ADR 0015).
//!
//! The node joins once with a join token and keeps its key and
//! certificate in its directory. From then on it holds a node connection to
//! every instance of the control plane, on which it serves feosd's API as
//! the HTTP/2 server, and uses an uplink to one instance at a time for
//! feos.fleet.v1: its telemetry, the renewal of its certificate and its
//! events. Nothing here stops feosd: whatever fails is logged and tried
//! again, while feosd serves its socket.

mod events;
mod identity;
mod node;
mod telemetry;
mod tls;
mod token;
mod uplink;

pub use events::Events;
pub use identity::NODE_DIR;
pub use node::{server, Incoming, NodeConn};
pub use telemetry::{inventory_hash, Diskd, Installed, Inventory};
pub use token::{check_trust_domain, JoinToken};

use chrono::Utc;
use feos_proto::fleet_service::{JoinMeshRequest, JoinRequest, RenewCertificateRequest};
use identity::{Identity, Instance, NodeDir};
use log::{error, info, warn};
use node::Nodes;
use rustls::crypto::CryptoProvider;
use rustls::ClientConfig;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;
use telemetry::Collector;
use tokio::sync::watch;
use tokio::time::{sleep, timeout};
use tonic::transport::Channel;
use tonic::Code;
use uplink::{describe, Link, Uplink};

/// The join token, `feosd --join-token-file`; read only while the node has
/// not joined.
pub const JOIN_TOKEN_FILE: &str = "/etc/feos/join-token";
/// The control plane's default trust domain.
pub const TRUST_DOMAIN: &str = "feos.local";

/// The wait between reports until the control plane names one.
const FIRST_INTERVAL: Duration = Duration::from_secs(5);
/// A report's deadline (fleet.proto).
const REPORT_DEADLINE: Duration = Duration::from_secs(3);
const JOIN_DEADLINE: Duration = Duration::from_secs(10);
const RENEW_DEADLINE: Duration = Duration::from_secs(10);
const JOIN_MESH_DEADLINE: Duration = Duration::from_secs(10);
/// How often a node that waits for a join token looks at its file.
const TOKEN_POLL: Duration = Duration::from_secs(5);
const JOIN_BACKOFF: (Duration, Duration) = (Duration::from_secs(1), Duration::from_secs(60));
const RENEW_BACKOFF: (Duration, Duration) = (Duration::from_secs(10), Duration::from_secs(600));
/// How long feosd waits for the control plane to acknowledge that it
/// stops.
const STOPPING_WAIT: Duration = Duration::from_secs(3);

/// How feosd reaches the control plane.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Config {
    /// `--control-plane`: the address of its port for nodes, `host:port`,
    /// which the join dials, and the node connections and the uplink while
    /// the control plane lists no instances.
    pub address: String,
    /// `--control-plane-name`: the DNS name its certificate is checked
    /// for, wherever feosd dials.
    pub name: String,
    pub trust_domain: String,
    pub join_token_file: PathBuf,
    pub node_dir: PathBuf,
    /// The name the node proposes when it joins; its hostname when unset.
    pub node_name: Option<String>,
    /// The node's IPv6 prefix for pods, such as "2001:db8:1:2::/64", or
    /// empty.
    pub pod_prefix: String,
    /// `--mesh-address`: where the other nodes reach the node's mesh; the
    /// address from which it reaches the control plane when unset.
    pub mesh_address: Option<std::net::IpAddr>,
    /// feosd's version.
    pub version: String,
}

impl Config {
    /// The control plane at `address`, `host:port`, whose certificate names
    /// `name`, or the host when that is a DNS name.
    pub fn new(address: &str, name: Option<&str>, trust_domain: &str) -> Result<Self, String> {
        let (host, port) = address
            .rsplit_once(':')
            .ok_or_else(|| format!("{address:?} is not HOST:PORT"))?;
        if port.parse::<u16>().map_or(true, |port| port == 0) {
            return Err(format!("{address:?} is not HOST:PORT"));
        }
        let bracketed = host
            .strip_prefix('[')
            .and_then(|host| host.strip_suffix(']'));
        let ip = match bracketed {
            Some(v6) => match v6.parse::<std::net::Ipv6Addr>() {
                Ok(_) => true,
                Err(_) => return Err(format!("{address:?} is not HOST:PORT")),
            },
            None if host.contains(':') => {
                return Err(format!("{address:?}: an IPv6 address goes in brackets"))
            }
            None => host.parse::<std::net::IpAddr>().is_ok(),
        };
        if host.is_empty() {
            return Err(format!("{address:?} is not HOST:PORT"));
        }
        let name = match name {
            Some(name) => name,
            None if ip => {
                return Err(format!(
                    "{address:?} is an address; the name of the control plane's certificate \
                     goes in --control-plane-name"
                ))
            }
            None => host,
        };
        let is_dns_name = matches!(
            rustls::pki_types::ServerName::try_from(name),
            Ok(rustls::pki_types::ServerName::DnsName(_))
        );
        if !is_dns_name {
            return Err(format!(
                "{name:?} is not a DNS name, which the control plane's certificate names"
            ));
        }
        check_trust_domain(trust_domain)?;
        Ok(Self {
            address: address.to_string(),
            name: name.to_string(),
            trust_domain: trust_domain.to_string(),
            join_token_file: JOIN_TOKEN_FILE.into(),
            node_dir: NODE_DIR.into(),
            node_name: None,
            pod_prefix: String::new(),
            mesh_address: None,
            version: String::new(),
        })
    }
}

/// feosd's handle on its side of the control plane.
#[derive(Clone)]
pub struct Fleet {
    events: Events,
}

impl Fleet {
    /// The node's events, which go to the control plane.
    pub fn events(&self) -> &Events {
        &self.events
    }

    /// Tells the control plane that feosd stops, for a restart or an
    /// upgrade, so that it waits a little longer for the node to come
    /// back; waits a moment for it to be acknowledged.
    pub async fn stopping(&self, message: &str) {
        self.events.node("stopping", message);
        if !self.events.flush(STOPPING_WAIT).await {
            warn!("Fleet: The control plane did not acknowledge that feosd stops");
        }
    }
}

/// Starts the node's side of the control plane: the join if the node has
/// not joined, then the uplink and the node connections, which the caller
/// serves its API on.
pub fn start<I: Inventory>(config: Config, inventory: I) -> Result<(Fleet, Incoming), String> {
    let link = Arc::new(Link::new(&config.name)?);
    let start_id = uuid::Uuid::new_v4().to_string();
    let events = Events::new(&start_id);
    events.node("started", &format!("feosd {}", config.version));
    let (nodes, incoming) = Nodes::new(link.name.clone());
    let (uplink, published) = watch::channel(None);
    tokio::spawn(events::publish(events.clone(), published));
    let collector = Collector::new(inventory, &start_id, &config.version, &config.pod_prefix);
    info!(
        "Fleet: The control plane is at {} as {}, trust domain {}",
        config.address, config.name, config.trust_domain
    );
    let runner = Runner {
        config,
        provider: tls::provider(),
        link,
        nodes,
        collector,
        uplink,
        spread: std::hash::BuildHasher::hash_one(
            &std::collections::hash_map::RandomState::new(),
            &start_id,
        ) as usize,
    };
    tokio::spawn(runner.run());
    Ok((Fleet { events }, incoming))
}

struct Runner<I> {
    config: Config,
    provider: Arc<CryptoProvider>,
    link: Arc<Link>,
    nodes: Nodes,
    collector: Collector<I>,
    /// The uplink's channel, for the events' stream.
    uplink: watch::Sender<Option<Channel>>,
    /// Picks the instance the uplink starts with.
    spread: usize,
}

/// Why the node stopped using its identity.
enum Outcome {
    /// The control plane refused it: the node has to join again.
    Refused(String),
    /// It does not work here; it is loaded again.
    Broken(String),
}

/// Why a join failed.
enum JoinError {
    /// Nothing to join with: no token, or the one that was refused.
    NoToken(String),
    /// Refused for good: the node waits for a new token.
    Refused(String, String),
    /// Failed for now: tried again.
    Failed(String),
}

impl<I: Inventory> Runner<I> {
    async fn run(mut self) {
        let dir = loop {
            match NodeDir::open(&self.config.node_dir) {
                Ok(dir) => break dir,
                Err(e) => {
                    error!(
                        "Fleet: Failed to open the node's directory {}: {e}",
                        self.config.node_dir.display()
                    );
                    sleep(JOIN_BACKOFF.1).await;
                }
            }
        };
        // The token in the file when the node was last refused: a join
        // waits for another.
        let mut refused = None;
        loop {
            let identity = self.identity(&dir, &mut refused).await;
            match self.serve(&dir, identity).await {
                Outcome::Refused(message) => {
                    self.nodes.clear();
                    self.uplink.send_replace(None);
                    error!(
                        "Fleet: The control plane refused the node ({message}); it has to \
                         join again with a new join token in {}",
                        self.config.join_token_file.display()
                    );
                    match dir.set_aside() {
                        Ok(aside) => warn!(
                            "Fleet: The node's refused identity is kept in {}",
                            aside.display()
                        ),
                        Err(e) => error!("Fleet: Failed to set the node's identity aside: {e}"),
                    }
                    refused = std::fs::read_to_string(&self.config.join_token_file).ok();
                }
                Outcome::Broken(message) => {
                    error!("Fleet: The node's identity does not work: {message}");
                    sleep(JOIN_BACKOFF.1).await;
                }
            }
        }
    }

    /// The node's identity: the one it has, or a new one from a join.
    async fn identity(&mut self, dir: &NodeDir, refused: &mut Option<String>) -> Identity {
        let mut wait = JOIN_BACKOFF.0;
        let mut said = String::new();
        loop {
            let error = match dir.load() {
                Ok(Some(identity)) => return identity,
                Ok(None) => match self.join(dir, refused.as_deref()).await {
                    Ok(identity) => return identity,
                    Err(e) => e,
                },
                Err(e) => JoinError::Failed(format!(
                    "the node's identity in {} does not load: {e}",
                    dir.path().display()
                )),
            };
            match error {
                // Said once, until something else happens.
                JoinError::NoToken(message) => {
                    if message != said {
                        error!("Fleet: The node has not joined the control plane: {message}");
                        said = message;
                    }
                    sleep(TOKEN_POLL).await;
                }
                JoinError::Refused(token, message) => {
                    error!(
                        "Fleet: {message}; the node waits for a new join token in {}",
                        self.config.join_token_file.display()
                    );
                    *refused = Some(token);
                    wait = JOIN_BACKOFF.0;
                    sleep(TOKEN_POLL).await;
                }
                JoinError::Failed(message) => {
                    error!(
                        "Fleet: Failed to join the control plane: {message}; trying again in {}s",
                        wait.as_secs()
                    );
                    said.clear();
                    sleep(wait).await;
                    wait = (wait * 2).min(JOIN_BACKOFF.1);
                }
            }
        }
    }

    async fn join(&self, dir: &NodeDir, refused: Option<&str>) -> Result<Identity, JoinError> {
        let file = self.config.join_token_file.display();
        let text = match std::fs::read_to_string(&self.config.join_token_file) {
            Ok(text) => text,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Err(JoinError::NoToken(format!(
                    "there is no join token in {file}"
                )))
            }
            Err(e) => {
                return Err(JoinError::NoToken(format!(
                    "the join token in {file} does not read: {e}"
                )))
            }
        };
        if refused == Some(text.as_str()) {
            return Err(JoinError::NoToken(format!(
                "the join token in {file} was refused; it waits for a new one"
            )));
        }
        let token: JoinToken = text
            .parse()
            .map_err(|e| JoinError::Refused(text.clone(), format!("{file}: {e}")))?;
        let name = self.proposed_name().map_err(JoinError::Failed)?;
        let key = dir.join_key().map_err(|e| {
            JoinError::Failed(format!("the node's key in {}: {e}", dir.path().display()))
        })?;
        let csr = identity::csr(&key, &name).map_err(JoinError::Failed)?;
        let config = tls::join_config(&self.provider, &token.pin, &self.config.trust_domain)
            .map_err(|e| JoinError::Failed(e.to_string()))?;
        let mut client = feos_proto::fleet_service::fleet_service_client::FleetServiceClient::new(
            self.link.channel(&self.config.address, config),
        );
        info!(
            "Fleet: Joining the control plane at {} as {name}",
            self.config.address
        );
        let request = JoinRequest {
            token: token.secret.clone(),
            name,
            csr,
        };
        let response = match timeout(JOIN_DEADLINE, client.join(request)).await {
            Ok(Ok(response)) => response.into_inner(),
            Ok(Err(status))
                if matches!(
                    status.code(),
                    Code::PermissionDenied | Code::AlreadyExists | Code::InvalidArgument
                ) =>
            {
                return Err(JoinError::Refused(
                    text,
                    format!("the control plane refused the join: {}", status.message()),
                ))
            }
            Ok(Err(status)) => return Err(JoinError::Failed(describe(&status))),
            Err(_) => return Err(JoinError::Failed("the join timed out".into())),
        };
        let Some(certificate) = response.certificate else {
            return Err(JoinError::Failed(
                "the control plane answered without a certificate".into(),
            ));
        };
        let pinned = certificate
            .trust_bundle
            .iter()
            .any(|root| token::pin(root).is_ok_and(|pin| pin == token.pin));
        if !pinned {
            return Err(JoinError::Failed(
                "the control plane's trust bundle does not hold the root that the join token pins"
                    .into(),
            ));
        }
        let instances = response.instances.iter().map(Instance::from).collect();
        let identity = Identity::issued(
            key,
            certificate,
            &response.name,
            &self.config.trust_domain,
            instances,
            &self.provider,
        )
        .map_err(|e| JoinError::Failed(format!("the control plane's answer: {e}")))?;
        dir.store(&identity).map_err(|e| {
            JoinError::Failed(format!(
                "keeping the node's identity in {}: {e}",
                dir.path().display()
            ))
        })?;
        info!(
            "Fleet: Joined the control plane as {}, with a certificate until {}",
            identity.state.spiffe_id, identity.state.not_after
        );
        Ok(identity)
    }

    /// The name to join as: the hostname, as a node's name has to be.
    fn proposed_name(&self) -> Result<String, String> {
        let name = match &self.config.node_name {
            Some(name) => name.clone(),
            None => nix::unistd::gethostname()
                .map_err(|e| format!("the hostname: {e}"))?
                .to_string_lossy()
                .to_lowercase(),
        };
        if !token::is_dns_label(&name) {
            return Err(format!(
                "the hostname {name:?} is no node's name: 1 to 63 lowercase letters, digits \
                 and \"-\", beginning and ending with a letter or a digit"
            ));
        }
        Ok(name)
    }

    /// The uplink's and the node connections' configurations.
    fn configs(
        &self,
        identity: &Identity,
    ) -> Result<(Arc<ClientConfig>, Arc<ClientConfig>), String> {
        let config = |alpn| {
            tls::node_config(&self.provider, identity, &self.config.trust_domain, alpn)
                .map_err(|e| format!("the node's certificate: {e}"))
        };
        Ok((config(tls::ALPN_UPLINK)?, config(tls::ALPN_NODE)?))
    }

    /// Where the node connections go: every instance the control plane
    /// lists, or every address of the one feosd was configured with while
    /// it lists none. Those of `before` stay when the address resolves to
    /// nothing.
    async fn node_targets(&self, instances: &[Instance], before: &[String]) -> Vec<String> {
        let listed: Vec<String> = instances
            .iter()
            .map(|instance| instance.node_address.clone())
            .filter(|address| !address.is_empty())
            .collect();
        if !listed.is_empty() {
            return listed;
        }
        let resolved: Vec<String> = match tokio::net::lookup_host(&self.config.address).await {
            Ok(addresses) => addresses.map(|address| address.to_string()).collect(),
            Err(e) => {
                warn!("Fleet: Failed to resolve {}: {e}", self.config.address);
                Vec::new()
            }
        };
        match resolved.is_empty() {
            false => resolved,
            true if before.is_empty() => vec![self.config.address.clone()],
            true => before.to_vec(),
        }
    }

    /// The uplink's instances, in order: those listed, from one of them
    /// that this node picked, so that the nodes' uplinks spread over the
    /// instances; and the configured address last, which reaches the
    /// control plane even when every address it listed is gone.
    fn uplink_targets(&self, instances: &[Instance]) -> Vec<String> {
        let mut targets: Vec<String> = instances
            .iter()
            .map(|instance| instance.node_address.clone())
            .filter(|address| !address.is_empty())
            .collect();
        if !targets.is_empty() {
            let first = self.spread % targets.len();
            targets.rotate_left(first);
        }
        if !targets.contains(&self.config.address) {
            targets.push(self.config.address.clone());
        }
        targets
    }

    /// Serves with `identity` until the control plane refuses it.
    async fn serve(&mut self, dir: &NodeDir, mut identity: Identity) -> Outcome {
        let (uplink_config, node_config) = match self.configs(&identity) {
            Ok(configs) => configs,
            Err(e) => return Outcome::Broken(e),
        };
        info!(
            "Fleet: The node is {}, with a certificate until {}",
            identity.state.spiffe_id, identity.state.not_after
        );
        let mut targets = self.node_targets(&identity.state.instances, &[]).await;
        self.nodes.renew(node_config);
        self.nodes.set(&targets);
        let mut uplink = Uplink::new(
            self.link.clone(),
            uplink_config,
            self.uplink_targets(&identity.state.instances),
            self.uplink.clone(),
        );
        let mut interval = FIRST_INTERVAL;
        let mut renew_at = identity.renew_after;
        let mut renew_wait = RENEW_BACKOFF.0;
        // The last JoinMesh the control plane answered: asked again when
        // the node's key or endpoint changes.
        let mut joined: Option<JoinMeshRequest> = None;
        loop {
            if Utc::now() >= renew_at {
                match self.renew(dir, &identity, &mut uplink).await {
                    Ok(renewed) => match self.configs(&renewed) {
                        Ok((uplink_config, node_config)) => {
                            info!(
                                "Fleet: Renewed the node's certificate, until {}",
                                renewed.state.not_after
                            );
                            identity = renewed;
                            renew_at = identity.renew_after;
                            renew_wait = RENEW_BACKOFF.0;
                            uplink.set_config(uplink_config);
                            self.nodes.renew(node_config);
                        }
                        Err(e) => return Outcome::Broken(e),
                    },
                    Err(Outcome::Refused(message)) => return Outcome::Refused(message),
                    Err(Outcome::Broken(message)) => {
                        error!(
                            "Fleet: Failed to renew the node's certificate: {message}; trying \
                             again in {}s",
                            renew_wait.as_secs()
                        );
                        if Utc::now() >= identity.not_after {
                            error!(
                                "Fleet: The node's certificate expired at {}; once the control \
                                 plane refuses it, the node has to join again",
                                identity.state.not_after
                            );
                        }
                        renew_at =
                            Utc::now() + chrono::Duration::from_std(renew_wait).unwrap_or_default();
                        renew_wait = (renew_wait * 2).min(RENEW_BACKOFF.1);
                    }
                }
            }

            let report = self.collector.collect().await;
            let answer = uplink
                .call("ReportTelemetry", REPORT_DEADLINE, |mut client| {
                    let report = report.clone();
                    async move { client.report_telemetry(report).await }
                })
                .await;
            match answer {
                Ok(answer) => {
                    interval = match answer.interval_seconds {
                        0 => FIRST_INTERVAL,
                        seconds => Duration::from_secs(seconds.min(300).into()),
                    };
                    let instances: Vec<Instance> =
                        answer.instances.iter().map(Instance::from).collect();
                    if instances != identity.state.instances {
                        let listed: Vec<&str> = instances
                            .iter()
                            .map(|instance| instance.node_address.as_str())
                            .collect();
                        info!("Fleet: The control plane's instances are at {listed:?}");
                        identity.state.instances = instances;
                        if let Err(e) = dir.store_state(&identity.state) {
                            warn!("Fleet: Failed to keep the control plane's instances: {e}");
                        }
                    }
                    let now = self.node_targets(&identity.state.instances, &targets).await;
                    if now != targets {
                        targets = now;
                        self.nodes.set(&targets);
                    }
                    uplink.set_targets(self.uplink_targets(&identity.state.instances));
                    if let Err(outcome) = self.join_mesh(&mut uplink, &mut joined).await {
                        return outcome;
                    }
                }
                Err(status) if status.code() == Code::PermissionDenied => {
                    return Outcome::Refused(status.message().to_string())
                }
                // The uplink said why.
                Err(_) => {}
            }
            let until_renewal = (renew_at - Utc::now()).to_std().unwrap_or_default();
            sleep(interval.min(until_renewal.max(Duration::from_secs(1)))).await;
        }
    }

    /// Asks for the node's place on the cluster's mesh (ADR 0017) when it
    /// has not been answered for the node's key and endpoint, and hands the
    /// answer to feosd. A node with a prefix of its network's sends no key:
    /// it asks only whether the cluster is meshed.
    async fn join_mesh(
        &self,
        uplink: &mut Uplink,
        joined: &mut Option<JoinMeshRequest>,
    ) -> Result<(), Outcome> {
        let request = match self.collector.inventory().mesh_key() {
            Some((public_key, port)) => match self.mesh_endpoint(port).await {
                Ok(endpoint) => JoinMeshRequest {
                    public_key: public_key.to_vec(),
                    endpoint: endpoint.to_string(),
                },
                Err(e) => {
                    warn!("Fleet: The node's endpoint on the mesh: {e}");
                    return Ok(());
                }
            },
            None => JoinMeshRequest::default(),
        };
        if joined.as_ref() == Some(&request) {
            return Ok(());
        }
        let answer = uplink
            .call("JoinMesh", JOIN_MESH_DEADLINE, |mut client| {
                let request = request.clone();
                async move { client.join_mesh(request).await }
            })
            .await;
        match answer {
            Ok(answer) => {
                if !request.public_key.is_empty() {
                    info!(
                        "Fleet: On the mesh at {}: {} of {}",
                        request.endpoint,
                        or_none(&answer.pod_prefix),
                        or_none(&answer.pod_network)
                    );
                }
                match self
                    .collector
                    .inventory()
                    .joined_mesh(answer.pod_network, answer.pod_prefix)
                    .await
                {
                    Ok(()) => *joined = Some(request),
                    Err(e) => error!("Fleet: The mesh's answer is not taken, asking again: {e}"),
                }
            }
            Err(status) if status.code() == Code::PermissionDenied => {
                return Err(Outcome::Refused(status.message().to_string()))
            }
            Err(status) if status.code() == Code::Unimplemented => {
                warn!("Fleet: The control plane knows no mesh: an older ferund");
                *joined = Some(request);
            }
            // Asked again with the next report; the uplink said why.
            Err(_) => {}
        }
        Ok(())
    }

    /// Where the other nodes reach the node's mesh: `--mesh-address`, or
    /// the address from which the node reaches the control plane, which a
    /// connected UDP socket tells without sending anything.
    async fn mesh_endpoint(&self, port: u16) -> Result<std::net::SocketAddr, String> {
        if let Some(address) = self.config.mesh_address {
            return Ok(std::net::SocketAddr::new(address, port));
        }
        let control_plane = tokio::net::lookup_host(&self.config.address)
            .await
            .map_err(|e| format!("resolving {}: {e}", self.config.address))?
            .next()
            .ok_or_else(|| format!("{} resolves to nothing", self.config.address))?;
        let unspecified: std::net::SocketAddr = match control_plane {
            std::net::SocketAddr::V4(_) => (std::net::Ipv4Addr::UNSPECIFIED, 0).into(),
            std::net::SocketAddr::V6(_) => (std::net::Ipv6Addr::UNSPECIFIED, 0).into(),
        };
        let socket = std::net::UdpSocket::bind(unspecified).map_err(|e| e.to_string())?;
        socket
            .connect(control_plane)
            .map_err(|e| format!("no route to the control plane at {control_plane}: {e}"))?;
        let local = socket.local_addr().map_err(|e| e.to_string())?;
        Ok(std::net::SocketAddr::new(local.ip(), port))
    }

    /// A new certificate for the node's key, kept before it is used.
    async fn renew(
        &self,
        dir: &NodeDir,
        identity: &Identity,
        uplink: &mut Uplink,
    ) -> Result<Identity, Outcome> {
        let csr = identity::csr(&identity.key, &identity.state.name).map_err(Outcome::Broken)?;
        let answer = uplink
            .call("RenewCertificate", RENEW_DEADLINE, |mut client| {
                let csr = csr.clone();
                async move {
                    client
                        .renew_certificate(RenewCertificateRequest { csr })
                        .await
                }
            })
            .await;
        let answer = match answer {
            Ok(answer) => answer,
            Err(status) if status.code() == Code::PermissionDenied => {
                return Err(Outcome::Refused(status.message().to_string()))
            }
            Err(status) => return Err(Outcome::Broken(describe(&status))),
        };
        let certificate = answer.certificate.ok_or_else(|| {
            Outcome::Broken("the control plane answered without a certificate".into())
        })?;
        let key = rcgen::KeyPair::try_from(identity.key.serialize_der())
            .map_err(|e| Outcome::Broken(format!("the node's key: {e}")))?;
        let renewed = Identity::issued(
            key,
            certificate,
            &identity.state.name,
            &self.config.trust_domain,
            identity.state.instances.clone(),
            &self.provider,
        )
        .map_err(|e| Outcome::Broken(format!("the control plane's answer: {e}")))?;
        dir.store(&renewed).map_err(|e| {
            Outcome::Broken(format!(
                "keeping the renewed certificate in {}: {e}",
                dir.path().display()
            ))
        })?;
        Ok(renewed)
    }
}

fn or_none(value: &str) -> &str {
    match value {
        "" => "none",
        value => value,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_control_plane_is_configured() {
        let config = Config::new("ferun.example.com:7443", None, "feos.local").unwrap();
        assert_eq!(config.address, "ferun.example.com:7443");
        assert_eq!(config.name, "ferun.example.com");
        assert_eq!(config.join_token_file, PathBuf::from(JOIN_TOKEN_FILE));
        assert_eq!(config.node_dir, PathBuf::from(NODE_DIR));
        // An address needs the name its certificate carries.
        let config =
            Config::new("10.43.0.17:7443", Some("ferun.feos.test"), "run.fecode.dev").unwrap();
        assert_eq!(config.address, "10.43.0.17:7443");
        assert_eq!(config.name, "ferun.feos.test");
        assert!(Config::new("[fd00::1]:7443", Some("ferun.feos.test"), "feos.local").is_ok());
        for (address, name, domain) in [
            ("10.43.0.17:7443", None, "feos.local"),
            ("[fd00::1]:7443", None, "feos.local"),
            ("fd00::1:7443", Some("ferun.feos.test"), "feos.local"),
            ("ferun.example.com", None, "feos.local"),
            ("ferun.example.com:", None, "feos.local"),
            ("ferun.example.com:0", None, "feos.local"),
            ("ferun.example.com:99999", None, "feos.local"),
            (":7443", Some("ferun.feos.test"), "feos.local"),
            ("10.43.0.17:7443", Some("10.43.0.17"), "feos.local"),
            ("ferun.example.com:7443", None, "Feos.Local"),
        ] {
            assert!(
                Config::new(address, name, domain).is_err(),
                "{address} {name:?} {domain}"
            );
        }
    }
}
