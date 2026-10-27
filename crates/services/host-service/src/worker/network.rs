// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Network configuration as the host pushes it: a static IPv6 address, a
//! static IPv4 address or both, their default routes or the routes that
//! replace them, the resolvers and the hostname. A gateway may lie outside
//! its address's prefix, as a pod's IPv4 gateway does outside its /32: it
//! gets a route on the link. A guest runs no DHCP and takes no router
//! advertisements; its host pushes the whole configuration after every
//! boot and every resume. Netlink through rtnetlink, no ip(8).

use crate::error::HostError;
use feos_proto::host_service::{
    GetNetworkConfigResponse, NetworkRoute, ResolverFailure, SetNetworkConfigRequest,
    SetNetworkConfigResponse,
};
use feos_utils::filesystem::CONTAINERS_DIR;
use feos_utils::resolver;
use futures::TryStreamExt;
use log::{error, info};
use rtnetlink::packet_route::address::{
    AddressAttribute, AddressHeaderFlags, AddressMessage, AddressScope,
};
use rtnetlink::packet_route::link::{LinkFlags, LinkMessage};
use rtnetlink::packet_route::route::{
    RouteAddress, RouteAttribute, RouteHeader, RouteMessage, RouteMetric, RouteProtocol,
    RouteScope, RouteType,
};
use rtnetlink::packet_route::AddressFamily;
use rtnetlink::{Handle, RouteMessageBuilder};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::path::Path;
use std::sync::{Arc, Mutex};
use tokio::sync::oneshot;

/// The last configuration pushed since boot.
pub type PushedNetworkConfig = Arc<Mutex<Option<SetNetworkConfigRequest>>>;

/// Pushes take effect one after the other: each one makes the interface
/// what it says, and the last one is what GetNetworkConfig repeats.
static PUSH: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

pub async fn handle_set_network_config(
    req: SetNetworkConfigRequest,
    pushed: PushedNetworkConfig,
    responder: oneshot::Sender<Result<SetNetworkConfigResponse, HostError>>,
) {
    let _push = PUSH.lock().await;
    let result = set_network_config(&req).await;
    if result.is_ok() {
        let mut addresses: Vec<String> = [
            (&req.ipv6_cidr, &req.ipv6_gateway),
            (&req.ipv4_cidr, &req.gateway),
        ]
        .into_iter()
        .filter(|(cidr, _)| !cidr.is_empty())
        .map(|(cidr, gateway)| format!("{cidr} via {gateway}"))
        .collect();
        addresses.extend(req.routes.iter().map(|route| match route.mtu {
            0 => format!("{} via {}", route.destination, route.gateway),
            mtu => format!("{} via {} mtu {mtu}", route.destination, route.gateway),
        }));
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
            routes: c.routes,
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
    let mut ipv4 = parse_static(AddressFamily::Inet, &req.ipv4_cidr, &req.gateway)?;
    let mut ipv6 = parse_static(AddressFamily::Inet6, &req.ipv6_cidr, &req.ipv6_gateway)?;
    if ipv4.is_none() && ipv6.is_none() {
        return Err(HostError::InvalidArgument(
            "neither an IPv6 nor an IPv4 address".to_string(),
        ));
    }
    for route in &req.routes {
        let route = parse_route(route)?;
        let family = match route.destination {
            IpAddr::V4(_) => ipv4.as_mut(),
            IpAddr::V6(_) => ipv6.as_mut(),
        };
        let family = family.ok_or_else(|| {
            HostError::InvalidArgument(format!(
                "route to {}/{}: the push has no address of its family",
                route.destination, route.len
            ))
        })?;
        family.routes.push(route);
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

/// One family's part of a push: the address, its prefix length, the
/// default gateway, and the routes that replace the default route.
#[derive(Debug, PartialEq)]
struct Static {
    address: IpAddr,
    prefix_len: u8,
    gateway: IpAddr,
    routes: Vec<Via>,
}

impl Static {
    /// The routes the family gets on the interface, in order, beside those
    /// the kernel makes for the address: the default route via the gateway,
    /// or the pushed routes instead, and before them, for each gateway
    /// outside the address's prefix, a route to it on the link, without
    /// which the kernel takes no route via it. An IPv6 gateway on the
    /// link-local prefix needs none.
    fn routes(&self) -> Vec<Route> {
        let vias = match self.routes.is_empty() {
            true => vec![Via::default_route(self.gateway)],
            false => self.routes.clone(),
        };
        let mut routes = Vec::new();
        for gateway in vias.iter().map(|via| via.gateway) {
            let on_link = match gateway {
                IpAddr::V6(gateway) if gateway.is_unicast_link_local() => true,
                gateway => same_prefix(self.address, gateway, self.prefix_len),
            };
            if !on_link && !routes.contains(&Route::Link(gateway)) {
                routes.push(Route::Link(gateway));
            }
        }
        routes.extend(vias.into_iter().map(Route::Via));
        routes
    }
}

/// Whether `a` and `b` are of one family and share their first
/// `prefix_len` bits.
fn same_prefix(a: IpAddr, b: IpAddr, prefix_len: u8) -> bool {
    let (a, b, bits) = match (a, b) {
        (IpAddr::V4(a), IpAddr::V4(b)) => (u128::from(u32::from(a)), u128::from(u32::from(b)), 32),
        (IpAddr::V6(a), IpAddr::V6(b)) => (u128::from(a), u128::from(b), 128),
        _ => return false,
    };
    (a ^ b)
        .checked_shr(bits - u32::from(prefix_len))
        .unwrap_or(0)
        == 0
}

/// The address and gateway of `family` in a push, or None when the push
/// leaves both empty. An IPv6 address is one beyond the link, of /1 to
/// /128: the kernel makes the link-local address itself. The gateway may be
/// outside the address's prefix, but not the address itself.
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
                IpAddr::V4(a) => !ipv6 && unicast_v4(a),
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
            IpAddr::V4(g) => !ipv6 && unicast_v4(*g),
            IpAddr::V6(g) => ipv6 && !(g.is_unspecified() || g.is_loopback() || g.is_multicast()),
        })
        .filter(|g| *g != address)
        .ok_or_else(|| bad("gateway", gateway))?;
    Ok(Some(Static {
        address,
        prefix_len,
        gateway,
        routes: Vec::new(),
    }))
}

