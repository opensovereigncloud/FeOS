// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! diskd, the node's disk daemon (ADR 0011).
//!
//! One daemon per node: owns the /dev/ublkb<N> devices backing pod
//! disks, the local chunk cache, and the S3 client. Control via gRPC on
//! a unix socket.
//!
//! Startup: adopt the cache, pin the chunks only the node has, then
//! reattach every device found in `diskd.state` via ublk user recovery
//! (crash drill: kill -9, restart, I/O resumes).

#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

mod args;
mod dev;
mod state;
mod stats;
mod svc;
mod uffd;

use std::sync::Arc;

use clap::Parser;
use tracing::{error, info, warn};
use tracing_subscriber::EnvFilter;

fn init_tracing() {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let atty = unsafe { libc::isatty(1) } == 1;
    if atty {
        tracing_subscriber::fmt().with_env_filter(filter).init();
    } else {
        // JSON when not on a TTY.
        tracing_subscriber::fmt()
            .json()
            .with_env_filter(filter)
            .init();
    }
}

/// Tells systemd that diskd serves, its devices recovered (sd_notify
/// READY=1): one datagram to $NOTIFY_SOCKET, without libsystemd. Nothing to
/// do without it.
fn notify_ready() {
    use std::os::linux::net::SocketAddrExt;
    use std::os::unix::ffi::OsStrExt;
    use std::os::unix::net::{SocketAddr, UnixDatagram};

    let Some(socket) = std::env::var_os("NOTIFY_SOCKET") else {
        return;
    };
    let socket = socket.as_bytes();
    // "@" names a socket in the abstract namespace.
    let address = match socket.strip_prefix(b"@") {
        Some(name) => SocketAddr::from_abstract_name(name),
        None => {
            SocketAddr::from_pathname(std::path::Path::new(std::ffi::OsStr::from_bytes(socket)))
        }
    };
    let sent = address.and_then(|address| {
        let datagram = UnixDatagram::unbound()?;
        datagram.send_to_addr(b"READY=1", &address)
    });
    if let Err(e) = sent {
        warn!(error = %e, "failed to notify systemd");
    }
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> Result<(), String> {
    init_tracing();
    let args = args::Args::parse();
    let socket = args.socket();
    let s3_prefix = args.s3_prefix();
    std::fs::create_dir_all(&args.state_dir).map_err(|e| e.to_string())?;
    // One diskd per state directory: another one, such as the old pod's of
    // a rollout, ends first. Held until diskd ends.
    let lock = args.state_dir.join(STATE_LOCK);
    let _state = feos_utils::state_lock::take(&lock, |holder| {
        warn!(
            lock = %lock.display(),
            holder = ?holder,
            "another diskd runs on this state directory: waiting for it to end"
        )
    })
    .await
    .map_err(|e| format!("lock {}: {e}", lock.display()))?;
    // Handled from the start, so that a SIGTERM during the recovery waits
    // too.
    let terminated = match &args.stop_after_lock {
        Some(path) => Some((
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .map_err(|e| format!("handle SIGTERM: {e}"))?,
            path.clone(),
        )),
        None => None,
    };
    info!(
        state_dir = %args.state_dir.display(),
        socket = %socket.display(),
        budget_bytes = args.budget_bytes,
        s3_prefix = %s3_prefix,
        "diskd starting"
    );

    let s3 = disk_store::s3::S3::from_env(s3_prefix)
        .map_err(|e| e.to_string())?
        .map(Arc::new);
    if s3.is_none() {
        warn!("S3 env incomplete: tiering disabled, cache misses will EIO");
    }
    let cache = disk_store::cache::Cache::open(&args.state_dir, args.budget_bytes)
        .map_err(|e| e.to_string())?;
    // Teach the cache where its own contents come from. Everything it
    // holds that has been pushed is also in the bucket, so an eviction
    // must not be able to make a later seal fail (see cache::Refill).
    // Called from spawn_blocking threads only — never from a task.
    let cache = match &s3 {
        Some(s3) => {
            let s3 = s3.clone();
            let handle = tokio::runtime::Handle::current();
            cache.with_refill(Arc::new(move |h, raw_len| {
                handle.block_on(s3.get_chunk(h, raw_len))
            }))
        }
        None => cache,
    };
    let cache = Arc::new(cache);

    let svc = svc::DiskSvc::new(
        args.state_dir.clone(),
        cache,
        s3,
        args.s3_concurrency,
        args.mem_readahead,
    );

    // Pin what the node holds the only copy of before anything is admitted:
    // the layers of its devices that are not uploaded, and the own layers of
    // disks no head names yet.
    {
        let this = svc.clone();
        tokio::task::spawn_blocking(move || this.refresh_pins())
            .await
            .map_err(|e| e.to_string())?;
    }

    // Startup recovery: reattach every device found in the state file.
    let rows: Vec<(String, state::DeviceRow)> = match state::State::load(&args.state_dir) {
        Ok(st) => st
            .devices
            .iter()
            .filter(|(_, r)| r.dev_id.is_some())
            .map(|(k, r)| (k.clone(), r.clone()))
            .collect(),
        Err(e) => {
            warn!(error = %e, "state file unreadable; starting cold");
            Vec::new()
        }
    };
    for (pod_id, row) in rows {
        let this = svc.clone();
        let id = pod_id.clone();
        let id2 = id.clone();
        // Synchronous at startup: devices come up before we serve.
        match tokio::task::spawn_blocking(move || this.spawn_dev_recovery(row, id2)).await {
            Ok(Ok(dev)) => info!(pod = %id, dev = %dev.dev_path, "recovered"),
            Ok(Err(e)) => {
                error!(pod = %id, error = %e, "recovery failed; row dropped from table")
            }
            Err(e) => return Err(e.to_string()),
        }
    }

    // gRPC on the unix socket, for root and its group only, as feosd's:
    // the API reads and writes files on the node as root.
    let _ = std::fs::remove_file(&socket);
    let listener = tokio::net::UnixListener::bind(&socket).map_err(|e| e.to_string())?;
    let _ = std::fs::set_permissions(&socket, std::os::unix::fs::PermissionsExt::from_mode(0o660));
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(listener);

    info!("diskd serving");
    notify_ready();
    let server = tonic::transport::Server::builder()
        .add_service(feos_proto::disk_service::disk_service_server::DiskServiceServer::new(svc))
        .serve_with_incoming(incoming);
    tokio::select! {
        served = server => served.map_err(|e| e.to_string()),
        () = stop_after_lock(terminated) => Ok(()),
    }
}

/// The lock diskd holds on its state directory.
const STATE_LOCK: &str = "diskd.lock";
/// How often a stopping diskd looks whether the lock it outlives is free.
const STOP_POLL: std::time::Duration = std::time::Duration::from_millis(500);

/// With --stop-after-lock: resolves after SIGTERM, once no process holds
/// the lock; the devices stay for the next diskd to recover. Without, never.
async fn stop_after_lock(terminated: Option<(tokio::signal::unix::Signal, std::path::PathBuf)>) {
    let Some((mut signal, path)) = terminated else {
        return std::future::pending().await;
    };
    signal.recv().await;
    info!(lock = %path.display(), "SIGTERM: diskd stops once nothing holds the lock");
    loop {
        match feos_utils::state_lock::is_held(&path) {
            Ok(false) => break,
            Ok(true) => {}
            // Better to serve on than to leave a feosd without its disks.
            Err(e) => warn!(lock = %path.display(), error = %e, "the lock does not read"),
        }
        tokio::time::sleep(STOP_POLL).await;
    }
    info!("diskd stops");
}
