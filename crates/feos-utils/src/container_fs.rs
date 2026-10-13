// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! openat2-confined access to a container's root filesystem, for the tool
//! and watch services.
//!
//! Every file op is anchored at a dirfd of the container's rootfs and
//! resolved with RESOLVE_BENEATH, so neither lexical "..", nor symlinks
//! swapped in by the payload mid-walk, nor /proc magic links can escape
//! onto the guest filesystem. Absolute symlinks (and any ".." that would
//! climb above the anchor) are *refused* with EXDEV, not re-rooted: a
//! container's `/etc/foo -> /usr/share/foo` cannot be followed here.
//! Callers that need the container's own view of its root (image
//! extraction) pass RESOLVE_IN_ROOT to [`openat2_confined`] instead.
//!
//! One exception: the managed /etc/resolv.conf (feos_utils::resolver) is an
//! absolute link into /run/feos, a mount that exists only in the
//! container's mount namespace. Whether a path is that link is decided
//! before the open, by reading the link (never by an open that failed):
//! then a read is served from the share file it points to, and a write is
//! refused with a message that says why, as the container's own write
//! would fail on the read-only mount.

use std::ffi::CString;
use std::fs::File;
use std::os::fd::AsRawFd;
use std::path::{Component, Path, PathBuf};

use nix::fcntl::{OFlag, ResolveFlag};
use nix::libc;
use nix::sys::stat::Mode;

use crate::filesystem::CONTAINERS_DIR;
use crate::fs::openat2_confined;
use crate::resolver;

/// What a write to the managed /etc/resolv.conf gets; inside the container
/// the same write fails with EROFS.
pub const READ_ONLY: &str = "/etc/resolv.conf is managed by FeOS and read-only: \
     it links to /run/feos/resolv.conf, the resolvers the host pushed";

fn container_dir(name: &str) -> PathBuf {
    Path::new(CONTAINERS_DIR).join(name)
}

/// The container rootfs as seen from the guest; must exist (created).
pub fn rootfs_path(container: &str) -> Result<PathBuf, String> {
    let name = if container.is_empty() {
        "main"
    } else {
        container
    };
    let ok = name.len() <= 64
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'));
    if !ok {
        return Err(format!("bad container name {name:?}"));
    }
    let p = container_dir(name).join("bundle/rootfs");
    if !p.is_dir() {
        return Err(format!("container {name} has no rootfs (not created?)"));
    }
    Ok(p)
}

/// Anchor handle on the container rootfs (host-side path, not
/// payload-controlled). All confined accesses resolve beneath this dirfd.
pub fn open_root(container: &str) -> Result<File, String> {
    File::open(rootfs_path(container)?).map_err(|e| format!("open rootfs: {e}"))
}

/// Normalize a *container-view* path into a rootfs-relative form that stays
/// inside the root even lexically: leading "/" is dropped (paths are
/// container-rooted), "." components collapse, ".." pops a component, and
/// a ".." that would escape the root is an error.
pub fn to_rel(path: &str) -> Result<String, String> {
    let path = if path.is_empty() { "/" } else { path };
    let mut parts: Vec<&str> = Vec::new();
    for c in Path::new(path).components() {
        match c {
            Component::RootDir | Component::CurDir => {}
            Component::Normal(s) => parts.push(
                s.to_str()
                    .ok_or_else(|| "non-utf8 path component".to_string())?,
            ),
            Component::ParentDir => {
                if parts.pop().is_none() {
                    return Err(format!("path {path:?} escapes the container root"));
                }
            }
            Component::Prefix(_) => return Err("unsupported path prefix".to_string()),
        }
    }
    Ok(parts.join("/")) // "" == the root itself
}

/// ToolService flavour: RESOLVE_BENEATH, String errors.
fn openat2(dir: &File, rel: &str, flags: i32, mode: u32) -> Result<File, String> {
    let rel = if rel.is_empty() { "." } else { rel };
    if rel.as_bytes().contains(&0) {
        return Err("NUL byte in path".to_string());
    }
    openat2_confined(
        dir,
        Path::new(rel),
        OFlag::from_bits_retain(flags),
        Mode::from_bits_truncate(mode),
        ResolveFlag::RESOLVE_BENEATH,
    )
    .map(File::from)
    .map_err(|e| format!("{rel}: {e}"))
}

