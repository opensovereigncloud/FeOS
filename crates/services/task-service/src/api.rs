// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::Command;
use feos_proto::task_service::{
    task_service_server::TaskService, CreateRequest, CreateResponse, DeleteRequest, DeleteResponse,
    KillRequest, KillResponse, StartRequest, StartResponse, WaitRequest, WaitResponse,
};
use feos_utils::dispatch::dispatch_and_wait;
use log::info;
use tokio::sync::mpsc;
use tonic::{Request, Response, Status};

pub struct TaskApiHandler {
    dispatcher_tx: mpsc::Sender<Command>,
}

impl TaskApiHandler {
    pub fn new(dispatcher_tx: mpsc::Sender<Command>) -> Self {
        Self { dispatcher_tx }
    }
}

#[tonic::async_trait]
impl TaskService for TaskApiHandler {
    async fn create(
        &self,
        request: Request<CreateRequest>,
    ) -> Result<Response<CreateResponse>, Status> {
        info!(
            "API: Received Create request for {}",
            request.get_ref().container_id
        );
        dispatch_and_wait(&self.dispatcher_tx, |responder| Command::Create {
            req: request.into_inner(),
            responder,
        })
        .await
    }

    async fn start(
        &self,
        request: Request<StartRequest>,
    ) -> Result<Response<StartResponse>, Status> {
        info!(
            "API: Received Start request for {}",
            request.get_ref().container_id
        );
        dispatch_and_wait(&self.dispatcher_tx, |responder| Command::Start {
            req: request.into_inner(),
            responder,
        })
        .await
    }

    async fn kill(&self, request: Request<KillRequest>) -> Result<Response<KillResponse>, Status> {
        info!(
            "API: Received Kill request for {}",
            request.get_ref().container_id
        );
        dispatch_and_wait(&self.dispatcher_tx, |responder| Command::Kill {
            req: request.into_inner(),
            responder,
        })
        .await
    }

    async fn delete(
        &self,
        request: Request<DeleteRequest>,
    ) -> Result<Response<DeleteResponse>, Status> {
        info!(
            "API: Received Delete request for {}",
            request.get_ref().container_id
        );
        dispatch_and_wait(&self.dispatcher_tx, |responder| Command::Delete {
            req: request.into_inner(),
            responder,
        })
        .await
    }

    async fn wait(&self, request: Request<WaitRequest>) -> Result<Response<WaitResponse>, Status> {
        info!(
            "API: Received Wait request for {}",
            request.get_ref().container_id
        );
        dispatch_and_wait(&self.dispatcher_tx, |responder| Command::Wait {
            req: request.into_inner(),
            responder,
        })
        .await
    }
}
