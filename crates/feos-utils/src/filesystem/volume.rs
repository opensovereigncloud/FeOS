// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's volumes in its guest (ADR 0016): each an ext4 filesystem on a
//! virtio disk of its own, whose UUID is the volume's ID, mounted at
//! [`VOLUMES_DIR`]`/<name>`, from where containers bind-mount it.
//!
//! The host says when a volume is new; the guest then formats it with
//! [`MKE2FS`], the one tool FeOS runs. The guest still refuses to format
//! over a filesystem that was ever mounted: if it is the volume's own,
//! the host's record that it is new is stale, and the guest mounts it as
//! it is; anything else is an error. A filesystem whose superblock says it
//! was never mounted is what an interrupted `mke2fs` leaves, and is
//! formatted over.

use log::info;
use nix::mount::{mount, MsFlags};
use std::io;
use std::os::unix::fs::{FileExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::process::Stdio;

/// Where the guest mounts its volumes, a directory per name: in the
/// guest's memory, not on the pod's disk.
pub const VOLUMES_DIR: &str = "/run/feos/volumes";
/// The guest's `mke2fs`: static, from e2fsprogs, in its initramfs.
pub const MKE2FS: &str = "/sbin/mke2fs";
/// How much of a disk [`probe`] looks at: btrfs keeps its superblock at
/// 64 KiB.
pub const PROBE_BYTES: usize = 0x11000;
/// The mount options of a volume. Its disk reads zeroes where nothing was
/// written, so the inode tables that `mke2fs` left to the kernel are
/// zeroes already: the kernel need not write them.
const MOUNT_OPTIONS: &str = "noinit_itable";

/// Where the volume `name` is mounted in the guest.
pub fn mount_point(name: &str) -> PathBuf {
    Path::new(VOLUMES_DIR).join(name)
}

/// A volume's name in its pod: 1 to 63 lowercase letters, digits and "-",
/// beginning and ending with a letter or a digit, as a DNS label.
pub fn valid_name(name: &str) -> bool {
    let alphanumeric = |c: char| c.is_ascii_lowercase() || c.is_ascii_digit();
    !name.is_empty()
        && name.len() <= 63
        && name.chars().all(|c| alphanumeric(c) || c == '-')
        && name.starts_with(alphanumeric)
        && name.ends_with(alphanumeric)
}

/// A path a volume is mounted at in a container: absolute, without empty,
/// "." or ".." components, and not the container's root.
pub fn valid_mount_path(path: &str) -> bool {
    path.len() > 1
        && path.len() <= 4096
        && path.starts_with('/')
        && !path.contains('\0')
        && path[1..]
            .split('/')
            .all(|part| !part.is_empty() && part != "." && part != "..")
}

/// What is on a disk, as far as its first [`PROBE_BYTES`] tell.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Found {
    /// Zeroes: a disk nothing wrote to.
    Blank,
    /// Data, but no signature the guest knows.
    Unknown,
    /// An ext4 filesystem with this UUID, and whether it was ever mounted.
    Ext4 { uuid: String, mounted: bool },
    /// Another filesystem, a damaged ext4 superblock, a partition table
    /// or the like.
    Other(&'static str),
}

/// What to do with a volume's disk.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Plan {
    Format,
    /// Mount what is there: the volume's own filesystem.
    Mount,
}

