// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{
    error::VmServiceError,
    guest::Guests,
    persistence::{repository::VmRepository, VmRecord, VmStatus},
    vmm::Hypervisor,
    worker, VmEventWrapper,
};
use feos_proto::{
    image_service::{image_service_client::ImageServiceClient, PullImageRequest},
    vm_service::{
        stream_vm_console_request as console_input, AttachConsoleMessage, CreateVmRequest,
        CreateVmResponse, DeleteVmRequest, DeleteVmResponse, GetVmRequest, ListVmsRequest,
        ListVmsResponse, PauseVmRequest, PauseVmResponse, ResumeVmRequest, ResumeVmResponse,
        ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, StartVmResponse,
        StreamVmConsoleRequest, StreamVmConsoleResponse, StreamVmEventsRequest, VmEvent, VmInfo,
        VmState, VmStateChangedEvent,
    },
};
use hyper_util::rt::TokioIo;
use image_service::IMAGE_SERVICE_SOCKET;
use log::{error, info, warn};
use prost::Message;
use prost_types::Any;
use std::{path::PathBuf, sync::Arc};
use tokio::sync::{broadcast, mpsc, oneshot};
use tokio_stream::StreamExt;
use tonic::{
    transport::{Channel, Endpoint, Error as TonicTransportError, Uri},
    Status, Streaming,
};
use tower::service_fn;
use uuid::Uuid;

fn ensure_net_config_device_id(net_config: &mut feos_proto::vm_service::NetConfig) {
    if net_config.device_id.is_empty()
        && let Some(tap) = &net_config.tap
    {
        net_config.device_id = tap.tap_name.clone();
    }
}

fn vm_info(record: VmRecord, guests: &Guests) -> VmInfo {
    let vm_id = record.vm_id.to_string();
    VmInfo {
        vsock_path: crate::vsock_path(&vm_id).to_string_lossy().into_owned(),
        guest: guests.info(&vm_id),
        vm_id,
        state: record.status.state as i32,
        config: Some(record.config),
    }
}

pub(crate) async fn get_image_service_client(
) -> Result<ImageServiceClient<Channel>, TonicTransportError> {
    let socket_path = PathBuf::from(IMAGE_SERVICE_SOCKET);
    Endpoint::try_from("http://[::1]:50051")
        .unwrap()
        .connect_with_connector(service_fn(move |_: Uri| {
            let socket_path = socket_path.clone();
            async move {
                tokio::net::UnixStream::connect(socket_path)
                    .await
                    .map(TokioIo::new)
            }
        }))
        .await
        .map(ImageServiceClient::new)
}

async fn initiate_image_pull_for_vm(req: &CreateVmRequest) -> Result<String, VmServiceError> {
    let image_ref = match req.config.as_ref() {
        Some(config) if !config.image_ref.is_empty() => config.image_ref.clone(),
        _ => {
            return Err(VmServiceError::InvalidArgument(
                "VmConfig with a non-empty image_ref is required".to_string(),
            ));
        }
    };

    info!("VmDispatcher: Requesting image pull for {image_ref}");
    let mut client = get_image_service_client()
        .await
        .map_err(|e| VmServiceError::ImageService(format!("Could not connect: {e}")))?;

    let response = client
        .pull_image(PullImageRequest {
            image_ref: image_ref.clone(),
            // The VM writes to the image's disk.
            exclusive: true,
        })
        .await
        .map_err(|status| {
            VmServiceError::ImageService(format!("PullImage RPC failed for {image_ref}: {status}"))
        })?;

    let image_uuid = response.into_inner().image_uuid;
    info!("VmDispatcher: Image pull for {image_ref} initiated. UUID: {image_uuid}");
    Ok(image_uuid)
}

