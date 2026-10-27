// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Applying OCI image layers to a root filesystem.
//!
//! The tar crate only parses the archive (PAX and GNU long names included,
//! names as bytes); everything that touches the filesystem goes through
//! layer.rs and the one image-rooted resolver in rootfs.rs, never through
//! the crate's own unpack. That one path is what makes every entry type
//! resolve its parent the way the container will see it — through in-image
//! symlinks, absolute ones included, clamped at the image root — and
//! replace what is in its way by the same rules (containerd's): a whiteout
//! marker deletes instead of becoming a file, a restated hard link replaces
//! the old one, a file replaces a directory, a directory a symlink.

use flate2::read::GzDecoder;
use std::path::Path;
use tar::Archive;

mod hardlink;
mod layer;
mod rootfs;
mod whiteout;

/// Applies one gzip-compressed layer tar onto `rootfs`.
pub fn unpack_layer<R: std::io::Read>(r: R, rootfs: &Path) -> std::io::Result<()> {
    let mut ar = Archive::new(GzDecoder::new(std::io::BufReader::new(r)));
    let mut layer = layer::Layer::open(rootfs)?;
    for entry in ar.entries()? {
        layer.apply(&mut entry?)?;
    }
    layer.finish()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU32, Ordering};

    static SEQ: AtomicU32 = AtomicU32::new(0);

    fn scratch() -> PathBuf {
        let d = std::env::temp_dir().join(format!(
            "feos-layer-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&d).expect("scratch dir");
        d
    }

    /// Build a gzip'd layer tar. Entries are (path, contents); a None body
    /// makes a directory.
    fn layer(entries: &[(&str, Option<&[u8]>)]) -> Vec<u8> {
        let entries: Vec<Raw> = entries
            .iter()
            .map(|(path, body)| match body {
                Some(data) => Raw::file(path, data),
                None => Raw::dir(path),
            })
            .collect();
        raw_layer(&entries)
    }

    /// One archive entry written field by field. The tar crate's Builder
    /// refuses absolute and ".." paths — exactly what a hostile image
    /// would ship — so nothing here goes through its validating API; GNU
    /// long-name/long-link records carry what does not fit the 100-byte
    /// fields (absolute scratch paths below a long TMPDIR).
    struct Raw<'a> {
        kind: tar::EntryType,
        path: &'a [u8],
        link: &'a [u8],
        body: &'a [u8],
        mode: u32,
        mtime: u64,
        /// The extracting user's own ids by default: what a non-root test
        /// may hand to chown.
        uid: u64,
        gid: u64,
        device: (u32, u32),
        /// PAX records ahead of the entry.
        pax: Vec<(&'a str, &'a [u8])>,
        /// A pre-POSIX header (no ustar/GNU magic).
        old: bool,
    }

    impl<'a> Raw<'a> {
        fn new(kind: tar::EntryType, path: &'a [u8]) -> Raw<'a> {
            Raw {
                kind,
                path,
                link: b"",
                body: b"",
                mode: if kind == tar::EntryType::Directory {
                    0o755
                } else {
                    0o644
                },
                mtime: 1_600_000_000,
                uid: euid().into(),
                gid: egid().into(),
                device: (0, 0),
                pax: Vec::new(),
                old: false,
            }
        }
        fn file(path: &'a str, body: &'a [u8]) -> Raw<'a> {
            Raw {
                body,
                ..Raw::new(tar::EntryType::Regular, path.as_bytes())
            }
        }
        fn dir(path: &'a str) -> Raw<'a> {
            Raw::new(tar::EntryType::Directory, path.as_bytes())
        }
        fn symlink(path: &'a str, target: &'a str) -> Raw<'a> {
            Raw {
                link: target.as_bytes(),
                ..Raw::new(tar::EntryType::Symlink, path.as_bytes())
            }
        }
        fn link(path: &'a str, target: &'a str) -> Raw<'a> {
            Raw {
                link: target.as_bytes(),
                ..Raw::new(tar::EntryType::Link, path.as_bytes())
            }
        }
    }

    fn euid() -> u32 {
        // SAFETY: no preconditions; reads the calling process's ids.
        unsafe { nix::libc::geteuid() }
    }

    fn egid() -> u32 {
        // SAFETY: as euid().
        unsafe { nix::libc::getegid() }
    }

    fn raw_tar(entries: &[Raw]) -> Vec<u8> {
        let mut b = tar::Builder::new(Vec::new());
        for e in entries {
            if !e.pax.is_empty() {
                let mut records = Vec::new();
                for (key, value) in &e.pax {
                    // "<len> <key>=<value>\n", the length counting itself.
                    let rest = key.len() + value.len() + 3;
                    let mut len = rest + 1;
                    while len != rest + len.to_string().len() {
                        len = rest + len.to_string().len();
                    }
                    records.extend_from_slice(format!("{len} {key}=").as_bytes());
                    records.extend_from_slice(value);
                    records.push(b'\n');
                }
                let mut h = tar::Header::new_ustar();
                h.set_path("PaxHeaders/entry").unwrap();
                h.set_entry_type(tar::EntryType::XHeader);
                h.set_size(records.len() as u64);
                h.set_mode(0o644);
                h.set_cksum();
                b.append(&h, records.as_slice()).unwrap();
            }
            for (kind, bytes) in [
                (tar::EntryType::GNULongName, e.path),
                (tar::EntryType::GNULongLink, e.link),
            ] {
                if bytes.len() > 100 {
                    let mut long = tar::Header::new_gnu();
                    long.set_path("././@LongLink").unwrap();
                    long.set_entry_type(kind);
                    long.set_size(bytes.len() as u64 + 1);
                    long.set_mode(0o644);
                    long.set_cksum();
                    let mut name = bytes.to_vec();
                    name.push(0);
                    b.append(&long, name.as_slice()).unwrap();
                }
            }
            let mut h = if e.old {
                tar::Header::new_old()
            } else {
                tar::Header::new_gnu()
            };
            let old = h.as_old_mut();
            let path = &e.path[..e.path.len().min(100)];
            old.name[..path.len()].copy_from_slice(path);
            let link = &e.link[..e.link.len().min(100)];
            old.linkname[..link.len()].copy_from_slice(link);
            h.set_entry_type(e.kind);
            h.set_size(e.body.len() as u64);
            h.set_mode(e.mode);
            h.set_mtime(e.mtime);
            h.set_uid(e.uid);
            h.set_gid(e.gid);
            if !e.old {
                h.set_device_major(e.device.0).unwrap();
                h.set_device_minor(e.device.1).unwrap();
            }
            h.set_cksum();
            b.append(&h, e.body).expect("append raw entry");
        }
        b.into_inner().expect("finish tar")
    }

    fn gzip(tar: &[u8]) -> Vec<u8> {
        use std::io::Write;
        let mut enc = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
        enc.write_all(tar).unwrap();
        enc.finish().expect("finish gzip")
    }

    fn raw_layer(entries: &[Raw]) -> Vec<u8> {
        gzip(&raw_tar(entries))
    }

    fn try_apply(rootfs: &Path, entries: &[Raw]) -> std::io::Result<()> {
        unpack_layer(std::io::Cursor::new(raw_layer(entries)), rootfs)
    }

    /// A single empty regular file at a path a validating builder would
    /// refuse.
    fn hostile_layer(path: &str) -> Vec<u8> {
        raw_layer(&[Raw::file(path, b"")])
    }

    fn apply(rootfs: &Path, tar_gz: &[u8]) {
        unpack_layer(std::io::Cursor::new(tar_gz), rootfs).expect("unpack layer");
    }

    /// Regular `files`, then one hard link `path -> target`.
    fn hardlink_layer(path: &str, target: &str, files: &[(&str, &[u8])]) -> Vec<u8> {
        let mut entries: Vec<Raw> = files
            .iter()
            .map(|(name, body)| Raw::file(name, body))
            .collect();
        entries.push(Raw::link(path, target));
        raw_layer(&entries)
    }

    /// Absolute paths in these tests name a host directory inside the
    /// test's own scratch dir, and the image carries a copy of it under the
    /// same absolute path. Image-rooted resolution must act on the copy
    /// returned here; a regression that resolved against the host's "/"
    /// would hit the scratch original instead, never a real system path.
    fn in_image(root: &Path, absolute: &Path) -> PathBuf {
        root.join(absolute.strip_prefix("/").expect("absolute host path"))
    }

    fn s(path: &Path) -> &str {
        path.to_str().expect("scratch paths are UTF-8")
    }

    fn nlink(path: &Path) -> u64 {
        use std::os::unix::fs::MetadataExt;
        std::fs::symlink_metadata(path).unwrap().nlink()
    }

    fn assert_same_inode(a: &Path, b: &Path) {
        use std::os::unix::fs::MetadataExt;
        let a = std::fs::symlink_metadata(a).unwrap();
        let b = std::fs::symlink_metadata(b).unwrap();
        assert_eq!((a.dev(), a.ino()), (b.dev(), b.ino()));
    }

    #[test]
    fn hardlinks_resolve_absolute_and_relative_targets_in_the_image() {
        let base = scratch();
        // The scratch original of what the absolute paths name: a decoy a
        // host-rooted regression would link to (or next to) instead.
        let host_bin = base.join("usr/bin");
        std::fs::create_dir_all(&host_bin).unwrap();
        std::fs::write(host_bin.join("perlbug"), b"host").unwrap();
        let absolute = host_bin.join("perlbug");
        let new_link = base.join("new/directory/link");
        for (round, absolute_target) in [true, false].into_iter().enumerate() {
            let root = base.join(format!("root{round}"));
            let bin = in_image(&root, &host_bin);
            let rel_bin = bin.strip_prefix(&root).unwrap().to_path_buf();
            let target = if absolute_target {
                absolute.clone()
            } else {
                rel_bin.join("perlbug")
            };
            std::fs::create_dir(&root).unwrap();
            // Minimal complete layer: the regular file precedes its link.
            apply(
                &root,
                &hardlink_layer(
                    s(&rel_bin.join("perlthanks")),
                    s(&target),
                    &[(s(&rel_bin.join("perlbug")), b"original")],
                ),
            );
            let source = bin.join("perlbug");
            assert_eq!(std::fs::read(bin.join("perlthanks")).unwrap(), b"original");
            assert_same_inode(&source, &bin.join("perlthanks"));

            // Later layers may restate an existing link or even the source.
            let thanks = rel_bin.join("perlthanks");
            let bug = rel_bin.join("perlbug");
            apply(&root, &hardlink_layer(s(&thanks), s(&target), &[]));
            apply(&root, &hardlink_layer(s(&bug), s(&target), &[]));
            assert_same_inode(&source, &bin.join("perlthanks"));
            assert_eq!(std::fs::read_dir(&bin).unwrap().count(), 2);

            // Absolute entry paths are image-rooted too; create missing parents.
            apply(&root, &hardlink_layer(s(&new_link), s(&target), &[]));
            assert_same_inode(&source, &in_image(&root, &new_link));
            assert_eq!(
                std::fs::read_dir(in_image(&root, new_link.parent().unwrap()))
                    .unwrap()
                    .count(),
                1
            );
        }
        // The host side of every absolute path is untouched.
        assert_eq!(std::fs::read(&absolute).unwrap(), b"host");
        assert_eq!(nlink(&absolute), 1);
        assert_eq!(std::fs::read_dir(&host_bin).unwrap().count(), 1);
        assert!(!base.join("new").exists());
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn hardlinks_replace_files_and_destination_symlinks() {
        use std::os::unix::fs::symlink;
        let arena = scratch();
        let root = arena.join("root");
        let host = arena.join("data");
        let data = in_image(&root, &host);
        std::fs::create_dir_all(&host).unwrap();
        std::fs::create_dir_all(&data).unwrap();
        std::fs::write(host.join("source"), b"decoy").unwrap();
        std::fs::write(data.join("source"), b"new").unwrap();
        std::fs::write(root.join("destination"), b"old").unwrap();
        std::fs::write(arena.join("outside"), b"outside").unwrap();
        let rel_source = data.strip_prefix(&root).unwrap().join("source");
        apply(&root, &hardlink_layer("destination", s(&rel_source), &[]));
        assert_same_inode(&data.join("source"), &root.join("destination"));

        // An absolute target names the image's copy, and a destination
        // symlink is replaced, never followed.
        std::fs::remove_file(root.join("destination")).unwrap();
        symlink(arena.join("outside"), root.join("destination")).unwrap();
        apply(
            &root,
            &hardlink_layer("destination", s(&host.join("source")), &[]),
        );
        assert_same_inode(&data.join("source"), &root.join("destination"));
        assert_eq!(std::fs::read(arena.join("outside")).unwrap(), b"outside");
        assert_eq!(std::fs::read(host.join("source")).unwrap(), b"decoy");
        assert_eq!(nlink(&host.join("source")), 1);
        assert_eq!(std::fs::read_dir(&root).unwrap().count(), 2);
        std::fs::remove_dir_all(arena).unwrap();
    }

    #[test]
    fn hardlinks_preserve_source_symlink_inodes_including_dangling_links() {
        use std::os::unix::fs::symlink;
        let arena = scratch();
        let root = arena.join("root");
        let host = arena.join("data");
        let data = in_image(&root, &host);
        std::fs::create_dir_all(&host).unwrap();
        std::fs::create_dir_all(&data).unwrap();
        std::fs::write(host.join("source"), b"decoy").unwrap();
        std::fs::write(root.join("file"), b"inside").unwrap();
        std::fs::write(arena.join("outside"), b"outside").unwrap();
        for target in [
            PathBuf::from("file"),
            PathBuf::from("missing"),
            arena.join("outside"),
        ] {
            symlink(&target, data.join("source")).unwrap();
            apply(
                &root,
                &hardlink_layer("destination", s(&host.join("source")), &[]),
            );
            assert!(std::fs::symlink_metadata(root.join("destination"))
                .unwrap()
                .is_symlink());
            assert_eq!(
                std::fs::read_link(root.join("destination")).unwrap(),
                target
            );
            assert_same_inode(&data.join("source"), &root.join("destination"));
            std::fs::remove_file(data.join("source")).unwrap();
        }
        assert_eq!(std::fs::read(arena.join("outside")).unwrap(), b"outside");
        assert_eq!(std::fs::read(host.join("source")).unwrap(), b"decoy");
        assert_eq!(nlink(&host.join("source")), 1);
        // root holds "file", "destination" and the first component of
        // the in-image copy of the scratch path.
        assert_eq!(std::fs::read_dir(&root).unwrap().count(), 3);
        std::fs::remove_dir_all(arena).unwrap();
    }

    #[test]
    fn hardlinks_follow_contained_parent_symlinks_and_keep_non_utf8_names() {
        use std::os::unix::ffi::OsStrExt;
        use std::os::unix::fs::symlink;
        let root = scratch();
        std::fs::create_dir_all(root.join("usr/bin")).unwrap();
        std::fs::create_dir(root.join("lib")).unwrap();
        symlink("usr/bin", root.join("bin")).unwrap();
        symlink("../lib", root.join("usr/lib")).unwrap();
        std::fs::write(root.join("usr/bin/source"), b"inside").unwrap();
        apply(
            &root,
            &hardlink_layer("usr/lib/destination", "bin/source", &[]),
        );
        assert_same_inode(&root.join("usr/bin/source"), &root.join("lib/destination"));

        let source = Path::new(std::ffi::OsStr::from_bytes(b"usr/bin/\xff"));
        let destination = Path::new(std::ffi::OsStr::from_bytes(b"bin/\xfe"));
        std::fs::write(root.join(source), b"bytes").unwrap();
        layer::Layer::open(&root)
            .unwrap()
            .hard_link(destination, source)
            .unwrap();
        assert_same_inode(&root.join(source), &root.join(destination));
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn hardlinks_replace_directories_after_preparing_the_source() {
        let root = scratch();
        std::fs::create_dir_all(root.join("destination/subdir")).unwrap();
        std::fs::write(root.join("destination/subdir/source"), b"keep").unwrap();
        // Source is inside the directory being replaced: prepare its inode
        // before removing that directory or the last name would be lost.
        apply(
            &root,
            &hardlink_layer("destination", "destination/subdir/source", &[]),
        );
        assert_eq!(std::fs::read(root.join("destination")).unwrap(), b"keep");
        assert_eq!(std::fs::read_dir(&root).unwrap().count(), 1);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn rejected_hardlinks_preserve_existing_destinations_and_outside_files() {
        use std::os::unix::fs::symlink;
        let arena = scratch();
        let root = arena.join("root");
        std::fs::create_dir(&root).unwrap();
        std::fs::create_dir(root.join("directory")).unwrap();
        std::fs::write(root.join("source"), b"source").unwrap();
        std::fs::write(root.join("keep"), b"keep").unwrap();
        std::fs::write(arena.join("outside"), b"outside").unwrap();
        symlink(&arena, root.join("escape")).unwrap();
        symlink("..", root.join("relative-escape")).unwrap();
        // Absolute with "..": read against the host's "/", this names the
        // scratch file "outside", never a real system path.
        let absolute_escape = format!("{}/../outside", root.display());
        for (path, target) in [
            ("../outside", "source"),
            (absolute_escape.as_str(), "source"),
            ("keep", "../outside"),
            ("keep", absolute_escape.as_str()),
            ("keep", "escape/outside"),
            ("escape/outside", "source"),
            ("keep", "relative-escape/outside"),
            ("keep", "missing"),
            ("keep", "directory"),
        ] {
            let error = unpack_layer(
                std::io::Cursor::new(hardlink_layer(path, target, &[])),
                &root,
            )
            .expect_err("invalid hard link must fail");
            assert!(error.to_string().contains("hard link"), "{error}");
            assert_eq!(std::fs::read(root.join("keep")).unwrap(), b"keep");
            assert_eq!(std::fs::read(arena.join("outside")).unwrap(), b"outside");
            assert_eq!(std::fs::read_dir(&root).unwrap().count(), 5);
        }
        for (path, target) in [("", "source"), ("/", "source"), ("keep", ""), ("keep", "/")] {
            let mut layer = layer::Layer::open(&root).unwrap();
            assert!(layer.hard_link(Path::new(path), Path::new(target)).is_err());
            assert_eq!(std::fs::read(root.join("keep")).unwrap(), b"keep");
        }

        // RESOLVE_IN_ROOT clamps ".." in a parent symlink at the image root
        // (as the container will see it): accepted, but it lands inside.
        apply(
            &root,
            &hardlink_layer("relative-escape/outside", "relative-escape/source", &[]),
        );
        assert_same_inode(&root.join("source"), &root.join("outside"));
        assert_eq!(std::fs::read(arena.join("outside")).unwrap(), b"outside");
        std::fs::remove_dir_all(arena).unwrap();
    }

    #[test]
    fn hardlinks_follow_absolute_in_image_parent_symlinks() {
        use std::os::unix::fs::symlink;
        let arena = scratch();
        let root = arena.join("root");
        // Debian's layout (usr/lib/ssl/certs -> /etc/ssl/certs), with both
        // trees at scratch paths that exist on the host side as well: a
        // host-rooted regression would link inside the decoy copy.
        let host_certs = arena.join("etc/ssl/certs");
        let host_ssl = arena.join("usr/lib/ssl");
        let certs = in_image(&root, &host_certs);
        let ssl = in_image(&root, &host_ssl);
        for (c, l) in [(&host_certs, &host_ssl), (&certs, &ssl)] {
            std::fs::create_dir_all(c).unwrap();
            std::fs::create_dir_all(l).unwrap();
            symlink(&host_certs, l.join("certs")).unwrap();
            std::fs::write(c.join("ca.pem"), b"ca").unwrap();
        }
        let rel_ssl = ssl.strip_prefix(&root).unwrap();
        apply(
            &root,
            &hardlink_layer(
                s(&rel_ssl.join("certs/ca-link.pem")),
                s(&host_ssl.join("certs/ca.pem")),
                &[],
            ),
        );
        assert_same_inode(&certs.join("ca.pem"), &certs.join("ca-link.pem"));
        assert_eq!(std::fs::read_dir(&host_certs).unwrap().count(), 1);
        assert_eq!(nlink(&host_certs.join("ca.pem")), 1);
        // A link pointing at the scratch arena by its host path resolves
        // beneath the image root instead, where nothing exists.
        symlink(&arena, root.join("host")).unwrap();
        std::fs::write(arena.join("outside"), b"outside").unwrap();
        assert!(unpack_layer(
            std::io::Cursor::new(hardlink_layer("x", "host/outside", &[])),
            &root,
        )
        .is_err());
        assert!(!root.join("x").exists());
        std::fs::remove_dir_all(arena).unwrap();
    }

    #[test]
    fn whiteouts_follow_parent_symlinks_contained_in_the_image() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        let host = base.join("actual");
        let actual = in_image(&root, &host);
        for dir in [&host, &actual] {
            std::fs::create_dir_all(dir.join("deep")).unwrap();
            std::fs::write(dir.join("victim"), b"victim").unwrap();
            std::fs::write(dir.join("other"), b"other").unwrap();
            std::fs::write(dir.join("deep/file"), b"file").unwrap();
        }
        std::fs::create_dir(root.join("nested")).unwrap();
        let relative = Path::new("..").join(host.strip_prefix("/").unwrap());
        symlink(relative, root.join("nested/alias")).unwrap();
        symlink(&host, root.join("absolute-alias")).unwrap();
        std::fs::write(root.join("keep"), b"keep").unwrap();
        apply(&root, &layer(&[("nested/alias/.wh.victim", Some(b""))]));
        assert!(!actual.join("victim").exists());
        // Absolute in-image parent symlinks resolve within the image root.
        apply(&root, &layer(&[("absolute-alias/.wh.other", Some(b""))]));
        assert!(!actual.join("other").exists());
        assert!(actual.join("deep/file").exists());
        apply(&root, &layer(&[("nested/alias/.wh..wh..opq", Some(b""))]));
        assert_eq!(std::fs::read_dir(&actual).unwrap().count(), 0);
        assert_eq!(std::fs::read(root.join("keep")).unwrap(), b"keep");
        assert!(root
            .join("nested/alias")
            .symlink_metadata()
            .unwrap()
            .is_symlink());
        // The host directory the absolute link names stays untouched.
        assert_eq!(std::fs::read(host.join("victim")).unwrap(), b"victim");
        assert_eq!(std::fs::read(host.join("other")).unwrap(), b"other");
        assert_eq!(std::fs::read(host.join("deep/file")).unwrap(), b"file");
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn absolute_whiteout_markers_are_image_rooted() {
        let base = scratch();
        let root = base.join("root");
        let host = base.join("etc");
        let etc = in_image(&root, &host);
        for dir in [&host, &etc] {
            std::fs::create_dir_all(dir.join("dir")).unwrap();
            std::fs::write(dir.join("victim"), b"victim").unwrap();
            std::fs::write(dir.join("dir/file"), b"file").unwrap();
        }
        apply(
            &root,
            &hostile_layer(&format!("{}/.wh.victim", host.display())),
        );
        assert!(!etc.join("victim").exists());
        apply(
            &root,
            &hostile_layer(&format!("{}/dir/.wh..wh..opq", host.display())),
        );
        assert_eq!(std::fs::read_dir(etc.join("dir")).unwrap().count(), 0);
        // The same absolute paths on the host side keep their contents.
        assert_eq!(std::fs::read(host.join("victim")).unwrap(), b"victim");
        assert_eq!(std::fs::read(host.join("dir/file")).unwrap(), b"file");
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn hostile_whiteout_parents_and_derived_names_do_not_delete_siblings() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        let outside = base.join("outside");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&outside).unwrap();
        std::fs::write(outside.join("victim"), "keep").unwrap();
        std::fs::write(root.join("keep"), "keep").unwrap();
        symlink(&outside, root.join("absolute-parent")).unwrap();
        symlink("../outside", root.join("relative-parent")).unwrap();
        // hostile_layer fills raw tar header bytes so the archive builder
        // cannot mask these cases by rejecting the path before extraction.
        // Lexical ".." and degenerate target names are refused.
        for marker in [
            "../outside/.wh.victim",
            "../outside/.wh..wh..opq",
            ".wh.",
            ".wh..",
            ".wh...",
        ] {
            assert!(
                unpack_layer(std::io::Cursor::new(hostile_layer(marker)), &root).is_err(),
                "accepted {marker}"
            );
            assert_eq!(
                std::fs::read_to_string(outside.join("victim")).unwrap(),
                "keep",
                "{marker}"
            );
            assert_eq!(
                std::fs::read_to_string(root.join("keep")).unwrap(),
                "keep",
                "{marker}"
            );
        }
        // Host-absolute paths and escaping parent symlinks resolve inside
        // the image root, where they name nothing: a no-op, not a deletion.
        let absolute = format!("{}/.wh.victim", outside.display());
        let absolute_opaque = format!("{}/.wh..wh..opq", outside.display());
        for marker in [
            absolute.as_str(),
            absolute_opaque.as_str(),
            "absolute-parent/.wh.victim",
            "absolute-parent/.wh..wh..opq",
            "relative-parent/.wh.victim",
            "relative-parent/.wh..wh..opq",
        ] {
            apply(&root, &hostile_layer(marker));
            assert_eq!(
                std::fs::read_to_string(outside.join("victim")).unwrap(),
                "keep",
                "{marker}"
            );
            assert_eq!(
                std::fs::read_to_string(root.join("keep")).unwrap(),
                "keep",
                "{marker}"
            );
        }
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn whiteout_under_a_non_directory_parent_fails_the_layer() {
        let root = scratch();
        std::fs::write(root.join("file"), b"keep").unwrap();
        assert!(unpack_layer(
            std::io::Cursor::new(layer(&[("file/.wh.x", Some(b""))])),
            &root
        )
        .is_err());
        assert_eq!(std::fs::read(root.join("file")).unwrap(), b"keep");
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn whiteout_unlinks_terminal_symlinks_and_missing_targets_are_noops() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        std::fs::create_dir_all(root.join("nested")).unwrap();
        std::fs::write(base.join("sentinel"), "keep").unwrap();
        symlink(base.join("sentinel"), root.join("nested/link")).unwrap();
        apply(&root, &layer(&[("nested/.wh.link", Some(b""))]));
        assert!(root.join("nested/link").symlink_metadata().is_err());
        assert_eq!(
            std::fs::read_to_string(base.join("sentinel")).unwrap(),
            "keep"
        );
        apply(
            &root,
            &layer(&[
                ("missing/.wh.absent", Some(b"")),
                ("missing/.wh..wh..opq", Some(b"")),
            ]),
        );
        symlink(base.join("sentinel"), root.join("nested/link")).unwrap();
        std::fs::create_dir_all(root.join("nested/deep/a")).unwrap();
        std::fs::write(root.join("nested/deep/a/file"), "delete").unwrap();
        apply(&root, &layer(&[("nested/.wh..wh..opq", Some(b""))]));
        assert_eq!(std::fs::read_dir(root.join("nested")).unwrap().count(), 0);
        assert_eq!(
            std::fs::read_to_string(base.join("sentinel")).unwrap(),
            "keep"
        );
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn non_utf8_whiteout_names_delete_the_intended_entry() {
        use std::os::unix::ffi::OsStringExt;
        let root = scratch();
        let victim = std::ffi::OsString::from_vec(b"victim-\xff".to_vec());
        std::fs::write(root.join(&victim), "delete").unwrap();
        let mut enc = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
        {
            let mut b = tar::Builder::new(&mut enc);
            let mut h = tar::Header::new_gnu();
            // Raw name-field write (short name, fits the 100-byte field):
            // set_path would reject the non-UTF-8 bytes.
            let name = b".wh.victim-\xff";
            h.as_gnu_mut().unwrap().name[..name.len()].copy_from_slice(name);
            h.set_entry_type(tar::EntryType::Regular);
            h.set_size(0);
            h.set_mode(0o644);
            h.set_cksum();
            b.append(&h, std::io::empty()).unwrap();
            b.finish().unwrap();
        }
        apply(&root, &enc.finish().unwrap());
        assert!(!root.join(victim).exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    // The whole point: a later layer can DELETE. Without whiteout handling
    // the removed file stays visible in the container — which for a dropped
    // setuid binary or a rotated secret means it stays usable.
    #[test]
    fn whiteout_removes_a_path_from_the_lower_layers() {
        let root = scratch();
        apply(
            &root,
            &layer(&[
                ("etc/", None),
                ("etc/keep.conf", Some(b"keep")),
                ("etc/secret.key", Some(b"sensitive")),
            ]),
        );
        assert!(root.join("etc/secret.key").exists());

        apply(&root, &layer(&[("etc/.wh.secret.key", Some(b""))]));

        assert!(
            !root.join("etc/secret.key").exists(),
            "whiteout did not delete"
        );
        assert!(
            root.join("etc/keep.conf").exists(),
            "whiteout deleted too much"
        );
        std::fs::remove_dir_all(&root).ok();
    }

    // A whiteout for a directory takes the whole subtree.
    #[test]
    fn whiteout_removes_a_directory_subtree() {
        let root = scratch();
        apply(
            &root,
            &layer(&[
                ("var/", None),
                ("var/cache/", None),
                ("var/cache/a", Some(b"a")),
            ]),
        );
        apply(&root, &layer(&[("var/.wh.cache", Some(b""))]));
        assert!(!root.join("var/cache").exists());
        assert!(root.join("var").is_dir(), "the parent must survive");
        std::fs::remove_dir_all(&root).ok();
    }

    // The opaque marker hides everything BELOW it, but the layer's own
    // entries come after it in the tar and must survive.
    #[test]
    fn opaque_whiteout_clears_lower_entries_and_keeps_this_layers_own() {
        let root = scratch();
        apply(
            &root,
            &layer(&[
                ("opt/", None),
                ("opt/old-a", Some(b"a")),
                ("opt/old-b", Some(b"b")),
            ]),
        );
        apply(
            &root,
            &layer(&[("opt/.wh..wh..opq", Some(b"")), ("opt/fresh", Some(b"new"))]),
        );
        assert!(!root.join("opt/old-a").exists());
        assert!(!root.join("opt/old-b").exists());
        assert!(
            root.join("opt").is_dir(),
            "opaque must not remove the directory"
        );
        assert_eq!(std::fs::read(root.join("opt/fresh")).unwrap(), b"new");
        std::fs::remove_dir_all(&root).ok();
    }

    // Markers are instructions, not content: none of them may ever appear
    // in the unpacked tree.
    #[test]
    fn whiteout_markers_are_never_materialised() {
        let root = scratch();
        apply(&root, &layer(&[("srv/", None), ("srv/gone", Some(b"x"))]));
        apply(
            &root,
            &layer(&[("srv/.wh.gone", Some(b"")), ("srv/.wh..wh..opq", Some(b""))]),
        );
        let leftovers: Vec<_> = std::fs::read_dir(root.join("srv"))
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|n| n.starts_with(".wh."))
            .collect();
        assert!(leftovers.is_empty(), "markers materialised: {leftovers:?}");
        std::fs::remove_dir_all(&root).ok();
    }

    // A whiteout acts by DELETING, so an escaping one is worse than an
    // escaping regular entry: it must be refused, not clamped.
    #[test]
    fn whiteout_cannot_escape_the_rootfs() {
        let root = scratch();
        let outside = root.parent().unwrap().join(format!(
            "feos-victim-{}-{}",
            std::process::id(),
            SEQ.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::write(&outside, b"not yours").unwrap();

        let name = outside.file_name().unwrap().to_string_lossy().into_owned();
        let err = unpack_layer(
            std::io::Cursor::new(hostile_layer(&format!("../.wh.{name}"))),
            &root,
        )
        .expect_err("escaping whiteout must be refused");
        assert_eq!(err.kind(), std::io::ErrorKind::InvalidData);
        assert!(
            outside.exists(),
            "a whiteout deleted a file outside the rootfs"
        );

        std::fs::remove_file(&outside).ok();
        std::fs::remove_dir_all(&root).ok();
    }

    // Deleting something the lower layers never had is a no-op, not an
    // error — images do this routinely.
    #[test]
    fn whiteout_for_a_missing_path_is_not_an_error() {
        let root = scratch();
        apply(&root, &layer(&[("etc/", None)]));
        apply(&root, &layer(&[("etc/.wh.never-existed", Some(b""))]));
        assert!(root.join("etc").is_dir());
        std::fs::remove_dir_all(&root).ok();
    }

    // ---- one resolution path for every entry type -------------------------

    fn names(dir: &Path) -> Vec<String> {
        let mut names: Vec<String> = std::fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        names
    }

    fn is_symlink(path: &Path) -> bool {
        std::fs::symlink_metadata(path).unwrap().is_symlink()
    }

    // debian:13 ships var/run -> /run. A later layer's entries below it —
    // whatever their type — land where the container sees var/run: in the
    // image's run/, never in the host's. (The tar crate refused all but the
    // hard link: "trying to unpack outside of destination path".)
    #[test]
    fn entries_below_an_absolute_parent_symlink_land_in_the_image() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        let host_run = base.join("run");
        let run = in_image(&root, &host_run);
        std::fs::create_dir_all(&host_run).unwrap();
        std::fs::create_dir_all(&run).unwrap();
        std::fs::create_dir_all(root.join("var")).unwrap();
        std::fs::write(host_run.join("decoy"), b"decoy").unwrap();
        symlink(&host_run, root.join("var/run")).unwrap();
        apply(
            &root,
            &raw_layer(&[
                Raw::file("var/run/app.pid", b"42"),
                Raw::dir("var/run/sub/"),
                Raw::symlink("var/run/current", "app.pid"),
                Raw::link("var/run/hl", "var/run/app.pid"),
                Raw::file("var/run/sub/inner", b"inner"),
            ]),
        );
        assert_eq!(std::fs::read(run.join("app.pid")).unwrap(), b"42");
        assert!(run.join("sub").is_dir());
        assert_eq!(std::fs::read(run.join("sub/inner")).unwrap(), b"inner");
        assert_eq!(
            std::fs::read_link(run.join("current")).unwrap(),
            Path::new("app.pid")
        );
        assert_same_inode(&run.join("app.pid"), &run.join("hl"));
        assert!(is_symlink(&root.join("var/run")));
        assert_eq!(names(&host_run), ["decoy"]);
        std::fs::remove_dir_all(base).unwrap();
    }

    // A parent symlink climbing out of the image is clamped at its root,
    // for files exactly as for hard links (the tar crate refused the file
    // and clamped the hard link).
    #[test]
    fn a_parent_symlink_escaping_the_image_is_clamped_at_its_root() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        std::fs::create_dir_all(&root).unwrap();
        symlink("../..", root.join("esc")).unwrap();
        apply(
            &root,
            &raw_layer(&[
                Raw::file("esc/file", b"inside"),
                Raw::dir("esc/dir/"),
                Raw::symlink("esc/link", "file"),
            ]),
        );
        assert_eq!(std::fs::read(root.join("file")).unwrap(), b"inside");
        assert!(root.join("dir").is_dir());
        assert!(is_symlink(&root.join("link")));
        assert_eq!(names(&base), ["root"]);
        std::fs::remove_dir_all(base).unwrap();
    }

    // containerd: a directory entry replaces a symlink in its place — the
    // link itself, never what it points to — relative or absolute. Entries
    // of the same layer below the name follow the replacement.
    #[test]
    fn a_directory_entry_replaces_a_symlink() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        let host_lib = base.join("usr/lib");
        let abs_lib = in_image(&root, &host_lib);
        for dir in [&host_lib, &abs_lib, &root.join("usr/lib")] {
            std::fs::create_dir_all(dir).unwrap();
            std::fs::write(dir.join("libc.so"), b"libc").unwrap();
        }
        symlink("usr/lib", root.join("lib")).unwrap();
        symlink(&host_lib, root.join("lib64")).unwrap();
        apply(
            &root,
            &raw_layer(&[
                // Resolved through the symlink: lands in usr/lib.
                Raw::file("lib/before", b"before"),
                Raw {
                    mode: 0o750,
                    ..Raw::dir("lib/")
                },
                Raw::file("lib/after", b"after"),
                Raw::dir("lib64/"),
                Raw::file("lib64/after", b"after"),
            ]),
        );
        assert!(!is_symlink(&root.join("lib")));
        assert_eq!(names(&root.join("lib")), ["after"]);
        assert_eq!(names(&root.join("usr/lib")), ["before", "libc.so"]);
        assert!(!is_symlink(&root.join("lib64")));
        assert_eq!(names(&root.join("lib64")), ["after"]);
        assert_eq!(names(&abs_lib), ["libc.so"]);
        assert_eq!(names(&host_lib), ["libc.so"]);
        std::fs::remove_dir_all(base).unwrap();
    }

    // containerd: a non-directory replaces a directory, which goes with
    // its whole subtree (without following symlinks inside it).
    #[test]
    fn files_and_symlinks_replace_directories() {
        use std::os::unix::fs::symlink;
        let base = scratch();
        let root = base.join("root");
        std::fs::create_dir_all(root.join("d/sub")).unwrap();
        std::fs::create_dir_all(root.join("e/sub")).unwrap();
        std::fs::create_dir_all(base.join("outside")).unwrap();
        std::fs::write(base.join("outside/keep"), b"keep").unwrap();
        std::fs::write(root.join("d/sub/x"), b"x").unwrap();
        symlink(base.join("outside"), root.join("d/sub/out")).unwrap();
        std::fs::write(root.join("e/sub/y"), b"y").unwrap();
        apply(
            &root,
            &raw_layer(&[
                Raw::file("d/sub/fresh", b"fresh"),
                Raw::file("d", b"now a file"),
                Raw::symlink("e", "d"),
            ]),
        );
        assert_eq!(std::fs::read(root.join("d")).unwrap(), b"now a file");
        assert_eq!(std::fs::read_link(root.join("e")).unwrap(), Path::new("d"));
        assert_eq!(names(&root), ["d", "e"]);
        assert_eq!(names(&base.join("outside")), ["keep"]);
        std::fs::remove_dir_all(base).unwrap();
    }

    // A file replacing a file is a rename: the name gets a new inode in
    // one step; another hard link to the old inode keeps the old content.
    #[test]
    fn a_file_replaces_a_file_with_a_new_inode() {
        let root = scratch();
        std::fs::write(root.join("f"), b"old").unwrap();
        std::fs::hard_link(root.join("f"), root.join("g")).unwrap();
        apply(&root, &raw_layer(&[Raw::file("f", b"new")]));
        assert_eq!(std::fs::read(root.join("f")).unwrap(), b"new");
        assert_eq!(std::fs::read(root.join("g")).unwrap(), b"old");
        assert_eq!(nlink(&root.join("g")), 1);
        assert_eq!(names(&root), ["f", "g"]);
        std::fs::remove_dir_all(root).unwrap();
    }

    // OCI: an opaque marker hides what the lower layers have in its
    // directory, wherever it stands in the archive — the layer's own
    // entries before it stay, with the directories that lead to them.
    #[test]
    fn an_opaque_marker_after_the_layers_own_entries_keeps_them() {
        let root = scratch();
        apply(
            &root,
            &layer(&[
                ("opt/", None),
                ("opt/old", Some(b"old")),
                ("opt/sub/", None),
                ("opt/sub/old", Some(b"old")),
                ("opt/gone/", None),
                ("opt/gone/old", Some(b"old")),
            ]),
        );
        apply(
            &root,
            &layer(&[
                ("opt/", None),
                ("opt/new", Some(b"new")),
                // No entry for opt/sub itself: it leads to a new entry.
                ("opt/sub/new", Some(b"new")),
                ("opt/.wh..wh..opq", Some(b"")),
                ("opt/later", Some(b"later")),
            ]),
        );
        assert_eq!(names(&root.join("opt")), ["later", "new", "sub"]);
        assert_eq!(names(&root.join("opt/sub")), ["new"]);
        std::fs::remove_dir_all(root).unwrap();
    }

    // OCI: a whiteout only applies to lower layers, so an entry of the
    // same layer survives a marker after it; lower content below such an
    // entry does not.
    #[test]
    fn a_whiteout_after_the_layers_own_entry_keeps_it() {
        let root = scratch();
        apply(
            &root,
            &layer(&[
                ("etc/", None),
                ("etc/a", Some(b"lower")),
                ("etc/d/", None),
                ("etc/d/old", Some(b"old")),
            ]),
        );
        apply(
            &root,
            &layer(&[
                ("etc/a", Some(b"upper")),
                ("etc/d/new", Some(b"new")),
                ("etc/.wh.a", Some(b"")),
                ("etc/.wh.d", Some(b"")),
            ]),
        );
        assert_eq!(std::fs::read(root.join("etc/a")).unwrap(), b"upper");
        assert_eq!(names(&root.join("etc/d")), ["new"]);
        std::fs::remove_dir_all(root).unwrap();
    }

    fn xattr(path: &Path, key: &str) -> Option<Vec<u8>> {
        use std::os::unix::ffi::OsStrExt;
        let path = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
        let key = std::ffi::CString::new(key).unwrap();
        let mut value = vec![0u8; 256];
        // SAFETY: NUL-terminated strings, buffer pointer/length from a Vec.
        let n = unsafe {
            nix::libc::lgetxattr(
                path.as_ptr(),
                key.as_ptr(),
                value.as_mut_ptr().cast(),
                value.len(),
            )
        };
        (n >= 0).then(|| {
            value.truncate(n as usize);
            value
        })
    }

    /// A group of the test process other than its effective one, if it
    /// has any: a non-root chown may hand files to it.
    fn other_group() -> Option<u32> {
        let mut groups = vec![0 as nix::libc::gid_t; 256];
        // SAFETY: buffer pointer/length from a Vec.
        let n = unsafe { nix::libc::getgroups(groups.len() as i32, groups.as_mut_ptr()) };
        groups.truncate(n.max(0) as usize);
        groups.into_iter().find(|&g| g != egid())
    }

    // Owner, full mode, xattrs and mtimes come from the header, as with
    // containerd. Owner before mode: chown clears setuid/setgid (for root
    // too), so the other order would lose them.
    #[test]
    fn metadata_follows_the_header() {
        use std::os::unix::fs::{FileTypeExt, MetadataExt};
        let root = scratch();
        let group = other_group().unwrap_or(egid());
        apply(
            &root,
            &raw_layer(&[
                Raw {
                    mode: 0o4755,
                    mtime: 1_234_567_890,
                    pax: vec![
                        ("SCHILY.xattr.user.origin", b"layer"),
                        ("SCHILY.xattr.trusted.overlay.opaque", b"y"),
                    ],
                    ..Raw::file("bin/su", b"su")
                },
                Raw {
                    mode: 0o2755,
                    gid: group.into(),
                    ..Raw::file("bin/chage", b"chage")
                },
                Raw {
                    mode: 0o1777,
                    mtime: 1_234_567_890,
                    pax: vec![("SCHILY.xattr.user.dir", b"d")],
                    ..Raw::dir("tmp/")
                },
                // Entries below a directory change its mtime; the header's
                // is set when the layer is complete.
                Raw::file("tmp/later", b""),
                Raw {
                    mtime: 1_234_567_890,
                    gid: group.into(),
                    ..Raw::symlink("bin/sh", "su")
                },
                Raw {
                    mtime: 0,
                    ..Raw::file("epoch", b"")
                },
                Raw {
                    mode: 0o640,
                    mtime: 1_234_567_890,
                    ..Raw::new(tar::EntryType::Fifo, b"run/fifo")
                },
            ]),
        );
        let su = std::fs::metadata(root.join("bin/su")).unwrap();
        assert_eq!(su.mode() & 0o7777, 0o4755);
        assert_eq!((su.uid(), su.gid()), (euid(), egid()));
        assert_eq!((su.mtime(), su.atime()), (1_234_567_890, 1_234_567_890));
        assert_eq!(
            xattr(&root.join("bin/su"), "user.origin").unwrap(),
            b"layer"
        );
        assert!(xattr(&root.join("bin/su"), "trusted.overlay.opaque").is_none());
        let chage = std::fs::metadata(root.join("bin/chage")).unwrap();
        assert_eq!((chage.mode() & 0o7777, chage.gid()), (0o2755, group));
        let tmp = std::fs::metadata(root.join("tmp")).unwrap();
        assert_eq!((tmp.mode() & 0o7777, tmp.mtime()), (0o1777, 1_234_567_890));
        assert_eq!(xattr(&root.join("tmp"), "user.dir").unwrap(), b"d");
        let sh = std::fs::symlink_metadata(root.join("bin/sh")).unwrap();
        assert_eq!((sh.mtime(), sh.gid()), (1_234_567_890, group));
        assert_eq!(su.gid(), egid(), "the symlink's group went to its target");
        assert_eq!(std::fs::metadata(root.join("epoch")).unwrap().mtime(), 1);
        let fifo = std::fs::symlink_metadata(root.join("run/fifo")).unwrap();
        assert!(fifo.file_type().is_fifo());
        assert_eq!((fifo.mode() & 0o7777, fifo.mtime()), (0o640, 1_234_567_890));
        std::fs::remove_dir_all(root).unwrap();
    }

    // A directory's deferred mtime belongs to that directory: replaced
    // later in the same layer — itself or an ancestor — the replacement
    // keeps its own.
    #[test]
    fn a_directory_replaced_later_in_the_layer_keeps_the_replacements_mtime() {
        use std::os::unix::fs::MetadataExt;
        let root = scratch();
        apply(
            &root,
            &raw_layer(&[
                Raw {
                    mtime: 1_111_111_111,
                    ..Raw::dir("d/")
                },
                Raw {
                    mtime: 1_111_111_111,
                    ..Raw::dir("d/sub/")
                },
                Raw {
                    mtime: 1_222_222_222,
                    ..Raw::file("d", b"file")
                },
            ]),
        );
        let d = std::fs::symlink_metadata(root.join("d")).unwrap();
        assert!(d.is_file());
        assert_eq!(d.mtime(), 1_222_222_222);
        std::fs::remove_dir_all(root).unwrap();
    }

    // Device nodes are made with mknodat. Only root may: anyone else gets
    // the layer failed with the entry and the reason, nothing half-made.
    #[test]
    fn device_entries_become_device_nodes() {
        use std::os::unix::fs::{FileTypeExt, MetadataExt};
        let root = scratch();
        let entries = [
            Raw {
                mode: 0o666,
                device: (1, 3),
                ..Raw::new(tar::EntryType::Char, b"dev/null")
            },
            Raw {
                mode: 0o660,
                device: (7, 0),
                ..Raw::new(tar::EntryType::Block, b"dev/loop0")
            },
        ];
        if euid() == 0 {
            try_apply(&root, &entries).expect("root makes device nodes");
            let null = std::fs::symlink_metadata(root.join("dev/null")).unwrap();
            assert!(null.file_type().is_char_device());
            assert_eq!(null.mode() & 0o7777, 0o666);
            let rdev = null.rdev();
            let numbers = (nix::sys::stat::major(rdev), nix::sys::stat::minor(rdev));
            assert_eq!(numbers, (1, 3));
            let lo = std::fs::symlink_metadata(root.join("dev/loop0")).unwrap();
            assert!(lo.file_type().is_block_device());
            let rdev = lo.rdev();
            let numbers = (nix::sys::stat::major(rdev), nix::sys::stat::minor(rdev));
            assert_eq!(numbers, (7, 0));
        } else {
            let error = try_apply(&root, &entries).expect_err("mknod needs privilege");
            let text = error.to_string();
            assert!(text.contains("character device dev/null"), "{text}");
            assert!(text.contains("Operation not permitted"), "{text}");
            assert!(names(&root.join("dev")).is_empty());
        }
        std::fs::remove_dir_all(root).unwrap();
    }

    // File capabilities (debian-ai's mtr-packet, ping elsewhere) arrive as
    // security.capability in the entry's PAX records. Setting them needs
    // CAP_SETFCAP; without it the layer fails, naming the attribute.
    #[test]
    fn file_capabilities_are_applied() {
        // VFS_CAP_REVISION_2, effective; permitted: cap_net_raw (13).
        let cap: &[u8] = &[
            1, 0, 0, 2, 0, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
        ];
        let root = scratch();
        let entries = [Raw {
            mode: 0o755,
            pax: vec![("SCHILY.xattr.security.capability", cap)],
            ..Raw::file("usr/bin/mtr-packet", b"elf")
        }];
        if euid() == 0 {
            try_apply(&root, &entries).expect("root sets file capabilities");
            let path = root.join("usr/bin/mtr-packet");
            assert_eq!(xattr(&path, "security.capability").unwrap(), cap);
        } else {
            let error = try_apply(&root, &entries).expect_err("needs CAP_SETFCAP");
            let text = error.to_string();
            assert!(text.contains("set xattr security.capability"), "{text}");
            assert!(names(&root.join("usr/bin")).is_empty());
        }
        std::fs::remove_dir_all(root).unwrap();
    }

    // The root's own "./" entry is skipped, as by tar and containerd; a
    // non-directory naming the root cannot be applied and fails.
    #[test]
    fn entries_naming_the_root() {
        use std::os::unix::fs::PermissionsExt;
        let root = scratch();
        std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700)).unwrap();
        apply(
            &root,
            &raw_layer(&[
                Raw {
                    mode: 0o755,
                    ..Raw::dir("./")
                },
                Raw::dir("/"),
            ]),
        );
        let mode = std::fs::metadata(&root).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o700);
        // (A GNU-format regular entry "./" is an old-style directory.)
        for entry in [Raw::file(".", b"x"), Raw::symlink("/", "x")] {
            let error = try_apply(&root, &[entry]).expect_err("a non-directory at the root");
            assert!(error.to_string().contains("empty image path"), "{error}");
        }
        assert!(names(&root).is_empty());
        std::fs::remove_dir_all(root).unwrap();
    }

    // Pre-POSIX archives store directories as regular entries with a
    // trailing slash; the tar crate created directories for them.
    #[test]
    fn old_style_directory_entries() {
        let root = scratch();
        apply(
            &root,
            &raw_layer(&[
                Raw {
                    old: true,
                    mode: 0o755,
                    ..Raw::file("olddir/", b"")
                },
                Raw {
                    old: true,
                    ..Raw::file("olddir/file", b"content")
                },
            ]),
        );
        assert!(root.join("olddir").is_dir());
        assert_eq!(std::fs::read(root.join("olddir/file")).unwrap(), b"content");
        std::fs::remove_dir_all(root).unwrap();
    }

    // Names come from GNU long-name/long-link and PAX records as bytes.
    #[test]
    fn long_and_non_utf8_names_reach_every_entry_type() {
        use std::os::unix::ffi::OsStrExt;
        let root = scratch();
        let deep = format!("{}/leaf", ["segment"; 20].join("/"));
        let target = format!("../{}", ["t"; 60].join("/"));
        apply(
            &root,
            &raw_layer(&[
                Raw::file(&deep, b"deep"),
                Raw::symlink("long-link", &target),
                Raw {
                    pax: vec![("path", b"pax/\xffname"), ("linkpath", b"\xfetarget")],
                    ..Raw::symlink("ignored", "ignored")
                },
                Raw {
                    pax: vec![("path", b"pax/\xfdhard"), ("linkpath", b"pax/\xffname")],
                    ..Raw::link("ignored", "ignored")
                },
            ]),
        );
        assert_eq!(std::fs::read(root.join(&deep)).unwrap(), b"deep");
        assert_eq!(
            std::fs::read_link(root.join("long-link")).unwrap(),
            Path::new(&target)
        );
        let pax = root.join("pax");
        let name = pax.join(std::ffi::OsStr::from_bytes(b"\xffname"));
        assert_eq!(
            std::fs::read_link(&name).unwrap().as_os_str().as_bytes(),
            b"\xfetarget"
        );
        assert_same_inode(&name, &pax.join(std::ffi::OsStr::from_bytes(b"\xfdhard")));
        assert!(!root.join("ignored").exists());
        std::fs::remove_dir_all(root).unwrap();
    }

    // Errors say which entry failed, at which step, and why.
    #[test]
    fn errors_name_the_entry_and_the_cause() {
        use std::os::unix::fs::symlink;
        let root = scratch();
        std::fs::write(root.join("plain"), b"x").unwrap();
        symlink("/missing/dir", root.join("dangling")).unwrap();
        for (entry, expected) in [
            (
                Raw::file("plain/child", b""),
                "file plain/child: open plain",
            ),
            (Raw::dir("plain/sub/"), "directory plain/sub/: open plain"),
            (Raw::symlink("plain/l", "x"), "symlink plain/l: open plain"),
            (
                Raw::file("dangling/x", b""),
                "file dangling/x: dangling exists but does not resolve",
            ),
        ] {
            let error = try_apply(&root, &[entry]).expect_err("must fail");
            let text = error.to_string();
            assert!(text.contains(expected), "{text}");
            if !expected.contains("dangling") {
                assert!(text.contains("Not a directory"), "{text}");
            }
        }
        assert_eq!(names(&root), ["dangling", "plain"]);
        std::fs::remove_dir_all(root).unwrap();
    }

    // A transfer cut off inside an entry's data is an error (the streamed
    // first layer retries on it), not a short file.
    #[test]
    fn an_archive_ending_inside_an_entry_fails() {
        let root = scratch();
        let mut tar = raw_tar(&[Raw::file("big", &[7u8; 4096])]);
        tar.truncate(512 + 1000);
        let error = unpack_layer(std::io::Cursor::new(gzip(&tar)), &root)
            .expect_err("truncated entry must fail");
        assert_eq!(error.kind(), std::io::ErrorKind::UnexpectedEof, "{error}");
        assert!(error.to_string().contains("file big"), "{error}");
        assert!(names(&root).is_empty());
        std::fs::remove_dir_all(root).unwrap();
    }
}
