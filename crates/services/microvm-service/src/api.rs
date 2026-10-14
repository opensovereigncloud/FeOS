// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{Command, ConsoleInput};
use feos_proto::microvm::{
    micro_vm_service_server::MicroVmService, CreateVmRequest, CreateVmResponse, DeleteVmRequest,
    DeleteVmResponse, GetVmRequest, PingVmRequest, PingVmResponse, ResumeVmRequest,
    ResumeVmResponse, ShutdownVmRequest, ShutdownVmResponse, StartVmRequest, StartVmResponse,
    StreamVmConsoleRequest, StreamVmConsoleResponse, StreamVmEventsRequest, SuspendVmRequest,
    SuspendVmResponse, VmEvent, VmInfo,
};
use feos_utils::dispatch::dispatch_and_wait;
use log::info;
use std::pin::Pin;
use tokio::sync::mpsc;
use tokio_stream::{wrappers::ReceiverStream, Stream};
use tonic::{Request, Response, Status, Streaming};

#[derive(Clone)]
pub struct VmApiHandler {
    dispatcher_tx: mpsc::Sender<Command>,
}

impl VmApiHandler {
    pub fn new(dispatcher_tx: mpsc::Sender<Command>) -> Self {
        Self { dispatcher_tx }
    }

    /// The console of a VM, for a caller in this process.
    pub async fn console(
        &self,
        input: ConsoleInput,
    ) -> Result<ReceiverStream<Result<StreamVmConsoleResponse, Status>>, Status> {
        let (output_tx, output_rx) = mpsc::channel(32);
        self.dispatcher_tx
            .send(Command::StreamVmConsole(input, output_tx))
            .await
            .map_err(|e| Status::internal(format!("Failed to send command to dispatcher: {e}")))?;
        Ok(ReceiverStream::new(output_rx))
    }
}

#[tonic::async_trait]
impl MicroVmService for VmApiHandler {
    type StreamVmEventsStream = Pin<Box<dyn Stream<Item = Result<VmEvent, Status>> + Send>>;
    type StreamVmConsoleStream =
        Pin<Box<dyn Stream<Item = Result<StreamVmConsoleResponse, Status>> + Send>>;

    async fn create_vm(
        &self,
        request: Request<CreateVmRequest>,
    ) -> Result<Response<CreateVmResponse>, Status> {
        info!("VmApi: Received CreateVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::CreateVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn start_vm(
        &self,
        request: Request<StartVmRequest>,
    ) -> Result<Response<StartVmResponse>, Status> {
        info!("VmApi: Received StartVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::StartVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn get_vm(&self, request: Request<GetVmRequest>) -> Result<Response<VmInfo>, Status> {
        info!("VmApi: Received GetVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::GetVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn stream_vm_events(
        &self,
        request: Request<StreamVmEventsRequest>,
    ) -> Result<Response<Self::StreamVmEventsStream>, Status> {
        info!("VmApi: Received StreamVmEvents stream request.");
        let (stream_tx, stream_rx) = mpsc::channel(16);
        let cmd = Command::StreamVmEvents(request.into_inner(), stream_tx);
        self.dispatcher_tx
            .send(cmd)
            .await
            .map_err(|e| Status::internal(format!("Failed to send command to dispatcher: {e}")))?;
        let output_stream = ReceiverStream::new(stream_rx);
        Ok(Response::new(Box::pin(output_stream)))
    }

    async fn delete_vm(
        &self,
        request: Request<DeleteVmRequest>,
    ) -> Result<Response<DeleteVmResponse>, Status> {
        info!("VmApi: Received DeleteVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::DeleteVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn stream_vm_console(
        &self,
        request: Request<Streaming<StreamVmConsoleRequest>>,
    ) -> Result<Response<Self::StreamVmConsoleStream>, Status> {
        info!("VmApi: Received StreamVmConsole stream request.");
        let output_stream = self.console(Box::pin(request.into_inner())).await?;
        Ok(Response::new(Box::pin(output_stream)))
    }

    async fn ping_vm(
        &self,
        request: Request<PingVmRequest>,
    ) -> Result<Response<PingVmResponse>, Status> {
        info!("VmApi: Received PingVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::PingVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn shutdown_vm(
        &self,
        request: Request<ShutdownVmRequest>,
    ) -> Result<Response<ShutdownVmResponse>, Status> {
        info!("VmApi: Received ShutdownVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::ShutdownVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn resume_vm(
        &self,
        request: Request<ResumeVmRequest>,
    ) -> Result<Response<ResumeVmResponse>, Status> {
        info!("VmApi: Received ResumeVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::ResumeVm(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn suspend_vm(
        &self,
        request: Request<SuspendVmRequest>,
    ) -> Result<Response<SuspendVmResponse>, Status> {
        info!("VmApi: Received SuspendVm request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::SuspendVm(request.into_inner(), resp_tx)
        })
        .await
    }
}
