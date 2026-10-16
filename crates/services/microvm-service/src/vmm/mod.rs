// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::VmEventWrapper;
use feos_proto::microvm::{
    CreateVmRequest, DeleteVmRequest, DeleteVmResponse, GuestNetworkConfig, PingVmRequest,
    PingVmResponse, ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, VmConfig, VmEvent,
    VmStateChangedEvent,
};
use prost::Message;
use prost_types::Any;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::path::{Path, PathBuf};
use tokio::io::DuplexStream;
use tokio::sync::{broadcast, mpsc};
use tonic::Status;
use uuid::Uuid;

pub mod fc_api;
pub mod firecracker;
pub mod installed;
pub mod jail;
pub mod snapshot;

/// Where Firecracker serves a microVM's metadata to its guest (ADR 0010).
pub const METADATA_ADDRESS: Ipv4Addr = Ipv4Addr::new(169, 254, 169, 254);
/// Firecracker's limit of its metadata store, in bytes, by default: its
/// API's largest request.
pub const METADATA_LIMIT: usize = 51200;

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
    /// Prepares a VM from its request and its image's directory: a pulled
    /// image, or an installed guest. Returns the pid of a VMM process if
    /// creating the VM started one.
    async fn create_vm(
        &self,
        vm_id: &str,
        req: CreateVmRequest,
        image_dir: PathBuf,
    ) -> Result<Option<i64>, VmmError>;

    /// Boots the VM, discarding a snapshot it has; returns the pid of its
    /// VMM process.
    async fn start_vm(&self, req: StartVmRequest) -> Result<i64, VmmError>;

    /// Writes a snapshot of the running or paused VM and ends its VMM
    /// process; first waits until free page reporting is done, if asked.
    /// Returns what the snapshot's memory takes on disk. A VM that cannot
    /// be suspended is left running or paused, as it was.
    async fn suspend_vm(&self, vm_id: &str, settle_free_pages: bool) -> Result<u64, VmmError>;

    /// Restores a suspended VM in a new VMM process and resumes it, or with
    /// `remote` a created one from a snapshot written elsewhere, whose
    /// memory a userfaultfd handler serves; returns the process's pid. The
    /// snapshot is gone once the guest ran; an error before that keeps it.
    async fn restore_vm(
        &self,
        vm_id: &str,
        remote: Option<snapshot::Remote>,
    ) -> Result<i64, VmmError>;

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
}

/// Checks what a VMM cannot check before it boots the VM.
pub fn validate_config(config: &VmConfig) -> Result<(), VmmError> {
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
    validate_metadata(config)
}

/// The metadata of a microVM: a JSON object that fits Firecracker's store,
/// served on a network interface of the VM's.
fn validate_metadata(config: &VmConfig) -> Result<(), VmmError> {
    if config.metadata_json.is_empty() {
        return Ok(());
    }
    let invalid = |what: &str| Err(VmmError::InvalidConfig(format!("metadata: {what}")));
    if config.net.is_empty() {
        return invalid("a microVM without a network interface has no metadata service");
    }
    match serde_json::from_str::<serde_json::Value>(&config.metadata_json) {
        // Firecracker measures what it keeps as it serializes it.
        Ok(object @ serde_json::Value::Object(_)) => match object.to_string().len() {
            size if size > METADATA_LIMIT => invalid(&format!(
                "{size} bytes, more than the {METADATA_LIMIT} Firecracker keeps"
            )),
            _ => Ok(()),
        },
        Ok(_) => invalid("not a JSON object"),
        Err(e) => invalid(&e.to_string()),
    }
}

