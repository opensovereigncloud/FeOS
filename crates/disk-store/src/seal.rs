// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Seal (ADR 0011): turn a mutable sparse
//! `top.img` into an immutable layer.
//!
//! Change-set *rediscovery*: walk allocated extents of the top via
//! SEEK_DATA/SEEK_HOLE (O(allocated bytes), not O(logical size)), compose
//! each touched chunk from top extents + the parent chain for its holes,
//! hash it, and compare with the parent's chunk at the same offset:
//!
//!   composed == parent (data, same hash)  → absent entry (inherit)
//!   composed all-zero, parent zero/absent → absent entry (inherit)
//!   composed all-zero, parent has data    → explicit `zero` entry
//!   otherwise                             → `data` entry, chunk admitted
//!
//! Flatten-on-seal (ADR 0011): a pod's chain has the fixed shape `L0, L1?, S, top` — exactly one private layer `S`
//! above the shared prefix. Sealing with `merge_with = Some(S)` emits the
//! top's changed chunks ∪ every `S` entry the top did not override, with
//! the parent pointer at the last *shared* layer: a manifest-only merge
//! (unchanged chunks are referenced by hash, no byte is re-read), and the
//! chain never grows. Classification of top-touched chunks resolves
//! against the shared prefix; hole composition still reads through `S`.
//! With an untouched top the merge reproduces `S`'s entry table under the
//! same parent — the same layer id (idempotent reseal).
//!
//! `seal_file` seals a plain file as a FLAT layer (memory dumps, ADR
//! 0011): every non-zero chunk a data entry, every zero chunk an explicit `zero` entry, no parent.
//!
//! Commit rule (ADR 0011): chunks staged + renamed first, fsync,
//! then the manifest via atomic rename. A crash leaves only garbage
//! chunks (GC candidates) or a complete layer — never a dangling
//! manifest. The caller deletes the top only *after* this returns (and,
//! if requested, after upload).

use std::collections::HashSet;
use std::fs;
use std::io;
use std::os::unix::fs::FileExt;
use std::path::{Path, PathBuf};
use std::time::Instant;

use tracing::{debug, instrument};

use crate::cache::{fsync_dir, Cache};
use crate::chunk::{self, Hash};
use crate::error::{Error, Result};
use crate::manifest::{Entry, LayerId, Manifest};

#[derive(Debug, Clone, Default)]
pub struct SealStats {
    /// Chunks covered by at least one top extent.
    pub chunks_touched: u64,
    /// Data entries written (content differs from parent at that offset).
    pub chunks_new: u64,
    /// Of chunks_new: hash already present in the cache (pool dedup hit,
    /// no bytes written).
    pub chunks_pool_hit: u64,
    /// Explicit `zero` entries (composed all-zero over parent data).
    pub chunks_zeroed: u64,
    /// Absent entries: content identical to the resolved parent.
    pub chunks_inherited: u64,
    /// Entries copied verbatim from the merged private layer
    /// (flatten-on-seal; top did not override these chunks).
    pub chunks_merged: u64,
    pub bytes_new: u64,
    pub bytes_dedup_pool: u64,
    pub seal_seconds: f64,
}

#[derive(Debug)]
pub struct SealOutcome {
    pub layer_id: LayerId,
    pub manifest: Manifest,
    pub stats: SealStats,
    /// Local manifest path (pinned copy, `layers/<id>.manifest`).
    pub manifest_path: PathBuf,
}

/// Failure-injection hook points for the crash-consistency tests. Fired
/// between commit steps; a hook returning Err aborts the seal as if the
/// process died at that boundary.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SealStep {
    /// All new chunks admitted (staged + renamed + fsynced), cache dir fsynced.
    ChunksCommitted,
    /// Manifest written to staging (fsynced), not yet renamed.
    ManifestStaged,
}

pub type SealHook<'a> = dyn FnMut(SealStep) -> Result<()> + 'a;

/// Walk the allocated extents of `f`. Returns byte ranges [start, end),
/// clamped to `len`.
pub fn data_extents(f: &fs::File, len: u64) -> io::Result<Vec<(u64, u64)>> {
    let fd = std::os::unix::io::AsRawFd::as_raw_fd(f);
    let mut out = Vec::new();
    let mut pos = 0u64;
    while pos < len {
        let data = unsafe { libc::lseek(fd, pos as libc::off_t, libc::SEEK_DATA) };
        if data < 0 {
            let e = io::Error::last_os_error();
            if e.raw_os_error() == Some(libc::ENXIO) {
                break;
            }
            return Err(e);
        }
        let data = data as u64;
        if data >= len {
            break;
        }
        let hole = unsafe { libc::lseek(fd, data as libc::off_t, libc::SEEK_HOLE) };
        if hole < 0 {
            return Err(io::Error::last_os_error());
        }
        let end = (hole as u64).min(len);
        out.push((data, end));
        pos = end;
    }
    Ok(out)
}

