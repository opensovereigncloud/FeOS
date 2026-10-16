// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Container records, one `container.json` in each container's directory.
//!
//! A record lives next to what it describes: deleting the container's
//! directory deletes its record, and on a guest's data volume the record
//! survives a reboot together with the container's writable layer, which is
//! what adoption needs. Every write goes to a temporary file that is renamed
//! over the record, so a reader never sees half of one.

use crate::persistence::{ContainerRecord, ContainerStatus, PersistenceError};
use feos_proto::container_service::{ContainerConfig, ContainerState, VolumeMount};
use log::info;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::io::ErrorKind;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;

const RECORD: &str = "container.json";
const RECORD_TMP: &str = ".container.json.tmp";

#[derive(Clone)]
pub struct ContainerRepository {
    root: PathBuf,
    /// Serializes read-modify-write updates of records.
    writes: Arc<Mutex<()>>,
}

/// The record as it is stored.
#[derive(Serialize, Deserialize)]
struct StoredRecord {
    container_id: String,
    image_uuid: String,
    state: String,
    pid: Option<i64>,
    image_ref: String,
    command: Vec<String>,
    env: HashMap<String, String>,
    /// The pod's volumes it mounts; none in records from before volumes.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    volume_mounts: Vec<StoredVolumeMount>,
}

#[derive(Serialize, Deserialize)]
struct StoredVolumeMount {
    volume: String,
    path: String,
    #[serde(default)]
    read_only: bool,
}

fn string_to_container_state(s: &str) -> Result<ContainerState, PersistenceError> {
    match s {
        "PULLING_IMAGE" => Ok(ContainerState::PullingImage),
        "CREATED" => Ok(ContainerState::Created),
        "RUNNING" => Ok(ContainerState::Running),
        "STOPPED" => Ok(ContainerState::Stopped),
        "CONTAINER_STATE_UNSPECIFIED" => Ok(ContainerState::Unspecified),
        _ => Err(PersistenceError::InvalidStateString(s.to_string())),
    }
}

fn container_state_to_string(state: ContainerState) -> &'static str {
    match state {
        ContainerState::PullingImage => "PULLING_IMAGE",
        ContainerState::Created => "CREATED",
        ContainerState::Running => "RUNNING",
        ContainerState::Stopped => "STOPPED",
        ContainerState::Unspecified => "CONTAINER_STATE_UNSPECIFIED",
    }
}

impl StoredRecord {
    fn from_record(record: &ContainerRecord) -> Self {
        Self {
            container_id: record.container_id.clone(),
            image_uuid: record.image_uuid.to_string(),
            state: container_state_to_string(record.status.state).to_string(),
            pid: record.status.process_id,
            image_ref: record.config.image_ref.clone(),
            command: record.config.command.clone(),
            env: record.config.env.clone(),
            volume_mounts: record
                .config
                .volume_mounts
                .iter()
                .map(|mount| StoredVolumeMount {
                    volume: mount.volume.clone(),
                    path: mount.path.clone(),
                    read_only: mount.read_only,
                })
                .collect(),
        }
    }

    fn into_record(self) -> Result<ContainerRecord, PersistenceError> {
        Ok(ContainerRecord {
            image_uuid: Uuid::parse_str(&self.image_uuid)
                .map_err(|_| PersistenceError::InvalidUuid(self.image_uuid.clone()))?,
            status: ContainerStatus {
                state: string_to_container_state(&self.state)?,
                process_id: self.pid,
            },
            config: ContainerConfig {
                image_ref: self.image_ref,
                command: self.command,
                env: self.env,
                volume_mounts: self
                    .volume_mounts
                    .into_iter()
                    .map(|mount| VolumeMount {
                        volume: mount.volume,
                        path: mount.path,
                        read_only: mount.read_only,
                    })
                    .collect(),
            },
            container_id: self.container_id,
        })
    }
}

impl ContainerRepository {
    /// Records live in `<root>/<container ID>/container.json`.
    pub async fn connect(root: impl Into<PathBuf>) -> Result<Self, PersistenceError> {
        let root = root.into();
        tokio::fs::create_dir_all(&root).await?;
        info!("Persistence: Container records in {}", root.display());
        Ok(Self {
            root,
            writes: Arc::new(Mutex::new(())),
        })
    }

    fn record_path(&self, container_id: &str) -> PathBuf {
        self.root.join(container_id).join(RECORD)
    }

