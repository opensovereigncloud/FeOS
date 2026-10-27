// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A WireGuard device's key, port and peers, over the kernel's generic
//! netlink family `wireguard` (`linux/wireguard.h`), as `wg` sets them,
//! without `wg`. The device itself is a link of kind `wireguard`, which
//! rtnetlink makes.

use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, SocketAddrV4, SocketAddrV6};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::time::{Duration, SystemTime};

/// A key of WireGuard's, private or public: 32 bytes of Curve25519.
pub type Key = [u8; 32];

const NETLINK_GENERIC: libc::c_int = 16;
const GENL_ID_CTRL: u16 = 0x10;
const CTRL_CMD_GETFAMILY: u8 = 3;
const CTRL_ATTR_FAMILY_ID: u16 = 1;
const CTRL_ATTR_FAMILY_NAME: u16 = 2;

const WG_GENL_NAME: &str = "wireguard";
const WG_GENL_VERSION: u8 = 1;
const WG_CMD_GET_DEVICE: u8 = 0;
const WG_CMD_SET_DEVICE: u8 = 1;

const WGDEVICE_A_IFNAME: u16 = 2;
const WGDEVICE_A_PRIVATE_KEY: u16 = 3;
const WGDEVICE_A_PUBLIC_KEY: u16 = 4;
const WGDEVICE_A_LISTEN_PORT: u16 = 6;
const WGDEVICE_A_PEERS: u16 = 8;

const WGPEER_A_PUBLIC_KEY: u16 = 1;
const WGPEER_A_FLAGS: u16 = 3;
const WGPEER_A_ENDPOINT: u16 = 4;
const WGPEER_A_LAST_HANDSHAKE_TIME: u16 = 6;
const WGPEER_A_RX_BYTES: u16 = 7;
const WGPEER_A_TX_BYTES: u16 = 8;
const WGPEER_A_ALLOWEDIPS: u16 = 9;
const WGPEER_F_REMOVE_ME: u32 = 1;
const WGPEER_F_REPLACE_ALLOWEDIPS: u32 = 2;

const WGALLOWEDIP_A_FAMILY: u16 = 1;
const WGALLOWEDIP_A_IPADDR: u16 = 2;
const WGALLOWEDIP_A_CIDR_MASK: u16 = 3;

const NLM_F_REQUEST: u16 = 0x1;
const NLM_F_ACK: u16 = 0x4;
const NLM_F_DUMP: u16 = 0x300;
const NLMSG_ERROR: u16 = 0x2;
const NLMSG_DONE: u16 = 0x3;
const NLA_F_NESTED: u16 = 0x8000;
const NLA_TYPE_MASK: u16 = 0x3fff;
/// The header of a netlink message and of generic netlink's after it.
const HEADERS: usize = 16 + 4;
/// How large a message of peers grows before the rest go in the next.
const MESSAGE_SIZE: usize = 32 * 1024;

/// A peer as it is set: its key, where it is, and the prefixes it may
/// send from and that go to it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Peer {
    pub public_key: Key,
    pub endpoint: Option<SocketAddr>,
    pub allowed_ips: Vec<(IpAddr, u8)>,
}

/// A peer as the device has it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct PeerState {
    pub public_key: Key,
    pub endpoint: Option<SocketAddr>,
    pub allowed_ips: Vec<(IpAddr, u8)>,
    /// The last handshake, if there was one.
    pub last_handshake: Option<SystemTime>,
    pub rx_bytes: u64,
    pub tx_bytes: u64,
}

/// A device as the kernel has it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Device {
    /// None before a private key is set.
    pub public_key: Option<Key>,
    pub listen_port: u16,
    pub peers: Vec<PeerState>,
}

/// A private key: 32 random bytes, clamped as Curve25519 wants them.
pub fn generate_key() -> io::Result<Key> {
    let mut key = [0u8; 32];
    let mut filled = 0;
    while filled < key.len() {
        // SAFETY: the buffer outlives the call, which writes at most its
        // length.
        let n =
            unsafe { libc::getrandom(key[filled..].as_mut_ptr().cast(), key.len() - filled, 0) };
        if n < 0 {
            let e = io::Error::last_os_error();
            if e.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return Err(e);
        }
        filled += n as usize;
    }
    key[0] &= 248;
    key[31] = (key[31] & 127) | 64;
    Ok(key)
}

