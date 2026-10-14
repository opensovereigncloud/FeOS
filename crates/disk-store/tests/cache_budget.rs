// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Budget actor: LRU accounting, eviction = unlink, stats.

mod common;

use disk_store::cache::{Cache, EvictTarget};
use disk_store::chunk;

use common::*;

fn admit_some(cache: &Cache, seed_base: u64, n: usize, len: usize) -> Vec<blake3::Hash> {
    let mut out = Vec::new();
    for i in 0..n {
        let data = rand_bytes(&mut rng(seed_base + i as u64), len);
        let h = chunk::hash(&data);
        cache.admit(&h, &data).unwrap();
        out.push(h);
    }
    out
}

#[test]
fn budget_evicts_lru() {
    let dir = tempfile::tempdir().unwrap();
    let cache = Cache::open(dir.path(), 3 * 4096).unwrap();
    let hs = admit_some(&cache, 100, 3, 4096);
    assert_eq!(cache.stats().unwrap().bytes, 3 * 4096);

    // Touch the oldest so the second-oldest dies first.
    cache.touch(&hs[0]);
    let more = admit_some(&cache, 999, 1, 4096);
    // Eviction runs inside the budget actor; wait for it.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
    while cache.contains(&hs[1]) && std::time::Instant::now() < deadline {
        std::thread::sleep(std::time::Duration::from_millis(10));
    }
    assert!(cache.contains(&hs[0]), "recently touched survives");
    assert!(!cache.contains(&hs[1]), "LRU victim evicted");
    assert!(cache.contains(&hs[2]));
    assert!(cache.contains(&more[0]));
    let st = cache.stats().unwrap();
    assert_eq!(st.bytes, 3 * 4096);
    assert!(st.evicted_bytes >= 4096);
}

#[test]
fn evict_all_drops_everything() {
    let dir = tempfile::tempdir().unwrap();
    let cache = Cache::open(dir.path(), 1 << 30).unwrap();
    let hs = admit_some(&cache, 200, 5, 8192);
    let st = cache.evict(EvictTarget::All).unwrap();
    assert_eq!(st.freed_bytes, 5 * 8192);
    for h in &hs {
        assert!(!cache.contains(h));
    }
    assert_eq!(cache.stats().unwrap().bytes, 0);
}

#[test]
fn reopen_adopts_existing_chunks() {
    let dir = tempfile::tempdir().unwrap();
    let hs = {
        let cache = Cache::open(dir.path(), 1 << 30).unwrap();
        let hs = admit_some(&cache, 300, 4, 1024);
        assert_eq!(cache.stats().unwrap().entries, 4);
        hs
    };
    // "Restart": new actor adopts the files.
    let cache = Cache::open(dir.path(), 1 << 30).unwrap();
    assert_eq!(cache.stats().unwrap().entries, 4);
    assert_eq!(cache.stats().unwrap().bytes, 4 * 1024);
    for h in &hs {
        assert!(cache.contains(h));
        assert!(!cache.contains(&blake3::Hash::from_bytes([0u8; 32])));
    }
}

#[test]
fn admit_is_idempotent() {
    let dir = tempfile::tempdir().unwrap();
    let cache = Cache::open(dir.path(), 1 << 30).unwrap();
    let data = rand_bytes(&mut rng(5), 4096);
    let h = chunk::hash(&data);
    cache.admit(&h, &data).unwrap();
    cache.admit(&h, &data).unwrap();
    cache.admit(&h, &data).unwrap();
    assert_eq!(cache.stats().unwrap().entries, 1);
    assert_eq!(cache.stats().unwrap().bytes, 4096);
}
