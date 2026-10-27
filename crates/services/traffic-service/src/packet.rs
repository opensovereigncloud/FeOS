// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The headers of an IPv6 or IPv4 packet as nflog copies it: addresses,
//! the transport protocol and its ports, and of TCP the sequence number,
//! flags and payload. A copy may be cut short anywhere.

use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

pub const TCP: u8 = 6;
pub const UDP: u8 = 17;
pub const ICMPV6: u8 = 58;
const ICMP: u8 = 1;

/// How many extension headers are skipped before giving up.
const MAX_EXTENSIONS: usize = 8;

/// A connection's addresses and ports, as a flow's first packet had them.
/// For ICMP and ICMPv6, the source port is the echo identifier and the
/// destination port the type, as connection tracking keys them.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct Tuple {
    pub protocol: u8,
    pub source: IpAddr,
    pub source_port: u16,
    pub destination: IpAddr,
    pub destination_port: u16,
}

/// The TCP part of a packet.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Segment<'a> {
    pub seq: u32,
    pub syn: bool,
    /// The payload, as far as the copy has it.
    pub payload: &'a [u8],
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Packet<'a> {
    pub tuple: Tuple,
    pub segment: Option<Segment<'a>>,
}

/// Parses an IPv6 or IPv4 packet; None for anything else, or less than
/// its IP header. Ports the copy does not reach are 0.
pub fn parse(data: &[u8]) -> Option<Packet<'_>> {
    match data.first()? >> 4 {
        6 => ipv6(data),
        4 => ipv4(data),
        _ => None,
    }
}

fn ipv6(data: &[u8]) -> Option<Packet<'_>> {
    if data.len() < 40 {
        return None;
    }
    let source = Ipv6Addr::from(<[u8; 16]>::try_from(&data[8..24]).ok()?);
    let destination = Ipv6Addr::from(<[u8; 16]>::try_from(&data[24..40]).ok()?);
    let payload_len = u16::from_be_bytes([data[4], data[5]]) as usize;
    // A jumbogram says 0; then the copy is all there is.
    let end = match payload_len {
        0 => data.len(),
        len => data.len().min(40 + len),
    };
    let data = &data[..end];
    let mut next = data[6];
    let mut offset = 40;
    let mut first_fragment = true;
    for _ in 0..MAX_EXTENSIONS {
        let header = data.get(offset..offset + 2);
        match (next, header) {
            // Hop-by-hop, routing and destination options.
            (0 | 43 | 60, Some(h)) => {
                next = h[0];
                offset += (h[1] as usize + 1) * 8;
            }
            // Authentication header.
            (51, Some(h)) => {
                next = h[0];
                offset += (h[1] as usize + 2) * 4;
            }
            (44, Some(h)) => {
                let Some(fragment) = data.get(offset..offset + 8) else {
                    break;
                };
                next = h[0];
                first_fragment = u16::from_be_bytes([fragment[2], fragment[3]]) & 0xfff8 == 0;
                offset += 8;
            }
            _ => break,
        }
    }
    let transport = match first_fragment {
        true => data.get(offset..).unwrap_or(&[]),
        false => &[],
    };
    Some(transport_header(
        next,
        source.into(),
        destination.into(),
        transport,
    ))
}

fn ipv4(data: &[u8]) -> Option<Packet<'_>> {
    let header_len = (*data.first()? & 0x0f) as usize * 4;
    if header_len < 20 || data.len() < header_len {
        return None;
    }
    let total_len = u16::from_be_bytes([data[2], data[3]]) as usize;
    let data = &data[..data.len().min(total_len.max(header_len))];
    let first_fragment = u16::from_be_bytes([data[6], data[7]]) & 0x1fff == 0;
    let source = Ipv4Addr::new(data[12], data[13], data[14], data[15]);
    let destination = Ipv4Addr::new(data[16], data[17], data[18], data[19]);
    let transport = match first_fragment {
        true => &data[header_len..],
        false => &[],
    };
    Some(transport_header(
        data[9],
        source.into(),
        destination.into(),
        transport,
    ))
}

