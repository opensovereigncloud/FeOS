// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The one owner of waitpid(2) in a process that reaps for others: PID 1,
//! and every subreaper (PR_SET_CHILD_SUBREAPER) such as the task service.
//!
//! Orphans are reparented to such a process, and someone has to reap them or
//! they stay zombies. Exactly one task calls waitpid(-1): a second caller of
//! waitpid would race it, and whoever loses sees ECHILD and loses the exit
//! status. Everyone else who wants a child's exit code asks the reaper.
//!
//! On reap, an exit code goes to the waiter registered for the pid, or is
//! parked for a waiter that registers later (up to [`PARKED_CAP`] pids). The
//! check and the registration happen under one lock, so a child that dies
//! between fork and registration is not lost.

use log::warn;
use nix::sys::wait::{waitpid, WaitPidFlag, WaitStatus};
use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, MutexGuard};
use tokio::sync::oneshot;

/// Exit codes kept for pids nobody waits for yet.
const PARKED_CAP: usize = 256;

#[derive(Default)]
struct Registry {
    waiters: HashMap<i32, oneshot::Sender<i32>>,
    parked: HashMap<i32, i32>,
    parked_order: VecDeque<i32>,
}

static REGISTRY: Mutex<Option<Registry>> = Mutex::new(None);
static RUNNING: AtomicBool = AtomicBool::new(false);

fn registry() -> MutexGuard<'static, Option<Registry>> {
    // A panic while holding the lock leaves the maps consistent: every
    // operation on them is a single insert or remove.
    REGISTRY.lock().unwrap_or_else(|e| e.into_inner())
}

/// Whether [`spawn`] has started the reaper in this process.
pub fn is_running() -> bool {
    RUNNING.load(Ordering::Acquire)
}

/// Starts the reaper task. Call once, inside a tokio runtime.
pub fn spawn() {
    if RUNNING.swap(true, Ordering::AcqRel) {
        return;
    }
    tokio::spawn(async {
        use tokio::signal::unix::{signal, SignalKind};
        let mut sigchld = match signal(SignalKind::child()) {
            Ok(sigchld) => sigchld,
            Err(e) => {
                warn!("Reaper: no SIGCHLD handler ({e}); children will not be reaped");
                RUNNING.store(false, Ordering::Release);
                return;
            }
        };
        loop {
            // One SIGCHLD may stand for several exited children.
            reap_all();
            sigchld.recv().await;
        }
    });
}

fn reap_all() {
    loop {
        match waitpid(None, Some(WaitPidFlag::WNOHANG)) {
            Ok(WaitStatus::StillAlive) | Err(nix::Error::ECHILD) => return,
            Ok(status) => {
                if let (Some(pid), Some(code)) = (status.pid(), exit_code(status)) {
                    deliver(pid.as_raw(), code);
                }
            }
            Err(nix::Error::EINTR) => {}
            Err(e) => {
                warn!("Reaper: waitpid failed: {e}");
                return;
            }
        }
    }
}

/// The exit code of a terminated child; 128 + the signal when signalled.
fn exit_code(status: WaitStatus) -> Option<i32> {
    match status {
        WaitStatus::Exited(_, code) => Some(code),
        WaitStatus::Signaled(_, signal, _) => Some(128 + signal as i32),
        _ => None,
    }
}

fn deliver(pid: i32, code: i32) {
    let mut guard = registry();
    let registry = guard.get_or_insert_with(Registry::default);
    if let Some(waiter) = registry.waiters.remove(&pid) {
        let _ = waiter.send(code);
        return;
    }
    if registry.parked.len() >= PARKED_CAP
        && let Some(oldest) = registry.parked_order.pop_front()
    {
        registry.parked.remove(&oldest);
    }
    registry.parked.insert(pid, code);
    registry.parked_order.push_back(pid);
}

enum Exit {
    Known(i32),
    Pending(oneshot::Receiver<i32>),
}

fn register(pid: i32) -> Exit {
    let mut guard = registry();
    let registry = guard.get_or_insert_with(Registry::default);
    if let Some(code) = registry.parked.remove(&pid) {
        registry.parked_order.retain(|p| *p != pid);
        return Exit::Known(code);
    }
    let (tx, rx) = oneshot::channel();
    registry.waiters.insert(pid, tx);
    Exit::Pending(rx)
}

/// Waits until the child `pid` exits and returns its exit code. The reaper
/// must be running in this process.
pub async fn wait_for_exit(pid: i32) -> i32 {
    match register(pid) {
        Exit::Known(code) => code,
        // The sender only goes away with the process.
        Exit::Pending(rx) => rx.await.unwrap_or(-1),
    }
}

/// [`wait_for_exit`] for threads outside the async runtime.
pub fn wait_for_exit_blocking(pid: i32) -> i32 {
    match register(pid) {
        Exit::Known(code) => code,
        Exit::Pending(rx) => rx.blocking_recv().unwrap_or(-1),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_parked_exit_code_is_handed_out_once() {
        deliver(-42, 3);
        assert!(matches!(register(-42), Exit::Known(3)));
        assert!(matches!(register(-42), Exit::Pending(_)));
    }

    #[test]
    fn a_registered_waiter_gets_the_code() {
        let Exit::Pending(rx) = register(-43) else {
            panic!("nothing was parked for -43");
        };
        deliver(-43, 7);
        assert_eq!(rx.blocking_recv().unwrap(), 7);
    }

    #[test]
    fn signals_map_to_128_plus_the_signal() {
        let pid = nix::unistd::Pid::from_raw(1);
        assert_eq!(
            exit_code(WaitStatus::Signaled(pid, nix::sys::signal::SIGKILL, false)),
            Some(137)
        );
        assert_eq!(exit_code(WaitStatus::Exited(pid, 2)), Some(2));
    }
}
