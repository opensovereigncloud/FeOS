// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::error::HostError;
use feos_proto::host_service::{RebootRequest, RebootResponse, ShutdownRequest, ShutdownResponse};
use log::{error, info};
use tokio::sync::oneshot;

/// A guest is PID 1 and powers its VM down itself. Firecracker cannot
/// power a VM off: the guest would halt and its VMM run on. A guest that
/// reboots ends its VMM instead.
#[cfg(feature = "guest")]
fn shut_down() -> Result<(), nix::Error> {
    feos_utils::host::power::reboot().map(|never| match never {})
}

#[cfg(feature = "guest")]
fn reboot() -> Result<(), nix::Error> {
    feos_utils::host::power::reboot().map(|never| match never {})
}

/// feosd asks PID 1, systemd or feos-host-init, which both power off on
/// SIGRTMIN+4 and reboot on SIGRTMIN+5.
#[cfg(not(feature = "guest"))]
fn shut_down() -> Result<(), nix::Error> {
    signal_pid1(4)
}

#[cfg(not(feature = "guest"))]
fn reboot() -> Result<(), nix::Error> {
    signal_pid1(5)
}

#[cfg(not(feature = "guest"))]
fn signal_pid1(rtmin_offset: i32) -> Result<(), nix::Error> {
    // nix's Signal has no real-time signals.
    // SAFETY: kill(2) takes any pid and signal number.
    let sent = unsafe { libc::kill(1, libc::SIGRTMIN() + rtmin_offset) };
    if sent == 0 {
        Ok(())
    } else {
        Err(nix::Error::last())
    }
}

pub async fn handle_shutdown(
    _req: ShutdownRequest,
    responder: oneshot::Sender<Result<ShutdownResponse, HostError>>,
) {
    info!("HostWorker: Processing Shutdown request.");

    if responder.send(Ok(ShutdownResponse {})).is_err() {
        error!(
            "HostWorker: Failed to send response for Shutdown. The client may have disconnected."
        );
    }

    tokio::time::sleep(tokio::time::Duration::from_secs(1)).await;

    info!("HostWorker: Executing system shutdown.");
    if let Err(e) = shut_down() {
        error!("HostWorker: CRITICAL - Failed to execute system shutdown: {e}");
    }
}

pub async fn handle_reboot(
    _req: RebootRequest,
    responder: oneshot::Sender<Result<RebootResponse, HostError>>,
) {
    info!("HostWorker: Processing Reboot request.");

    if responder.send(Ok(RebootResponse {})).is_err() {
        error!("HostWorker: Failed to send response for Reboot. The client may have disconnected.");
    }

    tokio::time::sleep(tokio::time::Duration::from_secs(1)).await;

    info!("HostWorker: Executing system reboot.");
    if let Err(e) = reboot() {
        error!("HostWorker: CRITICAL - Failed to execute system reboot: {e}");
    }
}
