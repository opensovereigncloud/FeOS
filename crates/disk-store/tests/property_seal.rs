// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Property test: for random sparse tops over random parents,
//! `materialize(seal(top, parent))` is byte-identical to the composed
//! view; dedup counters match an independent count.

mod common;

use disk_store::chunk;
use disk_store::materialize::materialize_bytes;
use disk_store::seal::seal_top;
use proptest::prelude::*;

use common::*;

/// A generated write op: offset, length, all-zero flag.
#[derive(Debug, Clone)]
struct GenOp {
    off: u64,
    len: usize,
    zero: bool,
}

fn gen_ops(max_len: usize, max_n: usize) -> impl Strategy<Value = Vec<GenOp>> {
    prop::collection::vec(
        (0..LOGICAL, 1usize..max_len, prop::bool::weighted(0.15)),
        0..max_n,
    )
    .prop_map(|v| {
        v.into_iter()
            .map(|(off, len, zero)| GenOp {
                off,
                len: len.min((LOGICAL - off) as usize),
                zero,
            })
            .collect()
    })
}

fn realize(r: &mut rand::rngs::SmallRng, ops: &[GenOp]) -> Vec<Op> {
    ops.iter()
        .map(|g| {
            let data = if g.zero {
                vec![0u8; g.len]
            } else {
                rand_bytes(r, g.len)
            };
            (g.off, data)
        })
        .collect()
}

/// Chunks of `view` touched by the top file's *extents* (block-aligned
/// data ranges — ground truth is the file's content, not the writes),
/// and the independently computed per-chunk rule outcome.
#[derive(Default)]
struct Expect {
    touched: u64,
    new: u64,
    zeroed: u64,
    inherited: u64,
}

fn independent_count(
    view_after: &[u8],
    parent: &disk_store::Manifest,
    parent_view: &[u8],
    top_extents: &[(u64, u64)],
) -> Expect {
    let cs = CHUNK as u64;
    let mut touched = std::collections::BTreeSet::new();
    for &(s, e) in top_extents {
        if s >= e {
            continue;
        }
        let first = s / cs;
        let last = (e - 1) / cs;
        for ci in first..=last {
            touched.insert(ci);
        }
    }
    let mut e = Expect {
        touched: touched.len() as u64,
        ..Expect::default()
    };
    for ci in touched {
        let off = (ci * cs) as usize;
        let len = chunk::chunk_len_at(off as u64, LOGICAL, CHUNK) as usize;
        let composed = &view_after[off..off + len];
        let parent_bytes = &parent_view[off..off + len];
        let parent_entry = parent.entry_at(off as u64);
        let parent_has_data = matches!(
            parent_entry,
            Some(en) if en.kind == disk_store::EntryKind::Data
        );
        if chunk::is_all_zero(composed) {
            if parent_has_data {
                e.zeroed += 1;
            } else {
                e.inherited += 1;
            }
        } else if parent_has_data && composed == parent_bytes {
            e.inherited += 1;
        } else {
            e.new += 1;
        }
    }
    e
}

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 32,
        ..ProptestConfig::default()
    })]

    #[test]
    fn seal_materialize_roundtrip(
        seed in any::<u64>(),
        base_ops in gen_ops(32 * 1024, 24),
        top_ops in gen_ops(CHUNK as usize * 2, 24),
    ) {
        let mut r = rng(seed);
        let dir = tempfile::tempdir().unwrap();
        let state = dir.path().join("state");
        std::fs::create_dir_all(&state).unwrap();
        let cache = open_cache(&state);

        // Parent (L0): sparse base image.
        let base = realize(&mut r, &base_ops);
        let base_path = sparse_file(dir.path(), "base.raw", LOGICAL, &base);
        let mut base_view = vec![0u8; LOGICAL as usize];
        apply(&mut base_view, &base);

        let l0 = seal_top(&base_path, &[], None, &cache, &state, LOGICAL, CHUNK, None).unwrap();
        // L0 sanity: materialize == composed base view.
        let l0_view = materialize_bytes(&[&l0.manifest], &cache).unwrap();
        if l0_view != base_view {
            let i = l0_view.iter().zip(&base_view).position(|(a, b)| a != b).unwrap();
            panic!("l0 view mismatch at byte {i} (chunk {}) seal={} exp={}", i / CHUNK as usize, l0_view[i], base_view[i]);
        }

        // Top: sparse mutations over the chain. Ground truth is the top
        // *file's content*: block-allocated-but-unwritten bytes are zeros
        // that override the parent (SEEK_DATA is block granular).
        let top = realize(&mut r, &top_ops);
        let top_path = sparse_file(dir.path(), "top.img", LOGICAL, &top);
        let top_file = std::fs::File::open(&top_path).unwrap();
        let ext = disk_store::seal::data_extents(&top_file, LOGICAL).unwrap();
        let top_bytes = std::fs::read(&top_path).unwrap();
        let mut expected = base_view.clone();
        for (s, e) in &ext {
            expected[*s as usize..*e as usize].copy_from_slice(&top_bytes[*s as usize..*e as usize]);
        }

        let l1 = seal_top(&top_path, &[&l0.manifest], None, &cache, &state, LOGICAL, CHUNK, None).unwrap();

        // The core property.
        let view = materialize_bytes(&[&l0.manifest, &l1.manifest], &cache).unwrap();
        if view != expected {
            let i = view.iter().zip(&expected).position(|(a, b)| a != b).unwrap();
            panic!("chain view mismatch at byte {i} (chunk {}) got={} want={}", i / CHUNK as usize, view[i], expected[i]);
        }

        // Independent dedup-counter check.
        let e = independent_count(&expected, &l0.manifest, &base_view, &ext);
        prop_assert_eq!(l1.stats.chunks_touched, e.touched);
        prop_assert_eq!(l1.stats.chunks_new, e.new);
        prop_assert_eq!(l1.stats.chunks_zeroed, e.zeroed);
        prop_assert_eq!(l1.stats.chunks_inherited, e.inherited);
        prop_assert_eq!(
            l1.manifest.entries.len() as u64,
            e.new + e.zeroed,
            "entries = new data + zero overrides (inherited are absent)"
        );
    }
}
