// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{error::VmServiceError, guest::Guests, vmm::Hypervisor, VmEventWrapper};
use feos_proto::microvm::{
    stream_vm_console_request as console_input, ConsoleData, CreateVmRequest, CreateVmResponse,
    DeleteVmRequest, DeleteVmResponse, PingVmRequest, PingVmResponse, ResumeVmRequest,
    ResumeVmResponse, ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, StartVmResponse,
    StreamVmConsoleResponse, StreamVmEventsRequest, SuspendVmRequest, SuspendVmResponse, VmEvent,
    VmState, VmStateChangedEvent,
};
use feos_utils::pod_id::PodId;
use log::{error, info, warn};
use std::path::PathBuf;
use std::sync::Arc;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt, DuplexStream},
    sync::{broadcast, mpsc, oneshot},
};
use tokio_stream::StreamExt;
use tonic::Status;

/// The newest installed guest: the microVM links its files and keeps that
/// version.
fn guest_dir(vm_id: &str) -> Result<PathBuf, VmServiceError> {
    let guest = crate::vmm::installed::newest_guest().ok_or_else(|| {
        VmServiceError::InvalidArgument(format!(
            "no guest is installed in {}",
            crate::vmm::installed::GUEST_DIR
        ))
    })?;
    info!(
        "VmWorker ({vm_id}): Booting the installed guest {}",
        guest.display()
    );
    Ok(guest)
}

pub async fn handle_create_vm(
    vm_id: String,
    req: CreateVmRequest,
    responder: oneshot::Sender<Result<CreateVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    broadcast_tx: mpsc::Sender<VmEventWrapper>,
    guests: Arc<Guests>,
) {
    if responder
        .send(Ok(CreateVmResponse {
            vm_id: vm_id.clone(),
        }))
        .is_err()
    {
        error!("VmWorker ({vm_id}): Client disconnected before immediate response could be sent. Aborting creation.");
        return;
    }

    info!("VmWorker ({vm_id}): Starting creation process.");
    crate::vmm::broadcast_state_change_event(
        &broadcast_tx,
        &vm_id,
        "microvm-service",
        VmStateChangedEvent {
            new_state: VmState::Creating as i32,
            reason: "VM creation process started".to_string(),
        },
        None,
    )
    .await;

    let image_dir = match guest_dir(&vm_id) {
        Ok(dir) => dir,
        Err(e) => {
            let error_msg = e.to_string();
            error!("VmWorker ({vm_id}): {error_msg}");
            crate::vmm::broadcast_state_change_event(
                &broadcast_tx,
                &vm_id,
                "microvm-service",
                VmStateChangedEvent {
                    new_state: VmState::Crashed as i32,
                    reason: error_msg,
                },
                None,
            )
            .await;
            return;
        }
    };

    let guest_network = req
        .config
        .as_ref()
        .and_then(|config| config.guest_network.clone());
    let result = hypervisor.create_vm(&vm_id, req, image_dir).await;

    match result {
        Ok(pid) => {
            info!("VmWorker ({vm_id}): Background creation process completed successfully.");
            if let Err(e) = guests.listen(&vm_id, guest_network) {
                warn!("VmWorker ({vm_id}): Cannot listen for the guest's uplink: {e}");
            }
            crate::vmm::broadcast_state_change_event(
                &broadcast_tx,
                &vm_id,
                "microvm-service",
                VmStateChangedEvent {
                    new_state: VmState::Created as i32,
                    reason: "Hypervisor process started and VM configured".to_string(),
                },
                pid,
            )
            .await;
        }
        Err(e) => {
            let error_msg = e.to_string();
            error!("VmWorker ({vm_id}): Background creation process failed: {error_msg}");
            crate::vmm::broadcast_state_change_event(
                &broadcast_tx,
                &vm_id,
                "microvm-service",
                VmStateChangedEvent {
                    new_state: VmState::Crashed as i32,
                    reason: error_msg,
                },
                None,
            )
            .await;
        }
    }
}

pub fn start_healthcheck_monitor(
    vm_id: String,
    hypervisor: Arc<dyn Hypervisor>,
    broadcast_tx: mpsc::Sender<VmEventWrapper>,
    cancel_bus: broadcast::Receiver<PodId>,
) {
    let health_hypervisor = hypervisor;
    let health_broadcast_tx = broadcast_tx;
    tokio::spawn(async move {
        health_hypervisor
            .healthcheck_vm(vm_id, health_broadcast_tx, cancel_bus)
            .await;
    });
}

