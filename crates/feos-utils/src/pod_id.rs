// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Pod IDs (ADR 0019): what one is, a new one, and the names the node
//! gives a pod by it.
//!
//! A pod ID is 11 characters of lowercase Crockford base32, the first of
//! them a letter: `^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$`. Its TAP
//! device is `pod-<ID>`, 15 characters, and its guest's MAC address `06:00`
//! and the first four bytes of the SHA-256 of the ID.
//!
//! Pods made before have UUIDs, and keep them until they are deleted: a
//! microVM's snapshot names its TAP device. A UUID is a pod ID as well, in
//! any form [`Uuid::parse_str`] reads, as before; its TAP device is `pod-`
//! and the first 8 hex digits of the ID, and its MAC address is made of the
//! same digits. Nothing makes new pods with UUIDs.

use rand::Rng;
use sha2::{Digest, Sha256};
use std::fmt;
use std::str::FromStr;
use uuid::Uuid;

/// The characters of a pod ID: Crockford's base32 in lower case, without
/// `i`, `l`, `o` and `u`.
pub const ALPHABET: &[u8; 32] = b"0123456789abcdefghjkmnpqrstvwxyz";
/// The length of a pod ID.
pub const LEN: usize = 11;
/// The letters of [`ALPHABET`], which a pod ID starts with.
const LETTERS: &[u8] = ALPHABET.split_at(10).1;

/// A pod's ID: a short one, or the UUID of a pod made before short IDs.
#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum PodId {
    Short([u8; LEN]),
    Uuid(Uuid),
}

/// A string that is not a pod ID.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InvalidPodId(pub String);

impl fmt::Display for InvalidPodId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "pod ID {:?} is neither 11 characters of lowercase Crockford base32, a letter first, nor a UUID",
            self.0
        )
    }
}

impl std::error::Error for InvalidPodId {}

impl PodId {
    /// A new short ID, from a cryptographic random source.
    pub fn mint() -> Self {
        let mut rng = rand::rng();
        let mut id = [0u8; LEN];
        id[0] = LETTERS[rng.random_range(0..LETTERS.len())];
        for byte in &mut id[1..] {
            *byte = ALPHABET[rng.random_range(0..ALPHABET.len())];
        }
        Self::Short(id)
    }

    /// Reads a short ID or a UUID.
    pub fn parse(id: &str) -> Result<Self, InvalidPodId> {
        if is_short(id) {
            let mut short = [0u8; LEN];
            short.copy_from_slice(id.as_bytes());
            return Ok(Self::Short(short));
        }
        Uuid::parse_str(id)
            .map(Self::Uuid)
            .map_err(|_| InvalidPodId(id.to_string()))
    }

    /// Whether this is the UUID of a pod made before short IDs.
    pub fn is_uuid(&self) -> bool {
        matches!(self, Self::Uuid(_))
    }
}

impl FromStr for PodId {
    type Err = InvalidPodId;

    fn from_str(id: &str) -> Result<Self, Self::Err> {
        Self::parse(id)
    }
}

/// A short ID as it is, a UUID hyphenated in lower case.
impl fmt::Display for PodId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            // Only ASCII of the alphabet gets in.
            Self::Short(id) => f.write_str(std::str::from_utf8(id).map_err(|_| fmt::Error)?),
            Self::Uuid(id) => id.hyphenated().fmt(f),
        }
    }
}

impl fmt::Debug for PodId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "PodId({self})")
    }
}

/// A new short pod ID.
pub fn mint() -> String {
    PodId::mint().to_string()
}

/// Whether `id` is a short pod ID.
pub fn is_short(id: &str) -> bool {
    let bytes = id.as_bytes();
    bytes.len() == LEN && LETTERS.contains(&bytes[0]) && bytes.iter().all(|b| ALPHABET.contains(b))
}

/// Whether `id` is a pod ID: a short one or a UUID.
pub fn is_valid(id: &str) -> bool {
    PodId::parse(id).is_ok()
}

/// The pod's TAP device: `pod-` and a short ID; for any other ID, `pod-`
/// and its first 8 characters other than "-", the first 8 hex digits of a
/// UUID, as before short IDs.
pub fn tap_name(pod_id: &str) -> String {
    if is_short(pod_id) {
        format!("pod-{pod_id}")
    } else {
        format!("pod-{}", legacy_hex(pod_id))
    }
}

/// The guest's MAC address, locally administered and stable for the pod:
/// `06:00` and the first four bytes of the SHA-256 of a short ID; for any
/// other ID, of the 8 hex digits of its TAP device's name, as before short
/// IDs.
pub fn mac(pod_id: &str) -> [u8; 6] {
    if is_short(pod_id) {
        let hash = Sha256::digest(pod_id.as_bytes());
        return [0x06, 0x00, hash[0], hash[1], hash[2], hash[3]];
    }
    let hex = legacy_hex(pod_id);
    let byte = |i: usize| {
        hex.get(2 * i..2 * i + 2)
            .and_then(|byte| u8::from_str_radix(byte, 16).ok())
            .unwrap_or(0)
    };
    [0x06, 0x00, byte(0), byte(1), byte(2), byte(3)]
}

