// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use feos_proto::vm_service::{VmEvent, VmState, VmStateChangedEvent};
use log::{error, info, warn};
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use prost::Message;
use std::path::Path;
use tokio_stream::StreamExt;
use vm_service::vm_dir;

pub struct VmGuard {
    pub vm_id: String,
    pub pid: Option<Pid>,
    pub cleanup_disabled: bool,
}

impl VmGuard {
    pub fn new(vm_id: String) -> Self {
        Self {
            vm_id,
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

impl Drop for VmGuard {
    fn drop(&mut self) {
        if self.cleanup_disabled {
            return;
        }
        info!("Cleaning up VM '{}'...", self.vm_id);
        if let Some(pid) = self.pid {
            info!("Killing process with PID: {pid}");
            let _ = kill(pid, Signal::SIGKILL);
        }
        let dir = vm_dir(&self.vm_id);
        if let Err(e) = std::fs::remove_dir_all(&dir) {
            if e.kind() != std::io::ErrorKind::NotFound {
                warn!("Could not remove VM directory '{}': {e}", dir.display());
            }
        } else {
            info!("Removed VM directory '{}'", dir.display());
        }
    }
}

pub async fn wait_for_target_state(
    stream: &mut tonic::Streaming<VmEvent>,
    target_state: VmState,
) -> Result<()> {
    while let Some(event_res) = stream.next().await {
        let event = event_res?;
        let any_data = event.data.expect("Event should have data payload");
        if any_data.type_url == "type.googleapis.com/feos.vm.v1.VmStateChangedEvent" {
            let state_change = VmStateChangedEvent::decode(&*any_data.value)?;
            let new_state =
                VmState::try_from(state_change.new_state).unwrap_or(VmState::Unspecified);

            info!(
                "Received VM state change event: new_state={:?}, reason='{}'",
                new_state, state_change.reason
            );

            if new_state == target_state {
                return Ok(());
            }

            if new_state == VmState::Crashed {
                let err_msg = format!("VM entered Crashed state. Reason: {}", state_change.reason);
                error!("{err_msg}");
                return Err(anyhow::anyhow!(err_msg));
            }
        }
    }
    Err(anyhow::anyhow!(
        "Event stream ended before VM reached {target_state:?} state."
    ))
}

pub fn verify_vm_socket_cleanup(vm_id: &str) {
    let dir = vm_dir(vm_id);
    assert!(
        !Path::new(&dir).exists(),
        "VM directory '{}' should not exist after DeleteVm",
        dir.display()
    );
    info!("Verified the VM directory is deleted: {}", dir.display());
}
