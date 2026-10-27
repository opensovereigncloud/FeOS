// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Exec: a process in the namespaces of a running container, without a
//! terminal and without stdin.
//!
//! One dedicated OS thread per exec, never an async worker, because setns(2)
//! changes the calling thread:
//!   1. join the container's uts and ipc namespaces;
//!   2. join its pid namespace, which only affects children;
//!   3. fork; the child joins the mount namespace, changes directory, wires
//!      stdout and stderr to pipes, starts its own session (a process group
//!      to kill) and execs the command;
//!   4. the thread forwards what the pipes carry as frames, waits for the
//!      exit code from the reaper and sends it as the last frame.
//!
//! Everything the child needs is allocated before the fork: after fork(2) in
//! a multithreaded process only async-signal-safe calls are safe.
//!
//! When the caller goes away, the response stream is dropped and the guard
//! kills the process group, unless the process has exited already.

use crate::error::TaskError;
use feos_proto::task_service::exec_frame::Frame;
use feos_proto::task_service::{ExecFrame, ExecRequest};
use nix::sched::{setns, CloneFlags};
use std::ffi::CString;
use std::fs::File;
use std::io::Read;
use std::os::fd::{AsFd, AsRawFd, OwnedFd, RawFd};
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::Arc;
use std::task::{Context, Poll};
use tokio::sync::mpsc;
use tokio_stream::wrappers::UnboundedReceiverStream;
use tokio_stream::Stream;
use tonic::Status;

const CHUNK: usize = 16 * 1024;
const DEFAULT_PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
/// Exit code when the process could not be started.
const EXIT_NOT_STARTED: i32 = 126;
/// Exit code when the command was not found.
const EXIT_NOT_FOUND: i32 = 127;

type FrameResult = Result<ExecFrame, Status>;

/// What the stream and the exec thread share: the pid of the process group
/// to kill, and whether the process has exited already.
struct Shared {
    pid: AtomicI32,
    exited: AtomicBool,
}

/// The response stream of an exec. Dropping it before the last frame kills
/// the process group.
pub struct ExecStream {
    inner: UnboundedReceiverStream<FrameResult>,
    shared: Arc<Shared>,
}

impl Stream for ExecStream {
    type Item = FrameResult;