async fn prepare_vm_creation(
    repository: &VmRepository,
    req: &CreateVmRequest,
) -> Result<(Uuid, String), VmServiceError> {
    let vm_id_res: Result<(Uuid, bool), VmServiceError> =
        if let Some(id_str) = req.vm_id.as_deref().filter(|s| !s.is_empty()) {
            match Uuid::parse_str(id_str) {
                Ok(id) if !id.is_nil() => Ok((id, true)),
                Ok(_) => Err(VmServiceError::InvalidArgument(
                    "Provided vm_id cannot be the nil UUID.".to_string(),
                )),
                Err(_) => Err(VmServiceError::InvalidArgument(
                    "Provided vm_id is not a valid UUID format.".to_string(),
                )),
            }
        } else {
            Ok((Uuid::new_v4(), false))
        };

    let (vm_id, is_user_provided) = vm_id_res?;

    if is_user_provided && repository.get_vm(vm_id).await?.is_some() {
        return Err(VmServiceError::AlreadyExists(format!(
            "VM with ID {vm_id} already exists."
        )));
    }

    if let Some(config) = &req.config {
        crate::vmm::validate_config(config)
            .map_err(|e| VmServiceError::InvalidArgument(e.to_string()))?;
    }

    let image_uuid_str = initiate_image_pull_for_vm(req).await?;
    let image_uuid = Uuid::parse_str(&image_uuid_str)
        .map_err(|e| VmServiceError::ImageService(format!("Failed to parse image UUID: {e}")))?;

    let mut vm_config = req.config.clone().ok_or(VmServiceError::InvalidArgument(
        "VmConfig is required in CreateVmRequest".to_string(),
    ))?;

    vm_config
        .net
        .iter_mut()
        .for_each(ensure_net_config_device_id);
    for (index, disk) in vm_config.disks.iter_mut().enumerate() {
        if disk.device_id.is_empty() {
            disk.device_id = format!("disk{index}");
        }
    }

    let record = VmRecord {
        vm_id,
        image_uuid,
        status: VmStatus {
            state: VmState::Creating,
            last_msg: "VM creation initiated".to_string(),
            process_id: None,
        },
        config: vm_config,
    };

    repository.save_vm(&record).await?;
    info!("VmDispatcher: Saved initial record for VM {vm_id}");
    Ok((vm_id, image_uuid_str))
}

async fn get_vm_info(
    repository: &VmRepository,
    req: &GetVmRequest,
    guests: &Guests,
) -> Result<VmInfo, VmServiceError> {
    let vm_id = Uuid::parse_str(&req.vm_id)
        .map_err(|_| VmServiceError::InvalidArgument("Invalid VM ID format.".to_string()))?;

    match repository.get_vm(vm_id).await? {
        Some(record) => Ok(vm_info(record, guests)),
        None => Err(VmServiceError::Vmm(crate::vmm::VmmError::VmNotFound(
            vm_id.to_string(),
        ))),
    }
}

async fn parse_vm_id_and_get_record(
    vm_id_str: &str,
    repository: &VmRepository,
) -> Result<(Uuid, VmRecord), VmServiceError> {
    let vm_id = Uuid::parse_str(vm_id_str)
        .map_err(|_| VmServiceError::InvalidArgument("Invalid VM ID format.".to_string()))?;

    match repository.get_vm(vm_id).await? {
        Some(record) => Ok((vm_id, record)),
        None => Err(VmServiceError::Vmm(crate::vmm::VmmError::VmNotFound(
            vm_id.to_string(),
        ))),
    }
}

pub(crate) async fn handle_create_vm_command(
    repository: &VmRepository,
    req: CreateVmRequest,
    responder: oneshot::Sender<Result<CreateVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
    guests: Arc<Guests>,
) {
    let result = prepare_vm_creation(repository, &req).await;

    match result {
        Ok((vm_id, image_uuid_str)) => {
            tokio::spawn(worker::handle_create_vm(
                vm_id.to_string(),
                req,
                image_uuid_str,
                responder,
                hypervisor,
                event_bus_tx,
                guests,
            ));
        }
        Err(e) => {
            error!("VmDispatcher: Failed to handle CreateVm command: {e}");
            if responder.send(Err(e)).is_err() {
                error!(
                    "VmDispatcher: Failed to send error response for CreateVm. Responder closed."
                );
            }
        }
    }
}

