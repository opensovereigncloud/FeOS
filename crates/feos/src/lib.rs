// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

#[cfg(feature = "host")]
pub mod cgroup;
#[cfg(feature = "host")]
mod fleet;
#[cfg(feature = "guest")]
mod guest;
mod setup;
mod task_supervisor;
#[cfg(feature = "guest")]
mod volumes;

use anyhow::{Context, Result};
use container_service::api::ContainerApiHandler;
#[cfg(feature = "host")]
use feos_proto::network_service::{
    security_group_service_server::SecurityGroupServiceServer,
    traffic_service_server::TrafficServiceServer,
};
#[cfg(feature = "host")]
use feos_proto::pod_service::pod_service_server::PodServiceServer;
#[cfg(feature = "host")]
use feos_proto::volume_service::volume_service_server::VolumeServiceServer;
use feos_proto::{
    container_service::container_service_server::ContainerServiceServer,
    host_service::host_service_server::HostServiceServer,
    image_service::image_service_server::ImageServiceServer,
};
use host_service::{api::HostApiHandler, RestartSignal};
use image_service::{api::ImageApiHandler, IMAGE_SERVICE_SOCKET};
use log::{error, info, warn};
#[cfg(feature = "host")]
use network_service::SecurityGroups;
use nix::unistd::{chown, Group, Uid};
#[cfg(feature = "host")]
use pod_service::{forward::ForwardLayer, Pods, Volumes};
use setup::*;
use std::os::linux::net::SocketAddrExt;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{SocketAddr, UnixDatagram};
use std::path::Path;
use task_service::TASK_SERVICE_SOCKET;
use tokio::{fs, net::UnixListener, sync::mpsc};
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::server::Router;
use tonic::transport::Server;
use tower::layer::util::Identity;
#[cfg(feature = "host")]
use tower::layer::util::Stack;

/// The public gRPC API on a local Unix socket, served by feosd and
/// feos-guest. feos-guest serves the same API on vsock, feosd on TCP if
/// asked to.
pub const API_SOCKET: &str = "/run/feos/api.sock";
/// The group that may use the API socket, if it exists. The feosd package
/// creates it.
const API_GROUP: &str = "feos";

/// How feosd or feos-guest serves.
#[derive(Debug, Default)]
pub struct ServerOptions {
    /// This process is the one an upgrade exec'd: the one-time setup ran.
    pub restarted_after_upgrade: bool,
    /// UpgradeFeosBinary may replace this binary. A package manager owns
    /// feosd's binary unless feos-host-init runs it.
    pub self_upgrade: bool,
    /// Serves the API on TCP too, without authentication: for labs.
    #[cfg(feature = "host")]
    pub tcp: Option<std::net::SocketAddr>,
    /// The pods' network (ADR 0010).
    #[cfg(feature = "host")]
    pub network: pod_service::network::Config,
    /// The sockets handed to `feosd` (`feos_utils::listen_fds`): the
    /// resolver's, which outlive it.
    #[cfg(feature = "host")]
    pub dns_sockets: Vec<(std::os::fd::OwnedFd, String)>,
    /// diskd's socket, which the pods' disks come from (ADR 0011); its
    /// default when unset.
    #[cfg(feature = "host")]
    pub diskd_socket: Option<std::path::PathBuf>,
    /// The control plane (ADR 0015); none for a node of its own.
    #[cfg(feature = "host")]
    pub fleet: Option<fleet_client::Config>,
}

impl ServerOptions {
    /// Options without the TCP listener, and pods without a network.
    pub fn new(restarted_after_upgrade: bool, self_upgrade: bool) -> Self {
        Self {
            restarted_after_upgrade,
            self_upgrade,
            #[cfg(feature = "host")]
            tcp: None,
            #[cfg(feature = "host")]
            network: Default::default(),
            #[cfg(feature = "host")]
            dns_sockets: Vec::new(),
            #[cfg(feature = "host")]
            diskd_socket: None,
            #[cfg(feature = "host")]
            fleet: None,
        }
    }
}

