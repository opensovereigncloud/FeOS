// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd: the node agent, under systemd or feos-host-init, never PID 1.

use anyhow::{bail, Context, Result};
use main_server::{run_server, run_task_service, ServerOptions};
use nix::sys::prctl;
use std::net::SocketAddr;

const USAGE: &str = "usage: feosd [--tcp ADDRESS] [--self-upgrade]
       feosd --version

  --tcp ADDRESS   also serve the API on TCP, without authentication
  --self-upgrade  let UpgradeFeosBinary replace this binary";

#[derive(Debug, Default, PartialEq)]
struct Args {
    tcp: Option<SocketAddr>,
    self_upgrade: bool,
    /// Passed by feosd itself: the task service, and the process an upgrade
    /// exec'd.
    task_service: bool,
    restarted_after_upgrade: bool,
    version: bool,
}

impl Args {
    fn parse(args: impl IntoIterator<Item = String>) -> Result<Self> {
        let mut parsed = Self::default();
        let mut args = args.into_iter();
        while let Some(arg) = args.next() {
            match arg.as_str() {
                "--tcp" => {
                    let address = args.next().context("--tcp needs an address")?;
                    let address = address
                        .parse()
                        .with_context(|| format!("--tcp {address}"))?;
                    parsed.tcp = Some(address);
                }
                "--self-upgrade" => parsed.self_upgrade = true,
                "--task-service" => parsed.task_service = true,
                "--restarted-after-upgrade" => parsed.restarted_after_upgrade = true,
                "--version" => parsed.version = true,
                other => bail!("unknown argument {other}\n{USAGE}"),
            }
        }
        Ok(parsed)
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse(std::env::args().skip(1))?;
    if args.version {
        println!("feosd {}", feos_utils::version::full_version_string());
        return Ok(());
    }
    if args.task_service {
        return run_task_service().await;
    }
    if std::process::id() == 1 {
        bail!("feosd does not run as PID 1; feos-host-init runs it");
    }

    // feosd inherits the orphans of its children, such as the processes
    // of a task service that died, and reaps them.
    prctl::set_child_subreaper(true).context("become a subreaper")?;
    feos_utils::reaper::spawn();
    match main_server::cgroup::enter_leaf() {
        Ok(base) => vm_service::vmm::jail::set_cgroup_base(&base),
        Err(e) => eprintln!("feosd: Failed to move into a leaf of its cgroup: {e}"),
    }

    run_server(ServerOptions {
        tcp: args.tcp,
        ..ServerOptions::new(args.restarted_after_upgrade, args.self_upgrade)
    })
    .await
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(args: &[&str]) -> Result<Args> {
        Args::parse(args.iter().map(|arg| arg.to_string()))
    }

    #[test]
    fn arguments_are_parsed() {
        let args = parse(&["--tcp", "[::]:1337", "--self-upgrade"]).unwrap();
        assert_eq!(args.tcp, Some("[::]:1337".parse().unwrap()));
        assert!(args.self_upgrade && !args.task_service);
        assert_eq!(parse(&[]).unwrap(), Args::default());
    }

    #[test]
    fn bad_arguments_are_refused() {
        assert!(parse(&["--tcp"]).is_err());
        assert!(parse(&["--tcp", "nowhere"]).is_err());
        assert!(parse(&["--frobnicate"]).is_err());
    }
}
