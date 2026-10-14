// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods, on top of the VM service (ADR 0004).
//!
//! A pod comes up in steps: its microVM is created and booted, its guest
//! attaches, and its containers are created and started in the guest, one
//! after the other. Each step looks at what is there first, so that a
//! bring-up that was cut off, by a restart of feosd or a stop, is taken up
//! again where it ended. What changes a pod runs under its lock; a stop or
//! a delete ends a bring-up first.

use crate::spec;
use crate::store::Store;
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, ContainerConfig,
    ContainerState as GuestContainerState, CreateContainerRequest, GetContainerRequest,
    ListContainersRequest, StartContainerRequest,
};
use feos_proto::pod_service::{
    ContainerSpec, ContainerState, ContainerStatus, MicroVm, PodConsoleInput, PodConsoleOutput,
    PodEvent, PodInfo, PodSpec, PodState,
};
use feos_proto::vm_service::{
    stream_vm_console_request as console_request, vm_service_server::VmService,
    AttachConsoleMessage, ConsoleData, CreateVmRequest, DeleteVmRequest, GetVmRequest,
    PingVmRequest, ResumeVmRequest, ShutdownVmRequest, StartVmRequest, StreamVmConsoleRequest,
    StreamVmEventsRequest, SuspendVmRequest, VmEvent, VmInfo, VmState, VmStateChangedEvent,
};
use feos_utils::vsock::{self, API_PORT};
use log::{info, warn};
use prost::Message;
use std::collections::HashMap;
use std::io;
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;
use tokio::sync::{broadcast, OwnedMutexGuard};
use tokio::task::AbortHandle;
use tokio::time::{sleep, timeout, Instant};
use tokio_stream::{Stream, StreamExt};
use tonic::transport::Channel;
use tonic::{Code, Request, Status};
use vm_service::api::VmApiHandler;
use vm_service::vsock_path;

/// How long a booting guest may take to attach.
const GUEST_TIMEOUT: Duration = Duration::from_secs(60);
/// How often a step looks again while it waits.
const POLL: Duration = Duration::from_millis(250);
/// How long the VM service may take to record a change it made.
const SETTLE_TIMEOUT: Duration = Duration::from_secs(10);
/// How long GetPod waits for the guest's view of the containers.
const STATUS_TIMEOUT: Duration = Duration::from_secs(3);

#[derive(Clone)]
pub struct Pods {
    inner: Arc<Inner>,
}

