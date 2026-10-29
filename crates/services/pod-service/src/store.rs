// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Where the pods are kept across restarts of feosd: a file per pod,
//! `<id>.pb`, its PodInfo without the containers' status, replaced by a
//! rename after it is synced.
//!
//! The writes run on a thread of their own, in the order they were asked
//! for (`feos_utils::writer`): a save is asked for under the pods' lock,
//! which `ListPods`, `GetPod` and every call to a guest take too, and
//! returns at once; its caller awaits it once it let go of the lock. An
//! fsync that takes seconds, as while a snapshot's memory is written to the
//! same disk, holds neither that lock nor a thread of the runtime.

use feos_proto::pod_service::PodInfo;
use feos_utils::writer::{Done, Writer};
use log::warn;
use prost::Message;
use std::fs;
use std::io::{self, Write};
use std::path::{Path, PathBuf};

pub struct Store {
    dir: PathBuf,
    writer: Writer,
}

impl Store {
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let dir = dir.into();
        fs::create_dir_all(&dir)?;
        let writer = Writer::spawn("pod-records")?;
        Ok(Self { dir, writer })
    }

    /// The pods kept. A file that does not read is left alone, and logged.
    pub fn load(&self) -> io::Result<Vec<PodInfo>> {
        let mut pods = Vec::new();
        for entry in fs::read_dir(&self.dir)? {
            let path = entry?.path();
            if path.extension().is_none_or(|ext| ext != "pb") {
                continue;
            }
            match fs::read(&path).map(|bytes| PodInfo::decode(bytes.as_slice())) {
                Ok(Ok(pod)) => pods.push(pod),
                Ok(Err(e)) => warn!("Pods: {} does not decode: {e}", path.display()),
                Err(e) => warn!("Pods: {} does not read: {e}", path.display()),
            }
        }
        Ok(pods)
    }

    /// Keeps the pod as it is now, behind the writes asked for before;
    /// returns at once.
    pub fn save(&self, pod: &PodInfo) -> Done<()> {
        let mut kept = pod.clone();
        kept.containers.clear();
        let bytes = kept.encode_to_vec();
        let path = self.path(&pod.pod_id);
        self.writer.run(move || write(&path, &bytes))
    }

    /// Forgets the pod, behind the writes asked for before; returns at
    /// once.
    pub fn remove(&self, pod_id: &str) -> Done<()> {
        let path = self.path(pod_id);
        self.writer.run(move || match fs::remove_file(path) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
            _ => Ok(()),
        })
    }

    fn path(&self, pod_id: &str) -> PathBuf {
        self.dir.join(format!("{pod_id}.pb"))
    }
}

fn write(path: &Path, bytes: &[u8]) -> io::Result<()> {
    let temporary = path.with_extension("pb.new");
    let mut file = fs::File::create(&temporary)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    fs::rename(&temporary, path)
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::pod_service::{ContainerStatus, PodState};

    #[tokio::test]
    async fn pods_are_kept_without_their_containers() {
        let dir = tempfile::tempdir().unwrap();
        let store = Store::open(dir.path()).unwrap();
        let pod = PodInfo {
            pod_id: "11111111-2222-3333-4444-555555555555".to_string(),
            state: PodState::Running as i32,
            containers: vec![ContainerStatus::default()],
            ..Default::default()
        };
        store.save(&pod).await.unwrap();
        std::fs::write(dir.path().join("junk.pb"), b"\xff\xff").unwrap();

        let loaded = Store::open(dir.path()).unwrap().load().unwrap();
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].state, PodState::Running as i32);
        assert!(loaded[0].containers.is_empty());

        store.remove(&pod.pod_id).await.unwrap();
        store.remove(&pod.pod_id).await.unwrap();
        assert!(store.load().unwrap().is_empty());
    }

    #[test]
    fn a_save_returns_while_its_write_hangs_and_writes_keep_their_order() {
        let dir = tempfile::tempdir().unwrap();
        let store = Store::open(dir.path()).unwrap();
        let pod = |state: PodState| PodInfo {
            pod_id: "p".to_string(),
            state: state as i32,
            ..Default::default()
        };
        let hung = crate::hung::Hung::at(dir.path().join("p.pb.new"));
        // Asked for under the pods' lock: it must come back at once.
        let (asked, returned) = std::sync::mpsc::channel();
        let saves = std::thread::spawn(move || {
            let first = store.save(&pod(PodState::Pending));
            asked.send(()).unwrap();
            (store, first)
        });
        let came_back = returned.recv_timeout(std::time::Duration::from_secs(2));
        hung.open();
        let (store, first) = saves.join().unwrap();
        assert!(came_back.is_ok(), "the save waited for its write");
        assert!(first.wait().is_err(), "a FIFO does not sync");
        std::fs::remove_file(&hung.0).unwrap();
        // The later save lands last.
        let second = store.save(&pod(PodState::Running));
        let third = store.save(&pod(PodState::Stopped));
        second.wait().unwrap();
        third.wait().unwrap();
        assert_eq!(store.load().unwrap()[0].state(), PodState::Stopped);
    }
}
