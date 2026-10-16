// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's IPv4 (ADR 0010): a DHCPv4 client (RFC 2131) on the uplink.
//! It takes a lease, puts its address, its default route and its resolvers
//! on the node, and keeps the lease while the node runs. A node without
//! DHCPv4 goes on without IPv4, and the client keeps asking.

use super::resolv;
use dhcproto::v4::{DhcpOption, Flags, Message, MessageType, Opcode, OptionCode};
use dhcproto::{Decodable, Decoder, Encodable, Encoder};
use futures::stream::TryStreamExt;
use log::{debug, info, warn};
use rtnetlink::packet_route::address::{AddressAttribute, AddressMessage};
use rtnetlink::packet_route::route::{RouteMessage, RouteProtocol};
use rtnetlink::{Handle, RouteMessageBuilder};
use socket2::{Domain, Protocol, SockAddr, Socket, Type};
use std::io;
use std::net::{IpAddr, Ipv4Addr, SocketAddrV4};
use std::time::Duration;
use tokio::net::UdpSocket;
use tokio::time::{sleep, sleep_until, timeout_at, Instant};

const CLIENT_PORT: u16 = 68;
const SERVER_PORT: u16 = 67;
/// The first wait for an answer, doubled after every one that does not
/// come, up to [`MAX_WAIT`] (RFC 2131, section 4.1).
const FIRST_WAIT: Duration = Duration::from_secs(4);
const MAX_WAIT: Duration = Duration::from_secs(64);
/// How often the client asks for an offered address before it starts over.
const REQUESTS: u32 = 4;
/// The shortest wait between requests that extend a lease (RFC 2131,
/// section 4.4.5).
const MIN_EXTEND_WAIT: Duration = Duration::from_secs(60);
/// How many unanswered discoveries go by between two warnings.
const WARN_EVERY: u32 = 10;
/// The shortest message the client sends: a BOOTP message whose options
/// take 312 octets (RFC 2131, section 2). Some servers, QEMU's among them,
/// take none shorter.
const MIN_MESSAGE_LEN: usize = 548;

/// What a server granted.
#[derive(Clone, Debug, PartialEq)]
pub struct Lease {
    pub address: Ipv4Addr,
    pub prefix_len: u8,
    pub router: Option<Ipv4Addr>,
    pub dns: Vec<Ipv4Addr>,
    pub server: Ipv4Addr,
    /// The lease's time, its renewal (T1) and rebinding (T2) times, from
    /// the moment it was granted; none for a lease without end.
    pub times: Option<Times>,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Times {
    pub renew: Duration,
    pub rebind: Duration,
    pub end: Duration,
}

impl Lease {
    /// The lease an acknowledgement grants, or none if it lacks an address
    /// or the server's identifier.
    fn from_ack(ack: &Message) -> Option<Self> {
        let address = ack.yiaddr();
        if address.is_unspecified() || address.is_broadcast() || address.is_multicast() {
            return None;
        }
        let opts = ack.opts();
        let server = match opts.get(OptionCode::ServerIdentifier) {
            Some(DhcpOption::ServerIdentifier(server)) => *server,
            _ => return None,
        };
        let prefix_len = match opts.get(OptionCode::SubnetMask) {
            Some(DhcpOption::SubnetMask(mask)) => prefix_len(*mask),
            _ => None,
        }
        // Without a mask, the router is reached on the link.
        .unwrap_or(32);
        let router = match opts.get(OptionCode::Router) {
            Some(DhcpOption::Router(routers)) => routers
                .iter()
                .copied()
                .find(|router| !router.is_unspecified()),
            _ => None,
        };
        let dns = match opts.get(OptionCode::DomainNameServer) {
            Some(DhcpOption::DomainNameServer(servers)) => servers.clone(),
            _ => Vec::new(),
        };
        let seconds = |code| match opts.get(code) {
            Some(DhcpOption::AddressLeaseTime(s))
            | Some(DhcpOption::Renewal(s))
            | Some(DhcpOption::Rebinding(s)) => Some(*s),
            _ => None,
        };
        let times = seconds(OptionCode::AddressLeaseTime)
            .filter(|&end| end != u32::MAX)
            .map(|end| {
                times(
                    end,
                    seconds(OptionCode::Renewal),
                    seconds(OptionCode::Rebinding),
                )
            });
        Some(Self {
            address,
            prefix_len,
            router,
            dns,
            server,
            times,
        })
    }

