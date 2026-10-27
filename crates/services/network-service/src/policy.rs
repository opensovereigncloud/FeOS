// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The compiler: the node's security groups and pods become a [`Policy`],
//! named sets of addresses and what each pod may send and receive, which
//! feosd writes as nftables (ADR 0012). Pure: no I/O, and no panics on
//! anything it is given.
//!
//! Every set is of one family. A pod's IPv4 address is for leaving only
//! (ADR 0010): an IPv4 block counts in rules out, and rules in ignore it;
//! a group as a peer stands for its members' IPv6 addresses. ICMP is of
//! IPv4 and ICMPv6 of IPv6: a rule for either allows nothing of the other
//! family, and a rule for any protocol allows both.

use crate::group::{self, Direction, Peer, Protocol};
use crate::prefix::{Family, Prefix};
use crate::ranges::{self, Range};
use crate::registry;
use feos_proto::network_service::SecurityGroup;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::ops::RangeInclusive;

/// The sets of the default group's internet: global IPv4 addresses, and
/// global IPv6 addresses.
pub const INTERNET4: &str = "internet4";
pub const INTERNET6: &str = "internet6";
/// The sets of what no pod reaches and nothing reaches a pod from, one
/// of each family: the denied prefixes, and link-local, loopback,
/// multicast and unspecified addresses.
pub const DENY4: &str = "deny4";
pub const DENY6: &str = "deny6";

/// Set names are at most this long, which every kernel takes.
const MAX_SET_NAME: usize = 64;

/// A range of addresses of one family, first and last.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum AddressRange {
    V4(RangeInclusive<Ipv4Addr>),
    V6(RangeInclusive<Ipv6Addr>),
}

impl AddressRange {
    pub fn family(&self) -> Family {
        match self {
            AddressRange::V4(_) => Family::V4,
            AddressRange::V6(_) => Family::V6,
        }
    }

    /// Whether the range holds the address; one of the other family it
    /// never does.
    pub fn contains(&self, address: &IpAddr) -> bool {
        match (self, address) {
            (AddressRange::V4(range), IpAddr::V4(address)) => range.contains(address),
            (AddressRange::V6(range), IpAddr::V6(address)) => range.contains(address),
            _ => false,
        }
    }

    /// A range of numbers of the family, as addresses.
    fn new(family: Family, (start, end): Range) -> Self {
        let ipv4 = |n: u128| Ipv4Addr::from(u32::try_from(n).unwrap_or(u32::MAX));
        match family {
            Family::V4 => AddressRange::V4(ipv4(start)..=ipv4(end)),
            Family::V6 => AddressRange::V6(Ipv6Addr::from(start)..=Ipv6Addr::from(end)),
        }
    }

    /// The range as numbers of its family.
    fn numbers(&self) -> Range {
        match self {
            AddressRange::V4(range) => (
                u32::from(*range.start()).into(),
                u32::from(*range.end()).into(),
            ),
            AddressRange::V6(range) => ((*range.start()).into(), (*range.end()).into()),
        }
    }
}

/// A set of addresses of one family, normal: sorted, and its ranges
/// neither overlap nor touch.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AddressSet {
    pub family: Family,
    /// Ranges of the set's family.
    pub ranges: Vec<AddressRange>,
}

impl AddressSet {
    fn new(family: Family, ranges: &[Range]) -> Self {
        Self {
            family,
            ranges: ranges
                .iter()
                .map(|&range| AddressRange::new(family, range))
                .collect(),
        }
    }

    pub fn contains(&self, address: &IpAddr) -> bool {
        self.ranges.iter().any(|range| range.contains(address))
    }
}

/// Ranges of ports, first and last.
type Ports = Vec<(u16, u16)>;

/// A pod on the node, as the compiler needs it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Pod {
    pub pod_id: String,
    /// Its TAP device.
    pub tap: String,
    pub address: Ipv6Addr,
    /// The security groups its spec names; none means the default group.
    pub groups: Vec<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Policy {
    /// Named sets of addresses. Sets are [`DENY4`] and [`DENY6`],
    /// [`INTERNET4`] and [`INTERNET6`], `g-<group ID>` for a group's
    /// members, of IPv6, and `b-<12 hex digits>` for an address block
    /// without its exceptions, of the block's family, which identical
    /// blocks share.
    pub sets: BTreeMap<String, AddressSet>,
    /// The pods, by ID.
    pub pods: Vec<PodPolicy>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PodPolicy {
    pub pod_id: String,
    pub tap: String,
    pub address: Ipv6Addr,
    /// What may reach the pod: its peers are sources, of IPv6.
    pub ingress: Vec<Allow>,
    /// What the pod may send: its peers are destinations.
    pub egress: Vec<Allow>,
    /// Groups the pod names that the node does not have: the pod must not
    /// start (ADR 0012). While any is missing, the pod is allowed nothing.
    pub missing: Vec<String>,
}

/// What a rule allows: a protocol, its ports, and the set of peers. Rules
/// of a pod with the same protocol and peers are one, with their ports.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Allow {
    pub protocol: Protocol,
    /// Destination ports of TCP and UDP, normal; empty means every port.
    pub ports: Vec<RangeInclusive<u16>>,
    /// The set that holds the peers.
    pub peer: String,
    /// The set's family, which is the packets'.
    pub family: Family,
}

