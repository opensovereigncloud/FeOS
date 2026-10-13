// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Confined path resolution with openat2(2).

use nix::errno::Errno;
use nix::fcntl::{OFlag, OpenHow, ResolveFlag};
use nix::sys::stat::Mode;
use std::os::fd::{AsFd, OwnedFd};
use std::path::Path;

/// How often [`openat2_confined`] retries a spurious EAGAIN; runc and
/// libpathrs use the same bound.
const OPENAT2_RETRIES: usize = 32;

/// openat2(2) below `dir` with the given resolve flags, e.g.
/// RESOLVE_IN_ROOT to resolve a path the way a container rooted at `dir`
/// would, or RESOLVE_BENEATH to refuse anything that leaves `dir`.
/// O_CLOEXEC is always added; an empty path opens `dir` itself.
pub fn openat2_confined(
    dir: impl AsFd,
    path: &Path,
    flags: OFlag,
    mode: Mode,
    resolve: ResolveFlag,
) -> std::io::Result<OwnedFd> {
    let path = if path.as_os_str().is_empty() {
        Path::new(".")
    } else {
        path
    };
    let how = OpenHow::new()
        .flags(flags | OFlag::O_CLOEXEC)
        .mode(mode)
        .resolve(resolve);
    // The kernel fails RESOLVE_BENEATH and RESOLVE_IN_ROOT walks over ".."
    // with a spurious EAGAIN when a rename or mount races anywhere on the
    // system; retry a bounded number of times, then let it through.
    let mut left = OPENAT2_RETRIES;
    loop {
        match nix::fcntl::openat2(dir.as_fd(), path, how) {
            Err(Errno::EAGAIN) if left > 1 => left -= 1,
            result => return Ok(result?),
        }
    }
}
