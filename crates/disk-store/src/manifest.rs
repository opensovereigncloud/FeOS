// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Binary layer manifest (version 1).
//!
//! The entry table must stay compact at ~10^5 entries (256 KiB chunks on an
//! 8 GiB disk = 32k entries/layer), so the on-disk form is a fixed-width
//! little-endian binary table:
//!
//! ```text
//! header (64 bytes):
//!   magic        [u8; 8]  "DSKDMAN\0"
//!   version      u32      = 1
//!   flags        u32      reserved, 0
//!   chunk_size   u32
//!   entry_count  u32
//!   logical_size u64
//!   parent       [u8;32]  parent layer id, all-zero = none (L0)
//! entries, sorted by offset, 52 bytes each:
//!   offset       u64      chunk-aligned
//!   kind         u8       0 = data, 1 = zero
//!   reserved     [u8; 3]
//!   hash         [u8;32]  blake3 of the raw chunk; zero for kind=zero
//!   raw_len      u32      decompressed length (== chunk_len_at(offset))
//!   comp_len     u32      zstd length at seal time (informational; S3
//!                         object size is authoritative — chunk objects
//!                         are fetched whole, never range-read)
//! ```
//!
//! Zero semantics (ADR 0011): an absent entry means
//! *inherit from parent*; for L0 (no parent) absent means zero. A `zero`
//! entry explicitly overrides parent data.
//!
//! `layer_id` = blake3 over the serialized entry table: content-addressed,
//! resealing identical content yields the same layer.
//!
//! The pod's head (`head.json`: ordered chain, mem/vmstate refs)
//! stays JSON — see head.rs.

use std::fmt;

use crate::error::{Error, Result};

pub const MAGIC: &[u8; 8] = b"DSKDMAN\0";
pub const VERSION: u32 = 1;
pub const HEADER_SIZE: usize = 64;
pub const ENTRY_SIZE: usize = 52;

#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct LayerId(pub [u8; 32]);

impl LayerId {
    pub fn from_bytes(b: [u8; 32]) -> Self {
        LayerId(b)
    }
    pub fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }
    pub fn hex(&self) -> String {
        blake3::Hash::from(self.0).to_hex().to_string()
    }
    pub fn from_hex(s: &str) -> Result<Self> {
        let h: blake3::Hash = s
            .parse()
            .map_err(|_| Error::CorruptManifest(format!("bad layer id hex: {s}")))?;
        Ok(LayerId(*h.as_bytes()))
    }
}

impl fmt::Display for LayerId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.hex())
    }
}

impl fmt::Debug for LayerId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "LayerId({:.12}…)", self.hex())
    }
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum EntryKind {
    Data = 0,
    Zero = 1,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Entry {
    pub offset: u64,
    pub kind: EntryKind,
    pub hash: [u8; 32],
    pub raw_len: u32,
    pub comp_len: u32,
}

impl Entry {
    pub fn data(offset: u64, hash: [u8; 32], raw_len: u32, comp_len: u32) -> Self {
        Entry {
            offset,
            kind: EntryKind::Data,
            hash,
            raw_len,
            comp_len,
        }
    }
    pub fn zero(offset: u64, raw_len: u32) -> Self {
        Entry {
            offset,
            kind: EntryKind::Zero,
            hash: [0u8; 32],
            raw_len,
            comp_len: 0,
        }
    }
}

#[derive(Clone, Debug)]
pub struct Manifest {
    pub chunk_size: u32,
    pub logical_size: u64,
    /// None for L0.
    pub parent: Option<LayerId>,
    /// Sorted by offset.
    pub entries: Vec<Entry>,
}

impl Manifest {
    pub fn new(chunk_size: u32, logical_size: u64, parent: Option<LayerId>) -> Self {
        Manifest {
            chunk_size,
            logical_size,
            parent,
            entries: Vec::new(),
        }
    }

    /// Logical bytes this layer actually carries: data entries only.
    /// Zero entries describe absence and cost nothing.
    pub fn data_bytes(&self) -> u64 {
        self.entries
            .iter()
            .filter(|e| e.kind == EntryKind::Data)
            .map(|e| e.raw_len as u64)
            .sum()
    }

    /// Content-addressed layer id: blake3 over the header + serialized
    /// entry table. The parent pointer is part of the identity — the
    /// same entry table under different parents MUST NOT collide (a
    /// discard of a zeroed range looks identical in the entry table but
    /// belongs to a different chain; collision broke `materialize`'s
    /// contiguity check in lab_discard_no_resurrection).
    pub fn layer_id(&self) -> LayerId {
        let mut hasher = blake3::Hasher::new();
        hasher.update(&self.logical_size.to_le_bytes());
        hasher.update(&self.chunk_size.to_le_bytes());
        hasher.update(&self.parent.unwrap_or(LayerId([0u8; 32])).0);
        for e in &self.entries {
            hasher.update(&encode_entry(e));
        }
        LayerId(*hasher.finalize().as_bytes())
    }

    /// Exact-offset lookup (entries are chunk-aligned and sorted).
    pub fn entry_at(&self, offset: u64) -> Option<&Entry> {
        self.entries
            .binary_search_by_key(&offset, |e| e.offset)
            .ok()
            .map(|i| &self.entries[i])
    }

    /// Resolve the entry covering `offset` walking the chain newest-first.
    /// `chain` is ordered L0 … parent; `self` is the head layer.
    pub fn resolve<'a>(
        head: &'a Manifest,
        chain: &'a [&'a Manifest],
        offset: u64,
    ) -> Option<&'a Entry> {
        if let Some(e) = head.entry_at(offset) {
            return Some(e);
        }
        chain.iter().rev().find_map(|m| m.entry_at(offset))
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(HEADER_SIZE + ENTRY_SIZE * self.entries.len());
        out.extend_from_slice(MAGIC);
        out.extend_from_slice(&VERSION.to_le_bytes());
        out.extend_from_slice(&0u32.to_le_bytes()); // flags
        out.extend_from_slice(&self.chunk_size.to_le_bytes());
        out.extend_from_slice(&(self.entries.len() as u32).to_le_bytes());
        out.extend_from_slice(&self.logical_size.to_le_bytes());
        out.extend_from_slice(&self.parent.map(|p| p.0).unwrap_or([0u8; 32]));
        debug_assert_eq!(out.len(), HEADER_SIZE);
        for e in &self.entries {
            out.extend_from_slice(&encode_entry(e));
        }
        out
    }

    pub fn decode(buf: &[u8]) -> Result<Manifest> {
        if buf.len() < HEADER_SIZE {
            return Err(Error::CorruptManifest("short header".into()));
        }
        if &buf[0..8] != MAGIC {
            return Err(Error::CorruptManifest("bad magic".into()));
        }
        let version = u32_at(buf, 8);
        if version != VERSION {
            return Err(Error::CorruptManifest(format!("version {version}")));
        }
        let chunk_size = u32_at(buf, 16);
        let entry_count = u32_at(buf, 20) as usize;
        let logical_size = u64_at(buf, 24);
        let parent_raw: [u8; 32] = buf[32..64]
            .try_into()
            .map_err(|_| Error::CorruptManifest("short parent".into()))?;
        let parent = if parent_raw == [0u8; 32] {
            None
        } else {
            Some(LayerId(parent_raw))
        };
        let need = HEADER_SIZE + entry_count * ENTRY_SIZE;
        if buf.len() != need {
            return Err(Error::CorruptManifest(format!(
                "size {} != expected {need}",
                buf.len()
            )));
        }
        let mut entries = Vec::with_capacity(entry_count);
        let mut prev_off: Option<u64> = None;
        for i in 0..entry_count {
            let e = decode_entry(&buf[HEADER_SIZE + i * ENTRY_SIZE..][..ENTRY_SIZE])?;
            if let Some(p) = prev_off
                && e.offset <= p
            {
                return Err(Error::CorruptManifest("entries not sorted".into()));
            }
            prev_off = Some(e.offset);
            entries.push(e);
        }
        Ok(Manifest {
            chunk_size,
            logical_size,
            parent,
            entries,
        })
    }
}

