// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! nftables over netlink: tables, chains and the few expressions FeOS's
//! rules need, sent as one batch, which the kernel applies atomically or
//! not at all. No `nft` binary and no libnftnl.
//!
//! A [`Table`] is replaced whole: the batch adds it (a no-op if it
//! exists), deletes it with everything in it, and adds it again with its
//! chains and rules.

use std::io;
use std::net::Ipv4Addr;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};

const NETLINK_NETFILTER: libc::c_int = 12;
const NFNL_SUBSYS_NFTABLES: u16 = 10;
const NFNL_MSG_BATCH_BEGIN: u16 = 0x10;
const NFNL_MSG_BATCH_END: u16 = 0x11;

const NFT_MSG_NEWTABLE: u16 = 0;
const NFT_MSG_DELTABLE: u16 = 2;
const NFT_MSG_NEWCHAIN: u16 = 3;
const NFT_MSG_NEWRULE: u16 = 6;

const NLM_F_REQUEST: u16 = 0x1;
const NLM_F_ACK: u16 = 0x4;
const NLM_F_CREATE: u16 = 0x400;
const NLM_F_APPEND: u16 = 0x800;
const NLMSG_ERROR: u16 = 0x2;
const NLA_F_NESTED: u16 = 0x8000;

const NFTA_TABLE_NAME: u16 = 1;
const NFTA_CHAIN_TABLE: u16 = 1;
const NFTA_CHAIN_NAME: u16 = 3;
const NFTA_CHAIN_HOOK: u16 = 4;
const NFTA_CHAIN_POLICY: u16 = 5;
const NFTA_CHAIN_TYPE: u16 = 7;
const NFTA_HOOK_HOOKNUM: u16 = 1;
const NFTA_HOOK_PRIORITY: u16 = 2;
const NFTA_RULE_TABLE: u16 = 1;
const NFTA_RULE_CHAIN: u16 = 2;
const NFTA_RULE_EXPRESSIONS: u16 = 4;
const NFTA_LIST_ELEM: u16 = 1;
const NFTA_EXPR_NAME: u16 = 1;
const NFTA_EXPR_DATA: u16 = 2;
const NFTA_DATA_VALUE: u16 = 1;
const NFTA_DATA_VERDICT: u16 = 2;
const NFTA_VERDICT_CODE: u16 = 1;

const NFT_REG_VERDICT: u32 = 0;
const NFT_REG_1: u32 = 1;
const NF_DROP: u32 = 0;
const NF_ACCEPT: u32 = 1;

/// nftables families.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Family {
    Inet = 1,
    Ip = 2,
    Bridge = 7,
}

/// The netfilter hooks of the families FeOS uses: the inet ones, which
/// the bridge family numbers the same.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Hook {
    Prerouting = 0,
    Input = 1,
    Forward = 2,
    Output = 3,
    Postrouting = 4,
}

#[derive(Clone, Debug, PartialEq)]
pub struct Table {
    pub family: Family,
    pub name: String,
    pub chains: Vec<Chain>,
}

/// A base chain, with the policy accept.
#[derive(Clone, Debug, PartialEq)]
pub struct Chain {
    pub name: String,
    /// "filter" or "nat".
    pub kind: &'static str,
    pub hook: Hook,
    pub priority: i32,
    pub rules: Vec<Rule>,
}

/// Expressions, evaluated in order; a rule ends with a verdict or masq.
pub type Rule = Vec<Expr>;

/// Where a payload is read from.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Base {
    LinkLayer = 0,
    Network = 1,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub enum CmpOp {
    Eq = 0,
    Neq = 1,
}

/// Keys of the meta expression.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum MetaKey {
    Protocol = 2,
    IifName = 6,
    OifName = 7,
    NfProto = 15,
}

