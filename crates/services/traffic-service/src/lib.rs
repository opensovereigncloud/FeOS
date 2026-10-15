// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Traffic records (ADR 0013): what the node's pods did on the network,
//! as feosd sees it from the host, and `feos.network.v1.TrafficService`,
//! which streams them.
//!
//! - **Flows** come from connection tracking's events, NEW and DESTROY,
//!   over ctnetlink (`conntrack`): a record when a flow begins, and one
//!   with its counters when it ends.
//! - **Drops** come from the drop rules, which log to nflog group
//!   [`DROP_GROUP`] with a prefix that says why ([`prefix`]).
//! - **Server names** come from copies of the first packets of every TCP
//!   connection a pod opens, logged to nflog group [`COPY_GROUP`]: their
//!   first bytes are put together (`reassembly`) and read for a TLS
//!   ClientHello's server name or an HTTP request's Host
//!   (`server_name`).
//!
//! Records go to a broadcast channel, from which [`TrafficApi`] streams
//! them; DNS records come from feosd's resolver into the same channel.
//! Nothing here holds up traffic: what the kernel cannot hand over in time
//! is dropped, and counted in the `missed` of the next record.
//!
//! An IPv4 address under the NAT64 prefix is written as IPv4. A flow or
//! packet of IPv4 (the node's side of `--nat64=node`) is nobody's.

mod api;
mod conntrack;
mod netlink;
mod nflog;
mod packet;
mod reassembly;
mod server_name;

#[cfg(test)]
mod kernel_tests;
#[cfg(test)]
mod testdata;

pub use api::TrafficApi;

use feos_proto::network_service::{TrafficRecord, Tuple};
use netlink::{Message, Socket};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::broadcast;

/// The nflog group of the drop rules.
pub const DROP_GROUP: u16 = 2;
/// The nflog group of the copies of the first packets of the TCP
/// connections that pods open.
pub const COPY_GROUP: u16 = 3;
/// The snap length the copy rule needs: a post-quantum ClientHello (about
/// 1.8 KB) with its headers, if it comes in one segment.
pub const COPY_SNAPLEN: u32 = 2048;

/// The prefixes of the nflog rules: of a drop rule, why it dropped.
pub mod prefix {
    /// No rule of the pod's security groups allowed the packet.
    pub const NO_RULE: &str = "no-rule";
    /// Its destination is in a denied prefix (`--deny-prefix`).
    pub const DENY: &str = "deny";
    /// Its source address was not the pod's.
    pub const SPOOFED: &str = "spoofed";
    /// A copy of one of the first packets of a connection.
    pub const COPY: &str = "copy";
}

/// The bytes of a dropped packet that are read: the IPv6 header, some
/// extension headers and the ports.
const DROP_COPY_RANGE: u16 = 256;
/// How often the readers look up from the socket: to stop, and to count
/// what the kernel dropped.
const TICK: Duration = Duration::from_secs(1);
/// What one `recv` returns: an event, or a batch of logged packets, which
/// is at most one packet of 64 KiB unless nflog's buffer is larger.
const RECEIVE_SIZE: usize = 128 << 10;

/// Which pod an address or a TAP is, for the collector's threads.
pub trait Pods: Send + Sync {
    /// The ID of the pod with this address.
    fn by_address(&self, address: &Ipv6Addr) -> Option<String>;

    /// The ID of the pod whose TAP this is (`pod-<8 hex>`): a spoofed
    /// packet's source address is not the pod's.
    fn by_interface(&self, _name: &str) -> Option<String> {
        None
    }
}

#[derive(Clone, Debug)]
pub struct Config {
    /// The NAT64 prefix, a /96 (ADR 0010).
    pub nat64_prefix: Ipv6Addr,
    pub drop_group: u16,
    pub copy_group: u16,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            nat64_prefix: Ipv6Addr::new(0x64, 0xff9b, 0, 0, 0, 0, 0, 0),
            drop_group: DROP_GROUP,
            copy_group: COPY_GROUP,
        }
    }
}

/// The readers of conntrack events and nflog groups, a thread each. They
/// stop when the collector is dropped.
pub struct Collector {
    context: Arc<Context>,
    threads: Vec<JoinHandle<()>>,
}

impl Collector {
    /// Opens the sockets, in the calling thread's network namespace, and
    /// starts reading them. Fails if a socket cannot be opened, or an nflog
    /// group is bound by another socket (EBUSY).
    pub fn start(
        pods: Arc<dyn Pods>,
        config: Config,
        records: broadcast::Sender<TrafficRecord>,
    ) -> io::Result<Self> {
        let flows = conntrack::open()?;
        let drops = nflog::open(config.drop_group, DROP_COPY_RANGE)?;
        let copies = nflog::open(config.copy_group, u16::MAX)?;
        for socket in [&flows, &drops, &copies] {
            socket.set_timeout(TICK)?;
        }
        let mut collector = Self {
            context: Arc::new(Context::new(pods, config.nat64_prefix, records)),
            threads: Vec::new(),
        };
        let readers: [(&str, Socket, Reader); 3] = [
            ("traffic-flows", flows, conntrack::run),
            ("traffic-drops", drops, nflog::run_drops),
            ("traffic-copies", copies, nflog::run_copies),
        ];
        for (name, socket, run) in readers {
            let context = collector.context.clone();
            // On an error, dropping the collector stops the threads so far.
            let thread = thread::Builder::new()
                .name(name.to_string())
                .spawn(move || run(socket, &context))?;
            collector.threads.push(thread);
        }
        Ok(collector)
    }

    /// Stops the readers, and waits for them: up to a second.
    pub fn stop(mut self) {
        self.context.stop.store(true, Ordering::Relaxed);
        for thread in self.threads.drain(..) {
            let _ = thread.join();
        }
    }
}