/// The ports, and of TCP the segment, from what the copy has of the
/// transport header.
fn transport_header(
    protocol: u8,
    source: IpAddr,
    destination: IpAddr,
    transport: &[u8],
) -> Packet<'_> {
    let mut tuple = Tuple {
        protocol,
        source,
        source_port: 0,
        destination,
        destination_port: 0,
    };
    let mut segment = None;
    // Echo request and reply.
    let echo = match protocol {
        ICMPV6 => [128, 129],
        _ => [8, 0],
    };
    match protocol {
        TCP | UDP if transport.len() >= 4 => {
            tuple.source_port = u16::from_be_bytes([transport[0], transport[1]]);
            tuple.destination_port = u16::from_be_bytes([transport[2], transport[3]]);
            if protocol == TCP && transport.len() >= 20 {
                let header_len = (transport[12] >> 4) as usize * 4;
                segment = Some(Segment {
                    seq: u32::from_be_bytes([
                        transport[4],
                        transport[5],
                        transport[6],
                        transport[7],
                    ]),
                    syn: transport[13] & 0x02 != 0,
                    payload: transport.get(header_len.max(20)..).unwrap_or(&[]),
                });
            }
        }
        ICMPV6 | ICMP if !transport.is_empty() => {
            let kind = transport[0];
            tuple.destination_port = kind.into();
            if echo.contains(&kind) && transport.len() >= 6 {
                tuple.source_port = u16::from_be_bytes([transport[4], transport[5]]);
            }
        }
        _ => {}
    }
    Packet { tuple, segment }
}

#[cfg(test)]
pub mod tests {
    use super::*;

    /// An IPv6 packet with a TCP header (and no options) before `payload`.
    pub fn tcp(
        source: impl Into<IpAddr>,
        source_port: u16,
        destination: impl Into<IpAddr>,
        destination_port: u16,
        seq: u32,
        flags: u8,
        payload: &[u8],
    ) -> Vec<u8> {
        let mut tcp = Vec::new();
        tcp.extend_from_slice(&source_port.to_be_bytes());
        tcp.extend_from_slice(&destination_port.to_be_bytes());
        tcp.extend_from_slice(&seq.to_be_bytes());
        tcp.extend_from_slice(&0u32.to_be_bytes()); // ack
        tcp.push(5 << 4);
        tcp.push(flags);
        tcp.extend_from_slice(&[0xff, 0xff, 0, 0, 0, 0]); // window, checksum, urgent
        tcp.extend_from_slice(payload);
        match (source.into(), destination.into()) {
            (IpAddr::V6(source), IpAddr::V6(destination)) => ipv6(source, destination, TCP, &tcp),
            (IpAddr::V4(source), IpAddr::V4(destination)) => ipv4(source, destination, TCP, &tcp),
            _ => panic!("addresses of two families"),
        }
    }

    pub fn ipv6(source: Ipv6Addr, destination: Ipv6Addr, next: u8, payload: &[u8]) -> Vec<u8> {
        let mut packet = vec![0x60, 0, 0, 0];
        packet.extend_from_slice(&(payload.len() as u16).to_be_bytes());
        packet.push(next);
        packet.push(64);
        packet.extend_from_slice(&source.octets());
        packet.extend_from_slice(&destination.octets());
        packet.extend_from_slice(payload);
        packet
    }

    /// An IPv4 packet without options.
    pub fn ipv4(source: Ipv4Addr, destination: Ipv4Addr, protocol: u8, payload: &[u8]) -> Vec<u8> {
        let mut packet = vec![0x45, 0];
        packet.extend_from_slice(&(20 + payload.len() as u16).to_be_bytes());
        // Identification, flags (don't fragment), TTL, protocol, checksum.
        packet.extend_from_slice(&[0, 0, 0x40, 0, 64, protocol, 0, 0]);
        packet.extend_from_slice(&source.octets());
        packet.extend_from_slice(&destination.octets());
        packet.extend_from_slice(payload);
        packet
    }

    const A: Ipv6Addr = Ipv6Addr::new(0x2001, 0xdb8, 0, 0, 0, 0, 0, 1);
    const B: Ipv6Addr = Ipv6Addr::new(0x2001, 0xdb8, 0, 0, 0, 0, 0, 2);
    const A4: Ipv4Addr = Ipv4Addr::new(169, 254, 16, 5);
    const B4: Ipv4Addr = Ipv4Addr::new(192, 0, 2, 2);

