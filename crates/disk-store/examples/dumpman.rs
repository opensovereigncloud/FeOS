// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

fn main() {
    for f in std::env::args().skip(1) {
        let buf = std::fs::read(&f).unwrap();
        let m = disk_store::Manifest::decode(&buf).unwrap();
        let parent = match m.parent {
            Some(p) => p.hex(),
            None => "none".into(),
        };
        println!(
            "{}\n  id={} parent={} entries={}",
            f.rsplit('/').next().unwrap_or(&f),
            m.layer_id(),
            parent,
            m.entries.len()
        );
    }
}
