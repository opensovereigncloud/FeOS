// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Image-rooted path handling: the one resolution path every layer entry
//! goes through — regular files, directories, symlinks, hard links, device
//! nodes and whiteout markers alike.
//!
//! Archive paths and hard-link targets name entries *in the image*: a
//! leading "/" is the image root and ".." is refused outright. Parent
//! directories are opened with openat2 RESOLVE_IN_ROOT anchored at the
//! extraction root, so in-image symlinks along the way — absolute ones
//! included (Debian's var/run -> /run, usr/lib/ssl/certs -> /etc/ssl/certs)
//! — resolve the way the container will see them, ".." in a link target is
//! clamped at the root, and magic links are refused. Whatever creates,
//! replaces or removes then goes through *at calls on that pinned handle
//! with a single archive-supplied final component, never following it, so
//! a parent swapped for a symlink after opening cannot redirect them. This
//! protects image application from hostile layers; it is not an isolation
//! boundary against another root process in the guest.
//!
//! Replacement follows containerd's applier: an existing entry is replaced
//! unless both it and the new entry are directories, in which case the
//! directory stays and only takes the new entry's metadata. A non-directory
//! is created complete under a temporary name and renamed over its
//! destination, so a file replacing a file is atomic; a directory in its
//! way is removed with its subtree first. A directory replaces a file or a
//! symlink by unlinking it (the symlink itself, never what it points to).

use std::collections::{HashMap, HashSet};
use std::ffi::{CString, OsStr, OsString};
use std::fmt::Display;
use std::fs::File;
use std::io;
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, OwnedFd};
use std::os::unix::ffi::OsStrExt;
use std::path::{Component, Path, PathBuf};
use std::rc::Rc;

use nix::dir::Dir as Stream;
use nix::errno::Errno;
use nix::fcntl::{renameat, AtFlags, OFlag, ResolveFlag};
use nix::libc;
use nix::sys::stat::{fstat, fstatat, mkdirat, FileStat, Mode, SFlag};
use nix::unistd::{unlinkat, UnlinkatFlags};
use nix::NixPath;

use feos_utils::fs::openat2_confined;

/// Deletion recurses one open directory per level; cap it well below the
/// default 1024-fd soft limit instead of exhausting fds on a hostile,
/// absurdly deep tree. Real images stay far below this.
const MAX_DEPTH: usize = 256;

/// Temporary names are not reserved: a valid image may contain any of
/// them, so a taken one is skipped, never touched.
const TEMPORARY_PREFIX: &str = ".feos-unpack-";

pub(super) fn invalid(what: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, what.to_string())
}

/// `error`, prefixed with the step that failed; the kind is kept.
pub(super) fn step(what: impl Display, error: impl Into<io::Error>) -> io::Error {
    let error = error.into();
    io::Error::new(error.kind(), format!("{what}: {error}"))
}

/// Normalise an archive path into an image-root-relative one: "/" and "."
/// are dropped, any ".." is refused (as tar's unpack_in always did) rather
/// than resolved lexically. Empty means the root itself.
pub(super) fn image_relative(path: &Path) -> io::Result<PathBuf> {
    let mut relative = PathBuf::new();
    for component in path.components() {
        match component {
            Component::RootDir | Component::CurDir => {}
            Component::Normal(name) => relative.push(name),
            Component::ParentDir | Component::Prefix(_) => {
                return Err(invalid("image path traversal"));
            }
        }
    }
    Ok(relative)
}

/// [`image_relative`], split into (parent, final name); None for the root
/// itself, which has no name.
pub(super) fn split_root(path: &Path) -> io::Result<Option<(PathBuf, OsString)>> {
    let relative = image_relative(path)?;
    Ok(relative.file_name().map(|name| {
        let parent = relative.parent().unwrap_or(Path::new("")).to_path_buf();
        (parent, name.to_os_string())
    }))
}

/// [`split_root`] for paths that must name an entry below the root.
pub(super) fn split(path: &Path) -> io::Result<(PathBuf, OsString)> {
    split_root(path)?.ok_or_else(|| invalid("empty image path"))
}

/// Identity of a directory: (st_dev, st_ino).
pub(super) type Key = (u64, u64);

pub(super) fn key(st: &FileStat) -> Key {
    (st.st_dev, st.st_ino)
}

