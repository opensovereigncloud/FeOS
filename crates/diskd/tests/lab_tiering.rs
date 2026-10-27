// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: seal-on-pause with upload, plus cross-node resume —
//! a second diskd daemon with an empty state dir (mock "node 2") pulls
//! the sealed chain, serves reads lazily over S3, byte-checks, and a
//! warm pull then serves locally with no further S3 traffic.

mod common;

use common::Lab;

#[test]
#[ignore = "lab: needs root, ublk and S3 (make diskd-lab)"]
fn lab_cross_node_resume() {
    if !common::ensure_lab_s3() {
        return;
    }
    let l = Lab::new();
    const POD: &str = "lab-sync";
    const POD2: &str = "lab-sync2";

    let img = l.base_image();
    let out = l
        .ctl_json(&["import-base", &img.display().to_string()])
        .expect("import-base");
    let l0 = out["layer_id"].as_str().unwrap().to_string();

    l.cleanup(POD);
    let dev = l.ctl_json(&["create", POD, &l0]).expect("create");
    let link = dev["link_path"].as_str().unwrap().to_string();
    let marker = [0xabu8; 1 << 20];
    {
        use std::os::unix::fs::FileExt;
        use std::os::unix::fs::OpenOptionsExt;
        let f = std::fs::OpenOptions::new()
            .write(true)
            .custom_flags(libc::O_DIRECT)
            .open(&link)
            .unwrap();
        let buf = vec![0xabu8; 1 << 20];
        f.write_all_at(&buf, 32 << 20).unwrap();
        f.sync_all().unwrap();
    }

    l.stop(POD);
    let seal = l
        .ctl_json(&["seal", POD, "--upload"])
        .expect("seal --upload");
    let top_layer = seal["layer_id"].as_str().unwrap().to_string();
    assert_ne!(top_layer, l0);
    assert!(
        l.state.join("uploaded").join(&top_layer).is_file(),
        "uploaded marker missing after seal --upload"
    );

    // ---- node 2: fresh daemon, empty state dir ------------------------
    let state2 = l.root.join("target/diskd/state2");
    let _ = std::process::Command::new("sudo")
        .args(["rm", "-rf"])
        .arg(&state2)
        .output();
    std::fs::create_dir_all(&state2).unwrap();
    let log2 = std::fs::File::create(state2.join("diskd.log")).unwrap();
    let diskd_bin = l.root.join("target/release/diskd");
    let node2 = std::process::Command::new(&diskd_bin)
        .args(["--state-dir"])
        .arg(&state2)
        .stdout(std::process::Stdio::from(log2.try_clone().unwrap()))
        .stderr(std::process::Stdio::from(log2))
        .spawn()
        .expect("spawn node2 daemon");
    let _ = &node2;

    let sock2 = state2.join("diskd.sock");
    for _ in 0..300 {
        let o = std::process::Command::new(&l.ctl)
            .args(["--socket", &sock2.display().to_string(), "stats"])
            .output();
        if o.map(|o| o.status.success()).unwrap_or(false) {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    assert!(sock2.exists(), "node2 daemon never created its socket");

    // node 2: pull the sealed layer (cold — manifests only), create,
    // read the marker region: bytes must come over S3 lazily.
    let l2 = common::Lab {
        root: l.root.clone(),
        diskd: l.diskd.clone(),
        state: state2.clone(),
        socket: sock2.clone(),
        ctl: l.ctl.clone(),
    };
    let out = l2.ctl(&["pull", &top_layer]);
    assert!(
        out.status.success(),
        "pull failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    // chain manifests are now local
    assert!(state2
        .join("layers")
        .join(format!("{top_layer}.manifest"))
        .is_file());
    assert!(state2
        .join("layers")
        .join(format!("{l0}.manifest"))
        .is_file());

    let dev2 = l2
        .ctl_json(&["create", POD2, &l0, &top_layer])
        .expect("node2 create");
    let link2 = dev2["link_path"].as_str().unwrap().to_string();
    let got = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&link2).unwrap();
        let mut b = vec![0u8; 1 << 20];
        f.read_exact_at(&mut b, 32 << 20).unwrap();
        b
    };
    assert_eq!(got, marker, "cross-node marker bytes mismatch");

    let st2 = l2.ctl_json(&["stats", POD2]).expect("stats node2");
    let fetches = st2["devices"][0]["s3_fetches"].as_u64().unwrap_or(0);
    assert!(fetches > 0, "cold start made no S3 fetches");

    // ---- warm pull: drop the pod + cache, pull --warm -------------
    l2.cleanup(POD2);
    // kill daemon, erase chunk cache (keep manifests), restart, warmup
    let mut node2 = node2;
    unsafe {
        libc::kill(node2.id() as i32, libc::SIGTERM);
    }
    let _ = node2.wait();
    let _ = std::process::Command::new("sudo")
        .args(["rm", "-rf"])
        .arg(state2.join("cache"))
        .output();
    std::fs::create_dir_all(state2.join("cache")).unwrap();

    let log3 = std::fs::File::create(state2.join("diskd2.log")).unwrap();
    let node2b = std::process::Command::new(&diskd_bin)
        .args(["--state-dir"])
        .arg(&state2)
        .stdout(std::process::Stdio::from(log3.try_clone().unwrap()))
        .stderr(std::process::Stdio::from(log3))
        .spawn()
        .expect("respawn node2");
    for _ in 0..300 {
        let o = std::process::Command::new(&l.ctl)
            .args(["--socket", &sock2.display().to_string(), "stats"])
            .output();
        if o.map(|o| o.status.success()).unwrap_or(false) {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    let out = l2.ctl(&["pull", &top_layer, "--warm"]);
    assert!(
        out.status.success(),
        "warm pull failed: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    // After the warm pull every data chunk is cached locally.
    let cache_stats: serde_json::Value = serde_json::Value::Null; // keep for shape
    let _ = cache_stats;
    let dev3 = l2
        .ctl_json(&["create", POD2, &l0, &top_layer])
        .expect("node2 create after warm pull");
    let link3 = dev3["link_path"].as_str().unwrap().to_string();
    // mark the region via cold reads; then a second read must not fetch
    let first = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&link3).unwrap();
        let mut b = vec![0u8; 1 << 20];
        f.read_exact_at(&mut b, 32 << 20).unwrap();
        b
    };
    assert_eq!(first, marker);
    let st3 = l2.ctl_json(&["stats", POD2]).expect("stats warm");
    let warm_fetches = st3["devices"][0]["s3_fetches"].as_u64().unwrap_or(0);
    let hits = st3["devices"][0]["cache_hits"].as_u64().unwrap_or(0);
    assert_eq!(warm_fetches, 0, "warm-pulled device still fetches from S3");
    assert!(hits > 0);

    l2.cleanup(POD2);
    let mut node2b = node2b;
    unsafe {
        libc::kill(node2b.id() as i32, libc::SIGTERM);
    }
    let _ = node2b.wait();
    l.cleanup(POD);
    println!("lab_cross_node_resume PASS (cold fetches={fetches}, warm hits={hits})");
}
