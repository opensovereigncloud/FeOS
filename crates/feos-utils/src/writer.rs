// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A thread of its own for blocking work that must keep its order, such as
//! the durable writes of records: `fsync(2)` can take seconds while the
//! disk writes back much else, as a snapshot's memory, and must neither
//! block a thread of the async runtime nor run under a lock that a reader
//! takes.
//!
//! [`Writer::run`] queues a job and returns at once, so it may be called
//! under a lock: the jobs run in the order they were queued, which is the
//! order in which the lock saw the changes they keep. The caller awaits
//! the [`Done`] once it let go of the lock. A job whose `Done` was dropped
//! still runs, and its failure is logged.

use log::warn;
use std::future::Future;
use std::io;
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::pin::Pin;
use std::sync::mpsc;
use std::sync::Arc;
use std::task::{Context, Poll};
use tokio::sync::oneshot;

type Job = Box<dyn FnOnce() + Send>;

/// The handle of the thread; it ends once the last handle is gone and its
/// queue is done.
#[derive(Clone)]
pub struct Writer {
    jobs: mpsc::Sender<Job>,
    name: Arc<str>,
}

impl Writer {
    /// Starts the thread `name`.
    pub fn spawn(name: &str) -> io::Result<Self> {
        let (jobs, queue) = mpsc::channel::<Job>();
        std::thread::Builder::new()
            .name(name.to_string())
            .spawn(move || {
                for job in queue {
                    job();
                }
            })?;
        Ok(Self {
            jobs,
            name: name.into(),
        })
    }

    /// Queues `job` behind those queued before; never blocks.
    pub fn run<T: Send + 'static>(
        &self,
        job: impl FnOnce() -> io::Result<T> + Send + 'static,
    ) -> Done<T> {
        let (answer, done) = oneshot::channel();
        let name = self.name.clone();
        let queued = self.jobs.send(Box::new(move || {
            let result = catch_unwind(AssertUnwindSafe(job))
                .unwrap_or_else(|_| Err(io::Error::other("the job panicked")));
            if let Err(Err(e)) = answer.send(result) {
                warn!("Writer {name}: {e}");
            }
        }));
        match queued {
            Ok(()) => Done(Some(done)),
            Err(_) => Done(None),
        }
    }
}

/// The answer of a job: a future, or [`Done::wait`] off the runtime.
pub struct Done<T>(Option<oneshot::Receiver<io::Result<T>>>);

fn gone() -> io::Error {
    io::Error::other("the writer's thread is gone")
}

impl<T> Done<T> {
    /// Waits for the job, blocking the thread: for a caller that is not on
    /// the async runtime.
    pub fn wait(self) -> io::Result<T> {
        match self.0 {
            Some(done) => done.blocking_recv().unwrap_or_else(|_| Err(gone())),
            None => Err(gone()),
        }
    }
}

impl<T> Future for Done<T> {
    type Output = io::Result<T>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<T>> {
        match self.0.as_mut() {
            Some(done) => Pin::new(done)
                .poll(cx)
                .map(|answer| answer.unwrap_or_else(|_| Err(gone()))),
            None => Poll::Ready(Err(gone())),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::time::Duration;

    #[tokio::test]
    async fn jobs_run_in_their_order_and_their_callers_go_on() {
        let writer = Writer::spawn("test-writer").unwrap();
        let (release, held) = std::sync::mpsc::channel::<()>();
        let order = Arc::new(Mutex::new(Vec::new()));
        let first = {
            let order = order.clone();
            writer.run(move || {
                held.recv_timeout(Duration::from_secs(5)).ok();
                order.lock().unwrap().push(1);
                Ok(1)
            })
        };
        let second = {
            let order = order.clone();
            writer.run(move || {
                order.lock().unwrap().push(2);
                Ok(2)
            })
        };
        // Queued while the first job holds the thread: nothing ran yet.
        assert!(order.lock().unwrap().is_empty());
        release.send(()).unwrap();
        assert_eq!(second.await.unwrap(), 2);
        assert_eq!(first.await.unwrap(), 1);
        assert_eq!(*order.lock().unwrap(), [1, 2]);
    }

    #[test]
    fn a_job_that_panics_fails_and_the_thread_goes_on() {
        let writer = Writer::spawn("test-writer").unwrap();
        let failed = writer.run(|| -> io::Result<()> { panic!("no") }).wait();
        assert!(failed.unwrap_err().to_string().contains("panicked"));
        assert_eq!(writer.run(|| Ok(7)).wait().unwrap(), 7);
        // A dropped answer: the job runs all the same.
        drop(writer.run(|| -> io::Result<()> { Err(io::Error::other("logged")) }));
        assert_eq!(writer.run(|| Ok(8)).wait().unwrap(), 8);
    }
}
