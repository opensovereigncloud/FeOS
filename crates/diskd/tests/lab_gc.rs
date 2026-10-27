// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: the object-store GC sweeper (`Sweep`, ADR 0011).
//!
//! The drill: bake + hibernate + delete the pod → the sweep removes exactly the pod's private layers,
//! heads and exclusively-owned chunks, and keeps the shared bake plus
//! every chunk it references.
//!
//! The daemon under test gets its OWN bucket prefix (`--s3-prefix`): a
//! sweeper is the one RPC that deletes in the bucket, and the lab bucket
//! may be shared. Nothing here can reach an object in use.

mod common;

use std::collections::HashSet;
use std::os::unix::fs::{FileExt, OpenOptionsExt};

use common::Lab;

const POD: &str = "lab-gc";

fn hexes_of(m: &disk_store::Manifest) -> HashSet<String> {
    m.entries
        .iter()
        .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
        .map(|e| disk_store::chunk::Hash::from(e.hash).to_hex().to_string())
        .collect()
}

fn chunk_key(hex: &str) -> String {
    format!("chunks/{}/{}.zst", &hex[0..2], hex)
}

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_sweep_collects_exactly_the_dead_pod() {
    if !common::ensure_lab_s3() {
        return;
    }
    let l = Lab::new();
    let prefix = format!("diskd-test/gc-{}/", std::process::id());
    let state = l.root.join("target/diskd/state-gc");
    let (lg, mut daemon) = l.spawn_daemon(&state, &["--s3-prefix", &prefix]);

    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    let s3 = disk_store::s3::S3::from_env(prefix.clone())
        .expect("s3 env")
        .expect("s3 configured (S3_BUCKET + creds)");

    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        // ---- the shared "bake": a base layer the catalog pins -------
        let img = l.root.join("target/diskd/imported/gc-base.raw");
        std::fs::create_dir_all(img.parent().unwrap()).unwrap();
        {
            let f = std::fs::File::create(&img).unwrap();
            f.set_len(32 << 20).unwrap();
            // distinct content per run: these chunks must SURVIVE, so
            // they must not be shared with another test's layer.
            let mut blk = vec![0u8; 4 << 20];
            for (i, b) in blk.iter_mut().enumerate() {
                *b = (i as u8) ^ (std::process::id() as u8);
            }
            f.write_all_at(&blk, 0).unwrap();
        }
        let l0 = lg
            .ctl_json(&["import-base", &img.display().to_string()])
            .expect("import-base")["layer_id"]
            .as_str()
            .unwrap()
            .to_string();
        let o = lg.ctl(&["push", &l0]);
        assert!(o.status.success(), "push l0: {o:?}");

        // ---- the pod: private disk layer + memory layer ---------
        lg.cleanup(POD);
        let link = lg.ctl_json(&["create", POD, &l0]).expect("create")["link_path"]
            .as_str()
            .unwrap()
            .to_string();
        {
            // O_DIRECT so the bytes reach the device, not the host page
            // cache (see ADR 0011 on Firecracker's cache_type).
            let f = std::fs::OpenOptions::new()
                .write(true)
                .custom_flags(libc::O_DIRECT)
                .open(&link)
                .unwrap();
            let mut blk = vec![0u8; 1 << 20];
            for (i, b) in blk.iter_mut().enumerate() {
                *b = (i as u8).wrapping_add(0x5a) ^ (std::process::id() as u8);
            }
            f.write_all_at(&blk, 16 << 20).unwrap();
            f.sync_all().unwrap();
        }
        lg.stop(POD);
        let private = lg
            .ctl_json(&["seal", POD, "--upload"])
            .expect("seal --upload")["layer_id"]
            .as_str()
            .unwrap()
            .to_string();

        let memfile = state.join("gc-memory.bin");
        {
            let f = std::fs::File::create(&memfile).unwrap();
            let mut blk = vec![0u8; 8 << 20];
            for (i, b) in blk.iter_mut().enumerate() {
                *b = (i as u8).wrapping_mul(7) ^ (std::process::id() as u8);
            }
            f.write_all_at(&blk, 0).unwrap();
            f.sync_all().unwrap();
        }
        let mem = lg
            .ctl_json(&[
                "seal-file",
                &memfile.display().to_string(),
                "--upload",
                "--chunk-size",
                "262144",
            ])
            .expect("seal-file --upload")["layer_id"]
            .as_str()
            .unwrap()
            .to_string();

        // The head the caller would publish at hibernation time.
        let mut head = disk_store::head::PodHead::load(&lg.state, POD).expect("local head.json");
        assert!(head.logical_size > 0 && head.chunk_size > 0, "geometry");
        head.memory = Some(disk_store::head::MemoryRef {
            layer: mem.clone(),
            vmstate_key: disk_store::head::vmstate_key(POD, 1),
            epoch: 1,
            fc_version: "lab".into(),
            cpu_model: "lab".into(),
            kernel_release: "lab".into(),
        });
        let head_key = disk_store::head::head_key(POD, 1);
        rt.block_on(async {
            s3.put_object(&head_key, head.encode().unwrap())
                .await
                .unwrap();
            s3.put_object(&disk_store::head::vmstate_key(POD, 1), vec![7u8; 4096])
                .await
                .unwrap();
        });

        // Who owns what, computed from the manifests themselves.
        let l0_hexes = hexes_of(&lg.load_layer_manifest(&l0));
        let private_hexes = hexes_of(&lg.load_layer_manifest(&private));
        let mem_hexes = hexes_of(&lg.load_layer_manifest(&mem));
        let doomed: HashSet<String> = private_hexes
            .union(&mem_hexes)
            .cloned()
            .collect::<HashSet<_>>()
            .difference(&l0_hexes)
            .cloned()
            .collect();
        assert!(
            !doomed.is_empty(),
            "the pod must own chunks the bake does not"
        );

        // ---- 1. the pod is ALIVE: the sweep must not touch it ---
        let alive = lg
            .ctl_json(&["sweep", "--no-grace", "--layer", &l0, "--head", &head_key])
            .expect("sweep (alive)");
        assert_eq!(alive["snapshot_objects_deleted"].as_u64(), Some(0));
        assert_eq!(alive["layers_deleted"].as_u64(), Some(0));
        assert_eq!(
            alive["chunks_deleted"].as_u64(),
            Some(0),
            "a live head's chain and memory layer keep every chunk: {alive:#}"
        );

        // ---- 2. the caller deleted the pod: head no longer live ----
        // --dry-run first: the plan must already be exact.
        let dry = lg
            .ctl_json(&["sweep", "--no-grace", "--layer", &l0, "--dry-run"])
            .expect("sweep --dry-run");
        let planned: HashSet<String> = dry["deleted_keys"]
            .as_array()
            .unwrap()
            .iter()
            .map(|v| v.as_str().unwrap().to_string())
            .collect();
        for hex in &doomed {
            assert!(
                planned.contains(&chunk_key(hex)),
                "dry run misses the pod's own chunk {hex}"
            );
        }
        for hex in &l0_hexes {
            assert!(
                !planned.contains(&chunk_key(hex)),
                "dry run would delete a chunk the bake references ({hex})"
            );
        }
        assert!(planned.contains(&head_key), "dry run keeps the dead head");
        assert!(planned.contains(&format!("layers/{private}.manifest")));
        assert!(planned.contains(&format!("layers/{mem}.manifest")));
        assert!(
            !planned.contains(&format!("layers/{l0}.manifest")),
            "dry run would delete the catalog's own layer"
        );

        // Nothing actually went yet.
        rt.block_on(async {
            assert!(!s3.list("snapshots/").await.unwrap().is_empty());
        });

        // ---- 3. for real ---------------------------------------------
        let done = lg
            .ctl_json(&["sweep", "--no-grace", "--layer", &l0])
            .expect("sweep");
        println!("sweep: {done:#}");
        assert_eq!(
            done["snapshot_objects_deleted"].as_u64(),
            Some(2),
            "head.json + vmstate"
        );
        assert_eq!(done["layers_deleted"].as_u64(), Some(2), "private + memory");
        assert!(done["bytes_deleted"].as_u64().unwrap() > 0);

        let (snaps, layers, chunks) = rt.block_on(async {
            (
                s3.list("snapshots/").await.unwrap(),
                s3.list("layers/").await.unwrap(),
                s3.list("chunks/").await.unwrap(),
            )
        });
        assert!(snaps.is_empty(), "snapshots/ not empty: {snaps:?}");
        let layer_keys: HashSet<String> = layers.into_iter().map(|o| o.key).collect();
        assert_eq!(
            layer_keys,
            HashSet::from([format!("layers/{l0}.manifest")]),
            "exactly the catalog's layer survives"
        );
        let chunk_keys: HashSet<String> = chunks.into_iter().map(|o| o.key).collect();
        for hex in &l0_hexes {
            assert!(
                chunk_keys.contains(&chunk_key(hex)),
                "the bake lost a chunk it references ({hex})"
            );
        }
        for hex in &doomed {
            assert!(
                !chunk_keys.contains(&chunk_key(hex)),
                "an exclusively-owned chunk survived the sweep ({hex})"
            );
        }
        assert_eq!(
            chunk_keys.len(),
            l0_hexes.len(),
            "nothing but the bake's chunks is left"
        );

        // ---- 4. the upload markers followed the objects --------------
        // A deleted chunk whose marker stayed would make the next Push
        // skip a PUT the bucket needs (the stale-marker trap).
        for hex in &doomed {
            let marker = state.join("uploaded/chunks").join(&hex[0..2]).join(hex);
            assert!(!marker.exists(), "stale upload marker for {hex}");
        }

        // A second sweep is a no-op (idempotent).
        let again = lg
            .ctl_json(&["sweep", "--no-grace", "--layer", &l0])
            .expect("sweep again");
        assert_eq!(again["snapshot_objects_deleted"].as_u64(), Some(0));
        assert_eq!(again["layers_deleted"].as_u64(), Some(0));
        assert_eq!(again["chunks_deleted"].as_u64(), Some(0));
    }));

    lg.cleanup(POD);
    let _ = daemon.kill();
    let _ = daemon.wait();
    rt.block_on(async {
        let _ = s3.delete_prefix().await;
    });
    if let Err(e) = result {
        std::panic::resume_unwind(e);
    }
}
