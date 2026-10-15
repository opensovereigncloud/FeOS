// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! NAT64 for pods (ADR 0010): a stateless translator between IPv6 and IPv4
//! (SIIT, RFC 7915) with an explicit IPv4 address per pod (RFC 7757).
//!
//! It runs on the egress of the node's `feos-nat64` device, into which the
//! node routes the NAT64 prefix and the pods' IPv4 pool. It translates what
//! arrives there and hands it back to the node's stack on the device's
//! ingress, as if it came in from a link:
//!
//! - IPv6 to IPv4: the pod's address becomes its pool address, the
//!   destination `prefix + a.b.c.d` becomes `a.b.c.d`.
//! - IPv4 to IPv6: the pool address becomes the pod's address, the source
//!   `a.b.c.d` becomes `prefix + a.b.c.d`.
//!
//! It translates TCP, UDP and ICMP: echo, and the errors with the packet in
//! them. It drops what it does not translate: fragments, IPv4 options, IPv6
//! extension headers and other protocols, and what no pod's mapping covers.

#![no_std]
#![no_main]

use core::{mem::size_of, ptr};

use aya_ebpf::{
    bindings::{
        __sk_buff, BPF_F_INGRESS, BPF_F_MARK_MANGLED_0, BPF_F_NO_PREALLOC, BPF_F_PSEUDO_HDR,
        TC_ACT_SHOT,
    },
    helpers::{bpf_csum_diff, bpf_get_prandom_u32, bpf_redirect, bpf_skb_load_bytes},
    macros::{classifier, map},
    maps::{HashMap, PerCpuArray},
    programs::TcContext,
};

const ETH_LEN: usize = 14;
const IPV4_LEN: usize = 20;
const IPV6_LEN: usize = 40;
const ICMP_LEN: usize = 8;

const ETH_P_IP: u16 = 0x0800;
const ETH_P_IPV6: u16 = 0x86dd;

const PROTO_ICMP: u8 = 1;
const PROTO_TCP: u8 = 6;
const PROTO_UDP: u8 = 17;
const PROTO_ICMPV6: u8 = 58;

/// `bpf_skb_adjust_room`'s mode: room right behind the IP header.
const ADJ_ROOM_NET: u32 = 0;

/// Room for every pod of a /16 pool.
const MAX_PODS: u32 = 65536;

/// The first 96 bits of the NAT64 prefix. The loader sets these globals.
#[unsafe(no_mangle)]
static PREFIX: [u8; 12] = [0; 12];
/// The IPv4 address that stands for the translator itself: the source of
/// ICMP errors from the node or routers on the IPv6 side (RFC 6791).
#[unsafe(no_mangle)]
static NODE: [u8; 4] = [0; 4];
/// The pods' IPv4 pool, which is never a destination from a pod.
#[unsafe(no_mangle)]
static POOL: [u8; 4] = [0; 4];
#[unsafe(no_mangle)]
static POOL_MASK: [u8; 4] = [0; 4];

/// A pod's IPv6 address to its IPv4 address.
#[map(name = "nat64_pod_ipv4")]
static POD_IPV4: HashMap<[u8; 16], [u8; 4]> = HashMap::pinned(MAX_PODS, BPF_F_NO_PREALLOC);

/// A pod's IPv4 address to its IPv6 address.
#[map(name = "nat64_ipv4_pod")]
static IPV4_POD: HashMap<[u8; 4], [u8; 16]> = HashMap::pinned(MAX_PODS, BPF_F_NO_PREALLOC);

/// Counters per CPU, indexed by [`Stat`].
#[map(name = "nat64_stats")]
static STATS: PerCpuArray<u64> = PerCpuArray::pinned(STAT_COUNT, 0);

/// The headers of the packet at hand, per CPU.
#[map(name = "nat64_frame")]
static FRAMES: PerCpuArray<Frame> = PerCpuArray::with_max_entries(1, 0);

/// What the counters count. crates/nat64 reads them in this order.
#[derive(Clone, Copy)]
#[repr(u32)]
enum Stat {
    /// Packets translated from IPv6 to IPv4.
    SixToFour = 0,
    /// Packets translated from IPv4 to IPv6.
    FourToSix = 1,
    /// Dropped: no pod has the address.
    NoMapping = 2,
    /// Dropped: a destination outside the prefix, or one that no pod
    /// reaches through NAT64 (the pool, loopback, link-local, multicast).
    Denied = 3,
    /// Dropped: fragments, options, extension headers, other protocols and
    /// ICMP types that do not translate.
    Unsupported = 4,
    /// Dropped: a truncated or malformed packet, or a helper that failed.
    Error = 5,
    /// IPv4 UDP datagrams without a checksum that got one.
    UdpChecksum = 6,
}

const STAT_COUNT: u32 = 8;

/// The headers of a packet, old and new, in a per-CPU map rather than on
/// the stack: a BPF program has 512 bytes of stack, and the compiler
/// spreads a header on it byte by byte.
#[repr(C, align(8))]
struct Frame {
    /// For the checksum of a UDP datagram that has none.
    buf: [u8; 1024],
    ip6: Ipv6,
    inner6: Ipv6,
    ip4: Ipv4,
    inner4: Ipv4,
    icmp: Icmp,
    new_icmp: Icmp,
    eth: Eth,
}

#[repr(C)]
struct Eth {
    dst: [u8; 6],
    src: [u8; 6],
    proto: [u8; 2],
}

#[repr(C)]
struct Ipv4 {
    ver_ihl: u8,
    tos: u8,
    len: [u8; 2],
    id: [u8; 2],
    frag: [u8; 2],
    ttl: u8,
    proto: u8,
    check: [u8; 2],
    src: [u8; 4],
    dst: [u8; 4],
}

#[repr(C)]
struct Ipv6 {
    vtc_flow: [u8; 4],
    len: [u8; 2],
    next: u8,
    hops: u8,
    src: [u8; 16],
    dst: [u8; 16],
}