/// Gives the device `name` its private key and its port.
pub fn set_key(name: &str, private_key: &Key, listen_port: u16) -> io::Result<()> {
    let socket = Socket::open()?;
    let family = socket.family()?;
    let message = message(family, WG_CMD_SET_DEVICE, NLM_F_REQUEST | NLM_F_ACK, |m| {
        m.str(WGDEVICE_A_IFNAME, name);
        m.bytes(WGDEVICE_A_PRIVATE_KEY, private_key);
        m.u16(WGDEVICE_A_LISTEN_PORT, listen_port);
    });
    socket.request(&message, |_| Ok(()))
}

/// What the device `name` has: its public key, its port and its peers.
pub fn get(name: &str) -> io::Result<Device> {
    let socket = Socket::open()?;
    let family = socket.family()?;
    let message = message(family, WG_CMD_GET_DEVICE, NLM_F_REQUEST | NLM_F_DUMP, |m| {
        m.str(WGDEVICE_A_IFNAME, name);
    });
    let mut device = Device::default();
    socket.request(&message, |payload| {
        parse_device(payload, &mut device);
        Ok(())
    })?;
    Ok(device)
}

/// Makes `peers` the device's peers: those it has that are not among
/// them go, the others are set, their prefixes replaced. A peer that
/// stays keeps its session, so that what goes to it is not held up.
pub fn set_peers(name: &str, peers: &[Peer]) -> io::Result<()> {
    let current = get(name)?;
    let remove: Vec<Key> = current
        .peers
        .iter()
        .map(|peer| peer.public_key)
        .filter(|key| !peers.iter().any(|peer| peer.public_key == *key))
        .collect();
    let changed: Vec<&Peer> = peers
        .iter()
        .filter(|peer| {
            !current.peers.iter().any(|have| {
                have.public_key == peer.public_key
                    && have.endpoint == peer.endpoint
                    && same_prefixes(&have.allowed_ips, &peer.allowed_ips)
            })
        })
        .collect();
    if remove.is_empty() && changed.is_empty() {
        return Ok(());
    }
    let socket = Socket::open()?;
    let family = socket.family()?;
    for message in peer_messages(family, name, &remove, &changed) {
        socket.request(&message, |_| Ok(()))?;
    }
    Ok(())
}

/// Whether two lists hold the same prefixes, in any order.
fn same_prefixes(a: &[(IpAddr, u8)], b: &[(IpAddr, u8)]) -> bool {
    a.len() == b.len() && a.iter().all(|prefix| b.contains(prefix))
}

/// The messages that remove `remove` and set `set`, each kept under
/// [`MESSAGE_SIZE`].
fn peer_messages(family: u16, name: &str, remove: &[Key], set: &[&Peer]) -> Vec<Vec<u8>> {
    let mut entries: Vec<Vec<u8>> = Vec::new();
    for key in remove {
        let mut entry = Vec::new();
        Attrs { buf: &mut entry }.nested(0, |m| {
            m.bytes(WGPEER_A_PUBLIC_KEY, key);
            m.u32(WGPEER_A_FLAGS, WGPEER_F_REMOVE_ME);
        });
        entries.push(entry);
    }
    for peer in set {
        let mut entry = Vec::new();
        Attrs { buf: &mut entry }.nested(0, |m| peer_attrs(m, peer));
        entries.push(entry);
    }
    let mut messages = Vec::new();
    let mut batch: Vec<u8> = Vec::new();
    for entry in entries {
        if !batch.is_empty() && batch.len() + entry.len() > MESSAGE_SIZE {
            messages.push(device_message(family, name, &batch));
            batch.clear();
        }
        batch.extend_from_slice(&entry);
    }
    if !batch.is_empty() {
        messages.push(device_message(family, name, &batch));
    }
    messages
}

