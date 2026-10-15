// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Tests against the kernel, in two network namespaces of their own: a
//! pod's side with `fd00:feed::1`, and a server's with
//! `64:ff9b::c000:202` (192.0.2.2 behind NAT64), on a veth pair. They
//! need root and `ip`, and are ignored by default:
//!
//! ```text
//! cargo test -p traffic-service --no-run
//! sudo target/debug/deps/traffic_service-<hash> --ignored --test-threads 1
//! ```
//!
//! The nftables rules are written over netlink, so `nft` is not needed.
//! `capture_test_vectors` prints the messages of testdata.rs anew.

use crate::netlink::{self, Socket};
use crate::server_name::tests::{browser_like, client_hello};
use crate::{conntrack, enable_accounting, nflog, Collector, Config, Pods};
use feos_proto::network_service::{
    traffic_record::Record, DropReason, ServerNameSource, TrafficRecord,
};
use std::fs::File;
use std::io::{self, Read, Write};
use std::net::{Ipv6Addr, SocketAddr, TcpListener, TcpStream, UdpSocket};
use std::process::Command;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};
use tokio::sync::broadcast;

const POD: &str = "fd00:feed::1";
const SERVER: &str = "64:ff9b::c000:202";

struct PodA;

impl Pods for PodA {
    fn by_address(&self, address: &Ipv6Addr) -> Option<String> {
        (address.to_string() == POD).then(|| "pod-a".to_string())
    }
}

fn ip(args: &[&str]) {
    let status = Command::new("ip").args(args).status().expect("ip");
    assert!(status.success(), "ip {}", args.join(" "));
}

/// The two namespaces; deleted when dropped.
struct Net {
    pod: String,
    server: String,
}

impl Net {
    fn new(tag: &str) -> Self {
        let net = Self {
            pod: format!("feos-traffic-{tag}-pod"),
            server: format!("feos-traffic-{tag}-srv"),
        };
        let (pod, server) = (net.pod.as_str(), net.server.as_str());
        ip(&["netns", "add", pod]);
        ip(&["netns", "add", server]);
        ip(&[
            "-n", pod, "link", "add", "veth0", "type", "veth", "peer", "name", "veth0", "netns",
            server,
        ]);
        for (ns, address, other) in [
            (pod, "fd00:feed::1/64", "64:ff9b::/96"),
            (server, "64:ff9b::c000:202/96", "fd00:feed::/64"),
        ] {
            ip(&["-n", ns, "addr", "add", address, "dev", "veth0", "nodad"]);
            ip(&["-n", ns, "link", "set", "veth0", "up"]);
            ip(&["-n", ns, "link", "set", "lo", "up"]);
            ip(&["-n", ns, "-6", "route", "add", other, "dev", "veth0"]);
        }
        ip(&["-n", pod, "addr", "add", "192.0.2.1/24", "dev", "veth0"]);
        ip(&["-n", server, "addr", "add", "192.0.2.2/24", "dev", "veth0"]);
        net
    }

    fn in_pod<T: Send>(&self, f: impl FnOnce() -> T + Send) -> T {
        within(&self.pod, f)
    }

    /// Serves `port` in the server's namespace: reads a connection to its
    /// end, and answers with `answer` bytes.
    fn serve(
        &self,
        port: u16,
        connections: usize,
        answer: usize,
    ) -> thread::JoinHandle<Vec<Vec<u8>>> {
        let server = self.server.clone();
        let (ready, listening) = std::sync::mpsc::channel();
        let handle = thread::spawn(move || {
            enter(&server);
            let listener = TcpListener::bind(format!("[{SERVER}]:{port}")).unwrap();
            ready.send(()).unwrap();
            let mut received = Vec::new();
            for _ in 0..connections {
                let (mut stream, _) = listener.accept().unwrap();
                let mut buf = Vec::new();
                stream.read_to_end(&mut buf).unwrap();
                stream.write_all(&vec![b'x'; answer]).unwrap();
                received.push(buf);
            }
            received
        });
        listening.recv().unwrap();
        handle
    }
}

