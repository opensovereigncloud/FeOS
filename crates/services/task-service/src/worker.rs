// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The container runtime: youki's libcontainer, as a library. Its calls
//! block on the filesystem and on processes it forks, so they run on the
//! blocking pool.

use crate::error::TaskError;
use crate::{Event, RUNTIME_STATE_DIR};
use feos_proto::task_service::{
    CreateRequest, CreateResponse, DeleteRequest, DeleteResponse, KillRequest, KillResponse,
    StartRequest, StartResponse,
};
use libcontainer::container::builder::ContainerBuilder;
use libcontainer::container::{Container, ContainerStatus};
use libcontainer::signal::Signal;
use libcontainer::syscall::syscall::SyscallType;
use log::{error, info, warn};
use std::fs::{File, OpenOptions};
use std::os::unix::fs::FileTypeExt;
use std::path::Path;
use tokio::sync::{mpsc, oneshot};

/// Runs `f` on the blocking pool.
async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> Result<T, TaskError> + Send + 'static,
) -> Result<T, TaskError> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| TaskError::Internal(format!("runtime task: {e}")))?
}

fn load(id: &str) -> Result<Container, TaskError> {
    Container::load(Path::new(RUNTIME_STATE_DIR).join(id))
        .map_err(|e| TaskError::Runtime(format!("load container {id}: {e}")))
}

/// Where a container's stdout or stderr goes: appended to `path`, or
/// discarded when it is empty. A FIFO at `path`, as the container service
/// makes them (ADR 0026), is held open for reading too, the second file,
/// while the container is there: its reader may restart, and a FIFO
/// without one would end the container's writes with SIGPIPE.
fn output(path: &str) -> Result<(File, Option<File>), TaskError> {
    let path = if path.is_empty() { "/dev/null" } else { path };
    let failed = |e: std::io::Error| TaskError::Io(format!("{path}: {e}"));
    let is_fifo = std::fs::metadata(path).is_ok_and(|meta| meta.file_type().is_fifo());
    // Read and write: opening it never waits for a writer.
    let keeper = match is_fifo {
        true => Some(
            OpenOptions::new()
                .read(true)
                .write(true)
                .open(path)
                .map_err(failed)?,
        ),
        false => None,
    };
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .map_err(failed)?;
    Ok((file, keeper))
}

/// Creates the container from its bundle; returns the pid of its init
/// process, which waits for Start, and its output's FIFOs to hold.
fn create(req: &CreateRequest) -> Result<(i32, Vec<File>), TaskError> {
    let state = Path::new(RUNTIME_STATE_DIR).join(&req.container_id);
    if state.exists() {
        // Runtime state the dispatcher does not know: left from a task
        // service that restarted. Running containers are not touched.
        let mut stale = load(&req.container_id)?;
        if stale.status() == ContainerStatus::Running {
            return Err(TaskError::ContainerAlreadyExists(req.container_id.clone()));
        }
        warn!(
            "Worker: Removing stale runtime state of {}",
            req.container_id
        );
        stale
            .delete(true)
            .map_err(|e| TaskError::Runtime(format!("remove stale state: {e}")))?;
    }

    std::fs::create_dir_all(RUNTIME_STATE_DIR)?;
    let (stdout, stdout_keeper) = output(&req.stdout_path)?;
    let (stderr, stderr_keeper) = output(&req.stderr_path)?;
    let outputs = stdout_keeper.into_iter().chain(stderr_keeper).collect();
    let container = ContainerBuilder::new(req.container_id.clone(), SyscallType::Linux)
        .with_root_path(RUNTIME_STATE_DIR)
        .map_err(|e| TaskError::Runtime(format!("runtime state directory: {e}")))?
        .with_stdout(stdout)
        .with_stderr(stderr)
        .as_init(&req.bundle_path)
        .with_systemd(false)
        .build()
        .map_err(|e| TaskError::Runtime(format!("create {}: {e}", req.container_id)))?;
    container
        .pid()
        .map(|pid| (pid.as_raw(), outputs))
        .ok_or_else(|| TaskError::Internal("created container has no pid".to_string()))
}

pub async fn handle_create(
    req: CreateRequest,
    event_tx: mpsc::Sender<Event>,
    responder: oneshot::Sender<Result<CreateResponse, TaskError>>,
) {
    let id = req.container_id.clone();
    info!("Worker: Creating container {id} from {}", req.bundle_path);
    match blocking(move || create(&req)).await {
        Ok((pid, outputs)) => {
            info!("Worker: Created container {id} with init pid {pid}");
            let _ = event_tx
                .send(Event::ContainerCreated { id, pid, outputs })
                .await;
            let _ = responder.send(Ok(CreateResponse { pid: pid.into() }));
        }
        Err(error) => {
            error!("Worker: Failed to create container {id}: {error}");
            let _ = event_tx
                .send(Event::ContainerCreateFailed {
                    id,
                    error: error.clone(),
                })
                .await;
            let _ = responder.send(Err(error));
        }
    }
}

