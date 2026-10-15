// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' addresses (ADR 0010): IPv6 ones of the node's prefix, never
//! handed out twice, and IPv4 ones of the node's range, which come round
//! again.

use log::warn;
use std::collections::{HashSet, VecDeque};
use std::fs;
use std::io::{self, Write};
use std::net::{Ipv4Addr, Ipv6Addr};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard, PoisonError};
use std::time::{SystemTime, UNIX_EPOCH};

/// The first address of the prefix a pod gets: `::0` is the subnet's
/// router anycast address, and `::1` is left to the network.
const FIRST: u128 = 2;
/// The first offset of the IPv4 range a pod gets: the range's first
/// address is left out, and so is its last.
const FIRST_IPV4: u32 = 1;

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

/// Hands out the addresses of the pods' IPv4 range, a /32 each, unique on
/// the node: one never handed out first, the lowest, and when there is
/// none, the one that has been free the longest. The range's first and
/// last address are left out. Which addresses were handed out, and those
/// freed since in the order they were freed, are kept in a file, written
/// before an address is handed out, so that a restart keeps the order.
pub struct Ipv4Addresses {
    path: PathBuf,
    base: Ipv4Addr,
    len: u8,
    state: Mutex<Ipv4State>,
}

/// What [`Ipv4Addresses`] keeps, as offsets into the range.
#[derive(Clone, Debug, Default, PartialEq)]
struct Ipv4State {
    /// Every offset below has been handed out; one above may have been,
    /// if it is in `freed` or a pod holds it.
    next: u32,
    /// The offsets handed out and freed since, with when, in seconds since
    /// the epoch: the one free longest first.
    freed: VecDeque<(u32, u64)>,
}

impl Ipv4Addresses {
    pub fn new(dir: &Path, base: Ipv4Addr, len: u8) -> Self {
        let addresses = Self {
            path: dir.join("ipv4-addresses"),
            base,
            len,
            state: Mutex::default(),
        };
        let state = match fs::read_to_string(&addresses.path) {
            Ok(content) => addresses.parse(&content),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ipv4State::default(),
            Err(e) => {
                warn!(
                    "Network: {} does not read, the pods' IPv4 addresses start afresh: {e}",
                    addresses.path.display()
                );
                Ipv4State::default()
            }
        };
        *addresses.lock() = state;
        addresses
    }

    /// Whether a pod may have the address: it is in the range, and neither
    /// its first nor its last.
    pub fn contains(&self, address: Ipv4Addr) -> bool {
        self.offset(address).is_some()
    }

    /// An address no pod in `used` holds: the lowest never handed out, or
    /// else the one free longest.
    pub fn allocate(&self, used: &HashSet<Ipv4Addr>) -> io::Result<Ipv4Addr> {
        let mut state = self.lock();
        let mut next = state.clone();
        let is_used = |offset: u32| used.contains(&self.address(offset));
        let freed: HashSet<u32> = next.freed.iter().map(|(offset, _)| *offset).collect();
        // A pod or the freed may hold one above `next`, should the file
        // have been lost while the pods were not: it was handed out.
        let mut offset = next.next.max(FIRST_IPV4);
        while offset <= self.last() && (is_used(offset) || freed.contains(&offset)) {
            offset += 1;
        }
        let offset = if offset <= self.last() {
            next.next = offset + 1;
            offset
        } else {
            let free = next.freed.iter().position(|(offset, _)| !is_used(*offset));
            let Some((offset, _)) = free.and_then(|position| next.freed.remove(position)) else {
                return Err(io::Error::other(format!(
                    "the pods' IPv4 range {}/{} is used up",
                    self.base, self.len
                )));
            };
            offset
        };
        self.save(&next)?;
        *state = next;
        Ok(self.address(offset))
    }

    /// Frees an address a pod held, at `when`: it comes round again after
    /// the others free now.
    pub fn free(&self, address: Ipv4Addr, when: SystemTime) -> io::Result<()> {
        let Some(offset) = self.offset(address) else {
            return Ok(());
        };
        let mut state = self.lock();
        let mut next = state.clone();
        next.freed.retain(|(freed, _)| *freed != offset);
        next.freed.push_back((offset, seconds(when)));
        self.save(&next)?;
        *state = next;
        Ok(())
    }

    /// Brings what is kept in line with the addresses that pods hold, at
    /// the start: an address pods hold is not free, and one handed out
    /// that no pod holds and that is not free, such as one whose pod was
    /// deleted while feosd could not record it, is free from `when` on.
    pub fn reconcile(&self, used: &HashSet<Ipv4Addr>, when: SystemTime) -> io::Result<()> {
        let mut state = self.lock();
        let mut next = state.clone();
        next.freed
            .retain(|(offset, _)| !used.contains(&self.address(*offset)));
        let freed: HashSet<u32> = next.freed.iter().map(|(offset, _)| *offset).collect();
        let lost: Vec<u32> = (FIRST_IPV4..next.next.min(self.last() + 1))
            .filter(|offset| !freed.contains(offset) && !used.contains(&self.address(*offset)))
            .collect();
        for offset in lost {
            warn!(
                "Network: {} was handed out and is free",
                self.address(offset)
            );
            next.freed.push_back((offset, seconds(when)));
        }
        if next != *state {
            self.save(&next)?;
            *state = next;
        }
        Ok(())
    }

