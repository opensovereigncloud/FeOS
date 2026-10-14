// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use log::{info, warn};
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use std::path::Path;
use vm_service::vm_dir;

/// Kills a pod's Firecracker and removes its directory if a test ends
/// before it deleted the pod.
pub struct PodGuard {
    pub pod_id: String,
    pub pid: Option<Pid>,
    pub cleanup_disabled: bool,
}

impl PodGuard {
    pub fn new(pod_id: String) -> Self {
        Self {
            pod_id,
            pid: None,
            cleanup_disabled: false,
        }
    }

    pub fn disable_cleanup(&mut self) {
        self.cleanup_disabled = true;
    }

    pub fn set_pid(&mut self, pid: i32) {
        self.pid = Some(Pid::from_raw(pid));
    }
}

impl Drop for PodGuard {
    fn drop(&mut self) {
        if self.cleanup_disabled {
            return;
        }
        info!("Cleaning up pod '{}'...", self.pod_id);
        if let Some(pid) = self.pid {
            info!("Killing process with PID: {pid}");
            let _ = kill(pid, Signal::SIGKILL);
        }
        let dir = vm_dir(&self.pod_id);
        if let Err(e) = std::fs::remove_dir_all(&dir) {
            if e.kind() != std::io::ErrorKind::NotFound {
                warn!(
                    "Could not remove the pod's directory '{}': {e}",
                    dir.display()
                );
            }
        } else {
            info!("Removed the pod's directory '{}'", dir.display());
        }
    }
}

pub fn verify_microvm_cleanup(pod_id: &str) {
    let dir = vm_dir(pod_id);
    assert!(
        !Path::new(&dir).exists(),
        "The pod's directory '{}' should not exist after DeletePod",
        dir.display()
    );
    info!("Verified the pod's directory is deleted: {}", dir.display());
}
