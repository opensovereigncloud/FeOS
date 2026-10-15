// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' network (ADR 0010, 0012, 0013). Each pod has an IPv6 address
//! of the node's prefix, its address on the network, and an IPv4 address
//! of the node's link-local range for leaving through masquerade. Both are
//! routed to its TAP device, a link of its own between the node and the
//! pod, on which the node is `fe80::1` and answers ARP for `169.254.0.1`
//! by proxy. Names go through feosd's resolver; the pods' security groups
//! become nftables rules; and their traffic becomes records.

mod addresses;
mod devices;
mod rules;

pub use rules::TABLE;

use addresses::{Ipv4Addresses, Ipv6Addresses};
use devices::PodLink;
use dns_service::Resolver;
use feos_proto::network_service::{traffic_record, TrafficRecord};
use feos_proto::pod_service::{PodInfo, PodNetwork, PodSpec};
use feos_utils::nft::{self, Family, Table};
use log::{error, info, warn};
use network_service::{compile, delta, Delta, Policy, Prefix, SecurityGroups};
use std::collections::{HashMap, HashSet};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, RwLock};
use std::time::{Duration, SystemTime};
use tokio::net::{TcpListener, UdpSocket};
use tokio::sync::broadcast;
use tonic::Status;
use traffic_service::Collector;

/// The node's address on every pod's TAP device: the pods' IPv6 gateway.
pub const GATEWAY: Ipv6Addr = Ipv6Addr::new(0xfe80, 0, 0, 0, 0, 0, 0, 1);
/// The pods' IPv4 gateway, on their link although outside their /32. The
/// node answers ARP for it on every TAP device by proxy and never holds
/// it: where it is the gateway of the node's own network, as in some
/// clouds, the node would lose its own.
pub const GATEWAY4: Ipv4Addr = Ipv4Addr::new(169, 254, 0, 1);
/// The pods' IPv4 addresses by default (`--pod-ipv4-range`): 4096, of
/// which the first and the last are left out.
pub const POD_IPV4_RANGE: (Ipv4Addr, u8) = (Ipv4Addr::new(169, 254, 16, 0), 20);
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
/// How often the network's setup is tried, and how long apart.
const SET_UP_ATTEMPTS: u32 = 3;
const SET_UP_PAUSE: Duration = Duration::from_secs(1);
const DNS_PORT: u16 = 53;

/// How the node builds the pods' network: feosd's flags.
#[derive(Clone, Debug, PartialEq)]
pub struct Config {
    /// The prefix the network routes to the node (`--pod-prefix`).
    /// Without, pods with a network fail.
    pub pod_prefix: Option<(Ipv6Addr, u8)>,
    /// The pods' IPv4 addresses, a /32 of it each (`--pod-ipv4-range`).
    pub pod_ipv4_range: (Ipv4Addr, u8),
    /// The resolver's address on every node (`--dns-address`).
    pub dns_address: Ipv4Addr,
    /// The upstreams of pods that name none (`--dns-upstream`).
    pub dns_upstreams: Vec<IpAddr>,
    /// What no pod reaches (`--deny-prefix`), of either family.
    pub deny_prefixes: Vec<Prefix>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            pod_prefix: None,
            pod_ipv4_range: POD_IPV4_RANGE,
            dns_address: dns_service::ADDRESS,
            dns_upstreams: DNS_UPSTREAMS
                .iter()
                .filter_map(|address| address.parse().ok())
                .collect(),
            deny_prefixes: Vec::new(),
        }
    }
}

impl Config {
    /// Checks what the flags say together: the node holds the resolver's
    /// address, so it must be one of a host and neither the pods' gateway
    /// nor in their range; and the range must not hold the gateway.
    pub fn check(&self) -> Result<(), String> {
        let dns = self.dns_address;
        if dns.is_unspecified() || dns.is_loopback() || dns.is_multicast() || dns.is_broadcast() {
            return Err(format!("the DNS address {dns} is not one a host has"));
        }
        if dns == GATEWAY4 {
            return Err(format!(
                "the DNS address {dns} is the pods' gateway, which the node must not hold"
            ));
        }
        let (range, len) = self.pod_ipv4_range;
        let range = Prefix::V4(range, len);
        for (what, address) in [("the DNS address", dns), ("the pods' gateway", GATEWAY4)] {
            if range.contains(&Prefix::V4(address, 32)) {
                return Err(format!(
                    "the pods' IPv4 range {range} holds {what} {address}"
                ));
            }
        }
        Ok(())
    }
}

