// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use feos_proto::vm_service::{
    GuestAttachedEvent, GuestInfo, VmEvent, VmState, VmStateChangedEvent,
};
use log::{error, info, warn};
use nix::sys::signal::{kill, Signal};
use nix::sys::stat::{makedev, mknod, Mode, SFlag};
use nix::unistd::Pid;
use prost::Message;
use std::fs::File;
use std::os::fd::AsRawFd;
use std::path::{Path, PathBuf};
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

/// The next state change on the stream, whatever it is.
pub async fn next_state_change(
    stream: &mut tonic::Streaming<VmEvent>,
) -> Result<VmStateChangedEvent> {
    while let Some(event) = stream.next().await {
        if let Some(data) = event?.data
            && data.type_url == "type.googleapis.com/feos.vm.v1.VmStateChangedEvent"
        {
            return Ok(VmStateChangedEvent::decode(&*data.value)?);
        }
    }
    Err(anyhow::anyhow!("Event stream ended"))
}

/// Waits until a FeOS guest attaches to its uplink; returns what it reported.
pub async fn wait_for_guest_attached(stream: &mut tonic::Streaming<VmEvent>) -> Result<GuestInfo> {
    while let Some(event) = stream.next().await {
        let Some(data) = event?.data else {
            continue;
        };
        match data.type_url.as_str() {
            "type.googleapis.com/feos.vm.v1.GuestAttachedEvent" => {
                let guest = GuestAttachedEvent::decode(&*data.value)?
                    .guest
                    .unwrap_or_default();
                info!(
                    "The guest attached: boot {}, attach {}",
                    guest.boot_id, guest.attach_seq
                );
                return Ok(guest);
            }
            "type.googleapis.com/feos.vm.v1.VmStateChangedEvent" => {
                let state_change = VmStateChangedEvent::decode(&*data.value)?;
                if state_change.new_state == VmState::Crashed as i32 {
                    anyhow::bail!("VM entered Crashed state: {}", state_change.reason);
                }
            }
            _ => {}
        }
    }
    Err(anyhow::anyhow!(
        "Event stream ended before the guest attached."
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

/// A loop device over a file: a disk that is a block device. It goes when
/// the last process that opened it closes it.
pub struct LoopDevice {
    pub path: PathBuf,
    _device: File,
}

#[repr(C)]
struct LoopInfo64 {
    device: u64,
    inode: u64,
    rdevice: u64,
    offset: u64,
    size_limit: u64,
    number: u32,
    encrypt_type: u32,
    encrypt_key_size: u32,
    flags: u32,
    file_name: [u8; 64],
    crypt_name: [u8; 64],
    encrypt_key: [u8; 32],
    init: [u64; 2],
}

#[repr(C)]
struct LoopConfig {
    fd: u32,
    block_size: u32,
    info: LoopInfo64,
    reserved: [u64; 8],
}

const LOOP_MAJOR: u64 = 7;
const LO_FLAGS_AUTOCLEAR: u32 = 4;
nix::ioctl_none_bad!(loop_ctl_get_free, 0x4C82);
nix::ioctl_write_ptr_bad!(loop_configure, 0x4C0A, LoopConfig);

impl LoopDevice {
    pub fn new(backing: &Path) -> Result<Self> {
        let control = File::options()
            .read(true)
            .write(true)
            .open("/dev/loop-control")?;
        // SAFETY: the fd is the loop control device; the call takes no argument.
        let number = unsafe { loop_ctl_get_free(control.as_raw_fd()) }?;
        let path = PathBuf::from(format!("/dev/loop{number}"));
        // A container's /dev only has the devices there were when it started.
        match mknod(
            &path,
            SFlag::S_IFBLK,
            Mode::from_bits_truncate(0o660),
            makedev(LOOP_MAJOR, number as u64),
        ) {
            Ok(()) | Err(nix::Error::EEXIST) => {}
            Err(e) => return Err(e.into()),
        }
        let backing = File::options().read(true).write(true).open(backing)?;
        let device = File::options().read(true).write(true).open(&path)?;
        // SAFETY: all zeroes is a valid loop_config.
        let mut config: LoopConfig = unsafe { std::mem::zeroed() };
        config.fd = backing.as_raw_fd() as u32;
        config.info.flags = LO_FLAGS_AUTOCLEAR;
        // SAFETY: the fd is the loop device and config outlives the call.
        unsafe { loop_configure(device.as_raw_fd(), &config) }?;
        info!("{} is a loop device now", path.display());
        Ok(Self {
            path,
            _device: device,
        })
    }
}
