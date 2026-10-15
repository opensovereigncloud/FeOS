// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' network (ADR 0010, 0012, 0013). Pods are IPv6 only: each has
//! an address of the node's prefix, routed to its TAP device, a link of
//! its own between the node and the pod, on which the node is `fe80::1`.
//! IPv4 goes through NAT64, names through feosd's DNS64 resolver; the
//! pods' security groups become nftables rules; and their traffic becomes
//! records.

mod addresses;
mod devices;
mod rules;

pub use rules::TABLE;

use addresses::{Ipv6Addresses, Pool};
use dns_service::Resolver;
use feos_proto::network_service::{traffic_record, TrafficRecord};
use feos_proto::pod_service::{PodInfo, PodNetwork, PodSpec};
use feos_utils::nft::{self, Family, Table};
use log::{error, info, warn};
use nat64::Nat64;
use network_service::{compile, delta, Delta, Policy, Prefix, SecurityGroups};
use std::collections::{HashMap, HashSet};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::path::Path;
use std::str::FromStr;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, RwLock};
use tokio::net::{TcpListener, UdpSocket};
use tokio::sync::broadcast;
use tonic::Status;
use traffic_service::Collector;

/// The node's address on every pod's TAP device: the pods' gateway.
pub const GATEWAY: Ipv6Addr = Ipv6Addr::new(0xfe80, 0, 0, 0, 0, 0, 0, 1);
/// The device that holds the resolver's address.
pub const DNS_DEVICE: &str = "feos-dns";
/// The node's default upstream resolvers: Google's and Cloudflare's.
pub const DNS_UPSTREAMS: [&str; 4] = [
    "2001:4860:4860::8888",
    "2606:4700:4700::1111",
    "8.8.8.8",
    "1.1.1.1",
];
/// Records kept for the calls of the traffic API that fall behind.
pub const RECORDS: usize = 4096;
/// The bridge of the first version of the network, with IPv4 on it.
const OLD_BRIDGE: &str = "feos-pods";
const DNS_PORT: u16 = 53;

/// Where the node translates pods' IPv4 traffic (`--nat64`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Nat64Mode {
    /// On the node, with an IPv4 address of its pool per pod.
    #[default]
    Node,
    /// In the network: the node routes the NAT64 prefix like any other.
    Network,
}

impl FromStr for Nat64Mode {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, String> {
        match s {
            "node" => Ok(Self::Node),
            "network" => Ok(Self::Network),
            other => Err(format!("NAT64 {other:?}: node or network")),
        }
    }
}

/// How the node builds the pods' network: feosd's flags.
#[derive(Clone, Debug, PartialEq)]
pub struct Config {
    /// The prefix the network routes to the node (`--pod-prefix`).
    /// Without, pods with a network fail.
    pub pod_prefix: Option<(Ipv6Addr, u8)>,
    pub nat64: Nat64Mode,
    /// The NAT64 prefix, a /96 (`--nat64-prefix`).
    pub nat64_prefix: Ipv6Addr,
    /// The IPv4 addresses that stand for pods with `--nat64=node`
    /// (`--nat64-pool`).
    pub nat64_pool: (Ipv4Addr, u8),
    /// The resolver's address on every node (`--dns-address`).
    pub dns_address: Ipv6Addr,
    /// The upstreams of pods that name none (`--dns-upstream`).
    pub dns_upstreams: Vec<IpAddr>,
    /// What no pod reaches (`--deny-prefix`).
    pub deny_prefixes: Vec<Prefix>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            pod_prefix: None,
            nat64: Nat64Mode::Node,
            nat64_prefix: nat64::PREFIX,
            nat64_pool: (nat64::POOL, nat64::POOL_LEN),
            dns_address: dns_service::ADDRESS,
            dns_upstreams: DNS_UPSTREAMS
                .iter()
                .filter_map(|address| address.parse().ok())
                .collect(),
            deny_prefixes: Vec::new(),
        }
    }
}

/// `--pod-prefix`: an IPv6 prefix with room for pods.
pub fn parse_pod_prefix(s: &str) -> Result<(Ipv6Addr, u8), String> {
    match s.parse::<Prefix>()? {
        Prefix::V6(address, len) if (1..=120).contains(&len) => Ok((address, len)),
        Prefix::V6(..) => Err(format!("pod prefix {s}: from /1 to /120")),
        Prefix::V4(..) => Err(format!("pod prefix {s}: pods are IPv6 only")),
    }
}

/// `--nat64-prefix`: an IPv6 /96.
pub fn parse_nat64_prefix(s: &str) -> Result<Ipv6Addr, String> {
    match s.parse::<Prefix>()? {
        Prefix::V6(address, 96) => Ok(address),
        _ => Err(format!("NAT64 prefix {s}: an IPv6 /96")),
    }
}

