// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's nftables table, `inet feos`, from the compiled security groups
//! (ADR 0010, 0012, 0013):
//!
//! - `prerouting` drops what a pod sends from an address that is not
//!   routed back out its TAP, any IPv4, and router advertisements and
//!   redirects.
//! - `input` takes neighbor discovery and DNS to the resolver from pods,
//!   nothing else of theirs, and nothing from NAT64's device.
//! - `forward` copies the first packets of the TCP connections pods open
//!   to nflog, passes what belongs to a connection, and sends what is new
//!   through `pod-out` and `pod-in`, verdict maps from a TAP to the pod's
//!   chains `out-<TAP>` and `in-<TAP>`. Those drop denied prefixes, return
//!   what a rule of the pod's groups allows, and drop the rest. A pod's
//!   chains end in `return` rather than `accept`, which would end the
//!   base chain: a packet between two pods passes both.
//! - `postrouting` masquerades the NAT64 pool.
//!
//! Base chains accept by default: the node's other traffic is not
//! feosd's. Drops are logged to nflog, limited per pod.

use feos_utils::nft::matches::*;
use feos_utils::nft::{
    ct, Chain, CmpOp, Direction, Element, ElementOp, Expr, Family, Hook, KeyType, Per,
    Policy as ChainPolicy, Rule, Set, Table, Verdict,
};
use network_service::{AddressRange, Allow, Policy, Protocol, SetChange, DENY};
use std::collections::BTreeMap;
use std::net::{Ipv4Addr, Ipv6Addr};
use traffic_service::{prefix, COPY_GROUP, COPY_SNAPLEN, DROP_GROUP};

/// feosd's table, in the inet family.
pub const TABLE: &str = "feos";
/// The verdict maps from a pod's TAP to its chains.
pub const POD_IN: &str = "pod-in";
pub const POD_OUT: &str = "pod-out";
/// What the names of pods' TAP devices start with.
const PODS: &str = "pod-";
/// Drops are logged at most this often, per pod and reason.
const LOG_RATE: u64 = 10;
const LOG_BURST: u32 = 20;
/// The packets of a connection a pod opens that are copied to nflog: the
/// handshake and the ClientHello, even over two segments.
const COPY_PACKETS: u64 = 5;

const ND_ROUTER_ADVERT: u8 = 134;
const ND_NEIGHBOR_SOLICIT: u8 = 135;
const ND_NEIGHBOR_ADVERT: u8 = 136;
const ND_REDIRECT: u8 = 137;
const DNS_PORT: u16 = 53;

/// What the table needs besides the policy.
#[derive(Clone, Debug, PartialEq)]
pub struct Options {
    /// The resolver's address (ADR 0010).
    pub dns: Ipv6Addr,
    /// With `--nat64=node`: the pool, which is masqueraded and which
    /// nothing opens connections to but NAT64.
    pub pool: Option<(Ipv4Addr, u8)>,
}

/// The pod's chains.
pub fn in_chain(tap: &str) -> String {
    format!("in-{tap}")
}

pub fn out_chain(tap: &str) -> String {
    format!("out-{tap}")
}

pub fn table(policy: &Policy, options: &Options) -> Table {
    let mut sets: Vec<Set> = policy
        .sets
        .iter()
        .map(|(name, ranges)| {
            Set::intervals(
                name,
                KeyType::Ipv6Addr,
                ranges.iter().map(element).collect(),
            )
        })
        .collect();
    let map = |name: &str, chain: fn(&str) -> String| {
        let elements = policy
            .pods
            .iter()
            .map(|pod| Element::ifname(&pod.tap).with_verdict(Verdict::Jump(chain(&pod.tap))))
            .collect();
        Set::verdict_map(name, KeyType::IfName, elements)
    };
    sets.push(map(POD_IN, in_chain));
    sets.push(map(POD_OUT, out_chain));

    let mut chains = Vec::new();
    for pod in &policy.pods {
        chains.push(pod_chain(&in_chain(&pod.tap), &pod.ingress, ipv6_saddr_in));
        chains.push(pod_chain(&out_chain(&pod.tap), &pod.egress, ipv6_daddr_in));
    }
    chains.push(Chain::base(
        "prerouting",
        "filter",
        Hook::Prerouting,
        -150,
        ChainPolicy::Accept,
        prerouting(),
    ));
    chains.push(Chain::base(
        "input",
        "filter",
        Hook::Input,
        0,
        ChainPolicy::Accept,
        input(options),
    ));
    chains.push(Chain::base(
        "forward",
        "filter",
        Hook::Forward,
        0,
        ChainPolicy::Accept,
        forward(options),
    ));
    chains.push(Chain::base(
        "postrouting",
        "nat",
        Hook::Postrouting,
        100,
        ChainPolicy::Accept,
        postrouting(options),
    ));
    Table {
        family: Family::Inet,
        name: TABLE.to_string(),
        sets,
        chains,
    }
}