/// A directory of the image, pinned as an O_PATH handle.
pub(super) struct Pinned {
    fd: OwnedFd,
    key: Key,
}

impl AsFd for Pinned {
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.fd.as_fd()
    }
}

/// What an existing name below a pinned directory is, not following it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(super) enum Kind {
    Directory,
    Symlink,
    Other,
}

pub(super) fn kind_at(dir: impl AsFd, name: &OsStr) -> io::Result<Option<Kind>> {
    match fstatat(dir, name, AtFlags::AT_SYMLINK_NOFOLLOW) {
        Ok(st) => Ok(Some(
            match SFlag::from_bits_truncate(st.st_mode) & SFlag::S_IFMT {
                SFlag::S_IFDIR => Kind::Directory,
                SFlag::S_IFLNK => Kind::Symlink,
                _ => Kind::Other,
            },
        )),
        Err(Errno::ENOENT) => Ok(None),
        Err(error) => Err(error.into()),
    }
}

/// The entries one layer has put in place so far, by (parent directory,
/// name). OCI whiteouts apply to the layers below only, wherever in the
/// archive the marker stands; these are what a marker has to leave alone.
#[derive(Default)]
pub(super) struct Members(HashMap<Key, HashSet<Box<[u8]>>>);

impl Members {
    pub(super) fn insert(&mut self, dir: &Pinned, name: &OsStr) {
        self.0
            .entry(dir.key)
            .or_default()
            .insert(name.as_bytes().into());
    }

    fn contains(&self, dir: Key, name: &OsStr) -> bool {
        self.0
            .get(&dir)
            .is_some_and(|names| names.contains(name.as_bytes()))
    }
}

/// The extraction root of one layer application: the anchor all
/// resolution starts from, and the last parent directory it resolved.
pub(super) struct Root {
    anchor: File,
    /// Consecutive archive entries mostly share their parent. The handle
    /// stays valid until something replaces or removes a directory or a
    /// symlink, which may change what a path resolves to — those call
    /// [`Root::invalidate`].
    cached: Option<(PathBuf, Rc<Pinned>)>,
    next_temporary: u64,
}

impl Root {
    pub(super) fn open(path: &Path) -> io::Result<Root> {
        Ok(Root {
            anchor: File::open(path)?,
            cached: None,
            next_temporary: 0,
        })
    }

    pub(super) fn invalidate(&mut self) {
        self.cached = None;
    }

    fn open_dir(&self, relative: &Path) -> io::Result<OwnedFd> {
        openat2_confined(
            &self.anchor,
            relative,
            OFlag::O_PATH | OFlag::O_DIRECTORY,
            Mode::empty(),
            ResolveFlag::RESOLVE_IN_ROOT | ResolveFlag::RESOLVE_NO_MAGICLINKS,
        )
    }

    fn remember(&mut self, relative: &Path, fd: OwnedFd) -> io::Result<Rc<Pinned>> {
        let key = key(&fstat(&fd)?);
        let dir = Rc::new(Pinned { fd, key });
        self.cached = Some((relative.to_path_buf(), dir.clone()));
        Ok(dir)
    }

    fn cached(&self, relative: &Path) -> Option<Rc<Pinned>> {
        match &self.cached {
            Some((path, dir)) if path == relative => Some(dir.clone()),
            _ => None,
        }
    }

    /// The existing directory `relative` (image-rooted); None when it does
    /// not exist. Anything else in its way is an error.
    pub(super) fn dir(&mut self, relative: &Path) -> io::Result<Option<Rc<Pinned>>> {
        if let Some(dir) = self.cached(relative) {
            return Ok(Some(dir));
        }
        match self.open_dir(relative) {
            Ok(fd) => self.remember(relative, fd).map(Some),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(step(format_args!("open {}", relative.display()), error)),
        }
    }

