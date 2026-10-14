// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's spec: what is valid, and the microVM it becomes.

use feos_proto::pod_service::{PodSpec, Resources};
use feos_proto::vm_service::{
    BalloonConfig, CpuConfig, GuestNetworkConfig, MemoryConfig, NetConfig, TapConfig, VmConfig,
};
use std::collections::HashSet;
use tonic::Status;
use uuid::Uuid;

const DEFAULT_VCPUS: u32 = 1;
const DEFAULT_MEMORY_MIB: u64 = 512;
/// Firecracker's limit.
const MAX_VCPUS: u32 = 32;
/// Less does not hold the guest and an image.
const MIN_MEMORY_MIB: u64 = 128;
/// The pod's interface in the guest.
const INTERFACE: &str = "eth0";

/// Checks a pod's ID: a UUID, which also names its microVM and its paths.
pub fn check_id(pod_id: &str) -> Result<(), Status> {
    Uuid::parse_str(pod_id)
        .map(|_| ())
        .map_err(|_| Status::invalid_argument(format!("pod ID {pod_id:?} is not a UUID")))
}

/// Checks a spec and fills in the defaults, so that two creates of a pod
/// compare as they will run.
pub fn normalize(mut spec: PodSpec) -> Result<PodSpec, Status> {
    let resources = spec.resources.get_or_insert_with(Resources::default);
    if resources.vcpus == 0 {
        resources.vcpus = DEFAULT_VCPUS;
    }
    if resources.memory_mib == 0 {
        resources.memory_mib = DEFAULT_MEMORY_MIB;
    }
    if resources.vcpus > MAX_VCPUS {
        return Err(Status::invalid_argument(format!(
            "a pod has at most {MAX_VCPUS} vCPUs"
        )));
    }
    if resources.memory_mib < MIN_MEMORY_MIB {
        return Err(Status::invalid_argument(format!(
            "a pod has at least {MIN_MEMORY_MIB} MiB of memory"
        )));
    }

    if spec.containers.is_empty() {
        return Err(Status::invalid_argument("a pod has a container at least"));
    }
    let mut names = HashSet::new();
    for container in &spec.containers {
        if !valid_name(&container.name) {
            return Err(Status::invalid_argument(format!(
                "invalid container name {:?}: 1 to 64 letters, digits, \"-\", \"_\" and \".\"",
                container.name
            )));
        }
        if !names.insert(container.name.as_str()) {
            return Err(Status::invalid_argument(format!(
                "two containers are named {:?}",
                container.name
            )));
        }
        if container.image_ref.is_empty() {
            return Err(Status::invalid_argument(format!(
                "container {:?} has no image",
                container.name
            )));
        }
    }

    if let Some(network) = &spec.network {
        if network.tap_name.is_empty() {
            return Err(Status::invalid_argument(
                "the pod's network needs a TAP device",
            ));
        }
        if !network.ipv4_cidr.is_empty() && !network.ipv4_cidr.contains('/') {
            return Err(Status::invalid_argument(format!(
                "{:?} has no prefix length",
                network.ipv4_cidr
            )));
        }
    }
    Ok(spec)
}

/// The container names the guest takes as container IDs.
fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name != "."
        && name != ".."
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// The pod's microVM: the newest guest installed on the node, with a
/// balloon that reports free pages, so that a suspend writes what the
/// guest uses.
pub fn vm_config(pod_id: &str, spec: &PodSpec) -> VmConfig {
    let resources = spec.resources.unwrap_or_default();
    let mut config = VmConfig {
        cpus: Some(CpuConfig {
            vcpu_count: resources.vcpus,
        }),
        memory: Some(MemoryConfig {
            size_mib: resources.memory_mib,
            hugepages: false,
            balloon: Some(BalloonConfig {
                amount_mib: 0,
                deflate_on_oom: true,
                free_page_reporting: true,
                stats_interval_s: 0,
            }),
        }),
        image_ref: String::new(),
        ..Default::default()
    };
    if let Some(network) = &spec.network {
        config.net.push(NetConfig {
            device_id: INTERFACE.to_string(),
            tap: Some(TapConfig {
                tap_name: network.tap_name.clone(),
            }),
            mac_address: network.mac_address.clone(),
        });
        config.guest_network = Some(GuestNetworkConfig {
            interface: INTERFACE.to_string(),
            ipv4_cidr: network.ipv4_cidr.clone(),
            gateway: network.gateway.clone(),
            dns: network.dns.clone(),
            hostname: pod_id.to_string(),
        });
    }
    config
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::pod_service::{ContainerSpec, PodNetwork};

    fn container(name: &str) -> ContainerSpec {
        ContainerSpec {
            name: name.to_string(),
            image_ref: "docker.io/library/busybox:1.36".to_string(),
            ..Default::default()
        }
    }

    fn spec(containers: Vec<ContainerSpec>) -> PodSpec {
        PodSpec {
            containers,
            ..Default::default()
        }
    }

    #[test]
    fn defaults_are_filled_in() {
        let spec = normalize(spec(vec![container("main")])).unwrap();
        assert_eq!(
            spec.resources,
            Some(Resources {
                vcpus: 1,
                memory_mib: 512
            })
        );
        assert_eq!(normalize(spec.clone()).unwrap(), spec);
    }

    #[test]
    fn bad_specs_are_refused() {
        assert!(normalize(spec(vec![])).is_err());
        assert!(normalize(spec(vec![container("main"), container("main")])).is_err());
        assert!(normalize(spec(vec![container("a/b")])).is_err());
        assert!(normalize(spec(vec![container("..")])).is_err());
        let mut no_image = container("main");
        no_image.image_ref.clear();
        assert!(normalize(spec(vec![no_image])).is_err());
        let mut small = spec(vec![container("main")]);
        small.resources = Some(Resources {
            vcpus: 1,
            memory_mib: 64,
        });
        assert!(normalize(small).is_err());
        let mut no_tap = spec(vec![container("main")]);
        no_tap.network = Some(PodNetwork::default());
        assert!(normalize(no_tap).is_err());
    }

    #[test]
    fn pod_ids_are_uuids() {
        assert!(check_id("11111111-2222-3333-4444-555555555555").is_ok());
        assert!(check_id("../etc").is_err());
        assert!(check_id("").is_err());
    }

    #[test]
    fn the_network_becomes_eth0() {
        let mut spec = normalize(spec(vec![container("main")])).unwrap();
        spec.network = Some(PodNetwork {
            tap_name: "pod0".to_string(),
            ipv4_cidr: "10.201.0.2/24".to_string(),
            gateway: "10.201.0.1".to_string(),
            dns: vec!["1.1.1.1".to_string()],
            ..Default::default()
        });
        let config = vm_config("11111111-2222-3333-4444-555555555555", &spec);
        assert_eq!(config.image_ref, "");
        assert_eq!(config.net[0].tap.as_ref().unwrap().tap_name, "pod0");
        let guest = config.guest_network.unwrap();
        assert_eq!(guest.interface, "eth0");
        assert_eq!(guest.hostname, "11111111-2222-3333-4444-555555555555");
        assert_eq!(config.memory.unwrap().size_mib, 512);
    }
}