/// Resolve which parent chunk (if any) provides bytes for chunk `offset`.
/// `chain` ordered L0 … direct parent.
fn resolve_parent<'a>(chain: &'a [&'a Manifest], offset: u64) -> Option<&'a Entry> {
    chain.iter().rev().find_map(|m| m.entry_at(offset))
}

/// Read the data chunk `parent` refers to (zeros for `zero`/absent).
fn read_parent_chunk(parent: Option<&Entry>, cache: &Cache, raw_len: usize) -> Result<Vec<u8>> {
    match parent {
        Some(Entry {
            kind: crate::manifest::EntryKind::Data,
            hash,
            ..
        }) => {
            let h = Hash::from_bytes(*hash);
            let p = cache.chunk_path(&h);
            // A parent chunk the budget evicted is not an error while the
            // object store still has it: the seal has to read through the
            // chain, and "reconstructible" is the cache's whole contract.
            let pf = match fs::File::open(&p) {
                Ok(f) => f,
                Err(e) if e.kind() == io::ErrorKind::NotFound => {
                    cache.ensure_local(&h, raw_len as u32)?;
                    fs::File::open(&p).map_err(Error::Io)?
                }
                Err(e) => return Err(Error::Io(e)),
            };
            cache.touch(&h);
            read_exact_at(&pf, 0, raw_len)
        }
        _ => Ok(vec![0u8; raw_len]),
    }
}

/// Overlay `extents` clamped to [off, off+raw_len) onto `base`.
fn overlay_extents(
    f: &fs::File,
    extents: &[(u64, u64)],
    off: u64,
    raw_len: usize,
    mut base: Vec<u8>,
) -> Result<Vec<u8>> {
    if base.len() < raw_len {
        base.resize(raw_len, 0);
    }
    base.truncate(raw_len);
    for &(s, e) in extents {
        let s = s.max(off);
        let e = e.min(off + raw_len as u64);
        if s >= e {
            continue;
        }
        let rel = (s - off) as usize;
        let want = (e - s) as usize;
        read_exact_into(f, s, &mut base[rel..rel + want])?;
    }
    Ok(base)
}

/// New-chunk batching: chunks batch here and flush in ~64 MiB waves
/// (batched syncfs — per-chunk fsync dominates seal time otherwise).
const FLUSH_AT: u64 = 64 << 20;

struct Pending {
    chunks: Vec<(Hash, Vec<u8>)>,
    bytes: u64,
}

impl Pending {
    fn new() -> Self {
        Pending {
            chunks: Vec::new(),
            bytes: 0,
        }
    }
    fn push(&mut self, cache: &Cache, h: Hash, buf: Vec<u8>) -> Result<()> {
        self.bytes += buf.len() as u64;
        self.chunks.push((h, buf));
        if self.bytes >= FLUSH_AT {
            self.flush(cache)?;
        }
        Ok(())
    }
    fn flush(&mut self, cache: &Cache) -> Result<()> {
        if !self.chunks.is_empty() {
            cache.admit_batch(&self.chunks)?;
            self.chunks.clear();
            self.bytes = 0;
        }
        Ok(())
    }
}

/// Commit `manifest` (chunks MUST be admitted already): staging → fsync →
/// rename → dir fsync (commit point). Returns the pinned path.
fn commit_manifest(
    state_dir: &Path,
    manifest: &Manifest,
    hook: &mut Option<&mut SealHook<'_>>,
) -> Result<PathBuf> {
    let layers_dir = state_dir.join("layers");
    fs::create_dir_all(&layers_dir)?;
    let layer_id = manifest.layer_id();
    let dst = layers_dir.join(format!("{layer_id}.manifest"));
    let tmp = state_dir
        .join("staging")
        .join(format!("{layer_id}.manifest.tmp"));
    fs::create_dir_all(state_dir.join("staging"))?;
    fs::write(&tmp, manifest.encode())?;
    fs::File::open(&tmp)?.sync_all()?;
    if let Some(h) = hook {
        h(SealStep::ManifestStaged)?;
    }
    if !dst.is_file() {
        fs::rename(&tmp, &dst)?;
    } else {
        // Content-addressed: identical layer already committed.
        fs::remove_file(&tmp)?;
    }
    fsync_dir(&layers_dir)?;
    Ok(dst)
}

/// Seal `top` against `chain`, optionally flattening the last chain
/// element (`merge_with`). `logical_size` is the guest-visible disk
/// size. `hook` is for tests (pass `None`).
///
/// Caller contract with `merge_with = Some(s)`: `chain.last()` IS `s`
/// (the pod's private layer) and shares `chunk_size`/`logical_size`;
/// the result's parent becomes the last *shared* layer
/// (`chain[..len-1]`).
#[allow(clippy::too_many_arguments)]
#[instrument(skip(top, chain, merge_with, cache, hook), fields(chunk_size))]
pub fn seal_top(
    top: &Path,
    chain: &[&Manifest],
    merge_with: Option<&Manifest>,
    cache: &Cache,
    state_dir: &Path,
    logical_size: u64,
    chunk_size: u32,
    mut hook: Option<&mut SealHook<'_>>,
) -> Result<SealOutcome> {
    let start = Instant::now();

    // Flatten setup: split the chain into the shared prefix (classification
    // parent + result parent) and the merged private layer (composition
    // still reads through it).
    let shared: &[&Manifest] = match merge_with {
        Some(s) => {
            let Some(last) = chain.last() else {
                return Err(Error::CorruptManifest(
                    "merge_with set but chain is empty".into(),
                ));
            };
            if last.layer_id() != s.layer_id() {
                return Err(Error::CorruptManifest(
                    "merge_with layer is not the chain head".into(),
                ));
            }
            if s.chunk_size != chunk_size || s.logical_size != logical_size {
                return Err(Error::CorruptManifest(
                    "merge_with chunk/logical size mismatch".into(),
                ));
            }
            &chain[..chain.len() - 1]
        }
        None => chain,
    };

    let f = fs::File::open(top)?;
    let meta = f.metadata()?;
    let top_len = meta.len().min(logical_size);
    let cs = chunk_size as u64;
    let n_chunks = chunk::chunk_count(logical_size, chunk_size);

    let extents = data_extents(&f, top_len)?;
    debug!(extents = extents.len(), top_len, "extent walk done");

    // Chunk indices covered by ≥ 1 extent.
    let mut touched: Vec<u64> = Vec::new();
    for &(s, e) in &extents {
        let first = s / cs;
        let last = (e - 1) / cs;
        for ci in first..=last.min(n_chunks - 1) {
            touched.push(ci);
        }
    }
    touched.sort_unstable();
    touched.dedup();

    let mut stats = SealStats {
        chunks_touched: touched.len() as u64,
        ..Default::default()
    };
    let parent_id = shared.last().map(|m| m.layer_id());
    let mut manifest = Manifest::new(chunk_size, logical_size, parent_id);

    let mut pending = Pending::new();

    for &ci in &touched {
        let off = ci * cs;
        let raw_len = chunk::chunk_len_at(off, logical_size, chunk_size) as usize;
        // Composition parent: the content directly below the top (full
        // chain incl. the merged layer).
        let compose_parent = resolve_parent(chain, off);
        // Classification parent: what the RESULT's readers will resolve
        // below it (shared prefix only, post-flatten).
        let class_parent = resolve_parent(shared, off);

        // Does a single extent fully cover this chunk?
        let full = extents
            .iter()
            .any(|&(s, e)| s <= off && e >= off + raw_len as u64);
        let mut buf: Vec<u8> = if full {
            read_exact_at(&f, off, raw_len)?
        } else {
            // Base = resolved parent content (or zeros), overlay top extents.
            let base = read_parent_chunk(compose_parent, cache, raw_len)?;
            overlay_extents(&f, &extents, off, raw_len, base)?
        };

        let zero = chunk::is_all_zero(&buf);
        match (zero, class_parent) {
            (
                true,
                Some(Entry {
                    kind: crate::manifest::EntryKind::Data,
                    ..
                }),
            ) => {
                // Explicit zeros over parent data → `zero` entry.
                manifest.entries.push(Entry::zero(off, raw_len as u32));
                stats.chunks_zeroed += 1;
            }
            (true, _) => {
                // All-zero over zero/absent parent → inherit (absent).
                stats.chunks_inherited += 1;
            }
            (
                false,
                Some(Entry {
                    kind: crate::manifest::EntryKind::Data,
                    hash,
                    ..
                }),
            ) if *hash == *chunk::hash(&buf).as_bytes() => {
                // Identical content → inherit (dedup against the chain).
                stats.chunks_inherited += 1;
            }
            (false, _) => {
                let h = chunk::hash(&buf);
                // comp_len: zstd the chunk now (level 3, the wire format)
                // so the manifest records the compressed length; the raw
                // bytes stay authoritative in the cache.
                let comp = chunk::compress(&buf)?;
                let comp_len = comp.len() as u32;
                let blen = buf.len() as u32;
                if cache.contains(&h) {
                    stats.chunks_pool_hit += 1;
                    stats.bytes_dedup_pool += blen as u64;
                } else {
                    stats.bytes_new += blen as u64;
                    pending.push(cache, h, std::mem::take(&mut buf))?;
                }
                manifest
                    .entries
                    .push(Entry::data(off, *h.as_bytes(), blen, comp_len));
                stats.chunks_new += 1;
            }
        }
        buf.clear();
    }

    // Flatten: copy every merged-layer entry the top did not override.
    if let Some(s) = merge_with {
        let touched_offsets: HashSet<u64> = touched.iter().map(|ci| ci * cs).collect();
        for e in &s.entries {
            if !touched_offsets.contains(&e.offset) {
                manifest.entries.push(*e);
                stats.chunks_merged += 1;
            }
        }
        manifest.entries.sort_by_key(|e| e.offset);
    }

    pending.flush(cache)?;
    if let Some(h) = &mut hook {
        h(SealStep::ChunksCommitted)?;
    }
    fsync_dir(&cache.dir())?;

    let manifest_path = commit_manifest(state_dir, &manifest, &mut hook)?;

    stats.seal_seconds = start.elapsed().as_secs_f64();
    let layer_id = manifest.layer_id();
    Ok(SealOutcome {
        layer_id,
        manifest,
        stats,
        manifest_path,
    })
}

/// Seal a plain file as a FLAT layer (no parent): the memory-dump route
/// of ADR 0011. Domain is the whole file; every non-zero chunk a data
/// entry, every zero chunk an explicit `zero` entry. Idempotent like
/// `seal_top` (same content → same layer id).
#[instrument(skip(path, cache, hook), fields(chunk_size))]
pub fn seal_file(
    path: &Path,
    cache: &Cache,
    state_dir: &Path,
    chunk_size: u32,
    mut hook: Option<&mut SealHook<'_>>,
) -> Result<SealOutcome> {
    let start = Instant::now();
    let f = fs::File::open(path)?;
    let logical_size = f.metadata()?.len();
    let cs = chunk_size as u64;
    let n_chunks = chunk::chunk_count(logical_size, chunk_size);

    let extents = data_extents(&f, logical_size)?;
    let mut stats = SealStats {
        chunks_touched: extents
            .iter()
            .map(|&(s, e)| (e / cs).min(n_chunks) - (s / cs))
            .sum::<u64>(),
        ..Default::default()
    };
    let mut manifest = Manifest::new(chunk_size, logical_size, None);
    let mut pending = Pending::new();

    for ci in 0..n_chunks {
        let off = ci * cs;
        let raw_len = chunk::chunk_len_at(off, logical_size, chunk_size) as usize;
        let mut buf = overlay_extents(&f, &extents, off, raw_len, Vec::new())?;
        if chunk::is_all_zero(&buf) {
            manifest.entries.push(Entry::zero(off, raw_len as u32));
            stats.chunks_zeroed += 1;
        } else {
            let h = chunk::hash(&buf);
            let comp = chunk::compress(&buf)?;
            let comp_len = comp.len() as u32;
            let blen = buf.len() as u32;
            if cache.contains(&h) {
                stats.chunks_pool_hit += 1;
                stats.bytes_dedup_pool += blen as u64;
            } else {
                stats.bytes_new += blen as u64;
                pending.push(cache, h, std::mem::take(&mut buf))?;
            }
            manifest
                .entries
                .push(Entry::data(off, *h.as_bytes(), blen, comp_len));
            stats.chunks_new += 1;
        }
    }

    pending.flush(cache)?;
    if let Some(h) = &mut hook {
        h(SealStep::ChunksCommitted)?;
    }
    fsync_dir(&cache.dir())?;

    let manifest_path = commit_manifest(state_dir, &manifest, &mut hook)?;

    stats.seal_seconds = start.elapsed().as_secs_f64();
    let layer_id = manifest.layer_id();
    Ok(SealOutcome {
        layer_id,
        manifest,
        stats,
        manifest_path,
    })
}

fn read_exact_at(f: &fs::File, off: u64, len: usize) -> Result<Vec<u8>> {
    let mut buf = vec![0u8; len];
    read_exact_into(f, off, &mut buf)?;
    Ok(buf)
}

fn read_exact_into(f: &fs::File, off: u64, buf: &mut [u8]) -> Result<()> {
    let mut filled = 0usize;
    while filled < buf.len() {
        let n = f.read_at(&mut buf[filled..], off + filled as u64)?;
        if n == 0 {
            return Err(Error::Io(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "short read",
            )));
        }
        filled += n;
    }
    Ok(())
}
