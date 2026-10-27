// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Tests against the kernel, in three network namespaces of their own: a
//! pod, the node and a server, as ADR 0010 has them. The pod has
//! `fd00:fe05:1::1` and `169.254.16.5` on a veth whose other end is the
//! node's `pod-0123abcd`, where the node is `fe80::1` and `169.254.0.1`,
//! as on a TAP. The node routes to the server, `fd00:fe05:2::2` and
//! `192.0.2.2`, and masquerades IPv4 to its own `192.0.2.1`. The
//! collector runs in the node's namespace, and the rules in its forward
//! chain. They need root and `ip` (and `ping`), and are ignored by
//! default:
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
use crate::{conntrack, delete_connections, enable_accounting, nflog, Collector, Config, Pods};
use feos_proto::network_service::{
    traffic_record::Record, DropReason, Flow, ServerNameSource, TrafficRecord, Tuple,
};
use std::fs::File;
use std::io::{self, Read, Write};
use std::net::{IpAddr, SocketAddr, TcpListener, TcpStream, UdpSocket};
use std::process::Command;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};
use tokio::sync::broadcast;

const POD: &str = "fd00:fe05:1::1";
const POD4: &str = "169.254.16.5";
const SERVER: &str = "fd00:fe05:2::2";
const SERVER4: &str = "192.0.2.2";
/// The node's address towards the server, which the pod's IPv4 is
/// masqueraded to.
const NODE4: &str = "192.0.2.1";
const TAP: &str = "pod-0123abcd";

struct PodA;

impl Pods for PodA {
    fn by_address(&self, address: IpAddr) -> Option<String> {
        [POD, POD4]
            .contains(&address.to_string().as_str())
            .then(|| "pod-a".to_string())
    }

    fn by_interface(&self, name: &str) -> Option<String> {
        (name == TAP).then(|| "pod-a".to_string())
    }
}

fn address(address: &str) -> IpAddr {
    address.parse().unwrap()
}

fn ip(args: &[&str]) {
    let status = Command::new("ip").args(args).status().expect("ip");
    assert!(status.success(), "ip {}", args.join(" "));
}

/// The three namespaces; deleted when dropped.
struct Net {
    pod: String,
    node: String,
    server: String,
}

impl Net {
    fn new(tag: &str) -> Self {
        let net = Self {
            pod: format!("feos-traffic-{tag}-pod"),
            node: format!("feos-traffic-{tag}-node"),
            server: format!("feos-traffic-{tag}-srv"),
        };
        let (pod, node, server) = (net.pod.as_str(), net.node.as_str(), net.server.as_str());
        for ns in [pod, node, server] {
            ip(&["netns", "add", ns]);
            ip(&["-n", ns, "link", "set", "lo", "up"]);
        }
        ip(&[
            "-n", pod, "link", "add", "veth0", "type", "veth", "peer", "name", TAP, "netns", node,
        ]);
        ip(&[
            "-n", node, "link", "add", "uplink", "type", "veth", "peer", "name", "veth0", "netns",
            server,
        ]);
        let commands = [
            (pod, "addr add fd00:fe05:1::1/128 dev veth0 nodad"),
            (pod, "addr add 169.254.16.5/32 dev veth0"),
            (pod, "link set veth0 up"),
            (pod, "-6 route add default via fe80::1 dev veth0"),
            (pod, "-4 route add 169.254.0.1 dev veth0"),
            (pod, "-4 route add default via 169.254.0.1"),
            (node, "addr add fe80::1/64 dev pod-0123abcd nodad"),
            (node, "addr add 169.254.0.1/32 dev pod-0123abcd"),
            (node, "link set pod-0123abcd up"),
            (node, "-6 route add fd00:fe05:1::1/128 dev pod-0123abcd"),
            (node, "-4 route add 169.254.16.5/32 dev pod-0123abcd"),
            (node, "addr add fd00:fe05:2::1/64 dev uplink nodad"),
            (node, "addr add 192.0.2.1/24 dev uplink"),
            (node, "link set uplink up"),
            (server, "addr add fd00:fe05:2::2/64 dev veth0 nodad"),
            (server, "addr add 192.0.2.2/24 dev veth0"),
            (server, "link set veth0 up"),
            (server, "-6 route add fd00:fe05:1::/64 via fd00:fe05:2::1"),
        ];
        for (ns, command) in commands {
            let args: Vec<&str> = ["-n", ns].into_iter().chain(command.split(' ')).collect();
            ip(&args);
        }
        net.in_node(|| {
            for (name, value) in [("ipv4/ip_forward", "1"), ("ipv6/conf/all/forwarding", "1")] {
                std::fs::write(format!("/proc/sys/net/{name}"), value).unwrap();
            }
        });
        net
    }

