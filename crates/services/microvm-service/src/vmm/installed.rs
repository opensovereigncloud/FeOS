// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Firecracker and the guest as their packages install them, side by side
//! (ADR 0009): a directory per version, as kernel packages do. A boot takes
//! the newest; a VM keeps what it was booted with, so that a version
//! installed later changes no VM that runs or is suspended.

use std::cmp::Ordering;
use std::path::{Path, PathBuf};

/// `<version>/firecracker` and `<version>/jailer`.
pub const FIRECRACKER_DIR: &str = "/usr/lib/feos/firecracker";
/// `<version>/vmlinuz` and `<version>/initramfs`, and the blank data
/// volume `<version>/data.img.zst`.
pub const GUEST_DIR: &str = "/usr/lib/feos/guest";

/// The newest installed Firecracker's directory.
pub fn newest_firecracker() -> Option<PathBuf> {
    newest(Path::new(FIRECRACKER_DIR), &["firecracker", "jailer"])
}

/// The newest installed guest's directory, laid out as an image's.
pub fn newest_guest() -> Option<PathBuf> {
    newest(Path::new(GUEST_DIR), &["vmlinuz", "initramfs"])
}

/// The directory of the installed Firecracker `version`, as Firecracker
/// reports its version: what a snapshot of it needs.
pub fn firecracker(version: &str) -> Option<PathBuf> {
    version_dir(
        Path::new(FIRECRACKER_DIR),
        version,
        &["firecracker", "jailer"],
    )
}

/// The guest's blank data volume, which a pod's disk starts from: a
/// zstd-compressed ext4 image, in the guest's directory.
pub const BLANK_DATA_VOLUME: &str = "data.img.zst";

/// The directory of `version` in `dir`, if it has all of `files`.
fn version_dir(dir: &Path, version: &str, files: &[&str]) -> Option<PathBuf> {
    let candidate = dir.join(version);
    let plain = !version.is_empty() && !version.contains('/') && version != "..";
    (plain && files.iter().all(|file| candidate.join(file).is_file())).then_some(candidate)
}

/// The directory of the newest version in `dir` that has all of `files`.
fn newest(dir: &Path, files: &[&str]) -> Option<PathBuf> {
    std::fs::read_dir(dir)
        .ok()?
        .flatten()
        .map(|entry| entry.path())
        .filter(|path| files.iter().all(|file| path.join(file).is_file()))
        .max_by(|a, b| compare_versions(&name(a), &name(b)))
}

fn name(path: &Path) -> String {
    path.file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// Orders versions by their numbers, as dpkg does in the common cases: a
/// part after `~` is a pre-release and comes before the release.
fn compare_versions(a: &str, b: &str) -> Ordering {
    let key = |version: &str| {
        let (release, pre) = match version.split_once('~') {
            Some((release, pre)) => (release, Some(pre)),
            None => (version, None),
        };
        let numbers: Vec<u64> = release
            .split(['.', '-', '+'])
            .map(|part| {
                let digits: String = part.chars().take_while(char::is_ascii_digit).collect();
                digits.parse().unwrap_or(0)
            })
            .collect();
        (numbers, pre.is_none())
    };
    key(a).cmp(&key(b)).then_with(|| a.cmp(b))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn versions_compare_by_their_numbers() {
        assert_eq!(compare_versions("1.17.0", "1.9.2"), Ordering::Greater);
        assert_eq!(compare_versions("0.6.0~rc1", "0.6.0"), Ordering::Less);
        assert_eq!(compare_versions("0.6.0", "0.6.0"), Ordering::Equal);
        assert_eq!(compare_versions("0.10.0", "0.6.12"), Ordering::Greater);
    }

    #[test]
    fn the_newest_complete_version_is_found() {
        let dir = tempfile::tempdir().unwrap();
        for (version, files) in [
            ("1.9.0", &["firecracker", "jailer"][..]),
            ("1.17.0", &["firecracker", "jailer"][..]),
            // Half installed: no jailer.
            ("1.18.0", &["firecracker"][..]),
        ] {
            let version = dir.path().join(version);
            std::fs::create_dir(&version).unwrap();
            for file in files {
                std::fs::write(version.join(file), b"").unwrap();
            }
        }
        assert_eq!(
            newest(dir.path(), &["firecracker", "jailer"]),
            Some(dir.path().join("1.17.0"))
        );
        assert_eq!(newest(&dir.path().join("none"), &["x"]), None);

        // A snapshot needs its own version, not the newest.
        let files = ["firecracker", "jailer"];
        assert_eq!(
            version_dir(dir.path(), "1.9.0", &files),
            Some(dir.path().join("1.9.0"))
        );
        for missing in ["1.18.0", "1.10.0", "", "..", "../1.9.0"] {
            assert_eq!(version_dir(dir.path(), missing, &files), None, "{missing}");
        }
    }
}
