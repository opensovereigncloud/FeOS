// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's devices and routes for the pods, over netlink, and their
//! sysctls.

use futures::TryStreamExt;
use log::info;
use microvm_service::vmm::jail::tap;
use rtnetlink::packet_route::address::{AddressAttribute, AddressHeaderFlags, AddressScope};
use rtnetlink::packet_route::link::LinkMessage;
use rtnetlink::packet_route::route::RouteType;
use rtnetlink::{Handle, LinkDummy, LinkUnspec, RouteMessageBuilder};
use std::io;
use std::net::{IpAddr, Ipv6Addr};

const IPV6_CONF: &str = "/proc/sys/net/ipv6/conf";

pub async fn connect() -> io::Result<Handle> {
    let (connection, handle, _) = rtnetlink::new_connection()?;
    tokio::spawn(connection);
    Ok(handle)
}

fn netlink_error(what: &str) -> impl Fn(rtnetlink::Error) -> io::Error + '_ {
    move |e| io::Error::other(format!("{what}: {e}"))
}

fn errno(e: &rtnetlink::Error) -> Option<i32> {
    match e {
        rtnetlink::Error::NetlinkError(e) => Some(-e.raw_code()),
        _ => None,
    }
}

async fn link(handle: &Handle, name: &str) -> io::Result<Option<LinkMessage>> {
    let mut links = handle.link().get().match_name(name.to_string()).execute();
    match links.try_next().await {
        Ok(link) => Ok(link),
        Err(e) if errno(&e) == Some(libc::ENODEV) => Ok(None),
        Err(e) => Err(io::Error::other(format!("link {name}: {e}"))),
    }
}

async fn index(handle: &Handle, name: &str) -> io::Result<u32> {
    link(handle, name)
        .await?
        .map(|link| link.header.index)
        .ok_or_else(|| io::Error::other(format!("no device {name}")))
}

fn sysctl(path: &str, value: &str) -> io::Result<()> {
    std::fs::write(path, value).map_err(|e| io::Error::new(e.kind(), format!("{path}: {e}")))
}

/// Turns IPv4 and IPv6 forwarding on, after
/// [`keep_router_advertisements`].
pub fn enable_forwarding() -> io::Result<()> {
    keep_router_advertisements()?;
    sysctl("/proc/sys/net/ipv4/ip_forward", "1")?;
    sysctl(&format!("{IPV6_CONF}/all/forwarding"), "1")
}

/// Lets interfaces that take router advertisements (`accept_ra` 1) go on
/// taking them once IPv6 forwarding is on (`accept_ra` 2): a router
/// ignores them otherwise, and a node that learns its default route from
/// them loses it when it expires. Pods' TAP devices (`pod-*`) and FeOS's
/// own (`feos-*`) are left alone: the node takes no routes from them.
fn keep_router_advertisements() -> io::Result<()> {
    for entry in std::fs::read_dir(IPV6_CONF)? {
        let entry = entry?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if ["all", "default", "lo"].contains(&name.as_ref())
            || name.starts_with("pod-")
            || name.starts_with("feos-")
        {
            continue;
        }
        let path = format!("{IPV6_CONF}/{name}/accept_ra");
        // An interface may be gone by now.
        let Ok(value) = std::fs::read_to_string(&path) else {
            continue;
        };
        if value.trim() == "1" {
            sysctl(&path, "2")?;
            info!("Network: {name} keeps taking router advertisements while forwarding");
        }
    }
    Ok(())
}

/// IPv6 on a device of feosd's own: no router advertisements, no
/// autoconfiguration and no link-local address of the kernel's; feosd
/// adds the addresses. Before the device goes up.
fn own_ipv6(name: &str) -> io::Result<()> {
    for (setting, value) in [
        ("accept_ra", "0"),
        ("autoconf", "0"),
        ("addr_gen_mode", "1"),
    ] {
        sysctl(&format!("/proc/sys/net/ipv6/conf/{name}/{setting}"), value)?;
    }
    Ok(())
}

async fn up(handle: &Handle, index: u32, name: &str) -> io::Result<()> {
    handle
        .link()
        .set(LinkUnspec::new_with_index(index).up().build())
        .execute()
        .await
        .map_err(|e| io::Error::other(format!("bring {name} up: {e}")))
}

/// Adds an IPv6 address without duplicate address detection: the node is
/// its only holder. One that is there already is fine.
async fn add_address(handle: &Handle, index: u32, address: Ipv6Addr, len: u8) -> io::Result<()> {
    let mut add = handle.address().add(index, address.into(), len);
    add.message_mut().header.flags |= AddressHeaderFlags::Nodad;
    match add.execute().await {
        Err(e) if errno(&e) == Some(libc::EEXIST) => Ok(()),
        result => result.map_err(|e| io::Error::other(format!("add {address}/{len}: {e}"))),
    }
}

