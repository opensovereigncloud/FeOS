// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's side against a control plane of the test's own: one port,
//! TLS 1.3, its chain leaf, issuer and root, client certificates verified
//! when given. The uplink (`h2`) serves feos.fleet.v1, and on a node
//! connection (`feos-node/1`) the test calls the node's API.

use feos_proto::fleet_service::fleet_service_server::{FleetService, FleetServiceServer};
use feos_proto::fleet_service::{
    event, Certificate, ControlPlaneInstance, Event, EventAck, EventBatch,
    IssuePodCredentialsRequest, IssuePodCredentialsResponse, JoinRequest, JoinResponse,
    RenewCertificateRequest, RenewCertificateResponse, Telemetry, TelemetryResponse,
};
use feos_proto::pod_service::{PodEvent, PodInfo, PodState};
use feos_proto::volume_service::volume_service_client::VolumeServiceClient;
use feos_proto::volume_service::volume_service_server::{VolumeService, VolumeServiceServer};
use feos_proto::volume_service::{
    CreateVolumeRequest, DeleteVolumeRequest, DeleteVolumeResponse, GetVolumeRequest,
    ListVolumesRequest, ListVolumesResponse, Volume,
};
use fleet_client::{Config, Diskd, Installed, Inventory};
use hyper_util::rt::TokioIo;
use rcgen::{
    BasicConstraints, CertificateParams, CertificateSigningRequestParams, DnType,
    ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair, PublicKeyData, SanType,
};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::server::WebPkiClientVerifier;
use rustls::RootCertStore;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{Duration, SystemTime};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio::time::{sleep, Instant};
use tokio_rustls::server::TlsStream;
use tokio_rustls::TlsAcceptor;
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::Stream;
use tonic::transport::server::Connected;
use tonic::transport::{Endpoint, Server, Uri};
use tonic::{Request, Response, Status, Streaming};

const TRUST_DOMAIN: &str = "feos.local";
/// The name of the control plane's certificate; the test dials 127.0.0.1.
const NAME: &str = "ferun.feos.test";
const NODE: &str = "node-1";
const POD: &str = "3f2c0b8e-1d2a-4c5b-9e7f-0a1b2c3d4e5f";

fn key() -> KeyPair {
    KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap()
}

fn ca_params(name: &str) -> CertificateParams {
    let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
    params.distinguished_name.push(DnType::CommonName, name);
    params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    params
}

fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::Digest;
    sha2::Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn timestamp(t: SystemTime) -> Option<prost_types::Timestamp> {
    Some(t.into())
}

/// The platform CA: a root, and the issuer of the control plane's and the
/// nodes' certificates.
struct Pki {
    root: CertificateDer<'static>,
    /// The SHA-256 of the root's SubjectPublicKeyInfo, from its key.
    pin: String,
    issuer_cert: CertificateDer<'static>,
    issuer: Issuer<'static, KeyPair>,
}

impl Pki {
    fn new() -> Self {
        let root_key = key();
        let pin = sha256_hex(&root_key.subject_public_key_info());
        let root_params = ca_params("root");
        let root = root_params.self_signed(&root_key).unwrap().der().clone();
        let root_issuer = Issuer::new(root_params, root_key);
        let issuer_key = key();
        let issuer_params = ca_params("issuer");
        let issuer_cert = issuer_params
            .signed_by(&issuer_key, &root_issuer)
            .unwrap()
            .der()
            .clone();
        Self {
            root,
            pin,
            issuer_cert,
            issuer: Issuer::new(issuer_params, issuer_key),
        }
    }

