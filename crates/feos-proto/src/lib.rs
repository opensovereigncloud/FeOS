// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

/// Internal: between the pod service and the microVM layer, in feosd.
pub mod microvm {
    tonic::include_proto!("feos.microvm.v1");
}
pub mod pod_service {
    tonic::include_proto!("feos.pod.v1");
}
pub mod host_service {
    tonic::include_proto!("feos.host.v1");
}
pub mod image_service {
    tonic::include_proto!("feos.image.v1");
}
pub mod task_service {
    tonic::include_proto!("feos.task.v1");
}
pub mod container_service {
    tonic::include_proto!("feos.container.v1");
}
pub mod lifecycle_service {
    tonic::include_proto!("feos.lifecycle.v1");
}
pub mod tool_service {
    tonic::include_proto!("feos.tool.v1");
}
pub mod watch_service {
    tonic::include_proto!("feos.watch.v1");
}
pub mod portforward_service {
    tonic::include_proto!("feos.portforward.v1");
}
pub mod kube_service {
    tonic::include_proto!("feos.kube.v1");
}
pub mod ssh_service {
    tonic::include_proto!("feos.ssh.v1");
}
/// diskd's API, on the node.
pub mod disk_service {
    tonic::include_proto!("feos.disk.v1");
}
/// The pods' security groups and traffic records, on the node.
pub mod network_service {
    tonic::include_proto!("feos.network.v1");
}
/// What only a guest does: feosd's calls into feos-guest.
pub mod guest_service {
    tonic::include_proto!("feos.guest.v1");
}
/// The node's persistent volumes.
pub mod volume_service {
    tonic::include_proto!("feos.volume.v1");
}