pub(crate) async fn handle_get_vm_command(
    repository: &VmRepository,
    req: GetVmRequest,
    responder: oneshot::Sender<Result<VmInfo, VmServiceError>>,
    guests: &Guests,
) {
    let result = get_vm_info(repository, &req, guests).await;

    if responder.send(result).is_err() {
        error!("VmDispatcher: Failed to send response for GetVm.");
    }
}

pub(crate) async fn handle_stream_vm_events_command(
    repository: &VmRepository,
    req: StreamVmEventsRequest,
    stream_tx: mpsc::Sender<Result<VmEvent, Status>>,
    status_channel_tx: broadcast::Sender<VmEventWrapper>,
) {
    if let Some(vm_id_str) = req.vm_id.clone() {
        let vm_id = match Uuid::parse_str(&vm_id_str) {
            Ok(id) => id,
            Err(_) => {
                let status = Status::invalid_argument("Invalid VM ID format.");
                if stream_tx.send(Err(status)).await.is_err() {
                    warn!("StreamEvents: Client for {vm_id_str} disconnected before error could be sent.");
                }
                return;
            }
        };

        match repository.get_vm(vm_id).await {
            Ok(Some(record)) => {
                info!(
                    "StreamEvents: Sending initial state for VM {vm_id_str}: {:?}",
                    record.status.state
                );
                let state_change_event = VmStateChangedEvent {
                    new_state: record.status.state as i32,
                    reason: record.status.last_msg,
                };
                let initial_event = VmEvent {
                    vm_id: vm_id_str.clone(),
                    id: Uuid::new_v4().to_string(),
                    component_id: "vm-service-db".to_string(),
                    data: Some(Any {
                        type_url: "type.googleapis.com/feos.vm.v1.VmStateChangedEvent".to_string(),
                        value: state_change_event.encode_to_vec(),
                    }),
                };

                if stream_tx.send(Ok(initial_event)).await.is_err() {
                    info!(
                        "StreamEvents: Client for {vm_id_str} disconnected before live events could be streamed."
                    );
                    return;
                }

                tokio::spawn(worker::handle_stream_vm_events(
                    req,
                    stream_tx,
                    status_channel_tx,
                ));
            }
            Ok(None) => {
                warn!("VM with ID {vm_id} not found");
                if stream_tx
                    .send(Err(Status::not_found(format!(
                        "VM with ID {vm_id} not found"
                    ))))
                    .await
                    .is_err()
                {
                    warn!(
                        "StreamEvents: Client for {vm_id_str} disconnected before not-found error could be sent."
                    );
                }
            }
            Err(e) => {
                error!("StreamEvents: Failed to get VM {vm_id_str} from database for event stream: {e}");
                if stream_tx
                    .send(Err(Status::internal(
                        "Failed to retrieve VM information for event stream.",
                    )))
                    .await
                    .is_err()
                {
                    warn!("StreamEvents: Client for {vm_id_str} disconnected before internal-error could be sent.");
                }
            }
        }
    } else {
        info!("StreamEvents: Request to stream events for all VMs received.");
        match repository.list_all_vms().await {
            Ok(records) => {
                info!(
                    "StreamEvents: Found {} existing VMs to send initial state for.",
                    records.len()
                );
                for record in records {
                    let state_change_event = VmStateChangedEvent {
                        new_state: record.status.state as i32,
                        reason: format!("Initial state from DB: {}", record.status.last_msg),
                    };
                    let initial_event = VmEvent {
                        vm_id: record.vm_id.to_string(),
                        id: Uuid::new_v4().to_string(),
                        component_id: "vm-service-db".to_string(),
                        data: Some(Any {
                            type_url: "type.googleapis.com/feos.vm.v1.VmStateChangedEvent"
                                .to_string(),
                            value: state_change_event.encode_to_vec(),
                        }),
                    };

                    if stream_tx.send(Ok(initial_event)).await.is_err() {
                        info!("StreamEvents: Client for all VMs disconnected while sending initial states.");
                        return;
                    }
                }
            }
            Err(e) => {
                error!("StreamEvents: Failed to list all VMs from database for event stream: {e}");
                if stream_tx
                    .send(Err(Status::internal(
                        "Failed to retrieve initial VM list for event stream.",
                    )))
                    .await
                    .is_err()
                {
                    warn!("StreamEvents: Client for all VMs disconnected before internal-error could be sent.");
                }
                return;
            }
        }

        info!("StreamEvents: Initial states sent. Starting live event stream for all VMs.");
        tokio::spawn(worker::handle_stream_vm_events(
            req,
            stream_tx,
            status_channel_tx,
        ));
    }
}

