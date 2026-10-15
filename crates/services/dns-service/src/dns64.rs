// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! DNS64's rules: which AAAA records count, how IPv4 addresses are embedded
//! in the NAT64 prefix, and how answers are synthesized (RFC 6147, RFC 6052,
//! RFC 7050).

use hickory_proto::op::{Message, Query, ResponseCode};
use hickory_proto::rr::rdata::{A, AAAA};
use hickory_proto::rr::{Name, RData, Record, RecordType};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

/// The well-known NAT64 prefix, 64:ff9b::/96 (RFC 6052).
pub const WELL_KNOWN_PREFIX: Ipv6Addr = Ipv6Addr::new(0x64, 0xff9b, 0, 0, 0, 0, 0, 0);

/// What caps the TTL of synthesized records when the AAAA answer brought no
/// SOA record (RFC 6147 5.1.7).
const NEGATIVE_TTL: u32 = 600;
/// DNAME, which hickory-proto does not know.
const DNAME: u16 = 39;
/// The addresses of ipv4only.arpa (RFC 7050), and how long they hold.
const IPV4ONLY: [Ipv4Addr; 2] = [Ipv4Addr::new(192, 0, 0, 170), Ipv4Addr::new(192, 0, 0, 171)];
const IPV4ONLY_TTL: u32 = 3600;

/// `address` under the /96 `prefix` (RFC 6052 2.2).
pub fn embed(prefix: Ipv6Addr, address: Ipv4Addr) -> Ipv6Addr {
    let mut octets = prefix.octets();
    octets[12..].copy_from_slice(&address.octets());
    Ipv6Addr::from(octets)
}

/// Whether `prefix` is the well-known prefix, under which non-global IPv4
/// addresses must not be embedded (RFC 6052 3.1).
pub fn is_well_known(prefix: Ipv6Addr) -> bool {
    prefix.octets()[..12] == WELL_KNOWN_PREFIX.octets()[..12]
}

/// Whether `address` is reachable on the internet: not private, shared
/// (CGNAT), loopback, link-local, documentation, benchmarking, multicast or
/// reserved (RFC 6890).
pub fn is_global(address: Ipv4Addr) -> bool {
    const NON_GLOBAL: [(Ipv4Addr, u8); 14] = [
        (Ipv4Addr::new(0, 0, 0, 0), 8),
        (Ipv4Addr::new(10, 0, 0, 0), 8),
        (Ipv4Addr::new(100, 64, 0, 0), 10),
        (Ipv4Addr::new(127, 0, 0, 0), 8),
        (Ipv4Addr::new(169, 254, 0, 0), 16),
        (Ipv4Addr::new(172, 16, 0, 0), 12),
        (Ipv4Addr::new(192, 0, 0, 0), 24),
        (Ipv4Addr::new(192, 0, 2, 0), 24),
        (Ipv4Addr::new(192, 168, 0, 0), 16),
        (Ipv4Addr::new(198, 18, 0, 0), 15),
        (Ipv4Addr::new(198, 51, 100, 0), 24),
        (Ipv4Addr::new(203, 0, 113, 0), 24),
        (Ipv4Addr::new(224, 0, 0, 0), 4),
        (Ipv4Addr::new(240, 0, 0, 0), 4),
    ];
    // The anycast addresses of PCP and TURN are global (RFC 7723, RFC 8155).
    if address == Ipv4Addr::new(192, 0, 0, 9) || address == Ipv4Addr::new(192, 0, 0, 10) {
        return true;
    }
    let bits = u32::from(address);
    !NON_GLOBAL.iter().any(|(network, length)| {
        let mask = u32::MAX << (32 - u32::from(*length));
        bits & mask == u32::from(*network)
    })
}

/// Whether an AAAA record counts as absent (RFC 6147 5.1.4): an IPv4-mapped
/// address, ::ffff:0:0/96.
pub fn is_excluded(address: Ipv6Addr) -> bool {
    address.to_ipv4_mapped().is_some()
}

/// Removes the AAAA records that count as absent from an answer.
pub fn exclude(response: &mut Message) {
    response.answers_mut().retain(
        |record| !matches!(record.data(), RData::AAAA(AAAA(address)) if is_excluded(*address)),
    );
}

/// Whether an answer has AAAA records.
pub fn has_aaaa(response: &Message) -> bool {
    response
        .answers()
        .iter()
        .any(|record| matches!(record.data(), RData::AAAA(_)))
}

