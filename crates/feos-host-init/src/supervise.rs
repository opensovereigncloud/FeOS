// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Runs feosd and starts it again when it ends. feosd replaces its own
//! binary in an upgrade and runs it with the same pid, so the wait goes on
//! across an upgrade.

use log::{error, info, warn};
use std::process::{Command, Stdio};
use std::time::Duration;

/// Where the initramfs has feosd, where the package installs it.
const FEOSD: &str = "/usr/sbin/feosd";
/// feosd replaces its own binary here, and serves on TCP: a node whose
/// operating system is FeOS has no package manager, and no other way in.
const FEOSD_ARGS: &[&str] = &["--self-upgrade", "--tcp", "[::]:1337"];
const RESTART_DELAY: Duration = Duration::from_secs(1);

pub async fn run_feosd() {
    loop {
        // A std child: the reaper owns waitpid in PID 1.
        match Command::new(FEOSD)
            .args(FEOSD_ARGS)
            .stdin(Stdio::null())
            .spawn()
        {
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
