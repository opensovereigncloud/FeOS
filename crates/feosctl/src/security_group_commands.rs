// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A node's security groups (ADR 0012). The control plane sends them; these
//! commands are for development and for looking at what a node has.

use anyhow::Result;
use clap::{Args, Subcommand};
use feos_proto::network_service::{
    rule, security_group_service_client::SecurityGroupServiceClient, AddressBlock,
    DeleteSecurityGroupRequest, Direction, GetSecurityGroupRequest, ListSecurityGroupsRequest,
    PortRange, Protocol, PutSecurityGroupRequest, Rule, SecurityGroup, UpdateSecurityGroupRequest,
};

const RULE_HELP: &str = "\
A rule allows one kind of traffic, in or out:

  ingress <protocol> [<ports>] from <peer>
  egress <protocol> [<ports>] to <peer>

  protocol  tcp, udp, icmp, icmpv6 or any
  ports     TCP and UDP only, e.g. 443 or 80,443,8000-8100; every port without
  peer      group <id>: the members of a security group; or
            <prefix> [except <prefix>,...]: an address block, IPv4 ones
            under the NAT64 prefix

Examples:

  --allow 'ingress tcp 80,443 from ::/0'
  --allow 'egress any to group db'
  --allow 'egress tcp 443 to 0.0.0.0/0 except 10.0.0.0/8,192.168.0.0/16'";

#[derive(Args, Debug)]
pub struct SecurityGroupArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    #[command(subcommand)]
    command: SecurityGroupCommand,
}

#[derive(Subcommand, Debug)]
pub enum SecurityGroupCommand {
    /// Put a group whole, replacing the one the node has
    #[command(after_help = RULE_HELP)]
    Put {
        id: String,
        #[arg(long, default_value_t = 1)]
        version: u64,
        #[arg(
            long = "allow",
            value_name = "RULE",
            value_parser = parse_rule,
            help = "A rule, see below (repeatable)"
        )]
        rules: Vec<Rule>,
        #[arg(
            long = "member",
            value_name = "ADDRESS",
            help = "The IPv6 address of a pod in the group (repeatable)"
        )]
        members: Vec<String>,
    },
    /// Add and remove members of a group, from one version to the next
    Update {
        id: String,
        #[arg(long, help = "The version the node must have")]
        from_version: u64,
        #[arg(
            long,
            help = "The version after the change; the next one when not given"
        )]
        version: Option<u64>,
        #[arg(long, value_name = "ADDRESS", help = "A member to add (repeatable)")]
        add: Vec<String>,
        #[arg(long, value_name = "ADDRESS", help = "A member to remove (repeatable)")]
        remove: Vec<String>,
    },
    /// Remove a group that no pod on the node names
    Delete { id: String },
    /// Show a group: its rules and members
    Get { id: String },
    /// List the groups
    List,
}

pub async fn handle_security_group_command(args: SecurityGroupArgs) -> Result<()> {
    let mut client =
        SecurityGroupServiceClient::new(crate::transport::channel(&args.address).await?);

    match args.command {
        SecurityGroupCommand::Put {
            id,
            version,
            rules,
            members,
        } => {
            let group = SecurityGroup {
                id: id.clone(),
                version,
                rules,
                members,
            };
            client
                .put_security_group(PutSecurityGroupRequest { group: Some(group) })
                .await?;
            println!("Security group {id} is at version {version}");
        }
        SecurityGroupCommand::Update {
            id,
            from_version,
            version,
            add,
            remove,
        } => {
            let version = version.unwrap_or(from_version.saturating_add(1));
            client
                .update_security_group(UpdateSecurityGroupRequest {
                    id: id.clone(),
                    from_version,
                    version,
                    add_members: add,
                    remove_members: remove,
                })
                .await?;
            println!("Security group {id} is at version {version}");
        }
        SecurityGroupCommand::Delete { id } => {
            client
                .delete_security_group(DeleteSecurityGroupRequest { id: id.clone() })
                .await?;
            println!("Security group {id} is deleted");
        }
        SecurityGroupCommand::Get { id } => {
            let group = client
                .get_security_group(GetSecurityGroupRequest { id })
                .await?
                .into_inner();
            print_group(&group);
        }
        SecurityGroupCommand::List => {
            let groups = client
                .list_security_groups(ListSecurityGroupsRequest {})
                .await?
                .into_inner()
                .groups;
            if groups.is_empty() {
                println!("No security groups");
                return Ok(());
            }
            println!("{:<32} {:<10} {:<6} MEMBERS", "ID", "VERSION", "RULES");
            println!("{:-<32} {:-<10} {:-<6} {:-<7}", "", "", "", "");
            for group in groups {
                println!(
                    "{:<32} {:<10} {:<6} {}",
                    group.id,
                    group.version,
                    group.rules.len(),
                    group.members.len()
                );
            }
        }
    }
    Ok(())
}

