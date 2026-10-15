// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Network configuration as the host pushes it: a static IPv6 address, a
//! static IPv4 address or both, their default routes, the resolvers and the
//! hostname. A guest runs no DHCP and takes no router advertisements; its
//! host pushes the whole configuration after every boot and every resume.
//! Netlink through rtnetlink, no ip(8).

use crate::error::HostError;
use feos_proto::host_service::{
    GetNetworkConfigResponse, ResolverFailure, SetNetworkConfigRequest, SetNetworkConfigResponse,
};
use feos_utils::filesystem::CONTAINERS_DIR;
use feos_utils::resolver;
use futures::TryStreamExt;
use log::{error, info};
use rtnetlink::packet_route::address::{
    AddressAttribute, AddressHeaderFlags, AddressMessage, AddressScope,
};
use rtnetlink::packet_route::link::{LinkFlags, LinkMessage};
use rtnetlink::packet_route::route::{RouteAttribute, RouteHeader, RouteMessage};
use rtnetlink::packet_route::AddressFamily;
use rtnetlink::{Handle, RouteMessageBuilder};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::path::Path;
use std::sync::{Arc, Mutex};
use tokio::sync::oneshot;

/// The last configuration pushed since boot.
pub type PushedNetworkConfig = Arc<Mutex<Option<SetNetworkConfigRequest>>>;

pub async fn handle_set_network_config(
    req: SetNetworkConfigRequest,
    pushed: PushedNetworkConfig,
    responder: oneshot::Sender<Result<SetNetworkConfigResponse, HostError>>,
) {
    let result = set_network_config(&req).await;
    if result.is_ok() {
        let addresses: Vec<String> = [
            (&req.ipv6_cidr, &req.ipv6_gateway),
            (&req.ipv4_cidr, &req.gateway),
        ]
        .into_iter()
        .filter(|(cidr, _)| !cidr.is_empty())
        .map(|(cidr, gateway)| format!("{cidr} via {gateway}"))
        .collect();
        info!(
            "HostWorker: Network configured: {} {}",
            req.interface,
            addresses.join(", ")
        );
        *pushed.lock().unwrap_or_else(|e| e.into_inner()) = Some(req);
    }
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for SetNetworkConfig.");
    }
}

pub async fn handle_get_network_config(
    pushed: PushedNetworkConfig,
    responder: oneshot::Sender<Result<GetNetworkConfigResponse, HostError>>,
) {
    let last = pushed.lock().unwrap_or_else(|e| e.into_inner()).clone();
    let response = match last {
        Some(c) => GetNetworkConfigResponse {
            configured: true,
            interface: c.interface,
            ipv4_cidr: c.ipv4_cidr,
            gateway: c.gateway,
            dns: c.dns,
            hostname: c.hostname,
            ipv6_cidr: c.ipv6_cidr,
            ipv6_gateway: c.ipv6_gateway,
        },
        None => GetNetworkConfigResponse::default(),
    };
    if responder.send(Ok(response)).is_err() {
        error!("HostWorker: Failed to send response for GetNetworkConfig.");
    }
}

async fn set_network_config(
    req: &SetNetworkConfigRequest,
) -> Result<SetNetworkConfigResponse, HostError> {
    let ipv4 = parse_static(AddressFamily::Inet, &req.ipv4_cidr, &req.gateway)?;
    let ipv6 = parse_static(AddressFamily::Inet6, &req.ipv6_cidr, &req.ipv6_gateway)?;
    if ipv4.is_none() && ipv6.is_none() {
        return Err(HostError::InvalidArgument(
            "neither an IPv6 nor an IPv4 address".to_string(),
        ));
    }
    // Each one becomes a nameserver line of resolv.conf.
    if let Some(dns) = req.dns.iter().find(|dns| dns.parse::<IpAddr>().is_err()) {
        return Err(HostError::InvalidArgument(format!("bad resolver: {dns:?}")));
    }

    configure_interface(&req.interface, ipv4, ipv6).await?;
    // A container that cannot be linked to the resolvers again does not fail
    // the push; the host learns which one and why.
    let dns = req.dns.clone();
    let resolver_failed = tokio::task::spawn_blocking(move || {
        resolver::publish(&dns, Path::new("/etc"), Path::new(CONTAINERS_DIR))
    })
    .await
    .map_err(|e| HostError::Failed(format!("resolvers: {e}")))?
    .map_err(|e| HostError::Failed(format!("resolvers: {e}")))?
    .into_iter()
    .map(|(container_id, reason)| ResolverFailure {
        container_id,
        reason,
    })
    .collect();
    if !req.hostname.is_empty() {
        nix::unistd::sethostname(&req.hostname)
            .map_err(|e| HostError::Failed(format!("sethostname: {e}")))?;
    }
    Ok(SetNetworkConfigResponse { resolver_failed })
}

