// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd: the node agent, under systemd or feos-host-init, never PID 1.

use anyhow::{anyhow, bail, Context, Result};
use main_server::{run_server, run_task_service, ServerOptions};
use nix::sys::prctl;
use pod_service::network;
use std::net::SocketAddr;

const USAGE: &str = "usage: feosd [--tcp ADDRESS] [--self-upgrade] [--pod-prefix PREFIX]
             [--pod-ipv4-range PREFIX] [--dns-address ADDRESS]
             [--dns-upstream ADDRESS]... [--deny-prefix PREFIX]...
       feosd --version

  --tcp ADDRESS            also serve the API on TCP, without authentication
  --self-upgrade           let UpgradeFeosBinary replace this binary
  --pod-prefix PREFIX      the IPv6 prefix the network routes to this node, such
                           as a /64; pods get their addresses from it. Without,
                           pods with a network fail
  --pod-ipv4-range PREFIX  the IPv4 prefix pods get an address of each, for
                           leaving through masquerade; 169.254.16.0/20 by
                           default
  --dns-address ADDRESS    the pods' resolver, an IPv4 address the node holds;
                           169.254.0.53 by default
  --dns-upstream ADDRESS   an upstream resolver of pods that name none
                           (repeatable); Google's and Cloudflare's by default
  --deny-prefix PREFIX     a prefix of either family no pod reaches
                           (repeatable)";

#[derive(Debug, Default, PartialEq)]
struct Args {
    tcp: Option<SocketAddr>,
    self_upgrade: bool,
    network: network::Config,
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
        // The first --dns-upstream replaces the defaults.
        let mut upstreams = Vec::new();
        while let Some(arg) = args.next() {
            let mut value = |what: &str| {
                args.next()
                    .with_context(|| format!("{arg} needs {what}\n{USAGE}"))
            };
            let flag = |e: String| anyhow!("{arg}: {e}");
            let network = &mut parsed.network;
            match arg.as_str() {
                "--tcp" => {
                    let address = value("an address")?;
                    let address = address
                        .parse()
                        .with_context(|| format!("--tcp {address}"))?;
                    parsed.tcp = Some(address);
                }
                "--pod-prefix" => {
                    network.pod_prefix =
                        Some(network::parse_pod_prefix(&value("a prefix")?).map_err(flag)?);
                }
                "--pod-ipv4-range" => {
                    network.pod_ipv4_range =
                        network::parse_pod_ipv4_range(&value("a prefix")?).map_err(flag)?;
                }
                "--dns-address" => {
                    let address = value("an address")?;
                    network.dns_address = address
                        .parse()
                        .with_context(|| format!("--dns-address {address}: an IPv4 address"))?;
                }
                "--dns-upstream" => {
                    let address = value("an address")?;
                    upstreams.push(
                        address
                            .parse()
                            .with_context(|| format!("--dns-upstream {address}"))?,
                    );
                }
                "--deny-prefix" => {
                    let prefix = value("a prefix")?.parse().map_err(flag)?;
                    network.deny_prefixes.push(prefix);
                }
                "--self-upgrade" => parsed.self_upgrade = true,
                "--task-service" => parsed.task_service = true,
                "--restarted-after-upgrade" => parsed.restarted_after_upgrade = true,
                "--version" => parsed.version = true,
                other => bail!("unknown argument {other}\n{USAGE}"),
            }
        }
        if !upstreams.is_empty() {
            parsed.network.dns_upstreams = upstreams;
        }
        parsed
            .network
            .check()
            .map_err(|e| anyhow!("{e}\n{USAGE}"))?;
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
        Ok(base) => microvm_service::vmm::jail::set_cgroup_base(&base),
        Err(e) => eprintln!("feosd: Failed to move into a leaf of its cgroup: {e}"),
    }

    run_server(ServerOptions {
        tcp: args.tcp,
        network: args.network,
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
        for bad in [
            &["--pod-prefix", "10.0.0.0/8"][..],
            &["--pod-prefix", "fd00::1/64"],
            &["--pod-prefix"],
            &["--pod-ipv4-range", "fd00::/64"],
            &["--pod-ipv4-range", "169.254.16.0/31"],
            &["--pod-ipv4-range", "169.254.0.0/16"],
            &["--dns-address", "fd00::53"],
            &["--dns-address", "169.254.0.1"],
            &["--dns-address", "169.254.16.53"],
            &["--dns-upstream", "dns.google"],
            &["--deny-prefix", "169.254.169.254"],
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn the_network_is_configured() {
        let args = parse(&[]).unwrap();
        assert_eq!(args.network, network::Config::default());
        assert_eq!(args.network.pod_prefix, None);
        let args = parse(&[
            "--pod-prefix",
            "fd00:fe05:1::/64",
            "--pod-ipv4-range",
            "10.77.16.0/24",
            "--dns-address",
            "10.77.0.53",
            "--dns-upstream",
            "9.9.9.9",
            "--dns-upstream",
            "2620:fe::fe",
            "--deny-prefix",
            "169.254.169.254/32",
            "--deny-prefix",
            "fd00:ec2::/64",
        ])
        .unwrap();
        let network = args.network;
        assert_eq!(
            network.pod_prefix,
            Some(("fd00:fe05:1::".parse().unwrap(), 64))
        );
        assert_eq!(network.pod_ipv4_range, ("10.77.16.0".parse().unwrap(), 24));
        assert_eq!(
            network.dns_address,
            "10.77.0.53".parse::<std::net::Ipv4Addr>().unwrap()
        );
        assert_eq!(
            network.dns_upstreams,
            [
                "9.9.9.9".parse::<std::net::IpAddr>().unwrap(),
                "2620:fe::fe".parse().unwrap()
            ]
        );
        assert_eq!(
            network.deny_prefixes,
            [
                "169.254.169.254/32".parse().unwrap(),
                "fd00:ec2::/64".parse().unwrap()
            ]
        );
    }
}
