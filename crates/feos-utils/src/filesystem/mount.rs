// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use log::{debug, error};
use nix::mount::{mount, MsFlags};

/// Mounts the virtual filesystems PID 1 needs. A failed mount is logged, not
/// fatal: PID 1 must not die, or the kernel panics.
pub fn mount_virtual_filesystems() {
    const NONE: Option<&'static [u8]> = None;

    debug!("Mounting /proc");
    mount(
        Some(b"proc".as_ref()),
        "/proc",
        Some(b"proc".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| error!("/proc mount failed: {e}"));

    debug!("Mounting /sys");
    mount(
        Some(b"sys".as_ref()),
        "/sys",
        Some(b"sysfs".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| error!("/sys mount failed: {e}"));

    debug!("Mounting /dev");
    mount(
        Some(b"devtmpfs".as_ref()),
        "/dev",
        Some(b"devtmpfs".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| error!("/dev mount failed: {e}"));

    debug!("Mounting /dev/pts");
    // devtmpfs comes up without the directory.
    let _ = std::fs::create_dir_all("/dev/pts");
    mount(
        Some(b"devpts".as_ref()),
        "/dev/pts",
        Some(b"devpts".as_ref()),
        MsFlags::empty(),
        Some(b"ptmxmode=0666".as_ref()),
    )
    .unwrap_or_else(|e| error!("/dev/pts mount failed: {e}"));

    debug!("Mounting /run");
    mount(
        Some(b"tmpfs".as_ref()),
        "/run",
        Some(b"tmpfs".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| error!("/run mount failed: {e}"));

    debug!("Mounting /var/lib/feos");
    mount(
        Some(b"tmpfs".as_ref()),
        "/var/lib/feos",
        Some(b"tmpfs".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| error!("/var/lib/feos mount failed: {e}"));

    debug!("Mounting /sys/fs/cgroup");
    mount(
        Some(b"cgroup2".as_ref()),
        "/sys/fs/cgroup",
        Some(b"cgroup2".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| error!("/sys/fs/cgroup mount failed: {e}"));
}
