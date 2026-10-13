// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::VmEventWrapper;
use feos_proto::vm_service::{
    CreateVmRequest, DeleteVmRequest, DeleteVmResponse, GetBalloonResponse, GuestNetworkConfig,
    PauseVmRequest, PauseVmResponse, PingVmRequest, PingVmResponse, ResumeVmRequest,
    ResumeVmResponse, ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, VmConfig, VmEvent,
    VmStateChangedEvent,
};
use prost::Message;
use prost_types::Any;
use std::net::{IpAddr, Ipv4Addr};
use std::path::Path;
use tokio::io::DuplexStream;
use tokio::sync::{broadcast, mpsc};
use tonic::Status;
use uuid::Uuid;

mod fc_api;
pub mod firecracker;
pub mod snapshot;

#[derive(Debug, thiserror::Error)]
pub enum VmmError {
    #[error("Hypervisor process failed to start: {0}")]
    ProcessSpawnFailed(String),

    #[error("The provided configuration is invalid for this hypervisor: {0}")]
    InvalidConfig(String),

    #[error("Could not connect to the hypervisor's API socket: {0}")]
    ApiConnectionFailed(String),

    #[error("The hypervisor's API returned an error: {0}")]
    ApiOperationFailed(String),

    #[error("The requested VM (id: {0}) could not be found")]
    VmNotFound(String),

    #[error("The image service returned an error: {0}")]
    ImageServiceFailed(String),

    #[error("The snapshot cannot be restored: {0}")]
    SnapshotUnusable(String),

    #[error("An internal or unexpected error occurred: {0}")]
    Internal(String),
}

impl From<VmmError> for Status {
    fn from(err: VmmError) -> Self {
        match err {
            VmmError::VmNotFound(id) => Status::not_found(id),
            VmmError::InvalidConfig(msg) => Status::invalid_argument(msg),
            VmmError::SnapshotUnusable(msg) => Status::failed_precondition(msg),
            VmmError::ApiConnectionFailed(msg) | VmmError::ImageServiceFailed(msg) => {
                Status::unavailable(msg)
            }
            VmmError::ProcessSpawnFailed(msg)
            | VmmError::ApiOperationFailed(msg)
            | VmmError::Internal(msg) => Status::internal(msg),
        }
    }
}

#[tonic::async_trait]
pub trait Hypervisor: Send + Sync {
    /// Prepares a VM from its request and its pulled image. Returns the pid
    /// of a VMM process if creating the VM started one.
    async fn create_vm(
        &self,
        vm_id: &str,
        req: CreateVmRequest,
        image_uuid: String,
    ) -> Result<Option<i64>, VmmError>;

    /// Boots the VM, discarding a snapshot it has; returns the pid of its
    /// VMM process.
    async fn start_vm(&self, req: StartVmRequest) -> Result<i64, VmmError>;

    /// Writes a snapshot of the running or paused VM and ends its VMM
    /// process; first waits until free page reporting is done, if asked.
    /// Returns what the snapshot's memory takes on disk. A VM that cannot
    /// be suspended is left running or paused, as it was.
    async fn suspend_vm(&self, vm_id: &str, settle_free_pages: bool) -> Result<u64, VmmError>;

    /// Restores a suspended VM in a new VMM process and resumes it; returns
    /// the process's pid. The snapshot is gone once the guest ran; an error
    /// before that keeps it.
    async fn restore_vm(&self, vm_id: &str) -> Result<i64, VmmError>;

    /// Watches a running VM until its VMM process ends, and reports how it
    /// ended, unless the VM's ID arrives on the cancel bus first.
    async fn healthcheck_vm(
        &self,
        vm_id: String,
        broadcast_tx: mpsc::Sender<VmEventWrapper>,
        cancel_bus: broadcast::Receiver<Uuid>,
    );

    /// Whether `pid` is the VMM process of the VM, and alive.
    fn is_vmm_process(&self, vm_id: &str, pid: i64) -> bool;

