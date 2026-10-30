// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd and the control plane (ADR 0015): what the node reports of its
//! pods, diskd and installed versions, and its pods' events; and its pods'
//! logs, which go to the sink the control plane names (ADR 0026).

use feos_proto::container_service::StreamContainerLogsRequest;
use feos_proto::pod_service::PodInfo;
use fleet_client::{
    Diskd, Fleet, Incoming, Installed, Inventory, LogStream, Logs, ShippedPod, Shipper,
};
use log::warn;
use microvm_service::vmm::installed;
use pod_service::Pods;
use std::sync::Arc;
use tokio::runtime::Handle;
use tokio::sync::broadcast::error::RecvError;

/// The node, as the control plane's threads ask it.
struct Node {
    pods: Pods,
    /// feosd's runtime, which what the control plane's answers set up
    /// runs on: the pods' network serves the pods from there.
    runtime: Handle,
}

impl Inventory for Node {
    fn pods(&self) -> Vec<PodInfo> {
        self.pods.list()
    }

    fn installed(&self) -> Installed {
        Installed {
            firecracker: installed::firecracker_versions(),
            guest: installed::guest_versions(),
        }
    }

    async fn diskd(&self) -> Option<Diskd> {
        let stats = self.pods.disks().node_stats().await.ok()?;
        Some(Diskd {
            object_store: stats.object_store,
            object_store_id: stats.object_store_id,
            version: stats.version,
        })
    }

    fn pod_prefix(&self) -> Option<String> {
        let (prefix, len) = self.pods.network().pod_prefix()?;
        Some(format!("{prefix}/{len}"))
    }

    fn mesh_key(&self) -> Option<([u8; 32], u16)> {
        self.pods.network().mesh_key()
    }

    /// Taken on feosd's runtime, to the end, even when the control plane's
    /// side stops waiting for it.
    async fn joined_mesh(&self, pod_network: String, pod_prefix: String) -> Result<(), String> {
        let pods = self.pods.clone();
        self.runtime
            .spawn(async move { pods.network().join_mesh(&pod_network, &pod_prefix).await })
            .await
            .map_err(|e| format!("taking the mesh's answer: {e}"))?
    }
}

/// The pods' logs, as the shipper reads them.
struct PodLogs(Pods);

impl Logs for PodLogs {
    fn pods(&self) -> Vec<ShippedPod> {
        self.0
            .shipping_logs()
            .into_iter()
            .map(|pod| ShippedPod {
                containers: pod
                    .spec
                    .map(|spec| spec.containers.into_iter().map(|c| c.name).collect())
                    .unwrap_or_default(),
                id: pod.pod_id,
                labels: pod.log_labels,
                redact: pod.log_redact,
            })
            .collect()
    }

    async fn logs(
        &self,
        pod_id: &str,
        request: StreamContainerLogsRequest,
    ) -> Result<LogStream, tonic::Status> {
        Ok(Box::pin(self.0.container_logs(pod_id, request).await?))
    }
}

/// Ships the pods' logs to the sink the control plane names, and drains
/// a pod's before it pauses, hibernates or is deleted.
fn ship_logs(fleet: &Fleet, pods: &Pods) {
    let shipper = Shipper::start(PodLogs(pods.clone()), fleet.sink());
    let drain = shipper.clone();
    pods.set_log_drain(Arc::new(move |pod_id| {
        let shipper = drain.clone();
        Box::pin(async move { shipper.drain(&pod_id).await })
    }));
    let mut events = pods.subscribe();
    tokio::spawn(async move {
        loop {
            match events.recv().await {
                Ok(event) => {
                    if event.deleted {
                        shipper.deleted(&event.pod_id);
                    }
                    shipper.changed();
                }
                Err(RecvError::Lagged(_)) => shipper.changed(),
                Err(RecvError::Closed) => return,
            }
        }
    });
}

/// Starts the node's side of the control plane, with the pods' events. It
/// runs on threads of its own; what it asks of the node runs on this
/// runtime. `calls` are those of the node connections' server.
pub(crate) fn start(
    config: fleet_client::Config,
    pods: Pods,
    calls: fleet_client::NodeCalls,
) -> Result<(Fleet, Incoming), String> {
    let mut events = pods.subscribe();
    let node = Node {
        pods: pods.clone(),
        runtime: Handle::current(),
    };
    let (fleet, incoming) = fleet_client::start(config, node, calls)?;
    ship_logs(&fleet, &pods);
    let sink = fleet.events().clone();
    tokio::spawn(async move {
        loop {
            match events.recv().await {
                Ok(event) => sink.pod(event),
                Err(RecvError::Lagged(missed)) => {
                    warn!("Fleet: {missed} pod events were missed");
                    sink.lost(missed);
                }
                Err(RecvError::Closed) => return,
            }
        }
    });
    Ok((fleet, incoming))
}
