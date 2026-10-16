// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's resolvers: those DHCPv4 and DHCPv6 name, of both families,
//! in /etc/resolv.conf. Until one names any, the initramfs's stay.

use log::{info, warn};
use std::fs;
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::path::Path;
use std::sync::Mutex;

const RESOLV_CONF: &str = "/etc/resolv.conf";
/// How many resolvers musl's resolver takes (MAXNS).
const MAX_SERVERS: usize = 3;

struct Servers {
    ipv4: Vec<IpAddr>,
    ipv6: Vec<IpAddr>,
    written: Option<String>,
}

static SERVERS: Mutex<Servers> = Mutex::new(Servers {
    ipv4: Vec::new(),
    ipv6: Vec::new(),
    written: None,
});

/// The resolvers DHCPv4 named.
pub fn set_ipv4(servers: &[Ipv4Addr]) {
    update(|all| all.ipv4 = servers.iter().copied().map(IpAddr::V4).collect());
}

/// The resolvers DHCPv6 named.
pub fn set_ipv6(servers: &[Ipv6Addr]) {
    update(|all| all.ipv6 = servers.iter().copied().map(IpAddr::V6).collect());
}

fn update(change: impl FnOnce(&mut Servers)) {
    let mut servers = SERVERS.lock().unwrap_or_else(|e| e.into_inner());
    change(&mut servers);
    let Some(content) = render(&servers.ipv4, &servers.ipv6) else {
        return;
    };
    if servers.written.as_ref() == Some(&content) {
        return;
    }
    match write(Path::new(RESOLV_CONF), &content) {
        Ok(()) => {
            info!(
                "Resolvers: {}",
                content
                    .lines()
                    .filter_map(|line| line.strip_prefix("nameserver "))
                    .collect::<Vec<_>>()
                    .join(", ")
            );
            servers.written = Some(content);
        }
        Err(e) => warn!("Failed to write {RESOLV_CONF}: {e}"),
    }
}

/// resolv.conf with the resolvers of both families in turn, IPv4 first,
/// as many as the resolver takes; none if there are none.
fn render(ipv4: &[IpAddr], ipv6: &[IpAddr]) -> Option<String> {
    let mut servers: Vec<IpAddr> = Vec::new();
    for i in 0..ipv4.len().max(ipv6.len()) {
        for server in [ipv4.get(i), ipv6.get(i)].into_iter().flatten() {
            if !servers.contains(server) {
                servers.push(*server);
            }
        }
    }
    if servers.is_empty() {
        return None;
    }
    let mut content = String::from("# The resolvers DHCP named, written by feos-host-init.\n");
    for server in servers.iter().take(MAX_SERVERS) {
        content.push_str(&format!("nameserver {server}\n"));
    }
    Some(content)
}

/// Replaces the file in one step.
fn write(path: &Path, content: &str) -> io::Result<()> {
    let staging = path.with_extension("conf.new");
    fs::write(&staging, content)?;
    fs::rename(&staging, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn addresses(list: &[&str]) -> Vec<IpAddr> {
        list.iter()
            .map(|address| address.parse().unwrap())
            .collect()
    }

    #[test]
    fn both_families_take_turns() {
        let content = render(
            &addresses(&["10.0.2.3", "8.8.8.8"]),
            &addresses(&["fec0::3", "2001:4860:4860::8888"]),
        )
        .unwrap();
        assert_eq!(
            content.lines().skip(1).collect::<Vec<_>>(),
            [
                "nameserver 10.0.2.3",
                "nameserver fec0::3",
                "nameserver 8.8.8.8"
            ]
        );
    }

    #[test]
    fn one_family_alone_and_duplicates() {
        let content = render(&addresses(&["10.0.2.3", "10.0.2.3"]), &[]).unwrap();
        assert_eq!(
            content.lines().skip(1).collect::<Vec<_>>(),
            ["nameserver 10.0.2.3"]
        );
        assert_eq!(render(&[], &[]), None);
    }

    #[test]
    fn the_file_is_replaced() {
        let dir = std::env::temp_dir().join(format!("feos-resolv-{}", std::process::id()));
        fs::create_dir_all(&dir).unwrap();
        let path = dir.join("resolv.conf");
        fs::write(&path, "nameserver 192.0.2.1\n").unwrap();
        write(&path, "nameserver 10.0.2.3\n").unwrap();
        assert_eq!(fs::read_to_string(&path).unwrap(), "nameserver 10.0.2.3\n");
        fs::remove_dir_all(&dir).unwrap();
    }
}