/// The TTL that caps synthesized records: the negative TTL of the AAAA
/// answer, from its SOA record (RFC 2308), or 600 s (RFC 6147 5.1.7).
pub fn negative_ttl(aaaa: Option<&Message>) -> u32 {
    aaaa.into_iter()
        .flat_map(|response| response.name_servers())
        .filter_map(|record| match record.data() {
            RData::SOA(soa) => Some(record.ttl().min(soa.minimum())),
            _ => None,
        })
        .min()
        .unwrap_or(NEGATIVE_TTL)
}

/// The answer to an AAAA query made from the answer to the A query for the
/// same name: its CNAME chain, and an AAAA record under `prefix` for each A
/// record, with the A record's TTL, at most `ttl` (RFC 6147 5.1.6, 5.1.7).
/// Under the well-known prefix, non-global addresses get none. None when
/// there is nothing to synthesize.
pub fn synthesize(a: &Message, prefix: Ipv6Addr, ttl: u32) -> Option<Message> {
    let well_known = is_well_known(prefix);
    let mut answers = Vec::with_capacity(a.answers().len());
    let mut synthesized = false;
    for record in a.answers() {
        match record.data() {
            RData::A(A(address)) => {
                if well_known && !is_global(*address) {
                    continue;
                }
                let mut aaaa = Record::from_rdata(
                    record.name().clone(),
                    record.ttl().min(ttl),
                    RData::AAAA(AAAA(embed(prefix, *address))),
                );
                aaaa.set_dns_class(record.dns_class());
                answers.push(aaaa);
                synthesized = true;
            }
            _ if is_chain(record) => answers.push(record.clone()),
            // Signatures of the A records, which do not cover what replaces them.
            _ => {}
        }
    }
    if !synthesized {
        return None;
    }
    let mut response = Message::new();
    response.set_header(*a.header());
    response.set_authoritative(false).set_authentic_data(false);
    *response.answers_mut() = answers;
    Some(response)
}

/// The answer to an A query as the negative answer to an AAAA query: its
/// CNAME chain, without the A records.
pub fn without_a(mut a: Message) -> Message {
    a.answers_mut().retain(is_chain);
    a
}

/// Whether a record links a name to the next of a chain.
fn is_chain(record: &Record) -> bool {
    record.record_type() == RecordType::CNAME || u16::from(record.record_type()) == DNAME
}

/// The in-addr.arpa name of an ip6.arpa name for an address under the NAT64
/// prefix, which a PTR query asks for instead (RFC 6147 5.3.1).
pub fn reverse(name: &Name, prefix: Ipv6Addr) -> Option<Name> {
    let network = name.parse_arpa_name().ok()?;
    let IpAddr::V6(address) = network.addr() else {
        return None;
    };
    if network.prefix_len() != 128 || address.octets()[..12] != prefix.octets()[..12] {
        return None;
    }
    let [.., a, b, c, d] = address.octets();
    Some(Name::from(Ipv4Addr::new(a, b, c, d)))
}

/// Gives the answers that belong to `from` to `to`.
pub fn rename(response: &mut Message, from: &Name, to: &Name) {
    for record in response.answers_mut() {
        if record.name() == from {
            record.set_name(to.clone());
        }
    }
}

/// Whether `name` is ipv4only.arpa, which the resolver answers itself
/// (RFC 7050, RFC 8880).
pub fn is_ipv4only_arpa(name: &Name) -> bool {
    let mut labels = name.iter();
    matches!(
        (labels.next(), labels.next(), labels.next()),
        (Some(first), Some(second), None)
            if first.eq_ignore_ascii_case(b"ipv4only") && second.eq_ignore_ascii_case(b"arpa")
    )
}

/// The answer for ipv4only.arpa: its IPv4 addresses for A, those under
/// `prefix` for AAAA, nothing for other types.
pub fn ipv4only(query: &Query, prefix: Ipv6Addr) -> Message {
    let mut response = Message::new();
    response.set_response_code(ResponseCode::NoError);
    for address in IPV4ONLY {
        let data = match query.query_type() {
            RecordType::A => RData::A(A(address)),
            RecordType::AAAA => RData::AAAA(AAAA(embed(prefix, address))),
            _ => continue,
        };
        response.add_answer(Record::from_rdata(query.name().clone(), IPV4ONLY_TTL, data));
    }
    response
}

#[cfg(test)]
mod tests {
    use super::*;
    use hickory_proto::op::MessageType;
    use hickory_proto::rr::rdata::{CNAME, SOA};

