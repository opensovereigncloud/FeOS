// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! For tests: a write that hangs, as an fsync behind a snapshot's memory
//! does. Its file is a FIFO that no one reads, whose open(2) waits for a
//! reader; [`Hung::open`] lets it go, and the write then fails, as fsync(2)
//! of a FIFO does.

use std::path::PathBuf;

pub(crate) struct Hung(pub PathBuf);

impl Hung {
    pub(crate) fn at(path: PathBuf) -> Self {
        nix::unistd::mkfifo(&path, nix::sys::stat::Mode::S_IRWXU).unwrap();
        Self(path)
    }

    /// Lets the write go: reads what it writes, on a thread of its own.
    pub(crate) fn open(&self) {
        let path = self.0.clone();
        std::thread::spawn(move || {
            let mut reader = std::fs::File::open(path).unwrap();
            let _ = std::io::copy(&mut reader, &mut std::io::sink());
        });
    }
}