/// What to do with a volume's disk that holds `found`: format it if the
/// host says it is new (`format`) and nothing on it was ever mounted; else
/// mount it if it holds the volume's own filesystem. Never formats a
/// filesystem that was mounted, and never mounts another volume's.
pub fn plan(found: &Found, volume_id: &str, format: bool) -> Result<Plan, String> {
    let own = |uuid: &str| uuid.eq_ignore_ascii_case(volume_id);
    match found {
        Found::Ext4 { uuid, .. } if !own(uuid) && !format => Err(format!(
            "the disk holds the filesystem {uuid}, not volume {volume_id}'s"
        )),
        Found::Ext4 { uuid, mounted } if !own(uuid) && *mounted => Err(format!(
            "the disk holds the filesystem {uuid}, which was mounted: it is not formatted over"
        )),
        // An interrupted mkfs, of this volume's or not.
        Found::Ext4 { mounted: false, .. } if format => Ok(Plan::Format),
        // The volume's own: formatted before, by a format whose
        // confirmation the host did not get.
        Found::Ext4 { .. } => Ok(Plan::Mount),
        Found::Other(what) => Err(format!("the disk holds {what}: it is not formatted over")),
        Found::Blank | Found::Unknown if format => Ok(Plan::Format),
        Found::Blank => Err("the disk is blank: the volume's filesystem is gone".to_string()),
        Found::Unknown => Err("the disk holds no filesystem the guest knows".to_string()),
    }
}