impl Drop for Net {
    fn drop(&mut self) {
        for ns in [&self.pod, &self.server] {
            let _ = Command::new("ip").args(["netns", "del", ns]).status();
        }
    }
}

/// Moves the calling thread into a named network namespace.
fn enter(ns: &str) {
    let file = File::open(format!("/run/netns/{ns}")).unwrap();
    nix::sched::setns(file, nix::sched::CloneFlags::CLONE_NEWNET).unwrap();
}

fn within<T: Send>(ns: &str, f: impl FnOnce() -> T + Send) -> T {
    thread::scope(|s| {
        s.spawn(|| {
            enter(ns);
            f()
        })
        .join()
        .unwrap()
    })
}

/// Connects from the pod to the server, sends `parts` with a pause
/// between them (a segment each), shuts its side down and reads the
/// answer.
fn talk(port: u16, parts: &[&[u8]]) -> usize {
    let address: SocketAddr = format!("[{SERVER}]:{port}").parse().unwrap();
    let mut stream = TcpStream::connect(address).unwrap();
    stream.set_nodelay(true).unwrap();
    for part in parts {
        stream.write_all(part).unwrap();
        thread::sleep(Duration::from_millis(50));
    }
    stream.shutdown(std::net::Shutdown::Write).unwrap();
    let mut answer = Vec::new();
    stream.read_to_end(&mut answer).unwrap();
    answer.len()
}

/// Ends every tracked connection of the calling thread's namespace: each
/// has a DESTROY event.
fn flush_conntrack() {
    let mut socket = Socket::open(0).unwrap();
    socket.set_timeout(Duration::from_secs(5)).unwrap();
    socket
        .request(|seq| conntrack::flush_request(seq, libc::AF_INET6 as u8))
        .unwrap();
}

/// nftables over netlink, as little of it as the tests need: a table
/// `feos-traffic` of the inet family with an output chain.
mod nft {
    use super::*;

    pub enum Expr {
        /// Loads the conntrack state, which turns tracking on.
        CtState,
        L4Proto(u8),
        DestinationPort(u16),
        Log {
            group: u16,
            prefix: &'static str,
            snaplen: Option<u32>,
        },
        Drop,
    }

    const NLA_F_NESTED: u16 = 0x8000;
    const NFNL_SUBSYS_NFTABLES: u16 = 10;
    const INET: u8 = 1;

    struct Attrs(Vec<u8>);

    impl Attrs {
        fn attr(&mut self, kind: u16, payload: &[u8]) -> &mut Self {
            self.0
                .extend_from_slice(&((4 + payload.len()) as u16).to_ne_bytes());
            self.0.extend_from_slice(&kind.to_ne_bytes());
            self.0.extend_from_slice(payload);
            while !self.0.len().is_multiple_of(4) {
                self.0.push(0);
            }
            self
        }

        fn str(&mut self, kind: u16, value: &str) -> &mut Self {
            self.attr(kind, &[value.as_bytes(), &[0]].concat())
        }

        fn u32(&mut self, kind: u16, value: u32) -> &mut Self {
            self.attr(kind, &value.to_be_bytes())
        }

        fn nested(&mut self, kind: u16, fill: impl FnOnce(&mut Attrs)) -> &mut Self {
            let mut inner = Attrs(Vec::new());
            fill(&mut inner);
            self.attr(kind | NLA_F_NESTED, &inner.0)
        }
    }

    fn message(kind: u16, flags: u16, seq: u32, family: u8, res_id: u16, attrs: &Attrs) -> Vec<u8> {
        let mut buf = netlink::request(kind, flags, seq, family, res_id, &[]);
        buf.extend_from_slice(&attrs.0);
        let len = buf.len() as u32;
        buf[..4].copy_from_slice(&len.to_ne_bytes());
        buf
    }