fn validate_guest_network(network: &GuestNetworkConfig) -> Result<(), VmmError> {
    let invalid = |what: &str| Err(VmmError::InvalidConfig(format!("guest network: {what}")));
    if network.interface.is_empty() {
        return invalid("no interface");
    }
    // A family is pushed when either of its fields is set; one at least.
    let ipv4 = !network.ipv4_cidr.is_empty() || !network.gateway.is_empty();
    let ipv6 = !network.ipv6_cidr.is_empty() || !network.ipv6_gateway.is_empty();
    if !ipv4 && !ipv6 {
        return invalid("neither an IPv6 nor an IPv4 address");
    }
    if ipv4 {
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
    }
    // An address beyond the link: the guest's kernel makes the link-local
    // one itself.
    if ipv6 {
        let prefix_ok = network
            .ipv6_cidr
            .split_once('/')
            .is_some_and(|(address, prefix)| {
                address.parse::<Ipv6Addr>().is_ok_and(|a| {
                    !(a.is_unspecified()
                        || a.is_loopback()
                        || a.is_multicast()
                        || a.is_unicast_link_local())
                }) && prefix.parse::<u8>().is_ok_and(|p| (1..=128).contains(&p))
            });
        if !prefix_ok {
            return invalid(&format!("bad IPv6 address/prefix {:?}", network.ipv6_cidr));
        }
        let gateway_ok = network
            .ipv6_gateway
            .parse::<Ipv6Addr>()
            .is_ok_and(|g| !(g.is_unspecified() || g.is_loopback() || g.is_multicast()));
        if !gateway_ok {
            return invalid(&format!("bad IPv6 gateway {:?}", network.ipv6_gateway));
        }
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
            type_url: "type.googleapis.com/feos.microvm.v1.VmStateChangedEvent".to_string(),
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
        VmmType::Firecracker => Box::new(firecracker::FirecrackerAdapter::new(
            Path::new(super::FIRECRACKER_BIN).to_path_buf(),
            Path::new(super::JAILER_BIN).to_path_buf(),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::microvm::{NetConfig, TapConfig};

    fn network(ipv4: (&str, &str), ipv6: (&str, &str)) -> GuestNetworkConfig {
        GuestNetworkConfig {
            interface: "eth0".to_string(),
            ipv4_cidr: ipv4.0.to_string(),
            gateway: ipv4.1.to_string(),
            dns: vec!["169.254.0.53".to_string()],
            ipv6_cidr: ipv6.0.to_string(),
            ipv6_gateway: ipv6.1.to_string(),
            ..Default::default()
        }
    }

    #[test]
    fn a_guest_network_has_one_family_at_least() {
        let ipv4 = ("169.254.16.2/32", "169.254.0.1");
        let ipv6 = ("fd00:fe05:ffff::2/128", "fe80::1");
        for ok in [
            network(ipv4, ("", "")),
            network(("", ""), ipv6),
            network(ipv4, ipv6),
        ] {
            assert!(validate_guest_network(&ok).is_ok(), "{ok:?}");
        }
        for bad in [
            network(("", ""), ("", "")),
            network(("10.200.0.2/33", "10.200.0.1"), ("", "")),
            network(("10.200.0.2/16", ""), ipv6),
            network(("", "10.200.0.1"), ipv6),
            network(ipv4, ("fd00::2/129", "fe80::1")),
            network(ipv4, ("fd00::2/0", "fe80::1")),
            network(ipv4, ("fd00::2", "fe80::1")),
            network(ipv4, ("10.200.0.2/16", "fe80::1")),
            network(ipv4, ("fe80::2/64", "fe80::1")),
            network(ipv4, ("::1/128", "fe80::1")),
            network(ipv4, ("ff02::1/128", "fe80::1")),
            network(("", ""), ("fd00::2/128", "")),
            network(("", ""), ("fd00::2/128", "10.200.0.1")),
            network(("", ""), ("fd00::2/128", "ff02::2")),
            network(("", ""), ("", "fe80::1")),
        ] {
            assert!(validate_guest_network(&bad).is_err(), "{bad:?}");
        }
        let mut bad_resolver = network(("", ""), ipv6);
        bad_resolver.dns.push("fdfd::53%eth0".to_string());
        assert!(validate_guest_network(&bad_resolver).is_err());
    }

    #[test]
    fn metadata_is_a_json_object_on_a_network_interface() {
        let nic = NetConfig {
            device_id: "eth0".to_string(),
            tap: Some(TapConfig {
                tap_name: "pod-00000000".to_string(),
            }),
            mac_address: String::new(),
        };
        let config = |net: Vec<NetConfig>, metadata: &str| VmConfig {
            net,
            metadata_json: metadata.to_string(),
            ..Default::default()
        };
        for ok in [
            config(vec![], ""),
            config(vec![nic.clone()], ""),
            config(vec![nic.clone()], r#"{"feos": {"pod-id": "x"}}"#),
        ] {
            assert!(validate_config(&ok).is_ok(), "{ok:?}");
        }
        let large = format!(r#"{{"a": "{}"}}"#, "x".repeat(METADATA_LIMIT));
        for bad in [
            config(vec![], "{}"),
            config(vec![nic.clone()], "[]"),
            config(vec![nic.clone()], r#""text""#),
            config(vec![nic.clone()], "{"),
            config(vec![nic], &large),
        ] {
            assert!(validate_config(&bad).is_err(), "{bad:?}");
        }
    }
}
