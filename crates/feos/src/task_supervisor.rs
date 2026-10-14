// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Runs the task service in a process of its own, `feosd --task-service` or
//! `feos-guest --task-service`, and starts it again when it exits. The
//! container runtime forks from that process, not from feosd or the guest's
//! PID 1, and a runtime failure ends that process, not them.

use log::{error, info, warn};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;
use task_service::TASK_SERVICE_SOCKET;

/// The pid of the running task service. After a live upgrade the new binary
/// finds the task service it inherited here and keeps it, and so does a
/// feosd that started again.
const PID_FILE: &str = "/run/feos/task-service.pid";
const RESTART_DELAY: Duration = Duration::from_secs(1);
/// How often a task service that is no child is checked for its end.
const ORPHAN_POLL: Duration = Duration::from_millis(500);
/// How long startup waits for the task service's socket.
const STARTUP_TIMEOUT: Duration = Duration::from_secs(5);

/// The binary that runs the task service: this one, unless
/// FEOS_TASK_SERVICE_BINARY names another (the integration tests run FeOS
/// inside the test binary).
fn binary() -> std::io::Result<PathBuf> {
    match std::env::var_os("FEOS_TASK_SERVICE_BINARY") {
        Some(path) => Ok(PathBuf::from(path)),
        None => std::env::current_exe(),
    }
}

/// Starts the task service and keeps it running. Returns once its socket
/// exists, or after a timeout.
pub(crate) async fn start() {
    tokio::spawn(supervise());
    let deadline = tokio::time::Instant::now() + STARTUP_TIMEOUT;
    while !Path::new(TASK_SERVICE_SOCKET).exists() {
        if tokio::time::Instant::now() >= deadline {
            warn!("TaskSupervisor: No task service socket after {STARTUP_TIMEOUT:?}");
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

async fn supervise() {
    if let Some((pid, child)) = running() {
        info!("TaskSupervisor: Keeping the running task service (pid {pid})");
        if child {
            let code = feos_utils::reaper::wait_for_exit(pid).await;
            warn!("TaskSupervisor: Task service (pid {pid}) exited with code {code}");
        } else {
            // Not a child: whoever inherited it reaps it.
            while Path::new(&format!("/proc/{pid}")).exists() {
                tokio::time::sleep(ORPHAN_POLL).await;
            }
            warn!("TaskSupervisor: Task service (pid {pid}) exited");
        }
    }
    loop {
        if let Err(e) = run_once().await {
            error!("TaskSupervisor: Failed to run the task service: {e}");
        }
        tokio::time::sleep(RESTART_DELAY).await;
    }
}

/// Runs the task service until it exits.
async fn run_once() -> std::io::Result<()> {
    let binary = binary()?;

    if feos_utils::reaper::is_running() {
        // The reaper owns waitpid in this process: ask it for the exit code.
        // A std child handle neither waits nor reaps when dropped, unlike
        // tokio's, which would race the reaper.
        let child = std::process::Command::new(&binary)
            .arg("--task-service")
            .stdin(Stdio::null())
            .spawn()?;
        let pid = child.id() as i32;
        record(pid);
        info!("TaskSupervisor: Started the task service (pid {pid})");
        let code = feos_utils::reaper::wait_for_exit(pid).await;
        warn!("TaskSupervisor: Task service (pid {pid}) exited with code {code}");
    } else {
        let mut child = tokio::process::Command::new(&binary)
            .arg("--task-service")
            .stdin(Stdio::null())
            .spawn()?;
        let pid = child.id().unwrap_or_default() as i32;
        record(pid);
        info!("TaskSupervisor: Started the task service (pid {pid})");
        let status = child.wait().await?;
        warn!("TaskSupervisor: Task service (pid {pid}) exited: {status}");
    }
    Ok(())
}

fn record(pid: i32) {
    if let Some(dir) = Path::new(PID_FILE).parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    if let Err(e) = std::fs::write(PID_FILE, pid.to_string()) {
        warn!("TaskSupervisor: Failed to write {PID_FILE}: {e}");
    }
}

/// A task service that runs already, and whether it is a child of this
/// process. The binary that exec'd this one in a live upgrade left it as a
/// child; a feosd that ended left it orphaned, under systemd's KillMode=
/// process.
fn running() -> Option<(i32, bool)> {
    let pid: i32 = std::fs::read_to_string(PID_FILE)
        .ok()?
        .trim()
        .parse()
        .ok()?;
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    // The command name in field 2 may contain spaces; it ends at the last ')'.
    let ppid: u32 = stat
        .rsplit_once(')')?
        .1
        .split_whitespace()
        .nth(1)?
        .parse()
        .ok()?;
    let cmdline = std::fs::read(format!("/proc/{pid}/cmdline")).ok()?;
    let is_task_service = cmdline
        .split(|b| *b == 0)
        .any(|arg| arg == b"--task-service");
    is_task_service.then_some((pid, ppid == std::process::id()))
}