    fn in_pod<T: Send>(&self, f: impl FnOnce() -> T + Send) -> T {
        within(&self.pod, f)
    }

    fn in_node<T: Send>(&self, f: impl FnOnce() -> T + Send) -> T {
        within(&self.node, f)
    }

    /// Serves `port` at `at` in the server's namespace: reads a
    /// connection to its end, and answers with `answer` bytes. What each
    /// connection sent, and where from.
    fn serve(
        &self,
        at: &str,
        port: u16,
        connections: usize,
        answer: usize,
    ) -> thread::JoinHandle<Vec<(SocketAddr, Vec<u8>)>> {
        let server = self.server.clone();
        let address = SocketAddr::new(address(at), port);
        let (ready, listening) = std::sync::mpsc::channel();
        let handle = thread::spawn(move || {
            enter(&server);
            let listener = TcpListener::bind(address).unwrap();
            ready.send(()).unwrap();
            let mut received = Vec::new();
            for _ in 0..connections {
                let (mut stream, peer) = listener.accept().unwrap();
                let mut buf = Vec::new();
                stream.read_to_end(&mut buf).unwrap();
                stream.write_all(&vec![b'x'; answer]).unwrap();
                received.push((peer, buf));
            }
            received
        });
        listening.recv().unwrap();
        handle
    }

    /// Pings `address` from the pod, once.
    fn ping(&self, address: &str) {
        let status = Command::new("ip")
            .args(["netns", "exec", &self.pod, "ping", "-c", "1", "-W", "1"])
            .arg(address)
            .stdout(std::process::Stdio::null())
            .status()
            .unwrap();
        assert!(status.success(), "ping {address}");
    }
}

