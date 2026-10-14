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

/// 2 MiB pages for the VMs that ask for hugepages, at most.
const HUGEPAGES_NUM: u32 = 1024;
/// The share of the machine's memory they take at most: on a small node,
/// all 1024 would leave the kernel nothing.
const HUGEPAGES_MEMORY_SHARE: u64 = 4;
/// SR-IOV virtual functions per physical function.
const VFS_NUM: u32 = 125;

/// Sets the machine up. A failed step is logged and the next one taken: the
/// node may still be useful, and PID 1 must not die.
pub async fn set_up() {
    info!("Configuring hugepages...");
    let cmdline = tokio::fs::read_to_string("/proc/cmdline")
        .await
        .unwrap_or_default();
    let meminfo = tokio::fs::read_to_string("/proc/meminfo")
        .await
        .unwrap_or_default();
    if let Err(e) = configure_hugepages(hugepages_to_reserve(&cmdline, &meminfo)).await {
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

/// How many hugepages to reserve: none if the kernel's command line sets
/// `hugepages=` (the kernel reserved them at boot), else up to
/// HUGEPAGES_NUM in a quarter of the memory.
fn hugepages_to_reserve(cmdline: &str, meminfo: &str) -> Option<u32> {
    if cmdline
        .split_whitespace()
        .any(|arg| arg.starts_with("hugepages="))
    {
        return None;
    }
    let total_kib: u64 = meminfo
        .lines()
        .find_map(|line| line.strip_prefix("MemTotal:"))
        .and_then(|value| value.trim().trim_end_matches("kB").trim().parse().ok())
        .unwrap_or(0);
    let pages = total_kib / HUGEPAGES_MEMORY_SHARE / 2048;
    Some(pages.min(u64::from(HUGEPAGES_NUM)) as u32)
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hugepages_take_a_quarter_of_the_memory_at_most() {
        let meminfo = |gib: u64| format!("MemTotal:       {} kB\nMemFree: 1 kB\n", gib << 20);
        assert_eq!(hugepages_to_reserve("", &meminfo(64)), Some(1024));
        assert_eq!(hugepages_to_reserve("", &meminfo(8)), Some(1024));
        assert_eq!(hugepages_to_reserve("", &meminfo(4)), Some(512));
        assert_eq!(hugepages_to_reserve("", &meminfo(1)), Some(128));
        assert_eq!(hugepages_to_reserve("", ""), Some(0));
    }

    #[test]
    fn the_kernel_command_line_decides_when_it_names_them() {
        let meminfo = "MemTotal: 67108864 kB\n";
        assert_eq!(
            hugepages_to_reserve("console=ttyS0 hugepages=16", meminfo),
            None
        );
        assert_eq!(hugepages_to_reserve("hugepages=0\n", meminfo), None);
    }
}
