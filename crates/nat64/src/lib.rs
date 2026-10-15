// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! NAT64 on the node (ADR 0010, `--nat64=node`). Pods are IPv6 only and
//! reach IPv4 through a stateless translator (SIIT, RFC 7915) with an IPv4
//! address of the node's own pool per pod (RFC 7757): an eBPF program on
//! the egress of the node's `feos-nat64` device. The node routes the NAT64
//! prefix and the pool into the device, the program translates, and hands
//! the packet back to the node, which forwards it. feosd's nftables
//! masquerade the pool to the node's address; the pool never leaves the
//! node.
//!
//! The device, its routes, the program and its pinned maps outlive feosd:
//! the pods' mappings go on translating while feosd restarts, and the new
//! feosd takes them over with [`Nat64::attach`].
//!
//! The program is in `crates/nat64-ebpf`; `make nat64-bpf` builds it into
//! `bpf/nat64.bpf.o`, which this crate embeds.

mod net;
mod sysctl;

pub use sysctl::{enable_forwarding, keep_router_advertisements};

use aya::maps::{HashMap, Map, MapData, MapError, MapInfo, MapType, PerCpuArray};
use aya::programs::tc::{
    self, NlOptions, SchedClassifierLink, TcAttachOptions, TcAttachType, TcError, TcHandle,
};
use aya::programs::{ProgramError, SchedClassifier};
use aya::{Ebpf, EbpfLoader};
use log::{info, warn};
use std::io;
use std::net::{Ipv4Addr, Ipv6Addr};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

/// The translator's device.
pub const DEVICE: &str = "feos-nat64";
/// The well-known NAT64 prefix (RFC 6052), a /96.
pub const PREFIX: Ipv6Addr = Ipv6Addr::new(0x64, 0xff9b, 0, 0, 0, 0, 0, 0);
/// The pods' IPv4 pool.
pub const POOL: Ipv4Addr = Ipv4Addr::new(10, 200, 0, 0);
pub const POOL_LEN: u8 = 16;
/// Where the maps are pinned.
pub const PIN_DIR: &str = "/sys/fs/bpf/feos/nat64";

/// The program, built by `make nat64-bpf`.
static PROGRAM: &[u8] = aya::include_bytes_aligned!("../bpf/nat64.bpf.o");

const POD_IPV4: &str = "nat64_pod_ipv4";
const IPV4_POD: &str = "nat64_ipv4_pod";
const STATS: &str = "nat64_stats";

/// The pinned maps: name, type, key size, value size, entries.
const PINS: [(&str, MapType, u32, u32, u32); 3] = [
    (POD_IPV4, MapType::Hash, 16, 4, 65536),
    (IPV4_POD, MapType::Hash, 4, 16, 65536),
    (STATS, MapType::PerCpuArray, 4, 8, 8),
];

/// The filter's place on the device's egress. A new program replaces the
/// old one there, in one step.
const FILTER_PRIORITY: u16 = 1;
const FILTER_HANDLE: TcHandle = TcHandle::new(0, 1);

/// Where NAT64 translates.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Config {
    /// The NAT64 prefix, a /96: its last 32 bits are zero.
    pub prefix: Ipv6Addr,
    /// The pods' IPv4 pool, a network address. Its first address stands
    /// for the node itself; no pod gets it.
    pub pool: Ipv4Addr,
    pub pool_len: u8,
    /// The directory of the pinned maps, on a bpf filesystem.
    pub pin_dir: PathBuf,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            prefix: PREFIX,
            pool: POOL,
            pool_len: POOL_LEN,
            pin_dir: PathBuf::from(PIN_DIR),
        }
    }
}

impl Config {
    /// Checks that the prefix is a /96 and the pool a network of its own.
    pub fn validate(&self) -> io::Result<()> {
        if self.prefix.octets()[12..] != [0; 4] {
            return Err(invalid(format!(
                "NAT64 prefix {}: the last 32 bits of a /96 are zero",
                self.prefix
            )));
        }
        // Room for the node's address and a pod.
        if !(1..=30).contains(&self.pool_len) {
            return Err(invalid(format!(
                "NAT64 pool {}/{}: the length is 1 to 30",
                self.pool, self.pool_len
            )));
        }
        if u32::from(self.pool) & !mask(self.pool_len) != 0 {
            return Err(invalid(format!(
                "NAT64 pool {}/{}: not a network address",
                self.pool, self.pool_len
            )));
        }
        Ok(())
    }