/// `--nat64-pool`: an IPv4 prefix of /30 or more addresses.
pub fn parse_nat64_pool(s: &str) -> Result<(Ipv4Addr, u8), String> {
    match s.parse::<Prefix>()? {
        Prefix::V4(address, len) if (1..=30).contains(&len) => Ok((address, len)),
        _ => Err(format!("NAT64 pool {s}: an IPv4 prefix from /1 to /30")),
    }
}

/// A pod's place on the network, for its microVM.
#[derive(Clone, Debug, PartialEq)]
pub struct Attachment {
    pub tap: String,
    pub mac: [u8; 6],
    pub address: Ipv6Addr,
    /// The resolver.
    pub dns: Ipv6Addr,
}

/// The pod's TAP device: `pod-` and the first 8 hex digits of its ID.
pub fn tap_name(pod_id: &str) -> String {
    format!("pod-{}", id_hex(pod_id))
}

/// The guest's MAC address: locally administered, with the same digits of
/// the pod's ID as its TAP device, so that it stays the pod's.
pub fn mac(pod_id: &str) -> [u8; 6] {
    let hex = id_hex(pod_id);
    let byte = |i: usize| {
        hex.get(2 * i..2 * i + 2)
            .and_then(|byte| u8::from_str_radix(byte, 16).ok())
            .unwrap_or(0)
    };
    [0x06, 0x00, byte(0), byte(1), byte(2), byte(3)]
}

fn id_hex(pod_id: &str) -> String {
    pod_id.chars().filter(|c| *c != '-').take(8).collect()
}

pub fn mac_string(mac: [u8; 6]) -> String {
    mac.iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<Vec<_>>()
        .join(":")
}

/// Whether the pod has a network: unless its spec says none.
fn has_network(info: &PodInfo) -> bool {
    info.spec.as_ref().is_some_and(crate::spec::has_network)
}

fn pod_network(info: &PodInfo) -> PodNetwork {
    info.spec
        .as_ref()
        .and_then(|spec| spec.network.clone())
        .unwrap_or_default()
}

/// The pods' network on this node.
pub struct Network {
    config: Config,
    groups: SecurityGroups,
    records: broadcast::Sender<TrafficRecord>,
    /// What the network is made of, or why it is not there: pods with a
    /// network then fail.
    parts: Result<Parts, String>,
    /// Set when the rules could not be written at the start.
    broken: Mutex<Option<String>>,
    directory: Arc<Directory>,
    /// The policy and the table in the kernel.
    applied: tokio::sync::Mutex<Option<(Policy, Table)>>,
    /// The node's addresses, as last read.
    node_addresses: Mutex<Vec<IpAddr>>,
    collector: Mutex<Option<Collector>>,
}

struct Parts {
    ipv6: Ipv6Addresses,
    /// With `--nat64=node`.
    pool: Option<Pool>,
    nat64: Option<Nat64>,
    resolver: Arc<Resolver>,
}

impl Network {
    /// Sets the node up for pods: removes the first version's bridge and
    /// tables, turns forwarding on, routes the pod prefix as unreachable,
    /// gives the resolver its address and sockets, and attaches NAT64 or
    /// detaches it. A failure is logged, and fails the pods with a network
    /// from then on: feosd goes on serving the others.
    pub async fn start(
        config: Config,
        dir: &Path,
        groups: SecurityGroups,
        records: broadcast::Sender<TrafficRecord>,
    ) -> Self {
        let parts = match config.pod_prefix {
            Some(prefix) => match set_up(&config, prefix, dir, &records).await {
                Ok(parts) => {
                    info!(
                        "Network: Pods get addresses of {}/{}, NAT64 {:?} under {}/96, DNS at {}",
                        prefix.0, prefix.1, config.nat64, config.nat64_prefix, config.dns_address
                    );
                    Ok(parts)
                }
                Err(e) => {
                    error!(
                        "Network: The pods' network is not set up, pods with a network fail: {e}"
                    );
                    Err(format!("the pods' network is not set up: {e}"))
                }
            },
            None => {
                if let Err(e) = remove_old_network(true).await {
                    warn!("Network: Failed to remove the IPv4 network: {e}");
                }
                warn!("Network: No --pod-prefix: pods with a network fail");
                Err("feosd has no --pod-prefix: pods with a network cannot run".to_string())
            }
        };
        Self {
            config,
            groups,
            records,
            parts,
            broken: Mutex::new(None),
            directory: Arc::default(),
            applied: tokio::sync::Mutex::new(None),
            node_addresses: Mutex::new(Vec::new()),
            collector: Mutex::new(None),
        }
    }

    pub fn groups(&self) -> &SecurityGroups {
        &self.groups
    }

