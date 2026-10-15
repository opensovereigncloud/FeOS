// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Packets that nftables logs to an nflog group: the drop rules' (group
//! 2, as `Drop` records), and the copies of the first packets of the
//! TCP connections that pods open (group 3, for their server names).

use crate::netlink::{self, attributes, be32, be64, Message, Socket};
use crate::packet::{self, Packet};
use crate::reassembly::Streams;
use crate::server_name::Source;
use crate::{prefix, timestamp, tuple, Context, Pods};
use feos_proto::network_service::{
    traffic_record, Drop as DropRecord, DropReason, ServerName, ServerNameSource, TrafficRecord,
};
use std::io;
use std::net::{IpAddr, Ipv6Addr};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const NFNL_SUBSYS_ULOG: u16 = 4;
const NFULNL_MSG_PACKET: u16 = 0;
const NFULNL_MSG_CONFIG: u16 = 1;
const NFULA_CFG_CMD: u16 = 1;
const NFULA_CFG_MODE: u16 = 2;
const NFULA_CFG_TIMEOUT: u16 = 5;
const NFULNL_CFG_CMD_BIND: u8 = 1;
const NFULNL_COPY_PACKET: u8 = 2;

const NFULA_TIMESTAMP: u16 = 3;
const NFULA_IFINDEX_INDEV: u16 = 4;
const NFULA_IFINDEX_OUTDEV: u16 = 5;
const NFULA_PAYLOAD: u16 = 9;
const NFULA_PREFIX: u16 = 10;

const RECEIVE_BUFFER: usize = 4 << 20;
/// The kernel sends logged packets in batches: of 100 packets, or what
/// it has after this many hundredths of a second (a second by default).
const FLUSH_TIMEOUT: u32 = 10;

/// A packet that nftables logged.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Logged<'a> {
    pub group: u16,
    pub prefix: &'a str,
    /// The interfaces it came in on and was to go out of.
    pub indev: Option<u32>,
    pub outdev: Option<u32>,
    pub time: Option<SystemTime>,
    /// The packet from its network header on, cut at the snap length.
    pub payload: &'a [u8],
}

/// Opens a socket that receives the packets of `group`, at most
/// `copy_range` bytes of each; the rule's snap length may cut them
/// shorter. Fails with EBUSY while another socket has the group.
pub fn open(group: u16, copy_range: u16) -> io::Result<Socket> {
    let mut socket = Socket::open(0)?;
    socket.set_receive_buffer(RECEIVE_BUFFER);
    socket.set_timeout(Duration::from_secs(5))?;
    let kind = NFNL_SUBSYS_ULOG << 8 | NFULNL_MSG_CONFIG;
    let flags = netlink::NLM_F_REQUEST | netlink::NLM_F_ACK;
    let unspec = libc::AF_UNSPEC as u8;
    socket.request(|seq| {
        netlink::request(
            kind,
            flags,
            seq,
            unspec,
            group,
            &[(NFULA_CFG_CMD, &[NFULNL_CFG_CMD_BIND])],
        )
    })?;
    // struct nfulnl_msg_config_mode: copy_range (be32), copy_mode, pad.
    let mut mode = u32::from(copy_range).to_be_bytes().to_vec();
    mode.extend_from_slice(&[NFULNL_COPY_PACKET, 0]);
    socket.request(|seq| {
        netlink::request(kind, flags, seq, unspec, group, &[(NFULA_CFG_MODE, &mode)])
    })?;
    let timeout = FLUSH_TIMEOUT.to_be_bytes();
    socket.request(|seq| {
        netlink::request(
            kind,
            flags,
            seq,
            unspec,
            group,
            &[(NFULA_CFG_TIMEOUT, &timeout)],
        )
    })?;
    Ok(socket)
}