/// A pushed route: a prefix without bits beyond its length, a gateway of
/// its family that a host can have, and an MTU that the family can carry,
/// or 0 for the interface's.
fn parse_route(route: &NetworkRoute) -> Result<Via, HostError> {
    let bad = |what: &str| {
        HostError::InvalidArgument(format!(
            "route to {:?} via {:?}: {what}",
            route.destination, route.gateway
        ))
    };
    let (destination, len) = route
        .destination
        .split_once('/')
        .and_then(|(address, len)| Some((address.parse::<IpAddr>().ok()?, len.parse::<u8>().ok()?)))
        .ok_or_else(|| bad("not a prefix"))?;
    let (bits, min_mtu) = match destination {
        IpAddr::V4(_) => (32, 68),
        IpAddr::V6(_) => (128, 1280),
    };
    if len > bits {
        return Err(bad("not a prefix"));
    }
    if !beyond_is_zero(destination, len) {
        return Err(bad("the prefix has bits beyond its length"));
    }
    let gateway = route
        .gateway
        .parse::<IpAddr>()
        .ok()
        .filter(|g| g.is_ipv4() == destination.is_ipv4())
        .filter(|g| match g {
            IpAddr::V4(g) => unicast_v4(*g),
            IpAddr::V6(g) => !(g.is_unspecified() || g.is_loopback() || g.is_multicast()),
        })
        .ok_or_else(|| bad("not a gateway of the prefix's family"))?;
    if route.mtu != 0 && !(min_mtu..=65535).contains(&route.mtu) {
        return Err(bad(&format!("an MTU of {}", route.mtu)));
    }
    Ok(Via {
        destination,
        len,
        gateway,
        mtu: route.mtu,
    })
}

/// Whether the bits of `address` after its first `len` are all zero.
fn beyond_is_zero(address: IpAddr, len: u8) -> bool {
    let (value, bits) = match address {
        IpAddr::V4(a) => (u128::from(u32::from(a)), 32),
        IpAddr::V6(a) => (u128::from(a), 128),
    };
    value
        .checked_shl(u32::from(len) + (128 - bits))
        .unwrap_or(0)
        == 0
}