    /// The parts, if the network works.
    fn parts(&self) -> Result<&Parts, Status> {
        if let Some(broken) = lock(&self.broken).as_ref() {
            return Err(Status::failed_precondition(broken.clone()));
        }
        self.parts
            .as_ref()
            .map_err(|e| Status::failed_precondition(e.clone()))
    }

    /// Marks the network as not working.
    pub fn fail(&self, reason: String) {
        error!("Network: {reason}; pods with a network fail");
        *lock(&self.broken) = Some(reason);
    }

    /// Checks the network of a pod's spec: its security groups' IDs and
    /// its upstream resolvers.
    pub fn check(&self, spec: &PodSpec) -> Result<(), Status> {
        let Some(network) = &spec.network else {
            return Ok(());
        };
        for id in &network.security_groups {
            network_service::check_id(id)?;
        }
        self.upstreams(network)?;
        Ok(())
    }

    /// The pod's upstream resolvers, port 53. An address under the NAT64
    /// prefix is asked as the IPv4 address it stands for: the node itself
    /// has no address that NAT64 translates.
    fn upstreams(&self, network: &PodNetwork) -> Result<Vec<SocketAddr>, Status> {
        network
            .dns_upstreams
            .iter()
            .map(|upstream| {
                let address = upstream.parse::<IpAddr>().map_err(|_| {
                    Status::invalid_argument(format!(
                        "DNS upstream {upstream:?} is not an IP address"
                    ))
                })?;
                let address = match address {
                    IpAddr::V6(v6) => {
                        nat64::extract(self.config.nat64_prefix, v6).map_or(address, IpAddr::V4)
                    }
                    v4 => v4,
                };
                if let Some(why) = self.refused_upstream(address) {
                    return Err(Status::invalid_argument(format!(
                        "DNS upstream {upstream}: {why}"
                    )));
                }
                Ok(SocketAddr::new(address, DNS_PORT))
            })
            .collect()
    }

