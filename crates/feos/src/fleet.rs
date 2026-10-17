// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd and the control plane (ADR 0015): what the node reports of its
//! pods, diskd and installed versions, and its pods' events.

use feos_proto::pod_service::PodInfo;
use fleet_client::{Diskd, Fleet, Incoming, Installed, Inventory};
use log::warn;
use microvm_service::vmm::installed;
use pod_service::Pods;
use tokio::sync::broadcast::error::RecvError;

struct Node {
    pods: Pods,
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

    async fn joined_mesh(&self, pod_network: String, pod_prefix: String) -> Result<(), String> {
        self.pods
            .network()
            .join_mesh(&pod_network, &pod_prefix)
            .await
    }
}

/// Starts the node's side of the control plane, with the pods' events.
pub(crate) fn start(config: fleet_client::Config, pods: Pods) -> Result<(Fleet, Incoming), String> {
    let mut events = pods.subscribe();
    let (fleet, incoming) = fleet_client::start(config, Node { pods })?;
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
