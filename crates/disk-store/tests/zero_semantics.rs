// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Zero semantics (ADR 0011):
//! - parent has data, top writes zeros        → sealed layer: `zero` entry
//! - parent has data, top untouched           → absent (inherit)
//! - L0 all-zero chunk                        → absent
//! - data resurrection: a *punched hole* in the top is detected as
//!   inherit-from-parent — the trap that is why the daemon never punches.

mod common;

use disk_store::materialize::materialize_bytes;
use disk_store::seal::seal_top;

use common::*;

const L: u64 = 1024 * 1024; // 1 MiB

fn seal_l0_with_data(
    state: &std::path::Path,
    cache: &disk_store::cache::Cache,
    dir: &std::path::Path,
) -> disk_store::seal::SealOutcome {
    // chunk 0: data, chunk 1: hole (absent → zero view)
    let data = rand_bytes(&mut rng(7), CHUNK as usize);
    let base = sparse_file(dir, "base.raw", L, &[(0, data)]);
    seal_top(&base, &[], None, cache, state, L, CHUNK, None).unwrap()
}

#[test]
fn zeros_written_over_parent_data_become_zero_entry() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0_with_data(&state, &cache, dir.path());

    // Top: write zeros over chunk 0.
    let top = sparse_file(dir.path(), "top.img", L, &[(0, vec![0u8; CHUNK as usize])]);
    let l1 = seal_top(&top, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();

    let e = l1.manifest.entry_at(0).expect("chunk 0 must have an entry");
    assert_eq!(
        e.kind,
        disk_store::EntryKind::Zero,
        "zeros over parent data → explicit zero entry"
    );

    // Composed view: zeros.
    let view = materialize_bytes(&[&l0.manifest, &l1.manifest], &cache).unwrap();
    assert!(disk_store::chunk::is_all_zero(&view[..CHUNK as usize]));
}

#[test]
fn untouched_parent_data_is_inherited_absent() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0_with_data(&state, &cache, dir.path());

    // Top writes only into chunk 2, leaving chunk 0 untouched.
    let w = rand_bytes(&mut rng(8), 4096);
    let top = sparse_file(dir.path(), "top.img", L, &[(2 * CHUNK as u64, w.clone())]);
    let l1 = seal_top(&top, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();

    assert!(
        l1.manifest.entry_at(0).is_none(),
        "untouched parent data → absent"
    );
    let view = materialize_bytes(&[&l0.manifest, &l1.manifest], &cache).unwrap();
    let l0view = materialize_bytes(&[&l0.manifest], &cache).unwrap();
    assert_eq!(
        &view[..CHUNK as usize],
        &l0view[..CHUNK as usize],
        "parent bytes visible"
    );
    assert_eq!(&view[2 * CHUNK as usize..2 * CHUNK as usize + 4096], &w[..]);
}

#[test]
fn l0_all_zero_chunk_is_absent() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);

    // Write zeros (allocated extent, all-zero content) into chunk 0 of an
    // otherwise empty base.
    let base = sparse_file(dir.path(), "base.raw", L, &[(0, vec![0u8; 8192])]);
    let l0 = seal_top(&base, &[], None, &cache, &state, L, CHUNK, None).unwrap();
    assert!(
        l0.manifest.entry_at(0).is_none(),
        "L0 all-zero chunk → absent entry (absent == zero for L0)"
    );
    assert_eq!(l0.stats.chunks_inherited, 1);
}

#[test]
fn punched_hole_resurrects_parent_data_the_trap() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0_with_data(&state, &cache, dir.path());

    // Top: write fresh data over chunk 0, then PUNCH it.
    let top = sparse_file(
        dir.path(),
        "top.img",
        L,
        &[(0, rand_bytes(&mut rng(9), CHUNK as usize))],
    );
    let f = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(&top)
        .unwrap();
    let fd = std::os::unix::io::AsRawFd::as_raw_fd(&f);
    let rc = unsafe {
        libc::fallocate(
            fd,
            libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE,
            0,
            CHUNK as i64,
        )
    };
    assert_eq!(rc, 0);

    let l1 = seal_top(&top, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    assert!(
        l1.manifest.entry_at(0).is_none(),
        "hole = 'not written here, read the parent' → inherit"
    );
    let view = materialize_bytes(&[&l0.manifest, &l1.manifest], &cache).unwrap();
    let l0view = materialize_bytes(&[&l0.manifest], &cache).unwrap();
    assert_eq!(
        &view[..CHUNK as usize],
        &l0view[..CHUNK as usize],
        "RESURRECTION: punched top makes old parent data visible again — \
         this is why diskd implements discard/write-zeroes as explicit \
         zero writes and never punches top.img (ADR 0011)"
    );
}

#[test]
fn double_seal_is_idempotent() {
    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);
    let l0 = seal_l0_with_data(&state, &cache, dir.path());

    let ops = common::sparse_file(
        dir.path(),
        "top.img",
        L,
        &[(4096, rand_bytes(&mut rng(3), 8192))],
    );
    let a = seal_top(&ops, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    let b = seal_top(&ops, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    assert_eq!(a.layer_id, b.layer_id, "same content → same layer id");
    assert!(a.manifest_path.is_file());
    let layers = load_layers(&state);
    assert_eq!(layers.len(), 2, "exactly L0 and L1 on disk");
}