/// The unspecified address of `address`'s family.
fn unspecified(address: IpAddr) -> IpAddr {
    match address {
        IpAddr::V4(_) => Ipv4Addr::UNSPECIFIED.into(),
        IpAddr::V6(_) => Ipv6Addr::UNSPECIFIED.into(),
    }
}

/// Whether an IPv4 address can be a host's on a link: not unspecified,
/// loopback, multicast or the limited broadcast.
fn unicast_v4(address: Ipv4Addr) -> bool {
    !(address.is_unspecified()
        || address.is_loopback()
        || address.is_multicast()
        || address.is_broadcast())
}

/// Brings up lo and the interface and gives the interface exactly the pushed
/// addresses and routes. An address or route that is already there stays,
/// so that a repeated push of the same configuration changes nothing; one of
/// a previous push goes, and so does a family the push leaves out.
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
/// routes the family's routes through the interface; with None, removes
/// both. Link-local addresses stay, and so do the routes the kernel makes
/// for the addresses.
async fn configure_family(
    handle: &Handle,
    index: u32,
    family: AddressFamily,
    wanted: Option<Static>,
) -> Result<(), HostError> {
    let failed = |what: &str, e: rtnetlink::Error| HostError::Failed(format!("{what}: {e}"));
    let wanted_address = wanted.as_ref().map(|w| (w.address, w.prefix_len));
    let mut present = false;
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
        if message.header.family == family && message.header.scope == AddressScope::Universe {
            if current == wanted_address {
                present = true;
            } else {
                stale.push(message);
            }
        }
    }
    // Before the new address: removing a primary IPv4 address removes the
    // secondary ones of its subnet with it. Removing the last one removes
    // every IPv4 route through the interface, too; the routes come below.
    for message in stale {
        ignore_gone(
            handle.address().del(message).execute().await,
            "remove address",
        )?;
    }

    let routes = wanted.as_ref().map(Static::routes).unwrap_or_default();
    if let Some(wanted) = wanted.filter(|_| !present) {
        let mut add = handle
            .address()
            .add(index, wanted.address, wanted.prefix_len);
        if family == AddressFamily::Inet6 {
            // The node is the only other end of the link: no duplicate address
            // detection, which would hold the address back for a second.
            add.message_mut().header.flags |= AddressHeaderFlags::Nodad;
        } else if wanted.prefix_len >= 31 {
            // A /31 or /32 has no broadcast address, though rtnetlink makes
            // one of the prefix: a /32's would be the address itself.
            add.message_mut()
                .attributes
                .retain(|a| !matches!(a, AddressAttribute::Broadcast(_)));
        }
        ignore_exists(add.execute().await, "add address")?;
    }
    configure_routes(handle, index, family, &routes).await
}

/// A route via a gateway: a prefix, the default route's of length 0, and
/// its MTU, 0 for the interface's.
#[derive(Debug, Clone, Copy, PartialEq)]
struct Via {
    destination: IpAddr,
    len: u8,
    gateway: IpAddr,
    mtu: u32,
}

impl Via {
    fn default_route(gateway: IpAddr) -> Self {
        Self {
            destination: unspecified(gateway),
            len: 0,
            gateway,
            mtu: 0,
        }
    }
}

/// A route that a push gives the interface.
#[derive(Debug, Clone, Copy, PartialEq)]
enum Route {
    /// To the gateway alone, on the interface's link.
    Link(IpAddr),
    /// A prefix, or the default route, via a gateway.
    Via(Via),
}