fn print_group(group: &SecurityGroup) {
    println!("Security group {}", group.id);
    println!("  Version: {}", group.version);
    println!("  Rules:");
    for rule in &group.rules {
        println!("    {}", format_rule(rule));
    }
    println!("  Members:");
    for member in &group.members {
        println!("    {member}");
    }
}

/// A rule in the syntax of RULE_HELP.
fn parse_rule(s: &str) -> Result<Rule, String> {
    let mut words = s.split_whitespace().peekable();
    let (direction, name, preposition) = match words.next() {
        Some("ingress") => (Direction::Ingress, "ingress", "from"),
        Some("egress") => (Direction::Egress, "egress", "to"),
        _ => return Err(format!("{s:?} starts with neither ingress nor egress")),
    };
    let protocol = match words.next() {
        Some("tcp") => Protocol::Tcp,
        Some("udp") => Protocol::Udp,
        Some("icmpv6") => Protocol::Icmpv6,
        Some("icmp") => Protocol::Icmp,
        Some("any") => Protocol::Any,
        other => {
            return Err(format!(
                "{} is not a protocol: tcp, udp, icmp, icmpv6 or any",
                other.unwrap_or("nothing")
            ))
        }
    };
    let mut ports = Vec::new();
    if let Some(&word) = words.peek()
        && word != preposition
    {
        if !matches!(protocol, Protocol::Tcp | Protocol::Udp) {
            return Err("only tcp and udp have ports".to_string());
        }
        ports = parse_ports(word)?;
        words.next();
    }
    if words.next() != Some(preposition) {
        return Err(format!(
            "{name} rules name their peer after \"{preposition}\""
        ));
    }
    let peer = match words.next() {
        Some("group") => {
            let id = words.next().ok_or("\"group\" needs a group ID")?;
            rule::Peer::Group(id.to_string())
        }
        Some(cidr) => {
            let mut except = Vec::new();
            if let Some(word) = words.next() {
                if word != "except" {
                    return Err(format!("{word:?} follows the prefix; \"except\" may"));
                }
                except = words
                    .by_ref()
                    .flat_map(|word| word.split(','))
                    .filter(|prefix| !prefix.is_empty())
                    .map(str::to_string)
                    .collect();
                if except.is_empty() {
                    return Err("\"except\" needs prefixes".to_string());
                }
            }
            rule::Peer::Block(AddressBlock {
                cidr: cidr.to_string(),
                except,
            })
        }
        None => return Err("the rule has no peer".to_string()),
    };
    if let Some(word) = words.next() {
        return Err(format!("{word:?} follows the peer"));
    }
    Ok(Rule {
        direction: direction as i32,
        protocol: protocol as i32,
        ports,
        peer: Some(peer),
    })
}

/// Ports: `443`, `8000-8100`, or several of them with commas.
fn parse_ports(s: &str) -> Result<Vec<PortRange>, String> {
    s.split(',')
        .map(|range| {
            let port = |port: &str| {
                port.parse::<u16>()
                    .ok()
                    .filter(|&port| port > 0)
                    .ok_or_else(|| format!("{port:?} is not a port"))
            };
            let (first, last) = match range.split_once('-') {
                Some((first, last)) => (port(first)?, port(last)?),
                None => (port(range)?, 0),
            };
            if last != 0 && last < first {
                return Err(format!("{range} ends before it starts"));
            }
            Ok(PortRange {
                first: first.into(),
                last: last.into(),
            })
        })
        .collect()
}