/// How the sets of one policy become those of the next.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Delta {
    /// Only elements of sets change, set by set; nothing at all if empty.
    Elements(BTreeMap<String, SetChange>),
    /// Rules, pods or the sets themselves changed: the table is replaced
    /// whole.
    Replace,
}

/// The elements of a set to remove and to add, in one batch, removals
/// first. They are the elements the sets hold, each whole, as the kernel's
/// interval sets need them: a range that grows is removed and added again.
/// They are of the set's family.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SetChange {
    pub remove: Vec<AddressRange>,
    pub add: Vec<AddressRange>,
}

/// Sets as the compiler builds them: their family, and its numbers.
type Sets = BTreeMap<String, (Family, Vec<Range>)>;

/// Compiles the node's groups and pods. `denied` are prefixes of either
/// family that no pod reaches: `--deny-prefix`, and the node's own
/// addresses.
pub fn compile(
    groups: &BTreeMap<String, SecurityGroup>,
    pods: &[Pod],
    denied: &[Prefix],
) -> Policy {
    let denied: Vec<Prefix> = denied
        .iter()
        .copied()
        .chain(registry::always_denied())
        .collect();
    let mut sets = Sets::new();
    for (name, family) in [(DENY4, Family::V4), (DENY6, Family::V6)] {
        let deny = denied
            .iter()
            .filter(|prefix| prefix.family() == family)
            .map(Prefix::range)
            .collect();
        sets.insert(name.to_string(), (family, ranges::normalize(deny)));
    }
    sets.insert(INTERNET4.to_string(), (Family::V4, registry::internet4()));
    sets.insert(INTERNET6.to_string(), (Family::V6, registry::internet6()));

    let mut policies: Vec<PodPolicy> = pods
        .iter()
        .map(|pod| compile_pod(pod, groups, &mut sets))
        .collect();
    policies.sort_by(|a, b| a.pod_id.cmp(&b.pod_id));
    Policy {
        sets: sets
            .into_iter()
            .map(|(name, (family, set))| (name, AddressSet::new(family, &set)))
            .collect(),
        pods: policies,
    }
}

fn compile_pod(pod: &Pod, groups: &BTreeMap<String, SecurityGroup>, sets: &mut Sets) -> PodPolicy {
    let mut policy = PodPolicy {
        pod_id: pod.pod_id.clone(),
        tap: pod.tap.clone(),
        address: pod.address,
        ingress: Vec::new(),
        egress: Vec::new(),
        missing: Vec::new(),
    };
    if pod.groups.is_empty() {
        // The default group: out to the internet, nothing in.
        policy.egress = [(INTERNET4, Family::V4), (INTERNET6, Family::V6)]
            .into_iter()
            .map(|(peer, family)| Allow {
                protocol: Protocol::Any,
                ports: Vec::new(),
                peer: peer.to_string(),
                family,
            })
            .collect();
        return policy;
    }
    let names: BTreeSet<&String> = pod.groups.iter().collect();
    let mut named = Vec::new();
    for name in names {
        match groups.get(name) {
            Some(group) => named.push(group),
            None => policy.missing.push(name.clone()),
        }
    }
    if !policy.missing.is_empty() {
        return policy;
    }

    let mut ingress = Vec::new();
    let mut egress = Vec::new();
    for rule in named.iter().flat_map(|group| &group.rules) {
        // The store takes valid rules only; anything else allows nothing.
        let Ok(rule) = group::parse_rule(rule) else {
            continue;
        };
        let family = match &rule.peer {
            Peer::Group(_) => Family::V6,
            Peer::Block { cidr, .. } => cidr.family(),
        };
        if !applies(rule.direction, rule.protocol, family) {
            continue;
        }
        let name = match &rule.peer {
            Peer::Group(id) => {
                let name = group_set(id);
                sets.entry(name.clone())
                    .or_insert_with(|| (Family::V6, members(groups.get(id))));
                name
            }
            Peer::Block { cidr, except } => {
                let set = block(cidr, except);
                let name = block_set(family, &set);
                sets.entry(name.clone()).or_insert((family, set));
                name
            }
        };
        let allow = (rule.protocol, name, family, rule.ports);
        match rule.direction {
            Direction::Ingress => ingress.push(allow),
            Direction::Egress => egress.push(allow),
        }
    }
    policy.ingress = merge(ingress);
    policy.egress = merge(egress);
    policy
}