impl Route {
    /// The message that adds the route through the interface. A link-local
    /// gateway needs the interface, too.
    fn message(self, index: u32) -> RouteMessage {
        let mut message = match self {
            // IPv4 takes a route via a gateway only when the gateway's own
            // route is of the link's scope.
            Route::Link(IpAddr::V4(gateway)) => RouteMessageBuilder::<Ipv4Addr>::new()
                .destination_prefix(gateway, 32)
                .scope(RouteScope::Link)
                .output_interface(index)
                .build(),
            Route::Link(IpAddr::V6(gateway)) => RouteMessageBuilder::<Ipv6Addr>::new()
                .destination_prefix(gateway, 128)
                .output_interface(index)
                .build(),
            Route::Via(via) => match (via.destination, via.gateway) {
                (IpAddr::V4(destination), IpAddr::V4(gateway)) => {
                    let builder = RouteMessageBuilder::<Ipv4Addr>::new();
                    let builder = match via.len {
                        0 => builder,
                        len => builder.destination_prefix(destination, len),
                    };
                    builder.gateway(gateway).output_interface(index).build()
                }
                (IpAddr::V6(destination), IpAddr::V6(gateway)) => {
                    let builder = RouteMessageBuilder::<Ipv6Addr>::new();
                    let builder = match via.len {
                        0 => builder,
                        len => builder.destination_prefix(destination, len),
                    };
                    builder.gateway(gateway).output_interface(index).build()
                }
                // parse_route keeps the families together.
                _ => RouteMessage::default(),
            },
        };
        if let Route::Via(Via { mtu, .. }) = self
            && mtu != 0
        {
            message
                .attributes
                .push(RouteAttribute::Metrics(vec![RouteMetric::Mtu(mtu)]));
        }
        message
    }

    /// The route that a message of the kernel's is, if it is one of these.
    fn of(message: &RouteMessage) -> Option<Route> {
        let host_len = match message.header.address_family {
            AddressFamily::Inet => 32,
            AddressFamily::Inet6 => 128,
            _ => return None,
        };
        if message.header.kind != RouteType::Unicast {
            return None;
        }
        let ip = |address: &RouteAddress| match address {
            RouteAddress::Inet(a) => Some(IpAddr::V4(*a)),
            RouteAddress::Inet6(a) => Some(IpAddr::V6(*a)),
            _ => None,
        };
        let mut destination = None;
        let mut gateway = None;
        let mut mtu = 0;
        for attribute in &message.attributes {
            match attribute {
                RouteAttribute::Destination(d) => destination = ip(d),
                RouteAttribute::Gateway(g) => gateway = ip(g),
                RouteAttribute::Metrics(metrics) => {
                    for metric in metrics {
                        if let RouteMetric::Mtu(value) = metric {
                            mtu = *value;
                        }
                    }
                }
                _ => {}
            }
        }
        match (
            message.header.destination_prefix_length,
            destination,
            gateway,
        ) {
            (0, _, Some(gateway)) => Some(Route::Via(Via {
                mtu,
                ..Via::default_route(gateway)
            })),
            (len, Some(destination), Some(gateway)) => Some(Route::Via(Via {
                destination,
                len,
                gateway,
                mtu,
            })),
            // IPv6 routes have no scope.
            (len, Some(destination), None)
                if len == host_len
                    && (host_len == 128 || message.header.scope == RouteScope::Link) =>
            {
                Some(Route::Link(destination))
            }
            _ => None,
        }
    }
}

impl std::fmt::Display for Route {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Route::Link(gateway) => write!(f, "route to {gateway} on the link"),
            Route::Via(via) => {
                match via.len {
                    0 => write!(f, "default route via {}", via.gateway)?,
                    len => write!(f, "route to {}/{len} via {}", via.destination, via.gateway)?,
                }
                match via.mtu {
                    0 => Ok(()),
                    mtu => write!(f, " with an MTU of {mtu}"),
                }
            }
        }
    }
}