pub async fn handle_start(
    req: StartRequest,
    pid: i32,
    event_tx: mpsc::Sender<Event>,
    responder: oneshot::Sender<Result<StartResponse, TaskError>>,
) {
    let id = req.container_id.clone();
    let result = blocking(move || {
        load(&req.container_id)?
            .start()
            .map_err(|e| TaskError::Runtime(format!("start {}: {e}", req.container_id)))
    })
    .await;

    match result {
        Ok(()) => {
            let _ = event_tx
                .send(Event::ContainerStarted { id: id.clone() })
                .await;
            let _ = responder.send(Ok(StartResponse {}));
            tokio::spawn(wait_for_process_exit(id, pid, event_tx));
        }
        Err(error) => {
            let _ = event_tx
                .send(Event::ContainerStartFailed {
                    id,
                    error: error.clone(),
                })
                .await;
            let _ = responder.send(Err(error));
        }
    }
}

fn signal(number: u32) -> Result<Signal, TaskError> {
    i32::try_from(number)
        .ok()
        .and_then(|n| Signal::try_from(n).ok())
        .ok_or_else(|| TaskError::InvalidArgument(format!("invalid signal {number}")))
}

pub async fn handle_kill(
    req: KillRequest,
    responder: oneshot::Sender<Result<KillResponse, TaskError>>,
) {
    let result = blocking(move || {
        let signal = signal(req.signal)?;
        load(&req.container_id)?
            .kill(signal, true)
            .map_err(|e| TaskError::Runtime(format!("kill {}: {e}", req.container_id)))
    })
    .await;
    let _ = responder.send(result.map(|()| KillResponse {}));
}

pub async fn handle_delete(
    req: DeleteRequest,
    event_tx: mpsc::Sender<Event>,
    responder: oneshot::Sender<Result<DeleteResponse, TaskError>>,
) {
    let id = req.container_id.clone();
    let result = blocking(move || {
        let mut container = match load(&req.container_id) {
            Ok(container) => container,
            // Nothing left to delete.
            Err(_)
                if !Path::new(RUNTIME_STATE_DIR)
                    .join(&req.container_id)
                    .exists() =>
            {
                return Ok(());
            }
            Err(e) => return Err(e),
        };
        // force: a running container is killed first.
        container
            .delete(true)
            .map_err(|e| TaskError::Runtime(format!("delete {}: {e}", req.container_id)))
    })
    .await;

    if let Err(error) = result {
        let _ = responder.send(Err(error));
        return;
    }
    let _ = event_tx.send(Event::ContainerDeleted { id }).await;
    let _ = responder.send(Ok(DeleteResponse {}));
}

/// Reports the exit of a container's init process. The reaper owns
/// waitpid in this process.
pub async fn wait_for_process_exit(id: String, pid: i32, event_tx: mpsc::Sender<Event>) {
    info!("Worker: Waiting for container {id} (pid {pid}) to exit");
    let exit_code = feos_utils::reaper::wait_for_exit(pid).await;
    info!("Worker: Container {id} (pid {pid}) exited with code {exit_code}");
    if event_tx
        .send(Event::ContainerStopped { id, exit_code })
        .await
        .is_err()
    {
        error!("Worker: Failed to send ContainerStopped event. Dispatcher may be down.");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_fifo_is_held_open_for_its_reader() {
        let dir = tempfile::tempdir().unwrap();
        let fifo = dir.path().join("stdout");
        nix::unistd::mkfifo(&fifo, nix::sys::stat::Mode::S_IRWXU).unwrap();
        // No reader but the one it holds: the open does not wait, and a
        // write does not fail.
        let (mut file, keeper) = output(fifo.to_str().unwrap()).unwrap();
        assert!(keeper.is_some());
        std::io::Write::write_all(&mut file, b"line\n").unwrap();
        // A file is appended to, as before.
        let log = dir.path().join("container.log");
        let (_, keeper) = output(log.to_str().unwrap()).unwrap();
        assert!(keeper.is_none() && log.is_file());
        let (_, keeper) = output("").unwrap();
        assert!(keeper.is_none());
    }

    #[test]
    fn signal_numbers_are_checked() {
        assert!(signal(9).is_ok());
        assert!(signal(0).is_err());
        assert!(signal(u32::MAX).is_err());
    }
}