    /// The control plane's server configuration: leaf, issuer and root.
    fn server_config(&self) -> Arc<rustls::ServerConfig> {
        let key = key();
        let mut params = CertificateParams::new(vec![NAME.to_string()]).unwrap();
        params.subject_alt_names.push(SanType::URI(
            format!("spiffe://{TRUST_DOMAIN}/ferun").try_into().unwrap(),
        ));
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        let leaf = params.signed_by(&key, &self.issuer).unwrap().der().clone();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let mut roots = RootCertStore::empty();
        roots.add(self.root.clone()).unwrap();
        let clients =
            WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider.clone())
                .allow_unauthenticated()
                .build()
                .unwrap();
        let mut config = rustls::ServerConfig::builder_with_provider(provider)
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_client_cert_verifier(clients)
            .with_single_cert(
                vec![leaf, self.issuer_cert.clone(), self.root.clone()],
                PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
            )
            .unwrap();
        config.alpn_protocols = vec![b"feos-node/1".to_vec(), b"h2".to_vec()];
        Arc::new(config)
    }

    /// A node's certificate for the key of `csr`.
    fn issue(&self, csr: &[u8], name: &str, renew_after: SystemTime) -> Certificate {
        let mut csr = CertificateSigningRequestParams::from_der(&csr.to_vec().into()).unwrap();
        let spiffe_id = format!("spiffe://{TRUST_DOMAIN}/node/{name}");
        csr.params.subject_alt_names = vec![SanType::URI(spiffe_id.clone().try_into().unwrap())];
        csr.params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        let leaf = csr.signed_by(&self.issuer).unwrap();
        let now = SystemTime::now();
        Certificate {
            spiffe_id,
            chain: vec![leaf.der().to_vec(), self.issuer_cert.to_vec()],
            trust_bundle: vec![self.root.to_vec()],
            not_before: timestamp(now - Duration::from_secs(600)),
            not_after: timestamp(now + Duration::from_secs(3600)),
            renew_after: timestamp(renew_after),
        }
    }
}

/// What the test control plane saw.
#[derive(Default)]
struct Seen {
    joins: Vec<JoinRequest>,
    renewals: usize,
    /// The certificate issued last.
    issued: Vec<u8>,
    /// Each report, with the certificate it came with.
    reports: Vec<(Vec<u8>, Telemetry)>,
    events: Vec<Event>,
    last_seq: u64,
    /// ListVolumes answered on a node connection, with its certificate.
    volumes: Vec<(Vec<u8>, String)>,
    /// Refuse the node from now on.
    refuse: bool,
}

struct ControlPlane {
    pki: Arc<Pki>,
    secret: String,
    address: String,
    seen: Arc<Mutex<Seen>>,
}

/// The connection's client certificate, for the handlers.
#[derive(Clone)]
struct Peer(Option<Vec<u8>>);

struct Uplink(TlsStream<TcpStream>);

impl Connected for Uplink {
    type ConnectInfo = Peer;

    fn connect_info(&self) -> Peer {
        Peer(
            self.0
                .get_ref()
                .1
                .peer_certificates()
                .and_then(|certs| certs.first())
                .map(|cert| cert.to_vec()),
        )
    }
}

impl AsyncRead for Uplink {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.0).poll_read(cx, buf)
    }
}

impl AsyncWrite for Uplink {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        Pin::new(&mut self.0).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.0).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.0).poll_shutdown(cx)
    }
}

impl ControlPlane {
    fn peer<T>(&self, request: &Request<T>) -> Result<Vec<u8>, Status> {
        if self.seen.lock().unwrap().refuse {
            return Err(Status::permission_denied(format!(
                "node {NODE} was removed; join it again"
            )));
        }
        request
            .extensions()
            .get::<Peer>()
            .and_then(|peer| peer.0.clone())
            .ok_or_else(|| Status::unauthenticated("no client certificate"))
    }

    fn instances(&self) -> Vec<ControlPlaneInstance> {
        vec![ControlPlaneInstance {
            id: "i1".into(),
            node_address: self.address.clone(),
        }]
    }
}

#[tonic::async_trait]
impl FleetService for ControlPlane {
    type PublishEventsStream = Pin<Box<dyn Stream<Item = Result<EventAck, Status>> + Send>>;

    async fn join(&self, request: Request<JoinRequest>) -> Result<Response<JoinResponse>, Status> {
        let request = request.into_inner();
        self.seen.lock().unwrap().joins.push(request.clone());
        if request.token != self.secret {
            return Err(Status::permission_denied("unknown join token"));
        }
        // Renewed at once.
        let certificate = self
            .pki
            .issue(&request.csr, &request.name, SystemTime::now());
        {
            let mut seen = self.seen.lock().unwrap();
            seen.issued = certificate.chain[0].clone();
            seen.refuse = false;
        }
        Ok(Response::new(JoinResponse {
            name: request.name,
            certificate: Some(certificate),
            instances: self.instances(),
        }))
    }

