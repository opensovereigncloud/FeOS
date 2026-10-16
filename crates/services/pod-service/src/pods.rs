// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods, on top of the VM service (ADR 0004).
//!
//! A pod comes up in steps: its disk is started, its microVM is created
//! and booted, its guest attaches, and its containers are created and
//! started in the guest, one after the other. Each step looks at what is
//! there first, so that a bring-up that was cut off, by a restart of feosd
//! or a stop, is taken up again where it ended. A boot on a disk that has
//! the pod's containers adopts them. What changes a pod runs under its
//! lock; a stop or a delete ends a bring-up first.
//!
//! A pod hibernates (ADR 0008) under its lock, in this order: it is
//! suspended, its disk is stopped, sealed and uploaded, its memory sealed
//! and uploaded, its vmstate and its spec put beside them, and its head
//! last; then it is a record with its checkpoint, and what it held on the
//! node goes. Until the head is up the pod stays suspended on the node. A
//! restore opens the checkpoint's disk and restores the memory where the
//! node can load the snapshot; elsewhere, it boots anew on the disk.
//!
//! A pod's volumes (ADR 0016) follow its disk: started for a boot, a resume
//! and a restore, after the disk, stopped with it, sealed and uploaded
//! with it, each with a head of its own before the pod's, which names
//! them. A pod claims them at its creation and lets go of them at its
//! deletion. After a boot, the guest mounts them before the containers
//! are created, and formats those the node knows to be new.

use crate::disk::{volume_disk, Disks};
use crate::network::{Attachment, Network};
use crate::spec;
use crate::store::Store;
use crate::volumes::{self, Volumes};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, ContainerConfig,
    ContainerState as GuestContainerState, CreateContainerRequest, GetContainerRequest,
    ListContainersRequest, StartContainerRequest, VolumeMount as GuestVolumeMount,
};
use feos_proto::disk_service::{Head, MemoryRef};
use feos_proto::guest_service::{
    guest_service_client::GuestServiceClient, MountVolumesRequest, Volume as GuestVolume,
};
use feos_proto::microvm::{
    micro_vm_service_server::MicroVmService, stream_vm_console_request as console_request,
    AttachConsoleMessage, ConsoleData, CreateVmRequest, DeleteVmRequest, GetVmRequest,
    PingVmRequest, RemoteSnapshot, ResumeVmRequest, ShutdownVmRequest, StartVmRequest,
    StreamVmConsoleRequest, StreamVmEventsRequest, SuspendVmRequest, VmConfig, VmEvent, VmInfo,
    VmState, VmStateChangedEvent,
};
use feos_proto::pod_service::{
    Checkpoint, ContainerSpec, ContainerState, ContainerStatus, HibernatePodResponse, MicroVm,
    PodConsoleInput, PodConsoleOutput, PodEvent, PodInfo, PodSpec, PodState, RestorePodResponse,
};
use feos_proto::volume_service::{Volume, VolumeCheckpoint, VolumeState};
use feos_utils::vsock::{self, API_PORT};
use log::{info, warn};
use microvm_service::api::VmApiHandler;
use microvm_service::vmm::snapshot;
use microvm_service::{memory_socket_path, vsock_path};
use prost::Message;
use std::collections::HashMap;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, SystemTime};
use tokio::sync::{broadcast, OwnedMutexGuard};
use tokio::task::AbortHandle;
use tokio::time::{sleep, timeout, Instant};
use tokio_stream::{Stream, StreamExt};
use tonic::transport::Channel;
use tonic::{Code, Request, Status};

/// How long a booting guest may take to attach.
const GUEST_TIMEOUT: Duration = Duration::from_secs(60);
/// How often a step looks again while it waits.
const POLL: Duration = Duration::from_millis(250);
/// How long the VM service may take to record a change it made.
const SETTLE_TIMEOUT: Duration = Duration::from_secs(10);
/// How long GetPod waits for the guest's view of the containers.
const STATUS_TIMEOUT: Duration = Duration::from_secs(3);
/// How long the guest may take to mount the pod's volumes, formatting new
/// ones: mke2fs writes some MiB for every TiB.
const MOUNT_TIMEOUT: Duration = Duration::from_secs(300);
/// How often the rules are written anew without a change of the pods or
/// their groups: the node's own addresses, which no pod reaches, may have
/// changed.
const REFRESH: Duration = Duration::from_secs(60);

#[derive(Clone)]
pub struct Pods {
    inner: Arc<Inner>,
}

struct Inner {
    vms: VmApiHandler,
    store: Store,
    /// Where the pods are kept; a hibernation's and a restore's files pass
    /// through it.
    dir: PathBuf,
    network: Network,
    disks: Disks,
    volumes: Volumes,
    pods: Mutex<HashMap<String, Pod>>,
    events: broadcast::Sender<PodEvent>,
}

struct Pod {
    info: PodInfo,
    /// Held by what changes the pod: an operation, or its bring-up.
    lock: Arc<tokio::sync::Mutex<()>>,
    bring_up: Option<AbortHandle>,
}

impl Pods {
    /// The pods kept in `dir`, on the pods' network, with their disks from
    /// `disks` and the node's `volumes`. Those that were coming up come up
    /// now, and what hibernated pods left on the node goes; the others
    /// follow their microVMs from here on.
    pub async fn new(
        vms: VmApiHandler,
        dir: &Path,
        network: Network,
        disks: Disks,
        volumes: Volumes,
    ) -> io::Result<Self> {
        let store = Store::open(dir)?;
        // A hibernated pod has no addresses on the node.
        let (mut kept, hibernated): (Vec<_>, Vec<_>) = store
            .load()?
            .into_iter()
            .partition(|info| info.state() != PodState::Hibernated);
        for pod_id in network.adopt(&mut kept) {
            if let Some(info) = kept.iter().find(|info| info.pod_id == pod_id)
                && let Err(e) = store.save(info)
            {
                warn!("Pods: Failed to keep {pod_id}: {e}");
            }
        }
        kept.extend(hibernated);
        network.sync(&kept);
        network.serve_dns();
        volumes.reconcile(&kept);
        let (events, _) = broadcast::channel(256);
        let pods = Self {
            inner: Arc::new(Inner {
                vms,
                store,
                dir: dir.to_path_buf(),
                network,
                disks,
                volumes,
                pods: Mutex::new(HashMap::new()),
                events,
            }),
        };
        let mut pending = Vec::new();
        let mut running = Vec::new();
        let mut purge = Vec::new();
        for info in kept {
            match info.state() {
                PodState::Pending => pending.push(info.pod_id.clone()),
                PodState::Running => running.push(info.pod_id.clone()),
                PodState::Hibernated => purge.push(info.pod_id.clone()),
                _ => {}
            }
            pods.pods().insert(
                info.pod_id.clone(),
                Pod {
                    info,
                    lock: Arc::default(),
                    bring_up: None,
                },
            );
        }
        info!("Pods: {} kept", pods.pods().len());
        match pods.apply_network().await {
            Ok(()) => pods.inner.network.start_records(),
            Err(e) => pods
                .inner
                .network
                .fail(format!("the pods' rules are not written: {e}")),
        }
        let weak = Arc::downgrade(&pods.inner);
        pods.inner.network.groups().set_in_use(move |id| {
            weak.upgrade()
                .is_some_and(|inner| Pods { inner }.names_group(id))
        });
        for pod_id in running {
            if let Err(e) = pods.reattach(&pod_id).await {
                warn!("Pods: {pod_id} runs without its network: {}", e.message());
            }
        }
        for pod_id in pending {
            pods.spawn_bring_up(&pod_id);
        }
        // A hibernation that was cut off after its head was up.
        for pod_id in purge {
            let pods = pods.clone();
            tokio::spawn(async move {
                if let Ok(_lock) = pods.lock(&pod_id).await
                    && pods
                        .info(&pod_id)
                        .is_ok_and(|i| i.state() == PodState::Hibernated)
                {
                    pods.purge(&pod_id).await;
                }
            });
        }
        pods.follow_vms();
        pods.follow_security_groups();
        Ok(pods)
    }