/// Parses a logged packet; None for other messages.
pub fn parse<'a>(message: &Message<'a>) -> Option<Logged<'a>> {
    if message.kind != NFNL_SUBSYS_ULOG << 8 | NFULNL_MSG_PACKET {
        return None;
    }
    let (_, group, attrs) = message.netfilter()?;
    let mut logged = Logged {
        group,
        prefix: "",
        indev: None,
        outdev: None,
        time: None,
        payload: &[],
    };
    for (kind, value) in attributes(attrs) {
        match kind {
            NFULA_PREFIX => {
                let end = value.iter().position(|&b| b == 0).unwrap_or(value.len());
                logged.prefix = std::str::from_utf8(&value[..end])
                    .unwrap_or_default()
                    .trim();
            }
            NFULA_IFINDEX_INDEV => logged.indev = be32(value),
            NFULA_IFINDEX_OUTDEV => logged.outdev = be32(value),
            NFULA_PAYLOAD => logged.payload = value,
            NFULA_TIMESTAMP => {
                // struct nfulnl_msg_packet_timestamp: seconds and
                // microseconds, be64 each.
                let seconds = be64(value).map(Duration::from_secs);
                let micros = value.get(8..).and_then(be64).map(Duration::from_micros);
                logged.time = seconds
                    .zip(micros)
                    .and_then(|(s, us)| UNIX_EPOCH.checked_add(s)?.checked_add(us));
            }
            _ => {}
        }
    }
    Some(logged)
}

/// The reason of a drop rule's prefix.
pub fn reason(prefix: &str) -> DropReason {
    match prefix {
        prefix::NO_RULE => DropReason::NoRule,
        prefix::DENY => DropReason::DeniedPrefix,
        prefix::SPOOFED => DropReason::Spoofed,
        _ => DropReason::Unspecified,
    }
}

/// The record of a dropped packet, if it is a pod's. The pod sent it if
/// it came in on the pod's TAP, or from the pod's address; else it was
/// for the pod, if it was to go out on the pod's TAP, or to its address.
/// A spoofed packet has the pod's TAP, not its address, and may be IPv4.
pub fn drop_record(
    logged: &Logged,
    packet: &Packet,
    pods: &dyn Pods,
    interface: &dyn Fn(u32) -> Option<String>,
    nat64: &Ipv6Addr,
    now: SystemTime,
) -> Option<TrafficRecord> {
    let tap = |index: Option<u32>| pods.by_interface(&interface(index?)?);
    let address = |address: IpAddr| match address {
        IpAddr::V6(address) => pods.by_address(&address),
        IpAddr::V4(_) => None,
    };
    let (pod_id, outbound) = tap(logged.indev)
        .or_else(|| address(packet.tuple.source))
        .map(|pod| (pod, true))
        .or_else(|| {
            tap(logged.outdev)
                .or_else(|| address(packet.tuple.destination))
                .map(|pod| (pod, false))
        })?;
    Some(TrafficRecord {
        time: Some(timestamp(logged.time.unwrap_or(now))),
        pod_id,
        missed: 0,
        record: Some(traffic_record::Record::Drop(DropRecord {
            tuple: Some(tuple(&packet.tuple, nat64)),
            outbound,
            reason: reason(logged.prefix).into(),
        })),
    })
}

/// The name of an interface, by its index.
fn interface(index: u32) -> Option<String> {
    nix::net::if_::if_indextoname(index)
        .ok()?
        .into_string()
        .ok()
}

/// Reads dropped packets until the collector stops.
pub fn run_drops(socket: Socket, context: &Context) {
    crate::read(&socket, context, |message| {
        let Some(logged) = parse(&message) else {
            return;
        };
        let Some(packet) = packet::parse(logged.payload) else {
            return;
        };
        let pods = context.pods.as_ref();
        let now = SystemTime::now();
        if let Some(record) = drop_record(&logged, &packet, pods, &interface, &context.nat64, now) {
            context.send(record);
        }
    });
}

/// Reads copies of first packets until the collector stops, and records
/// the server names they tell.
pub fn run_copies(socket: Socket, context: &Context) {
    let mut streams = Streams::default();
    crate::read(&socket, context, |message| {
        let Some(logged) = parse(&message) else {
            return;
        };
        if let Some(record) = copy(
            &mut streams,
            &logged,
            context,
            Instant::now(),
            SystemTime::now(),
        ) {
            context.send(record);
        }
    });
}