#[repr(C)]
struct Icmp {
    kind: u8,
    code: u8,
    check: [u8; 2],
    rest: [u8; 4],
}

/// Four bytes of the transport header of the packet in an ICMP error, with
/// its checksum in them, before and after the translation. All fields are
/// set even without a fix: the verifier rejects a program that stores
/// bytes it never wrote, as an `Option` would.
#[derive(Clone, Copy)]
struct Fix {
    present: bool,
    offset: usize,
    old: [u8; 4],
    new: [u8; 4],
}

impl Fix {
    const NONE: Self = Self {
        present: false,
        offset: 0,
        old: [0; 4],
        new: [0; 4],
    };

    #[inline(always)]
    fn new(offset: usize, old: [u8; 4], new: [u8; 4]) -> Self {
        Self {
            present: true,
            offset,
            old,
            new,
        }
    }
}

/// A one's complement sum of 16-bit big-endian words (RFC 1071), kept wide
/// and folded at the end.
#[derive(Clone, Copy)]
struct Sum(u64);

impl Sum {
    #[inline(always)]
    fn add(self, bytes: &[u8]) -> Self {
        let mut sum = self.0;
        for word in bytes.chunks_exact(2) {
            sum += u64::from(u16::from_be_bytes([word[0], word[1]]));
        }
        Self(sum)
    }

    #[inline(always)]
    fn sub(self, bytes: &[u8]) -> Self {
        let mut sum = self.0;
        for word in bytes.chunks_exact(2) {
            sum += u64::from(!u16::from_be_bytes([word[0], word[1]]));
        }
        Self(sum)
    }

    #[inline(always)]
    fn word(self, word: u16) -> Self {
        Self(self.0 + u64::from(word))
    }

    #[inline(always)]
    fn sub_word(self, word: u16) -> Self {
        Self(self.0 + u64::from(!word))
    }

    #[inline(always)]
    fn fold(self) -> u16 {
        let mut sum = (self.0 & 0xffff_ffff) + (self.0 >> 32);
        sum = (sum & 0xffff) + (sum >> 16);
        sum = (sum & 0xffff) + (sum >> 16);
        sum = (sum & 0xffff) + (sum >> 16);
        sum as u16
    }

    /// The IPv6 pseudo-header of an upper-layer checksum.
    #[inline(always)]
    fn pseudo6(self, src: &[u8; 16], dst: &[u8; 16], len: u16, next: u8) -> Self {
        self.add(src).add(dst).word(len).word(u16::from(next))
    }

    #[inline(always)]
    fn sub_pseudo6(self, src: &[u8; 16], dst: &[u8; 16], len: u16, next: u8) -> Self {
        self.sub(src)
            .sub(dst)
            .sub_word(len)
            .sub_word(u16::from(next))
    }

    #[inline(always)]
    fn ipv4(self, h: &Ipv4) -> Self {
        self.word(word(h.ver_ihl, h.tos))
            .add(&h.len)
            .add(&h.id)
            .add(&h.frag)
            .word(word(h.ttl, h.proto))
            .add(&h.check)
            .add(&h.src)
            .add(&h.dst)
    }

    #[inline(always)]
    fn sub_ipv4(self, h: &Ipv4) -> Self {
        self.sub_word(word(h.ver_ihl, h.tos))
            .sub(&h.len)
            .sub(&h.id)
            .sub(&h.frag)
            .sub_word(word(h.ttl, h.proto))
            .sub(&h.check)
            .sub(&h.src)
            .sub(&h.dst)
    }

    #[inline(always)]
    fn ipv6(self, h: &Ipv6) -> Self {
        self.add(&h.vtc_flow)
            .add(&h.len)
            .word(word(h.next, h.hops))
            .add(&h.src)
            .add(&h.dst)
    }

    #[inline(always)]
    fn sub_ipv6(self, h: &Ipv6) -> Self {
        self.sub(&h.vtc_flow)
            .sub(&h.len)
            .sub_word(word(h.next, h.hops))
            .sub(&h.src)
            .sub(&h.dst)
    }

    /// The ICMP header without its checksum.
    #[inline(always)]
    fn icmp(self, h: &Icmp) -> Self {
        self.word(word(h.kind, h.code)).add(&h.rest)
    }

    #[inline(always)]
    fn sub_icmp(self, h: &Icmp) -> Self {
        self.sub_word(word(h.kind, h.code)).sub(&h.rest)
    }
}

#[inline(always)]
fn word(high: u8, low: u8) -> u16 {
    u16::from_be_bytes([high, low])
}

/// The checksum `check` after the changes that `diff` sums up (RFC 1624).
#[inline(always)]
fn replace(check: [u8; 2], diff: Sum) -> [u8; 2] {
    let sum = Sum(u64::from(!u16::from_be_bytes(check)) + diff.0);
    (!sum.fold()).to_be_bytes()
}

#[classifier]
pub fn nat64(ctx: TcContext) -> i32 {
    let skb = ctx.skb.skb;
    match classify(&ctx) {
        Ok(Path::SixToFour) => six_to_four(skb),
        Ok(Path::SixToFourEcho) => six_to_four_echo(skb),
        Ok(Path::SixToFourError) => six_to_four_error(skb),
        Ok(Path::FourToSix) => four_to_six(skb),
        Ok(Path::FourToSixEcho) => four_to_six_echo(skb),
        Ok(Path::FourToSixError) => four_to_six_error(skb),
        Err(stat) => shot(stat),
    }
}

/// The translations, each in a function of its own.
enum Path {
    /// TCP and UDP from a pod.
    SixToFour,
    /// ICMPv6 echo from a pod.
    SixToFourEcho,
    /// ICMPv6 errors about packets to a pod.
    SixToFourError,
    /// TCP and UDP to a pod.
    FourToSix,
    /// ICMPv4 echo to a pod.
    FourToSixEcho,
    /// ICMPv4 errors about packets from a pod.
    FourToSixError,
}