    /// Whether the router is outside the lease's subnet, and is reached on
    /// the link.
    fn router_on_link(&self, router: Ipv4Addr) -> bool {
        let host_bits = 32 - u32::from(self.prefix_len);
        u32::from(router).checked_shr(host_bits) != u32::from(self.address).checked_shr(host_bits)
            || self.prefix_len == 32
    }
}

/// T1 and T2 as the server gave them where they make sense, else half and
/// seven eighths of the lease (RFC 2131, section 4.4.5).
fn times(end: u32, renew: Option<u32>, rebind: Option<u32>) -> Times {
    let end = u64::from(end);
    let rebind = rebind
        .map(u64::from)
        .filter(|&rebind| rebind < end)
        .unwrap_or(end * 7 / 8);
    let renew = renew
        .map(u64::from)
        .filter(|&renew| renew < rebind)
        .unwrap_or((end / 2).min(rebind));
    Times {
        renew: Duration::from_secs(renew),
        rebind: Duration::from_secs(rebind),
        end: Duration::from_secs(end),
    }
}

/// The length of a subnet mask's prefix, if its ones are contiguous.
fn prefix_len(mask: Ipv4Addr) -> Option<u8> {
    let mask = u32::from(mask);
    let len = mask.leading_ones();
    (mask.checked_shl(len).unwrap_or(0) == 0).then_some(len as u8)
}

/// Takes and keeps a lease on the interface, for as long as the node runs.
/// Every failure is logged, and the client asks again.
pub async fn run(handle: Handle, interface: String, index: u32, mac: Vec<u8>) {
    let socket = loop {
        match client_socket(&interface) {
            Ok(socket) => break socket,
            Err(e) => {
                warn!("DHCPv4: No socket on {interface}, trying again: {e}");
                sleep(MAX_WAIT).await;
            }
        }
    };
    loop {
        let (mut lease, mut granted) = acquire(&socket, &interface, &mac).await;
        apply(&handle, &interface, index, None, &lease).await;
        // A lease without end is kept without asking again.
        while let Some(times) = lease.times {
            sleep_until(granted + times.renew).await;
            match extend(&socket, &mac, &lease, granted).await {
                Some((extended, at)) => {
                    if extended != lease {
                        apply(&handle, &interface, index, Some(&lease), &extended).await;
                    } else {
                        debug!("DHCPv4: Renewed {} on {interface}", lease.address);
                    }
                    lease = extended;
                    granted = at;
                }
                None => {
                    warn!(
                        "DHCPv4: The lease of {} on {interface} ended; asking for a new one",
                        lease.address
                    );
                    remove(&handle, index, &lease).await;
                    break;
                }
            }
        }
        if lease.times.is_none() {
            std::future::pending::<()>().await;
        }
    }
}

/// A UDP socket on the client's port of the interface, which sends to and
/// takes broadcasts before the interface has an IPv4 address.
fn client_socket(interface: &str) -> io::Result<UdpSocket> {
    let socket = Socket::new(Domain::IPV4, Type::DGRAM, Some(Protocol::UDP))?;
    socket.set_reuse_address(true)?;
    socket.set_broadcast(true)?;
    socket.bind_device(Some(interface.as_bytes()))?;
    socket.bind(&SockAddr::from(SocketAddrV4::new(
        Ipv4Addr::UNSPECIFIED,
        CLIENT_PORT,
    )))?;
    socket.set_nonblocking(true)?;
    UdpSocket::from_std(socket.into())
}

/// A client's message. `broadcast` asks the server to broadcast its answer,
/// to a client that does not hold its address yet.
fn message(kind: MessageType, xid: u32, mac: &[u8], ciaddr: Ipv4Addr, broadcast: bool) -> Message {
    let unspecified = Ipv4Addr::UNSPECIFIED;
    let mut message = Message::new_with_id(xid, ciaddr, unspecified, unspecified, unspecified, mac);
    if broadcast {
        message.set_flags(Flags::default().set_broadcast());
    }
    let mut client_id = vec![1];
    client_id.extend_from_slice(mac);
    let opts = message.opts_mut();
    opts.insert(DhcpOption::MessageType(kind));
    opts.insert(DhcpOption::ClientIdentifier(client_id));
    opts.insert(DhcpOption::ParameterRequestList(vec![
        OptionCode::SubnetMask,
        OptionCode::Router,
        OptionCode::DomainNameServer,
        OptionCode::AddressLeaseTime,
        OptionCode::Renewal,
        OptionCode::Rebinding,
    ]));
    message
}

/// The message on the wire, padded after its end option.
fn encode(message: &Message) -> Result<Vec<u8>, dhcproto::error::EncodeError> {
    let mut buf = Vec::new();
    message.encode(&mut Encoder::new(&mut buf))?;
    if buf.len() < MIN_MESSAGE_LEN {
        buf.resize(MIN_MESSAGE_LEN, 0);
    }
    Ok(buf)
}

async fn send(socket: &UdpSocket, message: &Message, to: Ipv4Addr) {
    let buf = match encode(message) {
        Ok(buf) => buf,
        Err(e) => {
            warn!("DHCPv4: Failed to encode a message: {e}");
            return;
        }
    };
    if let Err(e) = socket
        .send_to(&buf, SocketAddrV4::new(to, SERVER_PORT))
        .await
    {
        debug!("DHCPv4: Failed to send to {to}: {e}");
    }
}

/// The first answer to the exchange `xid` of one of the kinds, within
/// `wait`.
async fn answer(
    socket: &UdpSocket,
    xid: u32,
    mac: &[u8],
    kinds: &[MessageType],
    wait: Duration,
) -> Option<Message> {
    let deadline = Instant::now() + wait;
    let mut buf = [0u8; 1500];
    loop {
        let len = match timeout_at(deadline, socket.recv_from(&mut buf)).await {
            Err(_) => return None,
            Ok(Err(e)) => {
                debug!("DHCPv4: Failed to receive: {e}");
                sleep_until(deadline).await;
                return None;
            }
            Ok(Ok((len, _))) => len,
        };
        let Ok(message) = Message::decode(&mut Decoder::new(&buf[..len])) else {
            continue;
        };
        let ours = message.opcode() == Opcode::BootReply
            && message.xid() == xid
            && message.chaddr().get(..mac.len()) == Some(mac);
        if ours
            && message
                .opts()
                .msg_type()
                .is_some_and(|kind| kinds.contains(&kind))
        {
            return Some(message);
        }
    }
}

/// Discovers a server and takes the address it offers, asking until one
/// answers. Returns the lease and when it was granted.
async fn acquire(socket: &UdpSocket, interface: &str, mac: &[u8]) -> (Lease, Instant) {
    let mut wait = FIRST_WAIT;
    let mut unanswered = 0u32;
    loop {
        let xid = rand::random();
        send(
            socket,
            &message(MessageType::Discover, xid, mac, Ipv4Addr::UNSPECIFIED, true),
            Ipv4Addr::BROADCAST,
        )
        .await;
        let Some(offer) = answer(socket, xid, mac, &[MessageType::Offer], wait).await else {
            if unanswered.is_multiple_of(WARN_EVERY) {
                warn!("DHCPv4: No offer on {interface}; the node has no IPv4 yet, asking again");
            }
            unanswered += 1;
            wait = (wait * 2).min(MAX_WAIT);
            continue;
        };
        let Some(DhcpOption::ServerIdentifier(server)) =
            offer.opts().get(OptionCode::ServerIdentifier).cloned()
        else {
            continue;
        };
        let mut request = message(MessageType::Request, xid, mac, Ipv4Addr::UNSPECIFIED, true);
        request
            .opts_mut()
            .insert(DhcpOption::RequestedIpAddress(offer.yiaddr()));
        request
            .opts_mut()
            .insert(DhcpOption::ServerIdentifier(server));
        let kinds = [MessageType::Ack, MessageType::Nak];
        for attempt in 0..REQUESTS {
            let sent = Instant::now();
            send(socket, &request, Ipv4Addr::BROADCAST).await;
            match answer(socket, xid, mac, &kinds, FIRST_WAIT * 2u32.pow(attempt)).await {
                Some(ack) if ack.opts().msg_type() == Some(MessageType::Ack) => {
                    if let Some(lease) = Lease::from_ack(&ack) {
                        return (lease, sent);
                    }
                    warn!("DHCPv4: {server} acknowledged without an address or its identifier");
                    break;
                }
                Some(_) => {
                    info!("DHCPv4: {server} declined to give {}", offer.yiaddr());
                    break;
                }
                None => {}
            }
        }
        wait = FIRST_WAIT;
    }
}

/// Extends the lease, granted at `granted`: from its server until T2, then
/// from any. None once it ended or a server declined it.
async fn extend(
    socket: &UdpSocket,
    mac: &[u8],
    lease: &Lease,
    granted: Instant,
) -> Option<(Lease, Instant)> {
    let times = lease.times?;
    let rebind = granted + times.rebind;
    let end = granted + times.end;
    loop {
        let now = Instant::now();
        if now >= end {
            return None;
        }
        let (to, phase_end) = if now < rebind {
            (lease.server, rebind)
        } else {
            (Ipv4Addr::BROADCAST, end)
        };
        let wait = ((phase_end - now) / 2).max(MIN_EXTEND_WAIT).min(end - now);
        let xid = rand::random();
        let request = message(MessageType::Request, xid, mac, lease.address, false);
        let sent = Instant::now();
        send(socket, &request, to).await;
        let kinds = [MessageType::Ack, MessageType::Nak];
        match answer(socket, xid, mac, &kinds, wait).await {
            Some(ack) if ack.opts().msg_type() == Some(MessageType::Ack) => {
                if let Some(extended) = Lease::from_ack(&ack) {
                    return Some((extended, sent));
                }
            }
            Some(_) => return None,
            None => {}
        }
    }
}

/// Puts the lease on the node: its address, the default route and its
/// resolvers, after taking off what an earlier lease put there and this
/// one does not have.
async fn apply(handle: &Handle, interface: &str, index: u32, old: Option<&Lease>, new: &Lease) {
    if let Some(old) = old.filter(|old| {
        (old.address, old.prefix_len, old.router) != (new.address, new.prefix_len, new.router)
    }) {
        remove(handle, index, old).await;
    }
    let add = handle
        .address()
        .add(index, IpAddr::V4(new.address), new.prefix_len);
    match add.execute().await {
        Err(e) if errno(&e) == Some(libc::EEXIST) => {}
        Err(e) => warn!(
            "DHCPv4: Failed to add {}/{} to {interface}: {e}",
            new.address, new.prefix_len
        ),
        Ok(()) => {}
    }
    if let Some(router) = new.router
        && let Err(e) = handle
            .route()
            .add(default_route(new, router, index))
            .replace()
            .execute()
            .await
    {
        warn!("DHCPv4: Failed to route by {router} on {interface}: {e}");
    }
    resolv::set_ipv4(&new.dns);
    let until = match new.times {
        Some(times) => format!("for {}s", times.end.as_secs()),
        None => "without end".to_string(),
    };
    info!(
        "DHCPv4: {}/{} on {interface}, routed by {}, from {}, {until}",
        new.address,
        new.prefix_len,
        new.router
            .map_or_else(|| "no router".to_string(), |router| router.to_string()),
        new.server
    );
}

/// Takes the lease's default route and address off the node.
async fn remove(handle: &Handle, index: u32, lease: &Lease) {
    if let Some(router) = lease.router
        && let Err(e) = handle
            .route()
            .del(default_route(lease, router, index))
            .execute()
            .await
        && errno(&e) != Some(libc::ESRCH)
    {
        warn!("DHCPv4: Failed to remove the route by {router}: {e}");
    }
    let messages: Vec<AddressMessage> = match handle
        .address()
        .get()
        .set_link_index_filter(index)
        .execute()
        .try_collect()
        .await
    {
        Ok(messages) => messages,
        Err(e) => {
            warn!("DHCPv4: Failed to list the uplink's addresses: {e}");
            return;
        }
    };
    let address = IpAddr::V4(lease.address);
    for message in messages.into_iter().filter(|message| {
        message.header.prefix_len == lease.prefix_len
            && message
                .attributes
                .contains(&AddressAttribute::Address(address))
    }) {
        if let Err(e) = handle.address().del(message).execute().await {
            warn!("DHCPv4: Failed to remove {}: {e}", lease.address);
        }
    }
}

fn default_route(lease: &Lease, router: Ipv4Addr, index: u32) -> RouteMessage {
    let route = RouteMessageBuilder::<Ipv4Addr>::new()
        .gateway(router)
        .output_interface(index)
        .protocol(RouteProtocol::Dhcp);
    if lease.router_on_link(router) {
        route.onlink().build()
    } else {
        route.build()
    }
}

fn errno(error: &rtnetlink::Error) -> Option<i32> {
    match error {
        rtnetlink::Error::NetlinkError(e) => Some(-e.raw_code()),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ack(opts: Vec<DhcpOption>) -> Message {
        let mac = [2, 0, 0, 0, 0, 1];
        let mut ack = Message::new_with_id(
            7,
            Ipv4Addr::UNSPECIFIED,
            Ipv4Addr::new(10, 0, 2, 15),
            Ipv4Addr::UNSPECIFIED,
            Ipv4Addr::UNSPECIFIED,
            &mac,
        );
        ack.set_opcode(Opcode::BootReply);
        ack.opts_mut()
            .insert(DhcpOption::MessageType(MessageType::Ack));
        for opt in opts {
            ack.opts_mut().insert(opt);
        }
        ack
    }

    #[test]
    fn a_lease_is_what_the_ack_grants() {
        let lease = Lease::from_ack(&ack(vec![
            DhcpOption::ServerIdentifier(Ipv4Addr::new(10, 0, 2, 2)),
            DhcpOption::SubnetMask(Ipv4Addr::new(255, 255, 255, 0)),
            DhcpOption::Router(vec![Ipv4Addr::new(10, 0, 2, 2)]),
            DhcpOption::DomainNameServer(vec![Ipv4Addr::new(10, 0, 2, 3)]),
            DhcpOption::AddressLeaseTime(86400),
        ]))
        .unwrap();
        assert_eq!(
            lease,
            Lease {
                address: Ipv4Addr::new(10, 0, 2, 15),
                prefix_len: 24,
                router: Some(Ipv4Addr::new(10, 0, 2, 2)),
                dns: vec![Ipv4Addr::new(10, 0, 2, 3)],
                server: Ipv4Addr::new(10, 0, 2, 2),
                times: Some(Times {
                    renew: Duration::from_secs(43200),
                    rebind: Duration::from_secs(75600),
                    end: Duration::from_secs(86400),
                }),
            }
        );
        assert!(!lease.router_on_link(Ipv4Addr::new(10, 0, 2, 2)));
        assert!(lease.router_on_link(Ipv4Addr::new(10, 0, 3, 1)));
    }

    #[test]
    fn an_ack_without_address_or_server_grants_nothing() {
        assert!(Lease::from_ack(&ack(vec![])).is_none());
        let mut no_address = ack(vec![DhcpOption::ServerIdentifier(Ipv4Addr::new(
            10, 0, 2, 2,
        ))]);
        no_address.set_yiaddr(Ipv4Addr::UNSPECIFIED);
        assert!(Lease::from_ack(&no_address).is_none());
    }

    #[test]
    fn without_a_mask_the_router_is_on_the_link() {
        let lease = Lease::from_ack(&ack(vec![
            DhcpOption::ServerIdentifier(Ipv4Addr::new(169, 254, 169, 254)),
            DhcpOption::Router(vec![Ipv4Addr::new(10, 0, 0, 1)]),
        ]))
        .unwrap();
        assert_eq!(lease.prefix_len, 32);
        assert_eq!(lease.times, None);
        assert!(lease.router_on_link(Ipv4Addr::new(10, 0, 0, 1)));
    }

    #[test]
    fn renewal_and_rebinding_follow_the_server_where_they_make_sense() {
        let given = times(3600, Some(600), Some(1200));
        assert_eq!(given.renew, Duration::from_secs(600));
        assert_eq!(given.rebind, Duration::from_secs(1200));
        // T2 past the lease's end, T1 past T2: the defaults.
        let defaults = times(3600, Some(3200), Some(4000));
        assert_eq!(defaults.rebind, Duration::from_secs(3150));
        assert_eq!(defaults.renew, Duration::from_secs(1800));
        let infinite = Lease::from_ack(&ack(vec![
            DhcpOption::ServerIdentifier(Ipv4Addr::new(10, 0, 2, 2)),
            DhcpOption::AddressLeaseTime(u32::MAX),
        ]))
        .unwrap();
        assert_eq!(infinite.times, None);
    }

    #[test]
    fn masks_have_contiguous_ones() {
        assert_eq!(prefix_len(Ipv4Addr::new(255, 255, 255, 0)), Some(24));
        assert_eq!(prefix_len(Ipv4Addr::new(255, 255, 240, 0)), Some(20));
        assert_eq!(prefix_len(Ipv4Addr::BROADCAST), Some(32));
        assert_eq!(prefix_len(Ipv4Addr::UNSPECIFIED), Some(0));
        assert_eq!(prefix_len(Ipv4Addr::new(255, 0, 255, 0)), None);
    }

    #[test]
    fn a_discovery_asks_for_a_broadcast_answer() {
        let mac = [2, 0, 0, 0, 0, 1];
        let discover = message(MessageType::Discover, 9, &mac, Ipv4Addr::UNSPECIFIED, true);
        let buf = encode(&discover).unwrap();
        assert_eq!(buf.len(), MIN_MESSAGE_LEN);
        let decoded = Message::decode(&mut Decoder::new(&buf)).unwrap();
        assert!(decoded.flags().broadcast());
        assert_eq!(decoded.xid(), 9);
        assert_eq!(decoded.chaddr(), &mac);
        assert_eq!(decoded.opts().msg_type(), Some(MessageType::Discover));
        let renewal = message(
            MessageType::Request,
            9,
            &mac,
            Ipv4Addr::new(10, 0, 2, 15),
            false,
        );
        assert!(!renewal.flags().broadcast());
        assert_eq!(renewal.ciaddr(), Ipv4Addr::new(10, 0, 2, 15));
    }
}
