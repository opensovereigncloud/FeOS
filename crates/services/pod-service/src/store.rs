// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Where the pods are kept across restarts of feosd: a file per pod,
//! `<id>.pb`, its PodInfo without the containers' status, replaced by a
//! rename.

use feos_proto::pod_service::PodInfo;
use log::warn;
use prost::Message;
use std::fs;
use std::io::{self, Write};
use std::path::PathBuf;

pub struct Store {
    dir: PathBuf,
}

impl Store {
    pub fn open(dir: impl Into<PathBuf>) -> io::Result<Self> {
        let dir = dir.into();
        fs::create_dir_all(&dir)?;
        Ok(Self { dir })
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

    pub fn save(&self, pod: &PodInfo) -> io::Result<()> {
        let mut kept = pod.clone();
        kept.containers.clear();
        let path = self.path(&pod.pod_id);
        let temporary = path.with_extension("pb.new");
        let mut file = fs::File::create(&temporary)?;
        file.write_all(&kept.encode_to_vec())?;
        file.sync_all()?;
        fs::rename(&temporary, &path)
    }

    pub fn remove(&self, pod_id: &str) -> io::Result<()> {
        match fs::remove_file(self.path(pod_id)) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
            _ => Ok(()),
        }
    }

    fn path(&self, pod_id: &str) -> PathBuf {
        self.dir.join(format!("{pod_id}.pb"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::pod_service::{ContainerStatus, PodState};

    #[test]
    fn pods_are_kept_without_their_containers() {
        let dir = tempfile::tempdir().unwrap();
        let store = Store::open(dir.path()).unwrap();
        let pod = PodInfo {
            pod_id: "11111111-2222-3333-4444-555555555555".to_string(),
            state: PodState::Running as i32,
            containers: vec![ContainerStatus::default()],
            ..Default::default()
        };
        store.save(&pod).unwrap();
        std::fs::write(dir.path().join("junk.pb"), b"\xff\xff").unwrap();

        let loaded = Store::open(dir.path()).unwrap().load().unwrap();
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].state, PodState::Running as i32);
        assert!(loaded[0].containers.is_empty());

        store.remove(&pod.pod_id).unwrap();
        store.remove(&pod.pod_id).unwrap();
        assert!(store.load().unwrap().is_empty());
    }
}