#[inline(always)]
fn classify(ctx: &TcContext) -> Result<Path, Stat> {
    let proto: [u8; 2] = load(ctx, 12)?;
    match u16::from_be_bytes(proto) {
        ETH_P_IPV6 => match load::<u8>(ctx, ETH_LEN + 6)? {
            PROTO_TCP | PROTO_UDP => Ok(Path::SixToFour),
            PROTO_ICMPV6 => match load::<u8>(ctx, ETH_LEN + IPV6_LEN)? {
                128 | 129 => Ok(Path::SixToFourEcho),
                1..=4 => Ok(Path::SixToFourError),
                _ => Err(Stat::Unsupported),
            },
            _ => Err(Stat::Unsupported),
        },
        ETH_P_IP => {
            let ver_ihl = load::<u8>(ctx, ETH_LEN)?;
            if ver_ihl >> 4 != 4 {
                return Err(Stat::Error);
            }
            // Options do not translate.
            if ver_ihl != 0x45 {
                return Err(Stat::Unsupported);
            }
            match load::<u8>(ctx, ETH_LEN + 9)? {
                PROTO_TCP | PROTO_UDP => Ok(Path::FourToSix),
                PROTO_ICMP => match load::<u8>(ctx, ETH_LEN + IPV4_LEN)? {
                    0 | 8 => Ok(Path::FourToSixEcho),
                    3 | 11 | 12 => Ok(Path::FourToSixError),
                    _ => Err(Stat::Unsupported),
                },
                _ => Err(Stat::Unsupported),
            }
        }
        _ => Err(Stat::Unsupported),
    }
}

#[inline(always)]
fn verdict(result: Result<i32, Stat>) -> i32 {
    match result {
        Ok(action) => action,
        Err(stat) => shot(stat),
    }
}

#[inline(always)]
fn shot(stat: Stat) -> i32 {
    count(stat);
    TC_ACT_SHOT as i32
}

#[inline(always)]
fn frame() -> Result<&'static mut Frame, Stat> {
    let frame = FRAMES.get_ptr_mut(0).ok_or(Stat::Error)?;
    // SAFETY: a per-CPU value, and one program at a time runs on a CPU.
    Ok(unsafe { &mut *frame })
}

/// Reads the Ethernet and IPv6 headers of a packet from a pod. Returns the
/// pod's IPv4 address, if a pod sent it, and the IPv4 destination.
#[inline(always)]
fn ipv6_packet(ctx: &TcContext, f: &mut Frame) -> Result<(Option<[u8; 4]>, [u8; 4]), Stat> {
    read(ctx, 0, &mut f.eth)?;
    read(ctx, ETH_LEN, &mut f.ip6)?;
    if f.ip6.vtc_flow[0] >> 4 != 6 {
        return Err(Stat::Error);
    }
    let dst = extract(&f.ip6.dst).ok_or(Stat::Denied)?;
    if denied(dst) {
        return Err(Stat::Denied);
    }
    let len = u16::from_be_bytes(f.ip6.len);
    // Jumbograms (length 0) and what would not fit an IPv4 header.
    if len == 0 || len > 0xffff - IPV4_LEN as u16 {
        return Err(Stat::Unsupported);
    }
    Ok((pod_ipv4(&f.ip6.src), dst))
}

/// Reads the Ethernet and IPv4 headers of a packet to a pod, and puts the
/// IPv6 addresses into the new header. Returns the payload's length.
#[inline(always)]
fn ipv4_packet(ctx: &TcContext, f: &mut Frame) -> Result<u16, Stat> {
    read(ctx, 0, &mut f.eth)?;
    read(ctx, ETH_LEN, &mut f.ip4)?;
    if fragment(&f.ip4) {
        return Err(Stat::Unsupported);
    }
    let total = u16::from_be_bytes(f.ip4.len);
    if total < IPV4_LEN as u16 {
        return Err(Stat::Error);
    }
    ipv4_pod(&f.ip4.dst, &mut f.ip6.dst)?;
    embed(&mut f.ip6.src, &f.ip4.src);
    Ok(total - IPV4_LEN as u16)
}

#[inline(never)]
fn six_to_four(skb: *mut __sk_buff) -> i32 {
    verdict(try_six_to_four(&TcContext::new(skb)))
}

#[inline(always)]
fn try_six_to_four(ctx: &TcContext) -> Result<i32, Stat> {
    let f = frame()?;
    let (src, dst) = ipv6_packet(ctx, f)?;
    let src = src.ok_or(Stat::NoMapping)?;
    let total = u16::from_be_bytes(f.ip6.len) + IPV4_LEN as u16;
    let (tos, hops, next) = (tos(&f.ip6), f.ip6.hops, f.ip6.next);
    set_ipv4(&mut f.ip4, tos, total, hops, next, src, dst, random_id());
    let diff = Sum(0)
        .sub(&f.ip6.src)
        .sub(&f.ip6.dst)
        .add(&f.ip4.src)
        .add(&f.ip4.dst);
    change_proto(ctx, ETH_P_IP)?;
    store(ctx, ETH_LEN, &f.ip4)?;
    l4_checksum(ctx, ETH_LEN + IPV4_LEN, next, diff)?;
    done(ctx, f, ETH_P_IP, Stat::SixToFour)
}

#[inline(never)]
fn four_to_six(skb: *mut __sk_buff) -> i32 {
    verdict(try_four_to_six(&TcContext::new(skb)))
}

