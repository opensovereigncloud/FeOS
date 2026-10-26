// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The lock on a daemon's state directory: one `feosd` per
//! `/var/lib/feos`, one `diskd` per `/var/lib/feos/disk`, whatever runs
//! them, on the host or in containers that share the directory. An
//! exclusive flock(2) on a file in it, held for the process's life and let
//! go of by the kernel when the process ends, however it ends. The file is
//! opened close-on-exec: the programs it runs do not hold it, and a
//! self-upgrade's exec takes it again.

use nix::errno::Errno;
use nix::fcntl::{Flock, FlockArg};
use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::time::Duration;

/// How often a process that waits for the lock tries again.
const RETRY: Duration = Duration::from_secs(1);
/// How often it says that it still waits.
const SAY_AGAIN: u32 = 60;

/// A state directory's lock, held until it is dropped or the process ends.
pub struct StateLock {
    _lock: Flock<File>,
}

/// Takes the lock on `path` if no other process holds it, and writes this
/// process's pid into the file, for whoever looks.
pub fn try_take(path: &Path) -> io::Result<Option<StateLock>> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o644)
        .open(path)?;
    match Flock::lock(file, FlockArg::LockExclusiveNonblock) {
        Ok(mut lock) => {
            // The pid is a hint only; a failure to write it is not one to
            // take the lock.
            let _ = lock
                .set_len(0)
                .and_then(|()| writeln!(lock, "{}", std::process::id()));
            Ok(Some(StateLock { _lock: lock }))
        }
        Err((_, Errno::EWOULDBLOCK)) => Ok(None),
        Err((_, errno)) => Err(io::Error::from(errno)),
    }
}

/// Takes the lock on `path`, waiting as long as another process holds it:
/// a process that stops, such as the old pod of a rollout that suspends
/// its microVMs, gives it up when it ends. `waiting` is told the holder's
/// pid as the file names it, at first and then every minute.
pub async fn take(path: &Path, mut waiting: impl FnMut(Option<u32>)) -> io::Result<StateLock> {
    let mut tries = 0u32;
    loop {
        if let Some(lock) = try_take(path)? {
            return Ok(lock);
        }
        if tries.is_multiple_of(SAY_AGAIN) {
            waiting(holder(path));
        }
        tries = tries.wrapping_add(1);
        tokio::time::sleep(RETRY).await;
    }
}

/// Whether a process holds the lock on `path`; false when there is no such
/// file.
pub fn is_held(path: &Path) -> io::Result<bool> {
    let file = match File::open(path) {
        Ok(file) => file,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(false),
        Err(e) => return Err(e),
    };
    match Flock::lock(file, FlockArg::LockSharedNonblock) {
        // Let go of at once, when it drops.
        Ok(_) => Ok(false),
        Err((_, Errno::EWOULDBLOCK)) => Ok(true),
        Err((_, errno)) => Err(io::Error::from(errno)),
    }
}

/// The pid the lock's holder wrote into the file, if it reads.
pub fn holder(path: &Path) -> Option<u32> {
    std::fs::read_to_string(path).ok()?.trim().parse().ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_holder_at_a_time() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("state").join("feosd.lock");
        assert!(!is_held(&path).unwrap());
        let lock = try_take(&path).unwrap().expect("the lock is free");
        assert_eq!(holder(&path), Some(std::process::id()));
        // A second open file description is refused, in this process too.
        assert!(try_take(&path).unwrap().is_none());
        assert!(is_held(&path).unwrap());
        drop(lock);
        assert!(!is_held(&path).unwrap());
        assert!(try_take(&path).unwrap().is_some());
    }

    #[tokio::test]
    async fn a_waiter_takes_it_once_it_is_free() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("diskd.lock");
        let held = try_take(&path).unwrap().unwrap();
        let waiter = {
            let path = path.clone();
            tokio::spawn(async move {
                let mut told = Vec::new();
                let lock = take(&path, |pid| told.push(pid)).await;
                (lock.is_ok(), told)
            })
        };
        tokio::time::sleep(Duration::from_millis(200)).await;
        drop(held);
        let (taken, told) = waiter.await.unwrap();
        assert!(taken);
        assert_eq!(told, [Some(std::process::id())]);
    }
}
