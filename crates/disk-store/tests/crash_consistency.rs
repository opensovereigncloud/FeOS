// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Crash consistency: kill the seal at every step boundary,
//! restart, `fsck` reports either a complete layer or garbage chunks —
//! never a dangling manifest.

mod common;

use disk_store::error::Error;
use disk_store::gc;
use disk_store::seal::{seal_top, SealStep};

use common::*;

const L: u64 = 1024 * 1024;

fn build_scenario(
    dir: &tempfile::TempDir,
) -> (
    std::path::PathBuf,
    disk_store::cache::Cache,
    disk_store::Manifest,
    std::path::PathBuf,
) {
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let base = sparse_file(
        dir.path(),
        "base.raw",
        L,
        &[(0, rand_bytes(&mut rng(1), CHUNK as usize))],
    );
    let l0 = seal_top(&base, &[], None, &cache, &state, L, CHUNK, None).unwrap();
    let top = sparse_file(
        dir.path(),
        "top.img",
        L,
        &[
            (4096, rand_bytes(&mut rng(2), 8192)),
            (3 * CHUNK as u64, rand_bytes(&mut rng(3), CHUNK as usize)),
        ],
    );
    (state, cache, l0.manifest, top)
}

#[test]
fn abort_at_every_step_boundary() {
    for step in [SealStep::ChunksCommitted, SealStep::ManifestStaged] {
        let dir = tempfile::tempdir().unwrap();
        let (state, cache, l0, top) = build_scenario(&dir);

        let mut hook = |s: SealStep| -> disk_store::Result<()> {
            if s == step {
                Err(Error::SealAborted("injected"))
            } else {
                Ok(())
            }
        };
        let r = seal_top(
            &top,
            &[&l0],
            None,
            &cache,
            &state,
            L,
            CHUNK,
            Some(&mut hook),
        );
        assert!(matches!(r, Err(Error::SealAborted(_))), "step {step:?}");

        // fsck: no dangling manifest, ever. Garbage chunks are fine.
        let layers = load_layers(&state);
        let all: Vec<&disk_store::Manifest> = layers.iter().map(|(_, m)| m).collect();
        let report = gc::fsck(&state, &layers, None).unwrap();
        assert!(
            report.dangling_layers.is_empty(),
            "step {step:?}: dangling {:?}",
            report.dangling_layers
        );
        // L0 is complete.
        assert_eq!(report.complete_layers.len(), layers.len());
        drop(all);

        match step {
            SealStep::ChunksCommitted => {
                // New chunks admitted but the manifest never landed →
                // they show up as garbage for the GC.
                assert!(
                    !report.garbage_chunks.is_empty(),
                    "crash after chunk commit leaves garbage"
                );
            }
            SealStep::ManifestStaged => {
                // Manifest staged but not renamed: layer invisible.
                assert!(!report.staging_leftovers.is_empty());
            }
        }

        // Restart: the same seal succeeds and converges to a clean state.
        let out = seal_top(&top, &[&l0], None, &cache, &state, L, CHUNK, None).unwrap();
        let layers = load_layers(&state);
        let report = gc::fsck(&state, &layers, None).unwrap();
        assert!(report.dangling_layers.is_empty());
        // After successful re-seal the chain materializes correctly
        // (content addressing makes the retry land on the same hashes).
        let view =
            disk_store::materialize::materialize_bytes(&[&l0, &out.manifest], &cache).unwrap();
        let mut exp = vec![0u8; L as usize];
        let parent = rand_bytes(&mut rng(1), CHUNK as usize);
        exp[..CHUNK as usize].copy_from_slice(&parent);
        let w1 = rand_bytes(&mut rng(2), 8192);
        exp[4096..4096 + 8192].copy_from_slice(&w1);
        let w2 = rand_bytes(&mut rng(3), CHUNK as usize);
        exp[3 * CHUNK as usize..4 * CHUNK as usize].copy_from_slice(&w2);
        assert_eq!(view, exp);
    }
}

#[test]
fn reopen_cleans_staging_leftovers() {
    let dir = tempfile::tempdir().unwrap();
    let (state, cache, l0, top) = build_scenario(&dir);
    let mut hook = |s: SealStep| -> disk_store::Result<()> {
        if s == SealStep::ManifestStaged {
            Err(Error::SealAborted("injected"))
        } else {
            Ok(())
        }
    };
    let _ = seal_top(
        &top,
        &[&l0],
        None,
        &cache,
        &state,
        L,
        CHUNK,
        Some(&mut hook),
    );
    assert!(!state.join("staging").read_dir().unwrap().next().is_none());
    drop(cache);
    // Reopen = restart; staging is cleaned.
    let _cache2 = open_cache(&state);
    assert!(state.join("staging").read_dir().unwrap().next().is_none());
}

#[test]
fn gc_removes_only_unreferenced() {
    let dir = tempfile::tempdir().unwrap();
    let (state, cache, l0, top) = build_scenario(&dir);
    let l1 = seal_top(&top, &[&l0], None, &cache, &state, L, CHUNK, None).unwrap();

    // Plant an unreferenced chunk.
    let junk = rand_bytes(&mut rng(77), 8192);
    let junk_h = disk_store::chunk::hash(&junk);
    cache.admit(&junk_h, &junk).unwrap();

    let manifests: Vec<&disk_store::Manifest> = vec![&l0, &l1.manifest];
    let before = cache.stats().unwrap().bytes;
    let freed = gc::gc_local(&cache, &manifests).unwrap();
    assert_eq!(freed, 8192, "only the planted garbage is collected");
    assert!(!cache.contains(&junk_h));
    assert_eq!(cache.stats().unwrap().bytes, before - 8192);
    let view = disk_store::materialize::materialize_bytes(&manifests, &cache).unwrap();
    assert_eq!(view.len() as u64, L);
}