    /// An expression in a rule's list: NFTA_LIST_ELEM, with NFTA_EXPR_NAME
    /// and NFTA_EXPR_DATA. Registers are register 1.
    fn expression(list: &mut Attrs, expr: &Expr) {
        let cmp = |list: &mut Attrs, data: &[u8]| {
            list.nested(1, |e| {
                e.str(1, "cmp").nested(2, |d| {
                    // NFTA_CMP_SREG, NFTA_CMP_OP (eq), NFTA_CMP_DATA.
                    d.u32(1, 1).u32(2, 0).nested(3, |v| {
                        v.attr(1, data);
                    });
                });
            });
        };
        match expr {
            Expr::CtState => {
                list.nested(1, |e| {
                    e.str(1, "ct").nested(2, |d| {
                        // NFTA_CT_DREG, NFTA_CT_KEY (state).
                        d.u32(1, 1).u32(2, 0);
                    });
                });
            }
            Expr::L4Proto(proto) => {
                list.nested(1, |e| {
                    e.str(1, "meta").nested(2, |d| {
                        // NFTA_META_DREG, NFTA_META_KEY (l4proto).
                        d.u32(1, 1).u32(2, 16);
                    });
                });
                cmp(list, &[*proto]);
            }
            Expr::DestinationPort(port) => {
                list.nested(1, |e| {
                    e.str(1, "payload").nested(2, |d| {
                        // NFTA_PAYLOAD_DREG, _BASE (transport), _OFFSET, _LEN.
                        d.u32(1, 1).u32(2, 2).u32(3, 2).u32(4, 2);
                    });
                });
                cmp(list, &port.to_be_bytes());
            }
            Expr::Log {
                group,
                prefix,
                snaplen,
            } => {
                list.nested(1, |e| {
                    e.str(1, "log").nested(2, |d| {
                        // NFTA_LOG_GROUP, NFTA_LOG_PREFIX, NFTA_LOG_SNAPLEN.
                        d.attr(1, &group.to_be_bytes()).str(2, prefix);
                        if let Some(snaplen) = snaplen {
                            d.u32(3, *snaplen);
                        }
                    });
                });
            }
            Expr::Drop => {
                list.nested(1, |e| {
                    e.str(1, "immediate").nested(2, |d| {
                        // NFTA_IMMEDIATE_DREG (verdict), NFTA_IMMEDIATE_DATA
                        // with NFTA_DATA_VERDICT and NFTA_VERDICT_CODE (drop).
                        d.u32(1, 0).nested(2, |data| {
                            data.nested(2, |verdict| {
                                verdict.u32(1, 0);
                            });
                        });
                    });
                });
            }
        }
    }

    /// Writes the table with these rules, in the calling thread's
    /// namespace.
    pub fn apply(rules: &[Vec<Expr>]) {
        // NFT_MSG_NEWTABLE 0, NEWCHAIN 3, NEWRULE 6; batches begin with
        // NFNL_MSG_BATCH_BEGIN (0x10) and end with NFNL_MSG_BATCH_END.
        let kind = |message: u16| NFNL_SUBSYS_NFTABLES << 8 | message;
        let ack = netlink::NLM_F_REQUEST | netlink::NLM_F_ACK;
        let (create, append) = (0x400, 0x800);
        let mut batch = netlink::request(
            0x10,
            netlink::NLM_F_REQUEST,
            1,
            0,
            NFNL_SUBSYS_NFTABLES,
            &[],
        );
        let mut seq = 2;
        let mut table = Attrs(Vec::new());
        table.str(1, "feos-traffic");
        batch.extend(message(kind(0), ack | create, seq, INET, 0, &table));
        seq += 1;
        // NFTA_CHAIN_TABLE, _NAME, _HOOK (output, priority 0), _POLICY
        // (accept), _TYPE.
        let mut chain = Attrs(Vec::new());
        chain
            .str(1, "feos-traffic")
            .str(3, "output")
            .nested(4, |hook| {
                hook.u32(1, 3).u32(2, 0);
            })
            .u32(5, 1)
            .str(7, "filter");
        batch.extend(message(kind(3), ack | create, seq, INET, 0, &chain));
        for exprs in rules {
            seq += 1;
            // NFTA_RULE_TABLE, _CHAIN, _EXPRESSIONS.
            let mut rule = Attrs(Vec::new());
            rule.str(1, "feos-traffic")
                .str(2, "output")
                .nested(4, |list| {
                    for expr in exprs {
                        expression(list, expr);
                    }
                });
            batch.extend(message(kind(6), ack | create | append, seq, INET, 0, &rule));
        }
        batch.extend(netlink::request(
            0x11,
            netlink::NLM_F_REQUEST,
            seq + 1,
            0,
            NFNL_SUBSYS_NFTABLES,
            &[],
        ));

        let socket = Socket::open(0).unwrap();
        socket.set_timeout(Duration::from_secs(5)).unwrap();
        socket.send(&batch).unwrap();
        // An acknowledgement for each message but begin and end.
        let mut acks = 0;
        let mut buf = vec![0u8; 65536];
        while acks < seq - 1 {
            let n = socket.recv(&mut buf).unwrap();
            for message in netlink::messages(&buf[..n]) {
                if message.kind == 2 {
                    let code = i32::from_ne_bytes(message.payload[..4].try_into().unwrap());
                    assert_eq!(code, 0, "nftables: {}", io::Error::from_raw_os_error(-code));
                    acks += 1;
                }
            }
        }
    }
}

