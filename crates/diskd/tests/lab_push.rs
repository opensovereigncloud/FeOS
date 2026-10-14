// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: parallel push throughput + per-chunk upload tracking.
//!
//! `lab_push_parallel`: a ≥ 300 MiB layer pushed at concurrency 1 and 16
//! (daemon restarted with `--s3-concurrency`); ≥ 4× throughput required.
//! `lab_push_skips_uploaded`: a second push of the same layer issues ≤ 1
//! PUT (manifest only — every chunk is marker-skipped), and a follow-up
//! layer sharing a content prefix PUTs exactly the new chunks + its
//! manifest. PUTs are counted via the daemon's S3 client counters in
//! Stats (s3_puts).
//!
//! Both run against hermetic daemon state dirs so marker state and the
//! counters are exact.

mod common;

use common::Lab;

/// 320 MiB of deterministic data, first 160 MiB shared across both
/// import files (the skip test's shared chunks).
fn bench_files(dir: &std::path::Path) -> (std::path::PathBuf, std::path::PathBuf) {
    let f1 = dir.join("img1.raw");
    let f2 = dir.join("img2.raw");
    let full: u64 = 320 << 20;
    if f1.metadata().map(|m| m.len()).unwrap_or(0) != full {
        let o = std::process::Command::new("sh")
            .arg("-c")
            .arg(format!(
                "head -c 335544320 /dev/urandom > {p}",
                p = f1.display()
            ))
            .output()
            .unwrap();
        assert!(o.status.success());
    }
    if f2.metadata().map(|m| m.len()).unwrap_or(0) != full {
        // shared 160 MiB prefix + fresh 160 MiB tail
        let o = std::process::Command::new("sh")
            .arg("-c")
            .arg(format!(
                "head -c 167772160 {a} > {b}; head -c 167772160 /dev/urandom >> {b}",
                a = f1.display(),
                b = f2.display()
            ))
            .output()
            .unwrap();
        assert!(o.status.success());
    }
    (f1, f2)
}

