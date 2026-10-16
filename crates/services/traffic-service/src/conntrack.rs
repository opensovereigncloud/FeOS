// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Flows from connection tracking: its events over ctnetlink, a flow's
//! begin (NEW) and its end (DESTROY, with counters and timestamps). And
//! the deletion of an address's connections.

use crate::netlink::{self, attribute, attributes, be16, be32, be64, Message, Socket};
use crate::packet::{Tuple, ICMPV6};
use crate::{timestamp, tuple, Context, Pods};
use feos_proto::network_service::{traffic_record, Flow, TrafficRecord};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const NFNL_SUBSYS_CTNETLINK: u16 = 1;
const IPCTNL_MSG_CT_NEW: u16 = 0;
const IPCTNL_MSG_CT_GET: u16 = 1;
const IPCTNL_MSG_CT_DELETE: u16 = 2;
const NFNLGRP_CONNTRACK_NEW: u32 = 1;
const NFNLGRP_CONNTRACK_DESTROY: u32 = 3;

const CTA_TUPLE_ORIG: u16 = 1;
const CTA_TUPLE_REPLY: u16 = 2;
const CTA_COUNTERS_ORIG: u16 = 9;
const CTA_COUNTERS_REPLY: u16 = 10;
const CTA_ID: u16 = 12;
const CTA_ZONE: u16 = 18;
const CTA_TIMESTAMP: u16 = 20;
const CTA_FILTER: u16 = 25;
const CTA_FILTER_ORIG_FLAGS: u16 = 1;
/// CTA_FILTER_FLAG(CTA_IP_SRC): the original tuple's source address.
const CTA_FILTER_IP_SRC: u32 = 1 << 0;

const CTA_TUPLE_IP: u16 = 1;
const CTA_TUPLE_PROTO: u16 = 2;
const CTA_IP_V4_SRC: u16 = 1;
const CTA_IP_V4_DST: u16 = 2;
const CTA_IP_V6_SRC: u16 = 3;
const CTA_IP_V6_DST: u16 = 4;
const CTA_PROTO_NUM: u16 = 1;
const CTA_PROTO_SRC_PORT: u16 = 2;
const CTA_PROTO_DST_PORT: u16 = 3;
const CTA_PROTO_ICMP_ID: u16 = 4;
const CTA_PROTO_ICMP_TYPE: u16 = 5;
const CTA_PROTO_ICMPV6_ID: u16 = 7;
const CTA_PROTO_ICMPV6_TYPE: u16 = 8;
const CTA_COUNTERS_PACKETS: u16 = 1;
const CTA_COUNTERS_BYTES: u16 = 2;
const CTA_COUNTERS32_PACKETS: u16 = 3;
const CTA_COUNTERS32_BYTES: u16 = 4;
const CTA_TIMESTAMP_START: u16 = 1;
const CTA_TIMESTAMP_STOP: u16 = 2;

const ICMP: u8 = 1;

/// Events come in bursts; the kernel drops what does not fit.
const RECEIVE_BUFFER: usize = 8 << 20;

