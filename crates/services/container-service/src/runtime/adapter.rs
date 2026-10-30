// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use feos_proto::task_service::{
    task_service_client::TaskServiceClient, CreateRequest, DeleteRequest, ExecFrame, ExecRequest,
    KillRequest, StartRequest, WaitRequest,
};
use hyper_util::rt::TokioIo;
use log::info;
use std::path::Path;
use task_service::TASK_SERVICE_SOCKET;
use tonic::transport::{Channel, Endpoint, Uri};
use tonic::Streaming;
use tower::service_fn;

#[derive(Debug, thiserror::Error)]
pub enum AdapterError {
    #[error("Internal error: {0}")]
    Internal(String),
    #[error("I/O error")]
    Io(#[from] std::io::Error),
    #[error("Task service communication failed: {0}")]
    TaskService(#[from] tonic::Status),
}

pub struct ContainerAdapter;

impl Default for ContainerAdapter {
    fn default() -> Self {
        Self::new()
    }
}

impl ContainerAdapter {
    pub fn new() -> Self {
        Self
    }

    async fn get_task_service_client() -> Result<TaskServiceClient<Channel>, AdapterError> {
        let socket_path = Path::new(TASK_SERVICE_SOCKET).to_path_buf();
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
            .map(TaskServiceClient::new)
            .map_err(|e| AdapterError::TaskService(tonic::Status::unavailable(e.to_string())))
    }

    pub async fn create_container(
        &self,
        container_id: &str,
        bundle_path: &Path,
        stdout: &Path,
        stderr: &Path,
    ) -> Result<i64, AdapterError> {
        info!("Adapter: Connecting to TaskService for container {container_id}");
        let mut task_client = Self::get_task_service_client().await?;

        let bundle_path_str = bundle_path
            .to_str()
            .ok_or_else(|| AdapterError::Internal("Bundle path is not valid UTF-8".to_string()))?
            .to_string();

        info!("Adapter: Calling Create RPC on TaskService for container {container_id}");
        let request = CreateRequest {
            container_id: container_id.to_string(),
            bundle_path: bundle_path_str,
            stdin_path: "".to_string(),
            stdout_path: stdout.to_string_lossy().into_owned(),
            stderr_path: stderr.to_string_lossy().into_owned(),
        };

        let response = task_client.create(request).await?;

        let pid = response.into_inner().pid;
        info!("Adapter: TaskService created container {container_id} with PID {pid}");

        Ok(pid)
    }

    pub async fn start_container(&self, container_id: &str) -> Result<(), AdapterError> {
        let mut task_client = Self::get_task_service_client().await?;
        let request = StartRequest {
            container_id: container_id.to_string(),
        };
        task_client.start(request).await?;
        Ok(())
    }

    pub async fn stop_container(
        &self,
        container_id: &str,
        signal: u32,
    ) -> Result<(), AdapterError> {
        let mut task_client = Self::get_task_service_client().await?;
        let request = KillRequest {
            container_id: container_id.to_string(),
            signal,
        };
        task_client.kill(request).await?;
        Ok(())
    }

    pub async fn delete_container(&self, container_id: &str) -> Result<(), AdapterError> {
        let mut task_client = Self::get_task_service_client().await?;
        let request = DeleteRequest {
            container_id: container_id.to_string(),
        };
        task_client.delete(request).await?;
        Ok(())
    }

    /// Waits for the container's process to exit; returns its exit code.
    pub async fn wait_container(&self, container_id: &str) -> Result<i32, AdapterError> {
        let mut task_client = Self::get_task_service_client().await?;
        let request = WaitRequest {
            container_id: container_id.to_string(),
        };
        Ok(task_client.wait(request).await?.into_inner().exit_code)
    }

    pub async fn exec(
        &self,
        container_id: &str,
        args: Vec<String>,
        env: Vec<String>,
        working_dir: String,
    ) -> Result<Streaming<ExecFrame>, AdapterError> {
        let mut task_client = Self::get_task_service_client().await?;
        let request = ExecRequest {
            container_id: container_id.to_string(),
            args,
            env,
            working_dir,
        };
        Ok(task_client.exec(request).await?.into_inner())
    }
}
