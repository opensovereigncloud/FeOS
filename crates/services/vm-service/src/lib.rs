// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::error::VmServiceError;
use feos_proto::vm_service::{
    CreateVmRequest, CreateVmResponse, DeleteVmRequest, DeleteVmResponse, GetBalloonRequest,
    GetBalloonResponse, GetVmRequest, ListVmsRequest, ListVmsResponse, PauseVmRequest,
    PauseVmResponse, PingVmRequest, PingVmResponse, ResumeVmRequest, ResumeVmResponse,
    ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, StartVmResponse, StreamVmConsoleRequest,
    StreamVmConsoleResponse, StreamVmEventsRequest, SuspendVmRequest, SuspendVmResponse,
    UpdateBalloonRequest, UpdateBalloonResponse, VmEvent, VmInfo,
};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use tokio::sync::{mpsc, oneshot};
use tokio_stream::Stream;
use tonic::Status;

pub mod api;
pub mod dispatcher;
pub mod dispatcher_handlers;
pub mod error;
pub mod guest;
pub mod persistence;
pub mod vmm;
pub mod worker;

pub const DEFAULT_VM_DB_URL: &str = "sqlite:/var/lib/feos/vms.db";
/// The jailer's chroot base: a jail per VM in `firecracker/<id>/`.
pub const VM_DIR: &str = "/var/lib/feos/vms";
pub const FIRECRACKER_BIN: &str = "firecracker";
pub const JAILER_BIN: &str = "jailer";
pub const IMAGE_DIR: &str = "/var/lib/feos/images";

/// The VM's jail: its chroot and what the VM service keeps outside of it.
pub fn jail_dir(vm_id: &str) -> PathBuf {
    Path::new(VM_DIR).join(FIRECRACKER_BIN).join(vm_id)
}

/// The VM's directory, the chroot its Firecracker runs in: its
/// configuration, its sockets, its logs and its snapshot.
pub fn vm_dir(vm_id: &str) -> PathBuf {
    jail_dir(vm_id).join("root")
}

/// The host end of the VM's vsock device.
pub fn vsock_path(vm_id: &str) -> PathBuf {
    vm_dir(vm_id).join("v.sock")
}

/// What a console's client sends: an attach message, then its input.
pub type ConsoleInput = Pin<Box<dyn Stream<Item = Result<StreamVmConsoleRequest, Status>> + Send>>;

#[derive(Debug, Clone)]
pub struct VmEventWrapper {
    pub event: VmEvent,
    pub process_id: Option<i64>,
}

pub enum Command {
    CreateVm(
        CreateVmRequest,
        oneshot::Sender<Result<CreateVmResponse, VmServiceError>>,
    ),
    StartVm(
        StartVmRequest,
        oneshot::Sender<Result<StartVmResponse, VmServiceError>>,
    ),
    GetVm(
        GetVmRequest,
        oneshot::Sender<Result<VmInfo, VmServiceError>>,
    ),
    StreamVmEvents(StreamVmEventsRequest, mpsc::Sender<Result<VmEvent, Status>>),
    DeleteVm(
        DeleteVmRequest,
        oneshot::Sender<Result<DeleteVmResponse, VmServiceError>>,
    ),
    StreamVmConsole(
        ConsoleInput,
        mpsc::Sender<Result<StreamVmConsoleResponse, Status>>,
    ),
    ListVms(
        ListVmsRequest,
        oneshot::Sender<Result<ListVmsResponse, VmServiceError>>,
    ),
    PingVm(
        PingVmRequest,
        oneshot::Sender<Result<PingVmResponse, VmServiceError>>,
    ),
    ShutdownVm(
        ShutdownVmRequest,
        oneshot::Sender<Result<ShutdownVmResponse, VmServiceError>>,
    ),
    PauseVm(
        PauseVmRequest,
        oneshot::Sender<Result<PauseVmResponse, VmServiceError>>,
    ),
    ResumeVm(
        ResumeVmRequest,
        oneshot::Sender<Result<ResumeVmResponse, VmServiceError>>,
    ),
    SuspendVm(
        SuspendVmRequest,
        oneshot::Sender<Result<SuspendVmResponse, VmServiceError>>,
    ),
    UpdateBalloon(
        UpdateBalloonRequest,
        oneshot::Sender<Result<UpdateBalloonResponse, VmServiceError>>,
    ),
    GetBalloon(
        GetBalloonRequest,
        oneshot::Sender<Result<GetBalloonResponse, VmServiceError>>,
    ),
}

impl std::fmt::Debug for Command {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Command::CreateVm(req, _) => f.debug_tuple("CreateVm").field(req).finish(),
            Command::StartVm(req, _) => f.debug_tuple("StartVm").field(req).finish(),
            Command::GetVm(req, _) => f.debug_tuple("GetVm").field(req).finish(),
            Command::StreamVmEvents(req, _) => f.debug_tuple("StreamVmEvents").field(req).finish(),
            Command::DeleteVm(req, _) => f.debug_tuple("DeleteVm").field(req).finish(),
            Command::StreamVmConsole(_, _) => {
                f.write_str("StreamVmConsole(<gRPC Stream>, <mpsc::Sender>)")
            }
            Command::ListVms(req, _) => f.debug_tuple("ListVms").field(req).finish(),
            Command::PingVm(req, _) => f.debug_tuple("PingVm").field(req).finish(),
            Command::ShutdownVm(req, _) => f.debug_tuple("ShutdownVm").field(req).finish(),
            Command::PauseVm(req, _) => f.debug_tuple("PauseVm").field(req).finish(),
            Command::ResumeVm(req, _) => f.debug_tuple("ResumeVm").field(req).finish(),
            Command::SuspendVm(req, _) => f.debug_tuple("SuspendVm").field(req).finish(),
            Command::UpdateBalloon(req, _) => f.debug_tuple("UpdateBalloon").field(req).finish(),
            Command::GetBalloon(req, _) => f.debug_tuple("GetBalloon").field(req).finish(),
        }
    }
}