/// The changes of a delta, in one batch: removals before additions.
pub fn element_ops(changes: &BTreeMap<String, SetChange>) -> Vec<ElementOp> {
    let removals = changes
        .iter()
        .filter(|(_, change)| !change.remove.is_empty())
        .map(|(set, change)| ElementOp::Delete {
            set: set.clone(),
            elements: change.remove.iter().map(element).collect(),
        });
    let additions = changes
        .iter()
        .filter(|(_, change)| !change.add.is_empty())
        .map(|(set, change)| ElementOp::Add {
            set: set.clone(),
            elements: change.add.iter().map(element).collect(),
        });
    removals.chain(additions).collect()
}

fn element(range: &AddressRange) -> Element {
    Element::ipv6_range(*range.start(), *range.end())
}

fn verdict(verdict: Verdict) -> Vec<Expr> {
    vec![Expr::Verdict(verdict)]
}

/// Logs a dropped packet to the drop group, limited.
fn log(reason: &str) -> Vec<Expr> {
    [
        limit(LOG_RATE, Per::Second, LOG_BURST),
        log_group(DROP_GROUP, reason, 0),
    ]
    .concat()
}

fn from_pods() -> Vec<Expr> {
    iifname_prefix(CmpOp::Eq, PODS)
}

fn to_pods() -> Vec<Expr> {
    oifname_prefix(CmpOp::Eq, PODS)
}

/// A pod's chain for one direction: `peer` matches the far end's address
/// in a set, the destination of what the pod sends, the source of what
/// it receives.
fn pod_chain(name: &str, allows: &[Allow], peer: fn(&str) -> Vec<Expr>) -> Chain {
    let mut rules: Vec<Rule> = vec![
        [peer(DENY), log(prefix::DENY)].concat(),
        [peer(DENY), verdict(Verdict::Drop)].concat(),
    ];
    for allow in allows {
        let protocol = allow.protocol.number().map(l4proto).unwrap_or_default();
        let ports = match allow.protocol {
            Protocol::Tcp | Protocol::Udp => allow.ports.as_slice(),
            _ => &[],
        };
        if ports.is_empty() {
            rules.push([protocol, peer(&allow.peer), verdict(Verdict::Return)].concat());
            continue;
        }
        for range in ports {
            rules.push(
                [
                    protocol.clone(),
                    th_dport_range(*range.start(), *range.end()),
                    peer(&allow.peer),
                    verdict(Verdict::Return),
                ]
                .concat(),
            );
        }
    }
    rules.push(log(prefix::NO_RULE));
    rules.push(verdict(Verdict::Drop));
    Chain::regular(name, rules)
}

fn prerouting() -> Vec<Rule> {
    let spoofed = fib_saddr_iif_oif_missing;
    vec![
        // Pods have no IPv4.
        [from_pods(), ipv4(), log(prefix::SPOOFED)].concat(),
        [from_pods(), ipv4(), verdict(Verdict::Drop)].concat(),
        // A pod sends from its address alone: the one routed to its TAP.
        [from_pods(), spoofed(), log(prefix::SPOOFED)].concat(),
        [from_pods(), spoofed(), verdict(Verdict::Drop)].concat(),
        [
            from_pods(),
            icmpv6_type(ND_ROUTER_ADVERT),
            verdict(Verdict::Drop),
        ]
        .concat(),
        [
            from_pods(),
            icmpv6_type(ND_REDIRECT),
            verdict(Verdict::Drop),
        ]
        .concat(),
    ]
}

