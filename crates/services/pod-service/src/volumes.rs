// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's persistent volumes (ADR 0016), `feos.volume.v1`: a record per
//! volume, `<id>.pb` in [`VOLUME_DIR`], and its disk in diskd.
//!
//! A pod claims its volumes when it is created, all or none, and lets go
//! of them when it is deleted: a volume is attached to one pod at a time.
//! What claims, lets go of or deletes volumes runs one at a time. The
//! records and the pods' agree after a crash: at the start of feosd, a
//! volume that names a pod which is gone is let go of, and a pod's volume
//! that names no pod is claimed again.

use crate::disk::{volume_disk, Disks};
use feos_proto::pod_service::PodInfo;
use feos_proto::volume_service::{Volume, VolumeState};
use log::{info, warn};
use prost::Message;
use std::collections::HashMap;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};
use tonic::Status;
use uuid::Uuid;

/// Where the volumes are kept: a file each.
pub const VOLUME_DIR: &str = "/var/lib/feos/volumes";
/// The smallest volume: ext4 with a journal.
const MIN_SIZE: u64 = 64 << 20;
/// The largest volume: a guard, not a limit of ext4's.
const MAX_SIZE: u64 = 16 << 40;
/// Volumes come in whole MiB.
const SIZE_UNIT: u64 = 1 << 20;

/// The node's volumes.
#[derive(Clone)]
pub struct Volumes {
    inner: Arc<Inner>,
}

struct Inner {
    dir: PathBuf,
    disks: Disks,
    volumes: Mutex<HashMap<String, Volume>>,
    /// Held by what claims, lets go of, takes over or deletes volumes.
    changes: tokio::sync::Mutex<()>,
}

impl Volumes {
    /// The volumes kept in `dir`, with their disks from `disks`. A file that
    /// does not read is left alone, and logged.
    pub fn open(dir: impl Into<PathBuf>, disks: Disks) -> io::Result<Self> {
        let dir = dir.into();
        std::fs::create_dir_all(&dir)?;
        let mut volumes = HashMap::new();
        for entry in std::fs::read_dir(&dir)? {
            let path = entry?.path();
            if path.extension().is_none_or(|ext| ext != "pb") {
                continue;
            }
            match std::fs::read(&path).map(|bytes| Volume::decode(bytes.as_slice())) {
                Ok(Ok(volume)) => {
                    volumes.insert(volume.volume_id.clone(), volume);
                }
                Ok(Err(e)) => warn!("Volumes: {} does not decode: {e}", path.display()),
                Err(e) => warn!("Volumes: {} does not read: {e}", path.display()),
            }
        }
        info!("Volumes: {} kept", volumes.len());
        Ok(Self {
            inner: Arc::new(Inner {
                dir,
                disks,
                volumes: Mutex::new(volumes),
                changes: Default::default(),
            }),
        })
    }

    /// Where the volumes are kept, and the volumes' empty layer is made.
    pub fn dir(&self) -> &Path {
        &self.inner.dir
    }

    fn volumes(&self) -> MutexGuard<'_, HashMap<String, Volume>> {
        self.inner.volumes.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub fn get(&self, volume_id: &str) -> Result<Volume, Status> {
        self.volumes()
            .get(volume_id)
            .cloned()
            .ok_or_else(|| Status::not_found(format!("no volume {volume_id}")))
    }

    pub fn list(&self) -> Vec<Volume> {
        let mut volumes: Vec<Volume> = self.volumes().values().cloned().collect();
        volumes.sort_by(|a, b| a.volume_id.cmp(&b.volume_id));
        volumes
    }

