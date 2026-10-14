// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Lab: user recovery. `hack/diskd/lab-drill-recovery` reads through a
//! device under fio, kills the daemon with SIGKILL, starts it again and
//! checks that fio saw no error.

mod common;

#[test]
#[ignore = "lab: needs root, ublk and fio (make diskd-lab)"]
fn lab_recovery_read() {
    if !common::ensure_lab_daemon() {
        return;
    }
    let l = common::Lab::new();
    let out = match l.run_hack("lab-drill-recovery", &["lab-rec"]) {
        Ok(o) => o,
        Err(e) => panic!("drill failed:\n{e}"),
    };
    assert!(out.contains("[drill] PASS"), "drill did not pass:\n{out}");
    l.cleanup("lab-rec");
}
