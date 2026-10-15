// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The translator's device and its routes, over netlink, and the bpf
//! filesystem for its maps.

use crate::{Config, DEVICE};
use futures::TryStreamExt;
use log::{debug, info};
use nix::mount::{mount, MsFlags};
use nix::sys::statfs::{statfs, BPF_FS_MAGIC};
use rtnetlink::packet_route::link::{InfoKind, LinkAttribute, LinkInfo, LinkMessage};
use rtnetlink::packet_route::route::{
    RouteAddress, RouteAttribute, RouteHeader, RouteMessage, RouteProtocol,
};
use rtnetlink::{Handle, LinkDummy, LinkUnspec, RouteMessageBuilder};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::path::Path;

/// The next hop of the prefix and the pool on the device. A next hop rather
/// than on-link routes keeps the node at one neighbour entry, not one per
/// destination; on a device without ARP, a neighbour's address is the
/// device's own.
const NEXT_HOP: Ipv6Addr = Ipv6Addr::new(0xfe80, 0, 0, 0, 0, 0, 0, 1);

const BPFFS: &str = "/sys/fs/bpf";

pub(crate) fn connect() -> io::Result<Handle> {
    let (connection, handle, _) = rtnetlink::new_connection()?;
    tokio::spawn(connection);
    Ok(handle)
}

fn netlink_error(what: &str) -> impl Fn(rtnetlink::Error) -> io::Error + '_ {
    move |e| io::Error::other(format!("NAT64: {what}: {e}"))
}

async fn link(handle: &Handle, name: &str) -> io::Result<Option<LinkMessage>> {
    let mut links = handle.link().get().match_name(name.to_string()).execute();
    match links.try_next().await {
        Ok(link) => Ok(link),
        Err(rtnetlink::Error::NetlinkError(e)) if e.raw_code() == -libc::ENODEV => Ok(None),
        Err(e) => Err(io::Error::other(format!("NAT64: link {name}: {e}"))),
    }
}

fn is_dummy(link: &LinkMessage) -> bool {
    link.attributes.iter().any(|attribute| match attribute {
        LinkAttribute::LinkInfo(infos) => infos.contains(&LinkInfo::Kind(InfoKind::Dummy)),
        _ => false,
    })
}

/// Creates the device, a dummy one without ARP, if it is not there, and
/// brings it up. Returns its index.
pub(crate) async fn ensure_device(handle: &Handle) -> io::Result<u32> {
    let device = match link(handle, DEVICE).await? {
        Some(device) => device,
        None => {
            handle
                .link()
                .add(LinkDummy::new(DEVICE).build())
                .execute()
                .await
                .map_err(netlink_error("create the device"))?;
            info!("NAT64: created {DEVICE}");
            link(handle, DEVICE)
                .await?
                .ok_or_else(|| io::Error::other(format!("NAT64: {DEVICE} is gone")))?
        }
    };
    if !is_dummy(&device) {
        return Err(io::Error::other(format!(
            "NAT64: {DEVICE} is there, but not a dummy device"
        )));
    }
    let index = device.header.index;
    handle
        .link()
        .set(LinkUnspec::new_with_index(index).up().build())
        .execute()
        .await
        .map_err(netlink_error("bring the device up"))?;
    Ok(index)
}

pub(crate) async fn delete_device(handle: &Handle) -> io::Result<()> {
    if let Some(device) = link(handle, DEVICE).await? {
        handle
            .link()
            .del(device.header.index)
            .execute()
            .await
            .map_err(netlink_error("delete the device"))?;
    }
    Ok(())
}

/// Routes the prefix and the pool into the device, and removes routes of
/// another prefix or pool that an earlier feosd left there.
pub(crate) async fn set_routes(handle: &Handle, index: u32, config: &Config) -> io::Result<()> {
    let prefix = RouteMessageBuilder::<Ipv6Addr>::new()
        .destination_prefix(config.prefix, 96)
        .output_interface(index)
        .gateway(NEXT_HOP)
        .build();
    handle
        .route()
        .add(prefix)
        .replace()
        .execute()
        .await
        .map_err(netlink_error("route the prefix"))?;
    // The same next hop (RFC 5549), which needs no IPv4 address anywhere.
    let pool = RouteMessageBuilder::<Ipv4Addr>::new()
        .destination_prefix(config.pool, config.pool_len)
        .output_interface(index)
        .via(NEXT_HOP)
        .build();
    handle
        .route()
        .add(pool)
        .replace()
        .execute()
        .await
        .map_err(netlink_error("route the pool"))?;

    let wanted = [
        (IpAddr::V6(config.prefix), 96),
        (IpAddr::V4(config.pool), config.pool_len),
    ];
    let v4 = handle
        .route()
        .get(RouteMessageBuilder::<Ipv4Addr>::new().build())
        .execute();
    let v6 = handle
        .route()
        .get(RouteMessageBuilder::<Ipv6Addr>::new().build())
        .execute();
    let mut routes: Vec<RouteMessage> = v4
        .try_collect()
        .await
        .map_err(netlink_error("list the routes"))?;
    routes.extend(
        v6.try_collect::<Vec<_>>()
            .await
            .map_err(netlink_error("list the routes"))?,
    );
    for route in routes {
        if route.header.table != RouteHeader::RT_TABLE_MAIN
            || route.header.protocol != RouteProtocol::Static
            || !route.attributes.contains(&RouteAttribute::Oif(index))
        {
            continue;
        }
        let destination = route
            .attributes
            .iter()
            .find_map(|attribute| match attribute {
                RouteAttribute::Destination(RouteAddress::Inet(a)) => Some(IpAddr::V4(*a)),
                RouteAttribute::Destination(RouteAddress::Inet6(a)) => Some(IpAddr::V6(*a)),
                _ => None,
            });
        let Some(destination) = destination else {
            continue;
        };
        if wanted.contains(&(destination, route.header.destination_prefix_length)) {
            continue;
        }
        debug!(
            "NAT64: removing the route {destination}/{}",
            route.header.destination_prefix_length
        );
        handle
            .route()
            .del(route)
            .execute()
            .await
            .map_err(netlink_error("remove an earlier route"))?;
    }
    Ok(())
}

/// Mounts the bpf filesystem at /sys/fs/bpf if the pins go there and it is
/// not mounted (under feos-host-init), and creates the pins' directory.
pub(crate) fn prepare_bpffs(pin_dir: &Path) -> io::Result<()> {
    let root = Path::new(BPFFS);
    if pin_dir.starts_with(root) && !is_bpffs(root)? {
        mount(
            Some("bpf"),
            root,
            Some("bpf"),
            MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC,
            None::<&str>,
        )
        .map_err(|e| io::Error::other(format!("NAT64: mount {BPFFS}: {e}")))?;
        info!("NAT64: mounted {BPFFS}");
    }
    std::fs::create_dir_all(pin_dir)?;
    if !is_bpffs(pin_dir)? {
        return Err(io::Error::other(format!(
            "NAT64: {} is not on a bpf filesystem",
            pin_dir.display()
        )));
    }
    Ok(())
}

fn is_bpffs(path: &Path) -> io::Result<bool> {
    let fs = statfs(path).map_err(io::Error::from)?;
    Ok(fs.filesystem_type() == BPF_FS_MAGIC)
}
