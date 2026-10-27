// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Applying one layer tar onto the image root: every entry type goes
//! through the one image-rooted path of [`rootfs`] (parent resolution,
//! creation, replacement); the tar crate only parses the archive.
//!
//! Metadata comes from the entry header, as containerd's applier takes it:
//! owner and group, the full mode (setuid, setgid and sticky included),
//! the extended attributes of the entry's PAX `SCHILY.xattr.*` records
//! (file capabilities such as mtr-packet's security.capability) and the
//! mtime. Owner first, because chown clears setuid/setgid and file
//! capabilities; then xattrs, mode, times — all on the open file or with
//! no-follow *at calls, symlinks included (their mode is meaningless and
//! left alone). `trusted.*` attributes are the administrator's namespace,
//! not an image's: they are skipped, as containerd skips them. Any other
//! failure — an id the filesystem refuses, an attribute it does not
//! support — fails the entry. Directory mtimes are set once the layer is
//! complete, since adding entries changes them. Times are whole seconds
//! with atime = mtime, a 0 mtime becoming 1, as the tar crate wrote them.
//! Hard links take no metadata of their own: they share the inode they
//! name, which got its metadata from its own entry. Character and block
//! devices and FIFOs are created as such (mknodat).

use std::ffi::{OsStr, OsString};
use std::fs::File;
use std::io::{self, Read, Write};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};

use nix::fcntl::{openat, AtFlags, OFlag};
use nix::sys::stat::{
    fchmod, fstatat, futimens, makedev, mknodat, utimensat, Mode, SFlag, UtimensatFlags,
};
use nix::sys::time::TimeSpec;
use nix::unistd::{fchown, fchownat, symlinkat, Gid, Uid};

use super::rootfs::{self, step, Key, Members, Pinned, Root};

/// AUFS whiteout markers, as OCI defines deletions between layers. A layer
/// cannot carry "remove this" any other way: it is a tar, and a tar only
/// adds. `.wh.<name>` deletes `<name>` from everything below; the opaque
/// marker deletes the whole content of its directory.
const WHITEOUT_PREFIX: &[u8] = b".wh.";
const WHITEOUT_OPAQUE: &[u8] = b".wh..wh..opq";

const XATTR_RECORD: &[u8] = b"SCHILY.xattr.";
const XATTR_TRUSTED: &[u8] = b"trusted.";

/// Copy buffer for file contents: large reads keep the decompressor and
/// the write path in big chunks.
const COPY_BUFFER: usize = 256 * 1024;

pub(super) struct Layer {
    pub(super) root: Root,
    /// What this layer has put in place so far; whiteout markers leave it
    /// alone (see whiteout.rs).
    pub(super) members: Members,
    /// Directory mtimes, applied by [`Layer::finish`].
    directory_times: Vec<DirectoryTime>,
    buffer: Box<[u8]>,
}

struct DirectoryTime {
    parent: PathBuf,
    name: OsString,
    /// The directory this entry made or kept: a later entry of the layer
    /// may have replaced it, and then it is not this entry's to time.
    key: Key,
    mtime: TimeSpec,
}

/// Metadata from the entry header, applied as described in the module
/// documentation.
struct Meta {
    mode: Mode,
    uid: Uid,
    gid: Gid,
    mtime: TimeSpec,
    xattrs: Vec<(OsString, Vec<u8>)>,
}

fn id(value: u64, what: &str) -> io::Result<u32> {
    // (u32::MAX is chown's "leave unchanged", not an id.)
    u32::try_from(value)
        .ok()
        .filter(|&id| id != u32::MAX)
        .ok_or_else(|| rootfs::invalid(&format!("{what} {value} out of range")))
}