    /// Why a pod may not have its queries sent to `address`: one it may not
    /// reach itself, or the resolver.
    fn refused_upstream(&self, address: IpAddr) -> Option<&'static str> {
        if address == IpAddr::V6(self.config.dns_address) {
            return Some("it is the resolver itself");
        }
        let special = match address {
            IpAddr::V4(v4) => {
                v4.is_loopback()
                    || v4.is_unspecified()
                    || v4.is_link_local()
                    || v4.is_multicast()
                    || v4.is_broadcast()
            }
            IpAddr::V6(v6) => {
                v6.is_loopback()
                    || v6.is_unspecified()
                    || v6.is_multicast()
                    || v6.is_unicast_link_local()
            }
        };
        if special {
            return Some("not an address a resolver has");
        }
        let host = match address {
            IpAddr::V4(v4) => Prefix::V4(v4, 32),
            IpAddr::V6(v6) => Prefix::V6(v6, 128),
        };
        if self
            .config
            .deny_prefixes
            .iter()
            .any(|denied| denied.contains(&host))
        {
            return Some("it is in a denied prefix");
        }
        if let (Some((prefix, len)), IpAddr::V6(_)) = (self.config.pod_prefix, address)
            && Prefix::V6(prefix, len).contains(&host)
        {
            return Some("it is a pod's");
        }
        let (pool, len) = self.config.nat64_pool;
        if self.config.nat64 == Nat64Mode::Node && Prefix::V4(pool, len).contains(&host) {
            return Some("it is in the NAT64 pool");
        }
        if lock(&self.node_addresses).contains(&address) {
            return Some("it is the node's");
        }
        None
    }

    /// Gives a new pod its addresses: an IPv6 address of the prefix, and
    /// with `--nat64=node` an IPv4 address of the pool. `pods` are the
    /// others.
    pub fn allocate<'a>(
        &self,
        pods: impl Iterator<Item = &'a PodInfo>,
        info: &mut PodInfo,
    ) -> Result<(), Status> {
        let parts = self.parts()?;
        let tap = tap_name(&info.pod_id);
        let mut ipv6 = HashSet::new();
        let mut ipv4 = HashSet::new();
        for pod in pods.filter(|pod| has_network(pod)) {
            if tap_name(&pod.pod_id) == tap {
                return Err(Status::already_exists(format!(
                    "pod {} has the TAP device {tap} already: the first 8 digits of a pod's ID tell it",
                    pod.pod_id
                )));
            }
            ipv6.extend(pod.ipv6_address.parse::<Ipv6Addr>());
            ipv4.extend(pod.ipv4_address.parse::<Ipv4Addr>());
        }
        let exhausted = |e: io::Error| Status::resource_exhausted(format!("no address: {e}"));
        info.ipv6_address = parts.ipv6.allocate(&ipv6).map_err(exhausted)?.to_string();
        info.ipv4_address = match &parts.pool {
            Some(pool) => pool.allocate(&ipv4).map_err(exhausted)?.to_string(),
            None => String::new(),
        };
        Ok(())
    }

    /// Gives pods kept from before what this network needs: an IPv6
    /// address of the prefix to those without one, such as those of the
    /// first version's IPv4 network, and an IPv4 address of the pool with
    /// `--nat64=node`, or none without. Returns the IDs of those changed.
    pub fn adopt(&self, pods: &mut [PodInfo]) -> Vec<String> {
        let Ok(parts) = self.parts() else {
            return Vec::new();
        };
        // What pods keep comes first; the others get what is left.
        let mut ipv6 = HashSet::new();
        let mut ipv4 = HashSet::new();
        let mut kept = Vec::with_capacity(pods.len());
        for pod in pods.iter() {
            let network = has_network(pod);
            let v6 = pod.ipv6_address.parse::<Ipv6Addr>().ok();
            let v6 = v6.filter(|a| network && parts.ipv6.contains(*a) && ipv6.insert(*a));
            let v4 = pod.ipv4_address.parse::<Ipv4Addr>().ok().filter(|a| {
                network
                    && parts.pool.as_ref().is_some_and(|pool| pool.contains(*a))
                    && ipv4.insert(*a)
            });
            kept.push((v6, v4));
        }
        let mut changed = Vec::new();
        for (pod, (v6, v4)) in pods.iter_mut().zip(kept) {
            if !has_network(pod) {
                continue;
            }
            let before = (pod.ipv6_address.clone(), pod.ipv4_address.clone());
            if v6.is_none() {
                match parts.ipv6.allocate(&ipv6) {
                    Ok(address) => {
                        ipv6.insert(address);
                        pod.ipv6_address = address.to_string();
                    }
                    Err(e) => warn!("Network: No address for pod {}: {e}", pod.pod_id),
                }
            }
            pod.ipv4_address = match (&parts.pool, v4) {
                (None, _) => String::new(),
                (Some(_), Some(kept)) => kept.to_string(),
                (Some(pool), None) => match pool.allocate(&ipv4) {
                    Ok(address) => {
                        ipv4.insert(address);
                        address.to_string()
                    }
                    Err(e) => {
                        warn!("Network: No NAT64 address for pod {}: {e}", pod.pod_id);
                        String::new()
                    }
                },
            };
            if (&pod.ipv6_address, &pod.ipv4_address) != (&before.0, &before.1) {
                info!(
                    "Network: Pod {} has the address {}{}",
                    pod.pod_id,
                    pod.ipv6_address,
                    match pod.ipv4_address.as_str() {
                        "" => String::new(),
                        v4 => format!(", {v4} behind NAT64"),
                    }
                );
                changed.push(pod.pod_id.clone());
            }
        }
        changed
    }

    /// Brings NAT64's mappings and the resolver's pods in line with the
    /// pods kept: after a restart of feosd.
    pub fn sync(&self, pods: &[PodInfo]) {
        let Ok(parts) = self.parts() else {
            return;
        };
        let mut wanted = HashMap::new();
        for pod in pods.iter().filter(|pod| has_network(pod)) {
            let Ok(address) = pod.ipv6_address.parse::<Ipv6Addr>() else {
                continue;
            };
            if let Ok(ipv4) = pod.ipv4_address.parse::<Ipv4Addr>() {
                wanted.insert(address, ipv4);
            }
            match self.upstreams(&pod_network(pod)) {
                Ok(upstreams) => parts.resolver.set_client(address, &pod.pod_id, upstreams),
                Err(e) => warn!("Network: Pod {} gets no DNS: {}", pod.pod_id, e.message()),
            }
        }
        let Some(nat64) = &parts.nat64 else {
            return;
        };
        match nat64.mappings() {
            Ok(mappings) => {
                for (pod, ipv4) in mappings {
                    if wanted.get(&pod) != Some(&ipv4)
                        && let Err(e) = nat64.unmap(pod)
                    {
                        warn!("Network: Failed to unmap {pod} from NAT64: {e}");
                    }
                }
            }
            Err(e) => warn!("Network: NAT64's mappings do not read: {e}"),
        }
        for (pod, ipv4) in wanted {
            if let Err(e) = nat64.map(pod, ipv4) {
                warn!("Network: Failed to map {pod} to {ipv4} in NAT64: {e}");
            }
        }
    }

    /// Puts the pod on the network, if it has one: before every boot and
    /// resume. Fails if the node does not have the pod's security groups.
    pub async fn attach(&self, info: &PodInfo) -> Result<Option<Attachment>, Status> {
        if !has_network(info) {
            return Ok(None);
        }
        let parts = self.parts()?;
        let pod_id = &info.pod_id;
        let address = info
            .ipv6_address
            .parse::<Ipv6Addr>()
            .ok()
            .filter(|address| parts.ipv6.contains(*address))
            .ok_or_else(|| {
                Status::failed_precondition(format!(
                    "pod {pod_id} has no address of the pod prefix"
                ))
            })?;
        let network = pod_network(info);
        let missing = self.groups.missing(&network.security_groups);
        if !missing.is_empty() {
            return Err(Status::failed_precondition(format!(
                "the node does not have the security groups {}",
                missing.join(", ")
            )));
        }
        let upstreams = self
            .upstreams(&network)
            .map_err(|e| Status::failed_precondition(e.message()))?;
        let tap = tap_name(pod_id);
        let handle = devices::connect()
            .await
            .map_err(|e| Status::internal(format!("netlink: {e}")))?;
        devices::tap(&handle, &tap, GATEWAY, address)
            .await
            .map_err(|e| Status::internal(format!("attach {tap}: {e}")))?;
        if let (Some(nat64), Ok(ipv4)) = (&parts.nat64, info.ipv4_address.parse()) {
            nat64
                .map(address, ipv4)
                .map_err(|e| Status::internal(format!("NAT64 for {address}: {e}")))?;
        }
        parts.resolver.set_client(address, pod_id, upstreams);
        Ok(Some(Attachment {
            tap,
            mac: mac(pod_id),
            address,
            dns: self.config.dns_address,
        }))
    }

    /// Takes a deleted pod off the network: its TAP device, its NAT64
    /// mapping and its resolver.
    pub fn detach(&self, info: &PodInfo) -> io::Result<()> {
        if !has_network(info) {
            return Ok(());
        }
        if let (Ok(parts), Ok(address)) = (&self.parts, info.ipv6_address.parse::<Ipv6Addr>()) {
            parts.resolver.remove_client(address);
            if let Some(nat64) = &parts.nat64
                && let Err(e) = nat64.unmap(address)
            {
                warn!("Network: Failed to unmap {address} from NAT64: {e}");
            }
        }
        let tap = tap_name(&info.pod_id);
        microvm_service::vmm::jail::tap::delete(&tap)
            .map_err(|e| io::Error::new(e.kind(), format!("delete {tap}: {e}")))
    }

    /// Writes the rules for these pods and the node's security groups:
    /// the whole table the first time and whenever pods or rules change,
    /// only the elements of sets that changed otherwise.
    pub async fn apply(&self, pods: &[PodInfo]) -> io::Result<()> {
        if self.parts().is_err() {
            return Ok(());
        }
        let mut applied = self.applied.lock().await;
        let node = match devices::connect().await {
            Ok(handle) => devices::node_addresses(&handle).await,
            Err(e) => Err(e),
        };
        let node = match node {
            Ok(node) => {
                *lock(&self.node_addresses) = node.clone();
                node
            }
            Err(e) => {
                warn!("Network: The node's addresses do not read: {e}");
                lock(&self.node_addresses).clone()
            }
        };
        let members: Vec<network_service::Pod> = pods
            .iter()
            .filter(|pod| has_network(pod))
            .filter_map(|pod| {
                Some(network_service::Pod {
                    pod_id: pod.pod_id.clone(),
                    tap: tap_name(&pod.pod_id),
                    address: pod.ipv6_address.parse().ok()?,
                    groups: pod_network(pod).security_groups,
                })
            })
            .collect();
        self.directory.set(&members);
        let policy = compile(
            &self.groups.snapshot(),
            &members,
            self.config.nat64_prefix,
            &self.denied(&node),
        );
        let table = rules::table(&policy, &self.rules());
        match applied
            .as_ref()
            .map(|(old, table)| (delta(old, &policy), table))
        {
            Some((Delta::Elements(changes), _)) if changes.is_empty() => {}
            Some((Delta::Elements(changes), old)) => {
                if let Err(e) = nft::update(old, &rules::element_ops(&changes)) {
                    warn!("Network: Failed to change the sets, replacing the table: {e}");
                    nft::replace(&table)?;
                }
            }
            _ => nft::replace(&table)?,
        }
        *applied = Some((policy, table));
        Ok(())
    }

    /// What no pod reaches: the denied prefixes, the node's own
    /// addresses, and with `--nat64=node` the pool.
    fn denied(&self, node: &[IpAddr]) -> Vec<Prefix> {
        let mut denied = self.config.deny_prefixes.clone();
        denied.extend(node.iter().map(|address| match *address {
            IpAddr::V4(v4) => Prefix::V4(v4, 32),
            IpAddr::V6(v6) => Prefix::V6(v6, 128),
        }));
        if self.config.nat64 == Nat64Mode::Node {
            let (pool, len) = self.config.nat64_pool;
            denied.push(Prefix::V4(pool, len));
        }
        denied
    }

    fn rules(&self) -> rules::Options {
        rules::Options {
            dns: self.config.dns_address,
            pool: (self.config.nat64 == Nat64Mode::Node).then_some(self.config.nat64_pool),
        }
    }

    /// Starts the records of flows, drops and server names: once the
    /// table is written, whose `ct` expressions load connection tracking.
    pub fn start_records(&self) {
        if self.parts().is_err() {
            return;
        }
        if let Err(e) = traffic_service::enable_accounting() {
            warn!("Network: Flows have no counters: {e}");
        }
        let (sender, receiver) = broadcast::channel(RECORDS);
        let config = traffic_service::Config {
            nat64_prefix: self.config.nat64_prefix,
            ..Default::default()
        };
        match Collector::start(self.directory.clone(), config, sender) {
            Ok(collector) => {
                *lock(&self.collector) = Some(collector);
                // The resolver records the pods' queries to it.
                let resolver = self.config.dns_address;
                tokio::spawn(forward_records(
                    receiver,
                    self.records.clone(),
                    move |record| !is_dns_flow(record, resolver),
                ));
                info!("Network: Recording the pods' flows, drops and server names");
            }
            Err(e) => error!("Network: No records of flows, drops or server names: {e}"),
        }
    }
}