pub async fn run_server(options: ServerOptions) -> Result<()> {
    println!(
        "
    ███████╗███████╗ ██████╗ ███████╗
    ██╔════╝██╔════╝██╔═══██╗██╔════╝
    █████╗  █████╗  ██║   ██║███████╗
    ██╔══╝  ██╔══╝  ██║   ██║╚════██║
    ██║     ███████╗╚██████╔╝███████║
    ╚═╝     ╚══════╝ ╚═════╝ ╚══════╝
                 v{}
    ",
        feos_utils::version::full_version_string()
    );

    let log_handle = feos_utils::feos_logger::Builder::new()
        .filter_level(log::LevelFilter::Info)
        .max_history(150)
        .init()
        .expect("Failed to initialize feos_logger");

    if !Uid::current().is_root() {
        warn!("Not running as root! (uid: {})", Uid::current());
    }

    // feos-guest is PID 1 of its VM and sets it up; feosd sets nothing up.
    #[cfg(feature = "guest")]
    if std::process::id() == 1 {
        feos_utils::host::power::take_over_ctrl_alt_del();
        if options.restarted_after_upgrade {
            info!("Main: Skipping one-time initialization on restart after upgrade.");
        } else {
            perform_first_boot_initialization();
        }
    }

    let (restart_tx, mut restart_rx) = mpsc::channel::<RestartSignal>(1);

    #[cfg(feature = "host")]
    let vms = initialize_vm_service(&setup_database().await?).await?;
    #[cfg(feature = "host")]
    let (pods, volumes, groups, traffic) = initialize_pod_service(
        vms,
        options.network,
        options.dns_sockets,
        options.diskd_socket,
    )
    .await?;
    let services = Services {
        #[cfg(feature = "host")]
        pods,
        #[cfg(feature = "host")]
        volumes,
        #[cfg(feature = "host")]
        groups,
        #[cfg(feature = "host")]
        traffic,
        container: initialize_container_service().await?,
        host: initialize_host_service(options.self_upgrade.then_some(restart_tx), log_handle),
        image: initialize_image_service().await?,
        #[cfg(feature = "guest")]
        guest: guest::GuestServices::new(),
    };
    task_supervisor::start().await;

    // Every listener serves the same services.
    let api_uds_stream = bind_unix_socket(API_SOCKET).await?;
    allow_group(API_SOCKET);
    let api_unix_socket_server = services.router().serve_with_incoming(api_uds_stream);
    info!("Main: Public gRPC Server listening on Unix socket {API_SOCKET}");

    #[cfg(feature = "guest")]
    let vsock_server = services
        .router()
        .serve_with_incoming(guest::vsock_incoming()?);
    #[cfg(feature = "guest")]
    info!(
        "Main: Public gRPC Server listening on vsock port {}",
        feos_utils::vsock::API_PORT
    );
    #[cfg(not(feature = "guest"))]
    let vsock_server = std::future::pending::<Result<(), tonic::transport::Error>>();

    #[cfg(feature = "host")]
    let tcp_server = {
        let incoming = match options.tcp {
            Some(addr) => {
                let listener = tokio::net::TcpListener::bind(addr)
                    .await
                    .with_context(|| format!("bind {addr}"))?;
                info!("Main: Public gRPC Server listening on {addr}, without authentication");
                Some(tokio_stream::wrappers::TcpListenerStream::new(listener))
            }
            None => None,
        };
        let router = services.router();
        async move {
            match incoming {
                Some(incoming) => router.serve_with_incoming(incoming).await,
                None => std::future::pending().await,
            }
        }
    };
    #[cfg(not(feature = "host"))]
    let tcp_server = std::future::pending::<Result<(), tonic::transport::Error>>();

    let image_uds_stream = bind_unix_socket(IMAGE_SERVICE_SOCKET).await?;
    let image_unix_socket_server = Server::builder()
        .add_service(services.image.clone())
        .serve_with_incoming(image_uds_stream);
    info!("Main: Internal ImageService listening on Unix socket {IMAGE_SERVICE_SOCKET}");

    // The listeners are bound: calls land in their backlog from here on.
    notify_ready();
    // Tell the host that the guest is ready.
    #[cfg(feature = "guest")]
    tokio::spawn(guest::run_uplink());

    // The control plane's connections, once the listeners serve: the node
    // connections serve the same services.
    #[cfg(feature = "host")]
    let (fleet, node_server) = {
        let started = options.fleet.and_then(|config| {
            fleet::start(config, services.pods.clone())
                .map_err(|e| error!("Fleet: Not connecting to the control plane: {e}"))
                .ok()
        });
        let (fleet, incoming) = started.unzip();
        let router = services.router_with(fleet_client::server());
        let server = async move {
            match incoming {
                // Never ends, whatever becomes of the dialers.
                Some(incoming) => {
                    let incoming =
                        tokio_stream::StreamExt::chain(incoming, tokio_stream::pending());
                    router.serve_with_incoming(incoming).await
                }
                None => std::future::pending().await,
            }
        };
        (fleet, server)
    };
    #[cfg(not(feature = "host"))]
    let node_server = std::future::pending::<Result<(), tonic::transport::Error>>();
    let mut node_serving = true;

    // With a control plane, SIGTERM tells it that feosd stops before feosd
    // ends; without, feosd ends as the signal ends it.
    #[cfg(feature = "host")]
    let terminated = {
        let fleet = fleet.clone();
        let signal = fleet.is_some().then(|| {
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .map_err(|e| warn!("Main: Failed to handle SIGTERM: {e}"))
                .ok()
        });
        async move {
            match (fleet, signal.flatten()) {
                (Some(fleet), Some(mut signal)) => {
                    signal.recv().await;
                    info!("Main: SIGTERM: feosd stops");
                    fleet.stopping("feosd stops").await;
                }
                _ => std::future::pending().await,
            }
        }
    };
    #[cfg(not(feature = "host"))]
    let terminated = std::future::pending::<()>();

    tokio::pin!(
        api_unix_socket_server,
        vsock_server,
        tcp_server,
        image_unix_socket_server,
        node_server,
        terminated
    );

    // Serve until one of the servers stops. A failed upgrade leaves the
    // running binary serving.
    loop {
        tokio::select! {
            res = &mut api_unix_socket_server => {
                if let Err(e) = res {
                    error!("API unix socket server failed: {e}");
                }
                break;
            },
            res = &mut vsock_server => {
                if let Err(e) = res {
                    error!("vsock server failed: {e}");
                }
                break;
            },
            res = &mut tcp_server => {
                if let Err(e) = res {
                    error!("TCP server failed: {e}");
                }
                break;
            },
            res = &mut image_unix_socket_server => {
                if let Err(e) = res {
                    error!("Image unix socket server failed: {e}");
                }
                break;
            },
            // The node connections never end feosd.
            res = &mut node_server, if node_serving => {
                node_serving = false;
                error!("Fleet: The node connections' server stopped: {res:?}");
            },
            () = &mut terminated => return Ok(()),
            Some(RestartSignal(new_binary_path)) = restart_rx.recv() => {
                #[cfg(feature = "host")]
                if let Some(fleet) = &fleet {
                    fleet.stopping("feosd restarts for an upgrade").await;
                }
                if let Err(e) = handle_upgrade(&new_binary_path) {
                    error!("Upgrade failed: {e}");
                }
            }
        };
    }

    anyhow::bail!("a gRPC server stopped serving")
}

