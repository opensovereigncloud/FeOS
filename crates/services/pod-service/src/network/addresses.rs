// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' addresses: IPv6 ones of the node's prefix, never handed out
//! twice (ADR 0010).

use std::collections::HashSet;
use std::fs;
use std::io::{self, Write};
use std::net::Ipv6Addr;
use std::path::{Path, PathBuf};

/// The first address of the prefix a pod gets: `::0` is the subnet's
/// router anycast address, and `::1` is left to the network.
const FIRST: u128 = 2;

/// Hands out the addresses of the pod prefix in turn, from `::2` on. The
/// next one is kept in a file, written before an address is handed out,
/// so that none is handed out twice, across restarts too.
pub struct Ipv6Addresses {
    counter: PathBuf,
    prefix: Ipv6Addr,
    len: u8,
}

impl Ipv6Addresses {
    pub fn new(dir: &Path, prefix: Ipv6Addr, len: u8) -> Self {
        Self {
            counter: dir.join("next-ipv6-address"),
            prefix,
            len,
        }
    }

    /// Whether the address is in the prefix.
    pub fn contains(&self, address: Ipv6Addr) -> bool {
        self.offset(address).is_some()
    }

    /// The next address, and none that a pod in `used` holds.
    pub fn allocate(&self, used: &HashSet<Ipv6Addr>) -> io::Result<Ipv6Addr> {
        // Past every address a pod holds, should the counter have been
        // lost while the pods were not.
        let held = used.iter().filter_map(|address| self.offset(*address));
        let offset = held
            .map(|offset| offset.saturating_add(1))
            .fold(self.next(), u128::max);
        let last = self.last();
        if offset > last {
            return Err(io::Error::other(format!(
                "the pod prefix {}/{} is used up",
                self.prefix, self.len
            )));
        }
        let address = Ipv6Addr::from(u128::from(self.prefix) | offset);
        write_atomically(&self.counter, &offset.saturating_add(1).to_string())?;
        Ok(address)
    }

    /// The next offset, as the counter keeps it.
    fn next(&self) -> u128 {
        fs::read_to_string(&self.counter)
            .ok()
            .and_then(|next| next.trim().parse::<u128>().ok())
            .unwrap_or(FIRST)
            .max(FIRST)
    }

    fn last(&self) -> u128 {
        u128::MAX.checked_shr(u32::from(self.len)).unwrap_or(0)
    }

    fn offset(&self, address: Ipv6Addr) -> Option<u128> {
        let host = self.last();
        let address = u128::from(address);
        (address & !host == u128::from(self.prefix)).then_some(address & host)
    }
}

/// Replaces `path` by a rename, after the new content is on disk.
fn write_atomically(path: &Path, content: &str) -> io::Result<()> {
    let temporary = path.with_extension("new");
    let mut file = fs::File::create(&temporary)?;
    file.write_all(content.as_bytes())?;
    file.sync_all()?;
    fs::rename(&temporary, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(s: &str) -> Ipv6Addr {
        s.parse().unwrap()
    }

    #[test]
    fn ipv6_addresses_are_never_handed_out_twice() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Ipv6Addresses::new(dir.path(), a("fd00:fe05:1::"), 64);
        let mut used = HashSet::new();
        let first = addresses.allocate(&used).unwrap();
        assert_eq!(first, a("fd00:fe05:1::2"));
        used.insert(first);
        assert_eq!(addresses.allocate(&used).unwrap(), a("fd00:fe05:1::3"));
        // ::2 is free again, but never comes back: not after a restart
        // either.
        used.remove(&first);
        let again = Ipv6Addresses::new(dir.path(), a("fd00:fe05:1::"), 64);
        assert_eq!(again.allocate(&used).unwrap(), a("fd00:fe05:1::4"));
        assert_eq!(
            fs::read_to_string(dir.path().join("next-ipv6-address")).unwrap(),
            "5"
        );
    }

    #[test]
    fn ipv6_addresses_go_past_those_pods_hold() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Ipv6Addresses::new(dir.path(), a("fd00:fe05:1::"), 64);
        // The counter is gone, the pods are not.
        let used = HashSet::from([a("fd00:fe05:1::7"), a("2001:db8::99")]);
        assert_eq!(addresses.allocate(&used).unwrap(), a("fd00:fe05:1::8"));
        assert!(addresses.contains(a("fd00:fe05:1:0:ffff::1")));
        assert!(!addresses.contains(a("fd00:fe05:2::1")));
    }

    #[test]
    fn a_small_prefix_runs_out() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Ipv6Addresses::new(dir.path(), a("fd00::100"), 126);
        let used = HashSet::new();
        assert_eq!(addresses.allocate(&used).unwrap(), a("fd00::102"));
        assert_eq!(addresses.allocate(&used).unwrap(), a("fd00::103"));
        assert!(addresses.allocate(&used).is_err());
    }
}