/// A conntrack event.
#[derive(Clone, Debug, PartialEq)]
pub struct Event {
    pub ended: bool,
    pub original: Tuple,
    pub reply: Option<Tuple>,
    /// Packets and bytes of the original direction, and of the reply's;
    /// only DESTROY has them, with nf_conntrack_acct on.
    pub original_counters: Counters,
    pub reply_counters: Counters,
    /// With nf_conntrack_timestamp on, in DESTROY.
    pub start: Option<SystemTime>,
    pub stop: Option<SystemTime>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Counters {
    pub packets: u64,
    pub bytes: u64,
}

/// Opens a socket that receives NEW and DESTROY events.
pub fn open() -> io::Result<Socket> {
    let groups = 1 << (NFNLGRP_CONNTRACK_NEW - 1) | 1 << (NFNLGRP_CONNTRACK_DESTROY - 1);
    let socket = Socket::open(groups)?;
    socket.set_receive_buffer(RECEIVE_BUFFER);
    Ok(socket)
}

/// Reads events until the collector stops.
pub fn run(socket: Socket, context: &Context) {
    crate::read(&socket, context, |message| {
        let Some(event) = parse(&message) else {
            return;
        };
        for record in records(&event, context.pods.as_ref(), SystemTime::now()) {
            context.send(record);
        }
    });
}

/// Parses a NEW or DESTROY event; None for other messages, and for
/// events without an original tuple.
pub fn parse(message: &Message) -> Option<Event> {
    let ended = match message.kind {
        k if k == NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_NEW => false,
        k if k == NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_DELETE => true,
        _ => return None,
    };
    let (_, _, attrs) = message.netfilter()?;
    let mut event = Event {
        ended,
        original: parse_tuple(attribute(attrs, CTA_TUPLE_ORIG)?)?,
        reply: None,
        original_counters: Counters::default(),
        reply_counters: Counters::default(),
        start: None,
        stop: None,
    };
    for (kind, value) in attributes(attrs) {
        match kind {
            CTA_TUPLE_REPLY => event.reply = parse_tuple(value),
            CTA_COUNTERS_ORIG => event.original_counters = parse_counters(value),
            CTA_COUNTERS_REPLY => event.reply_counters = parse_counters(value),
            CTA_TIMESTAMP => {
                let time = |kind| {
                    let ns = attribute(value, kind).and_then(be64).filter(|&ns| ns > 0)?;
                    UNIX_EPOCH.checked_add(Duration::from_nanos(ns))
                };
                event.start = time(CTA_TIMESTAMP_START);
                event.stop = time(CTA_TIMESTAMP_STOP);
            }
            _ => {}
        }
    }
    Some(event)
}

fn parse_tuple(attrs: &[u8]) -> Option<Tuple> {
    let ip = attribute(attrs, CTA_TUPLE_IP)?;
    let proto = attribute(attrs, CTA_TUPLE_PROTO)?;
    let v6 = |kind| -> Option<IpAddr> {
        let bytes: [u8; 16] = attribute(ip, kind)?.get(..16)?.try_into().ok()?;
        Some(Ipv6Addr::from(bytes).into())
    };
    let v4 = |kind| -> Option<IpAddr> {
        let bytes: [u8; 4] = attribute(ip, kind)?.get(..4)?.try_into().ok()?;
        Some(Ipv4Addr::from(bytes).into())
    };
    let (source, destination) = match (v6(CTA_IP_V6_SRC), v6(CTA_IP_V6_DST)) {
        (Some(s), Some(d)) => (s, d),
        _ => (v4(CTA_IP_V4_SRC)?, v4(CTA_IP_V4_DST)?),
    };
    let protocol = *attribute(proto, CTA_PROTO_NUM)?.first()?;
    let port = |kind| attribute(proto, kind).and_then(be16).unwrap_or(0);
    let kind = |kind| {
        attribute(proto, kind)
            .and_then(|v| v.first().copied())
            .unwrap_or(0)
    };
    let (source_port, destination_port) = match protocol {
        ICMPV6 => (
            port(CTA_PROTO_ICMPV6_ID),
            kind(CTA_PROTO_ICMPV6_TYPE).into(),
        ),
        ICMP => (port(CTA_PROTO_ICMP_ID), kind(CTA_PROTO_ICMP_TYPE).into()),
        _ => (port(CTA_PROTO_SRC_PORT), port(CTA_PROTO_DST_PORT)),
    };
    Some(Tuple {
        protocol,
        source,
        source_port,
        destination,
        destination_port,
    })
}

fn parse_counters(attrs: &[u8]) -> Counters {
    let value = |wide, narrow| {
        attribute(attrs, wide)
            .and_then(be64)
            .or_else(|| attribute(attrs, narrow).and_then(be32).map(u64::from))
            .unwrap_or(0)
    };
    Counters {
        packets: value(CTA_COUNTERS_PACKETS, CTA_COUNTERS32_PACKETS),
        bytes: value(CTA_COUNTERS_BYTES, CTA_COUNTERS32_BYTES),
    }
}

/// The records of an event: one for each end that is a pod. A pod opened
/// the flow if it is the original source; it accepted it if it is the
/// original destination, or the reply's source (behind DNAT).
pub fn records(event: &Event, pods: &dyn Pods, now: SystemTime) -> Vec<TrafficRecord> {
    let pod = |address: IpAddr| pods.by_address(address);
    let mut ends: Vec<(String, bool)> = Vec::new();
    let original = &event.original;
    let candidates = [
        (Some(original.source), true),
        (Some(original.destination), false),
        (event.reply.map(|r| r.source), false),
    ];
    for (address, outbound) in candidates {
        if let Some(pod) = address.and_then(pod)
            && !ends.iter().any(|(p, o)| *p == pod && *o == outbound)
        {
            ends.push((pod, outbound));
        }
    }
    let (start, end) = match event.ended {
        false => (Some(now), None),
        true => (event.start, Some(event.stop.unwrap_or(now))),
    };
    ends.into_iter()
        .map(|(pod_id, outbound)| TrafficRecord {
            time: Some(timestamp(now)),
            pod_id,
            missed: 0,
            record: Some(traffic_record::Record::Flow(Flow {
                tuple: Some(tuple(original)),
                outbound,
                start: start.map(timestamp),
                end: end.map(timestamp),
                ended: event.ended,
                bytes_sent: event.original_counters.bytes,
                bytes_received: event.reply_counters.bytes,
                packets_sent: event.original_counters.packets,
                packets_received: event.reply_counters.packets,
            })),
        })
        .collect()
}

/// Deletes the tracked connections whose original source is `source`, in
/// the calling thread's network namespace, and says how many it deleted.
/// feosd calls it when a pod is deleted, before the pod's IPv4 address is
/// handed out again: else the next pod with the address would find the
/// old one's connections, and its records would be the old one's. Each
/// connection ends with a DESTROY event; its record names the pod that
/// the caller's [`Pods`] gives for the address when the collector reads
/// the event, which in feosd is the deleted one still.
///
/// The kernel is asked for an IPv4 source's connections alone
/// (CTA_FILTER, Linux 5.8 on). One that ignores the filter sends all of
/// the family's, and one that refuses it is asked for all; the source's
/// are picked from them here. For an IPv6 source it is asked for all:
/// its filter of IPv6 addresses passes the other addresses (Linux 6.12).
/// It waits for the kernel to acknowledge each deletion.
pub fn delete_connections(source: IpAddr) -> io::Result<usize> {
    let mut socket = Socket::open(0)?;
    socket.set_timeout(Duration::from_secs(5))?;
    let entries = match entries(&mut socket, source, source.is_ipv4()) {
        Err(e) if matches!(e.raw_os_error(), Some(libc::EINVAL | libc::EOPNOTSUPP)) => {
            entries(&mut socket, source, false)?
        }
        entries => entries?,
    };
    let mut deleted = 0;
    for entry in &entries {
        match socket.request(|seq| delete_request(seq, family(source), entry)) {
            Ok(()) => deleted += 1,
            // It ended in between.
            Err(e) if e.raw_os_error() == Some(libc::ENOENT) => {}
            Err(e) => return Err(e),
        }
    }
    Ok(deleted)
}

/// A tracked connection, as a deletion names it: its original tuple, its
/// zone and its ID, as the kernel sent them.
#[derive(Debug, PartialEq)]
pub(crate) struct Entry {
    tuple: Vec<u8>,
    zone: Option<Vec<u8>>,
    id: Option<Vec<u8>>,
}

/// The connections whose original source is `source`, of its family;
/// with `filter`, the kernel is asked for those alone.
pub(crate) fn entries(socket: &mut Socket, source: IpAddr, filter: bool) -> io::Result<Vec<Entry>> {
    let mut entries = Vec::new();
    socket.dump(
        |seq| dump_request(seq, source, filter),
        |message| entries.extend(entry(message, source)),
    )?;
    Ok(entries)
}

/// A dump of the conntrack table of the source's family: of the
/// connections from `source` with `filter`, else of all.
fn dump_request(seq: u32, source: IpAddr, filter: bool) -> Vec<u8> {
    let (kind, address) = match source {
        IpAddr::V4(v4) => (CTA_IP_V4_SRC, v4.octets().to_vec()),
        IpAddr::V6(v6) => (CTA_IP_V6_SRC, v6.octets().to_vec()),
    };
    let ip = netlink::nested(&[(kind, &address)]);
    let tuple = netlink::nested(&[(CTA_TUPLE_IP | netlink::NLA_F_NESTED, &ip)]);
    // In host order, as NLA_U32.
    let flags = netlink::nested(&[(CTA_FILTER_ORIG_FLAGS, &CTA_FILTER_IP_SRC.to_ne_bytes())]);
    let attrs: &[(u16, &[u8])] = match filter {
        true => &[
            (CTA_TUPLE_ORIG | netlink::NLA_F_NESTED, &tuple),
            (CTA_FILTER | netlink::NLA_F_NESTED, &flags),
        ],
        false => &[],
    };
    netlink::request(
        NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_GET,
        netlink::NLM_F_REQUEST | netlink::NLM_F_DUMP,
        seq,
        family(source),
        0,
        attrs,
    )
}

/// The connection of a dumped message, if its original source is
/// `source`.
fn entry(message: &Message, source: IpAddr) -> Option<Entry> {
    if message.kind != NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_NEW {
        return None;
    }
    let (_, _, attrs) = message.netfilter()?;
    let tuple = attribute(attrs, CTA_TUPLE_ORIG)?;
    if parse_tuple(tuple)?.source != source {
        return None;
    }
    Some(Entry {
        tuple: tuple.to_vec(),
        zone: attribute(attrs, CTA_ZONE).map(<[u8]>::to_vec),
        id: attribute(attrs, CTA_ID).map(<[u8]>::to_vec),
    })
}

/// The deletion of a connection: by its original tuple, in its zone, if
/// it still has its ID.
fn delete_request(seq: u32, family: u8, entry: &Entry) -> Vec<u8> {
    let mut attrs: Vec<(u16, &[u8])> = vec![(CTA_TUPLE_ORIG | netlink::NLA_F_NESTED, &entry.tuple)];
    if let Some(zone) = &entry.zone {
        attrs.push((CTA_ZONE, zone));
    }
    if let Some(id) = &entry.id {
        attrs.push((CTA_ID, id));
    }
    netlink::request(
        NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_DELETE,
        netlink::NLM_F_REQUEST | netlink::NLM_F_ACK,
        seq,
        family,
        0,
        &attrs,
    )
}

/// The netfilter family of an address.
fn family(address: IpAddr) -> u8 {
    match address {
        IpAddr::V4(_) => libc::AF_INET as u8,
        IpAddr::V6(_) => libc::AF_INET6 as u8,
    }
}

/// A request to flush the conntrack table of a family, for tests: every
/// entry ends with a DESTROY event.
#[cfg(test)]
pub fn flush_request(seq: u32, family: u8) -> Vec<u8> {
    netlink::request(
        NFNL_SUBSYS_CTNETLINK << 8 | IPCTNL_MSG_CT_DELETE,
        netlink::NLM_F_REQUEST | netlink::NLM_F_ACK,
        seq,
        family,
        0,
        &[],
    )
}

#[cfg(test)]
pub mod tests {
    use super::*;
    use crate::testdata;
    use feos_proto::network_service::Tuple as RecordTuple;
    use std::collections::HashMap;