    /// Creates a volume, attached to no pod: a record. Its disk is made at
    /// the first boot of a pod that has it.
    pub async fn create(
        &self,
        volume_id: &str,
        size_bytes: u64,
        labels: HashMap<String, String>,
    ) -> Result<Volume, Status> {
        check_id(volume_id)?;
        check_size(size_bytes)?;
        let _changes = self.inner.changes.lock().await;
        if let Ok(volume) = self.get(volume_id) {
            return if volume.size_bytes == size_bytes && volume.labels == labels {
                Ok(volume)
            } else {
                Err(Status::already_exists(format!(
                    "volume {volume_id} exists with another size or labels"
                )))
            };
        }
        let volume = Volume {
            volume_id: volume_id.to_string(),
            size_bytes,
            labels,
            state: VolumeState::Available as i32,
            ..Default::default()
        };
        self.save(&volume)?;
        info!("Volumes: {volume_id} created, {size_bytes} bytes");
        Ok(volume)
    }

    /// Deletes a volume that no pod has, with what the node holds of it.
    /// One whose disk cannot go stays, to be deleted again.
    pub async fn delete(&self, volume_id: &str) -> Result<(), Status> {
        let _changes = self.inner.changes.lock().await;
        let volume = self.get(volume_id)?;
        if !volume.pod_id.is_empty() {
            return Err(Status::failed_precondition(format!(
                "volume {volume_id} is attached to pod {}",
                volume.pod_id
            )));
        }
        self.inner.disks.remove(&volume_disk(volume_id)).await?;
        match std::fs::remove_file(self.path(volume_id)) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => {
                return Err(Status::internal(format!("forget volume {volume_id}: {e}")));
            }
            _ => {}
        }
        self.volumes().remove(volume_id);
        info!("Volumes: {volume_id} deleted");
        Ok(())
    }

    /// Attaches the volumes `volume_ids` to the pod, all or none: each must
    /// be on the node and attached to no other pod.
    pub async fn claim(&self, pod_id: &str, volume_ids: &[String]) -> Result<(), Status> {
        if volume_ids.is_empty() {
            return Ok(());
        }
        let _changes = self.inner.changes.lock().await;
        let mut claimed = Vec::new();
        for volume_id in volume_ids {
            let mut volume = self.get(volume_id).map_err(|_| {
                Status::failed_precondition(format!("no volume {volume_id} on this node"))
            })?;
            match volume.pod_id.as_str() {
                "" => {}
                pod if pod == pod_id => continue,
                pod => {
                    return Err(Status::failed_precondition(format!(
                        "volume {volume_id} is attached to pod {pod}"
                    )));
                }
            }
            volume.pod_id = pod_id.to_string();
            if volume.state() == VolumeState::Available {
                volume.state = VolumeState::Attached as i32;
            }
            claimed.push(volume);
        }
        for volume in &claimed {
            self.save(volume)?;
            info!("Volumes: {} is attached to pod {pod_id}", volume.volume_id);
        }
        Ok(())
    }

    /// Lets go of the volumes the pod has but those in `keep`: they are
    /// available again, or stay checkpointed. What fails to be kept is
    /// logged; the next start of feosd sees to it.
    pub async fn release(&self, pod_id: &str, keep: &[String]) {
        let _changes = self.inner.changes.lock().await;
        let held: Vec<Volume> = self
            .volumes()
            .values()
            .filter(|volume| volume.pod_id == pod_id && !keep.contains(&volume.volume_id))
            .cloned()
            .collect();
        for mut volume in held {
            volume.pod_id.clear();
            if volume.state() == VolumeState::Attached {
                volume.state = VolumeState::Available as i32;
            }
            match self.save(&volume) {
                Ok(()) => info!("Volumes: {} is attached to no pod", volume.volume_id),
                Err(e) => warn!(
                    "Volumes: Failed to let go of {}: {}",
                    volume.volume_id,
                    e.message()
                ),
            }
        }
    }

    /// Takes the volumes of a pod's checkpoint over, as a restore of the pod
    /// finds them in it: each must be one the node does not have, the pod's
    /// own, or checkpointed and attached to no pod.
    pub async fn take_over(&self, pod_id: &str, volumes: Vec<Volume>) -> Result<(), Status> {
        let _changes = self.inner.changes.lock().await;
        for volume in &volumes {
            if let Ok(had) = self.get(&volume.volume_id) {
                check_restorable(pod_id, &had)?;
            }
        }
        for volume in &volumes {
            self.save(volume)?;
            info!(
                "Volumes: {} comes from {}",
                volume.volume_id,
                volume
                    .checkpoint
                    .as_ref()
                    .map_or("", |checkpoint| checkpoint.head_key.as_str())
            );
        }
        Ok(())
    }

    /// Changes a volume's record and keeps it.
    pub fn update(&self, volume_id: &str, change: impl FnOnce(&mut Volume)) -> Result<(), Status> {
        let mut volume = self.get(volume_id)?;
        change(&mut volume);
        self.save(&volume)
    }

    /// Makes the records agree with the pods kept, after a crash between a
    /// change of a pod and its volumes'.
    pub fn reconcile(&self, pods: &[PodInfo]) {
        let mut volumes = self.volumes();
        let mut changed = Vec::new();
        for volume in volumes.values_mut() {
            if !volume.pod_id.is_empty() && !pods.iter().any(|pod| pod.pod_id == volume.pod_id) {
                warn!(
                    "Volumes: {} was attached to pod {}, which is gone",
                    volume.volume_id, volume.pod_id
                );
                volume.pod_id.clear();
                if volume.state() == VolumeState::Attached {
                    volume.state = VolumeState::Available as i32;
                }
                changed.push(volume.clone());
            }
        }
        for pod in pods {
            let spec = pod.spec.clone().unwrap_or_default();
            for wanted in &spec.volumes {
                match volumes.get_mut(&wanted.volume_id) {
                    None => warn!(
                        "Volumes: Pod {} has volume {}, which is not on this node",
                        pod.pod_id, wanted.volume_id
                    ),
                    Some(volume) if volume.pod_id.is_empty() => {
                        warn!(
                            "Volumes: {} is attached to pod {} again",
                            volume.volume_id, pod.pod_id
                        );
                        volume.pod_id = pod.pod_id.clone();
                        if volume.state() == VolumeState::Available {
                            volume.state = VolumeState::Attached as i32;
                        }
                        changed.push(volume.clone());
                    }
                    Some(volume) if volume.pod_id != pod.pod_id => warn!(
                        "Volumes: Pod {} has volume {}, which pod {} has",
                        pod.pod_id, volume.volume_id, volume.pod_id
                    ),
                    Some(_) => {}
                }
            }
        }
        drop(volumes);
        for volume in changed {
            if let Err(e) = self.save(&volume) {
                warn!(
                    "Volumes: Failed to keep {}: {}",
                    volume.volume_id,
                    e.message()
                );
            }
        }
    }

    /// Keeps the record, replaced by a rename, and puts it in the map.
    fn save(&self, volume: &Volume) -> Result<(), Status> {
        let path = self.path(&volume.volume_id);
        let temporary = path.with_extension("pb.new");
        let written = std::fs::File::create(&temporary).and_then(|mut file| {
            file.write_all(&volume.encode_to_vec())?;
            file.sync_all()
        });
        written
            .and_then(|()| std::fs::rename(&temporary, &path))
            .map_err(|e| Status::internal(format!("keep volume {}: {e}", volume.volume_id)))?;
        self.volumes()
            .insert(volume.volume_id.clone(), volume.clone());
        Ok(())
    }

    fn path(&self, volume_id: &str) -> PathBuf {
        self.inner.dir.join(format!("{volume_id}.pb"))
    }
}