    /// The pool's first address, which stands for the node: ICMP errors
    /// from the node and other IPv6 routers come from it.
    pub fn node_ipv4(&self) -> Ipv4Addr {
        Ipv4Addr::from(u32::from(self.pool) + 1)
    }

    /// Whether the address is in the pool.
    pub fn contains(&self, ipv4: Ipv4Addr) -> bool {
        u32::from(ipv4) & mask(self.pool_len) == u32::from(self.pool)
    }
}

/// `prefix + a.b.c.d` (RFC 6052, a /96).
pub fn embed(prefix: Ipv6Addr, ipv4: Ipv4Addr) -> Ipv6Addr {
    let mut octets = prefix.octets();
    octets[12..].copy_from_slice(&ipv4.octets());
    Ipv6Addr::from(octets)
}

/// `a.b.c.d` of `prefix + a.b.c.d`, if the address is under the prefix.
pub fn extract(prefix: Ipv6Addr, ipv6: Ipv6Addr) -> Option<Ipv4Addr> {
    let octets = ipv6.octets();
    (octets[..12] == prefix.octets()[..12])
        .then(|| Ipv4Addr::new(octets[12], octets[13], octets[14], octets[15]))
}

fn mask(len: u8) -> u32 {
    u32::MAX.checked_shl(32 - u32::from(len)).unwrap_or(0)
}

fn invalid(message: String) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}

/// The translator's counters, summed over CPUs, since its maps were
/// pinned.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Stats {
    /// Packets translated from IPv6 to IPv4.
    pub six_to_four: u64,
    /// Packets translated from IPv4 to IPv6.
    pub four_to_six: u64,
    /// Dropped: no pod has the address.
    pub no_mapping: u64,
    /// Dropped: a destination outside the prefix, or one that no pod
    /// reaches through NAT64 (the pool, loopback, link-local, multicast).
    pub denied: u64,
    /// Dropped: fragments, IPv4 options, IPv6 extension headers, other
    /// protocols and ICMP types that do not translate.
    pub unsupported: u64,
    /// Dropped: truncated or malformed packets, failed helpers.
    pub errors: u64,
    /// IPv4 UDP datagrams without a checksum that got one.
    pub udp_checksums: u64,
}

/// The translator on this node: the maps of its program.
pub struct Nat64 {
    config: Config,
    maps: Mutex<Maps>,
}

struct Maps {
    pods: HashMap<MapData, [u8; 16], [u8; 4]>,
    addresses: HashMap<MapData, [u8; 4], [u8; 16]>,
    stats: PerCpuArray<MapData, u64>,
}

impl Nat64 {
    /// Creates or takes over `feos-nat64`: the device, its routes, the
    /// sysctls it needs (IPv4 and IPv6 forwarding on, see
    /// [`enable_forwarding`]; a loose reverse path filter and no router
    /// advertisements on the device), the clsact qdisc and the program on
    /// its egress, and the pinned maps. A program that is there already is
    /// replaced in one step and its mappings stay. Idempotent.
    pub async fn attach(config: &Config) -> io::Result<Self> {
        config.validate()?;
        net::prepare_bpffs(&config.pin_dir)?;
        drop_incompatible_pins(&config.pin_dir);
        let mut ebpf = load(config)?;

        let handle = net::connect()?;
        let index = net::ensure_device(&handle).await?;
        sysctl::device(DEVICE)?;
        match tc::qdisc_add_clsact(DEVICE) {
            Ok(()) => {}
            Err(TcError::NetlinkError(e)) if e.raw_os_error() == Some(libc::EEXIST) => {}
            Err(e) => return Err(io::Error::other(format!("NAT64: clsact on {DEVICE}: {e}"))),
        }
        attach_program(&mut ebpf)?;
        net::set_routes(&handle, index, config).await?;
        enable_forwarding()?;

        let maps = Maps::take(&mut ebpf)?;
        info!(
            "NAT64: {} translates {}/96 for the pool {}/{}",
            DEVICE, config.prefix, config.pool, config.pool_len
        );
        Ok(Self {
            config: config.clone(),
            maps: Mutex::new(maps),
        })
    }