/// Whether a rule allows anything of peers of `family`: IPv4 only out,
/// since nothing new comes in over IPv4 (ADR 0010); ICMP only of IPv4,
/// and ICMPv6 only of IPv6.
fn applies(direction: Direction, protocol: Protocol, family: Family) -> bool {
    match (protocol, family) {
        (Protocol::Icmp, Family::V6) | (Protocol::Icmpv6, Family::V4) => false,
        (_, Family::V4) => direction == Direction::Egress,
        (_, Family::V6) => true,
    }
}

/// The members of a group, if the node has it: a group it does not have
/// yet allows nothing.
fn members(group: Option<&SecurityGroup>) -> Vec<Range> {
    let members = group
        .map(|group| group.members.as_slice())
        .unwrap_or_default();
    let addresses = members
        .iter()
        .filter_map(|member| member.parse::<Ipv6Addr>().ok())
        .map(|address| (u128::from(address), u128::from(address)))
        .collect();
    ranges::normalize(addresses)
}

/// The addresses of a block without its exceptions, which are of its
/// family.
fn block(cidr: &Prefix, except: &[Prefix]) -> Vec<Range> {
    let holes = except
        .iter()
        .filter(|prefix| prefix.family() == cidr.family())
        .map(Prefix::range)
        .collect();
    ranges::subtract(&[cidr.range()], &ranges::normalize(holes))
}

/// The set of a group's members: `g-<ID>`, and for an ID too long for
/// that, its start and a hash of it, one character longer than any short
/// name can be.
fn group_set(id: &str) -> String {
    if id.len() + 2 < MAX_SET_NAME {
        return format!("g-{id}");
    }
    let start: String = id.chars().take(MAX_SET_NAME - 15).collect();
    format!("g-{start}-{}", hash(id.as_bytes()))
}

/// The set of a block: named after its family and addresses.
fn block_set(family: Family, set: &[Range]) -> String {
    let family = match family {
        Family::V4 => 4,
        Family::V6 => 6,
    };
    let bytes: Vec<u8> = set
        .iter()
        .flat_map(|&(start, end)| [start.to_be_bytes(), end.to_be_bytes()])
        .flatten()
        .collect();
    format!("b-{}", hash(&[&[family][..], &bytes].concat()))
}

