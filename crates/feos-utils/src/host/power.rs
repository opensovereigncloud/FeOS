// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use log::{error, info, warn};
use nix::sys::reboot::RebootMode;
use std::convert::Infallible;

/// Writes everything back to the disks and reboots. reboot(2) itself does
/// not sync.
pub fn reboot() -> Result<Infallible, nix::errno::Errno> {
    nix::unistd::sync();
    nix::sys::reboot::reboot(RebootMode::RB_AUTOBOOT)
}

/// Writes everything back to the disks and powers off.
pub fn shutdown() -> Result<Infallible, nix::errno::Errno> {
    nix::unistd::sync();
    nix::sys::reboot::reboot(RebootMode::RB_POWER_OFF)
}

/// Ctrl-Alt-Del makes PID 1 reboot cleanly, where the kernel would reboot at
/// once without writing anything back. It is how a Firecracker host shuts
/// its guest down: a reboot ends the VM.
pub fn take_over_ctrl_alt_del() {
    use tokio::signal::unix::{signal, SignalKind};
    // With Ctrl-Alt-Del off, the kernel sends PID 1 a SIGINT instead.
    let mut sigint = match signal(SignalKind::interrupt()) {
        Ok(sigint) => sigint,
        Err(e) => {
            warn!("Main: No SIGINT handler, Ctrl-Alt-Del reboots at once: {e}");
            return;
        }
    };
    if let Err(e) = nix::sys::reboot::set_cad_enabled(false) {
        warn!("Main: Failed to take over Ctrl-Alt-Del: {e}");
        return;
    }
    tokio::spawn(async move {
        sigint.recv().await;
        info!("Main: Ctrl-Alt-Del, rebooting");
        // The logger prints from a task of its own.
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        let Err(e) = reboot();
        error!("Main: Failed to reboot: {e}");
    });
}