fn encode_entry(e: &Entry) -> [u8; ENTRY_SIZE] {
    let mut b = [0u8; ENTRY_SIZE];
    b[0..8].copy_from_slice(&e.offset.to_le_bytes());
    b[8] = e.kind as u8;
    // b[9..12] reserved = 0
    b[12..44].copy_from_slice(&e.hash);
    b[44..48].copy_from_slice(&e.raw_len.to_le_bytes());
    b[48..52].copy_from_slice(&e.comp_len.to_le_bytes());
    b
}

fn decode_entry(b: &[u8]) -> Result<Entry> {
    let offset = u64::from_le_bytes(b[0..8].try_into().map_err(|_| corrupt("offset"))?);
    let kind = match b[8] {
        0 => EntryKind::Data,
        1 => EntryKind::Zero,
        k => return Err(Error::CorruptManifest(format!("bad kind {k}"))),
    };
    let hash: [u8; 32] = b[12..44].try_into().map_err(|_| corrupt("hash"))?;
    let raw_len = u32::from_le_bytes(b[44..48].try_into().map_err(|_| corrupt("raw_len"))?);
    let comp_len = u32::from_le_bytes(b[48..52].try_into().map_err(|_| corrupt("comp_len"))?);
    Ok(Entry {
        offset,
        kind,
        hash,
        raw_len,
        comp_len,
    })
}

fn corrupt(what: &str) -> Error {
    Error::CorruptManifest(format!("bad {what}"))
}

fn u32_at(b: &[u8], off: usize) -> u32 {
    // Header length was validated by the caller before indexing.
    match b[off..off + 4].try_into() {
        Ok(a) => u32::from_le_bytes(a),
        Err(_) => 0, // unreachable: header size checked in decode
    }
}

fn u64_at(b: &[u8], off: usize) -> u64 {
    match b[off..off + 8].try_into() {
        Ok(a) => u64::from_le_bytes(a),
        Err(_) => 0, // unreachable: header size checked in decode
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let mut m = Manifest::new(256 * 1024, 8 << 30, None);
        m.entries.push(Entry::data(0, [7u8; 32], 256 * 1024, 100));
        m.entries.push(Entry::zero(512 * 1024, 256 * 1024));
        let id = m.layer_id();
        let buf = m.encode();
        let d = Manifest::decode(&buf).unwrap();
        assert_eq!(d.layer_id(), id);
        assert_eq!(d.chunk_size, 256 * 1024);
        assert_eq!(d.parent, None);
        assert_eq!(d.entries[1].kind, EntryKind::Zero);
        assert!(d.entry_at(512 * 1024).is_some());
        assert!(d.entry_at(256 * 1024).is_none());
    }

    #[test]
    fn layer_id_depends_on_parent() {
        let mut a = Manifest::new(64 * 1024, 1 << 20, None);
        a.entries.push(Entry::zero(0, 64 * 1024));
        let mut b = Manifest::new(64 * 1024, 1 << 20, Some(LayerId([9u8; 32])));
        b.entries.push(Entry::zero(0, 64 * 1024));
        assert_ne!(a.layer_id(), b.layer_id(), "parent is part of the id");
    }
}
