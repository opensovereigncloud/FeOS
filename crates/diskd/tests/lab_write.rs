// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: write round trip + discard-as-zero-write (no parent
//! resurrection) through the seal path.

mod common;

use common::Lab;

// The lab base image is 64 MiB; that is the scan window.
fn image_allocated_limit_default() -> u64 {
    64 << 20
}

fn dd_random_at(dev: &str, off_mb: u64, payload_mb: u64) {
    let st = std::process::Command::new("dd")
        .arg("if=/dev/urandom")
        .arg("oflag=direct")
        .arg("conv=fsync")
        .arg(format!("of={dev}"))
        .arg("bs=1M")
        .arg(format!("seek={off_mb}"))
        .arg(format!("count={payload_mb}"))
        .output()
        .expect("dd");
    assert!(
        st.status.success(),
        "dd: {}",
        String::from_utf8_lossy(&st.stderr)
    );
}

#[test]
#[ignore = "lab: needs root and ublk (make diskd-lab)"]
fn lab_open_roundtrip() {
    if !common::ensure_lab_daemon() {
        return;
    }
    let l = Lab::new();
    const POD: &str = "lab-oc";

    let img = l.base_image();
    let out = l
        .ctl_json(&["import-base", &img.display().to_string()])
        .expect("import-base");
    let l0 = out["layer_id"].as_str().unwrap().to_string();

    l.cleanup(POD);
    let _ = std::process::Command::new("sudo")
        .args(["rm", "-f"])
        .arg(l.state.join("live").join(POD).join("top.img"))
        .output();
    let dev = l.ctl_json(&["create", POD, &l0]).expect("create");
    let link = dev["link_path"].as_str().unwrap().to_string();

    // write a marker, flush, then stop→open: same link path, same bytes.
    dd_random_at(&link, 24, 4);
    let got_before = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&link).unwrap();
        let mut b = vec![0u8; 1 << 20];
        f.read_exact_at(&mut b, 24 << 20).unwrap();
        b
    };
    l.stop(POD);
    let _ = l.ctl_json(&["open", POD]).expect("open");
    let dev2 = l.ctl_json(&["stats", POD]).expect("stats");
    let link2 = dev2["devices"][0]["dev_path"].as_str().unwrap().to_string();
    assert_eq!(
        link,
        format!("{}/live/{}/disk", l.state.display(), POD),
        "link path stable across stop/open"
    );
    assert!(!link2.is_empty());
    let got_after = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&link).unwrap();
        let mut b = vec![0u8; 1 << 20];
        f.read_exact_at(&mut b, 24 << 20).unwrap();
        b
    };
    assert_eq!(
        blake3::hash(&got_before),
        blake3::hash(&got_after),
        "write not preserved across stop/open"
    );
    l.cleanup(POD);
    println!("lab_open_roundtrip PASS");
}

