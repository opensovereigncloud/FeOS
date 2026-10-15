// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Prefixes of either family, as rules and `--deny-prefix` write them.

use crate::ranges::{self, Range};
use std::fmt;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::str::FromStr;

/// An address family.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Family {
    V4,
    V6,
}

/// An IPv4 or IPv6 prefix: an address, whose bits beyond the length are
/// zero, and the length.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum Prefix {
    V4(Ipv4Addr, u8),
    V6(Ipv6Addr, u8),
}

impl Prefix {
    pub fn length(&self) -> u8 {
        match *self {
            Prefix::V4(_, len) | Prefix::V6(_, len) => len,
        }
    }

    pub fn is_ipv4(&self) -> bool {
        matches!(self, Prefix::V4(..))
    }

    pub fn family(&self) -> Family {
        match self {
            Prefix::V4(..) => Family::V4,
            Prefix::V6(..) => Family::V6,
        }
    }

    /// Whether `other` lies inside this prefix. A prefix of the other
    /// family never does.
    pub fn contains(&self, other: &Prefix) -> bool {
        match (*self, *other) {
            (Prefix::V4(address, len), Prefix::V4(inner, inner_len)) => {
                inner_len >= len && mask32(u32::from(inner), len) == u32::from(address)
            }
            (Prefix::V6(address, len), Prefix::V6(inner, inner_len)) => {
                inner_len >= len && mask128(u128::from(inner), len) == u128::from(address)
            }
            _ => false,
        }
    }

    /// The prefix's addresses as a range of numbers of its family: of 32
    /// bits for IPv4, of 128 for IPv6. A length beyond the address's
    /// counts as the address's.
    pub(crate) fn range(&self) -> Range {
        match *self {
            Prefix::V4(address, len) => {
                // As the top 32 bits of 128.
                let (start, end) = ranges::prefix(u128::from(u32::from(address)) << 96, len);
                (start >> 96, end >> 96)
            }
            Prefix::V6(address, len) => ranges::prefix(u128::from(address), len),
        }
    }
}

impl FromStr for Prefix {
    type Err = String;

    /// `address/length`, of either family. Bits beyond the length must be
    /// zero: `10.1.0.0/8` is refused rather than taken as `10.0.0.0/8`.
    fn from_str(s: &str) -> Result<Self, String> {
        let (address, len) = s
            .split_once('/')
            .ok_or_else(|| format!("{s:?} is not a prefix: address/length"))?;
        let len = len
            .parse::<u8>()
            .ok()
            .filter(|_| len.bytes().all(|b| b.is_ascii_digit()))
            .ok_or_else(|| format!("{s:?} has no valid length"))?;
        let address = address
            .parse::<IpAddr>()
            .map_err(|_| format!("{s:?} has no valid address"))?;
        let prefix = match address {
            IpAddr::V4(address) if len <= 32 => {
                let masked = Ipv4Addr::from(mask32(u32::from(address), len));
                (masked == address)
                    .then_some(Prefix::V4(address, len))
                    .ok_or_else(|| {
                        format!("{s:?} has bits set beyond its length: {masked}/{len}?")
                    })?
            }
            IpAddr::V6(address) if len <= 128 => {
                let masked = Ipv6Addr::from(mask128(u128::from(address), len));
                (masked == address)
                    .then_some(Prefix::V6(address, len))
                    .ok_or_else(|| {
                        format!("{s:?} has bits set beyond its length: {masked}/{len}?")
                    })?
            }
            _ => return Err(format!("{s:?} is longer than its address")),
        };
        Ok(prefix)
    }
}

impl fmt::Display for Prefix {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Prefix::V4(address, len) => write!(f, "{address}/{len}"),
            Prefix::V6(address, len) => write!(f, "{address}/{len}"),
        }
    }
}

fn mask32(address: u32, len: u8) -> u32 {
    match len {
        0 => 0,
        1..=31 => address & (u32::MAX << (32 - len)),
        _ => address,
    }
}