    /// The directory `relative`, its missing ancestors created like tar
    /// and containerd create them (0755). Every prefix is re-resolved from
    /// the root, so in-image symlinks along it behave as in the container;
    /// only a missing final component is created, on the pinned handle of
    /// the level above. A non-directory in the way fails with ENOTDIR.
    pub(super) fn parent(&mut self, relative: &Path) -> io::Result<Rc<Pinned>> {
        if let Some(dir) = self.dir(relative)? {
            return Ok(dir);
        }
        let mut level = self.open_dir(Path::new(""))?;
        let mut prefix = PathBuf::new();
        for component in relative.components() {
            let name = component.as_os_str();
            prefix.push(name);
            level = match self.open_dir(&prefix) {
                Ok(dir) => dir,
                Err(error) if error.kind() == io::ErrorKind::NotFound => {
                    match mkdirat(&level, name, Mode::from_bits_truncate(0o755)) {
                        Ok(()) | Err(Errno::EEXIST) => {}
                        Err(error) => {
                            return Err(step(format_args!("create {}", prefix.display()), error))
                        }
                    }
                    self.open_dir(&prefix).map_err(|error| {
                        if error.kind() == io::ErrorKind::NotFound {
                            // The name exists (mkdirat said so) but leads
                            // nowhere: a symlink to something missing.
                            io::Error::new(
                                error.kind(),
                                format!(
                                    "{} exists but does not resolve to a directory in the image",
                                    prefix.display()
                                ),
                            )
                        } else {
                            step(format_args!("open {}", prefix.display()), error)
                        }
                    })?
                }
                Err(error) => return Err(step(format_args!("open {}", prefix.display()), error)),
            };
        }
        self.remember(relative, level)
    }

    /// Create a non-directory under a fresh temporary name below `dir`;
    /// `make` creates it at the name it is given and fails with EEXIST when
    /// that name is taken. The result is put in place with
    /// [`Root::commit`]; dropped instead, the temporary name goes away.
    pub(super) fn temporary<'d, T>(
        &mut self,
        dir: &'d Pinned,
        name: &OsStr,
        mut make: impl FnMut(&Pinned, &OsStr) -> nix::Result<T>,
    ) -> io::Result<(Temporary<'d>, T)> {
        loop {
            let candidate = OsString::from(format!("{TEMPORARY_PREFIX}{}", self.next_temporary));
            self.next_temporary += 1;
            if candidate == name {
                continue;
            }
            match make(dir, &candidate) {
                Ok(made) => {
                    let temporary = Temporary {
                        dir,
                        name: candidate,
                        pending: true,
                    };
                    return Ok((temporary, made));
                }
                Err(Errno::EEXIST) => continue,
                Err(error) => return Err(step("create", error)),
            }
        }
    }

    /// Rename a prepared entry to `name`, replacing what is there. Fresh
    /// names (the common case) take one renameat2(RENAME_NOREPLACE). An
    /// existing directory goes first, with its subtree: nothing else can be
    /// renamed over it. Anything else is replaced atomically by the rename.
    pub(super) fn commit(&mut self, mut temporary: Temporary, name: &OsStr) -> io::Result<()> {
        let dir = temporary.dir;
        match rename_noreplace(dir, &temporary.name, name) {
            Ok(()) => {
                temporary.pending = false;
                return Ok(());
            }
            Err(Errno::EEXIST) => {}
            Err(error) => return Err(step("rename into place", error)),
        }
        match kind_at(dir, name).map_err(|e| step("inspect existing entry", e))? {
            Some(Kind::Directory) => {
                remove(dir, name, None).map_err(|e| step("remove existing directory", e))?;
                self.invalidate();
            }
            Some(Kind::Symlink) => self.invalidate(),
            Some(Kind::Other) | None => {}
        }
        renameat(dir, temporary.name.as_os_str(), dir, name)
            .map_err(|e| step("rename into place", e))?;
        temporary.pending = false;
        // rename(2) does nothing when both names already are the same
        // inode — a hard link restated over itself (usr/bin/perl5.40.1 ->
        // usr/bin/perl after a perl-base upgrade) — and then the temporary
        // name is still there.
        remove_temporary(dir, &temporary.name)
    }

    /// Create the directory `name` below `dir`. An existing directory stays
    /// (it only takes the entry's metadata); a file or symlink in the way
    /// is unlinked first.
    pub(super) fn make_dir(&mut self, dir: &Pinned, name: &OsStr) -> io::Result<()> {
        let mode = Mode::from_bits_truncate(0o700);
        match mkdirat(dir, name, mode) {
            Ok(()) => return Ok(()),
            Err(Errno::EEXIST) => {}
            Err(error) => return Err(step("create", error)),
        }
        match kind_at(dir, name).map_err(|e| step("inspect existing entry", e))? {
            Some(Kind::Directory) => return Ok(()),
            Some(Kind::Symlink) => self.invalidate(),
            Some(Kind::Other) | None => {}
        }
        unlink(dir.as_fd(), name, UnlinkatFlags::NoRemoveDir)
            .map_err(|e| step("remove existing entry", e))?;
        mkdirat(dir, name, mode).map_err(|e| step("create", e))
    }
}

