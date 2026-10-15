// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Materialize a layer chain into a raw file (tests: byte equality;
//! ops: image inspection), and unpack a compressed image into one.

use std::fs;
use std::io::{self, Read};
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

/// The granularity at which `unpack_zstd` leaves zeroes out: a
/// filesystem block.
const UNPACK_BLOCK: usize = 4096;

/// Write a zstd-compressed raw image to `out` as a sparse file: blocks
/// of zeroes stay holes, so that an image of many GiB that holds a few
/// MiB, such as a blank filesystem, costs what it holds. Returns the
/// image's size.
pub fn unpack_zstd(src: &Path, out: &Path) -> Result<u64> {
    let mut decoder = zstd::stream::Decoder::new(fs::File::open(src)?)?;
    let f = fs::File::create(out)?;
    let mut buf = vec![0u8; 1 << 20];
    let mut off = 0u64;
    loop {
        // Fill the buffer, so that blocks line up with the image's.
        let mut filled = 0;
        while filled < buf.len() {
            match decoder.read(&mut buf[filled..]) {
                Ok(0) => break,
                Ok(n) => filled += n,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Err(Error::Io(e)),
            }
        }
        for (i, block) in buf[..filled].chunks(UNPACK_BLOCK).enumerate() {
            if !crate::chunk::is_all_zero(block) {
                f.write_all_at(block, off + (i * UNPACK_BLOCK) as u64)?;
            }
        }
        off += filled as u64;
        if filled < buf.len() {
            break;
        }
    }
    f.set_len(off)?;
    f.sync_all()?;
    Ok(off)
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::MetadataExt;

    #[test]
    fn a_compressed_image_unpacks_sparse() {
        let dir = tempfile::tempdir().unwrap();
        // 8 MiB with two blocks of data, and a short tail.
        let mut image = vec![0u8; 8 << 20];
        image[4096..8192].fill(1);
        image[(5 << 20) + 17] = 2;
        image.extend_from_slice(&[0, 3, 0]);
        let src = dir.path().join("image.zst");
        fs::write(&src, zstd::bulk::compress(&image, 3).unwrap()).unwrap();

        let out = dir.path().join("image.raw");
        assert_eq!(unpack_zstd(&src, &out).unwrap(), image.len() as u64);
        assert_eq!(fs::read(&out).unwrap(), image);
        // The two blocks and the tail hold data; the rest are holes.
        assert!(fs::metadata(&out).unwrap().blocks() * 512 <= 4 * 4096);
    }
}