#[inline(always)]
fn try_four_to_six(ctx: &TcContext) -> Result<i32, Stat> {
    let f = frame()?;
    let len = ipv4_packet(ctx, f)?;
    let (tos, ttl, proto) = (f.ip4.tos, f.ip4.ttl, f.ip4.proto);
    set_ipv6(&mut f.ip6, tos, len, ttl, proto);
    let l4 = ETH_LEN + IPV4_LEN;
    // IPv6 requires the checksum that IPv4 UDP may leave out.
    let missing = proto == PROTO_UDP && load::<[u8; 2]>(ctx, l4 + 6)? == [0, 0];
    let check = if missing {
        Some(udp_checksum(ctx, f, l4, len)?)
    } else {
        None
    };
    change_proto(ctx, ETH_P_IPV6)?;
    store(ctx, ETH_LEN, &f.ip6)?;
    match check {
        Some(check) => store(ctx, ETH_LEN + IPV6_LEN + 6, &check)?,
        None => {
            let diff = Sum(0)
                .sub(&f.ip4.src)
                .sub(&f.ip4.dst)
                .add(&f.ip6.src)
                .add(&f.ip6.dst);
            l4_checksum(ctx, ETH_LEN + IPV6_LEN, proto, diff)?;
        }
    }
    done(ctx, f, ETH_P_IPV6, Stat::FourToSix)
}

#[inline(never)]
fn six_to_four_echo(skb: *mut __sk_buff) -> i32 {
    verdict(try_six_to_four_echo(&TcContext::new(skb)))
}

/// ICMPv6 echo to ICMPv4 echo (RFC 7915 5.2).
#[inline(always)]
fn try_six_to_four_echo(ctx: &TcContext) -> Result<i32, Stat> {
    let f = frame()?;
    let (src, dst) = ipv6_packet(ctx, f)?;
    let src = src.ok_or(Stat::NoMapping)?;
    let len = u16::from_be_bytes(f.ip6.len);
    read(ctx, ETH_LEN + IPV6_LEN, &mut f.icmp)?;
    f.new_icmp.kind = if f.icmp.kind == 128 { 8 } else { 0 };
    f.new_icmp.code = f.icmp.code;
    f.new_icmp.rest = f.icmp.rest;
    let diff = Sum(0)
        .sub_pseudo6(&f.ip6.src, &f.ip6.dst, len, PROTO_ICMPV6)
        .sub_icmp(&f.icmp)
        .icmp(&f.new_icmp);
    f.new_icmp.check = replace(f.icmp.check, diff);
    let (tos, hops) = (tos(&f.ip6), f.ip6.hops);
    let total = len + IPV4_LEN as u16;
    set_ipv4(
        &mut f.ip4,
        tos,
        total,
        hops,
        PROTO_ICMP,
        src,
        dst,
        random_id(),
    );
    change_proto(ctx, ETH_P_IP)?;
    store(ctx, ETH_LEN, &f.ip4)?;
    store(ctx, ETH_LEN + IPV4_LEN, &f.new_icmp)?;
    done(ctx, f, ETH_P_IP, Stat::SixToFour)
}

#[inline(never)]
fn six_to_four_error(skb: *mut __sk_buff) -> i32 {
    verdict(try_six_to_four_error(&TcContext::new(skb)))
}

/// An ICMPv6 error about a packet from an IPv4 host to a pod, as ICMPv4
/// (RFC 7915 5.2), with the packet in it translated too.
#[inline(always)]
fn try_six_to_four_error(ctx: &TcContext) -> Result<i32, Stat> {
    let f = frame()?;
    let (src, dst) = ipv6_packet(ctx, f)?;
    // From the node or a router on the IPv6 side: from the translator.
    let src = match src {
        Some(src) => src,
        None => [
            global(&NODE, 0),
            global(&NODE, 1),
            global(&NODE, 2),
            global(&NODE, 3),
        ],
    };
    let len = u16::from_be_bytes(f.ip6.len);
    if (len as usize) < ICMP_LEN + IPV6_LEN {
        return Err(Stat::Error);
    }
    read(ctx, ETH_LEN + IPV6_LEN, &mut f.icmp)?;
    let (kind, code, rest) = icmp6_error_type(&f.icmp).ok_or(Stat::Unsupported)?;
    read(ctx, ETH_LEN + IPV6_LEN + ICMP_LEN, &mut f.inner6)?;
    if f.inner6.vtc_flow[0] >> 4 != 6 {
        return Err(Stat::Error);
    }
    let inner_src = extract(&f.inner6.src).ok_or(Stat::Unsupported)?;
    let inner_dst = pod_ipv4(&f.inner6.dst).ok_or(Stat::NoMapping)?;
    let inner_len = u16::from_be_bytes(f.inner6.len);
    if inner_len > 0xffff - IPV4_LEN as u16 {
        return Err(Stat::Unsupported);
    }
    let inner_proto = match f.inner6.next {
        PROTO_TCP | PROTO_UDP => f.inner6.next,
        PROTO_ICMPV6 => PROTO_ICMP,
        _ => return Err(Stat::Unsupported),
    };
    // The original's identification did not make it into IPv6.
    let (inner_tos, inner_hops) = (tos(&f.inner6), f.inner6.hops);
    set_ipv4(
        &mut f.inner4,
        inner_tos,
        inner_len + IPV4_LEN as u16,
        inner_hops,
        inner_proto,
        inner_src,
        inner_dst,
        0,
    );
    let fix = inner_six_to_four(ctx, ETH_LEN + IPV6_LEN + ICMP_LEN + IPV6_LEN, f)?;

    f.new_icmp.kind = kind;
    f.new_icmp.code = code;
    f.new_icmp.rest = rest;
    let mut diff = Sum(0)
        .sub_pseudo6(&f.ip6.src, &f.ip6.dst, len, PROTO_ICMPV6)
        .sub_icmp(&f.icmp)
        .icmp(&f.new_icmp)
        .sub_ipv6(&f.inner6)
        .ipv4(&f.inner4);
    if fix.present {
        diff = diff.sub(&fix.old).add(&fix.new);
    }
    f.new_icmp.check = replace(f.icmp.check, diff);
    // The packet shrinks by 20 bytes twice: its own header and the inner one.
    let (tos, hops) = (tos(&f.ip6), f.ip6.hops);
    set_ipv4(
        &mut f.ip4,
        tos,
        len,
        hops,
        PROTO_ICMP,
        src,
        dst,
        random_id(),
    );
    change_proto(ctx, ETH_P_IP)?;
    ctx.adjust_room(-(IPV6_LEN as i32 - IPV4_LEN as i32), ADJ_ROOM_NET, 0)
        .map_err(|_| Stat::Error)?;
    store(ctx, ETH_LEN, &f.ip4)?;
    store(ctx, ETH_LEN + IPV4_LEN, &f.new_icmp)?;
    store(ctx, ETH_LEN + IPV4_LEN + ICMP_LEN, &f.inner4)?;
    if fix.present {
        let offset = ETH_LEN + IPV4_LEN + ICMP_LEN + IPV4_LEN + fix.offset;
        store(ctx, offset, &fix.new)?;
    }
    done(ctx, f, ETH_P_IP, Stat::SixToFour)
}

