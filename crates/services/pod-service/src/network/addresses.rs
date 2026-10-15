// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' addresses: IPv6 ones of the node's prefix, never handed out
//! twice (ADR 0010), and IPv4 ones of the NAT64 pool, which come round
//! again.

use std::collections::HashSet;
use std::fs;
use std::io::{self, Write};
use std::net::{Ipv4Addr, Ipv6Addr};
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

/// Hands out the NAT64 pool's addresses in turn, from a cursor kept in a
/// file: an address a deleted pod freed comes round again only after all
/// others. `.1` stands for the node (`nat64::Config::node_ipv4`).
pub struct Pool {
    cursor: PathBuf,
    base: Ipv4Addr,
    len: u8,
}

impl Pool {
    pub fn new(dir: &Path, base: Ipv4Addr, len: u8) -> Self {
        Self {
            cursor: dir.join("next-address"),
            base,
            len,
        }
    }

    pub fn contains(&self, address: Ipv4Addr) -> bool {
        let mask = u32::MAX.checked_shl(32 - u32::from(self.len)).unwrap_or(0);
        let offset = u32::from(address) & !mask;
        u32::from(address) & mask == u32::from(self.base) && (2..=self.last()).contains(&offset)
    }

    /// The next address no pod in `used` holds.
    pub fn allocate(&self, used: &HashSet<Ipv4Addr>) -> io::Result<Ipv4Addr> {
        let base = u32::from(self.base);
        // .0 is the network, .1 the node, the last the broadcast.
        let (first, last) = (2u32, self.last());
        if last < first {
            return Err(io::Error::other("the NAT64 pool has no room for pods"));
        }
        let start = fs::read_to_string(&self.cursor)
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
                write_atomically(&self.cursor, &next.to_string())?;
                return Ok(address);
            }
        }
        Err(io::Error::other("the NAT64 pool is full"))
    }

    fn last(&self) -> u32 {
        (u32::MAX.checked_shr(u32::from(self.len)).unwrap_or(0)).saturating_sub(1)
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

    #[test]
    fn pool_addresses_come_round_in_turn() {
        let dir = tempfile::tempdir().unwrap();
        let pool = Pool::new(dir.path(), Ipv4Addr::new(10, 200, 0, 0), 16);
        let mut used = HashSet::new();
        let first = pool.allocate(&used).unwrap();
        assert_eq!(first, Ipv4Addr::new(10, 200, 0, 2));
        used.insert(first);
        assert_eq!(pool.allocate(&used).unwrap(), Ipv4Addr::new(10, 200, 0, 3));
        // .2 is free again, but its turn comes last.
        used.remove(&first);
        assert_eq!(pool.allocate(&used).unwrap(), Ipv4Addr::new(10, 200, 0, 4));
        fs::write(dir.path().join("next-address"), "65534").unwrap();
        assert_eq!(
            pool.allocate(&used).unwrap(),
            Ipv4Addr::new(10, 200, 255, 254)
        );
        assert_eq!(pool.allocate(&used).unwrap(), Ipv4Addr::new(10, 200, 0, 2));
        assert!(pool.contains(Ipv4Addr::new(10, 200, 0, 2)));
        assert!(!pool.contains(Ipv4Addr::new(10, 200, 0, 1)), "the node's");
        assert!(!pool.contains(Ipv4Addr::new(10, 201, 0, 2)));
    }
}