    fn name(name: &str) -> Name {
        Name::from_ascii(name).unwrap()
    }

    fn a_answer(records: Vec<Record>) -> Message {
        let mut message = Message::new();
        message
            .set_message_type(MessageType::Response)
            .set_authentic_data(true);
        message.add_query(Query::query(name("www.example.com."), RecordType::A));
        *message.answers_mut() = records;
        message
    }

    fn a(owner: &str, ttl: u32, address: [u8; 4]) -> Record {
        Record::from_rdata(name(owner), ttl, RData::A(A(address.into())))
    }

    fn aaaa_addresses(message: &Message) -> Vec<Ipv6Addr> {
        message
            .answers()
            .iter()
            .filter_map(|record| match record.data() {
                RData::AAAA(AAAA(address)) => Some(*address),
                _ => None,
            })
            .collect()
    }

    #[test]
    fn embeds_in_the_last_32_bits() {
        assert_eq!(
            embed(WELL_KNOWN_PREFIX, Ipv4Addr::new(192, 0, 2, 33)),
            "64:ff9b::c000:221".parse::<Ipv6Addr>().unwrap()
        );
        // Whatever the prefix has there is replaced.
        let prefix: Ipv6Addr = "2001:db8:64::ffff:ffff".parse().unwrap();
        assert_eq!(
            embed(prefix, Ipv4Addr::new(1, 2, 3, 4)),
            "2001:db8:64::102:304".parse::<Ipv6Addr>().unwrap()
        );
    }

    #[test]
    fn knows_the_well_known_prefix() {
        assert!(is_well_known(WELL_KNOWN_PREFIX));
        assert!(!is_well_known("64:ff9b:1::".parse().unwrap()));
        assert!(!is_well_known("2001:db8:64::".parse().unwrap()));
    }

    #[test]
    fn tells_global_addresses() {
        for address in [
            "1.2.3.4",
            "8.8.8.8",
            "192.0.0.9",
            "100.63.255.255",
            "172.32.0.1",
        ] {
            assert!(is_global(address.parse().unwrap()), "{address}");
        }
        for address in [
            "0.1.2.3",
            "10.0.0.1",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.1.1",
            "172.16.0.1",
            "172.31.255.255",
            "192.0.0.170",
            "192.0.2.1",
            "192.168.1.1",
            "198.19.0.1",
            "198.51.100.1",
            "203.0.113.1",
            "224.0.0.1",
            "255.255.255.255",
        ] {
            assert!(!is_global(address.parse().unwrap()), "{address}");
        }
    }

    #[test]
    fn excludes_ipv4_mapped_addresses() {
        assert!(is_excluded("::ffff:1.2.3.4".parse().unwrap()));
        assert!(!is_excluded("2001:db8::1".parse().unwrap()));
        assert!(!is_excluded("64:ff9b::102:304".parse().unwrap()));

        let mut response = a_answer(vec![
            Record::from_rdata(
                name("www.example.com."),
                60,
                RData::AAAA(AAAA("::ffff:1.2.3.4".parse().unwrap())),
            ),
            Record::from_rdata(
                name("www.example.com."),
                60,
                RData::AAAA(AAAA("2001:db8::1".parse().unwrap())),
            ),
        ]);
        exclude(&mut response);
        assert_eq!(
            aaaa_addresses(&response),
            ["2001:db8::1".parse::<Ipv6Addr>().unwrap()]
        );
        response.answers_mut().clear();
        assert!(!has_aaaa(&response));
    }

    #[test]
    fn synthesizes_with_the_chain_and_the_a_records_ttl() {
        let cname = Record::from_rdata(
            name("www.example.com."),
            300,
            RData::CNAME(CNAME(name("cdn.example.net."))),
        );
        let answer = a_answer(vec![
            cname.clone(),
            a("cdn.example.net.", 30, [1, 2, 3, 4]),
            a("cdn.example.net.", 900, [5, 6, 7, 8]),
        ]);
        let synthesized = synthesize(&answer, WELL_KNOWN_PREFIX, 600).unwrap();
        assert!(!synthesized.authentic_data());
        assert_eq!(synthesized.answers()[0], cname);
        let records = &synthesized.answers()[1..];
        assert_eq!(records.len(), 2);
        assert_eq!(records[0].name(), &name("cdn.example.net."));
        assert_eq!(records[0].ttl(), 30);
        assert_eq!(records[1].ttl(), 600);
        assert_eq!(
            aaaa_addresses(&synthesized),
            [
                "64:ff9b::102:304".parse::<Ipv6Addr>().unwrap(),
                "64:ff9b::506:708".parse().unwrap()
            ]
        );
    }