/// Open (and optionally create) `path` inside the given rootfs anchor.
pub fn open_file(root: &File, path: &str, flags: i32, mode: u32) -> Result<File, String> {
    open_file_in(
        root,
        path,
        flags,
        mode,
        &Path::new(resolver::SHARE_DIR).join("resolv.conf"),
    )
}

/// [`open_file`] with the share file of the managed /etc/resolv.conf as a
/// parameter (tests relocate it).
fn open_file_in(
    root: &File,
    path: &str,
    flags: i32,
    mode: u32,
    share_file: &Path,
) -> Result<File, String> {
    let rel = to_rel(path)?;
    if managed_resolver(root, &rel) {
        return open_managed_resolver(share_file, flags);
    }
    openat2(root, &rel, flags, mode)
}

/// Is `rel` the managed /etc/resolv.conf of this rootfs? Its parent is
/// entered beneath the root like any other path, and the entry itself is
/// only read, never followed. A workload's own file there is not the
/// managed link and takes the ordinary path.
fn managed_resolver(root: &File, rel: &str) -> bool {
    rel == "etc/resolv.conf"
        && openat2(root, "etc", libc::O_PATH | libc::O_DIRECTORY, 0)
            .is_ok_and(resolver::is_managed_link)
}

/// The container's view of the managed link: an open that only reads
/// follows it to the share file; one that could write is refused, as the
/// container's own write fails on its read-only /run/feos.
fn open_managed_resolver(share_file: &Path, flags: i32) -> Result<File, String> {
    // Not O_ACCMODE: musl folds O_PATH into it.
    let writes = flags & (libc::O_WRONLY | libc::O_RDWR | libc::O_CREAT | libc::O_TRUNC) != 0;
    if writes {
        return Err(READ_ONLY.to_string());
    }
    nix::fcntl::open(
        share_file,
        OFlag::from_bits_retain(flags) | OFlag::O_CLOEXEC,
        Mode::empty(),
    )
    .map(File::from)
    .map_err(|e| format!("etc/resolv.conf -> {}: {e}", resolver::CONTAINER_RESOLVER))
}

/// mkdir -p of path's *parent* inside the rootfs, component by component:
/// every level is entered via openat2, so a symlink swapped in mid-walk
/// still cannot escape. Then create/open the final file node.
pub fn create_file(root: &File, path: &str) -> Result<File, String> {
    let rel = to_rel(path)?;
    if rel.is_empty() {
        return Err("write target is the container root".to_string());
    }
    if managed_resolver(root, &rel) {
        return Err(READ_ONLY.to_string());
    }
    let mut cur: File = root.try_clone().map_err(|e| format!("clone rootfd: {e}"))?;
    let mut comps: Vec<&str> = rel.split('/').collect();
    let last = comps.pop().unwrap_or_default();
    for comp in comps {
        cur = match openat2(&cur, comp, libc::O_PATH | libc::O_DIRECTORY, 0) {
            Ok(f) => f,
            Err(_) => {
                // missing component (or non-dir) — try to create it
                let c = CString::new(comp).map_err(|_| "NUL byte in path".to_string())?;
                // SAFETY: plain mkdirat on a valid dirfd.
                let rc = unsafe { libc::mkdirat(cur.as_raw_fd(), c.as_ptr(), 0o755) };
                let err = std::io::Error::last_os_error();
                if rc != 0 && err.raw_os_error() != Some(libc::EEXIST) {
                    return Err(format!("mkdir {comp}: {err}"));
                }
                openat2(&cur, comp, libc::O_PATH | libc::O_DIRECTORY, 0)
                    .map_err(|e| format!("mkdir {comp}: {e}"))?
            }
        };
    }
    openat2(
        &cur,
        last,
        libc::O_WRONLY | libc::O_CREAT | libc::O_TRUNC,
        0o644,
    )
}

