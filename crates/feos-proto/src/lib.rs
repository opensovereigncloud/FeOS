// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

pub mod vm_service {
    tonic::include_proto!("feos.vm.v1");
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