    #[test]
    fn synthesizes_no_non_global_address_under_the_well_known_prefix() {
        let answer = a_answer(vec![
            a("www.example.com.", 60, [10, 0, 0, 1]),
            a("www.example.com.", 60, [1, 2, 3, 4]),
        ]);
        let synthesized = synthesize(&answer, WELL_KNOWN_PREFIX, 600).unwrap();
        assert_eq!(
            aaaa_addresses(&synthesized),
            ["64:ff9b::102:304".parse::<Ipv6Addr>().unwrap()]
        );

        let private = a_answer(vec![a("www.example.com.", 60, [192, 168, 1, 1])]);
        assert!(synthesize(&private, WELL_KNOWN_PREFIX, 600).is_none());
        // A prefix of the network's own may hold any address.
        let prefix = "2001:db8:64::".parse().unwrap();
        let synthesized = synthesize(&private, prefix, 600).unwrap();
        assert_eq!(
            aaaa_addresses(&synthesized),
            ["2001:db8:64::c0a8:101".parse::<Ipv6Addr>().unwrap()]
        );
    }

    #[test]
    fn synthesizes_nothing_without_a_records() {
        assert!(synthesize(&a_answer(Vec::new()), WELL_KNOWN_PREFIX, 600).is_none());
    }

    #[test]
    fn caps_the_ttl_by_the_soa_record() {
        assert_eq!(negative_ttl(None), 600);
        let mut aaaa = a_answer(Vec::new());
        assert_eq!(negative_ttl(Some(&aaaa)), 600);
        let soa = SOA::new(
            name("ns.example.com."),
            name("hostmaster.example.com."),
            1,
            3600,
            600,
            86400,
            120,
        );
        aaaa.add_name_server(Record::from_rdata(
            name("example.com."),
            300,
            RData::SOA(soa),
        ));
        assert_eq!(negative_ttl(Some(&aaaa)), 120);
    }

    #[test]
    fn keeps_the_chain_of_a_negative_answer() {
        let cname = Record::from_rdata(
            name("www.example.com."),
            300,
            RData::CNAME(CNAME(name("cdn.example.net."))),
        );
        let answer = a_answer(vec![cname.clone(), a("cdn.example.net.", 30, [1, 2, 3, 4])]);
        assert_eq!(without_a(answer).answers(), [cname]);
    }

    #[test]
    fn maps_reverse_names_under_the_prefix() {
        let address: Ipv6Addr = "64:ff9b::c000:221".parse().unwrap();
        assert_eq!(
            reverse(&Name::from(address), WELL_KNOWN_PREFIX),
            Some(name("33.2.0.192.in-addr.arpa."))
        );
        let other: Ipv6Addr = "2001:db8::c000:221".parse().unwrap();
        assert_eq!(reverse(&Name::from(other), WELL_KNOWN_PREFIX), None);
        assert_eq!(reverse(&name("9.f.f.ip6.arpa."), WELL_KNOWN_PREFIX), None);
        assert_eq!(reverse(&name("www.example.com."), WELL_KNOWN_PREFIX), None);
    }

    #[test]
    fn answers_ipv4only_arpa() {
        assert!(is_ipv4only_arpa(&name("IPv4only.Arpa.")));
        assert!(!is_ipv4only_arpa(&name("www.ipv4only.arpa.")));
        assert!(!is_ipv4only_arpa(&name("arpa.")));

        let query = Query::query(name("ipv4only.arpa."), RecordType::AAAA);
        assert_eq!(
            aaaa_addresses(&ipv4only(&query, WELL_KNOWN_PREFIX)),
            [
                "64:ff9b::c000:aa".parse::<Ipv6Addr>().unwrap(),
                "64:ff9b::c000:ab".parse().unwrap()
            ]
        );
        let query = Query::query(name("ipv4only.arpa."), RecordType::A);
        assert_eq!(ipv4only(&query, WELL_KNOWN_PREFIX).answers().len(), 2);
        let query = Query::query(name("ipv4only.arpa."), RecordType::MX);
        assert!(ipv4only(&query, WELL_KNOWN_PREFIX).answers().is_empty());
    }
}
