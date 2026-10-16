// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's devices and routes for the pods, over netlink, and their
//! sysctls.

use futures::TryStreamExt;
use log::info;
use microvm_service::vmm::jail::tap;
use rtnetlink::packet_route::address::{
    AddressAttribute, AddressHeaderFlags, AddressMessage, AddressScope,
};
use rtnetlink::packet_route::link::LinkMessage;
use rtnetlink::packet_route::neighbour::NeighbourFlags;
use rtnetlink::packet_route::route::{RouteScope, RouteType};
use rtnetlink::packet_route::AddressFamily;
use rtnetlink::{Handle, LinkDummy, LinkUnspec, RouteMessageBuilder};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

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

/// Adds an address, an IPv6 one without duplicate address detection: the
/// node is its only holder. One that is there already is fine.
async fn add_address(handle: &Handle, index: u32, address: IpAddr, len: u8) -> io::Result<()> {
    let mut add = handle.address().add(index, address, len);
    if address.is_ipv6() {
        add.message_mut().header.flags |= AddressHeaderFlags::Nodad;
    }
    match add.execute().await {
        Err(e) if errno(&e) == Some(libc::EEXIST) => Ok(()),
        result => result.map_err(|e| io::Error::other(format!("add {address}/{len}: {e}"))),
    }
}

/// The addresses of a device.
async fn addresses(handle: &Handle, index: u32) -> io::Result<Vec<AddressMessage>> {
    handle
        .address()
        .get()
        .set_link_index_filter(index)
        .execute()
        .try_collect()
        .await
        .map_err(netlink_error("list addresses"))
}

/// The address of an address message: the local one, on a link that is
/// not point-to-point.
fn address_of(message: &AddressMessage) -> Option<IpAddr> {
    message
        .attributes
        .iter()
        .find_map(|attribute| match attribute {
            AddressAttribute::Address(address) => Some(*address),
            _ => None,
        })
}