/// A non-directory waiting for [`Root::commit`]. Dropping it removes the
/// name; that only happens on a path that already fails the layer, so
/// there is no error left to report it to.
pub(super) struct Temporary<'d> {
    dir: &'d Pinned,
    name: OsString,
    pending: bool,
}

impl Temporary<'_> {
    pub(super) fn name(&self) -> &OsStr {
        &self.name
    }
}

impl Drop for Temporary<'_> {
    fn drop(&mut self) {
        if self.pending {
            let _ = unlinkat(self.dir, self.name.as_os_str(), UnlinkatFlags::NoRemoveDir);
        }
    }
}

/// Gone is the normal case after the rename. Anything else would leave
/// the temporary name in the image, visible in the container: that fails
/// the layer instead.
pub(super) fn remove_temporary(dir: impl AsFd, name: &OsStr) -> io::Result<()> {
    match unlinkat(dir, name, UnlinkatFlags::NoRemoveDir) {
        Ok(()) | Err(Errno::ENOENT) => Ok(()),
        Err(error) => Err(step(
            format_args!("remove temporary {}", Path::new(name).display()),
            error,
        )),
    }
}

/// renameat2(RENAME_NOREPLACE): raw, because neither libc's nor nix's
/// wrapper exists for every target this builds for (musl included).
fn rename_noreplace(dir: impl AsFd, from: &OsStr, to: &OsStr) -> nix::Result<()> {
    let fd = dir.as_fd().as_raw_fd();
    let rc = from.with_nix_path(|from| {
        to.with_nix_path(|to| unsafe {
            libc::syscall(
                libc::SYS_renameat2,
                fd,
                from.as_ptr(),
                fd,
                to.as_ptr(),
                libc::RENAME_NOREPLACE,
            )
        })
    })??;
    Errno::result(rc).map(drop)
}

/// fchmodat2(AT_SYMLINK_NOFOLLOW): changes the mode of `name` itself,
/// without opening it (a FIFO would block, a device would run its
/// driver's open) and without following it.
pub(super) fn chmod_at(dir: impl AsFd, name: &OsStr, mode: u32) -> nix::Result<()> {
    let fd = dir.as_fd().as_raw_fd();
    let rc = name.with_nix_path(|name| unsafe {
        libc::syscall(
            libc::SYS_fchmodat2,
            fd,
            name.as_ptr(),
            mode as libc::c_uint,
            libc::AT_SYMLINK_NOFOLLOW,
        )
    })?;
    Errno::result(rc).map(drop)
}

/// Set an extended attribute on an open regular file.
pub(super) fn set_xattr(file: &File, key: &OsStr, value: &[u8]) -> nix::Result<()> {
    let key = CString::new(key.as_bytes()).map_err(|_| Errno::EINVAL)?;
    // SAFETY: valid fd, NUL-terminated key, value pointer/length from a slice.
    let rc = unsafe {
        libc::fsetxattr(
            file.as_raw_fd(),
            key.as_ptr(),
            value.as_ptr().cast(),
            value.len(),
            0,
        )
    };
    Errno::result(rc).map(drop)
}

/// Set an extended attribute on `name` itself (a directory, symlink or
/// device node), not following it. lsetxattr has no *at form before Linux
/// 6.13 (setxattrat), so the pinned directory is reached through its
/// /proc/self/fd magic link — the very inode the handle pins — and only the
/// final component is looked up, without following it.
pub(super) fn set_xattr_at(
    dir: &Pinned,
    name: &OsStr,
    key: &OsStr,
    value: &[u8],
) -> nix::Result<()> {
    let mut path = format!("/proc/self/fd/{}/", dir.fd.as_raw_fd()).into_bytes();
    path.extend_from_slice(name.as_bytes());
    let path = CString::new(path).map_err(|_| Errno::EINVAL)?;
    let key = CString::new(key.as_bytes()).map_err(|_| Errno::EINVAL)?;
    // SAFETY: NUL-terminated path and key, value pointer/length from a slice.
    let rc = unsafe {
        libc::lsetxattr(
            path.as_ptr(),
            key.as_ptr(),
            value.as_ptr().cast(),
            value.len(),
            0,
        )
    };
    Errno::result(rc).map(drop)
}