/// Waits for a record that `wanted` takes, up to five seconds.
fn wait_for<T>(
    rx: &mut broadcast::Receiver<TrafficRecord>,
    mut wanted: impl FnMut(&TrafficRecord) -> Option<T>,
) -> T {
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        match rx.try_recv() {
            Ok(record) => {
                if let Some(found) = wanted(&record) {
                    return found;
                }
            }
            Err(_) => thread::sleep(Duration::from_millis(10)),
        }
    }
    panic!("no such record");
}

fn start(net: &Net, rules: &[Vec<nft::Expr>]) -> (Collector, broadcast::Receiver<TrafficRecord>) {
    let (records, rx) = broadcast::channel(1024);
    let collector = net.in_pod(|| {
        nft::apply(rules);
        enable_accounting().unwrap();
        Collector::start(Arc::new(PodA), Config::default(), records).unwrap()
    });
    (collector, rx)
}

#[test]
#[ignore]
fn flows_from_conntrack() {
    let net = Net::new("flows");
    let (collector, mut rx) = start(&net, &[vec![nft::Expr::CtState]]);
    let server = net.serve(8080, 1, 100);
    let answer = net.in_pod(|| talk(8080, &[&[b'a'; 1000]]));
    assert_eq!(answer, 100);
    assert_eq!(server.join().unwrap()[0].len(), 1000);

    let flow = |ended| {
        move |record: &TrafficRecord| match &record.record {
            Some(Record::Flow(flow)) if flow.ended == ended => {
                assert_eq!(record.pod_id, "pod-a");
                Some(flow.clone())
            }
            _ => None,
        }
    };
    let begun = wait_for(&mut rx, flow(false));
    let tuple = begun.tuple.clone().unwrap();
    assert_eq!((tuple.protocol, tuple.source_address.as_str()), (6, POD));
    assert_eq!(
        (tuple.destination_address.as_str(), tuple.destination_port),
        ("192.0.2.2", 8080)
    );
    assert!(begun.outbound && begun.start.is_some() && begun.end.is_none());

    net.in_pod(flush_conntrack);
    let ended = wait_for(&mut rx, flow(true));
    assert_eq!(ended.tuple, begun.tuple);
    assert!(ended.outbound);
    // The payload, and a header of 60 bytes or more for each packet.
    assert!(
        ended.bytes_sent >= 1000 + 60 * ended.packets_sent,
        "{ended:?}"
    );
    assert!(
        ended.bytes_received >= 100 + 60 * ended.packets_received,
        "{ended:?}"
    );
    assert!(
        ended.packets_sent >= 4 && ended.packets_received >= 3,
        "{ended:?}"
    );
    let (start, end) = (ended.start.unwrap(), ended.end.unwrap());
    assert!((start.seconds, start.nanos) <= (end.seconds, end.nanos));
    collector.stop();
}

