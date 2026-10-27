// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Shared helpers for store integration tests (allowed to panic/unwrap).

#![allow(dead_code)]

use std::fs;
use std::os::unix::fs::FileExt;
use std::path::{Path, PathBuf};

use disk_store::cache::Cache;
use disk_store::manifest::Manifest;
use rand::rngs::SmallRng;
use rand::{Rng, SeedableRng};

pub const LOGICAL: u64 = 4 * 1024 * 1024; // 4 MiB test disk
pub const CHUNK: u32 = 64 * 1024;

/// One write op into a sparse file / composed view.
pub type Op = (u64, Vec<u8>);

pub fn rng(seed: u64) -> SmallRng {
    SmallRng::seed_from_u64(seed)
}

pub fn rand_bytes(r: &mut SmallRng, len: usize) -> Vec<u8> {
    let mut v = vec![0u8; len];
    r.fill(&mut v[..]);
    v
}

/// Create a sparse file of `size` with `ops` applied in order.
pub fn sparse_file(dir: &Path, name: &str, size: u64, ops: &[Op]) -> PathBuf {
    let p = dir.join(name);
    let f = fs::File::create(&p).unwrap();
    f.set_len(size).unwrap();
    for (off, data) in ops {
        f.write_all_at(data, *off).unwrap();
    }
    p
}

/// Apply ops to a composed view (holes = keep existing bytes).
pub fn apply(view: &mut [u8], ops: &[Op]) {
    for (off, data) in ops {
        let off = *off as usize;
        view[off..off + data.len()].copy_from_slice(data);
    }
}

/// Read the whole file into bytes.
pub fn bytes_of(p: &Path) -> Vec<u8> {
    fs::read(p).unwrap()
}

pub fn open_cache(dir: &Path) -> Cache {
    Cache::open(dir, u64::MAX).unwrap()
}

/// Load all manifests under state/layers as (layer_id, Manifest).
pub fn load_layers(state: &Path) -> Vec<(String, Manifest)> {
    let dir = state.join("layers");
    let mut out = Vec::new();
    if !dir.is_dir() {
        return out;
    }
    let mut paths: Vec<PathBuf> = fs::read_dir(&dir)
        .unwrap()
        .map(|e| e.unwrap().path())
        .collect();
    paths.sort();
    for p in paths {
        let id = p
            .file_name()
            .unwrap()
            .to_str()
            .unwrap()
            .trim_end_matches(".manifest")
            .to_string();
        let m = Manifest::decode(&fs::read(&p).unwrap()).unwrap();
        out.push((id, m));
    }
    out
}
