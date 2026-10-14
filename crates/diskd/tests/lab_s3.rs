// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: cold read path against S3. Seals L0 from a locally built
//! base image, pushes chunks-before-manifest to the real bucket
//! (prefix="" production layout), evicts the daemon cache, then reads
//! through the device and asserts (a) bytes match the source image and
//! (b) the fetch counter moved — reads were served from S3, not local.
//!
//! Cleanup removes exactly the objects this test uploaded.

mod common;

use std::path::Path;

use disk_store::manifest::LayerId;

fn read_at(path: &Path, off: u64, len: usize) -> Vec<u8> {
    use std::os::unix::fs::FileExt;
    let f = std::fs::File::open(path).expect("open for read_at");
    let mut buf = vec![0u8; len];
    f.read_exact_at(&mut buf, off).expect("read_at");
    buf
}

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_cache_miss_s3() {
    if !common::ensure_lab_s3() {
        return;
    }
    let Some(s3) = disk_store::s3::S3::from_env("").expect("s3 config") else {
        println!("skipping: no S3 env (S3_BUCKET/AWS_* absent)");
        return;
    };
    let l = common::Lab::new();
    const POD: &str = "lab-cold";
    // Own base image: this test's cleanup DELETES its S3 chunks, so the
    // content must not alias any other test's layer (content-addressed
    // — same bytes, same layer id, same objects).
    let img = l.root.join("target/diskd/imported/lab-cold-base.raw");
    if !img.is_file() {
        std::fs::create_dir_all(img.parent().unwrap()).unwrap();
        let o = std::process::Command::new("sh")
            .arg("-c")
            .arg(format!(
                "head -c 67108864 /dev/urandom > {p}",
                p = img.display()
            ))
            .output()
            .unwrap();
        assert!(o.status.success());
    }

    // 1) seal L0 locally via the daemon.
    let out = l
        .ctl_json(&["import-base", &img.display().to_string()])
        .expect("import-base");
    let l0 = out["layer_id"].as_str().unwrap().to_string();
    let l0_id = LayerId::from_hex(&l0).expect("layer id");
    let man_path = l.state.join("layers").join(format!("{l0}.manifest"));
    let man = disk_store::Manifest::decode(&std::fs::read(&man_path).expect("manifest read"))
        .expect("manifest decode");

    // 2) push chunks before the manifest (the commit rule).
    let cache = disk_store::cache::Cache::open(&l.state, 1 << 40).expect("cache open");
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("rt");
    let mut uploaded_bytes = 0u64;
    {
        let res: Result<(), Box<dyn std::error::Error>> = rt.block_on(async {
            for e in &man.entries {
                if e.kind != disk_store::manifest::EntryKind::Data {
                    continue;
                }
                let h = blake3::Hash::from_bytes(e.hash);
                if s3.has_chunk(&h).await {
                    continue;
                }
                let raw = std::fs::read(cache.chunk_path(&h))?;
                uploaded_bytes += raw.len() as u64;
                s3.put_chunk(&h, &raw).await?;
            }
            s3.put_manifest(&l0_id, &man).await?;
            Ok(())
        });
        if let Err(e) = res {
            panic!("push failed: {e} (cleanup still runs; orphaned zst objects are invisible below a missing manifest)");
        }
    }
    println!(
        "pushed {} chunks ({} MiB) + manifest",
        man.entries
            .iter()
            .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
            .count(),
        uploaded_bytes / (1 << 20)
    );

    // 3) evict all → the reads below are genuinely cold.
    let ev = l.ctl_json(&["evict", "all", "--force"]).expect("evict");
    println!("evicted: {}", serde_json::to_string(&ev).unwrap());

    // 4) create the device and read two cold regions; bytes must match.
    l.cleanup(POD);
    let _ = std::process::Command::new("sudo")
        .args(["rm", "-f"])
        .arg(l.state.join("live").join(POD).join("top.img"))
        .output();
    l.ctl_json(&["create", POD, &l0]).expect("create");
    let devp = format!("{}/live/{}/disk", l.state.display(), POD);

    // Tear-down guard: delete exactly what we uploaded, whatever happens
    // — and drop the matching LOCAL upload markers, or a later push
    // would skip a chunk that no longer exists in S3 (stale-marker trap;
    // `fsck --s3` is the repair path for out-of-band deletions).
    struct Cleanup<'a> {
        s3: &'a disk_store::s3::S3,
        l0_hex: String,
        state: std::path::PathBuf,
        rt: &'a tokio::runtime::Runtime,
    }
    impl Drop for Cleanup<'_> {
        fn drop(&mut self) {
            let s3 = self.s3;
            let l0 = LayerId::from_hex(&self.l0_hex).expect("layer id");
            let state = self.state.clone();
            self.rt.block_on(async move {
                if let Ok(m) = s3.get_manifest(&l0).await {
                    for e in m
                        .entries
                        .iter()
                        .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
                    {
                        let h = blake3::Hash::from_bytes(e.hash);
                        s3.delete_chunk(&h).await.ok();
                        let hex = h.to_hex().to_string();
                        let _ = std::fs::remove_file(
                            state
                                .join("uploaded")
                                .join("chunks")
                                .join(&hex[0..2])
                                .join(&hex),
                        );
                    }
                }
                s3.delete_object(&s3.manifest_key(&l0)).await.ok();
                let _ = std::fs::remove_file(state.join("uploaded").join(&self.l0_hex));
            });
        }
    }
    let cleanup = Cleanup {
        s3: &s3,
        l0_hex: l0.clone(),
        state: l.state.clone(),
        rt: &rt,
    };

    // Derive offsets from the file's size.
    let size = std::fs::metadata(&img).unwrap().len();
    let tail = size - (2u64 << 20);
    for off in [0u64, size / 2, tail] {
        let want = read_at(&img, off, 1 << 20);
        let got = read_at(Path::new(&devp), off, 1 << 20);
        assert_eq!(want.len(), got.len());
        assert_eq!(
            blake3::hash(&want),
            blake3::hash(&got),
            "cold read mismatch at offset {off}"
        );
    }

    // 5) the fetch counter must have moved — S3, not local, served this.
    let stats = l.ctl_json(&["stats", POD]).expect("stats");
    let dev = &stats["devices"][0];
    let misses = dev["cache_misses"].as_u64().unwrap_or(0);
    let s3f = dev["s3_fetches"].as_u64().unwrap_or(0);
    let errs = dev["errors"].as_u64().unwrap_or(1);
    println!("cache_misses={misses} s3_fetches={s3f} errors={errs}");
    assert!(misses > 0, "no cache misses — reads were served locally?");
    assert!(s3f > 0, "no S3 fetches — laziness not exercised");
    assert_eq!(errs, 0, "device errors");

    drop(cleanup);
    l.cleanup(POD);
    println!("lab_cache_miss_s3 PASS");
}