    pub async fn create(&self, pod_id: String, spec: PodSpec) -> Result<PodInfo, Status> {
        spec::check_id(&pod_id)?;
        let spec = spec::normalize(spec)?;
        self.inner.network.check(&spec)?;
        if let Some(existing) = self.pods().get(&pod_id).map(|pod| pod.info.clone()) {
            return same_spec(existing, &spec);
        }
        // The pod claims its volumes before it is recorded, all or none.
        self.inner
            .volumes
            .claim(&pod_id, &spec::volume_ids(&spec))
            .await?;
        let recorded = {
            let mut pods = self.pods();
            match pods.get(&pod_id) {
                Some(pod) => Err(Ok(pod.info.clone())),
                None => {
                    let mut info = PodInfo {
                        pod_id: pod_id.clone(),
                        spec: Some(spec.clone()),
                        state: PodState::Pending as i32,
                        ..Default::default()
                    };
                    let mut allocated = Ok(());
                    if info.spec.as_ref().is_some_and(spec::has_network) {
                        let others = pods.values().map(|pod| &pod.info);
                        allocated = self.inner.network.allocate(others, &mut info);
                    }
                    let kept = allocated.and_then(|()| {
                        self.inner.store.save(&info).map_err(|e| {
                            self.inner.network.release(&info);
                            Status::internal(format!("keep pod {pod_id}: {e}"))
                        })
                    });
                    match kept {
                        Ok(()) => {
                            pods.insert(
                                pod_id.clone(),
                                Pod {
                                    info: info.clone(),
                                    lock: Arc::default(),
                                    bring_up: None,
                                },
                            );
                            Ok(info)
                        }
                        Err(e) => Err(Err(e)),
                    }
                }
            }
        };
        let info = match recorded {
            Ok(info) => info,
            // Another create was faster: what its pod does not name is let
            // go of again.
            Err(Ok(existing)) => {
                let named = existing
                    .spec
                    .as_ref()
                    .map(spec::volume_ids)
                    .unwrap_or_default();
                self.inner.volumes.release(&pod_id, &named).await;
                return same_spec(existing, &spec);
            }
            Err(Err(e)) => {
                self.inner.volumes.release(&pod_id, &[]).await;
                return Err(e);
            }
        };
        info!("Pods: {pod_id} created");
        // The pod's rules stand before its TAP device carries a frame.
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }
        self.announce(&info, false);
        self.spawn_bring_up(&pod_id);
        Ok(info)
    }

    /// Brings a stopped or failed pod up again, on its disk.
    pub async fn start(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.lock(pod_id).await?;
        match self.info(pod_id)?.state() {
            PodState::Pending | PodState::Running => return Ok(()),
            PodState::Suspended => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is suspended: resume it"
                )));
            }
            PodState::Hibernated => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is hibernated: resume or restore it, or stop it to boot it anew"
                )));
            }
            _ => {}
        }
        self.set_state(pod_id, PodState::Pending, "");
        drop(_lock);
        self.spawn_bring_up(pod_id);
        Ok(())
    }

    /// Ends the pod's microVM and stops its disk. Whatever state it is in,
    /// the next start boots it anew on its disk; a hibernated pod's is the
    /// one of its checkpoint.
    pub async fn stop(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.end_bring_up_and_lock(pod_id).await?;
        match self.info(pod_id)?.state() {
            PodState::Stopped => return Ok(()),
            PodState::Hibernated => {
                self.set_state(
                    pod_id,
                    PodState::Stopped,
                    "stopped: its memory is dropped, its disk is its checkpoint's",
                );
                return Ok(());
            }
            _ => {}
        }
        match self.vm(pod_id).await?.map(|vm| vm.state()) {
            None | Some(VmState::Stopped) => {}
            Some(VmState::Running) => {
                let request = Request::new(ShutdownVmRequest {
                    vm_id: pod_id.to_string(),
                });
                match self.inner.vms.shutdown_vm(request).await {
                    Ok(_) => self.settle(pod_id, VmState::Stopped).await?,
                    Err(e) => {
                        // A guest that does not go is ended.
                        warn!("Pods: {pod_id} did not shut down: {}", e.message());
                        self.delete_vm(pod_id).await?;
                    }
                }
            }
            Some(_) => self.delete_vm(pod_id).await?,
        }
        self.release_disk(pod_id).await;
        self.set_state(pod_id, PodState::Stopped, "stopped");
        Ok(())
    }

    /// Deletes the pod: its microVM, its disk, its place on the network,
    /// its record and its rules, and then frees its IPv4 address. Its
    /// volumes stay, stopped, and are attached to no pod then.
    pub async fn delete(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.end_bring_up_and_lock(pod_id).await?;
        self.delete_vm(pod_id).await?;
        // A pod whose disk cannot go stays, to be deleted again: diskd
        // would keep what it wrote for ever.
        self.inner.disks.close_memory(pod_id).await;
        if let Err(e) = self.inner.disks.remove(pod_id).await {
            self.set_state(
                pod_id,
                PodState::Failed,
                &format!("its disk is not deleted: {}", e.message()),
            );
            return Err(e);
        }
        let info = self.info(pod_id)?;
        self.stop_volumes(&info).await;
        self.inner
            .network
            .detach(&info)
            .await
            .map_err(|e| Status::internal(e.to_string()))?;
        self.inner
            .store
            .remove(pod_id)
            .map_err(|e| Status::internal(format!("forget pod {pod_id}: {e}")))?;
        if let Some(pod) = self.pods().remove(pod_id) {
            self.announce(&pod.info, true);
        }
        self.inner.volumes.release(pod_id, &[]).await;
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }
        self.inner.network.release(&info);
        info!("Pods: {pod_id} deleted");
        Ok(())
    }

    /// Writes the running pod to a snapshot; returns the size of its memory.
    pub async fn suspend(&self, pod_id: &str) -> Result<u64, Status> {
        let _lock = self.lock(pod_id).await?;
        self.expect_state(pod_id, PodState::Running)?;
        self.suspend_locked(pod_id).await
    }

    /// `suspend`, under the pod's lock. The snapshot holds what the guest
    /// wrote to its disk: Firecracker flushes it first. The disk stops
    /// with the microVM.
    async fn suspend_locked(&self, pod_id: &str) -> Result<u64, Status> {
        let request = Request::new(SuspendVmRequest {
            vm_id: pod_id.to_string(),
            purge_guest: true,
        });
        let response = self.inner.vms.suspend_vm(request).await?.into_inner();
        self.settle(pod_id, VmState::Suspended).await?;
        self.release_disk(pod_id).await;
        self.set_state(pod_id, PodState::Suspended, "suspended");
        Ok(response.memory_bytes)
    }

    /// Restores a suspended pod from its snapshot, or a hibernated one from
    /// its checkpoint. A resume that comes while the pod hibernates waits
    /// for its lock, and finds it hibernated.
    pub async fn resume(&self, pod_id: &str) -> Result<(), Status> {
        let lock = self.lock(pod_id).await?;
        if self.info(pod_id)?.state() == PodState::Hibernated {
            drop(lock);
            self.restore(pod_id, "", false).await?;
            return Ok(());
        }
        self.expect_state(pod_id, PodState::Suspended)?;
        let attachment = self.attach(pod_id).await?;
        self.open_disk(pod_id).await?;
        self.open_volumes(pod_id).await?;
        let request = Request::new(ResumeVmRequest {
            vm_id: pod_id.to_string(),
            remote: None,
        });
        self.inner.vms.resume_vm(request).await?;
        self.settle(pod_id, VmState::Running).await?;
        self.set_state(pod_id, PodState::Running, "");
        self.check_microvm_network(pod_id, attachment.as_ref())
            .await;
        Ok(())
    }

    /// Takes the pod off the node into object storage (ADR 0008): under
    /// its lock, so that a resume waits for it. Until its head is up, the
    /// pod stays on the node, suspended if it ran.
    pub async fn hibernate(&self, pod_id: &str) -> Result<HibernatePodResponse, Status> {
        let _lock = self.lock(pod_id).await?;
        let info = self.info(pod_id)?;
        if info.state() != PodState::Hibernated {
            // Before anything changes: without one, the pod stays as it is.
            self.inner.disks.require_object_store(pod_id).await?;
        }
        let memory = match info.state() {
            PodState::Hibernated => {
                return Ok(HibernatePodResponse {
                    checkpoint: info.checkpoint,
                    ..Default::default()
                });
            }
            PodState::Running => {
                self.set_state(pod_id, PodState::Running, "hibernating");
                if let Err(e) = self.suspend_locked(pod_id).await {
                    if self.info(pod_id)?.state() == PodState::Running {
                        self.set_state(pod_id, PodState::Running, "");
                    }
                    return Err(e);
                }
                true
            }
            // One whose snapshot is gone has its disk.
            PodState::Suspended => snapshot::exists(pod_id),
            PodState::Stopped | PodState::Failed => false,
            state => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is {}",
                    state.as_str_name()
                )));
            }
        };
        if !memory {
            // A failed pod's microVM may have left its disk started.
            self.delete_vm(pod_id).await?;
        }
        self.inner.disks.stop(pod_id).await?;
        let spec = info.spec.clone().unwrap_or_default();
        let epoch = next_epoch(self.checkpoint_epochs(&info));
        info!("Pods: {pod_id} hibernates, epoch {epoch}");
        let disk = self.inner.disks.seal(pod_id).await?;
        let (volume_heads, volume_checkpoints) =
            self.hibernate_volumes(pod_id, &spec, epoch).await?;
        let mut memory_bytes = 0;
        let memory = if memory {
            let host = snapshot::Host::load(pod_id)
                .map_err(|e| Status::internal(format!("the snapshot's host record: {e}")))?;
            let layer = self
                .inner
                .disks
                .seal_memory(&snapshot::memory(pod_id))
                .await?;
            memory_bytes = layer.logical_bytes;
            let vmstate_key = checkpoint_key(pod_id, epoch, "vmstate");
            self.inner
                .disks
                .put_object(&vmstate_key, &snapshot::vmstate(pod_id))
                .await?;
            Some(MemoryRef {
                layer: layer.layer_id,
                vmstate_key,
                firecracker_version: host.firecracker_version,
                cpu_model: host.cpu_model,
                kernel_release: host.kernel_release,
            })
        } else {
            None
        };
        // The spec, for a node that does not have the pod.
        self.put_record(
            &format!("{pod_id}.spec"),
            &checkpoint_key(pod_id, epoch, SPEC_OBJECT),
            &spec.encode_to_vec(),
        )
        .await?;
        let head = self
            .inner
            .disks
            .put_head(pod_id, epoch, memory, volume_heads)
            .await?;
        let checkpoint = Checkpoint {
            head_key: head.key,
            epoch,
            memory: head.memory.is_some(),
        };
        info!("Pods: {pod_id} is in {}", checkpoint.head_key);
        // Its volumes are in it, too.
        for (volume_id, volume_checkpoint) in volume_checkpoints {
            let checkpointed = self.inner.volumes.update(&volume_id, |volume| {
                volume.state = VolumeState::Checkpointed as i32;
                volume.checkpoint = Some(volume_checkpoint);
            });
            if let Err(e) = checkpointed {
                warn!(
                    "Pods: Failed to record that volume {volume_id} is checkpointed: {}",
                    e.message()
                );
            }
        }

        // From here on, the checkpoint is the pod. It leaves the network
        // as a deleted pod does, its addresses last.
        let placed = self.info(pod_id)?;
        if let Err(e) = self.inner.network.detach(&placed).await {
            warn!("Pods: Failed to take {pod_id} off the network: {e}");
        }
        self.update(pod_id, |info| {
            info.checkpoint = Some(checkpoint.clone());
            info.ipv6_address.clear();
            info.ipv4_address.clear();
        });
        self.set_state(pod_id, PodState::Hibernated, "hibernated");
        self.purge(pod_id).await;
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }
        self.inner.network.release(&placed);
        Ok(HibernatePodResponse {
            checkpoint: Some(checkpoint),
            memory_bytes,
            disk_bytes: disk.logical_bytes,
        })
    }

    /// Removes what a hibernated pod held on the node: its microVM with
    /// its snapshot, its disk and its TAP device. What fails is logged and
    /// left; the next start of feosd tries again.
    async fn purge(&self, pod_id: &str) {
        if let Err(e) = self.delete_vm(pod_id).await {
            warn!("Pods: Failed to remove {pod_id}'s microVM: {}", e.message());
        }
        self.inner.disks.close_memory(pod_id).await;
        if let Err(e) = self.inner.disks.remove(pod_id).await {
            warn!("Pods: Failed to remove {pod_id}'s disk: {}", e.message());
        }
        let Ok(info) = self.info(pod_id) else {
            return;
        };
        // The volumes in its checkpoint.
        for wanted in &info.spec.clone().unwrap_or_default().volumes {
            let checkpointed = self
                .inner
                .volumes
                .get(&wanted.volume_id)
                .is_ok_and(|volume| volume.state() == VolumeState::Checkpointed);
            if checkpointed
                && let Err(e) = self
                    .inner
                    .disks
                    .remove(&volume_disk(&wanted.volume_id))
                    .await
            {
                warn!(
                    "Pods: Failed to remove the disk of {pod_id}'s volume {}: {}",
                    wanted.name,
                    e.message()
                );
            }
        }
        if let Err(e) = self.inner.network.detach(&info).await {
            warn!("Pods: Failed to take {pod_id} off the network: {e}");
        }
    }

    /// Brings a hibernated pod back from the checkpoint `head_key`, or its
    /// own: on this node, or on one that does not have the pod, which
    /// takes its spec from the checkpoint. The memory is restored where
    /// the node can load the snapshot, unless `cold`; otherwise, and if the
    /// restore of the memory fails before the guest ran, the pod boots
    /// anew on the checkpoint's disk.
    pub async fn restore(
        &self,
        pod_id: &str,
        head_key: &str,
        cold: bool,
    ) -> Result<RestorePodResponse, Status> {
        spec::check_id(pod_id)?;
        if self.info(pod_id).is_err() {
            self.adopt_checkpoint(pod_id, head_key).await?;
        }
        let lock = self.end_bring_up_and_lock(pod_id).await?;
        let info = self.info(pod_id)?;
        let own = info.checkpoint.as_ref().map(|c| c.head_key.clone());
        let key = match head_key {
            "" => own.clone().unwrap_or_default(),
            key => key.to_string(),
        };
        match info.state() {
            PodState::Hibernated => {}
            // Restored already, by a call whose caller lost the answer.
            PodState::Pending | PodState::Running if own.as_deref() == Some(key.as_str()) => {
                return Ok(RestorePodResponse {
                    pod: Some(info),
                    ..Default::default()
                });
            }
            state => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is {}, not hibernated",
                    state.as_str_name()
                )));
            }
        }
        if key.is_empty() {
            return Err(Status::failed_precondition(format!(
                "pod {pod_id} has no checkpoint"
            )));
        }
        let head = self.inner.disks.get_head(&key).await?;
        if head.pod_id != pod_id {
            return Err(Status::invalid_argument(format!(
                "{key} is pod {}'s, not {pod_id}'s",
                head.pod_id
            )));
        }
        // Its volumes come from the checkpoint, too, or the restore is
        // refused before anything changes.
        let spec = info.spec.clone().unwrap_or_default();
        let volumes = self.checkpoint_volumes(pod_id, &spec, &head).await?;

        // A disk the node has of the pod is the checkpoint's at best, and
        // so are its volumes'.
        self.inner.disks.remove(pod_id).await?;
        for volume in &volumes {
            self.inner
                .disks
                .remove(&volume_disk(&volume.volume_id))
                .await?;
        }
        self.inner.volumes.take_over(pod_id, volumes).await?;
        self.place(pod_id)?;
        self.update(pod_id, |info| {
            info.checkpoint = Some(Checkpoint {
                head_key: key.clone(),
                epoch: head.epoch,
                memory: head.memory.is_some(),
            });
        });
        self.set_state(pod_id, PodState::Pending, &format!("restoring from {key}"));
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }

        let reason = match why_cold(cold, head.memory.as_ref()) {
            Some(reason) => reason,
            None => match self.restore_memory(pod_id, &head).await {
                Ok(()) => {
                    self.set_state(pod_id, PodState::Running, "");
                    info!("Pods: {pod_id} is restored from {key}, with its memory");
                    return Ok(RestorePodResponse {
                        pod: Some(self.info(pod_id)?),
                        memory_restored: true,
                        reason: String::new(),
                    });
                }
                Err(Restore::Booted(e)) => {
                    // The guest runs: what is left to do is a bring-up's.
                    warn!("Pods: {pod_id} runs from {key}, but: {}", e.message());
                    drop(lock);
                    self.spawn_bring_up(pod_id);
                    return Err(e);
                }
                Err(Restore::NotBooted(e)) => {
                    warn!("Pods: {pod_id}'s memory is not restored: {}", e.message());
                    format!("its memory did not restore: {}", e.message())
                }
            },
        };
        info!("Pods: {pod_id} boots anew on the disk of {key}: {reason}");
        self.set_state(
            pod_id,
            PodState::Pending,
            &format!("booting anew on its disk: {reason}"),
        );
        drop(lock);
        self.spawn_bring_up(pod_id);
        Ok(RestorePodResponse {
            pod: Some(self.info(pod_id)?),
            memory_restored: false,
            reason,
        })
    }

    /// Records a pod the node does not have, hibernated, from its
    /// checkpoint: its spec is beside the head.
    async fn adopt_checkpoint(&self, pod_id: &str, head_key: &str) -> Result<(), Status> {
        if head_key.is_empty() {
            return Err(Status::not_found(format!(
                "no pod {pod_id}: name its checkpoint's head to restore it here"
            )));
        }
        let head = self.inner.disks.get_head(head_key).await?;
        if head.pod_id != pod_id {
            return Err(Status::invalid_argument(format!(
                "{head_key} is pod {}'s, not {pod_id}'s",
                head.pod_id
            )));
        }
        let path = self.inner.dir.join(format!("{pod_id}.spec"));
        let key = checkpoint_key(pod_id, head.epoch, SPEC_OBJECT);
        let fetched = self.inner.disks.get_object(&key, &path).await;
        let read = fetched.and_then(|()| {
            std::fs::read(&path).map_err(|e| Status::internal(format!("read {key}: {e}")))
        });
        let _ = std::fs::remove_file(&path);
        let spec = PodSpec::decode(read?.as_slice())
            .map_err(|e| Status::invalid_argument(format!("{key}: {e}")))?;
        let spec = spec::normalize(spec)?;
        self.inner.network.check(&spec)?;
        // Before the pod is recorded: a restore that cannot take its
        // volumes leaves nothing on the node.
        self.checkpoint_volumes(pod_id, &spec, &head).await?;
        let info = {
            let mut pods = self.pods();
            if pods.contains_key(pod_id) {
                // Another restore was faster.
                return Ok(());
            }
            let info = PodInfo {
                pod_id: pod_id.to_string(),
                spec: Some(spec),
                state: PodState::Hibernated as i32,
                reason: "hibernated".to_string(),
                checkpoint: Some(Checkpoint {
                    head_key: head_key.to_string(),
                    epoch: head.epoch,
                    memory: head.memory.is_some(),
                }),
                ..Default::default()
            };
            self.inner
                .store
                .save(&info)
                .map_err(|e| Status::internal(format!("keep pod {pod_id}: {e}")))?;
            pods.insert(
                pod_id.to_string(),
                Pod {
                    info: info.clone(),
                    lock: Arc::default(),
                    bring_up: None,
                },
            );
            info
        };
        info!("Pods: {pod_id} comes from {head_key}");
        self.announce(&info, false);
        Ok(())
    }

    /// Restores the pod's memory from its checkpoint into a microVM that
    /// never booted, on the checkpoint's disk: diskd serves the memory as
    /// the guest asks for it.
    async fn restore_memory(&self, pod_id: &str, head: &Head) -> Result<(), Restore> {
        let memory = head.memory.clone().unwrap_or_default();
        let vmstate = self.inner.dir.join(format!("{pod_id}.vmstate"));
        let prepared = async {
            let info = self.info(pod_id)?;
            let attachment = self.attach(pod_id).await?;
            let disk = self.inner.disks.open_checkpoint(pod_id, &head.key).await?;
            // The snapshot's drives: its volumes, from their heads.
            let volumes = self.open_volumes(pod_id).await?;
            let config = spec::vm_config(
                pod_id,
                &info.spec.unwrap_or_default(),
                attachment.as_ref(),
                Some(&disk),
                &volumes,
            );
            self.delete_vm(pod_id).await?;
            let request = Request::new(CreateVmRequest {
                config: Some(config),
                vm_id: Some(pod_id.to_string()),
            });
            self.inner.vms.create_vm(request).await?;
            self.settle(pod_id, VmState::Created).await?;
            self.inner
                .disks
                .get_object(&memory.vmstate_key, &vmstate)
                .await?;
            self.inner
                .disks
                .open_memory(pod_id, &memory.layer, &memory_socket_path(pod_id))
                .await?;
            let request = Request::new(ResumeVmRequest {
                vm_id: pod_id.to_string(),
                remote: Some(RemoteSnapshot {
                    vmstate_path: vmstate.to_string_lossy().into_owned(),
                    firecracker_version: memory.firecracker_version.clone(),
                    cpu_model: memory.cpu_model.clone(),
                    kernel_release: memory.kernel_release.clone(),
                }),
            });
            self.inner.vms.resume_vm(request).await.map(drop)
        }
        .await;
        let _ = std::fs::remove_file(&vmstate);
        if let Err(e) = prepared {
            // The guest did not run: the disk is as the checkpoint has it,
            // for a boot.
            self.inner.disks.close_memory(pod_id).await;
            if let Err(e) = self.delete_vm(pod_id).await {
                warn!("Pods: Failed to remove {pod_id}'s microVM: {}", e.message());
            }
            return Err(Restore::NotBooted(e));
        }
        self.settle(pod_id, VmState::Running)
            .await
            .map_err(Restore::Booted)
    }

    /// Gives a pod that is to come back onto the node what it gave up when
    /// it hibernated: its addresses.
    fn place(&self, pod_id: &str) -> Result<(), Status> {
        let mut pods = self.pods();
        let Some(pod) = pods.get(pod_id) else {
            return Err(Status::not_found(format!("no pod {pod_id}")));
        };
        let info = &pod.info;
        if !info.spec.as_ref().is_some_and(spec::has_network) || !info.ipv6_address.is_empty() {
            return Ok(());
        }
        let mut info = info.clone();
        let others = pods
            .values()
            .filter(|pod| pod.info.pod_id != pod_id)
            .map(|pod| &pod.info);
        self.inner.network.allocate(others, &mut info)?;
        self.inner
            .store
            .save(&info)
            .map_err(|e| Status::internal(format!("keep pod {pod_id}: {e}")))?;
        if let Some(pod) = pods.get_mut(pod_id) {
            pod.info = info;
        }
        Ok(())
    }

    /// Stops the pod's disk and its volumes' when its microVM has ended,
    /// and lets go of the memory diskd served it. What fails is logged: the
    /// disk's next start takes what is there.
    async fn release_disk(&self, pod_id: &str) {
        self.inner.disks.close_memory(pod_id).await;
        if let Err(e) = self.inner.disks.stop(pod_id).await {
            warn!("Pods: Failed to stop {pod_id}'s disk: {}", e.message());
        }
        if let Ok(info) = self.info(pod_id) {
            self.stop_volumes(&info).await;
        }
    }

    /// Stops the disks of the pod's volumes, which keep what it wrote.
    async fn stop_volumes(&self, info: &PodInfo) {
        for wanted in &info.spec.clone().unwrap_or_default().volumes {
            let disk = volume_disk(&wanted.volume_id);
            if let Err(e) = self.inner.disks.stop(&disk).await {
                warn!(
                    "Pods: Failed to stop the disk of {}'s volume {}: {}",
                    info.pod_id,
                    wanted.name,
                    e.message()
                );
            }
        }
    }

    /// Starts the pod's disk: the one the node has, or its checkpoint's,
    /// or a new one. Returns the path Firecracker takes.
    async fn open_disk(&self, pod_id: &str) -> Result<String, Status> {
        let info = self.info(pod_id)?;
        let head_key = info
            .checkpoint
            .as_ref()
            .map(|checkpoint| checkpoint.head_key.as_str())
            .filter(|key| !key.is_empty());
        self.inner.disks.open(pod_id, head_key).await
    }

    /// Starts the disks of the pod's volumes, in the order of its spec: the
    /// ones the node has, or their checkpoints', or blank ones for volumes
    /// without their filesystem. Returns the paths Firecracker takes.
    async fn open_volumes(&self, pod_id: &str) -> Result<Vec<String>, Status> {
        let spec = self.info(pod_id)?.spec.unwrap_or_default();
        let mut paths = Vec::with_capacity(spec.volumes.len());
        for wanted in &spec.volumes {
            let failed = |e: Status| {
                Status::new(e.code(), format!("volume {}: {}", wanted.name, e.message()))
            };
            let volume = self.inner.volumes.get(&wanted.volume_id).map_err(|_| {
                Status::failed_precondition(format!(
                    "volume {}: no volume {} on this node",
                    wanted.name, wanted.volume_id
                ))
            })?;
            if volume.pod_id != pod_id {
                return Err(Status::failed_precondition(format!(
                    "volume {}: {} is attached to pod {:?}",
                    wanted.name, volume.volume_id, volume.pod_id
                )));
            }
            let path = self
                .inner
                .disks
                .open_volume(&volume, self.inner.volumes.dir())
                .await
                .map_err(failed)?;
            if volume.state() != VolumeState::Attached {
                self.inner
                    .volumes
                    .update(&volume.volume_id, |volume| {
                        volume.state = VolumeState::Attached as i32;
                    })
                    .map_err(failed)?;
            }
            paths.push(path);
        }
        Ok(paths)
    }

    /// Has the guest mount the pod's volumes, and format the ones the node
    /// knows to be new, and records which have their filesystem now. Fails
    /// unless every volume is mounted.
    async fn mount_volumes(&self, pod_id: &str, spec: &PodSpec) -> Result<(), Status> {
        if spec.volumes.is_empty() {
            return Ok(());
        }
        let mut request = MountVolumesRequest::default();
        for (index, wanted) in spec.volumes.iter().enumerate() {
            let volume = self.inner.volumes.get(&wanted.volume_id)?;
            request.volumes.push(GuestVolume {
                name: wanted.name.clone(),
                volume_id: volume.volume_id,
                // The pod's disk is the first drive.
                drive: index as u32 + 1,
                size_bytes: volume.size_bytes,
                format: !volume.formatted,
            });
        }
        let mut guest = GuestServiceClient::new(guest_channel(pod_id).await?);
        let response = timeout(MOUNT_TIMEOUT, guest.mount_volumes(request))
            .await
            .map_err(|_| {
                Status::deadline_exceeded(format!(
                    "the guest did not mount the volumes within {}s",
                    MOUNT_TIMEOUT.as_secs()
                ))
            })?
            .map_err(|e| match e.code() {
                Code::Unimplemented => Status::failed_precondition(
                    "the guest mounts no volumes: it is older than volumes are",
                ),
                code => Status::new(code, format!("the guest's volumes: {}", e.message())),
            })?
            .into_inner();
        let mut failed = Vec::new();
        if response.volumes.len() != spec.volumes.len() {
            failed.push(format!(
                "the guest answered for {} of {} volumes",
                response.volumes.len(),
                spec.volumes.len()
            ));
        }
        for (wanted, mounted) in spec.volumes.iter().zip(&response.volumes) {
            if !mounted.error.is_empty() {
                failed.push(format!("volume {}: {}", wanted.name, mounted.error));
                continue;
            }
            if mounted.formatted {
                info!(
                    "Pods: {pod_id}'s guest made the filesystem of volume {} on {}",
                    wanted.name, mounted.device
                );
            }
            if !self.inner.volumes.get(&wanted.volume_id)?.formatted {
                self.inner
                    .volumes
                    .update(&wanted.volume_id, |volume| volume.formatted = true)?;
            }
        }
        if !failed.is_empty() {
            return Err(Status::failed_precondition(failed.join("; ")));
        }
        Ok(())
    }

    /// Seals and uploads each of the pod's volumes into its checkpoint
    /// `epoch`: its chain, its record and its head, which the pod's head
    /// is to name. A volume that is checkpointed already, of a pod that has
    /// not booted since, keeps its head. Returns the heads, and the new
    /// checkpoints of the volumes.
    async fn hibernate_volumes(
        &self,
        pod_id: &str,
        spec: &PodSpec,
        epoch: u64,
    ) -> Result<(Vec<String>, Vec<(String, VolumeCheckpoint)>), Status> {
        let mut heads = Vec::new();
        let mut checkpoints = Vec::new();
        for wanted in &spec.volumes {
            let volume = self.inner.volumes.get(&wanted.volume_id)?;
            if volume.pod_id != pod_id {
                return Err(Status::failed_precondition(format!(
                    "volume {} is attached to pod {:?}",
                    volume.volume_id, volume.pod_id
                )));
            }
            if volume.state() == VolumeState::Checkpointed
                && let Some(checkpoint) = &volume.checkpoint
            {
                heads.push(checkpoint.head_key.clone());
                continue;
            }
            let disk = volume_disk(&volume.volume_id);
            // One that never booted gets its blank disk now.
            self.inner
                .disks
                .open_volume(&volume, self.inner.volumes.dir())
                .await?;
            self.inner.disks.stop(&disk).await?;
            self.inner.disks.seal(&disk).await?;
            // Its record, for a node that does not have it.
            let record = Volume {
                volume_id: volume.volume_id.clone(),
                size_bytes: volume.size_bytes,
                labels: volume.labels.clone(),
                formatted: volume.formatted,
                ..Default::default()
            };
            self.put_record(
                &format!("{disk}.volume"),
                &checkpoint_key(&disk, epoch, VOLUME_OBJECT),
                &record.encode_to_vec(),
            )
            .await?;
            let head = self
                .inner
                .disks
                .put_head(&disk, epoch, None, Vec::new())
                .await?;
            info!("Pods: {pod_id}'s volume {} is in {}", wanted.name, head.key);
            heads.push(head.key.clone());
            checkpoints.push((
                volume.volume_id,
                VolumeCheckpoint {
                    head_key: head.key,
                    epoch,
                },
            ));
        }
        Ok((heads, checkpoints))
    }

    /// The pod's volumes as its checkpoint, the head `head`, has them, for a
    /// restore to take over. Fails before anything changes when the
    /// checkpoint lacks a volume, or when the node has one that a restore
    /// may not take: attached to another pod, or with data of its own.
    async fn checkpoint_volumes(
        &self,
        pod_id: &str,
        spec: &PodSpec,
        head: &Head,
    ) -> Result<Vec<Volume>, Status> {
        let mut volumes = Vec::with_capacity(spec.volumes.len());
        for wanted in &spec.volumes {
            if let Ok(had) = self.inner.volumes.get(&wanted.volume_id) {
                volumes::check_restorable(pod_id, &had)?;
            }
            let disk = volume_disk(&wanted.volume_id);
            let prefix = format!("snapshots/{disk}/");
            let key = head
                .volume_heads
                .iter()
                .find(|key| key.starts_with(&prefix))
                .ok_or_else(|| {
                    Status::failed_precondition(format!(
                        "{} has no head of volume {}",
                        head.key, wanted.volume_id
                    ))
                })?;
            let volume_head = self.inner.disks.get_head(key).await?;
            let record = self
                .get_record(
                    &format!("{disk}.volume"),
                    &checkpoint_key(&disk, volume_head.epoch, VOLUME_OBJECT),
                )
                .await?;
            let record = Volume::decode(record.as_slice())
                .map_err(|e| Status::invalid_argument(format!("{key}'s record: {e}")))?;
            volumes.push(Volume {
                volume_id: wanted.volume_id.clone(),
                size_bytes: volume_head.logical_size,
                labels: record.labels,
                state: VolumeState::Checkpointed as i32,
                pod_id: pod_id.to_string(),
                formatted: record.formatted,
                checkpoint: Some(VolumeCheckpoint {
                    head_key: key.clone(),
                    epoch: volume_head.epoch,
                }),
            });
        }
        Ok(volumes)
    }

    /// Puts `bytes` into the object store at `key`, through a file `name`
    /// in the pods' directory.
    async fn put_record(&self, name: &str, key: &str, bytes: &[u8]) -> Result<(), Status> {
        let path = self.inner.dir.join(name);
        std::fs::write(&path, bytes)
            .map_err(|e| Status::internal(format!("write {}: {e}", path.display())))?;
        let put = self.inner.disks.put_object(key, &path).await;
        let _ = std::fs::remove_file(&path);
        put
    }

    /// Gets the object at `key` from the object store, through a file
    /// `name` in the pods' directory.
    async fn get_record(&self, name: &str, key: &str) -> Result<Vec<u8>, Status> {
        let path = self.inner.dir.join(name);
        let fetched = self.inner.disks.get_object(key, &path).await;
        let read = fetched.and_then(|()| {
            std::fs::read(&path).map_err(|e| Status::internal(format!("read {key}: {e}")))
        });
        let _ = std::fs::remove_file(&path);
        read
    }

    /// The epochs of the pod's last checkpoint and of its volumes': its
    /// next is past all of them.
    fn checkpoint_epochs(&self, info: &PodInfo) -> Vec<u64> {
        let mut epochs: Vec<u64> = info.checkpoint.iter().map(|c| c.epoch).collect();
        for wanted in &info.spec.clone().unwrap_or_default().volumes {
            if let Ok(volume) = self.inner.volumes.get(&wanted.volume_id) {
                epochs.extend(volume.checkpoint.map(|checkpoint| checkpoint.epoch));
            }
        }
        epochs
    }

    /// Whether a pod on the node names the security group.
    fn names_group(&self, id: &str) -> bool {
        self.pods().values().any(|pod| {
            let network = pod
                .info
                .spec
                .as_ref()
                .and_then(|spec| spec.network.as_ref());
            network.is_some_and(|network| network.security_groups.iter().any(|g| g == id))
        })
    }

    /// Puts a pod that runs from before feosd started on the network again.
    async fn reattach(&self, pod_id: &str) -> Result<(), Status> {
        let attachment = self.attach(pod_id).await?;
        self.check_microvm_network(pod_id, attachment.as_ref())
            .await;
        Ok(())
    }

    /// Brings a running microVM that was made for another network in line.
    /// Where only its guest's configuration differs, such as one of IPv6
    /// alone with the resolver of that version, the guest is pushed this
    /// one and takes it at once. A microVM whose devices differ, such as
    /// the first version's with IPv4 on a bridge, keeps its network until
    /// the pod boots anew, and so does one from before pods had a metadata
    /// service; feosd warns.
    async fn check_microvm_network(&self, pod_id: &str, attachment: Option<&Attachment>) {
        let (Ok(info), Ok(Some(vm))) = (self.info(pod_id), self.vm(pod_id).await) else {
            return;
        };
        let config = vm.config.clone().unwrap_or_default();
        let disk = config.data_disk.as_ref().map(|disk| disk.path.as_str());
        let volumes: Vec<String> = config.volumes.iter().map(|v| v.path.clone()).collect();
        let wanted = spec::vm_config(
            pod_id,
            &info.spec.unwrap_or_default(),
            attachment,
            disk,
            &volumes,
        );
        if !stale(&vm, &wanted) {
            return;
        }
        if config.net != wanted.net {
            warn!(
                "Pods: {pod_id}'s microVM was made for another network; a stop and a start give it this one"
            );
            return;
        }
        if config.guest_network != wanted.guest_network {
            match wanted.guest_network {
                Some(network) => match self.inner.vms.set_guest_network(pod_id, network).await {
                    Ok(true) => info!("Pods: {pod_id}'s guest took this network"),
                    Ok(false) => {
                        info!("Pods: {pod_id}'s guest takes this network when it attaches")
                    }
                    Err(e) => warn!(
                        "Pods: {pod_id}'s guest keeps another network for now: {}",
                        e.message()
                    ),
                },
                None => warn!(
                    "Pods: {pod_id}'s microVM was made for another network; a stop and a start give it this one"
                ),
            }
        }
        if config.metadata_json != wanted.metadata_json {
            warn!(
                "Pods: {pod_id}'s microVM was made for other metadata; a stop and a start give it this one"
            );
        }
    }

    /// The pod, with its microVM and its containers as the guest reports
    /// them if it runs.
    pub async fn get(&self, pod_id: &str) -> Result<PodInfo, Status> {
        let mut info = self.info(pod_id)?;
        if info.state() == PodState::Running {
            info.microvm = self.microvm(pod_id).await;
            info.containers = self.container_status(&info).await;
        }
        Ok(info)
    }

    /// The serial console of the pod's microVM. The first message names the
    /// pod.
    pub async fn console(
        &self,
        mut input: impl Stream<Item = Result<PodConsoleInput, Status>> + Send + Unpin + 'static,
    ) -> Result<impl Stream<Item = Result<PodConsoleOutput, Status>> + Send + 'static, Status> {
        let first = input.next().await.ok_or_else(|| {
            Status::invalid_argument("the console's first message names no pod")
        })??;
        self.check_reachable(&first.pod_id)?;
        let data = |input: Vec<u8>| StreamVmConsoleRequest {
            payload: Some(console_request::Payload::Data(ConsoleData { input })),
        };
        let mut start = vec![Ok(StreamVmConsoleRequest {
            payload: Some(console_request::Payload::Attach(AttachConsoleMessage {
                vm_id: first.pod_id,
            })),
        })];
        if !first.input.is_empty() {
            start.push(Ok(data(first.input)));
        }
        let rest = input.map(move |message| message.map(|message| data(message.input)));
        let output = self
            .inner
            .vms
            .console(Box::pin(tokio_stream::iter(start).chain(rest)))
            .await?;
        Ok(output.map(|output| {
            output.map(|output| PodConsoleOutput {
                output: output.output,
            })
        }))
    }

    pub fn list(&self) -> Vec<PodInfo> {
        let mut pods: Vec<PodInfo> = self.pods().values().map(|pod| pod.info.clone()).collect();
        pods.sort_by(|a, b| a.pod_id.cmp(&b.pod_id));
        pods
    }

    pub fn subscribe(&self) -> broadcast::Receiver<PodEvent> {
        self.inner.events.subscribe()
    }

    /// Whether calls may go to the pod's guest: while its microVM runs.
    pub fn check_reachable(&self, pod_id: &str) -> Result<(), Status> {
        spec::check_id(pod_id)?;
        match self.info(pod_id)?.state() {
            PodState::Pending | PodState::Running => Ok(()),
            state => Err(Status::failed_precondition(format!(
                "pod {pod_id} is {}",
                state.as_str_name()
            ))),
        }
    }

    fn pods(&self) -> MutexGuard<'_, HashMap<String, Pod>> {
        self.inner.pods.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn info(&self, pod_id: &str) -> Result<PodInfo, Status> {
        self.pods()
            .get(pod_id)
            .map(|pod| pod.info.clone())
            .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))
    }

    fn expect_state(&self, pod_id: &str, state: PodState) -> Result<(), Status> {
        let actual = self.info(pod_id)?.state();
        if actual == state {
            Ok(())
        } else {
            Err(Status::failed_precondition(format!(
                "pod {pod_id} is {}, not {}",
                actual.as_str_name(),
                state.as_str_name()
            )))
        }
    }

    /// Takes the pod's lock, and makes sure the pod is still there.
    async fn lock(&self, pod_id: &str) -> Result<OwnedMutexGuard<()>, Status> {
        let lock = self
            .pods()
            .get(pod_id)
            .map(|pod| pod.lock.clone())
            .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))?;
        let guard = lock.lock_owned().await;
        self.info(pod_id)?;
        Ok(guard)
    }

    async fn end_bring_up_and_lock(&self, pod_id: &str) -> Result<OwnedMutexGuard<()>, Status> {
        if let Some(task) = self
            .pods()
            .get_mut(pod_id)
            .and_then(|pod| pod.bring_up.take())
        {
            task.abort();
        }
        self.lock(pod_id).await
    }

    /// Changes the pod's record and keeps it, without an event.
    fn update(&self, pod_id: &str, change: impl FnOnce(&mut PodInfo)) {
        let mut pods = self.pods();
        let Some(pod) = pods.get_mut(pod_id) else {
            return;
        };
        change(&mut pod.info);
        if let Err(e) = self.inner.store.save(&pod.info) {
            warn!("Pods: Failed to keep {pod_id}: {e}");
        }
    }

    fn set_state(&self, pod_id: &str, state: PodState, reason: &str) {
        let info = {
            let mut pods = self.pods();
            let Some(pod) = pods.get_mut(pod_id) else {
                return;
            };
            if pod.info.state() == state && pod.info.reason == reason {
                return;
            }
            pod.info.state = state as i32;
            pod.info.reason = reason.to_string();
            if let Err(e) = self.inner.store.save(&pod.info) {
                warn!("Pods: Failed to keep {pod_id}: {e}");
            }
            pod.info.clone()
        };
        if reason.is_empty() {
            info!("Pods: {pod_id} is {}", state.as_str_name());
        } else {
            info!("Pods: {pod_id} is {}: {reason}", state.as_str_name());
        }
        self.announce(&info, false);
    }

    fn announce(&self, info: &PodInfo, deleted: bool) {
        let _ = self.inner.events.send(PodEvent {
            pod_id: info.pod_id.clone(),
            state: info.state,
            reason: info.reason.clone(),
            deleted,
        });
    }

    fn spawn_bring_up(&self, pod_id: &str) {
        let pods = self.clone();
        let id = pod_id.to_string();
        let task = tokio::spawn(async move {
            let Ok(_lock) = pods.lock(&id).await else {
                return;
            };
            let Ok(info) = pods.info(&id) else {
                return;
            };
            if info.state() != PodState::Pending {
                return;
            }
            match pods.bring_up(&id, &info.spec.unwrap_or_default()).await {
                Ok(()) => pods.set_state(&id, PodState::Running, ""),
                Err(e) => pods.set_state(&id, PodState::Failed, e.message()),
            }
        });
        if let Some(pod) = self.pods().get_mut(pod_id)
            && let Some(previous) = pod.bring_up.replace(task.abort_handle())
        {
            previous.abort();
        }
    }

    async fn bring_up(&self, pod_id: &str, spec: &PodSpec) -> Result<(), Status> {
        let booted = self.boot(pod_id, spec).await?;
        self.wait_for_guest(pod_id).await?;
        self.mount_volumes(pod_id, spec).await?;
        let mut guest = ContainerServiceClient::new(guest_channel(pod_id).await?);
        for container in &spec.containers {
            run_container(&mut guest, container, booted)
                .await
                .map_err(|e| {
                    Status::new(
                        e.code(),
                        format!("container {}: {}", container.name, e.message()),
                    )
                })?;
        }
        Ok(())
    }

    /// Gets the pod's microVM running on its disk, from whatever state it
    /// is in. Returns whether it booted it anew.
    async fn boot(&self, pod_id: &str, spec: &PodSpec) -> Result<bool, Status> {
        let vm_id = pod_id.to_string();
        self.place(pod_id)?;
        let attachment = self.attach(pod_id).await?;
        let disk = self.open_disk(pod_id).await?;
        let volumes = self.open_volumes(pod_id).await?;
        let config = spec::vm_config(pod_id, spec, attachment.as_ref(), Some(&disk), &volumes);
        loop {
            let vm = self.vm(pod_id).await?;
            match vm.as_ref().map(|vm| vm.state()) {
                None => {
                    let request = Request::new(CreateVmRequest {
                        config: Some(config.clone()),
                        vm_id: Some(vm_id.clone()),
                    });
                    self.inner.vms.create_vm(request).await?;
                }
                Some(VmState::Creating) => sleep(POLL).await,
                // One made for another network or disk is made anew, such
                // as one of the first version's with IPv4, or one from
                // before pods had disks or a metadata service.
                Some(VmState::Created | VmState::Stopped)
                    if vm.as_ref().is_some_and(|vm| stale(vm, &config)) =>
                {
                    info!("Pods: {pod_id}'s microVM was made for another network, metadata or disk: making it anew");
                    self.delete_vm(pod_id).await?;
                }
                Some(VmState::Created | VmState::Stopped) => {
                    let request = Request::new(StartVmRequest { vm_id });
                    self.inner.vms.start_vm(request).await?;
                    return Ok(true);
                }
                Some(VmState::Running) => return Ok(false),
                Some(VmState::Paused) => {
                    let request = Request::new(ResumeVmRequest {
                        vm_id,
                        remote: None,
                    });
                    self.inner.vms.resume_vm(request).await?;
                    return Ok(false);
                }
                // A crashed microVM does not start again: it is made anew.
                Some(VmState::Crashed) => self.delete_vm(pod_id).await?,
                Some(state @ (VmState::Suspended | VmState::Unspecified)) => {
                    return Err(Status::failed_precondition(format!(
                        "the microVM is {}",
                        state.as_str_name()
                    )));
                }
            }
        }
    }

    async fn wait_for_guest(&self, pod_id: &str) -> Result<(), Status> {
        let deadline = Instant::now() + GUEST_TIMEOUT;
        // The VM service records the start of a microVM after StartVm
        // returns: until it runs, a stopped one has not started yet.
        let mut started = false;
        loop {
            let vm = self
                .vm(pod_id)
                .await?
                .ok_or_else(|| Status::aborted("the microVM is gone"))?;
            if let Some(guest) = vm.guest.as_ref().filter(|guest| guest.attached) {
                if !guest.push_error.is_empty() {
                    warn!(
                        "Pods: {pod_id}'s guest took no configuration: {}",
                        guest.push_error
                    );
                }
                return Ok(());
            }
            started |= vm.state() == VmState::Running;
            if started && matches!(vm.state(), VmState::Stopped | VmState::Crashed) {
                return Err(Status::aborted(format!(
                    "the microVM ended while it booted: {}",
                    vm.state().as_str_name()
                )));
            }
            if Instant::now() >= deadline {
                return Err(Status::deadline_exceeded(format!(
                    "the guest did not attach within {}s; the microVM is {}",
                    GUEST_TIMEOUT.as_secs(),
                    vm.state().as_str_name()
                )));
            }
            sleep(POLL).await;
        }
    }

    /// Puts the pod on the network, if it has one: its TAP device, its
    /// routes, proxy ARP for its gateway and the resolver, before every
    /// boot and resume. Fails while the node does not have the pod's
    /// security groups.
    async fn attach(&self, pod_id: &str) -> Result<Option<Attachment>, Status> {
        self.inner.network.attach(&self.info(pod_id)?).await
    }

    /// Writes the pods' rules for the pods there are.
    async fn apply_network(&self) -> io::Result<()> {
        let pods = self.list();
        self.inner.network.apply(&pods).await
    }

    /// Writes the pods' rules again whenever the security groups change,
    /// and every so often.
    fn follow_security_groups(&self) {
        let pods = self.clone();
        let mut changes = self.inner.network.groups().subscribe();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    changed = changes.changed() => {
                        if changed.is_err() {
                            return;
                        }
                    }
                    _ = sleep(REFRESH) => {}
                }
                if let Err(e) = pods.apply_network().await {
                    warn!("Pods: Failed to set the pods' rules: {e}");
                }
            }
        });
    }

    /// Waits until the VM service records the microVM in `state`: it
    /// records a change after the call that made it returned.
    async fn settle(&self, pod_id: &str, state: VmState) -> Result<(), Status> {
        let deadline = Instant::now() + SETTLE_TIMEOUT;
        loop {
            let actual = self.vm(pod_id).await?.map(|vm| vm.state());
            if actual == Some(state) {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(Status::deadline_exceeded(format!(
                    "the microVM is {}, not {}",
                    actual.map_or("gone", |actual| actual.as_str_name()),
                    state.as_str_name()
                )));
            }
            sleep(POLL).await;
        }
    }

    async fn vm(&self, pod_id: &str) -> Result<Option<VmInfo>, Status> {
        let request = Request::new(GetVmRequest {
            vm_id: pod_id.to_string(),
        });
        match self.inner.vms.get_vm(request).await {
            Ok(vm) => Ok(Some(vm.into_inner())),
            Err(e) if e.code() == Code::NotFound => Ok(None),
            Err(e) => Err(e),
        }
    }

    /// The running microVM, as far as it answers.
    async fn microvm(&self, pod_id: &str) -> Option<MicroVm> {
        let vm = self.vm(pod_id).await.ok()??;
        let request = Request::new(PingVmRequest {
            vm_id: pod_id.to_string(),
        });
        let pid = match self.inner.vms.ping_vm(request).await {
            Ok(ping) => ping.into_inner().pid,
            Err(_) => 0,
        };
        let guest = vm.guest.unwrap_or_default();
        Some(MicroVm {
            pid,
            guest_version: guest.feos_version,
            guest_attached: guest.attached,
        })
    }

    async fn delete_vm(&self, pod_id: &str) -> Result<(), Status> {
        let request = Request::new(DeleteVmRequest {
            vm_id: pod_id.to_string(),
        });
        match self.inner.vms.delete_vm(request).await {
            Err(e) if e.code() != Code::NotFound => Err(e),
            _ => Ok(()),
        }
    }

    async fn container_status(&self, info: &PodInfo) -> Vec<ContainerStatus> {
        let list = timeout(STATUS_TIMEOUT, async {
            let mut guest = ContainerServiceClient::new(guest_channel(&info.pod_id).await.ok()?);
            guest
                .list_containers(ListContainersRequest {})
                .await
                .ok()
                .map(|list| list.into_inner().containers)
        })
        .await;
        let Ok(Some(containers)) = list else {
            return Vec::new();
        };
        let spec = info.spec.clone().unwrap_or_default();
        spec.containers
            .iter()
            .filter_map(|wanted| {
                let container = containers.iter().find(|c| c.container_id == wanted.name)?;
                Some(ContainerStatus {
                    name: wanted.name.clone(),
                    state: match container.state() {
                        GuestContainerState::PullingImage => ContainerState::Creating,
                        GuestContainerState::Created => ContainerState::Created,
                        GuestContainerState::Running => ContainerState::Running,
                        GuestContainerState::Stopped => ContainerState::Exited,
                        GuestContainerState::Unspecified => ContainerState::Unspecified,
                    } as i32,
                    exit_code: container.exit_code,
                })
            })
            .collect()
    }

    /// Follows the microVMs' states: a running pod whose microVM crashed
    /// failed, and one whose microVM stopped is stopped. While an operation
    /// or a bring-up holds the pod, it sees to that itself.
    fn follow_vms(&self) {
        let pods = self.clone();
        tokio::spawn(async move {
            loop {
                let request = Request::new(StreamVmEventsRequest::default());
                match pods.inner.vms.stream_vm_events(request).await {
                    Ok(stream) => {
                        let mut stream = stream.into_inner();
                        while let Some(Ok(event)) = stream.next().await {
                            pods.on_vm_event(event);
                        }
                    }
                    Err(e) => warn!("Pods: No events of the microVMs: {}", e.message()),
                }
                sleep(Duration::from_secs(1)).await;
            }
        });
    }

    fn on_vm_event(&self, event: VmEvent) {
        let Some(data) = event.data else {
            return;
        };
        if !data
            .type_url
            .ends_with("feos.microvm.v1.VmStateChangedEvent")
        {
            return;
        }
        let Ok(changed) = VmStateChangedEvent::decode(data.value.as_slice()) else {
            return;
        };
        let next = {
            let pods = self.pods();
            let Some(pod) = pods.get(&event.vm_id) else {
                return;
            };
            if pod.lock.try_lock().is_err() || pod.info.state() != PodState::Running {
                return;
            }
            match changed.new_state() {
                VmState::Crashed => Some((
                    PodState::Failed,
                    format!("the microVM crashed: {}", changed.reason),
                )),
                VmState::Stopped => Some((PodState::Stopped, "the microVM stopped".to_string())),
                _ => None,
            }
        };
        if let Some((state, reason)) = next {
            self.set_state(&event.vm_id, state, &reason);
            // The microVM is gone; its disk keeps what the pod wrote, for
            // the next start.
            let pods = self.clone();
            let pod_id = event.vm_id;
            tokio::spawn(async move {
                if let Ok(_lock) = pods.lock(&pod_id).await
                    && pods.info(&pod_id).is_ok_and(|info| {
                        matches!(info.state(), PodState::Failed | PodState::Stopped)
                    })
                {
                    pods.release_disk(&pod_id).await;
                }
            });
        }
    }
}

