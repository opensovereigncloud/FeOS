// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

#[cfg(feature = "guest")]
mod guest;
mod setup;
mod task_supervisor;

use anyhow::{Context, Result};
use host_service::RestartSignal;
use image_service::IMAGE_SERVICE_SOCKET;
use log::{error, info, warn};
use nix::unistd::Uid;
use setup::*;
use std::path::Path;
use task_service::TASK_SERVICE_SOCKET;
use tokio::{fs, net::UnixListener, sync::mpsc};
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

/// The public gRPC API on a local Unix socket, served in every profile. The
/// guest profile serves the same API on vsock, the host profile on TCP.
pub const API_SOCKET: &str = "/run/feos/api.sock";

pub async fn run_server(restarted_after_upgrade: bool) -> Result<()> {
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

    dotenvy::dotenv().ok();

    let mut boot_info = BootInfo::default();

    if !restarted_after_upgrade {
        if std::process::id() == 1 {
            boot_info = perform_first_boot_initialization().await?;
        }
    } else {
        info!("Main: Skipping one-time initialization on restart after upgrade.");
    }

    let (restart_tx, mut restart_rx) = mpsc::channel::<RestartSignal>(1);

    #[cfg(feature = "host")]
    let vm_service = initialize_vm_service(&setup_database().await?).await?;
    let container_service = initialize_container_service().await?;

    let host_service = initialize_host_service(restart_tx.clone(), log_handle, boot_info);

    let image_service = initialize_image_service().await?;
    task_supervisor::start().await;

    let api_uds_stream = bind_unix_socket(API_SOCKET).await?;
    let api_unix_socket_server = Server::builder()
        .add_service(container_service.clone())
        .add_service(host_service.clone())
        .add_service(image_service.clone())
        .serve_with_incoming(api_uds_stream);

    #[cfg(feature = "guest")]
    let vsock_server = Server::builder()
        .add_service(container_service.clone())
        .add_service(host_service.clone())
        .add_service(image_service.clone())
        .serve_with_incoming(guest::vsock_incoming()?);
    #[cfg(not(feature = "guest"))]
    let vsock_server = std::future::pending::<Result<(), tonic::transport::Error>>();

    #[cfg(feature = "host")]
    let tcp_addr: std::net::SocketAddr = "[::]:1337".parse().unwrap();
    #[cfg(feature = "host")]
    let tcp_server = Server::builder()
        .add_service(vm_service)
        .add_service(container_service)
        .add_service(host_service)
        .serve(tcp_addr);
    #[cfg(not(feature = "host"))]
    let tcp_server = std::future::pending::<Result<(), tonic::transport::Error>>();

    let image_uds_stream = bind_unix_socket(IMAGE_SERVICE_SOCKET).await?;
    let image_unix_socket_server = Server::builder()
        .add_service(image_service)
        .serve_with_incoming(image_uds_stream);

    info!("Main: Public gRPC Server listening on Unix socket {API_SOCKET}");
    #[cfg(feature = "guest")]
    info!(
        "Main: Public gRPC Server listening on vsock port {}",
        feos_utils::vsock::API_PORT
    );
    #[cfg(feature = "host")]
    info!("Main: Public gRPC Server listening on {tcp_addr}");
    info!("Main: Internal ImageService listening on Unix socket {IMAGE_SERVICE_SOCKET}");

    // Tell the host that the guest is ready. The listeners are bound, so the
    // host's first call lands in their backlog.
    #[cfg(feature = "guest")]
    tokio::spawn(guest::run_uplink());

    tokio::pin!(
        api_unix_socket_server,
        vsock_server,
        tcp_server,
        image_unix_socket_server
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
            Some(RestartSignal(new_binary_path)) = restart_rx.recv() => {
                if let Err(e) = handle_upgrade(&new_binary_path) {
                    error!("Upgrade failed: {e}");
                }
            }
        };
    }

    anyhow::bail!("a gRPC server stopped serving")
}

/// The task service process, `feos --task-service`, which run_server starts.
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
