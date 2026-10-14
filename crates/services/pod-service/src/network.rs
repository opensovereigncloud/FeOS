// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' network (ADR 0010): one bridge with the pods' gateway, a TAP
//! device per pod as an isolated port of it, NAT to the outside, and
//! nftables rules that keep the pods away from each other, from the node
//! and from private networks, and pin each pod's addresses to its TAP.

use feos_utils::nft::{self, matches::*, Chain, CmpOp, Expr, Family, Hook, Table};
use futures::TryStreamExt;
use log::info;
use microvm_service::vmm::jail::tap;
use rtnetlink::packet_route::link::{
    InfoBridgePort, InfoPortData, InfoPortKind, LinkAttribute, LinkMessage,
};
use rtnetlink::{Handle, LinkBridge, LinkUnspec};
use std::collections::HashSet;
use std::io;
use std::net::Ipv4Addr;
use std::path::{Path, PathBuf};

/// The bridge of the pods' TAP devices, with their gateway.
pub const BRIDGE: &str = "feos-pods";
/// The node's end of the pods' network.
pub const GATEWAY: Ipv4Addr = Ipv4Addr::new(10, 200, 0, 1);
pub const PREFIX_LEN: u8 = 16;
/// The resolvers the guests get.
pub const DNS: [&str; 2] = ["8.8.8.8", "1.1.1.1"];
/// feosd's nftables tables, in the inet and the bridge family.
const TABLE: &str = "feos";

/// What pods may not reach: private, CGNAT, link-local, loopback, "this
/// network", multicast and reserved addresses. The pods' own subnet is in
/// 10.0.0.0/8: a pod reaches no other through the gateway either.
const DENY: [(Ipv4Addr, u8); 9] = [
    (Ipv4Addr::new(10, 0, 0, 0), 8),
    (Ipv4Addr::new(172, 16, 0, 0), 12),
    (Ipv4Addr::new(192, 168, 0, 0), 16),
    (Ipv4Addr::new(100, 64, 0, 0), 10),
    (Ipv4Addr::new(169, 254, 0, 0), 16),
    (Ipv4Addr::new(127, 0, 0, 0), 8),
    (Ipv4Addr::new(0, 0, 0, 0), 8),
    (Ipv4Addr::new(224, 0, 0, 0), 4),
    (Ipv4Addr::new(240, 0, 0, 0), 4),
];

const ETHERTYPE_IPV4: u16 = 0x0800;
const ETHERTYPE_ARP: u16 = 0x0806;

/// A pod on the bridge.
#[derive(Clone, Debug, PartialEq)]
pub struct Attachment {
    pub tap: String,
    pub address: Ipv4Addr,
}

impl Attachment {
    pub fn new(pod_id: &str, address: Ipv4Addr) -> Self {
        Self {
            tap: tap_name(pod_id),
            address,
        }
    }

    pub fn mac(&self) -> [u8; 6] {
        mac(self.address)
    }
}

/// The pod's TAP device: `pod-` and the first 8 hex digits of its ID.
pub fn tap_name(pod_id: &str) -> String {
    let hex: String = pod_id.chars().filter(|c| *c != '-').take(8).collect();
    format!("pod-{hex}")
}

/// The guest's MAC address: locally administered, with the address's last
/// three bytes, so that it stays the same for the pod's life.
pub fn mac(address: Ipv4Addr) -> [u8; 6] {
    let [_, b, c, d] = address.octets();
    [0x06, 0x00, 0xac, b, c, d]
}

pub fn mac_string(mac: [u8; 6]) -> String {
    mac.iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<Vec<_>>()
        .join(":")
}

/// Hands out the subnet's addresses in turn, from a cursor kept in a file:
/// an address a deleted pod freed comes round again only after all others.
pub struct Addresses {
    cursor: PathBuf,
}

impl Addresses {
    pub fn new(dir: &Path) -> Self {
        Self {
            cursor: dir.join("next-address"),
        }
    }

    /// The next address no pod in `used` holds.
    pub fn allocate(&self, used: &HashSet<Ipv4Addr>) -> io::Result<Ipv4Addr> {
        let base = u32::from(GATEWAY) & !(u32::MAX >> PREFIX_LEN);
        // .0 is the network, .1 the gateway, the last the broadcast.
        let (first, last) = (2u32, (1u32 << (32 - PREFIX_LEN)) - 2);
        let start = std::fs::read_to_string(&self.cursor)
            .ok()
            .and_then(|cursor| cursor.trim().parse::<u32>().ok())
            .filter(|cursor| (first..=last).contains(cursor))
            .unwrap_or(first);
        let span = last - first + 1;
        for step in 0..span {
            let offset = first + (start - first + step) % span;
            let address = Ipv4Addr::from(base + offset);
            if !used.contains(&address) {
                let next = if offset == last { first } else { offset + 1 };
                std::fs::write(&self.cursor, next.to_string())?;
                return Ok(address);
            }
        }
        Err(io::Error::other("the pods' subnet is full"))
    }
}

