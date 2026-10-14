// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Flatten-on-seal (ADR 0011) and flat memory
//! layers (`seal_file`, ADR 0011).
//!
//! Fixed chain shape: `L0, L1?, S, top` — one private layer above the
//! shared prefix. Sealing merges the top *into* S: emitted entries = the
//! top's changed chunks ∪ every S entry the top did not override, parent
//! = last shared layer. The chain never grows; an untouched top reseals
//! to the same layer id.

mod common;

use disk_store::materialize::materialize_bytes;
use disk_store::seal::{seal_file, seal_top};

use common::*;

const L: u64 = 1024 * 1024; // 1 MiB = 16 chunks at 64 KiB

fn seal_l0(
    state: &std::path::Path,
    cache: &disk_store::cache::Cache,
    dir: &std::path::Path,
) -> disk_store::seal::SealOutcome {
    // chunk 0 and chunk 1 carry base data.
    let d0 = rand_bytes(&mut rng(11), CHUNK as usize);
    let d1 = rand_bytes(&mut rng(12), CHUNK as usize);
    let base = sparse_file(dir, "base.raw", L, &[(0, d0), (CHUNK as u64, d1)]);
    seal_top(&base, &[], None, cache, state, L, CHUNK, None).unwrap()
}

#[test]
fn flatten_merges_private_layer_and_moves_parent() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0(&state, &cache, dir.path());

    // First seal (no private layer yet): writes at chunk 2.
    let w1 = rand_bytes(&mut rng(21), CHUNK as usize);
    let top1 = sparse_file(dir.path(), "top1.img", L, &[(2 * CHUNK as u64, w1.clone())]);
    let s1 = seal_top(&top1, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    assert_eq!(s1.manifest.parent, Some(l0.layer_id));

    // Second seal (flatten): overwrite chunk 2, add chunk 3.
    let w2 = rand_bytes(&mut rng(22), CHUNK as usize);
    let w3 = rand_bytes(&mut rng(23), CHUNK as usize);
    let top2 = sparse_file(
        dir.path(),
        "top2.img",
        L,
        &[
            (2 * CHUNK as u64, w2.clone()),
            (3 * CHUNK as u64, w3.clone()),
        ],
    );
    let s2 = seal_top(
        &top2,
        &[&l0.manifest, &s1.manifest],
        Some(&s1.manifest),
        &cache,
        &state,
        L,
        CHUNK,
        None,
    )
    .unwrap();

    assert_eq!(
        s2.manifest.parent,
        Some(l0.layer_id),
        "flattened S' parents at the last SHARED layer"
    );
    let e2 = s2
        .manifest
        .entry_at(2 * CHUNK as u64)
        .expect("chunk 2 entry");
    assert_eq!(
        *disk_store::chunk::hash(&w2).as_bytes(),
        e2.hash,
        "top wins over S"
    );
    assert!(
        s2.manifest.entry_at(3 * CHUNK as u64).is_some(),
        "chunk 3 present"
    );

    // Ground truth: L0 + w2 + w3.
    let mut expected = vec![0u8; L as usize];
    apply(
        &mut expected,
        &[
            (
                0,
                materialize_bytes(&[&l0.manifest], &cache).unwrap()[..CHUNK as usize].to_vec(),
            ),
            (
                CHUNK as u64,
                materialize_bytes(&[&l0.manifest], &cache).unwrap()
                    [CHUNK as usize..2 * CHUNK as usize]
                    .to_vec(),
            ),
            (2 * CHUNK as u64, w2.clone()),
            (3 * CHUNK as u64, w3.clone()),
        ],
    );
    let view = materialize_bytes(&[&l0.manifest, &s2.manifest], &cache).unwrap();
    assert_eq!(
        view, expected,
        "flattened chain == fresh chain on the truth"
    );

    // Chain shape: the pod owns exactly one private layer — sealing
    // must never need S1 to read the disk again.
    let view = materialize_bytes(&[&l0.manifest, &s2.manifest], &cache).unwrap();
    assert_eq!(&view[2 * CHUNK as usize..3 * CHUNK as usize], &w2[..]);
}

#[test]
fn flatten_classification_resolves_against_shared() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0(&state, &cache, dir.path());
    let l0_view = materialize_bytes(&[&l0.manifest], &cache).unwrap();

    // S1: zeros over chunk 0 (L0 data) → explicit `zero` entry; data at chunk 5.
    let w5 = rand_bytes(&mut rng(31), CHUNK as usize);
    let top1 = sparse_file(
        dir.path(),
        "top1.img",
        L,
        &[
            (0, vec![0u8; CHUNK as usize]),
            (5 * CHUNK as u64, w5.clone()),
        ],
    );
    let s1 = seal_top(&top1, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    assert_eq!(
        s1.manifest.entry_at(0).unwrap().kind,
        disk_store::EntryKind::Zero,
        "S1: zeros over L0 data → zero entry"
    );

    // top2: restores chunk 0 to L0's original content. Flatten classifies
    // against the SHARED prefix ([L0]): content equal → inherit (no
    // entry), NOT "copy S1's zero entry".
    let top2 = sparse_file(
        dir.path(),
        "top2.img",
        L,
        &[(0, l0_view[..CHUNK as usize].to_vec())],
    );
    let s2 = seal_top(
        &top2,
        &[&l0.manifest, &s1.manifest],
        Some(&s1.manifest),
        &cache,
        &state,
        L,
        CHUNK,
        None,
    )
    .unwrap();
    assert!(
        s2.manifest.entry_at(0).is_none(),
        "chunk 0 back to shared content → inherit (S1's zero override dropped)"
    );
    // Chunk 5 untouched by top2 → S1's data entry copied verbatim.
    let e5 = s2
        .manifest
        .entry_at(5 * CHUNK as u64)
        .expect("chunk 5 merged");
    assert_eq!(
        e5.hash,
        s1.manifest.entry_at(5 * CHUNK as u64).unwrap().hash
    );
    assert_eq!(s2.stats.chunks_merged, 1, "exactly one merged entry");

    let mut expected = l0_view.clone();
    apply(&mut expected, &[(5 * CHUNK as u64, w5.clone())]);
    let view = materialize_bytes(&[&l0.manifest, &s2.manifest], &cache).unwrap();
    assert_eq!(view, expected);
}