/// What a restore of a pod's memory left.
enum Restore {
    /// The guest did not run: the disk is as the checkpoint has it.
    NotBooted(Status),
    /// The guest ran, and something after it failed.
    Booted(Status),
}

impl From<Status> for Restore {
    fn from(e: Status) -> Self {
        Restore::NotBooted(e)
    }
}

/// Why a restore boots the pod anew on its checkpoint's disk instead of
/// restoring its memory, if it does: it was asked to, the checkpoint
/// holds no memory, or this node cannot load the snapshot. That is
/// decided before anything starts, from what the snapshot pins; a load
/// that fails all the same falls back, too, as long as the guest did not
/// run.
fn why_cold(cold: bool, memory: Option<&MemoryRef>) -> Option<String> {
    match memory {
        _ if cold => Some("asked to boot anew".to_string()),
        None => Some("its checkpoint holds no memory".to_string()),
        Some(memory) => snapshot::Host {
            firecracker_version: memory.firecracker_version.clone(),
            cpu_model: memory.cpu_model.clone(),
            kernel_release: memory.kernel_release.clone(),
        }
        .unrestorable_here(),
    }
}

/// The object of a checkpoint that holds the pod's spec, for a node that
/// does not have the pod.
const SPEC_OBJECT: &str = "pod.pb";
/// The object of a volume's checkpoint that holds its record, a
/// `feos.volume.v1.Volume`: its size, labels and whether it has its
/// filesystem.
const VOLUME_OBJECT: &str = "volume.pb";

