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
//!
//! `lab_seal_upload_dropped`: a `Seal{upload}` whose caller goes away in
//! the middle of the push (a hibernation abandoned for a resume) leaves
//! the layer sealed, pinned and unheaded, its uploaded chunks marked and
//! listed in the disk's journal; the next push skips them, and `PutHead`
//! ends the pins. `lab_unheaded_expiry`: once the journal is older than 12
//! hours, the next push uploads those chunks again. Both run a daemon of
//! their own, under a bucket prefix of their own that they delete.

mod common;

use std::collections::HashSet;
use std::os::unix::fs::{FileExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

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

// ── a push whose caller went away ──────────────────────────────────────

/// What the pod writes: 48 MiB, 768 chunks, past the first 256 a push
/// marks at a time.
const WRITTEN: usize = 48 << 20;
const WRITTEN_AT: u64 = 8 << 20;

/// A daemon of the test's own, on its own state dir and bucket prefix.
struct Resume {
    lab: Lab,
    daemon: std::process::Child,
    s3: disk_store::s3::S3,
    prefix: String,
    rt: tokio::runtime::Runtime,
    pod: String,
}

impl Resume {
    fn start(tag: &str) -> Resume {
        let l = Lab::new();
        let prefix = format!("diskd-test/resume-{tag}-{}/", std::process::id());
        let state = l.root.join(format!("target/diskd/state-resume-{tag}"));
        // A few PUTs at a time, so that the drop lands in the middle.
        let (lab, daemon) =
            l.spawn_daemon(&state, &["--s3-prefix", &prefix, "--s3-concurrency", "4"]);
        let s3 = disk_store::s3::S3::from_env(prefix.clone())
            .expect("s3 env")
            .expect("s3 configured (S3_BUCKET + creds)");
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        Resume {
            lab,
            daemon,
            s3,
            prefix,
            rt,
            pod: format!("lab-resume-{tag}"),
        }
    }

    fn state(&self) -> &Path {
        &self.lab.state
    }

    fn journal(&self) -> PathBuf {
        self.state().join("unheaded").join(&self.pod)
    }

    /// The journal: when it started, and the chunks it lists.
    fn read_journal(&self) -> (u64, HashSet<String>) {
        let (since, chunks, _) = self.read_journal_layers();
        (since, chunks)
    }

    /// The journal: when it started, the chunks and the layers it lists.
    fn read_journal_layers(&self) -> (u64, HashSet<String>, HashSet<String>) {
        let text = std::fs::read_to_string(self.journal()).expect("the disk's journal");
        let mut lines = text.lines();
        let since = lines.next().unwrap().trim().parse().unwrap();
        let (mut chunks, mut layers) = (HashSet::new(), HashSet::new());
        for l in lines.map(str::trim) {
            match l.strip_prefix("L ") {
                Some(layer) => layers.insert(layer.to_string()),
                None => chunks.insert(l.to_string()),
            };
        }
        (since, chunks, layers)
    }

    /// Moves the journal's start `hours` back, and returns the time now.
    fn age_journal(&self, hours: u64) -> u64 {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
        let text = std::fs::read_to_string(self.journal()).unwrap();
        let rest = text.split_once('\n').map_or("", |(_, rest)| rest);
        std::fs::write(self.journal(), format!("{}\n{rest}", now - hours * 60 * 60)).unwrap();
        now
    }

    fn puts(&self) -> u64 {
        jnum(&self.lab.ctl_json(&["stats"]).unwrap(), "s3_puts")
    }

    fn marked(&self, hex: &str) -> bool {
        self.state()
            .join("uploaded/chunks")
            .join(&hex[0..2])
            .join(hex)
            .is_file()
    }

    fn cached(&self, hex: &str) -> bool {
        self.state()
            .join("cache")
            .join(&hex[0..2])
            .join(hex)
            .is_file()
    }

    fn has_manifest(&self, layer: &str) -> bool {
        let id = disk_store::manifest::LayerId::from_hex(layer).unwrap();
        self.rt.block_on(self.s3.has_manifest(&id))
    }

    fn ok(&self, args: &[&str]) -> serde_json::Value {
        self.lab
            .ctl_json(args)
            .unwrap_or_else(|e| panic!("diskctl {args:?}: {e}"))
    }

    /// A pod on a pushed L0 writes `WRITTEN` bytes; its disk is sealed
    /// without upload, and then a `Seal{upload}` is dropped once the push
    /// has marked its first chunks. Returns the sealed layer, its chunks
    /// and what was written.
    fn dropped_upload(&self, seed: u64) -> (String, HashSet<String>, Vec<u8>) {
        let base = self.state().join("base.raw");
        {
            let f = std::fs::File::create(&base).unwrap();
            f.set_len(64 << 20).unwrap();
            f.write_all_at(&noise(seed ^ 0xba5e, 1 << 20), 0).unwrap();
        }
        let l0 = self.ok(&["import-base", &base.display().to_string()])["layer_id"]
            .as_str()
            .unwrap()
            .to_string();
        let o = self.lab.ctl(&["push", &l0]);
        assert!(o.status.success(), "push l0: {o:?}");

        let link = self.ok(&["create", &self.pod, &l0])["link_path"]
            .as_str()
            .unwrap()
            .to_string();
        let data = noise(seed, WRITTEN);
        direct_write(&link, WRITTEN_AT, &data);
        self.lab.stop(&self.pod);

        let layer = self.ok(&["seal", &self.pod])["layer_id"]
            .as_str()
            .unwrap()
            .to_string();
        assert!(self.journal().is_file(), "a sealed disk is unheaded");
        let chunks: HashSet<String> = self
            .lab
            .load_layer_manifest(&layer)
            .entries
            .iter()
            .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
            .map(|e| disk_store::chunk::Hash::from(e.hash).to_hex().to_string())
            .collect();
        assert!(
            chunks.len() >= WRITTEN / (64 << 10),
            "the layer holds the writes"
        );

        let mut caller = std::process::Command::new(&self.lab.ctl)
            .args(["--socket", &self.lab.socket.display().to_string()])
            .args(["seal", &self.pod, "--upload"])
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .spawn()
            .expect("diskctl seal --upload");
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(300);
        loop {
            assert!(
                caller.try_wait().unwrap().is_none(),
                "the push ended before it could be dropped"
            );
            if self.journal().is_file() && !self.read_journal().1.is_empty() {
                break;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "no chunk was journaled"
            );
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        // The caller goes away: its connection with it.
        caller.kill().unwrap();
        let _ = caller.wait();
        std::thread::sleep(std::time::Duration::from_secs(1));
        let stopped = self.puts();
        std::thread::sleep(std::time::Duration::from_secs(3));
        assert_eq!(self.puts(), stopped, "the dropped push goes on");
        (layer, chunks, data)
    }

    /// Stops the daemon and deletes the test's prefix; the state dir, with
    /// the daemon's log, stays for a test that failed.
    fn stop(mut self, passed: bool) {
        self.lab.cleanup(&self.pod);
        let _ = self.daemon.kill();
        let _ = self.daemon.wait();
        // Never a sweep of the bucket: the test's own prefix only.
        assert!(self.prefix.starts_with("diskd-test/resume-"));
        self.rt.block_on(async {
            let _ = self.s3.delete_prefix().await;
        });
        if passed {
            let _ = std::fs::remove_dir_all(&self.lab.state);
        }
    }
}

/// Bytes of their own for a seed (xorshift).
fn noise(seed: u64, len: usize) -> Vec<u8> {
    let mut x = seed ^ ((std::process::id() as u64) << 32) | 1;
    let mut out = Vec::with_capacity(len + 8);
    while out.len() < len {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        out.extend_from_slice(&x.to_le_bytes());
    }
    out.truncate(len);
    out
}

/// A buffer of `len` bytes at a 4 KiB boundary, for O_DIRECT.
fn aligned(len: usize) -> (Vec<u8>, usize) {
    let buf = vec![0u8; len + 4096];
    let at = (4096 - buf.as_ptr() as usize % 4096) % 4096;
    (buf, at)
}

/// Writes through the device, not the host's page cache.
fn direct_write(link: &str, at: u64, data: &[u8]) {
    let f = std::fs::OpenOptions::new()
        .write(true)
        .custom_flags(libc::O_DIRECT)
        .open(link)
        .unwrap();
    let (mut buf, a) = aligned(1 << 20);
    for (i, piece) in data.chunks(1 << 20).enumerate() {
        buf[a..a + piece.len()].copy_from_slice(piece);
        f.write_all_at(&buf[a..a + piece.len()], at + (i << 20) as u64)
            .unwrap();
    }
    f.sync_all().unwrap();
}

fn direct_read(link: &str, at: u64, len: usize) -> Vec<u8> {
    let f = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECT)
        .open(link)
        .unwrap();
    let (mut buf, a) = aligned(len);
    f.read_exact_at(&mut buf[a..a + len], at).unwrap();
    buf[a..a + len].to_vec()
}

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_seal_upload_dropped() {
    if !common::ensure_lab_s3() {
        return;
    }
    let r = Resume::start("dropped");
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let (layer, chunks, data) = r.dropped_upload(1);

        // Sealed on the node, not in the bucket.
        assert!(!r.has_manifest(&layer), "a dropped push put the manifest");
        assert!(!r.state().join("uploaded").join(&layer).exists());
        let (_, journaled, layers) = r.read_journal_layers();
        assert!(
            journaled.is_subset(&chunks),
            "the journal lists the layer's chunks"
        );
        assert!(layers.is_empty(), "the journal lists a layer not put");
        for hex in &journaled {
            assert!(r.marked(hex), "a journaled chunk has its marker");
        }
        println!(
            "dropped after {} of {} chunks were journaled",
            journaled.len(),
            chunks.len()
        );

        // The disk opens over the sealed layer.
        let link = r.ok(&["open", &r.pod])["link_path"]
            .as_str()
            .unwrap()
            .to_string();
        assert!(
            direct_read(&link, WRITTEN_AT, WRITTEN) == data,
            "the reopened disk lost writes"
        );
        r.lab.stop(&r.pod);

        // Its chunks are the node's only copy: pinned.
        r.ok(&["evict", "all", "--force"]);
        for hex in &chunks {
            assert!(r.cached(hex), "Evict dropped a chunk of an unheaded layer");
        }

        // The next push skips what the dropped one marked.
        let before = r.puts();
        let again = r.ok(&["seal", &r.pod, "--upload"])["layer_id"]
            .as_str()
            .unwrap()
            .to_string();
        assert_eq!(again, layer, "an untouched top seals into the same layer");
        let puts = r.puts() - before;
        println!("second push: {puts} PUTs for {} chunks", chunks.len());
        assert!(
            puts as usize <= chunks.len() - journaled.len() + 1,
            "the second push uploaded journaled chunks again ({puts} PUTs)"
        );
        assert!(r.has_manifest(&layer));
        assert!(r.state().join("uploaded").join(&layer).is_file());
        let (_, _, layers) = r.read_journal_layers();
        assert_eq!(
            layers,
            HashSet::from([layer.clone()]),
            "the layer is listed"
        );

        // A head names the layer: no journal, no pins.
        r.ok(&["put-head", &r.pod, "--epoch", "1"]);
        assert!(!r.journal().exists(), "PutHead leaves the journal");
        r.ok(&["evict", "all", "--force"]);
        for hex in &chunks {
            assert!(!r.cached(hex), "a headed layer's chunk stayed pinned");
        }
    }));
    r.stop(result.is_ok());
    if let Err(e) = result {
        std::panic::resume_unwind(e);
    }
}

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_unheaded_expiry() {
    if !common::ensure_lab_s3() {
        return;
    }
    let r = Resume::start("expiry");
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let (layer, chunks, _) = r.dropped_upload(2);
        let (_, journaled) = r.read_journal();
        assert!(!journaled.is_empty());

        // 13 hours on: the markers of the journal are not trusted.
        let now = r.age_journal(13);
        let before = r.puts();
        r.ok(&["seal", &r.pod, "--upload"]);
        let puts = r.puts() - before;
        println!("push after expiry: {puts} PUTs for {} chunks", chunks.len());
        assert_eq!(
            puts as usize,
            chunks.len() + 1,
            "every chunk again, and the manifest"
        );
        assert!(r.has_manifest(&layer));
        let (since, listed, layers) = r.read_journal_layers();
        assert!(since + 60 >= now, "the journal starts again");
        assert_eq!(listed, chunks, "and lists what the push uploaded");
        assert_eq!(layers, HashSet::from([layer.clone()]));

        // Within the 12 hours, the layer is trusted: a push puts nothing.
        let before = r.puts();
        r.ok(&["seal", &r.pod, "--upload"]);
        assert_eq!(r.puts(), before, "a push within the trust put something");

        // A push that put the whole layer, its manifest too, 13 hours ago:
        // a sweep may delete the layer any time now, so it is put again,
        // although its id did not change.
        r.age_journal(13);
        let before = r.puts();
        r.ok(&["seal", &r.pod, "--upload"]);
        let puts = r.puts() - before;
        println!("push of a whole layer after expiry: {puts} PUTs");
        assert_eq!(
            puts as usize,
            chunks.len() + 1,
            "every chunk again, and the manifest"
        );
        assert!(r.state().join("uploaded").join(&layer).is_file());
        let (_, listed, layers) = r.read_journal_layers();
        assert_eq!(listed, chunks);
        assert_eq!(layers, HashSet::from([layer.clone()]));
    }));
    r.stop(result.is_ok());
    if let Err(e) = result {
        std::panic::resume_unwind(e);
    }
}