/// Makes `wanted` the routes of `family` through the interface in the main
/// table, beside those the kernel makes for the addresses: a route that is
/// there already stays, the others go, and the missing ones come in order.
async fn configure_routes(
    handle: &Handle,
    index: u32,
    family: AddressFamily,
    wanted: &[Route],
) -> Result<(), HostError> {
    let mut query = RouteMessage::default();
    query.header.address_family = family;
    let mut routes = handle.route().get(query).execute();
    let mut present = Vec::new();
    let mut stale = Vec::new();
    while let Some(message) = routes
        .try_next()
        .await
        .map_err(|e| HostError::Failed(format!("list routes: {e}")))?
    {
        if message.header.table != RouteHeader::RT_TABLE_MAIN
            || message.header.protocol == RouteProtocol::Kernel
            || !message.attributes.contains(&RouteAttribute::Oif(index))
        {
            continue;
        }
        match Route::of(&message) {
            Some(route) if wanted.contains(&route) && !present.contains(&route) => {
                present.push(route)
            }
            _ => stale.push(message),
        }
    }
    for message in stale {
        ignore_gone(handle.route().del(message).execute().await, "remove route")?;
    }
    for route in wanted.iter().filter(|route| !present.contains(route)) {
        handle
            .route()
            .add(route.message(index))
            .execute()
            .await
            .map_err(|e| HostError::Failed(format!("add {route}: {e}")))?;
    }
    Ok(())
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

    fn default(gateway: IpAddr) -> Route {
        Route::Via(Via::default_route(gateway))
    }

    #[test]
    fn parses_ipv4() {
        assert_eq!(
            parse_static(AddressFamily::Inet, "192.168.100.10/24", "192.168.100.1").unwrap(),
            Some(Static {
                address: Ipv4Addr::new(192, 168, 100, 10).into(),
                prefix_len: 24,
                gateway: Ipv4Addr::new(192, 168, 100, 1).into(),
                routes: Vec::new(),
            })
        );
        // A pod's: a /32 with the node's address on the link as gateway.
        assert_eq!(
            parse_static(AddressFamily::Inet, "169.254.16.2/32", "169.254.0.1").unwrap(),
            Some(Static {
                address: Ipv4Addr::new(169, 254, 16, 2).into(),
                prefix_len: 32,
                gateway: Ipv4Addr::new(169, 254, 0, 1).into(),
                routes: Vec::new(),
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
            ("0.0.0.0/0", "10.0.0.2"),
            ("127.0.0.2/8", "127.0.0.1"),
            ("224.0.0.5/32", "10.0.0.2"),
            ("255.255.255.255/32", "10.0.0.2"),
            ("169.254.16.2/32", "169.254.16.2"),
            ("169.254.16.2/32", "0.0.0.0"),
            ("169.254.16.2/32", "255.255.255.255"),
            ("169.254.16.2/32", "224.0.0.1"),
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
                routes: Vec::new(),
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
            ("fd00::2/128", "fd00::2"),
            ("", "fe80::1"),
        ] {
            assert!(
                parse_static(AddressFamily::Inet6, cidr, gateway).is_err(),
                "{cidr} via {gateway}"
            );
        }
    }

    #[test]
    fn routes_reach_the_gateway() {
        let routes = |family, cidr, gateway| {
            parse_static(family, cidr, gateway)
                .unwrap()
                .unwrap()
                .routes()
        };
        let ip = |a: &str| a.parse::<IpAddr>().unwrap();
        // A gateway outside the address's prefix gets a route on the link
        // first.
        assert_eq!(
            routes(AddressFamily::Inet, "169.254.16.2/32", "169.254.0.1"),
            [Route::Link(ip("169.254.0.1")), default(ip("169.254.0.1"))]
        );
        assert_eq!(
            routes(AddressFamily::Inet, "192.168.100.10/24", "192.168.100.1"),
            [default(ip("192.168.100.1"))]
        );
        assert_eq!(
            routes(AddressFamily::Inet, "192.168.100.10/24", "192.168.101.1"),
            [
                Route::Link(ip("192.168.101.1")),
                default(ip("192.168.101.1"))
            ]
        );
        assert_eq!(
            routes(AddressFamily::Inet, "10.0.0.2/0", "192.168.101.1"),
            [default(ip("192.168.101.1"))]
        );
        assert_eq!(
            routes(AddressFamily::Inet6, "fd00:fe05:1::2/128", "fe80::1"),
            [default(ip("fe80::1"))]
        );
        assert_eq!(
            routes(AddressFamily::Inet6, "2001:db8::2/64", "2001:db8::1"),
            [default(ip("2001:db8::1"))]
        );
        assert_eq!(
            routes(AddressFamily::Inet6, "2001:db8::2/128", "2001:db8::1"),
            [Route::Link(ip("2001:db8::1")), default(ip("2001:db8::1"))]
        );
        // Pushed routes replace the default route, and a gateway outside
        // the prefix gets its route on the link once.
        let ula = |destination: &str, gateway: &str, mtu| NetworkRoute {
            destination: destination.to_string(),
            gateway: gateway.to_string(),
            mtu,
        };
        let mut pod = parse_static(AddressFamily::Inet6, "fd00:fe05:1::2/128", "fe80::1")
            .unwrap()
            .unwrap();
        pod.routes
            .push(parse_route(&ula("fc00::/7", "fe80::1", 1420)).unwrap());
        let mesh = Route::Via(Via {
            destination: ip("fc00::"),
            len: 7,
            gateway: ip("fe80::1"),
            mtu: 1420,
        });
        assert_eq!(pod.routes(), [mesh]);
        let mut far = parse_static(AddressFamily::Inet6, "2001:db8::2/128", "2001:db8::1")
            .unwrap()
            .unwrap();
        for destination in ["2001:db8:1::/48", "2001:db8:2::/48"] {
            far.routes
                .push(parse_route(&ula(destination, "2001:db8::1", 0)).unwrap());
        }
        assert_eq!(far.routes().len(), 3);
        assert_eq!(far.routes()[0], Route::Link(ip("2001:db8::1")));
        // What the guest adds, it knows again.
        for route in [
            Route::Link(ip("169.254.0.1")),
            default(ip("169.254.0.1")),
            Route::Link(ip("2001:db8::1")),
            default(ip("fe80::1")),
            mesh,
            Route::Via(Via {
                destination: ip("10.9.0.0"),
                len: 16,
                gateway: ip("169.254.0.1"),
                mtu: 0,
            }),
        ] {
            assert_eq!(Route::of(&route.message(2)), Some(route));
        }
        assert_eq!(
            mesh.to_string(),
            "route to fc00::/7 via fe80::1 with an MTU of 1420"
        );
    }

    #[test]
    fn routes_are_checked() {
        let route = |destination: &str, gateway: &str, mtu| {
            parse_route(&NetworkRoute {
                destination: destination.to_string(),
                gateway: gateway.to_string(),
                mtu,
            })
        };
        assert!(route("fc00::/7", "fe80::1", 1420).is_ok());
        assert!(route("::/0", "fe80::1", 0).is_ok());
        assert!(route("10.9.0.0/16", "169.254.0.1", 1400).is_ok());
        for (destination, gateway, mtu) in [
            ("fc00::1/7", "fe80::1", 0),
            ("fc00::/129", "fe80::1", 0),
            ("fc00::", "fe80::1", 0),
            ("fc00::/7", "169.254.0.1", 0),
            ("10.9.0.0/16", "fe80::1", 0),
            ("10.9.0.0/33", "169.254.0.1", 0),
            ("fc00::/7", "ff02::1", 0),
            ("fc00::/7", "::", 0),
            ("fc00::/7", "fe80::1", 1279),
            ("fc00::/7", "fe80::1", 65536),
            ("10.9.0.0/16", "169.254.0.1", 67),
        ] {
            assert!(
                route(destination, gateway, mtu).is_err(),
                "{destination} via {gateway} mtu {mtu}"
            );
        }
    }

    /// The global addresses of the interface, with their broadcast
    /// addresses, and the routes through it in the main table that the
    /// kernel did not make.
    async fn configured(handle: &Handle, index: u32) -> (Vec<String>, Vec<String>) {
        let mut addresses = Vec::new();
        let mut messages = handle
            .address()
            .get()
            .set_link_index_filter(index)
            .execute();
        while let Some(message) = messages.try_next().await.unwrap() {
            if message.header.scope == AddressScope::Universe {
                let address = address_of(&message).unwrap();
                let mut address = format!("{address}/{}", message.header.prefix_len);
                for attribute in &message.attributes {
                    if let AddressAttribute::Broadcast(broadcast) = attribute {
                        address.push_str(&format!(" brd {broadcast}"));
                    }
                }
                addresses.push(address);
            }
        }
        let ip = |address: &RouteAddress| match address {
            RouteAddress::Inet(a) => a.to_string(),
            RouteAddress::Inet6(a) => a.to_string(),
            other => format!("{other:?}"),
        };
        let mut routes = Vec::new();
        for family in [AddressFamily::Inet, AddressFamily::Inet6] {
            let mut query = RouteMessage::default();
            query.header.address_family = family;
            let mut messages = handle.route().get(query).execute();
            while let Some(message) = messages.try_next().await.unwrap() {
                if message.header.table != RouteHeader::RT_TABLE_MAIN
                    || message.header.protocol == RouteProtocol::Kernel
                    || !message.attributes.contains(&RouteAttribute::Oif(index))
                {
                    continue;
                }
                let mut route = "default".to_string();
                for attribute in &message.attributes {
                    if let RouteAttribute::Destination(d) = attribute {
                        let len = message.header.destination_prefix_length;
                        route = format!("{}/{len}", ip(d));
                    }
                }
                for attribute in &message.attributes {
                    if let RouteAttribute::Gateway(g) = attribute {
                        route.push_str(&format!(" via {}", ip(g)));
                    }
                }
                for attribute in &message.attributes {
                    if let RouteAttribute::Metrics(metrics) = attribute {
                        for metric in metrics {
                            if let RouteMetric::Mtu(mtu) = metric {
                                route.push_str(&format!(" mtu {mtu}"));
                            }
                        }
                    }
                }
                if message.header.scope == RouteScope::Link {
                    route.push_str(" scope link");
                }
                routes.push(route);
            }
        }
        routes.sort();
        (addresses, routes)
    }

    fn strings(values: &[&str]) -> Vec<String> {
        values.iter().map(|v| v.to_string()).collect()
    }

    /// Pushes into one end of a veth pair, in a network namespace of the
    /// test's thread; needs CAP_SYS_ADMIN and CAP_NET_ADMIN, which a user
    /// namespace gives:
    /// `unshare -rn cargo test -p host-service --lib -- --ignored`.
    #[test]
    #[ignore]
    fn pushes_replace_addresses_and_routes() {
        use rtnetlink::constants::{
            RTMGRP_IPV4_IFADDR, RTMGRP_IPV4_ROUTE, RTMGRP_IPV6_IFADDR, RTMGRP_IPV6_ROUTE,
        };
        use rtnetlink::sys::{AsyncSocket, SocketAddr};

        assert_eq!(unsafe { libc::unshare(libc::CLONE_NEWNET) }, 0, "unshare");
        // Duplicate address detection of the link-local addresses would
        // change them a second after the links come up.
        std::fs::write("/proc/sys/net/ipv6/conf/default/accept_dad", "0\n").unwrap();
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
            // Hears of every change to addresses and routes.
            let (mut connection, monitor, mut changes) = rtnetlink::new_connection().unwrap();
            let groups =
                RTMGRP_IPV4_IFADDR | RTMGRP_IPV6_IFADDR | RTMGRP_IPV4_ROUTE | RTMGRP_IPV6_ROUTE;
            connection
                .socket_mut()
                .socket_mut()
                .bind(&SocketAddr::new(0, groups))
                .unwrap();
            tokio::spawn(connection);

            let ipv4 = |cidr, gateway| parse_static(AddressFamily::Inet, cidr, gateway).unwrap();
            let ipv6 = |cidr| parse_static(AddressFamily::Inet6, cidr, "fe80::1").unwrap();

            // A pod's: a /32 and a gateway outside it, and a /128.
            let pod = (
                strings(&["169.254.16.2/32", "fd00:fe05:1::2/128"]),
                strings(&[
                    "169.254.0.1/32 scope link",
                    "default via 169.254.0.1",
                    "default via fe80::1",
                ]),
            );
            configure_interface(
                "eth0",
                ipv4("169.254.16.2/32", "169.254.0.1"),
                ipv6("fd00:fe05:1::2/128"),
            )
            .await
            .unwrap();
            assert_eq!(configured(&handle, index).await, pod);
            // A request answered on the monitor's socket comes after the
            // changes before it.
            link_index(&monitor, "eth0").await.unwrap();
            let mut changed = 0;
            while let Ok(Some(_)) = changes.try_next() {
                changed += 1;
            }
            assert!(changed > 0, "the monitor heard of no change");
            configure_interface(
                "eth0",
                ipv4("169.254.16.2/32", "169.254.0.1"),
                ipv6("fd00:fe05:1::2/128"),
            )
            .await
            .unwrap();
            assert_eq!(configured(&handle, index).await, pod, "the same again");
            link_index(&monitor, "eth0").await.unwrap();
            if let Ok(Some((change, _))) = changes.try_next() {
                panic!("the same push again changed {change:?}");
            }

            // As after a resume on another node.
            configure_interface(
                "eth0",
                ipv4("169.254.16.3/32", "169.254.0.1"),
                ipv6("fd00:fe05:1::3/128"),
            )
            .await
            .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (strings(&["169.254.16.3/32", "fd00:fe05:1::3/128"]), pod.1)
            );

            // Another gateway, while the address stays: the route to the
            // old one goes, and so do routes of others through the
            // interface and the family the push leaves out.
            for route in [
                RouteMessageBuilder::<Ipv4Addr>::new()
                    .destination_prefix(Ipv4Addr::new(10, 9, 0, 0), 16)
                    .scope(RouteScope::Link)
                    .protocol(RouteProtocol::Boot)
                    .output_interface(index)
                    .build(),
                RouteMessageBuilder::<Ipv6Addr>::new()
                    .destination_prefix("fd00:9::".parse().unwrap(), 64)
                    .protocol(RouteProtocol::Boot)
                    .output_interface(index)
                    .build(),
            ] {
                handle.route().add(route).execute().await.unwrap();
            }
            configure_interface("eth0", ipv4("169.254.16.3/32", "169.254.0.2"), None)
                .await
                .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (
                    strings(&["169.254.16.3/32"]),
                    strings(&["169.254.0.2/32 scope link", "default via 169.254.0.2"])
                )
            );

            // A gateway in the address's prefix needs no route of its own.
            configure_interface("eth0", ipv4("10.0.0.2/24", "10.0.0.1"), ipv6("fd00::2/128"))
                .await
                .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (
                    strings(&["10.0.0.2/24 brd 10.0.0.255", "fd00::2/128"]),
                    strings(&["default via 10.0.0.1", "default via fe80::1"])
                )
            );

            // A pod whose address is a ULA: a route for the ULAs in
            // place of the default route.
            let mut ula = ipv6("fd00::3/128").unwrap();
            ula.routes.push(Via {
                destination: "fc00::".parse().unwrap(),
                len: 7,
                gateway: "fe80::1".parse().unwrap(),
                mtu: 1420,
            });
            configure_interface("eth0", ipv4("169.254.16.4/32", "169.254.0.1"), Some(ula))
                .await
                .unwrap();
            let routes = strings(&[
                "169.254.0.1/32 scope link",
                "default via 169.254.0.1",
                "fc00::/7 via fe80::1 mtu 1420",
            ]);
            assert_eq!(configured(&handle, index).await.1, routes);
            let mut ula = ipv6("fd00::3/128").unwrap();
            ula.routes.push(Via {
                destination: "fc00::".parse().unwrap(),
                len: 7,
                gateway: "fe80::1".parse().unwrap(),
                mtu: 1420,
            });
            configure_interface("eth0", ipv4("169.254.16.4/32", "169.254.0.1"), Some(ula))
                .await
                .unwrap();
            assert_eq!(configured(&handle, index).await.1, routes, "the same again");

            configure_interface("eth0", None, ipv6("fd00::3/128"))
                .await
                .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (strings(&["fd00::3/128"]), strings(&["default via fe80::1"]))
            );
            configure_interface("eth0", ipv4("169.254.16.4/32", "169.254.0.1"), None)
                .await
                .unwrap();
            assert_eq!(
                configured(&handle, index).await,
                (
                    strings(&["169.254.16.4/32"]),
                    strings(&["169.254.0.1/32 scope link", "default via 169.254.0.1"])
                )
            );
            for setting in ["accept_ra", "autoconf"] {
                let path = format!("/proc/sys/net/ipv6/conf/eth0/{setting}");
                assert_eq!(std::fs::read_to_string(path).unwrap(), "0\n", "{setting}");
            }
        });
    }
}