impl Meta {
    fn of<R: Read>(entry: &mut tar::Entry<R>, path: &Path) -> io::Result<Meta> {
        let header = entry.header();
        let mode = header.mode().map_err(|e| step("mode", e))?;
        let uid = id(header.uid().map_err(|e| step("uid", e))?, "uid")?;
        let gid = id(header.gid().map_err(|e| step("gid", e))?, "gid")?;
        let mtime = header.mtime().map_err(|e| step("mtime", e))?;
        // The tar crate never wrote a 0 mtime (some tools mishandle it).
        let mtime =
            i64::try_from(mtime.max(1)).map_err(|_| rootfs::invalid("mtime out of range"))?;
        let mut xattrs = Vec::new();
        if let Some(records) = entry.pax_extensions().map_err(|e| step("pax records", e))? {
            for record in records {
                let record = record.map_err(|e| step("pax record", e))?;
                let Some(key) = record.key_bytes().strip_prefix(XATTR_RECORD) else {
                    continue;
                };
                if key.starts_with(XATTR_TRUSTED) {
                    log::warn!(
                        "image: {}: skipping xattr {} (trusted.* is not an image's to set)",
                        path.display(),
                        String::from_utf8_lossy(key)
                    );
                    continue;
                }
                xattrs.push((
                    OsStr::from_bytes(key).to_owned(),
                    record.value_bytes().to_vec(),
                ));
            }
        }
        Ok(Meta {
            mode: Mode::from_bits_truncate(mode & 0o7777),
            uid: Uid::from_raw(uid),
            gid: Gid::from_raw(gid),
            mtime: TimeSpec::new(mtime, 0),
            xattrs,
        })
    }

    fn file(&self, file: &File) -> io::Result<()> {
        fchown(file, Some(self.uid), Some(self.gid)).map_err(|e| self.owner_error(e))?;
        for (key, value) in &self.xattrs {
            rootfs::set_xattr(file, key, value).map_err(|e| xattr_error(key, e))?;
        }
        fchmod(file, self.mode).map_err(|e| step("set mode", e))?;
        futimens(file, &self.mtime, &self.mtime).map_err(|e| step("set mtime", e))
    }

    /// Owner and xattrs of `name` itself, then — where they apply — its
    /// mode and times.
    fn at(&self, dir: &Pinned, name: &OsStr, mode: bool, times: bool) -> io::Result<()> {
        fchownat(
            dir,
            name,
            Some(self.uid),
            Some(self.gid),
            AtFlags::AT_SYMLINK_NOFOLLOW,
        )
        .map_err(|e| self.owner_error(e))?;
        for (key, value) in &self.xattrs {
            rootfs::set_xattr_at(dir, name, key, value).map_err(|e| xattr_error(key, e))?;
        }
        if mode {
            rootfs::chmod_at(dir, name, self.mode.bits()).map_err(|e| step("set mode", e))?;
        }
        if times {
            utimensat(
                dir,
                name,
                &self.mtime,
                &self.mtime,
                UtimensatFlags::NoFollowSymlink,
            )
            .map_err(|e| step("set mtime", e))?;
        }
        Ok(())
    }

    fn owner_error(&self, error: nix::Error) -> io::Error {
        step(format_args!("set owner {}:{}", self.uid, self.gid), error)
    }
}

fn xattr_error(key: &OsStr, error: nix::Error) -> io::Error {
    step(format_args!("set xattr {}", key.to_string_lossy()), error)
}

impl Layer {
    pub(super) fn open(rootfs: &Path) -> io::Result<Layer> {
        Ok(Layer {
            root: Root::open(rootfs)?,
            members: Members::default(),
            directory_times: Vec::new(),
            buffer: vec![0; COPY_BUFFER].into_boxed_slice(),
        })
    }