    async fn renew_certificate(
        &self,
        request: Request<RenewCertificateRequest>,
    ) -> Result<Response<RenewCertificateResponse>, Status> {
        self.peer(&request)?;
        let later = SystemTime::now() + Duration::from_secs(3000);
        let certificate = self.pki.issue(&request.get_ref().csr, NODE, later);
        let mut seen = self.seen.lock().unwrap();
        seen.renewals += 1;
        seen.issued = certificate.chain[0].clone();
        Ok(Response::new(RenewCertificateResponse {
            certificate: Some(certificate),
        }))
    }

    async fn report_telemetry(
        &self,
        request: Request<Telemetry>,
    ) -> Result<Response<TelemetryResponse>, Status> {
        let peer = self.peer(&request)?;
        self.seen
            .lock()
            .unwrap()
            .reports
            .push((peer, request.into_inner()));
        Ok(Response::new(TelemetryResponse {
            interval_seconds: 1,
            instances: self.instances(),
        }))
    }

    async fn publish_events(
        &self,
        request: Request<Streaming<EventBatch>>,
    ) -> Result<Response<Self::PublishEventsStream>, Status> {
        self.peer(&request)?;
        let mut batches = request.into_inner();
        let seen = self.seen.clone();
        let (tx, rx) = mpsc::channel(4);
        tokio::spawn(async move {
            while let Ok(Some(batch)) = batches.message().await {
                let seq = {
                    let mut seen = seen.lock().unwrap();
                    for event in batch.events {
                        if event.seq > seen.last_seq {
                            seen.last_seq = event.seq;
                            seen.events.push(event);
                        }
                    }
                    seen.last_seq
                };
                let ack = EventAck {
                    start_id: batch.start_id,
                    seq,
                };
                if tx.send(Ok(ack)).await.is_err() {
                    return;
                }
            }
        });
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    async fn issue_pod_credentials(
        &self,
        _request: Request<IssuePodCredentialsRequest>,
    ) -> Result<Response<IssuePodCredentialsResponse>, Status> {
        Err(Status::unimplemented("not in this test"))
    }
}

/// Starts the test control plane; its token and its address.
async fn control_plane(pki: Arc<Pki>, seen: Arc<Mutex<Seen>>) -> (String, String) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let secret = format!("feos-join-{}", "Q".repeat(43));
    let fleet = FleetServiceServer::new(ControlPlane {
        pki: pki.clone(),
        secret: secret.clone(),
        address: address.clone(),
        seen: seen.clone(),
    });
    let (uplinks, incoming) = mpsc::channel::<Result<Uplink, std::io::Error>>(16);
    tokio::spawn(
        Server::builder()
            .add_service(fleet)
            .serve_with_incoming(ReceiverStream::new(incoming)),
    );
    let acceptor = TlsAcceptor::from(pki.server_config());
    tokio::spawn(async move {
        loop {
            let (tcp, _) = listener.accept().await.unwrap();
            let acceptor = acceptor.clone();
            let uplinks = uplinks.clone();
            let seen = seen.clone();
            tokio::spawn(async move {
                // A node that refuses the server ends the handshake.
                let Ok(tls) = acceptor.accept(tcp).await else {
                    return;
                };
                match tls.get_ref().1.alpn_protocol() {
                    Some(b"h2") => {
                        uplinks.send(Ok(Uplink(tls))).await.ok();
                    }
                    Some(b"feos-node/1") => call_node(tls, seen).await,
                    _ => {}
                }
            });
        }
    });
    (format!("{secret}.{}", pki.pin), address)
}

/// The control plane's side of a node connection: it is the HTTP/2 client
/// on the connection the node opened.
async fn call_node(tls: TlsStream<TcpStream>, seen: Arc<Mutex<Seen>>) {
    let peer = tls
        .get_ref()
        .1
        .peer_certificates()
        .and_then(|certs| certs.first())
        .map(|cert| cert.to_vec())
        .unwrap_or_default();
    let stream = Mutex::new(Some(tls));
    let channel = Endpoint::from_static("http://node")
        .connect_with_connector(tower::service_fn(move |_: Uri| {
            let stream = stream.lock().unwrap().take();
            async move {
                stream
                    .map(TokioIo::new)
                    .ok_or_else(|| std::io::Error::other("the node connection was used"))
            }
        }))
        .await
        .unwrap();
    let mut client = VolumeServiceClient::new(channel.clone());
    let volumes = client.list_volumes(ListVolumesRequest {}).await;
    if let Ok(volumes) = volumes {
        for volume in volumes.into_inner().volumes {
            seen.lock()
                .unwrap()
                .volumes
                .push((peer.clone(), volume.volume_id));
        }
    }
    // The connection stays, as the control plane's do.
    sleep(Duration::from_secs(600)).await;
    drop(channel);
}