/// Takes a copy of a packet that a pod sent on a connection it opened.
pub fn copy(
    streams: &mut Streams,
    logged: &Logged,
    context: &Context,
    now: Instant,
    time: SystemTime,
) -> Option<TrafficRecord> {
    let packet = packet::parse(logged.payload)?;
    let segment = packet.segment?;
    let (IpAddr::V6(source), IpAddr::V6(destination)) =
        (packet.tuple.source, packet.tuple.destination)
    else {
        return None;
    };
    // The pod's side of the connection; the server's answers do not name
    // the server.
    let pod_id = context.pods.by_address(&source)?;
    let key = (
        source,
        packet.tuple.source_port,
        destination,
        packet.tuple.destination_port,
    );
    let (source, name) = streams.segment(now, key, segment.seq, segment.syn, segment.payload)?;
    let source = match source {
        Source::Tls => ServerNameSource::Tls,
        Source::Http => ServerNameSource::Http,
    };
    Some(TrafficRecord {
        time: Some(timestamp(time)),
        pod_id,
        missed: 0,
        record: Some(traffic_record::Record::ServerName(ServerName {
            tuple: Some(tuple(&packet.tuple, &context.nat64)),
            source: source.into(),
            name,
        })),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::conntrack::tests::Map;
    use crate::packet::tests::tcp;
    use crate::server_name::tests::{browser_like, client_hello};
    use crate::testdata;
    use feos_proto::network_service::Tuple as RecordTuple;
    use std::collections::HashMap;
    use std::sync::Arc;
    use tokio::sync::broadcast;

    const NAT64: Ipv6Addr = Ipv6Addr::new(0x64, 0xff9b, 0, 0, 0, 0, 0, 0);

    fn pod() -> Ipv6Addr {
        "fd00:feed::1".parse().unwrap()
    }

    fn pods() -> Map {
        Map(HashMap::from([(pod(), "pod-a".to_string())]))
    }

    fn logged(bytes: &[u8]) -> Logged<'_> {
        parse(&netlink::messages(bytes).next().unwrap()).unwrap()
    }

    #[test]
    fn a_dropped_packet() {
        let logged = logged(testdata::NFLOG_DROP_UDP);
        assert_eq!((logged.group, logged.prefix), (2, "deny"));
        assert!(logged.outdev.is_some());
        let packet = packet::parse(logged.payload).unwrap();
        let record = drop_record(
            &logged,
            &packet,
            &pods(),
            &|_| None,
            &NAT64,
            SystemTime::now(),
        )
        .unwrap();
        assert_eq!(record.pod_id, "pod-a");
        let Some(traffic_record::Record::Drop(drop)) = record.record else {
            panic!("not a drop");
        };
        assert!(drop.outbound);
        assert_eq!(drop.reason(), DropReason::DeniedPrefix);
        assert_eq!(
            drop.tuple,
            Some(RecordTuple {
                protocol: 17,
                source_address: "fd00:feed::1".to_string(),
                source_port: u32::from(packet.tuple.source_port),
                destination_address: "192.0.2.2".to_string(),
                destination_port: 9999,
            })
        );
    }

    /// A pod on its TAP, whatever the source address says.
    struct Tap;

    impl Pods for Tap {
        fn by_address(&self, _: &Ipv6Addr) -> Option<String> {
            None
        }

        fn by_interface(&self, name: &str) -> Option<String> {
            (name == "pod-0123abcd").then(|| "spoofer".to_string())
        }
    }

    #[test]
    fn spoofed_packets_are_the_tap_s_pod_s() {
        let mut logged = logged(testdata::NFLOG_DROP_UDP);
        logged.prefix = "spoofed";
        logged.indev = Some(7);
        let packet = packet::parse(logged.payload).unwrap();
        let name = |index| (index == 7).then(|| "pod-0123abcd".to_string());
        let record = drop_record(&logged, &packet, &Tap, &name, &NAT64, SystemTime::now()).unwrap();
        assert_eq!(record.pod_id, "spoofer");
        let Some(traffic_record::Record::Drop(drop)) = record.record else {
            panic!("not a drop");
        };
        assert!(drop.outbound);
        assert_eq!(drop.reason(), DropReason::Spoofed);
        // Without the TAP, the address is nobody's.
        logged.indev = None;
        assert!(drop_record(&logged, &packet, &Tap, &name, &NAT64, SystemTime::now()).is_none());

        // An IPv4 packet: UDP from 10.1.2.3:68 to 255.255.255.255:67.
        let mut ipv4 = vec![0x45, 0, 0, 28, 0, 0, 0, 0, 64, 17, 0, 0, 10, 1, 2, 3];
        ipv4.extend_from_slice(&[255, 255, 255, 255, 0, 68, 0, 67, 0, 8, 0, 0]);
        logged.payload = &ipv4;
        logged.indev = Some(7);
        let packet = packet::parse(logged.payload).unwrap();
        let record = drop_record(&logged, &packet, &Tap, &name, &NAT64, SystemTime::now()).unwrap();
        let Some(traffic_record::Record::Drop(drop)) = record.record else {
            panic!("not a drop");
        };
        let tuple = drop.tuple.unwrap();
        assert_eq!(tuple.source_address, "10.1.2.3");
        assert_eq!(
            (tuple.destination_address.as_str(), tuple.destination_port),
            ("255.255.255.255", 67)
        );
    }

    #[test]
    fn reasons_of_prefixes() {
        assert_eq!(reason("no-rule"), DropReason::NoRule);
        assert_eq!(reason("deny"), DropReason::DeniedPrefix);
        assert_eq!(reason("spoofed"), DropReason::Spoofed);
        assert_eq!(reason("copy"), DropReason::Unspecified);
    }

    fn context() -> Context {
        let (records, _) = broadcast::channel(16);
        Context::new(Arc::new(pods()), NAT64, records)
    }

    #[test]
    fn a_copied_client_hello_from_the_kernel() {
        let context = context();
        let mut streams = Streams::default();
        let now = Instant::now();
        let mut names = Vec::new();
        let mut packets = 0;
        for message in netlink::messages(testdata::NFLOG_COPIES) {
            // A batch ends with NLMSG_DONE.
            let Some(logged) = parse(&message) else {
                continue;
            };
            assert_eq!((logged.group, logged.prefix), (3, "copy"));
            names.extend(copy(
                &mut streams,
                &logged,
                &context,
                now,
                SystemTime::now(),
            ));
            packets += 1;
        }
        assert!(packets >= 4);
        assert_eq!(names.len(), 1);
        assert_eq!(names[0].pod_id, "pod-a");
        let Some(traffic_record::Record::ServerName(name)) = &names[0].record else {
            panic!("not a server name");
        };
        assert_eq!(name.name, "kernel.test.example");
        assert_eq!(name.source(), ServerNameSource::Tls);
        let tuple = name.tuple.as_ref().unwrap();
        assert_eq!(
            (tuple.destination_address.as_str(), tuple.destination_port),
            ("192.0.2.2", 8443)
        );
    }

    #[test]
    fn copies_out_of_order() {
        let context = context();
        let mut streams = Streams::default();
        let server: Ipv6Addr = "64:ff9b::102:304".parse().unwrap();
        let flight = browser_like(&client_hello("copies.example", true));
        let segment = |seq: u32, flags: u8, payload: &[u8]| {
            tcp(pod(), 40000, server, 443, seq, flags, payload)
        };
        let packets = [
            segment(9, 0x02, &[]),
            segment(1010, 0x18, &flight[1000..]),
            // The server's side is not read.
            tcp(server, 443, pod(), 40000, 5, 0x12, &[]),
            segment(10, 0x10, &flight[..1000]),
            segment(10, 0x10, &flight[..1000]),
        ];
        let mut names = Vec::new();
        for packet in &packets {
            let logged = Logged {
                group: 3,
                prefix: "copy",
                indev: None,
                outdev: None,
                time: None,
                payload: packet,
            };
            names.extend(copy(
                &mut streams,
                &logged,
                &context,
                Instant::now(),
                SystemTime::now(),
            ));
        }
        assert_eq!(names.len(), 1);
        let Some(traffic_record::Record::ServerName(name)) = &names[0].record else {
            panic!("not a server name");
        };
        assert_eq!(name.name, "copies.example");
        assert_eq!(name.tuple.as_ref().unwrap().destination_address, "1.2.3.4");
    }

    #[test]
    fn garbage_is_no_packet() {
        for vector in [testdata::NFLOG_DROP_UDP, testdata::NFLOG_COPIES] {
            for len in 0..vector.len() {
                let mut bytes = vector[..len].to_vec();
                if len >= 4 {
                    bytes[..4].copy_from_slice(&(len as u32).to_ne_bytes());
                }
                if let Some(message) = netlink::messages(&bytes).next()
                    && let Some(logged) = parse(&message)
                {
                    let _ = packet::parse(logged.payload);
                }
            }
        }
    }
}