/// A rule in the syntax of RULE_HELP, as the node has it.
fn format_rule(rule: &Rule) -> String {
    let (direction, preposition) = match rule.direction() {
        Direction::Ingress => ("ingress", "from"),
        Direction::Egress => ("egress", "to"),
        Direction::Unspecified => ("unspecified", "with"),
    };
    let protocol = match rule.protocol() {
        Protocol::Any => "any",
        Protocol::Tcp => "tcp",
        Protocol::Udp => "udp",
        Protocol::Icmpv6 => "icmpv6",
        Protocol::Icmp => "icmp",
    };
    let mut words = vec![direction.to_string(), protocol.to_string()];
    if !rule.ports.is_empty() {
        let ports: Vec<String> = rule
            .ports
            .iter()
            .map(|range| match range.last {
                0 => range.first.to_string(),
                last if last == range.first => last.to_string(),
                last => format!("{}-{last}", range.first),
            })
            .collect();
        words.push(ports.join(","));
    }
    words.push(preposition.to_string());
    match &rule.peer {
        Some(rule::Peer::Group(id)) => words.push(format!("group {id}")),
        Some(rule::Peer::Block(block)) => {
            words.push(block.cidr.clone());
            if !block.except.is_empty() {
                words.push(format!("except {}", block.except.join(",")));
            }
        }
        None => words.push("nobody".to_string()),
    }
    words.join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::Parser;

    #[derive(Parser)]
    struct Cli {
        #[command(subcommand)]
        command: SecurityGroupCommand,
    }

    #[test]
    fn rules_parse_and_print_alike() {
        for rule in [
            "ingress tcp 80,443 from ::/0",
            "egress any to group db",
            "egress tcp 443 to 0.0.0.0/0 except 10.0.0.0/8,192.168.0.0/16",
            "ingress udp 53,8000-8100 from fd00::/8",
            "ingress icmpv6 from group web.eu-1",
        ] {
            assert_eq!(format_rule(&parse_rule(rule).unwrap()), rule);
        }
        let rule =
            parse_rule("  egress  udp 53 to 0.0.0.0/0 except 10.0.0.0/8, 172.16.0.0/12 ").unwrap();
        assert_eq!(rule.direction(), Direction::Egress);
        assert_eq!(rule.protocol(), Protocol::Udp);
        assert_eq!(rule.ports, vec![PortRange { first: 53, last: 0 }]);
        let Some(rule::Peer::Block(block)) = rule.peer else {
            panic!("not a block");
        };
        assert_eq!(block.cidr, "0.0.0.0/0");
        assert_eq!(block.except, ["10.0.0.0/8", "172.16.0.0/12"]);
    }

    #[test]
    fn bad_rules_are_refused() {
        for rule in [
            "",
            "inbound tcp from ::/0",
            "ingress sctp from ::/0",
            "ingress tcp 80 to ::/0",
            "egress tcp 80 from ::/0",
            "ingress any 80 from ::/0",
            "ingress icmpv6 1 from ::/0",
            "ingress tcp 0 from ::/0",
            "ingress tcp 65536 from ::/0",
            "ingress tcp 90-80 from ::/0",
            "ingress tcp 80- from ::/0",
            "ingress tcp 80,,443 from ::/0",
            "ingress tcp http from ::/0",
            "ingress tcp 80",
            "ingress tcp 80 from",
            "ingress tcp from group",
            "ingress tcp from group a b",
            "ingress tcp from ::/0 except",
            "ingress tcp from ::/0 but fd00::/8",
        ] {
            assert!(parse_rule(rule).is_err(), "{rule:?} parsed");
        }
    }

    #[test]
    fn a_put_takes_rules_and_members() {
        let cli = Cli::try_parse_from([
            "security-group",
            "put",
            "web",
            "--version",
            "3",
            "--allow",
            "ingress tcp 80,443 from ::/0",
            "--allow",
            "egress any to group db",
            "--member",
            "fd00:fe05::5",
        ])
        .unwrap();
        let SecurityGroupCommand::Put {
            id,
            version,
            rules,
            members,
        } = cli.command
        else {
            panic!("not a put");
        };
        assert_eq!((id.as_str(), version), ("web", 3));
        assert_eq!(rules.len(), 2);
        assert_eq!(rules[1].peer, Some(rule::Peer::Group("db".to_string())));
        assert_eq!(members, ["fd00:fe05::5"]);
        assert!(
            Cli::try_parse_from(["security-group", "put", "web", "--allow", "out tcp"]).is_err()
        );
    }

    #[test]
    fn an_update_goes_to_the_next_version() {
        let cli = Cli::try_parse_from([
            "security-group",
            "update",
            "web",
            "--from-version",
            "3",
            "--add",
            "fd00::1",
            "--remove",
            "fd00::2",
        ])
        .unwrap();
        let SecurityGroupCommand::Update {
            from_version,
            version,
            add,
            remove,
            ..
        } = cli.command
        else {
            panic!("not an update");
        };
        assert_eq!((from_version, version), (3, None));
        assert_eq!(add, ["fd00::1"]);
        assert_eq!(remove, ["fd00::2"]);
        assert!(Cli::try_parse_from(["security-group", "update", "web"]).is_err());
    }
}