fn device_message(family: u16, name: &str, peers: &[u8]) -> Vec<u8> {
    message(family, WG_CMD_SET_DEVICE, NLM_F_REQUEST | NLM_F_ACK, |m| {
        m.str(WGDEVICE_A_IFNAME, name);
        m.nested(WGDEVICE_A_PEERS, |m| m.raw(peers));
    })
}

/// A peer's attributes: its key, its endpoint and its prefixes, which
/// replace those it had.
fn peer_attrs(m: &mut Attrs, peer: &Peer) {
    m.bytes(WGPEER_A_PUBLIC_KEY, &peer.public_key);
    m.u32(WGPEER_A_FLAGS, WGPEER_F_REPLACE_ALLOWEDIPS);
    if let Some(endpoint) = peer.endpoint {
        m.bytes(WGPEER_A_ENDPOINT, &sockaddr(endpoint));
    }
    m.nested(WGPEER_A_ALLOWEDIPS, |m| {
        for (address, len) in &peer.allowed_ips {
            m.nested(0, |m| {
                let (family, bytes) = match address {
                    IpAddr::V4(v4) => (libc::AF_INET as u16, v4.octets().to_vec()),
                    IpAddr::V6(v6) => (libc::AF_INET6 as u16, v6.octets().to_vec()),
                };
                m.u16(WGALLOWEDIP_A_FAMILY, family);
                m.bytes(WGALLOWEDIP_A_IPADDR, &bytes);
                m.u8(WGALLOWEDIP_A_CIDR_MASK, *len);
            });
        }
    });
}

/// A `sockaddr_in` or `sockaddr_in6`, as the kernel lays them out.
fn sockaddr(address: SocketAddr) -> Vec<u8> {
    let mut buf = Vec::new();
    match address {
        SocketAddr::V4(v4) => {
            buf.extend_from_slice(&(libc::AF_INET as u16).to_ne_bytes());
            buf.extend_from_slice(&v4.port().to_be_bytes());
            buf.extend_from_slice(&v4.ip().octets());
            buf.extend_from_slice(&[0; 8]);
        }
        SocketAddr::V6(v6) => {
            buf.extend_from_slice(&(libc::AF_INET6 as u16).to_ne_bytes());
            buf.extend_from_slice(&v6.port().to_be_bytes());
            buf.extend_from_slice(&v6.flowinfo().to_be_bytes());
            buf.extend_from_slice(&v6.ip().octets());
            buf.extend_from_slice(&v6.scope_id().to_ne_bytes());
        }
    }
    buf
}

fn parse_sockaddr(buf: &[u8]) -> Option<SocketAddr> {
    let family = u16::from_ne_bytes(buf.get(0..2)?.try_into().ok()?);
    let port = u16::from_be_bytes(buf.get(2..4)?.try_into().ok()?);
    if family == libc::AF_INET as u16 {
        let ip: [u8; 4] = buf.get(4..8)?.try_into().ok()?;
        Some(SocketAddrV4::new(Ipv4Addr::from(ip), port).into())
    } else if family == libc::AF_INET6 as u16 {
        let flowinfo = u32::from_be_bytes(buf.get(4..8)?.try_into().ok()?);
        let ip: [u8; 16] = buf.get(8..24)?.try_into().ok()?;
        let scope = u32::from_ne_bytes(buf.get(24..28)?.try_into().ok()?);
        Some(SocketAddrV6::new(Ipv6Addr::from(ip), port, flowinfo, scope).into())
    } else {
        None
    }
}

/// Adds what one message of a dump says about the device to `device`: a
/// device with many peers comes in several messages, each with some.
fn parse_device(payload: &[u8], device: &mut Device) {
    for (ty, value) in attrs(payload) {
        match ty {
            WGDEVICE_A_PUBLIC_KEY => {
                if let Ok(key) = <Key>::try_from(value) {
                    // A device without a private key has a key of zeroes.
                    device.public_key = (key != [0; 32]).then_some(key);
                }
            }
            WGDEVICE_A_LISTEN_PORT => {
                if let Ok(port) = <[u8; 2]>::try_from(value) {
                    device.listen_port = u16::from_ne_bytes(port);
                }
            }
            WGDEVICE_A_PEERS => {
                for (_, peer) in attrs(value) {
                    let state = parse_peer(peer);
                    // A peer's prefixes may go on in the next message.
                    match device
                        .peers
                        .iter_mut()
                        .find(|have| have.public_key == state.public_key)
                    {
                        Some(have) => have.allowed_ips.extend(state.allowed_ips),
                        None => device.peers.push(state),
                    }
                }
            }
            _ => {}
        }
    }
}