struct Inner {
    vms: VmApiHandler,
    store: Store,
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
    /// The pods kept in `dir`. Those that were coming up come up now; the
    /// others follow their microVMs from here on.
    pub fn new(vms: VmApiHandler, dir: &Path) -> io::Result<Self> {
        let store = Store::open(dir)?;
        let kept = store.load()?;
        let (events, _) = broadcast::channel(256);
        let pods = Self {
            inner: Arc::new(Inner {
                vms,
                store,
                pods: Mutex::new(HashMap::new()),
                events,
            }),
        };
        let mut pending = Vec::new();
        for info in kept {
            if info.state() == PodState::Pending {
                pending.push(info.pod_id.clone());
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
        for pod_id in pending {
            pods.spawn_bring_up(&pod_id);
        }
        pods.follow_vms();
        Ok(pods)
    }

    pub async fn create(&self, pod_id: String, spec: PodSpec) -> Result<PodInfo, Status> {
        spec::check_id(&pod_id)?;
        let spec = spec::normalize(spec)?;
        let info = {
            let mut pods = self.pods();
            if let Some(pod) = pods.get(&pod_id) {
                return if pod.info.spec.as_ref() == Some(&spec) {
                    Ok(pod.info.clone())
                } else {
                    Err(Status::already_exists(format!(
                        "pod {pod_id} exists with another spec"
                    )))
                };
            }
            let info = PodInfo {
                pod_id: pod_id.clone(),
                spec: Some(spec),
                state: PodState::Pending as i32,
                ..Default::default()
            };
            self.inner
                .store
                .save(&info)
                .map_err(|e| Status::internal(format!("keep pod {pod_id}: {e}")))?;
            pods.insert(
                pod_id.clone(),
                Pod {
                    info: info.clone(),
                    lock: Arc::default(),
                    bring_up: None,
                },
            );
            info
        };
        info!("Pods: {pod_id} created");
        self.announce(&info, false);
        self.spawn_bring_up(&pod_id);
        Ok(info)
    }

    /// Brings a stopped or failed pod up again.
    pub async fn start(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.lock(pod_id).await?;
        match self.info(pod_id)?.state() {
            PodState::Pending | PodState::Running => return Ok(()),
            PodState::Suspended => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is suspended: resume it"
                )));
            }
            _ => {}
        }
        self.set_state(pod_id, PodState::Pending, "");
        drop(_lock);
        self.spawn_bring_up(pod_id);
        Ok(())
    }

    /// Ends the pod's microVM. Whatever state it is in, the next start
    /// boots it anew.
    pub async fn stop(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.end_bring_up_and_lock(pod_id).await?;
        if self.info(pod_id)?.state() == PodState::Stopped {
            return Ok(());
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
        self.set_state(pod_id, PodState::Stopped, "stopped");
        Ok(())
    }

    pub async fn delete(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.end_bring_up_and_lock(pod_id).await?;
        self.delete_vm(pod_id).await?;
        self.inner
            .store
            .remove(pod_id)
            .map_err(|e| Status::internal(format!("forget pod {pod_id}: {e}")))?;
        if let Some(pod) = self.pods().remove(pod_id) {
            self.announce(&pod.info, true);
        }
        info!("Pods: {pod_id} deleted");
        Ok(())
    }

    /// Writes the running pod to a snapshot; returns the size of its memory.
    pub async fn suspend(&self, pod_id: &str) -> Result<u64, Status> {
        let _lock = self.lock(pod_id).await?;
        self.expect_state(pod_id, PodState::Running)?;
        let request = Request::new(SuspendVmRequest {
            vm_id: pod_id.to_string(),
            purge_guest: true,
        });
        let response = self.inner.vms.suspend_vm(request).await?.into_inner();
        self.settle(pod_id, VmState::Suspended).await?;
        self.set_state(pod_id, PodState::Suspended, "suspended");
        Ok(response.memory_bytes)
    }

    pub async fn resume(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.lock(pod_id).await?;
        self.expect_state(pod_id, PodState::Suspended)?;
        let request = Request::new(ResumeVmRequest {
            vm_id: pod_id.to_string(),
        });
        self.inner.vms.resume_vm(request).await?;
        self.settle(pod_id, VmState::Running).await?;
        self.set_state(pod_id, PodState::Running, "");
        Ok(())
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
        self.boot(pod_id, spec).await?;
        self.wait_for_guest(pod_id).await?;
        let mut guest = ContainerServiceClient::new(guest_channel(pod_id).await?);
        for container in &spec.containers {
            run_container(&mut guest, container).await.map_err(|e| {
                Status::new(
                    e.code(),
                    format!("container {}: {}", container.name, e.message()),
                )
            })?;
        }
        Ok(())
    }

    /// Gets the pod's microVM running, from whatever state it is in.
    async fn boot(&self, pod_id: &str, spec: &PodSpec) -> Result<(), Status> {
        let vm_id = pod_id.to_string();
        loop {
            match self.vm(pod_id).await?.map(|vm| vm.state()) {
                None => {
                    let request = Request::new(CreateVmRequest {
                        config: Some(spec::vm_config(pod_id, spec)),
                        vm_id: Some(vm_id.clone()),
                    });
                    self.inner.vms.create_vm(request).await?;
                }
                Some(VmState::Creating) => sleep(POLL).await,
                Some(VmState::Created | VmState::Stopped) => {
                    let request = Request::new(StartVmRequest { vm_id });
                    self.inner.vms.start_vm(request).await?;
                    return Ok(());
                }
                Some(VmState::Running) => return Ok(()),
                Some(VmState::Paused) => {
                    let request = Request::new(ResumeVmRequest { vm_id });
                    self.inner.vms.resume_vm(request).await?;
                    return Ok(());
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
        if !data.type_url.ends_with("feos.vm.v1.VmStateChangedEvent") {
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
        }
    }
}

/// A channel to the API of the pod's guest.
pub(crate) async fn guest_channel(pod_id: &str) -> Result<Channel, Status> {
    vsock::channel(vsock_path(pod_id), API_PORT)
        .await
        .map_err(|e| Status::unavailable(format!("the guest of pod {pod_id}: {e}")))
}

/// Gets a container running in the guest, from whatever is there.
async fn run_container(
    guest: &mut ContainerServiceClient<Channel>,
    container: &ContainerSpec,
) -> Result<(), Status> {
    let container_id = container.name.clone();
    loop {
        let request = GetContainerRequest {
            container_id: container_id.clone(),
        };
        match guest.get_container(request).await {
            Err(e) if e.code() == Code::NotFound => {
                let request = CreateContainerRequest {
                    config: Some(ContainerConfig {
                        image_ref: container.image_ref.clone(),
                        command: container.command.clone(),
                        env: container.env.clone(),
                    }),
                    container_id: Some(container_id.clone()),
                    adopt: false,
                };
                guest.create_container(request).await?;
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
                GuestContainerState::Running | GuestContainerState::Stopped => return Ok(()),
                GuestContainerState::Unspecified => {
                    return Err(Status::internal("the guest reports no state"));
                }
            },
        }
    }
}
