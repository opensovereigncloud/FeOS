// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The resolver configuration of a guest and of its containers.
//!
//! FeOS owns /etc/resolv.conf in every container. The host pushes the
//! resolvers (HostService.SetNetworkConfig); FeOS writes them to the guest's
//! /etc/resolv.conf and to resolv.conf in the share directory, which every
//! container sees read-only at /run/feos. A container's /etc/resolv.conf is a
//! symlink to /run/feos/resolv.conf in its overlay's upper layer; the image
//! stays untouched. FeOS installs the link when it creates or adopts a
//! container, and again on every push in each container whose overlay is
//! mounted, so whatever a workload puts in its place lasts until the next
//! push. An entry that already is the link is left alone.
//!
//! Each update writes a complete temporary file and renames it into place,
//! so a reader sees either the old or the new file, never a partial one.
//! Before the first push the shared file is empty.
//!
//! FeOS replaces a file or link of the workload's at /etc/resolv.conf, but
//! never destroys anything else: a directory there, an /etc that is not a
//! directory, or temporary names all taken by the workload's own entries
//! keep the link out, and the caller gets the reason.

use crate::fs::openat2_confined;
use nix::errno::Errno;
use nix::fcntl::{readlinkat, renameat, OFlag, ResolveFlag};
use nix::sys::stat::{mkdirat, Mode};
use nix::unistd::{symlinkat, unlinkat, UnlinkatFlags};
use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::os::fd::AsFd;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

pub use crate::share::{make_share_dir, SHARE_DEST, SHARE_DIR};
/// What a container's /etc/resolv.conf links to.
pub const CONTAINER_RESOLVER: &str = "/run/feos/resolv.conf";

const NAME: &str = "resolv.conf";
/// The fixed temporary name for [`replace`]; updates are serialized, and a
/// leftover from a crash is removed first.
const TMP: &str = ".feos-resolv.conf.tmp";
/// Temporary names for the link in a container's /etc. The workload may
/// own entries of these names; only one that already is the managed link is
/// reused, nothing else is touched.
const LINK_TMPS: [&str; 4] = [
    ".feos-resolv.conf.tmp",
    ".feos-resolv.conf.tmp.1",
    ".feos-resolv.conf.tmp.2",
    ".feos-resolv.conf.tmp.3",
];

/// Serializes updates and provisioning: both use fixed temporary names.
static UPDATE: Mutex<()> = Mutex::new(());

fn lock() -> MutexGuard<'static, ()> {
    UPDATE.lock().unwrap_or_else(|e| e.into_inner())
}

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

/// A container whose /etc/resolv.conf could not be linked: (id, reason).
pub type LinkFailure = (String, String);

/// Publishes `nameservers` to the share directory and to `guest_etc`, then
/// links every container under `containers` whose overlay is mounted to
/// them again. A failure to publish is an error; a container that cannot be
/// linked comes back as a [`LinkFailure`].
pub fn publish(
    nameservers: &[String],
    guest_etc: &Path,
    containers: &Path,
) -> Result<Vec<LinkFailure>, String> {
    publish_in(nameservers, Path::new(SHARE_DIR), guest_etc, containers)
}

fn publish_in(
    nameservers: &[String],
    share: &Path,
    guest_etc: &Path,
    containers: &Path,
) -> Result<Vec<LinkFailure>, String> {
    let _guard = lock();
    let content = render(nameservers);
    make_share_dir(share)
        .and_then(|()| replace(share, NAME, content.as_bytes()))
        .map_err(|e| format!("{}: {e}", share.join(NAME).display()))?;
    replace(guest_etc, NAME, content.as_bytes())
        .map_err(|e| format!("{}: {e}", guest_etc.join(NAME).display()))?;
    let roots = mounted_container_roots(containers)
        .map_err(|e| format!("containers to link to the resolvers: {e}"))?;
    let mut failed = Vec::new();
    for (id, root) in roots {
        if let Err(reason) = link(&root) {
            log::warn!("Container {id}: {reason}");
            failed.push((id, reason));
        }
    }
    Ok(failed)
}

/// Makes `root`/etc/resolv.conf the managed link, for a container being
/// created or adopted. Creates the shared file empty if no push wrote it yet.
pub fn provision(root: &Path) -> Result<(), String> {
    let _guard = lock();
    let share = Path::new(SHARE_DIR);
    let file = share.join(NAME);
    make_share_dir(share)
        .and_then(|()| {
            match OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o644)
                .open(&file)
            {
                Err(e) if e.kind() == io::ErrorKind::AlreadyExists => Ok(()),
                result => result.map(drop),
            }
        })
        .map_err(|e| format!("{}: {e}", file.display()))?;
    link(root)
}