    pub struct Map(pub HashMap<IpAddr, String>);

    impl Pods for Map {
        fn by_address(&self, address: IpAddr) -> Option<String> {
            self.0.get(&address).cloned()
        }
    }

    /// The addresses of the pod of testdata.rs.
    pub const POD: &str = "fd00:fe05:1::1";
    pub const POD4: &str = "169.254.16.5";

    pub fn address(address: &str) -> IpAddr {
        address.parse().unwrap()
    }

    /// pod-a, by both its addresses.
    pub fn pods() -> Map {
        Map(HashMap::from([
            (address(POD), "pod-a".to_string()),
            (address(POD4), "pod-a".to_string()),
        ]))
    }

    fn event(bytes: &[u8]) -> Event {
        let message = netlink::messages(bytes).next().unwrap();
        parse(&message).unwrap()
    }

    fn flow(record: &TrafficRecord) -> &Flow {
        let Some(traffic_record::Record::Flow(flow)) = &record.record else {
            panic!("not a flow");
        };
        flow
    }

    #[test]
    fn a_new_tcp_flow() {
        let event = event(testdata::CT_NEW_TCP);
        assert!(!event.ended);
        assert_eq!(event.original.protocol, 6);
        assert_eq!(event.original.source, address(POD));
        assert_eq!(event.original.destination, address("fd00:fe05:2::2"));
        assert_eq!(event.original.destination_port, 8080);
        let reply = event.reply.unwrap();
        assert_eq!(
            (reply.source, reply.destination),
            (event.original.destination, event.original.source)
        );
        assert_eq!(event.original_counters, Counters::default());

        let now = SystemTime::now();
        let records = records(&event, &pods(), now);
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].pod_id, "pod-a");
        let flow = flow(&records[0]);
        assert!(flow.outbound && !flow.ended);
        assert_eq!(flow.start, Some(timestamp(now)));
        assert_eq!(
            flow.tuple,
            Some(RecordTuple {
                protocol: 6,
                source_address: POD.to_string(),
                source_port: u32::from(event.original.source_port),
                destination_address: "fd00:fe05:2::2".to_string(),
                destination_port: 8080,
            })
        );
    }

    #[test]
    fn a_destroyed_tcp_flow_has_counters_and_times() {
        let event = event(testdata::CT_DESTROY_TCP);
        assert!(event.ended);
        assert!(event.original_counters.bytes >= 1000);
        assert!(event.original_counters.packets >= 3);
        assert!(event.reply_counters.bytes >= 100);
        let (start, stop) = (event.start.unwrap(), event.stop.unwrap());
        assert!(start <= stop);
        let records = records(&event, &pods(), SystemTime::now());
        let flow = flow(&records[0]);
        assert!(flow.ended);
        assert_eq!(flow.start, Some(timestamp(start)));
        assert_eq!(flow.end, Some(timestamp(stop)));
        assert_eq!(flow.bytes_sent, event.original_counters.bytes);
        assert_eq!(flow.packets_received, event.reply_counters.packets);
    }

    /// The node masqueraded the pod's IPv4: the reply goes to the node's
    /// address, the record has the pod's.
    #[test]
    fn an_ipv4_flow_is_the_pod_s_before_masquerade() {
        let event = event(testdata::CT_NEW_TCP4);
        assert_eq!(event.original.source, address(POD4));
        assert_eq!(event.original.destination, address("192.0.2.2"));
        let reply = event.reply.unwrap();
        assert_eq!(reply.source, address("192.0.2.2"));
        assert_eq!(reply.destination, address("192.0.2.1"));

        let records = records(&event, &pods(), SystemTime::now());
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].pod_id, "pod-a");
        let flow = flow(&records[0]);
        assert!(flow.outbound);
        assert_eq!(
            flow.tuple,
            Some(RecordTuple {
                protocol: 6,
                source_address: POD4.to_string(),
                source_port: u32::from(event.original.source_port),
                destination_address: "192.0.2.2".to_string(),
                destination_port: 8080,
            })
        );
    }

    #[test]
    fn udp_icmpv6_and_icmp_flows() {
        let udp = event(testdata::CT_NEW_UDP);
        assert_eq!(udp.original.protocol, 17);
        assert_eq!(udp.original.destination_port, 5353);
        let icmpv6 = event(testdata::CT_NEW_ICMPV6);
        assert_eq!(icmpv6.original.protocol, 58);
        assert_eq!(icmpv6.original.destination_port, 128);
        assert_ne!(icmpv6.original.source_port, 0);
        assert_eq!(icmpv6.reply.unwrap().destination_port, 129);
        let icmp = event(testdata::CT_NEW_ICMP);
        assert_eq!(icmp.original.protocol, 1);
        assert_eq!(icmp.original.source, address(POD4));
        assert_eq!(icmp.original.destination_port, 8);
        assert_ne!(icmp.original.source_port, 0);
        let reply = icmp.reply.unwrap();
        assert_eq!(reply.destination_port, 0);
        assert_eq!(reply.destination, address("192.0.2.1"));
        let records = records(&icmp, &pods(), SystemTime::now());
        let tuple = flow(&records[0]).tuple.clone().unwrap();
        assert_eq!((tuple.protocol, tuple.source_address.as_str()), (1, POD4));
        assert_eq!(tuple.source_port, u32::from(icmp.original.source_port));
        assert_eq!(tuple.destination_port, 8);
    }

    #[test]
    fn inbound_and_pod_to_pod_flows() {
        let mut event = event(testdata::CT_NEW_TCP);
        std::mem::swap(&mut event.original.source, &mut event.original.destination);
        event.reply = None;
        let inbound = records(&event, &pods(), SystemTime::now());
        assert!(!flow(&inbound[0]).outbound);

        let other = address("fd00:fe05:1::2");
        event.original.source = other;
        let mut pods = pods();
        pods.0.insert(other, "pod-b".to_string());
        let both = records(&event, &pods, SystemTime::now());
        let ends: Vec<_> = both.iter().map(|r| r.pod_id.as_str()).collect();
        assert_eq!(ends, ["pod-b", "pod-a"]);
    }

    #[test]
    fn other_messages_are_no_events() {
        for vector in [testdata::CT_NEW_TCP, testdata::CT_DESTROY_TCP] {
            for len in 0..vector.len() {
                let mut bytes = vector[..len].to_vec();
                if len >= 4 {
                    // Make the header claim what is left.
                    bytes[..4].copy_from_slice(&(len as u32).to_ne_bytes());
                }
                if let Some(message) = netlink::messages(&bytes).next() {
                    let _ = parse(&message);
                    let _ = entry(&message, address(POD));
                }
            }
        }
        let mut bytes = testdata::CT_NEW_TCP.to_vec();
        bytes[5] = 4; // another subsystem
        assert_eq!(parse(&netlink::messages(&bytes).next().unwrap()), None);
    }

    /// The attributes of a request, by type, without its flags.
    fn request_attributes(request: &[u8]) -> (u16, u16, u8, HashMap<u16, Vec<u8>>) {
        let message = netlink::messages(request).next().unwrap();
        let flags = u16::from_ne_bytes([request[6], request[7]]);
        let (family, _, attrs) = message.netfilter().unwrap();
        let attrs = attributes(attrs).map(|(k, v)| (k, v.to_vec())).collect();
        (message.kind, flags, family, attrs)
    }

    #[test]
    fn dumps_ask_for_a_source() {
        let (kind, flags, family, attrs) =
            request_attributes(&dump_request(7, address(POD4), true));
        assert_eq!(kind, 0x101);
        assert_eq!(flags, netlink::NLM_F_REQUEST | netlink::NLM_F_DUMP);
        assert_eq!(family, libc::AF_INET as u8);
        let ip = attribute(&attrs[&CTA_TUPLE_ORIG], CTA_TUPLE_IP).unwrap();
        assert_eq!(attribute(ip, CTA_IP_V4_SRC), Some(&[169, 254, 16, 5][..]));
        assert_eq!(attribute(ip, CTA_IP_V4_DST), None);
        let flags = attribute(&attrs[&CTA_FILTER], CTA_FILTER_ORIG_FLAGS).unwrap();
        assert_eq!(flags, 1u32.to_ne_bytes());

        let (_, _, family, attrs) = request_attributes(&dump_request(7, address(POD), true));
        assert_eq!(family, libc::AF_INET6 as u8);
        let ip = attribute(&attrs[&CTA_TUPLE_ORIG], CTA_TUPLE_IP).unwrap();
        let pod: Ipv6Addr = POD.parse().unwrap();
        assert_eq!(attribute(ip, CTA_IP_V6_SRC), Some(&pod.octets()[..]));

        let (_, _, family, attrs) = request_attributes(&dump_request(7, address(POD4), false));
        assert_eq!(family, libc::AF_INET as u8);
        assert!(attrs.is_empty());
    }

    /// A NEW event has what a dump has of a connection.
    #[test]
    fn connections_of_a_source_are_deleted_by_their_tuple() {
        let message = netlink::messages(testdata::CT_NEW_TCP4).next().unwrap();
        assert_eq!(entry(&message, address("192.0.2.2")), None);
        let entry = entry(&message, address(POD4)).unwrap();
        let (_, _, attrs) = message.netfilter().unwrap();
        assert_eq!(entry.tuple, attribute(attrs, CTA_TUPLE_ORIG).unwrap());
        assert_eq!(entry.id.as_deref(), attribute(attrs, CTA_ID));
        assert!(entry.id.is_some());
        assert_eq!(entry.zone, None);

        let request = delete_request(9, libc::AF_INET as u8, &entry);
        let (kind, flags, family, attrs) = request_attributes(&request);
        assert_eq!(kind, 0x102);
        assert_eq!(flags, netlink::NLM_F_REQUEST | netlink::NLM_F_ACK);
        assert_eq!(family, libc::AF_INET as u8);
        assert_eq!(attrs[&CTA_TUPLE_ORIG], entry.tuple);
        assert_eq!(
            parse_tuple(&attrs[&CTA_TUPLE_ORIG]),
            Some(event(testdata::CT_NEW_TCP4).original)
        );
        assert_eq!(Some(&attrs[&CTA_ID]), entry.id.as_ref());
        assert!(!attrs.contains_key(&CTA_ZONE));
        // The tuple is nested.
        let at = 16 + 4;
        assert_eq!(
            u16::from_ne_bytes([request[at + 2], request[at + 3]]),
            CTA_TUPLE_ORIG | netlink::NLA_F_NESTED
        );

        // A destroyed connection is not dumped.
        let message = netlink::messages(testdata::CT_DESTROY_TCP).next().unwrap();
        assert_eq!(super::entry(&message, address(POD)), None);
    }
}