fn input(options: &Options) -> Vec<Rule> {
    let mut rules = Vec::new();
    if options.pool.is_some() {
        // What NAT64 translated is forwarded, never the node's.
        rules.push([iifname(CmpOp::Eq, nat64::DEVICE), verdict(Verdict::Drop)].concat());
    }
    let dns = |protocol| {
        [
            from_pods(),
            ipv6_daddr(CmpOp::Eq, options.dns),
            l4proto(protocol),
            th_dport(CmpOp::Eq, DNS_PORT),
            verdict(Verdict::Accept),
        ]
        .concat()
    };
    rules.extend([
        [
            from_pods(),
            icmpv6_type(ND_NEIGHBOR_SOLICIT),
            verdict(Verdict::Accept),
        ]
        .concat(),
        [
            from_pods(),
            icmpv6_type(ND_NEIGHBOR_ADVERT),
            verdict(Verdict::Accept),
        ]
        .concat(),
        dns(UDP),
        dns(TCP),
        [from_pods(), verdict(Verdict::Drop)].concat(),
    ]);
    rules
}

fn forward(options: &Options) -> Vec<Rule> {
    let mut rules = vec![
        // Before anything passes: copies of the first packets of the
        // connections pods open, for their server names (ADR 0013).
        [
            from_pods(),
            l4proto(TCP),
            ct_direction(Direction::Original),
            ct_original_packets(CmpOp::Lt, COPY_PACKETS),
            log_group(COPY_GROUP, prefix::COPY, COPY_SNAPLEN),
        ]
        .concat(),
        [
            ct_state(ct::ESTABLISHED | ct::RELATED),
            verdict(Verdict::Accept),
        ]
        .concat(),
        [from_pods(), ct_state(ct::INVALID), verdict(Verdict::Drop)].concat(),
        [to_pods(), ct_state(ct::INVALID), verdict(Verdict::Drop)].concat(),
    ];
    if let Some((pool, len)) = options.pool {
        // The pool takes replies to what NAT64 sent, and nothing new.
        rules.push(
            [
                ipv4(),
                ipv4_prefix(16, CmpOp::Eq, pool, len),
                iifname(CmpOp::Neq, nat64::DEVICE),
                verdict(Verdict::Drop),
            ]
            .concat(),
        );
    }
    rules.push(iifname_vmap(POD_OUT));
    rules.push(oifname_vmap(POD_IN));
    rules
}

fn postrouting(options: &Options) -> Vec<Rule> {
    let Some((pool, len)) = options.pool else {
        return Vec::new();
    };
    vec![[
        ipv4(),
        ipv4_prefix(12, CmpOp::Eq, pool, len),
        oifname(CmpOp::Neq, nat64::DEVICE),
        vec![Expr::Masquerade],
    ]
    .concat()]
}

#[cfg(test)]
mod tests {
    use super::*;
    use network_service::{PodPolicy, INTERNET};

    fn a(s: &str) -> Ipv6Addr {
        s.parse().unwrap()
    }

    fn range(first: &str, last: &str) -> AddressRange {
        a(first)..=a(last)
    }

    fn options() -> Options {
        Options {
            dns: a("fdfd:7b5:61d9::53"),
            pool: Some((Ipv4Addr::new(10, 200, 0, 0), 16)),
        }
    }

    fn pod(tap: &str, ingress: Vec<Allow>, egress: Vec<Allow>) -> PodPolicy {
        PodPolicy {
            pod_id: format!("{tap}-id"),
            tap: tap.to_string(),
            address: a("fd00:fe05:1::2"),
            ingress,
            egress,
            missing: Vec::new(),
        }
    }

    fn allow(protocol: Protocol, ports: &[(u16, u16)], peer: &str) -> Allow {
        Allow {
            protocol,
            ports: ports.iter().map(|&(first, last)| first..=last).collect(),
            peer: peer.to_string(),
        }
    }

    fn policy() -> Policy {
        Policy {
            sets: BTreeMap::from([
                (DENY.to_string(), vec![range("fe80::", "febf::")]),
                (INTERNET.to_string(), vec![range("2000::", "3fff::")]),
                ("g-web".to_string(), vec![]),
            ]),
            pods: vec![
                pod(
                    "pod-11111111",
                    vec![allow(Protocol::Tcp, &[(80, 80), (8000, 8100)], "g-web")],
                    vec![allow(Protocol::Any, &[], INTERNET)],
                ),
                pod("pod-22222222", vec![], vec![]),
            ],
        }
    }

