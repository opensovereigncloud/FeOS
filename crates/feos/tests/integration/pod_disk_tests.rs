// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Pods' disks from diskd (ADR 0011) and their hibernation (ADR 0008): what
//! is on a pod's disk survives a stop, a crash and a trip through object
//! storage, and its memory comes back where the node can restore it.
//!
//! The pods have no network, so that the tests need no registry: what is
//! on a disk is told by container records, which the guest keeps on its
//! data volume and lists. A test puts a record on a stopped pod's disk
//! from the node; the guest's own changes are a record it deletes.
//! Hibernation needs diskd with an object store, as `make test-pods` gives
//! it when it has one; without, that test skips.

use super::fixtures::{verify_microvm_cleanup, PodGuard};
use super::pod_tests::{for_pod, get_pod, guest_ping, new_pod_id};
use super::{ensure_server, get_public_clients, installed_guest, PUBLIC_SERVER_ADDRESS};
use anyhow::{bail, Context, Result};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, DeleteContainerRequest, ListContainersRequest,
};
use feos_proto::disk_service::{
    disk_service_client::DiskServiceClient, evict_request, EvictRequest, GetHeadRequest,
    OpenDiskRequest, StatsRequest, StopDiskRequest,
};
use feos_proto::pod_service::{
    pod_service_client::PodServiceClient, CreatePodRequest, DeletePodRequest, HibernatePodRequest,
    PodInfo, PodNetwork, PodSpec, PodState, Resources, RestorePodRequest, ResumePodRequest,
    StartPodRequest, StopPodRequest,
};
use hyper_util::rt::TokioIo;
use log::info;
use microvm_service::vm_dir;
use nix::mount::{mount, umount, MsFlags};
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use pod_service::disk::DISKD_SOCKET;
use pod_service::network;
use std::path::{Path, PathBuf};
use std::time::Duration;
use tokio::time::timeout;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

/// How long a pod may take to come up.
const UP_TIMEOUT: Duration = Duration::from_secs(120);

/// diskd's state directory, beside its socket.
fn diskd_state() -> PathBuf {
    Path::new(DISKD_SOCKET)
        .parent()
        .expect("the socket is in the state directory")
        .to_path_buf()
}

async fn diskd() -> Result<DiskServiceClient<Channel>> {
    let channel = Endpoint::from_static("http://diskd")
        .connect_with_connector(service_fn(|_: Uri| async {
            tokio::net::UnixStream::connect(DISKD_SOCKET)
                .await
                .map(TokioIo::new)
        }))
        .await?;
    Ok(DiskServiceClient::new(channel))
}

/// A pod without containers and without a network.
fn spec() -> PodSpec {
    PodSpec {
        resources: Some(Resources {
            vcpus: 1,
            memory_mib: 256,
        }),
        network: Some(PodNetwork {
            none: true,
            ..Default::default()
        }),
        ..Default::default()
    }
}

