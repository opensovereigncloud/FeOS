// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: `OpenDisk{head_key}` — restore a pod sealed on
//! "node A" (the shared lab daemon) on "node B" (hermetic daemon, empty
//! state dir), driven by the S3 key of its head.json.
//!
//! The head is uploaded by the test process itself, standing in for the
//! caller that uploads a head per epoch: seal on A commits the local head.json, the test
//! PUTs it to `snapshots/<id>/1/head.json`, then B opens the device from
//! that key and reads the marker — lazily over S3.

mod common;

use std::os::unix::fs::FileExt;
use std::os::unix::fs::OpenOptionsExt;

use common::Lab;

const POD: &str = "lab-head";

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_open_from_head() {
    if !common::ensure_lab_s3() {
        return;
    }
    let l = Lab::new();

    // Marker-bearing disk on the shared daemon.
    let img = l.root.join("target/diskd/imported/head-base.raw");
    std::fs::create_dir_all(img.parent().unwrap()).unwrap();
    {
        let f = std::fs::File::create(&img).unwrap();
        f.set_len(64 << 20).unwrap();
        f.write_all_at(&[0x55u8; 1 << 20], 0).unwrap();
    }
    let out = l
        .ctl_json(&["import-base", &img.display().to_string()])
        .expect("import-base");
    let l0 = out["layer_id"].as_str().unwrap().to_string();

    l.cleanup(POD);
    let dev = l.ctl_json(&["create", POD, &l0]).expect("create");
    let link = dev["link_path"].as_str().unwrap().to_string();
    let marker = vec![0xabu8; 1 << 20];
    {
        let f = std::fs::OpenOptions::new()
            .write(true)
            .custom_flags(libc::O_DIRECT)
            .open(&link)
            .unwrap();
        f.write_all_at(&marker, 32 << 20).unwrap();
        f.sync_all().unwrap();
    }
    l.stop(POD);
    let seal = l
        .ctl_json(&["seal", POD, "--upload"])
        .expect("seal --upload");
    let s = seal["layer_id"].as_str().unwrap().to_string();
    println!("sealed: {s}");

    // Seal committed the local head.json (proto contract). Upload it as
    // the epoch-1 head — the caller's job, the test doubles for it here.
    let head = disk_store::head::PodHead::load(&l.state, POD).expect("local head.json");
    assert_eq!(head.chain.last().unwrap(), &s, "head chain ends at S");
    assert!(!head.has_top);
    let key = disk_store::head::head_key(POD, 1);
    let s3 = disk_store::s3::S3::from_env("")
        .expect("s3 env")
        .expect("s3 configured");
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    rt.block_on(async {
        s3.put_object(&key, head.encode().unwrap()).await.unwrap();
    });
    println!("head uploaded at {key}");

    // Node B: empty state dir, open from the head key.
    let state_b = l.root.join("target/diskd/state-head");
    let (lb, mut daemon_b) = l.spawn_daemon(&state_b, &[]);
    let o = lb.ctl(&["open", POD, "--head", &key]);
    assert!(
        o.status.success(),
        "open --head: {}",
        String::from_utf8_lossy(&o.stderr)
    );
    let dev_b: serde_json::Value = serde_json::from_slice(&o.stdout).unwrap();
    let link_b = dev_b["link_path"].as_str().unwrap().to_string();

    let got = {
        let f = std::fs::File::open(&link_b).unwrap();
        let mut b = vec![0u8; 1 << 20];
        f.read_exact_at(&mut b, 32 << 20).unwrap();
        b
    };
    assert_eq!(got, marker, "restored disk content mismatch");

    let st = lb.ctl_json(&["stats", POD]).expect("stats on B");
    let fetches = st["devices"][0]["s3_fetches"].as_u64().unwrap_or(0);
    assert!(fetches > 0, "restore made no lazy S3 fetches");
    // Manifests local + marked uploaded on B.
    for lay in head.chain.iter() {
        assert!(lb
            .state
            .join("layers")
            .join(format!("{lay}.manifest"))
            .is_file());
        assert!(lb.state.join("uploaded").join(lay).is_file());
    }

    lb.cleanup(POD);
    let _ = daemon_b.kill();
    let _ = daemon_b.wait();
    let _ = std::fs::remove_dir_all(&state_b);
    l.cleanup(POD);
    rt.block_on(async {
        let _ = s3.delete_object_key(&key).await;
    });
    println!("lab_open_from_head PASS (lazy fetches={fetches})");
}