#[inline(never)]
fn four_to_six_echo(skb: *mut __sk_buff) -> i32 {
    verdict(try_four_to_six_echo(&TcContext::new(skb)))
}

/// ICMPv4 echo to ICMPv6 echo (RFC 7915 4.2).
#[inline(always)]
fn try_four_to_six_echo(ctx: &TcContext) -> Result<i32, Stat> {
    let f = frame()?;
    let len = ipv4_packet(ctx, f)?;
    read(ctx, ETH_LEN + IPV4_LEN, &mut f.icmp)?;
    f.new_icmp.kind = if f.icmp.kind == 8 { 128 } else { 129 };
    f.new_icmp.code = f.icmp.code;
    f.new_icmp.rest = f.icmp.rest;
    let diff = Sum(0)
        .pseudo6(&f.ip6.src, &f.ip6.dst, len, PROTO_ICMPV6)
        .sub_icmp(&f.icmp)
        .icmp(&f.new_icmp);
    f.new_icmp.check = replace(f.icmp.check, diff);
    let (tos, ttl) = (f.ip4.tos, f.ip4.ttl);
    set_ipv6(&mut f.ip6, tos, len, ttl, PROTO_ICMPV6);
    change_proto(ctx, ETH_P_IPV6)?;
    store(ctx, ETH_LEN, &f.ip6)?;
    store(ctx, ETH_LEN + IPV6_LEN, &f.new_icmp)?;
    done(ctx, f, ETH_P_IPV6, Stat::FourToSix)
}

#[inline(never)]
fn four_to_six_error(skb: *mut __sk_buff) -> i32 {
    verdict(try_four_to_six_error(&TcContext::new(skb)))
}

/// An ICMPv4 error about a packet from a pod to an IPv4 host, as ICMPv6
/// (RFC 7915 4.2), with the packet in it translated too.
#[inline(always)]
fn try_four_to_six_error(ctx: &TcContext) -> Result<i32, Stat> {
    let f = frame()?;
    let len = ipv4_packet(ctx, f)?;
    if (len as usize) < ICMP_LEN + IPV4_LEN {
        return Err(Stat::Error);
    }
    read(ctx, ETH_LEN + IPV4_LEN, &mut f.icmp)?;
    read(ctx, ETH_LEN + IPV4_LEN + ICMP_LEN, &mut f.inner4)?;
    if f.inner4.ver_ihl >> 4 != 4 {
        return Err(Stat::Error);
    }
    if f.inner4.ver_ihl != 0x45 || fragment(&f.inner4) {
        return Err(Stat::Unsupported);
    }
    let inner_total = u16::from_be_bytes(f.inner4.len);
    if inner_total < IPV4_LEN as u16 {
        return Err(Stat::Error);
    }
    let (kind, code, rest) = icmp4_error_type(&f.icmp, inner_total).ok_or(Stat::Unsupported)?;
    ipv4_pod(&f.inner4.src, &mut f.inner6.src)?;
    embed(&mut f.inner6.dst, &f.inner4.dst);
    let inner_next = match f.inner4.proto {
        PROTO_TCP | PROTO_UDP => f.inner4.proto,
        PROTO_ICMP => PROTO_ICMPV6,
        _ => return Err(Stat::Unsupported),
    };
    let (inner_tos, inner_ttl) = (f.inner4.tos, f.inner4.ttl);
    let inner_len = inner_total - IPV4_LEN as u16;
    set_ipv6(&mut f.inner6, inner_tos, inner_len, inner_ttl, inner_next);
    let fix = inner_four_to_six(ctx, ETH_LEN + IPV4_LEN + ICMP_LEN + IPV4_LEN, f)?;

    // The packet grows by 20 bytes twice: its own header and the inner one.
    let len6 = len + IPV4_LEN as u16;
    f.new_icmp.kind = kind;
    f.new_icmp.code = code;
    f.new_icmp.rest = rest;
    let mut diff = Sum(0)
        .pseudo6(&f.ip6.src, &f.ip6.dst, len6, PROTO_ICMPV6)
        .sub_icmp(&f.icmp)
        .icmp(&f.new_icmp)
        .sub_ipv4(&f.inner4)
        .ipv6(&f.inner6);
    if fix.present {
        diff = diff.sub(&fix.old).add(&fix.new);
    }
    f.new_icmp.check = replace(f.icmp.check, diff);
    let (tos, ttl) = (f.ip4.tos, f.ip4.ttl);
    set_ipv6(&mut f.ip6, tos, len6, ttl, PROTO_ICMPV6);
    change_proto(ctx, ETH_P_IPV6)?;
    ctx.adjust_room(IPV6_LEN as i32 - IPV4_LEN as i32, ADJ_ROOM_NET, 0)
        .map_err(|_| Stat::Error)?;
    store(ctx, ETH_LEN, &f.ip6)?;
    store(ctx, ETH_LEN + IPV6_LEN, &f.new_icmp)?;
    store(ctx, ETH_LEN + IPV6_LEN + ICMP_LEN, &f.inner6)?;
    if fix.present {
        let offset = ETH_LEN + IPV6_LEN + ICMP_LEN + IPV6_LEN + fix.offset;
        store(ctx, offset, &fix.new)?;
    }
    done(ctx, f, ETH_P_IPV6, Stat::FourToSix)
}