    /// Opens the pinned maps of an attached translator, without touching
    /// the device or the program.
    pub fn open(config: &Config) -> io::Result<Self> {
        config.validate()?;
        let open = |name: &str| {
            let path = config.pin_dir.join(name);
            MapData::from_pin(&path)
                .and_then(Map::from_map_data)
                .map_err(|e| map_error(&format!("open {}", path.display()), e))
        };
        let maps = Maps::new(open(POD_IPV4)?, open(IPV4_POD)?, open(STATS)?)?;
        Ok(Self {
            config: config.clone(),
            maps: Mutex::new(maps),
        })
    }

    /// Gives the pod an IPv4 address of the pool. A pod that had another
    /// gives that up; an address that another pod holds is refused.
    pub fn map(&self, pod: Ipv6Addr, ipv4: Ipv4Addr) -> io::Result<()> {
        if !self.config.contains(ipv4) || ipv4 == self.config.node_ipv4() {
            return Err(invalid(format!(
                "NAT64: {ipv4} is not a pod's address of {}/{}",
                self.config.pool, self.config.pool_len
            )));
        }
        let mut maps = self.lock()?;
        let (pod_key, ipv4_key) = (pod.octets(), ipv4.octets());
        match maps.addresses.get(&ipv4_key, 0) {
            Ok(holder) if holder != pod_key => {
                return Err(io::Error::new(
                    io::ErrorKind::AlreadyExists,
                    format!("NAT64: {ipv4} is {}'s", Ipv6Addr::from(holder)),
                ));
            }
            Ok(_) | Err(MapError::KeyNotFound) => {}
            Err(e) => return Err(map_error("look up an address", e)),
        }
        match maps.pods.get(&pod_key, 0) {
            Ok(old) if old != ipv4_key => remove(&mut maps.addresses, &old)?,
            Ok(_) | Err(MapError::KeyNotFound) => {}
            Err(e) => return Err(map_error("look up a pod", e)),
        }
        // The way back first: a pod's packets go out only once both are in.
        maps.addresses
            .insert(ipv4_key, pod_key, 0)
            .map_err(|e| map_error("map an address", e))?;
        maps.pods
            .insert(pod_key, ipv4_key, 0)
            .map_err(|e| map_error("map a pod", e))
    }

    /// Takes the pod's IPv4 address away. A pod without one is fine.
    pub fn unmap(&self, pod: Ipv6Addr) -> io::Result<()> {
        let mut maps = self.lock()?;
        let pod_key = pod.octets();
        let ipv4_key = match maps.pods.get(&pod_key, 0) {
            Ok(ipv4) => ipv4,
            Err(MapError::KeyNotFound) => return Ok(()),
            Err(e) => return Err(map_error("look up a pod", e)),
        };
        remove(&mut maps.pods, &pod_key)?;
        match maps.addresses.get(&ipv4_key, 0) {
            Ok(holder) if holder == pod_key => remove(&mut maps.addresses, &ipv4_key),
            Ok(_) | Err(MapError::KeyNotFound) => Ok(()),
            Err(e) => Err(map_error("look up an address", e)),
        }
    }

    /// The pods' IPv4 addresses.
    pub fn mappings(&self) -> io::Result<Vec<(Ipv6Addr, Ipv4Addr)>> {
        let maps = self.lock()?;
        maps.pods
            .iter()
            .map(|entry| {
                entry
                    .map(|(pod, ipv4)| (Ipv6Addr::from(pod), Ipv4Addr::from(ipv4)))
                    .map_err(|e| map_error("list the pods", e))
            })
            .collect()
    }

    pub fn stats(&self) -> io::Result<Stats> {
        let maps = self.lock()?;
        let mut counts = [0u64; 7];
        for (index, count) in (0u32..).zip(counts.iter_mut()) {
            let values = maps
                .stats
                .get(&index, 0)
                .map_err(|e| map_error("read the counters", e))?;
            *count = values.iter().sum();
        }
        let [six_to_four, four_to_six, no_mapping, denied, unsupported, errors, udp_checksums] =
            counts;
        Ok(Stats {
            six_to_four,
            four_to_six,
            no_mapping,
            denied,
            unsupported,
            errors,
            udp_checksums,
        })
    }