/// Sets the node up for pods, see [`Network::start`].
async fn set_up(
    config: &Config,
    (prefix, len): (Ipv6Addr, u8),
    dir: &Path,
    records: &broadcast::Sender<TrafficRecord>,
) -> io::Result<Parts> {
    // The bridge's route to 10.200.0.0/16 would take NAT64's pool.
    remove_old_network(false).await?;
    let handle = devices::connect().await?;
    nat64::enable_forwarding()?;
    devices::route_unreachable(&handle, prefix, len).await?;
    devices::dummy_with_address(&handle, DNS_DEVICE, config.dns_address).await?;

    // Where the pods cannot reach the IPv6 internet, as from a ULA prefix,
    // every name goes through NAT64 (ADR 0010).
    let ula = u128::from(prefix) >> 121 == 0xfc >> 1;
    let resolver = Resolver::new(dns_service::Config {
        nat64_prefix: config.nat64_prefix,
        exclude_all_aaaa: ula,
        default_upstreams: config
            .dns_upstreams
            .iter()
            .map(|address| SocketAddr::new(*address, DNS_PORT))
            .collect(),
        ..Default::default()
    });
    let address = SocketAddr::new(config.dns_address.into(), DNS_PORT);
    let udp = UdpSocket::bind(address)
        .await
        .map_err(|e| io::Error::new(e.kind(), format!("DNS: bind udp {address}: {e}")))?;
    let tcp = TcpListener::bind(address)
        .await
        .map_err(|e| io::Error::new(e.kind(), format!("DNS: bind tcp {address}: {e}")))?;
    tokio::spawn(forward_records(
        resolver.subscribe(),
        records.clone(),
        |_| true,
    ));
    let serving = resolver.clone();
    tokio::spawn(async move {
        if let Err(e) = serving.serve(udp, tcp).await {
            error!("Network: The resolver stopped: {e}");
        }
    });

    let (pool, pool_len) = config.nat64_pool;
    let nat64_config = nat64::Config {
        prefix: config.nat64_prefix,
        pool,
        pool_len,
        pin_dir: nat64::PIN_DIR.into(),
    };
    let (nat64, pool) = match config.nat64 {
        Nat64Mode::Node => (
            Some(Nat64::attach(&nat64_config).await?),
            Some(Pool::new(dir, pool, pool_len)),
        ),
        Nat64Mode::Network => {
            Nat64::detach(Path::new(nat64::PIN_DIR)).await?;
            (None, None)
        }
    };
    Ok(Parts {
        ipv6: Ipv6Addresses::new(dir, prefix, len),
        pool,
        nat64,
        resolver,
    })
}