    #[test]
    fn tcp_segments_parse() {
        let pairs: [(IpAddr, IpAddr, usize); 2] =
            [(A.into(), B.into(), 40), (A4.into(), B4.into(), 20)];
        for (a, b, header) in pairs {
            let data = tcp(a, 40000, b, 443, 1000, 0x18, b"hello");
            let packet = parse(&data).unwrap();
            assert_eq!(packet.tuple.protocol, TCP);
            assert_eq!((packet.tuple.source, packet.tuple.destination), (a, b));
            assert_eq!(
                (packet.tuple.source_port, packet.tuple.destination_port),
                (40000, 443)
            );
            let segment = packet.segment.unwrap();
            assert_eq!(
                (segment.seq, segment.syn, segment.payload),
                (1000, false, &b"hello"[..])
            );
            // Cut short by the snap length: what is there counts.
            let packet = parse(&data[..data.len() - 2]).unwrap();
            assert_eq!(packet.segment.unwrap().payload, b"hel");
            // The header alone.
            let packet = parse(&data[..header + 4]).unwrap();
            assert_eq!(packet.tuple.destination_port, 443);
            assert_eq!(packet.segment, None);
        }
    }

    #[test]
    fn extension_headers_are_skipped() {
        let udp = [0x30, 0x39, 0, 53, 0, 8, 0, 0];
        let mut options = vec![UDP, 0, 1, 4, 0, 0, 0, 0]; // destination options, PadN
        options.extend_from_slice(&udp);
        let data = ipv6(A, B, 60, &options);
        let packet = parse(&data).unwrap();
        assert_eq!(packet.tuple.protocol, UDP);
        assert_eq!(
            (packet.tuple.source_port, packet.tuple.destination_port),
            (12345, 53)
        );
        // A later fragment has no ports.
        let mut fragment = vec![UDP, 0, 0x01, 0x00, 0, 0, 0, 1];
        fragment.extend_from_slice(&udp);
        let data = ipv6(A, B, 44, &fragment);
        let packet = parse(&data).unwrap();
        assert_eq!(packet.tuple.protocol, UDP);
        assert_eq!(packet.tuple.destination_port, 0);
    }

    #[test]
    fn icmpv6_keys_by_identifier_and_type() {
        let echo = [128, 0, 0, 0, 0x12, 0x34, 0, 1];
        let data = ipv6(A, B, ICMPV6, &echo);
        let packet = parse(&data).unwrap();
        assert_eq!(
            (packet.tuple.source_port, packet.tuple.destination_port),
            (0x1234, 128)
        );
    }

    #[test]
    fn ipv4_packets_parse() {
        // UDP from 10.0.0.1:53 to 192.0.2.1:4000, with an option.
        let mut data = vec![
            0x46, 0, 0, 32, 0, 0, 0, 0, 64, UDP, 0, 0, 10, 0, 0, 1, 192, 0, 2, 1,
        ];
        data.extend_from_slice(&[1, 1, 1, 0]);
        data.extend_from_slice(&[0, 53, 0x0f, 0xa0, 0, 8, 0, 0]);
        let packet = parse(&data).unwrap();
        assert_eq!(packet.tuple.source, "10.0.0.1".parse::<IpAddr>().unwrap());
        assert_eq!(
            (packet.tuple.source_port, packet.tuple.destination_port),
            (53, 4000)
        );
        // A later fragment has no ports.
        data[6] = 0x00;
        data[7] = 0x10;
        assert_eq!(parse(&data).unwrap().tuple.destination_port, 0);
        // Echo requests have their identifier.
        let mut echo = vec![
            0x45, 0, 0, 28, 0, 0, 0, 0, 64, ICMP, 0, 0, 10, 0, 0, 1, 10, 0, 0, 2,
        ];
        echo.extend_from_slice(&[8, 0, 0, 0, 0x12, 0x34, 0, 1]);
        let packet = parse(&echo).unwrap();
        assert_eq!(
            (packet.tuple.source_port, packet.tuple.destination_port),
            (0x1234, 8)
        );
    }

    #[test]
    fn garbage_is_not_a_packet() {
        assert_eq!(parse(&[]), None);
        assert_eq!(parse(&[0x45; 19]), None);
        assert_eq!(parse(&[0x44; 60]), None);
        assert_eq!(parse(&[0x15; 60]), None);
        assert_eq!(parse(&[0x60; 39]), None);
        // Headers that claim more than there is.
        let mut data = tcp(A, 1, B, 2, 0, 0x02, &[]);
        data[40 + 12] = 0xf0;
        assert_eq!(parse(&data).unwrap().segment.unwrap().payload, b"");
        let data = ipv6(A, B, 0, &[0, 255]);
        assert_eq!(parse(&data).unwrap().tuple.protocol, 0);
        for len in 0..80 {
            let _ = parse(&vec![0x60; len]);
            let _ = parse(&vec![0x4f; len]);
            let _ = parse(&vec![0x45; len]);
        }
    }
}
