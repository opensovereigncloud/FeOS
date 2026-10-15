// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Netfilter's netlink sockets (NETLINK_NETFILTER): opening them, and
//! reading their messages and attributes. No libmnl, no libnetfilter_*.
//!
//! Whatever the kernel sends is read defensively: a message or attribute
//! that does not fit ends the walk, it never panics.

use nix::sys::socket::{
    self, sockopt, AddressFamily, MsgFlags, NetlinkAddr, SockFlag, SockProtocol, SockType,
};
use nix::sys::time::{TimeVal, TimeValLike};
use std::io;
use std::os::fd::{AsRawFd, OwnedFd};
use std::time::Duration;

pub const NLM_F_REQUEST: u16 = 0x1;
pub const NLM_F_ACK: u16 = 0x4;
/// NLM_F_ROOT and NLM_F_MATCH: all of a table.
pub const NLM_F_DUMP: u16 = 0x300;
pub const NLA_F_NESTED: u16 = 0x8000;
const NLMSG_ERROR: u16 = 0x2;
const NLMSG_DONE: u16 = 0x3;
const NLMSG_HDRLEN: usize = 16;
/// The attribute type without NLA_F_NESTED and NLA_F_NET_BYTEORDER.
const NLA_TYPE_MASK: u16 = 0x3fff;

/// A netlink socket of netfilter.
pub struct Socket {
    fd: OwnedFd,
    seq: u32,
}

impl Socket {
    /// Opens a socket that joins the multicast groups in `groups`, a bit
    /// per group (group n is bit n - 1).
    pub fn open(groups: u32) -> io::Result<Self> {
        let fd = socket::socket(
            AddressFamily::Netlink,
            SockType::Raw,
            SockFlag::SOCK_CLOEXEC,
            SockProtocol::NetlinkNetFilter,
        )?;
        socket::bind(fd.as_raw_fd(), &NetlinkAddr::new(0, groups))?;
        Ok(Self { fd, seq: 0 })
    }

    /// Asks for a receive buffer of `bytes`: beyond net.core.rmem_max
    /// where the process may (CAP_NET_ADMIN), up to it otherwise.
    pub fn set_receive_buffer(&self, bytes: usize) {
        if socket::setsockopt(&self.fd, sockopt::RcvBufForce, &bytes).is_err()
            && let Err(e) = socket::setsockopt(&self.fd, sockopt::RcvBuf, &bytes)
        {
            log::warn!("cannot size a netlink receive buffer: {e}");
        }
    }

    /// Makes `recv` give up after `timeout`, with EAGAIN.
    pub fn set_timeout(&self, timeout: Duration) -> io::Result<()> {
        let micros = i64::try_from(timeout.as_micros()).unwrap_or(i64::MAX);
        let timeout = TimeVal::microseconds(micros);
        socket::setsockopt(&self.fd, sockopt::ReceiveTimeout, &timeout)?;
        Ok(())
    }

    /// Sends a request to the kernel and waits for its acknowledgement;
    /// `build` gets the sequence number to put into the message. Other
    /// messages that arrive in between are dropped.
    pub fn request(&mut self, build: impl FnOnce(u32) -> Vec<u8>) -> io::Result<()> {
        self.seq = self.seq.wrapping_add(1);
        let seq = self.seq;
        self.send(&build(seq))?;
        let mut buf = vec![0u8; 65536];
        loop {
            let n = self.recv(&mut buf)?;
            for message in messages(&buf[..n]) {
                if message.kind != NLMSG_ERROR || message.seq != seq {
                    continue;
                }
                return status(&message);
            }
        }
    }

    /// Sends a dump request (NLM_F_DUMP) and hands each message of the
    /// answer to `each`, until the kernel says it is done.
    pub fn dump(
        &mut self,
        build: impl FnOnce(u32) -> Vec<u8>,
        mut each: impl FnMut(&Message),
    ) -> io::Result<()> {
        self.seq = self.seq.wrapping_add(1);
        let seq = self.seq;
        self.send(&build(seq))?;
        // More than a datagram of a dump holds (32 KiB at most).
        let mut buf = vec![0u8; 65536];
        loop {
            let n = self.recv(&mut buf)?;
            for message in messages(&buf[..n]) {
                match message.kind {
                    _ if message.seq != seq => {}
                    // Old kernels end a dump without an error code.
                    NLMSG_DONE if message.payload.len() < 4 => return Ok(()),
                    NLMSG_ERROR | NLMSG_DONE => return status(&message),
                    _ => each(&message),
                }
            }
        }
    }