pub(crate) async fn handle_delete_vm_command(
    repository: &VmRepository,
    healthcheck_cancel_bus: &broadcast::Sender<Uuid>,
    req: DeleteVmRequest,
    responder: oneshot::Sender<Result<DeleteVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
    guests: Arc<Guests>,
) {
    let vm_id = match Uuid::parse_str(&req.vm_id) {
        Ok(id) => id,
        Err(_) => {
            let _ = responder.send(Err(VmServiceError::InvalidArgument(
                "Invalid VM ID format.".to_string(),
            )));
            return;
        }
    };

    match repository.get_vm(vm_id).await {
        Ok(Some(record)) => {
            let image_uuid_to_delete = record.image_uuid.to_string();
            let process_id_to_kill = record.status.process_id;

            if let Err(e) = repository.delete_vm(vm_id).await {
                error!("Failed to delete VM {vm_id} from database: {e}");
                let _ = responder.send(Err(e.into()));
                return;
            }
            info!("VmDispatcher: Deleted record for VM {vm_id} from database.");

            if let Err(e) = healthcheck_cancel_bus.send(vm_id) {
                warn!("VmDispatcher: Failed to send healthcheck cancellation for {vm_id}: {e}");
            }

            tokio::spawn(worker::handle_delete_vm(
                req,
                image_uuid_to_delete,
                process_id_to_kill,
                responder,
                hypervisor,
                event_bus_tx,
                guests,
            ));
        }
        Ok(None) => {
            let msg = format!("VM with ID {vm_id} not found in database for deletion");
            warn!("VmDispatcher: {msg}. Still attempting hypervisor cleanup.");

            if let Err(e) = healthcheck_cancel_bus.send(vm_id) {
                warn!("VmDispatcher: Failed to send healthcheck cancellation for {vm_id}: {e}");
            }

            tokio::spawn(worker::handle_delete_vm(
                req,
                String::new(),
                None,
                responder,
                hypervisor,
                event_bus_tx,
                guests,
            ));
        }
        Err(e) => {
            error!("Failed to get VM {vm_id} from database: {e}");
            let _ = responder.send(Err(e.into()));
        }
    }
}

async fn get_attach_message(
    stream: &mut Streaming<StreamVmConsoleRequest>,
) -> Result<String, Status> {
    match stream.next().await {
        Some(Ok(msg)) => match msg.payload {
            Some(console_input::Payload::Attach(AttachConsoleMessage { vm_id })) => Ok(vm_id),
            _ => Err(Status::invalid_argument(
                "First message must be an Attach message.",
            )),
        },
        Some(Err(e)) => Err(e),
        None => Err(Status::invalid_argument(
            "Client disconnected before sending Attach message.",
        )),
    }
}