#[derive(Clone, Debug, PartialEq)]
pub enum Expr {
    /// Loads a meta key into register 1.
    Meta(MetaKey),
    /// Loads `len` bytes at `offset` of a header into register 1.
    Payload {
        base: Base,
        offset: u32,
        len: u32,
    },
    /// Register 1 &= mask (^ 0).
    Bitwise {
        mask: Vec<u8>,
    },
    /// Compares register 1 with data.
    Cmp {
        op: CmpOp,
        data: Vec<u8>,
    },
    Masquerade,
    Drop,
    Accept,
}

/// Matches of the rules FeOS writes, as expressions.
pub mod matches {
    use super::*;

    /// The interface name as nft compares it: IFNAMSIZ bytes, zero padded.
    pub fn ifname(name: &str) -> Vec<u8> {
        let mut bytes = name.as_bytes().to_vec();
        bytes.resize(libc::IFNAMSIZ, 0);
        bytes
    }

    pub fn iifname(op: CmpOp, name: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::IifName),
            Expr::Cmp {
                op,
                data: ifname(name),
            },
        ]
    }

    pub fn oifname(op: CmpOp, name: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::OifName),
            Expr::Cmp {
                op,
                data: ifname(name),
            },
        ]
    }

    /// An IPv4 packet, in the inet family.
    pub fn ipv4() -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::NfProto),
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![libc::NFPROTO_IPV4 as u8],
            },
        ]
    }

    /// The IPv4 source (offset 12) or destination (16) in, or out of, a
    /// prefix.
    pub fn ipv4_prefix(offset: u32, op: CmpOp, address: Ipv4Addr, len: u8) -> Vec<Expr> {
        let mask = u32::MAX.checked_shl(32 - u32::from(len)).unwrap_or(0);
        let net = u32::from(address) & mask;
        vec![
            Expr::Payload {
                base: Base::Network,
                offset,
                len: 4,
            },
            Expr::Bitwise {
                mask: mask.to_be_bytes().to_vec(),
            },
            Expr::Cmp {
                op,
                data: net.to_be_bytes().to_vec(),
            },
        ]
    }

    pub fn ipv4_saddr(op: CmpOp, address: Ipv4Addr) -> Vec<Expr> {
        ipv4_prefix(12, op, address, 32)
    }

    pub fn ipv4_daddr_in(address: Ipv4Addr, len: u8) -> Vec<Expr> {
        ipv4_prefix(16, CmpOp::Eq, address, len)
    }

    /// The EtherType of a frame, in the bridge family.
    pub fn ether_type(op: CmpOp, ether_type: u16) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::LinkLayer,
                offset: 12,
                len: 2,
            },
            Expr::Cmp {
                op,
                data: ether_type.to_be_bytes().to_vec(),
            },
        ]
    }

    pub fn ether_saddr(op: CmpOp, mac: [u8; 6]) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::LinkLayer,
                offset: 6,
                len: 6,
            },
            Expr::Cmp {
                op,
                data: mac.to_vec(),
            },
        ]
    }

    /// ARP's sender hardware (offset 8) and protocol (14) address.
    pub fn arp_sha(op: CmpOp, mac: [u8; 6]) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::Network,
                offset: 8,
                len: 6,
            },
            Expr::Cmp {
                op,
                data: mac.to_vec(),
            },
        ]
    }

    pub fn arp_spa(op: CmpOp, address: Ipv4Addr) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::Network,
                offset: 14,
                len: 4,
            },
            Expr::Cmp {
                op,
                data: address.octets().to_vec(),
            },
        ]
    }
}

/// Replaces `table` atomically: whatever was in it goes.
pub fn replace(table: &Table) -> io::Result<()> {
    send(&replace_batch(table, 1))
}

/// Deletes `table`, if it exists.
pub fn delete(family: Family, name: &str) -> io::Result<()> {
    let mut batch = Batch::new(1);
    batch.message(NFT_MSG_NEWTABLE, NLM_F_CREATE, family, |m| {
        m.str(NFTA_TABLE_NAME, name)
    });
    batch.message(NFT_MSG_DELTABLE, 0, family, |m| {
        m.str(NFTA_TABLE_NAME, name)
    });
    send(&batch.finish())
}