/// The node's volumes, as its API answers on the node connection.
struct Volumes;

#[tonic::async_trait]
impl VolumeService for Volumes {
    async fn create_volume(
        &self,
        _request: Request<CreateVolumeRequest>,
    ) -> Result<Response<Volume>, Status> {
        Err(Status::unimplemented("not in this test"))
    }

    async fn delete_volume(
        &self,
        _request: Request<DeleteVolumeRequest>,
    ) -> Result<Response<DeleteVolumeResponse>, Status> {
        Err(Status::unimplemented("not in this test"))
    }

    async fn get_volume(
        &self,
        _request: Request<GetVolumeRequest>,
    ) -> Result<Response<Volume>, Status> {
        Err(Status::unimplemented("not in this test"))
    }

    async fn list_volumes(
        &self,
        _request: Request<ListVolumesRequest>,
    ) -> Result<Response<ListVolumesResponse>, Status> {
        Ok(Response::new(ListVolumesResponse {
            volumes: vec![Volume {
                volume_id: "v1".into(),
                ..Default::default()
            }],
        }))
    }
}

struct Node;

impl Inventory for Node {
    fn pods(&self) -> Vec<PodInfo> {
        vec![PodInfo {
            pod_id: POD.into(),
            state: PodState::Running as i32,
            ..Default::default()
        }]
    }

    fn installed(&self) -> Installed {
        Installed {
            firecracker: vec!["1.12.0".into()],
            guest: vec!["0.6.0".into()],
        }
    }

    async fn diskd(&self) -> Option<Diskd> {
        Some(Diskd {
            object_store: true,
            object_store_id: "abc".into(),
            version: "0.6.0".into(),
        })
    }
}

/// Starts the node's side with the token in `dir`, serving `Volumes` on
/// its node connections.
fn node(address: &str, dir: &Path) -> fleet_client::Fleet {
    let mut config = Config::new(address, Some(NAME), TRUST_DOMAIN).unwrap();
    config.join_token_file = dir.join("join-token");
    config.node_dir = dir.join("node");
    config.node_name = Some(NODE.into());
    config.pod_prefix = "fd00:fe05:1::/64".into();
    config.version = "0.6.0".into();
    let (fleet, incoming) = fleet_client::start(config, Node).unwrap();
    tokio::spawn(
        fleet_client::server()
            .add_service(VolumeServiceServer::new(Volumes))
            .serve_with_incoming(incoming),
    );
    fleet
}

/// Waits until `done` holds of what the control plane saw.
async fn wait_for(seen: &Mutex<Seen>, what: &str, done: impl Fn(&Seen) -> bool) {
    let deadline = Instant::now() + Duration::from_secs(30);
    while !done(&seen.lock().unwrap()) {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        sleep(Duration::from_millis(50)).await;
    }
}

fn uri_of(cert: &[u8]) -> Vec<String> {
    let (_, cert) = x509_parser::parse_x509_certificate(cert).unwrap();
    cert.subject_alternative_name()
        .unwrap()
        .map(|names| {
            names
                .value
                .general_names
                .iter()
                .filter_map(|name| match name {
                    x509_parser::extensions::GeneralName::URI(uri) => Some(uri.to_string()),
                    _ => None,
                })
                .collect()
        })
        .unwrap_or_default()
}

