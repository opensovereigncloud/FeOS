// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A store whose changes are written off the runtime's threads and seen
//! once they are kept. A change writes its file with fsync(2), which can
//! take seconds while the disk writes back much else, as a snapshot's
//! memory: it runs on the blocking pool, one change at a time, and the
//! readers, the resolver's lookups among them, take the last state kept
//! and never wait for a write.

use std::sync::{Arc, Mutex, PoisonError};
use tonic::Status;

pub(crate) struct Kept<S> {
    /// Held by the change that writes, on the blocking pool.
    writing: Arc<tokio::sync::Mutex<S>>,
    /// The last state kept, for the readers.
    kept: Arc<Mutex<Arc<S>>>,
}

impl<S: Clone + Send + Sync + 'static> Kept<S> {
    pub(crate) fn new(store: S) -> Self {
        Self {
            kept: Arc::new(Mutex::new(Arc::new(store.clone()))),
            writing: Arc::new(tokio::sync::Mutex::new(store)),
        }
    }

    /// The last state kept.
    pub(crate) fn read(&self) -> Arc<S> {
        self.kept
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    /// Changes the store on the blocking pool, after the changes before:
    /// `change` writes what it keeps. What it kept is seen from then on,
    /// also when the caller went away meanwhile.
    pub(crate) async fn change<T: Send + 'static>(
        &self,
        change: impl FnOnce(&mut S) -> Result<T, Status> + Send + 'static,
    ) -> Result<T, Status> {
        let mut store = self.writing.clone().lock_owned().await;
        let kept = self.kept.clone();
        tokio::task::spawn_blocking(move || {
            let changed = change(&mut store)?;
            *kept.lock().unwrap_or_else(PoisonError::into_inner) = Arc::new(store.clone());
            Ok(changed)
        })
        .await
        .unwrap_or_else(|e| Err(Status::internal(format!("the change ended: {e}"))))
    }
}

/// For tests: a write that hangs, as an fsync behind a snapshot's memory
/// does. Its file is a FIFO that no one reads, whose open(2) waits for a
/// reader; [`Hung::open`] lets it go, and the write then fails, as fsync(2)
/// of a FIFO does.
#[cfg(test)]
pub(crate) struct Hung(pub std::path::PathBuf);

#[cfg(test)]
impl Hung {
    pub(crate) fn at(path: std::path::PathBuf) -> Self {
        nix::unistd::mkfifo(&path, nix::sys::stat::Mode::S_IRWXU).unwrap();
        Self(path)
    }

    pub(crate) fn open(&self) {
        let path = self.0.clone();
        std::thread::spawn(move || {
            let mut reader = std::fs::File::open(path).unwrap();
            let _ = std::io::copy(&mut reader, &mut std::io::sink());
        });
    }

    /// Lets the write go once the test says that its runtime moved, or
    /// after 3 s, should the write hold the runtime's thread: a test on a
    /// runtime of one thread fails then instead of hanging. The thread
    /// tells whether the write held the runtime.
    pub(crate) fn watchdog(&self) -> (std::sync::mpsc::Sender<()>, std::thread::JoinHandle<bool>) {
        let (moved, watched) = std::sync::mpsc::channel::<()>();
        let path = self.0.clone();
        let watchdog = std::thread::spawn(move || {
            let stalled = watched
                .recv_timeout(std::time::Duration::from_secs(3))
                .is_err();
            Hung(path).open();
            stalled
        });
        (moved, watchdog)
    }
}
