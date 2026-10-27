// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! `bench-store` — the chunk store's measurement rig (`make diskd-bench-store`).
//!
//! Workload: one synthetic "base image" + N pod tops with small
//! scattered 4 KiB writes (~2 % dirty). Sealed at 64 KiB / 256 KiB / 2 MiB
//! chunk sizes in independent state dirs. Records seal throughput, dedup
//! hit rate, new-bytes write amplification per chunk size, and (when the
//! S3 env is present) upload/download MiB/s against the real bucket.
//!
//! Output: JSON appended into <out>/<date>-<sha>.json. This bench fixed
//! the disk chunk size.
//!
//! Performance numbers are only valid on XFS (ADR 0011); the Makefile
//! refuses to run this elsewhere.

use std::fs;
use std::os::unix::fs::FileExt;
use std::path::{Path, PathBuf};
use std::time::Instant;

use disk_store::cache::Cache;
use disk_store::manifest::Manifest;
use disk_store::seal;
use rand::rngs::SmallRng;
use rand::{Rng, SeedableRng};
use serde::Serialize;

const MIB: u64 = 1 << 20;

#[derive(Serialize)]
struct Section {
    chunk_size_kib: u32,
    seal_base_seconds: f64,
    seal_base_gib_per_s: f64,
    pods: Vec<PodRow>,
}

#[derive(Serialize)]
struct PodRow {
    dirty_4k_blocks: u64,
    seal_seconds: f64,
    chunks_touched: u64,
    chunks_new: u64,
    chunks_pool_hit: u64,
    chunks_zeroed: u64,
    chunks_inherited: u64,
    bytes_new: u64,
    dedup_hit_rate: String,
}

#[derive(Serialize)]
struct S3Row {
    upload_mib_per_s: f64,
    download_mib_per_s: f64,
    mib: f64,
}

#[derive(Serialize)]
struct Report {
    date: String,
    git_sha: String,
    image_mib: u64,
    pods_per_size: u32,
    dirty_fraction: f64,
    sections: Vec<Section>,
    s3: Option<S3Row>,
}