    /// Sends messages to the kernel.
    pub fn send(&self, messages: &[u8]) -> io::Result<()> {
        let kernel = NetlinkAddr::new(0, 0);
        socket::sendto(self.fd.as_raw_fd(), messages, &kernel, MsgFlags::empty())?;
        Ok(())
    }

    pub fn recv(&self, buf: &mut [u8]) -> io::Result<usize> {
        Ok(socket::recv(self.fd.as_raw_fd(), buf, MsgFlags::empty())?)
    }

    /// The messages the kernel dropped for this socket so far, because
    /// its receive buffer was full: the Drops column of
    /// /proc/net/netlink. None when it cannot be read.
    pub fn drops(&self) -> Option<u64> {
        let inode = nix::sys::stat::fstat(&self.fd).ok()?.st_ino;
        // The calling thread's network namespace, which is the socket's.
        let table = std::fs::read_to_string("/proc/thread-self/net/netlink").ok()?;
        drops_in(&table, inode)
    }
}

/// The Drops of the socket with this inode in /proc/net/netlink, whose
/// columns are sk, Eth, Pid, Groups, Rmem, Wmem, Dump, Locks, Drops and
/// Inode.
fn drops_in(table: &str, inode: u64) -> Option<u64> {
    table.lines().skip(1).find_map(|line| {
        let columns: Vec<&str> = line.split_whitespace().collect();
        if columns.len() < 10 || columns[9].parse::<u64>().ok()? != inode {
            return None;
        }
        columns[8].parse().ok()
    })
}

/// A netlink message: its header's type, flags and sequence number, and
/// what follows the header.
#[derive(Clone, Copy, Debug)]
pub struct Message<'a> {
    pub kind: u16,
    pub seq: u32,
    pub payload: &'a [u8],
}

impl<'a> Message<'a> {
    /// The nfgenmsg header of a netfilter message, its family and its
    /// resource ID (in network order on the wire), and the attributes.
    pub fn netfilter(&self) -> Option<(u8, u16, &'a [u8])> {
        let header = self.payload.get(..4)?;
        let res_id = u16::from_be_bytes([header[2], header[3]]);
        Some((header[0], res_id, &self.payload[4..]))
    }
}

/// The netlink messages in what one `recv` returned.
pub fn messages(buf: &[u8]) -> impl Iterator<Item = Message<'_>> {
    let mut rest = buf;
    std::iter::from_fn(move || {
        let header = rest.get(..NLMSG_HDRLEN)?;
        let len = u32::from_ne_bytes([header[0], header[1], header[2], header[3]]) as usize;
        if len < NLMSG_HDRLEN || len > rest.len() {
            rest = &[];
            return None;
        }
        let message = Message {
            kind: u16::from_ne_bytes([header[4], header[5]]),
            seq: u32::from_ne_bytes([header[8], header[9], header[10], header[11]]),
            payload: &rest[NLMSG_HDRLEN..len],
        };
        rest = rest.get(align(len)..).unwrap_or(&[]);
        Some(message)
    })
}

/// The attributes in `buf`, as their type (without flags) and payload.
pub fn attributes(buf: &[u8]) -> impl Iterator<Item = (u16, &[u8])> {
    let mut rest = buf;
    std::iter::from_fn(move || {
        let header = rest.get(..4)?;
        let len = u16::from_ne_bytes([header[0], header[1]]) as usize;
        if len < 4 || len > rest.len() {
            rest = &[];
            return None;
        }
        let kind = u16::from_ne_bytes([header[2], header[3]]) & NLA_TYPE_MASK;
        let payload = &rest[4..len];
        rest = rest.get(align(len)..).unwrap_or(&[]);
        Some((kind, payload))
    })
}

/// The payload of the first attribute of this type.
pub fn attribute(buf: &[u8], kind: u16) -> Option<&[u8]> {
    attributes(buf).find(|(k, _)| *k == kind).map(|(_, v)| v)
}

pub fn be16(buf: &[u8]) -> Option<u16> {
    Some(u16::from_be_bytes(buf.get(..2)?.try_into().ok()?))
}

pub fn be32(buf: &[u8]) -> Option<u32> {
    Some(u32::from_be_bytes(buf.get(..4)?.try_into().ok()?))
}