/// 12 hex digits of the SHA-256 of `bytes`.
fn hash(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .take(6)
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

/// Rules with the same protocol and peers become one, with the ports of
/// all; one with every port takes every port. Sorted by protocol and peer.
fn merge(rules: Vec<(Protocol, String, Family, Ports)>) -> Vec<Allow> {
    // None: every port. A set's name tells its family.
    let mut merged: BTreeMap<(Protocol, String), (Family, Option<Ports>)> = BTreeMap::new();
    for (protocol, peer, family, ports) in rules {
        let (_, entry) = merged
            .entry((protocol, peer))
            .or_insert_with(|| (family, Some(Vec::new())));
        match entry {
            Some(all) if !ports.is_empty() => all.extend(ports),
            _ => *entry = None,
        }
    }
    merged
        .into_iter()
        .map(|((protocol, peer), (family, ports))| Allow {
            protocol,
            peer,
            family,
            ports: ports
                .map(merge_ports)
                .unwrap_or_default()
                .into_iter()
                .map(|(first, last)| first..=last)
                .collect(),
        })
        .collect()
}

fn merge_ports(mut ports: Vec<(u16, u16)>) -> Vec<(u16, u16)> {
    ports.sort_unstable();
    let mut merged: Vec<(u16, u16)> = Vec::with_capacity(ports.len());
    for (first, last) in ports {
        match merged.last_mut() {
            Some(previous) if u32::from(first) <= u32::from(previous.1) + 1 => {
                previous.1 = previous.1.max(last)
            }
            _ => merged.push((first, last)),
        }
    }
    merged
}

/// What changes from `old` to `new`: elements of sets, or everything.
pub fn delta(old: &Policy, new: &Policy) -> Delta {
    if old.pods != new.pods || !old.sets.keys().eq(new.sets.keys()) {
        return Delta::Replace;
    }
    let mut changes = BTreeMap::new();
    for (name, after) in &new.sets {
        let Some(before) = old.sets.get(name) else {
            return Delta::Replace;
        };
        if before.family != after.family {
            return Delta::Replace;
        }
        if before == after {
            continue;
        }
        let numbers = |set: &AddressSet| -> BTreeSet<Range> {
            set.ranges.iter().map(AddressRange::numbers).collect()
        };
        let family = after.family;
        let (before, after) = (numbers(before), numbers(after));
        let range = |&range: &Range| AddressRange::new(family, range);
        changes.insert(
            name.clone(),
            SetChange {
                remove: before.difference(&after).map(range).collect(),
                add: after.difference(&before).map(range).collect(),
            },
        );
    }
    Delta::Elements(changes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::{self as proto, rule, AddressBlock, PortRange};

    fn a(s: &str) -> Ipv6Addr {
        s.parse().unwrap()
    }

    fn block(cidr: &str, except: &[&str]) -> rule::Peer {
        rule::Peer::Block(AddressBlock {
            cidr: cidr.to_string(),
            except: except.iter().map(|e| e.to_string()).collect(),
        })
    }

    fn peer_group(id: &str) -> rule::Peer {
        rule::Peer::Group(id.to_string())
    }

    fn ingress(protocol: proto::Protocol, ports: &[u32], peer: rule::Peer) -> proto::Rule {
        proto::Rule {
            direction: proto::Direction::Ingress as i32,
            protocol: protocol as i32,
            ports: ports
                .iter()
                .map(|&first| PortRange { first, last: 0 })
                .collect(),
            peer: Some(peer),
        }
    }

    fn egress(protocol: proto::Protocol, ports: &[u32], peer: rule::Peer) -> proto::Rule {
        proto::Rule {
            direction: proto::Direction::Egress as i32,
            ..ingress(protocol, ports, peer)
        }
    }

    fn group(id: &str, rules: Vec<proto::Rule>, members: &[&str]) -> SecurityGroup {
        SecurityGroup {
            id: id.to_string(),
            version: 1,
            rules,
            members: members.iter().map(|m| m.to_string()).collect(),
        }
    }

    fn store(groups: Vec<SecurityGroup>) -> BTreeMap<String, SecurityGroup> {
        groups.into_iter().map(|g| (g.id.clone(), g)).collect()
    }

    fn pod(n: u8, groups: &[&str]) -> Pod {
        Pod {
            pod_id: format!("{n}{n}{n}{n}{n}{n}{n}{n}-0000-0000-0000-000000000000"),
            tap: format!("pod-{n}{n}{n}{n}{n}{n}{n}{n}"),
            address: Ipv6Addr::from(u128::from(a("2a00:1::")) + u128::from(n)),
            groups: groups.iter().map(|g| g.to_string()).collect(),
        }
    }

    fn prefixes(prefixes: &[&str]) -> Vec<Prefix> {
        prefixes.iter().map(|p| p.parse().unwrap()).collect()
    }

    fn compile_with(groups: &BTreeMap<String, SecurityGroup>, pods: &[Pod]) -> Policy {
        compile(groups, pods, &[])
    }

    fn holds(set: &AddressSet, address: &str) -> bool {
        set.contains(&address.parse().unwrap())
    }

    fn range6(first: &str, last: &str) -> AddressRange {
        AddressRange::V6(a(first)..=a(last))
    }

    fn range4(first: &str, last: &str) -> AddressRange {
        AddressRange::V4(first.parse().unwrap()..=last.parse().unwrap())
    }

    fn allow(protocol: Protocol, ports: &[(u16, u16)], peer: &str, family: Family) -> Allow {
        Allow {
            protocol,
            ports: ports.iter().map(|&(first, last)| first..=last).collect(),
            peer: peer.to_string(),
            family,
        }
    }

    fn v6(protocol: Protocol, ports: &[(u16, u16)], peer: &str) -> Allow {
        allow(protocol, ports, peer, Family::V6)
    }

    fn v4(protocol: Protocol, ports: &[(u16, u16)], peer: &str) -> Allow {
        allow(protocol, ports, peer, Family::V4)
    }

    fn elements(name: &str, change: SetChange) -> Delta {
        Delta::Elements(BTreeMap::from([(name.to_string(), change)]))
    }

    /// The protocols and families of allows, sorted.
    fn kinds(allows: &[Allow]) -> Vec<(Protocol, Family)> {
        let mut kinds: Vec<_> = allows.iter().map(|a| (a.protocol, a.family)).collect();
        kinds.sort();
        kinds
    }

    #[test]
    fn a_pod_without_groups_gets_the_default_group() {
        let policy = compile_with(&BTreeMap::new(), &[pod(1, &[])]);
        let pod = &policy.pods[0];
        assert_eq!(pod.ingress, vec![], "nothing in");
        assert_eq!(
            pod.egress,
            vec![
                v4(Protocol::Any, &[], INTERNET4),
                v6(Protocol::Any, &[], INTERNET6)
            ]
        );
        assert!(pod.missing.is_empty());
        assert_eq!(pod.tap, "pod-11111111");
        assert_eq!(pod.address, a("2a00:1::1"));
        assert_eq!(
            policy.sets.keys().collect::<Vec<_>>(),
            [DENY4, DENY6, INTERNET4, INTERNET6]
        );
        let (internet4, internet6) = (&policy.sets[INTERNET4], &policy.sets[INTERNET6]);
        assert_eq!(
            (internet4.family, internet6.family),
            (Family::V4, Family::V6)
        );
        for (set, family) in [(internet4, Family::V4), (internet6, Family::V6)] {
            assert!(set.ranges.iter().all(|range| range.family() == family));
        }
        for address in ["8.8.8.8", "1.1.1.1", "223.255.255.255"] {
            assert!(holds(internet4, address), "{address}");
        }
        assert!(holds(internet6, "2001:4860:4860::8888"));
        for address in [
            "10.1.2.3",
            "192.168.1.1",
            "100.64.0.1",
            "169.254.16.1",
            "169.254.0.53",
            "224.0.0.1",
        ] {
            assert!(!holds(internet4, address), "{address}");
        }
        for address in ["64:ff9b::808:808", "fd00::1", "fe80::1", "2001:db8::1"] {
            assert!(!holds(internet6, address), "{address}");
        }
        assert!(!holds(internet6, "8.8.8.8"), "a set holds its family only");
    }

    #[test]
    fn denied_prefixes_and_what_is_denied_always() {
        let denied = prefixes(&[
            "169.254.169.254/32",
            "fd00:ffff::/32",
            "192.0.2.7/32",
            "2001:db8::7/128",
        ]);
        let policy = compile(&BTreeMap::new(), &[], &denied);
        let (deny4, deny6) = (&policy.sets[DENY4], &policy.sets[DENY6]);
        assert_eq!((deny4.family, deny6.family), (Family::V4, Family::V6));
        for address in [
            "169.254.169.254",
            "169.254.16.7",
            "192.0.2.7",
            "127.0.0.1",
            "224.0.0.251",
            "0.0.0.0",
        ] {
            assert!(holds(deny4, address), "{address}");
        }
        for address in [
            "fd00:ffff::1",
            "2001:db8::7",
            "fe80::1",
            "::1",
            "ff02::1",
            "::",
        ] {
            assert!(holds(deny6, address), "{address}");
        }
        for address in ["8.8.8.8", "192.0.2.8", "10.0.0.1"] {
            assert!(!holds(deny4, address), "{address}");
        }
        // Pods reach each other over IPv6, as their groups say.
        for address in ["fd00:fffe::1", "2001:db8::8", "fd00:fe05:1::2"] {
            assert!(!holds(deny6, address), "{address}");
        }
        // The link-local /16 holds the denied /32: they are one range.
        assert!(deny4
            .ranges
            .contains(&range4("169.254.0.0", "169.254.255.255")));
    }

    #[test]
    fn a_pod_whose_groups_are_missing_is_allowed_nothing() {
        let rules = vec![ingress(proto::Protocol::Tcp, &[80], block("::/0", &[]))];
        let groups = store(vec![group("web", rules, &[])]);
        let policy = compile_with(&groups, &[pod(1, &["web", "db", "cache", "db"])]);
        let pod = &policy.pods[0];
        assert_eq!(pod.missing, ["cache", "db"]);
        assert!(pod.ingress.is_empty() && pod.egress.is_empty());
        assert_eq!(policy.sets.len(), 4, "no sets for its rules");
    }

    #[test]
    fn peer_groups_are_sets_of_their_members() {
        let groups = store(vec![
            group(
                "web",
                vec![
                    ingress(proto::Protocol::Tcp, &[443, 80], peer_group("lb")),
                    egress(proto::Protocol::Any, &[], peer_group("db")),
                    ingress(proto::Protocol::Icmpv6, &[], peer_group("web")),
                    // Members are IPv6 addresses: no ICMP of IPv4.
                    ingress(proto::Protocol::Icmp, &[], peer_group("web")),
                    egress(proto::Protocol::Icmp, &[], peer_group("lb")),
                ],
                &["2a00:1::1"],
            ),
            group("lb", vec![], &["2a00:2::7", "2a00:2::5", "2a00:2::6"]),
        ]);
        let policy = compile_with(&groups, &[pod(1, &["web"])]);
        let pod = &policy.pods[0];
        assert_eq!(
            pod.ingress,
            vec![
                v6(Protocol::Tcp, &[(80, 80), (443, 443)], "g-lb"),
                v6(Protocol::Icmpv6, &[], "g-web"),
            ]
        );
        assert_eq!(pod.egress, vec![v6(Protocol::Any, &[], "g-db")]);
        let set = |ranges| AddressSet {
            family: Family::V6,
            ranges,
        };
        assert_eq!(
            policy.sets["g-lb"],
            set(vec![range6("2a00:2::5", "2a00:2::7")])
        );
        assert_eq!(
            policy.sets["g-web"],
            set(vec![range6("2a00:1::1", "2a00:1::1")])
        );
        assert_eq!(
            policy.sets["g-db"],
            set(vec![]),
            "a group not on the node allows nothing"
        );
    }

    #[test]
    fn ipv4_blocks_count_out_and_not_in() {
        let groups = store(vec![group(
            "web",
            vec![
                egress(
                    proto::Protocol::Tcp,
                    &[443],
                    block("0.0.0.0/0", &["10.0.0.0/8"]),
                ),
                ingress(proto::Protocol::Tcp, &[80], block("0.0.0.0/0", &[])),
                ingress(proto::Protocol::Any, &[], block("192.0.2.0/24", &[])),
                ingress(proto::Protocol::Icmp, &[], block("192.0.2.0/24", &[])),
                ingress(proto::Protocol::Tcp, &[80], block("::/0", &[])),
            ],
            &[],
        )]);
        let policy = compile_with(&groups, &[pod(1, &["web"])]);
        let pod = &policy.pods[0];
        assert_eq!(kinds(&pod.ingress), [(Protocol::Tcp, Family::V6)]);
        assert_eq!(kinds(&pod.egress), [(Protocol::Tcp, Family::V4)]);
        assert_eq!(pod.egress[0].ports, vec![443..=443]);
        assert_eq!(
            policy.sets[&pod.egress[0].peer],
            AddressSet {
                family: Family::V4,
                ranges: vec![
                    range4("0.0.0.0", "9.255.255.255"),
                    range4("11.0.0.0", "255.255.255.255"),
                ],
            }
        );
        assert_eq!(policy.sets.len(), 4 + 2, "no sets for what is ignored");
    }

    #[test]
    fn icmp_is_of_ipv4_and_icmpv6_of_ipv6() {
        let groups = store(vec![group(
            "ping",
            vec![
                egress(proto::Protocol::Icmp, &[], block("0.0.0.0/0", &[])),
                egress(proto::Protocol::Icmp, &[], block("::/0", &[])),
                egress(proto::Protocol::Icmpv6, &[], block("::/0", &[])),
                egress(proto::Protocol::Icmpv6, &[], block("0.0.0.0/0", &[])),
                egress(proto::Protocol::Any, &[], block("192.0.2.0/24", &[])),
                egress(proto::Protocol::Any, &[], block("2001:db8::/32", &[])),
                ingress(proto::Protocol::Icmpv6, &[], peer_group("ping")),
                ingress(proto::Protocol::Icmp, &[], peer_group("ping")),
            ],
            &["2a00:1::1"],
        )]);
        let policy = compile_with(&groups, &[pod(1, &["ping"])]);
        let pod = &policy.pods[0];
        assert_eq!(
            kinds(&pod.egress),
            [
                (Protocol::Any, Family::V4),
                (Protocol::Any, Family::V6),
                (Protocol::Icmpv6, Family::V6),
                (Protocol::Icmp, Family::V4),
            ]
        );
        assert_eq!(kinds(&pod.ingress), [(Protocol::Icmpv6, Family::V6)]);
        for allow in pod.egress.iter().chain(&pod.ingress) {
            assert_eq!(policy.sets[&allow.peer].family, allow.family);
        }
    }

    #[test]
    fn identical_blocks_share_a_set() {
        let everyone_but_private = || block("0.0.0.0/0", &["10.0.0.0/8"]);
        let groups = store(vec![
            group(
                "a",
                vec![egress(proto::Protocol::Tcp, &[443], everyone_but_private())],
                &[],
            ),
            group(
                "b",
                vec![egress(proto::Protocol::Udp, &[53], everyone_but_private())],
                &[],
            ),
            group(
                "c",
                vec![egress(proto::Protocol::Any, &[], block("::/0", &[]))],
                &[],
            ),
        ]);
        let policy = compile_with(&groups, &[pod(1, &["a", "b"]), pod(2, &["a", "c"])]);
        let names: Vec<&String> = policy
            .sets
            .keys()
            .filter(|name| name.starts_with("b-"))
            .collect();
        assert_eq!(names.len(), 2);
        assert!(names.iter().all(|name| name.len() == 14));

        let shared = &policy.pods[0].egress[0].peer;
        assert_eq!(&policy.pods[0].egress[1].peer, shared);
        let tcp = policy.pods[1]
            .egress
            .iter()
            .find(|a| a.protocol == Protocol::Tcp);
        assert_eq!(&tcp.unwrap().peer, shared);
        assert_eq!(policy.sets[shared].family, Family::V4);
        let any = policy.pods[1]
            .egress
            .iter()
            .find(|a| a.protocol == Protocol::Any);
        assert_eq!(
            policy.sets[&any.unwrap().peer].ranges,
            vec![range6("::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")]
        );
        // The same numbers of the other family are another set.
        let numbers = [(0, u128::from(u32::MAX))];
        assert_ne!(
            block_set(Family::V4, &numbers),
            block_set(Family::V6, &numbers)
        );
    }

    #[test]
    fn rules_with_the_same_peers_merge_their_ports() {
        let everyone = || block("::/0", &[]);
        let groups = store(vec![
            group(
                "a",
                vec![
                    ingress(proto::Protocol::Tcp, &[443, 80], everyone()),
                    ingress(proto::Protocol::Tcp, &[81], everyone()),
                    ingress(proto::Protocol::Udp, &[53], everyone()),
                ],
                &[],
            ),
            group(
                "b",
                vec![
                    ingress(proto::Protocol::Tcp, &[80], everyone()),
                    ingress(proto::Protocol::Udp, &[], everyone()),
                ],
                &[],
            ),
        ]);
        let policy = compile_with(&groups, &[pod(1, &["a"]), pod(2, &["a", "b"])]);
        let set = &policy.pods[0].ingress[0].peer;
        let tcp = v6(Protocol::Tcp, &[(80, 81), (443, 443)], set);
        assert_eq!(
            policy.pods[0].ingress,
            vec![tcp.clone(), v6(Protocol::Udp, &[(53, 53)], set)]
        );
        assert_eq!(
            policy.pods[1].ingress,
            vec![tcp, v6(Protocol::Udp, &[], set)],
            "a rule for every port takes them all"
        );
        assert_eq!(
            merge_ports(vec![(1, 65535), (65535, 65535), (5, 6)]),
            vec![(1, 65535)]
        );
        assert_eq!(
            merge_ports(vec![(10, 20), (21, 30), (32, 40)]),
            vec![(10, 30), (32, 40)]
        );
    }

    #[test]
    fn the_policy_does_not_depend_on_the_order_of_pods() {
        let rules = vec![egress(proto::Protocol::Any, &[], peer_group("a"))];
        let groups = store(vec![group("a", rules, &["2a00:1::2"])]);
        let pods = [pod(2, &["a"]), pod(1, &[]), pod(3, &["a"])];
        let policy = compile_with(&groups, &pods);
        let ids: Vec<&str> = policy.pods.iter().map(|pod| &pod.pod_id[..1]).collect();
        assert_eq!(ids, ["1", "2", "3"]);
        let reversed: Vec<Pod> = pods.iter().rev().cloned().collect();
        assert_eq!(compile_with(&groups, &reversed), policy);
    }

    #[test]
    fn set_names_stay_short_and_apart() {
        let long = "x".repeat(64);
        let longer = format!("{}y", "x".repeat(63));
        let short = "x".repeat(61);
        assert_eq!(group_set(&short), format!("g-{short}"));
        assert_eq!(group_set(&long).len(), 64);
        assert_eq!(group_set(&longer).len(), 64);
        assert_ne!(group_set(&long), group_set(&longer));
        // Longer than any name of the short form.
        assert_eq!(group_set(&"x".repeat(62)).len(), 64);
        assert_eq!(group_set("db.eu-1"), "g-db.eu-1");
    }

    #[test]
    fn nothing_it_is_given_makes_it_panic() {
        let everyone = || block("::/0", &[]);
        let mut bad = group(
            "bad",
            vec![
                ingress(proto::Protocol::Tcp, &[80], block("nonsense", &[])),
                ingress(proto::Protocol::Tcp, &[0], everyone()),
                proto::Rule {
                    peer: None,
                    ..ingress(proto::Protocol::Any, &[], everyone())
                },
                proto::Rule {
                    direction: 9,
                    protocol: 77,
                    ..ingress(proto::Protocol::Any, &[], everyone())
                },
                egress(proto::Protocol::Any, &[], peer_group("ünïcödé")),
                egress(
                    proto::Protocol::Udp,
                    &[53],
                    block("10.0.0.0/8", &["11.0.0.0/8"]),
                ),
                egress(proto::Protocol::Udp, &[53], block("::/0", &["10.0.0.0/8"])),
            ],
            &["nonsense", "10.0.0.1", "2a00::1"],
        );
        bad.id = "not the key".to_string();
        let groups = BTreeMap::from([("bad".to_string(), bad)]);
        let pods = [pod(1, &["bad"]), pod(2, &["", "ünïcödé"]), pod(1, &["bad"])];
        let top = a("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff");
        let denied = [
            Prefix::V4(Ipv4Addr::BROADCAST, 255),
            Prefix::V6(top, 200),
            Prefix::V4(Ipv4Addr::BROADCAST, 0),
        ];
        let policy = compile(&groups, &pods, &denied);
        assert!(policy.pods[0].ingress.is_empty() && policy.pods[0].egress.is_empty());
        assert_eq!(policy.pods[2].missing, ["", "ünïcödé"]);
        assert_eq!(
            policy.sets[DENY4].ranges,
            [range4("0.0.0.0", "255.255.255.255")]
        );
        // A set of numbers it never makes.
        let odd = AddressRange::new(Family::V4, (0, u128::MAX));
        assert_eq!(odd, range4("0.0.0.0", "255.255.255.255"));
    }

    #[test]
    fn a_member_that_comes_or_goes_changes_elements() {
        let web = |members: &[&str]| {
            let rules = vec![ingress(proto::Protocol::Tcp, &[80], peer_group("lb"))];
            store(vec![group("web", rules, &[]), group("lb", vec![], members)])
        };
        let pods = [pod(1, &["web"])];
        let before = compile_with(&web(&["2a00:2::1", "2a00:2::3"]), &pods);
        assert_eq!(delta(&before, &before), Delta::Elements(BTreeMap::new()));

        // The new member joins its neighbours into one range.
        let after = compile_with(&web(&["2a00:2::1", "2a00:2::2", "2a00:2::3"]), &pods);
        let change = SetChange {
            remove: vec![
                range6("2a00:2::1", "2a00:2::1"),
                range6("2a00:2::3", "2a00:2::3"),
            ],
            add: vec![range6("2a00:2::1", "2a00:2::3")],
        };
        assert_eq!(delta(&before, &after), elements("g-lb", change));
        // And back: the range goes, the two come again.
        let Delta::Elements(back) = delta(&after, &before) else {
            panic!("the table was replaced");
        };
        assert_eq!(back["g-lb"].remove, vec![range6("2a00:2::1", "2a00:2::3")]);
        assert_eq!(back["g-lb"].add.len(), 2);

        // A member leaves: exactly its range goes.
        let after = compile_with(&web(&["2a00:2::1"]), &pods);
        let change = SetChange {
            remove: vec![range6("2a00:2::3", "2a00:2::3")],
            add: vec![],
        };
        assert_eq!(delta(&before, &after), elements("g-lb", change));

        // A peer group that arrives fills its empty set.
        let mut groups = web(&[]);
        groups.remove("lb");
        let without = compile_with(&groups, &pods);
        let change = SetChange {
            remove: vec![],
            add: vec![
                range6("2a00:2::1", "2a00:2::1"),
                range6("2a00:2::3", "2a00:2::3"),
            ],
        };
        assert_eq!(delta(&without, &before), elements("g-lb", change));
    }

    #[test]
    fn ipv4_sets_change_elements_too() {
        let rules = vec![egress(
            proto::Protocol::Tcp,
            &[443],
            block("0.0.0.0/0", &[]),
        )];
        let groups = store(vec![group("web", rules, &[])]);
        let pods = [pod(1, &["web"])];
        let compile_denied = |denied: &[&str]| compile(&groups, &pods, &prefixes(denied));
        let before = compile_denied(&["192.0.2.7/32"]);

        // The node's address changes.
        let after = compile_denied(&["192.0.2.9/32"]);
        let change = SetChange {
            remove: vec![range4("192.0.2.7", "192.0.2.7")],
            add: vec![range4("192.0.2.9", "192.0.2.9")],
        };
        assert_eq!(delta(&before, &after), elements(DENY4, change));

        // A denied prefix next to the link-local one makes it grow: it goes
        // and comes again, whole.
        let after = compile_denied(&["192.0.2.7/32", "169.255.0.0/16"]);
        let change = SetChange {
            remove: vec![range4("169.254.0.0", "169.254.255.255")],
            add: vec![range4("169.254.0.0", "169.255.255.255")],
        };
        assert_eq!(delta(&before, &after), elements(DENY4, change));
        let Delta::Elements(back) = delta(&after, &before) else {
            panic!("the table was replaced");
        };
        assert_eq!(back[DENY4].add, [range4("169.254.0.0", "169.254.255.255")]);
    }

    #[test]
    fn other_changes_replace_the_table() {
        let rules = vec![ingress(proto::Protocol::Tcp, &[80], block("::/0", &[]))];
        let groups = store(vec![group("web", rules, &[])]);
        let pods = [pod(1, &["web"])];
        let before = compile_with(&groups, &pods);

        let mut changed = groups.clone();
        if let Some(web) = changed.get_mut("web") {
            web.rules[0].ports[0].first = 8080;
        }
        assert_eq!(
            delta(&before, &compile_with(&changed, &pods)),
            Delta::Replace,
            "a port"
        );

        for (peer, why) in [
            (block("2a00::/16", &[]), "a block"),
            (block("0.0.0.0/0", &[]), "a block of the other family"),
        ] {
            let mut changed = groups.clone();
            if let Some(web) = changed.get_mut("web") {
                web.rules[0].direction = proto::Direction::Egress as i32;
                web.rules[0].peer = Some(peer);
            }
            assert_eq!(
                delta(&before, &compile_with(&changed, &pods)),
                Delta::Replace,
                "{why}"
            );
        }

        for (pods, why) in [
            (vec![pod(1, &["web"]), pod(2, &[])], "a pod more"),
            (vec![], "no pods"),
            (vec![pod(1, &["web", "db"])], "a missing group"),
            (vec![pod(1, &[])], "the default group"),
        ] {
            assert_eq!(
                delta(&before, &compile_with(&groups, &pods)),
                Delta::Replace,
                "{why}"
            );
        }

        // A set that changes its family, as no compiled one does.
        let mut other = before.clone();
        if let Some(set) = other.sets.get_mut(DENY4) {
            set.family = Family::V6;
        }
        assert_eq!(delta(&before, &other), Delta::Replace);

        // The denied prefixes are elements of a set.
        let denied = compile(&groups, &pods, &prefixes(&["2a00:dead::/32"]));
        let change = SetChange {
            remove: vec![],
            add: vec![range6(
                "2a00:dead::",
                "2a00:dead:ffff:ffff:ffff:ffff:ffff:ffff",
            )],
        };
        assert_eq!(delta(&before, &denied), elements(DENY6, change));
    }
}