/// The public API's services, one instance each for every listener.
struct Services {
    #[cfg(feature = "host")]
    pods: Pods,
    #[cfg(feature = "host")]
    volumes: Volumes,
    #[cfg(feature = "host")]
    groups: SecurityGroups,
    #[cfg(feature = "host")]
    traffic: traffic_service::TrafficApi,
    container: ContainerServiceServer<ContainerApiHandler>,
    host: HostServiceServer<HostApiHandler>,
    image: ImageServiceServer<ImageApiHandler>,
    #[cfg(feature = "guest")]
    guest: guest::GuestServices,
}

/// feosd forwards the calls for a pod's guest (ADR 0004).
#[cfg(feature = "host")]
type Layers = Stack<ForwardLayer, Identity>;
#[cfg(not(feature = "host"))]
type Layers = Identity;

impl Services {
    fn router(&self) -> Router<Layers> {
        self.router_with(Server::builder())
    }

    /// The services on `server`, as a listener configures it.
    fn router_with(&self, server: Server) -> Router<Layers> {
        #[cfg(feature = "host")]
        let mut server = server.layer(ForwardLayer::new(self.pods.clone()));
        #[cfg(not(feature = "host"))]
        let mut server = server;
        let router = server
            .add_service(self.container.clone())
            .add_service(self.host.clone())
            .add_service(self.image.clone());
        #[cfg(feature = "host")]
        let router = router
            .add_service(PodServiceServer::new(self.pods.clone()))
            .add_service(VolumeServiceServer::new(self.volumes.clone()))
            .add_service(SecurityGroupServiceServer::new(self.groups.clone()))
            .add_service(TrafficServiceServer::new(self.traffic.clone()));
        #[cfg(feature = "guest")]
        let router = self.guest.add_to(router);
        router
    }
}