pub(crate) async fn handle_stream_vm_console_command(
    repository: &VmRepository,
    mut input_stream: Streaming<StreamVmConsoleRequest>,
    output_tx: mpsc::Sender<Result<StreamVmConsoleResponse, Status>>,
    hypervisor: Arc<dyn Hypervisor>,
) {
    let vm_id_str = match get_attach_message(&mut input_stream).await {
        Ok(id) => id,
        Err(status) => {
            let _ = output_tx.send(Err(status)).await;
            return;
        }
    };

    let (_vm_id, record) = match parse_vm_id_and_get_record(&vm_id_str, repository).await {
        Ok(result) => result,
        Err(e) => {
            if output_tx.send(Err(e.into())).await.is_err() {
                warn!(
                    "StreamConsole: Client for {vm_id_str} disconnected before error could be sent."
                );
            }
            return;
        }
    };

    if record.status.state != VmState::Running {
        let status = VmServiceError::InvalidState(format!(
            "Cannot open console for VM in {:?} state. Must be in Running.",
            record.status.state
        ))
        .into();
        if output_tx.send(Err(status)).await.is_err() {
            warn!(
                "StreamConsole: Client for {vm_id_str} disconnected before precondition error could be sent."
            );
        }
        return;
    }

    tokio::spawn(worker::spawn_console_bridge(
        vm_id_str,
        input_stream,
        output_tx,
        hypervisor,
    ));
}

pub(crate) async fn handle_list_vms_command(
    repository: &VmRepository,
    _req: ListVmsRequest,
    responder: oneshot::Sender<Result<ListVmsResponse, VmServiceError>>,
    guests: &Guests,
) {
    let result = repository.list_all_vms().await.map(|records| {
        let vms = records
            .into_iter()
            .map(|record| vm_info(record, guests))
            .collect();
        ListVmsResponse { vms }
    });

    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmDispatcher: Failed to send response for ListVms.");
    }
}

pub(crate) async fn handle_start_vm_command(
    repository: &VmRepository,
    req: StartVmRequest,
    responder: oneshot::Sender<Result<StartVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
    healthcheck_cancel_bus_tx: &broadcast::Sender<Uuid>,
    guests: Arc<Guests>,
) {
    let (_vm_id, record) = match parse_vm_id_and_get_record(&req.vm_id, repository).await {
        Ok(result) => result,
        Err(e) => {
            let _ = responder.send(Err(e));
            return;
        }
    };

    let current_state = record.status.state;
    if !matches!(current_state, VmState::Created | VmState::Stopped) {
        let _ = responder.send(Err(VmServiceError::InvalidState(format!(
            "Cannot start VM in {current_state:?} state. Must be in Created or Stopped."
        ))));
        return;
    }

    // Every boot is a new VMM process to watch.
    let cancel_bus = Some(healthcheck_cancel_bus_tx.subscribe());

    tokio::spawn(worker::handle_start_vm(
        req,
        responder,
        hypervisor,
        event_bus_tx,
        cancel_bus,
        guests,
    ));
}

pub(crate) async fn handle_shutdown_vm_command(
    repository: &VmRepository,
    req: ShutdownVmRequest,
    responder: oneshot::Sender<Result<ShutdownVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
) {
    let (_vm_id, record) = match parse_vm_id_and_get_record(&req.vm_id, repository).await {
        Ok(result) => result,
        Err(e) => {
            let _ = responder.send(Err(e));
            return;
        }
    };

    let current_state = record.status.state;
    if current_state != VmState::Running {
        let _ = responder.send(Err(VmServiceError::InvalidState(format!(
            "Cannot shutdown VM in {current_state:?} state. Must be in Running."
        ))));
        return;
    }

    tokio::spawn(worker::handle_shutdown_vm(
        req,
        responder,
        hypervisor,
        event_bus_tx,
    ));
}

pub(crate) async fn handle_pause_vm_command(
    repository: &VmRepository,
    req: PauseVmRequest,
    responder: oneshot::Sender<Result<PauseVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
) {
    let (_vm_id, record) = match parse_vm_id_and_get_record(&req.vm_id, repository).await {
        Ok(result) => result,
        Err(e) => {
            let _ = responder.send(Err(e));
            return;
        }
    };

    let current_state = record.status.state;
    if current_state != VmState::Running {
        let _ = responder.send(Err(VmServiceError::InvalidState(format!(
            "Cannot pause VM in {current_state:?} state. Must be in Running."
        ))));
        return;
    }

    tokio::spawn(worker::handle_pause_vm(
        req,
        responder,
        hypervisor,
        event_bus_tx,
    ));
}