#[tokio::test]
async fn a_node_joins_reports_renews_and_serves_its_api() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let fleet = node(&address, dir.path());

    // The join: the secret alone, the node's name, a signing request.
    wait_for(&seen, "the join", |seen| !seen.joins.is_empty()).await;
    let join = seen.lock().unwrap().joins[0].clone();
    assert_eq!(join.token, token.split_once('.').unwrap().0);
    assert_eq!(join.name, NODE);
    assert!(!join.csr.is_empty());

    // The identity is kept, the key for root alone.
    let node_dir = dir.path().join("node");
    wait_for(&seen, "a renewal", |seen| seen.renewals > 0).await;
    for file in ["key.pem", "chain.pem", "bundle.pem", "node.json"] {
        assert!(node_dir.join(file).exists(), "{file}");
    }
    let mode = |path: &Path| std::fs::metadata(path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&node_dir), 0o700);
    assert_eq!(mode(&node_dir.join("key.pem")), 0o600);

    // Telemetry, with the renewed certificate.
    wait_for(&seen, "a report with the renewed certificate", |seen| {
        seen.reports.iter().any(|(cert, _)| *cert == seen.issued)
    })
    .await;
    let (cert, report) = seen.lock().unwrap().reports.last().unwrap().clone();
    assert_eq!(
        uri_of(&cert),
        [format!("spiffe://{TRUST_DOMAIN}/node/{NODE}")]
    );
    assert!(uuid::Uuid::parse_str(&report.start_id).is_ok());
    assert_eq!(report.pods_running, 1);
    assert_eq!(
        report.inventory_hash,
        sha256_hex(format!("{POD} POD_STATE_RUNNING\n").as_bytes())
    );
    assert_eq!(report.pod_prefix, "fd00:fe05:1::/64");
    assert!(report.object_store);
    assert_eq!(report.object_store_id, "abc");
    let versions = report.versions.unwrap();
    assert_eq!(versions.feosd, "0.6.0");
    assert_eq!(versions.firecracker, ["1.12.0"]);
    assert_eq!(versions.diskd, "0.6.0");
    assert!(report.memory_total_bytes > 0);
    let state = std::fs::read_to_string(node_dir.join("node.json")).unwrap();
    assert!(state.contains(&address), "{state}");

    // The node's API on a node connection, with the renewed certificate.
    wait_for(&seen, "ListVolumes on a node connection", |seen| {
        seen.volumes
            .iter()
            .any(|(cert, volume)| *cert == seen.issued && volume == "v1")
    })
    .await;

    // Events: feosd started, then a pod's; acknowledged.
    fleet.events().pod(PodEvent {
        pod_id: POD.into(),
        state: PodState::Running as i32,
        ..Default::default()
    });
    wait_for(&seen, "the pod's event", |seen| seen.events.len() == 2).await;
    {
        let seen = seen.lock().unwrap();
        let seqs: Vec<u64> = seen.events.iter().map(|event| event.seq).collect();
        assert_eq!(seqs, [1, 2]);
        assert!(matches!(
            &seen.events[0].body,
            Some(event::Body::Node(node)) if node.kind == "started"
        ));
        assert!(matches!(&seen.events[1].body, Some(event::Body::Pod(_))));
    }
    fleet.stopping("restart").await;
    assert!(fleet.events().flush(Duration::from_millis(1)).await);
    assert_eq!(seen.lock().unwrap().events.len(), 3);
}

#[tokio::test]
async fn a_refused_node_waits_for_a_new_token() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    let token_file = dir.path().join("join-token");
    // A token that pins another root: the node never sends it.
    let (secret, _) = token.split_once('.').unwrap();
    std::fs::write(&token_file, format!("{secret}.{}", "0".repeat(64))).unwrap();
    let _fleet = node(&address, dir.path());
    sleep(Duration::from_secs(2)).await;
    assert!(seen.lock().unwrap().joins.is_empty());

    std::fs::write(&token_file, &token).unwrap();
    wait_for(&seen, "a report", |seen| !seen.reports.is_empty()).await;

    // Removed: the identity goes aside, and the node waits for a new token.
    seen.lock().unwrap().refuse = true;
    let node_dir = dir.path().join("node");
    let deadline = Instant::now() + Duration::from_secs(30);
    while node_dir.join("node.json").exists() {
        assert!(Instant::now() < deadline, "the identity was not set aside");
        sleep(Duration::from_millis(50)).await;
    }
    let aside: Vec<_> = std::fs::read_dir(&node_dir)
        .unwrap()
        .flatten()
        .filter(|entry| entry.file_name().to_string_lossy().starts_with("removed-"))
        .collect();
    assert_eq!(aside.len(), 1);
    assert!(aside[0].path().join("key.pem").exists());
    sleep(Duration::from_secs(2)).await;
    assert_eq!(seen.lock().unwrap().joins.len(), 1);

    // A new token joins it again.
    let secret = format!("feos-join-{}", "R".repeat(43));
    std::fs::write(
        &token_file,
        format!("{secret}.{}", token.split_once('.').unwrap().1),
    )
    .unwrap();
    wait_for(&seen, "the second join", |seen| seen.joins.len() == 2).await;
    assert_eq!(seen.lock().unwrap().joins[1].token, secret);
}