    fn chain<'a>(table: &'a Table, name: &str) -> &'a Chain {
        table
            .chains
            .iter()
            .find(|chain| chain.name == name)
            .unwrap_or_else(|| panic!("no chain {name}"))
    }

    fn verdict_of(rule: &Rule) -> Option<&Verdict> {
        match rule.last() {
            Some(Expr::Verdict(verdict)) => Some(verdict),
            _ => None,
        }
    }

    fn logs(rule: &Rule, group: u16, reason: &str) -> bool {
        rule.iter().any(|expr| {
            matches!(expr, Expr::Log { group: g, prefix, .. } if *g == group && prefix == reason)
        })
    }

    #[test]
    fn every_pod_has_its_chains_and_map_entries() {
        let table = table(&policy(), &options());
        assert_eq!((table.family, table.name.as_str()), (Family::Inet, "feos"));
        let names: Vec<&str> = table.sets.iter().map(|set| set.name.as_str()).collect();
        assert_eq!(names, [DENY, "g-web", INTERNET, POD_IN, POD_OUT]);
        // An empty set is a set all the same.
        assert!(table.sets[1].elements.is_empty() && table.sets[1].interval);
        let pod_in = &table.sets[3];
        assert!(pod_in.map);
        assert_eq!(
            pod_in.elements,
            [
                Element::ifname("pod-11111111")
                    .with_verdict(Verdict::Jump("in-pod-11111111".to_string())),
                Element::ifname("pod-22222222")
                    .with_verdict(Verdict::Jump("in-pod-22222222".to_string())),
            ]
        );
        let names: Vec<&str> = table.chains.iter().map(|c| c.name.as_str()).collect();
        assert_eq!(
            names,
            [
                "in-pod-11111111",
                "out-pod-11111111",
                "in-pod-22222222",
                "out-pod-22222222",
                "prerouting",
                "input",
                "forward",
                "postrouting"
            ]
        );
        for chain in &table.chains[..4] {
            assert!(chain.base.is_none(), "{} is a regular chain", chain.name);
        }
    }

    #[test]
    fn a_pod_chain_returns_what_is_allowed_and_drops_the_rest() {
        let table = table(&policy(), &options());
        let ingress = &chain(&table, "in-pod-11111111").rules;
        // Denied prefixes first, logged, then dropped.
        assert!(logs(&ingress[0], DROP_GROUP, "deny"));
        assert_eq!(verdict_of(&ingress[1]), Some(&Verdict::Drop));
        assert!(ingress[1].contains(&Expr::Lookup {
            set: DENY.to_string(),
            invert: false
        }));
        // A rule per port range, which returns: an accept would end the
        // base chain before the other pod's chain.
        let allowed = &ingress[2..4];
        for rule in allowed {
            assert_eq!(verdict_of(rule), Some(&Verdict::Return));
            assert!(rule.contains(&Expr::Lookup {
                set: "g-web".to_string(),
                invert: false
            }));
        }
        assert_eq!(&allowed[0][..2], &l4proto(TCP)[..]);
        let ports = th_dport_range(8000, 8100);
        assert!(allowed[1]
            .windows(ports.len())
            .any(|w| w == ports.as_slice()));
        assert!(logs(&ingress[4], DROP_GROUP, "no-rule"));
        assert_eq!(verdict_of(&ingress[5]), Some(&Verdict::Drop));
        assert_eq!(ingress.len(), 6);
        // The source is the peer in, the destination out.
        let source = ipv6_saddr_in("g-web");
        assert!(ingress[2]
            .windows(source.len())
            .any(|w| w == source.as_slice()));
        let egress = &chain(&table, "out-pod-11111111").rules;
        assert_eq!(
            egress[2],
            [ipv6_daddr_in(INTERNET), verdict(Verdict::Return)].concat()
        );
        // Allowed nothing: no return at all.
        let nothing = &chain(&table, "out-pod-22222222").rules;
        assert!(nothing
            .iter()
            .all(|rule| verdict_of(rule) != Some(&Verdict::Return)));
        assert_eq!(verdict_of(nothing.last().unwrap()), Some(&Verdict::Drop));
    }

    #[test]
    fn forward_copies_first_and_jumps_through_the_maps() {
        let table = table(&policy(), &options());
        let forward = chain(&table, "forward");
        let base = forward.base.as_ref().unwrap();
        assert_eq!(
            (base.hook, base.policy),
            (Hook::Forward, ChainPolicy::Accept)
        );
        let rules = &forward.rules;
        // The copy is not terminal, and comes before established packets
        // pass.
        assert!(logs(&rules[0], COPY_GROUP, "copy"));
        assert_eq!(verdict_of(&rules[0]), None);
        assert!(rules[0]
            .windows(2)
            .any(|w| w == ct_direction(Direction::Original).as_slice()));
        assert!(rules[0].contains(&Expr::Log {
            group: COPY_GROUP,
            prefix: "copy".to_string(),
            snaplen: COPY_SNAPLEN
        }));
        assert_eq!(
            rules[1],
            [
                ct_state(ct::ESTABLISHED | ct::RELATED),
                verdict(Verdict::Accept)
            ]
            .concat()
        );
        let n = rules.len();
        assert_eq!(rules[n - 2], iifname_vmap(POD_OUT));
        assert_eq!(rules[n - 1], oifname_vmap(POD_IN));
        // New connections to the pool only come from NAT64.
        assert!(rules[4].contains(&Expr::Cmp {
            op: CmpOp::Neq,
            data: ifname(nat64::DEVICE)
        }));
        let postrouting = &chain(&table, "postrouting").rules;
        assert_eq!(postrouting.len(), 1);
        assert_eq!(postrouting[0].last(), Some(&Expr::Masquerade));
    }

    #[test]
    fn input_takes_neighbor_discovery_and_dns_from_pods() {
        let table = table(&policy(), &options());
        let rules = &chain(&table, "input").rules;
        assert_eq!(
            rules[0],
            [iifname(CmpOp::Eq, "feos-nat64"), verdict(Verdict::Drop)].concat()
        );
        let accepted: Vec<&Rule> = rules
            .iter()
            .filter(|rule| verdict_of(rule) == Some(&Verdict::Accept))
            .collect();
        assert_eq!(accepted.len(), 4);
        for rule in &accepted[2..] {
            assert!(rule
                .windows(ipv6_daddr(CmpOp::Eq, options().dns).len())
                .any(|w| w == ipv6_daddr(CmpOp::Eq, options().dns).as_slice()));
        }
        assert_eq!(
            rules.last().unwrap(),
            &[from_pods(), verdict(Verdict::Drop)].concat()
        );
        let prerouting = &chain(&table, "prerouting");
        assert_eq!(prerouting.base.as_ref().unwrap().priority, -150);
        assert!(logs(&prerouting.rules[2], DROP_GROUP, "spoofed"));
        assert_eq!(verdict_of(&prerouting.rules[3]), Some(&Verdict::Drop));
    }

    #[test]
    fn without_node_nat64_there_is_no_pool() {
        let options = Options {
            pool: None,
            ..options()
        };
        let table = table(&policy(), &options);
        assert!(chain(&table, "postrouting").rules.is_empty());
        assert!(!chain(&table, "input").rules[0].contains(&Expr::Cmp {
            op: CmpOp::Eq,
            data: ifname(nat64::DEVICE)
        }));
        let forward = &chain(&table, "forward").rules;
        assert_eq!(forward.len(), 6);
    }

    #[test]
    fn deltas_remove_before_they_add() {
        let changes = BTreeMap::from([
            (
                "g-a".to_string(),
                SetChange {
                    remove: vec![],
                    add: vec![range("fd00::1", "fd00::1")],
                },
            ),
            (
                "g-b".to_string(),
                SetChange {
                    remove: vec![range("fd00::2", "fd00::3")],
                    add: vec![range("fd00::2", "fd00::4")],
                },
            ),
        ]);
        let ops = element_ops(&changes);
        assert_eq!(
            ops,
            [
                ElementOp::Delete {
                    set: "g-b".to_string(),
                    elements: vec![Element::ipv6_range(a("fd00::2"), a("fd00::3"))]
                },
                ElementOp::Add {
                    set: "g-a".to_string(),
                    elements: vec![Element::ipv6(a("fd00::1"))]
                },
                ElementOp::Add {
                    set: "g-b".to_string(),
                    elements: vec![Element::ipv6_range(a("fd00::2"), a("fd00::4"))]
                },
            ]
        );
    }
}