/// The key of an object of the pod's checkpoint `epoch`, beside its head.
fn checkpoint_key(pod_id: &str, epoch: u64, name: &str) -> String {
    format!("snapshots/{pod_id}/{epoch}/{name}")
}

/// The epoch of the pod's next checkpoint: the seconds since 1970, or one
/// more than the last one's of the pod and of its volumes, so that it grows
/// and no head of the pod or of a volume is written twice.
fn next_epoch(last: impl IntoIterator<Item = u64>) -> u64 {
    let now = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |since| since.as_secs());
    now.max(last.into_iter().map(|epoch| epoch + 1).max().unwrap_or(0))
}

/// Whether the microVM's network, its metadata or its disks are not the
/// ones `wanted` has.
fn stale(vm: &VmInfo, wanted: &VmConfig) -> bool {
    let config = vm.config.as_ref();
    config.map(|config| {
        (
            &config.net,
            &config.guest_network,
            &config.metadata_json,
            &config.data_disk,
            &config.volumes,
        )
    }) != Some((
        &wanted.net,
        &wanted.guest_network,
        &wanted.metadata_json,
        &wanted.data_disk,
        &wanted.volumes,
    ))
}

/// A create of a pod that exists finds it if the spec is the same.
fn same_spec(existing: PodInfo, spec: &PodSpec) -> Result<PodInfo, Status> {
    if existing.spec.as_ref() == Some(spec) {
        Ok(existing)
    } else {
        Err(Status::already_exists(format!(
            "pod {} exists with another spec",
            existing.pod_id
        )))
    }
}

