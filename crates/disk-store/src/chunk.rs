// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Chunk primitives: hashing, zero detection, zstd wire format.
//!
//! The local cache holds *raw* chunks (zero-copy read path, ADR 0011);
//! zstd is the wire/S3 format only (level 3).

use std::path::PathBuf;

use crate::error::{Error, Result};

/// Disk chunk size: **64 KiB**, measured: with 2 % scattered 4 KiB writes
/// the seal's write amplification is 14× at 64 KiB and 50× at 2 MiB, and
/// a cold miss fetches 64 KiB instead of 2 MiB.
pub const DEFAULT_DISK_CHUNK_SIZE: u32 = 64 * 1024;

/// Memory-layer chunk size: **256 KiB**, measured: the
/// second-dump deltas after one short exec were 14 % at 256 KiB vs 35 %
/// at 2 MiB (one dirty 4 KiB page reships the whole chunk); manifest
/// entries 2048 vs 256 per 512 MiB dump — cheap.
pub const DEFAULT_MEM_CHUNK_SIZE: u32 = 256 * 1024;

/// zstd compression level for the S3/wire format.
pub const ZSTD_LEVEL: i32 = 3;

pub use blake3::Hash;

pub fn hash(data: &[u8]) -> Hash {
    blake3::hash(data)
}

/// 8-byte-block zero scan (hot path in seal).
pub fn is_all_zero(data: &[u8]) -> bool {
    // Fast path: word-wise.
    let (head, words, tail) = unsafe { data.align_to::<u64>() };
    head.iter().all(|&b| b == 0) && words.iter().all(|&w| w == 0) && tail.iter().all(|&b| b == 0)
}

pub fn compress(data: &[u8]) -> Result<Vec<u8>> {
    zstd::bulk::compress(data, ZSTD_LEVEL).map_err(Error::Io)
}

/// zstd decompress a frame produced by [`compress`].
pub fn decompress(data: &[u8]) -> Result<Vec<u8>> {
    let mut dec = zstd::stream::Decoder::new(data).map_err(Error::Io)?;
    let mut out = Vec::new();
    std::io::Read::read_to_end(&mut dec, &mut out).map_err(Error::Io)?;
    Ok(out)
}

/// Cache layout: `cache/<hh>/<full-hex>` where hh = first two hex chars.
pub fn cache_rel_path(h: &Hash) -> PathBuf {
    let hex = h.to_hex().to_string();
    PathBuf::from(&hex[0..2]).join(hex)
}

/// Number of chunks covering `logical_size` bytes at `chunk_size`.
pub fn chunk_count(logical_size: u64, chunk_size: u32) -> u64 {
    logical_size.div_ceil(chunk_size as u64)
}

/// Raw length of the chunk at `offset` (trailing chunk may be short).
pub fn chunk_len_at(offset: u64, logical_size: u64, chunk_size: u32) -> u32 {
    let cs = chunk_size as u64;
    let remaining = logical_size.saturating_sub(offset);
    remaining.min(cs) as u32
}
