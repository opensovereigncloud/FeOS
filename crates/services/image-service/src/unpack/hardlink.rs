// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! OCI hard links name entries in the image, including when linkname is
//! absolute. Both parents go through the one image-rooted resolver of
//! [`rootfs`] (RESOLVE_IN_ROOT); the link is made with linkat on those
//! handles without following the final source symlink, under a temporary
//! name, and renamed into place like every other non-directory entry.

use std::io;
use std::path::Path;

use nix::fcntl::AtFlags;
use nix::unistd::linkat;

use super::layer::Layer;
use super::rootfs::{self, Kind};

impl Layer {
    /// `path` becomes another name of the inode `target` names.
    pub(super) fn hard_link(&mut self, path: &Path, target: &Path) -> io::Result<()> {
        let (parent, name) = rootfs::split(path)?;
        let (target_parent, target_name) = rootfs::split(target)?;
        let source = self.root.dir(&target_parent)?.ok_or_else(|| {
            io::Error::new(io::ErrorKind::NotFound, "hard-link source does not exist")
        })?;
        match rootfs::kind_at(&*source, &target_name)? {
            Some(Kind::Directory) => {
                return Err(rootfs::invalid("hard-link source is a directory"))
            }
            Some(_) => {}
            None => {
                return Err(io::Error::new(
                    io::ErrorKind::NotFound,
                    "hard-link source does not exist",
                ))
            }
        }
        let dir = self.root.parent(&parent)?;
        // linkat without AT_SYMLINK_FOLLOW links a final source symlink
        // itself. The link exists before anything at the destination is
        // touched: a missing source leaves the destination intact, and a
        // source inside a directory the link replaces keeps its inode.
        let (temporary, ()) = self.root.temporary(&dir, &name, |d, t| {
            linkat(&*source, target_name.as_os_str(), d, t, AtFlags::empty())
        })?;
        self.root.commit(temporary, &name)?;
        self.members.insert(&dir, &name);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsStr;

    #[test]
    fn temporary_name_collisions_preserve_valid_image_entries() {
        let root =
            std::env::temp_dir().join(format!("feos-hardlink-collisions-{}", std::process::id()));
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("source"), b"source").unwrap();
        for index in 0..8 {
            std::fs::write(root.join(format!(".feos-unpack-{index}")), b"keep").unwrap();
        }
        let mut layer = Layer::open(&root).unwrap();
        layer
            .hard_link(Path::new("destination"), Path::new("source"))
            .unwrap();
        assert_eq!(std::fs::read(root.join("destination")).unwrap(), b"source");
        for index in 0..8 {
            assert_eq!(
                std::fs::read(root.join(format!(".feos-unpack-{index}"))).unwrap(),
                b"keep"
            );
        }
        assert_eq!(std::fs::read_dir(&root).unwrap().count(), 10);
        std::fs::remove_dir_all(root).unwrap();
    }

    // The Perl restate: rename(2) of a name onto another name of the same
    // inode does nothing, so the temporary name has to be removed apart
    // from the rename.
    #[test]
    fn restating_a_link_over_itself_leaves_no_temporary_name() {
        let root =
            std::env::temp_dir().join(format!("feos-hardlink-restate-{}", std::process::id()));
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("perl5.40.1"), b"perl").unwrap();
        std::fs::hard_link(root.join("perl5.40.1"), root.join("perl")).unwrap();
        let mut layer = Layer::open(&root).unwrap();
        for _ in 0..2 {
            layer
                .hard_link(Path::new("perl"), Path::new("perl5.40.1"))
                .unwrap();
        }
        let mut names: Vec<_> = std::fs::read_dir(&root)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        names.sort();
        assert_eq!(names, ["perl", "perl5.40.1"]);
        std::fs::remove_dir_all(root).unwrap();
    }

    // Removing the temporary name is not best effort: one that stays
    // would be a file in the container nobody put there.
    #[test]
    fn a_temporary_name_that_cannot_be_removed_fails_the_layer() {
        let root = std::env::temp_dir().join(format!("feos-hardlink-stuck-{}", std::process::id()));
        // unlinkat without AT_REMOVEDIR refuses a directory (EISDIR).
        std::fs::create_dir_all(root.join(".feos-unpack-7/blocker")).unwrap();
        let dir = std::fs::File::open(&root).unwrap();
        let error = rootfs::remove_temporary(&dir, OsStr::new(".feos-unpack-7"))
            .expect_err("a temporary name that stays must fail");
        assert!(error.to_string().contains(".feos-unpack-7"), "{error}");
        // Already gone is the normal case after a real rename.
        rootfs::remove_temporary(&dir, OsStr::new(".feos-unpack-8")).unwrap();
        std::fs::remove_dir_all(root).unwrap();
    }
}