fn parse_peer(payload: &[u8]) -> PeerState {
    let mut peer = PeerState::default();
    for (ty, value) in attrs(payload) {
        match ty {
            WGPEER_A_PUBLIC_KEY => {
                if let Ok(key) = <Key>::try_from(value) {
                    peer.public_key = key;
                }
            }
            WGPEER_A_ENDPOINT => peer.endpoint = parse_sockaddr(value),
            WGPEER_A_LAST_HANDSHAKE_TIME => {
                // struct __kernel_timespec: two 64-bit integers.
                let seconds = value
                    .get(0..8)
                    .and_then(|b| b.try_into().ok())
                    .map(i64::from_ne_bytes);
                let nanos = value
                    .get(8..16)
                    .and_then(|b| b.try_into().ok())
                    .map(i64::from_ne_bytes);
                if let (Some(seconds), Some(nanos)) = (seconds, nanos)
                    && (seconds, nanos) != (0, 0)
                {
                    peer.last_handshake = SystemTime::UNIX_EPOCH.checked_add(Duration::new(
                        u64::try_from(seconds).unwrap_or_default(),
                        u32::try_from(nanos).unwrap_or_default(),
                    ));
                }
            }
            WGPEER_A_RX_BYTES => peer.rx_bytes = u64_of(value),
            WGPEER_A_TX_BYTES => peer.tx_bytes = u64_of(value),
            WGPEER_A_ALLOWEDIPS => {
                for (_, allowed) in attrs(value) {
                    if let Some(prefix) = parse_allowed_ip(allowed) {
                        peer.allowed_ips.push(prefix);
                    }
                }
            }
            _ => {}
        }
    }
    peer
}

fn parse_allowed_ip(payload: &[u8]) -> Option<(IpAddr, u8)> {
    let mut address = None;
    let mut len = None;
    for (ty, value) in attrs(payload) {
        match ty {
            WGALLOWEDIP_A_IPADDR => {
                address = match value.len() {
                    4 => <[u8; 4]>::try_from(value)
                        .ok()
                        .map(|b| IpAddr::V4(b.into())),
                    16 => <[u8; 16]>::try_from(value)
                        .ok()
                        .map(|b| IpAddr::V6(b.into())),
                    _ => None,
                }
            }
            WGALLOWEDIP_A_CIDR_MASK => len = value.first().copied(),
            _ => {}
        }
    }
    Some((address?, len?))
}

fn u64_of(value: &[u8]) -> u64 {
    <[u8; 8]>::try_from(value)
        .map(u64::from_ne_bytes)
        .unwrap_or_default()
}

/// The attributes in `buf`, their types without the flags.
fn attrs(mut buf: &[u8]) -> Vec<(u16, &[u8])> {
    let mut out = Vec::new();
    while buf.len() >= 4 {
        let len = u16::from_ne_bytes([buf[0], buf[1]]) as usize;
        let ty = u16::from_ne_bytes([buf[2], buf[3]]) & NLA_TYPE_MASK;
        if len < 4 || len > buf.len() {
            break;
        }
        out.push((ty, &buf[4..len]));
        buf = &buf[((len + 3) & !3).min(buf.len())..];
    }
    out
}

/// Attributes of generic netlink, in the host's byte order.
struct Attrs<'a> {
    buf: &'a mut Vec<u8>,
}