/// Removes the first version of the network: its bridge and the table
/// `bridge feos`, on a node whose kernel has the bridge family; with
/// `inet`, the table `inet feos` too, which a working network replaces
/// anyway.
async fn remove_old_network(inet: bool) -> io::Result<()> {
    match nft::delete(Family::Bridge, TABLE) {
        Ok(()) => {}
        // No bridge family, as in feos-host-init's kernel.
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) if e.to_string().contains("not supported") => {}
        Err(e) => warn!("Network: Failed to remove the table bridge {TABLE}: {e}"),
    }
    if inet && let Err(e) = nft::delete(Family::Inet, TABLE) {
        warn!("Network: Failed to remove the table inet {TABLE}: {e}");
    }
    let handle = devices::connect().await?;
    if devices::delete_link(&handle, OLD_BRIDGE).await? {
        info!("Network: Removed the bridge {OLD_BRIDGE} of the IPv4 network");
    }
    Ok(())
}

/// Whether the record is the flow of a pod's query to the resolver, which
/// the resolver's own record covers.
fn is_dns_flow(record: &TrafficRecord, resolver: Ipv6Addr) -> bool {
    let Some(traffic_record::Record::Flow(flow)) = &record.record else {
        return false;
    };
    flow.tuple.as_ref().is_some_and(|tuple| {
        tuple.destination_port == u32::from(DNS_PORT)
            && tuple.destination_address.parse::<Ipv6Addr>().ok() == Some(resolver)
    })
}

