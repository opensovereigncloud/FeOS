// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Freezing a filesystem so that the host can copy its block device
//! consistently: FIFREEZE syncs it and blocks new writes until FITHAW.
//! ioctl(2) directly, not fsfreeze(8).

use crate::error::HostError;
use feos_proto::host_service::{
    FreezeFilesystemRequest, FreezeFilesystemResponse, ThawFilesystemRequest,
    ThawFilesystemResponse,
};
use feos_utils::filesystem::DATA_ROOT;
use log::{error, info};
use std::fs::File;
use std::os::fd::AsRawFd;
use tokio::sync::oneshot;

// _IOWR('X', 119, int) and _IOWR('X', 120, int) in linux/fs.h.
nix::ioctl_readwrite!(fifreeze, b'X', 119, libc::c_int);
nix::ioctl_readwrite!(fithaw, b'X', 120, libc::c_int);

pub async fn handle_freeze(
    req: FreezeFilesystemRequest,
    responder: oneshot::Sender<Result<FreezeFilesystemResponse, HostError>>,
) {
    let result = tokio::task::spawn_blocking(move || {
        let mountpoint = mountpoint_or_data_root(req.mountpoint);
        let device = freeze(&mountpoint, Op::Freeze)?;
        info!("HostWorker: Froze {mountpoint} ({device})");
        Ok(FreezeFilesystemResponse { device })
    })
    .await
    .unwrap_or_else(|e| Err(HostError::Failed(format!("freeze task: {e}"))));
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for FreezeFilesystem.");
    }
}

pub async fn handle_thaw(
    req: ThawFilesystemRequest,
    responder: oneshot::Sender<Result<ThawFilesystemResponse, HostError>>,
) {
    let result = tokio::task::spawn_blocking(move || {
        let mountpoint = mountpoint_or_data_root(req.mountpoint);
        let device = freeze(&mountpoint, Op::Thaw)?;
        info!("HostWorker: Thawed {mountpoint} ({device})");
        Ok(ThawFilesystemResponse { device })
    })
    .await
    .unwrap_or_else(|e| Err(HostError::Failed(format!("thaw task: {e}"))));
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for ThawFilesystem.");
    }
}

#[derive(Clone, Copy)]
enum Op {
    Freeze,
    Thaw,
}

fn mountpoint_or_data_root(mountpoint: String) -> String {
    if mountpoint.is_empty() {
        DATA_ROOT.to_string()
    } else {
        mountpoint
    }
}

/// Freezes or thaws the filesystem mounted at `mountpoint` and returns the
/// name of its block device.
fn freeze(mountpoint: &str, op: Op) -> Result<String, HostError> {
    let mountinfo = std::fs::read_to_string("/proc/self/mountinfo")
        .map_err(|e| HostError::Failed(format!("/proc/self/mountinfo: {e}")))?;
    let device = device_of(&mountinfo, mountpoint)
        .ok_or_else(|| HostError::NotFound(format!("no filesystem mounted at {mountpoint}")))?;
    let dir =
        File::open(mountpoint).map_err(|e| HostError::Failed(format!("open {mountpoint}: {e}")))?;
    let mut arg: libc::c_int = 0;
    // SAFETY: the fd is open for the whole call and arg is a valid int.
    let result = unsafe {
        match op {
            Op::Freeze => fifreeze(dir.as_raw_fd(), &mut arg),
            Op::Thaw => fithaw(dir.as_raw_fd(), &mut arg),
        }
    };
    result.map_err(|e| HostError::Failed(format!("{mountpoint}: {e}")))?;
    Ok(device)
}

/// The block device name (e.g. "vda") behind the mount at `mountpoint`, read
/// through /sys/dev/block/<major:minor>. Mounts stack, and mountinfo lists
/// the one on top last: that is the one a path resolves to.
fn device_of(mountinfo: &str, mountpoint: &str) -> Option<String> {
    let majmin = mountinfo.lines().rev().find_map(|line| {
        let fields: Vec<&str> = line.split(' ').collect();
        (fields.len() > 4 && fields[4] == mountpoint).then(|| fields[2].to_string())
    })?;
    let target = std::fs::read_link(format!("/sys/dev/block/{majmin}")).unwrap_or_default();
    Some(
        target
            .file_name()
            .map(|name| name.to_string_lossy().into_owned())
            .unwrap_or(majmin),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_top_mount_wins() {
        let mountinfo = "\
30 22 0:25 / /var/lib/feos rw,relatime - tmpfs tmpfs rw
31 30 0:0 / /var/lib/feos rw,relatime - ext4 /dev/vda rw";
        // 0:0 has no /sys/dev/block entry, so the major:minor comes back.
        assert_eq!(
            device_of(mountinfo, "/var/lib/feos").as_deref(),
            Some("0:0")
        );
        assert_eq!(device_of(mountinfo, "/nowhere"), None);
    }
}