/// ICMPv6 errors as ICMPv4 errors (RFC 7915 5.2).
#[inline(always)]
fn icmp6_error_type(icmp: &Icmp) -> Option<(u8, u8, [u8; 4])> {
    let none = [0; 4];
    match (icmp.kind, icmp.code) {
        // Destination unreachable.
        (1, 0 | 2 | 3) => Some((3, 1, none)),
        (1, 1) => Some((3, 10, none)),
        (1, 4) => Some((3, 3, none)),
        // Packet too big: fragmentation needed, for 20 bytes less.
        (2, _) => {
            let mtu = u32::from_be_bytes(icmp.rest).saturating_sub(20).min(0xffff) as u16;
            let [high, low] = mtu.to_be_bytes();
            Some((3, 4, [0, 0, high, low]))
        }
        // Time exceeded.
        (3, code) => Some((11, code, none)),
        // Parameter problem, pointing at the IPv4 header's field.
        (4, 0) => {
            let pointer = match u32::from_be_bytes(icmp.rest) {
                0 => 0,
                1 => 1,
                4 | 5 => 2,
                6 => 9,
                7 => 8,
                8..=23 => 12,
                24..=39 => 16,
                _ => return None,
            };
            Some((12, 0, [pointer, 0, 0, 0]))
        }
        (4, 1) => Some((3, 2, none)),
        _ => None,
    }
}

/// ICMPv4 errors as ICMPv6 errors (RFC 7915 4.2).
#[inline(always)]
fn icmp4_error_type(icmp: &Icmp, inner_total: u16) -> Option<(u8, u8, [u8; 4])> {
    let none = [0; 4];
    match (icmp.kind, icmp.code) {
        // Destination unreachable.
        (3, 0 | 1 | 5 | 6 | 7 | 8 | 11 | 12) => Some((1, 0, none)),
        // Protocol unreachable: a problem with the next header.
        (3, 2) => Some((4, 1, [0, 0, 0, 6])),
        (3, 3) => Some((1, 4, none)),
        // Fragmentation needed: packet too big, for 20 bytes more.
        (3, 4) => {
            let mtu = u32::from(u16::from_be_bytes([icmp.rest[2], icmp.rest[3]]));
            let mtu = if mtu == 0 { plateau(inner_total) } else { mtu };
            Some((2, 0, (mtu + 20).to_be_bytes()))
        }
        (3, 9 | 10 | 13 | 15) => Some((1, 1, none)),
        // Time exceeded.
        (11, code) => Some((3, code, none)),
        // Parameter problem, pointing at the IPv6 header's field.
        (12, 0 | 2) => {
            let pointer = match icmp.rest[0] {
                0 => 0,
                1 => 1,
                2 | 3 => 4,
                8 => 7,
                9 => 6,
                12..=15 => 8,
                16..=19 => 24,
                _ => return None,
            };
            Some((4, 0, [0, 0, 0, pointer]))
        }
        _ => None,
    }
}

/// The largest MTU plateau below a packet's length (RFC 1191), for a router
/// that did not say its MTU.
#[inline(always)]
fn plateau(len: u16) -> u32 {
    match len {
        32001.. => 32000,
        17915.. => 17914,
        8167.. => 8166,
        4353.. => 4352,
        2003.. => 2002,
        1493.. => 1492,
        1007.. => 1006,
        509.. => 508,
        297.. => 296,
        _ => 68,
    }
}

/// The transport header of the packet in an ICMPv6 error, as IPv4.
#[inline(always)]
fn inner_six_to_four(ctx: &TcContext, l4: usize, f: &Frame) -> Result<Fix, Stat> {
    match f.inner6.next {
        PROTO_TCP | PROTO_UDP => {
            let diff = Sum(0)
                .sub(&f.inner6.src)
                .sub(&f.inner6.dst)
                .add(&f.inner4.src)
                .add(&f.inner4.dst);
            Ok(checksum_fix(ctx, l4, f.inner6.next == PROTO_UDP, diff))
        }
        _ => {
            // The error carries as much of the packet as it can; an ICMP
            // header that is not in it needs nothing.
            let mut old = [0; 4];
            if read(ctx, l4, &mut old).is_err() {
                return Ok(Fix::NONE);
            }
            let kind = match old[0] {
                128 => 8,
                129 => 0,
                _ => return Err(Stat::Unsupported),
            };
            let len = u16::from_be_bytes(f.inner6.len);
            let diff = Sum(0)
                .sub_pseudo6(&f.inner6.src, &f.inner6.dst, len, PROTO_ICMPV6)
                .sub_word(word(old[0], old[1]))
                .word(word(kind, old[1]));
            let [high, low] = replace([old[2], old[3]], diff);
            Ok(Fix::new(0, old, [kind, old[1], high, low]))
        }
    }
}

/// The transport header of the packet in an ICMPv4 error, as IPv6.
#[inline(always)]
fn inner_four_to_six(ctx: &TcContext, l4: usize, f: &Frame) -> Result<Fix, Stat> {
    match f.inner4.proto {
        PROTO_TCP | PROTO_UDP => {
            let diff = Sum(0)
                .sub(&f.inner4.src)
                .sub(&f.inner4.dst)
                .add(&f.inner6.src)
                .add(&f.inner6.dst);
            Ok(checksum_fix(ctx, l4, f.inner4.proto == PROTO_UDP, diff))
        }
        _ => {
            let mut old = [0; 4];
            if read(ctx, l4, &mut old).is_err() {
                return Ok(Fix::NONE);
            }
            let kind = match old[0] {
                8 => 128,
                0 => 129,
                _ => return Err(Stat::Unsupported),
            };
            let len = u16::from_be_bytes(f.inner6.len);
            let diff = Sum(0)
                .pseudo6(&f.inner6.src, &f.inner6.dst, len, PROTO_ICMPV6)
                .sub_word(word(old[0], old[1]))
                .word(word(kind, old[1]));
            let [high, low] = replace([old[2], old[3]], diff);
            Ok(Fix::new(0, old, [kind, old[1], high, low]))
        }
    }
}