    /// Apply one archive entry. Errors name the entry and the step that
    /// failed, with the underlying cause.
    pub(super) fn apply<R: Read>(&mut self, entry: &mut tar::Entry<R>) -> io::Result<()> {
        let path = entry.path()?.into_owned();
        // Bytes, not str: a non-UTF-8 marker is still a marker.
        let name = path.file_name().unwrap_or_default().as_bytes();

        if name == WHITEOUT_OPAQUE || name.starts_with(WHITEOUT_PREFIX) {
            // A marker whose path escapes the rootfs is the same class of
            // hostile input as an escaping regular entry — and far more
            // dangerous, because acting on it DELETES. ".." is refused,
            // parent resolution is confined to the image root.
            let result = if name == WHITEOUT_OPAQUE {
                self.opaque(&path)
            } else {
                self.whiteout(&path, OsStr::from_bytes(&name[WHITEOUT_PREFIX.len()..]))
            };
            return result.map_err(|e| step(format_args!("whiteout {}", path.display()), e));
        }

        let kind = entry.header().entry_type();
        if kind.is_hard_link() {
            let target = entry.link_name()?.ok_or_else(|| {
                io::Error::new(io::ErrorKind::InvalidData, "hard link has no target")
            })?;
            return self.hard_link(&path, &target).map_err(|e| {
                step(
                    format_args!("hard link {} -> {}", path.display(), target.display()),
                    e,
                )
            });
        }
        if kind.is_pax_global_extensions()
            || kind.is_pax_local_extensions()
            || kind.is_gnu_longname()
            || kind.is_gnu_longlink()
        {
            // Metadata records, not files (the iterator folds the ones
            // describing an entry into it).
            return Ok(());
        }

        let (what, result) = if kind.is_dir() {
            ("directory", self.directory(&path, entry))
        } else if kind.is_symlink() {
            ("symlink", self.symlink(&path, entry))
        } else if kind.is_character_special() {
            (
                "character device",
                self.special(&path, entry, SFlag::S_IFCHR),
            )
        } else if kind.is_block_special() {
            ("block device", self.special(&path, entry, SFlag::S_IFBLK))
        } else if kind.is_fifo() {
            ("fifo", self.special(&path, entry, SFlag::S_IFIFO))
        } else if old_style_directory(entry) {
            ("directory", self.directory(&path, entry))
        } else {
            // Regular files, and — as with the tar crate — every type not
            // created specially: POSIX says unknown types read as regular
            // files.
            ("file", self.file(&path, entry))
        };
        result.map_err(|e| step(format_args!("{what} {}", path.display()), e))
    }

    /// Complete the layer: the directory mtimes its entries carried.
    pub(super) fn finish(&mut self) -> io::Result<()> {
        for time in std::mem::take(&mut self.directory_times) {
            let what = || time.parent.join(&time.name);
            let dir = match self.root.dir(&time.parent) {
                Ok(Some(dir)) => dir,
                // An ancestor was removed or replaced by a non-directory
                // later in this layer: the directory is gone with it.
                Ok(None) => continue,
                Err(e) if e.kind() == io::ErrorKind::NotADirectory => continue,
                Err(e) => return Err(step(format_args!("directory {}", what().display()), e)),
            };
            match fstatat(&*dir, time.name.as_os_str(), AtFlags::AT_SYMLINK_NOFOLLOW) {
                Ok(st) if rootfs::key(&st) == time.key => {}
                // Replaced or removed later in this layer.
                Ok(_) | Err(nix::errno::Errno::ENOENT) => continue,
                Err(e) => return Err(step(format_args!("directory {}", what().display()), e)),
            }
            utimensat(
                &*dir,
                time.name.as_os_str(),
                &time.mtime,
                &time.mtime,
                UtimensatFlags::NoFollowSymlink,
            )
            .map_err(|e| step(format_args!("directory {}: set mtime", what().display()), e))?;
        }
        Ok(())
    }

    fn directory<R: Read>(&mut self, path: &Path, entry: &mut tar::Entry<R>) -> io::Result<()> {
        let Some((parent, name)) = rootfs::split_root(path)? else {
            // The root's own entry ("./"): the extraction root is not the
            // layer's to replace or re-mode, as with tar and containerd.
            return Ok(());
        };
        let meta = Meta::of(entry, path)?;
        let dir = self.root.parent(&parent)?;
        self.root.make_dir(&dir, &name)?;
        meta.at(&dir, &name, true, false)?;
        let st = fstatat(&*dir, name.as_os_str(), AtFlags::AT_SYMLINK_NOFOLLOW)
            .map_err(|e| step("inspect", e))?;
        self.directory_times.push(DirectoryTime {
            parent,
            name: name.clone(),
            key: rootfs::key(&st),
            mtime: meta.mtime,
        });
        self.members.insert(&dir, &name);
        Ok(())
    }