async fn connect() -> io::Result<Handle> {
    let (connection, handle, _) = rtnetlink::new_connection()?;
    tokio::spawn(connection);
    Ok(handle)
}

async fn link(handle: &Handle, name: &str) -> io::Result<Option<LinkMessage>> {
    let mut links = handle.link().get().match_name(name.to_string()).execute();
    match links.try_next().await {
        Ok(link) => Ok(link),
        Err(rtnetlink::Error::NetlinkError(e)) if e.raw_code() == -libc::ENODEV => Ok(None),
        Err(e) => Err(io::Error::other(format!("link {name}: {e}"))),
    }
}

fn netlink_error(what: &str) -> impl Fn(rtnetlink::Error) -> io::Error + '_ {
    move |e| io::Error::other(format!("{what}: {e}"))
}

/// Creates the bridge with the gateway's address, if it is not there, and
/// lets the node forward.
pub async fn set_up_bridge() -> io::Result<()> {
    let handle = connect().await?;
    let bridge = match link(&handle, BRIDGE).await? {
        Some(bridge) => bridge,
        None => {
            handle
                .link()
                .add(LinkBridge::new(BRIDGE).build())
                .execute()
                .await
                .map_err(netlink_error("create the pods' bridge"))?;
            info!("Pods: Created the bridge {BRIDGE}");
            link(&handle, BRIDGE)
                .await?
                .ok_or_else(|| io::Error::other("the bridge is gone"))?
        }
    };
    let index = bridge.header.index;
    match handle
        .address()
        .add(index, GATEWAY.into(), PREFIX_LEN)
        .execute()
        .await
    {
        Err(rtnetlink::Error::NetlinkError(e)) if e.raw_code() == -libc::EEXIST => {}
        result => result.map_err(netlink_error("address the pods' bridge"))?,
    }
    handle
        .link()
        .set(LinkUnspec::new_with_index(index).up().build())
        .execute()
        .await
        .map_err(netlink_error("bring the pods' bridge up"))?;
    std::fs::write("/proc/sys/net/ipv4/ip_forward", "1\n")
}

/// Makes the pod's TAP device an isolated port of the bridge, isolated
/// before it goes up: it never forwards a frame to another pod.
pub async fn attach(attachment: &Attachment) -> io::Result<()> {
    tap::ensure(&attachment.tap, 0)?;
    let handle = connect().await?;
    let bridge = link(&handle, BRIDGE)
        .await?
        .ok_or_else(|| io::Error::other(format!("no bridge {BRIDGE}")))?;
    let device = link(&handle, &attachment.tap)
        .await?
        .ok_or_else(|| io::Error::other(format!("no TAP device {}", attachment.tap)))?;
    let index = device.header.index;
    let enslaved = device
        .attributes
        .contains(&LinkAttribute::Controller(bridge.header.index));
    if !enslaved {
        handle
            .link()
            .set(
                LinkUnspec::new_with_index(index)
                    .controller(bridge.header.index)
                    .build(),
            )
            .execute()
            .await
            .map_err(netlink_error("put the TAP device on the bridge"))?;
    }
    // A port's flags go in RTM_NEWLINK: RTM_SETLINK ignores them.
    handle
        .link()
        .set_port(
            LinkUnspec::new_with_index(index)
                .set_port_kind(InfoPortKind::Bridge)
                .set_port_data(InfoPortData::BridgePort(vec![InfoBridgePort::Isolated(
                    true,
                )]))
                .build(),
        )
        .execute()
        .await
        .map_err(netlink_error("isolate the TAP device's port"))?;
    handle
        .link()
        .set(LinkUnspec::new_with_index(index).up().build())
        .execute()
        .await
        .map_err(netlink_error("bring the TAP device up"))
}

/// Deletes the pod's TAP device.
pub fn detach(attachment: &Attachment) -> io::Result<()> {
    tap::delete(&attachment.tap)
}

/// Replaces feosd's rules with the ones for these pods.
pub fn apply(pods: &[Attachment]) -> io::Result<()> {
    nft::replace(&inet_table())?;
    nft::replace(&bridge_table(pods))
}

