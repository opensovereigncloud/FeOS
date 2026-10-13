// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The data volume of a guest: the virtio disk that does not hold the root
//! filesystem. In a microVM booted from an initramfs it is the only disk.
//! Images and container state live on it, so they survive pause and resume.

use log::{info, warn};
use nix::mount::{mount, MsFlags};
use std::path::Path;
use std::time::Duration;

/// virtio-blk probes asynchronously: the disk can appear after PID 1 starts.
const PROBE_ATTEMPTS: u32 = 80;
const PROBE_INTERVAL: Duration = Duration::from_millis(25);

/// Waits up to two seconds for the data volume and mounts it (ext4) at
/// `target`. Returns the device it mounted, or None if there is no data
/// volume or the mount failed; both are logged.
pub fn mount_data_volume(target: &str) -> Option<String> {
    let device = (0..PROBE_ATTEMPTS).find_map(|attempt| {
        if attempt > 0 {
            std::thread::sleep(PROBE_INTERVAL);
        }
        find_data_device()
    });
    let Some(device) = device else {
        warn!("No data volume found; {target} stays in RAM");
        return None;
    };

    if let Err(e) = std::fs::create_dir_all(target) {
        warn!("Failed to create {target}: {e}");
    }
    match mount(
        Some(device.as_str()),
        target,
        Some("ext4"),
        MsFlags::empty(),
        None::<&str>,
    ) {
        Ok(()) => {
            info!("Mounted data volume {device} at {target}");
            Some(device)
        }
        Err(e) => {
            warn!("Failed to mount data volume {device} at {target}: {e}");
            None
        }
    }
}

/// The first virtio disk (vda, vdb, vdc) that does not back the root mount.
fn find_data_device() -> Option<String> {
    let root_disk = root_major_minor().and_then(|majmin| {
        std::fs::read_link(format!("/sys/dev/block/{majmin}"))
            .ok()
            .and_then(|target| target.file_name().map(|n| n.to_string_lossy().into_owned()))
    });
    ["vda", "vdb", "vdc"]
        .into_iter()
        .filter(|disk| root_disk.as_deref() != Some(disk))
        .map(|disk| format!("/dev/{disk}"))
        .find(|path| Path::new(path).exists())
}

/// major:minor of the device mounted at /, from /proc/self/mountinfo.
fn root_major_minor() -> Option<String> {
    let mountinfo = std::fs::read_to_string("/proc/self/mountinfo").ok()?;
    root_major_minor_from(&mountinfo)
}

fn root_major_minor_from(mountinfo: &str) -> Option<String> {
    mountinfo.lines().find_map(|line| {
        let fields: Vec<&str> = line.split(' ').collect();
        (fields.len() > 4 && fields[4] == "/").then(|| fields[2].to_string())
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finds_the_root_mount() {
        let mountinfo = "\
22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
23 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs devtmpfs rw";
        assert_eq!(root_major_minor_from(mountinfo).as_deref(), Some("252:1"));
    }

    #[test]
    fn no_root_mount_in_an_unrelated_table() {
        let mountinfo = "23 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs devtmpfs rw";
        assert_eq!(root_major_minor_from(mountinfo), None);
    }
}