    fn symlink<R: Read>(&mut self, path: &Path, entry: &mut tar::Entry<R>) -> io::Result<()> {
        let meta = Meta::of(entry, path)?;
        let target = entry
            .link_name_bytes()
            .ok_or_else(|| rootfs::invalid("symlink has no target"))?
            .into_owned();
        if target.is_empty() {
            return Err(rootfs::invalid("symlink target is empty"));
        }
        let target = OsStr::from_bytes(&target);
        let (parent, name) = rootfs::split(path)?;
        let dir = self.root.parent(&parent)?;
        let (temporary, ()) = self
            .root
            .temporary(&dir, &name, |d, t| symlinkat(target, d, t))?;
        meta.at(&dir, temporary.name(), false, true)?;
        self.root.commit(temporary, &name)?;
        self.members.insert(&dir, &name);
        Ok(())
    }

    /// Character and block devices and FIFOs.
    fn special<R: Read>(
        &mut self,
        path: &Path,
        entry: &mut tar::Entry<R>,
        kind: SFlag,
    ) -> io::Result<()> {
        let meta = Meta::of(entry, path)?;
        let device = if kind == SFlag::S_IFIFO {
            0
        } else {
            let header = entry.header();
            let major = header.device_major().map_err(|e| step("device major", e))?;
            let minor = header.device_minor().map_err(|e| step("device minor", e))?;
            match (major, minor) {
                (Some(major), Some(minor)) => makedev(major.into(), minor.into()),
                _ => return Err(rootfs::invalid("device numbers missing from the header")),
            }
        };
        let (parent, name) = rootfs::split(path)?;
        let dir = self.root.parent(&parent)?;
        let (temporary, ()) = self.root.temporary(&dir, &name, |d, t| {
            mknodat(d, t, kind, Mode::from_bits_truncate(0o600), device)
        })?;
        meta.at(&dir, temporary.name(), true, true)?;
        self.root.commit(temporary, &name)?;
        self.members.insert(&dir, &name);
        Ok(())
    }

    fn file<R: Read>(&mut self, path: &Path, entry: &mut tar::Entry<R>) -> io::Result<()> {
        let meta = Meta::of(entry, path)?;
        let size = entry.size();
        let (parent, name) = rootfs::split(path)?;
        let dir = self.root.parent(&parent)?;
        // O_EXCL|O_NOFOLLOW: a fresh inode under a fresh name, never an
        // existing file or whatever a symlink of that name points to.
        let (temporary, fd) = self.root.temporary(&dir, &name, |d, t| {
            openat(
                d,
                t,
                OFlag::O_WRONLY
                    | OFlag::O_CREAT
                    | OFlag::O_EXCL
                    | OFlag::O_NOFOLLOW
                    | OFlag::O_CLOEXEC,
                Mode::from_bits_truncate(0o600),
            )
        })?;
        let mut file = File::from(fd);
        copy_exact(entry, size, &mut file, &mut self.buffer)?;
        // After the data: writing would clear setuid and capabilities.
        meta.file(&file)?;
        drop(file);
        self.root.commit(temporary, &name)?;
        self.members.insert(&dir, &name);
        Ok(())
    }
}

/// Old (pre-POSIX) archives store directories as regular entries with a
/// trailing slash; the tar crate honoured that for headers that are not
/// ustar, and so does this.
fn old_style_directory<R: Read>(entry: &tar::Entry<R>) -> bool {
    entry.header().as_ustar().is_none() && entry.path_bytes().ends_with(b"/")
}

/// Exactly `size` bytes of entry data into `file`; an archive that ends
/// early fails the entry instead of leaving a short file.
fn copy_exact(
    entry: &mut impl Read,
    size: u64,
    file: &mut File,
    buffer: &mut [u8],
) -> io::Result<()> {
    let mut copied = 0u64;
    loop {
        let n = match entry.read(buffer) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(step("read archive", e)),
        };
        file.write_all(&buffer[..n]).map_err(|e| step("write", e))?;
        copied += n as u64;
    }
    if copied != size {
        return Err(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            format!("archive ended after {copied} of {size} bytes"),
        ));
    }
    Ok(())
}