/// The containers under `containers` whose overlay is mounted, by id, with
/// their root as the guest sees it. A container directory of a previous boot
/// that is not adopted yet has no overlay mounted and is left alone.
pub fn mounted_container_roots(containers: &Path) -> io::Result<Vec<(String, PathBuf)>> {
    use std::os::unix::fs::MetadataExt;
    let entries = match std::fs::read_dir(containers) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        result => result?,
    };
    let mut roots = Vec::new();
    for entry in entries {
        let entry = entry?;
        let bundle = entry.path().join("bundle");
        let root = bundle.join("rootfs");
        // A mounted overlay is a filesystem of its own.
        if let (Ok(b), Ok(r)) = (std::fs::metadata(&bundle), std::fs::metadata(&root))
            && b.dev() != r.dev()
        {
            roots.push((entry.file_name().to_string_lossy().into_owned(), root));
        }
    }
    roots.sort();
    Ok(roots)
}

/// Replaces dir/name with a complete file holding `content`. Callers hold
/// the update lock.
fn replace(dir: &Path, name: &str, content: &[u8]) -> io::Result<()> {
    let tmp = dir.join(TMP);
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

/// Is dir/resolv.conf the managed link? Reads the link, never follows it.
pub fn is_managed_link(dir: impl AsFd) -> bool {
    readlinkat(dir, NAME).is_ok_and(|target| target == CONTAINER_RESOLVER)
}

/// [`install_link`] with the reason a caller reports.
fn link(root: &Path) -> Result<(), String> {
    install_link(root)
        .map_err(|e| format!("/etc/resolv.conf cannot be linked to {CONTAINER_RESOLVER}: {e}"))
}

/// The parent is resolved the way the container sees it: absolute links and
/// parent traversals stay in the root, missing targets fail. The terminal
/// entry is renamed over, never followed; one that already is the managed
/// link is left as it is.
fn install_link(root: &Path) -> Result<(), String> {
    let root = File::open(root).map_err(|e| format!("{}: {e}", root.display()))?;
    let open_etc = || {
        openat2_confined(
            &root,
            Path::new("etc"),
            OFlag::O_RDONLY | OFlag::O_DIRECTORY,
            Mode::empty(),
            ResolveFlag::RESOLVE_IN_ROOT | ResolveFlag::RESOLVE_NO_MAGICLINKS,
        )
    };
    let etc = match open_etc() {
        // Scratch images often have no /etc. mkdirat does not follow a
        // dangling symlink: that stays an error below.
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            match mkdirat(&root, "etc", Mode::from_bits_truncate(0o755)) {
                Ok(()) | Err(Errno::EEXIST) => {}
                Err(e) => return Err(format!("mkdir /etc: {e}")),
            }
            open_etc()
        }
        result => result,
    }
    .map_err(|e| format!("/etc: {e}"))?;
    if is_managed_link(&etc) {
        return Ok(());
    }
    let tmp = link_tmp(&etc)?;
    renameat(&etc, tmp, &etc, NAME).map_err(|e| {
        // Only the managed link placed or reused here is ours to remove.
        if readlinkat(&etc, tmp).is_ok_and(|t| t == CONTAINER_RESOLVER) {
            let _ = unlinkat(&etc, tmp, UnlinkatFlags::NoRemoveDir);
        }
        e.to_string()
    })
}