    async fn read(path: &Path) -> Result<Option<ContainerRecord>, PersistenceError> {
        match tokio::fs::read(path).await {
            Ok(bytes) => serde_json::from_slice::<StoredRecord>(&bytes)?
                .into_record()
                .map(Some),
            Err(e) if e.kind() == ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    async fn write(&self, record: &ContainerRecord) -> Result<(), PersistenceError> {
        let dir = self.root.join(&record.container_id);
        tokio::fs::create_dir_all(&dir).await?;
        let json = serde_json::to_vec_pretty(&StoredRecord::from_record(record))?;
        let tmp = dir.join(RECORD_TMP);
        tokio::fs::write(&tmp, json).await?;
        tokio::fs::rename(&tmp, dir.join(RECORD)).await?;
        Ok(())
    }

    pub async fn get_container(
        &self,
        container_id: &str,
    ) -> Result<Option<ContainerRecord>, PersistenceError> {
        Self::read(&self.record_path(container_id)).await
    }

    pub async fn list_all_containers(&self) -> Result<Vec<ContainerRecord>, PersistenceError> {
        let mut records = Vec::new();
        let mut entries = match tokio::fs::read_dir(&self.root).await {
            Ok(entries) => entries,
            Err(e) if e.kind() == ErrorKind::NotFound => return Ok(records),
            Err(e) => return Err(e.into()),
        };
        while let Some(entry) = entries.next_entry().await? {
            if let Some(record) = Self::read(&entry.path().join(RECORD)).await? {
                records.push(record);
            }
        }
        records.sort_by(|a, b| a.container_id.cmp(&b.container_id));
        Ok(records)
    }

    pub async fn save_container(
        &self,
        container: &ContainerRecord,
    ) -> Result<(), PersistenceError> {
        let _guard = self.writes.lock().await;
        self.write(container).await
    }

    pub async fn update_container_state(
        &self,
        container_id: &str,
        new_state: ContainerState,
    ) -> Result<bool, PersistenceError> {
        let _guard = self.writes.lock().await;
        let Some(mut record) = self.get_container(container_id).await? else {
            return Ok(false);
        };
        record.status.state = new_state;
        self.write(&record).await?;
        Ok(true)
    }

    pub async fn update_container_pid(
        &self,
        container_id: &str,
        pid: i64,
    ) -> Result<(), PersistenceError> {
        let _guard = self.writes.lock().await;
        if let Some(mut record) = self.get_container(container_id).await? {
            record.status.process_id = Some(pid);
            self.write(&record).await?;
        }
        Ok(())
    }

    /// Removes the record; the container's directory is its owner's to
    /// remove.
    pub async fn delete_container(&self, container_id: &str) -> Result<(), PersistenceError> {
        let _guard = self.writes.lock().await;
        match tokio::fs::remove_file(self.record_path(container_id)).await {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == ErrorKind::NotFound => {
                log::warn!(
                    "Attempted to delete the record of container {container_id}, but there was none."
                );
                Ok(())
            }
            Err(e) => Err(e.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn record(id: &str) -> ContainerRecord {
        ContainerRecord {
            container_id: id.to_string(),
            image_uuid: Uuid::new_v4(),
            status: ContainerStatus {
                state: ContainerState::PullingImage,
                process_id: None,
            },
            config: ContainerConfig {
                image_ref: "docker.io/library/busybox:1.36".to_string(),
                command: vec!["sleep".to_string(), "1".to_string()],
                env: HashMap::from([("A".to_string(), "1".to_string())]),
                volume_mounts: vec![VolumeMount {
                    volume: "data".to_string(),
                    path: "/data".to_string(),
                    read_only: true,
                }],
            },
        }
    }

    #[tokio::test]
    async fn records_round_trip_and_update() {
        let dir = tempfile::tempdir().unwrap();
        let repo = ContainerRepository::connect(dir.path()).await.unwrap();
        let main = record("main");
        repo.save_container(&main).await.unwrap();
        repo.save_container(&record("other")).await.unwrap();

        repo.update_container_pid("main", 42).await.unwrap();
        assert!(repo
            .update_container_state("main", ContainerState::Running)
            .await
            .unwrap());
        let got = repo.get_container("main").await.unwrap().unwrap();
        assert_eq!(got.image_uuid, main.image_uuid);
        assert_eq!(got.status.state, ContainerState::Running);
        assert_eq!(got.status.process_id, Some(42));
        assert_eq!(got.config, main.config);

        let ids: Vec<_> = repo
            .list_all_containers()
            .await
            .unwrap()
            .into_iter()
            .map(|r| r.container_id)
            .collect();
        assert_eq!(ids, ["main", "other"]);

        repo.delete_container("main").await.unwrap();
        assert!(repo.get_container("main").await.unwrap().is_none());
        assert!(!repo
            .update_container_state("main", ContainerState::Stopped)
            .await
            .unwrap());
    }

    #[tokio::test]
    async fn records_from_before_volumes_read() {
        let dir = tempfile::tempdir().unwrap();
        let repo = ContainerRepository::connect(dir.path()).await.unwrap();
        std::fs::create_dir(dir.path().join("old")).unwrap();
        let old = serde_json::json!({
            "container_id": "old",
            "image_uuid": Uuid::nil().to_string(),
            "state": "STOPPED",
            "pid": null,
            "image_ref": "x",
            "command": [],
            "env": {},
        });
        std::fs::write(dir.path().join("old").join(RECORD), old.to_string()).unwrap();
        let old = repo.get_container("old").await.unwrap().unwrap();
        assert!(old.config.volume_mounts.is_empty());
    }

    #[tokio::test]
    async fn directories_without_a_record_are_not_containers() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("stray")).unwrap();
        let repo = ContainerRepository::connect(dir.path()).await.unwrap();
        assert!(repo.list_all_containers().await.unwrap().is_empty());
    }
}
