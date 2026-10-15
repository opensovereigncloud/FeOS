// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The sysctls that NAT64 needs.

use log::info;
use std::fs;
use std::io;
use std::path::Path;

const IPV6_CONF: &str = "/proc/sys/net/ipv6/conf";

fn write(path: &Path, value: &str) -> io::Result<()> {
    fs::write(path, value)
        .map_err(|e| io::Error::new(e.kind(), format!("NAT64: {}: {e}", path.display())))
}

/// Turns IPv4 and IPv6 forwarding on, after
/// [`keep_router_advertisements`]. Pods need it with or without NAT64:
/// feosd's network setup should call this before it routes to pods.
pub fn enable_forwarding() -> io::Result<()> {
    keep_router_advertisements()?;
    write(Path::new("/proc/sys/net/ipv4/ip_forward"), "1")?;
    write(&Path::new(IPV6_CONF).join("all/forwarding"), "1")
}

/// Lets interfaces that take router advertisements (`accept_ra` 1) go on
/// taking them once IPv6 forwarding is on (`accept_ra` 2): a router
/// ignores them otherwise, and a node that learns its default route from
/// them loses it when it expires. Pods' TAP devices (`pod-*`) and FeOS's
/// own (`feos-*`) are left alone: the node takes no routes from them.
pub fn keep_router_advertisements() -> io::Result<()> {
    for entry in fs::read_dir(IPV6_CONF)? {
        let entry = entry?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if ["all", "default", "lo"].contains(&name.as_ref())
            || name.starts_with("pod-")
            || name.starts_with("feos-")
        {
            continue;
        }
        let path = entry.path().join("accept_ra");
        // An interface may be gone by now.
        let Ok(value) = fs::read_to_string(&path) else {
            continue;
        };
        if value.trim() == "1" {
            write(&path, "2")?;
            info!("NAT64: {name} keeps taking router advertisements while forwarding");
        }
    }
    Ok(())
}

/// The translator's device forwards, takes no router advertisements, and
/// filters reverse paths loosely. The translated IPv4 packets that enter
/// it come from the pool, which is routed into it, so a strict filter
/// passes them too; a loose one also passes them while the routes change.
pub(crate) fn device(name: &str) -> io::Result<()> {
    let ipv4 = Path::new("/proc/sys/net/ipv4/conf").join(name);
    let ipv6 = Path::new(IPV6_CONF).join(name);
    write(&ipv4.join("rp_filter"), "2")?;
    write(&ipv4.join("forwarding"), "1")?;
    write(&ipv6.join("accept_ra"), "0")?;
    write(&ipv6.join("forwarding"), "1")
}