/// One family's part of a push: the address, its prefix length and the
/// default gateway.
#[derive(Debug, PartialEq)]
struct Static {
    address: IpAddr,
    prefix_len: u8,
    gateway: IpAddr,
}

/// The address and gateway of `family` in a push, or None when the push
/// leaves both empty. An IPv6 address is one beyond the link, of /1 to
/// /128: the kernel makes the link-local address itself.
fn parse_static(
    family: AddressFamily,
    cidr: &str,
    gateway: &str,
) -> Result<Option<Static>, HostError> {
    if cidr.is_empty() && gateway.is_empty() {
        return Ok(None);
    }
    let ipv6 = family == AddressFamily::Inet6;
    let (name, prefix_lens) = if ipv6 {
        ("IPv6", 1..=128)
    } else {
        ("IPv4", 0..=32)
    };
    let bad = |what: &str, value: &str| {
        HostError::InvalidArgument(format!("bad {name} {what}: {value:?}"))
    };
    let (address, prefix_len) = cidr
        .split_once('/')
        .and_then(|(address, prefix_len)| {
            let address: IpAddr = address.parse().ok()?;
            let prefix_len: u8 = prefix_len.parse().ok()?;
            let usable = match address {
                IpAddr::V4(_) => !ipv6,
                IpAddr::V6(a) => {
                    ipv6 && !(a.is_unspecified()
                        || a.is_loopback()
                        || a.is_multicast()
                        || a.is_unicast_link_local())
                }
            };
            (usable && prefix_lens.contains(&prefix_len)).then_some((address, prefix_len))
        })
        .ok_or_else(|| bad("address/prefix", cidr))?;
    let gateway = gateway
        .parse::<IpAddr>()
        .ok()
        .filter(|g| match g {
            IpAddr::V4(_) => !ipv6,
            IpAddr::V6(g) => ipv6 && !(g.is_unspecified() || g.is_loopback() || g.is_multicast()),
        })
        .ok_or_else(|| bad("gateway", gateway))?;
    Ok(Some(Static {
        address,
        prefix_len,
        gateway,
    }))
}

/// Brings up lo and the interface and gives the interface exactly the pushed
/// addresses and default routes. An address that is already there stays, so
/// that a repeated push of the same configuration changes nothing; one of a
/// previous push goes, and so does a family the push leaves out.
async fn configure_interface(
    interface: &str,
    ipv4: Option<Static>,
    ipv6: Option<Static>,
) -> Result<(), HostError> {
    let (connection, handle, _) = rtnetlink::new_connection()
        .map_err(|e| HostError::Failed(format!("netlink connection: {e}")))?;
    tokio::spawn(connection);

    if let Ok(lo) = link_index(&handle, "lo").await {
        ignore_exists(handle.link().set(link_up(lo)).execute().await, "set lo up")?;
    }

    // The lookup comes first: a name the kernel knows is no path.
    let index = link_index(&handle, interface).await?;
    no_ipv6_autoconf(interface, ipv6.is_some())?;
    ignore_exists(
        handle.link().set(link_up(index)).execute().await,
        "set interface up",
    )?;
    configure_family(&handle, index, AddressFamily::Inet, ipv4).await?;
    configure_family(&handle, index, AddressFamily::Inet6, ipv6).await
}