fn replace_batch(table: &Table, seq: u32) -> Vec<u8> {
    let mut batch = Batch::new(seq);
    let name = table.name.as_str();
    // Add, delete and add again: the first add makes the delete find a
    // table on a node that has none yet.
    batch.message(NFT_MSG_NEWTABLE, NLM_F_CREATE, table.family, |m| {
        m.str(NFTA_TABLE_NAME, name)
    });
    batch.message(NFT_MSG_DELTABLE, 0, table.family, |m| {
        m.str(NFTA_TABLE_NAME, name)
    });
    batch.message(NFT_MSG_NEWTABLE, NLM_F_CREATE, table.family, |m| {
        m.str(NFTA_TABLE_NAME, name)
    });
    for chain in &table.chains {
        batch.message(NFT_MSG_NEWCHAIN, NLM_F_CREATE, table.family, |m| {
            m.str(NFTA_CHAIN_TABLE, name);
            m.str(NFTA_CHAIN_NAME, &chain.name);
            m.nested(NFTA_CHAIN_HOOK, |m| {
                m.u32(NFTA_HOOK_HOOKNUM, chain.hook as u32);
                m.u32(NFTA_HOOK_PRIORITY, chain.priority as u32);
            });
            m.u32(NFTA_CHAIN_POLICY, NF_ACCEPT);
            m.str(NFTA_CHAIN_TYPE, chain.kind);
        });
        for rule in &chain.rules {
            batch.message(
                NFT_MSG_NEWRULE,
                NLM_F_CREATE | NLM_F_APPEND,
                table.family,
                |m| {
                    m.str(NFTA_RULE_TABLE, name);
                    m.str(NFTA_RULE_CHAIN, &chain.name);
                    m.nested(NFTA_RULE_EXPRESSIONS, |m| {
                        for expr in rule {
                            m.nested(NFTA_LIST_ELEM, |m| expression(m, expr));
                        }
                    });
                },
            );
        }
    }
    batch.finish()
}

fn expression(m: &mut Attrs, expr: &Expr) {
    let data_value = |m: &mut Attrs, ty: u16, value: &[u8]| {
        m.nested(ty, |m| m.bytes(NFTA_DATA_VALUE, value));
    };
    match expr {
        Expr::Meta(key) => {
            m.str(NFTA_EXPR_NAME, "meta");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_META_DREG
                m.u32(2, *key as u32); // NFTA_META_KEY
            });
        }
        Expr::Payload { base, offset, len } => {
            m.str(NFTA_EXPR_NAME, "payload");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_PAYLOAD_DREG
                m.u32(2, *base as u32); // NFTA_PAYLOAD_BASE
                m.u32(3, *offset); // NFTA_PAYLOAD_OFFSET
                m.u32(4, *len); // NFTA_PAYLOAD_LEN
            });
        }
        Expr::Bitwise { mask } => {
            m.str(NFTA_EXPR_NAME, "bitwise");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_BITWISE_SREG
                m.u32(2, NFT_REG_1); // NFTA_BITWISE_DREG
                m.u32(3, mask.len() as u32); // NFTA_BITWISE_LEN
                data_value(m, 4, mask); // NFTA_BITWISE_MASK
                data_value(m, 5, &vec![0; mask.len()]); // NFTA_BITWISE_XOR
            });
        }
        Expr::Cmp { op, data } => {
            m.str(NFTA_EXPR_NAME, "cmp");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_CMP_SREG
                m.u32(2, *op as u32); // NFTA_CMP_OP
                data_value(m, 3, data); // NFTA_CMP_DATA
            });
        }
        Expr::Masquerade => m.str(NFTA_EXPR_NAME, "masq"),
        Expr::Drop | Expr::Accept => {
            let code = if *expr == Expr::Drop {
                NF_DROP
            } else {
                NF_ACCEPT
            };
            m.str(NFTA_EXPR_NAME, "immediate");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_VERDICT); // NFTA_IMMEDIATE_DREG
                m.nested(2, |m| {
                    // NFTA_IMMEDIATE_DATA
                    m.nested(NFTA_DATA_VERDICT, |m| m.u32(NFTA_VERDICT_CODE, code));
                });
            });
        }
    }
}

