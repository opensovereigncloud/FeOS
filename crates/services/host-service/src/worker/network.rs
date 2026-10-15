// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Network configuration as the host pushes it: a static IPv4 address, the
//! default route, the resolvers and the hostname. A guest runs no DHCP; its
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
use rtnetlink::packet_route::link::{LinkFlags, LinkMessage};
use rtnetlink::{Handle, RouteMessageBuilder};
use std::net::{IpAddr, Ipv4Addr};
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
        info!(
            "HostWorker: Network configured: {} {} via {}",
            req.interface, req.ipv4_cidr, req.gateway
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
    let (address, prefix_len) = parse_ipv4_cidr(&req.ipv4_cidr)?;
    let gateway: Ipv4Addr = req
        .gateway
        .parse()
        .map_err(|_| HostError::InvalidArgument(format!("bad gateway: {:?}", req.gateway)))?;

    configure_interface(&req.interface, address, prefix_len, gateway).await?;
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

fn parse_ipv4_cidr(cidr: &str) -> Result<(Ipv4Addr, u8), HostError> {
    let invalid = || HostError::InvalidArgument(format!("bad IPv4 address/prefix: {cidr:?}"));
    let (address, prefix_len) = cidr.split_once('/').ok_or_else(invalid)?;
    let address: Ipv4Addr = address.parse().map_err(|_| invalid())?;
    let prefix_len: u8 = prefix_len.parse().map_err(|_| invalid())?;
    if prefix_len > 32 {
        return Err(invalid());
    }
    Ok((address, prefix_len))
}

/// Brings up lo and the interface, adds the address and the default route.
/// An address or route that already exists counts as success, so that a
/// repeated push of the same configuration works.
async fn configure_interface(
    interface: &str,
    address: Ipv4Addr,
    prefix_len: u8,
    gateway: Ipv4Addr,
) -> Result<(), HostError> {
    let (connection, handle, _) = rtnetlink::new_connection()
        .map_err(|e| HostError::Failed(format!("netlink connection: {e}")))?;
    tokio::spawn(connection);

    if let Ok(lo) = link_index(&handle, "lo").await {
        ignore_exists(handle.link().set(link_up(lo)).execute().await, "set lo up")?;
    }

    let index = link_index(&handle, interface).await?;
    ignore_exists(
        handle.link().set(link_up(index)).execute().await,
        "set interface up",
    )?;
    ignore_exists(
        handle
            .address()
            .add(index, IpAddr::V4(address), prefix_len)
            .execute()
            .await,
        "add address",
    )?;
    let default_route = RouteMessageBuilder::<Ipv4Addr>::new()
        .gateway(gateway)
        .build();
    ignore_exists(
        handle.route().add(default_route).execute().await,
        "add default route",
    )
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_ipv4_cidr() {
        assert_eq!(
            parse_ipv4_cidr("192.168.100.10/24").unwrap(),
            (Ipv4Addr::new(192, 168, 100, 10), 24)
        );
        for bad in ["192.168.100.10", "fd00::1/64", "10.0.0.1/33", "x/24"] {
            assert!(parse_ipv4_cidr(bad).is_err(), "{bad}");
        }
    }
}