/// Whether a restore of the pod `pod_id` may take over the volume the node
/// has: the pod's own, or one in object storage alone that no pod has.
/// One with data on the node may hold what its checkpoint does not.
pub fn check_restorable(pod_id: &str, had: &Volume) -> Result<(), Status> {
    match had.pod_id.as_str() {
        pod if pod == pod_id => Ok(()),
        "" if had.state() == VolumeState::Checkpointed => Ok(()),
        "" => Err(Status::failed_precondition(format!(
            "volume {} is on this node with data its checkpoint may not have: delete it, or restore the pod on another node",
            had.volume_id
        ))),
        pod => Err(Status::failed_precondition(format!(
            "volume {} is attached to pod {pod}",
            had.volume_id
        ))),
    }
}

/// Checks a volume's ID: a UUID in lowercase, with hyphens, which also
/// names its disk and is its filesystem's UUID.
pub fn check_id(volume_id: &str) -> Result<(), Status> {
    Uuid::parse_str(volume_id)
        .ok()
        .filter(|uuid| uuid.hyphenated().to_string() == volume_id)
        .map(|_| ())
        .ok_or_else(|| {
            Status::invalid_argument(format!(
                "volume ID {volume_id:?} is not a UUID in lowercase, with hyphens"
            ))
        })
}

