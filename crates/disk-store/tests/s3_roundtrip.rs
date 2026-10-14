// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! S3 roundtrip (skipped without S3_BUCKET and credentials in the env): seal → upload
//! (chunks before manifests) → wipe local → materialize from S3 only →
//! byte-identical.

mod common;

use disk_store::cache::{Cache, EvictTarget};
use disk_store::materialize::materialize_bytes;
use disk_store::s3::{hydrate_chain, S3};
use disk_store::seal::seal_top;
use disk_store::Manifest;

use common::*;

const L: u64 = 4 * 1024 * 1024;

/// A key prefix this test alone owns. The `name` matters: the tests in
/// this file run concurrently in one process and each wipes its prefix
/// when it finishes, so pid+seconds alone would let one delete the
/// other's objects out from under it.
fn run_prefix(name: &str) -> String {
    format!(
        "diskd-test/{name}-{}-{}/",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    )
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn s3_roundtrip() {
    let Some(s3) = S3::from_env(run_prefix("roundtrip")).unwrap() else {
        eprintln!("S3 env absent (S3_BUCKET / AWS_* / AWS_ENDPOINT_URL); skipping");
        return;
    };

    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);

    // L0 + one dirty top.
    let base = sparse_file(
        dir.path(),
        "base.raw",
        L,
        &[
            (0, rand_bytes(&mut rng(11), 2 * CHUNK as usize)),
            (10 * CHUNK as u64, rand_bytes(&mut rng(12), CHUNK as usize)),
        ],
    );
    let l0 = seal_top(&base, &[], None, &cache, &state, L, CHUNK, None).unwrap();
    let top = sparse_file(
        dir.path(),
        "top.img",
        L,
        &[
            (12345, rand_bytes(&mut rng(13), 5000)),
            (20 * CHUNK as u64, rand_bytes(&mut rng(14), CHUNK as usize)),
            (0, vec![0u8; 4096]), // zeros over parent data
        ],
    );
    let l1 = seal_top(&top, &[&l0.manifest], None, &cache, &state, L, CHUNK, None).unwrap();
    let expected = materialize_bytes(&[&l0.manifest, &l1.manifest], &cache).unwrap();

    // Upload: chunks first, manifests last (the commit rule).
    for m in [&l0.manifest, &l1.manifest] {
        for e in &m.entries {
            if e.kind != disk_store::EntryKind::Data {
                continue;
            }
            let h = blake3::Hash::from_bytes(e.hash);
            let raw = std::fs::read(cache.chunk_path(&h)).unwrap();
            if !s3.has_chunk(&h).await {
                s3.put_chunk(&h, &raw).await.unwrap();
            }
        }
    }
    s3.put_manifest(&l0.layer_id, &l0.manifest).await.unwrap();
    s3.put_manifest(&l1.layer_id, &l1.manifest).await.unwrap();

    // "Node B": fresh empty state dir, manifests fetched from S3, chunks
    // hydrated lazily.
    let state_b = dir.path().join("stateB");
    std::fs::create_dir_all(&state_b).unwrap();
    let cache_b = Cache::open(&state_b, 1 << 40).unwrap();
    let l0_b = s3.get_manifest(&l0.layer_id).await.unwrap();
    let l1_b = s3.get_manifest(&l1.layer_id).await.unwrap();
    assert_eq!(l0_b.layer_id(), l0.layer_id);
    assert_eq!(l1_b.layer_id(), l1.layer_id);

    let chain: Vec<&Manifest> = vec![&l0_b, &l1_b];
    let fetched = hydrate_chain(&chain, &cache_b, &s3, 16).await.unwrap();
    assert!(fetched > 0);

    let got = materialize_bytes(&[&l0_b, &l1_b], &cache_b).unwrap();
    assert_eq!(got, expected, "S3-only materialize is byte-identical");

    // Evict-all wipes the node-B cache completely.
    let st = cache_b.evict(EvictTarget::All).unwrap();
    assert!(st.freed_bytes > 0);
    assert_eq!(cache_b.stats().unwrap().bytes, 0);

    s3.delete_prefix().await.unwrap();
}

/// A seal reads through the chain for every chunk the top only partly
/// covers. When the budget has evicted one of those parents the bytes are
/// still in the bucket, so the seal has to fetch them rather than fail —
/// otherwise a cache eviction permanently breaks the next hibernation of
/// every pod on that chain.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn seal_refills_an_evicted_parent_from_s3() {
    let Some(s3) = S3::from_env(run_prefix("seal-refill")).unwrap() else {
        eprintln!("S3 env absent (S3_BUCKET / AWS_* / AWS_ENDPOINT_URL); skipping");
        return;
    };
    let s3 = std::sync::Arc::new(s3);

    let dir = tempfile::tempdir().unwrap();
    let state = dir.path().join("state");
    std::fs::create_dir_all(&state).unwrap();
    let cache = open_cache(&state);

    // L0 with one full data chunk, then a top that overwrites only PART
    // of it — that partial overlap is what forces the parent read.
    let base = sparse_file(
        dir.path(),
        "base.raw",
        L,
        &[(0, rand_bytes(&mut rng(21), CHUNK as usize))],
    );
    let l0 = seal_top(&base, &[], None, &cache, &state, L, CHUNK, None).unwrap();
    for e in &l0.manifest.entries {
        if e.kind != disk_store::EntryKind::Data {
            continue;
        }
        let h = blake3::Hash::from_bytes(e.hash);
        s3.put_chunk(&h, &std::fs::read(cache.chunk_path(&h)).unwrap())
            .await
            .unwrap();
    }
    s3.put_manifest(&l0.layer_id, &l0.manifest).await.unwrap();

    // Restart the node with a refill source, then empty the cache: the
    // parent chunk now lives only in the bucket.
    let handle = tokio::runtime::Handle::current();
    let refill_s3 = s3.clone();
    let cache = Cache::open(&state, u64::MAX)
        .unwrap()
        .with_refill(std::sync::Arc::new(
            move |h: &blake3::Hash, raw_len: u32| {
                let s3 = refill_s3.clone();
                let h = *h;
                handle.block_on(async move { s3.get_chunk(&h, raw_len).await })
            },
        ));
    let evicted = cache.evict(EvictTarget::All).unwrap();
    assert!(evicted.freed_bytes > 0);

    let top = sparse_file(dir.path(), "top.img", L, &[(4096, vec![7u8; 1024])]);
    let l1 = tokio::task::spawn_blocking({
        let state = state.clone();
        move || seal_top(&top, &[&l0.manifest], None, &cache, &state, L, CHUNK, None)
    })
    .await
    .unwrap()
    .expect("seal must refill the evicted parent from S3, not fail");
    assert!(l1.stats.chunks_new > 0);

    s3.delete_prefix().await.unwrap();
}
