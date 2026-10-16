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
             [--diskd-socket PATH]
             [--control-plane HOST:PORT [--control-plane-name NAME]
              [--trust-domain NAME] [--join-token-file PATH] [--node-dir PATH]]
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
                           (repeatable)
  --diskd-socket PATH      diskd's socket, which the pods' disks come from;
                           /var/lib/feos/disk/diskd.sock by default
  --control-plane HOST:PORT
                           the control plane's port for nodes, which the node
                           joins and connects to (ADR 0015); without, the node
                           is on its own
  --control-plane-name NAME
                           the DNS name the control plane's certificate is
                           checked for, wherever feosd dials; HOST by default
  --trust-domain NAME      the control plane's SPIFFE trust domain; feos.local
                           by default
  --join-token-file PATH   the join token, read while the node has not joined;
                           /etc/feos/join-token by default
  --node-dir PATH          the node's key, certificate and trust bundle;
                           /var/lib/feos/node by default";

#[derive(Debug, Default, PartialEq)]
struct Args {
    tcp: Option<SocketAddr>,
    self_upgrade: bool,
    network: network::Config,
    diskd_socket: Option<std::path::PathBuf>,
    /// The control plane, and how the node joins it.
    fleet: Option<fleet_client::Config>,
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
        let mut control_plane = None;
        let mut control_plane_name = None;
        let mut trust_domain = None;
        let mut join_token_file = None;
        let mut node_dir = None;
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
                "--diskd-socket" => parsed.diskd_socket = Some(value("a path")?.into()),
                "--control-plane" => control_plane = Some(value("HOST:PORT")?),
                "--control-plane-name" => control_plane_name = Some(value("a name")?),
                "--trust-domain" => trust_domain = Some(value("a name")?),
                "--join-token-file" => join_token_file = Some(value("a path")?.into()),
                "--node-dir" => node_dir = Some(value("a path")?.into()),
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
        if let Some(address) = control_plane {
            let trust_domain = trust_domain
                .as_deref()
                .unwrap_or(fleet_client::TRUST_DOMAIN);
            let mut config =
                fleet_client::Config::new(&address, control_plane_name.as_deref(), trust_domain)
                    .map_err(|e| anyhow!("--control-plane: {e}\n{USAGE}"))?;
            if let Some(file) = join_token_file {
                config.join_token_file = file;
            }
            if let Some(dir) = node_dir {
                config.node_dir = dir;
            }
            parsed.fleet = Some(config);
        } else if let Some(trust_domain) = trust_domain {
            fleet_client::check_trust_domain(&trust_domain)
                .map_err(|e| anyhow!("--trust-domain: {e}\n{USAGE}"))?;
        }
        parsed
            .network
            .check()
            .map_err(|e| anyhow!("{e}\n{USAGE}"))?;
        Ok(parsed)
    }
}

fn main() -> Result<()> {
    // The resolver's sockets, if systemd, feos-host-init or the feosd
    // before a self-upgrade handed them over (ADR 0010).
    // SAFETY: the process has no other threads yet.
    let dns_sockets = unsafe { feos_utils::listen_fds::take() };
    serve(dns_sockets)
}

#[tokio::main]
async fn serve(dns_sockets: Vec<(std::os::fd::OwnedFd, String)>) -> Result<()> {
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

    let fleet = args.fleet.map(|config| fleet_client::Config {
        pod_prefix: args
            .network
            .pod_prefix
            .map(|(prefix, len)| format!("{prefix}/{len}"))
            .unwrap_or_default(),
        version: feos_utils::version::full_version_string(),
        ..config
    });
    run_server(ServerOptions {
        tcp: args.tcp,
        network: args.network,
        dns_sockets,
        diskd_socket: args.diskd_socket,
        fleet,
        ..ServerOptions::new(args.restarted_after_upgrade, args.self_upgrade)
    })
    .await?;
    // SIGTERM, and the control plane knows: nothing is left to wait for.
    std::process::exit(0)
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
        let args = parse(&["--diskd-socket", "/run/diskd.sock"]).unwrap();
        assert_eq!(args.diskd_socket, Some("/run/diskd.sock".into()));
        assert_eq!(parse(&[]).unwrap(), Args::default());
    }

    #[test]
    fn the_control_plane_is_configured() {
        assert_eq!(parse(&[]).unwrap().fleet, None);
        let fleet = parse(&["--control-plane", "ferun.example.com:7443"])
            .unwrap()
            .fleet
            .unwrap();
        assert_eq!(fleet.address, "ferun.example.com:7443");
        assert_eq!(fleet.name, "ferun.example.com");
        assert_eq!(fleet.trust_domain, "feos.local");
        assert_eq!(
            fleet.join_token_file,
            std::path::Path::new("/etc/feos/join-token")
        );
        assert_eq!(fleet.node_dir, std::path::Path::new("/var/lib/feos/node"));
        let fleet = parse(&[
            "--control-plane",
            "10.43.0.17:7443",
            "--control-plane-name",
            "ferun-nodes.ferun.svc.cluster.local",
            "--trust-domain",
            "run.fecode.dev",
            "--join-token-file",
            "/run/credentials/feosd/join-token",
            "--node-dir",
            "/srv/feos/node",
        ])
        .unwrap()
        .fleet
        .unwrap();
        assert_eq!(fleet.address, "10.43.0.17:7443");
        assert_eq!(fleet.name, "ferun-nodes.ferun.svc.cluster.local");
        assert_eq!(fleet.trust_domain, "run.fecode.dev");
        assert_eq!(
            fleet.join_token_file,
            std::path::Path::new("/run/credentials/feosd/join-token")
        );
        assert_eq!(fleet.node_dir, std::path::Path::new("/srv/feos/node"));
        for bad in [
            &["--control-plane"][..],
            &["--control-plane", "ferun.example.com"],
            &["--control-plane", "10.43.0.17:7443"],
            &[
                "--control-plane",
                "ferun.example.com:7443",
                "--trust-domain",
                "Feos",
            ],
            &[
                "--control-plane",
                "10.43.0.17:7443",
                "--control-plane-name",
                "10.43.0.17",
            ],
            &["--trust-domain", "feos.local:443"],
            &["--node-dir"],
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
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