fn check_size(size_bytes: u64) -> Result<(), Status> {
    if !(MIN_SIZE..=MAX_SIZE).contains(&size_bytes) || !size_bytes.is_multiple_of(SIZE_UNIT) {
        return Err(Status::invalid_argument(format!(
            "a volume has from {} MiB to {} TiB, in whole MiB: not {size_bytes} bytes",
            MIN_SIZE >> 20,
            MAX_SIZE >> 40
        )));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::pod_service::{PodSpec, PodVolume};
    use feos_proto::volume_service::VolumeCheckpoint;

    const A: &str = "aaaaaaaa-0000-0000-0000-000000000001";
    const B: &str = "bbbbbbbb-0000-0000-0000-000000000002";
    const POD: &str = "11111111-2222-3333-4444-555555555555";
    const OTHER: &str = "66666666-7777-8888-9999-000000000000";

    fn volumes(dir: &Path) -> Volumes {
        Volumes::open(dir, Disks::new(dir.join("no-diskd.sock"))).unwrap()
    }

    #[tokio::test]
    async fn volumes_are_kept_and_created_once() {
        let dir = tempfile::tempdir().unwrap();
        let store = volumes(dir.path());
        let labels = HashMap::from([("app".to_string(), "db".to_string())]);
        let volume = store.create(A, 1 << 30, labels.clone()).await.unwrap();
        assert_eq!(volume.state(), VolumeState::Available);
        assert!(!volume.formatted && volume.pod_id.is_empty());
        // A create delivered twice finds the volume; another size does not.
        assert_eq!(
            store.create(A, 1 << 30, labels.clone()).await.unwrap(),
            volume
        );
        let status = store.create(A, 2 << 30, labels).await.unwrap_err();
        assert_eq!(status.code(), tonic::Code::AlreadyExists);
        std::fs::write(dir.path().join("junk.pb"), b"\xff\xff").unwrap();
        assert_eq!(volumes(dir.path()).list(), [volume]);
    }

    #[tokio::test]
    async fn sizes_and_ids_are_checked() {
        let dir = tempfile::tempdir().unwrap();
        let store = volumes(dir.path());
        for size in [0, 32 << 20, (1 << 30) + 1, 17 << 40] {
            let status = store.create(A, size, HashMap::new()).await.unwrap_err();
            assert_eq!(status.code(), tonic::Code::InvalidArgument, "{size}");
        }
        for id in [
            "",
            "../etc",
            "aaaaaaaa00000000000000000000000001",
            "AAAAAAAA-0000-0000-0000-000000000001",
        ] {
            assert!(
                store.create(id, 1 << 30, HashMap::new()).await.is_err(),
                "{id}"
            );
        }
        store.create(A, 64 << 20, HashMap::new()).await.unwrap();
        store.create(B, 16 << 40, HashMap::new()).await.unwrap();
    }

    #[tokio::test]
    async fn a_volume_is_attached_to_one_pod_at_a_time() {
        let dir = tempfile::tempdir().unwrap();
        let store = volumes(dir.path());
        store.create(A, 1 << 30, HashMap::new()).await.unwrap();
        store.create(B, 1 << 30, HashMap::new()).await.unwrap();
        let ids = |ids: &[&str]| ids.iter().map(|id| id.to_string()).collect::<Vec<_>>();

        store.claim(POD, &ids(&[A])).await.unwrap();
        // Again for the same pod: nothing changes.
        store.claim(POD, &ids(&[A])).await.unwrap();
        let a = store.get(A).unwrap();
        assert_eq!((a.state(), a.pod_id.as_str()), (VolumeState::Attached, POD));

        // All or none: B stays free when A is another pod's.
        let status = store.claim(OTHER, &ids(&[B, A])).await.unwrap_err();
        assert_eq!(status.code(), tonic::Code::FailedPrecondition);
        assert!(status.message().contains(POD), "{}", status.message());
        assert!(store.get(B).unwrap().pod_id.is_empty());
        let missing = "cccccccc-0000-0000-0000-000000000003".to_string();
        let status = store.claim(OTHER, &[missing]).await.unwrap_err();
        assert_eq!(status.code(), tonic::Code::FailedPrecondition);

        // An attached volume is not deleted. (The free one would ask diskd,
        // which is not there.)
        let status = store.delete(A).await.unwrap_err();
        assert_eq!(status.code(), tonic::Code::FailedPrecondition);

        store.release(POD, &[]).await;
        let a = store.get(A).unwrap();
        assert_eq!((a.state(), a.pod_id.as_str()), (VolumeState::Available, ""));
        store.claim(OTHER, &ids(&[A, B])).await.unwrap();
        // Kept across a restart.
        assert_eq!(volumes(dir.path()).get(B).unwrap().pod_id, OTHER);
    }

    #[tokio::test]
    async fn a_checkpointed_volume_stays_checkpointed_without_its_pod() {
        let dir = tempfile::tempdir().unwrap();
        let store = volumes(dir.path());
        store.create(A, 1 << 30, HashMap::new()).await.unwrap();
        store.claim(POD, &[A.to_string()]).await.unwrap();
        let checkpoint = VolumeCheckpoint {
            head_key: format!("snapshots/volume-{A}/7/head.json"),
            epoch: 7,
        };
        store
            .update(A, |volume| {
                volume.state = VolumeState::Checkpointed as i32;
                volume.checkpoint = Some(checkpoint.clone());
            })
            .unwrap();
        store.release(POD, &[]).await;
        let a = store.get(A).unwrap();
        assert_eq!(a.state(), VolumeState::Checkpointed);
        assert_eq!(a.checkpoint, Some(checkpoint));
        // A restore of any pod may take it over, as one of another pod's
        // may not.
        assert!(check_restorable(OTHER, &a).is_ok());
        store.claim(POD, &[A.to_string()]).await.unwrap();
        assert!(check_restorable(POD, &store.get(A).unwrap()).is_ok());
        assert!(check_restorable(OTHER, &store.get(A).unwrap()).is_err());
        // An available one has data of its own on the node.
        store.create(B, 1 << 30, HashMap::new()).await.unwrap();
        assert!(check_restorable(POD, &store.get(B).unwrap()).is_err());
    }

    #[tokio::test]
    async fn the_records_agree_with_the_pods_after_a_crash() {
        let dir = tempfile::tempdir().unwrap();
        let store = volumes(dir.path());
        store.create(A, 1 << 30, HashMap::new()).await.unwrap();
        store.create(B, 1 << 30, HashMap::new()).await.unwrap();
        // A was claimed by a pod whose create did not get to keep it.
        store.claim(OTHER, &[A.to_string()]).await.unwrap();
        // The pod that has B was kept, B's claim was not.
        let pod = PodInfo {
            pod_id: POD.to_string(),
            spec: Some(PodSpec {
                volumes: vec![PodVolume {
                    name: "db".to_string(),
                    volume_id: B.to_string(),
                }],
                ..Default::default()
            }),
            ..Default::default()
        };
        store.reconcile(&[pod]);
        let store = volumes(dir.path());
        assert!(store.get(A).unwrap().pod_id.is_empty());
        assert_eq!(store.get(A).unwrap().state(), VolumeState::Available);
        assert_eq!(store.get(B).unwrap().pod_id, POD);
        assert_eq!(store.get(B).unwrap().state(), VolumeState::Attached);
    }
}
