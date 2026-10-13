// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Whiteout markers: `.wh.<name>` deletes `<name>` from the lower layers,
//! `.wh..wh..opq` empties its directory of what the lower layers put there.
//!
//! Marker paths are image-rooted like every other entry (a leading "/" is
//! the image root, ".." is refused; see [`rootfs::image_relative`]), and
//! the marker's parent goes through the same resolver as every entry
//! (RESOLVE_IN_ROOT), so in-image parent symlinks, absolute ones included,
//! resolve the way the container will see them and never leave the
//! extraction root. Deletion then runs through unlinkat on that handle and
//! no-follow child opens: a final symlink is unlinked, never followed, and
//! a parent renamed or swapped after pinning cannot redirect the deletion.
//! Names keep their Unix bytes (non-UTF-8 is fine).
//!
//! A missing parent or target is a no-op: images routinely white out
//! things the lower layers never had. A parent that exists but is not a
//! directory, a degenerate target name (`.wh.`, `.wh..`, `.wh...`) or any
//! IO error fails the pull, so an incompletely applied deletion cannot
//! pass for success.
//!
//! OCI: whiteouts apply to the layers below only. Entries the current
//! layer has already put in place stay, wherever the marker stands in the
//! archive — an opaque marker after the layer's own files in its directory
//! clears only what was there before — and a directory that still leads
//! to such an entry stays too. containerd's applier does the same for
//! opaque markers.

use std::ffi::OsStr;
use std::io;
use std::os::unix::ffi::OsStrExt;
use std::path::Path;

use super::layer::Layer;
use super::rootfs;

impl Layer {
    /// `.wh.<target>`: delete `target` (with its subtree) next to the marker.
    pub(super) fn whiteout(&mut self, marker: &Path, target: &OsStr) -> io::Result<()> {
        // Validate the derived name before any filesystem access.
        let bytes = target.as_bytes();
        if bytes.is_empty() || bytes == b"." || bytes == b".." {
            return Err(rootfs::invalid("invalid whiteout target name"));
        }
        let marker = rootfs::image_relative(marker)?;
        let Some(dir) = self.root.dir(marker.parent().unwrap_or(Path::new("")))? else {
            return Ok(());
        };
        // A deletion may take a directory or symlink a cached parent
        // resolved through.
        self.root.invalidate();
        rootfs::remove(&dir, target, Some(&self.members))
    }

    /// `.wh..wh..opq`: empty the marker's directory, keeping the directory.
    pub(super) fn opaque(&mut self, marker: &Path) -> io::Result<()> {
        let marker = rootfs::image_relative(marker)?;
        let Some(dir) = self.root.dir(marker.parent().unwrap_or(Path::new("")))? else {
            return Ok(());
        };
        self.root.invalidate();
        rootfs::clear(&dir, Some(&self.members))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pinned_parent_does_not_follow_replacement() {
        use std::os::unix::fs::symlink;
        let base = std::env::temp_dir().join(format!("feos-whiteout-swap-{}", std::process::id()));
        let root = base.join("root");
        let outside = base.join("outside");
        std::fs::create_dir_all(root.join("parent")).unwrap();
        std::fs::create_dir_all(&outside).unwrap();
        std::fs::write(root.join("parent/victim"), "delete").unwrap();
        std::fs::write(outside.join("victim"), "keep").unwrap();
        let mut layer = Layer::open(&root).unwrap();
        let pinned = layer.root.dir(Path::new("parent")).unwrap().unwrap();
        std::fs::rename(root.join("parent"), root.join("old-parent")).unwrap();
        symlink(&outside, root.join("parent")).unwrap();
        rootfs::clear(&pinned, None).unwrap();
        assert!(!root.join("old-parent/victim").exists());
        assert_eq!(
            std::fs::read_to_string(outside.join("victim")).unwrap(),
            "keep"
        );
        // The host-absolute replacement resolves inside the image root,
        // where it names nothing.
        layer.root.invalidate();
        assert!(layer.root.dir(Path::new("parent")).unwrap().is_none());
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn deep_trees_fail_cleanly_instead_of_exhausting_fds() {
        let base = std::env::temp_dir().join(format!("feos-whiteout-deep-{}", std::process::id()));
        let mut deep = base.join("root/top");
        for _ in 0..300 {
            deep.push("d");
        }
        std::fs::create_dir_all(&deep).unwrap();
        let mut layer = Layer::open(&base.join("root")).unwrap();
        let error = layer
            .whiteout(Path::new(".wh.top"), OsStr::new("top"))
            .expect_err("over-deep tree must be refused");
        assert!(error.to_string().contains("too deep"), "{error}");
        std::fs::remove_dir_all(base).unwrap();
    }
}
