use nix::mount::{mount, MsFlags};

pub fn mount_virtual_filesystems() {
    const NONE: Option<&'static [u8]> = None;

    mount(
        Some(b"proc".as_ref()),
        "/proc",
        Some(b"proc".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| panic!("/proc mount failed: {e}"));

    mount(
        Some(b"sys".as_ref()),
        "/sys",
        Some(b"sysfs".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| panic!("/sys mount failed: {e}"));

    mount(
        Some(b"devtmpfs".as_ref()),
        "/dev",
        Some(b"devtmpfs".as_ref()),
        MsFlags::empty(),
        NONE,
    )
    .unwrap_or_else(|e| panic!("/dev mount failed: {e}"));
}