pub async fn handle_start_vm(
    req: StartVmRequest,
    responder: oneshot::Sender<Result<StartVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    broadcast_tx: mpsc::Sender<VmEventWrapper>,
    cancel_bus: Option<broadcast::Receiver<PodId>>,
    guests: Arc<Guests>,
) {
    let vm_id = req.vm_id.clone();
    guests.reset(&vm_id);
    let result = hypervisor.start_vm(req).await;

    if let Ok(pid) = result {
        crate::vmm::broadcast_state_change_event(
            &broadcast_tx,
            &vm_id,
            "microvm-service",
            VmStateChangedEvent {
                new_state: VmState::Running as i32,
                reason: "Start command successful".to_string(),
            },
            Some(pid),
        )
        .await;

        if let Some(cancel_bus) = cancel_bus {
            start_healthcheck_monitor(vm_id, hypervisor, broadcast_tx, cancel_bus);
        }
    }

    let result = result.map(|_pid| StartVmResponse {});
    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmWorker: Failed to send response for StartVm.");
    }
}

pub async fn handle_stream_vm_events(
    req: StreamVmEventsRequest,
    stream_tx: mpsc::Sender<Result<VmEvent, Status>>,
    broadcast_tx: broadcast::Sender<VmEventWrapper>,
) {
    let mut broadcast_rx = broadcast_tx.subscribe();
    let vm_id_to_watch = req.vm_id;

    let watcher_desc = vm_id_to_watch
        .clone()
        .unwrap_or_else(|| "all VMs".to_string());

    loop {
        match broadcast_rx.recv().await {
            Ok(VmEventWrapper { event, .. }) => {
                if vm_id_to_watch.as_ref().is_none_or(|id| event.vm_id == *id)
                    && stream_tx.send(Ok(event)).await.is_err()
                {
                    info!("VmWorker (Stream): Client for '{watcher_desc}' disconnected.");
                    break;
                }
            }
            Err(broadcast::error::RecvError::Lagged(n)) => {
                warn!(
                    "VmWorker (Stream): Event stream for '{watcher_desc}' lagged by {n} messages."
                );
            }
            Err(broadcast::error::RecvError::Closed) => {
                info!(
                    "VmWorker (Stream): Broadcast channel closed. Shutting down stream for '{watcher_desc}'."
                );
                break;
            }
        }
    }
}

pub async fn handle_delete_vm(
    req: DeleteVmRequest,
    process_id: Option<i64>,
    responder: oneshot::Sender<Result<DeleteVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    _broadcast_tx: mpsc::Sender<VmEventWrapper>,
    guests: Arc<Guests>,
) {
    let vm_id = req.vm_id.clone();
    guests.forget(&vm_id);
    let result = hypervisor.delete_vm(req, process_id).await;

    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmWorker: Failed to send response for DeleteVm.");
    }
}

pub async fn spawn_console_bridge(
    vm_id: String,
    input_stream: crate::ConsoleInput,
    output_tx: mpsc::Sender<Result<StreamVmConsoleResponse, Status>>,
    hypervisor: Arc<dyn Hypervisor>,
) {
    let console = match hypervisor.open_console(&vm_id).await {
        Ok(console) => console,
        Err(e) => {
            let _ = output_tx.send(Err(e.into())).await;
            return;
        }
    };

    bridge_console_streams(vm_id, console, input_stream, output_tx).await;
}

pub async fn handle_ping_vm(
    req: PingVmRequest,
    responder: oneshot::Sender<Result<PingVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
) {
    let result = hypervisor.ping_vm(req).await;
    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmWorker: Failed to send response for PingVm.");
    }
}

pub async fn handle_shutdown_vm(
    req: ShutdownVmRequest,
    responder: oneshot::Sender<Result<ShutdownVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    broadcast_tx: mpsc::Sender<VmEventWrapper>,
) {
    let vm_id = req.vm_id.clone();
    let result = hypervisor.shutdown_vm(req).await;

    if result.is_ok() {
        crate::vmm::broadcast_state_change_event(
            &broadcast_tx,
            &vm_id,
            "microvm-service",
            VmStateChangedEvent {
                new_state: VmState::Stopped as i32,
                reason: "Shutdown command successful".to_string(),
            },
            None,
        )
        .await;
    }

    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmWorker: Failed to send response for ShutdownVm.");
    }
}

pub async fn handle_suspend_vm(
    req: SuspendVmRequest,
    responder: oneshot::Sender<Result<SuspendVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    broadcast_tx: mpsc::Sender<VmEventWrapper>,
    guests: Arc<Guests>,
    free_page_reporting: bool,
) {
    let vm_id = req.vm_id.clone();
    if req.purge_guest {
        guests.prepare_pause(&vm_id, free_page_reporting).await;
    }
    let result = hypervisor
        .suspend_vm(&vm_id, req.purge_guest && free_page_reporting)
        .await;

    if let Ok(memory_bytes) = result {
        crate::vmm::broadcast_state_change_event(
            &broadcast_tx,
            &vm_id,
            "microvm-service",
            VmStateChangedEvent {
                new_state: VmState::Suspended as i32,
                reason: format!(
                    "Suspended to a snapshot with {} MiB of memory",
                    memory_bytes >> 20
                ),
            },
            None,
        )
        .await;
    }

    let result = result.map(|memory_bytes| SuspendVmResponse { memory_bytes });
    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmWorker: Failed to send response for SuspendVm.");
    }
}