impl Attrs<'_> {
    fn attr(&mut self, ty: u16, payload: &[u8]) {
        let len = 4 + payload.len();
        self.buf.extend_from_slice(&(len as u16).to_ne_bytes());
        self.buf.extend_from_slice(&ty.to_ne_bytes());
        self.buf.extend_from_slice(payload);
        pad(self.buf);
    }

    fn str(&mut self, ty: u16, value: &str) {
        let mut bytes = value.as_bytes().to_vec();
        bytes.push(0);
        self.attr(ty, &bytes);
    }

    fn u8(&mut self, ty: u16, value: u8) {
        self.attr(ty, &[value]);
    }

    fn u16(&mut self, ty: u16, value: u16) {
        self.attr(ty, &value.to_ne_bytes());
    }

    fn u32(&mut self, ty: u16, value: u32) {
        self.attr(ty, &value.to_ne_bytes());
    }

    fn bytes(&mut self, ty: u16, value: &[u8]) {
        self.attr(ty, value);
    }

    /// Attributes encoded already.
    fn raw(&mut self, attrs: &[u8]) {
        self.buf.extend_from_slice(attrs);
    }

    fn nested(&mut self, ty: u16, fill: impl FnOnce(&mut Attrs)) {
        let start = self.buf.len();
        self.buf.extend_from_slice(&[0; 4]);
        fill(&mut Attrs { buf: self.buf });
        let len = (self.buf.len() - start) as u16;
        self.buf[start..start + 2].copy_from_slice(&len.to_ne_bytes());
        self.buf[start + 2..start + 4].copy_from_slice(&(ty | NLA_F_NESTED).to_ne_bytes());
    }
}

fn pad(buf: &mut Vec<u8>) {
    while !buf.len().is_multiple_of(4) {
        buf.push(0);
    }
}

/// A socket of generic netlink, for one call after the other.
struct Socket {
    fd: OwnedFd,
}