    /// Removes the device, which takes its routes and the program with it,
    /// and the pins (for `--nat64=network`). Forwarding stays on.
    pub async fn detach(pin_dir: &Path) -> io::Result<()> {
        let handle = net::connect()?;
        net::delete_device(&handle).await?;
        for (name, ..) in PINS {
            match std::fs::remove_file(pin_dir.join(name)) {
                Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
                _ => {}
            }
        }
        match std::fs::remove_dir(pin_dir) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => {
                warn!("NAT64: keeping {}: {e}", pin_dir.display())
            }
            _ => {}
        }
        info!("NAT64: removed {DEVICE}");
        Ok(())
    }

    fn lock(&self) -> io::Result<MutexGuard<'_, Maps>> {
        self.maps
            .lock()
            .map_err(|_| io::Error::other("NAT64: the maps' lock is poisoned"))
    }
}

impl Maps {
    fn new(pods: Map, addresses: Map, stats: Map) -> io::Result<Self> {
        Ok(Self {
            pods: HashMap::try_from(pods).map_err(|e| map_error(POD_IPV4, e))?,
            addresses: HashMap::try_from(addresses).map_err(|e| map_error(IPV4_POD, e))?,
            stats: PerCpuArray::try_from(stats).map_err(|e| map_error(STATS, e))?,
        })
    }

    fn take(ebpf: &mut Ebpf) -> io::Result<Self> {
        let mut take = |name: &str| {
            ebpf.take_map(name)
                .ok_or_else(|| io::Error::other(format!("NAT64: the program has no map {name}")))
        };
        Self::new(take(POD_IPV4)?, take(IPV4_POD)?, take(STATS)?)
    }
}

fn remove<K: aya::Pod, V: aya::Pod>(map: &mut HashMap<MapData, K, V>, key: &K) -> io::Result<()> {
    match map.remove(key) {
        Ok(()) | Err(MapError::KeyNotFound) => Ok(()),
        // Removing a missing key fails in the syscall with ENOENT.
        Err(MapError::SyscallError(e)) if e.io_error.raw_os_error() == Some(libc::ENOENT) => Ok(()),
        Err(e) => Err(map_error("unmap", e)),
    }
}

fn map_error(what: &str, e: MapError) -> io::Error {
    io::Error::other(format!("NAT64: {what}: {e}"))
}

/// Removes pins that the program cannot take over, from another version of
/// it. Their mappings are lost; feosd maps its pods again.
fn drop_incompatible_pins(pin_dir: &Path) {
    for (name, kind, key, value, entries) in PINS {
        let path = pin_dir.join(name);
        let Ok(info) = MapInfo::from_pin(&path) else {
            continue;
        };
        let fits = matches!(info.map_type(), Ok(t) if t == kind)
            && info.key_size() == key
            && info.value_size() == value
            && info.max_entries() == entries;
        if !fits {
            warn!(
                "NAT64: {} does not fit the program, removing it",
                path.display()
            );
            if let Err(e) = std::fs::remove_file(&path) {
                warn!("NAT64: remove {}: {e}", path.display());
            }
        }
    }
}

/// Loads the program with the configuration in its globals and the pinned
/// maps, which it creates if they are not there.
fn load(config: &Config) -> io::Result<Ebpf> {
    let octets = config.prefix.octets();
    let mut prefix = [0u8; 12];
    prefix.copy_from_slice(&octets[..12]);
    let node = config.node_ipv4().octets();
    let pool = config.pool.octets();
    let pool_mask = mask(config.pool_len).to_be_bytes();
    EbpfLoader::new()
        .default_map_pin_directory(&config.pin_dir)
        .override_global("PREFIX", &prefix, true)
        .override_global("NODE", &node, true)
        .override_global("POOL", &pool, true)
        .override_global("POOL_MASK", &pool_mask, true)
        .load(PROGRAM)
        .map_err(|e| io::Error::other(format!("NAT64: load the program: {e}")))
}