    async fn delete_vm(
        &self,
        req: DeleteVmRequest,
        process_id: Option<i64>,
    ) -> Result<DeleteVmResponse, VmmError>;

    /// The VM's serial console: reading gives its output from now on,
    /// writing is its input.
    async fn open_console(&self, vm_id: &str) -> Result<DuplexStream, VmmError>;

    async fn ping_vm(&self, req: PingVmRequest) -> Result<PingVmResponse, VmmError>;
    async fn shutdown_vm(&self, req: ShutdownVmRequest) -> Result<ShutdownVmResponse, VmmError>;
    async fn pause_vm(&self, req: PauseVmRequest) -> Result<PauseVmResponse, VmmError>;
    async fn resume_vm(&self, req: ResumeVmRequest) -> Result<ResumeVmResponse, VmmError>;
    /// Sets the balloon's target size.
    async fn update_balloon(&self, vm_id: &str, amount_mib: u32) -> Result<(), VmmError>;
    async fn get_balloon(&self, vm_id: &str) -> Result<GetBalloonResponse, VmmError>;
}

/// Checks what a VMM cannot check before it boots the VM.
pub fn validate_config(config: &VmConfig) -> Result<(), VmmError> {
    if config.disks.iter().any(|disk| disk.path.is_empty()) {
        return Err(VmmError::InvalidConfig("a disk needs a path".to_string()));
    }
    if config
        .net
        .iter()
        .any(|nic| nic.tap.as_ref().is_none_or(|tap| tap.tap_name.is_empty()))
    {
        return Err(VmmError::InvalidConfig(
            "a network interface needs a TAP device".to_string(),
        ));
    }
    if let Some(network) = &config.guest_network {
        validate_guest_network(network)?;
    }
    Ok(())
}

fn validate_guest_network(network: &GuestNetworkConfig) -> Result<(), VmmError> {
    let invalid = |what: &str| Err(VmmError::InvalidConfig(format!("guest network: {what}")));
    if network.interface.is_empty() {
        return invalid("no interface");
    }
    let prefix_ok = network
        .ipv4_cidr
        .split_once('/')
        .is_some_and(|(address, prefix)| {
            address.parse::<Ipv4Addr>().is_ok() && prefix.parse::<u8>().is_ok_and(|p| p <= 32)
        });
    if !prefix_ok {
        return invalid(&format!("bad address/prefix {:?}", network.ipv4_cidr));
    }
    if network.gateway.parse::<Ipv4Addr>().is_err() {
        return invalid(&format!("bad gateway {:?}", network.gateway));
    }
    if let Some(dns) = network
        .dns
        .iter()
        .find(|dns| dns.parse::<IpAddr>().is_err())
    {
        return invalid(&format!("bad resolver {dns:?}"));
    }
    Ok(())
}

pub async fn broadcast_state_change_event(
    broadcast_tx: &mpsc::Sender<VmEventWrapper>,
    vm_id: &str,
    component: &str,
    data: VmStateChangedEvent,
    process_id: Option<i64>,
) {
    let event = VmEvent {
        vm_id: vm_id.to_string(),
        id: Uuid::new_v4().to_string(),
        component_id: component.to_string(),
        data: Some(Any {
            type_url: "type.googleapis.com/feos.vm.v1.VmStateChangedEvent".to_string(),
            value: data.encode_to_vec(),
        }),
    };

    if broadcast_tx
        .send(VmEventWrapper { event, process_id })
        .await
        .is_err()
    {
        log::warn!("Failed to broadcast event for VM '{vm_id}': channel closed.");
    }
}

pub enum VmmType {
    Firecracker,
}

pub fn factory(vmm_type: VmmType) -> Box<dyn Hypervisor> {
    match vmm_type {
        VmmType::Firecracker => {
            let binary = Path::new(super::FIRECRACKER_BIN).to_path_buf();
            Box::new(firecracker::FirecrackerAdapter::new(binary))
        }
    }
}