impl Socket {
    fn open() -> io::Result<Self> {
        // SAFETY: plain socket(2); the fd is owned from here on.
        let fd = unsafe {
            libc::socket(
                libc::AF_NETLINK,
                libc::SOCK_RAW | libc::SOCK_CLOEXEC,
                NETLINK_GENERIC,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: fd is a socket this function just opened.
        Ok(Self {
            fd: unsafe { OwnedFd::from_raw_fd(fd) },
        })
    }

    /// WireGuard's family ID, which the kernel gives the family when its
    /// module loads: asking for it loads the module.
    fn family(&self) -> io::Result<u16> {
        let message = message(
            GENL_ID_CTRL,
            CTRL_CMD_GETFAMILY,
            NLM_F_REQUEST | NLM_F_ACK,
            |m| m.str(CTRL_ATTR_FAMILY_NAME, WG_GENL_NAME),
        );
        let mut family = None;
        self.request(&message, |payload| {
            for (ty, value) in attrs(payload) {
                if ty == CTRL_ATTR_FAMILY_ID
                    && let Ok(id) = <[u8; 2]>::try_from(value)
                {
                    family = Some(u16::from_ne_bytes(id));
                }
            }
            Ok(())
        })
        .map_err(|e| match e.kind() {
            io::ErrorKind::NotFound => {
                io::Error::new(e.kind(), "the kernel has no WireGuard (CONFIG_WIREGUARD)")
            }
            _ => e,
        })?;
        family.ok_or_else(|| io::Error::other("the kernel did not name WireGuard's family"))
    }

    /// Sends a request and hands the payload of every answer, after the
    /// generic netlink header, to `each`, until the acknowledgement or the
    /// end of a dump.
    fn request(
        &self,
        message: &[u8],
        mut each: impl FnMut(&[u8]) -> io::Result<()>,
    ) -> io::Result<()> {
        // SAFETY: sockaddr_nl is plain data; all zeroes addresses the kernel.
        let mut kernel: libc::sockaddr_nl = unsafe { std::mem::zeroed() };
        kernel.nl_family = libc::AF_NETLINK as libc::sa_family_t;
        // SAFETY: the buffer and the address outlive the call.
        let sent = unsafe {
            libc::sendto(
                self.fd.as_raw_fd(),
                message.as_ptr().cast(),
                message.len(),
                0,
                (&kernel as *const libc::sockaddr_nl).cast(),
                size_of::<libc::sockaddr_nl>() as libc::socklen_t,
            )
        };
        if sent < 0 {
            return Err(io::Error::last_os_error());
        }
        let seq = u32::from_ne_bytes(message[8..12].try_into().unwrap_or_default());
        let mut buf = vec![0u8; 65536];
        loop {
            // SAFETY: the buffer outlives the call.
            let n =
                unsafe { libc::recv(self.fd.as_raw_fd(), buf.as_mut_ptr().cast(), buf.len(), 0) };
            if n < 0 {
                let e = io::Error::last_os_error();
                if e.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(e);
            }
            let mut rest = &buf[..n as usize];
            while rest.len() >= 16 {
                let len = u32::from_ne_bytes(rest[0..4].try_into().unwrap_or_default()) as usize;
                let ty = u16::from_ne_bytes(rest[4..6].try_into().unwrap_or_default());
                let answers = u32::from_ne_bytes(rest[8..12].try_into().unwrap_or_default());
                if len < 16 || len > rest.len() {
                    break;
                }
                if answers == seq {
                    match ty {
                        NLMSG_DONE => return Ok(()),
                        NLMSG_ERROR if len >= 20 => {
                            let code =
                                i32::from_ne_bytes(rest[16..20].try_into().unwrap_or_default());
                            if code == 0 {
                                return Ok(());
                            }
                            return Err(io::Error::from_raw_os_error(-code));
                        }
                        _ if len >= HEADERS => each(&rest[HEADERS..len])?,
                        _ => {}
                    }
                }
                rest = &rest[((len + 3) & !3).min(rest.len())..];
            }
        }
    }
}

/// A netlink header, generic netlink's (the command and the version) and
/// the attributes.
fn message(ty: u16, cmd: u8, flags: u16, fill: impl FnOnce(&mut Attrs)) -> Vec<u8> {
    static SEQ: std::sync::atomic::AtomicU32 = std::sync::atomic::AtomicU32::new(1);
    let seq = SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
    let mut buf = Vec::new();
    buf.extend_from_slice(&[0; 4]); // nlmsg_len
    buf.extend_from_slice(&ty.to_ne_bytes());
    buf.extend_from_slice(&flags.to_ne_bytes());
    buf.extend_from_slice(&seq.to_ne_bytes());
    buf.extend_from_slice(&0u32.to_ne_bytes()); // nlmsg_pid
    buf.push(cmd);
    buf.push(if ty == GENL_ID_CTRL {
        2
    } else {
        WG_GENL_VERSION
    });
    buf.extend_from_slice(&[0; 2]);
    fill(&mut Attrs { buf: &mut buf });
    let len = buf.len() as u32;
    buf[0..4].copy_from_slice(&len.to_ne_bytes());
    buf
}

#[cfg(test)]
mod tests {
    use super::*;

    fn peer(n: u8, endpoint: &str, prefix: &str) -> Peer {
        let (address, len) = prefix.split_once('/').unwrap();
        Peer {
            public_key: [n; 32],
            endpoint: Some(endpoint.parse().unwrap()),
            allowed_ips: vec![(address.parse().unwrap(), len.parse().unwrap())],
        }
    }

    #[test]
    fn keys_are_clamped() {
        let a = generate_key().unwrap();
        let b = generate_key().unwrap();
        assert_ne!(a, b);
        assert_eq!(a[0] & 7, 0);
        assert_eq!(a[31] & 0xc0, 0x40);
    }

    #[test]
    fn endpoints_are_laid_out_as_the_kernel_does() {
        for endpoint in ["10.0.0.11:51820", "[2001:db8::11]:51820"] {
            let address: SocketAddr = endpoint.parse().unwrap();
            let bytes = sockaddr(address);
            assert_eq!(bytes.len(), if address.is_ipv4() { 16 } else { 28 });
            assert_eq!(parse_sockaddr(&bytes), Some(address));
        }
        assert_eq!(
            &sockaddr("10.0.0.11:51820".parse().unwrap())[2..8],
            [0xca, 0x6c, 10, 0, 0, 11]
        );
    }

    /// A peer comes back as it was put, through the attributes the kernel
    /// answers with.
    #[test]
    fn peers_are_encoded_as_they_are_parsed() {
        let peer = peer(7, "10.0.0.11:51820", "fd5e:1b2c:93a0:7::/64");
        let mut buf = Vec::new();
        Attrs { buf: &mut buf }.nested(WGDEVICE_A_PEERS, |m| m.nested(0, |m| peer_attrs(m, &peer)));
        let mut device = Device::default();
        parse_device(&buf, &mut device);
        assert_eq!(
            device.peers,
            [PeerState {
                public_key: peer.public_key,
                endpoint: peer.endpoint,
                allowed_ips: peer.allowed_ips.clone(),
                ..Default::default()
            }]
        );
    }

    #[test]
    fn many_peers_take_several_messages() {
        let peers: Vec<Peer> = (0..=255u8)
            .chain(0..=255u8)
            .map(|n| peer(n, "10.0.0.11:51820", "fd5e:1b2c:93a0:7::/64"))
            .collect();
        let set: Vec<&Peer> = peers.iter().collect();
        let messages = peer_messages(30, "feos-mesh", &[[9; 32]], &set);
        assert!(messages.len() > 1);
        assert!(messages.iter().all(|m| m.len() <= MESSAGE_SIZE + 512));
        // Every peer is in one of them, the removed one first.
        let mut keys = Vec::new();
        for message in &messages {
            for (ty, value) in attrs(&message[HEADERS..]) {
                if ty == WGDEVICE_A_PEERS {
                    keys.extend(
                        attrs(value)
                            .into_iter()
                            .map(|(_, p)| parse_peer(p).public_key),
                    );
                }
            }
        }
        assert_eq!(keys.len(), 513);
        assert_eq!(keys[0], [9; 32]);
    }

    /// Makes a device and its peers in the kernel. Needs CAP_NET_ADMIN, in
    /// a network namespace of its own, and a kernel with WireGuard:
    /// `sudo unshare -n <the test binary> --ignored the_kernel_takes_a_device`.
    #[test]
    #[ignore]
    fn the_kernel_takes_a_device() {
        let alone = std::fs::read_to_string("/proc/self/net/dev").is_ok_and(|dev| {
            dev.lines()
                .skip(2)
                .all(|line| line.trim_start().starts_with("lo:"))
        });
        assert!(alone, "run this in a network namespace of its own");
        let status = std::process::Command::new("ip")
            .args(["link", "add", "wg-test", "type", "wireguard"])
            .status()
            .unwrap();
        assert!(status.success());
        let key = generate_key().unwrap();
        set_key("wg-test", &key, 51820).unwrap();
        let device = get("wg-test").unwrap();
        assert_eq!(device.listen_port, 51820);
        let public = device.public_key.unwrap();
        assert_ne!(public, key);
        assert!(device.peers.is_empty());

        let mut peers: Vec<Peer> = (1..=3u8)
            .map(|n| {
                peer(
                    n,
                    &format!("10.0.0.{n}:51820"),
                    &format!("fd5e:1b2c:93a0:{n}::/64"),
                )
            })
            .collect();
        set_peers("wg-test", &peers).unwrap();
        let have = |device: &Device| {
            let mut have: Vec<Peer> = device
                .peers
                .iter()
                .map(|p| Peer {
                    public_key: p.public_key,
                    endpoint: p.endpoint,
                    allowed_ips: p.allowed_ips.clone(),
                })
                .collect();
            have.sort_by_key(|p| p.public_key);
            have
        };
        let want = |peers: &[Peer]| {
            let mut want = peers.to_vec();
            want.sort_by_key(|p| p.public_key);
            want
        };
        assert_eq!(have(&get("wg-test").unwrap()), want(&peers));
        // One goes, one moves, one comes.
        peers.remove(0);
        peers[0].endpoint = Some("10.0.0.22:51820".parse().unwrap());
        peers.push(peer(4, "[2001:db8::4]:51820", "fd5e:1b2c:93a0:4::/64"));
        set_peers("wg-test", &peers).unwrap();
        assert_eq!(have(&get("wg-test").unwrap()), want(&peers));
        set_peers("wg-test", &[]).unwrap();
        assert!(get("wg-test").unwrap().peers.is_empty());
        assert_eq!(get("wg-test").unwrap().public_key, Some(public));
        assert!(get("no-such-device").is_err());
    }
}
