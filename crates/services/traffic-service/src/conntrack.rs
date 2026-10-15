// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Flows from connection tracking: its events over ctnetlink, a flow's
//! begin (NEW) and its end (DESTROY, with counters and timestamps).

use crate::netlink::{attribute, attributes, be16, be32, be64, Message, Socket};
use crate::packet::{Tuple, ICMPV6};
use crate::{timestamp, tuple, Context, Pods};
use feos_proto::network_service::{traffic_record, Flow, TrafficRecord};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const NFNL_SUBSYS_CTNETLINK: u16 = 1;
const IPCTNL_MSG_CT_NEW: u16 = 0;
const IPCTNL_MSG_CT_DELETE: u16 = 2;
const NFNLGRP_CONNTRACK_NEW: u32 = 1;
const NFNLGRP_CONNTRACK_DESTROY: u32 = 3;

const CTA_TUPLE_ORIG: u16 = 1;
const CTA_TUPLE_REPLY: u16 = 2;
const CTA_COUNTERS_ORIG: u16 = 9;
const CTA_COUNTERS_REPLY: u16 = 10;
const CTA_TIMESTAMP: u16 = 20;

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
        for record in records(
            &event,
            context.pods.as_ref(),
            &context.nat64,
            SystemTime::now(),
        ) {
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
pub fn records(
    event: &Event,
    pods: &dyn Pods,
    nat64: &Ipv6Addr,
    now: SystemTime,
) -> Vec<TrafficRecord> {
    let pod = |address: IpAddr| match address {
        IpAddr::V6(address) => pods.by_address(&address),
        IpAddr::V4(_) => None,
    };
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
                tuple: Some(tuple(original, nat64)),
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

/// A request to flush the conntrack table of a family, for tests: every
/// entry ends with a DESTROY event.
#[cfg(test)]
pub fn flush_request(seq: u32, family: u8) -> Vec<u8> {
    use crate::netlink;
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
    use crate::{netlink, testdata};
    use feos_proto::network_service::Tuple as RecordTuple;
    use std::collections::HashMap;

    pub struct Map(pub HashMap<Ipv6Addr, String>);

    impl Pods for Map {
        fn by_address(&self, address: &Ipv6Addr) -> Option<String> {
            self.0.get(address).cloned()
        }
    }

    const NAT64: Ipv6Addr = Ipv6Addr::new(0x64, 0xff9b, 0, 0, 0, 0, 0, 0);

    fn event(bytes: &[u8]) -> Event {
        let message = netlink::messages(bytes).next().unwrap();
        parse(&message).unwrap()
    }

    fn pod() -> Ipv6Addr {
        "fd00:feed::1".parse().unwrap()
    }

    fn pods() -> Map {
        Map(HashMap::from([(pod(), "pod-a".to_string())]))
    }

    #[test]
    fn a_new_tcp_flow() {
        let event = event(testdata::CT_NEW_TCP);
        assert!(!event.ended);
        assert_eq!(event.original.protocol, 6);
        assert_eq!(event.original.source, IpAddr::from(pod()));
        assert_eq!(
            event.original.destination,
            "64:ff9b::c000:202".parse::<IpAddr>().unwrap()
        );
        assert_eq!(event.original.destination_port, 8080);
        let reply = event.reply.unwrap();
        assert_eq!(
            (reply.source, reply.destination),
            (event.original.destination, event.original.source)
        );
        assert_eq!(event.original_counters, Counters::default());

        let now = SystemTime::now();
        let records = records(&event, &pods(), &NAT64, now);
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].pod_id, "pod-a");
        let Some(traffic_record::Record::Flow(flow)) = &records[0].record else {
            panic!("not a flow");
        };
        assert!(flow.outbound && !flow.ended);
        assert_eq!(flow.start, Some(timestamp(now)));
        assert_eq!(
            flow.tuple,
            Some(RecordTuple {
                protocol: 6,
                source_address: "fd00:feed::1".to_string(),
                source_port: u32::from(event.original.source_port),
                destination_address: "192.0.2.2".to_string(),
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
        let records = records(&event, &pods(), &NAT64, SystemTime::now());
        let Some(traffic_record::Record::Flow(flow)) = &records[0].record else {
            panic!("not a flow");
        };
        assert!(flow.ended);
        assert_eq!(flow.start, Some(timestamp(start)));
        assert_eq!(flow.end, Some(timestamp(stop)));
        assert_eq!(flow.bytes_sent, event.original_counters.bytes);
        assert_eq!(flow.packets_received, event.reply_counters.packets);
    }

    #[test]
    fn udp_and_icmpv6_flows() {
        let udp = event(testdata::CT_NEW_UDP);
        assert_eq!(udp.original.protocol, 17);
        assert_eq!(udp.original.destination_port, 5353);
        let icmp = event(testdata::CT_NEW_ICMPV6);
        assert_eq!(icmp.original.protocol, 58);
        assert_eq!(icmp.original.destination_port, 128);
        assert_ne!(icmp.original.source_port, 0);
        assert_eq!(icmp.reply.unwrap().destination_port, 129);
    }

    #[test]
    fn ipv4_flows_are_nobody_s() {
        let event = event(testdata::CT_NEW_UDP4);
        assert_eq!(
            event.original.source,
            "192.0.2.1".parse::<IpAddr>().unwrap()
        );
        assert!(records(&event, &pods(), &NAT64, SystemTime::now()).is_empty());
    }

    #[test]
    fn inbound_and_pod_to_pod_flows() {
        let mut event = event(testdata::CT_NEW_TCP);
        std::mem::swap(&mut event.original.source, &mut event.original.destination);
        event.reply = None;
        let inbound = records(&event, &pods(), &NAT64, SystemTime::now());
        let Some(traffic_record::Record::Flow(flow)) = &inbound[0].record else {
            panic!("not a flow");
        };
        assert!(!flow.outbound);

        let other: Ipv6Addr = "fd00:feed::2".parse().unwrap();
        event.original.source = other.into();
        let mut pods = pods();
        pods.0.insert(other, "pod-b".to_string());
        let both = records(&event, &pods, &NAT64, SystemTime::now());
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
                }
            }
        }
        let mut bytes = testdata::CT_NEW_TCP.to_vec();
        bytes[5] = 4; // another subsystem
        assert_eq!(parse(&netlink::messages(&bytes).next().unwrap()), None);
    }
}