    fn lock(&self) -> MutexGuard<'_, Ipv4State> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The last offset a pod may have: the one before the last address.
    fn last(&self) -> u32 {
        u32::MAX
            .checked_shr(u32::from(self.len))
            .unwrap_or(0)
            .saturating_sub(1)
    }

    fn offset(&self, address: Ipv4Addr) -> Option<u32> {
        let host = u32::MAX.checked_shr(u32::from(self.len)).unwrap_or(0);
        let address = u32::from(address);
        let offset = address & host;
        (address & !host == u32::from(self.base) && (FIRST_IPV4..=self.last()).contains(&offset))
            .then_some(offset)
    }

    fn address(&self, offset: u32) -> Ipv4Addr {
        Ipv4Addr::from(u32::from(self.base) | offset)
    }

    /// The file: `next <address>`, then `freed <address> <seconds>`, a
    /// line each, the one free longest first. Addresses outside the range,
    /// as after a change of `--pod-ipv4-range`, are left out.
    fn parse(&self, content: &str) -> Ipv4State {
        let mut state = Ipv4State::default();
        for line in content.lines() {
            let fields: Vec<&str> = line.split_whitespace().collect();
            let offset = |field: Option<&&str>| {
                field
                    .and_then(|address| address.parse().ok())
                    .and_then(|address| self.offset(address))
            };
            match fields.first() {
                Some(&"next") => {
                    // Past the last: all were handed out.
                    let address = fields.get(1).and_then(|a| a.parse::<Ipv4Addr>().ok());
                    let range = u32::MAX.checked_shr(u32::from(self.len)).unwrap_or(0);
                    if let Some(address) = address
                        && u32::from(address) & !range == u32::from(self.base)
                    {
                        state.next = u32::from(address) & range;
                    }
                }
                Some(&"freed") => {
                    let when = fields.get(2).and_then(|when| when.parse().ok());
                    if let (Some(offset), Some(when)) = (offset(fields.get(1)), when)
                        && !state.freed.iter().any(|(freed, _)| *freed == offset)
                    {
                        state.freed.push_back((offset, when));
                    }
                }
                _ if line.trim().is_empty() || line.starts_with('#') => {}
                _ => warn!("Network: {}: a line not read: {line}", self.path.display()),
            }
        }
        state
    }

    fn save(&self, state: &Ipv4State) -> io::Result<()> {
        let mut content = format!("next {}\n", self.address(state.next));
        for (offset, when) in &state.freed {
            content.push_str(&format!("freed {} {when}\n", self.address(*offset)));
        }
        write_atomically(&self.path, &content)
            .map_err(|e| io::Error::new(e.kind(), format!("{}: {e}", self.path.display())))
    }
}