/// What the first [`PROBE_BYTES`] of a disk hold.
pub fn probe(head: &[u8]) -> Found {
    if let Some(found) = ext4(head) {
        return found;
    }
    let at = |offset: usize, magic: &[u8]| head.get(offset..offset + magic.len()) == Some(magic);
    let signatures: [(usize, &[u8], &'static str); 14] = [
        (0, b"XFSB", "an XFS filesystem"),
        (0x10040, b"_BHRfS_M", "a btrfs filesystem"),
        (0, b"LUKS\xba\xbe", "a LUKS header"),
        (0, b"SKUL\xba\xbe", "a LUKS header"),
        (0, b"hsqs", "a squashfs filesystem"),
        (4086, b"SWAPSPACE2", "swap space"),
        (4086, b"SWAP-SPACE", "swap space"),
        (3, b"NTFS    ", "an NTFS filesystem"),
        (3, b"EXFAT   ", "an exFAT filesystem"),
        (54, b"FAT", "a FAT filesystem"),
        (82, b"FAT32   ", "a FAT filesystem"),
        (512, b"EFI PART", "a GPT partition table"),
        (536, b"LVM2 001", "an LVM physical volume"),
        (0x8001, b"CD001", "an ISO 9660 filesystem"),
    ];
    if let Some((_, _, what)) = signatures
        .iter()
        .find(|(offset, magic, _)| at(*offset, magic))
    {
        return Found::Other(what);
    }
    if at(1024, &0xF2F5_2010u32.to_le_bytes()) {
        return Found::Other("an F2FS filesystem");
    }
    if at(1080, &0xEF53u16.to_le_bytes()) {
        return Found::Other("a damaged ext2, ext3 or ext4 superblock");
    }
    if at(510, &[0x55, 0xaa]) {
        return Found::Other("a partition table or a boot sector");
    }
    if head.iter().all(|&byte| byte == 0) {
        Found::Blank
    } else {
        Found::Unknown
    }
}

/// The ext4 superblock at 1024, if there is a valid one: its magic, a
/// geometry that makes sense, and its checksum where it has one.
fn ext4(head: &[u8]) -> Option<Found> {
    let sb = head.get(1024..2048)?;
    let u16_at = |offset: usize| u16::from_le_bytes([sb[offset], sb[offset + 1]]);
    let u32_at = |offset: usize| {
        u32::from_le_bytes([sb[offset], sb[offset + 1], sb[offset + 2], sb[offset + 3]])
    };
    if u16_at(0x38) != 0xEF53 {
        return None;
    }
    let geometry = u32_at(0x00) > 0 // inodes
        && u32_at(0x18) <= 6 // log2 of the block size over 1 KiB
        && u32_at(0x20) > 0 // blocks per group
        && u32_at(0x28) > 0; // inodes per group
    const METADATA_CSUM: u32 = 0x400;
    let checksummed = u32_at(0x64) & METADATA_CSUM != 0;
    if !geometry || (checksummed && crc32c(!0, &sb[..0x3FC]) != u32_at(0x3FC)) {
        return None;
    }
    // A mount writes its time and counts itself; mke2fs leaves both 0.
    let mounted = u32_at(0x2C) != 0 || sb[0x275] != 0 || u16_at(0x34) != 0;
    Some(Found::Ext4 {
        uuid: uuid_string(&sb[0x68..0x78]),
        mounted,
    })
}

/// A UUID as text: 8-4-4-4-12 lowercase hexadecimal digits.
fn uuid_string(bytes: &[u8]) -> String {
    let hex: String = bytes.iter().map(|byte| format!("{byte:02x}")).collect();
    format!(
        "{}-{}-{}-{}-{}",
        &hex[0..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..32]
    )
}

/// CRC-32C (Castagnoli) without the final inversion, as ext4 computes its
/// checksums.
fn crc32c(mut crc: u32, data: &[u8]) -> u32 {
    for &byte in data {
        crc ^= u32::from(byte);
        for _ in 0..8 {
            crc = if crc & 1 != 0 {
                (crc >> 1) ^ 0x82F6_3B78
            } else {
                crc >> 1
            };
        }
    }
    crc
}

/// Reads the start of a disk, as far as [`probe`] looks.
pub fn read_head(device: &str) -> io::Result<Vec<u8>> {
    let file = std::fs::File::open(device)?;
    let mut head = vec![0; PROBE_BYTES];
    let mut read = 0;
    while read < head.len() {
        match file.read_at(&mut head[read..], read as u64)? {
            0 => break,
            n => read += n,
        }
    }
    head.truncate(read);
    Ok(head)
}

/// Whether `target` is the root of a mount of `device`.
pub fn mounted_from(target: &Path, device: &str) -> bool {
    let (Ok(target), Ok(device)) = (std::fs::metadata(target), std::fs::metadata(device)) else {
        return false;
    };
    target.dev() == device.rdev()
}

/// Whether `path` is the root of a mount: its filesystem is not its
/// parent's.
pub fn is_mount_point(path: &Path) -> bool {
    let parent = path.parent().unwrap_or(Path::new("/"));
    match (std::fs::metadata(path), std::fs::metadata(parent)) {
        (Ok(path), Ok(parent)) => path.dev() != parent.dev(),
        _ => false,
    }
}

/// Makes the volume's filesystem on `device` with [`MKE2FS`]: ext4 with
/// 4 KiB blocks, none reserved, the volume's ID as its UUID, and its inode
/// tables and journal left as they are, zeroes, and no discards, which on
/// a pod's disk write zeroes.
pub async fn make_filesystem(device: &str, volume_id: &str) -> Result<(), String> {
    let mut command = std::process::Command::new(MKE2FS);
    command
        .args([
            "-q", "-F", "-t", "ext4", "-b", "4096", "-m", "0", "-U", volume_id,
        ])
        .args(["-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard"])
        .arg(device)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped());
    let mut child = command
        .spawn()
        .map_err(|e| format!("start {MKE2FS}: {e}"))?;
    let pid = child.id() as i32;
    let stderr = child.stderr.take();
    let output = tokio::task::spawn_blocking(move || {
        let mut output = String::new();
        if let Some(mut stderr) = stderr {
            let _ = io::Read::read_to_string(&mut stderr, &mut output);
        }
        output
    });
    // Where a reaper owns waitpid, as in PID 1, it has the exit code.
    let code = if crate::reaper::is_running() {
        crate::reaper::wait_for_exit(pid).await
    } else {
        tokio::task::spawn_blocking(move || child.wait())
            .await
            .map_err(|e| e.to_string())?
            .map_err(|e| e.to_string())?
            .code()
            .unwrap_or(-1)
    };
    let output = output.await.unwrap_or_default();
    if code != 0 {
        return Err(format!(
            "{MKE2FS} exited with {code}: {}",
            output.trim().replace('\n', "; ")
        ));
    }
    info!("Volumes: Made volume {volume_id}'s filesystem on {device}");
    Ok(())
}

