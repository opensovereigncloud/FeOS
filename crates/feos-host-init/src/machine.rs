// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The machine's setup: the network, SR-IOV and the clock, and
//! the power signals.

use crate::network::{configure_network_devices, configure_sriov};
use crate::ntp::TimeSyncWorker;
use log::{error, info, warn};
use std::net::Ipv6Addr;
use std::time::Duration;
use tokio::signal::unix::{signal, SignalKind};

/// SR-IOV virtual functions per physical function.
const VFS_NUM: u32 = 125;
/// The kernel's command line names the pods' prefix where the network
/// routes one to the node statically, not by DHCPv6.
const POD_PREFIX_PARAMETER: &str = "feos.pod-prefix=";

/// Sets the machine up. A failed step is logged and the next one taken: the
/// node may still be useful, and PID 1 must not die. Returns the pods'
/// prefix (ADR 0010), if any: the one the kernel's command line names, else
/// the one DHCPv6 delegated to the node.
pub async fn set_up() -> Option<(Ipv6Addr, u8)> {
    let is_on_vm = is_running_on_vm().await.unwrap_or_else(|e| {
        error!("Error checking VM status: {e}");
        false // Default to false in case of error
    });

    info!("Configuring network devices...");
    let mut ntp_servers = Vec::new();
    let mut delegated = None;
    match configure_network_devices().await {
        Ok(Some((delegated_prefix, delegated_prefix_length, servers))) => {
            info!("Delegated prefix: {delegated_prefix}/{delegated_prefix_length}");
            delegated = Some((delegated_prefix, delegated_prefix_length));
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
    let cmdline = std::fs::read_to_string("/proc/cmdline").unwrap_or_default();
    match pod_prefix(&cmdline) {
        Some(Ok((prefix, len))) => {
            info!("The pods' prefix, from the kernel's command line: {prefix}/{len}");
            Some((prefix, len))
        }
        Some(Err(given)) => {
            error!("{POD_PREFIX_PARAMETER}{given} is no IPv6 prefix; ignoring it");
            delegated
        }
        None => delegated,
    }
}

/// The pods' prefix the kernel's command line names, or what it gave
/// instead of one.
fn pod_prefix(cmdline: &str) -> Option<Result<(Ipv6Addr, u8), &str>> {
    let given = cmdline
        .split_whitespace()
        .find_map(|parameter| parameter.strip_prefix(POD_PREFIX_PARAMETER))?;
    let parsed = given.split_once('/').and_then(|(prefix, len)| {
        let len = len.parse::<u8>().ok().filter(|len| *len <= 128)?;
        Some((prefix.parse::<Ipv6Addr>().ok()?, len))
    });
    Some(parsed.ok_or(given))
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
    fn the_command_line_names_the_pods_prefix() {
        let prefix: Ipv6Addr = "fd00:fe05:ffff::".parse().unwrap();
        assert_eq!(
            pod_prefix("console=ttyS0 feos.pod-prefix=fd00:fe05:ffff::/64 quiet"),
            Some(Ok((prefix, 64)))
        );
        assert_eq!(pod_prefix("console=ttyS0"), None);
        assert_eq!(pod_prefix("feos.pod-prefix=fd00::"), Some(Err("fd00::")));
        assert_eq!(
            pod_prefix("feos.pod-prefix=10.0.0.0/8"),
            Some(Err("10.0.0.0/8"))
        );
        assert_eq!(
            pod_prefix("feos.pod-prefix=fd00::/129"),
            Some(Err("fd00::/129"))
        );
    }
}
