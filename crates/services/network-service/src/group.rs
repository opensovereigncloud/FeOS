// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What a valid security group is, and its rules as the compiler reads
//! them.

use crate::prefix::Prefix;
use feos_proto::network_service::{self as proto, rule, SecurityGroup};
use std::collections::BTreeSet;
use std::net::Ipv6Addr;
use tonic::Status;

/// The protocols a rule allows.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Protocol {
    Any,
    Tcp,
    Udp,
    Icmpv6,
    Icmp,
}

impl Protocol {
    /// The IP protocol's number; none for any protocol.
    pub fn number(self) -> Option<u8> {
        match self {
            Protocol::Any => None,
            Protocol::Tcp => Some(6),
            Protocol::Udp => Some(17),
            Protocol::Icmpv6 => Some(58),
            Protocol::Icmp => Some(1),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Direction {
    Ingress,
    Egress,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum Peer {
    /// A prefix, without the prefixes in except.
    Block { cidr: Prefix, except: Vec<Prefix> },
    /// The members of a group.
    Group(String),
}

/// A rule as the compiler reads it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Rule {
    pub direction: Direction,
    pub protocol: Protocol,
    /// First and last port; empty for every port.
    pub ports: Vec<(u16, u16)>,
    pub peer: Peer,
}

/// Checks a group's ID: 1 to 64 letters, digits, "-", "_" and ".".
pub fn check_id(id: &str) -> Result<(), Status> {
    if valid_id(id) {
        Ok(())
    } else {
        Err(Status::invalid_argument(format!(
            "invalid security group ID {id:?}: 1 to 64 letters, digits, \"-\", \"_\" and \".\""
        )))
    }
}

pub(crate) fn valid_id(id: &str) -> bool {
    (1..=64).contains(&id.len())
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// Checks a group, and writes its members as a sorted set in the
/// canonical form of RFC 5952, so that two groups compare as they apply.
pub(crate) fn normalize(mut group: SecurityGroup) -> Result<SecurityGroup, Status> {
    check_id(&group.id)?;
    for (index, rule) in group.rules.iter().enumerate() {
        parse_rule(rule).map_err(|e| {
            Status::invalid_argument(format!("rule {index} of security group {}: {e}", group.id))
        })?;
    }
    group.members = members(&group.members)?
        .iter()
        .map(|m| m.to_string())
        .collect();
    Ok(group)
}

/// Parses members: IPv6 addresses.
pub(crate) fn members(members: &[String]) -> Result<BTreeSet<Ipv6Addr>, Status> {
    members
        .iter()
        .map(|member| {
            member.parse::<Ipv6Addr>().map_err(|_| {
                Status::invalid_argument(format!("member {member:?} is not an IPv6 address"))
            })
        })
        .collect()
}

/// A rule of the API as the compiler reads it, or why it is not valid.
pub(crate) fn parse_rule(rule: &proto::Rule) -> Result<Rule, String> {
    let direction = match proto::Direction::try_from(rule.direction) {
        Ok(proto::Direction::Ingress) => Direction::Ingress,
        Ok(proto::Direction::Egress) => Direction::Egress,
        _ => return Err("it has no direction".to_string()),
    };
    let protocol = match proto::Protocol::try_from(rule.protocol) {
        Ok(proto::Protocol::Any) => Protocol::Any,
        Ok(proto::Protocol::Tcp) => Protocol::Tcp,
        Ok(proto::Protocol::Udp) => Protocol::Udp,
        Ok(proto::Protocol::Icmpv6) => Protocol::Icmpv6,
        Ok(proto::Protocol::Icmp) => Protocol::Icmp,
        Err(_) => return Err(format!("it has an unknown protocol {}", rule.protocol)),
    };
    if !rule.ports.is_empty() && !matches!(protocol, Protocol::Tcp | Protocol::Udp) {
        return Err("only TCP and UDP have ports".to_string());
    }
    let ports = rule
        .ports
        .iter()
        .map(|range| {
            let last = if range.last == 0 {
                range.first
            } else {
                range.last
            };
            match (u16::try_from(range.first), u16::try_from(last)) {
                (Ok(first @ 1..), Ok(last)) if first <= last => Ok((first, last)),
                _ => Err(format!(
                    "ports {}-{} are not a range of 1 to 65535",
                    range.first, range.last
                )),
            }
        })
        .collect::<Result<_, _>>()?;
    let peer = match &rule.peer {
        Some(rule::Peer::Block(block)) => {
            let cidr = block.cidr.parse::<Prefix>()?;
            let except = block
                .except
                .iter()
                .map(|except| {
                    let except = except.parse::<Prefix>()?;
                    if cidr.contains(&except) {
                        Ok(except)
                    } else {
                        Err(format!("{except} is not inside {cidr}"))
                    }
                })
                .collect::<Result<_, String>>()?;
            Peer::Block { cidr, except }
        }
        Some(rule::Peer::Group(group)) => {
            if !valid_id(group) {
                return Err(format!("{group:?} is not a security group ID"));
            }
            Peer::Group(group.clone())
        }
        None => return Err("it has no peer".to_string()),
    };
    Ok(Rule {
        direction,
        protocol,
        ports,
        peer,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::{AddressBlock, PortRange};

    fn block(cidr: &str, except: &[&str]) -> Option<rule::Peer> {
        Some(rule::Peer::Block(AddressBlock {
            cidr: cidr.to_string(),
            except: except.iter().map(|e| e.to_string()).collect(),
        }))
    }

    fn rule(
        protocol: proto::Protocol,
        ports: &[(u32, u32)],
        peer: Option<rule::Peer>,
    ) -> proto::Rule {
        proto::Rule {
            direction: proto::Direction::Ingress as i32,
            protocol: protocol as i32,
            ports: ports
                .iter()
                .map(|&(first, last)| PortRange { first, last })
                .collect(),
            peer,
        }
    }

    fn group(rules: Vec<proto::Rule>, members: &[&str]) -> SecurityGroup {
        SecurityGroup {
            id: "web".to_string(),
            version: 1,
            rules,
            members: members.iter().map(|m| m.to_string()).collect(),
        }
    }

    #[test]
    fn ids_are_short_names() {
        // A sandbox's group is named by its pod's ID, a short one
        // (ADR 0019) or a UUID.
        for id in [
            "web",
            "a",
            "db-1.prod_eu",
            &"x".repeat(64),
            "sbx-k7m2q9x4c1b",
            "sbx-8d99e180-1694-4d94-a42e-097769b89510",
        ] {
            assert!(check_id(id).is_ok(), "{id}");
        }
        for id in ["", "a/b", "a b", "grüne", &"x".repeat(65)] {
            assert!(check_id(id).is_err(), "{id}");
        }
    }

    #[test]
    fn a_rule_parses() {
        let parsed = parse_rule(&rule(
            proto::Protocol::Tcp,
            &[(80, 0), (8000, 8100)],
            block("0.0.0.0/0", &["10.0.0.0/8"]),
        ))
        .unwrap();
        assert_eq!(parsed.direction, Direction::Ingress);
        assert_eq!(parsed.protocol, Protocol::Tcp);
        assert_eq!(parsed.ports, vec![(80, 80), (8000, 8100)]);
        assert_eq!(
            parsed.peer,
            Peer::Block {
                cidr: "0.0.0.0/0".parse().unwrap(),
                except: vec!["10.0.0.0/8".parse().unwrap()],
            }
        );
        let peer = Some(rule::Peer::Group("db".to_string()));
        let parsed = parse_rule(&rule(proto::Protocol::Any, &[], peer)).unwrap();
        assert_eq!(parsed.peer, Peer::Group("db".to_string()));
        assert_eq!(parsed.protocol.number(), None);
        assert_eq!(Protocol::Icmpv6.number(), Some(58));
    }

    #[test]
    fn bad_rules_are_refused() {
        let tcp = proto::Protocol::Tcp;
        let everyone = || block("::/0", &[]);
        for (rule, why) in [
            (rule(tcp, &[(0, 0)], everyone()), "port 0"),
            (rule(tcp, &[(0, 80)], everyone()), "port 0 first"),
            (rule(tcp, &[(65536, 0)], everyone()), "port 65536"),
            (rule(tcp, &[(80, 70000)], everyone()), "last port 70000"),
            (rule(tcp, &[(443, 80)], everyone()), "first after last"),
            (
                rule(proto::Protocol::Any, &[(80, 0)], everyone()),
                "ports of any protocol",
            ),
            (
                rule(proto::Protocol::Icmpv6, &[(80, 0)], everyone()),
                "ports of ICMPv6",
            ),
            (rule(tcp, &[], None), "no peer"),
            (
                rule(tcp, &[], block("10.0.0.0/8", &["11.0.0.0/16"])),
                "except outside",
            ),
            (
                rule(tcp, &[], block("10.0.0.0/16", &["10.0.0.0/8"])),
                "except larger",
            ),
            (
                rule(tcp, &[], block("::/0", &["10.0.0.0/8"])),
                "except of the other family",
            ),
            (
                rule(tcp, &[], block("10.0.0.0/8", &["10.1.0.0"])),
                "except no prefix",
            ),
            (rule(tcp, &[], block("10.1.0.0/8", &[])), "host bits"),
            (rule(tcp, &[], block("", &[])), "no cidr"),
            (
                rule(tcp, &[], Some(rule::Peer::Group("a/b".to_string()))),
                "group ID",
            ),
        ] {
            assert!(parse_rule(&rule).is_err(), "{why}");
        }
        let mut no_direction = rule(tcp, &[], everyone());
        no_direction.direction = proto::Direction::Unspecified as i32;
        assert!(parse_rule(&no_direction).is_err());
        no_direction.direction = 7;
        assert!(parse_rule(&no_direction).is_err());
        let mut unknown = rule(tcp, &[], everyone());
        unknown.protocol = 9;
        assert!(parse_rule(&unknown).is_err());
        assert!(parse_rule(&rule(
            tcp,
            &[(65535, 0), (1, 65535)],
            block("10.0.0.0/8", &["10.0.0.0/8"])
        ))
        .is_ok());
    }

    #[test]
    fn members_are_kept_canonical_and_sorted() {
        let normal = normalize(group(vec![], &["FD00::5", "fd00::1", "fd00:0::5"])).unwrap();
        assert_eq!(normal.members, vec!["fd00::1", "fd00::5"]);
        assert!(normalize(group(vec![], &["10.0.0.1"])).is_err());
        assert!(normalize(group(vec![], &["fd00::/64"])).is_err());
    }

    #[test]
    fn a_bad_rule_names_its_place() {
        let rules = vec![
            rule(proto::Protocol::Tcp, &[], block("::/0", &[])),
            rule(proto::Protocol::Tcp, &[], None),
        ];
        let error = normalize(group(rules, &[])).unwrap_err();
        assert_eq!(error.code(), tonic::Code::InvalidArgument);
        assert!(error.message().contains("rule 1"), "{}", error.message());
    }
}