/// Mounts the volume's filesystem on `device` at `target`, without device
/// files and setuid.
pub fn mount_volume(device: &str, target: &Path) -> Result<(), String> {
    std::fs::create_dir_all(target).map_err(|e| format!("{}: {e}", target.display()))?;
    mount(
        Some(device),
        target,
        Some("ext4"),
        MsFlags::MS_NOSUID | MsFlags::MS_NODEV,
        Some(MOUNT_OPTIONS),
    )
    .map_err(|e| format!("mount {device} at {}: {e}", target.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    const VOLUME: &str = "11111111-2222-3333-4444-555555555555";
    const OTHER: &str = "66666666-7777-8888-9999-aaaaaaaaaaaa";

    /// A disk's start with an ext4 superblock of `uuid`, checksummed, as
    /// mke2fs makes it, and mounted `mounts` times.
    fn ext4_disk(uuid: &str, mounts: u16, mount_time: u32) -> Vec<u8> {
        let mut head = vec![0u8; PROBE_BYTES];
        let sb = &mut head[1024..2048];
        sb[0x00..0x04].copy_from_slice(&8192u32.to_le_bytes());
        sb[0x18..0x1C].copy_from_slice(&2u32.to_le_bytes());
        sb[0x20..0x24].copy_from_slice(&32768u32.to_le_bytes());
        sb[0x28..0x2C].copy_from_slice(&8192u32.to_le_bytes());
        sb[0x2C..0x30].copy_from_slice(&mount_time.to_le_bytes());
        sb[0x34..0x36].copy_from_slice(&mounts.to_le_bytes());
        sb[0x38..0x3A].copy_from_slice(&0xEF53u16.to_le_bytes());
        sb[0x64..0x68].copy_from_slice(&0x400u32.to_le_bytes());
        let hex: String = uuid.chars().filter(|c| *c != '-').collect();
        for (i, byte) in sb[0x68..0x78].iter_mut().enumerate() {
            *byte = u8::from_str_radix(&hex[2 * i..2 * i + 2], 16).unwrap();
        }
        let checksum = crc32c(!0, &sb[..0x3FC]);
        sb[0x3FC..0x400].copy_from_slice(&checksum.to_le_bytes());
        head
    }

    #[test]
    fn crc32c_is_castagnolis() {
        assert_eq!(!crc32c(!0, b"123456789"), 0xE306_9283);
    }

    #[test]
    fn a_superblock_tells_its_uuid_and_whether_it_was_mounted() {
        assert_eq!(
            probe(&ext4_disk(VOLUME, 0, 0)),
            Found::Ext4 {
                uuid: VOLUME.to_string(),
                mounted: false
            }
        );
        // A mount counts itself and writes its time; e2fsck resets the
        // count, not the time.
        for (mounts, time) in [(1, 1_700_000_000), (0, 1_700_000_000), (3, 0)] {
            assert_eq!(
                probe(&ext4_disk(VOLUME, mounts, time)),
                Found::Ext4 {
                    uuid: VOLUME.to_string(),
                    mounted: true
                }
            );
        }
    }

    #[test]
    fn a_damaged_superblock_is_not_a_filesystem_to_format_over() {
        let mut head = ext4_disk(VOLUME, 1, 1_700_000_000);
        head[1024 + 0x100] ^= 1;
        let found = probe(&head);
        assert!(matches!(found, Found::Other(_)), "{found:?}");
        assert!(plan(&found, VOLUME, true).is_err());
    }

    #[test]
    fn other_signatures_are_found() {
        let with = |offset: usize, magic: &[u8]| {
            let mut head = vec![0u8; PROBE_BYTES];
            head[offset..offset + magic.len()].copy_from_slice(magic);
            probe(&head)
        };
        assert_eq!(with(0, b"XFSB"), Found::Other("an XFS filesystem"));
        assert_eq!(
            with(0x10040, b"_BHRfS_M"),
            Found::Other("a btrfs filesystem")
        );
        assert_eq!(with(4086, b"SWAPSPACE2"), Found::Other("swap space"));
        assert_eq!(
            with(512, b"EFI PART"),
            Found::Other("a GPT partition table")
        );
        assert_eq!(
            with(510, &[0x55, 0xaa]),
            Found::Other("a partition table or a boot sector")
        );
        assert_eq!(with(4096, b"anything"), Found::Unknown);
        assert_eq!(probe(&vec![0u8; PROBE_BYTES]), Found::Blank);
    }

    /// The format safety rule (ADR 0016).
    #[test]
    fn a_new_volume_is_formatted_unless_a_mounted_filesystem_is_on_it() {
        let blank = Found::Blank;
        let interrupted = probe(&ext4_disk(OTHER, 0, 0));
        let own_unmounted = probe(&ext4_disk(VOLUME, 0, 0));
        let own_mounted = probe(&ext4_disk(VOLUME, 2, 1_700_000_000));
        let other_mounted = probe(&ext4_disk(OTHER, 2, 1_700_000_000));
        let xfs = Found::Other("an XFS filesystem");

        // The host says it is new.
        assert_eq!(plan(&blank, VOLUME, true), Ok(Plan::Format));
        assert_eq!(plan(&Found::Unknown, VOLUME, true), Ok(Plan::Format));
        assert_eq!(plan(&interrupted, VOLUME, true), Ok(Plan::Format));
        assert_eq!(plan(&own_unmounted, VOLUME, true), Ok(Plan::Format));
        // Formatted and used before the host learnt of it: kept.
        assert_eq!(plan(&own_mounted, VOLUME, true), Ok(Plan::Mount));
        assert!(plan(&other_mounted, VOLUME, true).is_err());
        assert!(plan(&xfs, VOLUME, true).is_err());

        // The host knows it formatted: nothing is ever formatted.
        assert_eq!(plan(&own_mounted, VOLUME, false), Ok(Plan::Mount));
        assert_eq!(plan(&own_unmounted, VOLUME, false), Ok(Plan::Mount));
        assert!(plan(&blank, VOLUME, false).is_err());
        assert!(plan(&Found::Unknown, VOLUME, false).is_err());
        assert!(plan(&interrupted, VOLUME, false).is_err());
        assert!(plan(&other_mounted, VOLUME, false).is_err());
        assert!(plan(&xfs, VOLUME, false).is_err());
        // UUIDs compare without case.
        assert_eq!(
            plan(&own_mounted, &VOLUME.to_uppercase(), false),
            Ok(Plan::Mount)
        );
    }

    /// The probe reads what this host's mke2fs makes, if it has one.
    #[test]
    fn a_real_filesystem_is_found() {
        let mke2fs = ["/sbin/mke2fs", "/usr/sbin/mke2fs"]
            .into_iter()
            .find(|path| Path::new(path).exists());
        let Some(mke2fs) = mke2fs else {
            return;
        };
        let dir = tempfile::tempdir().unwrap();
        let image = dir.path().join("volume.img");
        std::fs::File::create(&image)
            .unwrap()
            .set_len(64 << 20)
            .unwrap();
        let status = std::process::Command::new(mke2fs)
            .args(["-q", "-F", "-t", "ext4", "-b", "4096", "-U", VOLUME])
            .arg(&image)
            .env("MKE2FS_CONFIG", "/nonexistent")
            .status()
            .unwrap();
        assert!(status.success());
        let head = read_head(image.to_str().unwrap()).unwrap();
        assert_eq!(
            probe(&head),
            Found::Ext4 {
                uuid: VOLUME.to_string(),
                mounted: false
            }
        );
    }

    #[test]
    fn names_and_paths() {
        for name in ["data", "a", "db-1", "0", &"a".repeat(63)] {
            assert!(valid_name(name), "{name}");
        }
        for name in ["", "-a", "a-", "A", "a_b", "a.b", "../x", &"a".repeat(64)] {
            assert!(!valid_name(name), "{name}");
        }
        for path in ["/data", "/var/lib/db", "/a b"] {
            assert!(valid_mount_path(path), "{path}");
        }
        for path in [
            "", "/", "data", "/data/", "//data", "/a/../b", "/a/./b", "/a\0",
        ] {
            assert!(!valid_mount_path(path), "{path:?}");
        }
        assert_eq!(mount_point("db"), Path::new("/run/feos/volumes/db"));
    }
}