/// Creates the managed link under the first free name of LINK_TMPS. A taken
/// name is reused only if it is the managed link already.
fn link_tmp(parent: &impl AsFd) -> Result<&'static str, String> {
    for tmp in LINK_TMPS {
        match symlinkat(CONTAINER_RESOLVER, parent, tmp) {
            Ok(()) => return Ok(tmp),
            Err(Errno::EEXIST) => {}
            Err(e) => return Err(format!("symlink /etc/{tmp}: {e}")),
        }
        match readlinkat(parent, tmp) {
            Ok(target) if target == CONTAINER_RESOLVER => return Ok(tmp),
            _ => {} // the workload's, or gone meanwhile: keep it
        }
    }
    Err(format!(
        "no free temporary name in /etc ({} are the workload's)",
        LINK_TMPS.join(", ")
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::MetadataExt;

    fn scratch(tag: &str) -> PathBuf {
        let base = std::env::temp_dir().join(format!("feos-resolver-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&base).unwrap();
        base
    }

    #[test]
    fn replacement_preserves_open_readers_and_does_not_follow_symlinks() {
        use std::io::Read;
        use std::os::unix::fs::symlink;

        let base = scratch("replace");
        let old = b"nameserver 192.0.2.53\n";
        let new = b"nameserver 198.51.100.53\nnameserver 203.0.113.53\n";
        replace(&base, NAME, old).unwrap();
        let mut reader = File::open(base.join(NAME)).unwrap();
        replace(&base, NAME, new).unwrap();
        let mut read = Vec::new();
        reader.read_to_end(&mut read).unwrap();
        assert_eq!(read, old);
        assert_eq!(std::fs::read(base.join(NAME)).unwrap(), new);

        let sentinel = base.join("sentinel");
        std::fs::write(&sentinel, "keep").unwrap();
        std::fs::remove_file(base.join(NAME)).unwrap();
        symlink(&sentinel, base.join(NAME)).unwrap();
        symlink(&sentinel, base.join(TMP)).unwrap();
        replace(&base, NAME, new).unwrap();
        assert_eq!(std::fs::read_to_string(&sentinel).unwrap(), "keep");
        assert_eq!(std::fs::read(base.join(NAME)).unwrap(), new);
        assert!(!base.join(TMP).exists());
        std::fs::remove_dir_all(base).unwrap();
    }

    /// A workload's own file or link at /etc/resolv.conf is replaced; the
    /// parent resolves inside the payload root, whatever links lead there.
    #[test]
    fn image_paths_and_terminal_symlinks_stay_in_root() {
        use std::os::unix::fs::symlink;
        let base = scratch("paths");
        let root = base.join("root");
        std::fs::create_dir_all(&root).unwrap();
        install_link(&root).unwrap(); // scratch: missing etc and file
        assert_eq!(
            std::fs::read_link(root.join("etc/resolv.conf")).unwrap(),
            Path::new(CONTAINER_RESOLVER)
        );
        let sentinel = base.join("sentinel");
        std::fs::write(&sentinel, "keep").unwrap();
        for target in [
            sentinel.as_path(),
            Path::new("../../sentinel"),
            Path::new("missing"),
        ] {
            std::fs::remove_file(root.join("etc/resolv.conf")).unwrap();
            symlink(target, root.join("etc/resolv.conf")).unwrap();
            install_link(&root).unwrap();
            assert_eq!(std::fs::read_to_string(&sentinel).unwrap(), "keep");
            assert!(is_managed_link(File::open(root.join("etc")).unwrap()));
        }
        std::fs::remove_file(root.join("etc/resolv.conf")).unwrap();
        std::fs::write(root.join("etc/resolv.conf"), "nameserver 203.0.113.9\n").unwrap();
        install_link(&root).unwrap(); // the workload's own file
        assert!(is_managed_link(File::open(root.join("etc")).unwrap()));

        std::fs::remove_dir_all(root.join("etc")).unwrap();
        std::fs::create_dir(root.join("config")).unwrap();
        symlink("/config", root.join("etc")).unwrap();
        install_link(&root).unwrap(); // absolute parent resolves in payload
        assert!(root
            .join("config/resolv.conf")
            .symlink_metadata()
            .unwrap()
            .file_type()
            .is_symlink());
        std::fs::remove_file(root.join("etc")).unwrap();
        symlink("../outside", root.join("etc")).unwrap();
        std::fs::create_dir(base.join("outside")).unwrap();
        let e = install_link(&root).unwrap_err(); // dangling inside the root
        assert!(e.starts_with("/etc: "), "{e}");
        assert!(!base.join("outside/resolv.conf").exists());
        // Leading .. clamps at the payload root, as it would in chroot.
        std::fs::create_dir(root.join("outside")).unwrap();
        install_link(&root).unwrap();
        assert_eq!(
            std::fs::read_link(root.join("outside/resolv.conf")).unwrap(),
            Path::new(CONTAINER_RESOLVER)
        );
        assert!(!base.join("outside/resolv.conf").exists());
        std::fs::remove_dir_all(base).unwrap();
    }

    /// The link in place is not touched again: a push writes nothing to a
    /// container that kept it.
    #[test]
    fn managed_link_is_left_alone() {
        let base = scratch("kept");
        std::fs::create_dir_all(base.join("etc")).unwrap();
        install_link(&base).unwrap();
        let before = std::fs::symlink_metadata(base.join("etc/resolv.conf")).unwrap();
        install_link(&base).unwrap();
        let after = std::fs::symlink_metadata(base.join("etc/resolv.conf")).unwrap();
        assert_eq!(before.ino(), after.ino());
        std::fs::remove_dir_all(base).unwrap();
    }

    /// A payload that replaced /etc/resolv.conf with a directory, or /etc
    /// with a file, cannot get the link: the reason says so, the payload's
    /// data stays, and no temporary is left behind.
    #[test]
    fn payload_owned_etc_is_kept() {
        let base = scratch("owned-etc");
        let root = base.join("root");
        std::fs::create_dir_all(root.join("etc/resolv.conf")).unwrap();
        std::fs::write(root.join("etc/resolv.conf/data"), "keep").unwrap();
        let e = link(&root).unwrap_err();
        assert!(
            e.starts_with("/etc/resolv.conf cannot be linked to /run/feos/resolv.conf: ")
                && e.contains("Is a directory"),
            "{e}"
        );
        assert_eq!(
            std::fs::read_to_string(root.join("etc/resolv.conf/data")).unwrap(),
            "keep"
        );
        for tmp in LINK_TMPS {
            assert!(root.join("etc").join(tmp).symlink_metadata().is_err());
        }

        std::fs::remove_dir_all(root.join("etc")).unwrap();
        std::fs::write(root.join("etc"), "a file").unwrap();
        let e = link(&root).unwrap_err();
        assert!(e.contains("/etc: ") && e.contains("Not a directory"), "{e}");
        assert_eq!(std::fs::read_to_string(root.join("etc")).unwrap(), "a file");
        std::fs::remove_dir_all(base).unwrap();
    }

    /// A temporary left behind by a crash (same pid on every boot) must
    /// not block the next update or provisioning.
    #[test]
    fn stale_temporaries_are_replaced() {
        let base = scratch("stale");
        std::fs::create_dir_all(base.join("etc")).unwrap();
        std::fs::write(base.join(TMP), "stale").unwrap();
        replace(&base, NAME, b"nameserver 192.0.2.53\n").unwrap();
        assert_eq!(
            std::fs::read(base.join(NAME)).unwrap(),
            b"nameserver 192.0.2.53\n"
        );
        assert!(!base.join(TMP).exists());
        let etc = base.join("etc");
        std::os::unix::fs::symlink(CONTAINER_RESOLVER, etc.join(LINK_TMPS[0])).unwrap();
        install_link(&base).unwrap();
        assert_eq!(
            std::fs::read_link(etc.join(NAME)).unwrap(),
            Path::new(CONTAINER_RESOLVER)
        );
        assert!(etc.join(LINK_TMPS[0]).symlink_metadata().is_err());
        std::fs::remove_dir_all(base).unwrap();
    }

    /// Entries of the temporary names that the payload owns (anything but
    /// the managed link) are kept; with all names taken, the link fails
    /// and the workload's file stays.
    #[test]
    fn payload_owned_temporaries_are_kept() {
        use std::os::unix::fs::symlink;
        let base = scratch("owned-tmp");
        let etc = base.join("etc");
        std::fs::create_dir_all(&etc).unwrap();
        std::fs::write(etc.join(LINK_TMPS[0]), "keep").unwrap();
        symlink("/run/feos/other", etc.join(LINK_TMPS[1])).unwrap();
        install_link(&base).unwrap();
        assert_eq!(
            std::fs::read_link(etc.join(NAME)).unwrap(),
            Path::new(CONTAINER_RESOLVER)
        );
        assert_eq!(
            std::fs::read_to_string(etc.join(LINK_TMPS[0])).unwrap(),
            "keep"
        );
        assert_eq!(
            std::fs::read_link(etc.join(LINK_TMPS[1])).unwrap(),
            Path::new("/run/feos/other")
        );
        assert!(etc.join(LINK_TMPS[2]).symlink_metadata().is_err());

        std::fs::remove_file(etc.join(NAME)).unwrap();
        std::fs::write(etc.join(NAME), "workload").unwrap();
        for tmp in &LINK_TMPS[2..] {
            std::fs::create_dir(etc.join(tmp)).unwrap();
        }
        let e = install_link(&base).unwrap_err();
        assert!(e.starts_with("no free temporary name in /etc"), "{e}");
        assert_eq!(std::fs::read_to_string(etc.join(NAME)).unwrap(), "workload");
        assert_eq!(
            std::fs::read_to_string(etc.join(LINK_TMPS[0])).unwrap(),
            "keep"
        );
        std::fs::remove_dir_all(base).unwrap();
    }
}