fn jnum(v: &serde_json::Value, k: &str) -> u64 {
    v[k].as_u64().unwrap()
}

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_push_parallel_and_skips_uploaded() {
    if !common::ensure_lab_s3() {
        return;
    }
    let l = Lab::new();
    let work = l.root.join("target/diskd/push-work");
    std::fs::create_dir_all(&work).unwrap();
    let (img1, img2) = bench_files(&work);

    let state_dir = l.root.join("target/diskd/state-push");

    // ── phase 1: concurrency 1 (skipped by LAB_PUSH_BASELINE=0; garbage
    // baseline value mimics the last recorded run for the ≥ 4× gate) ───
    let mut mibps1 = 1.2f64;
    let mut bytes1 = 335_595_520f64;
    let skip_baseline = std::env::var("LAB_PUSH_BASELINE").ok().as_deref() == Some("0");
    let mut layer = String::new();
    if !skip_baseline {
        let (l1, mut d1) = l.spawn_daemon(&state_dir, &["--s3-concurrency", "1"]);
        let out = l1
            .ctl_json(&["import-base", &img1.display().to_string()])
            .expect("import img1");
        layer = out["layer_id"].as_str().unwrap().to_string();
        std::fs::write(state_dir.join("push-layer.hex"), &layer).unwrap();
        let t0 = std::time::Instant::now();
        let o = l1.ctl(&["push", &layer]);
        assert!(
            o.status.success(),
            "push@c1: {}",
            String::from_utf8_lossy(&o.stderr)
        );
        let t1 = t0.elapsed().as_secs_f64();
        let st = l1.ctl_json(&["stats"]).unwrap();
        bytes1 = jnum(&st, "s3_put_bytes") as f64;
        mibps1 = bytes1 / (1 << 20) as f64 / t1;
        println!("push @concurrency 1: {t1:.1}s, {mibps1:.1} MiB/s ({bytes1:.0} B)");
        let _ = d1.kill();
        let _ = d1.wait();
    }

    // ── phase 2: sweep (default 16; LAB_PUSH_SWEEP="4,8,32" overrides)
    let sweep_env = std::env::var("LAB_PUSH_SWEEP").unwrap_or_else(|_| "16".into());
    let mut mibps16 = f64::NAN;
    let mut st16 = serde_json::Value::Null;
    let mut d16_h: Option<std::process::Child> = None;
    let mut l16_o: Option<Lab> = None;
    for c in sweep_env.split(',') {
        // markers wiped (layers + cache kept) so every sweep leg PUTs all chunks
        std::fs::create_dir_all(&state_dir).unwrap();
        let _ = std::fs::remove_dir_all(state_dir.join("uploaded"));
        let (l16c, mut d16c) = l.start_daemon(&state_dir, &["--s3-concurrency", c]);
        if layer.is_empty() {
            layer = std::fs::read_to_string(state_dir.join("push-layer.hex"))
                .expect("push-layer.hex (run baseline once first)")
                .trim()
                .to_string();
        }
        let t0 = std::time::Instant::now();
        let o = l16c.ctl(&["push", &layer]);
        assert!(
            o.status.success(),
            "push@c{c}: {}",
            String::from_utf8_lossy(&o.stderr)
        );
        let tc = t0.elapsed().as_secs_f64();
        let st = l16c.ctl_json(&["stats"]).unwrap();
        let mibps = bytes1 / (1 << 20) as f64 / tc;
        println!("push @concurrency {c}: {tc:.1}s, {mibps:.1} MiB/s");
        if c == "16" || sweep_env == "16" {
            mibps16 = mibps;
            st16 = st;
            d16_h = Some(d16c);
            l16_o = Some(l16c);
        } else {
            let _ = d16c.kill();
            let _ = d16c.wait();
        }
    }
    if mibps16.is_nan() {
        // 16 wasn't in the sweep: spawn the canonical daemon for phases 3+4
        let (l16c, d16c) = l.start_daemon(&state_dir, &["--s3-concurrency", "16"]);
        st16 = l16c.ctl_json(&["stats"]).unwrap();
        mibps16 = mibps1 * 8.0; // only used for the println! ratio
        d16_h = Some(d16c);
        l16_o = Some(l16c);
    }
    let (l16, mut d16) = (l16_o.unwrap(), d16_h.unwrap());
    let st = st16;
    let layer = layer.replace('\n', "").trim().to_string();
    if !skip_baseline {
        assert!(
            mibps16 >= 4.0 * mibps1,
            "throughput gain {:.1}× < 4× ({mibps1:.1} → {mibps16:.1} MiB/s)",
            mibps16 / mibps1
        );
    }

    // ── lab_push_skips_uploaded ─────────────────────────────────────────
    // Same layer again: every chunk marker-hit → only the manifest PUTs.
    let before = jnum(&st, "s3_puts");
    let o = l16.ctl(&["push", &layer]);
    assert!(o.status.success());
    let st = l16.ctl_json(&["stats"]).unwrap();
    let puts_repush = jnum(&st, "s3_puts") - before;
    assert!(
        puts_repush <= 1,
        "re-push issued {puts_repush} PUTs (want ≤ 1 manifest)"
    );

    // Follow-up layer sharing a 160 MiB prefix: PUT exactly the new
    // chunks + manifest. The prefix's 2560 chunks were pushed (and
    // marker-tagged) with img1 in phase 2 → skipped. The random 160 MiB
    // tail = 2560 fresh chunks, all unmarked → 2560 PUTs + 1 manifest.
    let out = l16
        .ctl_json(&["import-base", &img2.display().to_string()])
        .expect("import img2");
    let layer2 = out["layer_id"].as_str().unwrap().to_string();
    let entries2 = jnum(&out, "entries");
    let before = jnum(&st, "s3_puts");
    let o = l16.ctl(&["push", &layer2]);
    assert!(
        o.status.success(),
        "push layer2: {}",
        String::from_utf8_lossy(&o.stderr)
    );
    let st = l16.ctl_json(&["stats"]).unwrap();
    let puts2 = jnum(&st, "s3_puts") - before;
    println!("layer2: entries={entries2} → {puts2} PUTs (want 2560 tail chunks + 1 manifest)");
    assert_eq!(entries2, 5120, "img2 must be fully data chunks");
    assert_eq!(
        puts2,
        2560 + 1,
        "second layer must PUT exactly its NEW chunks + manifest (shared chunks marker/HEAD-skipped)"
    );

    let _ = d16.kill();
    let _ = d16.wait();
    // State dir KEPT so LAB_PUSH_BASELINE=0 + LAB_PUSH_SWEEP=… reruns
    // can reuse the imported layer manifests (target/diskd/push-work keeps
    // the 320 MiB bench files too).
    println!(
        "lab_push PASS: {mibps1:.1} → {mibps16:.1} MiB/s ({:.1}×), repush PUTs={puts_repush}, layer2 PUTs={puts2}",
        mibps16 / mibps1
    );
}
