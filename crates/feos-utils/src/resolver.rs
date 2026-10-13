// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The resolver configuration a host pushes to its guest.
//!
//! FeOS writes the pushed resolvers to the guest's /etc/resolv.conf and to
//! /run/feos/resolv.conf, which every container's /etc/resolv.conf links to.
//! Each update writes a complete temporary file and renames it into place,
//! so a reader sees either the old or the new file, never a partial one.

use std::fs::OpenOptions;
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::Mutex;

/// The directory of the resolver file that containers link to.
pub const SHARED_DIR: &str = "/run/feos";
/// The file name, in /etc and in [`SHARED_DIR`].
pub const FILE_NAME: &str = "resolv.conf";
/// The fixed temporary name in the target directory. Updates are serialized,
/// so one name is enough; a leftover from a crash is removed first.
const TMP_NAME: &str = ".feos-resolv.conf.tmp";

static UPDATE: Mutex<()> = Mutex::new(());

/// The resolv.conf content for `nameservers`, in order.
pub fn render(nameservers: &[String]) -> String {
    let mut content = String::from("# Pushed by the FeOS host. Do not edit.\n");
    for ns in nameservers {
        content.push_str("nameserver ");
        content.push_str(ns);
        content.push('\n');
    }
    content
}

/// Writes `nameservers` to `<dir>/resolv.conf` for each of `dirs`, atomically
/// per file. Stops at the first directory that fails.
pub fn publish(nameservers: &[String], dirs: &[&Path]) -> io::Result<()> {
    let _guard = UPDATE.lock().unwrap_or_else(|e| e.into_inner());
    let content = render(nameservers);
    for dir in dirs {
        std::fs::create_dir_all(dir)?;
        replace(dir, FILE_NAME, content.as_bytes()).map_err(|e| {
            io::Error::new(e.kind(), format!("{}: {e}", dir.join(FILE_NAME).display()))
        })?;
    }
    Ok(())
}

/// Replaces `<dir>/<name>` with a complete file holding `content`.
fn replace(dir: &Path, name: &str, content: &[u8]) -> io::Result<()> {
    let tmp = dir.join(TMP_NAME);
    match std::fs::remove_file(&tmp) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
        _ => {}
    }
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o644)
            .open(&tmp)?;
        file.write_all(content)?;
        std::fs::rename(&tmp, dir.join(name))
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn renders_nameservers_in_order() {
        let content = render(&["10.0.0.1".to_string(), "10.0.0.2".to_string()]);
        assert!(content.ends_with("nameserver 10.0.0.1\nnameserver 10.0.0.2\n"));
    }

    #[test]
    fn publishes_to_every_directory_and_replaces() {
        let a = tempfile::tempdir().unwrap();
        let b = tempfile::tempdir().unwrap();
        publish(&["1.1.1.1".to_string()], &[a.path(), b.path()]).unwrap();
        publish(&["9.9.9.9".to_string()], &[a.path(), b.path()]).unwrap();
        for dir in [a.path(), b.path()] {
            let content = std::fs::read_to_string(dir.join(FILE_NAME)).unwrap();
            assert!(content.ends_with("nameserver 9.9.9.9\n"));
            assert!(!dir.join(TMP_NAME).exists());
        }
    }
}