impl Drop for Collector {
    fn drop(&mut self) {
        self.context.stop.store(true, Ordering::Relaxed);
    }
}

/// Turns on what flow records need of connection tracking, in the calling
/// thread's network namespace: counters (`nf_conntrack_acct`) and start
/// and end times (`nf_conntrack_timestamp`). They apply to the
/// connections tracked from then on. The nf_conntrack module must be
/// loaded, which a table with a `ct` expression does.
pub fn enable_accounting() -> io::Result<()> {
    for name in ["nf_conntrack_acct", "nf_conntrack_timestamp"] {
        std::fs::write(format!("/proc/sys/net/netfilter/{name}"), "1")?;
    }
    Ok(())
}

/// A reader's loop over its socket.
type Reader = fn(Socket, &Context);

/// What the readers share.
pub(crate) struct Context {
    pods: Arc<dyn Pods>,
    nat64: Ipv6Addr,
    records: broadcast::Sender<TrafficRecord>,
    /// Lost since the last record.
    missed: AtomicU64,
    stop: AtomicBool,
}

impl Context {
    fn new(
        pods: Arc<dyn Pods>,
        nat64: Ipv6Addr,
        records: broadcast::Sender<TrafficRecord>,
    ) -> Self {
        Self {
            pods,
            nat64,
            records,
            missed: AtomicU64::new(0),
            stop: AtomicBool::new(false),
        }
    }

    /// Sends a record, with what was missed before it. With nobody
    /// listening, it goes nowhere.
    fn send(&self, mut record: TrafficRecord) {
        record.missed += self.missed.swap(0, Ordering::Relaxed);
        let _ = self.records.send(record);
    }

    fn lost(&self, count: u64) {
        if count > 0 {
            self.missed.fetch_add(count, Ordering::Relaxed);
        }
    }
}

/// Reads a socket's messages until the collector stops, and counts what
/// the kernel dropped for it: the socket's drops, or one for each ENOBUFS
/// where they cannot be read.
fn read(socket: &Socket, context: &Context, mut handle: impl FnMut(Message)) {
    let mut buf = vec![0u8; RECEIVE_SIZE];
    let mut drops = socket.drops();
    let mut counted = Instant::now();
    let count = |drops: &mut Option<u64>, overrun: bool| {
        let now = socket.drops();
        let lost = match (*drops, now) {
            (Some(before), Some(now)) => now.saturating_sub(before),
            _ => u64::from(overrun),
        };
        *drops = now.or(*drops);
        context.lost(lost);
    };
    while !context.stop.load(Ordering::Relaxed) {
        match socket.recv(&mut buf) {
            Ok(n) => netlink::messages(&buf[..n]).for_each(&mut handle),
            Err(e) if e.raw_os_error() == Some(libc::ENOBUFS) => {
                count(&mut drops, true);
                counted = Instant::now();
            }
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
                ) => {}
            Err(e) => {
                log::warn!("traffic records: cannot read netlink: {e}");
                thread::sleep(TICK);
            }
        }
        if counted.elapsed() >= TICK {
            count(&mut drops, false);
            counted = Instant::now();
        }
    }
}

/// A time as records have it; the epoch for one before it.
fn timestamp(time: SystemTime) -> prost_types::Timestamp {
    let since = time.duration_since(UNIX_EPOCH).unwrap_or_default();
    prost_types::Timestamp {
        seconds: i64::try_from(since.as_secs()).unwrap_or(i64::MAX),
        nanos: since.subsec_nanos() as i32,
    }
}

/// A tuple as records have it.
fn tuple(tuple: &packet::Tuple, nat64: &Ipv6Addr) -> Tuple {
    Tuple {
        protocol: tuple.protocol.into(),
        source_address: address(tuple.source, nat64),
        source_port: tuple.source_port.into(),
        destination_address: address(tuple.destination, nat64),
        destination_port: tuple.destination_port.into(),
    }
}

/// An address as records have it: IPv4 when it is under the NAT64 /96.
fn address(address: IpAddr, nat64: &Ipv6Addr) -> String {
    match address {
        IpAddr::V6(v6) if v6.octets()[..12] == nat64.octets()[..12] => {
            let o = v6.octets();
            Ipv4Addr::new(o[12], o[13], o[14], o[15]).to_string()
        }
        address => address.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nat64_addresses_are_ipv4() {
        let nat64 = Config::default().nat64_prefix;
        let a = |s: &str| address(s.parse().unwrap(), &nat64);
        assert_eq!(a("64:ff9b::102:304"), "1.2.3.4");
        assert_eq!(a("64:ff9b::1:102:304"), "64:ff9b::1:102:304");
        assert_eq!(a("2001:db8::1"), "2001:db8::1");
        assert_eq!(a("10.0.0.1"), "10.0.0.1");
        let other: Ipv6Addr = "2001:db8:64::".parse().unwrap();
        assert_eq!(
            address("2001:db8:64::c000:201".parse().unwrap(), &other),
            "192.0.2.1"
        );
    }

    #[test]
    fn missed_records_go_with_the_next() {
        struct Nobody;
        impl Pods for Nobody {
            fn by_address(&self, _: &Ipv6Addr) -> Option<String> {
                None
            }
        }
        let (records, mut rx) = broadcast::channel(4);
        let context = Context::new(Arc::new(Nobody), Config::default().nat64_prefix, records);
        context.lost(3);
        context.lost(2);
        context.send(TrafficRecord::default());
        context.send(TrafficRecord::default());
        assert_eq!(rx.try_recv().unwrap().missed, 5);
        assert_eq!(rx.try_recv().unwrap().missed, 0);
    }
}