    fn poll_next(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<FrameResult>> {
        Pin::new(&mut self.inner).poll_next(cx)
    }
}

impl Drop for ExecStream {
    fn drop(&mut self) {
        let pid = self.shared.pid.load(Ordering::Acquire);
        if pid > 0 && !self.shared.exited.load(Ordering::Acquire) {
            // The child called setsid(), so its pid is its process group.
            let _ = nix::sys::signal::killpg(
                nix::unistd::Pid::from_raw(pid),
                nix::sys::signal::Signal::SIGKILL,
            );
        }
    }
}

/// Everything the child needs, prepared before the fork.
struct Prepared {
    /// Candidate paths of the executable, tried in order.
    candidates: Vec<CString>,
    argv: Vec<CString>,
    envp: Vec<CString>,
    working_dir: CString,
    /// Message for a command that is not found, with a newline.
    not_found: Vec<u8>,
}

impl Prepared {
    fn new(req: &ExecRequest) -> Result<Prepared, TaskError> {
        let invalid = |what: &str| TaskError::InvalidArgument(format!("NUL byte in {what}"));
        let arg0 = req
            .args
            .first()
            .ok_or_else(|| TaskError::InvalidArgument("empty command".to_string()))?;
        let argv = req
            .args
            .iter()
            .map(|a| CString::new(a.as_str()))
            .collect::<Result<Vec<_>, _>>()
            .map_err(|_| invalid("args"))?;
        let envp = req
            .env
            .iter()
            .map(|e| CString::new(e.as_str()))
            .collect::<Result<Vec<_>, _>>()
            .map_err(|_| invalid("env"))?;
        let working_dir = if req.working_dir.is_empty() {
            "/"
        } else {
            req.working_dir.as_str()
        };
        let working_dir = CString::new(working_dir).map_err(|_| invalid("working_dir"))?;
        // A name with a slash is a path; a bare name is looked up in the PATH
        // of the environment the process gets, like execvpe(3).
        let candidates = if arg0.contains('/') {
            vec![CString::new(arg0.as_str()).map_err(|_| invalid("args"))?]
        } else {
            let path = req
                .env
                .iter()
                .find_map(|e| e.strip_prefix("PATH="))
                .unwrap_or(DEFAULT_PATH);
            path.split(':')
                .filter(|dir| !dir.is_empty())
                .filter_map(|dir| CString::new(format!("{dir}/{arg0}")).ok())
                .collect()
        };
        Ok(Prepared {
            candidates,
            argv,
            envp,
            working_dir,
            not_found: format!("feos-exec: command not found: {arg0}\n").into_bytes(),
        })
    }
}

/// Starts `req` in the container whose init process is `container_pid`.
pub fn spawn(container_pid: i32, req: ExecRequest) -> Result<ExecStream, TaskError> {
    let prepared = Prepared::new(&req)?;
    let (tx, rx) = mpsc::unbounded_channel();
    let shared = Arc::new(Shared {
        pid: AtomicI32::new(0),
        exited: AtomicBool::new(false),
    });
    let thread_shared = shared.clone();
    std::thread::Builder::new()
        .name(format!("exec-{}", req.container_id))
        .spawn(move || exec_thread(container_pid, prepared, tx, thread_shared))
        .map_err(|e| TaskError::Internal(format!("exec thread: {e}")))?;
    Ok(ExecStream {
        inner: UnboundedReceiverStream::new(rx),
        shared,
    })
}

fn send(tx: &mpsc::UnboundedSender<FrameResult>, frame: Frame) {
    // A closed receiver means the guard kills the process; stop sending.
    let _ = tx.send(Ok(ExecFrame { frame: Some(frame) }));
}

fn pipe() -> std::io::Result<(OwnedFd, OwnedFd)> {
    Ok(nix::unistd::pipe2(nix::fcntl::OFlag::O_CLOEXEC)?)
}

fn exec_thread(
    container_pid: i32,
    prepared: Prepared,
    tx: mpsc::UnboundedSender<FrameResult>,
    shared: Arc<Shared>,
) {
    let fail = |message: String| {
        send(
            &tx,
            Frame::Stderr(format!("feos-exec: {message}\n").into_bytes()),
        );
        send(&tx, Frame::ExitCode(EXIT_NOT_STARTED));
    };

    let namespace = |kind: &str| {
        File::open(format!("/proc/{container_pid}/ns/{kind}"))
            .map_err(|e| format!("namespace {kind} of pid {container_pid}: {e}"))
    };
    let namespaces = (|| {
        Ok::<_, String>((
            namespace("uts")?,
            namespace("ipc")?,
            namespace("mnt")?,
            namespace("pid")?,
        ))
    })();
    let (uts, ipc, mnt, pidns) = match namespaces {
        Ok(namespaces) => namespaces,
        Err(e) => return fail(e),
    };

    let ((out_read, out_write), (err_read, err_write)) = match (pipe(), pipe()) {
        (Ok(out), Ok(err)) => (out, err),
        (Err(e), _) | (_, Err(e)) => return fail(format!("pipe: {e}")),
    };

    // Drain stderr concurrently, or a child that floods stderr while stdout
    // is open deadlocks. The helper has to start before this thread joins
    // the pid namespace: after that, spawning a thread fails with EAGAIN.
    let tx_err = tx.clone();
    let err_thread =
        std::thread::spawn(move || forward(File::from(err_read), &tx_err, Frame::Stderr));

    for (namespace, flag, what) in [
        (uts.as_fd(), CloneFlags::CLONE_NEWUTS, "uts"),
        (ipc.as_fd(), CloneFlags::CLONE_NEWIPC, "ipc"),
        (pidns.as_fd(), CloneFlags::CLONE_NEWPID, "pid"),
    ] {
        if let Err(e) = setns(namespace, flag) {
            // Closing the write ends ends the stderr helper.
            drop((out_write, err_write));
            let _ = err_thread.join();
            return fail(format!("join {what} namespace: {e}"));
        }
    }

    // SAFETY: the child only makes async-signal-safe calls (see exec_child).
    let child = unsafe { libc::fork() };
    if child < 0 {
        let e = std::io::Error::last_os_error();
        drop((out_write, err_write));
        let _ = err_thread.join();
        return fail(format!("fork: {e}"));
    }
    if child == 0 {
        exec_child(
            mnt.as_raw_fd(),
            out_write.as_raw_fd(),
            err_write.as_raw_fd(),
            &prepared,
        );
    }

    shared.pid.store(child, Ordering::Release);
    // The child holds its own copies; EOF comes when it and its children
    // close theirs.
    drop((out_write, err_write));
    forward(File::from(out_read), &tx, Frame::Stdout);
    let _ = err_thread.join();

    let code = feos_utils::reaper::wait_for_exit_blocking(child);
    shared.exited.store(true, Ordering::Release);
    send(&tx, Frame::ExitCode(code));
}

/// Forwards what `reader` delivers as frames until EOF.
fn forward(mut reader: File, tx: &mpsc::UnboundedSender<FrameResult>, frame: fn(Vec<u8>) -> Frame) {
    let mut buf = [0u8; CHUNK];
    loop {
        match reader.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => send(tx, frame(buf[..n].to_vec())),
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => break,
        }
    }
}