/// The TCP or UDP checksum of the packet in an ICMP error, for other
/// addresses, if the error carries it. A UDP checksum of zero stays: there
/// is none.
#[inline(always)]
fn checksum_fix(ctx: &TcContext, l4: usize, udp: bool, diff: Sum) -> Fix {
    let mut old = [0; 4];
    if udp {
        // Length and checksum.
        if read(ctx, l4 + 4, &mut old).is_err() || [old[2], old[3]] == [0, 0] {
            return Fix::NONE;
        }
        let mut check = replace([old[2], old[3]], diff);
        if check == [0, 0] {
            check = [0xff, 0xff];
        }
        Fix::new(4, old, [old[0], old[1], check[0], check[1]])
    } else {
        // Checksum and urgent pointer.
        if read(ctx, l4 + 16, &mut old).is_err() {
            return Fix::NONE;
        }
        let check = replace([old[0], old[1]], diff);
        Fix::new(16, old, [check[0], check[1], old[2], old[3]])
    }
}

/// The checksum of an IPv4 UDP datagram without one, for IPv6
/// (RFC 7915 4.5). The datagram starts at `l4` and is `len` bytes long; the
/// new IPv6 header is in the frame.
#[inline(always)]
fn udp_checksum(ctx: &TcContext, f: &mut Frame, l4: usize, len: u16) -> Result<[u8; 2], Stat> {
    let skb = ctx.skb.skb.cast();
    let buf = f.buf.as_mut_ptr();
    let len = u32::from(len);
    let chunks = len >> 9;
    // bpf_csum_diff sums in the host's byte order.
    let mut seed: u32 = 0;
    let mut chunk: u32 = 0;
    while chunk < 128 {
        if chunk >= chunks {
            break;
        }
        let offset = l4 as u32 + (chunk << 9);
        // SAFETY: the buffer holds 1024 bytes.
        let sum = unsafe {
            if bpf_skb_load_bytes(skb, offset, buf.cast(), 512) != 0 {
                return Err(Stat::Error);
            }
            bpf_csum_diff(ptr::null_mut(), 0, buf.cast(), 512, seed)
        };
        if sum < 0 {
            return Err(Stat::Error);
        }
        seed = sum as u32;
        chunk += 1;
    }
    let tail = len & 511;
    if tail != 0 {
        let offset = l4 as u32 + (chunks << 9);
        // SAFETY: the buffer holds 1024 bytes. The last word is zeroed
        // first, so that the bytes behind the datagram add nothing.
        let sum = unsafe {
            buf.add((tail & 0x1fc) as usize)
                .cast::<u32>()
                .write_unaligned(0);
            if bpf_skb_load_bytes(skb, offset, buf.cast(), tail) != 0 {
                return Err(Stat::Error);
            }
            bpf_csum_diff(ptr::null_mut(), 0, buf.cast(), (tail + 3) & 0x3fc, seed)
        };
        if sum < 0 {
            return Err(Stat::Error);
        }
        seed = sum as u32;
    }
    let data = Sum(u64::from(seed)).fold().swap_bytes();
    let sum = Sum(u64::from(data)).pseudo6(&f.ip6.src, &f.ip6.dst, len as u16, PROTO_UDP);
    let check = !sum.fold();
    count(Stat::UdpChecksum);
    Ok(if check == 0 {
        [0xff, 0xff]
    } else {
        check.to_be_bytes()
    })
}

/// Updates a TCP or UDP checksum for the new pseudo-header. The helper
/// takes the difference in the host's byte order, and also handles
/// checksums that the stack or the device completes later.
#[inline(always)]
fn l4_checksum(ctx: &TcContext, l4: usize, proto: u8, diff: Sum) -> Result<(), Stat> {
    let (offset, flags) = if proto == PROTO_TCP {
        (16, BPF_F_PSEUDO_HDR)
    } else {
        (6, BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0)
    };
    ctx.l4_csum_replace(
        l4 + offset,
        0,
        u64::from(diff.fold().swap_bytes()),
        u64::from(flags),
    )
    .map_err(|_| Stat::Error)
}

/// Writes an IPv4 header. DF is set above 1260 bytes only (RFC 7915 5.1):
/// a smaller packet may still meet an IPv4 link below 1280 bytes, which
/// IPv6 does not know of.
#[allow(clippy::too_many_arguments)]
#[inline(always)]
fn set_ipv4(
    h: &mut Ipv4,
    tos: u8,
    len: u16,
    ttl: u8,
    proto: u8,
    src: [u8; 4],
    dst: [u8; 4],
    id: u16,
) {
    h.ver_ihl = 0x45;
    h.tos = tos;
    h.len = len.to_be_bytes();
    h.id = id.to_be_bytes();
    h.frag = if len > 1260 { [0x40, 0] } else { [0, 0] };
    h.ttl = ttl;
    h.proto = proto;
    h.check = [0, 0];
    h.src = src;
    h.dst = dst;
    h.check = (!Sum(0).ipv4(h).fold()).to_be_bytes();
}

/// Writes an IPv6 header but its addresses. The traffic class is the TOS,
/// the flow label zero.
#[inline(always)]
fn set_ipv6(h: &mut Ipv6, tos: u8, len: u16, hops: u8, next: u8) {
    h.vtc_flow = [0x60 | (tos >> 4), tos << 4, 0, 0];
    h.len = len.to_be_bytes();
    h.next = next;
    h.hops = hops;
}