/// `--pod-prefix`: an IPv6 prefix with room for pods.
pub fn parse_pod_prefix(s: &str) -> Result<(Ipv6Addr, u8), String> {
    match s.parse::<Prefix>()? {
        Prefix::V6(address, len) if (1..=120).contains(&len) => Ok((address, len)),
        Prefix::V6(..) => Err(format!("pod prefix {s}: from /1 to /120")),
        Prefix::V4(..) => Err(format!(
            "pod prefix {s}: an IPv6 prefix; the pods' IPv4 range is --pod-ipv4-range"
        )),
    }
}

/// `--pod-ipv4-range`: an IPv4 prefix with room for pods besides its first
/// and last address.
pub fn parse_pod_ipv4_range(s: &str) -> Result<(Ipv4Addr, u8), String> {
    match s.parse::<Prefix>()? {
        Prefix::V4(address, len) if (1..=30).contains(&len) => Ok((address, len)),
        Prefix::V4(..) => Err(format!("pods' IPv4 range {s}: from /1 to /30")),
        Prefix::V6(..) => Err(format!("pods' IPv4 range {s}: an IPv4 prefix")),
    }
}

/// A pod's place on the network, for its microVM.
#[derive(Clone, Debug, PartialEq)]
pub struct Attachment {
    pub tap: String,
    pub mac: [u8; 6],
    pub ipv6: Ipv6Addr,
    pub ipv4: Ipv4Addr,
    /// The resolver.
    pub dns: Ipv4Addr,
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
    ipv4: Ipv4Addresses,
    resolver: Arc<Resolver>,
}