async fn remove_addresses(
    handle: &Handle,
    name: &str,
    messages: Vec<AddressMessage>,
) -> io::Result<()> {
    for message in messages {
        handle
            .address()
            .del(message)
            .execute()
            .await
            .map_err(netlink_error(&format!("remove an address of {name}")))?;
    }
    Ok(())
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

/// Makes `name` a dummy device that holds `address`, as a host address,
/// and no other global address.
pub async fn dummy_with_address(handle: &Handle, name: &str, address: IpAddr) -> io::Result<()> {
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
    let len = if address.is_ipv4() { 32 } else { 128 };
    let stale = addresses(handle, index)
        .await?
        .into_iter()
        .filter(|message| {
            message.header.scope == AddressScope::Universe
                && (address_of(message) != Some(address) || message.header.prefix_len != len)
        })
        .collect();
    remove_addresses(handle, name, stale).await?;
    add_address(handle, index, address, len).await
}

/// A pod's link, as [`tap`] makes it.
#[derive(Clone, Copy, Debug)]
pub struct PodLink {
    /// The node's address on it, the pod's IPv6 gateway.
    pub gateway: Ipv6Addr,
    /// The pod's IPv4 gateway, which the node answers ARP for by proxy,
    /// without holding it.
    pub gateway4: Ipv4Addr,
    pub ipv6: Ipv6Addr,
    pub ipv4: Ipv4Addr,
}

/// Makes the pod's TAP device a link of its own between the node and the
/// pod (ADR 0010): without router advertisements or addresses of the
/// kernel's, and without IPv4 addresses; the node is `gateway` on it and
/// answers ARP for `gateway4` by proxy, at once, and for none of its own
/// addresses; the pod's addresses are routed to it.
pub async fn tap(handle: &Handle, name: &str, pod: &PodLink) -> io::Result<()> {
    // The jail hands a TAP device to the pod's user; one that is there
    // may be in use.
    if link(handle, name).await?.is_none() {
        tap::ensure(name, 0).map_err(|e| io::Error::new(e.kind(), format!("TAP {name}: {e}")))?;
    }
    let index = index(handle, name).await?;
    own_ipv6(name)?;
    pod_ipv4(name)?;
    up(handle, index, name).await?;
    let ipv4 = addresses(handle, index)
        .await?
        .into_iter()
        .filter(|message| message.header.family == AddressFamily::Inet)
        .collect();
    remove_addresses(handle, name, ipv4).await?;
    add_address(handle, index, pod.gateway.into(), 64).await?;
    route_to(handle, index, name, pod.ipv6.into()).await?;
    route_to(handle, index, name, pod.ipv4.into()).await?;
    handle
        .neighbours()
        .add(index, pod.gateway4.into())
        .flags(NeighbourFlags::Proxy)
        .replace()
        .execute()
        .await
        .map_err(netlink_error(&format!(
            "answer ARP for {} on {name}",
            pod.gateway4
        )))
}

/// IPv4 on a pod's TAP device, before it goes up: ARP answered only for
/// the addresses on the device, of which it has none, and asked from the
/// best address for the target; forwarding, without which proxy ARP is
/// not answered; and proxy ARP answered at once rather than after a random
/// delay of up to 0.8 s.
fn pod_ipv4(name: &str) -> io::Result<()> {
    for (setting, value) in [
        ("arp_ignore", "1"),
        ("arp_announce", "2"),
        ("forwarding", "1"),
    ] {
        sysctl(&format!("/proc/sys/net/ipv4/conf/{name}/{setting}"), value)?;
    }
    sysctl(&format!("/proc/sys/net/ipv4/neigh/{name}/proxy_delay"), "0")
}

/// Routes an address, as a host route, to the device.
async fn route_to(handle: &Handle, index: u32, name: &str, address: IpAddr) -> io::Result<()> {
    let route = match address {
        IpAddr::V4(address) => RouteMessageBuilder::<Ipv4Addr>::new()
            .destination_prefix(address, 32)
            .scope(RouteScope::Link)
            .output_interface(index)
            .build(),
        IpAddr::V6(address) => RouteMessageBuilder::<Ipv6Addr>::new()
            .destination_prefix(address, 128)
            .output_interface(index)
            .build(),
    };
    handle
        .route()
        .add(route)
        .replace()
        .execute()
        .await
        .map_err(netlink_error(&format!("route {address} to {name}")))
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

#[cfg(test)]
mod tests {
    use super::*;
    use rtnetlink::packet_route::link::{LinkAttribute, State};
    use rtnetlink::packet_route::neighbour::{
        NeighbourAddress, NeighbourAttribute, NeighbourState,
    };
    use rtnetlink::packet_route::route::{RouteAddress, RouteAttribute};
    use rtnetlink::LinkVeth;
    use std::fs::File;
    use std::os::fd::AsRawFd;
    use std::sync::mpsc;
    use std::time::{Duration, Instant};

    const POD: PodLink = PodLink {
        gateway: Ipv6Addr::new(0xfe80, 0, 0, 0, 0, 0, 0, 1),
        gateway4: Ipv4Addr::new(169, 254, 0, 1),
        ipv6: Ipv6Addr::new(0xfd00, 0xfe05, 1, 0, 0, 0, 0, 2),
        ipv4: Ipv4Addr::new(169, 254, 16, 1),
    };
    /// Where the guest sends a packet, through its gateway.
    const OUTSIDE: Ipv4Addr = Ipv4Addr::new(192, 0, 2, 99);

    /// Whether this process has a network namespace of its own: no device
    /// but loopback.
    fn alone() -> bool {
        std::fs::read_to_string("/proc/self/net/dev").is_ok_and(|dev| {
            dev.lines()
                .skip(2)
                .all(|line| line.trim_start().starts_with("lo:"))
        })
    }

    fn read(path: &str) -> String {
        std::fs::read_to_string(path).unwrap().trim().to_string()
    }

    /// The addresses on a device, with their lengths.
    async fn addresses_on(handle: &Handle, index: u32) -> Vec<(IpAddr, u8)> {
        let mut found: Vec<(IpAddr, u8)> = addresses(handle, index)
            .await
            .unwrap()
            .iter()
            .filter_map(|message| Some((address_of(message)?, message.header.prefix_len)))
            .collect();
        found.sort();
        found
    }

    /// The host routes to a device, of both families.
    async fn routes_to(handle: &Handle, index: u32) -> Vec<IpAddr> {
        let mut found = Vec::new();
        let dumps = [
            RouteMessageBuilder::<Ipv4Addr>::new().build(),
            RouteMessageBuilder::<Ipv6Addr>::new().build(),
        ];
        for dump in dumps {
            let routes: Vec<_> = handle
                .route()
                .get(dump)
                .execute()
                .try_collect()
                .await
                .unwrap();
            for route in routes {
                // Not the kernel's routes to the device's own addresses.
                let to_device = route.attributes.contains(&RouteAttribute::Oif(index))
                    && route.header.kind == RouteType::Unicast;
                let host = matches!(route.header.destination_prefix_length, 32 | 128);
                let destination = route
                    .attributes
                    .iter()
                    .find_map(|attribute| match attribute {
                        RouteAttribute::Destination(RouteAddress::Inet(v4)) => {
                            Some(IpAddr::V4(*v4))
                        }
                        RouteAttribute::Destination(RouteAddress::Inet6(v6)) => {
                            Some(IpAddr::V6(*v6))
                        }
                        _ => None,
                    });
                if let (true, true, Some(destination)) = (to_device, host, destination) {
                    found.push(destination);
                }
            }
        }
        found.sort();
        found
    }

    /// The addresses the node answers ARP for by proxy, and on which
    /// devices.
    async fn proxies(handle: &Handle) -> Vec<(u32, IpAddr)> {
        let neighbours: Vec<_> = handle
            .neighbours()
            .get()
            .proxies()
            .execute()
            .try_collect()
            .await
            .unwrap();
        neighbours
            .iter()
            .filter_map(|neighbour| {
                neighbour
                    .attributes
                    .iter()
                    .find_map(|attribute| match attribute {
                        NeighbourAttribute::Destination(NeighbourAddress::Inet(v4)) => {
                            Some((neighbour.header.ifindex, IpAddr::V4(*v4)))
                        }
                        _ => None,
                    })
            })
            .collect()
    }

    /// The guest's end of the link, in a network namespace of its own: it
    /// holds the pod's IPv4 /32 and routes everything via the gateway on
    /// its link, as feos-guest does. It sends `OUTSIDE` a packet and
    /// returns how long the gateway's MAC address took to resolve, and
    /// what it resolved to. The node says when the link is here, and when
    /// its end is up after the guest's (`up`).
    fn guest(
        namespace: mpsc::Sender<File>,
        link_is_here: mpsc::Receiver<()>,
        up: mpsc::Sender<()>,
    ) -> io::Result<(Duration, Vec<u8>)> {
        // A thread's own network namespace: the node's stays the process's.
        if unsafe { libc::unshare(libc::CLONE_NEWNET) } != 0 {
            return Err(io::Error::last_os_error());
        }
        namespace
            .send(File::open("/proc/thread-self/ns/net")?)
            .map_err(io::Error::other)?;
        link_is_here.recv().map_err(io::Error::other)?;
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()?;
        runtime.block_on(async {
            let handle = connect().await?;
            let index = index(&handle, "eth0").await?;
            add_address(&handle, index, POD.ipv4.into(), 32).await?;
            super::up(&handle, index, "eth0").await?;
            up.send(()).map_err(io::Error::other)?;
            link_is_here.recv().map_err(io::Error::other)?;
            let on_link = RouteMessageBuilder::<Ipv4Addr>::new()
                .destination_prefix(POD.gateway4, 32)
                .scope(RouteScope::Link)
                .output_interface(index)
                .build();
            let default = RouteMessageBuilder::<Ipv4Addr>::new()
                .gateway(POD.gateway4)
                .output_interface(index)
                .build();
            for route in [on_link, default] {
                handle
                    .route()
                    .add(route)
                    .execute()
                    .await
                    .map_err(io::Error::other)?;
            }
            let start = Instant::now();
            let socket = std::net::UdpSocket::bind((Ipv4Addr::UNSPECIFIED, 0))?;
            socket.send_to(b"hello", (OUTSIDE, 9))?;
            while start.elapsed() < Duration::from_secs(3) {
                let neighbours: Vec<_> = handle
                    .neighbours()
                    .get()
                    .execute()
                    .try_collect()
                    .await
                    .map_err(io::Error::other)?;
                for neighbour in neighbours {
                    let gateway = neighbour
                        .attributes
                        .contains(&NeighbourAttribute::Destination(NeighbourAddress::Inet(
                            POD.gateway4,
                        )));
                    let mac = neighbour
                        .attributes
                        .iter()
                        .find_map(|attribute| match attribute {
                            NeighbourAttribute::LinkLocalAddress(mac) => Some(mac.clone()),
                            _ => None,
                        });
                    if let (true, NeighbourState::Reachable, Some(mac)) =
                        (gateway, neighbour.header.state, mac)
                    {
                        return Ok((start.elapsed(), mac));
                    }
                }
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
            Err(io::Error::other("no answer to ARP for the gateway"))
        })
    }

    /// Sets a pod's link up as feosd does, with a veth pair for the TAP
    /// device and the guest's end in a network namespace of its own, and
    /// the resolver's dummy device; checks addresses, routes, sysctls and
    /// the proxy entry, and that the guest gets an answer to ARP for its
    /// gateway at once. Needs CAP_NET_ADMIN, in a network namespace of its
    /// own: `sudo unshare -n <the test binary> --ignored a_pod_gets_its_link`.
    #[tokio::test]
    #[ignore]
    async fn a_pod_gets_its_link() {
        assert!(alone(), "run this in a network namespace of its own");
        let handle = connect().await.unwrap();
        // The node's uplink, with a default route: the node answers ARP for
        // the gateway only while its route there leaves through another
        // device.
        handle
            .link()
            .add(LinkDummy::new("uplink").build())
            .execute()
            .await
            .unwrap();
        let uplink = index(&handle, "uplink").await.unwrap();
        add_address(&handle, uplink, "192.0.2.7".parse().unwrap(), 24)
            .await
            .unwrap();
        up(&handle, uplink, "uplink").await.unwrap();
        let default = RouteMessageBuilder::<Ipv4Addr>::new()
            .gateway(Ipv4Addr::new(192, 0, 2, 1))
            .output_interface(uplink)
            .build();
        handle.route().add(default).execute().await.unwrap();
        enable_forwarding().unwrap();
        assert_eq!(read("/proc/sys/net/ipv4/ip_forward"), "1");
        assert_eq!(read("/proc/sys/net/ipv6/conf/all/forwarding"), "1");

        // The pod's TAP device, a veth pair here, with an IPv4 address of
        // an earlier setup on it.
        handle
            .link()
            .add(LinkVeth::new("pod-test", "eth0").build())
            .execute()
            .await
            .unwrap();
        let tap = index(&handle, "pod-test").await.unwrap();
        add_address(&handle, tap, "192.0.2.200".parse().unwrap(), 32)
            .await
            .unwrap();
        let (namespace, from_guest) = mpsc::channel();
        let (link_is_here, to_guest) = mpsc::channel();
        let (guest_up, guest_is_up) = mpsc::channel();
        let guest = std::thread::spawn(move || guest(namespace, to_guest, guest_up));
        let namespace = from_guest.recv().unwrap();
        let peer = index(&handle, "eth0").await.unwrap();
        handle
            .link()
            .set(
                LinkUnspec::new_with_index(peer)
                    .setns_by_fd(namespace.as_raw_fd())
                    .build(),
            )
            .execute()
            .await
            .unwrap();

        // Twice: a pod is attached before every boot and resume.
        for _ in 0..2 {
            super::tap(&handle, "pod-test", &POD).await.unwrap();
        }
        assert_eq!(
            addresses_on(&handle, tap).await,
            [(IpAddr::V6(POD.gateway), 64)],
            "fe80::1 alone, no IPv4"
        );
        assert_eq!(
            routes_to(&handle, tap).await,
            [IpAddr::V4(POD.ipv4), IpAddr::V6(POD.ipv6)]
        );
        assert_eq!(proxies(&handle).await, [(tap, IpAddr::V4(POD.gateway4))]);
        for (path, value) in [
            ("ipv4/conf/pod-test/arp_ignore", "1"),
            ("ipv4/conf/pod-test/arp_announce", "2"),
            ("ipv4/conf/pod-test/forwarding", "1"),
            ("ipv4/neigh/pod-test/proxy_delay", "0"),
            ("ipv6/conf/pod-test/accept_ra", "0"),
            ("ipv6/conf/pod-test/autoconf", "0"),
            ("ipv6/conf/pod-test/addr_gen_mode", "1"),
        ] {
            assert_eq!(read(&format!("/proc/sys/net/{path}")), value, "{path}");
        }

        // Until the kernel has the node's end up after the guest's, which
        // it does a moment later, the node's answers do not leave. A pod's
        // TAP device is up long before its guest asks.
        link_is_here.send(()).unwrap();
        guest_is_up.recv().unwrap();
        let start = Instant::now();
        while !link(&handle, "pod-test")
            .await
            .unwrap()
            .is_some_and(|link| {
                link.attributes
                    .contains(&LinkAttribute::OperState(State::Up))
            })
        {
            assert!(
                start.elapsed() < Duration::from_secs(3),
                "pod-test is not up"
            );
            tokio::time::sleep(Duration::from_millis(1)).await;
        }
        link_is_here.send(()).unwrap();

        // The guest asks for its gateway, and the node answers by proxy,
        // at once, with the TAP device's MAC address.
        let (took, mac) = guest.join().unwrap().unwrap();
        let device = link(&handle, "pod-test").await.unwrap().unwrap();
        let tap_mac = device
            .attributes
            .iter()
            .find_map(|attribute| match attribute {
                LinkAttribute::Address(mac) => Some(mac.clone()),
                _ => None,
            });
        assert_eq!(Some(mac), tap_mac);
        assert!(took < Duration::from_millis(100), "ARP took {took:?}");
        println!("The node answered ARP for {} in {took:?}", POD.gateway4);
        // The node holds no address of the guest's link.
        let node = node_addresses(&handle).await.unwrap();
        assert!(!node.contains(&IpAddr::V4(POD.gateway4)), "{node:?}");

        // Its routes and its proxy entry go with the device.
        assert!(delete_link(&handle, "pod-test").await.unwrap());
        assert!(proxies(&handle).await.is_empty());
        assert!(routes_to(&handle, tap).await.is_empty());

        // The resolver's device holds its address alone, of either family.
        let resolver: IpAddr = "169.254.0.53".parse().unwrap();
        dummy_with_address(&handle, "feos-dns", "fd00::53".parse().unwrap())
            .await
            .unwrap();
        let dns = index(&handle, "feos-dns").await.unwrap();
        add_address(&handle, dns, resolver, 24).await.unwrap();
        for _ in 0..2 {
            dummy_with_address(&handle, "feos-dns", resolver)
                .await
                .unwrap();
            assert_eq!(addresses_on(&handle, dns).await, [(resolver, 32)]);
        }
    }
}
