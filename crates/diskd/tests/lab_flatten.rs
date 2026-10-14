// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: flatten-on-seal (ADR 0011) against a live
//! daemon on its own hermetic state dir.
//!
//! Three stop/seal/open cycles with writes in between: the row chain is
//! a constant `<L0, S>`, `Materialize` equals the host-computed ground
//! truth of everything written through the ublk device, each previous
//! `S` manifest is gone locally, and `Fsck` lists exactly the former S
//! layers' exclusively-owned chunks as garbage. Plus the idempotence
//! rule: seal with no writes → same layer id, no manifest churn.
//!
//! Writes go through the device's live symlink with O_DIRECT — the same
//! data path a guest exercises, without booting a VM.

mod common;

use std::os::unix::fs::FileExt;
use std::os::unix::fs::OpenOptionsExt;

use common::Lab;

const POD: &str = "lab-flatten";
const MIB: u64 = 1 << 20;
const DISK: u64 = 4 * MIB; // 64 chunks at 64 KiB
const CHUNK: u64 = 64 * 1024;

fn pattern(seed: u8, len: usize) -> Vec<u8> {
    // Deterministic pseudo-random: xorshift64* over the seed.
    let mut x = (seed as u64 + 1) * 0x9E3779B97F4A7C15;
    let mut out = vec![0u8; len];
    let mut i = 0;
    while i < len {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        let n = (len - i).min(8);
        out[i..i + n].copy_from_slice(&x.to_le_bytes()[..n]);
        i += n;
    }
    out
}

fn write_dev(link: &str, off: u64, buf: &[u8]) {
    let f = std::fs::OpenOptions::new()
        .write(true)
        .custom_flags(libc::O_DIRECT)
        .open(link)
        .unwrap();
    f.write_all_at(buf, off).unwrap();
    f.sync_all().unwrap();
}

fn row_chain(l: &Lab, pod: &str) -> Vec<String> {
    let st: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(l.state.join("diskd.state")).unwrap())
            .unwrap();
    st["devices"][pod]["chain"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap().to_string())
        .collect()
}

#[test]
#[ignore = "lab: needs root and ublk (make diskd-lab)"]
fn lab_flatten() {
    if !common::ensure_lab_daemon() {
        return;
    }
    let l = Lab::new();
    let state_dir = l.root.join("target/diskd/state-flatten");
    let (l2, mut daemon) = l.spawn_daemon(&state_dir, &[]);

    // Base image: data at chunks 0 and 1, rest sparse.
    let base = state_dir.join("base.raw");
    {
        let f = std::fs::File::create(&base).unwrap();
        f.set_len(DISK).unwrap();
        f.write_all_at(&pattern(1, CHUNK as usize), 0).unwrap();
        f.write_all_at(&pattern(2, CHUNK as usize), CHUNK).unwrap();
    }
    let out = l2
        .ctl_json(&["import-base", &base.display().to_string()])
        .unwrap();
    let l0 = out["layer_id"].as_str().unwrap().to_string();

    l2.cleanup(POD);
    let dev = l2.ctl_json(&["create", POD, &l0]).expect("create");
    let link = dev["link_path"].as_str().unwrap().to_string();

    // Ground truth of the composed disk.
    let mut expected = vec![0u8; DISK as usize];
    expected[0..CHUNK as usize].copy_from_slice(&pattern(1, CHUNK as usize));
    expected[CHUNK as usize..2 * CHUNK as usize].copy_from_slice(&pattern(2, CHUNK as usize));

    let mut prev_s: Option<String> = None;
    // (cycle, writes): full-chunk aligned writes through the device.
    let cycles: &[&[(u64, Vec<u8>)]] = &[
        &[
            (2 * CHUNK, pattern(3, CHUNK as usize)),
            (0, vec![0u8; CHUNK as usize]),
        ],
        &[
            (2 * CHUNK, pattern(4, CHUNK as usize)),
            (3 * CHUNK, pattern(5, CHUNK as usize)),
        ],
        &[
            (3 * CHUNK, vec![0u8; CHUNK as usize]),
            (4 * CHUNK, pattern(6, CHUNK as usize)),
        ],
    ];
    // Chunks that must end up as fsck garbage: superseded private-layer
    // content — cycle 1's chunk 2 (pattern 3, overwritten in cycle 2)
    // and cycle 2's chunk 3 (pattern 5, re-zeroed in cycle 3). Cycle 2's
    // chunk 2 (pattern 4) is NOT garbage: the cycle-3 flatten copies it
    // into S3 verbatim (manifest-only merge), where it stays referenced.
    let garbage_expect: std::collections::HashSet<String> = [3u8, 5]
        .iter()
        .map(|&s| {
            blake3::hash(&pattern(s, CHUNK as usize))
                .to_hex()
                .to_string()
        })
        .collect();

    let mut last_s = String::new();
    for (i, writes) in cycles.iter().enumerate() {
        for (off, buf) in *writes {
            write_dev(&link, *off, buf);
            let off = *off as usize;
            expected[off..off + buf.len()].copy_from_slice(buf);
        }
        l2.stop(POD);
        let seal = l2.ctl_json(&["seal", POD]).expect("seal");
        let s = seal["layer_id"].as_str().unwrap().to_string();
        println!("cycle {}: S = {}", i + 1, s);

        // Row chain: constant shape [L0, S].
        let chain = row_chain(&l2, POD);
        assert_eq!(
            chain,
            vec![l0.clone(), s.clone()],
            "chain grew — flatten broken"
        );

        // The previous private layer's manifest is gone locally.
        if let Some(prev) = &prev_s {
            assert_ne!(*prev, s, "writes happened, layer must change");
            assert!(
                !l2.state
                    .join("layers")
                    .join(format!("{prev}.manifest"))
                    .is_file(),
                "previous S manifest still local"
            );
        }
        prev_s = Some(s.clone());
        last_s = s;

        // Reopen on the flattened chain (fresh top) for the next cycle.
        l2.ctl_json(&["open", POD]).expect("open after seal");
    }

    // Idempotence: seal with an untouched top → same layer id.
    l2.stop(POD);
    let seal = l2.ctl_json(&["seal", POD]).expect("idempotent seal");
    assert_eq!(
        seal["layer_id"].as_str().unwrap(),
        last_s,
        "untouched top must reseal to the same layer id"
    );
    l2.ctl_json(&["open", POD])
        .expect("open after idempotent seal");

    // Materialize the final chain == host ground truth.
    let raw = state_dir.join("materialized.raw");
    let o = l2.ctl(&["materialize", &raw.display().to_string(), &l0, &last_s]);
    assert!(
        o.status.success(),
        "materialize: {}",
        String::from_utf8_lossy(&o.stderr)
    );
    assert_eq!(
        std::fs::read(&raw).unwrap(),
        expected,
        "materialized != ground truth"
    );

    // Fsck: garbage = exactly the superseded private chunks.
    let fsck = l2.ctl_json(&["fsck"]).expect("fsck");
    let garbage: std::collections::HashSet<String> = fsck["garbage_chunk_ids"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap().to_string())
        .collect();
    assert_eq!(
        garbage, garbage_expect,
        "fsck garbage != superseded private chunks"
    );
    assert!(fsck["dangling_layers"].as_array().unwrap().is_empty());

    l2.cleanup(POD);
    let _ = daemon.kill();
    let _ = daemon.wait();
    let _ = std::fs::remove_dir_all(&state_dir);
    println!("lab_flatten PASS (chain constant [L0, S] over 3 cycles + idempotence)");
}