#[test]
fn flatten_empty_top_is_idempotent() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0(&state, &cache, dir.path());

    let w2 = rand_bytes(&mut rng(41), CHUNK as usize);
    let top1 = sparse_file(dir.path(), "top1.img", L, &[(2 * CHUNK as u64, w2)]);
    let s1 = seal_top(&top1, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();

    // No writes since the open (empty top): the merge reproduces S1 —
    // same entry table under the same parent → same layer id.
    let top2 = sparse_file(dir.path(), "top2.img", L, &[]);
    let s2 = seal_top(
        &top2,
        &[&l0.manifest, &s1.manifest],
        Some(&s1.manifest),
        &cache,
        &state,
        L,
        CHUNK,
        None,
    )
    .unwrap();
    assert_eq!(s2.layer_id, s1.layer_id, "idempotent reseal");
    let layers = load_layers(&state);
    assert_eq!(layers.len(), 2, "only L0 and S on disk");
}

#[test]
fn flatten_rejects_wrong_merge_head() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0(&state, &cache, dir.path());
    let w = rand_bytes(&mut rng(71), CHUNK as usize);
    let t = sparse_file(dir.path(), "other.img", L, &[(0, w)]);
    let other = seal_top(&t, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    let top = sparse_file(dir.path(), "top.img", L, &[]);
    // merge_with a layer that is NOT the chain head → error.
    let r = seal_top(
        &top,
        &[&l0.manifest],
        Some(&other.manifest),
        &cache,
        &state,
        L,
        CHUNK,
        None,
    );
    assert!(r.is_err(), "merge_with must be the chain head");
}

#[test]
fn seal_file_flat_layer_shape() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);

    // 4 chunks: data, allocated zeros, data, hole (implicit zeros).
    let d0 = rand_bytes(&mut rng(51), CHUNK as usize);
    let d2 = rand_bytes(&mut rng(52), CHUNK as usize);
    let f = sparse_file(
        dir.path(),
        "mem.dump",
        4 * CHUNK as u64,
        &[
            (0, d0.clone()),
            (CHUNK as u64, vec![0u8; CHUNK as usize]),
            (2 * CHUNK as u64, d2.clone()),
        ],
    );
    let out = seal_file(&f, &cache, &state, CHUNK, None).unwrap();

    assert_eq!(out.manifest.parent, None, "flat layer");
    assert_eq!(
        out.manifest.entries.len(),
        4,
        "every chunk classified (data ∪ zero), none absent"
    );
    assert_eq!(
        out.manifest.entry_at(0).unwrap().kind,
        disk_store::EntryKind::Data
    );
    assert_eq!(
        out.manifest.entry_at(CHUNK as u64).unwrap().kind,
        disk_store::EntryKind::Zero
    );
    assert_eq!(
        out.manifest.entry_at(2 * CHUNK as u64).unwrap().kind,
        disk_store::EntryKind::Data
    );
    assert_eq!(
        out.manifest.entry_at(3 * CHUNK as u64).unwrap().kind,
        disk_store::EntryKind::Zero,
        "holes are zeros for a flat layer"
    );

    let view = materialize_bytes(&[&out.manifest], &cache).unwrap();
    let mut expected = vec![0u8; 4 * CHUNK as usize];
    apply(&mut expected, &[(0, d0), (2 * CHUNK as u64, d2)]);
    assert_eq!(view, expected, "materialize == original file");

    // Idempotent: same content → same layer id.
    let out2 = seal_file(&f, &cache, &state, CHUNK, None).unwrap();
    assert_eq!(out2.layer_id, out.layer_id);
    assert_eq!(out2.stats.chunks_pool_hit, 2, "second seal fully dedups");
}

#[test]
fn seal_file_trailing_short_chunk() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);

    let d = rand_bytes(&mut rng(61), CHUNK as usize + 123);
    let f = sparse_file(
        dir.path(),
        "mem.dump",
        CHUNK as u64 + 123,
        &[(0, d.clone())],
    );
    let out = seal_file(&f, &cache, &state, CHUNK, None).unwrap();
    let view = materialize_bytes(&[&out.manifest], &cache).unwrap();
    assert_eq!(view, d, "trailing short chunk survives");
}