/// Lets the group `feos` use a socket, if the group exists.
fn allow_group(path: &str) {
    let Ok(Some(group)) = Group::from_name(API_GROUP) else {
        return;
    };
    if let Err(e) = chown(path, None, Some(group.gid)) {
        warn!("Main: Failed to give {path} to group {API_GROUP}: {e}");
        return;
    }
    if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o660)) {
        warn!("Main: Failed to open {path} to group {API_GROUP}: {e}");
    }
}

/// Tells systemd that feosd serves (sd_notify READY=1): one datagram to
/// $NOTIFY_SOCKET, without libsystemd. Nothing to do without it.
fn notify_ready() {
    let Some(socket) = std::env::var_os("NOTIFY_SOCKET") else {
        return;
    };
    let socket = socket.as_bytes();
    // "@" names a socket in the abstract namespace.
    let address = match socket.strip_prefix(b"@") {
        Some(name) => SocketAddr::from_abstract_name(name),
        None => SocketAddr::from_pathname(Path::new(std::ffi::OsStr::from_bytes(socket))),
    };
    let sent = address.and_then(|address| {
        let datagram = UnixDatagram::unbound()?;
        datagram.send_to_addr(b"READY=1", &address)
    });
    if let Err(e) = sent {
        warn!("Main: Failed to notify systemd: {e}");
    }
}

/// The task service process, `feosd --task-service` or `feos-guest
/// --task-service`, which run_server starts.
pub async fn run_task_service() -> Result<()> {
    feos_utils::feos_logger::Builder::new()
        .filter_level(log::LevelFilter::Info)
        .max_history(150)
        .init()
        .context("logger")?;
    task_service::server::serve(TASK_SERVICE_SOCKET)
        .await
        .map_err(|e| anyhow::anyhow!("task service: {e}"))
}

/// Binds a Unix socket, creating its directory and replacing a stale socket.
async fn bind_unix_socket(path: &str) -> Result<UnixListenerStream> {
    if let Some(dir) = Path::new(path).parent() {
        fs::create_dir_all(dir)
            .await
            .with_context(|| format!("create {}", dir.display()))?;
    }
    fs::remove_file(path).await.ok();
    let listener = UnixListener::bind(path).with_context(|| format!("bind {path}"))?;
    Ok(UnixListenerStream::new(listener))
}