#[test]
#[ignore = "lab: needs root and ublk (make diskd-lab)"]
fn lab_discard_no_resurrection() {
    if !common::ensure_lab_daemon() {
        return;
    }
    let l = Lab::new();
    const POD: &str = "lab-discard";

    let img = l.base_image();
    let out = l
        .ctl_json(&["import-base", &img.display().to_string()])
        .expect("import-base");
    let l0 = out["layer_id"].as_str().unwrap().to_string();

    l.cleanup(POD);
    let _ = std::process::Command::new("sudo")
        .args(["rm", "-f"])
        .arg(l.state.join("live").join(POD).join("top.img"))
        .output();
    let dev = l.ctl_json(&["create", POD, &l0]).expect("create");
    let link = dev["link_path"].as_str().unwrap().to_string();

    // Find a 4 MiB-aligned, actually-allocated window of the L0 image
    // (sparse tails read as zeros — cannot serve as the parent-data
    // witness). Scan the source image directly.
    let len = 4 << 20u64;
    let off = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&img).unwrap();
        let img_len = f.metadata().unwrap().len();
        let mut found = None;
        let mut b = vec![0u8; len as usize];
        let end = (img_len - len).min(image_allocated_limit_default());
        let mut o = 0u64;
        while o + len <= end {
            f.read_exact_at(&mut b, o).unwrap();
            if b.iter().any(|x| *x != 0) {
                found = Some(o);
                break;
            }
            o += len;
        }
        found.expect("no non-zero 4MiB window in the base image")
    };
    let before = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&link).unwrap();
        let mut b = vec![0u8; len as usize];
        f.read_exact_at(&mut b, off).unwrap();
        b
    };
    assert!(
        before.iter().any(|b| *b != 0),
        "test region should be non-zero"
    );

    // Discard (fstrim's primitive at the host level) → explicit zeros.
    // (Locate blkdiscard without depending on sudo's secure_path.)
    let blkdiscard = ["/usr/sbin/blkdiscard", "/sbin/blkdiscard", "blkdiscard"]
        .iter()
        .copied()
        .find(|p| std::path::Path::new(p).exists())
        .unwrap_or("blkdiscard");
    let st = std::process::Command::new(blkdiscard)
        .args(["-f", "-o", &off.to_string(), "-l", &len.to_string(), &link])
        .output()
        .expect("blkdiscard");
    assert!(
        st.status.success(),
        "blkdiscard failed: {}",
        String::from_utf8_lossy(&st.stderr)
    );

    let after = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&link).unwrap();
        let mut b = vec![0u8; len as usize];
        f.read_exact_at(&mut b, off).unwrap();
        b
    };
    assert!(
        after.iter().all(|b| *b == 0),
        "discard did not zero the range"
    );

    // Seal; the new layer must mark the discarded chunks `zero` (never
    // resurrect the parent), and materialize must show zeros there.
    l.stop(POD);
    let seal = l.ctl_json(&["seal", POD]).expect("seal");
    let new_layer = seal["layer_id"].as_str().unwrap().to_string();
    let man_path = l.state.join("layers").join(format!("{new_layer}.manifest"));
    let man = disk_store::Manifest::decode(&std::fs::read(&man_path).unwrap()).unwrap();
    // A discarded chunk must never resurrect parent data. Sealed entries:
    //   Some(Zero)  — required exactly where the parent chunk carries data
    //   None        — fine where the parent chunk is zero/absent anyway
    let cs = man.chunk_size as u64;
    let mut zero_seen = 0u64;
    let mut parent_with_data = 0u64;
    for ci in (off / cs)..((off + len - 1) / cs + 1) {
        let parent = {
            use std::os::unix::fs::FileExt;
            let f = std::fs::File::open(&img).unwrap();
            let mut b = vec![0u8; cs as usize];
            f.read_exact_at(&mut b, ci * cs).unwrap();
            b
        };
        let parent_nonzero = parent.iter().any(|&b| b != 0);
        match man.entry_at(ci * cs) {
            Some(e) if e.kind == disk_store::manifest::EntryKind::Zero => {
                zero_seen += 1;
                assert!(
                    parent_nonzero,
                    "surplus explicit zero entry at chunk {ci} over zero/absent parent"
                );
            }
            other => {
                assert!(
                    other.is_none() && !parent_nonzero,
                    "chunk {ci} in sealed layer is {:?}, expected explicit zero                      because parent has data",
                    other.map(|e| e.kind)
                );
            }
        }
        if parent_nonzero {
            parent_with_data += 1;
        }
    }
    println!("sealed {zero_seen} zero entries ({parent_with_data} parent chunks have data)");
    assert_eq!(zero_seen, parent_with_data);

    // Materialize the full chain w/ new layer: the discarded region must
    // now be zeros — the parent's data must NOT resurrect.
    let out_path = l.state.join(format!("live/{POD}/materialized.img"));
    let _ = std::process::Command::new("sudo")
        .args(["rm", "-f"])
        .arg(&out_path)
        .output();
    let out = l.ctl(&[
        "materialize",
        &out_path.display().to_string(),
        &l0,
        &new_layer,
    ]);
    assert!(
        out.status.success(),
        "materialize: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    let materialized = {
        use std::os::unix::fs::FileExt;
        let f = std::fs::File::open(&out_path).unwrap();
        let mut b = vec![0u8; len as usize];
        f.read_exact_at(&mut b, off).unwrap();
        b
    };
    assert!(
        materialized.iter().all(|b| *b == 0),
        "materialized chain resurrected parent data after discard!"
    );
    l.cleanup(POD);
    println!("lab_discard_no_resurrection PASS");
}