impl Drop for Net {
    fn drop(&mut self) {
        for ns in [&self.pod, &self.node, &self.server] {
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

/// Connects to the server at `to`, sends `parts` with a pause between
/// them (a segment each), shuts its side down and reads the answer.
fn talk(to: &str, port: u16, parts: &[&[u8]]) -> usize {
    let mut stream = TcpStream::connect(SocketAddr::new(address(to), port)).unwrap();
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

/// Sends a UDP datagram from `source` (port chosen by the kernel) to
/// `destination`.
fn send_udp(source: &str, destination: &str, port: u16, payload: &[u8]) {
    let socket = UdpSocket::bind(SocketAddr::new(address(source), 0)).unwrap();
    socket
        .send_to(payload, SocketAddr::new(address(destination), port))
        .unwrap();
}

/// Ends every tracked connection of the calling thread's namespace, of
/// both families: each has a DESTROY event.
fn flush_conntrack() {
    let mut socket = Socket::open(0).unwrap();
    socket.set_timeout(Duration::from_secs(5)).unwrap();
    socket
        .request(|seq| conntrack::flush_request(seq, libc::AF_UNSPEC as u8))
        .unwrap();
}

/// nftables over netlink, as little of it as the tests need: a table
/// `feos-traffic` of the inet family with a forward chain, and a
/// postrouting chain that masquerades IPv4.
mod nft {
    use super::*;

    pub enum Expr {
        /// Loads the conntrack state, which turns tracking on.
        CtState,
        NfProto(u8),
        L4Proto(u8),
        DestinationPort(u16),
        Log {
            group: u16,
            prefix: &'static str,
            snaplen: Option<u32>,
        },
        Drop,
        Masquerade,
    }

    const NLA_F_NESTED: u16 = 0x8000;
    const NFNL_SUBSYS_NFTABLES: u16 = 10;
    const INET: u8 = 1;
    const NFPROTO_IPV4: u8 = 2;

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
        // NFTA_META_DREG, NFTA_META_KEY.
        let meta = |list: &mut Attrs, key: u32| {
            list.nested(1, |e| {
                e.str(1, "meta").nested(2, |d| {
                    d.u32(1, 1).u32(2, key);
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
            Expr::NfProto(proto) => {
                meta(list, 15);
                cmp(list, &[*proto]);
            }
            Expr::L4Proto(proto) => {
                meta(list, 16);
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
            Expr::Masquerade => {
                list.nested(1, |e| {
                    e.str(1, "masq");
                });
            }
        }
    }

    /// Writes the table, in the calling thread's namespace: a forward
    /// chain with these rules, and IPv4's masquerade in postrouting.
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
        let masquerade = [vec![Expr::NfProto(NFPROTO_IPV4), Expr::Masquerade]];
        // Name, hook (forward 2, postrouting 4), priority, type.
        let chains = [
            ("forward", 2, 0, "filter", rules),
            ("postrouting", 4, 100, "nat", &masquerade[..]),
        ];
        for (name, hook, priority, chain_type, rules) in chains {
            seq += 1;
            // NFTA_CHAIN_TABLE, _NAME, _HOOK, _POLICY (accept), _TYPE.
            let mut chain = Attrs(Vec::new());
            chain
                .str(1, "feos-traffic")
                .str(3, name)
                .nested(4, |h| {
                    h.u32(1, hook).u32(2, priority);
                })
                .u32(5, 1)
                .str(7, chain_type);
            batch.extend(message(kind(3), ack | create, seq, INET, 0, &chain));
            for exprs in rules {
                seq += 1;
                // NFTA_RULE_TABLE, _CHAIN, _EXPRESSIONS.
                let mut rule = Attrs(Vec::new());
                rule.str(1, "feos-traffic").str(2, name).nested(4, |list| {
                    for expr in exprs {
                        expression(list, expr);
                    }
                });
                batch.extend(message(kind(6), ack | create | append, seq, INET, 0, &rule));
            }
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
    wanted: impl FnMut(&TrafficRecord) -> Option<T>,
) -> T {
    collect(rx, 1, wanted).remove(0)
}

/// Waits for `n` records that `wanted` takes, in any order, up to five
/// seconds. Records it does not take are passed over.
fn collect<T>(
    rx: &mut broadcast::Receiver<TrafficRecord>,
    n: usize,
    mut wanted: impl FnMut(&TrafficRecord) -> Option<T>,
) -> Vec<T> {
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut found = Vec::new();
    while found.len() < n {
        assert!(Instant::now() < deadline, "{} of {n} records", found.len());
        match rx.try_recv() {
            Ok(record) => found.extend(wanted(&record)),
            Err(_) => thread::sleep(Duration::from_millis(10)),
        }
    }
    found
}

/// The collector in the node's namespace, with these forward rules.
fn start(net: &Net, rules: &[Vec<nft::Expr>]) -> (Collector, broadcast::Receiver<TrafficRecord>) {
    let (records, rx) = broadcast::channel(1024);
    let collector = net.in_node(|| {
        nft::apply(rules);
        enable_accounting().unwrap();
        Collector::start(Arc::new(PodA), Config::default(), records).unwrap()
    });
    (collector, rx)
}

/// A flow of pod-a's that begun, or ended, whose tuple `of` takes.
fn flow(ended: bool, of: impl Fn(&Tuple) -> bool) -> impl FnMut(&TrafficRecord) -> Option<Flow> {
    move |record| match &record.record {
        Some(Record::Flow(flow)) if flow.ended == ended && flow.tuple.as_ref().is_some_and(&of) => {
            assert_eq!(record.pod_id, "pod-a");
            Some(flow.clone())
        }
        _ => None,
    }
}

/// Tuples of this protocol to this destination.
fn to(protocol: u32, destination: &'static str) -> impl Fn(&Tuple) -> bool {
    move |tuple| tuple.protocol == protocol && tuple.destination_address == destination
}

/// A pod's flows over IPv6 and IPv4, recorded with its own addresses: the
/// IPv4 one before the node masqueraded it.
#[test]
#[ignore]
fn flows_from_conntrack() {
    let net = Net::new("flows");
    let (collector, mut rx) = start(&net, &[vec![nft::Expr::CtState]]);
    for (pod, server) in [(POD, SERVER), (POD4, SERVER4)] {
        let listener = net.serve(server, 8080, 1, 100);
        let answer = net.in_pod(|| talk(server, 8080, &[&[b'a'; 1000]]));
        assert_eq!(answer, 100);
        let (peer, received) = listener.join().unwrap().remove(0);
        assert_eq!(received.len(), 1000);
        // The server sees the node's IPv4 address, the record the pod's.
        let seen_as = if server == SERVER4 { NODE4 } else { POD };
        assert_eq!(peer.ip(), address(seen_as));

        let begun = wait_for(&mut rx, flow(false, to(6, server)));
        let tuple = begun.tuple.clone().unwrap();
        assert_eq!(tuple.source_address, pod);
        assert_ne!(tuple.source_port, 0);
        assert_eq!(tuple.destination_port, 8080);
        assert!(begun.outbound && begun.start.is_some() && begun.end.is_none());

        net.in_node(flush_conntrack);
        let ended = wait_for(&mut rx, flow(true, to(6, server)));
        assert_eq!(ended.tuple, begun.tuple);
        assert!(ended.outbound);
        // The payload, and a header of 40 bytes (IPv4) or 60 (IPv6) or
        // more for each packet.
        let header = if pod == POD { 60 } else { 40 };
        assert!(
            ended.bytes_sent >= 1000 + header * ended.packets_sent,
            "{ended:?}"
        );
        assert!(
            ended.bytes_received >= 100 + header * ended.packets_received,
            "{ended:?}"
        );
        assert!(
            ended.packets_sent >= 4 && ended.packets_received >= 3,
            "{ended:?}"
        );
        let (start, end) = (ended.start.unwrap(), ended.end.unwrap());
        assert!((start.seconds, start.nanos) <= (end.seconds, end.nanos));
    }

    // Echo requests: their type, and their identifier as the source port.
    for (server, protocol, kind) in [(SERVER, 58, 128), (SERVER4, 1, 8)] {
        net.ping(server);
        let begun = wait_for(&mut rx, flow(false, to(protocol, server)));
        let tuple = begun.tuple.unwrap();
        assert_eq!(tuple.destination_port, kind);
        assert_ne!(tuple.source_port, 0);
        assert!(begun.outbound);
    }
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
    let flight = browser_like(&client_hello("kernel.test.example", true));
    let tls = net.serve(SERVER, 8443, 1, 0);
    let tls4 = net.serve(SERVER4, 8443, 1, 0);
    let http4 = net.serve(SERVER4, 8080, 1, 0);
    net.in_pod(|| {
        talk(SERVER, 8443, &[&flight[..700], &flight[700..]]);
        talk(SERVER4, 8443, &[&flight[..700], &flight[700..]]);
        talk(
            SERVER4,
            8080,
            &[b"GET / HTTP/1.1\r\nHost: plain.example\r\n\r\n"],
        );
    });
    assert_eq!(tls.join().unwrap()[0].1, flight);
    assert_eq!(tls4.join().unwrap()[0].1, flight);
    http4.join().unwrap();

    let mut names = Vec::new();
    for _ in 0..3 {
        names.push(wait_for(&mut rx, |record| match &record.record {
            Some(Record::ServerName(name)) => {
                assert_eq!(record.pod_id, "pod-a");
                Some(name.clone())
            }
            _ => None,
        }));
    }
    let mut found: Vec<_> = names
        .iter()
        .map(|name| {
            let tuple = name.tuple.as_ref().unwrap();
            (
                tuple.source_address.as_str(),
                tuple.destination_address.as_str(),
                tuple.destination_port,
                name.source(),
                name.name.as_str(),
            )
        })
        .collect();
    found.sort();
    assert_eq!(
        found,
        [
            (POD4, SERVER4, 8080, ServerNameSource::Http, "plain.example"),
            (
                POD4,
                SERVER4,
                8443,
                ServerNameSource::Tls,
                "kernel.test.example"
            ),
            (
                POD,
                SERVER,
                8443,
                ServerNameSource::Tls,
                "kernel.test.example"
            ),
        ]
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
    for (pod, server) in [(POD, SERVER), (POD4, SERVER4)] {
        net.in_pod(|| send_udp(pod, server, 9999, b"dropped"));
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
                tuple.source_address.as_str(),
                tuple.destination_address.as_str(),
                tuple.destination_port
            ),
            (17, pod, server, 9999)
        );
    }
    collector.stop();
}

/// A deleted pod's connections are forgotten, IPv4 and IPv6 apart: each
/// ends with its record, and the kernel knows them no more.
#[test]
#[ignore]
fn connections_of_an_address_are_deleted() {
    let net = Net::new("delete");
    let (collector, mut rx) = start(&net, &[vec![nft::Expr::CtState]]);
    // A connection that stays open, and flows that were not answered.
    let listener = net.serve(SERVER4, 8080, 1, 0);
    let (open, closed) = std::sync::mpsc::channel::<()>();
    let pod = net.pod.clone();
    let client = thread::spawn(move || {
        enter(&pod);
        let mut stream = TcpStream::connect(SocketAddr::new(address(SERVER4), 8080)).unwrap();
        stream.write_all(b"held").unwrap();
        let _ = closed.recv();
    });
    net.in_pod(|| {
        send_udp(POD4, SERVER4, 5353, b"x");
        send_udp(POD4, SERVER4, 5354, b"x");
        send_udp(POD, SERVER, 5353, b"x");
    });
    collect(&mut rx, 4, flow(false, |_| true));

    let (filtered, all) = net.in_node(|| {
        let mut socket = Socket::open(0).unwrap();
        socket.set_timeout(Duration::from_secs(5)).unwrap();
        let filtered = conntrack::entries(&mut socket, address(POD4), true).unwrap();
        let all = conntrack::entries(&mut socket, address(POD4), false).unwrap();
        (filtered, all)
    });
    assert_eq!(filtered.len(), 3);
    assert_eq!(filtered, all);

    let deleted = net.in_node(|| delete_connections(address(POD4)).unwrap());
    assert_eq!(deleted, 3);
    let ended = collect(&mut rx, 3, flow(true, |_| true));
    let mut ends: Vec<_> = ended
        .iter()
        .map(|flow| {
            let tuple = flow.tuple.as_ref().unwrap();
            (
                tuple.source_address.as_str(),
                tuple.protocol,
                tuple.destination_port,
            )
        })
        .collect();
    ends.sort();
    assert_eq!(ends, [(POD4, 6, 8080), (POD4, 17, 5353), (POD4, 17, 5354)]);

    // Gone; the pod's IPv6 flow is not.
    assert_eq!(
        net.in_node(|| delete_connections(address(POD4)).unwrap()),
        0
    );
    assert_eq!(net.in_node(|| delete_connections(address(POD)).unwrap()), 1);
    collector.stop();
    drop(open);
    client.join().unwrap();
    // The server's end of the connection may never see the client's: it
    // is left behind.
    drop(listener);
}

/// A socket the kernel cannot hand its events over to drops them, and
/// counts them where `drops` reads.
#[test]
#[ignore]
fn lost_events_are_counted() {
    let net = Net::new("lost");
    net.in_node(|| {
        nft::apply(&[vec![nft::Expr::CtState]]);
        let socket = conntrack::open().unwrap();
        socket.set_receive_buffer(4096);
        socket.set_timeout(Duration::from_millis(100)).unwrap();
        let before = socket.drops().unwrap();
        let udp = UdpSocket::bind("[fd00:fe05:2::1]:0").unwrap();
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
    let server = net.serve(SERVER, 8080, 1, 100);
    let server4 = net.serve(SERVER4, 8080, 1, 100);
    let tls = net.serve(SERVER, 8443, 1, 0);
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
    let (flows, drops, copies) = net.in_node(move || {
        nft::apply(&[vec![nft::Expr::CtState], copy, deny]);
        enable_accounting().unwrap();
        let flows = conntrack::open().unwrap();
        let drops = nflog::open(2, 256).unwrap();
        let copies = nflog::open(3, u16::MAX).unwrap();
        (flows, drops, copies)
    });
    net.in_pod(|| {
        talk(SERVER, 8080, &[&[b'a'; 1000]]);
        talk(SERVER4, 8080, &[&[b'a'; 1000]]);
        talk(SERVER, 8443, &[&flight[..700], &flight[700..]]);
        send_udp(POD, SERVER, 5353, b"x");
        send_udp(POD, SERVER, 9999, b"dropped");
        send_udp(POD4, SERVER4, 9999, b"dropped");
    });
    net.ping(SERVER);
    net.ping(SERVER4);
    net.in_node(flush_conntrack);
    server.join().unwrap();
    server4.join().unwrap();
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
    let find = |name: &str, ended: bool, protocol: u8, port: u16, v4: bool| {
        let event = events.iter().find(|bytes| {
            let message = netlink::messages(bytes).next().unwrap();
            conntrack::parse(&message).is_some_and(|e| {
                e.ended == ended
                    && e.original.protocol == protocol
                    && e.original.destination_port == port
                    && e.original.source.is_ipv4() == v4
            })
        });
        print_vector(name, event.expect(name));
    };
    find("CT_NEW_TCP", false, 6, 8080, false);
    find("CT_DESTROY_TCP", true, 6, 8080, false);
    find("CT_NEW_TCP4", false, 6, 8080, true);
    find("CT_NEW_UDP", false, 17, 5353, false);
    find("CT_NEW_ICMPV6", false, 58, 128, false);
    find("CT_NEW_ICMP", false, 1, 8, true);
    let batches = read(&drops);
    let drops: Vec<&[u8]> = batches.iter().flat_map(|batch| split(batch)).collect();
    let drop = |name: &str, version: u8| {
        let drop = drops.iter().find(|bytes| {
            let message = netlink::messages(bytes).next().unwrap();
            nflog::parse(&message)
                .is_some_and(|l| l.payload.first().map(|b| b >> 4) == Some(version))
        });
        print_vector(name, drop.expect(name));
    };
    drop("NFLOG_DROP_UDP", 6);
    drop("NFLOG_DROP_UDP4", 4);
    // nflog sends packets in batches: all of them, in one or more.
    print_vector("NFLOG_COPIES", &read(&copies).concat());
}

/// The messages of a batch, a message each, with its header.
fn split(batch: &[u8]) -> Vec<&[u8]> {
    netlink::messages(batch)
        .map(|message| {
            let end =
                message.payload.as_ptr() as usize - batch.as_ptr() as usize + message.payload.len();
            let start = end - message.payload.len() - 16;
            &batch[start..end]
        })
        .collect()
}

fn print_vector(name: &str, bytes: &[u8]) {
    println!("pub const {name}: &[u8] = &{};", hex(bytes));
}

fn hex(bytes: &[u8]) -> String {
    let items: Vec<String> = bytes.iter().map(|b| format!("0x{b:02x}")).collect();
    format!("[{}]", items.join(", "))
}
