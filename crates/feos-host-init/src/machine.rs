// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The machine's setup: hugepages, the network, SR-IOV and the clock, and
//! the power signals.

use crate::hugepages::configure_hugepages;
use crate::network::{configure_network_devices, configure_sriov};
use crate::ntp::TimeSyncWorker;
use log::{error, info, warn};
use std::time::Duration;
use tokio::signal::unix::{signal, SignalKind};

/// 2 MiB pages for the VMs that ask for hugepages.
const HUGEPAGES_NUM: u32 = 1024;
/// SR-IOV virtual functions per physical function.
const VFS_NUM: u32 = 125;

/// Sets the machine up. A failed step is logged and the next one taken: the
/// node may still be useful, and PID 1 must not die.
pub async fn set_up() {
    info!("Configuring hugepages...");
    if let Err(e) = configure_hugepages(HUGEPAGES_NUM).await {
        warn!("Failed to configure hugepages: {e}");
    }

    let is_on_vm = is_running_on_vm().await.unwrap_or_else(|e| {
        error!("Error checking VM status: {e}");
        false // Default to false in case of error
    });

    info!("Configuring network devices...");
    let mut ntp_servers = Vec::new();
    match configure_network_devices().await {
        Ok(Some((delegated_prefix, delegated_prefix_length, servers))) => {
            info!("Delegated prefix: {delegated_prefix}/{delegated_prefix_length}");
            ntp_servers = servers;
        }
        Ok(None) => {}
        Err(e) => error!("Could not configure network devices: {e}"),
    }

    if !is_on_vm {
        info!("configuring sriov...");
        if let Err(e) = configure_sriov(VFS_NUM).await {
            warn!("failed to configure sriov: {e}")
        }
    }

    let time_worker = TimeSyncWorker::new(ntp_servers);
    tokio::spawn(async move {
        time_worker.run().await;
    });
}

pub async fn is_running_on_vm() -> Result<bool, Box<dyn std::error::Error>> {
    let files = [
        "/sys/class/dmi/id/product_name",
        "/sys/class/dmi/id/sys_vendor",
    ];

    let mut match_count = 0;

    for file_path in files.iter() {
        let contents = tokio::fs::read_to_string(file_path).await?;

        let lowercase_contents = contents.to_lowercase();
        if lowercase_contents.contains("cloud") && lowercase_contents.contains("hypervisor") {
            match_count += 1;
        }
    }

    Ok(match_count == 2)
}

/// Reboots on SIGRTMIN+5 and powers off on SIGRTMIN+4, as systemd does, so
/// that feosd asks for either the same way under both.
pub fn take_over_power_signals() {
    for (offset, reboot) in [(5, true), (4, false)] {
        let kind = SignalKind::from_raw(libc::SIGRTMIN() + offset);
        let mut signals = match signal(kind) {
            Ok(signals) => signals,
            Err(e) => {
                warn!("No handler for SIGRTMIN+{offset}: {e}");
                continue;
            }
        };
        tokio::spawn(async move {
            signals.recv().await;
            info!(
                "SIGRTMIN+{offset}: {}",
                if reboot { "rebooting" } else { "powering off" }
            );
            // The kernel's log takes the line before the machine goes.
            tokio::time::sleep(Duration::from_millis(100)).await;
            let Err(e) = if reboot {
                feos_utils::host::power::reboot()
            } else {
                feos_utils::host::power::shutdown()
            };
            error!("Failed to power the machine down: {e}");
        });
    }
}