pub async fn handle_restore_vm(
    req: ResumeVmRequest,
    responder: oneshot::Sender<Result<ResumeVmResponse, VmServiceError>>,
    hypervisor: Arc<dyn Hypervisor>,
    broadcast_tx: mpsc::Sender<VmEventWrapper>,
    cancel_bus: broadcast::Receiver<PodId>,
    guests: Arc<Guests>,
) {
    let vm_id = req.vm_id.clone();
    let remote = req.remote.map(|remote| crate::vmm::snapshot::Remote {
        vmstate: PathBuf::from(remote.vmstate_path),
        host: crate::vmm::snapshot::Host {
            firecracker_version: remote.firecracker_version,
            cpu_model: remote.cpu_model,
            kernel_release: remote.kernel_release,
        },
    });
    let from_elsewhere = remote.is_some();
    let result = hypervisor.restore_vm(&vm_id, remote).await;

    if let Ok(pid) = result {
        crate::vmm::broadcast_state_change_event(
            &broadcast_tx,
            &vm_id,
            "microvm-service",
            VmStateChangedEvent {
                new_state: VmState::Running as i32,
                reason: "Restored from its snapshot".to_string(),
            },
            Some(pid),
        )
        .await;
        start_healthcheck_monitor(vm_id.clone(), hypervisor, broadcast_tx, cancel_bus);
        // The guest's clock stood still in the snapshot, and one from
        // elsewhere had the network of another node.
        guests.resumed(&vm_id, from_elsewhere).await;
    }

    let result = result.map(|_pid| ResumeVmResponse {});
    if responder.send(result.map_err(Into::into)).is_err() {
        error!("VmWorker: Failed to send response for ResumeVm.");
    }
}

async fn bridge_console_streams(
    vm_id: String,
    console: DuplexStream,
    mut grpc_input: crate::ConsoleInput,
    grpc_output: mpsc::Sender<Result<StreamVmConsoleResponse, Status>>,
) {
    let (mut socket_reader, mut socket_writer) = tokio::io::split(console);
    let grpc_output_clone = grpc_output.clone();
    let read_task_vm_id = vm_id.clone();

    let read_task = tokio::spawn(async move {
        let mut buf = vec![0; 4096];
        loop {
            tokio::select! {
                biased;
                _ = grpc_output_clone.closed() => {
                    info!("VmmHelper (Console {read_task_vm_id}): gRPC client disconnected, terminating read task.");
                    break;
                }
                read_result = socket_reader.read(&mut buf) => {
                    match read_result {
                        Ok(0) => {
                            info!("VmmHelper (Console {read_task_vm_id}): Console socket closed (EOF).");
                            break;
                        }
                        Ok(n) => {
                            let output_msg = StreamVmConsoleResponse { output: buf[..n].to_vec() };
                            if grpc_output_clone.send(Ok(output_msg)).await.is_err() {
                            }
                        }
                        Err(e) => {
                            let err_msg = format!("Error reading from console socket: {e}");
                            let _ = grpc_output_clone.send(Err(Status::internal(err_msg))).await;
                            break;
                        }
                    }
                }
            }
        }
    });

    let write_task_vm_id = vm_id.clone();
    let write_task = tokio::spawn(async move {
        while let Some(result) = grpc_input.next().await {
            match result {
                Ok(msg) => match msg.payload {
                    Some(console_input::Payload::Data(ConsoleData { input })) => {
                        if let Err(e) = socket_writer.write_all(&input).await {
                            warn!("VmmHelper (Console {write_task_vm_id}): Failed to write to socket: {e}. VM may have shut down.");
                            break;
                        }
                    }
                    Some(console_input::Payload::Attach(_)) => {
                        let _ = grpc_output
                            .send(Err(Status::invalid_argument(
                                "Cannot send Attach message more than once.",
                            )))
                            .await;
                        break;
                    }
                    None => {
                        let _ = grpc_output
                            .send(Err(Status::invalid_argument("Empty ConsoleInput payload.")))
                            .await;
                        break;
                    }
                },
                Err(e) => {
                    warn!(
                        "VmmHelper (Console {write_task_vm_id}): Error reading from gRPC client stream: {e}"
                    );
                    break;
                }
            }
        }
    });

    tokio::select! {
        _ = read_task => {},
        _ = write_task => {},
    }
}
