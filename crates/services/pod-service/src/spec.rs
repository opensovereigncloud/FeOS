// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's spec: what is valid, and the microVM it becomes.

use crate::network::{self, Attachment};
use feos_proto::microvm::{
    BalloonConfig, CpuConfig, DataDiskConfig, GuestNetworkConfig, MemoryConfig, NetConfig,
    TapConfig, VmConfig,
};
use feos_proto::pod_service::{PodSpec, Resources};
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

    Ok(spec)
}

/// Whether the pod has a network: unless its spec says none.
pub fn has_network(spec: &PodSpec) -> bool {
    !spec.network.as_ref().is_some_and(|network| network.none)
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
/// guest uses, the pod's disk if it has one started, and the pod's TAP
/// device and addresses if it has a network (ADR 0010): an IPv6 /128
/// behind the node at `fe80::1`, an IPv4 /32 behind `169.254.0.1`, which
/// is on the link although outside the /32, and the node's resolver.
pub fn vm_config(
    pod_id: &str,
    spec: &PodSpec,
    attachment: Option<&Attachment>,
    disk: Option<&str>,
) -> VmConfig {
    let resources = spec.resources.unwrap_or_default();
    let mut config = VmConfig {
        cpus: Some(CpuConfig {
            vcpu_count: resources.vcpus,
        }),
        memory: Some(MemoryConfig {
            size_mib: resources.memory_mib,
            balloon: Some(BalloonConfig {
                amount_mib: 0,
                deflate_on_oom: true,
                free_page_reporting: true,
                stats_interval_s: 0,
            }),
        }),
        data_disk: disk.map(|path| DataDiskConfig {
            path: path.to_string(),
        }),
        ..Default::default()
    };
    if let Some(attachment) = attachment {
        config.net.push(NetConfig {
            device_id: INTERFACE.to_string(),
            tap: Some(TapConfig {
                tap_name: attachment.tap.clone(),
            }),
            mac_address: network::mac_string(attachment.mac),
        });
        config.guest_network = Some(GuestNetworkConfig {
            interface: INTERFACE.to_string(),
            ipv4_cidr: format!("{}/32", attachment.ipv4),
            gateway: network::GATEWAY4.to_string(),
            ipv6_cidr: format!("{}/128", attachment.ipv6),
            ipv6_gateway: network::GATEWAY.to_string(),
            dns: vec![attachment.dns.to_string()],
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
    }

    #[test]
    fn a_pod_has_a_network_unless_it_says_none() {
        let mut spec = spec(vec![]);
        assert!(has_network(&spec));
        spec.network = Some(PodNetwork {
            none: false,
            ..Default::default()
        });
        assert!(has_network(&spec));
        spec.network = Some(PodNetwork {
            none: true,
            ..Default::default()
        });
        assert!(!has_network(&spec));
    }

    #[test]
    fn a_pod_may_come_without_containers() {
        assert!(normalize(spec(vec![])).is_ok());
    }

    #[test]
    fn pod_ids_are_uuids() {
        assert!(check_id("11111111-2222-3333-4444-555555555555").is_ok());
        assert!(check_id("../etc").is_err());
        assert!(check_id("").is_err());
    }

    #[test]
    fn the_network_becomes_eth0() {
        let spec = normalize(spec(vec![container("main")])).unwrap();
        let id = "11111111-2222-3333-4444-555555555555";
        let attachment = Attachment {
            tap: network::tap_name(id),
            mac: network::mac(id),
            ipv6: "fd00:fe05:1::9".parse().unwrap(),
            ipv4: "169.254.16.9".parse().unwrap(),
            dns: "169.254.0.53".parse().unwrap(),
        };
        let config = vm_config(id, &spec, Some(&attachment), None);
        assert_eq!(config.net[0].tap.as_ref().unwrap().tap_name, "pod-11111111");
        assert_eq!(config.net[0].mac_address, "06:00:11:11:11:11");
        let guest = config.guest_network.unwrap();
        assert_eq!(guest.interface, "eth0");
        assert_eq!(guest.ipv6_cidr, "fd00:fe05:1::9/128");
        assert_eq!(guest.ipv6_gateway, "fe80::1");
        assert_eq!(
            (guest.ipv4_cidr.as_str(), guest.gateway.as_str()),
            ("169.254.16.9/32", "169.254.0.1")
        );
        assert_eq!(guest.dns, ["169.254.0.53"]);
        assert_eq!(guest.hostname, id);
        assert_eq!(config.memory.unwrap().size_mib, 512);

        let config = vm_config(id, &spec, None, None);
        assert!(config.net.is_empty() && config.guest_network.is_none());
    }

    #[test]
    fn the_disk_is_the_data_disk() {
        let spec = normalize(spec(vec![])).unwrap();
        let id = "11111111-2222-3333-4444-555555555555";
        let link = format!("/var/lib/feos/disk/live/{id}/disk");
        let config = vm_config(id, &spec, None, Some(&link));
        assert_eq!(config.data_disk.unwrap().path, link);
        assert!(vm_config(id, &spec, None, None).data_disk.is_none());
    }
}