/// Forwards the records that `keep` keeps, until `from` closes. What was
/// lost on the way is counted in the next record's missed, and so is what
/// a record left out counted.
async fn forward_records(
    mut from: broadcast::Receiver<TrafficRecord>,
    to: broadcast::Sender<TrafficRecord>,
    keep: impl Fn(&TrafficRecord) -> bool + Send + 'static,
) {
    let mut missed = 0;
    loop {
        match from.recv().await {
            Ok(mut record) => {
                if !keep(&record) {
                    missed += record.missed;
                    continue;
                }
                record.missed += std::mem::take(&mut missed);
                // Nobody may be listening.
                let _ = to.send(record);
            }
            Err(broadcast::error::RecvError::Lagged(lost)) => missed += lost,
            Err(broadcast::error::RecvError::Closed) => return,
        }
    }
}

/// Which pod an address or a TAP device is, for the records.
#[derive(Default)]
struct Directory {
    addresses: RwLock<HashMap<Ipv6Addr, String>>,
    taps: RwLock<HashMap<String, String>>,
}

impl Directory {
    fn set(&self, pods: &[network_service::Pod]) {
        let addresses = pods
            .iter()
            .map(|pod| (pod.address, pod.pod_id.clone()))
            .collect();
        let taps = pods
            .iter()
            .map(|pod| (pod.tap.clone(), pod.pod_id.clone()))
            .collect();
        *self
            .addresses
            .write()
            .unwrap_or_else(PoisonError::into_inner) = addresses;
        *self.taps.write().unwrap_or_else(PoisonError::into_inner) = taps;
    }
}

impl traffic_service::Pods for Directory {
    fn by_address(&self, address: &Ipv6Addr) -> Option<String> {
        self.addresses
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(address)
            .cloned()
    }

    fn by_interface(&self, name: &str) -> Option<String> {
        self.taps
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(name)
            .cloned()
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::{Flow, Tuple};
    use traffic_service::Pods as _;

    fn network(config: Config) -> Network {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path()).unwrap();
        Network {
            config,
            groups,
            records: broadcast::channel(4).0,
            parts: Err("not set up".to_string()),
            broken: Mutex::new(None),
            directory: Arc::default(),
            applied: tokio::sync::Mutex::new(None),
            node_addresses: Mutex::new(vec!["192.0.2.7".parse().unwrap()]),
            collector: Mutex::new(None),
        }
    }

    fn with_upstreams(upstreams: &[&str]) -> PodNetwork {
        PodNetwork {
            dns_upstreams: upstreams.iter().map(|u| u.to_string()).collect(),
            ..Default::default()
        }
    }

    #[test]
    fn names_and_macs_come_from_the_pod() {
        let id = "8d99e180-1694-4d94-a42e-097769b89510";
        assert_eq!(tap_name(id), "pod-8d99e180");
        assert!(tap_name(id).len() < libc::IFNAMSIZ);
        assert_eq!(mac_string(mac(id)), "06:00:8d:99:e1:80");
    }

    #[test]
    fn flags_are_parsed() {
        assert_eq!(
            parse_pod_prefix("fd00:fe05:1::/64"),
            Ok(("fd00:fe05:1::".parse().unwrap(), 64))
        );
        assert!(parse_pod_prefix("fd00:fe05:1::1/64").is_err(), "host bits");
        assert!(parse_pod_prefix("fd00::/127").is_err(), "no room");
        assert!(parse_pod_prefix("10.0.0.0/8").is_err());
        assert_eq!(
            parse_nat64_prefix("64:ff9b::/96"),
            Ok("64:ff9b::".parse().unwrap())
        );
        assert!(parse_nat64_prefix("64:ff9b::/64").is_err());
        assert_eq!(
            parse_nat64_pool("10.200.0.0/16"),
            Ok((Ipv4Addr::new(10, 200, 0, 0), 16))
        );
        assert!(parse_nat64_pool("10.200.0.0/31").is_err());
        assert!(parse_nat64_pool("fd00::/64").is_err());
        assert_eq!("network".parse(), Ok(Nat64Mode::Network));
        assert!("both".parse::<Nat64Mode>().is_err());
        let config = Config::default();
        assert_eq!(config.dns_upstreams.len(), 4);
        assert_eq!(config.dns_address, dns_service::ADDRESS);
    }

