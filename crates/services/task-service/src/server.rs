// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The task service as a process of its own (`feos --task-service`). It is
//! the subreaper of the containers it creates, so their processes become its
//! children and its reaper collects their exit codes.

use crate::api::TaskApiHandler;
use crate::dispatcher::Dispatcher;
use crate::{Command, RUNTIME_STATE_DIR};
use feos_proto::task_service::task_service_server::TaskServiceServer;
use log::info;
use std::path::Path;
use tokio::net::UnixListener;
use tokio::sync::mpsc;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

/// Serves the task service on the Unix socket `socket` until the server
/// fails. Call once, in a tokio runtime of its own process.
pub async fn serve(socket: &str) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    die_with_parent()?;
    nix::sys::prctl::set_child_subreaper(true)?;
    feos_utils::reaper::spawn();
    std::fs::create_dir_all(RUNTIME_STATE_DIR)?;

    let (dispatcher_tx, dispatcher_rx) = mpsc::channel::<Command>(32);
    tokio::spawn(Dispatcher::new(dispatcher_rx).run());

    if let Some(dir) = Path::new(socket).parent() {
        std::fs::create_dir_all(dir)?;
    }
    let _ = std::fs::remove_file(socket);
    let listener = UnixListener::bind(socket)?;
    info!("TaskService: Listening on {socket}");
    Server::builder()
        .add_service(TaskServiceServer::new(TaskApiHandler::new(dispatcher_tx)))
        .serve_with_incoming(UnixListenerStream::new(listener))
        .await?;
    Ok(())
}

/// Ends this process when the FeOS that started it ends, unless that is
/// PID 1: PID 1 never ends, and after a live upgrade the new binary keeps
/// the task service. Without this, a FeOS run as an ordinary process (a
/// test, `make run`) would leave the task service behind.
fn die_with_parent() -> nix::Result<()> {
    let parent = nix::unistd::getppid();
    if parent.as_raw() == 1 {
        return Ok(());
    }
    nix::sys::prctl::set_pdeathsig(nix::sys::signal::Signal::SIGTERM)?;
    // The parent may have ended before the signal was armed.
    if nix::unistd::getppid() != parent {
        std::process::exit(0);
    }
    Ok(())
}