/// Deletes the device, if it is there. Returns whether it was.
pub async fn delete_link(handle: &Handle, name: &str) -> io::Result<bool> {
    let Some(device) = link(handle, name).await? else {
        return Ok(false);
    };
    handle
        .link()
        .del(device.header.index)
        .execute()
        .await
        .map_err(netlink_error(&format!("delete {name}")))?;
    Ok(true)
}

/// Routes the prefix nowhere: a packet for a pod that is not here goes
/// back as unreachable rather than out into the network again. The pods'
/// own routes are longer.
pub async fn route_unreachable(handle: &Handle, prefix: Ipv6Addr, len: u8) -> io::Result<()> {
    let route = RouteMessageBuilder::<Ipv6Addr>::new()
        .destination_prefix(prefix, len)
        .kind(RouteType::Unreachable)
        .build();
    handle
        .route()
        .add(route)
        .replace()
        .execute()
        .await
        .map_err(netlink_error(&format!(
            "route {prefix}/{len} as unreachable"
        )))
}

/// Makes `name` a dummy device that holds `address` and no other global
/// address.
pub async fn dummy_with_address(handle: &Handle, name: &str, address: Ipv6Addr) -> io::Result<()> {
    if link(handle, name).await?.is_none() {
        handle
            .link()
            .add(LinkDummy::new(name).build())
            .execute()
            .await
            .map_err(netlink_error(&format!("create {name}")))?;
        info!("Network: Created {name}");
    }
    let index = index(handle, name).await?;
    own_ipv6(name)?;
    up(handle, index, name).await?;
    let mut addresses = handle
        .address()
        .get()
        .set_link_index_filter(index)
        .execute();
    let mut stale = Vec::new();
    while let Some(message) = addresses
        .try_next()
        .await
        .map_err(netlink_error("list addresses"))?
    {
        let local = message
            .attributes
            .iter()
            .find_map(|attribute| match attribute {
                AddressAttribute::Address(address) => Some(*address),
                _ => None,
            });
        if message.header.scope == AddressScope::Universe && local != Some(address.into()) {
            stale.push(message);
        }
    }
    for message in stale {
        handle
            .address()
            .del(message)
            .execute()
            .await
            .map_err(netlink_error(&format!("remove an address of {name}")))?;
    }
    add_address(handle, index, address, 128).await
}

/// Makes the pod's TAP device a link of its own between the node and the
/// pod: the node is `gateway` on it, without router advertisements or
/// addresses of the kernel's, and answers no ARP; the pod's address is
/// routed to it.
pub async fn tap(handle: &Handle, name: &str, gateway: Ipv6Addr, pod: Ipv6Addr) -> io::Result<()> {
    // The jail hands a TAP device to the pod's user; one that is there
    // may be in use.
    if link(handle, name).await?.is_none() {
        tap::ensure(name, 0).map_err(|e| io::Error::new(e.kind(), format!("TAP {name}: {e}")))?;
    }
    let index = index(handle, name).await?;
    own_ipv6(name)?;
    sysctl(&format!("/proc/sys/net/ipv4/conf/{name}/arp_ignore"), "8")?;
    up(handle, index, name).await?;
    add_address(handle, index, gateway, 64).await?;
    let route = RouteMessageBuilder::<Ipv6Addr>::new()
        .destination_prefix(pod, 128)
        .output_interface(index)
        .build();
    handle
        .route()
        .add(route)
        .replace()
        .execute()
        .await
        .map_err(netlink_error(&format!("route {pod} to {name}")))
}

/// The node's addresses, of every device but loopback, link-local ones
/// left out.
pub async fn node_addresses(handle: &Handle) -> io::Result<Vec<IpAddr>> {
    let mut messages = handle.address().get().execute();
    let mut addresses = Vec::new();
    while let Some(message) = messages
        .try_next()
        .await
        .map_err(netlink_error("list the node's addresses"))?
    {
        if message.header.scope != AddressScope::Universe {
            continue;
        }
        let local = message
            .attributes
            .iter()
            .find_map(|attribute| match attribute {
                AddressAttribute::Local(address) => Some(*address),
                _ => None,
            })
            .or_else(|| {
                message
                    .attributes
                    .iter()
                    .find_map(|attribute| match attribute {
                        AddressAttribute::Address(address) => Some(*address),
                        _ => None,
                    })
            });
        if let Some(address) = local {
            addresses.push(address);
        }
    }
    addresses.sort();
    addresses.dedup();
    Ok(addresses)
}