/// The child after fork: join the mount namespace, wire stdio, start a
/// session and exec. Only async-signal-safe libc calls; never returns.
fn exec_child(mnt: RawFd, out_write: RawFd, err_write: RawFd, prepared: &Prepared) -> ! {
    // SAFETY: plain syscalls on fds and buffers this process owns; every
    // path ends in execve or _exit.
    unsafe {
        let die = |message: &[u8], code: i32| -> ! {
            libc::write(err_write, message.as_ptr().cast(), message.len());
            libc::_exit(code)
        };
        if libc::setns(mnt, libc::CLONE_NEWNS) != 0 {
            die(
                b"feos-exec: cannot join the mount namespace\n",
                EXIT_NOT_STARTED,
            );
        }
        libc::setsid();
        if libc::chdir(prepared.working_dir.as_ptr()) != 0 {
            libc::chdir(c"/".as_ptr());
        }
        // dup2 clears O_CLOEXEC on the new fds; the originals close on exec.
        libc::dup2(out_write, 1);
        libc::dup2(err_write, 2);

        let mut argv: [*const libc::c_char; 256] = [std::ptr::null(); 256];
        let mut envp: [*const libc::c_char; 1024] = [std::ptr::null(); 1024];
        if prepared.argv.len() >= argv.len() || prepared.envp.len() >= envp.len() {
            die(
                b"feos-exec: too many arguments or variables\n",
                EXIT_NOT_STARTED,
            );
        }
        for (slot, arg) in argv.iter_mut().zip(&prepared.argv) {
            *slot = arg.as_ptr();
        }
        for (slot, var) in envp.iter_mut().zip(&prepared.envp) {
            *slot = var.as_ptr();
        }
        for candidate in &prepared.candidates {
            libc::execve(candidate.as_ptr(), argv.as_ptr(), envp.as_ptr());
        }
        die(&prepared.not_found, EXIT_NOT_FOUND)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(args: &[&str], env: &[&str]) -> ExecRequest {
        ExecRequest {
            container_id: "c".to_string(),
            args: args.iter().map(|s| s.to_string()).collect(),
            env: env.iter().map(|s| s.to_string()).collect(),
            working_dir: String::new(),
        }
    }

    #[test]
    fn bare_names_are_looked_up_in_the_path_of_the_env() {
        let prepared = Prepared::new(&request(&["ls", "-l"], &["PATH=/a:/b"])).unwrap();
        let candidates: Vec<_> = prepared
            .candidates
            .iter()
            .map(|c| c.to_str().unwrap())
            .collect();
        assert_eq!(candidates, ["/a/ls", "/b/ls"]);
        assert_eq!(prepared.working_dir.to_str().unwrap(), "/");
    }

    #[test]
    fn paths_are_used_as_they_are() {
        let prepared = Prepared::new(&request(&["./run"], &[])).unwrap();
        assert_eq!(prepared.candidates.len(), 1);
    }

    #[test]
    fn nul_bytes_and_empty_commands_are_refused() {
        assert!(Prepared::new(&request(&[], &[])).is_err());
        assert!(Prepared::new(&request(&["a\0b"], &[])).is_err());
    }
}