/// An identification for a packet that may be fragmented on the way.
#[inline(always)]
fn random_id() -> u16 {
    // SAFETY: a helper without arguments.
    unsafe { bpf_get_prandom_u32() as u16 }
}

#[inline(always)]
fn tos(h: &Ipv6) -> u8 {
    (h.vtc_flow[0] << 4) | (h.vtc_flow[1] >> 4)
}

/// A fragment, or a packet that would be one: MF or an offset.
#[inline(always)]
fn fragment(h: &Ipv4) -> bool {
    u16::from_be_bytes(h.frag) & 0x3fff != 0
}

/// A byte of a global that the loader sets.
#[inline(always)]
fn global<const N: usize>(global: &[u8; N], i: usize) -> u8 {
    // SAFETY: `i` is in bounds; a volatile read keeps the compiler from
    // assuming the global's initial value.
    unsafe { ptr::read_volatile(global.as_ptr().add(i)) }
}

/// `prefix + a.b.c.d`.
#[inline(always)]
fn embed(into: &mut [u8; 16], ipv4: &[u8; 4]) {
    for i in 0..12 {
        into[i] = global(&PREFIX, i);
    }
    into[12] = ipv4[0];
    into[13] = ipv4[1];
    into[14] = ipv4[2];
    into[15] = ipv4[3];
}

/// `a.b.c.d` of `prefix + a.b.c.d`.
#[inline(always)]
fn extract(ipv6: &[u8; 16]) -> Option<[u8; 4]> {
    let mut differ = 0;
    for i in 0..12 {
        differ |= ipv6[i] ^ global(&PREFIX, i);
    }
    if differ != 0 {
        return None;
    }
    Some([ipv6[12], ipv6[13], ipv6[14], ipv6[15]])
}

/// What no pod reaches through NAT64: "this network", loopback, link-local,
/// multicast and reserved addresses, and the pods' own pool.
#[inline(always)]
fn denied(ipv4: [u8; 4]) -> bool {
    let pool = u32::from_be_bytes([
        global(&POOL, 0),
        global(&POOL, 1),
        global(&POOL, 2),
        global(&POOL, 3),
    ]);
    let mask = u32::from_be_bytes([
        global(&POOL_MASK, 0),
        global(&POOL_MASK, 1),
        global(&POOL_MASK, 2),
        global(&POOL_MASK, 3),
    ]);
    ipv4[0] == 0
        || ipv4[0] == 127
        || ipv4[0] >= 224
        || (ipv4[0] == 169 && ipv4[1] == 254)
        || u32::from_be_bytes(ipv4) & mask == pool
}

#[inline(always)]
fn pod_ipv4(pod: &[u8; 16]) -> Option<[u8; 4]> {
    // SAFETY: the map is BPF_F_NO_PREALLOC: an entry that is replaced or
    // removed stays readable until the program is done with it.
    unsafe { POD_IPV4.get(pod).copied() }
}

/// Copies the IPv6 address of the pod with this IPv4 address.
#[inline(always)]
fn ipv4_pod(ipv4: &[u8; 4], into: &mut [u8; 16]) -> Result<(), Stat> {
    let pod = IPV4_POD.get_ptr(ipv4).ok_or(Stat::NoMapping)?;
    // SAFETY: as in pod_ipv4; the map's value and the frame are distinct.
    unsafe { ptr::copy_nonoverlapping(pod, into, 1) };
    Ok(())
}

#[inline(always)]
fn count(stat: Stat) {
    if let Some(counter) = STATS.get_ptr_mut(stat as u32) {
        // SAFETY: a per-CPU value, which only this CPU writes.
        unsafe { *counter += 1 };
    }
}

#[inline(always)]
fn load<T>(ctx: &TcContext, offset: usize) -> Result<T, Stat> {
    ctx.load(offset).map_err(|_| Stat::Error)
}

/// Reads a header from the packet into the frame.
#[inline(always)]
fn read<T>(ctx: &TcContext, offset: usize, into: &mut T) -> Result<(), Stat> {
    // SAFETY: `into` holds size_of::<T>() bytes, and any bytes are a T.
    let ret = unsafe {
        bpf_skb_load_bytes(
            ctx.skb.skb.cast(),
            offset as u32,
            ptr::from_mut(into).cast(),
            size_of::<T>() as u32,
        )
    };
    if ret == 0 {
        Ok(())
    } else {
        Err(Stat::Error)
    }
}

#[inline(always)]
fn store<T>(ctx: &TcContext, offset: usize, value: &T) -> Result<(), Stat> {
    ctx.store(offset, value, 0).map_err(|_| Stat::Error)
}

#[inline(always)]
fn change_proto(ctx: &TcContext, proto: u16) -> Result<(), Stat> {
    ctx.change_proto(proto.to_be(), 0).map_err(|_| Stat::Error)
}

/// Rewrites the Ethernet header for the new protocol and hands the packet
/// back to the node, on the device's ingress. The device's own address as
/// the destination makes it a packet for this host.
#[inline(always)]
fn done(ctx: &TcContext, f: &mut Frame, proto: u16, stat: Stat) -> Result<i32, Stat> {
    f.eth.dst = f.eth.src;
    f.eth.proto = proto.to_be_bytes();
    store(ctx, 0, &f.eth)?;
    count(stat);
    // SAFETY: the context is the program's own.
    let ifindex = unsafe { (*ctx.skb.skb).ifindex };
    // SAFETY: a helper on scalars.
    Ok(unsafe { bpf_redirect(ifindex, u64::from(BPF_F_INGRESS)) } as i32)
}

#[unsafe(link_section = "license")]
#[unsafe(no_mangle)]
static LICENSE: [u8; 11] = *b"Apache-2.0\0";

#[cfg(not(test))]
#[panic_handler]
fn panic(_info: &core::panic::PanicInfo) -> ! {
    loop {}
}
