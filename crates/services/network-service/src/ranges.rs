// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Ranges of addresses of one family, as numbers: the arithmetic behind
//! the address sets. IPv4 addresses are numbers of 32 bits, IPv6 ones of
//! 128. A list of ranges is normal when it is sorted, and its ranges
//! neither overlap nor touch: the form an nftables interval set takes.

/// The first and the last address of a range.
pub(crate) type Range = (u128, u128);

/// The range of the prefix of `address` that is `len` bits long.
pub(crate) fn prefix(address: u128, len: u8) -> Range {
    let host = if len >= 128 { 0 } else { u128::MAX >> len };
    (address & !host, address | host)
}

/// The ranges, sorted and merged where they overlap or touch. Ranges that
/// end before they start are dropped.
pub(crate) fn normalize(mut ranges: Vec<Range>) -> Vec<Range> {
    ranges.retain(|(start, end)| start <= end);
    ranges.sort_unstable();
    let mut merged: Vec<Range> = Vec::with_capacity(ranges.len());
    for (start, end) in ranges {
        match merged.last_mut() {
            Some(last) if last.1 == u128::MAX || start <= last.1 + 1 => last.1 = last.1.max(end),
            _ => merged.push((start, end)),
        }
    }
    merged
}

/// The addresses of normal `ranges` that are not in normal `holes`.
pub(crate) fn subtract(ranges: &[Range], holes: &[Range]) -> Vec<Range> {
    let mut rest = Vec::with_capacity(ranges.len());
    for &(first, end) in ranges {
        let mut start = first;
        let mut covered = false;
        for &(hole_start, hole_end) in holes {
            if hole_end < start {
                continue;
            }
            if hole_start > end {
                break;
            }
            if hole_start > start {
                rest.push((start, hole_start - 1));
            }
            if hole_end >= end {
                covered = true;
                break;
            }
            start = hole_end + 1;
        }
        if !covered {
            rest.push((start, end));
        }
    }
    rest
}

/// The addresses in both ranges.
pub(crate) fn intersect(a: Range, b: Range) -> Option<Range> {
    let (start, end) = (a.0.max(b.0), a.1.min(b.1));
    (start <= end).then_some((start, end))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::Ipv6Addr;

    fn a(s: &str) -> u128 {
        u128::from(s.parse::<Ipv6Addr>().unwrap())
    }

    #[test]
    fn prefixes_become_ranges() {
        assert_eq!(prefix(0, 0), (0, u128::MAX));
        assert_eq!(
            prefix(a("2001:db8::"), 32),
            (a("2001:db8::"), a("2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"))
        );
        assert_eq!(prefix(a("fd00::5"), 128), (a("fd00::5"), a("fd00::5")));
        assert_eq!(prefix(u128::MAX, 128), (u128::MAX, u128::MAX));
        assert_eq!(prefix(u128::MAX, 1), (1 << 127, u128::MAX));
    }

    #[test]
    fn ranges_merge_where_they_overlap_or_touch() {
        assert_eq!(
            normalize(vec![(5, 9), (1, 2), (3, 4), (20, 30), (25, 26)]),
            vec![(1, 9), (20, 30)]
        );
        assert_eq!(normalize(vec![(1, 2), (4, 5)]), vec![(1, 2), (4, 5)]);
        assert_eq!(normalize(vec![(3, 2), (7, 7)]), vec![(7, 7)]);
        assert_eq!(normalize(vec![]), vec![]);
        // The top of the space: nothing comes after it, nothing overflows.
        assert_eq!(
            normalize(vec![
                (u128::MAX, u128::MAX),
                (u128::MAX - 1, u128::MAX - 1),
                (0, u128::MAX)
            ]),
            vec![(0, u128::MAX)]
        );
        assert_eq!(
            normalize(vec![(10, u128::MAX), (u128::MAX, u128::MAX)]),
            vec![(10, u128::MAX)]
        );
    }

    #[test]
    fn holes_are_cut_out() {
        // Holes in the middle, and touching either edge.
        assert_eq!(
            subtract(&[(0, 100)], &[(10, 20), (30, 40)]),
            vec![(0, 9), (21, 29), (41, 100)]
        );
        assert_eq!(subtract(&[(0, 100)], &[(0, 10), (90, 100)]), vec![(11, 89)]);
        assert_eq!(subtract(&[(10, 20)], &[(0, 15)]), vec![(16, 20)]);
        assert_eq!(subtract(&[(10, 20)], &[(15, 30)]), vec![(10, 14)]);
        assert_eq!(subtract(&[(10, 20)], &[(10, 20)]), vec![]);
        assert_eq!(subtract(&[(10, 20)], &[(0, 5), (25, 30)]), vec![(10, 20)]);
        // A hole across two ranges.
        assert_eq!(
            subtract(&[(0, 10), (20, 30)], &[(5, 25)]),
            vec![(0, 4), (26, 30)]
        );
        // The whole space, holes at its edges.
        assert_eq!(
            subtract(&[(0, u128::MAX)], &[(0, 0), (u128::MAX, u128::MAX)]),
            vec![(1, u128::MAX - 1)]
        );
        assert_eq!(subtract(&[(0, u128::MAX)], &[(0, u128::MAX)]), vec![]);
        assert_eq!(subtract(&[(0, u128::MAX)], &[]), vec![(0, u128::MAX)]);
    }

    #[test]
    fn exceptions_of_a_block() {
        // ::/0 without 2001:db8::/32 and the top /64.
        let rest = subtract(
            &[prefix(0, 0)],
            &normalize(vec![prefix(a("2001:db8::"), 32), prefix(u128::MAX, 64)]),
        );
        assert_eq!(
            rest,
            vec![
                (0, a("2001:db7:ffff:ffff:ffff:ffff:ffff:ffff")),
                (
                    a("2001:db9::"),
                    a("ffff:ffff:ffff:fffe:ffff:ffff:ffff:ffff")
                ),
            ]
        );
    }

    #[test]
    fn ranges_intersect() {
        assert_eq!(intersect((0, 10), (5, 20)), Some((5, 10)));
        assert_eq!(intersect((0, 10), (10, 20)), Some((10, 10)));
        assert_eq!(intersect((0, 10), (11, 20)), None);
    }
}
