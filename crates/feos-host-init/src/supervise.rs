// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Runs feosd and starts it again when it ends. feosd replaces its own
//! binary in an upgrade and runs it with the same pid, so the wait goes on
//! across an upgrade.

use log::{error, info, warn};
use std::net::Ipv6Addr;
use std::process::{Command, Stdio};
use std::time::Duration;

/// Where the initramfs has feosd, where the package installs it.
const FEOSD: &str = "/usr/sbin/feosd";
/// feosd replaces its own binary here, and serves on TCP: a node whose
/// operating system is FeOS has no package manager, and no other way in.
const FEOSD_ARGS: &[&str] = &["--self-upgrade", "--tcp", "[::]:1337"];
const RESTART_DELAY: Duration = Duration::from_secs(1);

/// feosd's arguments: the prefix delegated to the node, if any, is its
/// pods' (ADR 0010).
fn feosd_args(pod_prefix: Option<(Ipv6Addr, u8)>) -> Vec<String> {
    let mut args: Vec<String> = FEOSD_ARGS.iter().map(|arg| arg.to_string()).collect();
    // feosd refuses a prefix without room for pods, and would never run.
    if let Some((prefix, len)) = pod_prefix.filter(|(_, len)| (1..=120).contains(len)) {
        let network = u128::from(prefix) & (u128::MAX << (128 - u32::from(len)));
        args.push("--pod-prefix".to_string());
        args.push(format!("{}/{len}", Ipv6Addr::from(network)));
    }
    args
}

pub async fn run_feosd(pod_prefix: Option<(Ipv6Addr, u8)>) {
    let args = feosd_args(pod_prefix);
    loop {
        // A std child: the reaper owns waitpid in PID 1.
        match Command::new(FEOSD).args(&args).stdin(Stdio::null()).spawn() {
            Ok(child) => {
                let pid = child.id() as i32;
                info!("Started feosd (pid {pid})");
                let code = feos_utils::reaper::wait_for_exit(pid).await;
                warn!("feosd (pid {pid}) exited with code {code}; starting it again");
            }
            Err(e) => error!("Failed to start {FEOSD}: {e}"),
        }
        tokio::time::sleep(RESTART_DELAY).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_delegated_prefix_is_the_pods() {
        assert_eq!(feosd_args(None), FEOSD_ARGS);
        let prefix = "2001:db8:7:100::".parse().unwrap();
        let args = feosd_args(Some((prefix, 56)));
        assert_eq!(&args[3..], ["--pod-prefix", "2001:db8:7:100::/56"]);
        // Bits past the length go; a prefix without room is left out.
        let args = feosd_args(Some(("2001:db8::1".parse().unwrap(), 64)));
        assert_eq!(args.last().unwrap(), "2001:db8::/64");
        assert_eq!(feosd_args(Some((prefix, 128))), FEOSD_ARGS);
    }
}