/// NAT out, nothing to the node, nothing to private networks.
fn inet_table() -> Table {
    let from_pods = || iifname(CmpOp::Eq, BRIDGE);
    let mut forward = Vec::new();
    for (address, len) in DENY {
        forward.push(
            [
                from_pods(),
                ipv4(),
                ipv4_daddr_in(address, len),
                vec![Expr::Drop],
            ]
            .concat(),
        );
    }
    Table {
        family: Family::Inet,
        name: TABLE.to_string(),
        chains: vec![
            Chain {
                name: "input".to_string(),
                kind: "filter",
                hook: Hook::Input,
                priority: 0,
                rules: vec![[from_pods(), vec![Expr::Drop]].concat()],
            },
            Chain {
                name: "forward".to_string(),
                kind: "filter",
                hook: Hook::Forward,
                priority: 0,
                rules: forward,
            },
            Chain {
                name: "postrouting".to_string(),
                kind: "nat",
                hook: Hook::Postrouting,
                priority: 100,
                rules: vec![[
                    ipv4(),
                    ipv4_prefix(12, CmpOp::Eq, GATEWAY, PREFIX_LEN),
                    oifname(CmpOp::Neq, BRIDGE),
                    vec![Expr::Masquerade],
                ]
                .concat()],
            },
        ],
    }
}

/// Each pod's TAP passes only the pod's MAC, its IPv4 source address, and
/// ARP with both; no other protocol.
fn bridge_table(pods: &[Attachment]) -> Table {
    let mut rules = Vec::new();
    for pod in pods {
        let port = || iifname(CmpOp::Eq, &pod.tap);
        let mac = pod.mac();
        rules.push([port(), ether_saddr(CmpOp::Neq, mac), vec![Expr::Drop]].concat());
        rules.push(
            [
                port(),
                ether_type(CmpOp::Eq, ETHERTYPE_IPV4),
                ipv4_saddr(CmpOp::Neq, pod.address),
                vec![Expr::Drop],
            ]
            .concat(),
        );
        rules.push(
            [
                port(),
                ether_type(CmpOp::Eq, ETHERTYPE_ARP),
                arp_sha(CmpOp::Neq, mac),
                vec![Expr::Drop],
            ]
            .concat(),
        );
        rules.push(
            [
                port(),
                ether_type(CmpOp::Eq, ETHERTYPE_ARP),
                arp_spa(CmpOp::Neq, pod.address),
                vec![Expr::Drop],
            ]
            .concat(),
        );
        rules.push(
            [
                port(),
                ether_type(CmpOp::Neq, ETHERTYPE_IPV4),
                ether_type(CmpOp::Neq, ETHERTYPE_ARP),
                vec![Expr::Drop],
            ]
            .concat(),
        );
    }
    Table {
        family: Family::Bridge,
        name: TABLE.to_string(),
        chains: vec![Chain {
            name: "prerouting".to_string(),
            kind: "filter",
            hook: Hook::Prerouting,
            priority: -200,
            rules,
        }],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_and_macs_come_from_the_pod() {
        assert_eq!(
            tap_name("8d99e180-1694-4d94-a42e-097769b89510"),
            "pod-8d99e180"
        );
        assert!(tap_name("8d99e180-1694-4d94-a42e-097769b89510").len() < libc::IFNAMSIZ);
        let mac = mac(Ipv4Addr::new(10, 200, 3, 7));
        assert_eq!(mac_string(mac), "06:00:ac:c8:03:07");
    }

    #[test]
    fn addresses_come_round_in_turn() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Addresses::new(dir.path());
        let mut used = HashSet::new();
        let first = addresses.allocate(&used).unwrap();
        assert_eq!(first, Ipv4Addr::new(10, 200, 0, 2));
        used.insert(first);
        assert_eq!(
            addresses.allocate(&used).unwrap(),
            Ipv4Addr::new(10, 200, 0, 3)
        );
        // .2 is free again, but its turn comes last.
        used.remove(&first);
        assert_eq!(
            addresses.allocate(&used).unwrap(),
            Ipv4Addr::new(10, 200, 0, 4)
        );
        std::fs::write(dir.path().join("next-address"), "65534").unwrap();
        assert_eq!(
            addresses.allocate(&used).unwrap(),
            Ipv4Addr::new(10, 200, 255, 254)
        );
        assert_eq!(
            addresses.allocate(&used).unwrap(),
            Ipv4Addr::new(10, 200, 0, 2)
        );
    }

    #[test]
    fn every_pod_gets_its_pins() {
        let pods = [
            Attachment::new(
                "11111111-2222-3333-4444-555555555555",
                Ipv4Addr::new(10, 200, 0, 2),
            ),
            Attachment::new(
                "22222222-2222-3333-4444-555555555555",
                Ipv4Addr::new(10, 200, 0, 3),
            ),
        ];
        let table = bridge_table(&pods);
        assert_eq!(table.chains[0].rules.len(), 10);
        let inet = inet_table();
        assert_eq!(inet.chains[1].rules.len(), DENY.len());
    }
}