fn mask128(address: u128, len: u8) -> u128 {
    match len {
        0 => 0,
        1..=127 => address & (u128::MAX << (128 - len)),
        _ => address,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn prefix(s: &str) -> Prefix {
        s.parse().unwrap()
    }

    #[test]
    fn prefixes_parse_and_print() {
        for s in [
            "::/0",
            "0.0.0.0/0",
            "10.0.0.0/8",
            "2001:db8::/32",
            "fd00::5/128",
        ] {
            assert_eq!(prefix(s).to_string(), s);
        }
        assert_eq!(
            prefix("10.0.0.0/8"),
            Prefix::V4(Ipv4Addr::new(10, 0, 0, 0), 8)
        );
        assert!(prefix("1.2.3.4/32").is_ipv4());
        assert_eq!(prefix("1.2.3.4/32").family(), Family::V4);
        assert_eq!(prefix("::/0").family(), Family::V6);
        assert_eq!(prefix("2001:DB8::/32").to_string(), "2001:db8::/32");
    }

    #[test]
    fn bad_prefixes_are_refused() {
        for s in [
            "",
            "10.0.0.0",
            "10.0.0.0/",
            "10.0.0.0/33",
            "::/129",
            "10.1.0.0/8",
            "2001:db8::1/64",
            "10.0.0.0/+8",
            "10.0.0.0/-1",
            "example.com/8",
            "::ffff:10.0.0.0/300",
        ] {
            assert!(s.parse::<Prefix>().is_err(), "{s} parsed");
        }
        let error = "10.1.0.0/8".parse::<Prefix>().unwrap_err();
        assert!(error.contains("10.0.0.0/8"), "{error}");

        // Built by hand, too long: they are their address, and nothing panics.
        let address = Ipv4Addr::new(10, 0, 0, 1);
        let (start, end) = Prefix::V4(address, 255).range();
        assert_eq!((start, end), (0x0a00_0001, 0x0a00_0001));
        assert_eq!(Prefix::V6(Ipv6Addr::LOCALHOST, 200).range(), (1, 1));
        assert!(Prefix::V4(address, 200).contains(&Prefix::V4(address, 255)));
    }

    #[test]
    fn a_prefix_contains_its_subprefixes_of_its_family() {
        assert!(prefix("10.0.0.0/8").contains(&prefix("10.1.0.0/16")));
        assert!(prefix("10.0.0.0/8").contains(&prefix("10.0.0.0/8")));
        assert!(!prefix("10.0.0.0/8").contains(&prefix("11.0.0.0/16")));
        assert!(!prefix("10.0.0.0/16").contains(&prefix("10.0.0.0/8")));
        assert!(prefix("::/0").contains(&prefix("2001:db8::/32")));
        assert!(prefix("0.0.0.0/0").contains(&prefix("255.255.255.255/32")));
        assert!(!prefix("::/0").contains(&prefix("10.0.0.0/8")));
        assert!(!prefix("0.0.0.0/0").contains(&prefix("::/128")));
    }

    #[test]
    fn prefixes_are_ranges_of_their_family() {
        let v4 = |s: &str| prefix(s).range();
        assert_eq!(v4("0.0.0.0/0"), (0, u128::from(u32::MAX)));
        assert_eq!(v4("10.0.0.0/8"), (0x0a00_0000, 0x0aff_ffff));
        assert_eq!(v4("255.255.255.255/32"), (0xffff_ffff, 0xffff_ffff));
        let (start, end) = prefix("2001:db8::/32").range();
        assert_eq!(
            (Ipv6Addr::from(start), Ipv6Addr::from(end)),
            (
                "2001:db8::".parse().unwrap(),
                "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff".parse().unwrap()
            )
        );
        assert_eq!(prefix("::/96").range(), v4("0.0.0.0/0"), "the numbers");
    }
}