fn seconds(when: SystemTime) -> u64 {
    when.duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_secs())
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

    fn v4(s: &str) -> Ipv4Addr {
        s.parse().unwrap()
    }

    fn at(seconds: u64) -> SystemTime {
        UNIX_EPOCH + std::time::Duration::from_secs(seconds)
    }

    /// Allocates for a pod: the address is used from then on.
    fn take(addresses: &Ipv4Addresses, used: &mut HashSet<Ipv4Addr>) -> Ipv4Addr {
        let address = addresses.allocate(used).unwrap();
        assert!(used.insert(address), "{address} twice");
        address
    }

    #[test]
    fn ipv4_addresses_never_used_come_first() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Ipv4Addresses::new(dir.path(), v4("169.254.16.0"), 20);
        let mut used = HashSet::new();
        // The range's first address is left out.
        assert_eq!(take(&addresses, &mut used), v4("169.254.16.1"));
        assert_eq!(take(&addresses, &mut used), v4("169.254.16.2"));
        // .1 is free, but one never used comes first.
        used.remove(&v4("169.254.16.1"));
        addresses.free(v4("169.254.16.1"), at(100)).unwrap();
        assert_eq!(take(&addresses, &mut used), v4("169.254.16.3"));
        assert_eq!(
            fs::read_to_string(dir.path().join("ipv4-addresses")).unwrap(),
            "next 169.254.16.4\nfreed 169.254.16.1 100\n"
        );
    }

    #[test]
    fn ipv4_addresses_free_longest_come_next() {
        let dir = tempfile::tempdir().unwrap();
        // .1 to .6: .0 and .7 are left out.
        let addresses = Ipv4Addresses::new(dir.path(), v4("10.77.0.0"), 29);
        let mut used = HashSet::new();
        let all: Vec<Ipv4Addr> = (0..6).map(|_| take(&addresses, &mut used)).collect();
        assert_eq!(all.first(), Some(&v4("10.77.0.1")));
        assert_eq!(all.last(), Some(&v4("10.77.0.6")));
        let full = addresses.allocate(&used).unwrap_err();
        assert!(full.to_string().contains("used up"), "{full}");
        // Freed in the order .5, .2, .4: they come round in that order.
        for (address, when) in [("10.77.0.5", 10), ("10.77.0.2", 20), ("10.77.0.4", 30)] {
            used.remove(&v4(address));
            addresses.free(v4(address), at(when)).unwrap();
        }
        assert_eq!(take(&addresses, &mut used), v4("10.77.0.5"));
        // One freed again goes to the end.
        used.remove(&v4("10.77.0.5"));
        addresses.free(v4("10.77.0.5"), at(40)).unwrap();
        assert_eq!(take(&addresses, &mut used), v4("10.77.0.2"));
        assert_eq!(take(&addresses, &mut used), v4("10.77.0.4"));
        assert_eq!(take(&addresses, &mut used), v4("10.77.0.5"));
        assert!(addresses.allocate(&used).is_err());
        // An address outside the range is no pod's: freeing it does nothing.
        addresses.free(v4("10.77.0.7"), at(50)).unwrap();
        addresses.free(v4("192.0.2.1"), at(50)).unwrap();
        assert!(addresses.allocate(&used).is_err());
    }

    #[test]
    fn ipv4_addresses_keep_their_order_across_restarts() {
        let dir = tempfile::tempdir().unwrap();
        let mut used = HashSet::new();
        {
            let addresses = Ipv4Addresses::new(dir.path(), v4("10.77.0.0"), 30);
            assert_eq!(take(&addresses, &mut used), v4("10.77.0.1"));
            assert_eq!(take(&addresses, &mut used), v4("10.77.0.2"));
            for address in ["10.77.0.2", "10.77.0.1"] {
                used.remove(&v4(address));
                addresses.free(v4(address), at(7)).unwrap();
            }
        }
        let again = Ipv4Addresses::new(dir.path(), v4("10.77.0.0"), 30);
        assert_eq!(take(&again, &mut used), v4("10.77.0.2"));
        assert_eq!(take(&again, &mut used), v4("10.77.0.1"));
        // A /30 has two addresses for pods.
        assert!(again.allocate(&used).is_err());
        // Another range forgets what was kept of the old one.
        let other = Ipv4Addresses::new(dir.path(), v4("10.78.0.0"), 30);
        assert_eq!(other.allocate(&HashSet::new()).unwrap(), v4("10.78.0.1"));
    }

    #[test]
    fn ipv4_addresses_leave_the_first_and_last_out() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Ipv4Addresses::new(dir.path(), v4("169.254.16.0"), 20);
        for (address, pod) in [
            ("169.254.16.0", false),
            ("169.254.16.1", true),
            ("169.254.31.254", true),
            ("169.254.31.255", false),
            ("169.254.32.1", false),
            ("169.254.0.53", false),
        ] {
            assert_eq!(addresses.contains(v4(address)), pod, "{address}");
        }
    }

    #[test]
    fn ipv4_addresses_go_past_those_pods_hold() {
        let dir = tempfile::tempdir().unwrap();
        let addresses = Ipv4Addresses::new(dir.path(), v4("169.254.16.0"), 20);
        // The file is gone, the pods are not.
        let mut used = HashSet::from([v4("169.254.16.1"), v4("169.254.16.3")]);
        assert_eq!(take(&addresses, &mut used), v4("169.254.16.2"));
        assert_eq!(take(&addresses, &mut used), v4("169.254.16.4"));
    }

    #[test]
    fn ipv4_addresses_are_reconciled_with_the_pods() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join("ipv4-addresses"),
            "next 169.254.16.6\nfreed 169.254.16.3 10\nfreed 169.254.16.2 20\nfreed 10.200.0.2 30\nnonsense\n",
        )
        .unwrap();
        let addresses = Ipv4Addresses::new(dir.path(), v4("169.254.16.0"), 20);
        // .1 and .3 are pods'; .4 and .5 were handed out, but their pods
        // are gone and nothing recorded it: they are free from now on.
        let used = HashSet::from([v4("169.254.16.1"), v4("169.254.16.3")]);
        addresses.reconcile(&used, at(40)).unwrap();
        assert_eq!(
            fs::read_to_string(dir.path().join("ipv4-addresses")).unwrap(),
            "next 169.254.16.6\nfreed 169.254.16.2 20\nfreed 169.254.16.4 40\nfreed 169.254.16.5 40\n"
        );
        let mut used = used;
        assert_eq!(take(&addresses, &mut used), v4("169.254.16.6"));
    }
}