/// Turns router advertisements and address autoconfiguration off on the
/// interface, before it goes up: the host pushes the address and the route.
/// A kernel without IPv6 has neither, which is an error only for a push with
/// an IPv6 address.
fn no_ipv6_autoconf(interface: &str, required: bool) -> Result<(), HostError> {
    for setting in ["accept_ra", "autoconf"] {
        let path = format!("/proc/sys/net/ipv6/conf/{interface}/{setting}");
        match std::fs::write(&path, "0\n") {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound && !required => {}
            result => result.map_err(|e| HostError::Failed(format!("{path}: {e}")))?,
        }
    }
    Ok(())
}

/// Makes `wanted` the interface's only global address of `family`, and its
/// gateway the family's default route; with None, removes both. Link-local
/// addresses stay.
async fn configure_family(
    handle: &Handle,
    index: u32,
    family: AddressFamily,
    wanted: Option<Static>,
) -> Result<(), HostError> {
    let failed = |what: &str, e: rtnetlink::Error| HostError::Failed(format!("{what}: {e}"));
    let wanted_address = wanted.as_ref().map(|w| (w.address, w.prefix_len));
    let mut stale = Vec::new();
    let mut addresses = handle
        .address()
        .get()
        .set_link_index_filter(index)
        .execute();
    while let Some(message) = addresses
        .try_next()
        .await
        .map_err(|e| failed("list addresses", e))?
    {
        let current = address_of(&message).map(|a| (a, message.header.prefix_len));
        if message.header.family == family
            && message.header.scope == AddressScope::Universe
            && current != wanted_address
        {
            stale.push(message);
        }
    }
    // Before the new address: removing a primary IPv4 address removes the
    // secondary ones of its subnet with it.
    for message in stale {
        ignore_gone(
            handle.address().del(message).execute().await,
            "remove address",
        )?;
    }

    let Some(wanted) = wanted else {
        return remove_default_routes(handle, index, family).await;
    };
    let mut add = handle
        .address()
        .add(index, wanted.address, wanted.prefix_len);
    if family == AddressFamily::Inet6 {
        // The node is the only other end of the link: no duplicate address
        // detection, which would hold the address back for a second.
        add.message_mut().header.flags |= AddressHeaderFlags::Nodad;
    }
    ignore_exists(add.execute().await, "add address")?;
    // A link-local gateway needs the interface.
    let default_route = match wanted.gateway {
        IpAddr::V4(gateway) => RouteMessageBuilder::<Ipv4Addr>::new()
            .gateway(gateway)
            .output_interface(index)
            .build(),
        IpAddr::V6(gateway) => RouteMessageBuilder::<Ipv6Addr>::new()
            .gateway(gateway)
            .output_interface(index)
            .build(),
    };
    handle
        .route()
        .add(default_route)
        .replace()
        .execute()
        .await
        .map_err(|e| failed("set default route", e))
}

/// The local address of an address message.
fn address_of(message: &AddressMessage) -> Option<IpAddr> {
    let mut address = None;
    for attribute in &message.attributes {
        match attribute {
            AddressAttribute::Local(local) => return Some(*local),
            AddressAttribute::Address(a) => address = Some(*a),
            _ => {}
        }
    }
    address
}

/// Removes the default routes of `family` in the main table that leave
/// through the interface.
async fn remove_default_routes(
    handle: &Handle,
    index: u32,
    family: AddressFamily,
) -> Result<(), HostError> {
    let mut query = RouteMessage::default();
    query.header.address_family = family;
    let mut routes = handle.route().get(query).execute();
    let mut defaults = Vec::new();
    while let Some(route) = routes
        .try_next()
        .await
        .map_err(|e| HostError::Failed(format!("list routes: {e}")))?
    {
        if route.header.destination_prefix_length == 0
            && route.header.table == RouteHeader::RT_TABLE_MAIN
            && route.attributes.contains(&RouteAttribute::Oif(index))
        {
            defaults.push(route);
        }
    }
    for route in defaults {
        ignore_gone(
            handle.route().del(route).execute().await,
            "remove default route",
        )?;
    }
    Ok(())
}

