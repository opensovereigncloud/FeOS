// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! GC helpers and Fsck of the local store. The object store's sweeper is
//! sweep.rs.
//!
//! Truth: manifests reference chunks; local chunk files not referenced by
//! any considered manifest are garbage (e.g. leftovers of a crashed
//! seal). A manifest whose chunks are missing everywhere is *dangling* —
//! the commit rule makes that impossible, so fsck flags it loudly.

use std::collections::HashSet;
use std::fs;
use std::path::{Path, PathBuf};

use crate::cache::{scan_chunks, Cache};
use crate::chunk::Hash;
use crate::error::Result;
use crate::manifest::{EntryKind, Manifest};

/// All chunks referenced by the given manifests.
pub fn referenced_chunks<'a>(manifests: impl IntoIterator<Item = &'a Manifest>) -> HashSet<Hash> {
    let mut out = HashSet::new();
    for m in manifests {
        for e in &m.entries {
            if e.kind == EntryKind::Data {
                out.insert(Hash::from_bytes(e.hash));
            }
        }
    }
    out
}

/// All local chunk files under the cache dir.
pub fn list_local_chunks(cache_dir: &Path) -> Result<Vec<(Hash, u64)>> {
    Ok(scan_chunks(cache_dir)?
        .into_iter()
        .map(|(h, len, _)| (h, len))
        .collect())
}

/// Local chunks referenced by none of `manifests` — safe to unlink.
pub fn unreferenced(cache_dir: &Path, manifests: &[&Manifest]) -> Result<Vec<(Hash, u64)>> {
    let want = referenced_chunks(manifests.iter().copied());
    Ok(list_local_chunks(cache_dir)?
        .into_iter()
        .filter(|(h, _)| !want.contains(h))
        .collect())
}

/// Local GC: unlink local chunks not referenced by `manifests`. Returns
/// freed bytes.
pub fn gc_local(cache: &Cache, manifests: &[&Manifest]) -> Result<u64> {
    let garbage = unreferenced(&cache.dir(), manifests)?;
    let mut freed = 0;
    for (h, len) in garbage {
        let p = cache.chunk_path(&h);
        match fs::remove_file(&p) {
            Ok(()) => {
                cache.forget(&h);
                freed += len;
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                cache.forget(&h);
            }
            Err(e) => return Err(e.into()),
        }
    }
    Ok(freed)
}

#[derive(Debug, Default)]
pub struct FsckReport {
    /// Manifests whose data chunks are all present locally (or in S3 if
    /// `s3_has` was given).
    pub complete_layers: Vec<String>,
    /// Manifests referencing chunks available nowhere → dangling. The
    /// commit rule forbids this; any entry here is a bug/corruption.
    pub dangling_layers: Vec<DanglingLayer>,
    /// Local chunk files referenced by no manifest (crash leftovers).
    pub garbage_chunks: Vec<String>,
    pub garbage_bytes: u64,
    /// Leftover staging files (interrupted admissions/commits).
    pub staging_leftovers: Vec<PathBuf>,
}

#[derive(Debug)]
pub struct DanglingLayer {
    pub layer_id: String,
    pub missing_local: u64,
    pub missing_total: u64,
}

/// Check manifests ↔ local chunks (↔ S3 when `s3_has` is supplied).
/// `layers` = (layer_id, manifest) pairs; manifests not present locally
/// are not checked (callers load what they care about).
pub fn fsck(
    state_dir: &Path,
    layers: &[(String, Manifest)],
    s3_has: Option<&dyn Fn(&Hash) -> bool>,
) -> Result<FsckReport> {
    let cache_dir = state_dir.join("cache");
    let mut report = FsckReport::default();

    for (id, m) in layers {
        let mut missing_local = 0u64;
        let mut missing_total = 0u64;
        for e in &m.entries {
            if e.kind != EntryKind::Data {
                continue;
            }
            let h = Hash::from_bytes(e.hash);
            let local = cache_dir.join(crate::chunk::cache_rel_path(&h)).is_file();
            if !local {
                missing_local += 1;
                let remote = s3_has.map(|f| f(&h)).unwrap_or(false);
                if !remote {
                    missing_total += 1;
                }
            }
        }
        if missing_total > 0 {
            report.dangling_layers.push(DanglingLayer {
                layer_id: id.clone(),
                missing_local,
                missing_total,
            });
        } else {
            report.complete_layers.push(id.clone());
        }
    }

    let manifests: Vec<&Manifest> = layers.iter().map(|(_, m)| m).collect();
    for (h, len) in unreferenced(&cache_dir, &manifests)? {
        report.garbage_chunks.push(h.to_hex().to_string());
        report.garbage_bytes += len;
    }

    let staging = state_dir.join("staging");
    if staging.is_dir() {
        for e in fs::read_dir(&staging)? {
            let e = e?;
            if e.file_type()?.is_file() {
                report.staging_leftovers.push(e.path());
            }
        }
    }
    Ok(report)
}
