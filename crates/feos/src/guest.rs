// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The guest profile: FeOS as PID 1 in a microVM whose host configures it.
//!
//! The API listens on vsock, where the host reaches it through the VMM, and
//! not on TCP. Once the listeners are up, FeOS dials the host and holds a
//! LifecycleService call open: that call is how the host learns that the
//! guest is ready, and its end tells the host that the guest is gone.

use feos_proto::lifecycle_service::lifecycle_service_client::LifecycleServiceClient;
use feos_proto::lifecycle_service::{hello, Hello};
use feos_proto::tool_service::tool_service_server::ToolServiceServer;
use feos_utils::filesystem::{mount_data_volume, DATA_ROOT};
use feos_utils::vsock::{API_PORT, UPLINK_PORT};
use hyper_util::rt::TokioIo;
use log::{debug, info, warn};
use std::io;
use std::time::Duration;
use tokio_vsock::{
    Incoming, VsockAddr, VsockListener, VsockStream, VMADDR_CID_ANY, VMADDR_CID_HOST,
};
use tonic::transport::server::Router;
use tonic::transport::{Endpoint, Uri};
use tool_service::ToolApiHandler;
use tower::service_fn;

/// Backoff for failed connects to the host, which only happen while no
/// manager listens (a manager rollout, or no manager at all). A call that
/// was up and then ended is retried at once.
const BACKOFF_START: Duration = Duration::from_millis(100);
const BACKOFF_MAX: Duration = Duration::from_secs(1);

/// HTTP/2 keepalive on the uplink. A connection held across a snapshot is
/// not reset in a way that wakes its reader, so without keepalive the call
/// would look alive forever after a resume. With it, FeOS notices within
/// INTERVAL + TIMEOUT and attaches again. Nothing waits on that, hence the
/// generous interval.
const KEEPALIVE_INTERVAL: Duration = Duration::from_secs(10);
const KEEPALIVE_TIMEOUT: Duration = Duration::from_secs(5);

/// The services only the guest serves: what an agent in a container uses.
/// One instance serves every listener, so that state they keep (watches,
/// listeners) is the guest's, whichever transport a call came in on.
#[derive(Clone)]
pub(crate) struct GuestServices {
    tool: ToolServiceServer<ToolApiHandler>,
}

impl GuestServices {
    pub(crate) fn new() -> Self {
        Self {
            tool: ToolServiceServer::new(ToolApiHandler),
        }
    }

    pub(crate) fn add_to(&self, router: Router) -> Router {
        router.add_service(self.tool.clone())
    }
}

/// First-boot setup of the guest: the data volume and memory overcommit.
pub(crate) fn initialize_guest() {
    mount_data_volume(DATA_ROOT);
    allow_overcommit();
}

/// vm.overcommit_memory = 1. The kernel's default heuristic refuses a single
/// writable private mapping larger than RAM plus swap, which in a small VM
/// breaks software that reserves address space it never touches (Chromium's
/// V8 fails to start). The VM's memory is the hard limit either way.
fn allow_overcommit() {
    if let Err(e) = std::fs::write("/proc/sys/vm/overcommit_memory", "1") {
        warn!("Failed to set vm.overcommit_memory=1: {e}");
    }
}

/// Binds the API's vsock listener.
pub(crate) fn vsock_incoming() -> io::Result<Incoming> {
    Ok(VsockListener::bind(VsockAddr::new(VMADDR_CID_ANY, API_PORT))?.incoming())
}

/// Holds the lifecycle uplink to the host for the lifetime of the guest.
/// Spawn it after the API listeners are bound, so that the host's first call
/// after the announcement lands in their backlog.
pub(crate) async fn run_uplink() {
    let boot_id = std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .map(|s| s.trim().to_string())
        .unwrap_or_default();
    let mut attach_seq: u64 = 0;
    let mut reason = hello::Reason::Boot;
    let mut backoff = BACKOFF_START;

    loop {
        match attach(&boot_id, reason, attach_seq).await {
            // The call was up and has ended: a pause, a resume the keepalive
            // noticed, or a manager restart.
            Ok(()) => {
                info!("Uplink: Detached from host (seq {attach_seq})");
                backoff = BACKOFF_START;
            }
            Err(e) => {
                // Repeated failures only mean that nobody listens yet.
                if backoff == BACKOFF_START {
                    warn!("Uplink: Failed to attach to host: {e}");
                } else {
                    debug!("Uplink: Failed to attach to host: {e}");
                }
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(BACKOFF_MAX);
                continue;
            }
        }
        attach_seq = attach_seq.saturating_add(1);
        reason = hello::Reason::Reconnect;
    }
}

/// One attach: connect, announce, then hold the call until it ends. Ok means
/// the call was up and is gone now; Err that it never came up.
async fn attach(
    boot_id: &str,
    reason: hello::Reason,
    attach_seq: u64,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let channel = Endpoint::from_static("http://feos.uplink")
        .http2_keep_alive_interval(KEEPALIVE_INTERVAL)
        .keep_alive_timeout(KEEPALIVE_TIMEOUT)
        .keep_alive_while_idle(true)
        .connect_with_connector(service_fn(|_: Uri| async {
            VsockStream::connect(VsockAddr::new(VMADDR_CID_HOST, UPLINK_PORT))
                .await
                .map(TokioIo::new)
        }))
        .await?;

    let mut stream = LifecycleServiceClient::new(channel)
        .attach(Hello {
            reason: reason as i32,
            boot_id: boot_id.to_string(),
            attach_seq,
            feos_version: feos_utils::version::full_version_string(),
        })
        .await?
        .into_inner();
    info!(
        "Uplink: Attached to host ({}, seq {attach_seq})",
        reason.as_str_name()
    );

    // The host sends nothing; this waits for the call to end.
    while stream.message().await?.is_some() {}
    Ok(())
}
