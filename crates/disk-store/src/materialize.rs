// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Materialize a layer chain into a raw file (tests: byte equality;
//! ops: image inspection).

use std::fs;
use std::io;
use std::os::unix::fs::FileExt;
use std::path::Path;

use tracing::instrument;

use crate::cache::Cache;
use crate::chunk::Hash;
use crate::error::{Error, Result};
use crate::manifest::{EntryKind, Manifest};

/// Write the composed view of `chain` (ordered L0 … head) to `out` as a
/// sparse raw file (zero chunks are skipped).
#[instrument(skip(chain, cache))]
pub fn materialize(chain: &[&Manifest], cache: &Cache, out: &Path) -> Result<()> {
    let Some(head) = chain.last() else {
        return Err(Error::CorruptManifest("empty chain".into()));
    };
    let cs = head.chunk_size as u64;
    let logical = head.logical_size;
    let f = fs::File::create(out)?;

    for m in chain {
        if m.chunk_size != head.chunk_size || m.logical_size != logical {
            return Err(Error::CorruptManifest(
                "chain inconsistency: chunk size / logical size mismatch".into(),
            ));
        }
    }

    let n = crate::chunk::chunk_count(logical, head.chunk_size);
    for ci in 0..n {
        let off = ci * cs;
        let Some(e) = Manifest::resolve(head, &chain[..chain.len() - 1], off) else {
            continue; // absent everywhere → zero
        };
        if e.kind == EntryKind::Zero {
            continue;
        }
        let h = Hash::from_bytes(e.hash);
        let p = cache.chunk_path(&h);
        let data = fs::read(&p).map_err(|err| match err.kind() {
            io::ErrorKind::NotFound => Error::ChunkMissing(h.to_hex().to_string()),
            _ => Error::Io(err),
        })?;
        cache.touch(&h);
        if data.len() != e.raw_len as usize {
            return Err(Error::ChunkCorrupt(format!(
                "{}: len {} != raw_len {}",
                h.to_hex(),
                data.len(),
                e.raw_len
            )));
        }
        f.write_all_at(&data, off)?;
    }
    f.set_len(logical)?;
    Ok(())
}

/// Composed view as bytes (test helper for small disks).
pub fn materialize_bytes(chain: &[&Manifest], cache: &Cache) -> Result<Vec<u8>> {
    let tmp = std::env::temp_dir().join(format!(
        "store-materialize-{}-{}.raw",
        std::process::id(),
        blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
    ));
    let r = materialize(chain, cache, &tmp);
    let bytes = if r.is_ok() {
        fs::read(&tmp)
    } else {
        Ok(Vec::new())
    };
    let _ = fs::remove_file(&tmp);
    r?;
    Ok(bytes?)
}