/// Remove `name` below `dir`, whatever it is; a directory goes with its
/// whole subtree, a symlink is unlinked, never followed. A missing entry
/// is not an error. With `keep`, what this layer itself put in place
/// stays, and so does every directory on the way to such an entry.
pub(super) fn remove(dir: &Pinned, name: &OsStr, keep: Option<&Members>) -> io::Result<()> {
    remove_at(dir.as_fd(), dir.key, name, keep, 0)
}

/// Empty `dir` without removing it; `keep` as for [`remove`].
pub(super) fn clear(dir: &Pinned, keep: Option<&Members>) -> io::Result<()> {
    // A fresh open of "." gives the stream its own fd and offset; getdents
    // cannot read through an O_PATH handle.
    let mut stream = Stream::openat(
        dir,
        ".",
        OFlag::O_RDONLY | OFlag::O_DIRECTORY | OFlag::O_CLOEXEC,
        Mode::empty(),
    )?;
    clear_stream(&mut stream, dir.key, keep, 0)
}

fn remove_at(
    parent: BorrowedFd,
    parent_key: Key,
    name: &OsStr,
    keep: Option<&Members>,
    depth: usize,
) -> io::Result<()> {
    let member = keep.is_some_and(|members| members.contains(parent_key, name));
    // O_NOFOLLOW: a directory swapped for a symlink is unlinked, not entered.
    match Stream::openat(
        parent,
        name,
        OFlag::O_RDONLY | OFlag::O_DIRECTORY | OFlag::O_NOFOLLOW | OFlag::O_CLOEXEC,
        Mode::empty(),
    ) {
        Ok(mut dir) => {
            let dir_key = match keep {
                Some(_) => key(&fstat(&dir)?),
                None => (0, 0),
            };
            clear_stream(&mut dir, dir_key, keep, depth + 1)?;
            drop(dir);
            if member {
                return Ok(());
            }
            match unlinkat(parent, name, UnlinkatFlags::RemoveDir) {
                Ok(()) | Err(Errno::ENOENT) => Ok(()),
                // What is left is this layer's own: the directory stays.
                Err(Errno::ENOTEMPTY) if keep.is_some() => Ok(()),
                Err(e) => Err(e.into()),
            }
        }
        Err(Errno::ENOENT) => Ok(()),
        Err(Errno::ENOTDIR | Errno::ELOOP) if member => Ok(()),
        Err(Errno::ENOTDIR | Errno::ELOOP) => unlink(parent, name, UnlinkatFlags::NoRemoveDir),
        Err(e) => Err(e.into()),
    }
}

/// Remove every entry of an open directory stream; one fd per level.
fn clear_stream(
    dir: &mut Stream,
    dir_key: Key,
    keep: Option<&Members>,
    depth: usize,
) -> io::Result<()> {
    if depth >= MAX_DEPTH {
        return Err(invalid("directory tree too deep to delete"));
    }
    let fd = dir.as_raw_fd();
    for entry in dir.iter() {
        let entry = entry?;
        let name = entry.file_name().to_bytes();
        if name == b"." || name == b".." {
            continue;
        }
        // SAFETY: fd belongs to `dir`, which outlives this borrow; the
        // stream's offset is not touched by *at calls on the same fd.
        let parent = unsafe { BorrowedFd::borrow_raw(fd) };
        remove_at(parent, dir_key, OsStr::from_bytes(name), keep, depth)?;
    }
    Ok(())
}

fn unlink(parent: BorrowedFd, name: &OsStr, flags: UnlinkatFlags) -> io::Result<()> {
    match unlinkat(parent, name, flags) {
        Ok(()) | Err(Errno::ENOENT) => Ok(()),
        Err(e) => Err(e.into()),
    }
}