/// Waits until the pod runs.
async fn wait_until_up(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<PodInfo> {
    timeout(UP_TIMEOUT, async {
        loop {
            let pod = get_pod(pods, pod_id).await?;
            match pod.state() {
                PodState::Running => return Ok(pod),
                PodState::Failed => bail!("pod {pod_id} failed: {}", pod.reason),
                _ => tokio::time::sleep(Duration::from_millis(200)).await,
            }
        }
    })
    .await
    .with_context(|| format!("pod {pod_id} did not come up"))?
}

/// The pod's disk on the node: diskd's directory of it.
fn disk_dir(pod_id: &str) -> PathBuf {
    diskd_state().join("live").join(pod_id)
}

/// Puts container records named `names` on the stopped pod's disk, as the
/// guest keeps them, with `marker` for their image: the disk is started,
/// mounted on the node, written and stopped again.
async fn plant(pod_id: &str, names: &[&str], marker: &str) -> Result<()> {
    let mut disks = diskd().await?;
    let device = disks
        .open_disk(OpenDiskRequest {
            pod_id: pod_id.to_string(),
            head_key: String::new(),
        })
        .await?
        .into_inner();
    let dir = tempfile::tempdir()?;
    mount(
        Some(device.dev_path.as_str()),
        dir.path(),
        Some("ext4"),
        MsFlags::empty(),
        None::<&str>,
    )
    .context("mount the pod's disk")?;
    let written = (|| -> Result<()> {
        for name in names {
            let container = dir.path().join("containers").join(name);
            std::fs::create_dir_all(&container)?;
            let record = serde_json::json!({
                "container_id": name,
                "image_uuid": uuid::Uuid::nil().to_string(),
                "state": "STOPPED",
                "pid": null,
                "image_ref": marker,
                "command": [],
                "env": {},
            });
            std::fs::write(container.join("container.json"), record.to_string())?;
        }
        Ok(())
    })();
    nix::unistd::sync();
    umount(dir.path()).context("unmount the pod's disk")?;
    disks
        .stop_disk(StopDiskRequest {
            pod_id: pod_id.to_string(),
        })
        .await?;
    written
}

/// The container records the pod's guest lists: name and image.
async fn records(pod_id: &str) -> Result<Vec<(String, String)>> {
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let mut listed: Vec<(String, String)> = containers
        .list_containers(for_pod(pod_id, ListContainersRequest {}))
        .await?
        .into_inner()
        .containers
        .into_iter()
        .map(|c| (c.container_id, c.config.unwrap_or_default().image_ref))
        .collect();
    listed.sort();
    Ok(listed)
}

fn marked(names: &[&str], marker: &str) -> Vec<(String, String)> {
    names
        .iter()
        .map(|name| (name.to_string(), marker.to_string()))
        .collect()
}

async fn start(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<PodInfo> {
    pods.start_pod(StartPodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    wait_until_up(pods, pod_id).await
}

async fn stop(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<()> {
    pods.stop_pod(StopPodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    Ok(())
}

fn pid(pod: &PodInfo) -> Result<i32> {
    Ok(pod
        .microvm
        .as_ref()
        .context("a running pod has a microVM")?
        .pid as i32)
}

#[tokio::test]
async fn test_pod_disk_survives_stop_and_crash() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;

    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec()),
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_until_up(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);

    // The pod's disk is a ublk device of diskd's, linked in its directory,
    // and the guest's data volume: a new disk holds no containers.
    let link = std::fs::read_link(disk_dir(&pod_id).join("disk"))?;
    assert!(
        link.to_string_lossy().starts_with("/dev/ublkb"),
        "the pod's disk is {}",
        link.display()
    );
    assert!(records(&pod_id).await?.is_empty());
    let first_boot = guest_ping(&pod_id).await?.boot_id;

    info!("Stopping pod {pod_id}: its disk stops, and keeps what it holds");
    stop(&mut pods, &pod_id).await?;
    let stats = diskd()
        .await?
        .stats(StatsRequest {
            pod_id: pod_id.clone(),
        })
        .await?
        .into_inner();
    assert!(stats.devices.is_empty(), "a stopped pod's disk is stopped");
    assert!(disk_dir(&pod_id).join("top.img").is_file());
    let marker = format!("marker/{}", uuid::Uuid::new_v4());
    plant(&pod_id, &["keep", "drop"], &marker).await?;

    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_ne!(guest_ping(&pod_id).await?.boot_id, first_boot, "a new boot");
    assert_eq!(
        records(&pod_id).await?,
        marked(&["drop", "keep"], &marker),
        "the guest booted on another disk"
    );
    // The guest's own writes stay, too.
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    containers
        .delete_container(for_pod(
            &pod_id,
            DeleteContainerRequest {
                container_id: "drop".to_string(),
            },
        ))
        .await?;
    stop(&mut pods, &pod_id).await?;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    info!("Pod {pod_id} booted anew on its disk, with what the node and the guest wrote");

    let crashed = pid(&pod)?;
    info!("Killing Firecracker, pid {crashed}");
    kill(Pid::from_raw(crashed), Signal::SIGKILL)?;
    timeout(Duration::from_secs(30), async {
        while get_pod(&mut pods, &pod_id).await?.state() != PodState::Failed {
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
        anyhow::Ok(())
    })
    .await
    .context("the pod did not fail")??;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_eq!(
        records(&pod_id).await?,
        marked(&["keep"], &marker),
        "a crash lost the disk"
    );
    info!("Pod {pod_id} came back from a crash on its disk");

    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    verify_microvm_cleanup(&pod_id);
    assert!(
        !disk_dir(&pod_id).exists(),
        "the pod's disk is gone with it"
    );
    guard.disable_cleanup();
    Ok(())
}

/// Hibernates the pod; returns its checkpoint's head.
async fn hibernate(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<String> {
    let response = pods
        .hibernate_pod(HibernatePodRequest {
            pod_id: pod_id.to_string(),
        })
        .await?
        .into_inner();
    let checkpoint = response
        .checkpoint
        .context("a hibernation has a checkpoint")?;
    info!(
        "Pod {pod_id} is in {}: {} MiB of memory, {} KiB on its disk",
        checkpoint.head_key,
        response.memory_bytes >> 20,
        response.disk_bytes >> 10
    );
    assert!(
        checkpoint.memory,
        "a running pod hibernates with its memory"
    );
    assert!(response.memory_bytes > 0 && response.disk_bytes > 0);
    let pod = get_pod(pods, pod_id).await?;
    assert_eq!(pod.state(), PodState::Hibernated);
    assert_eq!(pod.checkpoint.as_ref(), Some(&checkpoint));
    // Nothing of it is left on the node.
    assert!(!vm_dir(pod_id).exists(), "a hibernated pod has no microVM");
    assert!(!disk_dir(pod_id).exists(), "a hibernated pod has no disk");
    assert!(
        !Path::new("/sys/class/net")
            .join(network::tap_name(pod_id))
            .exists(),
        "a hibernated pod has no TAP device"
    );
    Ok(checkpoint.head_key)
}

#[tokio::test]
async fn test_pod_hibernate_and_restore() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    if std::env::var("S3_BUCKET").map_or(true, |bucket| bucket.is_empty()) {
        log::warn!("Skipping test: no object store (S3_BUCKET) to hibernate to.");
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut disks = diskd().await?;

    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec()),
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    wait_until_up(&mut pods, &pod_id).await?;
    stop(&mut pods, &pod_id).await?;
    let marker = format!("marker/{}", uuid::Uuid::new_v4());
    plant(&pod_id, &["keep"], &marker).await?;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    let before = guest_ping(&pod_id).await?;

    let head_key = hibernate(&mut pods, &pod_id).await?;

    // Another node: one that has neither the pod nor anything of it in
    // its cache.
    info!("Forgetting pod {pod_id} and what diskd has of it");
    let head = disks
        .get_head(GetHeadRequest {
            key: head_key.clone(),
        })
        .await?
        .into_inner();
    assert_eq!(head.chain.len(), 2, "the blank volume and the pod's layer");
    let memory = head.memory.clone().context("the head has the memory")?;
    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    disks
        .evict(EvictRequest {
            target: evict_request::Target::All as i32,
            force: false,
        })
        .await?;
    // The pod's own layers come from the object store again; the blank
    // volume is every pod's.
    let layers = diskd_state().join("layers");
    for layer in [&head.chain[1], &memory.layer] {
        std::fs::remove_file(layers.join(format!("{layer}.manifest")))?;
    }

    info!("Restoring pod {pod_id} from {head_key}");
    let restored = pods
        .restore_pod(RestorePodRequest {
            pod_id: pod_id.clone(),
            head_key: head_key.clone(),
            cold: false,
        })
        .await?
        .into_inner();
    assert!(
        restored.memory_restored,
        "the memory was not restored: {}",
        restored.reason
    );
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(pod.spec, Some(spec()), "the spec comes with the checkpoint");
    guard.set_pid(pid(&pod)?);
    // The same boot, with its memory: the ping count goes on.
    let after = guest_ping(&pod_id).await?;
    assert_eq!(after.boot_id, before.boot_id, "the same boot");
    assert!(after.tick > before.tick, "the guest forgot its pings");
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    for layer in [&head.chain[1], &memory.layer] {
        assert!(layers.join(format!("{layer}.manifest")).is_file());
    }
    let stats = disks
        .stats(StatsRequest {
            pod_id: pod_id.clone(),
        })
        .await?
        .into_inner();
    let served = stats.memories.first().context("diskd serves the memory")?;
    info!(
        "diskd served {} faults, {} chunks from the object store",
        served.faults, served.s3_fetches
    );
    assert!(
        served.s3_fetches > 0,
        "the memory came from the object store"
    );

    // A resume that comes while the pod hibernates waits, and restores it.
    info!("Hibernating pod {pod_id} again, and resuming it meanwhile");
    let hibernation = {
        let mut pods = pods.clone();
        let pod_id = pod_id.clone();
        tokio::spawn(async move { hibernate(&mut pods, &pod_id).await })
    };
    timeout(Duration::from_secs(30), async {
        loop {
            let pod = get_pod(&mut pods, &pod_id).await?;
            if pod.reason == "hibernating" || pod.state() != PodState::Running {
                return anyhow::Ok(());
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .context("the hibernation did not start")??;
    pods.resume_pod(ResumePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let second = hibernation.await??;
    assert_ne!(second, head_key, "every hibernation has a head of its own");
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    guard.set_pid(pid(&pod)?);
    assert_eq!(guest_ping(&pod_id).await?.boot_id, before.boot_id);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));

    // Without its memory: a new boot on the checkpoint's disk.
    info!("Hibernating pod {pod_id}, and restoring it without its memory");
    let third = hibernate(&mut pods, &pod_id).await?;
    let restored = pods
        .restore_pod(RestorePodRequest {
            pod_id: pod_id.clone(),
            head_key: String::new(),
            cold: true,
        })
        .await?
        .into_inner();
    assert!(!restored.memory_restored);
    let pod = wait_until_up(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_ne!(
        guest_ping(&pod_id).await?.boot_id,
        before.boot_id,
        "a new boot"
    );
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    // Hibernation after hibernation, the chain keeps its shape.
    let head = disks
        .get_head(GetHeadRequest { key: third })
        .await?
        .into_inner();
    assert_eq!(head.chain.len(), 2, "the chain grew: {:?}", head.chain);

    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    verify_microvm_cleanup(&pod_id);
    assert!(!disk_dir(&pod_id).exists());
    guard.disable_cleanup();
    Ok(())
}
