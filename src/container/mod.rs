use container_service::container_service_server::ContainerService;
use libcontainer;
use libcontainer::container::builder::ContainerBuilder;
use libcontainer::container::Container;
use libcontainer::signal::Signal;
use libcontainer::syscall::syscall::SyscallType;
use log::info;
use std::path::PathBuf;
use tonic::{Request, Response, Status};

use libcontainer::workload::default::DefaultExecutor;

pub mod container_service {
    tonic::include_proto!("container");
}

#[derive(Debug, Default)]
pub struct ContainerAPI {}

fn create(
    id: String,
    bundle: PathBuf,
    socket: Option<PathBuf>,
) -> anyhow::Result<libcontainer::container::Container> {
    let container = ContainerBuilder::new(id, SyscallType::default())
        .with_executor(DefaultExecutor {})
        .with_root_path("/run/containers/youki")?
        .with_console_socket(socket)
        .validate_id()?
        .as_init(&bundle)
        .with_systemd(false)
        .with_detach(true)
        .build()?;

    Ok(container)
}

#[tonic::async_trait]
impl ContainerService for ContainerAPI {
    async fn create_container(
        &self,
        request: Request<container_service::CreateContainerRequest>,
    ) -> Result<Response<container_service::CreateContainerResponse>, Status> {
        info!("Got create_container request");

        let id = request.get_ref().uuid.clone();
        // TODO get from image
        let bundle = PathBuf::from("/home/lukasfrank/dev/sample-nginx-pod");

        // let socket = PathBuf::from("/home/lukasfrank/dev/sample-nginx-pod/sock.tty");

        let mut container = create(id, bundle, None).expect("msg");

        Ok(Response::new(container_service::CreateContainerResponse {}))
    }

    async fn run_container(
        &self,
        request: Request<container_service::RunContainerRequest>,
    ) -> Result<Response<container_service::RunContainerResponse>, Status> {
        info!("Got run_container request");

        let container_id: String = request.get_ref().uuid.clone();

        let container_root = PathBuf::from(format!("/run/containers/youki/{}", container_id));
        if !container_root.exists() {
            info!("container {} does not exist.", container_id)
        }

        let mut container = Container::load(container_root).expect("msg");
        container.start().expect("msg");

        Ok(Response::new(container_service::RunContainerResponse {}))
    }

    async fn kill_container(
        &self,
        request: Request<container_service::KillContainerRequest>,
    ) -> Result<Response<container_service::KillContainerResponse>, Status> {
        info!("Got kill_container request");

        let container_id: String = request.get_ref().uuid.clone();

        let container_root = PathBuf::from(format!("/run/containers/youki/{}", container_id));
        if !container_root.exists() {
            info!("container {} does not exist.", container_id)
        }

        let signal: Signal = "9".try_into().expect("msg");

        let mut container = Container::load(container_root).expect("msg");
        container.kill(signal, false).expect("msg");

        Ok(Response::new(container_service::KillContainerResponse {}))
    }

    async fn state_container(
        &self,
        request: Request<container_service::StateContainerRequest>,
    ) -> Result<Response<container_service::StateContainerResponse>, Status> {
        info!("Got state_container request");

        let container_id: String = request.get_ref().uuid.clone();

        let container_root = PathBuf::from(format!("/run/containers/youki/{}", container_id));
        if !container_root.exists() {
            info!("container {} does not exist.", container_id)
        }

        let mut container = Container::load(container_root.clone()).expect("msg");
        let _ = container.refresh_status().expect("msg");

        info!("{:?}", container.state);

        Ok(Response::new(container_service::StateContainerResponse {
            state: container.state.status.to_string(),
            pid: container.state.pid,
        }))
    }

    async fn delete_container(
        &self,
        request: Request<container_service::DeleteContainerRequest>,
    ) -> Result<Response<container_service::DeleteContainerResponse>, Status> {
        info!("Got delete_container request");

        let container_id: String = request.get_ref().uuid.clone();

        let container_root = PathBuf::from(format!("/run/containers/youki/{}", container_id));
        if !container_root.exists() {
            info!("container {} does not exist.", container_id)
        }

        let mut container = Container::load(container_root).expect("msg");
        container.delete(false).expect("msg");

        Ok(Response::new(container_service::DeleteContainerResponse {}))
    }
}