/// A channel to the API of the pod's guest.
pub(crate) async fn guest_channel(pod_id: &str) -> Result<Channel, Status> {
    vsock::channel(vsock_path(pod_id), API_PORT)
        .await
        .map_err(|e| Status::unavailable(format!("the guest of pod {pod_id}: {e}")))
}

/// Gets a container running in the guest, from whatever is there. After a
/// new boot, a container that the pod's disk has from the boot before is
/// stopped, and is adopted with what it wrote; one that is not there is
/// created.
async fn run_container(
    guest: &mut ContainerServiceClient<Channel>,
    container: &ContainerSpec,
    booted: bool,
) -> Result<(), Status> {
    let container_id = container.name.clone();
    let create = CreateContainerRequest {
        config: Some(ContainerConfig {
            image_ref: container.image_ref.clone(),
            command: container.command.clone(),
            env: container.env.clone(),
            volume_mounts: container
                .volume_mounts
                .iter()
                .map(|mount| GuestVolumeMount {
                    volume: mount.volume.clone(),
                    path: mount.path.clone(),
                    read_only: mount.read_only,
                })
                .collect(),
        }),
        container_id: Some(container_id.clone()),
        // Without a container to adopt, a plain create.
        adopt: true,
    };
    // Adopted once at most: one that stops again exited in this boot.
    let mut adopt = booted;
    loop {
        let request = GetContainerRequest {
            container_id: container_id.clone(),
        };
        match guest.get_container(request).await {
            Err(e) if e.code() == Code::NotFound => {
                guest.create_container(create.clone()).await?;
            }
            Err(e) => return Err(e),
            Ok(info) => match info.into_inner().state() {
                // A create that a cut-off bring-up made is still pulling.
                GuestContainerState::PullingImage => sleep(POLL).await,
                GuestContainerState::Created => {
                    let request = StartContainerRequest { container_id };
                    guest.start_container(request).await?;
                    return Ok(());
                }
                GuestContainerState::Stopped if adopt => {
                    adopt = false;
                    guest.create_container(create.clone()).await?;
                }
                GuestContainerState::Running | GuestContainerState::Stopped => return Ok(()),
                GuestContainerState::Unspecified => {
                    return Err(Status::internal("the guest reports no state"));
                }
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn memory(firecracker_version: &str) -> MemoryRef {
        let here = snapshot::Host::this(firecracker_version);
        MemoryRef {
            layer: "aa".to_string(),
            vmstate_key: "snapshots/p/1/vmstate".to_string(),
            firecracker_version: here.firecracker_version,
            cpu_model: here.cpu_model,
            kernel_release: here.kernel_release,
        }
    }

    #[test]
    fn a_checkpoint_boots_anew_where_its_memory_cannot_be_restored() {
        assert_eq!(
            why_cold(true, Some(&memory("1.17.0"))).unwrap(),
            "asked to boot anew"
        );
        assert!(why_cold(false, None).unwrap().contains("no memory"));
        // A Firecracker no node has.
        let reason = why_cold(false, Some(&memory("0.0.0-none"))).unwrap();
        assert!(reason.contains("not installed"), "{reason}");
        // What the snapshot does not record counts as different.
        let mut unknown = memory("0.0.0-none");
        unknown.cpu_model.clear();
        assert!(why_cold(false, Some(&unknown)).is_some());
    }

    #[test]
    fn epochs_grow_and_are_never_written_twice() {
        let now = next_epoch(None);
        assert!(now > 1_700_000_000);
        assert_eq!(next_epoch(Some(now + 100)), now + 101);
        // A volume's last checkpoint counts, too: it may come from a pod
        // that hibernated later.
        assert_eq!(next_epoch([now - 5, now + 200, now + 100]), now + 201);
        assert_eq!(checkpoint_key("p", 7, SPEC_OBJECT), "snapshots/p/7/pod.pb");
        assert_eq!(
            checkpoint_key(&volume_disk("v"), 7, VOLUME_OBJECT),
            "snapshots/volume-v/7/volume.pb"
        );
    }

    #[test]
    fn a_microvm_with_other_volumes_is_stale() {
        let disk = |path: &str| feos_proto::microvm::DataDiskConfig {
            path: path.to_string(),
        };
        let wanted = VmConfig {
            data_disk: Some(disk("/d")),
            volumes: vec![disk("/a")],
            ..Default::default()
        };
        let mut vm = VmInfo {
            config: Some(wanted.clone()),
            ..Default::default()
        };
        assert!(!stale(&vm, &wanted));
        // One made before the pod had volumes, or with others.
        vm.config.as_mut().unwrap().volumes.clear();
        assert!(stale(&vm, &wanted));
    }

    #[test]
    fn a_microvm_without_its_metadata_is_stale() {
        let wanted = VmConfig {
            metadata_json: r#"{"feos":{}}"#.to_string(),
            ..Default::default()
        };
        let mut vm = VmInfo {
            config: Some(wanted.clone()),
            ..Default::default()
        };
        assert!(!stale(&vm, &wanted));
        // One made before pods had a metadata service.
        vm.config.as_mut().unwrap().metadata_json.clear();
        assert!(stale(&vm, &wanted));
    }
}
