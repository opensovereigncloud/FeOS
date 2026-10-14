// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! diskd, the node's disk daemon (ADR 0011).
//!
//! One daemon per node: owns the /dev/ublkb<N> devices backing pod
//! disks, the local chunk cache, and the S3 client. Control via gRPC on
//! a unix socket.
//!
//! Startup: adopt the cache, then reattach every device found in
//! `diskd.state` via ublk user recovery (crash drill: kill -9, restart,
//! I/O resumes).

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

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> Result<(), String> {
    init_tracing();
    let args = args::Args::parse();
    let socket = args.socket();
    let s3_prefix = args.s3_prefix();
    std::fs::create_dir_all(&args.state_dir).map_err(|e| e.to_string())?;
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

    let svc = svc::DiskSvc {
        state_dir: args.state_dir.clone(),
        cache,
        s3,
        devices: Default::default(),
        memories: Default::default(),
        s3_concurrency: args.s3_concurrency,
        mem_readahead: args.mem_readahead,
    };

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
    tonic::transport::Server::builder()
        .add_service(feos_proto::disk_service::disk_service_server::DiskServiceServer::new(svc))
        .serve_with_incoming(incoming)
        .await
        .map_err(|e| e.to_string())
}