#[test]
#[ignore]
fn server_names_from_copies() {
    let net = Net::new("names");
    let copy = |port| {
        vec![
            nft::Expr::L4Proto(6),
            nft::Expr::DestinationPort(port),
            nft::Expr::Log {
                group: 3,
                prefix: "copy",
                snaplen: Some(2048),
            },
        ]
    };
    let (collector, mut rx) = start(&net, &[vec![nft::Expr::CtState], copy(8443), copy(8080)]);
    let tls = net.serve(8443, 1, 0);
    let http = net.serve(8080, 1, 0);
    let flight = browser_like(&client_hello("kernel.test.example", true));
    net.in_pod(|| {
        talk(8443, &[&flight[..700], &flight[700..]]);
        talk(8080, &[b"GET / HTTP/1.1\r\nHost: plain.example\r\n\r\n"]);
    });
    assert_eq!(tls.join().unwrap()[0], flight);
    http.join().unwrap();

    let mut names = Vec::new();
    for _ in 0..2 {
        names.push(wait_for(&mut rx, |record| match &record.record {
            Some(Record::ServerName(name)) => Some(name.clone()),
            _ => None,
        }));
    }
    names.sort_by_key(|name| name.tuple.as_ref().unwrap().destination_port);
    assert_eq!(names[0].name, "plain.example");
    assert_eq!(names[0].source(), ServerNameSource::Http);
    assert_eq!(names[1].name, "kernel.test.example");
    assert_eq!(names[1].source(), ServerNameSource::Tls);
    let tuple = names[1].tuple.as_ref().unwrap();
    assert_eq!(
        (
            tuple.source_address.as_str(),
            tuple.destination_address.as_str()
        ),
        (POD, "192.0.2.2")
    );
    collector.stop();
}

#[test]
#[ignore]
fn drops_from_nflog() {
    let net = Net::new("drops");
    let rule = vec![
        nft::Expr::L4Proto(17),
        nft::Expr::DestinationPort(9999),
        nft::Expr::Log {
            group: 2,
            prefix: "deny",
            snaplen: None,
        },
        nft::Expr::Drop,
    ];
    let (collector, mut rx) = start(&net, &[rule]);
    net.in_pod(|| {
        let socket = UdpSocket::bind(format!("[{POD}]:0")).unwrap();
        // The drop in output fails the send.
        assert!(socket
            .send_to(b"dropped", format!("[{SERVER}]:9999"))
            .is_err());
    });
    let (pod_id, drop) = wait_for(&mut rx, |record| match &record.record {
        Some(Record::Drop(drop)) => Some((record.pod_id.clone(), drop.clone())),
        _ => None,
    });
    assert_eq!(pod_id, "pod-a");
    assert!(drop.outbound);
    assert_eq!(drop.reason(), DropReason::DeniedPrefix);
    let tuple = drop.tuple.unwrap();
    assert_eq!(
        (
            tuple.protocol,
            tuple.destination_address.as_str(),
            tuple.destination_port
        ),
        (17, "192.0.2.2", 9999)
    );
    collector.stop();
}

/// A socket the kernel cannot hand its events over to drops them, and
/// counts them where `drops` reads.
#[test]
#[ignore]
fn lost_events_are_counted() {
    let net = Net::new("lost");
    net.in_pod(|| {
        nft::apply(&[vec![nft::Expr::CtState]]);
        let socket = conntrack::open().unwrap();
        socket.set_receive_buffer(4096);
        socket.set_timeout(Duration::from_millis(100)).unwrap();
        let before = socket.drops().unwrap();
        let udp = UdpSocket::bind(format!("[{POD}]:0")).unwrap();
        for port in 1000..1300 {
            let _ = udp.send_to(b"x", format!("[{SERVER}]:{port}"));
        }
        let mut buf = vec![0u8; 65536];
        let mut overrun = false;
        loop {
            match socket.recv(&mut buf) {
                Ok(_) => {}
                Err(e) if e.raw_os_error() == Some(libc::ENOBUFS) => overrun = true,
                Err(_) => break,
            }
        }
        assert!(overrun);
        assert!(socket.drops().unwrap() > before);
    });
}