/// Puts the program on the device's egress, in place of an earlier one.
/// The filter stays when this process is gone.
fn attach_program(ebpf: &mut Ebpf) -> io::Result<()> {
    let program: &mut SchedClassifier = ebpf
        .program_mut("nat64")
        .ok_or_else(|| io::Error::other("NAT64: the object has no program nat64"))?
        .try_into()
        .map_err(|e| program_error("the program", e))?;
    program
        .load()
        .map_err(|e| program_error("verify the program", e))?;
    let earlier = SchedClassifierLink::attached(
        DEVICE,
        TcAttachType::Egress,
        FILTER_PRIORITY,
        FILTER_HANDLE,
        None,
    )?;
    let id = match program.attach_to_link(earlier) {
        Ok(id) => id,
        Err(e) if errno(&e) == Some(libc::ENOENT) => program
            .attach_with_options(
                DEVICE,
                TcAttachType::Egress,
                TcAttachOptions::Netlink(NlOptions {
                    priority: FILTER_PRIORITY,
                    handle: FILTER_HANDLE,
                    classid: None,
                }),
            )
            .map_err(|e| program_error("attach the program", e))?,
        Err(e) => return Err(program_error("replace the program", e)),
    };
    // A netlink filter holds no file descriptor: forgetting the link only
    // keeps aya from detaching it.
    let link = program
        .take_link(id)
        .map_err(|e| program_error("keep the program attached", e))?;
    std::mem::forget(link);
    Ok(())
}

fn errno(e: &ProgramError) -> Option<i32> {
    match e {
        ProgramError::TcError(TcError::NetlinkError(e)) => e.raw_os_error(),
        ProgramError::NetlinkError(e) => e.raw_os_error(),
        _ => None,
    }
}

fn program_error(what: &str, e: ProgramError) -> io::Error {
    io::Error::other(format!("NAT64: {what}: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn embeds_and_extracts() {
        let ipv4 = Ipv4Addr::new(192, 0, 2, 33);
        let ipv6 = embed(PREFIX, ipv4);
        assert_eq!(ipv6, "64:ff9b::c000:221".parse::<Ipv6Addr>().unwrap());
        assert_eq!(extract(PREFIX, ipv6), Some(ipv4));
        assert_eq!(extract(PREFIX, "64:ff9c::c000:221".parse().unwrap()), None);
        let local: Ipv6Addr = "fd00:64::".parse().unwrap();
        assert_eq!(
            embed(local, ipv4),
            "fd00:64::c000:221".parse::<Ipv6Addr>().unwrap()
        );
    }

    #[test]
    fn validates() {
        assert!(Config::default().validate().is_ok());
        let config = |prefix: &str, pool: [u8; 4], pool_len| Config {
            prefix: prefix.parse().unwrap(),
            pool: Ipv4Addr::from(pool),
            pool_len,
            ..Config::default()
        };
        assert!(config("64:ff9b::1:0:0", [10, 200, 0, 0], 16)
            .validate()
            .is_ok());
        assert!(config("64:ff9b::1", [10, 200, 0, 0], 16)
            .validate()
            .is_err());
        assert!(config("64:ff9b::", [10, 200, 0, 1], 16).validate().is_err());
        assert!(config("64:ff9b::", [10, 200, 0, 0], 31).validate().is_err());
        assert!(config("64:ff9b::", [10, 200, 0, 0], 0).validate().is_err());
        assert!(config("64:ff9b::", [10, 200, 0, 4], 30).validate().is_ok());
        assert!(config("64:ff9b::", [128, 0, 0, 0], 1).validate().is_ok());
    }

    #[test]
    fn pool() {
        let config = Config::default();
        assert_eq!(config.node_ipv4(), Ipv4Addr::new(10, 200, 0, 1));
        assert!(config.contains(Ipv4Addr::new(10, 200, 255, 254)));
        assert!(!config.contains(Ipv4Addr::new(10, 201, 0, 1)));
        assert_eq!(mask(16), 0xffff_0000);
        assert_eq!(mask(30), 0xffff_fffc);
        assert_eq!(mask(0), 0);
    }
}