async fn link_index(handle: &Handle, name: &str) -> Result<u32, HostError> {
    handle
        .link()
        .get()
        .match_name(name.to_string())
        .execute()
        .try_next()
        .await
        .map_err(|e| HostError::NotFound(format!("interface {name}: {e}")))?
        .map(|link| link.header.index)
        .ok_or_else(|| HostError::NotFound(format!("interface {name}")))
}

fn link_up(index: u32) -> LinkMessage {
    let mut msg = LinkMessage::default();
    msg.header.index = index;
    msg.header.flags |= LinkFlags::Up;
    msg.header.change_mask |= LinkFlags::Up;
    msg
}

fn ignore_exists(result: Result<(), rtnetlink::Error>, what: &str) -> Result<(), HostError> {
    match result {
        Err(rtnetlink::Error::NetlinkError(e)) if e.raw_code() == -libc::EEXIST => Ok(()),
        Err(e) => Err(HostError::Failed(format!("{what}: {e}"))),
        Ok(()) => Ok(()),
    }
}

/// Like [`ignore_exists`], for what is gone already: an address the kernel
/// removed with another one, a route with its address.
fn ignore_gone(result: Result<(), rtnetlink::Error>, what: &str) -> Result<(), HostError> {
    match result {
        Err(rtnetlink::Error::NetlinkError(e))
            if [-libc::EADDRNOTAVAIL, -libc::ESRCH].contains(&e.raw_code()) =>
        {
            Ok(())
        }
        Err(e) => Err(HostError::Failed(format!("{what}: {e}"))),
        Ok(()) => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_ipv4() {
        assert_eq!(
            parse_static(AddressFamily::Inet, "192.168.100.10/24", "192.168.100.1").unwrap(),
            Some(Static {
                address: Ipv4Addr::new(192, 168, 100, 10).into(),
                prefix_len: 24,
                gateway: Ipv4Addr::new(192, 168, 100, 1).into(),
            })
        );
        assert_eq!(parse_static(AddressFamily::Inet, "", "").unwrap(), None);
        for (cidr, gateway) in [
            ("192.168.100.10", "192.168.100.1"),
            ("fd00::1/64", "192.168.100.1"),
            ("10.0.0.1/33", "10.0.0.2"),
            ("x/24", "10.0.0.2"),
            ("10.0.0.1/24", ""),
            ("10.0.0.1/24", "fe80::1"),
            ("", "10.0.0.2"),
        ] {
            assert!(
                parse_static(AddressFamily::Inet, cidr, gateway).is_err(),
                "{cidr} via {gateway}"
            );
        }
    }

    #[test]
    fn parses_ipv6() {
        assert_eq!(
            parse_static(AddressFamily::Inet6, "fd00:fe05:ffff::2/128", "fe80::1").unwrap(),
            Some(Static {
                address: "fd00:fe05:ffff::2".parse::<Ipv6Addr>().unwrap().into(),
                prefix_len: 128,
                gateway: "fe80::1".parse::<Ipv6Addr>().unwrap().into(),
            })
        );
        assert!(parse_static(AddressFamily::Inet6, "2001:db8::2/64", "2001:db8::1").is_ok());
        assert_eq!(parse_static(AddressFamily::Inet6, "", "").unwrap(), None);
        for (cidr, gateway) in [
            ("fd00::2", "fe80::1"),
            ("fd00::2/129", "fe80::1"),
            ("fd00::2/0", "fe80::1"),
            ("10.0.0.1/24", "fe80::1"),
            ("fe80::2/64", "fe80::1"),
            ("::/128", "fe80::1"),
            ("::1/128", "fe80::1"),
            ("ff02::1/128", "fe80::1"),
            ("fd00::2/128", ""),
            ("fd00::2/128", "10.0.0.1"),
            ("fd00::2/128", "ff02::2"),
            ("fd00::2/128", "::"),
            ("", "fe80::1"),
        ] {
            assert!(
                parse_static(AddressFamily::Inet6, cidr, gateway).is_err(),
                "{cidr} via {gateway}"
            );
        }
    }

    /// The global addresses of the interface and the gateways of the default
    /// routes.
    async fn configured(handle: &Handle, index: u32) -> (Vec<String>, Vec<String>) {
        use rtnetlink::packet_route::route::RouteAddress;
        let mut addresses = Vec::new();
        let mut messages = handle
            .address()
            .get()
            .set_link_index_filter(index)
            .execute();
        while let Some(message) = messages.try_next().await.unwrap() {
            if message.header.scope == AddressScope::Universe {
                let address = address_of(&message).unwrap();
                addresses.push(format!("{address}/{}", message.header.prefix_len));
            }
        }
        let mut gateways = Vec::new();
        for family in [AddressFamily::Inet, AddressFamily::Inet6] {
            let mut query = RouteMessage::default();
            query.header.address_family = family;
            let mut routes = handle.route().get(query).execute();
            while let Some(route) = routes.try_next().await.unwrap() {
                if route.header.destination_prefix_length != 0 {
                    continue;
                }
                for attribute in route.attributes {
                    match attribute {
                        RouteAttribute::Gateway(RouteAddress::Inet(g)) => {
                            gateways.push(g.to_string())
                        }
                        RouteAttribute::Gateway(RouteAddress::Inet6(g)) => {
                            gateways.push(g.to_string())
                        }
                        _ => {}
                    }
                }
            }
        }
        (addresses, gateways)
    }

    /// Pushes into one end of a veth pair, in a network namespace of the
    /// test's thread; needs CAP_NET_ADMIN.
    #[test]
    #[ignore]
    fn pushes_replace_addresses_and_routes() {
        assert_eq!(unsafe { libc::unshare(libc::CLONE_NEWNET) }, 0, "unshare");
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            let (connection, handle, _) = rtnetlink::new_connection().unwrap();
            tokio::spawn(connection);
            let veth = rtnetlink::LinkVeth::new("eth0", "node0").build();
            handle.link().add(veth).execute().await.unwrap();
            let node = link_index(&handle, "node0").await.unwrap();
            handle.link().set(link_up(node)).execute().await.unwrap();
            let index = link_index(&handle, "eth0").await.unwrap();

            let ipv4 = |cidr| parse_static(AddressFamily::Inet, cidr, "10.0.0.1").unwrap();
            let ipv6 = |cidr| parse_static(AddressFamily::Inet6, cidr, "fe80::1").unwrap();
            let both = (
                vec!["10.0.0.2/24".to_string(), "fd00::2/128".to_string()],
                vec!["10.0.0.1".to_string(), "fe80::1".to_string()],
            );
            configure_interface("eth0", ipv4("10.0.0.2/24"), ipv6("fd00::2/128"))
                .await
                .unwrap();
            assert_eq!(configured(&handle, index).await, both);
            configure_interface("eth0", ipv4("10.0.0.2/24"), ipv6("fd00::2/128"))
                .await
                .unwrap();
            assert_eq!(configured(&handle, index).await, both, "the same again");

            configure_interface("eth0", None, ipv6("fd00::3/128"))
                .await
                .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (vec!["fd00::3/128".to_string()], vec!["fe80::1".to_string()])
            );
            configure_interface("eth0", ipv4("10.0.0.3/24"), None)
                .await
                .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (
                    vec!["10.0.0.3/24".to_string()],
                    vec!["10.0.0.1".to_string()]
                )
            );
            for setting in ["accept_ra", "autoconf"] {
                let path = format!("/proc/sys/net/ipv6/conf/eth0/{setting}");
                assert_eq!(std::fs::read_to_string(path).unwrap(), "0\n", "{setting}");
            }
        });
    }
}