pub(crate) async fn handle_resume_vm_command(
    repository: &VmRepository,
    req: ResumeVmRequest,
    responder: oneshot::Sender<Result<ResumeVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
    guests: Arc<Guests>,
) {
    let (_vm_id, record) = match parse_vm_id_and_get_record(&req.vm_id, repository).await {
        Ok(result) => result,
        Err(e) => {
            let _ = responder.send(Err(e));
            return;
        }
    };

    let current_state = record.status.state;
    if current_state != VmState::Paused {
        let _ = responder.send(Err(VmServiceError::InvalidState(format!(
            "Cannot resume VM in {current_state:?} state. Must be in Paused."
        ))));
        return;
    }

    tokio::spawn(worker::handle_resume_vm(
        req,
        responder,
        hypervisor,
        event_bus_tx,
        guests,
    ));
}

pub(crate) async fn check_and_cleanup_vms(
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
    healthcheck_cancel_bus: &broadcast::Sender<Uuid>,
    vms: Vec<VmRecord>,
) {
    for vm in vms {
        // Only a running or paused VM has a VMM process.
        if !matches!(vm.status.state, VmState::Running | VmState::Paused) {
            continue;
        }
        let vm_id = vm.vm_id.to_string();
        let alive = vm
            .status
            .process_id
            .is_some_and(|pid| hypervisor.is_vmm_process(&vm_id, pid));

        if alive {
            info!("VmDispatcher (Sanity Check): Found running VM {vm_id} from previous session. Starting health monitor.");
            let cancel_bus = healthcheck_cancel_bus.subscribe();
            worker::start_healthcheck_monitor(
                vm_id,
                hypervisor.clone(),
                event_bus_tx.clone(),
                cancel_bus,
            );
        } else {
            // The host restarted, or the VMM died while nobody watched. The
            // VM keeps its disks and can be started again.
            warn!("VmDispatcher (Sanity Check): VM {vm_id} was {:?}, but its VMM process is gone. Marking it stopped.", vm.status.state);
            crate::vmm::broadcast_state_change_event(
                &event_bus_tx,
                &vm_id,
                "vm-service",
                VmStateChangedEvent {
                    new_state: VmState::Stopped as i32,
                    reason: "The VMM process was gone when the VM service started".to_string(),
                },
                None,
            )
            .await;
        }
    }
}

pub(crate) async fn perform_startup_sanity_check(
    repository: &VmRepository,
    hypervisor: Arc<dyn Hypervisor>,
    event_bus_tx: mpsc::Sender<VmEventWrapper>,
    healthcheck_cancel_bus: &broadcast::Sender<Uuid>,
    guests: &Arc<Guests>,
) {
    info!("VmDispatcher: Running initial sanity check...");
    match repository.list_all_vms().await {
        Ok(vms) => {
            // A guest that runs on attaches again once it is listened for.
            for vm in &vms {
                let vm_id = vm.vm_id.to_string();
                if crate::vm_dir(&vm_id).is_dir()
                    && let Err(e) = guests.listen(&vm_id, vm.config.guest_network.clone())
                {
                    warn!("VmDispatcher (Sanity Check): Cannot listen for the guest of VM {vm_id}: {e}");
                }
            }
            if vms.is_empty() {
                info!("VmDispatcher (Sanity Check): No VMs found in persistence, check complete.");
            } else {
                info!(
                    "VmDispatcher (Sanity Check): Found {} VMs in persistence, checking status...",
                    vms.len()
                );
                check_and_cleanup_vms(hypervisor, event_bus_tx, healthcheck_cancel_bus, vms).await;
                info!("VmDispatcher (Sanity Check): Check complete.");
            }
        }
        Err(e) => {
            error!("VmDispatcher (Sanity Check): Failed to list VMs from repository: {e}. Skipping check.");
        }
    }
}