impl Network {
    /// Sets the node up for pods: removes the first version's bridge and
    /// tables, turns forwarding on, routes the pod prefix as unreachable,
    /// and gives the resolver its address and sockets. A failure is logged,
    /// and fails the pods with a network from then on: feosd goes on
    /// serving the others.
    pub async fn start(
        config: Config,
        dir: &Path,
        groups: SecurityGroups,
        records: broadcast::Sender<TrafficRecord>,
    ) -> Self {
        let parts = match config.pod_prefix {
            Some(prefix) => match set_up_again(&config, prefix, dir, &records).await {
                Ok(parts) => {
                    let (range, range_len) = config.pod_ipv4_range;
                    info!(
                        "Network: Pods get addresses of {}/{} and {range}/{range_len}, DNS at {}",
                        prefix.0, prefix.1, config.dns_address
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

    /// The pod's upstream resolvers, port 53.
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
        if address == IpAddr::V4(self.config.dns_address) {
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
        let (range, len) = self.config.pod_ipv4_range;
        let pods = [
            self.config
                .pod_prefix
                .map(|(prefix, len)| Prefix::V6(prefix, len)),
            Some(Prefix::V4(range, len)),
        ];
        if pods.iter().flatten().any(|pods| pods.contains(&host)) {
            return Some("it is a pod's");
        }
        if lock(&self.node_addresses).contains(&address) {
            return Some("it is the node's");
        }
        None
    }

    /// Gives a new pod its addresses: an IPv6 address of the prefix and an
    /// IPv4 address of the range. `pods` are the others.
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
        info.ipv4_address = parts.ipv4.allocate(&ipv4).map_err(exhausted)?.to_string();
        Ok(())
    }

    /// Gives pods kept from before what this network needs: an IPv6
    /// address of the prefix to those without one, such as those of the
    /// first version's IPv4 network, and an IPv4 address of the range to
    /// those without one, such as those of the IPv6 only network or with
    /// an address of the first version's pool. Returns the IDs of those
    /// changed.
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
            let v4 = pod.ipv4_address.parse::<Ipv4Addr>().ok();
            let v4 = v4.filter(|a| network && parts.ipv4.contains(*a) && ipv4.insert(*a));
            kept.push((v6, v4));
        }
        // An address handed out that no pod kept is free.
        if let Err(e) = parts.ipv4.reconcile(&ipv4, SystemTime::now()) {
            warn!("Network: The pods' IPv4 addresses are not kept: {e}");
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
            if v4.is_none() {
                pod.ipv4_address = match parts.ipv4.allocate(&ipv4) {
                    Ok(address) => {
                        ipv4.insert(address);
                        address.to_string()
                    }
                    Err(e) => {
                        warn!("Network: No IPv4 address for pod {}: {e}", pod.pod_id);
                        String::new()
                    }
                };
            }
            if (&pod.ipv6_address, &pod.ipv4_address) != (&before.0, &before.1) {
                info!(
                    "Network: Pod {} has the addresses {} and {}",
                    pod.pod_id, pod.ipv6_address, pod.ipv4_address
                );
                changed.push(pod.pod_id.clone());
            }
        }
        changed
    }

    /// Brings the resolver's pods in line with the pods kept: after a
    /// restart of feosd.
    pub fn sync(&self, pods: &[PodInfo]) {
        let Ok(parts) = self.parts() else {
            return;
        };
        for pod in pods.iter().filter(|pod| has_network(pod)) {
            let Ok(address) = pod.ipv4_address.parse::<Ipv4Addr>() else {
                continue;
            };
            match self.upstreams(&pod_network(pod)) {
                Ok(upstreams) => parts
                    .resolver
                    .set_client(address.into(), &pod.pod_id, upstreams),
                Err(e) => warn!("Network: Pod {} gets no DNS: {}", pod.pod_id, e.message()),
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
        let ipv6 = info
            .ipv6_address
            .parse::<Ipv6Addr>()
            .ok()
            .filter(|address| parts.ipv6.contains(*address))
            .ok_or_else(|| {
                Status::failed_precondition(format!(
                    "pod {pod_id} has no address of the pod prefix"
                ))
            })?;
        let ipv4 = info
            .ipv4_address
            .parse::<Ipv4Addr>()
            .ok()
            .filter(|address| parts.ipv4.contains(*address))
            .ok_or_else(|| {
                Status::failed_precondition(format!(
                    "pod {pod_id} has no IPv4 address of the pods' range"
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
        let link = PodLink {
            gateway: GATEWAY,
            gateway4: GATEWAY4,
            ipv6,
            ipv4,
        };
        devices::tap(&handle, &tap, &link)
            .await
            .map_err(|e| Status::internal(format!("attach {tap}: {e}")))?;
        parts.resolver.set_client(ipv4.into(), pod_id, upstreams);
        Ok(Some(Attachment {
            tap,
            mac: mac(pod_id),
            ipv6,
            ipv4,
            dns: self.config.dns_address,
        }))
    }

    /// Takes a deleted pod off the network: its resolver, its TAP device
    /// with its routes and its proxy ARP, and the connections its IPv4
    /// address has in connection tracking, so that the next pod with the
    /// address finds none of them. Before the pod leaves the directory, so
    /// that their ends are recorded as the pod's.
    pub async fn detach(&self, info: &PodInfo) -> io::Result<()> {
        if !has_network(info) {
            return Ok(());
        }
        let ipv4 = match (&self.parts, info.ipv4_address.parse::<Ipv4Addr>()) {
            (Ok(parts), Ok(address)) if parts.ipv4.contains(address) => {
                parts.resolver.remove_client(address.into());
                Some(address)
            }
            _ => None,
        };
        let tap = tap_name(&info.pod_id);
        microvm_service::vmm::jail::tap::delete(&tap)
            .map_err(|e| io::Error::new(e.kind(), format!("delete {tap}: {e}")))?;
        let Some(ipv4) = ipv4 else {
            return Ok(());
        };
        let deleted =
            tokio::task::spawn_blocking(move || traffic_service::delete_connections(ipv4.into()))
                .await
                .map_err(io::Error::other)
                .and_then(|deleted| deleted);
        match deleted {
            Ok(0) => {}
            Ok(n) => info!("Network: Deleted {n} connections of {ipv4}"),
            Err(e) => warn!("Network: Failed to delete the connections of {ipv4}: {e}"),
        }
        Ok(())
    }

    /// Frees a deleted pod's IPv4 address, once the pod is off the network
    /// and out of the rules: it comes round again after the others free.
    pub fn release(&self, info: &PodInfo) {
        if !has_network(info) {
            return;
        }
        if let (Ok(parts), Ok(address)) = (&self.parts, info.ipv4_address.parse::<Ipv4Addr>())
            && let Err(e) = parts.ipv4.free(address, SystemTime::now())
        {
            warn!("Network: Failed to free {address}: {e}");
        }
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
        let pods: Vec<&PodInfo> = pods.iter().filter(|pod| has_network(pod)).collect();
        self.directory.set(&pods);
        let members: Vec<network_service::Pod> = pods
            .iter()
            .filter_map(|pod| {
                Some(network_service::Pod {
                    pod_id: pod.pod_id.clone(),
                    tap: tap_name(&pod.pod_id),
                    address: pod.ipv6_address.parse().ok()?,
                    groups: pod_network(pod).security_groups,
                })
            })
            .collect();
        let policy = compile(&self.groups.snapshot(), &members, &self.denied(&node));
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

    /// What no pod reaches: the denied prefixes, the node's own addresses,
    /// and the pods' IPv4 range, which link-local addresses cover unless
    /// `--pod-ipv4-range` moves it: pods reach each other over IPv6 alone.
    fn denied(&self, node: &[IpAddr]) -> Vec<Prefix> {
        let mut denied = self.config.deny_prefixes.clone();
        denied.extend(node.iter().map(|address| match *address {
            IpAddr::V4(v4) => Prefix::V4(v4, 32),
            IpAddr::V6(v6) => Prefix::V6(v6, 128),
        }));
        let (range, len) = self.config.pod_ipv4_range;
        denied.push(Prefix::V4(range, len));
        denied
    }

    fn rules(&self) -> rules::Options {
        rules::Options {
            dns: IpAddr::V4(self.config.dns_address),
            ipv4_range: self.config.pod_ipv4_range,
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
        let config = traffic_service::Config::default();
        match Collector::start(self.directory.clone(), config, sender) {
            Ok(collector) => {
                *lock(&self.collector) = Some(collector);
                // The resolver records the pods' queries to it.
                let resolver = IpAddr::V4(self.config.dns_address);
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

/// Sets the node up for pods, see [`Network::start`]. Every step can be
/// taken again; the resolver's sockets are bound last.
async fn set_up(
    config: &Config,
    (prefix, len): (Ipv6Addr, u8),
    dir: &Path,
    records: &broadcast::Sender<TrafficRecord>,
) -> io::Result<Parts> {
    config.check().map_err(io::Error::other)?;
    remove_old_network(false).await?;
    let handle = devices::connect().await?;
    devices::enable_forwarding()?;
    devices::route_unreachable(&handle, prefix, len).await?;
    devices::dummy_with_address(&handle, DNS_DEVICE, config.dns_address.into()).await?;

    let resolver = Resolver::new(dns_service::Config {
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
    let (range, range_len) = config.pod_ipv4_range;
    Ok(Parts {
        ipv6: Ipv6Addresses::new(dir, prefix, len),
        ipv4: Ipv4Addresses::new(dir, range, range_len),
        resolver,
    })
}

/// [`set_up`], taken again a few times after a failure: netlink may answer
/// a moment later.
async fn set_up_again(
    config: &Config,
    prefix: (Ipv6Addr, u8),
    dir: &Path,
    records: &broadcast::Sender<TrafficRecord>,
) -> io::Result<Parts> {
    let mut attempt = 1;
    loop {
        match set_up(config, prefix, dir, records).await {
            Err(e) if attempt < SET_UP_ATTEMPTS => {
                warn!("Network: Setting the pods' network up failed, trying again: {e}");
                attempt += 1;
                tokio::time::sleep(SET_UP_PAUSE).await;
            }
            result => return result,
        }
    }
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
fn is_dns_flow(record: &TrafficRecord, resolver: IpAddr) -> bool {
    let Some(traffic_record::Record::Flow(flow)) = &record.record else {
        return false;
    };
    flow.tuple.as_ref().is_some_and(|tuple| {
        tuple.destination_port == u32::from(DNS_PORT)
            && tuple.destination_address.parse::<IpAddr>().ok() == Some(resolver)
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

/// Which pod an address, of either family, or a TAP device is, for the
/// records.
#[derive(Default)]
struct Directory {
    addresses: RwLock<HashMap<IpAddr, String>>,
    taps: RwLock<HashMap<String, String>>,
}

impl Directory {
    /// Knows these pods, those with a network, and no others.
    fn set(&self, pods: &[&PodInfo]) {
        let addresses = pods
            .iter()
            .flat_map(|pod| {
                [&pod.ipv6_address, &pod.ipv4_address]
                    .into_iter()
                    .filter_map(|address| address.parse::<IpAddr>().ok())
                    .map(|address| (address, pod.pod_id.clone()))
            })
            .collect();
        let taps = pods
            .iter()
            .map(|pod| (tap_name(&pod.pod_id), pod.pod_id.clone()))
            .collect();
        *self
            .addresses
            .write()
            .unwrap_or_else(PoisonError::into_inner) = addresses;
        *self.taps.write().unwrap_or_else(PoisonError::into_inner) = taps;
    }
}

impl traffic_service::Pods for Directory {
    fn by_address(&self, address: IpAddr) -> Option<String> {
        self.addresses
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(&address.to_canonical())
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
    use std::fs;
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

    /// A network whose addresses are kept in `dir`, as if it were set up.
    fn set_up_in(dir: &Path, config: Config) -> Network {
        let (prefix, len) = config.pod_prefix.expect("a pod prefix");
        let (range, range_len) = config.pod_ipv4_range;
        let mut network = network(config);
        network.parts = Ok(Parts {
            ipv6: Ipv6Addresses::new(dir, prefix, len),
            ipv4: Ipv4Addresses::new(dir, range, range_len),
            resolver: Resolver::new(dns_service::Config::default()),
        });
        network
    }

    fn lab() -> Config {
        Config {
            pod_prefix: Some(("fd00:fe05:1::".parse().unwrap(), 64)),
            ..Config::default()
        }
    }

    /// A pod with a network and these addresses.
    fn pod(id: &str, ipv6: &str, ipv4: &str) -> PodInfo {
        PodInfo {
            pod_id: id.to_string(),
            spec: Some(PodSpec::default()),
            ipv6_address: ipv6.to_string(),
            ipv4_address: ipv4.to_string(),
            ..Default::default()
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
            parse_pod_ipv4_range("169.254.16.0/20"),
            Ok((Ipv4Addr::new(169, 254, 16, 0), 20))
        );
        assert_eq!(
            parse_pod_ipv4_range("10.77.0.0/30"),
            Ok((Ipv4Addr::new(10, 77, 0, 0), 30))
        );
        assert!(parse_pod_ipv4_range("10.77.0.0/31").is_err(), "no room");
        assert!(parse_pod_ipv4_range("10.77.0.1/24").is_err(), "host bits");
        assert!(parse_pod_ipv4_range("fd00::/64").is_err());
        let config = Config::default();
        assert_eq!(config.dns_upstreams.len(), 4);
        assert_eq!(config.dns_address, Ipv4Addr::new(169, 254, 0, 53));
        assert_eq!(config.pod_ipv4_range, (Ipv4Addr::new(169, 254, 16, 0), 20));
        assert_eq!(config.check(), Ok(()));
    }

    #[test]
    fn the_node_holds_neither_the_gateway_nor_a_pods_address() {
        let config = |dns: &str, range: &str| Config {
            dns_address: dns.parse().unwrap(),
            pod_ipv4_range: parse_pod_ipv4_range(range).unwrap(),
            ..Config::default()
        };
        assert_eq!(config("10.77.0.53", "10.78.0.0/20").check(), Ok(()));
        for (dns, range) in [
            // This very gateway may be the node's own network's.
            ("169.254.0.1", "169.254.16.0/20"),
            ("0.0.0.0", "169.254.16.0/20"),
            ("127.0.0.53", "169.254.16.0/20"),
            ("224.0.0.53", "169.254.16.0/20"),
            ("255.255.255.255", "169.254.16.0/20"),
            ("169.254.0.53", "169.254.0.0/16"),
            ("169.254.0.53", "169.254.0.0/24"),
            ("10.77.0.53", "169.254.0.0/30"),
        ] {
            assert!(config(dns, range).check().is_err(), "{dns} and {range}");
        }
    }

    #[test]
    fn upstreams_a_pod_may_not_reach_are_refused() {
        let network = network(Config {
            pod_prefix: Some(("fd00:fe05:1::".parse().unwrap(), 64)),
            pod_ipv4_range: (Ipv4Addr::new(10, 77, 0, 0), 20),
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
        for refused in [
            "169.254.0.53",
            "169.254.0.1",
            "100.64.1.1",
            "2001:db8::53",
            "127.0.0.1",
            "::1",
            "fe80::1",
            "fd00:fe05:1::2",
            "10.77.0.9",
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
    fn denied_has_the_node_and_the_pods_ipv4() {
        let network = network(Config {
            deny_prefixes: vec!["169.254.169.254/32".parse().unwrap()],
            pod_ipv4_range: (Ipv4Addr::new(10, 77, 0, 0), 20),
            ..Config::default()
        });
        let node = ["192.0.2.7".parse().unwrap(), "2001:db8::7".parse().unwrap()];
        assert_eq!(
            network.denied(&node),
            [
                "169.254.169.254/32".parse().unwrap(),
                "192.0.2.7/32".parse().unwrap(),
                "2001:db8::7/128".parse().unwrap(),
                "10.77.0.0/20".parse().unwrap(),
            ]
        );
        assert_eq!(
            self::tests::network(Config::default()).denied(&[]),
            ["169.254.16.0/20".parse().unwrap()]
        );
        let rules = network.rules();
        assert_eq!(rules.dns, "169.254.0.53".parse::<IpAddr>().unwrap());
        assert_eq!(rules.ipv4_range, (Ipv4Addr::new(10, 77, 0, 0), 20));
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
    fn a_pod_gets_an_address_of_each_family() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(dir.path(), lab());
        let others = [
            pod(
                "11111111-0000-0000-0000-000000000001",
                "fd00:fe05:1::2",
                "169.254.16.1",
            ),
            // Without a network, its addresses are no pod's.
            PodInfo {
                spec: Some(PodSpec {
                    network: Some(PodNetwork {
                        none: true,
                        ..Default::default()
                    }),
                    ..Default::default()
                }),
                ..pod("22222222-0000-0000-0000-000000000000", "", "169.254.16.2")
            },
        ];
        let mut info = pod("33333333-0000-0000-0000-000000000000", "", "");
        network.allocate(others.iter(), &mut info).unwrap();
        assert_eq!(
            (info.ipv6_address.as_str(), info.ipv4_address.as_str()),
            ("fd00:fe05:1::3", "169.254.16.2")
        );
        // A pod whose ID starts as another's would share its TAP device.
        let mut twin = pod("11111111-0000-0000-0000-000000000009", "", "");
        let error = network.allocate(others.iter(), &mut twin).unwrap_err();
        assert_eq!(error.code(), tonic::Code::AlreadyExists);
    }

    #[test]
    fn a_deleted_pods_ipv4_address_comes_round_last() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(
            dir.path(),
            Config {
                pod_ipv4_range: (Ipv4Addr::new(10, 77, 0, 0), 30),
                ..lab()
            },
        );
        let ids = [
            "11111111-0000-0000-0000-000000000000",
            "22222222-0000-0000-0000-000000000000",
            "33333333-0000-0000-0000-000000000000",
        ];
        let mut pods: Vec<PodInfo> = Vec::new();
        for id in &ids[..2] {
            let mut info = pod(id, "", "");
            network.allocate(pods.iter(), &mut info).unwrap();
            pods.push(info);
        }
        assert_eq!(pods[1].ipv4_address, "10.77.0.2");
        let mut third = pod(ids[2], "", "");
        let error = network.allocate(pods.iter(), &mut third).unwrap_err();
        assert_eq!(error.code(), tonic::Code::ResourceExhausted);
        let deleted = pods.remove(0);
        network.release(&deleted);
        network.allocate(pods.iter(), &mut third).unwrap();
        assert_eq!(third.ipv4_address, "10.77.0.1");
        // Its IPv6 address never comes round.
        assert_eq!(third.ipv6_address, "fd00:fe05:1::5");
    }

    #[test]
    fn pods_kept_from_before_get_what_they_lack() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(dir.path(), lab());
        let mut kept = vec![
            pod("a", "fd00:fe05:1::2", "169.254.16.1"),
            // An address of the first version's pool.
            pod("b", "fd00:fe05:1::3", "10.200.0.5"),
            // IPv6 only.
            pod("c", "fd00:fe05:1::4", ""),
            // The first version's: neither.
            pod("d", "", ""),
            // Another's IPv4 address.
            pod("e", "fd00:fe05:1::5", "169.254.16.1"),
            PodInfo {
                spec: Some(PodSpec {
                    network: Some(PodNetwork {
                        none: true,
                        ..Default::default()
                    }),
                    ..Default::default()
                }),
                ..pod("f", "", "10.200.0.6")
            },
        ];
        assert_eq!(network.adopt(&mut kept), ["b", "c", "d", "e"]);
        let addresses: Vec<(&str, &str)> = kept
            .iter()
            .map(|pod| (pod.ipv6_address.as_str(), pod.ipv4_address.as_str()))
            .collect();
        assert_eq!(
            addresses,
            [
                ("fd00:fe05:1::2", "169.254.16.1"),
                ("fd00:fe05:1::3", "169.254.16.2"),
                ("fd00:fe05:1::4", "169.254.16.3"),
                ("fd00:fe05:1::6", "169.254.16.4"),
                ("fd00:fe05:1::5", "169.254.16.5"),
                ("", "10.200.0.6"),
            ]
        );
        // A second start changes nothing.
        assert!(network.adopt(&mut kept).is_empty());
        assert_eq!(
            fs::read_to_string(dir.path().join("ipv4-addresses")).unwrap(),
            "next 169.254.16.6\n"
        );
    }

    #[tokio::test]
    async fn a_pod_without_its_addresses_is_not_attached() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(dir.path(), lab());
        for (ipv6, ipv4) in [
            ("fd00:fe05:1::2", ""),
            ("fd00:fe05:1::2", "10.200.0.5"),
            ("", "169.254.16.1"),
        ] {
            let info = pod("11111111-0000-0000-0000-000000000000", ipv6, ipv4);
            let error = network.attach(&info).await.unwrap_err();
            assert_eq!(
                error.code(),
                tonic::Code::FailedPrecondition,
                "{ipv6} {ipv4}"
            );
        }
        let mut none = pod("11111111-0000-0000-0000-000000000000", "", "");
        none.spec = Some(PodSpec {
            network: Some(PodNetwork {
                none: true,
                ..Default::default()
            }),
            ..Default::default()
        });
        assert_eq!(network.attach(&none).await.unwrap(), None);
    }

    #[test]
    fn the_directory_knows_pods_by_both_addresses_and_tap() {
        let directory = Directory::default();
        let pods = [
            pod(
                "aaaaaaaa-0000-0000-0000-000000000000",
                "fd00::2",
                "169.254.16.1",
            ),
            pod("bbbbbbbb-0000-0000-0000-000000000000", "fd00::3", ""),
        ];
        directory.set(&pods.iter().collect::<Vec<_>>());
        let id = |address: &str| directory.by_address(address.parse().unwrap());
        let a = Some(pods[0].pod_id.clone());
        assert_eq!(id("fd00::2"), a);
        assert_eq!(id("169.254.16.1"), a);
        assert_eq!(id("::ffff:169.254.16.1"), a);
        assert_eq!(id("fd00::3"), Some(pods[1].pod_id.clone()));
        assert_eq!(id("169.254.16.2"), None);
        assert_eq!(directory.by_interface("pod-aaaaaaaa"), a);
        assert_eq!(directory.by_interface("eth0"), None);
        directory.set(&[]);
        assert_eq!(id("169.254.16.1"), None);
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
        let resolver = IpAddr::V4(dns_service::ADDRESS);
        assert!(is_dns_flow(&flow("169.254.0.53", 53, 0), resolver));
        assert!(!is_dns_flow(&flow("169.254.0.53", 853, 0), resolver));
        assert!(!is_dns_flow(&flow("8.8.8.8", 53, 0), resolver));

        let (from, receiver) = broadcast::channel(8);
        let (to, mut records) = broadcast::channel(8);
        let forwarding = tokio::spawn(forward_records(receiver, to, move |record| {
            !is_dns_flow(record, resolver)
        }));
        from.send(flow("169.254.0.53", 53, 2)).unwrap();
        from.send(flow("8.8.8.8", 53, 1)).unwrap();
        drop(from);
        forwarding.await.unwrap();
        let record = records.recv().await.unwrap();
        // What was left out counts as missed.
        assert_eq!(record.missed, 3);
        assert!(records.try_recv().is_err());
    }
}