/// Prints the messages of testdata.rs, from the kernel.
#[test]
#[ignore]
fn capture_test_vectors() {
    let net = Net::new("capture");
    let server = net.serve(8080, 1, 100);
    let tls = net.serve(8443, 1, 0);
    let flight = browser_like(&client_hello("kernel.test.example", true));
    let copy = vec![
        nft::Expr::L4Proto(6),
        nft::Expr::DestinationPort(8443),
        nft::Expr::Log {
            group: 3,
            prefix: "copy",
            snaplen: Some(2048),
        },
    ];
    let deny = vec![
        nft::Expr::L4Proto(17),
        nft::Expr::DestinationPort(9999),
        nft::Expr::Log {
            group: 2,
            prefix: "deny",
            snaplen: None,
        },
        nft::Expr::Drop,
    ];
    let pod = net.pod.clone();
    let (flows, drops, copies) = net.in_pod(move || {
        nft::apply(&[vec![nft::Expr::CtState], copy, deny]);
        enable_accounting().unwrap();
        let flows = conntrack::open().unwrap();
        let drops = nflog::open(2, 256).unwrap();
        let copies = nflog::open(3, u16::MAX).unwrap();
        talk(8080, &[&[b'a'; 1000]]);
        talk(8443, &[&flight[..700], &flight[700..]]);
        let udp = UdpSocket::bind(format!("[{POD}]:0")).unwrap();
        let _ = udp.send_to(b"x", format!("[{SERVER}]:5353"));
        let _ = udp.send_to(b"dropped", format!("[{SERVER}]:9999"));
        let udp4 = UdpSocket::bind("192.0.2.1:0").unwrap();
        let _ = udp4.send_to(b"x", "192.0.2.2:5353");
        let status = Command::new("ip")
            .args([
                "netns", "exec", &pod, "ping", "-6", "-c", "1", "-W", "1", SERVER,
            ])
            .status()
            .unwrap();
        assert!(status.success());
        flush_conntrack();
        (flows, drops, copies)
    });
    server.join().unwrap();
    tls.join().unwrap();

    let read = |socket: &Socket| {
        // Longer than nflog's flush timeout.
        socket.set_timeout(Duration::from_millis(1500)).unwrap();
        let mut buf = vec![0u8; 65536];
        let mut messages = Vec::new();
        // A message each: an event, or a packet.
        while let Ok(n) = socket.recv(&mut buf) {
            messages.push(buf[..n].to_vec());
        }
        messages
    };
    let events = read(&flows);
    let find = |name: &str, ended: bool, protocol: u8, port: u16| {
        let event = events.iter().find(|bytes| {
            let message = netlink::messages(bytes).next().unwrap();
            conntrack::parse(&message).is_some_and(|e| {
                e.ended == ended
                    && e.original.protocol == protocol
                    && (e.original.destination_port == port)
                    && (name != "CT_NEW_UDP4" || e.original.source.is_ipv4())
                    && (name != "CT_NEW_UDP" || e.original.source.is_ipv6())
            })
        });
        print_vector(name, event.expect(name));
    };
    find("CT_NEW_TCP", false, 6, 8080);
    find("CT_DESTROY_TCP", true, 6, 8080);
    find("CT_NEW_UDP", false, 17, 5353);
    find("CT_NEW_UDP4", false, 17, 5353);
    find("CT_NEW_ICMPV6", false, 58, 128);
    print_vector("NFLOG_DROP_UDP", &read(&drops)[0]);
    // nflog sends packets in batches: all of them, in one or more.
    print_vector("NFLOG_COPIES", &read(&copies).concat());
}

fn print_vector(name: &str, bytes: &[u8]) {
    println!("pub const {name}: &[u8] = &{};", hex(bytes));
}

fn hex(bytes: &[u8]) -> String {
    let items: Vec<String> = bytes.iter().map(|b| format!("0x{b:02x}")).collect();
    format!("[{}]", items.join(", "))
}