    #[test]
    fn upstreams_a_pod_may_not_reach_are_refused() {
        let network = network(Config {
            pod_prefix: Some(("fd00:fe05:1::".parse().unwrap(), 64)),
            deny_prefixes: vec![
                "169.254.169.254/32".parse().unwrap(),
                "2001:db8::/32".parse().unwrap(),
                "100.64.0.0/10".parse().unwrap(),
            ],
            ..Config::default()
        });
        let check = |upstreams: &[&str]| network.upstreams(&with_upstreams(upstreams));
        assert_eq!(
            check(&["2606:4700:4700::1111", "9.9.9.9"]).unwrap(),
            [
                "[2606:4700:4700::1111]:53".parse().unwrap(),
                "9.9.9.9:53".parse().unwrap()
            ]
        );
        // An address under the NAT64 prefix is asked over IPv4.
        assert_eq!(
            check(&["64:ff9b::909:909"]).unwrap(),
            ["9.9.9.9:53".parse::<SocketAddr>().unwrap()]
        );
        for refused in [
            "fdfd:7b5:61d9::53",
            "100.64.1.1",
            "64:ff9b::6440:101",
            "2001:db8::53",
            "127.0.0.1",
            "::1",
            "fe80::1",
            "fd00:fe05:1::2",
            "10.200.0.9",
            "192.0.2.7",
            "dns.google",
        ] {
            let error = check(&[refused]).unwrap_err();
            assert_eq!(error.code(), tonic::Code::InvalidArgument, "{refused}");
        }
        let spec = PodSpec {
            network: Some(PodNetwork {
                security_groups: vec!["a/b".to_string()],
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(network.check(&spec).is_err(), "a bad group ID");
    }

    #[test]
    fn denied_has_the_node_and_the_pool() {
        let network = network(Config {
            deny_prefixes: vec!["169.254.169.254/32".parse().unwrap()],
            ..Config::default()
        });
        let node = ["192.0.2.7".parse().unwrap(), "2001:db8::7".parse().unwrap()];
        assert_eq!(
            network.denied(&node),
            [
                "169.254.169.254/32".parse().unwrap(),
                "192.0.2.7/32".parse().unwrap(),
                "2001:db8::7/128".parse().unwrap(),
                "10.200.0.0/16".parse().unwrap(),
            ]
        );
        let network_nat64 = self::tests::network(Config {
            nat64: Nat64Mode::Network,
            ..Config::default()
        });
        assert!(network_nat64.denied(&[]).is_empty());
        assert_eq!(network_nat64.rules().pool, None);
    }

    #[test]
    fn without_a_prefix_pods_with_a_network_fail() {
        let network = network(Config::default());
        let mut info = PodInfo::default();
        let error = network.allocate(std::iter::empty(), &mut info).unwrap_err();
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        network.fail("nftables: no".to_string());
        assert_eq!(network.parts().err().unwrap().message(), "nftables: no");
    }

    #[test]
    fn the_directory_knows_pods_by_address_and_tap() {
        let directory = Directory::default();
        directory.set(&[network_service::Pod {
            pod_id: "a".to_string(),
            tap: "pod-aaaaaaaa".to_string(),
            address: "fd00::2".parse().unwrap(),
            groups: vec![],
        }]);
        assert_eq!(
            directory.by_address(&"fd00::2".parse().unwrap()),
            Some("a".to_string())
        );
        assert_eq!(
            directory.by_interface("pod-aaaaaaaa"),
            Some("a".to_string())
        );
        assert_eq!(directory.by_interface("eth0"), None);
    }

    #[tokio::test]
    async fn dns_flows_are_left_to_the_resolver() {
        let flow = |destination: &str, port: u32, missed: u64| TrafficRecord {
            pod_id: "a".to_string(),
            missed,
            record: Some(traffic_record::Record::Flow(Flow {
                tuple: Some(Tuple {
                    protocol: 17,
                    destination_address: destination.to_string(),
                    destination_port: port,
                    ..Default::default()
                }),
                ..Default::default()
            })),
            ..Default::default()
        };
        let resolver = dns_service::ADDRESS;
        assert!(is_dns_flow(&flow("fdfd:7b5:61d9::53", 53, 0), resolver));
        assert!(!is_dns_flow(&flow("fdfd:7b5:61d9::53", 853, 0), resolver));
        assert!(!is_dns_flow(&flow("8.8.8.8", 53, 0), resolver));

        let (from, receiver) = broadcast::channel(8);
        let (to, mut records) = broadcast::channel(8);
        let forwarding = tokio::spawn(forward_records(receiver, to, move |record| {
            !is_dns_flow(record, resolver)
        }));
        from.send(flow("fdfd:7b5:61d9::53", 53, 2)).unwrap();
        from.send(flow("8.8.8.8", 53, 1)).unwrap();
        drop(from);
        forwarding.await.unwrap();
        let record = records.recv().await.unwrap();
        // What was left out counts as missed.
        assert_eq!(record.missed, 3);
        assert!(records.try_recv().is_err());
    }
}