fn main() -> disk_store::Result<()> {
    let args: Vec<String> = std::env::args().collect();
    let opt = |name: &str, default: &str| -> String {
        args.windows(2)
            .find(|w| w[0] == format!("--{name}"))
            .map(|w| w[1].clone())
            .unwrap_or_else(|| default.into())
    };
    let out = PathBuf::from(opt("out", "target/diskd/bench"));
    let state_root = PathBuf::from(opt("state", "target/diskd/bench-state"));
    let image_mib: u64 = opt("size-mib", "256").parse().unwrap_or(256);
    let n_pods: u32 = opt("pods", "4").parse().unwrap_or(4);
    let dirty = 0.02f64;

    let git_sha = std::env::var("GIT_SHA").unwrap_or_else(|_| "unknown".into());
    let date = std::env::var("DATE").unwrap_or_else(|_| "unknown".into());
    fs::create_dir_all(&out)?;
    fs::create_dir_all(&state_root)?;

    eprintln!("== building synthetic base image ({image_mib} MiB)");
    let logical = image_mib * MIB;
    let base_path = state_root.join("base.raw");
    build_base_image(&base_path, logical)?;

    // Pre-compute the mutation sets (shared across chunk sizes).
    let mut rng = SmallRng::seed_from_u64(0xBE11C5);
    let n_blocks = logical / 4096;
    let dirty_blocks = (n_blocks as f64 * dirty) as u64;
    let mut mutations: Vec<Vec<(u64, Vec<u8>)>> = Vec::new();
    for s in 0..n_pods {
        let mut seen = std::collections::BTreeSet::new();
        let mut ops = Vec::new();
        while (ops.len() as u64) < dirty_blocks {
            let blk = rng.random_range(0..n_blocks);
            if !seen.insert(blk) {
                continue;
            }
            let mut data = vec![0u8; 4096];
            rng.fill(&mut data[..]);
            // tag with pod id so pods differ from each other
            data[0..4].copy_from_slice(&s.to_le_bytes());
            ops.push((blk * 4096, data));
        }
        mutations.push(ops);
    }

    let mut sections = Vec::new();
    for &cs_kib in &[64u32, 256, 2048] {
        let chunk_size = cs_kib * 1024;
        eprintln!("== chunk size {cs_kib} KiB");
        let state = state_root.join(format!("run-{cs_kib}k"));
        let _ = fs::remove_dir_all(&state);
        fs::create_dir_all(&state)?;
        let cache = Cache::open(&state, 1 << 40)?;

        // Base seal (L0): the top *is* the base image file.
        let t = Instant::now();
        let base_seal = seal::seal_top(
            &base_path,
            &[],
            None,
            &cache,
            &state,
            logical,
            chunk_size,
            None,
        )?;
        let base_secs = t.elapsed().as_secs_f64();
        let l0 = base_seal.manifest.clone();

        let mut rows = Vec::new();
        for (i, ops) in mutations.iter().enumerate() {
            let top = state.join(format!("top-{i}.img"));
            let f = fs::File::create(&top)?;
            f.set_len(logical)?;
            for (off, data) in ops {
                f.write_all_at(data, *off)?;
            }
            drop(f);
            let r = seal::seal_top(
                &top,
                &[&l0],
                None,
                &cache,
                &state,
                logical,
                chunk_size,
                None,
            )?;
            let hit = if r.stats.chunks_new > 0 {
                format!(
                    "{:.1}%",
                    100.0 * r.stats.chunks_pool_hit as f64 / r.stats.chunks_new as f64
                )
            } else {
                "—".into()
            };
            rows.push(PodRow {
                dirty_4k_blocks: ops.len() as u64,
                seal_seconds: r.stats.seal_seconds,
                chunks_touched: r.stats.chunks_touched,
                chunks_new: r.stats.chunks_new,
                chunks_pool_hit: r.stats.chunks_pool_hit,
                chunks_zeroed: r.stats.chunks_zeroed,
                chunks_inherited: r.stats.chunks_inherited,
                bytes_new: r.stats.bytes_new,
                dedup_hit_rate: hit,
            });
            fs::remove_file(&top)?;
        }
        sections.push(Section {
            chunk_size_kib: cs_kib,
            seal_base_seconds: base_secs,
            seal_base_gib_per_s: (logical as f64 / (1 << 30) as f64) / base_secs,
            pods: rows,
        });
    }

    // S3 tiering numbers against the real bucket (skipped without env).
    let rt = tokio::runtime::Runtime::new().map_err(disk_store::Error::Io)?;
    let run_id = format!(
        "{}",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0)
    );
    let s3row = rt.block_on(async {
        let Some(s3) = disk_store::s3::S3::from_env(format!("diskd-test/bench-{run_id}/"))? else {
            eprintln!("== S3 env absent; skipping upload/download section");
            return Ok::<Option<S3Row>, disk_store::Error>(None);
        };
        eprintln!("== s3 section (prefix diskd-test/bench-{run_id}/)");
        let state = state_root.join("run-256k");
        let cache = Cache::open(&state, 1 << 40)?;
        // Load a manifest from disk (the first pod layer of the 256k run).
        let layers_dir = state.join("layers");
        let mut manifests: Vec<Manifest> = Vec::new();
        let mut entries: Vec<_> = fs::read_dir(&layers_dir)?
            .filter_map(|e| e.ok())
            .map(|e| e.path())
            .collect();
        entries.sort();
        for p in entries.iter().take(2) {
            manifests.push(Manifest::decode(&fs::read(p)?)?);
        }
        let refs: Vec<&Manifest> = manifests.iter().collect();

        // Upload all chunks + manifest (chunks first, the commit rule).
        let mut up_bytes = 0u64;
        let t = Instant::now();
        for m in &refs {
            for e in &m.entries {
                if e.kind != disk_store::EntryKind::Data {
                    continue;
                }
                let raw = fs::read(cache.chunk_path(&blake3::Hash::from_bytes(e.hash)))?;
                up_bytes += raw.len() as u64;
                s3.put_chunk(&blake3::Hash::from_bytes(e.hash), &raw)
                    .await?;
            }
        }
        let up_secs = t.elapsed().as_secs_f64();

        // Wipe the local copies and hydrate back.
        let st = cache.evict(disk_store::cache::EvictTarget::All)?;
        eprintln!("   evicted {} bytes", st.freed_bytes);
        let t = Instant::now();
        let fetched = disk_store::s3::hydrate_chain(&refs, &cache, &s3, 16).await?;
        let down_secs = t.elapsed().as_secs_f64();

        s3.delete_prefix().await?;
        Ok(Some(S3Row {
            upload_mib_per_s: (up_bytes as f64 / MIB as f64) / up_secs,
            download_mib_per_s: (fetched as f64 / MIB as f64) / down_secs,
            mib: up_bytes as f64 / MIB as f64,
        }))
    })?;

    let report = Report {
        date: date.clone(),
        git_sha: git_sha.clone(),
        image_mib,
        pods_per_size: n_pods,
        dirty_fraction: dirty,
        sections,
        s3: s3row,
    };
    let out_path = out.join(format!("{date}-{git_sha}.json"));
    fs::write(
        &out_path,
        serde_json::to_vec_pretty(&report)
            .map_err(|e| disk_store::Error::S3(format!("json: {e}")))?,
    )?;
    eprintln!("== wrote {}", out_path.display());
    Ok(())
}

fn build_base_image(path: &Path, logical: u64) -> disk_store::Result<()> {
    // ~rootfs-ish: a pool of 64 random dense 4KiB blocks (compressible
    // little), zero blocks, and repeated copies — so dedup and zstd both
    // have realistic input.
    let mut rng = SmallRng::seed_from_u64(42);
    let pool: Vec<Vec<u8>> = (0..64)
        .map(|_| {
            let mut b = vec![0u8; 4096];
            rng.fill(&mut b[..]);
            b
        })
        .collect();
    let f = fs::File::create(path)?;
    let n_blocks = logical / 4096;
    let zero = vec![0u8; 4096];
    let mut block = vec![0u8; 4096];
    for i in 0..n_blocks {
        // 60% dense pool blocks, 25% zeros, 15% repeated text-ish.
        let pick = rng.random_range(0..100u32);
        let data: &[u8] = match pick {
            0..=59 => &pool[(i as usize + pick as usize) % 64],
            60..=84 => &zero,
            _ => {
                block.clear();
                while block.len() < 4096 {
                    block.extend_from_slice(b"/usr/lib/x86_64-linux-gnu/libsomething.so.1.2.3\n");
                }
                block.truncate(4096);
                &block
            }
        };
        f.write_all_at(data, i * 4096)?;
    }
    Ok(())
}
