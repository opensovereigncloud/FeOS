// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The compiler: the node's security groups and pods become a [`Policy`],
//! named sets of addresses and what each pod may send and receive, which
//! feosd writes as nftables (ADR 0012). Pure: no I/O, and no panics on
//! anything it is given.

use crate::group::{self, Direction, Peer, Protocol};
use crate::prefix::Prefix;
use crate::ranges::{self, Range};
use crate::registry;
use feos_proto::network_service::SecurityGroup;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::net::Ipv6Addr;
use std::ops::RangeInclusive;

/// The set of the default group's internet: global IPv6 addresses, and
/// global IPv4 addresses under the NAT64 prefix.
pub const INTERNET: &str = "internet";
/// The set of what no pod reaches and nothing reaches a pod from: the
/// denied prefixes, and link-local, loopback and multicast addresses.
pub const DENY: &str = "deny";

/// Set names are at most this long, which every kernel takes.
const MAX_SET_NAME: usize = 64;

/// A range of addresses, first and last.
pub type AddressRange = RangeInclusive<Ipv6Addr>;

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
    /// Named sets of addresses, normal: sorted, and their ranges neither
    /// overlap nor touch. Sets are [`INTERNET`], [`DENY`], `g-<group ID>`
    /// for a group's members, and `b-<12 hex digits>` for an address block
    /// without its exceptions, which identical blocks share.
    pub sets: BTreeMap<String, Vec<AddressRange>>,
    /// The pods, by ID.
    pub pods: Vec<PodPolicy>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PodPolicy {
    pub pod_id: String,
    pub tap: String,
    pub address: Ipv6Addr,
    /// What may reach the pod: its peers are sources.
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
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SetChange {
    pub remove: Vec<AddressRange>,
    pub add: Vec<AddressRange>,
}

/// Compiles the node's groups and pods. `nat64` is the NAT64 prefix, a
/// /96 of which the first 96 bits count; IPv4 prefixes in `denied` mean
/// their addresses under it.
pub fn compile(
    groups: &BTreeMap<String, SecurityGroup>,
    pods: &[Pod],
    nat64: Ipv6Addr,
    denied: &[Prefix],
) -> Policy {
    let nat64 = ranges::nat64_base(u128::from(nat64));
    let mut sets: BTreeMap<String, Vec<Range>> = BTreeMap::new();
    let deny = denied
        .iter()
        .map(|prefix| prefix.range(nat64))
        .chain(registry::always_denied(nat64))
        .collect();
    sets.insert(DENY.to_string(), ranges::normalize(deny));
    sets.insert(INTERNET.to_string(), registry::internet(nat64));

    let mut policies: Vec<PodPolicy> = pods
        .iter()
        .map(|pod| compile_pod(pod, groups, nat64, &mut sets))
        .collect();
    policies.sort_by(|a, b| a.pod_id.cmp(&b.pod_id));
    Policy {
        sets: sets
            .into_iter()
            .map(|(name, set)| (name, ranges::to_addresses(&set)))
            .collect(),
        pods: policies,
    }
}

fn compile_pod(
    pod: &Pod,
    groups: &BTreeMap<String, SecurityGroup>,
    nat64: u128,
    sets: &mut BTreeMap<String, Vec<Range>>,
) -> PodPolicy {
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
        policy.egress.push(Allow {
            protocol: Protocol::Any,
            ports: Vec::new(),
            peer: INTERNET.to_string(),
        });
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
        let name = match &rule.peer {
            Peer::Group(id) => {
                let name = group_set(id);
                sets.entry(name.clone())
                    .or_insert_with(|| members(groups.get(id)));
                name
            }
            Peer::Block { cidr, except } => {
                let set = block(cidr, except, nat64);
                let name = block_set(&set);
                sets.entry(name.clone()).or_insert(set);
                name
            }
        };
        let allow = (rule.protocol, name, rule.ports);
        match rule.direction {
            Direction::Ingress => ingress.push(allow),
            Direction::Egress => egress.push(allow),
        }
    }
    policy.ingress = merge(ingress);
    policy.egress = merge(egress);
    policy
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

/// The addresses of a block without its exceptions.
fn block(cidr: &Prefix, except: &[Prefix], nat64: u128) -> Vec<Range> {
    let holes = except.iter().map(|prefix| prefix.range(nat64)).collect();
    ranges::subtract(&[cidr.range(nat64)], &ranges::normalize(holes))
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

/// The set of a block: named after its addresses.
fn block_set(set: &[Range]) -> String {
    let bytes: Vec<u8> = set
        .iter()
        .flat_map(|&(start, end)| [start.to_be_bytes(), end.to_be_bytes()])
        .flatten()
        .collect();
    format!("b-{}", hash(&bytes))
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
fn merge(rules: Vec<(Protocol, String, Ports)>) -> Vec<Allow> {
    // None: every port.
    let mut merged: BTreeMap<(Protocol, String), Option<Ports>> = BTreeMap::new();
    for (protocol, peer, ports) in rules {
        let entry = merged
            .entry((protocol, peer))
            .or_insert_with(|| Some(Vec::new()));
        match entry {
            Some(all) if !ports.is_empty() => all.extend(ports),
            _ => *entry = None,
        }
    }
    merged
        .into_iter()
        .map(|((protocol, peer), ports)| Allow {
            protocol,
            peer,
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
        if before == after {
            continue;
        }
        let element = |range: &AddressRange| (*range.start(), *range.end());
        let before: BTreeSet<_> = before.iter().map(element).collect();
        let after: BTreeSet<_> = after.iter().map(element).collect();
        let range = |&(start, end): &(Ipv6Addr, Ipv6Addr)| start..=end;
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

    const NAT64: &str = "64:ff9b::";

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

    fn compile_with(groups: &BTreeMap<String, SecurityGroup>, pods: &[Pod]) -> Policy {
        compile(groups, pods, a(NAT64), &[])
    }

    fn holds(set: &[AddressRange], address: &str) -> bool {
        set.iter().any(|range| range.contains(&a(address)))
    }

    fn range(first: &str, last: &str) -> AddressRange {
        a(first)..=a(last)
    }

    fn allow(protocol: Protocol, ports: &[(u16, u16)], peer: &str) -> Allow {
        Allow {
            protocol,
            ports: ports.iter().map(|&(first, last)| first..=last).collect(),
            peer: peer.to_string(),
        }
    }

    fn elements(name: &str, change: SetChange) -> Delta {
        Delta::Elements(BTreeMap::from([(name.to_string(), change)]))
    }

    #[test]
    fn a_pod_without_groups_gets_the_default_group() {
        let policy = compile_with(&BTreeMap::new(), &[pod(1, &[])]);
        let pod = &policy.pods[0];
        assert_eq!(pod.ingress, vec![], "nothing in");
        assert_eq!(pod.egress, vec![allow(Protocol::Any, &[], INTERNET)]);
        assert!(pod.missing.is_empty());
        assert_eq!(pod.tap, "pod-11111111");
        assert_eq!(pod.address, a("2a00:1::1"));
        assert_eq!(policy.sets.keys().collect::<Vec<_>>(), [DENY, INTERNET]);
        let internet = &policy.sets[INTERNET];
        for address in ["64:ff9b::808:808", "2001:4860:4860::8888"] {
            assert!(holds(internet, address), "{address}");
        }
        for address in [
            "64:ff9b::a01:203",
            "64:ff9b::c0a8:101",
            "fd00::1",
            "fe80::1",
            "2001:db8::1",
        ] {
            assert!(!holds(internet, address), "{address}");
        }
    }

    #[test]
    fn denied_prefixes_and_what_is_denied_always() {
        let denied = [
            "169.254.169.254/32".parse().unwrap(),
            "fd00:ffff::/32".parse().unwrap(),
        ];
        let policy = compile(&BTreeMap::new(), &[], a(NAT64), &denied);
        let deny = &policy.sets[DENY];
        for address in [
            "64:ff9b::a9fe:a9fe",
            "fd00:ffff::1",
            "fe80::1",
            "::1",
            "ff02::1",
            "64:ff9b::7f00:1",
            "64:ff9b::e000:fb",
        ] {
            assert!(holds(deny, address), "{address}");
        }
        assert!(!holds(deny, "fd00:fffe::1"));
        assert!(!holds(deny, "64:ff9b::808:808"));
        // The link-local IPv4 /16 holds the denied /32: they are one range.
        assert!(deny.contains(&range("64:ff9b::a9fe:0", "64:ff9b::a9fe:ffff")));
    }

    #[test]
    fn a_pod_whose_groups_are_missing_is_allowed_nothing() {
        let rules = vec![ingress(proto::Protocol::Tcp, &[80], block("::/0", &[]))];
        let groups = store(vec![group("web", rules, &[])]);
        let policy = compile_with(&groups, &[pod(1, &["web", "db", "cache", "db"])]);
        let pod = &policy.pods[0];
        assert_eq!(pod.missing, ["cache", "db"]);
        assert!(pod.ingress.is_empty() && pod.egress.is_empty());
        assert_eq!(policy.sets.len(), 2, "no sets for its rules");
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
                allow(Protocol::Tcp, &[(80, 80), (443, 443)], "g-lb"),
                allow(Protocol::Icmpv6, &[], "g-web"),
            ]
        );
        assert_eq!(pod.egress, vec![allow(Protocol::Any, &[], "g-db")]);
        assert_eq!(policy.sets["g-lb"], vec![range("2a00:2::5", "2a00:2::7")]);
        assert_eq!(policy.sets["g-web"], vec![range("2a00:1::1", "2a00:1::1")]);
        assert_eq!(
            policy.sets["g-db"],
            vec![],
            "a group not on the node allows nothing"
        );
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
                vec![ingress(proto::Protocol::Udp, &[53], everyone_but_private())],
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
        assert_eq!(&policy.pods[0].ingress[0].peer, shared);
        let tcp = policy.pods[1]
            .egress
            .iter()
            .find(|a| a.protocol == Protocol::Tcp);
        assert_eq!(&tcp.unwrap().peer, shared);
        assert_eq!(
            policy.sets[shared],
            vec![
                range("64:ff9b::", "64:ff9b::9ff:ffff"),
                range("64:ff9b::b00:0", "64:ff9b::ffff:ffff"),
            ]
        );
        let any = policy.pods[1]
            .egress
            .iter()
            .find(|a| a.protocol == Protocol::Any);
        assert_eq!(
            policy.sets[&any.unwrap().peer],
            vec![range("::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")]
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
        let tcp = allow(Protocol::Tcp, &[(80, 81), (443, 443)], set);
        assert_eq!(
            policy.pods[0].ingress,
            vec![tcp.clone(), allow(Protocol::Udp, &[(53, 53)], set)]
        );
        assert_eq!(
            policy.pods[1].ingress,
            vec![tcp, allow(Protocol::Udp, &[], set)],
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
            ],
            &["nonsense", "10.0.0.1", "2a00::1"],
        );
        bad.id = "not the key".to_string();
        let groups = BTreeMap::from([("bad".to_string(), bad)]);
        let pods = [pod(1, &["bad"]), pod(2, &["", "ünïcödé"]), pod(1, &["bad"])];
        let top = a("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff");
        let policy = compile(&groups, &pods, top, &[]);
        assert!(policy.pods[0].ingress.is_empty() && policy.pods[0].egress.is_empty());
        assert_eq!(policy.pods[2].missing, ["", "ünïcödé"]);
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
                range("2a00:2::1", "2a00:2::1"),
                range("2a00:2::3", "2a00:2::3"),
            ],
            add: vec![range("2a00:2::1", "2a00:2::3")],
        };
        assert_eq!(delta(&before, &after), elements("g-lb", change));
        // And back: the range goes, the two come again.
        let Delta::Elements(back) = delta(&after, &before) else {
            panic!("the table was replaced");
        };
        assert_eq!(back["g-lb"].remove, vec![range("2a00:2::1", "2a00:2::3")]);
        assert_eq!(back["g-lb"].add.len(), 2);

        // A member leaves: exactly its range goes.
        let after = compile_with(&web(&["2a00:2::1"]), &pods);
        let change = SetChange {
            remove: vec![range("2a00:2::3", "2a00:2::3")],
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
                range("2a00:2::1", "2a00:2::1"),
                range("2a00:2::3", "2a00:2::3"),
            ],
        };
        assert_eq!(delta(&without, &before), elements("g-lb", change));
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

        let mut changed = groups.clone();
        if let Some(web) = changed.get_mut("web") {
            web.rules[0].peer = Some(block("2a00::/16", &[]));
        }
        assert_eq!(
            delta(&before, &compile_with(&changed, &pods)),
            Delta::Replace,
            "a block"
        );

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
        // Another NAT64 prefix moves the IPv4 addresses of sets, and
        // renames the sets of IPv4 blocks.
        let moved = compile(&groups, &pods, a("2a00:64::"), &[]);
        let Delta::Elements(changes) = delta(&before, &moved) else {
            panic!("the table was replaced");
        };
        assert_eq!(changes.keys().collect::<Vec<_>>(), [DENY, INTERNET]);
        let mut ipv4 = groups.clone();
        if let Some(web) = ipv4.get_mut("web") {
            web.rules[0].peer = Some(block("0.0.0.0/0", &[]));
        }
        let before = compile_with(&ipv4, &pods);
        let moved = compile(&ipv4, &pods, a("2a00:64::"), &[]);
        assert_eq!(delta(&before, &moved), Delta::Replace);
        let before = compile_with(&groups, &pods);

        // The denied prefixes are elements of a set.
        let denied = ["2a00:dead::/32".parse().unwrap()];
        let denied = compile(&groups, &pods, a(NAT64), &denied);
        let change = SetChange {
            remove: vec![],
            add: vec![range(
                "2a00:dead::",
                "2a00:dead:ffff:ffff:ffff:ffff:ffff:ffff",
            )],
        };
        assert_eq!(delta(&before, &denied), elements(DENY, change));
    }
}