fn legacy_hex(pod_id: &str) -> String {
    pod_id.chars().filter(|c| *c != '-').take(8).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    const UUID: &str = "8d99e180-1694-4d94-a42e-097769b89510";

    fn mac_string(mac: [u8; 6]) -> String {
        mac.iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<Vec<_>>()
            .join(":")
    }

    /// The regular expression of ADR 0019, by hand.
    fn matches_format(id: &str) -> bool {
        let first =
            |c: char| matches!(c, 'a'..='h' | 'j' | 'k' | 'm' | 'n' | 'p'..='t' | 'v'..='z');
        let rest = |c: char| c.is_ascii_digit() || first(c);
        id.len() == 11 && id.chars().next().is_some_and(first) && id.chars().skip(1).all(rest)
    }

    #[test]
    fn the_alphabet_is_crockfords_in_lower_case() {
        assert_eq!(ALPHABET.len(), 32);
        assert_eq!(ALPHABET.iter().collect::<HashSet<_>>().len(), 32);
        for c in *b"ilou" {
            assert!(!ALPHABET.contains(&c));
        }
        assert_eq!(LETTERS.len(), 22);
        assert!(LETTERS.iter().all(u8::is_ascii_lowercase));
    }

    #[test]
    fn short_ids_are_checked() {
        for id in ["a0000000000", "zzzzzzzzzzz", "k7m2q9x4c1b", "h0123456789"] {
            assert!(is_short(id), "{id}");
            assert!(matches_format(id), "{id}");
            assert_eq!(PodId::parse(id).unwrap().to_string(), id);
            assert!(!PodId::parse(id).unwrap().is_uuid());
        }
        for id in [
            "",
            "a000000000",   // 10
            "a00000000000", // 12
            "00000000000",  // a digit first
            "A0000000000",  // upper case
            "aBcdefghjkm",
            "a000000000i",
            "a000000000l",
            "a000000000o",
            "a000000000u",
            "a00000-0000",
            "a0000000/00",
            "../etc/abcd",
            "pod-1234567",
        ] {
            assert!(!is_short(id), "{id}");
            assert!(!is_valid(id), "{id}");
        }
    }

    #[test]
    fn uuids_are_pod_ids_as_before() {
        let id = PodId::parse(UUID).unwrap();
        assert!(id.is_uuid());
        assert_eq!(id.to_string(), UUID);
        // Uuid::parse_str's forms, as before short IDs; written hyphenated
        // in lower case.
        assert_eq!(PodId::parse(&UUID.to_uppercase()).unwrap(), id);
        assert_eq!(PodId::parse(&UUID.replace('-', "")).unwrap(), id);
        assert_eq!(
            PodId::parse(&UUID.to_uppercase()).unwrap().to_string(),
            UUID
        );
        assert!(PodId::parse("8d99e180-1694-4d94-a42e-097769b8951").is_err());
        assert!(PodId::parse("not-a-uuid").is_err());
    }

    #[test]
    fn minted_ids_have_the_format_and_differ() {
        let ids: Vec<String> = (0..10_000).map(|_| mint()).collect();
        for id in &ids {
            assert!(matches_format(id), "{id}");
            assert!(is_short(id), "{id}");
        }
        assert_eq!(ids.iter().collect::<HashSet<_>>().len(), ids.len());
        // Every letter comes first and every character later, about as
        // often as the others: 10,000 IDs give each letter some 450 first
        // places and each character some 3,100 later ones.
        let mut first = [0usize; 256];
        let mut rest = [0usize; 256];
        for id in &ids {
            let bytes = id.as_bytes();
            first[bytes[0] as usize] += 1;
            for &b in &bytes[1..] {
                rest[b as usize] += 1;
            }
        }
        for &c in LETTERS {
            assert!((250..700).contains(&first[c as usize]), "{}", c as char);
        }
        for &c in ALPHABET {
            assert!((2_500..3_800).contains(&rest[c as usize]), "{}", c as char);
        }
        // No ID starts with a digit.
        assert_eq!(
            first[b'0' as usize..=b'9' as usize].iter().sum::<usize>(),
            0
        );
    }

    #[test]
    fn uuid_pods_keep_their_names_and_macs() {
        assert_eq!(tap_name(UUID), "pod-8d99e180");
        assert_eq!(mac_string(mac(UUID)), "06:00:8d:99:e1:80");
        // As before: the characters as they are written.
        assert_eq!(tap_name(&UUID.to_uppercase()), "pod-8D99E180");
        assert_eq!(mac_string(mac(&UUID.to_uppercase())), "06:00:8d:99:e1:80");
        assert_eq!(
            tap_name("11111111-2222-3333-4444-555555555555"),
            "pod-11111111"
        );
    }

    #[test]
    fn short_ids_name_their_taps_and_macs() {
        let id = "k7m2q9x4c1b";
        assert_eq!(tap_name(id), "pod-k7m2q9x4c1b");
        assert_eq!(tap_name(id).len(), 15);
        assert!(tap_name(id).len() < libc::IFNAMSIZ);
        let hash = Sha256::digest(id.as_bytes());
        assert_eq!(mac(id), [0x06, 0x00, hash[0], hash[1], hash[2], hash[3]]);
        // Locally administered, unicast.
        assert_eq!(mac(id)[0] & 0b11, 0b10);
        assert_ne!(mac(id), mac("k7m2q9x4c1c"));
    }

    #[test]
    fn short_and_uuid_taps_never_meet() {
        // 15 characters against 12: no short ID's TAP is a UUID's.
        for _ in 0..1_000 {
            assert_ne!(tap_name(&mint()).len(), tap_name(UUID).len());
        }
    }
}
