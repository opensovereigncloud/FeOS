// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feos-guest: PID 1 in a pod's microVM.

use anyhow::Result;
use feos_utils::filesystem::{get_root_fstype, move_root};
use main_server::{run_server, run_task_service, ServerOptions};
use nix::sys::prctl;
use nix::unistd::{execv, pause};
use std::env;
use std::ffi::CString;

/// feos-guest's own arguments: the ones it passes itself, and --version.
/// The kernel hands unknown command-line words to init, so anything else is
/// ignored.
#[derive(Debug, Default)]
struct ServerArgs {
    restarted_after_upgrade: bool,

    /// Run the task service, the process feos-guest starts for the
    /// container runtime.
    task_service: bool,

    version: bool,
}

impl ServerArgs {
    fn parse(args: impl IntoIterator<Item = String>) -> Self {
        let mut parsed = Self::default();
        for arg in args {
            match arg.as_str() {
                "--restarted-after-upgrade" => parsed.restarted_after_upgrade = true,
                "--task-service" => parsed.task_service = true,
                "--version" => parsed.version = true,
                _ => {}
            }
        }
        parsed
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let result = run().await;

    if std::process::id() == 1 {
        // PID 1 must never exit: the kernel panics when it does. Idle
        // instead, so the console and the processes that still run stay
        // usable.
        if let Err(e) = &result {
            eprintln!("[feos-guest] {e:#}");
        }
        eprintln!("[feos-guest] feos-guest stopped serving; PID 1 idles to keep the kernel alive.");
        loop {
            pause();
        }
    }

    result
}

async fn run() -> Result<()> {
    let args = ServerArgs::parse(env::args().skip(1));
    if args.version && std::process::id() != 1 {
        println!("feos-guest {}", feos_utils::version::full_version_string());
        return Ok(());
    }

    if args.task_service {
        return run_task_service().await;
    }

    if std::process::id() == 1 {
        let root_fstype = get_root_fstype().unwrap_or_else(|e| {
            eprintln!("[feos-guest] Failed to get root fstype: {e}");
            String::new()
        });

        if root_fstype == "rootfs" {
            move_root().map_err(|e| anyhow::anyhow!("[feos-guest] move_root failed: {e}"))?;

            let argv: Vec<CString> = env::args()
                .map(|arg| CString::new(arg).unwrap_or_default())
                .collect();
            let _ = execv(&argv[0], &argv)
                .map_err(|e| anyhow::anyhow!("[feos-guest] execv failed: {e}"))?;

            return Err(anyhow::anyhow!("execv failed to replace process"));
        }
    } else {
        prctl::set_child_subreaper(true).map_err(|e| {
            anyhow::anyhow!(
                "[feos-guest] Failed to set as child subreaper when running as not PID 1: {e}"
            )
        })?;
    }

    // PID 1 and subreapers inherit orphans; reap them.
    feos_utils::reaper::spawn();

    // A guest replaces its own binary in an upgrade: nothing else owns it.
    run_server(ServerOptions::new(args.restarted_after_upgrade, true)).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn known_flags_are_recognized_and_the_rest_ignored() {
        let args = ServerArgs::parse(
            [
                "quiet",
                "--task-service",
                "--restarted-after-upgrade",
                "x=1",
            ]
            .map(String::from),
        );
        assert!(args.task_service && args.restarted_after_upgrade && !args.version);
        let args = ServerArgs::parse(["--help"].map(String::from));
        assert!(!args.task_service && !args.restarted_after_upgrade);
    }
}