pub fn be64(buf: &[u8]) -> Option<u64> {
    Some(u64::from_be_bytes(buf.get(..8)?.try_into().ok()?))
}

/// A netfilter request: the netlink header, nfgenmsg and flat
/// attributes.
pub fn request(
    kind: u16,
    flags: u16,
    seq: u32,
    family: u8,
    res_id: u16,
    attrs: &[(u16, &[u8])],
) -> Vec<u8> {
    let mut buf = Vec::with_capacity(64);
    buf.extend_from_slice(&[0; 4]); // nlmsg_len
    buf.extend_from_slice(&kind.to_ne_bytes());
    buf.extend_from_slice(&flags.to_ne_bytes());
    buf.extend_from_slice(&seq.to_ne_bytes());
    buf.extend_from_slice(&0u32.to_ne_bytes()); // nlmsg_pid
    buf.push(family);
    buf.push(0); // NFNETLINK_V0
    buf.extend_from_slice(&res_id.to_be_bytes());
    buf.extend_from_slice(&nested(attrs));
    let len = buf.len() as u32;
    buf[..4].copy_from_slice(&len.to_ne_bytes());
    buf
}

/// Attributes one after the other, as the payload of a nested one.
pub fn nested(attrs: &[(u16, &[u8])]) -> Vec<u8> {
    let mut buf = Vec::new();
    for (kind, payload) in attrs {
        buf.extend_from_slice(&((4 + payload.len()) as u16).to_ne_bytes());
        buf.extend_from_slice(&kind.to_ne_bytes());
        buf.extend_from_slice(payload);
        buf.resize(align(buf.len()), 0);
    }
    buf
}

/// What an NLMSG_ERROR (an acknowledgement, with 0) or NLMSG_DONE says:
/// the error code that its payload begins with.
fn status(message: &Message) -> io::Result<()> {
    let code = message
        .payload
        .get(..4)
        .map(|b| i32::from_ne_bytes([b[0], b[1], b[2], b[3]]))
        .unwrap_or(-libc::EPROTO);
    match code {
        0 => Ok(()),
        code => Err(io::Error::from_raw_os_error(-code)),
    }
}

fn align(len: usize) -> usize {
    (len + 3) & !3
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn requests_parse_back() {
        let buf = request(
            0x401,
            NLM_F_REQUEST,
            7,
            0,
            3,
            &[(1, &[1]), (2, &[0, 0, 8, 0, 2, 0])],
        );
        assert_eq!(buf.len(), 16 + 4 + 8 + 12);
        let messages: Vec<_> = messages(&buf).collect();
        assert_eq!(messages.len(), 1);
        assert_eq!(messages[0].kind, 0x401);
        assert_eq!(messages[0].seq, 7);
        let (family, res_id, attrs) = messages[0].netfilter().unwrap();
        assert_eq!((family, res_id), (0, 3));
        let attrs: Vec<_> = attributes(attrs).collect();
        assert_eq!(attrs, vec![(1, &[1u8][..]), (2, &[0, 0, 8, 0, 2, 0][..])]);
    }

    #[test]
    fn garbage_ends_the_walk() {
        assert_eq!(messages(&[0xff; 15]).count(), 0);
        // A length beyond the buffer, and one below the header's.
        let mut buf = request(1, 0, 1, 0, 0, &[]);
        buf[0] = 200;
        assert_eq!(messages(&buf).count(), 0);
        buf[0] = 3;
        assert_eq!(messages(&buf).count(), 0);
        assert_eq!(attributes(&[8, 0, 1, 0, 1]).count(), 0);
        assert_eq!(attributes(&[2, 0, 1, 0]).count(), 0);
        assert_eq!(be32(&[1, 2, 3]), None);
    }

    #[test]
    fn nested_flags_are_masked() {
        let attrs = [8u8, 0, 0x01, 0x80, 1, 2, 3, 4];
        assert_eq!(attribute(&attrs, 1), Some(&[1u8, 2, 3, 4][..]));
    }

    #[test]
    fn drops_are_read_by_inode() {
        let table = "\
sk               Eth Pid        Groups   Rmem     Wmem     Dump  Locks    Drops    Inode
0000000000000000 12  4026531840 00000005 0        0        0     2        17       123456
0000000000000000 0   1          00000000 0        0        0     2        0        42
";
        assert_eq!(drops_in(table, 123456), Some(17));
        assert_eq!(drops_in(table, 42), Some(0));
        assert_eq!(drops_in(table, 7), None);
    }
}