/// A pinned, escaped-proof handle path for walking: /proc/self/fd/<fd>
/// keeps referring to the exact directory open beneath the rootfs even if
/// path components above it are renamed.
pub fn fd_walk_path(f: &File) -> String {
    format!("/proc/self/fd/{}", f.as_raw_fd())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Linux only (openat2): the managed link is recognized before any
    /// open — a read gets the share file, whatever spelling leads there; a
    /// write gets the reason, not EXDEV; a lookup as a directory fails as
    /// it would in the container. A workload's own file there, and any
    /// other absolute link, take the ordinary path.
    #[test]
    fn managed_resolver_is_decided_before_the_open() {
        use crate::resolver::CONTAINER_RESOLVER as PAYLOAD_RESOLVER;
        use std::io::Read;
        use std::os::unix::fs::symlink;
        let base =
            std::env::temp_dir().join(format!("feos-toolfs-resolver-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(base.join("root/etc")).unwrap();
        std::fs::create_dir_all(base.join("share")).unwrap();
        let share = base.join("share/resolv.conf");
        std::fs::write(&share, "nameserver 192.0.2.53\n").unwrap();
        symlink(PAYLOAD_RESOLVER, base.join("root/etc/resolv.conf")).unwrap();
        symlink("/etc/hostname", base.join("root/etc/hosts")).unwrap();
        let root = File::open(base.join("root")).unwrap();
        let open = |path: &str, flags: i32| open_file_in(&root, path, flags, 0, &share);
        let read = |path: &str| {
            let mut got = String::new();
            open(path, libc::O_RDONLY)
                .unwrap()
                .read_to_string(&mut got)
                .unwrap();
            got
        };

        assert_eq!(read("/etc/resolv.conf"), "nameserver 192.0.2.53\n");
        assert_eq!(read("etc/../etc/./resolv.conf"), "nameserver 192.0.2.53\n");
        for flags in [
            libc::O_WRONLY | libc::O_TRUNC,
            libc::O_RDWR,
            libc::O_RDONLY | libc::O_TRUNC,
        ] {
            assert_eq!(open("/etc/resolv.conf", flags).unwrap_err(), READ_ONLY);
        }
        assert_eq!(
            create_file(&root, "/etc/resolv.conf").unwrap_err(),
            READ_ONLY
        );
        let e = open("/etc/resolv.conf", libc::O_PATH | libc::O_DIRECTORY).unwrap_err();
        assert!(e.contains("Not a directory"), "{e}");
        assert_eq!(
            std::fs::read_to_string(&share).unwrap(),
            "nameserver 192.0.2.53\n",
            "share file untouched"
        );
        let e = open("/etc/hosts", libc::O_RDONLY).unwrap_err();
        assert!(e.contains("os error 18"), "{e}"); // EXDEV

        // The workload replaced the link with a file of its own: that file
        // is what the container reads, and the tools read and write it.
        std::fs::remove_file(base.join("root/etc/resolv.conf")).unwrap();
        std::fs::write(
            base.join("root/etc/resolv.conf"),
            "nameserver 203.0.113.9\n",
        )
        .unwrap();
        assert_eq!(read("/etc/resolv.conf"), "nameserver 203.0.113.9\n");
        use std::io::Write;
        create_file(&root, "/etc/resolv.conf")
            .unwrap()
            .write_all(b"nameserver 203.0.113.10\n")
            .unwrap();
        assert_eq!(read("/etc/resolv.conf"), "nameserver 203.0.113.10\n");
        assert_eq!(
            std::fs::read_to_string(&share).unwrap(),
            "nameserver 192.0.2.53\n"
        );
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn paths_are_container_rooted() {
        assert_eq!(to_rel("/workspace/x.rs").unwrap(), "workspace/x.rs");
        assert_eq!(to_rel("workspace/x.rs").unwrap(), "workspace/x.rs");
        assert_eq!(to_rel("/").unwrap(), "");
        assert_eq!(to_rel("").unwrap(), "");
        assert_eq!(to_rel(".").unwrap(), "");
        assert_eq!(to_rel("/a/./b/../c").unwrap(), "a/c");
        assert_eq!(to_rel("/a/../b").unwrap(), "b");
    }

    #[test]
    fn escapes_are_rejected() {
        assert!(to_rel("../../etc/passwd").is_err());
        assert!(to_rel("/..").is_err());
        assert!(to_rel("/a/../../b").is_err()); // pops past the root
    }
}
