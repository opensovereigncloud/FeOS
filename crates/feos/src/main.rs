// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use clap::Parser;
use feos_utils::filesystem::{get_root_fstype, move_root};
use main_server::{run_server, run_task_service};
use nix::sys::prctl;
use nix::unistd::{execv, pause};
use std::env;
use std::ffi::CString;

#[derive(Parser, Debug, Default)]
#[command(author, version, about, long_about = None)]
struct ServerArgs {
    #[arg(long, hide = true)]
    restarted_after_upgrade: bool,

    /// Run the task service, the process FeOS starts for the container
    /// runtime.
    #[arg(long, hide = true)]
    task_service: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    let result = run().await;

    if std::process::id() == 1 {
        // PID 1 must never exit: the kernel panics when it does. Idle
        // instead, so the console and the processes that still run stay
        // usable.
        if let Err(e) = &result {
            eprintln!("[feos] {e:#}");
        }
        eprintln!("[feos] FeOS stopped serving; PID 1 idles to keep the kernel alive.");
        loop {
            pause();
        }
    }

    result
}

async fn run() -> Result<()> {
    // The kernel hands unknown command-line words to init. As PID 1, ignore
    // what clap cannot parse instead of exiting.
    let args = ServerArgs::try_parse().unwrap_or_else(|e| {
        if std::process::id() != 1 {
            e.exit();
        }
        eprintln!("[feos] Ignoring arguments: {e}");
        ServerArgs::default()
    });

    if args.task_service {
        return run_task_service().await;
    }

    if std::process::id() == 1 {
        let root_fstype = get_root_fstype().unwrap_or_else(|e| {
            eprintln!("[feos] Failed to get root fstype: {e}");
            String::new()
        });

        if root_fstype == "rootfs" {
            move_root().map_err(|e| anyhow::anyhow!("[feos] move_root failed: {e}"))?;

            let argv: Vec<CString> = env::args()
                .map(|arg| CString::new(arg).unwrap_or_default())
                .collect();
            let _ =
                execv(&argv[0], &argv).map_err(|e| anyhow::anyhow!("[feos] execv failed: {e}"))?;

            return Err(anyhow::anyhow!("execv failed to replace process"));
        }
    } else {
        prctl::set_child_subreaper(true).map_err(|e| {
            anyhow::anyhow!(
                "[feos] Failed to set as child subreaper when running as not PID 1: {e}"
            )
        })?;
    }

    // PID 1 and subreapers inherit orphans; reap them.
    feos_utils::reaper::spawn();

    run_server(args.restarted_after_upgrade).await
}