/// Netlink attributes, appended to a message.
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

    /// nftables' integers are big endian.
    fn u32(&mut self, ty: u16, value: u32) {
        self.attr(ty, &value.to_be_bytes());
    }

    fn bytes(&mut self, ty: u16, value: &[u8]) {
        self.attr(ty, value);
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

/// Netlink messages of nfnetlink, between a batch's begin and end.
struct Batch {
    buf: Vec<u8>,
    seq: u32,
}

impl Batch {
    fn new(seq: u32) -> Self {
        let mut batch = Self {
            buf: Vec::new(),
            seq,
        };
        batch.header(
            NFNL_MSG_BATCH_BEGIN,
            NLM_F_REQUEST,
            0,
            NFNL_SUBSYS_NFTABLES,
            |_| {},
        );
        batch
    }

    fn message(&mut self, ty: u16, flags: u16, family: Family, fill: impl FnOnce(&mut Attrs)) {
        self.header(
            (NFNL_SUBSYS_NFTABLES << 8) | ty,
            NLM_F_REQUEST | NLM_F_ACK | flags,
            family as u8,
            0,
            fill,
        );
    }

    fn finish(mut self) -> Vec<u8> {
        self.header(
            NFNL_MSG_BATCH_END,
            NLM_F_REQUEST,
            0,
            NFNL_SUBSYS_NFTABLES,
            |_| {},
        );
        self.buf
    }

    /// A netlink header, nfgenmsg (family, version 0, resource ID) and the
    /// attributes.
    fn header(
        &mut self,
        ty: u16,
        flags: u16,
        family: u8,
        res_id: u16,
        fill: impl FnOnce(&mut Attrs),
    ) {
        let start = self.buf.len();
        self.buf.extend_from_slice(&[0; 4]); // nlmsg_len
        self.buf.extend_from_slice(&ty.to_ne_bytes());
        self.buf.extend_from_slice(&flags.to_ne_bytes());
        self.buf.extend_from_slice(&self.seq.to_ne_bytes());
        self.buf.extend_from_slice(&0u32.to_ne_bytes()); // nlmsg_pid
        self.buf.push(family);
        self.buf.push(0);
        self.buf.extend_from_slice(&res_id.to_be_bytes());
        fill(&mut Attrs { buf: &mut self.buf });
        let len = (self.buf.len() - start) as u32;
        self.buf[start..start + 4].copy_from_slice(&len.to_ne_bytes());
        self.seq += 1;
    }
}

/// Sends a batch and reads the acknowledgements: the first error fails it.
fn send(batch: &[u8]) -> io::Result<()> {
    // SAFETY: plain socket(2); the fd is owned from here on.
    let fd = unsafe {
        libc::socket(
            libc::AF_NETLINK,
            libc::SOCK_RAW | libc::SOCK_CLOEXEC,
            NETLINK_NETFILTER,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: fd is a socket this function just opened.
    let socket = unsafe { OwnedFd::from_raw_fd(fd) };
    // SAFETY: sockaddr_nl is plain data; all zeroes addresses the kernel.
    let mut kernel: libc::sockaddr_nl = unsafe { std::mem::zeroed() };
    kernel.nl_family = libc::AF_NETLINK as libc::sa_family_t;
    // SAFETY: the buffer and the address outlive the call.
    let sent = unsafe {
        libc::sendto(
            socket.as_raw_fd(),
            batch.as_ptr().cast(),
            batch.len(),
            0,
            (&kernel as *const libc::sockaddr_nl).cast(),
            size_of::<libc::sockaddr_nl>() as libc::socklen_t,
        )
    };
    if sent < 0 {
        return Err(io::Error::last_os_error());
    }
    // One acknowledgement per message between begin and end.
    let mut pending = count_messages(batch).saturating_sub(2);
    let mut buf = vec![0u8; 65536];
    while pending > 0 {
        // SAFETY: the buffer outlives the call.
        let n = unsafe { libc::recv(socket.as_raw_fd(), buf.as_mut_ptr().cast(), buf.len(), 0) };
        if n < 0 {
            return Err(io::Error::last_os_error());
        }
        let mut rest = &buf[..n as usize];
        while rest.len() >= 16 {
            let len = u32::from_ne_bytes(rest[0..4].try_into().unwrap_or_default()) as usize;
            let ty = u16::from_ne_bytes(rest[4..6].try_into().unwrap_or_default());
            if len < 16 || len > rest.len() {
                break;
            }
            if ty == NLMSG_ERROR && len >= 20 {
                let code = i32::from_ne_bytes(rest[16..20].try_into().unwrap_or_default());
                if code != 0 {
                    return Err(io::Error::from_raw_os_error(-code));
                }
                pending -= 1;
            }
            rest = &rest[(len + 3) & !3..];
            if pending == 0 {
                break;
            }
        }
    }
    Ok(())
}

fn count_messages(batch: &[u8]) -> usize {
    let mut count = 0;
    let mut rest = batch;
    while rest.len() >= 4 {
        let len = u32::from_ne_bytes(rest[0..4].try_into().unwrap_or_default()) as usize;
        if len < 16 || len > rest.len() {
            break;
        }
        count += 1;
        rest = &rest[(len + 3) & !3..];
    }
    count
}

#[cfg(test)]
mod tests {
    use super::matches::*;
    use super::*;

    fn table() -> Table {
        Table {
            family: Family::Inet,
            name: "feos-test".to_string(),
            chains: vec![Chain {
                name: "input".to_string(),
                kind: "filter",
                hook: Hook::Input,
                priority: 0,
                rules: vec![[iifname(CmpOp::Eq, "feos-pods"), vec![Expr::Drop]].concat()],
            }],
        }
    }

    #[test]
    fn a_batch_has_begin_messages_and_end() {
        let batch = replace_batch(&table(), 7);
        // Begin, add, delete, add, a chain, a rule, end.
        assert_eq!(count_messages(&batch), 7);
        let ty = u16::from_ne_bytes(batch[4..6].try_into().unwrap());
        assert_eq!(ty, NFNL_MSG_BATCH_BEGIN);
        assert!(batch.len().is_multiple_of(4));
    }

    #[test]
    fn interface_names_are_padded() {
        let name = ifname("eth0");
        assert_eq!(name.len(), 16);
        assert_eq!(&name[..5], b"eth0\0");
    }

    #[test]
    fn prefixes_mask_the_address() {
        let exprs = ipv4_daddr_in(Ipv4Addr::new(10, 1, 2, 3), 8);
        assert_eq!(
            exprs[1],
            Expr::Bitwise {
                mask: vec![255, 0, 0, 0]
            }
        );
        assert_eq!(
            exprs[2],
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![10, 0, 0, 0]
            }
        );
        let any = ipv4_prefix(16, CmpOp::Eq, Ipv4Addr::new(1, 2, 3, 4), 0);
        assert_eq!(any[1], Expr::Bitwise { mask: vec![0; 4] });
    }

    /// Writes and deletes a table in the kernel; needs CAP_NET_ADMIN.
    #[test]
    #[ignore]
    fn the_kernel_takes_a_table() {
        replace(&table()).unwrap();
        replace(&table()).unwrap();
        delete(Family::Inet, "feos-test").unwrap();
        delete(Family::Inet, "feos-test").unwrap();
    }
}
